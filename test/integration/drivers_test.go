package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/slave-agent/agent"
)

const rtx4090 = "pci:v000010DEd00002684sv00001043sd000088E2bc03sc00i00"

// modulesPkg is the module package the Controller should pick for this
// machine's kernel, which the agent reports (none on Windows).
func modulesPkg() string {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	flavor := "generic"
	if m := regexp.MustCompile(`^\d+\.\d+\.\d+-\d+-([a-z0-9-]+)$`).FindStringSubmatch(strings.TrimSpace(string(b))); m != nil {
		flavor = m[1]
	}
	return "linux-modules-nvidia-580-server-open-" + flavor
}

// fakeArchive stands in for archive.ubuntu.com with one driver that
// supports the RTX 4090.
func fakeArchive(t *testing.T) (*httptest.Server, func() int) {
	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	restricted := gz(`Package: nvidia-driver-580-server-open
Version: 580.178.04-0ubuntu0.24.04.1
Modaliases: nvidia(pci:v000010DEd00002684sv*sd*bc03sc*i*)

Package: nvidia-headless-no-dkms-580-server-open
Version: 580.178.04-0ubuntu0.24.04.1

Package: nvidia-utils-580-server
Version: 580.178.04-0ubuntu0.24.04.1

Package: ` + modulesPkg() + `
Version: 6.8.0-147.147
`)
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/restricted/binary-amd64/Packages.gz"):
			if strings.Contains(r.URL.Path, "/noble-updates/") {
				_, _ = w.Write(restricted)
			} else {
				_, _ = w.Write(gz(""))
			}
		case r.URL.Path == "/ubuntu/dists/noble/InRelease":
			_, _ = w.Write([]byte("signed noble release"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return hits }
}

// fakeDrivers stands in for apt: Install fetches the release file through
// the node's shim, as apt would, and Load "loads" the driver.
type fakeDrivers struct {
	mu        sync.Mutex
	installed []string
	release   string
	loaded    bool
	restarted int
}

func (f *fakeDrivers) Installed(_ context.Context, pkgs []string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.installed, " ") == strings.Join(pkgs, " ")
}

func (f *fakeDrivers) Install(ctx context.Context, spec proto.DriverSpec, base string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/dists/"+spec.Suites[0]+"/InRelease", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("archive: %s", resp.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed, f.release = spec.Packages, string(b)
	return nil
}

func (f *fakeDrivers) Load(context.Context, proto.DriverSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = true
	return "580.178.04", nil
}

func (f *fakeDrivers) Reboot() error { return fmt.Errorf("fake can't reboot") }

func (f *fakeDrivers) RestartRuntime(context.Context) error {
	f.mu.Lock()
	f.restarted++
	f.mu.Unlock()
	return nil
}

// gpus reports the card as a PCI scan, then nvidia-smi would.
func (f *fakeDrivers) gpus() []proto.GPUInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := proto.GPUInfo{Vendor: "nvidia", PCIID: "10de:2684", BusID: "0000:01:00.0", Modalias: rtx4090}
	if f.loaded {
		g.Model, g.VRAM = "NVIDIA GeForce RTX 4090", 24<<30
		g.Driver = proto.GPUDriver{Installed: true, Version: "580.178.04"}
		g.Compute = proto.GPUCompute{API: "cuda", Capability: "8.9"}
	}
	return []proto.GPUInfo{g}
}

func observedOf(t *testing.T, c *ctrl, id string) proto.ObservedState {
	var obs proto.ObservedState
	_ = json.Unmarshal(c.node(t, id).Observed, &obs)
	return obs
}

func TestGPUDriverInstall(t *testing.T) {
	archive, hits := fakeArchive(t)
	c := startController(t, core.ModeAuto, func(cfg *core.Config) { cfg.AptMirror = archive.URL + "/ubuntu" })

	fd := &fakeDrivers{}
	gpu := startSlave(t, c, func(cfg *agent.Config) { cfg.Drivers, cfg.ScanGPUs = fd, fd.gpus })
	// A node with an NVIDIA card but no way to install drivers says so.
	unable := startSlave(t, c, func(cfg *agent.Config) { cfg.ScanGPUs = (&fakeDrivers{}).gpus })
	// A discrete AMD card gets Mesa's Vulkan driver.
	amd := startSlave(t, c, func(cfg *agent.Config) {
		cfg.Drivers = &fakeDrivers{}
		cfg.ScanGPUs = func() []proto.GPUInfo {
			return []proto.GPUInfo{{Vendor: "amd", PCIID: "1002:744c", Modalias: "pci:v00001002d0000744Cbc03sc00i00"}}
		}
	})
	adoptAll(t, c, gpu, unable, amd)

	waitFor(t, 30*time.Second, "the driver to be ready", func() bool {
		d := observedOf(t, c, gpu.NodeID()).Driver
		return d != nil && d.State == proto.DriverReady
	})
	var ds proto.DesiredState
	_ = json.Unmarshal(c.node(t, gpu.NodeID()).Desired, &ds)
	if ds.Drivers == nil || ds.Drivers.Name != "nvidia-driver-580-server-open" || !ds.AllowReboot ||
		strings.Join(ds.Drivers.Packages, " ") != "nvidia-headless-no-dkms-580-server-open nvidia-utils-580-server "+modulesPkg() {
		t.Fatalf("desired drivers: %+v", ds.Drivers)
	}
	if fd.release != "signed noble release" {
		t.Fatalf("archive through the Controller: %q", fd.release)
	}
	if fd.restarted != 1 {
		t.Fatalf("runtime restarted %d times", fd.restarted)
	}
	if d := observedOf(t, c, gpu.NodeID()).Driver; d.Version != "580.178.04" {
		t.Fatalf("driver version %q", d.Version)
	}
	// The Controller now knows the card's memory, so models can go on it.
	waitFor(t, 10*time.Second, "the inventory to show the card's memory", func() bool {
		var inv proto.Inventory
		_ = json.Unmarshal(c.node(t, gpu.NodeID()).Inventory, &inv)
		return len(inv.GPUs) == 1 && inv.GPUs[0].VRAM == 24<<30 && inv.Accel.Best == "cuda"
	})

	waitFor(t, 10*time.Second, "the node without an installer to report it", func() bool {
		d := observedOf(t, c, unable.NodeID()).Driver
		return d != nil && d.State == proto.DriverFailed && strings.Contains(d.Error, "can't install drivers")
	})
	if d := ds0(t, c, amd.NodeID()); d == nil || d.Vendor != "amd" || d.Name != "mesa-vulkan-drivers" {
		t.Fatalf("AMD node's driver: %+v", d)
	}

	// The second fetch of the release file is served from the Controller's copy.
	before := hits()
	fd.mu.Lock()
	fd.installed = nil
	fd.mu.Unlock()
	if err := fd.Install(context.Background(), *ds0(t, c, gpu.NodeID()), "http://"+gpu.ShimHost()+"/apt/ubuntu"); err != nil {
		t.Fatal(err)
	}
	if hits() != before {
		t.Fatal("the archive was fetched again for a cached file")
	}
}

func ds0(t *testing.T, c *ctrl, id string) *proto.DriverSpec {
	var ds proto.DesiredState
	_ = json.Unmarshal(c.node(t, id).Desired, &ds)
	return ds.Drivers
}

// An Intel integrated GPU gets Mesa's Vulkan driver, no reboot, and the
// Settings switch reaches the node.
func TestIntegratedGPUDriver(t *testing.T) {
	archive, _ := fakeArchive(t)
	c := startController(t, core.ModeAuto, func(cfg *core.Config) { cfg.AptMirror = archive.URL + "/ubuntu" })

	fd := &fakeDrivers{}
	var mu sync.Mutex
	var specs []proto.DriverSpec
	igpu := startSlave(t, c, func(cfg *agent.Config) {
		cfg.Drivers = &recordingDrivers{fakeDrivers: fd, onLoad: func(s proto.DriverSpec) { mu.Lock(); specs = append(specs, s); mu.Unlock() }}
		cfg.ScanGPUs = func() []proto.GPUInfo {
			g := proto.GPUInfo{Vendor: "intel", PCIID: "8086:3e92", BusID: "0000:00:02.0", Integrated: true,
				Modalias: "pci:v00008086d00003E92sv0000103Csd00008767bc03sc00i00", Driver: proto.GPUDriver{Installed: true}}
			fd.mu.Lock()
			if fd.loaded {
				g.Compute = proto.GPUCompute{API: "vulkan"}
			}
			fd.mu.Unlock()
			return []proto.GPUInfo{g}
		}
	})
	adoptAll(t, c, igpu)

	waitFor(t, 30*time.Second, "the Vulkan driver to be ready", func() bool {
		d := observedOf(t, c, igpu.NodeID()).Driver
		return d != nil && d.State == proto.DriverReady
	})
	var ds proto.DesiredState
	_ = json.Unmarshal(c.node(t, igpu.NodeID()).Desired, &ds)
	if ds.Drivers == nil || ds.Drivers.Vendor != "intel" || ds.Drivers.Name != "mesa-vulkan-drivers" ||
		!ds.Drivers.UseIntegrated || ds.AllowReboot {
		t.Fatalf("desired: %+v allow_reboot=%v", ds.Drivers, ds.AllowReboot)
	}
	waitFor(t, 10*time.Second, "the inventory to show Vulkan", func() bool {
		var inv proto.Inventory
		_ = json.Unmarshal(c.node(t, igpu.NodeID()).Inventory, &inv)
		return len(inv.GPUs) == 1 && inv.GPUs[0].Compute.API == "vulkan" && inv.Accel.Best == "vulkan"
	})

	// Turning integrated GPUs off reaches the node, which applies it.
	if err := c.SetUseIntegratedGPUs(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the node to apply the switch", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(specs) >= 2 && !specs[len(specs)-1].UseIntegrated
	})
	if d := ds0(t, c, igpu.NodeID()); d == nil || d.UseIntegrated {
		t.Fatalf("desired after switching off: %+v", d)
	}
}

type recordingDrivers struct {
	*fakeDrivers
	onLoad func(proto.DriverSpec)
}

func (r *recordingDrivers) Load(ctx context.Context, s proto.DriverSpec) (string, error) {
	r.onLoad(s)
	return r.fakeDrivers.Load(ctx, s)
}
