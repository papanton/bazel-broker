# E6 — `brokerctl` CLI

> ⚠️ **Superseded where it conflicts with [`00-consolidated-review.md`](00-consolidated-review.md) + E2 §4 (frozen contract).** Conform before coding:
> - **P1:** the shared package is **`internal/api`** (`api.Build`), NOT `internal/wire` — rename all ~23 refs. E2 froze `internal/api` and retired `wire`.
> - **P2:** `kill` → `POST /builds/{invocation_id}/kill`; **P3:** fields `invocation_id`/`start_time` (not `id`/`started_at`).
> - Degradation keys on **`501`** (E2's reserved-route status), not `404`.

> Executable implementation plan for the terminal front-end (C5a) of the Bazel Broker.
> Standalone `/goal`. Authoritative context: `01-architecture.md` §5 (C5a) + §6, `02-epics.md` EPIC 6,
> and **`E2-broker-core.md` §4 (the FROZEN API contract this epic consumes)**.

Status: **Draft v2** · Owner: Antonis · Depends on: **E2** (API + config + `internal/wire`) · richer with **E3** (kill) and **E4** (metrics/profile) · Maps to: M5

> **Contract-alignment note (v2):** v1 of this plan invented endpoint paths, wire field names,
> a config shape, and a degradation signal (feature-404) that do **not** match E2's frozen §4
> contract. v2 rewrites every interface to consume E2 verbatim. The authoritative facts E6 must
> obey, taken directly from `E2-broker-core.md` §4:
> - **Endpoints:** `GET /healthz`, `GET /builds`, `WS /events`, `POST /kill`, `POST /drain`,
>   `POST /resume`, `GET /metrics?invocation_id=…`. There is **no** `POST /admission/...`,
>   no `/builds/{id}/...` sub-resources, no `pause`, and **no `/profile` endpoint**.
> - **Not-yet-implemented signal is `501 Not Implemented`, NOT `404`.** Reserved routes (`/kill`
>   E3, `/metrics` E4, `/drain`/`/resume` E5) are *routed now* and return
>   `501 {"error":"not_implemented","message":"owned by <epic>"}`. A 404 means "no such route"
>   (a bug or version skew); a 404 on a *resource* (e.g. unknown build id) is a real error.
> - **`/builds` returns `{"builds":[...]}` (`wire.BuildsResponse`), not a bare array.**
> - **Wire types live in `internal/wire` (owned by E2), field names are `invocation_id`,
>   `start_time`, `elapsed_ms` (server-computed), etc.** E6 imports them; it does not redefine them.

---

## 1. Goal & scope recap

Ship `brokerctl`: the first front-end and the primary `/verify` surface for the whole
project. It is a thin, stateless HTTP/WS client over the broker daemon (E2) that lets a
human or an agent see and steer builds from the terminal.

**Commands (from EPIC 6 scope):**
- `brokerctl ls [--json]` — registry snapshot (`GET /builds`) as a human table or machine JSON.
- `brokerctl kill <id>` — stop a build (`POST /kill`; 501-degrades until E3).
- `brokerctl drain` / `resume` — stop / resume admitting builds (`POST /drain` · `POST /resume`; 501-degrades until E5).
- `brokerctl watch` — subscribe to `WS /events` and render a live-updating view.
- `brokerctl profile <id>` — resolve the build's Bazel `--profile` from `GET /metrics` and `open` it in Perfetto (needs E4; **endpoint shape is an E4-owned decision — see OD-6**).

**Cross-cutting deliverables:** an HTTP client that reads the loopback `port` + bearer `token`
from `~/.config/bazel-broker/config.json` (E2's config file; **loopback-only, no `host` field**)
and sets the `Authorization: Bearer` header on every request and the WS upgrade; clean table
output for humans and `--json` (echoing E2's wire JSON) for machines/`/verify` assertions;
**stable, documented exit codes**; a `make brokerctl` target and a `CLAUDE.md` "how to run &
verify" recipe.

**Done when (from epic):** `brokerctl ls` build count mirrors `/healthz` `builds`; `kill` works
end-to-end (with fake-bazel + E3); `brokerctl ls --json | jq '.builds | length'` is parseable.

**Out of scope (other epics):** the broker daemon itself and its endpoints (E2/E3/E4/E5);
the web dashboard (E7); the menu-bar app (E8). This epic only *consumes* the E2 API contract.

### Dependency posture
E6 hard-depends only on **E2**. `ls`, `watch`, the config/client, and output formatting are
buildable against E2 alone. `kill` (E3), `drain`/`resume` (E5), `profile`/metrics columns (E4)
call routes that E2 **already registers** but that return **`501 Not Implemented`** until their
owning epic fills the handler body (E2 §4.2). The plan **builds those commands now** but makes
them **feature-detect and degrade on `501`** (clear "not available on this broker yet" message +
dedicated exit code `ExitNotImplemented`) so E6 is not blocked on E3/E4/E5 and lights up
automatically as they land.

> **Why 501, not 404:** E2 deliberately routes the reserved endpoints and returns `501` (its
> acceptance criterion is literally "All reserved routes return 501 (not 404)"). This means E6
> does **not** need a path-allowlist (`isFeaturePath`) or a typed-error convention to tell
> "feature missing" from "resource missing": `501` ⇒ not-yet-implemented; `404` ⇒ unknown
> route/resource (e.g. unknown build id, or a broker older/newer than expected). v1's OD-3
> (404 disambiguation) is therefore **dropped** — the contract already disambiguates.

---

## 2. Design & implementation details

### 2.1 Package layout (within the E0 single module)

```
cmd/brokerctl/
  main.go                 // cobra root wiring + Execute()
  cmd_ls.go               // `ls`
  cmd_kill.go             // `kill`
  cmd_drain.go            // `drain` / `resume`  (POST /drain, POST /resume — no `pause` in contract)
  cmd_watch.go            // `watch`
  cmd_profile.go          // `profile`
internal/cli/
  config.go               // load ~/.config/bazel-broker/config.json (read-only)
  client.go               // broker HTTP+WS client (bearer auth)
  render.go               // table + json rendering
  exit.go                 // exit-code constants + cliError
internal/wire/            // OWNED BY E2; shared JSON DTOs (wire.Build, wire.Event, …) — IMPORTED, not redefined
```

`internal/wire` is the single source of truth for the wire types and **is authored and owned by
E2** (E2 §2.1 deliberately places the importable DTOs there, separate from the daemon-internal
`internal/build` domain object). E6 imports `internal/wire` so the CLI and daemon can never drift.

> **Wire-type ownership is largely resolved by E2, not open.** E2 §2.1/§4.1 already export
> `wire.Build`, `wire.Event`, `wire.HealthResponse`, `wire.BuildsResponse`,
> `wire.RegisterRequest`, `wire.DeregisterRequest`, `wire.ErrorResponse` in `internal/wire`.
> v1's plan to author structs in a new `internal/api` package and have E2 "adopt" them is wrong
> and would fork the contract. **E6 imports `internal/wire` as-is.** The only genuinely open
> pieces are the types for *not-yet-implemented* endpoints whose response shapes E2 left
> unspecified (kill result, metrics/profile payload) — those are owned by **E3/E4**, not E6, and
> are tracked as **OD-1 (kill-response shape)** and **OD-6 (metrics/profile-response shape)**.
> E6 must **not** speculatively define them in `internal/wire`; until the owning epic lands, the
> 501 path means those commands never decode a body anyway.

### 2.2 CLI framework — recommendation: **cobra**

**Recommend cobra**, not stdlib `flag`. Justification:
- We have a real subcommand *tree* (`ls`, `kill`, `drain`, `resume`, `watch`, `profile`, later a
  possible `metrics`). `flag` has no native subcommand dispatch — we'd hand-roll `os.Args[1]`
  switching, per-command `flag.FlagSet`s, and usage text. Cobra gives that for free.
- Auto-generated `--help`/usage per command, suggestions on typos, and a clean place to hang
  persistent flags (`--json`, `--config`, `--port`, `--token`, `--timeout`).
- It is the de-facto Go CLI standard; one well-understood dependency. Pin it in `go.mod`.
- **Cost we accept:** one third-party dep. Mitigation: keep cobra usage vanilla (no Viper,
  no cobra codegen) so it stays a thin dispatcher and the logic lives in `internal/cli`,
  which is independently unit-testable without cobra.

Persistent (root) flags, available to every command:

| Flag | Default | Purpose |
|---|---|---|
| `--json` | `false` | machine-readable output (commands that have a JSON form) |
| `--config <path>` | `$BAZEL_BROKER_CONFIG`, then `$XDG_CONFIG_HOME/bazel-broker/config.json`, then `~/.config/...` | override config path (tests). **Same resolution order as E2's `config.Load`** (E2 §2.5) so CLI and daemon agree on which file is authoritative |
| `--port <n>` | from config | override broker port (tests / non-default port). Host is always loopback `127.0.0.1` — E2 binds loopback-only and the config has **no `host` field** |
| `--token <tok>` | from config | override bearer token (tests) |
| `--timeout <dur>` | `5s` | per-request HTTP timeout (**not** applied to `watch`, which is long-lived) |

> v1 had `--addr host:port`; dropped in favour of `--port` because E2 is loopback-only by
> contract (`net.Listen("tcp", "127.0.0.1:PORT")`, never `:PORT`) and exposes no host field.
> A host override would imply a non-loopback broker, which the architecture forbids (single-Mac,
> token-over-loopback security model, D-stack-2). Keeping it loopback-only also keeps the bearer
> token from ever being sent to an arbitrary host by a fat-fingered `--addr`.

### 2.3 Root command + version (`main.go`)

```go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"bazelbroker/internal/cli"
)

var version = "dev" // set via -ldflags "-X main.version=..."

func main() {
	root := newRootCmd()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(cli.ExitCodeFor(err)) // maps cliError -> code; default 1
	}
}

func newRootCmd() *cobra.Command {
	var opt cli.GlobalOpts
	root := &cobra.Command{
		Use:           "brokerctl",
		Short:         "Control and observe Bazel builds via the local broker",
		Version:       version,
		SilenceUsage:  true, // don't dump usage on runtime errors (only on arg errors)
		SilenceErrors: true, // we print errors ourselves with exit-code mapping
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&opt.JSON, "json", false, "machine-readable JSON output")
	pf.StringVar(&opt.ConfigPath, "config", "", "path to config.json (default: $BAZEL_BROKER_CONFIG / XDG / ~)")
	pf.IntVar(&opt.Port, "port", 0, "broker loopback port (overrides config; host is always 127.0.0.1)")
	pf.StringVar(&opt.Token, "token", "", "bearer token (overrides config)")
	pf.DurationVar(&opt.Timeout, "timeout", 5*time.Second, "per-request timeout (not applied to watch)")

	root.AddCommand(
		newLsCmd(&opt), newKillCmd(&opt), newDrainCmd(&opt),
		newWatchCmd(&opt), newProfileCmd(&opt),
	)
	return root
}
```

`GlobalOpts` carries the resolved flags; each command turns it into a `*cli.Client` via
`cli.NewClient(opt)`, which loads config and applies overrides.

### 2.4 Config loading (`internal/cli/config.go`)

Config shape and file location are **defined and written by E2** (E2 §2.5). **E6 reads the
fields it needs and ignores the rest** — it never writes the file. E2's actual config struct is:

```jsonc
{
  "port": 8765,                 // loopback TCP port           ← E6 reads
  "token": "b9f1…32bytes-hex",  // bearer token                ← E6 reads
  "disk_cache": "/…/bazel-disk",// shared --disk_cache (E1/E5) — E6 ignores
  "max_concurrency": 2,         // admission ceiling (E5)      — E6 ignores
  "db_path": "/…/broker.db",    // SQLite file                 — E6 ignores
  "log_path": "/…/broker.log"   // slog sink                   — E6 ignores
}
```

There is **no `host`** (loopback is implied) and **no `profile_open`** field — both were
invented by v1 and are removed. The Perfetto/open behaviour is fixed (`open` on macOS, §2.11),
not configurable.

```go
// Config mirrors only the subset of E2's config.json that brokerctl consumes.
// It is intentionally lenient about unknown fields so the CLI keeps working as E2 adds
// config keys for later epics (disk_cache, max_concurrency, db_path, log_path, …).
type Config struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// LoadConfig resolves the path (flag > $BAZEL_BROKER_CONFIG > $XDG_CONFIG_HOME > ~/.config),
// matching E2's config.Load resolution order (E2 §2.5), then reads + validates.
func LoadConfig(explicitPath string) (*Config, error) {
	path := resolveConfigPath(explicitPath) // flag → $BAZEL_BROKER_CONFIG → $XDG_CONFIG_HOME → ~/.config
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, wrap(ExitConfig,
				"no config at %s — is the broker installed/running? (E2 writes this file on first run)", path)
		}
		return nil, wrap(ExitConfig, "read config %s: %w", path, err)
	}
	var c Config
	// LENIENT decode (no DisallowUnknownFields): E2 owns the file and adds keys per-epic; a CLI
	// that hard-fails on an unknown key would break every time the daemon grows a field. (OD-2.)
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, wrap(ExitConfig, "parse config %s: %w", path, err)
	}
	if c.Port == 0 || c.Token == "" {
		return nil, wrap(ExitConfig, "config %s missing port or token", path)
	}
	return &c, nil
}
```

> **Resolution-order alignment (load-bearing):** E6's `resolveConfigPath` MUST mirror E2's
> `config.Load` (E2 §2.5): `--config` flag → `$BAZEL_BROKER_CONFIG` → `$XDG_CONFIG_HOME/...` →
> `~/.config/...`. If the two diverge, `brokerctl` can read a *different* config file than the
> running daemon wrote (e.g. when launchd sets `$BAZEL_BROKER_CONFIG` in the plist, E2 §2.8) and
> talk to the wrong port/token. This is the single most likely silent-misconfig bug; it has a
> dedicated unit test in §5.1.

### 2.5 Broker HTTP+WS client (`internal/cli/client.go`)

Single client type; all commands go through it. Bearer header set on every request.

```go
type Client struct {
	base    string        // "http://127.0.0.1:8765"
	wsBase  string        // "ws://127.0.0.1:8765"
	token   string
	http    *http.Client  // Timeout from GlobalOpts (watch uses a separate no-timeout client)
}

func NewClient(opt GlobalOpts) (*Client, error) {
	cfg, err := LoadConfig(opt.ConfigPath)
	if err != nil {
		return nil, err
	}
	port := cfg.Port
	token := cfg.Token
	if opt.Port != 0 {            // --port overrides config port; host is always loopback
		port = opt.Port
	}
	if opt.Token != "" {
		token = opt.Token
	}
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) // E2 is loopback-only by contract
	return &Client{
		base:   "http://" + authority,
		wsBase: "ws://" + authority,
		token:  token,
		http:   &http.Client{Timeout: opt.Timeout},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return wrap(ExitUsage, "build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrap(ExitUnavailable, "broker unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized: // 401
		return wrap(ExitAuth, "broker rejected token (401) — check config token")
	case resp.StatusCode == http.StatusNotImplemented: // 501 = reserved route, owning epic not landed
		return wrap(ExitNotImplemented,
			"%s %s not available on this broker yet (%s)", method, path, brokerErrMessage(resp))
	case resp.StatusCode == http.StatusNotFound: // 404 = unknown route OR unknown resource (e.g. build id)
		return wrap(ExitBroker, "broker %s %s -> 404: %s", method, path, brokerErrMessage(resp))
	case resp.StatusCode >= 400:
		return wrap(ExitBroker, "broker %s %s -> %d: %s", method, path, resp.StatusCode, brokerErrMessage(resp))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return wrap(ExitBroker, "decode response: %w", err)
		}
	}
	return nil
}

// brokerErrMessage best-effort decodes E2's {"error","message"} body (wire.ErrorResponse)
// for a human-readable tail; falls back to a short raw snippet. Never returns "".
func brokerErrMessage(resp *http.Response) string { /* decode wire.ErrorResponse, else snippet */ }
```

> **The degradation switch keys on `501`, not `404`.** This matches E2 exactly: reserved routes
> are *registered* and return `501 {"error":"not_implemented","message":"owned by E3"}`. So
> `brokerctl kill <id>` against an E3-less broker hits a real route, gets `501`, and degrades to
> `ExitNotImplemented` with the broker's own message. A `404` is treated as a genuine error
> (`ExitBroker`) — it means either an unknown *resource* (E2 may 404 an unknown build id once E3
> fills `/kill`) or a route the CLI expected but the broker doesn't serve (version skew). No
> `isFeaturePath` allowlist is needed; the status code is the contract.

Typed convenience methods — **paths and types from E2 §4.2 verbatim**:

```go
func (c *Client) Healthz(ctx context.Context) (wire.HealthResponse, error)        // GET  /healthz
func (c *Client) ListBuildsResponse(ctx context.Context) (wire.BuildsResponse, error) // GET /builds → full {"builds":[…]}
func (c *Client) ListBuilds(ctx context.Context) ([]wire.Build, error)            // convenience: .Builds of the above
func (c *Client) Kill(ctx context.Context, id string) error                 // POST /kill   {"invocation_id":id}  (501 until E3)
func (c *Client) Drain(ctx context.Context) error                           // POST /drain  (501 until E5)
func (c *Client) Resume(ctx context.Context) error                          // POST /resume (501 until E5)
func (c *Client) Metrics(ctx context.Context, id string) (json.RawMessage, error) // GET /metrics?invocation_id=id (501 until E4)
func (c *Client) StreamEvents(ctx context.Context) (*EventStream, error)    // WS   /events
```

> Notes on the corrected contract:
> - **`/builds` is wrapped.** `ListBuilds` decodes `wire.BuildsResponse{Builds []wire.Build}` and
>   returns the slice. v1 decoded a bare array, which would have failed against E2.
> - **`POST /kill`** takes a JSON body `{"invocation_id":"…"}` (E2 §4.2 — also accepts `{"pid":N}`,
>   unused by the CLI). There is **no** `/builds/{id}/kill` sub-resource. The success-response
>   shape is E3-owned and unspecified by E2, so `Kill` returns only `error` for now (**OD-1**);
>   when E3 fixes the shape, add a typed return that decodes whatever E3 adds to `internal/wire`.
> - **No `/admission/drain` or `pause`.** It is `POST /drain` + `POST /resume` (E5). `pause` does
>   not exist in the contract and is dropped from the CLI (was a v1 invention).
> - **No `/profile` endpoint exists.** Profile-path resolution piggybacks on `GET /metrics`
>   (E4 keys metrics on `invocation_id` and reads `worktree` to locate the `--profile` gz). The
>   exact field that carries the profile URL/path is **E4-owned (OD-6)**; `Metrics` returns
>   `json.RawMessage` until E4 freezes it, so `profile` can extract a field without E6 inventing a struct.

`wire.Build` is **imported from E2** (E2 §4.1) — E6 does not define it. For reference, the
columns the table renders come from these E2 fields (note the names — `invocation_id`,
`start_time`, server-computed `elapsed_ms`):

```go
// from internal/wire (E2 §4.1) — shown here for column mapping only; NOT redefined in E6
type Build struct {
	InvocationID string   `json:"invocation_id"`
	Worktree     string   `json:"worktree"`
	Targets      []string `json:"targets"`
	PID          int      `json:"pid"`
	State        string   `json:"state"`       // queued|running|finished|failed|killed|unknown
	StartTime    string   `json:"start_time"`  // RFC3339 UTC
	EndTime      string   `json:"end_time,omitempty"`
	ExitCode     int      `json:"exit_code"`
	Source       string   `json:"source"`      // registered|discovered
	ElapsedMS    int64    `json:"elapsed_ms"`  // server-computed; CLI does NOT recompute from clock
}
```

> **No `cache_hit_ratio`/`has_profile` on `wire.Build`.** v1 added metrics fields to the build
> DTO; E2 keeps metrics in a **separate `metrics` table** served by `GET /metrics` (E4). So the
> `ls` table's CACHE column is **dropped from the default view** (it would require an N+1
> `/metrics` call per build, all 501 until E4). When E4 lands, a `ls --metrics` flag (or a
> widened `/builds` if E2/E4 choose to embed a summary) can add it — tracked as **OD-7**. Until
> then the table shows only fields present on `wire.Build`.

### 2.6 Output rendering — table vs `--json` (`internal/cli/render.go`)

One rule: **`--json` echoes the broker's JSON shape verbatim** (`wire.BuildsResponse` for `ls`);
the table is a derived human view. JSON output is the contract for `/verify` and must stay stable
(§6 output-stability). Because `--json` re-emits E2's bytes, the CLI and daemon cannot drift.

```go
// RenderJSON marshals v indented to w and is the only writer for --json mode.
func RenderJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// RenderBuildsTable writes a fixed-column, tab-aligned table via text/tabwriter.
func RenderBuildsTable(w io.Writer, builds []wire.Build) {
	if len(builds) == 0 {
		fmt.Fprintln(w, "no active builds")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWORKTREE\tSTATE\tELAPSED\tTARGETS")
	for _, b := range builds {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			shortID(b.InvocationID),       // first 8 chars
			filepath.Base(b.Worktree),
			b.State,
			humanizeElapsedMS(b.ElapsedMS), // SERVER-computed; CLI never recomputes from its clock
			joinTargets(b.Targets, 1),     // first target + "(+N)"
		)
	}
	tw.Flush()
}
```

Design choices:
- **`text/tabwriter`** (stdlib) for alignment — no table dependency, NUL-free, easy to test.
- Human columns intentionally lossy (short ID, basename, first target). The **full** values
  always live in `--json`, so scripts never parse the table.
- **ELAPSED comes from the server's `elapsed_ms`** (E2 computes it from a monotonic-ish wall
  clock, E2 §6), **not** `time.Since(start_time)` on the CLI side. v1 recomputed it client-side,
  which re-introduces exactly the clock-skew bug E2 deliberately solved by computing it server-side.
  The "clamp negative elapsed" edge case in v1's §6 is therefore moot and removed.
- **No CACHE column** in the default `ls` table: cache% lives in `GET /metrics` (E4), not on
  `wire.Build`. See OD-7 for the post-E4 `--metrics` opt-in.
- Colour: **off by default**; only colourize when stdout `isatty` AND `NO_COLOR` unset.
  Colour is cosmetic and never emitted in `--json`, keeping machine output clean.

### 2.7 `ls` (`cmd_ls.go`)

```go
func newLsCmd(opt *GlobalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List active and recent builds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			resp, err := c.ListBuildsResponse(cmd.Context()) // full wire.BuildsResponse
			if err != nil {
				return err
			}
			if opt.JSON {
				// Echo E2's exact shape: {"builds":[…]}. /verify uses `jq '.builds | length'`.
				return cli.RenderJSON(cmd.OutOrStdout(), resp)
			}
			cli.RenderBuildsTable(cmd.OutOrStdout(), resp.Builds)
			return nil
		},
	}
}
```

### 2.8 `kill` (`cmd_kill.go`) — depends on E3

```go
func newKillCmd(opt *GlobalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <id>",
		Short: "Stop a build by invocation id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			// POST /kill {"invocation_id": args[0]}. Returns 501→ExitNotImplemented until E3.
			if err := c.Kill(cmd.Context(), args[0]); err != nil {
				return err
			}
			if opt.JSON {
				return cli.RenderJSON(cmd.OutOrStdout(), map[string]string{"killed": args[0]})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "killed %s\n", args[0])
			return nil
		},
	}
}
```

`POST /kill` is asynchronous in spirit; E3 decides whether the broker responds on SIGINT
delivery or fire-and-forget, and what (if any) success body it returns — **OD-1**. Until then the
CLI treats a 2xx as success and renders a minimal acknowledgement. Disambiguation of "feature not
landed" vs "unknown build id" is by **status code**, not path inspection: `501` ⇒
`ExitNotImplemented`; `404` (which E3 may return for an unknown id once `/kill` is live) ⇒
`ExitBroker` "no such build". No `isFeaturePath`/typed-error convention is required (v1's OD-3 is dropped).

### 2.9 `drain` / `resume` (`cmd_drain.go`) — depends on E5

`drain` → `POST /drain`; `resume` → `POST /resume` (E2 §4.2 — these exact paths; there is **no**
`/admission/...` route and **no `pause`** in the contract). Until E5 lands, both routes are
registered by E2 and return **`501`** → `ExitNotImplemented` + the broker's "owned by E5" message.
v1's `pause` subcommand and `/admission/{drain,pause,resume}` paths are removed as they have no
backing contract. If E5 later adds a pause semantic, add the subcommand then against E5's actual route.

### 2.10 `watch` (`cmd_watch.go`) — WS loop

`watch` subscribes to WS `/events` (E2 `StreamEvents`) and renders a live, in-place
updating build table. Uses **`coder/websocket`** (per the tech stack; modern, context-first,
`net/http`-native handshake so the bearer header goes on `DialOptions.HTTPHeader`).

```go
import "github.com/coder/websocket"

type EventStream struct{ conn *websocket.Conn }

func (c *Client) StreamEvents(ctx context.Context) (*EventStream, error) {
	conn, _, err := websocket.Dial(ctx, c.wsBase+"/events", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if err != nil {
		return nil, wrap(ExitUnavailable, "ws connect %s/events: %w", c.wsBase, err)
	}
	return &EventStream{conn: conn}, nil
}

// Next blocks for the next event frame (JSON). Returns io.EOF on clean close.
func (s *EventStream) Next(ctx context.Context) (wire.Event, error) {
	var ev wire.Event
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return ev, err
	}
	return ev, json.Unmarshal(data, &ev)
}
```

Wire event — **imported from E2 (`wire.Event`, E2 §4.1), not redefined**. The actual contract is:

```go
// from internal/wire (E2 §4.1) — shown for reference only
type Event struct {
	Type   EventType `json:"type"`               // "snapshot" (once on connect) | "build"
	Seq    uint64    `json:"seq"`                // monotonic per connection
	Build  *Build    `json:"build,omitempty"`    // set when type=="build"
	Builds []Build   `json:"builds,omitempty"`   // set when type=="snapshot"
	Ts     string    `json:"ts"`                 // RFC3339 UTC
}
```

> **Corrected event model (load-bearing for `applyEvent`):** E2 emits only two event types —
> `snapshot` (full list, once on connect) and `build` (one created/updated/**terminated** build).
> There is **no** `build_update`/`build_removed`/`build_id`. A build is never "removed" from the
> feed; it transitions to a terminal `state` (`finished`/`failed`/`killed`/`unknown`) via a
> `build` event and stays in the list. So `applyEvent` is: on `snapshot`, replace the whole map;
> on `build`, upsert by `Build.InvocationID`. The watch view may *filter* terminal builds from the
> live table for readability, but it does not key off a removal event. v1's three-type model and
> `/verify` assertion ("a `build_update` then a `build_removed`") are corrected accordingly.

`watch` run loop — **outer reconnect loop** around an inner read loop. Each (re)connection
begins with E2's guaranteed `snapshot` frame, which makes reconnect *lossless*: we discard local
state and rebuild it from the snapshot, so no missed-event bookkeeping is needed.

```go
func runWatch(ctx context.Context, c *cli.Client, jsonMode bool, out io.Writer) error {
	render := cli.NewLiveRenderer(out) // clears + redraws table; no-op tty control if --json
	backoff := newBackoff(200*time.Millisecond, 5*time.Second) // exp, capped, jittered
	for {
		err := watchOnce(ctx, c, jsonMode, out, render)
		switch {
		case ctx.Err() != nil:                 // Ctrl-C: clean exit 0
			return nil
		case errors.Is(err, errAuth):          // 401 on upgrade: don't spin-retry a bad token
			return wrap(cli.ExitAuth, "broker rejected token on /events (401)")
		case err == nil || isConnClosed(err):  // broker restarted / dropped us → reconnect
			if !jsonMode {
				render.Notice("broker connection lost — reconnecting…")
			}
			if waitErr := backoff.Sleep(ctx); waitErr != nil { // ctx cancelled mid-backoff
				return nil
			}
			continue
		default:
			return wrap(cli.ExitBroker, "watch: %w", err)
		}
	}
}

// watchOnce holds exactly one connection; resets local state from the snapshot, then applies
// incremental `build` events until the connection ends.
func watchOnce(ctx context.Context, c *cli.Client, jsonMode bool, out io.Writer, render *cli.LiveRenderer) error {
	stream, err := c.StreamEvents(ctx)
	if err != nil {
		return err // may wrap errAuth (401) or a dial error
	}
	defer stream.Close(websocket.StatusNormalClosure, "bye")
	backoffReset()                  // a successful connect resets the outer backoff
	state := map[string]wire.Build{}
	for {
		ev, err := stream.Next(ctx)
		if err != nil {             // EOF / normal-close / abnormal-close all surface here
			return err              // outer loop decides reconnect vs exit
		}
		applyEvent(state, ev)       // snapshot → replace map; build → upsert by InvocationID
		if jsonMode {
			_ = cli.RenderJSON(out, ev) // NDJSON: one event per line for /verify + `jq -c`
		} else {
			render.Draw(sortedBuilds(state))
		}
	}
}
```

Live rendering & lifecycle details:
- **`applyEvent`:** `type=="snapshot"` ⇒ rebuild the map from `ev.Builds`; `type=="build"` ⇒
  `state[ev.Build.InvocationID] = *ev.Build` (terminal states stay in the map; the renderer may
  hide them after a grace period, but the watch loop never deletes on a "removed" event — there isn't one).
- **Reconnect-with-backoff is the default** (resolves v1's OD-4 toward "reconnect"): the broker is
  launchd-managed and restarts on crash/upgrade, so a one-shot `watch` that dies on every daemon
  restart is a poor terminal dashboard. Backoff is exponential 200ms→5s with jitter, **reset on a
  successful connect**, and **interruptible by Ctrl-C** mid-sleep. A `--once` flag (exit on first
  disconnect instead of reconnecting) is offered for scripted/`/verify` use. *The reconnect-vs-once
  default and the `--once` flag remain an owner-facing decision — see OD-4.*
- **Human mode:** clear screen + cursor-home (`\x1b[2J\x1b[H`) then redraw `RenderBuildsTable`,
  plus a header line (`N building · M queued · updated HH:MM:SS`), where counts come from the
  current `state` map. Gated on `isatty(stdout)`; if not a TTY, fall back to appending a fresh
  table per event.
- **`--json` mode:** emit each event as one JSON object per line (**NDJSON**) — directly
  consumable by `jq -c` and by `/verify` ("see the `snapshot`, then ≥1 `build` event that
  reaches a terminal `state`"). `--json` implies `--once` is NOT set by default; for a bounded
  capture in `/verify`, combine with a timeout or `--once`.
- **Ctrl-C / SIGINT:** `signal.NotifyContext` cancels `ctx` → `conn.Read` returns → clean WS
  close, exit 0. Checked before every reconnect so Ctrl-C during backoff also exits 0.
- **Auth on the WS upgrade:** the bearer header is sent on `DialOptions.HTTPHeader` (E2 requires
  it on `/events`, E2 §2.6). A `401` on upgrade ⇒ `ExitAuth` and **no retry** (retrying a bad
  token just hammers the broker).
- **Initial snapshot is guaranteed by E2, not open.** E2 §2.6 commits to sending a `snapshot`
  frame on connect ("resync contract"); the reconnect design above *depends* on it. v1's OD-5
  (does the broker send a snapshot?) is therefore **resolved by E2** and dropped here — but E6's
  integration test asserts it (§5.1) so a future E2 regression is caught.

### 2.11 `profile` (`cmd_profile.go`) — depends on E4

Resolve the build's Bazel `--profile` path/URL from the broker, then `open` it. **There is no
dedicated `/profile` endpoint in the E2 contract.** Profile resolution piggybacks on
`GET /metrics?invocation_id=…` (E4 owns this route; it already reads `worktree` to locate the
per-invocation `--profile` gz, E2 §4.4). The exact field that carries the Perfetto URL / local
path is **E4-owned and unfrozen — OD-6**; until E4 lands, `/metrics` returns `501` →
`ExitNotImplemented`, so this whole command degrades cleanly with no struct to maintain.

```go
func runProfile(ctx context.Context, c *cli.Client, id string, out io.Writer, opener func(context.Context, string) error) error {
	raw, err := c.Metrics(ctx, id) // GET /metrics?invocation_id=id → json.RawMessage (501 until E4)
	if err != nil {
		return err                  // ExitNotImplemented until E4 fills /metrics
	}
	target, err := extractProfileTarget(raw) // OD-6: which field carries the Perfetto URL / path
	if err != nil || target == "" {
		return wrap(cli.ExitBroker, "broker returned no profile for %s", id)
	}
	if opener == nil { // injectable for tests / --print
		opener = openURL
	}
	if err := opener(ctx, target); err != nil {
		return err
	}
	fmt.Fprintf(out, "opened profile for %s: %s\n", id, target)
	return nil
}

// openURL shells out to macOS `open`. A `--print`/`--json` mode skips opening for headless verify.
func openURL(ctx context.Context, target string) error {
	cmd := exec.CommandContext(ctx, "open", target)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return wrap(cli.ExitOpen, "open %q: %w", target, err)
	}
	return nil
}
```

Flags on `profile`:
- `--print`: print the resolved URL/path and **do not** launch a browser — required for headless
  `/verify` (CI has no GUI). `--json` prints the resolved target as JSON (`{"profile":"…"}`)
  without opening; `--print` prints it bare. Both skip `open`.
- The Perfetto deep-link mechanics (how E4 serves the `.gz` and constructs the
  `ui.perfetto.dev/#!/?url=` link) are an **E4** detail; E6 just opens whatever target E4 returns
  in `/metrics`. **Whether E4 returns a ready `ui.perfetto.dev` URL or a bare local path the CLI
  must wrap is OD-6** — prefer E4 returns the full URL so the CLI stays dumb.

### 2.12 Exit codes (`internal/cli/exit.go`)

Stable, documented codes — the scripting/`/verify` contract. `cliError` carries a code; a
non-`cliError` maps to `1`.

| Code | Constant | Meaning |
|---|---|---|
| 0 | — | success |
| 1 | `ExitUsage` | generic/usage error (also cobra arg errors) |
| 2 | `ExitConfig` | config missing/unreadable/invalid |
| 3 | `ExitUnavailable` | broker unreachable (connection refused/timeout) |
| 4 | `ExitAuth` | broker rejected token (401), incl. on the WS upgrade |
| 5 | `ExitBroker` | broker returned an error (other 4xx incl. 404, any 5xx except 501, or undecodable response) |
| 6 | `ExitNotImplemented` | endpoint reserved on this broker (**HTTP 501**, E3/E4/E5 not landed) |
| 7 | `ExitOpen` | `open` (Perfetto) failed |

> Exit-code disambiguation rests entirely on the HTTP status: **401→4**, **501→6**, every other
> ≥400→5, transport failure→3. This is why E6 needs no path-allowlist: the broker's status code is
> the whole signal. The codes are a stable scripting contract and are asserted in §5.1.

```go
type cliError struct { code int; msg string }
func (e *cliError) Error() string { return e.msg }
func wrap(code int, format string, a ...any) error { return &cliError{code, fmt.Sprintf(format, a...)} }
func ExitCodeFor(err error) int {
	var ce *cliError
	if errors.As(err, &ce) { fmt.Fprintln(os.Stderr, "brokerctl: "+ce.msg); return ce.code }
	fmt.Fprintln(os.Stderr, "brokerctl: "+err.Error())
	return ExitUsage
}
```

---

## 3. Sequencing (ordered, independently verifiable checkpoints)

Each step is buildable and has its own verify. Steps 1–5 need only **E2**; 6–8 light up
features as E3/E4/E5 land but are written and unit-verifiable now against a fake server.

**S1 — Skeleton + root command.** Add `cmd/brokerctl/main.go` + cobra root with persistent
flags, `--version`, `internal/cli/exit.go`. `make brokerctl` builds; `brokerctl --help` and
`brokerctl --version` work. *Verify:* binary builds, help lists all 5 subcommands.

**S2 — Config + client.** `internal/cli/config.go` + `client.go` (HTTP `do`, bearer header,
status→exit-code mapping, **`/builds` unwrap**). *Verify:* unit test loads a temp `config.json`;
missing file → `ExitConfig`; `--port/--token` override config; **config-path resolution order
matches E2** (a test sets `$BAZEL_BROKER_CONFIG` and asserts it wins over `$XDG_CONFIG_HOME`).

**S3 — `ls` against a fake broker.** Implement `ls` + `render.go` (table + `--json`).
Stand up an `httptest.Server` returning canned `{"builds":[…]}` (`wire.BuildsResponse`). *Verify:*
`brokerctl ls --json | jq -e '.builds | length'` parses; table has expected columns (ELAPSED from
`elapsed_ms`, no CACHE column); empty list → "no active builds"; broker down → `ExitUnavailable`.

**S4 — `ls` count mirrors `/healthz` against the real E2 broker.** Wire `Healthz` + cross-check:
`brokerctl ls --json | jq '.builds | length'` equals `/healthz` `.builds`. *Verify (acceptance):*
matches EPIC 6 "ls mirrors /healthz" with E2 running + a `/register`'d fake build.

**S5 — `watch` WS loop + reconnect.** `coder/websocket` dial with bearer header, snapshot-reset +
incremental apply, live table, NDJSON `--json`, **reconnect-with-backoff**, Ctrl-C clean close.
*Verify:* against an `httptest`+WS fake emitting `snapshot`→`build`(running)→`build`(finished);
assert the table reflects each; `--json` emits the matching NDJSON lines; killing the fake
mid-stream triggers a reconnect (assert a second `snapshot` is consumed); SIGINT exits 0;
**401 on upgrade → `ExitAuth`, no retry**.

**S6 — `kill` (E3-gated).** Implement against fake `POST /kill` (`{"invocation_id":…}`).
*Verify (unit):* 2xx → success ack; **`501` → `ExitNotImplemented`**; `404` (unknown id) →
`ExitBroker`. *Verify (e2e, when E3 lands):* fake-bazel long build → `ls` shows it →
`kill <id>` → fake-bazel exits with the cancel code in <1s.

**S7 — `drain`/`resume` (E5-gated).** Implement against `POST /drain` + `POST /resume`;
**`501` → `ExitNotImplemented`**. (No `pause` — not in contract.) *Verify (unit + later e2e with E5).*

**S8 — `profile` (E4-gated).** Implement resolve via `GET /metrics?invocation_id=…` + `open`,
with `--print` for headless. *Verify (unit):* fake `/metrics` returning a profile field →
`--print` emits the target, no `open` call (inject a fake `opener`); `501` → `ExitNotImplemented`;
real run shells `open`. *e2e when E4 lands:* Perfetto auto-loads. **Blocked on OD-6** (profile field name).

**S9 — Polish + docs.** `make brokerctl`, `make verify-brokerctl`, `CLAUDE.md` recipe,
`NO_COLOR`/isatty handling, golden-file tests for table output, exit-code table doc.

---

## 4. Interfaces & contracts

### 4.1 Config file (reused from E2 — `~/.config/bazel-broker/config.json`)
Resolution order (**must match E2's `config.Load`**, E2 §2.5): `--config` flag →
`$BAZEL_BROKER_CONFIG` → `$XDG_CONFIG_HOME/bazel-broker/config.json` →
`~/.config/bazel-broker/config.json`. Fields E6 reads: `port`, `token` (both written by E2 at
first-run). Fields E6 ignores: `disk_cache`, `max_concurrency`, `db_path`, `log_path`. There is
**no `host` field** (loopback implied) and **no `profile_open`**. **E6 never writes this file.**

### 4.2 Endpoints each command calls — **verbatim from E2 §4.2**

| Command | Method + path | Provided by | Behaviour while reserved |
|---|---|---|---|
| `ls` | `GET /builds` → `{"builds":[…]}` | **E2** | n/a (hard dep) |
| `ls` cross-check | `GET /healthz` | **E2** | n/a |
| `watch` | `WS /events` (first frame `snapshot`) | **E2** (`StreamEvents`) | n/a |
| `kill` | `POST /kill` `{"invocation_id":…}` | **E3** | **`501`** → `ExitNotImplemented` |
| `drain` | `POST /drain` | **E5** | **`501`** → `ExitNotImplemented` |
| `resume` | `POST /resume` | **E5** | **`501`** → `ExitNotImplemented` |
| `profile` | `GET /metrics?invocation_id=…` | **E4** | **`501`** → `ExitNotImplemented` |

These map directly to the FROZEN E2 §4.2 route table. **Notable corrections vs v1:** the
degradation signal is **`501`, not `404`**; there is no `/builds/{id}/...`, no `/admission/...`,
no `pause`, and no `/profile` route; `/builds` is **wrapped** in `{"builds":[…]}`.

### 4.3 Shared wire types (`internal/wire`, owned by E2 — IMPORTED)
E6 imports from E2's `internal/wire` (E2 §4.1): `wire.Build`, `wire.BuildsResponse`,
`wire.HealthResponse`, `wire.Event` (+ `EventType`), `wire.ErrorResponse`, and the `State*` /
`Source*` / `Event*` string consts. **E6 does not redefine any of these.** The only types not yet
in `internal/wire` are the success-body shapes for the *still-reserved* endpoints (kill result,
metrics/profile payload); these are **owned by E3/E4** (OD-1, OD-6) — E6 consumes `/kill` for its
status code only and `/metrics` as `json.RawMessage` until E4 freezes the shape, so E6 needs no
speculative structs.

### 4.4 Auth
Every HTTP request and the WS handshake send `Authorization: Bearer <token>` (E2 requires it on
all routes except `GET /healthz`, and on the `/events` upgrade — E2 §2.6/§4). `401 →` `ExitAuth`
(including on the WS upgrade, where it does **not** trigger a reconnect). Loopback-only TCP +
token is the E2 transport decision (`D-stack-2`); E6 inherits it and never sends the token to a
non-loopback host (no `--addr`/`host` knob — see §2.2).

---

## 5. Testing & verification

### 5.1 Unit tests (no broker, fast)
- **config:** valid/missing/malformed; **resolution-order test asserting `$BAZEL_BROKER_CONFIG`
  beats `$XDG_CONFIG_HOME` beats `~/.config`** (must match E2); flag overrides (`--port`,
  `--token`, `--config`); lenient decode ignores unknown E2 keys (`disk_cache`, etc.).
- **client status mapping:** `httptest.Server` returning 200 / 401 / **501** / 404 / 500 →
  assert `ExitConfig`-free codes: 401→`ExitAuth`, 501→`ExitNotImplemented`, 404→`ExitBroker`,
  500→`ExitBroker`. Also assert `/builds` **unwrap** (`{"builds":[…]}` → slice).
- **render:** golden-file tests for the table (fixed-width, sorted, ELAPSED from `elapsed_ms`,
  **no CACHE column**) and for `--json` (re-emits E2's `BuildsResponse` bytes, indented).
- **watch apply:** feed `snapshot` then `build`(running) then `build`(finished) into `applyEvent`;
  assert the final state map upserts by `invocation_id` and keeps the terminal build.
- **watch reconnect:** scripted fake that closes the conn after one event; assert a second
  `snapshot` is consumed and backoff resets; assert `401` on dial → `ExitAuth` with no retry.
- **profile:** inject a fake `opener`; assert `--print` skips it and prints the resolved target;
  `501` from `/metrics` → `ExitNotImplemented`.

### 5.2 Integration with the E2 broker (fake-bazel, ~seconds)
1. Start E2 broker (`launchctl` or `broker --foreground`), which writes `config.json`.
2. `POST /register` a fake build (or launch `testdata/fake-bazel.sh` with a long duration if E3).
3. **`ls` count mirrors `/healthz`:**
   ```sh
   n=$(brokerctl ls --json | jq '.builds | length')
   h=$(curl -s -H "Authorization: Bearer $TOKEN" 127.0.0.1:$PORT/healthz | jq .builds)
   test "$n" = "$h"
   ```
4. **`--json` parseable:** `brokerctl ls --json | jq -e '.builds[0].invocation_id'` exits 0.
5. **`watch` live updates:** run `brokerctl watch --json --once &`, `register` then `deregister`
   a build, assert the captured NDJSON contains the `snapshot` then a `build` event whose
   `.build.state` reaches a terminal value (`finished`).
6. **`kill` e2e (E3):** launch long fake-bazel →
   `id=$(brokerctl ls --json | jq -r '.builds[0].invocation_id')` → `brokerctl kill "$id"` →
   assert the fake-bazel process exited with the cancel code <1s.

### 5.3 `/verify` recipe (CLAUDE.md, `make verify-brokerctl`)
```
make brokerctl                                   # builds binary
make broker && ./bin/broker --foreground &       # E2 daemon (writes config.json + token)
TOKEN=$(jq -r .token ~/.config/bazel-broker/config.json); PORT=$(jq -r .port ~/.config/bazel-broker/config.json)
curl -s -X POST 127.0.0.1:$PORT/register -H "Authorization: Bearer $TOKEN" \
  -d '{"invocation_id":"v1","worktree":"/wt/a","targets":["//app:App"]}'   # seed a build
brokerctl ls --json | jq -e '.builds | type == "array"'   # exit 0
brokerctl ls                                              # eyeball the table
[ "$(brokerctl ls --json | jq '.builds|length')" = "$(curl -s -H "Authorization: Bearer $TOKEN" 127.0.0.1:$PORT/healthz | jq .builds)" ]
brokerctl kill v1; echo "exit=$?"                         # 6 (ExitNotImplemented) until E3; 0 after
# E4: brokerctl profile v1 --print | grep -q perfetto      # (after E4 + OD-6)
```

### 5.4 Acceptance criteria (from EPIC 6 "Done when")
- [ ] `brokerctl ls --json | jq '.builds|length'` mirrors `/healthz` `.builds` (S4/§5.2.3).
- [ ] `brokerctl kill <id>` works end-to-end with fake-bazel (S6, once E3 lands); returns
      `ExitNotImplemented` (6) cleanly before E3.
- [ ] `brokerctl ls --json` is parseable by `jq` against the `{"builds":[…]}` shape (S3/§5.2.4).
- [ ] `make brokerctl` builds the binary; `CLAUDE.md` recipe documented (S9).
- [ ] (richer-with-E4) `brokerctl profile <id>` opens Perfetto; `--print` emits the target headlessly.
- [ ] All commands return the documented exit code on broker-down (`ExitUnavailable`) and on
      reserved-route (`ExitNotImplemented` on 501) (§2.12).

---

## 6. Risks, edge cases, open decisions

### Risks & edge cases (with mitigations)
- **Broker down / not installed.** Distinguish: no config (`ExitConfig`, "is the broker
  installed?") vs config present but connection refused/timeout (`ExitUnavailable`). Both
  are non-crashing, single-line stderr messages.
- **Partial feature availability (E3/E4/E5 not built).** `kill`/`drain`/`resume`/`profile` are
  present in the command tree from day one but return `ExitNotImplemented` (6) with a clear "not
  available on this broker yet" message **on HTTP `501`** (E2 routes the reserved endpoints and
  returns 501). This is the central design move that decouples E6 from E3/E4/E5 — verify it
  explicitly per command. **It requires no path-allowlist and no typed-error convention** because
  the status code is the signal (v1's `isFeaturePath`/OD-3 are eliminated).
- **Output stability for scripting.** The table is for humans and may change; **`--json` is the
  contract** and re-emits the broker's wire JSON exactly — for `ls` that is E2's
  `{"builds":[…]}` (`wire.BuildsResponse`), **not** a bare array. No CLI-side reshaping, so the
  daemon and CLI can't drift. Document "parse `--json`, key off `.builds[]`, never the table."
- **Long-running `watch` connection drops.** WS closes on broker restart (launchd KeepAlive
  cycles the daemon). **Default: reconnect with capped, jittered backoff**, resetting state from
  the `snapshot` E2 sends on every (re)connect (§2.10). `--once` opts out for scripted use.
  `401` on the upgrade is fatal (`ExitAuth`), not retried.
- **`open` in headless/CI.** `open` needs a GUI session; in `/verify`/CI use `profile --print`
  to resolve the target without launching. `ExitOpen` (7) on `open` failure.
- **TTY vs pipe for tables/watch.** Detect `isatty(stdout)`; clear-screen live redraw only on a
  TTY, append-mode otherwise. `--json` is never affected by TTY state.
- **Elapsed is server-computed.** The table's ELAPSED comes from E2's `elapsed_ms` (computed
  server-side from a monotonic-ish clock, E2 §6), **not** `time.Since` on the CLI. This removes
  the clock-skew/negative-elapsed risk entirely; no client-side clamping needed.
- **Version skew (broker older/newer than CLI).** A route the CLI calls but the broker doesn't
  serve returns `404` (→ `ExitBroker`, a real error), distinct from `501` (reserved-but-known).
  This makes "you're running an old broker" diagnosable rather than silently degrading.

### Open decisions (surfaced, NOT resolved here)
> Several v1 ODs were **resolved by reading E2's frozen contract** and are recorded as resolved
> rather than carried, to avoid re-litigating settled questions: ~~OD-3 (404 disambiguation)~~
> — moot, E2 uses 501; ~~OD-5 (snapshot on connect)~~ — E2 §2.6 guarantees it. The remaining
> open items:

- **OD-1 — Reserved-endpoint *response* shapes.** `internal/wire` ownership itself is **settled**
  (E2 owns it; E6 imports it). What is still open is the **success-body shape of `/kill`** (does
  E3 return a body? what fields?) — owned by **E3**. E6 currently treats `/kill` 2xx as a bare
  success. *Escalate to E3 owner; E6 adds a typed return only once E3 defines it in `internal/wire`.*
- **OD-2 — Config strictness.** Recommendation: **lenient** (no `DisallowUnknownFields`) so the
  CLI keeps working as E2 adds config keys per epic (it already has `disk_cache`,
  `max_concurrency`, `db_path`, `log_path` the CLI ignores). *Needs E2 owner sign-off, but the
  lean is clear given E2 owns and grows the file.*
- **OD-4 — `watch` reconnect default.** This plan **defaults to reconnect-with-backoff** (+
  `--once` to opt out) because the broker is launchd-managed and restarts routinely. *Confirm the
  default with the owner; the alternative is exit-on-disconnect with `--follow` to opt in.*
- **OD-6 — Profile retrieval shape (E4).** There is **no `/profile` endpoint**; profile resolution
  rides `GET /metrics`. Open: which field of the metrics payload carries the profile target, and
  does E4 return a ready `ui.perfetto.dev` deep-link or a bare local path the CLI must wrap?
  Prefer E4 returns the full URL so the CLI stays dumb. *Escalate to E4 owner; blocks S8 e2e.*
- **OD-7 — cobra dependency.** Recommendation is cobra (§2.2); if the project wants zero
  third-party deps in the CLI, fall back to stdlib `flag` with a hand-rolled dispatcher.
  Decision belongs to the project owner.

---

## 7. Effort & internal ordering

**Estimated effort:** ~2–3 focused days for the E2-only surface (S1–S5 + S9), plus ~0.5 day
each to wire `kill`, `drain`, `profile` as their backing epics land (S6–S8 are mostly already
written and unit-tested against fakes).

**Internal ordering (critical path):**
1. **S1 → S2** (skeleton + config/client) — everything depends on the client + exit codes.
2. **S3 → S4** (`ls` + render, then `/healthz` cross-check) — delivers the first acceptance
   criterion and the `--json` contract that `/verify` leans on.
3. **S5** (`watch`) — second-highest user value; exercises the WS path and `coder/websocket`.
4. **S6–S8** (`kill`, `drain`, `profile`) — implement now against fakes; their e2e verifies
   unlock automatically when E3/E5/E4 land. Order by dependency readiness, not by S-number.
5. **S9** (docs, golden tests, polish) — last, but `CLAUDE.md` recipe should be stubbed early
   so `/verify` works from S4 onward.

**Parallelism:** S5 (`watch`) and S6–S8 (feature commands) are independent of each other once
S2's client exists and can be split across sessions. S3/S4 are on the critical path and should
land first because they prove the config/client/render stack end-to-end.

**Blocking external work:** none beyond E2 being runnable and exporting `internal/wire` (E2's
T1 — already in its plan). The CLI imports `internal/wire` directly, so S2 can start as soon as
E2's T1 lands. The remaining open items (OD-1 kill-body, OD-6 profile field) block only the
*e2e* verifies of S6/S8, not the unit-tested-against-fakes implementations, so they don't gate
the E2-only critical path (S1–S5, S9).

---

## Staff Engineer Review

*Reviewer: staff eng · Date: 2026-06-17 · Plan revised v1 → v2 in place against the E2 §4 FROZEN contract.*

### (a) Verdict
**Approve with changes — now contract-aligned.** The plan's *shape* was excellent: thin stateless
client, `--json`-as-contract, graceful degradation, clean exit-code table, sensible cobra scoping,
testability against fakes. But v1 was reviewing a broker API it *imagined* rather than the one E2
froze, so nearly every interface was subtly wrong and the central degradation mechanism keyed on
the wrong signal. After the v2 rewrite the plan consumes E2 verbatim and is buildable as written.
**Right-sized** for a CLI — not over-engineered (no Viper, no codegen, stdlib tabwriter) and not
under-engineered (reconnect, isatty, golden tests are all justified).

### (b) Top findings (v1, all fixed in v2)
1. **Degradation keyed on the wrong status.** v1 detected "feature not landed" via a **404 +
   `isFeaturePath` allowlist** and invented OD-3 (typed-error 404 disambiguation). E2 actually
   returns **`501`** for reserved routes (its acceptance criterion is literally "501, not 404").
   This was the single most important error — it inverted the core design move. Fixed: switch on
   `501`; `isFeaturePath` and OD-3 deleted; `404` is now a real error (`ExitBroker`), which as a
   bonus makes version-skew diagnosable.
2. **Wrong endpoint paths, methods, and a non-existent endpoint.** v1: `POST /builds/{id}/kill`,
   `POST /admission/drain`, `/admission/{pause,resume}`, `GET /builds/{id}/profile`. E2: `POST
   /kill` (body `{"invocation_id"}`), `POST /drain`, `POST /resume`, `GET /metrics?invocation_id=`.
   **There is no `/profile` endpoint and no `pause`.** Fixed: all paths corrected; `profile` now
   rides `GET /metrics`; `pause` dropped.
3. **`/builds` shape mismatch.** E2 returns `{"builds":[…]}` (`wire.BuildsResponse`); v1 decoded
   and re-emitted a **bare array**, breaking both the client decode and the `jq 'length'` verify.
   Fixed: unwrap in the client; `ls --json` echoes `{"builds":[…]}`; all `jq` recipes use `.builds`.
4. **Wire-type duplication / wrong package + wrong field names.** v1 invented an `internal/api`
   package and a `BuildInfo{id, started_at, cache_hit_ratio, has_profile}` struct. E2 **owns
   `internal/wire`** with `wire.Build{invocation_id, start_time, elapsed_ms, …}` and keeps metrics
   in a **separate table**. Fixed: import `internal/wire`; correct field names; drop the CACHE
   column (deferred to a post-E4 `--metrics` opt-in, OD-7); use server-computed `elapsed_ms`
   (which also deletes v1's clock-skew edge case).
5. **Invented config fields + wrong resolution order.** v1 added `host` and `profile_open` and
   resolved `$XDG_CONFIG_HOME` first. E2's config is loopback-only (no `host`), has no
   `profile_open`, and resolves **`$BAZEL_BROKER_CONFIG` first**. Fixed: `--addr` → `--port`,
   loopback hard-coded, resolution order matched to E2 (with a dedicated test — this is the most
   likely silent-misconfig bug, since launchd sets `$BAZEL_BROKER_CONFIG` in the plist).
6. **Wrong WS event model.** v1 invented `build_update`/`build_removed`/`build_id`. E2 emits only
   `snapshot` + `build` (terminal builds transition state, never "removed"). Fixed `applyEvent`
   semantics and the verify assertions.
7. **`watch` reconnect was left as an open question with a fragile default.** Fixed: default to
   **reconnect-with-backoff**, made lossless by E2's guaranteed on-connect `snapshot`; `--once`
   to opt out; `401`-on-upgrade is fatal-no-retry; backoff is jittered, capped, and Ctrl-C-interruptible.

### (c) What I changed
- Added a v2 contract-alignment banner enumerating the authoritative E2 facts.
- Rewrote every endpoint, type, field name, and the config struct to match E2 §4 verbatim;
  switched the degradation/exit-code logic from 404 to 501; corrected the `/builds` unwrap and the
  `ls --json` shape; replaced `internal/api`/`BuildInfo` with imported `internal/wire`/`wire.Build`.
- Replaced `--addr` with `--port` (loopback-only), aligned config resolution order to E2, removed
  invented config fields, removed the `pause` command and CACHE column.
- Rewrote `watch` with an outer reconnect loop (backoff + `--once` + snapshot-reset) and the
  corrected two-type event model; reworked `profile` to use `/metrics` with an injectable opener.
- Updated all of §3 (S2/S5/S6/S7/S8), §4 (4.1–4.4), §5 (every test + `/verify` recipe + acceptance
  criteria), and §6 (risks + ODs); pruned resolved ODs (OD-3, OD-5) and re-scoped OD-1/OD-6 to the
  genuinely-open *response-shape* questions owned by E3/E4. Preserved the 7-section structure.

### (d) Decisions / risks to escalate
- **OD-1 (→ E3 owner):** does `POST /kill` return a body, and with what fields? E6 treats 2xx as a
  bare success until E3 defines the shape in `internal/wire`. *Low risk; unblocks a nicer kill ack.*
- **OD-6 (→ E4 owner):** **blocks S8 e2e.** There is no `/profile` route; the profile target must
  come from `GET /metrics`. Confirm (i) which metrics field carries it and (ii) that E4 returns a
  ready `ui.perfetto.dev` URL (preferred) rather than a bare path the CLI must wrap.
- **OD-2 (→ E2 owner):** confirm **lenient** config decoding (the lean, since E2 owns and grows the
  file with per-epic keys the CLI ignores).
- **OD-4 (→ project owner):** confirm **reconnect-with-backoff** as the `watch` default vs
  exit-on-disconnect. This plan defaults to reconnect (broker is launchd-cycled) with `--once` opt-out.
- **OD-7 (→ project owner):** cobra vs stdlib `flag`. Recommendation stands at cobra.
- **Cross-epic flag for the consolidated review:** E6 v1's drift is evidence that the **E2 §4
  contract should be cited by SHA/section in every consuming epic (E3/E4/E5/E7/E8)**, and that a
  single shared `internal/wire` import (not per-epic struct copies) is enforced — otherwise each
  front-end will re-derive a slightly-wrong API. Recommend a contract-conformance test in E2 that
  the CLI/web/app build against.
