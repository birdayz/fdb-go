# RFC-161: Propagate in-template DDL errors' specific SQLSTATE instead of wrapping to 42F59 (item 990)

**Status:** Implemented
**Gate:** Torvalds + codex + @claude (DDL error classification; no query-engine/executor change → no Graefe)

## Problem

Every error raised while building a table or index inside a `CREATE SCHEMA TEMPLATE` was re-wrapped
to the outer SQLSTATE **42F59** (`ErrCodeInvalidSchemaTemplate`), with the real cause embedded in the
message — e.g. `42F59: index: 0A000: index "T_A": INCLUDE clause (covering index) is not yet
supported`, or `42F59: table "T": 42701: duplicate column …`. A `database/sql` caller extracting the
SQLSTATE got `42F59`, never the specific code. And 42F59 is the *wrong* code: a duplicate column is
not an "invalid schema template" — it is a duplicate column (42701).

## Investigation (Java is the spec)

Java does **not** blanket-wrap in-template errors. Its `DdlVisitor.visitCreateSchemaTemplate`
(`fdb-relational-core`) builds the metadata directly; an invalid column/index throws its own
exception, and `ExceptionUtil.recordCoreToRelationalException` maps each exception *type* to its
specific `ErrorCode` (MetaData → SYNTAX_OR_ACCESS_VIOLATION, Semantic → the semantic code, …). There
is no `INVALID_SCHEMA_TEMPLATE` wrap of in-template object errors. So Go's blanket 42F59 wrap is a
real divergence — Go buries a code Java surfaces.

## Fix

`createSchemaTemplate` (`ddl.go`): when `parseTableDefinition` / `parseIndexDefinition` returns a
**structured `*api.Error`** (it already carries the right SQLSTATE — 42701 duplicate column, 42703 PK
over unknown column, 0A000 unsupported INCLUDE, …), **return it directly** instead of wrapping it in
42F59. A non-structured parse error still wraps (it has no SQLSTATE to surface). Two sites (table +
index), 8 lines total.

Per-code conformance note: the *approach* now matches Java (propagate the specific code, don't wrap).
The exact code may still drift from Java's per-type `ExceptionUtil` mapping (e.g. Go reports the
SQL-standard `42701` for a duplicate column where Java reports the generic
`SYNTAX_OR_ACCESS_VIOLATION`) — but that is acceptable DDL-error wording/code drift (the cross-engine
corpus already classifies DDL errors as `DivergenceBothErrorMessagesDrift`), and the specific code is
strictly more correct than the false 42F59 wrapper. The separate "implement covering indexes
(`INCLUDE`)" item (1004) is what would make INCLUDE *succeed* like Java — out of scope here.

The duplicate-template-**name** rejection keeps its 42F59 (`ErrCodeInvalidSchemaTemplate`) — that is a
genuinely-invalid-template error on a different path, not an in-template object error, and unaffected.

## Test plan

- `include_clause_rejected_probe_test.go`: `… INDEX … INCLUDE` now reports SQLSTATE **0A000** (the
  propagated cause), and the message no longer carries the generic 42F59 wrapper.
- `ddl_errors_probe_test.go`: duplicate column → 42701, PK over unknown column → 42703 are now the
  **outer** SQLSTATE (were embedded under 42F59); duplicate-template-name → 42F59 unchanged.
- Full `just test` green.
