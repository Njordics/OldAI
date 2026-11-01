package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/gateway"
	"aicluster/controller/store"
)

func adoptAll(t *testing.T, c *ctrl, ss ...*slave) {
	t.Helper()
	for _, s := range ss {
		c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	}
}

func installPool(t *testing.T, c *ctrl, model string, ss ...*slave) (leader, worker *store.Assignment) {
	t.Helper()
	var ids []string
	for _, s := range ss {
		ids = append(ids, s.NodeID())
	}
	rows, err := c.Install(context.Background(), core.InstallRequest{ModelID: model, Nodes: ids, Pool: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(ss) {
		t.Fatalf("pool rows = %d, want %d", len(rows), len(ss))
	}
	for _, r := range rows {
		if r.GroupID == "" || r.GroupID != rows[0].GroupID {
			t.Fatalf("rows not grouped: %+v", rows)
		}
		if r.Role == store.RoleLeader {
			leader = r
		} else {
			worker = r
		}
	}
	if leader == nil || worker == nil {
		t.Fatalf("roles: %+v", rows)
	}
	for _, r := range rows {
		c.waitModel(t, r.ID, proto.ModelLoaded)
	}
	return leader, worker
}

func slaveByNode(id string, ss ...*slave) *slave {
	for _, s := range ss {
		if s.NodeID() == id {
			return s
		}
	}
	return nil
}

func TestPooledModel(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s1, s2, s3 := startSlave(t, c), startSlave(t, c), startSlave(t, c)
	adoptAll(t, c, s1, s2, s3)
	ctx := context.Background()

	leader, worker := installPool(t, c, "qwen2.5:0.5b", s1, s2)
	ls, ws := slaveByNode(leader.NodeID, s1, s2), slaveByNode(worker.NodeID, s1, s2)
	if ls.PoolRoles()[leader.ID] != "leader" || ws.PoolRoles()[worker.ID] != "worker" {
		t.Fatalf("roles on agents: %v %v", ls.PoolRoles(), ws.PoolRoles())
	}
	if leader.Share+worker.Share < 0.99 || leader.Share <= 0 || worker.Share <= 0 {
		t.Fatalf("shares: %v + %v", leader.Share, worker.Share)
	}

	// Gateway: the pooled copy is one model, and answering it crosses the
	// mTLS tunnel to the worker (the fake leader pings each worker).
	g := startGateway(t, c, gateway.Config{})
	_, key, _ := c.Store.CreateAPIKey(ctx, "t", nil, 0)
	_, b := g.do(key, "GET", "/v1/models", "")
	if !strings.Contains(b, `"id":"qwen2.5:0.5b"`) || !strings.Contains(b, `"pooled":1`) {
		t.Fatalf("models: %s", b)
	}
	r, b := g.do(key, "POST", "/v1/chat/completions", chatBody)
	if r.StatusCode != 200 || !strings.Contains(b, "Pooled stand-in reply") || !strings.Contains(b, ws.cfg.FakeLabel) || !strings.Contains(b, ls.cfg.FakeLabel) {
		t.Fatalf("pooled chat: %d %s", r.StatusCode, b)
	}
	if r.Header.Get("X-AICluster-Node") == "" {
		t.Fatal("no serving node header")
	}

	// Only the pool's leader may open the worker's tunnel: not another
	// node of the same cluster, and not the Controller.
	group := leader.GroupID
	wn := c.node(t, worker.NodeID)
	tunnelURL := fmt.Sprintf("https://%s:%d/v1/rpc-tunnel/%s", wn.Addr, wn.Port, group)
	for name, client := range map[string]*http.Client{
		"other node": nodeClient(t, s3, worker.NodeID),
		"controller": c.NodeClient(worker.NodeID, 5*time.Second),
	} {
		resp, err := client.Get(tunnelURL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s opened the tunnel: %d %s", name, resp.StatusCode, body)
		}
	}
	if resp, err := nodeClient(t, ls, worker.NodeID).Get(tunnelURL); err != nil || resp.StatusCode != 200 {
		t.Fatalf("leader probe: %v %v", err, resp)
	}

	// Pooled copies can't be swapped.
	if _, err := c.Swap(ctx, leader.ID, "all-minilm:22m", ""); !errors.Is(err, core.ErrPooledSwap) {
		t.Fatalf("swap: %v", err)
	}

	// Worker down: the pool stops being offered; back up: it reloads.
	ws.stop()
	c.waitModel(t, leader.ID, proto.ModelFailed)
	waitFor(t, 10*time.Second, "pool hidden from /v1/models", func() bool {
		_, b := g.do(key, "GET", "/v1/models", "")
		return !strings.Contains(b, "qwen2.5:0.5b")
	})
	ws.start(t)
	c.waitModel(t, worker.ID, proto.ModelLoaded)
	c.waitModel(t, leader.ID, proto.ModelLoaded)
	waitFor(t, 10*time.Second, "pool answering again", func() bool {
		r, b := g.do(key, "POST", "/v1/chat/completions", chatBody)
		return r.StatusCode == 200 && strings.Contains(b, "Pooled stand-in reply")
	})

	// Removing any member removes the whole pool from every node.
	if err := c.RemoveAssignment(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "pool processes stopped", func() bool {
		return len(ls.PoolRoles()) == 0 && len(ws.PoolRoles()) == 0
	})
	if ms, _ := c.ModelStatuses(ctx); len(ms) != 0 {
		t.Fatalf("assignments left: %+v", ms)
	}
}

// Releasing a member node tears the whole pool down.
func TestPoolReleasedWithMember(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s1, s2 := startSlave(t, c), startSlave(t, c)
	adoptAll(t, c, s1, s2)
	leader, worker := installPool(t, c, "qwen2.5:0.5b", s1, s2)
	if err := c.Release(context.Background(), worker.NodeID, false); err != nil {
		t.Fatal(err)
	}
	ls := slaveByNode(leader.NodeID, s1, s2)
	waitFor(t, 10*time.Second, "leader stopped its pool", func() bool { return len(ls.PoolRoles()) == 0 })
	if ms, _ := c.ModelStatuses(context.Background()); len(ms) != 0 {
		t.Fatalf("assignments left after release: %+v", ms)
	}
}

func TestPoolNeedsTwoNodes(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	adoptAll(t, c, s)
	_, err := c.Install(context.Background(), core.InstallRequest{ModelID: "qwen2.5:0.5b", Nodes: []string{s.NodeID()}, Pool: true})
	var fe *core.FitError
	if !errors.As(err, &fe) {
		t.Fatalf("single-node pool: %v", err)
	}
}

// nodeClient is an mTLS client presenting slave s's node certificate and
// accepting only the given peer node.
func nodeClient(t *testing.T, s *slave, peer string) *http.Client {
	t.Helper()
	pemFile := filepath.Join(s.cfg.DataDir, "node.pem")
	cert, err := tls.LoadX509KeyPair(pemFile, pemFile)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := pki.PoolFromPEM(caPEM)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := pki.VerifyPeer(raw, pool, pki.NodeURI(peer), x509.ExtKeyUsageServerAuth)
			return err
		},
	}}}
}
