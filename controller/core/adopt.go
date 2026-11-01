package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/pairing"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/controller/store"
)

// Errors surfaced to the management API.
var (
	ErrBusy            = errors.New("an adoption is already running for this node")
	ErrWrongState      = errors.New("node is not in a state that allows this")
	ErrPairingRequired = errors.New("this node requires its pairing code")
	ErrPreviouslyHeld  = errors.New("this hardware was released earlier; adopt it explicitly")
)

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func nodeBase(n *store.Node) string {
	port := n.Port
	if port == 0 {
		port = proto.DefaultAgentPort
	}
	return "https://" + net.JoinHostPort(n.Addr, strconv.Itoa(port))
}

// bootstrapClient talks to an unadopted Slave and accepts only the server
// certificate whose fingerprint the Slave announced (§3).
func bootstrapClient(wantFP string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // replaced by the fingerprint pin below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || pki.Fingerprint(cs.PeerCertificates[0].Raw) != wantFP {
					return errors.New("slave certificate does not match announced fingerprint")
				}
				return nil
			},
		}},
	}
}

// NodeClient talks to an adopted Slave over mTLS (§4).
func (c *Core) NodeClient(nodeID string, timeout time.Duration) *http.Client {
	pool, want := c.CA.Pool(), pki.NodeURI(nodeID)
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			Certificates:       []tls.Certificate{c.cert},
			InsecureSkipVerify: true, // replaced by CA pin + URI SAN below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				_, err := pki.VerifyPeer(raw, pool, want, x509.ExtKeyUsageServerAuth)
				return err
			},
		}},
	}
}

// Adopt runs the enrollment handshake (§3) against one node. explicit marks
// an admin request (CLI/UI) as opposed to auto-adoption.
func (c *Core) Adopt(ctx context.Context, nodeID, code string, explicit bool) (err error) {
	if !c.tryBegin(nodeID) {
		return ErrBusy
	}
	defer c.end(nodeID)

	n, err := c.Store.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	switch n.State {
	case store.StateDiscovered, store.StatePending:
	case store.StateReleased, store.StateForeign:
		if !explicit {
			return ErrWrongState
		}
	default:
		return fmt.Errorf("%w (state %s)", ErrWrongState, n.State)
	}
	if n.AgentState == proto.AgentAdopted {
		return fmt.Errorf("%w: node reports it is adopted by cluster %s", ErrWrongState, n.ClusterID)
	}

	log := c.log.With("node_id", nodeID, "addr", n.Addr)
	var serial string
	defer func() {
		if err == nil {
			return
		}
		// Undo: revoke a cert we may have issued, and park the node.
		_ = c.Store.Revoke(context.Background(), serial, nodeID)
		next := store.StateDiscovered
		if c.Mode() == ModeApprove || errors.Is(err, ErrPairingRequired) || errors.Is(err, ErrPreviouslyHeld) ||
			httpx.Code(err) == proto.ErrPairingFailed {
			next = store.StatePending
		}
		if explicit && n.State == store.StateReleased {
			next = store.StateReleased
		}
		_, _ = c.Store.UpdateNode(context.Background(), nodeID, func(m *store.Node) error {
			if m.State == store.StateEnrolling || m.State == store.StateDiscovered || m.State == store.StatePending {
				setState(m, next, time.Now().UTC())
			}
			m.CertSerial, m.LastError = "", err.Error()
			return nil
		})
		log.Warn("adoption failed", "err", err)
	}()

	client := bootstrapClient(n.FP)
	base := nodeBase(n)

	// 1. hello
	var hello proto.Hello
	if err := httpx.Do(ctx, client, http.MethodGet, base+"/v1/enroll/hello", nil, &hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if hello.NodeID != nodeID {
		return fmt.Errorf("hello: node id mismatch (%s)", hello.NodeID)
	}
	inv, _ := json.Marshal(hello.Inventory)
	matches, err := c.Store.FindByFingerprint(ctx, hello.HWFingerprint, nodeID)
	if err != nil {
		return err
	}
	if _, err := c.Store.UpdateNode(ctx, nodeID, func(m *store.Node) error {
		m.Inventory, m.HWFingerprint = inv, hello.HWFingerprint
		m.Returning = m.Returning || len(matches) > 0
		return nil
	}); err != nil {
		return err
	}
	if !explicit {
		for _, m := range matches {
			if m.State == store.StateReleased {
				return ErrPreviouslyHeld
			}
		}
	}

	// 2. optional CPace pairing
	var key []byte
	nonceC := make([]byte, 32)
	_, _ = rand.Read(nonceC)
	if hello.Pairing == proto.PairingCPace {
		if code == "" {
			return ErrPairingRequired
		}
		if key, err = c.pake(ctx, client, base, code, nodeID, hello.NonceS, n.FP, nonceC); err != nil {
			return err
		}
	}

	// 3. CSR
	csrReq := proto.CSRRequest{
		V: proto.Version, ClusterID: c.ClusterID, CACertPEM: string(c.CA.CertPEM),
		NonceC: nonceC, NonceS: hello.NonceS,
	}
	csrReq.MAC = pairing.MAC(key, "csr-req", []byte(c.ClusterID), pairing.Sum(c.CA.CertPEM), hello.NonceS, nonceC)
	var csrResp proto.CSRResponse
	if err := httpx.Do(ctx, client, http.MethodPost, base+"/v1/enroll/csr", csrReq, &csrResp); err != nil {
		return fmt.Errorf("csr: %w", err)
	}
	block, _ := pem.Decode([]byte(csrResp.CSRPEM))
	if block == nil {
		return errors.New("csr: not PEM")
	}
	if !pairing.CheckMAC(key, csrResp.MAC, "csr-resp", pairing.Sum(block.Bytes), nonceC) {
		return errors.New("csr: response MAC mismatch")
	}
	certPEM, serial, err := c.CA.SignNodeCSR([]byte(csrResp.CSRPEM), nodeID, pki.NodeCertLifetime)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}

	// 4. record intent, then complete. Choosing and saving the name happen
	// under one lock so concurrent adoptions can't pick the same one.
	c.nameMu.Lock()
	name, err := c.uniqueName(ctx, pickName(n, hello.Inventory, matches), nodeID, matches)
	if err != nil {
		c.nameMu.Unlock()
		return err
	}
	gen := n.DesiredGen + 1
	desired := proto.DesiredState{V: proto.Version, Generation: gen, NodeName: name, GCGraceS: int(c.cfg.ModelGCGrace / time.Second)}
	desiredJSON, _ := json.Marshal(desired)
	now := time.Now().UTC()
	_, err = c.Store.UpdateNode(ctx, nodeID, func(m *store.Node) error {
		setState(m, store.StateEnrolling, now)
		m.Name, m.CertSerial, m.LastError = name, serial, ""
		m.Desired, m.DesiredGen = desiredJSON, gen
		return nil
	})
	c.nameMu.Unlock()
	if err != nil {
		return err
	}
	complete := proto.EnrollComplete{
		V:                  proto.Version,
		NodeCertPEM:        string(certPEM),
		CAChainPEM:         string(c.CA.CertPEM),
		Controller:         proto.ControllerRef{ClusterID: c.ClusterID, Endpoints: c.endpoints, CertFP: c.CertFP()},
		NodeName:           name,
		HeartbeatIntervalS: int(c.cfg.HeartbeatInterval / time.Second),
		DesiredState:       desired,
	}
	complete.MAC = pairing.MAC(key, "complete", pairing.CompleteMACInput(complete))
	if err := httpx.Do(ctx, client, http.MethodPost, base+"/v1/enroll/complete", complete, nil); err != nil {
		return fmt.Errorf("complete: %w", err)
	}

	// Merge records of the same hardware (reinstall): the new node_id
	// inherits the name; the old identity's certificate is revoked.
	for _, m := range matches {
		if m.State == store.StateReleased && !explicit {
			continue
		}
		_ = c.Store.Revoke(ctx, m.CertSerial, m.NodeID)
		_ = c.Store.MoveAssignments(ctx, m.NodeID, nodeID)
		_ = c.Store.DeleteNode(ctx, m.NodeID)
		log.Info("merged reinstalled node record", "old_node_id", m.NodeID)
	}
	// Restores a reinstalled node's models; the node fetches the new
	// generation with its first heartbeat.
	c.SyncDesired(ctx, nodeID)
	c.mu.Lock()
	delete(c.backoff, nodeID)
	c.mu.Unlock()
	log.Info("enrollment complete, waiting for first heartbeat", "name", name)
	return nil
}

func (c *Core) pake(ctx context.Context, client *http.Client, base, code, nodeID string, nonceS []byte, fp string, nonceC []byte) ([]byte, error) {
	ya, st, err := pairing.ControllerStart(code, nodeID, nonceS, fp)
	if err != nil {
		return nil, err
	}
	var sr proto.PakeStartResp
	if err := httpx.Do(ctx, client, http.MethodPost, base+"/v1/enroll/pake/start", proto.PakeStart{V: proto.Version, YA: ya, NonceC: nonceC}, &sr); err != nil {
		return nil, fmt.Errorf("pairing: %w", err)
	}
	key, err := st.Finish(sr.YB)
	if err != nil {
		return nil, fmt.Errorf("pairing: %w", err)
	}
	var cr proto.PakeConfirmResp
	if err := httpx.Do(ctx, client, http.MethodPost, base+"/v1/enroll/pake/confirm",
		proto.PakeConfirm{V: proto.Version, TagC: pairing.MAC(key, "confirm-c", ya, sr.YB)}, &cr); err != nil {
		return nil, fmt.Errorf("pairing: %w", err)
	}
	if !bytes.Equal(cr.TagS, pairing.MAC(key, "confirm-s", ya, sr.YB)) {
		return nil, errors.New("pairing: slave confirmation mismatch")
	}
	return key, nil
}

func pickName(n *store.Node, inv *proto.Inventory, matches []*store.Node) string {
	for _, m := range matches {
		if m.Name != "" && m.State != store.StateReleased {
			return m.Name
		}
	}
	if n.Name != "" {
		return n.Name
	}
	if inv != nil && inv.Host.Hostname != "" && inv.Host.Hostname != "localhost" {
		return inv.Host.Hostname
	}
	return "node-" + n.NodeID[:8]
}

// uniqueName makes name unique among nodes so the CLI and dashboard can
// address nodes by name (machines often share a hostname such as "ubuntu").
// Records being merged into this node don't count as clashes.
func (c *Core) uniqueName(ctx context.Context, name, nodeID string, merging []*store.Node) (string, error) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	skip := map[string]bool{nodeID: true}
	for _, m := range merging {
		skip[m.NodeID] = true
	}
	taken := map[string]bool{}
	for _, o := range nodes {
		if !skip[o.NodeID] && o.Name != "" {
			taken[strings.ToLower(o.Name)] = true
		}
	}
	if !taken[strings.ToLower(name)] {
		return name, nil
	}
	for i := 2; ; i++ {
		if cand := fmt.Sprintf("%s-%d", name, i); !taken[strings.ToLower(cand)] {
			return cand, nil
		}
	}
}

// Release returns a node to the unadopted state (§5.5). The certificate is
// revoked even if the node is unreachable; it releases itself on its next
// heartbeat attempt.
func (c *Core) Release(ctx context.Context, nodeID string, keepModels bool) error {
	n, err := c.Store.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if n.State == store.StateAdopted || n.State == store.StateOffline || n.State == store.StateEnrolling {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := httpx.Do(rctx, c.NodeClient(nodeID, 5*time.Second), http.MethodPost, nodeBase(n)+"/v1/ops/release",
			proto.ReleaseRequest{KeepModels: keepModels}, nil)
		cancel()
		if err != nil {
			c.log.Warn("release: node unreachable, revoking anyway", "node_id", nodeID, "err", err)
		}
	}
	if err := c.Store.Revoke(ctx, n.CertSerial, nodeID); err != nil {
		return err
	}
	groups, err := c.Store.GroupsOfNode(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if err := c.removePool(ctx, g); err != nil {
			return err
		}
	}
	if err := c.Store.DeleteNodeAssignments(ctx, nodeID); err != nil {
		return err
	}
	_, err = c.Store.UpdateNode(ctx, nodeID, func(m *store.Node) error {
		setState(m, store.StateReleased, time.Now().UTC())
		m.CertSerial, m.Phase, m.Returning = "", "", false
		return nil
	})
	c.log.Info("node released", "node_id", nodeID)
	return err
}
