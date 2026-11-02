package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/drivers"
	"aicluster/controller/store"
)

// GPU drivers (protocol §14). The Controller keeps a copy of Ubuntu's archive
// for the Slaves, which have no internet, and picks a driver for each node's
// NVIDIA cards from the archive's own list of which driver supports which
// card. The node installs it with apt through the Controller.

const (
	aptPath = "/apt/ubuntu" // where the node API serves the archive
	// driverIndexAge is how often the driver list is read again.
	driverIndexAge = 12 * time.Hour
	defaultSuite   = "noble"
)

var aptComponents = []string{"main", "restricted"}

type driverState struct {
	mu      sync.Mutex
	index   map[string]*drivers.Index // by suite
	loaded  map[string]time.Time
	loading map[string]bool
	errs    map[string]string // by suite: why the index couldn't be read
	why     map[string]string // by node: why it has no driver
}

func (c *Core) setupDrivers() {
	if c.cfg.AptMirror == "" {
		return
	}
	c.mirror = &drivers.Mirror{
		Upstream: c.cfg.AptMirror,
		Dir:      filepath.Join(c.cfg.DataDir, "apt"),
		Client:   &http.Client{Timeout: 30 * time.Minute},
		MaxAge:   30 * time.Minute,
		Offline:  c.Models.Offline.Load,
		Log:      c.log.With("component", "apt-mirror"),
	}
	c.drv = driverState{index: map[string]*drivers.Index{}, loaded: map[string]time.Time{}, loading: map[string]bool{},
		errs: map[string]string{}, why: map[string]string{}}
}

// nodeGPUs reads a node's GPUs and kernel from its inventory.
func nodeGPUs(n *store.Node) (gpus []proto.GPUInfo, kernel, suite string) {
	var inv proto.Inventory
	if len(n.Inventory) > 0 && json.Unmarshal(n.Inventory, &inv) == nil {
		gpus, kernel, suite = inv.GPUs, inv.Host.Kernel, inv.Host.Codename
	}
	var obs proto.ObservedState
	if len(n.Observed) > 0 && json.Unmarshal(n.Observed, &obs) == nil && obs.Versions["kernel"] != "" {
		kernel = obs.Versions["kernel"]
	}
	if suite == "" {
		suite = defaultSuite
	}
	return
}

// driverFor picks the driver for a node. known is false while the archive's
// driver list is still being read; the caller keeps what it had.
func (c *Core) driverFor(n *store.Node) (spec *proto.DriverSpec, known bool) {
	if c.mirror == nil {
		return nil, true
	}
	gpus, kernel, suite := nodeGPUs(n)
	var aliases []string
	for _, g := range gpus {
		if g.Vendor == "nvidia" {
			if g.Modalias == "" {
				c.setDriverWhy(n.NodeID, "the node's agent is too old to report its NVIDIA card; update it")
				return nil, true
			}
			aliases = append(aliases, g.Modalias)
		}
	}
	if len(aliases) == 0 {
		c.setDriverWhy(n.NodeID, "")
		return nil, true
	}
	ix := c.driverIndex(suite)
	if ix == nil {
		c.drv.mu.Lock()
		why := c.drv.errs[suite]
		c.drv.mu.Unlock()
		if why != "" {
			c.setDriverWhy(n.NodeID, why)
		}
		return nil, false
	}
	choice, err := ix.Choose(aliases, kernel)
	if err != nil {
		c.setDriverWhy(n.NodeID, err.Error())
		return nil, true
	}
	c.setDriverWhy(n.NodeID, "")
	return &proto.DriverSpec{
		Vendor: "nvidia", Name: choice.Driver.Package, Version: choice.Driver.Version, Packages: choice.Packages,
		Mirror: aptPath, Suites: []string{suite, suite + "-updates", suite + "-security"}, Components: aptComponents,
	}, true
}

func (c *Core) setDriverWhy(nodeID, why string) {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	if why == "" {
		delete(c.drv.why, nodeID)
	} else {
		c.drv.why[nodeID] = why
	}
}

// DriverProblem says why a node with an NVIDIA card has no driver, if so.
func (c *Core) DriverProblem(nodeID string) string {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	return c.drv.why[nodeID]
}

// driverIndex returns the suite's driver list, starting a (re)load in the
// background when it is missing or old. It is nil until the first load.
func (c *Core) driverIndex(suite string) *drivers.Index {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	ix := c.drv.index[suite]
	if (ix == nil || time.Since(c.drv.loaded[suite]) > driverIndexAge) && !c.drv.loading[suite] {
		c.drv.loading[suite] = true
		go c.loadDriverIndex(suite)
	}
	return ix
}

func (c *Core) loadDriverIndex(suite string) {
	ctx, cancel := context.WithTimeout(c.runCtx, 10*time.Minute)
	defer cancel()
	ix := drivers.NewIndex()
	var err error
	for _, s := range []string{suite, suite + "-updates", suite + "-security"} {
		if err = c.addPackages(ctx, ix, s); err != nil {
			break
		}
	}
	c.drv.mu.Lock()
	c.drv.loading[suite] = false
	if err != nil {
		c.drv.errs[suite] = "can't read Ubuntu's driver list: " + err.Error()
		// Try again on a later sync, not in a tight loop.
		c.drv.loaded[suite] = time.Now().Add(-driverIndexAge + 5*time.Minute)
	} else {
		c.drv.index[suite], c.drv.loaded[suite] = ix, time.Now()
		delete(c.drv.errs, suite)
	}
	c.drv.mu.Unlock()
	if err != nil {
		c.log.Warn("GPU driver list unavailable", "suite", suite, "err", err)
	} else {
		c.log.Info("GPU driver list loaded", "suite", suite, "packages", ix.Len())
	}
	// Nodes waiting on the list (or its error) get their desired state now.
	nodes, lerr := c.Store.ListNodes(ctx)
	if lerr != nil {
		return
	}
	for _, n := range nodes {
		// Enrolling too: adoption's own sync may have run before the list.
		if (servesModels(n) || n.State == store.StateEnrolling) && hasNvidia(n) {
			c.SyncDesired(ctx, n.NodeID)
		}
	}
}

func (c *Core) addPackages(ctx context.Context, ix *drivers.Index, suite string) error {
	rel := fmt.Sprintf("dists/%s/restricted/binary-amd64/Packages.gz", suite)
	rc, err := c.mirror.Open(ctx, rel)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	defer rc.Close()
	if err := ix.AddPackages(rc, true); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return nil
}

func hasNvidia(n *store.Node) bool {
	gpus, _, _ := nodeGPUs(n)
	for _, g := range gpus {
		if g.Vendor == "nvidia" {
			return true
		}
	}
	return false
}

// mergeObservedGPUs replaces the inventory's GPU list with the one the agent
// reports, which has memory and model names once a driver is loaded. It
// reports whether anything changed.
func mergeObservedGPUs(n *store.Node, gpus []proto.GPUInfo) bool {
	if gpus == nil {
		return false
	}
	var inv proto.Inventory
	if len(n.Inventory) > 0 && json.Unmarshal(n.Inventory, &inv) != nil {
		return false
	}
	before, _ := json.Marshal(inv.GPUs)
	after, _ := json.Marshal(gpus)
	if string(before) == string(after) {
		return false
	}
	inv.GPUs = gpus
	inv.Accel = proto.AccelSummary{Best: "cpu", GPUCount: len(gpus)}
	for _, g := range gpus {
		inv.Accel.TotalVRAM += g.VRAM
		if g.Compute.API != "" && g.Compute.API != "none" && inv.Accel.Best == "cpu" {
			inv.Accel.Best = g.Compute.API
		}
	}
	n.Inventory, _ = json.Marshal(inv)
	return true
}

var errNoMirror = errors.New("GPU drivers are off on this Controller")

// serveApt serves the archive copy to nodes.
func (c *Core) serveApt() http.Handler {
	if c.mirror == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, errNoMirror.Error(), http.StatusNotFound)
		})
	}
	return http.StripPrefix(aptPath, c.mirror)
}
