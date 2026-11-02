package web

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"aicluster/common/discovery"
	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/gateway"
	"aicluster/controller/store"
)

// WithGateway enables the playground and lets the dashboard show the
// gateway's address. port is the gateway's listen port.
func (s *Server) WithGateway(g *gateway.Gateway, port int) *Server {
	s.gw, s.gwPort = g, port
	return s
}

func (s *Server) keyRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/keys", s.listKeys)
	api.HandleFunc("POST /api/v1/keys", s.createKey)
	api.HandleFunc("DELETE /api/v1/keys/{id}", s.deleteKey)
	api.HandleFunc("GET /api/v1/playground/models", s.playgroundModels)
	api.HandleFunc("POST /api/v1/playground/chat/completions", s.playgroundChat)
	api.HandleFunc("POST /api/v1/playground/embeddings", s.playgroundEmbeddings)
}

// gatewayURLs are the addresses clients can use, one per local IPv4.
func (s *Server) gatewayURLs() []string {
	if s.gw == nil || s.gwPort == 0 {
		return []string{}
	}
	port := strconv.Itoa(s.gwPort)
	out := []string{}
	for _, ip := range discovery.LocalIPv4s() {
		out = append(out, "http://"+net.JoinHostPort(ip, port)+"/v1")
	}
	return append(out, "http://localhost:"+port+"/v1")
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.core.Store.ListAPIKeys(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, keys)
}

type CreateKeyRequest struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	RPM    int      `json:"rpm"`
	// Memory is the memory space the key's chats use; "" = none (§18).
	Memory string `json:"memory"`
}

type CreatedKey struct {
	*store.APIKey
	// Key is the secret. It is returned only here, once.
	Key string `json:"key"`
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req CreateKeyRequest
	if err := httpx.ReadJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" || req.RPM < 0 {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "a name is required")
		return
	}
	if req.Memory != "" && !spaceRe.MatchString(req.Memory) {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "memory space must be lowercase letters, digits, . _ -")
		return
	}
	k, secret, err := s.core.Store.CreateAPIKey(r.Context(), strings.TrimSpace(req.Name), req.Models, req.RPM)
	if err == nil && req.Memory != "" {
		if err = s.core.Store.SetAPIKeyMemory(r.Context(), k.ID, req.Memory); err == nil {
			k.Memory = req.Memory
		}
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("api key created", "name", k.Name, "prefix", k.Prefix)
	httpx.WriteJSON(w, http.StatusCreated, CreatedKey{APIKey: k, Key: secret})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	err := s.core.Store.DeleteAPIKey(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such key")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("api key revoked", "id", r.PathValue("id"))
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

// The playground runs through the same gateway code as API clients, as the
// signed-in admin, so the dashboard needs no API key.
// The dashboard's chat uses the "default" memory space (§18).
var dashboardPrincipal = &gateway.Principal{Name: "dashboard", Admin: true, Memory: "default"}

func (s *Server) playgroundModels(w http.ResponseWriter, r *http.Request) {
	if s.gw == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "gateway_disabled", "the gateway is not running")
		return
	}
	ms, err := s.gw.Models(r.Context(), dashboardPrincipal)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ms)
}

func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) {
	if s.gw == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "gateway_disabled", "the gateway is not running")
		return
	}
	s.gw.Infer(w, r, dashboardPrincipal, "/v1/chat/completions")
}

func (s *Server) playgroundEmbeddings(w http.ResponseWriter, r *http.Request) {
	if s.gw == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "gateway_disabled", "the gateway is not running")
		return
	}
	s.gw.Infer(w, r, dashboardPrincipal, "/v1/embeddings")
}
