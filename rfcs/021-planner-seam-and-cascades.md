# RFC 021: Planner seam + Cascades optimizer

## Status: Draft

## Problem

`pkg/relational/core/embedded/connection.go` is a 12,000-line god file.
It implements `database/sql/driver.Conn`, the parser-to-selectQuery
extractor, every SELECT / JOIN / UNION / CTE / aggregate / DDL / DML
execution path, all pushdown extractors + cursor builders, the scalar
function library, and system-table handlers — in one file, one
package-private namespace, with 204 functions.

Two concrete consequences:

1. **Cascades can't land.** Cascades is a rule-based optimizer: rules
   pattern-match a LogicalOperator IR and rewrite into a
   `RecordQueryPlan`. Today's execution path walks the ANTLR parse tree
   directly (`execSelectQueryFull` reads ANTLR context nodes inline and
   emits rows). There is no LogicalOperator IR, no `Plan` interface, no
   boundary where a planner could plug in. Cascades has nowhere to
   connect.

2. **"Frontend-neutral" is false.** `database/sql/driver.Conn` is supposed
   to be a thin adapter over an SQL engine. Ours carries the engine.
   Any second frontend (gRPC, REPL, embedded-in-process API without
   the `database/sql` layer) would either duplicate everything or
   depend on `embedded.Conn`. Neither is viable.

Java's `fdb-relational-*` module split doesn't have this problem
because the build system forced a boundary. `fdb-relational-jdbc`
physically cannot depend on `fdb-relational-core`'s execution internals
— it's a separate maven module. Go's single-package `embedded` gave
us no equivalent forcing function, and we let the monolith grow.

## What Java does (reference architecture)

Five modules, each a maven artifact:

```
fdb-relational-api/       Interfaces only. RelationalConnection,
                          RelationalStatement, RelationalResultSet,
                          DataType, Column, Schema, SchemaTemplate.
                          Zero execution code. Pure contract.

fdb-relational-core/      The engine.
  recordlayer/
    AbstractDatabase, Catalog                 resource handles
    EmbeddedRelationalConnection   591 lines  session state + txn
                                              lifecycle. NO SQL logic.
    AbstractEmbeddedStatement      274 lines  40-line execute(sql):
                                              plan := PlanGenerator.plan(sql)
                                              plan.execute(ctx)
                                              return wrap(plan.result)
    ddl/                                      DDL handlers
    catalog/                                  system tables, schemas
    metadata/                                 metadata types
    query/                43 files            parser → LogicalOperator →
                                              PlanGenerator; SemanticAnalyzer;
                                              Plan interface; visitors;
                                              functions/; cache/.

fdb-relational-grpc/      gRPC service wrapping an embedded connection
                          as a network endpoint.

fdb-relational-jdbc/      JDBC driver. Thin adapter — delegates every
                          method to a RelationalConnection, either
                          directly (embedded) or over gRPC (remote).

fdb-relational-server/    Standalone process; main() + grpc service.
fdb-relational-cli/       sqlline-based REPL (fdb-relational-sqlline).
```

The hinge is one interface:

```java
interface Plan<T> {
    T execute(ExecutionContext ctx);
    boolean isUpdatePlan();
}
```

Three subclasses: `QueryPlan` (wraps a Cascades-produced
`RecordQueryPlan`), `ProceduralPlan` (DDL), `ContinuationPlan`
(paginated resume). `AbstractEmbeddedStatement.executeInternal` is
40 lines of dispatch — it does not know SELECT from INSERT from JOIN.

## Proposed architecture

We are close to Java's structure already — `pkg/relational/api/`,
`core/parser/`, `core/catalog/`, `core/metadata/`, `sqldriver/` are
distinct packages. The monolith is the execution half of
`core/embedded/`. Two-phase refactor:

### Phase 1 — Seam (the mechanical refactor)

Define the planner / plan boundary without changing any execution
semantics. Cascades does not land in this phase.

**New packages:**

```
pkg/relational/core/
  session/              ← NEW. ~500 lines.
    session.go              Session type. Holds: FDB txn, catalog
                            handle, current schema, options,
                            correlated-subquery scope stack, CTE stack,
                            source-alias stack. Created per logical
                            SQL session; one per database/sql conn but
                            reusable from gRPC.
    options.go              Connection-level options.

  query/                ← NEW. ~3,500 lines (moved from connection.go).
    plan.go                 Plan interface:
                              type Plan interface {
                                  Execute(ctx context.Context, sess *Session) (Result, error)
                                  IsUpdate() bool
                              }
                            Result interface (Cols, Next, Close).
    generator.go            PlanGenerator. Entry point: Plan(sql string,
                            sess *Session) (Plan, error). Wraps parse +
                            semantic analyze + plan-build.
    parser.go               Thin wrapper over pkg/relational/core/parser.
    semantic.go             SemanticAnalyzer — resolves schema/table/
                            column refs, type-checks, builds logical IR.
    logical/                LogicalOperator IR.
      operator.go             LogicalOperator interface.
      select.go               LogicalSelect.
      join.go                 LogicalJoin.
      aggregate.go            LogicalAggregate.
      union.go                LogicalUnion.
      cte.go                  LogicalCTE.
      filter.go               LogicalFilter (WHERE).
      project.go              LogicalProject.
      sort.go                 LogicalSort.
      limit.go                LogicalLimit.
    visitors/               Parse-tree → logical IR visitors (port of
                            Java's recordlayer/query/visitors/).

  plan/                 ← NEW. ~2,000 lines.
    query_plan.go           QueryPlan — wraps a physical plan tree.
    procedural_plan.go      DDL.
    continuation_plan.go    Resume from continuation token.
    physical/               Physical operator tree (pre-Cascades impl).
      scan.go                 Scan (full / pk-equality / pk-range /
                              idx-equality / idx-range / ...).
      filter.go               Filter.
      project.go              Project.
      nested_loop_join.go     NestedLoopJoin.
      hash_aggregate.go       HashAggregate.
      sort.go                 Sort (stable, in-memory).
      limit.go                Limit (with offset + early-termination).
      union.go                UnionAll + UnionDistinct.

  functions/            ← NEW. ~1,200 lines (moved from connection.go).
    registry.go             map[string]FuncImpl dispatch.
    string.go               UPPER/LOWER/TRIM/CONCAT/REPLACE/SUBSTRING/…
    math.go                 ABS/CEILING/FLOOR/ROUND/…
    date.go                 CURRENT_TIMESTAMP/YEAR/MONTH/…
    cast.go                 castValue, convertToProtoValue.

  eval/                 ← NEW. ~700 lines (collapsed from ~1,500 in two
                        paths in connection.go).
    expr.go                 Value + predicate evaluator. ONE path —
                            today's proto vs map divergence goes away
                            because physical/ operators produce a
                            uniform Row representation.
    tri_bool.go             Kleene three-valued logic.
```

**Existing packages shrink:**

```
pkg/relational/core/embedded/
  conn.go                 ~400 lines. driver.Conn impl: Begin, Close,
                          Prepare, Exec, Query, IsValid, ResetSession.
                          Holds a *query.Session and a *query.Generator.
  stmt.go                 ~300 lines. driver.Stmt impl.
  rows.go                 ~200 lines. driver.Rows impl wrapping a
                          query.Result.
  params.go               ~150 lines. substituteParams.
```

The sqldriver package remains the outer boundary. `embedded.Conn` is
a database/sql-flavoured adapter over `query.Session` + `query.Plan`.
A future `pkg/relational/grpc/` server is the same pattern over a
different wire.

**Refactor steps (no behaviour change):**

1. Create `pkg/relational/core/session/`. Move session state (txn,
   schema, options, outer-scope stacks, CTE stack) out of `Embedded
   Connection` into `Session`. `Conn` now holds a `*Session`.
2. Create `pkg/relational/core/query/plan.go` with the `Plan` interface.
3. Create `pkg/relational/core/plan/physical/` with one-to-one physical
   operator types for each today's execution shape. This is the
   **naive planner** — no rules, no cost, no rewrites. Just a direct
   translation of today's `execSelect*` code paths.
4. Move `execSelectQueryFull` logic out of connection.go and into the
   appropriate physical operator's `Execute()` method. Same line count,
   different file. Each of today's hand-rolled branches (PK equality /
   IN-list / range / composite / secondary / full scan) becomes one
   physical operator type or one flag on a Scan operator.
5. Move `extractFromSimpleTable` logic into `core/query/visitors/` —
   the visitor builds a `logical.LogicalSelect`, then `NaivePlanGenerator`
   picks the physical operator based on today's pushdown-chain
   conditions.
6. Move `evalScalarFunctionCallCore` into `core/functions/`. Split by
   family. Registry map for dispatch. Same line count total; just one
   file per family.
7. Move `evalExprAtom` / `evalExprAtomOnMap` into `core/eval/`. Collapse
   the proto-vs-map divergence by running physical operators against a
   `Row` interface (`Get(col) (Value, bool)`) with implementations for
   `*proto.Message` and `map[string]driver.Value`.
8. `embedded.Conn.Exec/Query` reduces to:

   ```go
   plan, err := c.generator.Plan(sql, c.session)
   if err != nil { return nil, err }
   result, err := plan.Execute(ctx, c.session)
   if err != nil { return nil, err }
   return wrapResult(result), nil
   ```

After Phase 1:
- `connection.go` deleted. Replaced by `conn.go` + `stmt.go` + `rows.go`
  + `params.go` totaling ~1,000 lines.
- `pkg/relational/core/query/` holds parser-to-IR + planning.
- `pkg/relational/core/plan/` holds the physical plan + executor.
- `pkg/relational/core/functions/` + `eval/` + `session/` hold
  orthogonal pieces.
- **Zero behaviour change.** Same 94 yamsql scenarios green. Same 28
  bazel targets green. This is a pure rearrangement.

### Phase 2 — Cascades

With the seam in place, Cascades is additive. A second planner
implementation.

**New package:**

```
pkg/recordlayer/plan/cascades/
  memo/
    memo.go                 Memo + equivalence classes + Reference.
    group.go                Group (set of logically-equivalent expressions).
  expressions/
    relational.go           RelationalExpression (logical / physical
                            operators as a unified type tree).
    values.go               Value — column refs, literals, arithmetic,
                            function calls, case expressions, etc.
    predicates.go           Predicate — ==, <, IN, LIKE, AND, OR, NOT,
                            IS NULL; tri-state aware.
    typing.go               Type system integration.
  rules/
    rule.go                 Rule interface + PlannerRule + Matcher.
    transformation/         Logical-to-logical rewrites. Port from
                            Java's `transformation-rules/` directory.
    implementation/         Logical-to-physical.
    optimization/           Physical-to-physical (MergeFetchIntoCovering
                            IndexRule, PushPredicateThroughDistinctRule,
                            MergeFetchIntoTypeFilterRule, …).
  planner.go                CascadesPlanner. Task stack, EXPLORE phase
                            → OPTIMIZE phase.
  cost.go                   Cost model.
  tasks/                    Planner task queue (ExploreGroupTask,
                            OptimizeExpressionTask, ApplyRuleTask, …).
  explain.go                EXPLAIN output.
```

Integration: `query.PlanGenerator` gains a second implementation.

```go
type PlanGenerator interface {
    Plan(sql string, sess *Session) (plan.Plan, error)
}

// Today (Phase 1):
type NaivePlanGenerator struct{...}

// After Phase 2:
type CascadesPlanGenerator struct{
    planner *cascades.Planner
}
```

A feature flag or options field on `Session` picks which generator runs.
Default flips to Cascades after parity is verified against the yamsql
corpus.

**What happens to swingshift-44's pushdown chain:**

- The 11-branch `execSelectQueryFull` pushdown chain → 11 Cascades rules
  (plus a handful more that were impossible to express in the
  hand-rolled chain).
- `canCoverIndex` / `naturalOrderSatisfies` / `earlyTermTarget` → plan
  properties propagated automatically by the memo.
- `secondaryIndexInListCursor` etc. → disappear; Cascades emits the
  same physical-scan shape via rule application.
- All 94 yamsql scenarios → regression corpus for the Cascades output.
  That is the real value of the swingshift-44 work: the tests are the
  spec, the code is disposable.

### Phase 3 — Second frontend (validation of the seam)

Once Phase 1 is done, validate the seam by adding one more frontend.
Concrete candidate: a gRPC service that exposes `Session.Execute(sql)`
as an RPC. This is the Java `fdb-relational-server` + `fdb-relational-
grpc` analogue.

If the seam is clean, the gRPC implementation is ~500 lines: request/
response proto messages, a service wrapper, a streaming result-set
adapter. No execution code touches it.

If the gRPC implementation ends up reaching into `embedded.Conn`'s
private state, the seam failed and Phase 1 needs more work.

## Line-count accounting

| Concern                           | Today (connection.go) | Phase 1 destination                        | Net lines |
|-----------------------------------|----------------------:|--------------------------------------------|----------:|
| `driver.Conn` impl                |             ~400      | `embedded/conn.go`                         |     ~400  |
| `driver.Stmt` / `Rows`            |             ~300      | `embedded/stmt.go` + `rows.go`             |     ~500  |
| substituteParams                  |             ~110      | `embedded/params.go`                       |     ~150  |
| selectQuery + extractors          |           ~1,300      | `core/query/visitors/`                     |   ~1,500  |
| execSelect* / JOIN / UNION / CTE  |           ~2,500      | `core/plan/physical/`                      |   ~1,800  |
| Aggregate exec                    |             ~500      | `core/plan/physical/hash_aggregate.go`     |     ~400  |
| DML (INSERT/UPDATE/DELETE)        |             ~600      | `core/plan/procedural/` + thin exec.go     |     ~600  |
| Pushdown extractors + cursors     |           ~2,000      | `core/plan/physical/scan.go` + rule stubs  |   ~1,500  |
| Scalar functions                  |             ~900      | `core/functions/`                          |     ~900  |
| Expression evaluators             |           ~1,500      | `core/eval/`                               |     ~700  |
| System tables (INFORMATION_SCHEMA)|             ~500      | `core/system/`                             |     ~500  |
| DDL                               |           ~1,000      | `core/plan/procedural/ddl/`                |   ~1,000  |
| **Total**                         |         **~12,000**   |                                            | **~10,000** |

connection.go proper: 12,000 → ~1,000 lines (a 92% reduction of the
file). Total relational execution code: ~12,000 → ~10,000 lines
(collapse of the proto-vs-map evaluator duplication) — but spread
across ~30 focused files instead of one.

Phase 2 adds ~5,000 lines of Cascades infrastructure to
`pkg/recordlayer/plan/cascades/` and obsoletes most of
`core/plan/physical/`'s hand-rolled planning (the physical operators
themselves stay; just the naive "if-else pushdown chain" goes away).

## Risk + non-goals

**Non-goals for Phase 1:**
- Changing any query semantics. Every yamsql scenario must pass
  byte-identically. The only acceptable diff is "same rows emitted
  via a different code path."
- Introducing Cascades concepts (memos, rules, cost, equivalence
  classes) in the naive planner. Phase 1 is the seam only.
- Renaming existing public API. `sql.Open("fdbsql", ...)` still works.
  `pkg/relational/api` interfaces unchanged.
- Wire-format changes.

**Non-goals for Phase 2:**
- Full parity with Java's Cascades. ~69 rules in Java; we pick the
  highest-ROI subset (index selection, predicate pushdown, join
  reordering for small-N cases, covering-index merging) first. Rule
  parity is continuous, not a one-shot port.
- Cost model tuning. V1 uses a simple row-count-based cost; tuning
  comes after correctness parity.

**Risk:**
1. **Refactor scope creep.** Phase 1 is deliberately mechanical —
   moving code to new files, introducing interfaces, collapsing
   duplicate evaluator paths. It is very easy to "just fix this small
   thing while I'm here" and lose the behaviour-preserving property.
   Each PR in Phase 1 should pass every yamsql scenario unchanged.
2. **Evaluator collapse hides divergence.** The proto-vs-map split in
   today's code has accumulated subtle differences over time (nightshift
   -36 found several). Collapsing them behind one `Row` interface
   must not silently paper over remaining divergences. Mitigation: the
   collapse is its own PR, separate from the file-move PRs, and runs
   the full yamsql suite before and after.
3. **Java-hash cache-key stability gets harder with Cascades.** Today
   the plan cache is keyed by SQL string. With Cascades, the key should
   be a normalized plan-tree hash that matches Java's hash. This was
   already unchecked (TODO.md line 635). Phase 2 needs an explicit
   task for it.
4. **Some pushdown shapes don't fit a clean rule** — e.g. the LIKE
   prefix extractor with ESCAPE handling is not trivially expressible
   as a pattern match. Mitigation: the rule that recognizes LIKE
   predicates dispatches to a helper function for prefix extraction;
   not every line of today's code becomes a pure rule.
5. **Team velocity during the refactor.** Feature work lands on top of
   a moving target if Phase 1 takes multiple shifts. Mitigation: one
   feature freeze during Phase 1, or the refactor goes on a dedicated
   `relational-refactor` branch and feature PRs rebase onto it.

## Phasing + effort estimate

| Phase | Scope                                             | Shifts    | Merge gate                           |
|-------|---------------------------------------------------|-----------|--------------------------------------|
| 1a    | Session + Plan interface + naive PlanGenerator    | 1         | All yamsql green; no feature flag.   |
| 1b    | Move exec*/eval*/functions out of connection.go   | 2–3       | All yamsql green on each PR.         |
| 1c    | Collapse proto-vs-map evaluator paths             | 1         | All yamsql green + manual diff.      |
| 2a    | Cascades memo + Value / Predicate IR + parity     | 3–4       | Cascades produces rows-identical     |
|       | for SELECT without pushdown                       |           | output on yamsql subset.             |
| 2b    | Core rules: filter pushdown, index selection,     | 3–4       | Yamsql subset green on Cascades;     |
|       | covering-index, sort elimination                  |           | naive planner still default.         |
| 2c    | Flip default to Cascades; retire naive physical   | 1–2       | All yamsql green on Cascades; naive  |
|       | planner                                           |           | removed.                             |
| 3     | gRPC frontend as seam validation                  | 2         | A SELECT via gRPC returns the same   |
|       |                                                   |           | rows as a SELECT via database/sql.   |

Total: ~14–17 shifts. Phase 1 is the gating investment — ~4–5 shifts
of mechanical work that unlocks everything after it.

## Open questions

1. **Does `Session` live in `pkg/relational/core/session/` or
   `pkg/relational/api/`?** Java puts it in `fdb-relational-api` as an
   interface with a concrete impl in core. We could do the same, which
   would let a future non-embedded `Session` impl (e.g. a gRPC client's
   local proxy) satisfy the same contract. But the interface-first
   pattern is heavier than Go usually needs. Lean toward a concrete
   `Session` in `core/session/` for now; extract an interface when a
   second implementation arrives.

2. **How do continuations survive the refactor?** Today the continuation
   token encodes the cursor state (record-layer continuation + per-row
   index for IN-list sub-scan chains). With Cascades, the token should
   encode the plan-tree state instead — similar to Java's
   `ContinuationImpl`. Phase 2b problem.

3. **Feature flag or hard flip for Cascades?** Feature flag keeps the
   naive planner around as a fallback during the parity verification.
   Adds maintenance burden (two codepaths). Hard flip is cleaner but
   riskier. Lean toward feature flag during Phase 2a–2b, hard flip in
   Phase 2c.

4. **Does the `core/functions/` registry need to match Java's function
   dispatch mechanism?** Java's `BuiltInFunction` and the Cascades
   Value subclasses are tightly coupled. If we want Java-compatible
   plan-tree hashes (open question 3 above), we need registry keys
   that line up with Java's. Lean toward: Phase 1 uses a simple string
   map, Phase 2 re-keys on Java's canonical names.

5. **Backpressure + streaming.** Today result-set consumption is
   eager — `driver.Rows` iterates a cursor that holds a live FDB
   transaction. With Cascades producing plans that might include
   memory-hungry operators (hash aggregate, sort), we should decide
   whether the physical operators are fully streaming (row-at-a-time,
   survives the 5s FDB txn limit via continuations) or materialise
   intermediate results. Java streams. Phase 2 problem but worth
   noting before the physical operator interface is frozen.

## References

- Java: `fdb-relational-core/src/main/java/com/apple/foundationdb/relational/recordlayer/query/`
  — 43 files, the layout we are approximating.
- Java: `fdb-relational-core/src/main/java/com/apple/foundationdb/relational/recordlayer/AbstractEmbeddedStatement.java`
  — 40-line execute() method, the shape `embedded.Conn` should have.
- Java: `fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades/`
  — ~104K LOC of Cascades; canonical reference implementation.
- TODO.md line 627: existing unchecked TODO "Split connection.go into
  ~12 files. Mechanical, no behavioral risk."
- TODO.md line 629: existing unchecked TODO "Add a Planner / Plan seam
  before Phase 4. `execSelect*` walks the ANTLR parse tree directly;
  when Cascades lands, there's nowhere to plug in."
- TODO.md line 635: unchecked "Java↔Go SQL conformance harness" —
  will regress fastest if the refactor introduces subtle semantic
  drift, so the harness should land before or alongside Phase 1.
- `shifts/2026-04-22-swingshift-44.md`: the pushdown-chain work this
  RFC plans to subsume into Cascades rules.
