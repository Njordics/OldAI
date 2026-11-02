package inventory

import (
	"os"
	"testing"

	"aicluster/common/proto"
)

func TestParseNvidiaSMI(t *testing.T) {
	gpus := []proto.GPUInfo{
		{Vendor: "intel", BusID: "0000:00:02.0"},
		{Vendor: "nvidia", BusID: "0000:01:00.0"},
		{Vendor: "nvidia", BusID: "0000:02:00.0"},
	}
	parseNvidiaSMI(gpus, "00000000:01:00.0, NVIDIA GeForce RTX 4090, 24564, 580.178.04, 8.9\nnot, a, line\n")
	g := gpus[1]
	if g.Model != "NVIDIA GeForce RTX 4090" || g.VRAM != 24564<<20 || g.Driver.Version != "580.178.04" ||
		!g.Driver.Installed || g.Compute.API != "cuda" || g.Compute.Capability != "8.9" {
		t.Fatalf("parsed %+v", g)
	}
	if gpus[0].Model != "" || gpus[2].VRAM != 0 {
		t.Fatalf("other cards changed: %+v", gpus)
	}
	inv := &proto.Inventory{GPUs: gpus}
	Summarize(inv)
	if inv.Accel.Best != "cuda" || inv.Accel.TotalVRAM != 24564<<20 || inv.Accel.GPUCount != 3 {
		t.Fatalf("summary %+v", inv.Accel)
	}
}

func TestPCINames(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/pci.ids"
	_ = os.WriteFile(file, []byte("# comment\n10de  NVIDIA Corporation\n\t2684  AD102 [GeForce RTX 4090]\n\t\t1043 88e2  sub\n8086  Intel Corporation\n\t3e92  CometLake-S GT2 [UHD Graphics 630]\n1234  Other\n\t0001  Thing\nC 00  Unclassified device\n"), 0o644)
	n := loadPCINames(file)
	if n["10de:2684"] != "AD102 [GeForce RTX 4090]" || n["8086:3e92"] != "CometLake-S GT2 [UHD Graphics 630]" || len(n) != 2 {
		t.Fatalf("names %v", n)
	}
}

func TestDescribeGPU(t *testing.T) {
	icd := t.TempDir()
	VulkanICDDir = icd
	dev := t.TempDir()
	_ = os.MkdirAll(dev+"/drm/renderD128", 0o755)

	igpu := proto.GPUInfo{Vendor: "intel", BusID: "0000:00:02.0", Driver: proto.GPUDriver{Installed: true}}
	describeGPU(&igpu, dev)
	if !igpu.Integrated || igpu.Compute.API != "" {
		t.Fatalf("without Mesa: %+v", igpu)
	}
	_ = os.WriteFile(icd+"/intel_icd.x86_64.json", []byte("{}"), 0o644)
	describeGPU(&igpu, dev)
	if igpu.Compute.API != "vulkan" {
		t.Fatalf("with Mesa: %+v", igpu)
	}
	arc := proto.GPUInfo{Vendor: "intel", BusID: "0000:03:00.0"}
	describeGPU(&arc, dev)
	if arc.Integrated {
		t.Fatal("an Arc card counted as integrated")
	}
	_ = os.WriteFile(dev+"/mem_info_vram_total", []byte("536870912\n"), 0o644)
	apu := proto.GPUInfo{Vendor: "amd", BusID: "0000:05:00.0"}
	describeGPU(&apu, dev)
	if !apu.Integrated || apu.VRAM != 0 {
		t.Fatalf("APU: %+v", apu)
	}
	_ = os.WriteFile(dev+"/mem_info_vram_total", []byte("17163091968\n"), 0o644)
	card := proto.GPUInfo{Vendor: "amd", BusID: "0000:03:00.0"}
	describeGPU(&card, dev)
	if card.Integrated || card.VRAM != 17163091968 {
		t.Fatalf("discrete AMD: %+v", card)
	}
}
