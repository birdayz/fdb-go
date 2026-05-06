# TODOs

Authoritative priority list for fdb-record-layer-go. Strict precedence: **CRITICAL > HIGH > MEDIUM > LOW**. Pick work from the highest unchecked bucket. Shift handover follow-ups are context, not priority — see CLAUDE.md "Priority discipline at shift start".

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

Restructured 2026-04-25 (swingshift-50). Previous structure: `git show 4cc93ccb:TODO.md`.

---

## Execution roadmap (read this first)

The bucketed sections below list work by priority but not by **sequencing**. This roadmap is the dependency-ordered execution path. A worker should do **the next unchecked item in this list whose prerequisites are satisfied** rather than picking by gut feel.

Update the checkboxes as items land. When an item lists multiple parallel tracks, finish the gating track first; the parallel-track items can run in any order once their gate clears.

### Track A — Cross-language SQL conformance (CRITICAL, unblocked, biggest leverage)

The fastest route to closing the "yamsql is the only oracle" hole. Each item below depends on the previous one in the same track.

- [x] **A1 — Phase B SqlSteps**: COMPLETE swingshift-52. Foundation, runSql + runWithSetup, generic cross-engine harness (Go embedded ↔ Java fdb-relational), **170-entry corpus** with strict byte-equivalent assertions on column metadata + row values, plus `ExpectErrorContains` negative-test framework. All entries pass. Adding a new test case is just `{Name, SchemaTemplate, SetupSqls, Query[, ExpectErrorContains]}` — no baseline RowSet to capture, no test wiring. Detail:
  - **Java side** (`conformance/sql_plan_steps.java`): `runSql(clusterFile, schemaTemplate, sql)` and `runWithSetup(clusterFile, schemaTemplate, setupSqls[], querySql)` steps. Both share `runWithEphemeralSchema` (DDL on `__SYS?schema=CATALOG`, query on `<dbPath>?schema=<schemaName>`). `resultSetToJson` encoder covers Number / Boolean / String / byte[] (base64) / NULL; unknown types render as `{"__unsupported__": "<class>"}`.
  - **Go side** (`pkg/relational/conformance/plandiff/`): `Runner` / `SetupRunner` interfaces, `RowSet` / `Column` / `RunResult` / `RunDiff` / `RunReport` types, `javaRunner` HTTP plumbing, `RunCorpus` + `RunCorpusWithSetup` for batch comparison, shared `invokeStep` helper. Per-entry `Expected RowSet` in `SeedRunCorpus` so failure diagnostics pinpoint exactly which entry diverged.
  - **Long-standing bugs fixed along the way**: (a) `setSchema()` on JDBC wrapper doesn't propagate to `EmbeddedRelationalConnection`; must use `?schema=NAME` URL query string. (b) DDL requires `?schema=CATALOG` on `/__SYS`. (c) `setAPIVersion` race with sibling tests caught and tolerated. (d) Strict end-to-end assertions instead of "typed Java error acceptable" early-returns (planSql tests had been silently passing through this path since first written).
  - **CLAUDE.md** gained "Java↔Go conformance gotchas" section: schema-URL, DDL-CATALOG, NOT-NULL-ARRAY, BYTES literal `X'...'`, `blob` reserved word, INTEGER/FLOAT no-promotion, GROUP BY/LIMIT unsupported, etc.
  - **Corpus coverage**: 19 entries — single-row BIGINT, multi-row STRING, NULL, empty, BOOLEAN, DOUBLE, ORDER BY DESC, WHERE filter, multi-column PK, projection expressions, INNER JOIN, COUNT(*), LIKE pattern, IN list, AND/OR predicates, IS [NOT] NULL, range comparisons, math in projection. INTEGER / FLOAT / BYTES round-trip tests as separate end-to-end specs.
  - **Go runner lands swingshift-52** (`pkg/relational/conformance/plandiff/go_runner.go`): `goSQLRunner` drives the in-process embedded engine via the `fdbsql` driver — per-call ephemeral schema lifecycle (CREATE SCHEMA TEMPLATE / DATABASE / SCHEMA on `/__SYS`, query on `<dbPath>?schema=<schemaName>`). 30/31 entries pass `_Values` strict assertion; 1 entry (`uuid_round_trip`) hits the UUID-column-type DDL gap and is `t.Skipf`'d via `isGoFeatureGap`.
  - **Go-engine conformance gaps surfaced AND closed by the harness in the same shift** (swingshift-52): identifier-case (`id` → `ID`), qualifier stripping (`t.id` → `ID`), anonymous-projection naming (`COUNT(*)` → `_0`), JDBC type-name plumbing (BIGINT/STRING/DOUBLE/etc.), empty-result type inference, **full UUID end-to-end** (DDL + proto sub-message + CAST + INSERT + SELECT + JDBC `OTHER` type). The harness is strict end-to-end (column metadata + row values), **all 31 entries pass byte-equivalence with Java**.
  - **Remaining for full A1 close**: A2/A3/A4 — yamsql equivalence + catalog round-trip + INFORMATION_SCHEMA — all build on top of the wired-up runner.
- [x] **A2 — Catalog wire format Go↔Java round-trip**: DONE dayshift-54. Both layers shipped:
  - **FDBMetaDataStore wire format (nightshift-53, PR #119)**: 11 cross-language specs covering records, VALUE/COUNT/SUM/MAX_EVER index types, split records, multi-record-type stores. Bug fix in `normalizeSubspaceKey` for `[]byte` subspace keys. Three CLAUDE.md gotchas (ExtensionRegistry, DynamicMessage, GroupingKeyExpression).
  - **SchemaTemplateCatalog wire format (dayshift-54, PR #120)**: end-to-end Java JDBC `CREATE SCHEMA TEMPLATE` → Go's `OpenRecordLayerStoreCatalog().LoadSchemaTemplate` round-trip. Fixed four onion layers: keyspace prefix verified matching, catalog metadata-version alignment (`SetVersion(1)` in `BuildCatalogMetaData`), FileDescriptorProto type-name absolutization (`absolutizeFieldTypeNames`), and union-descriptor name + field-naming-convention support (RecordTypeUnion + `TypeName_N`). Test: `conformance/schema_template_catalog_conformance_test.go`.

  **Future** (not gating, mechanical follow-on): Reverse direction (Go writes → Java reads via standard JDBC). The FDBMetaDataStore wire format covers most of the byte-format guarantees; SchemaTemplateCatalog reverse direction would close the symmetry. The Go sqldriver's three-string subspace divergence is a separate sqldriver-specific concern.

  Original A2 spec (kept for reference): Eleven new specs in `metadata_store_conformance_test.go` under "Cross-language functional round-trip (A2)":
  1. Java loads Go-written metadata + scans Go-written records
  2. Java scans Go-built VALUE index (`Order$price`)
  3. Go scans Java-built VALUE index (reverse direction)
  4. Java reads Go-written split records (>100KB, `splitLongRecords=true`)
  5. Java scans multi-record-type store (Orders + Customers) — pins union-descriptor type-tag dispatch
  6. Java scans Go-built COUNT index (BY_GROUP) — atomic-mutation maintainer + atomic-counter wire format
  7. Go loads Java-written metadata + scans Java-written records (reverse direction)
  8. Java scans Go-built SUM index (BY_GROUP, ungrouped) — atomic-ADD payload encoding
  9. Go scans Java-built COUNT index (reverse direction)
  10. Go scans Java-built SUM index (reverse direction)
  11. Java scans Go-built MAX_EVER_LONG index — third atomic op (BYTE_MAX vs ADD/+1)

  Surfaced three new gotchas, all documented in CLAUDE.md: (a) `parseFrom` MUST use `EXTENSION_REGISTRY` (otherwise `[record].usage=UNION` extension is dropped, build raises "Union descriptor is required"); (b) Java-loaded RecordMetaData produces `DynamicMessage` so `Order.newBuilder().mergeFrom(rec.getRecord())` fails with "can only merge messages of the same type" — round-trip via raw bytes; (c) Java's `IndexTypes.COUNT` requires `GroupingKeyExpression(field, 0)` not `field.groupBy(empty())`.

  Plus a fuzz-found bug fix: `RecordMetaDataBuilder.Build` panicked on `[]byte` subspace keys (slices unhashable as map keys); fix in `normalizeSubspaceKey` adds `case []byte: return string(k)` and `case tuple.Tuple: return fmt.Sprintf("%v", k)` for defensive coverage. Regression seed `6b2b0fc91474e6b9` checked in.
- [~] **A3 — SQL semantic equivalence on yamsql corpus**: feed the 1587 yamsql statements through both engines via A1; require identical result sets for read queries. **Progress through nightshift-61: 94 cross-engine scenarios** (was 43 dayshift-55+swingshift-56, +24 nightshift-60, +16 nightshift-61). Each test runs through BOTH the Java fdb-relational executor and the Go embedded engine (via plandiff's `NewJavaRunnerHTTP` + `NewGoSQLSetupRunner`) and asserts (a) Java vs scenario expected, (b) Go vs scenario expected, (c) Java rows == Go rows directly. **nightshift-61 conformance milestones**: 7 Go-permissive divergences aligned with Java (simple-CASE, bit-shift `<<` `>>`, MIN/MAX over non-numeric, HAVING-empty-input, NULLIF, CROSS JOIN, all DISTINCT-aggregates) — every one with structural alignment (visitor-doesn't-implement → Go falls through to default unsupported arm) plus verbatim Java error messages where the message can be cleanly shared. 4 cross-engine `ExpectErrorMessage` proofs (bitshift_left_rejected, bitshift_right_rejected, nullif_rejected, min_over_string_rejected) that assert Go's `api.Error.Message` literally equals Java's `JavaError.Message`. Per-entry Java-server isolation in the corpus harness for negative entries (eliminates fdb-relational planner-state-leak from stalling the run). 6 deferred Go-permissive divergences documented (col IN (SELECT ...), FROM-less SELECT, LIMIT N, WITH RECURSIVE non-self-ref, WHERE bare-paren boolean, ORDER BY <alias>) — all need ~14-test cleanup shifts. Yamsql_test exposed (was hidden by `tags = ["manual"]` in BUILD.bazel; 12 testdata files updated to align with the nightshift-60 ORDER BY rejection). **Plus the real fdbsql-driver leak fix surfaced and fixed swingshift-56**: process-global cache for FDB databases by cluster_file path. Pre-fix suite took ~12 min; post-fix ~45 s. CLAUDE.md gotchas: 7 RESOLVED, 2 RETRACTED post-nightshift-61. Scenario expansion is mechanical follow-on.
- [ ] **A4 — System-table contents byte-equivalence**: `SELECT * FROM INFORMATION_SCHEMA.TABLES` byte-identical from Go and Java against the same store. Subset / specific shape exercised by A3. **Blocker (found swingshift-52):** fdb-relational 4.11.1.0 has NO `INFORMATION_SCHEMA` support — its SQL parser rejects the schema-qualified reference. Our Go side already implements SCHEMATA / TABLES / COLUMNS (see `pkg/relational/core/embedded/system_tables.go`). A4 needs the upstream Java side to gain INFORMATION_SCHEMA before any cross-engine byte-equivalence is possible; Go-side enforcement unilaterally would create divergence the wrong direction.

### Track B — Cascades planner port (HIGH, longer fuse, sequencing strict per RFC-022)

The path from today's seeded `cascades/values/` + matchers to a real Cascades planner. Each phase strictly depends on the previous. Per RFC-022 §"sequencing", do NOT attempt 4.0+ until 4.-1 (the plan-equivalence harness) is shipped — that's already done in plandiff Phase 1+2+3.

- [~] **B0 — Phase 4.0 Type hierarchy**: seed landed nightshift-50, follow-ons COMPLETE swingshift-52. Now in `cascades/values/type.go` (~1350 LOC):
  - Structured types: RecordType, ArrayType (with IsErased), EnumType, RelationType (always non-nullable, IsErased) ✅
  - Primitive singletons: NullableX/NotNullX for every primitive (incl. UUID + VERSION), plus NullType, UnknownType, NoneType (always non-nullable, panics on nullable=true), AnyType (always nullable, panics on nullable=false) ✅
  - Promotion lattice: IsPromotable (PROMOTION_MAP), MaximumType (binary common-supertype with full structural recursion: ARRAY, RECORD, ENUM, RELATION), MaximumTypeOfMany (variadic fold) ✅
  - TypeRepository (named-type registry) ✅
  - Legacy bridge: FromValueType / ToValueType ✅
  - Shape predicates: IsNull / IsNone / IsAny / IsUnresolved / IsArray / IsRecord / IsEnum / IsUuid / IsRelation (mirror Java's default methods) ✅
  - **Value RichType() migration: complete** — every Value impl in `cascades/values/` (12 of them) implements the Typed interface. ValueRichType free function dispatches via Typed; the 80-line type-switch retired. Future Value impls in this package MUST implement Typed.
  - **Remaining**: plan-serialisation hooks (proto encoding for plan-cache) — gated on B7 cache-key spec. When `cascades/values/type.go` exceeds ~1500 LOC (currently ~1350), split into `cascades/typing/` per RFC-025. The legacy ValueType enum + Type() method remain — they're still used at many call sites — but the new code reaches for RichType() directly.
- [~] **B1 — Phase 4.1 RelationalExpression**: substantial seed shipped dayshift-58. Foundation types (RelationalExpression interface + Quantifier + Reference + AliasMap + permutation-aware SemanticEquals) + 11 of the 12 enumerated concretes (8 logical: Filter/Projection/Sort/TypeFilter/Distinct/Union/Intersection/Select; 3 DML: Insert/Update/Delete; 1 leaf: FullUnorderedScan) + correlation walking through expressions. **Remaining**: TableFunctionExpression (gated on StreamingValue port). **Originally sized 4–6 shifts; landed in 1.** Originally listed gates: B3 / B4 / B5 — those tracks unblocked.
- [ ] **B2 — Phase 4.2 matcher catalogue completion**: substantially done nightshift-50 + OptionalIfPresentMatcher swingshift-52 (`SomeElementsMatcher`, `AtLeastElementsMatcher`, `EmptyCollectionMatcher`, generic `TypedMatcher`, `NegationMatcher`, generic `SatisfyingMatcher`, `OptionalIfPresentMatcher`, `CollectionMatcher` interface). **Remaining**: `PartialMatchMatchers`, `graph/` matchers — both gated on B3 (Reference / DAG infra). Run in parallel with B1.
- [~] **B3 — Phase 4.3 Memo & references**: `Reference` (= Cascades "group"), implicit DAG via `Reference` pointers, `PlanContext`, `CascadesRuleCall`. Gates B4, B5, the partial-match matchers in B2. **Seeds shipped dayshift-58:** `Reference` (multi-member equivalence class with two-tier dedup — pointer-identity fast path + SemanticEquals fallback gated on hash equality, in expressions/), PlanContext + PlannerConfiguration + MatchCandidate + EmptyPlanContext (cascades root), ExpressionRuleCall (parallel to Predicate/Value RuleCall — Yield inserts into a Reference), FixpointApply with sub-Reference descent (rules compose across Quantifier boundaries). **Remaining**: full Memo machinery (cross-Reference equivalence-class merging, partial-match propagation, cost-driven extraction), planner phases / yieldExploratoryExpression / yieldFinal distinctions. Sized 1–2 shifts remaining (down from 2–3 with the descent + dedup-fallback in place).
- [ ] **B4 — Phase 4.4 Cost model**: `CascadesCostModel` heuristic comparator matching Java; cardinality estimation hooks; `properties/` package (~25 classes). Per RFC-024, hash-identical Java compatibility is NOT a goal — free to ship simpler Go-native cost. Sized 4–5 shifts.
- [ ] **B5 — Phase 4.5 Rules**: rule batches per yamsql feature (JOIN, CTE, aggregate). 69 rules total in three batches:
  - **Batch A** (port FIRST per RFC-022): `PrimaryScanRule`, `ImplementFilterRule`, `ImplementSortRule`, `MergeFetchIntoCoveringIndexRule`, index-equality + index-range rules, `InComparisonToExplodeRule`. Covers swingshift-44's existing 11-branch pushdown chain. Triggers RFC-025's Phase 2 `cascades/rules/` sub-package split. Sized 3–4 shifts.
  - **Batch B+C** (rest of data-access + implementation + decomposition + finalization rules). ~58 rules. Sized 6–8 shifts.
- [ ] **B6 — Phase 4.6 Planner driver**: `CascadesPlanner` task stack (EXPLORE → OPTIMIZE), `PlannerEvent` debug hooks, integration with `RecordMetaData` + index availability. Sized 3 shifts.
- [ ] **B7 — Phase 4.7 Correctness tests**: port enough of Java's planner test suite to validate rule-by-rule equivalence; extend the 4.-1 harness as rules land. Cross-cuts every Bx item — write tests as the rule lands, not after. Pin per-query plan-equivalence as the rule batches converge.
- [ ] **B8 — plandiff Phase 4 plan-cache-key diff**: today the harness only hashes the rendered tree text. Once B4 cost model + B7 cache-key spec land, diff the plan-cache key directly (RFC-024-aligned Go-internal key; Java-hash compatibility NOT required). Sized 1–2 shifts. **THIS IS THE GATE on the original RFC-022 §4.-1 Phase 4 question.**

### Track C — Phase 5 query execution (HIGH, sequencing strict)

Once the planner produces a `RecordQueryPlan`, the engine needs to run it against an `FDBRecordStore`. Strictly serialised within Track C; parallel to Track B5+ (don't need rules to start).

- [~] **C1 — `PlanGenerator`**: `LogicalOperator → RelationalExpression` adapter. Bridges today's text-based logical builder to the new RelationalExpression hierarchy. **dayshift-58 ships 8 operator types + simple-literal lowering**: `pkg/relational/core/query/plangen/Convert(op)` covers LogicalScan / LogicalFilter (with QueryPredicate) / LogicalUnion (incl. UNION DISTINCT wrapper) / LogicalDelete / LogicalInsert (Source non-nil) / LogicalProject / LogicalSort / LogicalUpdate. Project / Sort / Update each accept simple scalar forms in their text fields via `lowerSimpleScalarText`: bare column → FieldValue, signed integer → ConstantValue(int64), simple float (`d.d` form) → ConstantValue(float64), TRUE/FALSE/NULL → ConstantValue(bool) / NullValue, single-quoted string (no escape) → ConstantValue(string). `FuzzConvert` pins the no-panic invariant (88M execs/60s clean) over random trees; `FuzzConvertAndOptimise` (146M execs/120s clean) pins the C1→B5 pipeline. **Remaining**: full text→Value parsing for arithmetic / function calls / qualified refs / exponent-form numerics / apostrophe-escape strings (gated on threading the SQL expression parser through), LogicalLimit needs a RelationalExpression equivalent, LogicalAggregate needs GroupByExpression port, LogicalJoin needs SelectExpression-with-multi-Quantifiers + predicate placement, LogicalValues / CTE / DDL have no equivalent. Sized 1 shift remaining.
- [ ] **C2 — `QueryExecutor`**: executes a `RecordQueryPlan` against a `FDBRecordStore`, returns `RecordCursor`. Sized 2 shifts. Gated on C1 + B6.
- [ ] **C3 — `RecordLayerResultSet`**: wraps the cursor, implements `api.ResultSet`. Sized 1 shift. Gated on C2.
- [ ] **C4 — Continuation support**: cursor continuation → SQL-level cursor state; match Java encoding. Sized 1 shift. Gated on C3.
- [ ] **C5 — Prepared parameter binding via cascades.Value.Evaluate**: `PreparedParams` substitutes `?` at runtime evaluation time (replaces today's textual `substituteParams`). Unblocks the deferred `ParameterBinder caller plumbing` MEDIUM. Sized 1 shift. Gated on B0 type inference.

### Track D — DDL + cache + driver completion (HIGH/MEDIUM, parallel to B/C)

- [ ] **D1 — Phase 6 DDL action types**: individual `CreateTableAction`, `CreateIndexAction`, `DropTableAction`, `DropIndexAction`, `SetStoreStateAction`, etc. Sized 2 shifts.
- [ ] **D2 — DDL types DATE / TIMESTAMP / ARRAY / JSON**: today's CREATE TABLE accepts BIGINT / INTEGER / DOUBLE / FLOAT / STRING / BYTES / BOOLEAN. Java has all of these. Gated on B0 (TypeDate / TypeTimestamp need the Type hierarchy port to complete). Sized 2 shifts.
- [ ] **D3 — Online indexer integration via DDL**: CREATE INDEX triggers a background build via the existing `OnlineIndexer`. Sized 1 shift. Gated on D1.
- [ ] **D4 — Phase 7 Plan cache**: port `RelationalPlanCache` (3-tier with per-tier TTL + max-entries), `QueryCacheKey` (SQL + param types + catalog version), `PhysicalPlanEquivalence`, async eviction. Gated on B7 cache-key spec. Sized 3 shifts.
- [ ] **D5 — Phase 8 driver adapter remaining gaps**: `Stmt`, `Rows` column-type interfaces, `Tx`, custom scanner/valuer (Struct/Array/Versionstamp/Continuation), integration test matrix. Sized 2 shifts. Most of Phase 8 is already shipped.

### Track E — Cross-language perf + security (HIGH/MEDIUM)

- [ ] **E1 — FRL perf comparison Go vs Java SQL**: once C2 lands enough to run a real SELECT, stand up the same comparison harness for SQL workloads (simple SELECT, secondary-index, INSERT, aggregate, prepared statement). Mirrors the record-layer numbers in CLAUDE.md. Gated on C2.
- [ ] **E2 — ANTLR parser DoS hardening**: 4-min FuzzParse run (swingshift-35) found a 3.4KB unclosed-parens input that takes ~8.7s. Same grammar as Java so the vulnerability exists upstream too. Real fix likely needs grammar tweaks or a parse-time limit in BOTH Go and Java. **Action**: file upstream ticket first, then coordinate Go-side fix to match Java's. NOT a Go-only fix.

### Track F — Architectural follow-ups (MEDIUM, can run any time)

These are bounded clean-ups that don't gate anything but make future work easier.

- [ ] **F1 — Mutual recursion in CTEs**: `WITH RECURSIVE a AS (..., b ...), b AS (..., a ...)`. Today CTEs evaluated in declaration order; mutual refs bail.
- [ ] **F2 — Continuation resumption of partial recursive results**: for `maxRows: 1` pagination. Java's `RecursiveStateManager` uses TempTable cursors. Gated on C4.
- [ ] **F3 — Intermingled schema type-filter wrapper**: re-enable PK pushdown on `FieldKeyExpression` PKs (intermingled tables) by wrapping the narrowed cursor with a type-filtering predicate. Blocker: `INTERMINGLE_TABLES` exists in lexer/parser grammar but isn't wired through DDL.
- [ ] **F4 — `MetaDataEvolutionValidator` wiring**: exists in `recordlayer` but not wired into `SaveSchemaTemplate` / `CreateTemplate`. Java's equivalent flow also skips. Adding Go-side validation unilaterally would reject evolutions Java accepts → divergence. Needs upstream discussion before Go-side enforcement.
- [ ] **F5 — RFC-025 Phase 2 cascades/rules/ split**: triggered when the FIRST B5 Batch A rule (PrimaryScanRule / ImplementFilterRule / etc.) is ready to commit. Mechanical move per RFC-025.
- [ ] **F6 — RFC-025 Leak closure**: extract `ExpressionFolder` (done), `ExpressionResolver` (done), and move `NewExplainOnlyGenerator` to `pkg/relational/core/query/`. Last item is architectural-tax-only — defer until a real fake-engine test needs the indirection.
- [ ] **F7 — RFC-025 Test ports**: port priority-1 + priority-2 Java test classes (ArithmeticValueTest, BooleanValueTest, CastValueTest in values; QueryPredicateTest, ConstantFoldingTest in predicates). Existing coverage is functionally equivalent; the formal port is mostly about cross-shift discoverability. Cosmetic.

### Track G — Cascades cleanup (MEDIUM, single-shift each)

- [x] **G1 — Retire legacy `ValueType` enum** — DONE swingshift-52 (commits b37b6b0a + 96f11e96). Every Value impl's `Type()` now returns rich `Type`. Field migration (`Typ ValueType` → `Typ Type`) on all 8 Value structs + Target on Cast/Promote. The legacy `TypeBool/TypeInt/TypeString/TypeFloat/TypeUnknown` constants kept their names but are now `Type`-typed `var`s aliased to the canonical singletons (`NullableBoolean`/`NullableLong`/`NullableString`/`NullableDouble`/`UnknownType`) — every existing `Typ: values.TypeInt` call site continues to compile unchanged. Bridge functions (`FromValueType`/`ToValueType`/`ValueRichType`) deleted. `RichType()` method + `Typed` interface removed; the Value interface's `Type()` is now the single rich-Type accessor. ConstantValue.Type() correctly preserves the NOT-NULL signal for non-nil Value (commit 96f11e96 — without that, every non-NULL literal lost its NOT NULL information at the Type layer).

### Out-of-track / opportunistic

These don't fit a critical path; pick them up only when a related shift is already touching the area.

- Wire `SimplifyValue` into ORDER BY / aggregate / INSERT VALUES Value-tree paths. Gated on C5 (cascades.Value runtime evaluation seam).
- More walker scalar functions (date functions: NOW, CURDATE, YEAR, etc.). Gated on B0 TypeDate / TypeTime.
- GROUP BY (a+b) AS alias — multi-touch; the rewrite path only handles bare-column entries.
- Pure Go FDB Client `C++ ConnectionID dedup` — not needed unless we add server-side functionality.

### How to use this roadmap

1. Find the highest-letter track with an unchecked item whose prerequisites are checked.
2. If multiple tracks have such items, prefer Track A (CRITICAL leverage) over Track B (CRITICAL but longer fuse) over Track C/D/E (HIGH).
3. Within a track, items are listed in dependency order — start at the lowest unchecked number.
4. Mark `[~]` while in progress; `[x]` when done with a brief shift-name + commit-hash note.
5. The bucketed sections below this roadmap remain authoritative for the **what**; this roadmap is authoritative for the **next**.

---

## CRITICAL

These block the Cascades port (`fdb-relational` Phase 2) or cross-language SQL compatibility verification. Per RFC-022, 4.-1 must land before 4.0 rule porting, otherwise we burn shifts on rules we'd later need to redo when plan divergence shows up. Per the dayshift-34 audit, Java↔Go SQL conformance is the single largest unverified surface.

- [~] **Cascades package structure + deep unit tests (architectural prerequisite for Phase 4)** — see `rfcs/025-cascades-package-structure.md` (swingshift-50). Audit + target sub-package layout + test-port priorities documented.
  - [x] **Audit** — current cascades/ flat-package state, cross-package leaks identified (projection_fold's SimplifyValue/Resolver dance, plandiff's NewExplainOnlyGenerator). RFC-025 §"Cross-package leaks identified".
  - [x] **Layout proposal** — Phase 1 splits `values/` + `predicates/` + `matching/` (mirrors Java, ~3K LOC moved). Phase 2 adds `rules/` once Batch A starts landing. Phase 3 placeholders (`expressions/`, `properties/`) created when ported types exist. Things deliberately NOT split: `events/`, `explain/`, `debug/`, `typing/` (until ~300 LOC), `correlation.go` (96 LOC stays in root).
  - [x] **Test-port priorities** — RFC-025 §E. Priority 1: ArithmeticValueTest, BooleanValueTest, CastValueTest (`values/`). Priority 2: QueryPredicateTest, ConstantFoldingTest (`predicates/`). Priority 3: matcher tests (Go-native — Java has no dedicated test in 4.11.1.0). Priority 4: rule-infrastructure tests post-Phase-2.
  - [x] **Phase 1 execution** — done nightshift-50. `cascades/values/`, `cascades/predicates/`, `cascades/matching/` sub-packages live; root cascades retains rule infra (`Simplify`, `DefaultSimplifyRules`, rule structs). Helpers `ToInt64` / `ToFloat64` / `LiteralValue` extracted from comparisons.go into `values/coercion.go` (the move surfaced a layering inversion — `toFloat64` lived in comparisons.go but values.go's scalar functions used it; promoted both helpers + `LiteralValue` to exported `values/` so predicates can still call them). `correlation.go` moved INTO `values/` (not root) since values is its only intra-package consumer (RFC §"Don't split" footnote allowed this). Two tests extracted from sub-packages into root cascades to avoid cycles: `comparison_simplify_test.go` (rule-fire tests over ComparisonConstantSimplifyRule), `predicate_matchers_test.go` (unexported root-cascades predicate-matcher constructors). 39/39 test targets green.
  - [ ] **Phase 2 execution** — `rules/` split, triggered when first Batch A rule (PrimaryScanRule / ImplementFilterRule / etc.) is ready to commit.
  - [ ] **Leak closure** — post-split: extract `ExpressionFolder` interface in `values/`, `ExpressionResolver` interface in `expr/`, move `NewExplainOnlyGenerator` to `pkg/relational/core/query/`. RFC-025 §"Closing the leaks".
  - [ ] **Test ports** — port priority-1 + priority-2 Java test classes after Phase 1 lands. Each Go sub-package gets unit tests sized to run in <1s without conformance/testcontainer infra.

- [~] **4.-1 — Plan-equivalence harness (build FIRST, per RFC-022)**
  - [x] **Phase 1 (b4ecb49c):** Go-side baseline harness shipped at `pkg/relational/conformance/plandiff/`. 26-query SeedCorpus, structural diff, SHA-256 corpus hash pinned at `7f373f382aa17411…`. `embedded.NewExplainOnlyGenerator()` exposed. Plan trees captured via `query.Plan.Explain()`. 9 unit tests + the corpus-level regression hash. Location decision: `pkg/relational/conformance/plandiff/` (Go-only orchestration) — Java side reachable via existing `conformance/` testcontainer infra.
  - [x] **Phase 2 (a9f281e4 + 175990b3):** Java side wired. swingshift-50 bumped fdb-record-layer-core / fdb-relational-api / fdb-relational-core to **4.11.1.0** in MODULE.bazel; the gitignored `fdb-record-layer/` submodule is on the matching tag. `SqlPlanSteps.java` lands the `planSql(clusterFile, schemaTemplate, sql)` step on `conformance_server.java` — registers fdb-relational's `EmbeddedRelationalDriver` lazily on first call, creates a uniquely-named schema template + database + schema per call, runs `EXPLAIN <sql>` via JDBC, returns the PLAN column. `pkg/relational/conformance/plandiff/`'s `javaEngine` is wired to POST to `/invoke` with the right shape (4 unit tests with httptest mock the server). End-to-end conformance test in `conformance/plan_diff_conformance_test.go` drives both engines against shared FDB testcontainer + real Java server — passes 6.1s.
  - [x] **Phase 3 — catalog-aware Go mode:** done nightshift-50. `embedded.NewExplainOnlyGeneratorWithSchema(schemaDDL)` parses a CREATE SCHEMA TEMPLATE body (auto-wrapping bare bodies the way `conformance/sql_plan_steps.java#planSql` does) into an in-memory `RecordLayerSchemaTemplate`, builds a synthetic schema cache, and returns a Generator whose connection routes through `buildLogicalPlanFor*WithCatalog`. plandiff's goEngine now picks the catalog-aware constructor when `Query.SchemaTemplate` is non-empty. Three new tests pin: (1) catalog-aware tree differs from text-only on `WHERE val=5`, (2) bare CREATE TABLE bodies are auto-wrapped, (3) malformed schema DDL surfaces as a goEngine error rather than silently falling through.
  - [ ] **Phase 4 — plan-cache-key diff:** today the harness only hashes the rendered tree text. Once 4.4 cost model + 4.7 cache-key spec land, diff the plan-cache key directly (RFC-024-aligned Go-internal key; Java-hash compatibility NOT required).
- [ ] **Java↔Go SQL conformance harness Phase B** — wire fdb-relational maven deps into Bazel; extend `conformance_server.java` with `SqlSteps` to drive the same SQL through both engines and diff result sets. Single biggest test-coverage improvement; yamsql currently is the only oracle and we KNOW it's incomplete because every probe finds bugs. **Same Maven version blocker as 4.-1 Phase 2 above** — needs the project-level version-bump decision before either can land.
- [ ] **Catalog wire format Go↔Java round-trip** — extract a schema via Go, load with Java, run a SELECT. And the reverse. Would have caught the catalog subspace bug fixed in swingshift-35. (Subset of conformance harness above; pinned separately because of historical scar.)
- [ ] **SQL semantic equivalence** — feed the yamsql execution corpus (1587 statements) through both engines; require identical result sets for read queries. Today only parsing is verified.

### Go-only divergences — keep/remove decision needed

These features are implemented in Go's embedded engine but Java's fdb-relational rejects or lacks them. Each is a **divergence from "Java is the spec"**. User decision needed per item: **keep** (Go ships ahead, propose upstream Java change, document the divergence) or **remove** (align Go to reject, rewrite affected test surface). Items below are listed regardless of whether they have detailed entries elsewhere in this doc — this is the single audit list.

**Already-detailed below in CRITICAL (cross-reference):**

- [ ] **`col IN (SELECT ...)`** — Java parser-NPE; Go implements. ~14 file rewrite. See entry below.
- [ ] **Standalone `SELECT <expr>` (no FROM)** — Java rejects with UnableToPlanException; Go evaluates as 1-row constant. Intertwined with CTE base case. See entry below.
- [ ] **`LIMIT N` clause** — Java rejects (pagination is JDBC-only via `setMaxRows`); Go implements directly. Dozens of LIMIT-using tests affected. See entry below.
- [ ] **`WHERE (bare-paren-boolean-expr)`** — Java rejects with "expected BooleanValue but got RecordConstructorValue"; Go accepts. ~3 yamsql tests. See entry below.
- [ ] **ORDER BY non-natural in-memory sort fallback (JOIN / CTE / UNION paths)** — single-table SELECT path resolved nightshift-60; JOIN/CTE/UNION still have `sort.SliceStable` fallback. Tracked separately because aligned via C2 QueryExecutor port, not a remove decision. See entry below.

**Not yet detailed — investigate:**

- [ ] **`SELECT DISTINCT` (plain projection form)** — fdb-relational rejects DISTINCT in non-trivial cases (planner-NPE / Cascades has no `DistinctRule` for the projection-DISTINCT shape; only the DISTINCT-aggregate path was aligned nightshift-61). Go has the full DISTINCT pipeline at `pkg/relational/core/embedded/select_distinct.go` and the deduping single-table path. ~15+ yamsql + sqldriver files use plain `SELECT DISTINCT`. The DISTINCT-as-pipeline-stage approach also drives the ORDER BY fallback exemption (see ORDER BY entry above). Keep means: ship a Go-side feature with no cross-engine equivalence and a CLAUDE.md gotcha. Remove means: rewrite affected tests to use `GROUP BY` or `SELECT * ... DISTINCT` rejection assertions.
- [ ] **Scalar functions Java's `BaseVisitor` rejects: `UPPER` / `LOWER` / `LENGTH` / `ABS` / `SUBSTRING` / `SUBSTR` / `TRIM` / `LTRIM` / `RTRIM` / `CONCAT` / `||` / `REPLACE` / `LEFT` / `RIGHT` / `POSITION` / `REVERSE` / `CURRENT_TIMESTAMP` / `COALESCE` (in some shapes) / `NULLIF`** — Java's fdb-relational has no visitor entry for these (rejects with "Unsupported operator NAME" or NPE). Go implements them in `pkg/relational/core/functions/scalar*.go` + `pkg/relational/core/embedded/scalar_functions.go`. Each function group is roughly a 9-yamsql-file rewrite if removed. Removal is mechanical: delete the implementation; the default arm in `evalCall` already emits matching error. Pick groups: STRING family (UPPER/LOWER/LENGTH/SUBSTRING/TRIM/CONCAT) ~25 files, ARITHMETIC family (ABS/SQRT/POWER) ~5 files, DATETIME family (CURRENT_TIMESTAMP/NOW) ~3 files, NULL family (COALESCE/NULLIF — NULLIF was aligned nightshift-61).
- [ ] **Date-part functions (Go-only extensions per nightshift-39): `YEAR` / `MONTH` / `DAY` / `HOUR` / `MINUTE` / `SECOND` / `DAYOFWEEK` / `DAYOFYEAR` / `NOW` / `CURDATE`** — Java has no DATE / TIMESTAMP type in v4.11.1.0 at all (see § HIGH "DDL types" — DATE/TIMESTAMP listed as gap). Go implements both the types (in CAST chain) and the date-part functions. Strictly speaking these are gated on Phase 4.0 Type hierarchy reaching DATE / TIMESTAMP; until then they're Go-only. Decision: keep (and have Java port DATE/TIMESTAMP eventually) or remove (until Java has DATE).
- [ ] **`INFORMATION_SCHEMA.SCHEMATA / TABLES / COLUMNS` queries** — fdb-relational's parser rejects schema-qualified `INFORMATION_SCHEMA.X` references (verified swingshift-52). Go's embedded engine implements all three system tables in `pkg/relational/core/embedded/system_tables.go`. **A4 (cross-engine system-table byte-equivalence) is blocked on Java gaining INFORMATION_SCHEMA upstream.** Decision: keep (propose upstream addition, ship Go-only meanwhile) or remove (until upstream lands). Affected: A4 spec; the embedded engine's system-tables tests.
- [ ] **`SUM(CASE WHEN ... THEN <int-literal> ELSE <int-literal> END)` column type** — type-inference divergence (Go reports BIGINT, Java INTEGER). Two-layer fix needed; workaround `case_in_aggregate_bigint_cast` corpus entry uses explicit CAST. See line 169 entry. Listed here for completeness — same "Go is loose" shape, decision is keep-with-cast-workaround OR fix the type-inference layer.

**Closed / retracted (do not re-investigate):**

- ✅ `WITH RECURSIVE` non-self-ref — RESOLVED dayshift-62 (Go now rejects).
- ✅ `UPDATE` on PK column — RESOLVED dayshift-62 (Go now rejects).
- ✅ `UNION` without ALL — RESOLVED dayshift-62 (Go now rejects, see `union.go:54-57`).
- ✅ NULL in IN-list — RESOLVED dayshift-62.
- ✅ Aggregate in WHERE — RESOLVED dayshift-62.
- ✅ Simple-CASE / bit-shift `<<` `>>` / MIN/MAX over non-numeric / HAVING-empty / NULLIF / CROSS JOIN / DISTINCT-aggregates — RESOLVED nightshift-61.
- ❌ IS TRUE / IS FALSE — RETRACTED nightshift-61 (Java accepts after all).
- ❌ Integer overflow in arithmetic — RETRACTED nightshift-61 (Go's `AddInt64Checked` family already throws).
- ❌ `CAST(double AS BIGINT)` rounding — RETRACTED nightshift-61 (Go's `cast.go` already uses `math.Floor(n + 0.5)` matching Java's `Math.round`).

---

- [ ] **`col IN (SELECT ...)` Go-permissive divergence from Java** — Java NPEs (visitor walks `ExpressionsContext` which is null when the IN list comes from a subquery, per CLAUDE.md gotcha "`col IN (SELECT ...)` parser-NPEs in fdb-relational"); Go's embedded engine implements it correctly. Probed nightshift-61 — alignment was prototyped but reverted because rejecting at evaluation time invalidated 9 yamsql scenarios + 5 sqldriver tests (correlated subquery probes, recursive CTE, DML subquery, etc.). Aligning Go to reject is the correct conformance call but needs a coordinated rewrite of the affected test surface (~14 files): each test using `col IN (SELECT ...)` rewritten as JOIN or EXISTS, OR converted to `error_code: "0A000"` rejection assertions. Defer to a dedicated cleanup shift. Until then, IN-subquery is a Go-only feature with no cross-engine corpus entry.

- [ ] **Standalone FROM-less SELECT Go-permissive divergence from Java** — Java rejects standalone `SELECT <expr>` (no FROM clause) with UnableToPlanException (CLAUDE.md gotcha "SELECT <expr> without FROM is unsupported by the planner"); Go's embedded engine accepts it and evaluates as a single-row constant projection. Probed nightshift-61 — alignment was prototyped but reverted because the same parser path also handles **CTE base cases** (`WITH RECURSIVE counter(n) AS (SELECT 1 AS n UNION ALL ...)`) which Java DOES accept. Standalone-rejected-but-CTE-base-accepted requires context-aware parsing — separate parseSelectQuery entry points OR a `inCTEBase` flag threaded through. Defer to a dedicated cleanup shift. Until then, FROM-less SELECT is a Go-only feature.

- [ ] **`LIMIT N` clause Go-permissive divergence from Java** — Java rejects standalone `... LIMIT N` because pagination is JDBC-only via `Statement.setMaxRows` (CLAUDE.md gotcha "LIMIT clause is not supported in SQL"). Go's embedded engine implements LIMIT directly in SQL. Probed nightshift-61 — Java rejection confirmed. Aligning Go to reject would invalidate dozens of yamsql + sqldriver tests that use `LIMIT N` directly. Defer to a dedicated cleanup shift; each affected test rewritten to use `setMaxRows` via `db.Stmt.SetMaxRows()` (or equivalent), OR converted to `error_code: "0A000"` rejection assertions.

- [x] **`WITH RECURSIVE name AS (non-self-referencing-body)` — RESOLVED dayshift-62.** Java rejects with verbatim "condition is not met!"; aligned Go to reject too at parse/dispatch time when `recursiveKeyword && !containsTableRef(body, cteName)`. Cross-engine corpus entries `recursive_cte_non_self_ref_rejected` and `recursive_cte_multi_partial_self_ref_rejected` pin byte-equality.

- [ ] **`WHERE (boolean_expr)` with bare parens Go-permissive divergence from Java** — Java rejects with "expected BooleanValue but got RecordConstructorValue" because its parser treats `(...)` as a record/tuple constructor unless it appears in a context that forces predicate parsing. Go's embedded engine accepts the form. Probed dayshift-62 to clarify scope: Java REJECTS `WHERE (b = TRUE)`, `WHERE (v > 0)`, `WHERE (a = 1 AND b = 1)` (outer-paren forms) but ACCEPTS `WHERE (n > 5) AND (s IS NULL)` (per-operand parens with outer AND), `WHERE NOT (b = TRUE)` (NOT forces predicate context). Aligning Go would need a parser-level analysis distinguishing top-level-paren-around-bare-expression from operand-of-NOT/AND/OR-paren. Sized 1 dedicated cleanup shift; ~3 yamsql tests affected (boolean.yaml, bug_hunt_probes.yaml, pk_pushdown.yaml) plus possibly sqldriver tests.

- [ ] **`SUM(CASE WHEN ... THEN <int-literal> ELSE <int-literal> END)` column-type divergence — Go reports BIGINT, Java reports INTEGER.** Surfaced dayshift-62 by `case_in_aggregate` probe. Java's Cascades planner types small integer literals as INT (fits in int32) and SUM(INT)→INT preserves the type without widening. Go's `inferConstantJDBCType` types all bare integer literals as BIGINT (existing arithmetic-context decision; correct for `WHERE val = 5` but loose for CASE branches), and `aggregateResultJDBCType` for SUM-with-`aggExpr` inherits the inner type, so Go reports BIGINT. The fix is two-layered: (a) align literal-typing to INTEGER when value fits int32 (with explicit BIGINT widening on arithmetic / CAST), AND (b) make SUM(INTEGER)→INTEGER. The fix touches `inferConstantJDBCType`, `aggregateResultJDBCType`, and the runtime SUM accumulator (today int64; fdb-relational keeps INT result via promotion-then-narrow at emit OR via 32-bit accumulator). Defer until aggregate column-type inference is unified with Cascades SUM-overload resolution. Workaround for cross-engine corpus: explicit `CAST(... AS BIGINT)` on CASE branches (see `case_in_aggregate_bigint_cast`). Same divergence applies to `SELECT SUM(1) FROM t` (no corpus entry; would also fail).

- [x] **UPDATE on PK column — RESOLVED dayshift-62.** Aligned `update_delete.go` to reject UPDATE-of-PK with verbatim `record does not exist`. Check fires inside the per-row loop after the NULL→NOT-NULL check, so `SET pk_col = NULL` still surfaces NotNullViolation (more specific). Cross-engine corpus entry `update_pk_column_rejected` pins byte-equality.

- [ ] **`CAST(double-out-of-int-range AS BIGINT)` — Java silently clamps; Go errors.** Surfaced dayshift-62. `CAST(1.0E20 AS BIGINT)`: Java returns `9.223372036854776e+18` (MaxLong-ish, but actually float-rounded — not even MaxLong exactly); Go errors `value out of range for integer`. Java's clamp is silent data corruption — large doubles get pinned to MaxLong without notice. Go's reject is defensive. fdb-relational's behaviour is questionable from a SQL-spec view; the conformance question is "match Java's clamp anyway, or push back upstream?". Defer — needs decision on whether to align Go to Java's data-corrupting behaviour.

<!-- CAST(double AS BIGINT) alignment is NOT needed — `cast.go`'s
DOUBLE_TO_LONG arm already uses `math.Floor(n + 0.5)`, matching Java's
`Math.round`. Initial nightshift-61 probe misread Go's expected (1 in
the test) as Go's actual (which is 2); both engines return 2.
Retracted upon re-reading the cast implementation. -->


<!-- Integer-overflow alignment is NOT needed — `ApplyMathOp` in
pkg/relational/core/functions/arith.go already overflow-checks via
AddInt64Checked/SubInt64Checked/MulInt64Checked, raising
ErrCodeNumericValueOutOfRange (22003). Initial nightshift-61 probe
misread the Java-only error as a Go acceptance; retracted upon
re-reading the Go arith implementation. -->


- [~] **ORDER BY non-natural in-memory sort fallback (Go-permissive divergence from Java)** — surfaced nightshift-60 by `string_compare` cross-engine scenario. fdb-relational's Cascades planner has only `RemoveSortRule` + `PushRequestedOrderingThroughSortRule`; **no `ImplementSortRule` exists** (the only call site of `RecordQuerySortPlan` is the legacy heuristic `RecordQueryPlanner.java:315`, not the SQL stack's Cascades planner). When no inner plan satisfies the requested ordering, the `LogicalSortExpression` has no physical implementation, the Cascades planner produces no `RecordQueryPlan`, and `CascadesPlanner.resultOrFail()` throws `UnableToPlanException`. The rejection is **emergent from rule-set incompleteness**, not an explicit check.
  - [x] **Single-table SELECT path (`select_query_full.go`) — RESOLVED nightshift-60.** Removed `sort.SliceStable` fallback. Gated EVERY PK / secondary-index pushdown branch (equality, range, composite-range, composite-prefix, IN-list, single-value secondary-IN-list) on natural-order satisfiability via `pkOrderingSatisfiesOrderBy` / `indexBranchSatisfiesOrderBy` — branches decline when their emission order can't satisfy ORDER BY, letting the chain fall through. Gated multi-value lazy-chain IN-list / composite-IN-list on `len(orderBy)==0 || allOrderByEquated` (no usable natural order). PK / composite-PK IN-list values pre-sorted to make sub-scan concatenation emit in PK order (matches Java's IN-list-emit-in-key-order). Added `tryIndexScanForOrdering` — full secondary-index scan as the last branch before full-PK fallback, picks an index whose `(idxCols, pkCols)` satisfies ORDER BY (matches Java's "RemoveSortRule fires when an index scan's Ordering satisfies"). Aggregate / DISTINCT exempted (DISTINCT is a Go-only feature; aggregate produces a small result set). **At-most-1-row scans NOT exempted** — Java's RemoveSortRule checks the Ordering property explicitly, and an equality match has `Ordering=()` which doesn't satisfy a non-empty requested ordering; the gates above let those branches decline so the chain falls through to `tryIndexScanForOrdering` (or rejection if no index satisfies). Rejection check at the end of execSelectQueryFull throws `ErrCodeUnsupportedSort` (0AF01) when `!satisfiable && !isAggregate && !sq.distinct`.
  - [ ] **JOIN path (`join.go:561`)** — same `sort.SliceStable` fallback, not addressed. **Note (nightshift-60)**: a static "left.PK natural order only" gate was prototyped but reverted — Java's Cascades planner picks the JOIN-side outer based on cost, so `FROM A, B WHERE A.pk = B.fk ORDER BY B.pk` succeeds in Java by running B as outer (emits in B.pk order) with per-row A lookups. A correct Go-side fix needs either JOIN-side reordering at plan-time or routing through Cascades. Defer to C2 QueryExecutor.
  - [ ] **CTE path (`cte_scan.go:257`)** — same.
  - [ ] **UNION path (`union.go:177`)** — same.
  - **Long-term resolution**: complete Track C2 (QueryExecutor) so the embedded engine drives queries through Cascades; the in-memory sort fallback disappears mechanically across all paths.

---

## HIGH

### Cascades planner port (Phase 4.x)

Per RFC-022, only attempt 4.0+ AFTER 4.-1 lands. Listed here so the work scope is visible.

- [~] **4.0 — Foundation types**
  - [x] `Type` / `TypeRepository` — DONE swingshift-52. `Type` interface (Code + IsNullable + Equals + String); `TypeCode` enum mirroring Java's well-known codes; concrete impls cover **PrimitiveType + RecordType + ArrayType + EnumType + RelationType**; canonical singletons for every primitive (incl. UUID, VERSION, NoneType, AnyType); `WithNullability` helper; `IsPromotable` / `MaximumType` / `MaximumTypeOfMany` promotion lattice with full structural recursion (ARRAY/RECORD/ENUM/RELATION); shape predicates (`IsNull` / `IsArray` / etc.); `TypeRepository` for named-type lookup; `FuzzMaximumType_Properties` pinning symmetry / idempotence / closure. Track G1 (swingshift-52) retired the legacy `ValueType` enum — every Value impl's `Type()` now returns rich `Type` directly; `Typed` interface, `FromValueType` / `ToValueType` / `ValueRichType` bridges all deleted.
  - [~] `Value` hierarchy — dayshift-46 seeded `Value` interface (with `Evaluate`) + 5 concrete types (Constant, Field, Arithmetic, Boolean, Cast). nightshift-48 + dayshift-49 added Promote, Null, Aggregate, QuantifiedObject, RecordConstructor, ParameterValue, ScalarFunctionValue. swingshift-50 added NotValue (Value-layer boolean negation, Kleene 3VL) + overflow-checked `ArithmeticValue.Evaluate`. **swingshift-59 added 15 more**: InOpValue (Value-layer SQL IN with Kleene 3VL, byte-slice-safe equality), EmptyValue (empty record placeholder + canonical singleton), OfTypeValue (runtime type guard; **Java-eval conformant for NULL branch + STRICT primitive TypeCode match; non-primitive cross-type promotion + DynamicMessage record-shape branches gated on infrastructure**), LikeOperatorValue (Value-layer SQL LIKE; **routes through canonical values.LikeMatch shared with predicates.likeMatch — same fuzz-tested matcher, same Java-conformance contract**), VersionValue (FDBRecordVersion extractor for VERSION-aware queries), RecordTypeValue (record-type discriminator extractor for type-filter rules), IndexedValue (index-key placeholder for pattern-matching, non-evaluable), EvaluatesToValue (IS TRUE/FALSE/NULL/NOT NULL predicate Value with NotNullBoolean type — Java-eval-conformant 4-branch switch), DerivedValue (non-evaluable wrapper for plan-rewrite child-tracking), ExistsValue (Value-layer SQL EXISTS for projection / SELECT-list contexts; pairs with predicate-layer ExistsPredicate), ConstantObjectValue (named-constant placeholder via ConstantDeref interface — Java's EvaluationContext.dereferenceConstant capability), SubscriptValue (1-based SQL array subscript; out-of-bounds → NULL per Java spec), ArrayDistinctValue (SQL ARRAY_DISTINCT; bytes.Equal for []byte slice elements), CardinalityValue (SQL CARDINALITY array length), FirstOrDefaultValue (FIRST_OR_DEFAULT(arr, def) — for scalar subquery materialization). Remaining: ~50 of Java's 78 value classes.
  - [~] `QueryPredicate` hierarchy — Constant / And / Or / Not / Value / Comparison ported. **swingshift-59 added ComparisonRange** (predicate-side range type — Empty/Equality/Inequality discriminator + Merge() with the full transition table covering all 8 type-pairs; used by index-pushdown rules). Remaining: `ComparisonRanges` (multi-column composite), `MatchesValue`, `Placeholder`, `PredicateWithValueAndRanges`.
  - [~] `Simplification` — dayshift-49 added `SimplifyValue` (constant-fold over standalone Values). swingshift-50 added `SimplifyPredicateValues` (folds Value operands inside QueryPredicates). Phase 4.6 brings the full `ValueSimplificationRuleSet` and the rule-driven driver retires the seed.
  - [~] `Comparisons` / `Comparison` — 13 operators + `Comparison.Operand` as `Value` + `LiteralValue` / `NewLiteralComparison` helpers + `ParameterValue`-bound variant. Remaining: real binder plumbing through Evaluate (`ParameterBinder` interface seeded but no runtime callers).
  - [~] `Correlated<T>` + `CorrelationIdentifier` — dayshift-46 seeded interface + Named/Unique factories. Concrete `Correlated` impls land as Values gain richer Quantifier references.
- [~] **4.1 — Relational expressions** — `RelationalExpression`, `RelationalExpressionWithChildren`, `RelationalExpressionWithPredicates` + Logical exprs (`LogicalFilterExpression`, `LogicalProjectionExpression`, `LogicalSortExpression`, `LogicalTypeFilterExpression`, `LogicalUnionExpression`, `LogicalDistinctExpression`, `LogicalIntersectionExpression`, `SelectExpression`) + DML (`InsertExpression`, `UpdateExpression`, `DeleteExpression`, `TableFunctionExpression`). **Substantial seed shipped dayshift-58.** New package `pkg/recordlayer/query/plan/cascades/expressions/` with `RelationalExpression` interface (GetResultValue / GetQuantifiers / CanCorrelate / ChildrenAsSet / GetCorrelatedToWithoutChildren / EqualsWithoutChildren / HashCodeWithoutChildren), `Quantifier` (ForEach kind only — Existential / Physical land when needed), `Reference` (single-member memo group seed; full Memo lands in B3), `AliasMap` (bijection with Compose / Equals / GetTarget / GetSource), `SemanticEquals` walker with permutation-aware enumeration for ChildrenAsSet operators (UNION / INTERSECTION / SELECT), 11 concrete expressions: 7 logical (Filter, Projection, Sort, TypeFilter, Distinct, Union, Intersection) + Select + 3 DML (Insert, Update, Delete) + leaf (FullUnorderedScan). Correlation walking through expressions wired via `values.GetCorrelatedToOfValue` / `predicates.GetCorrelatedToOfPredicate` free helpers (don't change the Value/QueryPredicate interfaces — bridge until each impl ports a per-class method). **Remaining**: `TableFunctionExpression` (gated on StreamingValue port), Java's `RelationalExpressionWithChildren` / `RelationalExpressionWithPredicates` interfaces (Go interfaces use composition + `GetPredicates() []QueryPredicate` accessor on the concretes — no marker interface needed), TempTable / RecursiveUnion / Explode / GroupBy expressions (Java has them but they're not in TODO.md's listed scope).
- [~] **4.2 — Matching engine**
  - [~] `BindingMatcher` DSL — dayshift-46 seeded interface + `PlannerBindings` + `MergedWith` + generic `Get[T]` retrieval helper + `AnyValue` + `Instance` + `ArithmeticMatcher` + `AllOfMatcher` + `AnyOfMatcher`. swingshift-50 added `ListMatcher` + `AllElementsMatcher`. nightshift-50 added `SomeElementsMatcher`, `AtLeastElementsMatcher`, `EmptyCollectionMatcher`, generic `TypedMatcher[T, U]`, `CollectionMatcher` interface. swingshift-52 added `OptionalIfPresentMatcher` (Go-idiomatic absence: nil interface or typed-nil pointer). **Remaining (B3-gated):** `PartialMatchMatchers` + `graph/` matchers — need Reference / DAG infra.
  - [x] `PlannerBindings` — dayshift-46.
- [~] **4.3 — Memo & references** — `Reference` (= Cascades "group"), implicit DAG via `Reference` pointers, `PlanContext`, `CascadesRuleCall`. **Seeds shipped dayshift-58:** `Reference` (in expressions/, single-member equivalence class with EqualsWithoutChildren-based dedup on Insert; full multi-member Memo + cost-aware best-plan extraction lands in the bigger B3 piece), `PlanContext` interface + `EmptyPlanContext` singleton + `PlannerConfiguration` struct + `MatchCandidate` placeholder interface (in cascades root), `ExpressionRuleCall` (parallel to the existing Predicate/Value RuleCall — Yield inserts into a Reference, dedup absorbed via Reference.Insert; Yielded() records intent independent of dedup outcome). **Remaining**: full Memo machinery (multi-member equivalence, partial-match propagation through DAG, cost-driven extraction), planner phases / yieldExploratoryExpression / yieldFinal distinctions, MatchCandidate concrete impls (per-index, lands with B5 Batch A index rules).
- [ ] **B5 / B6 follow-on — physical-wrapper cleanup**. Once the proper plan-aware Memo lands (Java-equivalent: RelationalExpression hierarchy includes RecordQueryPlan as a sibling), the cascades-root `physicalScanWrapper` / `physicalFilterWrapper` / `physicalSortWrapper` / `physicalDistinctWrapper` / `physicalTypeFilterWrapper` adapters become redundant. ~300 lines of bridge code + every Implement* rule's wrapper-detection switch can collapse. Track here so the cleanup doesn't get forgotten when the Memo overhaul ships.

- [~] **4.4 — Cost model** — **Seed shipped swingshift-59.** New `pkg/recordlayer/query/plan/cascades/properties/` package: `Cost{Cardinality, CPU}` Go-native heuristic, `EstimateCost(e)` walking the RelationalExpression hierarchy (12 operator arms covering all seed concrete types), `BestRefCost(ref)` with Reference-keyed memoisation (O(N+K) vs un-memoised O(N*K) for wide References sharing inner sub-trees), `CostLess` comparator, `Reference.GetBest(less)` extraction primitive, `ExtractBestPlan(ref)` recursive plan extractor returning a fresh singleton-Reference tree. Tunable constants (LeafScanCardinality / FilterSelectivity / SortCPU / etc.) calibrated to four targets pinned in unit tests: Filter beats Sort, Distinct(scan) beats Distinct(Sort(scan)), Sort(Filter(scan)) beats Filter(Sort(scan)), Intersection bounded by min child. End-to-end integration through Convert + FixpointApply + GetBest pinned in plangen/cost_extraction_test.go (3 tests). Two new fuzz targets: FuzzCostMonotonicity (best-cost is non-increasing across iters; 8.8M execs/20s clean) + FuzzExtractBestPlan_SingletonInvariant (every reachable Reference in extracted tree has exactly 1 member; 12.6M execs/15s clean). Sub-Reference recursion picks first-member rather than best-member to avoid exponential blowup without memoisation; B6's task-stack planner replaces this with proper memoisation. **Remaining**: per-record-type cardinality plumbing (StatisticsProvider via Catalog), CardinalityProperty / OrderingProperty as separate property types (today Cost is monolithic), `properties/` follow-on classes (DistinctProperty / IntervalsProperty / etc., gated on Batch A consumers).
- [~] **4.5 — Rules**
  - [~] Rule base classes (`CascadesRule`, `CascadesRuleCall`) — seeded dayshift-46. dayshift-58 added the parallel ExpressionRule + ExpressionMatcher + FireExpressionRule + ExpressionRuleCall infrastructure for RelationalExpression-shaped rules (cascades root); plus `FixpointApply` as the seed-level multi-rule driver and `DefaultExpressionRules()` curated list.
  - [~] Predicate-simplification rule set — dayshift-49 + earlier shifts shipped `AndFlattenRule`, `OrFlattenRule`, `AndConstantSimplifyRule` (annulment + identity unified), `OrConstantSimplifyRule`, `NotConstantSimplifyRule`, `AndDedupRule`, `OrDedupRule`, `AndAbsorbOrRule`, `OrAbsorbAndRule`, `NotComparisonRewriteRule`, `ComparisonConstantSimplifyRule`. swingshift-50 added `DeMorganRule` + `NormalizationRules()` rule set (separate from `DefaultSimplifyRules` because Java applies De Morgan via `BooleanNormalizer` as a pre-CNF pass) + `ValuePredicateConstantFoldRule` (unwraps `VP(constant)` to `ConstantPredicate`, type-degraded inputs → UNKNOWN). Remaining (mostly Phase 4.6 ValueSimplificationRuleSet): `ConstantFoldingMultiConstraintPredicateRule`, `ConstantFoldingPredicateWithRangesRule`, `IdentityAndRule`/`IdentityOrRule` (already covered by our unified rules), `NormalFormRule`.
  - [~] RelationalExpression-shaped logical-rewrite rule set — **dayshift-58 shipped 31 rules** (8 initial + 11 follow-ons in the same shift): `FilterMergeRule` (collapse nested filters), `FilterDropTruePredicatesRule` (drop TRUE conjuncts), `FilterDedupPredicatesRule` (Filter([P, Q, P], X) → Filter([P, Q], X) — idempotent AND), `PushFilterThroughDistinctRule` / `PushFilterThroughTypeFilterRule` / `PushFilterThroughSortRule` / `PushFilterThroughUnionRule` / `PushFilterThroughIntersectionRule` / `PushFilterThroughProjectionRule` (push or distribute filter under operators that don't reshape rows — fewer rows for the wrapped operator OR more pushdown opportunities for downstream rules), `DistinctMergeRule` (collapse nested DISTINCT), `DistinctOverSortElimRule` (Distinct(Sort(X)) → Distinct(X) — sort wasted by dedup), `DistinctOverUnionDedupRule` (Distinct(Union(A, B, A')) → Distinct(Union(A, B)) — sound only because outer Distinct dedupes the row stream), `PullFilterAboveDistinctRule` (Distinct(Filter(P, X)) → Filter(P, Distinct(X)) — inverse of PushFilterThroughDistinct; both shapes coexist as cost-model alternatives), `TypeFilterMergeRule` (intersect record-type sets), `TypeFilterRedundantOverScanRule` (eliminate type-filter when scan ⊆ filter), `PushTypeFilterBelowFilterRule` (TypeFilter([T], Filter(P, X)) → Filter(P, TypeFilter([T], X)) — inverse direction; both shapes coexist as cost-model alternatives), `UnionMergeRule` (flatten nested UNION ALL), `PullCommonFilterAboveUnionRule` (Union(Filter([P], A), Filter([P], B)) → Filter([P], Union(A, B)) — fires only when ALL children share the same predicate list), `IntersectionMergeRule` (flatten nested INTERSECTION with matching comparison keys), `PullCommonFilterAboveIntersectionRule` (Intersection(Filter([P], A), Filter([P], B), keys=K) → Filter([P], Intersection(A, B, keys=K)) — fires when ALL children share the same predicate list), `NoOpFilterRule` (eliminate empty/all-TRUE filters), `ProjectionMergeRule` (collapse stacked projections — outer wins), `ProjectionElimRule` (eliminate identity SELECT *), `PullFilterAboveProjectionRule` (Projection([cols], Filter(P, X)) → Filter(P, Projection([cols], X)) — inverse of PushFilterThroughProjection), `SortMergeRule` (Sort over Sort → outer Sort), `PullFilterAboveSortRule` (Sort([k], Filter(P, X)) → Filter(P, Sort([k], X)) — inverse of PushFilterThroughSort; both shapes coexist as cost-model alternatives), `SortDedupKeysRule` (Sort([a, b, a]) → Sort([a, b]) — dedup duplicate (Value, Reverse) pairs), `SortConstantKeysElimRule` (Sort([42, 'x']) → inner — all-constant keys mean no ordering refinement), `UnsortedSortElimRule` (eliminate Sort([])), `UnionSingletonElimRule` (Union([Q]) → Q), `IntersectionSingletonElimRule` (Intersection([Q]) → Q). All 31 in `DefaultExpressionRules()` and tested through FixpointApply end-to-end. **dayshift-58 also extended FixpointApply to descend into sub-References** (commit c4123d15) — rules now compose across Quantifier boundaries, so a Filter chain inside a Sort's inner sub-tree gets fully optimised. Previously the rule engine only fired on the top-level Reference's members; sub-trees were unreachable. The push-through-X family is unblocked by the SemanticEquals fallback in `Reference.Insert` (commit 680e664a): fast path stays O(1) for rules that reuse input Quantifiers, fallback walks SemanticEquals only when the fast path misses (catches the case of fresh-Reference wrapping that earlier non-terminated). **Remaining**: index-pushdown rules are Batch A; pushing Filter through Projection where the projection list TRUNCATES the columns the predicate references would need reference-tracking (the seed PushFilterThroughProjection is sound because LogicalProjection currently passes-through rows; the pushdown is unsafe under future column-truncating projection semantics).
  - [~] **Batch A (port FIRST per RFC-022 — covers swingshift-44's existing 11-branch pushdown chain so 4.-1 harness gets end-to-end yamsql coverage)**. **Substantial seed shipped swingshift-59.** New `pkg/recordlayer/query/plan/plans/` subpackage with `RecordQueryPlan` interface + 4 concrete plans (Scan / Filter / Sort / Distinct). Batch A rules ported: `PrimaryScanRule` (FullUnorderedScan → ScanPlan), `ImplementFilterRule` (LogicalFilter → FilterPlan; gated on inner Reference having a physical-plan member), `ImplementSortRule` (LogicalSort → SortPlan; same gating), `ImplementDistinctRule` (LogicalDistinct → DistinctPlan; same gating). Bridges via `physicalScanWrapper` / `physicalFilterWrapper` / `physicalSortWrapper` / `physicalDistinctWrapper` in cascades root — adapt RecordQueryPlan → RelationalExpression so the wrapped plan is a valid Reference member; all 4 implement `properties.WithChildren` for ExtractBestPlan rebuild. End-to-end Planner test: Sort(Filter(Scan)) input + Batch A rules converges to a Reference holding the chained physical plan. `BatchAExpressionRules()` composer + auto-registration in the rule registry. **Remaining**: `MergeFetchIntoCoveringIndexRule`, index-equality + index-range implementation rules, `InComparisonToExplodeRule` — all gated on `IndexAccessHint` / `MatchCandidate` infrastructure ports (B3 follow-on). Wrapper bridges are a SEED workaround; proper Memo plan-aware membership replaces them in a follow-up shift.
  - [ ] **Batch B and beyond** — rest of data access rules (`AbstractDataAccessRule`, `AggregateDataAccessRule`), implementation rules (`ImplementDistinctRule`, `ImplementNestedLoopJoinRule`, `ImplementRecursiveDfsJoinRule`, `ImplementStreamingAggregationRule`…), decomposition (`DecorrelateValuesRule`), optimization (`PushPredicateThroughDistinctRule`, `MergeFetchIntoTypeFilterRule`…), finalization (`FinalizeExpressionsRule`). ~69 rules total. Port in batches aligned to yamsql feature flags (JOIN, CTE, aggregate).
- [~] **4.6 — Planner driver** — **Seed shipped swingshift-59.** `cascades.Planner` with task-stack EXPLORE phase + Plan() convenience EXPLORE→OPTIMIZE entry point. ExploreReferenceTask / ExploreExpressionTask / ApplyRulesTask, bottom-up traversal via LIFO push order, per-Reference saturation tracking (skips ApplyRules when member-count hasn't moved since last fully-saturated pass). PlannerEventHandler exposes diagnostic hooks (OnExploreReference / OnExploreExpression / OnApplyRules). MaxTasks hard cap (default 100k). Confluence with FixpointApply pinned via FuzzPlanner_Confluence (4.2M execs/20s clean post-fix; the fuzz caught a real saturation bug that incorrectly skipped re-fires after growth). Idempotence + initial-member preservation pinned by 2 more fuzz targets. **Real perf win**: BenchmarkPlanner_RealisticTree (2.43ms / 54K allocs) vs BenchmarkOptimise_RealisticTree (FixpointApply: 4.65ms / 99K allocs) — ~48% faster from saturation tracking. **Remaining**: per-rule task granularity (current ApplyRulesTask fires every rule per pass; Java has separate TransformTask / ImplementTask), PlannerEvent integration with RecordMetaData + index-availability lookup (gated on Batch A index rules). FixpointApply remains for legacy callers; removal after Planner is wired into plangen.
- [ ] **4.7 — Correctness tests** — port enough of Java's planner test suite to validate rule-by-rule equivalence; extend the 4.-1 harness as rules land.

### Cross-language conformance & infra

- [ ] **System table contents byte-equivalence** — `SELECT * FROM INFORMATION_SCHEMA.TABLES` returns byte-identical rows from Go and Java against the same store.
- [ ] **FRL perf comparison — Go vs Java SQL** — once Phase 5 lands enough to run a real SELECT, stand up the same comparison harness for SQL workloads (simple SELECT, secondary-index SELECT, INSERT, aggregate, prepared statement). Go-vs-Java table for relational layer mirroring the record-layer numbers in CLAUDE.md.

### Security / robustness

- [ ] **ANTLR parser exponential-time on unclosed parens (DoS)** — 4-min FuzzParse run (swingshift-35) surfaced a 3.4KB `CASE WHEN x IS NULL T((((...` input that takes ~8.7s to parse. Same grammar as Java so the vulnerability exists there too. Corpus entry `a1c9802306691af3` pinned as regression. Real fix likely needs grammar tweaks or a parse-time limit in both Go and Java. Upstream ticket worthwhile before Go-only hardening.

### SQL feature gaps (significant)

- [ ] **DDL types** — `DATE` / `TIMESTAMP` / `ARRAY` / `JSON` column types. Today's `CREATE TABLE` accepts only BIGINT / INTEGER / DOUBLE / FLOAT / STRING / BYTES / BOOLEAN. Java has all of these.
- [x] **DDL + INSERT + SELECT `UUID` column type** — DONE swingshift-52 (full end-to-end). DDL parser hook accepts `key UUID` (`pkg/relational/core/embedded/ddl.go`); metadata builder emits the `tuple_fields.UUID` proto sub-message reference (`pkg/relational/core/metadata/builder.go`); `CAST(string AS UUID)` validates and returns canonical form (`pkg/relational/core/functions/cast.go`); `ConvertToProtoValue` / `ProtoValueToDriver` round-trip the canonical-string ↔ proto-message representation via `most/least_significant_bits` (`pkg/relational/core/functions/proto_value.go`); JDBC type-name reports `OTHER` per Java's `Types.OTHER`. plandiff `uuid_round_trip` corpus entry passes strict equivalence with Java.
- [x] **Result-set metadata: identifier-case + projection naming + qualifier stripping** — DONE swingshift-52. JDBC normalizer at `staticRows.Columns()` uppercases unquoted identifiers, strips qualifiers, and emits synthetic `_N` for anonymous projections. Closes 28 of 30 column-name gaps; combined with the new ColumnTypeDatabaseTypeName driver method (below), all 31 corpus entries match.
- [x] **Embedded driver: `ColumnTypeDatabaseTypeName`** — DONE swingshift-52. `staticRows` carries an optional `colTypes []string` populated from proto FieldDescriptors in the SELECT path; aggregate columns default to BIGINT (covers SUM/MIN/MAX/COUNT). Plandiff Go runner now reads `sql.ColumnType.DatabaseTypeName()` directly instead of inferring from values — DOUBLE columns and empty result sets resolve correctly.
- [~] **EXPLAIN / ANALYZE** — swingshift-50 wired `EXPLAIN <query|insert|update|delete>` through naiveGenerator. Returns a 1-row driver.Rows with column `PLAN` carrying the rendered logical-operator tree (catalog-aware path on warm-cache, text-builder fallback on cold). Matches fdb-relational's PLAN column shape. Remaining: ANALYZE (statistics-aware planning), `EXPLAIN FOR CONNECTION`, `EXPLAIN <continuation>`, plus EXPLAIN format options (FORMAT=JSON / DOT / GML).
- [ ] **Mutual recursion in CTE** — `WITH RECURSIVE a AS (..., b ...), b AS (..., a ...)`. Today CTEs evaluated in declaration order; mutual refs bail.
- [ ] **Continuation resumption of partial recursive results** — for `maxRows: 1` pagination. Java's `RecursiveStateManager` uses TempTable cursors.
- [ ] **Intermingled schema type-filter wrapper** — re-enable PK pushdown on `FieldKeyExpression` PKs (intermingled tables) by wrapping the narrowed cursor with a type-filtering predicate. Blocker: `INTERMINGLE_TABLES` exists in lexer/parser grammar but isn't wired through DDL.
- [ ] **`MetaDataEvolutionValidator` wiring** — exists in `recordlayer` but not wired into `SaveSchemaTemplate` / `CreateTemplate`. swingshift-35 audit: Java's equivalent flow also skips. Adding Go-side validation unilaterally would reject evolutions Java accepts → divergence. Needs upstream discussion before Go-side enforcement.
- [ ] **Plan cache key stability** — Java cache key hash = Go cache key hash. Per RFC-024 decision: NOT a hard goal (Java cache is per-process Caffeine, no wire format), but Go-internal stability + schema-version-sensitive keys still required.

### frl CLI — pre-v1 release blockers

- [x] **Phase A.5 — metadata loading (file + FDB store sources)** — DONE in earlier shifts. `MetadataSource` proto union (meta_file / meta_store_keyspace) lives in `cmd/frl/proto/frl/config/v1/config.proto`. `cmd/frl/internal/meta/meta.go` has `Source` interface + `FileSource` + `FDBStoreSource` + `FromContext` factory + the `buildFromBytes` / `buildFromProto` validators. `pkg/recordlayer/metadata_export.go` ships `WriteRecordMetaData`. The `--meta-file` flag is wired in commands like `frl index describe`. Consumers: `frl store info` (in `cmd/frl/internal/cmd/store.go`) + `frl meta get` (`meta.go:254`).
- [x] **Phase A.6 — operator guide** — DONE. `cmd/frl/docs/operator-guide.md` (251 lines) covers both metadata sources for Go and Java apps.

### Pure Go FDB Client

- [ ] **C++ ConnectionID dedup** — C++ FlowTransport deduplicates bidirectional connections via ConnectionID exchange in ConnectPacket. Not needed as a pure client (we never accept incoming connections). Implement only if we add server-side functionality.

- [~] **Isolated unit tests for big core files** — `transaction.go` (1495 lines, state machine + isolation + conflict detection), `transport/conn.go` (847 lines, framing + multiplexing), `topology.go` (112 lines, topology monitor + connection-error eviction), `readpath.go` (857 lines, get/getRange/Watch), `locality.go` (652 lines, location cache). dayshift-51 added 109 isolated unit tests across these files: `transaction_unit_test.go` (38 — `validateVersionstampOffset`, `keyAfterBytes`, `isSystemKey`, `checkTimeout`, `conflictBufAlloc` pool reuse + growth, conflict-range append/disable/auto-reset, `EnsureMutationCapacity`, `postCommitReset` + `reset` + user-facing `Reset` state machine, `Cancel`/watch-context); `topology_unit2_test.go` (13 — `kickTopology` non-blocking, `applyDBInfo` apply/broadcast, `waitProxiesChanged` channel identity, `handleConnError`, `topologyMonitor` ctx-cancel + kick-burst, concurrent waiters); `transport/conn_unit_test.go` (22 — `extractPingReplyToken` defensive parse, `splitmix64`/`NewUID`/`newConnectionID` RNG sanity, FrameReader truncation/checksum/persistent-header, `WriteFrame` writer-error + 1 MiB body, ConnectPacket short-buffer + length>40 + `ReadConnectPacket`/`WriteConnectPacket` error propagation + full round-trip); `readpath_unit_test.go` (16 — `isWrongShardServer`/`isAllAlternativesFailed`/`isFutureVersionOrProcessBehind` table-driven, `sleepCtx` natural elapse + cancel + already-cancelled, `buildGetValueRequest`/`buildGetKeyValuesRequest` round-trip + lockAware option); `locality_unit2_test.go` (11 — `stripTenantPrefix` 4 cases, `buildGetKeyServerLocationsRequest`/`buildGetKeyServerLocationsRangeRequest` forward + reverse, `collectOverlapping` 4 cases). Surfaced two production tweaks: (a) extracted `applyDBInfo` from `refreshTopology` so the apply/broadcast contract is testable without a live cluster; (b) defensive guard in `tryAllCoordinators` for empty coordinator list (prior fall-through returned `(nil, nil)` which nil-derefed in `dbInfoEqual`). `grv.go` partially closed earlier by swingshift-50 (`grv_test.go` covers the adaptive-refresh math, 7 cases). Sized 2 shifts originally; ~half landed dayshift-51 — remaining: ryw.go (1010 LOC, but already heavily tested + fuzzed), `commitpath.go` request build / parseCommitReply.

- [~] **Multi-shard test matrix** — 24 sub-tests in `multishard_test.go` against ONE 3-process container with `max_shard_bytes=50KB`. **dayshift-58 closes the shard-count axis**: `TestMultiShard_LargerShards` runs a 5-subtest topology-sensitive subset (GetRange / GetRangeWithLimit / GetKey / ClearRange / ContinuationCorrectnessWithCacheInvalidation) against `max_shard_bytes=200KB` (~5 shards from 1MB), parameterised via `shardSizeConfig`. Full 24-subtest at every shard config would 3x container time — subset picks the boundary-correctness surface where shard count matters most. **Remaining**: chaos during rebalancing, continuation-token correctness across shard splits at varied sizes (the latter is partially exercised by ContinuationCorrectnessWithCacheInvalidation but only at one config).

- [x] **Round-trip fuzz: KeyServerLocationsReply + GetKeyValuesReply + remaining nested types** — swingshift-50 added round-trip for SplitRangeRequest/Reply, WaitMetricsRequest, WatchValueRequest (and found the KeyRangeRef single-key bug). dayshift-51 extended with three more in `pkg/fdbgo/wire/types/marshal_fuzz_test.go`: `FuzzKeyRangeRef_SingleKeyOptimization` (forces the (begin, begin+\x00) shape that drives the keyrangeref_custom.go optimization, pinning the swingshift-50 fix; 17M execs/15s clean), `FuzzGetKeyRequest_RoundTrip` (KeySelectorRef + Version + TenantInfo through GetKeyRequest, the smallest wrapper that exposes the read-hot-path selector primitive; 17M execs/15s clean), `FuzzGetKeyServerLocationsReply_RoundTrip` (the diff-oracle skips this type because constructing the deeply-nested `vector<pair<KeyRangeRef, vector<StorageServerInterface>>>` shape on the C++ side is impractical; round-trip catches vtable/slot-index/padding bugs without needing the inner payload to be valid; 24M execs/15s clean). Surfaced one wire-protocol contract worth pinning while writing it: `Arena` fields are deserialization-only memory-ownership markers — C++'s SaveVisitor intentionally OMITS them from precomputeSize and writeToBuffer, so any future generator change that emits Arena into outbound bytes would now break the round-trip assertion immediately.

---

## MEDIUM

### Cascades port — already-seeded follow-ups (defer until 4.-1 lands)

- [ ] **`ParameterBinder` caller plumbing** — eval-context capability seeded (`cascades.ParameterValue.Evaluate` checks for it) but no runtime caller exists. The embedded executor textually substitutes `?` via `substituteParams` BEFORE parsing, so cascades never sees a `ParameterValue` at runtime. A real wire-in needs the embedded executor to evaluate cascades.Value at runtime — bigger than half-shift sized. Unblocks runtime prepared-statement plan-cache reuse.
- [ ] **Wire `SimplifyValue` more places** — swingshift-50 wired projection (proto + map paths) and WHERE predicate-tree Value operands. Other callers: aggregate `aggExpr` / `outExpr`, ORDER BY expression keys, INSERT VALUES expressions.
- [~] **More walker scalar functions** — verified nightshift-50: walker `scalarFunctionResultType` already covers SIGN, MOD, IFNULL, IF/IIF, GREATEST/LEAST, EXP/LN/LOG, REVERSE, POSITION, LEFT, RIGHT (in addition to swingshift-50's batch: ABS, FLOOR, CEIL, CEILING, ROUND, SQRT, POWER, POW, COALESCE, NULLIF, TRIM, LTRIM, RTRIM, CONCAT, SUBSTRING, SUBSTR, REPLACE). Remaining gap: date/time functions (NOW, CURDATE, YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DAYOFWEEK, DAYOFYEAR — `embedded.scalar_functions.go:716-757`). These need TypeDate / TypeTime in the value-type seed; gated on Phase 4.0 Type hierarchy port.
- [~] **Derived-table WHERE in catalog-aware builder** — basic shape landed nightshift-50. `(SELECT col1, col2 FROM realtable) AS x WHERE x.col = ?` now routes through `buildWherePredicateForDerived` → `buildDerivedTableSource` which extracts the inner query's projections via `extractFromQueryTerm` and synthesises a `semantic.StaticTable` whose columns inherit the inner-table types. Computed projections (`SELECT a + 1 AS v FROM ...`), `SELECT *`, joins inside the derived, aggregates, and qualified-star projections all decline cleanly to text fallback. **Remaining (Phase 4.0-gated):** computed projections need real type inference for the projected expression's result type — the current bail will lift once the Type hierarchy lands.
- [ ] **Multi-source scope for JOIN in cascades walker** — buildWherePredicateForJoins handles JOIN; further parity with Java's scope semantics needed as more rules land.

### SQL feature gaps (smaller)

- [x] **`SUM(BIGINT)` accumulator now preserves int64 precision** — DONE nightshift-57. `mapGroupState` / `groupState` carry parallel `sumsI []int64` + `sumIntOnly []bool` accumulators alongside the float64 sum (`pkg/relational/core/embedded/aggregate.go`, `select_query_full.go`). Each `sumIntOnly[i]` starts true and only flips to false when a non-int64 runtime value is observed; on emit, integer-only groups return the int64 accumulator, mixed/float groups fall back to the float64 sum. `SELECT SUM(qty) / COUNT(*) FROM t` now integer-divides on both engines (10/3=3). Cross-engine corpus entry re-enabled in `aggregateExprScenario` (`conformance/yamsql_cross_engine_conformance_test.go`).
- [x] **IN-list via subquery decomposition** — verified nightshift-50 the path is wired end-to-end. `preEvaluateInSubqueries` (in `in_subquery.go`) walks the outer WHERE's AND-chain leaves, executes each `col IN (SELECT ...)` once, and caches the value list keyed by the inner `QueryExpressionBody` in `EmbeddedConnection.inSubqueryCache`. `extractColInList` (in `in_list_pushdown.go:88-103`) consults that cache and treats the cached values as a literal IN-list, driving the existing PK / composite-PK / secondary-index point-scan chain. Remaining gap: subqueries nested under OR / NOT / LIKE / BETWEEN escape the AND-chain walk and stay on the runtime evalPredicate path; correlated subqueries deliberately bail too. Both are by design.
- [ ] **GROUP BY (a+b) AS alias** — expression group keys can't be referenced via alias from the SELECT list because the rewrite path only handles bare-column group-by entries.
- [~] **ORDER BY dedup edge cases** — verified nightshift-50: `ORDER BY b, B` IS case-insensitively deduped today (`select_parser.go:923,943` upper-folds the dedup key). The remaining gap is `ORDER BY t.x, x` qualified-vs-bare — those stay distinct because the dedup is string-keyed and alias resolution happens later. Per inline comment, matches Java's behavior today; rides on a future semantic-analyzer-aware identifier-folding pass.
- [ ] **INFORMATION_SCHEMA case-insensitive TABLE_NAME filter** — works today for exact-case match. Real fix is upstream identifier-normalisation, not a system-tables handler patch. Workaround: `WHERE UPPER(TABLE_NAME) = 'EMP'`.
- [ ] **Parser rejects unquoted `FROM INFORMATION_SCHEMA.TABLES`** — `TABLES` / `COLUMNS` / `INDEXES` / `SCHEMATA` are reserved keywords. Workaround: quote (`FROM "INFORMATION_SCHEMA"."TABLES"`). Proper fix: extend grammar to allow these identifiers in qualified-table position.

### Phase 5 — Query execution

- [ ] **`PlanGenerator`** — `LogicalOperator → RelationalExpression` adapter.
- [ ] **`QueryExecutor`** — executes a `RecordQueryPlan` against a `FDBRecordStore`, returns `RecordCursor`.
- [ ] **`RecordLayerResultSet`** — wraps cursor, implements `api.ResultSet`.
- [ ] **Continuation support** — cursor continuation → SQL-level cursor state; match Java encoding.
- [ ] **Prepared parameter binding** — `PreparedParams` substitutes `?` at evaluation time (replaces today's textual `substituteParams`).

### Phase 6 — DDL completion

- [ ] Individual actions: `CreateTableAction`, `CreateIndexAction`, `DropTableAction`, `DropIndexAction`, `SetStoreStateAction`, etc.
- [ ] Integration with online indexer (CREATE INDEX triggers background build).

### Phase 7 — Plan cache

- [ ] Port `RelationalPlanCache` — 3-tier (primary/secondary/tertiary) with per-tier TTL + max-entries.
- [ ] `QueryCacheKey` — SQL + param types + catalog version.
- [ ] `PhysicalPlanEquivalence` — deduplicates semantically identical plans.
- [ ] Async eviction.

### Phase 8 — `database/sql/driver` adapter

- [ ] **`Stmt`** implementing `driver.Stmt`, `driver.StmtExecContext`, `driver.StmtQueryContext`, `driver.NamedValueChecker`.
- [ ] **`Rows`** implementing `driver.Rows` + `driver.RowsColumnTypeDatabaseTypeName` / `Nullable` / `Length` / `PrecisionScale` / `ScanType`.
- [ ] **`Result`** — `LastInsertId` is always an error (FDB has no auto-inc; match Postgres driver convention).
- [ ] **`Tx`** implementing `driver.Tx`.
- [ ] **Value conversion** — `driver.Value` ⇄ `api.DataType` values, including structs and arrays.
- [ ] **Custom scanner/valuer** — `Struct`, `Array`, `Versionstamp`, `Continuation`.
- [ ] **Integration test matrix** — `db.BeginTx` + Commit/Rollback; context cancellation mid-query; concurrent connections from shared `sql.DB`.

### frl CLI — Phase B.3 writes (deferred pending UX design)

- [ ] `record put --file <file>`
- [ ] `record delete <pk>`
- [ ] `meta set / apply` — deferred, dangerous without dry-run
- [ ] `store create --meta <file>`
- [ ] `store truncate`
- [ ] `store destroy`
- [ ] `store lock --reason <r>` / `store unlock`
- [ ] `index build <name>` — OnlineIndexer run (progress + throttle flags)
- [ ] `index rebuild <name>`
- [ ] `index set-state <name> <state>`
- [ ] `config add-context --name <n> ...` — flag design pending
- [ ] `keyspace ls <path>` / `keyspace tree <path>` — FDB directory layer reads
- [ ] `tx run` — ad-hoc transaction wrapping

### frl CLI — small follow-ups

- [ ] **`frl sql -o json`** output mode for `-c` / `-f` (NDJSON rows). Current output is the styled table only.
- [ ] **`frl sql` tab-completion** on table / column names resolved from the catalog.
- [ ] **`frl sql` BEGIN / COMMIT / ROLLBACK** explicit handling (currently autocommit per statement).
- [ ] **`frl sql` EXPLAIN formatting** when the Cascades planner can surface a plan tree.
- [ ] **Auto-detect relational catalog path** in existing commands: if `meta_file` isn't configured but the catalog subspace has entries, fall back to the relational source instead of erroring with "no metadata source". Low priority — explicit `meta catalog get` is clearer.

### Architecture / refactor (in progress)

- [~] **RFC-021 Phase 1c — exec move + path normalisation** — connection.go shrunk from ~11,880 → ~2,900 lines (75%) by nightshift-45. Still TODO: move from `core/embedded/` to RFC-destined homes under `core/plan/physical/`, `core/query/visitors/`, `core/eval/`, `core/functions/`; collapse proto-vs-map evaluator divergence behind a uniform `Row` interface; extract expression evaluator (`evalExpr` / `evalExprAtom` / `evalScalarFunctionCallCore`, ~1,500 lines remaining) and predicate evaluator (~700 lines).
- [~] **RFC-021 Phase 1d — Session migration** — `SchemaCache`, `CatalogMu`, `CatalogReady`, `StatementTime` moved to Session. Remaining statement-scope state (`ctes`, `scalarSubqueryCache`, `validQualifiers`, `outerScopes`, `currentSourceAliases`) still lives on `EmbeddedConnection` — blocked on Phase 1c finishing the exec* moves before those fields can follow their callers.
- [~] **Break up `evalScalarFunctionCallCore`** (715-line switch) — split by family (`evalStringFns`, `evalMathFns`, `evalDateFns`, `evalCastFn`) via `map[string]FuncImpl` dispatch. Subsumed by RFC-021 Phase 1c.
- [ ] **Typed enums for `joinType` / `aggFunc`** (currently magic strings).

### Performance

- [ ] **Pool proto messages in `deserializeAndDiscover`** — `rt.newMessage()` allocates via reflection per record (77.5MB / 564K allocs in BenchmarkScanRecords, ~9%). BUT: messages escape to user code via `FDBStoredRecord.Record`, so pooling isn't safe without API changes (copy-on-return or explicit release). Only viable if scan API returns copies or users opt-in.
- [ ] **Benchmark pushdown vs full scan** — no direct micro-benchmark. Existing `just bench` doesn't isolate the cursor-pick cost; a targeted bench would make the perf win observable and guard against regressions.

### Testing

- [x] **Fuzz targets in `pkg/relational/`** — DONE (item was stale at TODO restructure time). 11 fuzz targets exist today across `pkg/relational/`: `FuzzParse`, `FuzzParseFunction`, `FuzzParseView` (parser), `FuzzDeserializeTemplate` (catalog), `FuzzMessageTypeFromDescriptor` (metadata), `FuzzApplyMathOp`, `FuzzApplyBitOp`, `FuzzLikePrefixStrinc`, `FuzzLikePatternToPrefix` (embedded), `FuzzNormaliseTree`, `FuzzHashTree` (plandiff). Continuation token / record version fuzz lives at the recordlayer level (`FuzzUnwrapContinuation`, `FuzzCompleteVersionFromBytes`, `FuzzConcatContinuation`, etc.).
- [ ] **Error-path coverage ~0.2% in `pkg/relational/`** (2 error assertions vs 862 success in `embedded_fdb_test.go`). Add tests for type mismatch on INSERT, NOT NULL violation, missing schema, invalid SQL at execute time, duplicate CREATE DATABASE, PK conflict.
- [ ] **Parser tree-shape conformance tests** (stretch) — feed the same SQL corpus through both parsers and diff trees, or pick representative corners. Requires JSON serialiser on both sides. Not a blocker for Phase 2 — semantic analyzer tests catch tree-shape regressions indirectly.

### Infrastructure

- [ ] **Throughput benchmarks fail on single-node testcontainer** — `BenchmarkThroughputInsertBatchConcurrent128` overwhelms FDB testcontainer. Two issues: (1) GRV cache staleness causes "record store does not exist" on first goroutines after setup; fix: `InvalidateGRVCache()` after store creation. (2) FDB 5-second tx timeout under load → "context deadline exceeded". Fix: skip in `just bench` or use larger cluster. `just bench-ci` excludes throughput benchmarks and works fine.

- [ ] **Per-PR binding-stress smoke + runner deps** — first attempt at adding a `just binding-stress 5 100` step to PR CI hit a real env blocker: the Hetzner self-hosted runner is missing Python `foundationdb` package + `libfdb_c.so`. Same blocker breaks the nightly-fuzz `binding-stress` job (silently failing/cancelled for weeks). Fix: provision runner with `pip install foundationdb==<matching version>` AND `apt install foundationdb-clients`, OR have `cmd/fdb-binding-stress` install/extract them on first run. Then re-add the smoke step (5 seeds × 100 ops, ~30s) to ci.yml. Closes a 24h hide-window between nightly runs. Sized 1 shift. From the 2026-04-25 client quality audit.

- [x] **FDB client error-code coverage** — DONE dayshift-58. `pkg/fdbgo/fdb/error.go`'s `errorDescriptions` now mirrors the full C++ `flow/include/flow/error_definitions.h` (343 codes vs the previous ~50). Section comments match the C++ source structure. New `IsRetryable(code int) bool` matches `fdb_error_predicate(RETRYABLE, code)` exactly — 12 canonical retryable codes (1021, 1039 MAYBE_COMMITTED + 1007/1009/1020/1037/1038/1042/1051/1078/1213/1223 RETRYABLE_NOT_COMMITTED). 1031 transaction_timed_out is explicitly NOT retryable. Two latent bugs fixed in the prior map (1004 had wrong description, 2015 had wrong description). 3 unit tests pin retryable/non-retryable/unknown-code/error-string contracts. **Follow-up**: `pkg/fdbgo/wire/reader.go::FDBError.Retryable` has its own retryable list with extra codes (1006, 1200, 1235, 1242) — reconcile with `fdb.IsRetryable` so both error types agree.

### Proto / metadata

- [ ] **Proto definitions** — copy `fdb-relational-*` proto files from Java source into `proto/apple/relational/` (`record_layer_context.proto`, catalog messages). Regenerate via `just generate`.

---

## LOW

### Out of scope (Java doesn't have it OR explicitly deferred)

These are listed for visibility — DO NOT add them. Verified against Java source 2026-04-21.

- ❌ **Window functions** — verified zero `WindowedAggregateValue`/`WindowExpression` in Java's `fdb-record-layer-core`. Grammar accepts them; evaluator errors 0A000 cleanly. Don't add.
- ❌ **INTERSECT / EXCEPT** — Java grammar only has UNION. Don't add.
- ❌ **ANY / ALL with subquery** — verify Java first if revisited.
- ❌ **GROUPING SETS / ROLLUP / CUBE** — verify Java first.
- ❌ **LATERAL joins** — verify Java first.
- ❌ **PIVOT / UNPIVOT** — Oracle/SQL Server idiom; Java unlikely to have.
- ❌ **Date arithmetic** (`DATE_ADD`/`DATE_SUB`/`DATEDIFF`/`EXTRACT`) — verified zero implementations in Java fdb-relational-core. Don't add.
- ❌ **INTERVAL syntax** — Java doesn't have. Don't add.

### Record Layer — out-of-scope query-planner prerequisites

Only used by Java's query planner / SQL layer, not by core CRUD. Defer until Cascades work motivates them.

- [ ] **Synthetic record types** — `JoinedRecordType` (equi-join), `UnnestedRecordType` (repeated message fan-out). Proto fields 12-13 in MetaData. 11+ Java classes. Large feature.
- [ ] **Views** — `PView` in MetaData proto (field 15). SQL layer concept.
- [ ] **User-defined functions** — `PUserDefinedFunction` in MetaData proto (field 14). SQL layer concept.
- [ ] **AggregateCursor** — accumulator-based aggregation over cursor results.
- [ ] **ComparatorCursor** — custom comparator ordering.
- [ ] **UnorderedUnionCursor** — union without order preservation.
- [ ] **MapPipelinedCursor** — async pipelined map (no Go equivalent of CompletableFuture).
- [ ] **ProbableIntersectionCursor** — Bloom filter intersection.
- [ ] **SizeStatisticsGroupingCursor** — key/value size tracking.
- [ ] **RecordCursorVisitor pattern** — cursor tree inspection.

### Pure Go FDB Client — niche features

- [ ] **Tenant groups** — Metacluster-only. `tenantGroupTenantIndex`, `tenantGroupMap`, group cleanup on delete.
- [ ] **Tenant tombstones** — Metacluster data cluster feature. Prevents tenant ID reuse.
- [ ] **Tenant ID prefix** — Multi-cluster ID partitioning (`tenantIdPrefix` shifts prefix into upper 2 bytes of 8-byte ID). Standalone clusters use prefix=0.
- [ ] **Multi-version client** — Plugin loading for older client protocol versions.
- [ ] **FDB status JSON parsing** — Cluster status monitoring via `\xff\xff/status/json`.
- [ ] **Version vector support** — Causal consistency optimization for multi-region deployments.

### Pure Go FDB Client — niche perf

- [ ] **`net.Buffers` (writev)** — scatter-gather I/O for frame writes. Low impact now that write coalescing works.
- [ ] **LRU eviction for location cache** — currently random eviction. Works well at 600K entries.
- [ ] **Pre-allocate prefixed keys** — commit path tenant prefix allocation. Not on read hot path.

### Pure Go FDB Client — quality polish (2026-04-25 audit)

- [ ] **Split `transaction.go` (1495 lines)** — mixes state machine, retry, conflict tracking, options, version handling. Pure refactor into ~400 LOC files makes the next bug easier to find. Sized 1 shift.
- [x] **Document accepted divergences inline** — items #6 (auto-reset), #18 (wrong-shard cap), #19 (GRV refresh, partially closed by swingshift-50) all carry inline comments at the divergent site. #18: `MaxWrongShardRetries` (transaction.go:55) — `C++ is unbounded (relies on tx 5s timeout); 50×10ms = 500ms, generous safety margin`. #19: `nextGRVRefreshDelay` (grv.go:280) — documented by the swingshift-50 port of C++'s `backgroundGrvUpdater` formula. #6: dayshift-51 added a multi-line block comment at `Commit()`'s `postCommitReset` call site (transaction.go) explaining the divergence from C++ NativeAPI (which leaves the tx in committed state) and the rationale (binding-tester contract, Go-idiomatic `db.Run` reuse).
- [x] **Backoff jitter** — `commitDummyTransaction` exponential backoff now has ±10% jitter via `jitterBackoff(d)` helper (commitpath.go, dayshift-51). 1000-sample range test pins the bounds. One `rand.Float64` per retry, per-call independent so concurrent goroutines also desync.
- [x] **`unsafe.Pointer` `[]Mutation`→`[]MutationRef` reinterpret guard** — dayshift-51: replaced the single size-only compile-time assertion with five additional per-field offset and per-field size assertions in commitpath.go, AND added `TestMutationLayout_BitIdenticalRoundTrip` (commitpath_unit_test.go) which serializes 3 mutations through the production unsafe-cast path (Set/ClearRange/AddValue) and asserts every (MutType, Param1, Param2) tuple matches the original (Type, Key, Value). Compile-time pins catch field reorders; runtime pin catches type-swaps that preserve offsets.

### Record Layer — niche

- [ ] **FDBReverseDirectoryCache** — reverse prefix→name caching (~496 lines Java).
- [ ] **KeySpace/KeySpacePath Phase 2/3** — Phase 1 done (core types, path nav, reverse resolution, range queries, 11 tests). Phase 2: `LocatableResolver` + `ScopedDirectoryLayer`. Phase 3: `FDBReverseDirectoryCache`. See `docs/design-keyspace.md`.
- [ ] **Extension options processing** — advanced FDBMetaDataStore feature for proto extension options.
- [ ] **Schema validation cross-language** — needs Java conformance server additions.
- [ ] **AtomKE** — Java interface only; no concrete consumers.

### Future phases (not needed for v1)

- [ ] **Phase 9 — gRPC server + remote driver** — port `fdb-relational-grpc/` proto definitions; `cmd/frl-server` standalone binary with TLS + auth; remote `sqldriver` path: DSN host:port → gRPC client.
- [ ] **Phase 10 — separate CLI** — `cmd/frl` SQL shell with EXPLAIN, formatted output. (Mostly subsumed by `frl sql` Phase E already shipped.)

---

## Recently shipped (last ~5 shifts)

Trimmed history list for context. Older completions trimmed; full history in git log.

### swingshift-59 (2026-04-28)

**Major Cascades port advancement: 4 of RFC-022's buckets** (4.0, 4.4, 4.5, 4.6). 75+ commits. Reviewer LGTM mid-shift; user directive "Java conformance is absolute king" addressed via audits + fixes.

- [x] **B4 cost model (4.4) — full seed**. New `pkg/recordlayer/query/plan/cascades/properties/`: `Cost{Cardinality, CPU}`, `EstimateCost` over the 11 RelationalExpression types, `BestRefCost` with Reference-keyed memoisation, `ExtractBestPlan` recursive plan extractor, `CostLess` comparator, `StatisticsProvider` interface (Default / Fixed / Map impls), `CostHinter` for opaque wrappers, `BestMemberSelector` for planner integration, `WithChildren` interface for opaque-type rebuild support. Memoisation gives O(N+K) vs O(N×K) for wide References sharing inner sub-trees.
- [x] **B6 task-stack planner (4.6) — full seed with Plan() entry**. `cascades.Planner` with EXPLORE + OPTIMIZE phases: `ExploreReferenceTask` / `ExploreExpressionTask` / `ApplyRulesTask` / `OptimizeReferenceTask`, bottom-up traversal via LIFO push order, per-Reference saturation tracking. `Plan()` convenience runs both phases; `BestMember(ref)` accessor exposes per-Reference winners. PlannerEventHandler diagnostic hooks (OnExploreReference / OnExploreExpression / OnApplyRules / OnOptimizeReference). MaxTasks hard cap (default 100k). **Real perf win**: 4.65ms FixpointApply → 2.43ms Planner on RealisticTree (~48% faster from saturation tracking).
- [x] **B5 Batch A — 5 of 6 read-side rules + 3 of 3 DML rules**. New `pkg/recordlayer/query/plan/plans/` subpackage with `RecordQueryPlan` interface + 9 physical plans (Scan / Filter / Sort / Distinct / TypeFilter / Union / Insert / Delete / Update). Read-side rules: PrimaryScanRule, ImplementFilterRule, ImplementSortRule, ImplementDistinctRule, ImplementTypeFilterRule. DML rules: ImplementInsertRule, ImplementDeleteRule, ImplementUpdateRule. **All 8 implement rules** have full 5-wrapper inner-recognition (asymmetry caught by reviewer + fixed). Bridges via 8 physical wrappers — RelationalExpression adapters with WithChildren + CostHinter (write-heavy WriteCPU for DML). `BatchAExpressionRules()` + `DMLImplementationRules()` composers. End-to-end DELETE pipeline test through Convert + Plan(). Remaining: covering-index + index-equality / range rules (gated on MatchCandidate / IndexAccessHint).
- [x] **4.0 Value hierarchy — 17 ports** (totals: ~29 of Java's 78 now). InOpValue (SQL IN, byte-slice-safe equality), EmptyValue, OfTypeValue (**Java-eval conformant** — NULL → IsNullable + STRICT primitive TypeCode match per OfTypeValueTest; non-primitive promotion + DynamicMessage gated), LikeOperatorValue (**routes through canonical values.LikeMatch shared with predicates.likeMatch — same fuzz-tested, Java-conformant matcher**), VersionValue, RecordTypeValue, IndexedValue, EvaluatesToValue (Java-eval conformant), DerivedValue, ExistsValue, ConstantObjectValue (with ConstantDeref interface), SubscriptValue (1-based per SQL spec, OOB → NULL), ArrayDistinctValue, CardinalityValue, FirstOrDefaultValue, ObjectValue, QueriedValue.
- [x] **4.0 QueryPredicate — 2 ports + 1 helper**. ComparisonRange (predicate-side range with 8-transition Merge state machine), ExistsPredicate, IsLeafQueryPredicate helper.
- [x] **6 new fuzz targets**. FuzzCostMonotonicity (8.8M execs/20s), FuzzExtractBestPlan_SingletonInvariant (12.6M execs/15s), FuzzPlanner_Confluence (4.2M execs/20s — caught a real saturation bug at introduction; bug fixed same shift), FuzzPlanner_Idempotence (4.7M execs/10s), FuzzPlanner_InitialMemberPreserved, FuzzPlanner_PlanFullPipeline (7.3M execs/15s).
- [x] **Conformance audits + fixes** (per "Java conformance is absolute king"). LikeOperatorValue regex-vs-likeMatch divergence found + fixed (canonical matcher unified). InOpValue []byte panic + numeric-coercion gap (latter documented). OfTypeValue 2 of 3 Java-eval branches closed. ImplementFilter/Sort/Distinct missing-wrapper-cases asymmetry closed (5-wrapper symmetry across all 5 implement rules). BooleanValue Go/Java naming divergence documented.
- [x] **End-to-end integration tests through plangen.Convert + Planner.Plan + extraction** (~7 tests covering Filter/Sort/Distinct/Scan shapes, stats-driven extraction flipping, full SQL-shape Distinct(Sort(Filter(Scan))) at 4-deep, UNION ALL DML pipeline).
- [x] **10+ new benchmarks**: BenchmarkPlanner_RealisticTree, BenchmarkPlanner_FullPlan, BenchmarkExtractBestPlan_DeepTree / WideAlternatives, BenchmarkBestRefCost / BenchmarkBestRefCost_WideRef, BenchmarkLikeMatch_{Simple,Wildcards,LiteralOnly}, BenchmarkOfTypeValue_Evaluate.
- [x] **Late-shift Value-port batch — 19 ports (totals: ~50 of Java's 76 now, ~66% concrete coverage)**. Plus IndexableAggregate Go interface + GetIndexTypeName accessor on AggregateValue (Java's IndexableAggregateValue marker; Go uses a method on the existing AggregateValue with COUNT_NOT_NULL / COUNT / SUM / MIN_EVER_LONG / MAX_EVER_LONG mapping). Plus 4 metric-specific sugar constructors for DistanceRowNumberValue (Java's class-per-metric naming). Plus FirstOrDefaultStreamingValue (streaming variant of FirstOrDefaultValue — pulls first element of streaming child via RangeValue.EvaluateAsStream). Plus IndexOnlyAggregateValue (compile-time MAX_EVER / MIN_EVER aggregate that MUST be backed by an aggregate index — Java's IndexOnlyAggregateValue with MAX_EVER_LONG / MIN_EVER_LONG operators; implements IndexableAggregate). Plus Values utility helpers (DeconstructRecord, SimplifyAll — Java's Values.java functions). IncarnationValue (store-incarnation leaf, NotNullInt; SQL `get_versionstamp_incarnation()`); PatternForLikeValue (SQL LIKE→regex builder for plan-equivalence with Java; **Go runtime path doesn't consume the regex — our LIKE matcher works directly on SQL pattern via canonical values.LikeMatch**); IndexEntryObjectValue + TupleSource enum (covering-index ordinal-path extraction; KEY/VALUE/OTHER tuple-source discriminator); QuantifiedRecordValue (queried-record flow marker, distinct from QuantifiedObjectValue); WindowedValue base (PartitioningValues + ArgumentValues + SplitNewChildren helper); RankValue (SQL RANK() window function); RowNumberValue (SQL ROW_NUMBER() with HNSW EfSearch + IsReturningVectors config — index-only marker per Java); UdfValue (user-defined function — Go-idiomatic `func(args []any) any` callback instead of Java's abstract-class subclass-per-UDF pattern); ConditionSelectorValue (SQL CASE-WHEN selector, paired with PickValue to lower CASE expressions; strict-TRUE check matching Java's Boolean.TRUE.equals); ToOrderedBytesValue + FromOrderedBytesValue + OrderedBytesDirection enum (DESC index-key encoder/decoder pair, 4 directions ASC/DESC × NULLS_FIRST/NULLS_LAST; Eval is placeholder gated on tuple.PackOrdered/UnpackOrdered port); RangeValue (SQL range(begin, end, step) table function — placeholder Evaluate, EvaluateAsStream materialises finite ranges, Cardinality() returns static count when constants); ArrayConstructorValue (SQL ARRAY[a,b,c] literal — non-nullable Array(ElementType), empty != NULL contract, defensive copy of children); RowNumberHighOrderValue (curried ROW_NUMBER higher-order — carries HNSW config ahead of partition/args, Apply() produces fully-configured RowNumberValue); DistanceValue + DistanceOperator enum (4 vector distance metrics: Euclidean / EuclideanSquare / Cosine / DotProduct — FUNCTIONAL eval over []float64 + []float32 vectors; Java has each metric as a separate class — Go unifies via Operator field for K-NN rule matchability); DistanceRowNumberValue (K-NN ROW_NUMBER + 4 metrics — Java has 4 concrete classes, Go unifies via Metric field, matchable via switch); CollateValue (locale-specific sort-key encoder — placeholder Eval gated on golang.org/x/text/collate wiring; Value-shape reachable for plan equivalence with Java's CollateValue); AndOrValue (Value-layer AND/OR for non-predicate contexts e.g. SELECT a AND b — full Kleene 3VL truth tables, short-circuit on dominant left, NullableBoolean result type).
- [x] **Late-shift implement-rule unit-test batch**. Per-rule dedicated tests added for ImplementUnionRule (4 tests incl. per-child gating, empty-union guard, 3-children scaling), ImplementInsertRule + ImplementDeleteRule + ImplementUpdateRule (6 tests, fire-when-and-only-when contract for the 3 DML rules with target-record-type + transforms preservation), ImplementDistinctRule + ImplementTypeFilterRule (5 tests incl. 5-wrapper-symmetry pinning for Distinct-over-TypeFilter — the asymmetry caught by reviewer mid-shift). Closes the per-rule test gap on the 6 Batch A read-side rules + 3 DML rules — all 9 implement rules now have dedicated unit-test files, not just plangen integration coverage.
- [x] **B5 Batch A 7th read-side rule — ImplementIntersectionRule + RecordQueryIntersectionPlan + physicalIntersectionWrapper**. Closes set-operator implement-rule symmetry: every logical-set operator (Union + Intersection) has a matching implement rule. Per-child gating identical to ImplementUnionRule. comparisonKeyValues from logical Intersection carry through unchanged into physical plan. HintCost: cardinality bounded by SMALLEST child (intersection can't be larger than its smallest participant) versus Union's sum; CPU = sum(child) + sumCard*IntersectionCPU(1.0) — more expensive than UnionCPU(0.1) — comparison-key matching is heavier than concat. 5 tests (4 fire-when-and-only-when + 1 Planner end-to-end test pinning cost-driven extraction picks physicalIntersectionWrapper over logical alternative). Added to BatchAExpressionRules() (now 7 read-side rules).
- [x] **4 new fuzz targets**. FuzzCaseExpression_FirstMatchWins (3.0M execs/8s clean; CASE lowering through PickValue+ConditionSelector under random implication shapes — pins first-TRUE-wins, all-FALSE returns nil); FuzzArrayConstructorValue_LengthInvariant (3.0M execs/8s clean; ARRAY[...] under random presence + NULL masks — pins empty-array != NULL-array contract, 1:1 child-to-slot); FuzzPlanner_WithBatchA_NoPanic (430K execs/10s clean; Planner with FULL rule set Default+BatchA — stresses implement rules interacting with logical-rewrite chain); FuzzDistanceValue_NumericProperties (3.4M execs/10s clean; 4 distance metrics under random vector pairs — pins symmetry, self-distance == 0 for L2/L2-sq, non-NaN/non-Inf bounded results). Brings cascades fuzz target count to ~63 codebase-wide.
- [x] **11 new benchmarks for late-shift Value batch**. ConditionSelector (FirstTrueWins / AllFalse), PickValue over ConditionSelector, ArrayConstructor (8 elements / empty), RangeValue EvaluateAsStream(100) / Cardinality, Rank / RowNumber harness eval, Udf 2-arg, PatternForLike. Hot paths are 0-allocation (selector chain, harness eval, cardinality math).
- [x] **13 plan-coverage tests** for Distinct/TypeFilter/Union/Intersection/Insert/Delete/Update — closes per-plan-type test gap on the 10-plan RecordQueryPlan hierarchy.
- [x] **7-wrapper symmetry across all 9 implement rules** (mid-late-shift review fix). Reviewer caught structural symmetry gap: implement rules' inner-type switches were missing `*physicalUnionWrapper` / `*physicalIntersectionWrapper`. Filter-over-Union shapes silently couldn't physically implement. Fixed across all 9 rules (Filter / Sort / Distinct / TypeFilter / Union / Intersection / Insert / Delete / Update); also added `*plans.RecordQueryIntersectionPlan` case to `wrapPhysicalPlan`. Regression tests `TestImplementFilterRule_FiresOverPhysicalUnion` + `TestImplementDistinctRule_FiresOverPhysicalUnion` (UNION DISTINCT pattern) + `TestImplementSortRule_FiresOverPhysicalIntersection` (ORDER BY INTERSECT pattern) pin the fix end-to-end. Plus low-priority docs fixes (PickValue position-stable Children, PatternForLikeValue DOTALL gap documented, WindowedValue.SplitNewChildren length-contract documented).
- [x] **CardinalityProperty + OrderingProperty seeds**. Two new property-walk accessors in `properties/`:
  - `EstimateCardinality(e)` / `EstimateCardinalityWith(e, stats)` / `BestRefCardinality(ref)` / `CardinalityLess` (separate accessor over Cost — same underlying walk, projects out CPU axis; useful for cardinality-aware rules).
  - `EstimateOrdering(e)` / `IsOrdered(e)` (yes/no + ordering-keys analysis; Sort produces known order, Filter/Projection/TypeFilter/DML inherit; Union/Intersection/Distinct/Scan return unknown).
  Both have tests (5 + 6 cases) + benchmarks. Closes the TODO.md "Remaining: CardinalityProperty / OrderingProperty as separate property types".
- [x] **SimplifyValue extensions**. SimplifyValue now folds AndOrValue / ConditionSelectorValue / PickValue when all-constant. Means a SQL CASE expression with all-constant operands collapses to a single literal at plan time. Covers the full Boolean-Value composite hierarchy. 5 new fold tests pinning this. Pins GetResultType/GetChildren/GetTargetRecordType/GetComparisonKeyValues/Explain rendering + cross-plan invariants (Union and Intersection hash differently; Insert/Delete/Update hash distinctly; Distinct/Union HashCodeWithoutChildren is consistent across calls).

### nightshift-57 (2026-04-28)

Two Go-vs-Java divergences from CLAUDE.md "Java↔Go conformance gotchas" closed; six new cross-engine A3 scenarios; SQLState wiring infrastructure for cross-engine error_code matching (skipped at the harness gate pending fdb-relational planner stabilisation).

- [x] **Bare-bool projection accepted (Java alignment)** — `SELECT b AND TRUE`, `SELECT NOT b`, `SELECT b OR FALSE` etc. over a BOOLEAN column now match Java. Threaded `allowBareField bool` through `evalExprPredicateTri` / `evalComparisonPredicateTri` (`pkg/relational/core/embedded/eval_predicate.go`): operands of AND/OR/NOT/XOR (any context) and projection-level `evalExpr` (`eval_proto.go`) pass `true` so a bare `FullColumnName` evaluates as a value via IsTruthy. Top-level WHERE / HAVING entry still passes `false`, preserving Java's `WHERE flag` rejection. Five corpus entries re-enabled in `booleanScenario`. CLAUDE.md gotcha entry marked RESOLVED. Go-side regression test in `TestFDB_BareBoolProjection`.
- [x] **`SUM(BIGINT)` int-preserving accumulator** — see HIGH-bucket entry above. `SELECT SUM(qty) / COUNT(*) FROM t` now integer-divides on both engines. CLAUDE.md gotcha entry marked RESOLVED. Go-side regression test in `TestFDB_SumIntegerDivision`. `mapGroupState` / `groupState`'s flag was `sumIntOnly` then inverted to `sumNonInt` per /simplify review (zero-value semantics let `make([]bool, n)` give the correct initial state, dropping the `newAllTrue` helper).
- [x] **6 new A3 scenarios** — `nested_derived_table` (3 specs: 3-level nesting + COUNT(*) + aliased aggregate), `ambiguous_column` (2 qualified-positives specs, comma-join), `correlated_subquery_probes` (2 specs: correlated EXISTS / NOT EXISTS), `union_columns_renamed` (1 spec: positional UNION ALL with differently-named columns), `join_chained` (2 specs: comma-join 2-way + 3-way), `multi_feature_select` (1 spec: column-to-column WHERE with NULL), `count_distinct_join_positive` (1 spec: COUNT(*) cross-join). Net 12 new cross-engine specs; running total ~405.
- [x] **SQLState wiring (Skip'd at harness gate)** — `conformance/conformance_server.java` now extracts SQLSTATE from `SQLException.getSQLState()` and `RelationalException.getErrorCode().getErrorCode()` (via reflection to avoid hard imports) into the structured error response. `pkg/relational/conformance/plandiff/httpclient.go` returns a typed `*JavaError{Message, ExceptionClass, ExceptionFullClass, SQLState}` instead of `fmt.Errorf`. Cross-engine yamsql harness's `assertCrossEngineErrorCode` helper added (currently dead — harness Skips at the per-test gate because lifting it surfaces fdb-relational planner stalls on certain error paths under load: type-mismatch IN-list, GREATEST mixed types, comma-join + bare-col-with-aggregate). Re-enable is a one-line Skip removal once upstream stabilises. `TestJavaError_SQLStateExtraction` + `TestJavaError_NoSQLState` pin the typed-error wiring.
- [x] **CLAUDE.md gotchas added** — `col IN (SELECT ...)` parser-NPE in fdb-relational 4.11.1.0 (joins existing CROSS JOIN / scalar-subquery / COUNT(DISTINCT) NPE list); `WHERE bare-bool-operand` rejection in Java (one-sided gap — Go accepts, Java's planner rejects). Two existing gotchas marked RESOLVED.

### Pure Go FDB Client quality batch (2026-04-25, PR #114)

Three audit findings from a 2026-04-25 deep-research pass on `pkg/fdbgo/`. Verdict: production-grade for FDB 7.3.x; gaps are in test concentration, cadence, and ergonomic papercuts — not in wire correctness or transaction semantics.

- [x] **Adaptive `backgroundGrvUpdater` matching C++** — verbatim port of C++ `release-7.3` `NativeAPI.actor.cpp`. Replaces fixed 50ms ticker with `next = max(1ms, min(MAX_PROXY_CONTACT_LAG - elapsed, (MAX_VERSION_CACHE_LAG - grvDelay) - elapsed))` and EMA `grvDelay = (grvDelay + latency)/2`. Cuts GRV RPC rate ~2× under low load. Math extracted to pure-function `nextGRVRefreshDelay` with 7 table-driven tests. `backgroundRefresher` also fixed to use `b.priority` instead of always `grvPriorityDefault` so BATCH/DEFAULT batchers refresh their respective ratekeeper state.
- [x] **Round-trip fuzz for 4 wire types not in the differential oracle** — SplitRangeRequest, SplitRangeReply, WaitMetricsRequest, WatchValueRequest. Found a real bug: `KeyRangeRef`'s custom MarshalFDB applies C++'s single-key serialize optimization (`begin+'\x00' == end` → write `(end, empty)`) but the generated `UnmarshalFromReader` didn't invert it. Single-key payloads from servers parsed with Begin/End **swapped**. Fixed in custom code; same fix applied to `ParseKeyRangeRefStringVector`; generator updated to skip duplicate-method emission. ~100M fuzz executions clean post-fix.
- [x] **`OnError` + `commitDummyTransaction` respect ctx during retry backoff** — `OnError(err)` → `OnError(ctx, err)`; bare `time.Sleep(d)` → `select { ctx.Done() | timer.C }` via `backoffSleep` helper. Pre-fix: cancelled ctx blocked for full backoff (up to 30s for resource-constrained errors). Post-fix: returns `context.Canceled` within ~50ms.

### swingshift-50 (2026-04-25)

- [x] **Wire `cascades.SimplifyValue` into projection path** — `SELECT 1+2 FROM t` folds at plan time. New `projection_fold.go` walks projExprs through `expr.WalkExpression` → `cascades.SimplifyValue` → `EvaluateConstant` and caches result; per-row consumers (proto + map paths) short-circuit `evalExpr` for cached slots.
- [x] **`SimplifyPredicateValues` for QueryPredicate trees** — folds Value operands inside ComparisonPredicate / ValuePredicate so `WHERE name = 1+2` renders `NAME = 3` in EXPLAIN. Pointer-stable when nothing folds. `buildWherePredicate*` invokes after `WalkPredicate`.
- [x] **First scalar-function batch in walker** — ABS, FLOOR, CEIL, CEILING, ROUND, SQRT, POWER, POW, COALESCE, NULLIF, TRIM, LTRIM, RTRIM, CONCAT, SUBSTRING, SUBSTR, REPLACE. Semantics mirror `embedded.scalar_functions.go`. 26 cascades-side tests + 18 walker tests + 1 EXPLAIN-pinning test + 1 integration test.
- [x] **TODO.md restructure** — flat priority buckets (CRITICAL/HIGH/MEDIUM/LOW). CLAUDE.md priority discipline directive added.

### dayshift-49 (2026-04-25)

- [x] **`ParameterValue`** — `?` and `?name` walk to `cascades.ParameterValue`; positional ordinal counter on Resolver assigns 1-based ordinals matching `database/sql` `NamedValue.Ordinal`. Optional `ParameterBinder` eval-context capability seeded.
- [x] **`ScalarFunctionValue`** — UPPER, LOWER, LENGTH / CHAR_LENGTH / CHARACTER_LENGTH, OCTET_LENGTH dispatch via `walkScalarFunction`.
- [x] **`SimplifyValue`** — standalone-Value constant-fold for ArithmeticValue / CastValue / PromoteValue / ScalarFunctionValue.
- [x] **`DIV` keyword** — shares OpDiv with `/` at the seed (Go's `/` on int64 already truncates).
- [x] **Race-fix on Resolver.functionCatalog** — `sync.Once` lazy build hardened against concurrent first-use.

### nightshift-48 (2026-04-25)

- [x] **Cascades walker wired into logical builder** — `buildLogicalPlanFor{Select,Delete,Update}WithCatalog` attach `cascades.QueryPredicate` to LogicalFilter; text fallback on walker decline / catalog miss / JOIN.
- [x] **Comparison.Operand promoted from `any` to `Value`** — unblocks `a = b`, `a < b + 1`, CAST/arithmetic RHS. `LiteralValue` / `NewLiteralComparison` helpers.
- [x] **CAST/CONVERT to INT/STRING/BOOL** via DataTypeFunctionCall dispatch.
- [x] **likeMatch trailing-escape malformed → no match** — fuzz finding.

### swingshift-47 (2026-04-24)

- [x] **`semantic.Analyzer` seed** — Identifier, QualifiedName, Catalog/Table/Column, Analyzer with ResolveTable/ResolveColumn/ExpandStar, Scope with ambiguity + correlation-chain detection, FunctionCatalog with COUNT/SUM/MIN/MAX/AVG.
- [x] **`rlcatalog/` adapter** for RecordMetaData; `expr/` parse-tree → cascades walker covering binary comparisons, AND/OR/NOT, XOR (Kleene-exact desugar), IS [NOT] NULL/TRUE/FALSE (2VL), BETWEEN, IN, LIKE, parens, aggregate calls.
- [x] **LogicalOperator hierarchy seed** — 12 operators (Scan, Filter, Project, Sort, Limit, Aggregate, Join, Union, Insert, Update, Delete, DDL) + indented Explain rendering.
- [x] **Typed errors** — `TableNotFoundError`, `ColumnNotFoundError`, `AmbiguousColumnError`, `SourceNotFoundError`, `DuplicateAliasError`, `FunctionNotFoundError`, `FunctionArityError`, `UnsupportedFromShapeError`, `UnsupportedExpressionShapeError`.

### dayshift-46 (2026-04-24)

- [x] **4.-0.5 Generics-vs-interfaces decision** (RFC 023) — non-generic `BindingMatcher` + `any` + free-function `Get[T]`. Production seed at `pkg/recordlayer/query/plan/cascades/`.
- [x] **4.-0.25 Plan-cache-key compatibility spec** (RFC 024) — hash-identical Java compatibility is NOT a goal.
- [x] **Cascades Phase 4.0 seed** — Value (5 concrete types), QueryPredicate (4), Comparison/ComparisonPredicate (6 ops), CorrelationIdentifier, BindingMatcher DSL, AllOf/AnyOf, CascadesRule + RuleCall, eleven Phase 4.5 Batch A-style rules, Simplify driver, 7 micro-benchmarks.
- [x] **ORDER BY equality-prefix relaxation** — `WHERE a = 1 ORDER BY b, c` on PK (a,b,c) eliminates the sort.
- [x] **Secondary-index reverse scan** — `WHERE v > 0 ORDER BY v DESC` on `idx_v(v)` reverses instead of sorting.
- [x] **Pure-prefix composite-secondary-index pushdown** — narrows to tuple-prefix scan on the index subspace.

### nightshift-45 (2026-04-23)

- [x] **RFC-021 Phase 1c substantial progress** — connection.go shrunk 11,880 → 2,900 lines (75%). Per-shape files: select_query_full.go, join.go, aggregate.go, cte_scan.go, union.go, insert.go, update_delete.go, select_parser.go, pushdown shapes, order_by.go, scope.go, system_tables.go + system_rows.go, recursive_cte.go, scalar_subquery.go, utilities.go, tri_bool.go, where_extractors.go, select_helpers.go, select_dispatch.go, stmt.go.
- [x] **ORDER BY DESC via reverse scan (PK)** — `naturalOrderSatisfiesReverse` + `scanPropsForOrder`.
- [x] **One-element IN-list → equality** — drops lazy-chain wrapper, unlocks `naturalOrder = pkCols`.
- [x] **Pure-prefix composite-PK pushdown** — `tryPKCompositePrefixFromWhere`.
- [x] **Fuzz corpus with string dimension** — `FuzzLikePatternToPrefix` now `(pattern, escape, s)`; asserts `strings.HasPrefix(s, prefix)` when `likeMatch` returns true.

---

## Reference

### Architectural decisions

**Why a `database/sql/driver` adapter instead of building natively against `database/sql`:** Java's API is JDBC-extending (`RelationalConnection extends java.sql.Connection`). Strict 1:1 means we keep an internal Go API mirroring Java's method surface, then wrap it with a thin `database/sql/driver` adapter. Users get both `sql.Open("fdbsql", ...)` for portability and direct access to the Go-native API via type assertion or a package-level `Open()` for FDB-specific features (options, struct/array types, continuations, fluent SQL).

**Why the cascades planner lives in `pkg/recordlayer/query/plan/cascades`:** Matches Java's layout. Cascades is a planning framework over `RecordQuery`, reusable by anyone writing queries against the record layer — not intrinsic to SQL. The SQL layer *consumes* it.

**DSN format:**
```
fdbsql:///PATH                             # embedded, default cluster file
fdbsql:///PATH?cluster_file=/etc/.../fdb.cluster
fdbsql://HOST:PORT/PATH                    # remote gRPC (later)
```

**Transaction model:** `sql.DB` auto-commit → each statement is its own FDB transaction. `sql.DB.BeginTx()` → explicit `FDBRecordContext` for the lifetime of the `sql.Tx`. Isolation level `sql.LevelSerializable` only. `context.Context` mandatory (5s FDB tx limit).

**Generics decision (RFC-023):** Non-generic `BindingMatcher` + `any` + free-function `Get[T]` retrieval helper. Zero-size-struct identity gotcha — all matcher structs carry a nonce + atomic-counter factory. Rule authors MUST use factories, never bare struct literals.

**Plan-cache-key compatibility (RFC-024):** Hash-identical Java compatibility is NOT a goal. Java's `RelationalPlanCache` is per-process Caffeine; no wire format to preserve. Phase 4.4 free to ship simpler Go-native cost. Go-internal hash stability + schema-version-sensitive keys + test fixtures still required.

**Cascades conformance staging (RFC-022):** 4.-1 plan-equivalence harness builds FIRST. Rule porting in 4.5 starts with Batch A (covers swingshift-44's existing 11-branch pushdown chain) so the harness gets end-to-end yamsql coverage early. Semantic equivalence is hard-required; plan equivalence is separately scoped.

### Type mapping (`driver.Value`)

| SQL type | Go `driver.Value` | Notes |
|---|---|---|
| BOOLEAN | `bool` | |
| INTEGER / BIGINT | `int64` | Java widens to int64 same way |
| FLOAT / DOUBLE | `float64` | |
| STRING / VARCHAR | `string` | UTF-8 |
| BYTES | `[]byte` | |
| TIMESTAMP | `time.Time` | Map to Java's tuple encoding |
| UUID | `[16]byte` / `uuid.UUID` | TBD — match Java SQL UUID |
| STRUCT | custom type | Implement `driver.Valuer` and `sql.Scanner`; expose `pkg/relational/api.Struct` |
| ARRAY | custom type | Same |
| NULL | `nil` | |

Versionstamps and continuations require custom types accessed via type assertion on `*sql.Rows` or a `pkg/relational` helper.

### Scope map (Java → Go)

| Java module | Go package | Role |
|---|---|---|
| `fdb-relational-api` | `pkg/relational/api` | Interfaces, options, error codes, type system (`DataType`), metadata types, struct/array helpers |
| `fdb-record-layer-core/query/plan/cascades` | `pkg/recordlayer/query/plan/cascades` | **Cascades optimizer.** Expressions, Values, Predicates, Rules, Matching, Typing, Memo/References, Cost model. ~104K LOC in Java. |
| `fdb-record-layer-core/query/plan/plans` | `pkg/recordlayer/plan/plans` | Physical plan nodes (`RecordQueryPlan` subclasses). |
| `fdb-relational-core/antlr/*.g4` | `pkg/relational/core/parser` | ANTLR4 lexer/parser (same `.g4` files, regenerated for Go) |
| `fdb-relational-core/recordlayer/query` | `pkg/relational/core/query` | `SemanticAnalyzer`, `PlanGenerator`, `LogicalOperator`, `QueryExecutor` |
| `fdb-relational-core/recordlayer/query/cache` | `pkg/relational/core/cache` | `RelationalPlanCache` (3-tier, TTL) |
| `fdb-relational-core/recordlayer/catalog` | `pkg/relational/core/catalog` | `RecordLayerStoreCatalog`, system tables, schema versioning |
| `fdb-relational-core/recordlayer/metadata` | `pkg/relational/core/metadata` | `RecordLayerSchemaTemplate`, `RecordLayerTable`, `RecordLayerIndex`, `RecordLayerColumn` |
| `fdb-relational-core/recordlayer/ddl` | `pkg/relational/core/ddl` | `ConstantAction` pattern for CREATE/DROP/ALTER |
| `fdb-relational-core/recordlayer/structuredsql` | `pkg/relational/core/structuredsql` | Fluent SQL AST (lower priority) |
| `fdb-relational-core/recordlayer` (conn/stmt/resultset) | `pkg/relational/core/embedded` | `EmbeddedConnection`, `EmbeddedStatement`, `RecordLayerResultSet` |
| `fdb-relational-jdbc` | `pkg/relational/sqldriver` | `database/sql/driver.Driver` adapter |
| `fdb-relational-grpc` | `pkg/relational/grpc` *(later)* | gRPC service stubs + protobuf wire |
| `fdb-relational-server` | `cmd/frl-server` *(later)* | Standalone SQL server binary |
| `fdb-relational-cli` | `cmd/frl` | Operator/developer CLI |

### Behavioral divergences from C++ FDB client (audit 2026-04-13, updated swingshift-18)

Three remaining acknowledged divergences (all judged acceptable):

| # | Area | Type | Description |
|---|---|---|---|
| 6 | Auto-reset after commit | DESIGN | C++ no auto-reset at API >= 410. Go `postCommitReset()` clears for reuse. |
| 18 | Wrong-shard retry cap | CONSERVATIVE | Go caps at `MaxWrongShardRetries=50`. C++ loops unbounded (relies on 5s tx timeout). Go returns error earlier under extreme shard movement. |
| 19 | GRV background refresh | PERF | Go refreshes at fixed 50ms. C++ uses adaptive delay `(grvDelay + latency)/2` (1ms-100ms range). Go is more aggressive (2x more RPCs under low load). |

15 of 21 audit divergences fixed (see git history for resolution detail). Cosmetic differences (FLAG_FIRST_IN_BATCH, frame checksum CRC32 vs XXH3-64, QueueModel key) intentionally not aligned.

### Missing C API surface

All data-path functions implemented. Missing observability/admin only:

| C Function | Category | Assessment |
|---|---|---|
| `fdb_transaction_get_mapped_range` | Niche | Server-side index join. Record Layer doesn't use it. |
| `fdb_transaction_get_total_cost` | Observability | Estimated transaction cost for rate limiting. |
| `fdb_database_force_recovery_with_data_loss` | Admin | DR operation. |
| `fdb_database_create_snapshot` | Admin | Disk-level backup. |
| `fdb_database_get_main_thread_busyness` | N/A | Go has no network thread. |
| `fdb_database_get_server_protocol` | Niche | Multi-version client coordination. |

### Core SQL requirements

1. **1:1 aligned with Java.** Package names, class/struct names, behavior, wire format. Catalog storage, plan cache keys, protobuf encodings, SQL dialect must be bit-compatible.
2. **Usable from `database/sql`.** Primary public entry is `driver.Driver` registered as `fdbsql`. `sql.Open("fdbsql", dsn)` is non-negotiable.
3. **Embedded first.** Start with in-process execution. gRPC remote / standalone server later.
4. **Keep parser dialect identical.** Same `RelationalLexer.g4` / `RelationalParser.g4` grammar; regenerate with `antlr4-go/antlr4`. No dialect drift.

### Risks & open questions

1. **Cascades port scope is enormous** (~104K LOC Java → 80K+ Go). Many shifts; needs sub-RFCs per rule family. Hand-rolled heuristic alternative rejected — would break plan-cache-key compat forever.
2. **ANTLR-go performance.** Java's runtime is well-tuned; antlr4-go/antlr4 less mature. Parse-hot-path benchmarking required.
3. **`database/sql` impedance mismatch.** `driver.Value` is closed (bool/int64/float64/string/[]byte/time.Time/nil). Struct/array/enum/versionstamp need custom `Scanner`/`Valuer` types; users opt in explicitly.
4. **Catalog migration.** Wire format wrong → user data needs migration. Conformance tests for catalog read-back BEFORE writing.
5. **Testing the planner.** No FDB call-site validates plan quality end-to-end beyond correctness. Need yamsql runner + EXPLAIN diff harness against Java (covered by 4.-1).

### Non-goals (explicit)

- UDFs (`PUserDefinedFunction`) — out of scope until planner is done
- Views (except trivial `SELECT *`-over-base-table) — deferred
- Synthetic record types (`JoinedRecordType`, `UnnestedRecordType`) — deferred
- Java SQL function catalog / semantic analyzer rules that depend on it — simplify or defer
- Callable statements, holdable/scrollable result sets, savepoints — Java throws `SQLFeatureNotSupportedException`; we do the same
- LOB types (`Blob`, `Clob`, `NClob`, `SQLXML`) — same, unsupported

### Test coverage snapshot (current)

- 2817 Ginkgo specs + 438 conformance specs + 220 chaos tests + 93 C binding port tests + 34 correctness tests + 15 Go↔CGo interop tests + 200+ binding tester seeds (0 failures)
- Line coverage: 81.0% overall. `just coverage` generates HTML report
- Race detector: CI runs on all 5 FDB test targets. Locally: `just race-all`
- Fuzz targets: 28 (12 record layer parsers + FuzzRYWCache + 8 wire reply parsers + 2 wire Reader parsers + FuzzPackIntoEquivalence + FuzzLikePrefixStrinc + FuzzLikePatternToPrefix + FuzzLikeMatch + FuzzLikeMatchEscape)
- Performance: Go wins 5/8 benchmarks vs Java Record Layer. Reads 27-39% faster, writes within 2-7%

### Conformance status

- **Record Layer:** CRUD, split records, continuation tokens, record versioning, record counting, **all 19 index types**, KeyWithValueExpression covering indexes, index scanning/state/build/rebuild, **OnlineIndexer** (BY_RECORDS, BY_INDEX, MULTI_TARGET, MUTUAL strategies), cursor combinators (concat/map/filter/skip/limit/union/intersection/dedup/flatmap/chained/auto-continuing/fallback), time/byte/record scan limits, MetaDataValidator, MetaDataEvolutionValidator (full IndexValidatorRegistry), commit hooks, retry runner, store state management, EvaluateAggregateFunction, EvaluateRecordFunction, FDB directory layer, FDBMetaDataStore.
- **FDB Client vs C:** 100% data-path API coverage. 18/21 C++ audit divergences fixed (3 remaining: auto-reset after commit, wrong-shard retry cap, GRV background refresh — all judged acceptable).
- **SQL Layer:** parser (1587/1587 yamsql statements parse), CRUD, JOINs (INNER/LEFT/RIGHT OUTER), GROUP BY + HAVING, CTEs (incl. recursive with TRAVERSAL ORDER), UNION ALL/DISTINCT, derived tables, subqueries (IN/EXISTS scalar), CASE WHEN, scalar functions (~40), 11-branch pushdown chain (PK + secondary, equality + range + IN-list + LIKE prefix + composite, covering-index variants). Cross-language conformance NOT yet verified — see CRITICAL.
