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
  Product limit is implemented but parked in `git stash` ("WS-E/F-7b IN-union
  product limit"). It contains the executor check (Java
  RecordQueryInUnionPlan.java:151-153, saturating product), relational max 24,
  both rule arms carrying it, unit tests, and `in_union_max_size.yaml`. It cannot
  land before F-7b: Go plans `col1 IN (25) ORDER BY id` as InUnion where Java
  scans (`w8_in25_order_by_id_*`), so the check alone refuses a query that both
  engines answer today.
- [ ] Semantics/pins: scalar variadic promoted-child types, Value nullability
  census, target simplification regime, adjacent/decorated literals and lexer
  boundaries, FROM-less metadata, LOG_QUERY. Decimal normalization and structured
  variadic promotion have prior implementations; check current coverage first.
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
  already in place (`d5a5132a1`). Still open: the design's D1/D2/D5 progress
  and staleness model, the `outerJoinCount` placement review, and F-7a/F-8.
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
  a cascades matching change awaiting its ACK. Reassigned to that.
- [ ] Reconcile F-6/F-7b with RFC-191's existing `Fetch(InJoin)` ruling; see
  `DIVERGENCES.md` “Plan choice: an ordered IN over a non-covering index”.
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
- [ ] **SUM-index decision:** whether Go-created metadata may add a
  COUNT(col) companion (`__NONNULL_COUNT`) for nullable SUM indexes. Current
  fail-closed scan fallback is correct but expensive. No silent metadata change.
- [ ] **Lucene decision:** queue/heartbeat/quota/spell-check/state contracts
  presuppose a backend Go does not implement. Obtain a scope ruling, not a fake
  completed checkbox.
- [ ] **Catalog/keyspace migration decision:** Go's SQL driver still uses the
  string-key catalog/schema layout rather than Java's typed/directory-layer
  layout. Decide compatibility rollout and migrate existing data safely. Copy
  stored MetaData bytes verbatim; do not rebuild old templates from DDL. Preserve
  record-type keys, union numbers and index versions; literal-carrier changes
  require the versioned carry/rebind path. This predates the version upgrade but
  must remain visible (`TODO_OLD.md`, “Go SQL driver stores the relational catalog…”).
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
