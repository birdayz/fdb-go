# RFC-072: Reply-channel pool discipline (RFC-010 #13)

**Status:** Implemented
**Area:** Native fdbgo client transport (`pkg/fdbgo/transport/conn.go`)
**Reviewers:** FDB C++ developer (transport correctness — C++ is the spec), Torvalds (code quality), codex, @claude

## Problem

Each RPC gets a pooled `chan Response` (`replyChanPool`) via `PrepareReply` or
`Send`. The pool comment claimed "readLoop returns it after dispatch" — **false**:
`readLoop` only *delivers* (`ch <- Response{...}`); it never returns the channel
to the pool. So on the **success path** the channel is never pooled — a hot-path
allocation on every RPC (the pool is defeated for the common case).

`Release()` (called after a successful receive) just nilled `h.ch` and pooled the
*handle* — never the channel. `SendAndWait`'s success path returned without
pooling, and its timeout path left the pending token + channel to linger.

The fix can't be "always pool in Release", because of the **timeout/delivery
race**: if a caller times out exactly as `readLoop` delivers, the channel holds a
stale buffered `Response`. Pooling it would hand that stale reply to a future
request (silent wrong-answer). So the channel must be pooled **only when no send
can still race it.**

## Investigation

`readLoop` (conn.go) takes `pendingMu`, deletes the token from `pending`, releases
the lock, then sends to the channel. Key invariants this gives:
- **Success**: once a caller has received its reply, `readLoop` already deleted
  the token *before* sending, and only `readLoop` ever sends per token
  (`failAllPending` skips deleted tokens). So no further send can race → safe to pool.
- **Cancel/timeout that wins the delete race** (token still in `pending`): the
  delete (under `pendingMu`) prevents `readLoop` from ever finding/sending to it →
  channel is clean → safe to pool.
- **Cancel/timeout that loses the race** (token already gone): `readLoop` delivered
  (or is mid-deliver); the channel may hold a buffered value → must NOT pool (leak it).

Caller convention (hedge.go, `sendPingWithReply`): **success → `Release()` only;
cancel/loser → `Cancel()` then `Release()`.** `SendAndWait` has no production
callers (test-only) but is in scope per the TODO.

## Fix (`conn.go`)

- **`Cancel()`**: unchanged win/lose pooling (pool iff it won the delete), plus set
  `h.ch = nil` to mark the channel handled.
- **`Release()`**: pool `h.ch` iff non-nil — the success path (no `Cancel`, so
  `h.ch` is still set). `Cancel` nils `h.ch`, so the cancel path never double-pools.
- **`SendAndWait`**: success → `putReplyChannel`; timeout (`ctx.Done`) →
  `cancelPending` (delete + pool iff it won the race); conn-close (`c.ctx.Done`) →
  leave to GC (`failAllPending` may be delivering concurrently). Extracted
  `sendInternal` so `SendAndWait` holds the bidirectional channel; added
  `cancelPending` (the raw-token analog of `ReplyHandle.Cancel`).
- Corrected the false pool comment.

The race-loser leak is rare and deliberate (the price of never serving a stale reply).

## Performance

Improves the steady state: the success path now actually reuses pooled channels
(removing the per-RPC `make(chan Response, 1)` the false comment masked). No new
locking — `cancelPending`/`Cancel` use the existing `pendingMu`.

## Test plan

`reply_pool_test.go` — deterministic, race-clean (`-race`), via a `putReplyChannel`
seam that records exactly which channels are pooled (so the assertions don't depend
on `sync.Pool`'s non-deterministic reuse). These tests mutate package state, so
they run serially (documented exception to the t.Parallel() rule):
- Cancel won-race → pools exactly once; Release after Cancel does NOT re-pool.
- Cancel lost-race (token gone, value buffered) → does NOT pool (leak the stale channel).
- Release success path → pools exactly once; handle reset.
- `cancelPending` won/lost discipline (SendAndWait's timeout path).

Full multi-goroutine race coverage (a real timeout landing exactly on a delivery)
awaits the `SimTransport` deterministic harness (TODO C4); the unit-level seam pins
the who-pools-when discipline that is the substance of the fix.
