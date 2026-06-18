# Contributing to Bazel Broker

Thanks for your interest! This is a single-Mac, single-developer-scope tool for macOS
(arm64 + x86_64). Issues and PRs welcome.

## Develop

```sh
make build         # bin/broker + bin/brokerctl
make test          # go test ./...
make fmt vet       # gofmt + go vet
make verify-fast   # ~3s headless: build + tests + fake-bazel + daemon /healthz
```

Run the daemon from source: `scripts/up.sh` (Ctrl-C to stop). Drive it with `bin/brokerctl`.

Exercise the broker with long, controllable builds:

```sh
scripts/loadtest.sh -n 4 -s 180   # 4 worktrees, 3-min builds each
scripts/loadtest.sh --down        # clean up
```

## Menu-bar app

```sh
cd apps/MenuBar
xcodegen generate
xcodebuild -project BrokerMenuBar.xcodeproj -scheme BrokerMenuBar build
xcodebuild -project BrokerMenuBar.xcodeproj -scheme BrokerMenuBar test   # contract-decode gate
```

The project is generated from `project.yml` (the `.xcodeproj` is gitignored). A build phase
bundles the `broker` daemon into the app.

## Conventions (don't break — multiple components depend on these)

- **Module:** `github.com/papanton/bazel-broker`. Go 1.26. cgo only in
  `internal/discovery` (libproc); the rest stays pure-Go.
- **`internal/api` is the FROZEN wire contract** between the daemon and every front-end. Golden
  serializations live in `testdata/api/*.json`; `internal/api` and the Swift app both decode
  them as a contract test. Changing a field means updating the fixtures and all consumers.
- **State values:** `queued | running | finished | failed | killed | gone | unknown`
  (`running`, not `building`).
- API is loopback-only (`127.0.0.1`) + `Authorization: Bearer <token>` from
  `~/.config/bazel-broker/config.json` (0600). See [SECURITY.md](SECURITY.md).

`CLAUDE.md` has per-component run/verify recipes and the deeper rationale.

## Pull requests

- Keep `make test` and `make verify-fast` green; run `make fmt vet`.
- Don't modify `testdata/ios-app/` (the pristine end-to-end fixture) or regenerate
  `testdata/api/*.json` without intent.
- One focused change per PR with a clear description.

## License

By contributing you agree your contributions are licensed under the [MIT License](LICENSE).
