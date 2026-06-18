// Bazel Broker dashboard — vanilla ES2020, no build step.
//
// Auth: same-origin session cookie (OD-B, Option A). The page POSTs the broker
// token once to /login; the broker sets an HttpOnly; SameSite=Strict cookie that
// fetch() and WebSocket send automatically. The token never lives in the DOM.
// Mutating requests (kill) echo the CSRF token from /login (double-submit).
//
// Data contract (E2 FROZEN §4): GET /builds → {"builds":[api.Build...]}; WS
// /events sends EXACTLY two frame types — {"type":"snapshot","builds":[...]} once
// on connect, then {"type":"build","build":{...}} upserts. Heartbeats are WS ping
// frames (handled by the browser), never JSON. applyEvent handles only these two.

const TERMINAL = new Set(["finished", "failed", "killed", "gone", "unknown"]);
const ACTIVE = new Set(["queued", "running"]);

const state = new Map(); // invocation_id -> api.Build
let csrfToken = "";
let ws = null;
let backoff = 500;

const $ = (id) => document.getElementById(id);

// ---------- auth / login ----------

async function checkSession() {
  // /api/csrf returns 200 + the CSRF token if a valid session cookie is present.
  const r = await fetch("/api/csrf", { credentials: "same-origin" });
  if (r.ok) {
    csrfToken = (await r.json()).csrf || "";
    return true;
  }
  return false;
}

async function login(token) {
  const r = await fetch("/login", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
  if (!r.ok) return false;
  csrfToken = (await r.json()).csrf || "";
  return true;
}

async function logout() {
  await fetch("/logout", { method: "POST", credentials: "same-origin" });
  if (ws) { ws.onclose = null; ws.close(); ws = null; }
  showLogin();
}

function showLogin() {
  $("login").hidden = false;
  $("dashboard").hidden = true;
  $("logout").hidden = true;
  $("token").focus();
}

function showDashboard() {
  $("login").hidden = true;
  $("dashboard").hidden = false;
  $("logout").hidden = false;
}

// ---------- seed + live updates ----------

async function seed() {
  const r = await fetch("/builds", { credentials: "same-origin" });
  if (!r.ok) return; // 401 → session expired; WS will also fail and we re-gate.
  const { builds } = await r.json();
  state.clear();
  for (const b of builds ?? []) upsert(b);
  renderTable();
}

function wsURL() {
  const proto = location.protocol === "https:" ? "wss://" : "ws://";
  return proto + location.host + "/events";
}

function connect() {
  ws = new WebSocket(wsURL());
  ws.onopen = () => { setDot(true); backoff = 500; };
  ws.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }
    applyEvent(msg);
    renderTable();
  };
  ws.onerror = () => { try { ws.close(); } catch {} };
  ws.onclose = () => {
    setDot(false);
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, 10000);
  };
}

function upsert(b) {
  if (!b || !b.invocation_id) return;
  state.set(b.invocation_id, { ...state.get(b.invocation_id), ...b });
}

// applyEvent handles ONLY the two frozen frame types: snapshot (full resync,
// clears + rebuilds) and build (upsert-by-invocation_id). Anything else is ignored.
function applyEvent(msg) {
  switch (msg.type) {
    case "snapshot":
      state.clear();
      for (const b of msg.builds ?? []) upsert(b);
      break;
    case "build":
      upsert(msg.build);
      break;
    // no build_started/updated/finished/removed; heartbeats are ping frames.
  }
}

// ---------- rendering ----------

function setDot(online) {
  const dot = $("status");
  dot.classList.toggle("online", online);
  dot.classList.toggle("offline", !online);
  $("offline-banner").hidden = online;
}

function byStartDesc(a, b) {
  return (b.start_time || "").localeCompare(a.start_time || "");
}

function renderTable() {
  const rows = $("rows");
  rows.replaceChildren();
  let running = 0, queued = 0;
  const builds = [...state.values()].sort(byStartDesc);
  for (const b of builds) {
    if (b.state === "running") running++;
    if (b.state === "queued") queued++;
    rows.appendChild(renderRow(b));
  }
  $("summary").textContent = `${running} running · ${queued} queued`;
  $("empty").hidden = builds.length > 0;
}

function td(text, cls) {
  const el = document.createElement("td");
  if (cls) el.className = cls;
  el.textContent = text;
  return el;
}

function renderRow(b) {
  const tr = document.createElement("tr");
  tr.dataset.iid = b.invocation_id; // the local ticker looks the build up in `state`

  tr.append(
    td(b.worktree_name || b.worktree || "—", "worktree"),
    td((b.targets ?? []).join(" ") || "—", "targets"),
    stateCell(b),
    elapsedCell(b),
    cacheCell(b),
    profileCell(b),
    killCell(b),
  );
  return tr;
}

function stateCell(b) {
  const cell = document.createElement("td");
  const span = document.createElement("span");
  const st = b.state || "unknown";
  span.className = "state state-" + st;
  span.textContent = st;
  cell.append(span);
  return cell;
}

function elapsedCell(b) {
  const cell = document.createElement("td");
  cell.className = "elapsed";
  cell.textContent = fmtElapsed(b);
  return cell;
}

// fmtElapsed uses the server-computed elapsed_ms as the baseline and, for active
// builds, adds local wall-clock drift so rows tick between WS frames.
function fmtElapsed(b) {
  let ms = b.elapsed_ms ?? 0;
  if (ACTIVE.has(b.state) && b.start_time) {
    const started = Date.parse(b.start_time);
    if (!Number.isNaN(started)) ms = Date.now() - started;
  }
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  return m + "m" + String(s % 60).padStart(2, "0") + "s";
}

// cacheCell renders cache_hit_ratio (0–1) as a CSS bar + percentage. Null/absent
// until E4 populates it → "—".
function cacheCell(b) {
  const cell = document.createElement("td");
  const r = b.cache_hit_ratio;
  if (r === null || r === undefined) {
    cell.className = "muted";
    cell.textContent = "—";
    return cell;
  }
  const pct = Math.round(Math.max(0, Math.min(1, r)) * 100);
  const wrap = document.createElement("div");
  wrap.className = "cache";
  const bar = document.createElement("div");
  bar.className = "cache-bar";
  const fill = document.createElement("div");
  fill.className = "cache-fill";
  fill.style.width = pct + "%";
  bar.append(fill);
  const label = document.createElement("span");
  label.className = "cache-pct";
  label.textContent = pct + "%";
  wrap.append(bar, label);
  cell.append(wrap);
  return cell;
}

// profileCell renders the ready-to-open Perfetto deep-link from profile_url
// (fully formed by E4; absent until then).
function profileCell(b) {
  const cell = document.createElement("td");
  if (b.profile_url) {
    const a = document.createElement("a");
    a.href = b.profile_url;
    a.textContent = "perfetto";
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    cell.append(a);
  } else {
    cell.className = "muted";
    cell.textContent = "—";
  }
  return cell;
}

function killCell(b) {
  const cell = document.createElement("td");
  if (ACTIVE.has(b.state)) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = "kill";
    btn.addEventListener("click", () => killBuild(b.invocation_id, btn));
    cell.append(btn);
  }
  return cell;
}

async function killBuild(id, btn) {
  if (!confirm(`Kill build ${id}?`)) return;
  btn.disabled = true;
  btn.textContent = "killing…";
  const r = await fetch(`/builds/${encodeURIComponent(id)}/kill`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "X-Broker-CSRF": csrfToken },
  });
  if (r.status === 501) { btn.textContent = "unavailable"; return; } // E3 not landed
  if (!r.ok) {
    btn.disabled = false;
    btn.textContent = "kill (retry)";
    return;
  }
  // Success: E3 transitions the build → killed → E2 emits a `build` WS frame that
  // re-renders this row. Leave the button disabled meanwhile.
  btn.textContent = "killed";
}

// ---------- local elapsed ticker (active rows tick without server chatter) ----------
// Reads the authoritative build from `state` rather than reconstructing it from
// data-* attributes, and only touches the DOM while builds are actually active.

setInterval(() => {
  for (const tr of document.querySelectorAll("#rows tr")) {
    const b = state.get(tr.dataset.iid);
    if (!b || !ACTIVE.has(b.state)) continue;
    const cell = tr.querySelector(".elapsed");
    if (cell) cell.textContent = fmtElapsed(b);
  }
}, 1000);

// ---------- boot ----------

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("login-error");
  err.hidden = true;
  const ok = await login($("token").value.trim());
  if (!ok) {
    err.textContent = "Invalid token.";
    err.hidden = false;
    return;
  }
  $("token").value = "";
  start();
});

$("logout").addEventListener("click", logout);

async function start() {
  showDashboard();
  await seed();
  connect();
}

// fragmentToken reads a one-shot token from the URL fragment (#token=...) used by
// `brokerctl dashboard` / `make up` to open the page pre-authenticated. The fragment
// is never sent to the server (so it is not logged); we consume it then strip it
// from the address bar before doing anything else.
function fragmentToken() {
  const m = /(?:^|[#&])token=([^&]+)/.exec(location.hash);
  if (!m) return "";
  history.replaceState(null, "", location.pathname + location.search);
  return decodeURIComponent(m[1]);
}

(async function boot() {
  if (await checkSession()) {
    start();
    return;
  }
  const t = fragmentToken();
  if (t && (await login(t))) {
    start();
    return;
  }
  showLogin();
})();
