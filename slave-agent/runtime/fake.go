package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aicluster/common/pki"
	"aicluster/common/registry"
)

// Fake stands in for Ollama on machines without it (the Windows dev
// cluster, tests). It pulls for real through the registry shim, verifying
// every digest, but loading is just bookkeeping.
type Fake struct {
	Dir         string
	BytesPerSec int64 // slow pulls down so progress is visible (0 = unlimited)

	mu     sync.Mutex
	loaded map[string]bool
}

func NewFake(dir string) *Fake {
	_ = os.MkdirAll(filepath.Join(dir, "blobs"), 0o755)
	return &Fake{Dir: dir, loaded: map[string]bool{}}
}

func (f *Fake) Kind() string                    { return "fake" }
func (f *Fake) Check(ctx context.Context) error { return nil }

func (f *Fake) index() (map[string]string, error) { // local name -> manifest JSON
	idx := map[string]string{}
	b, err := os.ReadFile(filepath.Join(f.Dir, "installed.json"))
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (f *Fake) saveIndex(idx map[string]string) error {
	b, _ := json.MarshalIndent(idx, "", "  ")
	return pki.WriteFileAtomic(filepath.Join(f.Dir, "installed.json"), b, 0o644)
}

func (f *Fake) Installed(ctx context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx, err := f.index()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for k := range idx {
		out[k] = true
	}
	return out, nil
}

func (f *Fake) Pull(ctx context.Context, m Model, p Progress) error {
	base := "http://" + m.RegistryHost + "/v2/" + m.Ref.Name
	raw, err := get(ctx, base+"/manifests/"+m.Ref.Tag, nil)
	if err != nil {
		return err
	}
	var man registry.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	total := man.TotalSize()
	var done int64
	for _, b := range man.Blobs() {
		if !registry.ValidDigest(b.Digest) {
			return fmt.Errorf("bad digest %q", b.Digest)
		}
		dst := filepath.Join(f.Dir, "blobs", registry.BlobFile(b.Digest))
		if st, err := os.Stat(dst); err == nil && st.Size() == b.Size {
			done += b.Size
			continue
		}
		tmp, err := os.CreateTemp(filepath.Join(f.Dir, "blobs"), "pull-*")
		if err != nil {
			return err
		}
		vw := registry.NewVerifyingWriter(tmp, b.Digest)
		vw.OnData = func(n int) {
			done += int64(n)
			if p != nil {
				p(done, total)
			}
			if f.BytesPerSec > 0 {
				time.Sleep(time.Duration(float64(n) / float64(f.BytesPerSec) * float64(time.Second)))
			}
		}
		_, err = get(ctx, base+"/blobs/"+b.Digest, vw)
		tmp.Close()
		if err == nil {
			err = vw.Verify()
		}
		if err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return err
		}
	}
	if p != nil {
		p(total, total)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	idx, err := f.index()
	if err != nil {
		return err
	}
	idx[m.LocalName] = string(raw)
	return f.saveIndex(idx)
}

func get(ctx context.Context, url string, w io.Writer) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if w != nil {
		_, err = io.Copy(w, resp.Body)
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (f *Fake) Load(ctx context.Context, m Model) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded[m.LocalName] = true
	return nil
}

func (f *Fake) Unload(ctx context.Context, m Model) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.loaded, m.LocalName)
	return nil
}

// Loaded is for tests.
func (f *Fake) Loaded(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded[name]
}

func (f *Fake) Remove(ctx context.Context, m Model) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx, err := f.index()
	if err != nil {
		return err
	}
	delete(idx, m.LocalName)
	delete(f.loaded, m.LocalName)
	return f.saveIndex(idx)
	// Blobs are left for a later sweep; they may be shared with other models.
}
