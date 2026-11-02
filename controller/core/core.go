// Package core is the Controller's brain: node registry, internal CA,
// adoption, heartbeat ingest and health monitoring. It is the only writer of
// the SQLite store.
package core

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"aicluster/common/discovery"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/common/selfupdate"
	"aicluster/controller/modelcache"
	"aicluster/controller/provision"
	"aicluster/controller/slavenet"
	"aicluster/controller/store"
)

// Adoption modes (setting "adoption_mode").
const (
	ModeAuto    = "auto"
	ModeApprove = "approve"
)

type Config struct {
	DataDir       string
	NodeAPIListen string // ":7443"
	// Advertise lists node-API endpoints given to Slaves ("https://ip:port").
	// Empty means every local IPv4 with the node-API port.
	Advertise []string

	BeaconListen  string // ":47800"; "" disables
	BeaconPort    int
	BeaconTargets []string
	MDNS          bool

	DefaultAdoptionMode string
	HeartbeatInterval   time.Duration
	EnrollTimeout       time.Duration
	DiscoveredTTL       time.Duration
	MonitorEvery        time.Duration
	ControllerBeacon    time.Duration
	Version             string

	// ProvisionerAddr is "unix:/run/aicluster/provisioner.sock" or "tcp:host:port".
	ProvisionerAddr string
	// ModelDir holds the model cache (default DataDir/models).
	ModelDir string
	// RegistryUpstream is where models come from (default registry.ollama.ai).
	RegistryUpstream string
	// ModelGCGrace is how long a Slave keeps an unassigned model on disk.
	ModelGCGrace time.Duration
	// SlaveNet moves adopted Slaves onto the Controller's own subnet on the
	// same wire (§12): "off" (default), "auto", or a CIDR.
	SlaveNet string
	// SlaveIface carries the Slave network (default: the default-route interface).
	SlaveIface string
	// BuildCommit is the commit this Controller runs (default: the binary's
	// VCS stamp). Installer installs updates; nil means it can't (§13).
	BuildCommit string
	Installer   selfupdate.Installer
}

func (c *Config) defaults() {
	if c.NodeAPIListen == "" {
		c.NodeAPIListen = ":7443"
	}
	if c.BeaconPort == 0 {
		c.BeaconPort = proto.DefaultBeaconPort
	}
	if c.DefaultAdoptionMode == "" {
		c.DefaultAdoptionMode = ModeAuto
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = proto.DefaultHeartbeatInterval
	}
	if c.EnrollTimeout == 0 {
		c.EnrollTimeout = 60 * time.Second
	}
	if c.DiscoveredTTL == 0 {
		c.DiscoveredTTL = 5 * time.Minute
	}
	if c.MonitorEvery == 0 {
		c.MonitorEvery = time.Second
	}
	if c.ControllerBeacon == 0 {
		c.ControllerBeacon = 30 * time.Second
	}
	if c.ProvisionerAddr == "" {
		c.ProvisionerAddr = "unix:/run/aicluster/provisioner.sock"
	}
	if c.ModelDir == "" {
		c.ModelDir = filepath.Join(c.DataDir, "models")
	}
	if c.ModelGCGrace == 0 {
		c.ModelGCGrace = 24 * time.Hour
	}
	if c.BuildCommit == "" {
		c.BuildCommit = selfupdate.BuildCommit()
	}
}

type Core struct {
	cfg   Config
	log   *slog.Logger
	Store *store.Store

	ClusterID  string
	CA         *pki.CA
	cert       tls.Certificate
	AdminToken string
	loginCode  string // dashboard login code; see logincode.go

	nodeLn    net.Listener
	endpoints []string
	slaveNet  *slavenet.Net // nil unless the Slave network is on

	Prov   *provision.Client
	Models *modelcache.Cache

	mu       sync.Mutex
	mode     string
	nameMu   sync.Mutex         // serialises node-name allocation
	inflight map[string]bool    // node IDs with an adoption or probe running
	backoff  map[string]backoff // auto-adoption retry schedule
	roll     rolloutState       // in-place update; see rollout.go

	runCtx context.Context // Run's context, for work started by API calls
}

type backoff struct {
	next  time.Time
	delay time.Duration
}

func New(cfg Config, log *slog.Logger) (*Core, error) {
	cfg.defaults()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "aicluster.db"))
	if err != nil {
		return nil, err
	}
	c := &Core{cfg: cfg, log: log, Store: st, inflight: map[string]bool{}, backoff: map[string]backoff{}, runCtx: context.Background()}
	ctx := context.Background()

	if c.ClusterID, err = st.Setting(ctx, "cluster_id", ""); err != nil {
		return nil, err
	}
	if c.ClusterID == "" {
		c.ClusterID = uuid.NewString()
		if err := st.SetSetting(ctx, "cluster_id", c.ClusterID); err != nil {
			return nil, err
		}
		log.Info("new cluster created", "cluster_id", c.ClusterID)
	}
	if c.mode, err = st.Setting(ctx, "adoption_mode", cfg.DefaultAdoptionMode); err != nil {
		return nil, err
	}
	if c.Prov, err = provision.New(cfg.ProvisionerAddr); err != nil {
		return nil, err
	}
	if c.Models, err = modelcache.New(cfg.ModelDir, cfg.RegistryUpstream, log.With("component", "modelcache")); err != nil {
		return nil, fmt.Errorf("model cache: %w", err)
	}
	c.Models.OnReady = c.onModelCached
	if err := c.setupSlaveNet(ctx); err != nil {
		log.Error("slave network disabled; Slaves stay on the home network", "err", err)
	}
	if err := c.loadPKI(); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if err := c.loadAdminToken(); err != nil {
		return nil, err
	}
	if err := c.loadLoginCode(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Core) Close() error { return c.Store.Close() }

func (c *Core) loadPKI() error {
	caDir := filepath.Join(c.cfg.DataDir, "ca")
	ca, err := pki.LoadCA(caDir)
	if errors.Is(err, os.ErrNotExist) {
		if ca, err = pki.NewCA(c.ClusterID); err == nil {
			err = ca.Save(caDir)
		}
	}
	if err != nil {
		return err
	}
	c.CA = ca

	// Browsers check the dashboard certificate by name/IP, so it is reissued
	// whenever the machine's addresses change (e.g. a new DHCP lease).
	host, _ := os.Hostname()
	dnsNames, ips := pki.ControllerSANs(host, append(discovery.LocalIPv4s(), "127.0.0.1"))
	certFile := filepath.Join(c.cfg.DataDir, "controller.pem")
	if cert, err := tls.LoadX509KeyPair(certFile, certFile); err == nil {
		leaf, _ := x509.ParseCertificate(cert.Certificate[0])
		if leaf != nil && pki.HasURI(leaf, pki.ControllerURI(c.ClusterID)) &&
			time.Until(leaf.NotAfter) > 30*24*time.Hour && leaf.CheckSignatureFrom(ca.Cert) == nil &&
			coversIPs(leaf, ips) {
			c.cert = cert
			return nil
		}
	}
	cert, err := ca.IssueController(c.ClusterID, dnsNames, ips)
	if err != nil {
		return err
	}
	keyPEM, err := pki.EncodeKeyPEM(cert.PrivateKey)
	if err != nil {
		return err
	}
	certPEM := pemCert(cert.Certificate[0])
	if err := pki.WriteFileAtomic(certFile, append(certPEM, keyPEM...), 0o600); err != nil {
		return err
	}
	c.cert = cert
	return nil
}

func coversIPs(leaf *x509.Certificate, ips []net.IP) bool {
	for _, ip := range ips {
		found := false
		for _, have := range leaf.IPAddresses {
			if have.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (c *Core) loadAdminToken() error {
	p := filepath.Join(c.cfg.DataDir, "admin.token")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		c.AdminToken = strings.TrimSpace(string(b))
		return nil
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	c.AdminToken = hex.EncodeToString(b)
	return pki.WriteFileAtomic(p, []byte(c.AdminToken+"\n"), 0o600)
}

// Cert is the Controller's certificate, used by every listener it runs.
func (c *Core) Cert() tls.Certificate { return c.cert }

// CertFP is the fingerprint Slaves record for the Controller.
func (c *Core) CertFP() string { return pki.Fingerprint(c.cert.Certificate[0]) }

func (c *Core) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

func (c *Core) SetMode(ctx context.Context, mode string) error {
	if mode != ModeAuto && mode != ModeApprove {
		return fmt.Errorf("adoption_mode must be %q or %q", ModeAuto, ModeApprove)
	}
	if err := c.Store.SetSetting(ctx, "adoption_mode", mode); err != nil {
		return err
	}
	c.mu.Lock()
	c.mode = mode
	c.mu.Unlock()
	c.log.Info("adoption mode changed", "mode", mode)
	return nil
}

// Endpoints are the node-API URLs handed to Slaves.
func (c *Core) Endpoints() []string { return c.endpoints }

// ListenNodeAPI binds the node-API port so its real port is known before
// endpoints are computed (tests use ":0").
func (c *Core) ListenNodeAPI() error {
	ln, err := net.Listen("tcp", c.cfg.NodeAPIListen)
	if err != nil {
		return err
	}
	c.nodeLn = ln
	port := ln.Addr().(*net.TCPAddr).Port
	c.endpoints = c.cfg.Advertise
	if len(c.endpoints) == 0 {
		var slaveEP string
		for _, ip := range discovery.LocalIPv4s() {
			ep := "https://" + net.JoinHostPort(ip, strconv.Itoa(port))
			if c.slaveNet != nil && ip == c.slaveNet.ControllerIP.String() {
				slaveEP = ep
				continue
			}
			c.endpoints = append(c.endpoints, ep)
		}
		// Last: a Slave still on the home network would wait for it to time
		// out before trying the others.
		if slaveEP != "" {
			c.endpoints = append(c.endpoints, slaveEP)
		}
	}
	return nil
}

// Run starts discovery, announcement, the monitor and the node API, and
// blocks until ctx ends.
func (c *Core) Run(ctx context.Context) error {
	if c.nodeLn == nil {
		if err := c.ListenNodeAPI(); err != nil {
			return err
		}
	}
	c.log.Info("controller core starting", "cluster_id", c.ClusterID, "mode", c.Mode(), "endpoints", c.endpoints)
	c.mu.Lock()
	c.runCtx = ctx
	c.mu.Unlock()
	c.ensureAssignedCached(ctx)
	if c.cfg.Installer != nil {
		c.resumeRollout(ctx)
	}
	if c.slaveNet != nil {
		c.serveSlaveNet(ctx)
	}
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){c.discoveryLoop, c.announceLoop, c.monitorLoop} {
		wg.Add(1)
		go func() { defer wg.Done(); loop(ctx) }()
	}
	err := c.serveNodeAPI(ctx)
	wg.Wait()
	return err
}

func (c *Core) tryBegin(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[id] {
		return false
	}
	c.inflight[id] = true
	return true
}

func (c *Core) end(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
}

func (c *Core) isInflight(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[id]
}
