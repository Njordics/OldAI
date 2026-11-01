package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"aicluster/common/proto"
	"aicluster/common/registry"
	"aicluster/slave-agent/runtime"
)

// Model reconciler: makes the runtime hold exactly the models in the desired
// state (docs/protocol.md §5.1). Unassigned models are unloaded at once (a
// swap frees memory immediately) and deleted from disk after gc_grace_s, so
// a model that comes back soon doesn't have to be downloaded again.

const (
	fileManaged      = "managed-models.json"
	pullRetryBackoff = 30 * time.Second
	defaultGCGrace   = 24 * time.Hour
)

type managedModel struct {
	Ref          string    `json:"ref"`
	UnassignedAt time.Time `json:"unassigned_at,omitempty"`
}

type modelTracker struct {
	obs      map[string]*proto.ObservedModel // by assignment id
	failedAt map[string]time.Time
	loaded   map[string]bool // local names this agent loaded
	managed  map[string]managedModel
	rtErr    string
}

func newModelTracker() modelTracker {
	return modelTracker{
		obs: map[string]*proto.ObservedModel{}, failedAt: map[string]time.Time{},
		loaded: map[string]bool{}, managed: map[string]managedModel{},
	}
}

func (a *Agent) loadManaged() {
	var m map[string]managedModel
	if a.readJSON(fileManaged, &m) == nil {
		a.models.managed = m
	}
}

func (a *Agent) saveManaged() {
	if err := a.writeJSON(fileManaged, a.models.managed, 0o644); err != nil {
		a.log.Error("persist managed models", "err", err)
	}
}

func toRuntimeModel(dm proto.DesiredModel, shimHost string) (runtime.Model, error) {
	ref, err := registry.ParseRef(dm.Source.Ref)
	if err != nil {
		return runtime.Model{}, err
	}
	return runtime.Model{Ref: ref, LocalName: ref.LocalName(), RegistryHost: shimHost}, nil
}

func (a *Agent) modelLoop(ctx context.Context) {
	if a.rt == nil {
		return
	}
	t := time.NewTicker(a.cfg.ReconcileEvery)
	defer t.Stop()
	for {
		a.reconcileModels(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.reconcileC:
		case <-t.C:
		}
	}
}

// setObs updates one model's observed state; state changes (not mere
// progress) trigger an immediate heartbeat.
func (a *Agent) setObs(dm proto.DesiredModel, local string, fn func(o *proto.ObservedModel)) {
	a.mu.Lock()
	o := a.models.obs[dm.AssignmentID]
	if o == nil {
		o = &proto.ObservedModel{AssignmentID: dm.AssignmentID, ModelID: dm.ModelID, Variant: dm.Variant,
			Runtime: dm.Runtime, LocalName: local, State: proto.ModelPending}
		a.models.obs[dm.AssignmentID] = o
	}
	before := o.State
	fn(o)
	after := o.State
	a.mu.Unlock()
	if before != after {
		nudge(a.wakeHB)
	}
}

func (a *Agent) desiredModels() ([]proto.DesiredModel, time.Duration, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	grace := time.Duration(a.desired.GCGraceS) * time.Second
	if grace <= 0 {
		grace = defaultGCGrace
	}
	return append([]proto.DesiredModel(nil), a.desired.Models...), grace, a.adopted != nil
}

func (a *Agent) stillDesired(assignmentID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, m := range a.desired.Models {
		if m.AssignmentID == assignmentID {
			return true
		}
	}
	return false
}

func (a *Agent) reconcileModels(ctx context.Context) {
	desired, grace, adopted := a.desiredModels()
	if !adopted {
		return
	}
	shim := a.ShimHost()

	if err := a.rt.Check(ctx); err != nil {
		a.mu.Lock()
		a.models.rtErr = err.Error()
		a.mu.Unlock()
		for _, dm := range desired {
			a.setObs(dm, "", func(o *proto.ObservedModel) {
				o.State, o.Error = proto.ModelPending, "runtime unavailable: "+err.Error()
			})
		}
		return
	}
	a.mu.Lock()
	a.models.rtErr = ""
	a.mu.Unlock()

	installed, err := a.rt.Installed(ctx)
	if err != nil {
		a.log.Warn("list installed models", "err", err)
		return
	}

	want := map[string]bool{}
	for _, dm := range desired {
		if m, err := toRuntimeModel(dm, shim); err == nil {
			want[m.LocalName] = true
		}
	}

	// 1. Models no longer assigned: unload now, delete after the grace period.
	a.mu.Lock()
	managed := make(map[string]managedModel, len(a.models.managed))
	for k, v := range a.models.managed {
		managed[k] = v
	}
	a.mu.Unlock()
	dirty := false
	for name, mm := range managed {
		if want[name] {
			continue
		}
		ref, _ := registry.ParseRef(mm.Ref)
		m := runtime.Model{Ref: ref, LocalName: name, RegistryHost: shim}
		if mm.UnassignedAt.IsZero() {
			if err := a.rt.Unload(ctx, m); err != nil {
				a.log.Warn("unload failed", "model", name, "err", err)
			}
			a.mu.Lock()
			delete(a.models.loaded, name)
			mm.UnassignedAt = time.Now().UTC()
			a.models.managed[name] = mm
			a.mu.Unlock()
			a.log.Info("model unassigned; unloaded", "model", name, "delete_after", grace)
			dirty = true
		} else if time.Since(mm.UnassignedAt) > grace {
			if err := a.rt.Remove(ctx, m); err != nil {
				a.log.Warn("remove failed", "model", name, "err", err)
				continue
			}
			a.mu.Lock()
			delete(a.models.managed, name)
			a.mu.Unlock()
			a.log.Info("model deleted from disk", "model", name)
			dirty = true
		}
	}

	// 2. Assigned models: pull, then load the resident ones.
	for _, dm := range desired {
		m, err := toRuntimeModel(dm, shim)
		if err != nil {
			a.setObs(dm, "", func(o *proto.ObservedModel) { o.State, o.Error = proto.ModelFailed, err.Error() })
			continue
		}
		a.mu.Lock()
		if mm, ok := a.models.managed[m.LocalName]; !ok || !mm.UnassignedAt.IsZero() {
			a.models.managed[m.LocalName] = managedModel{Ref: dm.Source.Ref}
			dirty = true
		}
		failedAt := a.models.failedAt[dm.AssignmentID]
		loaded := a.models.loaded[m.LocalName]
		a.mu.Unlock()

		if !installed[m.LocalName] {
			if time.Since(failedAt) < pullRetryBackoff {
				continue
			}
			if err := a.pullModel(ctx, dm, m); err != nil {
				if errors.Is(err, errNoLongerDesired) || ctx.Err() != nil {
					continue
				}
				a.log.Warn("model pull failed", "model", m.LocalName, "err", err)
				a.mu.Lock()
				a.models.failedAt[dm.AssignmentID] = time.Now()
				a.mu.Unlock()
				a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) { o.State, o.Error = proto.ModelFailed, err.Error() })
				continue
			}
			a.log.Info("model downloaded", "model", m.LocalName)
		}
		a.mu.Lock()
		delete(a.models.failedAt, dm.AssignmentID)
		a.mu.Unlock()

		if dm.Residency == proto.ResidencyOnDemand {
			a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) {
				o.State, o.Error, o.Progress = proto.ModelInstalled, "", 1
				o.BytesDone = o.BytesTotal
			})
			continue
		}
		if !loaded {
			a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) { o.State, o.Error, o.Progress = proto.ModelLoading, "", 1 })
			if err := a.rt.Load(ctx, m); err != nil {
				a.log.Warn("model load failed", "model", m.LocalName, "err", err)
				a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) { o.State, o.Error = proto.ModelFailed, "load: "+err.Error() })
				continue
			}
			a.mu.Lock()
			a.models.loaded[m.LocalName] = true
			a.mu.Unlock()
			a.log.Info("model loaded", "model", m.LocalName)
		}
		a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) {
			o.State, o.Error, o.Progress = proto.ModelLoaded, "", 1
			o.BytesDone = o.BytesTotal
		})
	}

	// 3. Forget progress for assignments that are gone.
	a.mu.Lock()
	keep := map[string]bool{}
	for _, dm := range desired {
		keep[dm.AssignmentID] = true
	}
	for id := range a.models.obs {
		if !keep[id] {
			delete(a.models.obs, id)
			delete(a.models.failedAt, id)
		}
	}
	a.mu.Unlock()
	if dirty {
		a.mu.Lock()
		a.saveManaged()
		a.mu.Unlock()
	}
}

var errNoLongerDesired = errors.New("model was unassigned during download")

// pullModel downloads one model, reporting progress, and cancels the
// download if the model is unassigned meanwhile (e.g. swapped away).
func (a *Agent) pullModel(ctx context.Context, dm proto.DesiredModel, m runtime.Model) error {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-a.desiredChanged():
				if !a.stillDesired(dm.AssignmentID) {
					cancel()
					return
				}
			}
		}
	}()
	a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) {
		o.State, o.Error, o.BytesTotal = proto.ModelDownloading, "", dm.Size
	})
	err := a.rt.Pull(pctx, m, func(done, total int64) {
		a.setObs(dm, m.LocalName, func(o *proto.ObservedModel) {
			o.BytesDone = done
			if total > 0 {
				o.BytesTotal = total
				o.Progress = float64(done) / float64(total)
			}
		})
	})
	if err != nil && pctx.Err() != nil && ctx.Err() == nil {
		return errNoLongerDesired
	}
	return err
}

// desiredChanged returns a channel closed at the next desired-state change.
func (a *Agent) desiredChanged() <-chan struct{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.desiredC
}

// modelObservations is the models part of the observed state, plus the
// derived phase and conditions. Caller holds a.mu (read).
func (a *Agent) modelObservationsLocked() ([]proto.ObservedModel, string, []proto.Condition) {
	now := time.Now().UTC()
	var out []proto.ObservedModel
	phase := proto.PhaseReady
	ready := true
	for _, dm := range a.desired.Models {
		o, ok := a.models.obs[dm.AssignmentID]
		if !ok {
			o = &proto.ObservedModel{AssignmentID: dm.AssignmentID, ModelID: dm.ModelID, Variant: dm.Variant, Runtime: dm.Runtime, State: proto.ModelPending}
		}
		out = append(out, *o)
		switch o.State {
		case proto.ModelLoaded:
		case proto.ModelInstalled:
			if dm.Residency != proto.ResidencyOnDemand {
				ready = false
			}
		case proto.ModelFailed:
			phase = proto.PhaseDegraded
			ready = false
		default:
			ready = false
		}
	}
	if !ready && phase == proto.PhaseReady {
		phase = proto.PhaseProvisioning
	}
	conds := []proto.Condition{}
	if len(a.desired.Models) > 0 && a.rt != nil {
		conds = append(conds,
			proto.Condition{Type: "RuntimesReady", Status: a.models.rtErr == "", Reason: a.models.rtErr, Since: now},
			proto.Condition{Type: "ModelsReady", Status: ready, Since: now},
		)
	}
	if len(a.desired.Models) > 0 && a.rt == nil {
		phase = proto.PhaseDegraded
		conds = append(conds, proto.Condition{Type: "RuntimesReady", Status: false, Reason: "no runtime configured", Since: now})
	}
	if out == nil {
		out = []proto.ObservedModel{}
	}
	return out, phase, conds
}

// removeAllModels is used by release without keep_models.
func (a *Agent) removeAllModels(ctx context.Context) {
	if a.rt == nil {
		return
	}
	a.mu.RLock()
	names := map[string]managedModel{}
	for k, v := range a.models.managed {
		names[k] = v
	}
	a.mu.RUnlock()
	for name, mm := range names {
		ref, _ := registry.ParseRef(mm.Ref)
		m := runtime.Model{Ref: ref, LocalName: name}
		_ = a.rt.Unload(ctx, m)
		if err := a.rt.Remove(ctx, m); err != nil {
			a.log.Warn("remove on release failed", "model", name, "err", err)
		}
	}
	a.mu.Lock()
	a.models = newModelTracker()
	_ = os.Remove(a.file(fileManaged))
	a.mu.Unlock()
}

// observedJSON lets the heartbeat detect changes cheaply.
func observedJSON(o *proto.ObservedState) string {
	b, _ := json.Marshal(o)
	return string(b)
}
