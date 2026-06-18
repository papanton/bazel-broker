package bep

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanDiskCacheMatchesDu(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"x": "aaaa", "sub/y": "bbbbbb"}
	var want int64
	for rel, data := range files {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		want += int64(len(data))
	}
	total, count, _, entries, err := scanDiskCache(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if total != want || count != 2 || len(entries) != 2 {
		t.Errorf("scan = total %d count %d entries %d; want total %d count 2", total, count, len(entries), want)
	}
}

func TestPruneRespectsMinAgeAndHysteresis(t *testing.T) {
	dir := t.TempDir()
	// Two old (evictable) entries + one fresh (protected by min_age).
	mk := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		_ = os.Chtimes(p, mt, mt)
	}
	mk("old1", 100, 48*time.Hour)
	mk("old2", 100, 36*time.Hour)
	mk("fresh", 100, 1*time.Hour) // younger than 24h min_age → never deleted

	rp := newReporter(DiskCacheConfig{
		Dir: dir, MaxBytes: 150, MinAge: 24 * time.Hour, Hysteresis: 0.9, EnableDirect: true,
	}, func() bool { return false }, testLogger())

	v, err := rp.run()
	if err != nil {
		t.Fatal(err)
	}
	if v.GCFreed == 0 {
		t.Errorf("expected GC to free space (total 300 > cap 150), freed %d", v.GCFreed)
	}
	// The fresh file must survive.
	if _, err := os.Stat(filepath.Join(dir, "fresh")); err != nil {
		t.Errorf("fresh file (< min_age) was wrongly evicted: %v", err)
	}
}

func TestGCSkippedWhileBuildActive(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "big"), make([]byte, 1000), 0o644)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "big"), old, old)

	rp := newReporter(DiskCacheConfig{
		Dir: dir, MaxBytes: 1, MinAge: 24 * time.Hour, Hysteresis: 0.9, EnableDirect: true,
	}, func() bool { return true }, testLogger()) // a build is active

	v, err := rp.run()
	if err != nil {
		t.Fatal(err)
	}
	if v.GCFreed != 0 {
		t.Errorf("GC must be skipped while a build is active, freed %d", v.GCFreed)
	}
	if _, err := os.Stat(filepath.Join(dir, "big")); err != nil {
		t.Errorf("file deleted despite active build: %v", err)
	}
}

func TestReportOnlyByDefault(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "big"), make([]byte, 1000), 0o644)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "big"), old, old)

	// EnableDirect defaults to false → report-only, even over cap (D-E4-4).
	rp := newReporter(DiskCacheConfig{Dir: dir, MaxBytes: 1}, nil, testLogger())
	v, err := rp.run()
	if err != nil {
		t.Fatal(err)
	}
	if v.GCFreed != 0 {
		t.Errorf("default config must be report-only, freed %d", v.GCFreed)
	}
}
