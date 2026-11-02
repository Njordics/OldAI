// Package agent is the Slave Agent: it announces the node, serves the
// enrollment handshake, then accepts commands only from its adopting
// Controller over mTLS and reports heartbeats (docs/protocol.md).
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/common/selfupdate"
	"aicluster/slave-agent/inventory"
	"aicluster/slave-agent/netcfg"
	"aicluster/slave-agent/runtime"
	"aicluster/slave-agent/thermal"
)

// Config is the agent's local configuration. Everything else comes from the
// Controller.
type Config struct {
	DataDir  string // persistent state, e.g. /var/lib/aicluster
	ModelDir string // where models live; used for disk-free reporting
	Listen   string // TLS listen address, default ":7450"
	Version  string

	// Pairing makes adoption require the 6-digit code shown on the console.
	Pairing bool
	// PairingCodeFile is where the console status screen reads the code.
	PairingCodeFile string

	MDNS          bool
	BeaconPort    int
	BeaconTargets []string // override broadcast destinations (tests)
	// ControllerBeaconListen receives controller beacons ("" disables).
	ControllerBeaconListen string

	// HWFingerprint overrides the computed fingerprint (tests only).
	HWFingerprint string
	// UnadoptedBeaconEvery / AdoptedBeaconEvery default to 5 s / 30 s.
	UnadoptedBeaconEvery time.Duration
	AdoptedBeaconEvery   time.Duration

	// Runtime is "ollama" (default), "fake" (dev/tests: real pulls, no
	// inference) or "none".
	Runtime   string
	OllamaURL string
	// OllamaModels is Ollama's OLLAMA_MODELS directory (default
	// ModelDir/ollama); other nodes copy model files from its blobs.
	OllamaModels string
	// FakeBytesPerSec slows the fake runtime's pulls so progress is visible.
	FakeBytesPerSec int64
	// RegistryShimListen is the loopback address the runtime pulls from.
	RegistryShimListen string
	// ReconcileEvery is the model reconcile period (default 15 s).
	ReconcileEvery time.Duration
	// InferenceSlots is how many requests the runtime serves at once
	// (Ollama's OLLAMA_NUM_PARALLEL); the gateway balances on it.
	InferenceSlots int
	// FakeLabel names this node in the fake runtime's replies.
	FakeLabel string
	// LlamaCppDir holds llama.cpp's rpc-server and llama-server, used for
	// models split across nodes.
	LlamaCppDir string
	// ManageNetwork lets the agent move its interface onto the Controller's
	// Slave network when asked (Linux with systemd-networkd only).
	ManageNetwork bool
	// BuildCommit is reported as the running commit (default: the binary's
	// VCS stamp). Installer installs updates; nil means this node can't.
	BuildCommit string
	Installer   selfupdate.Installer
	// Drivers installs the GPU drivers the Controller picks; nil means
	// this node can't.
	Drivers DriverSystem
	// ScanGPUs replaces the PCI scan (tests).
	ScanGPUs func() []proto.GPUInfo
	// Thermal reads temperatures (default: this machine's). Fans drives
	// fans when the Controller asks; nil means this node can't.
	Thermal *thermal.Monitor
	Fans    *thermal.FanController
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":7450"
	}
	if c.ModelDir == "" {
		c.ModelDir = c.DataDir
	}
	if c.BeaconPort == 0 {
		c.BeaconPort = proto.DefaultBeaconPort
	}
	if c.UnadoptedBeaconEvery == 0 {
		c.UnadoptedBeaconEvery = 5 * time.Second
	}
	if c.AdoptedBeaconEvery == 0 {
		c.AdoptedBeaconEvery = 30 * time.Second
	}
	if c.PairingCodeFile == "" {
		c.PairingCodeFile = "/run/aicluster/" + filePairingCode
	}
	if c.Runtime == "" {
		c.Runtime = "ollama"
	}
	if c.OllamaURL == "" {
		c.OllamaURL = "http://127.0.0.1:11434"
	}
	if c.OllamaModels == "" {
		c.OllamaModels = filepath.Join(c.ModelDir, "ollama")
	}
	if c.RegistryShimListen == "" {
		c.RegistryShimListen = "127.0.0.1:7452"
	}
	if c.ReconcileEvery == 0 {
		c.ReconcileEvery = 15 * time.Second
	}
	if c.InferenceSlots == 0 {
		c.InferenceSlots = 4
	}
	if c.BuildCommit == "" {
		c.BuildCommit = selfupdate.BuildCommit()
	}
}

type Agent struct {
	cfg Config
	log *slog.Logger

	ln   net.Listener
	port int

	mu          sync.RWMutex
	nodeID      string
	bootstrap   tls.Certificate
	bootstrapFP string
	adopted     *adopted
	desired     proto.DesiredState
	inv         *proto.Inventory
	hwFP        string
	enroll      enrollState
	pairing     pairingState
	seq         uint64
	lastObsSent time.Time
	lastObs     string
	models      modelTracker
	desiredC    chan struct{} // closed and replaced on every desired-state change

	jobs   *jobs
	wakeHB chan struct{} // nudges the heartbeat loop (adoption, desired change)
	stateC chan struct{} // nudges the announcer (state change)

	rt         runtime.Runtime
	poolRT     runtime.PoolRuntime
	pools      poolState
	infer      inferStats
	reconcileC chan struct{}
	shimLn     net.Listener
	shimLn6    net.Listener // [::1] twin of shimLn: the redirect target "localhost" may resolve there
	shimMu     sync.Mutex
	shimHTTP   *http.Client
	shimFor    *adopted

	netMgr *netcfg.Manager // nil when this node can't switch networks
	net    netState
	netC   chan struct{}

	upd   updState
	drv   drvState
	therm thermState
	peers peerState
}

func New(cfg Config, log *slog.Logger) (*Agent, error) {
	cfg.defaults()
	a := &Agent{
		cfg:        cfg,
		log:        log,
		jobs:       newJobs(),
		wakeHB:     make(chan struct{}, 1),
		stateC:     make(chan struct{}, 1),
		reconcileC: make(chan struct{}, 1),
		netC:       make(chan struct{}, 1),
		desiredC:   make(chan struct{}),
		models:     newModelTracker(),
	}
	if err := a.loadOrInit(); err != nil {
		return nil, err
	}
	a.loadManaged()
	a.initNetwork()
	a.initUpdate()
	if cfg.Fans != nil {
		// Fans a crashed agent left boosted go back to the firmware.
		if err := thermal.RestoreFromFile(cfg.Fans.StateFile); err != nil {
			log.Warn("restore fans", "err", err)
		}
	}
	a.pools.entries = map[string]*poolEntry{}
	switch cfg.Runtime {
	case "ollama":
		a.rt = runtime.NewOllama(cfg.OllamaURL)
		dir := cfg.LlamaCppDir
		if dir == "" {
			dir = "/usr/lib/aicluster/llama.cpp"
		}
		a.poolRT = &runtime.LlamaCpp{Dir: dir, CacheDir: filepath.Join(cfg.ModelDir, "rpc-cache")}
	case "fake":
		f := runtime.NewFake(cfg.ModelDir)
		f.BytesPerSec = cfg.FakeBytesPerSec
		if f.Label = cfg.FakeLabel; f.Label == "" {
			f.Label, _ = os.Hostname()
		}
		a.poolRT = &runtime.FakePool{Label: f.Label, TokenDelay: f.TokenDelay}
		a.rt = f
	case "none":
	default:
		return nil, fmt.Errorf("unknown runtime %q", cfg.Runtime)
	}
	a.refreshInventory()
	if cfg.Pairing {
		a.rotatePairingCode("startup")
	}
	return a, nil
}

func (a *Agent) refreshInventory() {
	inv := inventory.Collect(a.cfg.ModelDir)
	if a.cfg.ScanGPUs != nil {
		inv.GPUs = a.cfg.ScanGPUs()
		inventory.Summarize(inv)
	}
	fp := a.cfg.HWFingerprint
	if fp == "" {
		fp = inventory.HWFingerprint(inv)
	}
	a.mu.Lock()
	a.inv, a.hwFP = inv, fp
	a.mu.Unlock()
}

// NodeID, State, Port and PairingCode are read-only accessors (tests, CLI).
func (a *Agent) NodeID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.nodeID
}

func (a *Agent) State() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.stateLocked()
}

func (a *Agent) stateLocked() string {
	switch {
	case a.adopted != nil:
		return proto.AgentAdopted
	case a.enroll.lockedUntil.After(time.Now()):
		return proto.AgentEnrolling
	default:
		return proto.AgentUnadopted
	}
}

func (a *Agent) Port() int { return a.port }

// Runtime exposes the model runtime (tests inspect the fake one).
func (a *Agent) Runtime() runtime.Runtime { return a.rt }

func (a *Agent) PairingCode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.pairing.code
}

func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Listen binds the TLS port. It is separate from Run so callers (tests) can
// learn the port before discovery starts.
func (a *Agent) Listen() error {
	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return err
	}
	a.ln = ln
	a.port = ln.Addr().(*net.TCPAddr).Port
	return nil
}

// Run serves until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.ln == nil {
		if err := a.Listen(); err != nil {
			return err
		}
	}
	if a.shimLn == nil {
		if err := a.listenShim(); err != nil {
			return fmt.Errorf("registry shim: %w", err)
		}
	}
	a.log.Info("agent starting", "node_id", a.NodeID(), "state", a.State(), "port", a.port, "version", a.cfg.Version)

	srv := &http.Server{
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	tlsLn := tls.NewListener(a.ln, a.serverTLS())

	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){a.announceLoop, a.heartbeatLoop, a.controllerLocator, a.pairingRotator, a.serveShim, a.modelLoop, a.netLoop, a.updateLoop, a.driverLoop, a.thermalLoop} {
		wg.Add(1)
		go func() { defer wg.Done(); loop(ctx) }()
	}

	errC := make(chan error, 1)
	go func() { errC <- srv.Serve(tlsLn) }()
	select {
	case <-ctx.Done():
	case err := <-errC:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	wg.Wait()
	return nil
}

// serverTLS picks the certificate and client-auth policy per connection, so
// adoption and release take effect without restarting the listener.
func (a *Agent) serverTLS() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			a.mu.RLock()
			defer a.mu.RUnlock()
			if ad := a.adopted; ad != nil {
				// The Controller, and other nodes of this cluster (pool
				// tunnels). Each route then checks exactly who may call it.
				pool, ctrl := ad.caPool, pki.ControllerURI(ad.ClusterID)
				return &tls.Config{
					MinVersion:   tls.VersionTLS13,
					Certificates: []tls.Certificate{ad.cert},
					ClientAuth:   tls.RequireAnyClientCert,
					VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
						leaf, err := pki.VerifyPeer(raw, pool, "", x509.ExtKeyUsageClientAuth)
						if err == nil && !pki.HasURI(leaf, ctrl) && pki.NodeIDFromCert(leaf) == "" {
							err = errors.New("peer is neither this cluster's controller nor one of its nodes")
						}
						return err
					},
				}, nil
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{a.bootstrap},
			}, nil
		},
	}
}

// release returns the node to the unadopted state (§5.5).
func (a *Agent) release(reason string, newIdentity, keepModels bool) error {
	a.stopAllPools()
	if !keepModels {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		a.removeAllModels(ctx)
		cancel()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log.Warn("releasing node", "reason", reason, "new_identity", newIdentity)
	a.wipeAdoption()
	a.adopted = nil
	a.desired = proto.DesiredState{}
	a.signalDesiredLocked()
	a.enroll = enrollState{}
	if newIdentity {
		_ = os.Remove(a.file(fileIdentity))
		if err := a.loadOrInitIdentityLocked(); err != nil {
			return err
		}
	}
	if err := a.newBootstrap(); err != nil {
		return err
	}
	if a.cfg.Pairing {
		a.rotatePairingCodeLocked("release")
	}
	nudge(a.stateC)
	return nil
}

func (a *Agent) loadOrInitIdentityLocked() error {
	a.adopted = nil
	return a.loadOrInit()
}

// FactoryReset is the console escape hatch: wipe adoption and get a new
// node_id, without needing the Controller.
func FactoryReset(cfg Config, log *slog.Logger) error {
	cfg.defaults()
	a := &Agent{cfg: cfg, log: log, jobs: newJobs(), wakeHB: make(chan struct{}, 1), stateC: make(chan struct{}, 1),
		reconcileC: make(chan struct{}, 1), desiredC: make(chan struct{}), models: newModelTracker()}
	if err := a.loadOrInit(); err != nil {
		return err
	}
	// Back to the home network, so a new Controller can find this node.
	a.initNetwork()
	a.leaveSlaveNet(context.Background(), "factory reset from console")
	// Models stay on disk: the runtime may not be running at the console.
	return a.release("factory reset from console", true, true)
}

// signalDesiredLocked wakes everything waiting on a desired-state change.
// Caller holds a.mu.
func (a *Agent) signalDesiredLocked() {
	close(a.desiredC)
	a.desiredC = make(chan struct{})
	nudge(a.reconcileC)
	nudge(a.netC)
}
