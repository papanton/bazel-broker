// Package version holds build metadata injected at link time via -ldflags.
package version

var (
	Version = "dev"     // -X .../internal/version.Version=...
	Commit  = "none"    // -X .../internal/version.Commit=...
	Date    = "unknown" // -X .../internal/version.Date=...
)

// String renders the full version triple for -version / `version` output.
func String() string { return Version + " (" + Commit + ", " + Date + ")" }
