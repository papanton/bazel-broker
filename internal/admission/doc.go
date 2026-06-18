// Package admission is the Bazel Broker's block-before-build admission engine
// (E5). It gates `bazel build/test/...` invocations at the front door so the
// machine stays inside its CPU/RAM budget and queues the overflow instead of
// thrashing.
//
// # Shape
//
//   - Engine: a FIFO queue of waiters behind an ordered gate chain
//     (Semaphore -> TokenBucket -> Stagger). Admit() long-polls a waiter up to
//     PollSeconds, then returns Queue so the bash wrapper re-polls. The verdict
//     channel is cap-1 BUFFERED + single-shot so schedule() can deliver a
//     verdict without ever blocking while holding e.mu (the unbuffered-verdict
//     deadlock the consolidated review flagged).
//
//   - Admitter: the httpapi.Admitter implementation. POST /admission returns a
//     STATUS CODE + ONE-WORD TEXT BODY (200 ALLOW / 202 QUEUE / 403 DENY), never
//     JSON, because the wrapper is bash 3.2 with no guaranteed jq (conflict C5).
//
// # Slot lifecycle (why exec-skips-trap is solved server-side)
//
// A slot is acquired exactly once, inside schedule() under e.mu, when all gates
// admit a waiter. It is released by ANY of:
//
//   - POST /admission/release {invocation_id}  (the wrapper's explicit release),
//   - a registry terminal event (DELETE /builds/{id} deregister, BEP
//     BuildFinished, or E3 process-gone reconcile) observed via the Hub, or
//   - the PID-liveness reaper (backstop for a wrapper that died before release).
//
// Because the wrapper exec's $BAZEL_REAL (replacing its own shell image), a
// trap-EXIT release would never fire. The engine therefore does NOT depend on
// the wrapper for slot release: the registry-event path and the PID reaper free
// the slot when the broker observes the build finish, independent of the
// wrapper. The connection closing NEVER frees a slot.
package admission
