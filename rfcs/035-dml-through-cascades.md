# RFC-035: DML executes through Cascades (P0.4)

**Status:** Mostly implemented — INSERT … VALUES, DELETE, UPDATE execute through
Cascades; INSERT … SELECT and naive-path deletion remain.
**Item:** P0.4 — DML must execute through Cascades (forbidden parallel pipeline)

## Implementation status

Landed (commits on `fix/p0.4-dml-through-cascades`), all 46 targets green:
- **INSERT … VALUES executes through Cascades end-to-end.** Reuses the Explode
  operator (RecordConstructorValue → ArrayConstructorValue → ExplodeExpression →
  InsertExpression inner); plan-time validation (arity 42601/22000, NOT NULL,
  "expected Record but got Primitive", type mismatch 22000); enum/nested fidelity
  via carried `protoreflect.Value`. `planOne` routes INSERT VALUES to `planDML`.
- **`cascadesPlan.IsUpdate()` derived** from physical plan type + (implicit)
  explain gate, matching `QueryPlan.isUpdatePlan()`.
- **`RowsAffected` counting** (`countAll`, Java's `countUpdates`) and **`runInTx`**
  so DML joins an open explicit transaction (Gap B).
- `executeInsert` Datum→message bridge for computed-row inners.

Done since: **DELETE** and **UPDATE** also execute through Cascades.
- DELETE: simple/schema-qualified/correlated + non-correlated EXISTS. Fixing the
  non-correlated semi-join (FirstOrDefault defeated the presence check) also fixed
  a latent SELECT bug.
- UPDATE: SET RHS resolved to Values (was raw text); WHERE via
  `upgradeDMLWhereWithCatalog`; plan-time NOT-NULL + unsupported-function
  rejection. Corner-case tests in `dml_cascades_fdb_test.go`.

Remaining:
1. **INSERT … SELECT through Cascades.** `executeInsert` prefers the source
   `qr.Record` over the projected `qr.Datum`, re-saving source rows (PK collision,
   23505). Fix: map the projected row to the target columns (positionally, like
   Java), then flip INSERT … SELECT.
2. **Delete naive `execInsert`/`execUpdate`/`execDelete`/`execInsertSelect`** and
   the `execStatement` DML dispatch; repoint `planDMLExplainOnly.ExecFn`; reword
   QueryContext DML rejection; record the QueryContext-rejection divergence in
   `DIVERGENCES.md`.

Reviewer follow-ups (direction check-in, both ACK):
- Graefe: push the target `Type.Record` down into the INSERT/UPDATE Value tree
  and land `PromoteValue` at plan time (instead of `ConstantValue{UnknownType}` +
  executor coercion) so the plan is fully typed; fold `LogicalInsert.ValuesArray`
  and `Source` into a single inner-producing child so `translateInsert` stops
  branching.
- Torvalds: delete each naive `exec*` in the same commit its shape flips (don't
  leave a resting dual-state); `upgradeDMLWhereWithCatalog` duplicates the SELECT
  exists-wiring — factor to avoid drift (e.g. `correlatedScalarSubqueries`).

---

**Original design follows.**

## Problem

Go has **two** DML pipelines, which CLAUDE.md explicitly forbids ("Java has one
query path (Cascades). Go has one query path (Cascades). Don't maintain a naive
fallback alongside Cascades"):

1. **Naive path** — `ExecContext` → `newCascadesGeneratorForExec` (`execMode=true`)
   → `planOne` (`cascades_generator.go:124-128`) routes DML to `planDDL` →
   `execStatement` (`connection.go:511`) → `execInsert`/`execUpdate`/`execDelete`.
   These hand-roll record save/delete directly against the store, bypassing the
   planner and executor.

2. **Cascades path** — `QueryContext` (`execMode=false`) → `planDML`
   (`cascades_generator.go:563`) builds an `InsertExpression`/`UpdateExpression`/
   `DeleteExpression`, runs the Cascades planner, extracts a
   `RecordQueryInsertPlan`/`UpdatePlan`/`DeletePlan` — and is then **rejected** at
   `connection.go:359` (`"only SHOW and SELECT statements are supported"`). The
   Cascades DML plan is built and thrown away.

Two paths that drift, double maintenance, and hide Cascades DML gaps (e.g. the
`"DML plan extraction failed"` INSERT gap at `cascades_generator.go:631`) behind
the naive fallback. Surfaced by RFC-034: `planDML`'s metrics hook fires but logs
a throwaway plan.

## Investigation

### Java (the reference — one path)

- `PlanGenerator.getPlan(sql)` is the **single** entry for SELECT *and* DML — no
  branch by statement type (`PlanGenerator.java:127`).
- DML logical exprs (`InsertExpression`/`UpdateExpression`/`DeleteExpression`)
  are implemented to physical plans (`RecordQueryInsertPlan`/`UpdatePlan`/
  `DeletePlan`) by Cascades rules (`ImplementInsertRule` etc.), exactly like
  SELECT operators.
- **DML plans emit one result row per affected record.** Both SELECT and DML
  produce `Plan<RelationalResultSet>`. `QueryPlan.isUpdatePlan()` returns true
  iff the physical plan is an Insert/Update/Delete plan.
- The statement layer (`AbstractEmbeddedStatement.clockAndExecuteQueryPlan`)
  runs the plan, then **if `isUpdatePlan()` counts the emitted rows**
  (`countUpdates`, `AbstractEmbeddedStatement.java:212`) and returns the count,
  committing once; otherwise returns the result set. `executeUpdate`/
  `executeQuery` are pure method-routing on top of that boolean.

### Go (current state)

- The Cascades DML **executor already exists and emits rows**: `executeInsert`/
  `executeUpdate`/`executeDelete` (`executor/executor.go:1421-1565`), wired in
  `ExecutePlan`. They iterate the inner cursor, mutate the store, and emit a
  `QueryResult` per affected record — identical to Java.
- `planDML` already produces a `cascadesPlan{isUpdate:true, physicalPlan:…}`.
- **Gap A — no count.** `cascadesPlan.Execute` (`cascades_generator.go:747`)
  always returns `Result{Rows: pr}` and never sets `RowsAffected`. The
  `query.Result` contract (`query/plan.go:72`) already has `RowsAffected`; the
  Cascades path just never populates it. Separately, `cascadesPlan` stores a
  `isUpdate bool` flag set at construction (`cascades_generator.go:646,727,733`).
  Java does **not** store this — `QueryPlan.isUpdatePlan()` *derives* it from the
  physical plan type (`recordQueryPlan instanceof RecordQueryInsert/Update/
  DeletePlan`) and returns **false when `isForExplain()`** (`QueryPlan.java:229`).
  A stored flag risks counting rows / committing mutations for an EXPLAIN'd DML
  and silently drifts from the plan type.
- **Gap B — transactions.** `paginatingRows.fetchPage` (`cascades_generator.go:957`)
  executes via bare `c.sess.DB.Run` (auto-commit per page). The naive DML path
  uses `runInTx` (`connection.go:172`), which honors `activeTx` (an explicit
  `BeginTx` transaction). Routing DML through the current `fetchPage` would
  **auto-commit mutations regardless of an open explicit transaction** — a
  correctness regression. Explicit-transaction DML is tested
  (`embedded_test.go`, `embedded_fdb_test.go`).
- **Gap C — INSERT VALUES has no Cascades path at all.** This is the big one.
  `LogicalInsert` carries only `{Table, Columns, Source}` (`operators.go:372`)
  and **drops the literal VALUES rows entirely** — for the VALUES form `Source`
  is always nil (`logical_builder.go:602`). `translateInsert` then builds an
  `InsertExpression` with a **nil inner quantifier** (`cascades_translator.go:1118`),
  and `executeInsert` iterates `p.GetInner()` (`executor.go:1469`) — a nil inner
  inserts zero rows. There is no values-materialization operator wired for INSERT.
  The `"DML plan extraction failed"` error is a symptom, not the disease:
  extraction isn't the problem — **there is nothing to insert.** All INSERT
  VALUES behavior (multi-row iteration, arity validation with 42601/22000, NOT
  NULL enforcement, `ConvertToProtoValue` coercion, ErrorIfExists) lives **only**
  in the naive `execInsert` (`insert.go:88-259`). Deleting it deletes the only
  working INSERT VALUES implementation. (INSERT…SELECT, UPDATE, DELETE *do* build
  a Cascades inner via `Source`/predicate scans — but their Cascades execution is
  unproven end-to-end because `connection.go:359` rejects them before they run.)

### Java's INSERT VALUES architecture (the port target)

Java materializes literal VALUES rows **without** a dedicated values-scan: each
row becomes a `RecordConstructorValue`, the N rows are wrapped in one homogeneous
array `Value` (the `array` built-in / `LightArrayConstructorValue`), and an
`ExplodeExpression` (logical) / `RecordQueryExplodePlan` (physical) streams the
array element-by-element as the inner `ForEach` of `InsertExpression`:

```
RecordQueryInsertPlan
  └─ ForEach
       └─ RecordQueryExplodePlan( array[ RecordConstructorValue(row0), …(row1), … ] )
```

Crucially, **arity, NOT NULL, and type coercion all happen at PLAN time in the
visitor** (`ExpressionVisitor.parseRecordFieldsUnderReorderings`,
`coerceValueIfNecessary` — `PromoteValue`/`NullValue` per field, target type
pushed down), not at runtime. The Explode/Insert plans need zero special-casing.

**Go already has every building block:** `ExplodeExpression`
(`expressions/explode.go`, holds a `collectionValue`), `RecordQueryExplodePlan`
(`plans/explode.go`, streams an array Value's elements) + `ImplementExplodeRule`
+ `physical_explode_wrapper.go` (used today for IN→explode), plus
`NewArrayConstructorValue(elementType, elements)`, `NewRecordConstructorValue`,
`PromoteValue`, `NullValue`, `QueriedValue`. The work is **wiring + moving
validation to plan time**, not inventing a new operator.

## Fix

One query path. DML plans like SELECT plans; the *statement layer* decides
count-vs-rows, exactly like Java.

1. **Unify planning.** Delete the `execMode` DML branch in `planOne`
   (`cascades_generator.go:124-128`): INSERT/UPDATE/DELETE **always** route to
   `planDML`. DDL and transaction statements stay on `planDDL`/`execStatement`
   (Java treats DDL as a non-query plan too — out of scope).

2. **Derive update-ness, then count emitted rows (Gap A).** Replace the stored
   `isUpdate bool` with a `IsUpdate()` that **derives** the answer from the
   physical plan type — `_, ok := physicalPlan.(*plans.RecordQueryInsertPlan)`
   etc. — and returns false in explain mode, matching Java's
   `QueryPlan.isUpdatePlan()` (`QueryPlan.java:229`; Principle 10: emergent
   property, not a bolted-on flag). When `IsUpdate()` is true, `Execute` drains
   the emitted affected-record rows, counts them, and returns
   `Result{RowsAffected: n}` (Go's `countUpdates`). `ExecContext` already reads
   `result.RowsAffected` (`connection.go:329`) — no change there. EXPLAIN of a
   DML statement therefore returns plan-text rows and performs **no** mutation.

3. **Respect explicit transactions (Gap B).** DML execution must run through
   `runInTx`, not bare `DB.Run`, so mutations join an open `activeTx` and commit
   only on explicit `COMMIT` (auto-commit otherwise). SELECT pagination's use of
   `DB.Run` is existing behavior and stays as-is (separate concern; not widened
   here to avoid scope creep).

4. **Build the INSERT VALUES Cascades path (Gap C).** Port Java's
   array-of-RecordConstructors + Explode shape, reusing Go's existing operators:
   - **Capture the rows as an explode child** (per Graefe: don't grow a parallel
     `LogicalInsert` field — keep `translateInsert` uniform with UPDATE/DELETE).
     Build a `RecordConstructorValue` per row, wrap the N rows in
     `NewArrayConstructorValue(elementType, rows)`, and set it as
     `LogicalInsert.Source` via a child values/explode operator (so `Source` is
     non-nil for VALUES just like INSERT…SELECT). The element type is the target
     record type.
   - **Coerce + validate at plan time** (matching Java's visitor, not the
     executor): per-field push the target column type down and inject
     `PromoteValue` where promotion is needed; fill omitted columns with
     `NullValue` only if the column is nullable else raise NOT NULL
     (`NOT_NULL_VIOLATION`); reject arity mismatch (too many → `42601`
     SYNTAX_ERROR with explicit cols / `22000` CANNOT_CONVERT_TYPE without).
     These are the exact checks `execInsert` does today, moved to the
     value-construction site.
   - **Translate.** `translateInsert` wraps the array in an `ExplodeExpression`,
     makes a `ForEachQuantifier`, and feeds it as `InsertExpression`'s inner —
     no more nil quantifier. `ImplementExplodeRule` + `ImplementInsertRule`
     produce `RecordQueryExplodePlan` → `RecordQueryInsertPlan`. Extraction then
     succeeds because there is a real plan to extract.
   - **Executor.** `executeInsert` already iterates the inner cursor; verify
     `RecordQueryExplodePlan` streams the array elements as records and that
     `ErrorIfExists` duplicate-PK semantics (`23505`) are preserved.
   - Pin with the INSERT VALUES e2e tests below; only then is Gap C closed.

5. **QueryContext stays a rejection — but honest.** `database/sql` routes
   `db.Exec` → `ExecContext` (DML) and `db.Query` → `QueryContext` (rows). DML
   via `Query()` is a method misuse, the analog of Java's `executeQuery` on an
   update plan. Keep rejecting it, but reword the misleading
   `"only SHOW and SELECT statements are supported"` to direct callers to
   `Exec()`. The rejection is now purely statement-layer method-routing — not a
   "Cascades can't do DML" limitation. (We do **not** replicate Java's
   execute-then-throw side effect: rejecting before mutation is cleaner and
   avoids surprise writes. Record this in `DIVERGENCES.md`.)

6. **Verify UPDATE/DELETE/INSERT…SELECT parity, then delete the naive
   executors.** UPDATE/DELETE/INSERT…SELECT already build a Cascades inner but
   have never executed (rejected at `connection.go:359`). Before deleting
   anything, prove each runs end-to-end through Cascades with correct row effects
   and counts (tests below); fix any execution gaps found (same DFS rule as Gap
   C — no papering over). **Only once every DML shape has proven Cascades
   parity** remove `execInsert`, `execUpdate`, `execDelete`, `execInsertSelect`,
   and their dispatch in `execStatement` (`connection.go:510-548`).
   `planDMLExplainOnly`'s `ExecFn` (which delegates to the naive exec* funcs) is
   only reachable in explain-only mode where `ExecFn` is never called — repoint
   it to an error so no dead exec* reference survives.

### Sequencing

Land in this order, each gated on green tests (the naive path keeps DML working
throughout, so there is no window of broken DML):
1. INSERT VALUES Cascades path (§Fix.4) + plan-time validation, tested in
   isolation against `planDML` (still reachable only via the would-be path).
2. Derived `IsUpdate()` + `RowsAffected` counting (§Fix.2, Gap A).
3. `runInTx` for DML execution (§Fix.3, Gap B).
4. Flip `planOne` to always `planDML` (§Fix.1); verify UPDATE/DELETE/INSERT…SELECT
   parity (§Fix.6 first half).
5. QueryContext reword (§Fix.5).
6. Delete naive executors (§Fix.6 second half).

## Performance

- DML now pays Cascades planning cost (~SELECT planning), vs the naive path's
  direct save. For a single-row `INSERT … VALUES` this is more work, but it is
  the same trade Java accepts and DML is `PlanCacheSkip` either way. The win —
  eliminating a divergent pipeline — outweighs micro-overhead on trivial DML.
- No extra FDB round-trips: the executor already does exactly the store
  mutations the naive path did. The counting drain is required regardless (Java
  counts too) and is the same cursor iteration the naive path performed.
- Pagination/continuation model unchanged. Per-page time limit (`txPageTimeLimit`,
  4s) still bounds each transaction.

## Test plan

E2E (FDB integration + yamsql), all asserting both row effect **and** plan shape:

- `INSERT … VALUES` (single + multi-row) → `RowsAffected` correct, row persisted;
  `EXPLAIN` shows `RecordQueryInsertPlan` over `RecordQueryExplodePlan` (proves
  the values-explode path fires, not a fallback).
- INSERT VALUES validation parity (all moved to plan time, must match
  `execInsert`'s codes): too-many-values with explicit cols → `42601`;
  arity mismatch without cols → `22000`; NULL into NOT NULL column →
  `NOT_NULL_VIOLATION`; omitted nullable column → NULL stored; literal type
  coercion (e.g. int literal into a long/double column) succeeds; duplicate PK →
  `23505` (ErrorIfExists preserved).
- `INSERT … SELECT` → rows copied, count correct (replaces `execInsertSelect`).
- `UPDATE … WHERE` on an indexed predicate and on a full scan → count + mutated
  values correct; `EXPLAIN` shows `RecordQueryUpdatePlan` over the expected scan.
- `DELETE … WHERE` → count + remaining rows correct; `EXPLAIN` shows
  `RecordQueryDeletePlan`.
- **Explicit transaction** (Gap B): `BeginTx` → INSERT/UPDATE/DELETE → row not
  visible to a concurrent auto-commit read until `COMMIT`; `ROLLBACK` discards.
  Proves `runInTx`/`activeTx` is honored.
- **Multi-statement batch** still aggregates `RowsAffected` (`MultiPlan`).
- **Gap C regression**: the previously-failing INSERT shape now extracts a plan.
- `db.Query("INSERT …")` returns the reworded "use Exec()" error (no mutation).
- Determinism: 10× runs on the DML EXPLAIN tests (planner stability).
- Grep proves `execInsert`/`execUpdate`/`execDelete`/`execInsertSelect` are gone
  (no dead code).
