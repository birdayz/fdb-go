# RFC-204 — Struct types in the relational layer

Status: **ACCEPTED**, revision 2 — dual joint-review ACK; implementation may
start. Revision 1 received a Graefe ACK with two conditions (the §4.3 CQ-72
merge-order contract and the §4.2 nullable array-of-struct byte-golden) and a
Torvalds ACK conditional on text corrections (an over-claimed index census
row, an over-counted array row, three stale citations) — all folded here and
each re-verified against the sources before folding. RFC-201 Phase 3
prerequisite; query-engine review gate applies (RFC-201 §"Engine-gap phases
that touch the query engine (struct types above all) carry the standing
Graefe RFC + review gate individually", rfcs/201-layered-test-corpus.md:392);
the implementation lap at each phase completion remains owed.

Java reference: `fdb-record-layer/` at tag 4.12.11.0. All Java citations are
relative to that tree; all Go citations to the repo root.

## 0. Summary

`CREATE TYPE AS STRUCT` and struct-typed columns are the largest measured
engine gap in the vendored Java corpus after value-index-AS-SELECT: **39 of
238 files** are booked `unsupported-DDL:struct` in the pinned ledger
(`pkg/relational/conformance/javacorpus/pinned_ledger_test.go:45`), and CQ-73
(TODO.md:13053-13060) plus the RFC-201 Phase 3 line (rfcs/201:163-165) both
point here. This RFC specifies the full SQL-surface port: DDL, type registry,
proto/wire mapping, DML literals, nested field access, result metadata, and
index expressions over nested fields — phased into five independently
landable units, each with per-phase corpus-ledger movement as the acceptance
criterion.

The record-layer core is not the gap: nested messages, `values.RecordType`
(`pkg/recordlayer/query/plan/cascades/values/type.go:337`), multi-accessor
`FieldPath` (`values/values.go:233-260`), and nesting key expressions all
exist. What is missing is exactly the relational seams enumerated in §4.

## 1. Demand, measured

### 1.1 The 39 files

Measured, not inferred: the per-file `path status class` assignment was dumped
from the pinned-ledger test (temporarily blanking `pinnedAssignmentDigest`,
running `//pkg/relational/conformance/javacorpus:javacorpus_test`, reverting).
The `unsupported-DDL:struct` class holds exactly these 39 files:

```
aggregate-index-tests, array-join-at, arrays-cardinality,
arrays-unnesting-documentation-queries, arrays-unnesting, arrays,
check-result-metadata/shouldFail/{wrong-array-of-struct-field-name,
  wrong-array-of-struct-field-type, wrong-nested-struct-name,
  wrong-nested-struct-type, wrong-struct-field-name, wrong-struct-field-type,
  wrong-struct-type-name},
check-result-metadata/shouldPass/{array-of-struct-column, field-named-array,
  nested-struct-column, struct-type-name, type-named-array},
create-drop-create-template, documentation-queries/subqueries-…,
documentation-queries/vector-…, functions, groupby-tests, in-predicate,
inserts-updates-deletes, nested-tests, nested-with-nulls, orderby,
primary-key-tests, select-a-star, showcasing-tests,
star-expression-metadata, struct-type-nullability-variants, subquery-tests,
user-defined-macro-function-tests, uuid-non-prepared, uuid-prepared,
valid-identifiers, vector
```

(All `.yamsql`, under
`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/`.)

Two files grep-match `create type as struct` but are NOT in the class:
`case-when.yamsql` (passes — its struct template sits in a version variant not
selected at 4.12.11.0) and `update-delete-returning.yamsql` (its selected
variant has no struct; the file dies later on
`engine-gap:returning-dry-run`). `showcasing-tests` and
`create-drop-create-template` reach the same gap through a `setup:`-step
`create schema template` and are booked into the class via
`pkg/relational/conformance/javacorpus/gaps.go:101-102` (CQ-73).

### 1.2 Construct census over the 39 (grep-measured)

| Construct | Files | Representative shape |
|---|---|---|
| `CREATE TYPE AS STRUCT` declaration | 39/39 | `CREATE TYPE AS STRUCT s1(a bigint, b bigint)` |
| Nested field access (`a.b.c`, up to 7 segments) | 38/39 | `q.est.dst.cst.bst.ast.a` (orderby.yamsql:41) |
| ARRAY column (`name ARRAY`; array-of-struct is the wire-relevant subset) | 22/39 | `pts point array` (…/array-of-struct-column.yamsql) — count re-measured over DDL lines excluding comments; field-named-array.yamsql has NO array column (its `"array"` is a struct FIELD name) and functions.yamsql's is a primitive `integer array` |
| `resultMetadata:` descent into struct/array | 17/39 | `PT: [point, {X: BIGINT}, {Y: BIGINT}]` |
| `CREATE INDEX … AS SELECT` over nested fields | 16/39 | `create index i7 as select q.est.….a from t4 order by …` — every index over a NESTED path is AS-SELECT form; the one classic `ON` index in the 39 (`CREATE INDEX "T2_view_index" ON "T2_view" ("item")`, arrays-unnesting.yamsql:44) is a flat single column over a `CREATE VIEW` (:43), which the classic form's grammar is limited to (bare `uid`, §2.1) |
| Struct literal in INSERT (nested parens) | ≥9/39 | `VALUES (1, 2, ((3, 4), (5, 6)))`, nested `null` |
| Struct star expansion (`structcol.*` / qualified `.*`) | 7/39 | select-a-star, star-expression-metadata |
| ORDER BY nested field | 5/39 | orderby, in-predicate, groupby-tests |
| GROUP BY nested field | 2/39 | `create index i2 as select r.v.z from nested order by r.v.z` |
| WHERE on nested field | 2/39 | `where a.a.b is not null` (nested-with-nulls.yamsql:29) |
| Primary key over nested struct fields | 1/39 | `create table t1(id s1, g bigint, primary key(id.a, id.b))` (primary-key-tests.yamsql:23) |
| Named struct literal `STRUCT name(…)` in queries | ≥4/39 | `select struct "x$$" ("foo.tableA".*) from "foo.tableA"` (valid-identifiers.yamsql:457-467) |
| Nullability variants of one struct name | 1/39 | struct-type-nullability-variants |
| Struct containing VECTOR / UUID fields | 4/39 | vector, uuid-{non-}prepared |

Also present and load-bearing: struct-of-struct declarations (nested-tests
declares `s3(e s1, f s2)`), `NULL`-able nested structs (`b s1 NULL` inside a
struct declaration, nested-with-nulls.yamsql:25-26), UPDATE SET with a struct
literal (`update B set b2 = 30, b3 = (100, 100) where b1 < 10;`,
inserts-updates-deletes.yamsql:68; :58 is the INSERT-SELECT),
INSERT-SELECT of struct columns, unnest-defined indexes over struct arrays
(`create index ir_f as select sq.f from r, (select f from r.nr) sq`,
subquery-tests.yamsql:29).

### 1.3 Residual blockers (what the 39 hit AFTER struct lands)

Grep-measured per file; the hard (file-killing) residuals:

- **16 files** carry AS-SELECT indexes → blocked on RFC-202 (PROPOSED, rev 2)
  until its generator lands; they re-book `unsupported-DDL:value-index-as-select`.
- **12 files** (check-result-metadata struct set) assert nested
  `resultMetadata:` — they execute but their defining assertion is the
  CQ-74 metadata surface (Phase 4 here).
- **4 files** are `statement_type: prepared`-heavy (RFC-201 Phase 5) and one
  (`showcasing-tests`) uses `!r` random injection.
- Array-literal INSERT (`engine-gap:array-literal-values`, CQ-72, 6 files
  today) shares the exact converter seam this RFC's Phase 2 rebuilds; the
  class closes as a side effect (§6 Phase 2, merge-order contract in §4.3).
- `arrays-unnesting.yamsql` additionally declares a `CREATE VIEW` (:43) with
  a classic `ON` index over it (:44) — views are a §8 non-goal, so the file
  carries a view residual that outlives every phase of this RFC and must
  stay visible in the accounting rather than read as a struct straggler.

This interplay is already on record from the other side: RFC-202 §1.2 notes
that struct columns reject the whole template before any index is examined,
so the two ledgers mask each other; landing either RFC re-measures both
classes (rfcs/202-value-index-as-select.md:182-186).

## 2. Java is the spec — the complete mechanism

### 2.1 Grammar

- `STRUCT` keyword: `RelationalLexer.g4:707`.
- `structDefinition : (TYPE AS STRUCT) uid '(' columnDefinition (',' columnDefinition)* ')'`
  — `fdb-relational-core/src/main/antlr/RelationalParser.g4:121-123`. Struct
  fields reuse `columnDefinition`; no primary-key part.
- `columnDefinition : colName=uid columnType ARRAY? columnConstraint?` (:129-131);
  `columnType : primitiveType | customType=uid` (:138-139). A struct column is
  a bare identifier; array-of-struct is `name ARRAY`. `STRUCT` is NOT a
  primitive — there are no inline/anonymous struct columns.
- Literals: `recordConstructorForInsert` (:945-947), `recordConstructor` with
  `ofTypeClause? …` (:953-955), `ofTypeClause : STRUCT uid` (:957-959),
  `arrayConstructor : '[' expressions? ']'` (:961-963).
- Dotted access is just `fullId` (:747) — the grammar does not split table
  qualifier from struct path; resolution does.
- Legacy `indexColumnSpec : columnName=uid orderClause?` (:182-184) is a bare
  `uid` — the classic `ON` form CANNOT express nested paths; only AS-SELECT can.

Go already has the identical grammar
(`pkg/relational/core/parser/grammar/RelationalParser.g4:120-137, 939-957`);
the generated parser accepts every construct above today. The gap is entirely
downstream of the parse tree.

### 2.2 DDL semantics

- `DdlVisitor.visitStructDefinition`
  (`fdb-relational-core/...(query/visitors)/DdlVisitor.java:194-201`): a struct
  is built **as a `RecordLayerTable` without a primary key**; only its
  `getDatatype()` (a `DataType.StructType`) is kept.
- `visitColumnDefinition` (DdlVisitor.java:153-173): `isRepeated = ARRAY()`,
  nullable defaults true, and **`NOT NULL` is rejected except on ARRAY**
  (`Assert.thatUnchecked(isRepeated || isNullable, …, "NOT NULL is only
  allowed for ARRAY column type")`, :161). Custom types go through
  `SemanticAnalyzer.ParsedTypeInfo.ofCustomType` → `lookupType`.
- **Forward references**: `SemanticAnalyzer.lookupType`
  (SemanticAnalyzer.java:672-742) returns `DataType.UnresolvedType.of(name,
  nullable)` when the name is not yet known (:677-681).
- **Registration + resolution**:
  `RecordLayerSchemaTemplate` keeps `auxiliaryTypes` (a `LinkedHashMap`,
  RecordLayerSchemaTemplate.java:409), `addAuxiliaryType` guards name
  collisions against tables/aux types/routines/views (:552-556, :713-718),
  `findType` looks tables FIRST then aux types (:592-603) — a table name is
  itself usable as a column type. `build()` (:605-640) triggers
  `resolveTypes()` (:642-711): dependency graph via `getDependencies`
  (:719-751), **topological sort**, and `"Invalid cyclic dependency in the
  schema definition"` on a cycle (:663-664) — recursive structs are rejected.
  Unknown names die with `ErrorCode.UNKNOWN_TYPE, "could not find type '%s'"`.
- Array-of-struct: `if (isRepeated) return DataType.ArrayType.from(
  type.withNullable(false), isNullable)` (SemanticAnalyzer.java:738-742) —
  **the element type is always non-nullable; the array carries nullability.**
- Case sensitivity by normalization (`SemanticAnalyzer.normalizeString`,
  :143-154): unquoted identifiers upper-case, quoted verbatim; struct-name
  lookup is exact-match on the normalized string.
- Nested primary keys: `RecordLayerTable.Builder.toKeyExpression`
  (RecordLayerTable.java:313-329) walks the dotted path and emits
  `Key.Expressions.field(storageName).nest(child)`, with
  `getFieldStorageName` falling back to `ProtoUtils.toProtoBufCompliantName`
  (:336-339).

### 2.3 Proto mapping — THIS IS WIRE

Chain: `DataType.StructType` → `Type.Record`
(`DataTypeUtils.toRecordLayerType`, DataTypeUtils.java:112-115, via
`Type.Record.fromFieldsWithName`, Type.java:2537-2539) → `DescriptorProto`
(`Type.Record.defineProtoType`, Type.java:2339-2356).

- **Message name** = `ProtoUtils.toProtoBufCompliantName(userName)`
  (ProtoUtils.java:51-66): validates `^[A-Za-z_][A-Za-z0-9_]*$`, escapes
  `__`→`__0`, `$`→`__1`, `.`→`__2`; reversible via `toUserIdentifier` (:79-81).
- **Every struct field is `LABEL_OPTIONAL`** (Type.java:2352) unless the field
  type is an array.
- **Placement**: the relational serializer
  (`FileDescriptorSerializer.registerTypeDescriptors`,
  fdb-relational-core/.../serde/FileDescriptorSerializer.java:117-140) copies
  every message descriptor produced by `defineProtoType` into the file as a
  **top-level message**, de-duplicated by name. Struct types referenced by
  multiple tables share one top-level descriptor.
- Field numbers: `field.getFieldIndex()` (declaration index + 1; normalized
  in `Type.Record.normalizeFields`).
- **Known Java limitation to reproduce, not fix**: nullable and non-nullable
  uses of one named struct collapse to one descriptor — the TODO block at
  RecordLayerTable.java:156-174. Wire compat means Go emits the same
  collapsed descriptor. (`struct-type-nullability-variants.yamsql` pins the
  observable behaviour.)
- **Serialization asymmetry to reproduce**: a `CREATE TYPE AS STRUCT` no
  table references never reaches the file descriptor (only reachable
  transitively from tables), and on deserialize
  (`RecordMetadataDeserializer.deserializeRecordMetaData`,
  serde/RecordMetadataDeserializer.java:77-129) struct types do NOT come back
  as named auxiliary types — only union-member messages (tables) and enums do.

The Go catalog persists exactly Java's bytes:
`pkg/relational/core/catalog/fdb_template_catalog.go:249` — "Wire-compatible
with Java: RecordMetaData.toProto().toByteArray()". The descriptor is inside
that proto, so **descriptor shape is under the wire-compat hard line**: a
Java client must open a Go-created template and vice versa.

### 2.4 Query surface — nested field access

- `ExpressionVisitor.visitFullColumnName` → `SemanticAnalyzer.resolveIdentifier`
  (ExpressionVisitor.java:184-187; SemanticAnalyzer.java:371-379).
- `lookup` (SemanticAnalyzer.java:439-503): per output attribute — exact
  match, unqualified match, qualifier-prefixed match, then
  **`lookupNestedField`** (:549-601): the remaining path segments after the
  matched prefix each become a `FieldValue.Accessor(name, ordinal)` by
  scanning the `StructType`'s fields (name-equality on normalized
  identifiers, :581-593); the chain resolves through
  `FieldValue.resolveFieldPath` and fuses via
  `FieldValue.ofFieldsAndFuseIfPossible` (:597-598).
- `structcol.*`: `expandStar` asserts `Code.STRUCT`
  ("attempt to expand non-struct column", SemanticAnalyzer.java:355-367) and
  `expandStructExpression` (:746-762) emits `FieldValue.ofOrdinalNumber` per
  field.
- Named struct literal: `ExpressionVisitor.visitRecordConstructor`
  (:889-926); the `ofTypeClause` branch (:919-923) uses
  `RecordConstructorValue.ofColumnsAndName(columns, name)` →
  `Type.Record.withName` recomputes the storage name (Type.java:2219-2222).
- **Whole-struct comparison is unsupported in Java**:
  `RelOpValue.isSupportedOperandType` (fdb-record-layer-core/.../values/
  RelOpValue.java:320-322) admits primitive/enum/uuid/array/none — never
  `Type.Record`; violations raise
  `COMPARAND_TO_COMPARISON_IS_OF_COMPLEX_TYPE` (:333-350). vector.yamsql:578
  keeps `where embedding is null` commented out for exactly this reason.
  Conformance principle: Go rejects identically. (`ORDER BY structcol`,
  `GROUP BY structcol` fail the same way; only leaf access is comparable.)

### 2.5 DML

- INSERT pushes the target table's `Type.Record` as plan-fragment state
  (`QueryVisitor.visitInsertStatement`, QueryVisitor.java:751-776); each row
  is a `recordConstructorForInsert` under
  `parseRecordFieldsUnderReorderings` (ExpressionVisitor.java:1038-1090).
- **`parseRecordField` (ExpressionVisitor.java:967-1008) is the recursion
  point**: it pushes the target field's type before visiting a nested
  `(2, 3)`, coerces (`coerceIfNecessary`, :1011-1036), and adopts the target
  field's name if the literal is unnamed. A bare positional tuple acquires
  the struct's names and types entirely by target-type push-down.
- UPDATE: transform map keyed by `FieldValue.FieldPath`
  (QueryVisitor.java:827-833) — `set b3 = (100, 100)` and nested-path SETs.
- Prepared struct params (JDBC `RelationalStructBuilder`,
  `MutablePlanGenerationContext.processPreparedStatementStructParameter`,
  MutablePlanGenerationContext.java:429-452) are RFC-201 Phase 5 surface, out
  of scope here; noted so the seam is not designed shut.

### 2.6 Metadata surface

- `StructMetaData` (fdb-relational-api/.../StructMetaData.java:36-102):
  `getTypeName`, recursive `getStructMetaData(int)`, `getArrayMetaData(int)`,
  `getRelationalDataType()`.
- Implementation `RelationalStructMetaData` is backed directly by a
  `DataType.StructType` (RelationalStructMetaData.java:46-59); `getTypeName`
  returns the declared struct name (:62-65).
- The corpus consumer: `CheckResultMetadataConfig.extractDescriptors`
  (yaml-tests/.../CheckResultMetadataConfig.java:91-118) — `Types.STRUCT` →
  recurse with the type name; array-of-struct renders `"ARRAY(STRUCT)"`.

### 2.7 Index expressions over nested fields

`MaterializedViewIndexGenerator.toKeyExpression`
(fdb-relational-core/.../query/ddl/MaterializedViewIndexGenerator.java:778-789)
converts the resolved `FieldValue.ResolvedAccessor` chain into
`field(storageName).nest(child)`; `toFieldKeyExpression` (:810-827) uses the
**storage** field name (comment at :825), maps arrays to
`FanType.FanOut`/`Concatenate`, and rejects an index directly on an array
field without unnesting (:813-819). This is RFC-202's machinery; the struct
delta is that the accessor chains are longer than one.

## 3. Go today — the five seams

(Verified against this tree; the full audit is in §2 of the research notes
each phase cites.)

1. **DDL executor ignores struct clauses.** `execCreateSchemaTemplate`
   (`pkg/relational/core/embedded/ddl.go:120-203`) makes two passes — tables
   (:148-171) and indexes (:173-192); `structDefinition` clauses are silently
   skipped, and the struct-typed column then dies in `parseColumnType`
   (ddl.go:862-868): `pt == nil` → `"only primitive column types are
   supported"` (0A000). That one string is the entire 39-file class.
2. **Type registry**: `api.StructType`/`StructField` exist
   (`pkg/relational/api/datatype_composite.go:224-336`, including
   `Resolve`/`IsResolved` — the `UnresolvedType` machinery has a Go
   counterpart) but nothing constructs them from SQL.
3. **Descriptor emission exists but is latent AND wrong vs Java.**
   `buildMessageDescriptor`/`structDescriptor`
   (`pkg/relational/core/metadata/builder.go:543-636`) emit a struct as a
   **nested** type named `.Table.Struct` (builder.go:687), use
   `LABEL_REQUIRED` for non-nullable fields (`datatypeToLabel`,
   builder.go:696), and do no storage-name escaping. Java: top-level
   placement, always `LABEL_OPTIONAL` for struct fields,
   `toProtoBufCompliantName`. Because the catalog persists the descriptor
   (fdb_template_catalog.go:249) this is a wire divergence the moment struct
   DDL becomes reachable — Phase 1 reworks it before making it reachable.
4. **DML conversion**: `ConvertToProtoValue`
   (`pkg/relational/core/functions/proto_value.go:133-263`) has a
   `MessageKind` arm for UUID only (:239-256) and never consults
   `fd.IsList()`; `insert_cascades.go:155-175` types every cell
   `values.UnknownType`. The read direction (`ProtoValueToDriver`,
   :119-127) leaks a raw `protoreflect.Message`.
5. **Name resolution is flat.** `parseColRef`
   (`pkg/relational/core/embedded/colref.go:10-22`) splits on the LAST dot,
   so `t.h.e.a` becomes qualifier `"t.h.e"` + column `"a"` and dies in
   `ResolveQualifiedColumn`
   (`pkg/relational/core/query/semantic/scope.go:208-270`), which matches
   qualifiers against source aliases only. SQL-side `FieldValue`
   construction is single-accessor
   (`pkg/relational/core/query/cascades_translator.go:3598, :4820`).
   The cascades side already has everything: `FieldPath`
   (`values/values.go:233-260`), `AccessorNamePath`
   (`values/accessor_name_path.go:34`), fusion (`values/replace.go:412`).
6. **Result metadata truncates to one string.** `executor.ColumnDef`
   (`pkg/recordlayer/query/executor/resultset.go:62-69`) carries `TypeName
   string`; a struct column arrives as `"STRUCT"` with no fields
   (`valueTypeName`, `pkg/relational/core/embedded/cascades_generator.go:4319-4325`)
   and a struct-array element's `values.Type` is Unknown because the type
   system does not model message element types — the standing landmine
   documented at cascades_generator.go:3680-3700 and :3752-3771. This is
   CQ-74 (TODO.md:13062-13141) and the `unsupported:result-metadata-nested`
   ledger class (ledger.go:81-95).

(Brief correction: the task brief referenced a "STRUCT landmine in the
agreement-gate comment in cascades_generator.go"; no comment by that name
exists. The real landmines are the two blocks cited above, plus
`protoKindToTypeName`'s `MessageKind` fall-through to `UNKNOWN`,
cascades_generator.go:4432.)

## 4. Design

One principle: **1:1 with Java at every seam, because every seam is either
wire (descriptor, record bytes, key expressions) or shared query surface
(conformance principle).** No Go-only extensions are introduced; the one
place Go's latent code already diverges (nested descriptor placement) is
reworked to Java's shape before it becomes reachable.

### 4.1 DDL + type registry (mirrors DdlVisitor + RecordLayerSchemaTemplate)

- `execCreateSchemaTemplate` grows a struct pass: collect
  `StructDefinitionContext` clauses, build each as a table-without-PK through
  the same column parser (Java models structs as `RecordLayerTable`s,
  DdlVisitor.java:194-201), keep only the `api.StructType`, and register it
  as an auxiliary type on `metadata.Builder`.
- `parseColumnType` gains the `customType` arm: resolve against
  tables-then-aux-types (Java `findType` order); unknown names produce an
  unresolved placeholder (`api.StructType` unresolved form,
  datatype_composite.go:287-312), resolved at `Builder` build time by a
  ported `resolveTypes`: dependency graph, topological sort, cycle →
  `"Invalid cyclic dependency in the schema definition"`, missing →
  `"could not find type '%s'"` with Java's error codes.
- Constraint parity: `NOT NULL` only on ARRAY (DdlVisitor.java:161);
  array-of-struct element forced non-nullable (SemanticAnalyzer.java:739);
  name-collision rejection (`verifyNameIsNotUsed` semantics); normalization
  reuses the existing identifier normalization (the case-folding machinery
  the positional layout already enforces, ddl.go:806-810).
- Nested primary keys: port `toKeyExpression`'s dotted walk
  (RecordLayerTable.java:313-329) into the Go builder's primary-key path,
  emitting `field(storage).nest(…)` — the Go record layer's nesting key
  expressions already exist (builder.go:243 comment).

### 4.2 Wire (descriptor emission reworked to Java's shape)

- Struct descriptors become **top-level messages** in the template's file
  descriptor, named by a ported `toProtoBufCompliantName` (with
  `toUserIdentifier` inverse), de-duplicated template-wide by name — exactly
  `FileDescriptorSerializer.registerTypeDescriptors`. The current
  nested-type emission in `buildMessageDescriptor`/`structDescriptor` is
  replaced, not kept alongside.
- Struct fields (and struct-typed columns) are `LABEL_OPTIONAL`
  (Type.java:2352); field numbers = declaration index + 1.
- The nullability-collapse limitation (RecordLayerTable.java:156-174) and
  the unreferenced-struct / deserializer asymmetry (§2.3) are reproduced,
  each pinned by a test naming the Java TODO so the day upstream fixes it
  the Go pin fails loudly instead of silently diverging.
- **Proof**: descriptor and full `RecordMetaData` proto byte-goldens
  generated from the Java conformance server (`conformance/`) for a
  representative template set — every §1.2 construct: struct-of-struct,
  array-of-struct, nullable variants, nested PK, deep nesting, struct with
  VECTOR/UUID fields, name escaping (`"x$$"`), and explicitly a **nullable
  array-of-struct column**, so the NullableArrayWrapper emission shape (the
  RFC-143 §3a divergence the builder comment at builder.go:540-542 already
  tracks) is pinned by bytes rather than left to the scalar-array goldens.
  Plus a record round-trip:
  Java writes / Go reads and Go writes / Java reads the nested-tests rows.

### 4.3 DML (mirrors parseRecordField target-type push-down)

- The INSERT path types row constructors against the target table's
  `values.RecordType` (pushed as generator state, as Java pushes fragment
  state), recursing per field with coercion — replacing the
  `values.UnknownType` cells in `insert_cascades.go:155-175`. A bare
  `(2, 3)` acquires names and types from the target, nested `null` included.
- `ConvertToProtoValue` gains the general `MessageKind` arm (build the
  nested dynamic message from the typed constructor output) and the
  `fd.IsList()` arm. The list arm is exactly CQ-72's
  `engine-gap:array-literal-values` seam (gaps.go:38-47): scalar arrays,
  struct arrays, and arrays nested inside structs all land here, so that
  ledger class closes in the same phase — array-of-struct literals cannot
  work without it, and shipping a struct-only list arm would be the
  simplified-for-now split CLAUDE.md forbids.

  **CQ-72 merge-order contract**: a concurrent branch
  (`fix/array-literal-insert-values`) is landing the plain-array
  `ConvertToProtoValue` list arm now. Whichever of that branch and this
  RFC's Phase 2 lands second rebases onto the other's converter arm and
  extends it in place — never a second list arm beside the first, and never
  a rewrite that discards CQ-72's pinned regression tests. Phase 2's
  array-literal acceptance line is therefore re-measured at landing time:
  `engine-gap:array-literal-values` may already be empty, in which case
  Phase 2 inherits and extends the arm (struct elements, arrays nested in
  structs) rather than claiming the class.
- UPDATE SET with struct literals and nested-path targets goes through the
  same typed-constructor machinery, keyed by `FieldPath` as Java keys its
  transform map. INSERT-SELECT needs no new machinery once the planner
  flows record-typed values (§4.4).
- Read direction: `ProtoValueToDriver` returns an `api.Struct`
  implementation (Java: `RelationalStruct`) instead of leaking
  `protoreflect.Message`; the corpus matcher compares it against the nested
  YAML maps (`H: {E: {A: 3, …}}`).

### 4.4 Query surface (mirrors SemanticAnalyzer resolution)

- Replace the `parseColRef` last-dot split with Java's `Identifier` model:
  the full segment list flows to the semantic scope, and `ResolveColumn` /
  `ResolveQualifiedColumn` grow the `lookupNestedField` descent — after the
  longest prefix matches an output attribute, remaining segments walk the
  `StructType` fields into `FieldValue.Accessor(name, ordinal)` entries,
  resolved and fused into one multi-accessor `FieldValue`
  (`values.FieldPath` exists; the SQL layer finally uses it). This honours
  RFC-197: the accessor is (name→ordinal at resolution time), and ordinals
  are what survive into the plan.
- `structcol.*` and qualified-star expansion port `expandStar` /
  `expandStructExpression` (ordinal-numbered field values), with Java's
  "attempt to expand non-struct column" rejection.
- Named struct literals `STRUCT name(…)` port
  `RecordConstructorValue.ofColumnsAndName` + `withName` (storage-name
  recomputation), so a projected named struct carries its declared type name
  into metadata (§4.5) and coercion.
- Whole-struct comparison: verify Go's relop encapsulation rejects
  `TypeCodeRecord` operands with Java's error, and pin it — including the
  `IS NULL` case vector.yamsql keeps commented out. If Go currently accepts
  any of it, that is a conformance bug to close here.

  **AMENDED — measured, and the bullet above is wrong on two counts.** The
  measurement is `conformance/whole_struct_comparison_java_probe_test.go`,
  both engines live against the 4.12.11.0 conformance server.

  1. *There is no clean SQLSTATE to pin Go to.* Java's rejection comes from
     `RelOpValue`'s primitive-comparand assert (`RelOpValue.java:333,:345,
     :350` → `SemanticException.ErrorCode.
     COMPARAND_TO_COMPARISON_IS_OF_COMPLEX_TYPE`, declared at
     `SemanticException.java:45`), and `ExceptionUtil.translateErrorCode`'s
     switch has **no case** for that code (`ExceptionUtil.java:88-103`), so it
     falls through to `default: INTERNAL_ERROR` — `ErrorCode.java:177`,
     `XX000`. What the probe MEASURES at the surface is weaker still: the raw
     SemanticException text with an EMPTY sqlstate, not `XX000`. So the
     original bullet's "reject with Java's error" describes an internal error
     leaking to the client, and "pin it" would pin an artefact of a missing
     switch case. Go rejects the same shapes today with `0AF00` "Cascades
     planner could not plan query" — a different code for a shape both engines
     refuse, which is the `engine-gap:error-class` family, not a silent-wrong.

  2. *`IS NULL` on a whole struct must NOT be broken in Go.* vector.yamsql
     keeps `where embedding is null` and `embeddingGrouped is null` commented
     out against upstream issue 3700 — that is a Java **limitation filed as a
     bug**, not a designed rejection, and Java answers those shapes with the
     same internal error. Go answers them CORRECTLY (measured: `WHERE home IS
     NULL` → the NULL-struct row, `IS NOT NULL` → the other, `SELECT home IS
     NULL` → `true`). Deleting that to match an internal error would trade a
     right answer for a wrong one, and the read-side reach rule is explicit
     that a capability Java lacks entirely is an allowed extension so long as
     wire compat holds — which it does; nothing here touches what is written.
     The negative pin the original bullet asked for is therefore recorded as
     what it is: the two commented-out vector.yamsql shapes are Java's
     limitation, and the probe is the standing record of Go answering them.

  One thing the probe DID surface that is a genuine Go defect, and it is
  neither of the above: `SELECT id FROM T_S WHERE home = home` returns ZERO
  rows in Go where the non-NULL row should match. Java errors on it. Go
  neither errors nor answers correctly — it plans a whole-struct comparison
  and silently evaluates it false. That is the one arm of this bullet that is
  still open, and it is silent-wrong rather than an error-class difference.
- The array-element landmine (§3 item 6) is closed at the type-system level:
  array types model their element type (Java `Type.Array` does; "7.6 does
  not model message element types" is the standing admission), retiring the
  proto-field fallback in `gatheredExplodeElement` rather than extending it.

### 4.5 Metadata surface (CQ-74, scoped to what the corpus asserts)

- `executor.ColumnDef` carries the structured type (`api.DataType`), not one
  string; `deriveColumnsFromPlan` maps `values.RecordType` (with its
  retained declared name) → `api.StructType`. Requires threading the
  declared type NAME through the planner as Java's `Type.Record`
  name/storageName pair does — the piece `valueTypeName`'s flat `"STRUCT"`
  cannot express.
- Implement `api.StructMetaData` / `api.ArrayMetaData` backed by
  `api.StructType` (Java `RelationalStructMetaData`), thread through the
  driver, and enable the corpus's nested `resultMetadata:` assertion —
  emptying `unsupported:result-metadata-nested` (a class that only becomes
  REACHABLE once struct DDL lands; today it is masked, corpus_run_test.go:295).
  Array type names render Java's `ARRAY(elem)` / `ARRAY(STRUCT)` forms.

### 4.6 Index expressions over nested fields (joint with RFC-202)

This RFC adds no index syntax: every nested-path index in the 39 files is
AS-SELECT form, and the classic `ON` form CANNOT express a nested path — its
`indexColumnSpec` is a bare `uid` (RelationalParser.g4:182-184). The single
classic index in the 39 (arrays-unnesting.yamsql:44) is a flat column over a
`CREATE VIEW`, a §8 non-goal booked as that file's residual in §1.3. The requirement laid on RFC-202's generator: consume
multi-accessor resolved chains and emit `field(storage).nest(…)` per
MaterializedViewIndexGenerator.java:778-827, storage names throughout,
fan-out for unnest-defined index quantifiers, reject direct array-field
indexing without unnesting. The nested-PK walk (§4.1) and this share the
key-expression emission helper. If RFC-204 Phase 1 lands first, the 16
asIdx files re-book to `unsupported-DDL:value-index-as-select` — measured,
named, and already owned by CQ-71.

## 5. Rejected alternatives

- **Keep the nested-descriptor emission and translate on read.** Rejected:
  the catalog persists `RecordMetaData.toProto().toByteArray()` as the
  wire-compat bar (fdb_template_catalog.go:249); a translation layer means
  Go-created templates are not byte-identical to Java's and Java clients
  see a different descriptor shape. Emit Java's shape, delete the nested
  form — no parallel pipelines.
- **Inline/anonymous struct column types** (`col STRUCT(a bigint)`).
  Rejected: Java's grammar has no such production (columnType is
  `primitiveType | uid`, RelationalParser.g4:138-139); inventing one widens
  the shared surface without demand — zero corpus files use it.
- **Whole-struct comparison as a Go extension.** Rejected: Java rejects it
  structurally (RelOpValue.java:320-322) and the corpus's shared surface is
  governed by the conformance principle; an unreviewed widening is exactly
  the `maxRows.yamsql` divergence class the ledger books
  (`conformance:go-accepts-what-java-rejects`).
- **Extending the last-dot `colRef` split with try-every-split heuristics.**
  Rejected: Java resolves through the scope with typed descent
  (lookupNestedField); string-split heuristics are the qualifier-stripping
  hack CLAUDE.md bans and break on `"foo.tableA"`-style quoted identifiers
  the corpus uses (valid-identifiers.yamsql).
- **A struct-literal AST/value node distinct from record constructors.**
  Rejected: Java has exactly one mechanism — `RecordConstructorValue` typed
  by target push-down (parseRecordField) — and it covers positional, named,
  nested-null, UPDATE and projection cases uniformly.
- **Doing DML conversion at the executor by reflecting on the descriptor
  only (no typed constructor).** Rejected: coercion rules (bigint literal
  into integer field, named-field adoption) live in the typed layer in Java
  (coerceIfNecessary); a descriptor-only converter would re-derive them and
  drift.
- **Landing metadata (Phase 4) before DML (Phase 2).** Rejected: the 12
  check-result-metadata files cannot even load their rows without struct
  literals; ledger movement would be zero.

## 6. Phasing, acceptance, and estimates

Each phase is independently landable, updates the pinned ledger + digest in
the same commit, and carries its own tests (unit + FDB + corpus). Ledger
predictions below are INFERRED from the §1.3 matrix and re-MEASURED at each
landing; the pinned ledger is the acceptance instrument, not the prediction.

**Phase 1 — types, DDL, wire.** §4.1 + §4.2. Struct registry, forward refs +
topological resolve, custom-type column arm, array-of-struct, constraint
parity, nested PKs, descriptor emission reworked to Java's shape, catalog
round-trip, byte-goldens vs the Java conformance server.
Acceptance: `unsupported-DDL:struct` 39 → 0. Files re-book: ~16 →
`unsupported-DDL:value-index-as-select` (if RFC-202 has not landed), the
remainder → a NEW measured class (`engine-gap:struct-dml`, declared in
ledger.go with per-file signatures in gaps.go so any other failure stays
red). CQ-73's two gap entries delete. Estimate: **L** (~2.5-4k LOC incl.
tests; the resolver + descriptor rework + goldens dominate).

**Phase 2 — DML + values.** §4.3. Typed row constructors, MessageKind + list
converter arms, UPDATE SET structs, `api.Struct` read-back, corpus matcher.
Acceptance: `engine-gap:struct-dml` empties; `engine-gap:array-literal-values`
(6 files today) is re-measured under the §4.3 merge-order contract — it
empties here unless `fix/array-literal-insert-values` already emptied it, in
which case Phase 2 extends that arm in place; first struct files go green (predicted: nested-tests,
arrays, nested-with-nulls-class files whose only residual is plan-assertion
suppression). Estimate: **L** (~2-3k LOC).

**Phase 3 — query surface.** §4.4. Nested resolution, star expansion, named
literals, comparison-rejection parity, typed array elements.
Acceptance: dotted-access files green where no index/prepared residual
remains (predicted: functions, in-predicate/groupby/orderby move to their
index-gated class if RFC-202 is still open); the negative pins (42701-style
rejections, non-struct expansion) land as tests. Estimate: **L** (~2-3k LOC;
the semantic-scope surgery is the risk center).

**Phase 4 — metadata surface (CQ-74 struct/array half).** §4.5.
Acceptance: `unsupported:result-metadata-nested` becomes reachable, then
empties; the 12 check-result-metadata struct files go green (7 as passing
NEGATIVES — wrong-name/wrong-type must fail for the right reason, which the
polarity accounting already distinguishes); the arraymetadata truncation
sentinels (`arraymetadata_fdb_test.go`) flip to asserting the real values.
Estimate: **M** (~1.5-2k LOC).

**Phase 5 — nested index expressions (joint with RFC-202).** §4.6.
Acceptance: the asIdx files green as the intersection of both RFCs lands;
index key expressions byte-identical to Java's for the orderby/i7 deep-nest
and subquery-tests unnest shapes (CQ-71's own DONE bar, TODO.md:12988-12992).
Estimate: **M as a delta on RFC-202** (~1-1.5k LOC).

End state over the 39: predicted ~30+ green; the rest re-book to honest
later-phase classes (`unsupported:prepared`, `unsupported:random-injection`,
`unsupported:temporary-function` — RFC-201 Phases 4/5), each still counted,
none silent.

## 7. Test strategy beyond the corpus

- **Descriptor byte-goldens** (Phase 1): Java conformance server emits
  `RecordMetaData.toProto()` bytes per representative template; Go must
  produce identical bytes. Golden files are committed; a mismatch prints the
  descriptor diff.
- **Cross-engine record round-trip** (Phase 2): Java writes nested-tests
  rows, Go reads and asserts; Go writes, Java reads. Split-record and
  version paths unchanged (struct columns are ordinary message fields).
- **Negative pins**: cyclic struct rejection, unknown type, NOT NULL on
  struct, duplicate name across tables/aux types, whole-struct comparison
  rejection, non-struct `.*` expansion — each with Java's error identity.
- **Fuzz**: the resolver (random dependency graphs incl. cycles) and the
  literal-conversion path (random typed trees) get fuzz targets per the
  repo's fuzz bar.
- **Mutation checks** per phase per the standing protocol: each fix
  direction (placement, label, storage-name escaping, ordinal vs name
  resolution) reverted independently must go red.

## 8. Explicit non-goals

- `CREATE TYPE AS ENUM`, views, SQL functions (RFC-201 Phase 4;
  `unsupported-DDL:other`/`function` classes).
- Prepared struct parameters and `!r` injection (RFC-201 Phase 5).
- The classic `CREATE INDEX … ON t(col)` form for nested paths — Java cannot
  express it (bare `uid`, RelationalParser.g4:182-184) and neither will Go.
- Fixing Java's nullability-collapse TODO ahead of upstream.
