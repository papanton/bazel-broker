// Package bep tails and parses Bazel's Build Event Protocol JSON stream and
// derives metrics (cache hit ratio, action counts). Implemented in E4 (with the
// generated build_event_stream protos). E0 ships only this placeholder so the
// import path exists.
package bep
