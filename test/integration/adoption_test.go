package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/store"
	"aicluster/controller/web"
)

const (
	adoptTimeout   = 15 * time.Second
	offlineTimeout = 8 * time.Second
)

// Auto mode: two Slaves are discovered by beacon, adopted, heartbeat, go
// offline when stopped, and come back with the same identity.
func TestAutoAdoptOfflineAndReturn(t *testing.T) {
	c := startController(t, core.ModeAuto)
	s1 := startSlave(t, c)
	s2 := startSlave(t, c)

	for _, s := range []*slave{s1, s2} {
		c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
		var n *store.Node
		waitFor(t, 5*time.Second, "node to converge", func() bool {
			n = c.node(t, s.NodeID())
			return n.Phase == proto.PhaseReady && n.ObservedGen == n.DesiredGen && n.DesiredGen > 0
		})
		if s.State() != proto.AgentAdopted {
			t.Fatalf("agent state = %s", s.State())
		}
		if len(n.Inventory) == 0 || n.HWFingerprint == "" {
			t.Fatal("inventory not recorded")
		}
	}
	// Both agents report this machine's hostname; names must still be unique.
	if a, b := c.node(t, s1.NodeID()).Name, c.node(t, s2.NodeID()).Name; strings.EqualFold(a, b) {
		t.Fatalf("duplicate node names: %q", a)
	}
	before := c.node(t, s1.NodeID())

	s1.stop()
	c.waitState(t, s1.NodeID(), store.StateOffline, offlineTimeout)
	if n := c.node(t, s2.NodeID()); n.State != store.StateAdopted {
		t.Fatalf("other node affected: %s", n.State)
	}

	s1.restart(t)
	after := c.waitState(t, s1.NodeID(), store.StateAdopted, adoptTimeout)
	if after.CertSerial != before.CertSerial || after.Name != before.Name || !after.AdoptedAt.Equal(before.AdoptedAt) {
		t.Fatalf("returning node was re-adopted instead of resuming: %+v -> %+v", before, after)
	}
}

// Approve-first mode: nodes wait as pending; a pairing node accepts only the
// right code, and a wrong code is rejected without leaking anything.
func TestApproveFirstAndPairing(t *testing.T) {
	c := startController(t, core.ModeApprove)
	plain := startSlave(t, c)
	paired := startSlave(t, c, withPairing)
	ctx := context.Background()

	c.waitState(t, plain.NodeID(), store.StatePending, adoptTimeout)
	n := c.waitState(t, paired.NodeID(), store.StatePending, adoptTimeout)
	if !n.PairingRequired {
		t.Fatal("pairing requirement not discovered")
	}
	waitFor(t, 5*time.Second, "pending node to be probed", func() bool { return len(c.node(t, plain.NodeID()).Inventory) > 0 })
	time.Sleep(500 * time.Millisecond)
	if plain.State() != proto.AgentUnadopted {
		t.Fatal("approve mode adopted a node on its own")
	}

	if err := c.Adopt(ctx, plain.NodeID(), "", true); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	c.waitState(t, plain.NodeID(), store.StateAdopted, adoptTimeout)

	if err := c.Adopt(ctx, paired.NodeID(), "", true); !errors.Is(err, core.ErrPairingRequired) {
		t.Fatalf("adopt without code: %v", err)
	}
	code := paired.PairingCode()
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if err := c.Adopt(ctx, paired.NodeID(), wrong, true); httpx.Code(err) != proto.ErrPairingFailed {
		t.Fatalf("adopt with wrong code: %v", err)
	}
	if paired.State() != proto.AgentUnadopted {
		t.Fatal("wrong code changed agent state")
	}
	if err := c.Adopt(ctx, paired.NodeID(), code, true); err != nil {
		t.Fatalf("adopt with right code: %v", err)
	}
	c.waitState(t, paired.NodeID(), store.StateAdopted, adoptTimeout)
}

// Three wrong codes rotate the pairing code.
func TestPairingCodeRotatesAfterFailures(t *testing.T) {
	c := startController(t, core.ModeApprove)
	s := startSlave(t, c, withPairing)
	c.waitState(t, s.NodeID(), store.StatePending, adoptTimeout)
	first := s.PairingCode()
	b, err := os.ReadFile(s.cfg.PairingCodeFile)
	if err != nil || strings.TrimSpace(string(b)) != first {
		t.Fatalf("pairing code file: %q, %v", b, err)
	}
	for i := 0; i < 3; i++ {
		wrong := "999999"
		if first == wrong {
			wrong = "888888"
		}
		_ = c.Adopt(context.Background(), s.NodeID(), wrong, true)
	}
	if s.PairingCode() == first {
		t.Fatal("code not rotated after 3 failures")
	}
}

// Release over the network: the agent returns to unadopted, the Controller
// does not auto-adopt it again, and an explicit adopt brings it back.
func TestReleaseAndExplicitReadopt(t *testing.T) {
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	ctx := context.Background()
	old := c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)

	if err := c.Release(ctx, s.NodeID(), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "agent to be unadopted", func() bool { return s.State() == proto.AgentUnadopted })
	waitFor(t, 5*time.Second, "controller to see unadopted beacon", func() bool {
		return c.node(t, s.NodeID()).AgentState == proto.AgentUnadopted
	})
	time.Sleep(time.Second)
	if n := c.node(t, s.NodeID()); n.State != store.StateReleased {
		t.Fatalf("released node auto-adopted: %s", n.State)
	}

	if err := c.Adopt(ctx, s.NodeID(), "", true); err != nil {
		t.Fatal(err)
	}
	n := c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	if n.CertSerial == old.CertSerial {
		t.Fatal("re-adoption reused the revoked certificate")
	}
}

// A node released while offline learns of it from its next heartbeat
// (certificate revoked) and resets itself.
func TestReleasedWhileOffline(t *testing.T) {
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)

	s.stop()
	if err := c.Release(context.Background(), s.NodeID(), false); err != nil {
		t.Fatal(err)
	}
	s.restart(t)
	waitFor(t, 10*time.Second, "agent to reset after revocation", func() bool { return s.State() == proto.AgentUnadopted })
	time.Sleep(time.Second)
	if n := c.node(t, s.NodeID()); n.State != store.StateReleased {
		t.Fatalf("state = %s, want released", n.State)
	}
}

// A reinstalled machine (new disk, new node_id, same hardware) is adopted
// automatically and takes over the old record's name.
func TestReinstallSameHardware(t *testing.T) {
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c, withHW("hw-reinstall"))
	old := c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	s.stop()

	s2 := startSlave(t, c, withHW("hw-reinstall"))
	if s2.NodeID() == s.NodeID() {
		t.Fatal("expected a new node id")
	}
	n := c.waitState(t, s2.NodeID(), store.StateAdopted, adoptTimeout)
	if n.Name != old.Name {
		t.Fatalf("name not inherited: %q vs %q", n.Name, old.Name)
	}
	if c.node(t, s.NodeID()) != nil {
		t.Fatal("old record not merged")
	}
	if revoked, _ := c.Store.IsRevoked(context.Background(), old.CertSerial); !revoked {
		t.Fatal("old identity's certificate not revoked")
	}
}

// A factory reset on the console (same disk, new identity) is recognised by
// hardware and re-adopted automatically.
func TestFactoryResetReadopted(t *testing.T) {
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	old := c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	s.stop()
	if err := agentFactoryReset(s); err != nil {
		t.Fatal(err)
	}
	s.start(t)
	if s.NodeID() == old.NodeID {
		t.Fatal("factory reset kept node id")
	}
	n := c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	if n.Name != old.Name {
		t.Fatalf("name not inherited after factory reset")
	}
}

// The agent must refuse enrollment from a Controller that pinned the wrong
// fingerprint, and the agent's state must survive a restart mid-cluster.
func TestFingerprintPinAndRestartIdempotent(t *testing.T) {
	c := startController(t, core.ModeApprove)
	s := startSlave(t, c)
	n := c.waitState(t, s.NodeID(), store.StatePending, adoptTimeout)

	// Spoofed sighting: same node, attacker's fingerprint.
	spoof := core.Sighting{NodeID: n.NodeID, Addr: n.Addr, Port: n.Port, FP: "sha256:00", State: proto.AgentUnadopted}
	c.OnSighting(context.Background(), spoof)
	if err := c.Adopt(context.Background(), n.NodeID, "", true); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("adopt with wrong pin: %v", err)
	}
	// The real beacon restores the right fingerprint.
	waitFor(t, 5*time.Second, "real fingerprint", func() bool { return c.node(t, n.NodeID).FP != "sha256:00" })
	if err := c.Adopt(context.Background(), n.NodeID, "", true); err != nil {
		t.Fatal(err)
	}
	c.waitState(t, n.NodeID, store.StateAdopted, adoptTimeout)

	// Restart both sides: nothing should change.
	s.restart(t)
	time.Sleep(2 * time.Second)
	if got := c.node(t, n.NodeID); got.State != store.StateAdopted || s.State() != proto.AgentAdopted {
		t.Fatalf("after restart: controller=%s agent=%s", got.State, s.State())
	}
}

func TestMgmtAPIAuth(t *testing.T) {
	c := startController(t, core.ModeAuto)
	srv := httptest.NewServer(web.New(c.Core, logger("web")).Handler())
	defer srv.Close()
	ctx := context.Background()

	err := httpx.Do(ctx, srv.Client(), http.MethodGet, srv.URL+"/api/v1/nodes", nil, nil)
	if ae, ok := err.(*httpx.APIError); !ok || ae.Status != http.StatusUnauthorized {
		t.Fatalf("no token: %v", err)
	}
	auth := []string{"Authorization", "Bearer " + c.AdminToken}
	var s web.Settings
	if err := httpx.Do(ctx, srv.Client(), http.MethodPut, srv.URL+"/api/v1/settings", web.Settings{AdoptionMode: "approve"}, &s, auth...); err != nil || s.AdoptionMode != "approve" {
		t.Fatalf("set mode: %v %+v", err, s)
	}
	if err := httpx.Do(ctx, srv.Client(), http.MethodPut, srv.URL+"/api/v1/settings", web.Settings{AdoptionMode: "yolo"}, nil, auth...); err == nil {
		t.Fatal("accepted invalid mode")
	}
}
