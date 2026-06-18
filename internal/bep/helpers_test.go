package bep

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	bes "github.com/papanton/bazel-broker/internal/genproto/buildeventstream"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// lastUUID returns the BuildStarted.uuid in an NDJSON BEP byte stream.
func lastUUID(t *testing.T, data []byte) string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	u := protojson.UnmarshalOptions{DiscardUnknown: true}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev bes.BuildEvent
		if err := u.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if st := ev.GetStarted(); st != nil {
			return st.GetUuid()
		}
	}
	t.Fatal("no BuildStarted.uuid in stream")
	return ""
}
