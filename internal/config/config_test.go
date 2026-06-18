package config

import (
	"path/filepath"
	"testing"
)

func TestConfigPathEnvOverride(t *testing.T) {
	want := "/tmp/custom/config.json"
	t.Setenv(EnvConfig, want)
	got, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigPathXDGFallback(t *testing.T) {
	t.Setenv(EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/xdg", appDir, cfgName)
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestDefault(t *testing.T) {
	d := Default()
	if d.Port != DefaultPort {
		t.Fatalf("Default().Port = %d, want %d", d.Port, DefaultPort)
	}
	if d.MaxConcurrency != 0 {
		t.Fatalf("Default().MaxConcurrency = %d, want 0", d.MaxConcurrency)
	}
}
