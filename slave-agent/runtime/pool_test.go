package runtime

import (
	"reflect"
	"strings"
	"testing"
)

const listOutput = `ggml_cuda_init: found 1 CUDA devices:
  Device 0: NVIDIA GeForce RTX 3090, compute capability 8.6
Available devices:
  CUDA0: NVIDIA GeForce RTX 3090 (24135 MiB, 23700 MiB free)
  RPC[127.0.0.1:41001]: RPC[127.0.0.1:41001] (16384 MiB, 16000 MiB free)
  RPC[127.0.0.1:41002]: RPC[127.0.0.1:41002] (16384 MiB, 16000 MiB free)
`

func TestParseDevices(t *testing.T) {
	got := ParseDevices(listOutput)
	want := []string{"CUDA0", "RPC[127.0.0.1:41001]", "RPC[127.0.0.1:41002]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devices = %v", got)
	}
	if d := ParseDevices("no devices here"); len(d) != 0 {
		t.Fatalf("parsed %v from junk", d)
	}
}

func arg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestLeaderArgsWithLocalGPU(t *testing.T) {
	spec := LeaderSpec{ModelPath: "/m.gguf", ModelName: "llama3.1:70b", LocalShare: 0.4, Layers: 80, Parallel: 2,
		Workers: []PoolWorkerRef{{Addr: "127.0.0.1:41001", Share: 0.35}, {Addr: "127.0.0.1:41002", Share: 0.25}}}
	args := LeaderArgs(spec, ParseDevices(listOutput), 9000)
	if arg(args, "--tensor-split") != "0.400,0.350,0.250" {
		t.Fatalf("split follows device order: %v", args)
	}
	if arg(args, "-ngl") != "999" || arg(args, "--rpc") != "127.0.0.1:41001,127.0.0.1:41002" || arg(args, "--host") != "127.0.0.1" {
		t.Fatalf("args = %v", args)
	}
}

func TestLeaderArgsCPUOnlyLeaderKeepsItsShareOnCPU(t *testing.T) {
	spec := LeaderSpec{ModelPath: "/m.gguf", ModelName: "m", LocalShare: 0.25, Layers: 80,
		Workers: []PoolWorkerRef{{Addr: "127.0.0.1:41001", Share: 0.75}}}
	devs := ParseDevices("Available devices:\n  RPC[127.0.0.1:41001]: RPC (1 MiB, 1 MiB free)\n")
	args := LeaderArgs(spec, devs, 9000)
	if arg(args, "-ngl") != "60" || arg(args, "--tensor-split") != "0.750" {
		t.Fatalf("args = %s", strings.Join(args, " "))
	}
}
