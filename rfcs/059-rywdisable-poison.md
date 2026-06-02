# RFC-059: Poison the transaction when READ_YOUR_WRITES_DISABLE is set after an operation

**Status:** Draft
**Item:** RFC-010 C3 (fresh differential axes — error-code/option semantics). A divergence
surfaced while building the RFC-058 conflict differential.

## Problem

libfdb_c forbids setting `READ_YOUR_WRITES_DISABLE` once any read or write has happened on the
transaction. The Go client's `SetReadYourWritesDisable()` is an unguarded setter
(`transaction.go:1524`) — it always succeeds and the transaction proceeds normally — so a Go
app can disable RYW mid-transaction where libfdb_c errors. C++ is the spec for the FDB client
(CLAUDE.md principle 2), so this is a Go bug: go is silently too permissive.

## Investigation (empirical — the OBSERVABLE libfdb_c behavior is the spec)

The C++ source `ReadYourWritesTransaction::setOptionImpl` (`ReadYourWrites.actor.cpp:2535`)
throws `client_invalid_operation` if `reading.getFutureCount() > 0 || !cache.empty() ||
!writes.empty()`. But the throw is **deferred** through the binding: the option call itself
returns success; the error surfaces at the NEXT operation. A differential characterization
against libfdb_c (cgo) pins the exact observable behavior:

| sequence (per client) | libfdb_c | Go (before fix) |
|---|---|---|
| `disable` (before any op) → `Get` | 0 (storage read) | 0 |
| `Set` → `disable` → `Get` | **2000** | 0 |
| `Set` → `disable` → `GetKey` | **2000** | 0 |
| `Set` → `disable` → `GetRange` | **2000** | 0 |
| `Get` → `disable` → `Get` | **2000** | 0 |
| `Set` → `disable` → `Commit` | **2000** | 0 |

| `Set` → `disable` → `GetReadVersion` | **2000** | 0 |
| `Set` → `disable` → `GetEstimatedRangeSizeBytes` | **2000** | 0 |
| `Set` → `disable` → snapshot `Get` | **2000** | 0 |

So: a **clean** (pre-op) disable works normally; a disable AFTER any read or write **poisons
the WHOLE transaction** — EVERY subsequent operation that touches the server returns
`client_invalid_operation` (2000): regular + snapshot reads, GetKey, GetRange, **GetReadVersion,
GetEstimatedRangeSizeBytes (metrics), and Commit**. The deferred `deferredError` gates them all
(`RYWImpl::checkValid`). The option call itself returns success in both clients (so an
option-set-time guard would *create* a divergence, not fix one). Writes (Set/Clear/atomic) are
void in both clients and do not surface the error. (Note: this empirically poisons GRV + metrics
too — the structural assumption that they are exempt is wrong; the differential is the spec.)

## Fix

Faithfully model the poisoned transaction:
- Add `Transaction.rywPoisonErr error` and `Transaction.hadRead bool`.
- `SetReadYourWritesDisable()` sets `rywDisabled = true` and, if a prior op exists, sets
  `rywPoisonErr = &wire.FDBError{Code: 2000}`. "Prior op" = `tx.hadRead || !tx.ryw.isEmpty()`:
  - writes → `rywCache.isEmpty()` is false (`len(writes)>0 || len(cleared)>0`; serverCache too).
  - reads → `hadRead`, set when a read is ISSUED at the three chokes every read funnels through:
    `getValue`, `getRange`, and `GetPipelined`. A `serverCache`-only check is INSUFFICIENT — the
    facade's `Get` uses `GetPipelined`, which does not populate `serverCache`, so a
    `Get → disable → Get` would not poison (the C++ `reading.getFutureCount()>0` disjunct).
  The option call still returns success (matching libfdb_c).
- **Uniform gate at `ensureReadVersion`**: it returns `rywPoisonErr` first if set. Every read
  (regular + snapshot), `Commit`, and `GetReadVersion` fetch a read version through here — and
  libfdb_c poisons ALL of them, so the single gate is correct (NOT a divergence-creating
  over-gate, because GRV does poison empirically). `GetEstimatedRangeSizeBytes` bypasses
  `ensureReadVersion`, so it gets an explicit check.
- `reset()`/`resetForRetry()` clear `rywPoisonErr` + `hadRead` (a clean retry re-applies the
  option over an empty layer with no poison — matching C++ persistentOptions reapplication; 2000
  is non-retryable, so a poisoned commit kills the txn).

## Performance

A nil-pointer check at `ensureReadVersion` / metrics; an O(1) check on the rarely-set option; a
bool store on each read. No wire impact.

## Test plan

`TestDifferential_RYWDisableAfterOp` (`pkg/fdbgo/bench/`): nine sequences, on EACH client,
asserting go error code == cgo error code. The clean-disable case proves the fix doesn't
over-poison (both 0); the eight post-op cases prove the poison (both 2000) across regular Get,
GetKey, GetRange, snapshot Get, GetReadVersion, GetEstimatedRangeSizeBytes, Commit, and
read-(via GetPipelined)-then-disable. Verified red→green: all eight post-op cases fail without
the poison (Go returns 0 where libfdb_c returns 2000) and pass after.
