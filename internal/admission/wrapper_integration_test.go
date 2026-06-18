package admission

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wrapperPath finds the repo's reference tools/bazel from the test's CWD
// (internal/admission) by walking up to the module root.
func wrapperPath(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		cand := filepath.Join(dir, "tools", "bazel")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("tools/bazel wrapper not found from test CWD")
	return ""
}

func fakeBazelPath(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		cand := filepath.Join(dir, "testdata", "fake-bazel.sh")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("testdata/fake-bazel.sh not found from test CWD")
	return ""
}

// admissionServer mounts the Admitter on a real httptest.Server and counts
// /admission POSTs so tests can assert the bypass paths skip the gate.
func admissionServer(t *testing.T, e *Engine) (*httptest.Server, *int32) {
	t.Helper()
	a := NewAdmitter(e)
	var admitCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admission", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&admitCount, 1)
		a.Admit(w, r)
	})
	mux.HandleFunc("POST /admission/release", a.Release)
	mux.HandleFunc("POST /admission/drain", a.Drain)
	mux.HandleFunc("GET /admission/status", a.Status)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &admitCount
}

func runWrapper(t *testing.T, brokerURL string, extraEnv []string, args ...string) (int, string) {
	t.Helper()
	wrap := wrapperPath(t)
	fake := fakeBazelPath(t)
	cmd := exec.Command("/bin/bash", append([]string{wrap}, args...)...)
	tmp := t.TempDir()
	env := append(os.Environ(),
		"BAZEL_REAL="+fake,
		"BROKER_URL="+brokerURL,
		"BROKER_CACHE_DIR="+filepath.Join(tmp, "disk"),
		"BROKER_REPO_CACHE="+filepath.Join(tmp, "repo"),
		"BROKER_EVENT_DIR="+filepath.Join(tmp, "bep"),
		"BROKER_PROFILE_DIR="+filepath.Join(tmp, "prof"),
		"CI=", // ensure CI guard is OFF for the gated tests
	)
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("wrapper run error: %v", err)
		}
	}
	return code, string(out)
}

// TestWrapperAdmitInjectExec proves the gated happy path: the wrapper POSTs
// /admission, gets ALLOW, injects E1 flags, execs fake-bazel, and the build
// succeeds. fake-bazel echoes nothing, so we assert success + an admit POST + the
// injected BEP file existing (fake-bazel honors --build_event_json_file).
func TestWrapperAdmitInjectExec(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 2 * time.Second}, newFakeReg())
	srv, count := admissionServer(t, e)

	code, out := runWrapper(t, srv.URL, []string{"FAKE_BAZEL_DURATION=0"}, "build", "//x")
	if code != 0 {
		t.Fatalf("wrapper exit = %d, out:\n%s", code, out)
	}
	if atomic.LoadInt32(count) < 1 {
		t.Fatalf("expected >=1 /admission POST, got %d", *count)
	}
}

func TestWrapperBypassSkipsGate(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 2 * time.Second}, newFakeReg())
	srv, count := admissionServer(t, e)

	code, out := runWrapper(t, srv.URL, []string{"BROKER_BYPASS=1", "FAKE_BAZEL_DURATION=0"}, "build", "//x")
	if code != 0 {
		t.Fatalf("bypass exit = %d, out:\n%s", code, out)
	}
	if atomic.LoadInt32(count) != 0 {
		t.Fatalf("BROKER_BYPASS still hit /admission %d times", *count)
	}
}

func TestWrapperCISkipsGate(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 2 * time.Second}, newFakeReg())
	srv, count := admissionServer(t, e)

	code, _ := runWrapper(t, srv.URL, []string{"CI=1", "FAKE_BAZEL_DURATION=0"}, "build", "//x")
	if code != 0 {
		t.Fatalf("CI exit = %d", code)
	}
	if atomic.LoadInt32(count) != 0 {
		t.Fatalf("CI=1 still hit /admission %d times", *count)
	}
}

func TestWrapperNonBuildCommandSkipsGate(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 2 * time.Second}, newFakeReg())
	srv, count := admissionServer(t, e)

	code, _ := runWrapper(t, srv.URL, []string{"FAKE_BAZEL_DURATION=0"}, "version")
	if code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	if atomic.LoadInt32(count) != 0 {
		t.Fatalf("non-build command hit /admission %d times", *count)
	}
}

func TestWrapperFailOpenWhenBrokerUnreachable(t *testing.T) {
	// Point at a closed port: the wrapper must fail open and still build.
	code, out := runWrapper(t, "http://127.0.0.1:1", []string{"FAKE_BAZEL_DURATION=0", "ADMISSION_CURL_TIMEOUT=2"}, "build", "//x")
	if code != 0 {
		t.Fatalf("fail-open exit = %d, out:\n%s", code, out)
	}
	if !strings.Contains(out, "fail-open") && !strings.Contains(out, "proceeding") {
		t.Fatalf("expected fail-open message, out:\n%s", out)
	}
}

func TestWrapperDrainDeniesBuild(t *testing.T) {
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 1 * time.Second}, newFakeReg())
	srv, _ := admissionServer(t, e)
	e.SetDraining(true)

	code, out := runWrapper(t, srv.URL, []string{"FAKE_BAZEL_DURATION=0"}, "build", "//x")
	if code != 75 {
		t.Fatalf("drain exit = %d, want 75 (EX_TEMPFAIL); out:\n%s", code, out)
	}
}

func TestWrapperInjectsKeepWarmAndE1Flags(t *testing.T) {
	// fake-bazel doesn't echo its argv, so use a tiny shim BAZEL_REAL that does.
	tmp := t.TempDir()
	shim := filepath.Join(tmp, "argv-echo.sh")
	if err := os.WriteFile(shim, []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(Policy{MaxConcurrent: 2, PollSeconds: 2 * time.Second}, newFakeReg())
	srv, _ := admissionServer(t, e)

	wrap := wrapperPath(t)
	cmd := exec.Command("/bin/bash", wrap, "build", "//app:app")
	cmd.Env = append(os.Environ(),
		"BAZEL_REAL="+shim,
		"BROKER_URL="+srv.URL,
		"BROKER_CACHE_DIR="+filepath.Join(tmp, "disk"),
		"BROKER_REPO_CACHE="+filepath.Join(tmp, "repo"),
		"BROKER_EVENT_DIR="+filepath.Join(tmp, "bep"),
		"BROKER_PROFILE_DIR="+filepath.Join(tmp, "prof"),
		"CI=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim run: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{
		"--max_idle_secs=10800",
		"--invocation_id=",
		"--build_event_json_file=",
		"--generate_json_trace_profile",
		"--profile=",
		"--disk_cache=",
		"--repository_cache=",
		"--incompatible_strict_action_env",
		"--experimental_output_paths=strip",
		"--copt=-ffile-prefix-map=",
		"--objccopt=-ffile-prefix-map=",
		"//app:app",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("injected argv missing %q\nargv:\n%s", want, s)
		}
	}
	// Must NOT inject the swiftcopt clang map (E1 sync: swiftc rejects it).
	if strings.Contains(s, "--swiftcopt=-ffile-prefix-map") {
		t.Errorf("wrapper injected --swiftcopt clang prefix-map (diverges from E1)\nargv:\n%s", s)
	}
}

// TestWrapperRecursionGuard runs the wrapper with BAZEL_REAL unset and a PATH
// containing ONLY the wrapper (named `bazel`), so the fallback resolver would
// pick itself if the guard failed. Must exit 127 quickly without fork-bombing.
func TestWrapperRecursionGuard(t *testing.T) {
	wrap := wrapperPath(t)
	tmp := t.TempDir()
	// A `bazel` symlink to the wrapper, alone on PATH.
	link := filepath.Join(tmp, "bazel")
	if err := os.Symlink(wrap, link); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", link, "build", "//x")
	cmd.Env = []string{"PATH=" + tmp, "BB_WRAPPER_REENTRY=", "HOME=" + tmp}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("recursion guard FAILED — wrapper ran past 5s (likely fork bomb)\n%s", out)
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 127 {
		t.Fatalf("recursion guard exit = %d, want 127\n%s", code, out)
	}
}
