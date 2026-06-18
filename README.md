# Bazel Broker

A local, self-contained **control + observability layer for Bazel builds** running across
multiple git worktrees on macOS — for an iOS app built with Bazel + Bazelisk, where multiple
agents (and the developer) trigger builds concurrently.

It lets you **trace** what's building, **kill / queue** builds before they oversubscribe the
machine, **share + visualize cache** across worktrees, and **triage** build-time issues — with
no third-party SaaS.

## Status

Planning + verification fixture. No production code yet.

- **`plans/`** — architecture and epic breakdown.
  - `01-architecture.md` — components, cache strategy, tech stack, milestones, decisions.
  - `02-epics.md` — E0–E8 epic decomposition with dependency graph.
  - `epics/E0..E8-*.md` — per-epic implementation plans (each staff-reviewed).
  - `epics/00-consolidated-review.md` — binding cross-epic reconciliation (read first).
- **`testdata/ios-app/`** — a small **rules_apple** iOS app with dummy Swift packages
  (`Greeting`, `MathKit`, `Analytics`) used for true end-to-end verification of the broker.

## Architecture in one line

A headless `launchd` daemon (control plane) + a `tools/bazel` wrapper + thin front-ends
(`brokerctl` CLI, a localhost web page, a SwiftUI menu-bar app), with observability built from
Bazel's own BEP stream + `--profile` (viewed in Perfetto). See `plans/01-architecture.md`.

## Verification fixture

```sh
cd testdata/ios-app
bazelisk build //...        # builds the signed BrokerDemo.ipa for the iOS simulator
```

Each build emits `command.profile.gz` (Perfetto trace) and `.bazel-broker-bep.json` (BEP
stream) — the real inputs the broker is exercised against. Pinned to Bazel 8.3.1
(`.bazelversion`) for rules_apple 4.5.x compatibility.

## Toolchain

Go 1.26 (daemon/CLI), Xcode 26 / Swift 6.3 (menu-bar app + iOS fixture), Bazelisk, protoc +
protoc-gen-go (BEP codegen). Ships as a Homebrew cask; ad-hoc signed for personal use.
