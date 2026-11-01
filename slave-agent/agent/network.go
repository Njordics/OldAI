package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/slave-agent/netcfg"
)

// Slave network handoff (protocol §12). After adoption the Controller
// reserves an address for the interface this node reaches it through; the
// agent then switches that interface to take DHCP and DNS only from the
// Controller, verifies it still reaches the Controller, and otherwise
// rolls back. Release, or losing the Controller for netFallbackAfter, puts
// the interface back on the home network.

const (
	fileNetwork      = "network.json"
	netApplyTimeout  = 90 * time.Second
	netRetryAfter    = 10 * time.Minute
	netFallbackAfter = 10 * time.Minute
)

type netState struct {
	mu      sync.Mutex
	applied *proto.SlaveNetwork // nil = home network
	iface   string
	err     string
	retryAt time.Time
	hbOK    time.Time // last successful heartbeat
}

type savedNetwork struct {
	Iface string             `json:"iface"`
	Spec  proto.SlaveNetwork `json:"spec"`
}

func (a *Agent) initNetwork() {
	a.net.hbOK = time.Now()
	if !a.cfg.ManageNetwork || runtime.GOOS != "linux" {
		return
	}
	m := netcfg.NewManager()
	if !m.Supported() {
		a.log.Info("network not managed by systemd-networkd; the Slave network is unavailable on this node")
		return
	}
	a.netMgr = m
	if !m.Active() {
		return
	}
	var saved savedNetwork
	if err := a.readJSON(fileNetwork, &saved); err == nil {
		a.net.applied, a.net.iface = &saved.Spec, saved.Iface
	} else {
		a.net.applied = &proto.SlaveNetwork{} // installed by an earlier run; spec unknown
	}
}

func (a *Agent) noteHeartbeatOK() {
	a.net.mu.Lock()
	a.net.hbOK = time.Now()
	a.net.mu.Unlock()
}

// observedNetwork reports the interface used to reach the Controller.
// Nodes that can't switch networks report nothing, so the Controller never
// reserves them an address. Caller holds a.mu.
func (a *Agent) observedNetworkLocked() *proto.ObservedNetwork {
	if a.netMgr == nil || a.adopted == nil || len(a.adopted.Controller.Endpoints) == 0 {
		return nil
	}
	host := endpointHost(a.adopted.Controller.Endpoints[0])
	o, err := netcfg.Route(host)
	if err != nil {
		return &proto.ObservedNetwork{Error: err.Error()}
	}
	a.net.mu.Lock()
	o.OnSlaveNet = a.net.applied != nil
	o.Error = a.net.err
	a.net.mu.Unlock()
	return o
}

func endpointHost(ep string) string {
	u, err := url.Parse(ep)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (a *Agent) netLoop(ctx context.Context) {
	if a.netMgr == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.netC:
		case <-time.After(30 * time.Second):
		}
		a.reconcileNetwork(ctx)
	}
}

func (a *Agent) reconcileNetwork(ctx context.Context) {
	a.mu.RLock()
	ad := a.adopted
	want := a.desired.Network
	a.mu.RUnlock()

	a.net.mu.Lock()
	applied, iface, retryAt, hbOK := a.net.applied, a.net.iface, a.net.retryAt, a.net.hbOK
	a.net.mu.Unlock()

	switch {
	case ad == nil || want == nil:
		if applied != nil {
			a.leaveSlaveNet(ctx, "the Controller no longer uses a Slave network")
		}
	case applied != nil && *applied == *want:
		if time.Since(hbOK) > netFallbackAfter {
			a.leaveSlaveNet(ctx, fmt.Sprintf("no contact with the Controller for %s", netFallbackAfter))
			a.net.mu.Lock()
			a.net.retryAt = time.Now().Add(netRetryAfter)
			a.net.mu.Unlock()
		}
	default:
		if time.Now().Before(retryAt) {
			return
		}
		if iface == "" {
			route, err := netcfg.Route(endpointHost(ad.Controller.Endpoints[0]))
			if err != nil {
				a.setNetErr(err.Error(), true)
				return
			}
			if route.MAC != want.MAC {
				a.setNetErr(fmt.Sprintf("the reservation is for %s but this node reaches the Controller through %s (%s)", want.MAC, route.Iface, route.MAC), true)
				return
			}
			iface = route.Iface
		}
		a.log.Info("joining the slave network", "iface", iface, "address", want.Address, "controller", want.ControllerIP)
		verify := func(ctx context.Context) error {
			d := net.Dialer{Timeout: 3 * time.Second}
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(want.ControllerIP, strconv.Itoa(want.NodeAPIPort)))
			if err != nil {
				return fmt.Errorf("controller not reachable at %s: %w", want.ControllerIP, err)
			}
			return c.Close()
		}
		if err := a.netMgr.Apply(ctx, iface, *want, netApplyTimeout, verify); err != nil {
			a.log.Warn("slave network handoff failed", "err", err)
			a.setNetErr(err.Error(), true)
			return
		}
		spec := *want
		a.net.mu.Lock()
		a.net.applied, a.net.iface, a.net.err = &spec, iface, ""
		a.net.mu.Unlock()
		if err := a.writeJSON(fileNetwork, savedNetwork{Iface: iface, Spec: spec}, 0o644); err != nil {
			a.log.Error("persist network state", "err", err)
		}
		a.preferEndpoint("https://" + net.JoinHostPort(want.ControllerIP, strconv.Itoa(want.NodeAPIPort)))
		a.log.Info("on the slave network", "iface", iface, "address", want.Address)
		nudge(a.wakeHB)
	}
}

func (a *Agent) setNetErr(msg string, backoff bool) {
	a.net.mu.Lock()
	a.net.err = msg
	if backoff {
		a.net.retryAt = time.Now().Add(netRetryAfter)
	}
	a.net.mu.Unlock()
	nudge(a.wakeHB)
}

// leaveSlaveNet puts the interface back on the home network.
func (a *Agent) leaveSlaveNet(ctx context.Context, reason string) {
	if a.netMgr == nil {
		return
	}
	a.net.mu.Lock()
	iface, active := a.net.iface, a.net.applied != nil
	a.net.mu.Unlock()
	if !active && !a.netMgr.Active() {
		return
	}
	if iface == "" {
		if r, err := netcfg.Route("192.0.2.1"); err == nil { // any off-link address: the default-route interface
			iface = r.Iface
		}
	}
	a.log.Warn("leaving the slave network", "reason", reason, "iface", iface)
	if err := a.netMgr.Revert(ctx, iface, netApplyTimeout); err != nil {
		a.log.Error("returning to the home network failed", "err", err)
	}
	a.net.mu.Lock()
	a.net.applied, a.net.iface = nil, ""
	a.net.mu.Unlock()
	if err := os.Remove(filepath.Join(a.cfg.DataDir, fileNetwork)); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("remove network state", "err", err)
	}
}

// preferEndpoint puts ep first in the Controller endpoints and saves them.
func (a *Agent) preferEndpoint(ep string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ad := a.adopted
	if ad == nil {
		return
	}
	ad.Controller.Endpoints = append([]string{ep}, slices.DeleteFunc(slices.Clone(ad.Controller.Endpoints), func(s string) bool { return s == ep })...)
	if err := a.writeJSON(fileAdoption, ad.adoption, 0o644); err != nil {
		a.log.Error("persist adoption", "err", err)
	}
}
