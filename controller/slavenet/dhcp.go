package slavenet

import (
	"context"
	"log/slog"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

// ServeDHCP answers DHCP on the Slave network's interface until ctx ends.
// Binding port 67 to the interface needs CAP_NET_BIND_SERVICE and CAP_NET_RAW.
func (n *Net) ServeDHCP(ctx context.Context, leases Leases, log *slog.Logger) error {
	handler := func(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
		resp, err := n.Reply(req, leases)
		if err != nil {
			log.Warn("dhcp reply", "mac", req.ClientHWAddr, "err", err)
			return
		}
		if resp == nil {
			return
		}
		dst := replyAddr(req, peer)
		if _, err := conn.WriteTo(resp.ToBytes(), dst); err != nil {
			log.Warn("dhcp send", "mac", req.ClientHWAddr, "to", dst, "err", err)
			return
		}
		log.Debug("dhcp", "type", resp.MessageType(), "mac", req.ClientHWAddr, "ip", resp.YourIPAddr)
	}
	srv, err := server4.NewServer(n.Iface, &net.UDPAddr{IP: net.IPv4zero, Port: dhcpv4.ServerPort}, handler)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// replyAddr follows RFC 2131 §4.1: relays get the reply on port 67, a
// client that already has an address gets it unicast, everyone else by
// broadcast (it can't receive unicast before it has an address).
func replyAddr(req *dhcpv4.DHCPv4, peer net.Addr) net.Addr {
	switch {
	case req.GatewayIPAddr != nil && !req.GatewayIPAddr.IsUnspecified():
		return &net.UDPAddr{IP: req.GatewayIPAddr, Port: dhcpv4.ServerPort}
	case req.ClientIPAddr != nil && !req.ClientIPAddr.IsUnspecified() && !req.IsBroadcast():
		return &net.UDPAddr{IP: req.ClientIPAddr, Port: dhcpv4.ClientPort}
	default:
		return &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort}
	}
}
