package core

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/controller/store"
)

// serveNodeAPI serves the Slave-facing mTLS API (§8) on the node-API port.
func (c *Core) serveNodeAPI(ctx context.Context) error {
	pool := c.CA.Pool()
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{c.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := pki.VerifyPeer(raw, pool, "", x509.ExtKeyUsageClientAuth)
			if err == nil && pki.NodeIDFromCert(leaf) == "" {
				err = errors.New("client is not a node")
			}
			return err
		},
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/nodes/{id}/heartbeat", c.nodeOnly(c.handleHeartbeat))
	mux.Handle("GET /v1/nodes/{id}/desired-state", c.nodeOnly(c.handleGetDesired))
	mux.Handle("POST /v1/nodes/{id}/renew", c.nodeOnly(c.handleRenew))
	mux.Handle("POST /v1/nodes/{id}/events", c.nodeOnly(c.handleEvent))
	// Read-only model registry backed by the Controller's cache (§8).
	mux.Handle("GET /v2/", c.anyNode(c.Models))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(c.log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(tls.NewListener(c.nodeLn, tlsCfg))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// nodeOnly binds the request to the node named in the client certificate:
// the path {id} must match it and its serial must not be revoked.
func (c *Core) nodeOnly(h func(w http.ResponseWriter, r *http.Request, leaf *x509.Certificate)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, "client certificate required")
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		if pki.NodeIDFromCert(leaf) != r.PathValue("id") {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, "certificate is for another node")
			return
		}
		revoked, err := c.Store.IsRevoked(r.Context(), pki.SerialString(leaf.SerialNumber))
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
			return
		}
		if revoked {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrRevoked, "certificate revoked")
			return
		}
		h(w, r, leaf)
	})
}

// anyNode admits any adopted, unrevoked node (used for shared resources
// such as the model registry).
func (c *Core) anyNode(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, "client certificate required")
			return
		}
		revoked, err := c.Store.IsRevoked(r.Context(), pki.SerialString(r.TLS.PeerCertificates[0].SerialNumber))
		if err != nil || revoked {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrRevoked, "certificate revoked")
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (c *Core) handleHeartbeat(w http.ResponseWriter, r *http.Request, leaf *x509.Certificate) {
	var hb proto.Heartbeat
	if err := httpx.ReadJSON(r, &hb); err != nil || hb.NodeID != r.PathValue("id") {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "malformed heartbeat")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	serial := pki.SerialString(leaf.SerialNumber)
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	health, _ := json.Marshal(hb.Health)

	addrChanged := false
	update := func(n *store.Node) error {
		if n.State == store.StateReleased {
			return errReleased
		}
		addrChanged = n.Addr != "" && n.Addr != remoteIP
		prev := n.State
		setState(n, store.StateAdopted, now)
		switch prev {
		case store.StateEnrolling:
			n.AdoptedAt = now
			c.log.Info("node adopted", "node_id", n.NodeID, "name", n.Name)
		case store.StateOffline:
			c.log.Info("node back online", "node_id", n.NodeID, "name", n.Name)
		}
		if n.BootID != "" && n.BootID != hb.BootID {
			c.log.Info("node rebooted", "node_id", n.NodeID)
		}
		n.BootID, n.LastSeq = hb.BootID, hb.Seq
		n.LastHeartbeat, n.LastSeen = now, now
		n.AgentState, n.ClusterID = proto.AgentAdopted, c.ClusterID
		n.Addr, n.CertSerial, n.Health, n.LastError = remoteIP, serial, health, ""
		if hb.Observed != nil {
			n.Observed, _ = json.Marshal(hb.Observed)
			n.ObservedGen, n.Phase = hb.Observed.ObservedGeneration, hb.Observed.Phase
		}
		return nil
	}
	n, err := c.Store.UpdateNode(ctx, hb.NodeID, update)
	if errors.Is(err, store.ErrNotFound) {
		// A valid, unrevoked cert from our CA proves membership even if the
		// record is gone (e.g. restored from an older backup).
		n = &store.Node{NodeID: hb.NodeID, Name: "node-" + hb.NodeID[:8], FirstSeen: now, State: store.StateEnrolling,
			Port: proto.DefaultAgentPort, DesiredGen: 0}
		if err = update(n); err == nil {
			err = c.Store.PutNode(ctx, n)
		}
	}
	if errors.Is(err, errReleased) {
		httpx.WriteError(w, http.StatusForbidden, proto.ErrRevoked, "node was released")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if hb.Observed != nil && hb.Observed.Network != nil {
		c.noteSlaveMAC(ctx, hb.NodeID, hb.Observed.Network.MAC)
	}
	if addrChanged {
		// Pool leaders dial their workers by address.
		go c.resyncPoolsOf(context.Background(), hb.NodeID)
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	httpx.WriteJSON(w, http.StatusOK, proto.HeartbeatResp{
		V:                   proto.Version,
		DesiredGeneration:   n.DesiredGen,
		CertRenew:           time.Until(leaf.NotAfter) < life/3,
		ControllerEndpoints: c.endpoints,
		HeartbeatIntervalS:  int(c.cfg.HeartbeatInterval / time.Second),
	})
}

var errReleased = errors.New("node released")

func (c *Core) handleGetDesired(w http.ResponseWriter, r *http.Request, _ *x509.Certificate) {
	n, err := c.Store.GetNode(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrUnknownNode, err.Error())
		return
	}
	ds := proto.DesiredState{V: proto.Version, Generation: n.DesiredGen, NodeName: n.Name}
	if len(n.Desired) > 0 {
		_ = json.Unmarshal(n.Desired, &ds)
	}
	httpx.WriteJSON(w, http.StatusOK, ds)
}

func (c *Core) handleRenew(w http.ResponseWriter, r *http.Request, _ *x509.Certificate) {
	var req proto.RenewRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	certPEM, serial, err := c.CA.SignNodeCSR([]byte(req.CSRPEM), id, pki.NodeCertLifetime)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	// The new serial is recorded by the next heartbeat, which presents it.
	c.log.Info("node certificate renewed", "node_id", id, "serial", serial)
	httpx.WriteJSON(w, http.StatusOK, proto.RenewResponse{V: proto.Version, NodeCertPEM: string(certPEM), CAChainPEM: string(c.CA.CertPEM)})
}

func (c *Core) handleEvent(w http.ResponseWriter, r *http.Request, _ *x509.Certificate) {
	var ev map[string]any
	if err := httpx.ReadJSON(r, &ev); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	c.log.Info("node event", "node_id", r.PathValue("id"), "event", ev)
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}
