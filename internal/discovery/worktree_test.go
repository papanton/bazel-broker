package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestResolveFromCwd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	real, _ := filepath.EvalSymlinks(base)

	// Primary working tree.
	primary := filepath.Join(real, "main")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, primary, "init", "-q")
	git(t, primary, "commit", "-q", "--allow-empty", "-m", "init")

	// A nested subdir of the primary tree.
	nested := filepath.Join(primary, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	// A linked worktree (.git is a FILE).
	linked := filepath.Join(real, "feature-x")
	git(t, primary, "worktree", "add", "-q", linked)

	// A non-repo dir.
	nonrepo := filepath.Join(real, "outside")
	if err := os.MkdirAll(nonrepo, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("primary tree (.git dir)", func(t *testing.T) {
		wt, err := ResolveFromCwd(primary)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Root != primary || wt.Name != "main" {
			t.Errorf("got root=%q name=%q", wt.Root, wt.Name)
		}
		if fi, err := os.Lstat(wt.GitDir); err != nil || !fi.IsDir() {
			t.Errorf("expected GitDir to be a dir: %q (%v)", wt.GitDir, err)
		}
	})

	t.Run("nested subdir resolves to primary root", func(t *testing.T) {
		wt, err := ResolveFromCwd(nested)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Root != primary {
			t.Errorf("nested resolved to %q, want %q", wt.Root, primary)
		}
	})

	t.Run("linked worktree (.git file -> gitdir:)", func(t *testing.T) {
		wt, err := ResolveFromCwd(linked)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Root != linked || wt.Name != "feature-x" {
			t.Errorf("got root=%q name=%q", wt.Root, wt.Name)
		}
		// GitDir points into the primary repo's .git/worktrees/<name>.
		if !filepath.IsAbs(wt.GitDir) {
			t.Errorf("linked GitDir not absolute: %q", wt.GitDir)
		}
		if _, err := os.Stat(wt.GitDir); err != nil {
			t.Errorf("linked GitDir does not exist: %q (%v)", wt.GitDir, err)
		}
	})

	t.Run("non-repo dir -> ErrNotInWorktree", func(t *testing.T) {
		if _, err := ResolveFromCwd(nonrepo); err != ErrNotInWorktree {
			t.Errorf("got %v, want ErrNotInWorktree", err)
		}
	})

	t.Run("empty cwd -> ErrNotInWorktree", func(t *testing.T) {
		if _, err := ResolveFromCwd(""); err != ErrNotInWorktree {
			t.Errorf("got %v, want ErrNotInWorktree", err)
		}
	})
}
