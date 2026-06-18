# Bazel Broker

A local, self-contained **control + observability layer for Bazel builds** running across
multiple git worktrees on macOS — built for iOS apps (Bazel + Bazelisk + rules_apple), where
multiple agents and the developer trigger builds concurrently.

It lets you **trace** what's building, **kill / queue** builds before they oversubscribe the
machine, **share + visualize cache** across worktrees, and **triage** build timing — all on your
Mac, with **no third-party SaaS**.

> Status: working MVP. Daemon + CLI + menu-bar app, exercised end-to-end against a real
> rules_apple iOS fixture. See [`plans/`](plans/) for the full design.

## Why

Multiple worktrees building at once is uncontrolled and opaque:

- **No admission control** — N concurrent builds oversubscribe CPU/RAM and make *total*
  throughput worse.
- **No visibility** — no single view of what's building, where, for how long.
- **No control** — no easy way to kill or queue a runaway build.
- **Fragmented cache** — each worktree redoes identical work instead of sharing it.

Bazel Broker is a small background daemon that fixes all four, locally.

## Install (Homebrew)

```sh
brew tap papanton/tap
brew trust papanton/tap              # one-time: Homebrew requires trusting third-party taps
brew install --cask bazel-broker     # the menu-bar app (bundles the daemon)
brew install brokerctl               # the CLI
```

Launching the app installs the broker as a per-user LaunchAgent (it keeps running
independently of the app) and shows live builds in the menu bar.

## Quickstart

- **Menu bar (main UX):** open **Bazel Broker** — it auto-starts the daemon and lists live
  builds with elapsed time, the bazel command + targets, cache %, and a kill button. A gear
  reveals broker controls + "Speed Up a Project's Builds…" (applies shared-cache config to a
  workspace).
- **CLI:**
  ```sh
  brokerctl ls            # live builds (also --json)
  brokerctl watch         # live stream
  brokerctl kill <id>     # stop a build
  brokerctl drain         # stop admitting new builds (pause/resume too)
  brokerctl profile <id>  # open the build's trace in Perfetto
  ```
- **Speed up a project (highest-leverage, no daemon needed):**
  ```sh
  cache-config/setup.sh /path/to/your/ios-workspace
  ```
  Configures the workspace so all its git worktrees share one Bazel disk cache (measured ~90%
  cross-worktree hit rate on the fixture).

Without any setup, the broker still **passively discovers** running `bazel`/`bazelisk`
processes and lets you trace + kill them.

## How it works

A headless `launchd` daemon (the control plane) plus thin front-ends:

- **Discovery + kill** — finds `bazel` client processes via macOS `libproc` (no wrapper needed).
- **Admission control** — an optional committed `tools/bazel` wrapper gates builds before they
  start (block/queue when the machine is busy); fails open if the broker is down.
- **Observability** — tails each build's Build Event Protocol (BEP) stream and `--profile`,
  storing cache %, timing, and pass/fail in SQLite; deep per-action timing opens in Perfetto.
- **Front-ends** — a SwiftUI menu-bar app (primary) and the `brokerctl` CLI, thin clients over
  a loopback HTTP+WebSocket API.

Full architecture: [`plans/01-architecture.md`](plans/01-architecture.md).

## Build from source

```sh
make build        # bin/broker + bin/brokerctl
make test         # go test ./...
make verify-fast  # ~3s headless smoke (build + tests + fake-bazel + daemon healthz)
make dist         # universal (arm64+x86_64) app + CLI into dist/ (for Homebrew)
```

The menu-bar app (`apps/MenuBar/`, [XcodeGen](https://github.com/yonyz/XcodeGen) project) is
built with `xcodebuild`; it bundles the daemon binary. Requirements: Go 1.26, Xcode, Bazelisk.

## Layout

```
cmd/broker, cmd/brokerctl   daemon + CLI
internal/                   api (wire contract) · registry · store (SQLite) · httpapi
                            discovery (libproc, cgo) · bep (BEP ingest) · admission
apps/MenuBar/               SwiftUI menu-bar app (the main UX)
cache-config/               shared-disk-cache + profiling setup (setup.sh, .bazelrc fragment)
tools/bazel                 optional admission wrapper (commit into your iOS repo)
scripts/                    loadtest.sh, dist.sh, verify-fast.sh
testdata/                   synthetic workspace + a real rules_apple iOS fixture
plans/                      architecture + epic decomposition
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Single-Mac, single-developer scope; macOS arm64/x86_64.

## License

[MIT](LICENSE). Vendored Bazel Build Event Protocol protos under `third_party/` are
Apache-2.0 (The Bazel Authors) — see [`third_party/bazel_protos/LICENSE`](third_party/bazel_protos/LICENSE).
