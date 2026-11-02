package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The registry shim listens on loopback only. The runtime (Ollama) pulls
// "127.0.0.1:7452/library/<model>" from it as an insecure registry, and the
// shim forwards each request to the Controller's model cache over mTLS, so
// the runtime never needs the node certificate or internet access. apt
// reaches the Controller's copy of Ubuntu's archive the same way, under
// /apt/ (GPU drivers, §14).

func (a *Agent) listenShim() error {
	ln, err := net.Listen("tcp", a.cfg.RegistryShimListen)
	if err != nil {
		return err
	}
	a.shimLn = ln
	// Blob redirects point at "localhost", which may resolve to ::1; listen
	// there too (same port, still loopback). Hosts without IPv6 skip it.
	if addr := ln.Addr().(*net.TCPAddr); addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		if ln6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(addr.Port))); err == nil {
			a.shimLn6 = ln6
		}
	}
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
	if a.shimLn6 != nil {
		go func() { _ = srv.Serve(a.shimLn6) }()
	}
	if err := srv.Serve(a.shimLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("registry shim stopped", "err", err)
	}
}

// directPrefix marks blob downloads that already followed the shim's redirect.
const directPrefix = "/direct"

func (a *Agent) handleShim(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	direct := strings.HasPrefix(path, directPrefix+"/v2/")
	if direct {
		path = strings.TrimPrefix(path, directPrefix)
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !strings.HasPrefix(path, "/v2/") && !strings.HasPrefix(path, "/apt/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Ollama fetches a blob by asking the registry where it lives (a 307 to a
	// CDN on registry.ollama.ai) and then downloading that URL in ranges; a
	// blob answered without the redirect fails with "no Location header".
	// Ollama follows redirects within the same host name itself, so point to
	// the other name for loopback (127.0.0.1 <-> localhost).
	if !direct && r.Method == http.MethodGet && strings.Contains(path, "/blobs/") {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			host, port = r.Host, ""
		}
		other := "localhost"
		if host == "localhost" {
			other = "127.0.0.1"
		}
		if port != "" {
			other = net.JoinHostPort(other, port)
		}
		http.Redirect(w, r, "http://"+other+directPrefix+path, http.StatusTemporaryRedirect)
		return
	}
	a.mu.RLock()
	ad := a.adopted
	a.mu.RUnlock()
	if ad == nil {
		http.Error(w, "node is not adopted", http.StatusServiceUnavailable)
		return
	}
	// Large model files may come from another node that has them (§16).
	if digest := blobDigest(path); digest != "" && r.Method == http.MethodGet && a.servePeerBlob(w, r, ad, digest) {
		return
	}
	client := a.shimClient(ad)
	var lastErr error
	for _, ep := range a.endpointsOf(ad) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, ep+path, nil)
		if err != nil {
			continue
		}
		for _, h := range []string{"Range", "Accept", "If-Range", "If-Modified-Since"} {
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
