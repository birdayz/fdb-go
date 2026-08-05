# RFC-210 — A secondary UNIQUE index may prove DISTINCT elision

- **Status:** DRAFT — Graefe ACK **with conditions** (C1-C7, folded), then a
  second **binding ruling** on the reachability refutation (§10). Torvalds
  **NAK → resolved in text**: his blocker invalidated §5.1 clause 1 and produced
  §5.1.1. A second refutation showed the resulting arm could not fire on any SQL
  query at all; the ruling reformulated clause 8 as the **exempt-set proof** with
  three routes (§5.1.2 R1/R2/R3) and folded #617's dead strict-ordering half into
  this RFC (§5.7). One Graefe lap at completion of the whole arm.
- **Branch:** `rfc/secondary-unique-distinct-lift`
- **Area:** Cascades planner (`ImplementDistinctFinalRule`), index-state planning,
  plan cache
- **Depends on:** #612 (RFC-209's readable-index candidate filter), #616
  (execution-time index-state revalidation and the `proofOnly` dependency seam),
  #617 (the logical-distinctness proof over raw-NaN and nullable keys)
- **Wire footprint:** none. Read-side only; see §6.

## 1. Decision in one sentence

A secondary `UNIQUE` index becomes an admissible proof that a `DISTINCT` is
redundant — under an admission predicate that is a conjunction of facts the
planner can already state, whose last clause is a proof that the index's
**exempt set** (entries where `UNIQUE` constrains nothing, or constrains
something finer than logical row equality) is empty on this stream, discharged by
metadata (**R1**), by a NULL-rejecting predicate (**R2**), or — where neither
proves it — by *narrowing the operator to the exempt subset* rather than removing
it (**R3**) — and the index every route rests on is recorded as a **plan-carried
dependency**, so that an index whose state moves out from under a live statement
invalidates the plan with `40001` instead of silently returning duplicate rows.

Two things this RFC deliberately does *not* do. It does not widen the candidate
set: the proof is admissible **only** for an index that is already a match
candidate, which is already exactly the strictly-`READABLE` set (§4.1), so
`READABLE_UNIQUE_PENDING` is excluded structurally and no uniqueness-specific
state check is written anywhere. And it does not add a third derivation of "does
this storage key's uniqueness survive logical equality" — it makes the existing
two into one carried property (§5.5).

The measured benefit is narrower than the phrase "use the unique index" suggests,
and §2.1 leads with **two** refutations. The first: the elision removes an
operator, it does not change the access path. The second, which forced the
reformulation above: the fixture's own DDL put every measured query outside the
original metadata-only clause 8, so every figure in §2.1's first table measures
the *cost of the operator* rather than this RFC's benefit. §2.1's second table is
the delivered delta, and the acceptance criteria are written against it. The
value is concentrated in one regime — a paged, unordered
`SELECT DISTINCT <unique column>`, where the redundant dedup is ~68-72% of the
query's runtime.

## 2. What is declined today, and what it costs

`rule_implement_distinct_final.go`'s `distinctEliminatedByUniqueKey` proves
`DISTINCT` redundant from **primary-key** coverage only. Its closing paragraph
declines the secondary-`UNIQUE` case and — since #617 — records accurately that
both preconditions which originally forced the decline have closed:

> So the decline is now a CONSERVATIVE LEFTOVER, not a necessity […] Lifting it
> is a real behaviour change (it decides when a general-purpose rule fires) and
> needs its own RFC and review; it is deliberately NOT done here.

This RFC is that review.

### 2.1 The measurements, and the two things they refute

**The second refutation comes first, because it is what forced this RFC's
reformulation and because everything in the first table has to be read through
it.**

The fixture below declares `email STRING` — and the SQL DDL cannot declare a
`NOT NULL` scalar column at all. So the fixture's own DDL puts every row of the
first table on the far side of R1's admission predicate: the metadata arm
(§5.1.2 R1) was **inapplicable to the very query being measured**, and no
arrangement of the fixture could have made it applicable while staying in SQL.
The consequence is exact and it is not a caveat, it is a reclassification:

> **Every figure in the first table measures the COST OF THE OPERATOR. None of
> them measures this RFC's benefit**, because under the RFC as originally drafted
> the optimization could not fire on any of these queries.

That is the second refutation this section leads with (the first, below, is the
access-path one). It is what turned clause 8 from a metadata check into the
exempt-set proof of §5.1.2, and it is why there are now two tables: one for what
the operator costs, and one — new, and the one the acceptance criteria are
written against — for what this RFC actually delivers.

Real FDB via `TestFDB_DistinctUniqueElisionCostProbe`
(`pkg/relational/sqldriver/distinct_unique_elision_cost_probe_test.go`).
Fixture: `users(id BIGINT, email STRING, payload STRING, PRIMARY KEY (id))` with
`CREATE UNIQUE INDEX by_email ON users (email)`, **100 000 rows, every email
distinct** — so the `DISTINCT` provably removes nothing and every microsecond it
spends is waste. Each figure is a median over repeated runs on one store,
reproduced across two full end-to-end runs.

**The plans, verbatim from `EXPLAIN`:**

| query | plan |
|---|---|
| `SELECT DISTINCT email FROM users` | `Distinct(Project([EMAIL#1], Scan(USERS)))` |
| `SELECT email FROM users` | `Project([EMAIL#1], Scan(USERS))` |
| `SELECT DISTINCT email FROM users ORDER BY email` | `Distinct(Project([EMAIL#1], IndexScan(BY_EMAIL, [*])))` |
| `SELECT email FROM users ORDER BY email` | `Project([EMAIL#1], IndexScan(BY_EMAIL, [*]))` |
| `SELECT DISTINCT email FROM users LIMIT 10` | `Limit(10, Distinct(Project([EMAIL#1], Scan(USERS))))` |
| `SELECT email FROM users LIMIT 10` | `Project([EMAIL#1], Limit(10, Scan(USERS)))` |

**The first thing the measurement establishes is a negative, and it is stated
first because an earlier framing of this RFC got it wrong.** The elision does
**not** unlock an index-only plan. `SELECT email FROM users` — the exact shape the
elision produces — plans as a base-record full scan, the same access path the
`DISTINCT` variant already uses. The transformation is precisely
`Distinct(Project(Scan))` → `Project(Scan)` and nothing else, in every shape
measured. Any motivation of the form "the unique index answers the query
directly" is false here and must not appear in the argument.

It would in fact be a *regression* to make it true: when `ORDER BY email` forces
the `BY_EMAIL` path, the query takes **6.95 s** against **0.33 s** for the base
scan — a 15x-21x gap at 100k rows across boxes, because the index path fetches a base record per
entry. The cost model's preference for the base scan is empirically right. (That
gap is a real and separate finding about the index access path; it is not this
RFC's subject and no claim here rests on it.)

**What the decline actually costs**, then, is one redundant operator — and the
operator is more expensive than its CPU suggests. **This is the cost-of-the-
operator table**, in the sense established above: an upper bound on what any
route could recover, not a measurement of what any route does recover.

| shape | rows | with `DISTINCT` | without | delta |
|---|---|---|---|---|
| unordered, single page | 100 000 | 388 ms | 333 ms | **+55 ms (+17%)** |
| unordered, **paged** (`EXECUTION_SCANNED_ROWS_LIMIT=10000`, ~10 pages) | 100 000 | **559 ms** | **328 ms** | **+235 ms (≥+68%; see band note)** |
| paged allocation churn | 100 000 | **424.9 MiB** | 157.7 MiB | **+267 MiB** |
| ordered (streaming variant) | 100 000 | 6.95 s | 7.05 s | **noise-dominated at this scale** |
| `ORDER BY … LIMIT 10` | 10 | 6 ms | 6 ms | 0 |
| `LIMIT 10`, unordered | 10 | 5 ms | 5 ms | 0 |

**The paged row is the real number, and the reason is structural rather than
arithmetic.** The unordered shape gets the **hash-set** executor
(`RecordQueryDistinctPlan.Streaming == false`, read off the planned plan —
`EXPLAIN` does not render it). `executeHashDistinct` serializes **every seen key**
into `DistinctHashContinuation` on each page and rebuilds the set from it on
resume, so across P pages the seen-key set is copied O(P²) times in total. The
measured +267 MiB of churn at ten pages is that. The paged no-`DISTINCT` run
costs the same as the unpaged one (328 vs 333 ms), so pagination itself is free
here and the entire delta is the operator.

**Band note — the figures above are one box, and a third data point widened
them.** An independent reproduction measured the paged delta at **+88%**. So the
paged cost is quoted as **≥+68%** rather than as a point estimate or a narrow
band, and every downstream criterion (§7.5) is written against the lower edge.
Likewise the paged ratio is **1.68x-1.88x**, not "1.72x", and §2.1's
index-path penalty is **15x-21x** (the reproduction measured 15.3x), not "21x".
None of the arguments here turn on the width of those bands, which is why the
conservative edge is used throughout.

**And the honest negatives, all measured:** the `DISTINCT` does not block a
covering-index-only plan (there is none for either query), does not force a
base-record fetch the other shape avoids, and does not defeat early termination —
`Limit(10, Distinct(…))` and `Project(Limit(10, …))` both return in 5-6 ms over
100k rows because the cursors are pipelined.

**The ordered shape is noise-dominated at this scale — which is a weaker claim
than "no difference", and the weaker claim is the true one.** The reproduction's
spreads were `[9.2-15.3 s]` with `DISTINCT` and `[7.3-12.6 s]` without: heavily
overlapping, so no delta is resolvable, but the earlier "below noise (±300 ms)"
overstated the precision by an order of magnitude. Nothing here establishes that
the streaming variant is free — only that this fixture cannot measure it. §7.5
therefore sets no timing criterion on that shape, and stating why matters: an
unmeasurable delta is not a measured zero.

So the case for lifting is narrower than it first looks and rests on one regime:
**a paged, unordered `SELECT DISTINCT <unique-column>`, where the redundant
operator costs ~68-72% of the query's runtime and an allocation load that grows with
the number of distinct values.** That regime is the ordinary one for a
result-set-paginating client over a unique column, which is exactly the query a
`UNIQUE` index invites.

**And that regime is an upper bound on the recoverable cost, not the delivered
one.** What is delivered depends on which route of §5.1.2 fires, which depends on
the query. Hence the second table.

#### The delivered delta — what each route actually recovers

Three runs on the §2.1 fixture, same store, back-to-back, median of 5 pairs, in
the paged unordered regime the criteria are written for (100k rows,
`EXECUTION_SCANNED_ROWS_LIMIT=10000`):

| # | query | route | operator after | measured |
|---|---|---|---|---|
| A | `SELECT email FROM users` | — (baseline, no `DISTINCT` written) | none | ⟨M-A⟩ |
| B | `SELECT DISTINCT email FROM users` | **R3** — nullable key, no NULL-rejecting filter | narrowed distinct | ⟨M-B⟩ |
| C | `SELECT DISTINCT email FROM users WHERE email IS NOT NULL` | **R2** — full elision | none | ⟨M-C⟩ |
| D | `SELECT DISTINCT email FROM users` on today's `master` | — | full distinct | ⟨M-D⟩ |

The comparisons that matter, and what each would mean:

- **B vs D** is R3's delivered benefit on the bare query — the shape a user
  writes without thinking about NULLs, and the shape that could not be helped at
  all under the RFC as originally drafted.
- **C vs A** is R2's delivered benefit: it should be *indistinguishable*, because
  under R2 the plans are the same plan modulo the filter.
- **B vs A** is the residual R3 leaves on the table, and is the honest measure of
  how much R2's reachability matters.

> **MEASUREMENT PLACEHOLDER.** ⟨M-A⟩…⟨M-D⟩ and the sweep below are filled by the
> §7.5 probe rewrite against the delivered implementation, on the same box, in
> one run. They are deliberately **not** carried over from the cost-of-the-
> operator table: that table's numbers are a bound on B–D, not a substitute for
> it. This RFC is not complete while these read ⟨M-x⟩.

#### The NULL-density sweep — R3's dominance as a measurement

R3's claim is that the narrowed seen-set is a subset of the full one on **every**
input, degenerating to empty on an ordinary table. That is an argument in §5.1.2;
this is the measurement that makes it a fact. Same fixture, same paged unordered
shape, varying only the fraction of rows whose `email` is NULL:

| NULL density | rows retained by full distinct | retained by R3 | runtime (full) | runtime (R3) | churn (full) | churn (R3) |
|---|---|---|---|---|---|---|
| 0% | 100 000 | 0 | ⟨S-0⟩ | ⟨S-0'⟩ | ⟨S-0c⟩ | ⟨S-0c'⟩ |
| 1% | 100 000 | 1 000 | ⟨S-1⟩ | ⟨S-1'⟩ | ⟨S-1c⟩ | ⟨S-1c'⟩ |
| 50% | 100 000 | 50 000 | ⟨S-50⟩ | ⟨S-50'⟩ | ⟨S-50c⟩ | ⟨S-50c'⟩ |

The retained-row columns are the structural claim and are asserted exactly (they
are counts, not timings, so no band is needed). The 50% row is the near-worst
case and its acceptance criterion is **"no worse than full"**, not "faster" —
which is the whole content of *strictly dominates*: R3 may not be allowed to be
slower anywhere, and it is not required to be faster everywhere.

Note the sweep also has to hold the **row results** constant across the three
densities: `SELECT DISTINCT email` over 50% NULLs must return one NULL row
however the operator was narrowed. That is the correctness half and it is asserted
alongside the counts.

## 3. Java, read first — Java never reaches the decision at all

**Java's Cascades planner cannot plan `SELECT DISTINCT`.** That is the first
fact and it governs everything else in this section, because it settles what
category of change this RFC is before any of the finer reading matters. Every
`DISTINCT` entry in the cross-engine corpus is marked
`DivergenceJavaErrorsGoCorrect` — Java throws `UnableToPlanException`, Go
answers — and the corpus says so in the section header
(`pkg/relational/conformance/plandiff/corpus.go:15548-15549`: *"Java's Cascades
planner can't plan DISTINCT (TODO #42)"*), with the same finding recorded at
`:289-292` where the shape was first deferred out of the shared corpus.

So this RFC is **not** parity work and cannot be. There is no Java behaviour to
match on this query shape: the operator Java would have to elide is one Java
never builds, because it never plans the query that would contain it. The whole
of RFC-210 lives in the **sanctioned read-side-extension lane** — the
"query reach is not the hard line" half of the project's two axes. Wire compat is
untouched (§6), and the extension only lets Go *express and optimize* a query
Java declines. The conformance principle *doesn't work in Java → doesn't work in
Go* governs the **shared** surface, and `SELECT DISTINCT` is not on it.

The rest of this section is therefore about a mechanism Java *has* rather than a
decision Java *makes*, and it is retained for one reason: the fact this RFC
proves is one Java also derives, and the hazards Java hit deriving it (§5.5's
prefixing warning) are inherited whether or not Java ever consults it.

Within `fdb-record-layer-core`, whose `ImplementDistinctRule` does run on plans
built by the core planner's own API, Java never uses a secondary `UNIQUE` index
to eliminate a `DISTINCT`. It does, however, *derive* uniqueness-based
distinctness and carry it in the ordering property — so the accurate finding
about the core layer is not "Java lacks the concept" but "Java computes the
concept and no path connects it to this decision". The distinction is drawn
precisely below.

**What licenses eliminating a distinct in Java.** `ImplementDistinctRule` reads
exactly one thing, `DistinctRecordsProperty`:

```java
if (innerPlanPartition.getPartitionPropertyValue(DistinctRecordsProperty.distinctRecords())) {
    call.yieldPlans(innerPlanPartition.getPlans());   // absorb the distinct
} else {
    call.yieldPlan(new RecordQueryUnorderedPrimaryKeyDistinctPlan(...));
}
```

and that property's index case reduces to fan-out, never to the `unique` flag
(`DistinctRecordsProperty.java:272`, `ValueIndexScanMatchCandidate.java:212`):

```java
return !matchCandidate.createsDuplicates();
// createsDuplicates() { return index.getRootExpression().createsDuplicates(); }
```

`KeyExpression.createsDuplicates()` is repeated-field fan-out. So Java's
"distinct" is **record** distinctness — one row per primary key — and a
non-fan-out index scan qualifies because each record contributes exactly one
entry, not because the index is unique. `ImplementUniqueRule` is the same
absorption with no fallback plan.

**Every `isUnique()` call site under `query/plan`**, and what each licenses:

| Site | Licenses |
|---|---|
| `cascades/rules/RemoveSortRule.java:153` | strict sort order |
| `cascades/properties/CardinalitiesProperty.java:339` | max cardinality 1 under a full equality bind |
| `plans/RecordQueryIndexPlan.java:454` | `maxCardinality()` = 1 |
| `RecordQueryPlanner.java:1570, 2815, 3020` | heuristic planner's `strictlySorted` flag |
| `{Value,Windowed,Vector,Aggregate}IndexScanMatchCandidate` | plain delegation to `index.isUnique()` |
| `PrimaryScanMatchCandidate.java:181` | `return true` — the PK is unique by definition |
| `IndexScanParameters.java:74`, `IndexScanComparisons.java:118`, `MultidimensionalIndexScanComparisons.java:114`, `VectorIndexScanComparisons.java:125` | the `isUnique(Index)` overload family — plumbing that forwards the flag to the sites above, licensing nothing on its own |

Distinctness is absent from that list. The place the proof would have to live is
`DistinctRecordsProperty.DistinctRecordsVisitor.visitIndexPlan`, and that method
consults `createsDuplicates()` and nothing else.

**The one chain that looks like it connects — and the precise way it stops.** An
earlier draft called this chain "dead", which is wrong in a way worth correcting,
because the accurate version is the stronger position.

The **derived** side is alive. `RemoveSortRule` sets `strictlySorted` from
`isUnique()`; `OrderingProperty.java:363-366` feeds that flag into the ordering
via `computeOrderingFromScanComparisons(..., indexPlan.isStrictlySorted())`, so
`Ordering.isDistinct` genuinely carries a uniqueness-derived fact. It is load
bearing: `Ordering.java:1134` guards ordering concatenation with a hard
`Verify.verify(leftOrdering.isDistinct())`, and `OrderingProperty.java:596-604` (a different file — the property, not the
value type) uses it in the flatMap concat case. Java *does* derive distinctness from a unique index.

What is dead is the **requested** side. `Ordering.satisfies` consults
`requestedOrdering.isDistinct()` at four sites, but
`RequestedOrdering.Distinctness.DISTINCT` is never constructed anywhere in
`src/main` — every producer uses `PRESERVE_DISTINCTNESS`, and `RemoveSortRule`
explicitly downgrades to it at line 106.

So the accurate statement is sharper than "Java lacks the concept": **Java derives
the concept and never lets it reach the decision.** The derived distinctness
exists, flows, and is verified — and no path connects it to eliminating a
`DISTINCT` operator. This RFC connects a fact Java computes to a decision Java
never permits it to make. That is a smaller and better-founded step than
inventing the fact, and it is also why §5.5's prefixing hazard is inherited
rather than novel: the flag Java derives is the one its own javadoc warns must
not be propagated across coordinate-set changes.

Java's own view of the rule this RFC modifies is worth recording, because it
concedes the structural point from the other side
(`ImplementDistinctRule.java:52-57`):

```java
 * <p>
 * This rule is somewhat suspect. In particular, if the inner plan that it matches against does not produce duplicates,
 * this rule will then return that plan. This is fine unless the plan is later modified in such a way that it then
 * <em>can</em> produce duplicates. … To address that, the plan is to add a mechanism for enforcing properties (e.g.,
 * distinctness or sort order) on the plans produced by the planner. See Issue #635.
 * </p>
```

(The Java source's own link is inconsistent — the `href` points at
`fdb-record-layer/issues/635` while the anchor text reads "Issue #653". Cited as
**#635**, the URL, since that is the machine-checkable half; noted so a reader
chasing the reference is not left wondering which is the typo.)

Java describes eliding a distinct on an unenforced property as *suspect*, and the
fix it names — a mechanism that makes the plan carry the property it was elided
under — is structurally what §5.2's plan-carried stamp does for the index-state
dimension.

**Conformance position — restated now the finer reading is in.** An earlier draft
argued this RFC was conformant because `SELECT DISTINCT email FROM users` "works
in Java and in Go and returns the same rows". That sentence is **false**, and its
falsity strengthens rather than weakens the position. Java's Cascades planner
errors on the query; there is no Java answer to agree with. The conformance
principle governs the shared query surface, and this shape is not on it, so the
principle does not reach this RFC in either direction.

What remains true, and is what actually matters, is the wire axis: the rows a
Java application reads back out of the store are byte-identical before and after
(§6), because nothing here touches what is written. This RFC adds no syntax and
no operator semantics; it changes which physical plan Go picks for a query only
Go can plan.

It is worth being explicit that Go's **existing** primary-key arm sits in exactly
the same lane for exactly the same reason, and has since it shipped. This is a
second arm on an extension Go already ships and tests, not a new class of one.

## 4. Why the decline is no longer forced

The decline rested on two gaps. Both closed, in the two commits immediately
preceding this RFC.

### 4.1 Candidate filtering is now Java-strict (#612 / RFC-209)

The hazard the decline existed for is `READABLE_UNIQUE_PENDING`: a unique index
whose build completed over violating data is fully populated, scannable, and
carries a `unique` flag the data contradicts. `IndexState.java:59-66` says so
outright — safe to consider `READABLE` for queries *as long as uniqueness is not
assumed*.

Java never assumes it, and — this is the load-bearing part — **not by checking
for that state**. A repo-wide grep of `READABLE_UNIQUE_PENDING` over Java's
`src/main` returns zero hits under `query/plan`. The safety is structural:

```java
// MetaDataPlanContext.java:96-103
private static List<Index> readableOf(RecordStoreState recordStoreState, List<Index> indexes) {
    if (recordStoreState.allIndexesReadable()) { return indexes; }
    return indexes.stream().filter(recordStoreState::isReadable).collect(Collectors.toList());
}
// RecordStoreState.java:172-174
public boolean isReadable(String indexName) { return getState(indexName).equals(IndexState.READABLE); }
```

Exact equality with `READABLE`. A `READABLE_UNIQUE_PENDING` index is never
offered as a `MatchCandidate` at all, so every Java site that trusts
`MatchCandidate.isUnique()` is safe without asking.

Go now has the same filter in the same position — on the index list, before any
candidate is created (`cascades_generator.go:2548`, the port of
`MetaDataPlanContext.forRootReference`'s
`indexList.removeIf(index -> !allowedIndexes.contains(index.getName()))`), fed by
`readableIndexesFrom(md, indexStateSnapshot)` over a snapshot taken from the same
store open that planning uses. `ReadableIndexes` models Java's
`Optional<Set<String>>` faithfully, including the distinction between an absent
allow-list and an empty one.

> **But only on two call sites.** Java's filter is reached by *every* planning
> path; Go's is fed by `cascades_generator.go:350` and `:1086` alone, and every
> other entry point leaves the zero value, which permits everything. The
> faithfulness above is about the filter's *shape and position*, not its
> *coverage* — and the difference is load-bearing enough to have produced
> duplicate rows in review. §5.1.1 is the correction; read it before relying on
> anything in this subsection.

**So this RFC writes no state check.** Admissibility is "the planning run
established state, and the index is a match candidate" (§5.1.1); the state filter
is what makes the second half mean strictly `READABLE` on the paths where the
first half holds. A
bolted-on `if state == READABLE_UNIQUE_PENDING { refuse }` at the proof site
would be exactly the special-case check design principle 10 forbids: it would
diverge the moment the candidate-set structure changes, and it would be a second
authority on a question that already has one.

### 4.2 Execution-time revalidation exists, and covers the case a leaf check cannot (#616)

Planning-time filtering is not sufficient on its own, because the state can move
between planning and execution — and, crucially, between one page of a
continuation and the next. #616 ported Java's
`DatabaseObjectDependenciesPredicate`: every execution re-reads the live store
and fails the statement when a depended-on index has disappeared, changed
`lastModifiedVersion`, or is no longer strictly readable.

The reason this is the right mechanism *for a uniqueness proof specifically* is
stated in `index_state_planning.go` and is worth repeating because it is the
whole argument for why an executor-side backstop could never have substituted:

> an index whose METADATA PROPERTY licensed a transformation is a dependency even
> when no leaf scans it. A uniqueness proof that elides a DISTINCT is the
> motivating case — the resulting plan reads base records and names the index
> nowhere, so a scan-only set would miss precisely the dependency that can
> produce WRONG ROWS rather than an error.

#616 built the seam for it (`collectPlanIndexDependencies`'s fourth parameter,
`proofOnly []string`), passed `nil` at all three call sites, and pinned the
emptiness with a test rather than a comment:
`TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof` is what fails
if the decline is lifted without the dependency being recorded.

This RFC's §5.2 is that recording — and it changes where the recording lives, for
a reason #616 could not have seen without a producer to try it against.

## 5. Design

### 5.1 The admission predicate

A secondary `UNIQUE` index `I` on record type `R` proves that `DISTINCT` over a
projection `P` is redundant iff **all** of the following hold. Each clause names
the fact the planner already states and the failure it prevents; the predicate is
a conjunction, and every clause fails closed.

1. **The planning run affirmatively established index state, AND `I` is a match
   candidate for `R`.** Both halves are required. The second half alone was this
   RFC's original clause 1 and it is **false as a proof of readability** — see
   §5.1.1, which is the correction that turned a NAK into this text.

   **The operational route is part of the clause, not an implementation
   detail.** The rule asks `call.Context.GetMatchCandidates()` and nothing else.
   That list is the one built in `cascades_generator.go:2510-2560`, where the
   `ReadableIndexes.Allows` filter (`:2548`) has already removed every
   non-`READABLE` index *before any candidate is created*. Stating the route
   matters because an unstated one is exactly where a second, unfiltered
   authority gets invented — an implementation that reaches for
   `md.GetAllIndexes()` or `store.GetReadableIndexes()` (which, by its own
   doc at `store_api.go:33-35`, **includes** `READABLE_UNIQUE_PENDING`) would
   satisfy every word of this clause and reintroduce the exact bug the decline
   existed for.

2. **`I.IsUnique()`.** The declared intent.

3. **`I` preserves base-record cardinality**, via
   `candidatePreservesBaseRecordCardinality` (`plan_context.go:255-286`) — not
   via `createsDuplicates` directly. The distinction is load-bearing:
   `createsDuplicates` answers only the fan-out question, while the shared gate
   additionally rejects a candidate whose traversal was refused
   (`!canProduceScanPlan()` — scalar nesting, unsupported `FAN_OUT` shape,
   inconsistent metadata) and a candidate carrying a stored predicate (clause 4).
   Using the narrow signal where the codebase already has the affirmative one is
   how a second admission authority is born.

   Prevents (the fan-out part): `UNIQUE` on a fan-out index constrains index
   *entries*, and a record whose repeated field is empty produces no entry at
   all, so the key says nothing about base-row distinctness. This includes the
   nested-under-a-repeated-parent case that a leaf's own `FanType` does not
   reveal.

4. **`I` is not SPARSE** — `predicateProto == nil`. This is enforced inside
   clause 3's gate, and it is called out as its own numbered clause because it
   is the one failure whose consequence is *wrong rows* and whose cause is
   invisible in the index's key definition.

   A sparse (`WHERE`-filtered) index omits every record its stored predicate
   rejects (`RecordMetaDataProto.Index.predicate`, via the maintainer's
   `shouldIndexThisRecord` gate). Its `UNIQUE` declaration therefore constrains
   **only the rows the predicate admits** — it says nothing whatever about the
   rows it excludes, which may hold arbitrarily many duplicates of an admitted
   value. Eliding a `DISTINCT` on that proof emits those duplicates. The failure
   is not hypothetical in this codebase: the same gate's comment records that
   `boolean-ddl.yamsql`'s `WHERE NULL` index is an *empty* index, and that
   `OrderedIndexScanRule` once served it as the whole table.

5. **`I`'s key columns are the projected columns themselves**, not a derived
   value: no function-keyed component (`CARDINALITY(...)` and friends).
   Prevents: a unique index over `f(col)` says nothing about `col`.

6. **Every key column of `I` is projected as a BARE, top-level `FieldValue`**,
   resolved to an ordinal in the scan row's layout (RFC-197's boundary rule —
   resolve names once, compare ordinals). Prevents two distinct failures: a
   many-to-one projection (`f(pk)`) being credited as injective, and a
   same-named column from a *different* source being credited as covering this
   one's key.

7. **The stream carries exactly one record type.** Prevents: `A`'s and `B`'s
   unique keys colliding once a shared visible coordinate is projected — the
   same restriction #617 put on the primary-key arm.

8. **`I`'s EXEMPT SET is empty on this stream** — or, under R3, is dedup'd
   rather than proved away. This clause is not a metadata question and the
   original draft's version of it was a category error; §5.1.2 is the clause,
   and it is long enough to be its own subsection.

Clauses 3 and 5-7 are already implemented and pinned — they are what
`TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof`'s `TAGS`,
`SCORE`, `CITY` arms and `TestDistinctFinal_MultiTypeVisiblePrimaryKeyDoesNotEliminate`
assert, each for a reason that survives independently of this RFC. Clause 4 is
**not** pinned anywhere today, which is why §7.2 gains an end-to-end row for it.
That test's `EMAIL` arm — a plain scalar unique index — is the one whose
expectation inverts.

**Filters and predicates above the scan are transparent.** A filter removes rows;
it cannot create a duplicate of a key that was unique. The existing walk through
transparent logical operators (`findScanExpression` / `findRecordTypes`) is
unchanged.

**What is not in scope.** Joins, unions, and aggregations are untouched: the arm
is stated over a single stored-record source, exactly like the primary-key arm
beside it. A `UNIQUE` index cannot prove distinctness of a join output, and this
RFC makes no claim there.

### 5.1.1 "Match candidate ⟹ strictly READABLE" is FALSE, and the fix is a third state

**The defect.** §4.1's reduction — an index is a candidate, therefore the store
said it was `READABLE` — holds on exactly **two** call sites:
`cascades_generator.go:350` and `:1086`, the SELECT and DML paths that call
`readableIndexesFrom(md, indexStateSnapshot)`. Every other planning entry point
leaves `PlannerConfiguration.ReadableIndexes` at its **zero value**, which
`AllIndexesReadable()` defines as "no restriction" — so every index in the
metadata becomes a candidate, whatever the store thinks.

This was demonstrated, not deduced: clauses 1-3 implemented as originally
specified return **duplicate rows** on
`TestFDB_UniquePendingIndexDoesNotEliminateDistinct` (3 rows `[7 7 9]`, want 2).
That test plans through `PlanRecordQueryWithMetadata`
(`unique_pending_index_distinct_fdb_test.go:144`) — the metadata-only harness —
and its own doc comment describes this failure mode verbatim
(`plan_harness.go:453-459`):

> it has no record store from which to load mutable index state. A caller that
> executes the result against a real store must independently establish that
> EVERY secondary index in md is strictly READABLE … Otherwise **a metadata-only
> UNIQUE proof can survive even when the final plan contains no index leaf for
> the executor to reject.**

§5.2 noticed this API for the plan-cache dimension and missed it for the
index-state dimension — the same API, the same paragraph, one dimension read.

**The decision, and why the obvious form of it is wrong.** The steer was to gate
on a restricted allow-list being *present*. Taken literally that is broken, and
the reason is worth stating so nobody re-proposes it: `readableIndexesFrom`
returns `AllIndexesReadable()` — the **unrestricted** form — precisely when the
store is healthy and every index is readable (`cascades_generator.go:2392-2400`).
Gating on `IsRestricted()` would disable the optimization on every healthy store
and enable it only while some *other* index was mid-build. Exactly backwards.

The real problem is that `ReadableIndexes` is two-valued where the question has
three answers. Java's `Optional.empty()` means *"I checked, and everything is
readable"* — an affirmative assertion derived from `RecordStoreState`. Go's zero
value means *"nobody ever asked"*. Those are opposite epistemic states rendered
identically, and the elision needs to distinguish them.

**So: `ReadableIndexes` gains a third state, and the zero value becomes the
unknown one.**

| state | constructor | means | may back a scan? | may prove uniqueness? |
|---|---|---|---|---|
| unknown | zero value (`IndexStatesUnknown()`) | nobody consulted the store | **yes** (unchanged) | **no** |
| all-readable | `AllIndexesReadable()` | consulted; every index strictly `READABLE` | yes | yes |
| restricted | `OnlyReadableIndexes(names)` | consulted; only these | only these | only these |

Three consequences, each deliberate:

- **The zero value stays permissive for scanning.** Not one existing plan
  changes, on any harness, offline tool, or unit test. The zero value is
  demoted only as *evidence*, never as *permission* — which is the whole point:
  a path that never asked about index state is not thereby forbidden from
  planning, it is forbidden from proving.
- **`readableIndexesFrom`'s degenerate early returns must mint UNKNOWN, not
  all-readable.** Today `states == nil || len(states) == 0` returns
  `AllIndexesReadable()` (`:2385-2387`) — an affirmative claim manufactured from
  the absence of information, which is the same bug one layer up. Only the path
  that fetched a snapshot and found every named index `READABLE` may mint the
  affirmative form.
- **The plan-cache key must separate them.** `cacheKeyPart` emits nothing for an
  unrestricted allow-list (`planner_options.go:136-138`); it must now emit a
  marker distinguishing unknown from asserted-all-readable, because the two can
  now produce different plans for the same SQL. The restricted case already
  keys.

This is the fail-closed shape: an admission predicate that requires **affirmative
evidence** rather than the **absence of contrary evidence**. Every unstated path
— present and future, including ones nobody has written yet — is safe by
construction, rather than safe by everyone remembering to thread a config field.

**Both witnesses are kept, and they assert different things.** The
harness-path test stays **byte-for-byte** and must still return 2 rows: under
this design it passes because the harness path is `unknown` and never elides —
so it is the pin on the fail-closed default itself. The SQL-path witness is
added because a metadata-only harness cannot exercise the mechanism the
production path actually relies on. See §7.3.

### 5.1.2 Clause 8 is a fact about the STREAM, not about the catalog — the exempt set

**The category error, stated first because it is what the refutation exposed.**
Clause 8 as originally drafted asked `SecondaryUniqueKeyGloballyEnforced` over
the index's declared key component types: *is every component `NOT NULL`, and is
none of them `FLOAT`/`DOUBLE`?* That is a question about the **catalog**. The
question the rule actually has to answer is about the **stream**: *are the rows
arriving at this `DISTINCT` already duplicate-free?* The two coincide only in the
degenerate case, and the divergence is not a nicety — it is the entire reason the
arm was unreachable. The SQL layer cannot express a `NOT NULL` scalar column
(deliberately, for Java parity; `NOT NULL` is accepted only on `ARRAY` types), so
**every SQL-expressible secondary unique index has a nullable key** and the
catalog question is false for all of them, forever. A predicate that can only
ever return false on the surface it was written for is not conservative; it is
inert.

**The exempt set.** A `UNIQUE` index constrains its entries — but not uniformly.
There is a set of index entries on which the declaration constrains *nothing*, or
constrains something *finer* than logical row equality. Call it the **exempt
set**:

- **NULL key components.** Under `NULLS DISTINCT` the uniqueness check is
  **skipped** when an indexed component is NULL. One NULL prefix legitimately
  holds arbitrarily many entries, distinguished only by their appended primary
  key. `UNIQUE` says nothing at all about these rows.
- **NaN key components.** FDB preserves distinct raw NaN sign and payload
  encodings, so `0xfff8000000000001` and `0x7ff8000000000001` are two tuple keys
  the index happily holds — while the dedup key canonicalizes every NaN to one
  value (`values.CompareFloat64`, faithful to `java.lang.Double.compare`).
  `UNIQUE` constrains these rows, but at a **finer** grain than the equality
  `DISTINCT` uses, so its guarantee does not transfer.

Everything outside the exempt set is genuinely one-row-per-value. The proof
obligation is therefore exactly:

> **The exempt set is empty on this stream.**

Three routes discharge it, in strength order. They are not alternatives to choose
between — R1 and R2 both prove the obligation and yield full elision; R3 declines
to prove it and instead makes the residual cheap. All three are implemented.

#### R1 — metadata (today's clause 8, unchanged)

`SecondaryUniqueKeyGloballyEnforced` over the authoritative physical key
component types (§5.4). Every component `NOT NULL` and none `FLOAT`/`DOUBLE`/
unknown ⟹ the exempt set is empty **for every possible stream** over this index,
which is the strongest form of the fact and the cheapest to check.

**R1 stays, unrelaxed, and stays vacuous on SQL.** It is kept because it is
correct and free: the record-layer API can build a `NOT NULL` scalar key that SQL
cannot, so R1 is the arm that fires for a direct-API caller. Its vacuity on the
SQL surface is a *measured property of the SQL DDL*, not a defect in the
predicate — which is why the refutation test is rewritten to pin exactly that
(condition 6, §7.6) rather than deleted. An implementation that finds itself
relaxing `SecondaryUniqueKeyGloballyEnforced` to make an arm fire has the wrong
arm, not a too-strict predicate; R2 and R3 exist precisely so that relaxing it is
never the move.

#### R2 — predicate: every key column carries a NULL-rejecting conjunct

If a filter strictly between the scan and the `DISTINCT` rejects NULL on **every**
key column of `I`, then no row reaching the `DISTINCT` can have a NULL key
component — the exempt set is empty **on this stream** even though it is
non-empty in general. Full elision, exactly as under R1.

**This is built on the range-enclosure encoding, and that is a citation rather
than an invention.** Java already encodes "rejects NULL" as a range that excludes
the NULL boundary: `RangeConstraints.java:650-653` compiles
`Comparisons.Type.NOT_NULL` — and `GREATER_THAN`, in the same `case`
fall-through — to `Range.greaterThan(boundary)` over the NULL boundary. NULL
sorts below every value in the tuple encoding, so *any* comparison whose compiled
range is strictly above the NULL boundary rejects NULL, and Java's own switch is
the enumeration of which comparison kinds those are. Go ports the same closed
enumeration at `predicates/range_constraints.go:161-173`
(`canBeUsedInScanPrefix`). **R2 introduces no new nullability lattice**; it reads
the encoding both engines already have.

**The allow-list, failing closed.** A comparison kind admits a column only if it
appears here:

| admitted | why it rejects NULL |
|---|---|
| `IsNotNull` | the direct form; Java's `NOT_NULL` → `Range.greaterThan(NULL)` |
| `Equals` | `Range.singleton(v)`, `v` non-NULL; NULL `=` anything is UNKNOWN |
| `LessThan`, `LessThanOrEq`, `GreaterThan`, `GreaterThanOrEq` | NULL compares UNKNOWN against every operand |
| `StartsWith` | a prefix predicate over a non-NULL operand |
| `In` | UNKNOWN for a NULL probe against any list |
| `Like` | UNKNOWN for a NULL subject |
| `NotEquals` | UNKNOWN for a NULL subject — `NULL <> x` is not TRUE |

Everything else is **refused**, and three refusals are load-bearing:

- **`IsNull`** — admits only NULL. The exact inverse.
- **`NotDistinctFrom` — refused, and this is the trap.** It reads like an
  equality and is not one: `NotDistinctFrom` is *NULL-safe* equality, so
  `x IS NOT DISTINCT FROM NULL` is TRUE for a NULL `x`. It admits NULL rows.
  Go's `canBeUsedInScanPrefix` lists it beside `ComparisonEquals`
  (`range_constraints.go:166`) because for the *scan-prefix* question the two
  behave alike; for the *NULL-rejection* question they are opposites. An
  implementation that reuses `canBeUsedInScanPrefix` as R2's allow-list is
  therefore wrong in exactly one entry, silently, on the shape most likely to be
  written by a user who knows SQL's three-valued semantics. **R2's allow-list is
  its own closed enumeration, not a call into the scan-prefix one**, and a test
  pins the refusal by name.
- **Anything under `OR` or `NOT`** — a disjunct that rejects NULL in one branch
  proves nothing about the other, and a negation inverts the admission. Only
  **top-level conjuncts** count, and only conjuncts sitting **strictly between
  the scan and the `DISTINCT`** — a filter above the `DISTINCT` cannot license
  eliding it, because the duplicates would have already been emitted.

**Each key column needs its own conjunct.** A composite `UNIQUE (a, b)` with only
`a` constrained still admits `(1, NULL), (1, NULL)`. Coverage is per-column and
the conjunction is over the key, not over the filter.

**R2's soundness rests entirely on three-valued semantics, and a test must pin
that at the call site.** The whole argument is that a row whose key column is
NULL evaluates the conjunct to UNKNOWN and is therefore **dropped** by the filter
executor — not passed through, not treated as TRUE. That is a property of the
executor, it is asserted nowhere today, and if it ever changed R2 would emit
duplicate rows with every one of R2's own tests still green. The pin belongs on
the filter executor's UNKNOWN handling, named as R2's ground.

**R2 is what makes the arm reachable from SQL at all.**
`SELECT DISTINCT email FROM users WHERE email IS NOT NULL` is an ordinary query,
it is the query a user writes when they know the column is nullable, and under R2
it fully elides. R1 could never fire on it.

#### R3 — residual: dedup the exempt subset instead of proving it empty

R3 gives up on the proof and attacks the operator instead. If the exempt set is
non-empty (or unprovable), the `DISTINCT` is **not** elided — but it is
**narrowed**: only rows whose key components are exempt (NULL or NaN) enter the
seen-set; every other row passes through in O(1) with nothing retained.

The soundness argument is one line: the index's uniqueness already guarantees at
most one row per non-exempt key value, so a non-exempt row can never duplicate
anything — not another non-exempt row (uniqueness), and not an exempt row (an
exempt row's key differs from a non-exempt row's by construction, since
exemptness is a property of the key value itself).

**R3 strictly dominates today's operator, which is why it needs no cost-model
trade-off.** The narrowed seen-set is a **subset** of the full one on every
input: worst case (every row exempt) it is identical; typical case (a nullable
column with no NULLs, which is the ordinary state of an ordinary table) it is
**empty**, and the operator degenerates to an O(1) pass-through holding nothing.
There is no input on which R3 is worse. A change that is never worse does not
need to be costed against the alternative, and no cost formula moves.

**The executor change is small and local**: a predicate on the keyer in
`executeHashDistinct` — is any component of this row's dedup key exempt? — and
the existing seen-set machinery unchanged behind it. It is tens of lines, not a
new operator.

**§9(g)'s rejection of a plan-visible node does not transfer to R3**, and the
distinction is the one §9(g) itself draws. That proposal added a node carrying
*provenance* and no execution behaviour, so every consumer of the plan tree would
have had to learn to see through a node that did nothing. R3's narrowing **is**
execution behaviour — it changes which rows the operator retains — so it belongs
in the plan, visibly.

**Three binding properties, each of which is a wrong-rows condition if missed:**

1. **The narrowing flag is part of plan identity.** It enters
   `RecordQueryDistinctPlan.structuralKey`. A re-plan that flipped
   narrowed↔full while resuming a `DistinctHashContinuation` would rebuild a
   seen-set from a serialization written under the other discipline and **return
   wrong rows** — a narrowed continuation holds only exempt keys, and a full
   operator resuming from it believes it has already seen every key that was
   omitted. This is the same argument §7.4 makes for the §5.2 stamp, on a flag
   whose content is stronger.
2. **R3 renders distinctly in `EXPLAIN`.** The optimization's evidence must be
   assertable positively; a narrowed distinct that silently degraded to a full
   one is otherwise invisible. §7.1 asserts on the rendering.
3. **R3 still records the index dependency.** This is the property most likely to
   be dropped, because R3 "doesn't elide anything" and so reads as unconditional.
   It is not: R3's soundness rests on *the index's uniqueness* exactly as R1's
   and R2's do — remove the uniqueness guarantee and a non-exempt row can
   duplicate another non-exempt row, which is precisely what the narrowed
   seen-set no longer catches. So §5.2's stamp, §5.3's transition table and
   §5.2's `40001` path apply to R3 **unchanged**, and the stamp rides on the
   narrowed distinct plan rather than on an elided inner.

#### How the three compose

The rule evaluates in strength order and takes the first that discharges the
obligation: **R1 → R2 → full elision; otherwise R3 → narrowed distinct**. R3 is
the floor, not a fallback that can fail: when neither R1 nor R2 proves the exempt
set empty, R3 always applies as long as clauses 1-7 hold, because it needs no
fact about the stream at all. When *none* of clauses 1-7 hold there is no
qualifying index and the operator is the unmodified full distinct, as today.

### 5.2 The dependency is carried by the plan, not by a side channel

#616 anticipated a `proofOnly []string` list threaded from the planner to
`collectPlanIndexDependencies`. Implementing against it surfaces a hole that
was invisible without a producer.

**The hole.** `cascades_generator.go:374` derives dependencies on a plan-cache
**hit** from the cached plan alone, with the invariant stated in the comment:

> Dependencies are a function of the PLAN, so a cache hit derives them from the
> cached plan rather than carrying anything in the cache entry. A cached plan
> gets the same guarantee as a freshly planned one, which is what the cache
> exists to be transparent about.

A proof-only dependency is, by construction, **not** a function of the plan — the
whole point is that the plan names the index nowhere. So a side-channel list
threaded only through the fresh-planning path is dropped on every cache hit, and
the cache hit is precisely the long-lived case the revalidation exists for. The
first execution would be guarded and every later one unguarded: the worst
possible shape, because it passes the obvious test.

**The design.** Make the dependency a function of the plan by putting it *in* the
plan. The eliding rule yields the inner plan stamped with the proving index's
identity — a struct copy, exactly the mechanism `makeStrictlySorted` /
`WithStrictlySorted` already uses to stamp a proved fact onto a plan node
(`rule_implement_sort.go`). `collectScannedIndexNames`' walk gains one arm that
reads the stamp, and the "dependencies are a function of the plan" invariant
holds for proof-only dependencies too, unchanged and unqualified.

Four properties follow, and they are the argument for this over the side channel:

- **Cache-transparent — and the strong form of this is not "no format change".**
  The weak argument is that a side channel would need a new cache-entry field;
  that argument is thin, because `planCacheEntry` already carries a second field
  beside the plan (`scalarSubs`, `plan_cache.go:47-49`), so adding a third is
  mechanically easy. The real argument is that **the cache is not the only
  plan-producing path**: `PlanRecordQueryWithMetadata` /
  `PlanRecordQueryWithMetadataSchema` (`plan_harness.go:469-499`) plan through
  the same pipeline while bypassing the cache entirely, and it is the path the
  package-level tests and §2.1's probe use to read facts off a planned plan. A
  dependency living in the cache entry is absent on that path by construction —
  so the fact would be unassertable exactly where the unit-level pins are
  written. A plan-carried stamp is present on every path that produces a plan,
  which is the property being bought.
- **Only the winner counts.** A planner-global accumulator would record every
  index whose proof fired during exploration, including proofs on expressions
  that lost on cost. That over-records, and over-recording is not benign: it is
  the self-inflicted outage `index_state_planning.go` rejects unscoped signatures
  for — an unrelated index build 40001-ing statements that never depended on it.
  A stamp rides on the expression and dies with it if the expression loses.
- **Continuation-safe by the same route.** Java attaches
  `DatabaseObjectDependenciesPredicate` as the *continuation* plan constraint
  (`fdb-relational-core/…/recordlayer/query/QueryPlan.java:667, 726-735` — the
  relational layer, not `fdb-record-layer-core`) so a resumed page is gated too.
  A plan-carried
  stamp is present on the plan a continuation resumes, with no extra plumbing.
- **EXPLAIN-visible.** The optimization's evidence is otherwise the *absence* of
  an operator, which is a weak acceptance assertion (a rule that silently died
  also produces an absence). The stamp is **rendered in `EXPLAIN`** — as a
  suffix annotation on the stamped node, e.g.
  `Project([EMAIL#1], Scan(USERS)) distinct-by:BY_EMAIL` — so §7.1 can assert
  positively that the rule fired *and* which index licensed it. No existing
  `EXPLAIN` golden moves, because no plan carries a stamp today. This is
  deliberately unlike `RecordQueryDistinctPlan.Streaming`, which `EXPLAIN` does
  not render and which §2.1's probe therefore has to read off the plan object:
  a fact an acceptance criterion must assert should be visible where the criteria
  are written.

**Where the stamp lives.** Not on `PlanExprBase` — that is a zero-size marker
carrying default methods, and giving it state would reach into every plan type's
hand-written `structuralKey`. It goes on the plan types the eliding rule can
actually yield (the projection and the scan/index plans beneath it), set by a
struct-copy `With…` in the `WithStrictlySorted` shape
(`index_scan.go:366-370`), and it is read through a **capability interface**, the
way `index_state_planning.go`'s own `indexNamedPlan` already is:

> Asking for the CAPABILITY rather than enumerating concrete plan types is what
> keeps this complete as plan types are added.

So the collecting walk gains one arm, not a type switch, and a plan type that
cannot carry a proof cannot be handed one.

**The stamp is part of plan identity — each carrier's `structuralKey` includes
it.** The
tempting call is the opposite one, by analogy with
`RecordQueryProjectionPlan.aliasMinted`, which is deliberately excluded from
identity — and that exclusion is not merely a precedent, it is an explicit
prohibition (`expressions/logical_projection.go:180-186`):

> Excluding the marker stays correct (a display tag must not split a memo
> group). The fix belongs on the READ side … **Do not answer this by folding
> `aliasMinted` into identity** — that trades a wrong label for a duplicated memo
> group, and re-creates the recover-instead-of-record pattern the marker was
> introduced to end.

The distinction that makes this RFC's stamp the opposite case is in that
comment's own first clause. `aliasMinted` is a **display tag**: two plans
differing only in it compute identical rows, so splitting the group buys nothing
and costs a duplicated group. The stamp is a **proved fact that licensed the
plan's shape**: two plans differing only in it are *not* interchangeable, because
one is correct only while an index stays `READABLE` and the other is correct
unconditionally. A display tag must not split a group; a proved fact must. And
the recover-instead-of-record pattern that comment warns against is precisely
what a side channel would reintroduce here — recovering the dependency later from
whatever the memo happened to keep, instead of recording it on the plan.

The analogy fails, and the direction it fails in is the one that matters. The
eliding rule
yields a stamped copy of a plan whose unstamped original is already in the memo;
if the stamp is not identity-bearing, the two collapse and the survivor may be
the unstamped one — silently dropping the dependency and producing exactly the
unguarded elision this RFC exists to prevent. The alternative repair, unioning
stamps on collapse, is worse: it would attach the dependency to every *other* use
of that plan, which is the over-scoping of §9(b).

So the stamp is identity-bearing, and the cost is stated rather than hidden: the
memo can hold a stamped and an unstamped entry for the same physical work, and
they cost identically. That is one extra group member on one query shape, and the
tie-break is already deterministic. Losing a dependency is a wrong-answer bug;
holding one redundant memo entry is not. It is pinned by §7.4 rather than
asserted here.

**The stamp records only when the secondary-`UNIQUE` proof is the SOLE
license.** This is a correctness requirement on the rule's evaluation order, not
a tidiness preference. `ImplementDistinctFinalRule` already has a property path —
`partition.IsDistinct()` over `PropDistinctRecords`, Java's mechanism — that
licenses the elision on its own for many plans. That path **must be evaluated
first**, and when it succeeds the yielded plan is **UNSTAMPED**, even if a
qualifying unique index also happens to exist.

Stamping a plan whose elision the property path already justified records a
dependency the plan's correctness does not rest on. Transitioning that index
would then `40001` a statement that would have been correct regardless — which is
precisely the over-scoping this RFC rejects in §9(b), arrived at from the other
direction. The primary-key arm likewise never stamps: a PK is a storage
invariant, not an index whose state can move.

So the rule's order is: property path → PK arm → secondary-`UNIQUE` arm, and only
the last one stamps.

**New acceptance criterion (§7.3).** A query where **both** licenses hold — the
inner plan is `PropDistinctRecords`-distinct *and* a qualifying unique index
covers the projection — must yield an **unstamped** plan, and transitioning that
index out of `READABLE` must **not** produce `40001`. Without this test the
conservative-looking implementation (stamp whenever a unique index qualifies)
passes everything else in §7 while turning unrelated index builds into statement
failures.

**Recorded content:** the index `Name` and its `LastModifiedVersion` at planning
time — `predicates.UsedIndex`, the same type the ported
`DatabaseObjectDependenciesPredicate` carries, deliberately not a second
structurally identical type. `LastModifiedVersion` is not redundant with the
name: an index can be dropped and recreated under the same name with a different
definition between planning and execution, and Java checks it for that reason
(`DatabaseObjectDependenciesPredicate.java:95-97`).

**The predicate makes three checks, not two, and the third is the one §5.3 rests
on.** Java's `eval` (`DatabaseObjectDependenciesPredicate.java:87-105`) asks:
does the index still exist; does its `lastModifiedVersion` still match; and —
`:99` — `recordStoreState.isReadable(currentIndex)`. That third check is the
mechanism behind every "leaves `READABLE` → `40001`" row in §5.3's table. Without
naming it, that table reads as an aspiration; with it, the table is a
consequence of a predicate Go already ports (`storeIndexAvailability.IsIndexReadable`,
strictly `READABLE`, `index_state_planning.go:207-213`).

**Consequence for the `proofOnly` parameter.** With the stamp collected by the
walk, `collectPlanIndexDependencies`' fourth parameter has no producer at any of
its three production call sites (`cascades_generator.go:374`, `:535`, `:1136`,
all passing `nil` today) nor in its one test. A seam with no producer is dead
weight that reads as coverage, so this RFC deletes the parameter and folds its
unit-test arm into a test of the stamp walk. This is a deliberate reversal of one shape of #616's
design one commit later, made with the information #616 lacked — a producer — and
it is called out here rather than buried so the gate can rule on it directly.

**Explicitly excluded from execution identity.** The stamp is planner
provenance, not physical execution. It must not enter
`scanRangeExecutionIdentity` (the SHA-256 continuation fingerprint), and a test
pins that a stamped and an unstamped plan produce the same fingerprint —
otherwise this read-side change would invalidate live continuations, which §6
promises it does not.

### 5.3 State transitions mid-statement

Three windows, and what each does:

| window | mechanism | outcome |
|---|---|---|
| Index leaves `READABLE` between planning and first execution | `validatePlanIndexDependencies` (#616) sees the stamped dependency | `40001`, replan |
| Index leaves `READABLE` between two pages of a continuation | same predicate, evaluated per execution transaction | `40001`, replan |
| Index leaves `READABLE` while a *cached* plan is reused | same predicate — because §5.2 keeps the dependency a function of the plan | `40001`, replan |
| Index *becomes* readable | not examined | plan stays valid |

The last row is a theorem, not a rule anyone maintains — **given §5.1.1's first
half**, without which it is merely a hope. On a run that established index state,
an index that was not readable at planning was not a candidate, so no proof rests
on it, so it is not in the dependency set, so its becoming readable is never
examined. On a run that did *not* establish index state, no secondary-`UNIQUE`
proof was admitted in the first place, so the dependency set has no such entry
either and the theorem holds vacuously. Both branches close; the second is the
one §5.1.1 added. Java lands in the same place by the same structural route
(`RecordStoreState.compatibleWith` iterates only the non-readable exceptions),
and reaches it on every path because its filter has no unstated callers.

`40001` (`ErrCodeSerializationFailure`) rather than a plan error is the existing
choice and the right one: the condition is a lost race between two transactions
and the caller's correct response is a retry, which replans against the new
state.

**The window this does not close, stated plainly.** Between the snapshot
planning read the index state from and the transaction that executes, the state
can change; that is exactly what the revalidation is for, and it converts the
window into a retry rather than eliminating it. What it cannot do is protect a
statement that never re-enters the store — there is none: every execution opens
the store, and the predicate is evaluated there.

### 5.4 Float and NULL safety — R1's implementation (#617's gates, reused verbatim)

This subsection is **R1** (§5.1.2) in detail: the metadata route, and the exact
statement of the two hazards that define the exempt set. Both make a *unique
storage key* fail to be a *unique logical row*, in opposite directions, and #617
already built the predicate for both. R2 and R3 discharge the same obligation by
other means; nothing here is relaxed to accommodate them.

**NULL — the direction most likely to be got wrong.** Under SQL's default
`NULLS DISTINCT` semantics the uniqueness check is skipped when an indexed
component is NULL, so one NULL prefix can hold arbitrarily many entries
distinguished only by their appended primary key. A `UNIQUE` index on a nullable
column therefore permits `(NULL), (NULL), (NULL)` — and `SELECT DISTINCT col`
must return one row. Eliding on such an index emits three. Refused by
`SecondaryUniqueKeyGloballyEnforced`'s nullability clause.

**FLOAT/DOUBLE.** FDB preserves distinct raw NaN sign and payload encodings, so
`0xfff8000000000001` and `0x7ff8000000000001` are two distinct tuple keys that
the unique index will happily hold — while `values.CompareFloat64` (faithful to
`java.lang.Double.compare`) canonicalizes every NaN to one value. Storage
identity is *finer* than logical equality, so the index's uniqueness does not
imply the projection is duplicate-free: elision would emit two rows where
`DISTINCT` emits one. Refused by
`TupleKeyUniquenessMatchesLogicalEquality`, which
`SecondaryUniqueKeyGloballyEnforced` calls first.

Note this is the mirror image of the hazard #617 fixed on the primary-key arm
(`TestDistinctFinal_FloatingPrimaryKeyNeverEliminates`), and the same predicate
answers both — which is the point of §5.5.

The types fed to the predicate must be the **authoritative physical key
component types** (RFC-208's `KeyComponentTypes`), not a declared SQL type or a
Go carrier: an `Unknown` type fails closed, because a visible literal cannot
substitute for missing metadata.

**Worth saying out loud: this predicate was written for this caller and has never
had one.** `SecondaryUniqueKeyGloballyEnforced`'s own doc
(`physical_equality_shape.go:124-131`) states its purpose as *"used for DISTINCT
elimination and strict-ordering claims"* — and before this RFC only the
strict-ordering half had a caller (`indexHasGloballyEnforcedUniqueKey`,
`rule_implement_sort.go:317`). The DISTINCT half of that sentence had been
aspirational since #617 landed it. R1 makes it true.

**And R1 alone leaves it true-but-unreachable on SQL, which is the finding that
produced R2 and R3.** The predicate fits; it is the *surface* that cannot feed
it, because SQL has no `NOT NULL` scalar column. That is a fact about the DDL,
not about this predicate, and the correct response is a second and third route to
the same obligation — never a relaxation of this one. An implementation that
finds itself widening `SecondaryUniqueKeyGloballyEnforced` to make an arm fire
should treat that as evidence it is on the wrong arm.

### 5.5 One property, not two cross-checked

The #617 review lap raised an architectural note this RFC adopts as mechanism
rather than booking as a follow-on, because ignoring it would make the problem
worse by one:

> the truncated ordering should CARRY the fact that it dropped uniqueness-making
> coordinates rather than the consumer re-deriving it from `KeyComponentTypes`.

Today two consumers cross-check two properties.
`RecordQueryIndexPlan.HintOrdering` (`plans/ordering.go:575`) truncates the
advertised sorted claim at the first
`FLOAT`/`DOUBLE` coordinate (`claimableNameLimit`), and then
`rule_implement_sort.go` asks a *second* question of a *different* property —

```go
if partition.IsDistinct() && expressionStorageOrderingIsComplete(expr) {
```

— where `planStorageOrderingIsComplete` re-derives, from `KeyComponentTypes`,
whether that truncation happened. The ordering already knows. Re-deriving it is
two properties kept in agreement by hand, and design principle 10 says to match
the architectural property rather than cross-check a downstream observable.

Adding this RFC's admission predicate as a **third** re-derivation from
`KeyComponentTypes` is the failure mode. So the design is:

**One carried property.** The index scan plan and its match candidate carry a
single `UniquenessProof`-shaped value stating (a) whether the storage key's
uniqueness is globally enforced in the sense of §5.4, and (b) which coordinates
it covers. The ordering derivation stamps completeness on the ordering it
produces (it is the site that truncates, so it is the site that knows). Then:

- `rule_implement_sort.go` reads the ordering's own completeness flag instead of
  calling `expressionStorageOrderingIsComplete`;
- `rule_implement_distinct_final.go`'s new arm reads the carried proof instead of
  re-deriving from `KeyComponentTypes`;
- `physical_equality_shape.go` remains the single implementation, called once at
  the producer rather than once per consumer.

**Carried completeness MUST NOT survive prefixing, and this is the part of §5.5
most likely to be got wrong.** A carried flag is a claim about a *specific
coordinate set*. The moment an ordering is truncated, prefixed, or otherwise
reduced to fewer coordinates, a completeness claim computed for the longer set is
no longer about the object carrying it.

Java built this exact mechanism, hit this exact hazard, and left the warning in
the field's own javadoc (`Ordering.java:185-192`):

```java
/**
 * Indicator if the records flowed are to be considered distinct, thus producing a strict order. This field
 * should get deprecated as it is not correct to assign this property to all enumerated orderings. For instance,
 * an ordering that produces {@code a, b, c} is also ordered by {@code a, b}. While the former one might be strict,
 * the latter one may not. I think for now we should just interpret this indicator in a way that only enumerated
 * orderings that contain all values of the ordering are strict if this indicator is {@code true}.
 */
private final boolean isDistinct;
```

The fourth sentence is quoted in full deliberately: it is not commentary, it is
**the interpretation rule this section otherwise paraphrases unsourced**. "Only
enumerated orderings that contain **all values** of the ordering are strict" is
precisely the coordinate-set binding below, written by the person who shipped the
field and could not make the type system enforce it. Go can — by making the
binding structural rather than a convention a reader has to know.

An ordering by `(a, b, c)` is also an ordering by `(a, b)`. The first may be
strict; the second need not be. Java's own field is documented as *not correct*
to propagate, with a hand-written interpretation rule bolted on top ("only
enumerated orderings that contain all values of the ordering are strict"). Go must
not import the bug along with the mechanism.

So the binding is explicit: **completeness is bound to the coordinate set it was
computed over.** Any operation that reduces that set either (a) drops the
completeness claim entirely, or (b) recomputes it against the retained
coordinates. Never (c) carries it forward unexamined. The same rule applies
wherever a carried ordering is concatenated or intersected — Java's
`Ordering.java:1134` guards its concat with a hard `Verify.verify(leftOrdering.isDistinct())`
precondition rather than deriving it, which is the shape of a claim that cannot
be safely assumed.

**Pinned by a test on the `(a, b, c)` → `(a, b)` shape**, over an index whose
trailing coordinate is what makes the key unique: the full ordering may claim
completeness, the two-coordinate prefix must not. **That test does not exist on
either side today** — Java has the warning and no pin, Go has neither.

**This is why §8's criterion is necessary but not sufficient.** Every #617 pin
stays green through this bug: they all exercise a *full* key, so nothing there
ever prefixes a carried ordering. "All #617 tests green, unmodified" would read
as *behaviour-preserving* while the refactor was unsound in a dimension none of
them probes — the dimensional-gap failure the repo's testing rule describes. The
prefix test is the criterion that closes it, and it is required for §5.5 to be
considered done.

Subject to that, the migration is behaviour-preserving by construction and pinned
as such: all of #617's tests (§8) stay green **unmodified**. A refactor that needs
its pins edited is not the refactor it claims to be.

### 5.6 Interaction with the physical distinct's own choices

`newPhysicalDistinctFor` picks between the resume-clean streaming dedup and the
per-page hash set, and freezes the inner plan in the streaming case because the
executor is sound only over the exact ordering the flag was computed for. None of
that machinery changes: when the elision fires there is no distinct operator, so
there is nothing to choose an executor for and no inner to freeze.

The interaction that *does* need stating is with the follow-up recorded in that
function — requesting the dedup-key ordering so an unordered `SELECT DISTINCT
col` can stream instead of hashing. That follow-up would insert an in-memory sort
where no access path provides the ordering. On exactly the shapes this RFC
admits, the elision fires first and there is no dedup key to order by, so the two
do not collide; on every shape this RFC declines, the follow-up is unaffected.
Whichever lands second must not weaken the other, and the §7.2 negative table is
what enforces it: those shapes must keep a distinct operator regardless of which
executor it picks.

### 5.7 #617's other half — the strict-ordering claim, fixed here rather than filed

`SecondaryUniqueKeyGloballyEnforced` has **two** consumers, and §5.4 quotes its
doc naming both: *"used for DISTINCT elimination and strict-ordering claims"*.
The strict-ordering consumer is `indexHasGloballyEnforcedUniqueKey`
(`rule_implement_sort.go:317`), and it is dead on the SQL surface for **exactly
the reason R1 is** — the same nullability clause, the same inexpressible `NOT
NULL` scalar, the same inert predicate. It is not a separate defect; it is the
same defect with a second consumer, and the refutation test already asserts both
halves in one function (its arm (c)).

**So it is fixed here, and gets no separate item.** Filing it would split one
finding across two changes, and the second half would inherit none of the context
that makes the first correct.

**R2 applies verbatim.** `SELECT email FROM users ORDER BY email WHERE email IS
NOT NULL` — a filter rejecting NULL on every key column, strictly between the
scan and the ordering consumer — leaves a stream on which the index's key is
genuinely unique, so the scan is genuinely **strictly sorted**: no two rows share
a key, so there are no ties for the claim to be wrong about. The same allow-list,
the same top-level-conjunct restriction, the same coverage-per-key-column rule.
R2's analysis is written once and consumed twice.

**R1 stays**, unchanged, for the direct-API caller exactly as in §5.1.2.

**There is NO R3 analogue, and inventing one would be a soundness bug.** This is
the part most likely to be got wrong by symmetry, so it is stated as a
prohibition rather than an omission. R3 works for `DISTINCT` because a `DISTINCT`
has a *residual*: the operator can still run, narrowed, and catch what the proof
did not cover. A **sort claim has no residual**. `strictlySorted` is a
proposition the plan asserts to its consumers — it licenses *them* to skip work —
and there is no narrowed version of an assertion. A "mostly strictly sorted" scan
is a scan whose claim is false, and the consumer that trusted it has already
skipped the dedup or the merge that would have caught it. **A residual cannot
compensate a sort claim.** Where R1 and R2 both decline, the claim is simply not
made, which is today's behaviour and is correct.

#### Java is unsound here, and this is an upstream bug rather than a parity target

`RemoveSortRule.strictlyOrderedIfUnique` (`RemoveSortRule.java:144-156`) decides
the whole question at `:153`:

```java
return matchCandidate.isUnique() && numKeys >= matchCandidate.getColumnSize();
```

**There is no nullability term.** No branch of that method, and nothing on the
path into it from `:132`, consults the key columns' types or their nullability.
So over a `UNIQUE` index on a **nullable** column, Java claims `strictlySorted`
on a scan that has **genuine ties** — under `NULLS DISTINCT` the index legitimately
holds `(NULL, pk=1), (NULL, pk=2), (NULL, pk=3)`, three entries whose *claimed*
sort key is identical. A consumer that trusts `strictlySorted` to mean "no two
rows compare equal on the sort key" is being told something false by construction,
not by an edge case.

This mirrors the NaN half exactly: the same rule, the same absent type
consultation, the same "storage distinguishes what the claim says is
indistinguishable" shape — and DIVERGENCES.md already carries that half as an
upstream bug. The NULL half is its sibling and belongs beside it.

**Disposition, and it is deliberate:**

- **Go does not port the bug.** `indexHasGloballyEnforcedUniqueKey` keeps its
  nullability clause; R2 is wired into it so the claim becomes *reachable* on the
  streams where it is *true*, which is the opposite of relaxing it.
- **The divergence is documented at the call site**, `rule_implement_sort.go:317`
  — the site that would otherwise read as an unexplained extra check someone
  could "simplify" back into Java's shape.
- **A DIVERGENCES.md entry goes beside the NaN/ordering one**, in the *Java
  Upstream Bugs* framing that entry already uses, citing `RemoveSortRule.java:153`
  verbatim and naming the concrete three-NULL-entry witness.
- **It is reported upstream**, and the entry says so, so the next reader knows
  whether the divergence is expected to persist.

This is the "fix it at the boundary, document the divergence at the call site,
report it upstream" disposition — not a deferral, and not a reason to weaken Go's
predicate to match a claim Java cannot support.

## 6. Wire compatibility

**Nothing this RFC changes is written to FDB.**

- No key encoding, no index entry format, no record or version format, no split
  behaviour, no store header.
- No metadata change: no new index type, no new index option. The `unique` flag
  read here is the one Java already writes and reads.
- No index maintenance change: the write path is untouched, so the entries a Go
  application produces are byte-identical to today's and to Java's.
- No continuation format change, and §5.2 pins that the plan stamp does not enter
  the continuation fingerprint. A continuation minted before this lands resumes
  after it.

The interop statement in full: a Java application and a Go application sharing a
cluster read and write the same records before and after. The only observable
difference is the physical plan Go chooses for one query shape, and both engines
return the same rows for it — Java via a dedup operator it always builds, Go via
the same scan with the operator proved unnecessary. The index is consulted as
*metadata*, not read: §2.1 measured that the access path is identical either way.

## 7. Acceptance criteria

Every criterion is an FDB integration test or a yamsql scenario with an `EXPLAIN`
assertion. Per repo rules the optimization must be shown to **fire**, not merely
to return correct rows.

### 7.1 The optimization fires

Over §2.1's fixture, `SELECT DISTINCT email FROM users` must plan as

```
Project([EMAIL#1], Scan(USERS)) distinct-by:BY_EMAIL
```

— today's `SELECT email FROM users` plan plus the §5.2 stamp — and must return
100 000 rows. Three assertions, and all three are load-bearing:

- `EXPLAIN` **does not contain** `Distinct(` — the elision fired;
- `EXPLAIN` **names `BY_EMAIL` as the licensing index** — the positive assertion.
  An absence alone is a weak test: a rule that silently died also produces one;
- with the stamp annotation removed, `EXPLAIN` **equals the no-`DISTINCT` plan
  string exactly** — the elision did not also change the access path, which §2.1
  shows would be a 15x-21x regression rather than a win.

The negative controls in §7.2 are what keep the first assertion from being
satisfiable by deleting the `DISTINCT` operator outright.

`SELECT DISTINCT email FROM users ORDER BY email` must likewise lose its
`Distinct(` while keeping `IndexScan(BY_EMAIL, [*])` — the elision must not
disturb an ordering the query asked for.

**Both of the above are R1 shapes and are therefore stated over a fixture built
through the record-layer API**, whose key can be `NOT NULL`. On the SQL surface
R1 cannot fire, which is the whole content of the second refutation, so the two
criteria below are the ones that carry §7.1 for SQL — and both are now
**REACHABLE**, which is the difference this ruling made.

- **R2, full elision, end-to-end from SQL.**
  `SELECT DISTINCT email FROM users WHERE email IS NOT NULL` must plan with **no
  `Distinct(`** and must name `BY_EMAIL` in the stamp, over an ordinary
  SQL-declared nullable `email STRING`. The same three assertions as above apply,
  plus a fourth that is specific to R2: the plan **retains the filter**. An
  implementation that proved the exempt set empty and then dropped the predicate
  that made it empty would return the NULL rows it just licensed itself to ignore.
- **R3, narrowed distinct, end-to-end from SQL.**
  `SELECT DISTINCT email FROM users` (no filter) must plan with a `Distinct(`
  that renders as **narrowed** and names `BY_EMAIL`, and must return the correct
  rows at every NULL density in §2.1's sweep. The negative half is as important:
  a query with no qualifying unique index must render an **un-narrowed**
  `Distinct(`, or the rendering asserts nothing.

### 7.2 It does not fire where it must not

Each of these asserts the physical distinct is **present** and the rows are
correct. Every one is a dimension on which a wrong admission predicate would
silently emit duplicates.

| shape | why it must not elide |
|---|---|
| `UNIQUE` on a **nullable** column, with ≥2 NULL rows | `NULLS DISTINCT` — the index holds every NULL |
| `UNIQUE` on a **DOUBLE** column, two rows with differing raw NaN encodings | storage identity finer than logical equality |
| `UNIQUE` on a **fan-out** (repeated) column | constrains entries, not rows; empty repeated → no entry |
| `UNIQUE` nested under a **repeated parent**, scalar leaf | fan-out whatever the leaf's fan type says |
| `UNIQUE` on `CARDINALITY(col)` | keys a derived value |
| **SPARSE** `UNIQUE` (`WHERE`-filtered), with duplicate values among the rows the predicate EXCLUDES | the `unique` declaration constrains only admitted rows (§5.1 clause 4) |
| **composite** `UNIQUE (a,b)`, only `a` projected | partial coverage |
| unique column projected only **inside an expression** (`f(email)`) | not injective |
| **multi-record-type** stream | key unique only within a type |

The nullable, DOUBLE and **sparse** rows must be **end-to-end with real data**,
not unit-level: they are the ones where the wrong answer is duplicate rows rather
than a missed optimization. The sparse row's fixture is specific and must not be
weakened into "a sparse index exists" — the table needs **real duplicate values
sitting outside the predicate**, because a sparse-index test whose excluded rows
happen to be distinct passes with the bug fully present.

**Fixture reality — each row is constructible, but not all at the same layer, and
three of them not in SQL at all.** This is specified here because "write an e2e
test" for these shapes runs into DDL that does not exist, and an implementer
discovering that mid-task will be tempted to weaken the shape instead of moving
the layer:

- **DOUBLE / raw NaN.** `0xfff8000000000001` has **no SQL literal spelling**. Two
  routes, either acceptable: (i) stay in SQL and use the reachable pair —
  `0x7ff8000000000001` (a positive-payload NaN, producible arithmetically)
  against `0xfff8000000000000`, obtainable as `(+Inf) + (-Inf)`; or (ii) drop to
  the record-layer API and write both encodings directly, as
  `rawnan_pk_suffix_fdb_test.go` already does. Route (ii) is preferred — it is
  the existing precedent and it can express payloads SQL cannot reach.
- **The two fan-out rows** (`TAGS`, and the nested-under-repeated-parent `CITY`)
  cannot be built through the SQL DDL or the schema-template builder:
  `AddFanOutIndex` takes no `unique` parameter, and SQL rejects a non-unnested
  array index outright. They need raw `recordlayer.NewIndex` with the `unique`
  option set, and are therefore **unit-level** — marked as such in the plan, not
  quietly demoted later.
- **`CARDINALITY`** is `CREATE UNIQUE INDEX … AS SELECT CARDINALITY(col) …
  FROM … ORDER BY …` only. The `ON t (CARDINALITY(col))` form does not parse.
  Precedent: `ddl_fail_closed_test.go:303`.
- **Sparse.** The syntax is `CREATE UNIQUE INDEX … AS SELECT … FROM … WHERE …`;
  the `ON t (col) WHERE …` form is a **parse error**. More importantly:
  **`UNIQUE` and a stored predicate have never been combined anywhere in this
  repo** — not in a test, not in a yamsql scenario. So the first task in this row
  is a **smoke check** that the combination is even accepted by the DDL and
  produces an index with both `unique` and `predicateProto` set. If it is
  rejected, clause 4 is unreachable-by-construction today and the row becomes a
  pin on *that* fact (with the reachability re-armed the moment the DDL accepts
  it) — which is a different test, and must not be silently substituted for this
  one.

### 7.3 Index state

- **`TestFDB_UniquePendingIndexDoesNotEliminateDistinct` stays green,
  byte-for-byte.** It must still return 2 rows. Under §5.1.1 it passes for a
  specific reason that must be understood before touching it: it plans through
  the metadata-only harness, which is in the **unknown** index-state, so the
  secondary-`UNIQUE` arm never fires there at all. It is therefore the pin on
  the **fail-closed default**, not on the candidate filter. If it needs an edit,
  the lift is wrong — and if it goes red, the zero-value demotion was not done.

- **NEW, and required — the SQL-path witness (§5.1.1(c)).**
  `TestFDB_IndexStatePlanning_SecondaryUniqueIndexIsNotADistinctnessProof`
  (`index_state_planning_fdb_test.go:312`) already runs `SELECT DISTINCT EMAIL
  FROM T` over a `CREATE UNIQUE INDEX U_EMAIL ON T (EMAIL)` fixture through the
  **production SQL path**, and its own comment states the migration in advance:

  > Whoever lifts it replaces this test with coverage of the pending state; they
  > do not delete it to go green.

  That is this RFC's instruction, already written by the commit that created the
  decline. The test's current assertion (`EXPLAIN` contains `Distinct`) inverts
  for the `READABLE` case, and gains the `READABLE_UNIQUE_PENDING` arm: same
  query, index transitioned to pending, `DISTINCT` **present** and rows correct.
  This is the assertion the harness path structurally cannot make, because it is
  the one that exercises the candidate filter rather than the fail-closed
  default.

  This also converts §8's "unmodified" clause from an unenforceable request into
  an executable property: one witness is pinned byte-for-byte on the harness
  path, the other is deliberately rewritten on the SQL path, and the two cannot
  be confused because they fail for different reasons.
- `WRITE_ONLY` and `DISABLED`: no elision, correct rows, no
  `IndexNotReadableError`.
- **Mid-statement transition → `40001`.** Plan the eliding query, transition the
  proving index out of `READABLE`, execute: `40001` naming the index, not stale
  rows.
- **Transition across a continuation page.** Same, with the transition between
  page 1 and page 2 of a resumed cursor.
- **Transition against a CACHED plan — the §5.2 hole's regression.** Plan and
  cache the eliding query, transition the index, execute again so the cache hits:
  `40001`. Under a side-channel design this test returns duplicate rows; it is
  what makes §5.2's choice a proved property rather than an argument.
- **Both licenses hold → NO `40001` (§5.2).** A query whose inner plan is already
  `PropDistinctRecords`-distinct *and* over which a qualifying unique index
  covers the projection: the plan must be **unstamped**, and transitioning that
  index out of `READABLE` must leave the statement running normally. This is the
  criterion that fails the over-eager implementation — stamp whenever a unique
  index qualifies — which every other test here would pass.

Note, so an implementation does not mistake one refusal for another: the unit
test `TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof` passes
**today** only because `distinctEliminatedByUniqueKey` ends in an unconditional
`return false`. The decline's blanket refusal is currently doing the work the
admission predicate must do afterwards, and the arms do not distinguish them.

Which clause catches which arm, stated exactly — an earlier draft got this wrong
in both directions and the corrected version is more useful:

| arm | refused by | note |
|---|---|---|
| `TAGS` | **clause 3** | `createsDuplicates` is **`true`** here (`rule_distinct_on_unique_elim_test.go:481`, `fanOut := true`) |
| `SCORE` | **clause 5** | `createsDuplicates` is `false`; it is the `CARDINALITY()` function-keying that refuses it |
| `CITY` | clause 3, *only if the signal is derived* | the fixture hand-supplies `createsDuplicates = false`, so this arm pins the rule's **use** of the signal, not the signal's derivation from the nested-repeated root key expression |
| `EMAIL` | nothing — **inverts** | the arm this RFC exists for |

The conclusion the earlier note reached still stands and is the one that matters:
a predicate built from **clause 3 alone** admits `SCORE`, `CITY` and `EMAIL`, so
it reds this test on the first day of implementation. `CITY` is the arm to watch
— it is refused in production only if the candidate's duplicate signal is derived
from the root key expression, which this fixture bypasses.

### 7.4 Plan identity and continuations

Two assertions that pull in opposite directions, which is why both are pinned:

- **`structuralKey` DOES distinguish** a stamped plan from its unstamped
  original — the memo must not collapse them (§5.2). The test builds both and
  asserts the keys differ; without it, the collapse would delete the dependency
  silently and every other test in §7.3 would still pass on a fresh plan.
- **`scanRangeExecutionIdentity` does NOT** — a stamped and an unstamped plan
  produce the same SHA-256 continuation fingerprint, and a continuation minted
  before this change resumes correctly after it. The stamp is planner provenance;
  it describes nothing about the physical scan a continuation resumes.

The pairing is not novel, and the precedent should be followed rather than
re-derived: `strictlySorted` is already exactly this shape. It **is** in
`RecordQueryIndexPlan.structuralKey` (`index_scan.go:409-421`, alongside
`reverse` and `covering`) and **is not** in `indexScanRangeFingerprintSalt`
(`scan_range_execution_identity.go:367-385`, which takes index name, scan type,
reverse, covering, covering columns, PK columns, record types, key types, flowed
type — and no strictness). A proved planning fact that splits the memo group and
leaves the continuation identity alone is an established pattern here, not a new
exception being carved for this RFC.

### 7.5 Performance

The probe from §2.1 is committed with this RFC, but **as committed it only
asserts plan shapes and executor variants — it does not assert a single timing or
allocation figure.** Its timing sections `t.Logf`. Turning it into the regression
harness is therefore implementation work, not a property it already has, and this
section is a specification for that rewrite rather than a description of it. An
implementation that lands the elision and leaves the probe logging has not met
§7.5.

**And the criteria are written against §2.1's DELIVERED-DELTA table, not against
its cost-of-the-operator table.** That is the substantive change the second
refutation forced here: a criterion of the form "the `DISTINCT` query approaches
the no-`DISTINCT` query" was written when the elision was believed to fire on the
bare SQL query. It does not — the bare query gets **R3**, which narrows the
operator rather than removing it — so the criterion has to name its route.

The criteria, on the regime the change is actually for and deliberately not on
the regimes §2.1 could not resolve. Rows A/B/C/D are §2.1's delivered-delta rows:

- **R2, full elision (C vs A).** Paged, unordered, 100k rows, all-distinct:
  `SELECT DISTINCT email FROM users WHERE email IS NOT NULL` must come within
  **1.15x** of `SELECT email FROM users`. This is the criterion the old text
  wanted; it is only now attached to a query on which the elision can actually
  fire. Both sides run back-to-back on the same store so a slow moment lands on
  both, and the ratio is a median of 5 pairs, for the reason RFC-209 §7 sets out:
  single pairs on this harness drift enough to fail a clean run.
- **R3, narrowed distinct (B vs D).** The bare `SELECT DISTINCT email FROM users`
  must be **strictly faster than today's full distinct** on the same shape — the
  0%-NULL case, where R3's seen-set is empty. The bound is expressed against **D**
  (today's operator) rather than against A, because R3 does not remove the
  operator and a bound against A would be a criterion on an outcome R3 does not
  claim. The exempt test must be evaluated on the row's raw slots **before** the
  dedup key is packed, so a non-exempt row costs a nil/NaN check and nothing
  else; an implementation that packs first and tests after will fail this row
  while passing every correctness test.
- **Allocation churn**, R2 shape within **1.15x** of the no-`DISTINCT` run (today
  ~2.7x), and R3 shape **at or below** today's. This is the criterion that would
  catch an implementation that elides the operator in `EXPLAIN` while some other
  path reintroduces per-page key retention — and, for R3, one that narrows the
  *seen-set* without narrowing what gets serialized into the continuation.
- **The NULL-density sweep's retained-row counts are asserted exactly** (§2.1).
  They are counts, not timings, so they carry no band and no noise argument, and
  they are what turns "R3 strictly dominates" from an argument into a test. The
  50% row's timing criterion is **"no worse than full"**, never "faster".
- **No criterion on the ordered or `LIMIT` shapes**, because §2.1 could not
  resolve a delta there — the ordered spreads overlap heavily. A bound on an
  unresolvable delta measures the harness, not the change. They are still
  asserted for *correct rows and plan shape* in §7.1, just not timed. Note this
  is "unmeasurable here", not "measured as zero" (§2.1).

The bound is 1.15x rather than 1.0x because the elision removes an operator, not
a scan: the two plans do the same I/O, and the residual is per-query fixed cost.
A criterion of "strictly no slower" would fail on noise while proving nothing
more.

### 7.6 The refutation test is rewritten, never deleted

`secondary_unique_proof_reachability_test.go` is the probe that produced the
second refutation. It is the reason this RFC has R2 and R3 at all, and the
instinct once the arm goes live will be to delete it as obsolete. It is not
obsolete; three of its four claims survive, one inverts, and two are added. The
rewrite:

- **(a) The DDL-rejection arm stays, unchanged.** `CREATE TABLE` still rejects a
  `NOT NULL` scalar column, and that fact is now *more* load-bearing rather than
  less: it is the reason R1 is vacuous on SQL, which is the reason R2 and R3
  exist. If it ever changes, R1 becomes live on the SQL surface for the first
  time and needs the end-to-end coverage it does not have. The failure message
  says exactly that.
- **(b) The metadata arm's claim is NARROWED, not dropped.** It asserted "the
  proof does not fire on any SQL query". That is now false — R2 and R3 fire. The
  surviving claim is the precise one: **`SecondaryUniqueKeyGloballyEnforced` is
  false for every SQL-expressible unique index**, i.e. *R1* never fires from SQL.
  Same loop, same fixtures, a claim about R1 rather than about the arm.
- **(c) The reachability arm INVERTS.** It asserted that a SQL-planned unique
  index scan is *not* `strictlySorted`. Under §5.7 that is exactly what R2 makes
  false on a NULL-rejecting stream: `SELECT n FROM t WHERE n IS NOT NULL ORDER BY
  n` **must** now plan a strictly-sorted scan. The arm keeps its negative twin —
  the unfiltered `ORDER BY n` must still *not* claim strict sorting, because R2
  did not fire and R1 cannot — so the test pins the boundary rather than one side
  of it.
- **(d) NEW: R2 end-to-end.** The DISTINCT consumer of the same gate, on the same
  filtered shape, asserting full elision and correct rows.
- **(e) NEW: R3 end-to-end.** The unfiltered shape, asserting a narrowed distinct
  and correct rows.

The test is renamed to say what it now pins: the *boundary* between what R1
cannot reach and what R2/R3 do, rather than the blanket unreachability it was
written to record.

## 8. Must not regress

The #617 pins are the guard rails on this change, because #617 is where the
proof predicates this RFC reuses were established. All must stay green, and
those in the first group must stay green **without being edited** — an edit
means the refactor in §5.5 changed behaviour it claimed to preserve.

**This list is necessary and NOT sufficient — see §5.5.** Every pin below stays
green through the prefixing bug §5.5 describes, because each exercises a full
key and none prefixes a carried ordering. The `(a, b, c)` → `(a, b)` test named
there is what makes "all #617 pins green" mean *behaviour-preserving* rather than
*unprobed in the dimension that matters*.

Unmodified:

- `TestDistinctFinal_FloatingPrimaryKeyNeverEliminates`
- `TestDistinctFinal_MultiTypeVisiblePrimaryKeyDoesNotEliminate`
- `TestDistinctFinal_PartitionDistinctnessNeedsLogicalEquality`
- `TestRecordIdentityMatchesLogicalEquality`
- `TestStorageOrderingCompletenessIsStrongerThanRecordIdentity`
- `TestLogicalDistinctnessProofQuantifiesLiveChildAlternatives`
- `TestStrictlySorted_UniqueIndexFullCoverage`
- `TestStrictlySorted_UniqueIndexPartialCoverage`
- `TestStrictlySorted_UniqueStorageKeyMustMatchLogicalEquality`
- `TestStrictlySorted_NonUniqueIndex`
- `TestPlanner_NaNBarrierPrefixIsOrderedButNotStrict`
- `TestPlanner_StrictlySorted_UniqueIndex` / `_NonUniqueIndex`
- `TestSecondaryUniqueKeyGloballyEnforced`
- `TestTupleKeyUniquenessMatchesLogicalEquality`
- `TestFDB_RawNaNPrimaryKeySuffixRetainsLogicalSort`
- `TestFDB_UniquePendingIndexDoesNotEliminateDistinct`
- `TestFDB_NonReadableIndexIsNotAMatchCandidate`

Edited, with the edit named up front so it is reviewed and not discovered:

- `TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof` — its `TAGS`,
  `SCORE` and `CITY` arms keep asserting non-elision (each for a reason
  independent of this RFC); its `EMAIL` arm inverts, and the test is renamed to
  say what it now pins. Its PK positive control stays: without it, a test
  asserting non-elision passes with the whole rule inert.
- `index_state_planning_test.go`'s `proofOnly` arm — folded into a test of the
  stamp walk (§5.2).

## 9. Alternatives rejected

**(a) Keep the decline.** The status quo trade — "declining costs a DISTINCT that
was redundant, assuming costs duplicate rows" — was correct while assuming was
unguarded. It no longer is: the candidate set is state-filtered and the
dependency is revalidated per execution. Keeping it now costs the measured
overhead in §2 permanently, in exchange for a hazard that two landed mechanisms
already close. A conservative leftover that outlives its reason is technical debt
with a good story.

**(b) A planner-global side-channel accumulator** (the literal reading of #616's
`proofOnly []string`). Rejected twice over: it drops the dependency on every
plan-cache hit (§5.2), and it records proofs from expressions that lost on cost,
which over-scopes the dependency set — the same self-inflicted outage
`index_state_planning.go` rejects whole-snapshot signatures for.

**(c) Sign the whole index-state snapshot instead of scoping the dependency.**
Already rejected by #616 and the reasoning is unchanged: every query against a
store holding any `WRITE_ONLY` index would `40001`, including the queries whose
whole point is that they still answer correctly from a slower plan.

**(d) Widen candidate admission to `isScannable()` and add a uniqueness-specific
state check at the proof site.** This is the shape that looks like it makes the
danger explicit and instead makes it fragile. It is a bolted-on
`if state == X { refuse }` (principle 10), it puts a second authority on a
question the candidate filter already answers, and it diverges from Java's
structural route the moment either side changes. Java's own planner contains zero
checks of `READABLE_UNIQUE_PENDING`, and that is the design, not an oversight.

**(e) An executor-side per-leaf index-state backstop.** Cannot work, and the
reason is the defining property of this optimization: an elided `DISTINCT` yields
a plan that never reads the proving index, so no leaf is ever checked. This is
recorded in the decline comment and witnessed by
`TestFDB_UniquePendingIndexDoesNotEliminateDistinct` firing on a base-record
scan.

**(f) Derive the proof from `Ordering.isDistinct`, following Java's
`strictlySorted` chain.** That chain dead-ends in Java:
`RequestedOrdering.Distinctness.DISTINCT` is never constructed in `src/main`
(§3), so the four `requestedOrdering.isDistinct()` branches are unreachable.
Building Go's proof on a path Java has never executed would mean inheriting an
untested mechanism and calling it parity.

**(g) A plan-visible no-op `ProvenDistinct` operator instead of a stamp.** It
would also make the dependency a function of the plan and would read very well in
`EXPLAIN`. Rejected because it adds an executor node and a plan type for
provenance that carries no execution behaviour, and every consumer of the plan
tree — cost model, ordering derivation, push rules, continuation identity — would
have to learn to see through it. A stamp on an existing node needs none of that,
and `WithStrictlySorted` is the precedent for stamping a proved fact onto a plan
in this codebase.

**(h) Fix the hash-distinct continuation instead.** The strongest competing
proposal, because §2.1 shows most of the measured cost is the seen-key set being
serialized into every page's continuation and rebuilt on resume — O(P²) key
copies across P pages. Repairing that would help *every* `DISTINCT` query, not
only the ones over a unique column, which is more value per line of change.

It is not an alternative to this RFC, and the two do not overlap in the way they
appear to. Even with a perfect continuation, running a dedup that has been
*proved* to remove nothing still costs the measured +17% single-page overhead and
still holds O(distinct values) in memory for the duration. Proving an operator
unnecessary and not running it is a strictly stronger outcome than running it
efficiently.

But the finding is real and independent of whether this RFC is accepted, and it
is recorded here rather than left in a measurement transcript: **the hash-distinct
continuation's cost grows quadratically in page count**
(`executor.go:2261-2315`, `distinct_stream.go:259-271`).

**R3 shrinks that quadratic term without removing the need for the general fix,
and the distinction is worth stating precisely so neither is mistaken for the
other.** The O(P²) cost is O(P²) *in the number of retained keys*: the seen-set is
serialized into every page and rebuilt on resume. R3 narrows the retained set to
the exempt subset, so on a unique-column `DISTINCT` over an ordinary table the
constant in front of the P² collapses toward zero and the term stops mattering —
which the NULL-density sweep in §2.1 measures directly (the 0% row's churn
column). What R3 does **not** do is change the algorithm: at 100% NULL density
the term is back in full, and every `DISTINCT` that is not over a unique column
is untouched. R3 is a narrowing of one input to the problem; §9(h)'s work is the
fix to the problem. Neither substitutes for the other, and R3 landing must not be
read as retiring the item.

**That work is in flight now**, on branch `fix/distinct-continuation-growth` (PR
number to follow). It is tracked there rather than here, and the two are
independent in both directions: RFC-210 does not wait on it, and it does not wait
on RFC-210. The probe committed with this RFC is a reproducer for both — which is
also why §7.5's allocation criterion is written against the no-`DISTINCT`
baseline rather than against today's absolute MiB figure, so it stays meaningful
after that fix lands and moves the baseline.

**(i) Extend the proof to joins and unions.** Out of scope, and not because it is
hard: a `UNIQUE` index proves nothing about a join output, so the extension would
need a genuinely different argument. Stating the boundary is cheaper than
discovering it in a bug.

## 10. Review

**Graefe: ACK with conditions**, on the three questions this RFC was put up to
answer — §5.1's admission predicate, §5.2's plan-carried dependency (including
that it reverses one shape of #616 a commit later), and §5.5's migration being
scoped into this RFC rather than deferred. His measurements reproduced §2.1.

The conditions are folded into the text above rather than listed as an appendix,
because a condition kept beside the design is a condition the implementer reads.
Where each landed:

| # | condition | folded into |
|---|---|---|
| C1 | admission authority is `candidatePreservesBaseRecordCardinality`, not `createsDuplicates`; sparseness is its own clause; clause 1 states its operational route | §5.1 clauses 1, 3, 4; §7.2 sparse row |
| C2 | stamp only when the secondary-`UNIQUE` proof is the SOLE license | §5.2; new §7.3 criterion |
| C3 | carried completeness must not survive prefixing | §5.5; §8 sufficiency note |
| C4 | `isUnique(Index)` overload family; derived-side-alive / requested-side-dead reframing; `ImplementDistinctRule`'s own fragility caveat | §3 |
| C5 | the third `eval` check (`isReadable`); `QueryPlan.java` is in `fdb-relational-core`; cache-bypass argument; `aliasMinted` prohibition quoted | §5.2, §5.3 |
| C6 | paging delta quoted as a band | §1, §2.1 |
| C7 | name the in-flight O(P²) work | §9(h) |

Three of these — C1, C2, C3 — are blocking on the RFC text, and all three are
above. C1 and C3 are wrong-rows conditions; C2 is a spurious-failure condition.

**Torvalds: NAK, resolved.** He implemented §5.1 clauses 1-3 as specified and
reproduced **duplicate rows** on
`TestFDB_UniquePendingIndexDoesNotEliminateDistinct` — which is the strongest
possible form of review, and the reason this RFC's central reduction is now
§5.1.1 rather than a sentence in clause 1. The NAK was correct and the design is
better for it: the fix (a third index-state epistemic state, zero value = unknown
= may scan but may not prove) makes every *unwritten* planning path safe by
construction, which the original could not claim even after being patched.

One steer was adjusted rather than adopted verbatim, and it is flagged for the
delta-read: gating on "a restricted allow-list is present" is broken, because
`readableIndexesFrom` returns the **unrestricted** form exactly when the store is
healthy — the gate would fire only while some unrelated index was mid-build.
§5.1.1 keeps the fail-closed intent and moves it onto affirmative evidence.

| item | disposition |
|---|---|
| **BLOCKER**: candidate ⇏ `READABLE` on non-generator paths | §5.1.1 — new subsection; clause 1 rewritten |
| decision (b) + (c), both | §5.1.1 (b, refined to a tri-state) and §7.3 (c, on the existing SQL-path test) |
| §7.3 note wrong twice (`TAGS` `createsDuplicates=true`; `SCORE` via clause 5) | §7.3 — replaced with a per-arm table; conclusion unchanged |
| measurement bands (+88% third point; 15.3x) | §2.1 — `≥+68%`, `1.68x-1.88x`, `15x-21x` |
| ordered shape is noise-dominated, not "no difference" | §2.1 — spreads quoted, claim weakened |
| §7.5 must say the probe is REWRITTEN to assert | §7.5 — stated as implementation work |
| citations: `OrderingProperty.java:596-604`, `:52-57`, `:185-192` + 4th sentence, `:272`, #635/#653 | §3, §5.5 |
| §7.2 fixture reality (NaN spelling, fan-out unit-level, `AS SELECT` forms, sparse smoke check) | §7.2 |

**Second refutation → binding ruling: the arm as specified could not fire on any
SQL query.** A reachability probe (`secondary_unique_proof_reachability_test.go`)
established that the SQL DDL rejects a `NOT NULL` scalar column, so every
SQL-expressible secondary unique index has a nullable key and clause 8 was false
for all of them — the arm was correct and inert, and *both* consumers of the
shared gate were dead on the SQL surface, not just this one.

The ruling declined a three-way fork and unified two of its branches: the
metadata route and the residual route are two halves of one design, and deferring
either was deferral with a scoping story. Where each condition landed:

| # | condition | folded into |
|---|---|---|
| R-1 | clause 8 rewritten as the exempt-set proof; `SecondaryUniqueKeyGloballyEnforced` becomes R1's implementation, never relaxed | §5.1 clause 8, §5.1.2, §5.4 |
| R-2 | R2's allow-list, fail-closed, `NotDistinctFrom` refused; per-key-column top-level conjuncts strictly between scan and `DISTINCT`; the UNKNOWN-rows-dropped pin | §5.1.2 R2, §7.6 |
| R-3 | R3's narrowing flag is part of plan identity | §5.1.2 R3(1), §7.4 |
| R-4 | R3 renders distinctly in `EXPLAIN` | §5.1.2 R3(2), §7.1 |
| R-5 | §5.2's dependency + `40001` path apply unchanged to all three routes | §5.1.2 R3(3), §5.3 |
| R-6 | the refutation test is REWRITTEN, never deleted | §7.6 |
| R-7 | #617's dead strict-ordering half is fixed here, no separate item; no R3 analogue; Java's `RemoveSortRule.java:153` recorded as an upstream soundness bug | §5.7 |
| R-8 | §2.1 restructured and re-measured — cost-of-operator vs delivered-delta, NULL-density sweep | §2.1, §7.5 |
| R-9 | §3 must say FIRST that Java cannot plan `DISTINCT` at all | §3 |

Remaining gates: **Torvalds delta-read** on §5.1.1; **Graefe** on the
implementation lap, one lap at completion of the whole arm; codex + `@claude` at
implementation completion.
