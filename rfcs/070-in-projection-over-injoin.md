# RFC-070: `IN` over an indexed column drops the outer projection

**Status:** Implemented
**Area:** Cascades query planner (physical-plan extraction + projection/fetch merge rule)
**Reviewers:** Graefe (Cascades alignment — mandatory), Torvalds (code quality), codex, @claude

## Problem

`SELECT id FROM t WHERE a IN (1, 7)` over a table with an index on `a`
planned as `InJoin(IndexScan(IDX_A, [=]))` with **no** outer `Project([ID])`.
It therefore returned columns `[ID, A]` instead of `[ID]`; the *values* were
correct, but the output schema was wrong and `rows.Scan(&id)` failed with
`sql: expected 2 destination arguments in Scan, not 1`. The same query on an
unindexed copy planned `Project([ID], PredicatesFilter(Scan(T)))` and returned
`[ID]` — so the divergence was index-path-only.

While fixing it, a second, related defect surfaced (same root cause): the
expression projection `SELECT id + 100 FROM t WHERE a IN (1, 7)` planned
`Project([id+100], Fetch(<nil>))` and returned **zero rows**.

Flagged by RFC-048's W3.5 plan-diversity oracle (TODO line 63). This is a
**correctness** bug: a SELECT returns the wrong columns / no rows.

## Investigation

The logical tree is `LogicalProjection([id]) → LogicalFilter([a IN (1,7)]) →
LogicalScan(t)`. `InComparisonToExplodeRule` rewrites the filter into a
predicate-free `SelectExpression(resultValue=QOV(inner), [innerFilter, explode])`,
which `ImplementInJoinRule` implements as a bare `InJoin` (Java parity: the
InJoin rule only fires when the result value *is* the inner QOV, so the `id`
projection necessarily lives in the **parent** `LogicalProjectionExpression`).

Dumping the root (projection) group's members revealed **three**:
`LogicalProjectionExpression`, the correct `physicalProjectionWrapper →
Project([id])` (`isIdentity=false`), and a **bare `physicalInJoinWrapper →
InJoin`** — illegally a member of the projection group, *cheaper* than the
Project, so it won (`bestExpr = physicalInJoinWrapper`).

Two defects:

### Defect 1 — `MergeProjectionAndFetchRule` drops the projection for join children

After `PushInJoinThroughFetchRule` produces `Fetch(InJoin(coveringScan))`, the
plan above is `Projection([id], Fetch(InJoin(coveringScan)))`.
`MergeProjectionAndFetchRule` removes the fetch when all projected values are
available in the partial record. Its covering-index branch correctly yields
`Projection([id], coveringScan)` — it *keeps* the projection. But its
**fallback** (`call.Yield(fetchInnerExpr)`), taken when the fetch's child is
not a directly-coverable index scan (e.g. an InJoin), dropped **both** the
fetch and the projection, yielding a bare InJoin (result = full partial record
`[id, a]`) into the projection group.

Java's rule yields `fetchPlan.getChild()` (no projection), which is sound **only
under Java's `pushValue` model**: `RecordQueryFetchFromPartialRecordPlan.pushValue`
rewrites the projected `Value` into the partial-record domain, so the covering
child's *flowed value already is the projected columns*. Go's `WithCovering`
merely sets a flag — the covering scan still flows the full partial record
`[id, a]`. So in Go the projection must be retained; the fix makes the fallback
consistent with its own covering-index sibling branch, which already retains it.
This is a **Go divergence that compensates for Go's no-`pushValue` covering
model**, not a faithful copy of Java's rule (see follow-up below).

### Defect 2 — transparent-wrapper `WithChildren` doesn't relink a compound-join inner

Plan extraction (`properties.ExtractBestPlan`) rebuilds each node via
`WithChildren(freshChildren)`, where `freshChildren[0]` is a singleton
Reference holding the **fully-extracted** inner plan. `physicalProjectionWrapper`
and `physicalFetchFromPartialRecordWrapper` only rebuilt their plan when the
inner was `isLeafReplaceable` (scans/filters/etc.) — *excluding* joins. For an
InJoin inner they kept the rule-time placeholder plan, whose `inner` is nil (a
join tracks its child in its wrapper quantifier, not the plan object). So
extraction produced `Project([id], InJoin(<nil>))` and `Fetch(<nil>)`. Latent
until now: a projection/fetch over a join had never previously won.

This mirrors RFC-069, which removed the same gate from `physicalInMemorySortWrapper`
for the same reason (the gate "protects a join's INTERNAL structure from being
swapped"; a transparent unary cap imposes no such constraint).

## Fix

1. **`rule_merge_projection_and_fetch.go`** — the fallback retains the
   projection over the fetch's child:
   `call.Yield(Project(projectedValues, child))` instead of `call.Yield(child)`.
   The fetch is still elided (values are in the partial record); the projection
   is preserved. Sound because the rule's `allPushable` guard already proved
   every projected value resolves in the partial record.

2. **`physical_wrapper.go` (projection) + `physical_fetch_from_partial_record_wrapper.go`**
   — `WithChildren` relinks the inner to the extracted child plan even for
   compound joins (drop the `isLeafReplaceable` gate). Scoped to these two
   **transparent** caps (plus `InMemorySort` from RFC-069); `WithChildren` is
   extraction-only and qs[0] is the authoritative fully-extracted winner.

Result: `Project([ID], InJoin(IndexScan(IDX_A, [=])))`, columns `[ID]`; the
expression projection returns `{101, 107}`.

### Scope: gate retained for filter/DML-carrying wrappers

Removing the gate from **all** unary wrappers regressed
`TestFDB_AggregateIndexUsage` (`count_with_eq_filter`, `delete_then_verify`):
aggregation and DML wrappers embed filter/selection semantics in their *own*
plan, and their child quantifier need not range over the filtered inner — so
relinking to `findPhysicalPlan(qs[0])` drops the filter (deletes/counts all
rows). The gate is therefore retained for those; only the genuinely transparent
caps relink unconditionally.

## Performance

No regression. The fix removes an invalid plan (a bare InJoin with extra
columns) from the search space; the winning plan is the same index InJoin,
correctly capped with a thin `Project`. The fetch is still elided on the
covering path. The extraction change is one-time, post-search. `just test`
48/48 unchanged; 8× external determinism on the regression test, identical plan
each run.

## Test plan

`TestFDB_INProj_OuterProjectionOverInJoin` (real FDB):
- indexed table: plan must be `Project(... InJoin ...)` (optimization fires AND
  is capped by a Project, not a bare InJoin) and return exactly `[ID]` = `{1,7}`;
- unindexed copy: PredicatesFilter scan path, also exactly `[ID]` = `{1,7}`;
- subtests: multi-column `SELECT id, a` → `[ID, A]`; expression `SELECT id+100`
  → `{101, 107}` (the `Fetch(<nil>)` regression);
- 8× external determinism (planner change), identical plan each run.

## Follow-ups (filed in TODO.md)

- **`pushValue`-into-covering-result-value modeling gap.** The truly
  Java-faithful design pushes the projected value into the covering plan's own
  result value, after which both `MergeProjectionAndFetchRule` branches collapse
  to a bare `yieldPlan(child)` with no surviving Project. Go instead compensates
  with a thin outer Project. Closing this would align Go with Java's model.
- **Other transparent unary wrappers over joins.** `Map`, `Distinct`, `Limit`,
  `TypeFilter`, `FirstOrDefault`, `DefaultOnEmpty` still gate `WithChildren` on
  `isLeafReplaceable` and could exhibit the same nil-inner-over-join bug if a
  rule ever builds them with a placeholder inner over a join. Not currently
  reachable via SQL (SQL projections route through `LogicalProjectionExpression`,
  not `Map`), so left gated; the blanket removal is unsafe (it drops filters on
  aggregation/DML wrappers — see Scope).
