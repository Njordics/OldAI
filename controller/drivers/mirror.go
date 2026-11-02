package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mirror serves an apt archive (Ubuntu's by default) to the nodes, which have
// no internet, from a disk cache it fills on demand. Packages and by-hash
// index files never change once published, so they are kept as they are.
// Other index files (InRelease, Packages) are fetched again when older than
// MaxAge, and the old copy is served when the upstream can't be reached.
//
// The Mirror doesn't need to be trusted: apt on the node checks the
// archive's signature on InRelease and every file's hash from there.
type Mirror struct {
	Upstream string // e.g. http://archive.ubuntu.com/ubuntu
	Dir      string
	Client   *http.Client
	MaxAge   time.Duration
	// Offline, when it returns true, serves only what is already cached.
	Offline func() bool
	Log     *slog.Logger

	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	done chan struct{}
	err  error
}

var (
	errNotFound = errors.New("not in the archive")
	errOffline  = errors.New("offline mode: not in the Controller's copy of the archive")
)

// ServeHTTP serves one archive path, e.g. "dists/noble/InRelease", relative
// to wherever the handler is mounted (use http.StripPrefix).
func (m *Mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	file, err := m.fetch(r.Context(), rel)
	switch {
	case errors.Is(err, errNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, errOffline):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, path.Base(rel), st.ModTime(), f)
}

// Open returns an archive file, fetching it first if needed.
func (m *Mirror) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	file, err := m.fetch(ctx, rel)
	if err != nil {
		return nil, err
	}
	return os.Open(file)
}

// validPath admits only clean paths inside the archive's two trees.
func validPath(rel string) bool {
	return rel != "" && path.Clean(rel) == rel && !strings.Contains(rel, "..") && !strings.ContainsAny(rel, "\\\x00") &&
		(strings.HasPrefix(rel, "dists/") || strings.HasPrefix(rel, "pool/"))
}

// immutable paths never change content once published.
func immutable(rel string) bool {
	return strings.HasPrefix(rel, "pool/") || strings.Contains(rel, "/by-hash/")
}

func (m *Mirror) fetch(ctx context.Context, rel string) (string, error) {
	if !validPath(rel) {
		return "", errNotFound
	}
	local := filepath.Join(m.Dir, filepath.FromSlash(rel))
	st, statErr := os.Stat(local)
	cached := statErr == nil && st.Mode().IsRegular()
	if cached && (immutable(rel) || time.Since(st.ModTime()) < m.MaxAge) {
		return local, nil
	}
	if m.Offline != nil && m.Offline() {
		if cached {
			return local, nil
		}
		return "", errOffline
	}

	m.mu.Lock()
	if m.calls == nil {
		m.calls = map[string]*call{}
	}
	c, running := m.calls[rel]
	if !running {
		c = &call{done: make(chan struct{})}
		m.calls[rel] = c
		// Not tied to the request: other nodes may want the same file.
		go func() {
			c.err = m.download(rel, local)
			m.mu.Lock()
			delete(m.calls, rel)
			m.mu.Unlock()
			close(c.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-c.done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if c.err != nil {
		if cached && !errors.Is(c.err, errNotFound) {
			m.log().Warn("archive upstream failed; serving the cached copy", "path", rel, "err", c.err)
			return local, nil
		}
		return "", c.err
	}
	return local, nil
}

func (m *Mirror) log() *slog.Logger {
	if m.Log == nil {
		return slog.Default()
	}
	return m.Log
}

func (m *Mirror) download(rel, local string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(m.Upstream, "/")+"/"+rel, nil)
	if err != nil {
		return err
	}
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("archive returned %s for %s", resp.Status, rel)
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(local), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fmt.Errorf("%s: got %d of %d bytes", rel, n, resp.ContentLength)
	}
	if strings.HasPrefix(rel, "pool/") {
		m.log().Info("cached archive package", "path", rel, "bytes", n)
	}
	return os.Rename(tmp.Name(), local)
}
