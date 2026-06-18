# E7 — Local web dashboard

> ⚠️ **Superseded where it conflicts with [`00-consolidated-review.md`](00-consolidated-review.md) + E2 §4 (frozen contract).** Conform before coding:
> - **P2:** kill → `POST /builds/{invocation_id}/kill`. **P3:** WS has only **two** types `snapshot`/`build` (upsert-by-`invocation_id`); state is `running`; the cache field is `cache_hit_ratio`.
> - **OD-B (blocking, E2↔E7 joint sign-off before T5):** browser auth to the token API — recommend Option A (same-origin `HttpOnly; SameSite=Strict` cookie E2's middleware also accepts + CSRF on POSTs). A modifies E2's auth.

> A glanceable browser view of in-flight Bazel builds, **served by the broker itself**
> from an embedded static page — no separate deploy, no npm/build step. The broker
> serves HTML + vanilla JS + a vendored uPlot over a localhost HTTP server; the page
> opens a WebSocket to the broker for the live build list, exposes kill buttons, draws
> cache% / duration charts, and deep-links into Perfetto.

Status: **Draft v2** · Owner: Antonis · Maps to architecture §5 C5b, §11 M5 · Epic E7 in `02-epics.md`
Depends on: **E2** (Broker daemon core — HTTP+WS API + bearer token). Best with **E4** (BEP metrics for charts).
Last updated: 2026-06-17

> **Conforms to E2's FROZEN §4 contract.** E2-broker-core.md §4 is the authoritative, frozen
> cross-epic API contract; this epic was reconciled against it in v2. In particular: the WS
> envelope is `{type:"snapshot"|"build", build|builds, seq, ts}` (two types only — **not**
> the `build_started/updated/finished/removed` deltas an earlier draft guessed); the running
> state is `"running"` (**not** `"building"`); build timestamps are `start_time` (RFC3339) +
> server-computed `elapsed_ms`; and **cache%/profile come from E4's `/metrics`, not from
> E2's `wire.Build`**. The browser-auth question is an **open, escalated E2↔E7 decision**
> (see OD-1): E2's frozen middleware accepts **only** a bearer token today, so the
> recommended cookie path (Option A) is a *change to E2* that must be co-signed, not a free
> E7-local choice.

---

## 1. Goal & scope recap

**Goal (from 02-epics.md E7):** a glanceable browser view served by the broker at
`http://127.0.0.1:PORT/` that shows live builds, lets you kill them, and visualizes cache%.

**In scope**
- Broker serves a self-contained static UI from a Go `embed.FS` mounted on the existing
  E2 HTTP server (same loopback listener, same process — no second port, no static-file
  server, no reverse proxy).
- **Live build list** rendered from the E2 `/events` WebSocket, which (per E2 §2.6/§4.1)
  sends a **`snapshot` frame on connect** then incremental **`build` upsert frames**. The
  one-shot `GET /builds` fetch on load is a belt-and-suspenders seed (the WS `snapshot`
  already self-syncs the UI), and the resync path after a reconnect.
- **Kill buttons** per row → `POST /kill` with `{"invocation_id":"…"}` (E2 §4.2 body shape),
  carrying whatever auth OD-1 lands on. Note `/kill` is **routed in E2 but owned by E3** —
  it returns `501` until E3 lands, so E7's kill button must tolerate a `501` (render disabled
  / "kill unavailable") in addition to `401`.
- **Charts** (E4 dependency): cache hit% and build durations via **uPlot**, loaded from a
  single vendored `uplot.iife.min.js` + `uplot.min.css` embedded in the binary. CSS-bar
  fallback when E4 is not yet wired (so E7 ships usefully on E2 alone).
- **Perfetto deep-links**: per-build "open profile" link to the E4 Perfetto-serving endpoint.
- **No build step**: plain HTML + ES2020 vanilla JS. No bundler, no transpile, no `node_modules`.

**Out of scope (belongs to other epics)**
- The WS event schema (`wire.Event`), `GET /builds`, `POST /kill`, the bearer token, and the
  loopback listener are **E2 deliverables** (frozen in E2 §4) — E7 consumes them, does not
  define them. `/kill` is reserved by E2 and implemented by **E3**.
- BEP parsing, the metrics SQLite shape, the `cache.hit_ratio`/`timing` fields, and the
  Perfetto-serving endpoint are **E4 deliverables** — E7 only reads E4's `GET /metrics` and
  the `profile.perfetto_url` it supplies. **`cache_hit_pct`/`profile_url` are NOT fields on
  E2's `wire.Build`** — E7 joins them in from E4 by `invocation_id`.
- Admission/queue controls (Pause/Resume/Drain) are **E5**; E7 may show queued state if E2
  emits it but adds no admission UI in this epic.
- The menu-bar app is **E8** (independent sibling).

**Design stance:** dependency-light, embeddable, instant. The whole UI is three static files
plus one vendored chart lib, all compiled into the `broker` binary. Opening the page is the
entire "deploy".

---

## 2. Design & implementation details

### 2.1 Embedded asset file layout

All web assets live under the broker command package so `go:embed` can reach them. (E0/E2
establish `cmd/broker`; E7 adds the `web/` subtree and one Go file.)

```
cmd/broker/
  main.go                 # E2 — wires the HTTP server (E7 calls RegisterWebUI on its mux)
  api.go                  # E2 — /healthz, /builds, /events, /kill, /metrics handlers + token mw
  webui.go                # E7 — embed.FS + RegisterWebUI(mux) + same-origin auth shim
  web/
    index.html            # E7 — the page (table + chart canvases + script tag)
    app.js                # E7 — WS client, render loop, kill fetch, uPlot setup
    app.css               # E7 — minimal layout (dark, monospace, glanceable)
    vendor/
      uPlot.iife.min.js   # E7 — vendored uPlot (single IIFE file, ~40KB) — committed, pinned
      uPlot.min.css       # E7 — vendored uPlot styles
      uPlot.LICENSE        # E7 — MIT license text (vendoring hygiene)
```

`vendor/` holds the **committed, version-pinned** uPlot files (fetch once during E7 task 1;
record the exact release tag, e.g. `uPlot 1.6.x`, in a comment at the top of `webui.go` and
in `uPlot.LICENSE`). No CDN at runtime — the page must work fully offline / air-gapped.

### 2.2 embed.FS wiring + http handlers (`cmd/broker/webui.go`)

```go
package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// uPlot vendored at release v1.6.x (MIT). See web/vendor/uPlot.LICENSE.
//go:embed web/index.html web/app.js web/app.css web/vendor/*
var webFS embed.FS

// RegisterWebUI mounts the dashboard on the broker's existing loopback mux.
// `cfg` carries the loopback origin + the same-origin auth strategy (see §2.4).
func RegisterWebUI(mux *http.ServeMux, cfg WebUICfg) error {
	// Strip the leading "web/" so URLs are "/", "/app.js", "/vendor/uPlot.iife.min.js".
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(sub))

	// GET / and static assets. No bearer-auth on the *static* files themselves (they carry
	// no secrets); the data endpoints (/builds, /events, /kill, /metrics) stay token-
	// protected by E2's middleware. See §2.4 for how the browser presents the token.
	// NOTE (OD-1 interaction): if Option A is chosen, GET / (or /session) is also where the
	// broker MINTS + Set-Cookies the session; if Option B is chosen, index.html must be
	// SERVED THROUGH A TEMPLATE (not a raw embed.FS file) so the per-process token can be
	// injected into <meta name="broker-token">. Either way the static handler gains a small
	// auth-aware responsibility under A/B — keep it behind WebUICfg.AuthMode.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only serve known asset paths from "/"; everything else 404s rather than
		// leaking the file tree or shadowing API routes (which are registered on
		// their own patterns and win by ServeMux longest-prefix matching).
		setSecurityHeaders(w)
		if r.URL.Path == "/" {
			r.URL.Path = "/index.html"
		}
		fileServer.ServeHTTP(w, r)
	})
	return nil
}

func setSecurityHeaders(w http.ResponseWriter) {
	// Tight CSP: only self, no inline event handlers, no external origins.
	// uPlot is a separate file (not inline), so 'unsafe-inline' is NOT needed for scripts.
	w.Header().Set("Content-Security-Policy",
		strings.Join([]string{
			"default-src 'none'",
			"script-src 'self'",
			"style-src 'self'",
			"img-src 'self' data:",
			"connect-src 'self'", // XHR + ws:// to same origin
			"frame-ancestors 'none'",
		}, "; "))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
```

**Mux interaction with E2:** `http.ServeMux` matches by longest registered pattern. E2's
data endpoints register exact patterns (`/healthz`, `/builds`, `/events`, `/kill`,
`/metrics`); E7 registers the catch-all `/`. The exact patterns win, so the UI's `/` handler
only ever sees static-asset requests. **E7 must not register any pattern that collides with
an E2 route** — keep all UI assets under `/` and `/vendor/`, charts/data strictly via the
E2 endpoints.

**`main.go` change (E7 adds one line to E2's server setup):**

```go
// after E2 builds `mux` and registers its API routes:
if err := RegisterWebUI(mux, webUICfg); err != nil {
	log.Fatalf("web ui: %v", err)
}
```

### 2.3 The page + WS client JS

`web/index.html` (no inline JS/CSS — CSP-clean):

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Bazel Broker</title>
  <link rel="stylesheet" href="/vendor/uPlot.min.css">
  <link rel="stylesheet" href="/app.css">
</head>
<body>
  <header>
    <h1>Bazel Broker</h1>
    <span id="status" class="dot offline" title="WS state"></span>
    <span id="summary">— building, — queued</span>
  </header>

  <table id="builds">
    <thead><tr>
      <th>worktree</th><th>targets</th><th>state</th><th>elapsed</th>
      <th>cache%</th><th>profile</th><th></th>
    </tr></thead>
    <tbody id="rows"></tbody>
  </table>

  <section class="charts">
    <figure><figcaption>cache hit %</figcaption><div id="chart-cache"></div></figure>
    <figure><figcaption>build duration (s)</figcaption><div id="chart-dur"></div></figure>
  </section>

  <script src="/vendor/uPlot.iife.min.js"></script>
  <script src="/app.js" type="module"></script>
</body>
</html>
```

`web/app.js` — the live client. Key behaviours: seed via `GET /builds` (which returns
`{"builds":[…]}`, E2 §4.1), stream via `/events` (a `snapshot` frame then `build` upserts),
auto-reconnect with backoff, re-render an elapsed-time ticker locally (so rows tick without
needing an event every second).

> **Schema alignment (E2 §4.1, frozen):** the WS envelope is `wire.Event`:
> `{ "type": "snapshot"|"build", "seq": <uint>, "ts": "<RFC3339>", "build"?: Build, "builds"?: [Build] }`.
> There are **only two event types**: `snapshot` (full list on connect, in `builds[]`) and
> `build` (a single created/updated/terminated build, in `build`). There is no
> `build_started/updated/finished/removed`, and no explicit "removed" event — terminal builds
> arrive as `build` frames with a terminal `state` and age out client-side. So `applyEvent`
> is a plain **upsert-by-`invocation_id`** keyed on `msg.type`. Build state strings are
> `queued|running|finished|failed|killed|unknown` (**`running`, not `building`**); timestamps
> are `start_time` (RFC3339) and the server-computed `elapsed_ms`. (This resolves OD-3 against
> E2's frozen contract — see §4 / §6.)

```js
// ----- config: same-origin; token handling per §2.4 (OD-1) -----
const API = "";                       // same-origin
const wsURL = (location.protocol === "https:" ? "wss://" : "ws://")
            + location.host + "/events";

const TERMINAL = new Set(["finished", "failed", "killed", "unknown"]);
const ACTIVE   = new Set(["queued", "running"]);

// ----- auth helper (see §2.4 for which branch the broker chooses, OD-1) -----
// Option A (recommended) — same-origin HttpOnly cookie: fetch()/WebSocket send it
//   automatically and authHeaders() is empty (REQUIRES an E2 middleware change — co-sign).
// Option C (localhost relaxation): also empty.
// Option B (token-in-page): the broker injects the token into <meta name="broker-token">
//   and we send it as a bearer header; the WS upgrade must carry it via subprotocol/query
//   since browsers can't set WS headers. THE BROKER PICKS — see OD-1.
function authHeaders() {
  const meta = document.querySelector('meta[name="broker-token"]');
  return meta ? { "Authorization": "Bearer " + meta.content } : {};
}
// WS auth: cookie (A/C) rides the upgrade automatically. For Option B only, the token must
// ride the subprotocol (browsers cannot set an Authorization header on a WebSocket):
function wsProtocols() {
  const meta = document.querySelector('meta[name="broker-token"]');
  return meta ? ["broker-bearer", meta.content] : []; // E2/E3 must accept this if B is chosen
}

const state = new Map();              // invocation_id -> Build (wire.Build shape) + joined E4 fields

async function seed() {
  const r = await fetch(API + "/builds", { headers: authHeaders() });
  if (!r.ok) { showApiError(r.status); return; }       // 401 (auth) — surface, then WS retries
  const { builds } = await r.json();                   // E2 returns {"builds":[...]}
  for (const b of builds ?? []) upsert(b);
  renderTable();
  refreshCharts();
}

let ws, backoff = 500;
function connect() {
  ws = new WebSocket(wsURL, wsProtocols());            // protocols empty unless Option B
  ws.onopen = () => { setDot("online"); backoff = 500; };
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);     // wire.Event: {type, seq, ts, build?, builds?}
    applyEvent(msg);
    renderTable();
  };
  ws.onclose = () => {
    setDot("offline");
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, 10_000);   // capped exponential backoff
  };
  ws.onerror = () => ws.close();
}

function upsert(b) {
  state.set(b.invocation_id, { ...state.get(b.invocation_id), ...b });
}

function applyEvent(msg) {
  switch (msg.type) {
    case "snapshot":                     // full resync (sent on connect; lossless after a drop)
      state.clear();
      for (const b of msg.builds ?? []) upsert(b);
      refreshCharts();
      break;
    case "build":                        // create/update/terminate — upsert by id
      upsert(msg.build);
      if (TERMINAL.has(msg.build.state)) refreshCharts();
      break;
  }
}

function renderTable() {
  const rows = document.getElementById("rows");
  rows.replaceChildren();              // simple full re-render; build counts are small (<dozens)
  let running = 0, queued = 0;
  for (const b of [...state.values()].sort(byStartTimeDesc)) {
    if (b.state === "running") running++;
    if (b.state === "queued")  queued++;
    rows.appendChild(renderRow(b));
  }
  document.getElementById("summary").textContent = `${running} building, ${queued} queued`;
}

function renderRow(b) {
  const tr = document.createElement("tr");
  tr.dataset.start = b.start_time;     // for the local elapsed ticker
  tr.append(
    td(b.worktree), td((b.targets ?? []).join(" ")), td(b.state),
    td(fmtElapsed(b)),                  // from elapsed_ms (server-computed) + local tick
    td(fmtPct(b.cache_hit_pct)),        // joined in from E4 /metrics; undefined → "—"
    profileCell(b), killCell(b),
  );
  return tr;
}

function killCell(b) {
  const td = document.createElement("td");
  if (ACTIVE.has(b.state)) {
    const btn = document.createElement("button");
    btn.textContent = "kill";
    btn.onclick = () => killBuild(b.invocation_id, btn);
    td.append(btn);
  }
  return td;
}

async function killBuild(id, btn) {
  btn.disabled = true; btn.textContent = "killing…";
  const r = await fetch(API + "/kill", {                // E2 §4.2 body: {"invocation_id":"…"}
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ invocation_id: id }),
  });
  if (r.status === 501) { btn.textContent = "kill (unavailable)"; return; } // E3 not landed yet
  if (!r.ok) { btn.disabled = false; btn.textContent = "kill (retry)"; showApiError(r.status); return; }
  // success: E3 transitions the build to "killed" → E2 emits a `build` WS frame → row updates itself.
}

// local elapsed ticker so rows tick every second without server chatter (between WS frames)
setInterval(() => {
  for (const tr of document.querySelectorAll("#rows tr")) { /* recompute elapsed from data-start */ }
}, 1000);

seed();
connect();
```

### 2.4 Browser-to-token-API auth (the key design point — open E2↔E7 decision, escalated)

E2 protects its data endpoints with a **bearer token** and, per its **frozen §4 contract**,
the `auth` middleware accepts **only** `Authorization: Bearer <token>` (constant-time compare;
`/healthz` exempt). E2 explicitly leaves *browser* auth for E7 **open and escalated** (E2 §2.6:
"browsers in E7 use a same-origin fetch of a short-lived token or send it via the
`Sec-WebSocket-Protocol` shim — flagged as an E7 concern, not solved here"; E2's header banner
also lists "browser auth for E7" as one of two genuinely-open escalated decisions).

> **The load-bearing coupling:** none of the three options below is purely E7-local.
> **Option A and Option C both require changing E2's frozen auth middleware** (A adds a
> cookie-acceptance branch; C adds a loopback/Origin exemption). Only **Option B is
> implementable with zero E2 change** (it reuses the existing bearer check). So OD-1 is a
> **cross-epic E2↔E7 decision that must be co-signed by E2's owner**, and — because E2's §4
> is frozen — picking A or C means **amending the frozen contract**, which is a deliberate,
> reviewed act, not a quiet E7 toggle. E7 implements behind a single `WebUICfg.AuthMode`
> switch so whichever option is chosen is localized, but the *acceptance rule lives in E2*.

A browser page cannot read the token from `~/.config/bazel-broker/` the way `brokerctl` can,
so one of these must be true:

- **Option A — same-origin session cookie (E7's recommendation; REQUIRES an E2 change).**
  The broker, on `GET /` (or a `GET /session` bootstrap), sets `Set-Cookie:
  broker_session=<random>; HttpOnly; SameSite=Strict; Path=/` whose value it maps internally
  to "authorized" (a short-lived, in-memory session minted from the real token, so the bearer
  token itself never derives from or appears in the cookie). **E2's `auth` middleware must be
  extended to also accept this session cookie** for same-origin requests, and the WS upgrade
  authenticates via the same cookie (cookies ride the upgrade; no header needed). `fetch()`
  and `WebSocket` send it automatically — nothing in the DOM to leak. Pairs with a CSRF guard
  on the `POST /kill` mutation (OD-2). **Pro:** token never enters the page; standard browser
  auth; clean WS story. **Con:** a real, reviewed change to E2's *frozen* middleware — must be
  co-signed by E2's owner and recorded as a §4 amendment.

- **Option B — token injected into the page (zero E2 change).**
  The broker templates the bearer token into a `<meta name="broker-token">` tag (or a
  `/session` JSON the page fetches) at serve time; `authHeaders()` reads it and sends
  `Authorization: Bearer …` on every request. The WS upgrade carries the token via
  `Sec-WebSocket-Protocol` (the `wsProtocols()` shim in §2.3) — browsers **cannot** set an
  `Authorization` header on a WebSocket. **Pro:** reuses E2's existing frozen bearer check
  verbatim — no E2 amendment. **Con:** the live token sits in the DOM, readable by any injected
  script (mitigated, not eliminated, by the strict CSP in §2.2); and the token rides the WS
  subprotocol, so E2/E3's WS accept must tolerate that subprotocol value (a small but real
  E2-side touch for the WS path).

- **Option C — localhost relaxation (REQUIRES an E2 change).**
  E2 exempts requests whose `RemoteAddr` is loopback **and** `Origin`/`Host` match the broker's
  own `127.0.0.1:PORT` from the token requirement. **Pro:** simplest in JS — no cookie, no DOM
  token. **Con:** any local process / any local page that can reach the loopback port (malicious
  cross-origin POST, DNS-rebinding) could hit the API; must be paired with strict `Origin`/`Host`
  allow-listing + CSRF defense. Weakest isolation on a shared/multi-user Mac, and still an E2
  middleware change. (E2 already constrains the WS `OriginPatterns` to `127.0.0.1:*`/`localhost:*`,
  which helps but does not by itself authenticate.)

**E7's recommendation into the OD-1 discussion:** **Option A** (session cookie + CSRF token on
mutations). It keeps the token out of the page, gives the cleanest WS-upgrade auth, and the
required E2 change is small and contained. **But the recommendation is explicitly NOT a
decision** — A touches E2's frozen contract, so it is escalated to the E2↔E7 owners to co-sign
(see OD-1 in §6). If the project prefers to keep E2's §4 strictly frozen for now, **Option B**
ships E7 on E2 *as-is* (at the cost of token-in-DOM), and is the fallback. The §2.3
implementation abstracts all three through `authHeaders()` + `wsProtocols()` + automatic cookie
handling, so the choice is a `WebUICfg.AuthMode` flip on E7's side plus the acceptance rule on
E2's side.

### 2.5 uPlot chart setup (E4 dependency)

Two small synchronous charts, fed from E4's `Metrics`/history endpoint. uPlot is chosen
because it's a single vendored file, no build step, tiny, and fast (canvas). Setup:

```js
let cacheChart, durChart;

function makeCharts() {
  const optsCache = {
    width: 480, height: 160,
    scales: { x: { time: false }, y: { range: [0, 100] } },
    series: [{}, { label: "cache hit %", stroke: "#4caf50", width: 2 }],
    axes: [{}, { values: (u, ticks) => ticks.map(t => t + "%") }],
  };
  const optsDur = {
    width: 480, height: 160,
    series: [{}, { label: "duration (s)", stroke: "#2196f3", width: 2 }],
  };
  cacheChart = new uPlot(optsCache, [[], []], document.getElementById("chart-cache"));
  durChart   = new uPlot(optsDur,   [[], []], document.getElementById("chart-dur"));
}

async function refreshCharts() {
  // E4 endpoint (E4 §2.6): GET /metrics with NO `id` returns the list form {"builds":[...]}
  // of trimmed rows. (E2 reserves /metrics and returns 501 until E4 lands — handle both.)
  const r = await fetch(API + "/metrics", { headers: authHeaders() });
  if (!r.ok) return;                         // 404/501 → E4 absent → charts stay empty, table still works
  const { builds } = await r.json();         // each row carries cache.hit_ratio (0..1) + timing.wall_time_ms
  const rows = (builds ?? []).slice(-50);
  const xs   = rows.map((_, i) => i);
  // E4's cache.hit_ratio is a 0..1 fraction → ×100 for the percentage axis.
  cacheChart.setData([xs, rows.map(d => (d.cache?.hit_ratio ?? 0) * 100)]);
  durChart.setData([xs,   rows.map(d => (d.timing?.wall_time_ms ?? 0) / 1000)]);

  // Per-row cache% join: fold E4's hit_ratio onto the matching build in `state` by id,
  // so the table's cache% column lights up even though it isn't on E2's wire.Build.
  for (const d of builds ?? []) {
    const b = state.get(d.invocation_id);
    if (b) { b.cache_hit_pct = (d.cache?.hit_ratio ?? null) === null ? undefined : d.cache.hit_ratio * 100;
             b.profile_url = d.profile?.perfetto_url ?? b.profile_url; }
  }
}
```

> **Metrics-shape alignment (E4 §2.6):** E4's `/metrics` does **not** return a flat
> `[{x, cache_hit_pct, duration_s}]` array. Per build it returns a nested object with
> `cache.hit_ratio` (a **0..1 fraction**, ×100 for display), `cache.actions_*` counts,
> `timing.wall_time_ms`, and a `profile.perfetto_url`. The keyed single-build form is
> `GET /metrics?id=<invocation_id>` (param **`id`**, not `invocation_id`/`recent`); the
> no-`id` form returns the paged list `{"builds":[…]}`. E7 reads the list for the charts +
> the per-row cache% join, and `profile.perfetto_url` for the Perfetto link (§2.6).

**Graceful degradation:** if `/metrics` returns `404` **or `501`** (E4 not yet merged — E2
reserves the route as a 501 stub), `refreshCharts()` returns early and the cache% column /
charts render empty — the live table + kill still function on E2 alone. This keeps E7
shippable against E2 and richer once E4 lands.

### 2.6 Perfetto deep-link

Per-build "profile" cell links to the E4-served profile. E4 (§2.9) serves the build's
`--profile` `.gz` over localhost (`GET /profile/{id}/{name}`, CORS-allowed for
`https://ui.perfetto.dev`) and supplies a ready-made deep-link as **`profile.perfetto_url`**
in the `/metrics` payload. E7 reads it (joined onto `state` in `refreshCharts()`, §2.5) and
renders an anchor:

```js
function profileCell(b) {
  const td = document.createElement("td");
  if (b.profile_url) {             // joined from E4's metrics.profile.perfetto_url (NOT on E2's wire.Build)
    const a = document.createElement("a");
    a.href = b.profile_url;        // e.g. https://ui.perfetto.dev/#!/?url=<encoded localhost /profile url>
    a.textContent = "perfetto";
    a.target = "_blank"; a.rel = "noopener";
    td.append(a);
  }
  return td;
}
```

> **Note the CSP interaction:** the anchor *navigates* to `ui.perfetto.dev` in a new tab (a
> top-level navigation), which the strict §2.2 CSP allows — `frame-ancestors`/`connect-src`
> restrict *embedding* and *fetch*, not link navigation. Perfetto then fetches the trace from
> the broker's localhost `/profile/...`; that cross-origin fetch is governed by E4's
> `Access-Control-Allow-Origin: https://ui.perfetto.dev` header, not E7's CSP.

The exact deep-link form (`ui.perfetto.dev/#!/?url=…` over the broker's `/profile/<id>`)
is **E4's** to define and supply; E7 just consumes `metrics.profile.perfetto_url`.

---

## 3. Sequencing (ordered, independently verifiable checkpoints)

Each task is small and verifiable on its own. T1–T5 need only **E2**; T6 needs **E4**.

- **T1 — Vendor uPlot + scaffold the asset tree.** Add `cmd/broker/web/{index.html,app.css}`
  (static placeholder), `web/vendor/uPlot.{iife.min.js,min.css}` (pinned release), and
  `uPlot.LICENSE`. *Verify:* files committed; `index.html` is valid HTML; record the uPlot
  version in `webui.go` header + LICENSE.

- **T2 — embed.FS + RegisterWebUI handler.** Add `cmd/broker/webui.go` with the `go:embed`
  directive, `RegisterWebUI(mux, cfg)`, and `setSecurityHeaders`. Wire the one line into
  E2's `main.go`. *Verify:* `make build` succeeds; `curl -s http://127.0.0.1:PORT/` returns
  the HTML; `curl -I .../vendor/uPlot.iife.min.js` returns `200` with `Content-Type:
  text/javascript`; CSP header present.

- **T3 — seed table from `/builds`.** Implement `seed()` + `renderTable()` + `renderRow()`
  in `app.js`; render the build list from a one-shot `/builds` fetch. *Verify:* with the
  broker running and a (fake) build Register'd, loading `/` shows the row with worktree +
  state.

- **T4 — live WS updates + reconnect.** Implement `connect()`, `applyEvent()` (handling the
  `snapshot` + `build` frame types per E2 §4.1), the elapsed ticker, and the status dot with
  capped backoff. *Verify:* start a long fake-bazel build → row appears via WS without reload
  as a `build` frame (`state:"running"`); finish it → row transitions to `finished` via a
  `build` frame; restart the broker → dot goes offline then reconnects and the `snapshot`
  frame rebuilds the table.

- **T5 — kill button + auth wiring.** Implement `killCell()` + `killBuild()` and the chosen
  `AuthMode` path (OD-1). *Verify:* clicking "kill" on a `running` row makes the build exit
  with the cancel code (<1s, matching E3's fake-bazel SIGINT behaviour) and the row transitions
  to `state:"killed"` over WS. Confirm the request carries auth (cookie or bearer) and a
  missing/forged token is rejected by E2 (`401`); confirm the button tolerates `501` while E3
  is not yet merged. *Note:* T5's full pass needs **E3** (which fills `/kill`); against E2
  alone the button correctly shows "kill (unavailable)" on the `501`.

- **T6 — charts + cache% column + Perfetto link (needs E4).** Implement `makeCharts()`,
  `refreshCharts()`, the `cache%` cell, and `profileCell()`. *Verify:* after a real build,
  the cache% column and uPlot cache chart match Bazel's own summary (cross-check via E4's
  `Metrics`/`brokerctl`); the "perfetto" link opens the profile.

- **T7 — `/verify` recipe + CLAUDE.md.** Add a Make target and a CLAUDE.md "how to run &
  verify" entry (headless curl checks + the headless-browser check from §5). *Verify:* the
  documented recipe passes end-to-end.

---

## 4. Interfaces & contracts

E7 is a **pure consumer** of E2 and E4. It defines no new wire protocol.

### Consumes from E2 (must exist before E7 ships) — per E2's FROZEN §4
| Endpoint | Method | E7 use |
|---|---|---|
| `GET /builds` | HTTP GET → `{"builds":[wire.Build]}` | seed the table on load + resync |
| `WS /events` | WebSocket → `wire.Event` frames | live build list updates |
| `POST /kill` | HTTP POST `{"invocation_id":"…"}` | kill button (E2-reserved; **E3** fills it; `501` until then) |
| `GET /healthz` | HTTP GET → `{status,builds,queued,total,version,uptime_ms}` | (optional) header summary fallback (no auth) |
| bearer token + loopback listener | E2 auth (D-stack-2) | the auth mechanism E7 must satisfy (OD-1) |

**`wire.Build` fields E7 reads** (frozen in E2 §4.1 — these are the exact names):
`invocation_id`, `worktree`, `targets` (string array), `state`
(**`queued|running|finished|failed|killed|unknown`** — note `running`, not `building`),
`start_time` (RFC3339 UTC), `elapsed_ms` (server-computed; E7 does **not** compute elapsed
from client time), `end_time` (RFC3339, omitted until terminal), `exit_code`, `source`.
**`cache_hit_pct` and `profile_url` are NOT on `wire.Build`** — E7 joins them in from E4's
`/metrics` by `invocation_id` (§2.5).

**WS event envelope (frozen — E2 §4.1 `wire.Event`):**
`{ "type": "snapshot"|"build", "seq": <uint>, "ts": "<RFC3339>", "build"?: wire.Build, "builds"?: [wire.Build] }`.
**Only two types:** `snapshot` (full list in `builds[]`, sent once on connect) and `build`
(one created/updated/terminated build in `build`). There is no `build_started/updated/
finished/removed` and no explicit removal event — terminal builds arrive as `build` frames
with a terminal `state` and age out client-side. E7's `applyEvent()` is a plain
upsert-by-`invocation_id`; the `snapshot`-on-connect contract makes reconnect resync lossless
(E2 §2.6). *This was OD-3; it is now decided by E2's frozen contract — see §6.*

### Consumes from E4 (charts / cache% / Perfetto — optional, graceful-degrade) — per E4 §2.6
| Endpoint | Shape E7 expects | E7 use |
|---|---|---|
| `GET /metrics` (no `id`, list form) | `{"builds":[{invocation_id, cache:{hit_ratio (0..1), actions_*}, timing:{wall_time_ms}, profile:{perfetto_url}}]}` | both uPlot charts + per-row cache% join |
| `GET /metrics?id=<invocation_id>` | single nested metrics object (E4 §2.6) | per-build drill-down (future) |
| `metrics.profile.perfetto_url` | `ui.perfetto.dev/#!/?url=<broker /profile/...>` deep-link | "perfetto" anchor |

`cache.hit_ratio` is a **0..1 fraction** (E7 multiplies by 100 for the % axis/column);
duration is `timing.wall_time_ms` (÷1000 for seconds). E7's `refreshCharts()` degrades to
empty charts on `404`/`501` so E7 does not block on E4. The exact `/metrics` list pagination
is **E4's to finalize**; E7 reads at most the last 50 rows.

### Auth mechanism E7 needs from E2 (OD-1 — cross-epic, escalated)
E7 needs E2 to support **one** browser-presentable auth path. **E2's frozen middleware today
accepts only `Authorization: Bearer`** — so:
- **Option A (cookie, recommended):** E2's `auth` middleware **must change** to accept a
  same-origin session cookie (and the WS upgrade via that cookie). *Amends E2 §4.*
- **Option B (token-in-page):** **no change to E2's bearer check**; E2/E3's WS accept must
  tolerate the token riding `Sec-WebSocket-Protocol`. The only option that ships on E2 as-is.
- **Option C (loopback exemption):** E2's middleware **must change** to exempt loopback+Origin.
See §2.4 and OD-1. **Do not resolve here — co-sign with E2's owner.**

---

## 5. Testing & verification

### Manual / interactive
- **Live builds over WS:** run the broker + 2 long `fake-bazel` builds → open `/` → both rows
  appear (`state:"running"`) and tick without reload; finishing one updates its row to
  `finished` live via a `build` frame.
- **Kill works (needs E3):** click "kill" → the fake build exits with the cancel code and the
  row transitions to `killed` over WS. On E2-only (E3 not merged) the button shows
  "kill (unavailable)" on the `501`.
- **Cache% renders (E4):** run a real build, reload → cache% column + uPlot chart match
  Bazel's own cache-hit summary (cross-check with `brokerctl`/E4 `/metrics`; remember E4's
  `cache.hit_ratio` is 0..1, ×100 for display).

### Headless `/verify` recipe (curl — no browser)
```bash
# 1. static page served + CSP present
curl -fsS  http://127.0.0.1:$PORT/ | grep -q '<title>Bazel Broker</title>'
curl -fsSI http://127.0.0.1:$PORT/ | grep -qi 'content-security-policy'
# 2. vendored uPlot served with a JS content-type
curl -fsSI http://127.0.0.1:$PORT/vendor/uPlot.iife.min.js | grep -qi 'text/javascript\|application/javascript'
# 3. data endpoints still token-protected (unauthenticated request is rejected)
test "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/builds)" = "401"
# 4. authenticated seed works (token/cookie per OD-1 choice) — E2 returns {"builds":[...]}
curl -fsS -H "Authorization: Bearer $BROKER_TOKEN" http://127.0.0.1:$PORT/builds | jq -e '.builds | type=="array"'
# 5. /kill tolerates the reserved-route stub before E3 lands: 501 (reserved) or 200/4xx (E3 present)
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $BROKER_TOKEN" \
  -X POST http://127.0.0.1:$PORT/kill -d '{"invocation_id":"x"}')
case "$code" in 501|200|400|404) ;; *) echo "unexpected /kill code: $code"; exit 1;; esac
```

> `$BROKER_TOKEN` = `jq -r .token ~/.config/bazel-broker/config.json` (E2 §2.5). The curl
> checks exercise the bearer path regardless of which OD-1 option the *browser* uses; the
> browser-specific cookie/subprotocol path is covered by the headless-browser check below.

### Headless browser check (asserts JS actually runs + WS connects)
A minimal automated check that exercises the JS path (the curl checks can't). Use the
available **chrome-devtools-mcp** (or a tiny headless Chrome run via the
`chrome-devtools-mcp:chrome-devtools-cli` skill) to:
1. navigate to `http://127.0.0.1:$PORT/`,
2. assert the WS status dot becomes `online` (WS connected),
3. assert at least one `#rows tr` exists after a fake build is registered,
4. click the kill button and assert the row transitions / the fake build exits.

Keep this optional/secondary to the curl recipe so `/verify` stays fast and dependency-light;
the curl checks are the gating ones.

### Acceptance criteria (from E7 "Done when")
1. Opening the page shows **live builds updating over WS** — covered by T3/T4 + headless WS check.
2. **Kill button works** — covered by T5 + fake-bazel cancel-code assertion.
3. **cache% renders from E4 metrics** — covered by T6 + cross-check vs Bazel summary.

---

## 6. Risks, edge cases, open decisions

### Open decisions (do NOT resolve unilaterally — surface to owners)
- **OD-1 (THE key decision) — browser-to-token-API auth. CROSS-EPIC E2↔E7; ESCALATED.**
  Option A session cookie (E7's recommendation) vs B token-in-page vs C localhost relaxation
  (§2.4). **This is not an E7-local toggle:** A and C **amend E2's FROZEN §4 auth middleware**;
  only **B ships on E2 as-is** (reusing the existing bearer check, with E2/E3's WS accept
  tolerating the token in `Sec-WebSocket-Protocol`). E2's own plan already lists "browser auth
  for E7" as one of its two genuinely-open, escalated decisions — so this is a *shared* open
  decision, and choosing A/C is a deliberate, reviewed amendment to a frozen contract, co-signed
  by E2's owner. **E7 recommends A; fallback B if §4 must stay frozen now.** Resolve before T5.
- **OD-2 — CSRF on the `POST /kill` mutation.** On localhost, a malicious web page in the
  same browser could attempt a cross-origin POST to `127.0.0.1:PORT/kill`. Mitigations to
  choose between: (a) `SameSite=Strict` cookie (blocks cross-site cookie send) + strict
  `Origin`/`Host` allow-list check on mutations (E2 already pins WS `OriginPatterns` to
  `127.0.0.1:*`/`localhost:*` — extend the same allow-list to mutating HTTP); (b) a CSRF token
  echoed from `/session`; (c) require the bearer header (Option B) which a cross-origin page
  cannot forge without the token. Tie to the OD-1 choice. Lands on E2's side. **Surface; don't
  decide here.**
- **OD-3 — WS event shape — RESOLVED by E2's frozen §4.1 (no longer open).** E2 freezes the
  envelope as two types, `snapshot` (full list on connect) + `build` (single upsert). E7's
  earlier `build_started/updated/finished/removed` guess is **superseded**; §2.3 now does a
  plain upsert-by-id and relies on the `snapshot`-on-connect resync. *Kept here only as a
  pointer; no decision needed.*
- **OD-4 — charts: uPlot vs CSS bars.** Epic text allows "uPlot **or** CSS bars". uPlot
  recommended for the duration trend; if the no-extra-file constraint is valued over trend
  charts, CSS bars need zero vendoring. Default proposed: **uPlot vendored**.

### Risks & edge cases
- **No-build-step vs charting:** uPlot is the only third-party asset; vendoring one pinned
  IIFE file (no npm, no transpile) preserves the constraint. Pin the version and commit the
  license. *Risk:* future uPlot API drift — mitigated by pinning and an offline copy. *Realism
  check:* `<script src="/app.js" type="module">` + `<script src="/vendor/uPlot.iife.min.js">`
  (a global-exposing IIFE, loaded **before** the module so `uPlot` is on `window` when the
  module runs) is genuinely build-step-free in evergreen browsers; ES2020 + native modules
  need no transpile for a localhost-only, single-developer tool. The only true constraint is
  "modern Chrome/Safari," which is fine for this audience.
- **WS reconnect / broker restart:** handled by capped exponential backoff; resync is via E2's
  **`snapshot`-on-connect frame** (E2 §2.6) — `applyEvent("snapshot")` clears + rebuilds `state`,
  so a fresh connection is immediately consistent without a separate `/builds` round-trip. The
  on-load `seed()` fetch is belt-and-suspenders. Edge: events missed during the gap are
  reconciled by the next `snapshot`. *Also note E2's slow-consumer policy:* if E7's WS buffer
  backs up, **E2 drops the connection** (closes it) rather than blocking — E7's `onclose`
  backoff + reconnect + `snapshot` recovers cleanly, which is exactly why E2 made `snapshot`
  lossless-enough.
- **WS-in-browser can't set headers:** if OD-1 picks Option B (bearer), the WS upgrade must
  carry the token via `Sec-WebSocket-Protocol` (the `wsProtocols()` shim, §2.3), not an
  `Authorization` header — and **E2/E3's `websocket.Accept` must echo/accept that subprotocol**.
  Option A (cookie) avoids this entirely (cookie rides the upgrade). Noted as a reason to
  prefer A, and as a concrete E2-side touch even under B.
- **DNS-rebinding / cross-origin reach to the loopback port:** mitigated by strict CSP
  (`connect-src 'self'`, `frame-ancestors 'none'`) + `Origin`/`Host` allow-listing on the
  API (E2 side) — especially important if OD-1 chooses Option C.
- **ServeMux collision with E2 routes:** E7's catch-all `/` must never shadow E2 data routes;
  guaranteed by ServeMux longest-prefix matching as long as E7 registers no exact API
  patterns. Documented in §2.2 as a hard constraint.
- **Many builds / re-render cost:** full table re-render is fine for the expected scale
  (a handful of worktrees, < dozens of builds). If this grows, switch to keyed row diffing —
  not needed now.
- **Finished rows lifecycle:** terminal builds (`finished|failed|killed|unknown`) arrive as
  ordinary `build` WS frames (no removal event) and linger in `state` for the charts, then age
  out **client-side** (E7 prunes its own `Map` after a window). E2 keeps all builds for the
  process lifetime + the most recent N=200 terminal rows from SQLite on boot (E2 §2.3), and a
  reconnect `snapshot` will re-include recent terminals — so E7's client-side ageing is purely
  cosmetic and must be idempotent against re-delivery (a re-seen terminal id just re-upserts).
  Confirm the cosmetic window with the desired glance UX; it does not need to match E2's
  retention exactly.

---

## 7. Effort & internal ordering

**Dependencies:** hard on **E2** (T1–T5, T7). **The kill *action* needs E3** — E2 only
*reserves* `/kill` (returns `501`); **E3** implements it. So on E2 alone E7 ships a useful
**live table** (seed + WS updates) with the kill button present-but-disabled (`501` →
"kill unavailable"); the button goes live when **E3** merges, and charts/cache% light up when
**E4** merges. (Correction vs v1, which implied kill worked on E2 alone.)

**Suggested order & rough effort** (1 engineer):
1. T1 vendor uPlot + scaffold — ~0.5 day.
2. T2 embed.FS + handler + CSP + main.go wiring — ~0.5 day.
3. T3 seed table from `/builds` — ~0.5 day.
4. T4 WS live updates + reconnect + ticker — ~1 day (the bulk of the JS).
5. T5 kill button + OD-1 auth wiring — ~0.5–1 day (auth coordination with E2 dominates).
6. **(gate on E4)** T6 charts + cache% + Perfetto link — ~1 day.
7. T7 verify recipe + CLAUDE.md — ~0.5 day.

**Total ≈ 4–5 engineer-days**, of which ~3 days (T1–T5, T7) are unblocked by E2 alone and
~1 day (T6) waits on E4. Critical path item is **OD-1** (auth) — resolve it with E2's owner
before starting T5 so the `WebUICfg.AuthMode` switch and any E2 middleware change land
together.

---

## Staff Engineer Review

*Reviewer: staff eng · 2026-06-17 · scope: E7 against E2's frozen §4 contract and E4 §2.6, per
architecture §5 C5b. This review revised the plan in place (v1 → v2).*

### (a) Verdict
**Approve with changes — now consistent with the frozen contracts after this revision.** The
core architecture is sound and right-sized: a static `embed.FS` mounted on E2's existing
loopback mux, vanilla ES2020 + one vendored uPlot, WS-driven table, graceful E4 degradation,
no build step. The design stance matches the project's "dependency-light, self-contained,
single-Mac" goals. **But the v1 draft consumed E2's API by guessed shapes that did not match
E2's now-frozen §4** — wrong WS envelope, wrong state enum, wrong field names, and a metrics
shape that did not match E4 — and it under-stated the browser-auth ↔ E2 coupling. Those are
corrected here. The one genuinely-open item (OD-1 browser auth) is correctly **left open and
escalated**, not decided.

### (b) Top findings
1. **WS envelope was wrong (highest impact).** v1 expected
   `build_started/updated/finished/removed` deltas; E2 §4.1 freezes **two** types only —
   `snapshot` (full list on connect) + `build` (single upsert). v1's `applyEvent()` would have
   matched *zero* real frames → a dead table. *(This also retro-resolves the plan's own OD-3.)*
2. **State enum was wrong.** v1 keyed on `state === "building"`; E2's frozen enum is
   `queued|running|finished|failed|killed|unknown`. "building" never appears → counts and
   kill-eligibility both broken.
3. **Field names + layering were wrong.** v1 read `started_at`, `cache_hit_pct`, `profile_url`
   off the build record. E2's DTO is `start_time` + server-computed `elapsed_ms`; **cache% and
   profile are E4 fields**, not E2's `wire.Build` — they must be *joined in* from `/metrics`.
4. **E4 metrics shape was wrong.** v1 fetched `/metrics?recent=50` expecting a flat
   `[{x,cache_hit_pct,duration_s}]`. E4 §2.6 returns nested objects with `cache.hit_ratio`
   (**0..1**, not 0..100), `timing.wall_time_ms`, `profile.perfetto_url`; the keyed form is
   `?id=`, and the list form returns `{"builds":[…]}`.
5. **Browser-auth ↔ E2 coupling under-stated (the key issue).** v1 framed Option A (cookie) as
   "a small E2 tweak." In fact **E2's §4 is frozen and its middleware accepts ONLY a bearer
   token**; A and C **amend the frozen contract**, and **only Option B ships on E2 as-is**. E2's
   own plan already lists "browser auth for E7" as an open, escalated decision — so OD-1 is a
   *shared* E2↔E7 decision, and choosing A/C is a reviewed amendment, not an E7 toggle.
6. **`/kill` ownership.** v1 implied kill works "on E2 alone." E2 only *reserves* `/kill`
   (`501`); **E3** implements it. The button must tolerate `501`, and E7's kill story depends on
   E3, not just E2.
7. **Minor:** `/healthz` shape (full object, not `{builds,queued}`); WS-subprotocol auth (Option
   B) needs E2/E3's `websocket.Accept` to accept the subprotocol; `index.html` must be
   *templated* (not raw `embed.FS`) if Option B injects a `<meta>` token; reconnect resync is
   E2's `snapshot` frame, not a re-`seed()`; finished-row ageing is cosmetic/client-side and must
   be idempotent against `snapshot` re-delivery.

### (c) What I changed
- **§2.3 JS client (largest change):** rewrote `applyEvent()` to the frozen `snapshot`+`build`
  envelope (upsert-by-id, `snapshot` clears+rebuilds); fixed the state enum (`running`, sets
  `ACTIVE`/`TERMINAL`); fixed `GET /builds` to read `{"builds":[…]}`; switched elapsed to
  server `elapsed_ms`; added a `wsProtocols()` shim for Option-B WS auth; made `killBuild()`
  tolerate `501`; corrected the success-path comment to "E3 → `killed` → WS `build` frame."
- **§2.4 auth:** reframed as an **open, escalated E2↔E7 decision**; stated explicitly that A/C
  **amend E2's frozen middleware** and **only B is zero-E2-change**; kept A as E7's
  *recommendation* (not decision), B as the frozen-contract fallback.
- **§2.5 charts:** corrected to E4's nested shape (`cache.hit_ratio` ×100, `timing.wall_time_ms`
  ÷1000, list form `{"builds":[…]}`, param `id`), added the per-row cache%/profile join, and
  degrade on `404`/`501`.
- **§2.6 Perfetto:** sourced the link from E4's `metrics.profile.perfetto_url`; clarified the
  CSP-vs-navigation-vs-CORS interaction.
- **§2.2:** noted the Option-A cookie-mint / Option-B templating responsibility on `GET /`.
- **§4 contract tables:** rewrote both consumed-from-E2 and consumed-from-E4 tables to the exact
  frozen shapes/field names; OD-3 marked resolved-by-contract.
- **§3/§5/§6/§7:** aligned task verifies and acceptance to the real states/frames; fixed the
  curl recipe (`.builds` array; `/kill` 501-tolerance); sharpened OD-1 as cross-epic; corrected
  the "kill on E2 alone" dependency claim (needs E3); added a no-build-step realism note and an
  idempotent finished-row-ageing note.
- Added a **v2 banner** at the top recording conformance to E2 §4.

### (d) Decisions / risks to escalate
- **OD-1 (ESCALATE — cross-epic E2↔E7).** Browser auth: **A (cookie, recommended) and C both
  amend E2's frozen §4 middleware; only B ships on E2 as-is.** Must be co-signed by E2's owner
  and recorded as a §4 amendment if A/C is chosen. This is the single gating decision before T5;
  it is the same item E2's plan already flags as open. **Recommend A; accept B as the frozen-
  contract-preserving fallback.**
- **OD-2 (ESCALATE — lands on E2).** CSRF on `POST /kill`. Tie to OD-1; reuse E2's existing
  `Origin` allow-list (already `127.0.0.1:*`/`localhost:*` for WS) for mutating HTTP.
- **Cross-epic dependency to surface in the consolidated review:** E7's kill button depends on
  **E3** (not E2); its charts/cache% on **E4**. E7 ships a *live table* on E2 alone — set
  expectations accordingly in the milestone plan.
- **Risk (low, watch):** if Option B is chosen, the live bearer token sits in the DOM — accept
  only because the tool is loopback-only + single-developer + strict CSP; revisit if the threat
  model widens (multi-user Mac), which would also reopen E2's D-stack-2 (Unix socket).
- **Non-blocker:** OD-4 (uPlot vs CSS bars) — uPlot vendored is fine and build-step-free; no
  escalation needed.
