package core

import (
	"context"
	"encoding/json"

	"aicluster/common/proto"
	"aicluster/controller/store"
)

// Temperatures and fans (protocol §15). Nodes report temperatures in every
// heartbeat. The fan setting is cluster-wide: "firmware" (default) leaves
// the fans to each board, "boost" has agents run the fans they can control
// harder than the firmware would.

const settingFans = "fans"

// HotC is the CPU temperature above which a node counts as hot even when
// it doesn't report throttling (AMD processors don't).
const HotC = 95.0

// FanMode is the cluster's fan setting.
func (c *Core) FanMode(ctx context.Context) string {
	v, _ := c.Store.Setting(ctx, settingFans, proto.FansFirmware)
	if v != proto.FansBoost {
		return proto.FansFirmware
	}
	return v
}

// SetFanMode changes the fan setting and tells every node.
func (c *Core) SetFanMode(ctx context.Context, mode string) error {
	if mode != proto.FansFirmware && mode != proto.FansBoost {
		return ErrBadFanMode
	}
	if err := c.Store.SetSetting(ctx, settingFans, mode); err != nil {
		return err
	}
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if servesModels(n) {
			c.SyncDesired(ctx, n.NodeID)
		}
	}
	return nil
}

func (c *Core) fanSpec(ctx context.Context) *proto.FanSpec {
	if c.FanMode(ctx) == proto.FansBoost {
		return &proto.FanSpec{Mode: proto.FansBoost}
	}
	return nil
}

// isHot reports whether a node is slowing down for heat, or close to it.
func isHot(n *store.Node) bool {
	var h proto.Health
	if len(n.Health) == 0 || json.Unmarshal(n.Health, &h) != nil {
		return false
	}
	for _, g := range h.GPUs {
		if g.Throttled {
			return true
		}
	}
	return h.Thermal != nil && (h.Thermal.Throttled || h.Thermal.CPUTempC >= HotC)
}
