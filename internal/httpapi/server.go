// Package httpapi serves the broker's HTTP/WS API. E0 ships a /healthz-only
// stub; E2 §2.6 widens the constructor (registry, hub) and adds the
// builds/events/register routes plus bearer auth.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/antoniospapantoniou/bazel-broker/internal/config"
)

// Server is the broker's HTTP front. E2 widens New to New(cfg, reg, hub, log);
// since httpapi is internal/ and both are E2-boundary code, that is a same-PR
// change, not a cross-epic break.
type Server struct {
	cfg *config.Config
	log *slog.Logger
}

// New constructs the E0 stub server.
func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

// Run binds the loopback listener and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz) // E2 gates other routes behind bearer auth

	host := s.cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return err
	}
	// E2: write resolved addr+token to the discovery file (OD-5); E0 just logs it.
	s.log.Info("listening", "addr", ln.Addr().String())

	hs := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		hs.Close()
	}()
	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// healthz returns a strict subset of E2's wire.HealthResponse so the E0 verify
// probe and E2 stay compatible.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"builds": 0,
		"queued": 0,
		"total":  0,
	})
}
