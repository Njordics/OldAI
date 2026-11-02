package web

import (
	"errors"
	"net/http"
	"sort"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/gateway"
)

// Benchmarks (protocol §17): measure how fast each model runs on each node.

func (s *Server) benchRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/benchmarks", s.getBench)
	api.HandleFunc("POST /api/v1/benchmarks", s.startBench)
	api.HandleFunc("DELETE /api/v1/benchmarks", s.clearBench)
}

// BenchTarget is a model on a node that can be benchmarked.
type BenchTarget struct {
	ModelID  string   `json:"model_id"`
	NodeID   string   `json:"node_id"`
	NodeName string   `json:"node_name"`
	Loaded   bool     `json:"loaded"`
	Slots    int      `json:"slots"`
	Pool     []string `json:"pool,omitempty"`
}

// BenchView is GET /api/v1/benchmarks.
type BenchView struct {
	Targets []BenchTarget         `json:"targets"`
	Job     *gateway.BenchJob     `json:"job,omitempty"`
	Results []gateway.BenchResult `json:"results"`
}

// BenchRequest is POST /api/v1/benchmarks.
type BenchRequest struct {
	Targets []gateway.BenchTarget `json:"targets"`
	// Parallel is the number of requests at once; 0 = each node's slots.
	Parallel int `json:"parallel"`
}

func (s *Server) gwRequired(w http.ResponseWriter) bool {
	if s.gw == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "gateway_disabled", "the gateway is not running")
		return false
	}
	return true
}

func (s *Server) getBench(w http.ResponseWriter, r *http.Request) {
	if !s.gwRequired(w) {
		return
	}
	reps, err := s.gw.BenchTargets(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	v := BenchView{Targets: []BenchTarget{}, Job: s.gw.BenchJobNow(), Results: s.gw.BenchHistory(r.Context())}
	if v.Results == nil {
		v.Results = []gateway.BenchResult{}
	}
	for _, rep := range reps {
		v.Targets = append(v.Targets, BenchTarget{ModelID: rep.ModelID, NodeID: rep.NodeID, NodeName: rep.NodeName,
			Loaded: rep.Loaded, Slots: rep.Slots, Pool: rep.Pool})
	}
	sort.Slice(v.Targets, func(i, j int) bool {
		a, b := v.Targets[i], v.Targets[j]
		if a.ModelID != b.ModelID {
			return a.ModelID < b.ModelID
		}
		return a.NodeName < b.NodeName
	})
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) startBench(w http.ResponseWriter, r *http.Request) {
	if !s.gwRequired(w) {
		return
	}
	var req BenchRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if req.Parallel < 0 || req.Parallel > 64 {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "parallel must be between 0 and 64")
		return
	}
	job, err := s.gw.StartBench(req.Targets, req.Parallel)
	switch {
	case errors.Is(err, gateway.ErrBenchRunning):
		httpx.WriteError(w, http.StatusConflict, "busy", err.Error())
	case err != nil:
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
	default:
		s.log.Info("benchmark started from the dashboard", "targets", len(req.Targets), "parallel", req.Parallel)
		httpx.WriteJSON(w, http.StatusAccepted, job)
	}
}

func (s *Server) clearBench(w http.ResponseWriter, r *http.Request) {
	if !s.gwRequired(w) {
		return
	}
	if err := s.gw.ClearBench(r.Context()); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}
