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
  Go extension that needs an owner decision.
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
  - Still open: (c) ConstantFoldingRuleSet over the whole conjunction and
    `rejectsNull` as `foldPredicateAtNull`; (e) the
    NULL mapping; (f) `EffectiveConstant`; the deletion of the Go-only
    driver and the `EvaluateConstant` arms of `SimplifyValue`; (g) the
    REWRITING cost model rungs; the constant-evaluation census; and (j) the
    oracle rows as Go assertions.
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
  Still open: the simplification regime (design 5.4, gated on WS-F step 7).

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
  Still open: the design's D1/D2/D5 progress and staleness model, the
  `outerJoinCount` placement review, and F-8.
- [ ] Reconcile query-block acceptance with the current translator: top-level
  Sort(Select), ORDER BY resolution against projected Values, DISTINCT ordering,
  index-DDL root handling and ordered IN. Old blocker prose in `TODO_OLD.md`
  predates later fixes; do not reimplement the already-landed single-Select port.
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
  `w9_distinct_explain`. `w13_display_scan_explain` (a dotted escaped table gets
  no PK scan) needs RFC-238 §7c: storage names in the scan leaf and DML targets,
  a cascades matching change. The owner ACKed §7c on 2026-10-05, so it is
  ready to implement. Reassigned to that.
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
  collapse, which WS-E section 4 step (5) deletes, so it is reassigned there.
  Still open: item 10 (the two-source in-union, WS-E section 4) and the DESC
  tie row's cause (needs the W6 step 1 observer).
  F-7c, measured and not landed. The SQL configuration already plans with
  PREFER_INDEX and in-union size 24. What keeps Go from Java's predicate-free
  index reads is the Go-only pruning in `abstract_data_access_rule.go`: a full
  index scan with no search argument and no requested ordering is dropped.
  With the pruning off, 53 corpus plans move, most of them from a filtered
  primary scan to a covering full index scan. Four oracle rows reach Java's
  path: `w8_covering_all`, `w8_covering_neq`, `w8_covering_id_neq` and
  `w8_prefer_index_neq`. `SELECT * FROM T1` stays `Scan(T1)`, the declared
  `w8_no_predicate` class. The explain-differ corpus took 14 s instead of 6 s,
  which is the pruning's stated planning cost. The design orders F-6, the
  covering emission gate, before F-7c, and requires the 1M stress comparison
  with F-7c. F-6 needs no change: `ToScanPlan` is Java's `toEquivalentPlan`.
  The fast lane with the pruning off failed 22 tests and exposed two defects
  the pruning had masked, both of which block F-7c:
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
- [ ] RANK-index match-candidate gap and quoted dotted identifier GROUP BY/order
  gaps (`embedded/dotted_identifier_gap_test.go`); verify target reach first.
- [ ] Close the large-join memo planning-cost regression introduced by
  `54fcf78f0`. Preserve Java's block-Select architecture. Investigate a cheap
  negative filter or fewer sibling alternatives using Java's PartitionSelectRule
  and Reference.insert. Cross-batch reference-comparison caching was rejected for
  excessive live heap, not left as a recommended fix. Exact historical measurements
  and SHAs are in the archive's RFC-257 completion ledger.
- [ ] Re-measure planner/executor stress against the actual merge-base with
  explicit SHAs and equal row populations; resolve regressions, not just timeouts.

## 3. WS-D — vector engines and maintenance

- [ ] GuardiANN safety: zero-candidate admission; n<k/peel/unsplittable split
  fallbacks; empty-core repair; primary-preferred cleanup; underreplication
  deltas; committed negative-count disable. Checked decoding, task poisoning,
  KMeans preconditions and merge/drain target checks have prior fixes.
- [ ] HNSW/engine: general fetch/cardinality/layer scans, ordered retrieval,
  covering/rank results, search-free continuation replay, operation-local caches,
  partition locks, cosine zero/clamp, sample-UUID closure, option catalog/identity.
  efSearch defaults and bounded-beam use already have fixes.
- [ ] Distinguishing pins for codecs, evaluator, collapse, bounce, reassignment,
  task counts and merge locks.
- [ ] Runner: unified bounded attempts, per-owner retries, commit ownership and
  deactivation, client proxy wait/body-chain causes, SPFresh stall bound and
  instrumentation. Apply the SPFresh paper review to affected algorithms.

## 4. WS-H — stored-query runtime

- [x] CallSiteArguments, typed options and row-number encapsulation (`781468430`);
  macro catalog bytes/named calls and metadata getters have existing Java pins.
- [ ] Engine-wide plan cache keyed by schema template, matching Java's
  RelationalPlanCache; stored-query startup warming, invalidation, timing and
  counters. The existing per-connection cache does not satisfy this obligation.

## 5. WS-I — shared APIs and lifecycle

- [ ] Lock-registry cleanup; serializer retry diagnostics; typed client knobs
  (read C++ 7.3.77); typed session/index-update sets and write-only key collisions.
- [ ] Client range/HNSW/GuardiANN/vector-task/queue timer instrumentation;
  online-indexer configuration limits; ICU byte baseline.
- [ ] Resolve the Lucene backend scope decision below before claiming parity.

## 6. WS-K — harness and relational entry points

- [ ] Direct-API Struct inserts: UUID scalar/nested/array and nested unique index.
- [ ] JSON descriptor FieldOptions import; recursive result metadata in the
  corpus runner; setup version gating; typed INDEX_FETCH_METHOD.
- [ ] Relational queued-state plumbing; SQL vector-option and preference-cache pins.

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
- [ ] **Lucene: DECIDED 2026-10-05, in scope, in process.** Java runs Apache
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
  Deferred by the owner on 2026-10-05; the engine choice waits with it.
- [ ] **Catalog/keyspace: DECIDED 2026-10-05, exactly Java's layout, no
  compatibility with the old Go layout.** Go's SQL driver uses the string-key
  catalog/schema layout `(__SYS, __SYS, CATALOG)` / `(dbPath, schemaName)`.
  Replace it with Java's `RelationalKeyspaceProvider` layout: the typed system
  path `(NULL, NULL, int64(0))` and directory-layer domain -> database ->
  schema levels. No migration from the Go layout: data written by an earlier Go
  build is recreated. A template created through Go must load in Java and the
  reverse (`TODO_OLD.md`, “Go SQL driver stores the relational catalog…”).
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
- [ ] Run `just test-full` once end to end. Not yet run: `fdb-diff-oracle_test`
  needs the one-time FDB C++ build, and the manual targets
  (`million_record_test`, `bench_test`, `stress_test`, `bindingtester_test`) have
  never run under this recipe.
- [ ] Split `sqldriver_test` (1748 tests, ~7.5 min, 77% of its time in ~40
  sweep/probe tests) so its cheap regression pins return to the fast lane.
