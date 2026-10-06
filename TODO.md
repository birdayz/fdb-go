# Java 4.14.2.0 migration

Active branch: `upgrade/java-4.14.2.0` · PR #786. FDB/C++ remains **7.3.77**.

## Execution contract

**Active: WS-E. Finish it before starting another workstream.** Work in the order
below; do not substitute an easier item or reopen the archived backlog as a new
parallel campaign. A completed patch is not a completed workstream.

During coding use **`just test`**, with normal Bazel caching. Implement coherent
batches, not a suite invocation after each edit. At a workstream boundary use
**`just test-full`** and the required Java/FDB acceptance checks. Completion means
implemented behavior, regression coverage that exercises it end to end, truthful
compatibility notes, and a green workstream—not merely types or rules that exist.
The final migration receives the milestone review; no intermediate RFC rounds.

`TODO_OLD.md` preserves the old backlog and evidence, not the active execution
order. Historical audit/design source (not a current completion claim):

```
git show 13d5a3d1e:rfcs/257-java-4.14.2.0-upgrade.md
git show 13d5a3d1e:rfcs/257-java-upgrade-audit/ws-e-design.md
# Other workstreams: replace ws-e with ws-d, ws-f, etc.
```

Verify remaining audit claims against current Go and Java before changing code.
Never mark a whole workstream complete because one of its subitems passed.

## 1. WS-E — scalar/SQL semantics and statement execution — ACTIVE

- [x] Invalid UTF-8 rejection on SQL, nested bindings and record writes; copied
  byte/VECTOR bindings (`7875cb7b7`).
- [x] Parameter typing order: known types before Valuer, typed pointers/null
  wrappers, fixed/named byte arrays and slices, empty-array typing, nested-array
  refusal (`564c52f48`).
- [x] Temporal values (`bd59131f7` and its completion): one parser; a bound
  `time.Time` is UTC TIMESTAMP text (22008 outside 0000–9999, arrays included,
  sub-second dropped); DATE→TIMESTAMP and DATE/TIMESTAMP→STRING promotions with
  NULL arms, injected for comparisons, both IN forks, CASE, COALESCE, IFNULL,
  GREATEST and LEAST (IF/IIF are not SQL-reachable: "Unsupported operator IF").
  Pins: `yamsql/testdata/temporal_promotion.yaml` (index and residual, IN forks,
  merging operators, bound times, column types incl. DATE/TIMESTAMP-spelled
  columns as STRING, assignment 22000), `expr/temporal_promotion_property_test.go`
  (producers × operators × NOT × commute, GREATEST/LEAST; mutation-checked),
  binder and promotion unit tests. The explain-differ harness now binds a
  scenario's `args`. OWNER RULING (2026-10-05): only current behavior is
  supported; no legacy mixed-text pins or repair procedure. Full-lane copies
  (`sqldriver` param-rendering and temporal-comparand pins) are updated but
  were not run.
- [x] Bit-exact bindings and floating NaN bits: CAST(string AS DOUBLE/FLOAT) is
  `javanum` (Java's grammar, single binary32 rounding, canonical NaN; replaces
  recordlayer's private parser); MIN/MAX are `Math.min`/`Math.max` line for
  line (NaN operand bits); the plan-cache key renders bound values exactly
  (TRUE/FALSE shared a plan: wrong rows). Pins: `javanum_test` (+30 s fuzz),
  `TestCastValue_StringToFloatingIsJavas`, `TestAggMinMax_ReturnsTheNaNOperandsBits`,
  `param_cache_key_test.go` (red with the old key), `bound_parameter_bits.yaml`.
  The Java-oracle pins (`ws_e_probe_conformance_test.go` v11/v12/cross) are
  flipped to the target's outcomes but not yet run (full lane).
- [x] Terminal NaN equality over a value index or primary key: the binder reads
  both NaN key blocks (`nanBlockTails`), the planner binds a NaN equality as the
  terminal component and prices two seeks, and a NaN never pins ordering.
  Aggregate-index and vector-partition keep the refusal; DIVERGENCES.md "A NaN
  equality over an index returns every stored NaN". Pins:
  `nan_block_binding_test.go`, `nan_index_equality.yaml` (ORDER BY row
  mutation-checked).
- [x] Non-terminal NaN equality: the two NaN blocks plus a key filter on later
  components (`scanKeyFilter`, `filterScanKeys`), below the continuation, for
  value-index and primary-key scans; filter comparands in the fingerprint (NaN
  and ±0 as classes). Pins: `TestNaNEqualityKeyFilterSelectsExactly` (equality,
  zero, range tail, second NaN, reverse, NULL), `TestKeyFilterCursorForwardsContinuations`
  (limit expiring on a rejected entry), and the FDB test
  `TestFDB_DynamicNaNCompositeIndexKeyFilter` (was the refusal pin; DOUBLE and
  FLOAT, forward/reverse, scan limits 0/1/2) — the last is in `sqldriver_test`
  (full lane) and has NOT run yet. The primary-key filter's record-type-prefix
  offset has no end-to-end pin: SQL plans for a dynamic NaN PK prefix with a
  bound suffix pick a scan on the corpus tables.
- [x] Conflict-free index-state reads (design 6.4): planning and per-page
  revalidation read `PeekIndexStates` (no conflict; Java's PlanContext), scans
  keep their per-index key, `GetAllIndexStates` takes one range over the
  index-state subspace (Java's getAllIndexStates). Pins: Ginkgo
  `index-state read conflicts` (fast lane) and `TestFDB_IndexStateReadScope`
  (sqldriver, full lane, not run: scanned index conflicts; unused index and
  record scan commit).
- [x] Connection options (design 6.2): `EmbeddedConnection.SetOption` merges
  one option checked by Java's contracts (`api.ValidateOption`, 22023);
  `SetOptions` replaces and is what `ResetSession` restores; DSN `dry_run` and
  `isolation_level_snapshot`; an unknown DSN parameter is 22023 listing the
  accepted names; a result set reads the options captured at execution on
  every page. Connection DRY_RUN/SNAPSHOT were already merged per statement.
  Pins (fast lane): `option_contracts_test.go`, `connection_option_scope_test.go`,
  `dsn_test` (new fast-lane target). FDB pins in `dml_dry_run_fdb_test.go`
  (Raw DRY_RUN lasts one borrow, DSN dry_run persists, DDL ignores DRY_RUN) —
  full lane, not run.
- [ ] IN semantics: rewrite/partition/cost behavior, covering unions, multi-binding
  product limit, and constant-IN evaluation timing; coordinate shared machinery
  with WS-F without losing either acceptance obligation.
  The product limit is done (F-7b item 5, after item 2 removed its blocker).
  The executor checks first (Java RecordQueryInUnionPlan.java:151-153, with a
  saturating product declared in DIVERGENCES.md), the relational maximum is
  24, and both rule arms carry it. Pins: `TestInUnionValuesSize`,
  `TestExecuteInUnion_MaxSizeBoundsProduct` and `in_union_max_size.yaml`
  (24 runs, 25 and 5 x 5 fail, 4 x 6 runs).
  The single-element collapse deletion of step (5) was measured on F-7b's tree
  and reverted. Fifteen corpus plans move. Two of them,
  `category IN ('food') ORDER BY id` and `status IN ('active') ORDER BY id`,
  go from a streaming index scan to `InMemorySort(InJoin(...))`. The design
  measured these as IN-unions, but F-7b item 2 now builds no in-union ordered
  by an id past the record-type coordinate. Java plans a scan with a filter.
  A one-value `IN (?)` ordered by the primary key would buffer its whole
  result. Before the collapse goes, either F-7c's cost model has to choose
  the streaming plan, or a one-literal in-join has to bind its value FIXED, a
  Go extension that needs an owner decision. Decided with F-7c (2026-10-06):
  the cost model keeps the sorted probes for a multi-value IN (DIVERGENCES.md
  "an IN ordered by a key no probe provides"), so the collapse stays; its
  deletion would turn the one-value form into the same sorted probe. It is
  declared (DIVERGENCES.md "a one-value IN is an equality"), and the WS-F
  row `w8_rl_default_in1_order_by_price` is `DIFF-PATH single-element-in`
  (the oracle's acceptance now reads DIFF-PATH for record-layer rows).
- [ ] Semantics/pins: scalar variadic promoted-child types, Value nullability
  census, target simplification regime, adjacent/decorated literals and lexer
  boundaries, FROM-less metadata, LOG_QUERY. Decimal normalization and structured
  variadic promotion have prior implementations; check current coverage first.
  Simplification regime (design 5.4), in progress.
  - (b): the generic fold (`DefaultFolder`, `ExpressionFolder`,
    `SimplifyAll`) is deleted; it had no non-test caller.
  - (a): 17 of the 18 translator `SimplifyPredicateValues` sites are gone, so
    a SQL predicate reaches Cascades unfolded. No corpus plan moved: the
    planner's simplification rule folds the same constants.
    - The sparse-index DDL path now folds its stored predicate itself, which
      keeps the enum and UUID refusal messages.
    - The EXISTS bound-query site keeps its fold. Without it
      `foldable_colliding_answers` (`COALESCE(TRUE, ST."C" = 1)` over a
      shadowing inner ST) would decline as scope-ambiguous where Java
      answers. It goes with the mint-per-leg inner-shadow fix.
  - (d): Java's COALESCE rule. Only a NULL head (NullValue, a nil constant)
    or a BOOLEAN literal head folds; an INT/STRING literal head stays, so
    `COALESCE(1, 1/0) = 1` in a WHERE raises 22012 as in Java.
    `simplification_regime.yaml` holds the 30 Java-measured rows. With an INT
    head a colliding EXISTS reference no longer folds away, so
    `case1_notexists_colliding_foldable` and `int_head_colliding_declines`
    (sqldriver, full lane, not run) now decline as scope-ambiguous until the
    mint-per-leg fix.
    - Known gap, part of (e): `NOT CAST(NULL AS BOOLEAN)` inside a predicate
      value is not folded. Go keeps `NOT x` as a predicate value, which the
      simplifier does not enter (`coalesce_not_cast_null_head_where` is
      excluded from the yaml with that note).
  - (c), first half: Java's ConstantFoldingRuleSet is ported as
    `constantFoldingRules` (`rule_constant_folding.go`): the default predicate
    rules, `ValuePredicateSimplificationRule` (the leaves' PREDICATE value set,
    with the ranges and multi-constraint folds) and
    `ConstantFoldingValuePredicateRule` over effective constants
    (`predicates.FoldComparisonMaybe`, Java's `foldComparisonMaybe`; Go's
    boolean ValuePredicate folds as `value = TRUE`). Both of Java's consumers
    run it: QueryPredicateSimplificationRule over the whole conjunction, and
    `foldPredicateAtNull`, which no longer runs the Go-only
    `DefaultSimplifyRules` (EvaluateConstant comparison folds, NOT over a
    constant). No corpus plan moved; sqldriver, factorycorpus and javacorpus
    pass.
  - (c), second half: the Go-only driver set is deleted: `DefaultSimplifyRules`,
    `NormalizationRules`, AndFlatten, OrFlatten, AndDedup, OrDedup,
    ComparisonConstantSimplify and ValuePredicateConstantFold, with their
    tests. The tests that used the set as an oracle (`expr` walk, resolver
    and fullstack tests, the simplify fuzzers, `simplifier_test.go`, the De
    Morgan tests) run Java's set and are re-pinned to its answers: `5 = 5`
    and `(1 + 2) > 0` stay, NOT over a constant stays, NOT over OR
    distributes. `NotConstantSimplifyRule` stays, in
    `TranslatorConstantPredicateRules`, for the translator's EXISTS fold.
  - (e): Java's CollapseNullStrictValueOverNullValueRule is in both value sets
    (`values.collapseNullStrict`, the six classes of `IsNullStrictValue`, which
    the null-on-empty substitution shares; Go's boolean NULL literal
    `BooleanValue{nil}` counts as a NULL). `CAST(NULL AS T)` is the typed
    NullValue (`ResolveCast`, Java's `CastValue.inject`), and an IN item is
    refused at plan time only when NULL-TYPED, a typed NULL when the list is
    evaluated, as before. NOT over a bare boolean value is Java's `NotValue`
    (EXPLAIN `NOT _.B`), so `COALESCE(NOT CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1)`
    answers every row as Java does (`coalesce_not_cast_null_head_where`, now in
    `simplification_regime.yaml`). NOT over a literal no longer folds
    (`NotValue` left `isFoldableComposite`): Java keeps `NOT 'false'`. The WS-E
    oracle: one row fixed, none moved the other way. The substitution keeps
    its collapse because Go cannot rebuild a FieldValue over a NullValue.
  - (j), the oracle half: the WS-E oracle asserts Go's side of the 89
    simplification rows of its v4, v5, v6 and v8 rounds (`wsE5GoPins` to
    `wsE8GoPins`, `checkGo`): every pin probed and answered exactly, and every
    row answering as the target does (`wseOutcome`: outcome class, SQLSTATE,
    rows) unless `wseGoDivergences` declares it. Eight are declared: the
    target's VerifyException and CASE-branch defects (5.4(h)) and two
    REWRITING hash ties Go's prune decides for the fold (5.4(g)). A moved pin
    was checked to redden it (full lane). Go's EXPLAIN shows `[n preds]`, so the
    surviving member's predicate (`[FALSE]` against `n > 0`) is not asserted
    there; that needs a predicate-showing EXPLAIN.
  - The `EvaluateConstant` arms of `SimplifyValue` are gone: neither value
    set evaluates a constant composite (`1 + 2`, `CAST(3 AS STRING)`,
    `UPPER('x')`, a CASE or a promotion stays for the plan to compute), and
    NOT over a literal no longer folds. The one evaluation left is
    `values.EvaluateConstantComparand` / `predicates.EvaluatePredicateComparands`,
    for the sparse-index predicate, which Java stores with
    `comparison.getComparand(null, null)` (IndexComparison.java:166); it keeps
    the enum and UUID refusal messages. Measured: no corpus plan or EXPLAIN
    moved with the arms removed. The remaining `EvaluateConstant` callers read
    comparands for plan properties (equality shapes, ordering, intermediate
    matching) or translator checks and rewrite nothing.
  - (f) closed: `effectiveConstant` already has Java's three shapes, and the
    Object overload has no Go caller (Go comparands are Values).
  - (g), the conjunct rung: `residualConjuncts` drops tautologies as Java's
    NormalizedResidualPredicateProperty does (java:81-121), so `[A, TRUE]`
    beats `[A, B]` on the count instead of tying into the hash
    (`TestRewritingCostModel_TautologyCountsNoConjunct`). Corpus golden
    unchanged; the WS-E oracle passes with no Go pin moved. The dense
    predicate-level map is WS-F's (Finding 6-followup), and the hash rung stays
    Go's `deepHash` (no `semanticHashCode` port).
  - (g)/(j) tie pins: `TestFoldTiePins_DecidingRung` (embedded, fast lane)
    plans each tie row 20 times cold over the v5 and v4 schemas and asserts
    the plan, the kept member's predicates and the deciding rung. Mutation
    runs: inverting REWRITING's `deepHash` flips the six rewriting-hash rows
    (the five div0 rows keep `NULL = 1`, the NOT row keeps `NOT (FALSE)`) and
    nothing else; inverting PLANNING's `costExprHash` flips only
    `in_cast_null_join_inner_empty` (NLJ sides swap). Both redden the test.
    `null_strict_div0_cast_null_is_null_where` and the COALESCE-under-AND row
    are decided on the conjunct count.
    Go-only: the walker collapses arithmetic over a typed NULL and folds IS
    [NOT] NULL over a NOT NULL constant (`expr.ResolveArithmetic`,
    `ResolveIsNull`). So the div0 pair is `NULL = 1` vs `UNKNOWN`, both
    answering no rows, and the hash picks the plan, not the answer; and
    `COALESCE(1 / 0, 5) IS NULL` reaches the memo already `FALSE`. Measured
    with both folds removed: the pairs become Java's, Go's hash then keeps
    the unfolded `COALESCE(1 / 0, 5) IS NULL` (22012 where the target
    answers `[]`), and six projections lose their NULL collapse (arithmetic
    yaml #14-17 and #19, simplification_regime #25), because Go has no
    DEFAULT-set simplify of result values at pull-up (Expression.java:
    243-245). The walk-time folds stay until that pull-up simplify is
    ported.
  - (j) prune scenarios: `fold_prune_regime.yaml` (round 8: T with T_N) and
    `fold_prune_unique_regime.yaml` (round 9: UNIQUE T_UN, the union leg)
    carry every round-8/9 fold row at Java's measured answer: literal
    annulments 22012 (`1 = 2` is no effective constant), the IN-list row
    22012 as declared, reductions, type annulments, type reductions, the tie
    folds and the deduplicated variants of `COALESCE(1 / 0, 5) IS NULL`
    (Go keeps the duplicates; the fold wins on the count, added to the tie
    pins). Plan-only difference: Java scans T_N `<,>` under `FILTER false`,
    Go scans T.
  - (j) negative control: `TestFoldPrune_NegativeControl` (cascades) ranks
    the original and the fold directly. REWRITING keeps the fold; PLANNING
    over the implemented plans prefers the unfolded probe for the
    primary-key, primary-key IN, UNIQUE-index (provable cardinality 1) and
    union-leg groups. So the REWRITING prune decides these folds.
  - (f)/(j) the rest: `effectiveConstant` is now exactly Java's
    `from(Value)` (a non-boolean literal takes the type arm; only an
    untyped constant changes, from NOT_NULL to UNKNOWN; corpus unchanged),
    pinned shape by shape (`TestEffectiveConstant_JavaShapes`). The COALESCE
    head classes not yet pinned (a `NOT FALSE` head never folds at a
    fixpoint, an arithmetic head is not evaluated, a `CAST(NULL)` head
    collapses and is skipped) are pinned per set
    (`TestSimplifyCoalesce_HeadClassesPerSet`). `rejectsNull` is pinned over
    13 rejecting and accepting shapes through the ported set
    (`TestRejectsNull_ShapesThroughThePortedSet`). `NOT (k = 5)` is not
    proven in either engine: children fold first, and the set has no NOT
    over a constant.
  - (h): DIVERGENCES.md "Constant-fold defects and fold ties" records the
    target's VerifyException (COALESCE under AND/NOT, beside an IN list) and
    CASE-branch defects with Go's answers, and the schema-dependent hash ties.
  - (i): no set evaluates a LIKE pattern (`TestPatternForLikeValue_NeverEvaluated`).
  - (b) census: `//pkg/recordlayer/query/plan/constfoldcensus` parses every
    non-test Go file under pkg/ (1113; the target follows MODULE.bazel's
    runfiles symlink to the real tree and is tagged `external`, never
    cached). It counts every reference to the simplification and
    constant-evaluation entry points, plus every `Evaluate(nil)` /
    `Eval(nil)`, keyed by file, function and callee. All 41 keys are
    assigned a reason (simplify, comparand, analysis, IN, LIKE, coercion,
    walk-time NULL, Go-only, harness), with a vacuity floor, a positive
    control and a matcher arm test. Found by it: the CAST arm evaluated
    `tryCastConstant` before checking the evaluate mode (result discarded);
    it now checks first.
  Done: LOG_QUERY (statement and connection) sets `PlanGenerationInfo.LogQuery`
  (`TestPlanLogging_LogQueryFlag`). Literal decoding and the decorated-literal
  refusals were already implemented and now have a fast-lane pin
  (`string_literal_tokens.yaml`); lexer comment boundaries are pinned in
  `parser/parser_test.go` `TestParse_Comments`. COALESCE/GREATEST/LEAST and CAST
  nullability already follow Java (`ScalarFunctionValue.Type`, `CastValue.Type`).
  A CAST never yields NULL from a non-NULL operand: an operand no arm converts
  is a cast error, and a RECORD cast passes an equal type through
  (`TestCastValue_NonNullOperandNeverCastsToNull`, every admitted pair).
  The per-class nullability table is `valueNullabilityCensus`
  (`values/value_nullability_census_test.go`). It enumerates every Type()
  class from the embedded sources and checks each fixed type against its entry.
  Every class Go typed NOT NULL where Java types it nullable is loosened, and no
  `censusGoStricter` entry remains. This covers AndOr, Not, Exists,
  EvaluatesTo, ToOrderedBytes, Rank, RowNumber, RecordType, Incarnation,
  ConditionSelector, Collate, Distance, the *DistanceRowNumber values and Empty.
  Java's oracle measured `not_false_select` and `true_and_true_select` as
  nullable.
  FROM-less metadata: `TestFromlessSelect_ResultMetadata` (fast lane) pins
  result types and nullability to the oracle's measured Java values for the
  same expressions. `FromlessSelectJavaProbe` (full lane, not run) now compares
  nullability between the engines.
  Variadic promoted-child types: COALESCE/GREATEST/LEAST promote every argument
  to the common type with its own nullability, scalars included
  (`variadic_promotion.yaml`).
  Simplification regime (design 5.4): (a)-(j) implemented and pinned (the
  sub-bullets above). Remaining, each owned elsewhere:
  - the EXISTS bound-query fold stays until the mint-per-leg inner-shadow fix;
  - the dense predicate-level map is WS-F's (Finding 6-followup);
  - the walk-time typed-NULL arithmetic collapse and the IS NULL fold over a
    NOT NULL constant are KEPT as Go extensions (DIVERGENCES "Constant-fold
    defects and fold ties"): without them the answer of a fold tie depends on
    the hash, as Java's does per schema. Removing them would also need the
    pull-up DEFAULT-set result simplify (Expression.java:243-245), which Go
    lacks.

**Done:** every remaining WS-E design obligation is reconciled to implementation
and an executable pin; temporal compatibility/repair documentation is shipped;
fast and full lanes plus Java/FDB acceptance pass. Then move to WS-F.

## 2. WS-F — planner scheduling, query blocks and properties

- [ ] Conditional decorrelate→simplify and merge→pushdown rule chains with
  progress-driven fallback; partition-based select merge; multi-leg pushdown;
  physical REWRITING prune; full comparator configuration; per-partition yields.
  The physical REWRITING prune (F-5) is done.
  - Measured on the whole plan corpus before the change: every group crossed into
    PLANNING with exactly one final, and every child the REWRITING comparator
    descended had exactly one. The designated final was degenerate.
  - The crossing now requires exactly one final (`RewritingCrossingError`, Java's
    advancePlannerStage Verify) and no longer carries members.
  - The comparator reads each child's one final (`rewritingComparator`,
    `RewritingPruneError`).
  - `designated_final.go`, its coherence instrument, the finals generation and
    the DIVERGENCES entry are deleted.
  - Two gaps the invariant exposed are fixed. First, `ImplementDistinctFinalRule`
    and `ImplementSortRule` minted plans as exploratory members of
    canonical-stage groups; they now use Java's memoizePlan. Second, a memo
    merge into a never-explored group inherited the loser's exploration
    progress, so no rule ran on the survivor's members (the asymmetric-union
    no-plan shape). `Reference.Absorb` now keeps constraints but not progress.
  - No corpus plan moved. The full lane (sqldriver and conformance) has not run
    against the invariant.
  The conditional chains, finalization partitions and pruned-input rules were
  already in place (`d5a5132a1`). F-7a is done: the planner's and the rule
  calls' comparators rank with the whole planner context (configuration and
  metadata) without statistics too; `costModelDiagnosticsOnlyContext` is
  deleted. Two corpus plans moved, both a self-join's inner leg, from a primary
  key range with a filter to the index equality probe, as PREFER_INDEX ranks
  them (`join_optimization_probes.yaml#4`, `multi_feature_integer.yaml#3`).
  D2 (rule dependencies) is done. The 23 Java rules that declare a constraint
  (`ImmutableSet.of(REQUESTED_ORDERING | REFERENCED_FIELDS)`) declare it in Go
  (`ConstraintDependencies`), as do the three Go-only readers
  (AggregateDataAccess, ImplementLimit, PushRequestedOrderingThroughFilter).
  An undeclared rule now has Java's empty set, so a re-exploration re-queues
  only rules whose declared key changed since the group's last committed
  exploration (`shouldPushRule`); Go's group-wide re-arm still re-queues all.
  `TestRuleConstraintDependencies_DeclareWhatTheyRead` fails any rule method
  that reads a constraint its type does not declare. No corpus plan moved;
  tasks fall 4-24% (chains 3/4/5: 564→546, 2367→2219, 13968→12985; star
  2559→2379; right-deep hub+6 9308→7071), pins re-baselined. D1 (progress)
  and D3/D4 (conditional chains on progress) were already in place.
  Still open: D5's re-arm conversion (Go's group-wide `lastRearmTick` re-arm
  to Java's per-expression forced exploration). The `outerJoinCount` review
  is closed (2026-10-06): the design kept it omitted with a PLANNING
  re-derivation of `RewriteOuterJoinRule`, and WS-J's v36 fold superseded
  that. `outerJoinCount` is the REWRITING comparator's first criterion, as
  in Java (`rewritingComparator.compare`, LEFT OUTER selects), and
  `RewriteOuterJoinRule` runs only in REWRITING (`RewritingRules`); no
  PLANNING registration remains (DIVERGENCES.md, the REWRITING cost model).
  F-8 is done (2026-10-06). ImplementTypeFilterRule is
  Java's per-partition rule. Over each stored-record partition of the inner,
  a plan the filter already covers is yielded bare and the others are grouped
  by kept types into a TypeFilterPlan over `MemoizeMemberPlansFromOther`; no
  winner is pre-selected. No corpus plan moved; tasks fell (chains 3/4/5
  546→526, 2219→2155, 12985→12775; star 2379→2355). Insert and temp-table
  insert yield per plan partition; intersection legs range over every
  ordering-satisfying stored-record member (spines pinned) in one fresh
  reference; the level union's legs range over their rolled-up partitions;
  the DFS join yields one plan per initial plan under the insert, its
  recursive leg over the rolled-up partition below the insert. None moved a
  corpus plan. Kept, in DIVERGENCES.md "Implementation rules that still
  choose a child": ImplementLimit (approved Go extension) and the NLJ, whose
  preserve-winner pick is the local choice Java's rolled-up inner partition
  makes. Per-inner-member yields were measured and reverted: 3 golden and 15
  factory plans moved, several to a fetch before the residual; per outer
  member too, the fixed-factor harness hits the task cap.
  D5 measured and reverted (2026-10-06): forcing only the
  members past the last round's count, with a re-arm no longer re-queuing
  every rule, moved no corpus plan but RAISED tasks (4-table chain 4500→4818,
  5-table 20557→21836) and broke `TestUnorderedUnionFetchSchedulingRetainsFutureFetchLeg`
  (the union's fetch lift is lost) and the member-invalidation pins of
  `rule_error_propagation_test.go`. Go's re-arm also stands in for a CHILD's
  membership change (a parent rule over a member whose child gained a
  member must re-fire), which Java gets from bottom-up task order, not from
  forcing. The conversion needs that ordering first. The large join is not cured by D2: a 6-table FK chain takes
  134894 tasks (5 tables: 20k), at Java's own count (122839; see the
  large-join item below).
- [x] Reconcile query-block acceptance with the current translator: top-level
  Sort(Select), ORDER BY resolution against projected Values, DISTINCT ordering,
  index-DDL root handling and ordered IN. Old blocker prose in `TODO_OLD.md`
  predates later fixes; do not reimplement the already-landed single-Select port.
  Reconciled (2026-10-06), each against the current tree:
  - Top-level Sort(Select): `topLevelSort` states every top as Java's
    `generateSelect` does, an unsorted sort over the block, and now a bare
    table read as a block Select under it (`TestTranslateScan` and the
    `queryBody` helper); an ORDER BY is `sortedBlock`'s Sort over the block's
    Select with the keys pulled up, and an unprojected key adds the Select
    above the sort (`orderBySorts`).
  - ORDER BY against projected Values (old blocker b): over a join `ORDER BY
    a.id` is sort-free with the ordered outer, projected or not
    (`ambiguous_column.yaml`, golden `Map(FlatMap(outer=Scan(A), ...))`).
  - DISTINCT ordering (old blocker a): `SELECT DISTINCT category FROM t ORDER
    BY category` keeps its order (`distinct_order_by.yaml`, golden).
  - Index-DDL root: `ddl.checkTop` is `DdlVisitor.java:274`'s `viewPlan
    instanceof LogicalSortExpression` (`index_ddl_resolver_arms_test.go`).
  - Ordered IN (old blocker c): the ordered in-join is DIVERGENCES.md's
    RFC-191 entry, and IN ordered by an unprobed key is the in-memory-sort
    entry; both declared in the WS-F oracle.
- [x] Explicit raw KEY/VALUE readers, ordered-bytes evaluation, extraction trie,
  covering reader/Value plan, aggregate cardinality/distinctness/entry readers
  (`c1ad5a87d` through `d819eb25e`). Plan transport remains outside this closure.
- [ ] IN-union product limit/size, null-safe singleton candidates, zero-based
  EXPLODE ordinality/distinctness, subscript typing/errors, display-only EXPLAIN
  decoding, ordered Value folding, vector-preference applicability pins.
  Null-safe singleton candidates (W9, design section 5) are done.
  NOT_DISTINCT_FROM binds index and PK scans: a NULL operand reads the null
  key, and the literal may be on either side. IS DISTINCT FROM stays residual,
  and a sparse IS NOT NULL index serves only a non-NULL literal. EXPLAIN shows
  the bound as `≡`. Pins: `null_safe_equality_scan.yaml`, which includes the
  #4598 keyset shape with bound parameters. The seven w9 oracle rows' Go pins
  are predicted and moved out of `wsfOpenUntil` (full lane, not run). Still
  open in W9 is item 4, the range builder's `isCompileTime` port, which is
  latent because it has no consumer. The table-function form of the keyset
  query needs WS-E's simplification regime.
  Zero-based EXPLODE ordinality and distinctness (W12, F-2) are done. Both
  Explode classes take Java's zero-based flag through checked constructors and
  every rebuild; it is in equality, and in the hash only when set. The executor
  numbers the whole list from 0 or 1 before resume and skip/limit, and an
  ordinality explode reports distinct records (one- or zero-based). Pins:
  `TestExecuteExplode_ZeroBasedOrdinality`, `TestExplodePlan_ZeroBasedOrdinality`,
  `TestExplode_ZeroBasedOrdinality`, `TestExplodePlan_DistinctRecordsIffWithOrdinality`
  and `unnest_at_distinct.yaml`. No corpus plan moved, since Go's FlatMap claims
  no distinctness, so a DISTINCT above an `AT` unnest stays. Subscript
  typing/errors were already ported, and the oracle pins them (`w13_subscript_*`).
  F-1 leftovers. `w10_enum_not_distinct_explain` reaches its target with W9; its
  Go pin is updated. `w10_enum_distinct_explain` is the IS DISTINCT FROM covering
  scan Java picks under PREFER_INDEX, so it is reassigned to F-7c, like
  `w9_distinct_explain`. `w13_display_scan_explain` (a dotted escaped table got
  no PK scan) is at SAME-PATH with RFC-238 §7c, ACKed on 2026-10-05 and done:
  the scan leaf and the INSERT, UPDATE and DELETE targets carry the storage
  name, UPDATE carries its correlation separately (`WithTargetAlias`),
  `AddGeneratedIndex` matches by storage name, and EXPLAIN decodes the
  record-type name as Java 4.14 does (#4437). Escaped tables now get PK ranges,
  secondary indexes and aggregate indexes. The same pass fixed a regression
  from WS-E 5.4(a): with the translator no longer folding `CAST(3 AS BIGINT)`,
  range enclosure saw a CastValue and refused the sparse index
  (`w9_sparse_not_distinct_cast_value_explain`). `compileTimeComparand` now
  evaluates a row-free, binding-free comparand, as Java's
  `CompilableRange.compile` does.
  F-7b (design 4.3), the IN-list plans. Item 2 is done.
  - An index plan's rich ordering marks the primary-key keys it reaches only past
    the record-type coordinate (`WithPastRecordTypeHorizon`).
  - Only the in-union rule reads the mark. It builds no merge for a request
    naming a marked key, but a marked key may still be a free suffix.
  - Sort elision is unchanged, and so is the `col1 = ? ORDER BY id` index read
    (DIVERGENCES.md "an index read past the record-type coordinate").
  - Ten corpus plans moved off `InUnion`, nine to `InMemorySort(InJoin)` and
    one to a sorted range scan: every IN ordered by the primary key over an
    index without `id`. The simfdb golden rows are unchanged. Four FDB tests in
    `sqldriver_test` now name `id` in the index key to keep their merge; they
    are full lane and have not run.
  - The oracle's `w8_in_union`, `w8_in25_order_by_id` and `w8_tie_in_order_by_id`
    explain rows have new Go pins and move to F-7c, whose cost model decides
    between Go's sorted InJoin and Java's `SCAN | FILTER`. The sorted InJoin
    buffers the whole result where the in-union streamed, so F-7c and the
    stress re-measure must check large results (the stress IN-list query
    returns 46 rows).
  Item 5, the size check, is done (see WS-E "IN semantics"). The record-layer
  oracle row `w8_rl_default_in2_order_by_pk` now fails at size 0 as Java's
  does, but it stays open on item 3's fetch placement. The SQL in-union
  benchmarks stop at N=24. Items 4 and 6 are done. The unordered in-union
  arm is deleted (one corpus plan, an unordered IN, became Java's in-join).
  The size-less constructors are gone: `NewRecordQueryInUnionPlan` takes the
  size, and the rule fails a call without a planner context.
  Item 3 is done.
  - The rule memoizes each ordering partition whole
    (`MemoizeFinalExpressionsFromOther`, which carries the inner reference's
    requested orderings). The single-member pin is gone.
  - Extraction checks every in-union's child against its comparison keys
    (`InUnionChildOrderingError` as a planner invariant violation;
    `TestCheckInUnionChildOrdering`).
  - The set-operation push through a fetch translated comparison keys over a
    fresh placeholder alias, so a keyed set operation never pushed. It now
    uses the keys' own `_current` root, as Java rebases them.
  - Result: `w8_explicit_id_in_order_by_id_explain` is Java's
    `Fetch(InUnion(COVERING))` and leaves `wsfOpenUntil`. With the in-join
    disabled, `ORDER BY col1` is the same shape
    (`TestInUnion_MemoizesThePartitionWhole`). No corpus plan moved.
  - The record-layer `w8_rl_default_in2_order_by_pk` is now
    `Fetch(InUnion(COVERING))` too, and it leaves `wsfOpenUntil`. The oracle's
    Go index definition now states the primary key's entry positions, as
    embedded's metadataIndexDef does; without them the covering record
    omitted `order_id`. Predicted from the planner alone; the full lane has
    not run.
  Item 9's matcher is already Java's: `InComparisonToExplodeRule.onSelect`
  rewrites a select holding the IN, and the `w8_rl` in-joins plan. The
  one-value row `w8_rl_default_in1_order_by_price` is Go's single-element
  collapse, declared `DIFF-PATH single-element-in` (WS-E section 4).
  Still open: item 10 (the two-source in-union, WS-E section 4) and the DESC
  tie row's cause (needs the W6 step 1 observer).
  Item 10 measured (2026-10-06), both variants reverted. Go's WHERE carrier
  (the filter arm of InComparisonToExplodeRule) explodes one IN per firing,
  so two lists nest two one-source in-unions; ImplementInUnionRule itself
  already takes every explode of a select. (1) Exploding every IN into one
  predicate-free select builds Java's two-source in-union (`InUnion(...,
  bindings=2)` with the in-join disabled) and moves no golden plan, but loses
  Go's in-join over one list with the other as a residual: `a IN (7, 8) AND
  d IN (9, 3)` falls from `InJoin(PredicatesFilter(IndexScan(IDX_A, [=])))`
  to a full IDX_A read with both INs residual (a factory scenario drifted).
  The split rule skips a predicate-free select, and PartitionSelectRule's
  partitions are not the in-join rule's shape. (2) Java's form, the
  equalities in the select itself, plans `InJoin(FlatMap(IndexScan,
  Map(Filter(Explode))))` at five times the tasks. Java plans that query as
  nested FLATMAPs over both explodes with an intersection of the two index
  probes. The nested in-unions are reachable only with the in-join disabled
  (the 5x5 and 4x6 rows are DIFF-PATH rfc-191), so the port waits on Go's
  in-join rules accepting a select with several explodes.
  F-7c LANDED (2026-10-06). The Go-only pruning in `abstract_data_access_rule.go`
  (a full index scan with no search argument and no requested ordering was
  dropped) is deleted: a PRESERVE request is satisfied by every scan, as in
  Java, and PREFER_INDEX ranks the full index scan against the primary scan.
  - 64 corpus plans move, none regressed. Classified against Java's plan for
    the same SQL (scratch oracle over every moved query): every query Java
    plans with a data access is a full index read in Java (`ISCAN`/`COVERING
    <,>`), none `SCAN`, and Go now reads the same index (26 rows); 33 rows Java
    cannot run (DATE, LIMIT, UnableToPlan); 5 are pre-existing sargability gaps
    (NaN IN probes, the DESC index) that move to the same index as Java. The
    WS-F rows `w8_covering_all`, `_neq`, `_id_neq`, `w8_prefer_index_neq`,
    `w9_distinct` and `w10_enum_distinct` reach SAME-PATH and leave
    `wsfOpenUntil`; the WS-E v8 type-annulment EXPLAINs reach Java's `COVERING(T_N
    <,>) | FILTER false`; Java's corpus file `versions-tests.yamsql` now passes
    (javacorpus 130 pass).
  - With it: the PLANNING residual rung is Java's NormalizedResidualPredicate-
    Property (an ordered union on values ORs its legs' residuals, tautologies
    dropped), so `(a=1 OR b=2) AND (c=10 OR c=20)` plans Java's union of the two
    index probes (`TestPlanHarness_UnionWithFixedUnindexedFactor` re-pinned to
    Java's measured plan; the old pin's "one access over two" claim was wrong).
  - Two latent defects it reached, fixed: PartitionSelectRule's positional
    merge typed a de-null-on-emptied leg's slot NOT NULL under a group typed
    nullable (memo admission error 64, planning failed), and the executor
    refused a NOT NULL row under the leg's nullable carrier. The slot keeps the
    stated nullable row and the layout attach admits exactly that widening
    (`TestLeftJoinLegUnderInPlans`,
    `TestAttachOrdinalLayout_NotNullRowTakesNullableCarrier`). The factory full
    corpus (8147 scenarios) passes: it failed 21 at the parent commit.
  - Plan pins re-pinned to the new (Java) shapes; the DISTINCT-elision
    (R2/R3) and proof-stamp FDB tests now pin a primary-key range so their
    base-record-scan premise holds (bare, the unique index is read whole in
    key order and dedups streaming). `TestPlanHarness_FixedFactorUnionJavaComparable`
    unordered now stops at the task cap: Java does not plan that statement
    either (StackOverflowError, `FixedFactorUnionScalarJava`).
  - Still open: Java skips a match that satisfies none of the requested
    orderings; Go does not (alone it moves 336 plans, a join's inner probe
    among them: Go's request sets differ from Java's). Re-measured after the
    leaf climb (2026-10-06), reverted: under ORDER BY a join leg is asked for
    a concrete ordering no probe provides (Java cannot plan those queries),
    and the skip drops the leg's probes, so yamsql join scenarios degrade to
    scans. It needs Go's in-memory sort to request PRESERVE below it too. Rows still open in
    `wsfOpenUntil` as F-7c follow-ups: none. The IN-join versus filtered scan
    rows (`w8_in25`, `w8_in_union`, `w8_tie_in`) are declared `DIFF-PATH
    in-memory-sort` (DIVERGENCES.md "an IN ordered by a key no probe
    provides"): Java's only plan for them is the ordered scan, because it has
    no in-memory sort. The 1M stress comparison the design requires with
    F-7c ran on 2026-10-06 (section 2's last item): equal rows, no regression.
  - Follow-ups closed or re-diagnosed (2026-10-06):
    - `w8_no_predicate` was no rank question: a bare `SELECT * FROM T1`
      reached the planner as `Sort(Scan)`, with no Select for the data-access
      rules to match, so no index read was ever built. `topLevelSort` now
      states it as Java's generateSelect does, a block Select returning the
      read's row, and Go reads `IndexScan(I1, [*])`, Java's `ISCAN(I1 <,>)`.
      No corpus plan moved; the row leaves `wsfOpenUntil`.
    - `w6_left_join_indexed` DONE (2026-10-06), and the diagnosis above was
      wrong: measured on the JVM (`planRuleTrace`), Java's preserved leg is a
      bare LogicalTypeFilter with no Select, and WithPrimaryKeyDataAccessRule
      gives it `ISCAN(I1 <,>)` because the leaf match climbs through the
      candidate's Select to its MatchableSort (`SelectExpression.adjustMatch`).
      Go's AdjustMatchRule refused the climb (it required zero node-local
      correlations); it now runs Java's subset check (node-local correlations
      minus own quantifiers and placeholder parameters within the child's).
      The row is SAME-PATH: `FlatMap(outer=IndexScan(I1, [*]),
      inner=DefaultOnEmpty(IndexScan(I6, [=])))`
      (`TestAdjustMatches_LeafMatchClimbsToTheCandidateRoot`). Bare join legs
      and aggregation inputs now read the full index scan Java reads (oracle-
      checked: MAX/SUM/COUNT and GROUP BY stream off `ISCAN(I <,>)`, DELETE
      reads `ISCAN | DELETE`); a GROUP BY over an indexed column streams with
      no sort. With it, an existential FlatMap plans one outer per ordering
      requested of the outer group (Java's per-ordering roll-up), else
      `EXISTS ... ORDER BY id` sorted a cheaper full index scan instead of
      using the PK-ordered outer. javacorpus `array-agg-documentation-
      queries.yamsql` now runs its scan-choice row (class retired); 3125
      factory headers re-blessed, 6 duplicate points retired (RETIREMENT_
      LEDGER.md; census: 0 equality-probe losses). Left: `secondary_index_
      pushdown.yaml#69` (`v NOT BETWEEN .. ORDER BY id`) lost COVERING on its
      union legs, a fetch per entry where the legs were covering; Java plans
      the ordered SCAN | FILTER there (in-memory-sort divergence). Follow-up:
      with the climb working, the Go-only OrderedIndexScanRule /
      OrderedPrimaryScanRule can be measured for retirement.
    - `w8_or_two_indexes` is done: Go built the ordered union only over two
      fetching index scans, because the merge-distinct identity proof refused
      a `Fetch(COVERING)` leg on the stale premise that Go's Fetch passes its
      child through (it loads the stored record by primary key since
      RFC-220). A covering scan now proves its index's identity under a
      primary-key Fetch (through row-selecting operators only), so the merge
      pushes below the fetch: `Fetch(MergeSortUnion(COVERING, COVERING))`,
      Java's `COVERING ∪ COVERING COMPARE BY (_.ID) | FETCH`
      (`TestMergeDistinctStoredRecordIdentity_CoveringOnlyUnderAFetch`). Five
      corpus plans move to covering merges that fetch nothing
      (`index_range_*`, `null_safe_equality_scan#15/16`), two factory
      scenarios filter on index entries before fetching; the OR-union FDB
      tests accept the merge as the primary-key dedup below the fetch.
    - The depth rungs now read an absent operator as Java's
      `ExpressionDepthProperty` does (Integer.MAX_VALUE, deepest): Go skipped
      the rung whenever one side lacked the operator. Only the distinct rung
      can differ in practice. One factory scenario moved (3 projections, re-
      blessed), to the plan Java measures for it: `(NOT e >= 4.0 AND s IS
      NOT NULL) OR b IN (5, 5)` merges two IDX_S reads by (S, ID) instead of
      deduplicating an IDX_S range and an IDX_B probe by primary key
      (`TestPlanningCostModel_DistinctDepthRanksAbsentAsDeepest`, mutation-
      checked). ImplementDistinctUnionRule's legs now range over every
      member that can feed the merge, spines pinned, not the cheapest one
      (F-8; no corpus plan moved;
      `TestImplementDistinctUnionRule_LegKeepsEveryMergeableMember`).
  - Measured after F-7c (2026-10-06), both reverted: deleting the
    single-element IN collapse (WS-E step 5) moves 13 plans, and the ORDER BY
    rows still go from a streaming probe to `InMemorySort(InJoin(...))`;
    ranking the in-memory-sort count FIRST (emulating Java, which has no
    in-memory sort and so plans an order-providing plan wherever one exists)
    flips 93 plans, several clearly worse (`cte.yaml#20` runs a streaming
    aggregate per row of a full scan; `comma_join_exists.yaml#2` reverses the
    driving table). The IN-join-versus-filtered-scan follow-up needs a narrower
    rule than "sort-free first": Java's plans for these rows are order-
    providing scans because Java cannot sort, not because its cost model
    prefers them.
  History, from the measurement before it landed:
  The fast lane with the pruning off failed 22 tests and exposed two defects
  the pruning had masked, both of which blocked F-7c:
  - Wrong rows, FIXED. A unique index filtered by a Go closure (opaque
    filter) was served as a full index scan, as if it held every record:
    `TestClosureSparseUniqueIndex_IsNeverAnEliminationProof` planned
    `Distinct(Map(IndexScan(CS_EMAIL, [*] COVERING)))`. The defect was live
    without F-7c: an ORDER BY, a range or an equality probe read the index.
    The candidate now refuses to produce any scan when it has an opaque
    filter (`canProduceScanPlan`). Pin:
    `TestClosureSparseIndex_ServesNoQuery`, with a full-index control.
  - The task budget. `TestPlanHarness_FixedFactorUnionJavaComparable/#00`
    (ORDER BY b, id) hits MaxTasks (150,000 tasks, 8.3 s). Its distinct-union
    group grows to 2998 members, 1980 of them MergeSortUnion plans. This is
    the OR-union budget the pruning comment named. Measured cause:
    `ImplementDistinctUnionRule` fires once per union alternative, of which the
    DNF expansion holds about 500. It fires under PRESERVE over legs of 3 to 11
    disjuncts with 19 to 67 ordering partitions in all, and every firing
    reaches 4 merge states. With the pruning on, a leg whose disjunct binds no
    index has only the primary scan, and fewer states result. Each state yields
    a pinned MergeSortUnion, so about 500 x 4 merges reach the group. Java has
    no task cap here. A fix has to bound the merges built per union group, not
    per alternative.
  - `TestPlanHarness_UnionWithFixedUnindexedFactor` is a plan pin: the
    two-residual tie now breaks toward a full covering scan, not the primary
    scan.
  The rest are plan pins encoding the pruning: like-prefix and unindexed-IN
  full scans, ORDER BY elimination, the rfc202 generated index plans,
  order_by_nulls, the self-join probe, and the javacorpus run.
- [x] Reconcile F-6/F-7b with RFC-191's existing `Fetch(InJoin)` ruling; see
  `DIVERGENCES.md` “Plan choice: an ordered IN over a non-covering index”.
  Go's covering emission is already Java's gate: `ToScanPlan` is
  `toEquivalentPlan`. The `w8_in_*` rows differ only by RFC-191's push of a
  comparand in-join through its fetch, so they are declared `DIFF-PATH rfc-191`.
  The 25-value and 5 x 5 rows are declared `DIFF`, because Java's IN-union
  fails and Go's in-join answers. They leave `wsfOpenUntil` (full lane, not
  run).
- [x] RANK-index match-candidate gap: verified, no reach. Java plans a RANK
  index only for a record-layer `RecordQuery` with a rank comparison
  (`QueryRecordFunctionWithComparison` builds the `RankValue` that
  `WindowedIndexExpansionVisitor`'s candidate matches); fdb-relational SQL
  can neither declare a RANK index nor build a `RankValue`, and Go has no
  declarative RecordQuery API (its record layer exposes BY_RANK scans and the
  `rank` record function directly). The candidate is ported with a RecordQuery
  API, if one is ever ported: it then also needs Java's windowed expansion
  (Go's expands the columns as a value index) and a BY_RANK scan type on
  `RecordQueryIndexPlan`, which today always scans BY_VALUE. Both dotted
  identifier gaps of `embedded/dotted_identifier_gap_test.go` are closed: the
  primary-key ORDER BY (the escaped-column item below) and the aliased GROUP BY
  (`t."foo.tableA.A2"` was 42703: the GROUP BY check peeled the alias and asked
  only whether the root segment is a field; `dotted_column_group_by.yaml`).
- [x] Escaped COLUMN names (`"c$1"`, stored `c__1`) plan as their unescaped
  twins. Key expressions carry stored names and the planner's row layouts
  decoded ones (`FieldNameForProtoField`), so names are decoded once where
  metadata enters the planner, as Java's KeyExpressionExpansionVisitor and
  ScalarTranslationVisitor read `toUserIdentifier`: `embedded.layoutNames`
  (index columns, value columns, vector and aggregate names and paths),
  `coveredPrimaryKeyColumns`, `TranslatePrimaryKeyToValues` (was
  `strings.ToUpper`), and in cascades `resolveKeyFieldPath`/`layoutFieldPath`
  and the key-expression name comparisons. `storedFieldName` re-encodes where a
  root is spelled back from names. ON-source DDL resolves the SQL name against
  the decoded layout. Thirteen shapes measured against their twins (index and
  PK ordering, IN, DISTINCT, SUM index, intersection, ON-source) now match;
  `escaped_column_index.yaml` pins them with rows.
- [x] Close the large-join memo planning-cost regression introduced by
  `54fcf78f0`. Preserve Java's block-Select architecture. Investigate a cheap
  negative filter or fewer sibling alternatives using Java's PartitionSelectRule
  and Reference.insert. Cross-batch reference-comparison caching was rejected for
  excessive live heap, not left as a recommended fix. Exact historical measurements
  and SHAs are in the archive's RFC-257 completion ledger.
  Closed as Java parity (2026-10-06). Java avoids neither cost: its
  Reference.insert scans every member with `isMemoizedExpression`
  (Reference.java:996-1018, deep `findMatches` + `containsAllInMemo`), and
  Go's relational switches (right-deep, deferred cross products) are Java's.
  Measured with the target's `planRuleTraceOutcome` TASK-COUNT (a scratch
  conformance probe, not kept) against Go's `plan-trace`, `SELECT t1.id FROM
  t1..tn WHERE ti.next_id = ti+1.id`:
  4 tables Java 6731 / Go 3652 tasks; 5 tables 26722 / 20092; 6 tables
  122839 tasks in 77.7 s / 134894 in 23.5 s; a 6-way self-join `o_i.id =
  o_i+1.id` Java 118219 in 54 s; at 7 tables both shapes take Java over two
  minutes. Go's 7-way self-join stops at its 150k cap in ~45 s
  (`TestPlannerCapHit_ProductionSelectPathSQLSTATE`). Two filters were
  measured on it and reverted, neither moving the time: Java's same-reference
  shortcut restricted to the group's free aliases, and pruning the quantifier
  bijection by the aliases' result-value/predicate components (3.76M
  node-equal pairs, 3.73M negative; the cost is the pair count, ~10 us each,
  ~40% GC).
- [x] Re-measure planner/executor stress against the actual merge-base with
  explicit SHAs and equal row populations; resolve regressions, not just timeouts.
  2026-10-06, `c116de63b` against the recorded merge-base `e48f5b49` (2 samples
  each, the baseline's command: `TestFDB_Stress_1M$`, `--nocache_test_results`).
  Both samples pass (167.5 s, 167.9 s); every one of the 22 queries returns the
  merge-base's row count; median ratio cur/base 0.25-1.29, the largest
  increases 2-3 ms absolute on single-digit-ms lookups (needle, ORDER BY PK +
  index filter), within run noise. Faster: GROUP BY customer HAVING 641 -> 209
  ms, ORDER BY PK (full) 5.8 -> 4.3 s, full scan filter 831 -> 590 ms. Load
  average 41.9 falling to 3.6 across the runs (the baseline's was 5.6-5.8).

## 3. WS-D — vector engines and maintenance

- [ ] GuardiANN safety: zero-candidate admission; n<k/peel/unsplittable split
  fallbacks; empty-core repair; primary-preferred cleanup; underreplication
  deltas; committed negative-count disable. Checked decoding, task poisoning,
  KMeans preconditions and merge/drain target checks have prior fixes.
  Committed negative-count disable is done: the disable is Java's keyed commit
  check, run after the merger's heartbeat refresh, so it commits (it used to
  roll back with the refresh's "Unexpected index state(s)"), and counts
  `vector_index_disabled_on_negative_task_count`
  (`guardiann_negative_count_test.go`). Zero-candidate admission is done:
  `insertMaxCandidateClusters < 1` refuses the insert (engine and queue
  enqueue) with a poisoning `VectorCapabilityError`, declared (b)
  (`guardiann_insert_admission_test.go`). Knob consumers are done: the
  declared (c) livelocks (split/merge/reassign neighbour fetch of width or
  pipeline below 1) raise `VectorCapabilityError` where Java writes the
  empty-list task back; split-merge/reassign/collapse/bounce/delete
  concurrency below 1 fails with forEach's IllegalArgumentException at Java's
  statement (`guardiann_knob_consumers_test.go`). The KMeans knobs had prior
  fixes. Split fallbacks and empty-core repair are done, declared (h)
  (DIVERGENCES "GuardiANN splits what Java cannot",
  `guardiann_unsplittable_test.go`, each fixture mutation-checked). They cover
  the n<k INVALID rule, the admitted outlier peel with its geometric floor and
  admission bound, the terminal reconcile with `ClusterUnsplittableError`
  above the hard cap, the zero-primary new-child drop, and the empty merge
  core. Not done: the design's performance-criterion timing runs (peel at
  W = B under suite load) and the d = 768/2048 acceptance fixtures.
  Primary-preferred cleanup and underreplication deltas are done, declared (h)
  (DIVERGENCES "GuardiANN keeps primaries and underreplication counts exact",
  `guardiann_counts_test.go`, mutation-checked). Declared (d) is done:
  `consumerOutcome` (CONSUMED / RUNS / REFUSED / REFUSED_UNLESS_ALL_NK) over
  each kind's prologue, and an inline delete skips a refused head task
  (DIVERGENCES "GuardiANN inline deletes skip a head task Go would refuse",
  `guardiann_consumer_outcome_test.go`, mutation-checked). Not ported from
  the design's (d) fixture list: the JVM-row byte comparisons (bounce
  follow-up ids, bits-9 quantizer refusal, which Go does not raise) and the
  1020 race fixtures.
- [ ] HNSW/engine: general fetch/cardinality/layer scans, ordered retrieval,
  covering/rank results, search-free continuation replay, operation-local caches,
  partition locks, cosine zero/clamp, sample-UUID closure, option catalog/identity.
  efSearch defaults and bounded-beam use already have fixes. Cosine done
  (2026-10-06): the HNSW/GuardiANN cosine is Java's CosineMetric (zero vector
  +Inf, non-finite NaN, no clamp) and HNSW orders distances by Double.compare
  (`hnsw_cosine_java_test.go`, mutation-checked); SPFresh keeps its own
  clamped cosine (`spfreshVectorDistance`), as the design requires.
  Operation-local caches done (2026-10-06): the maintainer no longer caches
  HNSW storage across operations; each insert/delete/search gets its own
  parsed-node cache, as Java's. The shared cache let a snapshot search's
  nodes serve a later insert in the same transaction, which then committed
  without the read conflicts it owed
  (`hnsw_operation_local_cache_fdb_test.go`, mutation-checked). Partition
  locks done: the executor's scan read-locked nothing and SearchKNN the whole
  index subspace, so neither excluded a partition writer; both now read-lock
  the partition subspace for the search (`vector_partition_lock_fdb_test.go`,
  each path mutation-checked; hold span declared in DIVERGENCES). The option
  catalog (aliases, alias conflicts, Java parsing) was already ported
  (`hnsw_options.go`); a plan carries only Java's two SQL vector options
  (RowNumberValue.SUPPORTED_OPTIONS: ef_search, return_vectors), both in the
  continuation salt.
- [ ] Distinguishing pins for codecs, evaluator, collapse, bounce, reassignment,
  task counts and merge locks.
- [ ] Runner: unified bounded attempts, per-owner retries, commit ownership and
  deactivation, client proxy wait/body-chain causes, SPFresh stall bound and
  instrumentation. Apply the SPFresh paper review to affected algorithms.

## 4. WS-H — stored-query runtime

- [x] CallSiteArguments, typed options and row-number encapsulation (`781468430`);
  macro catalog bytes/named calls and metadata getters have existing Java pins.
- [x] Engine-wide plan cache keyed by schema template, matching Java's
  RelationalPlanCache; stored-query startup warming, invalidation, timing and
  counters. The existing per-connection cache does not satisfy this obligation.
  Done: `RelationalPlanCache`, one per driver connector, shared by every
  connection, with Java's three stages (template / query / equivalence),
  sizes, TTLs (primary expire-after-access, the others after-write) and the
  PLAN_CACHE_* hit/miss/LRU-eviction counts (`relational_plan_cache_test.go`,
  `TestFDB_PlanCacheIsEngineWide`). Declared (DIVERGENCES "Engine-wide plan
  cache"): the query key keeps database and schema and Go's planned-in
  literals, so plans are shared per schema, not per template. Also done:
  UPDATE/DELETE plans are cached (INSERT never, Java's shouldNotCache); the
  transaction's temporary functions are part of the key, so their DDL no
  longer invalidates (`TestFDB_PlanCacheKeysTemporaryFunctions`); a session
  reset keeps the shared cache; PLAN_CACHE_* sizes/TTLs are DSN parameters;
  the corpus `check_cache` pass runs (Java's EmbeddedConfig one-hour TTLs,
  checks shuffled after the executions, +1 tertiary hit required; the
  check-cache skip class is emptied). Per-template keys and the stored-query
  warm-up DONE (2026-10-06): the query key drops database and schema (kept
  only for a PLANNER_STATISTICS plan), so a template's schemas share plans
  as in Java; `WarmStoredQueries` ports OfflineStoredQueriesProcessor (catalog
  read, offline planning with every index readable, DECLAREd functions
  declared first, failures logged and counted) and runs at the connector's
  start (`TestFDB_StoredQueriesWarmThePlanCache`: a stored query's first
  execution hits, an unstored one misses). Cache timing: Go reports a
  plan's cache outcome and planning duration per statement
  (PlanGenerationInfo.Cache / PlanningDuration); Java's per-phase
  RelationalEvent timers (LEX_PARSE, CACHE_LOOKUP, OPTIMIZE_PLAN, ...) have
  no Go registry to land in, declared with the warm-up's counts (DIVERGENCES
  "Engine-wide plan cache").

## 5. WS-I — shared APIs and lifecycle

- [x] Lock-registry cleanup; serializer retry diagnostics; typed client knobs
  (read C++ 7.3.77); typed session/index-update sets and write-only key collisions.
  - Lock registry (#4545): an entry counts its holders and waiters and is
    removed with the last (`TestLockRegistryRemovesCompletedLocks`).
  - Serializer retries (#4290): the read retries were ported; a failure now
    carries Java's RETRY_COUNT / RESULT (and META_DATA_VERSION on a refused
    reattempt) as `RecordSerializationError` fields.
  - Client knobs (#4488): `FDBClientKnob` (Java's 19, typed), `SetKnob` /
    `SetKnobByName` / `Knobs` / `ClearKnobs` with Java's validation (decode
    without '#', `Double.parseDouble`'s grammar, boolean or int). They are set
    before the factory's first open, through `fdbclient.SetKnob`: the `knob`
    network option on libfdb_c, a refused open on the pure-Go client
    (DIVERGENCES "Client knobs").
  - Session sets (#4289): `ContextSessionKey`, `GetInSession` /
    `PutInSession`, and the readable / write-only / queued index-update sets
    filled per index update. Java's two write-only keys share the name
    "writeOnlyIndexesUpdated" and so one set; Go keeps that.
- [x] Client range/HNSW/GuardiANN/vector-task/queue timer instrumentation;
  online-indexer configuration limits; ICU byte baseline.
  - Timers: HNSW node reads/writes by layer and bytes with the generic
    index key/value counters (HnswVectorIndexEngine OnRead/OnWrite); GuardiANN
    `vector_vector_reads`, task enqueued/executed, disabled-on-negative-count;
    `wait_delete_store` around DROP SCHEMA's deletion. The pending-queue
    counters were already in. Pinned by `vector_counters_test.go`.
  - Client range instrumentation: Java's 4.14 change is EventKeeperTranslator's
    count/event dispatch over fdb-java's EventKeeper; neither Go client has an
    EventKeeper, so there is nothing to translate (pre-existing, no analogue).
  - Online-indexer limits: initial limit, increaseLimitAfter (Java's default
    -1 re-increases after every success, as its code does, not as its doc
    says), the 900,000-byte write limit and 4,000 ms transaction time limit,
    which end a range early (`hadTransactionReachedLimits`). Pinned by the
    throttle tests and `online_indexer_txn_limits_test.go`.
  - ICU: declared in DIVERGENCES ("Collation keys are not ICU's"); no byte
    baseline, since matching ICU 78.3 sort keys means porting ICU collation.
- [x] Lucene backend: out of scope for this migration PR (#786), owner decision;
  tracked as a separate follow-up PR (section 7).

## 6. WS-K — harness and relational entry points

- [x] Direct-API Struct inserts: UUID scalar/nested/array and nested unique index.
  Go's `api.DirectAccessStatement` had no implementation; it is now ported
  (`embedded/direct_access.go`: insert, get, scan, delete, delete-range over
  Java's KeyBuilder and `toDynamicMessage` with the #4243 UUID case;
  `rowstruct.StructBuilder`/`ArrayBuilder`). Pinned by
  `TestFDB_DirectAccessNestedUUIDUniqueIndex` (Java's
  `insertToArrayNestedUuidFieldMarkedUnique`) and the round-trip test.
  Limitations: the connection's own schema only, no INDEX_HINT, scans are
  materialized and do not resume from a continuation. A bare UUID array is
  written (Java's repeated-field path cannot write it; see DIVERGENCES, Go-only
  extensions).
- [x] Setup-block version gating (`e510c3d2a`: a setup block's
  `supported_version` is checked before its connection options, as
  `SetupBlock.java:95`). Typed INDEX_FETCH_METHOD: `api.OptionFromString` ports
  `Options.parseStringOption` (enum valueOf, comma-split collections,
  `Boolean.parseBoolean`), and the corpus runner routes a string option through
  it, as Java's `TestBlockOptions.parseConnectionOptions`.
- [x] Recursive result metadata in the corpus runner (CQ-74 closed). A result
  column carries its planned type (`executor.ColumnDef.DataType`, from
  `rowstruct.DataTypeOf`); `api.WithResultSetMetaDataObserver` hands a query's
  metadata to the caller, and the runner ports `extractDescriptors` over it.
  An array column's `DatabaseTypeName` is Java's `ARRAY` (was the element
  name). `unsupported:result-metadata-nested` emptied (190 inner, 6 file
  skips): the check-result-metadata positives pass and the six struct/array
  negatives fail on their metadata mismatch.
- [ ] JSON descriptor FieldOptions import (#4540). Harness-only in Java
  (`CommandUtil.loadRecordMetaDataFromJson` for the yaml `load schema
  template` command); Go has no JSON metadata import and its runner books
  every `load schema template` / `set schema state` file as
  `unsupported:schema-command` (9 files). Porting it needs that command pair
  in the runner, the yaml-tests protos as Go descriptors (Bazel
  `go_proto_library`), and relational table generations
  (`RecordLayerTable.addGeneration`, the check #4540's file exercises), which
  Go's schema template does not model.
- [x] Relational queued-state plumbing; SQL vector-option and preference-cache pins.
  Relational planning reads only READABLE indexes and the fleet build treats
  WRITE_ONLY_WITH_QUEUE as write-only; `TestFDB_QueuedVectorIndexIsWriteOnlyToSQL`
  pins a format-15 store with a queued GuardiANN index through SQL (format and
  state kept, INSERT queued, the index-only KNN unplannable with 0AF00). Java's
  only other relational reach is `RecordLayerSetStoreStateConstantAction`, the
  yaml `set schema state` command (see the JSON item). Every
  `SUPPORTED_VECTOR_OPTIONS` entry is pinned to its canonical option key and
  serialized value (`TestVectorIndexSQLOptionsStoreJavasOptions`), and each
  engine preference keys its own plans (`TestPlannerOptions_CacheKeyPart`).

## 7. Migration-wide acceptance and decisions

- [ ] Reconcile implemented WS-A/B/C/J against the historical designs and current
  regression tests. The archive's completion ledger did not list new obligations
  for them; that is not a fresh completeness proof. Include recursive promotion,
  RaBitQ/HNSW legacy compatibility, storage lifecycle, pending writes/format 15,
  catalog version/rebind/carry rules and index-definition fidelity.
- [x] WS-G implementation: Java aggregate continuation state, legacy reads,
  grouping-output simplification/ARRAY_AGG cap/resume and plan-schema tags have
  committed pins. Whole-upgrade acceptance remains open.
- [x] **SUM index: DECIDED 2026-10-05, exactly as in Java; done.** A SUM or
  COUNT aggregate index answers alone: a vacated group reads 0, an all-NULL
  group has no SUM / COUNT(col) row, a SUM residue reads 0 (Java 4.12.11.0
  measured `[[10 0] [11 0] [13 7]]`, `conformance/probe_zerokey_allnull_java_test.go`;
  4.14.2.0's `AggregateIndexMatchCandidate` and maintainer have no change here,
  checked by source diff). RFC-209 is withdrawn: the `__GROUP_COUNT` companions,
  the `GroupExistenceMerge` outer merge, the COUNT(*) zero drop, the
  `NeedsNonNullCompanion` gate (5459c90a2) and the reserved-name guard are
  deleted, and an ungrouped SUM / COUNT index is a candidate. The tests pin both
  answers through `mmAggregateIndexRowsAgree` / `WantAggregateIndex`
  (sqldriver), which admits exactly Java's three differences from the records.
  The 43 WSJ `both-accept-companion` runs are now `both-accept-equal` (their Go
  digests equal Java's, recomputed offline). The metamorphic sweeps no longer
  declare SUM / COUNT aggregate indexes.
- [x] Full-lane failures found running the sqldriver and factory suites locally
  (2026-10-05). `TestFDB_InListSignedZeroKeepsPrimaryKeyOrder`'s twins index
  `(e, id)` (F-7b item 2 builds no IN-union past the record-type coordinate);
  `TestFDB_DynamicNaNCompositeIndexKeyFilter` reads the id after the record-type
  key. The factory corpus drift (53 scenarios) was censused against `d5a5132a1`
  before re-blessing: the 12 lost equality probes are 6 IN-unions F-7b item 2
  removes (Java builds none) and 6 join outers F-7a moved from `IDX_AB [=,*]` to
  a fully matched range index, Java's `unmatchedFieldsCount` rung.
- [x] **Aggregate roll-up port.** `yieldAggregateGroupingSubsumption`
  (`rule_aggregate_roll_up.go`) ports the rest of Java's
  `GroupByExpression.groupingSubsumedBy`: query grouping keys match the
  candidate's columns as a set, an equality on a grouping column matches it
  implicitly, an exact match is the index scan with a projecting Map, and
  otherwise the candidate rolls up to its longest matched prefix (explicit keys
  inside it, a rollable index type, no residual) as a streaming SUM / MAX / MIN
  over the scan. `aggregate_index_roll_up.yaml` is Java's
  aggregate-empty-table.yamsql T2 blocks: every plan shape and answer, the
  commented-out residue reads included. Not ported: roll-up inside the
  multi-aggregate intersection.
- [x] **Lucene: DE-SCOPED from PR #786 by the owner; a separate follow-up PR.**
  It is still to be done (the 2026-10-05 in-scope decision stands for the
  port), but it does not gate this migration's completion or parity claim;
  the Lucene queue/heartbeat/quota/spell-check/state contracts go with it.
  The notes below are the follow-up's starting point. Java runs Apache
  Lucene 8.11.1 inside the JVM (`fdb-record-layer-lucene`, 28.7k lines of main
  Java, 7 protos). It stores the segment files in FDB through `FDBDirectory`,
  with `LuceneOptimizedCodec` over Lucene87 parts (Lucene84 postings, Lucene80
  doc values, Lucene86 points and segment info, Lucene50 compound). Java's
  relational layer does not reach it. The engine is not chosen yet:
  - Zinc (now `vcaesar/riot` + `ice` segments), bluge and bleve write their
    own segment formats. Java could not read a Go-written Lucene index, and Go
    could not maintain a Java-written one, which breaks the wire hard line.
  - `geange/lucene-go` (Apache-2.0, Lucene 8.11.2 base, experimental) has
    ports of the same Lucene87 codec parts plus BlockTree and FST. It is a
    candidate reference or vendored start for a format-compatible port.
  The engine choice is made in the follow-up PR.
- [x] **Catalog/keyspace: DECIDED 2026-10-05, exactly Java's layout, no
  compatibility with the old Go layout.** Go's SQL driver uses the string-key
  catalog/schema layout `(__SYS, __SYS, CATALOG)` / `(dbPath, schemaName)`.
  Replace it with Java's `RelationalKeyspaceProvider` layout: the typed system
  path `(NULL, NULL, int64(0))` and directory-layer domain -> database ->
  schema levels. No migration from the Go layout: data written by an earlier Go
  build is recreated. A template created through Go must load in Java and the
  reverse (`TODO_OLD.md`, “Go SQL driver stores the relational catalog…”).
  Measured scope (4.14.2.0 source):
  - Catalog store at `KeySpaceDirectory(__SYS, NULL) / (__SYS, NULL) /
    (CATALOG, LONG, 0)`, so the tuple `(nil, nil, 0)`.
  - A database path is `/DOMAIN/DB`. A domain must be registered
    (`registerDomainIfNotExists`; Java's server registers `FRL`, its tests
    `TEST`); the domain is a `DirectoryLayerDirectory` resolved by the GLOBAL
    `ScopedDirectoryLayer` (FDB directory layer `createOrOpen([domain])`, the
    prefix's tuple long, plus an `FDBReverseDirectoryCache` entry). `dbName`
    and `schema` are `DirectoryLayerDirectory`s resolved by a
    `ScopedInterningLayer` at `domain/__internedStrings` ("IL");
    `defaultSchema` is a NULL directory. A one-segment path is INVALID_PATH.
  - Go has the FDB directory layer (`pkg/fdbgo/fdb/directory`) but no
    `ScopedDirectoryLayer`, `FDBReverseDirectoryCache`, `StringInterningLayer`,
    record-layer `HighContentionAllocator` or `ScopedInterningLayer`; its
    `keyspace.FDBResolver` is a Go-only format. These are byte-exact ports
    (about 2,800 lines of Java with `LocatableResolver`), then the keyspace tree,
    `sqldriver`, `catalog`, `ddl`, `fleet`, the `frl` CLI.
  - Domains: DONE (owner decision 2026-10-06, exactly Java). A process-wide
    registry (`keyspace.RegisterDomainIfNotExists`, re-exported by
    `sqldriver`), Java's path matcher (`keyspace.ToDatabasePath`, a port of
    KeySpaceUtils.toKeySpacePath over RelationalKeyspaceProvider's tree) at
    Java's sites (CREATE DATABASE, a schema's store resolution, connect), and
    SemanticAnalyzer.validateDatabaseUri for parsed paths. The driver
    registers nothing; `frl`, the factory/stress tools and the javacorpus
    runner register FRL; test packages register FRL and TEST. About 3,700 test,
    doc and help paths were renamed to `/FRL/...` by script.
  - Byte-exact layout: DONE (2026-10-06). The catalog store is `(NULL, NULL,
    0)`; a schema store is `(domain, database, schema)`, the domain resolved by
    the global FDB directory layer (ScopedDirectoryLayer.global, plus the
    `recdb_rd_cache` reverse directory cache) and the names interned by the
    domain's ScopedInterningLayer at `(domain, "IL")` (StringInterningLayer
    mapping/reverse/counter at 2/1/0, state at -10, the record-layer
    HighContentionAllocator). `pkg/recordlayer/locatable_resolver.go` ports
    the resolution path: resolveWithMetadata in a committed child transaction
    with the database's directory cache, readInTransaction (no create,
    used by the frl CLI so a mistyped address interns nothing),
    reverseLookup, the UNLOCKED state check. `pkg/fdbgo/fdb/directory` now
    runs over any `fdb.WritableTransaction` (libfdb_c and the simulator too)
    and takes an injected random source (the DST seam). Pinned by the Java
    interop spec "Relational key space shared with Java" (each engine resolves
    the other's schema at the same prefix and reads its rows; the interning
    mapping moved off 2 reddens it). Not ported: resolver locking/migration/
    setMapping/setWindow administration, the global root interning layer
    (0xFC), and the in-memory reverse-directory cache's retriable mismatch.
    `pkg/recordlayer/keyspace`'s Go-only `FDBResolver` has no users; remove it
    with that package's review.
- [ ] Reconcile living compatibility claims/CHANGELOG, run `just test-full` and
  required interop/performance checks, then final migration review and PR CI.
  Fix all Medium-or-higher findings before declaring completion.

## Test lanes and test-speed work (committed with the temporal change)

- [x] Fast/full lanes (`just test` / `just test-full`, `infra/test_lanes_test.go`,
  the fast pre-commit hook). CLAUDE.md restored (its deletion broke 126
  references) and aligned with AGENTS.md; stale "every `just test`" claims fixed
  in the workflows, DIVERGENCES.md and the dst skill. PR CI is unchanged: it
  runs `//... -stress`, so `test-full` targets still gate every PR.
- [x] Every test target now builds: `million_record_test` (never compiled
  standalone) gets the Ginkgo suite file and deps, and its env-gated Skip is
  removed; the FDB C++ genrules cap ninja by available memory (`-j$(nproc)`
  took 36 GB on a 24-thread host).
- [x] FDB readiness (`WaitAvailable`, `DatabaseAvailable`: "unavailable" no longer
  matches), rowdiff shared planning (held by reference, released by the last
  holder, pinned by `sweep_helper_test.go`), private parser ATNs per prediction
  lease (removes ATN lock sharing; no measured speed gain).
- [x] Run `just test-full` once end to end. 2026-10-06 at `a15d1ec70`, 111
  targets (manual ones included): 109 pass, 2 failed and were fixed in
  `c116de63b` (a typed NULL CASE branch lost its type in result metadata;
  stale CQ-74 oracle pins), both re-run green.
- [ ] Split `sqldriver_test` (1748 tests, ~7.5 min, 77% of its time in ~40
  sweep/probe tests) so its cheap regression pins return to the fast lane.
