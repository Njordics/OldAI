package thermal

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"aicluster/common/proto"
)

// FanController runs fans harder than the firmware would ("boost") and
// hands them back. Before taking a fan it saves the chip's mode to
// StateFile, so `aicluster-agent fans-restore` (run by systemd whenever
// the agent stops, crashes included) can return it to the firmware.
//
// It never runs a fan slower than the firmware did when the agent took it
// over: the firmware's curve is the safety net against overheating.
type FanController struct {
	StateFile string
	Log       *slog.Logger

	mu      sync.Mutex
	driving map[string]*saved // pwm path → what to restore
	broken  map[string]string // pwm path → why the agent can't drive it
}

type saved struct {
	Enable   int64 `json:"enable"`   // pwmN_enable before the agent took over
	Baseline int64 `json:"baseline"` // pwmN then; the agent never goes lower
	Written  int64 `json:"-"`
}

// Boost curve: duty in percent by temperature.
const (
	boostLowC     = 45.0
	boostHighC    = 75.0
	boostMinDuty  = 35
	emergencyC    = 85.0 // full speed regardless
	minDutyChange = 8    // PWM steps; avoids rewriting for noise
)

// BoostDuty is the boost curve: 35% at 45 °C or less, rising evenly to
// 100% at 75 °C.
func BoostDuty(tempC float64) int {
	switch {
	case tempC <= boostLowC:
		return boostMinDuty
	case tempC >= boostHighC:
		return 100
	}
	return boostMinDuty + int((tempC-boostLowC)/(boostHighC-boostLowC)*float64(100-boostMinDuty)+0.5)
}

// Driving reports whether the agent drives the pwm at path.
func (f *FanController) Driving(pwm string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.driving[pwm]
	return ok
}

// Status is Thermal.FanControl: "yes", or why the agent can't drive fans.
func (f *FanController) Status(m *Monitor) string {
	if f == nil {
		return "not supported on this system"
	}
	n, seen := 0, false
	for _, c := range m.chips() {
		for _, p := range c.pwms() {
			seen = true
			f.mu.Lock()
			_, bad := f.broken[p]
			f.mu.Unlock()
			if !bad {
				n++
			}
		}
		if len(indexes(c.dir, "fan", "_input")) > 0 {
			seen = true
		}
	}
	switch {
	case n > 0:
		return "yes"
	case seen:
		return "the board's firmware runs the fans; the operating system can't set their speed"
	default:
		return "no fans are visible to the operating system"
	}
}

// pwms lists the chip's fan outputs the agent may write.
func (c chip) pwms() []string {
	var out []string
	for _, i := range indexes(c.dir, "pwm", "") {
		p := filepath.Join(c.dir, "pwm"+strconv.Itoa(i))
		if writable(p) && writable(p+"_enable") {
			out = append(out, p)
		}
	}
	return out
}

func gpuChip(name string) bool { return name == "amdgpu" || name == "nouveau" || name == "radeon" }

// Apply runs one control step for mode, using the temperatures in t. In
// firmware mode it hands back every fan the agent drives.
func (f *FanController) Apply(m *Monitor, mode string, t *proto.Thermal) {
	if mode != proto.FansBoost {
		f.Restore()
		return
	}
	// Board fans cool everything: follow the hottest CPU or GPU.
	hottest := t.CPUTempC
	for _, s := range t.Sensors {
		if (s.Chip == "nvidia" || gpuChip(s.Chip)) && s.TempC > hottest {
			hottest = s.TempC
		}
	}
	for _, c := range m.chips() {
		ref := hottest
		if gpuChip(c.name) {
			// A graphics card's fan follows that card.
			ref = 0
			for _, s := range c.temps() {
				ref = max(ref, s.TempC)
			}
		}
		for _, p := range c.pwms() {
			f.drive(p, ref)
		}
	}
}

func (f *FanController) drive(pwm string, tempC float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.driving == nil {
		f.driving, f.broken = map[string]*saved{}, map[string]string{}
	}
	if _, bad := f.broken[pwm]; bad {
		return
	}
	s, ok := f.driving[pwm]
	if tempC <= 0 {
		// No temperature to go by: the firmware knows better.
		if ok {
			f.restoreLocked(pwm, s)
		}
		return
	}
	if !ok {
		enable, ok1 := readInt(pwm + "_enable")
		base, ok2 := readInt(pwm)
		if !ok1 || !ok2 {
			f.broken[pwm] = "can't read the fan's mode"
			return
		}
		s = &saved{Enable: enable, Baseline: base, Written: -1}
		f.driving[pwm] = s
		// Saved before anything is written, so a crash can still restore it.
		if err := f.saveLocked(); err != nil {
			delete(f.driving, pwm)
			f.broken[pwm] = err.Error()
			f.log().Error("fan control: can't save the fans' modes", "err", err)
			return
		}
		if err := os.WriteFile(pwm+"_enable", []byte("1"), 0); err != nil {
			delete(f.driving, pwm)
			f.broken[pwm] = err.Error()
			_ = f.saveLocked()
			f.log().Warn("fan control: the chip refused manual mode", "pwm", pwm, "err", err)
			return
		}
		f.log().Info("fan control: boosting", "pwm", pwm, "firmware_mode", enable, "firmware_pwm", base)
	}
	want := int64(BoostDuty(tempC)) * 255 / 100
	if tempC >= emergencyC {
		want = 255
	}
	want = max(want, s.Baseline)
	if s.Written >= 0 && abs(want-s.Written) < minDutyChange && want != 255 {
		return
	}
	if err := os.WriteFile(pwm, []byte(strconv.FormatInt(want, 10)), 0); err != nil {
		f.log().Warn("fan control: can't set the fan; handing it back to the firmware", "pwm", pwm, "err", err)
		f.restoreLocked(pwm, s)
		f.broken[pwm] = err.Error()
		return
	}
	s.Written = want
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Restore hands every fan back to the firmware.
func (f *FanController) Restore() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, s := range f.driving {
		f.restoreLocked(p, s)
	}
}

func (f *FanController) restoreLocked(pwm string, s *saved) {
	if s.Enable == 1 {
		// It was manual before us: put its speed back too.
		_ = os.WriteFile(pwm, []byte(strconv.FormatInt(s.Baseline, 10)), 0)
	}
	if err := os.WriteFile(pwm+"_enable", []byte(strconv.FormatInt(s.Enable, 10)), 0); err != nil {
		f.log().Error("fan control: can't hand the fan back to the firmware", "pwm", pwm, "err", err)
	} else {
		f.log().Info("fan control: back to firmware", "pwm", pwm)
	}
	delete(f.driving, pwm)
	_ = f.saveLocked()
}

func (f *FanController) saveLocked() error {
	if len(f.driving) == 0 {
		err := os.Remove(f.StateFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, _ := json.Marshal(f.driving)
	tmp := f.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.StateFile)
}

func (f *FanController) log() *slog.Logger {
	if f.Log == nil {
		return slog.Default()
	}
	return f.Log
}

// RestoreFromFile hands back the fans a previous agent left boosted. It
// is safe to run when there is nothing to restore.
func RestoreFromFile(stateFile string) error {
	b, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m map[string]*saved
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", stateFile, err)
	}
	var errs []error
	for p, s := range m {
		if s.Enable == 1 {
			_ = os.WriteFile(p, []byte(strconv.FormatInt(s.Baseline, 10)), 0)
		}
		if err := os.WriteFile(p+"_enable", []byte(strconv.FormatInt(s.Enable, 10)), 0); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return os.Remove(stateFile)
	}
	return errors.Join(errs...)
}
