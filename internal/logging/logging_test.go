package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewJSONOutput(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "info")
	log.Info("hello", "invocation_id", "abc123")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["invocation_id"] != "abc123" {
		t.Errorf("invocation_id = %v, want abc123", rec["invocation_id"])
	}
}

func TestLevelMapping(t *testing.T) {
	// At warn level, an Info line must be suppressed.
	var buf bytes.Buffer
	log := New(&buf, "warn")
	log.Info("suppressed")
	log.Warn("kept")
	out := buf.String()
	if strings.Contains(out, "suppressed") {
		t.Errorf("info line leaked at warn level: %s", out)
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("warn line missing: %s", out)
	}
}

func TestUnknownLevelDefaultsInfo(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "bogus")
	log.Info("shown")
	if !strings.Contains(buf.String(), "shown") {
		t.Errorf("unknown level should default to info, got: %s", buf.String())
	}
}
