This range requires substantive parity work across SQL semantics, aggregation, functions, metadata, catalog operations, snapshot execution, and vector indexing. Several Go mechanisms already match portions of the target, but the upgrade cannot be considered complete from a pin change or the array fixes alone.

This is a source audit. **Implementation must await RFC design review.** No builds, tests, repository edits, branch switches, or worktrees were used.

**1. Scope and completeness**

| Item | Audited revision or count |
|---|---|
| Go baseline | `e48f5b4965543cd4d99b5578356059e12d969c7c` |
| Java base | `4.12.11.0` — `257aa83cae7f90e18ea6595fdf2cf841ca72e802` |
| Java target | `4.14.2.0` — `fdacd162a9c8acfadc49082b89185c823ab8ae4a` |
| Assigned population | [relational.paths](/var/tmp/query-grind-cast/java-upgrade/audit/relational.paths): **404 unique paths** |
| Net statuses | **340 modified, 63 added, 1 deleted** |
| Complete scoped textual diff | **47,749 lines; 2,311,955 bytes; 404 file headers** |
| Binary artifacts | **79 `.metrics.binpb` paths**, decoded separately |
| Scoped history | **90 commits** touching the assigned population |

The 404 paths comprise:

| Module | Paths |
|---|---:|
| `fdb-relational-api` | 9 |
| `fdb-relational-cli` | 4 |
| `fdb-relational-core` | 117 |
| `fdb-relational-grpc` | 11 |
| `fdb-relational-jdbc` | 4 |
| `fdb-relational-server` | 3 |
| `yaml-tests` | 256 |
| **Total** | **404** |

By file type: **165 Java, 79 binary metrics, 79 YAML metrics, 67 YAMSQL, 7 Gradle, 4 protobuf, 2 grammar, and 1 JSON**.

I passed the literal path list as arguments to:

```text
git -C fdb-record-layer diff 4.12.11.0 4.14.2.0 -- <the 404 exact assigned paths>
```

I read the complete output in successive manageable chunks, recovering truncated ranges. I then read necessary target implementations and tests, scoped history and relevant commit bodies, and corresponding Go code through `git show`/`git grep` against the specified baseline. Current working-tree changes were not used to establish baseline behavior.

There is **no unread assigned textual diff**. Binary metric messages were decoded and compared at the identifier, explain, counter/timer, and DOT-string level. DOT graphs were not rendered or individually inspected visually. No runtime parity, FoundationDB concurrency behavior, or mixed-version compatibility was exercised.

The remaining **785 Java changed paths** belong to other researchers. References below to core planner/value/protobuf implementations outside this population are narrow dependency checks, not coverage claims for those groups.

For compact references below:

- `C/` = Java `fdb-relational-core/src/main/java/com/apple/foundationdb/relational/`
- `A/` = Java `fdb-relational-api/src/main/java/com/apple/foundationdb/relational/`
- `Y/` = Java `yaml-tests/src/test/resources/`
- `R/` = Go `pkg/relational/`
- `P/` = Go `pkg/recordlayer/query/plan/cascades/`
- `V/` = Go `pkg/recordlayer/query/plan/cascades/values/`

Java line references are at the target; Go line references are at the baseline.

**2. Work units and findings**

**W1 — SQL comments, canonicalization, literal decoding, and SELECT without FROM**

Upstream: `7f846514a` / **#4392**, `e556005f8` / **#4379**, `b9790b9af` / **#4524**, `dfde45f3c` / **#4199**.

Java changes:

- Comments are skipped lexically, including their token-index contribution. This matters because literal identifiers derive from token positions.
- `--` starts a comment without requiring following whitespace. CR, LF, CRLF, and EOF terminate it.
- Block comments nest. Unterminated nesting produces a lexer syntax error.
- `#` is no longer a line-comment introducer.
- MySQL executable-comment and hint-looking forms are ordinary ignored block comments.
- Keyword case is canonicalized independently of quoted identifier case.
- String literals decode doubled quotes. Adjacent literal tokens concatenate independently: `'a' 'b'` means `ab`, while `'a''b'` means `a'b`.
- New grammar tokens and identifier allowances include `ARRAY_AGG`, `RESPECT`, GuardiANN, and vector-option identifiers.
- SELECT without FROM receives a one-row source. The grammar’s placement of WHERE remains significant.

Evidence: lexer `RelationalLexer.g4:29`; `C/recordlayer/query/QueryParser.java:88`; `AstNormalizer.java:155,191`; `SemanticAnalyzer.java:197,223`; `visitors/QueryVisitor.java:258`.

Go status:

- **Required:** comment grammar and cache normalization alignment. `R/core/parser/grammar/RelationalLexer.g4:29` retains the old comment rules. `R/core/embedded/query_hash.go:168` separately strips comments with incompatible nesting, hash-comment, and line-ending behavior.
- **Already present:** keyword case folding and quoted-text preservation in `query_hash.go:118,139`.
- **Already present for single tokens:** doubled-quote decoding in `R/core/functions/cast.go:244`.
- **Required:** adjacent-token decoding. `R/core/query/expr/walk.go:2246` decodes the concatenated parse-tree text, losing the distinction between adjacent tokens and doubled-quote escapes.
- **Already present:** a no-FROM values source in `R/core/embedded/logical_builder.go:438`.

Regression contracts: comment/no-comment cache equivalence with literals and parameters; every line ending; nested and unterminated comments; markers inside strings and identifiers; adjacent versus escaped literals across projection, DML, predicates, and LIKE ESCAPE; one-row no-FROM expressions.

Two existing expectations need targeted revision: `R/core/embedded/query_hash_test.go:30` treats hash comments as removable, and `:32` pins malformed block-comment normalization to `SELECT * FROM FOO D`. Malformed SQL must not gain legitimacy through normalization.

**W2 — Array shape, element nullability, NULL predicates, and IN validation**

Upstream: `851712f14` / **#4171**, `a3694976f` / **#4453**, `18ea45ba6` / **#4480**, `9713752de` / **#4469**, `f525f927f` / **#4595**.

Java changes:

- Array construction preserves one-field records instead of flattening them indiscriminately.
- SQL array elements are non-nullable. Bare NULL elements are rejected; nullable expressions are promoted to non-nullable element types and fail when their evaluated value is NULL.
- Type conversion no longer rejects every array merely because its inferred element type was nullable.
- Direct nested arrays remain unsupported; a record containing an array is a different, supported shape.
- Bare NULL items in explicit IN lists are rejected during normalization, before cache lookup, with `WRONG_OBJECT_TYPE`.
- Typed NULL expressions are not rejected by that syntactic check. Actual NULL array elements still fail with `UNSUPPORTED_OPERATION`.
- Bound NULL used as a predicate behaves like inline NULL.
- IN subqueries receive an explicit `UNSUPPORTED_QUERY` rejection in Java.

Evidence: `C/recordlayer/metadata/DataTypeUtils.java:76,114`; `SemanticAnalyzer.java:863`; `visitors/ExpressionVisitor.java:1023,1187`; `AstNormalizer.java:507`; `ExpressionVisitor.java:714`; target `InListNullParameterTest.java:53,65`.

Go status:

- **Required:** array construction/type enforcement in `R/core/query/expr/walk.go:1546`; current construction can retain nullable element types.
- **Required:** distinguish array-of-record from scalar-array behavior. The IN path at `walk.go:1983` also contains one-field record flattening that must be checked against the target shape contract.
- **Required:** target error classification. `R/core/functions/proto_value.go:196` currently reports the older collection-NULL failure at serialization.
- **Already present in part:** inline NULL predicate handling and rejection of literal NULL IN items; see `walk_test.go:366` and `R/core/embedded/logical_predicate.go:1004`.
- **Pre-existing parameter-model gap exposed by these contracts:** `R/core/embedded/utilities.go:94,164` substitutes bound nil as SQL NULL. That loses the distinction between a bare NULL token and a bound NULL (untyped in the target: setNull's SQL type is discarded, measured by the WS-E oracle). Inline rejection alone does not prove the target’s prepared-parameter behavior.

The normalizer change is defensive validation placement. Its target source explicitly notes that current NULL-containing canonical keys still differ; this audit does not claim a demonstrated cache-poisoning exploit in either engine.

The controller-owned mixed-array and **pre-existing structured `PromoteValue` defect remain mandatory upgrade dependencies**. Baseline `V/values.go:5152,5177` derives promotion nullability from the child and implements primitive/UUID/enum conversion without general recursive structured conversion. The accepted structured defect must be re-armed and closed, including positive success witnesses.

Regression contracts must distinguish:

- Bare NULL IN item → `42809`.
- Bound NULL IN item (untyped in the target) → `0A000`.
- Non-NULL bound value → successful membership.
- NULL predicate → unknown/filtered, matching inline NULL.
- Outer nullable array versus forbidden NULL element.
- Empty array, one-field record array, nested record containing an array, and unsupported direct nested array.
- Cold and warm cache behavior.

Existing SQL array rejection pins requiring revision include `R/conformance/yamsql/cast_boundary_fdb_test.go:110` and `R/sqldriver/in_list_array_column_fdb_test.go:193`. Preserve their negative assertions while adopting the target failure contract.

Go-only query extensions may remain; Java’s new IN-subquery rejection is not authorization to remove an existing Go extension. [Checked later: at the base `71ccd8cf8` Go already refuses every IN-subquery shape with 0AF00, so there is no such extension to keep; the umbrella RFC's WS-E section records it.]

**W3 — LIKE semantics and SQLSTATEs**

Upstream: `35f0de646` / **#4430**, with literal-decoding integration from **#4524**.

This is a substantial semantic change, not simply an algorithm replacement.

Java now:

- Uses polynomial matching with full-string semantics.
- Allows wildcards to consume line terminators.
- Removes the old regex-derived trailing-newline behavior.
- Treats `_` as one Unicode code point, including a supplementary character.
- Requires ESCAPE to be exactly one non-surrogate UTF-16 character.
- Rejects `%` and `_` as escape characters.
- Permits escaping `%`, `_`, and the escape character itself.
- Rejects dangling or invalid escape sequences when the pattern value is evaluated.
- Handles NULL patterns and reports non-string operands/patterns through the function argument error contract.

Evidence: `C/recordlayer/query/visitors/ExpressionVisitor.java:695`; `C/recordlayer/util/ExceptionUtil.java:95`; `A/api/exceptions/ErrorCode.java:83`; `Y/like.yamsql:97,114,138,406`. Necessary helper inspection confirmed `PatternForLikeValue.java:92,115,133,141` validates the whole pattern before matching; `LikeOperatorValue.java:145` implements the matcher.

New SQLSTATEs:

| Condition | SQLSTATE |
|---|---|
| Escape conflicts with wildcard | `2200B` |
| Invalid escape character | `22019` |
| Invalid escape sequence | `22025` |
| Non-string function argument | `22F00` |

Go already has a polynomial matcher, but preserves the **old semantics**:

- `V/like_match.go:98` trims a final newline.
- `:133,147` exclude Java regex line terminators.
- `:203` preserves old escape behavior.
- `V/value_like.go:104` returns nil for some invalid operand types.
- `R/core/query/expr/expr.go:1560` restricts pattern forms and uses different error classification.
- `R/core/query/expr/walk.go:2027` measures ESCAPE as Go runes, permitting supplementary characters that Java rejects.

**Required port:** semantic alignment across both scalar-value and predicate/comparison paths, including system-table filtering. Dynamic pattern restrictions are a pre-existing Go gap, not a newly introduced Java feature. (Corrected by the WS-E oracle: the target itself rejects a bound pattern, `LIKE ?` is 42601 because the grammar's pattern is `constant` [prepared_like_param_pattern]; Go accepting it today is the divergence, and WS-E removes it.)

Confirmed obsolete tests include:

- `V/like_match_test.go:51,106,329`
- `P/predicates/comparisons_test.go:1057,1258`
- `R/core/embedded/embedded_test.go:477`
- `R/sqldriver/like_escape_parity_fdb_test.go:56,108`
- `R/core/embedded/like_prefix_not_sargable_test.go:77`

The regex oracle and newline-based prefix arguments must change individually. This does **not** authorize a new LIKE optimization project.

Compatibility dependency: the Java pattern intermediate value now has a record result containing pattern and escape. Matching protobuf field numbers alone cannot establish compiled-plan compatibility.

**W4 — ARRAY_AGG, aggregate validation, whole-record arguments, and COALESCE typing**

Upstream: `d830f6847` / **#4424**, `2dd8f6459` / **#4425**, `1b124cecc` / **#4463**, `d470e4c82` / **#4597**, `4c41cce8f` / **#4600**, `ec1c94c5d` / **#4535**, `db79d3e4c` / **#4500**, `4257ebfca` / **#4501**, `4e44a35ac` / **#4440**.

ARRAY_AGG is a new required end-to-end feature:

- Default `RESPECT NULLS`; `IGNORE NULLS` available.
- A group containing only ignored NULLs returns `[]`.
- Empty ungrouped input returns one NULL aggregate; empty grouped input returns no rows.
- Actual NULL elements under RESPECT NULLS remain unsupported.
- Typed NULL under IGNORE NULLS is meaningful; untyped NULL cannot establish the element type.
- In-call LIMIT is a literal integer from zero through `Integer.MAX_VALUE`, applies per group, and limits retained elements.
- After the limit is reached, later NULLs are ignored; a NULL encountered before reaching the limit still fails.
- DISTINCT, aggregate OVER, and in-call ORDER BY remain unsupported. Argument and sort-expression validation order is intentional.
- Partial aggregate state survives continuation boundaries, including a page returning no completed group.
- Records, UUIDs, and bytes require correct result conversion. Direct nested-array results remain unsupported.
- Without ORDER BY, SQL does not promise which elements a limited aggregate retains.

Evidence: `C/recordlayer/query/visitors/ExpressionVisitor.java:367,448`; `functions/SqlFunctionCatalogImpl.java:126`; `Y/array-agg-tests.yamsql:174,292,395,484,520`.

Go’s actual catalog and executor establish the gap: `R/core/query/semantic/functions.go:110`, `R/core/embedded/select_parser.go:491`, and `pkg/recordlayer/query/executor/streaming_cursors.go:713,932` implement existing aggregate families, not ARRAY_AGG.

**Required:** grammar, typed aggregate representation, planner lowering, runtime accumulator, bounded retained state, serialization/continuation, and nested result metadata. A parser-only implementation is insufficient.

Two validation changes are **already implemented in Go**: DISTINCT and aggregate OVER rejection with `0AF00`, through `R/core/embedded/cascades_generator.go:390,421` and `R/core/embedded/ddl.go:604`. Older comments in `select_parser.go` are stale; they must not be mistaken for current behavior.

Whole-record aggregation additionally requires:

- Bare table-alias arguments, with real-column resolution taking precedence.
- Simplification before matching expressions against grouping outputs.
- Preservation of ambiguity errors and record field order.

Java evidence: `C/recordlayer/query/Expression.java:240`; `Expressions.java:101`; `OrderByExpression.java:74`. Go currently resolves column paths at `R/core/query/expr/expr.go:273`, supports explicit parenthesized stars at `walk.go:1644`, and compares group keys structurally without the new paired simplification at `R/core/embedded/logical_predicate.go:4398,4685`.

COALESCE’s result is nullable only when all arguments are nullable; argument promotion must preserve each argument’s nullability. Go’s shared type routine unconditionally widens to nullable at `V/scalar_function_catalog.go:504,543`. The COALESCE expectation at `scalar_function_catalog_test.go:176` explicitly pins the obsolete result for non-null inputs.

COUNT requires care: Java’s raw `CountValue` remains nullable, while the **ungrouped SQL result** is adjusted through `COALESCE(count,0)` at `C/recordlayer/query/LogicalOperator.java:651` and `visitors/QueryVisitor.java:307`. Do not globally change raw aggregate typing. Reconcile `R/core/embedded/agg_output_cols_nullability_test.go:26` with this distinction.

Target defect retained upstream: **#4573**, pinned by `b2e3b9eae` / **#4589**, still fails when resuming certain partial groups whose aggregate has no value. `GroupByQueryTests.java:118` is an explicit failure pin. It is not a delivered fix and should not cause Go to reproduce a known failure.

**W5 — User-defined macro functions, call-site arguments, and window options**

Upstream: `b2b930437` / **#4318**, `daaf0f2e6` / **#4397**.

Java expands macro functions from restricted bodies to arbitrary expressions, with positional/named parameters, defaults, explicit return-type promotion, array parameters/returns, and `AS` or `RETURN` bodies. Table-returning macro forms remain unsupported. Function-body literal handling deliberately avoids unsafe extraction into the surrounding query’s literal bindings.

Function invocation now carries positional/named arguments, window specifications, and typed options through `CallSiteArguments`. Mixed named/unnamed forms are rejected. Argument resolution and promotion are shared across invocation routes. A bad argument to an existing SQL function now receives `INVALID_ARGUMENT_FOR_FUNCTION`; see target `SqlFunctionTest.java:259`.

Evidence: `C/recordlayer/query/functions/UserDefinedFunctionBuilder.java:76,198,251`; `CompiledSqlFunction.java:94`; `Expressions.java:325`; `SemanticAnalyzer.java:1139`; `WindowSpecExpression.java:109`; `visitors/ExpressionVisitor.java:291,347`.

Go status:

- **Pre-existing major gap:** persisted SQL functions, temporary functions, and views are not implemented on the shared SQL surface. `R/core/embedded/ddl.go:300` rejects them; `R/core/metadata/schema_template.go:154,181` exposes empty collections; `R/core/query/expr/walk.go:1180` rejects unknown user functions.
- These gaps are required foundations for this range’s expanded macros and stored-query declarations. Their size does not remove them from the port list.
- **Already present:** ROW_NUMBER lowering, partitions, ascending-order restriction, and window `ef_search` at `walk.go:1267,1337`.
- **Required alignment:** target option typing/error handling and updated call-site semantics. Go’s `strconv.Atoi` handling is not the target’s declared integer option contract.
- `return_vectors` exists in the core call-site API; the scoped SQL grammar still exposes EF_SEARCH. Do not invent a new SQL spelling from the core API alone.

Preserve target failures still documented in macro fixtures, including issues **#4177** and **#4317**; they were not fixed by this range.

The removed higher-order function serialization route is a dependency for the core plan/protobuf audit. It is not proof that existing serialized window plans remain compatible.

**W6 — Stored queries, metadata round trips, startup cache warming, and metrics**

Upstream: `53daa4dee` / **#4157**, `02af41b57` / **#4291**, `9e65a9847` / **#4327**, with `b8bfcacbd` / **#4351** metadata cleanup.

Final syntax is **CREATE STORED QUERY**. Stored queries contain original SELECT text and an ordered list of temporary-function declarations. DECLARE bodies are rewritten into standalone temporary-function statements. Metadata builders, copies, serialization, and deserialization retain them.

Evidence: `A/api/metadata/StoredQuery.java:35`; `SchemaTemplate.java:139`; `C/recordlayer/metadata/RecordLayerSchemaTemplate.java:85,562,798`; serde serializer `:114`, deserializer `:126`; `visitors/DdlVisitor.java:541,893`.

Important API detail: `NoOpSchemaTemplate.getStoredQueries()` throws `INVALID_PARAMETER` at `:145`; it does not return an empty map.

Startup behavior:

- Load the relevant latest templates using a catalog transaction.
- Finish catalog access before offline planning.
- Compile each query’s temporary functions in sequence in a fresh per-query template context.
- Plan using default options and offline store state.
- One bad function/query does not abort startup or suppress unrelated queries.
- Creating the template does not immediately warm its plans.

Evidence: `C/recordlayer/RecordLayerEngine.java:75`; `query/OfflineStoredQueriesProcessor.java:88,121,176,202`.

The metric collector becomes usable with either an FDB context or a registry. It clocks failures as well as successes, converts timer units appropriately, and supplies startup processing/failure counters. Evidence: `C/recordlayer/metric/StoreTimerMetricCollector.java:79,94,102,115`; `api/metrics/RelationalMetric.java:109,144`.

Go has a plan cache and timers but no stored-query model/startup workflow: `R/api/metadata.go:127`; `R/core/metadata/schema_template.go:181`; `R/core/embedded/plan_cache.go:27`; `pkg/recordlayer/store_timer.go`.

**Required new port**, dependent on W5 and metadata generation.

A particularly important pin-generation dependency:

- Java metadata adds `stored_queries = 16`; each stored query has name `1`, SQL `2`, temporary functions `3`.
- Baseline Go already preserves unknown metadata bytes at `pkg/recordlayer/metadata_proto.go:195,419`.
- Once regenerated Go protobufs recognize field 16, it stops being unknown. It must then be explicitly modeled or carried in `FromProto`/`ToProto`, alongside the existing function/view preservation at `:186,405`.

Thus, protobuf regeneration without corresponding preservation work can turn an initially preserved field into a dropped field. Require a Java-authored stored-query metadata round trip.

**W7 — Explicit schema-existence policies and header-aware DROP**

Upstream: `944e06df9` / **#4363**, `ab19a6a93` / **#4354**.

`SchemaExistsBehavior` introduces four distinct contracts:

| Policy | Existing schema behavior |
|---|---|
| `ERROR` | Reject |
| `ERROR_IF_DIFFERENT` | No-op only for the same template name/version |
| `DO_NOTHING` | Preserve existing binding |
| `UPGRADE` | Same template name; reject downgrade; equal version no-op; newer version writes |

Database/template validation still occurs before a no-op decision. Template identity here is name/version, not structural equality.

Evidence: `C/api/catalog/SchemaExistsBehavior.java:46,57,76,86`; `recordlayer/catalog/RecordLayerStoreCatalog.java:189,232,278`; `query/CopyPlan.java:454`.

Catalog initialization uses `ERROR_IF_DIFFERENT`; creation uses `ERROR`; repair uses `UPGRADE`; COPY adopts the corresponding existence policy and `SCHEMA_ALREADY_EXISTS` failure. The tests exercise concurrent initializations and no-op saves, so unnecessary writes are part of the behavioral change.

Go’s `R/api/catalog.go` still has the older save API. `R/core/catalog/fdb_store_catalog.go:229` writes through the existing path; `:305` implements evolution/monotonicity validation, which is not a substitute for these four policies. The in-memory catalog at `:75` already uses overwrite semantics rather than Java’s old duplicate append, but still needs policy selection.

**Required port:** explicit policy through all callers, including initialization, repair, and eventual COPY support. Additional Go evolution checks must be considered separately in the RFC.

DROP switches to header-aware asynchronous store deletion and metadata-version-stamp handling at `C/recordlayer/ddl/DropSchemaConstantAction.java:66`. Go calls `DeleteStore` at `R/core/ddl/drop_schema.go:55`, whose baseline implementation is a range clear at `pkg/recordlayer/store_api.go:145`. The header/cache-invalidation semantics require coordination with the record-store audit. The Java asynchronous method shape itself need not be copied.

**W8 — GuardiANN, vector options, engine preference, pending queues, and deferred maintenance**

Upstream: `87e3fb716` / **#4398**, `38c05809e` / **#4422**, `6d4bf06f0` / **#4423**, `1ce8c71dd` / **#4357**, `b9ceed781` / **#4418**, `6a685875a` / **#4293**; target fixture changes also reflect **#4614** and `24863e8af` / **#4604**.

This is a major required feature, not an optional DDL spelling.

The target exposes:

- HNSW and GuardiANN engine selection.
- A centralized option table with engine-specific applicability, duplicate checks, and typed parsing.
- Shared options: metric, RaBitQ enablement/bits, statistics probabilities, and statistics threshold.
- HNSW options: connectivity, construction search, `m_max`, `m_max_0`.
- Thirteen GuardiANN SQL options: primary-cluster minimum/hard maximum/maximum; under-replicated maximum; replicated write maximum/target; replication priority; insert/delete candidate maxima; split/merge neighbor counts; reassignment neighbor count; collapse minimum duplicates.
- Connection-level engine preference: no preference, prefer HNSW, prefer GuardiANN. It breaks otherwise comparable choices; it is not an eligibility filter.
- Preference participates in planner-configuration equality/hash.
- Automatic merge during commit.
- `WRITE_ONLY_WITH_QUEUE` store-state plumbing.

Evidence: `C/recordlayer/query/visitors/DdlVisitor.java:86,95,120,407,424`; `A/api/Options.java:176,300`; `C/recordlayer/query/PlannerConfiguration.java:131,153`; `storage/BackingRecordStore.java:240`; `ddl/RecordLayerSetStoreStateConstantAction.java:89`.

Go status:

- HNSW and the separate Go SPFRESH extension exist at `R/core/embedded/ddl.go:358`.
- HNSW `m_max` and `m_max_0` are already mapped at `:439`; they are not missing Go features.
- Existing legacy option spelling is already correct for the metric: `pkg/recordlayer/vector_index_maintainer.go:23` uses `hnswMetric`.
- **GuardiANN is absent from the actual maintainer dispatch:** `pkg/recordlayer/store.go:1250` selects HNSW for the vector index type; SPFRESH has a separate branch at `:1278`.
- `pkg/recordlayer/index_state.go:17,62,211` lacks the queued write-only state.
- New preference must enter both planner construction and cache identity through `R/core/embedded/planner_options.go:74,142`.

SPFRESH is not GuardiANN parity. Opening Java GuardiANN metadata through the HNSW dispatch is a compatibility hazard that must be addressed before claiming interoperability.

Required dependencies include the actual GuardiANN engine, index mutations/deletes, queue writing/draining during online builds, and deferred split/merge maintenance. Core storage/policy details are owned by the corresponding other researchers; this report establishes their relational entry points and required integration.

Compatibility limits are explicit upstream:

- `Y/vector-mixed-version-metadata.yamsql:20` tests INSERT-only metadata compatibility and preservation of legacy `hnsw*` option names.
- At `:29`, it expressly excludes KNN SELECT continuation/plan-hash compatibility.
- GuardiANN fixtures are target-version gated because their persisted layout evolved.
- Small single-cluster result fixtures do not prove general ANN behavior.

**W9 — Index-definition generation and enum predicate handling**

Upstream: `a629befaf` / **#4525**, `181112c4a` / **#4624**.

The materialized-view index generator is substantially reorganized around an index specification, resolved quantifiers, projection resolution, and a value visitor. The rewrite’s stated intent is preservation of supported behavior; class additions alone are not evidence of newly added SQL capabilities.

The source-reviewed contracts include:

- Supported scan/filter/group/sort structure and rejection boundaries.
- Resolution of quantifier-derived values before key-expression construction.
- Preservation of independent versus shared unnest identities.
- Field-path and fan-out key construction.
- Grouping-key placement and ordering requirements.
- One aggregate per supported aggregate index.
- COUNT variants, SUM, extrema, bitmap aggregation, and legacy versus tuple extrema storage.
- Predicate conversion and supported residual predicate forms.
- Key versus included-value layout.

Evidence: `C/recordlayer/query/ddl/IndexSpec.java:82,164,220,342`; `QuantifierValues.java:67,144`; `ValueToKeyExpressionVisitor.java:131,178,284,419,454`; new visitor tests cover exact generated expressions.

Go already has real index-generation machinery: `R/core/query/ddl/generator.go:62,129,186`; `generator_aggregate.go:39,71,107`; `generator_predicate.go`; `R/core/embedded/index_onsource.go:24`.

**Required:** demonstrate target-equivalent metadata for overlapping supported definitions. Preserve exact key-expression protobuf structure, fan-out correlation, grouping, covering split, and extrema storage options. Do not regenerate persisted index definitions indiscriminately.

Go’s restricted decomposition of derived/unnested index sources is a **pre-existing gap**. It remains a prerequisite where new/shared SQL fixtures depend on those definitions; it cannot be dismissed because the Java rewrite is large.

The enum comparison fix prevents DDL-time evaluation using an inadequate type repository. `Y/enum-distinct-from-function.yamsql` is the scoped regression. Go already supports generic enum promotion at `R/core/query/expr/expr.go:912`, while SQL enum/function DDL is declined at `R/core/embedded/ddl.go:300`. This is a new regression contract sitting behind pre-existing DDL gaps.

`RecordLayerIndex.from` avoiding an unnecessary whole-index serialization is a behavior-preserving optimization. `RecordLayerTable` also corrects the error-message typo `UNKNONW` to `UNKNOWN`.

**W10 — Statement options and snapshot SELECT execution**

Upstream: `14d6445a9` / **#4362**, `7cb2f5ec8` / **#4364**, `45dbfdf7f` / **#4603**.

Options move from query terms to statement boundaries: SELECT, DML, EXPLAIN, and EXECUTE CONTINUATION. Statement-level EF_SEARCH disappears; window EF_SEARCH remains.

`ISOLATION_LEVEL_SNAPSHOT` is a connection/query option, default false. Java validates it before cache reuse, permits it only for appropriate read-only queries, and applies it to execution properties. COPY continuations and mutation/DDL paths are rejected.

Evidence: parser grammar `:398,591,690`; `A/api/Options.java:232,337`; `C/recordlayer/query/PlanGenerator.java:171,300,507`; `QueryPlan.java:503`.

Required semantics:

- Snapshot scans share the transaction’s read version and see its own writes.
- They do not add normal read conflict ranges.
- Other reads/writes retain their configured semantics.
- Isolation is chosen for each execution; the snapshot option is **not inherited through serialized continuations**.
- Cached plan execution settings must not leak between calls.

Java JDBC and embedded connections now accept only SERIALIZABLE transaction isolation. Snapshot is not an alternative `setTransactionIsolation` level.

Go status:

- **Already present:** Default/Serializable transaction acceptance at `R/core/embedded/connection.go:766`.
- **Already present:** lower-level snapshot scan support at `pkg/recordlayer/scan_properties.go:62`, `key_value_cursor.go:470`, and index-scan plumbing.
- **Required:** relational option, statement grammar/visitors, validation, and per-execution propagation. `R/core/embedded/cascades_generator.go:1956` does not apply the new option.
- **Pre-existing gap:** SQL continuation import/export at `cascades_generator.go:1343`; private internal paging does not prove Java continuation compatibility.

Required tests include the target’s conflict-producing concurrent scenarios, with a reader-side write that makes conflict behavior observable; joins, unions, aggregate/count/extrema indexes; read-your-writes; stable read version; cached executions; and explicit/default behavior on resume.

**W11 — UUID conversion, struct metadata, JDBC/gRPC changes**

Upstream: `c59085a6b` / **#4243**, `851712f14` / **#4171**, **#4423**, **#4364**, **#4603**, `f05475d89` / **#4485**.

UUID conversion now handles UUID-valued fields supplied through `RelationalStruct`, including nested structures and arrays. Evidence: `C/recordlayer/RecordTypeTable.java:232`, `RecordTypeTableSerDeTest`, and the nested unique-index regression in `UniqueIndexTests`.

Go already implements the UUID two-word protobuf representation and recursive conversion at `R/core/functions/proto_value.go:41,165,336,379`. That is substantial existing coverage, but SQL conversion does not establish every direct `api.Struct` route. Require direct/nested/array UUID round trips and nested unique-index enforcement.

gRPC metadata changes:

- `ListColumnMetadata` becomes `StructMetadata`.
- Existing repeated-column field number `1` remains.
- Optional type name uses field `2`.
- Existing enclosing field numbers remain unchanged.
- Missing names fall back to `ANONYMOUS`.

Evidence: assigned `column.proto`, `result_set.proto`, and `TypeConversion.java:282,516,524`. This is a generated Java API rename with an additive protobuf field, not a blanket binary wire break.

New JDBC option fields carry vector preference at field `34` and isolation at `35`; explicit false/default values and unknown enum handling matter. Evidence: `jdbc.proto:173,181`; `TypeConversion.java:737,820,894`.

Go has no corresponding relational JDBC/gRPC service implementation in the baseline. Those transport implementation changes have **no direct Go component analogue**. Shared result metadata still matters: local struct type names exist at `R/recordlayer/rowstruct/rowstruct.go:176`, while the SQL-driver nested metadata gap is explicitly documented at `R/conformance/javacorpus/ledger.go:81`.

The server reflection endpoint changes from the alpha API to v1 at `RelationalServer.java:162`. CLI isolation defaults and commons-cli formatting changes are Java client concerns.

**W12 — Corpus harness, correction tooling, JSON descriptor options, and artifact integrity**

Upstream: `679b00ae6` / **#4434**, `b53b78741` / **#4439**, `b24efd374` / **#4460**, `6e3ba69ea` / **#4462**, `6dfe34ddc` / **#4247**, `5f7ae64ad` / **#4386**, `b56a128f7` / **#4540**, and setup gating introduced with **#4318**.

Java changes include:

- Separate file and metric maintainers.
- Eager resource loading, synchronized/deduplicated corrections, and ordered edits.
- Explain correction also updates metric artifacts.
- Exact explain checks require matching metric entries; correction flags do not universally bypass missing metrics.
- Five counters are compared independently of timing noise.
- Metrics are saved before corrected source files.
- Duplicate binary metric identifiers are detected; YAML locations remain available.
- A custom result printer replaces asciitable, preserving whitespace and handling empty/ragged results.
- Percent-change analysis and histograms are added.
- JSON metadata loading restores custom protobuf `FieldOptions` extensions, including dependent descriptors and nested fields.

Evidence: Java YAML `YamlFilesMaintainer`; `YamlMetricsMaintainer.java:75,149,190,234`; `CheckExplainConfig.java:92,207,267`; `CommandUtil.java:166`; `MetricsStatistics`; `Matchers`.

Concrete Go port obligations:

1. Add the three new LIKE error-name mappings. They are absent from `R/conformance/javayamsql/errorcodes.go:12`.
2. Implement setup-block supported-version gating. Target `SetupBlock.java:95` checks it before connection options. Baseline `R/conformance/javayamsql/parse.go:332,339` recognizes only `connection_options`; unknown keys become inert at `:181`.
3. Carry enum-valued connection options through the existing parser/runner paths.
4. Update manifest, polarity, version, and census entries individually for newly assigned corpus files and changed expectations.
5. Retain explicit accounting for prepared statements, temporary functions, COPY, nested metadata, and continuations until implemented.
6. Treat JSON extension restoration as a separate import capability. Existing Go binary descriptor preservation at `pkg/recordlayer/metadata_proto.go:66` is useful, but does not prove JSON import parity.

Java-format explain/plan-hash assertions remain explicitly declined by the existing Go ledger at `R/conformance/javacorpus/ledger.go:34`. This audit does not propose a new renderer or correction framework.

Binary review results:

- Base: **1,180 entries**, **3,085,022 bytes** across existing assigned binaries.
- Target: **1,205 entries**, **3,172,461 bytes**.
- **54 added and 29 removed entries**.
- Among shared entries: **331 explain changes**, **1,151 counter/timer changes**, **494 DOT-string changes**.

Target artifact inconsistencies remain:

- `scoped-keyset-pagination`: the binary contains a parameterized explain while YAML contains a NULL-bound explain for the shared normalized identifier. Both query forms appear in the YAMSQL file at `:58,69`.
- `valid-identifiers`: **16 explain mismatches**, plus **three task-count and three task-time mismatches**, between binary and YAML. Some binary explains retain encoded storage names while YAML uses display names.

These are artifact inconsistencies, not proof of wrong query results. They make an indiscriminate golden refresh especially inappropriate.

**W13 — Planner changes visible through this population**

Upstream: `f76213e1e` / **#4382**, `3afcd0de2` / **#4461**, `f4b2283b2` / **#4394**, `1ad782c8d` / **#4417**, `7cefc75de` / **#4598**, `296c13989` / **#4468**, `545efe8f0` / **#4437**, `f4bc3c33a` / **#4302**.

The scoped history and artifacts expose important dependencies:

- Select merge and predicate pushdown move to final expressions.
- Merge is attempted before conditional pushdown.
- Their scheduling moves after child optimization/pruning.
- Finalization enumerates combinations of child expression partitions.
- Decorrelation and simplification become conditionally ordered.
- `IS NOT DISTINCT FROM` becomes usable as an index equality condition.
- Aggregate continuation serialization removes the temporary old/new mode.
- Explain output uses display names consistently and fixes the subscript closing bracket.

Scoped evidence includes `ExplainTests.java:159`, `Y/distinct-from.yamsql`, `Y/scoped-keyset-pagination.yamsql:58`, `Y/valid-identifiers.yamsql`, and `Y/array-join-at.yamsql:208`.

Go mechanisms are present, so absence cannot be inferred from changed Java class names:

- `P/default_rules.go:146,396` registers existing merge, pushdown, and decorrelation rules separately.
- `P/rule_predicate_push_down.go:60,118` processes individual children and their members.
- `P/rule_select_merge.go:48` documents the different Go phase placement.
- `P/predicates/comparison_range.go:266` already supports null-safe equality ranges.
- `pkg/recordlayer/query/executor/scan_range_binding.go:345,639,719` already handles null-safe binding.

**Required coordination:** target scheduling/partition algorithm parity with the planner group, preserving Go’s existing semantic barriers and authorized extensions. Metric churn alone is not evidence of a demonstrated wrong-result bug.

Null-safe index equality is **already implemented in relevant Go mechanisms**. Require the new parameterized/NULL/scoped-keyset regressions, including descending suffix and union branches, before marking the end-to-end contract verified.

Display-name and bracket corrections do not imply changes to persisted storage names or key encoding.

**N — Java/build-only and mechanical changes**

The assigned Gradle files, JUnit migration, task/configuration-cache changes, dependency updates, shadow packaging, Commons replacements, annotation cleanup, and new JMH benchmarks were read. Relevant upstream changes include **#4282, #4283, #4294, #4297, #4356, #4365, #4367, #4368, #4378, #4383, #4388, #4483, #4485, #4512**.

They do not justify Go engine ports or a new performance campaign. Build/pin integration remains controller-owned. Mechanical Java helper changes include the cause-preserving Assert overload, duration-based cache API calls, import substitutions, and test-fixture plumbing. The exhaustive ledger distinguishes these from behavioral tests.

**3. Regression and compatibility decisions**

The following expectation changes are concrete and must be reviewed individually:

| Existing Go expectation/mechanism | Required treatment |
|---|---|
| Hash-comment stripping and malformed block-comment normalization | Align lexer/cache contracts; replace obsolete expectations |
| Adjacent string literals processed through whole-context text | Decode token-by-token; retain single-token escape tests |
| LIKE newline, dangling escape, non-meta escape, wildcard-as-escape pins | Adopt target semantics and exact SQLSTATEs |
| LIKE regex-based fuzz/reference oracle | Replace the obsolete semantic oracle without weakening assertions |
| SQL array NULL rejection as `XX000`/old message | Retain rejection; assert target classification |
| COALESCE with non-null arguments expected nullable | Assert non-null result type |
| COUNT metadata treated uniformly at every layer | Separate raw/grouped aggregate type from adjusted ungrouped SQL output |
| Structured Promote accepted defect | Re-arm positive success tests and close the defect |
| Setup `supported_version` recorded as inert | Implement gating and remove only the corresponding obsolete inert ledger entries |

Required compatibility evidence after implementation:

- Java-authored metadata through Go read/write round trips, particularly newly known field 16.
- Named struct metadata through nested arrays and driver conversion.
- Existing HNSW option spellings and engine dispatch.
- Queued index-state transitions and online-build mutation handling.
- Exact index key-expression metadata for supported DDL.
- ARRAY_AGG partial state across scan limits, including empty intermediate pages.
- Snapshot choice on each continuation execution.
- Explicit treatment of changed vector/window/LIKE compiled-plan representations; no blanket cross-version continuation claim.

The target still contains known Java failures such as issue #4573 and the cited macro limitations. Record these accurately; do not weaken successful Go assertions to imitate them.

The existing five restricted hunts remain unapproved. No new bug-hunt campaign or unrelated performance work was started or proposed.

**4. Prioritized concrete port list**

All rows require RFC design review before implementation.

| Priority | Work | Coupling and completion condition |
|---|---|---|
| P0 | Close controller-owned mixed-array and structured Promote defects | W2; prerequisites for typed arrays, macro returns, nested values, and ARRAY_AGG |
| P0 | Port comment/literal/cache and LIKE contracts | W1–W3; update exact obsolete tests and SQLSTATE mappings |
| P0 | Preserve new metadata through protobuf regeneration | W6; field 16 must survive once recognized |
| P0 | Prevent wrong-engine handling of GuardiANN metadata and recognize queued state | W8; coordinate engine, metadata, and online-indexing groups |
| P1 | Implement ARRAY_AGG end to end | W4 plus core value/plan/protobuf/cursor work; bounded state and continuation tests required |
| P1 | Correct COALESCE and whole-record aggregate semantics | W4; preserve raw COUNT typing and ambiguity behavior |
| P1 | Implement function/view foundations and range-expanded macro/call-site contracts | W5; named/default arguments, typing, serialization, temporary functions |
| P1 | Implement stored-query metadata and startup planning | W6 after W5; failure isolation, cache use, and counters |
| P1 | Implement full GuardiANN relational integration | W8; options, preference/cache identity, mutations, queues, deferred maintenance |
| P1 | Port schema-existence policies and DROP invalidation | W7; caller selection, no-op/concurrency semantics, header-aware invalidation |
| P1 | Expose snapshot SELECT and statement options | W10; execution-local settings, guards, conflict tests, resume semantics |
| P1 | Establish target index-definition fidelity | W9; close required pre-existing nested/unnest/enum DDL gaps |
| P1 | Coordinate planner scheduling and partition changes | W13; source algorithm alignment and scoped result regressions |
| P2 | Complete harness/API regression integration | W11–W12; setup gates, metadata assertions, explicit new-file classification and census changes |

P2 is sequencing, not permission to omit the work. Java-only transport/build changes should be recorded as such rather than converted into invented Go infrastructure.

**5. Exhaustive path ledger**

Every assigned path is accounted for below. Prefix expansion is literal. Codes refer to the work units above:

- `N`: source-reviewed Java/build/support change with no independent Go runtime port.
- `F`: source-reviewed fixture maintenance/expansion or aged-version cleanup; no production behavior inferred from changed rows alone.
- `T`: added regression coverage for existing behavior.
- `M`: metric artifact individually read/decoded and compared under W12/W13. **M does not mean mechanical or runtime-verified.**

The seven build paths:

```text
N fdb-relational-api/fdb-relational-api.gradle
N fdb-relational-cli/fdb-relational-cli.gradle
N fdb-relational-core/fdb-relational-core.gradle
N fdb-relational-grpc/fdb-relational-grpc.gradle
N fdb-relational-jdbc/fdb-relational-jdbc.gradle
N fdb-relational-server/fdb-relational-server.gradle
N yaml-tests/yaml-tests.gradle
```

API main Java — prefix `fdb-relational-api/src/main/java/com/apple/foundationdb/relational/` — **7 paths**:

```text
W8,W10 api/Options.java
W3     api/exceptions/ErrorCode.java
W10    api/fluentsql/statement/StructuredQuery.java
N      api/metadata/DataType.java
W6     api/metadata/SchemaTemplate.java
W6     api/metadata/StoredQuery.java
W8,N   util/Assert.java
```

API test fixtures — prefix `fdb-relational-api/src/testFixtures/java/com/apple/foundationdb/relational/` — **1 path**:

```text
W8,W10,W12 utils/OptionsTestHelper.java
```

CLI — **3 paths**:

```text
W10,N fdb-relational-cli/src/main/java/com/apple/foundationdb/relational/cli/sqlline/Customize.java
N     fdb-relational-cli/src/main/java/com/apple/foundationdb/relational/cli/sqlline/RelationalSQLLine.java
N     fdb-relational-cli/src/test/java/com/apple/foundationdb/relational/cli/SQLLineTest.java
```

Core grammar — **2 paths**:

```text
W1,W4,W8          fdb-relational-core/src/main/antlr/RelationalLexer.g4
W4,W5,W6,W8,W10   fdb-relational-core/src/main/antlr/RelationalParser.g4
```

Core benchmarks — prefix `fdb-relational-core/src/jmh/java/com/apple/foundationdb/relational/` — **3 paths**, new Java benchmark infrastructure; no benchmark was run:

```text
N recordlayer/BenchmarkConnHolder.java
N recordlayer/DirectAccessVsQueryBenchmark.java
N recordlayer/IndexScanVsQueryBenchmark.java
```

Core main Java — prefix `fdb-relational-core/src/main/java/com/apple/foundationdb/relational/` — **64 paths**:

```text
W7        api/catalog/SchemaExistsBehavior.java
W7        api/catalog/StoreCatalog.java
W6        api/metrics/RelationalMetric.java
N         recordlayer/ArrayRow.java
W6,W10    recordlayer/EmbeddedRelationalConnection.java
N         recordlayer/QueryExecutor.java
W6        recordlayer/RecordLayerEngine.java
N         recordlayer/RecordLayerStorageCluster.java
W11       recordlayer/RecordTypeTable.java
W7        recordlayer/catalog/RecordLayerStoreCatalog.java
W7        recordlayer/ddl/DropSchemaConstantAction.java
W7        recordlayer/ddl/RecordLayerCreateSchemaConstantAction.java
W8        recordlayer/ddl/RecordLayerSetStoreStateConstantAction.java
W2        recordlayer/metadata/DataTypeUtils.java
W6        recordlayer/metadata/NoOpSchemaTemplate.java
W9,N      recordlayer/metadata/RecordLayerIndex.java
W6        recordlayer/metadata/RecordLayerSchemaTemplate.java
W9,N      recordlayer/metadata/RecordLayerTable.java
W6,N      recordlayer/metadata/serde/RecordMetadataDeserializer.java
W6        recordlayer/metadata/serde/RecordMetadataSerializer.java
W6        recordlayer/metric/RecordLayerMetricCollector.java [deleted]
W6        recordlayer/metric/StoreTimerMetricCollector.java
W1,W2,W10 recordlayer/query/AstNormalizer.java
W7,W10    recordlayer/query/CopyPlan.java
W4        recordlayer/query/Expression.java
W4,W5     recordlayer/query/Expressions.java
W4,W5     recordlayer/query/LogicalOperator.java
W6        recordlayer/query/OfflineStoredQueriesProcessor.java
W8        recordlayer/query/OptionsUtils.java
W4,W5     recordlayer/query/OrderByExpression.java
W6,W10    recordlayer/query/PlanGenerator.java
W8        recordlayer/query/PlannerConfiguration.java
W1        recordlayer/query/QueryParser.java
W10       recordlayer/query/QueryPlan.java
W1,W2,W4,W5 recordlayer/query/SemanticAnalyzer.java
W5        recordlayer/query/WindowSpecExpression.java
N         recordlayer/query/cache/MultiStageCache.java
W9        recordlayer/query/ddl/ExtremumEverStorage.java
W9        recordlayer/query/ddl/IndexGenerationOptions.java
W9        recordlayer/query/ddl/IndexGenerator.java
W9        recordlayer/query/ddl/IndexPredicates.java
W9        recordlayer/query/ddl/IndexSpec.java
W9        recordlayer/query/ddl/MaterializedViewIndexGenerator.java
W9        recordlayer/query/ddl/OnSourceIndexGenerator.java
W9        recordlayer/query/ddl/ProjectionResolver.java
W9        recordlayer/query/ddl/QuantifierValues.java
W9        recordlayer/query/ddl/ValueToKeyExpressionVisitor.java
W5        recordlayer/query/functions/CompiledSqlFunction.java
W5        recordlayer/query/functions/SqlFunctionCatalog.java
W4,W5     recordlayer/query/functions/SqlFunctionCatalogImpl.java
W5        recordlayer/query/functions/UserDefinedFunctionBuilder.java
W5        recordlayer/query/functions/UserDefinedFunctionCatalog.java
W5        recordlayer/query/functions/UserDefinedMacroFunctionBuilder.java
W4,W5,W10 recordlayer/query/visitors/BaseVisitor.java
W5,W6,W8,W9 recordlayer/query/visitors/DdlVisitor.java
W4,W5,W6,W8,W10 recordlayer/query/visitors/DelegatingVisitor.java
W1,W2,W3,W4,W5 recordlayer/query/visitors/ExpressionVisitor.java
N         recordlayer/query/visitors/IdentifierVisitor.java
W1,W4     recordlayer/query/visitors/QueryVisitor.java
W4,W5,W10 recordlayer/query/visitors/TypedVisitor.java
W8        recordlayer/storage/BackingRecordStore.java
W10       recordlayer/structuredsql/statement/UpdateStatementImpl.java
W3        recordlayer/util/ExceptionUtil.java
W7        transactionbound/catalog/HollowStoreCatalog.java
```

Core tests — prefix `fdb-relational-core/src/test/java/com/apple/foundationdb/relational/` — **47 paths**:

```text
W10       api/RelationalConnectionTest.java
W6        api/ddl/DdlStatementParsingTest.java
W8,W9     api/ddl/IndexTest.java
W5        api/ddl/SqlFunctionTest.java
N         autotest/engine/AutoTestEngine.java
N         autotest/engine/WorkloadTestDescriptor.java
W7        memory/InMemoryCatalog.java
W7        memory/InMemoryRelationalConnection.java
N         recordlayer/AbstractRecordLayerResultSetTest.java
W1        recordlayer/CaseSensitiveDbObjectsTest.java
W7,W10    recordlayer/CopyCommandTest.java
W6        recordlayer/EmbeddedRelationalExtension.java
N         recordlayer/KeySpacePathParsingTest.java
W10       recordlayer/OptionScopeTest.java
W1        recordlayer/QueryLoggingTest.java
W11       recordlayer/RecordTypeTableSerDeTest.java
W11       recordlayer/UniqueIndexTests.java
W7        recordlayer/catalog/CatalogMetaDataProviderTest.java
W7        recordlayer/catalog/RecordLayerStoreCatalogImplTest.java
W7        recordlayer/catalog/RecordLayerStoreCatalogTestBase.java
W7        recordlayer/catalog/RecordLayerStoreCatalogWithNoTemplateOperationsTest.java
W6        recordlayer/metadata/NoOpSchemaTemplateTests.java
W6        recordlayer/metric/StoreTimerMetricCollectorFromFDBRecordContextTest.java
W6        recordlayer/metric/StoreTimerMetricCollectorFromMetricRegistryTest.java
W1,W2     recordlayer/query/AstNormalizerTests.java
N         recordlayer/query/DelegatingVisitorTest.java
W13       recordlayer/query/ExplainTests.java
W4,W5     recordlayer/query/ExpressionTests.java
W13,N     recordlayer/query/ForceContinuationQueryTests.java
W4        recordlayer/query/GroupByQueryTests.java
W2        recordlayer/query/InListNullParameterTest.java
W2        recordlayer/query/NullParameterParityTest.java
W1,W4     recordlayer/query/QueryParserTests.java
W4        recordlayer/query/QueryTestUtils.java
W10       recordlayer/query/SnapshotIsolationConcurrencyTest.java
W2,N      recordlayer/query/StandardQueryTests.java
W6        recordlayer/query/StoredQueriesTest.java
W1        recordlayer/query/StringLiteralNormalizationTest.java
N         recordlayer/query/UpdateTest.java
W1        recordlayer/query/cache/ConstraintValidityTests.java
N         recordlayer/query/cache/MultiStageCacheTests.java
W1        recordlayer/query/cache/RelationalPlanCacheTests.java
W9        recordlayer/query/ddl/ValueToKeyExpressionVisitorTest.java
W5        recordlayer/query/functions/UserDefinedMacroFunctionBuilderTest.java
W7        recordlayer/storage/BackingLocatableResolverStoreTest.java
N         utils/InMemoryTransactionManager.java
W10,N     utils/RelationalResultSetAssert.java
```

For the `N` test entries above, the reviewed changes are dependency/import substitutions, JUnit discovery/API adaptation, removed redundant overrides, or helper plumbing. `StandardQueryTests` is explicitly mixed: its new array tests are behavioral and were not classified as mechanical.

gRPC main Java — prefix `fdb-relational-grpc/src/main/java/com/apple/foundationdb/relational/` — **5 paths**:

```text
W11        jdbc/RelationalArrayFacade.java
W11        jdbc/RelationalResultSetFacade.java
W11        jdbc/RelationalResultSetMetaDataFacade.java
W11        jdbc/RelationalStructFacade.java
W8,W10,W11 jdbc/TypeConversion.java
```

gRPC protobuf — prefix `fdb-relational-grpc/src/main/proto/grpc/relational/jdbc/v1/` — **3 paths**:

```text
W11        column.proto
W8,W10,W11 jdbc.proto
W11        result_set.proto
```

gRPC tests — prefix `fdb-relational-grpc/src/test/java/com/apple/foundationdb/relational/` — **2 paths**:

```text
W8,W10,W11 jdbc/ProtobufConversionTest.java
W11        jdbc/RelationalResultSetMetadataFacadeTest.java
```

JDBC — **3 paths**:

```text
W10 fdb-relational-jdbc/src/main/java/com/apple/foundationdb/relational/jdbc/JDBCRelationalConnection.java
W10 fdb-relational-jdbc/src/main/java/com/apple/foundationdb/relational/jdbc/JDBCRelationalDatabaseMetaData.java
W10 fdb-relational-jdbc/src/test/java/com/apple/foundationdb/relational/jdbc/JDBCRelationalConnectionTest.java
```

Server — **2 paths**:

```text
W11 fdb-relational-server/src/main/java/com/apple/foundationdb/relational/server/RelationalServer.java
W11 fdb-relational-server/src/test/java/com/apple/foundationdb/relational/server/RelationalServerTest.java
```

YAML harness main Java — prefix `yaml-tests/src/main/java/com/apple/foundationdb/relational/yamltests/` — **19 paths**:

```text
W12   Matchers.java
W12   MetricsDiffAnalyzer.java
W12,N MetricsInfo.java
W12   MetricsStatistics.java
W12   YamlCorrection.java
W12   YamlExecutionContext.java
W12   YamlFilesMaintainer.java
W12   YamlMetricsMaintainer.java
N     block/IncludeBlock.java
W12   block/SetupBlock.java
W12   block/TestBlock.java
N     command/Command.java
W12   command/CommandUtil.java
W12   command/QueryCommand.java
N     command/QueryConfig.java
N     command/QueryExecutor.java
N     command/QueryInterpreter.java
W12   command/queryconfigs/CheckExplainConfig.java
W12   command/queryconfigs/CheckResultMetadataConfig.java
```

Here `IncludeBlock` changes an annotation; `Command` changes static helper invocation; `QueryConfig`/`QueryExecutor` use equivalent pattern matching; `QueryInterpreter` changes Pair/static helper usage. These are specifically reviewed mechanical classifications.

YAML test Java — **9 paths**:

```text
W12 yaml-tests/src/test/java/CheckExplainTest.java
W1,W3,W4,W10 yaml-tests/src/test/java/DocumentationQueriesTests.java
W12 yaml-tests/src/test/java/SupportedVersionTest.java
W12 yaml-tests/src/test/java/YamlIntegrationTests.java
W12 yaml-tests/src/test/java/com/apple/foundationdb/relational/yamltests/CheckExplainConfigTest.java
W12 yaml-tests/src/test/java/com/apple/foundationdb/relational/yamltests/MetricsDiffAnalyzerTest.java
W12 yaml-tests/src/test/java/com/apple/foundationdb/relational/yamltests/MetricsStatisticsTest.java
W12 yaml-tests/src/test/java/com/apple/foundationdb/relational/yamltests/ResultSetPrettyPrinterTest.java
W12 yaml-tests/src/test/java/com/apple/foundationdb/relational/yamltests/YamlCorrectionUnitTest.java
```

YAML descriptor fixtures — **2 paths**:

```text
W12 yaml-tests/src/test/proto/custom_field_option.proto
W12 yaml-tests/src/test/resources/field-options-extension-metadata.json
```

YAMSQL — prefix `yaml-tests/src/test/resources/` — **67 paths**:

```text
W4        aggregate-empty-table.yamsql
W4        aggregate-index-tests-count-empty.yamsql
W4        aggregate-index-tests-count.yamsql
W4        array-agg-tests.yamsql
W13       array-join-at.yamsql
W2        arrays.yamsql
F         boolean-ddl.yamsql
F         boolean.yamsql
F         bytes.yamsql
W5        case-sensitivity.yamsql
F         case-when.yamsql
W12       check-explain/addExplain/add-explain.yamsql
W4        create-drop.yamsql
W4,W13    distinct-from.yamsql
W4        documentation-queries/array-agg-documentation-queries.yamsql
W1        documentation-queries/comments-documentation-queries.yamsql
W13       documentation-queries/is-distinct-from-operator-queries.yamsql
W10       documentation-queries/isolation-level-snapshot-documentation-queries.yamsql
W3        documentation-queries/like-operator-queries.yamsql
W5,W8     documentation-queries/window-function-documentation-queries.yamsql
W5,W9     enum-distinct-from-function.yamsql
T         exists-in-select.yamsql
W4        field-index-tests-proto.yamsql
W12       field-options-extension.yamsql
F         filter-index.yamsql
F         functions.yamsql
W4        groupby-tests.yamsql
W8        guardiann-semantic-search.yamsql
W2,F      in-predicate.yamsql
F         insert-enum.yamsql
W10       isolation-level-snapshot.yamsql
W2,W4,T   join-tests-outer.yamsql
T,F       join-tests.yamsql
W1        keyword-case-insensitivity.yamsql
W1,W3     like.yamsql
W1        literal-tests.yamsql
F         nested-tests.yamsql
W4        nested-with-nulls-proto.yamsql
W4        nested-with-nulls.yamsql
W4        null-operator-tests.yamsql
W4        primary-key-tests.yamsql
W4        record-type-key-tests.yamsql
F         scenario-tests.yamsql
W6        schema-template-stored-queries.yamsql
W13       scoped-keyset-pagination.yamsql
W1        select-without-from.yamsql
W5,W8     semantic-search-advanced-metrics.yamsql
W5,W8     semantic-search.yamsql
W1,F      showcasing-tests.yamsql
F         skipped-field-number-proto.yamsql
W5,W8     sliding-window-semantic-search.yamsql
W1        sql-comments.yamsql
W5,F      sql-functions.yamsql
W4        standard-tests-metadata.yamsql
W4        standard-tests-proto.yamsql
W4        standard-tests.yamsql
W1        struct-type-nullability-variants.yamsql
W12       supported-version/current-version-at-file-and-setup-block.yamsql
W12       supported-version/current-version-at-setup-block.yamsql
W4        union-empty-tables.yamsql
W4        union.yamsql
W2,W5     user-defined-macro-function-tests.yamsql
W13,F     valid-identifiers.yamsql
W8        vector-engine-preference.yamsql
W8        vector-mixed-version-metadata.yamsql
W2        vector.yamsql
W5,W13,F  versions-tests.yamsql
```

The ten F-only files above change through `545619e7c` / **#4197**, expanding data/results to make forced-continuation testing meaningful. The T entries add anti-join/NOT EXISTS coverage through **#4594**. Aged schema-template variants and version-floor updates are separately included in mixed F entries; they are not newly introduced engine capabilities.

Metric artifacts — prefix `yaml-tests/src/test/resources/` — **79 explicit pairs, 158 paths**.

For each exact stem below, the two assigned paths are literally:

```text
<stem>.metrics.yaml
<stem>.metrics.binpb
```

Both members of every pair were consumed. This notation compresses the ledger only; it does not classify semantic explain changes as mechanical.

```text
M aggregate-empty-table
M aggregate-index-tests-count-empty
M aggregate-index-tests-count
M aggregate-index-tests
M array-agg-tests
M array-join-at
M arrays-cardinality
M arrays-unnesting
M arrays
M between
M bitmap-aggregate-index
M cast-tests
M catalog
M check-explain/shouldPass/exact
M composite-aggregates
M create-drop
M cte
M distinct-from
M enum-distinct-from-function
M enum
M exists-in-select
M field-index-tests-proto
M filter-index
M groupby-tests
M guardiann-semantic-search
M in-predicate
M include-block/includes/include-scoped
M include-block/shouldPass/include-falls-back-to-global-available-uri
M include-block/shouldPass/include-prioritize-local-uri
M include-block/shouldPass/multiple-includes
M include-block/shouldPass/multiple-same-includes
M include-block/shouldPass/nested-includes
M include-block/shouldPass/simple-include-different-env
M include-block/shouldPass/simple-include
M index-ddl-aggregates-only
M index-ddl-values-only
M index-ddl
M indexed-functions
M insert-enum
M join-tests-outer
M join-tests
M join-with-order-by-tests
M like
M literal-tests
M nested-with-nulls-proto
M nested-with-nulls
M null-extraction-tests
M null-operator-tests
M orderby
M primary-key-tests
M pseudo-field-clash
M record-type-key-tests
M recursive-cte
M right-deep-plan-tests
M scoped-keyset-pagination
M select-a-star
M semantic-search-advanced-metrics
M semantic-search
M showcasing-tests
M simple-query-with-different-debuggers
M sql-functions
M standard-tests-metadata
M standard-tests-proto
M standard-tests
M struct-type-nullability-variants
M subquery-tests
M table-functions
M transaction-setup/shouldPass/double-query-metrics
M union-empty-tables
M union
M update-delete-returning
M user-defined-macro-function-tests
M uuid-non-prepared
M uuid-prepared
M valid-identifiers
M vector-engine-preference
M vector
M versions-tests
M versions-with-single-type-tests
```

Ledger accounting: **7 build + 8 API Java + 3 CLI Java + 2 grammars + 114 core Java + 10 gRPC + 3 JDBC + 2 server + 19 YAML main Java + 9 YAML test Java + 2 descriptor fixtures + 67 YAMSQL + 158 metric artifacts = 404 assigned paths.**