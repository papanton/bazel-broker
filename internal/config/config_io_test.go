package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadBootstrapWritesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv(EnvConfig, path)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Token) != 64 {
		t.Fatalf("token len = %d, want 64 hex chars", len(cfg.Token))
	}
	if cfg.Port != DefaultPort || cfg.Host != DefaultHost {
		t.Fatalf("defaults wrong: %+v", cfg)
	}

	// File must exist with 0600 perms.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", info.Mode().Perm())
	}

	// Loading again reads it back identically (same token).
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Token != cfg.Token {
		t.Fatal("token changed on second Load")
	}
}

func TestLoadLenientUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv(EnvConfig, path)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	body := `{"port":9000,"token":"deadbeef","future_field":"ignored"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("lenient decode failed: %v", err)
	}
	if cfg.Port != 9000 || cfg.Token != "deadbeef" {
		t.Fatalf("parsed wrong: %+v", cfg)
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	c := &Config{Port: 70000, Token: "x", DBPath: "/d", LogPath: "/l"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected invalid port error")
	}
	c.Port = 8765
	c.Token = ""
	if err := c.Validate(); err == nil {
		t.Fatal("expected empty token error")
	}
}
