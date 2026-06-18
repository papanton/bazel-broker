# Third-party notices

Bazel Broker is licensed under the [MIT License](LICENSE). It includes the following
third-party material under its own license:

## Bazel Build Event Protocol protos

- **Path:** `third_party/bazel_protos/` (and the Go types generated from them in
  `internal/genproto/`)
- **Source:** [bazelbuild/bazel](https://github.com/bazelbuild/bazel) — `build_event_stream.proto`
  and its transitive `.proto` dependencies, pinned to the Bazel 8.3.1 release.
- **Copyright:** The Bazel Authors.
- **License:** Apache License 2.0 — see [`third_party/bazel_protos/LICENSE`](third_party/bazel_protos/LICENSE).
  Each vendored `.proto` retains its original Apache copyright header.

Go module dependencies are fetched via the Go module system and retain their respective
licenses; see `go.mod` / `go.sum`.
