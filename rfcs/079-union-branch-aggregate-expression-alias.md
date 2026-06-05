# RFC-079: Preserve post-aggregate-expression aliases in UNION branches (RFC-078 follow-up b)

**Status:** Implemented (shared `buildPostAggregateProjection` helper; whole-builder unify filed as a standing cleanup)
**Area:** Cascades logical-plan builder — UNION branch construction
**Reviewers:** Graefe (Cascades alignment / the parallel-builder decision), Torvalds (code quality), codex, @claude

## Problem

A `UNION [ALL]` whose branches project a **post-aggregate expression with an alias**
(e.g. `COUNT(*)+1 AS x`), read downstream **by name**, silently returns NULL:

```sql
SELECT u.x FROM (SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b) u
-- → [NULL, NULL]   (want [3, 4] for count(a)=2, count(b)=3)
```

Verified wrong on master. RFC-078 fixed the *bare* aggregate union case (`COUNT(*) AS x`);
this is its documented follow-up (b) — the **expression** case is a distinct root cause.

## Investigation

Two parallel SELECT builders exist:
- **`visitSelectGroupBy`** (`plan_visitor.go`) — the modern path used by standalone SELECTs.
  For a post-aggregate expression it builds `LogicalProject` WITH the alias
  (`plan_visitor.go:986-993,1020-1033`: the `ac.outExpr != nil && ac.aggFunc == ""` arm sets
  `alias = ac.outName`, `hasAlias = true`).
- **the legacy builder** (`logical_builder.go` via `buildLogicalPlanForUnionWithCTECatalog` →
  `buildLogicalPlanForQueryBodyWithCTECatalog`) — used to build UNION **branches**. Its
  aggregate arm has NO post-agg-expression case and builds the projection with
  `logical.NewProject(op, allProj, nil)` (`logical_builder.go:386`) — **nil aliases**.

Confirmed by inspecting the real-pipeline logical plan:
- standalone `SELECT COUNT(*)+1 AS x FROM a` → `Project(projections=[COUNT(*)+1], aliases=[X])` ✓
- union branch (same SELECT) → `Project(projections=[COUNT(*)+1], aliases=[])` ✗

So the branch `ProjectionPlan.aliases` is empty → `executeProjection` keys the branch row only
by the expression text `(COUNT(*)+1)` (never `X`), and `planColumnNamesWithMD` reports the same
→ the union output is keyed `(COUNT(*)+1)`, so the outer `Project([X])` reads NULL. (The bare
aggregate case works because RFC-078 reads the alias off the `StreamingAgg`'s AggregateSpec; the
expression case has a `Project` on top whose alias is the dropped one.)

This is a **parallel-pipeline divergence** (CLAUDE.md "no parallel pipelines"): UNION branches do
not go through the unified `visitSelectGroupBy` path, so they miss its aliasing.

## Fix — Graefe ruling: (a) via EXTRACTION, file (b) as standing cleanup

Graefe NAK'd a literal copy-paste of the modern arm into the legacy builder ("don't fix a
duplication bug by adding a third copy"). The ACK'd approach is **(a) by extraction**: lift the
post-aggregate projection-building loop (proj texts + per-column aliases + `IsComputed` + the
`hasAlias` decision) into a **single helper** over `[]aggSelectCol`, called by both
`visitSelectGroupBy` and the legacy `buildSelectShell` arm — **one source of alias truth**.

Implemented as `buildPostAggregateProjection(op, aggCols, strip)` in `logical_builder.go`:

```go
func buildPostAggregateProjection(op logical.LogicalOperator, aggCols []aggSelectCol,
    strip func(string) string) (*logical.LogicalProject, []antlrgen.IExpressionContext)
```

- Returns the fully-built `LogicalProject` (aliases + `IsComputed` set) and the per-column
  post-agg expression contexts the caller stores as `postAggExprs`; `(nil, nil)` when no visible
  column projects.
- `strip` is passed in because the two callers' column-qualifier closures differ
  (`visitSelectGroupBy` strips a derived-table prefix AND a table-alias prefix;
  `buildSelectShell` strips only the derived-table prefix).
- The modern arm collapses from ~58 lines to a 4-line call; the legacy arm gains the alias handling
  it lacked. **No third copy.**
- Modern path is **plandiff byte-identical** (the helper is the modern loop verbatim — the legacy
  arm's redundant `strings.TrimSpace` on the canonical text is dropped, a no-op since
  `canonicalTextOf` returns token-interval text with no surrounding whitespace).

**(b) Whole-builder unify** (route UNION branches — and the other `buildSelectShell` callers —
through `visitSelectGroupBy`, eliminating the legacy SimpleTable builder entirely) is the
CLAUDE.md "one query path" endgame but is broader than this bug: `buildSelectShell` /
`buildLogicalPlanForSelect` has multiple callers (plain-table SELECT, derived tables, UNION
branches). Filed as a standing TODO cleanup, NOT scoped to the union entry alone — per Graefe's
condition (3), it must unify the whole SimpleTable builder, not graft a special case onto the
union path.

## Performance

Read-side only; no wire/plan-shape change (plandiff byte-identical — only the branch projection's
alias metadata changes, which adds an output-row key). No new operators.

## Test plan

- **Red→green e2e** (`TestFDB_UnionAggregateExprAlias`, `union_aggregate_expr_alias_test.go`):
  `SELECT u.x FROM (SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b) u`
  returns both expression values `[3,4]` (was `[NULL,NULL]`); ORDER BY variant; GROUP BY +
  expression variant; same-named no-regression.
- **Red→green logical-plan unit** (`TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias`,
  `logical_builder_test.go`): the legacy `buildSelectShell` path (used by UNION branches) builds the
  post-agg-expression `LogicalProject` WITH the alias — verified to fail with empty `Aliases` when
  the helper passes `nil` (the pre-fix legacy behavior). Plus `_NoSpuriousAlias` (an unaliased
  expression is NOT aliased).
- **No regression**: full union/aggregate/projection/GROUP BY e2e surface (~150 `TestFDB_*` tests)
  green; standalone post-agg-expression SELECTs (`AggregateExpressionVariants`,
  `CompositeAggregateExpressions`, `DerivedTableArithmeticOnAggregates`) unchanged; plandiff
  byte-identical (`//pkg/relational/conformance/plandiff` green).
