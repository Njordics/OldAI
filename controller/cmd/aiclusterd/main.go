// Command aiclusterd is the Controller daemon. In build step 2 one process
// runs discovery, adoption, the node API and the management API; they split
// into separate systemd units once the gateway and provisioner exist.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"aicluster/controller/core"
	"aicluster/controller/web"
)

var version = "0.2.0-dev"

func main() {
	cfg := core.Config{Version: version}
	var mgmtListen, advertise, beaconTargets, logLevel string
	flag.StringVar(&cfg.DataDir, "data-dir", "/var/lib/aicluster/controller", "persistent state directory")
	flag.StringVar(&cfg.NodeAPIListen, "nodeapi-listen", ":7443", "Slave-facing mTLS API")
	flag.StringVar(&mgmtListen, "mgmt-listen", ":8090", "management API (CLI, dashboard)")
	flag.StringVar(&advertise, "advertise", "", "comma-separated node-API URLs given to Slaves (default: all local IPv4s)")
	flag.StringVar(&cfg.BeaconListen, "beacon-listen", ":47800", "UDP beacon listen address (empty disables)")
	flag.IntVar(&cfg.BeaconPort, "beacon-port", 47800, "UDP beacon port for controller announcements")
	flag.StringVar(&beaconTargets, "beacon-targets", "", "comma-separated host:port beacon destinations (default: broadcast)")
	flag.BoolVar(&cfg.MDNS, "mdns", true, "use mDNS discovery and announcement")
	flag.StringVar(&cfg.DefaultAdoptionMode, "adoption-mode", "auto", "initial adoption mode: auto|approve (later changes are stored in the DB)")
	flag.StringVar(&cfg.ProvisionerAddr, "provisioner", "unix:/run/aicluster/provisioner.sock", "provisioner address (unix:/path or tcp:host:port)")
	flag.StringVar(&cfg.ModelDir, "model-dir", "", "model cache directory (default <data-dir>/models)")
	flag.StringVar(&cfg.RegistryUpstream, "registry-upstream", "https://registry.ollama.ai", "where the Controller downloads models from")
	flag.DurationVar(&cfg.ModelGCGrace, "model-gc-grace", 24*time.Hour, "how long Slaves keep unassigned models on disk")
	flag.StringVar(&logLevel, "log-level", "info", "debug|info|warn|error")
	flag.Parse()
	if advertise != "" {
		cfg.Advertise = strings.Split(advertise, ",")
	}
	if beaconTargets != "" {
		cfg.BeaconTargets = strings.Split(beaconTargets, ",")
	}

	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(logLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))

	c, err := core.New(cfg, log)
	if err != nil {
		log.Error("init failed", "err", err)
		os.Exit(1)
	}
	defer c.Close()
	mgmtLn, err := net.Listen("tcp", mgmtListen)
	if err != nil {
		log.Error("mgmt listen failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.Run(ctx) })
	g.Go(func() error { return web.New(c, log).Serve(ctx, mgmtLn) })
	if err := g.Wait(); err != nil {
		log.Error("controller stopped", "err", err)
		os.Exit(1)
	}
}
