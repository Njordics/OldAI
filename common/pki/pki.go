// Package pki implements the cluster's small internal CA and the identity
// rules from docs/protocol.md §1: peers are identified by a URI SAN
// (aicluster://node/<id> or aicluster://controller/<cluster_id>) chained to a
// pinned CA, never by hostname or IP.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	NodeCertLifetime       = 30 * 24 * time.Hour
	ControllerCertLifetime = 365 * 24 * time.Hour
	CALifetime             = 20 * 365 * 24 * time.Hour
	clockSkew              = 5 * time.Minute
)

func NodeURI(nodeID string) string          { return "aicluster://node/" + nodeID }
func ControllerURI(clusterID string) string { return "aicluster://controller/" + clusterID }

// Fingerprint returns "sha256:<hex>" of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CA is the cluster certificate authority held by the Controller.
type CA struct {
	Cert    *x509.Certificate
	Key     crypto.Signer
	CertPEM []byte
}

// NewCA creates a fresh ECDSA P-256 CA for clusterID.
func NewCA(clusterID string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: "aicluster CA " + clusterID, Organization: []string{"aicluster"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,

		// Admins install this CA in their browsers to use the dashboard, so
		// it must not be able to vouch for anything outside the LAN. Only DNS
		// and IP constraints: Chrome rejects a critical name-constraints
		// extension containing URI constraints, and browsers never use URI
		// SANs for site identity.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         PermittedDNSDomains,
		PermittedIPRanges:           permittedIPRanges(),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{Cert: cert, Key: key, CertPEM: pemBlock("CERTIFICATE", der)}, nil
}

// PermittedDNSDomains are the only DNS names the cluster CA may sign:
// mDNS names, localhost and the reserved home/internal suffixes.
var PermittedDNSDomains = []string{"local", "localhost", "home.arpa", "internal", "lan"}

// PermittedIPNets are the only addresses the cluster CA may sign: loopback,
// RFC 1918, CGNAT (100.64/10, used by some VPNs) and IPv6 ULA/loopback.
var PermittedIPNets = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7", "::1/128"}

func permittedIPRanges() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range PermittedIPNets {
		_, n, _ := net.ParseCIDR(s)
		out = append(out, n)
	}
	return out
}

// ControllerSANs keeps only the names and addresses the CA is allowed to
// sign; anything else would make the certificate fail validation.
func ControllerSANs(hostname string, ips []string) (dns []string, addrs []net.IP) {
	dns = []string{"localhost"}
	if h := strings.ToLower(strings.TrimSuffix(hostname, ".")); h != "" && h != "localhost" {
		if !strings.Contains(h, ".") {
			h += ".local"
		}
		if inPermittedDomain(h) {
			dns = append(dns, h)
		}
	}
	nets := permittedIPRanges()
	for _, s := range ips {
		ip := net.ParseIP(s)
		for _, n := range nets {
			if ip != nil && n.Contains(ip) {
				addrs = append(addrs, ip)
				break
			}
		}
	}
	return dns, addrs
}

func inPermittedDomain(h string) bool {
	for _, d := range PermittedDNSDomains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// Save writes ca.crt and ca.key (0600) into dir.
func (ca *CA) Save(dir string) error {
	keyPEM, err := EncodeKeyPEM(ca.Key)
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(dir, "ca.key"), keyPEM, 0o600); err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, "ca.crt"), ca.CertPEM, 0o644)
}

// LoadCA reads ca.crt and ca.key from dir.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, err
	}
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	key, err := ParseKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

// Pool returns a CertPool holding only this CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// IssueController issues the Controller's own server+client certificate.
// dnsNames and ips are added for convenience (browsers, curl); peers in the
// cluster check only the URI SAN.
func (ca *CA) IssueController(clusterID string, dnsNames []string, ips []net.IP) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	u, _ := url.Parse(ControllerURI(clusterID))
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: "aicluster controller", Organization: []string{"aicluster"}},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(ControllerCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, _ := EncodeKeyPEM(key)
	return tls.X509KeyPair(pemBlock("CERTIFICATE", der), keyPEM)
}

// SignNodeCSR verifies a Slave CSR and issues its node certificate. The CSR's
// subject is ignored; identity comes only from nodeID.
func (ca *CA) SignNodeCSR(csrPEM []byte, nodeID string, lifetime time.Duration) (certPEM []byte, serial string, err error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, "", errors.New("pki: CSR is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("pki: CSR signature: %w", err)
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, "", errors.New("pki: node key must be ECDSA")
	}
	u, _ := url.Parse(NodeURI(nodeID))
	now := time.Now()
	sn := randSerial()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"aicluster node"}},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, "", err
	}
	return pemBlock("CERTIFICATE", der), SerialString(sn), nil
}

// NewBootstrapCert makes the Slave's pre-adoption Ed25519 self-signed cert.
func NewBootstrapCert(nodeID string) (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"aicluster bootstrap"}},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(CALifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = EncodeKeyPEM(priv)
	return pemBlock("CERTIFICATE", der), keyPEM, err
}

// NewNodeKeyAndCSR generates the Slave's ECDSA node key and a CSR for it.
func NewNodeKeyAndCSR(nodeID string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nodeID},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = EncodeKeyPEM(key)
	return keyPEM, pemBlock("CERTIFICATE REQUEST", der), err
}

// VerifyPeer checks that rawCerts chain to roots and that the leaf carries
// wantURI. It is used from tls.Config.VerifyPeerCertificate on both sides.
func VerifyPeer(rawCerts [][]byte, roots *x509.CertPool, wantURI string, usage x509.ExtKeyUsage) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("pki: no peer certificate")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{usage},
	}); err != nil {
		return nil, err
	}
	if wantURI != "" && !HasURI(certs[0], wantURI) {
		return nil, fmt.Errorf("pki: peer is not %s", wantURI)
	}
	return certs[0], nil
}

// HasURI reports whether cert carries the URI SAN u.
func HasURI(cert *x509.Certificate, u string) bool {
	for _, have := range cert.URIs {
		if have.String() == u {
			return true
		}
	}
	return false
}

// NodeIDFromCert extracts <id> from an aicluster://node/<id> URI SAN.
func NodeIDFromCert(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u.Scheme == "aicluster" && u.Host == "node" && len(u.Path) > 1 {
			return u.Path[1:]
		}
	}
	return ""
}

// ClusterIDFromCert extracts <id> from an aicluster://controller/<id> URI SAN.
func ClusterIDFromCert(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u.Scheme == "aicluster" && u.Host == "controller" && len(u.Path) > 1 {
			return u.Path[1:]
		}
	}
	return ""
}

func SerialString(n *big.Int) string { return hex.EncodeToString(n.Bytes()) }

func randSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n.Add(n, big.NewInt(1))
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func EncodeKeyPEM(key crypto.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pemBlock("PRIVATE KEY", der), nil
}

func ParseKeyPEM(b []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("pki: key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("pki: key is not a signer")
	}
	return s, nil
}

func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("pki: certificate is not PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// PoolFromPEM builds a CertPool from one or more PEM certificates.
func PoolFromPEM(b []byte) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, errors.New("pki: no certificates in PEM")
	}
	return p, nil
}

// WriteFileAtomic writes via temp file + fsync + rename so a crash never
// leaves a half-written key or cert behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Chmod is a no-op on Windows dev machines; on Linux it cannot fail for a
	// file we just created.
	_ = tmp.Chmod(perm)
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
