package agent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"aicluster/common/pki"
	"aicluster/common/proto"
)

// On-disk layout under Config.DataDir. adoption.json is written last and
// removed first, so it acts as the commit marker for the adopted state.
const (
	fileIdentity     = "identity.json"
	fileBootstrapCrt = "bootstrap.crt"
	fileBootstrapKey = "bootstrap.key"
	fileNode         = "node.pem" // cert + key in one file so renewal is one atomic write
	fileCA           = "ca.crt"
	fileAdoption     = "adoption.json"
	fileDesired      = "desired.json"
	filePairingCode  = "pairing-code"
)

type identity struct {
	NodeID    string    `json:"node_id"`
	CreatedAt time.Time `json:"created_at"`
}

// adoption is what the Slave remembers about its Controller.
type adoption struct {
	ClusterID          string              `json:"cluster_id"`
	Controller         proto.ControllerRef `json:"controller"`
	NodeName           string              `json:"node_name"`
	HeartbeatIntervalS int                 `json:"heartbeat_interval_s"`
	AdoptedAt          time.Time           `json:"adopted_at"`
}

// adopted bundles everything loaded when the node belongs to a cluster.
type adopted struct {
	adoption
	cert   tls.Certificate
	leaf   *x509.Certificate
	caPool *x509.CertPool
	caPEM  []byte
}

func (a *Agent) file(name string) string { return filepath.Join(a.cfg.DataDir, name) }

func (a *Agent) writeJSON(name string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return pki.WriteFileAtomic(a.file(name), b, perm)
}

func (a *Agent) readJSON(name string, v any) error {
	b, err := os.ReadFile(a.file(name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// loadOrInit restores identity, bootstrap cert and (if present) adoption.
func (a *Agent) loadOrInit() error {
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return err
	}
	var id identity
	if err := a.readJSON(fileIdentity, &id); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("identity: %w", err)
		}
		id = identity{NodeID: uuid.NewString(), CreatedAt: time.Now().UTC()}
		if err := a.writeJSON(fileIdentity, id, 0o644); err != nil {
			return err
		}
	}
	a.nodeID = id.NodeID

	if err := a.loadBootstrap(); err != nil {
		if err := a.newBootstrap(); err != nil {
			return err
		}
	}

	ad, err := a.loadAdoption()
	switch {
	case err == nil:
		a.adopted = ad
	case errors.Is(err, os.ErrNotExist):
	default:
		a.log.Error("adoption state unreadable, returning to unadopted", "err", err)
		a.wipeAdoption()
	}

	var ds proto.DesiredState
	if a.adopted != nil && a.readJSON(fileDesired, &ds) == nil {
		a.desired = ds
	}
	return nil
}

func (a *Agent) loadBootstrap() error {
	c, err := tls.LoadX509KeyPair(a.file(fileBootstrapCrt), a.file(fileBootstrapKey))
	if err != nil {
		return err
	}
	a.bootstrap = c
	a.bootstrapFP = pki.Fingerprint(c.Certificate[0])
	return nil
}

func (a *Agent) newBootstrap() error {
	certPEM, keyPEM, err := pki.NewBootstrapCert(a.nodeID)
	if err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(a.file(fileBootstrapKey), keyPEM, 0o600); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(a.file(fileBootstrapCrt), certPEM, 0o644); err != nil {
		return err
	}
	return a.loadBootstrap()
}

func (a *Agent) loadAdoption() (*adopted, error) {
	var ad adoption
	if err := a.readJSON(fileAdoption, &ad); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(a.file(fileNode), a.file(fileNode))
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(a.file(fileCA))
	if err != nil {
		return nil, err
	}
	pool, err := pki.PoolFromPEM(caPEM)
	if err != nil {
		return nil, err
	}
	return &adopted{adoption: ad, cert: cert, leaf: leaf, caPool: pool, caPEM: caPEM}, nil
}

// persistAdoption writes the node identity, the CA pin and finally the
// commit marker.
func (a *Agent) persistAdoption(ad adoption, keyPEM, certPEM, caPEM []byte) error {
	if err := a.writeNodeIdentity(keyPEM, certPEM); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(a.file(fileCA), caPEM, 0o644); err != nil {
		return err
	}
	return a.writeJSON(fileAdoption, ad, 0o644)
}

func (a *Agent) writeNodeIdentity(keyPEM, certPEM []byte) error {
	return pki.WriteFileAtomic(a.file(fileNode), append(append([]byte{}, certPEM...), keyPEM...), 0o600)
}

// wipeAdoption removes the commit marker first, then the rest.
func (a *Agent) wipeAdoption() {
	for _, f := range []string{fileAdoption, fileNode, fileCA, fileDesired} {
		if err := os.Remove(a.file(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Warn("remove failed", "file", f, "err", err)
		}
	}
}
