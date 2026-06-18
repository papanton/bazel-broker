package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Opener launches a resolved profile target (URL or local path). Injectable for
// tests and the --print/--json paths.
type Opener func(ctx context.Context, target string) error

// OpenURL shells out to macOS `open`. A non-zero exit maps to ExitOpen.
func OpenURL(ctx context.Context, target string) error {
	cmd := exec.CommandContext(ctx, "open", target)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return wrap(ExitOpen, "open %q: %v", target, err)
	}
	return nil
}

// ProfileOpts configures the profile command.
type ProfileOpts struct {
	Print  bool   // print the resolved target and do NOT open
	JSON   bool   // print {"profile":"…"} and do NOT open
	Opener Opener // injectable; defaults to OpenURL
}

// RunProfile resolves a build's profile target then opens it (or prints it).
//
// Resolution order (frozen contract): prefer Build.ProfileURL from
// GET /builds/{id} (the ready-to-open deep-link E4 populates); if absent, fall
// back to GET /builds/{id}/profile → api.ProfileRef (PerfettoURL, then LocalPath).
// Both endpoints' enrichment is E4-owned, so until E4 lands ProfileURL is empty
// and /profile returns 501 → ExitNotImplemented.
func RunProfile(ctx context.Context, c *Client, id string, opt ProfileOpts, out io.Writer) error {
	target, err := resolveProfileTarget(ctx, c, id)
	if err != nil {
		return err
	}
	if target == "" {
		return wrap(ExitBroker, "no profile available for %s yet (needs E4)", id)
	}

	if opt.JSON {
		return RenderJSON(out, map[string]string{"invocation_id": id, "profile": target})
	}
	if opt.Print {
		fmt.Fprintln(out, target)
		return nil
	}

	open := opt.Opener
	if open == nil {
		open = OpenURL
	}
	if err := open(ctx, target); err != nil {
		return err
	}
	fmt.Fprintf(out, "opened profile for %s: %s\n", id, target)
	return nil
}

// resolveProfileTarget tries Build.ProfileURL first, then the dedicated
// /builds/{id}/profile endpoint. A 501 from the fallback is surfaced as-is
// (ExitNotImplemented) only if the build carried no profile_url.
func resolveProfileTarget(ctx context.Context, c *Client, id string) (string, error) {
	b, err := c.GetBuild(ctx, id)
	if err != nil {
		return "", err // 404 (unknown id) / auth / unavailable bubble up
	}
	if b.ProfileURL != "" {
		return b.ProfileURL, nil
	}

	// Fall back to the dedicated profile endpoint. A 501 here (E4 not landed)
	// surfaces as ExitNotImplemented — the expected "not available yet" path.
	ref, err := c.Profile(ctx, id)
	if err != nil {
		return "", err
	}
	if ref.PerfettoURL != "" {
		return ref.PerfettoURL, nil
	}
	return ref.LocalPath, nil
}
