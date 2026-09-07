# RFC-243: LIMIT arithmetic preserves unboundedness and representable offsets

## Problem and reference

Two logical LIMIT rewrites perform arithmetic on values that are not ordinary
bounded row counts:

* `LimitMergeRule` adds two signed 64-bit offsets without checking overflow.
  `LIMIT 1 OFFSET 1` over `LIMIT MaxInt64 OFFSET MaxInt64` can become
  `LIMIT 1 OFFSET MinInt64`. The nested query skips every row of a small table;
  a negative combined skip must never make that query return a prefix instead.
* `PushLimitThroughUnionRule` computes `limit + offset` before recognizing a
  negative limit as the no-cap sentinel. A skip-only logical limit of `-1, 2`
  consequently caps every union branch at one row, discarding required rows.

These are Go read-side extensions, not ports of Java logical LIMIT rules. The
pinned Java reference is `ExecuteProperties.clearSkipAndAdjustLimit`: it tests
`rowLimit == ROW_LIMIT_UNLIMITED` **before** adding the skip, and leaves an
unlimited request unlimited. `SkipCursor.onNext` discards exactly the requested
prefix; `RowLimitedCursor.onNext` supplies the separate finite cap. Neither
licenses turning a no-cap sentinel into a finite cap.

Go SQL's grammar requires `LIMIT` before `OFFSET`. A bare OFFSET-only clause is
rejected, but `parseLimitClause` also uses the negative sentinel for an unresolved
parameter. The driver substitutes bound parameters before planning; pin a
parameterized UNION limit through direct execution, prepared-statement reuse,
and EXPLAIN rather than inferring that boundary from the grammar alone.

## Decision

Keep the current operators and continuation formats. Enforce the numeric domain
at construction, check rewrite arithmetic, and saturate only execution budgets:

1. Decline LIMIT merging when adding nonnegative offsets would exceed MaxInt64.
   Retain both operators so each skip remains exact. Do not saturate the combined
   offset: MaxInt64 is not the mathematical sum and can itself change semantics.
2. Decline branch-cap pushdown for a negative static limit, as already done for a
   runtime cap. A finite branch cap cannot represent an unlimited request.
3. Use a shared `checkedLimitSum` in the two rules: both inputs must be
   nonnegative and their sum must fit int64. The union rule declines explicitly
   for an overflowing finite branch cap instead of relying on a wrapped negative
   value. Preserve the runtime-cap guard and ordinary finite pushdown.
4. Reject negative offsets in both logical constructors and
   `NewRecordQueryLimitPlanFromQuantifier` with
   `expressions.InvalidLimitOffsetError{Offset}`. The physical static constructor,
   runtime-value constructor and `WithInner` all delegate to that constructor;
   test its exported quantifier path directly too. Unlike
   a negative limit, a negative offset has no defined no-cap meaning. Otherwise
   a negative outer offset can also make `iLimit - oOffset` overflow during
   merging. This is a Go LIMIT construction contract, not a claim that Java's
   `SkipCursor` rejects negative skips (it treats them as zero).
5. `executeLimit` also adds `remOffset + emit` unchecked when setting the child
   read budget. It currently wraps negative and therefore disables the budget
   at consumers that test `> 0`; this case does not change rows today. Extract
   `limitChildRowLimit(parentCap, remOffset, remLimit)` and saturate its sum at
   MaxInt, preserving the existing parent-cap and unbounded handling. A read
   budget may saturate because reaching it yields a resumable page boundary;
   a semantic offset may not. Unit-drive the extracted decision, including
   parent-cap, zero, unbounded and overflow cases.

No new SQL syntax, persisted bytes, or cross-engine continuation contract changes.
The existing SQL/executor behavior of representable LIMIT operators is retained.

## Verification

* Rule tests: offset sums at MaxInt64 and just beyond; nested finite and unbounded
  caps; skip-only union offsets 0, 1, 2, 5 and MaxInt64; finite branch caps at and
  beyond the addition boundary; zero limits and runtime caps remain covered.
* Real-FDB SQL scenario `limit_offset_bounds.yaml`: nested overflowing offsets
  return no rows, ordinary nested windows still return the correct row, and a
  large standalone finite limit still returns the tail. Pin SQL's rejection of
  an OFFSET-only union separately. Add a driver test for parameterized UNION
  limits, repeated binds and EXPLAIN, asserting both bounded plan and row count.
* Real-FDB logical API regression: a skip-only limit above UNION ALL retains all
  rows after the skipped prefix, with the full planner and executor in the path.
* Register new files with Gazelle; see each new test execute under Bazel. Run
  the relevant targets, `just test`, bounded fuzzing of LIMIT rewrite semantics,
  and the million-row stress target before and after the production changes.
  Regenerate `FEATURE_MATRIX.md` with `just feature-matrix` and the SQL ledgers
  with `just sql-coverage` for the new scenario.

## Reproduction on 42a79173557936707a32a969e51489667db6ff01

Fresh Bazel runs, before production edits:

* `TestYamsqlConformance/limit_offset_bounds`: 1 of the initial 5 assertions
  failed. The MaxInt64 inner offset plus outer offset 1 returned `[1]` instead
  of no rows; the other 4 assertions passed. The existing rewrite really is
  reachable through this derived-table SQL shape.
* `TestIntegration_UnboundedUnionLimit`: the full planner produced
  `Limit(-1, offset=2, UnorderedUnion(Limit(1, Scan(Order)), Limit(1, Scan(Order))))`.
  Real FDB execution returned 0 rows instead of the required 4.
* In the 5-arm unbounded-union rule test, offsets 2, 5 and MaxInt64 failed;
  offsets 0 and 1 are positive controls for behavior already correct.
  Both overflow-precondition arms of the merge test failed; only outer offset
  1 also changes rows (outer offset MaxInt64 has an empty window either way).
* `TestFDB_UnionLimitParameters` passed on unchanged production code: direct
  execution and prepared reuse each returned the requested row counts for caps
  4, 1, 3, 4 with OFFSET 2; direct and prepared EXPLAIN each showed the bound cap
  in all four iterations. A bound negative SQL limit was rejected with 42601.
  This pins those driver paths, not every possible caller of the embedded API.

The parameter EXPLAIN pin intentionally compares complete plans for the **same**
cap, not across different caps. The pre-fix cap-3 plan had no branch caps while
caps 1 and 4 did. `designationScope.compare`'s REWRITING tiers count selects,
table functions and predicates, not LIMIT nodes; tied alternatives are ordered
by deep semantic hash, which includes the cap. This is not a monotone row-count
threshold. Repeated execution must still be deterministic for a given binding.

## Performance and alternatives

The checks are constant-time and do not change the search for ordinary bounded
limits. Keeping stacked operators costs another cursor only where no exact
single offset can represent the composition. Saturating or wrapping semantic
offsets is rejected on correctness grounds; saturating an execution read budget
is the existing record-layer approach (`cursor.go:saturatingAdd`). Increasing
integer width across the SQL, planner, executor and continuation APIs is
unnecessary: the existing nested representation already expresses both skips
exactly.
