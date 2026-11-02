package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"aicluster/common/proto"
	"aicluster/common/selfupdate"
	"aicluster/controller/store"
)

// Rolling updates (protocol §13): the Controller downloads and verifies the
// bundle, installs it on itself through the root updater (which restarts
// it), then updates the Slaves one at a time, waiting for each to come back
// on the new build before starting the next. Progress lives in the setting
// "update_rollout", so it carries on across the Controller's own restart.

const (
	RolloutDownloading = "downloading"
	RolloutController  = "controller"
	RolloutSlaves      = "slaves"
	RolloutDone        = "done"
	RolloutFailed      = "failed"

	NodeWaiting  = "waiting"
	NodeUpdating = "updating"
	NodeUpdated  = "updated"
	NodeFailed   = "failed"
	NodeSkipped  = "skipped"

	bundlePath = "/v1/update/bundle"
)

var (
	// Variables so tests can shorten them.
	RolloutNodeTimeout = 10 * time.Minute
	rolloutSelfTimeout = 5 * time.Minute
	rolloutPoll        = 2 * time.Second
)

type Rollout struct {
	Target    string        `json:"target"`
	Phase     string        `json:"phase"`
	Message   string        `json:"message,omitempty"`
	Error     string        `json:"error,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Nodes     []RolloutNode `json:"nodes"`
}

type RolloutNode struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Active is true while the rollout still has work to do.
func (r *Rollout) Active() bool {
	return r != nil && (r.Phase == RolloutDownloading || r.Phase == RolloutController || r.Phase == RolloutSlaves)
}

// FetchFunc downloads the bundle for commit into dir/bundle.tar.gz and
// returns its path.
type FetchFunc func(ctx context.Context, commit, dir string) (string, error)

type rolloutState struct {
	cur     *Rollout
	updNode string // node whose desired state carries the update
	upd     *proto.AgentUpdate
}

// BuildCommit is the commit this Controller runs.
func (c *Core) BuildCommit() string { return c.cfg.BuildCommit }

// CanSelfUpdate says why this Controller can't install updates, or nil.
func (c *Core) CanSelfUpdate() error {
	if c.cfg.Installer == nil {
		return errors.New("in-place updates are only available on Linux installs from the Controller ISO")
	}
	return c.cfg.Installer.Available()
}

// Rollout returns a copy of the current or last rollout, or nil.
func (c *Core) Rollout() *Rollout {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roll.cur == nil {
		return nil
	}
	r := *c.roll.cur
	r.Nodes = make([]RolloutNode, len(c.roll.cur.Nodes))
	copy(r.Nodes, c.roll.cur.Nodes)
	return &r
}

func (c *Core) loadRollout(ctx context.Context) {
	v, _ := c.Store.Setting(ctx, "update_rollout", "")
	if v == "" {
		return
	}
	var r Rollout
	if json.Unmarshal([]byte(v), &r) == nil {
		c.roll.cur = &r
	}
}

// saveRollout applies fn to the rollout and persists it.
func (c *Core) saveRollout(fn func(r *Rollout)) {
	c.mu.Lock()
	r := c.roll.cur
	if r == nil {
		c.mu.Unlock()
		return
	}
	fn(r)
	r.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(r)
	c.mu.Unlock()
	if err := c.Store.SetSetting(context.Background(), "update_rollout", string(b)); err != nil {
		c.log.Error("save update progress", "err", err)
	}
}

func (c *Core) failRollout(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.log.Error("update failed", "err", msg)
	c.saveRollout(func(r *Rollout) { r.Phase, r.Error, r.Message = RolloutFailed, msg, "" })
}

// StartUpdate begins a rolling update to commit target.
func (c *Core) StartUpdate(target string, fetch FetchFunc) error {
	if target == "" {
		return errors.New("no update to install")
	}
	selfNeeded := !selfupdate.SameCommit(target, c.cfg.BuildCommit)
	if selfNeeded {
		if err := c.CanSelfUpdate(); err != nil {
			return err
		}
	}
	if c.cfg.Installer == nil {
		return errors.New("in-place updates are only available on Linux installs from the Controller ISO")
	}
	c.mu.Lock()
	if c.roll.cur.Active() {
		c.mu.Unlock()
		return errors.New("an update is already running")
	}
	now := time.Now().UTC()
	c.roll.cur = &Rollout{Target: target, Phase: RolloutDownloading, Message: "Downloading the update", StartedAt: now, Nodes: []RolloutNode{}}
	c.mu.Unlock()
	c.saveRollout(func(*Rollout) {})
	c.log.Info("update started", "target", target, "controller_first", selfNeeded)
	c.mu.Lock()
	ctx := c.runCtx
	c.mu.Unlock()
	go c.runRollout(ctx, target, fetch, selfNeeded)
	return nil
}

func (c *Core) runRollout(ctx context.Context, target string, fetch FetchFunc, selfNeeded bool) {
	dir := c.cfg.Installer.Dir()
	bundle, err := fetch(ctx, target, dir)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		c.failRollout("%v", err)
		return
	}
	m, err := selfupdate.Stage(dir, bundle)
	if err != nil {
		c.failRollout("%v", err)
		return
	}
	if !selfupdate.SameCommit(m.Commit, target) {
		c.failRollout("the downloaded bundle is for %.7s, not %.7s", m.Commit, target)
		return
	}
	if !selfNeeded {
		c.saveRollout(func(r *Rollout) { r.Phase, r.Message = RolloutSlaves, "The Controller already runs this version" })
		c.runSlaves(ctx)
		return
	}
	c.saveRollout(func(r *Rollout) {
		r.Phase, r.Message = RolloutController, "Installing on the Controller; it restarts and this page reconnects"
	})
	if err := c.cfg.Installer.Request(target); err != nil {
		c.failRollout("start the updater: %v", err)
		return
	}
	c.log.Info("update handed to the updater; expecting a restart", "target", target)
	// The updater stops this process; still running after the timeout
	// means it never ran.
	select {
	case <-ctx.Done():
	case <-time.After(rolloutSelfTimeout):
		if res, _ := c.cfg.Installer.Result(); res != nil && !res.OK {
			c.failRollout("Controller update failed and was rolled back: %s", res.Error)
		} else {
			c.failRollout("the updater didn't install the update within %s", rolloutSelfTimeout)
		}
	}
}

// resumeRollout picks the rollout up after a restart.
func (c *Core) resumeRollout(ctx context.Context) {
	c.loadRollout(ctx)
	r := c.Rollout()
	if r == nil || !r.Active() {
		return
	}
	switch r.Phase {
	case RolloutDownloading:
		c.failRollout("the Controller restarted while downloading the update; start it again")
	case RolloutController:
		if selfupdate.SameCommit(r.Target, c.cfg.BuildCommit) {
			c.saveRollout(func(r *Rollout) { r.Message = "Checking the updated Controller stays up" })
			go func() {
				// The updater may still roll this build back; nodes wait
				// for its verdict.
				if !c.waitSelfVerdict(ctx, r.Target) {
					return
				}
				c.log.Info("Controller updated", "commit", c.cfg.BuildCommit)
				c.saveRollout(func(r *Rollout) { r.Phase, r.Message = RolloutSlaves, "Controller updated" })
				c.runSlaves(ctx)
			}()
			return
		}
		res, _ := c.cfg.Installer.Result()
		if res != nil && !res.OK {
			c.failRollout("Controller update failed and was rolled back: %s", res.Error)
		} else {
			c.failRollout("the Controller restarted without installing the update")
		}
	case RolloutSlaves:
		go c.runSlaves(ctx)
	}
}

// waitSelfVerdict waits for the updater to report on the Controller's own
// update. A failed verdict means this process is about to be replaced by
// the old version, which reports the failure when it starts.
func (c *Core) waitSelfVerdict(ctx context.Context, target string) bool {
	deadline := time.Now().Add(rolloutSelfTimeout)
	for time.Now().Before(deadline) {
		res, err := c.cfg.Installer.Result()
		if err == nil && res != nil && selfupdate.SameCommit(res.Commit, target) {
			return res.OK
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(rolloutPoll):
		}
	}
	c.failRollout("the updater gave no verdict on the Controller within %s", rolloutSelfTimeout)
	return false
}

// runSlaves updates adopted nodes one at a time, stopping at the first
// failure: a bundle that breaks one node would likely break them all.
func (c *Core) runSlaves(ctx context.Context) {
	target := c.Rollout().Target
	bundle := filepath.Join(c.cfg.Installer.Dir(), "bundle.tar.gz")
	sum, size, err := selfupdate.SHA256File(bundle)
	if err != nil {
		c.failRollout("the downloaded bundle is gone: %v", err)
		return
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		c.failRollout("list nodes: %v", err)
		return
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	c.saveRollout(func(r *Rollout) {
		known := map[string]bool{}
		for _, n := range r.Nodes {
			known[n.NodeID] = true
		}
		for _, n := range nodes {
			if !known[n.NodeID] && (n.State == store.StateAdopted || n.State == store.StateOffline) {
				r.Nodes = append(r.Nodes, RolloutNode{NodeID: n.NodeID, Name: n.Name, State: NodeWaiting})
			}
		}
	})
	upd := &proto.AgentUpdate{Commit: target, Path: bundlePath, SHA256: sum, Size: size}
	for _, rn := range c.Rollout().Nodes {
		if rn.State != NodeWaiting && rn.State != NodeUpdating {
			continue
		}
		state, detail := c.updateNode(ctx, rn.NodeID, upd)
		if ctx.Err() != nil {
			return // shutting down; resumeRollout carries on after the restart
		}
		c.saveRollout(func(r *Rollout) {
			for i := range r.Nodes {
				if r.Nodes[i].NodeID == rn.NodeID {
					r.Nodes[i].State, r.Nodes[i].Detail = state, detail
				}
			}
		})
		if state == NodeFailed {
			c.failRollout("%s: %s. The remaining nodes were left as they are.", rn.Name, detail)
			return
		}
	}
	var updated, skipped int
	for _, n := range c.Rollout().Nodes {
		switch n.State {
		case NodeUpdated:
			updated++
		case NodeSkipped:
			skipped++
		}
	}
	msg := fmt.Sprintf("Update complete: Controller and %d node%s on %.7s", updated, plural(updated), target)
	if skipped > 0 {
		msg += fmt.Sprintf("; %d skipped", skipped)
	}
	c.log.Info("update finished", "updated", updated, "skipped", skipped)
	c.saveRollout(func(r *Rollout) { r.Phase, r.Message = RolloutDone, msg })
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// updateNode puts the update in one node's desired state and waits for the
// node to report the new build.
func (c *Core) updateNode(ctx context.Context, nodeID string, upd *proto.AgentUpdate) (string, string) {
	n, err := c.Store.GetNode(ctx, nodeID)
	if err != nil {
		return NodeSkipped, "no longer in the cluster"
	}
	commit, selfUpd := agentBuild(n)
	switch {
	case n.State != store.StateAdopted:
		return NodeSkipped, "offline; update it later with Update again"
	case selfupdate.SameCommit(commit, upd.Commit):
		return NodeUpdated, "already on this version"
	case selfUpd == "":
		return NodeSkipped, "its agent is too old to update itself; reinstall it once from the current Slave ISO"
	case selfUpd != "yes":
		return NodeSkipped, selfUpd
	}
	c.log.Info("updating node", "node", n.Name, "commit", upd.Commit)
	c.saveRollout(func(r *Rollout) {
		for i := range r.Nodes {
			if r.Nodes[i].NodeID == nodeID {
				r.Nodes[i].State, r.Nodes[i].Detail = NodeUpdating, "downloading"
			}
		}
		r.Message = "Updating " + n.Name
	})
	c.setNodeUpdate(ctx, nodeID, upd)
	defer c.setNodeUpdate(context.WithoutCancel(ctx), nodeID, nil)

	deadline := time.Now().Add(RolloutNodeTimeout)
	lastDetail := ""
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return NodeFailed, "the Controller stopped"
		case <-time.After(rolloutPoll):
		}
		n, err := c.Store.GetNode(ctx, nodeID)
		if err != nil {
			return NodeSkipped, "no longer in the cluster"
		}
		commit, _ := agentBuild(n)
		o := observedUpdate(n)
		verifying := o != nil && o.State == proto.UpdateVerifying
		if selfupdate.SameCommit(commit, upd.Commit) && n.State == store.StateAdopted && !verifying {
			c.log.Info("node updated", "node", n.Name, "commit", upd.Commit)
			return NodeUpdated, ""
		}
		if o != nil && selfupdate.SameCommit(o.Commit, upd.Commit) {
			if o.State == proto.UpdateFailed {
				return NodeFailed, o.Error
			}
			if o.State != lastDetail {
				lastDetail = o.State
				c.saveRollout(func(r *Rollout) {
					for i := range r.Nodes {
						if r.Nodes[i].NodeID == nodeID {
							r.Nodes[i].Detail = o.State
						}
					}
				})
			}
		}
	}
	return NodeFailed, fmt.Sprintf("didn't come back on the new version within %s", RolloutNodeTimeout)
}

func agentBuild(n *store.Node) (commit, selfUpdate string) {
	var o proto.ObservedState
	if len(n.Observed) == 0 || json.Unmarshal(n.Observed, &o) != nil {
		return "", ""
	}
	return o.Versions[proto.VersionAgentCommit], o.Versions[proto.VersionSelfUpdate]
}

func observedUpdate(n *store.Node) *proto.ObservedUpdate {
	var o proto.ObservedState
	if len(n.Observed) == 0 || json.Unmarshal(n.Observed, &o) != nil {
		return nil
	}
	return o.Update
}

func (c *Core) setNodeUpdate(ctx context.Context, nodeID string, upd *proto.AgentUpdate) {
	c.mu.Lock()
	c.roll.updNode, c.roll.upd = nodeID, upd
	c.mu.Unlock()
	c.SyncDesired(ctx, nodeID)
}

// agentUpdateFor is the update in nodeID's desired state, or nil.
func (c *Core) agentUpdateFor(nodeID string) *proto.AgentUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roll.updNode != nodeID || c.roll.upd == nil {
		return nil
	}
	u := *c.roll.upd
	return &u
}

// serveBundle hands nodes the verified bundle being rolled out.
func (c *Core) serveBundle(w http.ResponseWriter, r *http.Request) {
	if c.cfg.Installer == nil {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(c.cfg.Installer.Dir(), "bundle.tar.gz")
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeContent(w, r, "bundle.tar.gz", st.ModTime(), f)
}
