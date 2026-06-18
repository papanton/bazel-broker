package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

func TestRenderBuildsTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	RenderBuildsTable(&buf, nil)
	if got := strings.TrimSpace(buf.String()); got != "no active builds" {
		t.Fatalf("empty table: got %q", got)
	}
}

func TestRenderBuildsTable_Columns(t *testing.T) {
	var buf bytes.Buffer
	RenderBuildsTable(&buf, []api.Build{
		{
			InvocationID: "abcdef0123456789",
			Worktree:     "/Users/x/wt/feature-a",
			State:        api.StateRunning,
			ElapsedMS:    3120,
			Targets:      []string{"//app:App", "//lib:lib"},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"ID", "WORKTREE", "STATE", "ELAPSED", "TARGETS", // header
		"abcdef01",       // short id (first 8)
		"feature-a",      // worktree basename
		"running",        // state
		"3.1s",           // elapsed from elapsed_ms (server-computed)
		"//app:App (+1)", // first target + count
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
	// No CACHE column in the default view (cache_hit_ratio lives in metrics).
	if strings.Contains(out, "CACHE") {
		t.Errorf("default table should not have a CACHE column\n%s", out)
	}
}

func TestRenderBuildsTable_WorktreeNamePreferred(t *testing.T) {
	var buf bytes.Buffer
	RenderBuildsTable(&buf, []api.Build{
		{InvocationID: "x", WorktreeName: "displayname", Worktree: "/some/path", State: "running"},
	})
	if !strings.Contains(buf.String(), "displayname") {
		t.Fatalf("worktree_name should be preferred:\n%s", buf.String())
	}
}

func TestRenderJSON_Indented(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, api.BuildsResponse{Builds: []api.Build{{InvocationID: "a", Targets: []string{}}}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"builds"`) || !strings.Contains(out, `"invocation_id": "a"`) {
		t.Fatalf("json output not the expected shape:\n%s", out)
	}
}

func TestHumanizeElapsedMS(t *testing.T) {
	cases := map[int64]string{
		0:       "0ms",
		500:     "500ms",
		3120:    "3.1s",
		90000:   "1m30s",
		3700000: "1h01m",
		-5:      "0ms", // never negative
	}
	for ms, want := range cases {
		if got := humanizeElapsedMS(ms); got != want {
			t.Errorf("humanizeElapsedMS(%d): got %q want %q", ms, got, want)
		}
	}
}
