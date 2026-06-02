# RFC-062: Atomic-fold differential across operand/base widths and edge operands

**Status:** Draft
**Item:** RFC-010 C3 (fresh differential axes) — "atomic-op edge cases across ALL of `Atomic.h`
(empty / missing / present-empty operand per op)". Atomic fold semantics are the **wire hard
line**: the folded value is what gets persisted and what Java/C clients read.

## Problem

The Go atomic fold (`pkg/fdbgo/client/ryw.go:1053-1262`, `doAdd`/`doAnd`/`doOr`/`doXor`/`doMax`/
`doMin`/`doByteMin`/`doByteMax`/`doAppendIfFits`/`doCompareAndClear` + `doAndV2`/`doMinV2`) is a
faithful 1:1 port of C++ `Atomic.h`, including the subtle **result-width = operand-width**
truncation rule (Add/And/Or/Xor/Max/Min) and the absent/empty semantics. But the **differential**
(go-vs-cgo) coverage does not prove that identity across widths:

- `TestDifferential_WriteBattery` tests 11/12 ops, but only **single-op, missing-key, 8-byte
  operand** (and omits `CompareAndClear` entirely).
- `TestDifferential_RYWCoalescing` tests the client-side RYW fold (Set+Atomic same txn) but with
  **8-byte or 1-byte operands** only; the one width-asymmetric case is `set_then_add` (8-byte
  base + 1-byte operand). No long operands (>8 bytes, past Go's 8-byte fast path), no
  operand-longer-than-base for And/Or/Xor, no empty operand, no present-base ByteMin/ByteMax
  folds, no missing-key `CompareAndClear`.
- The unit-level `ryw_fuzz_test` exercises all ops across widths but against an **in-memory model,
  not libfdb_c** — so it proves Go-internal consistency, not cross-engine identity.

So the wire-critical claim "Go folds atomics byte-identically to libfdb_c" is unproven on the
**width dimension** — exactly where a port is most likely to drift (an off-by-one in a byte loop,
a fast-path/slow-path split, a zero-pad-on-existing-win).

## Investigation

**Where Go's fold actually runs (corrected during the teeth-check).** `tx.Set`/`tx.Atomic`
append **raw mutations** to `tx.mutations` (`transaction.go:795`); `Commit` ships the raw stream
`[SetValue, AddValue, …]` and the **server** folds it. So a commit-then-read-back differential
exercises the *server* fold + the mutation wire format — **NOT** Go's `doAdd`/`doMin`/etc. Go's
client-side fold (`ryw.go`: Site B coalesce when an atomic lands on a pending Set, and
`resolveAtomics` against a storage base) runs **only on a read WITHIN the txn** (read-your-writes).
An earlier commit-only version of this test passed even with `doAdd` deliberately broken — the
teeth-check caught it. So every case must do an **in-txn read** to exercise the fold under test.

Two things to verify, both reachable, both must match libfdb_c:

1. **Client-side fold (the thing under test)** — after `Set(base)` + `Atomic(operand)` (or a bare
   `Atomic` against a committed storage base), a **read within the same txn** returns the
   RYW-folded value via Go's `ryw.go`. This is where a port drifts.
2. **Server-side fold + wire format** — after commit, a read-back returns the server-folded
   value. Both clients send the same mutation stream, so this is the weaker check (it fails only
   on a wrong op code/operand encoding).

The facade `tx.Add`/`tx.And`/`tx.Min` emit the V2 op codes at API 730 (the
`apiVersionAtLeast(510)` upgrade), so testing through the facade exercises `AddValue`/`AndV2`/
`MinV2` — the codes a modern app actually sends.

## Fix

Net-new differential coverage (no production change expected — the fold is a faithful port; this
proves it across the untested width dimension). If a width case diverges, that is a wire bug
fixed in `ryw.go` under this RFC with the differential as its sentinel. Teeth verified by
fault-injection on a fold function (e.g. drop the operand-width truncation) and confirming the
matching case fails, then reverting.

New file `pkg/fdbgo/bench/differential_atomic_test.go`. Each case runs on both clients via a
custom runner that, in one txn, does `Set(base)` (when `base != nil`) + `Atomic(operand)` then a
**read within that txn** (Go's client-side fold), commits, and reads back (server fold). It
byte-compares go-vs-cgo for **both** the in-txn value (the primary assertion — fault-injecting a
fold function breaks this) and the committed value (server fold / wire format), and asserts
in-txn == committed within each client (read-your-writes consistency). go and cgo use **distinct
key prefixes** (a missing-key fold reads storage; sharing a key would let the second client's
fold see the first's committed value — found during the teeth-check). Both `base != nil` (fold
over a pending Set, Site B) and `base == nil` (fold over storage-absent, `resolveAtomics`) rows
exercise Go's fold via the in-txn read.

1. **`TestDifferential_AtomicFoldWidths`** — width-sensitive ops (`Add`/`And`/`Or`/`Xor`/`Max`/
   `Min`), `Set(base)` + `Atomic(operand)` over `(len(base), len(operand))`: operand-longer
   `(1,8)`; long operand past Go's 8-byte fast path `(8,16)`, `(16,8)`; symmetric non-8 `(3,3)`;
   Add carry wrap at 1-byte and 8-byte widths; **empty operand**; **present-empty base** — run
   for `And` and `Min` specifically so `AndV2`/`MinV2` fold via `doAnd`/`doMin` (→ empty/zero)
   rather than the absent→operand path. **Max/Min** additionally cover: operand-width-only
   comparison (high base bytes ignored), existing-wins with base **wider** than operand
   (truncate) AND base **shorter** than operand (**zero-pad** — the `len(existing) < len(param)`
   branch, FDB-C++ dev), and the operand-longer-with-nonzero-high-byte early-return/early-scan.
   The operand-shorter `(8,1)` combo is **not** repeated (already in RYWCoalescing's
   `set_then_add`). Each op also has a missing-key (server-fold) row.
2. **`TestDifferential_ByteMinMaxFold`** — `ByteMin`/`ByteMax` vs a present base: base-greater,
   base-less, **operand-is-prefix-of-base** and base-is-prefix-of-operand (lexicographic, no
   truncation — full winning value), equal, plus missing-key.
3. **`TestDifferential_CompareAndClearFold`** — present-equal (clears), present-unequal (kept),
   present-vs-empty-operand, empty-base-vs-empty-operand (equal → clears),
   **operand-prefix-of-base** (unequal → kept, proving full-byte compare), and missing-key (the
   `WriteBattery` gap) with non-empty and empty operand.
4. **`TestDifferential_AppendIfFitsFold`** — present-base concat, empty base (→ operand), empty
   operand (→ base), missing-key, and a 90KB concat that still fits under the 100KB limit.

All assert byte-identical persisted state via `runDifferentialSequence`.

## Performance

Test-only. No production change unless a divergence is found.

## Test plan

The tests above ARE the plan. Teeth verified by fault injection on the **in-txn** assertion:
breaking `doAdd`'s operand-width (`size := len(e)` instead of `len(param)`) reddened 6 add rows
(`add_b1_o8` go=`0a` vs cgo=`0a00000000000000`, plus the width/empty/missing rows); flipping
`doByteMin`'s comparison reddened 4 bytemin rows. The committed-only version of the same test
passed under both faults — confirming the in-txn read is load-bearing. All revert clean. Run
under `bazelisk test //pkg/fdbgo/bench:bench_test`; the existing `FuzzDifferential` continues to
cover random sequences.
