// Command broker is the headless control + observability daemon (E2). It loads
// config, opens the SQLite store, hydrates the in-memory registry, and serves the
// loopback HTTP + WebSocket API guarded by a bearer token, with graceful
// shutdown on SIGINT/SIGTERM (launchd sends SIGTERM on unload, KeepAlive
// restarts; the registry rehydrates from SQLite so /builds is continuous).
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/admission"
	"github.com/antoniospapantoniou/bazel-broker/internal/bep"
	"github.com/antoniospapantoniou/bazel-broker/internal/config"
	"github.com/antoniospapantoniou/bazel-broker/internal/discovery"
	"github.com/antoniospapantoniou/bazel-broker/internal/httpapi"
	"github.com/antoniospapantoniou/bazel-broker/internal/logging"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
	"github.com/antoniospapantoniou/bazel-broker/internal/store"
	"github.com/antoniospapantoniou/bazel-broker/internal/version"
	"github.com/antoniospapantoniou/bazel-broker/internal/web"
)

// hydrateLimit is the number of recent builds loaded from the store at boot.
const hydrateLimit = 200

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "broker:", err)
		os.Exit(1)
	}
}

func run() error {
	// --config / --version are accepted for the launchd plist and CLAUDE recipes.
	// The config *file* is resolved via $BAZEL_BROKER_CONFIG (set by the plist).
	showVersion := flag.Bool("version", false, "print version and exit")
	cfgPath := flag.String("config", "", "config file path")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return nil
	}
	if *cfgPath != "" {
		// Honor an explicit --config by setting the env the resolver reads.
		_ = os.Setenv(config.EnvConfig, *cfgPath)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log := logging.NewFile(cfg.LogPath)
	log.Info("broker starting", "version", version.Version, "port", cfg.Port, "db", cfg.DBPath)

	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	hub := registry.NewHub()
	reg := registry.New(st, hub, log)
	if err := reg.HydrateFromStore(hydrateLimit); err != nil {
		log.Error("hydrate failed", "err", err)
	}

	// E3: passive process discovery (libproc) reconciles running bazel clients
	// into the registry; the killer fronts POST /builds/{id}/kill via the seam.
	disco := discovery.NewReconciler(discovery.NewScanner(), reg, log, discovery.DefaultInterval)
	killer := discovery.NewKiller(reg, discovery.KillConfig{}, log, disco.ReconcileOnce)

	// E4: BEP ingest enriches builds with cache_hit_ratio + profile_url and serves
	// the metrics/profile (Perfetto) routes via the WithMetrics seam.
	ingest := bep.NewIngest(reg, st, bep.IngestConfig{
		BaseURL:   fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
		DiskCache: cfg.DiskCache, // DiskMaxBytes 0 => report-only disk-cache GC
	}, log)

	// E5: admission engine gates builds before they oversubscribe the machine;
	// slots are released server-side on registry terminal events (no wrapper trap).
	adAdapter := admission.NewRegistryAdapter(reg, hub)
	policy := admission.DefaultPolicy()
	if cfg.MaxConcurrency > 0 {
		policy.MaxConcurrent = cfg.MaxConcurrency // config override (0 = keep default)
	}
	engine := admission.NewEngine(policy, adAdapter)
	admitter := admission.NewAdmitter(engine)

	// E7: same-origin session store backs the web dashboard's cookie auth (OD-B).
	sessions := web.NewSessionStore(cfg.Token)

	// Always bind loopback regardless of cfg.Host (loopback-only guarantee).
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("listen 127.0.0.1:%d: %w", cfg.Port, err)
	}

	srv := httpapi.New(cfg, reg, hub, log,
		httpapi.WithVersion(version.Version),
		httpapi.WithKiller(killer),
		httpapi.WithMetrics(ingest.Provider()),
		httpapi.WithAdmitter(admitter),
		httpapi.WithBrowserAuth(sessions),
		httpapi.WithMux(func(mux *http.ServeMux) {
			if err := web.RegisterRoutes(mux, web.Deps{Sessions: sessions}); err != nil {
				log.Error("web dashboard mount failed", "err", err)
			}
		}),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go disco.Run(ctx)
	go ingest.Run(ctx)
	go engine.Run(ctx, admission.NewLoadProbe(), adAdapter)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
