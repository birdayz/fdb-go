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
- [ ] Isolation: conflict-free index-state reads and DSN/SetOption options. The
  executor DML snapshot guard (`4f61e07b5`) and statement-class admission
  (`7287666da`) are already implemented.
- [ ] IN semantics: rewrite/partition/cost behavior, covering unions, multi-binding
  product limit, and constant-IN evaluation timing; coordinate shared machinery
  with WS-F without losing either acceptance obligation.
- [ ] Semantics/pins: scalar variadic promoted-child types, Value nullability
  census, target simplification regime, adjacent/decorated literals and lexer
  boundaries, FROM-less metadata, LOG_QUERY. Decimal normalization and structured
  variadic promotion have prior implementations; check current coverage first.

**Done:** every remaining WS-E design obligation is reconciled to implementation
and an executable pin; temporal compatibility/repair documentation is shipped;
fast and full lanes plus Java/FDB acceptance pass. Then move to WS-F.

## 2. WS-F — planner scheduling, query blocks and properties

- [ ] Conditional decorrelate→simplify and merge→pushdown rule chains with
  progress-driven fallback; partition-based select merge; multi-leg pushdown;
  physical REWRITING prune; full comparator configuration; per-partition yields.
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
