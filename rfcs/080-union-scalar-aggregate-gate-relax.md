# RFC-080: Relax the union-branch gate for bare scalar aggregates (RFC-078 follow-up a+c)

**Status:** Implemented
**Area:** Cascades translator — `unionBranchNormalizable` gate
**Reviewers:** Graefe (Cascades/executor alignment + the gate decision), Torvalds (code quality), codex, @claude

## Problem

A `UNION` whose branches are **bare scalar aggregates** with mismatched output aliases, read
downstream **by name** (derived table or join leg):

```sql
WITH u AS (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b)
SELECT c.w FROM u, c WHERE u.x = c.id
```

is **untranslatable** today (clean error, never wrong rows): RFC-077's `unionBranchNormalizable`
gate returns `false` for ANY `LogicalAggregate` branch (`cascades_translator.go:423-424`). RFC-078
already taught the executor to remap StreamingAgg union branches, so this conservative gate now
rejects a case that actually works.

## Investigation — the RFC-078 follow-up (a) premise does not hold in Go

RFC-078 left the gate shut on the theory that a `LogicalAggregate` branch *might* plan as
`AggregateIndex`, whose cursor flows the aggregate under only the canonical `aggFunc(col)` name and
drops the alias, and which `planColumnNamesWithMD` does not report at all — so anchoring there would
drop rows. That theory is **real, but only for GROUPED bare aggregates**:

1. **The gate's `LogicalAggregate` case is reached ONLY by a *bare* aggregate branch** (no Project on
   top). An *aliased* or partially-projected GROUP BY tops with a stripping `LogicalProject`
   (`buildSelectShell`'s `needsStrip`) — handled by the `*logical.LogicalProject` arm. But an
   **unaliased, all-visible grouped** SELECT skips the Project: `SELECT g, COUNT(*) FROM t GROUP BY g`
   → bare `Aggregate(group=[G], …)` (verified by probe). So **a bare aggregate can be GROUPED.**
2. **Ungrouped** bare aggregates are always StreamingAgg: Go produces **no aggregate-index candidate**
   for an ungrouped index (`tryAggregateIndexCandidate` returns `nil` when `groupingCount == 0`,
   `cascades_generator.go:1371`), so `AggregateDataAccessRule` cannot fire — confirmed by EXPLAIN
   (`SELECT COUNT(*) FROM ai` plans as StreamingAgg *even with* an ungrouped `COUNT(*)` index).
   **Grouped** bare aggregates (`groupingCount > 0`) DO get a candidate and CAN plan as AggregateIndex
   (single agg) or MultiIntersection (multi agg).
3. **StreamingAgg already carries the alias** for every aggregate: `aggregateCursor.finalizeGroup`
   dual-keys each row by canonical `aggResultName` AND `agg.Alias` (`streaming_cursors.go:335`), and
   `streamingAggOutputNames` reports it (`executor.go:1704`) — any arity. The AggregateIndex /
   MultiIntersection cursors do NOT, and `planColumnNamesWithMD` returns nil for them
   (`executor.go:1216-1220`).

So the safe, reachable case is the **UNGROUPED** bare aggregate (always StreamingAgg). The GROUPED
bare aggregate is the genuine RFC-078 follow-up (a) — its AggregateIndex/MultiIntersection cursors
must carry the alias and be reported before it can open; until then it stays gated.

## Fix

`unionBranchNormalizable`'s `case *logical.LogicalAggregate` returns
`len(o.Aggregates) >= 1 && len(o.GroupKeys) == 0` (was `false`). An ungrouped bare aggregate branch
always plans as StreamingAgg, which flows every aggregate under its alias regardless of arity — so
single- AND multi-aggregate ungrouped branches are remappable by the executor's position-remap. A
GROUPED bare aggregate (which may plan as AggregateIndex/MultiIntersection, whose names
`planColumnNamesWithMD` does not report) stays gated — clean error, never wrong rows. A 0-aggregate
(group-only) shape is also left gated.

No cursor / plan / executor change is made for the gate itself — the AggregateIndex alias-carrying
work is genuinely needed only for the GROUPED case, which this RFC does NOT open, so adding it now
would be untested dead weight. The gate comment documents why grouped is excluded. Closing the
grouped case is the deferred follow-up (a).

**One related planner fix (codex).** `ImplementUnorderedUnionRule.physicalPlanColumnNames`
(`rule_implement_unordered_union.go`) unwrapped a `RecordQueryStreamingAggregationPlan` through
`GetInner()` to its *pre-aggregation* input column names — unlike the executor's
`planColumnNamesWithMD`, which RFC-078 taught to report a StreamingAgg's *output* schema. For an
unordered union of StreamingAgg branches over differently-named inners, that would build a rename
`MapPlan` reading columns absent from the aggregate row → NULLs. (In practice such unions plan as the
ordered `Union`, whose executor path already remaps correctly — the reproduction returns correct rows
— so this is a latent inconsistency, not a triggered bug; but the gate relax widens the
aggregate-union surface, so it is fixed here.) The fix: `physicalPlanColumnNames` returns nil for a
StreamingAgg (does not unwrap), deferring that branch's normalization to the executor's position-remap
(the RFC-078 mechanism) instead of guessing wrong names. Pinned by
`TestPhysicalPlanColumnNames_StreamingAggNotUnwrapped`.

## Performance

Translator-only, one comparison; no executor/wire/plan-shape change. plandiff byte-identical.

## Test plan

- **Red→green e2e** (`TestFDB_UnionScalarAggregateAlias`): single- and multi-aggregate bare-scalar
  unions with mismatched aliases, read by name (derived table) + ORDER BY, return correct rows; and an
  EXPLAIN assertion that a scalar `COUNT(*)` does NOT plan as AggregateIndex **even with an ungrouped
  index present** — pinning the gate-relax invariant (if it ever flips, the test fails loudly).
- **`TestFDB_UnionJoinLeg` case (3) flips** from "aggregate-mismatch errors cleanly" to "returns
  correct rows" — the single-aggregate union join leg now resolves (plans as StreamingAgg).
- **Grouped-stays-gated e2e** (`TestFDB_UnionGroupedAggregateStillGated`): a UNION of bare GROUPED
  aggregate branches with mismatched group-key names, read by name, stays untranslatable — even though
  (EXPLAIN-pinned) the grouped branch DOES plan as AggregateIndex. Pins that the gate keeps the
  unreportable AggregateIndex realization out of the union normalizer (never wrong rows).
- **Gate unit test** (`TestUnionBranchNormalizable_AggregateArity`): true for ungrouped 1- and
  2-aggregate `LogicalAggregate`, false for a grouped bare aggregate and for 0-aggregate (grouped and
  ungrouped-zero).
- **Planner unit test** (`TestPhysicalPlanColumnNames_StreamingAggNotUnwrapped`): `physicalPlanColumnNames`
  returns nil for a StreamingAgg (does not unwrap to its inner's pre-aggregation column names).
- **No regression**: full union/aggregate/aggregate-index e2e surface; plandiff byte-identical;
  stress-1M.

## Out of scope — the real follow-up (a)

The GROUPED bare aggregate union case stays gated. Opening it requires the AggregateIndex cursor
(single agg) AND the MultiIntersection result value (multi agg) to flow each aggregate under its alias
(mirroring StreamingAgg's dual-key) AND `planColumnNamesWithMD` to report their group-col + output
names — then the gate's `len(o.GroupKeys) == 0` clause can drop. Deferred; the gate comment and the
`TestFDB_UnionGroupedAggregateStillGated` sentinel mark it.
