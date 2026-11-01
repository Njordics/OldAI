package slavenet

import (
	"net"
	"net/netip"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/miekg/dns"
)

func TestChoose(t *testing.T) {
	home := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	if p, err := Choose("auto", "", home); err != nil || p.String() != "10.10.10.0/24" {
		t.Fatalf("auto = %v %v", p, err)
	}
	// A home network on 10.10.0.0/16 rules out the first two candidates.
	if p, _ := Choose("auto", "", []netip.Prefix{netip.MustParsePrefix("10.10.0.0/16")}); p.String() != "10.20.30.0/24" {
		t.Fatalf("collision skip = %v", p)
	}
	// The stored choice wins while it still fits, so node addresses persist.
	if p, _ := Choose("auto", "172.31.250.0/24", home); p.String() != "172.31.250.0/24" {
		t.Fatalf("stored = %v", p)
	}
	if p, _ := Choose("auto", "192.168.1.0/24", home); p.String() != "10.10.10.0/24" {
		t.Fatalf("stored but colliding = %v", p)
	}
	if _, err := Choose("192.168.1.0/24", "", home); err == nil {
		t.Fatal("explicit subnet overlapping the LAN accepted")
	}
	if _, err := Choose("10.0.0.0/8", "", home); err == nil {
		t.Fatal("a /8 accepted")
	}
}

func TestAllocate(t *testing.T) {
	p := netip.MustParsePrefix("10.10.10.0/24")
	if ControllerAddr(p).String() != "10.10.10.1" {
		t.Fatal(ControllerAddr(p))
	}
	used := map[netip.Addr]bool{netip.MustParseAddr("10.10.10.10"): true}
	if a, _ := Allocate(p, used); a.String() != "10.10.10.11" {
		t.Fatalf("allocate = %v", a)
	}
	small := netip.MustParsePrefix("10.10.10.0/28") // hosts .10-.14 usable
	used = map[netip.Addr]bool{}
	for i := 0; i < 5; i++ {
		a, err := Allocate(small, used)
		if err != nil {
			t.Fatalf("host %d: %v", i, err)
		}
		used[a] = true
	}
	if a, err := Allocate(small, used); err == nil {
		t.Fatalf("allocated %v past the end (broadcast is .15)", a)
	}
}

type fakeLeases map[string]Lease

func (f fakeLeases) ByMAC(mac string) (Lease, bool) { l, ok := f[mac]; return l, ok }
func (f fakeLeases) ByName(name string) (Lease, bool) {
	for _, l := range f {
		if l.Name == name {
			return l, true
		}
	}
	return Lease{}, false
}

func testNet() (*Net, fakeLeases) {
	n := &Net{Iface: "eth0", Subnet: netip.MustParsePrefix("10.10.10.0/24"), ControllerIP: netip.MustParseAddr("10.10.10.1"), Domain: "aicluster"}
	return n, fakeLeases{"52:54:00:77:00:21": {MAC: "52:54:00:77:00:21", IP: netip.MustParseAddr("10.10.10.10"), Name: "node-a"}}
}

func discover(t *testing.T, mac string) *dhcpv4.DHCPv4 {
	hw, _ := net.ParseMAC(mac)
	d, err := dhcpv4.NewDiscovery(hw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDHCPAnswersOnlyReservedMACs(t *testing.T) {
	n, leases := testNet()
	if r, _ := n.Reply(discover(t, "aa:bb:cc:dd:ee:ff"), leases); r != nil {
		t.Fatal("answered a MAC without a reservation (would fight the home router)")
	}
	offer, err := n.Reply(discover(t, "52:54:00:77:00:21"), leases)
	if err != nil || offer == nil {
		t.Fatalf("offer: %v %v", offer, err)
	}
	if offer.MessageType() != dhcpv4.MessageTypeOffer || !offer.YourIPAddr.Equal(net.ParseIP("10.10.10.10")) {
		t.Fatalf("offer = %s", offer.Summary())
	}
	if !offer.ServerIdentifier().Equal(net.ParseIP("10.10.10.1")) || len(offer.Router()) != 1 || !offer.Router()[0].Equal(net.ParseIP("10.10.10.1")) {
		t.Fatalf("server id %v router %v", offer.ServerIdentifier(), offer.Router())
	}
	if dnsIPs := offer.DNS(); len(dnsIPs) != 1 || !dnsIPs[0].Equal(net.ParseIP("10.10.10.1")) {
		t.Fatalf("dns = %v", dnsIPs)
	}
	if offer.SubnetMask().String() != net.CIDRMask(24, 32).String() || offer.HostName() != "node-a" {
		t.Fatalf("mask %v host %q", offer.SubnetMask(), offer.HostName())
	}

	req, _ := dhcpv4.NewRequestFromOffer(offer)
	ack, _ := n.Reply(req, leases)
	if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("request for our offer: %v", ack)
	}

	// A REQUEST addressed to the home router's offer stays the router's.
	homeOffer, _ := dhcpv4.NewReplyFromRequest(discover(t, "52:54:00:77:00:21"),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer), dhcpv4.WithYourIP(net.ParseIP("192.168.1.50")),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("192.168.1.1"))))
	homeReq, _ := dhcpv4.NewRequestFromOffer(homeOffer)
	if r, _ := n.Reply(homeReq, leases); r != nil {
		t.Fatalf("answered a request meant for another server: %s", r.Summary())
	}

	// Renewing an address we didn't reserve gets a NAK.
	hw, _ := net.ParseMAC("52:54:00:77:00:21")
	renew, _ := dhcpv4.New(dhcpv4.WithHwAddr(hw), dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithClientIP(net.ParseIP("10.10.10.99")))
	if r, _ := n.Reply(renew, leases); r == nil || r.MessageType() != dhcpv4.MessageTypeNak {
		t.Fatalf("renewing a foreign address: %v", r)
	}
}

func TestReplyAddr(t *testing.T) {
	d := discover(t, "52:54:00:77:00:21")
	if a := replyAddr(d, nil).(*net.UDPAddr); !a.IP.Equal(net.IPv4bcast) || a.Port != 68 {
		t.Fatalf("new client = %v", a)
	}
	d.ClientIPAddr = net.ParseIP("10.10.10.10")
	d.Flags = 0
	if a := replyAddr(d, nil).(*net.UDPAddr); !a.IP.Equal(net.ParseIP("10.10.10.10")) {
		t.Fatalf("renewing client = %v", a)
	}
}

func TestDNS(t *testing.T) {
	n, leases := testNet()
	ask := func(name string) *dns.Msg {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		return n.Answer(q, leases)
	}
	if r := ask("node-a.aicluster."); r.Rcode != dns.RcodeSuccess || r.Answer[0].(*dns.A).A.String() != "10.10.10.10" {
		t.Fatalf("node-a = %v", r)
	}
	if r := ask("Controller.AICluster."); r.Answer[0].(*dns.A).A.String() != "10.10.10.1" {
		t.Fatalf("controller = %v", r)
	}
	if r := ask("nope.aicluster."); r.Rcode != dns.RcodeNameError {
		t.Fatalf("unknown node = %v", r.Rcode)
	}
	if r := ask("example.com."); r.Rcode != dns.RcodeRefused || len(r.Answer) != 0 {
		t.Fatalf("outside the zone = %v", r.Rcode)
	}
}
