package web

import (
	"errors"
	"net/http"
	"strings"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/modelcache"
	"aicluster/controller/provision"
	"aicluster/controller/store"
)

func (s *Server) modelRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/catalog", s.catalog)
	api.HandleFunc("GET /api/v1/models", s.models)
	api.HandleFunc("POST /api/v1/models/install", s.install)
	api.HandleFunc("POST /api/v1/assignments/{id}/swap", s.swap)
	api.HandleFunc("GET /api/v1/assignments/{id}/swap-options", s.swapOptions)
	api.HandleFunc("POST /api/v1/assignments/{id}/retry", s.retry)
	api.HandleFunc("DELETE /api/v1/assignments/{id}", s.unassign)
}

// FitSummary is the catalog view of one model on one node.
type FitSummary struct {
	Fits      bool                  `json:"fits"`
	Quant     string                `json:"quant,omitempty"`
	Placement string                `json:"placement,omitempty"`
	Need      int64                 `json:"need,omitempty"`
	Reason    string                `json:"reason"`
	Options   []provision.FitOption `json:"options"`
}

type CatalogView struct {
	*provision.Catalog
	// Fit[model_id][node_id] for every adopted node.
	Fit map[string]map[string]FitSummary `json:"fit"`
	// Cache[ref] for every quant the Controller has (or is fetching).
	Cache map[string]modelcache.Entry `json:"cache"`
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	cat, err := s.core.Prov.Catalog(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", err.Error())
		return
	}
	view := CatalogView{Catalog: cat, Fit: map[string]map[string]FitSummary{}, Cache: map[string]modelcache.Entry{}}
	if fr, err := s.core.CatalogFit(r.Context()); err == nil {
		for _, res := range fr.Results {
			if view.Fit[res.ModelID] == nil {
				view.Fit[res.ModelID] = map[string]FitSummary{}
			}
			view.Fit[res.ModelID][res.NodeID] = FitSummary{
				Fits: res.Fits, Quant: res.Quant, Placement: res.Placement, Need: res.Need, Reason: res.Reason, Options: res.Options,
			}
		}
	} else {
		s.log.Warn("catalog fit failed", "err", err)
	}
	for _, e := range s.core.Models.List() {
		view.Cache[e.Ref] = e
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	ms, err := s.core.ModelStatuses(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ms)
}

func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	var req core.InstallRequest
	if err := httpx.ReadJSON(r, &req); err != nil || req.ModelID == "" {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "model_id is required")
		return
	}
	out, err := s.core.Install(r.Context(), req)
	if err != nil {
		s.modelError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, out)
}

type SwapRequest struct {
	ModelID string `json:"model_id"`
	Quant   string `json:"quant,omitempty"`
}

func (s *Server) swap(w http.ResponseWriter, r *http.Request) {
	var req SwapRequest
	if err := httpx.ReadJSON(r, &req); err != nil || req.ModelID == "" {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "model_id is required")
		return
	}
	a, err := s.core.Swap(r.Context(), r.PathValue("id"), req.ModelID, req.Quant)
	if err != nil {
		s.modelError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, a)
}

func (s *Server) swapOptions(w http.ResponseWriter, r *http.Request) {
	res, err := s.core.SwapOptions(r.Context(), r.PathValue("id"))
	if err != nil {
		s.modelError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	if err := s.core.RetryAssignment(r.Context(), r.PathValue("id")); err != nil {
		s.modelError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) unassign(w http.ResponseWriter, r *http.Request) {
	if err := s.core.RemoveAssignment(r.Context(), r.PathValue("id")); err != nil {
		s.modelError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) modelError(w http.ResponseWriter, err error) {
	var fe *core.FitError
	switch {
	case errors.As(err, &fe):
		httpx.WriteError(w, http.StatusConflict, "does_not_fit", err.Error())
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, err.Error())
	case errors.Is(err, core.ErrUnknownModel):
		httpx.WriteError(w, http.StatusNotFound, "unknown_model", err.Error())
	case errors.Is(err, core.ErrAlreadyThere), errors.Is(err, core.ErrNotAdopted), errors.Is(err, core.ErrNoNodeFits):
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, err.Error())
	case strings.Contains(err.Error(), "provisioner:"):
		httpx.WriteError(w, http.StatusServiceUnavailable, "provisioner_unavailable", err.Error())
	default:
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
	}
}
