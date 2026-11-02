package inventory

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aicluster/common/proto"
)

// VulkanICDDir holds the Vulkan drivers' manifests (Mesa: intel_icd…,
// intel_hasvk_icd…, radeon_icd…).
var VulkanICDDir = "/usr/share/vulkan/icd.d"

// integratedVRAM: an AMD GPU reporting less dedicated memory than this is
// built into the CPU (an APU's small carve-out); discrete cards have more.
const integratedVRAM = 2 << 30

// describeGPU fills in what the kernel tells about an Intel or AMD card:
// whether it is built into the CPU, its dedicated memory, and whether
// Vulkan can use it.
func describeGPU(g *proto.GPUInfo, dev string) {
	switch g.Vendor {
	case "intel":
		// Intel's integrated graphics always sit at 00:02.0 on the root bus;
		// Arc cards sit behind a bridge.
		g.Integrated = strings.HasSuffix(g.BusID, ":00:02.0")
	case "amd":
		vram, _ := strconv.ParseUint(readFile(filepath.Join(dev, "mem_info_vram_total")), 10, 64)
		g.Integrated = vram > 0 && vram < integratedVRAM
		if !g.Integrated {
			g.VRAM = vram
		}
	default:
		return
	}
	if !g.Driver.Installed || !hasVulkanICD(g.Vendor) {
		return
	}
	if r, _ := filepath.Glob(filepath.Join(dev, "drm", "renderD*")); len(r) == 0 {
		return
	}
	g.Compute = proto.GPUCompute{API: "vulkan"}
	g.Driver.Version = "Mesa " + mesaVersion()
}

func hasVulkanICD(vendor string) bool {
	prefix := map[string]string{"intel": "intel", "amd": "radeon"}[vendor]
	m, _ := filepath.Glob(filepath.Join(VulkanICDDir, prefix+"*_icd*.json"))
	return len(m) > 0
}

func mesaVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "dpkg-query", "-W", "-f=${Version}", "mesa-vulkan-drivers").Output()
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	if i := strings.IndexAny(v, "-~+"); i > 0 {
		v = v[:i] // 25.2.8-0ubuntu0.24.04.4 → 25.2.8
	}
	return v
}

// PCIIDs is the PCI ID database (the pci.ids package on Ubuntu).
var PCIIDs = "/usr/share/misc/pci.ids"

var (
	pciOnce  sync.Once
	pciNames map[string]string // "8086:3e92" → "CometLake-S GT2 [UHD Graphics 630]"
)

// pciName looks up a GPU's model name; vendor and device are 4 hex digits.
// Only the GPU vendors' entries are kept.
func pciName(vendor, device string) string {
	pciOnce.Do(func() { pciNames = loadPCINames(PCIIDs) })
	return pciNames[strings.ToLower(vendor+":"+device)]
}

func loadPCINames(file string) map[string]string {
	names := map[string]string{}
	f, err := os.Open(file)
	if err != nil {
		return names
	}
	defer f.Close()
	keep := map[string]bool{"10de": true, "1002": true, "8086": true}
	var vendor string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "" || line[0] == '#':
		case line[0] != '\t':
			vendor = ""
			if id, _, ok := strings.Cut(line, " "); ok && keep[id] {
				vendor = id
			}
			if strings.HasPrefix(line, "C ") {
				return names // device classes follow the vendors
			}
		case vendor != "" && len(line) > 1 && line[1] != '\t':
			if id, name, ok := strings.Cut(line[1:], " "); ok {
				names[vendor+":"+strings.ToLower(id)] = strings.TrimSpace(name)
			}
		}
	}
	return names
}
