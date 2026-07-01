# RFC-168: Watch-setup per-watch-context restructure

**Status:** IMPLEMENTED (round 18 of the fdbgo client bug-hunt) — the per-watch-context restructure
landed after all. `watchCtx`/`watchCancel` → `watchCancels map[uint64]context.CancelFunc`;
`getWatchCtx` → `newWatchCtx` (returns the context + a scoped cancel); WatchSetup returns the scoped
cancel (6th value), threaded to WatchPoll (deferred for self-cleaning deregister) and the fdb facade
(the future's Cancel scopes to one watch). Verified: per-watch unit tests + `TestNewWatchCtx_
PerWatchScoped` (cancel one, sibling survives) + `TestWatch_NewWatchCtxCancelRaceFree` under `-race`,
the watch integration suite, and both concurrency fault tests. Closes the round-13 poisoning, round-17
future-Cancel, and round-18 over-cancel edges at once. Still owed the codex/persona review gauntlet on
this HEAD.
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
- `cancelWatches()`: snapshot + clear the map under `watchMu`, then cancel every entry — Cancel()/
  reset() still tear down all watches (C++ `resetRyow`).
- **Threading:** WatchSetup returns the scoped cancel (a 6th return value); its setup-error paths call
  it (replacing the round-13 `created`-clear); WatchPoll takes it and `defer`s it (deregister on
  completion → the map self-cleans, no growth across a reused post-commit handle); the fdb facade
  `Watch` passes it to `newFutureNilCancel` (replacing `inner.CancelWatches`).
- Remove the exported `CancelWatches` (round 17) — the facade uses the scoped cancel.

This closes: round-13 poisoning (each watch owns its context), round-17 future-Cancel (scoped), and
round-18 over-cancel (scoped) — the whole class, at once.

## Semantics to preserve (why it's subtle)

- **Watches survive commit:** `postCommitReset` must NOT cancel/clear the map (a committed watch keeps
  polling); `reset()` (OnError retry / user Reset) and `Cancel()` DO (`cancelWatches`). The round-16
  `OnError` abort-cancel defer stays.
- **Race-free bind (round 4/7):** `newWatchCtx` appends under `watchMu`; `cancelWatches` snapshots
  under `watchMu`; the async WatchPoll only USES the captured ctx. Verify with
  `TestWatch_GetWatchCtxCancelRaceFree` (adapted) under `-race`.
- **Signature ripple:** WatchSetup's 6th return value touches the combined client `Watch`, the fdb
  facade `Watch`, and ~8 test callers in `watch_validation_test.go` — mechanical but wide; do it as a
  focused change, not a session-end rush.

## Test plan (all must pass, plus the existing rounds-11–17 watch regressions)
- **Per-watch scoping (the round-18 fix):** register two watches on one txn; Cancel the FIRST future;
  assert the SECOND's context is still live and only the first's slot is released. Revert-proof: with
  the txn-wide cancel, the second is cancelled too.
- **Survives commit:** a watch registered + committed keeps polling (not cancelled by postCommitReset).
- **Cancel/reset cancels all;** **abort (OnError) cancels all** (round-16 pin still green).
- **-race:** the adapted race-free test + the full watch suite.

## Why deferred (not landed in the bug-hunt PR)
Landing an 8-return signature restructure of the most-fragile area at the end of a very long review
session, without a fresh review cycle, risks a subtle watch-lifecycle regression worse than round-18's
niche multi-watch over-cancel (documented as a known limitation on the round-17 fix). This is the
correct, reviewed follow-up.
