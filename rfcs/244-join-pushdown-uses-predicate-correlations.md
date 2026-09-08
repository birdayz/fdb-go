# RFC-244: join pushdown uses the predicate's complete correlation set

## Finding and reference

At `c50ec7e53`, `PushFilterBelowJoinRule.predicateSingleSide` walks only
`ComparisonPredicate` and `ValuePredicate` field values, then disregards fields
whose resolved path has more than one accessor. That is not a conservative
decline: in `A.ID = B.N.ID` it sees A, ignores B, and returns side 0, authorizing
the whole predicate to move below the join to A, where B is unavailable.

`TestPushFilterBelowJoin_NestedDependencyStaysAboveJoin` reproduces this under
Bazel: `A.ID = B.N.ID classified as side 0; both correlations must keep it above
the join`. Three SQL forms (WHERE, reversed WHERE operands, and ON) in
`TestFDB_JoinFilterKeepsNestedDependency` return the correct two pairs against
real FDB on this head. These are negative controls, not evidence that the
faulty rewrite is unreachable for all SQL.

Java 4.12.11.0 `PredicatePushDownRule.onMatch` partitions by
`queryPredicate.getCorrelatedTo().stream().noneMatch(otherAliases::contains)`.
It does not infer dependencies from field depth or enumerate predicate node
classes. Go already exposes that transitive contract through
`QueryPredicate.GetCorrelatedTo` and `predicates.GetCorrelatedToOfPredicate`;
the generic `PredicatePushDownRule` uses it.

## Decision

Replace the local field/predicate walker with the predicate's full correlation
set. A predicate referencing A and B stays above the join, independent of how
deeply either dependency is nested or which predicate/value kind carries it.
A predicate referencing only one owned side remains eligible. As in Java,
external correlations are not sibling dependencies. Predicates referencing
neither owned side remain above the join, retaining this specialized rule's
existing conservative policy: this two-leg rule has no side to choose for a
predicate that references neither leg. Record this divergence in DIVERGENCES.md.
The specialized rule matches Filter(Select(...)); the Java-shaped generic rule
matches Select(...). They operate in the same memo, not separate query pipelines.
The specialized rule must use the same complete dependency contract.

Classify with the actual typed owned quantifier aliases, not identities
reconstructed from source labels: a unique-kind alias and a named-kind alias
can share a label without being the same identity. Require source labels to
agree with the parallel owned labels as a separate consistency check; decline
missing or mismatched labels. Duplicate owned identities naturally classify as
both/neither and decline; there is no redundant duplicate-identity guard.
Preserve the actual owned alias when wrapping a leg, as Java does. Keep the join-kind and strict-single barriers and also
refuse null-on-empty edges: wrapping either flagged edge with an unflagged
ForEach loses its semantics. This does not rework the separate generic rule.

Delete the walkers once their production caller is removed. The scoped search
is `rg -n 'walkPredicateFieldValues|walkValueForFieldValues' pkg --glob '*.go'`;
before removal its production hits were confined to rule_push_filter_below_join.go.

A prior real SQL defect had the same missing-dependency consequence:
`case_cross_table_predicate_fdb_test.go` pins CASE conditions hidden by
`predicateValue.Children` before its fix (`expr/walk.go`). That was a missing
carrier-child contract; this finding is a local classifier discarding children
whose correlations the shared contract already reports.

This changes logical rewrite admission, not wire formats, physical operators,
continuations, or costs. Restricting the missing-sibling cases restores the
rule's stated equivalence; recognizing a deeply nested single-side predicate
uses the same correlation proof as its top-level counterpart.

## Verification

* Unit-drive the decision with direct and nested fields on either/both sides,
  non-field correlation carriers, composite and range predicates, constants,
  and external correlations. Assert actual correlation sets as preconditions.
* Fire the rule on typed two-leg expressions and verify a two-side predicate
  is retained, including mixed pushable/residual predicates.
* Retain the real-FDB SQL controls with explicit row assertions; record plan
  shapes without claiming they reach this particular rule unless demonstrated.
* Run the Cascades and SQL-driver Bazel targets uncached, `just test`, the
  affected determinism checks, and a bounded planner fuzz run.
* Compare the physical-plan corpus and the 1M stress target before/after.
  A valid timing comparison cannot currently be made: the filesystem is 98%
  full and there is no owner authorization to delete unrelated data to free it.
  Run the stress target for correctness only; do not publish timing ratios or
  claim the performance gate passed. A valid performance comparison remains a
  pre-merge requirement, not a reason to leave this correctness defect unfixed.
* Pin the observed translator shape behind the passing SQL controls:
  translateFilter absorbs ordinary inner-join WHERE predicates into the join's
  SelectExpression, rather than constructing the Filter(Select) shape targeted
  by this rule. The ON spelling also starts inside Select. Assert that shape
  and its cross-leg dependency, without generalizing to every rewrite later
  performed by the optimizer.

## Review

RFC review: Graefe ACK and Torvalds ACK before implementation. The latter
requires the translator pin's failure message to name the re-armed specialized
pushdown path; the pin does so. Implementation review and verification follow.
These ACKs authorize implementation, not merging without the remaining gates.

## Final verification

Reference tree: `c50ec7e5313b45e968e9b196da5eebd191151810`. The modified Go/build
inputs were frozen through verification; the SHA-256 of the sorted-path MD5
manifest was `a9a7da9741dd076a1cb09ca11f9af777dd1779b1152d60dec84336365049f009`,
and every listed input was checked again after the runs.

* `just test`: 92 test targets pass, 45 executed and 47 cached. The changed
  Cascades, embedded and SQL-driver targets executed, including the new tests.
* Ten uncached repetitions of the focused Cascades and embedded targets pass.
* `FuzzPlanner_Determinism`: 15 seconds, 2,959,536 harness executions, PASS.
* EXPLAIN comparison: all 2,955 entries identical (2,783 queries and 172 DML
  across 372 scenario files); no plan-shape or plan-error-classification change.
* `TestFDB_Stress_1M`: two uncached runs per tree, each with 24 RUN lines
  (the parent plus 23 subtests). All four runs pass and agree on the 22 labelled
  query row-count readings. These are correctness results, not timing parity:
  filesystem utilization remained 98%, so no performance ratio is claimed.

### Corpus firing measurement

The requested corpus check does **not** show a formerly live rule becoming
dead. Over the full `//pkg/relational/core/embedded:embedded_test` target,
uncached Bazel coverage measured the same dispatch partition in both trees:
711 `OnMatch` invocations, 707 declined because the child was not a Select,
and four declined at the non-inner-join barrier. Both yield sites have count
zero in both trees. The changed classification body is therefore not reached
by that target on either tree. This is scoped to that target, not every SQL
query the engine can accept. The direct rule tests, including the unique-kind
alias case, independently require successful yields; the real-FDB SQL controls
and translator-shape pin intentionally make no such claim.

Reproduce the measurement in each tree with:

```sh
bazelisk coverage //pkg/relational/core/embedded:embedded_test \
  --instrumentation_filter='^//pkg/recordlayer/query/plan/cascades(:|/)' \
  --combined_report=lcov --nocache_test_results
```

Read the `rule_push_filter_below_join.go` record in
`bazel-out/_coverage/_coverage_report.dat`. At the reference tree, entry/early
returns/yields are lines 51/56/61/155/163; at the implementation they are
52/57/62/157/165. These are execution counts for individual blocks, not sums
across repeated line counters.

### Review and remaining merge requirement

Implementation findings were folded: typed quantifier identities replace
label reconstruction, alias-barrier tests use single-side predicates, the
redundant duplicate-identity guard is removed, and the translator pin checks
only the immediate Filter-over-two-leg-Select boundary. Graefe, Torvalds and
@claude ACKed the code delta; codex re-confirmed with no concrete regressions.
The corpus firing evidence above is recorded separately from those ACKs.

A valid timing comparison remains a pre-merge requirement. Freeing sufficient
space requires owner-authorized cleanup of unrelated data; no such deletion
has been performed. Nothing here claims merge authorization.
