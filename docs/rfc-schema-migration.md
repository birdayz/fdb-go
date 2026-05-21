# RFC: Schema Migration for the Relational Layer

**Status:** Draft
**Author:** Johannes Bruederl
**Date:** 2026-05-20

## Problem

The FRL relational layer has no mechanism for evolving a live schema.
`CREATE SCHEMA TEMPLATE` is immutable once bound to a schema via
`CREATE SCHEMA`. There is no `ALTER TABLE`, `ALTER INDEX`, `ADD COLUMN`,
`DROP INDEX`, or any other DDL that modifies an existing schema in place.

The record layer underneath has all the necessary primitives:
`RecordMetaDataBuilder` evolves metadata, `OnlineIndexer` backfills
indexes, `FormerIndex` prevents subspace reuse, and the index state
machine (`Disabled -> WriteOnly -> Readable`) gates query visibility.
But none of this is exposed through SQL DDL.

Java's FRL has the same gap and shows no signs of closing it (zero GH
issues, zero git history, `ALTER` is a dead lexer token with no parser
rules). This RFC proposes a Go-only extension.

## Scope

In scope:
- `ALTER SCHEMA TEMPLATE` DDL: add column, add index, drop index
- Background index build orchestration via existing `OnlineIndexer`
- Safe catalog rebinding (template version upgrade without data loss)
- Index state visibility in `INFORMATION_SCHEMA`

Out of scope (future RFCs):
- Drop column (requires data migration or tombstone semantics)
- Column type changes (requires per-record rewrite)
- Table renames (requires index key rewrite)
- Drop table (requires cascading index/FK cleanup)
- Cross-template migration (schema A template X -> schema A template Y)

## Background: Existing Primitives

### Record Layer (all implemented in Go)

| Primitive | Location | Status |
|-----------|----------|--------|
| `RecordMetaDataBuilder.AddIndex()` | `metadata.go:344` | Working, bumps version |
| `RecordMetaDataBuilder.RemoveIndex()` | `metadata.go:426` | Working, creates FormerIndex |
| `MetaDataEvolutionValidator` | `metadata_evolution_validator.go` | Working, validates old->new |
| `FormerIndex` tracking | `metadata.go:68` | Working, prevents subspace reuse |
| `OnlineIndexer` (BY_RECORDS) | `online_indexer.go:206` | Working, tested |
| `OnlineIndexer` (BY_INDEX) | `online_indexer.go:458` | Working |
| `OnlineIndexer` (MULTI_TARGET) | `online_indexer.go:868` | Working |
| `OnlineIndexer` (MUTUAL/concurrent) | `online_indexer.go:579` | Working |
| Index state machine | `index_state.go:12` | Working: Disabled/WriteOnly/Readable/ReadableUniquePending |
| `MarkIndexWriteOnly()` | `index_state.go:211` | Working |
| `MarkIndexReadable()` | `index_state.go:122` | Working |
| `MarkIndexDisabled()` | `index_state.go:226` | Working, clears index data |

### Relational Layer

| Primitive | Location | Status |
|-----------|----------|--------|
| `SchemaTemplateCatalog.CreateTemplate()` | `fdb_template_catalog.go` | Working, versioned storage |
| `StoreCatalog.RepairSchema()` | `fdb_store_catalog.go:402` | **Working** — rebinds schema to latest template version |
| `StoreCatalog.SaveSchema()` | `fdb_store_catalog.go` | Working |
| `RelationalSchemaEvolutionValidator` | `schema_evolution_validator.go` | Working: no table drop, no column drop, no type change |
| ANTLR `ALTER` token | `RelationalLexer.g4` | Exists but unused |

Key discovery: **`RepairSchema` already does catalog rebinding.** It loads
the current schema, loads the latest template version, and re-saves the
schema bound to the new template. The underlying FDB record store is
untouched. This is the foundation for migration.

## Design

### Grammar Extension

```antlr
alterStatement
    : ALTER SCHEMA TEMPLATE schemaTemplateId alterClause+
    ;

alterClause
    : ADD tableColumn TO tableName
    | ADD indexDefinition
    | DROP INDEX indexName
    ;

// Reuse existing rules:
// tableColumn  -> columnName columnType (NOT NULL)?
// indexDefinition -> INDEX indexName ON tableName '(' indexColumns ')'
```

### DDL Flow

```
ALTER SCHEMA TEMPLATE tmpl
  ADD COLUMN email STRING TO users,
  ADD INDEX idx_email ON users (email),
  DROP INDEX idx_old_name;
```

1. **Parse** — ANTLR produces `AlterSchemaTemplateStatement`
2. **Load current template** — `catalog.LoadSchemaTemplate(txn, "tmpl")`
3. **Build new metadata** — clone `RecordMetaData` into a new
   `RecordMetaDataBuilder`, apply deltas:
   - `ADD COLUMN`: add field to proto descriptor (proto3 allows this
     without breaking wire format; existing records return zero-value)
   - `ADD INDEX`: `builder.AddIndex(table, index)`
   - `DROP INDEX`: `builder.RemoveIndex(indexName)` (creates FormerIndex)
4. **Validate evolution** — `MetaDataEvolutionValidator.Validate(old, new)`
   and `RelationalSchemaEvolutionValidator.Validate(old, new)`
5. **Save new template version** — `catalog.CreateTemplate(txn, newTemplate)`
6. **Rebind all schemas using this template** — for each schema bound to
   `tmpl`, call `RepairSchema(txn, dbURI, schemaName)` (already implemented)
7. **Commit catalog transaction**
8. **Schedule index jobs** — for each new index, enqueue a build job

### Index Build Orchestration

New indexes go through a three-phase lifecycle:

```
Phase 1: ALTER DDL (single FDB transaction)
  - New template version saved to catalog
  - Schema rebound to new template
  - New index state set to WRITE_ONLY
  - Build stamp written (IndexBuildIndexingStamp proto)
  → From this point, all new writes maintain the index

Phase 2: Backfill (multi-transaction background job)
  - OnlineIndexer.BuildIndex() scans existing records
  - Processes in batches (configurable limit, default 2000/tx)
  - Tracks progress via RangeSet (survives restarts)
  - Retries on conflict (built into OnlineIndexer)
  → Handles crash recovery: stamp + RangeSet = resumable

Phase 3: Finalize (single FDB transaction)
  - Verify: no remaining ranges unbuilt
  - For unique indexes: check no uniqueness violations
  - MarkIndexReadable() — queries can now use this index
  - Clear build stamp
  → Index is fully live
```

For `DROP INDEX`:
- Single transaction: `RemoveIndex()` + clear index subspace range
- No background job needed

For `ADD COLUMN`:
- Single transaction: new template version + rebind
- No background job needed (proto3 default values)

### Index Build Job Representation

```go
type IndexBuildJob struct {
    TemplateID   string
    IndexName    string
    Strategy     IndexBuildStrategy  // BY_RECORDS (default), BY_INDEX
    SourceIndex  string              // for BY_INDEX strategy
    Limit        int                 // records per transaction (default 2000)
    MaxRetries   int                 // per-batch retry limit (default 10)
}
```

Jobs are tracked by the `IndexBuildIndexingStamp` proto already persisted
at `{IndexBuildSpaceKey, index.SubspaceTupleKey(), 2}`. The stamp
prevents concurrent conflicting builds and encodes the strategy.

### Execution Model

**Option A: Synchronous (simpler, recommended for v1)**

`ALTER SCHEMA TEMPLATE ... ADD INDEX` blocks until the index is fully
built. For small tables (<100K records) this completes in seconds. For
large tables, the caller can set a statement timeout and the index
remains in `WRITE_ONLY` state — a subsequent `ALTER` or explicit
`BUILD INDEX idx_name` command resumes the build.

```sql
ALTER SCHEMA TEMPLATE tmpl ADD INDEX idx_email ON users (email);
-- blocks until backfill completes, then index is READABLE
```

**Option B: Asynchronous (recommended for v2)**

`ALTER` returns immediately after Phase 1. Index state is `WRITE_ONLY`.
A background goroutine (or external job) runs Phase 2 + 3. Callers
can monitor progress:

```sql
-- Check index state
SELECT index_name, index_state FROM INFORMATION_SCHEMA.INDEXES
  WHERE table_name = 'users';

-- Manually trigger build (if background job failed/not running)
BUILD INDEX idx_email;

-- Block until a WRITE_ONLY index becomes READABLE
AWAIT INDEX idx_email;
```

### Query Behavior During Migration

- **WRITE_ONLY indexes**: maintained on every write but NOT used for
  query planning. The Cascades planner already filters by index state
  in match candidate generation — `WRITE_ONLY` indexes produce no
  match candidates.
- **Dropped indexes**: immediately invisible to queries after the
  `ALTER` transaction commits. `FormerIndex` prevents the subspace
  key from being reused by a future index.
- **New columns**: visible in `SELECT *` and `INFORMATION_SCHEMA`
  immediately. Existing rows return zero-value (proto3 default).

### Error Handling

| Scenario | Behavior |
|----------|----------|
| `ADD COLUMN` that already exists | Error: column already exists |
| `ADD INDEX` that already exists | Error: index already exists |
| `DROP INDEX` that doesn't exist | Error: unknown index |
| `ADD COLUMN` + `DROP TABLE` in same ALTER | Error: DROP TABLE out of scope |
| Backfill crash mid-build | Resumable: RangeSet tracks progress |
| Unique index with violations | State becomes `READABLE_UNIQUE_PENDING` |
| Concurrent `ALTER` on same template | Second ALTER fails (FDB conflict) |
| Concurrent `BUILD INDEX` on same index | Stamp check prevents duplicate builds |

### Catalog Safety

The `MetaDataEvolutionValidator` enforces:
- Version must increase
- Cannot remove split-long-records once enabled
- `FormerIndex` subspace keys cannot be reused
- No unsafe field renames

The `RelationalSchemaEvolutionValidator` enforces:
- No table removal
- No column removal
- No column type changes
- No primary key reordering

Both validators run before the new template is persisted. Invalid
migrations fail atomically — no partial state.

## Implementation Plan

### Phase 1: Grammar + Catalog (estimated: 1 shift)

1. Add `alterStatement` / `alterClause` rules to `RelationalParser.g4`
2. Regenerate ANTLR Go code
3. Add `AlterSchemaTemplateConstantAction` implementing `ConstantAction`
4. Wire `execAlter()` dispatch in `EmbeddedConnection`
5. Implement `ADD COLUMN` (proto descriptor evolution + new template version)
6. Implement `DROP INDEX` (single-tx: RemoveIndex + clear subspace)
7. Tests: ALTER + verify INFORMATION_SCHEMA reflects changes

### Phase 2: Synchronous Index Build (estimated: 1 shift)

1. Implement `ADD INDEX` with synchronous `OnlineIndexer.BuildIndex()`
2. Wire index state transitions: WRITE_ONLY during build, READABLE on completion
3. Handle unique index violations (READABLE_UNIQUE_PENDING)
4. Tests: ALTER ADD INDEX + verify index is used by planner + verify
   data correctness for pre-existing rows

### Phase 3: Async + Monitoring (estimated: 1-2 shifts)

1. Background build goroutine with crash recovery
2. `BUILD INDEX` DDL for manual trigger
3. `INFORMATION_SCHEMA.INDEXES` shows `index_state` column
4. `AWAIT INDEX` DDL for blocking wait
5. Tests: crash recovery, concurrent writes during build, state
   transitions visible in INFORMATION_SCHEMA

## Risks

- **Proto descriptor evolution**: adding a field to an existing proto
  message descriptor at runtime (without regenerating code) requires
  dynamic proto manipulation (`dynamicpb`). The relational layer
  already uses `dynamicpb` for record construction — this is
  tractable but not trivial.

- **`RepairSchema` assumptions**: the current implementation assumes
  the underlying FDB record store is compatible with the new template.
  For index additions this is fine (new index starts empty). For
  column additions this is fine (proto3 defaults). For destructive
  changes it would corrupt data — but those are blocked by the
  evolution validators.

- **Multi-schema rebinding**: one template can back multiple schemas
  (different databases using the same template). `ALTER SCHEMA TEMPLATE`
  must rebind ALL schemas using that template, not just one. This
  requires a catalog scan — `ListSchemas` filtered by template name.

- **Concurrent writes during WRITE_ONLY**: the index maintainer must
  be active for the WRITE_ONLY state to work correctly. This means
  the store must load the new metadata (with the WRITE_ONLY index)
  before any writes happen. The metadata version stamp mechanism
  (`dirtyMetaDataVersionStamp`) handles cross-transaction cache
  invalidation, but there's a window between ALTER commit and other
  connections reloading metadata where writes might not maintain the
  new index. Mitigation: the OnlineIndexer's final scan covers any
  records written in this window.

## Alternatives Considered

**1. Application-level migration (status quo)**

Callers manage schema evolution via Go API: build new metadata, save
new template version, call RepairSchema, run OnlineIndexer manually.
This works but requires deep knowledge of the record layer internals
and cannot be expressed in SQL.

**2. Drop + recreate**

`DROP SCHEMA` + `CREATE SCHEMA` with new template. Loses all data.
Only viable for development/testing.

**3. Shadow table migration**

Create a new schema with the new template, copy all data, swap.
Requires 2x storage and a data copy phase. Overkill for additive
changes (which are the common case).

## Open Questions

1. Should `ALTER SCHEMA TEMPLATE` implicitly rebind all schemas, or
   require explicit `REPAIR SCHEMA` per schema? Implicit is more
   convenient; explicit is safer for multi-tenant deployments.

2. Should we support `ADD INDEX IF NOT EXISTS` / `DROP INDEX IF EXISTS`
   for idempotent migrations?

3. Should index build progress be exposed as a system table
   (`INFORMATION_SCHEMA.INDEX_BUILDS`) with estimated completion?
