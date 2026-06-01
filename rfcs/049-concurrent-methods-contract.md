# RFC-049: Honor the "methods safe for concurrent use" transaction contract

**Status:** Draft
**Item:** RFC-010 audit #7 (MEDIUM)
**Scope:** `pkg/fdbgo/client` transaction conflict/mutation buffers

## Problem

`fdb.Transaction` publishes this contract (`pkg/fdbgo/fdb/transaction.go:25`):

> Individual methods (Get, Set, Commit, etc.) are safe for concurrent use by
> multiple goroutines.

This is a *deliberate strengthening* over the Apple C binding. The C client runs
a single-threaded Flow actor, so `ReadYourWritesTransaction` never needs internal
locking. Our Go futures resolve on goroutines: a `Get` future's continuation
appends a **read conflict** (`addReadConflictForKey`, `transaction.go:421`) on a
*different* goroutine than the one the caller drives. `conflictMu` exists exactly
to make that safe.

The **writers** all honor it — `Set`/`Clear`/`ClearRange`/`Atomic` append
`mutations` under `conflictMu`; `addReadConflict*`/`addWriteConflict*` append the
conflict slices under it; `EnsureMutationCapacity` and the `OnError`
self-conflict copy lock too. But several **readers** and two **clears** touch the
same three slices (`mutations`, `readConflicts`, `writeConflicts`) with **no
lock**, so the published contract is a lie there — `-race` would fire:

| Site | Access |
|------|--------|
| `Commit` validation + read-only check (`transaction.go:774,794,809`) | reads `mutations`, `len(writeConflicts)` |
| `GetApproximateSize` (`transaction.go:1103–1111`) | reads all three |
| `buildCommitTransactionRequest` marshal (`commitpath.go:287,291,297`) | reads all three (zero-copy cast of `mutations`) |
| `commitDummyTransaction` (`commitpath.go:113,121`) | reads `writeConflicts`, `readConflicts` |
| `postCommitReset` (`transaction.go:1450`) | `mutations = mutations[:0]` **outside** the `conflictMu` block (the conflict-slice clears at 1454–1455 are already inside) |
| `reset` (`transaction.go:1484`) | same — `mutations[:0]` outside the lock |

**Realistic trigger** (not theoretical): a `Get` future resolving on goroutine G
appends to `readConflicts`; the caller's goroutine calls `Commit` →
`buildCommitTransactionRequest`, which iterates `readConflicts` to build the
`ReadConflictRanges` — concurrent append vs. concurrent read of the same slice =
data race. `GetApproximateSize` is a public method users poll while a transaction
is still accumulating reads from in-flight futures.

## Investigation

**What the contract requires.** The bar is *memory safety / race-freedom*, not a
serialization point. Racing a `Set` against a `Commit` on the same transaction is
inherently undefined regardless of locking (does the mutation make the batch?).
The C binding never even allows it (single actor). Our promise is only: concurrent
calls don't corrupt memory, don't trip `-race`, and don't crash. The one race the
contract is genuinely *meant* to cover — read futures appending conflicts while
the caller builds/commits — touches **only `readConflicts`**, which is consumed
**only** by the marshal path (and the already-locked `OnError` self-conflict copy).

**Why snapshot-and-release is sufficient (not hold-during-marshal).** The three
slices are **append-only**: no code mutates an existing element in place.
- `mutations`: `Set`/`Clear`/`Atomic` only `append`. The zero-copy cast aliases
  the backing array; a concurrent append either reallocates (snapshot keeps the
  old array, GC-live) or writes at index `len` (beyond the snapshot's length —
  a different memory location, not a race against the snapshot's `[0:len)` reads).
  The tenant path already copies headers into a pooled scratch before prefixing,
  so it never writes the aliased array (RFC-010 #4).
- `readConflicts`/`writeConflicts`: `KeyRange.Begin/End` point into `conflictBuf`.
  `conflictBufAlloc` only **reserves new regions** (`buf[len:len+n]`) or reallocates
  (copies the old buffer — a read, not an overwrite). It never overwrites bytes an
  existing `KeyRange` points at. So a captured `KeyRange` stays byte-stable.

Therefore copying the three slice **headers** (3 machine words each) under
`conflictMu`, releasing, then operating on the locals is fully race-free — and it
keeps the critical section to a few-word copy instead of holding a transaction-wide
lock across `MarshalFDBPooled` + pool churn. This mirrors C++ `tryCommit`, which
builds the `CommitTransactionRequest` once and operates on that immutable snapshot
(the request is passed by value; `applyTenantPrefix` builds a fresh
`VectorRef<MutationRef>`).

**`conflictBuf` reset is out of scope of the race.** `reset`/`postCommitReset` do
`conflictBuf = conflictBuf[:0]`, which *would* let a later `conflictBufAlloc`
overwrite a snapshot's bytes. But `Reset()` is documented NOT concurrent-safe
(`fdb/transaction.go:26`: drain futures first), and these run after `Commit`
returns, so no marshal snapshot is live across them. Unchanged.

**RYW lost-update stays documented-not-safe.** The RYW cache (`tx.ryw`) is a
separate concern. Two goroutines doing read-modify-write through the RYW layer can
still lose an update; that is explicitly out of scope (the C binding has the same
property under its own contract) and the existing `TestConcurrentRYW_SameTransaction`
already documents it as "doesn't crash / corrupt," not "serializable."

### Reset-vs-snapshot boundary — and why `GetApproximateSize` must HOLD the lock

A reviewer flagged that the snapshot's race-freedom proof leans on "the backing
buffers are never overwritten," yet `reset`/`postCommitReset` do `mutations[:0]` /
`conflictBuf[:0]` (**reuse** the backing arrays), and a later `Set`/`conflictBufAlloc`
*overwrites* slots a released snapshot still points at. He was right, and a `-race`
test proved it: a released `GetApproximateSize` snapshot reads `mutations[0]` at the
same address a post-reset `Set` writes. The fix splits by **which reset can race
which reader**:

- **`GetApproximateSize` HOLDS `conflictMu` across its iteration** (no snapshot).
  It is a public method a monitoring goroutine may call concurrently with `Commit`
  on another goroutine, and `Commit`'s **auto-reset** (`postCommitReset`) reuses the
  backing arrays. That overlap is **in-contract** (`Get*`/`Commit`/`GetApproximateSize`
  are all concurrent-safe), so a released snapshot is genuinely unsafe here. Holding
  the lock blocks reset/append for the (pure-CPU, microsecond) duration of the read.

- **`Commit`-validation, the marshal, and `commitDummyTransaction` keep
  snapshot-and-release.** These run *inside* a `Commit`, so the only reset that could
  overwrite their snapshot is **another goroutine resetting the same transaction**:
  a second concurrent `Commit`/`Reset` on one tx. That is out-of-contract — you don't
  commit the same transaction from two goroutines, and `Reset()` requires draining
  pending ops first (`fdb/transaction.go:26`). The *in-contract* concurrency they
  face is read-futures **appending** conflicts (and the caller `Set`ting), which is
  append-only and safe for a released snapshot (a concurrent append writes index
  `len` or reallocates — never an index the snapshot reads). Holding the lock across
  `MarshalFDBPooled` would needlessly serialize it and buys nothing the boundary
  doesn't already give.

The `-race` tests exercise the in-contract cases (GetApproximateSize-vs-reset,
conflict-readers-vs-appends, concurrent Sets); they deliberately do **not** race two
`Commit`s / a `Reset` against a live marshal snapshot — that would be testing a
contract violation no correct program performs.

### Option setters and `tags` are not "operations" (contract scope)

The published doc said "Get, Set, Commit, **etc.**" — the "etc." over-promised. The
genuine operation-vs-operation race the contract must cover is two data ops on the
shared buffers. Option setters (`SetPriority`, `SetWriteConflictsDisabled`,
`SetTag`, …) write config fields (`tags` at 1373, `writeConflictsDisabled` at 1330)
that operations only *read* — a setter racing an operation is **setter-vs-operation**,
the same "configure before use" boundary the FDB C API itself draws
(`fdb_transaction_set_option` is not concurrent-safe with operations). So `tags`
and the option fields are **out of the operation concurrency contract** — narrowing
the doc to say so is the fix, not locking ten setters and their readers.

The **one exception** is `nextWriteNoConflict`: unlike the other option fields it is
read **and written on the `Set`/`Clear`/`Atomic` operation path** (`addWriteConflict*`
clears it after consuming it, 1232–1233 / 1250–1251). Two concurrent `Set`s
therefore race on it operation-vs-operation — squarely in contract. This must be
moved under `conflictMu` (see Fix #6).

## Fix

Pure locking discipline — no signature or behavior change on the single-threaded
(C-binding-equivalent) path; identical bytes on the wire.

1. **`buildCommitTransactionRequest`** — marshal the validated `muts` snapshot
   threaded in by `Commit` (the zero-copy cast reads that snapshot's header).
   Snapshot the (unvalidated) read/write conflict headers under `conflictMu` at
   entry, release, then range over the locals.
2. **`Commit`** — snapshot `mutations` + `len(writeConflicts)` once under the
   lock; the two validation loops and the read-only short-circuit use the snapshot.
   **Thread that same `muts` snapshot into `commit()` → `buildCommitTransactionRequest`**
   so the marshaled (shipped) set is byte-identical to the validated set. Without
   this the marshal re-read `tx.mutations` independently, and a `Set` landing on
   another goroutine between validation and marshal (allowed by the contract)
   would ship an **unvalidated** mutation to the commit proxy — bypassing the
   `maxWriteKey`/`metadataVersionKey`/versionstamp-offset checks (FDB-C reviewer
   catch). The racing `Set` now lands in `tx.mutations` *beyond* the snapshot and
   is simply not in this commit. (Safe against buffer reuse too: only an
   out-of-contract concurrent `Commit`/`Reset` could reset mid-validation.)
3. **`GetApproximateSize`** — iterate all three **under** `conflictMu` (not a
   released snapshot): it can race `Commit`'s in-contract auto-reset, which reuses
   the backing arrays. (Caller `Commit` does *not* hold `conflictMu`, so no
   re-entrancy/deadlock.)
4. **`commitDummyTransaction`** — snapshot `writeConflicts`/`readConflicts` under
   the lock before the `len`-check and `intersectConflictRanges`.
5. **`postCommitReset` / `reset`** — move `mutations = mutations[:0]` *inside* the
   existing `conflictMu` critical section (next to the conflict-slice clears).
6. **`addWriteConflictForKey` / `addWriteConflict`** — move the
   `writeConflictsDisabled` / `nextWriteNoConflict` check (and the
   `nextWriteNoConflict = false` clear) *inside* `conflictMu`. The lock is already
   taken a few lines down for the append; extending it up to cover the flags closes
   the operation-vs-operation race on `nextWriteNoConflict` without a new lock.
   `reset`'s `nextWriteNoConflict = false` (1483) moves into its `conflictMu` block
   for the same reason. Exact semantics preserved (`writeConflictsDisabled` still
   short-circuits *without* touching `nextWriteNoConflict`).
7. **Tighten the contract doc** (`fdb/transaction.go:25`) — replace "Get, Set,
   Commit, etc." with: data operations (`Get`/`GetRange`/`Set`/`Clear`/`Atomic`/
   `Commit`/`GetApproximateSize` and conflict-range adders) are safe for concurrent
   use; option setters (`SetXxx`) and `Reset` are **not** — configure the
   transaction before issuing concurrent operations (matches the FDB C API's
   `fdb_transaction_set_option` contract).
8. **`Set`/`Clear`/`ClearRange`/`Atomic` publish the mutation + its write-conflict
   range atomically** — hold `conflictMu` across *both* appends (via new
   `addWriteConflictForKeyLocked`/`addWriteConflictLocked` bodies; the lock-taking
   `addWriteConflictForKey`/`addWriteConflict` survive as thin wrappers for the
   public `AddWriteConflict*` methods + the dummy-tx builder). **Codex catch:** the
   old code appended the mutation under the lock, *released*, then re-acquired to
   append the write conflict. A `Commit` snapshot landing in that window shipped
   the mutation **without** its conflict range → a concurrent transaction that read
   the key would not be conflicted (a **missed** conflict — the dangerous
   direction, not a spurious one). One lock now covers both, so a Commit snapshot
   sees the whole write or none of it. (This also subsumes Fix #6 — the
   `nextWriteNoConflict` consume now happens inside the same critical section — and
   *reduces* `Set` from two lock acquisitions to one.)

## Performance

- Per reader: one `Mutex.Lock`/`Unlock` (uncontended fast path is a single CAS)
  plus three slice-header copies. Negligible vs. the network commit it precedes.
- Marshal critical section *shrinks* relative to "hold during marshal": the lock
  is held for a few-word copy, not across `MarshalFDBPooled`. No new allocations.
- Single-threaded callers (the overwhelming majority, matching C-binding usage)
  pay only the uncontended lock cost, which is already paid on every `Set`.
- Stress-1M is a planner/executor benchmark above this layer; no change expected,
  but I'll confirm before/after is within noise if a reviewer wants it.

## Test plan

A deterministic `-race` unit test is the load-bearing proof (no FDB cluster
needed — `Transaction{}` is directly constructible, per existing
`transaction_unit_test.go`). New `transaction_concurrent_test.go`:

- **`TestConcurrent_ConflictReaders_NoRace`**: one goroutine hammers
  `addReadConflictForKey`/`Set` (the read-future + writer roles); concurrently
  other goroutines call `GetApproximateSize()` and `buildCommitTransactionRequest()`
  in a tight loop. Asserts no race (the suite runs under `-race` in CI) and no
  panic. **Fails on master under `-race`** (proves the bug), passes after the fix.
- **`TestConcurrent_NextWriteNoConflict_NoRace`**: `SetNextWriteNoWriteConflictRange()`
  then many concurrent `Set`s. **Fails on master under `-race`** (the read+write of
  `nextWriteNoConflict` on the `Set` path), passes after Fix #6/#8.
- **`TestConcurrent_SetIsAtomic`** (Fix #8, codex): 20k plain `Set`s + an observer
  snapshotting `len(mutations)` and `len(writeConflicts)` under the lock — they must
  never differ. **Fails on the pre-fix code** (5985 torn-write observations: a
  mutation visible without its conflict), 0 after. A logical-invariant test (no
  `-race` needed).
- **`TestConcurrent_ResetWhileSizing_NoRace`**: races `postCommitReset`/`reset`'s
  `mutations[:0]` against `GetApproximateSize`/`Set` to pin the moved clear.
  (Does NOT race `Reset` against a live `Commit` snapshot — that is the
  out-of-contract case per the Reset-vs-snapshot boundary above.)
- Snapshot-correctness: `buildCommitTransactionRequest` over a known
  `mutations`/conflict set still produces byte-identical `ReadConflictRanges` /
  `WriteConflictRanges` / `Mutations` (guards against a snapshot dropping data).
- **Tenant no-alias assertion** (FDB-C reviewer ask): after
  `buildCommitTransactionRequest` on a tenant-scoped tx, assert the prefixed
  `Param1`/`Param2` of the marshaled mutations do **not** alias the
  `tx.mutations` backing array or `conflictBuf`, and `tx.mutations` is byte-unchanged
  (the snapshot + tenant-scratch copy must not write through the alias).
- Full `just test` (46 targets) green, including the existing
  `concurrent_stress_test.go` FDB integration suite under `-race`.
