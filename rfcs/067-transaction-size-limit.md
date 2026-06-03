# RFC-067: enforce TRANSACTION_SIZE_LIMIT (Go client committed oversized txns)

**Status:** Implemented
**Item:** RFC-010 C3 (client divergence hunt). Found by a fresh error-CODE differential.

## Problem (a real wire divergence, found differentially)

The pure-Go FDB client did **not** enforce `TRANSACTION_SIZE_LIMIT`. A transaction
accumulating more than 10 MB of mutations **committed successfully** in Go, while
libfdb_c rejects it **client-side** with `transaction_too_large` (2101). A Go app could
therefore commit an oversized transaction that a C/Java app on the same cluster never
could — a behavioral divergence on the write path.

Found by `TestDifferential_ErrorCodes` (new), which drives the same size/legal-range
triggers through both clients and compares the returned error **code**:

| trigger | go | cgo (libfdb_c) |
|---|---|---|
| value > 100 000 (`value_too_large`) | 2103 | 2103 ✓ |
| key > 10 000 (`key_too_large`) | 2102 | 2102 ✓ |
| read `\xff…` w/o access (`key_outside_legal_range`) | 2004 | 2004 ✓ |
| **txn > 10 MB (`transaction_too_large`)** | **0 (committed)** ✗ | **2101** |

## Root cause

The Go client already had the enforcement machinery — `transaction.go:938` rejects with
2101 when `tx.sizeLimit > 0 && GetApproximateSize() > tx.sizeLimit` — but the **default**
was wrong. `database.go` applied the size limit to a new transaction only when the DB
option was explicitly set:

```go
if td.SizeLimit > 0 {            // td.SizeLimit defaults to 0 ("disabled")
    tx.SetSizeLimit(td.SizeLimit)
}
```

C++ defaults **every** transaction's `sizeLimit` to `CLIENT_KNOBS->TRANSACTION_SIZE_LIMIT`
(`NativeAPI.actor.cpp:6133`; `ClientKnobs.cpp:75` → `1e7`). There is no "disabled" state:
the `SIZE_LIMIT` option floor is 32 and its default/ceiling is 10 MB
(`extractIntOption(value, 32, TRANSACTION_SIZE_LIMIT)`, `NativeAPI:7069`). Go's
`0 = disabled` default left a default transaction with no size enforcement.

## Fix

`database.go`: default a new transaction's `sizeLimit` to `transactionSizeLimit`
(10 000 000, added to `sizelimits.go` alongside the key/value knobs) when the DB option
is unset; the DB option, when set (>0), lowers it. One-line behavioral change; the
existing commit-time check (`transaction.go:938`) then fires.

```go
if td.SizeLimit > 0 {
    tx.SetSizeLimit(td.SizeLimit)
} else {
    tx.SetSizeLimit(transactionSizeLimit) // C++ default (NativeAPI:6133)
}
```

## Companion fix: online-indexer lessen-work codes (Torvalds review)

Enforcing the limit made a **latent** bug live. The online indexer batches records
per transaction and **halves the batch** on transient "do less work" errors
(`OnlineIndexer.shouldLessenWork`). Its code whitelist had the right error *names* in
the comment but the wrong *numbers* — `{1007, 1020, 1028, 1031, 1039, 2501}` — where
`1028` is `new_coordinators_timed_out`, `1039` is `cluster_version_changed`, and `2501`
is not `transaction_timed_out`; it was **missing** `1004`/`2002`/**`2101`**. Before this
RFC, `transaction_too_large` (2101) was unreachable on the default path, so the gap
stayed latent. With the limit now enforced, a >10 MB index batch → 2101 → *not* in the
whitelist → not retried → the index build fails hard instead of halving. Fixed to match
Java `IndexingThrottle.lessenWorkCodes` 1:1 (`{1004, 1007, 1020, 1031, 2002, 2101}`) and
pinned by `TestMayRetryAfterHandlingException` (now asserts the correct set retries AND
the formerly-bogus `1028/1039/2501` do NOT — revert-proof).

## Follow-up (not blocking)

`GetApproximateSize`'s per-mutation overhead constant `48` approximates
`sizeof(MutationRef)` (pre-existing; not changed here). A transaction within tens of
bytes of 1e7 built from *many tiny* mutations could expose a boundary divergence if
`48 ≠ sizeof(MutationRef)`; the window is negligible and the gross case (≫1e7) is pinned.
A near-1e7 boundary differential is a clean separate hunt.

## Performance / compatibility

No hot-path cost — the size check already ran (gated on `sizeLimit > 0`); only the
default value changed. A transaction that previously committed >10 MB now fails with
2101, which is exactly what libfdb_c does, so any code relying on the old behavior was
relying on the divergence (and would already fail against a C/Java client). The
record-layer splits values into 100 KB chunks and does not commit >10 MB single
transactions in normal operation.

## Test plan

- `TestDifferential_ErrorCodes` (`pkg/fdbgo/bench/`) — error-CODE differential vs
  libfdb_c for value/key/txn-too-large + key-outside-legal-range. The
  `transaction_too_large` case was **red** (go=0, cgo=2101) before the fix and **green**
  (both 2101) after — the red→green proof. It also pins value/key/legal-range codes,
  which already matched.
- `TestTransactionSizeLimit_DefaultsToKnob` (`pkg/fdbgo/client/`, no FDB) — a default
  transaction's `sizeLimit` is `TRANSACTION_SIZE_LIMIT`; an explicit DB option lowers it.
  Revert-proof: with the default left at 0, the first assertion fails.
- `just test` green — the default change does not break any large-commit path.
