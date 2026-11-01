// Command aicluster-agent is the Slave Agent.
//
//	aicluster-agent [run] [flags]      serve (systemd unit)
//	aicluster-agent factory-reset      console escape hatch: forget the Controller
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"aicluster/slave-agent/agent"
)

var version = "0.2.0-dev"

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("aicluster-agent", flag.ExitOnError)
	cfg := agent.Config{Version: version}
	var beaconTargets, logLevel string
	fs.StringVar(&cfg.DataDir, "data-dir", "/var/lib/aicluster", "persistent state directory")
	fs.StringVar(&cfg.ModelDir, "model-dir", "/var/lib/aicluster/models", "model storage directory")
	fs.StringVar(&cfg.Listen, "listen", ":7450", "agent TLS listen address")
	fs.BoolVar(&cfg.Pairing, "pairing", false, "require the console pairing code for adoption")
	fs.StringVar(&cfg.PairingCodeFile, "pairing-code-file", "/run/aicluster/pairing-code", "where the console screen reads the pairing code")
	fs.BoolVar(&cfg.MDNS, "mdns", true, "announce via mDNS")
	fs.IntVar(&cfg.BeaconPort, "beacon-port", 47800, "UDP beacon port")
	fs.StringVar(&beaconTargets, "beacon-targets", "", "comma-separated host:port beacon destinations (default: broadcast)")
	fs.StringVar(&cfg.ControllerBeaconListen, "controller-beacon-listen", ":47800", "address for receiving controller beacons (empty disables)")
	fs.StringVar(&logLevel, "log-level", "info", "debug|info|warn|error")
	fs.StringVar(&cfg.HWFingerprint, "hw-fingerprint", "", "override the hardware fingerprint (only for running several agents on one machine)")
	fs.StringVar(&cfg.Runtime, "runtime", "ollama", "inference runtime: ollama | fake (dev: real pulls, no inference) | none")
	fs.StringVar(&cfg.OllamaURL, "ollama-url", "http://127.0.0.1:11434", "local Ollama API")
	fs.StringVar(&cfg.RegistryShimListen, "registry-shim-listen", "127.0.0.1:7452", "loopback address the runtime pulls models from")
	fs.StringVar(&cfg.FakeLabel, "fake-label", "", "name the fake runtime uses for this node in its replies")
	fs.IntVar(&cfg.InferenceSlots, "inference-slots", 4, "requests the runtime serves at once (match OLLAMA_NUM_PARALLEL)")
	fs.StringVar(&cfg.LlamaCppDir, "llamacpp-dir", "/usr/lib/aicluster/llama.cpp", "directory with llama.cpp's ggml-rpc-server and llama-server (pooled models)")
	var fakeMBps int64
	fs.Int64Var(&fakeMBps, "fake-mbps", 0, "throttle the fake runtime's pulls (MB/s, 0 = unlimited)")
	_ = fs.Parse(args)
	if beaconTargets != "" {
		cfg.BeaconTargets = strings.Split(beaconTargets, ",")
	}
	cfg.FakeBytesPerSec = fakeMBps << 20
	if err := os.MkdirAll(cfg.ModelDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(logLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))

	switch cmd {
	case "run":
		a, err := agent.New(cfg, log)
		if err != nil {
			log.Error("init failed", "err", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := a.Run(ctx); err != nil {
			log.Error("agent stopped", "err", err)
			os.Exit(1)
		}
	case "factory-reset":
		if err := agent.FactoryReset(cfg, log); err != nil {
			log.Error("factory reset failed", "err", err)
			os.Exit(1)
		}
		fmt.Println("factory reset done; restart aicluster-agent")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}
