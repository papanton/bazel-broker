package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
	"github.com/antoniospapantoniou/bazel-broker/internal/build"
	"github.com/antoniospapantoniou/bazel-broker/internal/registry"
)

// serveKill mounts the Killer on a mux exactly as httpapi.WithKiller would route it, so
// the PathValue("invocation_id") extraction is exercised end to end.
func serveKill(k *Killer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /builds/{invocation_id}/kill", k.Kill)
	return mux
}

func TestKillerHandlerKillsRunningProc(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	time.Sleep(100 * time.Millisecond)

	reg := registry.New(nil, nil, nil)
	reg.Upsert(&build.Build{
		PID:      pid,
		Worktree: "/tmp/wt",
		Source:   build.SourceDiscovered,
		State:    build.StateRunning,
	})
	id := reg.Snapshot()[0].InvocationID // synthesized "pid-<pid>"

	k := NewKiller(reg, KillConfig{}, nil, nil)
	srv := httptest.NewServer(serveKill(k))
	defer srv.Close()

	// Force so a plain sleep (no graceful trap) is reaped fast and deterministically.
	resp, err := http.Post(srv.URL+"/builds/"+id+"/kill", "application/json",
		strings.NewReader(`{"force":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var res api.KillResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if !res.Killed || res.PID != pid {
		t.Errorf("bad result: %+v", res)
	}
	if res.Outcome != string(OutcomeSIGKILL) {
		t.Errorf("outcome = %q, want sigkill", res.Outcome)
	}

	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("process did not exit after kill")
	}
}

func TestKillerHandlerNotFound(t *testing.T) {
	reg := registry.New(nil, nil, nil)
	k := NewKiller(reg, KillConfig{}, nil, nil)
	srv := httptest.NewServer(serveKill(k))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/builds/nope/kill", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
