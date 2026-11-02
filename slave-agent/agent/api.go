package agent

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"aicluster/common/httpx"
	"aicluster/common/pki"
	"aicluster/common/proto"
)

func pemDER(b []byte) []byte {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil
	}
	return block.Bytes
}

func (a *Agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", a.handleInfo)

	mux.HandleFunc("GET /v1/enroll/hello", a.handleHello)
	mux.HandleFunc("POST /v1/enroll/pake/start", a.handlePakeStart)
	mux.HandleFunc("POST /v1/enroll/pake/confirm", a.handlePakeConfirm)
	mux.HandleFunc("POST /v1/enroll/csr", a.handleCSR)
	mux.HandleFunc("POST /v1/enroll/complete", a.handleComplete)

	mux.Handle("GET /v1/inventory", a.controllerOnly(a.handleInventory))
	mux.Handle("PUT /v1/desired-state", a.controllerOnly(a.handlePutDesired))
	mux.Handle("GET /v1/observed-state", a.controllerOnly(a.handleObserved))
	mux.Handle("POST /v1/ops/release", a.controllerOnly(a.handleRelease))
	mux.Handle("GET /v1/jobs/{id}", a.controllerOnly(a.handleJob))
	mux.Handle("/v1/infer/", a.controllerOnly(a.handleInfer))
	mux.HandleFunc("GET /v1/rpc-tunnel/{group}", a.handleRPCTunnel)
	// Other nodes copy model files from this one (§16).
	mux.Handle("GET /v2/blobs/{digest}", a.nodeOnly(a.handlePeerBlob))
	mux.Handle("GET /v1/nettest", a.controllerOnly(a.handleNetTestSend))
	mux.Handle("POST /v1/nettest", a.controllerOnly(a.handleNetTestReceive))
	return mux
}

// controllerOnly re-checks the peer against the *current* adoption: a
// connection opened before a release or re-adoption must not keep its rights.
func (a *Agent) controllerOnly(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		ad := a.adopted
		a.mu.RUnlock()
		if ad == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, "controller certificate required")
			return
		}
		raw := make([][]byte, len(r.TLS.PeerCertificates))
		for i, c := range r.TLS.PeerCertificates {
			raw[i] = c.Raw
		}
		if _, err := pki.VerifyPeer(raw, ad.caPool, pki.ControllerURI(ad.ClusterID), x509.ExtKeyUsageClientAuth); err != nil {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, err.Error())
			return
		}
		h(w, r)
	})
}

func (a *Agent) handleInfo(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	info := proto.Info{V: proto.Version, NodeID: a.nodeID, State: a.stateLocked(), AgentVersion: a.cfg.Version, FP: a.bootstrapFP}
	if a.adopted != nil {
		info.ClusterID = a.adopted.ClusterID
		info.FP = pki.Fingerprint(a.adopted.cert.Certificate[0])
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (a *Agent) handleInventory(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		a.refreshInventory()
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	httpx.WriteJSON(w, http.StatusOK, a.inv)
}

func (a *Agent) handlePutDesired(w http.ResponseWriter, r *http.Request) {
	var ds proto.DesiredState
	if err := httpx.ReadJSON(r, &ds); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if !a.applyDesired(ds) {
		httpx.WriteError(w, http.StatusConflict, proto.ErrStaleGeneration, "generation is older than current")
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, proto.AcceptedGeneration{V: proto.Version, AcceptedGeneration: ds.Generation})
}

// applyDesired stores ds unless it is older than what we have. Re-sending the
// same generation is accepted (idempotent).
func (a *Agent) applyDesired(ds proto.DesiredState) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.adopted == nil {
		return false
	}
	if ds.Generation < a.desired.Generation {
		return false
	}
	if ds.Generation > a.desired.Generation {
		a.log.Info("desired state updated", "generation", ds.Generation)
	}
	a.desired = ds
	a.signalDesiredLocked()
	if ds.NodeName != "" {
		a.adopted.NodeName = ds.NodeName
	}
	if err := a.writeJSON(fileDesired, ds, 0o644); err != nil {
		a.log.Error("persist desired state", "err", err)
	}
	nudge(a.wakeHB)
	return true
}

func (a *Agent) handleObserved(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, a.observed())
}

// observed reports the reconcile result: the generation the agent has
// accepted, and how far its models are.
func (a *Agent) observed() *proto.ObservedState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	models, phase, conds := a.modelObservationsLocked()
	versions := map[string]string{"agent": a.cfg.Version, "kernel": a.inv.Host.Kernel, "os": a.inv.Host.OS}
	if a.rt != nil {
		versions["runtime"] = a.rt.Kind()
	}
	update := a.updateVersions(versions)
	if a.blobFile("sha256:"+strings.Repeat("0", 64)) != "" {
		versions[proto.VersionPeerBlobs] = "yes"
	}
	return &proto.ObservedState{
		ObservedGeneration: a.desired.Generation,
		Phase:              phase,
		Conditions:         conds,
		Versions:           versions,
		Models:             models,
		Network:            a.observedNetworkLocked(),
		Update:             update,
		Driver:             a.observedDriver(),
		GPUs:               append([]proto.GPUInfo{}, a.inv.GPUs...), // [] when none, never null
	}
}

func (a *Agent) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req proto.ReleaseRequest
	_ = httpx.ReadJSON(r, &req)
	job := a.jobs.start("release")
	httpx.WriteJSON(w, http.StatusAccepted, job)
	// Run after the response is flushed: release tears down this mTLS identity.
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := a.release("released by controller", false, req.KeepModels); err != nil {
			a.jobs.finish(job.JobID, err)
			return
		}
		a.jobs.finish(job.JobID, nil)
	}()
}

func (a *Agent) handleJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.jobs.get(r.PathValue("id"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such job")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, j)
}

// jobs tracks async operations. Persistence across restarts arrives with the
// long-running provisioning jobs in step 3.
type jobs struct {
	mu sync.Mutex
	m  map[string]*proto.Job
}

func newJobs() *jobs { return &jobs{m: map[string]*proto.Job{}} }

func (j *jobs) start(kind string) proto.Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := &proto.Job{JobID: ulid.Make().String(), Kind: kind, State: "running", StartedAt: time.Now().UTC()}
	j.m[job.JobID] = job
	return *job
}

func (j *jobs) finish(id string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.m[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	job.FinishedAt, job.Progress, job.State = &now, 1, "succeeded"
	if err != nil {
		job.State, job.Message = "failed", err.Error()
	}
}

func (j *jobs) get(id string) (proto.Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.m[id]
	if !ok {
		return proto.Job{}, false
	}
	return *job, true
}
