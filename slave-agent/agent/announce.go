package agent

import (
	"context"
	"strings"
	"time"

	"aicluster/common/discovery"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/slave-agent/inventory"
)

// announcement is the current discovery payload, derived from agent state.
func (a *Agent) announcement() proto.Beacon {
	a.mu.RLock()
	defer a.mu.RUnlock()
	b := proto.Beacon{
		Kind:            proto.BeaconKindSlave,
		NodeID:          a.nodeID,
		State:           a.stateLocked(),
		Port:            a.port,
		FP:              a.bootstrapFP,
		HW:              inventory.GPUSummary(a.inv),
		PairingRequired: a.cfg.Pairing,
		AgentVersion:    a.cfg.Version,
		Addrs:           discovery.LocalIPv4s(),
	}
	if a.adopted != nil {
		b.ClusterID = a.adopted.ClusterID
		b.FP = pki.Fingerprint(a.adopted.cert.Certificate[0])
	}
	return b
}

func txtFor(b proto.Beacon) map[string]string {
	pc := "0"
	if b.PairingRequired {
		pc = "1"
	}
	return map[string]string{
		"v": "1", "id": b.NodeID, "st": b.State, "fp": b.FP, "cl": b.ClusterID,
		"hw": b.HW, "pc": pc, "ag": b.AgentVersion,
	}
}

// announceLoop keeps mDNS TXT records current and sends UDP beacons: every
// 5 s while unadopted, every 30 s once adopted, and immediately on change.
func (a *Agent) announceLoop(ctx context.Context) {
	sender, err := discovery.NewBeaconSender(a.cfg.BeaconPort, a.cfg.BeaconTargets)
	if err != nil {
		a.log.Error("beacon sender disabled", "err", err)
	} else {
		defer sender.Close()
	}

	// mDNS records carry the addresses the node had when it registered, so
	// it registers again when they change (e.g. after moving to the Slave
	// network); otherwise listeners keep seeing the old address.
	var ann *discovery.Announcer
	var annAddrs string
	announce := func(b proto.Beacon) {
		if ann != nil {
			ann.Close()
			ann = nil
		}
		var err error
		if ann, err = discovery.Announce("aic-"+b.NodeID[:8], proto.SlaveMDNSService, a.port, txtFor(b)); err != nil {
			a.log.Error("mdns announce disabled", "err", err)
			ann = nil
		}
		annAddrs = strings.Join(b.Addrs, ",")
	}
	defer func() {
		if ann != nil {
			ann.Close()
		}
	}()
	b := a.announcement()
	if a.cfg.MDNS {
		announce(b)
	}

	for {
		b = a.announcement()
		if sender != nil {
			if err := sender.Send(b); err != nil {
				a.log.Debug("beacon send failed", "err", err)
			}
		}
		if a.cfg.MDNS && strings.Join(b.Addrs, ",") != annAddrs {
			a.log.Info("addresses changed; announcing again", "addrs", b.Addrs)
			announce(b)
		} else if ann != nil {
			ann.Update(txtFor(b))
		}
		every := a.cfg.UnadoptedBeaconEvery
		if b.State == proto.AgentAdopted {
			every = a.cfg.AdoptedBeaconEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-a.stateC:
		case <-time.After(discovery.Jitter(every)):
		}
	}
}
