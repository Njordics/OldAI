// Package netcfg moves a Slave's interface onto the Controller's Slave
// network and back, through systemd-networkd (Ubuntu Server's netplan
// backend). The Slave network shares the wire with the home LAN, so the
// interface keeps DHCP but accepts leases only from the Controller.
package netcfg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aicluster/common/proto"
)

// FileName sorts before netplan's "10-netplan-*.network", so networkd
// matches it first; deleting it brings netplan's home-network config back.
const FileName = "05-aicluster-slave.network"

// Render is the networkd config for the Slave network on the interface
// with spec.MAC.
func Render(spec proto.SlaveNetwork) string {
	return fmt.Sprintf(`# Written by aicluster-agent: this interface is on the AI Cluster's Slave
# network %[1]s. The Controller (%[2]s) is its only DHCP and DNS server.
# To return to the home network: delete this file, then
# "networkctl reload && networkctl reconfigure <interface>".
[Match]
MACAddress=%[3]s

[Network]
DHCP=ipv4
LinkLocalAddressing=no
IPv6AcceptRA=no
Domains=%[4]s

[DHCPv4]
# Leases from the home router are ignored; only the Controller's count.
AllowList=%[2]s
ClientIdentifier=mac
UseDNS=yes
UseDomains=yes
SendHostname=yes
`, spec.Subnet, spec.ControllerIP, strings.ToLower(spec.MAC), spec.Domain)
}

// Manager applies and reverts the config. Run executes networkctl; tests
// replace it.
type Manager struct {
	Dir string // /etc/systemd/network
	Run func(ctx context.Context, name string, args ...string) error
	// Addrs lists an interface's IPv4 addresses (tests replace it).
	Addrs func(iface string) []string
}

func NewManager() *Manager {
	return &Manager{
		Dir: "/etc/systemd/network",
		Run: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		Addrs: IPv4Addrs,
	}
}

func (m *Manager) path() string { return filepath.Join(m.Dir, FileName) }

// Supported reports whether this machine is managed by systemd-networkd.
func (m *Manager) Supported() bool {
	if _, err := os.Stat(m.Dir); err != nil {
		return false
	}
	_, err := os.Stat("/run/systemd/netif")
	return err == nil
}

// Active reports whether the Slave-network config is installed.
func (m *Manager) Active() bool {
	_, err := os.Stat(m.path())
	return err == nil
}

func (m *Manager) reconfigure(ctx context.Context, iface string) error {
	if err := m.Run(ctx, "networkctl", "reload"); err != nil {
		return err
	}
	return m.Run(ctx, "networkctl", "reconfigure", iface)
}

// Apply installs the config, waits until iface holds spec.Address and
// verify passes, and otherwise restores the previous config.
func (m *Manager) Apply(ctx context.Context, iface string, spec proto.SlaveNetwork, timeout time.Duration, verify func(context.Context) error) error {
	if err := os.WriteFile(m.path()+".tmp", []byte(Render(spec)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(m.path()+".tmp", m.path()); err != nil {
		return err
	}
	err := m.reconfigure(ctx, iface)
	if err == nil {
		err = m.wait(ctx, timeout, func() error {
			if !contains(m.Addrs(iface), spec.Address) {
				return fmt.Errorf("no lease for %s from %s on %s", spec.Address, spec.ControllerIP, iface)
			}
			return verify(ctx)
		})
	}
	if err != nil {
		if rerr := m.Revert(ctx, iface, timeout); rerr != nil {
			return fmt.Errorf("%w (and rolling back failed: %v)", err, rerr)
		}
		return fmt.Errorf("%w; rolled back to the previous network", err)
	}
	return nil
}

// Revert removes the config and waits for iface to get an address from
// its normal (home network) config.
func (m *Manager) Revert(ctx context.Context, iface string, timeout time.Duration) error {
	if err := os.Remove(m.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := m.reconfigure(ctx, iface); err != nil {
		return err
	}
	return m.wait(ctx, timeout, func() error {
		if len(m.Addrs(iface)) == 0 {
			return fmt.Errorf("%s got no address back", iface)
		}
		return nil
	})
}

func (m *Manager) wait(ctx context.Context, timeout time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// IPv4Addrs lists an interface's IPv4 addresses.
func IPv4Addrs(iface string) []string {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return nil
	}
	addrs, _ := ifc.Addrs()
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// Route finds the interface (and its MAC and address) this machine uses to
// reach host, e.g. the Controller.
func Route(host string) (*proto.ObservedNetwork, error) {
	c, err := net.Dial("udp4", net.JoinHostPort(host, "9")) // no packet is sent
	if err != nil {
		return nil, err
	}
	local := c.LocalAddr().(*net.UDPAddr).IP
	c.Close()
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local) {
				return &proto.ObservedNetwork{Iface: ifc.Name, MAC: strings.ToLower(ifc.HardwareAddr.String()), Address: local.String()}, nil
			}
		}
	}
	return nil, fmt.Errorf("no interface holds %s", local)
}
