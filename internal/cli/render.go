package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/papanton/bazel-broker/internal/api"
)

// RenderJSON marshals v indented to w. It is the writer for one-shot --json
// output (ls, kill, profile) and echoes the broker's wire shape verbatim, so the
// CLI and daemon cannot drift.
func RenderJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// RenderNDJSON writes v as a single compact line (newline-terminated by Encode).
// watch --json uses this so each event is one object per line, directly
// consumable by `jq -c` and /verify.
func RenderNDJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// RenderBuildsTable writes a fixed-column, tab-aligned table. Human columns are
// intentionally lossy (short id, basename, first target); the full values always
// live in --json so scripts never parse the table. ELAPSED comes from the
// server-computed elapsed_ms — the CLI never recomputes from its own clock.
func RenderBuildsTable(w io.Writer, builds []api.Build) {
	if len(builds) == 0 {
		fmt.Fprintln(w, "no active builds")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWORKTREE\tSTATE\tELAPSED\tTARGETS")
	for _, b := range builds {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			shortID(b.InvocationID),
			worktreeLabel(b),
			b.State,
			humanizeElapsedMS(b.ElapsedMS),
			joinTargets(b.Targets),
		)
	}
	_ = tw.Flush()
}

// sortBuilds orders builds in place by start time (newest first), falling back to
// invocation_id for stability, so the watch view is deterministic. The caller
// cedes ownership of the slice (it is built fresh from the state map per event).
func sortBuilds(builds []api.Build) []api.Build {
	sort.SliceStable(builds, func(i, j int) bool {
		if builds[i].StartTime != builds[j].StartTime {
			return builds[i].StartTime > builds[j].StartTime // RFC3339 sorts lexically by time
		}
		return builds[i].InvocationID < builds[j].InvocationID
	})
	return builds
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// worktreeLabel prefers the server-provided display name, falling back to the
// basename of the worktree path.
func worktreeLabel(b api.Build) string {
	if b.WorktreeName != "" {
		return b.WorktreeName
	}
	if b.Worktree == "" {
		return "-"
	}
	return filepath.Base(b.Worktree)
}

func joinTargets(targets []string) string {
	switch len(targets) {
	case 0:
		return "-"
	case 1:
		return targets[0]
	default:
		return fmt.Sprintf("%s (+%d)", targets[0], len(targets)-1)
	}
}

// humanizeElapsedMS renders a server-computed elapsed_ms as a compact duration.
func humanizeElapsedMS(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
