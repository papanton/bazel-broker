# Bazel Broker — Consolidated Cross-Epic Review (Round 3)

> Capstone review after Round 1 (9 author subagents) and Round 2 (9 staff-engineer-review
> subagents revised each plan in place). This document reconciles the **cross-epic conflicts**
> that the per-epic reviews — running in parallel — could not see, and is the **binding
> tie-breaker** where two epic plans disagree.

Reviewer: main agent · Date: 2026-06-17 · Scope: E0–E8 as one integrated system.

---

## 0. Authority statement (read first)

When any epic plan disagrees with another, precedence is:

1. **E2 §4 ("FROZEN cross-epic API contract")** — the wire/types/routes/events authority.
2. **This document (§3 below)** — for conflicts E2 doesn't settle (build harness, BEP path,
   cache-metric semantics, signals) and for conflicts where a consumer review *crossed* E2.
3. The individual epic plan — for everything internal to that epic.

Several Round-2 reviewers revised their files *in parallel with* E2's freeze and therefore
**re-introduced drift E2 had just resolved** (most importantly the shared package name). Those
are listed in §3 and must be patched before implementation — see §6.

---

## 1. Overall verdict

**GO, with a mandatory pre-implementation reconciliation pass (§6).** The architecture is sound,
the decomposition is clean, the dependency graph is acyclic and correctly ordered, and every epic
now has code-level detail and a `/verify` recipe. The Round-2 reviews were high-value: they caught
**real, ship-blocking defects** (a SIGINT-ignored-when-backgrounded cancel bug in E0; an
`exec`-skips-`trap` slot-leak and a mutex deadlock in E5; an inverted cache-hit formula in E4;
wrong libproc buffer sizes in E3; a wrong Swift prefix-map flag in E1). None of those are
architectural — they're correctness fixes, now applied.

The remaining risk is **not** within any epic; it's the **seams between them**. Nine authors
guessing one API produced ~6 incompatible spellings of every shared shape. E2 froze the contract,
but the freeze and the consumer revisions crossed in flight. The §6 patch list closes that gap.

---

## 2. Per-epic verdicts (from the Round-2 staff reviews)

| Epic | Verdict | Headline defect caught & fixed |
|---|---|---|
| **E0** Scaffolding | ship-with-changes | Backgrounded bash **ignores SIGINT** → fake-bazel exited 0 not 8; switched cancel to **SIGTERM**. Also aligned E0's "frozen contracts" to E2 (it had drifted on ~7 names). |
| **E1** Cache config | approve-with-changes | `--swiftcopt=-ffile-prefix-map` is the **wrong spelling** (swiftc uses single-dash `-file-prefix-map`); rewrote to rules_apple/rules_swift **features-first**. |
| **E2** Broker core | approve-with-mandatory-changes | Was *not a usable contract*: 6 route spellings, 4 WS taxonomies, field drift, `/admission` self-contradiction. **§4 frozen.** |
| **E3** Discovery & kill | approve-with-changes | `proc_pidpath` needs **4096** buf not 1024; errno collapsed to -1 (EPERM≠ESRCH); client/server heuristic unsound (use cwd, not exe). |
| **E4** BEP metrics | approve-with-changes | Cache metric **inverted/contaminated** (`created−executed` is wrong); switched to `ActionSummary.runner_count[]`. Perfetto `?url=` needs HTTPS → postMessage shim. |
| **E5** Admission & wrapper | approve-with-changes | `exec` skips the `trap EXIT` that releases the slot (throughput collapse); unbuffered verdict send **deadlocks** the engine. Both fixed; wrapper **fail-open**. |
| **E6** brokerctl CLI | approve-with-changes | Degradation keyed on 404 but E2 returns **501**; every endpoint path/field re-aligned to E2 §4. |
| **E7** Web dashboard | approve-with-changes | WS event names matched **zero** real frames (E2 emits only `snapshot`/`build`); browser-auth is a real E2↔E7 decision. |
| **E8** Menu-bar app | approve-with-changes | Codable models wouldn't decode a single real payload; conformed field-for-field; `building`→`running`. |

No epic is **needs-rework**. All are buildable once §6 lands.

---

## 3. Cross-epic conflicts & authoritative resolutions

### C1 — Shared Go package name: **`internal/api`** (not `internal/wire`) — BINDING
**The single most important unreconciled conflict.** E2 (the contract owner) froze the shared
types package as **`internal/api`** and explicitly *retired* `internal/wire`. But the E6 and E8
Round-2 reviewers — believing E2 owned `wire` — renamed their files **to** `internal/wire`
(grep: E6 = 23 refs, E8 = 4 refs, E0 = 8 stray, even E2 retains 4 in "retired" prose).
**Resolution: the package is `internal/api`; the type is `api.Build`.** `internal/wire`/`wire.Build`
are wrong wherever they appear in a consumer plan and must be renamed (§6, P1). E2's mentions of
`internal/wire` are deliberate "this name is retired" prose and must NOT be blindly replaced.

### C2 — Kill route: **`POST /builds/{invocation_id}/kill`** — BINDING (E2 §4.2)
E2 froze the nested per-build path. E3, E6, E7 still say `POST /kill` in many places (grep: E3=6,
E6=8, E7=6). **Resolution: `POST /builds/{invocation_id}/kill`.** Patch E3/E6/E7 (§6, P2). Same
convention applies to `GET /builds/{id}/metrics` and `GET /builds/{id}/profile`.

### C3 — WS event envelope: **exactly two types `snapshot` + `build`** — BINDING (E2 §2.6/§4.1)
Upsert-by-`invocation_id`; read `state` for lifecycle; **no** `build_started/updated/finished/
removed/added`; heartbeats are WS **ping frames**, not JSON events; `metrics`/`alert` reserved for
E4. E7's `applyEvent` and E8's event enum must use only these two (mostly done in their revisions;
verify in P3).

### C4 — Field names & state value — BINDING (E2 §4.1, §4.6)
Frozen: `invocation_id` (not `id`), `start_time` (not `started_at`), `cache_hit_ratio` 0–1 float
ptr (not `cache_hit_pct`/`cache_hit_rate`/percent), state **`running`** (not `building`). E2 §4.6
tabulates the per-consumer renames; apply them (§6, P3). `profile_url` is a fully-formed URL
**E4 builds** — clients just `open` it (resolves E6-OD6, E8-D5).

### C5 — `POST /admission` returns **status-code + one-word body** (not JSON) — RESOLVED
`200 ALLOW` / `202 QUEUE` / `403 DENY`; any other / unreachable → wrapper **fails open**. E5's
bash 3.2 wrapper can't parse JSON. E2's old "{decision}" stub note was the bug and is corrected.
Consistent across E2 §4.2 and E5. ✔

### C6 — Kill signal ↔ fake-bazel exit code — NEEDS A DECISION (see §4, OD-A)
E0 changed fake-bazel to trap **SIGTERM** (a backgrounded script ignores SIGINT) and exit **8**.
E3's kill state machine sends **SIGINT → grace → SIGKILL**. In the E3 test harness fake-bazel runs
**backgrounded**, so SIGINT is ignored and the process dies by **SIGKILL (137)**, not the graceful
**code 8** — so E3's "kills with cancel code in <1s" criterion would pass on *timing* but the
**exit-code assertion across E0/E3/E5 is inconsistent**. This is a genuine harness-vs-real-tool gap
(real bazel client *does* honor SIGINT in the foreground). **Must be reconciled before E3 — see
OD-A for the recommended convention.**

### C7 — BEP file path contract (E1 ↔ E4) — RESOLVED, E1 authoritative
E1 locked a **per-worktree, relative, reused** path (`<worktree>/.bazel-broker/bep.json`) and
declined per-invocation filenames; the per-output-base lock makes it collision-safe. E4 conformed:
the **truncation supervisor is mandatory** (truncation is certain on each rebuild, not a race) and
the registry join key is **`BuildStarted.uuid`**, not the filename. ✔ (Lock the exact path string
in P4 so E1 and E4 agree byte-for-byte.)

### C8 — Cache-hit metric definition — RESOLVED, align E1 to E4
E4 (correctly) derives hits from **`ActionSummary.runner_count[]`** (the structured form of Bazel's
"N processes: X disk cache hit…" line); `actions_created − actions_executed` is wrong (a disk-cache
hit is *inside* `actions_executed`). E1's measurement script uses the human summary line — same
underlying number, but **both must report the same definition**. Align E1's `measure.sh` wording to
the runner_count semantics (P4).

### C9 — Perfetto serving & `/profile` auth — RESOLVED mechanism, one open sub-decision
`ui.perfetto.dev/#!/?url=` requires **HTTPS**, so it can't fetch `http://127.0.0.1`. E4 switched to
the **postMessage shim** (or serves the `.gz` with `Origin`-restricted CORS). Because Perfetto
fetches cross-origin it **cannot present the bearer token** → `GET /profile/{id}/{name}` needs
token-exemption or a per-profile unguessable token (OD-B, co-owned E2↔E4).

---

## 4. Consolidated open-decision register (for the user)

These are genuine product/architecture decisions the reviews **did not** resolve. Recommendations
included; none is blocking until the noted epic.

| ID | Decision | Recommendation | Blocks |
|---|---|---|---|
| **OD-A** | Kill signal + fake-bazel exit-code convention (C6) | fake-bazel traps **both** SIGTERM **and** SIGINT → exit 8; E3 sends SIGINT first (correct for real bazel) but the *test* asserts "exited within grace" and accepts code 8 **or** 137; document that graceful-cancel fidelity is only provable against real bazel, not the backgrounded stub | E3 |
| **D-stack-1** | Process discovery: **cgo+libproc** vs shell-out (`lsof`/`ps`) | cgo+libproc (behind the `Scanner` interface seam E3 already built; non-darwin stub keeps CI green) | E3 |
| **D-stack-2** | Transport: **loopback TCP+token** vs Unix socket | TCP+token (browser + SwiftUI speak HTTP/WS trivially; `net.Listener` seam localizes a later flip). Note: token-in-0600-config is the *entire* security boundary — acceptable only at single-user-Mac scope | E7, E8 |
| **D3** | Admission: block-before-build vs kill-based vs hybrid | Block-before-build via the wrapper (E5), **fail-open** when the broker is down | E5 |
| **D4** | Kill: owned-PID **SIGINT** vs command-server `Cancel` | SIGINT→SIGKILL as default; keep `Cancel` flag-gated (it needs an in-flight `command_id` only the E5 wrapper can supply) | E3/E5 |
| **OD-B** | Browser auth to the token API (E7) | **Option A** — same-origin `HttpOnly; SameSite=Strict` session cookie that E2's middleware also accepts, + CSRF on POST mutations. A modifies E2 auth → **E2+E7 joint sign-off before E7 T5** | E7 |
| **OD-C** | `GET /profile/{id}/{name}` token exemption (E2↔E4) | Token-exempt + `Origin`-restricted CORS for that one read-only, id-addressed route | E4 |
| **OD-D** | proc cwd `EPERM` under hardened runtime (E3 risk) | Validate early on real macOS 26; if same-user cwd reads are denied, the "no-wrapper discovery" value prop weakens and admission/wrapper (E5) becomes the primary path. **Spike this in E3 T1** | E3 |
| **OD-E** | SQLite single-writer vs dedicated writer goroutine | Ship single-writer (`SetMaxOpenConns(1)`+WAL); revisit only if E4's hot-path metric writes contend | E4 |

---

## 5. Sequencing & buildability check

The dependency graph (E0 → E2 → {E3, E4, E6} → E5; E1 independent; E7/E8 over E2) is **acyclic and
correct**. Confirmations:

- **E0 first, non-negotiable.** Every epic imports its module/layout/`fake-bazel`. E0's SIGTERM fix
  and its alignment to E2 names must land before any consumer.
- **E1 fully parallel.** Config-only, touches the user's iOS `.bazelrc` + a `setup.sh`; its sole
  coupling is the C7/C8 contract with E4.
- **E2 is the gate for 6 epics.** Its §4 must be frozen (it is) *and* the consumers patched to it
  (§6) before they start, or the drift re-accumulates.
- **E2 T1 ships golden fixtures** (`testdata/api/*.json`). Make these the **executable contract
  test** every consumer (E6 Go decode, E8 Swift Codable, E7 JS) runs — this is the cheapest guard
  against field-name regression and should be treated as a hard gate, not a nicety.
- **E5 last in the core chain** (needs E2 admission seams + E3 registry/PID liveness).
- **E7/E8 anytime after E2**, but each carries one blocking decision (OD-B for E7, D-stack-2 for E8).

Recommended order unchanged from `02-epics.md`: **E0 → E1∥E2 → E3 → E6 → E4 → E5 → E7∥E8**, with
the §6 patch pass inserted **immediately after E2 freezes and before E3 starts**.

---

## 6. Required pre-implementation patches (actionable)

Apply these to the epic files before writing code. They are mechanical conformance to the frozen
contract — not redesign.

- **P1 — Rename `internal/wire`→`internal/api`, `wire.`→`api.`** in **E6 (23), E8 (4), E0 (8)**.
  Skip E2's deliberate "retired" prose. (C1)
- **P2 — Kill/metrics/profile routes** → nested `/builds/{invocation_id}/…` in **E3, E6, E7**. (C2)
- **P3 — Field/state/event conformance** per E2 §4.6 table in **E3, E4, E6, E7, E8**:
  `id`→`invocation_id`, `started_at`→`start_time`, `cache_hit_*`→`cache_hit_ratio`,
  `building`→`running`, WS types → `snapshot`/`build` only, drop `heartbeat` JSON event. (C3, C4)
- **P4 — Lock the E1↔E4 BEP path string and the cache-hit definition** (runner_count) in **E1 + E4**
  so they're byte/semantics-identical. (C7, C8)
- **P5 — Resolve OD-A** and update **E0 fake-bazel** + **E3 kill test** to one signal/exit-code
  convention. (C6)
- **P6 — Add the golden-fixture contract test** as a gate consumed by E6/E7/E8 (E2 deliverable). (§5)

Decisions OD-B/OD-C/D-stack-2/D3/D4/OD-D/OD-E (§4) are **product calls for the user** and can be
recorded in a project decision log; only OD-A and the P1–P4/P6 conformance patches block the start
of implementation.

---

## 7. Bottom line

The plan set is **strong and ready to execute** after the §6 conformance pass. The genuine
intellectual work — Bazel's per-output-base concurrency model, relocatable-cache strategy, the
control-plane/observability split, the wrapper interception, the verify harness — is correct and
well-detailed. The fragility is entirely at the API seams, and E2 §4 + this document make that seam
explicit and testable (the golden fixtures). Land E0, freeze E2, run §6, then build outward.
