# RFC-273: Planner determinism from the 2026-10-24 quality review

**Status: Draft**

## Problem

The 2026-10-24 quality review (TODO.md "Quality review 2026-10-24", Planner)
reported five determinism and gate findings:

1. `joinOptimizationProbesScenario` is out of the cross-engine gate
   (`conformance/yamsql_cross_engine_conformance_test.go`) on a claim that a
   3-way or arithmetic join returns varying row counts (TODO-production.md P2.3).
2. `MaxMatchMap` reads child match results from a Go map, and pruning keeps the
   first variant on a `maxDepth` tie, so map order can pick the match.
3. `FuzzPlanner_Determinism` compares only `len(ref.Members())`.
4. `DecorrelateValuesRule` pushes values boxes in map order.
5. No corpus-wide plan comparison with Java: the javacorpus runner skips every
   `explain`/`explainContains`/`planHash` directive.

## Investigation

1. The scenario's six queries are two-table joins. Its cross-engine twin pins
   Eve for `e.salary > d.did * 50`, but 95 > 150 is false; the yamsql twin
   dropped her in RFC-180. Re-included, Java 4.14.2.0 rejects all six with
   `UnableToPlanException` (0AF00): it has no in-memory sort and none of the
   ORDER BYs is index-provided. No run varied: `yamsql_test` x20, the
   cross-engine scenario x5 (Java and Go agree), three-way arithmetic variants,
   and the same queries under two disabled-rule sets that force nested-loop joins,
   full scans and other join orders. Writing the variants also showed that both
   grammars fold `+ - * /` flat and left-associatively: `b.salary + d.did * 5`
   is `(b.salary + d.did) * 5`, which returns no rows in either engine. This is
   the documented Java quirk, not a Go bug.
2. Java's `BestMatches` keeps a `LinkedHashMap` when not pruning and keeps the
   first of tied variants when pruning (`MaxMatchMap.java:813-890`); the parent
   iterates the child map's `entrySet()` into `CrossProduct`, so Java's winner is
   the first variant found. Go's `recurseQueryResultValue` returns
   `map[values.Value]matchResult` and iterates it at the cross product, the
   expansion merge and the top-level pick. A shape with two depth-0 variants of
   a child (a record QOV and its `ExpandRecordRule` expansion, both reachable in
   the candidate) under a parent whose matcher sees a sibling picks the
   expansion about half the time.
3. Two plannings that build the same number of members in a different order, or
   pick a different winner, pass the fuzzer.
4. Java builds `qunsToPushDown` with `ImmutableMap.builder()` in binding order,
   and `PushValuesIntoVisitor` adds the boxes in that order
   (`DecorrelateValuesRule.java:206-339`). Go uses a map.
5. 1,266 directives (1,214 `explain`, 52 `explainContains`, no `planHash`) in
   87 of 255 files. The formats differ (no Java-format renderer, RFC-201 ruling
   2), but an access-path normalizer exists in a test file.

## Fix

1. Re-include the scenario. Fix the Eve pin; assert rows unordered where Java
   cannot plan the ORDER BY, group the aggregate by the indexed `e.did`, and keep
   one index-ordered ORDER BY. Add the three-way probes and the flat-precedence
   case to the yamsql twin, and pin two forced-plan twins
   (`join_optimization_probes_scan_only`, `_nested_loop`) whose plan assertions
   prove the alternative plans run. Close the P2.3 row-count bullet.
2. `recurseQueryResultValue` returns `matchResults`, a slice in insertion order
   with Java's `put` semantics (a variant already present keeps its position);
   the memo holds the same slices. The cross product, the expansion merge and the
   top-level minimum read them in order.
3. The fuzzer compares a rendering of every reachable Reference's members (type,
   result value, predicates, child references; generated aliases renumbered) and
   the chosen physical plan's EXPLAIN across two fresh plannings.
   `TestRepeatedPlanningIsStable` (explaindiff) plans each corpus statement whose
   plan joins, unions, intersects, iterates an IN list or reads an index ten more
   times and requires one EXPLAIN.
4. `qunsToPushDown` becomes a slice in quantifier order.
5. Measurement and a numbered proposal in TODO.md; no harness in this RFC.

## Performance

`matchResults.put` scans the variant list (a few entries; the cross product's
`computeForCurrent` per combination dominates). The decorrelation slice is
smaller than the map it replaces. No plan in the corpus golden changes.

## Test plan

- `TestComputeMaxMatchMap_TiedVariantsKeepTheFirstFound`: 64 computations of the
  tie shape must match the unexpanded query (red before: the expanded variant
  wins on run 0).
- `TestDecorrelateValuesRule_PushedQuantifierOrderFollowsTheSelect`: four boxes
  pushed into a select and under a DISTINCT keep the select's order across 32
  firings (red before: permuted on firing 0).
- `FuzzPlanner_Determinism`: with `Reference` insertion order randomized as a
  mutation, the new oracle fails and the member-count oracle passes.
- `TestRepeatedPlanningIsStable`: at least 1,000 statements replanned.
- `join_optimization_probes*.yaml` in `yamsql_test`; the scenario in the
  cross-engine gate (`conformance_corpora_test`, full lane).
