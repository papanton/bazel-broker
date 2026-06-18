// Package genproto is the root for Bazel's Build Event Protocol (BEP) Go types,
// generated from the pinned Bazel 8.3.1 .proto sources vendored under
// third_party/bazel_protos/. The generated *.pb.go files ARE committed so a clean
// checkout builds without protoc; codegen is only re-run on a Bazel version bump.
//
// Regenerate with `make protos` (see the //go:generate directive below). The
// vendored protos mirror Bazel's source tree layout so their cross-imports
// (src/main/protobuf/..., src/main/java/...) resolve under a single -I root, and
// the --go_opt=module= prefix strip plus per-file M-mappings give each proto a
// stable internal import path (Bazel's protos carry no go_package option).
package genproto

//go:generate make -C ../.. protos
