package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/config"
)

// GlobalOpts carries the resolved persistent (root) flags. Each command turns it
// into a *Client via NewClient.
type GlobalOpts struct {
	JSON       bool
	ConfigPath string
	Port       int
	Token      string
	Timeout    time.Duration
}

// Config mirrors only the subset of E2's config.json that brokerctl consumes
// (host/port/token). Decoding is LENIENT (E2 owns the file and adds keys per epic:
// disk_cache, max_concurrency, db_path, log_path, profile_open) so an unknown key
// never breaks the CLI.
type Config struct {
	Host  string `json:"host"`  // advisory loopback host; default 127.0.0.1
	Port  int    `json:"port"`  // loopback TCP port
	Token string `json:"token"` // bearer token
}

// resolveConfigPath adds the CLI-only --config override on top of E2's shared
// config.ConfigPath (which already resolves $BAZEL_BROKER_CONFIG > $XDG_CONFIG_HOME
// > ~/.config). Delegating means the CLI and daemon can never disagree on which
// file is authoritative:
//
//	--config flag  >  [ config.ConfigPath: $BAZEL_BROKER_CONFIG > $XDG > ~/.config ]
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return config.ConfigPath()
}

// LoadConfig resolves the path then reads + validates. Missing file / parse
// failure / missing port|token all map to ExitConfig. E6 NEVER writes the file.
func LoadConfig(explicitPath string) (*Config, error) {
	path, err := resolveConfigPath(explicitPath)
	if err != nil {
		return nil, wrap(ExitConfig, "resolve config path: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, wrap(ExitConfig,
				"no config at %s — is the broker installed/running? (the daemon writes this file on first run)", path)
		}
		return nil, wrap(ExitConfig, "read config %s: %v", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil { // lenient: no DisallowUnknownFields
		return nil, wrap(ExitConfig, "parse config %s: %v", path, err)
	}
	if c.Port == 0 || c.Token == "" {
		return nil, wrap(ExitConfig, "config %s missing port or token", path)
	}
	return &c, nil
}
