# Factory corpus retirement ledger

A committed factory scenario is a frozen expectation. When one stops being the
right expectation, the reason is recorded here, permanently, next to the corpus
it governs.

This file exists because a re-bless erases its own motive. After the digests
move, the diff shows two hex strings changing and nothing about why the old
plan was wrong — and the next person to see a similar drift has no way to tell
a fix from a regression. RFC-201 §5.3 allows re-blessing only inside a
behaviour-changing fix; this ledger is where that behaviour change is stated in
words.

There is no ledger convention elsewhere in the repo for this
(`javacorpus/ledger.go` and `yamsql/ansiledger.go` are Go inapplicability
ledgers, a different instrument), so this file is the home.

---

## Float-leading ORDER BY loses its sort elision — CORRECTNESS CHANGE

**Retired: 27 scenarios — 9 candidates × 3 projections — across 9 family files.
Result rows: UNCHANGED.**

(An earlier revision of this line called those 9 candidates "9 plan points".
That is wrong, and the two are not interchangeable: a plan point is a
`(feature vector, plan shape)` pair, which is exactly what the dedup key
digests, and the three projections of one candidate carry three DIFFERENT
feature vectors — `proj=star`, `proj=n1`, `proj=n5`. So the 27 scenarios are
**27 distinct plan points**, held by 9 candidates. Counted, not reasoned:
re-deriving every committed header against master yields 27 drifted scenarios
with 27 distinct dedup keys over 9 distinct `(seed, query index)` candidates in
9 files.)

### What the old plan claimed

That an index scan whose leading *sorted* coordinate is a `FLOAT` or `DOUBLE`
column delivers rows in that column's order, so an `ORDER BY <float>, id` could
be answered by scanning the index and skipping the sort entirely. Every retired
scenario is of the form `ORDER BY <float column>[ NULLS FIRST], id` where the
float is the leading sort column and an index on it exists.

Representative, `fc_0000000019_q0_p1` —
`SELECT id FROM t_rd WHERE (((s <> 'beta') AND (d = 4)) OR ((a > 2) AND (f = FALSE) AND (CAST(c AS STRING) <= '6'))) ORDER BY e NULLS FIRST, id`:

```
old: [RecordQueryProjectionPlan  RecordQueryPredicatesFilterPlan
        RecordQueryFetchFromPartialRecordPlan  RecordQueryIndexPlan]

new: [RecordQueryProjectionPlan  RecordQueryInMemorySortPlan
        RecordQueryPredicatesFilterPlan  RecordQueryScanPlan]
```

The `RecordQueryInMemorySortPlan` that appears is the sort that should never
have been elided.

### Why the old claim is unsound

FDB tuple encoding flips the sign bit of a non-negative double and every bit of
a negative one, laying the IEEE-754 domain out physically as:

```
negative NaN payloads < -Inf < … < -0.0 < +0.0 < … < +Inf < positive NaN payloads
```

`values.CompareFloat64` — faithful to `java.lang.Double.compare`, the Record
Layer's ordering authority — instead collapses every NaN bit pattern to one
canonical value and ranks it GREATEST. Two independent defects follow:

1. **The column itself is misordered.** A negative NaN is the physically FIRST
   row and the logically LAST one. A scan hands back negative NaNs before
   `-Inf` where the comparator wants them after `+Inf`.

2. **Any LATER sort column is unordered too**, and this is why no range-set can
   repair it. All NaN payloads are ONE logical tie class, spread across TWO
   disjoint physical ranges at opposite ends of the key space. Within each
   range the following coordinate is ordered; across the tie class as a whole it
   is not. So even visiting both NaN blocks in the correct order would not fix
   the `, id` after the float.

Because of (2) a float coordinate cannot merely be *reordered* — it
**TERMINATES** the ordering claim. Everything before it stays claimable; the
float coordinate and everything after it does not.

Java is unsound in exactly this way. Go deliberately diverges; see
`DIVERGENCES.md`.

**Signed zero is NOT part of this.** `-0.0` packs immediately before `+0.0` and
`CompareFloat64` also ranks `-0.0` below `+0.0`, so the two orders agree — a
measured negative result, pinned by
`TestFDB_FloatOrderingClaim_Differential/negative_result_non_nan_edges_order_identically`
so that a wider divergence cannot appear unnoticed. (Signed zero *does* matter
for a float EQUALITY, which is a different question — see the scope note below.)

### The shape that proves it

`pkg/relational/sqldriver/float_ordering_claim_differential_fdb_test.go`.

It seeds a ladder covering every IEEE-754 edge class, with the two NaN rows
carrying DISTINCT payloads and adversarial ids (the negative NaN gets the
LARGER id, so the tie-break disagrees with the physical order rather than
agreeing with it by accident), then runs each shape as a differential: the
indexed table against an oracle table holding identical rows with NO index,
where the planner has no scan order to claim and must sort. The expected
logical order is asserted outright as well, because a differential alone passes
when both sides are wrong the same way. Every shape binds the index with an
equality on a leading INTEGER column and asserts via `EXPLAIN` that the index
scan is really in the plan — without that, both sides full-scan and the test
compares a baseline against a copy of itself.

Plan-shape assertions covering the same ground without a cluster:
`pkg/relational/core/embedded/float_ordering_claim_plan_test.go`.

### The mechanism that produced the wrong plan

Ordering claims were built from index column-name sequences with **no
consultation of column type at all**:

- the scan-side ordering producers in
  `pkg/recordlayer/query/plan/plans/ordering.go` (`PKScanOrdering`,
  `RecordQueryIndexPlan.HintOrdering`, and both `HintRichOrdering` forms) —
  and the two AGGREGATE producers,
  `RecordQueryStreamingAggregationPlan.HintOrdering` and
  `RecordQueryAggregateIndexPlan.HintOrdering`, which an earlier revision of
  this list omitted while claiming the four were all of them;
- both match candidates' `ComputeMatchedOrderingParts`
  (`match_candidate_index.go`, `primary_scan_match_candidate.go`);
- and the two sort-elision rules (`rule_ordered_index_scan.go`,
  `rule_ordered_primary_scan.go`), which name-matched sort keys against column
  names without consulting any ordering-capability machinery whatsoever.

A contributing factor made the defect invisible to any type-aware guard that
might have been added: the match candidate's row layout carried `UnknownType`
for every field, so a predicate asking "is this column a double?" could not
have gotten a true answer until `executor.PositionalTypeForDescriptor` was made
to carry declared field types.

The claim is now asked in one place —
`values.TypeTerminatesOrderingClaim` / `ColumnCanExtendOrderingClaim` /
`ClaimableOrderingPrefix` — and every producer routes through it, so no two
derivations can classify the same column differently.

### This is a live bug, not a theoretical one

NaN is reachable from ordinary SQL today. Plain `INSERT ... VALUES` and bound
parameters reject non-finite values at the proto-write boundary with `22023`
("cannot store NaN or Infinity"), but **`UPDATE` and `INSERT ... SELECT` do
not route through that guard**. The differential test seeds its NaN rows
through exactly that path — `UPDATE t SET e = (e * 10.0) + (e * -10.0)` — which
is how it can exist at all. So a user can put a NaN in a table with supported
SQL and then get wrong-ordered rows out of an ORDER BY, and with `LIMIT`, the
wrong rows entirely.

### Scope note: what was NOT retired

An **equality-bound** float column is not affected and keeps its claim. A
nonzero float equality pins ONE tuple-encoded key prefix; every row under it
carries identical float bits, so the coordinate is FIXED rather than sorted, it
claims no order of its own, and the primary-key suffix after it is genuinely
delivered in key order.

This distinction is load-bearing and was worth 30 additional scenarios. An
earlier revision of the fix terminated the claim on equality-bound floats too,
which drifted 57 scenarios instead of 27: 15 gained a spurious in-memory sort on
`ORDER BY id DESC`, and 15 lost an index intersection (an intersection needs a
common ordering from both legs, and a leg claiming nothing cannot supply one).
Those 30 were plan REGRESSIONS with correct rows, and blessing them would have
frozen the regression into the corpus as though intended. They are not in the
retirement above.

The one float equality that pins nothing is **zero**: `= 0.0` spans both signed
zeros, which are two distinct adjacent keys, so the scan covers two physical
prefixes and the suffix restarts at the boundary. That case still terminates the
claim. `plans.EqualityPinsSinglePhysicalKey` is the single authority separating
the two, consulted by both the plan-side and candidate-side derivations; the
pair is pinned by `TestEqualityBoundFloatDoesNotTerminateTheClaim` and
`TestZeroFloatEqualityStillTerminatesTheClaim`.

### Counts, and how they were established

Method: each committed scenario's candidate was regenerated from its header
recipe and its plan shape recomputed, with and without the change applied, and
the two dumps compared. With the change reverted, all 5000 committed digests
reproduce exactly — so the change is the sole cause of the drift.

| set | scenarios | plan points (distinct dedup keys) | files | candidates |
|---|---|---|---|---|
| retired here | 27 | 27 | 9 | 9 |

Two other figures circulated while this was in flight. "18 drifting files" was a
true measurement of the *earlier, over-broad* 57-scenario drift and is
superseded; it also shipped as a MEASURED row in `DIVERGENCES.md`'s scope table,
where it has now been corrected to 9 rather than left to be found here.
"36 scenarios across 12 plan points" could not be reproduced against either
state and is not adopted.

**Extending the termination to the two AGGREGATE producers retires NOTHING
further.** Re-derived from scratch after that change: 0 drifted scenarios of
5000. No committed factory scenario groups by a float, so the corpus never
exercised either aggregate producer — which is why their proof is an FDB
differential (`TestFDB_FloatOrderingClaim_Aggregate_Differential`) and not a
corpus entry. A corpus that would have caught them is a separate gap, recorded
here so it is not mistaken for a clean bill of health.

Rows were verified unchanged empirically, not by argument:
`//pkg/relational/conformance/factorycorpus/full:full_test` executes every
committed scenario against a real cluster and is green before and after. The
re-bless itself touched only the two derived header lines — 27 `plan-shape` and
27 `dedup-key` — and no row cell in any file.

### How the re-bless was done

`go run ./cmd/factory-rebless`, which re-derives the plan-shape and dedup-key
headers from each scenario's committed reproduction recipe, verifies every other
determinism invariant (candidate name, feature vector, all four TLP renderings,
schema template, setup INSERT) rather than rewriting it, refuses to run if any
of those drifted, checks for dedup-key collisions, and regenerates
`census_baseline.json`. No digest was hand-edited.
