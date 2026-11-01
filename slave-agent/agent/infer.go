package agent

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Inference proxy (docs/protocol.md §4): the Controller's gateway sends
// OpenAI-style requests to /v1/infer/<runtime>/v1/..., and the agent passes
// them to the runtime on loopback. The runtime itself is never reachable
// from the network.

var inferPaths = []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/models"}

type inferStats struct {
	inFlight atomic.Int64
}

func (a *Agent) handleInfer(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/infer/")
	route, path, ok := strings.Cut(rest, "/")
	// "ollama" goes to the local Ollama; "llamacpp:<assignment>" to the
	// llama-server of a pooled model this node leads.
	base := ""
	if id, pooled := strings.CutPrefix(route, "llamacpp:"); pooled {
		base = a.poolURL(id)
	} else if a.rt != nil {
		base = a.rt.InferenceURL()
	}
	if base == "" {
		http.Error(w, `{"error":{"message":"model not served on this node","type":"server_error"}}`, http.StatusServiceUnavailable)
		return
	}
	path = "/" + path
	allowed := false
	for _, p := range inferPaths {
		allowed = allowed || path == p
	}
	if !ok || !allowed {
		http.NotFound(w, r)
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.infer.inFlight.Add(1)
	defer a.infer.inFlight.Add(-1)

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = target.Scheme, target.Host, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.Header.Del("Authorization")
		},
		FlushInterval: -1, // stream tokens as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			a.log.Warn("runtime request failed", "path", path, "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"runtime unavailable on this node","type":"server_error"}}`))
		},
	}
	start := time.Now()
	rp.ServeHTTP(w, r)
	a.log.Debug("inference served", "path", path, "took", time.Since(start).Round(time.Millisecond))
}
