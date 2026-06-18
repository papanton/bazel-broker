// Package discovery finds running Bazel processes via libproc and owns the kill
// path. Implemented in E3 (cgo+libproc, behind a Scanner interface with a
// non-darwin stub). E0 ships only this placeholder so the import path exists and
// the cgo boundary is established here, keeping the default build pure-Go.
package discovery
