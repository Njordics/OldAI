package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/common/registry"
)

// Peer copying (protocol §16). On 1 Gb links the Controller's single port is
// the bottleneck when several nodes need the same model, so nodes copy model
// files from each other. Before each download the shim asks the Controller
// where to fetch the file from: a node that already has it, or the
// Controller. The runtime checks every file's SHA-256 as usual, so a peer
// can't hand over anything else. If the peer fails, the shim falls back to
// the Controller and tells it to avoid that peer.

// peerFailTTL is how long a failed peer is avoided for a file.
const peerFailTTL = 10 * time.Minute

type peerState struct {
	mu      sync.Mutex
	clients map[string]*http.Client // by node ID, for the current adoption
	forAd   *adopted
	failed  map[string]peerFail // by digest
	served  atomic.Int64
}

type peerFail struct {
	nodeID string
	until  time.Time
}

// PeerServed counts model files this node sent to other nodes (tests).
func (a *Agent) PeerServed() int64 { return a.peers.served.Load() }

// blobFile is where the runtime keeps a model file; "" when unknown.
func (a *Agent) blobFile(digest string) string {
	if !registry.ValidDigest(digest) {
		return ""
	}
	if rt, ok := a.rt.(interface{ BlobDir() string }); ok {
		return filepath.Join(rt.BlobDir(), registry.BlobFile(digest))
	}
	if a.cfg.Runtime == "ollama" {
		return filepath.Join(a.cfg.OllamaModels, "blobs", registry.BlobFile(digest))
	}
	return ""
}

// nodeOnly admits other nodes of this cluster (not the Controller).
func (a *Agent) nodeOnly(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		ad := a.adopted
		a.mu.RUnlock()
		if ad == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, "node certificate required")
			return
		}
		raw := make([][]byte, len(r.TLS.PeerCertificates))
		for i, c := range r.TLS.PeerCertificates {
			raw[i] = c.Raw
		}
		leaf, err := pki.VerifyPeer(raw, ad.caPool, "", x509.ExtKeyUsageClientAuth)
		if err == nil && pki.NodeIDFromCert(leaf) == "" {
			err = errors.New("not a node of this cluster")
		}
		if err != nil {
			httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, err.Error())
			return
		}
		h(w, r)
	})
}

// handlePeerBlob serves a complete model file to another node, with Range.
func (a *Agent) handlePeerBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	file := a.blobFile(digest)
	if file == "" {
		http.Error(w, "unknown model file", http.StatusNotFound)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "model file not on this node", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Range") == "" || strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
		a.peers.served.Add(1)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// blobDigest returns the digest of a registry blob path, or "".
func blobDigest(path string) string {
	i := strings.LastIndex(path, "/blobs/")
	if i < 0 {
		return ""
	}
	return path[i+len("/blobs/"):]
}

// blobSource asks the Controller where to download digest from.
func (a *Agent) blobSource(ctx context.Context, ad *adopted, digest string) (proto.BlobSource, error) {
	q := url.Values{}
	a.peers.mu.Lock()
	if f, ok := a.peers.failed[digest]; ok && time.Now().Before(f.until) {
		q.Set("failed", f.nodeID)
	}
	a.peers.mu.Unlock()
	path := "/v1/nodes/" + a.NodeID() + "/blob-source/" + url.PathEscape(digest)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var src proto.BlobSource
	_, err := a.tryEndpoints(ad, func(base string) error {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return httpx.Do(cctx, controllerClient(ad), http.MethodGet, base+path, nil, &src)
	})
	return src, err
}

func (a *Agent) peerFailed(digest, nodeID string, err error) {
	a.log.Warn("model file: peer failed; using the Controller", "peer", nodeID, "digest", digest, "err", err)
	a.peers.mu.Lock()
	if a.peers.failed == nil {
		a.peers.failed = map[string]peerFail{}
	}
	a.peers.failed[digest] = peerFail{nodeID: nodeID, until: time.Now().Add(peerFailTTL)}
	a.peers.mu.Unlock()
}

// peerClient streams from another node: its certificate must be that
// node's, from our CA.
func (a *Agent) peerClient(ad *adopted, nodeID string) (*http.Client, error) {
	a.peers.mu.Lock()
	defer a.peers.mu.Unlock()
	if a.peers.forAd != ad {
		a.peers.clients, a.peers.forAd = map[string]*http.Client{}, ad
	}
	if c, ok := a.peers.clients[nodeID]; ok {
		return c, nil
	}
	cfg, err := a.peerTLS(nodeID)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig:       cfg,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}}
	a.peers.clients[nodeID] = c
	return c, nil
}

// servePeerBlob answers a runtime's blob request from another node when
// the Controller says one has the file. It returns false, having written
// nothing, when the Controller should serve it instead.
func (a *Agent) servePeerBlob(w http.ResponseWriter, r *http.Request, ad *adopted, digest string) bool {
	src, err := a.blobSource(r.Context(), ad, digest)
	if err != nil || src.NodeID == "" || src.Endpoint == "" {
		return false
	}
	client, err := a.peerClient(ad, src.NodeID)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimSuffix(src.Endpoint, "/")+"/v2/blobs/"+url.PathEscape(digest), nil)
	if err != nil {
		return false
	}
	for _, h := range []string{"Range", "If-Range"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		a.peerFailed(digest, src.NodeID, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		a.peerFailed(digest, src.NodeID, fmt.Errorf("peer returned %s", resp.Status))
		return false
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Docker-Content-Digest", "Last-Modified", "Etag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}
