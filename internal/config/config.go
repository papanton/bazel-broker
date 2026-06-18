// Package config resolves and loads the on-disk daemon configuration.
//
// E2 §2.5 is authoritative. The daemon ALWAYS binds 127.0.0.1 regardless of the
// `host` field — `host` is advisory for clients (E6/E8) so they can build the
// base URL without hard-coding the loopback address. E2 is the SOLE writer of
// config.json; consumers read it and must ignore unknown keys (lenient decode).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// EnvConfig overrides the config *file* path (used by the launchd plist and
	// the verify scripts).
	EnvConfig = "BAZEL_BROKER_CONFIG"

	appDir  = "bazel-broker"
	cfgName = "config.json"

	// DefaultPort is the fixed loopback default (E2 §4).
	DefaultPort = 8765
	// DefaultHost is advisory for clients; the daemon always binds loopback.
	DefaultHost = "127.0.0.1"
	// DefaultProfileOpen selects how E6/E8 open a profile link.
	DefaultProfileOpen = "perfetto"
)

// Config is the on-disk daemon configuration.
type Config struct {
	Host           string `json:"host"`            // advisory loopback host for clients; default 127.0.0.1
	Port           int    `json:"port"`            // loopback TCP port; default DefaultPort
	Token          string `json:"token"`           // bearer token; generated 32-byte hex if absent
	ProfileOpen    string `json:"profile_open"`    // "perfetto" | "chrome-tracing"; read by E6/E8
	DiskCache      string `json:"disk_cache"`      // shared --disk_cache path (E1/E5)
	MaxConcurrency int    `json:"max_concurrency"` // admission ceiling (E5); 0 = unlimited
	DBPath         string `json:"db_path"`         // SQLite file; default ~/.local/state/bazel-broker/broker.db
	LogPath        string `json:"log_path"`        // slog JSON sink; default ~/.local/state/bazel-broker/broker.log
}

// configDir returns the config directory: $XDG_CONFIG_HOME/bazel-broker if set,
// else ~/.config/bazel-broker.
func configDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", appDir), nil
}

// stateDir returns ~/.local/state/bazel-broker (the db/log home).
func stateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, appDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", appDir), nil
}

// ConfigPath resolves the config file path: $BAZEL_BROKER_CONFIG if set, else
// <config dir>/config.json.
func ConfigPath() (string, error) {
	if p := os.Getenv(EnvConfig); p != "" {
		return p, nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cfgName), nil
}

// Load resolves the config path, reads JSON, applies defaults, and validates. If
// the file is absent, Load writes a default config (with a freshly generated
// 32-byte hex token) and returns it — first-run bootstrap. The directory is
// created 0700 and the file 0600. Decoding is lenient: unknown keys are ignored.
func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		cfg := Default()
		if uerr := json.Unmarshal(data, cfg); uerr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, uerr)
		}
		if aerr := cfg.applyDefaults(); aerr != nil {
			return nil, aerr
		}
		if verr := cfg.Validate(); verr != nil {
			return nil, verr
		}
		return cfg, nil
	case os.IsNotExist(err):
		return bootstrap(path)
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
}

// bootstrap writes a default config with a fresh token to path and returns it.
func bootstrap(path string) (*Config, error) {
	cfg := Default()
	tok, err := generateToken()
	if err != nil {
		return nil, err
	}
	cfg.Token = tok
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := Save(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes cfg to path as pretty JSON, creating the parent dir 0700 and the
// file 0600. E2 is the sole writer of config.json.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Default returns the in-memory defaults applied before any file is read. Token
// is empty (bootstrap fills it on first run). Defaults are defined once, in
// applyDefaults.
func Default() *Config {
	c := &Config{}
	_ = c.applyDefaults() // ignore error: empty Config can only fail on stateDir, handled by callers
	return c
}

// applyDefaults fills any empty fields with their computed defaults. It is the
// single source of every default value (scalars and the state-dir paths). Called
// by Default(), after a partial file is parsed, and on bootstrap.
func (c *Config) applyDefaults() error {
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.ProfileOpen == "" {
		c.ProfileOpen = DefaultProfileOpen
	}
	if c.DBPath == "" || c.LogPath == "" {
		sd, err := stateDir()
		if err != nil {
			return err
		}
		if c.DBPath == "" {
			c.DBPath = filepath.Join(sd, "broker.db")
		}
		if c.LogPath == "" {
			c.LogPath = filepath.Join(sd, "broker.log")
		}
	}
	return nil
}

// Validate checks the invariants other code relies on.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d (want 1-65535)", c.Port)
	}
	if c.Token == "" {
		return fmt.Errorf("token must be non-empty")
	}
	if c.DBPath == "" {
		return fmt.Errorf("db_path must be set")
	}
	if c.LogPath == "" {
		return fmt.Errorf("log_path must be set")
	}
	return nil
}

// generateToken returns 32 bytes of crypto-random data as a 64-char hex string.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
