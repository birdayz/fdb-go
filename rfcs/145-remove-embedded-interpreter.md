# RFC-145 — Remove the legacy embedded SQL interpreter (kill the parallel pipeline)

**Status:** Draft
**Item:** Known-gaps "ARCHITECTURE — eliminate the legacy embedded SQL interpreter" (a "No parallel
pipelines" violation, surfaced during R8). Delete the ~3k+ LOC hand-rolled SQL execution engine that
duplicates Cascades, after re-routing its only two remaining entry points (INFORMATION_SCHEMA +
explain-only) off it.
**Reviewers:** **Graefe** + Torvalds (query-engine change — a planner/executor surface removal).

---

## 1. Problem (verified real)

`pkg/relational/core/embedded` contains a complete second SQL execution engine — `execSelect` →
`execSelectQuery` → `execSelectQueryFull` → `execSelectJoin` / `aggregateMapRows` / `execSelectFromCTE`
/ `execUnion` / `materializeRecursiveCTE`, plus an FDB scan/pushdown layer (`pk_pushdown.go`,
`covering_index.go`, `secondary_index_pushdown.go`, …) — that re-implements WHERE / GROUP BY / HAVING /
join / CTE / UNION / aggregate / ORDER BY / LIMIT. This violates CLAUDE.md "No parallel pipelines: Go has
ONE query path (Cascades)."

**It is dead for real queries.** `connection.QueryContext` (`connection.go:468`) routes EVERY SELECT
through `newCascadesGenerator(c).Plan` → `planSelectCascades`. The interpreter is reached only via two
fallbacks in `planSelect` (`cascades_generator.go`):
1. `referencesInformationSchema(q)` → `c.execSelect` (`:172/:175`) — INFORMATION_SCHEMA system tables, a
   **Go-only extension Java rejects entirely**, so no cross-engine reference exists for it.
2. `planSelectExplainOnly` → `c.execSelect` (`:216`) — the ExecFn of explain-only mode, which the
   plan-equivalence harness **never invokes** (it only calls ExplainFn). Dead.

Because no real query exercises it, it **rots**: e.g. `aggregateMapRows`'s empty-implicit-group-under-
HAVING still mirrors the OLD Java 4.11 behaviour while the Cascades path was fixed to 4.12. That is the
exact class of latent divergence a parallel pipeline hides.

**Not wire format** (read-side execution). Removing it is purely a code/maintenance-surface reduction;
Cascades already produces the identical results for every real query.

## 2. Investigation (rigorous audit — corrected)

A first audit wrongly proposed deleting 16 files including the parser. **That is wrong:** the
PARSER/BRIDGE/EVAL layer is SHARED with Cascades and must be kept. Verified:
- `plan_visitor.go` (Cascades) calls `parseFromSource(simpleTable)` → `selectQueryFromClassification(cls,
  fs)` (`select_parser.go:516`) and threads `fs.joins []joinClause` + `selectClassification` through
  `visitFrom` / `visitSelectGroupBy` / the upgrade functions (`logical_predicate.go`
  `upgradeJoinOnPredicates`, the R7 outer-join infra). So `selectQuery`/`joinClause`/`fromSource`/
  `selectClassification`, `extractSelectParts`/`extractFromQueryTerm`/`parseFromSource`/`parseJoinClauses`/
  `extractJoinClause`/`classifySelectElements`/`selectQueryFromClassification`, the aggregate
  *classification* helpers (`extractAggFunc`/`countStarOutName`), and `colref.go`/`utilities.go`/
  `value_compare.go` are **SHARED — KEEP**.
- The **eval clusters** (`eval_proto.go` `evalExpr`, `eval_map.go` `evalExprOnMap`, `eval_predicate*.go`
  Tri family, `scalar_functions.go`, `scope.go` correlation helpers) are **SHARED**: `insert_cascades.go:104`
  uses `evalExpr` to fold constant INSERT-VALUES expressions; `system_tables.go:51` `filterSysRows` uses
  `evalPredicateOnMapExpr` for INFORMATION_SCHEMA WHERE. KEEP.

**The two blockers** (why it isn't a naive delete):
- **Blocker A — live INFORMATION_SCHEMA entry.** `execSelect` is reached at `cascades_generator.go:175`.
- **Blocker B — ≥5 eval back-edges into the executor (Graefe: the inventory must be complete, or Phase 2
  won't link).** The kept eval clusters reach the executor's `execQueryBodyRows` through the
  `SubqueryExpressionAtom` / `ExistsExpressionAtom` arms of the shared evaluators. Every one is a Phase-1
  sever point:
  1. `evalPredicateOnMapExprTri` EXISTS branch — `eval_predicate_map.go:502`.
  2. `evalHavingTri` EXISTS branch — `eval_predicate_map.go:67`.
  3. `evalScalarSubquery → runScalarSubqueryOnce` — `scalar_subquery.go:63`.
  4. `eval_map.go:220-221` (scalar-subquery atom) + `eval_predicate_map.go:231` → `evalScalarSubquery`;
     and `eval_map.go:275` (ExistsExpressionAtom) — reachable from `evalPredicateOnMapExprTri` (system-table
     WHERE) and from `evalExpr`'s SubqueryExpressionAtom branch at `eval_proto.go:279` (INSERT-VALUES).
  5. `eval_predicate.go:120` (`evalExprPredicateTri` EXISTS) — the kept `evalExpr` (INSERT-VALUES) routes
     into `evalExprPredicateTri` via `eval_proto.go:57,76,218`, so this evaluator is in the shared set and
     its EXISTS arm is a real sever point.
  So system-table WHERE filters and INSERT-VALUES *could* embed `EXISTS(...)` / `(SELECT …)` and re-enter
  the executor through any of these. (The 3-vs-5 undercount in an earlier draft is exactly why Phase 1's
  exit gate is `git grep execQueryBodyRows == 0` outside the island — §3.)
- **Feasibility (verified): both back-edges are UNEXERCISED.** No test/usage puts a subquery or EXISTS in
  an INFORMATION_SCHEMA WHERE; INSERT-VALUES expressions are constant (`insert_cascades.go:26-30`: "VALUES
  expressions are constant after parameter substitution"). INFORMATION_SCHEMA query shapes are simple
  (SELECT [*|cols] FROM INFORMATION_SCHEMA.X [WHERE …] [ORDER BY …] [LIMIT …]) — the system-table handlers
  support no joins/aggregates/CTEs against system tables anyway.

**The EXEC-ONLY deletable island (~3k+ LOC):** whole files `select_query_full.go`, `join.go`,
`cte_scan.go`, `union.go`, `recursive_cte.go`, `pk_pushdown.go`, `pk_prefix_pushdown.go`,
`like_prefix_pushdown.go`, `secondary_index_pushdown.go`, `covering_index.go`, `in_list_pushdown.go`,
`order_by.go`, `projection_fold.go`; plus EXEC-ONLY functions in mixed files (`execSelectQuery`/
`execQueryBodyRows`/`stripCTEColumnQualifiers`/`containsTableRef` in `select_dispatch.go`; `aggregateMapRows`
+ map-exec helpers in `aggregate.go`; `preEvaluateScalarSubqueries`/`walkScalarSubqueries*` in
`scalar_subquery.go`; `evalPredicate`/`evalExprPredicate`/`rejectTopLevelParenthesizedWhere` in
`eval_predicate.go`; `evalHaving*`/`groupByKey` in `eval_predicate_map.go`; `pushValidQualifiersScope` in
`scope.go`; the EXEC-ONLY halves of `select_helpers.go`/`where_extractors.go`).

## 3. Fix — 2 phases (sever, then delete)

### Phase 1 — detach the executor (re-route the 2 entries + sever the 3 back-edges)
1. **INFORMATION_SCHEMA route (Blocker A).** Replace the `execSelect` dispatch at `cascades_generator.go:172`
   with a minimal, executor-free system-table handler: parse the simple `SELECT … FROM INFORMATION_SCHEMA.X
   [WHERE] [ORDER BY] [LIMIT]` (reusing the kept parser), call the kept `execSystemTable` (catalog-
   synthesized rows), apply the kept `filterSysRows` (with the subquery-free evaluator from step 3) +
   `projectSystemRows`. No `execSelectQuery`/join/aggregate/CTE/UNION. Reject any INFORMATION_SCHEMA shape
   the handler doesn't support (join/aggregate/CTE/subquery) with a clean error — verified none are used.
2. **Explain-only ExecFn (Blocker A, dead).** Drop the `c.execSelect` call in `planSelectExplainOnly`
   (`:216`); its ExecFn is never invoked (the harness calls only ExplainFn, which uses the Cascades logical
   plan via `buildLogicalPlanForQueryWithCatalog`). Replace with an error stub.
3. **Sever ALL ≥5 eval back-edges (Blocker B).** The kept eval clusters (system-table WHERE +
   INSERT-VALUES) never use subqueries/EXISTS. Replace EVERY `SubqueryExpressionAtom` / `ExistsExpressionAtom`
   arm in the shared evaluators that reaches `execQueryBodyRows` — the full set from §2 Blocker B:
   `eval_predicate_map.go:67,231,502`, `eval_map.go:220-221,275`, `eval_predicate.go:120`,
   `scalar_subquery.go:63` (`evalScalarSubquery`/`runScalarSubqueryOnce`), and `eval_proto.go:279` — with a
   clean "subquery/EXISTS not supported in this context" error (the stub message already exists at
   `scalar_subquery.go:42`). System-table WHERE + constant INSERT-VALUES keep full non-subquery expression/
   predicate support; this is not a capability regression (those callers never had a working subquery shape).
   **Phase-1 EXIT GATE (Graefe):** `git grep execQueryBodyRows` (and `execSelectQuery`) shows ZERO callers
   outside the executor island, proving the sever is complete — run this BEFORE starting Phase 2. The
   `git grep == 0` check is the objective proof that would have caught the 3-vs-5 undercount.

### Phase 2 — delete the EXEC-ONLY island
Delete the 13 whole files + the EXEC-ONLY functions in the mixed files (§2 list), and their tests. Verify
the build links with no references into the deleted set. `git grep` for each deleted symbol = 0 hits.

## 4. Performance

Removes ~3k+ LOC + an entire divergence/maintenance surface. Zero runtime impact: Cascades already serves
every real query; INFORMATION_SCHEMA goes through a thinner direct handler (no executor recursion); the
explain harness is unchanged (ExplainFn). No cost-model surface.

## 5. Test plan

- **INFORMATION_SCHEMA parity (the risk):** a comprehensive test sweeping every system table
  (SCHEMATA/TABLES/COLUMNS/INDEXES + SHOW DATABASES/SCHEMA TEMPLATES) × {SELECT *, projected columns,
  WHERE filter, ORDER BY, LIMIT/OFFSET} — same rows before/after. Pin that an unsupported shape
  (join/aggregate/subquery against INFORMATION_SCHEMA) errors cleanly (verified none used today).
- **INSERT-VALUES:** constant VALUES (literals, arithmetic, function calls, params) still evaluate +
  insert correctly (the kept subquery-free `evalExpr`).
- **Severed-arm negative tests (Graefe):** one per severed subquery/EXISTS arm — `EXISTS(...)` and
  `(SELECT …)` in an INFORMATION_SCHEMA WHERE, and in INSERT-VALUES, each return the clean "subquery/EXISTS
  not supported in this context" error (not a panic, not silent wrong rows). Pins the severed behaviour so
  it can't silently regrow.
- **Phase-1 sever proof:** `git grep execQueryBodyRows` / `execSelectQuery` = 0 outside the executor
  island before Phase 2 (the exit gate).
- **Explain-only harness:** the plan-equivalence harness (ExplainFn) renders unchanged.
- **No Cascades regression:** full `//pkg/relational/sqldriver:sqldriver_test`, the cross-engine
  conformance corpus (live Java 4.12), and the R5–R7 suites (unnest/CARDINALITY/outer-join/EXISTS) all
  green. Determinism 10×.
- **Deletion proof:** `git grep <symbol>` = 0 for every deleted function; build + nogo clean.

## 6. Risk + rollback

Risk concentrates on INFORMATION_SCHEMA (the one live consumer). Mitigation: the comprehensive
before/after parity test + the verified absence of unsupported shapes. Phases are independently
committable: Phase 1 (sever) lands + is proven green BEFORE Phase 2 (delete), so a regression is caught at
the sever step with the executor still present (trivial rollback). Phase 2 is pure deletion — if the build
links and the suite is green, it's correct.

RFC-145. Graefe + Torvalds gate (query-engine surface removal). codex deferred to the Jun 25 quota reset;
PR stays draft for codex + @claude.
