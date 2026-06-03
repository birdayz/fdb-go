# RFC-068: operations on a cancelled transaction must return transaction_cancelled (1025)

**Status:** Implemented
**Item:** RFC-010 C3 (client differential hunt). Found by a fresh error-CODE differential.

## Problem (a real error-surface divergence, found differentially)

After `tx.Cancel()`, every operation on the pure-Go client returns a bare string error
(`fmt.Errorf("transaction cancelled")`) carrying **no FDB error code**, while libfdb_c returns
`transaction_cancelled` (**1025**) as a proper coded error. An app that branches on the FDB
error code to detect cancellation — `errors.As(err, &fdb.Error)` / `err.Code == 1025`, the Go
analogue of `catch (transaction_cancelled)` — matches against a C/Java client but **not** against
the Go client. This is a divergence on the read/commit error surface (RFC-010's "compare the
error CODE" principle).

Found by a new cancel-lifecycle probe driving each op through both clients on a cancelled tx and
comparing the returned **code**:

| op on a cancelled txn | go (before) | cgo (libfdb_c) |
|---|---|---|
| `Get` | **none (string)** ✗ | 1025 |
| `GetReadVersion` | **none** ✗ | 1025 |
| `GetKey` | **none** ✗ | 1025 |
| `GetRange` | **none** ✗ | 1025 |
| `Commit` | **none** ✗ | 1025 |
| `AddReadConflictRange` | 0 (no-op) | 0 (no-op) ✓ |

(`fdbErrorCode` maps a non-FDB error to `-1`; the probe showed go=-1, cgo=1025 for all five.)

## Root cause

The Go client routes **all** reads through one gate, `ensureReadVersion` (transaction.go),
and `Commit` has its own state gate. Both returned `fmt.Errorf("transaction cancelled")` /
`fmt.Errorf("transaction not active")` — message-only errors with no code. The state machine
was correct (`Cancel()` sets `txStateCancelled`, irreversibly); only the returned error type was
wrong.

## C++ architecture

`ReadYourWritesTransaction::cancel()` (`ReadYourWrites.actor.cpp:2730`) does
`resetPromise.sendError(transaction_cancelled())`. Every RYW operation races `resetPromise`, so a
cancelled promise makes **every** pending and future op resolve with `transaction_cancelled`
(1025). The Go single-gate (`ensureReadVersion` + the `Commit` gate) is the faithful analogue of
that promise check — it just must surface the **coded** 1025, not a string.

## Fix

A `checkCancelled()` helper returns `&wire.FDBError{Code: 1025}` when the state is
`txStateCancelled` — the Go analogue of C++'s per-op `resetPromise` check. Every op entry calls
it:

- **Read/watch/commit surface** (`ensureReadVersion` + the `Commit` gate). All reads route through
  `ensureReadVersion`, including `WatchSetup` (`readpath.go:803`, the synchronous half of `Watch`)
  and `GetReadVersion`; the async `WatchPoll` is independently cancelled via the watch context
  (`getWatchCtx`, already tied to `Cancel()`).
- **Ops that BYPASS `ensureReadVersion`** — the FDB-C++ reviewer caught these still diverging, and
  a follow-up differential sweep confirmed each (go vs cgo before the fix): `GetEstimatedRangeSizeBytes`
  (go=0), `GetRangeSplitPoints` (go=0) — the metrics path, gated only on `rywPoisonErr`;
  `OnError` (go=0 → it would *reset-and-retry* a retryable input error, reusing a cancelled
  handle); `GetVersionstamp` (go=2015, the not-yet-committed code); and `GetAddressesForKey`
  (go=0). Each now calls `checkCancelled()` at entry (before its own checks, mirroring
  resetPromise-first), returning 1025.

The sweep also confirmed two ops that correctly do **not** gate: `GetApproximateSize` (a pure size
getter — C++ returns the raw size with no resetPromise check, code 0; both clients return 0, so it
is a negative control in the test) and `Watch` (already gated via `WatchSetup` → `ensureReadVersion`).

**Separate finding (out of scope, noted for a follow-up hunt):** `GetCommittedVersion` on a
*never-committed* (here, cancelled) txn returns `no_commit_version` (2015) in Go but **0** (no
error) in libfdb_c — a divergence on the committed-version-before-commit axis, **not** the
cancellation axis (cgo does not return 1025 here), so it is not part of this RFC's cancel gate. The non-cancelled "not active" states (committed/errored)
keep their existing message-only error — that is a separate axis (deferred-error latching) the
probe did not flag, and is out of scope here. `AddReadConflictRange` already matches (it buffers
without going through the gate; a cancelled tx never commits, so the range is harmless).

```go
if txState(tx.state.Load()) == txStateCancelled {
    return &wire.FDBError{Code: 1025} // transaction_cancelled — matches libfdb_c resetPromise
}
```

### Out of scope (separate, entangled divergence)

`commit → cancel → get` returns `used_during_commit` (2017) in libfdb_c but a cancelled error in
Go, because Go auto-resets a transaction to active after a successful commit (a deliberate
documented extension to match the binding tester's handle-reuse, audit #6) whereas C++ leaves it
in the committed state where `checkUsedDuringCommit()` fires. That corner is tangled with the
auto-reset extension, not the clean cancelled-error-code surface this RFC fixes; not changed here.

## Performance / compatibility

No behavioral change other than the error *type*: an op on a cancelled txn already failed; it now
fails with the same coded error libfdb_c returns. Code that matched the old string (there is no
exported sentinel — the message was never part of the API) is unaffected; code that inspects the
FDB code now works cross-engine.

## Test plan

- `TestDifferential_CancelLifecycle` (`pkg/fdbgo/bench/`) — 13 cases on a cancelled txn vs libfdb_c:
  the read/commit ops (Get / GetReadVersion / GetKey / GetRange / Commit), the five bypass ops
  (GetEstimatedRangeSizeBytes, GetRangeSplitPoints, OnError, GetVersionstamp, LocalityGetAddressesForKey),
  Watch — all asserted `goCode == cgoCode == 1025`; plus two negative controls
  (AddReadConflictRange and GetApproximateSize, both `== 0`). Every gated op was red before the fix
  (go=-1/0/2015, cgo=1025) and green (both 1025) after — the red→green proof.
- `just test` green.
