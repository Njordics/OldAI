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
	ours := s.ClusterID == c.ClusterID
	// apply runs inside the store's read-modify-write, so a sighting never
	// overwrites what adoption or a heartbeat saved meanwhile (mDNS and the
	// beacon report the same node every few seconds, during adoption too).
	var moved, lostIdentity bool
	var revokeSerial string
	apply := func(n *store.Node) error {
		moved = n.Addr != "" && (n.Addr != s.Addr || n.Port != s.Port)
		lostIdentity, revokeSerial = false, ""
		n.Addr, n.Port, n.FP = s.Addr, s.Port, s.FP
		n.AgentState, n.ClusterID = s.State, s.ClusterID
		n.HWSummary, n.PairingRequired, n.AgentVersion = s.HW, s.PairingRequired, s.AgentVersion
		n.LastSeen = now

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
				lostIdentity, revokeSerial = true, n.CertSerial
				n.CertSerial, n.Returning, n.Inventory = "", true, nil
				setState(n, store.StateDiscovered, now)
			case store.StateForeign:
				setState(n, store.StateDiscovered, now)
			}
		}
		return nil
	}

	_, err := c.Store.UpdateNode(ctx, s.NodeID, apply)
	if errors.Is(err, store.ErrNotFound) {
		n := &store.Node{NodeID: s.NodeID, State: store.StateDiscovered, FirstSeen: now, StateSince: now}
		_ = apply(n)
		var added bool
		if added, err = c.Store.InsertNode(ctx, n); err == nil {
			if added {
				c.log.Info("node discovered", "node_id", s.NodeID, "addr", s.Addr, "source", s.Source, "hw", s.HW)
			} else {
				// Another sighting created it first.
				_, err = c.Store.UpdateNode(ctx, s.NodeID, apply)
			}
		}
	}
	if err != nil {
		c.log.Error("save node", "err", err)
		return
	}
	if lostIdentity {
		c.log.Warn("adopted node reappeared unadopted", "node_id", s.NodeID)
		_ = c.Store.Revoke(ctx, revokeSerial, s.NodeID)
	}
	if moved && s.State == proto.AgentAdopted && ours {
		// Pool leaders dial their workers at this address.
		go c.resyncPoolsOf(context.Background(), s.NodeID)
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
