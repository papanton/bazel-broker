//go:build tools

// Package tools pins the codegen toolchain versions in go.mod so `make protos`
// uses a reproducible protoc-gen-go. It is never compiled into the broker (the
// `tools` build tag excludes it from normal builds); it exists only to record the
// dependency. Install with: go install google.golang.org/protobuf/cmd/protoc-gen-go
package tools

import (
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
