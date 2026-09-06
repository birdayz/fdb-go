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

NaN is reachable from ordinary SQL today, through **every** write path. When
this was written that was not so: plain `INSERT ... VALUES` and bound parameters
rejected non-finite values at the proto-write boundary with `22023` ("cannot
store NaN or Infinity") while `UPDATE` and `INSERT ... SELECT` did not, and the
differential test seeded its NaN rows through the one path that worked. That
Go-only guard is gone — Java has no such rule anywhere, and a column that
accepted a value through one syntax and refused it through another was the
defect, not the protection. So a user can put a NaN in a table with supported
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
5000.

The zero is stronger than "no scenario groups by a float", and the reason
matters: `grep -ril "group by" testdata/ | wc -l` is **0** — not one of the 309
committed files contains GROUP BY at all. That is ARCHITECTURAL, not an
accident of sampling. The TLP partition oracle declines aggregates by
construction (`factory/candidate.go:101-109`: the output is one row per group,
not a row set the input partition maps onto — "an oracle that holds for some
functions is not an oracle"). So no factory batch will EVER cover the two
aggregation producers, and the aggregate proof lives in the FDB differential
(`TestFDB_FloatOrderingClaim_Aggregate_Differential`) permanently, not until
the corpus catches up. Recorded here so the zero is not mistaken for a clean
bill of health.

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

---

## Coveringness became a plan type — REPRESENTATION CHANGE, over a concealed regression

**Re-blessed: 4340 scenarios of 7000, across 238 of 373 family files. Result
rows: UNCHANGED.** Machine half:
`retirements/2026-08-09-rfc220-coveringness-is-a-plan-type.json`, base commit
`431eb79af6624b9f7081ba6bfb09e3557cfc10d6`.

### What moved, and why it is not a retirement

RFC-220 makes `RecordQueryCoveringIndexPlan` a plan type built at the access
path rather than a wrapper discovered afterwards. Two things follow in the
RENDERING of almost every plan that touches a secondary index: a Fetch wrapper
collapses wherever the merge rule can remove it, and a scan that serves its
columns from the index acquires a COVERING marker. The reproduction recipe, the
feature vector, the four TLP renderings, the schema, the setup and every frozen
row cell are unchanged — only the two derived header lines moved. That is why
this is a re-bless and not a retirement: no plan point ceased to exist, they are
described differently.

### The part that is not routine

The same drift concealed a real regression, and that is the reason this entry
exists at all.

183 scenarios lost a correlated EQUALITY index probe on the inner side of a
nested-loop join and got a full scan instead — O(N) per outer row. The rows were
identical, so no row-based check in the repo could see it, and the digest
movement was indistinguishable from the thousands of benign movements around it.
A re-bless taken at that point would have frozen the regression into the corpus
as though intended, and destroyed the only record that those plans had ever been
probes.

Root cause: `collectScanPlanCorrelations` (`planner.go:1442`) recursed through
`GetChildren()`, and criterion C1 makes the covering plan hold its index scan as
a non-memoized FIELD. The walker therefore returned empty correlations for a
covering probe, `compensationResidualCorrelationSafe` (`planner.go:1245`)
rejected the outer-correlated residual, the compensation reached `InsertFinal`
with no exploration task, `ImplementFilterRule` never fired, and the probe was
never constructed. Java is immune because it dispatches on the type:
`RecordQueryCoveringIndexPlan.getCorrelatedTo()` delegates to
`indexPlan.getCorrelatedTo()`.

That is fixed in the same change. The re-bless above is taken over the FIXED
planner.

### The instrument, which is the durable part

`cmd/factory-plan-census` was written for this and is committed alongside it. It
dumps one `<scenario>\t<plan>` line per committed scenario through the same
entry point the digest is derived from, and classifies a two-dump difference
into NAMED counts. It exits non-zero when a scenario loses an equality index
probe or becomes unplannable, and stays silent on Fetch-wrapper removal and
COVERING acquisition, which are representation. It refuses a verdict when either
dump is empty.

Measured over this exact transition — base dump taken in a tree at
`431eb79af6624b9f7081ba6bfb09e3557cfc10d6`, branch dump on the fixed planner,
both 7000 lines, both processes exit 0:

```
scenarios compared: 7000
plans moved:        4333

  lost an EQUALITY index probe:   0
  lost ALL index access:          0
  gained an UNBOUNDED full scan:  0
  newly unplannable:              0

no regression class present; the movement is representation-only
EXIT=0
```

Before the fix the same three counts read 183 / 177 / 75.

4333 here versus the 4340 the re-bless reports is not a discrepancy to
reconcile away: `classify` compares `Explain()` text and the corpus digests
`explaindiff.ShapeOf`, so seven scenarios have a shape the digest separates and
the rendered explain does not. The census is the more conservative of the two on
that margin, never the looser.

### How the re-bless was done

`go run ./cmd/factory-rebless-plan-shapes` with `-ledger`, which re-derives the
plan-shape and dedup-key headers from each scenario's committed reproduction
recipe, verifies every other determinism invariant rather than rewriting it,
refuses to run if any of those drifted, regenerates `census_baseline.json`, and
emits the machine ledger carrying both census endpoints, both corpus-tree
fingerprints and a per-file disposition. No digest was hand-edited.

## A sort is judged through its source group — PLAN CHANGE, an elision Java makes

**Re-blessed: 42 scenarios — 14 candidates × 3 projections — across 10 family
files, all `single|and(…)|none`. Result rows: UNCHANGED.** Machine half:
`retirements/2026-09-05-rfc242-a-sort-is-judged-through-its-source-group.json`,
base commit `36b97f1e91b5c88658e8d759668b981ffebfcb7d` (master's tip, which the
branch's corpus equalled byte for byte before this transition).

### What moved

Every one of the 42 is the same shape, counted over the two `factory-plan-census`
dumps (8060 lines each, both processes exit 0 — the corpus holds 8150 scenarios with a
`plan-shape` header, and the dump omits the candidates with fewer than two TLP renderings,
which is where the two populations differ):

```
InMemorySort([K DIR, ID DIR], PredicatesFilter(IndexScan(IDX_K, [=]), [n preds]))
```

became

```
PredicatesFilter(IndexScan(IDX_K, [=]), [n preds])
```

with the same projection above it where the candidate projects. The query is
`… WHERE k = <literal> AND … ORDER BY k <dir> [NULLS …], id`: the index probe
binds `k` by equality, and within one bound value the index stores entries in
primary-key order, so the requested order is already delivered and the sort is
redundant. This is Java's `RemoveSortRule` equality-bound arm (its
`equalityBoundKeys` loop), answering here for the first time on these shapes.

### Why it moved now, and why it is sound

RFC-242's third adjacent finding changed how a sort over an order-PRESERVING
wrapper is judged. `ImplementSortRule` compared the request against the
wrapper's own derived ordering, which inherits its source's PLAIN ordering — and
a plain ordering drops equality-bound coordinates, so a `PredicatesFilter` over
an equality probe on `IDX_K` advertised `[ID]` alone and `ORDER BY k, id` was
never satisfied. The rule now judges such a wrapper through its source group
(`memberSatisfiesOrdering`, the `orderingDelegator` walk), which sees the index
scan's RICH ordering: `K` bound, `ID` ascending under it. A bound `DOUBLE` key
is included only where the existing ordering-claim predicate already admits it
(`EqualityBoundCoordinateClaimsOwnOrder`; a zero-capable float equality does not
pin and the claim is truncated there as before), so this re-bless changes no
float-ordering verdict the earlier entry in this ledger retired.

The elision is order-correct because both plans read the same probe with the
same residual filter and differ only in the sort; the probe's entries are
`(k, pk)`-ordered by construction. The frozen rows are unchanged, and the full
committed corpus was executed against real FDB on the re-blessed planner in the
same pre-commit run that surfaced the drift
(`//pkg/relational/conformance/factorycorpus/full:full_test`).

Classification over the transition:

```
scenarios compared: 8060
plans moved:        42

  lost an EQUALITY index probe:   0
  lost ALL index access:          0
  gained an UNBOUNDED full scan:  0
  newly unplannable:              0

no regression class present; the movement is representation-only
EXIT=0
```

### How the re-bless was done

`go run ./cmd/factory-rebless-plan-shapes -corpus … -census …`, then the same
command with `-ledger`, `-before` (the corpus as master committed it, exported
with `git archive`), `-base-commit`, `-rfc RFC-242`, `-date 2026-09-05` and the
reason above. The tool re-derived the two header lines from each scenario's
committed reproduction recipe, verified the feature vector, the four renderings,
the schema and the setup unchanged, regenerated `census_baseline.json`, and
emitted the machine ledger. No digest was hand-edited.
