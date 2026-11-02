package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"aicluster/common/proto"
)

type rebootDrivers struct {
	reboots int
	loadErr error
}

func (r *rebootDrivers) Installed(context.Context, []string) bool { return true }
func (r *rebootDrivers) Install(context.Context, proto.DriverSpec, string) error {
	return nil
}
func (r *rebootDrivers) Load(context.Context, proto.DriverSpec) (string, error) {
	return "", r.loadErr
}
func (r *rebootDrivers) Reboot() error                        { r.reboots++; return nil }
func (r *rebootDrivers) RestartRuntime(context.Context) error { return nil }

// A driver whose module only loads after a reboot gets exactly one reboot,
// and only when the Controller allows it.
func TestDriverReboot(t *testing.T) {
	rebootDelay = 0
	rd := &rebootDrivers{loadErr: fmt.Errorf("%w: module is for a newer kernel", ErrNeedReboot)}
	a := &Agent{cfg: Config{DataDir: t.TempDir(), Drivers: rd}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), wakeHB: make(chan struct{}, 1)}
	want := proto.DriverSpec{Name: "nvidia-driver-580-server-open", Version: "1", Packages: []string{"a"}}
	key := driverKey(&want)

	if err := a.installDriver(context.Background(), want, key, false); err != nil || rd.reboots != 0 {
		t.Fatalf("without permission: err %v, %d reboots", err, rd.reboots)
	}
	if o := a.observedDriver(); o.State != proto.DriverRebootRequired {
		t.Fatalf("state %+v", o)
	}
	if err := a.installDriver(context.Background(), want, key, true); err != nil || rd.reboots != 1 {
		t.Fatalf("with permission: err %v, %d reboots", err, rd.reboots)
	}
	if o := a.observedDriver(); o.State != proto.DriverRebooting {
		t.Fatalf("state %+v", o)
	}
	// Until the reboot happens, the loop asks for no second one.
	a.reconcileDriver(context.Background(), want, true)
	if rd.reboots != 1 {
		t.Fatalf("%d reboots while waiting for the first", rd.reboots)
	}
	a.drv.rebootAt = "" // as after the restart
	// After the reboot (another boot ID) the module still won't load.
	if err := os.WriteFile(a.file(fileDriverReboot), []byte(key+"\nother-boot"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.installDriver(context.Background(), want, key, true)
	if err == nil || !strings.Contains(err.Error(), "after a reboot") || rd.reboots != 1 {
		t.Fatalf("second reboot: err %v, %d reboots", err, rd.reboots)
	}
	// Once it loads, the marker goes.
	rd.loadErr = nil
	if err := a.installDriver(context.Background(), want, key, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.file(fileDriverReboot)); !errors.Is(err, os.ErrNotExist) || a.observedDriver().State != proto.DriverReady {
		t.Fatalf("after loading: marker %v, state %+v", err, a.observedDriver())
	}
}
