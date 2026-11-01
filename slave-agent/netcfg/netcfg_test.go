package netcfg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aicluster/common/proto"
)

var spec = proto.SlaveNetwork{Subnet: "10.10.10.0/24", ControllerIP: "10.10.10.1", Address: "10.10.10.10",
	MAC: "52:54:00:77:00:21", Domain: "aicluster", NodeAPIPort: 7443}

func TestRender(t *testing.T) {
	r := Render(spec)
	for _, want := range []string{"MACAddress=52:54:00:77:00:21", "DHCP=ipv4", "AllowList=10.10.10.1", "IPv6AcceptRA=no", "Domains=aicluster"} {
		if !strings.Contains(r, want) {
			t.Errorf("config lacks %q:\n%s", want, r)
		}
	}
	if FileName >= "10-netplan" {
		t.Fatal("our file must sort before netplan's so networkd uses it")
	}
}

// fakeNet simulates networkd: the address the interface ends up with
// depends on whether our file is installed.
type fakeNet struct {
	dir      string
	slaveIP  string // what the Controller hands out ("" = it never answers)
	homeIP   string
	commands []string
}

func (f *fakeNet) manager() *Manager {
	return &Manager{
		Dir: f.dir,
		Run: func(_ context.Context, name string, args ...string) error {
			f.commands = append(f.commands, name+" "+strings.Join(args, " "))
			return nil
		},
		Addrs: func(string) []string {
			if _, err := os.Stat(filepath.Join(f.dir, FileName)); err == nil {
				if f.slaveIP == "" {
					return nil
				}
				return []string{f.slaveIP}
			}
			return []string{f.homeIP}
		},
	}
}

func TestApplyAndRevert(t *testing.T) {
	f := &fakeNet{dir: t.TempDir(), slaveIP: "10.10.10.10", homeIP: "192.168.1.50"}
	m := f.manager()
	ctx := context.Background()
	if err := m.Apply(ctx, "enp1s0", spec, 3*time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !m.Active() || strings.Join(f.commands, ";") != "networkctl reload;networkctl reconfigure enp1s0" {
		t.Fatalf("active %v, commands %v", m.Active(), f.commands)
	}
	if err := m.Revert(ctx, "enp1s0", 3*time.Second); err != nil || m.Active() {
		t.Fatalf("revert: %v, active %v", err, m.Active())
	}
}

func TestApplyRollsBackWhenControllerNeverAnswers(t *testing.T) {
	f := &fakeNet{dir: t.TempDir(), homeIP: "192.168.1.50"}
	m := f.manager()
	err := m.Apply(context.Background(), "enp1s0", spec, 1500*time.Millisecond, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if m.Active() {
		t.Fatal("config left installed after a failed handoff")
	}
}

func TestApplyRollsBackWhenControllerUnreachable(t *testing.T) {
	f := &fakeNet{dir: t.TempDir(), slaveIP: "10.10.10.10", homeIP: "192.168.1.50"}
	m := f.manager()
	err := m.Apply(context.Background(), "enp1s0", spec, 1500*time.Millisecond, func(context.Context) error {
		return errors.New("node API not reachable")
	})
	if err == nil || m.Active() {
		t.Fatalf("err = %v, active %v", err, m.Active())
	}
}
