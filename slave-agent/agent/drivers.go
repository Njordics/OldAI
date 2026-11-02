package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/slave-agent/inventory"
)

// GPU drivers (protocol §14). The Controller names the Ubuntu packages for
// this node's cards; the agent installs them with apt from the Controller's
// copy of Ubuntu's archive, reached through the loopback shim, loads the
// kernel module and reboots when the module is for a newer kernel.

// DriverSystem installs GPU drivers. AptDrivers is the real one.
type DriverSystem interface {
	// Installed reports whether every package is installed.
	Installed(ctx context.Context, pkgs []string) bool
	// Install installs spec's packages from the archive at baseURL.
	Install(ctx context.Context, spec proto.DriverSpec, baseURL string) error
	// Load loads the driver and returns its version. ErrNeedReboot (wrapped)
	// means a reboot will make it load.
	Load(ctx context.Context) (string, error)
	Reboot() error
	// RestartRuntime makes the model runtime look for GPUs again.
	RestartRuntime(ctx context.Context) error
}

// ErrNeedReboot is wrapped by DriverSystem.Load when only a reboot helps.
var ErrNeedReboot = errors.New("reboot needed")

const (
	fileDriverReboot = "driver-reboot" // the driver key this node last rebooted for, and the boot
	// driverRetry is how long a failed install waits before trying again.
	driverRetry = 30 * time.Minute
)

var rebootDelay = 3 * time.Second

type drvState struct {
	mu       sync.Mutex
	obs      *proto.ObservedDriver
	readyKey string
	rebootAt string // the driver key a reboot was asked for; nothing more to do until then
	failKey  string
	failAt   time.Time
}

func driverKey(s *proto.DriverSpec) string {
	return s.Name + "=" + s.Version + " " + strings.Join(s.Packages, " ")
}

func (a *Agent) setDriver(obs *proto.ObservedDriver) {
	a.drv.mu.Lock()
	a.drv.obs = obs
	a.drv.mu.Unlock()
	nudge(a.wakeHB)
}

func (a *Agent) observedDriver() *proto.ObservedDriver {
	a.drv.mu.Lock()
	defer a.drv.mu.Unlock()
	if a.drv.obs == nil {
		return nil
	}
	o := *a.drv.obs
	return &o
}

func (a *Agent) driverLoop(ctx context.Context) {
	for {
		a.mu.RLock()
		ch := a.desiredC
		want, allowReboot := a.desired.Drivers, a.desired.AllowReboot
		ad := a.adopted
		a.mu.RUnlock()
		switch {
		case want == nil || ad == nil:
			a.drv.mu.Lock()
			a.drv.obs, a.drv.readyKey = nil, ""
			a.drv.mu.Unlock()
		case a.cfg.Drivers == nil:
			a.setDriver(&proto.ObservedDriver{Name: want.Name, State: proto.DriverFailed, Error: "this node can't install drivers"})
		default:
			a.reconcileDriver(ctx, *want, allowReboot)
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-time.After(time.Minute):
		}
	}
}

func (a *Agent) reconcileDriver(ctx context.Context, want proto.DriverSpec, allowReboot bool) {
	key := driverKey(&want)
	a.drv.mu.Lock()
	skip := a.drv.readyKey == key || a.drv.rebootAt == key || a.drv.failKey == key && time.Since(a.drv.failAt) < driverRetry
	a.drv.mu.Unlock()
	if skip {
		return
	}
	err := a.installDriver(ctx, want, key, allowReboot)
	if err == nil || ctx.Err() != nil {
		return
	}
	a.log.Error("GPU driver install failed", "driver", want.Name, "err", err)
	a.drv.mu.Lock()
	a.drv.failKey, a.drv.failAt = key, time.Now()
	a.drv.mu.Unlock()
	a.setDriver(&proto.ObservedDriver{Name: want.Name, State: proto.DriverFailed, Error: err.Error()})
}

func (a *Agent) installDriver(ctx context.Context, want proto.DriverSpec, key string, allowReboot bool) error {
	ds := a.cfg.Drivers
	if !ds.Installed(ctx, want.Packages) {
		host := a.ShimHost()
		if host == "" {
			return errors.New("the loopback shim isn't running")
		}
		a.log.Info("installing GPU driver", "driver", want.Name, "version", want.Version, "packages", want.Packages)
		a.setDriver(&proto.ObservedDriver{Name: want.Name, State: proto.DriverInstalling})
		if err := ds.Install(ctx, want, "http://"+host+want.Mirror); err != nil {
			return err
		}
	}
	version, err := ds.Load(ctx)
	if errors.Is(err, ErrNeedReboot) {
		return a.rebootForDriver(want, key, allowReboot, err)
	}
	if err != nil {
		return err
	}
	_ = os.Remove(a.file(fileDriverReboot))
	a.log.Info("GPU driver ready", "driver", want.Name, "version", version)
	// The cards now report their memory, and the runtime can use them.
	a.refreshInventory()
	if err := ds.RestartRuntime(ctx); err != nil {
		a.log.Warn("restart the model runtime", "err", err)
	}
	a.drv.mu.Lock()
	a.drv.readyKey, a.drv.failKey = key, ""
	a.drv.mu.Unlock()
	a.setDriver(&proto.ObservedDriver{Name: want.Name, State: proto.DriverReady, Version: version})
	return nil
}

// rebootForDriver reboots once per driver; if the module still won't load
// after that, rebooting again won't help.
func (a *Agent) rebootForDriver(want proto.DriverSpec, key string, allowReboot bool, why error) error {
	boot := inventory.BootID()
	if b, err := os.ReadFile(a.file(fileDriverReboot)); err == nil {
		prevKey, prevBoot, _ := strings.Cut(string(b), "\n")
		if prevKey == key && prevBoot != boot {
			return fmt.Errorf("installed, but it still doesn't load after a reboot: %w", why)
		}
	}
	if !allowReboot {
		a.setDriver(&proto.ObservedDriver{Name: want.Name, State: proto.DriverRebootRequired, Error: why.Error()})
		return nil
	}
	if err := os.WriteFile(a.file(fileDriverReboot), []byte(key+"\n"+boot), 0o600); err != nil {
		return err
	}
	a.log.Warn("rebooting to load the GPU driver", "driver", want.Name, "why", why)
	a.setDriver(&proto.ObservedDriver{Name: want.Name, State: proto.DriverRebooting, Error: why.Error()})
	time.Sleep(rebootDelay) // let the heartbeat carry the news
	if err := a.cfg.Drivers.Reboot(); err != nil {
		return err
	}
	a.drv.mu.Lock()
	a.drv.rebootAt = key
	a.drv.mu.Unlock()
	return nil
}
