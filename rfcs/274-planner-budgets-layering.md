# RFC-274: Planner budgets and record-layer layering from the 2026-10-24 quality review

**Status: Implemented**

## Problem

The 2026-10-24 quality review (the "Quality review 2026-10-24" section PR #994
adds to TODO.md; Planner) reported, besides the determinism items RFC-273 fixes:

4. The context is checked only between tasks, and the fan-out inside one rule is
   not counted against `MaxNumMatchesPerRuleCall` (`physical_wrapper.go:250-265`,
   `planner.go:490-530`).
6. `pkg/recordlayer/query/executor` imports `pkg/relational/api`
   (`executor.go`, `executor_new_plans.go`, `resultset.go`, `scalar_subquery.go`);
   Java's record layer does not depend on relational.

## Investigation

### 4a. Cancellation

The claim is wrong as stated. `TransformExprTask` and `TransformImplTask` check
the context before each binding and after `OnMatch` and drop the call's staged
yields and inserts when it is cancelled (`unified_tasks.go`); `Yield` is a no-op
once cancelled (`expression_rule_call.go:152`, `implementation_rule.go:125`); the
rules with a large internal fan-out check `CancellationErr()` in their loops
(`rule_implement_in_join.go`, `rule_partition_select.go`,
`rule_predicate_to_logical_union.go:100`);
`TestPlanner_MidTaskCancellationStopsRemainingBindings` pins the per-binding
check. Java's `CascadesPlanner` has no cancellation at all. What is left: after a
cancel, a rule's loop keeps searching and growing the memo through
`ExpressionRuleCall.MemoizeExpression(s)` and `MemoizeUnknownExpression` (whose
physical arm indexes the reference directly), and
`ImplementationRuleCall.MemoizeUnknownExpression` delegates without the run
context, so a check there would never fire.

### 4b. Matches per rule call

Java counts every binding of the rule's matcher, one `++numMatches` before each
`executeRuleCall` and so before any guard in `onMatch`, and throws
`RecordQueryPlanComplexityException` at the first binding over the bound
(`CascadesPlanner.java:1073-1086`). Java's matchers fan out: one binding per plan
partition (`planPartitions(any(anyPlanPartition()))`, e.g. `ImplementFilterRule`),
per stored-record partition (`filterPlanPartitions(storedRecord, …)`, e.g.
`ImplementDeleteRule`), per combination (`PartitionSelectRule`), per quantifier
(`PushRequestedOrderingThroughSelectRule`) and per permutation for set matchers
(`exactlyInAnyOrder`: `ImplementNestedLoopJoinRule`, `PartitionBinarySelectRule`).
Go's matchers bind once at the root (`expression_matcher.go:51-56`) and the rule
enumerates in `OnMatch`, so Go counts 1 where Java counts the fan-out: a bound of
2 or more never trips. Go also counts the planner's swapped-quantifier firing
for every rule, and reports the full eager count where Java's is bound+1. The
loops over `physicalMembersForParentEnumeration` the item cites are not a
divergence: Java's `ImplementStreamingAggregationRule` binds the rolled-up
partitions once (`atLeastOne`) and loops in `onMatch`, and in-memory sort is
Go-only.

### 6. Layering

The executor builds `api` errors at 16 sites (13 in `executor.go`, one in
`executor_new_plans.go`, two in `scalar_subquery.go`), and `resultset.go` is an
`api.ResultSet` over an executor cursor: Java's `RecordLayerResultSet` lives in
fdb-relational-core. Java's record layer throws a `SemanticException` carrying an
`ErrorCode`, and relational's `ExceptionUtil` maps it to an SQLSTATE. The
relational layer already maps typed executor errors in one place,
`translateExecError` (`cascades_generator.go`), which every error from
`ExecutePlan` and `EvaluateScalarSubquery` passes through.

## Fix

### 4a

`ExpressionRuleCall.MemoizeExpression`, `MemoizeExpressions` and
`MemoizeUnknownExpression` skip the memo once the call is cancelled (returning an
unregistered reference), and `ImplementationRuleCall.MemoizeUnknownExpression`
passes its run context to the call it delegates to. The yields are dropped
already, so no plan changes.

### 4b

- A rule whose Java matcher fans out implements `javaBindings(expr) int`, Java's
  count for that expression (`rule_java_bindings.go`): ImplementFilter,
  ImplementTypeFilter, ImplementDistinct(Final), ImplementSimpleSelect,
  ImplementInsert, ImplementTempTableInsert, ImplementDelete, ImplementUpdate,
  ImplementSort (Java's RemoveSortRule), PartitionSelect,
  PushRequestedOrderingThroughSelect and its existential twin, NestedLoopJoin and
  PartitionBinarySelect (both orders).
- The task drivers count once per rule call, before `OnMatch` and whatever its
  guards reject: `javaBindings` where the rule has it, else the Go bindings. The
  swapped firing adds nothing, since a set-matcher rule reports both orders. The
  count is computed only when the bound is set; past it the call fails with
  `ErrPlannerRuleMatchCapHit`, observing bound+1.
- The remaining differences go in DIVERGENCES.md: rules that take the first
  member where Java binds each (fetch push-downs, DistinctUnion, the IN rules'
  explode sources, DecorrelateValues, Intersection, RecursiveDfsJoin),
  PredicateToLogicalUnion (Java fires it per match partition), and Go-only rules.
  The `physical_wrapper.go` and task comments are updated.

### 6

- `executor.ExecutionError{Kind, Message}` replaces the executor's `api` errors;
  `translateExecError` maps each kind to the SQLSTATE the executor built before
  (22000, 23502, 21000, 42601, XX000). Range-bound and enum/UUID failures return
  their `values` errors, mapped there with the same code and message.
- `resultset.go` and its tests move to `pkg/relational/core/resultset`. The
  executor tests that asserted `*api.Error` codes assert `ExecutionError` kinds;
  of the four FDB tests that read rows through the result set, two read the
  ordinal rows instead and the string-coercion and by-name ones are dropped
  (those accessors stay unit-tested with the result set); the continuation
  encode-failure check moves with it.
- A nogo analyzer, `recordlayerdeps`, fails the build when a non-test file under
  `pkg/recordlayer` imports `pkg/relational`. Test files are exempt: two executor
  tests compare the record-layer converters with the relational ones
  (`relational/core/functions`, `rowstruct`).

## Performance

4a adds a context read per memoize call. 4b computes nothing unless the bound
is set; with it set, the partition counts are the ones the rules compute anyway.
6 moves code and changes error construction only.

## Test plan

- 4a `TestMemoizeAfterCancellationLeavesTheMemoAlone`: each memoize entry point
  (both call types, logical and physical) memoizes four expressions, cancelling
  before the third from the loop; the memo gains two references.
- 4b `TestJavaBindingsCountJavasMatcherBindings`: one case per rule and per
  rejected shape, over a reference with three plan partitions, two of them
  stored-record. `TestPlannerCapTrips`: a Java count over the bound trips (both
  drivers, both phases, observing bound+1, `OnMatch` not run) and one at the bound
  plans; the swapped firing of a non-counting rule adds nothing (this replaces the
  cumulative-swap pin); the count is not computed without a bound.
  `TestMaxNumMatchesPerRuleCallCountsPlanPartitions`: the SQL option trips at 2 on
  a filter over a two-index table and plans unset or at 1000.
- 6 `TestTranslateExecErrorMapsExecutorFailures` (code and message per kind, and
  the range-bound, enum and UUID errors); the analyzer's unit test and its red run
  against an executor file importing `api`; the existing SQLSTATE tests in the
  sqltest packages.
