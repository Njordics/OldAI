package core

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/slavenet"
	"aicluster/controller/store"
)

const slaveDomain = "aicluster"

// setupSlaveNet picks the Slave subnet and adds the Controller's address on
// it (§12). Runs before the node API listens so the address is advertised.
// Failure leaves the cluster on the home network only.
func (c *Core) setupSlaveNet(ctx context.Context) error {
	mode := c.cfg.SlaveNet
	if mode == "" || mode == "off" {
		return nil
	}
	iface := c.cfg.SlaveIface
	if iface == "" {
		var err error
		if iface, err = slavenet.DefaultRouteIface(); err != nil {
			return err
		}
	}
	stored, err := c.Store.Setting(ctx, "slave_net", "")
	if err != nil {
		return err
	}
	var own netip.Prefix // our own address from last time isn't a collision
	if p, err := netip.ParsePrefix(stored); err == nil {
		own = p
	}
	subnet, err := slavenet.Choose(mode, stored, slavenet.LocalPrefixes(own))
	if err != nil {
		return err
	}
	if subnet.String() != stored {
		// New subnet: old reservations point into the old one.
		if err := c.Store.ClearSlaveLeases(ctx); err != nil {
			return err
		}
		if err := c.Store.SetSetting(ctx, "slave_net", subnet.String()); err != nil {
			return err
		}
	}
	ctrl := slavenet.ControllerAddr(subnet)
	if err := slavenet.EnsureAddress(iface, netip.PrefixFrom(ctrl, subnet.Bits())); err != nil {
		return err
	}
	c.slaveNet = &slavenet.Net{Iface: iface, Subnet: subnet, ControllerIP: ctrl, Domain: slaveDomain}
	c.log.Info("slave network ready", "subnet", subnet, "controller", ctrl, "iface", iface)
	return nil
}

// SlaveNet describes the running Slave network, or nil.
func (c *Core) SlaveNet() *slavenet.Net { return c.slaveNet }

func (c *Core) serveSlaveNet(ctx context.Context) {
	n := c.slaveNet
	log := c.log.With("component", "slavenet")
	leases := coreLeases{c}
	go func() {
		if err := n.ServeDHCP(ctx, leases, log); err != nil {
			log.Error("dhcp server stopped", "err", err)
		}
	}()
	go func() {
		if err := n.ServeDNS(ctx, leases); err != nil {
			log.Error("dns server stopped", "err", err)
		}
	}()
	// The address isn't in the OS network config, so networkd drops it when
	// it reconfigures the link (cable replugged, ...). Put it back.
	go func() {
		addr := netip.PrefixFrom(n.ControllerIP, n.Subnet.Bits())
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if !slavenet.HasAddress(n.Iface, n.ControllerIP) {
				log.Warn("slave network address was removed; adding it back", "iface", n.Iface, "addr", addr)
				if err := slavenet.EnsureAddress(n.Iface, addr); err != nil {
					log.Error("slave network address", "err", err)
				}
			}
		}
	}()
}

// noteSlaveMAC records the interface a node reported and reserves it an
// address; a new or changed reservation goes out in its desired state.
func (c *Core) noteSlaveMAC(ctx context.Context, nodeID, mac string) {
	n := c.slaveNet
	mac = strings.ToLower(mac)
	if n == nil || mac == "" {
		return
	}
	l, err := c.Store.SlaveLease(ctx, nodeID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ip, err := c.allocateSlaveIP(ctx)
		if err != nil {
			c.log.Error("slave network", "node_id", nodeID, "err", err)
			return
		}
		l = &store.SlaveLease{NodeID: nodeID, IP: ip.String()}
	case err != nil:
		c.log.Error("slave lease", "err", err)
		return
	case l.MAC == mac:
		return
	}
	l.MAC = mac
	if err := c.Store.PutSlaveLease(ctx, *l); err != nil {
		c.log.Error("slave lease", "err", err)
		return
	}
	c.log.Info("slave network address reserved", "node_id", nodeID, "ip", l.IP, "mac", mac)
	go c.SyncDesired(context.Background(), nodeID)
}

func (c *Core) allocateSlaveIP(ctx context.Context) (netip.Addr, error) {
	leases, err := c.Store.ListSlaveLeases(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	used := map[netip.Addr]bool{}
	for _, l := range leases {
		if a, err := netip.ParseAddr(l.IP); err == nil {
			used[a] = true
		}
	}
	return slavenet.Allocate(c.slaveNet.Subnet, used)
}

// slaveNetworkFor is the node's network section of its desired state.
func (c *Core) slaveNetworkFor(ctx context.Context, nodeID string) *proto.SlaveNetwork {
	n := c.slaveNet
	if n == nil {
		return nil
	}
	l, err := c.Store.SlaveLease(ctx, nodeID)
	if err != nil || l.MAC == "" {
		return nil
	}
	return &proto.SlaveNetwork{
		Subnet: n.Subnet.String(), ControllerIP: n.ControllerIP.String(), Address: l.IP, MAC: l.MAC,
		Domain: n.Domain, NodeAPIPort: c.nodeAPIPort(),
	}
}

func (c *Core) nodeAPIPort() int {
	if c.nodeLn == nil {
		return 0
	}
	return c.nodeLn.Addr().(*net.TCPAddr).Port
}

// DNSLabel turns a node name into a host name ("WezCube 2" -> "wezcube-2").
func DNSLabel(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// coreLeases serves DHCP and DNS from the store.
type coreLeases struct{ c *Core }

func (l coreLeases) all() []slavenet.Lease {
	ctx := context.Background()
	leases, err := l.c.Store.ListSlaveLeases(ctx)
	if err != nil {
		return nil
	}
	names := map[string]string{}
	if nodes, err := l.c.Store.ListNodes(ctx); err == nil {
		for _, n := range nodes {
			names[n.NodeID] = DNSLabel(n.Name)
		}
	}
	out := make([]slavenet.Lease, 0, len(leases))
	for _, s := range leases {
		ip, err := netip.ParseAddr(s.IP)
		if err != nil || s.MAC == "" {
			continue
		}
		out = append(out, slavenet.Lease{MAC: s.MAC, IP: ip, Name: names[s.NodeID]})
	}
	return out
}

func (l coreLeases) ByMAC(mac string) (slavenet.Lease, bool) {
	for _, x := range l.all() {
		if x.MAC == mac {
			return x, true
		}
	}
	return slavenet.Lease{}, false
}

func (l coreLeases) ByName(name string) (slavenet.Lease, bool) {
	for _, x := range l.all() {
		if x.Name != "" && x.Name == name {
			return x, true
		}
	}
	return slavenet.Lease{}, false
}
