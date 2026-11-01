package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/pairing"
	"aicluster/common/pki"
	"aicluster/common/proto"
)

const (
	enrollLock          = 120 * time.Second
	pairingCodeLifetime = 10 * time.Minute
	maxPairingFailures  = 3
)

// enrollState is the in-memory half of one enrollment attempt (§3).
type enrollState struct {
	nonceS []byte

	// CPace session (only with pairing).
	pakeYA, pakeYB []byte
	pakeCandidate  []byte // key before the confirmation tags match
	pakeKey        []byte // confirmed session key

	// Lock taken by the CSR step.
	lockedUntil time.Time
	clusterID   string
	nonceC      []byte
	caPEM       []byte
	nodeKeyPEM  []byte
	csrPEM      []byte
}

type pairingState struct {
	code     string
	issuedAt time.Time
	failures int
}

func (a *Agent) rotatePairingCode(why string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rotatePairingCodeLocked(why)
}

func (a *Agent) rotatePairingCodeLocked(why string) {
	a.pairing = pairingState{code: pairing.NewCode(), issuedAt: time.Now()}
	a.enroll.pakeKey, a.enroll.pakeCandidate = nil, nil
	// The code goes to a root-only file read by the console status screen,
	// never to the journal.
	if err := os.MkdirAll(filepath.Dir(a.cfg.PairingCodeFile), 0o700); err == nil {
		_ = pki.WriteFileAtomic(a.cfg.PairingCodeFile, []byte(a.pairing.code+"\n"), 0o600)
	}
	a.log.Info("pairing code rotated", "reason", why, "file", a.cfg.PairingCodeFile)
}

func (a *Agent) pairingRotator(ctx context.Context) {
	if !a.cfg.Pairing {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.RLock()
			stale := a.adopted == nil && time.Since(a.pairing.issuedAt) > pairingCodeLifetime &&
				!a.enroll.lockedUntil.After(time.Now())
			a.mu.RUnlock()
			if stale {
				a.rotatePairingCode("expired")
			}
		}
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// enrollGuard rejects enrollment once adopted or while another Controller
// holds the lock.
func (a *Agent) enrollGuardLocked(w http.ResponseWriter) bool {
	if a.adopted != nil {
		httpx.WriteError(w, http.StatusConflict, proto.ErrAlreadyAdopted, "node is adopted")
		return false
	}
	return true
}

func (a *Agent) handleHello(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enrollGuardLocked(w) {
		return
	}
	// A new hello starts a new attempt unless another one holds the lock.
	if !a.enroll.lockedUntil.After(time.Now()) {
		a.enroll = enrollState{nonceS: randBytes(32)}
	}
	mode := proto.PairingNone
	if a.cfg.Pairing {
		mode = proto.PairingCPace
	}
	httpx.WriteJSON(w, http.StatusOK, proto.Hello{
		V:             proto.Version,
		NodeID:        a.nodeID,
		HWFingerprint: a.hwFP,
		NonceS:        a.enroll.nonceS,
		Pairing:       mode,
		Inventory:     a.inv,
		AgentVersion:  a.cfg.Version,
	})
}

func (a *Agent) handlePakeStart(w http.ResponseWriter, r *http.Request) {
	var req proto.PakeStart
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enrollGuardLocked(w) {
		return
	}
	if !a.cfg.Pairing || a.enroll.nonceS == nil || a.enroll.lockedUntil.After(time.Now()) {
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, "call hello first")
		return
	}
	yb, key, err := pairing.SlaveExchange(a.pairing.code, a.nodeID, a.enroll.nonceS, a.bootstrapFP, req.YA)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	a.enroll.pakeYA, a.enroll.pakeYB, a.enroll.pakeCandidate, a.enroll.pakeKey = req.YA, yb, key, nil
	httpx.WriteJSON(w, http.StatusOK, proto.PakeStartResp{V: proto.Version, YB: yb})
}

func (a *Agent) handlePakeConfirm(w http.ResponseWriter, r *http.Request) {
	var req proto.PakeConfirm
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enrollGuardLocked(w) {
		return
	}
	e := &a.enroll
	if e.pakeCandidate == nil {
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, "call pake/start first")
		return
	}
	if !pairing.CheckMAC(e.pakeCandidate, req.TagC, "confirm-c", e.pakeYA, e.pakeYB) {
		e.pakeCandidate = nil
		a.pairing.failures++
		a.log.Warn("pairing attempt failed", "remote", r.RemoteAddr, "failures", a.pairing.failures)
		if a.pairing.failures >= maxPairingFailures {
			a.rotatePairingCodeLocked("too many failures")
		}
		httpx.WriteError(w, http.StatusForbidden, proto.ErrPairingFailed, "wrong pairing code")
		return
	}
	e.pakeKey, e.pakeCandidate = e.pakeCandidate, nil
	httpx.WriteJSON(w, http.StatusOK, proto.PakeConfirmResp{
		V: proto.Version, TagS: pairing.MAC(e.pakeKey, "confirm-s", e.pakeYA, e.pakeYB),
	})
}

func (a *Agent) handleCSR(w http.ResponseWriter, r *http.Request) {
	var req proto.CSRRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enrollGuardLocked(w) {
		return
	}
	e := &a.enroll
	now := time.Now()
	if e.lockedUntil.After(now) && !(e.clusterID == req.ClusterID && bytes.Equal(e.nonceC, req.NonceC)) {
		httpx.WriteError(w, http.StatusConflict, proto.ErrEnrollmentInProgress, "another controller is enrolling this node")
		return
	}
	if e.nonceS == nil || !bytes.Equal(e.nonceS, req.NonceS) {
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, "stale or missing hello nonce")
		return
	}
	if a.cfg.Pairing && e.pakeKey == nil {
		httpx.WriteError(w, http.StatusForbidden, proto.ErrPairingRequired, "pairing code required")
		return
	}
	caSum := pairing.Sum([]byte(req.CACertPEM))
	if !pairing.CheckMAC(e.pakeKey, req.MAC, "csr-req", []byte(req.ClusterID), caSum, req.NonceS, req.NonceC) {
		httpx.WriteError(w, http.StatusForbidden, proto.ErrBadMAC, "csr request MAC mismatch")
		return
	}
	ca, err := pki.ParseCertPEM([]byte(req.CACertPEM))
	if err != nil || !ca.IsCA || req.ClusterID == "" || len(req.NonceC) < 16 {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "invalid CA certificate or request")
		return
	}
	if e.csrPEM == nil {
		keyPEM, csrPEM, err := pki.NewNodeKeyAndCSR(a.nodeID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
			return
		}
		e.nodeKeyPEM, e.csrPEM = keyPEM, csrPEM
	}
	e.lockedUntil = now.Add(enrollLock)
	e.clusterID, e.nonceC, e.caPEM = req.ClusterID, req.NonceC, []byte(req.CACertPEM)
	nudge(a.stateC)

	csrDER := pemDER(e.csrPEM)
	httpx.WriteJSON(w, http.StatusOK, proto.CSRResponse{
		V:      proto.Version,
		CSRPEM: string(e.csrPEM),
		MAC:    pairing.MAC(e.pakeKey, "csr-resp", pairing.Sum(csrDER), req.NonceC),
	})
}

func (a *Agent) handleComplete(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollComplete
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enrollGuardLocked(w) {
		return
	}
	e := &a.enroll
	if !e.lockedUntil.After(time.Now()) || e.csrPEM == nil || req.Controller.ClusterID != e.clusterID {
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, "no enrollment in progress for this cluster")
		return
	}
	if !pairing.CheckMAC(e.pakeKey, req.MAC, "complete", pairing.CompleteMACInput(req)) {
		httpx.WriteError(w, http.StatusForbidden, proto.ErrBadMAC, "complete MAC mismatch")
		return
	}
	pool, err := pki.PoolFromPEM(e.caPEM)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	certDER := pemDER([]byte(req.NodeCertPEM))
	if _, err := pki.VerifyPeer([][]byte{certDER}, pool, pki.NodeURI(a.nodeID), x509.ExtKeyUsageClientAuth); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "node certificate: "+err.Error())
		return
	}
	ad := adoption{
		ClusterID:          e.clusterID,
		Controller:         req.Controller,
		NodeName:           req.NodeName,
		HeartbeatIntervalS: req.HeartbeatIntervalS,
		AdoptedAt:          time.Now().UTC(),
	}
	if err := a.persistAdoption(ad, e.nodeKeyPEM, []byte(req.NodeCertPEM), e.caPEM); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	loaded, err := a.loadAdoption()
	if err != nil {
		a.wipeAdoption()
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "certificate does not match key: "+err.Error())
		return
	}
	a.adopted = loaded
	a.desired = req.DesiredState
	a.signalDesiredLocked()
	_ = a.writeJSON(fileDesired, a.desired, 0o644)
	a.enroll = enrollState{}
	if a.cfg.Pairing {
		_ = os.Remove(a.cfg.PairingCodeFile)
	}
	a.log.Info("adopted", "cluster_id", ad.ClusterID, "node_name", ad.NodeName, "controller", ad.Controller.Endpoints)
	nudge(a.wakeHB)
	nudge(a.stateC)
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}
