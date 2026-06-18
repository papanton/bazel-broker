// Package config resolves and loads the on-disk daemon configuration.
//
// E0 ships path resolution plus defaults; E2 §2.5 (authoritative) adds file IO,
// token generation on first run, and Validate(). The path/env conventions here
// are a frozen contract other epics and the launchd plist depend on.
package config

import (
	"os"
	"path/filepath"
)

const (
	// EnvConfig overrides the config *file* path (used by the launchd plist and
	// the verify scripts). Matches E2.
	EnvConfig = "BAZEL_BROKER_CONFIG"

	appDir  = "bazel-broker"
	cfgName = "config.json"

	// DefaultPort is the fixed loopback default (E2 §4). OD-5 tracks the
	// ephemeral-port + broker.json discovery alternative, which E2 has not adopted.
	DefaultPort = 8765
)

// Config is the on-disk daemon configuration. E2 §2.5 is authoritative; E0 ships
// this stub so imports resolve and the schema is stable.
type Config struct {
	Host           string `json:"host,omitempty"`  // loopback host; default 127.0.0.1
	Port           int    `json:"port"`            // loopback TCP port; default DefaultPort
	Token          string `json:"token"`           // bearer token; generated 32-byte hex if absent (E2)
	DiskCache      string `json:"disk_cache"`      // shared --disk_cache path (E1/E5)
	MaxConcurrency int    `json:"max_concurrency"` // admission ceiling (E5); 0 = unlimited
	DBPath         string `json:"db_path"`         // default ~/.local/state/bazel-broker/broker.db
	LogPath        string `json:"log_path"`        // default ~/.local/state/bazel-broker/broker.log
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

// ConfigPath resolves the config file path: $BAZEL_BROKER_CONFIG if set, else
// <config dir>/config.json. (E2 T2 adds the file IO that reads it.)
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

// Load reads/creates config.json, applies defaults, generates a token on first
// run, and validates. E0 stub: returns Default(). E2 implements the IO.
func Load() (*Config, error) {
	return Default(), nil
}

// Default returns the zero-config defaults applied before any file is read.
func Default() *Config {
	return &Config{Host: "127.0.0.1", Port: DefaultPort, MaxConcurrency: 0}
}
