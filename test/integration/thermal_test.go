package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/slave-agent/agent"
	"aicluster/slave-agent/thermal"
)

// Temperatures reach the Controller, and the cooling setting takes over
// the node's fan and hands it back.
func TestTemperaturesAndFans(t *testing.T) {
	dir := t.TempDir()
	write := func(p, v string) {
		p = filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return strings.TrimSpace(string(b)) }
	write("hwmon/hwmon0/name", "coretemp")
	write("hwmon/hwmon0/temp1_label", "Package id 0")
	write("hwmon/hwmon0/temp1_input", "71000")
	write("hwmon/hwmon1/name", "nct6798")
	write("hwmon/hwmon1/fan1_input", "900")
	write("hwmon/hwmon1/pwm1", "90")
	write("hwmon/hwmon1/pwm1_enable", "5")

	c := startController(t, core.ModeAuto)
	s := startSlave(t, c, func(cfg *agent.Config) {
		cfg.Thermal = &thermal.Monitor{HwmonRoot: filepath.Join(dir, "hwmon"), CPURoot: filepath.Join(dir, "cpu"), NvidiaSMI: "none"}
		cfg.Fans = &thermal.FanController{StateFile: filepath.Join(dir, "fans.json")}
	})
	adoptAll(t, c, s)

	health := func() proto.Health {
		var h proto.Health
		_ = json.Unmarshal(c.node(t, s.NodeID()).Health, &h)
		return h
	}
	waitFor(t, 20*time.Second, "temperatures in the heartbeat", func() bool {
		h := health()
		return h.Thermal != nil && h.Thermal.CPUTempC == 71 && h.Thermal.FanControl == "yes" && len(h.Thermal.Fans) == 1
	})

	ctx := context.Background()
	if err := c.SetFanMode(ctx, "boost"); err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(thermal.BoostDuty(71) * 255 / 100)
	waitFor(t, 20*time.Second, "the agent to boost the fan", func() bool {
		return read("hwmon/hwmon1/pwm1_enable") == "1" && read("hwmon/hwmon1/pwm1") == want
	})
	waitFor(t, 20*time.Second, "the dashboard to show the fan as boosted", func() bool {
		h := health()
		return h.Thermal != nil && len(h.Thermal.Fans) == 1 && h.Thermal.Fans[0].Control == "agent"
	})
	if err := c.SetFanMode(ctx, "loud"); err == nil {
		t.Fatal("accepted an unknown fan mode")
	}
	if err := c.SetFanMode(ctx, "firmware"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the fan back under the firmware", func() bool {
		return read("hwmon/hwmon1/pwm1_enable") == "5"
	})
	if _, err := os.Stat(filepath.Join(dir, "fans.json")); !os.IsNotExist(err) {
		t.Fatal("fan state file left behind")
	}

	// Stopping the agent while boosted hands the fan back too.
	if err := c.SetFanMode(ctx, "boost"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "boost again", func() bool { return read("hwmon/hwmon1/pwm1_enable") == "1" })
	s.stop()
	if read("hwmon/hwmon1/pwm1_enable") != "5" {
		t.Fatal("stopping the agent left the fan boosted")
	}
}
