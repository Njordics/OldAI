package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/modelcache"
	"aicluster/controller/provision"
	"aicluster/controller/store"
)

// Model management: an *assignment* puts one model (at one quantisation) on
// one node. The Controller first makes sure the model is in its cache, then
// adds it to the node's desired state; the node pulls it over the LAN.

var (
	ErrUnknownModel = errors.New("no such model in the catalog")
	ErrNoNodeFits   = errors.New("no node has room for this model")
	ErrNotAdopted   = errors.New("node is not adopted")
	ErrAlreadyThere = errors.New("model is already on this node")
	ErrNoPoolNeeded = errors.New("this model fits on one node; install it without splitting")
	ErrPooledSwap   = errors.New("a model split across nodes can't be swapped; remove it and install another")
)

// PoolParallel is how many requests a pooled model serves at once.
const PoolParallel = 2

// FitError lists why a model can't go on the requested nodes.
type FitError struct {
	Reasons map[string]string // node name -> reason
}

func (e *FitError) Error() string {
	parts := make([]string, 0, len(e.Reasons))
	for n, r := range e.Reasons {
		parts = append(parts, n+": "+r)
	}
	sort.Strings(parts)
	return "model does not fit: " + strings.Join(parts, "; ")
}

type InstallRequest struct {
	ModelID   string   `json:"model_id"`
	Quant     string   `json:"quant,omitempty"`     // "" = best that fits each node
	Nodes     []string `json:"nodes,omitempty"`     // empty = pick automatically
	Replicas  int      `json:"replicas,omitempty"`  // with automatic placement; default 1
	Residency string   `json:"residency,omitempty"` // resident (default) | on_demand
	// Pool splits one copy across nodes (llama.cpp RPC). With Nodes, the
	// pool uses those nodes; without, the provisioner picks the fewest.
	Pool bool `json:"pool,omitempty"`
}

func servesModels(n *store.Node) bool {
	return n.State == store.StateAdopted || n.State == store.StateOffline
}

// fitNode describes a node to the provisioner: its memory and what its
// current assignments already use.
func fitNode(n *store.Node, assigns []*store.Assignment) provision.FitNode {
	fn := provision.FitNode{NodeID: n.NodeID, Runtimes: []string{"ollama"}}
	var inv proto.Inventory
	if json.Unmarshal(n.Inventory, &inv) == nil {
		fn.RAMTotal = int64(inv.Memory.Total)
		for _, g := range inv.GPUs {
			// Integrated GPUs use the node's RAM, already counted; a card
			// without a working driver can't hold a model yet.
			if !g.Integrated && g.Compute.API != "" && g.Compute.API != "none" {
				fn.GPUs = append(fn.GPUs, provision.GPU{VRAM: int64(g.VRAM)})
			}
		}
		for _, d := range inv.Disks {
			fn.DiskFree = int64(d.Free)
		}
	}
	var h proto.Health
	if json.Unmarshal(n.Health, &h) == nil && h.DiskFree > 0 {
		fn.DiskFree = int64(h.DiskFree)
	}
	if fn.RAMTotal == 0 && h.MemTotal > 0 {
		fn.RAMTotal = int64(h.MemTotal)
	}
	downloaded := map[string]bool{}
	for _, m := range observedModels(n) {
		if m.State == proto.ModelInstalled || m.State == proto.ModelLoaded || m.State == proto.ModelLoading {
			downloaded[m.AssignmentID] = true
		}
	}
	for _, a := range assigns {
		fn.Committed = append(fn.Committed, provision.Committed{
			ID: a.ID, VRAM: a.VRAM, RAM: a.RAM, Residency: a.Residency, Size: a.Size, Downloaded: downloaded[a.ID],
		})
	}
	return fn
}

func observedModels(n *store.Node) []proto.ObservedModel {
	var obs proto.ObservedState
	if len(n.Observed) == 0 || json.Unmarshal(n.Observed, &obs) != nil {
		return nil
	}
	return obs.Models
}

func newAssignment(nodeID string, r provision.FitResult, residency string) *store.Assignment {
	if residency != proto.ResidencyOnDemand {
		residency = proto.ResidencyResident
	}
	return &store.Assignment{
		ID: ulid.Make().String(), NodeID: nodeID, ModelID: r.ModelID, Quant: r.Quant, Runtime: r.Runtime,
		Ref: r.Ref, Size: r.Size, Need: r.Need, Placement: r.Placement, VRAM: r.VRAM, RAM: r.RAM,
		Residency: residency, CreatedAt: time.Now().UTC(),
	}
}

// Install places a model on the requested nodes, or on the best-fitting
// nodes when none are named. It is all-or-nothing.
func (c *Core) Install(ctx context.Context, req InstallRequest) ([]*store.Assignment, error) {
	cat, err := c.Prov.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if _, v := cat.Variant(req.ModelID); v == nil {
		return nil, ErrUnknownModel
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	all, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return nil, err
	}
	byNode := map[string][]*store.Assignment{}
	for _, a := range all {
		byNode[a.NodeID] = append(byNode[a.NodeID], a)
	}
	has := func(nodeID string) bool {
		return slices.ContainsFunc(byNode[nodeID], func(a *store.Assignment) bool { return a.ModelID == req.ModelID })
	}

	if req.Pool {
		return c.installPool(ctx, req, nodes, byNode, has)
	}

	explicit := len(req.Nodes) > 0
	var targets []*store.Node
	names := map[string]string{}
	for _, n := range nodes {
		names[n.NodeID] = n.Name
		switch {
		case explicit && slices.Contains(req.Nodes, n.NodeID):
			if !servesModels(n) {
				return nil, fmt.Errorf("%s: %w", n.Name, ErrNotAdopted)
			}
			if has(n.NodeID) {
				return nil, fmt.Errorf("%s: %w", n.Name, ErrAlreadyThere)
			}
			targets = append(targets, n)
		case !explicit && n.State == store.StateAdopted && !has(n.NodeID):
			targets = append(targets, n)
		}
	}
	if explicit && len(targets) != len(req.Nodes) {
		return nil, fmt.Errorf("unknown node in request: %w", store.ErrNotFound)
	}
	if len(targets) == 0 {
		return nil, ErrNoNodeFits
	}

	freq := provision.FitRequest{Models: []string{req.ModelID}, Quant: req.Quant}
	for _, n := range targets {
		freq.Nodes = append(freq.Nodes, fitNode(n, byNode[n.NodeID]))
	}
	fr, err := c.Prov.Fit(ctx, freq)
	if err != nil {
		return nil, err
	}

	var chosen []provision.FitResult
	if explicit {
		fe := &FitError{Reasons: map[string]string{}}
		for _, r := range fr.Results {
			if !r.Fits {
				fe.Reasons[names[r.NodeID]] = r.Reason
			}
			chosen = append(chosen, r)
		}
		if len(fe.Reasons) > 0 {
			return nil, fe
		}
	} else {
		for _, r := range fr.Results {
			if r.Fits {
				chosen = append(chosen, r)
			}
		}
		if len(chosen) == 0 {
			fe := &FitError{Reasons: map[string]string{}}
			for _, r := range fr.Results {
				fe.Reasons[names[r.NodeID]] = r.Reason
			}
			return nil, fe
		}
		// Best placement first, then the node with the most room left.
		rank := map[string]int{"gpu": 0, "hybrid": 1, "cpu": 2}
		sort.SliceStable(chosen, func(i, j int) bool {
			if rank[chosen[i].Placement] != rank[chosen[j].Placement] {
				return rank[chosen[i].Placement] < rank[chosen[j].Placement]
			}
			return chosen[i].Need < chosen[j].Need || chosen[i].VRAM > chosen[j].VRAM
		})
		replicas := max(req.Replicas, 1)
		chosen = chosen[:min(replicas, len(chosen))]
	}

	var out []*store.Assignment
	for _, r := range chosen {
		out = append(out, newAssignment(r.NodeID, r, req.Residency))
	}
	if err := c.Store.PutAssignments(ctx, out); err != nil {
		return nil, err
	}
	for _, a := range out {
		c.log.Info("model assigned", "model", a.ModelID, "quant", a.Quant, "node", names[a.NodeID], "placement", a.Placement)
	}
	c.afterAssignmentChange(ctx, out...)
	return out, nil
}

// Swap replaces one assignment with another model on the same node. The old
// model's memory counts as free when checking the new one.
func (c *Core) Swap(ctx context.Context, assignmentID, modelID, quant string) (*store.Assignment, error) {
	old, err := c.Store.GetAssignment(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if old.GroupID != "" {
		return nil, ErrPooledSwap
	}
	n, err := c.Store.GetNode(ctx, old.NodeID)
	if err != nil {
		return nil, err
	}
	cat, err := c.Prov.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if _, v := cat.Variant(modelID); v == nil {
		return nil, ErrUnknownModel
	}
	assigns, err := c.Store.ListAssignments(ctx, n.NodeID)
	if err != nil {
		return nil, err
	}
	for _, a := range assigns {
		if a.ModelID == modelID && a.ID != old.ID {
			return nil, ErrAlreadyThere
		}
	}
	fr, err := c.Prov.Fit(ctx, provision.FitRequest{
		Nodes: []provision.FitNode{fitNode(n, assigns)}, Models: []string{modelID}, Quant: quant, Exclude: []string{old.ID},
	})
	if err != nil {
		return nil, err
	}
	if len(fr.Results) != 1 || !fr.Results[0].Fits {
		reason := "no fitting quantisation"
		if len(fr.Results) == 1 {
			reason = fr.Results[0].Reason
		}
		return nil, &FitError{Reasons: map[string]string{n.Name: reason}}
	}
	next := newAssignment(n.NodeID, fr.Results[0], old.Residency)
	if err := c.Store.SwapAssignment(ctx, old.ID, next); err != nil {
		return nil, err
	}
	c.log.Info("model swapped", "node", n.Name, "from", old.ModelID+"/"+old.Quant, "to", next.ModelID+"/"+next.Quant)
	c.afterAssignmentChange(ctx, next)
	return next, nil
}

func (c *Core) RemoveAssignment(ctx context.Context, id string) error {
	a, err := c.Store.GetAssignment(ctx, id)
	if err != nil {
		return err
	}
	if a.GroupID != "" {
		return c.removePool(ctx, a.GroupID)
	}
	if err := c.Store.DeleteAssignment(ctx, id); err != nil {
		return err
	}
	c.log.Info("model unassigned", "model", a.ModelID, "node_id", a.NodeID)
	c.SyncDesired(ctx, a.NodeID)
	return nil
}

// RetryAssignment restarts a failed Controller download for its model.
func (c *Core) RetryAssignment(ctx context.Context, id string) error {
	a, err := c.Store.GetAssignment(ctx, id)
	if err != nil {
		return err
	}
	if _, err := c.Models.Ensure(a.Ref); err != nil {
		return err
	}
	c.SyncDesired(ctx, a.NodeID)
	return nil
}

// afterAssignmentChange starts cache downloads and updates affected nodes.
func (c *Core) afterAssignmentChange(ctx context.Context, as ...*store.Assignment) {
	nodes := map[string]bool{}
	for _, a := range as {
		if _, err := c.Models.Ensure(a.Ref); err != nil {
			c.log.Warn("model download not started", "ref", a.Ref, "err", err)
		}
		nodes[a.NodeID] = true
	}
	for id := range nodes {
		c.SyncDesired(ctx, id)
	}
}

func (c *Core) onModelCached(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	all, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return
	}
	nodes := map[string]bool{}
	for _, a := range all {
		if a.Ref == ref {
			nodes[a.NodeID] = true
		}
	}
	for id := range nodes {
		c.SyncDesired(ctx, id)
	}
}

// SyncDesired rebuilds a node's desired state from its assignments (only
// models already in the Controller cache are included), bumps the generation
// if anything changed, and pushes it to the node. Nodes that miss the push
// pick it up from their next heartbeat.
func (c *Core) SyncDesired(ctx context.Context, nodeID string) {
	assigns, err := c.Store.ListAssignments(ctx, nodeID)
	if err != nil {
		c.log.Error("sync desired", "err", err)
		return
	}
	var models []proto.DesiredModel
	for _, a := range assigns {
		st := c.Models.Status(a.Ref)
		if a.GroupID != "" {
			if dm, ok := c.poolDesired(ctx, a, st); ok {
				models = append(models, dm)
			}
			continue
		}
		if st.State != modelcache.StateReady {
			continue
		}
		models = append(models, proto.DesiredModel{
			AssignmentID: a.ID, ModelID: a.ModelID, Variant: a.Quant, Runtime: a.Runtime,
			Source:    proto.ModelSource{Type: "ollama", Ref: a.Ref, Digest: st.Digest},
			Residency: a.Residency, Size: st.BytesTotal,
		})
	}
	network := c.slaveNetworkFor(ctx, nodeID)
	update := c.agentUpdateFor(nodeID)
	var drv *proto.DriverSpec
	drvKnown := false
	if node, err := c.Store.GetNode(ctx, nodeID); err == nil {
		drv, drvKnown = c.driverFor(node)
	}
	var changed bool
	var desired proto.DesiredState
	n, err := c.Store.UpdateNode(ctx, nodeID, func(n *store.Node) error {
		desired = proto.DesiredState{V: proto.Version, Generation: n.DesiredGen, NodeName: n.Name}
		if len(n.Desired) > 0 {
			_ = json.Unmarshal(n.Desired, &desired)
		}
		var runtimes []proto.RuntimeSpec
		if len(models) > 0 {
			runtimes = []proto.RuntimeSpec{{Kind: "ollama"}}
		}
		if !drvKnown {
			drv = desired.Drivers // the driver list is still loading
		}
		before, _ := json.Marshal([]any{desired.Models, desired.Runtimes, desired.NodeName, desired.Network, desired.Update, desired.Drivers})
		after, _ := json.Marshal([]any{models, runtimes, n.Name, network, update, drv})
		if string(before) == string(after) && desired.GCGraceS == int(c.cfg.ModelGCGrace/time.Second) {
			return nil
		}
		changed = true
		desired.V, desired.NodeName = proto.Version, n.Name
		desired.Models, desired.Runtimes, desired.Network, desired.Update = models, runtimes, network, update
		// A driver may need a reboot into a newer kernel (§14).
		desired.Drivers, desired.AllowReboot = drv, drv != nil && drv.Vendor == "nvidia"
		desired.GCGraceS = int(c.cfg.ModelGCGrace / time.Second)
		desired.Generation = n.DesiredGen + 1
		n.DesiredGen = desired.Generation
		n.Desired, _ = json.Marshal(desired)
		return nil
	})
	if err != nil || !changed {
		return
	}
	c.log.Info("desired state updated", "node", n.Name, "generation", desired.Generation, "models", len(models))
	if n.State != store.StateAdopted {
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpx.Do(pctx, c.NodeClient(n.NodeID, 5*time.Second), http.MethodPut, nodeBase(n)+"/v1/desired-state", desired, nil); err != nil {
			c.log.Debug("desired push failed; node will pull it", "node", n.Name, "err", err)
		}
	}()
}

// ModelStatus is one assignment as the dashboard shows it.
type ModelStatus struct {
	store.Assignment
	NodeName   string  `json:"node_name"`
	NodeState  string  `json:"node_state"`
	State      string  `json:"state"` // fetching | pending | downloading | installed | loading | loaded | failed | offline
	Progress   float64 `json:"progress"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	Error      string  `json:"error,omitempty"`
	// Where the bytes are: "controller" while the Controller downloads from
	// the internet, "node" while the node pulls from the Controller.
	Stage string `json:"stage"`
}

func (c *Core) ModelStatuses(ctx context.Context) ([]ModelStatus, error) {
	all, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return nil, err
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*store.Node{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	out := make([]ModelStatus, 0, len(all))
	for _, a := range all {
		ms := ModelStatus{Assignment: *a, State: proto.ModelPending}
		n := byID[a.NodeID]
		if n != nil {
			ms.NodeName, ms.NodeState = n.Name, n.State
		}
		cs := c.Models.Status(a.Ref)
		switch cs.State {
		case modelcache.StateReady:
			ms.Stage = "node"
		case modelcache.StateFailed:
			ms.Stage, ms.State, ms.Error = "controller", proto.ModelFailed, "Controller download failed: "+cs.Error
		case "":
			ms.Stage, ms.State = "controller", "fetching" // download not started (e.g. after a restart)
		default:
			ms.Stage, ms.State = "controller", "fetching"
			ms.Progress, ms.BytesDone, ms.BytesTotal = cs.Progress, cs.BytesDone, cs.BytesTotal
		}
		if ms.Stage == "node" && n != nil {
			for _, om := range observedModels(n) {
				if om.AssignmentID == a.ID {
					ms.State, ms.Progress, ms.BytesDone, ms.BytesTotal, ms.Error = om.State, om.Progress, om.BytesDone, om.BytesTotal, om.Error
				}
			}
			if n.State == store.StateOffline {
				ms.State = "offline"
			}
		}
		out = append(out, ms)
	}
	return out, nil
}

// CatalogFit answers "which models fit where" for the dashboard's catalog.
func (c *Core) CatalogFit(ctx context.Context) (*provision.FitResponse, error) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	all, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return nil, err
	}
	byNode := map[string][]*store.Assignment{}
	for _, a := range all {
		byNode[a.NodeID] = append(byNode[a.NodeID], a)
	}
	req := provision.FitRequest{Pool: true}
	for _, n := range nodes {
		if servesModels(n) {
			req.Nodes = append(req.Nodes, fitNode(n, byNode[n.NodeID]))
		}
	}
	if len(req.Nodes) == 0 {
		return &provision.FitResponse{}, nil
	}
	return c.Prov.Fit(ctx, req)
}

// Replica is one node able to serve a model right now.
type Replica struct {
	ModelID   string
	NodeID    string
	NodeName  string
	BaseURL   string // agent address
	LocalName string // the runtime's name for the model
	Loaded    bool   // in memory; otherwise on disk and loaded on first use
	Slots     int    // concurrent requests the node's runtime serves
	// Route is the agent's inference route: "ollama", or "llamacpp:<id>"
	// for a pooled model served by its leader.
	Route string
	// Pool lists the other nodes a pooled replica depends on.
	Pool []string
}

// Replicas lists, per model, the online nodes that hold it.
func (c *Core) Replicas(ctx context.Context) (map[string][]Replica, error) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	all, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return nil, err
	}
	pooled := poolReadiness(nodes, all)
	out := map[string][]Replica{}
	for _, n := range nodes {
		if n.State != store.StateAdopted {
			continue
		}
		slots := 4
		var h proto.Health
		if json.Unmarshal(n.Health, &h) == nil && h.Inference.Slots > 0 {
			slots = h.Inference.Slots
		}
		for _, m := range observedModels(n) {
			switch m.Role {
			case store.RoleWorker:
				continue // workers serve their leader, not clients
			case store.RoleLeader:
				members, ready := pooled[m.AssignmentID]
				if !ready {
					continue
				}
				out[m.ModelID] = append(out[m.ModelID], Replica{
					ModelID: m.ModelID, NodeID: n.NodeID, NodeName: n.Name, BaseURL: nodeBase(n), LocalName: m.LocalName,
					Loaded: true, Slots: PoolParallel, Route: "llamacpp:" + m.AssignmentID, Pool: members,
				})
				continue
			}
			if m.LocalName == "" || (m.State != proto.ModelLoaded && m.State != proto.ModelInstalled) {
				continue
			}
			out[m.ModelID] = append(out[m.ModelID], Replica{
				ModelID: m.ModelID, NodeID: n.NodeID, NodeName: n.Name, BaseURL: nodeBase(n),
				LocalName: m.LocalName, Loaded: m.State == proto.ModelLoaded, Slots: slots, Route: "ollama",
			})
		}
	}
	return out, nil
}

// SwapOptions says which catalog models would fit on the assignment's node
// if that assignment's model were removed.
func (c *Core) SwapOptions(ctx context.Context, assignmentID string) ([]provision.FitResult, error) {
	a, err := c.Store.GetAssignment(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	n, err := c.Store.GetNode(ctx, a.NodeID)
	if err != nil {
		return nil, err
	}
	assigns, err := c.Store.ListAssignments(ctx, n.NodeID)
	if err != nil {
		return nil, err
	}
	fr, err := c.Prov.Fit(ctx, provision.FitRequest{Nodes: []provision.FitNode{fitNode(n, assigns)}, Exclude: []string{a.ID}})
	if err != nil {
		return nil, err
	}
	return fr.Results, nil
}

// ensureAssignedCached restarts downloads for assigned models the cache does
// not have (e.g. after a Controller restart interrupted them).
func (c *Core) ensureAssignedCached(ctx context.Context) {
	all, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, a := range all {
		if seen[a.Ref] {
			continue
		}
		seen[a.Ref] = true
		if st := c.Models.Status(a.Ref); st.State == "" {
			_, _ = c.Models.Ensure(a.Ref)
		}
	}
}
