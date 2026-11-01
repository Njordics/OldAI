// Package slavenet runs the Controller's own subnet for Slaves (protocol
// §12). It shares the wire with the home LAN: the Controller adds a second
// address to its LAN interface and is the only DHCP and DNS server for that
// subnet. DHCP answers only MACs it has a reservation for, so the home
// network's devices (and its own DHCP server) are never affected.
package slavenet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/miekg/dns"
)

// Candidates are tried in order when the subnet is chosen automatically;
// the first that overlaps none of the Controller's networks wins.
var Candidates = []string{"10.10.10.0/24", "10.10.20.0/24", "10.20.30.0/24", "172.31.250.0/24", "192.168.250.0/24"}

const (
	LeaseTime = 12 * time.Hour
	firstHost = 10 // .1 is the Controller, .2-.9 stay free
)

// Net is a running Slave network.
type Net struct {
	Iface        string
	Subnet       netip.Prefix
	ControllerIP netip.Addr
	Domain       string
}

// Lease is one reservation.
type Lease struct {
	MAC  string // lower-case, colon-separated
	IP   netip.Addr
	Name string // DNS name inside Domain
}

// Leases is what DHCP and DNS look reservations up in.
type Leases interface {
	ByMAC(mac string) (Lease, bool)
	ByName(name string) (Lease, bool)
}

// Choose picks the subnet. mode is "auto" or a CIDR; stored is the subnet
// chosen earlier (kept so node addresses survive restarts) unless it now
// collides with a local network.
func Choose(mode, stored string, local []netip.Prefix) (netip.Prefix, error) {
	collides := func(p netip.Prefix) bool {
		for _, l := range local {
			if l.Overlaps(p) {
				return true
			}
		}
		return false
	}
	parse := func(s string) (netip.Prefix, error) {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p.Bits() > 28 || p.Bits() < 16 {
			return netip.Prefix{}, fmt.Errorf("slave network %q: need an IPv4 subnet between /16 and /28", s)
		}
		return p.Masked(), nil
	}
	if mode != "auto" {
		p, err := parse(mode)
		if err != nil {
			return p, err
		}
		if collides(p) {
			return p, fmt.Errorf("slave network %s overlaps a network the Controller is already on", p)
		}
		return p, nil
	}
	if stored != "" {
		if p, err := parse(stored); err == nil && !collides(p) {
			return p, nil
		}
	}
	for _, c := range Candidates {
		p, _ := parse(c)
		if !collides(p) {
			return p, nil
		}
	}
	return netip.Prefix{}, errors.New("every candidate slave network overlaps a local network; set one with --slave-net")
}

// ControllerAddr is the subnet's first host address.
func ControllerAddr(p netip.Prefix) netip.Addr { return p.Masked().Addr().Next() }

// Allocate returns the lowest free host address from .10 up.
func Allocate(p netip.Prefix, used map[netip.Addr]bool) (netip.Addr, error) {
	a := p.Masked().Addr()
	for i := 0; i < firstHost; i++ {
		a = a.Next()
	}
	for ; p.Contains(a); a = a.Next() {
		if !p.Contains(a.Next()) { // broadcast address
			break
		}
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("slave network %s is full", p)
}

func mask(p netip.Prefix) net.IPMask { return net.CIDRMask(p.Bits(), 32) }

// Reply answers one DHCP request, or returns nil to stay silent: unknown
// MACs are ignored so the home network's DHCP server keeps serving them.
func (n *Net) Reply(req *dhcpv4.DHCPv4, leases Leases) (*dhcpv4.DHCPv4, error) {
	if req.OpCode != dhcpv4.OpcodeBootRequest {
		return nil, nil
	}
	lease, ok := leases.ByMAC(strings.ToLower(req.ClientHWAddr.String()))
	if !ok {
		return nil, nil
	}
	ctrl := net.IP(n.ControllerIP.AsSlice())
	yours := net.IP(lease.IP.AsSlice())
	var msgType dhcpv4.MessageType
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		msgType = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		// A REQUEST aimed at another server (the home router) is not ours.
		if sid := req.ServerIdentifier(); sid != nil && !sid.Equal(ctrl) {
			return nil, nil
		}
		want := req.RequestedIPAddress()
		if want == nil || want.IsUnspecified() {
			want = req.ClientIPAddr
		}
		if want != nil && !want.IsUnspecified() && !want.Equal(yours) {
			return dhcpv4.NewReplyFromRequest(req,
				dhcpv4.WithMessageType(dhcpv4.MessageTypeNak),
				dhcpv4.WithOption(dhcpv4.OptServerIdentifier(ctrl)))
		}
		msgType = dhcpv4.MessageTypeAck
	default:
		return nil, nil
	}
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(msgType),
		dhcpv4.WithYourIP(yours),
		dhcpv4.WithServerIP(ctrl),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(ctrl)),
		dhcpv4.WithNetmask(mask(n.Subnet)),
		dhcpv4.WithDNS(ctrl),
		dhcpv4.WithLeaseTime(uint32(LeaseTime / time.Second)),
		dhcpv4.WithOption(dhcpv4.OptDomainName(n.Domain)),
		// The Controller as gateway gives Slaves a default route, which
		// beacon broadcasts need. It is no way out: the Controller doesn't
		// forward packets (ip_forward stays off), so Slaves still reach only it.
		dhcpv4.WithRouter(ctrl),
	}
	if lease.Name != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(lease.Name)))
	}
	return dhcpv4.NewReplyFromRequest(req, mods...)
}

// Answer resolves names in the Slave zone: the Controller ("controller"),
// and every reserved node by name. Everything else is refused: Slaves have
// no internet access, so there is nothing else to resolve.
func (n *Net) Answer(req *dns.Msg, leases Leases) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	if len(req.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		return m
	}
	q := req.Question[0]
	zone := dns.Fqdn(n.Domain)
	name := strings.ToLower(q.Name)
	if !dns.IsSubDomain(zone, name) {
		m.Authoritative = false
		m.Rcode = dns.RcodeRefused
		return m
	}
	host := strings.TrimSuffix(strings.TrimSuffix(name, zone), ".")
	var ip netip.Addr
	switch host {
	case "controller", "":
		ip = n.ControllerIP
	default:
		l, ok := leases.ByName(host)
		if !ok {
			m.Rcode = dns.RcodeNameError
			return m
		}
		ip = l.IP
	}
	if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.IP(ip.AsSlice()),
		})
	}
	return m
}

// ServeDNS answers on the Controller's Slave address until ctx ends.
func (n *Net) ServeDNS(ctx context.Context, leases Leases) error {
	addr := net.JoinHostPort(n.ControllerIP.String(), "53")
	mux := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) { _ = w.WriteMsg(n.Answer(r, leases)) })
	udp := &dns.Server{Addr: addr, Net: "udp", Handler: mux}
	tcp := &dns.Server{Addr: addr, Net: "tcp", Handler: mux}
	errc := make(chan error, 2)
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	select {
	case <-ctx.Done():
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return nil
	case err := <-errc:
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return err
	}
}

// LocalPrefixes lists the IPv4 networks of every interface except the
// Slave network itself, for collision checks.
func LocalPrefixes(skip netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		ip, _ := netip.AddrFromSlice(ipn.IP.To4())
		ones, _ := ipn.Mask.Size()
		p := netip.PrefixFrom(ip, ones).Masked()
		if skip.IsValid() && skip.Contains(ip) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// DefaultRouteIface is the interface of the IPv4 default route (Linux).
func DefaultRouteIface() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 2 && fields[1] == "00000000" && fields[0] != "Iface" {
			return fields[0], nil
		}
	}
	return "", errors.New("no IPv4 default route; set --slave-iface")
}

// HasAddress reports whether iface holds ip.
func HasAddress(iface string, ip netip.Addr) bool {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return false
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(net.IP(ip.AsSlice())) {
			return true
		}
	}
	return false
}

// EnsureAddress adds addr to iface (idempotent). Needs CAP_NET_ADMIN.
func EnsureAddress(iface string, addr netip.Prefix) error {
	out, err := exec.Command("ip", "-4", "addr", "replace", addr.String(), "dev", iface).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr replace %s dev %s: %v: %s", addr, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}
