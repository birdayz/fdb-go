# RFC-273: Planner determinism from the 2026-10-24 quality review

**Status: Implemented**

## Problem

The 2026-10-24 quality review (the "Quality review 2026-10-24" section PR #994
adds to TODO.md; Planner) reported seven planner findings. This RFC takes the
five about determinism and the Java gate, under the review's numbers:

1. `joinOptimizationProbesScenario` is out of the cross-engine gate
   (`conformance/yamsql_cross_engine_conformance_test.go`) on a claim that a
   3-way or arithmetic join returns varying row counts (TODO-production.md P2.3).
2. `MaxMatchMap` reads child match results from a Go map, and pruning keeps the
   first variant on a `maxDepth` tie, so map order can pick the match.
3. `FuzzPlanner_Determinism` compares only `len(ref.Members())`.
5. `DecorrelateValuesRule` pushes values boxes in map order.
7. No corpus-wide plan comparison with Java: the javacorpus runner skips every
   `explain`/`explainContains`/`planHash` directive.

Items 4 (rule fan-out against `MaxNumMatchesPerRuleCall`, cancellation inside a
rule) and 6 (`query/executor` importing `relational/api`) follow in a separate
PR.

## Investigation

1. The scenario's six queries are two-table joins. Its cross-engine twin pins
   Eve for `e.salary > d.did * 50`, but 95 > 150 is false; the yamsql twin
   dropped her in RFC-180. Re-included, Java 4.14.2.0 rejects all six with
   `UnableToPlanException` (0AF00): it has no in-memory sort and none of the
   ORDER BYs is index-provided. No run varied: `yamsql_test` x20, the
   cross-engine scenario x5 (Java and Go agree), three-way arithmetic variants,
   and the same queries under two disabled-rule sets: one forces full scans and
   other join orders for every probe, the other nested-loop joins for the
   two-way probes (the three-way plans do not change under it). Writing the variants also showed that both
   grammars fold `+ - * /` flat and left-associatively: `b.salary + d.did * 5`
   is `(b.salary + d.did) * 5`, which returns no rows in either engine. This is
   the documented Java quirk, not a Go bug.
2. Java's `BestMatches` keeps a `LinkedHashMap<Value, MatchResult>` when not
   pruning and keeps the first of tied variants when pruning
   (`MaxMatchMap.java:813-890`); `Value.equals` is `semanticEquals`, so a
   structurally equal variant replaces the stored result in place. The parent
   iterates the child map's `entrySet()` into `CrossProduct`, so Java's winner is
   the first variant found. Go's `recurseQueryResultValue` returns
   `map[values.Value]matchResult`, keyed by identity, and iterates it at the
   cross product, the expansion merge and the top-level pick. Only the cross
   product can see a tie: the top level and every memoized result prune to one
   entry, and the expansion merge feeds the cross product. A shape with two
   depth-0 variants of a child (a record QOV and its `ExpandRecordRule`
   expansion, both reachable in the candidate) under a parent whose matcher sees
   a sibling picks the expansion on run 0 of 64.
3. Two plannings that build the same number of members in a different order, or
   pick a different winner, pass the fuzzer.
5. Java builds `qunsToPushDown` with `ImmutableMap.builder()` in binding order,
   and `PushValuesIntoVisitor` adds the boxes in that order
   (`DecorrelateValuesRule.java:206-339`). Go uses a map.
7. In the vendored 4.14.2.0 corpus
   (`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/**/*.yamsql`,
   255 files, parsed as YAML; a grep for the keys agrees): 1,214 `explain` and
   52 `explainContains` directives in 87 files, and no `planHash` (a grep for the
   key finds no file). The formats differ (no Java-format renderer, RFC-201
   ruling 2), but an access-path normalizer exists in a test file.

## Fix

1. Re-include the scenario. Fix the Eve pin; assert rows unordered where Java
   cannot plan the ORDER BY, group the aggregate by the indexed `e.did`, and keep
   one index-ordered ORDER BY. Add the three-way probes and the flat-precedence
   case to the yamsql twin, and pin two forced-plan twins
   (`join_optimization_probes_scan_only`, `_nested_loop`) whose plan assertions
   prove the alternative plans run. explaindiff plans each scenario under its
   `connection_options`, so the golden and the replanning test see those plans. In TODO-production.md, close the P2.3
   row-count bullet and update the open-gap count in the summary and the
   priority list's "promoted nondeterminism". The exclusion comment was the
   only RFC-042 tracking; it goes with the re-inclusion.
2. `recurseQueryResultValue` returns `matchResults`, a slice in insertion order
   keyed as Java's map is: `put` of a variant structurally equal to a stored one
   (`values.ValuesStructurallyEqual`) replaces its result and keeps the first
   variant and its position. The memo holds the same slices. The cross product,
   the expansion merge and the top-level minimum read them in order.
3. The fuzzer compares a rendering of every reachable Reference's members (type,
   result value, predicates, child references; generated aliases renumbered) and
   the chosen physical plan's EXPLAIN across two fresh plannings.
   `TestRepeatedPlanningIsStable` (explaindiff) plans each corpus statement whose
   plan joins, unions, intersects, iterates an IN list or reads an index ten more
   times and requires one EXPLAIN.
5. `qunsToPushDown` becomes a slice in quantifier order.
7. The measurement and a numbered proposal go in TODO.md; no harness here.

## Performance

`matchResults.put` compares the new variant with each stored one (a few entries;
`computeForCurrent` per cross-product combination dominates). The decorrelation
slice is smaller than the map it replaces. Acceptance: no existing entry of
`explaindiff/testdata/plan_shape.golden` changes.

## Test plan

- `TestComputeMaxMatchMap_TiedVariantsKeepTheFirstFound`: 64 computations of the
  tie shape must match the unexpanded query (red before the fix).
- `TestMatchResults_PutKeysByStructuralEquality`: a structurally equal variant
  replaces the stored result in place.
- `TestDecorrelateValuesRule_PushedQuantifierOrderFollowsTheSelect`: four boxes
  pushed into a select and under a DISTINCT keep the select's order across 32
  firings (red before the fix).
- `FuzzPlanner_Determinism`: with `Reference` insertion order randomized as a
  mutation, it fails and the old member-count fuzzer passes.
- `TestRepeatedPlanningIsStable`: with the cost model's final hash tie-break
  made a coin flip, it fails (`cross_join.yaml#0` and others plan to two
  EXPLAINs). Member order alone does not move any corpus plan. Both tests are
  forward guards: the corpus has no plan that items 2 and 5 change.
- `join_optimization_probes*.yaml` in `yamsql_test`; the scenario in the
  cross-engine gate (`conformance_corpora_test`, full lane).
