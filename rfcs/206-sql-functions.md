# RFC-206: SQL-invoked functions — `CREATE FUNCTION` and `CREATE TEMPORARY FUNCTION`

**Status:** DRAFT — awaiting Graefe + Torvalds RFC review (query-engine gate).

**Decision in one sentence:** port Java's SQL-invoked-function machinery 1:1 —
functions are compile-time macro expansions into the Cascades logical graph
(never a runtime call operator), persisted table-valued functions are stored as
raw SQL text in `PRawSqlFunction` inside the schema template's
`RecordMetaDataProto.MetaData` (field 14, the wire hard line), scalar macro
functions as a serialized `Value` tree in `PUserDefinedMacroFunction`, and
temporary functions live exclusively in a transaction-bound schema template
that is never written to storage.

Java reference: tag 4.12.11.0 at `fdb-record-layer/`. All Java citations below
are `file:line` in that tree.

---

## 1. Scope, and why this is in scope at all

CLAUDE.md declares SQL-layer UDFs out of scope "unless a TODO entry calls for
them". A TODO entry now calls for them: **TODO.md item 69.7 — "Phase 4: SQL
functions (`create function`, temporary functions; 44 files), views (11),
enums (3)"** (`TODO.md:13025-13026`), which is RFC-201 §Phase 4
(`rfcs/201-layered-test-corpus.md:165-166`). This RFC takes the functions half
of that item; RFC-207 takes the views half.

Two populations, and they are not the same number. The item's **44** is the
union of every vendored file that touches a function at all (measured:
`grep -ril "create .*function"` over the corpus → 44 of 238; 11 declare
`create function`, 33 use temporary functions). The **24** this RFC works from
is the currently-*classified* population — the files where a function is the
winning skip cause: `unsupported:temporary-function` = 17, pinned at
`pkg/relational/conformance/javacorpus/pinned_ledger_test.go:50`, plus the 7 of
§2.1. The other 20 are masked by earlier-winning classes (struct,
value-index-as-select) and surface as those classes shrink. Note that
`unsupported-DDL:function` itself appears in **no** pinned ledger string today:
it is a MASKED class (`corpus_run_test.go:283-284`), so the 7 is a
post-unmasking projection, not a pinned count — Phase 0's dependency note is
where that unmasking happens.

Item 69.6's ruling applies verbatim to 69.7: this is
query-engine + DDL scale and requires its own RFC and a Graefe gate before
implementation. This document is that RFC.

In scope:
- Persisted table-valued SQL functions declared inside `CREATE SCHEMA TEMPLATE`
  (`... CREATE FUNCTION f(IN a BIGINT) AS SELECT ...`), invocation in FROM with
  positional, named (`a => 103`), defaulted, and absent (`SELECT * FROM f5`)
  arguments.
- `CREATE [OR REPLACE] TEMPORARY FUNCTION ... ON COMMIT DROP FUNCTION AS ...`
  and `DROP TEMPORARY FUNCTION [IF EXISTS]` as top-level statements, plus the
  yamsql runner's `setup:` / `setupReference:` / `transaction_setups:` surface.
- Scalar macro functions (`CREATE FUNCTION z(IN x TYPE ST3) RETURNS BIGINT AS
  x.v.z`) — the `AS <fullId>` body form, exactly as far as Java implements it.
- Wire: reading, writing, and round-trip-preserving
  `MetaData.user_defined_functions` (field 14).

Out of scope (each with its own reason, not a deferral of function work):
- `CREATE VIEW` (`PView`, field 15). RFC-201 books views as a separate 11-file
  class; `views.yamsql` is inside our 7 only because the classifier's text scan
  matches `create function` first (`runner.go:458-461`). Function support makes
  that file re-classify honestly, which is the correct outcome for this RFC.
- `RETURN <expr>` scalar bodies. Java's own visitor fails them with "scalar
  functions are not implemented" (`DdlVisitor.java:680-683`). Conformance
  principle: doesn't work in Java ⇒ doesn't work in Go, same way.
- `LANGUAGE JAVA` / non-SQL parameter style / `RETURNS NULL ON NULL INPUT` —
  Java rejects all three, but not uniformly, and Go must reproduce the
  asymmetry: parameter style at `DdlVisitor.java:635` and `LANGUAGE JAVA` at
  `:637-638` are `UNSUPPORTED_OPERATION` and live **only** on the
  table-function branch (the scalar branch, `:617-632`, checks neither);
  `RETURNS NULL ON NULL INPUT` is rejected earlier at `:611-613` through an
  `Assert` overload carrying no `ErrorCode`, i.e. `INTERNAL_ERROR`, not
  `UNSUPPORTED_OPERATION`.
- `__ROW_VERSION` pseudo-column and `resultMetadata:` — orthogonal blockers
  inside 3 of the 24 files, already tracked by their own classes.

## 2. Corpus demand (measured by replaying the runner's classification)

### 2.1 `unsupported-DDL:function` — 7 files

All under `third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/`:

| file | constructs | invocation shape |
|---|---|---|
| `sql-functions.yamsql` | positional + named params, `DEFAULT` values, function-calls-function (f3→f1,f2; f4→f3), expression args | TVF in FROM; named args `f1(a => 103, b => 'b')`; parenthesis-less `FROM f5`; inside CTE; correlated `EXISTS (SELECT * FROM f2(t2.z))`; self-join |
| `views.yamsql` | one TVF + `CREATE VIEW` in five flavours | view over TVF (needs views; re-classifies after this RFC) |
| `case-sensitivity.yamsql` | case-differing names `f1`/`F1`; body calls sibling with named args | TVF in FROM under `CASE_SENSITIVE_IDENTIFIERS` |
| `documentation-queries/udf-documentation-queries.yamsql` | named params, defaults, nested TVFs | FROM; join of two TVF instances; correlated arg in EXISTS |
| `versions-tests.yamsql` | TVF projecting `"__ROW_VERSION"`; `(t3.*) as r` nested-record column | FROM (also needs row-version pseudo-column) |
| `versions-with-single-type-tests.yamsql` | single `t1_v(in x bigint)` TVF | FROM (also needs row-version pseudo-column) |
| `join-with-order-by-tests.yamsql` | param without `IN` keyword; `t1.*` + alias projection | FROM, joined, with ordering assertions (also row-version) |

The corpus's **scalar** functions (`RETURNS <type> AS <fullId>`) all take struct
parameters, so their 4 files (`user-defined-macro-function-tests`,
`in-predicate`, `valid-identifiers`, `orderby`) are booked
`unsupported-DDL:struct` today — struct wins in the classifier
(`runner.go:458-459`). They re-block on functions the moment structs land,
which is why Phase 4 below exists even though it moves no file by itself.

### 2.2 `unsupported:temporary-function` — 17 files

Detection is per-config, purely syntactic: any `setup:`/`setupReference:` query
config is skipped (`testblock.go:334-336`) because Java restricts those to
`CREATE TEMPORARY FUNCTION` (`ledger.go:97-99`). The 17: `alias-tests.yamsql`
(41 setups, parameterised temp TVFs, CTEs, self-joins, `resultMetadata`),
`transactions-tests.yamsql`, `setup-with-connection-options.yamsql`,
`include-block/includes/include-scoped.yamsql`,
`include-block/shouldPass/include-with-txn-setup-included.yamsql`, and 12 files
under `transaction-setup/` (`shouldPass/`: single-setup,
single-setup-reference, double-setup, double-reference, setup-and-reference,
reference-and-setup, multiple-transaction-setups,
duplicate-setup-reference-name, double-query-metrics,
transaction-setup-after-setup; `shouldFail/`: unsupported-setup,
unsupported-setup-reference — the negatives that pin "only CREATE TEMPORARY
FUNCTION is permitted in a setup").

Representative (`transaction-setup/shouldPass/single-setup.yamsql:32-37`):

```yaml
    -
      - query: select * from t1 where id > 10;
      - setup: create temporary function t1() on commit drop function AS
               SELECT * FROM table_t1 where id < 50;
      - result: [{id: 30, col1: 40}]
```

Note the semantics this pins: the temp function **shadows a base table name for
the duration of one transaction**, definitions are re-issued per test with
different bodies under the same name, and with continuations the setup runs
only on the initial query (`transactions-tests.yamsql:45-47`).

### 2.3 Feature axes → files

1. Table-valued `CREATE FUNCTION ... AS <select>` invoked in FROM — all 24.
2. Parameter variants (`IN`/bare, `DEFAULT`, named/positional/mixed-absent
   call sites) — pervasive.
3. Transaction-scoped temporary functions + runner `setup` surface — the 17.
4. Function composition (body calls another function) — 4 files.
5. Correlated TVF arguments (`FROM f(outer.col)` under EXISTS) — 2 files.
6. Scalar macro functions — 4 struct-blocked files (Phase 4 insurance).
7. No `WITH RECURSIVE` interplay exists anywhere in the 24 — recursion lives in
   `recursive-cte.yamsql` under an unrelated class. Nothing to design for.

## 3. Java's architecture (the spec)

Two function kinds share only a name and a proto oneof:

**Grammar** (`fdb-relational-core/src/main/antlr/RelationalParser.g4`):
persisted functions are *only* schema-template clauses (`templateClause:92-95`);
temporary functions are top-level DDL (`createTempFunction:237-239`,
`dropTempFunction:241-243`). `functionSpecification:257-262` carries name,
`sqlParameterDeclarationList` (`:264-266` → `sqlParameterDeclarations:268-270` →
`sqlParameterDeclaration:272-274`: `parameterMode? name? type
(DEFAULT expr)?`), optional `returnsClause`, `routineCharacteristics`.
`routineBody` (`:331-336`) has three arms (`:332-335`): `AS queryTerm` (table function),
`AS fullId` (scalar macro), `sqlReturnStatement` (unimplemented). Invocation:
`tableSourceItem ... #tableValuedFunction` (`:486`), named args via
`key => value` (`:1220`), scalar calls via
`userDefinedScalarFunctionCall` (`:1006`).

**DDL compilation** (`fdb-relational-core/.../query/visitors/DdlVisitor.java`):
schema-template functions are collected and compiled **in lexical order**
against the metadata built so far (`:404-429`) — which is also why recursion is
structurally impossible for persisted functions: a self/forward reference fails
as `UNDEFINED_FUNCTION`. `getInvokedRoutineMetadata` (`:489-536`) builds a
`RecordLayerInvokedRoutine`: name, raw-SQL `description` (the source substring,
re-prefixed `"CREATE "` for persisted functions, `:503-505`), `temporary` flag,
a lazily-memoized compiler, and a *serializable form*. Dispatch (`:520-535`):
scalar → eager `UserDefinedMacroFunction` (itself serializable); table-valued
persisted → eager `CompiledSqlFunction`, serialized as `RawSqlFunction`;
table-valued temporary → **deferred** compilation (lazy provider), serialized
form `RawSqlFunction` (never used — temp routines are skipped by the
serializer). The compiler (`visitSqlInvokedFunction:599-668`) pushes a plan
fragment with a fake quantifier over the parameter row so body references
resolve (`:653-655`), then captures the body `RelationalExpression` and its
`Literals`. Literal processing is disabled for persisted bodies, enabled for
temporary ones (`:656-663`).

**Storage / wire** (`record_metadata.proto`): `MetaData.user_defined_functions
= 14` (`:211`); `PUserDefinedFunction` oneof (`:216-221`) =
`PUserDefinedMacroFunction` (`record_query_plan.proto:294-298`: `function_name`,
`repeated PValue arguments` — the parameter `QuantifiedObjectValue`s — and
`body` as a serialized `PValue` tree) |
`PRawSqlFunction {name, definition}` (`:192-195`). `RawSqlFunction.toProto`
(`cascades/RawSqlFunction.java:44-50`) writes the SQL text;
`CompiledSqlFunction.toProto` deliberately **throws**
(`query/functions/CompiledSqlFunction.java:93-95`) — a compiled body is never
persisted. The relational serializer skips temporary routines entirely
(`metadata/serde/RecordMetadataSerializer.java:92-98`). Deserialization
re-parses the stored text lazily through a throwaway visitor
(`RecordMetadataDeserializer.java:109-118,150-153`,
`RoutineParser.java:50-94`); `QueryParser.parseFunction` consumes the leading
`CREATE` token (`QueryParser.java:160-166`) — that is why the prefix is stored.
**SQL text, not a plan, is the compatibility surface for table functions.**

**Temporary lifetime**: `CreateTemporaryFunctionConstantAction.execute`
(`recordlayer/ddl/CreateTemporaryFunctionConstantAction.java:52-66`) copies the
schema template, adds the routine, and binds it to the transaction
(`txn.setBoundSchemaTemplate`), physically the FDBRecordContext session map
(`RecordContextTransaction.java:86-101`). Statements read
`getBoundSchemaTemplateMaybe().orElse(connTemplate)`
(`EmbeddedRelationalStatement.java:66`). Commit or rollback discards the
context ⇒ `ON COMMIT DROP FUNCTION`. Duplicate ⇒ `DUPLICATE_FUNCTION`, raised
at exactly one site — `CreateTemporaryFunctionConstantAction.java:57-60`, the
`OR REPLACE`-absent guard — and **not** inside the template builder, whose
`replaceInvokedRoutine` (`RecordLayerSchemaTemplate.java:492-500`) overwrites a
same-named temporary silently and raises only `INVALID_FUNCTION_DEFINITION`
("attempt to replace non-temporary invoked routine!", `:495`). Dropping a
non-temporary routine is likewise `INVALID_FUNCTION_DEFINITION`, from
`DropTemporaryFunctionConstantAction.java:52` (absent + `throwIfNotExists` ⇒
`UNDEFINED_FUNCTION`, `:58`). Go must keep the duplicate guard in the constant
action rather than pushing it into the builder, or `OR REPLACE` semantics
invert.

**Invocation = compile-time macro expansion**: a per-query `SqlFunctionCatalog`
registers every routine of the (possibly transaction-bound) template alongside
built-ins (`SqlFunctionCatalogImpl.java:163-172`); lookup is by name only — no
overloading (`UserDefinedFunctionCatalog.java:41,52` — a `Map<String,...>`),
built-in/UDF collision is `INTERNAL_ERROR`, neither found is
`UNDEFINED_FUNCTION` (`SqlFunctionCatalogImpl.java:56-72`). Compilation is lazy
and memoized (`UserDefinedFunctionCatalog.java:59-96`,
`RecordLayerInvokedRoutine.java:67`). At the FROM call site
(`SemanticAnalyzer.resolveTableFunction:1154-1184`),
`CompiledSqlFunction.encapsulate` (`CompiledSqlFunction.java:99-180`) builds a
one-row `SelectExpression` of promoted, default-filled argument values over
`range(0,1]` (`rangeOfOnePlan:193-200`), rebases the stored body graph into
fresh aliases (`References.rebaseGraphs(...,
Memoizer.noMemoization(PlannerStage.INITIAL), ...)`, `:163-168`), and returns
their join. Cascades sees an ordinary nested `SelectExpression`; Java's own
yamsql explains show the call collapsing into a covering index scan with
predicate pushdown (`yaml-tests/.../sql-functions.yamsql:66-92`). Scalar macros
substitute the body `Value` via `translateCorrelations`
(`UserDefinedMacroFunction.java:56-70`). Parenthesis-less `FROM f5` is a
name-resolution fallback after table and view lookup fail
(`LogicalOperator.java:208-215`), legal only when every parameter has a
default. **There is no runtime function-call operator anywhere.**

**Plan cache**: persisted functions are covered by the schema-template version
in `QueryCacheKey` (`query/cache/QueryCacheKey.java:100-127`); temporary
functions by `auxiliaryMetadata` = concatenated normalized descriptions of all
temp routines (`RecordLayerSchemaTemplate.computeTransactionBoundMetadata:
360-362`, fed at `AstNormalizer.java:630-633`), and `AstNormalizer:601-627`
re-parses every temp routine to import its bound literals into the query's
literal context.

## 4. Go current state

- **Grammar and parser: done.** `pkg/relational/core/parser/grammar/
  RelationalParser.g4:237-345` matches Java; generated contexts exist; a tested
  `ParseFunction` entry point mirroring `QueryParser.parseFunction` (leading
  `CREATE` pre-consumed) exists at `pkg/relational/core/parser/parser.go:79-117`.
- **Rejection today**: `CREATE TEMPORARY FUNCTION` falls through
  `execStatement`'s dispatch to "unsupported DDL statement"
  (`pkg/relational/core/embedded/connection.go:586-606`). Template-clause
  functions are **silently dropped** on master
  (`pkg/relational/core/embedded/ddl.go:148-189` visits only
  `TableDefinition()` / `IndexDefinition()`); the RFC-202 branch adds the
  fail-closed `rejectUnsupportedTemplateClauses` gate this RFC deletes.
- **Wire gap (a live divergence, not just a missing feature)**:
  `pkg/recordlayer/metadata_proto.go` neither writes nor reads field 14
  (measured: zero references), and the round-trip reconstructs the proto from
  Go-native state — a Java-authored template carrying UDFs, loaded and re-saved
  by Go, loses them silently. Phase 0 pins and fixes this first.
- **Reusable infra**: the routine API surface already exists, hard-wired empty
  (`pkg/relational/core/metadata/schema_template.go:181-200` —
  `InvokedRoutines`/`FindInvokedRoutine`/`TemporaryInvokedRoutines`/
  `TransactionBoundMetadataAsString`; `pkg/relational/api/metadata.go:82-86`).
  RFC-202's branch contributes `runFromResolutionPostPasses` (the single shared
  DDL/query post-pass sequence) and the "plan a parse subtree against the
  metadata built so far" pattern (`parseAsSelectIndexDefinition`). CTE
  machinery (`pkg/relational/core/embedded/plan_visitor.go:150-265` with
  `cteScopes`/`cteBodies` at `:56-57`, and `logical.LogicalCTE` at
  `pkg/relational/core/query/logical/operators.go:903`) is the
  body-plan-plus-positional-remap analogue for inlining. `values.Replace` / `WalkValue`
  (`pkg/recordlayer/query/plan/cascades/values/replace.go:19`) are the scalar
  substitution primitives. The single bare-ID call-site gate to extend is
  `walkUserDefinedScalarFunction` (`pkg/relational/core/query/expr/
  walk.go:950-978`); parallel gates that must not pre-reject UDF calls:
  `extractFunctionNameFromCall` / `findUnsupportedFunctionInSelectQuery`
  (`cascades_generator.go:5473-5555`), `embedded/scalar_functions.go:55-110`.

## 5. Design

1:1 with §3. Components (Go names follow existing conventions):

**5.1 `InvokedRoutine` metadata.** `pkg/relational/core/metadata` gains
`RecordLayerInvokedRoutine{Name, Description, NormalizedDescription, Temporary,
compile func() (UserDefinedFunction, error) /* memoized */, serializable
UserDefinedFunction}`. `RecordLayerSchemaTemplate` stores a routine map and
implements the four stubbed methods for real.
`TransactionBoundMetadataAsString` concatenates temp-routine normalized
descriptions (Java `:360-362`) and joins the plan-cache key exactly where
Java's `auxiliaryMetadata` does.

**5.2 DDL visitor.** The schema-template path collects `SqlInvokedFunction()`
clauses and compiles them in lexical order against the template built so far —
same two-pass structure `ddl.go` already has for tables/indexes, with the
build-partial-metadata + `ParseFunction`-subtree + shared-post-passes pattern
RFC-202 established. The compiler dispatch mirrors `DdlVisitor.java:520-535`:
scalar → macro function (eager); table-valued → compiled body (eager for
persisted, lazy for temporary); serializable form `RawSqlFunction` for
table-valued, the macro itself for scalar. Persisted functions reject prepared
parameters (Java `:510` ⇒ SYNTAX_ERROR).

**5.3 Compiled body.** `CompiledSqlFunction` holds the body as a planned
logical operator plus its parameter correlation and captured literals. The body
is compiled by pushing a scope whose only source is a synthetic
parameter-row quantifier (Java's fake quantifier, `:653-655`) — in Go terms, a
`semantic.ScopeSource` exposing the parameter names/types, the same mechanism
`cteScopes` uses. Its `toProto` is an explicit error, matching
`CompiledSqlFunction.java:93-95`.

**5.4 Invocation.** The per-query function catalog wraps built-ins + the
template's routines; `resolveTableFunction` runs after table lookup fails
(and, for the parenthesis-less form, after view lookup — `LogicalOperator.java:
208-215`). `encapsulate` is a faithful port: arguments (positional or named,
promoted via the existing promotion machinery, defaults filled) become the
result columns of a one-row select over `range(0,1]`; the body graph is rebased
to fresh aliases; the result is the join `SelectExpression`. Correlated
arguments (`FROM f(outer.col)`) work because the argument select is an
ordinary correlated subgraph — no special case. Scalar macros substitute
parameters into the body `Value` with `values.Replace` under a correlation
translation, the Go `translateCorrelations`. **No runtime call operator, no
parallel pipeline** — after expansion the planner has no idea a function was
ever there, which is precisely why predicate pushdown and index selection need
zero new rules.

**5.5 Temporary functions.** `execStatement` gains `CreateTempFunction` /
`DropTempFunction` arms producing constant actions. The embedded connection's
transaction state gains a bound-schema-template slot
(`getBoundSchemaTemplateMaybe` analogue); create copies the effective template,
`replaceInvokedRoutine` (refusing to replace a non-temporary routine), and
binds it; statements resolve their template through the binding; commit or
rollback clears it. Nothing ever reaches the catalog or the serializer — the
serializer skips `Temporary` routines exactly as Java `:92-98` does. The
javacorpus runner replaces the blanket `SkipTemporaryFunction` arm
(`testblock.go:334-336`) with real execution: run `setup`/resolved
`setupReference` statements at the start of the query's transaction, initial
execution only (not on continuation fetches), inheriting the test block's
connection options.

**5.6 Wire.** `metadata_proto.go` `ToProto`/`FromProto` gain field 14:
`PRawSqlFunction` round-trips as `{name, definition}` text;
`PUserDefinedMacroFunction` round-trips through the existing PValue
serialization (the Value-tree registry Go already has for plan continuations).
Loading re-parses raw-SQL routines lazily via `ParseFunction` — text in,
recompile on first use, matching `RecordMetadataDeserializer.java:150-153`.

**Byte-golden strategy** (the D11 pattern, `rfcs/202-value-index-as-select.md:
866-905`): the Java conformance server's `createSchemaTemplatePersistentJava`
already persists arbitrary schema-template DDL as stored
`RecordMetaDataProto.MetaData` bytes into the shared catalog; Go reads them
back via `SchemaTemplateCatalog().ListTemplates`. The conformance test drives
`CREATE SCHEMA TEMPLATE ... CREATE FUNCTION ...` through both engines and
compares the stored `user_defined_functions` field at the raw proto level
(with the existing proto2-default normalization), both directions: Java-written
template readable and invocable by Go, Go-written template byte-equal on
field 14. For macro functions the comparison is on the serialized `PValue`
body — the same cross-engine PValue discipline continuations already live
under.

**5.7 Error surfaces** (Java's, verbatim where shareable):

| condition | error |
|---|---|
| unknown function / arg-count / arg-type mismatch / missing non-default arg | `UNDEFINED_FUNCTION` ("could not find function 'x'" / "could not find function matching the provided arguments") |
| duplicate temp function (no OR REPLACE) | `DUPLICATE_FUNCTION` |
| OR REPLACE over persisted; drop non-temp; duplicate parameter names | `INVALID_FUNCTION_DEFINITION` |
| duplicate named argument | `SYNTAX_ERROR` |
| mixed named+positional args; `LANGUAGE JAVA`; non-SQL parameter style | `UNSUPPORTED_OPERATION` |
| prepared params in persisted function | `SYNTAX_ERROR` |
| built-in/UDF name collision | `INTERNAL_ERROR` |
| name collision with table/view at template build | `INVALID_SCHEMA_TEMPLATE` |

## 6. Phasing — acceptance is corpus-file movement, measured not estimated

Every phase ends with the pinned ledger re-run; the numbers below are the
*expected* movement and each phase's checkbox is the *measured* movement.

**Phase 0 — wire preservation (prerequisite, small).** Field 14 read/write in
`metadata_proto.go`; regression pinning today's silent drop (Java-authored
proto with UDFs survives a Go load→save byte-identically); D11-pattern
cross-engine golden for `PRawSqlFunction`. No corpus movement; this is the
hard line and it goes first so no later phase can ship a lossy round-trip.
Dependency note: RFC-202 (`feat/rfc202-value-index-as-select`) must land
before Phase 1 — it contributes `runFromResolutionPostPasses`, the fail-closed
template-clause gate we convert, and unmasking of `unsupported-DDL:function`
(today masked because every function-declaring template also declares an
AS-SELECT index, `corpus_run_test.go:283-285`).

**Phase 1 — persisted table-valued functions.** §5.1-5.4 + §5.6 for
`PRawSqlFunction`: template-clause compilation, catalog, FROM invocation with
positional/named/default/absent args, composition, correlated args,
parenthesis-less form, error surfaces. Expected movement: `sql-functions`,
`udf-documentation-queries`, `versions-with-single-type-tests` toward pass
(the latter minus row-version stanzas), `case-sensitivity` (case-sensitive
lookup rides the existing identifier machinery), `views.yamsql` re-classifies
to a views class, `versions-tests`/`join-with-order-by-tests` re-classify to
the row-version gap. yamsql `plan_contains`-style EXPLAIN assertions prove the
expansion collapses into index scans (the optimization fires; correct rows via
a slower path is not done).

**Phase 2 — temporary functions + runner setup surface.** §5.5. Expected
movement: all 17 `unsupported:temporary-function` files execute; the two
`shouldFail` negatives assert the "only CREATE TEMPORARY FUNCTION in setup"
rejection. Residual skips inside `alias-tests` (`resultMetadata`) stay booked
under their own classes. FDB integration tests pin transaction scoping:
invisible after commit, invisible after rollback, shadowing a table name,
re-definition across transactions, no catalog write (assert the stored
template bytes unchanged).

**Phase 3 — plan-cache correctness.** Temp-routine `auxiliaryMetadata` in the
cache key + literal import (Java `AstNormalizer:601-633`); regression: same
query text, different temp-function bodies in two transactions ⇒ different
plans, no stale hit. (If Go's plan cache does not yet key on auxiliary
metadata, this phase is where it gains the field — not optional; a stale hit
is a wrong-rows bug.)

**Phase 4 — scalar macro functions.** `UserDefinedMacroFunction` port +
`PUserDefinedMacroFunction` wire golden + the `walkUserDefinedScalarFunction`
catalog hook. Moves no file until structs land (all corpus scalars take struct
params), and exists so the struct work doesn't re-open this RFC; the corpus
entry is a Go-authored yamsql scenario until then, plus cross-engine goldens
for `self(x)`-style non-struct scalars Java accepts.

## 7. Rejected alternatives

- **A runtime function-call operator / plan node.** Java has none; it would be
  a parallel pipeline with its own pushdown, ordering, and continuation story.
  Macro expansion gets every existing rule for free. Rejected on the
  no-parallel-pipelines principle alone.
- **Persisting compiled bodies (plan/Value trees) for table functions.** Java
  deliberately throws on serializing a compiled function; text-in/recompile-on
  -read is the compatibility surface. Persisting plans would freeze planner
  internals into stored templates and diverge from every Java-written
  template on disk.
- **A global or per-connection function registry** (like
  `recordlayer.RegisterFunction`). Functions are schema-template metadata;
  a process-global map cannot express per-template visibility, versioning, or
  transaction-bound temporaries, and diverges from `SqlFunctionCatalog`'s
  per-query construction.
- **Overloading / arity-based resolution.** Java's catalog is name-keyed with
  a single candidate; adding overloading would be a Go-only extension on the
  *shared* surface, violating conformance (same query must fail the same way).
- **Implementing `RETURN <expr>` scalar bodies as a Go extension.** Tempting
  (grammar already parses it), but it is squarely on the shared surface where
  Java errors; the conformance principle wins. Revisit only as a sanctioned
  read-side extension with its own deep coverage if the owner asks.
- **Doing views in this RFC.** Separate 11-file class, separate proto field,
  zero shared compilation machinery beyond what Phase 1 builds anyway;
  bundling would couple two review gates for no corpus gain.

## 8. Effort

| phase | size | risk |
|---|---|---|
| 0 — wire field 14 + goldens | ~300 LOC + conformance test | low; mechanical, JVM available locally |
| 1 — persisted TVFs | ~2-3k LOC (metadata, visitor, catalog, encapsulate, tests) | main risk: alias-rebase correctness in `encapsulate` and scope plumbing for parameter resolution; mitigated by porting Java's exact expansion and pinning with EXPLAIN goldens + cross-engine differential |
| 2 — temporaries + runner | ~1-1.5k LOC | moderate; touches connection/transaction state; corpus gives 17 files of acceptance |
| 3 — plan cache | ~200-400 LOC | low once the key field exists; the regression is the point |
| 4 — scalar macros | ~500-800 LOC | low; blocked-value only until structs |

Review cadence per CLAUDE.md: Graefe + Torvalds ACK this RFC before
implementation; one joint implementation lap per phase-completion; codex on
the full span.
