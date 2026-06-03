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

## Commit validation order (codex review)

Making the size check run by default exposed two order divergences (codex P2), now
matched to C++ `commitMutations` (`NativeAPI.actor.cpp:6797-6836`):

1. **Read-only fast path** (`:6800`): a transaction with no mutations AND no
   write-conflict ranges returns success *before* the size check — even when it carries
   >10 MB of READ-conflict ranges (which `getSize` would otherwise count). The Go size
   check is now gated on `len(mutations) > 0 || writeConflicts > 0`.
2. **Per-mutation validation precedes size** (`:6818-6836` runs after `set()`'s
   key/value checks): an oversized key/value that also crosses 10 MB now reports
   `key_too_large`(2102)/`value_too_large`(2103), not `transaction_too_large`(2101). The
   Go size check moved to *after* the per-mutation validation loop.

Pinned by two new differential cases: `oversized_key_precedes_size` (go==cgo==2102) and
`readonly_large_read_conflicts` (go==cgo==0, ~12.8 MB of read-conflict ranges).

### Versionstamp validation order (codex follow-up review)

A second codex pass flagged that the now-default size check could also pre-empt
**versionstamp-offset** validation (`client_invalid_operation`, 2000). A fresh probe
(`TestProbe`-style, then promoted to `TestDifferential_VersionstampValidationOrder`)
established the full ordering ground truth by differential — **this is the spec, not the
C++ reading**:

| trigger (call order) | libfdb_c | go (before) | go (after) |
|---|---|---|---|
| bad versionstamp + >10 MB valid sets | 2000 | **2101** ✗ | 2000 ✓ |
| >10 MB valid sets + bad versionstamp | 2000 | **2101** ✗ | 2000 ✓ |
| bad versionstamp, then oversized value | 2000 | **2103** ✗ | 2000 ✓ |
| oversized value, then bad versionstamp | 2103 | 2103 ✓ | 2103 ✓ |
| bad versionstamp, then oversized key | 2000 | **2102** ✗ | 2000 ✓ |
| oversized key, then bad versionstamp | 2102 | 2102 ✓ | 2102 ✓ |
| oversized versionstamp key + bad offset (one op) | 2102 | 2102 ✓ | 2102 ✓ |
| oversized versionstamp value + bad offset (one op) | 2103 | 2103 ✓ | 2103 ✓ |

The model libfdb_c implements: key-size (2102), value-size (2103), and versionstamp-offset
(2000) are all **eager** (validated at the `Set()`/`atomicOp()` call, in **call order**) —
the **first eagerly-invalid op wins**; **within** one op the size check precedes the offset
check; `transaction_too_large` (2101) is **deferred** to commit and never pre-empts an eager
error. The Go client deferred *all* validation to commit and ran the versionstamp check in a
**separate loop after** the size check, so a bad versionstamp combined with an oversized txn/
key/value (versionstamp op first) reported 2101/2102/2103 instead of 2000.

**Fix:** move the versionstamp-offset validation **into the per-mutation validation loop**, in
mutation order (= call order), **after** the key/value-size checks and **before** the deferred
transaction-size check. This reproduces "first eagerly-invalid op wins" with a fixed loop
order and matches all eight differential cases above. The separate post-size loop is deleted.

### metadataVersionKey write contract (codex + FDB-C++ + Torvalds review)

Moving the versionstamp check into the per-mutation loop exposed a related divergence — and
the move itself regressed one case. The loop short-circuits `metadataVersionKey`
(`\xff/metadataVersion`) with an early `continue` (the key is exempt from system-key range
checks). The *old* separate versionstamp loop ran over **all** mutations (no metadataVersionKey
skip), so `SetVersionstampedValue("\xff/metadataVersion", badOffset)` *was* offset-validated;
inlining the check behind that `continue` made it unreachable. More broadly, the blanket
`continue` let the Go client **commit every illegal metadataVersionKey mutation silently**,
where libfdb_c rejects it. Ground truth (`TestDifferential_MetadataVersionKey` vs libfdb_c):

| op on metadataVersionKey | libfdb_c | go (before) |
|---|---|---|
| `SetVersionstampedValue`, operand == required (14 zero bytes) | 0 (legal) | 0 ✓ |
| `SetVersionstampedValue`, any other operand | 2000 | **0 (committed)** ✗ |
| `SetVersionstampedValue`, operand < 4 bytes | 2000 | **0** ✗ |
| plain `Set` | 2000 | **0** ✗ |
| `Add` (or any other atomic op) | 2000 | **0** ✗ |
| `SetVersionstampedKey` | 2000 | **0** ✗ |
| `Clear` / `ClearRange` beginning at metadataVersionKey | 2004 | **0** ✗ |

The C++ contract (`ReadYourWrites.actor.cpp`): `atomicOp` (:2226-2229) and `set` (:2300) accept
**only** `SetVersionstampedValue` with operand == `metadataVersionRequiredValue`
(`SystemData.cpp:1387` — 14 zero bytes); anything else → `client_invalid_operation` (2000),
checked *before* the size/offset checks. `clear`/`clear(range)` (:2357, :2406) have **no**
metadataVersionKey gate, so a clear hits the normal legal-range check → `key_outside_legal_range`
(2004) since metadataVersionKey ≥ maxWriteKey.

**Fix:** replace the blanket `continue` with the C++ gate in the per-mutation loop — reject any
non-`SetVersionstampedValue`, or any `SetVersionstampedValue` whose operand ≠ the required value,
with 2000; the legal write falls through to the (passing) size + offset checks. The gate is
scoped to non-`MutClearRange` types (Go models a single-key `Clear` as `MutClearRange`) so a
clear falls through to the legal-range check (2004), matching C++. The record layer's own
metadata-version bump (`database.go`: `SetVersionstampedValue` with the 14-zero-byte required
value) is unaffected — it is exactly the one legal case. Pinned by `TestDifferential_MetadataVersionKey`
(eight cases vs libfdb_c).

codex also flagged (P1) that the cgo `value_too_large`/`key_too_large` `Set` cases might
`abort()` via libfdb_c `CATCH_AND_DIE`. Not borne out for the Apple Go binding:
`fdb_transaction_set` buffers, and the size checks fire at *commit* and are returned
(CATCH_AND_RETURN), not aborted — verified empirically (those cases, and the new
oversized-key case, run to completion returning 2102/2103).

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
- `TestDifferential_VersionstampValidationOrder` (`pkg/fdbgo/bench/`) — eight cases pinning
  the eager-vs-deferred ordering of versionstamp-offset (2000) against key/value-size (2102/
  2103, eager) and transaction-size (2101, deferred), in both call orders plus the within-op
  intersections. The four "versionstamp op first" cases were **red** (go=2101/2102/2103,
  cgo=2000) before the per-mutation-loop fix and **green** (both 2000) after.
- `TestDifferential_MetadataVersionKey` (`pkg/fdbgo/bench/`) — eight cases pinning the
  metadataVersionKey write contract vs libfdb_c: the one legal `SetVersionstampedValue` (0),
  five `client_invalid_operation` (2000) rejections (wrong/short operand, plain Set, atomic
  Add, SetVersionstampedKey), and two `key_outside_legal_range` (2004) clears. All were
  silently committed (code 0) by the Go client before the fix.
- `TestTransactionSizeLimit_DefaultsToKnob` (`pkg/fdbgo/client/`, no FDB) — a default
  transaction's `sizeLimit` is `TRANSACTION_SIZE_LIMIT`; an explicit DB option lowers it.
  Revert-proof: with the default left at 0, the first assertion fails.
- `just test` green — the default change does not break any large-commit path.
