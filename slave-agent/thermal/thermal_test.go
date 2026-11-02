package thermal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aicluster/common/proto"
)

// fakeSys builds a hwmon tree: an Intel CPU, an NVMe drive and a
// motherboard chip with one fan the OS can drive and one it can't.
func fakeSys(t *testing.T) (*Monitor, string) {
	t.Helper()
	root := t.TempDir()
	write := func(p, v string) {
		p = filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hwmon/hwmon0/name", "coretemp")
	write("hwmon/hwmon0/temp1_label", "Package id 0")
	write("hwmon/hwmon0/temp1_input", "62000")
	write("hwmon/hwmon0/temp1_crit", "100000")
	write("hwmon/hwmon0/temp2_label", "Core 0")
	write("hwmon/hwmon0/temp2_input", "64000")
	write("hwmon/hwmon1/name", "nvme")
	write("hwmon/hwmon1/temp1_label", "Composite")
	write("hwmon/hwmon1/temp1_input", "41850")
	write("hwmon/hwmon2/name", "nct6798")
	write("hwmon/hwmon2/fan1_input", "820")
	write("hwmon/hwmon2/pwm1", "102")
	write("hwmon/hwmon2/pwm1_enable", "5")
	write("hwmon/hwmon2/fan2_input", "1500")
	write("cpu/cpu0/thermal_throttle/package_throttle_count", "10")
	return &Monitor{HwmonRoot: filepath.Join(root, "hwmon"), CPURoot: filepath.Join(root, "cpu"), NvidiaSMI: "no-such-nvidia-smi"}, root
}

func TestRead(t *testing.T) {
	m, root := fakeSys(t)
	ctl := &FanController{StateFile: filepath.Join(root, "fans.json")}
	th, gpus := m.Read(ctl)
	if th.CPUTempC != 62 || gpus != nil || th.Throttled {
		t.Fatalf("thermal %+v gpus %v", th, gpus)
	}
	var labels []string
	for _, s := range th.Sensors {
		labels = append(labels, s.Chip+":"+s.Label)
	}
	if strings.Join(labels, ",") != "coretemp:Package id 0,nvme:Composite" {
		t.Fatalf("sensors %v", labels)
	}
	if th.Sensors[0].CritC != 100 {
		t.Fatalf("crit %+v", th.Sensors[0])
	}
	if len(th.Fans) != 2 || th.Fans[0].Control != proto.FansFirmware || th.Fans[0].RPM != 820 || th.Fans[0].Duty != 40 ||
		th.Fans[1].Control != "fixed" {
		t.Fatalf("fans %+v", th.Fans)
	}
	if th.FanControl != "yes" {
		t.Fatalf("fan control %q", th.FanControl)
	}
	_ = os.WriteFile(filepath.Join(root, "cpu/cpu0/thermal_throttle/package_throttle_count"), []byte("12\n"), 0o644)
	if th, _ := m.Read(ctl); !th.Throttled {
		t.Fatal("throttling not noticed")
	}
	if th, _ := m.Read(ctl); th.Throttled {
		t.Fatal("throttled with no new events")
	}
}

func TestBoostDuty(t *testing.T) {
	for temp, want := range map[float64]int{30: 35, 45: 35, 60: 68, 75: 100, 90: 100} {
		if got := BoostDuty(temp); got != want {
			t.Errorf("BoostDuty(%v) = %d, want %d", temp, got, want)
		}
	}
}

func TestBoostAndRestore(t *testing.T) {
	m, root := fakeSys(t)
	pwm := filepath.Join(root, "hwmon/hwmon2/pwm1")
	state := filepath.Join(root, "fans.json")
	ctl := &FanController{StateFile: state}
	read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }

	th, _ := m.Read(ctl)
	ctl.Apply(m, proto.FansBoost, th) // 62 °C → 72% → 183
	if read(pwm+"_enable") != "1" || read(pwm) != "183" {
		t.Fatalf("boost: enable %s pwm %s", read(pwm+"_enable"), read(pwm))
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatal("modes not saved before taking over")
	}
	if th, _ := m.Read(ctl); th.Fans[0].Control != "agent" {
		t.Fatalf("fan control shown as %q", th.Fans[0].Control)
	}
	// Cool: never below what the firmware ran it at (102).
	ctl.Apply(m, proto.FansBoost, &proto.Thermal{CPUTempC: 30})
	if read(pwm) != "102" {
		t.Fatalf("cool: pwm %s", read(pwm))
	}
	// Overheating: full speed.
	ctl.Apply(m, proto.FansBoost, &proto.Thermal{CPUTempC: 90})
	if read(pwm) != "255" {
		t.Fatalf("hot: pwm %s", read(pwm))
	}
	// Back to firmware.
	ctl.Apply(m, proto.FansFirmware, th)
	if read(pwm+"_enable") != "5" {
		t.Fatalf("restore: enable %s", read(pwm+"_enable"))
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("state file left behind")
	}

	// A crashed agent: the next start (or systemd) restores from the file.
	ctl2 := &FanController{StateFile: state}
	ctl2.Apply(m, proto.FansBoost, th)
	if read(pwm+"_enable") != "1" {
		t.Fatal("not boosted")
	}
	if err := RestoreFromFile(state); err != nil {
		t.Fatal(err)
	}
	if read(pwm+"_enable") != "5" {
		t.Fatalf("restore from file: enable %s", read(pwm+"_enable"))
	}
	if err := RestoreFromFile(state); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	// Without a temperature the firmware keeps the fan.
	ctl3 := &FanController{StateFile: state}
	ctl3.Apply(m, proto.FansBoost, &proto.Thermal{})
	if read(pwm+"_enable") != "5" {
		t.Fatal("took the fan with no temperature to go by")
	}
}

func TestParseNvidia(t *testing.T) {
	g := parseNvidia("0, GPU-abc, 87, 20100, 24564, 71, 312.45, Not Active, Active\n")
	if len(g) != 1 || g[0].Util != 0.87 || g[0].MemUsed != 20100<<20 || g[0].TempC != 71 || g[0].PowerW != 312.45 || !g[0].Throttled {
		t.Fatalf("%+v", g)
	}
}
