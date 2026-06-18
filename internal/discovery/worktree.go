package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotInWorktree means the cwd has no .git ancestor before the filesystem root.
// This is how the reconciler drops the bazel server (cwd = output base), strays run
// outside a workspace, and the bazelisk download dir.
var ErrNotInWorktree = errors.New("discovery: cwd is not inside a git worktree")

// Worktree is a resolved git working tree identity.
type Worktree struct {
	Root   string // absolute path of the worktree working tree
	Name   string // last path component (display), e.g. "feature-a"
	GitDir string // resolved .git directory for THIS worktree (output-base lookup, D4)
}

// ResolveFromCwd walks up from dir until it finds a .git (dir OR file), then resolves
// the worktree. Returns ErrNotInWorktree if none is found before the FS root. It is a
// pure filesystem walk — no `git` subprocess — so it is fast (no spawn per PID) and
// works without git on $PATH.
func ResolveFromCwd(dir string) (Worktree, error) {
	if dir == "" {
		return Worktree{}, ErrNotInWorktree
	}
	cur := filepath.Clean(dir)
	for {
		dotgit := filepath.Join(cur, ".git")
		fi, err := os.Lstat(dotgit)
		if err == nil {
			if fi.IsDir() {
				// Primary working tree: .git is a directory.
				return Worktree{Root: cur, Name: filepath.Base(cur), GitDir: dotgit}, nil
			}
			// Linked worktree: .git is a FILE containing "gitdir: <path>".
			gitdir, err := readGitdirFile(dotgit)
			if err != nil {
				return Worktree{}, err
			}
			return Worktree{Root: cur, Name: filepath.Base(cur), GitDir: gitdir}, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur { // reached "/"
			return Worktree{}, ErrNotInWorktree
		}
		cur = parent
	}
}

// readGitdirFile parses `.git` files of the form
// "gitdir: /abs/path/.git/worktrees/<name>".
func readGitdirFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(b))
	line = strings.TrimPrefix(line, "gitdir:")
	return strings.TrimSpace(line), nil
}
