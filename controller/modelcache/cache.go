// Package modelcache is the Controller's model store: it downloads each
// model from the internet once (docs/architecture.md §8) and serves it to
// Slaves over the LAN as a read-only Ollama/OCI registry.
//
// Layout under Dir:
//
//	blobs/sha256-<hex>             content-addressed blobs, shared by all models
//	manifests/<name>/<tag>         raw manifest; its presence means "ready"
//
// A model is only handed to Slaves after it is completely in the cache, so
// Slave pulls never wait on the internet.
package modelcache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aicluster/common/pki"
	"aicluster/common/registry"
)

const DefaultUpstream = "https://registry.ollama.ai"

// Entry states.
const (
	StateQueued      = "queued"
	StateDownloading = "downloading"
	StateReady       = "ready"
	StateFailed      = "failed"
)

type Entry struct {
	Ref        string    `json:"ref"`
	State      string    `json:"state"`
	BytesDone  int64     `json:"bytes_done"`
	BytesTotal int64     `json:"bytes_total"`
	Progress   float64   `json:"progress"`
	Digest     string    `json:"digest,omitempty"` // manifest digest
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type job struct {
	ref      registry.Ref
	state    atomic.Value // string
	done     atomic.Int64
	total    atomic.Int64
	err      atomic.Value // string
	started  time.Time
	finished chan struct{}
}

type Cache struct {
	Dir      string
	Upstream string
	Client   *http.Client
	Log      *slog.Logger
	// OnReady is called (in its own goroutine) when a model finishes.
	OnReady func(ref string)
	// Offline refuses to contact the upstream registry.
	Offline atomic.Bool

	mu        sync.Mutex
	jobs      map[string]*job
	failed    map[string]Entry
	blobLocks map[string]*sync.Mutex
}

func New(dir, upstream string, log *slog.Logger) (*Cache, error) {
	if upstream == "" {
		upstream = DefaultUpstream
	}
	for _, d := range []string{"blobs", "manifests", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Cache{
		Dir:       dir,
		Upstream:  strings.TrimRight(upstream, "/"),
		Client:    &http.Client{Timeout: 0}, // large downloads; per-request contexts bound them
		Log:       log,
		jobs:      map[string]*job{},
		failed:    map[string]Entry{},
		blobLocks: map[string]*sync.Mutex{},
	}, nil
}

func (c *Cache) manifestPath(r registry.Ref) string {
	return filepath.Join(c.Dir, "manifests", filepath.FromSlash(r.Name), r.Tag)
}

func (c *Cache) blobPath(digest string) string {
	return filepath.Join(c.Dir, "blobs", registry.BlobFile(digest))
}

// Ready reports whether ref is fully cached.
func (c *Cache) Ready(ref string) bool {
	r, err := registry.ParseRef(ref)
	if err != nil {
		return false
	}
	_, err = os.Stat(c.manifestPath(r))
	return err == nil
}

// Manifest returns the cached manifest for ref.
func (c *Cache) Manifest(ref string) (*registry.Manifest, []byte, error) {
	r, err := registry.ParseRef(ref)
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(c.manifestPath(r))
	if err != nil {
		return nil, nil, err
	}
	var m registry.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, err
	}
	return &m, raw, nil
}

// Status returns the state of ref: ready, in progress, failed, or empty
// State if the cache has never seen it.
func (c *Cache) Status(ref string) Entry {
	c.mu.Lock()
	j := c.jobs[ref]
	f, failed := c.failed[ref]
	c.mu.Unlock()
	if j != nil {
		e := Entry{Ref: ref, State: j.state.Load().(string), BytesDone: j.done.Load(), BytesTotal: j.total.Load(), UpdatedAt: time.Now().UTC()}
		if e.BytesTotal > 0 {
			e.Progress = float64(e.BytesDone) / float64(e.BytesTotal)
		}
		return e
	}
	if m, raw, err := c.Manifest(ref); err == nil {
		st, _ := os.Stat(c.manifestPath(mustRef(ref)))
		return Entry{Ref: ref, State: StateReady, BytesDone: m.TotalSize(), BytesTotal: m.TotalSize(), Progress: 1,
			Digest: digestOf(raw), UpdatedAt: st.ModTime().UTC()}
	}
	if failed {
		return f
	}
	return Entry{Ref: ref}
}

func mustRef(s string) registry.Ref { r, _ := registry.ParseRef(s); return r }

// List returns every cached or in-flight model.
func (c *Cache) List() []Entry {
	seen := map[string]bool{}
	var out []Entry
	root := filepath.Join(c.Dir, "manifests")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		i := strings.LastIndex(rel, "/")
		if i < 0 {
			return nil
		}
		ref := rel[:i] + ":" + rel[i+1:]
		seen[ref] = true
		out = append(out, c.Status(ref))
		return nil
	})
	c.mu.Lock()
	var extra []string
	for ref := range c.jobs {
		extra = append(extra, ref)
	}
	for ref := range c.failed {
		extra = append(extra, ref)
	}
	c.mu.Unlock()
	for _, ref := range extra {
		if !seen[ref] {
			seen[ref] = true
			out = append(out, c.Status(ref))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// Ensure starts downloading ref unless it is cached or already downloading.
func (c *Cache) Ensure(ref string) (Entry, error) {
	r, err := registry.ParseRef(ref)
	if err != nil {
		return Entry{}, err
	}
	ref = r.String()
	if c.Ready(ref) {
		return c.Status(ref), nil
	}
	c.mu.Lock()
	if _, running := c.jobs[ref]; !running {
		if c.Offline.Load() {
			c.mu.Unlock()
			return Entry{}, errors.New("offline mode: model is not in the cache")
		}
		j := &job{ref: r, started: time.Now(), finished: make(chan struct{})}
		j.state.Store(StateQueued)
		j.err.Store("")
		c.jobs[ref] = j
		delete(c.failed, ref)
		go c.run(j)
	}
	c.mu.Unlock()
	return c.Status(ref), nil
}

// Wait blocks until ref is no longer downloading (tests, CLI).
func (c *Cache) Wait(ctx context.Context, ref string) Entry {
	c.mu.Lock()
	j := c.jobs[ref]
	c.mu.Unlock()
	if j != nil {
		select {
		case <-j.finished:
		case <-ctx.Done():
		}
	}
	return c.Status(ref)
}

func (c *Cache) run(j *job) {
	ref := j.ref.String()
	log := c.Log.With("ref", ref)
	err := c.download(context.Background(), j)
	c.mu.Lock()
	delete(c.jobs, ref)
	if err != nil {
		c.failed[ref] = Entry{Ref: ref, State: StateFailed, BytesDone: j.done.Load(), BytesTotal: j.total.Load(),
			Error: err.Error(), UpdatedAt: time.Now().UTC()}
	}
	c.mu.Unlock()
	close(j.finished)
	if err != nil {
		log.Warn("model download failed", "err", err)
		return
	}
	log.Info("model cached", "bytes", j.total.Load(), "took", time.Since(j.started).Round(time.Second))
	if c.OnReady != nil {
		go c.OnReady(ref)
	}
}

func (c *Cache) download(ctx context.Context, j *job) error {
	j.state.Store(StateDownloading)
	raw, m, err := c.fetchManifest(ctx, j.ref)
	if err != nil {
		return err
	}
	j.total.Store(m.TotalSize())
	for _, b := range m.Blobs() {
		if !registry.ValidDigest(b.Digest) {
			return fmt.Errorf("manifest has invalid digest %q", b.Digest)
		}
		if err := c.fetchBlob(ctx, j, b); err != nil {
			return fmt.Errorf("blob %s: %w", b.Digest, err)
		}
	}
	// The manifest goes last: its presence is what makes the model "ready".
	return pki.WriteFileAtomic(c.manifestPath(j.ref), raw, 0o644)
}

func (c *Cache) fetchManifest(ctx context.Context, r registry.Ref) ([]byte, *registry.Manifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.Upstream, r.Name, r.Tag)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", registry.ManifestMediaType)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("model %s not found in registry", r)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("manifest: upstream returned %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	var m registry.Manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Config.Digest == "" {
		return nil, nil, fmt.Errorf("manifest: unexpected format")
	}
	return raw, &m, nil
}

func (c *Cache) blobLock(digest string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.blobLocks[digest]
	if !ok {
		l = &sync.Mutex{}
		c.blobLocks[digest] = l
	}
	return l
}

// fetchBlob downloads one blob unless present; models that share a layer
// (same base weights) download it once.
func (c *Cache) fetchBlob(ctx context.Context, j *job, b registry.Descriptor) error {
	l := c.blobLock(b.Digest)
	l.Lock()
	defer l.Unlock()
	dst := c.blobPath(b.Digest)
	if st, err := os.Stat(dst); err == nil && st.Size() == b.Size {
		j.done.Add(b.Size)
		return nil
	}
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", c.Upstream, j.ref.Name, b.Digest)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.Client.Do(req) // follows the registry's redirect to its CDN
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Join(c.Dir, "tmp"), registry.BlobFile(b.Digest)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	vw := registry.NewVerifyingWriter(tmp, b.Digest)
	vw.OnData = func(n int) { j.done.Add(int64(n)) }
	_, err = io.Copy(vw, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		j.done.Add(-vw.N)
		return err
	}
	if err := vw.Verify(); err != nil {
		j.done.Add(-vw.N)
		return err
	}
	if vw.N != b.Size {
		j.done.Add(-vw.N)
		return fmt.Errorf("size %d, manifest says %d", vw.N, b.Size)
	}
	return os.Rename(tmp.Name(), dst)
}

func digestOf(raw []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}
