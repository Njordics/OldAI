// Package inventory collects the Slave's hardware report and live health.
// GPUs are found on the PCI bus; NVIDIA cards get their model, memory and
// compute capability from nvidia-smi once a driver is loaded.
package inventory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"aicluster/common/proto"
)

// Root prefixes every /proc and /sys path so tests can point at a fixture tree.
var Root = "/"

func path(p string) string { return filepath.Join(Root, p) }

func readTrim(p string) string {
	b, err := os.ReadFile(path(p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Collect builds a full inventory. modelDir is the disk whose free space
// matters for model storage.
func Collect(modelDir string) *proto.Inventory {
	inv := &proto.Inventory{
		V:           proto.Version,
		CollectedAt: time.Now().UTC(),
		Host:        host(),
		CPU:         cpu(),
		Memory:      proto.MemoryInfo{Total: memTotal()},
		GPUs:        pciGPUs(),
		Network:     network(),
	}
	if total, free, ok := diskUsage(modelDir); ok {
		inv.Disks = []proto.DiskInfo{{Mount: modelDir, Total: total, Free: free}}
	}
	nvidiaDetails(inv.GPUs)
	Summarize(inv)
	return inv
}

// Summarize recomputes the accelerator summary from inv.GPUs.
func Summarize(inv *proto.Inventory) {
	inv.Accel = proto.AccelSummary{Best: "cpu", GPUCount: len(inv.GPUs)}
	for _, g := range inv.GPUs {
		inv.Accel.TotalVRAM += g.VRAM
		if g.Compute.API != "" && g.Compute.API != "none" && inv.Accel.Best == "cpu" {
			inv.Accel.Best = g.Compute.API
		}
	}
}

func host() proto.HostInfo {
	h, _ := os.Hostname()
	return proto.HostInfo{
		Hostname:    h,
		OS:          osRelease("PRETTY_NAME", runtime.GOOS),
		Codename:    osRelease("VERSION_CODENAME", ""),
		Kernel:      readTrim("proc/sys/kernel/osrelease"),
		Product:     strings.TrimSpace(readTrim("sys/class/dmi/id/sys_vendor") + " " + readTrim("sys/class/dmi/id/product_name")),
		ProductUUID: readTrim("sys/class/dmi/id/product_uuid"),
	}
}

func osRelease(key, def string) string {
	f, err := os.Open(path("etc/os-release"))
	if err != nil {
		return def
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return def
}

func cpu() proto.CPUInfo {
	c := proto.CPUInfo{Threads: runtime.NumCPU(), Sockets: 1}
	f, err := os.Open(path("proc/cpuinfo"))
	if err != nil {
		c.Cores = c.Threads
		return c
	}
	defer f.Close()
	sockets := map[string]bool{}
	cores := map[string]bool{}
	var phys string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			c.Model = v
		case "vendor_id":
			switch v {
			case "GenuineIntel":
				c.Vendor = "intel"
			case "AuthenticAMD":
				c.Vendor = "amd"
			default:
				c.Vendor = strings.ToLower(v)
			}
		case "physical id":
			phys = v
			sockets[v] = true
		case "core id":
			cores[phys+"/"+v] = true
		case "flags":
			if c.Flags == nil {
				for _, fl := range strings.Fields(v) {
					if strings.HasPrefix(fl, "avx") || fl == "f16c" || fl == "fma" || strings.HasPrefix(fl, "amx") {
						c.Flags = append(c.Flags, fl)
					}
				}
			}
		}
	}
	if len(sockets) > 0 {
		c.Sockets = len(sockets)
	}
	c.Cores = len(cores)
	if c.Cores == 0 {
		c.Cores = c.Threads
	}
	return c
}

func meminfo(key string) uint64 {
	f, err := os.Open(path("proc/meminfo"))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+":"); ok {
			fs := strings.Fields(v)
			if len(fs) > 0 {
				n, _ := strconv.ParseUint(fs[0], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}

var gpuVendors = map[string]string{"0x10de": "nvidia", "0x1002": "amd", "0x8086": "intel"}

// computeDrivers are the kernel drivers that make a card usable for models;
// nouveau, for one, is not.
var computeDrivers = map[string]bool{"nvidia": true, "amdgpu": true, "i915": true, "xe": true}

// pciGPUs finds display-class PCI devices from known vendors.
func pciGPUs() []proto.GPUInfo {
	devs, _ := filepath.Glob(path("sys/bus/pci/devices/*"))
	sort.Strings(devs)
	var out []proto.GPUInfo
	for _, d := range devs {
		class := readFile(filepath.Join(d, "class"))
		if !strings.HasPrefix(class, "0x03") {
			continue
		}
		vendor, ok := gpuVendors[readFile(filepath.Join(d, "vendor"))]
		if !ok {
			continue
		}
		dev := strings.TrimPrefix(readFile(filepath.Join(d, "device")), "0x")
		vid := strings.TrimPrefix(readFile(filepath.Join(d, "vendor")), "0x")
		g := proto.GPUInfo{
			Index:    len(out),
			Vendor:   vendor,
			Model:    pciName(vid, dev),
			PCIID:    vid + ":" + dev,
			BusID:    filepath.Base(d),
			Modalias: readFile(filepath.Join(d, "modalias")),
			Driver:   proto.GPUDriver{Installed: computeDrivers[readLink(filepath.Join(d, "driver"))]},
		}
		describeGPU(&g, d)
		out = append(out, g)
	}
	return out
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readLink(p string) string {
	s, err := os.Readlink(p)
	if err != nil {
		return ""
	}
	return filepath.Base(s)
}

func network() []proto.NetIfInfo {
	ifaces, _ := net.Interfaces()
	var out []proto.NetIfInfo
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) == 0 {
			continue
		}
		// Skip virtual interfaces (bridges, veths) when sysfs says so.
		if _, err := os.Stat(path("sys/class/net/" + ifc.Name + "/device")); err != nil && runtime.GOOS == "linux" {
			continue
		}
		n := proto.NetIfInfo{Iface: ifc.Name, MAC: ifc.HardwareAddr.String()}
		n.SpeedMbps, _ = strconv.Atoi(readTrim("sys/class/net/" + ifc.Name + "/speed"))
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				n.Addrs = append(n.Addrs, a.String())
			}
		}
		out = append(out, n)
	}
	return out
}

// HWFingerprint identifies the physical machine across reinstalls (§1).
func HWFingerprint(inv *proto.Inventory) string {
	var macs []string
	for _, n := range inv.Network {
		macs = append(macs, strings.ToLower(n.MAC))
	}
	sort.Strings(macs)
	sum := sha256.Sum256([]byte(strings.ToLower(inv.Host.ProductUUID) + "\n" + strings.Join(macs, ",")))
	return hex.EncodeToString(sum[:])
}

// GPUSummary renders the short mDNS "hw" value, e.g. "nvidia:2x24G" or "cpu".
func GPUSummary(inv *proto.Inventory) string {
	if len(inv.GPUs) == 0 {
		return "cpu"
	}
	g := inv.GPUs[0]
	s := g.Vendor + ":" + strconv.Itoa(len(inv.GPUs)) + "x"
	if g.VRAM > 0 {
		return s + strconv.FormatUint(g.VRAM>>30, 10) + "G"
	}
	return s + "?"
}

func memTotal() uint64 {
	if t := meminfo("MemTotal"); t > 0 {
		return t
	}
	t, _ := platformMemory()
	return t
}
