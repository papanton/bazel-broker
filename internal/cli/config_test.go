package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// codeOf extracts the exit code from a (possibly nil) cliError.
func codeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ExitUsage
}

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, `{"host":"127.0.0.1","port":9000,"token":"abc","disk_cache":"/x","max_concurrency":4}`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.Token != "abc" || c.Host != "127.0.0.1" {
		t.Fatalf("decoded wrong: %+v", c)
	}
}

func TestLoadConfig_LenientUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	// Unknown future keys (db_path, log_path, profile_open) must not break loading.
	p := writeCfg(t, dir, `{"port":1,"token":"t","db_path":"/d","log_path":"/l","profile_open":"perfetto","future":42}`)
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("lenient decode should ignore unknown keys: %v", err)
	}
}

func TestLoadConfig_Missing(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if codeOf(err) != ExitConfig {
		t.Fatalf("missing config: want ExitConfig, got %v", err)
	}
}

func TestLoadConfig_Malformed(t *testing.T) {
	p := writeCfg(t, t.TempDir(), `{not json`)
	if _, err := LoadConfig(p); codeOf(err) != ExitConfig {
		t.Fatalf("malformed config: want ExitConfig, got %v", err)
	}
}

func TestLoadConfig_MissingFields(t *testing.T) {
	p := writeCfg(t, t.TempDir(), `{"port":0,"token":""}`)
	if _, err := LoadConfig(p); codeOf(err) != ExitConfig {
		t.Fatalf("missing port/token: want ExitConfig, got %v", err)
	}
}

// TestResolveConfigPath_Order asserts the resolution order MATCHES E2's
// config.ConfigPath: --config flag > $BAZEL_BROKER_CONFIG > $XDG_CONFIG_HOME >
// ~/.config. This is the single most likely silent-misconfig bug.
func TestResolveConfigPath_Order(t *testing.T) {
	t.Setenv("BAZEL_BROKER_CONFIG", "/from/env/config.json")
	t.Setenv("XDG_CONFIG_HOME", "/from/xdg")

	// 1. explicit flag wins over everything.
	if got, _ := resolveConfigPath("/flag/config.json"); got != "/flag/config.json" {
		t.Fatalf("flag should win: got %s", got)
	}
	// 2. $BAZEL_BROKER_CONFIG wins over $XDG_CONFIG_HOME.
	if got, _ := resolveConfigPath(""); got != "/from/env/config.json" {
		t.Fatalf("$BAZEL_BROKER_CONFIG should win over XDG: got %s", got)
	}
	// 3. $XDG_CONFIG_HOME wins over ~/.config when env is unset.
	t.Setenv("BAZEL_BROKER_CONFIG", "")
	want := filepath.Join("/from/xdg", "bazel-broker", "config.json")
	if got, _ := resolveConfigPath(""); got != want {
		t.Fatalf("$XDG_CONFIG_HOME should win over ~/.config: got %s want %s", got, want)
	}
}
