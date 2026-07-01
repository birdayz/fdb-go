# RFC-172: Commit from the coalesced RYW write map (finding #28)

**Status:** DESIGN ACK (FDB-C-dev) — ready for implementation. Required corrections folded in: the fold
gate is `isNonAssociativeOp`/`NON_ASSOCIATIVE_MASK` (ADD/OR/XOR/MAX/MIN/MINV2 fold only on EQUAL operand
length, not "always"), and 2101 is `transaction_too_large` (a byte-size limit, not a mutation count).
**Severity:** HIGH (app-breaking behavioral divergence). **Wire risk:** HIGHEST (the commit mutation vector).
**Scope:** RYW-enabled commits only (`!rywDisabled`); the RYW-disabled path already commits its op log 1:1
and stays as the control.

## Problem

Go's `tx.Commit` marshals `tx.mutations` — the raw, unfolded append log (one entry per `Set`/`Atomic`
call). libfdb_c does NOT commit its append log: `ReadYourWritesTransaction::commit` materializes the
**coalesced RYW write map** via `writeRangeToNativeTransaction` (`ReadYourWrites.actor.cpp:1997-2071`,
called from the commit actor at `:1392`). The write map folds same-key writes at INSERT time.

**Consequence (app-breaking):** a transaction that increments ONE counter key 150k times (or overwrites
one key repeatedly) works on libfdb_c/Java (folds to a single mutation) and FAILS on Go with `2101`.
`2101` is **`transaction_too_large`** — a pure **byte-size** limit: the commit gate compares the
transaction's serialized size against `options.sizeLimit` (default 10 MB), via `tr.expectedSize()` /
`getSize()` over the mutation vector (`NativeAPI.actor.cpp:6835`, `:7728`; `error_definitions.h:240`).
There is **no distinct mutation-COUNT limit** at 2101 — the earlier "too many mutations" framing was a
misnomer. The final DB state is identical either way; the divergence is the serialized BYTE size of the
committed mutation vector: Go ships the 150k-entry unfolded log (huge → trips the byte limit), libfdb_c
ships the single folded `ADD 150000` (tiny → under the limit). Verified vs the cgo differential.

## C++ mechanism (7.3.77)

**Fold, at insert time — `WriteMap::coalesceOver` (`WriteMap.cpp:480-494`).** The decision is a literal
port of these two branches; the impl MUST gate on `isNonAssociativeOp` (`NON_ASSOCIATIVE_MASK`,
`CommitTransaction.h:576-578`), NOT on a hand-listed "associative" set (the earlier draft got this
backwards — corrected here per the FDB-C-dev design review):

- **Same type AND newEntry is NOT `CompareAndClear`:**
  - if `isNonAssociativeOp(type)` **and** the existing top has a value **and** the two operand byte
    lengths **DIFFER** → **PUSH** (keep both); else → **FOLD** (`poppush(coalesce(existing,new))`).
  - `NON_ASSOCIATIVE_MASK` = `AddValue | Or | Xor | Max | Min | MinV2 | SetVersionstampedKey |
    SetVersionstampedValue | CompareAndClear`. So `ADD/OR/XOR/MAX/MIN/MINV2` fold **only when the new
    operand's byte length equals the existing top's** — a Go port that folds them unconditionally ships
    wrong bytes the moment operand widths differ.
  - The **only unconditional same-type folds** (associative — NOT in the mask) are
    `And, AndV2, ByteMin, ByteMax, AppendIfFits`. (The 150k-`ADD 1` headline still folds: all operands
    are 8 bytes → equal length → fold. The finding is real; the fold *condition* was mis-stated.)
- **Different type, OR newEntry IS `CompareAndClear`:**
  - if BOTH `existing` and `new` are atomic ops (`isAtomicOp`) → **PUSH** (two different atomic types
    keep both); else (the existing top is an absolute `SetValue`) → **FOLD** via `coalesce`.
  - Therefore `CompareAndClear` PUSHes over another atomic (`WriteMap.cpp:490-491`) but FOLDs over a
    pending `SetValue` (`coalesce`, `WriteMap.cpp:374-384`) — it is NOT an unconditional keep-both.
- `SetValue`/`ClearRange` are absolute; versionstamped ops are in the mask (push on size-mismatch, and
  they carry unreadable/`is_unreadable` stickiness handled by the write map).

**Materialize, at commit — `writeRangeToNativeTransaction` (`:1997-2071`):**
1. **Clears FIRST** (`:2004-2018`): emit `tr.clear` for every cleared sub-range, before any set/atomic
   ("because of keys that are both cleared and set to a new value").
2. Then per key, emit the (folded) operation stack `op[i]` for `i in 0..op.size()` (`:2035-2065`):
   `SetValue` with a present value → `tr.set`, absent → `tr.clear`; every atomic → `tr.atomicOp`.
3. Write-conflict ranges are emitted from the same sorted map walk (`:2022-2033, 2069-2071`).

## Go current state

- `tx.mutations` is the unfolded log (`transaction.go` `Set`/`Atomic` append).
- `tx.ryw` (`ryw.go`) is Go's WriteMap analog, already used for READ-your-writes (it holds the coalesced
  per-key operation stacks — the `do*` helpers at `ryw.go:1265-1474`). It is NOT currently materialized for
  commit.
- `buildCommitTransactionRequest` (`commitpath.go`) serializes `tx.mutations` verbatim.

## Design questions FDB C++ dev must resolve

1. **Where to coalesce:** port `coalesceOver` into the READ write-map (`rywCache.atomic`), unifying the
   read and commit fold (wider blast radius, one source of truth) — vs a commit-time materializer that
   walks the existing map (narrower, but duplicates fold logic). Recommendation to be decided.
2. **Fold decision table parity:** reproduce C++'s exact fold-vs-push table (same-type-associative fold;
   non-associative operand-size-change push; different-type push; CompareAndClear push;
   versionstamped/unreadable push). Confirm the `Min→MinV2`/`And→AndV2` op upgrade at `Atomic()`
   (apiVersion≥510) does not change which coalesce branch fires vs C++ (which upgrades at the same point).
3. **Write-conflict ranges:** source them from the coalesced map (C++ same-walk) or keep Go's separate
   `tx.writeConflicts` tracker — and does either change the shipped conflict-range bytes/count.
4. **Clears-first ordering:** the emitted vector must reproduce C++'s clears-before-sets ordering and the
   clear/set split around operation keys inside cleared ranges (`:2004-2018`).
5. **Limit + validation over the coalesced vector:** run the 2101 gate, `validateMutation`, and
   `GetApproximateSize`/`approximateCommitSize` (RFC-097) over the COALESCED vector, not the log.
6. **metadataVersionKey:** confirm `SetVersionstampedValue`/`SetVersionstampedKey` on `\xff/metadataVersion`
   stay 1:1 (not folded/misrouted); confirm tenant-prefix + versionstamp-offset handling is unaffected.

## Test plan (merge gate)

- **cgo differential over the full op-combination matrix**: for each shape (repeated ADD, ADD-then-SET,
  SET-then-CLEAR, mixed atomic types on one key, non-associative-operand-size-change, CompareAndClear,
  versionstamped-key/value, metadataVersion), assert the Go committed mutation vector is **byte-identical**
  to libfdb_c's.
- **The 150k-increment 2101 regression**: red before (Go 2101), green after (commits like cgo/Java).
- Scope strictly to `!rywDisabled`; a `rywDisabled` control asserts the op log still ships 1:1.
- Full binding-stress + 1M stress before/after (commit-path change).

## Why RFC-gated (not an inline fix)

~250-400 LOC touching the most critical wire path (the commit mutation vector), with a subtle fold table
and ordering contract. A wrong fold or ordering silently ships different bytes to the commit proxy for
every RYW write. Merge only behind the byte-identical differential above, one commit-path change at a time
through the full review gauntlet (FDB C++ dev + Torvalds + codex + @claude), never rushed at a session tail.
