// Package web serves the management REST API (docs/protocol.md §10) used by
// the aicluster CLI, and the embedded dashboard that runs on top of it.
package web

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/store"
)

type Server struct {
	core *core.Core
	log  *slog.Logger
}

func New(c *core.Core, log *slog.Logger) *Server { return &Server{core: c, log: log} }

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/cluster", s.cluster)
	api.HandleFunc("GET /api/v1/nodes", s.listNodes)
	api.HandleFunc("GET /api/v1/nodes/{id}", s.getNode)
	api.HandleFunc("POST /api/v1/nodes/{id}/adopt", s.adopt)
	api.HandleFunc("POST /api/v1/nodes/{id}/release", s.release)
	api.HandleFunc("GET /api/v1/settings", s.getSettings)
	api.HandleFunc("PUT /api/v1/settings", s.putSettings)
	s.modelRoutes(api)

	ui, _ := fs.Sub(uiFiles, "ui")
	mux := http.NewServeMux()
	mux.Handle("/api/", s.auth(api))
	mux.HandleFunc("GET /ca.crt", s.caCert)
	mux.HandleFunc("GET /trust-info", s.trustInfo)
	mux.HandleFunc("POST /session", s.login)
	mux.HandleFunc("DELETE /session", s.logout)
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(ui))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, ui, "index.html")
	})
	return securityHeaders(mux)
}

// securityHeaders locks the dashboard to its own origin: no inline script,
// no third-party loads, no framing.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Serve runs the HTTPS management API on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	tlsLn := tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{s.core.Cert()}})
	if err := srv.Serve(tlsLn); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// caCert lets an admin download the cluster CA to trust the dashboard. It is
// public: the certificate is not secret, and its fingerprint is shown on the
// Controller's console so the download can be checked.
func (s *Server) caCert(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="aicluster-ca.crt"`)
	_, _ = w.Write(s.core.CA.CertPEM)
}

type TrustInfo struct {
	CAFP      string   `json:"ca_fp"`
	CAName    string   `json:"ca_name"`
	Permitted []string `json:"permitted"`
}

func (s *Server) trustInfo(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, TrustInfo{
		CAFP:      pki.Fingerprint(s.core.CA.Cert.Raw),
		CAName:    s.core.CA.Cert.Subject.CommonName,
		Permitted: append(append([]string{}, pki.PermittedDNSDomains...), pki.PermittedIPNets...),
	})
}

type ClusterInfo struct {
	ClusterID    string   `json:"cluster_id"`
	CAFP         string   `json:"ca_fp"`
	ControllerFP string   `json:"controller_fp"`
	Endpoints    []string `json:"endpoints"`
	AdoptionMode string   `json:"adoption_mode"`
}

func (s *Server) cluster(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, ClusterInfo{
		ClusterID:    s.core.ClusterID,
		CAFP:         pki.Fingerprint(s.core.CA.Cert.Raw),
		ControllerFP: s.core.CertFP(),
		Endpoints:    s.core.Endpoints(),
		AdoptionMode: s.core.Mode(),
	})
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.core.Store.ListNodes(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if nodes == nil {
		nodes = []*store.Node{}
	}
	// The list view leaves out the large JSON blobs.
	for _, n := range nodes {
		n.Inventory, n.Observed, n.Desired = nil, nil, nil
	}
	httpx.WriteJSON(w, http.StatusOK, nodes)
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.core.Store.GetNode(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such node")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

type AdoptRequest struct {
	PairingCode string `json:"pairing_code"`
}

func (s *Server) adopt(w http.ResponseWriter, r *http.Request) {
	var req AdoptRequest
	_ = httpx.ReadJSON(r, &req)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	err := s.core.Adopt(ctx, r.PathValue("id"), strings.TrimSpace(req.PairingCode), true)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusAccepted, proto.OK{V: proto.Version, OK: true})
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such node")
	case errors.Is(err, core.ErrBusy), errors.Is(err, core.ErrWrongState):
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, err.Error())
	case errors.Is(err, core.ErrPairingRequired):
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrPairingRequired, err.Error())
	case httpx.Code(err) == proto.ErrPairingFailed:
		httpx.WriteError(w, http.StatusForbidden, proto.ErrPairingFailed, "wrong pairing code")
	default:
		httpx.WriteError(w, http.StatusBadGateway, proto.ErrInternal, err.Error())
	}
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	var req proto.ReleaseRequest
	_ = httpx.ReadJSON(r, &req)
	err := s.core.Release(r.Context(), r.PathValue("id"), req.KeepModels)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusAccepted, proto.OK{V: proto.Version, OK: true})
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such node")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
	}
}

type Settings struct {
	AdoptionMode string `json:"adoption_mode"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, Settings{AdoptionMode: s.core.Mode()})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req Settings
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if req.AdoptionMode != "" {
		if err := s.core.SetMode(r.Context(), req.AdoptionMode); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
			return
		}
	}
	s.getSettings(w, r)
}
