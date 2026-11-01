package pki

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func TestNodeCertVerify(t *testing.T) {
	ca, err := NewCA("cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	_, csr, err := NewNodeKeyAndCSR("node-1")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, err := ca.SignNodeCSR(csr, "node-1", time.Hour)
	if err != nil || serial == "" {
		t.Fatal(err)
	}
	cert, _ := ParseCertPEM(certPEM)
	raw := [][]byte{cert.Raw}

	if _, err := VerifyPeer(raw, ca.Pool(), NodeURI("node-1"), x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("valid cert rejected: %v", err)
	}
	if _, err := VerifyPeer(raw, ca.Pool(), NodeURI("node-2"), x509.ExtKeyUsageClientAuth); err == nil {
		t.Fatal("cert accepted for another node")
	}
	other, _ := NewCA("cluster-b")
	if _, err := VerifyPeer(raw, other.Pool(), NodeURI("node-1"), x509.ExtKeyUsageClientAuth); err == nil {
		t.Fatal("cert accepted by another cluster's CA")
	}
	if got := NodeIDFromCert(cert); got != "node-1" {
		t.Fatalf("NodeIDFromCert = %q", got)
	}
}

func TestControllerCertRole(t *testing.T) {
	ca, _ := NewCA("cluster-a")
	tc, err := ca.IssueController("cluster-a", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPeer(tc.Certificate, ca.Pool(), ControllerURI("cluster-a"), x509.ExtKeyUsageServerAuth); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPeer(tc.Certificate, ca.Pool(), NodeURI("cluster-a"), x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("controller cert accepted as a node")
	}
}

// The CA is installed in admins' browsers, so it must be useless for
// impersonating anything outside the LAN.
func TestCANameConstraints(t *testing.T) {
	ca, _ := NewCA("c")
	verify := func(dns []string, ips []net.IP, name string) error {
		tc, err := ca.IssueController("c", dns, ips)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(tc.Certificate[0])
		_, err = leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: name})
		return err
	}
	if err := verify([]string{"aic.local", "localhost"}, []net.IP{net.ParseIP("192.168.1.5")}, "aic.local"); err != nil {
		t.Fatalf("LAN name rejected: %v", err)
	}
	if err := verify([]string{"aic.local"}, []net.IP{net.ParseIP("192.168.1.5")}, "192.168.1.5"); err != nil {
		t.Fatalf("private IP rejected: %v", err)
	}
	if err := verify([]string{"www.google.com"}, nil, "www.google.com"); err == nil {
		t.Fatal("CA signed a public DNS name")
	}
	if err := verify(nil, []net.IP{net.ParseIP("8.8.8.8")}, "8.8.8.8"); err == nil {
		t.Fatal("CA signed a public IP")
	}
}

// Browsers reject a critical name-constraints extension with name types they
// don't support (Chrome: URI), so only DNS and IP constraints may appear.
func TestCAConstraintsBrowserCompatible(t *testing.T) {
	ca, _ := NewCA("c")
	c := ca.Cert
	if len(c.PermittedURIDomains)+len(c.ExcludedURIDomains)+len(c.PermittedEmailAddresses)+len(c.ExcludedEmailAddresses) != 0 {
		t.Fatal("CA carries name-constraint types browsers don't support")
	}
	if !c.PermittedDNSDomainsCritical || len(c.PermittedDNSDomains) == 0 || len(c.PermittedIPRanges) == 0 {
		t.Fatal("CA must carry critical DNS and IP constraints")
	}
}

func TestControllerSANsFiltersPublic(t *testing.T) {
	dns, ips := ControllerSANs("aic-controller", []string{"192.168.1.10", "8.8.8.8", "127.0.0.1"})
	if len(dns) != 2 || dns[1] != "aic-controller.local" {
		t.Fatalf("dns = %v", dns)
	}
	if len(ips) != 2 {
		t.Fatalf("ips = %v", ips)
	}
}

func TestCARoundTrip(t *testing.T) {
	dir := t.TempDir()
	ca, _ := NewCA("c")
	if err := ca.Save(dir); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCA(dir)
	if err != nil || !back.Cert.Equal(ca.Cert) {
		t.Fatalf("load: %v", err)
	}
}
