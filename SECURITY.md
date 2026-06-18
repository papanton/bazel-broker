# Security

## Scope & threat model

Bazel Broker is **single-user, single-Mac** software. Its security boundary is:

- The daemon binds **loopback only** (`127.0.0.1`) — nothing off-machine can reach it.
- The API requires a bearer token stored in `~/.config/bazel-broker/config.json`, created
  `0600` (readable only by your user). The daemon is the sole writer; the token is generated
  with `crypto/rand`.

So the effective boundary is **loopback + same-user file permissions**: the token stops other
*local* processes/users from driving your builds. The one read-only, id-addressed Perfetto
profile route (`GET /profile/{id}/{name}`) is token-exempt with Origin-restricted CORS, because
`ui.perfetto.dev` fetches it cross-origin and cannot present the token.

**Explicit non-goals:** this is *not* hardened for multi-user hosts, untrusted local users, or
network exposure. Do not bind it to a non-loopback interface. If you need that, the natural step
is a Unix-domain socket with peer-credential checks instead of TCP + token.

The menu-bar app is non-sandboxed (it manages a LaunchAgent and runs local builds) and, for
public distribution, should be Developer ID-signed + notarized.

## Reporting a vulnerability

Please open a GitHub issue for non-sensitive reports, or contact the maintainer privately for
anything sensitive. There is no formal SLA — this is a personal-scope tool — but reports are
appreciated and will be addressed on a best-effort basis.
