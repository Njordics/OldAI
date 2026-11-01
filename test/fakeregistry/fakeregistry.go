// Package fakeregistry is a stand-in for registry.ollama.ai used by tests and
// the local dev cluster. Every name:tag exists; its single layer is
// deterministic pseudo-random bytes, so digests are stable across runs.
package fakeregistry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"aicluster/common/registry"
)

type Registry struct {
	LayerSize int64 // bytes per model layer
	// BytesPerSec throttles blob downloads (0 = unlimited) so progress is visible.
	BytesPerSec int64
	// Missing lists refs that 404, to test failures.
	Missing map[string]bool

	mu        sync.Mutex
	manifests map[string][]byte
	blobs     map[string][]byte
	Requests  map[string]int // path -> count, for "downloaded once" assertions
}

func New(layerSize int64) *Registry {
	return &Registry{LayerSize: layerSize, manifests: map[string][]byte{}, blobs: map[string][]byte{}, Requests: map[string]int{}, Missing: map[string]bool{}}
}

func (r *Registry) model(name, tag string) []byte {
	key := name + ":" + tag
	if m, ok := r.manifests[key]; ok {
		return m
	}
	seed := sha256.Sum256([]byte(key))
	rng := rand.NewChaCha8(seed)
	layer := make([]byte, r.LayerSize)
	_, _ = rng.Read(layer)
	config := []byte(fmt.Sprintf(`{"model_format":"gguf","model_family":"fake","name":%q}`, key))
	put := func(b []byte) registry.Descriptor {
		d := fmt.Sprintf("sha256:%x", sha256.Sum256(b))
		r.blobs[d] = b
		return registry.Descriptor{Digest: d, Size: int64(len(b))}
	}
	cfg := put(config)
	cfg.MediaType = "application/vnd.docker.container.image.v1+json"
	l := put(layer)
	l.MediaType = "application/vnd.ollama.image.model"
	m, _ := json.Marshal(registry.Manifest{SchemaVersion: 2, MediaType: registry.ManifestMediaType, Config: cfg, Layers: []registry.Descriptor{l}})
	r.manifests[key] = m
	return m
}

func (r *Registry) Count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Requests[path]
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p := strings.TrimPrefix(req.URL.Path, "/v2/")
	r.mu.Lock()
	r.Requests[req.URL.Path]++
	r.mu.Unlock()
	if i := strings.LastIndex(p, "/manifests/"); i >= 0 {
		name, tag := p[:i], p[i+len("/manifests/"):]
		if r.Missing[name+":"+tag] {
			http.NotFound(w, req)
			return
		}
		r.mu.Lock()
		m := r.model(name, tag)
		r.mu.Unlock()
		w.Header().Set("Content-Type", registry.ManifestMediaType)
		_, _ = w.Write(m)
		return
	}
	if i := strings.LastIndex(p, "/blobs/"); i >= 0 {
		r.mu.Lock()
		b, ok := r.blobs[p[i+len("/blobs/"):]]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.WriteHeader(http.StatusOK)
		r.write(w, b)
		return
	}
	http.NotFound(w, req)
}

func (r *Registry) write(w http.ResponseWriter, b []byte) {
	if r.BytesPerSec <= 0 {
		_, _ = w.Write(b)
		return
	}
	chunk := max(r.BytesPerSec/20, 1)
	for len(b) > 0 {
		n := min(int64(len(b)), chunk)
		if _, err := w.Write(b[:n]); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		b = b[n:]
		time.Sleep(50 * time.Millisecond)
	}
}
