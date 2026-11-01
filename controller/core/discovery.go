package core

import (
	"context"
	"errors"
	"net"
	"time"

	"aicluster/common/discovery"
	"aicluster/common/proto"
	"aicluster/controller/store"
)

// Sighting is one discovery observation of a Slave (§2.3).
type Sighting struct {
	NodeID          string
	Addr            string
	Port            int
	FP              string
	State           string
	ClusterID       string
	HW              string
	PairingRequired bool
	AgentVersion    string
	Source          string // "mdns" | "beacon"
}

func (c *Core) discoveryLoop(ctx context.Context) {
	if c.cfg.MDNS {
		go discovery.Browse(ctx, proto.SlaveMDNSService, c.log, func(e discovery.MDNSEntry) {
			if e.TXT["v"] != "1" || e.TXT["id"] == "" || len(e.IPs) == 0 {
				return
			}
			c.OnSighting(ctx, Sighting{
				NodeID: e.TXT["id"], Addr: e.IPs[0].String(), Port: e.Port, FP: e.TXT["fp"],
				State: e.TXT["st"], ClusterID: e.TXT["cl"], HW: e.TXT["hw"],
				PairingRequired: e.TXT["pc"] == "1", AgentVersion: e.TXT["ag"], Source: "mdns",
			})
		})
	}
	if c.cfg.BeaconListen != "" {
		err := discovery.ListenBeacons(ctx, c.cfg.BeaconListen, c.log, func(b proto.Beacon, from *net.UDPAddr) {
			if b.Kind != proto.BeaconKindSlave || b.NodeID == "" {
				return
			}
			c.OnSighting(ctx, Sighting{
				// The datagram source is where the node really is; Addrs is self-reported.
				NodeID: b.NodeID, Addr: from.IP.String(), Port: b.Port, FP: b.FP,
				State: b.State, ClusterID: b.ClusterID, HW: b.HW,
				PairingRequired: b.PairingRequired, AgentVersion: b.AgentVersion, Source: "beacon",
			})
		})
		if err != nil {
			c.log.Error("beacon listener failed", "err", err)
		}
	}
	<-ctx.Done()
}

// OnSighting records what discovery saw. It only updates the registry; the
// monitor loop decides what to do (adopt, mark pending, ...).
func (c *Core) OnSighting(ctx context.Context, s Sighting) {
	now := time.Now().UTC()
	n, err := c.Store.GetNode(ctx, s.NodeID)
	if errors.Is(err, store.ErrNotFound) {
		n = &store.Node{NodeID: s.NodeID, State: store.StateDiscovered, FirstSeen: now, StateSince: now}
		c.log.Info("node discovered", "node_id", s.NodeID, "addr", s.Addr, "source", s.Source, "hw", s.HW)
	} else if err != nil {
		c.log.Error("load node", "err", err)
		return
	}

	n.Addr, n.Port, n.FP = s.Addr, s.Port, s.FP
	n.AgentState, n.ClusterID = s.State, s.ClusterID
	n.HWSummary, n.PairingRequired, n.AgentVersion = s.HW, s.PairingRequired, s.AgentVersion
	n.LastSeen = now

	ours := s.ClusterID == c.ClusterID
	switch {
	case s.State == proto.AgentAdopted && ours:
		// Ours; heartbeats drive its state. A node we have no record of (e.g.
		// restored DB) shows as offline until its first heartbeat.
		if n.State == store.StateDiscovered {
			setState(n, store.StateOffline, now)
		}
	case s.State == proto.AgentAdopted:
		if n.State != store.StateReleased {
			setState(n, store.StateForeign, now)
		}
	default: // unadopted or enrolling
		switch n.State {
		case store.StateAdopted, store.StateOffline:
			// Our node lost its identity (factory reset, wiped disk, expired
			// cert). Its old certificate is no longer in use.
			c.log.Warn("adopted node reappeared unadopted", "node_id", n.NodeID)
			_ = c.Store.Revoke(ctx, n.CertSerial, n.NodeID)
			n.CertSerial, n.Returning, n.Inventory = "", true, nil
			setState(n, store.StateDiscovered, now)
		case store.StateForeign:
			setState(n, store.StateDiscovered, now)
		}
	}
	if err := c.Store.PutNode(ctx, n); err != nil {
		c.log.Error("save node", "err", err)
	}
}

func setState(n *store.Node, state string, now time.Time) {
	if n.State != state {
		n.State, n.StateSince = state, now
	}
}

// announceLoop advertises the Controller so adopted Slaves can find it after
// an address change (§2.1, §2.2).
func (c *Core) announceLoop(ctx context.Context) {
	port := c.nodeLn.Addr().(*net.TCPAddr).Port
	if c.cfg.MDNS {
		ann, err := discovery.Announce("aicluster-controller", proto.ControllerMDNSService, port,
			map[string]string{"v": "1", "cl": c.ClusterID, "fp": c.CertFP()})
		if err != nil {
			c.log.Error("mdns announce disabled", "err", err)
		} else {
			defer ann.Close()
		}
	}
	sender, err := discovery.NewBeaconSender(c.cfg.BeaconPort, c.cfg.BeaconTargets)
	if err != nil {
		c.log.Error("controller beacon disabled", "err", err)
		<-ctx.Done()
		return
	}
	defer sender.Close()
	for {
		_ = sender.Send(proto.Beacon{
			Kind: proto.BeaconKindController, ClusterID: c.ClusterID, Port: port,
			FP: c.CertFP(), Addrs: discovery.LocalIPv4s(),
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(discovery.Jitter(c.cfg.ControllerBeacon)):
		}
	}
}
