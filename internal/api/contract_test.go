package api_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/papanton/bazel-broker/internal/api"
)

// updateGolden regenerates the testdata/api/*.json fixtures from the canonical
// Go values below. Run: `go test ./internal/api -update`.
var updateGolden = flag.Bool("update", false, "regenerate golden fixtures in testdata/api")

// fixtureDir is the committed cross-epic golden-fixture directory (repo-relative).
const fixtureDir = "../../testdata/api"

// ratio is a small helper for the *float64 enrichment field.
func ratio(f float64) *float64 { return &f }

// runningBuild is the canonical "active" build used across fixtures.
func runningBuild() api.Build {
	return api.Build{
		InvocationID: "a1b2",
		Worktree:     "/wt/feature-a",
		WorktreeName: "feature-a",
		Targets:      []string{"//app:App"},
		PID:          4242,
		State:        api.StateRunning,
		StartTime:    "2026-06-17T09:41:12Z",
		ExitCode:     0,
		Source:       api.SourceRegistered,
		ElapsedMS:    3120,
	}
}

// finishedBuild is a terminal build carrying the E4 enrichment fields so clients
// see a fully populated example.
func finishedBuild() api.Build {
	return api.Build{
		InvocationID:  "c3d4",
		Worktree:      "/wt/feature-b",
		WorktreeName:  "feature-b",
		Targets:       []string{"//lib:lib", "//app:App"},
		PID:           4243,
		State:         api.StateFinished,
		StartTime:     "2026-06-17T09:30:00Z",
		EndTime:       "2026-06-17T09:33:48Z",
		ExitCode:      0,
		Source:        api.SourceRegistered,
		ElapsedMS:     228000,
		CacheHitRatio: ratio(0.87),
		ProfileURL:    "http://127.0.0.1:8765/builds/c3d4/profile",
	}
}

// fixtures maps each golden file to its canonical value. These ARE the executable
// cross-epic contract: every consumer (E6 Go, E7 JS, E8 Swift) decodes verbatim.
func fixtures() map[string]any {
	return map[string]any{
		"healthz.json": api.HealthResponse{
			Status: "ok", Builds: 1, Queued: 0, Total: 2, Version: "0.1.0", Uptime: 1423,
		},
		"builds.json": api.BuildsResponse{
			Builds: []api.Build{runningBuild(), finishedBuild()},
		},
		"build.json": api.BuildResponse{Build: runningBuild()},
		"event_snapshot.json": api.Event{
			Type:   api.EventSnapshot,
			Seq:    0,
			Builds: []api.Build{runningBuild(), finishedBuild()},
			Ts:     "2026-06-17T09:41:00Z",
		},
		"event_build.json": api.Event{
			Type:  api.EventBuild,
			Seq:   1,
			Build: ptr(runningBuild()),
			Ts:    "2026-06-17T09:41:12Z",
		},
	}
}

func ptr(b api.Build) *api.Build { return &b }

// marshalIndent renders v as the canonical 2-space-indented JSON we commit.
func marshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// TestGoldenFixtures verifies (or with -update, regenerates) every fixture from
// the real api types. This is the drift guard: a json-tag change here fails the
// test until the committed fixtures are regenerated.
func TestGoldenFixtures(t *testing.T) {
	for name, v := range fixtures() {
		path := filepath.Join(fixtureDir, name)
		want := marshalIndent(t, v)

		if *updateGolden {
			if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run `go test ./internal/api -update`): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s drifted from the api types.\n--- on disk ---\n%s\n--- from types ---\n%s",
				name, got, want)
		}
	}
}

// TestRoundTrip decodes each committed fixture through the real types and
// re-marshals it, asserting the bytes are identical. If a consumer's decoder
// agrees with this, client and daemon agree on the wire shape.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		file string
		into func() any
	}{
		{"healthz.json", func() any { return new(api.HealthResponse) }},
		{"builds.json", func() any { return new(api.BuildsResponse) }},
		{"build.json", func() any { return new(api.BuildResponse) }},
		{"event_snapshot.json", func() any { return new(api.Event) }},
		{"event_build.json", func() any { return new(api.Event) }},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, tc.file))
			if err != nil {
				t.Fatalf("read %s (run `go test ./internal/api -update`): %v", tc.file, err)
			}
			v := tc.into()
			if err := json.Unmarshal(raw, v); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.file, err)
			}
			out := marshalIndent(t, v)
			if !bytes.Equal(out, raw) {
				t.Errorf("%s did not round-trip.\n--- in ---\n%s\n--- out ---\n%s", tc.file, raw, out)
			}
		})
	}
}

// TestFrozenFieldNames asserts the exact JSON spellings consumers depend on, so
// an accidental tag rename is caught even without the fixture comparison.
func TestFrozenFieldNames(t *testing.T) {
	data, err := json.Marshal(finishedBuild())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	required := []string{
		"invocation_id", "worktree", "targets", "pid", "state",
		"start_time", "end_time", "exit_code", "source", "elapsed_ms",
		"cache_hit_ratio", "profile_url",
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			t.Errorf("missing frozen field %q in Build JSON", k)
		}
	}
	// Forbidden legacy spellings must NOT appear.
	for _, k := range []string{"id", "started_at", "cache_hit_pct", "cache_hit_rate"} {
		if _, ok := m[k]; ok {
			t.Errorf("forbidden legacy field %q present in Build JSON", k)
		}
	}
}
