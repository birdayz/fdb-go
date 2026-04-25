# TODOs

Authoritative priority list for fdb-record-layer-go. Strict precedence: **CRITICAL > HIGH > MEDIUM > LOW**. Pick work from the highest unchecked bucket. Shift handover follow-ups are context, not priority — see CLAUDE.md "Priority discipline at shift start".

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

Restructured 2026-04-25 (swingshift-50). Previous structure: `git show 4cc93ccb:TODO.md`.

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

---

## HIGH

### Cascades planner port (Phase 4.x)

Per RFC-022, only attempt 4.0+ AFTER 4.-1 lands. Listed here so the work scope is visible.

- [~] **4.0 — Foundation types**
  - [~] `Type` / `TypeRepository` / `Typed` — seed landed nightshift-50 in `cascades/values/type.go`. `Type` interface (Code + IsNullable + Equals + String); `TypeCode` enum mirroring Java's well-known codes (NULL/BOOLEAN/INT/LONG/FLOAT/DOUBLE/STRING/BYTES/VERSION/ENUM/RECORD/ARRAY/RELATION/ANY/NONE/UUID); `PrimitiveType` impl + canonical singletons (NotNullInt, NullableString, …); `Typed` interface (RichType()); legacy-bridge adapters `FromValueType` / `ToValueType` for incremental migration. **Remaining:** `RecordType`, `ArrayType`, `EnumType`, `UuidType`, `RelationType` structured impls; `TypeRepository` for type registration; plan-serialisation hooks; full conversion / coercion lattice. Migration is incremental — old `ValueType` enum keeps working until call sites flip over piecewise.
  - [~] `Value` hierarchy — dayshift-46 seeded `Value` interface (with `Evaluate`) + 5 concrete types (Constant, Field, Arithmetic, Boolean, Cast). nightshift-48 + dayshift-49 added Promote, Null, Aggregate, QuantifiedObject, RecordConstructor, ParameterValue, ScalarFunctionValue. swingshift-50 added NotValue (Value-layer boolean negation, Kleene 3VL) + overflow-checked `ArithmeticValue.Evaluate` (ADD/SUB/MUL/DIV bounds-checked, MIN/-1 + MIN*-1 boundary cases — closes the divergence between fold-time and runtime semantics where the embedded executor's `ApplyMathOp` errors but the fold silently wrapped). Remaining: ~66 of Java's 77 value classes.
  - [~] `QueryPredicate` hierarchy — Constant / And / Or / Not / Value / Comparison ported. Remaining: `ComparisonRange(s)`, `MatchesValue`, `Placeholder`, `PredicateWithValueAndRanges`.
  - [~] `Simplification` — dayshift-49 added `SimplifyValue` (constant-fold over standalone Values). swingshift-50 added `SimplifyPredicateValues` (folds Value operands inside QueryPredicates). Phase 4.6 brings the full `ValueSimplificationRuleSet` and the rule-driven driver retires the seed.
  - [~] `Comparisons` / `Comparison` — 13 operators + `Comparison.Operand` as `Value` + `LiteralValue` / `NewLiteralComparison` helpers + `ParameterValue`-bound variant. Remaining: real binder plumbing through Evaluate (`ParameterBinder` interface seeded but no runtime callers).
  - [~] `Correlated<T>` + `CorrelationIdentifier` — dayshift-46 seeded interface + Named/Unique factories. Concrete `Correlated` impls land as Values gain richer Quantifier references.
- [ ] **4.1 — Relational expressions** — `RelationalExpression`, `RelationalExpressionWithChildren`, `RelationalExpressionWithPredicates` + Logical exprs (`LogicalFilterExpression`, `LogicalProjectionExpression`, `LogicalSortExpression`, `LogicalTypeFilterExpression`, `LogicalUnionExpression`, `LogicalDistinctExpression`, `LogicalIntersectionExpression`, `SelectExpression`) + DML (`InsertExpression`, `UpdateExpression`, `DeleteExpression`, `TableFunctionExpression`).
- [~] **4.2 — Matching engine**
  - [~] `BindingMatcher` DSL — dayshift-46 seeded interface + `PlannerBindings` + `MergedWith` + generic `Get[T]` retrieval helper + `AnyValue` + `Instance` + `ArithmeticMatcher` + `AllOfMatcher` + `AnyOfMatcher`. swingshift-50 added `ListMatcher` (positional []any pairing, length-strict) + `AllElementsMatcher` (Java's MultiMatcher.AllMatcher — same downstream applied to every element). Remaining: `TypedMatcherWithExtractAndDownstream`, `CollectionMatcher` interface, `OptionalIfPresentMatcher`, `PartialMatchMatchers`, the `graph/` matchers, `MultiMatcher.SomeMatcher`.
  - [x] `PlannerBindings` — dayshift-46.
- [ ] **4.3 — Memo & references** — `Reference` (= Cascades "group"), implicit DAG via `Reference` pointers, `PlanContext`, `CascadesRuleCall`.
- [ ] **4.4 — Cost model** — `CascadesCostModel` heuristic comparator matching Java; cardinality estimation hooks; `properties/` package (~25 classes). Per RFC-024, hash-identical Java compatibility is NOT a goal — free to ship simpler Go-native cost.
- [~] **4.5 — Rules**
  - [~] Rule base classes (`CascadesRule`, `CascadesRuleCall`) — seeded dayshift-46.
  - [~] Predicate-simplification rule set — dayshift-49 + earlier shifts shipped `AndFlattenRule`, `OrFlattenRule`, `AndConstantSimplifyRule` (annulment + identity unified), `OrConstantSimplifyRule`, `NotConstantSimplifyRule`, `AndDedupRule`, `OrDedupRule`, `AndAbsorbOrRule`, `OrAbsorbAndRule`, `NotComparisonRewriteRule`, `ComparisonConstantSimplifyRule`. swingshift-50 added `DeMorganRule` + `NormalizationRules()` rule set (separate from `DefaultSimplifyRules` because Java applies De Morgan via `BooleanNormalizer` as a pre-CNF pass) + `ValuePredicateConstantFoldRule` (unwraps `VP(constant)` to `ConstantPredicate`, type-degraded inputs → UNKNOWN). Remaining (mostly Phase 4.6 ValueSimplificationRuleSet): `ConstantFoldingMultiConstraintPredicateRule`, `ConstantFoldingPredicateWithRangesRule`, `IdentityAndRule`/`IdentityOrRule` (already covered by our unified rules), `NormalFormRule`.
  - [ ] **Batch A (port FIRST per RFC-022 — covers swingshift-44's existing 11-branch pushdown chain so 4.-1 harness gets end-to-end yamsql coverage):** `PrimaryScanRule`, `ImplementFilterRule`, `ImplementSortRule`, `MergeFetchIntoCoveringIndexRule`, index-equality + index-range implementation rules, `InComparisonToExplodeRule`.
  - [ ] **Batch B and beyond** — rest of data access rules (`AbstractDataAccessRule`, `AggregateDataAccessRule`), implementation rules (`ImplementDistinctRule`, `ImplementNestedLoopJoinRule`, `ImplementRecursiveDfsJoinRule`, `ImplementStreamingAggregationRule`…), decomposition (`DecorrelateValuesRule`), optimization (`PushPredicateThroughDistinctRule`, `MergeFetchIntoTypeFilterRule`…), finalization (`FinalizeExpressionsRule`). ~69 rules total. Port in batches aligned to yamsql feature flags (JOIN, CTE, aggregate).
- [ ] **4.6 — Planner driver** — `CascadesPlanner` task stack (EXPLORE → OPTIMIZE), `PlannerEvent` debug hooks, integration with `RecordMetaData` + index availability.
- [ ] **4.7 — Correctness tests** — port enough of Java's planner test suite to validate rule-by-rule equivalence; extend the 4.-1 harness as rules land.

### Cross-language conformance & infra

- [ ] **System table contents byte-equivalence** — `SELECT * FROM INFORMATION_SCHEMA.TABLES` returns byte-identical rows from Go and Java against the same store.
- [ ] **FRL perf comparison — Go vs Java SQL** — once Phase 5 lands enough to run a real SELECT, stand up the same comparison harness for SQL workloads (simple SELECT, secondary-index SELECT, INSERT, aggregate, prepared statement). Go-vs-Java table for relational layer mirroring the record-layer numbers in CLAUDE.md.

### Security / robustness

- [ ] **ANTLR parser exponential-time on unclosed parens (DoS)** — 4-min FuzzParse run (swingshift-35) surfaced a 3.4KB `CASE WHEN x IS NULL T((((...` input that takes ~8.7s to parse. Same grammar as Java so the vulnerability exists there too. Corpus entry `a1c9802306691af3` pinned as regression. Real fix likely needs grammar tweaks or a parse-time limit in both Go and Java. Upstream ticket worthwhile before Go-only hardening.

### SQL feature gaps (significant)

- [ ] **DDL types** — `DATE` / `TIMESTAMP` / `ARRAY` / `JSON` column types. Today's `CREATE TABLE` accepts only BIGINT / INTEGER / DOUBLE / FLOAT / STRING / BYTES / BOOLEAN. Java has all of these.
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

---

## MEDIUM

### Cascades port — already-seeded follow-ups (defer until 4.-1 lands)

- [ ] **`ParameterBinder` caller plumbing** — eval-context capability seeded (`cascades.ParameterValue.Evaluate` checks for it) but no runtime caller exists. The embedded executor textually substitutes `?` via `substituteParams` BEFORE parsing, so cascades never sees a `ParameterValue` at runtime. A real wire-in needs the embedded executor to evaluate cascades.Value at runtime — bigger than half-shift sized. Unblocks runtime prepared-statement plan-cache reuse.
- [ ] **Wire `SimplifyValue` more places** — swingshift-50 wired projection (proto + map paths) and WHERE predicate-tree Value operands. Other callers: aggregate `aggExpr` / `outExpr`, ORDER BY expression keys, INSERT VALUES expressions.
- [~] **More walker scalar functions** — verified nightshift-50: walker `scalarFunctionResultType` already covers SIGN, MOD, IFNULL, IF/IIF, GREATEST/LEAST, EXP/LN/LOG, REVERSE, POSITION, LEFT, RIGHT (in addition to swingshift-50's batch: ABS, FLOOR, CEIL, CEILING, ROUND, SQRT, POWER, POW, COALESCE, NULLIF, TRIM, LTRIM, RTRIM, CONCAT, SUBSTRING, SUBSTR, REPLACE). Remaining gap: date/time functions (NOW, CURDATE, YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DAYOFWEEK, DAYOFYEAR — `embedded.scalar_functions.go:716-757`). These need TypeDate / TypeTime in the value-type seed; gated on Phase 4.0 Type hierarchy port.
- [~] **Derived-table WHERE in catalog-aware builder** — basic shape landed nightshift-50. `(SELECT col1, col2 FROM realtable) AS x WHERE x.col = ?` now routes through `buildWherePredicateForDerived` → `buildDerivedTableSource` which extracts the inner query's projections via `extractFromQueryTerm` and synthesises a `semantic.StaticTable` whose columns inherit the inner-table types. Computed projections (`SELECT a + 1 AS v FROM ...`), `SELECT *`, joins inside the derived, aggregates, and qualified-star projections all decline cleanly to text fallback. **Remaining (Phase 4.0-gated):** computed projections need real type inference for the projected expression's result type — the current bail will lift once the Type hierarchy lands.
- [ ] **Multi-source scope for JOIN in cascades walker** — buildWherePredicateForJoins handles JOIN; further parity with Java's scope semantics needed as more rules land.

### SQL feature gaps (smaller)

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

- [ ] **Zero fuzz targets in `pkg/relational/`** (record-layer has 24). Add `FuzzParse(sql)`, `FuzzEvalExpr(tree)`, `FuzzContinuationToken`, `FuzzSchemaTemplateProto`.
- [ ] **Error-path coverage ~0.2% in `pkg/relational/`** (2 error assertions vs 862 success in `embedded_fdb_test.go`). Add tests for type mismatch on INSERT, NOT NULL violation, missing schema, invalid SQL at execute time, duplicate CREATE DATABASE, PK conflict.
- [ ] **Parser tree-shape conformance tests** (stretch) — feed the same SQL corpus through both parsers and diff trees, or pick representative corners. Requires JSON serialiser on both sides. Not a blocker for Phase 2 — semantic analyzer tests catch tree-shape regressions indirectly.

### Infrastructure

- [ ] **Throughput benchmarks fail on single-node testcontainer** — `BenchmarkThroughputInsertBatchConcurrent128` overwhelms FDB testcontainer. Two issues: (1) GRV cache staleness causes "record store does not exist" on first goroutines after setup; fix: `InvalidateGRVCache()` after store creation. (2) FDB 5-second tx timeout under load → "context deadline exceeded". Fix: skip in `just bench` or use larger cluster. `just bench-ci` excludes throughput benchmarks and works fine.

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
