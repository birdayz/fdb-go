# RFC-207 — CREATE VIEW: schema-template views, macro expansion, and indexes on views

- **Status:** PROPOSED
- **Branch:** `rfc/207-create-view`
- **Depends on:** RFC-202 implementation (PR #577, `feat/rfc202-value-index-as-select`)
  — see §0(b). Interacts with RFC-204 (arrays-unnesting) and
  RFC-206 (SQL functions, drafted concurrently). That branch moves; every
  `on that branch` line number below was read at `335b6beb7` and must be
  re-confirmed at rebase time, not trusted.
- **Authorized by:** TODO.md item 69.7 (`TODO.md:13025-13026`), RFC-201's
  Phase 4 — "SQL functions (… 44 files), **views** (11), **enums** (3)"
  (`rfcs/201-layered-test-corpus.md:165-166`). The "11" is this RFC's §1
  population, measured independently below. This is what puts a
  `fdb-relational` feature in scope at all under CLAUDE.md; RFC-206 takes the
  functions half of the same item.
- **Java reference:** `fdb-record-layer/` at tag 4.12.11.0. Views landed upstream in
  4.8.3.0 ("Support SQL views — PR #3680",
  `fdb-record-layer/docs/sphinx/source/ReleaseNotes.md:1642`).
- **Review gate:** this changes the query front end (FROM resolution) and the DDL
  metadata path — Graefe ACK required on this RFC and one implementation lap;
  Torvalds + @claude + codex per the milestone cadence.

CREATE VIEW is the single blocker named by three prior workstreams: the corpus
class `unsupported-DDL:other` (10 files on the RFC-202 branch), the three
`index-ddl*` files RFC-202 explicitly left "resting at CREATE VIEW", and
arrays-unnesting's view residual that RFC-204's review booked as outliving every
phase of that RFC. This RFC specifies the port: two strings on the wire, macro
expansion in the planner, and the RFC-202 generator reused unchanged for
indexes-on-views.

---

## 0. Framing and baseline corrections

Four facts the implementation must be built on, all measured (§9):

**(a) On master (`51d9e9701`), CREATE VIEW is SILENTLY DROPPED — not rejected.**
`execCreateSchemaTemplate` (`pkg/relational/core/embedded/ddl.go:120`) makes
exactly two passes over `AllTemplateClause()`: a table pass (`:148-170`) and an
index pass (`:173-190`); a `viewDefinition` clause satisfies neither `if` and is
`continue`d without error. The duplicate catalog-less builder
`buildSchemaTemplateFromDDL` (`pkg/relational/core/embedded/cascades_generator.go:5616`,
passes `:5648-5661` and `:5663-5672`) has the same shape. The template is
"created" minus its views; the first query against the view name dies later at
`validateTablesAndColumnsInner` (`cascades_generator.go:5256-5267`) with a
misleading `42F01 table "v" does not exist`. The fail-closed
`rejectUnsupportedTemplateClauses` the task brief attributes to master is **not
on master** — it lives on PR #577 (`feat/rfc202-value-index-as-select`,
`pkg/relational/core/embedded/ddl.go:210-236` on that branch, view arm at
`:231-233`), where a view clause gets an explicit
`0A000 views (CREATE VIEW) are not yet supported in a schema template`.
That rejection has **no dedicated unit test** on the branch (only the corpus
ledger pins it indirectly); §7 S3 closes that gap in the same commit that
deletes the arm.

**(b) Merge order: RFC-207's implementation rebases onto PR #577.** Three
reasons, each sufficient: (1) the three `index-ddl*` files rest on *nothing but*
views only after #577's INCLUDE/orderClause/ON-source work — on master they fail
earlier; (2) indexes-on-views consume `queryddl.Generate` and
`AddGeneratedIndex`, which exist only on that branch (§4 D5); (3) without #577
the baseline behaviour is silent data loss, and "delete the fail-closed arm" —
the correct, self-measuring acceptance step — has nothing to delete. Landing on
master first is a rejected alternative (§6.4).

**(c) Wire-fidelity defect, live today: Go DESTROYS Java-authored views on
rewrite.** `MetaData.views` (field 15) is a *known* field of the generated Go
type (`gen/record_metadata.pb.go:960`), so unknown-field preservation — the
mechanism CLAUDE.md relies on for out-of-scope relational features — does
**not** protect it: `proto.Unmarshal` parses views into the struct,
`RecordMetaDataFromProto` (`pkg/recordlayer/metadata_proto.go:104`) discards
them, and `ToProto()` (`:18-99`) rebuilds the proto without them.
`serializeTemplate` (`pkg/relational/core/catalog/fdb_template_catalog.go:250-257`)
then persists the stripped form. A Java-authored schema template containing
views, read and re-saved by Go, silently loses its views. No test pins this.
This is a hard-line wire bug independent of any query-side view support, and it
is the implementation's first red→green pin (§7 S1).

**(d) "MaterializedViewIndexGenerator" is a misnomer — Java views are never
materialized.** Four independent proofs: the persisted proto is two strings
(§2.2); `fdb-relational-api/.../api/metadata/View.java:26` says "Metadata for a **non-materialized**
view"; `RecordLayerView.java:32-33` says "stored as raw SQL definitions and
compiled lazily when referenced"; the docs (`VIEW.rst:7`) say "Views do not
store data themselves… read-only and cannot be the target of INSERT, UPDATE, or
DELETE." The *index* produced by the generator is the materialization; the view
is a macro. No Go design decision may assume view storage beyond the two
strings.

---

## 1. Demand, measured — every corpus file that declares a view

11 of the 238 vendored files declare `CREATE VIEW`
(`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/`). Counts
are `grep -ci "create view"` per file; constructs read from the files.

| file | views | constructs | needs beyond views | moves in |
|---|---|---|---|---|
| `views.yamsql` | 7 | predicate view (`:28`), **view over view** (`:29`), CTE body (`:30`), **view over SQL function** (`:31`), CTE+GROUP BY+MAX (`:32`), nested CTE (`:33`); queries: `SELECT *` over view, predicate pushdown into view (`:62,:81,:102`), projection | `function_view` needs CREATE FUNCTION (RFC-206); `join_view` is commented out upstream (`:23,:85-95`) | P1 (whole file once RFC-206 lands; without it, the file still skips on its `create function` clause) |
| `alias-tests.yamsql` | 2 | plain filtered view (`:25`), join view (`:26`); ~20 queries: view⋈view, view⋈table, CTEs over views, EXISTS over view (`:517`), derived tables over views | pre-existing planner gap CQ-72 ("EXISTS over a view inside a temporary-table-valued-function block", `javacorpus/gaps.go:81-83`) stays booked | P1 (partial; CQ-72 residual) |
| `index-ddl.yamsql` | 4 | aggregate views `min_ever`/`max_ever` GROUP BY (`:76-77,:85-86`) + `CREATE INDEX … ON view(cols)` incl. `options (legacy_extremum_ever)` (`:79-80,:88-89`) | — (rests on views only, per #577's ledger prose) | P2 |
| `index-ddl-values-only.yamsql` | 3 | filtered predicate views (`:87,:90,:93`) + ON-view indexes with `include` (`:88,:91,:94`) → sparse indexes from view predicates | — | P2 |
| `index-ddl-aggregates-only.yamsql` | 13 | aggregate views (SUM/COUNT/MIN/MAX/min_ever/max_ever + GROUP BY, `:69-99`) + `ON view(groupcols) include(agg)` | — | P2 |
| `arrays-unnesting.yamsql` | 1 | view over an UNNEST subquery (`:43`) + classic ON index over it (`:44`) — RFC-204's booked residual (`rfcs/204-struct-types-relational-layer.md:116-119`) | RFC-204 struct/unnest phases for the rest of the file | P2 (the view residual; file moves jointly with RFC-204) |
| `valid-identifiers.yamsql` | 6 | quoted/`$`-prefixed/unicode view names (`"$yay__view"`, `"வணக்கம்"`, `:70-74,:134-138`) | whatever else the file exercises past DDL | P1 (progresses past its view clauses) |
| `semantic-search.yamsql` | 2 | plain projection views (`:25,:30`) + `CREATE VECTOR INDEX … USING HNSW ON view(…)` (`:26-27,:31-32`) | vector-index-on-view (out of scope, §8) | residual |
| `semantic-search-advanced-metrics.yamsql` | 2 | same shape (`:25,:29`) | vector-on-view | residual |
| `sliding-window-semantic-search.yamsql` | 2 | views with **QUALIFY row_number() OVER (…)** bodies (`:25,:29`) + vector indexes on them | QUALIFY/window + vector-on-view (the `RowNumberWindowPredicate` form RFC-202 §10 scoped out) | residual |
| `documentation-queries/window-function-documentation-queries.yamsql` | 1 | plain projection view (`:25`) | window functions | residual |

Version gates: `views.yamsql` `supported_version: 4.8.3.0`,
`arrays-unnesting.yamsql` `4.12.1.0`, `index-ddl-aggregates-only.yamsql`
`4.9.1.0` — all ≤ our 4.12.11.0 pin; no gate excludes any file.

**P1 therefore does say so:** views alone move `views.yamsql` (modulo RFC-206),
`alias-tests.yamsql` (modulo CQ-72) and `valid-identifiers.yamsql`; P2 (indexes
on views, which is mostly plumbing over the RFC-202 generator, §4 D5) unlocks
RFC-202's three resting files **cheaply** plus arrays-unnesting's residual. The
four semantic-search/window files need views as a *prerequisite* but rest on
other named gaps afterwards.

---

## 2. Java is the spec

### 2.1 Grammar

`fdb-relational-core/src/main/antlr/RelationalParser.g4`:

```
viewDefinition                                                      // :245-247
    : VIEW viewName=fullId AS viewQuery=query
    ;
templateClause                                                      // :93-95
    : CREATE ( structDefinition | tableDefinition | enumDefinition
             | indexDefinition | sqlInvokedFunction | viewDefinition )
    ;
indexDefinition                                                     // :171-175
    : (UNIQUE)? INDEX indexName=uid AS queryTerm indexAttributes?                                  #indexAsSelectDefinition
    | (UNIQUE)? INDEX indexName=uid ON source=fullId indexColumnList includeClause? indexOptions?  #indexOnSourceDefinition
    | VECTOR INDEX indexName=uid USING HNSW ON source=fullId …                                     #vectorIndexDefinition
    ;
```

- CREATE VIEW is **only** a schema-template clause — `templateClause+` is
  consumed solely by `CREATE SCHEMA TEMPLATE` (`:97-101`); no standalone form
  (`docs/sphinx/source/reference/sql_commands/DDL/CREATE/VIEW.rst:7`).
- **There is no DROP VIEW** — `dropStatement` (`:113-117`) has only DATABASE /
  SCHEMA TEMPLATE / SCHEMA. Builder-level `removeView`
  (`RecordLayerSchemaTemplate.java:530-534`) has no SQL surface.
- `CREATE INDEX … ON source` takes a generic `fullId` resolved to table **or**
  view at DDL time (§2.5). The vector variant shares the same source resolution
  (`DdlVisitor.java:265-268`).
- `VIEW` is a non-reserved keyword (`RelationalLexer.g4:745`,
  `RelationalParser.g4:1369`) — a table or column may be named `view`.

Go already has the identical grammar and generated machinery:
`pkg/relational/core/parser/grammar/RelationalParser.g4:245-247`, generated
`TemplateClauseContext.ViewDefinition()`
(`pkg/relational/core/parser/gen/relational_parser.go:5228`), and an unused
`parser.ParseView` (`pkg/relational/core/parser/parser.go:118-142`, func at
`:124`) that
mirrors Java's `QueryParser.parseView`. **No grammar work is needed.**

### 2.2 Storage — two strings, nothing else

`fdb-record-layer-core/src/main/proto/record_metadata.proto`:

```proto
message MetaData {  …  repeated PView views = 15;  … }   // :212
message PView {                                          // :223-226
  optional string name = 1;
  optional string definition = 2;
}
```

The persisted form is the view name and the **raw SQL text of the query body**.
No serialized plan, no KeyExpression, no result-type descriptor. Model class
`metadata/View.java:33-92` (`toProto` `:57-62`, `fromProto` `:90-92`); builder
plumbing `RecordMetaDataBuilder.java:117` (viewMap), `:237-240` (deserialize),
`:1214-1216` (`addView`); serialize `RecordMetaData.java:706`; accessor
`getViewMap()` `:726-729`.

Two properties of the definition string are load-bearing:

1. **It is a verbatim substring of the user's DDL text**, cut at the query
   subtree's token start/stop offsets — `DdlVisitor.java:549-552`:
   `queryString.substring(viewCtx.viewQuery.start.getStartIndex(), viewCtx.viewQuery.stop.getStopIndex()+1)`.
   Java's tests assert exact text including casing and whitespace
   (`DdlStatementParsingTest.java:1169` expects `"SELECT * FROM bar"`, `:1292`
   the full CTE text). Go must capture the same substring, **never** a
   re-rendered AST (and never `GetText()`).
2. **Prepared parameters are rejected in view bodies** —
   `QueryParser.validateNoPreparedParams(viewCtx)` (`DdlVisitor.java:554-555`).

### 2.3 The relational model and the two compile paths

`RecordLayerView` (`fdb-relational-core/.../metadata/RecordLayerView.java`)
carries `{name, description(=SQL text), isTemporary(always false)}` plus a
**non-persisted** `Function<Boolean, LogicalOperator> compilableViewSupplier`
(`:60-61`; the Boolean is identifier case-sensitivity). `asRawView()`
(`:101-104`) drops the compiler for persistence; equality ignores the compiler
(`:107-122`, pinned by `RecordLayerViewTests.java:126-142`).

Java has **two deliberately different compiler installations**, and the port
keeps both 1:1:

- **DDL time** (`DdlVisitor.getViewMetadata`, `:540-571`): the body is compiled
  once, eagerly, against the metadata built so far, with literal processing
  disabled (`:557-563`); the installed supplier is a **constant closure**
  `ignored -> viewQuery` (`:566-570`) that ignores the case-sensitivity flag.
  Eager compilation is what makes a bad view body fail the DDL statement.
- **Load time** (`RecordMetadataDeserializer.java:122-127`): each `PView`
  gets a **lazy, per-reference recompiling** supplier
  `isCaseSensitive -> RoutineParser.sqlFunctionParser(metadata.get()).parseView(name, definition, isCaseSensitive)`
  (`:158-162`, `RoutineParser.java:66-95`), over a `Suppliers.memoize`d
  template snapshot — the memoization is what makes views-over-views work after
  deserialization (all views are registered before the first compile
  materializes the catalog).

### 2.4 Planner treatment — macro expansion, one `else if`

`LogicalOperator.generateAccess`
(`fdb-relational-core/.../query/LogicalOperator.java:177-226`) resolves a FROM
source in fixed priority order:

1. CTE (`:186-193`) → 2. table (`:194-200`) → 3. **view** (`:201-207`) →
4. table function (`:208-215`) → 5. correlated field access (`:216-225`).

The view branch:

```java
} else if (semanticAnalyzer.viewExists(identifier)) {
    Assert.thatUnchecked(atAlias.isEmpty(), ErrorCode.WRONG_OBJECT_TYPE, …
        "AT clause requires an array-typed column, but '%s' is a view" …);
    return semanticAnalyzer.resolveView(identifier).withNewSharedReferenceAndAlias(alias);
}
```

`resolveView` (`SemanticAnalyzer.java:1186-1196`) fetches the
`RecordLayerView` from the metadata catalog and calls
`compilableViewSupplier.apply(isCaseSensitive)`.
`withNewSharedReferenceAndAlias` (`LogicalOperator.java:170-175`) wraps the
compiled expression in a **fresh `Quantifier.forEach` over the same
`Reference`**, rewires the output QOV, and applies the alias. That is the whole
mechanism: the view's expression subtree is spliced into the caller's graph as
an ordinary quantifier, and every Cascades rule (predicate pushdown, index
matching on the base tables) applies transparently. There is **no expansion
cache** — `LogicalOperatorCatalog.lookupTableAccess`
(`LogicalOperatorCatalog.java:51-64`) caches table access only; the view branch
bypasses it.

### 2.5 Indexes on views — the SAME generator RFC-202 ported

`DdlVisitor.visitIndexOnSourceDefinition` (`:222-258`) resolves `source` via
`generateSourceAccessForIndex` (`:311-325`) — **the exact same
`generateAccess` used for FROM**, so a view source arrives macro-expanded. The
only view-specific line in the whole feature is the normalization for bare
tables: `if (semanticAnalyzer.tableExists(...))` wrap the table in a trivial
SELECT (`:318-322`) so both shapes reach the generator as a `SelectExpression`
(asserted at `OnSourceIndexGenerator.java:180`).

`OnSourceIndexGenerator.generate()` (`:172-229`) pushes the named index columns
through the view's result value (`:184-197`), rebuilds a select **reusing the
view's quantifiers and predicates** with the index projection (`:212-217`),
adds the ASC/DESC/NULLS sort (`OrderByExpression.of` at `:205-209`, applied via
`generateSort` at `:225`), and then delegates to
`MaterializedViewIndexGenerator.from(...)` (`:226-228`) — the identical entry
point `visitIndexAsSelectDefinition` uses (`DdlVisitor.java:216`). The index
lands **on the base table** (`DdlVisitor.java:434-439`,
`metadataBuilder.extractTable(index.getTableName())`); a view never carries an
index and nothing view-related is persisted for the index.

Proof it expands the body: `DdlStatementParsingTest.java:1416-1457`
(`createIndexOnPredicatedView`) — `CREATE VIEW v1 AS SELECT b, c FROM bar WHERE a < 100`
+ `CREATE INDEX i1 on v1(b, c)` yields an index on `bar` with
`concat(field("b"), field("c"))` and the view's `a < 100` lowered into the
index's `Predicate` proto (a sparse index). Derived-table view:
`createIndexOnBasicSyntaxComplex` (`:1504-1512`).

### 2.6 Name resolution, phase order, and errors

- **Flat namespace**: `RecordLayerSchemaTemplate.Builder.verifyNameIsNotUsed`
  (`:712-717`) rejects a view name colliding with a table, auxiliary type,
  routine, or another view — `ErrorCode.INVALID_SCHEMA_TEMPLATE` (42F59).
- **Lookup**: `SemanticAnalyzer.viewExists` (`:210-227`) mirrors `tableExists`
  (`:189-208`) byte-for-byte: ≤1 qualifier, single qualifier must equal the
  schema name, then `findViewByName`.
- **Phase order** (`DdlVisitor.java:400-439`): clauses are collected first, then
  processed enums → structs → tables → functions → **views** → indexes. Hence:
  a view may reference a table declared later in the template text
  (`DdlStatementParsingTest.java:1266-1276`); views resolve **among
  themselves in lexical order** (`:430-433` re-invokes `metadataBuilder.build()`
  per view, so each view sees all previously added views); a *function* body
  referencing a view fails `UNDEFINED_TABLE` (upstream issue #3493,
  `DdlStatementParsingTest.java:1344-1364`) because functions compile before
  views — Go reproduces this, per the conformance principle.
- **Views over views**: supported (`createNestedViewWorks` `:1176-1200`,
  view self-join `:1202-1213`, `views.yamsql:29`, `VIEW.rst:73-100`).

Error surface (Java → Go; the Go constants already carry the identical
SQLSTATEs, `pkg/relational/api/errcode.go`):

| condition | Java | SQLSTATE | Go constant |
|---|---|---|---|
| view name collides (table/type/routine/view) | `INVALID_SCHEMA_TEMPLATE` (`ErrorCode.java:147`) | 42F59 | `ErrCodeInvalidSchemaTemplate` (`errcode.go:114`) |
| unknown FROM name (unknown view, view over unknown table) | `UNDEFINED_TABLE` "Unknown table %s" (`SemanticAnalyzer.java:301-305`) | 42F01 | `ErrCodeUndefinedTable` (`errcode.go:95`) |
| malformed view body | `SYNTAX_ERROR` (`DdlStatementParsingTest.java:1249-1265`) | 42601 | `ErrCodeSyntaxError` (`errcode.go:83`) |
| `AT` (unnest) clause on a view | `WRONG_OBJECT_TYPE` (`LogicalOperator.java:202-206`) | 42809 | `ErrCodeWrongObjectType` (`errcode.go:92`) |
| index column unresolvable against view output | `UNDEFINED_COLUMN` (`OnSourceIndexGenerator.java:201,207`) | 42703 | `ErrCodeUndefinedColumn` (`errcode.go:87`) |
| prepared param in view body | rejected (`DdlVisitor.java:554-555`) | — | same routing as Java's `validateNoPreparedParams` |

---

## 3. The wire — what bytes persist, and the goldens

**Characterization.** A schema template persists as a marshaled
`com.apple.foundationdb.record.MetaData` in the `META_DATA` column of a
`Templates` catalog row (`fdb_template_catalog.go:127-165`, `:250-257`;
`proto/relational/catalog_data.proto:33-36`). A view contributes exactly one
`PView{name, definition}` element to `repeated views = 15` — name unescaped
(Java test `DdlStatementParsingTest.java:486-487`), definition the verbatim
source substring (§2.2). Nothing else on disk changes; indexes derived from
views are ordinary index protos on the base table (§2.5). Wire risk is
therefore **small and enumerable**: field 15 round-trip fidelity + the
substring capture.

**Go today** parses field 15 into `gen.MetaData.Views`
(`gen/record_metadata.pb.go:960,1184`) and then throws it away
(`pkg/recordlayer/metadata_proto.go` — zero hits for `Views`), per §0(c).

**Byte-goldens, live JVM, per RFC-202 D11 mechanics**
(`rfcs/202-value-index-as-select.md:866-905`): the conformance server's
`createSchemaTemplatePersistentJava` (`conformance/sql_plan_steps.java:643-657`
— RFC-202 §D11 cites `:332-346` for the same method, which is stale)
executes arbitrary template DDL and persists Java's `MetaData` into the shared
FDB catalog; Go reads the stored bytes back via
`SchemaTemplateCatalog().ListTemplates` (`fdb_template_catalog.go:167-192`).
RFC-207 adds to that existing `conformance_java`-tagged suite:

- **G1 (preservation, the §0(c) pin):** Java persists a template with ≥2 views
  (incl. one quoted unicode name and one multi-line CTE body); Go loads it,
  re-serializes, and the `views` field of the re-serialized proto is
  **byte-identical** to the stored Java bytes (comparison on the raw field, so
  a Go deserialization bug cannot mask a divergence).
- **G2 (production):** Go executes the same DDL text; the resulting stored
  `views` field is byte-identical to Java's — this pins the verbatim-substring
  capture (offsets, casing, whitespace) against the live JVM, not against our
  reading of the Java source.
- **G3 (index-on-view):** for the §2.5 predicated-view shape, Go's stored
  index proto (root expression, predicate, options) matches Java's — this
  extends RFC-202's D11 comparison to view-derived indexes.

---

## 4. The design

Architecture in one sentence: **a view is a catalog-level CTE** — the same
"name → compiled logical operator" substitution the CTE machinery already
performs, sourced from schema metadata instead of a WITH clause, resolved at
the same single FROM-dispatch point Java uses, with the RFC-202 generator
consuming the expanded body for indexes. No parallel pipeline, no new
persistence format, no materialization.

### D1 — record-layer metadata carries `PView` (the wire fix)

`recordlayer.RecordMetaData` (`pkg/recordlayer/metadata.go`) gains an ordered
`views` collection of `{Name, Definition string}` mirroring Java's `viewMap`
(`RecordMetaDataBuilder.java:117`); `ToProto`/`RecordMetaDataFromProto`
(`pkg/recordlayer/metadata_proto.go`) carry field 15 both directions,
order-preserving. This is deliberately dumb — two strings — because that is the
entire wire footprint (§2.2). Ships first (§7 S1) with the G1 golden, before
any SQL-layer work, because it fixes a live data-loss bug on the shared-cluster
path.

### D2 — the relational metadata model

`pkg/relational/core/metadata` gains `RecordLayerView` (1:1 with
`RecordLayerView.java`): `{name, description, compiler func(caseSensitive bool) (logical.LogicalOperator, error)}`,
equality ignoring the compiler, `asRawView` equivalent for serde.
`RecordLayerSchemaTemplate` (`schema_template.go:22-29`) gains `views` +
`viewsByN` alongside its existing `tables`/`tablesByN`; the stub
`Views()`/`FindView()` (`:154-158`) become real; `Accept` already walks views
(`:230-241`). The builder gains `AddView` guarded by a Go
`verifyNameIsNotUsed` covering tables, auxiliary types, routines (when RFC-206
lands them), and views → `ErrCodeInvalidSchemaTemplate`, matching
`RecordLayerSchemaTemplate.java:712-717`. Serde: the template→proto visitor
adds views via D1; the proto→template deserializer installs the **lazy
recompiling supplier over a memoized template snapshot**, 1:1 with
`RecordMetadataDeserializer.java:122-162` — `parser.ParseView`
(`parser.go:120`) is the existing, currently-unused entry point built for
exactly this.

### D3 — the DDL pass, in BOTH template builders, in Java's phase order

Both `execCreateSchemaTemplate` (`embedded/ddl.go:120`) and
`buildSchemaTemplateFromDDL` (`cascades_generator.go:5615`) restructure from
"two typed passes" to Java's collect-then-phase order: enums → structs →
tables → functions → **views** → indexes (`DdlVisitor.java:400-439`). The view
phase, per view in lexical order:

1. rebuild the catalog-so-far (Java's per-view `metadataBuilder.build()`,
   `:430-433`) so views-over-views and forward table references behave
   identically, including reproducing issue #3493 (functions cannot see views);
2. capture the definition as the **verbatim substring** of the statement text
   at the `viewQuery` subtree's token start/stop indices (`:549-552`) — never
   `GetText()`;
3. reject prepared parameters in the body (`:554-555`);
4. compile the body eagerly with the ordinary `PlanVisitor` front end
   (`plan_visitor.go:137`) with literal processing disabled, so a broken view
   fails the DDL statement — and install the **constant closure** compiler
   (`:566-570`);
5. `AddView` with the collision check (D2).

On the #577 base, the `ViewDefinition` arm of
`rejectUnsupportedTemplateClauses` (`ddl.go:229-232` on that branch) is
**deleted, not relaxed** — the same discipline that branch's
`rejectIndexOrderClause` doc comment prescribes for itself. The mutation check
for S3 is the corpus itself: re-adding the arm must flip the unblocked files
red.

### D4 — FROM-clause expansion through the semantic scope

Resolution follows Java's fixed priority at the FROM-dispatch point: CTE →
table → **view** → (table function, when RFC-206 adds them) → correlated
(`LogicalOperator.java:177-226`). Concretely in Go:

- `select_parser.go`'s atom-table arm (`:2391-2415`) consults the CTE scope,
  then table metadata, then the template's `FindView`. On a view hit with an
  `AT` clause → `ErrCodeWrongObjectType` with Java's message; otherwise the
  view's compiler runs and the resulting operator is spliced exactly the way a
  derived table (`:2364-2389`) is today — fresh quantifier + alias over the
  compiled body, mirroring `withNewSharedReferenceAndAlias`
  (`LogicalOperator.java:170-175`). The existing CTE body machinery
  (`plan_visitor.go:156-280`, `cteBodies`/`buildCTEBodySelfHidden`) is the
  in-repo template for the splice; a view differs only in being
  catalog-scoped, globally visible, and non-shadowing-relevant (a CTE named
  like a view **shadows** it, because CTE outranks view in the priority order —
  a pinned test, since Java's order guarantees it).
- **No expansion caching** — Java has none (§2.4); a fresh splice per
  reference keeps alias/correlation identity correct for `v AS a, v AS b`
  self-joins.
- `validateTablesAndColumnsInner` (`cascades_generator.go:5251-5267`) needs
  **no view exemption list**: expansion happens during plan construction, so
  the validated plan contains only base-table scans. The behaviour is emergent
  from the architecture, not a bolted-on check — if a view name ever reaches
  the validator, `42F01 Unknown table` is exactly Java's answer for an unknown
  name.
- Error for an unknown name is unchanged (`ErrCodeUndefinedTable`), because the
  view branch simply doesn't fire — same fall-through shape as Java
  (`SemanticAnalyzer.java:301-305`).

### D5 — indexes on views: the RFC-202 generator, unchanged

`CREATE INDEX i ON v(cols) [include(...)] [options(...)]` reuses #577's
ON-source front end (`index_onsource.go`, S6 of RFC-202) with one added
resolution step, mirroring `generateSourceAccessForIndex`
(`DdlVisitor.java:311-325`): resolve `source` through the same access path as
D4 — table → wrap in trivial SELECT (already there); **view → compiled,
expanded body**. Then the existing pipeline runs untouched: synthesize the
projection `keyColumns ++ (include \ keys)` + ORDER BY from the key columns'
order clauses, `runFromResolutionPostPasses`, `queryddl.Generate`
(`query/ddl/generator.go:63`), `AddGeneratedIndex`. The generator's
decompose spine (`Project → Sort → Aggregate → Filter → Scan`,
`generator.go:117`; `Generate` at `:63`) already accepts precisely the shapes the corpus's view
bodies produce: predicate views → sparse indexes (S5's predicate arm),
aggregate GROUP BY views → aggregate indexes (the aggregate arm), incl.
`legacy_extremum_ever` (`index-ddl.yamsql:88-89`). **No new generator
machinery** — that is the payoff of RFC-202's D1 decision to have a single
producer of index metadata, and it is why P2 is cheap. Vector indexes ON views
stay out of scope (§8).

### D6 — conformance accounting

**No new `SkipClass`.** A `unsupported-DDL:view` class would be born dying —
this RFC's phases drive every view-caused skip to zero, and the files that
remain skipped afterwards are blocked on *other* named causes (vector-on-view,
QUALIFY, CREATE FUNCTION), which is where their classification belongs.
`classifyDDLByDeclaration` (`javacorpus/runner.go:434-466`) is untouched. Each
phase updates, in the same commit: `pinnedLedger`, `pinnedAssignmentDigest`
(recomputed from the on-mismatch dump, the standing technique), `EngineGaps()`
(CQ-72's alias-tests entry survives P1 with its detail updated if the failing
query shifts), and `SetupNegatives` if any view DDL polarity changes. The ANSI
roster side: `yamsql/ansiledger.go` gains Go entries for F031-02 / F311-03
(`ansi_roster.go:225,:299` already book Java as `SupportPartial`).

---

## 5. What moves, per phase

| phase | corpus movement | proof artifacts |
|---|---|---|
| P1 (S1–S4) | `views.yamsql` past its view clauses (fully green once RFC-206's `create function` lands — coordinate; without it the file still skips at the function clause with #577's 0A000), `alias-tests.yamsql` view queries execute (CQ-72 residual re-measured and re-booked), `valid-identifiers.yamsql` progresses past view DDL | G1/G2 goldens; yamsql scenarios incl. plan assertions that predicate-pushdown-into-view uses base-table indexes; ledger+digest re-pin |
| P2 (S5) | `index-ddl.yamsql`, `index-ddl-values-only.yamsql`, `index-ddl-aggregates-only.yamsql` — the RFC-202 resting three, whose ledger prose names CREATE VIEW as "the class's remaining cause"; arrays-unnesting's `T2_view`/`T2_view_index` residual (jointly with RFC-204) | G3 golden; `plan_contains` assertions that ON-view sparse/aggregate indexes actually fire; ledger+digest re-pin |
| out | semantic-search trio (vector-on-view), sliding-window + window-doc (QUALIFY/window) | booked residuals, §8 |

---

## 6. Rejected alternatives

1. **Persist a compiled plan / KeyExpression for the view.** Breaks the hard
   line — Java persists two strings (§2.2) and re-parses on load; a serialized
   Go plan is unreadable to Java and freezes planner internals into stored
   bytes. The argument stands on the proto alone; do not read
   `RecordMetadataDeserializer.java:188`'s `@SuppressWarnings` note as
   corroboration — it is about an unused formal parameter, not about deferred
   richer storage.
2. **Text-macro substitution** (splice the view's SQL text into the referencing
   query string and re-parse the whole thing). String manipulation of SQL is
   banned in this repo for cause; it also breaks literal extraction, alias
   scoping, and error offsets. Java expands the *compiled operator*, not text.
3. **A parallel view-expansion pipeline** (e.g. a pre-pass rewriting the AST
   before the ordinary front end). "No parallel pipelines" — Java resolves
   views inside the one FROM dispatcher (§2.4), and Go does the same at its one
   FROM-dispatch point (D4).
4. **Land on master before PR #577.** Baseline is silent view-dropping; the
   three index-ddl files aren't measurably "resting on views" there; the
   generator D5 reuses doesn't exist; and the fail-closed arm whose deletion is
   the natural acceptance event isn't there to delete.
5. **A named `unsupported-DDL:view` ledger class.** Born dying (§4 D6); adds
   `TestSkipClassesAreAllReachable` masking churn for a class whose target
   population is zero at the end of this RFC.
6. **Caching view expansions in the operator catalog.** Java deliberately
   bypasses `LogicalOperatorCatalog` for views (§2.4); a shared expansion
   breaks quantifier-identity freshness for self-joins over a view.
7. **DROP VIEW / standalone CREATE VIEW / temporary views / WITH CHECK
   OPTION.** Java has none of them (§2.1 for the grammar;
   `fdb-relational-api/.../api/metadata/View.java:46-48` — "Temporary views are
   not currently supported"; WITH CHECK OPTION appears nowhere in the tree, so
   that one is an absence, not a citation; roster F311-04 `SupportNone` at
   `ansi_roster.go:300`). Conformance principle: doesn't work in Java → doesn't work
   in Go, same shape.

---

## 7. Sequencing and estimates

Implementation happens on a branch cut from #577's head (rebased onto master
when #577 merges). Every step lands with its tests; mutation checks per the
standing protocol (each fix direction separately reverted).

- **S1 — wire fidelity** (~150 LOC + tests): D1. Red→green: a unit test feeding
  `MetaData` bytes containing `views` through
  `RecordMetaDataFromProto`+`ToProto` (red on master), then golden G1 against
  the live JVM. Independent of everything else; could even merge ahead of the
  rest.
- **S2 — relational metadata model** (~250 LOC): D2 + collision errors +
  serde with the lazy supplier. Unit tests incl. equality-ignores-compiler and
  the memoized views-over-views load order.
- **S3 — DDL pass** (~300 LOC): D3 in both builders; delete the fail-closed
  view arm; add the missing direct test for the *remaining* rejection arms
  (§0(a)'s gap) so the next deletion has a sentinel too. Tests: verbatim
  substring (multi-line CTE body, quoted unicode name), prepared-param
  rejection, phase order (view over later table green; function over view
  42F01), duplicate/collision 42F59, per-view lexical resolution.
- **S4 — FROM expansion** (~400 LOC): D4 + error surface. Tests: every
  `views.yamsql` construct as FDB integration tests (star, pushdown,
  projection, view-over-view, CTE bodies, self-join `v AS a, v AS b`), CTE
  shadows view, AT-over-view 42809, EXPLAIN assertions that pushdown reaches
  base-table indexes. Corpus re-pin (P1 movement).
- **S5 — indexes on views** (~200 LOC): D5. Tests: the §2.5 predicated-view
  shape as a Go unit test (index on base table, predicate lowered), aggregate
  view + `legacy_extremum_ever`, `plan_contains` firing assertions, G3 golden.
  Corpus re-pin (P2 movement — the three index-ddl files + arrays-unnesting
  residual).
- **S6 — accounting close-out** (small): ansiledger F031-02/F311-03, DIVERGENCES
  note for issue #3493 parity, residual bookings (§8).

Estimate: ~1.3–1.6k LOC including tests; the largest risk is S4's splice
matching the CTE machinery's alias/correlation discipline, which is why the
self-join and shadowing tests are in-scope for S4, not polish.

Gates: Graefe ACK on this RFC before implementation; one joint implementation
lap at completion; codex one run, generous timeout; ledger/digest updated in
the same commit as each capability, never separately.

---

## 8. Residues (named, booked, not silently dropped)

- **Vector indexes ON views** (`semantic-search*.yamsql`,
  `sliding-window-semantic-search.yamsql`): needs the HNSW DDL path to accept
  a view source. The D5 resolution step is shared; the vector-specific
  generator work is its own item.
- **QUALIFY / row_number window views** (`sliding-window-semantic-search.yamsql:25,:29`):
  the `RowNumberWindowPredicate` index form RFC-202 §10 scoped out; still
  scoped out.
- **`function_view`** (`views.yamsql:31`): green only with RFC-206's CREATE
  FUNCTION; the two RFCs' P1s should be sequenced so whichever lands second
  re-pins `views.yamsql` to fully-passing.
- **CQ-72** (`alias-tests.yamsql`, `gaps.go:81-83`): pre-existing planner
  decline, re-measured in P1.
- **`join_view`** (`views.yamsql:23,:85-95`): commented out upstream — nothing
  to run; noted so nobody "fixes" the corpus.
- **Issue #3493 parity** (functions cannot reference views): reproduced
  deliberately, documented at the phase-order site.
- **No DROP VIEW / standalone / temporary / CHECK OPTION**: §6.7.

---

## 9. Measurements

**MEASURED** (commands run on this worktree / the vendored Java tree):

- Corpus demand: `grep -rlic / -rci "CREATE VIEW"` over
  `third_party/.../yaml-tests/src/test/resources/` — 11 files, counts in §1;
  every construct line quoted from the files (`views.yamsql:23-33,:49-103`,
  `index-ddl.yamsql:76-89`, `index-ddl-values-only.yamsql:87-94`,
  `index-ddl-aggregates-only.yamsql:69-99`, `arrays-unnesting.yamsql:43-44`,
  `alias-tests.yamsql:25-26,:378-525`, `valid-identifiers.yamsql:70-74`,
  `semantic-search.yamsql:25-32`, `sliding-window-semantic-search.yamsql:25-29`,
  `window-function-documentation-queries.yamsql:25`). Version gates read from
  the files.
- Java spec: every §2 claim spot-checked against the vendored source —
  `record_metadata.proto:212,223-226`, `RelationalParser.g4:93-95,171-175,245-247`,
  `DdlVisitor.java:395-439,540-571` read directly in this session; the
  remaining citations from a thorough read of `RecordLayerView.java`,
  `RecordLayerSchemaTemplate.java`, `RecordMetadataDeserializer.java`,
  `RoutineParser.java`, `LogicalOperator.java`, `SemanticAnalyzer.java`,
  `OnSourceIndexGenerator.java`, `DdlStatementParsingTest.java`.
- Go baseline: `rejectUnsupportedTemplateClauses` absent from master, present
  at `origin/feat/rfc202-value-index-as-select:pkg/relational/core/embedded/ddl.go:210-236`
  (shown via `git show` in-session); master's silent-drop two-pass shape read
  at `ddl.go:120-190`; `metadata_proto.go` zero `Views` hits
  (`grep -c 'Views' pkg/recordlayer/metadata_proto.go` → `0`, against
  `gen/record_metadata.pb.go:960` where the field is parsed and `:1184` where
  `PView` is declared — this is what makes §0(c) a data-loss bug and not a
  missing feature); `parser.ParseView` at `parser.go:118-142`; stubs at
  `schema_template.go:152-158`; error constants at
  `errcode.go:83,87,92,95,114`; Java SQLSTATEs at
  `ErrorCode.java:113` (SYNTAX_ERROR), `:117`, `:122`, `:125`, `:147`.

**INFERRED** (stated as design, to be proven by the phase gates):

- That the CTE splice machinery generalizes to the view splice without
  quantifier-identity issues (S4's self-join/shadowing tests are the proof
  obligation).
- That #577's generator accepts every P2 view body unchanged (G3 + the three
  files' execution are the proof; the generator's decompose spine makes it
  highly likely but it is not yet demonstrated).
- The LOC estimates.

**NOT measured:** no corpus run was executed in this session (the ledger
numbers for master and #577 are quoted from the respective
`pinned_ledger_test.go` files, not re-derived); no live-JVM golden exists yet —
G1–G3 are specified, not run.
