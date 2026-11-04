# RFC-168: Watch-setup per-watch-context restructure

**Status:** IMPLEMENTED (round 18 of the fdbgo client bug-hunt) — the per-watch-context restructure
landed after all. `watchCtx`/`watchCancel` → `watchCancels map[uint64]context.CancelFunc`;
`getWatchCtx` → `newWatchCtx` (returns the context + a scoped cancel); WatchSetup returns the scoped
cancel, threaded to WatchPoll (deferred for self-cleaning deregister) and the fdb facade
(the future's Cancel scopes to one watch). Verified: per-watch unit tests + `TestNewWatchCtx_
PerWatchScoped` (cancel one, sibling survives) + `TestWatch_NewWatchCtxCancelRaceFree` under `-race`,
the watch integration suite, and both concurrency fault tests. Closes the round-13 poisoning, round-17
future-Cancel, and round-18 over-cancel edges at once. Still owed the codex/persona review gauntlet on
this HEAD. The ownership rules below include commit handoff: committed watches leave the transaction's
cancellation registry but keep their scoped and parent cancellation.
**Item:** FDB client bug-hunt (2026-06-30). See `shifts/2026-06-30-fdbgo-client-bughunt.md`.
**Spec:** libfdb_c 7.3.77 (`/tmp/fdbsrc`).

## Problem — a 7-round whack-a-mole

The `WatchSetup`/`WatchPoll` path (`pkg/fdbgo/client/readpath.go`) + the per-txn watch context
(`pkg/fdbgo/client/transaction.go`) produced a codex finding in SEVEN of the bug-hunt's review rounds
(11, 12, 13, 14, 16, 17, 18). Each was a real correctness edge; each incremental patch exposed the
next. The two structural roots:

1. **One shared `watchCtx` per transaction** (`getWatchCtx` lazily mints one, `cancelWatches` cancels
   it). This caused: a failed setup poisoning the next watch with a cancelled child (round 13, patched
   with a `created`-clear that only covers the sequential case), and — the trigger for this RFC —
   **cancelling one watch future cancels ALL of the txn's watches** (round 17 wired the future's
   `Cancel()` to the txn-wide `CancelWatches`; round 18 flagged that it cancels unrelated watches in
   supported multi-watch transactions). There is no minimal patch for round 18: scoping a single
   watch's cancellation *requires* a per-watch context.
2. **Acquire/check ordering** (rounds 11, 13, 14, 16) — largely resolved: the slot acquire is
   synchronous at registration order, and terminal checks (cancelled/ctx/timeout) now run first.

## The fix — per-watch cancellable context

Replace the single `watchCtx`/`watchCancel` with a map of per-watch cancels:

```go
watchMu      sync.Mutex
watchCancels map[uint64]context.CancelFunc
nextWatchID  uint64
```

- `newWatchCtx(parent) (context.Context, context.CancelFunc)`: `ctx, cancel := WithCancel(parent)`
  (a CHILD of the caller's parent, so the caller's own cancellation still propagates); register
  `cancel` under a fresh id; return `ctx` + a **scoped** cancel that cancels+deregisters ONLY this
  watch (idempotent). Captured synchronously in WatchSetup (preserves the round-4/7 race-free bind).
- `cancelWatches()`: snapshot + clear the map and abandon the current activation under `watchMu`,
  then cancel every entry. Cancel/reset/OnError tear down only uncommitted watches.
- **Commit handoff:** clear the cancellation map without cancelling its entries before publishing
  successful activation, under the same `watchMu` as registration (C++ `setupWatches`). Each future's
  scoped cancel still owns its captured cancel function after deregistration.
- **Threading:** WatchSetup returns the scoped cancel; its setup-error paths call
  it (replacing the round-13 `created`-clear); WatchPoll takes it and `defer`s it (deregister on
  completion → the map self-cleans, no growth across a reused post-commit handle); the fdb facade
  `Watch` passes it to `newFutureNilCancel` (replacing `inner.CancelWatches`).
- Remove the exported `CancelWatches` (round 17) — the facade uses the scoped cancel.

This closes: round-13 poisoning (each watch owns its context), round-17 future-Cancel (scoped), and
round-18 over-cancel (scoped) — the whole class, at once.

## Semantics to preserve (why it's subtle)

- **Watches survive commit and later teardown:** successful activation clears the cancellation map
  without cancelling the contexts before `postCommitResetFields`. Reset/Cancel/OnError can then reach
  only uncommitted watches. A committed watch remains owned by its future and caller context.
- **Race-free bind (round 4/7):** registration captures activation under `watchMu`; commit handoff and
  teardown rotate activation under the same lock. The async WatchPoll only uses the captured context.
  `TestWatchCommitHandoffCancellationRaces` covers scoped cancellation and teardown racing handoff.
- **Signature ripple:** WatchSetup's scoped cancel touches the combined client `Watch`, the fdb
  facade `Watch`, and ~8 test callers in `watch_validation_test.go` — mechanical but wide; do it as a
  focused change, not a session-end rush.

## Test plan (all must pass, plus the existing rounds-11–17 watch regressions)
- **Per-watch scoping (the round-18 fix):** register two watches on one txn; Cancel the FIRST future;
  assert the SECOND's context is still live and only the first's slot is released. Revert-proof: with
  the txn-wide cancel, the second is cancelled too.
- **Survives commit:** both read-only and write commits hand watches off; later Reset, Cancel and
  retryable/terminal OnError leave them live. Scoped future cancellation and parent cancellation still
  stop their polls and release slots (`watch_lifecycle_fdb_test.go` in the client and facade).
- **Uncommitted teardown:** Cancel/reset/OnError cancel pending watches, including successor watches
  registered on a handle that has already handed committed watches off.
- **-race:** the adapted race-free test + the full watch suite.

## Why deferred (not landed in the bug-hunt PR)
Landing an 8-return signature restructure of the most-fragile area at the end of a very long review
session, without a fresh review cycle, risks a subtle watch-lifecycle regression worse than round-18's
niche multi-watch over-cancel (documented as a known limitation on the round-17 fix). This is the
correct, reviewed follow-up.
