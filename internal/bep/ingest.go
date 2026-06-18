package bep

import (
	"context"
	"log/slog"
	"time"

	"github.com/papanton/bazel-broker/internal/api"
	"github.com/papanton/bazel-broker/internal/build"
	"github.com/papanton/bazel-broker/internal/metrics"
	"github.com/papanton/bazel-broker/internal/store"
)

// FullRegistry is the registry surface the ingester uses: the BEP dispatch
// enrichment seams (Upsert/FindByInvocationID) plus Snapshot for the supervisor
// and Broadcast for the optional additive `metrics`/`alert` WS events. Satisfied
// by *registry.Registry.
type FullRegistry interface {
	Registry
	Snapshot() []*build.Build
	Broadcast(ev api.Event)
}

// Ingest is the self-contained E4 supervisor + MetricsProvider. main.go wires it
// with three lines (see package doc / E4 report):
//
//	ingest := bep.NewIngest(reg, st, cfg, log)
//	srv := httpapi.New(cfg, reg, hub, log, httpapi.WithMetrics(ingest.Provider()), …)
//	go ingest.Run(ctx)
type Ingest struct {
	mgr      *Manager
	provider *Provider
	reporter *reporter
	store    *store.Store
	reg      FullRegistry
	log      *slog.Logger
	diskCfg  DiskCacheConfig
}

// IngestConfig is the minimal config the ingester needs (a subset of the daemon
// config, passed explicitly to keep bep free of config import cycles).
type IngestConfig struct {
	BaseURL      string        // "http://127.0.0.1:<port>"
	DiskCache    string        // shared --disk_cache dir (config.disk_cache)
	DiskMaxBytes int64         // 0 = report-only (default)
	ReportEvery  time.Duration // 0 = 15m default
}

// NewIngest constructs the ingester. reg must be the live registry; st the store.
func NewIngest(reg FullRegistry, st *store.Store, cfg IngestConfig, log *slog.Logger) *Ingest {
	if log == nil {
		log = slog.Default()
	}
	diskCfg := DiskCacheConfig{
		Dir:         cfg.DiskCache,
		ReportEvery: cfg.ReportEvery,
		MaxBytes:    cfg.DiskMaxBytes,
		// EnableDirect stays false: report-only by default (D-E4-4).
	}.withDefaults()

	prov := NewProvider(storeAdapter{st}, cfg.BaseURL, log)
	prov.diskCfg = diskCfg

	ing := &Ingest{
		store:   st,
		reg:     reg,
		log:     log,
		diskCfg: diskCfg,
	}
	ing.provider = prov
	ing.mgr = NewManager(reg, st, log, prov.ProfileURLFor, ing.onFinalize)
	ing.reporter = newReporter(diskCfg, ing.anyBuildActive, log)

	// On-demand disk report for GET /diskcache (fresh scan, persisted).
	prov.report = func() (DiskReportView, error) {
		v, err := ing.reporter.run()
		if err == nil {
			ing.persistReport(v)
		}
		return v, err
	}
	return ing
}

// Provider returns the MetricsProvider to pass to httpapi.WithMetrics.
func (i *Ingest) Provider() *Provider { return i.provider }

// Run starts the BEP tailing supervisor and the periodic disk-cache report. It
// blocks until ctx is cancelled.
func (i *Ingest) Run(ctx context.Context) {
	go i.runDiskReports(ctx)
	i.mgr.Run(ctx, i.reg) // blocks
}

func (i *Ingest) runDiskReports(ctx context.Context) {
	if i.diskCfg.Dir == "" {
		return
	}
	ticker := time.NewTicker(i.diskCfg.ReportEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if v, err := i.reporter.run(); err == nil {
				i.persistReport(v)
			} else {
				i.log.Debug("disk report failed", "err", err)
			}
		}
	}
}

func (i *Ingest) persistReport(v DiskReportView) {
	if err := i.store.InsertDiskReport(store.DiskReport{
		TakenAt: v.TakenAt, CacheDir: v.CacheDir, TotalBytes: v.TotalBytes,
		FileCount: v.FileCount, OldestMtime: v.OldestMtime, GCFreed: v.GCFreed,
	}); err != nil {
		i.log.Debug("persist disk report failed", "err", err)
	}
}

// anyBuildActive reports whether any non-terminal build targets the shared cache
// (R4 GC safety gate). Conservative: returns true if ANY build is active, since
// every worktree shares the same --disk_cache.
func (i *Ingest) anyBuildActive() bool {
	for _, b := range i.reg.Snapshot() {
		if !b.State.IsTerminal() {
			return true
		}
	}
	return false
}

// onFinalize emits the optional additive WS `metrics` (+ `alert`) events when a
// build's metrics row is finalized. These are PURELY ADDITIVE — the two frozen
// live WS types (snapshot/build) are untouched; clients that don't understand
// these reserved types ignore them (api.EventMetrics/EventAlert are pre-declared).
func (i *Ingest) onFinalize(r *metrics.Row) {
	if i.reg == nil {
		return
	}
	dto := i.provider.toJSON(r)
	i.reg.Broadcast(api.Event{
		Type:    api.EventMetrics,
		Ts:      api.FormatTime(time.Now()),
		Metrics: dto,
	})
	if r.Alert != "" {
		i.reg.Broadcast(api.Event{
			Type:  api.EventAlert,
			Ts:    api.FormatTime(time.Now()),
			Alert: &api.AlertEvent{InvocationID: r.InvocationID, Worktree: r.Worktree, Alert: r.Alert},
		})
	}
}

// storeAdapter bridges *store.Store to the Provider's Store interface (mapping the
// store's concrete DiskReport to the provider's DiskReportView).
type storeAdapter struct{ st *store.Store }

func (a storeAdapter) GetMetrics(id string) (*metrics.Row, string, bool, error) {
	return a.st.GetMetrics(id)
}
func (a storeAdapter) ListMetrics(limit int) ([]*metrics.Row, error) { return a.st.ListMetrics(limit) }
func (a storeAdapter) LatestDiskReport() (DiskReportView, bool, error) {
	r, ok, err := a.st.LatestDiskReport()
	if err != nil || !ok {
		return DiskReportView{}, ok, err
	}
	return DiskReportView{
		TakenAt: r.TakenAt, CacheDir: r.CacheDir, TotalBytes: r.TotalBytes,
		FileCount: r.FileCount, OldestMtime: r.OldestMtime, GCFreed: r.GCFreed,
	}, true, nil
}
