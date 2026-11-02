package core

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/linkinfo"
	"aicluster/common/proto"
	"aicluster/common/registry"
	"aicluster/controller/store"
)

// Peer copying and network checks (protocol §16).
//
// Nodes on 1 Gb copper share the Controller's single port when they download
// models. A node that already has a model file can serve it to the others
// over its own port, so the Controller hands out sources: each download of a
// large file goes to the least busy node that has it, up to PeerUploads at
// a time per node, and to the Controller otherwise. A lease keeps one
// download (all of a runtime's ranged requests for one file) on one source.

const (
	leaseTTL     = time.Minute      // renewed by every request of the download
	peerAvoidTTL = 10 * time.Minute // after a node reports a peer failed
)

type peerState struct {
	mu        sync.Mutex
	leases    map[string]*blobLease // requester|digest
	avoid     map[string]time.Time  // digest|node
	manifests map[string][]registry.Descriptor
}

type blobLease struct {
	source string // node ID; "" = the Controller
	until  time.Time
}

func (c *Core) handleBlobSource(w http.ResponseWriter, r *http.Request, _ *x509.Certificate) {
	digest := r.PathValue("digest")
	if !registry.ValidDigest(digest) {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "bad digest")
		return
	}
	src := c.blobSource(r.Context(), r.PathValue("id"), digest, r.URL.Query().Get("failed"))
	httpx.WriteJSON(w, http.StatusOK, src)
}

// blobSource picks where requester should download digest from.
func (c *Core) blobSource(ctx context.Context, requester, digest, failed string) proto.BlobSource {
	src := proto.BlobSource{V: proto.Version}
	if c.cfg.PeerUploads <= 0 {
		return src
	}
	now := time.Now()
	c.peers.mu.Lock()
	if failed != "" {
		c.peers.avoid[digest+"|"+failed] = now.Add(peerAvoidTTL)
	}
	key := requester + "|" + digest
	if l, ok := c.peers.leases[key]; ok && now.Before(l.until) && !c.avoidedLocked(digest, l.source, now) {
		l.until = now.Add(leaseTTL)
		source := l.source
		c.peers.mu.Unlock()
		if source == "" {
			return src
		}
		if n, err := c.Store.GetNode(ctx, source); err == nil {
			src.NodeID, src.Endpoint = n.NodeID, nodeBase(n)
		}
		return src
	}
	c.peers.mu.Unlock()

	holders, size := c.blobHolders(ctx, digest)
	if size < c.cfg.PeerMinBlob {
		return src // small files: not worth a second connection
	}
	c.peers.mu.Lock()
	defer c.peers.mu.Unlock()
	load := map[string]int{}
	for k, l := range c.peers.leases {
		if now.After(l.until) {
			delete(c.peers.leases, k)
			continue
		}
		load[l.source]++
	}
	var best *store.Node
	for _, n := range holders {
		if n.NodeID == requester || c.avoidedLocked(digest, n.NodeID, now) || load[n.NodeID] >= c.cfg.PeerUploads {
			continue
		}
		if best == nil || load[n.NodeID] < load[best.NodeID] {
			best = n
		}
	}
	l := &blobLease{until: now.Add(leaseTTL)}
	if best != nil {
		l.source = best.NodeID
		src.NodeID, src.Endpoint = best.NodeID, nodeBase(best)
		to := requester
		if n, err := c.Store.GetNode(ctx, requester); err == nil {
			to = n.Name
		}
		c.log.Info("model file from a peer", "to", to, "from", best.Name, "digest", digest[:19], "size", size)
	}
	c.peers.leases[key] = l
	return src
}

func (c *Core) avoidedLocked(digest, node string, now time.Time) bool {
	if node == "" {
		return false
	}
	until, ok := c.peers.avoid[digest+"|"+node]
	if ok && now.After(until) {
		delete(c.peers.avoid, digest+"|"+node)
		return false
	}
	return ok
}

// blobHolders lists online nodes that have digest on disk and serve model
// files, sorted by name, and the file's size.
func (c *Core) blobHolders(ctx context.Context, digest string) ([]*store.Node, int64) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil, 0
	}
	assigns, err := c.Store.ListAssignments(ctx, "")
	if err != nil {
		return nil, 0
	}
	refOf := map[string]string{}
	for _, a := range assigns {
		refOf[a.ID] = a.Ref
	}
	var size int64
	has := map[string]bool{} // ref → contains digest
	contains := func(ref string) bool {
		if v, ok := has[ref]; ok {
			return v
		}
		for _, b := range c.manifestBlobs(ref) {
			if b.Digest == digest {
				has[ref], size = true, b.Size
				return true
			}
		}
		has[ref] = false
		return false
	}
	fresh := c.cfg.HeartbeatInterval * proto.MissedHeartbeatsOffline
	var out []*store.Node
	for _, n := range nodes {
		if n.State != store.StateAdopted || time.Since(n.LastHeartbeat) > fresh {
			continue
		}
		var obs proto.ObservedState
		if json.Unmarshal(n.Observed, &obs) != nil || obs.Versions[proto.VersionPeerBlobs] != "yes" {
			continue
		}
		for _, m := range obs.Models {
			if (m.State == proto.ModelInstalled || m.State == proto.ModelLoaded) && contains(refOf[m.AssignmentID]) {
				out = append(out, n)
				break
			}
		}
	}
	if size == 0 {
		// No holder: the size still decides whether peers are worth asking.
		for _, a := range assigns {
			if contains(a.Ref) {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, size
}

// manifestBlobs caches the blob list of a cached model's manifest.
func (c *Core) manifestBlobs(ref string) []registry.Descriptor {
	if ref == "" {
		return nil
	}
	c.peers.mu.Lock()
	b, ok := c.peers.manifests[ref]
	c.peers.mu.Unlock()
	if ok {
		return b
	}
	m, _, err := c.Models.Manifest(ref)
	if err != nil {
		return nil
	}
	b = m.Blobs()
	c.peers.mu.Lock()
	c.peers.manifests[ref] = b
	c.peers.mu.Unlock()
	return b
}

// ---------- Network checks ----------

// NetTestBytes is how much each direction of a speed test moves: about a
// second each way on 1 Gb, ten on 100 Mb.
var NetTestBytes int64 = 128 << 20

var ErrNetTestBusy = errors.New("another network test is running; try again in a few seconds")

type netTests struct {
	run  sync.Mutex
	mu   sync.Mutex
	last map[string]*proto.NetTestResult
}

// NetTest measures throughput between the Controller and a node, both ways,
// over the node API's mTLS connection. One test runs at a time, so tests
// don't skew each other.
func (c *Core) NetTest(ctx context.Context, nodeID string) (*proto.NetTestResult, error) {
	n, err := c.Store.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if n.State != store.StateAdopted {
		return nil, ErrNotAdopted
	}
	if !c.nets.run.TryLock() {
		return nil, ErrNetTestBusy
	}
	defer c.nets.run.Unlock()
	client := c.NodeClient(nodeID, 2*time.Minute)
	base := nodeBase(n)
	res := &proto.NetTestResult{Bytes: NetTestBytes, At: time.Now().UTC()}
	res.ToNodeMbps, err = upload(ctx, client, base, NetTestBytes)
	if err == nil {
		res.FromNodeMbps, err = download(ctx, client, base, NetTestBytes)
	}
	if err != nil {
		res.Error = err.Error()
	}
	c.nets.mu.Lock()
	c.nets.last[nodeID] = res
	c.nets.mu.Unlock()
	c.log.Info("network test", "node", n.Name, "to_node_mbps", int(res.ToNodeMbps), "from_node_mbps", int(res.FromNodeMbps), "err", res.Error)
	return res, nil
}

// LastNetTest is the node's latest test result, if any.
func (c *Core) LastNetTest(nodeID string) *proto.NetTestResult {
	c.nets.mu.Lock()
	defer c.nets.mu.Unlock()
	return c.nets.last[nodeID]
}

func mbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds() / 1e6
}

// zeros is an endless source for the upload; TLS encrypts it, so the wire
// carries no easy-to-compress pattern.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func upload(ctx context.Context, client *http.Client, base string, n int64) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/nettest", io.LimitReader(zeros{}, n))
	if err != nil {
		return 0, err
	}
	req.ContentLength = n
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("to the node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("to the node: the agent returned %s (too old for network tests?)", resp.Status)
	}
	return mbps(n, time.Since(start)), nil
}

func download(ctx context.Context, client *http.Client, base string, n int64) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/nettest?bytes="+strconv.FormatInt(n, 10), nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("from the node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("from the node: the agent returned %s", resp.Status)
	}
	got, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, fmt.Errorf("from the node: %w", err)
	}
	return mbps(got, time.Since(start)), nil
}

// ControllerLink is the link of the Controller's interface towards its
// Slaves: the Slave network's, else the default route's.
func (c *Core) ControllerLink() *proto.Link {
	iface := ""
	if c.slaveNet != nil {
		iface = c.slaveNet.Iface
	}
	if iface == "" {
		iface = linkinfo.DefaultIface()
	}
	return linkinfo.Read(iface)
}
