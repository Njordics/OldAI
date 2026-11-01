package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The registry shim listens on loopback only. The runtime (Ollama) pulls
// "127.0.0.1:7452/library/<model>" from it as an insecure registry, and the
// shim forwards each request to the Controller's model cache over mTLS, so
// the runtime never needs the node certificate or internet access.

func (a *Agent) listenShim() error {
	ln, err := net.Listen("tcp", a.cfg.RegistryShimListen)
	if err != nil {
		return err
	}
	a.shimLn = ln
	return nil
}

// ShimHost is "127.0.0.1:<port>" of the running shim.
func (a *Agent) ShimHost() string {
	if a.shimLn == nil {
		return ""
	}
	return a.shimLn.Addr().String()
}

func (a *Agent) serveShim(ctx context.Context) {
	srv := &http.Server{Handler: http.HandlerFunc(a.handleShim), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(a.shimLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("registry shim stopped", "err", err)
	}
}

func (a *Agent) handleShim(w http.ResponseWriter, r *http.Request) {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !strings.HasPrefix(r.URL.Path, "/v2/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	a.mu.RLock()
	ad := a.adopted
	a.mu.RUnlock()
	if ad == nil {
		http.Error(w, "node is not adopted", http.StatusServiceUnavailable)
		return
	}
	client := a.shimClient(ad)
	var lastErr error
	for _, ep := range a.endpointsOf(ad) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, ep+r.URL.Path, nil)
		if err != nil {
			continue
		}
		for _, h := range []string{"Range", "Accept", "If-Range"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Docker-Content-Digest", "Last-Modified", "Etag"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	a.log.Warn("registry shim: controller unreachable", "err", lastErr)
	http.Error(w, "controller unreachable", http.StatusBadGateway)
}

// shimClient is a long-timeout variant of the heartbeat client: blobs are
// large and stream for minutes.
func (a *Agent) shimClient(ad *adopted) *http.Client {
	a.shimMu.Lock()
	defer a.shimMu.Unlock()
	if a.shimFor != ad {
		c := controllerClient(ad)
		c.Timeout = 0
		a.shimHTTP, a.shimFor = c, ad
	}
	return a.shimHTTP
}

func (a *Agent) endpointsOf(ad *adopted) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]string(nil), ad.Controller.Endpoints...)
}
