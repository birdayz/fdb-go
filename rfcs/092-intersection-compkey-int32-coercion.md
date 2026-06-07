# RFC-092 — Intersection comparison keys must widen `int32` before tuple-packing

Part of TODO-production.md **P0.3-G**. Query-engine change (executor data-access /
cursor infra) → **Graefe ACK** (Cascades alignment) + **Torvalds** (code quality)
required.

## Problem

The FDB tuple layer (`pkg/fdbgo/fdb/tuple/tuple.go:373-406`) encodes `int`, `int64`,
`uint`, `uint64`, … but has **no `case int32`** — an `int32` element hits the
`default` arm and **panics** (`"unencodable element (%v, type %T)"`). This is correct
wire-parity with Apple's Go tuple layer and must not change.

The value-evaluation layer **does** produce `int32` (`values/values.go:615,1790`), so any
comparison key that evaluates to a 32-bit integer column is an `int32` at the cursor
boundary. Three sibling comparison/dedup-key builders consume such values:

| builder | use | int32 handling |
|---|---|---|
| `mergeSortCursor.extractKey` (`executor_new_plans.go:731`) | UNION/DISTINCT **dedup** | **widens `int32→int64`** ✓ |
| `aggregateCursor` group key (`streaming_cursors.go:225`) | GROUP BY **equality** | falls to `%T:%v` (injective ⇒ correct for equality) |
| `intersectionCompKeyFunc` (`executor.go:1402`) | INTERSECTION **merge order** | **stores raw `int32`** ✗ |
| `multiIntersectionCompKeyFunc` (`executor_new_plans.go:149`) | INTERSECTION **merge order** | **stores raw `int32`** ✗ |

The two intersection builders store the raw `Evaluate` result (`t[i] = v`). When the
merge cursor compares keys it does `bytes.Compare(a.Pack(), b.Pack())`
(`pkg/recordlayer/merge_cursor.go:28`). With an `int32` element, `Pack()` panics;
`compareKeys` **recovers** the panic into an error (pinned by
`bug_bounty_test.go::TestBug2_UnionCursorMixedKeyTypesPanic`), so the process does not
crash — but the **query fails with an error instead of returning rows**. An
`AND` of two index-scannable predicates on a 32-bit-integer-keyed record type is an
availability bug, not a crash, and a 3-way inconsistency with `extractKey`.

## Proposal

Widen `int32 → int64` in both intersection comparison-key builders, before the value is
placed in the `tuple.Tuple`, exactly as `extractKey` already does
(`case int32: t[i] = int64(tv)`):

```go
v, err := kv.Evaluate(qr.Datum)
if err != nil { /* capture keyErr, return */ }
if i32, ok := v.(int32); ok {
    v = int64(i32) // tuple has no int32; widening preserves integer sort order
}
t[i] = v
```

### Why only `int32`, not the full `extractKey` switch

`extractKey` is a **dedup/equality** key, so its `default: %T:%v` fallback is safe — any
injective string works. The intersection key drives **merge ordering** via the packed
byte comparison. `int32→int64` is the one coercion that is **order-preserving** (the FDB
tuple integer encoding orders `int64` identically to the index's integer encoding the
child streams are sorted by). A `%T:%v` string fallback for a genuinely exotic
comparison-key type could order differently from the input streams and silently
**mis-merge** the intersection — strictly worse than today's clean error. So exotic
types intentionally keep falling through to `compareKeys`' existing Pack-error path; only
the confirmed, order-safe, reachable `int32` case is fixed.

## Wire / parity impact

None. The tuple layer is untouched (stays Apple-parity, panicking on `int32`). Index
entry encoding, continuations, and record format are untouched. This is a read-side
executor fix: it only changes how an in-memory comparison key is built before an
in-memory `bytes.Compare`.

## Test

Unit regression on both builders: a `ConstantValue{Value: int32(N)}` comparison key →
the produced `tuple.Tuple` packs without panic and equals `tuple.Tuple{int64(N)}.Pack()`.
Before the fix the pack panics; after, it matches the `int64` encoding (proving
order-preservation). No planner round-trip needed to exercise the defect.

## Done when

Both intersection builders widen `int32`; the regression pins both; conformance + the
relevant executor tests stay green; Graefe + Torvalds ACK.

## Review outcome (2026-06-07)

**Graefe ACK, Torvalds ACK.** Graefe verified the order-preservation directly:
`key_expression_compiled.go:117` widens `Int32Kind` columns to `int64` in the *index*
encoding, so the children's sort order is already `int64` byte-order; the merge key now
matches via the same `tuple.encodeInt` path. Faithful to Java (Tuple stores `Long`; proto
int32 is read as `long`, so Java's `KEY_COMPARATOR` never sees a 32-bit element). Both
nits folded in: the widening is now a shared `widenInt32(any) any` helper (Torvalds), and
the symmetric **uint32** gap is documented (Graefe).

**Follow-up (non-blocking, not introduced here):** `uint32` is likewise tuple-unencodable
and `values.go`'s `toInt64ForArith` handles `int32` but not `uint32` — a pre-existing
symmetric gap, *not reachable* via the confirmed comparison-key paths (field reads
pre-widen at `query_result.go:131`). `widenInt32` intentionally leaves it on
`compareKeys`' Pack-error path rather than risk a non-monotonic coercion. Revisit only if
a `uint32` comparison-key path is ever shown reachable.
