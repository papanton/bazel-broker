package bep

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/metrics"
)

//go:embed perfetto.html
var perfettoHTML string

var perfettoTmpl = template.Must(template.New("perfetto").Parse(perfettoHTML))

// perfettoShimName is the sentinel {name} on GET /profile/{id}/{name} that returns
// the postMessage shim HTML instead of the raw .gz. Routing the shim through the
// existing (token-exempt) ProfileFile seam avoids adding a new route to E2's mux.
const perfettoShimName = "perfetto"

// perfettoOrigin is the only cross-origin the profile route's CORS allows (OD-C).
const perfettoOrigin = "https://ui.perfetto.dev"

// Store is the metrics persistence subset the provider reads.
type Store interface {
	GetMetrics(invocationID string) (*metrics.Row, string, bool, error)
	ListMetrics(limit int) ([]*metrics.Row, error)
	LatestDiskReport() (DiskReportView, bool, error)
}

// DiskReportView is the disk-cache report shape the provider serves (decoupled
// from the store's concrete type so bep need not import store).
type DiskReportView struct {
	TakenAt     int64  `json:"taken_at"`
	CacheDir    string `json:"cache_dir"`
	TotalBytes  int64  `json:"total_bytes"`
	FileCount   int64  `json:"file_count"`
	OldestMtime int64  `json:"oldest_mtime"`
	GCFreed     int64  `json:"gc_freed_bytes"`
}

// Provider implements httpapi.MetricsProvider (the E4 read routes). It is wired
// via httpapi.WithMetrics(provider).
type Provider struct {
	store   Store
	log     *slog.Logger
	baseURL string // e.g. "http://127.0.0.1:8765"
	diskCfg DiskCacheConfig
	report  func() (DiskReportView, error) // on-demand disk report (set by ingest)
}

// NewProvider constructs the MetricsProvider. baseURL is the broker's loopback
// base ("http://127.0.0.1:<port>") used to build served/perfetto URLs.
func NewProvider(store Store, baseURL string, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	return &Provider{store: store, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

// ProfileURLFor returns the ready-to-open Perfetto shim URL for an invocation id.
// This is what the registry build's profile_url is set to.
func (p *Provider) ProfileURLFor(id string) string {
	return fmt.Sprintf("%s/profile/%s/%s", p.baseURL, id, perfettoShimName)
}

// --- GET /builds/{invocation_id}/metrics ---

func (p *Provider) BuildMetrics(w http.ResponseWriter, r *http.Request) {
	p.writeMetrics(w, r.PathValue("invocation_id"))
}

// --- GET /metrics  (single via ?invocation_id, else recent list) ---

func (p *Provider) MetricsList(w http.ResponseWriter, r *http.Request) {
	if id := r.URL.Query().Get("invocation_id"); id != "" {
		p.writeMetrics(w, id)
		return
	}
	rows, err := p.store.ListMetrics(50)
	if err != nil {
		p.fail(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]metricsJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, p.toJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": out})
}

func (p *Provider) writeMetrics(w http.ResponseWriter, id string) {
	if id == "" {
		p.fail(w, http.StatusBadRequest, "bad_request", "invocation_id required")
		return
	}
	row, _, ok, err := p.store.GetMetrics(id)
	if err != nil {
		p.fail(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		p.fail(w, http.StatusNotFound, "not_found", "no metrics for "+id)
		return
	}
	writeJSON(w, http.StatusOK, p.toJSON(row))
}

// --- GET /builds/{invocation_id}/profile  → api.ProfileRef {perfetto_url, local_path} ---

func (p *Provider) BuildProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("invocation_id")
	row, _, ok, err := p.store.GetMetrics(id)
	if err != nil {
		p.fail(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	local := ""
	if ok {
		local = row.ProfilePath
	}
	writeJSON(w, http.StatusOK, api.ProfileRef{
		PerfettoURL: p.ProfileURLFor(id),
		LocalPath:   local,
	})
}

// --- GET /profile/{invocation_id}/{name}  (token-exempt + Origin-restricted CORS, OD-C) ---
//
// Two behaviors keyed on {name}:
//   - {name} == "perfetto"  → the postMessage shim HTML (same-origin page)
//   - otherwise             → stream the build's --profile .gz bytes
//
// The served file path is resolved ONLY from the DB-recorded profile_path for the
// id, never from the {name} URL component (traversal-safe, R6).
func (p *Provider) ProfileFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("invocation_id")
	name := r.PathValue("name")

	// OD-C: this route is read-only + id-addressed; Perfetto fetches it cross-origin
	// and cannot present the bearer token, so we allow the Perfetto origin here. The
	// auth-exemption itself is applied at the middleware seam (see ingest wiring doc).
	if origin := r.Header.Get("Origin"); origin == perfettoOrigin {
		w.Header().Set("Access-Control-Allow-Origin", perfettoOrigin)
		w.Header().Set("Vary", "Origin")
	}

	if name == perfettoShimName {
		p.serveShim(w, id)
		return
	}

	row, _, ok, err := p.store.GetMetrics(id)
	if err != nil {
		p.fail(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok || row.ProfilePath == "" {
		p.fail(w, http.StatusNotFound, "not_found", "no profile for "+id)
		return
	}
	f, err := os.Open(row.ProfilePath)
	if err != nil {
		p.fail(w, http.StatusNotFound, "not_found", "profile file missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "inline; filename=\""+filepath.Base(row.ProfilePath)+"\"")
	http.ServeContent(w, r, filepath.Base(row.ProfilePath), modTime(row.ProfilePath), f)
}

func (p *Provider) serveShim(w http.ResponseWriter, id string) {
	profileURL, _ := json.Marshal(fmt.Sprintf("%s/profile/%s/command.profile.gz", p.baseURL, id))
	title, _ := json.Marshal("bazel " + id)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = perfettoTmpl.Execute(w, map[string]template.JS{
		"ProfileURL": template.JS(profileURL),
		"Title":      template.JS(title),
	})
}

// --- GET /diskcache ---

func (p *Provider) DiskCache(w http.ResponseWriter, r *http.Request) {
	// On-demand fresh report if a reporter is wired; else latest persisted.
	if p.report != nil {
		if rep, err := p.report(); err == nil {
			writeJSON(w, http.StatusOK, rep)
			return
		}
	}
	rep, ok, err := p.store.LatestDiskReport()
	if err != nil {
		p.fail(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, DiskReportView{CacheDir: p.diskCfg.Dir})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
