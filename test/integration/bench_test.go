package integration

import (
	"context"
	"testing"
	"time"

	"aicluster/controller/core"
	"aicluster/controller/gateway"
)

// A benchmark measures a model on a node through the gateway and keeps
// the result.
func TestBenchmark(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	adoptAll(t, c, s)
	installOn(t, c, "qwen2.5:0.5b", s)
	g := gateway.New(c.Core, gateway.Config{}, logger("gateway"))

	targets, err := g.BenchTargets(context.Background())
	if err != nil || len(targets) != 1 || targets[0].NodeID != s.NodeID() {
		t.Fatalf("targets %+v %v", targets, err)
	}
	if _, err := g.StartBench(nil, 1); err == nil {
		t.Fatal("started a benchmark with nothing to measure")
	}
	run := func(parallel int) gateway.BenchResult {
		t.Helper()
		if _, err := g.StartBench([]gateway.BenchTarget{{ModelID: "qwen2.5:0.5b", NodeID: s.NodeID()}, {ModelID: "nope:1b", NodeID: s.NodeID()}}, parallel); err != nil {
			t.Fatal(err)
		}
		if _, err := g.StartBench([]gateway.BenchTarget{{ModelID: "qwen2.5:0.5b", NodeID: s.NodeID()}}, 1); err == nil {
			t.Fatal("two benchmarks ran at once")
		}
		var j *gateway.BenchJob
		waitFor(t, 60*time.Second, "the benchmark to finish", func() bool { j = g.BenchJobNow(); return !j.Running })
		if len(j.Results) != 2 || j.Results[1].Error == "" {
			t.Fatalf("results %+v", j.Results)
		}
		return j.Results[0]
	}
	r := run(1)
	if r.Error != "" || r.GenTokens < 2 || r.GenTPS <= 0 || r.PromptTokens <= 0 || r.PromptTPS <= 0 || r.TTFTms <= 0 || r.Approx || r.Parallel != 1 {
		t.Fatalf("single: %+v", r)
	}
	r = run(0) // the node's slots
	if r.Error != "" || r.Parallel != 4 || r.TotalTPS <= 0 {
		t.Fatalf("parallel: %+v", r)
	}
	if h := g.BenchHistory(context.Background()); len(h) != 4 || h[0].ID != g.BenchJobNow().Results[1].ID {
		t.Fatalf("history %d", len(h))
	}
	if err := g.ClearBench(context.Background()); err != nil || len(g.BenchHistory(context.Background())) != 0 {
		t.Fatal("history not cleared")
	}
}
