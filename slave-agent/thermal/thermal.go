// Package thermal reads a Slave's temperatures and fans and, when the
// Controller asks for it, runs the fans harder than the board's firmware
// would (protocol §15).
//
// Linux lists hardware monitors under /sys/class/hwmon: one directory per
// chip (coretemp, k10temp, nvme, amdgpu, the motherboard's nct6775 or it87,
// …) with tempN_input in millidegrees, fanN_input in RPM and, where the
// chip lets the OS drive a fan, pwmN (0-255) and pwmN_enable (1 = manual,
// 2 and up = the chip's automatic modes). NVIDIA cards report through
// nvidia-smi instead.
package thermal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aicluster/common/proto"
)

// Monitor reads sensors. The zero value reads the real system.
type Monitor struct {
	HwmonRoot string // default /sys/class/hwmon
	CPURoot   string // default /sys/devices/system/cpu
	NvidiaSMI string // default nvidia-smi

	mu           sync.Mutex
	lastThrottle uint64
	smi          string // resolved nvidia-smi path; "-" when absent
}

func (m *Monitor) hwmonRoot() string {
	if m.HwmonRoot == "" {
		return "/sys/class/hwmon"
	}
	return m.HwmonRoot
}

func (m *Monitor) cpuRoot() string {
	if m.CPURoot == "" {
		return "/sys/devices/system/cpu"
	}
	return m.CPURoot
}

// cpuChips report the processor's own temperature.
var cpuChips = map[string]bool{"coretemp": true, "k10temp": true, "zenpower": true, "cpu_thermal": true}

// maxSensors keeps heartbeats small on boards with dozens of inputs.
const maxSensors = 24

func readInt(p string) (int64, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

func readStr(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// chip is one hwmon directory.
type chip struct {
	dir, name string
}

func (m *Monitor) chips() []chip {
	dirs, _ := filepath.Glob(filepath.Join(m.hwmonRoot(), "hwmon*"))
	sort.Strings(dirs)
	var out []chip
	for _, d := range dirs {
		out = append(out, chip{dir: d, name: readStr(filepath.Join(d, "name"))})
	}
	return out
}

// indexes returns N for every <prefix>N<suffix> file in dir, sorted.
func indexes(dir, prefix, suffix string) []int {
	files, _ := filepath.Glob(filepath.Join(dir, prefix+"*"+suffix))
	var out []int
	for _, f := range files {
		s := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), prefix), suffix)
		if n, err := strconv.Atoi(s); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// temps reads a chip's temperatures. Per-core readings are left out: the
// package reading covers them.
func (c chip) temps() []proto.Sensor {
	var out []proto.Sensor
	for _, i := range indexes(c.dir, "temp", "_input") {
		base := filepath.Join(c.dir, "temp"+strconv.Itoa(i))
		milli, ok := readInt(base + "_input")
		if !ok || milli <= -40000 || milli >= 150000 {
			continue // missing or nonsense
		}
		label := readStr(base + "_label")
		if c.name == "coretemp" && strings.HasPrefix(label, "Core ") {
			continue
		}
		if label == "" {
			label = "temp" + strconv.Itoa(i)
		}
		s := proto.Sensor{Chip: c.name, Label: label, TempC: float64(milli) / 1000}
		if crit, ok := readInt(base + "_crit"); ok && crit > 0 {
			s.CritC = float64(crit) / 1000
		}
		out = append(out, s)
	}
	return out
}

// Read returns the node's temperatures and fans, and NVIDIA cards' live
// state for the heartbeat's GPU list. fans says which PWM outputs the
// agent is driving (nil: none).
func (m *Monitor) Read(fans *FanController) (*proto.Thermal, []proto.GPUHealth) {
	t := &proto.Thermal{}
	for _, c := range m.chips() {
		for _, s := range c.temps() {
			if cpuChips[c.name] && s.TempC > t.CPUTempC {
				t.CPUTempC = s.TempC
			}
			if len(t.Sensors) < maxSensors {
				t.Sensors = append(t.Sensors, s)
			}
		}
		t.Fans = append(t.Fans, c.fans(fans)...)
	}
	t.Throttled = m.cpuThrottled()
	gpus := m.nvidia()
	for _, g := range gpus {
		if g.TempC > 0 && len(t.Sensors) < maxSensors {
			t.Sensors = append(t.Sensors, proto.Sensor{Chip: "nvidia", Label: "GPU " + strconv.Itoa(g.Index), TempC: g.TempC})
		}
	}
	t.FanControl = fans.Status(m)
	return t, gpus
}

// fans lists a chip's fans, matching fanN's speed with pwmN's output.
func (c chip) fans(ctl *FanController) []proto.Fan {
	seen := map[int]bool{}
	var out []proto.Fan
	add := func(i int) {
		if seen[i] {
			return
		}
		seen[i] = true
		n := strconv.Itoa(i)
		f := proto.Fan{ID: c.name + "/fan" + n, Label: readStr(filepath.Join(c.dir, "fan"+n+"_label")), Control: "fixed"}
		if rpm, ok := readInt(filepath.Join(c.dir, "fan"+n+"_input")); ok {
			f.RPM = int(rpm)
		}
		pwm := filepath.Join(c.dir, "pwm"+n)
		if v, ok := readInt(pwm); ok {
			f.Duty = int((v*100 + 127) / 255)
			if writable(pwm) && writable(pwm+"_enable") {
				f.Control = proto.FansFirmware
				if ctl.Driving(pwm) {
					f.Control = "agent"
				}
			}
		}
		out = append(out, f)
	}
	for _, i := range indexes(c.dir, "fan", "_input") {
		add(i)
	}
	for _, i := range indexes(c.dir, "pwm", "") {
		add(i)
	}
	return out
}

// writable reports whether the agent may write p. Opening a sysfs file for
// writing changes nothing until something is written.
func writable(p string) bool {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// cpuThrottled reports whether Intel's thermal throttling counters went up
// since the last call. AMD doesn't expose one; there the dashboard goes by
// temperature.
func (m *Monitor) cpuThrottled() bool {
	files, _ := filepath.Glob(filepath.Join(m.cpuRoot(), "cpu*", "thermal_throttle", "package_throttle_count"))
	var sum uint64
	for _, f := range files {
		if v, ok := readInt(f); ok && v > 0 {
			sum += uint64(v)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	throttled := m.lastThrottle != 0 && sum > m.lastThrottle
	m.lastThrottle = sum
	return throttled
}

// nvidia reads NVIDIA cards' utilisation, memory, temperature, power and
// whether they slow down for heat. Nothing when nvidia-smi isn't there.
func (m *Monitor) nvidia() []proto.GPUHealth {
	m.mu.Lock()
	if m.smi == "" {
		name := m.NvidiaSMI
		if name == "" {
			name = "nvidia-smi"
		}
		if p, err := exec.LookPath(name); err == nil {
			m.smi = p
		} else {
			m.smi = "-"
		}
	}
	smi := m.smi
	m.mu.Unlock()
	if smi == "-" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, smi, "--query-gpu="+nvidiaFields, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNvidia(string(out))
}

const nvidiaFields = "index,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw," +
	"clocks_throttle_reasons.hw_thermal_slowdown,clocks_throttle_reasons.sw_thermal_slowdown"

func parseNvidia(out string) []proto.GPUHealth {
	var gpus []proto.GPUHealth
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 9 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
		idx, _ := strconv.Atoi(f[0])
		gpus = append(gpus, proto.GPUHealth{
			Index: idx, UUID: f[1], Util: num(f[2]) / 100,
			MemUsed: uint64(num(f[3])) << 20, MemTotal: uint64(num(f[4])) << 20,
			TempC: num(f[5]), PowerW: num(f[6]),
			Throttled: f[7] == "Active" || f[8] == "Active",
		})
	}
	return gpus
}
