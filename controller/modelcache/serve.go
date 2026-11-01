package modelcache

import (
	"net/http"
	"os"
	"strings"

	"aicluster/common/registry"
)

// ServeHTTP implements the read-only registry API Slaves pull from:
//
//	GET /v2/                              ping
//	GET /v2/<name>/manifests/<tag>        cached manifest
//	GET /v2/<name>/blobs/<digest>         blob, with Range support
//
// Anything not fully cached is 404: Slaves are only told about models after
// the Controller has them.
func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	if p == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if name, tag, ok := cutLast(p, "/manifests/"); ok {
		ref, err := registry.ParseRef(name + ":" + tag)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, raw, err := c.Manifest(ref.String())
		if err != nil {
			http.Error(w, "manifest not cached", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", registry.ManifestMediaType)
		w.Header().Set("Docker-Content-Digest", digestOf(raw))
		_, _ = w.Write(raw)
		return
	}
	if _, digest, ok := cutLast(p, "/blobs/"); ok {
		if !registry.ValidDigest(digest) {
			http.Error(w, "bad digest", http.StatusBadRequest)
			return
		}
		f, err := os.Open(c.blobPath(digest))
		if err != nil {
			http.Error(w, "blob not cached", http.StatusNotFound)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest)
		http.ServeContent(w, r, "", st.ModTime(), f)
		return
	}
	http.NotFound(w, r)
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}
