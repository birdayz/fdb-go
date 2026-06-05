# RFC-081: Grouped-aggregate union branches resolvable by name (RFC-078 follow-up a)

**Status:** Draft
**Area:** Cascades executor (`planColumnNamesWithMD`) + translator union gate
**Reviewers:** Graefe (Cascades/executor alignment + the gate decision), Torvalds (code quality), codex, @claude

## Problem

RFC-080 opened the union-branch gate (`unionBranchNormalizable`) for **ungrouped** bare aggregates
and left **grouped** bare aggregates gated, because a grouped bare aggregate can plan as
`AggregateIndex` (single agg) or `MultiIntersection` (multi agg), and `planColumnNamesWithMD` did not
report those plans' output column names — so the executor's UNION position-remap could not normalize a
grouped aggregate branch. A grouped-aggregate union read by name (derived table / join leg) with
mismatched group-key names was therefore a clean error:

```sql
WITH u AS (SELECT g, COUNT(*) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(*) FROM gb GROUP BY h)
SELECT c.w FROM u, c WHERE u.g = c.id   -- was: untranslatable; want: correct rows
```

(A bare grouped aggregate arises from an *unaliased, all-visible* GROUP BY SELECT — an aliased one tops
with a Project and was already normalizable via the `LogicalProject` arm.)

## Investigation

`planColumnNamesWithMD` (`executor.go`) resolves a branch's output column names; the UNION remap
(`remapUnionColumnsByPosition`) uses it to map a branch's keys to the first branch's. For aggregate
plans:
- **StreamingAgg**: handled (RFC-078) — `streamingAggOutputNames` (group keys + alias-or-canonical).
- **AggregateIndex**: NOT handled — its `GetResultType()` is `UnknownType`, so the function fell
  through to nil. Its cursor (`aggregateIndexCursor`) writes `datum[groupCol]=key` and
  `datum["FUNC(col)"]=value`, so the row IS keyed by `[groupCols…, FUNC(col)]` — only the *reporting*
  was missing.
- **MultiIntersection**: its `GetResultValue()` is a `RecordConstructorValue` whose field names are the
  output columns, and the merge cursor keys each row by those exact names. The `GetResultType()→RecordType`
  fallback already reported them (upper-cased), but only correctly because the names are upper in
  practice.

A bare grouped aggregate is always **unaliased** (aliased → Project), so there is no aggregate alias to
carry — the output names are simply the group columns + the canonical aggregate name. The only gap was
*reporting* them.

## Fix

1. **`RecordQueryAggregateIndexPlan.OutputColumnNames()`** (plans) returns `groupCols` + the canonical
   `CanonicalAggColumnName()` — exactly the keys `aggregateIndexCursor` writes (single source so cursor
   and reporter can't drift).
2. **`planColumnNamesWithMD` gains an AggregateIndex arm** (`return aggIdx.OutputColumnNames()`) and an
   explicit **MultiIntersection arm** (report the result value's `RecordConstructorValue` field names
   *verbatim* — byte-identical to the merge cursor's row keys, not relying on the fallback's
   upper-casing; mirrors the MapPlan arm / RFC-078 codex P2).
3. **Relax the gate**: `unionBranchNormalizable`'s `LogicalAggregate` arm returns `len(o.Aggregates) >= 1`
   (drops the RFC-080 `&& len(o.GroupKeys) == 0`), with ONE exception (below). Now every physical
   realization of a bare aggregate branch (StreamingAgg / AggregateIndex / MultiIntersection) reports
   its output schema, so the remap normalizes it regardless of grouping. A 0-aggregate (group-only)
   shape stays gated.

**Exception — grouped `COUNT(<constant>)` (codex).** `COUNT(1)` matches a count-star aggregate index
(`MatchesGroupBy` treats a constant operand as count-star), so its AggregateIndex realization reports
the canonical `COUNT(*)`, while the logical schema (`aggregateOutputColumns`) keeps `COUNT(1)`. That
logical-vs-physical name divergence would make the union position-remap read a missing key → NULL count.
So the gate conservatively rejects a GROUPED branch with a constant aggregate operand (detected via
`AggregateOperands` being a non-nil `ConstantValue` — no text-matching): clean error, never wrong rows.
Ungrouped `COUNT(<constant>)` is safe (no aggregate-index candidate → StreamingAgg names it `COUNT(1)`
consistently). Closing this needs the AggregateIndex plan to carry the logical output name — a follow-up.

No *behavioral* cursor change: the AggregateIndex and MultiIntersection cursors already write rows keyed
by the output names; this RFC teaches the planner-name reporter to report them. The `aggregateIndexCursor`
is additionally re-wired to build its aggregate-column key via the plan's `CanonicalAggColumnName()` (the
same method `OutputColumnNames` uses) instead of an inline copy, so the cursor's written key and the
reported name are a genuine single source and cannot drift (Torvalds).

Validated end-to-end before finalizing: the grouped single-agg union join leg plans as `AggregateIndex`
and returns correct rows; the grouped multi-agg one plans as `StreamingAgg` (cost) and also returns
correct rows.

## Performance

Read-side only; no wire/plan-shape change (plandiff byte-identical). `planColumnNamesWithMD` does
strictly less work for an aggregate-index branch (reports directly instead of falling through to nil).

## Test plan

- **Red→green e2e** (`TestFDB_UnionGroupedAggregate`): grouped single-aggregate union join leg
  (`SELECT g, COUNT(*) … GROUP BY g UNION ALL SELECT h, COUNT(*) … GROUP BY h`, joined on the group key)
  returns correct rows; EXPLAIN-pinned to plan as `AggregateIndex`. Grouped MULTI-aggregate variant
  FILTERED on the group key (`WHERE g = 100`) so each branch is EXPLAIN-pinned to plan as
  `MultiIntersection` (exercising the MI reporting arm e2e), returns correct rows. Mismatched group-key
  names throughout.
- **Flip `TestFDB_UnionGroupedAggregateStillGated`** → now returns correct rows (it was the RFC-080
  clean-error sentinel).
- **Unit**: `planColumnNamesWithMD` reports an AggregateIndex's `[groupCols…, FUNC(col)]` and a
  MultiIntersection's result-value field names; `OutputColumnNames` byte-matches the cursor keys.
- **Gate unit**: `unionBranchNormalizable` true for grouped 1- and 2-aggregate, false for 0-aggregate.
- **No regression**: full union/aggregate/aggregate-index e2e surface; plandiff byte-identical; stress-1M.
