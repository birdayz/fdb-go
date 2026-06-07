# RFC-084 — `INSERT … SELECT … GROUP BY` writes the wrong columns (spurious 23505)

**Status:** Draft — awaiting Graefe + Torvalds ACK.
**Found:** during RFC-083 (the AVG follow-up at TODO line 71). A real Java-parity
correctness bug, independent of AVG.

## Problem

```sql
INSERT INTO dst(id, s) SELECT g, SUM(v) FROM src GROUP BY g
```
fails on master with **SQLSTATE 23505** ("record already exists") and leaves `dst`
empty (the tx rolls back). The grouped rows have **distinct** primary keys
(`id = g ∈ {1,2}`), so there is no real conflict.

Java **supports** this exact shape — `insert_select_java.yaml:60`:
`INSERT INTO summary SELECT cat, SUM(val), COUNT(*) FROM source GROUP BY cat`
is accepted (rowcount 3). So Go diverges from Java on a documented, supported form.

## Investigation

INSERT…SELECT executes the inner SELECT, materializes its rows, and
`buildInsertRecord` (`executor.go`) builds each target record by looking up **each
target field name** in the row datum (`folded[lower(fieldName)]`).

For a non-GROUP-BY source, `alignInsertSelectColumns`
(`logical_predicate.go`) renames the `LogicalProject`'s output aliases to the
target column names positionally, so the datum is keyed by target names and the
name lookup succeeds.

A GROUP BY source is built by the **legacy** `buildLogicalPlanForSelectWithCatalog`
as a **bare `LogicalAggregate` with NO `LogicalProject`** on top (the modern
`visitSelectGroupBy` path *does* build a Project, but INSERT…SELECT uses the
legacy builder — an instance of the RFC-079 multi-builder drift). So:
- `alignInsertSelectColumns` calls `findProjection(Source)` → **nil** → does nothing.
- The aggregate cursor emits a datum keyed by its **output names**: group keys +
  aggregate names. Confirmed by instrumentation: `datumKeys = [G, SUM(V)]`.
- `buildInsertRecord` looks up `id` and `s` → **neither present** → both target
  fields left **unset** → every grouped row becomes the same all-default record
  (unset PK) → the second group collides with the first → **23505**.

Root cause: the bare-aggregate insert source's output columns are never aligned to
the target columns (Java maps source→target **positionally**; Go relies on a
name-match that only the Project path sets up).

## Fix (revised per Graefe + Torvalds)

A plain `SELECT g, SUM(v) GROUP BY g` (no post-agg expression, no HAVING-only
aggregate) builds a **bare `LogicalAggregate` with no Project** in *both* builders —
by design: standalone derives its result schema from the physical plan
(`deriveColumnsFromAggregation`), so no logical Project is needed. The INSERT path
inherits that bare aggregate and has nothing to align to the target columns.

**Reuse `buildPostAggregateProjection`** (`logical_builder.go:308`) — the canonical
helper already shared by `visitSelectGroupBy` and `buildSelectShell`. Given the
aggregate `op` + `sq.aggCols` + the `strip` func it emits the post-aggregate
`LogicalProject` with exactly the right columns: **visible-only** (skips
`!ac.visible`, so HAVING-only aggregates are excluded — closes the `keys==0`
`SELECT SUM(v) … HAVING COUNT(*)>1` hole that hand-rolling over
`aggregateOutputColumns` would mis-map), **canonical-named** (`FN(strip(arg))` /
stripped group col — matches the runtime aggregate datum key, NOT the alias), and
in **SELECT order** (so `SELECT SUM(v), g` maps correctly — no silent-wrong
reordering). Do **not** derive columns from `aggregateOutputColumns` (over-counts
on `keys==0`, returns alias-not-canonical, doesn't strip qualifiers).

In `buildLogicalPlanForInsertWithCatalog`, after the source is rebuilt, when
`findProjection(Source) == nil && findAggregate(Source) != nil`:
1. `proj, _ := buildPostAggregateProjection(Source, sq.aggCols, strip)` (a bare
   aggregate has no post-agg antlr exprs, so the second return is all-nil).
2. Fill `proj.ProjectedValues[i] = &FieldValue{Field: proj.Projections[i]}`
   (canonical names) and `proj.Input = Source`.
3. `insertOp.Source = proj`.
4. Let the **existing** `alignInsertSelectColumns` set `proj.Aliases[i]` to the
   target column names positionally (do NOT alias in two places).

Then the executor path is unchanged: the projection cursor reads `datum["SUM(V)"]`
(canonical) and stores the value under both canonical and the target alias, so
`buildInsertRecord` finds every target field.

**Why canonical names resolve:** `FieldValue.Evaluate` does a *direct verbatim map
lookup* (`row[f.Field]`, `values.go`) — no qualifier `.`-split, no case-fold — so
`FieldValue{Field:"SUM(V)"}` reads `datum["SUM(V)"]` exactly. This is *why*
canonical (upper-cased, qualifier-stripped) names are mandatory: an alias or a
qualified name would miss and silently re-introduce the unset-PK 23505.

Arity mismatch (source cols ≠ target cols) is caught by the existing INSERT arity
check.

**Two shapes need extra handling (found in impl review):**
- **`SELECT COUNT(*)`** is tracked as `sq.countStar` with an EMPTY `aggCols` (the
  parser sets the flag only when COUNT(*) is the sole SELECT element), so
  `buildPostAggregateProjection` returns nil and the wrap would no-op — leaving
  `INSERT INTO t SELECT COUNT(*) FROM src` silently inserting 0 / a 23505 under
  GROUP BY. Fix: synthesize a `COUNT(*)` aggCol before the helper when
  `sq.countStar`.
- **Qualified aggregate operand** (`SUM(s.v)`): on this insert-source path the
  qualified aggregate's operand is left unresolved (a SEPARATE pre-existing defect)
  so the aggregate computes NULL — wrapping would align the (NULL) column and
  *silently* insert NULL. The wrap therefore **skips** a source with any qualified
  aggregate/group-key name (a `.` in the canonical name), leaving the original LOUD
  23505. Tracked as a follow-up (qualified-operand resolution on the insert path).

This is localized to the insert-planning path, mirrors how the Project-source case
already aligns, and matches Java's positional source→target mapping. It does **not**
require the full RFC-079 builder unification (tracked separately) — but the comment
will point at it as the end-state (one builder that always produces the Project).

### Why not change the executor to positional mapping directly?

Positional source→target mapping in `buildInsertRecord` would be the cleaner
Java-faithful end-state and would subsume `alignInsertSelectColumns`. But it
touches every INSERT…SELECT path (large regression surface) and is really the
RFC-079 unification's job. This RFC fixes the divergence with minimal blast radius;
the positional/unification cleanup stays a tracked follow-up.

## Performance

Plan-time only: adds one `LogicalProject` over the aggregate on the INSERT…SELECT
path (a thin rename, no extra scan/sort). Zero runtime/wire impact; no change to
non-GROUP-BY INSERT or to standalone SELECT. plandiff for non-INSERT queries
byte-identical.

## Test plan

FDB integration (`*_fdb_test.go`), the load-bearing pins:
- `INSERT INTO dst(id,s) SELECT g, SUM(v) FROM src GROUP BY g` → succeeds, dst =
  `{(1,30),(2,30)}` (was 23505) — the Java-supported shape.
- Multi-aggregate: `INSERT INTO summary SELECT cat, SUM(val), COUNT(*) FROM source
  GROUP BY cat` (mirrors `insert_select_java.yaml:60`) → 3 rows, correct values.
- **Lowercase arg** (`SUM(v)`, Graefe/Torvalds): proves the canonical upper-casing
  agrees end-to-end (the `FieldValue` key matches the runtime datum key) — else
  nil → unset → the 23505 silently returns.
- **No-GROUP-BY HAVING** (`INSERT INTO d(s) SELECT SUM(v) FROM src HAVING COUNT(*)>1`,
  the `keys==0` hole): the HAVING-only `COUNT(*)` is excluded; only `SUM(v)` maps.
- **Qualified source alias** (`FROM src s … SUM(s.v)`, Torvalds): the group/agg key
  is stripped to match the runtime datum key.
- **Reordered SELECT** (`SELECT SUM(v), g`): maps correctly (SELECT order preserved
  by `buildPostAggregateProjection`) — NOT silently transposed.
- GROUP BY + HAVING over a non-visible aggregate (`… GROUP BY g HAVING COUNT(*)>1`)
  → goes through the existing strip-Project path; correct rows.
- Determinism (10×); non-GROUP-BY INSERT…SELECT regression (still works);
  standalone `SELECT g, SUM(v) GROUP BY g` unaffected; plandiff for non-INSERT
  byte-identical.
- Full `just test` green.

## Out of scope (follow-ups)

- **RFC-079 builder unification** (route INSERT…SELECT through `visitSelectGroupBy`)
  is the one-query-path end-state. **Commitment (Graefe condition):** that work
  *moves* the coercion into the Insert expression / makes this post-aggregate
  Project automatic and **deletes this insert-path wrap** — it must not leave a
  third parallel coercion path. Tracked in TODO.md.
