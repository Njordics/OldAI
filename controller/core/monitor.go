package core

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/store"
)

// monitorLoop is the reconciler for node lifecycle: it marks silent nodes
// offline, expires stalled enrollments, prunes stale sightings, and starts
// adoptions (auto mode) or probes (pending nodes) for newly seen nodes.
func (c *Core) monitorLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.MonitorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.monitorOnce(ctx)
		}
	}
}

func (c *Core) monitorOnce(ctx context.Context) {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		c.log.Error("monitor: list nodes", "err", err)
		return
	}
	now := time.Now().UTC()
	offlineAfter := c.cfg.HeartbeatInterval * proto.MissedHeartbeatsOffline
	mode := c.Mode()

	for _, n := range nodes {
		id := n.NodeID
		switch n.State {
		case store.StateAdopted:
			if now.Sub(n.LastHeartbeat) > offlineAfter {
				c.transition(ctx, id, store.StateAdopted, store.StateOffline, "")
				c.log.Warn("node offline", "node_id", id, "name", n.Name, "last_heartbeat", n.LastHeartbeat)
			}

		case store.StateEnrolling:
			if now.Sub(n.StateSince) > c.cfg.EnrollTimeout && !c.isInflight(id) {
				_ = c.Store.Revoke(ctx, n.CertSerial, id)
				c.transition(ctx, id, store.StateEnrolling, store.StateDiscovered, "no heartbeat after enrollment")
				c.log.Warn("enrollment timed out", "node_id", id)
			}

		case store.StateDiscovered, store.StatePending:
			if n.CertSerial == "" && !n.Returning && now.Sub(n.LastSeen) > c.cfg.DiscoveredTTL {
				_ = c.Store.DeleteNode(ctx, id)
				c.log.Info("forgot unseen node", "node_id", id)
				continue
			}
			if n.AgentState != proto.AgentUnadopted || c.isInflight(id) {
				continue
			}
			if mode == ModeAuto && !n.PairingRequired && n.State == store.StateDiscovered && c.backoffPassed(id, now) {
				go c.autoAdopt(ctx, id)
				continue
			}
			if n.State == store.StateDiscovered {
				c.transition(ctx, id, store.StateDiscovered, store.StatePending, "")
				c.log.Info("node waiting for approval", "node_id", id, "pairing_required", n.PairingRequired)
			}
			if len(n.Inventory) == 0 {
				go c.probe(ctx, id)
			}
		}
	}
}

// transition moves a node from one state to another only if it is still in
// the expected state (the heartbeat handler may have moved it meanwhile).
func (c *Core) transition(ctx context.Context, id, from, to, reason string) {
	_, _ = c.Store.UpdateNode(ctx, id, func(n *store.Node) error {
		if n.State == from {
			setState(n, to, time.Now().UTC())
			if reason != "" {
				n.LastError = reason
			}
			if to != store.StateAdopted {
				n.Phase = ""
			}
		}
		return nil
	})
}

func (c *Core) backoffPassed(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.After(c.backoff[id].next)
}

func (c *Core) autoAdopt(ctx context.Context, id string) {
	err := c.Adopt(ctx, id, "", false)
	if err == nil || err == ErrBusy {
		return
	}
	c.mu.Lock()
	b := c.backoff[id]
	b.delay = min(max(b.delay*2, 10*time.Second), 5*time.Minute)
	b.next = time.Now().Add(b.delay)
	c.backoff[id] = b
	c.mu.Unlock()
}

// probe fetches a pending node's hello (inventory and hardware fingerprint)
// so the admin can see what they are approving. It changes nothing on the node.
func (c *Core) probe(ctx context.Context, id string) {
	if !c.tryBegin(id) {
		return
	}
	defer c.end(id)
	n, err := c.Store.GetNode(ctx, id)
	if err != nil {
		return
	}
	var hello proto.Hello
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := httpx.Do(pctx, bootstrapClient(n.FP), http.MethodGet, nodeBase(n)+"/v1/enroll/hello", nil, &hello); err != nil || hello.NodeID != id {
		c.log.Debug("probe failed", "node_id", id, "err", err)
		return
	}
	inv, _ := json.Marshal(hello.Inventory)
	matches, _ := c.Store.FindByFingerprint(ctx, hello.HWFingerprint, id)
	_, _ = c.Store.UpdateNode(ctx, id, func(m *store.Node) error {
		m.Inventory, m.HWFingerprint = inv, hello.HWFingerprint
		m.Returning = m.Returning || len(matches) > 0
		return nil
	})
}
