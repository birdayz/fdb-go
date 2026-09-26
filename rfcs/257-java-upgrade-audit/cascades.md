The assigned audit is complete. This range requires substantive planner, expression, aggregation, function-call, and index-reader work. Updating pins and generated protobufs will not establish parity.

This is a source audit, not runtime parity verification or a review ACK. I made no edits, ran no builds or tests, and did not change branches or worktrees. **Implementation must await RFC design review.** The accepted, pre-existing structured `PromoteValue` defect is re-armed and must close during this upgrade.

**Scope and evidence.** I audited exactly the population in `/var/tmp/query-grind-cast/java-upgrade/audit/cascades.paths`.

| Item | Audited reference |
|---|---|
| Go baseline | `e48f5b4965543cd4d99b5578356059e12d969c7c` |
| Java base | `4.12.11.0` — `257aa83cae7f90e18ea6595fdf2cf841ca72e802` |
| Java target | `4.14.2.0` — `fdacd162a9c8acfadc49082b89185c823ab8ae4a` |
| Assigned paths | **282 unique paths** |
| Source populations | **191 main, 55 test, 36 testFixtures** |
| Net statuses within this population | **216 modified, 64 added, 2 deleted** |
| Complete scoped diff | **24,652 lines; 1,452,605 bytes; 282 file headers** |
| Scoped history | **34 commits** |

I consumed the complete output of `git -C fdb-record-layer diff 4.12.11.0 4.14.2.0 -- <exact assigned paths>`, passing the list entries as individual path arguments. I read it in manageable chunks and reread truncated portions. The complete diff’s SHA-256 is:

```text
0b29bff4b1d17993ef09a725fc98329d7a59b1126f0473fda13a46ea275e247c
```

I also read the scoped commit history and necessary target implementations/tests, and inspected corresponding Go implementation paths at the immutable baseline. There is **no unread assigned diff**. This does not claim exhaustive reading of unchanged portions of every file, every historical intermediate tree, or the other **907** net-changed Java paths. Selected protobuf declarations and fixture predecessors were dependency reads, not an audit of their owning groups.

All Java line references below refer to the target; all Go line references refer to the baseline, regardless of concurrent working-tree edits. These abbreviations make the evidence and exhaustive ledger manageable:

```text
Java, relative to fdb-record-layer/:
M/ = fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/
C/ = M/query/plan/cascades/
R/ = C/rules/
V/ = C/values/
P/ = M/query/plan/plans/
T/ = fdb-record-layer-core/src/test/java/com/apple/foundationdb/record/
F/ = fdb-record-layer-core/src/testFixtures/java/com/apple/foundationdb/record/

Go, relative to /home/birdy/projects/fdb-record-layer-go/:
G/  = pkg/recordlayer/query/plan/cascades/
GV/ = G/values/
GP/ = pkg/recordlayer/query/plan/plans/
GE/ = pkg/recordlayer/query/executor/
```

**W1. LIKE requires a semantic port; Go already has a polynomial matcher.** Upstream: `35f0de646`, PR **#4430**.

Java replaces the regex representation with a non-null record containing nullable pattern and escape strings. `V/PatternForLikeValue.java:92` defines that type; `:115` evaluates and validates it; `:133` validates escape characters; `:141` validates escape sequences. `V/LikeOperatorValue.java:154` implements the direct matcher. `M/query/expressions/Comparisons.java:281` consumes the new representation.

The target contracts are:

- Matching covers the entire string. `%` crosses line terminators; `_` consumes one Unicode code point.
- An escape must be exactly one non-surrogate UTF-16 character. `%` and `_` cannot themselves be escape characters.
- Only the escape character, `%`, and `_` may follow an escape. Dangling escapes and escapes before ordinary characters are errors.
- Escape-character validation still occurs when the pattern is NULL.
- `PatternForLikeValue` checks the actual second argument’s type; the old implementation accidentally examined argument zero twice.
- The retained serialized expression node now evaluates to the pattern/escape record, not a regex string.

Go’s `GV/like_match.go:90` already supplies a greedy polynomial matcher. However, it deliberately implements the **old** Java regex rules: final-line-terminator handling at `:98`, wildcard newline exclusions at `:133` and `:147`, and permissive escape handling at `:203`. `GV/value_pattern_for_like.go:69` still advertises STRING and emits regex text; `GV/value_like.go:83` rejects a `PatternForLikeValue` child. `G/predicates/comparisons.go:510` and `:596` use the same old matcher.

SQL admission also needs coordinated work: `pkg/relational/core/query/expr/walk.go:2014` checks one Go rune, which admits supplementary characters that Java rejects as escapes; `expr.go:1560` requires a constant string pattern and carries a rune without the new validation. The zero-rune sentinel also cannot distinguish absent escape from an explicit NUL escape. The representation limitation is a **pre-existing gap**. (Corrected by the WS-E oracle: constant-pattern-only admission is the TARGET's rule, not a Go gap: the grammar's LIKE pattern is `constant`, so `LIKE ?` is 42601 in the target [prepared_like_param_pattern], and Go accepting it today is the divergence WS-E removes.)

Required contracts: full-string/newline behavior; supplementary characters in subjects; invalid, doubled, dangling, wildcard, empty, and supplementary escape characters; NULL pattern plus invalid escape; rebuilt/cached expression type; predicate and Value agreement; and sound prefix-range extraction with residual evaluation. Port the semantics and error paths through existing machinery; another matcher framework or performance campaign is unnecessary.

**W2. Variadic typing, array rebuilding, and structured promotion are coupled work.** Upstream: `851712f14` **#4171**, `4e44a35ac` **#4440**, and `a3694976f` **#4453**.

`V/VariadicFunctionValue.java:128` derives a common underlying type and result nullability. COALESCE is nullable only when **all** arguments are nullable; GREATEST/LEAST are nullable when **any** argument is nullable. Encapsulation preserves each promoted argument’s own nullability (`:260`, `:266`). Construction, `withChildren`, and deserialization rederive and validate the type (`:107`, `:157`, `:211`).

Go has two relevant places to fix, not just one: `GV/scalar_function_catalog.go:455` uses `CommonValueType`, which forces nullability at `:543`; `GV/values.go:2516` independently forces every `ScalarFunctionValue` result nullable. The COALESCE change is range-introduced; unnecessarily nullable GREATEST/LEAST results are also a **pre-existing Go mismatch**.

The target array constructor no longer insists that rebuilt children’s freshly inferred common type exactly equal the retained element type. It promotes compatible children to that retained type (`V/AbstractArrayConstructorValue.java:227`). Go’s `GV/value_array_constructor.go:101`, `:119`, and `:137` still enforce the obsolete equality check.

Java also replaces accidental null-element failures with an explicit `UNSUPPORTED` error: “An ARRAY value cannot have NULL elements” (`AbstractArrayConstructorValue.java:204`; `MessageHelpers.java:505`). **This does not introduce support for NULL array elements.** Their rejection predates the range; Go’s pass-through behavior is a pre-existing mismatch.

Structured promotion changes include:

- Record-to-record promotability: `V/PromoteValue.java:453`.
- Promotion when record field names or field numbers differ, even if primitive field types match: `:478`, especially `:488`.
- Deserializing input and target types before the coercion trie, so referenced type IDs exist: `:339`.
- Wrapping nullable-array aggregate results before inserting them into a record: `V/RecordConstructorValue.java:365`.

Go already has nullable-array wrapper construction in `GV/proto_type.go:409`, `:430`, and `:469`, and record-field wrapping in `GV/record_constructor_message.go:84`. Those mechanisms should be reused. The Java `NullableArrayTypeUtils.wrapperTypeFor` extraction and `Type.java:3253`/`:3272` changes do **not** independently change the existing wrapper wire format.

The substantial missing dependency is Go’s actual promotion evaluator: `GV/values.go:5177` and `GV/value_constant_object.go:105` handle scalar conversions but do not recursively promote arrays/records. Returning the original structured object cannot implement descriptor, field-name, field-number, or nested-element conversion. **This is the re-armed pre-existing PromoteValue defect and a required upgrade closure**, coordinated with the controller’s accepted cases.

Other pre-existing discrepancies on this touched surface must remain explicit: Java evaluates all variadic children (`VariadicFunctionValue.java:117`), while Go COALESCE short-circuits (`GV/values.go:2551`); Go’s common-type helper also accepts all-untyped-NULL results where Java’s operator lookup fails. Shared COALESCE behavior cannot be dismissed as a Go-only query extension. Avoid changing unrelated IF/CASE behavior merely because they share Go helpers.

Required contracts: nested arrays and records, field renaming/renumbering, compatible numeric widening, retained array type after rebuilding, NULL versus empty wrapper presence, explicit null-element errors, operator-specific nullability, rebuilt values, and fresh versus deserialized descriptor identity. Positive structured-promotion cases must become successful tests, not accepted errors.

**W3. ARRAY_AGG is a new feature requiring planner, execution, and continuation work.** Upstream: `1b124cecc` **#4463**, `4c41cce8f` **#4600**, `d470e4c82` **#4597**; related streaming cleanup `296c13989` **#4468** and known-defect tests `b2e3b9eae` **#4589**.

`V/ArrayAggValue.java` establishes these target contracts:

- The result is a nullable array (`:117`). IGNORE NULLS makes its element type non-nullable.
- Scalar `eval()` is invalid; per-row evaluation evaluates the child (`:151`, `:162`).
- Encapsulation takes the expression plus resolved literal null-treatment and limit arguments; unresolved element types are rejected (`:313`).
- `NO_LIMIT` is `-1`; zero is valid.
- IGNORE NULLS skips NULLs. RESPECT NULLS raises the existing unsupported-null-element error when a NULL would be collected (`:446`).
- The cap is checked **before** null validation/conversion (`:443`). A NULL after the cap is therefore ignored, including at limit zero.
- Reaching the cap does not stop consumption at the group boundary or prevent child evaluation on subsequent rows.
- Collected structured values are converted to protobuf representation using the plan’s type repository (`:143`, `:454`).
- A group with no rows has no accumulator state. A seen group with no collected values has a present empty wrapper state (`:480`).
- Restoration requires one accumulator state containing one bytes state (`:169`, `:411`).

The raw accumulator finishes to a list, including an empty list. SQL’s empty ungrouped aggregate result must still be NULL through the surrounding aggregate-result/default-on-empty machinery; it must not be inferred solely from `finish()`.

Go’s absence is established by implementation, not a name search: `GV/values.go:5616` and `G/expressions/group_by.go:14` enumerate the existing aggregate operations; `GE/streaming_cursors.go:135`, `:667`, and `:712` contain count/sum/min/max/average state and dispatch, with no array accumulator, null policy, or collection cap.

Go already has partial-group continuation machinery (`GE/streaming_cursors.go:301`, `:362`, `:441`). It is not Java-compatible accumulator encoding: `GE/continuation.go:753` writes a private positional payload, and `:860` explicitly documents and checks its `1 + 6*numAggs` shape. Adding an array slot without a deliberate compatibility design would break that contract.

The streaming plan drops `SerializationMode` from its Java API (`P/RecordQueryStreamingAggregationPlan.java:139`, `:156`, `:379`). Go already follows a partial-state approach, so copying a Java mode-removal API is unnecessary. Its native continuation evolution still needs an RFC decision.

Required regressions include no input, all NULLs under IGNORE, RESPECT failure before the cap, NULL after the cap, zero limit, typed/untyped arguments, scalar and structured element types, repeated interruption/resumption, mixed aggregates, group boundaries, descriptor identity, corrupt state, and the existing large-partial-state fixtures. Unlimited ARRAY_AGG state grows with collected elements; the target’s size tests do not establish a constant bound.

Crucially, #4589 **does not fix** issue #4573. `T/provider/foundationdb/query/FDBStreamAggregationTest.java:399` and `:425` deliberately expect `VerifyException` for combinations of aggregates with absent partial state. Those tests must remain visible as an upstream limitation. Do not weaken working Go behavior to reproduce that failure or claim that target Java has solved it.

**W4. Function-call arguments, options, and window encapsulation require a native port.** Upstream: `daaf0f2e6`, **#4397**.

`C/CallSiteArguments.java` replaces a bare argument list with positional/named arguments, typed options, and a window specification. Argument arity excludes options/window data; rebuilding argument collections preserves the other call-site components. `C/WindowOrderingPart.java:46` associates an ordering Value with its requested direction and includes that in correlation/equality/hash handling.

Option handling is behavior, not just Java API reshaping:

- Supported-option validation and duplicate/unknown-option errors.
- Non-null values.
- Integral inputs with explicit 32-bit range checks for integer options (`CallSiteArguments.java:314`).
- No fractional-to-integer conversion (`:322`).
- Actual Boolean values, not string parsing (`:344`).
- Number-to-double, character-sequence-to-string, and case-insensitive enum-name conversion (`:336`, `:350`, `:358`).
- Option identity is its name (`:247`).

`V/RowNumberValue.java:421` becomes a zero-positional-argument built-in. It obtains partition/order Values from the window specification and reads `EF_SEARCH` and `RETURN_VECTORS` as typed options (`:428`, `:436`, `:454`). The target extracts the ordering **Values** when constructing `RowNumberValue`; the richer call-site wrapper alone is not proof that SQL descending vector ordering became supported.

Go already carries `EfSearch` and `IsReturningVectors` in `GV/value_row_number.go:40`. Its SQL path at `pkg/relational/core/query/expr/walk.go:1267` constructs row-number Values directly; `:1337` handles EF_SEARCH with ad hoc integer parsing, permits replacement of duplicate settings, and lacks the corresponding RETURN_VECTORS admission. Ordinary scalar calls still use positional argument handling at `:1058`.

Required work is a consistent call contract through the existing resolver/catalog, preserving Go-only functions. Java reflection in `JavaCallFunction` has no required Go-language analogue. Do not add a general plugin/function framework.

`RowNumberHighOrderValue` and its test are deleted upstream. Go still has the compile-time helper in `GV/value_row_number_high_order.go:30`; retire or replace its shared-surface role after the direct encapsulation path is designed.

Required contracts: options/window preservation during rebuilding, arity independent of options, strict option types/ranges/nulls/duplicates, both row-number options, unsupported options, and zero-argument row-number construction. Test actual target entry points: macro/UDF overrides do not universally pass through the built-in option normalizer.

**W5. Arbitrary macro bodies and named/default arguments are substantial missing functionality.** Upstream: `b2b930437`, **#4318**, with #4397 integration.

`C/CatalogedFunction.java:214` validates named argument names and missing required parameters; `:248` validates positional omission against defaults. Resolution orders arguments by declared parameters and promotes supplied arguments (`:272`, `:302`, `:336`). Defaults are Values, not merely literal values.

`C/UserDefinedMacroFunction.java:70` substitutes resolved argument Values into an arbitrary Value body using correlation translation. Its metadata encoding preserves parameter names and distinguishes “no default” from a provided default Value, including a NULL Value (`:90`). The target still accepts the old no-names representation (`:119`).

Go’s corresponding admission path is `pkg/relational/core/query/expr/walk.go:1169`: the user-defined-function route recognizes CARDINALITY and rejects other user functions at `:1187`. It does not implement general Java macro expansion. This is a **pre-existing missing feature whose supported surface grows in this range**, not an acceptable omission based on implementation size. Opaque metadata preservation is not invocation support.

Required contracts: positional/named invocation, unknown names, missing required arguments, omitted versus supplied NULL arguments, default presence, argument promotion, arbitrary expression bodies, repeated parameter references, correlation rebasing, and old/new macro metadata round trips. Structured argument promotion depends on W2; call admission depends on W4.

**W6. Conditional rules and rewrites after child optimization require a coupled planner port.** Upstream: `97d790cf1` **#4322**, `2bd9a79d8` **#4332**, `f76213e1e` **#4382**, `3afcd0de2` **#4461**, `1ad782c8d` **#4417**, and `f4b2283b2` **#4394**.

The final target behavior supersedes intermediate approaches in the history:

- `AbstractCascadesRule` supplies common rule machinery; exploration/implementation become interfaces.
- Conditional chains require compatible root/matcher/pruned-input characteristics (`C/ConditionalCascadesRule.java:88`), union dependencies, and respect wrapper/inner-rule enablement.
- A chain advances only if the preceding rule makes **no progress**. Matching alone is insufficient. Progress includes yielded constructs and scheduling caused by requirements (`C/CascadesPlanner.java:1066`, `:1146`, `:1234`).
- A stale group/expression invalidates the whole remaining chain.
- Rules marked for pruned inputs run after child cost optimization, using task-stack ordering (`CascadesPlanner.java:1371`).
- Rewriting uses conditional decorrelation→simplification and select-merge→predicate-pushdown chains (`C/RewritingRuleSet.java:50`, `:63`).
- `FinalizeExpressionsRule` forms the cross-product of child expression partitions (`R/FinalizeExpressionsRule.java:83`, `:92`).
- `SelectMergeableProperty` partitions child alternatives according to mergeability. The target no longer has each merge rule independently choose a cheapest exploratory alternative.
- Predicate pushdown handles eligible predicates across multiple legs in one result and removes only predicates successfully pushed (`R/PredicatePushDownRule.java:203`, `:213`, `:259`, `:284`).

Go’s `G/default_rules.go:146`, `:396`, and `:424` still register the relevant rewrites independently. `G/unified_tasks.go:359` and `:538` do not return the target progress result; `:955` optimizes/prunes inputs but does not schedule the new after-pruning rewrite class. `G/rule_finalize_expressions.go:40` yields the original expression rather than the partition cross-product.

`G/rule_select_merge.go:130` examines all child members. `G/rule_predicate_push_down.go:118` also loops members and returns after one pushed leg at `:171`.

This is not satisfied by renaming rule interfaces. Port scheduling, progress, dependency freshness, partitioning, finalization, and the two rewrite algorithms together. Preserve Go’s existing staged rule-call behavior and protections for outer joins, strict-single evaluation, null-on-empty, and correlated lateral extensions.

Required contracts: no-progress fallback; stop after progress; disabled inner rules; stale expressions/dependencies; requirements that cause progress; post-pruning ordering; 2×2 child partitions producing four final alternatives; multi-leg pushdown; residual preservation; and mergeability based on the appropriate select root. Existing Go extensions must continue to satisfy their semantic guards.

**W7. Index-entry conversion becomes explicit and Value-based; existing Go scaffolding is insufficient.** Upstream: `576773759` **#4547**, `12a0a326d` **#4550**, and `f8f7e9569` **#4593**.

`P/RecordQueryPlanWithIndexEntryToQueriedRecord.java:60` introduces the conversion contract, distinguishing stored-record descriptors from dynamic aggregate-result descriptors (`:90`, `:114`) and evaluating a reader Value with the index entry bound into its context (`:142`).

`C/IndexEntryToRecordValueHelper.java:64` constructs readers with these details:

- First covering source wins for a field.
- Nested field structure and column metadata are preserved (`:91`).
- Uncovered fields become NULL, except non-null arrays become empty arrays (`:116`).
- KEY/VALUE ordinal paths are explicit (`:135`).

`C/AggregateIndexMatchCandidate.java:431` constructs the aggregate reader. Its layout logic at `:548` is important: ordinary aggregate grouping columns come from KEY and aggregate columns from VALUE; permuted min/max places aggregate and reordered grouping columns in KEY, including the zero-permutation case.

`P/RecordQueryAggregateIndexPlan.java:145` prefers the Value reader and falls back to copiers. The fallback uses the dynamic result descriptor, not the stored record descriptor. Reader state must survive plan transformations (`:213`, `:275`, `:297`). Aggregate cardinality now obtains grouping evidence from the match candidate (`C/properties/CardinalitiesProperty.java:201`), allowing at-most-one proofs for scalar aggregation or equality-bound grouping.

A new `RecordQueryCoveringIndexValuePlan` supplies execution, fetch-value push-through, identity, serialization, explain, and property support. **At this target, normal value/windowed candidate planning still emits the old covering plan.** The new class is staged/manual coverage; aggregate Value readers are active. Do not accidentally migrate all covering planning beyond the target’s behavior.

Go has useful existing implementations:

- `GP/covering_index_scan.go:39` models direct covering columns.
- `GE/executor.go:1483` reads physical KEY/VALUE data, but `:1511`/`:1532` construct a flat row; nested/expression/multiple-type covering is intentionally refused by this path.
- `GP/aggregate_index.go:65` carries result/group metadata.
- `GE/executor_new_plans.go:355` and `:444` already implement permuted and ordinary aggregate layouts.
- `GP/cardinality_bounds.go:232` conservatively returns unknown maximum for aggregate index plans—a pre-existing optimization gap.

However, `GV/value_index_entry_object.go:88` is **not a production-ready adapter**. It declares `PrimaryKey() any` and `IndexValues() any`; real `recordlayer.IndexEntry` methods return `tuple.Tuple` and therefore do not implement that interface. Worse, `pkg/recordlayer/index_scan.go:233` returns the extracted primary key, while `:245` returns the indexed prefix of KEY—not the raw KEY and VALUE that the Value assumes. `walkOrdinalPath` also expects `[]any` (`value_index_entry_object.go:170`). Existing tests use a fake that hides these differences. Production planner/executor wiring does not currently instantiate this reader path.

Required work: a correct raw-entry binding/adapter, reader construction, nested/default-field semantics, aggregate reader preservation, and conservative candidate-based cardinality evidence. Keep existing Go aggregate-layout handling and its unrelated correctness protections.

Required regressions: real index entries rather than only fakes; KEY versus VALUE; nested ordinal paths; duplicate covering sources; uncovered non-null arrays; ordinary/permuted aggregate layouts; fresh versus rebuilt/deserialized plans; no-fetch execution; properties; dynamic descriptors; and absent match-candidate fallback.

**W8. GuardiANN-aware planning and vector-engine preference are new upgrade work.** Upstream: `4e3d42bc8` **#4083**, `1ce8c71dd` **#4357**, and `6d4bf06f0` **#4423**.

Assigned changes introduce `VectorIndexEnginePreference` with NONE, PREFER_HNSW, and PREFER_GUARDIANN; configuration access/serialization; generic vector metric lookup (`C/VectorIndexExpansionVisitor.java:188`); and candidate engine identification (`C/VectorIndexScanMatchCandidate.java:383`).

`C/PlanningCostModel.java:189` applies engine preference before type-filter comparisons. The comparison at `:355` only expresses a preference when both alternatives have exactly one identifiable vector access. It abstains for absent/multiple accesses, equal engines, or NONE. This is a preference, not an eligibility filter.

Go’s candidate at `G/vector_index_match_candidate.go:25` carries metric/search options but no engine classification. `G/plan_context.go:43` lacks the preference; `G/planning_cost_model.go:170` has no equivalent comparison. Actual store dispatch at `pkg/recordlayer/store.go:1250` selects HNSW for the existing vector type; SPFresh has separate handling at `:1278`, with unsupported types rejected at `:1282`. **SPFresh is not a GuardiANN implementation.**

Required scoped ports: metadata-to-candidate engine identity, generic metric selection, configuration/default handling, and the exact comparison applicability and priority.

The larger **GuardiANN backend, pending-write queues, and split/merge/reassignment maintenance remain explicit upgrade dependencies** for their owning researcher. Upstream commit intent includes those mechanisms; their implementation paths are outside this population and are not claimed reviewed here. Generated queue protobufs do not establish backend parity. The feature must not disappear from the upgrade plan because of its size.

Required planner contracts are represented by `T/query/plan/cascades/PlanningCostModelVectorEngineTest.java:157`: NONE, both preferences, same engine, absent candidate, non-vector plans, zero/multiple vector accesses, and interactions with earlier cost criteria.

**W9. Null-safe equality scan support is mostly already present in Go.** Upstream: `7cefc75de`, **#4598**.

Java classifies IS NOT DISTINCT FROM as an equality comparison for scans (`M/query/plan/ScanComparisons.java:151`) and permits it in range constraints (`C/predicates/RangeConstraints.java:658`, `:731`). IS DISTINCT FROM remains unsuitable as one contiguous scan range.

Go already treats IS NOT DISTINCT FROM as an exact-key equality in `G/predicates/comparison_range.go:226` and `:270`; IS DISTINCT FROM remains residual there. This is **source-confirmed existing support**, not a missing index-scan implementation.

A remaining distinction is `G/predicates/range_constraints.go:154`, which admits IS DISTINCT FROM into the builder. `AsComparisonRange` at `:126` refuses a merge with residuals, so this is not evidence of newly discovered wrong rows. Align the shared builder contract or document/test its conservative Go representation without losing residual filtering. The comment saying null-safe equality is not supported by Java is obsolete.

Required contracts: NULL and non-NULL singleton ranges, combined range constraints, and IS DISTINCT FROM residual retention.

**W10. Enum comparison fixes need a regression, not a copied Java descriptor workaround.** Upstream: `181112c4a`, **#4624**.

`V/RelOpValue.java:230` avoids compile-time materialization of enum comparison operands, retaining a Value comparison so an empty type repository does not cause a descriptor NPE.

Go’s comparison retains an operand Value (`G/predicates/comparisons.go:256`, `:379`); enum promotion/read paths use the declared numeric representation (`GV/values.go:5213` and `:1173`). The precise Java descriptor-NPE mechanism is therefore not present in those paths.

Classification: source-level corresponding representation already avoids this mechanism; **runtime DDL parity remains unverified**. Add the enum-column/literal DDL contract through the SQL/DDL owner, including valid and invalid literals. No separate Java-style descriptor workaround is indicated by the inspected Go code.

**W11. DML must reject snapshot execution before consuming its child.** Upstream: `7cb2f5ec8`, **#4364**.

`P/QueryPlanUtils.java:43` enforces serializable isolation. `P/RecordQueryAbstractDataModificationPlan.java:192` and `P/RecordQueryDeletePlan.java:96` call it before child execution, including dry-run paths. The same diff fixes a Java-specific update-plan cast in abstract modification-plan identity (`:288`); that cast has no Go analogue.

Go exposes snapshot isolation through `pkg/recordlayer/scan_properties.go:132` and `:271`, and uses it in scans. Its delete, insert, and update execution starts children at `GE/executor.go:3859`, `:3928`, and `:4175` without the corresponding isolation guard.

Required port: reject snapshot DML consistently before reads or writes, including dry-run execution and all supported entry paths. Tests should assert both the error and lack of child consumption/mutation. The SQL query-option plumbing is outside this assigned group. The Java option is per execution, not persisted as a continuation property.

**W12. EXPLODE gains optional zero-based ordinality and a stronger distinctness property.** Upstream: `2da82fb53` **#4625** and `9ccbc7699` **#4285**.

`C/expressions/ExplodeExpression.java:85` adds the flag and rejects zero-based ordinality without ordinality. It propagates through identity, rebasing, and implementation. `P/RecordQueryExplodePlan.java:112` assigns ordinals from original collection positions before limit/resume handling. `C/properties/DistinctRecordsProperty.java:238` recognizes that ordinality makes emitted rows distinct.

Go only carries `withOrdinality` (`G/expressions/explode.go:34`; `GP/explode.go:25`) and hardcodes one-based positions (`GE/executor.go:4731`, `:4749`, `:4767`). `G/plan_properties.go:74` has no corresponding EXPLODE distinctness case.

Required port: flag propagation, validation, execution, structural identity, and distinctness. Preserve existing one-based behavior and hashes/bytes where those contracts apply. Java SQL `AT` remains one-based; this is an opt-in core capability.

Required contracts: duplicate elements with/without ordinality, both bases, invalid flag combination, empty input, rebasing/rebuilding, limits/resumption without renumbering, and unchanged default explain behavior.

**W13. Explain/display changes require targeted expectation updates.** Upstream: `545efe8f0` **#4437** and `f4bc3c33a` **#4302**.

Java decodes storage identifiers for record-type comparisons, logical type filters, and insert/update/delete expression/plan displays. Evidence includes `M/query/expressions/RecordTypeKeyComparison.java:260` and `C/explain/ExplainPlanVisitor.java:473`, `:629`, `:699`. `V/SubscriptValue.java:77` fixes the closing bracket.

Go’s DML explain paths still print raw record-type names (`GP/delete.go:111`, `GP/insert.go:141`, `GP/update.go:173`), as does `GP/typefilter.go:105`. Existing decoding helpers are in `pkg/recordlayer/protoname/protoname.go:180` and `:228`; use the appropriate existing mechanism for Java-origin names while preserving Go-only raw-descriptor/provenance behavior.

Go’s `GV/values.go:1846` explain dispatcher has no subscript-specific rendering and falls back to the node name at `:2165`. Thus it does not contain the literal Java `>` typo; its corresponding renderer is a pre-existing abbreviated surface.

Required contracts: escaped user identifiers in each affected display, subscript syntax, and covering-Value-plan explain output. These are display changes, not permission to rename stored identifiers, metadata keys, or plan identities. Update affected expectations individually.

**W14. Value visitation is a new API with a consumer dependency.** Upstream: `a629befaf`, **#4525**.

`V/SimpleValueVisitor.java:30` introduces a bottom-up fold; `V/Value.java:786` accepts the visitor. Children are visited in order; pruning excludes a child’s result rather than inserting a positional placeholder (`SimpleValueVisitor.java:59`, `:105`); shared subtrees are visited once per occurrence, without memoization.

Go’s `GV/values.go:1482` provides a preorder `WalkValue` with pruning. It is not the same result-folding contract. Java annotation-generated dispatch has no required language-level copy, but consumers requiring the new traversal semantics need an equivalent implementation using the existing Value traversal structure.

The materialized-view generator itself is outside this population. Its owner must account for the consumer dependency; this report does not classify that feature as absent or complete. Required traversal tests: child-before-parent order, pruning, repeated shared subtrees, and specific-type/default dispatch.

**W15. Remaining Java/API/test-infrastructure changes do not independently require execution ports.**

`5cec45354` **#4388** removes SpotBugs annotations/imports across the explicitly listed mechanical group. Documentation, local `var` substitutions, and the test logging-extension import do not change query algorithms.

`C/TempTable.java:183` makes its Java factory final with a private constructor. Go uses a native constructor (`G/temp_table.go:12`) and has no subclass-injection API to remove.

`4b0143ac8` **#4410** changes `SyntheticRecordPlanner.java:228` from a store predicate to `getIndexState(...).isDisabled()`. It is equivalent for this caller. Go’s synthetic-record modeling remains a pre-existing limitation (`pkg/recordlayer/metadata.go:1848`, `:1865`), not a feature newly introduced by this predicate cleanup.

`80a499db8` **#4297** relocates test infrastructure. I compared the moved content with predecessor files:

- `DualPlannerExtension`, `DualPlannerTest`, and all **30** listed match-helper classes are byte-identical to their predecessor test files.
- `FDBRecordStoreQueryTestBase` additionally loses the small `Holder` helper, moved into assigned `FDBInQueryTest`.
- `FDBCollateQueryTestBase` renames/extracts the old base, changes its internal names collection to a private immutable list, and moves the JRE runner into `FDBCollateJREQueryTest`. Query assertions are retained.
- The two `package-info.java` files are documentation.

Build/testFixtures integration belongs to the controller. These moves are not additional Go query behavior.

The **60 rule declaration adapters** inherit W6’s common architecture changes; their own diffs do not introduce additional rule algorithms. The **39 call-site adapter paths** inherit W4’s invocation contract; their local edits wrap/extract argument lists, with annotation changes and the separately identified `Holder` relocation. In particular, `TypeRepositoryTest` and `CardinalitiesPropertyTest` only receive call-site adaptation in this range; their presence in the path list is not new coverage of array wrappers or aggregate cardinality.

**Compatibility must be recorded explicitly in the existing upgrade design and ledger.**

| Surface | Target contract and implication |
|---|---|
| LIKE expression | Existing `PatternForLikeValue` node survives, but its evaluated result changes from regex STRING to a record. Type registration, reconstruction, and cached-expression behavior need coverage. |
| Variadic Values | Result type is derived again on reconstruction. Different nullability can change result metadata and simplification without adding a new proto field. |
| PromoteValue | Deserialization order matters for referenced type IDs. Recursive conversion and descriptor identity remain necessary even where wire declarations are unchanged. |
| Nullable arrays | The wrapper helper extraction preserves the existing wrapper representation. NULL wrapper absence and present empty wrappers must remain distinct. |
| ARRAY_AGG | New `PValue` field **64**; `PArrayAggValue` has child, null policy, and limit. The writer explicitly writes `-1` for uncapped operation. **An omitted limit decodes as zero**, because `fromProto` directly calls `getLimit()`. This affects interim pre-#4600 payloads; base 4.12.11.0 has no ARRAY_AGG payload. |
| Aggregate continuation | Java uses typed accumulator states, including a bytes wrapper for arrays. Go currently uses a private positional payload under the same envelope. Schema equality is not interoperability. Preserve/reject/version old Go payloads deliberately. |
| Streaming plan | Target retains plan tag **38** and reserves retired tag **25**, whose old payload format is incompatible. Do not reinterpret tag 25 bytes as tag 38. |
| Aggregate index plan | Result type field **7** and reader Value field **8** accompany VC0/VC1 behavior. Target VC0 retains breadcrumb Values; VC1 omits them and reconstructs placeholders. Reader serialization/deserialization occurs last to respect type IDs. |
| Aggregate index hashing | `P/RecordQueryAggregateIndexPlan.java:343`: LEGACY/VC0 hash the result Value; VC1 hashes its type. Current continuation mode remains VC0; this upgrade does not silently authorize switching modes. |
| Covering Value plan | New plan tag **41**. Its serialization and property/explain support are real additions even though ordinary covering planning still emits the old class. |
| Macro metadata | New parameter names/default Values preserve “default absent” separately from “default provided as NULL”; old unnamed representation still reads. |
| Row-number helper | `PValue` tag **58** is reserved. Upstream documents the removed helper as compile-time-only and tests rejection of its old encoded form. |
| EXPLODE | Optional boolean field **3**; false is omitted by the writer. Existing shapes retain the old behavior/hash contract; zero-based mode is a distinct shape. |
| Vector preference | Proto/config changes need native planner consumption. Generated declarations alone do not implement the preference or GuardiANN. |
| Display names | Display-only decoding must not change stored names or schema identity. |

These protobuf details were checked as dependencies of assigned source, particularly `record_query_plan.proto:288`, `:293`, `:301`, `:490`, `:1748`, `:1753`, `:1777`, `:1801`, `:1883`, and `:2271`.

I found no production Go `PValue`/`PRecordQueryPlan` codec in the inspected baseline `pkg` implementation. That is a **pre-existing cross-language plan-serialization gap**, not proof that Java’s format changes are harmless. The RFC must state the supported interchange boundary and ownership. This audit does not propose creating a new serialization framework. Go’s existing observational plan hash is also explicitly not embedded in continuations (`GP/plan_hash.go:9`), so Java continuation hash changes should not be applied blindly to that hash.

**Existing expectations requiring deliberate treatment are identifiable now.** None were changed or executed.

| Go baseline location | Current expectation | Required treatment |
|---|---|---|
| `GV/like_match_test.go:54`, `:57`, `:61`, `:66` | Dangling/ordinary escapes and wildcard escape characters follow old permissive behavior | Replace individual cases with target values/errors. |
| `GV/like_match_test.go:117` | Regex newline exclusions and final-terminator tolerance | Replace with target full-string wildcard contracts. |
| `GV/like_match_test.go:176`; `G/predicates/comparisons_test.go:1584` | Old regex translation serves as oracle | It is no longer a valid target oracle. Retain bounded coverage with target semantics. |
| `GV/value_pattern_for_like_test.go:10`, `:18`, `:91`, `:136`, `:157` | STRING/regex output and invalid escapes returning nil | Assert record type, presence, and explicit errors. |
| `GV/value_like_test.go:17` | Rejects `PatternForLikeValue` child | Support the target expression shape after both sides are ported. |
| `GV/scalar_function_catalog_test.go:176`, `:177` | Non-null arguments yield nullable COALESCE/GREATEST | Correct operator-specific nullability. All-NULL/one-argument helper cases at `:183` also need admission review; a helper test alone does not prove SQL acceptance. |
| `GV/value_array_constructor_test.go:110`, `:183` | NULL elements succeed | Pre-existing mismatch; retain explicit target rejection, coordinated with controller-owned cases. |
| `GV/value_array_constructor_test.go:185`, `:186` | Compatible narrowing/widening rebuild cases fail | Restore success where target promotion permits it; keep genuinely incompatible cases negative. |
| `G/rule_predicate_push_down_test.go:1616` | Two firings leave two then one residual predicates | Target multi-leg rewrite should achieve the intended result in one firing. |
| `GV/scalar_functions_extra_test.go:445` | COALESCE suppresses a later division error | Pre-existing eager/lazy divergence; resolve for the shared surface without changing Go-only IF behavior. |
| `GV/value_index_entry_object_test.go:9` | Fake entry satisfies an interface the real entry does not | Add a real-entry integration contract when wiring W7; fake-only success is insufficient. |

`G/rule_finalize_expressions_test.go:9` remains useful leaf coverage; add partition-product cases rather than weakening it. Likewise, new Java #4573 expected-failure tests are not grounds for converting Go success tests into accepted failures.

Use the existing expectation/defect ledger to record each causal change: upstream commit, old expectation, target contract, implementation dependency, and regression result. No bulk golden refresh, weakened assertions, new bug-hunt campaign, or unrelated performance work is justified. The five restricted hunts remain unapproved.

**The concrete port order should be dependency-driven after RFC review.**

| Priority | Work | Coupling / completion condition |
|---|---|---|
| P0 | W2 structured promotion and array/variadic contracts | Close the re-armed PromoteValue defect and controller-owned accepted array cases. Needed by macros, ARRAY_AGG, and reader construction. |
| P0 | W1 LIKE semantics and error propagation | Change matcher, pattern representation, predicate path, admission, and targeted expectations together. |
| P0 | W11 snapshot-DML guard | Enforce before child execution across insert/update/delete and dry runs. |
| P1 | W6 planner scheduling, conditional rules, partitions, finalization, merge/pushdown | One coherent design; preserve Go semantic guards and extensions. |
| P1 | W3 ARRAY_AGG and continuation evolution | Depends on W2 and function admission. Requires an explicit native-continuation compatibility decision and bounded resume contracts. |
| P1 | W4/W5 call-site contract and macro invocation | Implement actual invocation, typed options, defaults, arbitrary bodies, and metadata semantics; retire the higher-order row-number helper. |
| P1 | W7 index-entry reader integration | Correct real-entry adapter first, then aggregate readers, transformation preservation, properties, and staged covering-plan support. |
| P1 | W8 vector planning plus owner handoff for GuardiANN/queues | Planner preference alone cannot complete GuardiANN. Backend and maintenance dependencies must have an explicit owner and closure evidence. |
| P2 | W12 EXPLODE flag/distinctness; W9 null-safe range alignment | Preserve default behavior and residual safety; add focused contracts. |
| P2 | W10 enum regression, W13 explain changes, W14 visitor-consumer integration | Verify existing representation where applicable; implement missing shared behavior and coordinate the external consumer. |
| Controller / language-only | W15 fixture/build changes and Java-only APIs | No independent Go algorithm port from the classified mechanical changes. |

Priority indicates sequencing, not permission to omit a required feature.

The following is the **exhaustive path ledger**. Every assigned path appears exactly once; the grouping was checked against the input list: **282 accounted for, zero missing, zero duplicates**. Prefix expansion is defined above. Each group inherits its work-unit findings and regression requirements; “test” means source-reviewed test content, not a passed test.

**LIKE — 5 paths; W1.**

```text
M/query/expressions/Comparisons.java
C/SemanticException.java
V/LikeOperatorValue.java
V/PatternForLikeValue.java
T/query/plan/cascades/LikeOperatorValueTest.java
```

**Arrays, promotion, and variadic typing — 8 paths; W2, with aggregate wrapping also used by W3.**

```text
C/NullableArrayTypeUtils.java
C/typing/Type.java
V/AbstractArrayConstructorValue.java
V/MessageHelpers.java
V/PromoteValue.java
V/RecordConstructorValue.java
V/VariadicFunctionValue.java
T/query/plan/cascades/VariadicFunctionValueTest.java
```

**ARRAY_AGG and streaming — 5 paths; W3.**

```text
V/ArrayAggValue.java
P/RecordQueryStreamingAggregationPlan.java
T/provider/foundationdb/query/FDBStreamAggregationTest.java
T/query/plan/cascades/values/ArrayAggValueTest.java
T/query/plan/plans/RecordQueryStreamingAggregationPlanTest.java
```

**Function/window APIs and macros — 17 paths; W4/W5. The two HighOrder files are deletions.**

```text
C/BuiltInFunction.java
C/CallSiteArguments.java
C/CatalogedFunction.java
C/EncapsulationFunction.java
C/RawSqlFunction.java
C/UserDefinedFunction.java
C/UserDefinedMacroFunction.java
C/WindowOrderingPart.java
V/JavaCallFunction.java
V/RowNumberHighOrderValue.java
V/RowNumberValue.java
V/UdfFunction.java
T/query/plan/cascades/CallSiteArgumentsTest.java
T/query/plan/cascades/CallSiteOptionsTest.java
T/query/plan/cascades/UserDefinedMacroFunctionTest.java
T/query/plan/cascades/values/RowNumberHighOrderValueTest.java
T/query/plan/cascades/values/RowNumberValueTest.java
```

**Call-site adaptations — 39 paths; W4/W15. These contain argument wrapping/extraction and associated mechanical edits, not additional operation-specific algorithms. `FDBInQueryTest` also receives the relocated Holder.**

```text
C/AggregateIndexExpansionVisitor.java
C/BitmapAggregateIndexExpansionVisitor.java
R/DecorrelateValuesRule.java
R/InComparisonToExplodeRule.java
R/RemoveRangeOneRule.java
V/AndOrValue.java
V/ArithmeticValue.java
V/CardinalityValue.java
V/CollateValue.java
V/CountValue.java
V/DistanceValue.java
V/ExistsValue.java
V/FromOrderedBytesValue.java
V/InOpValue.java
V/IndexOnlyAggregateValue.java
V/NotValue.java
V/NumericAggregationValue.java
V/PickValue.java
V/RangeValue.java
V/ToOrderedBytesValue.java
V/VersionFunction.java
T/provider/foundationdb/query/FDBInQueryTest.java
T/provider/foundationdb/query/FDBLongArithmeticFunctionQueryTest.java
T/provider/foundationdb/query/FDBNestedRepeatedQueryTest.java
T/provider/foundationdb/query/FDBPermutedMinMaxQueryTest.java
T/provider/foundationdb/query/FDBRecordStoreRepeatedQueryTest.java
T/provider/foundationdb/query/RecursiveQueriesTest.java
T/query/plan/cascades/ArithmeticValueTest.java
T/query/plan/cascades/BooleanValueTest.java
T/query/plan/cascades/ConstantFoldingTestUtils.java
T/query/plan/cascades/DistanceValueTest.java
T/query/plan/cascades/TypeRepositoryTest.java
T/query/plan/cascades/predicates/QueryPredicateTest.java
T/query/plan/cascades/properties/CardinalitiesPropertyTest.java
T/query/plan/cascades/rules/DecorrelateValuesRuleTest.java
T/query/plan/cascades/values/ExistsValueTest.java
T/query/plan/cascades/values/ValueTranslationTest.java
T/query/plan/cascades/values/simplification/OrderingValueSimplificationTest.java
T/query/plan/cascades/values/simplification/ValueComputationTest.java
```

**Planner architecture and rewrite behavior — 27 paths; W6. Includes matcher/API and test-helper adaptations supporting the algorithms.**

```text
C/AbstractCascadesRule.java
C/CascadesPlanner.java
C/CascadesRule.java
C/CascadesRuleSet.java
C/ConditionalCascadesRule.java
C/ExplorationCascadesRule.java
C/ExpressionPropertiesMap.java
C/ImplementationCascadesRule.java
C/PlanningRuleSet.java
C/RewritingRuleSet.java
C/matching/structure/ExpressionsPartitionMatchers.java
C/matching/structure/ReferenceMatchers.java
C/matching/structure/RelationalExpressionMatchers.java
C/properties/SelectMergeableProperty.java
R/FinalizeExpressionsRule.java
R/PredicatePushDownRule.java
R/SelectMergeRule.java
T/query/plan/cascades/CascadesPlannerTest.java
T/query/plan/cascades/ConditionalCascadesRuleTest.java
T/query/plan/cascades/MemoExpressionTest.java
T/query/plan/cascades/RuleTestHelper.java
T/query/plan/cascades/events/PlannerEventSerializationTests.java
T/query/plan/cascades/properties/SelectMergeablePropertyTest.java
T/query/plan/cascades/rules/FinalizeExpressionsRuleTest.java
T/query/plan/cascades/rules/PredicatePushDownRuleTest.java
T/query/plan/cascades/rules/SelectMergeRuleTest.java
T/query/plan/cascades/rules/TestRuleExecution.java
```

**Rule declaration adapters — 60 paths; W6/W15. Complete diffs change inheritance/interface declarations, imports, annotations, and equivalent formatting; `PushFilterThroughFetchRule` also reformats an equivalent lambda. No separate rule algorithm is introduced by these local diffs.**

```text
R/AbstractDataAccessRule.java
R/AdjustMatchRule.java
R/EliminateNullOnEmptyRule.java
R/ImplementDeleteRule.java
R/ImplementDistinctRule.java
R/ImplementDistinctUnionRule.java
R/ImplementFilterRule.java
R/ImplementInJoinRule.java
R/ImplementInUnionRule.java
R/ImplementInsertRule.java
R/ImplementIntersectionRule.java
R/ImplementNestedLoopJoinRule.java
R/ImplementRecursiveDfsJoinRule.java
R/ImplementRecursiveLevelUnionRule.java
R/ImplementSimpleSelectRule.java
R/ImplementStreamingAggregationRule.java
R/ImplementTableFunctionRule.java
R/ImplementTempTableInsertRule.java
R/ImplementTempTableScanRule.java
R/ImplementTypeFilterRule.java
R/ImplementUniqueRule.java
R/ImplementUnorderedUnionRule.java
R/ImplementUpdateRule.java
R/MatchIntermediateRule.java
R/MatchLeafRule.java
R/MergeFetchIntoCoveringIndexRule.java
R/MergeProjectionAndFetchRule.java
R/NormalizePredicatesRule.java
R/PartitionBinarySelectRule.java
R/PartitionSelectRule.java
R/PredicateToLogicalUnionRule.java
R/PushDistinctBelowFilterRule.java
R/PushDistinctThroughFetchRule.java
R/PushFilterThroughFetchRule.java
R/PushInJoinThroughFetchRule.java
R/PushMapThroughFetchRule.java
R/PushReferencedFieldsThroughDistinctRule.java
R/PushReferencedFieldsThroughFilterRule.java
R/PushReferencedFieldsThroughSelectRule.java
R/PushReferencedFieldsThroughUniqueRule.java
R/PushRequestedOrderingThroughDeleteRule.java
R/PushRequestedOrderingThroughDistinctRule.java
R/PushRequestedOrderingThroughGroupByRule.java
R/PushRequestedOrderingThroughInLikeSelectRule.java
R/PushRequestedOrderingThroughInsertRule.java
R/PushRequestedOrderingThroughInsertTempTableRule.java
R/PushRequestedOrderingThroughRecursiveUnionRule.java
R/PushRequestedOrderingThroughSelectExistentialRule.java
R/PushRequestedOrderingThroughSelectRule.java
R/PushRequestedOrderingThroughSortRule.java
R/PushRequestedOrderingThroughUnionRule.java
R/PushRequestedOrderingThroughUniqueRule.java
R/PushRequestedOrderingThroughUpdateRule.java
R/PushSetOperationThroughFetchRule.java
R/PushTypeFilterBelowFilterRule.java
R/QueryPredicateSimplificationRule.java
R/RemoveProjectionRule.java
R/RemoveSortRule.java
R/RewriteOuterJoinRule.java
R/SplitSelectExtractIndependentQuantifiersRule.java
```

**Index-entry readers and aggregate provenance — 20 paths; W7.**

```text
M/query/plan/bitmap/ComposedBitmapIndexQueryPlan.java
C/AggregateIndexMatchCandidate.java
C/IndexEntryToRecordValueHelper.java
C/ScanWithFetchMatchCandidate.java
C/ValueIndexScanMatchCandidate.java
C/WindowedIndexScanMatchCandidate.java
C/properties/CardinalitiesProperty.java
C/properties/DerivationsProperty.java
C/properties/OrderingProperty.java
C/properties/PrimaryKeyProperty.java
C/properties/StoredRecordProperty.java
P/RecordQueryAggregateIndexPlan.java
P/RecordQueryCoveringIndexPlan.java
P/RecordQueryCoveringIndexValuePlan.java
P/RecordQueryPlanWithIndexEntryToQueriedRecord.java
T/provider/foundationdb/query/FDBCoveringIndexValuePlanTest.java
T/provider/foundationdb/query/GroupByTest.java
T/query/plan/cascades/AggregateIndexEntryToRecordValueTest.java
T/query/plan/cascades/IndexEntryTranslatorEquivalenceTest.java
T/query/plan/explain/ExplainPlanVisitorTest.java
```

**Vector planning — 7 paths; W8.**

```text
M/query/plan/RecordQueryPlannerConfiguration.java
M/query/plan/VectorIndexEnginePreference.java
C/PlanningCostModel.java
C/VectorIndexExpansionVisitor.java
C/VectorIndexScanMatchCandidate.java
T/query/plan/RecordQueryPlannerConfigurationTest.java
T/query/plan/cascades/PlanningCostModelVectorEngineTest.java
```

**Explain/display — 13 paths; W13, with covering-plan visitor support also belonging to W7.**

```text
M/query/expressions/RecordTypeKeyComparison.java
C/explain/ExplainPlanVisitor.java
C/expressions/DeleteExpression.java
C/expressions/InsertExpression.java
C/expressions/LogicalTypeFilterExpression.java
C/expressions/UpdateExpression.java
V/SubscriptValue.java
P/RecordQueryInsertPlan.java
P/RecordQueryTypeFilterPlan.java
P/RecordQueryUpdatePlan.java
T/query/plan/cascades/expressions/DeleteExpressionTest.java
T/query/plan/cascades/expressions/InsertExpressionTest.java
T/query/plan/cascades/expressions/UpdateExpressionTest.java
```

**DML isolation — 4 paths; W11.**

```text
P/QueryPlanUtils.java
P/RecordQueryAbstractDataModificationPlan.java
P/RecordQueryDeletePlan.java
T/provider/foundationdb/query/FDBModificationQueryTest.java
```

**EXPLODE — 5 paths; W12. `DistinctRecordsProperty` also gains covering-Value-plan delegation under W7.**

```text
C/expressions/ExplodeExpression.java
C/properties/DistinctRecordsProperty.java
R/ImplementExplodeRule.java
P/RecordQueryExplodePlan.java
T/query/plan/plans/ExplodePlanTest.java
```

**Null-safe ranges and enum comparisons — 4 paths; W9/W10.**

```text
M/query/plan/ScanComparisons.java
C/predicates/RangeConstraints.java
V/RelOpValue.java
T/query/plan/cascades/MergeComparisonRangesTest.java
```

**Value visitor API — 3 paths; W14.**

```text
V/SimpleValueVisitor.java
V/Value.java
T/query/plan/cascades/values/ValueVisitorTest.java
```

**Java mechanical changes — 25 paths; W15. Complete diffs contain annotation/import removal, documentation, equivalent local-variable typing, or the logging-extension import migration.**

```text
M/query/plan/AvailableFields.java
M/query/plan/RecordQueryPlanner.java
C/CascadesRuleCall.java
C/IdentityBiMap.java
C/PartialMatch.java
C/PlanContext.java
C/PlannerRule.java
C/PlannerRuleCall.java
C/ValueEquivalence.java
C/ValueIndexExpansionVisitor.java
C/events/PlannerEventStatsCollectorState.java
C/expressions/TableFunctionExpression.java
C/matching/graph/MatchPredicate.java
C/matching/structure/SetMatcher.java
C/predicates/Placeholder.java
C/predicates/QueryPredicate.java
C/predicates/simplification/AbstractQueryPredicateRuleSet.java
C/properties/PredicateCountByLevelProperty.java
V/simplification/AbstractRuleSet.java
V/simplification/AbstractValueRuleSet.java
V/simplification/CollapseRecordConstructorOverFieldsToStarRule.java
V/simplification/ComposeFieldValueOverRecordConstructorRule.java
M/query/plan/planning/BooleanPredicateNormalizer.java
P/RecordQueryPlan.java
T/provider/foundationdb/query/FDBSimpleQueryGraphTest.java
```

**Factory visibility and equivalent index-state access — 2 paths; W15.**

```text
C/TempTable.java
M/query/plan/synthetic/SyntheticRecordPlanner.java
```

**Retired serialized Value rejection — 1 path; W4 and the compatibility table.**

```text
T/query/plan/serialization/PlanSerializationTest.java
```

**Fixture migration/extraction — 37 paths; W15. This exhaustive list includes the 32 byte-identical moved classes, two modified/extracted bases, the extracted JRE runner, and two documentation files described above.**

```text
T/provider/foundationdb/query/FDBCollateJREQueryTest.java
F/provider/foundationdb/query/DualPlannerExtension.java
F/provider/foundationdb/query/DualPlannerTest.java
F/provider/foundationdb/query/FDBCollateQueryTestBase.java
F/provider/foundationdb/query/FDBRecordStoreQueryTestBase.java
F/provider/foundationdb/query/package-info.java
F/query/plan/match/AnyFilterMatcher.java
F/query/plan/match/AnyParentMatcher.java
F/query/plan/match/ComposedBitmapIndexMatcher.java
F/query/plan/match/CoveringIndexMatcher.java
F/query/plan/match/DescendantMatcher.java
F/query/plan/match/EveryLeafMatcher.java
F/query/plan/match/FetchMatcher.java
F/query/plan/match/FilterMatcher.java
F/query/plan/match/FilterMatcherWithComponent.java
F/query/plan/match/InComparandJoinMatcher.java
F/query/plan/match/InParameterJoinMatcher.java
F/query/plan/match/InValueJoinMatcher.java
F/query/plan/match/IndexMatcher.java
F/query/plan/match/IntersectionMatcher.java
F/query/plan/match/NamedPredicate.java
F/query/plan/match/PlanMatcherWithChild.java
F/query/plan/match/PlanMatcherWithChildren.java
F/query/plan/match/PlanMatchers.java
F/query/plan/match/QueryPredicateDescendantMatcher.java
F/query/plan/match/ScanComparisonsEmptyMatcher.java
F/query/plan/match/ScanComparisonsStringMatcher.java
F/query/plan/match/ScanMatcher.java
F/query/plan/match/ScoreForRankMatcher.java
F/query/plan/match/SortMatcher.java
F/query/plan/match/TextIndexMatcher.java
F/query/plan/match/TypeFilterMatcher.java
F/query/plan/match/TypelessStringMatcher.java
F/query/plan/match/UnionMatcher.java
F/query/plan/match/UnorderedPrimaryKeyDistinctMatcher.java
F/query/plan/match/UnorderedUnionMatcher.java
F/query/plan/match/package-info.java
```