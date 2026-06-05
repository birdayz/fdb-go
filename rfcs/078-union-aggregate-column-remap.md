# RFC-078: Fix union column-remap for aggregate branches (TODO 7.6-union-remap)

**Status:** Implemented (StreamingAgg + UnorderedUnion remap; AggregateIndex + join-leg re-enable deferred)
**Area:** Cascades executor — UNION column normalization
**Reviewers:** Graefe (Cascades/executor alignment), Torvalds (code quality), codex, @claude

## Problem

A `UNION [ALL]` whose branches are **aggregates with mismatched output aliases**, read
downstream **by name**, silently **drops rows**:

```sql
WITH u AS (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b)
SELECT c.w FROM u, c WHERE u.x = c.id   -- only the FIRST branch's rows match; the
                                        -- second branch reads u.x = NULL → dropped
```

Verified **wrong on master too** — this is a pre-existing executor bug, independent of
RFC-077 7.6 (it was surfaced by the 7.6 review, where the union-as-join-leg path
conservatively rejected the shape to avoid inheriting the wrong rows; tracked as
TODO 7.6-union-remap).

## Investigation

SQL exposes a union's column names from the **first branch**; the executor unions later
branches by **position** (`remapUnionColumnsByPosition`, `executor.go`), remapping each
branch's keys to the first branch's. The remap source/target keys come from
`planColumnNamesWithMD(branch)`.

`planColumnNamesWithMD` (executor.go) resolves a branch's output column names by:
1. a top `RecordQueryProjectionPlan` → its alias/projection names; else
2. descend `innerPlanAccessor` to the deepest inner; then
3. that inner's `RecordType` result fields; else
4. (scan + md) the scan's columns; else nil.

A non-indexed aggregate branch is `StreamingAgg(keys=[], Scan)` with **no projection
wrapper**. `RecordQueryStreamingAggregationPlan` implements `GetInner()`, so step 2
**descends past it to the Scan**, and step 4 returns the **scan's** columns (`[ID]`) —
NOT the aggregate's output alias (`[X]`/`[Y]`). Both branches resolve to `[ID]`, so they
compare equal → **no remap fires** → each branch keeps its own alias key (`X` on branch
1, `Y` on branch 2). A downstream by-name read of `u.x` finds `X` only on branch 1; branch
2's rows read NULL and drop out.

The `aggregateCursor` (`streaming_cursors.go`) keys each output row by the grouping-key
name, the canonical `aggResultName` (`COUNT(*)`), AND the alias (`X`/`Y`) — so the row DOES
carry the alias key; the bug is purely that `planColumnNamesWithMD` reports the wrong name
to the remap.

## Fix (two parts — the real fix location is broader than the RFC draft assumed)

Investigation during impl found the reproduced shape plans as `UnorderedUnion`
(`RecordQueryUnorderedUnionPlan`), executed by `executeUnorderedUnion`
(`executor_new_plans.go`) — which **concatenated branch cursors with NO column
normalization at all**, unlike the ordered `RecordQueryUnionPlan`/`executeUnionStreaming`
(which already remaps). So two changes:

1. **`executeUnorderedUnion` remaps later branches to the first branch's column names by
   position** (`remapUnionColumnsByPosition`, keyed on `planColumnNamesWithMD`) — exactly
   as the ordered union does. A no-op when names already agree (the common case). This
   fixes ALL union-by-name reads over differently-named branches, not just aggregates.

2. **`planColumnNamesWithMD` reports a `*RecordQueryStreamingAggregationPlan`'s output
   column names** (grouping-key names `aggKeyName` + each aggregate's `Alias` when present,
   else `aggResultName`), stopping at the aggregate instead of descending through
   `GetInner()` to the input scan. These match the keys `aggregateCursor` writes and the
   schema the translator derives (`aggregateOutputColumns`), so the remap normalizes a
   mismatched-alias aggregate branch correctly. Placed before the `innerPlanAccessor`
   descent; the existing top-`ProjectionPlan` case is unchanged.

**Out of scope (follow-ups, per Torvalds review):**
- **AggregateINDEX branches** (`RecordQueryAggregateIndexPlan`): its cursor writes ONLY the
  canonical `aggFunc(col)` name, never the alias (`rule_aggregate_data_access` drops it),
  so naming it by the alias would not match its row keys. Fixing it needs the index cursor
  to carry the alias first — not handled here; documented in TODO 7.6-union-remap.
- **A renaming `RecordQueryMapPlan` directly over a StreamingAgg** (e.g.
  `SELECT COUNT(*)+1 AS x`): `planColumnNamesWithMD` descends through the Map to the
  StreamingAgg and returns the agg's pre-rename names, not the Map's output alias. The
  top-`RecordQueryProjectionPlan` case covers the common rename; only the Map variant is
  unhandled. Not a regression (master returned scan-cols/nil; no-op for symmetric unions);
  same deferred-shape family as AggregateIndex (Graefe impl-review condition).
- **Re-enabling the union-as-join-leg aggregate case** (relaxing the RFC-077
  `unionBranchNormalizable` gate): unsafe while the index-aggregate path drops the alias,
  because the translator gates on the *logical* `LogicalAggregate`, blind to whether it
  plans as StreamingAgg (fixed) or AggregateIndex (not). The join-leg aggregate-mismatch
  stays conservatively untranslatable (a clean "unsupported" error, never wrong rows).

## Performance

Read-side only; no wire change. `planColumnNamesWithMD` does strictly less work for
aggregate branches (stops earlier instead of descending to the scan). No plan-shape change
(plandiff byte-identical) — this only affects runtime row key normalization. Streaming
union path is preserved (the fix keeps `planColumnNamesWithMD` non-nil for aggregate
branches, so they still stream rather than buffer).

## Test plan

- **Red→green executor unit/integration**: a mismatched-alias aggregate union read by name
  returns all rows (was dropping the non-first branch).
- **`TestFDB_UnionJoinLeg`** (RFC-077): flip case (3) from "aggregate-mismatch errors
  cleanly" to "aggregate-mismatch returns **correct rows**" once the translator gate is
  relaxed; keep the same-named and projection-mismatch cases green.
- **Derived-table / ORDER-BY union-by-name** over mismatched-alias aggregates → correct.
- **No regression**: full union test surface, plandiff byte-identical, chain budget gate,
  stress-1M.
