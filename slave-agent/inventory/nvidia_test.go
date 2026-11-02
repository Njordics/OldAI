package inventory

import (
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
