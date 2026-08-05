# RFC-210 — A secondary UNIQUE index may prove DISTINCT elision

- **Status:** DRAFT — Graefe ACK **with conditions**; C1-C7 folded into this text.
  Implementation gated on kickoff; the implementation lap is a separate Graefe
  gate.
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
planner can already state — and the index it rests on is recorded as a
**plan-carried dependency**, so that an index whose state moves out from under a
live statement invalidates the plan with `40001` instead of silently returning
duplicate rows.

Two things this RFC deliberately does *not* do. It does not widen the candidate
set: the proof is admissible **only** for an index that is already a match
candidate, which is already exactly the strictly-`READABLE` set (§4.1), so
`READABLE_UNIQUE_PENDING` is excluded structurally and no uniqueness-specific
state check is written anywhere. And it does not add a third derivation of "does
this storage key's uniqueness survive logical equality" — it makes the existing
two into one carried property (§5.5).

The measured benefit is narrower than the phrase "use the unique index" suggests,
and §2.1 leads with the refutation: the elision removes an operator, it does not
change the access path. Its value is concentrated in one regime — a paged,
unordered `SELECT DISTINCT <unique column>`, where the redundant dedup is ~68-72% of
the query's runtime.

## 2. What is declined today, and what it costs

`rule_implement_distinct_final.go`'s `distinctEliminatedByUniqueKey` proves
`DISTINCT` redundant from **primary-key** coverage only. Its closing paragraph
declines the secondary-`UNIQUE` case and — since #617 — records accurately that
both preconditions which originally forced the decline have closed:

> So the decline is now a CONSERVATIVE LEFTOVER, not a necessity […] Lifting it
> is a real behaviour change (it decides when a general-purpose rule fires) and
> needs its own RFC and review; it is deliberately NOT done here.

This RFC is that review.

### 2.1 The measurement, and what it refutes

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
scan — a 21x gap at 100k rows, because the index path fetches a base record per
entry. The cost model's preference for the base scan is empirically right. (That
21x is a real and separate finding about the index access path; it is not this
RFC's subject and no claim here rests on it.)

**What the decline actually costs**, then, is one redundant operator — and the
operator is more expensive than its CPU suggests:

| shape | rows | with `DISTINCT` | without | delta |
|---|---|---|---|---|
| unordered, single page | 100 000 | 388 ms | 333 ms | **+55 ms (+17%)** |
| unordered, **paged** (`EXECUTION_SCANNED_ROWS_LIMIT=10000`, ~10 pages) | 100 000 | **559 ms** | **328 ms** | **+235 ms (+68-72%, band across runs)** |
| paged allocation churn | 100 000 | **424.9 MiB** | 157.7 MiB | **+267 MiB** |
| ordered (streaming variant) | 100 000 | 6.95 s | 7.05 s | below noise (±300 ms) |
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

**And the honest negatives, all measured:** the `DISTINCT` does not block a
covering-index-only plan (there is none for either query), does not force a
base-record fetch the other shape avoids, and does not defeat early termination —
`Limit(10, Distinct(…))` and `Project(Limit(10, …))` both return in 5-6 ms over
100k rows because the cursors are pipelined. On the ordered shape the streaming
variant's cost is below the noise floor at this scale.

So the case for lifting is narrower than it first looks and rests on one regime:
**a paged, unordered `SELECT DISTINCT <unique-column>`, where the redundant
operator costs ~68-72% of the query's runtime and an allocation load that grows with
the number of distinct values.** That regime is the ordinary one for a
result-set-paginating client over a unique column, which is exactly the query a
`UNIQUE` index invites.

## 3. Java, read first — Java derives the fact and never lets it decide

The Java Cascades planner never uses a secondary `UNIQUE` index to eliminate a
`DISTINCT`. It does, however, *derive* uniqueness-based distinctness and carry it
in the ordering property — so the accurate finding is not "Java lacks the
concept" but "Java computes the concept and no path connects it to this
decision". The distinction is drawn precisely below, because it changes what this
RFC is: not parity work, but a read-side extension that connects two things Java
keeps apart.

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
(`DistinctRecordsProperty.java:~265`, `ValueIndexScanMatchCandidate.java:212`):

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
`Verify.verify(leftOrdering.isDistinct())`, and `:596-604` uses it in the flatMap
concat case. Java *does* derive distinctness from a unique index.

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
(`ImplementDistinctRule.java:50-57`):

```java
 * <p>
 * This rule is somewhat suspect. In particular, if the inner plan that it matches against does not produce duplicates,
 * this rule will then return that plan. This is fine unless the plan is later modified in such a way that it then
 * <em>can</em> produce duplicates. … To address that, the plan is to add a mechanism for enforcing properties (e.g.,
 * distinctness or sort order) on the plans produced by the planner. See Issue #635.
 * </p>
```

Java describes eliding a distinct on an unenforced property as *suspect*, and the
fix it names — a mechanism that makes the plan carry the property it was elided
under — is structurally what §5.2's plan-carried stamp does for the index-state
dimension.

**Conformance position.** The principle is *doesn't work in Java → doesn't work
in Go*, and it governs the **shared query surface** — the answers, not the plans.
`SELECT DISTINCT email FROM users` works in Java and in Go and returns the same
rows; this RFC changes only which physical plan Go picks to produce them. It
adds no expressible query, no syntax, no operator semantics. It is therefore not
a divergence on shared semantics at all — it is a plan choice, the same category
as choosing an index scan over a full scan. The rows a Java application reads
back are byte-identical (§6).

It is worth being explicit that Go's **existing** primary-key arm is already
outside Java's mechanism for the same reason: Java has no projected-column
distinctness proof of any kind. The secondary-`UNIQUE` arm is not a new class of
extension; it is a second arm on one Go already ships and tests.

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

**So this RFC writes no state check.** Admissibility is "the index is a match
candidate", and the state filter is what makes that mean strictly `READABLE`. A
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

1. **`I` is a match candidate for `R`.** Which, by §4.1, means strictly
   `READABLE`. Prevents: trusting the `unique` flag of a
   `READABLE_UNIQUE_PENDING`, `WRITE_ONLY` or `DISABLED` index.

   **The operational route is part of the clause, not an implementation
   detail.** The rule asks `call.Context.GetMatchCandidates()` and nothing else.
   That list is the one built in `cascades_generator.go:2510-2560`, where the
   `ReadableIndexes.Allows` filter (`:2548`) has already removed every
   non-`READABLE` index *before any candidate is created*. Stating the route
   matters because an unstated one is exactly where a second, unfiltered
   authority gets invented — an implementation that reaches for
   `md.GetAllIndexes()` or `store.GetReadableIndexes()` (which, by its own
   doc at `store_api.go:33-35`, **includes** `READABLE_UNIQUE_PENDING`) would
   satisfy every word of this clause's first sentence and reintroduce the exact
   bug the decline existed for.

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

8. **`SecondaryUniqueKeyGloballyEnforced` over `I`'s authoritative physical key
   component types** (§5.4): every key component `NOT NULL` and none
   `FLOAT`/`DOUBLE`/unknown.

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

The last row is a theorem, not a rule anyone maintains: an index that was not
readable at planning was not a candidate, so no proof rests on it, so it is not
in the dependency set, so its becoming readable is never examined. Java lands in
the same place by the same structural route (`RecordStoreState.compatibleWith`
iterates only the non-readable exceptions).

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

### 5.4 Float and NULL safety (#617's gates, reused verbatim)

Both hazards make a *unique storage key* fail to be a *unique logical row*, in
opposite directions, and #617 already built the predicate for both.

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
elimination and strict-ordering claims"* — and today only the strict-ordering
half exists (`indexHasGloballyEnforcedUniqueKey`, `rule_implement_sort.go:317`).
The DISTINCT half of that sentence has been aspirational since #617 landed it.
This RFC makes it true, which is a reason to expect the predicate to fit rather
than to need widening: an implementation that finds itself relaxing
`SecondaryUniqueKeyGloballyEnforced` to make the arm fire should treat that as
evidence the arm is wrong, not the predicate.

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
the field's own javadoc (`Ordering.java:184-191`):

```java
/**
 * Indicator if the records flowed are to be considered distinct, thus producing a strict order. This field
 * should get deprecated as it is not correct to assign this property to all enumerated orderings. For instance,
 * an ordering that produces {@code a, b, c} is also ordered by {@code a, b}. While the former one might be strict,
 * the latter one may not. …
 */
private final boolean isDistinct;
```

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
  shows would be a 21x regression rather than a win.

The negative controls in §7.2 are what keep the first assertion from being
satisfiable by deleting the `DISTINCT` operator outright.

`SELECT DISTINCT email FROM users ORDER BY email` must likewise lose its
`Distinct(` while keeping `IndexScan(BY_EMAIL, [*])` — the elision must not
disturb an ordering the query asked for.

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

### 7.3 Index state

- **`TestFDB_UniquePendingIndexDoesNotEliminateDistinct` stays green,
  unmodified.** This is the single most important criterion in this RFC. It is
  the end-to-end witness for the exact bug the decline was protecting against —
  a `READABLE_UNIQUE_PENDING` index whose `unique` flag the data contradicts —
  and it must pass *because the index is not a candidate*, not because the arm
  was declined. If it needs an edit, the lift is wrong.
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

Note on the first item, so an implementation does not mistake one refusal for
another: `TestFDB_UniquePendingIndexDoesNotEliminateDistinct` passes **today**
only because `distinctEliminatedByUniqueKey` ends in an unconditional
`return false`. All four of its negative arms have `createsDuplicates == false`,
so a predicate built from clause 3 alone would return *true* for `EMAIL` and the
test would go red on the first day of implementation. The decline's blanket
refusal is currently doing the work the admission predicate must do afterwards;
they are not the same refusal and the test does not distinguish them.

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

The probe from §2.1 is committed with this RFC and becomes the regression
harness. The criterion is stated on the regime the change is actually for, and
deliberately not on the regimes where §2.1 measured nothing:

- **Paged, unordered, 100k rows, all-distinct.** `SELECT DISTINCT email FROM
  users` must come within **1.15x** of `SELECT email FROM users` — today it is
  1.72x. Both sides run back-to-back on the same store so a slow moment lands on
  both, and the ratio is a median of 5 pairs, for the reason RFC-209 §7 sets out:
  single pairs on this harness drift enough to fail a clean run.
- **Allocation churn** on the same shape must come within **1.15x** of the
  no-`DISTINCT` run, today 2.7x. This is the criterion that would catch an
  implementation that elides the operator in `EXPLAIN` while some other path
  reintroduces per-page key retention.
- **No criterion on the ordered or `LIMIT` shapes**, because §2.1 measured no
  difference there — a bound on a delta that is below the noise floor measures the
  harness, not the change. They are still asserted for *correct rows and plan
  shape* in §7.1, just not timed.

The bound is 1.15x rather than 1.0x because the elision removes an operator, not
a scan: the two plans do the same I/O, and the residual is per-query fixed cost.
A criterion of "strictly no slower" would fail on noise while proving nothing
more.

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

Remaining gates: **Torvalds** (code quality) on this text; **Graefe again** on
the implementation lap; codex + `@claude` at implementation completion.
