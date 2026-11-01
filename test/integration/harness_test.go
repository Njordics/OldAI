// Package integration runs a real Controller core and several Slave agents in
// one process over loopback, with discovery by UDP beacon (mDNS is off) and
// shortened timers. It exercises the same code paths as the VMs do.
package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aicluster/controller/core"
	"aicluster/controller/store"
	"aicluster/slave-agent/agent"
)

func logger(name string) *slog.Logger {
	var w io.Writer = io.Discard
	if os.Getenv("AICLUSTER_TEST_LOG") != "" {
		w = os.Stderr
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})).With("who", name)
}

func freeTCP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeUDP(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

type ctrl struct {
	*core.Core
	beaconAddr string
	stop       func()
}

func startController(t *testing.T, mode string) *ctrl {
	t.Helper()
	nodePort := freeTCP(t)
	beacon := "127.0.0.1:" + strconv.Itoa(freeUDP(t))
	c, err := core.New(core.Config{
		DataDir:             filepath.Join(t.TempDir(), "controller"),
		NodeAPIListen:       "127.0.0.1:" + strconv.Itoa(nodePort),
		Advertise:           []string{"https://127.0.0.1:" + strconv.Itoa(nodePort)},
		BeaconListen:        beacon,
		BeaconTargets:       []string{"127.0.0.1:9"}, // discard
		DefaultAdoptionMode: mode,
		HeartbeatInterval:   time.Second,
		EnrollTimeout:       10 * time.Second,
		MonitorEvery:        100 * time.Millisecond,
	}, logger("controller"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := c.Run(ctx); err != nil {
			t.Errorf("controller: %v", err)
		}
	}()
	stop := func() { cancel(); <-done; c.Close() }
	t.Cleanup(stop)
	return &ctrl{Core: c, beaconAddr: beacon, stop: stop}
}

type slave struct {
	*agent.Agent
	cfg    agent.Config
	cancel context.CancelFunc
	done   chan struct{}
}

type slaveOpt func(*agent.Config)

func withPairing(c *agent.Config)     { c.Pairing = true }
func withHW(fp string) slaveOpt       { return func(c *agent.Config) { c.HWFingerprint = fp } }
func withDataDir(dir string) slaveOpt { return func(c *agent.Config) { c.DataDir = dir } }

func startSlave(t *testing.T, c *ctrl, opts ...slaveOpt) *slave {
	t.Helper()
	dir := t.TempDir()
	cfg := agent.Config{
		DataDir:              filepath.Join(dir, "agent"),
		Listen:               "127.0.0.1:0",
		Version:              "test",
		BeaconTargets:        []string{c.beaconAddr},
		UnadoptedBeaconEvery: 200 * time.Millisecond,
		AdoptedBeaconEvery:   time.Second,
		PairingCodeFile:      filepath.Join(dir, "pairing-code"),
		HWFingerprint:        "hw-" + filepath.Base(dir),
	}
	for _, o := range opts {
		o(&cfg)
	}
	s := &slave{cfg: cfg}
	s.start(t)
	t.Cleanup(s.stop)
	return s
}

func (s *slave) start(t *testing.T) {
	t.Helper()
	a, err := agent.New(s.cfg, logger("slave"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Agent, s.cancel, s.done = a, cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("slave: %v", err)
		}
	}()
}

func (s *slave) stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
}

func (s *slave) restart(t *testing.T) {
	s.stop()
	s.start(t)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func (c *ctrl) node(t *testing.T, id string) *store.Node {
	t.Helper()
	n, err := c.Store.GetNode(context.Background(), id)
	if err != nil {
		return nil
	}
	return n
}

func (c *ctrl) waitState(t *testing.T, id, state string, timeout time.Duration) *store.Node {
	t.Helper()
	var n *store.Node
	waitFor(t, timeout, "node "+id[:8]+" to be "+state, func() bool {
		n = c.node(t, id)
		return n != nil && n.State == state
	})
	return n
}

func agentFactoryReset(s *slave) error {
	return agent.FactoryReset(s.cfg, logger("reset"))
}
