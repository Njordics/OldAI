// Package gateway is the cluster's single OpenAI-compatible endpoint
// (docs/protocol.md §9). It authenticates API keys, picks the least-busy
// online node that holds the requested model, forwards the request to that
// node's agent over mTLS, and streams the answer back. Requests wait in a
// per-model queue when every replica is busy, and fail over to another
// replica if a node fails before answering.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"aicluster/controller/core"
)

const (
	maxBody       = 32 << 20
	suspectFor    = 10 * time.Second
	snapshotTTL   = time.Second
	touchEvery    = time.Minute
	headerTimeout = 10 * time.Minute // first token can wait for a model to load
)

type Config struct {
	MaxQueue     int           // waiting requests per model (default 64)
	QueueTimeout time.Duration // how long a request may wait (default 60 s)
	MaxRetries   int           // failovers per request (default 2)
}

// Principal is who is calling: an API key, or the dashboard (Admin).
type Principal struct {
	KeyID  string
	Name   string
	Models []string // empty = all
	RPM    int
	Admin  bool
}

func (p *Principal) allows(model string) bool {
	return p.Admin || len(p.Models) == 0 || slices.Contains(p.Models, model)
}

type Gateway struct {
	core *core.Core
	log  *slog.Logger
	cfg  Config

	mu       sync.Mutex
	inflight map[string]int       // node id -> requests in progress via this gateway
	suspect  map[string]time.Time // node id -> avoid until
	queued   map[string]int       // model -> waiting requests
	released chan struct{}        // closed and replaced whenever a slot frees up
	snap     *snapshot
	clients  map[string]*http.Client
	windows  map[string]*rateWindow
	touched  map[string]time.Time
}

type snapshot struct {
	at       time.Time
	replicas map[string][]core.Replica
	aliases  map[string]string
	caps     map[string][]string
}

type rateWindow struct {
	start time.Time
	n     int
}

func New(c *core.Core, cfg Config, log *slog.Logger) *Gateway {
	if cfg.MaxQueue == 0 {
		cfg.MaxQueue = 64
	}
	if cfg.QueueTimeout == 0 {
		cfg.QueueTimeout = 60 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	return &Gateway{
		core: c, log: log, cfg: cfg,
		inflight: map[string]int{}, suspect: map[string]time.Time{}, queued: map[string]int{},
		released: make(chan struct{}), clients: map[string]*http.Client{},
		windows: map[string]*rateWindow{}, touched: map[string]time.Time{},
	}
}

// Handler is the public API, authenticated by API key.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.Handle("GET /v1/models", g.auth(g.listModels))
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		mux.Handle("POST "+p, g.auth(func(w http.ResponseWriter, r *http.Request, pr *Principal) { g.Infer(w, r, pr, p) }))
	}
	return mux
}

// Serve runs the public API on ln until ctx ends.
func (g *Gateway) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(g.log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// oaiError writes an error in OpenAI's format so client SDKs show it well.
func oaiError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func (g *Gateway) auth(h func(http.ResponseWriter, *http.Request, *Principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			oaiError(w, http.StatusUnauthorized, "invalid_request_error", "missing_api_key",
				"Missing API key. Send 'Authorization: Bearer aic_...'; create keys in the dashboard under Connect.")
			return
		}
		k, err := g.core.Store.VerifyAPIKey(r.Context(), strings.TrimSpace(secret))
		if err != nil {
			oaiError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Invalid API key.")
			return
		}
		p := &Principal{KeyID: k.ID, Name: k.Name, Models: k.Models, RPM: k.RPM}
		if !g.allowRate(p) {
			w.Header().Set("Retry-After", "60")
			oaiError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
				fmt.Sprintf("Rate limit of %d requests per minute reached for this key.", p.RPM))
			return
		}
		g.touch(k.ID)
		h(w, r, p)
	})
}

func (g *Gateway) allowRate(p *Principal) bool {
	if p.RPM <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	win := g.windows[p.KeyID]
	if win == nil || now.Sub(win.start) >= time.Minute {
		win = &rateWindow{start: now}
		g.windows[p.KeyID] = win
	}
	if win.n >= p.RPM {
		return false
	}
	win.n++
	return true
}

// touch records key use, at most once a minute per key.
func (g *Gateway) touch(keyID string) {
	g.mu.Lock()
	now := time.Now()
	due := now.Sub(g.touched[keyID]) > touchEvery
	if due {
		g.touched[keyID] = now
	}
	g.mu.Unlock()
	if due {
		go func() { _ = g.core.Store.TouchAPIKey(context.Background(), keyID, now.UTC()) }()
	}
}

// snapshotNow returns the routing table, rebuilt at most once a second.
func (g *Gateway) snapshotNow(ctx context.Context) (*snapshot, error) {
	g.mu.Lock()
	s := g.snap
	g.mu.Unlock()
	if s != nil && time.Since(s.at) < snapshotTTL {
		return s, nil
	}
	reps, err := g.core.Replicas(ctx)
	if err != nil {
		return nil, err
	}
	s = &snapshot{at: time.Now(), replicas: reps, aliases: map[string]string{}, caps: map[string][]string{}}
	if cat, err := g.core.Prov.Catalog(ctx); err == nil {
		for _, f := range cat.Families {
			for a, target := range f.Aliases {
				s.aliases[a] = target
			}
			for _, v := range f.Variants {
				s.caps[v.ID] = f.Capabilities
			}
		}
	}
	g.mu.Lock()
	g.snap = s
	g.mu.Unlock()
	return s, nil
}

// ModelInfo is one entry of GET /v1/models.
type ModelInfo struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Created   int64  `json:"created"`
	OwnedBy   string `json:"owned_by"`
	AICluster struct {
		Replicas     int      `json:"replicas"`
		Loaded       int      `json:"loaded"`
		Nodes        []string `json:"nodes"`
		Capabilities []string `json:"capabilities"`
		// Pooled copies are split across several nodes; Nodes lists their
		// leaders.
		Pooled int `json:"pooled"`
	} `json:"aicluster"`
}

// Models lists the models callable by p right now.
func (g *Gateway) Models(ctx context.Context, p *Principal) ([]ModelInfo, error) {
	s, err := g.snapshotNow(ctx)
	if err != nil {
		return nil, err
	}
	out := []ModelInfo{}
	for id, reps := range s.replicas {
		if !p.allows(id) {
			continue
		}
		mi := ModelInfo{ID: id, Object: "model", OwnedBy: "aicluster"}
		mi.AICluster.Replicas = len(reps)
		for _, r := range reps {
			mi.AICluster.Nodes = append(mi.AICluster.Nodes, r.NodeName)
			if r.Loaded {
				mi.AICluster.Loaded++
			}
			if len(r.Pool) > 0 {
				mi.AICluster.Pooled++
			}
		}
		mi.AICluster.Capabilities = s.caps[id]
		if mi.AICluster.Capabilities == nil {
			mi.AICluster.Capabilities = []string{}
		}
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request, p *Principal) {
	ms, err := g.Models(r.Context(), p)
	if err != nil {
		oaiError(w, http.StatusInternalServerError, "server_error", "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": ms})
}

func (g *Gateway) client(nodeID string) *http.Client {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.clients[nodeID]
	if !ok {
		c = g.core.NodeClient(nodeID, 0)
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.ResponseHeaderTimeout = headerTimeout
			tr.MaxIdleConnsPerHost = 32
		}
		g.clients[nodeID] = c
	}
	return c
}

var errQueueFull = errors.New("queue full")

// acquire picks the least-busy replica with a free slot, waiting in the
// model's queue if all are busy. Loaded replicas win over on-disk ones.
func (g *Gateway) acquire(ctx context.Context, reps []core.Replica, tried map[string]bool, deadline time.Time) (*core.Replica, error) {
	model := reps[0].ModelID
	queued := false
	defer func() {
		if queued {
			g.mu.Lock()
			g.queued[model]--
			g.mu.Unlock()
		}
	}()
	for {
		g.mu.Lock()
		var best *core.Replica
		var bestScore float64
		now := time.Now()
		usable := 0
		for i := range reps {
			r := &reps[i]
			if tried[r.NodeID] || now.Before(g.suspect[r.NodeID]) {
				continue
			}
			usable++
			slots := max(r.Slots, 1)
			if g.inflight[r.NodeID] >= slots {
				continue
			}
			score := float64(g.inflight[r.NodeID]) / float64(slots)
			if !r.Loaded {
				score += 1 // loading costs seconds; use only if nothing loaded is free
			}
			if best == nil || score < bestScore {
				best, bestScore = r, score
			}
		}
		if best != nil {
			g.inflight[best.NodeID]++
			g.mu.Unlock()
			return best, nil
		}
		if usable == 0 {
			g.mu.Unlock()
			return nil, nil // every replica failed or is suspect
		}
		if !queued {
			if g.queued[model] >= g.cfg.MaxQueue {
				g.mu.Unlock()
				return nil, errQueueFull
			}
			g.queued[model]++
			queued = true
		}
		wait := g.released
		g.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Until(deadline)):
			return nil, context.DeadlineExceeded
		}
	}
}

func (g *Gateway) release(nodeID string, failed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight[nodeID]--
	if failed {
		g.suspect[nodeID] = time.Now().Add(suspectFor)
	}
	close(g.released)
	g.released = make(chan struct{})
}

// Infer serves one OpenAI request on behalf of p. path is the OpenAI path,
// e.g. "/v1/chat/completions". The dashboard playground calls this directly.
func (g *Gateway) Infer(w http.ResponseWriter, r *http.Request, p *Principal, path string) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		oaiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "Request body too large.")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		oaiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body is not valid JSON.")
		return
	}
	asked, _ := req["model"].(string)
	if asked == "" {
		oaiError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "The 'model' field is required.")
		return
	}
	snap, err := g.snapshotNow(r.Context())
	if err != nil {
		oaiError(w, http.StatusInternalServerError, "server_error", "internal", err.Error())
		return
	}
	model := asked
	if target, ok := snap.aliases[asked]; ok {
		model = target
	}
	if !p.allows(model) {
		oaiError(w, http.StatusForbidden, "permission_error", "model_not_allowed", fmt.Sprintf("This API key may not use %q.", model))
		return
	}
	reps := snap.replicas[model]
	if len(reps) == 0 {
		oaiError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("Model %q is not installed on any online node. GET /v1/models lists what is available.", asked))
		return
	}
	stream, _ := req["stream"].(bool)

	deadline := start.Add(g.cfg.QueueTimeout)
	tried := map[string]bool{}
	for attempt := 0; attempt <= g.cfg.MaxRetries; attempt++ {
		rep, err := g.acquire(r.Context(), reps, tried, deadline)
		switch {
		case errors.Is(err, errQueueFull):
			w.Header().Set("Retry-After", "5")
			oaiError(w, http.StatusTooManyRequests, "rate_limit_error", "queue_full", fmt.Sprintf("Every node serving %q is busy and the queue is full.", model))
			return
		case errors.Is(err, context.DeadlineExceeded):
			w.Header().Set("Retry-After", "10")
			oaiError(w, http.StatusServiceUnavailable, "server_error", "queue_timeout", fmt.Sprintf("Timed out waiting for a free node serving %q.", model))
			return
		case err != nil:
			return // client went away
		case rep == nil:
			oaiError(w, http.StatusBadGateway, "server_error", "no_healthy_node", fmt.Sprintf("No node serving %q answered.", model))
			return
		}
		tried[rep.NodeID] = true

		req["model"] = rep.LocalName
		out, _ := json.Marshal(req)
		resp, err := g.forward(r.Context(), rep, path, out)
		if err != nil || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable {
			if resp != nil {
				resp.Body.Close()
			}
			g.release(rep.NodeID, true)
			if r.Context().Err() != nil {
				return
			}
			g.log.Warn("node failed before answering; trying another", "node", rep.NodeName, "model", model, "err", err)
			continue
		}
		g.respond(w, resp, rep, model, stream)
		resp.Body.Close()
		g.release(rep.NodeID, false)
		g.log.Info("request served", "key", p.Name, "model", model, "node", rep.NodeName, "status", resp.StatusCode,
			"stream", stream, "took", time.Since(start).Round(time.Millisecond))
		return
	}
	oaiError(w, http.StatusBadGateway, "server_error", "no_healthy_node", fmt.Sprintf("No node serving %q answered.", model))
}

func (g *Gateway) forward(ctx context.Context, rep *core.Replica, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rep.BaseURL+"/v1/infer/"+inferRoute(rep)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return g.client(rep.NodeID).Do(req)
}

// respond streams the node's answer to the client. Non-streaming JSON gets
// the cluster's model name back in place of the runtime's local name.
func (g *Gateway) respond(w http.ResponseWriter, resp *http.Response, rep *core.Replica, model string, stream bool) {
	ct := resp.Header.Get("Content-Type")
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-AICluster-Node", rep.NodeName)
	if !stream && strings.HasPrefix(ct, "application/json") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err == nil {
			var obj map[string]any
			if json.Unmarshal(data, &obj) == nil {
				if _, ok := obj["model"]; ok {
					obj["model"] = model
					data, _ = json.Marshal(obj)
				}
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	// Server-sent events arrive line by line; swap the runtime's model name
	// for the cluster's in each line, then flush so tokens reach the client
	// as soon as they are produced.
	local, _ := json.Marshal(rep.LocalName)
	public, _ := json.Marshal(model)
	from := append([]byte(`"model":`), local...)
	to := append([]byte(`"model":`), public...)
	br := bufio.NewReaderSize(resp.Body, 16<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(bytes.Replace(line, from, to, 1)); werr != nil {
				return
			}
			if fl != nil && (br.Buffered() == 0 || bytes.Equal(line, []byte("\n"))) {
				fl.Flush()
			}
		}
		if err != nil {
			if fl != nil {
				fl.Flush()
			}
			return
		}
	}
}

// inferRoute is the agent route for a replica: the local Ollama, or the
// llama-server of a pooled model the node leads.
func inferRoute(rep *core.Replica) string {
	if rep.Route != "" {
		return rep.Route
	}
	return "ollama"
}
