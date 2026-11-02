package agent

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/common/selfupdate"
)

// In-place updates (protocol §13). During a rolling update the Controller
// puts an AgentUpdate in this node's desired state; the agent downloads the
// bundle from the Controller, checks its SHA-256, unpacks it and asks the
// root updater to install it. The updater restarts the agent, or rolls back
// and leaves an error in result.json that the restarted agent reports.

// updateWait is how long the agent waits for the updater to restart it.
const updateWait = 5 * time.Minute

type updState struct {
	mu     sync.Mutex
	commit string // of the running binary
	obs    *proto.ObservedUpdate
	failed map[string]bool // commits not to try again until restart
	busy   bool
}

func (a *Agent) initUpdate() {
	a.upd.commit = a.cfg.BuildCommit
	a.upd.failed = map[string]bool{}
	if a.cfg.Installer == nil {
		return
	}
	// The updater's verdict on the last attempt survives the restart.
	res, err := a.cfg.Installer.Result()
	if err != nil {
		a.log.Warn("update result", "err", err)
	}
	if res != nil && !res.OK && !selfupdate.SameCommit(res.Commit, a.upd.commit) {
		a.upd.obs = &proto.ObservedUpdate{Commit: res.Commit, State: proto.UpdateFailed, Error: res.Error}
		a.upd.failed[res.Commit] = true
		a.log.Warn("the last update failed and was rolled back", "commit", res.Commit, "err", res.Error)
	}
}

// BuildCommit is the commit the agent reports as running.
func (a *Agent) BuildCommit() string {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	return a.upd.commit
}

// SetBuildCommit changes the commit the agent reports (tests: stands in for
// the restart into a new binary).
func (a *Agent) SetBuildCommit(c string) {
	a.upd.mu.Lock()
	a.upd.commit, a.upd.obs = c, nil
	a.upd.mu.Unlock()
	nudge(a.wakeHB)
}

// updateVersions adds the update entries to Versions and returns the
// progress report, if any.
func (a *Agent) updateVersions(v map[string]string) *proto.ObservedUpdate {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	if a.upd.commit != "" {
		v[proto.VersionAgentCommit] = a.upd.commit
	}
	switch {
	case a.cfg.Installer == nil:
		v[proto.VersionSelfUpdate] = "not supported on this system"
	case a.cfg.Installer.Available() != nil:
		v[proto.VersionSelfUpdate] = a.cfg.Installer.Available().Error()
	default:
		v[proto.VersionSelfUpdate] = "yes"
	}
	if a.upd.obs == nil {
		// Restarted into the new build: the updater may still roll it back.
		if a.cfg.Installer != nil && a.upd.commit != "" && selfupdate.Verifying(a.cfg.Installer.Dir(), a.upd.commit) {
			return &proto.ObservedUpdate{Commit: a.upd.commit, State: proto.UpdateVerifying}
		}
		return nil
	}
	o := *a.upd.obs
	return &o
}

func (a *Agent) updateLoop(ctx context.Context) {
	for {
		a.mu.RLock()
		ch := a.desiredC
		want := a.desired.Update
		ad := a.adopted
		a.mu.RUnlock()
		if want != nil && ad != nil {
			a.maybeUpdate(ctx, ad, *want)
		} else {
			// The Controller has taken note of any failure; pressing
			// Update again may retry the same build.
			a.upd.mu.Lock()
			if !a.upd.busy {
				a.upd.failed, a.upd.obs = map[string]bool{}, nil
			}
			a.upd.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-time.After(time.Minute):
		}
	}
}

func (a *Agent) maybeUpdate(ctx context.Context, ad *adopted, want proto.AgentUpdate) {
	a.upd.mu.Lock()
	skip := a.upd.busy || a.upd.failed[want.Commit] || selfupdate.SameCommit(want.Commit, a.upd.commit)
	if !skip {
		a.upd.busy = true
	}
	a.upd.mu.Unlock()
	if skip {
		return
	}
	go func() {
		err := a.update(ctx, ad, want)
		a.upd.mu.Lock()
		a.upd.busy = false
		if err != nil {
			a.upd.failed[want.Commit] = true
			a.upd.obs = &proto.ObservedUpdate{Commit: want.Commit, State: proto.UpdateFailed, Error: err.Error()}
		}
		a.upd.mu.Unlock()
		if err != nil {
			a.log.Error("update failed", "commit", want.Commit, "err", err)
			nudge(a.wakeHB)
		}
	}()
}

func (a *Agent) setUpdateState(commit, state string) {
	a.upd.mu.Lock()
	a.upd.obs = &proto.ObservedUpdate{Commit: commit, State: state}
	a.upd.mu.Unlock()
	nudge(a.wakeHB)
}

func (a *Agent) update(ctx context.Context, ad *adopted, want proto.AgentUpdate) error {
	inst := a.cfg.Installer
	if inst == nil {
		return fmt.Errorf("this node can't update itself")
	}
	if err := inst.Available(); err != nil {
		return err
	}
	a.log.Info("downloading update", "commit", want.Commit, "size", want.Size)
	a.setUpdateState(want.Commit, proto.UpdateDownloading)

	client := controllerClient(ad)
	client.Timeout = 10 * time.Minute
	var bundle string
	_, err := a.tryEndpoints(ad, func(base string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+want.Path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("controller returned %s for the update bundle", resp.Status)
		}
		bundle, err = selfupdate.Save(inst.Dir(), resp.Body, want.SHA256)
		return err
	})
	if err != nil {
		return err
	}
	m, err := selfupdate.Stage(inst.Dir(), bundle)
	if err != nil {
		return err
	}
	if !selfupdate.SameCommit(m.Commit, want.Commit) {
		return fmt.Errorf("the bundle is for commit %.7s, not %.7s", m.Commit, want.Commit)
	}
	a.log.Info("installing update; the updater restarts the agent", "commit", want.Commit)
	a.setUpdateState(want.Commit, proto.UpdateInstalling)
	if err := inst.Request(want.Commit); err != nil {
		return fmt.Errorf("start the updater: %w", err)
	}
	// Normally the updater stops this process long before the timer fires.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(updateWait):
	}
	if selfupdate.SameCommit(a.BuildCommit(), want.Commit) {
		return nil
	}
	if res, _ := inst.Result(); res != nil && !res.OK {
		return fmt.Errorf("%s", res.Error)
	}
	return fmt.Errorf("the updater didn't install the update within %s", updateWait)
}
