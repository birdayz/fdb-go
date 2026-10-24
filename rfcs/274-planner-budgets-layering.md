# RFC-274: Planner budgets and record-layer layering from the 2026-10-24 quality review

**Status: Draft**

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
yields and inserts when it is cancelled (`unified_tasks.go` around 740-790 and
970-1070); `Yield` is a no-op once cancelled (`expression_rule_call.go:152`,
`implementation_rule.go:125`); the rules with a large internal fan-out check
`CancellationErr()` in their loops (`rule_implement_in_join.go`,
`rule_partition_select.go`, `rule_predicate_to_logical_union.go:100`);
`TestPlanner_MidTaskCancellationStopsRemainingBindings` pins the per-binding
check. Java's `CascadesPlanner` has no cancellation at all. What is left: after a
cancel, a rule's loop keeps calling `MemoizeExpression`, whose memo lookup is
the expensive part of most fan-outs, until the loop ends.

### 4b. Matches per rule call

Java counts every binding of the rule's matcher, one `++numMatches` before each
`executeRuleCall`, and throws `RecordQueryPlanComplexityException` at the first
binding over the bound (`CascadesPlanner.java:1073-1086`, for expression,
match-partition and partial-match transforms). Java's matchers fan out: one
binding per plan partition (`planPartitions(any(anyPlanPartition()))`, e.g.
`ImplementFilterRule`), per stored-record partition
(`filterPlanPartitions(storedRecord, …)`, e.g. `ImplementDeleteRule`), per
combination (`PredicateToLogicalUnionRule`), per quantifier
(`PushRequestedOrderingThroughSelectRule`) and per permutation for set matchers
(`exactlyInAnyOrder`: `ImplementNestedLoopJoinRule`,
`PartitionBinarySelectRule`). Go's matchers bind once at the root
(`expression_matcher.go:51-56`) and the rule's `OnMatch` enumerates the same
partitions itself, so Go counts 1 where Java counts the fan-out: a bound of 2 or
more can never trip. Go also counts the planner's swapped-quantifier firing for
every rule, where Java counts a second binding only for the two set-matcher
rules. And Go reports the full eager count as the observed value, where Java's
is always bound+1.

### 6. Layering

The executor uses `api` for SQLSTATE errors (13 sites), and `resultset.go` is an
`api.ResultSet` over an executor cursor: Java's `RecordLayerResultSet` lives in
fdb-relational-core. Java's record layer throws a `SemanticException` carrying an
`ErrorCode`, and relational's `ExceptionUtil` maps the code to an SQLSTATE. The
relational layer already maps typed executor errors in one place,
`translateExecError` (`cascades_generator.go`), which every executor error from
`ExecutePlan` and `EvaluateScalarSubquery` passes through.

## Fix

### 4a

`ExpressionRuleCall.MemoizeExpression(s)` and the implementation call's memoize
helpers return an unregistered reference once the call is cancelled. The rule's
yields are already dropped, so no plan changes; a cancelled fan-out no longer
searches or grows the memo.

### 4b

- A per-call match budget: the task gives each rule call the bound and the
  call's running count, and `countBindings(n func() int)` lets a rule say "this
  root binding stands for n of Java's bindings". With the bound unset (0, the
  default) n is not computed, so the default planning path does no extra work.
  Past the bound the call fails with `ErrPlannerRuleMatchCapHit`, observed
  bound+1 as Java reports it.
- Rules whose Java count is computable from the existing partition helpers call
  it: ImplementFilter, ImplementTypeFilter, ImplementDistinct(Final),
  ImplementSimpleSelect, ImplementInsert, ImplementTempTableInsert,
  ImplementDelete, ImplementUpdate, ImplementSort (Java's RemoveSortRule),
  PredicateToLogicalUnion, PartitionSelect, PushRequestedOrderingThroughSelect
  and its existential twin.
- The swapped firing counts only for rules whose Java matcher is a set matcher
  (NestedLoopJoin, PartitionBinarySelect).
- The rest stay at one binding per call and are listed in DIVERGENCES.md: rules
  that take the first member where Java binds each (fetch push-downs,
  DistinctUnion, InJoin/InUnion explode sources, DecorrelateValues), Go-only
  rules, PartitionSelect below three quantifiers, and NestedLoopJoin over an
  existential quantifier.

### 6

- `executor.ExecutionError{Kind, Message}` replaces the executor's `api` errors;
  `translateExecError` maps each kind to the SQLSTATE the executor built before
  (22000, 23502, 21000, 42601, XX000). Range-bound and enum/UUID failures return
  their existing `values` errors, mapped there with the same code and message.
- `resultset.go` and its tests move to `pkg/relational/core/resultset`.
- A nogo analyzer, `recordlayerdeps`, fails the build when a non-test file under
  `pkg/recordlayer` imports `pkg/relational`. Test files are exempt: two executor
  tests compare the record-layer converters with the relational ones.

## Performance

4a adds a context read per memoize call. 4b computes nothing unless the bound
is set; with it set, the partition counts are the ones the rules compute anyway.
6 moves code and changes error construction only.

## Test plan

- 4a: a rule call cancelled midway through a fan-out memoizes nothing after the
  cancel.
- 4b: a filter over three plan partitions trips a bound of 2 and plans under 3; a
  three-conjunct OR trips at 6 and plans at 7; a non-set-matcher rule on a
  two-quantifier select counts its swapped firing as nothing; observed is
  bound+1; the SQL option trips (54F02) on the filter shape.
- 6: `TestTranslateExecErrorMapsExecutorFailures` (code and message per kind);
  the analyzer's unit test and its red run against an executor file importing
  `api`; the existing SQLSTATE tests in the sqltest packages.
