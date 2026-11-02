package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/common/registry"
	"aicluster/slave-agent/runtime"
)

// Pooled models (one copy split across nodes). Workers run an rpc-server on
// loopback and expose it only through /v1/rpc-tunnel/<group>, which accepts
// mTLS connections from that pool's leader node and nobody else. The leader
// opens a local listener per worker that forwards over such tunnels, then
// starts llama-server against those local addresses.

const tunnelProto = "aicluster-rpc"

type poolEntry struct {
	role   string
	key    string // what the entry was started with; a change restarts it
	proc   runtime.Proc
	tuns   []*tunnel
	epochs map[string]string // worker node -> rpc-server epoch seen at start
	model  string
	group  string
	leader string // node id (workers only accept this peer)
	epoch  string // workers: changes every time rpc-server restarts
}

func (e *poolEntry) stop() {
	for _, t := range e.tuns {
		t.Close()
	}
	if e.proc != nil {
		e.proc.Stop()
	}
}

type poolState struct {
	mu      sync.Mutex
	entries map[string]*poolEntry // by assignment ID
	failed  map[string]string     // group -> why this node's worker isn't running
}

// workerFailed records why this node can't serve as a worker in a pool, so
// the leader's probe can say so instead of only "not a worker".
func (a *Agent) workerFailed(group, why string) {
	a.pools.mu.Lock()
	defer a.pools.mu.Unlock()
	if why == "" {
		delete(a.pools.failed, group)
		return
	}
	if a.pools.failed == nil {
		a.pools.failed = map[string]string{}
	}
	a.pools.failed[group] = why
}

func (a *Agent) poolEntry(id string) *poolEntry {
	a.pools.mu.Lock()
	defer a.pools.mu.Unlock()
	return a.pools.entries[id]
}

// poolURL is where the agent proxies inference for a pooled model it leads.
func (a *Agent) poolURL(assignmentID string) string {
	e := a.poolEntry(assignmentID)
	if e == nil || e.role != proto.RoleLeaderName || e.proc == nil {
		return ""
	}
	return "http://" + e.proc.Addr()
}

func poolKey(dm proto.DesiredModel) string {
	b, _ := json.Marshal([]any{dm.Role, dm.Source, dm.Pool})
	return string(b)
}

// reconcilePools starts, checks and stops pool processes for the desired
// pooled models (dm.Role set).
func (a *Agent) reconcilePools(ctx context.Context, desired []proto.DesiredModel) {
	want := map[string]proto.DesiredModel{}
	for _, dm := range desired {
		if dm.Role != "" && dm.Pool != nil {
			want[dm.AssignmentID] = dm
		}
	}
	a.pools.mu.Lock()
	removedLeader := false
	for id, e := range a.pools.entries {
		dm, ok := want[id]
		if !ok || poolKey(dm) != e.key {
			delete(a.pools.entries, id)
			go e.stop()
			removedLeader = removedLeader || (!ok && e.role == proto.RoleLeaderName)
			a.log.Info("pool member stopped", "model", e.model, "role", e.role)
		}
	}
	a.pools.mu.Unlock()
	if removedLeader {
		a.removePoolFile() // takes pools.mu itself, so only after unlocking
	}
	if a.poolRT == nil {
		for _, dm := range want {
			a.workerFailed(dm.Pool.GroupID, "no pool runtime configured")
			a.setObs(dm, "", func(o *proto.ObservedModel) {
				o.Role, o.State, o.Error = dm.Role, proto.ModelFailed, "no pool runtime configured"
			})
		}
		return
	}
	if err := a.poolRT.Check(); err != nil {
		for _, dm := range want {
			a.workerFailed(dm.Pool.GroupID, err.Error())
			a.setObs(dm, "", func(o *proto.ObservedModel) { o.Role, o.State, o.Error = dm.Role, proto.ModelFailed, err.Error() })
		}
		return
	}
	for _, dm := range want {
		if dm.Role == proto.RoleWorkerName {
			a.reconcileWorker(ctx, dm)
		} else {
			a.reconcileLeader(ctx, dm)
		}
	}
}

func (a *Agent) reconcileWorker(ctx context.Context, dm proto.DesiredModel) {
	e := a.poolEntry(dm.AssignmentID)
	if e != nil {
		select {
		case <-e.proc.Done():
			a.log.Warn("rpc-server exited; restarting", "model", dm.ModelID)
			a.pools.mu.Lock()
			delete(a.pools.entries, dm.AssignmentID)
			a.pools.mu.Unlock()
		default:
			return
		}
	}
	proc, err := a.poolRT.StartWorker(ctx)
	if err != nil {
		a.workerFailed(dm.Pool.GroupID, "rpc-server: "+err.Error())
		a.setObs(dm, "", func(o *proto.ObservedModel) {
			o.Role, o.State, o.Error = dm.Role, proto.ModelFailed, "rpc-server: "+err.Error()
		})
		return
	}
	a.pools.mu.Lock()
	a.pools.entries[dm.AssignmentID] = &poolEntry{
		role: proto.RoleWorkerName, key: poolKey(dm), proc: proc, model: dm.ModelID, group: dm.Pool.GroupID,
		leader: dm.Pool.Leader, epoch: strconv.FormatInt(time.Now().UnixNano(), 36),
	}
	a.pools.mu.Unlock()
	a.workerFailed(dm.Pool.GroupID, "")
	a.log.Info("pool worker ready", "model", dm.ModelID, "leader", dm.Pool.Leader)
	a.setObs(dm, "", func(o *proto.ObservedModel) { o.Role, o.State, o.Error, o.Progress = dm.Role, proto.ModelLoaded, "", 1 })
}

func (a *Agent) reconcileLeader(ctx context.Context, dm proto.DesiredModel) {
	if e := a.poolEntry(dm.AssignmentID); e != nil {
		healthy := true
		select {
		case <-e.proc.Done():
			healthy = false
		default:
		}
		// A worker whose rpc-server restarted lost the layers it held, so
		// the leader must reload; an unreachable worker makes us degraded.
		for _, m := range dm.Pool.Members {
			if m.Leader {
				continue
			}
			ep, err := a.probeWorker(ctx, m, dm.Pool.GroupID)
			if err != nil {
				a.setObs(dm, e.model, func(o *proto.ObservedModel) {
					o.Role, o.State, o.Error = dm.Role, proto.ModelFailed, "worker "+shortID(m.NodeID)+" unreachable: "+err.Error()
				})
				return
			}
			if ep != e.epochs[m.NodeID] {
				healthy = false
			}
		}
		if healthy {
			a.setObs(dm, e.model, func(o *proto.ObservedModel) { o.Role, o.State, o.Error, o.Progress = dm.Role, proto.ModelLoaded, "", 1 })
			return
		}
		a.log.Warn("pool needs a reload", "model", dm.ModelID)
		a.pools.mu.Lock()
		delete(a.pools.entries, dm.AssignmentID)
		a.pools.mu.Unlock()
		e.stop()
	}

	// 1. The model file, from the Controller's cache.
	ref, err := registry.ParseRef(dm.Source.Ref)
	if err != nil {
		a.setObs(dm, "", func(o *proto.ObservedModel) { o.Role, o.State, o.Error = dm.Role, proto.ModelFailed, err.Error() })
		return
	}
	a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) {
		o.Role, o.State, o.Error, o.BytesTotal = dm.Role, proto.ModelDownloading, "", dm.Size
	})
	path, err := a.fetchGGUF(ctx, ref, func(done, total int64) {
		a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) {
			o.BytesDone, o.BytesTotal = done, total
			if total > 0 {
				o.Progress = float64(done) / float64(total)
			}
		})
	})
	if err != nil {
		a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) { o.State, o.Error = proto.ModelFailed, "download: "+err.Error() })
		return
	}

	// 2. Tunnels to the workers, then 3. llama-server.
	a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) { o.State, o.Error, o.Progress = proto.ModelLoading, "", 1 })
	entry := &poolEntry{role: proto.RoleLeaderName, key: poolKey(dm), model: ref.LocalName(), group: dm.Pool.GroupID, epochs: map[string]string{}}
	spec := runtime.LeaderSpec{ID: dm.AssignmentID, ModelPath: path, ModelName: dm.ModelID, Layers: dm.Pool.Layers, Parallel: dm.Pool.Parallel}
	for _, m := range dm.Pool.Members {
		if m.Leader {
			spec.LocalShare = m.Share
			continue
		}
		ep, err := a.probeWorker(ctx, m, dm.Pool.GroupID)
		if err == nil {
			var t *tunnel
			t, err = a.openTunnel(m, dm.Pool.GroupID)
			if err == nil {
				entry.tuns = append(entry.tuns, t)
				entry.epochs[m.NodeID] = ep
				spec.Workers = append(spec.Workers, runtime.PoolWorkerRef{Addr: t.Addr(), Share: m.Share, Label: shortID(m.NodeID)})
			}
		}
		if err != nil {
			entry.stop()
			a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) {
				o.State, o.Error = proto.ModelFailed, "worker "+shortID(m.NodeID)+" not ready: "+err.Error()
			})
			return
		}
	}
	proc, err := a.poolRT.StartLeader(ctx, spec)
	if err != nil {
		entry.stop()
		a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) { o.State, o.Error = proto.ModelFailed, err.Error() })
		return
	}
	entry.proc = proc
	a.pools.mu.Lock()
	a.pools.entries[dm.AssignmentID] = entry
	a.pools.mu.Unlock()
	a.log.Info("pooled model loaded", "model", dm.ModelID, "workers", len(spec.Workers), "local_share", spec.LocalShare)
	a.setObs(dm, ref.LocalName(), func(o *proto.ObservedModel) { o.State, o.Error, o.Progress = proto.ModelLoaded, "", 1 })
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---------- Model file ----------

func (a *Agent) ggufDir() string { return filepath.Join(a.cfg.ModelDir, "gguf") }

// fetchGGUF downloads the model layer (a GGUF file) of an Ollama-format
// model from the Controller, through the registry shim.
func (a *Agent) fetchGGUF(ctx context.Context, ref registry.Ref, progress runtime.Progress) (string, error) {
	base := "http://" + a.ShimHost() + "/v2/" + ref.Name
	raw, err := httpGet(ctx, base+"/manifests/"+ref.Tag, nil)
	if err != nil {
		return "", err
	}
	var m registry.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	// The weights are the "model" layer; failing that, the largest one.
	var layer *registry.Descriptor
	for i := range m.Layers {
		if m.Layers[i].MediaType == "application/vnd.ollama.image.model" {
			layer = &m.Layers[i]
			break
		}
	}
	for i := range m.Layers {
		if layer == nil || (layer.MediaType != "application/vnd.ollama.image.model" && m.Layers[i].Size > layer.Size) {
			layer = &m.Layers[i]
		}
	}
	if layer == nil || !registry.ValidDigest(layer.Digest) {
		return "", errors.New("manifest has no model layer")
	}
	_ = os.MkdirAll(a.ggufDir(), 0o755)
	dst := filepath.Join(a.ggufDir(), registry.BlobFile(layer.Digest)+".gguf")
	if st, err := os.Stat(dst); err == nil && st.Size() == layer.Size {
		return dst, nil
	}
	tmp, err := os.CreateTemp(a.ggufDir(), "pull-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	vw := registry.NewVerifyingWriter(tmp, layer.Digest)
	vw.OnData = func(int) {
		if progress != nil {
			progress(vw.N, layer.Size)
		}
	}
	_, err = httpGet(ctx, base+"/blobs/"+layer.Digest, vw)
	tmp.Close()
	if err == nil {
		err = vw.Verify()
	}
	if err != nil {
		return "", err
	}
	return dst, os.Rename(tmp.Name(), dst)
}

func (a *Agent) removePoolFile() {
	// The GGUF file is only kept while this node leads the pool.
	matches, _ := filepath.Glob(filepath.Join(a.ggufDir(), "*.gguf"))
	if len(matches) == 0 {
		return
	}
	a.pools.mu.Lock()
	leading := 0
	for _, x := range a.pools.entries {
		if x.role == proto.RoleLeaderName {
			leading++
		}
	}
	a.pools.mu.Unlock()
	if leading == 0 {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

func httpGet(ctx context.Context, u string, w io.Writer) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if w != nil {
		_, err = io.Copy(w, resp.Body)
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------- Tunnels: leader side ----------

// peerTLS authenticates a worker node: chain to our CA and its node URI.
func (a *Agent) peerTLS(nodeID string) (*tls.Config, error) {
	a.mu.RLock()
	ad := a.adopted
	a.mu.RUnlock()
	if ad == nil {
		return nil, errors.New("not adopted")
	}
	pool, want := ad.caPool, pki.NodeURI(nodeID)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{ad.cert},
		InsecureSkipVerify: true, // replaced by CA pin + node URI below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := pki.VerifyPeer(raw, pool, want, x509.ExtKeyUsageServerAuth)
			return err
		},
	}, nil
}

func hostOf(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad endpoint %q", endpoint)
	}
	return u.Host, nil
}

// dialWorker opens one tunnelled connection to a worker's rpc-server.
func (a *Agent) dialWorker(m proto.PoolMember, group string) (net.Conn, error) {
	cfg, err := a.peerTLS(m.NodeID)
	if err != nil {
		return nil, err
	}
	host, err := hostOf(m.Endpoint)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", host, cfg)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(conn, "GET /v1/rpc-tunnel/%s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", group, host, tunnelProto)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		conn.Close()
		return nil, fmt.Errorf("worker refused tunnel: %s %s", resp.Status, string(body))
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// probeWorker asks a worker's tunnel endpoint for its rpc-server epoch.
func (a *Agent) probeWorker(ctx context.Context, m proto.PoolMember, group string) (string, error) {
	cfg, err := a.peerTLS(m.NodeID)
	if err != nil {
		return "", err
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.Endpoint+"/v1/rpc-tunnel/"+group, nil)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var st struct {
		Epoch string `json:"epoch"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&st)
	if resp.StatusCode != http.StatusOK {
		if st.Error == "" {
			st.Error = resp.Status
		}
		return "", errors.New(st.Error)
	}
	return st.Epoch, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite passes a client's close on through the TLS tunnel; without it
// the worker's rpc-server (one client at a time) waits on that connection
// forever and every later one hangs.
func (c *bufConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

type tunnel struct {
	ln net.Listener
}

func (t *tunnel) Addr() string { return t.ln.Addr().String() }
func (t *tunnel) Close()       { _ = t.ln.Close() }

// openTunnel listens on loopback and forwards each connection to the worker.
func (a *Agent) openTunnel(m proto.PoolMember, group string) (*tunnel, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				remote, err := a.dialWorker(m, group)
				if err != nil {
					a.log.Warn("tunnel to worker failed", "worker", shortID(m.NodeID), "err", err)
					c.Close()
					return
				}
				splice(c, remote)
			}()
		}
	}()
	return &tunnel{ln: ln}, nil
}

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	// RPC is request/response: once one side is finished, the other has
	// little left to say. Don't let a peer that never closes pin the
	// connection (and with it the single-client rpc-server).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	a.Close()
	b.Close()
}

// ---------- Tunnels: worker side ----------

// handleRPCTunnel serves /v1/rpc-tunnel/<group>: a probe (GET without
// Upgrade) returns the rpc-server epoch; with Upgrade it becomes a raw byte
// stream to the local rpc-server. Only the pool's leader node may connect.
func (a *Agent) handleRPCTunnel(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	var entry *poolEntry
	a.pools.mu.Lock()
	for _, e := range a.pools.entries {
		if e.role == proto.RoleWorkerName && e.group == group {
			entry = e
		}
	}
	why := a.pools.failed[group]
	a.pools.mu.Unlock()
	fail := func(status int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	if entry == nil {
		if why != "" {
			fail(http.StatusServiceUnavailable, why)
			return
		}
		fail(http.StatusNotFound, "this node is not a worker in that pool")
		return
	}
	a.mu.RLock()
	ad := a.adopted
	a.mu.RUnlock()
	if ad == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		fail(http.StatusForbidden, "node certificate required")
		return
	}
	raw := make([][]byte, len(r.TLS.PeerCertificates))
	for i, c := range r.TLS.PeerCertificates {
		raw[i] = c.Raw
	}
	if _, err := pki.VerifyPeer(raw, ad.caPool, pki.NodeURI(entry.leader), x509.ExtKeyUsageClientAuth); err != nil {
		a.log.Warn("rpc tunnel refused", "remote", r.RemoteAddr, "err", err)
		fail(http.StatusForbidden, "only the pool's leader may connect")
		return
	}
	if r.Header.Get("Upgrade") != tunnelProto {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"epoch": entry.epoch})
		return
	}
	local, err := net.DialTimeout("tcp", entry.proc.Addr(), 5*time.Second)
	if err != nil {
		fail(http.StatusServiceUnavailable, "rpc-server not running")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		local.Close()
		fail(http.StatusInternalServerError, "connection can't be upgraded")
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		local.Close()
		return
	}
	_, _ = fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", tunnelProto)
	_ = brw.Flush()
	if n := brw.Reader.Buffered(); n > 0 {
		pending, _ := brw.Reader.Peek(n)
		_, _ = local.Write(pending)
	}
	splice(conn, local)
}

// stopAllPools is used on release.
func (a *Agent) stopAllPools() {
	a.pools.mu.Lock()
	entries := a.pools.entries
	a.pools.entries = map[string]*poolEntry{}
	a.pools.mu.Unlock()
	for _, e := range entries {
		e.stop()
	}
}

// PoolRoles reports this node's pool memberships (assignment ID -> role),
// for tests and diagnostics.
func (a *Agent) PoolRoles() map[string]string {
	a.pools.mu.Lock()
	defer a.pools.mu.Unlock()
	out := map[string]string{}
	for id, e := range a.pools.entries {
		out[id] = e.role
	}
	return out
}
