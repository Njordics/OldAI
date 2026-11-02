package inventory

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"aicluster/common/proto"
)

// NvidiaSMI is the nvidia-smi command (tests point it elsewhere).
var NvidiaSMI = "nvidia-smi"

// nvidiaDetails fills in NVIDIA cards' model, memory, driver version and
// compute capability. Without a loaded driver nvidia-smi fails and the
// cards stay as the PCI scan found them.
func nvidiaDetails(gpus []proto.GPUInfo) {
	has := false
	for _, g := range gpus {
		has = has || g.Vendor == "nvidia"
	}
	if !has {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, NvidiaSMI, "--query-gpu=pci.bus_id,name,memory.total,driver_version,compute_cap",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return
	}
	parseNvidiaSMI(gpus, string(out))
}

func parseNvidiaSMI(gpus []proto.GPUInfo, out string) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		// nvidia-smi pads the PCI domain to 8 digits: 00000000:01:00.0.
		bus := strings.ToLower(f[0])
		if len(bus) > 12 {
			bus = bus[len(bus)-12:]
		}
		for i := range gpus {
			g := &gpus[i]
			if g.Vendor != "nvidia" || !strings.EqualFold(g.BusID, bus) {
				continue
			}
			g.Model = f[1]
			if mib, err := strconv.ParseUint(f[2], 10, 64); err == nil {
				g.VRAM = mib << 20
			}
			g.Driver = proto.GPUDriver{Installed: true, Version: f[3]}
			g.Compute = proto.GPUCompute{API: "cuda", Capability: f[4]}
		}
	}
}
