# RFC-233: A type comparison must not build a type

**Status:** PROPOSED — measured, designed, not yet implemented.
**Base:** `ed4045adb` (branch `perf/plan-structural-hash-memo`, PR #754)
**Relates to:** RFC-232 (exact `FieldValue`/QOV resolution), RFC-197
**Wire impact:** none. No key, record, index, continuation, or protobuf plan
encoding changes. This is a read-side planner change only.

## 1. Decision and scope

Comparing two flowed row types, or asking whether one exists, must not allocate
an ordinary `Type` graph.

After this change:

- type EQUALITY between two exact-carrying values is answered by interned handle
  identity, not by thawing both sides and walking them;
- an existence check on a flowed type is answered from the handle, not by
  thawing and comparing to `nil`;
- `Type()` and `FlowedType()` keep returning a FRESH graph, unchanged. That
  contract is deliberately pinned and this RFC does not touch it.

Explicitly out of scope: memoizing `thaw` itself. See §4.

## 2. Why — the measurement, not a suspicion

RFC-232 left the planner slower than master. The gap resisted CPU profiling
because it is not a hot spot: on `TestStatsInvariant_PurePlannerSweep` no
application node exceeds ~2.3% flat.

The profile shape explains why. **The planner is allocation-bound**, measured on
a 171s run of that sweep at `ed4045adb`:

| runtime node | cumulative |
|---|---|
| `runtime.gcDrain` | **42.72%** |
| `runtime.scanSpan` | 31.61% |
| `runtime.scanObjectsSmall` | 23.28% |
| `runtime.mallocgc` | 16.78% |

Against a profile shaped like that, cutting allocation is the only lever that
moves anything. So the question is what allocates. Over the same sweep, 112.01 GB
total:

| site | alloc | share |
|---|---|---|
| **`exactType.thaw`** | **8.87 GB** | **7.92%** |
| `GetCorrelatedToOfValue.func1` | 8.17 GB | 7.29% |
| `newStructuralKey` | 8.10 GB | 7.23% |

`thaw` is the single largest allocator in the planner, and what it allocates is
pure waste: it rebuilds an ordinary `Type` graph from an INTERNED, IMMUTABLE
exact handle, producing an identical graph every call.

Its callers are the hot ones — `QOV.FlowedType` (28.5% of thaw),
`exactType.Type` (28.0%), `physicalFlowedRecordType` (14.5%),
`LayoutWithSeedLegs` (12.1%), `fieldValue.Type` (9.8%).

**The waste concentrates in comparisons.** Of 141 non-test `.FlowedType()` call
sites:

| shape | sites | available win |
|---|---|---|
| `.FlowedType().Equals(…)` / `Equals(….FlowedType())` | 53 | full, exactly equivalent |
| `.FlowedType() == nil` / `!= nil` | 13 | full, no thaw needed |
| `QuantifiedRowShapesAgree(…)` | 20 | positive short-circuit only (§3.2) |

`a.FlowedType().Equals(b.FlowedType())` allocates TWO complete graphs to produce
one bool.

## 3. Design

### 3.1 Handle identity IS type identity

The exact table interns: every path building an exact node routes through it, so
two structurally equal types are the SAME object. That is not an aspiration —
`TestEveryExactNodeIsInterned` pins it, and `ExactTypeForValue`'s doc already
states the long way round returns the same OBJECT, which is what licenses that
shortcut today.

So for two values carrying handles, `ea == eb` is exactly equivalent to
`a.FlowedType().Equals(b.FlowedType())`, in BOTH directions, at O(1) and zero
allocation.

### 3.2 `QuantifiedRowShapesAgree` gets only a positive short-circuit

That predicate is nullability-TOLERANT: when nullability differs it does not
reduce to `Equals`. Handles differ by nullability, so handle identity implies
agreement but handle inequality implies nothing. This RFC therefore uses it only
as a fast TRUE and falls through otherwise. Stating this explicitly because the
symmetric-looking case is where a "same thing, cheaper" change would silently
alter planner decisions.

### 3.3 The pattern already exists in-tree

This extends a mechanism rather than inventing one. `OrdinalDomainOfQuantified`
already takes the handle via `exactTypeOfValue`, answers from it, memoizes the
answer on the node (`exactType.ordinalDomain`), and goes the long way only for a
foreign QOV view that carries no handle. `thawShared()` exists for the internal
non-allocating case. The new helpers follow that shape exactly, including the
fallback.

## 4. Rejected: memoizing `thaw`

The obvious move — cache the thawed graph on the interned handle, since the
handle is immutable — is WRONG, and it is worth recording why so nobody
re-derives it.

`Type()`'s freshness is a pinned contract.
`rfc232_qov_exact_identity_test.go` mutates a `Type()` result:

```go
first.RecordName = "THAW-MUTATED"
first.Fields[0].Name = "THAW-FIELD-MUTATED"
second := qov.Type().(*values.RecordType)   // must still read "R" / "ID"
```

and requires a later `Type()` to be unaffected. A cache breaks that
deliberately. Partial schemes fail too: sharing the top node fails the
`RecordName` assertion, and sharing the `Fields` slice fails the field-name
assertion. Sharing child primitives would hand out globals that a caller could
corrupt.

The correct response is to stop CALLING thaw on the comparison paths, not to
make thaw cheaper. That also leaves the mutation contract — and the test — fully
intact, which is the conservative direction.

## 5. Expected effect, and how it will be judged

Removing the comparison and existence thaws should eliminate a large fraction of
that 8.87 GB. It is deliberately NOT predicted as a wall-clock number here:
allocation volume and time are different currencies, and this codebase has
already been burned once by assuming otherwise — 8.9 GB attributed to
`newStructuralKey` bought only 2.8% when memoized.

The claim to be tested is the ratio on `TestStatsInvariant_PurePlannerSweep`,
n>=2 per side, sequential, against the true merge-base, per the stress-comparison
recipe. Current position: **1.150x** vs master (down from 1.184x pre-memo);
end-to-end 1M is 1.020x.

## 6. Risk

The equality substitution is exact, so the risk is not in the arithmetic — it is
in whether every compared value actually carries a handle. A value that does not
falls back to the existing path, so a miss costs performance and never
correctness.

The pin required: a test asserting the fast path and the slow path AGREE on the
same inputs, driven over both carrying-handle and foreign-view cases, so a future
divergence between interning and `Equals` is caught here rather than as a wrong
plan.
