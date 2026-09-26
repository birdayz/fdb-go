This population contains substantive upgrade work. The clearest required changes are LIKE semantics, SQL comments and statement options, snapshot reads, `ARRAY_AGG`, stored-query metadata and warm-up, and the build/oracle dependency closure. The release notes also identify major storage and planner work that must remain in the combined audit, particularly pending-write queues and GuardiANN.

This is a source audit, **not runtime parity verification or an implementation ACK**. I made no edits and ran no builds or tests. Implementation must await RFC design review.

**1. Scope and consumption**

| Item | Audited reference |
|---|---|
| Go baseline | `e48f5b4965543cd4d99b5578356059e12d969c7c` |
| Java base | `4.12.11.0` → `257aa83cae7f90e18ea6595fdf2cf841ca72e802` |
| Java target | `4.14.2.0` → `fdacd162a9c8acfadc49082b89185c823ab8ae4a` |
| Exclusive input population | [build-docs.paths](/var/tmp/query-grind-cast/java-upgrade/audit/build-docs.paths) |
| Assigned paths | **126, all unique** |
| Net statuses | **39 added, 78 modified, 9 deleted** |
| Textual additions/deletions | **7,312 / 1,559**, excluding the binary jar |
| Complete scoped diff | **16,115 lines** |

I read the complete tag-to-tag diff using the requested command shape:

```text
git -C fdb-record-layer diff 4.12.11.0 4.14.2.0 -- <exact assigned paths>
```

The paths were supplied explicitly from the input list. Large outputs were split into manageable chunks; truncated portions were reread. In particular, the entire **6,092-line ReleaseNotes.md diff**, including historical edits, was consumed. I also read the scoped commit history and selected implementation commits to distinguish final changes from intermediate states and reverts.

Go findings below refer to `git show e48f5b496:<path>`, not potentially changing working-tree pins or restored tests.

The only unread assigned payload is the **binary implementation inside `gradle/wrapper/gradle-wrapper.jar`**: its binary replacement was accounted for, and wrapper configuration and launcher diffs were read, but the jar was not disassembled or executed. No assigned textual diff remains unread.

Implementation files outside these 126 paths were read where necessary to verify documentation claims. Those reads are limited corroboration, **not coverage claims for the other researchers’ populations**. Remaining release-note claims are explicitly handed to their implementation domains below.

For compact Java references:

- `JC/` means `fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/`.
- `JR/` means `fdb-relational-core/src/main/java/com/apple/foundationdb/relational/`.
- All Java line references are at the target unless another tag is stated.

**2. Work units and verified dispositions**

**W1 — Java toolchain, dependency resolution, and oracle classpath**

Relevant upstream changes include #4366 (`a8ce88f2e`), #4356, #4365, #4516, #4517, #4384, #4497 (`0d0c8cec4`), #4485 and #4556 (`15b4a5e55`).

Target [build.gradle:208](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/build.gradle:208) selects a **JDK 21 toolchain**, while lines 216–217 and 277–285 retain the **Java 17 source/API/bytecode target**. These are different requirements. `gradle.properties:41` enables the configuration cache, line 44 makes incompatibilities fatal, and line 47 disables automatic JDK provisioning.

The wrapper changes from **Gradle 9.5.1 to 9.7.1**. Target `gradle/wrapper/gradle-wrapper.properties:3` pins distribution SHA-256:

```text
92c1a136d76b5017732a66d2e0a648ebff00dd3687d8bff0d0047a1bd904fdf2
```

The complete dependency-version delta in `gradle/libs.versions.toml` is:

| Dependency | Base → target |
|---|---|
| Caffeine | 3.1.8 → 3.2.4; adds the Caffeine Guava adapter |
| Commons CLI | 1.5.0 → 1.11.0 |
| Dropwizard metrics | 4.2.28 → 4.2.39 |
| gRPC | 1.64.1 → 1.83.1 |
| Google common protos | 2.37.0 → 2.64.1 |
| Gson | 2.13.2 → 2.14.0 |
| Guava | 33.4.8-jre → 33.7.1-jre |
| ICU | 69.1 → 78.3 |
| JavaPoet | 1.12.0 → 1.13.0 |
| JTS | 1.16.1 → 1.20.0 |
| Log4j | 2.25.3 → 2.26.1 |
| Protobuf | 3.25.8 → 3.25.9 |
| AssertJ | 3.26.3 → 3.27.7 |
| Apache HTTP client | 5.2.1 → 5.6.4 |
| java-diff-utils | 4.12 → 4.17 |
| Hamcrest | 2.2 → 3.0 |
| JCommander | 1.81 → 3.0; coordinates change from `com.beust` to `org.jcommander` |
| JLine | 3.30.4 → 4.4.0 |
| JUnit | 5.14.1 → 6.1.3; platform version becomes unified with JUnit |
| Mockito | 3.7.7 → 5.23.0 |
| SnakeYAML | 2.2 → 2.6 |
| SpotBugs | 4.9.0 → 4.10.4 |

Removed catalog dependencies: `asciitable`, `h2`, `opencsv`, Commons Collections, and bndtools. The separate JUnit-platform version entry disappears because it uses the unified version.

Build plugins change as follows:

- Protobuf: 0.9.6 → 0.10.0.
- Shadow: 8.3.10 → 9.6.1.
- SpotBugs: 6.5.5 → 6.5.11.
- Git-version: 5.0.0 → 5.1.0.
- Nexus publishing plugin removed in favor of the Central Portal implementation.

ANTLR remains **4.13.2**; FoundationDB Java remains **7.1.26**, and `gradle.properties:34` retains test API version **710**. Those are not range-introduced upgrades.

Go baseline mappings:

- [MODULE.bazel:107](/home/birdy/projects/fdb-record-layer-go/MODULE.bazel:107): Bazel protobuf module `34.0.bcr.1`.
- `MODULE.bazel:117,122,123`: Record Layer core, relational API, and relational core pinned to 4.12.11.0.
- `MODULE.bazel:124–125`: independently declared Gson 2.10.1 and protobuf-java-util 4.29.3.
- `.bazelrc:11`: Java runtime already uses `remotejdk_21`.
- `go.mod:31`: Go protobuf runtime 1.36.11.
- `MODULE.bazel:150`: Go grammar generation uses ANTLR 4.13.1, a pre-existing mismatch with Java’s unchanged 4.13.2.

**Disposition:** required oracle/build integration, with several pre-existing dependency differences. Updating three Maven coordinates alone does not establish a coherent resolved classpath.

I checked the locally available target Record Layer POM and cached gRPC POMs. Record Layer core 4.14.2.0 declares protobuf-java **3.25.9**. Cached grpc-protobuf 1.83.1 also requests **3.25.9**; common-protos 2.64.1 requests **3.25.8**. Therefore, the proposition that this gRPC upgrade inherently forces protobuf 4.x is unsupported by these artifacts. The Go oracle’s existing protobuf-java-util 4.29.3 declaration is a separate classpath concern.

No dependency resolver was executed. The RFC should require inspection of the eventual resolved artifacts and generated-code/runtime combination, followed by focused validation. Dependency versions alone do not prove wire compatibility or a need to copy a Java library’s internal algorithms.

ICU deserves a separate compatibility disposition: Java `fdb-record-layer-icu/.../TextCollatorRegistryICU.java:79–102` creates ICU collators and persists their collation-key bytes. Go [collate_function_key_expression.go:13](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/collate_function_key_expression.go:13) explicitly uses `x/text/collate`, and lines 101–109 emit its keys. The existing code expressly identifies those keys as incompatible with Java/ICU. That is a **pre-existing shared-index compatibility gap**, now requiring a new target-version baseline; it must not be described as merely a Java build bump.

JTS changes affect the Java spatial module. Its `GeophileSpatial` and point-within-distance implementations import JTS geometry and parsing classes. The Go multidimensional index does not establish compatibility with that separate Geophile/JTS surface. Hand this dependency-sensitive behavior to the spatial/index domain; no specific geometric algorithm delta is proven by the version number alone.

**W2 — Generated sources, test-fixture relocation, packaging, and source consumers**

Relevant commits: #4297 (`80a499db8`), #4384, #4365, #4516.

Target [gradle/proto.gradle:31](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/gradle/proto.gradle:31):

- Uses protoc at the catalog protobuf version.
- Removes the old `protogen` output override.
- Adds main proto/include dependencies to non-main generators.
- Adds `src/testFixtures/proto` and extracted fixture include directories.
- Explicitly wires extraction tasks to avoid undeclared producer/consumer relationships under newer Gradle.
- Avoids making main generation depend on fixture output.

The associated `.gitignore`, Checkstyle, PMD, and SpotBugs changes follow the generated-source relocation. They do not change protobuf field numbers or record encoding.

Necessary target corroboration: `fdb-record-layer-core/fdb-record-layer-core.gradle:25–70` applies `java-test-fixtures`, exposes moved test-base signature dependencies through `testFixturesApi`, and configures fixture annotation processors.

The actual moved fixture files belong to other populations. I inspected the relocation commit’s structure and build consumers, but do not claim a complete content audit of those other paths. **None of the six assigned Java source/test paths is being dismissed as a mechanical fixture rename.**

Go baseline [conformance/BUILD.bazel:18](/home/birdy/projects/fdb-record-layer-go/conformance/BUILD.bazel:18) compiles local Java conformance sources against Maven main artifacts. Its local demo proto is generated separately. It does not import the relocated Java test bases through the assigned build mechanism.

The vendored acceptance corpus is a different input: `third_party/apple/fdb-record-layer/README.md` specifies verbatim `.yamsql` files from `yaml-tests/src/test/resources`, with its own version marker and census gate. Java test-base relocation is **not justification to remove or skip those acceptance files**.

`fdb-record-layer-core-shaded.gradle:71–129` moves unshaded dependency-coordinate collection to configuration time. The shaded dependency names remain Guava, protobuf-java, and fdb-extensions. This is packaging/configuration-cache work, not evidence of a new record wire format.

**Disposition:** Java build-only for the relocation mechanics; required adaptation for any source-based oracle or fixture consumer; required deliberate corpus/version synchronization for this repository. Do not copy fixture protos into production schemas or assume old `protogen` locations remain valid.

**W3 — Visitor generation and test execution environment**

The annotation-processor change is substantive tooling behavior, not formatting. #4525 (`a629befaf`) changes `GenerateVisitorAnnotationHelper.java`:

- Lines 102–112 recursively enumerate nested concrete subclasses.
- Lines 114–132 reject inaccessible subclasses or enclosing types instead of silently omitting them.
- Lines 192–225 use JavaPoet type placeholders for nested names and wildcard-parameterized generic types.
- Generated visitation methods and dispatch entries now include those subclasses.

`GenerateVisitorAnnotationProcessorTest.java:49–131` covers nested classes, deeper nesting, generics, and accessibility failures. The Gradle module adds the test dependencies and runtime `Nonnull` annotation needed by compilation tests.

**Go mapping:** Go uses explicit interfaces and dispatch rather than this annotation processor; for example, `pkg/recordlayer/query/plan/cascades/implementation_rule.go:17–20` defines its implementation-rule contract, and key-expression dispatch occurs in `key_expression.go:968` and subsequent switches. There is no requirement to port JavaPoet or add a generator merely for language symmetry. The **materialized-view/index-expression coverage enabled by #4525 remains a planner-domain handoff**, because a language-only generator disposition does not dispose of its consumers.

Other assigned test-environment changes:

- `TestDatabaseExtension.java:59–68,108–110` uses a dedicated cached callback executor instead of the JVM-wide FDB default.
- `RootLogLevelExtension.java:32–51` temporarily changes and restores the global root logger level.
- `Tags.java` adds SIMD-only, scalar-only, and dual-backend test classifications.
- `CommandsTest.java` accommodates worker-thread naming, preserves same-thread execution for debugger ThreadLocal state, and adapts its fake planner task to the changed execution return contract.
- `gradle/testing.gradle:404–415` enables JUnit parallel execution but keeps class/method defaults `same_thread`, with an opt-in fixed pool of two and `worker_thread_pool`.
- Mockito is installed as a Java agent for the applicable test JVMs; resolution is deferred to execution.
- Test temporary-directory cleanup and reporting closures become configuration-cache compatible.

SIMD is relevant to interpreting oracle results. `fdb-extensions/fdb-extensions.gradle:135–150` enables `jdk.incubator.vector` for its normal tests, while lines 213–227 run scalar fallback tests separately. `RealVectorPrimitives.java:130–151` selects scalar, strict SIMD, or automatic fallback.

Go’s oracle binary has only `-Xmx2g` in `conformance/BUILD.bazel:48`; it does not opt into the incubator module there. A result from that oracle is therefore not evidence that both Java backends were exercised. Preserve the distinction between exact scalar expectations and backend-independent vector contracts; do not weaken numerical assertions wholesale.

**Disposition:** test/build-only implementation changes, plus required awareness when interpreting conformance evidence. No Go worker-pool or logging architecture change follows from these files.

**W4 — LIKE: required shared-surface change, including value representation and errors**

Upstream #4430, commit `35f0de646`, is explicitly breaking.

Target evidence:

- `JC/query/plan/cascades/values/LikeOperatorValue.java:93–105`: consumes a message containing the SQL pattern and escape.
- Lines 145–241: polynomial-time matcher, consuming the entire input.
- Lines 207–215: `_` consumes one Unicode code point, including a surrogate pair.
- `PatternForLikeValue.java:78–95`: result is a record with pattern field 1 and escape field 2.
- Lines 115–147: validates escapes and escape sequences.

Required behavior includes:

- `%` and `_` can match line terminators.
- Literal matching has no Java-regex `$` tolerance for a final newline.
- Escape must be a valid single UTF-16 unit; `%` and `_` cannot serve as escape characters.
- Escaped escape is supported.
- A dangling escape or escape before an ordinary character is invalid.
- Invalid escape length, escape conflict, and invalid escape sequence have distinct SQL errors: `22019`, `2200B`, and `22025`.

Go baseline deliberately implements the previous behavior:

- [like_match.go:5](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values/like_match.go:5) documents and implements newline exclusions, trailing-terminator tolerance, and permissive escape fallthrough.
- `like_match.go:90–100` explicitly retries after stripping a final line terminator.
- `value_pattern_for_like.go:88–129` returns a regex-form string, declares a string result type, and returns nil for malformed escape length.
- The shared matcher backs value evaluation, predicates, and system-table filtering.

Tests that presently enshrine superseded behavior include:

- `values/like_match_test.go:51–67`: dangling escapes, ordinary-character escapes, and wildcard escape characters.
- `values/like_match_test.go:117`: old Java newline semantics.
- `values/like_match_test.go:176`: cross-check against the old regex translation.
- `values/value_pattern_for_like_test.go:91,110,136`: invalid escapes yielding nil without an error.
- [like_escape_parity_fdb_test.go:35](/home/birdy/projects/fdb-record-layer-go/pkg/relational/sqldriver/like_escape_parity_fdb_test.go:35) and line 85: user-table and INFORMATION_SCHEMA queries accepting a trailing escape.

**Disposition:** required port. The old matcher’s time bound is already polynomial; the primary work is semantic, typed-value, and error propagation fidelity. Change all shared evaluation paths together. The evaluator’s nil-for-invalid-escape behavior was already a gap relative to Java throwing an exception, independently of the newly changed escape rules.

**W5 — SQL comments, canonicalization, and IN-list validation**

SQL comments change in #4392, commit `7f846514a`.

Target `fdb-relational-core/src/main/antlr/RelationalLexer.g4:29–67,1425–1429` establishes:

- `--` begins a comment without following whitespace.
- CR, LF, CRLF, and EOF terminate line comments.
- Block comments nest through lexer modes.
- Unterminated block comments produce a lexer error.
- Comments are **skipped**, not emitted on a hidden channel. The source explains that hidden tokens would shift constant IDs while leaving canonical cache keys unchanged.
- `#` is no longer a comment; `/*! ... */` is an ordinary block comment.
- Quoted comment markers remain data.

`JR/recordlayer/query/QueryParser.java:87–124` now correctly reports lexer errors as syntax errors.

Go baseline `RelationalLexer.g4:29–39` still has a MySQL comment channel, non-nesting block comments, `#` comments, and the whitespace-dependent `--` rule. Separately, `embedded/query_hash.go:173–209` strips non-nesting comments, treats every `--` as a comment, accepts `#`, and only stops its line-comment scan at LF.

**Disposition:** required coordinated lexer/parser/cache normalization port. Updating only the grammar would leave a second, conflicting lexical interpretation in cache hashing.

Concrete breaking example: `SELECT 1--1` changes from subtraction of a negative literal to `SELECT 1`. Cache and continuation-query identity tests must cover that distinction.

IN-list documentation changes in #4480, commit `18ea45ba6`, correct a previous claim of ordinary three-valued NULL-list behavior. Target `AstNormalizer.java:493–527` rejects a syntactically bare, possibly wrapped `NULL` before cache lookup. A typed expression such as `CAST(NULL AS BIGINT)` is deliberately different and may fail later when array evaluation produces NULL.

Go already rejects NULL-list values in `core/query/expr/walk.go:1983–2003`, and maps the failure to `42809` in `embedded/logical_predicate.go:1002–1004`. Existing `sqldriver/not_in_null_probe_test.go:87–88` checks both IN and NOT IN.

**Disposition:** basic rejection already implemented; validate the **phase and expression distinctions**, including cache hits and typed NULLs. Do not mark all IN-list handling absent. Go’s current cache retains literal SQL text (`query_hash.go:118–123`), so Java’s literal-stripped cache reasoning cannot simply be projected onto it.

Case-insensitive canonicalization from #4379 also has an existing Go mechanism: `query_hash.go:118–163` uppercases outside quoted spans and preserves quoted contents. Exact equivalence across parameters, comments, metadata scope, and temporary functions remains a domain check.

**W6 — Statement options and snapshot reads**

Relevant upstream changes: #4362, #4364 (`7cb2f5ec8`), and #4603 (`45dbfdf7f`).

The grammar changes from query options attached at older query positions to statement options. The new documentation and diagrams describe top-level placement, including execution of continuations. This affects accepted syntax, not just terminology.

Target implementation evidence:

- Relational API `Options.java:232,337,594`: boolean snapshot-read option, default false, with type validation.
- `JR/recordlayer/query/AstNormalizer.java:306`: SQL option becomes execution options.
- `PlanGenerator.java:507–517`: snapshot option accepted only for SELECT or EXECUTE CONTINUATION.
- `PlanGenerator.java:295–305`: COPY continuation is rejected; it must not silently ignore the option.
- `QueryPlan.java:500–507`: option sets `ExecuteProperties` isolation to SNAPSHOT.
- JDBC wire `jdbc.proto:174–181`: extensible enum with SERIALIZABLE=0 and SNAPSHOT=1, optional field **35**.
- Connection transaction isolation remains SERIALIZABLE; snapshot is a statement-read option.

Go baseline:

- `RelationalParser.g4:587–596` has `queryOptions` with NOCACHE, LOG QUERY, DRY RUN, and EF_SEARCH; no snapshot option.
- `embedded/connection.go:758–774` already accepts only default/serializable transaction isolation.
- `recordlayer/index_maintainer.go:192–206` already routes index reads through the requested record-layer isolation level.
- Lower-level FDB snapshot implementations and tests already exist.

**Disposition:** transaction-level rejection already aligns; required SQL/API option plumbing and scope validation. Existing snapshot-capable storage is a useful foundation, not proof the SQL option exists.

The snapshot option is an execution choice that must be supplied again when resuming; the new continuation must not silently turn it into a transaction-wide or permanently serialized isolation setting. Test read-your-writes and conflict behavior, including index-backed reads. Reject mutation/DDL use with `0A000`.

Go’s conformance server uses HTTP/JSON (`conformance/conformance_server.java:5,112`), and its dependencies omit the Java JDBC/gRPC modules. Consequently, embedded oracle success would not validate field 35’s RPC conversion.

**W7 — ARRAY_AGG, bounded collection, and partial aggregate state**

Relevant changes: #4425, #4463, #4472 (`f176ec95c`), #4597 (`d470e4c82`), and #4600 (`4c41cce8f`).

Target `JC/query/plan/cascades/values/ArrayAggValue.java` verifies the substantive contract:

- Lines 117–124: nullable array result; IGNORE NULLS makes the element type non-nullable.
- Lines 169–179: accumulator restoration from continuation state.
- Lines 364–371: distinguishes no input rows from rows whose values were all ignored.
- Lines 439–456: collection, limit enforcement, NULL handling, and conversion to the plan’s protobuf representation.
- Lines 480–488: serializes the wrapper message into the accumulator’s bytes-state slot.

The resulting requirements are:

- ALL/default behavior; IGNORE/RESPECT NULLS, with RESPECT as default.
- Empty ungrouped input produces one NULL result; empty grouped input produces no group.
- All-NULL input under IGNORE NULLS yields `[]`.
- LIMIT caps collected elements but still consumes the group to find its boundary.
- Continuations retain accumulated elements and must restore descriptor-compatible record values.
- Element order follows execution order and is not a promised SQL ordering.
- LIMIT bounds element count; it is not a fixed byte quota for variable-size elements.

A documentation qualification is necessary: `array_agg.rst:202` broadly says RESPECT NULLS fails as soon as NULL is encountered. **Source checks the limit first**. Once the limit has been reached, a subsequent NULL is dropped without raising that error. `ArrayAggValueTest.java:428`, `accumulateNullBeyondLimitIsDropped`, explicitly tests this. Use source and tests, not the broader prose, as the contract.

The target documentation also lists restrictions on direct ARRAY arguments, DISTINCT, aggregate indexes, and scalar-subquery projection syntax. These must be assessed against the shared surface and existing allowed Go extensions; do not remove an approved Go-only projection or query capability merely to reproduce a Java parser limitation.

Go baseline has no ARRAY_AGG grammar arm: `RelationalParser.g4:1110–1125`. More decisively, `expressions/group_by.go:14–22` enumerates COUNT/SUM/MIN/MAX/AVG, and `executor/streaming_cursors.go:925–959` finalizes those aggregate states. This is a verified missing aggregate implementation, not an inference from filenames.

**Disposition:** required feature port, coupled to exact array/record types, partial aggregation, plan serialization, and the parent’s PromoteValue work.

Two compatibility issues need explicit review:

1. Target `record_query_plan.proto:490–493` adds optional `PArrayAggValue.limit`. Target `ArrayAggValue.fromProto():287–290` reads `getLimit()` without checking presence. At 4.13.5.0 the proto has only child and ignore-null fields, and `toProto():247–251` writes neither limit nor a sentinel. Thus an older serialized ARRAY_AGG value reaches target deserialization with protobuf’s default zero, whereas target’s unlimited sentinel is `-1`. This is a **source-demonstrated compatibility concern requiring a focused test**, not a claim that I reproduced a continuation failure. The direct 4.12.11 base predates ARRAY_AGG, but intermediate-version interoperability matters to the target’s mixed-mode claims.
2. #4589 (`b2e3b9eae`) deliberately adds tests preserving upstream Issue #4573: resuming composite aggregate state can fail when one scalar aggregate has not yet accumulated a value. `FDBStreamAggregationTest` and `GroupByQueryTests` assert that failure. Do not import those assertions as desired Go behavior or use them to weaken existing successful continuation tests. Record the upstream defect separately.

**W8 — Stored queries, temporary functions, and metadata preservation**

Relevant changes: #4157, #4291, #4327, and documentation #4411 (`59ae38f9e`).

Target source confirms that this is more than named SQL text:

- `JC/RecordMetaData.java:752–774` stores the query and temporary-function declarations.
- `record_metadata.proto:213` introduces `stored_queries = 16`.
- `PStoredQuery` fields at lines 229–232 are name=1, query=2, repeated temp_functions=3.
- `JR/recordlayer/RecordLayerEngine.java:75–82` loads catalog templates and starts offline planning.
- `OfflineStoredQueriesProcessor.java:88–105` reads templates inside a transaction.
- Lines 176–231 prepare each stored query and its temporary functions outside that catalog read.
- Per-query failures are logged/counted without aborting the remaining warm-up population.

Documentation `STORED_QUERY.rst:7–11` correctly distinguishes cache warm-up from invocation by name and describes instance-startup/catalog-snapshot behavior. Matching includes canonical query shape, temporary functions, and plan constraints.

Go baseline:

- `RelationalParser.g4:94` schema-template members do not include stored queries.
- `core/metadata/schema_template.go:18–29` has no stored-query model and documents pre-existing routine/view limitations.
- `embedded/plan_cache.go:38–46,90–105` caches by scope and normalized SQL.
- `query_hash.go:118–123` preserves literals, unlike the documented literal-stripped Java cache matching.
- `metadata.go:132–153` and `metadata_proto.go:193–197,405–421` preserve unmodelled metadata and unknown bytes.

**Disposition:** required range-introduced feature; temporary-function support is a **pre-existing dependency gap** that cannot be excluded because it makes the feature large.

There is an upgrade-specific preservation trap. Baseline Go can carry unknown field 16 through the binary metadata round trip. Once protobuf regeneration recognizes field 16, it will no longer be in unknown bytes. Unless the Go metadata model and `ToProto`/`FromProto` copy it explicitly, an otherwise innocent metadata round trip can drop stored queries. Generated schema synchronization and model preservation must land together.

**W9 — Online-index guidance, pending queues, GuardiANN, and sliding-window replay**

The new index-building guide is #4562 (`d3a493cb8`). It documents several existing APIs rather than introducing all of them.

Verified matches already in Go:

- `online_indexer.go:316–331`: batch limit 100, retries 100, rate 10,000, progress logging disabled by default.
- Lines 498–511: mutual indexing.
- Lines 1355–1363: snapshot reads for idempotent targets, serializable reads for non-idempotent targets.
- Lines 1401–1432: RangeSet progress and persisted scanned-record counters.
- Source-index validation exists at lines 626–659.

Pre-existing gaps exposed by the guide include independent initial limit, the Java 900,000-byte write cap, 4,000-ms per-transaction time limit, and configuration-loader surface. Go’s `timeLimit` is an overall build limit, not the per-transaction setting. These are existing Java API differences, not newly added limits in this range. Keep them distinguished from new pending-queue work.

A source/document discrepancy must not produce a Go regression:

- `SchemaEvolution.md:100` says large stores become WRITE_ONLY.
- Target `FDBRecordStoreBase.java:313–314` and `FDBRecordStore.java:5204–5206` default to **DISABLED**.
- The source boundary is **≤200** records, at `FDBRecordStore.java:2575`.
- Go `store_builder.go:1123–1128` already matches DISABLED and the ≤200 boundary.

Preserve the source-correct Go behavior.

The release-note queue/GuardiANN entries are corroborated by real target interfaces:

- `IndexingPendingWriteQueue.java:50–54,79–111,125–141`: deferred index updates, transactional draining, heartbeat registration, UPDATE and DELETE_WHERE payload handling.
- `VectorIndexEngine.java:215–219`: HNSW/GuardiANN dispatch.
- Lines 248–260: engine choice is immutable because switching would reinterpret storage.
- `VectorIndexMaintainer.java:118–123`: maintainer delegates to the selected engine.
- `IndexOptions.java:463` onward: GuardiANN controls, including the final merge/balance options.

Go baseline `store.go:1250–1281` dispatches the shared vector type to `newVectorIndexMaintainer`; `vector_index_maintainer.go:109–135` constructs HNSW directly. The separate `vector_spfresh` path is an explicitly Go-only extension. It is **not a GuardiANN implementation or substitute for its storage format**.

The inspected Go online-index builder applies updates directly and has no equivalent of the target queue/drain protocol. Existing `READABLE_UNIQUE_PENDING` state handling is unrelated to pending-write queues.

**Disposition:** required major feature work, with detailed algorithms and schema ownership handed to storage/vector researchers. Include queue format, overflow disabling, drain/enqueue conflicts, heartbeat/lease behavior, final readable transition, delete handling, deferred vector maintenance, engine-selection DDL, and the final #4604 split/merge policy. Size does not make these out of scope.

A concrete existing defect is also visible here:

- #4405 (`f9935060a`) replaces the old sliding-window preemptive-delete strategy.
- Target `SlidingWindowIndexMaintainer.java:395–424` routes through a common replay-safe path.
- Lines 552–565 check whether an entry is already tracked before incrementing window state, while still applying the appropriate delegate behavior.
- Go `sliding_window_index_maintainer.go:303–333` retains the old preemptive-delete implementation; its ordinary insert path increments the counter at lines 430–451.

The old method protects only one ordering of live writes and index building. Preserve the existing regression around `sliding_window_index_test.go:1254–1282`, but add the reverse ordering and queued replay contracts. The upstream commit message describes broader intent than the specific target insertion code; the storage owner should use the final source, not the message alone, for the exact stale-entry contract.

**W10 — CI selection, release production, documentation generation, and policy files**

These assigned changes are consequential to **evidence interpretation**, even where they require no Go runtime port.

CI selection, #4328 (`498bafdb2`):

- `gradle/root.gradle:74–126` constructs a dependency graph and reverse transitive closure.
- `build/affected_subprojects.py:64–92` distinguishes build-affecting and ignored paths.
- Unknown paths or malformed inputs select all tests.
- PR jobs run only affected expensive modules plus the residual test group.
- `build/test_affected_subprojects.py` tests that selection logic.

Coverage, #4413 (`32d5e73d5`):

- `gradle/root.gradle:158` filters to built jars.
- `createEmptyCoverageData` supplies zero-byte execution data when no tests ran.
- Such a report is **not evidence that tests ran or passed**.

Nightly/release changes parallelize module jobs, combine reports, run scalar fallback with tests rather than style checks, cache downloaded build assets, and improve Teamscale PR metadata. Action-version bumps are CI-only. Documentation now has its own validation path.

Publishing, #4402 (`d3fd6f8b8`) and #4415 (`a4731c555`):

- `gradle/central-publishing.gradle:126–175` stages a bundle and uploads using the Central Portal API.
- Lines 52–119 poll for PUBLISHED/FAILED with bounded waiting.
- Release bookkeeping may be pushed after an attempted publication even when publishing failed or was cancelled.

Therefore a tag or release-note entry alone is not proof that every required artifact was successfully published. The available target POMs corroborate the selected coordinates, but this audit did not download and validate an entire artifact closure.

Release-note format, #4441 (`2329f81da`):

- Stable `{#release-x-y-z-w}` anchors replace implicit numerical heading links.
- Category and mixed-mode headings become HTML so the table of contents lists releases.
- The generator parses these anchors and uses numeric version ordering.
- `publish-mixed-mode-results.py:97` changes its CLI from `--header-size` to integer `--header-level`.
- An old duplicate release heading and historical links are corrected.

These historical edits do not represent thousands of new runtime changes.

Release semantics also matter:

- 4.12.14.0 is intentionally skipped for external-server testing (#4400).
- Version-bump commits for intermediate numbers are not proof of usable published releases.
- Target `ReleaseNotes.md:27` lists ten mixed-mode predecessors beginning at 4.12.13.0; it does **not** list a direct 4.12.11.0→4.14.2.0 run.
- Earlier releases list 4.12.11.0, but transitive test history is not direct compatibility proof.
- #4548 explicitly reclassifies dependency notes. Conversely, #4540’s FieldOptions restoration is filed under Build/Test/Documentation despite changing fixture interpretation.

The Sphinx environment changes include Sphinx 8.1.3→9.1.0, MyST 4.0.0→5.1.0, Furo 2024.8.6→2025.12.19, and updates to Babel, BeautifulSoup, certifi, charset-normalizer, docutils, idna, imagesize, MarkupSafe, packaging, Pygments, requests, snowballstemmer, soupsieve, sphinx-design, and tomli. These are documentation-build dependencies, not Go SQL semantics.

The remaining navigation/heading changes, extracted SQL getting-started material, IDE settings, generated-file attributes, contribution guidance, security-reporting guidance, and assistant tooling have no direct Go runtime analogue. I treated upstream AGENTS/skills as audited repository content, **not instructions authorizing repository changes or delegation**.

**Release-note handoffs that must remain in the combined audit**

The following inventory accounts for behavioral entries in the newly added release history that are not fully adjudicated by this population. “Handoff” means implementation-domain review is still required; the documentation is not being used as proof of parity or absence.

| Domain / target ReleaseNotes.md evidence | Changes to retain | Go mapping and disposition |
|---|---|---|
| Planner rules, lines 248–257, 295, 349–354, 506 | #4322 rule-interface split; #4332 conditional rules; #4382 final-expression rules; #4394 post-optimize merge/pushdown; #4461 conditional grouping; #4417 decorrelation/simplification; #4476 collapsed replicated references; #4500 pull-up deduplication | Existing Go split-rule contract at `implementation_rule.go:17`, plus existing merge/decorrelation rules. Compare scheduling, matching, and fixpoint behavior; do not assume missing because Java class names differ. |
| Index-backed planning, lines 43–51, 99–101 | #4547 explicit index-entry conversion contract; #4593 value-based entry reading; #4550 aggregate breadcrumbs; #4598 IS NOT DISTINCT FROM index scans; #4563 rank in scanned entries | Existing aggregate candidates (`aggregate_index_candidate.go:22`), executor index paths (`executor.go:684`), and rank maintainer. Required owner decisions on value shapes and result metadata. |
| Types/SQL, lines 94–98, 164–165, 226–227, 249, 298–302, 472–483, 543 | #4171/#4453 arrays; #4440 COALESCE nullability; #4524 doubled quotes; #4535 bare alias aggregates; #4501 whole-record aggregate planning; #4469 bound NULL predicates; #4424 aggregate syntax; #4318 arbitrary-expression macros; #4397 encapsulation API; #4243 UUID structs; #4624 enum DDL | Array/Promote owner retains accepted defects. Go already decodes doubled quotes in `expr/walk.go:2336–2338`, but all literal-producing paths need domain confirmation. Existing aggregate code does not imply whole-record/ARRAY_AGG completion. |
| Query surface and identity, lines 17, 52, 252, 350, 424, 483, 539 | #4625 zero-based EXPLODE ordinals; #4285 distinctness with ordinality; #4199 SELECT without FROM; #4595 IN-subquery error classification; #4379 canonical case; #4437 display names; #4302 subscript explain | Existing Go unnest/type paths (`core/query/bound_unnest.go:184`, `cascades_translator.go:838`) and cache normalization. Preserve approved Go-only query extensions; compare shared contracts and identities. |
| Storage/index lifecycle, lines 50, 254, 348, 399–400, 425–431, 469–477 | #4309/#4293/#4350 queues; #4389 maximum size; #4373 overflow disabling; #4380 conflict fix; #4370 sliding-window integration; #4465 heartbeat; #4405 replay-safe insertion; #4410 state predicates; #4371 replaced-index disabling; #4354 delete-store metadata stamp | `online_indexer.go:1311`, `store.go:1250`, `sliding_window_index_maintainer.go:322`, existing state/rebuild code. Queue and lifecycle work is required; detailed format and concurrency review belongs to storage. |
| Vector algorithms and maintenance, lines 18, 294, 297, 347, 355, 426, 441–442, 474, 540 | #4083 GuardiANN; #4357 maintainer integration; #4398 DDL; #4422 options; #4423 engine preference; #4418 merge; #4387 cluster assignment; #4395 merge skew; #4604 final split/merge policy; #4218 SIMD | Existing HNSW and separate Go SPFresh do not satisfy GuardiANN. Source dispatch and immutable engine choice verified above; algorithm implementation remains vector-domain work. |
| Metadata/catalog, lines 118, 165, 352–353, 446, 486 | #4475 renamed types; #4399 ignored evolution options; #4363 SchemaExistsBehavior; #4351 deserialization cleanup; #4377 aged schema-template variants; #4540 FieldOptions JSON restoration | Existing `metadata_evolution_validator.go:19–33`, `core/ddl/create_schema.go:68–69`, and metadata preservation. Distinguish pre-existing model limitations from newly supported policies. |
| FDB/runtime operations, lines 49, 97, 104, 129, 537–538 | #4574 clear-size agility quota; #4488 client knobs; #4560 external-client warning; #4545 completed lock cleanup; #4289 updated-index observability; #4290 decryption retry | Existing Go context/index locks (`store.go:1321`) and serialization-options gap (`javacorpus/ledger.go:218–221`). Do not call decryption retry complete while the prerequisite serialization feature remains absent. |
| Continuation history, lines 225, 266, 291 | #4436 KeyValueCursorBase.SerializationMode change, **reverted by #4482**; separate #4468 aggregate serialization cleanup | I checked the revert (`f4b15ab3a`) and the empty base-to-target diff for `KeyValueCursorBase.java`. Do not port the abandoned intermediate change. Aggregate continuation changes remain independently relevant. |
| Upstream test guidance, lines 66–67, 121, 126, 296, 558 | #4614 GuardiANN deletes; #4597 ARRAY_AGG partial-state size; #4594 anti-joins/NOT EXISTS; #4589 Issue #4573 failure pins; #4434 CHECK_EXPLAIN metrics; #4197 sufficient data for forced continuations; #4262 multi-target→single takeover | Existing Go corpus/census and conformance infrastructure should consume the relevant contracts. Java planner metrics are not cross-engine goldens. A one-row fixture may fail to exercise continuation behavior even when its test is green. |

For #4540 specifically, source corroboration is `yaml-tests/.../command/CommandUtil.java:166–168,184` onward: it restores custom FieldOptions from raw JSON after resolving dependencies because the initial JsonFormat pass loses them. Go’s vendored-corpus README explicitly excludes the metadata JSON assets. This is a **fixture-fidelity gap to hand to the corpus/schema owner**, not evidence that upgrading protobuf alone fixes imported-schema behavior.

**3. Required regression contracts and compatibility decisions**

The RFC should specify focused tests for the following existing/new contracts. This does not authorize a new hunt campaign.

- **Oracle/build closure:** target artifact identity; one resolved protobuf runtime; generated Java proto compatibility; metadata JsonFormat behavior; embedded versus RPC coverage distinguished; scalar versus SIMD backend recorded.
- **LIKE:** newlines and final line terminators, Unicode `_`, escaped escape, all three escape error classes, malformed patterns independent of matching prefixes, NULL operands, user tables and INFORMATION_SCHEMA, cold/warm execution, and typed PatternForLike representation.
- **Comments/cache:** no-space `--`, CR/LF/CRLF/EOF, nested block comments, unterminated comments, quoted markers, `#` rejection, ignored `/*!...*/`, identical valid-query cache identity with inserted comments, and stable parameter/constant binding.
- **IN lists:** bare and wrapped NULL rejection, typed NULL distinction, parameters, cold/warm paths, and SQLSTATE preservation. Existing basic rejection tests should remain.
- **Statement options/snapshot:** legal top-level placement; rejection in nested locations; SELECT success; mutation/DDL and COPY-continuation rejection; snapshot conflict behavior; read-your-writes; repeat the option on resume; connection transaction isolation remains serializable.
- **ARRAY_AGG:** empty versus all-NULL input; IGNORE versus RESPECT; LIMIT zero and positive limits; NULL after the cap; consuming the entire group after the cap; scalar/record elements; result metadata; mid-group repeated continuation; descriptor identity; no loss or duplication; old serialized missing-limit handling.
- **Composite aggregation:** keep successful Go continuation tests where Java Issue #4573 fails. Book the upstream defect rather than introducing it into Go.
- **Stored queries:** field-16 preservation before and after regeneration; unique names; temporary-function declarations; startup-only catalog snapshot; literal-shape reuse and constraints; per-query warm-up failures; schema/template changes preventing inappropriate reuse.
- **Indexing/queues:** live-write-before-build and build-before-live-write, replay during draining, queue overflow, DELETE_WHERE, heartbeat renewal, readable transition only after required drain, and sliding-window count/boundary/delegate consistency.
- **Vector/schema:** engine choice survives metadata round trip, cannot change in place, and selects the correct physical layout. HNSW defaults and Go-only SPFresh remain distinct from GuardiANN.
- **Corpus provenance:** update the verbatim target corpus and measured census deliberately. New files and changed result/error assertions need named dispositions. Do not edit upstream data to make the Go run pass.

Important ledger changes:

- The LIKE tests named above need **targeted replacement of obsolete expectations**, with upstream source/test justification.
- Existing `SkipDDLFunction`, residual DDL, and `SkipCheckCache` entries at `javacorpus/ledger.go:148–159` must not become a permanent blanket exclusion for stored-query work.
- `SkipConformanceJavaPlannerBug` at lines 204–217 distinguishes known wrong Java answers from missing Go behavior. Preserve that distinction.
- Explain/plan-hash differences and Java `.metrics.*` changes are not authority for a bulk golden refresh.
- Existing approved Go-only query extensions remain allowed.
- The parent’s accepted **structured PromoteValue defect is explicitly re-armed and must close in this upgrade**. It is not an accepted residual gap.
- The five restricted hunts remain unapproved. Nothing in this report authorizes them, unrelated performance work, or implementation before RFC review.

Wire/schema implications are not limited to dependency bumps: stored metadata field 16, ARRAY_AGG value/state serialization, changed PatternForLike result representation, pending-write payloads, GuardiANN storage, and RPC options require explicit compatibility decisions. The reverted key-value cursor change should not be included as a final format migration.

**4. Exhaustive assigned-path ledger**

Every line below is an assigned path. Classifications refer to the findings above:

- `A`: assistant/contributor/security guidance; no direct runtime port.
- `F`: repository/IDE/generated-file metadata.
- `C`: CI selection, execution, reporting, or its tests.
- `B`: build, dependencies, generation, packaging, or publishing.
- `T`: source-reviewed visitor/test-environment changes.
- `D`: documentation navigation, heading, link, or content relocation.
- `O`: overview/onboarding claims; semantic additions accounted for above or handed off.
- `R`: release-note semantics, anchors, tooling, and handoff inventory.
- `I`: indexing/schema guidance checked against source.
- `S`: SQL contract documentation/diagrams; W4–W8 and domain handoffs.

The mechanical documentation classification is based on the complete diffs: heading normalization (#4324), navigation/content relocation (#4314/#4330), and repaired references. The nine deleted navigation/prose pages are explicitly marked; their deletion is not a SQL feature removal.

```text
A .claude/README.md
A .claude/commands/refine.md
A .claude/skills/code-reviewer/SKILL.md
A .claude/skills/code-simplifier/SKILL.md
A .claude/skills/context-resumption/SKILL.md
A .claude/skills/docs-writer/SKILL.md
A .claude/skills/frl-coding-standard/SKILL.md
A .claude/skills/frl-test-coding-standard/SKILL.md
A .claude/skills/relational-query-processor/SKILL.md
A .claude/skills/stacked-prs/SKILL.md
A .claude/skills/test-runner/SKILL.md
A .claude/skills/using-gradle/SKILL.md
F .gitattributes
A .github/copilot-instructions.md
C .github/dependabot.yml
C .github/workflows/create_branch.yml
C .github/workflows/metrics_analysis.yml
C .github/workflows/nightly.yml
C .github/workflows/pr_labels.yml
C .github/workflows/pr_mixed_mode.yml
C .github/workflows/pull_request.yml
C .github/workflows/release.yml
C .github/workflows/teamscale_upload.yml
F .gitignore
F .idea/checkstyle-idea.xml
F .idea/codeInsightSettings.xml
A AGENTS.md
A CLAUDE.md
A CONTRIBUTING.md
A GEMINI.md
O README.md
A SECURITY.md
C actions/gradle-test/action.yml
C actions/setup-base-env/action.yml
C actions/teamscale-upload/action.yml
B build.gradle
C build/affected_subprojects.py
R build/create_release_notes.py
R build/publish-mixed-mode-results.py
C build/test_affected_subprojects.py
B docs/sphinx/requirements.txt
B docs/sphinx/source/Building.md
D docs/sphinx/source/Coding_Best_Practices.md
D docs/sphinx/source/Extending.md
O docs/sphinx/source/FAQ.md
D docs/sphinx/source/GettingStarted.md
I docs/sphinx/source/IndexBuilding.md
D docs/sphinx/source/Overview.md
R docs/sphinx/source/ReleaseNotes.md
O docs/sphinx/source/SQL_Getting_Started.md
D docs/sphinx/source/SQL_Reference.md
I docs/sphinx/source/SchemaEvolution.md
R docs/sphinx/source/Versioning.md
D docs/sphinx/source/_templates/page.html
D docs/sphinx/source/api/index.md.template
D docs/sphinx/source/architecture/index.md
D docs/sphinx/source/architecture/vector-index-design.md
D docs/sphinx/source/conf.py
O docs/sphinx/source/index.md
D docs/sphinx/source/jdbc/advanced.rst
D docs/sphinx/source/jdbc/basic.rst
D docs/sphinx/source/jdbc/direct_access.rst
D docs/sphinx/source/jdbc/index.rst
S docs/sphinx/source/reference/Aggregates.rst
D docs/sphinx/source/reference/Concepts.md
D docs/sphinx/source/reference/Databases_Schemas_SchemaTemplates.rst
S docs/sphinx/source/reference/Expressions.rst
D docs/sphinx/source/reference/Functions.rst
D docs/sphinx/source/reference/Functions/aggregate_functions.rst [deleted]
S docs/sphinx/source/reference/Functions/aggregate_functions/array_agg.diagram
S docs/sphinx/source/reference/Functions/aggregate_functions/array_agg.rst
D docs/sphinx/source/reference/Functions/scalar_functions.rst [deleted]
D docs/sphinx/source/reference/Indexes.rst
D docs/sphinx/source/reference/Joins.rst
D docs/sphinx/source/reference/Lexical_Structure.md
S docs/sphinx/source/reference/Lexical_Structure/Comments.md
D docs/sphinx/source/reference/Subqueries.rst
D docs/sphinx/source/reference/sql_commands.rst [deleted]
S docs/sphinx/source/reference/sql_commands/DDL.rst
D docs/sphinx/source/reference/sql_commands/DDL/CREATE.rst [deleted]
S docs/sphinx/source/reference/sql_commands/DDL/CREATE/STORED_QUERY.diagram
S docs/sphinx/source/reference/sql_commands/DDL/CREATE/STORED_QUERY.rst
D docs/sphinx/source/reference/sql_commands/DDL/CREATE/TYPE.rst [deleted]
D docs/sphinx/source/reference/sql_commands/DDL/CREATE/VIEW.rst
D docs/sphinx/source/reference/sql_commands/DDL/DROP.rst [deleted]
S docs/sphinx/source/reference/sql_commands/DML.rst
S docs/sphinx/source/reference/sql_commands/DQL.rst
D docs/sphinx/source/reference/sql_commands/DQL/Operators.rst [deleted]
D docs/sphinx/source/reference/sql_commands/DQL/Operators/Comparison.rst [deleted]
S docs/sphinx/source/reference/sql_commands/DQL/Operators/IN.rst
D docs/sphinx/source/reference/sql_commands/DQL/Operators/IS.rst
S docs/sphinx/source/reference/sql_commands/DQL/Operators/LIKE.rst
D docs/sphinx/source/reference/sql_commands/DQL/Operators/Logical.rst [deleted]
D docs/sphinx/source/reference/sql_types.rst
S docs/sphinx/source/reference/statement_options.diagram
S docs/sphinx/source/reference/statement_options.rst
S docs/sphinx/source/reference/statement_options/ISOLATION_LEVEL_SNAPSHOT.diagram
S docs/sphinx/source/reference/statement_options/ISOLATION_LEVEL_SNAPSHOT.rst
D docs/sphinx/source/reference/understanding_bitmap.rst
B examples/examples.gradle
B fdb-java-annotations/fdb-java-annotations.gradle
T fdb-java-annotations/src/main/java/com/apple/foundationdb/annotation/GenerateVisitorAnnotationHelper.java
T fdb-java-annotations/src/test/java/com/apple/foundationdb/annotation/GenerateVisitorAnnotationProcessorTest.java
B fdb-record-layer-core-shaded/fdb-record-layer-core-shaded.gradle
T fdb-record-layer-debugger/src/test/java/com/apple/foundationdb/record/query/plan/cascades/debug/CommandsTest.java
T fdb-test-utils/src/main/java/com/apple/foundationdb/test/RootLogLevelExtension.java
T fdb-test-utils/src/main/java/com/apple/foundationdb/test/TestDatabaseExtension.java
T fdb-test-utils/src/main/java/com/apple/test/Tags.java
B gradle.properties
B gradle/antlr.gradle
B gradle/central-publishing.gradle
B gradle/check.gradle
B gradle/codequality/spotbugs_exclude.xml
B gradle/codequality/suppressions.xml
B gradle/libs.versions.toml
B gradle/proto.gradle
B gradle/publishing.gradle
B gradle/root.gradle
B gradle/scripts/log4j-test.properties
B gradle/sphinx.gradle
B gradle/testing.gradle
B gradle/wrapper/gradle-wrapper.jar [binary replacement; payload not disassembled]
B gradle/wrapper/gradle-wrapper.properties
B gradlew
B gradlew.bat
S scripts/YAML-SQL.xml
```

`Expressions.rst` consolidates operator documentation; it does not itself prove a new arithmetic implementation. `Aggregates.rst`, DML/DQL pages, and statement-option diagrams expose the new aggregate/options contracts described above. `scripts/YAML-SQL.xml` adds editor recognition of the snapshot syntax. The vector architecture page’s assigned changes are presentation changes; GuardiANN work is retained through the release-note/source handoff, not inferred from that page’s heading edits.

**5. Prioritized concrete port list and coupling**

| Priority | Work | Dependencies / completion condition |
|---|---|---|
| **P0 — design** | RFC review of the combined audit and compatibility contracts | Reconcile other domains’ findings. No implementation approval is implied by this report. |
| **P0 — upgrade integrity** | Target oracle coordinates and resolved dependency closure; generated schemas and metadata preservation | Field 16 must not become known-and-dropped. Record actual protobuf/runtime and scalar/SIMD configuration. |
| **P1 — shared SQL correctness** | LIKE value representation, matching, and errors; comments and cache normalization; statement-option grammar | Update the named obsolete tests precisely. Keep user/system-table paths consistent and preserve allowed Go extensions. |
| **P1 — accepted correctness closure** | Parent-owned mixed-array and structured PromoteValue fixes | Explicit upgrade requirement, coupled to ARRAY_AGG record elements, nullability, and continuation restoration. |
| **P1 — aggregate feature** | ARRAY_AGG grammar, types, accumulator, LIMIT, serialization, and continuation behavior | Resolve missing-limit compatibility; preserve successful composite aggregation instead of importing Issue #4573. |
| **P1 — execution option** | SQL/API snapshot option, validation, continuation resumption, and applicable RPC mapping | Build on existing record-layer isolation routing; do not change transaction isolation into snapshot. |
| **P1 — storage correctness** | Sliding-window replay-safe insertion and pending-write queue protocol | Coordinate live writes, build updates, drain, overflow, heartbeats, delete operations, and readable transition. |
| **P1 — major shared feature** | GuardiANN engine, metadata/DDL/options, deferred maintenance, merge policy, and storage compatibility | Depends on queue/maintenance design and vector-domain algorithm audit. Go SPFresh remains a separate extension. |
| **P1/P2 — catalog feature** | Stored-query model, temporary functions, persistence, and startup warm-up | Depends on metadata field preservation and appropriate canonical cache/constraint identity. Pre-existing routine gaps are dependencies to close, not grounds for exclusion. |
| **P2 — remaining shared contracts** | Planner/index-entry/rank/type/catalog/runtime handoffs in the inventory | Owner must classify each as already implemented, required, or legitimately language-only using source. |
| **P2 — pre-existing compatibility gaps** | ICU collation interoperability and documented online-indexer API differences | Keep their provenance explicit; resolve required shared-surface commitments in the RFC rather than relabeling them as new bumps. |
| **Validation gate** | Target corpus/census integration and focused cross-version regressions | Named expectation changes only; no bulk golden refresh, no weakened assertions, no restricted hunts. |

The assigned textual population is fully audited. The outstanding work is design adjudication, implementation, and the specified runtime verification—not additional sampling of these 126 paths.