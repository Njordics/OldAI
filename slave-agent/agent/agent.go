// Package agent is the Slave Agent: it announces the node, serves the
// enrollment handshake, then accepts commands only from its adopting
// Controller over mTLS and reports heartbeats (docs/protocol.md).
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/slave-agent/inventory"
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
	lastObsGen  int64

	jobs   *jobs
	wakeHB chan struct{} // nudges the heartbeat loop (adoption, desired change)
	stateC chan struct{} // nudges the announcer (state change)
}

func New(cfg Config, log *slog.Logger) (*Agent, error) {
	cfg.defaults()
	a := &Agent{
		cfg:    cfg,
		log:    log,
		jobs:   newJobs(),
		wakeHB: make(chan struct{}, 1),
		stateC: make(chan struct{}, 1),
	}
	if err := a.loadOrInit(); err != nil {
		return nil, err
	}
	a.refreshInventory()
	if cfg.Pairing {
		a.rotatePairingCode("startup")
	}
	return a, nil
}

func (a *Agent) refreshInventory() {
	inv := inventory.Collect(a.cfg.ModelDir)
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
	a.log.Info("agent starting", "node_id", a.NodeID(), "state", a.State(), "port", a.port, "version", a.cfg.Version)

	srv := &http.Server{
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	tlsLn := tls.NewListener(a.ln, a.serverTLS())

	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){a.announceLoop, a.heartbeatLoop, a.controllerLocator, a.pairingRotator} {
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
				pool, want := ad.caPool, pki.ControllerURI(ad.ClusterID)
				return &tls.Config{
					MinVersion:   tls.VersionTLS13,
					Certificates: []tls.Certificate{ad.cert},
					ClientAuth:   tls.RequireAnyClientCert,
					VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
						_, err := pki.VerifyPeer(raw, pool, want, x509.ExtKeyUsageClientAuth)
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

// release returns the node to the unadopted state (§5.5). keepModels is
// honoured once models exist (build step 3).
func (a *Agent) release(reason string, newIdentity bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log.Warn("releasing node", "reason", reason, "new_identity", newIdentity)
	a.wipeAdoption()
	a.adopted = nil
	a.desired = proto.DesiredState{}
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
	a := &Agent{cfg: cfg, log: log, jobs: newJobs(), wakeHB: make(chan struct{}, 1), stateC: make(chan struct{}, 1)}
	if err := a.loadOrInit(); err != nil {
		return err
	}
	return a.release("factory reset from console", true)
}
