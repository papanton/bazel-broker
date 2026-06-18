package metrics

import (
	"bufio"
	"math"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	bes "github.com/papanton/bazel-broker/internal/genproto/buildeventstream"
)

// decodeStream reads an NDJSON BEP fixture and folds every event into a Row,
// mirroring what the real dispatcher does for the metrics-relevant payloads.
func decodeStream(t *testing.T, path string) *Row {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	u := protojson.UnmarshalOptions{DiscardUnknown: true}
	row := &Row{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev bes.BuildEvent
		if err := u.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		if st := ev.GetStarted(); st != nil {
			row.InvocationID = st.GetUuid()
		}
		if bm := ev.GetBuildMetrics(); bm != nil {
			row.ExtractMetrics(bm)
		}
		if fin := ev.GetFinished(); fin != nil {
			row.HasFinish = true
			row.ExitCode = int(fin.GetExitCode().GetCode())
			row.Success = row.ExitCode == 0
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return row
}

// TestWarmIOSRatioMatchesBazelSummary is the binding acceptance check: the
// runner_count-derived disk-cache-hit count and ratio computed from a REAL warm
// iOS build's BEP must equal Bazel's own "N processes: 35 disk cache hit, 69
// internal, 4 worker" summary, i.e. 35/(35+4) = 0.8974…
func TestWarmIOSRatioMatchesBazelSummary(t *testing.T) {
	row := decodeStream(t, "testdata/bep/warm-ios.ndjson")

	if row.InvocationID == "" {
		t.Fatal("no invocation id bound from BuildStarted.uuid")
	}
	if !row.HasMetrics {
		t.Fatal("no BuildMetrics event seen")
	}
	if got := row.DiskCacheHits; got != 35 {
		t.Errorf("disk cache hits = %d, want 35 (Bazel summary)", got)
	}
	if got := row.ExecutedRunners; got != 4 {
		t.Errorf("executed runners = %d, want 4 (worker; excludes total/internal/disk-hit)", got)
	}
	if got := row.ProcessesTotal; got != 108 {
		t.Errorf("processes total = %d, want 108", got)
	}
	ratio, ok := row.CacheHitRatio()
	if !ok {
		t.Fatal("ratio not computable on a build with cacheable actions")
	}
	want := 35.0 / 39.0
	if math.Abs(ratio-want) > 1e-9 {
		t.Errorf("cache_hit_ratio = %v, want %v (35/(35+4))", ratio, want)
	}

	// Cross-check: the summary-line disk-hit count equals the runner_count one.
	// Synthesize the line Bazel printed (BEP carries it via buildToolLogs/progress;
	// we assert the parser against the known ground-truth string).
	const summary = "108 processes: 35 disk cache hit, 69 internal, 4 worker."
	parsed, ok := ParseSummaryDiskHits(summary)
	if !ok {
		t.Fatal("ParseSummaryDiskHits failed on the known summary line")
	}
	if parsed != row.DiskCacheHits {
		t.Errorf("summary-line disk hits %d != runner_count disk hits %d", parsed, row.DiskCacheHits)
	}
}

// TestFullCacheStreamYieldsNoRatio: a build with only total/internal runners
// (no executed actions, no disk hits) has an undefined ratio → CacheHitRatio
// reports ok=false so the store persists NULL.
func TestFullCacheStreamYieldsNoRatio(t *testing.T) {
	row := decodeStream(t, "testdata/bep/fullcache-ios.ndjson")
	if !row.HasMetrics {
		t.Fatal("no BuildMetrics event seen")
	}
	if row.DiskCacheHits != 0 || row.ExecutedRunners != 0 {
		t.Errorf("expected no disk hits / executed runners, got %d/%d", row.DiskCacheHits, row.ExecutedRunners)
	}
	if _, ok := row.CacheHitRatio(); ok {
		t.Error("ratio should be undefined (denominator 0) for a no-exec build")
	}
}

func TestParseSummaryDiskHits(t *testing.T) {
	cases := []struct {
		line string
		want int64
		ok   bool
	}{
		{"108 processes: 35 disk cache hit, 69 internal, 4 worker.", 35, true},
		{"1842 processes: 1678 disk cache hit, 142 darwin-sandbox.", 1678, true},
		{"5 processes: 5 internal.", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseSummaryDiskHits(c.line)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseSummaryDiskHits(%q) = (%d,%v), want (%d,%v)", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestEvaluateAlert(t *testing.T) {
	cfg := AlertConfig{} // defaults: floor 0.5, delta 0.2, minProcs 200

	if a := EvaluateAlert(0.3, 100, nil, cfg); a != "" {
		t.Errorf("tiny build should not alert, got %q", a)
	}
	if a := EvaluateAlert(0.3, 500, nil, cfg); a != AlertLowCacheHit {
		t.Errorf("low ratio no history → low_cache_hit, got %q", a)
	}
	hist := []float64{0.9, 0.92, 0.88, 0.91}
	if a := EvaluateAlert(0.55, 500, hist, cfg); a != AlertCacheBusting {
		t.Errorf("sudden drop vs ~0.9 baseline → cache_busting_suspected, got %q", a)
	}
	if a := EvaluateAlert(0.95, 500, hist, cfg); a != "" {
		t.Errorf("healthy ratio should not alert, got %q", a)
	}
}
