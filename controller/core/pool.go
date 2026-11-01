package core

import (
	"context"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"

	"aicluster/common/proto"
	"aicluster/controller/modelcache"
	"aicluster/controller/provision"
	"aicluster/controller/store"
)

// Pooled models: one copy of a model split across several nodes with
// llama.cpp's RPC mode. The leader holds the GGUF file and serves requests;
// each worker runs rpc-server and computes its share of the layers. Every
// member is an assignment row sharing a GroupID, so fit accounting, status
// and removal work per node like any other model.

func (c *Core) installPool(ctx context.Context, req InstallRequest, nodes []*store.Node,
	byNode map[string][]*store.Assignment, has func(string) bool) ([]*store.Assignment, error) {
	explicit := len(req.Nodes) > 0
	var targets []*store.Node
	names := map[string]string{}
	for _, n := range nodes {
		names[n.NodeID] = n.Name
		switch {
		case explicit && slices.Contains(req.Nodes, n.NodeID):
			if n.State != store.StateAdopted {
				return nil, ErrNotAdopted
			}
			if has(n.NodeID) {
				return nil, ErrAlreadyThere
			}
			targets = append(targets, n)
		case !explicit && n.State == store.StateAdopted && !has(n.NodeID):
			targets = append(targets, n)
		}
	}
	if explicit && len(targets) != len(req.Nodes) {
		return nil, store.ErrNotFound
	}
	if len(targets) < 2 {
		return nil, &FitError{Reasons: map[string]string{"pool": "splitting needs at least two adopted nodes"}}
	}
	preq := provision.PoolRequest{Model: req.ModelID, Quant: req.Quant, Explicit: explicit}
	for _, n := range targets {
		preq.Nodes = append(preq.Nodes, fitNode(n, byNode[n.NodeID]))
	}
	plan, err := c.Prov.Pool(ctx, preq)
	if err != nil {
		return nil, err
	}
	if !plan.Fits {
		return nil, &FitError{Reasons: map[string]string{"pool": plan.Reason}}
	}
	if !plan.Pooled {
		return nil, ErrNoPoolNeeded
	}

	group := ulid.Make().String()
	now := time.Now().UTC()
	var rows []*store.Assignment
	for _, m := range plan.Members {
		role, size := store.RoleWorker, int64(float64(plan.Size)*m.Share)
		if m.Leader {
			role, size = store.RoleLeader, plan.Size
		}
		rows = append(rows, &store.Assignment{
			ID: ulid.Make().String(), NodeID: m.NodeID, ModelID: req.ModelID, Quant: plan.Quant, Runtime: "llamacpp",
			Ref: plan.Ref, Size: size, Need: int64(float64(plan.Need) * m.Share), Placement: m.Placement,
			VRAM: m.VRAM, RAM: m.RAM, Residency: proto.ResidencyResident, CreatedAt: now,
			GroupID: group, Role: role, Share: m.Share,
		})
	}
	if err := c.Store.PutAssignments(ctx, rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		c.log.Info("pooled model assigned", "model", r.ModelID, "quant", r.Quant, "node", names[r.NodeID], "role", r.Role, "share", r.Share)
	}
	c.afterAssignmentChange(ctx, rows...)
	return rows, nil
}

func (c *Core) removePool(ctx context.Context, group string) error {
	nodes, err := c.Store.DeleteGroup(ctx, group)
	if err != nil {
		return err
	}
	c.log.Info("pooled model removed", "group", group, "nodes", len(nodes))
	for _, id := range nodes {
		c.SyncDesired(ctx, id)
	}
	return nil
}

// poolDesired builds a pool member's desired model. Workers get theirs at
// once (rpc-server needs no weights); the leader's waits until the
// Controller has the file.
func (c *Core) poolDesired(ctx context.Context, a *store.Assignment, st modelcache.Entry) (proto.DesiredModel, bool) {
	if a.Role == store.RoleLeader && st.State != modelcache.StateReady {
		return proto.DesiredModel{}, false
	}
	members, err := c.Store.ListGroup(ctx, a.GroupID)
	if err != nil || len(members) < 2 {
		return proto.DesiredModel{}, false
	}
	spec := &proto.PoolSpec{GroupID: a.GroupID, Parallel: PoolParallel}
	for _, m := range members {
		n, err := c.Store.GetNode(ctx, m.NodeID)
		if err != nil {
			return proto.DesiredModel{}, false
		}
		if m.Role == store.RoleLeader {
			spec.Leader = m.NodeID
		}
		spec.Members = append(spec.Members, proto.PoolMember{
			NodeID: m.NodeID, Endpoint: nodeBase(n), Share: m.Share, Leader: m.Role == store.RoleLeader,
		})
	}
	if cat, err := c.Prov.Catalog(ctx); err == nil {
		if _, v := cat.Variant(a.ModelID); v != nil {
			spec.Layers = v.Arch["layers"]
		}
	}
	dm := proto.DesiredModel{
		AssignmentID: a.ID, ModelID: a.ModelID, Variant: a.Quant, Runtime: "llamacpp",
		Residency: proto.ResidencyResident, Role: a.Role, Pool: spec,
	}
	if a.Role == store.RoleLeader {
		dm.Source = proto.ModelSource{Type: "ollama", Ref: a.Ref, Digest: st.Digest}
		dm.Size = st.BytesTotal
	}
	return dm, true
}

// poolReadiness maps each pool leader's assignment ID to the other member
// nodes, for pools whose leader and every worker report loaded and whose
// nodes are all online.
func poolReadiness(nodes []*store.Node, all []*store.Assignment) map[string][]string {
	byID := map[string]*store.Node{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	state := map[string]string{} // assignment ID -> observed state
	for _, n := range nodes {
		for _, m := range observedModels(n) {
			state[m.AssignmentID] = m.State
		}
	}
	groups := map[string][]*store.Assignment{}
	for _, a := range all {
		if a.GroupID != "" {
			groups[a.GroupID] = append(groups[a.GroupID], a)
		}
	}
	out := map[string][]string{}
	for _, members := range groups {
		var leader *store.Assignment
		var others []string
		ok := len(members) >= 2
		for _, m := range members {
			n := byID[m.NodeID]
			if n == nil || n.State != store.StateAdopted || state[m.ID] != proto.ModelLoaded {
				ok = false
			}
			if m.Role == store.RoleLeader {
				leader = m
			} else {
				others = append(others, m.NodeID)
			}
		}
		if ok && leader != nil {
			out[leader.ID] = others
		}
	}
	return out
}

// resyncPoolsOf refreshes the desired state of every pool member that
// depends on nodeID (its address changed, so leaders need the new one).
func (c *Core) resyncPoolsOf(ctx context.Context, nodeID string) {
	groups, err := c.Store.GroupsOfNode(ctx, nodeID)
	if err != nil {
		return
	}
	for _, g := range groups {
		members, _ := c.Store.ListGroup(ctx, g)
		for _, m := range members {
			c.SyncDesired(ctx, m.NodeID)
		}
	}
}
