// Command broker is the headless control+observability daemon. E0 ships a stub
// that parses flags, sets up slog, and runs the /healthz-only server. E2 fleshes
// out config loading, the registry, the full API, and launchd integration.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/antoniospapantoniou/bazel-broker/internal/config"
	"github.com/antoniospapantoniou/bazel-broker/internal/httpapi"
	"github.com/antoniospapantoniou/bazel-broker/internal/logging"
	"github.com/antoniospapantoniou/bazel-broker/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	_ = flag.String("config", "", "config file path (E2 wires this to config.Load)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	log := logging.New(os.Stderr, "info") // E2: switch writer to config.LogPath
	log.Info("broker starting", "version", version.Version, "port", cfg.Port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := httpapi.New(cfg, log) // E0: /healthz only; E2 widens this constructor
	if err := srv.Run(ctx); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
