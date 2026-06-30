# RFC-159: DML nonexistent WHERE-column / table → clean 42703 / 42F01 (item 1066)

**Status:** Implemented
**Gate:** Torvalds + codex + @claude (+ Graefe glance — DML SQL→logical translation; no Cascades
planner/optimizer/cost change)

## Problem

A `DELETE`/`UPDATE` whose WHERE references a nonexistent column, or whose target table does not
exist, fails with a generic `0AF00: DML Cascades translation failed` — whereas the SELECT and INSERT
paths already give the specific, clean SQLSTATE:

- `SELECT … WHERE nope = 1` → `42703` (undefined column); `DELETE/UPDATE … WHERE nope = 1` → `0AF00`.
- `INSERT INTO notable …` → `42F01` (undefined table); `DELETE/UPDATE FROM notable …` → `0AF00`.

A `database/sql` caller doing SQLSTATE extraction can't tell a typo'd column/table from any other DML
failure. The SET-column case was already fixed to `42703` (`update_undefined_column_probe`); this is
its WHERE-column + table sibling. **Verified real** (not stale): a red probe showed all four cases
returning `0AF00`.

## Investigation

- The clean codes already exist: `mapPredicateWalkError` (`logical_predicate.go:394`) classifies a
  bare `semantic.ColumnNotFoundError` → `42703` (and ambiguous/source errors); `42F01` is
  `ErrCodeUndefinedTable`, used by INSERT at `cascades_generator.go:814`.
- **Column case** — the DML WHERE resolver swallowed it: `upgradeDMLWhereWithCatalog` only surfaced a
  carried error for EXISTS shapes, then fell back to `buildWherePredicateForTableE`, which surfaced
  only an *already*-`*api.Error` (line 131) — a bare `ColumnNotFoundError` is not one, so it
  soft-fell-back (nil error). The DML builder then kept the text filter referencing the missing
  column → `TranslateToCascadesWithSubqueries` returned nil → generic `0AF00`
  (`cascades_generator.go:851`).
- **Table case** — no DML target-table existence check (INSERT has one; DELETE/UPDATE don't), and
  `buildWherePredicateForTableE`'s `ResolveTable` failure is swallowed. With no WHERE the resolver
  isn't even reached, so the bad table flows to `0AF00`.

## Fix

1. **Column** — `buildWherePredicateForTableE` classifies the WHERE walk error via the shared
   `mapPredicateWalkError` (the same the SELECT WHERE / JOIN-ON paths use), so a bare
   `ColumnNotFoundError`/ambiguous/source failure surfaces `42703`/`42702`. An unclassifiable shape
   failure still soft-falls-back (mapper returns nil), preserving existing behaviour.
2. **Table** — an explicit target-table existence check (`md.GetRecordType(bare) == nil` →
   `ErrCodeUndefinedTable`, mirroring INSERT) in `buildLogicalPlanForDeleteWithCatalog` and
   `buildLogicalPlanForUpdateWithCatalog`, run independent of WHERE so `DELETE FROM notable` (no
   WHERE) is caught too. In UPDATE it precedes the existing SET-column check, whose `rt != nil` guard
   would otherwise silently skip on a missing table.

Conformance: matches the SELECT/INSERT codes the engine already produces (Java's relational engine
rejects the same inputs with undefined-column/table errors); strictly an error-classification
improvement, no plan-shape/wire change.

## Performance

None — only the error path changes; the success path is unchanged (`GetRecordType` is one map lookup
the builder already does for UPDATE).

## Test plan

- `dml_where_undefined_probe_test.go` — 6 subtests: DELETE/UPDATE WHERE undefined column → 42703;
  DELETE/UPDATE nonexistent table → 42F01; valid-WHERE DELETE/UPDATE controls still execute.
- Revert-proven: before the fix all four error cases returned `0AF00`.
- Broad DML regression batch (DMLCascades, Update*, Delete*, DmlDryRun, InsertSelect, subquery-WHERE)
  green; full `just test` green.

## Note — RETURNING (item 973) deferred

`DELETE/UPDATE … RETURNING` (the original PR3 candidate) needs a Cascades `Project`-over-DML
integration (Java wraps a `generateSelect` over the mutation operator) — a Graefe-gated planner
change, out of scope for this error-classification fix. Tracked in TODO.md.
