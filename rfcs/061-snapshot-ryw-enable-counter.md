# RFC-061: SNAPSHOT_RYW_ENABLE/DISABLE must be a counter, not a one-way boolean

**Status:** Implemented
**Item:** RFC-010 C3 (fresh differential axes — error-code/option semantics). A divergence found
by the transaction-option-semantics survey and **confirmed differentially against libfdb_c**.

## Problem

libfdb_c models snapshot read-your-writes as an **integer counter**, not a boolean:

- `int snapshotRywEnabled` (`ReadYourWrites.h:47`), initialized per-transaction from the
  database default `apiVersion.hasSnapshotRYW() ? 1 : 0` (`NativeAPI.actor.cpp:1603`) — i.e. **1**
  for any modern API (≥ 300; our clients run at API 730).
- `SNAPSHOT_RYW_ENABLE` does `options.snapshotRywEnabled++`; `SNAPSHOT_RYW_DISABLE` does
  `options.snapshotRywEnabled--` (`ReadYourWrites.actor.cpp:2588-2596`).
- A snapshot read bypasses the RYW cache iff `snapshotRywEnabled <= 0`
  (`ReadYourWrites.actor.cpp:402`).

So `disable → enable` returns the count to 1 → snapshot reads **see the txn's own pending writes
again**; and two disables need two enables to re-enable.

The Go client modeled this as a **boolean** `snapshotRYWDisabled` (`transaction.go:252`) with
`SetSnapshotRYWDisable()` setting it `true` (`transaction.go:1606`) and the facade
`SetSnapshotRywEnable()` a **no-op** (`fdb/options.go:108`). There is no transaction-level
re-enable at all. Once disabled, a snapshot read can never go back through RYW. C++ is the spec
for the FDB client, so this is a Go bug: go is silently stuck-disabled.

## Investigation (differentially confirmed — the bug is real)

`TestDifferential_SnapshotRYWReenable` (`pkg/fdbgo/bench/`) writes a pending (uncommitted) value,
applies a sequence of toggles, then snapshot-reads the key; "pending" = RYW active (saw own
write), "absent" = bypassed to storage. On the unfixed code:

| toggle sequence | libfdb_c | Go (before) | C++ count |
|---|---|---|---|
| (none) | pending | pending | 1 |
| disable | absent | absent | 0 |
| **disable → enable** | **pending** | **absent** ✗ | 1 |
| **enable → disable** | **pending** | **absent** ✗ | 1 |
| disable → disable → enable | absent | absent | 0 |
| **disable → enable → enable** | **pending** | **absent** ✗ | 2 |

Three sequences diverge (go stuck "absent"). Note `disable → disable → enable` **agrees**
(both "absent") only because C++'s count is 0 there — this case is the discriminator that proves
the fix must be a **counter**, not a boolean that `enable` merely resets (a reset would make go
return "pending" there and re-diverge).

## Fix

Model the counter. Replace the boolean `snapshotRYWDisabled bool` with an integer
`snapshotRYWDisableCount int`, keeping the field disabled-oriented (its **Go zero value, 0, means
enabled** — matching both the current boolean default and the C++ default of 1-for-modern-API,
with no constructor magic required):

- `SetSnapshotRYWDisable()`: `snapshotRYWDisableCount++`.
- `SetSnapshotRYWEnable()` (new on `Transaction`): `snapshotRYWDisableCount--`.
- The four snapshot read sites (`Snapshot.Get`/`GetKey`/`GetRange`/`GetRangeReverse`,
  `transaction.go:332/350/363/379`) bypass RYW iff `snapshotRYWDisableCount > 0`.
- Facade `fdb/options.go:108 SetSnapshotRywEnable()` calls `tx.inner.SetSnapshotRYWEnable()`
  instead of returning nil.
- **Reset: preserve the count, do NOT zero it.** `reset()`/`postCommitReset()` already leave
  `snapshotRYWDisabled` untouched — it is a persistent option that C++ re-applies from
  `persistentOptions` on retry, so preserving the net count gives the identical state. This
  matches the existing `rywDisabled` treatment (listed as preserved-across-reset at
  `transaction.go:1766-1767`). So the reset functions need no change beyond updating the
  preserved-options comment to name the count field. (An earlier draft said "zero the count" —
  that was wrong; it would drop a user's persistent DISABLE on the first retry.)

**Why disabled-oriented (inverse) instead of C++'s `snapshotRywEnabled`-default-1?** Behaviorally
identical for every sequence — `disableCount = 1 − enabledCount`, so `enabledCount <= 0 ⟺
disableCount > 0` — but Go's zero value makes "default enabled" fall out for free, with no
"initialize to 1 in every constructor" footgun (a zero-valued `Transaction{}` literal, or the
commit-path dummy, would silently bypass RYW under an enabled-counter-default-1 scheme). This is
CLAUDE.md principle 10 (match the architectural property — net enable count, bypass when
non-positive — over the literal field representation). The C++ api-version nuance
(`hasSnapshotRYW() ? 1 : 0`) is moot: the Go client only targets API ≥ 520.

## Performance

A counter increment/decrement and an `int` compare on the snapshot read path instead of a bool
load. No wire impact.

## Test plan

`TestDifferential_SnapshotRYWReenable` (above) — sequences asserting the snapshot read of a
pending write agrees with libfdb_c. Three are red→green (disable→enable, enable→disable,
disable→enable→enable); `disable→disable→enable` is the counter-vs-boolean discriminator (stays
green only because the fix is a true counter). Added per FDB-C++ dev review: an **enable-only**
sequence (disableCount → −1, i.e. C++ enabledCount 2) to prove the count goes **negative** and
does not clamp at 0 — the one axis the ≥0 sequences leave unprobed; `enable→disable` then proves
it recovers to the default from a negative excursion. Plus a **READ_YOUR_WRITES_DISABLE-dominates**
case (clean, pre-op RYW-disable + snapshot-enabled still bypasses, matching the separate
`readYourWritesDisabled` check at `ReadYourWrites.actor.cpp:400`). Every sequence runs across
**all four snapshot read paths** (`Get`, `GetRange`, reverse `GetRange`, `GetKey`) — the fix
touches all four, so each is cross-checked, not just `Get` (@claude review). Plus updated
white-box unit coverage for the new `SetSnapshotRYWEnable` and the count field (including
preservation across `reset()`).
