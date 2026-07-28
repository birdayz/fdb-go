# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.12.11.0**. FDB wire protocol: **7.3.77**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types, 9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection with per-ordering properties.

---

## LATEST PRIOS 2026-07-24 — Cascades quality follow-ups

Source: 2026-07-24 end-to-end Cascades quality assessment on `master` at
`2543c4cf5`. Work branch: **`feat/cascades-quality-followups`**. Implement one
item at a time, with a focused regression test and review before starting the
next item. Correctness work is correct-or-loud: an unsupported shape must fail
closed rather than silently alter rows or output schema.

**Correctness and semantic identity:**
- [x] **CQ-1 (LOW, audit-corrected) — harden WHERE predicate installation.**
  The suspected fail-open path is not reachable: `visitWhere` retains a
  text-only `LogicalFilter` when predicate construction declines, and the
  Cascades translator rejects that filter rather than translating its input as
  an unfiltered scan. A focused regression now pins that behavior. Every live
  `upgradeFirstFilter` installation result is checked and returns a typed 0AF00
  if the builder's filter-on-unary-spine invariant ever breaks; successful
  comparison and correlated-EXISTS attachment paths are pinned too.
- [x] **CQ-2 (HIGH) — include projection aliases in semantic identity.**
  Confirmed reachable and fixed: logical and physical memo identity now includes
  each slot's executor-visible `OutputColumnName` (including the Value-derived
  name when its alias is missing/empty), and projection
  constructors/accessors defensively protect that identity.
  `ProjectionElimRule`, physical `IsIdentity`, and `RemoveProjectionRule` now
  preserve schema-bearing aliases and reject a whole-row value over the wrong
  quantifier. Internal `CorrelationIdentifier` spellings remain intentionally
  excluded: they are alpha-renamable planner binders, not SQL output aliases,
  as pinned by `TestRelationalAliasCompleteness` and
  `TestPlanIdentity_SemanticHashAliasInvariant_RFC176`. Schema-aware memo
  identity is deliberately decoupled from the historical schema-neutral
  cost/designation/extraction tie-break hash, so this correctness refinement
  does not churn otherwise-equivalent chosen plan shapes. Focused memo, rule,
  plan-identity, and full-pipeline regressions pin all paths.
- [x] **CQ-3 (HIGH) — fix correlated EXISTS over non-grouped aggregates with
  LIMIT/OFFSET.** The front end now proves the non-grouped aggregate's exact
  one-row cardinality *before* applying literal pagination and carries the
  resulting known EXISTS truth into the translator. Direct/top-level-AND WHERE
  consumers substitute TRUE/FALSE in both polarities, so `LIMIT 0` and every
  positive OFFSET are empty even when the correlated raw input has multiple
  matches; positive LIMIT with zero OFFSET stays true. Pagination atoms still
  unresolved at planning time and data-dependent positive OFFSET for
  non-global shapes (including GROUP BY) reject typed-loud instead of falling
  into raw row-existence. Public bound LIMIT/OFFSET arguments are substituted
  before parsing and retain their exact literal semantics. Projected,
  nested-boolean, and JOIN-ON known-truth
  consumers also reject typed-loud until their distinct substitution paths are
  implemented; HAVING remains blanket-rejected. The arity>=3 gathered-cluster
  projection bypass performs the same fold before existential lowering, and
  DML shares SELECT's parse-tree window-aggregate rejection. Focused
  classifier/fold/planner tests, live COUNT/MAX/SUM + joined-outer/gathered +
  DML FDB regressions, and yamsql controls pin the result.
- [x] **CQ-4 (HIGH) — finish correlated scalar-subquery cardinality enforcement.**
  Projection and scoped single-source WHERE consumers now share one LEFT-scalar
  join authority. Every data-dependent unpaged inner (raw rows or groups) carries
  a strict FirstOrDefault barrier, so a second post-WHERE/GROUP-BY/HAVING/ORDER-BY
  row raises SQLSTATE 21000 per outer row; empty remains NULL. WHERE materializes
  `[outer..., scalar]` on a fresh typed binding, filters above the LEFT/null and
  strict-cardinality barriers, then projects the private scalar slot away.
  Scalar-free top-level AND conjuncts run on the outer leg first, preventing an
  excluded outer row from spuriously evaluating a bad scalar. Written
  LIMIT/OFFSET is preserved exactly: multi-row-capable shapes accept LIMIT 0/1,
  intrinsically-single global aggregates also accept larger limits, and
  data-dependent LIMIT >1 or unresolved pagination rejects typed-loud until a
  post-pagination scalar-collapse mode exists. SELECT DISTINCT, window/QUALIFY,
  group-key-only HAVING, mixed/dual carriers, multi-source WHERE, and DML remain
  explicit 0AF00 boundaries rather than silent rewrites. Plan, live FDB, and
  yamsql regressions pin 0/1/2+ rows/groups, post-HAVING cardinality, ORDER BY
  without LIMIT, explicit/bound pagination, NULL-on-empty/IS NULL, output-schema
  hiding, per-outer isolation, and the correct-or-loud composition guards.
- [x] **CQ-5 (MED) — preserve SELECT-list order for GROUP BY output.**
  `LogicalAggregate` now keeps its native `[group keys..., aggregate calls...]`
  row private and records an exact visible SELECT-ordinal → native-ordinal
  contract. Every SQL aggregate exposes one final `LogicalProject` in immutable
  SELECT-list order (including duplicates, aliases, computed items, zero-call
  grouping, and DML/UNION/derived consumers); labels live only at that public
  boundary. Structural key/call binding and ORDER BY use exact native ordinals,
  including qualified same-bare joined keys, while malformed, stale, ambiguous,
  or unmatched references reject typed-loud instead of falling back to names.
  Focused unit, plan-shape, metadata, INSERT…SELECT, UNION/derived-table,
  correlated-scalar, gathered-unnest, and live FDB regressions pin the ABI,
  SELECT order, pagination placement, and fail-closed edges.

**Planner lifecycle and operations:**
- [x] **CQ-6 (MED) — make the Planner lifecycle explicit and safe.**
  A planner now owns exactly one non-nil planning attempt: an atomic one-way
  claim rejects every later non-nil `Plan` with `ErrPlannerAlreadyUsed` before
  touching the root, memo, task stack, counters, or diagnostics. `Plan(nil)`
  remains a zero-work no-op. This fail-closed contract is structural because a
  run mutates the whole caller-owned Reference DAG, which planner-field resets
  cannot restore; retries must construct a fresh planner. The memo's
  re-exploration callback is detached on every exit so its observable
  post-run state cannot enqueue orphaned tasks. Regressions pin reuse after
  success, a partial-stack task-cap failure, and a fully-drained extraction
  error, including exact zero-work rejection and unchanged memo/root state.
- [x] **CQ-7 (MED) — make planning cancelable.** `PlanWithContext` is the only
  entry point production code can reach, because the context-less `Plan` is
  declared test-only — the compiler, not a source scan, enforces it; the
  run-scoped context reaches the task driver, rule calls, exponential rule
  seams, and recursive plan extraction, and is never retained on the Planner.
  Cancellation preserves both the standard `Canceled`/`DeadlineExceeded`
  classification and custom cause, consumes the CQ-6 single-use planner, and
  always detaches the memo scheduler. SELECT, DML, scalar-subquery, and EXPLAIN
  paths propagate cancellation directly, and both entry points share one
  lifecycle authority. Deterministic regressions pin zero-work
  pre-cancellation, deadline causes, exact mid-task progress/residual work,
  extraction cancellation, error precedence, retry rejection, and public
  embedded boundaries.
- [x] **CQ-8 (MED) — preserve planner failure diagnostics.** One classifier,
  `translatePlannerError`, now maps every `PlanWithContext` failure and is
  called by all three user-facing planner callsites (SELECT, DML,
  scalar-subquery), which previously discarded the error, interpolated it with
  `%v`, and discarded it again respectively. Its default is INTERNAL ERROR with
  genuinely-unplannable as the one explicit arm — the polarity of Java's
  `ExceptionUtil.recordCoreToRelationalException`, whose default admits
  ignorance while `UnableToPlanException` is the explicit arm. The inverse would
  make every future planner failure mode assert something unproven about the
  user's SQL. `UnplannableIndexOnlyResidualError` is the only planner error that
  is a verdict on the query rather than evidence about the engine, so it alone
  keeps `0AF00` and its verbatim wording (the scalar-subquery pipeline keeps its
  own); the conformance corpus confirms no other shape needed naming. Memo
  yield-invariant violations and structural extraction failures consequently
  land on `XX000` instead of `0AF00`, and their text — which formats expressions
  with `%T` — is withheld from the rendered message while staying reachable
  through `Unwrap`, so Go type names no longer cross the SQL boundary. Since
  nothing in the repo logs the withheld text (there is no production
  `PlanGenerationLogger`), the rendered error still names the failure's
  *family* — `planner invariant violation`, `plan rebuild failure`, or
  unclassified — read from a type tagged at each minting chokepoint rather than
  from the message text, so 39-plus distinct engine bugs stay triageable without
  leaking anything. The three
  complexity caps get a **Go-only** `54F02` (class 54, program limit exceeded):
  Java's `PlannerConfiguration` never sets any of the three cap setters and all
  are gated on a bound defaulting to 0 ("unbound"), so Java's SQL layer cannot
  raise this at all — there is no shared surface to conform to, and `XXXXX`
  would have ported an omission from a path Java never walks. `54F01` is
  deliberately not reused (it means execution-time). Its user-facing message is
  an actionable "query is too complex to plan" rather than the internal
  sentinel's wording, which named a Go config field and rendered twice; the
  sentinel stays in the cause for `errors.Is`, and the caps carry Java's
  `addLogInfo` context (`max_task_count`/`task_count` and equivalents) via a
  typed `PlannerBudgetExceededError`. Auditing that code turned up three
  *pre-existing* Go-only codes (`21000`, `22003`, `22012`) that a doc comment
  had implicitly denied, so the enum difference is now a checked artifact:
  `goOnlyErrorCodes` lists all four with reasons, `DIVERGENCES.md` tabulates
  them, and `TestErrorCodesMatchJava` diffs the Go enum against a captured
  snapshot of Java's, failing on an unlisted Go-only code, a stale entry, or a
  Java code missing from Go. Cancellation still propagates untouched.
  Regressions drive every arm directly and pin the default polarity, cause
  preservation, and the non-leak; a six-way self-join exhausts the real
  100,000-task budget — no injected cap, no seam — through the production SELECT
  path, and against real FDB through the driver for SELECT, DELETE, and UPDATE,
  the last two reaching the DML callsite that needs a live connection to execute
  at all. Also fixed the round-cap message, which said 10 rounds against an
  actual cap of 100 and is now user-visible.

**Optimizer quality and scalability:**
- [x] **CQ-9 (HIGH) — wire the planner options trio; give wide join enumeration
  an opt-in bound.** The item's original framing — "make the 5-way all-live star
  converge by default" — was wrong and is corrected here. Java caps on the same
  shape at the same settings: `RecordQueryPlannerConfiguration` reads
  `DONT_DEFER_CROSS_PRODUCTS_MASK` INVERTED (unset ⇒ defer, `:312`) and
  `JOIN_RIGHT_DEEP_MASK` directly (unset ⇒ off, `:398`), which is exactly Go's
  `DefaultPlannerConfiguration`. So an all-live star wide enough to exhaust the
  budget exhausts it in both engines, and bounding enumeration by default would
  have been Go EXCEEDING Java while changing the plan shape of every multi-way
  join. No default moved. Measured at HEAD: hub+4 (five-way) converges at ~67k
  tasks and hub+5 (six-way) is the narrowest all-live star that caps — the
  existing star-budget comment claiming hub+4 already capped was stale and is
  corrected.
  The real defect the audit found is that Go's SQL surface accepted three planner
  options and silently ignored two of them: `DISABLED_PLANNER_RULES` and
  `DISABLE_PLANNER_REWRITING` were defined on the option enum with nothing in the
  planner reading either, and `PLAN_RIGHT_DEEP` was not defined at all — while
  both capabilities (`Planner.DisabledRules`, `PlannerConfiguration.
  ShouldJoinRightDeep`) already existed and were honored. Java's
  `PlannerConfiguration.of` + `buildRecordQueryPlannerConfiguration` read FOUR
  option names, three of which Go can act on; all three are now resolved in ONE
  place, `plannerOptionsFrom`, and turned into a planner in ONE place,
  `newCascadesPlanner` — the only planner-construction site in the embedded
  engine, so a query path cannot honor an option on SELECT and drop it on DML or
  on a scalar subquery (a single-funnel convention, not a type-system guarantee:
  `cascades.NewPlanner` stays exported). The FOURTH, `INDEX_FETCH_METHOD`, has
  the same accepted-and-ignored defect but is NOT plumbing — Go has no
  remote-fetch implementation at any layer — so it is documented at the option
  and tracked as CQ-9a below rather than faked. `DISABLE_PLANNER_REWRITING` folds Java's
  `RewritingRuleSet.OPTIONAL_RULES` into the same disabled-name set that
  `DISABLED_PLANNER_RULES` populates, exactly as Java's `disableRewritingRules()`
  does, and `FinalizeExpressionsRule` is excluded for Java's reason (it stamps
  FINAL; planning cannot proceed without it). `PLAN_RIGHT_DEEP` is added with
  Java's name, boolean type, and `false` default. Rule names are now the Java
  simple-class-name spelling (`isRuleEnabled` keys on `getClass().getSimpleName()`)
  instead of Go's `%T`, which no user-supplied value could ever have matched —
  that was a second, silent way for the option to do nothing. The resolved
  options also join the plan-cache key: the cache is per-connection and a plan
  built under different planner options is a different plan, so without it
  changing an option mid-connection would keep serving the previous plan. Java's
  `QueryCacheKey` carries the whole `PlannerConfiguration` for the same reason;
  the all-defaults case renders empty AND the delimiter is then omitted
  entirely, so the default scope stays byte-identical to the pre-change form —
  asserted against that literal form, not merely against the part being empty.
  Each option is pinned by a test that fails if it is accepted and ignored, not
  merely one that shows it does not crash: `PLAN_RIGHT_DEEP` brings the hub+5
  star from a capped 100,000 tasks to 39,464 (Cascades) / 53,367 (SQL surface)
  and still returns the right rows, while `false` and unset both still cap;
  `DISABLED_PLANNER_RULES=[MatchLeafRule]` turns an index plan into a full scan
  with identical rows, and neither an unknown name nor the Go `%T` spelling
  changes anything; `DISABLE_PLANNER_REWRITING` disables exactly Java's optional
  set — asserted against a LITERAL transcription of Java's
  `RewritingRuleSet.OPTIONAL_RULES`, because comparing against the Go helper that
  derives that set was circular and could not detect drift — and turns a
  rewritten correlated outer join into a plain nested-loop outer join. A separate
  differential pins the property that matters most for a search-space
  RESTRICTION: on a hub+3 star, which converges both ways, right-deep yields a
  demonstrably different plan and a byte-identical 9-row answer. All of it runs
  end to end through the driver against real FDB, plus deterministic
  Cascades-level and embedded-level gates. Bounding enumeration at
  the SOURCE (the RFC-074 PR-C2 bipartition lattice) remains separate, larger,
  and unstarted.
- [ ] **CQ-9a (MED) — `INDEX_FETCH_METHOD` is accepted and silently ignored.**
  Surfaced by CQ-9's audit. Java's `PlannerConfiguration.of` reads a FOURTH
  planner option beyond the three CQ-9 wired: `INDEX_FETCH_METHOD`, via
  `OptionsUtils.getIndexFetchMethod` (`OptionsUtils.java:59-60`) into
  `RecordQueryPlannerConfiguration.setIndexFetchMethod`, which every
  `MatchCandidate` (`ValueIndexScanMatchCandidate:241,268`,
  `AggregateIndexMatchCandidate:417`, `WindowedIndexScanMatchCandidate:410,436`)
  stamps onto the `RecordQueryIndexPlan` it builds. `api.OptIndexFetchMethod` is
  read by nothing in Go.
  Severity is narrower than "ignored" implies, and the distinction should not have
  to be re-derived. `SCAN_AND_FETCH` asks for what Go always does, so it is honored
  in effect. `USE_REMOTE_FETCH_WITH_FALLBACK` — Java's DEFAULT, hence what nearly
  every caller gets — explicitly permits falling back to scan-and-fetch, which is
  what Go does, so the DEFAULT PATH IS CORRECT rather than merely tolerated. The
  single sharp edge is an explicit `USE_REMOTE_FETCH`, which demands remote fetch
  with no fallback: Java issues a mapped-range scan, Go silently scan-and-fetches.
  Same rows either way; a different round-trip profile than the caller asked for.
  This is NOT option plumbing and must not be closed by adding a config field the
  planner reads and no executor honors: the capability is absent end to end.
  `SCAN_AND_FETCH` is what Go already does unconditionally; `USE_REMOTE_FETCH`
  needs FDB's `Transaction.getMappedRange` (Java drives it through
  `IndexPrefetchRangeKeyValueCursor`), which neither FDB client Go uses exposes —
  `pkg/fdbgo/fdb.Transaction` has only `GetRange`, and Apple's Go binding has no
  mapped-range call either. Go's index-scan plan has no fetch-method field to
  carry the choice, and its wire form would need one for plan-serialization
  parity.
  Order of work: (1) add `GetMappedRange` to the pure-Go client against the C++
  7.3.77 spec, under the client-engineer gate (C++ is the spec; wire compat is
  the hard line); (2) add a remote-fetch index cursor in the record layer with
  the fallback semantics `USE_REMOTE_FETCH_WITH_FALLBACK` names; (3) carry the
  method on the index-scan plan and stamp it from `PlannerConfiguration` at the
  match candidates, as Java does; (4) only then read the option in
  `plannerOptionsFrom`. Until (1)-(3) exist the option's own doc comment says
  plainly that it is not honored, and its default stays Java-identical so the
  default option set does not diverge on the wire.
- [x] **CQ-10 (MED) — make winner comparison globally order-safe.** Cost-model
  criterion #6 (`compareInPlan`) was not antisymmetric: two unSARGed IN-plans
  had BOTH orientations answer "+1, the first argument is worse", and a SARGed
  IN-plan against an unSARGed one had one orientation abstain while the swapped
  one short-circuited. Either way `less` was false both ways, so the decision
  skipped every later rung — fetch counts, type filters, sort count, the full
  cost model — and fell to the plan-hash tie-break, or, in `OptimizeGroup`'s
  fold (which compares without a tie-break), to member insertion order. The rung
  now ranks both sides via `inPlanPenaltyRank`, a total preorder on `{0,1}`, so
  antisymmetry and transitivity hold by construction. **Deliberate divergence:**
  Java has the identical defect; the derivation, its file:line evidence and the
  pair table live in DIVERGENCES.md ("Criterion 6 — IN-plan SARG penalty").
  Pinned by a property sweep over a 28-plan IN-rooted corpus (378 pairs for
  irreflexivity/antisymmetry/totality; 15,600 ordered triples and 52
  permutations for transitivity and permutation-independent minimum), two
  regressions on the concrete symptom, an end-to-end `OptimizeGroupTask` test
  (all 24 insertion orders of a four-member group elect the same winner, and it
  is the right one), and the SQL-level `in_plan_winner_stability` corpus file.
  **Reachability and impact, measured rather than assumed.** Byte-identical
  plans on their own prove neither reachability nor safety — an arm that is
  never reached also changes nothing — so both were instrumented. The both-IN
  arm fires 265 times across the six SQL-level suites, every one the
  `pa=1, pb=1` case where Java answers `+1` and Go answers `0`. Over one
  plan-shape corpus pass it fires 27 times (`in_list_pushdown` ×14,
  `in_plan_winner_stability` ×7, `in_over_intersection` ×4, `e2e_inventory` ×2);
  before the new file, 20. Reconstructing the pair's winner both ways over those
  27: **21 agree** with the pre-fix hash tie-break and **6 flip** —
  `in_over_intersection.yaml` ×4 and `e2e_inventory.yaml` ×2, both pre-existing
  files; the new file contributes 0 flips. The plans still hold, for a reason
  that differs per file and was checked, not assumed: in `in_over_intersection`
  the elected plan contains no IN operator at all (a third candidate beats both
  members of the flipped pair, the IN surviving as a residual), while in
  `e2e_inventory#4` the elected plan IS one member of the flipped pair — an
  `InUnion` chosen by the ordered-member retention path rather than by this
  rung. Independent of both explanations: regenerating the whole 339-file /
  2452-query plan-shape baseline with the pre-fix rung forced back on is
  byte-identical to the fixed rung's, which is byte-identical to the committed
  golden. No plan-shape change to any pre-existing query; the golden grows only
  by the new file's six entries.
  **Found by the sweep and deferred to CQ-10b:** criterion #7
  (`comparePrimaryScanVsIndexScan`) is a separate, pre-existing INTRANSITIVITY.
- [x] **CQ-10a (LOW) — SARG detection now folds comparisons, not aliases.**
  Outcome: **ported Java's granularity**, measured behaviour-neutral. Java's
  `ComparisonsProperty.ComparisonsVisitor` folds the intersection legs' child
  `Comparison` SETS with `intersected.retainAll(...)`
  (`ComparisonsProperty.java:126-141`) and only then filters to equality
  `ValueComparison`s and flat-maps their correlations
  (`PlanningCostModel.java:441-450`); Go folded the derived ALIAS sets.
  `collectSargedComparisons` / `intersectChildComparisons` /
  `comparisonsFromRanges` / `equalityComparisonAliases` now reproduce Java's two
  stages, with set membership by `comparisonsEqual` (the semantic equality
  Java's `Comparisons.ValueComparison.equals` = `semanticEquals(o,
  AliasMap.emptyMap())` implements, `Comparisons.java:1694-1696`, `:1651-1657`).
  **The divergence shape this item was filed with was WRONG and the correction
  matters:** the indexed field is NOT part of a `Comparison` — it is positional
  in `ScanComparisons`, and `getComparisons()`
  (`RecordQueryPlanWithComparisons.java:44-67`) yields only the RHS comparisons
  — so two legs carrying `f1 = $x` and `f2 = $x` hold EQUAL comparison objects,
  `retainAll` keeps them, and Java agrees with the alias fold. The real
  disagreeing shape is two legs binding one alias through DIFFERENT COMPARANDS,
  which Java reaches via record-typed IN:
  `InComparisonToExplodeRule.createSimpleEqualitiesForRecordTypeValue`
  (`:205-221`) splits `(a, b) IN ((1, 2), ...)` into `a = $x.0` / `b = $x.1`.
  **Measured, not assumed.** Instrumenting `compareInOperator` to compute both
  granularities with coverage and recursion held identical — so the intersection
  fold is the only variable — over the **five SQL corpora** (explaindiff,
  memoinvariant, plandiff, rowdiff, embedded) gives **48919 rung-6 evaluations,
  112 of them with an intersection under the IN-plan, and 0 disagreements** —
  because all **295** equality comparisons correlated to an IN binding carried a
  BARE `QuantifiedObjectValue` operand (Go's `rule_in_to_explode.go:159-161`
  emits exactly that and has no record-element branch; `(b, c) IN ((7, 5),
  (2, 6))` fails Cascades translation outright). So the port is a no-op on every
  currently reachable shape — golden byte-identical, no plan-shape change, no
  stress run warranted — and the equivalence stops depending on a missing
  feature staying missing.
  **The `cascades` unit-test suite is deliberately EXCLUDED from those figures,
  and must stay excluded.** It contains fixtures built expressly to disagree —
  `TestCollectSargedComparisons_IntersectionFoldsComparisonsNotAliases` uses
  `FieldValue` comparands precisely because no reachable query produces them —
  so counting it yields `disagreements >= 1` and `operands != all-QOV` and
  measures the test suite rather than the language. An earlier revision of this
  entry quoted 50478/116/930 over a suite list that named seven suites while
  saying six and included `cascades`; those numbers are withdrawn. Synthetic
  plans are never evidence about which shapes are reachable.
  Pinned by
  `TestCollectSargedComparisons_IntersectionFoldsComparisonsNotAliases`, which
  builds the distinguishing shape directly (verified red under the alias fold)
  and carries a same-comparand positive control so it cannot pass by returning
  nothing.
  **Found by the sweep, fixed alongside it as CQ-10c:** the same collector's
  COVERAGE — which plans it reads comparisons off at all — was a second, larger
  divergence, and a live plan-quality defect. See below.
- [x] **CQ-10c (MED) — criterion #6 now credits a SARGed PRIMARY scan.**
  Found by CQ-10a's sweep, in the same collector, and it was not cosmetic: it
  made Go plan a REDUNDANT IN-MEMORY SORT on `WHERE pk IN (...) ORDER BY pk`.
  Java's `ComparisonsProperty.ComparisonsVisitor.evaluateAtExpression` reads
  comparisons off every `RecordQueryPlanWithComparisons`, and
  `RecordQueryScanPlan` is one (`RecordQueryScanPlan.java:84-88`;
  `hasScanComparisons()` defaults true at
  `RecordQueryPlanWithComparisons.java:71-73`). Go read them off
  `RecordQueryIndexPlan` only, so an IN binding that HAD become a search
  argument on a primary-key range scan went uncredited, criterion #6 penalised
  the IN-plan, and the planner elected a shape needing a sort it did not need:

      schema: tbl(id BIGINT NOT NULL, k BIGINT NOT NULL, a, b, PRIMARY KEY (id, k)), INDEX ia ON tbl (a)
      SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id, k
      before: InMemorySort([ID ASC, K ASC], InUnion(Scan(TBL, [=]), bindings=1, ASC))
      after:  InUnion(Scan(TBL, [=]), bindings=1, ASC)

  The sort's input was already in primary-key order. Nothing was red: the rows
  were correct, so every row-level test passed, and the corpus had the sorted
  plan recorded as expected. **The shape has to keep the IN-plan at the ROOT to
  reach the rung at all** — criterion #6 inspects the root expression, so
  `SELECT id, k FROM ... ORDER BY id` plans identically before and after
  (checked per query against both collectors, not assumed), which is why the
  new corpus file uses `SELECT *`.
  Fixed by matching the visitor's structure rather than adding one case: the
  collector now takes its own comparisons off any `ScanComparisonProvider` (the
  Go analogue of `RecordQueryPlanWithComparisons`) and UNIONS them with its
  children's, where before a node either returned its own and stopped or
  returned its children's and ignored its own. Java's third arm, unwrapping
  `RecordQueryCoveringIndexPlan`, has no Go analogue — covering is a flag on the
  index plan, not a wrapper.
  **Measured.** Over the five SQL corpora (explaindiff, memoinvariant, plandiff,
  rowdiff, embedded — the `cascades` unit suite is excluded, see CQ-10a on why
  synthetic fixtures must never enter a reachability count) **110 of 48919
  rung-6 evaluations flip verdict**, every one of them 0→SARGed with a SARGed
  primary scan in the tree, and none involving an intersection. Re-measured on
  the final corpus, the DESC scenario included: it adds 5 evaluations and no
  flips. (An earlier sweep counted 36 on the PRE-FIX plan population; the
  populations differ because changed verdicts change which candidates get
  compared, so the two are not the same measurement.)
  **Plan-shape review — every diff, individually.** Regenerating the baseline
  with the new corpus file but the OLD collector isolates the file's own
  entries; diffing that against the NEW collector isolates the fix. The fix's
  entire effect on the 340-file / 2464-query corpus is **5 queries, all in the
  new file, each losing exactly one `RecordQueryInMemorySortPlan` over an
  unchanged `InUnion(Scan(TBL, [=]))`** — same justification for all five, the
  sort's input was already in the requested order. **Zero pre-existing corpus
  queries changed**; the committed golden diff is purely additive.
  Pinned by `pkg/relational/conformance/yamsql/testdata/in_over_primary_scan_sarg.yaml`
  (8 scenarios against real FDB — plan assertions AND exact ordered rows, since
  a redundant sort returns the same rows in the same order and only a plan
  assertion can see it; 4 of the 8 go red on the pre-fix collector) and by
  `TestCollectSargedComparisons_PrimaryScanCountsAsSarg` /
  `TestCollectSargedComparisons_NodeComparisonsUnionWithChildren`, both verified
  red on the pre-fix collector.
- [x] **CQ-10d (LOW) — no DESCENDING IN-union variant is enumerated, so
  `WHERE pk IN (...) ORDER BY pk DESC` materialises a sort the ascending form
  does not.** Surfaced by CQ-10c and deliberately not fixed there. Both
  directions now plan the same shape:
  `InUnion(Scan(TBL, [=]) REVERSE, bindings=1, DESC)`.
  **PARITY, not an extension** — Java has all three of the behaviours Go was
  missing, so nothing here exceeds Java and DIVERGENCES.md needs no
  extension entry (it gains one for a *limitation* the fix makes reachable, see
  the last paragraph). The rule (`rule_implement_in_union.go`) was never the
  problem; it enumerated a descending candidate the moment one could exist.
  Three defects UNDER it, each of which alone suppressed the whole shape:
  1. **`PrimaryScanMatchCandidate` reported no key order at all.** Java's
     inherits `computeMatchedOrderingParts` from `ValueIndexLikeMatchCandidate`
     (`ValueIndexLikeMatchCandidate.java:63-118`); Go's did not implement the
     optional `OrderingPartsComputer`, so `adjustMatchForMatchableSort` fell
     back to the child's empty parts, `SatisfiesRequestedOrdering` could never
     match a requested value, and a primary-scan match never got a reverse
     variant. Measured directly: `WHERE id = 1 ORDER BY k DESC` planned
     `InMemorySort([K DESC], Scan(TBL, [=]))` where the index mirror planned
     `Fetch(IndexScan(IA, [=]) REVERSE)`. Fixed by implementing it — a primary
     key is flat scalars, so neither the duplicate-producing branch nor the
     trimmed-PK suffix applies.
  2. **The IN-like SELECT downgraded its parent's ordering request to
     Preserve.** `pushRequestedOrderingToSelectChild` declines a
     correlation-free ordering part in a multi-quantifier SELECT; Go's SQL
     translator bakes sort keys as positional field reads with NO correlation,
     so every part was declined. Java never faces it — its `pushDown` produces
     values correlated to the child and keeps them (`RequestedOrdering.java:228`,
     an empty correlation set passes `allMatch(current)` vacuously). The
     Preserve then joined the concrete request in the base reference's
     constraint set, `SatisfiesAnyRequestedOrderings` resolved BOTH, and Java's
     `AbstractDataAccessRule.java:684-700` deliberately builds a FORWARD access
     only for BOTH (the reverse arm is commented out on purpose: it would double
     every data access). Fixed by recovering ownership from the result value —
     when the SELECT's result IS one child's row, a correlation-free part has
     exactly one possible owner. The composite-result case still declines.
  3. **The merge plans advertised their comparison keys without a direction.**
     `RecordQueryInUnionPlan.HintOrdering` (and `MergeSortUnionPlan`'s) ignored
     `reverse`, so a descending merge looked ascending and `ImplementSortRule`
     kept a sort over rows already in the requested order. Java derives the
     direction from the comparison-key ordering parts
     (`OrderingProperty.visitInUnionOnValuesPlan`).
  **Measured.** Verdict flips on cost-model criterion #6, same corpus (the new
  scenarios included) with the code change as the only delta, over four corpora
  (explaindiff, memoinvariant, plandiff, embedded): **0 flips across the 5050
  pairs present in both populations.** The population grows 90748→97348
  evaluations / 5371→6553 distinct pairs, because reverse access paths and
  descending IN-unions are new candidates to compare; 61 of the new pairs get a
  non-tie verdict, every one of them an unSARGed IN-plan ranked against a plain
  filtered scan. The rung itself is untouched.
  **Plan-shape review — every diff, individually.** 32 of 2465 corpus queries
  move, in three families. **10 lose an in-memory sort to a reverse scan**
  (`order_by_elimination` ×7, `in_over_primary_scan_sarg` ×1 — the item's own
  reproducer, `in_plan_winner_stability` ×1 where two full scans plus a sort
  become one reverse full scan, and `in_subquery_decomposition` ×1 where the
  ordering push restored by fix 2 lets a covering index scan serve `ORDER BY v`);
  all strictly better. **21 change only the predicate GROUPING of an otherwise
  identical `PredicatesFilter(Scan(T))`** (`[1 preds]`→`[2 preds]`), all of them
  `ORDER BY <pk>` with no helpful index: the primary-scan match now satisfies
  the ordering, so the zero-prefix access is kept rather than discarded, and its
  compensation renders the conjuncts unflattened instead of as one AND. Same
  shape (`shape:` lines identical), same work, same rows. The 32nd is the
  descending IN-union itself. **1M stress: every EXPLAIN string and every row
  count identical**, timings within noise; it contains no descending IN query,
  so it bounds collateral damage rather than measuring the fix.
  The `explaindiff` reachability ratchet moves 32→38 no-quantifier edges,
  measured to be exactly 6, all in `order_by_elimination.yaml`, all the same
  already-counted `TypeFilter(Scan)` adapter class, unreachable count still 0 —
  more ordered primary-scan accesses through the same adapter.
  Pinned by `in_over_primary_scan_sarg.yaml` against real FDB — the descending
  scenarios that assert a ROW ORDER assert it exactly (`id=1` carries two `k`
  values, so a leg scanned forward under a descending merge, or a merge run
  ascending, both come back visibly wrong); the prefix-only `ORDER BY id DESC`
  scenario is `unordered` and asserts rows ONLY, because SQL leaves the two
  `id=1` rows mutually unordered there and this file does not pin an order SQL
  does not promise. Also pinned by `descending_in_union_test.go`, whose
  behavioural tests were each verified RED before the fix.
  **The `ORDER BY <pk-prefix> DESC` shape is a REGRESSION, not parity** — an
  earlier revision of this entry claimed Java elects the same full reverse scan.
  It does not; that claim was reasoning, not measurement, and it was wrong. The
  absence of the IN-UNION there IS Java-identical (the ordering check skips
  equality-bound columns, reports BOTH directions, and BOTH deliberately yields
  a forward access only — `AbstractDataAccessRule.java:684-700`), but Java then
  plans a sorted IN-JOIN rather than a full scan. Tracked as CQ-10f with the
  measurement; deliberately left unpinned here rather than blessed.
  **One Go limitation is now REACHABLE** and recorded in DIVERGENCES.md: a
  MIXED-direction ordering request cannot be merged, because Go has no
  `ToOrderedBytesValue` evaluator. Both merge-union rules now fail closed on it
  (`NaturalComparisonKeyValues`) instead of silently building a plan whose merge
  runs one key the wrong way round — a latent wrong-row-order bug that only
  descending merges could have triggered. **Measured:** the IN-union gate
  declines exactly twice over the corpus, both times `parts=[ASC, DESC]` against
  a forward merge; the merge-sort-union gate declines nothing, because the union
  merge carries no equality-bound key a request could give a direction to. The
  reachable half is pinned at RULE level by
  `TestInUnionRuleRefusesMixedDirectionMerge` (drives `ImplementInUnionRule`
  itself, verified red with the gate removed, with ascending AND descending
  controls proving the fixture reaches the yield); the dormant half's premise is
  pinned by `TestDistinctUnionMergedOrderingCarriesNoEqualityBoundKeys`, so the
  fence going live would be noticed. The CORPUS scenarios do NOT pin either gate
  — the mixed-direction queries plan an in-memory sort with or without it, which
  an earlier revision of this entry and of DIVERGENCES.md wrongly claimed.
- [ ] **CQ-10f (MED, full-scan regression CLOSED by CQ-20 — sort-elimination
  parity still open; Graefe ruling OBTAINED, option (ii)/diverge
  deliberately, gated on conditions A–F with the FDB benchmark (E) able to
  flip it) — `WHERE pk IN (...) ORDER BY pk DESC` planned a FULL
  TABLE SCAN where Java plans N bounded seeks.** Introduced by CQ-10d and left
  deliberately unpinned there rather than blessed.
  ```
  SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id DESC
    before CQ-10d:  InMemorySort([ID DESC], InJoin(Scan(TBL, [=]), binding))   -- 3 bounded seeks + sort
    after  CQ-10d:  PredicatesFilter(Scan(TBL) REVERSE, [1 preds])             -- FULL TABLE SCAN
    Java:           [IN arrayDistinct(...) SORTED DESC] | INJOIN q0 -> { SCAN([IS TBL, EQUALS q0]) }
  ```
  Schema `tbl(id, k, a, b, PRIMARY KEY (id, k))`, `INDEX ia ON tbl(a)`. Planner
  default statistics, so the choice is SIZE-INDEPENDENT — the same full scan on
  a billion-row table. The Java line is MEASURED against the live fdb-relational
  planner through the conformance `SqlPlanSteps` harness, not reasoned: Java
  plans the IN-join with its bindings sorted descending, i.e. the three seeks
  with no sort at all. So this is a Go-vs-Java divergence, not the Java shape.
  **The deciding rung, measured, is NOT the one it looks like.** Criterion #2
  abstains (both whole-plan maxima unknown), residuals tie 1-1, and criterion #4
  DATA-ACCESS COUNT ties 1-1 — an IN-join executing one bounded inner scan per
  binding counts one data access, exactly like one unbounded full scan. Control
  then reaches `comparePrimaryScanVsIndexScan`, whose FIRST line
  (`primaryVsIndexRankOf`, `planning_cost_model.go`) ranks any plan carrying an
  in-memory sort into its last tier — so criterion #7 returns, and the promoted
  `inMemorySortCount` rung immediately below it never runs. The two rungs encode
  the same premise and #7's own comment says so, so the substance of "the sort
  rung breaks the tie" is right while the mechanism is not; a fix aimed at the
  sort-count rung alone would miss.
  **The premise that fails** is the one both rungs share: *once two candidates
  have done provably the same amount of real work, the one carrying an extra
  full materializing sort is strictly worse.* Three bounded seeks and one
  unbounded full scan are not the same amount of real work, and neither the
  cardinality rung (abstains) nor the data-access rung (counts NODES) can tell
  them apart.
  **Two further defects sit under it, both measured, and neither alone is
  enough.** (a) `RecordQueryInJoinPlan.HintOrdering` returns the empty ordering
  unconditionally, so a sorted IN-join can never satisfy an ORDER BY — Java
  derives one (`OrderingProperty.visitInJoinPlan`, `OrderingProperty.java:392`):
  when the inner's binding map holds the IN-bound value as a FIXED binding and
  the source is sorted, that binding becomes directional in the source's
  direction and the rest of the inner ordering is inherited. (b) The whole
  requested-ordering arm of `ImplementInJoinRule` is DEAD: it looks the
  requested part up in `richOrdering.GetBindingMap()` by Value IDENTITY, and the
  request carries the translator's baked `ID#0` while the ordering advertises
  the lazy `ID`, so it finds nothing and returns nil for every request. Every
  IN-source therefore comes from `buildSourcesFromProvided`, which hardcodes
  `sorted: true` with `reverse` left false — no descending IN-join is ever
  built. The fix for (b) is the bridge that already exists for exactly this
  (`RichOrdering.orderingKeyFor` / `CanBridgeOrderingValueRoots`).
  **Prototyped and REVERTED, with the measurement that says why it is its own
  workstream:** fixing (a)+(b) makes the descending IN-join real and eliminates
  the sort on `ORDER BY id DESC, k` — but it moves 16 further corpus plans, 15
  of them IN-union→IN-join flips on ASCENDING queries CQ-10d never touched, and
  it STILL does not fix the query above (the full scan keeps winning on a later
  structural rung). That is a milestone-sized planner behaviour change: it needs
  its own RFC and a Graefe ACK before implementation, per the query-engine gate.
  **Scope when the REMAINING (a)+(b) work is picked up:** decide the rung
  question first (is criterion #4 allowed to tie a bounded access against an
  unbounded one, or does the bounded/unbounded distinction belong in the cost
  model?), because (a)+(b) without it changes many plans and does not yet
  reach Java's no-sort shape.
  **Update (CQ-20) — the FULL-SCAN regression is CLOSED via a THIRD defect,
  distinct from (a)/(b) and NOT a milestone-sized change, so it was fixed in
  isolation while (a)/(b) stay open.** `PKScanOrdering`
  (`pkg/recordlayer/query/plan/plans/ordering.go`) reported EVERY primary-key
  column as a sorted key regardless of any equality comparison narrowing the
  scan, so a per-binding equality PK scan (`Scan(TBL,[id=q0])`, `id` really
  Fixed) and a fully unbound PK scan (`id` really Sorted-ascending) advertised
  the IDENTICAL `Ordering{Keys:[ID,K]}`. Plan partitioning
  (`expression_partition.go`'s `orderingsEqual`/`toPartitionsFromMap`) reads
  exactly that `Ordering`, so the two co-partitioned, and whichever member
  happened to be added to the `PlanPropertiesMap` first became the partition's
  sole representative — silently shadowing the bounded InJoin/InUnion
  candidate behind the unbound scan before the sort-vs-full-scan cost rungs
  above ever got to compare them. Fixed by trimming the equality-bound PK
  prefix out of `PKScanOrdering`'s `Keys`, mirroring the firstNonEq logic
  `RecordQueryIndexPlan.HintOrdering` already used and matching Java's
  `ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons`
  (`ValueIndexLikeMatchCandidate.java:126-196`), whose equality-bound prefix
  only populates the FIXED binding map and is never appended to
  `orderingSequenceBuilder` — Java's `Ordering.equals`
  (`Ordering.java:250-261`) always compares the binding map (Java has one
  `Ordering` type; Go split it into `Ordering` for partitioning and
  `RichOrdering` for richer reasoning, and the partitioning type was the one
  that lost the distinction). The reproducer above now plans
  `InMemorySort([ID DESC], InJoin(Scan(TBL, [=]), binding ASC))` — three
  bounded seeks under a sort — instead of the full reverse scan. **This is
  NOT yet Java parity**: Java needs no sort at all, because defects (a)
  (`RecordQueryInJoinPlan.HintOrdering` returning the empty ordering
  unconditionally) and (b) (the requested-ordering arm's dead Value-identity
  binding lookup) are UNCHANGED by this fix and remain their own
  milestone-sized workstream, deliberately not implemented here so the
  partition-key fix could land isolated and reviewed on its own. Corpus:
  2633 statements, **26 changed (0.99%), 13 shape flips, 0 regressions** — 5
  full-scan→bounded-InJoin (improvement, 3 DML + this SELECT reproducer's
  no-ORDER-BY control), 4 redundant `InMemorySort` eliminated (improvement),
  12 InJoin ordering-tag `binding`→`binding ASC` (neutral, all already under a
  sort), 2 InJoin→InUnion with sort eliminated (improvement), 1 REVERSE flag
  dropped on an inner scan under an outer re-sort (neutral), 1 the reproducer
  itself (full reverse scan → sorted bounded InJoin, the regression closure),
  1 `NestedLoopJoin` operand flip on an `unordered: true` cross join
  (neutral, pre-existing structural-hash tiebreak, `cte_error_codes.yaml#5`).
  Every category individually golden-reviewed against the regenerated
  `explaindiff` plan-shape baseline; zero planning-time delta. Now pinned in
  `in_over_primary_scan_sarg.yaml`: the no-ORDER-BY control asserts
  `InJoin(Scan(TBL, [=])`, and the `ORDER BY id DESC` prefix-only scenario
  (previously deliberately unpinned) now asserts
  `InMemorySort([ID DESC], InJoin(Scan(TBL, [=]), binding ASC))` with its
  exact descending rows kept, run against real FDB. Regression test:
  `TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan`
  (`pkg/recordlayer/query/plan/cascades/pk_scan_ordering_partition_test.go`)
  pins that a Fixed-bound and a Sorted-unbound PK scan land in different
  partitions, verified red without the fix.
  **Update (RFC-191 revision 3 / Graefe milestone review) — parity gap
  remains, now blocked on a Graefe ruling, not on more research.** With
  CQ-20 shipped the full-scan regression is closed; what's left is defects
  (a) and (b) (`HintOrdering` and the requested-ordering binding-lookup
  bridge). A live Java planner was instrumented (`MemoTraceProbe` on the
  `InsertIntoMemoPlannerEvent` bus, driven through the conformance server's
  `planSql` with FDB via `WithDirectIP()`) to watch what Java memoizes, not
  just what it wins. It refutes two prior hypotheses (Java DOES enumerate an
  ordered comparand InJoin for the FETCH-required shape; the ASC tie is
  decided one rung before `numSourcesInJoin`, at fetch-depth) and finds the
  actual mechanism: `PushInJoinThroughFetchRule` is registered only for
  `RecordQueryInValuesJoinPlan`/`RecordQueryInParameterJoinPlan`
  (`PlanningRuleSet.java:152-153`), never for `RecordQueryInComparandJoinPlan`
  — the class every sorted IN-source produces — so `Fetch(InJoin(Covering))`
  is structurally never built in Java, `Fetch(InUnion(...))` wins ASC on a
  depth-0-vs-depth-1 fetch-position tiebreak, and DESC reaches
  `numSourcesInJoin` instead because no depth-0 InUnion is available there
  (why the bounded-covering leg is unreachable for DESC is unresolved). This
  looks like a Java oversight, not a deliberate design choice, so Go must
  choose between reproducing the asymmetry faithfully or deliberately
  diverging — a decision this RFC documents both sides of but does not make.
  **Flagged for a Graefe ruling, full mechanism and file:line citations in
  RFC-191.** 23 of 25 previously-prototyped (a)+(b) corpus flips are
  Java-confirmed correct against the live trace; the 2 exceptions
  (`in_over_primary_scan_sarg.yaml#14,#15`, FETCH-required secondary-index
  shapes) already match Java as Go plans them today, so landing (a)+(b)
  unguarded would regress them — that is the concrete stake behind the
  ruling. Numbers corrected from earlier citations: defect (b)'s bail rate
  is 238/238 (100%) before a fix and 40/238 (17%, uncharacterised) after a
  prototyped bridge fix, not 196 or zero. The two false "InJoin gives no
  cross-value order" corpus comments are traced to two commits now, not one:
  `ec72b8e3f` originated the claim, `cebcbd94b` copied it into a second file
  citing the first rather than re-deriving it; neither cites Java.
  `ImplementInUnionRule` does not share defect (b) — its
  `bakeMergeComparisonKeys` already routes through the same
  `ColumnNameValue`/`orderingKeyFor` bridge defect (b) is about adding to
  InJoin, which is why InUnion's flips are reachable without waiting on (b).
  Status: still open, still its own milestone-sized workstream, but the
  blocker is now a decision, not an investigation.
  **Update (RFC-191 revision 4 — Graefe ruling: option (ii), diverge
  deliberately) — the ruling is IN, ACK on the design choice, NAK on
  revision 3's RFC text.** For an IN-list over a non-covering secondary
  index with an index-satisfiable requested ordering, Go elects
  `Fetch(InJoin(Covering))` where Java elects `Fetch(InUnion(Covering))` for
  ASC (DESC already agrees). Revision 3's text got three things wrong that
  the ruling corrected: (1) sortedness does NOT select Java's Comparand
  IN-join class — `SortedInValuesSource` never overrides `toInJoinPlan`;
  the actual selector is `ImplementInJoinRule.computeInSource`'s
  `isConstant()` fallback, which every SQL-originated IN-list hits
  regardless of direction, so `PushInJoinThroughFetchRule`'s exclusion of
  `RecordQueryInComparandJoinPlan` (`PlanningRuleSet.java:152-153`) excludes
  ALL SQL-driven IN-joins, not just sorted ones; (2) Go already has an inert
  guard for this at `rule_push_in_join_through_fetch.go:43-47`, with a
  fabricated "comparand values depend on the outer record" rationale, and
  it has plausibly never fired — Go **already plans**
  `Fetch(InJoin(IndexScan(IA,[=]), binding ASC))` for the unordered
  secondary-index shape today, so reproducing Java's asymmetry (option i)
  would mean REMOVING already-shipped behavior, not adding machinery — the
  RFC's cost accounting between the two options was backwards; (3) the two
  cost-model pieces revision 3 said Go would need for option (i)
  (implicit-fetch counting, fetch-depth-from-root) already exist
  (`planning_cost_model.go:321-327`, `:330-334`) — only the accident (one
  dead guard) was ever missing. The ruling's own supporting findings: an
  archaeology showing the Java exclusion is incidental (generic rule class,
  one registration site out of ~8 trio-aware sites, a 9-month gap between
  the registrations and the Comparand class shipping for an unrelated
  feature, zero comment/test/issue anywhere in Java); a reading of
  `PlanningCostModel.java:251-262`'s own "bigger [InJoin] wins" comment as
  contradicting the ASC outcome, which is instead decided by the one
  uncommented rung in the file (fetch-position) acting as an accidental
  proxy; a plan-quality argument (identical I/O, a provably degenerate
  InUnion merge for this shape family — legs can't interleave when the
  sort key's leading column IS the per-leg equality column — smaller
  open-cursor footprint, no async execution edge in Go's actual sequential
  executor). Gated on binding conditions A–F before implementation: (A)
  delete the dead guard, any future withholding must be a correlation
  predicate not a cost one; (B) `in_over_primary_scan_sarg.yaml#14,#15`
  MUST move to `Fetch(InJoin(...))` (inverted from revision 3, which had
  them as a negative test for the wrong default); (C) measure `#14`'s DESC
  shape against Java before re-blessing — revision 3's "23/25 confirmed"
  count treated it as confirmed when it was an ASC-measurement
  extrapolation; (D) a `DIVERGENCES.md` entry as a behavioural plan-choice
  divergence (not a cost-model criteria row, since no individual criterion
  diverges), with an explicit wire-compat statement — verified, not
  reasoned: continuations are decoded only by the same plan-tree shape
  that produced them (`ExecutePlan`'s type-switch, `executor.go:88-120`,
  rejects a mismatched shape loudly) and Go continuations are engine-private
  by construction (a Java-minted token is rejected outright,
  `cascades_generator.go:1205-1216`), so this plan-choice divergence is not
  a cross-engine wire concern; (E) a real-FDB benchmark at N ∈ {3,10,100}
  that can FLIP the ruling if InUnion measurably wins at any N, a permanent
  EXPLAIN test at fetch-depth 0 both directions plus a non-monotonic
  IN-list (`IN (30,10,20)`) FDB row-order test, a proof that leg
  concatenation yields the requested order for the secondary-index shape
  specifically (a correctness question that outranks the ruling if it
  fails), root-causing the 40/238 residual defect-(b) bails, and a full
  corpus/1M-stress/10x-determinism sweep at the final head; (F) (a)+(b) ship
  together with the (smaller-than-projected) third piece — deletion plus
  documentation, not new machinery — concentrating correctness risk in
  (a)+(b), reviewed against `OrderingProperty.visitInJoinPlan:392-459` line
  by line. Full text in `rfcs/191-in-join-descending-ordering.md` revision
  4. Status: **ruling obtained, option (ii)**; implementation still gated
  on conditions A–F, most consequentially the benchmark (E) which can still
  reverse the outcome.
- [x] **CQ-20 (MED) — `PKScanOrdering` reported every primary-key column as a
  sorted key even when a leading prefix was equality-bound, co-partitioning a
  Fixed-bound PK scan with a Sorted-unbound one and closing the CQ-10f
  full-table-scan regression.** Full writeup and measurements live under the
  CQ-10f "Update (CQ-20)" paragraph above — this entry exists only so CQ-20 has
  its own checklist line. One-line summary: `PKScanOrdering`
  (`pkg/recordlayer/query/plan/plans/ordering.go`) now trims the
  equality-bound PK prefix out of its `Keys`, mirroring
  `RecordQueryIndexPlan.HintOrdering`'s established firstNonEq logic and
  Java's `ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons`
  (`ValueIndexLikeMatchCandidate.java:126-196`). Deliberately does NOT touch
  `expression_partition.go` — fixing the ordering at its source was the
  narrower, Java-aligned change; the shared partition-comparison machinery
  was already correct, it was being fed lossy input. Deliberately does NOT
  implement RFC-191 defects (a) `RecordQueryInJoinPlan.HintOrdering` or (b)
  the requested-ordering binding-lookup bridge — those remain their own
  milestone-sized workstream under CQ-10f. Corpus: 2633 statements, 26
  changed (0.99%), 13 shape flips, 0 regressions (full breakdown under
  CQ-10f). 1M stress: all 23 subtests pass both sides, EXPLAIN + row counts
  byte-identical (stress corpus has no Fixed-vs-Sorted PK-scan-partitioning
  shape), runtime 153.67s baseline vs 153.69s branch — see the Stress test 1M
  baseline table below.
- [ ] **CQ-21 (LOW, found in CQ-20 review) — `PKScanOrdering` and
  `RecordQueryIndexPlan.HintOrdering` disagree on the fully-equality-bound
  degenerate case.** When every PK/index column is consumed by an equality
  comparison, `PKScanOrdering` (`ordering.go`) now returns
  `Ordering{}` (`IsKnown:false`), but `RecordQueryIndexPlan.HintOrdering`'s
  analogous corner case (index key fully equality-bound AND the trimmed PK
  suffix is empty — e.g. a composite unique index over exactly the PK
  columns) falls through to `IsKnown:true` with an empty `Keys`. Flagged by
  Graefe during the CQ-20 review as INERT today — every current consumer
  (`expression_partition.go`, `plan_properties.go`'s RichOrdering synthesis,
  the distinct-union/streaming-agg rules) branches on `len(Keys)==0`
  regardless of `IsKnown`, so the two representations behave identically
  everywhere they're read — but the two producers of the same logical
  "nothing left to sort by" fact should converge on one representation
  rather than carry a silent format difference a future `IsKnown`-branching
  consumer could trip on.
- [ ] **CQ-10e (LOW) — an aggregate index's comparisons are invisible to the
  comparisons property, from both directions.** Same axis as CQ-10c, currently
  unreachable. Java's `RecordQueryAggregateIndexPlan` implements
  `RecordQueryPlanWithComparisons`, so `ComparisonsProperty` reads its scan
  comparisons. Go's (`pkg/recordlayer/query/plan/plans/aggregate_index.go:22-38`)
  neither implements `GetScanComparisons` — so it is not a
  `ScanComparisonProvider` and contributes nothing of its own — nor exposes its
  wrapped index plan as a quantifier, so the collector cannot reach that plan's
  comparisons either. **No verdict can flip today**: the planner never places an
  IN-plan over an aggregate index, so criterion #6 never walks one. Recorded so
  the gap is closed deliberately if aggregate indexes ever appear under an
  IN-plan, rather than discovered as a wrong cost then. Closing it means making
  the aggregate-index plan a `ScanComparisonProvider` that delegates to its
  wrapped index scan; it changes verdicts only once the shape is reachable, but
  it is still a cost-model change and needs the usual stress + golden review.
- [x] **CQ-10b (MED) — make criterion #7 a total preorder.**
  `comparePrimaryScanVsIndexScan` was PAIR-RESTRICTED, inherited from Java's
  `comparePrimaryScanToIndexScan` (`PlanningCostModel.java:370-414`): it fired
  only for (lone primary scan) versus (singular index-scan-with-fetch) and
  abstained for index-versus-index. A criterion that adjudicates only some pair
  shapes is not a total preorder, and a lexicographic chain is transitive only
  if every rung is one. Over the property sweep's corpus the pre-fix rung
  produced **69 transitivity violations at the 28-plan corpus CQ-10 left behind,
  and 138 at the 32-plan corpus this item widens it to — 100% of them involving
  a primary scan** in both cases, antisymmetry clean (0 violations either way);
  cycles containing no IN operator at all
  (`sargedIndex+3fetches < primaryScan+typeFilter` by the type-filter sub-case,
  `primaryScan+typeFilter < plainIndex+1fetch` by the PREFER_SCAN config branch,
  `plainIndex+1fetch < sargedIndex+3fetches` by the fetch rung once #7
  abstains). Reproduced at `356ce2ab6`, at `4fae8f215`, and on `master`.
  **The verdicts could not simply be extended to the abstained pairs**: on the
  pairs the rung DOES adjudicate they are themselves cyclic, because the
  sub-case decides on the SUBSET relation between the two sides' comparison
  sets and a subset relation is a partial order. Four plans, every consecutive
  comparison inside Java's own guard: `P1{a} < I2{b,c} < P2{b} < I3{c,a} <
  P1{a}` (measured, `TestCriterion7_AdjudicatedCycleIsGone`). No total preorder
  can reproduce a cyclic relation, so a repair necessarily changes verdicts.
  The rung now ranks each plan independently (`primaryVsIndexRankOf`) on a
  four-tier ladder whose contested band — a type-filtered lone primary scan
  against a type-filter-free singular index-scan-with-fetch — orders by
  comparison-set SIZE, agreeing with Java's subset test on every comparable
  pair and differing only on incomparable ones, i.e. exactly the cyclic
  configuration. **The price:** on incomparable sets the size test is blind to
  WHICH comparisons they are, so an index MISSING a comparison the primary has
  can win on count alone (`primary{a,b}` versus `index{c,d,e}` goes to the
  index, where Java gives it to the primary). Java's guard is the more
  informative test and the repair gives it up — it has to, because
  `primaryMinusIndex.isEmpty()` is a subset test and the cycle is built from
  nothing but subset tests. The in-memory-sort guard Go already applied to the primary
  side becomes symmetric (a rank has no "abstain", and ranking sort-bearing
  plans last reproduces the sort-count rung directly below).
  **Deliberate divergence:** the derivation, Java file:line evidence, the tier
  table and the four-plan cycle live in DIVERGENCES.md ("Criterion 7 — primary
  scan versus index scan with fetch").
  **The property sweep's scope actually widened.** `TestCostModel_InPlanComparisonIsOrderIndependent`
  asserted transitivity and permutation-independence over the index-rooted
  subset only, precisely because of this rung; it now asserts them over the
  WHOLE corpus (496 pairs, 29,760 ordered triples, 64 permutations over 32
  plans, 6 of them primary-scan-rooted, up from 28/2). The scoped version is
  gone, not left passing alongside.
  **New pins, and exactly which are RED on the pre-fix rung.** Red (they fail
  when the pre-fix rung is forced back on, so they are genuine red-to-green
  proofs): the widened `TestCostModel_InPlanComparisonIsOrderIndependent`,
  `TestCriterion7_AbstentionCycleIsGone`, `TestCriterion7_AdjudicatedCycleIsGone`,
  `TestCriterion7_RankIsATotalPreorder` (irreflexivity / antisymmetry /
  strict-transitivity / INDIFFERENCE-transitivity over a 12-plan corpus covering
  every tier, for all three `IndexScanPreference` values),
  `TestPlanningCostModel_PrimaryVsIndexRungIgnoresSortBearingPlans`,
  `TestCriterion7_FetchPayingIndexLosesToCoveringIndex`, and
  `TestPlanningCostModel_SargRichIndexBeatsSargPoorIndex`. GREEN on the pre-fix
  rung, and deliberately so — they pin behaviour this change PRESERVES, not
  behaviour it introduces: `TestPlanningCostModel_PrimaryIndexSARGRichIndexWins`
  (its comparison sets are comparable, so the size test and Java's subset test
  agree — that is the whole point of the equivalence claim) and
  `TestPrimaryVsIndexRank_HonorsIndexScanPreference`. `TestDistinctSargCount`
  is neither: `distinctSargCount` does not exist pre-fix, so it cannot be
  compiled against the old source at all.
  **Reachability and impact, measured rather than assumed.** One instrumented
  pass over the six SQL-level suites; totals are stable to ~0.1% across runs
  (an independent reviewer measured 362,592 consultations against the 362,879
  here) because memo exploration order is not bit-reproducible between
  processes — the ZERO counts, however, reproduced exactly. The rung is
  consulted **362,879** times; **197,807** are pairs the pre-fix rung
  adjudicated, and **zero** of them change verdict (all 24 contested-band
  consultations carry one comparison per side, so the size test and the subset
  test agree). **26,083** previously-abstained pairs get a rung verdict: 10,786
  primary-with-type-filter versus primary-without and 14,147 sort-bearing versus
  sort-free are decided the SAME way by the rung immediately below (type-filter
  count, sort count), leaving **1,150** genuinely new verdicts — all of them the
  identical shape `I(tf=0,sarg=k,idx=1,fetch=1)` versus
  `E(tf=0,sarg=k,cov=1,fetch=0)`, i.e. an index-scan-with-fetch losing to a
  covering index that needs none, which the fetch rung below scores the same way
  (2 against 0). The index-versus-index search-argument ordering — the one
  dimension that could pre-empt the fetch-count and unmatched-field rungs — does
  not fire anywhere in the SQL corpus (all 49,091 index/index consultations have
  equal search-argument counts, unequal: 0), but it is NOT unreachable in
  general: it fires in the unit corpus, which is why
  `TestPlanningCostModel_ExplicitFetchCountBeforeUnmatchedFields` had to be
  adjusted, and it is now pinned directionally by
  `TestPlanningCostModel_SargRichIndexBeatsSargPoorIndex`.
  **Net effect on the comparator, which is the decisive measurement.**
  Evaluating every FULL-comparator comparison twice in the same process — once
  with the pre-fix rung, once with the ranked rung — over **967,069**
  comparisons yields **zero sign differences** (729,088 `+1`, 167,010 `-1`,
  70,971 ties, identical both ways). Every one of the 26,083 rung-level changes
  is absorbed by a rung below returning the same sign; that, not the rung's own
  statistics, is why nothing observable moves. Independently, recording the
  extracted plan of every planning across those suites (**42,560 plans**) and
  diffing pre-fix against fixed, modulo run-to-run correlation-identifier
  counters, yields **zero differences**.
  Two pre-existing unit tests changed, both deliberately and both explained:
  `TestPlanningCostModel_PrimaryIndexStrictSARGSuperset` used an
  `InMemorySort(IndexScan)` as its adversary, which the symmetric sort guard now
  ranks last — replaced by the differential
  `TestPlanningCostModel_PrimaryIndexSARGRichIndexWins` plus an explicit
  sort-guard pin; and `TestPlanningCostModel_ExplicitFetchCountBeforeUnmatchedFields`
  had one candidate carrying a search argument the other lacked, which the
  contested band now decides before the fetch rung — both candidates now carry
  one, restoring the isolation the test was written for.
- [x] **CQ-10g (LOW) — a SORTED IN-join's `sorted`/`reverse` flags claimed an
  ordering the values were never actually put in.** `RecordQueryInJoinPlan`
  carries `sorted`/`reverse` (`rule_implement_in_join.go`'s
  `buildSourcesFromProvided` sets `sorted:true` unconditionally whenever an
  explode alias correlates to a fixed equality binding — the only reachable
  arm today per CQ-10f), but `extractInValues` stored the RAW literal list
  and `executeInJoin` (`executor_new_plans.go:1023`) never consulted
  `IsSorted()`/`IsReverse()` — `WHERE a IN (3, 1, 2)` produced a plan that
  said SORTED ASC and executed 3, 1, 2. Latent, not live: `HintOrdering`
  (`ordering.go:471`) returns the empty ordering unconditionally and InJoin
  is absent from `RichOrderingHinter`, so nothing currently trusts the
  claim — but CQ-10f's planned next step is teaching `HintOrdering` to
  derive a real ordering from these flags (matching Java's
  `OrderingProperty.visitInJoinPlan`), and a prototype of exactly that
  produced wrong rows (`WHERE a IN (3,1,2) ORDER BY a` came back in literal
  order). **This item makes the claim true so CQ-10f's `HintOrdering` step
  is safe to take — it does NOT implement `HintOrdering` itself.**
  Java backs the claim by construction: `SortedInValuesSource`
  (`SortedInValuesSource.java:58-61`) sorts in its constructor via
  `InSource.sortValues(values, isReverse)` (`InSource.java:142-149` —
  `Comparator.comparing(Comparable.class::cast)`, reversed when
  `isReversed`, a no-op copy below size 2). Go's parallel
  `SortedInValuesSource`/`SortedInParameterSource` types
  (`pkg/recordlayer/query/plan/cascades/in_source.go`) mirrored the class
  names but COPIED WITHOUT SORTING, and had zero call sites anywhere
  (including tests) — dead code posing as the mechanism. Deleted; the
  live plan representation is the flat `sourceKind` + `[]any` fields on
  `RecordQueryInJoinPlan` (DIVERGENCES.md "InJoinPlan: InSourceKind +
  PushInJoinThroughFetch"), not a class hierarchy, so wiring the dead types
  through would have meant a parallel representation, not a fix.
  `in_source.go` now holds `sortInJoinValues(vals, reverse) (sorted []any,
  ok bool)`, the actual Java port, called from `OnMatch` right before
  `SetInValues` — sorts a copy with `sort.SliceStable` (Java's `List.sort`
  is stable too) over `values.CompareOrdered` (new,
  `values/compare_ordered.go` — moved out of the executor's
  `compareValues`, which is now a one-line delegate, so the sort and the
  merge-cursor/in-memory-sort ordering share ONE comparator instead of two
  that could drift). `CompareOrdered` matches Java's natural order for the
  types Java's `Comparable` cast covers (numbers, strings) and additionally
  gives `[]byte`/`[16]byte`/`bool`/`nil` the SAME total order the executor's
  merge/sort paths already use (FDB tuple order) rather than Java's
  `ClassCastException`/`NullPointerException` on those — library code here
  never panics, so an incomparable pair returns an error instead of
  crashing planning; `sortInJoinValues` responds by leaving `sorted=false`
  rather than shipping an unbacked claim.
  **Structural invariant, pinned always-on, not just in a test:**
  `plan_invariants.go`'s `ValidatePlanInvariants` — already wired into the
  no-FDB plan harness, the production `cascades_generator.go`, and
  `FuzzPlanner_Invariants` — now also rejects any `RecordQueryInJoinPlan`
  claiming `sorted` whose 2+ concrete values (`GetInValues()`) aren't
  actually in that order. Verified RED by reverting the `sortInJoinValues`
  call: `TestInJoinRule_SortedClaimIsBackedByActuallySortedValues`
  (rule-level) fails with a literal mismatch, and — unprompted —
  `TestFDB_InJoin_SortedClaimMatchesExecutionOrder` fails at the SQL layer
  with `malformed query plan: plan-invariant: InJoin claims
  sorted(reverse=false) but values are not in that order`, because the
  invariant is wired into the production planning path the embedded driver
  actually calls; both pass after restoring the fix.
  **Plan shape unchanged, measured not assumed:** `TestPlanShapeGolden`
  (byte-identical, no `-update-golden` needed) plus the full
  `pkg/relational/conformance/{explaindiff,plandiff,rowdiff,memoinvariant,yamsql}`
  suite green — sorting the literal list moved no plan and no row across
  the corpus.
  **End-to-end FDB proof, a genuine bare InJoin (not InUnion):**
  `TestFDB_InJoin_SortedClaimMatchesExecutionOrder`
  (`pkg/relational/sqldriver/injoin_sorted_claim_fdb_test.go`) — `WHERE a
  IN (3, 1, 2)` over an indexed column, no `ORDER BY` (so no
  `InMemorySort` sits downstream to mask the InJoin's own iteration order),
  asserts the plan contains `InJoin`/`ASC` and not `InUnion`, and asserts
  the actual row sequence against real FDB is `1, 2, 3` — not the literal
  `3, 1, 2` order.
  **`InParameterSource`/prepared-statement IN-lists, checked, not
  skipped:** confirmed by reading both producer sites that can reach
  `ImplementInJoinRule`'s match shape (`resultValue == QOV(the single
  non-explode inner quantifier)`) — `InComparisonToExplodeRule` (`col IN
  (v1,...,vn)`) always wraps the list as a `ConstantValue`, and SQL
  `UNNEST(...)` (`unnest_gather.go`/`chained_unnest.go`) routes through
  `ImplementExplodeRule` + `ImplementNestedLoopJoinRule` instead, since its
  result must expose the exploded value itself. **No SQL-facing rule
  today builds the QuantifiedObjectValue-collection shape
  `ImplementInJoinRule` would classify `InSourceParameter`,** and
  `executeInJoin` only ever loops over a plan-time literal list (`GetInValues`)
  — there is no execution-time value-fetch machinery for a runtime-bound
  IN-source at all, sorted or not. So "sort at the right point" is moot
  for now: there is no reachable point yet. This is a real, deeper gap
  (an `InSourceParameter`-kind InJoin would silently execute its inner
  ONCE instead of once per bound value) but it is unreachable from SQL
  today and building the runtime array-fetch + iteration path is its own
  multi-shift workstream with its own RFC, not a sorting fix — left
  explicitly open rather than touched here.
  **CQ-10f is now safe to take the `HintOrdering` step**: the ordering it
  would start advertising is backed by data, and the always-on invariant
  means any future construction site that sets `sorted=true` without
  sorting fails loud (a rejected plan / a fuzz failure) instead of shipping
  wrong rows.
- [ ] **CQ-11 (MED) — enrich and calibrate planner statistics.** Add at least
  distinct-count/selectivity hooks beyond table cardinality, keep safe defaults,
  and calibrate the model against representative indexed, skewed, and join
  workloads.

**Verification, performance, and documentation:**
- [x] **CQ-12 (LOW) — make the rule-registry tests repeatable.** The package-level
  `defaultRuleRegistry` was the only global-state leak found: a `-count=2` run of
  the whole `cascades` package panicked with
  `RegisterRule: duplicate name "concurrent_TestRuleRegistry_Concurrent_7"`, and
  the package passes at `-count=2` and `-count=3` with the registry fixed alone.
  The sweep was widened past the one package — all nine `//pkg/recordlayer/query/...`
  test targets pass at `-count=2` — and turned up no second leak; nothing is
  claimed for packages outside that subtree, which were not swept.
  The registry has no unregister and `RegisterRule` panics on a duplicate, so
  `rule_registry_test.go`'s five registering tests left permanent residue; their
  `t.Name()`-derived names are unique per test but identical across runs, so the
  second in-process iteration collided with the first. Production `init` was
  never exposed — every `register*Rules()` loop already skips a name `LookupRule`
  resolves; only direct `RegisterRule` calls from tests could do it.
  The fix removes the shared state rather than scheduling a cleanup a future test
  can forget: `ruleRegistry` grew `newRuleRegistry()` plus `register`/`lookup`/
  `names` methods, the exported trio became thin delegates to the one global
  instance, and the mechanics tests now each take a private instance. Registry
  behaviour is unchanged — in particular `RegisterRule`'s panic-on-duplicate is
  untouched, since it is correct production behaviour for a programmer error.
  The exported trio keeps direct coverage through a new read-only
  `TestRuleRegistry_ExportedAccessorsAgree` (sorted, deduplicated, every reported
  name resolves).
  The regression guard is real and needs no CI support: a `TestMain` snapshots
  `RegisteredRuleNames()` either side of `m.Run` and fails the binary if a test
  mutated it, which was verified by temporarily reintroducing a global
  `RegisterRule` call — it failed a **single** `-count=1` run naming the added
  entry. That ordering determinism is why the check lives in `TestMain`: the same
  probe left `TestRuleRegistry_ResolvesEveryRegisteredSetRule` green, because a
  `t.Parallel` test can read the registry before another parallel test writes it.
  With writes now excluded, that test's stale caveat ("registry contents are not
  stable under `t.Parallel`") is gone and its subset check is an exact set
  equality, so an unexpected extra name fails too.
  Also folded in: `typeNameForRegistry`'s doc comment claimed its
  `"*cascades.FilterMergeRule"` output was a registry key derived by
  `default_rules.go`'s init. It is not — the helper is reached only through
  `shortTypeName`, which strips down to the simple name. Prose only; no behaviour
  change.
- [x] **CQ-13 (MED) — strengthen performance and concurrency gates.** Four
  sub-claims; three held as written, one did not.
  **Benchmarks discarded planner results — held, and worse than filed.** The
  named sites (`benchmark_test.go` `p.Plan(ref)` bare at three call sites,
  `_, _, _ =` at a fourth) were real, and the sweep found the same defect in
  `plan_extraction_test.go`'s two `ExtractBestPlan` benchmarks. It also found
  the failure mode already *live*: five Value/predicate benchmarks
  (`FieldValue_Evaluate`, `ArithmeticValue_Evaluate`, `ComparisonPredicate_Eval`,
  `..._NonConstantRHS`, `KleeneAnd_Eval`) evaluated against a `map[string]any`
  row, which stopped being a recognized eval context — every iteration returned
  `*UnboundEvalContextError` and the recorded time was the error path, not
  evaluation. They now evaluate against a `values.OrdinalRow` (the production
  context) and assert the value. Every benchmark in both files now validates the
  work it times. Placement is split by what a pre-loop check can honestly cover:
  loop-invariant deterministic operations validate ONCE before
  `b.ResetTimer()` so the timed region is untouched, while per-iteration work
  (fresh Reference + fresh single-use Planner each iteration) validates inside
  the loop, where a few comparisons against microsecond-scale planning cannot
  move a delta both revisions pay. `b.StopTimer`/`StartTimer` is deliberately
  not used — the pair costs more than what it would exclude. The three
  index/aggregation benchmarks additionally assert the best expression `isPhysical`,
  because `Plan` can return a logical expression with a nil error, which is
  cheaper than real planning. The same audit was then extended to the OTHER
  binary this item puts behind a gate, `expressions_test`, which had the
  identical defect in all six of its benchmarks plus the two
  `HashCodeWithoutChildren` ones: `SemanticEquals` and `Reference.Insert`
  results were discarded, so a `SemanticEquals` regressed to always-false would
  early-out of every comparison and post a headline speedup. All eight now
  assert (match succeeded, permuted union still pairs, `Compose` merged to 3
  rather than taking its empty-operand early-return, dedup rejected, distinct
  accepted). The two hash benchmarks are the one honest exception — a hash has
  no success/failure signal, so they pin determinism only, and both the
  `bench-ci` comment and the benchmarks themselves say so rather than letting
  the gate's justification overstate its reach.
  **Expect a one-time baseline discontinuity, not a regression or a win.** The
  five repaired Value/predicate benchmarks now measure real evaluation under the
  same names they used while measuring an error return, so the first post-merge
  nightly will diff them against an S3 baseline recorded from the broken shape
  and report a large one-time move. It is an artefact of the fix, self-corrects
  on the following nightly, and should not be read as a signal either way.
  Scope caveat recorded at `benchRow`'s definition: it is a bare `[]any`
  satisfying `values.OrdinalRow`, not `executor.PositionalRow`, so these measure
  Value/predicate evaluation and interface dispatch rather than the production
  row implementation — the right boundary for a Value-tree micro-benchmark, and
  in any case cascades cannot import executor.
  **Regression baseline — the filed premise was imprecise.** A baseline and a
  comparison tool both already existed: nightly-coverage publishes
  `bench-results.txt` to S3 as `bench/master-latest.txt`, and `cmd/bench-report`
  diffs two files at a 10% threshold. What was missing is a *consumer* — nothing
  ever read the object back, so the baseline was write-only. Closed by fetching
  the previous baseline before the bench step and reporting the delta to the job
  summary. Deliberately NOT a gate: these binaries share a self-hosted runner
  with FDB testcontainers at `benchtime=1s`, and `bench-report`'s own note puts
  single-iteration CI variance at 10-30% — a threshold gate on that is a flake
  generator. The *deterministic* half of benchmark health is gated instead: the
  two CPU-only planner bench binaries (cascades, expressions) now turn `bench-ci`
  red on a non-zero exit, so the assertions above are acted on. The FDB-backed
  bench binaries stay report-only for the load-flake reason already documented.
  Timeouts on the two newly-gating binaries were raised 60s→300s so a busy
  runner cannot trip the gate on time rather than on failure — measured at
  `benchtime=1s` on an idle box, cascades takes 42s and expressions 11s, so the
  old 60s cap gave cascades under a 1.5x margin.
  The gate was verified to actually fire, not merely to exist: a deliberately
  inverted assertion in `BenchmarkPlanner_FullPlan` made the binary exit 1
  (`--- FAIL: BenchmarkPlanner_FullPlan`). The gating tail reads its verdict
  back out of `bench-raw.txt` by grepping the `!!! <binary> bench binary exited`
  sentinel it already writes. A first version instead carried a marker file,
  which was self-inflicted: `{ ...; } | tee` forks a subshell so a variable
  would not survive it. Grepping the existing sentinel needs no side channel at
  all, and unlike the other obvious fix (`{ ...; } > bench-raw.txt`, no
  subshell) it keeps `tee`, so the ~10-minute step still streams to the live CI
  log instead of going silent until it finishes. Exercised in isolation for both
  outcomes, including the one that matters most — an FDB bench binary failing
  while the planner ones pass must NOT gate (exit 0); a planner binary failing
  must (exit 1). A full `just bench-ci` exits 0 with 128 results, 0 failures.
  **Cascades never race-tested in PR CI — held.** Stated precisely: the
  relational scope already links and drives the planner, so planner code had
  INDIRECT race coverage; what was missing is the cascades package's own test
  binary under the detector, which is the only way to reach
  `TestRuleRegistry_Concurrent`'s goroutines. Added
  `//pkg/recordlayer/query/plan/cascades/...` as a third scope on the PR race
  job, carrying the file's per-scope no-op guard (0 resolved targets fails
  loudly) and the shared `_race_output_base`; it runs first, being the cheap
  CPU-only scope, so a planner race reports in seconds instead of behind two
  testcontainer suites. `just race-all` gained the same wildcard.
  It does NOT, however, "make local and CI agree" — an earlier version of this
  entry and of the recipe comment claimed that, and it was wrong. **Three race
  sets exist and are not meant to be equal**, now documented as such at
  `race-all`: the PR gate races relational + client + transport + fdb +
  cascades; nightly-coverage races client + fdb + recordlayer + chaos +
  conformance; `race-all` mirrors *nightly-coverage* (which it always has) plus
  cascades. The consequence is stated plainly in the recipe rather than papered
  over: `just race-all` does not race `relational/...`, so a green local run
  does not predict a green PR race job. `just verify`'s "(5 targets)" label was
  stale before this change and unfixable after it (the wildcard has no fixed
  count), so it no longer claims a number. **Added wall-clock, measured on a 24-thread box across all three
  regimes: 112s on a cold race cache (full instrumented rebuild — a one-time
  cost, since the base is shared with nightly-coverage and stays warm), 44s on a
  PR that edits the cascades package itself (recompiles that binary), and 22s
  fully warm.** That is cheap relative to the two testcontainer scopes already
  in this job, which is exactly why cascades qualifies where bench and
  conformance do not. 7/7 targets pass; `TestRuleRegistry_Concurrent` confirmed
  executing under `-race` (`--- PASS`, run explicitly by name). **No race was
  found — neither in that test nor anywhere in the seven targets.**
  **Stress scales misreported — held, with one correction.** The unconditional
  `t.Skip` in `TestFDB_Stress_10M` was real, and the sweep found a second
  identical one in `TestFDB_Ingest_10M`. Both are now in `stress_10m_test.go`
  behind `//go:build stress && realcluster`, so they are *registered only where
  they can run* rather than advertised-and-skipped; under the plain `stress` tag
  the package is exactly 10K/100K/1M and runs all three. Deleting them was
  rejected — the scale is legitimate on a real cluster and the reason is
  environmental, not a hidden bug. To make "run against a real cluster" an
  instruction rather than an aspiration, `TestMain` now honours
  `FDB_STRESS_CLUSTER_FILE` (previously it always started a container, so the
  10M tests could not have run anywhere); an unreadable value fails loudly rather
  than falling back to Docker. Because `realcluster` is not in `.bazelrc`'s tag
  list, gazelle leaves the file out of the `go_test` srcs and nothing in the
  Bazel build compiles it, so nightly-stress gained a `go vet -tags 'stress
  realcluster'` type-check — same pattern nightly-libfdbc uses for its
  Bazel-invisible tagged backend. That step carries its own no-op guard, because
  a vet over a package the file has dropped out of exits 0 having checked
  nothing — exactly the rot it exists to catch, wearing a green check. It
  asserts via `go list` that `stress_10m_test.go` is in the `stress realcluster`
  build before vetting, verified against both ways it can fail: renaming the tag
  and moving the file each produced the error and exit 1; restoring gave exit 0.
  The env guard READS the cluster file rather than `os.Stat`-ing it. Stat
  answers "does this exist", which is the wrong question, and it waved through
  three distinct bad inputs — all three confirmed: a mode-000 file, an empty
  file, and a directory, each of which would otherwise have failed unlabelled
  deep inside connection setup.
  The correction: **"nightly-stress runs 1M only" is false.** Its `bazelisk test`
  passes no `--test.run`, so it executes the whole target — 10K, 100K, 1M, the
  ingest-parallelism matrix and the SaveRecord comparisons. Only the job *name*
  and comments said "1M"; those are now accurate, and the README carries an
  explicit scale/tag/where-it-runs table.
  **`t.Skip` sweep (`pkg/`, `conformance/`).** Two prime-directive violations
  found — both the 10M ones above, both fixed. Everything else is one of:
  Docker/FDB-availability (sanctioned); `f.Fuzz` corpus domain restriction
  (rejecting oversized or invalid-UTF-8 inputs, not deferring a failure);
  explicit env opt-in for dataset- or hardware-dependent workloads (SIFT,
  SPFresh soaks, `tc netem`/NET_ADMIN, the Java submodule corpus); or an
  inapplicable matrix cell. Two classes are worth naming as pre-existing risk
  rather than violations, filed as CQ-15 below: skips on a *nondeterministic
  runtime condition* (`multishard_test.go`'s "shard splits did not occur",
  `connmon_test.go`'s "no proxy connections available"), which silently drop
  coverage while staying green. `plandiff/go_runner_test.go`'s
  `t.Skipf("Go-engine feature gap: ...")` is the same shape — it is built to turn
  a future Go gap into a green skip — but it fires zero times today and sits
  squarely in CQ-14's scope, so it is left for that item rather than duplicated.
- [x] **CQ-15 (LOW) — condition-gated skips replaced by loud preconditions.**
  All three sites were verified before being touched. None is an
  environment-availability check in disguise, and none was observed to fire in
  any run made for this item.
  **`multishard_test.go` (both sites) — the condition is already forced, and the
  skip was unreachable.** `setupMultiShardEnvWithConfig` seeds 1MB (100 keys ×
  10KB) under `min_shard_bytes`/`max_shard_bytes` knobs and then polls
  `locateRange` with `g.Eventually(…).Should(BeNumerically(">", 1))` for 60s.
  `gomega.NewWithT(t)` installs `t.Fatalf` as the fail handler (gomega 1.39.1
  `internal/gomega.go:35-38`), so a range that never split already aborted the
  test *inside the helper* — the `if env.numShards <= 1 { t.Skip(…) }` after it
  could not be reached. Measured over ten consecutive runs: 51 shards ×9 and 48
  ×1 (default knobs), 18 ×9 and 19 ×1 (larger-shards knobs); the "~5 shards"
  the file claimed for the larger-shards regime was wrong and is now expressed
  as a ratio, not a count. Both sites now fail through `crossShardPrecondition`,
  a pure function pinned by `TestCrossShardPrecondition` (0, 1 → error; 2, 51 →
  nil). The loud path was checked by suppression rather than by inspection:
  forcing `env.numShards = 1` after setup gave `--- FAIL: TestMultiShard …
  cross-shard precondition: locateRange reported 1 shard(s); cross-shard
  coverage needs > 1`, and making the `Eventually` threshold unsatisfiable gave
  the setup-side failure carrying its new lazily-built diagnostic (`range "ms_"
  never split (min_shard_bytes=40000 max_shard_bytes=200000); last locateRange
  error: <nil>`) — that message is new, because the previous matcher reported
  only "Expected 0 to be > 1" and dropped the polling error entirely. Both
  mutations were reverted.
  **`connmon_test.go` — also not an availability check.** `openTestDB` returns
  only after `database.bootstrap` stored the topology (`database.go:609`), so
  the `info == nil` half cannot occur; the `len(GRVProxies) == 0` half is
  reachable only if a coordinator answers bootstrap with no proxies, and in that
  state the test's own `Transact` further down would fail anyway. Skipping there
  suppressed exactly the failure worth reporting. It is now a `t.Fatalf` through
  `proxyTopologyPrecondition`, whose three branches are pinned by
  `TestProxyTopologyPrecondition`; suppression (passing `nil`) produced `--- FAIL:
  TestConnectionMonitor_BytesReceived … precondition: GetDBInfo returned nil:
  bootstrap stored no cluster topology`. The `bytesReceived == 0` message now
  carries the pooled-connection count, since an empty pool is how that assertion
  would fail vacuously.
  **The wider `if !condition { return }` sweep** — the shape a `t.Skip` grep
  cannot match — found three more in the same package, two fixed here.
  `testMultiShard_GetRangeSplitPoints` gated its internal-shard-boundary
  assertion on `env.numShards > 1`, so the one assertion that distinguishes
  multi-shard assembly from a single-shard locate could vanish silently; it is
  now unconditional, which the entry precondition already guarantees.
  `testMultiShard_ConcurrentWritesDuringDD` swallowed a `locateRange` error into
  a "0 shards" log line; the error is now asserted. The third is reported, not
  fixed: `coordinator_test.go`'s `TestCoordinatorBootstrap` gates roughly a
  hundred lines of assertions (Go-write/C-read interop, `Transact`, MVCC
  conflict detection) behind `if len(dbInfo.CommitProxies) > 0 && cErr == nil`,
  and logs-without-asserting on `locate`, GRV and C-binding-open failures — the
  same defect, but reworking a smoke-shaped test is wider than this item.
  Flakiness: the five affected tests at `--runs_per_test=10 --local_test_jobs=1`
  passed 10/10 with zero skips.
- [x] **CQ-14 (LOW) — reconcile the advertised SQL surface with conformance.**
  Both halves of the item held. Three things it did not name also had to be
  fixed: a live corpus entry that read as IN-subquery support, the upstream doc
  that originated the README claim, and a wrong `NOT IN` migration remedy.
  **Measured state of `IN (SELECT ...)`: no form plans, in either engine.** Every
  `x IN (SELECT …)` / `x NOT IN (SELECT …)` shape in the corpus is a rejection pin
  — `subquery_in` (8), `in_subquery_decomposition` (9), plus
  `correlated_subquery_probes`, `in_list_pushdown` and the `sqldriver`
  IN-subquery/DML/JOIN-ON probes — all `0AF00`, across SELECT WHERE, DML WHERE, a
  JOIN ON conjunct and a projection, correlated and uncorrelated, filtered and
  empty. The two `recursive_cte` occurrences return `0A000` only because the
  every-CTE-must-self-reference rule fires first, and `update_dml_cte`'s two
  `42601`s are grammar rejections of the `WITH …UPDATE` form and of a `WITH`
  nested inside the `IN (…)` parens — neither reaches the IN predicate. So the
  README's supported-list claim pointed users at a guaranteed error.
  **One live artifact contradicted that sweep and is now gone.** `plandiff`'s
  `SeedCorpus` carried `select_with_in_subquery`, and
  `TestSeedCorpus_GoEngineSucceedsOnAll` — which calls each entry a "regression
  sentinel" — was green on it, so the corpus read as "Go plans IN-subqueries".
  Probed directly through `NewGoEngine().Plan`, it does succeed:
  `Project(ID) / Filter(customer IN (SELECT id FROM users WHERE active = TRUE)) /
  Scan(ORDERS)`, and likewise on the `NOT IN` and catalog-aware paths. That is
  **not** evidence of support and does not weaken the README: `SeedCorpus` drives
  `NewExplainOnlyGenerator`, whose `LogicalFilter` carries `PredicateText` — the
  verbatim WHERE source text from `canonicalTextOf`, never a resolved predicate —
  so the tree is the input SQL echoed back, and the generator never executes. Left
  in place it would also have flipped to a spurious Go-only divergence the moment
  `SeedCorpus` runs against live Java via `NewJavaEngineHTTP`. The entry is
  deleted (no golden hash to update — `TestSeedCorpus_BaselineHash` was retired)
  and a comment at the site records why a text echo is not support and why it must
  not come back.
  **Java rejects it too — this is a shared gap, not a divergence.**
  `ExpressionVisitor.visitInPredicate` asserts `inList().queryExpressionBody() == null`
  with `UNSUPPORTED_QUERY` ("IN predicate does not support nested SELECT"), and the
  earlier `AstNormalizer.visitInPredicate` NPEs on the same shape
  (`ParseHelpers.isConstant` dereferences a null `ExpressionsContext`; the grammar's
  `inList` rule does parse `'(' queryExpressionBody ')'`). `DIVERGENCES.md` correctly
  carries no entry — there is no divergence to record — and
  `SQL_ANSI_CONFORMANCE.md` already scored E061-11 as a shared gap. Three stale
  statements asserting the opposite were corrected: the `plandiff` corpus NOTE
  claiming "Go's embedded engine implements it correctly … aligning Go to reject
  would invalidate ~14 Go-side test files"; this file's own "(Java supports it)"
  parenthetical on the IN-subquery reach-gap item further down; and
  `TODO-production.md` P1.7, which is the **origin** of the README claim — its
  sweep counted `IN (SELECT)` among the subquery forms it "verified against the
  yamsql corpus" as implemented and framed the gap as DML-only.
  **README made self-consistent.** The supported-SQL bullet no longer claims the
  form; the gap entry was widened from "in DML WHERE" (which implied the SELECT
  side worked) to every position, with the SQLSTATE, the Java citation and the
  `EXISTS` rewrite. `x IN (a, b, c)` value lists are called out as unaffected.
  The `NOT IN` caveat was **wrong** and is fixed: adding `IS NOT NULL` inside a
  `NOT EXISTS` is a no-op, because a NULL row already fails the equality —
  emulating `NOT IN` needs `x IS NOT NULL AND NOT EXISTS (… t.y = x) AND NOT
  EXISTS (… t.y IS NULL)`. The same wrong remedy was in `subquery_in.yaml` and
  `dml_subquery.yaml`; both fixed, and `subquery_in.yaml` now *pins* the
  emulation (empty result over a NULL-bearing inner, matching true `NOT IN`,
  plus a control restricted to the non-NULL row that returns `[1, 3]` — so the
  NULL-probe leg is proven load-bearing rather than incidentally empty).
  Pinned by a new `pkg/docscheck` guard that splits the README's supported list
  from its gap list and checks the **claim**, not a literal string: a supported
  block may name the feature only if the same block explicitly negates it. Mutation
  results — restoring the old wording FAILS, deleting the gap entry FAILS,
  rewording to "EXISTS / NOT EXISTS, IN-subqueries, and correlated scalar
  subqueries" FAILS (an earlier substring-only version passed this), and a correct
  negation passes (the earlier version false-positived on it).
  **FEATURE_MATRIX.md now splits cases by outcome.** It described itself as the
  "authoritative, exhaustive inventory" while counting a `0AF00` rejection
  identically to a working query (`Subqueries (EXISTS / IN / scalar) | 44 | 299`
  read as 299 cases of working subquery support). Both tables gained Supported /
  Unsupported / Error-path columns, per feature area and per scenario, plus a
  `**Total**` row and header prose saying what is being counted. That area now
  reads `44 | 301 | 245 | 36 | 20`, `in_subquery_decomposition` reads
  `11 | 2 | 9 | 0` and `subquery_in` reads `13 | 5 | 8 | 0` — the rejection pins
  are visibly separate from the `EXISTS`/join rewrites. Corpus-wide: 2370
  supported / 111 unsupported-feature / 230 error-path of 2711 (the corpus grew
  by the two `NOT IN` emulation cases above). No second
  classifier and no duplicated fold: both generators call one
  `classifyTests([]Test) CoverageBucket` in `coverage.go`, and `FeatureScenario`
  no longer carries a `Tests int` alongside `Outcomes` (it was equal to
  `Outcomes.Total()` by construction, so the conservation check it needed was a
  field compared against itself). What remains asserted is that the two
  generators inventory the same corpus — totals and scenario count must match
  `ParseCoverage`'s.
  **`plandiff/go_runner_test.go`'s feature-gap skip: removed as inert, replaced by
  a real gate.** The subtest body was `RunWithSetup` followed only by the
  `isGoFeatureGap` check, so it already passed for *any* error — the skip could
  never change pass/fail, only the reported status; it was cosmetic either way,
  independent of whether it fires. It is now a `t.Fatalf`: an ordinary query error
  stays tolerated (the corpus carries negative entries), but a Go-engine *type*
  gap ("unsupported column type" / "unsupported DataType code") fails loudly. No
  `SeedRunCorpus` entry hits it today, so the suite stays green.
  **Plan-shape golden re-blessed.** `explaindiff` plans the yamsql corpus
  positionally, so the two added `subquery_in` cases drifted it (reported as 1357
  lines, which is the line-shift artifact of a 38-line insertion). Re-blessed via
  `-update-golden`; the golden diff is a pure insertion — the two new plans plus
  the header count `2454 → 2456` — with **no** existing plan shape changed.

- [x] **CQ-16 (MED) — stop EXPLAIN rendering a plan the engine cannot execute.**
  `computeExplainText` swallowed a non-cancellation Cascades failure in its
  SELECT branch and fell through to `buildLogicalPlanForQueryWithCatalog` /
  `buildLogicalPlanForQuery`, so `SELECT q` raised `0AF00` while `EXPLAIN q`
  returned a tidy logical plan tree. Its own doc comment advertised the degrade.
  **Java has no such path — re-verified against 4.12.11.0.** EXPLAIN is not a
  statement type there: `ParseTreeInfoImpl.QueryTypeVisitor.visitFullDescribe
  Statement` (`fdb-relational-core/.../query/ParseTreeInfoImpl.java:133-136`)
  re-roots the tree at the inner statement and only tags `DESCRIBE_QUERY`, so
  planning is byte-for-byte the plain-statement path; the flag is read at
  execution (`QueryPlan.java:268-269` → `executeExplain`, `:319-414`). The PLAN
  text comes from `ExplainPlanVisitor.toStringForExternalExplain`, which takes a
  `RecordQueryPlan` — physical by type signature. When Cascades fails,
  `UnableToPlanException` (`CascadesPlanner.java:407`) is rethrown by the one
  `catch (RecordCoreException)` on the planning path (`QueryPlan.java:645-653`),
  mapped to `UNSUPPORTED_QUERY` at `ExceptionUtil.java:79-80` and surfaced as
  SQLSTATE `0AF00` — before `executeExplain` is ever reached, because
  `PhysicalQueryPlan` is never constructed. There are **zero** catches of
  `UnableToPlanException` in the tree, and the one logical-text hook that exists
  (`LogicalQueryPlan.explain()`, `QueryPlan.java:689-695`, returns the literal
  `"Logical Query Plan"`) is reachable only from logging, never from the EXPLAIN
  result set. `QueryLoggingTest.java:371-381` pins the throw.
  **The split.** The SELECT branch now mirrors `planSelect`'s routing
  one-for-one, error returns included. Three arms, each annotated at its site:
  (a) **no FDB session** → `planSelect` routes to `planSelectExplainOnly`, whose
  plan renders this same logical text and refuses to execute — there is no
  physical plan being hidden, so EXPLAIN agreeing with it is accurate. Kept.
  (b) **INFORMATION_SCHEMA** → a Go-only extension served off the catalog by
  `execSystemTableQuery`, never by Cascades; `planSelect`'s `PlanFunc` runs the
  same rendering as its own `Explain`, so EXPLAIN reports the plan
  that really runs. Kept. (c) **everything else** → Cascades IS the plan;
  `ensureMetaData` failure, nil metadata and `planSelectCascades` failure are
  all returned verbatim, because each is the error `SELECT q` itself raises.
  The shared logical-text rendering moved to one `explainLogicalQuery` helper.
  Cancellation handling is untouched (it already returned rather than degrading).
  **Blast radius: measured, not estimated.** The degrade path was instrumented
  at HEAD and `//pkg/relational/... //cmd/...` was run against real FDB (25
  targets, all green). Exactly **one** EXPLAIN in the entire suite took it:
  `TestFDB_FourWayFlatteningEvasionStaysNameModel/translation_keeps_cross_
  derived_predicate`, on the flattening-evasion shape, with
  `err=0AF00: Cascades planner could not plan query` — i.e. case (c), the
  violation. Zero hits on the no-metadata and not-attempted variants, and zero
  across the yamsql corpus (no `plan_contains` case is unplannable) and the
  `cmd/frl` `\explain` integration test. That one subtest **was pinning the
  defect**: its sibling `comma_form_fails_cleanly` asserted the query is 0AF00
  while it asserted EXPLAIN of the *same* query prints `Filter(t1.aid =
  t2.cid)`. It is now `explain_form_fails_cleanly` (same 0AF00 as the
  statement). The predicate-survives-translation property it was really after is
  real and is **not** dropped — it moved to
  `TestExplainOnlyMode_KeepsCrossDerivedPredicate`, the layer where logical text
  is the honest answer. `assertUnsupported`'s diagnostic was fixed alongside: it
  scanned two int columns unconditionally and printed `NULL|NULL` for an EXPLAIN
  row; it now renders any arity.
  **Pinned both directions.** Loud: `TestFDB_ExplainUnplannableQueryFailsLoudly`
  (three unplannable shapes — comma/CTE over two join-bodied derived tables and
  the solo derived join — each asserting the statement AND `EXPLAIN` of it are
  the same 0AF00). Verified red→green: with the fix reverted all three subtests
  fail with "expected clean rejection, but got a row back". Quiet:
  `plannable_still_renders_physical_plan` (same test — a plannable query still
  yields physical plan text), `TestFDB_ExplainInformationSchemaStillRenders`
  (the query executes, so EXPLAIN owes a plan — and this is the **first**
  coverage of the `referencesInformationSchema` guard on the EXPLAIN path; the
  census confirms nothing exercised it before), plus
  `TestExplainOnlyMode_StillRendersLogicalText`,
  `TestExplainOnlyModeWithSchema_StillRendersLogicalText` and
  `TestExplainDML_StillRendersLogicalText` in `core/embedded`.
  **Not fixed here, recorded: `EXPLAIN <DML>` renders logical text in Go, a
  physical plan in Java.** `describeObjectClause` admits
  `insertStatement|updateStatement|deleteStatement`
  (`RelationalParser.g4:738-744`) and they take the same
  `LogicalQueryPlan.optimize` path; `PhysicalQueryPlan.isUpdatePlan()` returns
  false under `isForExplain` (`QueryPlan.java:228-238`) so the mutation is not
  executed and the EXPLAIN row is returned instead. Java's corpus shows the
  rendered shape, e.g. `update-delete-returning.yamsql:44`
  (`SCAN(...) | DISTINCT BY PK | UPDATE A | MAP (...)`). Go's DML branches call
  `buildLogicalPlanFor{Delete,Insert,Update}WithCatalog` instead. Unlike the
  SELECT defect this is not a plan-the-engine-cannot-run (the DML does execute,
  through Cascades) — it is EXPLAIN describing a different tree than the one
  that runs. Separate item; routing it through `planDML` changes the plan text
  of every EXPLAIN-DML test and is out of CQ-16's scope. `README.md` and
  `DIVERGENCES.md` were checked and document neither behaviour, so neither
  needed an edit.
  **Review lap — four findings, all landed, all measured.** (1) A test comment
  claimed the catalog-vs-text builder swap was detectable when the assertion
  could not detect it: `Contains("Scan(T)")` + `Contains("Filter(")` pass on
  BOTH renderings (`"Filter(id > 5)\n  Scan(T)"` vs
  `"Filter(ID#0 > 5)\n  Scan(T)"`, measured). Now an exact-equality assertion on
  `ID#0`, the one token that separates them; verified red by disabling
  `buildLogicalPlanForQueryWithCatalog` in `explainLogicalQuery`. Ironic
  placement — an overstated comment inside the fix for overstated comments.
  (2) The relocated cross-derived-predicate test rode `NewExplainOnlyGenerator`
  (nil metadata → **text** builder), while the FDB subtest it replaced ran with
  metadata cached by its sibling and therefore exercised the **catalog**
  builder. Now `NewExplainOnlyGeneratorWithSchema` with the same four tables, so
  the pinned path matches the deleted one. The predicate survives on both
  builders, so no property was lost either way — the fix is about pinning the
  right path. (3) `computeExplainText`'s header asserted the never-renders-an-
  unrunnable-plan invariant for the whole function, but the DML arms do not hold
  it; the header now scopes the invariant to the SELECT branch and states the
  DML exception up front. (4) `explainLogicalQuery` returned `("", nil)` where
  `planSelect`'s last resort is `explainStatement("SELECT", sel)`, so when both
  logical builders decline, EXPLAIN raised `0A000 "produced no plan text"` while
  the query's own plan rendered the statement echo — the same defect class,
  pointed the other way. Reachable, not theoretical: `buildLogicalPlanForUnion`
  bails when `ALL()` is nil, so any non-`ALL` `UNION` hits it. Now mirrors, using
  the Query node (the grammar is `selectStatement : query`, a single child, so
  the two `GetText()` renderings are identical). Pinned as a property rather
  than a comment by `TestExplainMirrorsThePlansOwnExplain`: for the catalog,
  text and last-resort arms, `EXPLAIN q` must equal `Plan(q).Explain()`.
  Verified red on the old `("", nil)` — `union_distinct_last_resort` fails with
  exactly `0A000: EXPLAIN inner statement produced no plan text`.
- [x] **CQ-17 (MED) — `computeDistinctRecords` claimed every
  `RecordQueryMergeSortUnionPlan` was distinct, ignoring its own
  `RemovesDuplicates()` flag.** `plan_properties.go`'s `computeDistinctRecords`
  grouped `*plans.RecordQueryMergeSortUnionPlan` with the genuinely-always-
  distinct plans (`RecordQueryDistinctPlan`, `RecordQueryIntersectionPlan`, …)
  and returned `true` unconditionally, never reading
  `RemovesDuplicates()`/`removeDuplicates` — but the plan's own doc comment
  (`merge_sort_union.go:18`) and its executor (`executeMergeSortUnion` /
  `mergeSortCursor`, `executor_new_plans.go:1203`) both support
  `removeDuplicates=false` as an ordered UNION ALL, where tied rows from
  non-leading children legitimately re-emit. **Verified against Java
  4.12.11.0 first:** Java's counterpart
  (`RecordQueryUnionPlanBase`/`RecordQueryUnionPlan`, no `removeDuplicates`
  field at all — `UnionCursor` always dedups) has no non-dedup mode, so
  `DistinctRecordsProperty.visitUnionOnValuesPlan`/`visitUnionOnKeyExpressionPlan`
  (`DistinctRecordsProperty.java:312-314,366-368`) correctly return `true`
  unconditionally — confirming the Go doc comment's claim that
  `removeDuplicates=false` is a genuine Go-only extension, not a stale
  divergence. Fixed by gating the arm on `p.RemovesDuplicates()` instead of
  the blanket `true`. **Currently latent, not live:** every production
  constructor (`rule_implement_distinct_union.go:269`,
  `rule_push_set_operation_through_fetch.go:142-148`) passes `true` or
  forwards an existing plan's flag — nothing mints `removeDuplicates=false`
  today — so no plan shape moved; confirmed via
  `pkg/relational/conformance/explaindiff`'s golden byte-identical (untouched
  by `git status`) and the full `.../conformance/...` suite green. Had it gone
  live, `ImplementDistinctFinalRule` (`rule_implement_distinct_final.go:91`,
  trusting `partition.IsDistinct()` sourced from this property) would have
  elided a needed dedup wrapper and let duplicate rows through a `SELECT
  DISTINCT`. Verified RED: `TestComputeDistinctRecords_MergeSortUnionRemoveDuplicatesFalseIsFalse`
  (`plan_properties_test.go`) fails on the pre-fix code
  (`removeDuplicates=false` reported distinct) and passes after.
  **No structural invariant added, deliberately** — unlike CQ-10g's InJoin
  `sorted`/`GetInValues()` pair (two independently-settable pieces of DATA on
  the SAME plan instance that a buggy call site can desync), `removeDuplicates`
  is the SOLE source of truth for this property and the fix now reads it
  directly: there is no second piece of per-instance data that could
  legitimately drift out of sync from it. A regression back to unconditional
  `true` is a code bug (caught by the unit test above), not a data-consistency
  bug a per-instance runtime walk over `ValidatePlanInvariants` could catch.
  **Sweep of the rest of `computeDistinctRecords`'s switch:** checked every
  other plan type with a mode/direction-toggling field
  (`RecordQueryIntersectionPlan.reverse`, `RecordQueryInUnionPlan.reverse`/
  `maxSize`, `RecordQueryMultiIntersectionOnValuesPlan`'s fields,
  `RecordQueryDistinctPlan.Streaming`) against what the property claims and
  what the executor does. None are dedup-mode toggles: `reverse` is scan
  direction only (orthogonal to distinctness — Java's Intersection/InUnion
  visitors also return `true` unconditionally regardless of direction),
  `RecordQueryInUnionPlan.maxSize` is the unrelated fanout-cap gap tracked
  below as CQ-18, and `RecordQueryDistinctPlan.Streaming` picks an executor
  strategy (adjacent-dedup vs hash-set) — both modes dedup, so it does not
  affect distinctness. `RecordQueryMergeSortUnionPlan` was the only plan in
  the switch whose ignored field actually changed whether duplicates survive.
- [x] **CQ-24 (HIGH) — `compareJoinOrdering` is not a total preorder, so the
  elected plan depends on member insertion order.** `planning_cost_model.go`
  picks its metric from `joinShapesDiffer(planA, planB)` — a property of the
  PAIR, not of either plan: shapes differ → raw `Cost.CPU`, shapes same →
  `Cost.Less`. That is the same anti-pattern removed from `compareInPlan` and
  `comparePrimaryScanVsIndexScan`, left in place here.

  REPRODUCER (runs green against the defect today; inverted copy that asserts
  transitivity is parked at `scratchpad/join_ordering_cycle_reproducer_INVERTED.go.keep`
  and FAILS on current code):

      costA (FlatMap) CPU=36.5715 Total=36.9765
      costB (NLJ)     CPU=24.9885 Total=105.1785
      costC (FlatMap) CPU=15.795  Total=56.295
      compare(A,C)=-1   compare(A,B)=1   compare(B,C)=1   -> 3-cycle

  `Reference.GetBest` (`expressions/reference.go:300`) folds pairwise and
  elects a different winner per rotation: `[A,B,C]→C`, `[B,C,A]→A`,
  `[C,A,B]→B`. Its own doc comment states the precondition it violates: "the
  comparator must be a total order on Cost". A permutation test written
  alongside the reproducer found a SECOND independent cycle
  (`flatMap(1,1000)`, `flatMap(100,1)`, `nlj(1,220)`), so the hand-built triple
  is not the only one. No live SQL query is known to flip, because member
  insertion order is not SQL-controllable — which is the hazard, not the
  reassurance: an unrelated rule reordering silently changes plans for
  identical SQL.

  THE OBVIOUS FIX IS MEASURABLY WRONG — do not repeat it. Unifying the two
  cardinality formulas (`FlatMapCost.Cardinality = outerCard*sel` ignores
  inner cardinality; `NestedLoopJoinCost` uses the cross-product) and deleting
  the pair-dependent branch DOES kill both cycles, but it is unsound: the two
  shapes are not costed from the same inputs. `RewriteOuterJoinRule` pushes the
  join predicate INTO the FlatMap's inner subtree as a `PredicatesFilter`, so
  that selectivity is already baked into `inner.Cardinality`, while
  `NestedLoopJoinCost` receives raw inputs and applies `FilterSelectivity`
  itself. Unified, FlatMap applies selectivity TWICE and is systematically
  undercosted in exactly the non-probe case. Measured consequences of that
  attempt: 5 failing targets / 10 top-level tests, including the exact
  RFC-152/153 preserved-only regression those RFCs closed, an executor failure
  (`TestFDB_ExistsInnerShadow`), a join-order regression that stops
  index-probing a 2000-row table (`TestFDB_MultiwayJoinOrder_Nway`), 36/2633
  corpus plans flipped (32 NLJ→FlatMap, 3 losing an `InMemorySort`), and on
  real yamsql statistics `flatmap_secondary_index#3` turning two ordered index
  scans with no sort into a full scan plus a materialized sort. Hand-computed
  RFC-152 `A=1e6,B=1e6`: post-change FlatMap wins at Total 2.624e11 vs NLJ
  4.374e11 while doing strictly more work.

  Also measured, refuting the "cheap fix" hypothesis: only 182/375 (48.5%) of
  FlatMap occurrences in the plan-shape golden are probe-shaped (inner root
  scan all-equality-bound, where the formulas already agree). 51.5% are not —
  187 `PredicatesFilter`, 6 `DefaultOnEmpty`.

  So the real fix must make the cardinality INPUTS comparable, not point one
  formula at both: either `NestedLoopJoinCost` stops applying its own
  `FilterSelectivity` when the predicate is already reflected in a child's
  cost, or `FlatMapCost` detects and does not double-count a predicate the
  rewrite rule baked into its inner. **RFC-192 is written and carries all of
  the above plus three options and the gating conditions; it requests a Graefe
  ruling on which option — including the prior question of whether the Go-only
  materialized NLJ earns a cost model that must compare two shapes with
  structurally different cost derivations.** That is design work behind the
  query-engine gate — needs the RFC ACKed before implementation,
  plus a corpus re-bless and a stress comparison. Java sidesteps this entirely:
  it has no materialized NLJ plan at all and gates the criterion on both sides
  being `RecordQueryFlatMapPlan` (`PlanningCostModel.java:277`), so the whole
  NLJ-vs-FlatMap comparison is Go-only.

- [x] **CQ-23 (LOW, found alongside CQ-24) — `compareRecursiveCTE` violates
  indifference-transitivity.** `planning_cost_model.go:775-786`:
  `compare(DFS, Level) = -1`, but `compare(DFS, Unclassified) = 0` and
  `compare(Level, Unclassified) = 0` — DFS ~ Unclassified ~ Level while
  DFS < Level strictly. Ties must be transitive in a total preorder. It is
  called unconditionally on every pair
  (`planning_cost_model.go:257`), so "Unclassified" is the overwhelmingly
  common case (any non-recursive-CTE plan), and `recursiveCTEKind`
  (`:798`) classifies only through single-child pass-throughs — so a DFS or
  Level plan buried under a multi-child node (a `UNION` combining the CTE with
  something else) itself misclassifies as Unclassified. Whether this yields a
  full-comparator cycle depends on later criteria also tying those pairs;
  not yet constructed. Fold into the CQ-24 RFC — same file, same class, and a
  comparator-wide total-preorder property test should cover both.

- [x] **CQ-25 — `TestFDB_MultiwayJoinOrder_Nway` now elects the forward
  (index-probe) drive order for the physically correct reason: three
  compounding cost-model incoherences, each a "the same execution priced
  two different ways depending on which node performed it" defect, found
  and fixed in sequence.**

  **Fix 1 — the cap's condition wasn't reaching real plans.**
  `fk_chain_cardinality.go`'s cap ("a FlatMap chain's output can't exceed the
  probed table's own size") was already keyed on the right thing — the
  OUTER's proven uniqueness (via chained `pkThread`), not the inner leg's
  index kind — but `innerFullyBindsThread` failed closed on any
  `RecordTypeValue` component of `outerThread.pkValues`.
  `TranslatePrimaryKeyToValues` (`primary_key_translation.go`) stamps one
  whenever a table's declared PK compiles to `Concat(RecordTypeKey(),
  Field(...))` — the normal shape for every table in a non-intermingled
  multi-type SQL schema. A chain's first hop roots its outer thread at a
  plain `RecordQueryScanPlan` (`GetPrimaryKeyValues`, no prefix), so hop 1
  was never affected; every hop after that roots at the PRECEDING hop's
  inner leg — an `IndexPlan`, whose `GetCommonPrimaryKeyValues` DOES carry
  the prefix — so the cap could fire on hop 1 only, never hop 2+. Fixed by
  skipping `RecordTypeValue` components when building `wantFields`: within
  one `pkThread` every row already shares one record type, so that
  component is a per-thread CONSTANT, never a discriminating column.
  Regression: `TestFKChainCardinalityCap_PropagatesAcrossRecordTypeKeyPrefixedPK`
  (mutation-checked). Chain cardinality: **2000** (true value), was the
  impossible **8000**.

  **Fix 2 — the cap clamped Cardinality but left CPU inconsistent.**
  `combineConcreteCost`'s FlatMap case only overwrote `cost.Cardinality`
  when the cap fired, never `cost.CPU` — crediting a hop with producing at
  most `cap` rows while still charging it CPU for the larger, disproven
  uncapped row count. Fixed with `fkChainCappedInnerCost`
  (`fk_chain_cardinality.go`): when the cap binds, it derives a corrected
  inner `Cost` — `Cardinality: cap/outerCard`, `CPU` scaled by the SAME
  ratio — since `scanLikeCost` and every wrapper the cap sees through
  (Fetch/TypeFilter/Filter) are exactly linear, zero-intercept functions of
  Cardinality, so the identical ratio that corrects Cardinality also
  correctly corrects CPU; not a picked constant. Property-tested
  (`TestFKChainCappedInnerCost_DerivationProperty`), pinned end-to-end
  (`TestFKChainCardinalityCap_CPUConsistentAcrossChain`, mutation-checked),
  confirmed inert when the cap declines
  (`TestFKChainCardinalityCap_CPUUnaffectedWhenCapDoesNotFire`). Forward's
  CPU: 9146.29 → 2388.46 (~74% down) — still lost to backward's 807.58.

  **Fix 3 — a full-PK/unique-index point-probe was priced at the
  AMORTIZED sequential-scan rate (`ScanCPU`), not the isolated
  random-access rate (`FetchCPU`), even though the executor proves it is
  the SAME physical operation as a Fetch.** Verified against the executor
  before changing anything (per instruction: prove the physical claim, do
  not assume it): `key_value_cursor.go`'s `initIterator` issues its OWN
  `tx.GetRange` per invocation; `flat_map_cursor.go`'s `OnNext` opens a
  FRESH inner cursor per outer row via `ExecutePlan` with NO pipelining
  across rows (its own comment: "Go simplification: no async pipelining");
  `split_helper.go`'s `loadWithSplit` (the Fetch path) does one blocking
  `tx.Get(unsplitKey).Get()` in the common case — the SAME
  isolated-single-round-trip shape as the point-probe's `GetRange`, not the
  amortized-over-many-rows shape `ScanCPU` is calibrated for. Fixed at
  every site with this exact shape (same defect class, same pass, per
  instruction #3): `scanLikeCost`'s `fullBindUnique` branch
  (`planning_cost_model.go`), `RecordQueryScanPlan.HintCost`'s point-lookup
  branch and `RecordQueryIndexPlan.HintCost`'s unique point-lookup branch
  (`plans/cost.go` — the index one was charging literally **0**, an even
  starker version of the same defect), and `RecordQueryInJoinPlan.HintCost`
  (each IN value's index-probe term was `ScanCPU`, now `FetchCPU` — it pays
  the SAME two isolated round trips a Fetch-wrapped point-probe does).
  `properties.FetchCPU`/`ScanCPU`'s doc comments now state the general rule
  (isolated-random-access vs amortized-streaming) so future call sites can
  self-classify. Deliberately NOT touched: `BoundSelectivity` (a genuinely
  different, unrelated calibration axis) and every OTHER `ScanCPU` site that
  IS a real amortized multi-row streaming scan (general scanLikeCost
  branch, `FullUnorderedScanExpression`, aggregate/vector index scans —
  audited, left alone).

  Property-tested with a table of (cardinality) values, not one example
  (`TestScanLikeCost_PointProbeChargesFetchRate`,
  `TestPointProbeHintCost_ChargesFetchRateNotScanRate`,
  `TestInJoinHintCost_BothTermsAreFetchRate`), confirmed inert on the
  non-point-probe cases (`TestScanLikeCost_NonPointProbeCPUUnaffected`,
  `TestPointProbeHintCost_NonUniqueOrPartialBindUnaffected`), all
  mutation-checked (revert → red, restore → green, verbatim in the shift
  record).

  **Result: forward now wins on physically-justified numbers.** Forward
  card=2000 cpu=86.39 (synthetic) / real-FDB total ≈ matches; backward
  card=2000 cpu=7870.41 (synthetic) — backward's 6000 unbatched point
  probes, now correctly priced at the same isolated-round-trip rate as
  forward's 2220 index-probe+fetch operations, are the more expensive
  total. `TestFDB_MultiwayJoinOrder_Nway` passes (verified repeatedly, not
  a coincidence of one run).

  **Corpus impact (fix 3 alone, `cmd/explain-differ dump`, tree otherwise
  constant): 17 plans changed, in two categories, both verified —**
  (a) 5 FK-join drive-direction flips (`flatmap_secondary_index.yaml#0`,
  `join_index_correlation.yaml#0`, two department/employee variants, one
  customer/order variant) — small dimension table now drives, large table
  reached via its FK index; mechanism is fewer FlatMap re-executions
  (`IterationOverhead` scales with outer row count, now correctly the
  deciding factor once point-probes and fetches cost the same) — a genuine
  improvement, confirmed by running the full `flatmap_secondary_index` /
  `join_index_correlation` yamsql scenarios (all row assertions pass); (b)
  9 recursive-CTE/self-join cases gain an extra pass-through
  `RecordQueryProjectionPlan` that a projection-fusion rule doesn't
  eliminate on this specific memo alternative — cost-neutral-ish
  (`ProjectionCPU`=0.05/row, negligible on these tables), NOT a correctness
  issue (`recursive_cte` scenario re-run: 26/26 assertions pass; `cte`
  scenario's self-join `COUNT(*)` case doesn't expose individual columns
  at all). This is a pre-existing, separate projection-fusion completeness
  gap surfaced by the cost shift, not caused by it — worth a follow-up but
  not numbered here (too small to warrant its own item; note for whoever
  next touches `ProjectionMerge`-family rules). Golden left un-blessed for
  review, per instruction.

  **1M stress test** (`TestFDB_Stress_1M`, before/after): identical row
  counts and plan shapes throughout; `join_10_outer`'s
  `FlatMap(outer=Scan(ORDERS,[<>]), inner=Scan(CUSTOMERS,[=]))` (a PK
  point-probe inner) is UNCHANGED both ways — no cheaper alternative
  exists for that shape, so raising the point-probe rate didn't flip it;
  timings within noise (~16ms either way, real durations dominated by
  actual FDB I/O, not the planning-time cost model). No regression at
  scale.

  **Pre-existing finding, now root-caused and fixed:** `yamsql_test`'s
  `pk_pushdown` scenario (`SELECT id FROM t WHERE id > 2 ORDER BY id`
  expecting `TypeFilter([T], Scan(T, [<>]))`) failed identically with fix 3
  applied AND fully reverted (a RANGE bound, not a point-probe — none of
  fix 3's changed formulas even apply to it). Bisected (`git apply -R` per
  file, independently re-verified) to the SAME session's
  `primary_scan_match_candidate.go`/`cascades_generator.go` change: a
  record-type-key-prefixed primary scan (the default for every SQL table —
  `buildPrimaryKeyExpression` prepends `RecordTypeKey()` unless
  `INTERMINGLE_TABLES` is set) now correctly ELIMINATES the TypeFilter
  wrapper instead of keying elimination off `available==queried` record
  types, matching Java's `PrimaryScanMatchCandidate.hasAndOrderedByRecordTypeKey()`
  1:1 (`fdb-record-layer-core/.../PrimaryScanMatchCandidate.java:207-220`).
  Verified NOT a wrong-rows regression: `executeScan` (`executor.go:237-264`,
  pre-existing, untouched by this session) already clamps an unbounded scan
  endpoint to the record-type-key prefix range, so the scan physically
  cannot cross into another type's rows — the TypeFilter really was dead
  weight. This is case (a): the fix is correct, the scenario's pinned
  expectation was pinned to the pre-fix Go divergence. `pk_pushdown.yaml`
  updated (`Scan(T, [<>])` + `plan_not_contains: TypeFilter`, sort-elimination
  check split into its own entry since `plan_not_contains` is single-valued).
  New scenario `intermingle_type_filter.yaml` pins the DISCRIMINATING side
  (`INTERMINGLE_TABLES=true` — the filter is load-bearing there, and the
  test proves it with an actual WRONG-ROWS backstop: overlapping id ranges
  across two intermingled tables, `id > 2` unbounded on the high end). That
  surfaced its own gap: `WITH OPTIONS(...)` parsed but `execCreateSchemaTemplate`
  (`pkg/relational/core/embedded/ddl.go`) never read `s.OptionsClause()` —
  `ENABLE_LONG_ROWS`/`INTERMINGLE_TABLES`/`STORE_ROW_VERSIONS` were silently
  dropped from every SQL `CREATE SCHEMA TEMPLATE`. Fixed by porting Java's
  `DdlVisitor.visitCreateSchemaTemplateStatement` option-clause loop
  (1:1 — same three-way switch, same ordering before the table/index passes).
  Mutation-checked both fixes (revert → RED on `pk_pushdown`/
  `intermingle_type_filter` respectively → restore → GREEN, verbatim).
  `SQL_COVERAGE.md`/`FEATURE_MATRIX.md` regenerated for the new scenario.

- [ ] **CQ-18 (LOW) — `RecordQueryInUnionPlan.maxSize`/`GetMaxSize()` is
  stamped and preserved but never read by the executor.** Set at
  `rule_implement_in_union.go:337` from
  `PlannerConfiguration.AttemptFailedInJoinAsUnionMaxSize`, carried through
  rebuilds, but `executeInUnion` (`executor_new_plans.go:1084`) never calls
  `GetMaxSize()` — zero hits for `GetMaxSize` in the executor package. Java
  re-checks this cap at **runtime**
  (`RecordQueryInUnionPlan.java:151-154`, throws if actual fanout exceeds it)
  because Java's IN sources can resolve from live parameters. The knob
  defaults to 0 (disabled) with no production call site setting it today, so
  both engines are inert on this axis right now — a missing safety guard, not
  wrong rows. Found during the CQ-17 sweep; not implemented there (out of
  scope — recording only, per the sweep's mandate not to grow scope beyond
  the property bug it was chasing).
- [ ] **CQ-19 (LOW) — `RecordQueryVectorIndexPlan.IsReturningVectors()` is
  plumbed through but never read by the executor.** Set from
  `comparisons.go` through `vector_index_match_candidate.go:276` into the
  plan, but `executeVectorIndexScan` (`executor.go:404-536`) always does a
  full base-record fetch by PK regardless of the flag. Results are not
  wrong — a stored vector column is present in the base record either way —
  but the "read the vector straight from the index entry, skip the fetch"
  optimization the field requests is never delivered. No SQL surface
  constructs the triggering comparison today, so this is inert, not a live
  bug. Found during the CQ-17 sweep; not implemented there (out of scope —
  recording only).
- [x] **CQ-22 (executor, not planner/cost-model) — `mergeSortCursor.OnNext`
  selected the next row with an O(N)-per-emitted-row rescan of every leg;
  replaced with an O(log N) `container/heap` binary min-heap.** Investigated
  because a concurrent InJoin-vs-InUnion benchmark would otherwise be
  measuring InUnion with this overhead baked in. **Java does the same O(N)
  thing** — `UnionCursor.chooseStates`
  (`provider/foundationdb/cursors/UnionCursor.java:101-131`) is a per-row
  linear rescan of every leg, and `computeNextResultStates` (:74-98, the
  exhaustion/limit check) is the same shape; there is no heap or tournament
  tree in Java anywhere in this cursor family. This is NOT a Go-vs-Java
  divergence being fixed — Java's N is bounded by query shape (a two-cursor
  UNION, a handful of OR branches) and never needed better than O(N); Go's
  InUnion plan turns a SQL IN list into one leg per value, so N can reach the
  hundreds or thousands where the O(N) rescan dominates. Recorded as a
  Go-only extension in `DIVERGENCES.md` (clean-extensions list), not a parity
  fix. Implementation: `mergeSortHeap` orders by comparison key then by
  original leg index, reproducing `chooseStates`'s first-leg-wins tie rule
  exactly (`pkg/recordlayer/query/executor/executor_new_plans.go`); the
  exhaustion/limit scan is also made incremental (a leg's stop state can only
  change when it is re-pulled, so only legs pulled THIS round need checking).
  A first attempt eagerly re-pulled consumed legs before snapshotting the
  emitted row's continuation and tripped the "cannot return end continuation
  with next value" invariant — fixed by deferring the re-pull to the top of
  the FOLLOWING `OnNext` call (`pendingAdmit`), matching Java's actual timing
  (`consume()` clears the cached future; the next future is only resolved by
  the following round's `whenAll`). **Proof of behavior preservation**: the
  replaced linear scan is kept as a test-only oracle
  (`referenceMergeSortOnNext`, `merge_sort_heap_differential_test.go`) and
  compared row-by-row (value, stop reason, continuation bytes) against the
  heap over 500 randomized heavy-tie trials (1-12 legs, key range as narrow
  as 1-6 forcing dense cross- and within-leg ties) and 100 wide-key/many-leg
  trials (20-100 legs, key range 100k) — 0 divergences; resumption from every
  emitted continuation reproduces the exact remaining suffix for both a
  heap-resumed and reference-resumed cursor (150 trials); a differential fuzz
  target ran 621k executions / 51s with 0 failures. All pre-existing
  `mergeSortCursor` unit tests, `TestFDB_InUnionRowContinuation_BudgetSweep_Paged`,
  `TestFDB_InJoin_SortedClaimMatchesExecutionOrder`,
  `TestFDB_InOrCompoundSargProbe`, and the full yamsql/explaindiff corpus
  (`TestPlanShapeGolden`, `TestNoUnexpectedPlanFailures`,
  `TestCorpusPlanReachability`, etc.) pass unchanged against real FDB —
  explain/plandiff goldens are byte-identical (executor-only change, no plan
  shape reads merge cost). Intersection cursors (`multiIntersectionMergeCursor`
  → `recordlayer.IntersectionMultiResume`) are a separate cursor family and
  untouched by this change. **Measured improvement**: in-process CPU-only
  (`BenchmarkMergeSortCursor_HeapVsLinear`, 7 replicates/point, no I/O) —
  heap LOSES to the linear scan below N≈10 (heap bookkeeping exceeds the tiny
  linear cost: N=3, 2.29M vs 2.70M rows/sec), is a wash at N=10 (1.80M vs
  1.81M), and wins at N=100 (761k vs 363k, ~2.1x) and N=1000 (168k vs 43.4k,
  ~3.9x), all with sub-2% relative stddev. End-to-end against real FDB
  (`BenchmarkFDB_InUnionMergeSort_N*`, before = clean worktree of this
  commit's parent, after = this change, 5 replicates/point, `-benchtime=5x`):
  no measurable difference at N∈{3,10,100} (95% CIs overlap — FDB round-trip
  latency dominates at that scale), and a real ~11.7% rows/sec improvement at
  N=1000 with non-overlapping 95% CIs (before 8366 [8345,8388], after 9348
  [9287,9409] rows/sec, 5000 total rows). Not committed as part of any
  benchmark comparing InJoin vs InUnion — that comparison, wherever it lands,
  should now be against this non-handicapped InUnion. (Wording fix: N≈10
  loses to the linear scan below that point and breaks even just above it —
  not "a wash at N=10"; three replicates put N=10 consistently on the losing
  side.)

  **Milestone review (Graefe + Torvalds): first pass NAK, two mechanical
  blockers, algorithm ACK'd.** Tie-break stability (300 randomized
  staggered-tie trials), resumption (200 randomized trials), the fuzz target
  (799k execs at review time), byte-identical goldens, and Java's own O(N)
  shape were all accepted outright — "Clean extensions" framing stands.
  Both blockers fixed same-day:
  - **B1 — a leg pull error orphaned the rest of the admit batch.**
    `pullAndAdmit` used to take the whole pending batch as a captured local
    while the caller cleared `m.pendingAdmit` BEFORE calling it; a pull error
    on any leg but the last in that batch permanently dropped every leg after
    it — not in the heap, not in `pendingAdmit`, never pulled again. Depending
    on which leg failed and how many rounds followed, this either silently
    lost rows or, if every remaining leg in the batch ended up orphaned that
    way, panicked out of `stopWith` (`SourceExhausted` demands an all-END
    continuation snapshot; an orphaned leg's continuation was stuck mid-page).
    **Fixed** by making `pullAndAdmit` consume `m.pendingAdmit` as a queue,
    popping a leg off the front only AFTER its pull succeeds — a failing leg
    (and everything behind it) stays queued for the very next `OnNext` call to
    retry, and `mergeSortChildState.pull`'s existing idempotence (`s.pulled`
    only advances on success) means a leg that already succeeded is never
    re-pulled or double-pushed into the heap. New regression suite
    (`merge_sort_heap_error_test.go`, three cases: error on the first leg of
    the init batch, error on a middle leg of the init batch, error on a
    later round's re-admit) proves no row is dropped and no panic occurs;
    confirmed each test reds with the pre-fix `pullAndAdmit` restored
    (reproduces the exact `"SourceExhausted requires an end continuation"`
    panic) and greens with the fix. The differential harness in
    `merge_sort_heap_differential_test.go` cannot cover this axis at all —
    every leg there is a `recordlayer.FromList`, which only ever stops via
    `SourceExhausted`, never a hard `OnNext` error.
  - **B2 — `BUILD.bazel` was never regenerated, so none of the new evidence
    ran under Bazel/CI.** `just gazelle` now wires all three new test files
    (plus the new `merge_sort_heap_error_test.go` from the B1 fix) into
    `executor_test`'s `srcs`/`embedsrcs`
    (`pkg/recordlayer/query/executor/BUILD.bazel`) and the bench file into
    `pkg/relational/sqldriver/BUILD.bazel`; `bazel mod tidy` needed no
    changes (no new external deps). `FuzzMergeSortCursorHeapMatchesLinearScan`
    is now Bazel-discoverable (`-test.list='Fuzz.*'` lists it) and was run for
    20s / 1.04M executions with 0 failures. `pkg/docscheck`'s hygiene gate
    (`git ls-files`-driven) now covers all four files now that they're
    tracked.

  Folded non-blocking notes: the `pendingStop` field comment previously
  claimed the STOP CHECK was "deferred to the top of the next OnNext call" —
  reworded to say precisely what's deferred (the ADMIT, i.e. the pull of the
  legs consumed at the end of round R, pushed to the top of round R+1) versus
  what fires immediately once admitted (the `pendingStop` check, same call).
  `m.heapErr` used to latch permanently once set (a stray comparison error
  would poison every later, otherwise-unrelated heap operation into
  re-returning the same stale error); it is now read-and-cleared via a
  `takeHeapErr()` helper so it is a one-shot signal per mutation, matching
  the replaced scan's behavior of re-erroring deterministically on a genuine
  repeat rather than poisoning unrelated future calls. DIVERGENCES.md's
  headline now leads with the strongest form of the argument: the heap
  swap is ordering-neutral, so it cannot diverge from Java observably at
  all, plus the one-sentence cross-type-comparison-key caveat (N2: the heap
  compares leg pairs the linear scan never happened to compare, which can
  surface a pre-existing invariant violation earlier than the scan would
  have — not a new bug).

  **Full verify re-run post-fix**: `gofumpt -l pkg` clean; `just gazelle`
  wired the four files above; `bazelisk build //pkg/... //cmd/...
  //conformance/...` green (192 actions); `bazelisk test
  //pkg/recordlayer/query/executor:all --nocache_test_results` 1/1 green;
  `bazelisk test //pkg/relational/conformance/... //conformance/...
  --nocache_test_results` 6/6 green; `bazelisk test //pkg/docscheck:all
  --nocache_test_results` 1/1 green; fuzz target 1.04M execs / 0 failures
  under Bazel.

- [ ] **CQ-26 (LOW, found scaling the cardinality/cost bound oracle to random
  combos, 2026-07-26) — `TypeFilterCost` has no floor at the property's
  proven min, same class as the already-documented Distinct/
  StreamingAggregation-ungrouped/DefaultOnEmpty-over-zero-cost gaps.**
  `properties.TypeFilterCost` (cost_formulas.go) applies a flat
  `TypeFilterSelectivity=0.5` multiplier unconditionally; when the child
  structurally proves an exact positive floor (e.g. `RecordQueryValuesPlan`,
  which is `ExactlyOne`, min=1 max=1 — a literal row, not a "might not
  exist" point lookup), the resulting cost estimate (0.5) falls below the
  proven min (1), violating the standing invariant
  `pkg/recordlayer/query/plan/cascades/cardinality_cost_bound_test.go`
  checks. The existing fixed `typeFilter/overBoundedChild` shape cannot see
  this — its child (`pointLookupScan`) proves `AtMostOne` (min=0), and
  0.5 >= 0 never violates — so this was invisible until the random-combo
  generator (`TestCardinalityPropertyBoundsCostEstimate_RandomCombos`,
  scaled via `CARDINALITY_COMBOS`/`CARDINALITY_SEED`) composed
  `TypeFilter(Values(1))` directly. PINNED (not fixed) as
  `typeFilter/overExactlyOneChild` (`knownBelowMin`, self-cleaning) in the
  same file; the random generator's leaf pool deliberately excludes
  `RecordQueryValuesPlan` so it does not rediscover this SAME known gap on
  every run (documented at the leaf-pool site). Fixing `TypeFilterCost` to
  floor at the child's proven min (mirroring `FirstOrDefaultCost`'s
  unconditional floor) is a **cost-model change** and therefore needs the
  Graefe+Torvalds milestone-level ACK the query-engine skill requires before
  it lands — out of scope for the harness-scaling work that found it.
  Reproduce: `CARDINALITY_COMBOS=1000 CARDINALITY_SEED=1 bazelisk test
  //pkg/recordlayer/query/plan/cascades:cascades_test
  --test_arg="--test.run=TestCardinalityPropertyBoundsCostEstimate/typeFilter/overExactlyOneChild"
  --test_arg="-test.v"` (logs "KNOWN violation still reproduces").
- [x] **CQ-27 (MED, found extending the sargability differential oracle to
  indexed DOUBLE/FLOAT columns, 2026-07-26; FIXED 2026-07-27) — an indexed
  DOUBLE/FLOAT column's equality/IN/range SARG DISAGREES with the full-scan
  residual-filter path on a value stored as -0.0 (negative zero).**
  `scanComparisonsToTupleRange` (executor.go) packed a comparand into the raw
  FDB tuple encoding and compared byte ranges, and that encoding preserves
  the IEEE sign bit — so -0.0 sorts strictly BELOW +0.0 as two DISTINCT,
  physically ADJACENT keys with nothing else representable between them. The
  residual-filter path instead evaluates through `predicates.Comparison`'s
  `cmpAny`, which follows SQL/IEEE numeric equality (-0.0 == +0.0 is TRUE,
  and per RFC-082 a `>=` predicate correctly keeps a -0.0 row against a 0.0
  bound — a deliberate, already-reviewed Go-correct-vs-Java-wrong
  divergence: Java's own `Comparisons.compare`/`compareEquals` are
  `Double.compareTo`/`.equals`-based and are NOT IEEE-correct on signed
  zero either, so this bug was Go-only from the start, with no Java
  reference to port).
  **Model chosen: widen the scan RANGE at probe-construction time, not
  normalize the stored/wire encoding.** Checked CockroachDB
  (`pkg/util/encoding/float.go EncodeFloatAscending`): it collapses ±0 to
  ONE key (`floatZero`) at the KEY-ENCODING layer specifically (not the
  general tuple/datum layer), with `DDecimal.IsComposite` marking the value
  as needing the value part to reconstruct sign — i.e. even Cockroach's
  "normalize" is scoped to the key, never the generic encoder. Checked Java
  (`fdb-extensions/.../TupleOrderingTest.java`): the raw Tuple layer
  preserves the sign bit and sorts -0.0 strictly below +0.0, same as Go —
  confirming the RAW TUPLE ENCODER (`pkg/fdbgo/fdb/tuple`'s
  `encodeFloat`/`encodeDouble`) is the shared, generic FDB wire format and
  must not diverge from Java's identical implementation (also used verbatim
  by the SORT comparator, which deliberately stays sign-preserving per
  RFC-082). Normalizing at the SQL-layer index-key-write boundary instead
  (Cockroach's move) was rejected too: unlike Cockroach's own single-engine
  key format, this store's index entries are read by BOTH Go and Java on a
  shared cluster, and Java's Record Layer index maintainer does not
  normalize signed zero before packing — a Go-side write-time normalization
  would silently split one indexed value across two physically different
  keys depending on which engine inserted the row. Range-widening only
  changes how Go's OWN executor builds a scan boundary; it touches no wire
  byte anyone else reads.
  **Fix:** `scanComparisonsToTupleRange` widens a zero-valued FLOAT/DOUBLE
  bound to cover BOTH adjacent keys wherever it terminates the range: an
  equality (or IN-list per-element sub-probe) that is the LAST comparison
  widens to an inclusive `[-0.0, +0.0]` subtree; `>=`/`<` canonicalize their
  bound to whichever of the two adjacent keys makes the IEEE-correct
  endpoint (`>=` pins to -0.0, `<` pins to -0.0 as the exclusive stop); `>`
  and `<=` are canonicalized too (pin to +0.0) for symmetry, closing a
  latent `col > -0.0`-literal gap the bug report didn't call out. A
  zero-valued equality that is NOT the terminal comparison in a composite
  index's equality prefix (more columns follow it) is deliberately left
  UNWIDENED — see CQ-28 below. `expr.promoteColumnColumnNumeric`
  (`pkg/relational/core/query/expr/expr.go`) — a SEPARATE bug in the same
  introducing commit — no longer wraps an INT-vs-LONG column-vs-column
  comparison in `PromoteValue` (`sharesIntegerWireEncoding`): the two codes
  pack to byte-identical tuple encodings, and the wrapper was defeating the
  SARG matcher's `AccessorNamePath` (only unwraps `*FieldValue` chains),
  degrading a BIGINT=INTEGER point lookup to a residual full scan.
  Pinned by `pkg/relational/sqldriver/negative_zero_index_sarg_probe_test.go`
  (rewritten to assert CORRECT behavior across `=`,`<>`,`<`,`<=`,`>`,`>=`,
  `IN`, `IS [NOT] NULL` on indexed DOUBLE and FLOAT — was the buggy-boundary
  pin) and a new `bigint_eq_integer_uses_index` EXPLAIN subtest in
  `cross_type_join_probe_test.go`. Both mutation-checked RED→GREEN. The
  signed-zero exclusions in `sargability_differential_oracle_fdb_test.go`'s
  `dbl_col`/`flt_col` seeding and `pkg/relational/conformance/rowdiff/gen.go`'s
  DOUBLE/FLOAT domains are REMOVED — both now seed `-0.0` and stay green
  (the oracle's existing bare-int-literal `= 0`/`>= 0`/`< 0`/… sweep exercises
  the fixed row on every run). `cmd/explain-differ` before/after dump over the
  full yamsql corpus: 2637/2637 identical, zero plan-shape flips (neither
  fix changes a plan the corpus happens to exercise).
  **Review-found sibling bug, fixed in the same change:** removing the
  generators' signed-zero exclusion exposed that `packedDedupKey`
  (executor.go, the encoder every unordered DISTINCT/UNION-DISTINCT/CTE
  dedup path routes through) tuple-packed a FLOAT/DOUBLE slot verbatim —
  sign-preserving, like the raw encoder — so `SELECT DISTINCT v` would
  silently split a single SQL-equal value (`-0.0`/`+0.0`) into two output
  rows, disagreeing with `=` (cmpAny is IEEE per RFC-082). DISTINCT is an
  EQUALITY concept and must agree with `=`, not with the sign-preserving
  SORT total order (`compareValues`), which is correctly unchanged. Fixed by
  canonicalizing a zero-valued FLOAT/DOUBLE dedup slot to `+0.0` before
  packing. Pinned by
  `pkg/relational/sqldriver/negative_zero_distinct_dedup_probe_test.go`
  (DOUBLE and FLOAT, both signs seeded twice), mutation-checked RED→GREEN
  (reverting the canonicalization reproduces the exact `[-0 0 5]` 3-row
  split).
  **REVERTED 2026-07-27 — the canonicalization above was itself a wrong-rows
  bug.** The principle ("DISTINCT is an equality concept") is right; it moved
  the wrong side. Canonicalizing ONLY the dedup encoder, while index order,
  aggregate-index keys and unique-key proofs all stay sign-preserving, made
  the same query return different rows depending on the plan — MEASURED:
  `SELECT DISTINCT d, a` returned 2 rows but `SELECT DISTINCT d, a ORDER BY
  d, a` returned 3 over rows `(-0.0,1) (-0.0,2) (+0.0,1)`, because the
  ORDERED dedup path (`distinctStreamCursor`) compares only against the
  PREVIOUS row — sound solely because equal keys are adjacent in index order,
  which canonicalizing the key alone destroys. It also put `SELECT DISTINCT
  d` (1 row) in direct contradiction with `GROUP BY d` (2 rows), the same
  question in SQL. GROUP BY cannot follow: a maintained aggregate index
  stores each group as its own entry keyed by the grouping prefix, so two
  signed zeros are two physical entries Java also reads
  (`AtomicMutationIndexMaintainer.java:141-158`), and Java's own grouping
  splits them (`DynamicMessage.equals` → `Double.equals`). Splitting needs no
  guard on any path; merging needs one on each (ordered dedup, unique-key
  elision, aggregate-index read) and still ends in a Go-vs-Java divergence on
  the same index. Java is consistent here end-to-end — its scalar `=` is also
  bit identity (`Comparisons.java:246` → `Double.equals`) and its index probe
  agrees — so splitting moves Go TOWARD Java. The oracle's `distinctKey` was
  reverted in lockstep; an oracle that disagrees with the engine reports the
  correct engine as the defect. Now pinned as a plan-INDEPENDENCE property
  (hash vs ordered vs GROUP BY must all agree), which is strictly stronger
  than the row-count assertion it replaces.
  **Accepted, documented divergence:** `WHERE v = 0` still matches BOTH zeros
  (cmpAny is IEEE and the SARG range is widened to agree with it) while
  DISTINCT keeps them apart. In strict SQL those should agree, and CockroachDB
  makes them agree by normalizing zero inside its key encoder
  (`pkg/util/encoding/float.go:36-39`) — an option closed to a port whose
  encoder is Java's. Whether Go should instead adopt Java's bit-identity `=`
  engine-wide (which would also make `NaN = NaN` true, as it is in Java) is a
  genuine semantics question for the review gate, NOT a silent choice to make
  here.
- [x] **CQ-28 (LOW, follow-up from CQ-27) — CLOSED 2026-07-28 — a zero-valued FLOAT/DOUBLE
  equality is left un-widened when it is NOT the terminal column of a
  composite index's equality prefix** (e.g. index `(a DOUBLE, b BIGINT)`,
  predicate `a = 0 AND b = 5`, where `a`'s stored value is `-0.0`).
  `scanComparisonsToTupleRange`'s CQ-27 fix only widens a zero bound at the
  position that terminates the range (nothing pinned after it), because
  widening a MIDDLE column requires either a genuine two-way range union
  (not expressible as a single `TupleRange` low/high pair — verified: the
  interval `[(-0.0,5), (+0.0,5)]` is not the 2-key set `{(-0.0,5),(+0.0,5)}`,
  it also admits every `(-0.0, x>5)` and `(+0.0, x<5)`) or teaching the SARG
  matcher (`abstract_data_access_rule.go`/`match_candidate_index.go`) to
  keep a residual filter for the abandoned trailing predicates when it
  decides a composite equality prefix is "fully sarg'd, no residual
  needed" — infra that does not exist today. Needs its own RFC (this is
  matching/data-access infra, milestone-level Graefe/Torvalds ACK) deciding
  between (a) multi-range union execution (reusing the same per-element
  fan-out the IN-list path already has) or (b) matcher-side residual-filter
  cooperation.
  **RULING 2026-07-28 — the premise is CONFIRMED, not contingent; CQ-28 is real
  work.** The prior note said this item might dissolve if Go adopted Java's
  bit-identity `=`. It will not. Go keeps IEEE `=` (both zeros match), matching
  the SQL standard, Postgres and CockroachDB. Java's alternative makes `d = 0`
  fail to find a stored `-0.0`, breaks the most common operation in SQL, makes
  `NaN = NaN` true, and moves away from the stated CRDB reference — for a corner
  case, at the cost of touching every comparison, hash join and ordering in the
  engine. Full-CRDB (everything merges) is physically impossible: a maintained
  aggregate index stores the two zeros as two entries Java also reads. ANSI is
  violated *somewhere* regardless; equality filters run constantly and
  `SELECT DISTINCT` over a deliberately-stored `-0.0` is rare, so the common
  path stays correct.
  **ATTEMPTED AND REVERTED 2026-07-28 — the approach is viable but blocked on a
  second bug.** Rewriting `floatCol = <zero>` to `floatCol IN (-0.0, +0.0)` at
  resolve time is position-independent and needs no new execution machinery
  (`rule_in_to_explode` already fans IN into per-value probes). It DID fix the
  composite case — `v = 0 AND w = 5` returned `[1]` instead of nothing — but it
  REGRESSED the terminal case from `[1 3]` to `[1]`, so it was reverted rather
  than shipped. Root cause measured:
  `inListValueEqual` (`rule_in_to_explode.go:211`) dedups IN elements with Go
  `==`, under which `-0.0 == +0.0`, so the pair collapses to ONE element and the
  surviving single probe does not get the zero-widening. That dedup is
  semantically defensible (`v IN (-0.0,0.0)` ≡ `v = 0` under IEEE); the defect
  is that the collapsed probe seeks only one of the two stored keys.
  **FIXED 2026-07-28.** A zero-valued float equality now TERMINATES the scan
  prefix during index matching (`match_candidate_index.go`), even when more
  indexed columns could be consumed. The zero equality is then the last
  comparison, so the executor widens it across both signed-zero keys, and the
  trailing predicate is applied as a RESIDUAL filter. The scan is bounded to the
  two zero groups and the residual drops the in-between keys, leaving exactly
  the wanted pair. NO multi-range union machinery was needed -- the original
  writeup below overestimated the cost by assuming the whole range had to be
  expressed at once.
  Cost: this shape no longer uses the full composite prefix, so it scans both
  zero groups instead of seeking one key -- bounded by the rows sharing a zero
  in the leading column, and the price of a correct answer.
  KNOWN GAP: only a compile-time-constant zero is detectable at match time. A
  correlated comparand that is zero at runtime keeps the full prefix and can
  still miss the row; de-sargging every correlated composite join to cover it
  would trade a rare wrong row for a broad performance cliff.
  Mutation-verified; sentinel flipped to assert correct rows.
  **Two earlier attempts failed and are recorded so they are not retried:** The correct fix is: keep the IN-list dedup as VALUE dedup
  (it is semantically right — `v IN (-0.0, 0.0)` genuinely is one set member
  under IEEE), and give the surviving single-element probe the same zero-widening
  a plain `v = 0` already gets. That is correct on BOTH paths simultaneously:
  the residual/filter path evaluates one predicate under IEEE and returns both
  rows with no duplicates, and the index path issues one probe widened across
  the two adjacent keys. The widening currently does not reach the IN-derived
  probe, which is the whole defect.
  **Attempt 1 — rewrite `floatCol = <zero>` to `floatCol IN (-0.0, +0.0)`:
  REVERTED.** Fixed the composite case but regressed the terminal case from
  `[1 3]` to `[1]`, because the IN dedup collapsed the pair back to one probe.
  **Attempt 2 — make the IN dedup compare floats by BIT PATTERN so both
  survive: REVERTED (commit 36b7ad7fc, reverted by 6e8acdcf3).** It fixed the
  index path and broke the filter path: `IN` is executed as a JOIN over an
  exploded element list, emitting one row per matching element, which is sound
  only when the elements are MUTUALLY EXCLUSIVE under the comparison. `-0.0` and
  `+0.0` are IEEE-EQUAL, so on an unindexed column every zero row matched both
  probes — `v IN (-0.0, 0.0)` returned `[1 3 1 3]`, each row twice. The test
  that "verified" it used an indexed column in every case, where key-exact
  probes hide the duplication. Mutation-checking it proved only that it changed
  the indexed path.
  **Therefore the IN-rewrite direction is DEAD for CQ-28**, independently of the
  dedup: it manufactures exactly the IEEE-equal element pair that makes
  IN-as-join unsound. Any future attempt must not route a zero equality through
  IN.
  **Pre-existing bug found while measuring, still OPEN and independent of


  blocks CQ-28 and is user-visible on its own.** Over rows
  `(-0.0,5) (5.0,5) (0.0,9)` with index `(v,w)`:

      v IN (-0.0, 0.0)  ->  [1]      plan IndexScan(T_VW,[=,*])   ONE probe
      v IN (0.0, -0.0)  ->  [3]      same plan, keeps whichever is FIRST
      v IN (-0.0, 5.0)  ->  [1] [2]  plan InJoin(...)             correct

  So any user writing `v IN (-0.0, 0.0)` silently loses a row today, and the
  result depends on element ORDER. Independent of CQ-28.
  **Also measured: a separate, more general matcher gap.** An IN on a leading
  column combined with a trailing equality does not sarg at all —
  `v IN (5.0, 9.0) AND w = 5` plans as `PredicatesFilter(Scan(T))` while
  `v IN (5.0, 9.0)` alone plans as `InJoin(IndexScan(T_V))` and
  `v = 5.0 AND w = 5` as `IndexScan(T_VW,[=,=])`. Every `IN + trailing equality`
  query pays a full scan. Far more general than signed zero and worth its own
  item.
  REPRODUCED and PINNED (not fixed) by
  `pkg/relational/sqldriver/negative_zero_composite_index_sarg_probe_test.go`
  (composite index `(v DOUBLE, w BIGINT)`, `WHERE v = 0 AND w = 5` misses a
  stored `-0.0` row via the index) — fails the day this closes, forcing that
  file to flip. `pkg/relational/conformance/rowdiff/gen.go` and
  `sargability_differential_oracle_fdb_test.go` both carry a documented
  safety invariant (no FLOAT/DOUBLE column in a non-terminal composite-index
  position) that keeps their random/deterministic -0.0 seeding from tripping
  this gap; re-check that invariant before adding any composite index with a
  leading DOUBLE/FLOAT column to either generator.
  **PREMISE NOW CONTINGENT (2026-07-27) — do NOT start this item until the
  `=` semantics question is settled.** CQ-28 exists only to extend CQ-27's
  zero-widening, and that widening exists only because Go's `=` is IEEE, so
  `v = 0` is expected to match a stored `-0.0` and the index range must be
  widened to agree with the filter. Java's `=` is NOT IEEE: it is bit
  identity (`Comparisons.java:246` → `toClassWithRealEquals(...).equals(...)`
  → `Double.equals` → `doubleToLongBits`), its ordering comparisons use
  `Double.compareTo` (so `-0.0 < +0.0`), and its index probe agrees with its
  filter — Java is self-consistent and simply does not match `-0.0` for
  `= 0.0`. If Go adopts Java's contract (the open question raised by
  `a8cad2edf`, now at the review gate), CQ-27's widening should be DELETED
  rather than extended and CQ-28 dissolves along with the composite-prefix
  problem, the multi-range-union RFC, and the two generator safety
  invariants above. Building the (a)/(b) infra first risks constructing a
  mechanism the semantics decision then removes. The sentinel test stays
  either way: it just flips to asserting the Java-correct miss as CORRECT
  instead of as a known gap. Note the dependency runs the other way for
  `a8cad2edf`'s DISTINCT split, which is right under EITHER answer — value
  identity is tuple-key identity regardless of what `=` does.

**Exit gate:** focused tests for every item, Cascades unit + race suites,
planner/translator tests, explain-plan golden, yamsql/FDB conformance, generated
file drift checks, and the repository's full non-stress CI suite. Keep this
checklist updated in the same commit as each completed item.

## LATEST PRIOS 2026-07-23 — Cascades quality review v2 (owner-directed, RFC-190)

Source: 2026-07-23 full-engine quality assessment — 5 parallel subsystem review agents
(architecture fidelity A−, cost-model soundness B, rule-set completeness A / fidelity B+,
test coverage B+, code quality B) cross-verified against Java 4.12.11.0 and the current tree.
Wire-compat hard line is CLEAN — every item below is read/optimization-path. **ONE branch
(`feat/rfc190-cascades-quality-audit`), ONE RFC (`rfcs/190-cascades-quality-audit.md`), ONE PR.**
Full gate: RFC → Graefe+Torvalds ACK → implement (one item at a time, DFS, regression test each)
→ milestone review lap → @claude LGTM. Grind order = severity (correctness first).

Current RFC state: **complete.** Whole-PR Graefe/Torvalds review, current-head `@claude` review,
and CI are enforced as PR merge gates.

**Correctness:**
- [x] **190.1 (HIGH) — DONE; naive delete premise INVALIDATED, redesigned N-way path safely retired the arm.**
  The RFC-190 delete plan (Graefe-ACKed) rested on the arm's own gravestone: *"HAS NEVER PRODUCED
  AN EXECUTABLE PLAN."* **That gravestone is FALSE.** Attempting the delete broke two FDB tests:
  `TestFDB_BuriedInnerJoinProjectedExists` and `_Discriminating` (`0AF00: could not plan query`) —
  the arm DOES produce correct working plans (`[[10 true]]`, Java-verified in-test) for the
  explicit-`JOIN…ON` projected-EXISTS-over-3-way shape (`SELECT p.v, EXISTS(…) FROM p JOIN q ON…
  JOIN r ON…`). The crash is specific to the **comma-join** shape (`FROM a,b,c WHERE a.id=b.id…`)
  the gravestone author tested and over-generalized from. Both are flat `[ForEach×3, Existential]`
  selects that reached the arm before the redesign; the comma-join's seed tripped the RFC-173 ordinal
  tripwire, while the explicit-join's did not. **Deleting the arm without its replacement path was a
  net regression — forbidden.** Corpus checks
  (golden byte-identical, "zero firings") missed this because these are hand-written FDB tests, not
  corpus queries — a reminder that FDB is the gold standard, not the corpus.
  Real fix options (both DEEP, Graefe-gated, need a corrected RFC + re-review since the premise
  changed): **(a)** fix the comma-join ordinal-seed crash INSIDE the arm (the RFC-173 tripwire), or
  **(b)** route BOTH shapes through the gathered-cluster wrap with a correct projected-EXISTS
  correlation. Option (b) was attempted (remove the `:309` decline + admit `ExistsValue{QOV(esq)}`
  in `wrapRVFullyBaked`): it PLANS but returns WRONG ROWS — the correlation `d.id=a.id` is dropped
  over the wrap → the existential inner scans all of `nd` → **always-true EXISTS** (a.id=2 wrongly
  true). The wrap was built for WHERE-EXISTS (semi-join FILTER); projected EXISTS needs the
  existential inner CORRELATED to the box's `a.id` (per-row boolean via FirstOrDefault), like
  `buildExistentialJoinSelect` does. That correlation wiring over the wrap is the deep part.
  Also: the false gravestone comment (`rule_implement_nested_loop_join.go` ~:2907) must be corrected.
  **RE-DESIGNED (Graefe ACK, 2-round dialogue).** Converged solution = **Option A**: make Go's
  `PartitionSelectRule` partition existential selects the Java way (`all(anyQuantifier())`), guarded.
  Replace the over-broad `existentialCount==1` bail with a targeted **live-existential guard** (reject
  any bipartition whose live set `lowersCorrelatedToByUppers` contains an existential — Go's
  `positionalMergeCase`/Case-2 can't represent a projected existential as a positional ordinal, a real
  Go constraint Java lacks). This decomposes the flat `[ForEach×N, Existential]` select into binary
  sub-selects Go's existing `implementExistentialSelect` already handles, SARG-preserving, retiring
  the N-way arm while retaining the separately useful gathered-cluster wrap (its retirement is
  post-RFC FU-4). Round-1 NAK (Graefe's `{a,δ}` counterexample
  reached the merge case → wrong rows) closed by the guard; round-2 ACK verified airtight (all lower-
  flow sites `:537`/`:550`/`:559` covered, no plan lost, `{a,δ}` now rejected before `:550`). The
  original five-step migration order was superseded by the atomic direct-emit + guard + retirement
  below; its invariant still held inside that commit: the replacement and guard accompanied the arm
  deletion, unlike the naive delete.
  Cell 7 (multi projected EXISTS) = honest conservative decline (0AF00, never wrong rows), NOT claimed
  fixed. Full design in `rfcs/190-cascades-quality-audit.md` §190.1. **Both gates PASS: Graefe ACK
  (2-round dialogue) + Torvalds ACK.**
  **IMPLEMENTATION CHECKPOINT (before the completed milestone review lap).** The RFC's
  original "Step 1 = flag-flip" estimate was WRONG (Go ordinalizes N-way clusters at TRANSLATION time,
  `cluster_gate.go:399-419`); the correct Step 1 is DIRECT-EMIT (a `QueryVisitor.java:429-434` port —
  dissolve the ≥3-way cluster into a flat NAME-model `[ForEach×N, Existential]` select, bypassing the
  ordinalization machinery), atomic with the guard + arm-retirement. DONE and green:
  - Full N-way matrix returns CORRECT ROWS: comma-join projected EXISTS crash→`[100|t,200|f,300|t]`;
    `buried_inner` PK no longer panics; `_Discriminating` (dup columns), WHERE-EXISTS, NOT-EXISTS,
    4-leg all PASS. Plan-shape golden BYTE-IDENTICAL (zero corpus drift). Full 1165-test sqldriver
    sweep green; nogo/gofumpt clean; +212/−552 LOC (arm retired, NLJ 3418→2974).
  - **Root cause of the hard bug (PK-column existential correlation) = a PRE-EXISTING executor bug the
    new shape exposed:** `flat_map_cursor.go` had `isIdentityOuterRV` but no `isIdentityInnerRV`, so a
    FlatMap result value that passes the INNER quantifier's row through unchanged (which Case-2 flowing
    + a cost-chosen inner-flowed join direction produces) got scalar-wrapped → whole row nested in slot
    0 → PK-column SARG read the record (panic) / non-PK got lucky. Fixed with the symmetric inner branch.
  - **Scoped bail (reviewed deviation from the pure design — Graefe accepted it for this milestone):** removing the
    `existentialCount==1` bail ENTIRELY raced the working Go-only 2-way arm → malformed plans on 7
    tests; scoped to `existentialCount==1 && foreachCount<=2` (2-way stays on the arm; N-way partitions).
    Guard proof unaffected. Full 2-way convergence = separable follow-on.
  Files: `cascades_translator.go` (direct-emit + `gatherInnerClusterOnPredicates`), `rule_partition_select.go`
  (scoped bail + live-existential guard), `rule_implement_nested_loop_join.go` (arm retired, −444 LOC),
  `flat_map_cursor.go` (`isIdentityInnerRV`), memo-shape test strengthened, comma-join FDB regression
  added. Committed `95598761f`; 1M stress green.
  **MILESTONE DONE — Graefe ACK + Torvalds ACK on `95598761f`.** Graefe accepted the scoped bail as
  RFC-190's correct N-way endpoint (correct-or-decline); it does not claim the separately scoped
  two-way convergence is finished. Post-RFC follow-ups remain explicit in RFC §190.1 FU-1..FU-4:
  (FU-1) fix the 2-alias existential correlation then retire the 2-way arm; (FU-2) rename
  `qualifyOuterPositional`→`qualifyPositional`; (FU-3) extract the direct-emit/AXIS-1 shared tail;
  (FU-4) retire the WHERE-EXISTS gathered-cluster wrap.
- [x] **190.2 (MED) — DONE (implementation + review findings folded).** Cost-comparator
  transitivity. The 5 sort-count-gated rungs made the relation non-transitive (verified: real
  3-cycles → arbitrary/nondeterministic winner). Fixed per Graefe's 3-round ruling (root cause: Go's
  `ImplementInMemorySortRule` is a read-side extension Java has no cost rung for; Java's ordinal rungs
  assume a no-sort invariant): (1) **sort-invariant depth** — `concretePlanDepth` skips the
  `InMemorySort` node (Java-verbatim: `ExpressionDepthProperty` strips nothing because Java's tree has
  no sort node), and the 5 rungs are UNGATED; (2) **promoted the `inMemorySortCount` rung** to fire
  just before the structural block (the cost-time analog of Java's structural `RemoveSortRule` — reject
  a redundant sort before the sort-blind rungs; lexicographic reorder, preserves transitivity); (3)
  found+fixed a **real Java-parity bug** — `unmatchedFieldsCount` was missing Java's
  `RecordQueryScanPlan` branch (`UnmatchedFieldsCountProperty.java:96-119`). Pinned by
  `TestCostModel_SortGateCycleRegression` (34 plans, 35,904 triples, incl. two explicitly pinned
  sort-bearing 3-cycles), RED→GREEN.
  Golden re-blessed: 7 flips, all rows-correct (yamsql 338/338) — 6 are sort-elimination/InUnion
  improvements or benign (Graefe-verified); the `covering_index_java` regression REVERTED (fix 2). Two
  stale `plan_contains: InJoin` pins re-pinned per Graefe (the InJoin re-scanned per IN-value / provided
  no order — the old sort-gate resurrected the worse plan; Java produces InUnion/filtered-scan).
  The review lap found and closed two final gaps. The logical memo fallback
  (`expressionDepthRec`) now treats `InMemorySort` as transparent too, pinned RED→GREEN by
  `TestExpressionDepth_LogicalFallbackSortTransparent`. The previously unexplained
  `union_aggregate_java#3` flip is benign and fully traced: both candidates have one sort and the
  same scan/index/unmatched-field profile; ungating Java's fetch-depth rung makes the Go
  `RecordQueryUnionPlan` candidate win at depth 4 over `RecordQueryUnorderedUnionPlan` at depth 5
  because its indexed arm has one fewer projection. Go's `RecordQueryUnionPlan` is not Java's
  ordered merge here — it has no comparison key/ordering hint and executes through
  `executeUnionStreaming`/`ConcatCursors`, with the same union cost as the unordered sibling.
  Live Java cannot plan this exact aggregate-over-union query; Java's rule set and analogous
  upstream fixtures use `ImplementUnorderedUnionRule`. The partial `StreamingAgg` was present
  before and after. No redundant-order regression and no cost change required.
  **Follow-on (taxonomy, not a 190.2 cost fix):** retire or rename Go's duplicate concat
  `RecordQueryUnionPlan`/`ImplementUnionRule` path. Java implements a bare logical UNION only as
  `RecordQueryUnorderedUnionPlan`; until convergence, comments and `DIVERGENCES.md` explicitly mark
  the extra Go candidate instead of calling it ordered or Java-aligned.
  **FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.** Full `just test`:
  56/56 targets green.
- [x] **190.3 (MED, narrow)** — partial-PK prefix scan priced as a 1-row point probe. The
  existing plan-stamped `primaryKeyVals` arity is now authoritative; unknown/partial coverage
  falls back to exact selectivity pricing, and an exactly-one-record-type `PlanContext` is only
  the legacy/hand-built-plan fallback. The obsolete RFC-186 strict/advisory policy split is gone.
  The shared gate covers direct `HintCost`, concrete/adapter costing, logical operator counts, and
  cardinality derivation. Production candidate + copy paths were audited and pinned. RED→GREEN
  regressions cover partial/full/missing/range, conflicting ctx, multi-type abstention, the real
  TypeFilter-backed data-access adapter, and the logical max-cardinality walk. Parent-vs-current
  explain corpus: 2,579/2,579 identical, zero flips. Post-change 1M FDB stress: all 23 subtests
  green with correct rows. **FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.**
  Full `just test`: 56/56 targets green.
- [x] **190.x-bundled (cheap latents)** — both findings had already landed as discrete RFC-189
  commits: finding 8's final-only merge-cycle guard in `eac6ef9ab` and finding 5's scalar
  signed-zero equality/hash repair in `8fcb1a426`. RFC-190's re-audit caught one over-broad parity
  claim in the latter: raw `math.FloatNNbits` distinguishes NaN payloads, while Java boxed
  Float/Double equality canonicalizes them. Scalar equality now uses Java-style canonical bits for
  both widths (zero sign preserved, all NaNs canonical), with distinct-payload/sign NaN and
  signed-zero hash-bucket controls. The direct vector-slice carriers keep their separately
  documented raw-bit identity. The final-only cycle and scalar-float regressions are green 20×.
  Parent-vs-current explain corpus: 2,579/2,579 identical, zero flips. **FINAL REVIEW: Graefe ACK
  + Torvalds ACK + independent Codex ACK.** Full `just test`: 56/56 targets green.

**Fidelity / optimization reach (Graefe-gated design):**
- [x] **190.4 (Graefe-reruled)** — the original “query fewer / compensate candidate extras” MED
  premise is NAK: executable Java enumerates dependency-sound partial bijections and then applies
  expression-specific subsumption; every Select `ForEach` on both sides must still be covered.
  Staged closure:
  - [x] **190.4a** — guarded single-source reach for candidate-only dead existential legs:
    position-independent sole-`ForEach` selection, dead-alias/result/predicate/dependency checks,
    outer/extra-`ForEach`/non-tautology rejection, child-match retention, and usable-compensation
    regression plus negative twins. Focused regression green 20×; parent-vs-current explain corpus
    2,579/2,579 identical; full `just test` 56/56. **FINAL REVIEW: Graefe ACK + Torvalds ACK +
    independent Codex ACK.** This is explicitly not the full Java port.
  - [x] **190.4b** — dependency-sound/topological subset enumeration, all child-match branches,
    `RegularMatchInfo` merge/child metadata, semantic multi-match dedup, and deterministic bounded
    search (40,320 visited search states / 64 unique outputs; safe miss on exhaustion). Includes
    constant-aware recursive group-value pull-up and fail-closed multi-member metadata adjustment.
    Focused regressions green 20×; affected uncached Bazel targets 3/3 green; full `just test`
    56/56. **FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.**
  - [x] **190.4c** — current Java Select `ForEach` coverage, existential ownership/dependencies,
    predicate implication/candidate coverage, composed child pull-up/result mapping, and
    possible/impossible compensation state. Existential-to-`ForEach` matches install a required
    PK `Unique` over the pinned physical access. Production correlated primary `UNNEST` reaches a
    structurally expanded FAN_OUT index and yields
    `Project → UnorderedPrimaryKeyDistinct → Fetch → IndexScan`, with no base-scan/filter/explode
    fallback. Real FDB rows pin empty/nonmatching/duplicate-element cardinality, fresh-transaction
    serialized continuations, and a five-row base `COUNT(*)` that must avoid the fan-out index.
    Unknown/contradictory metadata, scalar nesting, unsupported fan-out/function shapes, false
    coverage/ordering, and every raw cardinality shortcut fail closed. Focused race + 10× green;
    affected Go/Bazel targets green; parent-vs-current explain corpus 2,579/2,579 identical; full
    `just test` 56/56. **FINAL REVIEW: three independent Codex audits ACK** (candidate parity,
    shortcut/cardinality safety, SQL/FDB semantics).
- [x] **190.5 (MED)** — index-intersection reach: bounded generic k-way plus Java-faithful rich
  common-ordering, redundancy, directional comparison-key, and reverse execution now land.
  - [x] **190.5a** — enumerate every 2..4 restricted-candidate subset under the existing
    eight-match budget; pair sieve + early-out; exact-input re-entry guard; cap after adjusted-twin
    collapse; retain subsets until Java's redundancy proof exists. Production SQL + real FDB prove
    one selected four-way intersection and exclude every three-of-four decoy. Focused race/repeat,
    affected Bazel 3/3, parent EXPLAIN 2,579/2,579, 1M stress 23/23, and full suite 56/56 green.
  - [x] **190.5b** — port rich common-ordering/comparison-key derivation and
    `isPartitionRedundant`; require every free PK component; atomically carry comparison direction
    and reverse through plan identity, execution, ordering properties, and fetch/set-op rewrites;
    add the `DESC` SQL/FDB regression. The current Java target's top-to-top requested-order translation,
    fixed-binding dependency normalization, fan-out-leg PK distinct wrapping, and non-empty-only
    subpartition eviction are included. Natural forward-ASC/reverse-DESC flat field keys execute;
    mixed/counterflow, ordered-bytes, ambiguous baked-layout, and non-flat shapes decline safely.
    Focused race/repeat and affected Go/Bazel targets pass; real FDB pins exact DESC rows,
    continuation, and low scan budget. Parent-vs-current EXPLAIN is 2,573/2,579 identical with six
    Java-verified sort-elimination/ordered-index winners, zero regressions; their three row corpora
    pass 58/58. The checked-in golden records those six flips. The 1M gate passes all 23 subtests in
    150.95s vs the 190.5a checkpoint's 151.71s; full `just test` is 56/56. **FINAL REVIEW: two
    independent Codex audits ACK.** Safe residual: Go's separate cross-candidate pass retains
    already-yielded singleton alternatives that Java's shared partition map evicts; tracked in
    `DIVERGENCES.md` as plan-space widening, not a row-safety gap.
- [x] **190.6 (architectural — Graefe ruling)** —
  port Java 4.12.11's NLJ/FlatMap Case 1/2a/2b source-order partition matrix while retaining Go's
  sanctioned `RecordQueryInMemorySortPlan` fallback. Ordered variants keep the cheapest exact
  expression per Java-retained source-order partition and freeze both legs in private final
  singleton references. Rich ordering now pulls through result values with fixed-binding/
  dependency preservation, a fail-closed value-root bridge, and a projected-EXISTS ordering lens;
  ordered primary/index recovery reuses the existing scan rules and retained partial matches,
  including safe reverse-unbounded-primary recovery while bounded synthesis declines. The paired
  indexed/index-less regressions pin sort elimination versus fallback.
  Final EXPLAIN census: old=2,579/new=2,581, identical=2,491, differing=90; the 88 comparable
  shape flips are 87 eliminated outer/unary sort enforcers plus one already-sorted fixture changed
  by its new index, with zero plan-error regressions/recoveries; the other two entries are the new
  queries. Focused tests pass 20×; affected Cascades, yamsql, and real-FDB tests are race-clean.
  Yamsql passes 5/5, and real FDB pins exact ASC/DESC/LIMIT/NOT-EXISTS rows plus the index-less
  fallback. Generated ledgers/golden, lint, and the full `just test` suite pass (56/56). The uncached
  1M FDB stress gate passes all 23 subtests with exact row counts; its four planner-query timings
  remain within 1–4% of the prior checkpoint. **FINAL REVIEW: two independent Codex audits ACK**
  after closing the cross-root baked-ordinal safety finding and three enumeration-control/key-map
  collision coverage findings.

**Maintainability:**
- [x] **190.7 (MED-HIGH)** — DONE. The original four-copy census was stale after 190.1 retired the
  N-way arm: three source sites remained, reached through four behavioral routes (direct fallback,
  join fold, primary-key fast path, and secondary-index fast path). Their below-FOD filter →
  `FirstOrDefault(NULL)` → optional existential residual is now centralized in
  `buildExistsCompensationChain`; its explicit mode preserves the direct path's fresh bookkeeping
  aliases and the correlated paths' real inner alias. `buildExistsFlatMap` was removed: its
  comparison-range construction and residual split are shared with `tryExistsFlatMap`, while the
  distinct PK/index failure behavior remains local. `buildCorrelatedFlatMapPlan` intentionally
  remains separate because its ForEach, strict-FOD/default-on-empty, compensation, and predicate
  ordering contracts differ. The alias-contract regression passes 20×; affected Cascades, yamsql
  (26/26), and real-FDB routes pass, including race coverage. The parent-to-worktree EXPLAIN corpus
  is byte-identical (2,581/2,581), and generate, lint, and full `just test` pass (56/56).
  **FINAL REVIEW: two independent Codex audits ACK** after the zero-below-FOD and predicate/QOV
  identity coverage was added.
- [x] **190.8 (LOW)** — DONE. The original tri-layer/fold-v-runtime premise was stale after RFC-181:
  constant folding, INSERT VALUES, and ordinary SELECT execution already shared
  `ScalarFunctionValue.Evaluate`; `embedded/scalar_functions.go` is only the intentionally different
  INFORMATION_SCHEMA map interpreter. The residual three-way duplication—name/operator dispatch,
  generic-call result inference, and route admission—is now one values-owned catalog with separately
  pinned capabilities (56 evaluator spellings, 53 Cascades-safe, 49 generic scalar calls, 12 legacy
  map calls). Aliases share operators/result strategies; the expression walker and dedicated BIT*/
  CURRENT_* routes obtain their types from the catalog; the map interpreter shares admission/operator
  identity but retains its Java-aligned carrier, arity, parsing, and SQLSTATE bodies.
  The audit also found and fixed a real wrong-result seam: common-typed scalars, CASE/PickValue, and
  PromoteValue could declare DOUBLE/FLOAT while returning an integer carrier, so downstream division
  took the integer lane (`COALESCE(3,1.5)/2 = 1`). Numeric results/promotions now honor the declared
  carrier (including direct LONG→FLOAT single rounding), and arithmetic selects the DOUBLE lane from
  static types. Integer ROUND now honors negative precision without float conversion, floating ROUND
  clamps SQL precision and avoids temporary-scale overflow, and NULL precision propagates before the
  integral fast path. Exhaustive catalog/operator/alias tests, constant-fold/runtime carrier
  regressions, the 47/47 numeric yamsql scenario, and the generated ledgers pass. The required race
  audit also exposed ANTLR's generated constructors sharing mutable DFA/context caches; all public
  parse routes now lease bounded exclusive warmed state and detach returned read-only trees, with
  concurrent valid/error/tree-reuse regressions. Existing plan shapes are unchanged (2,581/2,581
  identical); the golden adds only the nineteen new regression queries. `just generate`, `just lint`,
  and full `just test` pass (56/56). **FINAL REVIEW: two independent Codex audits ACK.**
- [x] **190.9 (LOW)** — DONE. The twin hash/linear NLJ inner loops now share one candidate-position
  body. A literal identity-index slice was rejected because it would add unbudgeted O(inner rows)
  memory to every linear join; the final candidate view uses `(nil, count)` for zero-allocation
  linear/degraded scans and `(bucket, len(bucket))` for hash hits, while keeping hash misses distinct
  as `(nil, 0)`. Predicate evaluation, ordinal binding, match tracking, positional emission, and all
  join types now have one implementation. Continuation PK capture/reposition uses the same view, so
  hash positions remain bucket-relative and linear/degraded positions remain row-relative.
  Regressions pin sparse hash-bucket resumes at every split, shifted-bucket PK repositioning,
  positive time/string degraded probes, hash misses, and FULL OUTER matched-inner/drain bookkeeping
  over noncontiguous indices. The executor is race-clean and 20× repeat-green, the large-inner
  real-FDB FULL OUTER route passes, and the 2,600-entry plan golden is byte-identical. `just generate`,
  `just lint`, and full `just test` pass (56/56). **FINAL REVIEW: two independent Codex audits ACK.**

**Test gaps (the plan-quality axis):**
- [x] **190.10 (MED)** — DONE. Replaced the stale text/reference census with a source-derived
  completeness gate over the exact 125-rule production universe. The gate accepts only canonical
  rule-constructor calls inside runnable Go tests (including correctly imported external-package
  tests), and rejects comments, helpers, malformed test signatures, unrelated selectors, and
  locally shadowed constructors. Thirteen isolated positive tests now pin the missing or ambiguous
  ordering, projection, fetch, limit/sort implementation, and finalization transformations,
  including alias/correlation translation, source-kind preservation, exact child relinking, and
  constraint propagation. Red-verification by hiding one constructor reported exactly the omitted
  rule. Focused/race/repeat validation passes, the 2,600-entry plan golden is byte-identical, and
  full `just test` passes (56/56). **FINAL REVIEW: two independent Codex audits ACK.**
- [x] **190.11 (MED)** — DONE. The stale “3 tests + 1 fuzz” census understated the suite:
  `planning_cost_model_test.go` already contained 25 tests, while the fuzz target checks scalar-cost
  finiteness rather than the winner comparator. A source audit found 22 ordered PLANNING decision
  slots (20 conceptual criteria; the fetch block has three separately decisive sub-rungs), with 15
  covered end to end. Seventeen focused tests now close all seven missing winner rungs (data-access
  count, recursive DFS, IN penalty, explicit Fetch count, nested InJoin count, NLJ predicate count,
  DefaultOnEmpty), pin the primary/index config and strict-SARG branches, prove a same-shape
  join-order winner flip under swapped statistics, exercise adversarial rung ordering, and cover the
  two missing REWRITING tiers. Late-rung fixtures deliberately make scalar cost or stable hash favor
  the loser. The scoped 34-plan property corpus checks irreflexivity, antisymmetry, totality up to
  stable identity, 35,904 ordered triples, tie substitutability, and 68 rotated/reversed minimum
  folds. Global antisymmetry is deliberately not claimed: Java's root-IN `flipFlop` can return `+1`
  in both orientations for heterogeneous SARG states. Focused/race/repeat validation passes, the
  2,600-entry plan golden is byte-identical, and full `just test` passes (56/56). **FINAL REVIEW:
  two independent Codex audits ACK.**
- [x] **190.11-FU (MED)** — DONE. Finals-only memo children now use `Reference.Get()`'s established
  exploratory-first/final-fallback contract instead of becoming an unknown `1e6` subtree; the
  explicit best-cost helpers include both exploratory and final members. The six-shape probe also
  exposed and closed `InUnion` underpricing: literal fanout is the Cartesian product of source
  sizes, unknown dimensions retain a conservative factor of ten, child CPU is paid once per
  execution, and zero/one combinations match the executor's empty/pass-through fast paths. A
  known-empty source now skips the child even beside an unknown source. Direct cost, cardinality,
  comparator, and executor regressions cover finals-only, mixed-member, multi-final, literal,
  unknown, empty, and order-invariant cases. The audited 2,600-query golden changes only the two
  valid key-only HAVING pushdowns; all four repeated-full-scan IN regressions from the raw lookup
  probe remain unchanged. Focused/race/20× validation passes; `just generate`, `just lint`,
  `just build`, and full `just test` pass (56/56). **FINAL REVIEW: two independent Codex audits
  ACK.**
- [x] **190.12 (MED)** — DONE (impl commit #1). Committed `explaindiff/testdata/plan_shape.golden`
  (16550 lines, 2421 queries + 158 DML) + `TestPlanShapeGolden` (`plan_shape_golden_test.go`) — an
  always-on, no-FDB snapshot net that fails on any un-blessed physical-plan-shape change and prints
  the first divergence + a re-bless command. Red-verified (perturbed a plan line → RED; committed
  golden → GREEN). This is the standing net every later RFC-190 commit is explain-diff'd against.

**Doc rot + diagnostics:**
- [x] **190.13 (LOW)** — DONE. `plandiff.go` header rewritten (Java IS wired via NewJavaRunnerHTTP →
  the Bazel `conformance_server`; the no-URL forms are offline fallbacks; the "naive generator" was
  retired — single Cascades path); `abstract_data_access_rule.go:29` corrected (Pareto containment
  pruning IS implemented via `findContainingAccess`); all "cross-page-buggy hash-set" occurrences
  (4 source + 1 test, no `distinct.go:38` — it was `:172`) corrected to "memory-heavy hash-set" with
  a C5 note (cross-page correctness fixed 2026-07-20; streaming preferred for O(1) memory, not
  correctness). Final audit removed the accidentally tracked `screenlog.0`, ignored future GNU
  Screen logs, narrowed the retired N-way dead dispatch, and corrected every surviving production,
  regression, and historical-ledger description that still presented that arm as live or denied
  PartitionSelectRule's correlated-existential route. Zero plan behavior change.
- [x] **190.14 (LOW)** — DONE. A shared exhaustive taxonomy assigns all 41 production
  `RecordQueryPlan` types an explicit count and residual policy; unknown types and future
  unhandled policy kinds warn on the concrete/logical count and residual walks, while a type
  missing `HintCost` warns on the join-cost walk instead of silently becoming free. Diagnostics
  are opt-in through the outermost
  `WithCostModelDiagnostics(PlanContext, *slog.Logger)` wrapper, use structured stable walk tokens,
  deduplicate concurrently per `(wrapper, walk, concrete type)`, and never call `Explain` or write
  to `os.Stderr`; nil loggers mask an inner sink and disabled WARN levels do not consume a dedupe
  key. Planner and both rule-call comparator paths retain the sink while preserving their
  historical nil-context winner semantics. Twelve parallel regressions pin the 41-type inventory,
  logical/concrete compatibility boundaries, folded children, future enum arms, logger scope and
  concurrency, and end-to-end plumbing. Focused/race/20× tests pass; the 2,600-query plan golden is
  byte-identical; `just generate`, `just lint`, `just build`, and full `just test` pass (56/56).
  **FINAL REVIEW: adversarial Codex audit ACK; second independent audit ACK.**

---

## LATEST PRIOS 2026-07-21 — owner directive (takes precedence over everything below until done)

Source: 2026-07-21 full-engine triple review — Graefe (Cascades alignment, B+), Torvalds (code
quality, B-), codex `gpt-5.6-sol` ultra (adversarial correctness, F/NAK; prose archived at the
PR for prio 1). Grind ONE AT A TIME, in order, each done RIGHT (root cause + regression tests +
milestone review lap), each its own PR.

- [x] **1. Panics on user-reachable paths** — DONE (PR #509, Graefe ACK + Torvalds ACK + codex
  clean ×2 on `7c4da6c72`). The §3 eval refactor had ALREADY landed with RFC-173 (the audit doc
  was stale); the milestone delivered the rest: `ComparisonKeyFunc` error channel (Java parity:
  `KeyedMergeCursorState.comparisonKeyFunction` → RecordCoreException through the future chain)
  converting the twin intersection closure panic sites; four `ordinal_join.go` malformed-plan
  tripwires → typed errors under the new assert-locality rule (docs/panic-audit.md — asserts
  only for same-derivation invariants; cross-component malformed-plan detection returns errors);
  DistanceRank row-eval panic → error through `Eval`'s channel; `positional_merge.go:101` and
  `ordinal_join.go:245` verified genuinely local → stay asserts; `plans/intersection.go`
  constructor needs no validation (Java parity — runtime errors, now delivered via the error
  channel); `NewRecordType` dup-name probed (both review claims wrong in turn — see audit doc)
  and defended by construction via the shared `unnestAliasReject` guard (dedupes 4 inline AS==AT
  guards). Audit doc refreshed truthfully; regression pins on every converted site. Remaining
  panic surface = §4 asserts + RFC-173 seed tripwires (retire with item 4).
- [x] **2. Cost model soundness** — DONE (PR #510, merged `2b053c0d8`; Graefe + Torvalds + codex
  + @claude all ACK). RFC-186 rewriting-cost-derivation: (a) `getWinnerForOrdering` now returns
  `(expr, satisfied bool)` so a global-cheapest fallback can't be mistaken for satisfaction (§2C);
  (b) the memo-population-history dependence is replaced by the DESIGNATED-final virtual prune
  (`designated_final.go`) — REWRITING cost derives through one deterministically-chosen final per
  child, generation-keyed, with cycle + exploratory-child taint guards (§2A); (c) the all-equality
  point-probe gates on full-PK equality binding via `pkFullyEqualityBound` (§2B; its original
  strict/advisory split was later unified around plan-stamped PK arity by RFC-190.3);
  (d) the missing plan types get `HintCost` dispatch + `warnUnpricedPlanType` (§2D). Plus
  the codex adversarial rounds (identity refinement, attribute-aware tie-break) and two latent
  bugs found en route (PartitionBinarySelect noe-leg absorption, memo orphan registration).
- [x] **3. Debt ledger understates reality** — DONE (this PR). Four sub-fixes:
  - **3a** `RecordQueryRecursiveLevelUnionPlan.CanCorrelate` false (Java parity) — the UNSAFE
    wrong-rows anchor that suppressed an outer alias a leg reads; pinned by a colliding-alias
    propagation test.
  - **3b** "memo costs an expression that is not the one that executes" — VERIFIED already fixed
    by RFC-184 W2 (wrappers deleted; physical plans store children solely as quantifiers, so the
    dual-storage divergence is unrepresentable). Ledger corrected CURRENT-BUG → CLOSED; pinned by
    `TestFlatMapPlan_WithQuantifiers_NoStalePlanSnapshot`.
  - **3c** plan-cache key: injective (`canonicalTextOf` not `GetText`; quote-aware normalizeSQL
    for delimited-id case + string-literal whitespace) + schema-scoped (schema name + metadata
    version). Pinned by `plan_cache_key_test.go`.
  - **3d** aggregate continuation fails LOUD on any non-Go-format shape (was silent zero-fill);
    ledger's blanket "byte-identical continuations" claim carved out to name the engine-private
    aggregate + in-memory-sort payloads precisely. Pinned by
    `TestDecodeAggregateContinuation_ForeignShapeFailsLoud`.
- [x] **4. Name-string/ordinal seam** (= RFC-173 endgame) — DONE. This item's parenthetical was
  STALE the day the LATEST PRIOS list was written (2026-07-21): it was copied verbatim from the
  2026-07-13 "S4 CAP checkpoint" block and never updated, while the endgame it describes had
  already landed in-tree. Verified against `master` (HEAD `8d847810a`, `git log master..HEAD` = 0):
  - The **deletion pass is complete** — `QueryResult.Datum` gone (`29d50bf90`), `AnchoredJoin`
    flag+producer + `NewAnchoredJoinRecord` + `buildUnnestResultValue` gone (`715c8d20e`), the
    dual-window §5 oracle machinery gone (`2b285eb69`). Grep for any live field access / func def
    of these returns nothing; only historical WHY-comments remain (verified: runtime is
    Positional-only, no live name-model path in these targets).
  - The **three shapes are green live on FDB** and ordinalize positionally — each now PLAN-pinned
    (the plan assertions were added in the item-4 closure PR after review found the shapes were
    row-pinned only):
    - `buried_2chain_straddle` (`chained_unnest_3link_filtered_ordinal_fdb_test.go`): asserts the
      output bakes to a positional ordinal (`Y.Y#0`) over nested FlatMap-over-Explode — the buried
      path is positional, distinct from the chained-ordinal DISPATCH the 5 sibling cases take.
    - `SELECT *` multi-source unnest (W5, PR #466): the `starRows` helper in
      `array_unnest_ordinality_fdb_test.go` now asserts every multi-source `SELECT *` gathers as
      `FlatMap(outer=Scan(WSRC), inner=Explode(field…))`, never a name-model merged row.
    - FULL-box+subquery: `baretwin_gather_fdb_test.go` `grouped_subquery_conjunct_gathers` asserts
      the baked GROUP-BY ordinal (`X#N`) over a FlatMap-over-Explode gather of the FULL OUTER box.
      This GATHERS positionally — a better outcome than the originally-planned loud-reject; only a
      duplicate-FROM-alias shape still loud-declines.
  - Graefe **DESIGN-ACK** is recorded in the RFC for every endgame piece (W5 multi-source,
    S4 AnchoredJoin demolition boundary), no NAK.
  The residual RFC-173 work (B/C/D-phase: the runtime `FieldIndex(name)` census, `PositionalRow.GetByName`
  heuristic, `descendResolvedPath`) is a SEPARATE, later migration phase tracked in the RFC — NOT
  this endgame item, whose named targets (the 3 shapes + the 5 deleted machineries) are all closed.

---

## FINDINGS 2026-07-22 — Cascades engine quality review (owner-directed)

Source: 2026-07-22 Cascades quality review — 5 parallel subsystem review agents (rules,
matching/data-access, cost/properties, values/predicates, memo/tasks) + main-context
verification. Every finding below was spot-checked against the code AND Java 4.12.11.0. The
CORE (epoch termination, memo merge/cycle guards, 3-tier interning, `memoEqual` correlation
guard, 3VL null logic, directional cost rungs) was traced and is SOUND — the defects are in
the periphery/derivations.

**RFC-189 STATUS (branch `feat/rfc189-cascades-review-residuals`, `rfcs/189-cascades-review-residuals.md`,
ACKED rev 2):** the residuals below are being ground out in ONE PR (owner directive).
- **DONE (landed green):** finding 8 (A1 hang), finding 5 (A2 signed-zero), finding 9 (A3 projection),
  finding 7 (A4 correlation → DIVERGENCES.md closed); finding 10-M3-followup (B1) + a `WithPrimaryKey`
  builder footgun; finding 10-M4-followup-3 (B2 VERSION fan-out); finding 12 a/b/c/d (C1–C4; 12b premise
  corrected — Java also trims; 12c-GroupBy pull-up booked separately); dead-fn deletes (Demote,
  findMatchingReachableCandidate wrapper, isExploratoryMember dup); finding 2-followup-a (F2 real
  RemoveRangeOne); finding 3-followup (F3 IndexScanPreference). finding 10-M4-followup-2: confirmed already
  resolved (RFC-188 round 7). GetPlans: no change needed (already guarded).
- **RECLASSIFIED:** finding 13 intersection-rule deletion → KEEP (fuzz completeness net + white-box tests
  exercise the logical→physical path; SQL-unreachable but not dead-in-the-harmful-sense).
- **DONE (added):** finding 10-M5-followup (B3 structural PK — index arm re-ported structurally via a new
  KeyExpression→Value translator; validated by rowdiff+plandiff+1M-stress, no flip/no dropped rows;
  scan-arm item-d booked — B1 cardinality-count ripple).
- **E2 (finding 6-followup) — AUDITED, REVERTED, KEPT BOOKED:** the dense predicate-count producer is
  Java-faithful in isolation but REGRESSES designated survivors — for Sort(Scan) vs Scan (both 0 preds)
  the dense tree-depth tiebreak fires the predicate-count rung too early (before a simplicity rung) and
  prefers the deeper Sort(Scan). plandiff/rowdiff/1M-stress were green (final plans unaffected) but the
  designation-invariant unit tests caught the regression. Root cause = Go's designated-comparator RUNG
  ORDER; proper fix needs Java rung-order verification, not just densifying the producer. Low value; keep
  booked (as RFC-188 deliberately did). This is the per-flip audit protocol working as intended.
- **REMAINING (large/review-gated — not to be rushed):** finding 13 MatchIntermediate permutation port
  (medium, edge-case optimization), finding 11 (F1 OR-expansion phase relocation — Graefe-gated infra),
  finding 10-M4-followup (E1 Fetch-storedRecord ~48 flips — the DANGEROUS dropped-dedup direction, needs
  the full per-flip duplicate-bearing audit + reviewer sign-off; E2's outcome shows these arms can
  regress). Then the milestone review lap (Graefe+Torvalds+codex+@claude) + PR.

**Owner directive (2026-07-22): land finding A (leaf-name column matching) IN FULL —
principles-first, no quick fixes: RFC → Graefe+Torvalds review → implement in full → review.
A is the current focus; the rest are booked below in severity order. A is the planner-side
analog of the RFC-173 fight (stop resolving columns by leaf name), on the index-match path.**

### [x] A — SYSTEMIC: leaf-name column matching across the planner — DONE (RFC-187, all 10 sites)
The same `strings.EqualFold(fv.Field, …)` / bare-leaf-name shortcut recurs, each ignoring the
`FieldValue.Child` accessor chain — it binds/matches the WRONG column when a nested-path leaf
name collides with a top-level (or differently-rooted) column of the same source. Violates the
CLAUDE.md prime directive ("fix the resolution infrastructure — don't strip qualifiers with
string hacks"). `buildTranslateValueFunction` (match_candidate_index.go:405) was hardened
against exactly this collision; the rest were not. Sites:
- `rule_match_intermediate.go:755` `valuesMatchColumn` FieldValue arm — **CRITICAL, wrong rows**
  (finding 1): `WHERE addr.city='NYC'` binds as a sargable seek on a same-named top-level `city`
  index, marked matched → no residual re-check → returns rows where top-level `city='NYC'`.
- `aggregate_index_candidate.go:129,151,157,175,198,204` `MatchesGroupBy`/`MatchesSingleAggregateOf`
  — `GROUP BY addr.city` / `COUNT(addr.city)` matches a top-level `city` aggregate index → wrong column.
- `rule_aggregate_data_access.go:195` `groupColEqualityIndex` — group-key `WHERE addr.city='x'`
  → AISCAN bound on top-level `city`.
- `expression_partition.go:261` `orderingPartitionHash` — hashes `fv.Field` only, drops `fv.Child`;
  `toPartitionsFromMap:218` groups hash-only (sibling RollUp path uses `ValuesStructurallyEqual`)
  → same-leaf-name orderings from different sources collapse → sort elided → **wrong ORDER BY** (finding 4).
- `rule_push_filter_through_groupby.go:107,135` + `rule_streaming_agg_from_index.go:80` — pushdown
  eligibility by leaf name (documented "at worst leaks a duplicate").
Root cause (proven, RFC-187 §2): match-time column identity is compared across a MIXED, MULTI
representation — query qualified refs are resolver-BAKED (ordinal) while the candidate is LAZY,
NAME-based by construction (`columnNames []string`, no ordinals), so raw `SemanticEqualsUnderAliasMap`
binds nothing (baked≠lazy) and leaf-name `EqualFold` was the bridge. A nested ref also exists in 3
forms (nested-Child, fused-baked, flat-dotted). Fix (Graefe-ACKed name-path, `rfcs/187-column-identity-matching.md`):
one `values.AccessorNamePath`/`ColumnNamePathsEqual` primitive (full accessor NAME path, root-alias
excluded, loud `ok=false` on pure-ordinal accessors) at ALL 10 sites S1-S10.
Aggregate sites (S4/S5/S8) ship the TRANSITIONAL reject-nested via `aggColumnMatches` (query
grouping-key/agg-operand compared by full path against the candidate's declared column): a nested
query key does not match → base-record StreamingAgg (correct rows).
Follow-up A (Graefe RFC-187 §3.2 condition 3, booked): full nested-aggregate-index SUPPORT needs the
candidate to carry real nested paths end-to-end — fix the `cascades_generator.go` group/agg column
mis-flatten (`groupCols[0]="addr"` for `GROUP BY addr.city`), `ColumnValue` to build nested
placeholder FieldValues, and expansion/execution (`WithGroupColumns`) — before nested agg matches can
be safely ENABLED. Also form-(c) flat-dotted qualified grouping keys (`T.city`) conservatively miss
the agg index until grouping keys are uniformly structured.
Follow-up B (RFC-187 §8, post-RFC-173): ordinalize the candidate (resolve `columnNames`→ordinals) so
both match sides are baked and match-domain identity collapses into Java's `FieldPath` ordinal
identity — the true-parity endgame, entangled with RFC-173.

DONE (branch feat/rfc187-column-identity-matching): [x] primitive [x] S1 [x] S2/S3 [x] S6
[x] S4/S5/S8 [x] S7/S9/S10 (all 10 sites) [x] milestone review lap — Graefe ACK + Torvalds ACK on the
implementation [x] 1M stress green (no row-count/plan regression) [x] full 56-target suite green on
every commit. Only the two booked follow-ups remain (nested-agg-index SUPPORT; §8 candidate
ordinalization) — both distinct from A's wrong-rows/wrong-order fix.

**Systemic problem B (cost/cardinality-model soundness) = findings 2, 3, 6, 10, covered by
`rfcs/188-cost-model-soundness.md` (branch feat/rfc188-cost-model-soundness). RFC ACKED rev 2 (Graefe +
Torvalds; rev 1 NAK'd, rev 2 folded: finding 2 → DELETE not rename, finding 3 → bare-comparison set +
flipFlop sign + config confirmed-absent, finding 6 → sparse-sorted first-map, finding 10 M4 → plan
plumbing). Java refs confirmed against 4.12.11.0. Grind order: 2 → 6 → 3 → 10(M2 → M5 → M3&M4).
One follow-up remains booked below (port Java's real RemoveRangeOne);
`IndexScanPreference` configuration parity landed in RFC-189 F3.**

### [x] Finding 2 (HIGH, wrong results) — RemoveRangeOneRule deletes LIMIT 1 on an unfloored estimate — DONE
`rule_remove_range_one.go:52,68` gated deletion of `LIMIT 1 OFFSET 0` on `EstimateCardinality(e)<=1.0`;
`cost.go:520` (LogicalFilter) and `:609` (Select) compute `in * 0.5^numPreds` UNFLOORED over
`LeafScanCardinality=1e6`, so `1e6*0.5^24≈0.06<1.0` → LIMIT deleted → plan returns ALL matching rows.
Correctness gated on a heuristic constant (cardinal sin). Also: the rule wore Java's name but did
something Java's `RemoveRangeOneRule` (remove an unreferenced `RANGE(0,1)` quantifier) does not — Go
invention. **DONE (RFC-188 §1, Graefe ruling): DELETED the rule + tests + registration** — Java has no
limit-removal rule (keeping one is itself a plan divergence), and the narrowed-to-`LogicalValues` rule
is production-dead (`NewLogicalValuesExpression`: 0 non-test callers).
**Reachability (verified): memo-level real, not SQL-reachable today.** Probed: `Limit(1,
24-separate-predicate-filter)` planned to `PredicatesFilter(Scan,[24 preds])` — limit stripped. But SQL
collapses `a AND b AND …` into ONE `AndPredicate` (numPreds=1 → estimate 5e5 → never fires). Latent
landmine, not a live SQL regression. Pins: planner-level red→green
`TestPlanner_LimitOneOverMultiRowFilterRetainsLimit` (RED = limit stripped, GREEN = retained) + yamsql
`limit_one_over_wide_filter` SQL-surface guard (green sentinel for the conjunct-collapse boundary).

### [ ] Finding 2-followup-a — port Java's REAL RemoveRangeOneRule (booked by RFC-188 §1)
Java `RemoveRangeOneRule.java:45-102` drops an unreferenced `RANGE(0,1)` table-function quantifier from
a `SelectExpression` (nothing to do with LIMIT) — UNPORTED. Porting it reclaims the `RemoveRangeOne`
name cleanly. Distinct missing-rule item; needs Graefe+Torvalds review (query-engine rule).

### [x] Finding 3-followup — model IndexScanPreference config (booked by RFC-188 §2) — DONE
RFC-189 F3 (`7313c51c2`) added `IndexScanPreference` to the existing `PlannerConfiguration` mirror,
defaults it to Cascades' `PREFER_SCAN`, and consults it through the real `PlanContext` cost-model path.
`PREFER_INDEX` and `PREFER_PRIMARY_KEY_INDEX` mirror Java's index-preferring branch. Direct tests pin
all three values and the config-read path; RFC-190.11 additionally pins a full-comparator winner flip.

### [x] Finding 3 (HIGH, worse plan) — comparePrimaryScanVsIndexScan drops Java's type-filter SARG subcase — DONE
`planning_cost_model.go` ported only the shape check and dropped Java's SARG sub-case: when the primary
side carries a type filter, the index side none, and the index SARGs strictly more than the primary,
Java prefers the INDEX. FIXED: added `primaryVsIndexVerdict` (SARG sub-case + PREFER_SCAN default),
threading the sign by which side is the primary scan (Java's flipFlop negation). Comparison set built
from BARE comparisons (type + comparand, column/position EXCLUDED — Java's ComparisonsProperty
`Set<Comparison>`), via `scanSargComparisonSet`. RFC-189 F3 subsequently wired the full
`IndexScanPreference` config branch; RFC-190.11 pins both that branch and the strict-SARG-superset
branch through the full comparator.
**Reachability (corrected after codex P1a fold): INERT on the corpus.** The sub-case's SARG walk must
descend the CONCRETE plan (the production `scanPlanExpression`/`TypeFilter(Scan)` wrapper exposes no
quantifiers — `GetQuantifiers()==nil`), else the primary SARG set is spuriously empty and the sub-case
fires whenever the index has any SARG (could demote a PK point lookup). After that fix
(`scanSargComparisonSet` → `collectPlanSargComparisons` over `GetRecordQueryPlan().GetChildren()`), the
earlier `join_optimization_probes#4` "flip" REVERTS: the EMP self-join primary SARGs an eid inequality
the index lacks while the index SARGs the did equality the primary lacks — NEITHER is a strict superset,
so the sub-case correctly abstains → PREFER_SCAN (matches master, matches Java). The genuine strict-
superset shape (index SARGs everything the primary does plus a record-type-key comparison, no type
filter) is not SQL-corpus-reachable today. Pins: unit `TestComparisonSetKey_BareComparisonIdentity` /
`TestSetDifferenceEmpty` (the set logic); `join_optimization_probes#4` keeps a rows-only correctness pin
(the sub-case correctly does NOT fire there). Faithful Java port, latent-gap like M2/M3/M5.

### [x] Finding 5 (MED) — scalar signed-zero ConstantValue: equal-but-different-hash — DONE
Landed in `8fcb1a426`: scalar float equality no longer falls through to Go `==`, so −0.0 and +0.0
remain distinct exactly like Java and no longer compare equal while `%v` hashes them apart. RFC-190
review tightened the implementation from raw bits to Java's canonical Float/Double bit identity:
the zero sign remains significant, while every NaN payload/sign encoding compares equal and hashes
coherently. `TestConstantValue_SignedZeroEqualsIsHashConsistent` covers both widths, signed zero,
ordinary equality, and distinct-payload NaNs.

### [x] Finding 6 (MED) — comparePredicateCountByLevel: keep the antisymmetric UNION iteration — DONE (self-corrected)
Original claim (walk a's keys first-map-only to "match Java") was WRONG — it rested on misreading Java as
having a SPARSE producer. Java's `PredicateCountByLevelVisitor.evaluateAtExpression` ALWAYS
`.put(currentLevel, count)` (0 for a non-predicate node) → its maps are DENSE, so "iterate a's entries" =
iterate ALL levels = the ascending UNION pass. On Go's SPARSE producer
(`designationScope.predCountByLevel`), a first-map-only pass is NON-ANTISYMMETRIC (`Filter(Distinct(Scan))`
{2:1} vs `Distinct(Filter(Scan))` {1:1} → +1 in BOTH orientations → REWRITING survivor becomes
insertion-order dependent, a determinism bug) AND diverges from Java (returns +1 where Java returns -1).
Reverted to the UNION iteration (= master), which is antisymmetric on any input and equals Java's per-level
counts on sparse input. Pin: `TestComparePredicateCountByLevel_Antisymmetric` (compare(a,b)==-compare(b,a)
incl. the {2:1}/{1:1} trap) + `_SanityCases`. Explain-diff vs master: zero (this was a no-op-vs-master
correction of a mistaken change).

### [ ] Finding 6-followup — dense predicate-count producer for Java tiebreak parity
Go's `predCountByLevel` is SPARSE, so the highest-level tiebreak (`intCompare(maxLevelA, maxLevelB)`) uses
the highest PREDICATE level, not Java's tree-depth `getHighestLevel` (dense). Only bites when every
per-level count ties (rare). Making the producer dense (always `counts[currentLevel]+=predCount`, 0
included — matching Java's visitor) closes it, but FLIPS ~11 corpus plan lines (REWRITING survivors:
e.g. a nested redundant Project, a Limit/Project reorder) that each need Java-verification before shipping.
Pre-existing (master's producer is sparse too); booked, not bundled into RFC-188.

### [ ] Finding 7 (MED, fragile) — Quantifier.GetCorrelatedTo() returns empty; Java transitive-walks
`expressions/quantifier.go:250` returns `{}` vs Java `getRangesOver().getCorrelatedTo()`. UNDER-
approximation (dangerous direction) — any new consumer treats a correlated leg as free-standing (0-row
class). Contained today (consumers rewired), latent trap.

### [x] Finding 8 (MED, latent hang) — merge cycle guard walks all members — DONE
Landed in `eac6ef9ab`: `reachable` traverses `AllMembers()` rather than relying on the undocumented
"every final is exploratory" invariant. `TestMemoMerge_SkipsCyclicMergeThroughFinal` constructs an
ancestor edge visible only through a distinct final member and proves `mergeable` declines the
self-cycle.

### [ ] Finding 9 (MED, latent) — TranslateQueryValueMaybe pulls up candidate sub-values against themselves
`max_match_map.go:771` `PullUpValue(entry.candidateValue, entry.candidateValue, alias)` collapses every
candidate part to `QOV(alias)` (case-1 self-equal always fires) vs Java's root-relative
`candidateValue.pullUp(mapping.values(), …)`. Masked until a covering-index `RecordConstructorValue`
result value appears → wrong projection. `PullUpValues(parts, m.candidateValue, alias)` exists unused.

### [x] Finding 10 (MED) — missing Java cost rungs / property divergences — DONE (M2 M5 M4 M3)
- [x] M2: `numDefaultOnEmpty` rung — DONE. Count `RecordQueryDefaultOnEmptyPlan` in all 3 count sites
  (walk/merge/concrete); rung "fewer ON EMPTY NULL wins" after the ordinal rungs, before the scalar-cost
  extension (Java's last ordinal rung before the planHash tiebreak). Pin:
  `TestConcretePlanCounts_DefaultOnEmpty`. Explain-diff: no corpus flip (faithful port, latent-gap fix).
- [x] M3: whole-plan-cardinality OUTER guard — DONE. Criterion #2 (max data-access cardinality) is now
  gated behind `wholePlanMaxCardinalityKnown(a) || ...(b)` — the PROVEN whole-plan max cardinality
  (`computeCardinalities().GetMaxCardinality()`), Java `PlanningCostModel`'s outer guard. When both
  whole-plan maxima are unknown but a data access is provably bounded (InUnion/Explode over point
  lookups), Go now abstains like Java instead of ranking on the data-access maximum. Pin:
  `TestWholePlanMaxCardinalityKnown` (scan→unknown, FirstOrDefault→known — proves the guard
  discriminates). Explain-diff: no corpus flip (reachability caveat resolved — no spurious over-abstention;
  the divergence case is not in the corpus).
- [x] M4: DistinctRecords for index — DONE. `computeDistinctRecords` now returns
  `!matchCandidate.createsDuplicates()` (Java `DistinctRecordsProperty.visitIndexPlan`), not `IsUnique()`.
  Plumbed the candidate fan-out signal onto `RecordQueryIndexPlan` (`createsDuplicates` +
  `distinctRecordsKnown` fields, `WithDistinctRecordsSignal`, `ProducesDistinctRecords`), stamped at all
  production index-plan build sites (stampIndexMetadata, wrapScanPlanWithCoverage×2, streaming-agg,
  ordered-index-scan) via `candidateDistinctSignal`. Empty-candidate → false (Java default). A non-unique
  SCALAR index is now correctly distinct. Pin: `TestComputeDistinctRecords_IndexFanOutSignal` (4 cases).
  **Graefe NAK fold:** the candidate's `createsDuplicates` was NEVER populated (constructor omitted it →
  constant false → fan-out indexes over-reported distinct = UNSAFE dropped-dedup direction). Fixed:
  exported `Index.CreatesDuplicates()` (over the root key expression, `createsDuplicates(RootExpression)`),
  optional `IndexDefWithCreatesDuplicates` interface, `metadataIndexDef.IndexCreatesDuplicates()`, threaded
  through the candidate constructor. Pins: `TestIndex_CreatesDuplicates` (FanOut→true, scalar/nested/empty),
  `TestPlanContext_ThreadsCreatesDuplicates` (fan-out def → candidate true; absent → false). Explain-diff:
  no corpus flip (no fan-out index in the SQL corpus).
- [~] M5: PrimaryKey for index scans — REVERTED to nil (safe); re-booked as structural. The by-COLUMN-NAME
  PK (`GetPKColumnNames()` → FieldValues) wrongly equates record types whose PK EXPRESSIONS differ but
  share field names (`Field("ID")` vs `Concat(RecordTypeKey(), Field("ID"))` both flatten to `["ID"]`),
  which would let `ImplementDistinctUnionRule` dedup two legs that must both survive (dropped rows —
  codex P1b, a finding-A-class leaf-name bug). `computePrimaryKey` now returns nil for index scans (safe
  under-report: disables the optimization, never wrong dedup). Pin:
  `TestComputePrimaryKey_IndexScanIsNilPendingStructuralPK`. See Finding 10-M5-followup.

### [ ] Finding 10-M5-followup — structural common-PK for index scans (Java parity)
Port Java `PrimaryKeyProperty.visitIndexPlan` PROPERLY: `ScalarTranslationVisitor.translateKeyExpression(
index.getCommonPrimaryKey(), …)` — translate the index's common PK KEY EXPRESSION to Values that encode
STRUCTURE (record-type-key prefixes, nesting), not bare column names. Then `commonPKFromChildren`'s
`ValuesStructurallyEqual` compares real PK identity and cannot equate `Field("ID")` with
`Concat(RecordTypeKey(), Field("ID"))`. Finding-A-class (carry the key expression / translated Values on
the plan; also handles the fan-out case — a fan-out index's entries DO carry the common PK, so it should
be surfaced for the PK property while still suppressed for the ordering suffix). Needs Graefe+Torvalds.

### [ ] Finding 10-M4-followup — Fetch stored-record transparency to activate the non-covering DISTINCT elision (codex P2)
`computeStoredRecord` has no `RecordQueryFetchFromPartialRecordPlan` arm (Java StoredRecordProperty treats
a fetch as producing a stored record → true), so `ImplementDistinctFinalRule` filters out the
`Fetch(IndexScan)` partition and the M4 non-unique-scalar-index DISTINCT elision does not fire on the
common non-covering path. Adding the arm is Java-faithful BUT activates the elision broadly: explain-diff
showed ~48 changed plan lines — `InMemorySort([ID], Fetch(InJoin(IndexScan)))` → `InUnion(IndexScan)` on
IN-list queries (sort elimination, apparent improvements). Deferred from RFC-188 because it opens a NEW
optimization surface that needs its own validation (row-level + EXPLAIN review of every flip + 1M stress
+ reviewer sign-off), not a milestone-end add. The distinct/PK/cardinality Fetch-transparency arms
already landed (safe, zero-flip); this is the storedRecord arm that turns them on.

### [ ] Finding 10-M4-followup-2 — fail-closed distinct for an UNKNOWN index-root key expression (codex)
`createsDuplicates`'s default returns false (shared with `validateSplitKeyExpression`, which uses it as
POSITIVE proof — a fail-closed `true` there wrongly admits `Split(customScalar)` that master rejects). So
an index whose RootExpression is an UNRECOGNIZED (custom/external) KeyExpression is stamped known
non-fan-out → M4 distinct=true (theoretically unsafe). NOT reachable today: every proto/SQL index root is
a recognized type. The M4 fail-closed need must be DECOUPLED from the shared default — e.g.
`index.CreatesDuplicates()` (or `metadataIndexDef.IndexCreatesDuplicates`) returns UNKNOWN (→ don't stamp
→ distinct=false, safe) for a root type not in the recognized set, without touching the Split-validation
default. Or add fan-out to the KeyExpression interface contract (Java's approach: abstract method).

### [ ] Finding 10-M4-followup-3 — refine the non-VALUE fail-closed (optimization) + audit value-candidacy vs Java
`Index.CreatesDuplicates()` fails closed to duplicate-producing for ANY non-VALUE index type that reaches a
value-scan candidate (TEXT tokenizes → dup; the conservative default also covers rank/multidimensional/
time_window_leaderboard/non-atomic bitmap). This is SAFE (identical rows cross-engine; at most a redundant
DISTINCT) but may OVER-report duplicates for a genuinely-distinct non-VALUE type (e.g. a VERSION index —
one entry per record), losing a DISTINCT elision. Refinement (safe, optimization-only): per-type analysis
of which non-VALUE types' SCANS actually emit multiple entries per record, narrowing the fail-closed to
those. Deeper (Java-parity): should Go EXCLUDE these types from value candidates entirely (as it already
does for aggregate/vector/atomic-mutation), matching Java, rather than value-scanning them — surfaced by
the M4 signal but pre-existing. Not a correctness gap now (fail-closed is safe); cross-engine reachable
(Java-created index in shared metadata).

### [ ] Finding 10-M3-followup — recognize equality-bound primary scans in the outer guard (codex P2)
`wholePlanMaxCardinalityKnown` uses `computeCardinalities`, whose `RecordQueryScanPlan` arm always returns
`UnknownMaxCardinality` — so a full-PK-equality primary scan (a point lookup, max=1, which
`concretePlanCounts`/`scanProvableMaxCard` DO bound) is reported unknown, and the M3 outer guard
over-abstains for a bounded-primary-scan-vs-unbounded comparison where Java would apply criterion #2.
Safe direction (abstain → falls through to other rungs); zero corpus impact. Fix: bound a full-PK-equality
scan in `computeCardinalities` (needs the PK column count → thread `ctx`, like `scanProvableMaxCard`).

**Codex milestone-review fold (finding B):** codex NAK surfaced 1 P1 + 3 P2.
- [x] P1 (fixed): absent `IndexDefWithCreatesDuplicates` was read as known non-fan-out (stamped
  distinct=true) — a fan-out index whose def omits the signal would over-report distinct. Fixed with a
  known/unknown tri-state: candidate carries `createsDuplicatesKnown`; the constructor takes a `*bool`
  signal (nil = unknown); `DistinctRecordsSignal()` returns nil when unknown → property abstains to
  distinct=false (safe). Pin: `TestPlanContext_ThreadsCreatesDuplicates` (plain def → nil signal).
- [x] P2 Fetch transparency (fixed): `RecordQueryFetchFromPartialRecordPlan` is 1:1 but was absent from the
  DistinctRecords/PrimaryKey/Cardinalities switches (fell through to default), hiding M4/M5/M3 above the
  common `Fetch(IndexScan)`. Added the transparent arm to all three (Java treats Fetch transparent). Pin:
  `TestComputeDistinctRecords_FetchIsTransparent`. Explain-diff: no corpus flip.

(Round-1 codex P2s resolved/superseded: the "carry index common PK separately from the ordering suffix"
item is folded into Finding 10-M5-followup — with M5 reverted to nil, the fan-out-PK concern is subsumed
by the structural-PK re-port, which must surface the PK for the property while suppressing it in ordering.
The "SARG walk uses bestPhysicalChild not the winner" item is RESOLVED: the P1a fold rewrote
`scanSargComparisonSet` to walk `GetRecordQueryPlan().GetChildren()` — the concrete plan being compared —
so there is no ref child-selection left in the SARG walk.)

### [ ] Finding 11 (MED) — PredicateToLogicalUnionRule expands every top-level OR
`rule_predicate_to_logical_union.go:99` fires OR→`Distinct(Union)` unconditionally; Java
`PredicateToLogicalUnionRule:197` only expands index-exploitable ORs (`partiallyMatchedOrs`). Memo bloat
+ worse-plan risk for `a=1 OR b=2` with no indexes.

### [ ] Finding 12 (LOW-MED, latent) — value-layer / compensation gaps
- `values/value_in.go:52` value-layer IN promotes both int64→float64 → loses precision >2^53 (disagrees
  with exact predicate-layer IN); `:55`/`value_array_distinct.go:101` `==` panics on non-comparable
  slice/map elements.
- `values/values.go:3365,3394,3519` `CAST(string AS INT/LONG/DOUBLE)` `TrimSpace` where Java rejects.
- `compensation.go:1035` `ForMatchCompensation.Union` picks c's rcf even when only other's is needed
  (Java `Verify.verify` both) → wrong output shape; `:151` `ComputeResultCompensation` hardcodes
  `EmptyGroupByMappings` vs Java's `pullUpAggregateCandidateMappings`. Both latent (single-child gated).
- `predicates/predicates.go:117` `PredicateEquals` has no `ExistentialValuePredicate` case →
  EXISTS AND EXISTS never deduped (optimization only; memo interning DOES handle it — inconsistent).

### [ ] Finding 13 (LOW) — dead code / missed matches / maintainability
- Dead rules: `rule_implement_intersection.go:46`, `rule_intersection_merge.go:37`,
  `rule_set_op_singleton.go:49` (no query seeds `LogicalIntersectionExpression`). RFC-190.5b resolves
  the intersection rule's latent unordered-child bug with an explicit ASC request and ordered-winner
  selection; the rule remains dead/maintainability-only.
- `rule_merge_fetch_into_covering_index.go` unreachable (`wrapScanPlanWithCoverage` strips the Fetch) —
  unported Java optimization.
- `rule_match_intermediate.go:227` MatchIntermediateRule pairs quantifiers POSITIONALLY; Java enumerates
  permutations → silently misses index matches for permuted multi-quantifier expressions.
- `matched_ordering_part.go:193` `Demote()` dead + panics; `max_match_map.go:643`
  `findMatchingReachableCandidate` dead.
- `expression_partition.go:69` `PlanPartition.GetPlans()` not index-aligned with `GetExpressions()`
  (footgun, bit `rule_implement_in_join` once); `:30` `NewPlanPartition` map-iteration nondeterminism (dead path).
- `unified_tasks.go:675/696` `isExploratoryMember` ≡ `isAlreadyExploratoryMember` (byte-identical dup);
  O(n) membership scans in per-round loops (O(n²)/group); cost comparator re-walks whole subtrees uncached.
- Maintainability: the NLJ rule remains ~3.5k LOC vs Java 331 (10×, #1 churn file, hand-rolled
  existential/buried-leg subsystem), though RFC-190.7 centralized its repeated existential
  compensation tail; ~40% comment density carries reverted-attempt process narrative (violates
  CLAUDE.md comment guidance).

---

# ⛔ ALL WORK FROZEN — sole priority is RFC-173 (ordinal/group column-resolution migration)

**Owner directive (2026-07-01): pause ALL other project work until RFC-173 lands.** Do NOT pick up
any item below, any handover follow-up, or any new hunt — RFC-173 (`rfcs/173-ordinal-column-resolution.md`)
is the exclusive focus. It retires the name-based `AnchoredJoin` column model for Java's ordinal/group
model (the RFC-164 WS-2 root fix): one RFC, **staged merged PRs** (precursors P1/P2/P3/Slice 1 each
merge independently; atomic Slice 3 as its own PR), RFC-ack → per-slice re-ack. Foundational,
**~25–30 shifts**. See RFC-173 §4 for the slice order and §5 for the (execution-pin, not dark-diff)
validation gate.

### RFC-173 S4 CAP — current state (checkpoint 2026-07-13, branch feat/rfc173-s4)

**The name model is DEAD for query OUTPUT and nearly dead for the plumbing.** Definitive findings
this session (all committed: `73753d722` 3 gaps, `b54ec7522` scalar, `e7a662b41` union-agg + probe):

- **`resultset.go` is already 100% Positional** (0 `.Datum` reads) — the final client-facing
  materialization is ordinal-only. Consequence: the §5 **dual-window differential is effectively
  already retired** — its NAME side (DisablePositionalEmission=true) can no longer materialize output
  (`resultset` errors "result row carries no positional output row"). It is NOT a usable net anymore;
  do not treat its red as a live regression. The **`RequirePositional` probe** (added this session,
  `executor.evaluation_context.go`, inert by default) is its replacement: armed, it makes every
  live-side name-model consumption arm (filter/predicatesFilter/map/projection/aggregate) loud, naming
  the site — green-under-armed-probe == that shape is ordinalized. Use it as the forcing function.

- **3 ordinalization gaps CLOSED:** (1) EXISTS-in-INNER-JOIN-ON now folds into the WHERE-EXISTS gather
  (Java parity — `translateProject`); (2) B1 N-way WHERE-EXISTS confirmed already ordinal (the earlier
  "failures" were parallel-pollution from the failing gap-3 test); (3) FULL-box chained-unnest straddle
  loud-rejects (`chainedSpineBottomsInFullBox`, Java rejects FULL OUTER JOIN at the grammar level).
- **scalar-subquery extraction** and **aggregate-over-UNION/UnorderedUnion/RecursiveLevelUnion** are
  ordinalized (the latter cleared ~13 tests via `aggregateInputIsFlatFrontier`).

**CLEARED since the checkpoint (commit `44d51c877`, all 6 real suites green):**
- Aggregate/consumer over a bare-projected JOIN or **recursive-CTE** branch — `executeProjection`/
  `executeMap` now emit the ordinal Positional for a BARE + UNIQUELY-named output (dup bare names stay
  name-model — a flat row can't disambiguate `SELECT c.name, p.name`→[NAME,NAME]); `aggregateEvalArg`
  resolves against any flat Positional the row carries; `aggregateInputIsFlatFrontier` peels the union
  plans. `PositionalRow.GetByName` strips a self-qualifier (`V.X`→`X`) when the leaf is UNIQUE.
- Booked flip-sentinels landed (GraefeImplProbe2): Q51 absent-column read → LOUD (not silent NULL);
  Q54 qualified read → resolves `1|1` (matches sibling Q52); Q5 passes.

**REMAINING name-model surface — 3 birth-disabled reach shapes (armed-probe sweep):**
1. **`ThreeLinkFilteredOrdinalizes/buried_2chain_straddle`** — `SELECT Y FROM T4, T4.SARR AS X, X.SUB AS Y,
   T4 AS T4C WHERE T4.ID = Y`: a chained unnest ENCLOSED as a leg of a larger cluster (+T4C), with a
   straddle predicate. The enclosed-chain MERGE stays name-model (the cluster-gate's enclosure decline,
   `buildUnnestResultValue` → `NewAnchoredJoinRecord`). **Java-SUPPORTED → must ordinalize** (the
   enclosed-inner-cluster ordinalization the gate defers to "item 3"). DEEP.
2. **`GraefeImplProbe2/Q5_star_body_enclosed`** — `WITH S AS (SELECT * FROM la, la.arr AS x) SELECT S.K,
   S.X ...`: `SELECT *` over a lateral unnest keeps QUALIFIED multi-source names, so the derived body
   stays name-model (`derivedBodyOpaqueOrdinalLeg` admits only bare-projected bodies). **Java-SUPPORTED →
   must ordinalize** (SELECT*-multi-source). DEEP. (Passes today via the name path; the probe flags it.)
3. **`BareTwinGather/grouped_over_name_model_fallback`** — `SELECT X, COUNT(*) FROM A FULL OUTER JOIN B …,
   A.ARR AS X WHERE A.K > (subquery) GROUP BY X`: FULL-box + unnest + Unbakeable-subquery-conjunct. FULL
   OUTER JOIN is **Java-UNSUPPORTED → loud-reject candidate** (like gap 1's chained FULL-box straddle;
   the single-unnest analog needs a targeted reject in `translateUnnestJoin` that does NOT catch the c5a
   FULL-box shapes that DO ordinalize).

**To finish (Datum=0):** ordinalize (1) [enclosed-chain merge] and (2) [SELECT*-multi-source]; loud-reject
(3) [FULL-box+subquery]. Then delete `QueryResult.Datum` + the internal-plumbing name arms (guarded by
`requirePositional`) + the dead §5 oracle machinery (`DisablePositionalEmission`/`SetNameModelOracle`/
`OracleBakedNameFallback`/dualwindow pkg) + `buildUnnestResultValue`/`AnchoredJoin`. The two DEEP
ordinalizations touch guarded planner/enclosure invariants → **need a Graefe ACK** before merge. The
`RequirePositional` probe (arm `var RequirePositional = true`) is the forcing function; the 6 real FDB
suites are the correctness authority (the §5 dual-window is already retired — resultset is Positional-only).

### RFC-173 progress (slice tracker)

- [x] **S4 EXISTS-composition (collision-mint) sub-slice — CLOSED, fully 4-gated on `bcfff218c`
  (2026-07-10).** 12 commits, 8 gate rounds, every round a real find. Landed: the collision MINT
  (single-table correlated EXISTS inners born under unique identities — the R5o inner-shadow
  conformance fix + those shapes ordinalize), the clean-path guard narrowing, the multi-source
  scope-ambiguity decline with the full polarity/consumption-mode guard (four silent-wrong classes
  fixed, all sentinels live-Java-grounded with flip rows recorded), the subquery-alias distinct
  minting (both directions of the identity-collision namespace), and the mint-collision hardening.
  See the RFC's EXISTS-COMPOSITION entries for the arc record and the (e)-(j) booked residuals.
- [x] **RFC** — `rfcs/173-ordinal-column-resolution.md`, all-four-acked, merged (#422).
- [x] **P1 — ordinal `FieldPath` substrate** (dark): `FieldValue.resolveOrdinal` + `RecordType.FieldIndex`
  (list-position = Java ordinal) + `NewRecordType` normalises `Fields[i].Ordinal == i`. All-four-acked,
  merged (#423, `a20794e9b`).
- [~] **P2 — positional/typed runtime row** (in gauntlet, PR #427). Typed positional row emitted
  alongside the name-keyed `map[string]any` by the NON-JOIN producers (scans, covering index,
  projections; filters pass it through free) + the `PositionalRow` substrate + `shadowMismatch`.
  **Scope (gauntlet-agreed):** the JOIN/lateral producers (`mergeRows`/`flatmap`/`explode`) and the
  outer-join null-extension primitive (`appendNullLeg`) move to **Slice 2/3** (they're restructured
  positional-native there; dual-emitting over the doomed AnchoredJoin merge would be throwaway).
  **Deferred to Slice 1** (before the ordinal path goes live): (i) the dual-emission per-row cost
  benchmark (RFC §4 P2 hard part); (ii) [@claude] a dedicated **e2e shadow test for the projection
  producer** (`executeProjection`), analogous to `TestBuildCoveringRow_ShadowAndCollision_RFC173P2`
  — projection's `slots[i]=val` has no index arithmetic so risk is low, but pin it before Slice 1
  makes ordinal access authoritative. **Carry-forward:** (a) [Graefe] when a resolution path becomes
  AUTHORITATIVE, escalate `resolveOrdinal`'s absent-field / non-record decline from silent
  `(0,false)` to Java's `SemanticException`; (b) [@claude] `RecordType.FieldIndex` and `LookupField`
  are near-duplicate scans — dedup (`LookupField` → `FieldIndex` + index) when a slice touches both.
- [x] **P3 — alias-bijection interning → FOLDED INTO SLICE 3** (gauntlet call, PR #429 NOT merged:
  Graefe + Torvalds + codex all ACK-with-fold; @claude n/a). The dark-shadow spike proved the
  mechanism (tier-3 predicate minus the `aliasAware` gate → `would=true` == the flip's extra dedup)
  and quantified it (≈259 extra dedups / 1500 planned corpus exprs — an **approximate, Insert-only
  under-count**, not a pinned number). But the observer is a nil-in-prod hook + an unasserted `t.Logf`
  — transitional scaffolding deleted at the flip, so it lands **with its Slice 3 consumer**, not
  stranded ahead of it. Spike preserved on `feat/rfc173-p3-bijection-interning`. **Slice 3 owes:**
  (i) build the global bijection tier live; (ii) **assert** shadow-predicted-delta == actual
  member-count-delta (the pin the spike omitted); (iii) certify no CTE-rename NULL-read via §5
  execution pins + RFC-077 task-count baseline (safety is flip-live-gated, un-shadowable). Full
  analysis banked in RFC §4 P3.
  - [x] (ii) **shadow-delta pin DONE** (`TestRFC173S3_AliasAwareInterningShadowDelta`, branch
    `feat/rfc173-slice1-ordinal-nonjoin`): the exact form is shadow (per-`Reference.AliasAwareDedups`)
    ↔ measured member-count delta (via `SetDisableAliasAwareInterning`), NOT the naive equality —
    cascade makes delta > shadow (3-chain 4→20, 4-chain load-bearing: converges on / blows budget off).
  - [ ] Follow-up (non-blocking, Torvalds ACK w/ next-slice defer): `disableAliasAwareInterning` is a
    test-only process-global read in Insert's hot loop. Race-free today (non-parallel test, -race
    enforced), but thread it through per-Memo state (like `mergeAliasCounter`) so no global is read in
    Insert — deferred because `Reference.Insert` has no Memo handle. Do when a slice gives Insert Memo
    context (the alias-aware tier itself survives S4, so this is not auto-retired).
  - [ ] Follow-up (W4b review residuals, master-parity — broken on master too, pinned as clean
    errors): (a) aggregate-local outer refs in a correlated scalar (`SUM(o.amount + e.ref)` over a
    clustered outer) never planned — the true enablement is an EXECUTION-CONTEXT property (the
    aggregate cursor inheriting the outer binding through the NLJ context, as Java threads it), not an
    expression rewrite (Graefe ruling on the codex P2-2 fix); (b) box-leg clusters (nested FULL boxes)
    stay on the pre-W4b path even when gated (@claude scope note — intended W4b narrowing); (c) the
    UPDATE-transform row-context site (`executor.go` ~:2754) still gates the binder on the old
    params/scalar-subqueries-only condition — the same class round 5 fixed for projections
    (`hasBindingContext`); PROBED: unreachable through SQL today — a subquery in UPDATE ... SET never
    reached the binder at all (the builder wrote the LITERAL SUBQUERY TEXT into the row — silent data
    corruption, on master too; now guarded with a clean 0AF00 decline + a no-mutation pin). Align the
    site with the shared predicate whenever subquery-in-SET support lands.
    FIXED in the gauntlet (not residuals): parenthesized-column-over-JOIN-inner (qualified scalarCol
    from the walked QOV; the ambiguous-column probe returned the WRONG LEG before the fix — pinned);
    OUTER-scope parenthesized columns (scope discrimination via the binder-exact innerSourceAliases;
    materialized, keyed by the executor's shared naming contract — a wrong-scope read before the fix);
    the unnest AT-alias scope corner (the collector mirrors unnestSourceCorrelation: AS-else-AT-only);
    the name-model projection binder gap (round 5 — outer refs over Datum rows silently NULLed).
- [x] **Slice 1 non-join ordinal — MERGED (#437, `12516e33f`), all four gates ACKed at the merge
  HEAD** (Graefe impl+delta ACK · Torvalds fix-first→ACK incl. MUTATION-TESTING the authority proof ·
  codex clean (P2 fixed; delta finding = documented Go≥1.22 loopvar false positive) · @claude "nothing
  blocks merge"). Ordinal resolution authoritative on the non-join frontier; buried-reference
  reverse-map retired (4 RFC-082 divergences lifted, real-Java-validated); §5 dual-window differential
  standing (1617 entries, first catch fixed: recursive-CTE computed-column silent-wrong, known-red
  lock shrank); dual-emission benchmark satisfied (+71% window, `positionalTypeCache` → Slice 4 ends
  net-faster); 1M stress FASTER than pre-merge master (scan_all_wide 4.52s vs 5.57s — the cache repays
  the window). Full execution log: RFC §4 Slice 1.
  - [ ] Follow-up (non-blocking, PRE-EXISTING, recursive-CTE re-keying area): (a) un-aliased QUALIFIED
    seed projection folds a qualified name into `outCols`; (c) LENGTH-MISMATCHED column-alias list is
    silently ignored rather than a loud error (SQL says error). Fix when a slice touches the area.
  - [ ] Graefe recorded obligations live in RFC §4: Slice-4 kill list (sort-comparator fallback dies
    loud · dualwindow retires with the name map · `legPhysicalOutputNames` must not outlive the window ·
    bound `positionalTypeCache` dynamicpb leak) + Slices 2–3 oracle-gate rule (new birth sites extend
    `DisablePositionalEmission`).
- [x] **Slice 2 2-way wedge — DONE, MERGED as PR #447 (squash 7f7100199, 2026-07-02).** All four
  gates at the final HEAD: Graefe ✅ (design + impl), Torvalds ✅, codex ✅ (clean P1/P2/P3),
  @claude ✅ ("🟢 Clean"). Master moved twice mid-gauntlet (PR #446 recursive-CTE alias frontier +
  PR #450 RFC-176 P1); both merged in with the suite green. #446 had independently invented a
  SECOND baked-ordinal mechanism (`ResolvedOrdinal`/`HasResolvedOrdinal`, childless, quiet) —
  unified onto `ResolvedAccessor` in 51e3327ca with a `FrontierPinned` contract bit (Graefe
  pre-code ruling: bit on the accessor, NOT child-presence — passthrough copies strip Child;
  excluded from identity/hash/Explain; dies in S4). Watch-item banked in the S3 map: pinned vs
  unpinned equal-(field,ordinal) nodes are identity-equal but guard-different.
  - [x] **ENTRY GATE: the name-burial inventory — SATISFIED** (`rfcs/173-name-burial-inventory.md`,
    two-axis sweep, ~95 sites each slotted S2/S3/S4/S6). Key conclusions: ordinal frontier dies at
    `mergeRows`/`qualifyOuterRow`/union-remap/aggregate-output (S2/S3 re-birth + extend the oracle
    registry); `executeProjection` straddles until S3; `AnchoredJoin` flag is the linchpin (all read
    sites enumerated); `qualifyTypeFallback` exists (executor.go:2140 — the "not found" was a
    directory-scope artifact).
  - [x] **W1 baked-ordinal FieldValue substrate (dark)** — `Resolved *ResolvedAccessor`,
    `NewFieldValueOfOrdinal`, (name, ordinal) identity for baked / baked≠lazy, marker through every
    copy site, loud `BakedNameContextError` on every name-keyed + unrecognized eval arm (both tails),
    `OracleBakedNameFallback` twin oracle behind `executor.SetNameModelOracle`, by-ordinal
    compose/push-down + lazy dup-name DECLINES, §5 dup-name identity pin. Graefe ACK ×2, Torvalds
    NAK→ACK ×3 (e74c4d192, ec739f83f, fd9d83636, 6168a56e9, 089979ea8).
  - [x] **W2 cluster-arity scoping gate + drift asserts (dark)** — `rfc173_cluster_gate.go`
    (clusterArity walk + per-seed `wedgeGate` decisions), `inInnerCluster` enclosure flag threaded
    through every leg-translation site, SelectMergeRule + rebaseBuriedLowerReferences panics (both
    pinned red→green), flattening-evasion + enclosure-matrix + HAVING-EXISTS pins. Two Graefe-confirmed
    errata vs the contract shorthand (subquery-carrying filter/project = poison; outer boxes gate
    unconditionally). Graefe ACK, Torvalds NAK→ACK (53ba2a9ac, 089979ea8).
  - [ ] **BOOKED (Graefe, RFC-173 S4 :3234 clean-lift condition 3): retire the mutable `inInnerCluster`
    field for a DOWNWARD enclosure-context PARAMETER (Volcano required-property style).** The
    `!t.inInnerCluster` read in `translateFilter` (`:2211`, the enclosed-gather rotation guard) is
    CALLER-CONTEXTUAL — "is this filter a merge leg or a fresh cluster root?" is caller state, NOT
    subtree-derivable, so there is no clean local tree predicate to replace it (unlike the join gate's
    `gatesAsFreshCluster`). The genuine decouple is threading enclosure context as an explicit downward
    parameter instead of a global mutable field (~15 threading sites). Separate architectural refactor;
    NOT a producer-retirement blocker — the `:3234` lift landed without it. Do AFTER the remaining
    enclosure setters are retired (it touches all of them).
  - [x] **Slice 2b: filtered-chained ordinalizes — DONE, FULLY 4-GATED on 60cbb0c08 (Graefe design+impl ACK, @claude ACK, Torvalds ACK, codex clean).** A chained lateral
    unnest under an ancestor WHERE (`FROM t, t.SARR AS x, x.SUB AS y WHERE <pred>`) now ORDINALIZES
    instead of declining to the name-model residual (buildUnnestResultValue → NewAnchoredJoinRecord).
    The coarse `chainedUnnestUnderFilter` "any filter suppresses" decline is RETIRED (field + set/restore
    + gate all deleted). Predicate placement is per-conjunct via the ⊆-outerLegs pushable-to-scan rebase
    gate (`rebaseChainedOuterLegPredicate`/`chainedPredScanPushable`): outer-col-only (correlated-to ⊆
    {t}) → SARG on Scan(t); anything referencing an in-chain correlation (x/y) → keep the rebase at the
    inner Explode. Axis-audited every filter shape (eq/AND/IN-list/BETWEEN/arithmetic/IS-NULL/NOT/
    multi-conjunct/straddling — all row-verified correct; end-to-end plan asserts pin the SARG/inner-filter
    placement). Cert: `TestFDB_RFC173S4_FilteredChained`. Retires 10+ name-model-caller invocations.
    B1 corpus (1641) green. TWO narrow residuals decline to name-model (correct-or-loud), booked below:
    (i) an OR in the filter (the name-key rebase strands the CNF-extracted pure-outer clause on the ordinal
    first-link row — @claude's NAK caught it; NARROW `chainedUnderOrFilter` bit declines OR filters to
    name-model, correct rows); (ii) a scalar subquery in the filter → LOUD `0A000` (typed, gated
    `len(f.ScalarSubqueries) > 0 && filterInputHasChainedUnnest(f.Input)` — the detector WALKS the join
    spine so a chained unnest buried behind a trailing table / join leg is caught too, not just the direct
    rightmost; stops at relation boundaries so an encapsulated derived-table chained unnest is NOT falsely
    gated). Pinned by `.../or_over_chained_declines_correct_rows` + `.../scalar_subquery_loud_0A000` +
    `.../scalar_subquery_buried_loud_0A000`.
  - [x] **Slice 2c: 2-chain OR ordinalizes via the POSITIONAL bake — DONE (Graefe design-ACK'd).** A
    filter OR over a 2-chain (`WHERE (t.id=10 AND y>2) OR (t.id=3 AND y<5)`) previously declined to
    name-model (the interim `chainedUnderOrFilter` bit): a name-key rebase of the whole OR is stranded by
    NormalizePredicatesRule (CNF) + PredicatePushDownRule (the pure-outer clause pushes to the first-link
    ORDINAL FlatMap where the name key resolves ordinal -1). Fix: the chained keep-branch bakes
    POSITIONALLY — `rebaseUnnestOuterLegPredicateOrdinal(p, ordType, ordType, …)` with
    `ordType = ordinalLegType(join.Left)` (the outer QOV's OWN type). Root cause of the earlier "5 vs 6
    DIVERGENT baked types" panic: it was a WRONG-TYPE-in-the-bake bug (passed the 6-field merged type;
    the outer QOV is the 5-field ordinalLegType), NOT an `OrdinalSeedLegWindows` disagreement — the
    authority + executor span + cross-agreement fixture are UNTOUCHED. `chainedUnderOrFilter` +
    `predicateContainsOr` DELETED (a decline retired). `:1161` narrows for free via line 404 (a declining
    shape keeps its subtree name-model). Cert: `TestFDB_RFC173S4_FilteredChained/or_over_chained_ordinalizes_correct_rows`.
    First cut (7950a6ed0) shipped a regression codex + @claude BOTH caught: the positional bake was gated
    on `isChainedUnnest`, so it fired on the name-model FALLBACK too — a mixed-inner-ref clause over a
    3+-link chain (or a buried 2-chain) baked `ofOrdinal(QOV)` against the name-keyed row → ordinal -1
    malformed plan. Fixed by an `ordinalSeed` seed-form discriminator (positional bake ONLY over an ordinal
    seed `!rc.AnchoredJoin`; a name-model seed keeps the name-key rebase) + the `TestFDB_RFC173S4_ThreeLinkFilteredNameModel`
    regression cert pinning that axis. FULLY 4-GATED on `4cde13db2`: Graefe re-ACK, Torvalds ACK, @claude ACK, codex clean.
    SCOPE (Graefe (A)): 2-CHAIN only. A 3+-link chain declines earlier (clusterArity poison, untouched)
    and stays name-model — its mixed-inner-ref STRAND lives in a DIFFERENT layer (pushBuriedUnnestPredicateDown
    + rewriteUnnestPredicate bake the deepest element against the 2-chain row). That deeper-nesting slice
    (below) owns lifting clusterArity + the placement fix + the FULL `:1161` retire. Scalar-subquery stays
    the separate `:2720`/0A000 residual (booked). Audit green: 2-chain OR ordinalizes, 3-link OR name-model,
    OR-with-scalar-subquery → 0A000, B1 (1641) green.
  - [x] **Slice 2d (deeper-nesting): LINEAR 3+-link chains ORDINALIZE (filtered + unfiltered) — DONE after a
    3-gate NAK round corrected the first cut.** The first cut (272b3e855) OVER-CLAIMED: only unfiltered chains
    ordinalized (the box-leg-conjunct arm of `unnestExistsSeedSafe` kicked every FILTERED chained base to
    name-model), its "SARG ⇒ ordinal" cert discriminator was FALSE (model-independent — the certs passed
    verbatim on the parent, caught by a neutered-gate control + a reviewer's independent parent-worktree
    control), and the unscoped gate admitted a FORK chain (`t.arr X, X.substruct Y, X.sub W` — owner two links
    back) that malformed-planned at ordinal -1 (reviewer kill, differentially verified). Corrective commit:
    (1) owner-LINEARITY at both walk levels (`u.Segments[0]==firstUnnest.Alias` + per-level check in
    `chainedBaseOrdinalizes`, which walks `j.Left` NOT firstBase — firstBase alone would miss a fork one level
    below the top); forks decline to name-model. (2) The box-leg-conjunct arm SCOPED via
    `unnestExistsSeedSafe(left, spineBase)` — `flag && !spineBase && multiAlias`; only the chained gate passes
    spineBase=admitted; every other decline arm stays live for spines. Filtered linear spines now GENUINELY
    ordinalize (trace-verified; the depth-3 straddle resolves through the 2c positional bake for real).
    (3) The boundary pinned WHITE-BOX in `rfc173_2d_chained_spine_seed_test.go` (seed RC `AnchoredJoin` flag
    off `translateChainedUnnestJoin`: linear 2/3/4-link × filtered/unfiltered → ordinal; top/mid-spine fork,
    box-base ±filter → name-model; twin-fork + 1-seg-mid-spine → loud upstream). ONE-TIME feature-off control:
    gate neutered → all six linear pins FAIL (discriminating by construction). Fork rows-certs e2e + honest
    cert rewrites (SQL certs are rows+placement pins and say so). STANDING DISCIPLINE (Graefe): a
    model-discriminator cert without a neutered-feature control run doesn't count. Accounting: box-base +
    `!ok` declines still reach `NewAnchoredJoinRecord` (chained producer NARROWS; zeros with c5b); `:1161`
    narrows (box/cluster readers remain). NEW ORTHOGONAL gap booked below: deep-WHERE 42703 (4+-link AS, any
    AT) — PRE-EXISTING semantic-resolver, reproduces name-model, upstream of translation.
    SECOND corrective round (two more gate kills on 59eb67aaa): a P1 silent-wrong — `clusterArity(FULL
    OUTER)==1` let a FULL-box-BOTTOMED spine pass spineBase=true, suppressing the box-leg-conjunct arm for
    genuine box legs (chained link ordinal OVER the first link's name-model seed → wrong A-side values);
    fixed by the walk returning (admitted, pureSpine) with pureSpine = `len(outerBoundAliases(bottom))==1`
    (admission unchanged — FULL-box bottoms still ordinalize unfiltered, pre-slice parity), pinned white-box
    + e2e (`fullbox_bottom_boxleg_filter`, the exact repro). And a false "pinned below" coverage claim in the
    white-box file (the walk's Segments<2 check had NO walk-test case — a reviewer control neutering it
    stayed green); the 1-segment walk case added. Cert harvest: depth-3 shadow-slot straddle `T4.SUB=Z`
    (positive slot-3 pin) ± OR, AT-first/AT-mid ordinal rows.
    FULLY 4-GATED on `c1f6e2059`: Graefe re-ACK, Torvalds ACK (controls all discriminating; live silent-wrong
    repro of the revert), @claude ACK (coherence by construction; three-way differential conclusive),
    codex clean. Residue landed in `6feebc150` (pureSpine rename + FullBoxChainedSpine cert), 4-gated.
  - [x] **FORK SLICE: fork spines ORDINALIZE via owner-slot rooting (Graefe design-ACK'd fork-first).**
    `chainedSpineWalk` peels the spine ONCE (links + admitted + pureSpine; ownership generalized to
    "resolves to exactly ONE deeper link" — forks admitted, table-owned/orphan/dup-alias declined
    defensively, dup 42712-loud upstream + pinned); `chainedOwnerElementSlot` roots the collection at the
    OWNER's element slot (`len(ordinalLegColumns(owner.join.Left))` — the layout law, AT-invariant, pinned
    per combination incl. the AT-only-upstream case). Purely translation-side. Controls at introduction:
    full feature-off → all four fork seed-form pins FAIL; MIS-ROOT control (tip slot) → the colliding-schema
    cert fails with exactly [W:100] (silent axis has teeth). Coupling cert (REQUIRED): fork-over-FULL-box +
    box-leg WHERE declines whole-chain via the pureSpine arm (white-box + e2e). P1's chained residual now
    {box-base, FULL-box-filtered, enclosed, defensive !ok} — NEXT: the box-substrate slice (box-base +
    box-leg-conjunct TOGETHER, axis-coupled), circular arms last.
    FULLY 4-GATED (impl 94f5b1ccc + comments-only ghost-fix e2bf12484): Graefe impl-ACK ("controls the
    strongest this arc has produced"), @claude ACK (prefix-stability coherence proof; adversarial variants
    all correct; independent teeth-check), Torvalds comments-NAK→ACK (seven chainedBaseOrdinalizes ghosts
    rewritten; controls F/F2/G all reproduced under his hand), codex clean ×2. Probe extras booked for a
    future touch: threebranch_fork, fork_owner_first_link_4deep, at_dense_fork pins, fork_over_fullbox_inner_only.
  - [ ] **BOX-SUBSTRATE slice (in de-risk; Graefe design consult round 3 in flight).** Commit 1 landed
    (b8441025a: the ordinalSlotInLegWindow fail-closed hardening + the NullSupplyBarrier cert). RECORD
    CORRECTION owed: commit 1's message attributes the spike's silent-wrong to "empty rt.Legs → flat
    fallback" — REFUTED by a layout probe (windows already propagate through chained merged types incl.
    box bottoms: [{A 0 3},{B 3 3},{X 6 1},{Y 7 1}]; a prototyped bottom-only propagation BROKE the 2c
    certs — per-link windows are load-bearing for runtime qualified-name resolution — reverted
    uncommitted). TRUE mechanism (trace-confirmed): the box-leg conjunct takes rebaseChainedOuterLegPredicate's
    LAZY fork ({B} ⊆ outerLegs "scan-pushable"), leaving a FOREIGN-correlation name read at the merged
    select; the runtime bare-name fallback over an ordinal row first-matches across legs → A's slot 0.
    The pureBottom lazy-fork gate is the correct semantic fix but NOT sufficient: with it, the bake is
    altitude+slot-correct yet evaluates NULL — the chained-over-box FlatMap's runtime binding does not
    flip positional in sync with the seed (the seed/birth mismatch class at the CHAINED FlatMap birth).
    ROUND-3/4 RESOLUTION (executor-birth mapping + gather:93 spike): gather:93 is NOT vestigial — its
    decline and the seed arm are a COUPLED PAIR (lifting either alone builds an ordinal seed over a box
    whose NLJ is not birthActive: ContainsBakedOrdinal on the box's OWN result value decides the birth,
    and adaptLegPositional's synthesis from a merged multi-namespace box Datum is unfaithful → silent
    NULL/empty). Even the full three-piece lift (gather:93 + arm + WHERE-merge bake) fails: TWO more
    uncoupled sites gate the simple case — legIsOrdinalSafe categorically rejects non-INNER legs
    (rule_implement_nested_loop_join.go:1106) and the FULL NLJ is built with sel.GetResultValue()
    VERBATIM (:166). LANDED (Outcome A): the coherence guard on the chained gate —
    `!pureSpine && !boxOuterBirthsPositional(bottom) → decline` — AMENDED per two independent review
    falsifications: NOT a demonstrated-bug fix (pre-guard rows were CORRECT on every probed observable —
    the cleared-enclosure first-link translate ordinalizes the nested box too, so the tower is coherently
    positional with dual emission backstopping) but a CONSERVATIVE guard over an UNVALIDATED tower
    (zero e2e coverage, outside the box slices' verified surface) that becomes load-bearing when the
    name model is deleted at the cap; pinned nested_box_bottom_* + feature-off control.
    FULLY 4-GATED (b8441025a + 90e9fbe82 + amendment 2095b0ce8): Graefe ACK→re-confirm (with ownership;
    NEW STANDING RULE: latent-bug-on-HEAD claims require a rows-probe+trace on the PRE-change tree),
    Torvalds NAK→ACK (theory #4 falsified by his instrumented probes; zero refuted-claim residue; the six
    tripwires "the real prize"), @claude ACK×2 (22-probe differential byte-identical; harvest faithful),
    codex clean×3. Arc: FOUR falsified theories, THREE revertible spikes, ZERO unsound code shipped.
    BOOKED (Outcome B, the real box-substrate ordinalization — sequenced WITH the circular arms, LAST):
    the 5-site checklist — (1) chained gate ↔ birth coupling (landed as A), (2) the enclosure declines
    cluster_gate.go:315/:356 (the circular arms), (3) legIsOrdinalSafe non-INNER widening, (4) the FULL
    NLJ ordinal-seed build, (5) unnestExistsSeedSafe box-leg-conjunct/pureSpine re-scoping + the
    pureBottom lazy-fork gate + the WHERE-merge ordinal bake (all documented, none landed standalone).
    Also booked: the below-FOD hoist ordinal-4/leg-row rebase gap (the nested-box strand) + the grouped
    consumer row-loss — both surfaced only under the lift, both stay declined today.
  - [ ] **REFRAME (Graefe Outcome-B design consult, EMPIRICALLY FALSIFIED the "flip the boxes" premise —
    design-NAK on "minimal box sub-slice that flips a production shape without touching Site 2").** Two
    reproducible probes: (a) a gate census on constructed logical trees — EVERY fresh (un-enclosed) box shape
    ALREADY gates ordinal (standalone FULL/LEFT/INNER, box-over-inner-cluster, box-over-outer-box); the ONLY
    declines are the two `inInnerCluster` arms (2a arityPoison, 2b name-model). (b) Runtime producer
    instrumentation across the whole box e2e suite + a wide RFC173|Join|Outer|Unnest|Lateral net: 30+ P4/P5
    firings, **100% `enclosed=true`, zero `enclosed=false`**, all green. So the residual name-model producer
    set is EXACTLY the `inInnerCluster==true` shapes — the "box substrate" is mislabeled: it is an ENCLOSURE
    problem and the enclosure ROOTS ARE NOT BOXES. `inInnerCluster` set-sites: translateUnnest:1180 (name-model
    unnest LOWERING fallback — dominant, every P5 rode it), translateJoin:4789 (propagation from an already-non-
    gated parent), the `ordinalEligible==false` roots = correlated-scalar-in-projection (the booked W4b
    clusterPullUp) + recursive-CTE, and translateUnnestJoin:1475 (unnest-over-box where boxOuterBirthsPositional
    is false). **Producer graph collapses to {P4 buildJoinResultValue:718, P5 buildUnnestResultValue:1660} +
    the birth predicate; P1/P2 re-enumeration:469/:567 fire only when parentIsMerge, P3 legColumns:347 only
    under a name-model parent — all THREE are DERIVED, retire for free when P4/P5 hit zero.** CORRECT sequencing
    INVERTS the framing: **starve `inInnerCluster` by ordinalizing the enclosure ROOTS — do NOT flip 2a/2b
    (they are dead-code-at-the-cap, not incremental flips).** (Corrected line refs: 2a rfc173_cluster_gate.go:383,
    2b :424 — the old :315/:356 are stale. Site 3 legIsOrdinalSafe rule_implement_nested_loop_join.go:1106,
    Site 4 verbatim RV :166/:240, Site 5 unnestExistsSeedSafe:881 / rebaseChainedOuterLegPredicate:2888.)
    Graefe atomic edges: E1 2a-flip ⟺ enclosing parent's producer (fixpoint, parent must ordinalize FIRST);
    E2 AXIS1 ⟺ AXIS2 is literally ONE predicate (:1475 box-birth bit == unnestExistsSeedSafe:914 read); E3
    seed ⟺ birth (newOrdinalJoinBirth decides purely from the box RV's own ContainsBakedOrdinal — no separate
    executor gate, so a WHERE-merge ordinal bake that leaves the FlatMap birth name-model is unsound); E4 Site3
    ⟺ Site4 only on the correlated-FlatMap FOLD path (reconstructFoldStep1Seed:1188), NOT the box's own birth
    (Site 4 :166 passes the ordinal RC through once the translator gates the box — Site-4 coupling was OVERSTATED
    for the plain box). SEQUENCE: **B0 (design-ACK, FIRST) = producer census GATE** — a permanent test running
    the dualwindow corpus asserting per-P4/P5-firing the shape class + `enclosed==true` (pure observation, zero
    prod cost via a nil-in-prod observer like forceOrdinalSpike; the "can we reach zero" instrument; building it
    ALSO independently re-verifies the reframe). **B1 (design-ACK conditional, first REAL flip) = unnest-over-
    single-clustered-LEFT/RIGHT-box birth** — relax boxOuterBirthsPositional:952 `clusterArity==1` (FULL-only)
    to admit a LEFT/RIGHT box that already gates standalone; CONDITIONS: seed-gate relaxation moves ATOMICALLY
    with birth admission (E2, one predicate) + §3 control red-under-sabotage before green. **NOT first:** the
    nested-outer-box leg (deepest, "unverified tower", max multi-namespace risk) — LAST. Falsification protocol:
    `executor.SetNameModelOracle(true)` neutered control; PRE-change rows-probe on HEAD both modes (agree+match-
    Java ⇒ NO latent bug, it's coverage — say so, the Outcome-A amendment lesson); discrimination REQUIRES
    dup-named legs + buried-leg-column + null-extended leg, then SABOTAGE-test the control (mis-key a leg window,
    confirm the differential goes red). HIGHEST falsification risk: multi-namespace box Datum — adaptLegPositional
    :628 partial-matches same-named legs → silently WRONG slot; the qr.Positional passthrough:620 guards ONLY
    when positionalMatchesLegType (ordered name agreement); a reordered/covering box row drops to the colliding
    Datum synthesis. Every box control MUST include dup-named legs + exercise the passthrough-fails path.
  - [x] **B0 LANDED — producer census gate + a CENSUS CORRECTION to the reframe.** Built the census
    (nil-in-prod observer on P4 buildJoinResultValue / P5 buildUnnestResultValue + a dualwindow-package gate:
    `TestFDB_RFC173_ProducerCensus`). Two findings: (1) the SeedRunCorpus (1641 executable §5 entries) produces
    **ZERO** name-model firings — the whole differential corpus is already fully ordinal (pinned as a regression
    sentinel). (2) **The reframe's "every residual P4/P5 firing is ENCLOSED" claim is EMPIRICALLY FALSE.** A
    multi-way (≥3) inner join UNDER a WHERE EXISTS declines its whole join subtree to name-model, and the TOP
    join is **UN-ENCLOSED** (`P4 enclosed=false Join|Scan`). Discriminated: plain 2-way inner, plain 3-way inner
    (NO exists), and correlated-scalar-in-projection ALL ordinalize (0 firings) — the WHERE EXISTS is the sole
    trigger. So the residual name-model set is NOT "exactly the inInnerCluster shapes": it also includes the
    **existential-over-multi-way-join OUTER join** (un-enclosed). The consult's box-suite instrumentation net
    missed this (it never ran a plain 3-way inner join under WHERE-EXISTS). **CONSEQUENCE FOR B1: do NOT assume
    all residual producers are enclosed; do NOT treat 2a/2b as the only residual.** The un-enclosed
    existential-over-multi-join is its own residual class (pinned as a flip-sentinel in the census probe;
    ordinalizing it is a separate slice — likely part of the EXISTS-composition arc, not the box substrate).
    Re-consult Graefe on B1 sequencing WITH this correction before building B1.
  - [x] **B1 = U-1 LANDED — FULLY 4-GATED on 7c4d3ea8f (2026-07-11): Torvalds ACK (r3) · Graefe ACK (r3) ·
    codex clean (r4) · @claude coherence ACK (final).** Six commits, four gate rounds, every round a real find
    (2 silent-NULL classes, 1 loud break, 1 side-effect leak, 3 doc-honesty violations — all fixed + pinned
    pre-merge; 16-subtest cert + census zero-firings pin + SARG plan-shape asserts). The mechanism
    (rfc173_b1_exists_gather.go, per the Graefe rebase-mechanism design-ACK): a plain WHERE-EXISTS over a gated
    arity≥3 non-dup INNER cluster routes through a WIDENED projection fold → `translateExistsOverGatheredCluster`
    builds the join + the WHERE's non-EXISTS conjuncts as its OWN gathered ordinal cluster
    (translateGatheredInnerCluster + extraPreds — SARG-preserving, separately enumerated), wraps it
    `[ForEach(box), Existential...]`, and REBASES every leg reference (the folded projection + each EXISTS
    correlation, rebaseLegRefsToBox) to ofOrdinal(QOV(box), window.Offset+idx) via values.OrdinalSeedLegWindows,
    with a post-walk declining any surviving leg-QOV ref (correct-or-decline). Plus the SelectMergeRule
    existential-wrap guard (>2-window positional-seed box under a single-ForEach existential parent stays nested —
    at this B1 checkpoint the flat form was only implementable MATERIALIZED because PartitionSelectRule was
    ForEach-only. RFC-190 later added guarded existential partitioning and retired the N-way arm; the guard remains
    to preserve the gathered wrapper's deliberate SARG boundary). VERIFIED:
    census 0 producers on the shape (was 2 — U-1 retired); correlated-index SARG green; B1 cert
    (TestFDB_RFC173S4_B1_NwayExists) — EXISTS→1st/3rd/4th-leg falsification, comma-join, NOT EXISTS, conjunct,
    ORDER-BY fail-open, SARG+not-cross-product plan shape; FULL suite 55/55 incl. the dualwindow differential.
    SCOPE (fail-open, each a booked follow-on): ORDER BY/LIMIT chains decline (the fold's chain re-application
    emits unrebased leg-qualified reads above the wrap — the LIVE scope-out is the translateProject gate's
    `len(chain) == 0` widening condition; existsFoldHasChain is only the defense-in-depth tripwire behind it);
    projected-EXISTS keeps
    buildExistentialJoinSelect (its FOD semantics not re-verified over the wrap); dup-alias declines loud.
    NAK ROUND (Graefe live-probe adjudication of the honesty flag + Torvalds reachability): the rebase was THREE
    channels — (a) EXISTS-correlation rebase LIVE/load-bearing (a -1 skew flips correlate_third_leg); (b) BARE
    projected columns DEAD (dotted frontier reads, no QOV — the +1 sabotage never executed; they resolved via the
    pre-existing S4-commit-2 name/window channel); (c) COMPUTED projections LIVE (+1 flips {2}→{101}). The killer:
    a MIXED projection (`SELECT p.id, p.id + r.rc … WHERE EXISTS`) → `(NULL, 101)` silent-wrong — the computed
    field's baked ordinals flipped the whole wrap cursor to birth-ordinal evaluation and the lazy bare read NULLed
    (the post-walk couldn't catch it: no QOV in a dotted read). FIXED via Graefe's preferred TOTAL rebase:
    rebaseLegRefsToBox now bakes QOV-shaped + DOTTED-frontier + unique-BARE-frontier reads, and wrapRVFullyBaked
    DECLINES any RV not uniformly baked (correct-or-decline restored; the file header is now true). Pinned:
    mixed_bare_and_computed_projection (1,101) + computed_projection {101}; the slot skew now flips a cert subtest
    (the bare channel is live). Torvalds: existsFoldHasChain is UNREACHABLE (the gate's len(chain)==0 is the live
    ORDER-BY scope-out; a chained fold implies projected-EXISTS which declines first) — kept, relabeled
    DEFENSE-IN-DEPTH tripwire per both reviewers. Multi-EXISTS now declines explicitly (was 0AF00-parity). Nits:
    concrete *SelectExpression assert; guard continue-safety comment. Full suite 55/55.
    ROUND 3 (Torvalds ACK + Graefe ACK + codex 2×P2): COV dropped from the whitelist (Graefe: not birth-evaluable,
    zero mints today — a pre-armed landmine); scalarSubqueries ROLLBACK on arm decline (codex: a post-translation
    decline left the nested uncorrelated scalar registered → the fallback re-registers → double pre-evaluation;
    fixed with a defer-truncate, pinned rows-level by outer_conjunct_with_nested_scalar → {2}). BOOKED follow-ons:
    (i) predicate-wrapper whitelist widening (codex P2: comparisons/IN/LIKE/CASE-WHEN conditions are wrapped in
    query/expr's predicateValue, which default-denies → those projections keep name-model, correct rows; widening
    needs the SAME Children()-completeness + Evaluate-purity verification Graefe applied to the 12 kinds — do NOT
    widen without it, two silent-NULL rounds prove why); (ii) a white-box assert that a DECLINED WIDENED FOLD
    leaves t.scalarSubqueries at its FOLD-entry length — measure at the FOLD level, not just the arm (the @claude
    coherence gate found translateProjectOverExistsFilter registers f.ScalarSubqueries at :3453 BEFORE the arm,
    so a widened-fold bail re-registers them via translateFilter:2321 → double pre-evaluation for
    `WHERE <scalar-conjunct> AND EXISTS(...)` shapes that decline; the arm-level rollback alone would pass while
    that leak persists — add a fold-level mark/truncate with the assert); (iii) **arity-2 comma+EXISTS SARG loss
    (PROMOTED from the superseded item so it isn't skipped)** — Graefe's directive stands: at minimum a plan-shape
    regression pinning the arity-2 shape (`FROM a,b WHERE b.aid=a.id AND EXISTS(...)` materializes on HEAD), ideally
    fold arity-2 into the B1 wrap (the same structural fix closes both arities).
    Superseded-attempts record:
  - [ ] (superseded) **B1 — THREE approaches tried, each instructive, the CORRECT target then pinned.** The goal:
    ordinalize (ZERO the producer for) an arity>=3 inner join under a WHERE EXISTS while preserving the index SARG.
    Attempts: (A) bespoke helper (`translateGatheredInnerClusterWithExists`, merge join legs + existential + baked
    WHERE into ONE select) → row-correct but MATERIALIZED (SARG lost via implementJoinWithExistential's step-1).
    (B) minimal seed-flip (arity>=3 seed → ordinal RC, WHERE lazy) → also MATERIALIZED (EXPLAIN-confirmed). (C)
    fall-through to the GENERIC existential-wrap arm (route arity>=3 to the OUTER-join path,
    cascades_translator.go:2368) → PRESERVES the SARG (P7) and is row-correct, BUT does NOT ordinalize: the generic
    arm translates the join as an ENCLOSED leg of the wrap select (inInnerCluster=true → the gate poisons it →
    name-model), so the census still fires 2 ENCLOSED P4 producers. And the enclosed-under-a-permanent-existential-
    wrap leg is NOT retirable by enclosure-starvation (the wrap never lifts), so (C) is a no-op for the atomic cap —
    same P7 name-model plan as baseline, just re-accounted. CORRECT TARGET (the one none of A/B/C did): translate
    `join + f.Predicate` (the WHERE conjuncts, WITHOUT the EXISTS — already split by splitNonExistsPredicates) as a
    FRESH ordinal expression — `FROM a,b,c WHERE b.aid=a.id AND c.aid=a.id` ordinalizes to P1 (0 producers, SARG
    preserved; the census plain-3-way probe PROVES a fresh comma-join+WHERE ordinalizes) — THEN wrap THAT ordinal
    relation with the existential as an outer semi-join. The crux the wrap must solve: the join must be translated
    FRESH (un-enclosed, so it gates ordinal), NOT as an enclosed leg (which the generic arm does). Find how the
    generic-arm wrap builds its inner (after cascades_translator.go:2371) and make the inner join translate fresh —
    OR build the two-level structure explicitly (fresh ordinal join expr + existential-wrap machinery). CERT (still
    mandatory): SARG `[=]` + ordinal-fired (census 0) asserted TOGETHER + row falsification (EXISTS→3rd/4th leg) +
    NOT-cross-product plan shape + red-under-sabotage. Guard dup-alias with mintedBindingLeg. STILL OPEN: arity-2
    comma+EXISTS SARG loss (P4, booked below) — the same correct target (fresh ordinal join + existential wrap)
    closes both arities.
    **COMPLETE MECHANISM DIAGNOSIS (the exact missing piece, after 4 empirical attempts).** The generic WHERE-EXISTS
    wrap ALREADY exists (cascades_translator.go:2560-2584: translateRef(f.Input) with enclosure decided by
    existsOuterGatesFresh, then buildExistentialSelect:3130 wraps it + threads f.Predicate as allPreds + the EXISTS
    correlation via existsInnerCorrelation). The FUNDAMENTAL blocker: buildExistentialSelect's allPreds (the WHERE
    join-conjuncts `b.aid=a.id`) AND the EXISTS correlation (`e.eref=r.rc`) reference join LEGS (QOV(a), QOV(b),
    QOV(r)). When f.Input (the join) is translated NAME-MODEL, its merged output carries QUALIFIED leg keys
    (A.ID, R.RC), so these leg-refs resolve by name — that is why existsOuterGatesFresh keeps a join-with-
    leg-predicates NAME-MODEL, and why every attempt to ordinalize it collapses/doesn't-fire. When the join is
    ORDINAL, its output is POSITIONAL (named-ordinal fields per the merged leg windows) and the top-level leg QOVs
    (a, b, r) are GONE (nested inside the wrapped join's single QOV) — so `FieldValue(QOV(r),RC)` no longer
    resolves. THE MISSING MECHANISM: rebase BOTH the WHERE conjuncts and the EXISTS correlation from leg-QOV
    name-refs to `FieldValue.ofOrdinal(QOV(wrappedJoin), slot)` over the wrapped join's MERGED output — a
    bakeGatedJoinPredicates-analog for the NESTED/wrapped case (bakeGatedJoinPredicates today targets the FLAT leg
    QOVs; this variant targets the single wrapped-merged QOV, using the merged leg-window layout OrdinalSeedLegWindows
    to map leg.col → slot). Build that rebase, then: translate `LogicalFilter{Predicate:f.Predicate, Input:join}`
    (no EXISTS) FRESH → ordinal P1 innerRef; wrap via buildExistentialSelect with the correlation rebased onto the
    innerRef merged QOV. This is the one focused mechanism the whole slice needs; NOT landable as a routing tweak
    (proven by attempts A/B/C). ATTEMPT (D/E): fall-through to the generic arm + WIDEN existsOuterGatesFresh
    (rfc173_cluster_gate.go:86) to admit INNER arity>=3 (so the join gates fresh/ordinal and the LEFT/RIGHT
    below-FOD rebase would fire) → ALSO MATERIALIZES (correlated-index SARG lost). Confirms the below-FOD ordinal
    rebase is genuinely LEFT/RIGHT-specific (the 1+1 dissolve-to-INNER shape); the N-way INNER cluster is a
    different shape it cannot handle. So NO existing rebase machinery works — the new bakeGatedJoinPredicates-analog
    for the wrapped merged QOV (above) must be built. FIVE attempts total, all reverted clean, nothing shipped.
    Reverted-prototype record:
  - [ ] (superseded) **B1 first attempt (bespoke helper) — row-correct but a PLAN-QUALITY (SARG) regression + a dup-alias bypass;
    superseded by the corrected target above.** The prototype
    (`translateGatheredInnerClusterWithExists`: N ForEach legs + all nested ON preds + WHERE conjuncts + EXISTS
    existential quantifiers + correlation preds, all baked over the N-leg ordinal seed; intercepted in
    translateJoinWithExists before the arity!=2 decline) PASSED the falsification control on ROWS
    (EXISTS→3rd-leg → [2], →4th-leg 4-way → [3], distinct from 1st-leg [1] — the per-alias legTypes bake maps to
    the correct window regardless of leg count, confirming Java's alias-keyed semantics) + mixed conjunct + NOT
    EXISTS. BUT it broke TWO existing tests, so it was reverted (uncommitted): (1)
    **TestFDB_RFC173_CorrelatedIndexExistsStaysIndexed** — `FROM a,b,c WHERE b.aid=a.id AND c.aid=a.id AND
    EXISTS(...)` LOST its SARG'd index scan and collapsed to a full A×B×C cross product. (2)
    **TestFDB_RFC173Item1_KeyBindingAndBuriedExists/P4b_arity3_dup_exists** — a duplicate FROM alias must LOUDLY
    decline; B1 bypassed the minted-binding decline.
    **CORRECTED ROOT CAUSE (Graefe SARG consult — my first framing "baking the WHERE loses the SARG" was
    EMPIRICALLY REFUTED):** a 7-shape EXPLAIN probe showed baked cross-leg ON equijoins STILL SARG (`a JOIN b ON
    b.aid=a.id JOIN c ON…` == the plain comma gather, both keep `Scan(A,[=])`) — baking is a red herring. The SARG
    dies because the existential flatten routes the inner join through `implementJoinWithExistential`'s MATERIALIZED
    step-1 (`correlatedStep1` rule_implement_nested_loop_join.go:1963-1973/:2110), which keys on one leg laterally
    depending on the other — NEVER on the join predicate. Two independent Scan legs → correlatedStep1=false → a
    materialized `NewRecordQueryNestedLoopJoinPlan(Scan(B),Scan(A),joinPreds)` that takes already-formed leg plans
    and can't re-enumerate for index access → no `[=]`. SARG survives ONLY when the join is optimized as its OWN
    expression with EXISTS layered on top (the P2/P7 shape: `FlatMap(outer=<join>, inner=EXISTS)`).
    **SOUND B1 DESIGN (Graefe conditional design-ACK — STRUCTURAL, not predicate-placement):** translate the
    multi-way inner join as its OWN gathered ordinal cluster (translateGatheredInnerCluster — the SARG-preserving
    P1/P5 machinery) and layer the EXISTS as an OUTER existential semi-join FlatMap over it (the proven P2/P7
    shape). Do NOT merge the join legs + existential into one bespoke baked select; do NOT hand the join to the
    materialized step-1. Each half is independently proven, so B1 is their composition. Correlation rebase over the
    ordinal gathered outer uses the existing W4-left machinery (ordinalSeedLegWindowsOf/rebaseOuterLegRefsOrdinal,
    NLJ rule ~2152). Guards: WHERE conjunct referencing the existential stays below the FOD (predicateReferencesInnerLeg
    split, NLJ 2039-2061); buried-leg straddle bakes on the box QOV (predicateRefsBuriedLeg) — both already handled
    by translateGatheredInnerCluster, another reason to DELEGATE the join to it. MINIMAL EXPERIMENT first (Graefe):
    the current arity≥3 branch already builds a NAME-MODEL flat 3+1 select that lowers to P7 (SARG-preserving); flip
    ONLY the join legs' seed to the ordinal RC while keeping WHERE lazy + the join sub-tree independently optimized
    (not collapsed into step-1), EXPLAIN-check it stays P7-shaped — if so that's the smallest B1, else do the
    explicit wrap. Guard: `mintedBindingLeg(legOps...) == ""` (dup-alias → loud decline, keeps P4b green). CERT (6
    assertions, MANDATORY — the row-only cert is why the regression passed review): (1) SARG `[=]` present; (2) NOT
    the materialized cross product `NLJ(INNER, NLJ(INNER,Scan(A),Scan(B)),Scan(C))`; (3) EXISTS-as-semi-join
    `FlatMap(outer=<join>, inner=…FirstOrDefault(…Scan(E)))` shape; (4) ORDINALIZATION FIRED (census 0 firings on
    the shape — SARG+ordinal-fired MUST be asserted together, else the cert passes on the name-model P2 plan and
    proves nothing); (5) row falsification (EXISTS→3rd/4th leg distinct windows + dup-named legs); (6)
    red-under-sabotage (mis-key a leg window / force step-1 → both SARG and ordinal-fired go red).
    **SAME-DAY SURFACE (Graefe) — SHIPPED LATENT PLAN-QUALITY BUG, arity-2 comma-join+EXISTS SARG loss:**
    `SELECT a.av FROM a, b WHERE b.aid = a.id AND EXISTS (SELECT 1 FROM e WHERE e.eref = a.id)` collapses to a
    MATERIALIZED `NLJ(Scan(B), Scan(A))` cross product (O(|A|×|B|)) on HEAD where an O(|B|) PK-probe plan exists —
    the SAME mechanism as B1 (the existential flatten's materialized step-1 can't do index access), one arity down,
    untested (no plan-shape assertion on a 2-way comma+EXISTS; invisible to a row-only cert). The structural B1 fix
    (join as its own optimized expression + EXISTS as outer semi-join) closes BOTH arities — fold arity-2 into the
    B1 slice (Graefe: "ideally fold arity-2 into the B1 slice so one structural change closes both"). At minimum add
    a plan-shape regression pinning the arity-2 shape. NOT a Go-only divergence — it's a Cascades plan-quality gap.
    Original design record (Graefe design-ACK, WITH the census correction): MECHANISM (Graefe, code-confirmed): a plain `p JOIN q JOIN r` routes through
    `translateJoin` → `ordinalWedgeGateDecide` returns Gated/Arity=3 → `translateGatheredInnerCluster` (full
    N-way ordinal seed, 0 firings). Under `WHERE EXISTS(…)` it routes through `translateJoinWithExists`, whose
    `:5003` narrows `if gatedFlatten && Arity != 2 { gatedFlatten = false }` (Reason: "existential flatten builds
    exactly two ForEach legs") — the flat-EXISTS path has NO N-way gather (the missing twin of translateJoin:4769).
    So the arity≥3 subtree name-models: inner p JOIN q enclosed=true Scan|Scan, outer (pq) JOIN r UN-ENCLOSED
    Join|Scan. FIX: give the flat-EXISTS path the N-way ordinal gather it lacks — compose the two ALREADY-PROVEN
    mechanisms (translateGatheredInnerCluster + the arity-2 gatedFlatten ordinal-seed-plus-existential attach at
    :5152-5176). No new row synthesis. WHY B1 (not the box): (a) corrects the class that broke the reframe model;
    (b) LOWER risk than unnest-over-box birth (no null-extended/reordered box-row positional birth, no
    adaptLegPositional same-named-leg hazard); (c) retires the enclosed inner leg (E-5) for FREE (the gather
    translates legs fresh). CONTROL (the genuinely new question — does the existential ordinal rebase, today over a
    2-leg seed, generalize to an N-leg seed?): EXISTS correlated to the 3rd+ leg — `… p JOIN q JOIN r … WHERE
    EXISTS (SELECT 1 FROM e WHERE e.x = r.col)` — PLUS a dup-named-leg variant; the falsification is a baked
    correlation predicate rebasing onto the WRONG ordinal window of the N-way concat. JAVA REFERENCE (read-first,
    sourced): Java resolves the EXISTS correlation PURELY by quantifier-alias — `visitExistsExpressionAtom`
    (ExpressionVisitor.java:560) makes the subquery an EXISTENTIAL quantifier in the SAME flat operator list as the
    join legs; the correlation `e.x=r.col` resolves via SemanticAnalyzer.resolveAcrossFragments (:382) to
    `FieldValue(QuantifiedObjectValue(r-alias), col)` — keyed on the leg ALIAS, never a position/ordinal into a
    merged concat. Inner joins are FLAT (QueryVisitor.visitInnerJoin:439 accumulates legs; generateSimpleSelect →
    GraphExpansion.buildSelect:396 = ONE SelectExpression with all N ForEach + the existential). There is NO N=2
    special case anywhere; the only literal is PartitionSelectRule:78 `size()<3 → return` (a don't-split-small
    guard, the OPPOSITE direction — N≥3 is the case it works). So Java proves the SEMANTICS are leg-count-agnostic:
    the Go arity==2/arity≥3-decline split is a Go artifact with no Java analog, and the falsification control
    (EXISTS→3rd+ leg) tests only the Go ordinal-window MAPPING (an impl risk), not the semantics. Semi-join impl to
    mirror: ImplementNestedLoopJoinRule (existential inner → FlatMap short-circuit, `instanceof Existential`, never
    an index). Corrected residual inventory:
    (reconciled with the B0-fix Finding A — TWO un-enclosed classes, not one).
    **UN-ENCLOSED** (not reachable by starving enclosure — each needs an explicit flip): U-1 P4
    existential-over-multi-way-join (B1); **U-2** P5 top-level box-base (and other declining) chained-unnest
    lowering — a fresh chained unnest over a box base declines to name-model and its OUTERMOST link fires
    un-enclosed (the SAME box-substrate class is ENCLOSED when nested, so box-substrate residuals are NOT
    enclosed-only; retired by the box-substrate ordinalization = B2/box slices, just observed un-enclosed at the
    top level). The SQL e2e census can't fire P5 (arrays aren't SQL-INSERTable); U-2's un-enclosed firing is
    pinned by the white-box TestRFC173_ProducerCensusP5EnclosureBit. **ENCLOSED** (starve by ordinalizing the
    roots): E-1 name-model unnest lowering, E-2 unnest-over-single-clustered LEFT/RIGHT box birth (the former
    B1 → now **B2**; also covers U-2's box-base chained shape), E-3 correlated-scalar-in-projection (W4b
    clusterPullUp), E-4 recursive-CTE reference legs, E-5 flat-EXISTS inner leg (retired FREE by B1). Sequence:
    B1=U-1 → B2=E-2 (+U-2) → E-1/E-3/E-4 → nested-outer-box tower LAST. 2a/2b retire WITH the enclosed classes.
    **B2 RE-SCOPED (post-B1 pre-change probe, census-grounded).** The plain LEFT-box unnest
    (`FROM LA LEFT JOIN LB ON …, LA.ARR AS X`) ALREADY ordinalizes on HEAD — 0 producers, rows correct on all
    three probe variants incl. the dup-K leg discriminators (LA.K=100 / LB.K=NULL) — so "relax the FULL-only
    boxOuterBirthsPositional gate" is NOT the slice (the gate doesn't bite the plain shape). The LIVE E-2/U-2
    class is the **CONJUNCT/EXISTS-FILTERED LEFT-box unnest**: `… , LA.ARR AS X WHERE LA.K = 100` (or WHERE
    EXISTS) fires P4 enclosed (the box seed name-models) + P5 UN-ENCLOSED (the unnest) — the
    unnestOuterConjunctOnBoxLeg + boxOuterBirthsPositional coupling (5-site map Site 5 + Site 1), i.e. exactly
    the "box-base + box-leg-conjunct TOGETHER, axis-coupled" booking. RIGHT-box unnest: 0 producers, correct [].
    B2 therefore = ordinalize the FILTERED LEFT-box unnest (box birth + seed + conjunct placement move together);
    needs a Graefe design consult before code (the coupled-axis territory the original Outcome-B booking flagged).
    ALSO RECORDED (probe finding, non-production): `SELECT LA."K", X` over the plain LEFT-box unnest DIVERGES
    under the test-only name ORACLE (ordinal 100|7 correct; oracle NULL|7) — an oracle-bridge gap on an array
    shape outside the §5 SQL corpus (arrays aren't SQL-INSERTable), NOT a production silent-wrong (production =
    ordinal = correct; the oracle scaffolding dies at the cap). Note for the dual-window carve-out list if the
    corpus ever grows an array shape.
    **B2 STEP-0 COMPLETE (13-shape probe battery, both models + census; Graefe design-ACK sub-slice A).**
    Q00–Q11 — preserved-leg / null-supplied-leg (value + IS NULL) / FULL-both-legs / RIGHT / OR-spanning / NOT /
    mixed element+leg / AT-ordinal / scalar-subquery / GROUP-BY conjuncts: rows CORRECT in BOTH models, 2
    producers each — sub-slice A is a PURE COVERAGE change (no latent bug). Q12 the ENCLOSED SIBLING
    (`FROM (LA LEFT LB), LA.ARR AS X, CC WHERE LA.K=100`): 0 producers + CORRECT production rows — the conjunct
    ALREADY bakes through the $BOX buried windows on the enclosed path, so the A execution substrate is PROVEN
    e2e (the consult's decisive fork, branch 1). Q12's name-ORACLE returns [] — the SECOND instance of the
    oracle-bridge gap class ($BOX-window bakes have no name fallback under DisablePositionalEmission; production
    ordinal correct; the oracle dies at the cap). MECHANISM (consult-corrected): the plain shape ordinalizes via
    translateGatheredUnnestCluster (the GATHER), NOT the binary path (clusterArity(LEFT)==2 blocks
    boxOuterBirthsPositional unconditionally); the filtered decline is gather:93 (the blanket
    unnestOuterConjunctOnBoxLeg decline); the conjunct's ordinal placement is the gathered select's predicate
    list baked via bakeGatedJoinPredicates over the $BOX buried windows (WHERE-above-LEFT semantics for BOTH
    legs; pushdown is the optimizer's job, never hand-placed SARGs). NEXT: implement A1 (3-state boxConj verdict
    — None/Bakeable/Unbakeable — computed PRE-translation, metadata-only: no subquery values + every legRef
    FieldIndex-resolves in its buried window's leafTyp; gather:93 declines only Unbakeable; EXISTS site always
    Unbakeable this slice) + A2 (the gather RECORDS its legTypes keyed by join node —
    t.unnestGatherBoxLegTypes[j] — and the WHERE-merge arm consumes the record, fires iff present,
    bakeGatedJoinPredicates over the RECORDED map, never re-derived: the seed⟺merge one-authority law; +
    defensive no-unbaked-legRef assert) as ONE atomic commit; A3 = ZERO changes to :920/:966/:1490/chained.
    Controls: dup-K sabotage (whole-concat FieldIndex → red), mis-keyed legTypes (gatedJoinLegTypes(box) → loud),
    feature-off (gather:93 blanket restore → all pins fail), census promotion (filtered LEFT/RIGHT/FULL → 0
    producers; EXISTS variant still 2 — sub-slice B, design-NAK deferred to its own consult: the
    existential-rebase-over-$BOX-windows question is unverified). Comment debt in the same commit: the flag's
    field doc, gather:77-95, unnestExistsSeedSafe:897-919, the :2392-2398 flag-site comment.
    **B2 SUB-SLICE A LANDED — awaiting 4-gate.** A1: `unnestOuterConjunctOnBoxLeg` bool → the 3-state
    `unnestBoxLegConjunct` verdict (None/Bakeable/Unbakeable); `classifyBoxLegConjunct`
    (rfc173_b2_box_conjunct.go) computes bakeability PRE-translation, metadata-only (box-as-one-leg
    ordinalJoinSeedFields map derives + no ScalarSubqueryValue/ExistsValue + no foreign correlation + every legRef
    FieldIndex-resolves in its buried window's leafTyp + dotted-frontier reads decline); the EXISTS site always
    sets Unbakeable (sub-slice B). gather:93 declines ONLY Unbakeable. A2: the gather RECORDS its legTypes
    (t.unnestGatherBoxLegTypes[j], box-arm only, success-only) and the WHERE-merge arm consumes the record FIRST
    (fires iff present — the box select's 2 quantifiers are count-indistinguishable from the binary name-model
    select), bakes via bakeGatedJoinPredicates over the RECORDED map (the one-authority law) + a
    predicateRefsBuriedLeg defensive assert (loud internal error on verdict/bake drift). A3 honored: ZERO changes
    to unnestExistsSeedSafe's semantics (:920 reads non-None == the pre-verdict true), boxOuterBirthsPositional,
    :1490, chained. CERTS: e2e TestFDB_RFC173B2_FilteredBoxUnnest (12 exact-row pins over the dup-K fixture:
    preserved/null-supplied value + IS NULL/FULL both legs/RIGHT/OR-spanning/NOT/mixed element+leg/AT-ordinal/
    scalar-subquery-stays-name-model/GROUP-BY) + white-box TestRFC173B2_FilteredBoxUnnestCensus (bakeable → 0
    producers; unresolvable-ref control → producers fire). CONTROLS RUN: feature-off (blanket decline restored) →
    the census pin goes RED (discriminating; the e2e rows are model-independent BY DESIGN — step-0 proved
    name-model parity, so the census pin is the model discriminator, the B1 "asserted together" split). Full
    suite 55/55. Producers retired: the filtered-box-unnest P4-enclosed + P5-un-enclosed pair (U-2's
    conjunct-triggered class); the EXISTS variant still fires 2 (sub-slice B, booked).
    **ROUND 2 (all three gate NAKs addressed):** codex P1×3 — classifyBoxLegConjunct gates
    `gatesAsFreshCluster` FIRST (ordinalLegColumns panics by design on name-model legs), absent-legTypes-key
    box-leg refs → Unbakeable (transparent-filter-wrapped operands), stale record across CTE retranslations →
    Graefe's CONSUME-ONCE delete-on-read at the merge site. Graefe — his shape battery is a PERMANENT probe
    file (rfc173_b2_graefe_impl_probe_fdb_test.go); the P1/Q CTE pins are flip-sentinels documenting the
    PRE-EXISTING enclosed-CTE silent-NULL residual (booked below). Torvalds — post-B2 truth rewrite: baretwin's
    anchored-fallback GROUP-BY pin re-pointed at a subquery conjunct (the surviving Unbakeable decline; found
    the scalar-subquery silent-NULL hole, fixed + booked above), c5a's two lying `*-stays-name-model`
    subtests renamed `*-bakes-ordinal` + the noshare pair's comments rewritten (all four gather since B2-A;
    the EXISTS pin comment states WHY it stays name-model),
    unnestExistsSeedSafe's leading paragraph reconciled with its inline block (binary seed declines for ANY
    verdict; the gathered path is where Bakeable bakes), gather-coupling test pins the full 3-state routing
    (None gathers / Bakeable gathers + RECORDS / Unbakeable declines), per-arm classifier pins
    (scalar-subquery/ExistsValue/foreign-correlation/dotted-frontier + bakeable baseline), typed
    `boxConjVerdict`.
    **ROUND 2 VERDICTS + NIT ROUND:** Graefe ACK (consume-once sound incl. the leaked-record hunt; the
    scalar-subquery loud boundary correct — the nil-ctx probe can't false-fold, constant classification
    whitelists before evaluating); Torvalds ACK-with-nits (all fix-list items verified in-tree; he probed the
    pre-fix parent in a worktree); codex P1 = flip-sentinels bless the enclosed-CTE NULL rows → answered by
    taking the booked enclosed-CTE fix as the immediate next slice. Nit batch landed: WithParams carries
    scalarSubqueries (copy symmetry + pin), dup-alias gate-first classifier pin (discriminating: without the
    gate the seed derives and answers Bakeable), NESTED-box class VERIFIED CORRECT and pinned three ways
    (classify=Bakeable + census 0-producers + e2e correct rows `(LA FULL LB) FULL CC` — the wedge gate admits
    nested boxes by design; boxGatesFresh's exclusion is the BINARY/birth gate, a different authority),
    clustered-leg census arm (buried non-owner conjunct → 0 producers), nested-scalar-subquery loud
    flip-sentinel (inner subquery uncollected → UnboundScalarSubqueryError, deletes nothing; flips when
    nested collection lands), shared prebindScalarSubqueries harness helper (4 copies → 1), c5a CLUSTERED
    comment truth-rewrite (mechanism moved twice: c5b admits the cluster, the EXISTS composition is what
    keeps it name-model), cluster-gate + gather-test wording.
  - [x] **RESOLVED (was: enclosed-CTE box-unnest silent all-NULL rows — fixed per the Graefe design
    consult, Option A: schema-complete merge fabrication).** The original root-cause direction ("the
    enclosed-CTE translation loses the result-value wiring") was WRONG — the consult dumped the trees and
    executed every subplan node: translation loses NOTHING; the values flow to the last hop and are dropped
    by the EXECUTOR's name-model merge. `qualifyAlias`/`qualifyOuterRow` refused to fabricate `C.*` keys for
    any leg carrying dotted keys — a guard against the real join-merge-leg hazard (bare keys are
    last-leg-wins leftovers) that wrongly caught SCHEMA-COMPLETE projection outputs, whose dotted keys are
    executeProjection's source-name convenience keys and whose alias genuinely names the whole row. A
    plain-join CTE body has the same Datum hole MASKED by the positional frontier; the enclosed name-model
    unnest FlatMap is the one shape that forces pos=false end-to-end and unmasks it. FIX: executeProjection
    outputs, unnest-FlatMap RC rows, and the §5 oracle mirror rows set QueryResult.Complete; the merge/pad
    sites fabricate-if-absent `ALIAS.key` for COMPLETE legs only (merge OUTPUTS stay incomplete — the
    mixed-nesting refusal survives at the next level, white-box-pinned both ways in
    merge_fabrication_test.go). All five flip-sentinels flipped to the strictly-correct rows + new pins:
    star-body (RC-complete class), bare-read guard, ORDER BY with the ORDER asserted, pad-row
    (qualifyOuterRow dimension).
    **SECOND pre-existing bug UNMASKED by the fix (Q8): the ON of an explicit JOIN over a join/unnest-bodied
    CTE was silently DROPPED → cross-product rows** (every C row matched every CC row; plain-join CTE bodies
    too — no unnest needed). Root cause: buildCTEColumnSource declined multi-leg bodies, the CTE never
    entered cteScopes, and the ON resolver's scope build classified the name as "unresolvable table, no drop
    risk" — but a CTE name RESOLVES downstream, so nothing errored. FIXED twice over, SURGICALLY:
    buildCTEOnOnlySource derives an ON-RESOLUTION-ONLY schema at WITH registration (explicitly-ALIASED
    projection items only — the one class whose runtime keys provably exist; unaliased refs key by their
    QUALIFIED source name and WITH-c(x) renames never reach the runtime row, so both DECLINE to the loud
    marker) into a dedicated cteOnScopes map threaded through BOTH build pipelines (the plan visitor and
    the WithCTECatalog chain incl. the subquery planner and the TWO empty-scope short-circuits that
    silently dropped it on the scalar-subquery path — the review-proven T6 hole, pinned Q10 with a
    COUNT 0-vs-3 discriminator). The map's ONLY reader is upgradeJoinOnPredicates: ONs resolve and pad
    correctly (Q9a-Q9d LEFT/INNER/reversed/plain-join, Q13 FULL both-pads), a declared-but-underivable CTE
    routes to the loud drop-risk 0AF00 (Q9e derived twin, Q11 unaliased-qualified, Q12 column-aliased), and
    WHERE/projection resolution over comma-joined multi-leg CTEs keeps its clean decline — a first GLOBAL
    version broke TestFDB_RFC173_GatePinB_FlatteningEvasion/cte_form_fails_cleanly exactly as that gate pin
    was designed to catch.
    (i) STILL BOOKED with the derived-table twin (below): the loud classes that Java answers.
  - [ ] **BOOKED (enclosed-CTE slice follow-ups — the LOUD reach classes Java answers).** One widening
    slice covering the 0AF00/42703 family the ON-drop fix deliberately kept loud: (a) join-bodied DERIVED
    tables in an explicit JOIN with ON (buildDerivedTableSource declines them — Q9e flip-sentinel);
    (b) UNALIASED-QUALIFIED projections in multi-leg CTE bodies (execution keys the slot "D.ID" with no
    bare key — needs a real post-CTE output-schema authority, Q11 flip-sentinel); (c) WITH c(x, y) COLUMN
    ALIASES over multi-leg bodies (scope-level renames never reach the runtime row — needs the rename
    pushed into the body projection or the CTE wrapper, Q12 flip-sentinel); (d) qualified WHERE over a
    CTE leg (already-loud 0AF00, consult finding); (e) NEW (round-6 repro of the delta-review P2, standalone
    control): a derived table whose INNER projection is qualified-spelled fails at RUNTIME with a
    malformed-plan error even with no CTE at all — `SELECT D."AID" AS "A" FROM (SELECT LA."AID" FROM LA
    LEFT JOIN LB ON …) "D"` → `field "AID" not resolvable (row columns [LA.AID])`; the derived row keys by
    the inner SPELLING instead of a canonical output name (the CTE ON derivation now declines these
    fail-closed, Q19/Q20/Q25 pins, but the standalone reach gap remains); (f) NEW (round-6 repro of the
    delta-review P1, standalone control): the 42702 ambiguity backstop does NOT run when a derived-table
    leg sits among multiple FROM legs — `SELECT "AID" FROM (SELECT LA."AID" FROM …) "D", LA "L2"` silently
    resolves the ambiguous bare ref against the enumerable leg (returns rows; should 42702) because the
    derived leg's columns are invisible to the resolver (CTE ON derivation declines fail-closed, Q18/Q21
    pins; the standalone resolver gap remains); (g) NEW (round-7, review-caught): an ON-ONLY CTE name used
    as a FROM leg gives buildSelectScope a NIL resolver — BOTH the 42702 ambiguity gate and the 42703
    unknown-column gate are skipped for the whole body standalone (`SELECT "NOPE" FROM "V", LA` plans fine
    when V is join-bodied); the CTE ON derivation declines such bodies fail-closed (Q27/Q28/Q29 pins) but
    the standalone nil-resolver gap remains — the real fix is a resolvable post-CTE output schema, i.e.
    this slice. EXPLICIT WIDENING CANDIDATE (review-recommended): the multi-leg-with-derived/ON-only
    decline over-declines the sound sub-class where every read is an ALIASED item over the ENUMERABLE legs
    only (`WITH U AS (SELECT L2."K" AS "B" FROM (SELECT "AID" FROM LA) "D", LA "L2") … ON U."B"` answered
    correctly pre-narrowing, 0AF00 now) — admitting it needs per-read leg attribution + per-leg emitted-set
    validation. All seven are one output-naming authority problem: the scope's advertised names must be
    the names execution emits. TWO SYSTEMIC RIDERS for the same slice (review-noted, pre-existing):
    (i) encodeSortContinuation drops Positional across a continuation RESUME — every positional consumer
    downstream of a resumed sort sees name-keyed rows only (benign today because the admitted classes key
    bare in the Datum too — the expr.go single-source Field rewrite — but any future positional-only
    consumer breaks at page boundaries); (ii) the GROUP-BY validator's harvestColumnRefs walk cannot
    distinguish a subquery-LOCAL ref (no group check due) from a CORRELATED-into-outer ref (group check
    due) — a potential over-loud corner on outExpr entries containing subqueries. (h) NEW (round-8 review,
    pre-existing at base and HEAD, absurd-rare but SILENT conformance divergences vs Java on DOTTED
    spellings): a quoted-dotted CTE name equal to schema.table (`WITH "S.LA" AS (…) … FROM "S.LA"`) is
    silently bypassed for the TABLE's rows; inversely `FROM "s"."LA"` with a bare CTE "LA" declared reads
    the CTE where Java's generateAccess reads the table. Rider: the explicit-dotted-alias corner
    (`AS "S.LA"`) is loud malformed both sides — same booking. Round-9-review riders, same family:
    the QUOTED-dotted spelling also evades the round-9 collision decline (typed-segments vs lossy-split
    mismatch — `WITH V AS (SELECT K FROM LA AS "s", "S.LB")` admits with a dead backstop, 4 silent rows,
    identical at base and HEAD; needs the same three stacked absurdities); JAVA-CONFORMANCE PROBE DUE for
    the R5b aliased-away-name read (`SELECT PA.ID FROM PA AS s, s.PB AS B` — the R5b pins' Java citations
    cover the LEG classification only, NOT the read; standard SQL says the alias replaces the name, so if
    Java rejects it the round-9 leniency carve-out preserves a Go-only accident and can narrow to leg
    classification, which also collapses the ON-vs-WHERE loud-vs-lenient divergence on collision bodies).
    CLOSING ITEM for the schema-qualified family: normalize sq on the visitor path as the catalog path
    does (the round-9 buildSelectScope strip closed the WHERE gap (Q40) and the ambiguity backstop (Q39);
    a full sq normalization would collapse the three strip sites into one — but the R5b collision
    carve-out must MIGRATE INTO the normalizer when it lands, and the dotted-name divergences above
    interact with the same pass: one slice for all of it). The ordering half (round-11 review): the arc's
    real one-rule ending is a SINGLE shared source-name resolution function all four scope consumers call
    (translateScan / cteLegKind / buildSelectScope.addSource / upgradeJoinOnPredicates.resolveTable — all
    four now individually CTE-first, but as four hand-mirrored copies). Two more riders, same family:
    (i) the ON-ONLY READ class (pre-existing; the SILENT residual is narrow — WHERE-based main-query
    reads are already LOUD 0AF00, only the leniency-path MAX/scalar-subquery reads stay silent):
    buildSelectScope consults cteScopes only, so an ON-only name's UN-ENCLOSED reads either fall to a
    same-named TABLE's schema (loud 42703) or, with no such table, to the NIL-resolver leniency (which is
    LOAD-BEARING for the enclosed comma-FROM reads Q1-Q5 — it lets the executor merge fabrication resolve
    them). On the leniency path a VALID non-shadow enclosed read answers while an INVALID/unresolvable
    non-shadow scalar read is silently NULL (Q51 flip-sentinel: MAX("NOPE") over a non-shadow ON-only CTE).
    SHADOW-CASE resolution: the inner shadow ON-only CTE is EVICTED from cteScopes (mirroring the derivable
    arm's shadow delete) and joins the SAME booked ON-only READ class as any non-shadow ON-only CTE.
    Boundary (Q52/Q54): a BARE read over the coinciding shadow answers via leniency+fabrication; a QUALIFIED
    read (V."X") is SILENT NULL (fabrication provides bare keys on the merged row, not the "V.X" qualified
    key) — the one remaining silent-wrong, booked here as a FLIP-SENTINEL. HISTORY (do NOT re-attempt
    lightly): installing the inner shadow schema into cteScopes to make the qualified read answer was tried
    TWICE and both are UNSOUND — a plain lossy install silently mis-resolved quoted/dup bodies; a
    read-sound install (case-preserving Ids + dup-decline) reopened flatten-evasion with an EXECUTION PANIC
    on a comma-multi-leg shadow ("divergent baked types") AND its case-preserved Ids mismatched execution's
    UPPERCASE row keys (executeProjection ToUpper) → plan-then-runtime-fail. The SOUND fix is
    cteOnScopes-aware read resolution AND fixing execution's quoted-alias uppercasing (so quoted-lowercase
    identifiers survive as lowercase) — BOTH large, both here. Until then the qualified shadow read stays
    the booked silent flip-sentinel; the exotic shape (nested WITH, join-bodied inner CTE shadowing its
    same-named outer, qualified solo read) does not justify a fragile install.
    Fix for the remaining silent leniency residuals = cteOnScopes-aware read resolution,
    CAREFULLY: the flatten-evasion gate pin (cte_form_fails_cleanly) must keep its clean decline AND the
    enclosed comma-FROM reads (Q1-Q5) must keep resolving. (i-b) FORWARD-VISIBILITY divergence (round-12
    review, both reviewers independently; pre-existing, A/B'd to before the arc): `WITH A AS (SELECT …
    FROM B), B AS (…)` ANSWERS on both pipelines where SQL/PG reject the forward reference — the chain
    builds bodies after ALL registrations, and the visitor's REBUILD arm (running after the eager build
    correctly failed and was swallowed) does too; the rebuild-arm comment now states this truthfully.
    Java-conformance probe decides the fix; if Java rejects, the preState infrastructure is the vehicle
    (each registration point's snapshot is precisely the earlier-siblings-only view). (ii) JAVA-CONFORMANCE PROBE: the ORDER BY
    output-alias-precedence shapes (Q42/Q46 — bare unique alias over FROM-scope ambiguity) are standard
    SQL/PG behavior but unverified vs Java's SemanticAnalyzer (the M5 42702 text was live-verified for
    FROM-scope shapes only); probe alongside the R5b read probe — if Java 42702s them, the pins flip to
    documented divergences or revert. (iii) FIFTH STRIP SITE (round-11 review): buildCTEColumnSource —
    the registration-time global deriver — does not apply the schema strip, so a CTE with a
    schema-qualified body lands ON-only and an aggregate read over it misroutes into the correlated
    fallback (0A000, pre-existing rounds 9-11); the normalization slice must catch it. (iv) CONSUMER
    CENSUS for the one-shared-resolution-function ending (round-11 review — the fifth copy was missed by
    the round dedicated to aligning copies, which is the argument for the collapse): LIVE-fixed CTE-first:
    translateScan, cteLegKind, buildSelectScope.addSource, upgradeJoinOnPredicates.resolveTable,
    buildCTEColumnSource's inner-table resolution (round 13 — its metadata-first was LIVE-wrong: a
    shadowing body's schema derived from the TABLE, declined on the CTE-only column, and dumped the CTE
    into the ON-only marker path; the nested-shadow pin had been green only via the stale-outer accident
    the round-12/13 evict removed). SEVENTH copy (round-13 review, live LOUD reach, booking-grade):
    buildDerivedTableSource (~:318) resolves its inner table via the analyzer with NO CTE fallback at all
    — a derived table over a shadowing CTE OVER-DECLINES the valid read (0AF00; the ready-made
    flip-sentinel — answers when it goes CTE-first), the SELECT * variant advertises the TABLE's columns
    and fails at RUNTIME where plan-time 42703 was due, and the name-coincidence variant answers by
    accident. buildDerivedTableSourceFromAgg is clean (derives from aggregate output shapes, no table
    resolution). EIGHTH copy (review, A/B-confirmed pre-existing at the round-9 baseline, live LOUD reach):
    buildCTEColumnSource (the deriver) consults cteScopes but NOT cteOnScopes, so a join-bodied CTE
    shadowing a same-named base TABLE is invisible to it — it derives the dependent CTE's schema from the
    TABLE, accepts a column the CTE never exposes, and fails at RUNTIME (malformed-plan, "field K not
    resolvable, row columns [M N]") where plan-time 42703 is due. Fix: the deriver must recognize a name
    that is a join-bodied CTE (cteOnScopes membership) and decline to plan-time rather than resolving
    through the table. LATENT
    catalog-first (masked by the text fallback / upstream gates today; goes live when the text fallback
    retires): buildWherePredicateForJoinsWithCTEScopes.addSource (~:1508, its own comment says
    "metadata first, then CTE scopes") plus ~7 more ResolveTable sites in the same masked category
    (~:320, :912, :3754, :5790, :6083, :6206 — derived-carrier and exists-planner scope builders).
    NINTH-family (review, all three gates ACK on 60268dc9e + the follow-up): the ON-only CTE schema
    (buildCTEOnOnlySource) is now COMPLETE-SCHEMA-OR-DECLINE — it installs ONLY when every runtime column
    (keyed by executeProjection's uppercased emit-name) is unique AND case-safe; ANY obstruction (a quoted
    case-sensitive alias `AS "x"`, or a duplicate runtime name incl. `AS "x", AS "X"` both emitting "X")
    declines the WHOLE source (caller's loud 0AF00), never a partial table. WHY complete-or-decline and not
    partial-omit: the schema is ONE source of the enclosing join and the resolver decides bare-ref ambiguity
    by which SOURCES carry a name (scope.ResolveColumn), so a DROPPED-but-runtime-present column lets a bare
    ref silently REBIND to another enclosing source (`WITH C AS (… AS AID, … AS AID, … AS Y) … FROM C JOIN
    LA L ON AID = L.AID` returned rows instead of 42702 — review-caught silent-wrong; Q55(c) pins it loud).
    TWO reach-restorations for a future conformance slice (both read-surface, NOT wire-visible): (a) the
    POISON-MARKER — keep the UNIQUE columns of an obstructed body usable while making the obstructed name
    resolve AMBIGUOUS via a per-source ambiguous-names set checked in scope.ResolveColumn/ResolveQualifiedColumn
    (Java's per-attribute 42702 model) — restores the unique-Y-in-dup-body reach complete-or-decline now
    declines (Q55(d)/Q54); (b) execution's quoted-alias UPPERCASING itself (`deriveProjectionColumnDef`
    `label = ToUpper(alias)`, `OutputColumnName`, executeProjection Datum keys, the RFC-173 positional-frontier
    slot names, every downstream re-reader) — Java's `getColumnLabel` returns `x`/`MixedCase` case-preserved;
    Go loses it. Graefe scoping: (i) scope the fix as the INVARIANT not the label — the positional-frontier
    slot names are the LOAD-BEARING site (a label-only fix re-creates the half-fix runtime failure); (ii)
    gate = a cross-engine DIFFERENTIAL proving Go's getColumnLabel/resolution case matches Java's for quoted,
    unquoted, mixed aliases (a conformance-parity claim → needs the harness). When (b) lands, Q55(a)'s two
    mustLoud pins flip decline→answer (matching Java). Both sequence AFTER the S4 demolition unless a shift
    picks up conformance. SEQUENCING (Graefe): (a) and (b) INTERACT — if (b) the execution-uppercasing slice
    lands FIRST (quoted identifiers case-preserving end-to-end), the case-sensitive obstruction class
    DISSOLVES entirely (an `AS "x"` body then emits key `x`, advertisable truthfully), leaving only the
    genuine dup-name class for the (a) poison-marker. Build (a) against the POST-uppercasing identifier model
    or it encodes a workaround for a bug (b) removes. The gate covers BOTH the projection path (Q55) AND the
    AGGREGATE path (Q56 — buildDerivedTableSourceFromAgg, folded via NewUnquoted with its output names
    already StripIdentifierQuotes'd, so the quoted flag is re-read off the Uid via cteBodyAllAliasesCaseSafe;
    both the schema build and the dup gate consume the visible-only aggOutputCols authority so a hidden
    HAVING/ORDER-BY aggregate is neither advertised nor false-counted). (c) NEW (Graefe, pre-existing,
    general-read surface — NOT ON-only, NOT the rebind class): a WITHIN-SOURCE duplicate aggregate/projection
    output name in a DERIVED-TABLE or GLOBAL-CTE source silently LAST-WINS-resolves instead of 42702 —
    `WITH C AS (SELECT MIN(LA."AID") AS M, MAX(LA."K") AS M FROM LA) SELECT C.M FROM C` returns 110 (last-wins),
    Java's SemanticAnalyzer 42702s the ambiguous C.M; identical on the derived-table path
    `(SELECT … AS M, … AS M) AS d … d.M`. Single source, within-source ambiguity — belongs to the
    output-naming-authority / poison-marker family (resolver-level per-attribute 42702), booked here not
    fixed. (Distinct from the wrong-case-accept on those same paths, which is value-correct — the (b)
    uppercasing divergence — not a silent-wrong.) (d) NEW (Graefe ruling, LATENT executor silent-wrong, part
    of the (b) read-surface uppercasing family): aggResultName (pkg/recordlayer/query/executor/executor.go
    ~:2490) ToUppers the aggregate group-result slot key (`strings.ToUpper("SUM(%s)")` etc.), and
    finalizeGroup (streaming_cursors.go ~:349) writes each value under that folded key — so two aggregates
    differing only in a CASE-SENSITIVE token (a string literal: `COUNT(CASE WHEN s='x' …)` vs `…'X'…`)
    COLLIDE into one slot (Graefe confirmed at the datum level: `'x'` came back as slot key `'X'`; two
    case-differing aggregates → a single folded slot). Currently LATENT, not live: the only shape whose two
    case-differing aggregates compute DIFFERENT values (a string literal in a GROUPED CASE aggregate) does
    NOT evaluate in this engine today (grouped COUNT(CASE)=0, SUM(CASE)=nil), so the collision never yields
    observable wrong rows. If grouped CASE aggregation is ever made to compute, this becomes a LIVE
    silent-wrong across ALL callers (top-level HAVING too, not just ON-only CTEs) → fix aggResultName to
    case-preserve the render, or resolve HAVING/ORDER-BY refs by the case-preserved agg.Alias key
    finalizeGroup already writes (the HAVING/ORDER-BY resolution key is untraced — confirm which key it
    binds by). FRAMING CORRECTION (Graefe): the pre-004e215f2 ToUpper dup-gate that declined this shape was
    ACCIDENTALLY PROTECTIVE (consistent with the executor's ToUpper), NOT over-declining — the shape does
    NOT execute correctly at the slot level, it is merely masked by grouped-CASE-not-computing. This is why
    the fix must NOT go in the ON-only gate (principle-10 mis-location AND it would re-break the
    `COUNT(*) … HAVING COUNT(*)` false-decline: that harmless same-value collision is indistinguishable
    from the harmful case-differing one at the gate) — it belongs at aggResultName. A sentinel comment lives
    at the aggResultName site; the red→green pin lands WITH the fix (needs a string-column fixture).
  - [ ] **BOOKED (enclosed-CTE consult finding — LATENT collision hazard): the derived-table
    qualified-ref→bare-read rewrite.** `FROM (SELECT a.k AS x FROM …) AS d … WHERE d.x = 1` resolves d.x
    by rewriting to a BARE `x` read at build time — collision-unsafe in principle when another visible
    source carries an `x`. No wrong-rows repro today; audit + pin when the output-naming slice (above)
    lands. SIBLING (review round-4 note, pre-existing): nested same-named CTEs where a join-bodied INNER
    shadows a derivable OUTER — the outer's schema stays visible in cteScopes while the inner registers
    only an ON-only MARKER, so resolveTable falls through to the wrong-generation schema. Ultra-rare
    shadowing semantics; same audit.
  - [ ] **BOOKED (Slice 2d discovery — PRE-EXISTING semantic-resolver gap, orthogonal): a WHERE ref to a
    DEEP chained alias → 42703 "column does not exist".** Boundary: a 3-link AS-alias (`Z` in `…Y.DEEP AS Z
    WHERE Z…`) resolves; a 4+-link AS-alias (`v` in the 4-link chain), a 3-link AT-alias (`o`), and a
    mid-link AT-alias (`yo`) all 42703. Thrown at semantic resolution UPSTREAM of cascades translation, so it
    reproduces identically in name-model (NOT caused by ordinalization; the clean projections at every depth,
    incl. AT, do resolve). Affects filtered-deeper-than-3-link + any AT-in-WHERE shape in BOTH engines. A
    SemanticAnalyzer scope-depth fix, separate slice — filtered 3-link (the expressible filtered depth) already
    ordinalizes.
  - [x] **RESOLVED (was: "engine-wide scalar subquery in WHERE returns `[]`" — the Slice 2b booking was
    MIS-SCOPED; root-caused and fixed in the B2-A round-2 batch).** The `[]` was never an engine gap: the
    sql-driver path answers scalar-subquery WHERE comparisons correctly (fetchPage pre-evaluates each
    subquery via `executor.EvaluateScalarSubquery` and binds via `WithScalarSubqueries` —
    `TestFDB_HavingSubqueryProbe/where_gt_scalar_then_group` pins non-empty rows). The silent `[]` came from
    a TWO-LAYER hole on the DIRECT-EXECUTOR path only: (1) `embedded.PlanRecordQueryWithMetadata*` DISCARDED
    the translator's `[]ScalarSubqueryPlan` (`ref, _, translateErr :=`), so harness callers had nothing to
    bind; (2) `ScalarSubqueryValue.Evaluate` answered SILENT NULL for an absent binding (and for raw-map row
    contexts — the executor's no-bindings filter paths pass the bare Datum map), so every comparison
    degraded to UNKNOWN and rows vanished. FIXED correct-or-loud: absent binding / bindingless row context →
    loud `*values.UnboundScalarSubqueryError` (present-nil stays legit zero-rows NULL; the nil ctx stays the
    plan-time probe per Comparison.Eval's constant-RHS contract); subquery planning factored into ONE shared
    path (`planScalarSubqueryPlans`, used by the generator AND the new `PlanRecordQueryWithSubqueries`
    harness API); the direct-harness tests (baretwin/B2/Graefe-probe) pre-bind exactly like fetchPage.
    PINNED: `TestScalarSubqueryValue_Evaluate` (absent→error incl. raw-map/scalar ctx, present-nil→NULL,
    nil-ctx→nil), B2's `scalar_subquery_unbakeable_true_arm` (subquery actually EVALUATES: 100<900 → rows —
    before the fix both arms returned `[]` indistinguishably), baretwin's re-pointed
    `grouped_over_name_model_fallback_does_not_bake` (real rows through a bound subquery over the anchored
    fallback). THE LOUD ERROR IMMEDIATELY CAUGHT A THIRD SITE — production DML: planDML ALSO discarded the
    translator's scalar subqueries (`ref, _ :=`), so a driver `DELETE … WHERE v > (SELECT …)` silently
    compared v > NULL and deleted NOTHING — dualwindow stayed green because BOTH windows were identically
    wrong (the both-models-agree blind spot). planDML now plans + carries them (same shared helper; fetchPage
    pre-binds for DML exactly as for SELECT). PINNED:
    `TestFDB_DmlSubqueryWhereProbe/{delete,update}_where_scalar_subquery_threshold` (driver-level RowsAffected
    + survivors; the corpus's scalar_subq_after_delete_with_subq_threshold entry now answers its documented
    rows for real). STILL OPEN (narrowed, chained-only): the chained-unnest scalar-subquery WHERE stays a LOUD
    0A000 (the Slice 2b sentinel) because the chained predicate would ride the wedgeGate POSITIONAL bake and
    crash (ordinal -1) — flipping 0A000 → rows is the remaining (now much smaller) slice; sibling
    IN-subquery/EXISTS-in-filter-over-chained 0AF00 reach gaps unchanged.
  - [ ] **BOOKED SLICE (Graefe, RFC-173 S4 Slice 2b follow-up — orthogonal resolver gap): scalar sibling
    of an unnested array element does not resolve (42703).** `SELECT X.K FROM T4, T4.SARR AS X` where the
    SARR element has a scalar sibling `K` → `42703 column "X.K" does not exist`, IN THE BASE CASE (no
    chaining, no filter). A semantic identifier-resolution gap (the resolver can't reach an unnest
    element's own sibling scalar by the element alias), orthogonal to filtered-chained predicate placement.
    Java resolves it (standard lateral ref) → real Go divergence, must eventually close (likely a small
    resolver fix once scoped). Discovered during the Slice-2b de-risk (the would-be x-column conjunct
    `WHERE X.K = v` is unreachable via this gap; the ⊆-outerLegs gate needs no special handling for it —
    an x-ref flows through the keep-rebase branch once the resolver closes).
  - [ ] **BOOKED (EXISTS-composition commit-1 follow-ups, RFC-173 S4 collision mint — see the RFC entry):**
    (a) **mint-per-leg for MULTI-SOURCE colliding EXISTS inners** — `EXISTS (SELECT 1 FROM OT OI, ST WHERE
    ST.c < OT.k)` with outer leg ST still answers per-outer-row (live Java: 3 rows, inner-shadow); commit 3
    of the slice lands the interim LOUD 0A000 decline, mint-per-leg closes the conformance gap for real.
    (b) **buildCorrelatedScalar sibling mint** — the correlated-scalar fallback shares buildCorrelatedExists'
    walk/qualify structure and therefore the same capture class; needs its own three-way-discriminating probe
    first, then the same mint. (c) **Java's array-constructor literal** (`INSERT … VALUES (1, 100, [10, 200])`,
    RelationalParser `arrayConstructor`) → Go 0A000 "unsupported expression atom ArrayConstructorExpressionAtom"
    — reach gap found by the live-Java probe; Java populates array columns via SQL, Go cannot. (d) **Case-2
    nested-EXISTS hoist dangles a middle-ref** — `EXISTS(SELECT 1 FROM M WHERE EXISTS(SELECT 1 FROM N WHERE
    N.b = M.a))` (middle has ONLY the nested EXISTS) hoists the inner plan and drops M entirely, leaving
    `M.a` unbound — pre-existing, unchanged by the mint (minted it resolves loud-deterministic instead of
    accidentally binding a same-named outer); needs its own probe + decline-or-fix. (e) **outer-CTE-leg scope
    registration** — buildOuterScopeSources.addSrc silently drops a resolve-failed leg, so an OUTER CTE leg
    (aliased or not) never enters the correlated subquery scope: correlated refs to it die 42703 (same family
    as the fixed derived-table registration), and an ALIASED CTE leg quoted `"Q$N"` escapes the mint's
    visible set (the unaliased half is folded via cteScopes names). Register CTE legs from the registry's
    ScopeSource like derived tables. SAME FAMILY: a correlated-fallback MIDDLE's JOIN-LEG names are absent
    from nestedOuterScopes (only the primary is appended), so an inner-inner ref to a middle leg that
    shadows a different-table outer alias silently binds the OUTER table — walk-time mis-binding, needs the
    same scope-registration fix. (f) **multi-EXISTS-over-unnest reach gap** (commit-2 review battery,
    live-Java-grounded): TWO sibling EXISTS subqueries over a lateral unnest fail to plan in Go ("best
    expression is not a physical plan"), colliding or not, correlated or not — pre-existing on the slice
    parent, LOUD, orthogonal to the mint. Java ANSWERS both shapes (constant-true pair → all elements;
    correlated pair OT.k>X AND MA.c<X → {10,20} on the probe fixture) → shared-surface parity gap, needs
    its own slice (likely the exists-filter path only carries one ExistsSubqueries consumer per unnest
    select). (g) **w4b in-tree quantifier mints** (rfc173_w4b_clustered_outer.go:149/:673 — outerCorr/
    innerCorr) mint raw UniqueCorrelationIdentifier into the SAME expression-tree namespace as user-named
    correlations — the translator-side half of the namespace law ("no generated identity equals a
    user-visible name"). No out-of-surface argument has been constructed for them; route them through a
    visible-name skip (the translator needs its own visible-set authority) or write the argument at the
    site. (h) **uncorrelated scalar-in-projection silent NULL + label leak** (commit-3a review battery,
    pre-existing, shape-general): `SELECT (SELECT MAX(C) FROM OT) FROM ST` returns NULL rows with the
    internal mint label in the column name (`(SCALAR_SUBQUERY Q$N)`), while the CORRELATED twin answers
    correctly — contradicts the scalar-subquery-in-WHERE booking's claim that projection-position scalars
    work; Java-probe first (silent-wrong candidate on the shared surface), then reconcile the bookings.
    (i) **name-set roles refactor** (logic-inert, own commit): buildCorrelatedExists now carries THREE
    purpose-built name sets — outerAliases (walk-time ref matching for the ON split), visibleScopeNames
    (mint-collision closure), and scopeAmbiguousName's bound set (runtime binding collision). Each is
    semantically forced and locally documented, but the mispick hazard is proven (the display-alias
    over-fire was exactly answering the binding question with the resolution set). Consolidate the
    DERIVATION under one scope-walk authority with role-named projections (displayNames / boundNames /
    allVisibleNames), each documented with the QUESTION it answers, consumed by name at each site.
    (j) **one outer-routing authority** (when the multi-EXISTS composition wall (f) falls): Case-1 has
    shipped outside the placement invariant twice; per-case discipline is proven insufficient. Consolidate
    ALL outer-routing decisions through one polarity-taking authority (OuterOnlyJoinConjuncts is the hook)
    so the invariant is enforced structurally; retires the exemption hazard class.
  - [x] **W3 the coupled 2-way flip — DONE, ALL ACKs.** W3a-1/W3a-2 (36297a253, fd07e2f49,
    140799069, d98bbac91, 139c6cb94); **W3b-1 LIVE FLIP** (1aca8addd + 47d3b48bb RFC log +
    00c7a206e Graefe notes + 5ead4e149 Torvalds nits): Graefe ACK (cross-leg baking BLESSED as the
    correct ruling-#2 scoping; premise correction + re-ruling recorded; spanAwareRow + driver
    positional read + oracleNameDatum ratified), Torvalds ACK (nits fixed: ordinalEligible stale
    doc, count_mismatch vacuity, reversed-star differential pin). Suite 54/54; dualwindow
    carve-outs EMPTY; stress: branch FASTER than master on all heavy scans (table below).
    **W3b-2 pins LANDED** (a3d323808 + fc653b4b0 fail-closed ON-drop fix — a PRE-EXISTING
    silent-cross-product bug the pins caught; Graefe ACK on both). **STANDING OBLIGATION (Graefe
    condition): gate pin (b)'s runtime-green half is BLOCKED on the pre-existing derived-with-join
    planner gap (join-bodied derived tables don't plan; identical on master). The solo control in
    rfc173_slice2_gate_pins_test.go goes RED the instant the planner learns the shape — when it
    does, convert the 0AF00 decline pins to rows+plan assertions (the evasion shape must plan
    name-model with correct rows).** Historical grind record:
    (~12 files: translator seed rfc173_ordinal_seed.go + gate revisions + executor fixes; commit
    blocked on green — pre-commit runs the suite). Fallout fixed so far (each = a gate/executor
    correction, all reviewed-in-principle): (1) LEFT OUTER = POISON — RewriteOuterJoinRule
    dissolves LEFT boxes post-translation, so translation-time opacity was FALSE (Graefe RE-RULED:
    poison confirmed; premise correction "opacity must span all phases" + conditions: pin RFC-153
    shape name-model e2e ✅, pin RewriteOuterJoinRule declines FULL ⏳TODO, dup-alias poison noted as
    S3/S4 unique-quantifier-alias item ⏳RFC); FULL OUTER stays gated; preserved leg ENCLOSED /
    null-supplying leg fresh. (2) JOIN legs categorically ineligible (nested bare concat erases
    buried aliases — S3 FieldPath territory). (3) Dup-leg-alias poison (FROM p JOIN p). (4)
    SelectMergeRule assert refined to SelectExpression children (filter-merge is legal). (5)
    flatMap outer binding = positional leg row (baked SARGs). (6) datumFromSpans: coexistence Datum
    = bare + ALIAS.COL + TYPE.COL fallback keys (mirrors mergeRows/qualifyTypeFallback);
    spanAwareRow resolves dotted names alias-first-then-type. (7) Predicate baking = CROSS-LEG
    CONJUNCTS ONLY (single-leg preds are pushdown fodder into name-model legs; lazy is sound there
    by the load-bearing invariant). **REMAINING RED: TestFDB_AmbiguousColumnStar
    (select_star_cross_join_all_cols + cte variant) — §5 dup-name SELECT * over cross join reads
    the wrong leg (cxName gets b's value). Diagnosis: compose-fold through the ordinal RC rewrote
    projection values from dotted FieldValue{Field:"CX.NAME"} to BAKED refs with display name
    "NAME" → executeProjection projNames (values.ProjectionColumnName→fv.Field) collide in the
    projected Datum map (last-wins) and/or ColumnDef.Name (deriveProjectionColumnDef: ExplainValue
    for Child!=nil) mismatches the Datum key. Fix direction: driver-side POSITIONAL read
    (resultset.go columnValue: when rs.current.Positional != nil and slots parallel columns, read
    slot columnIndex-1 — the §7 dup-name fix arriving via the positional row; verify projection
    slots ARE parallel to ColumnDefs, and guard non-projection rows) OR restore qualified
    projNames for baked refs. Then: conformance/dualwindow/plandiff targets re-run, full suite,
    commit W3b-1, Graefe+Torvalds re-request, RFC execution log update.** Then W3b-2 pin batch
    (GROUP-BY-over-2-way E2E, dup-named box-leg, EXPLAIN stability, dualwindow, stress
    before/after).
  - [x] **W4 — DEFERRED TO S3 (Graefe ruling, f034707eb RFC amendment).** The correlated-scalar
    2-leg seed is a pre-rewrite LEFT OUTER select (the ephemeral object the W3b premise
    correction covers) and the unnest port needs S3 FieldPaths — both moved into S3's scope with
    rationale; S3 honestly resized 4–5 shifts; the FINALIZED S2 wedge (pure inner/cross 2-way over
    non-join legs + FULL boxes over non-join legs) recorded as the definitive statement. **NO
    interning flip** (canonical sequence: Slice 3, unchanged).
  - [x] **W5** gauntlet → PR #447 MERGED (squash 7f7100199). Full four-gate trail at every HEAD
    (461c38074 branch content; 51e3327ca unification; final merges mechanically verified —
    single-parent hunks only). Gate pins (a)/(b) runtime halves LANDED in W3b-2 (a3d323808;
    pin (b) partial per Graefe with the conversion obligation above). PR description MUST state
    the contract delta (Graefe condition 4: reviewers review against the amended §4, not the
    stale text): premise correction, LEFT-outer poison, join-legs ineligible, W4 deferral,
    finalized wedge scope.
- [x] **Slice 3 — DONE, MERGED as PR #464 (rebase-merge `38454886a`, 2026-07-03).** Graefe's design
  gauntlet REFRAMED it from an "atomic deletion" to a **dispatch-authority flip + certification**
  slice: the premise "widen gate → `parentIsMerge` never true → trio dead" is FALSE (anchored seeds —
  scalar-subquery, multi-source unnest, non-wedge — stay constructible), so the trio + `AnchoredJoin`
  flag + seed constructors + `InternsAliasAware` anchored arm ALL move to Slice 4; W5 stays a separate
  post-green PR. This slice is strictly ADDITIVE instrumentation + six Q1–Q6 gate pins (dispatch
  `MergeArmHits`==0; Q2 plan-shape stability + byte-identical; Q6 STAR wall-clock; Q6 shadow-delta
  exact + load-bearing; Q4 legRowTypes bridge), no planner behavior change. All four gates at the final
  HEAD: Graefe ✅ (design + impl + delta), Torvalds ✅ (+ delta; nits folded), codex ✅ (caught + fixed
  a real P3 — Absorb dropped the loser's alias-aware dedup counter on `Memo.merge`, pinned by
  `TestReference_AbsorbFoldsAliasAwareDedups`), @claude ✅ ("the fourth ACK"). CI green.
- **Slice 4 is a GATED DEMOLITION, not a free delete** (6-agent producer-retirement map + Graefe
  boundary DESIGN-ACK, RFC §"Slice 4 — retire AnchoredJoin: boundary design"). After S3 the
  `AnchoredJoin` flag axis has ZERO individually-dead symbols (the flag is a field on the SHARED RC
  type) — undeletable while ANY of FOUR live seed producers survives. **Canonical sequence (each a full
  four-gate slice):**
  - [x] **W4b — correlated-scalar ordinalization — MERGED (PR #465), all four gates ACKed at the
    merge HEAD** (Graefe design+impl+5 delta ACKs · Torvalds 2 NAKs fixed → "merge it" ·
    codex 4 P2-class findings fixed → clean · @claude 2 passes + coverage-gap catch → gate closed).
    All three shapes landed; the gauntlet additionally found+fixed: silent-NULL computed scalars,
    unplannable/silent-NULL clustered outers, wrong-leg + wrong-scope parenthesized-column reads, the
    spanAwareRow literal-name routing gap, the name-model projection binder gap, and THREE silent
    text-corruption writes in UPDATE...SET (subquery/EXISTS/IN-subquery RHS — pre-existing on master,
    now clean 0AF00 declines with no-mutation pins). Exit gate amended per ruling: the constructor
    keeps exactly ONE production call site (ungated-outer rightmost-corr residual, pinned both
    directions) until W4-left/W5 gate every cluster; dies in S4.
  - [x] **W5 — multi-source lateral UNNEST** — MERGED (PR #466, master 1050831ff). Five commits
    + five gauntlet rounds; five real review findings fixed red-first (two silent-wrong
    production classes among them: unrewritten element-referencing ON conjuncts, and the
    PRE-EXISTING buried-unnest spanning-WHERE 0-row class); the §5 oracle rebuilt for gathered
    N-way tops (which also fixed the silently-broken plain-3-way oracle class); SELECT * over
    multi-source newly plannable with correct FROM-order metadata. Final tally on the merged
    HEAD: Graefe ACK, Torvalds ACK, codex clean, @claude clean; 1M stress green; 1621-entry
    dualwindow corpus green. Was IN PROGRESS on feat/rfc173-w5-multisource-unnest:
    (Graefe DESIGN-ACK, five forks ruled; charter amended per the F4 rider: the dotted classifiers go
    DEAD-FOR-GATED in W5, physical deletion rides the last dotted producer's killer; the
    under-existential class is re-chartered to the W4-left+EXISTS slice). Commit 1 LANDED: the
    gathered flat (N+1)-quantifier translation (Java's shape, per-source baked Explode correlation),
    the partition connectivity revisit, the gathered WHERE arm, white-box seed/decline pins, and the
    disjoint-schema FDB e2e (WSRC/WAUX — the only disjoint pair; every other corpus pair shares
    column names and correctly declines through the commit-1 ambiguity gate to the residual).
    Commit 2 LANDED (+riders): the span-derivation extension (single-accessor bare-non-record-QOV
    terminal synthesis), the ON-carrying/shadowed-element decline lifts, the phantom fail-open
    removal, case authority (bake emits UPPER), the Q6 nil-legRVs dimension pin. Commit 3a LANDED:
    the Q2 interning widening (IsOrdinalJoinRV admits bare TYPED QOV fields; STAR re-baselined
    51377→42788, -17%, chain/dispatch pins unchanged; typed/untyped pins). Commit 3b LANDED: the
    ENCLOSED class (`FROM A, A.arr AS x, B`) — rotation to the root form (gatherLegsWithBuriedUnnest
    → Join(Join(plain legs, FROM order), Unnest), collected ONs on the rebuilt root where the
    element is in scope), the translateFilter enclosed merge arm (rewrite+bake, the root form's
    treatment), and the pushBuriedUnnestPredicateDown stand-down when the gather fires — FIXING a
    pre-existing SILENT 0-ROW class (spanning WHERE over a buried unnest, `FROM A, A.arr AS x, B
    WHERE x > B.c`: the unpushable conjunct landed raw on the residual NLJ with the element unbound).
    Rotation/decline white-box pins + 5 enclosed FDB pins (incl. the fixed spanning class).
    Commit 4 LANDED: SELECT-*-over-multi-source metadata (the ordinal-top arm in
    deriveColumnsFromJoin, scoped to the _N leak; mixed-element INTEGER from the Explode; values
    via the §7 positional-aligned read) + FROM-order preservation through the rotation (unnestPos
    threaded; element mid-list) + the MANDATORY §5 oracle rebind: the dual-mode FDB differential
    ran RED (gathered family NIL/0-row oracle-side; the PLAIN 3-way merge-sub-product class was
    silently broken the same way, corpus-blind) → recoverOracleDatumSpans (NLJ twin of the
    FlatMap legRV recovery, DatumSpans-only), oracleSwapFusedDatum (output-shaped oracle Datum at
    all 4 emission sites), the values raw-map MERGE-SLOT-ONLY pinned bare arm (cut wider, the
    corpus caught a.k=b.k→k=k instantly). 7-query differential + 1621-entry corpus green.
    Commit 5 LANDED: the F4-rider dead-for-gated pins (gated=empty, emergent owner edge,
    residual dotted still classifies — the reachability proof deferring physical deletion).
    Remaining: the four-gate gauntlet (Graefe impl / Torvalds / codex / @claude on the PR).
    Pre-existing wart surfaced by the round-5 fix (NOT W5; master-identical, worktree-verified):
    an EQUIJOIN multi-way join plans through the correlated FlatMap path whose fold arm emits
    BARE duplicate column Names ([TAID K TBID K TCID K]) — the NLJ-planned cross form keeps
    qualified names. A by-name dup-column read over the FlatMap-planned form conflates today.
    Out of W5 scope; investigate with the metadata follow-ups.
    Review nit booked (metadata-only, later pass): arrayElementTypeNameFromDescs resolves a BARE
    collection-field name via descriptorForColumn, whose ambiguity rule is first-match — two
    joined tables sharing a repeated-field NAME with different element kinds could mistype the
    erased-type fall-through's STRUCT/scalar metadata (reachable only when the plan-level element
    type is erased AND the bare name is ambiguous across legs).
    Gauntlet ledger: probe-purity DONE (consume-once enclosedGatherCache, round 2). S4 rider
    kept: revisit deriveColumnsFromJoin's ordinal-top arm when S4 makes the positional read the
    sole authority (the structural IsPositionalMergeRC discriminator landed in round 4 and
    closed the leak-keying note; S4 may retire the arm outright).
    - RETRACTED (probe-verified with discriminating data): the "KNOWN-BROKEN spanning residual" was
      a PHANTOM — the commit-1 seeds (WV {5,6} vs elements {7,8}) made `EL > WV` all-true, so the
      all-rows result briefly read as a dropped predicate was the CORRECT answer. With WV {5,6,7}
      the spanning WHERE filters correctly through BOTH the gathered path and the residual (and the
      "P2a-vs-disjoint divergence axis" does not exist). Pins corrected to discriminating data.
      Commit 2 lifts the now-unnecessary `predSpansUnnestAndLeg`/`forceUnnestResidual` fail-open.
    - Commit-2 investigation results (probe-verified end-to-end): the case-A dotted-resolution
      mechanism over the flat output = LAZY dotted names + runtime span routing
      (`downstreamLegWindows` → `ordinalJoinSpansOf` with legRVs → `spanAwareRow.GetByName`); the
      R18 divergence = `resolveSpanLeaf` stops at merge quantifiers for SINGLE-accessor paths, so
      the mixed (no-AT) bare-QOV element yields partial leg coverage → windows decline → strict
      positional context → loud miss (the AS+AT full-baked ON-carrying gathered shape ALREADY WORKS
      end-to-end). Commit-2 core = the ~40-line span-derivation extension (synthesize the 1-field
      element leg for a single-accessor bare-non-record-QOV merge slot — the existing :145-155
      synthesis pattern), then lift the ON-carrying decline AND the spanning fail-open. Latent
      hazards flagged: alias-case at the gather boundary (bake preserves original case, gather
      uppercases — normalize + pin); the NLJ birth's nil-legRVs Datum dimension.
    - Pre-existing (NOT W5, worktree-verified on master): a filtered cartesian with only per-leg
      WHERE conjuncts (`FROM ca a, cb b, cc c WHERE a.id=1 AND b.bid=10`, no cross-leg predicate)
      CANNOT PLAN (0AF00) — a partition gap, booked so it is not mistaken for a W5 regression.
      (The no-WHERE dotted 3-way works.)
  - [x] **W4-left + EXISTS + recursive-CTE joins** — MERGED (PR #467, rebase; four-gate tally:
    Graefe ACK, Torvalds ACK, codex clean over three delta rounds, @claude clean over two
    hand-traced passes + tail; CI green; 1M stress green; live-Java SeedRunCorpus green)
    (Graefe DESIGN-ACK, 4 conditions; the F3 divergence recorded in DIVERGENCES.md). Commit 1
    LANDED: single-source LEFT/RIGHT boxes gate with the at-translation ordinal seed (I2
    declaration order, I3 record-level nullability); the RIGHT SELECT * metadata reversed-check
    ordinal arm; the W4b decline class now ANSWERS (pre-chartered pin flip). Commit 2 LANDED:
    EXISTS-over-join — the I2 latent RIGHT+EXISTS column-order fix, the OUTER-flatten
    WHERE-as-ON silent-wrong fix (both master-identical, red-first), and the F2 ORDINAL
    existential rebase (baked merged-positional offsets; name-model rebase dead-for-gated,
    FrontierPinned-policed; un-mappable refs decline CORRECT-or-LOUD). I1 closed by
    construction + pinned e2e (condition-4 matrix: EXISTS/NOT/non-correlated all green).
    Commit 3 LANDED (recursive-CTE truth pins: reference joins gate ordinal since the fulcrum;
    definition-node poison production-unreachable). Commit 4 LANDED (column-aware dup-alias
    rejection, Java 42702 byte-equal, F3 REVISED against live Java runs — 2 marked divergences
    + 1 parity entry). Commit 5 LANDED (exit gates: producer audit in the RFC — the INNER
    flatten stays anchored, its ordinal seed corpus-reverted twice pending data-access
    positional binders; the W5 F4 rider resolves to S4 for classifier deletion; budgets held;
    1M stress green; Java harness green). Graefe IMPL-ACK conditions LANDED (layout
    consolidation into values.OrdinalSeedLegWindows + the QP-REF-BIND charter). Gauntlet
    round 2 LANDED (three commits): column-aware dup-alias for EVERY leg kind (derived/CTE
    column derivation, all-priors tracking, 42F01 unmasked — live-Java-verified corpus
    entries); mixed outer-nesting soundness (the demanded runtime pins caught TWO pre-existing
    MASTER bugs — fabricated ALIAS.* keys from merged-row bare keys = wrong-source rows, and
    the nondeterministic anchored re-enumeration panic — both fixed; enclosure guard on the
    outer gate arms; ordinalSeedFromAnchoredLeft deleted); the INNER-only flatten contract.
    Gauntlet round 3 LANDED: generated output names (unaliased aggregate/computed text) join
    the dup-alias collision check (a."COUNT(*)" over duplicate legs read one side silently —
    red-first both directions; live-Java-classified corpus entry, message drift) + the two
    review-condition comment fixes. All four gates ACK'd the final HEAD.

  - [ ] **S4 atomic demolition** (LAST — gated on QP-REF-BIND items 1+2+3, the riders, and the
    unnest-residual slice; sequencing ruling banked in the RFC). **Gates now satisfied:** items
    1+2+3 merged (item 3 = #483); rider 2 (aggregate metadata) DONE on feat/rfc173-next; rider 1
    folds into S4 (flips as the ordinal seed widens); unnest-residual c1/c2/c3 DONE on
    feat/rfc173-next. **The correlated-scalar prerequisite is DISCHARGED (W4b #465)** — JOIN-inner
    + COMPUTED-scalar ordinalize; the lone `NewScalarSubqueryAnchoredRecord` producer is now the
    UNGATED-OUTER residual (cascades_translator.go:3860). **Reachability: strong evidence :3860 is
    near-dead.** Probed with a loud marker: (1) no existing sqldriver/core-query/embedded test hits
    it; (2) direct ungated-shape probes — a correlated scalar over an UNNEST outer
    (`SELECT c.name,(SELECT COUNT(*) FROM o WHERE o.cid=c.id) FROM c, c.arr AS x`) does NOT reach
    :3860 (it declines at PLANNING, "could not plan query" 0AF00 — a pre-existing limitation, not
    the anchored fallback); a dup-alias outer declines loudly in buildCorrelatedScalar; a
    plain-single outer ordinalizes. So the research's assumption that unnest-outer "keeps the
    anchored record" is WRONG — it declines elsewhere. **BUT S4 is NOT near-done: only the SCALAR
    producer (#1) is near-dead. The name model's CORE is heavily live** — `NewAnchoredJoinRecord`
    (producers #2 unnest + #3 join) at cascades_translator.go:318/:698/:1528 (every join + unnest
    result value), and `NewReEnumerationAnchoredRecord` (#4) at rule_partition_select.go:469
    (GROUP-BY over an anchored parent). Retiring these = making EVERY join/unnest/group-by
    ORDINALIZE = the full RFC-173 endgame, a fresh multi-shift campaign (the RFC's ~2-shift estimate
    is on top of these three producers, NOT just :3860). **Remaining for the S4 exit gate:** the
    empirical zero-producers proof must cover ALL FOUR producers (probe #1's last existential-ON
    shape; then retire #2/#3/#4 by ordinalizing their shapes), then the atomic deletion. Gated on a
    Graefe RFC DESIGN-ACK of the demolition plan + all four impl gates (codex incl.). See the
    corrected RFC Slice-4 PREREQUISITE block.
    **STALE-NOTE CORRECTION (the ":1102 core heavily live" para above predates the S4 slices):** every
    join/unnest/group-by shape now ORDINALIZES — the census sweep (TestRFC173CensusSweep, 18 families
    incl. join2/leftjoin/fulljoin/join3/unnest*/groupby*/exists*/box) fires 0 P4/P5 producers, and E-1b
    zeroed the LAST EXISTS family. The ordinalspike CERTIFICATE is GREEN at 505fc32c9 (1641 corpus
    entries, 0 carve-outs, 0 mismatches) — the force-ordinalize path is result-identical to the name
    model across the whole corpus, PROVING the atomic cap is safe to fire. ReEnumeration (#4) is
    downstream of P4/P5 (fires only for anchored parents → dies mechanically); legColumns:382 is a
    name-DERIVATION helper (fires on the ordinal path too), not a seed producer. **Demolition is
    UNBLOCKED** — remaining: Graefe design-confirm the fire + the atomic deletion commit + 4 impl gates
    + exit gates (dualwindow/1M-stress/rfc153).
    **E-1b DONE (505fc32c9, 4-gate ACK — Graefe design+impl, Torvalds, codex, @claude):** the Bakeable
    leg-conjunct INNER cluster under EXISTS (`… A.K=100 AND EXISTS(…)`) ordinalizes via the one-authority
    classifyLegConjunct + in-select gatedJoinLegTypes bake; the review round added the enclosed-CTE-leg
    seedWindowed guard (P1) + the real flat-leg drift safety net (bakeGatedJoinPredicatesChecked).
    **FULL-SUITE FLIP MEASUREMENT (ed19761c1) — the cap is ONE blocker away.** Graefe NAK'd firing on the
    1641/0/0 corpus cert (it's structurally blind — no array cols — so it never exercises the producer
    deletion). The REAL exit gate is the full-suite flip: forceOrdinalSpike=true over //pkg/relational +
    //pkg/recordlayer, panic at rule_partition_select:475 temporarily neutered so it runs to completion.
    RESULT: 2/33 targets red, 0 panics, **0 SILENT wrong-rows** — every real red is a LOUD "field LA.K not
    resolvable in the runtime row — malformed plan" (the RFC-077 group-by panic class is GONE). Reds =
    (A) name-model-assertion tests that RETIRE with the cap [Item2C4/C3, WedgeGate, BareTwin does_not_bake,
    FilteredBox scalar_subquery_unbakeable — delete/rewrite IN the cap commit] + (B) ONE real blocker: the
    ENCLOSED BOX-LEG. ROOT CAUSE: a plan-time PROJECTION bake miss — the box-leg's per-leaf windows exist
    (gatedJoinLegTypes/addBuriedBakeWindows, used by the PREDICATE channel) but the PROJECTION channel
    (SELECT LA.K in a CTE body / output cols / aggregate) leaves box-leg column refs as name-model
    FieldValue("LA.K"), which the ordinal runtime row can't resolve (values.go:586, Ordinal:-1). THE SLICE:
    bake enclosed box-leg column PROJECTION refs to their windows — the projection twin of
    bakeGatedJoinPredicates. Locus: translateProject (cascades_translator.go:4724) + output-col derivation
    (cascades_generator.go ~2733-2876, the "output order from result-value Type ordinals" hard part).
    **GRAEFE DESIGN-ACK (the endgame ruling):** (1) it's ONE windowing slice — verify scalar-conj-over-box
    (FilteredBox) is windowing not a verdict split; (2) the flip (default=true, panic-neutered) is the
    STANDING exit-gate dashboard, BUT the cap must make :475 not-panic because the box genuinely windows
    (never ship the neuter), greenness is suite-scoped ("suite-green + complement is loud", state in the cap
    commit), and enumerate the retire-(A) tests explicitly; (3) agg_count_over_gather FLIPS from booked
    plan-quality → CAP BLOCKER (the cap deletes its name-model fallback) — same windowing root, structurally
    aggregate-over-EXISTS-wrapper (INNER not box), one slice or paired follow-up; (4) before firing, run the
    flip on the B2-INCLUSIVE tree + verify a DISCRIMINATING dup-column shape. Fire the cap (2 circular
    declines + producers + ReEnumeration + §5 oracle, ONE commit) when the flip is 0-red-except-retires.
    This slice is the handoff's flagged "fresh focus / 0-row-planner-bug class" — the next focused push;
    red-first is the flip dashboard. See scratchpad/flip-measurement-505fc32c9.md.
    **ENCLOSED-BOX SLICE PROGRESS (flip-dashboard-driven, all Graefe-design-locked):**
    - PART 1 LANDED (55c9d43f5): route the ENCLOSED box-unnest through translateGatheredUnnestCluster
      under the flip (cascades_translator.go:1510 `|| forceOrdinalSpike`), so a box outer's ON stays the
      null-on-empty condition INSIDE the dissolve (Graefe (2)(b): keep the ON inside, NEVER window it →
      LEFT→INNER). BYTE-IDENTICAL in production; verified LEFT-preserving (2 rows not 0). Cleared the
      GraefeImplProbe2 enclosed-CTE-over-box class (~10 shapes).
    - 🚩 STRADDLE CAP-BLOCKER (deferred to the cap, Graefe ruling; NOT production-reachable — flip-only):
      boxbase_straddle (`… T4, T4C, T4.SARR AS X … WHERE T4.ID=Z`) 0-rows under the flip (name-key read
      of a box-base column over an ordinal multi-leg box base). The box-base-local locus is architecturally
      incapable (3 attempts reverted — both shapes identical at the box base; only the ABOVE straddle read
      differs, invisible to translateChainedUnnestJoin). FIX AT THE FILTER-REBASE SITE, IN THE CAP COMMIT
      (atomic with the enclosure-decline deletion). Cap-fix: TRY leg-window resolution of the straddle read
      (T4's OrdinalSeedLegWindows) → correct 3 rows; 0AF00 decline only if the qualifier is ambiguous.
    - LOUD reach gaps (Graefe: schedule, convert runtime strand → plan-time decline for cap-cleanliness):
      FullBoxChained, scalar-subquery-conjunct (the Unbakeable-verdict split — partial-bake the leg refs,
      leave the scalar as a bound comparand, a separate slice). Both already correct-or-loud.
    - 🚩 agg_count_over_gather CAP-BLOCKER (root-caused, same class as the straddle — ordinal-child-under-
      name-model-parent): the A,B cluster ordinalizes under the flip but E-1a's underAggregate decline
      (cascades_translator.go:2766) keeps the aggregate-over-EXISTS handling name-model → the EXISTS
      correlation reads A.K/A.AID over the windowless NLJ → strand (LOUD malformed, not silent). FIX at cap:
      remove the underAggregate decline + expose the gathered seed's windows through the existential-wrapper
      NLJ to translateAggregate's gatheredSeedBakeContext. JAVA-PARITY (lateral chained unnest IS Java —
      generateCorrelatedFieldAccess; the corpus's no-array-columns is a CORPUS gap) ⇒ ORDINALIZE (COUNT(X)=2),
      NOT loud-decline (which would be a conformance regression). Same for the straddle. COMPLETE atomic-cap
      execution plan (both blockers' fixes, reach-gap dispositions, cap mechanics, exit gates) in
      scratchpad/flip-measurement-505fc32c9.md. EXIT-GATE PROVEN CLEAN: full flip = exactly 1 silent-wrong
      (the booked straddle), all else loud/retire — the cap is safe to plan on the correctness axis.
      ✅ **agg cap-blocker DONE (f67a2c8c7 + 914aa226d, 4-gate in flight):** the DIRECT aggregate-over-EXISTS
      ordinalizes — gatheredSeedBakeContext peels the single semi-join wrapper (E-1a's under-aggregate decline
      masked a nil-seedQOV bug: seedElementSlots looked for the Explode as a direct quantifier; under EXISTS
      it's one level down). COUNT(X)=2/SUM(A.K)=200/GROUP BY X all bake correctly, 0 producers. Review (codex)
      caught that removing the decline WHOLESALE was too broad — CTE/DISTINCT-WRAPPED aggregates qualify the
      group key with the wrapper alias (unbakeable over the seed windows) → silent NULL/collapse. First fix
      NARROWED admission to the direct shape (declined wrapped → name-model); superseded — see below, the
      wrapped case now ORDINALIZES. Multi-EXISTS-under-aggregate stays name-model + LOUD (pre-existing
      planner gap, confirmed at parent — agg_multiexists_loud sentinel).
      🔬 **JAVA CONFORMANCE (6-reader workflow, HIGH confidence):** Java 4.12.11.0 FULLY supports GROUP
      BY (grouped+global COUNT/SUM/AVG/MIN/MAX via streaming aggregator, no index required — AstNormalizer
      rejects only OFFSET/LIMIT). The old translateAggregate comment claiming Java lacks GROUP BY was
      STALE/FALSE — corrected. Java ALSO plans GROUP BY over a multi-source-FROM derived table
      (GroupByQueryTests.java:699 `SELECT max(y) FROM (SELECT y,b AS L FROM t1,t2) AS q GROUP BY l`), and
      a CTE ≡ derived table structurally → the WRAPPED shape (CTE+lateral-unnest+GROUP BY) is Java-parity.
      ✅ **WRAPPED CASE NOW ORDINALIZED (no post-cap deferral — a query answering on master must not
      regress at the cap):** replaced the direct-gate decline with a recursive identity-wrapper seed walk
      — gatheredSeedBakeContext's findWindowedSeed walks the outer-quantifier chain through IDENTITY
      wrappers only (SELECT-*, DISTINCT; never a row-reshaping GROUP BY) to reach the seed, and
      slotInGatheredSeed resolves a BARE leg column (D.AID → seed A-leg window) when exactly one leg
      carries it. Ordinalizes correctly in BOTH flip states (works under the demolition) for IDENTITY
      wrappers at ANY depth — findWindowedSeed's walk is UNBOUNDED (visited-set over the finite plan
      tree), so a deep (≥6) DISTINCT chain reaches the seed rather than exhausting a bound and skipping
      the bake → NULL (Torvalds-caught depth cliff). Pins: agg_cte_groupby_leg,
      agg_cte_distinct_groupby_element, agg_cte_deep6_distinct_groupby_leg.
      🔒 **PROJECTING-CTE-aggregate class → correct-or-loud LOUD (review-caught silent-NULL, Graefe +
      @claude):** a subset/reorder/qualified derived source (`SELECT A."AID"`/`SELECT B.BID, A.AID, …`)
      reshapes the row — findWindowedSeed stops at the LogicalProjection (can't bake against the reshaped
      layout) AND the name-model path mis-names the projected column (output "A.AID" ≠ D-stripped key
      "AID" → NULL, PRE-EXISTING per @claude's parent differential). Both give a silent NULL group, so
      translateAggregate refuses the whole class LOUD (UnbakeableProjectedGatherError) via the
      positionalGatherUnbaked detector. Pins: agg_cte_projecting_single_loud, agg_cte_qualreorder_loud.
      🚩 **CAP-BLOCKER (booked):** ordinalize the projecting derived source CORRECTLY — resolve the group
      key against the PROJECTED output's own layout, not the pre-projection seed (same as the enclosed-
      box-leg PROJECTION bake). Java answers it (GroupByQueryTests:699 projects `SELECT y, b AS L`), so at
      the cap this must ordinalize, not decline-to-loud. Only the STRADDLE + this projecting-derived
      ordinalization remain as booked cap-blockers.
      📌 BOOKED (Graefe, separate follow-up, no correctness urgency — Verdict-None is conservative =
      correct-or-loud already): investigate whether the NON-aggregate E-1a admission (the executor-hoist
      path) can route through a translator-side bake (like gatheredSeedBakeContext) instead of the hoist,
      and whether its surviving `Verdict-None only` restriction is consequently over-conservative (same
      NLJ-hoist misattribution the aggregate fix disproved). Reach question, not a cap-blocker.
    **FLIP-DASHBOARD RED CLASSIFICATION (Graefe guardrail — the cap fires only when every red is one of):**
    (1) booked CAP-BLOCKERS fixed IN the cap (straddle 0-row, agg, FullBox/scalar-conj plan-time declines);
    (2) RETIRE-TESTS deleted at the cap (name-model-assertion tests: Item2C4/C3, WedgeGate, BareTwin
    does_not_bake, FilteredBox scalar_subquery_unbakeable); (3) NEW/UNEXPECTED → STOP + root-cause. A 0-ROW
    IS NEVER A RETIRE-TEST.
    **E-1a DONE (ce2777bc3, 4-gate ACK):** the INNER flat cluster under EXISTS ordinalizes (translator
    alias-aware leg+element bake over the seed windows; the NLJ physicalization drops the windowed layout
    so the executor hoist can't recover it — bake in the translator). Zeros its P5 firing. BOOKED
    plan-quality follow-up (Graefe): AGGREGATE-under-INNER-EXISTS ordinalization — E-1a DECLINES the INNER
    admission under t.underAggregate (COUNT(X)/GROUP BY X would collapse to one NULL group;
    gatheredSeedBakeContext needs the raw seed, hidden behind the existential wrapper's NLJ). Name-model
    handles it correct-or-loud today (pinned agg_count_over_gather → COUNT(X)=2); ordinalizing it needs
    exposing the seed through the wrapper to the aggregate bake — same class as the box's deferred cases.
    **B1 DISCHARGED (corpus-level ordinal-spike certificate GREEN):** the whole-gate
    force-ordinalize flip (`query.SetForceOrdinalSpike` guarding the three circular declines
    :143/:157/:190, in `pkg/relational/conformance/ordinalspike`) preserves EVERY corpus row —
    **1641 entries, 0 carve-outs, 0 mismatches**. This PROVES the atomic cap (flag + producers +
    §5 oracle deletion) is SAFE TO FIRE: any shape the cap would change now surfaces as a spike
    mismatch, never silently. The certificate retires with the name model. Remaining before the cap:
    B2 (item-3/unnest-residual/rider-2 to master) + the atomic deletion itself (codex-gated). Then:
    delete the flag + trio + the
    three seeds + `NewReEnumerationAnchoredRecord` (dies mechanically) + 8 value-layer flag
    branches + 4 executor consumers + `select.go:251` arm-1 + the §5 oracle (load-bearing until
    now) — ONE commit, which also LIFTS the W5 bare-twin duplicate-column decline (the
    circularity cut: e2e matrix must row-verify that class). Kill-list amendments recorded:
    OrdinalFieldName SURVIVES (ordinal infrastructure); `select.go:274` survives for the
    CTE-rename slice. Exit gate: EMPIRICAL zero-anchored-producers proof (caller-free
    constructors, exhausted decline reasons), never inventory argument.
    S4 rider (item-3 c4 review follow-up): extend the span/window cross-agreement
    harness with DATUM-KEY accounting — datumFromSpans was a fourth layout site that
    stayed misaligned while the three window sites agreed (the silent-zero aggregate
    inversion); the FDB cells tripwire it per-shape today, but the class closes
    structurally only when the harness compares Datum keys too. Moot if S4 retires
    the coexistence Datum outright — decide at demolition time.
  - [x] **F2-LEFT DONE** (projected-EXISTS over LEFT JOIN, scan legs — Java-parity reach gap: Go
    rejected 0AF00, Java answers; live-verified 4.12.11.0). The translator INNER-guard lift builds a
    JoinLeftOuter select; the executor routes it through the commit-2 **ORDINAL** path
    (correlatedStep1 false on the undissolved LEFT → gatedSeedStep1 true) + a JoinLeftOuter NLJ that
    null-extends the null-supplying leg **positionally** — NO name-model `:698` producer (verified: the
    scan-leg query never hits buildJoinResultValue). Buried box `(a JOIN b) LEFT JOIN c` DECLINES
    cleanly (`isScanFamilyLeg` gate → §8 → 0AF00) rather than mint a producer; INNER keeps its
    buried-box `:698` (task-scope, no null-extension) — asymmetry deliberate. noop-(J) flipped
    decline→rows; live-Java-parity corpus entry added → **dualwindow green BOTH windows, 0 new
    carve-outs** (S4-ready, not name-model-dependent). Pins: 3 scan-leg FDB (null-padded/star-null/
    uncorrelated) + buried-box clean-decline + `isScanFamilyLeg` white-box. RIGHT-outer booked
    follow-on (needs the operand swap; no JoinRightOuter in the fold's JoinType). **The
    conformance differential caught an ORDER-BY trap** (first commit reverted): the FOLD is Java
    parity, but `ORDER BY` on top makes Java's Cascades fail-to-plan while Go handles it (a
    Go-beyond-Java reach) — so all tests use the VERIFIED ORDER-BY-free shape, sorting in Go. Four-gate pending.
  - [ ] SEPARATE later slices (F2/F3): CTE-rename `select.go:274` widening (gated on CTE-column-rename
    ordinalization); lazy name-identity arm deletion (gated on FULL FieldValue baking, NOT S4).
- [ ] Slice 5 closure invariant · [ ] Slice 6 extensions + ANSI headroom.

## 🔖 RESUME AFTER 173 — where we pick up (do not lose this)

RFC-173 is a **detour**. We reached it while executing the RFC-164 umbrella (the `/goal`): WS-2
correlation-completeness turned out to be blocked on the column-resolution model, so 173 is both the
unblocker and the substrate. When 173 lands, un-freeze and resume **in this order**:

1. **RFC-164 WS-2 — FINISH IT.** Correlation-completeness invariant becomes always-on **for free** via
   RFC-173 Slice 5 (closed-by-construction). Then re-evaluate the three **field-level** invariants
   (set-op comparison-key columns, `COVERING ⇒ referenced-fields`, result-type consistency) — RFC-164
   marked them "un-checkable on the untyped extracted plan"; RFC-173's **typed/ordinal rows likely
   unblock them**. This is the direct continuation of what 173 detoured from.
2. **RFC-164 WS-3** — `RecordQueryPlanVisitor` port + `comparandReadsField` audit (single-source-of-truth).
   Mostly independent of 173.
3. **RFC-164 WS-4** — map-iteration lint + **Phase-1b InJoin inner-tie determinism**; the latter is
   **RFC-167 Phase 1b** (comprehensive tie-resolution), documented in merged #418. Plan shapes converge
   to Java after 173, so re-check these against the new shapes.
4. **RFC-164 WS-1** — generative Go-vs-Java row differential. Heavy-infra (live Java + FDB), multi-week.
   Untouched by 173.
5. **3 open Cascades hunt bugs** (from `hunt/cascades-bug-hunt`) — own focused Graefe cycle each.
6. **INFRA — Nightly Fuzz** (see item directly below): dispatch-timing bug. The **red nightly runs** are
   RESOLVED — root-caused to a Go stdlib race that leaks `-fuzztime` expiry as a test failure, fixed by
   `cmd/fuzzrun`; see "INFRA — Nightly Fuzz false reds" below. Only the dispatch-timing item remains open.
7. Then the numbered-phase execution order below (lowest-numbered unchecked, gates satisfied).

**RFC-164 `/goal` status:** WS-5 done (audit); WS-2 nil-child + WS-3 isCountStar + WS-4
cost-monotonicity/IN-determinism landed (#411–#419); WS-2 correlation-completeness + field-level,
WS-3 visitor, WS-4 map-lint/tie, WS-1 all remain → the list above.

### [ ] POST-RFC-173 reach extension — ordinalize the FULL-OUTER-box chained lateral-unnest straddle
RFC-173 S4 cap **loud-rejects** a chained lateral unnest whose spine bottoms in a FULL OUTER box
(`SELECT … FROM A FULL OUTER JOIN B ON …, A.arr AS X, X.sub AS Y WHERE A.id = …`, and the nested
`(A LEFT B) FULL C` variant) — see `chainedSpineBottomsInFullBox` + the reject in
`translateChainedUnnestJoin` (`rfc173_w5_chained_unnest.go`), error "lateral unnest over a FULL OUTER
JOIN with a join-leg predicate is not supported". This is **Java-aligned** (Java rejects FULL OUTER JOIN
at the grammar level — `RelationalParser.g4` `joinPart` has no FULL alternative), so it is NOT a parity
gap — it caps a **Go-only extension** rather than sink a per-leg-window composition into a shape Java
cannot express. The plain FULL-box unnest (single link) and the UNFILTERED chained FULL-box spine already
ordinalize and keep working; only the box-leg-predicate straddle + nested-outer-box bottom reject.
**To make it work** (reach beyond Java, optional): give INNER/FULL clusters per-leg window composition into
the chained ordinal seed (`boxOuterBirthsPositional`/`boxGatesFresh` are OUTER-birth only today), resolve
the box-leg conjunct through the per-leg merge window in `unnestExistsSeedSafe`, then drop the reject.
Tests pinning the reject: `TestFDB_RFC173S4_FullBoxChainedSpine` (box-leg-filter + `nestedbox_*` cases),
`TestFDB_RFC173S4_ThreeLinkFilteredOrdinalizes/fullbox_bottom_boxleg_filter` — flip those `wantReject`
cases back to row assertions when ordinalized.

### [ ] POST-RFC-173 reach extension — ordinalize the box-leg-WHERE straddle over a chained LEFT/RIGHT outer box
The nested LEFT/RIGHT outer box under a chained lateral unnest (`(A LEFT B) LEFT C, A.SARR AS X, X.SUB AS Y`)
now **ordinalizes** for the element / leg-projection / element-or-AT-WHERE / deeper-link shapes (S4-B:
`chainedSpineWalk` admits a gated LEFT/RIGHT box bottom + the `SelectMergeRule` dissolved-box barrier lets
the box physicalize). But a **box-leg WHERE conjunct** over it (`… WHERE C.ID = 110`, references only box
legs, no chain element) is **loud-rejected** (`chainedSpineBottomOuterBox` + the reject in `translateFilter`,
error "WHERE on a join-leg column of an OUTER JOIN under a chained lateral unnest is not supported"). It is
the un-ordinalizable straddle: the chained merged-corr rebase bakes onto the previous unnest alias, which
**collides** with the first link's own inner Explode quantifier (a pushed-down `ofOrdinal` binds to the
element row, not the merged seed → ordinal-(-1) strand); and baking it onto the box quantifier at the first
link lets `PushFilterBelowJoinRule` **sink it below the nested outer null-extension** into the null-supplying
scan (LEFT→INNER, silent wrong rows). The name-model residual strands at physicalization too, so there is no
correct representation today — reject (correct-or-loud) rather than ship wrong rows. **To make it work**:
inject the box-leg conjunct into the FIRST-LINK box select on a NON-colliding quantifier AND teach the
pushdown to keep a positional box-leg predicate above the nested outer null-extension (the direct
non-chained nested box already does this — its box-leg WHERE plans as `PredicatesFilter(box, [pred])` above
the box). Tests pinning the reject: `TestFDB_RFC173S4_NestedLeftBoxChained` (`chained_boxleg{A,B,C}_filter`)
— flip those `wantReject` cases to row assertions when ordinalized.

### [x] Executor/ordinal-binding — LEFT OUTER PK-join source-relative ordinal over a multi-leg row — FIXED
A `LEFT OUTER JOIN` on the **primary key** (`l.id = r.id`) combined with a **WHERE filter** (either side —
an L-side `l.b IN (…)` triggers it too, seed 980000056) AND an **ORDER BY on the left key** used to plan
successfully but **die at execution**: `correlated FieldValue "ID" (correlation "L") … multi-leg row
cannot serve a source-relative ordinal — no frontier row resolved`.

**ROOT CAUSE (traced via instrumentation, RFC-182 shift):** the plan is
`InMemorySort(InJoin(PredicatesFilter(FlatMap(join))))`. The in-memory sort resolves a source-relative
ORDER-BY key (`l.id`, addressed to L's own leg window) over the join's MERGED multi-leg row via LEG WINDOWS —
`executeInMemorySort` calls `legWindowRowContext` when `downstreamLegWindows(sort.GetInner())` returns
`windowsOK=true`. But `unwrapToJoinPlan` (ordinal_join.go) — the passthrough walk that finds the join below
the sort — did NOT list `RecordQueryInJoinPlan`, so a `… IN (…)` filter (which lowers to an InJoin between
the sort and the join) broke the walk → `windowsOK=false` → the sort key hit a bare multi-leg row and the
correlated fall-through guard went LOUD. The `IS NULL`/equality specifics never mattered; the trigger was
"an IN-filter (InJoin) between an in-memory sort and a join whose ORDER-BY key is source-relative."

**FIX (`ordinal_join.go`):** an InJoin re-emits the inner join's merged rows VERBATIM under a per-in-value
binding (executeInJoin: `WithBinding` then `ExecutePlan(GetInner())` flat-mapped — no projection/transform);
the binding changes row count/content, never the row LAYOUT, and leg windows are a purely positional property.
So InJoin IS a row-layout-preserving passthrough — added it to `unwrapToJoinPlan`. The doc's prior
"deliberately left out (re-executes under bindings)" was overly conservative. Regression:
`TestFDB_LeftJoinPkOrdinal_InJoinSortRegression` (asserts `[(2,2),(1,1)]`); knownGaps entry removed;
`TestKnownGaps_LedgerIsNarrow` now asserts the retired signature is NO LONGER declined.
**Gate:** executor change → Graefe+Torvalds BOTH ACKed (delta re-confirm pending on the final head). The fix
covers BOTH lowerings of `… IN (…)`: `RecordQueryInJoinPlan` AND `RecordQueryInUnionPlan` (the sibling both
reviewers flagged — same verbatim-inner layout-preserving property; ImplementInUnionRule allows the inner to be
a join, so the merged multi-leg row can flow up under it). Unit-pinned in `TestDownstreamLegWindows`
(in-join / in-union / in-memory-sort-over-each).

### [ ] Conformance harness — per-step idempotency-aware retry for mid-request Java-server drops (residual)
The `POST /invoke: EOF` conformance flake's COMMON cause (reusing a keep-alive connection the pooled Java
server closed while idle) is fixed by `newJavaHTTPClient`'s `DisableKeepAlives` (fresh connection per call,
pinned by `TestJavaHTTPClient_DisablesKeepAlives`). RESIDUAL: a server drop DURING a request (GC pause /
crash mid-`/invoke`) a fresh connection cannot prevent. A blanket retry is UNSAFE — `Invoke` is generic and
some `/invoke` steps write, so a retry could double-apply. The fix is a per-step idempotency map (retry only
read/plan steps like `RunSql SELECT`/`EXPLAIN`; never retry a write step) driven off the transport-error class.
Low priority (mid-request drops are rare; keep-alive fix covers the observed flake).

### [x] Soundness — empty IN-list `x IN ()` — INVESTIGATED; InUnion hardened by RFC-190.11-FU
`executeInJoin` short-circuits an EMPTY in-value list by running the inner plan directly (unbound) — which
would return the inner's rows where SQL yields ZERO for `x IN ()`. Surfaced as a possible soundness divergence
(Torvalds review point on the multi-leg PR). **InJoin reachability verified — the branch cannot be reached by
any query the engine plans:**
- literal `IN ()` is a PARSE ERROR — the grammar's `inList → '(' expressions ')'` and
  `expressions → expression (',' expression)*` require ≥1 element;
- `IN (subquery)` lowers to a semi-join / projected `ExistsValue`, NOT a static-values `RecordQueryInJoinPlan`
  (whose `inValues` is specifically the STATIC literal comparand, in_join.go:106);
- there is no parameter-IN path that builds an empty-static-`inValues` InJoin (`executeInJoin` reads only
  static `GetInValues()`, and the translator has no `InParameter`→InJoin lowering).
So the InJoin `len(inValues)==0` arm is a defensive fallback that no valid plan reaches and remains as-is.
`InUnion` is no longer part of that residual: RFC-190.11-FU distinguishes its absent outer source slice
(the established pass-through constructor shape) from a present empty dimension, which now returns an exact
empty cursor without executing the child. Direct tests cover a lone empty dimension and mixed unknown/empty
dimensions in both orders.

### [x] Executor/ordinal-binding — 3-way join shared-column ordinal not resolvable — FIXED
**FIXED** (branch fix-threeway-shared-col-ordinal, stacked on #501). Root cause: when
`PushFilterThroughFetchRule` pushes the join key `m.c = r.c` below the index Fetch, it translates `r.c` into the
index-scan domain via `ValueIndexScanMatchCandidate.buildTranslateValueFunction`, which produced a bare
`NewFieldValue` — DROPPING the reference's baked ordinal. The lazy node then dies loud when the equality loses
the index bound to a competing `r.c IS NULL` and is evaluated as a residual. Fix: the translate function
PRESERVES the incoming baked ordinal (`NewCorrelatedFieldValueWithResolvedOrdinal`) — the fetch's inner presents
a logical-slot-shaped partial record so the full-record ordinal reads the same slot; correct-or-loud otherwise
(index-layout rows guarded). Regression: `TestFDB_ThreeWaySharedColOrdinal_Regression` (0 rows); knownGaps ledger
now EMPTY (both tracked defects fixed). Gate: query-engine change → Graefe+Torvalds.

**FOLLOW-UP — parallel defect in UNWIRED dead code (INVESTIGATED — not a reachable bug):**
`WindowedIndexScanMatchCandidate.buildTranslateValueFunction` (windowed_index_match_candidate.go ~247) has the
IDENTICAL ordinal-drop (`NewFieldValue`, no ordinal). **But the candidate type is DEAD CODE:**
`NewWindowedIndexScanMatchCandidate` has ZERO callers in the whole tree, and `WindowedIndexScanMatchCandidate`
is not referenced outside its own file — the planner never instantiates it, so its translate function is never
invoked in any plan. The ordinal-drop is therefore latent-in-dead-code, not a reachable soundness bug. (Even if
it WERE wired: the executor's wrong-slot guard is generic — `executeIndexScan` never serves an index-layout row
to a baked ordinal — so the identical single-accessor fix would be correct-or-loud.) **To close WHEN the
windowed candidate is wired into the planner:** apply the identical `Resolved.Single()` gate + a fused-collision
unit test alongside the first real windowed-index query. Not worth a gated change to dead code now. (Original symptom, for reference:)
A 3-WAY join in which the SAME column is referenced across all three legs — a filter on the first
(`l.c = 1`), the m↔r join key (`m.c = r.c`), and a filter on the third (`r.c IS NULL`) — planned but **died at
execution**: `ordinal resolution: field "C" not resolvable in the runtime row (ordinal -1, row columns
[ID A B C S F]) — malformed plan`. Removing ANY one factor (drop a leg to 2-way / drop the m.c=r.c join /
drop either c-filter) fixes it. Fails LOUD; the query's correct result is EMPTY (m.c=r.c can never hold when
r.c IS NULL). Surfaced by the RFC-182 rowdiff harness (seed 1001000229) once 3-way self-joins were generated
at scale; classified as a tracked `knownGaps` DECLINE (`pkg/relational/conformance/rowdiff/run.go`). Same
ordinal-binding FAMILY as the multi-leg gap above, distinct signature — likely the same underlying
executor/ordinal-binding workstream. **Live pin:** `TestFDB_ThreeWaySharedColOrdinal_KnownGap` (goes RED when
the query executes — then delete the `knownGaps` entry and assert zero rows). **To make it work:** fix how a
3-way join binds a column's ordinal when that column appears as both a join key and per-leg filters across
the legs — the ordinal resolves to -1 in the runtime row today.

**ROOT CAUSE — DEFINITIVELY TRACED (RFC-182 shift, live instrumentation of the pin query).** Supersedes the
earlier merged-layout/FlatMap-frontier hypotheses. Full evidence chain:

1. **Plan (EXPLAIN):** left-deep FlatMap —
   `FlatMap(outer=FlatMap(outer=Fetch(IndexScan IDX_C [=])[l.c=1], inner=Scan(T)[l.a=m.id]), inner=Fetch(PredicatesFilter(IndexScan IDX_C [=]), [1 pred]))`.
2. **The failing predicate is NOT `r.c IS NULL`.** Instrumenting the residual PredicatesFilter shows the dying
   predicate is the JOIN EQUALITY `m.c = r.c` (ComparisonEquals) reapplied as a residual above the IDX_C scan:
   `LHS = FieldValue{field=C, corr=$m(merged outer), baked=TRUE}` ✓, `RHS = FieldValue{field=C, corr=q$NNNN(candidate base quantifier), baked=FALSE}` ✗.
   So the unbaked node is the INNER/indexed side (`r.c`) of the reapplied join equality — over a QUANTIFIER
   alias, `Resolved==nil` → `resolveOrdinal()` false → ordinal -1.
3. **Birth site (instrumented `NewFieldValue`):** the lazy `r.c` is minted by
   `ValueIndexScanMatchCandidate.ColumnValue` (`match_candidate_index.go:127`, via `index_expansion.go:66`) —
   the index's per-column PLACEHOLDER value `NewFieldValue(base, "C")`, intentionally UNBAKED because a
   match-candidate value is compared by name and NEVER evaluated (see `NewFlatFieldValue` doc).
4. **Why it reaches the executor:** `expr.go` BORN-BAKES every SQL column ref (`NewCorrelatedFieldValueWithResolvedOrdinal`,
   expr.go:278/296) and `WithChildren` PRESERVES `Resolved` (replace.go:429), so the query's own `r.c` is always
   baked — the ONLY way an unbaked node reaches runtime is a fresh lazy `NewFieldValue`. Here the candidate
   PLACEHOLDER leaks into the executable residual: `r.c` is referenced by BOTH `m.c = r.c` (sargable equality)
   AND `r.c IS NULL` (sargable-for-match, `match_max_match_map.go:72`) on the SINGLE indexed column c; they
   cannot co-sarg one index column, so the equality is reapplied as a residual — and that reapplication takes
   the indexed side from the candidate placeholder (lazy) instead of the query's baked `r.c`. The 2-way analog
   / single-c-reference cases never trigger the two-comparisons-on-one-indexed-column contention, so they stay
   baked (matches the pin's "remove ANY factor → works").
5. **Generalized (NOT IS-NULL-specific):** the differential sweep independently hit the SAME defect on a
   DIFFERENT shape — seed 3002111, indexes `[IDX_A IDX_AB IDX_B]`, query
   `… JOIN m ON l.b=m.b JOIN r ON m.b=r.b WHERE m.b=8 AND l.b=7` — where indexed column `b` is bound by FOUR
   sargable comparisons across the legs (two join keys + two equality filters, no IS NULL). Confirms the trigger
   is generally "≥2 sargable-for-match comparisons on ONE indexed column shared across join legs", of which the
   IDX_C + `IS NULL` pin is one instance.

**FIX DIRECTIONS (both are FROZEN RFC-173 index-match compensation machinery — Graefe+Torvalds gate + full
determinism sweep across every index-match shape REQUIRED; do NOT ship unGated):**
  (a) Bake `ColumnValue` against the record-type ordinal so a leaked placeholder still evaluates — BUT this
      changes the candidate Value consumed by ALL of predicate-matching / sort-matching / covering-index /
      max-match-map, and `semantic_hash.go` makes baked≠lazy by contract → wide blast radius, risks matching
      (this is the class of change the reverted `rebaseOuterLegValueOrdinal` fix belongs to).
  (b) At the residual reapplication, carry the QUERY's baked reference (Java's
      `PredicateCompensationFunction.ofPredicate(queryPredicate)`) instead of the candidate placeholder — OR
      suppress the REDUNDANT residual entirely (the equality already IS the `[=]` sarg; reapplying it as a
      residual is redundant and only fires under the shared-column contention). (b) is narrower than (a) but
      still core match/compensation code.

### [investigated, not shipped] rowdiff — 4-way joins: measured-infeasible for the always-on differential
Implemented and isolation-validated a 4-way self-join generator + a HASH-JOIN oracle (indexes on the probed
columns → O(n+matches), not the O(n⁴) a nested loop would cost), then MEASURED it against real FDB and
**reverted the generation** — two evidence-based reasons, not speculation:
1. **Prohibitive planning cost.** Each 4-way query costs ~10s in the Cascades build-order search (measured:
   60 4-way-inclusive seeds ran at 0.3 seeds/s vs ~1.5 for 3-way). A NON-selective join key (e.g. `m.a = p.a`
   over the 0..9 domain) is pathological — one such query pinned a core at 145% for **11+ minutes**. Forcing
   every probed side to the unique pk bounds the blowup (the previously-stalling range then completes clean),
   but even bounded, ~10s/query makes a 50k nightly take tens of hours at any meaningful rate. Note the
   pathological-planning case may itself be a planner-performance defect (unbounded 4-way build-order search),
   distinct from the ordinal-binding soundness gaps.
2. **Yield is the already-tracked ordinal family.** A generated 4-way join hit the SAME
   "not resolvable in the runtime row / ordinal -1" defect (seed 1006000182, a `JOIN … ON m.id = p.id`
   shared-key chain) and was correctly DECLINED by the existing knownGaps signature-matcher — i.e. 4-way
   re-exercises the 3-way/multi-leg ordinal-binding gaps, surfacing no NEW soundness class.
**To include it later:** either the Cascades 4-way planning cost drops, or run it as a dedicated low-rate
sweep separate from the main nightly. The hash-join oracle pattern (buildKeyIndex / joinKey) is the reusable
piece if revived.

---

# NEXT

> ## [ ] RFC-181 — query-engine correctness wave 3 (rfcs/181-query-engine-correctness-wave3.md)
> Owner priority: the NAME-MODEL findings are the priority PROGRAM; Graefe amendment:
> the hours-scale silent-corruption P0s land BEFORE Phase A opens (P0.1, P0.2, P0.3,
> P0.4, P0.6, C1-stopgap first). Execution order (details in the RFC):
> 1. [ ] WS-N interim pins (red today): quoted case-colliding ORDER BY; column literally
>        named "A.ID" on a join; duplicate-named ORDER BY with baked key (poison bypass);
>        quoted "Q$1" table alias; cross-leg same-name-different-type metadata.
> 2. [ ] WS-N Phase A — resolver provenance end-to-end (typed leg+accessors on every
>        FieldValue; structural rebases; kills N-F2/N-F3/N-F5).
> 3. [~] WS-N Phase B — SUBSTANTIALLY COMPLETE (B1-B3b landed); the residual is a
>        purity refactor, not a bug class. Landed: B1 binding-keyed scalar
>        lowering (dup-outer scalars answer; + the outer-projected plain-column
>        wrong-rows master fix); B2 the dup-alias flat-name projection carve-out
>        RETIRED (dup qualifiers bake QOV(binding) per-attribute, first leg
>        included; UNION-face decline upgraded to plan-time); B3a quote-aware
>        aliases resolve in EVERY position (FromNormalized for parse-captured
>        strings; Scope.AddSource canonicalizes CorrelationName to the UPPER
>        runtime correlation-key namespace — the quoted-"Q$1" interim pin is
>        GREEN, TestFDB_QuotedMachineShapedAliases); B3b ONE correlation-key
>        namespace end-to-end (leg names verbatim, correlation-vs-leg compares
>        EXACT; lowercase q$N machine mints case-DISJOINT from user
>        correlations — unforgeable). N-F6's harm cases are closed. RESIDUAL
>        (purity, own RFC if wanted): the full machine-mint flip for every leg
>        (statement-scoped deterministic mints; retires QualifierIsDuplicated's
>        concept and the remaining display-alias plumbing); the
>        dotted-split arm of fieldValueAliasAndCol retires with the
>        enclosed-unnest residual (box-substrate ordinalization), not B.
> 4. [~] WS-N Phase C — ordering on Value identity: the label-collision family's
>        live wrong-order bug is FIXED (translateSort's BareRef split,
>        sort_key_label_collision_fdb_test.go; the RFC-180 follow-up entry).
>        REMAINING (the phase core): requested parts carry RESOLVED values,
>        poset/binding maps keyed on semantic Value identity in the
>        current-quantifier post-rebase frame (the Graefe amendment), then
>        DELETE orderingKeyFor's three rendering bridges + normLookup
>        (rich_ordering.go — construction at :60-130, bridges at :317-335).
>        The bridges exist BECAUSE requesters mint lazy values while providers
>        are baked — C1 is requester-side born-baked ordering values, C2 the
>        identity keying, C3 the EnumerateSatisfyingComparisonKeyValues /
>        set-op merge-key consumers.
> 5. [ ] WS-N Phase D — metadata from the flowed type (positional ColumnDef; delete
>        descriptorForColumn/innerByName/qualifyAndMergeColumns/colref.go; kills N-F4).
>        Full agent handoff: shifts/handoff-ws-n-phase-d-typed-metadata.md.
>        D1 OPENED: plan_visitor mints 0 (carve-out retirement removed the last);
>        logical_predicate down to 11 (mostly justified error-arm unknowns +
>        3 lazy-strip arms); pullUpToOutputField slots now carry the projected
>        value's own type. Translator census (48 sites, 4 clusters):
>        (1) leg/record-type synthesis from NAME lists (:418,:432,:442,:475,
>        :524,:742,:749,:7978) — types derivable from catalog descriptors /
>        leg output types; the HIGH-VALUE cluster (typed legs ⇒ positional
>        types everywhere downstream);
>        (2) ofOrdinal rebinds (:4550,:4565,:4714,:5719,:5725,:5757,:7541,
>        :8207) — derivable from the input layout's field type once (1) lands;
>        (3) lazy flat name reads (:4221,:4557,:4568,:4717,:5739,:6019,:6239,
>        :6262,:6366,:6433,:8188,:8197) — documented correct-or-loud
>        fallbacks, type arrives at resolution; drain with their producers;
>        (4) honest unknowns (:293-316 7.6 model gap, :1304-1352 probe
>        multi-returns, :6431 NULL literal, :8267,:8286) — justify or leave.
> Interleaved at phase boundaries (independent wrong-rows P0s, each small+red-pinned):
> - [x] P0.1 forward-PK intersection ordering gate (row DROPS, plain SQL).
>       Follow-up CLOSED: the AND-over-two-indexes SQL shape fires the path e2e
>       (and_index_intersection.yaml) — and exposed unbaked comparison keys
>       (OrdinalResolutionError on every such query); fixed by flowing the
>       descriptor row type into candidates + baking pk keys, plan-time decline.
>       Ledger correction: reverse threading never landed; it remains the atomic
>       RFC-190.5b plan/executor/ordering/rewrite slice.
> - [x] P0.2 streaming-agg ordered child pinned (FinalOf) instead of shared-group memoize
> - [x] P0.3 union-leg pinning (delegator-hint lie + arity bake → dup rows through DISTINCT)
> - [x] P0.4 rebuildOrderedSpine executable-plan verification (extraction twin of RFC-180's)
> - [x] P0.5 INT/FLOAT 32-bit arithmetic lanes (Graefe+Torvalds ACK; Type() INT case,
>       literal narrowing per parseDecimal — closed 5 RFC-082 known-red entries)
> - [x] P0.6 bestPhysicalChild hash tie-break + properties-side comparator injection
> - [x] P0.7 PushSetOperationThroughFetch rebuild over pushed inners (Graefe+Torvalds ACK;
>       single-pass tryPushValues, dynamic InUnion arm live, Case-2 partial push,
>       ordered-union instantiation, e2e Fetch(Union(IndexScan…)) pin)
 - [ ] C5 follow-up: DISTINCT cross-page dedup — **CORRECTNESS FIXED end-to-end (2026-07-20); only a
       cost/perf optimization remains.** Both paths now resume-clean: ORDERED input streams (carries
       the last key), UNORDERED input (incl. DISTINCT + ORDER-BY-non-output-column) uses the hash
       cursor that carries its whole seen-set through gen.DistinctHashContinuation (Approach B,
       Graefe-ACK'd; distinctHashCursor in distinct_stream.go). Pinned by
       TestFDB_SelectDistinct_CrossPageDedup_Unordered (g=id%10 scattered, scanLimit=2). REMAINING
       (perf, NOT correctness): enumerate a cost-based sort-distinct alternative so a high-NDV DISTINCT
       (where the seen-set would blow the memory budget → today a LOUD budget error, never wrong rows)
       routes to a sort instead — Graefe's phase-2. Until then high-cardinality unordered DISTINCT over
       a huge table errors on the budget rather than completing; that is a performance limit, not a
       wrong-rows bug. FIXED: over input already ordered by the dedup key (`SELECT DISTINCT col
       ORDER BY col`, or a covering ordered index), a resume-clean STREAMING distinct now dedups
       adjacent rows and carries ONLY the last emitted key through the DedupContinuation —
       `RecordQueryDistinctPlan.Streaming`, set by ImplementDistinctFinalRule when
       `orderingSatisfiesGroupingKeys(EstimateOrdering(inner), projected-dedup-cols)` holds; executor
       distinctStreamCursor (distinct_stream.go). Pinned by TestFDB_SelectDistinct_CrossPageDedup
       (N=50, scanLimit=2, incl. LIMIT/OFFSET). REMAINING (needs a DESIGN pass, not a mechanical
       extension — do NOT just insert an InMemorySort): the UNORDERED case (`SELECT DISTINCT col`
       with no ORDER BY and no covering index → primary scan) still uses the fresh-per-page hash-set
       and re-admits across pages. Two candidate fixes, each with a real drawback — the choice is a
       genuine trade-off that should get a Graefe design ACK before implementation:
         (A) REQUEST the dedup-key ordering → planner inserts an InMemorySort, then stream. CORRECT
             and bounded-continuation, BUT the InMemorySort BUFFERS EVERY input row (memory-bounded +
             spills) to emit few distinct rows — a severe memory regression for the COMMON
             low-cardinality / high-row shape (`SELECT DISTINCT status FROM huge`: 2 distinct values,
             10M rows → buffer 10M to emit 2, where today's hash-set holds 2 keys). The streaming-agg
             analogy does NOT transfer: GROUP BY must see every row to aggregate anyway, so its
             InMemorySort adds no extra buffering; DISTINCT's hash-set specifically AVOIDS materializing
             the input, so an always-sort forfeits that. Also must not disturb the DISTINCT +
             ORDER-BY-on-a-NON-projected-column semantic (`SELECT DISTINCT v ORDER BY id`: dedup by v,
             output ordered by id via a first-occurrence sort — sorting by v destroys the id order), so
             gate on `GetRequestedOrderings()` being empty / a subset of the dedup cols.
         (B) Keep the hash-set but carry the seen-set THROUGH the continuation (bounded by the
             statement memory budget the set is already charged against; boundedSet.m is enumerable).
             Fixes BOTH the unordered case-2 AND the ORDER-BY-non-output-column case-3 (it does not
             rely on ordering at all — just makes the existing hash-set resume-clean). Memory-parity
             with today and a SMALL continuation for the common LOW-cardinality shape (executor-only,
             no planner/plan-shape churn). Cost: the seen-set grows monotonically, so every page
             re-sends the full set-so-far → O(pages × distinct-keys) CUMULATIVE work/transfer, heavy
             for high cardinality (where today is merely wrong). Needs a Go-internal continuation proto
             (proto/relational/, since DISTINCT is Go-only — no Java wire to match).
       No clean mechanical winner: (A) trades a correctness bug for a MEMORY regression on
       low-cardinality/high-row; (B) trades it for CUMULATIVE-TRANSFER on high cardinality. This is a
       genuine DESIGN decision (likely (B), or a cardinality-aware A/B choice) that needs a Graefe
       design ACK BEFORE implementation, per the query-engine RFC-first gate — do NOT unilaterally
       ship either. The ordering-DETECTION step already shipped (case-1) is deliberately separated from
       this and is safe on its own (only ever streams when the ordering already proves adjacency, else
       unchanged hash-set).
       ORIGINAL DIAGNOSIS (kept for the unordered follow-up): `executeDistinct` (executor.go ~1552)
       rebuilt the `seen` hash-set FRESH per page, so any `SELECT DISTINCT <non-unique-col>` over a
       table larger than one page returned WRONG ROWS. Adversarial
       repro (N=500, EXECUTION_SCANNED_ROWS_LIMIT=9 forcing ~56 page breaks): `SELECT DISTINCT w
       ORDER BY w` → 100 rows (every value twice) vs correct 50; `SELECT DISTINCT g` (no ORDER BY,
       unordered input) → 500 rows (TOTAL dedup failure) vs 25; `... LIMIT 30 OFFSET 10` → wrong
       window. `SELECT DISTINCT <unique-col>` PASSES (no duplicates to drop). This is a COMMON
       query pattern → the wrong-rows blast radius is large. Single-page baseline is always correct
       → purely a resume bug.
       The code comment's Java-parity claim is only HALF true: Java's
       `RecordQueryUnorderedPrimaryKeyDistinctPlan` also uses a fresh HashSet per page but dedups by
       PRIMARY KEY (unique per record), so the hazard doesn't bite it; Go routes `SELECT DISTINCT
       col` (dedup by projected VALUE) through the same fresh-set shape, where it DOES bite.
       DEFINITIVELY RESOLVED (2026-07-20, read the Java source): Java's fdb-relational SQL layer
       does NOT dedup SELECT DISTINCT AT ALL. `QueryVisitor.visitSimpleTable` never reads the
       DISTINCT token; there is NO `.DISTINCT()` consumer anywhere in relational-core; all 3
       Java yamsql SELECT-DISTINCT tests use already-distinct data (prove parsing, not dedup).
       So there is no Java value-distinct path to match — Go's is a pure read-side EXTENSION and
       the continuation is Go-internal. IMPLEMENTATION NOW TRACTABLE (all pieces confirmed to
       exist): (a) InMemorySort physical operator exists (RecordQueryInMemorySortPlan +
       MemorySortContinuation, resume-clean); (b) streaming dedup cursor + DedupContinuation
       (innerContinuation+lastValue) exist in pkg/recordlayer/dedup_cursor.go (no prod callers —
       ready to wire); (c) the streaming-agg rule's InMemorySort+pinOrderedSpine pattern is the
       exact template; (d) per-column sort keys built via
       values.NewFieldValueWithResolvedOrdinal(col, i, typ) over the inner's RecordType (see
       rule_aggregate_data_access.go:418). CAVEAT: inserting InMemorySort under every non-elided
       distinct churns EXPLAIN for all SELECT-DISTINCT plan-shape tests — expect broad manifest
       churn; the elision path (PK/unique-key coverage) MUST be preserved.
       FIX DESIGN (ordering-aware distinct arc — Graefe-gated, wire-compat-sensitive, ~multi-piece):
         (1) EXECUTOR: a STREAMING distinct — when the input is ordered by the distinct key, emit a
             row only when its key differs from the previous emitted key; carry ONLY that last key
             through the continuation (bounded, resume-clean). This fixes the ordered case
             (`DISTINCT w ORDER BY w`), which today still fails because the hash-set is used even
             over ordered input.
         (2) PLANNER: for value-distinct, REQUEST the input ordered by the distinct-key columns even
             without an outer ORDER BY (extend the ordering constraint pushed by
             PushRequestedOrderingThroughDistinctRule), so the unordered case (`DISTINCT g`) gets an
             ordered plan and the streaming distinct applies. Fall back to a Sort when no index
             provides the order.
         (3) CONTINUATION: the RecordQueryDistinctPlan continuation must carry the last-key — a wire
             format change; encode it in the proto continuation, version-guard the decode.
       The current fresh-set-per-page is a Go-only correctness hole (Java doesn't auto-page inside one
       statement). Pin FIRST with a large-N (>1 page) multi-value-per-key rows test at a small scan
       limit, both with and without ORDER BY, before touching the executor. Documented at
       executeDistinct.
       DEEPER DIAGNOSIS (2026-07-20, DFS): (a) `SELECT DISTINCT` is a GO-ONLY extension — Java's
       fdb-relational REJECTS it for most shapes (see rule_implement_distinct_final.go header), and
       Java's RecordQueryUnorderedDistinctPlan.executePlan(:109) also builds a fresh HashSet per
       call, so there is NO Java wire-format to match: the DISTINCT continuation is GO-INTERNAL —
       NOT a Java-wire-compat concern. That removes the "hard line" blocker; the continuation can be
       extended. (b) The scan limit returns a CLIENT-facing continuation (ScanLimitReached →
       noNextOrFail(..., limitContinuation()) in the scan cursors), i.e. a NEW Execute per page, so
       the seen-set is genuinely lost across Executes — the state MUST ride the continuation (not
       merely persist in statement State). (c) TEMPLATE: RecordQueryStreamingAggregationPlan already
       resumes cleanly across pages at large N (agent-verified) by carrying its running group key in
       the continuation — the streaming DISTINCT is the same pattern (carry the last emitted key).
       Port executeStreamingAggregation's continuation encoding. So the arc stays 3 pieces but is
       now de-risked and Go-internal: streaming-distinct executor + carry last-key in the (Go-only)
       distinct continuation + planner requests distinct-key ordering
       (PushRequestedOrderingThroughDistinctRule already pushes an OUTER ORDER BY through; extend it
       to request the distinct-key ordering even without one, falling back to a Sort). Graefe-gated
       (executor + continuation), but no cross-engine wire risk.
> WS-C: DONE (Graefe ACK; Torvalds conditions folded — lazy continuations, depth
> parity, ctor charge release). C1 full RecursiveUnionCursor port (UNION ALL
> recursion streams + resumes mid-level), C2
> engine-private decision + loud OptContinuation boundary, C3 kept-armed pending
> inner, C4 LoadByKeys lazy key-list resume, C5 documented at executeDistinct,
> C6 nil-continuation ban, intersectionMultiCursor consume().
> C1 REMAINDER (no stopgaps — every rejectUnsupportedResume guard replaced by
> the real port; the helper is deleted):
> - RecursiveCursor 1:1 port (Java cursors/RecursiveCursor.java: per-depth
>   node stack, RecursiveContinuation LevelCursor protos, primary-key check
>   values with the discard-on-drift re-descent) — the DFS join now STREAMS
>   and resumes; ImplementRecursiveDfsJoinRule strips the level-union
>   TempTableInsert tops (Java's matcher shape), so the DFS legs are bare
>   bodies and the eager DISTINCT arm charges its collects directly.
>   E2E: TRAVERSAL ORDER pre_order/post_order paginate mid-traversal under a
>   scanned-rows budget and resume with EXACT order
>   (TestFDB_RecursiveDFS_Continuation_ResumeAcrossPages). Note the resume
>   floor: checkpoints store PRE-YIELD positions (Java buildContinuation
>   reads childContinuationBefore), so a page budget below ~depth scans
>   cannot progress — inherent to Java's design, documented in the test.
> - DISTINCT recursion arms (Go extension; Java rejects recursive UNION
>   distinct): deterministic position-replay resume (lossless-codec
>   {emitted count, boundary row}; drift under the token is LOUD), level
>   union and DFS alike (TestRecursiveDistinctReplay_ResumesMidStream + the
>   cyclic-graph FDB pin).
> - explode / temp-table scan / table function / VALUES resume via Java's
>   ListCursor 4-byte position (FromListWithContinuation)
>   (TestListShapedPlans_ResumeMidList); corrupt tokens on every recursive
>   arm are typed ContinuationParseError.
> Phase B slice B1 LANDED: the correlated-scalar lowering is binding-aware —
> clusterPullUp spans/seed/bake key by leg BINDING (legByBinding; display
> aliases never key a span), the classifier's rightmost/outer-universe reads
> sourceBinding + Binding entries, and the front-end dup-outer decline is
> DELETED (P4c/P4d pins flipped from loud decline to exact rows, incl. the
> minted-leg outer projection — the old silent-NULL class). Found+fixed in
> the same slice (pre-existing on master, plain SQL, no dups): an
> OUTER-scoped PLAIN column projected by a correlated scalar
> (`SELECT (SELECT a.id FROM q AS z WHERE z.qid=5) FROM p AS a`) served the
> INNER row's first column — the plain spelling bypassed the walked arm's
> scope classification and derived the seed key from text; unified via
> classifyProjFieldValue over the resolver channel (parenthesized and plain
> spellings classify identically). Pinned: outer_projected_scalar_fdb_test.
> - [ ] Follow-up (found in B1, loud today): a scalar subquery whose ONLY
>       outer reference sits in the PROJECTION (`SELECT (SELECT a.id + z.qid
>       FROM q AS z WHERE z.qid = 5) FROM p AS a`) is classified
>       UNCORRELATED (correlation detection looks at WHERE position only) →
>       routed to the pre-eval planner → loud 0AF00 "could not plan scalar
>       subquery". Java plans it as correlated. Fix: classify by the walked
>       projection's free correlations, route to buildCorrelatedScalar.
> WS-N interim: quoted-lowercase column 42703 fixed (rlcatalog exact-key lookup);
> case-colliding quoted columns reject 0A000 at CREATE (was a planning panic);
> green pins landed (quoted_identifier_pins, join_leg_name_pins). Still red →
> Phase A: derived "A.ID" through a join (parser→IR string channel re-split).
> WS-T: DONE (both reviewers ACK; conditions folded). Lanes, div wrap, ADD
> string family + Double.toString contract, cast strictness + plan-time
> cast-pair gate, plan-time promotion gate (+ the double-vs-integer SARG
> narrowing and out-of-range clamps it exposed), IN/LIKE LHS gates,
> document-and-pin batch, datetime edge nets. Cross-engine wins landed live:
> string_concat_via_plus annotation retired; type_mismatch_boolean_eq_int
> left the known-red lock; bigint_eq_double_above_2p53 corpus entry added.
> - [x] FIRST-PRIORITY follow-up (Torvalds condition, sanctioned TODO) — DONE:
>       SubqueryPlanner.BuildScalar returns the inner plan's output column
>       type (project value / Java aggregate result table; correlated arm
>       stays Unknown); ScalarSubqueryValue flows it into the cast-pair and
>       promotion gates. Pinned in scalar_subquery_typed_gates.yaml.
> - [x] Follow-up (Torvalds, sanctioned TODO) — DONE: the 0AF00 came from the
>       walker being unable to EXPRESS a BYTES cast target (unsupported-shape
>       decline → text-channel fallback → opaque "could not plan"). BYTES joined
>       primitiveTypeToValueType, so ResolveCast's pair gate now rejects
>       STRING→BYTES with its own 22F3H in both positions (WHERE via
>       mapPredicateWalkError verbatim; projection via the InvalidCast surface
>       arm), identity BYTES→BYTES evaluates instead of silently NULLing, and
>       the cast.yaml pins assert 22F3H.
> WS-N Phase A progress: slice 1 (structural validation channel) and slice 2
> (born-baked — the four lazy resolver fallbacks retired as
> UnresolvableOrdinalError after zero-hit probes across yamsql/embedded/full
> sqldriver) are LANDED; slices 1-2 need their joint Graefe/Torvalds lap.
> - [ ] Slice 3 IN PROGRESS: (a) DONE — the builder qualified-projection
>       else-arm twins narrowed to the DUP-ALIAS carve-out (only live family;
>       QOV(alias) is ambiguous across same-named legs, Java rejects dup
>       aliases; Phase B's unique quantifier aliases retire the carve-out) and
>       fail UnresolvableOrdinalError otherwise. (b) DONE — the correlated
>       scalar join-inner hasJoins special-cases are DELETED: every join-inner
>       reference (qualified/bare, group keys/aggregate args) resolves through
>       resolver.ResolveIdentifier, the single scope channel, and the flat
>       mints are gone (five-shape column-agg FDB pin; NOTE: verify probe
>       builds compile — a goimports strip once turned a live arm into a
>       false zero-hit read as dead). (b2) Graefe slice-3 follow-up DONE: the
>       ambiguous bare re-read of qualified group keys (GROUP BY po.id, pi.id
>       re-read as `id`) was SILENT-WRONG in the SELECT list (last-wins leg
>       bind) and 0AF00 in HAVING — both now 42702 via scope validation of
>       groupCol re-reads + loud semantic errors from upgradeHavingPredicate
>       (pinned in ambiguous_group_key_reread.yaml, all re-read positions).
>       (c) translator re-mints (translateProject :5994,
>       translateProjectOverExistsFilter :4223 + the sort/group-key mints at
>       :4559/:4719/:5730/:6218/:6345): zero lazy escapes past the bakes in
>       yamsql+embedded (survival probe); the mint→bake pair is translator-
>       internal representation until Phase C/D move the IR off name-carrying
>       LogicalProject — full deletion is re-scoped THERE, not slice 3. The
>       dup-alias face is the only designed lazy escape (FDB survival probe to
>       enumerate).
> - [x] Slice 4 DONE: the RFC-153 buried-preserved rebase is ordinal-first —
>       buriedLegOrdinalLayout derives (leg, col) → global ordinal from the
>       outer FlatMap's positional RC concat (or planBuriedLegConcat windows),
>       and rebaseOuterLegValue bakes the merged-row ordinal when the layout
>       answers, lazy qualified mint only when it cannot. EMPIRICAL FINDING:
>       the whole leg-match arm is dead-in-effect today (box substrate rebases
>       buried refs onto box correlations upstream — probed zero across all
>       suites incl. the RFC-153 matrix); it stays as the fail-closed safety
>       net and now bakes when it fires. White-box pins for the layout
>       derivation + all rebase arms; 1M stress green at thresholds (full
>       scans ~3.3s, lookups 10ms).
>       REFINED MAP (read before starting): ONE call site (:479), gated
>       innerNullOnEmpty && buriedPreservedAliases — the RFC-153 LEFT-OUTER
>       buried-preserved rebase ONLY (regular INNER multiway already rebases
>       onto $m in the merge collapse). The value rewrite QOV(leg).col →
>       FieldValue(QOV($m), "LEG.COL") is the lazy half; study the plan-side
>       twin rebasePlanBuriedRefs for the ordinal-composition symmetry, and
>       planBuriedLegConcat (:1006, Fields+RecordTypeLegs boundaries) for the
>       window layout. The FrontierPinned panic guard inside rebaseOuterLegValue
>       documents the baked/lazy contract to preserve. Verifier
>       planReferencesAnyBuriedAlias fail-closes — the ordinal rewrite must keep
>       that conservatism. Pins to re-run: RFC-153 outer-join FDB suite + 1M
>       stress before/after.
> - [ ] Slice 5 RE-GATED (probed, evidence in code comments): both arms are
>       dead-in-effect on every covered surface (zero hits across yamsql,
>       embedded, cascades, full FDB driver — ok-line-verified probe runs),
>       but their PRODUCERS still live: fieldValueAliasAndCol's dot-split
>       serves the dup-alias carve-out's flat "ALIAS.COL" mints (retires with
>       Phase B unique quantifier aliases) and MergeSeedLegsOfValue defends
>       the enclosed-unnest name-model residual's dotted merged reads
>       (retires with the box-substrate ordinalization). Killing before the
>       producers would delete live defenses for constructible shapes
>       (RFC-142 zero-rows / misclassification hazards). NOT gated on 3-4.
> - [ ] Slice 6 SWEPT + CLASSIFIED (remaining callers each mapped to their
>       retirement owner): converted-to-structural this slice —
>       resolveCorrelatedColumnValue (takes aggArgBare/Qualifier/Qualified
>       segments, no text re-split) and aggColRefFromExpr (returns
>       extractAwfFields' structural argBare). Remaining census:
>       (a) resolveColumnName else-arms (8 sites, both build paths) — the
>       slice-1 dual-channel fallback for entries with EMPTY segments;
>       retires when Phase D saturates segment population;
>       (b) resolved-value dotted-defense splits (bareCol :3269 pair texts,
>       qualifyBareFieldValue :7752, scalar classify :8557/:8573, checkColumn
>       :4190 RFC-088 follow-up) — defend legacy dotted names; retire with
>       their producers (dup-alias carve-out / name-model residuals);
>       (c) eval_map/eval_predicate/eval_proto/select_helpers (5 sites) —
>       RUNTIME datum-key splits, the name-keyed row model itself (Phase C/D);
>       (d) cascades_generator metadata cluster (18 sites) — Phase D.
>       Exit criterion unchanged: (a)-(c) drain as Phases B-D land.
> WS-P: DONE — all four amendment stages landed, double-ACK'd (Graefe ACK
> with the 15c-wording condition folded; Torvalds conditions folded: dead
> helpers + ContainsFinal deleted, stale pre-flip comments rewritten).
> Stage (a): ConstraintsMap 1:1 epoch port (ticks/watermarks) + finals
> routing + Set choke-point mirror + MaxObservedExplorationRounds export.
> Stage (b): the convergence handover — NeedsExploration is epoch-driven,
> dual insertion retired (physical yields are FINALS ONLY), OptimizeInputs
> guard reverted to Java's containsExactly, insert-driven exploration at
> every insert site (data-access sites push tasks on InsertFinal), Absorb
> folds constraints via the typed per-key combine + epoch ReArm. Four
> latent order-dependences the flip exposed, each fixed structurally:
> streaming-agg empty-keys arm enumerates ALL valid physical members;
> findPhysicalPlan/Expr prefer valid physicals (nil-inner Fetch shells are
> relink templates, never plan-embedded); intersector same-index guard
> compares CandidateName; raw/adjusted partial-match twins collapse in the
> intersection path only, preferring most matched ordering parts.
> Stage (c): REWRITING finals route through OptimizeInputs (Java
> ExploreGroup shape) — parent-chain-optimized groups cross the stage
> boundary pruned to their REWRITING winner. RESIDUAL (documented at the
> boundary arm + DIVERGENCES): UNIVERSAL prune-to-1 is gated on PLANNING
> re-derivation parity — the forced-prune attempt lost canonical
> alternatives Go's PLANNING cannot re-derive (RFC-153 buried-leg,
> cross-join-EXISTS NoNulls).
> Stage (d): 15b (compareFlatMapVsNLJ) RETIRED (regression no longer
> reproduces; deleted with tests); 15c reclassified — a Go statistics
> EXTENSION in the pre-hash tiebreak slot (Java's cost model is purely
> heuristic), retiring it regressed real selectivity decisions; round cap
> 10→100 as a LOUD divergence tripwire (epoch rounds structurally bounded
> by the finite constraint lattices; round_cap_trips fixture).
> Gate each stage: full sweep green, 1M stress at thresholds, determinism
> clean, plandiff EXPLAIN parity. Ten white-box tests re-baselined to pin
> FORMATION via direct rule firing (prune-to-winner hides losers from
> post-Plan member walks); trailing-partition vector fan-out now plans AND
> executes (exact-rows re-pin).
> WS-P residual follow-ups (evidence-gated, not deferrals): universal
> boundary prune-to-1 (needs PLANNING re-derivation parity, red shapes
> named above); dual-store collapse (planner-global constraint map →
> per-Reference ConstraintsMap once the stores can merge).
> Remaining otherwise: WS-N Phases B-D after the slices.

> ## [ ] RFC candidate — typed row representation: retire []any slots (GATED on WS-N Phase D)
> Owner direction (2026-07-18): "we know the type of a column from proto —
> get rid of any." Ground truth from the Java source: Java is MORE boxed
> mechanically (QueryResult.datum is Object; rows are DynamicMessages whose
> scalars live in a FieldDescriptor-keyed Object map), but structurally
> STRICTER — Value extends Typed, getResultType() is part of the interface
> contract, rows carry a descriptor synthesized from the plan-time type,
> and client metadata reads positionally off Type.Record. Go's []any slots
> are the lighter runtime shape; Go's DEFECT is type LOSS (UnknownType
> minting + name-keyed re-derivation — the N-F4 family). Sequencing:
> 1. WS-N Phase D ports Java's DISCIPLINE (type populated/preserved on
>    every value, positional ColumnDef from the flowed type, the
>    descriptor-guessing helpers deleted). Prerequisite; already scheduled.
> 2. RFC A — typed scalar slots: PositionalRow.Slots []any → []Datum
>    (kind tag + int64/float64/string/[]byte fields; no per-value heap
>    alloc, kind-switch instead of type assertions). Row-at-a-time
>    architecture unchanged. Mechanical but wide: evaluators, comparators,
>    aggregate states, continuation codec, temp tables. MUST open with a
>    pprof of the 1M stress + vector benches to quantify boxing cost and
>    pick migration order. Boundaries that STAY any (correct): the FDB
>    tuple layer (wire format is dynamically typed — wire-compat, hard
>    line) and proto dynamic messages.
> 3. RFC B — vectorized batches for the hot scan→filter→project pipeline
>    (per-column typed arrays + null bitmaps, per-batch dispatch); complex
>    operators (NLJ, recursion) stay row-at-a-time initially. This is also
>    what makes SIMD distance kernels worthwhile (owner's c2goasm
>    question): typed contiguous buffers first; then gonum asm / avo / Go
>    1.26 GOEXPERIMENT=simd for euclidean_distance IF the profile shows it
>    hot — never c2goasm (unmaintained, unreviewable output), and any
>    kernel needs a fuzz differential pinning bit-equivalence with the
>    pure-Go reference (distance ties feed plan/result determinism).
> Both are allowed read-side Go extensions (Java never vectorized; wire
> compat untouched). Each its own RFC with the standard review gauntlet.

> ## [x] RFC-180 follow-up — output-label collision mis-binds sort keys over the IMMEDIATE reshaping strip — FIXED
> (translateSort's flat-key arm now applies the RFC-180 round-14 BareRef
> split: rendered-item keys bind PROJECTION text only, bare identifiers bind
> alias-preferred names; pinned across four shapes in
> sort_key_label_collision_fdb_test.go. Original diagnosis below.)
> `SELECT player AS "SUM(SCORE)", SUM(score) AS s2 FROM scores GROUP BY player ORDER BY SUM(score) DESC`
> sorts by PLAYER (the aliased column), not the aggregate: the sort sits ABOVE the reshaping projection
> whose output row carries a column literally labeled `SUM(SCORE)` (the delimited alias), and both
> `upgradeSortKeyValues`' colToIdx text match and translateSort's aggProjFields name-fallback bind the
> aggregate key to that colliding label by NAME. Fails identically on pre-RFC-180 master (verified at
> 58ee9daa7) — a translator field-naming ambiguity, NOT a regression: `postAggregateProjectionFields`
> names fields alias-preferred, so a rendered-item key and a same-spelled alias are indistinguishable
> at bind time. Fix direction: positional identity for reshaping-projection outputs (carry the slot,
> match rendered-item text against PROJECTION text and alias text only for SortKey.BareRef keys —
> the same split RFC-180 round 14 pinned for the DEFERRED strip). The deferred-strip variant IS fixed
> and pinned (aggregate_order_by_java "SUM(S.SCORE)" collision pin).

> ## [ ] FLAKE — TestDifferential_GetKeyConflict diverged once under cold full-suite load (go over-conflicts vs cgo)
> One occurrence (2026-07-17, fresh worktree, cold bazel output base, full `just test` running every
> container suite concurrently): 3 subtests diverged — `cleared_range_excluded`: "go-A conflicted=true
> (resolved=\"d\") cgo-A conflicted=false (probe=\"b\" sel=FGT)", plus independent_write_excluded and
> outside_span_no_conflict — i.e. the GO side OVER-CONFLICTED exactly on the three trimming cases the
> getKey conflict-set fix (PR #235 family) exists for. Passes in isolation, passes 5× under the bench
> binary's own parallel load, did not recur on the next full-suite run (bench cached). Prefixes are
> per-attempt unique (gkconf_<pid>_<name>_<attempt>_), so not cross-test key collision; the divergence
> pattern suggests a LOAD-dependent path in the Go client's getKey conflict trimming (updateConflictMap)
> falling back to the untrimmed span, or a differential-harness sequencing assumption that bends under
> scheduler pressure. hunt-divergences track; C++ 7.3.77 is the spec. Repro condition: cold output base +
> full concurrent suite. Log preserved at bazel-out .../ce2b9a2731780c0b259bca1a4820ae7d/.../pkg/fdbgo/
> bench/bench_test/test.log (first failing run).

> ## [ ] INFRA — scheduled nightlies dispatch HOURS late ("nightly" fuzz runs at noon)
> The Nightly Fuzz (`cron: 17 3 * * *`, moved to `17 4` = 4:17 AM UTC) consistently *runs* mid-day,
> not at night: GitHub CREATES the scheduled run ~4-5 h after the cron (07:04 on 06-30, 07:50 on 07-01),
> then it queues behind the single self-hosted `hetzner-fdb` runner. Two compounding causes:
> (1) **GitHub scheduled-workflow dispatch is best-effort and heavily deprioritized under high account
> usage** — a documented GitHub behavior; the off-minute cron (`:17`, not `:00`) mitigates but does not
> eliminate it. (2) **`--max-runners 1`** (RFC-155, box is 4-core/7.6 GiB) serializes a merge-burst backlog,
> so even once dispatched the fuzz waits behind CI/differential runs. Moving the cron to 4 AM does NOT fix
> the delay — it just renames the nominal time. Real options (pick later): (a) accept GitHub best-effort
> scheduling for nightlies (they're regression nets, exact time doesn't matter); (b) reduce the backlog —
> a second runner box, or a larger box to lift `max-runners` to 2 (RFC-155 §3 locked it at 1 for
> dependency isolation — revisit); (c) dispatch nightlies via an external scheduler (`workflow_dispatch`
> from a reliable cron outside GitHub) so they fire on time; (d) a dedicated nightly runner so scheduled
> jobs don't contend with per-PR CI. Also: the last two Nightly Fuzz runs FAILED (06-29, 06-30) — a
> separate real signal to investigate (a fuzz target has been red for two nights; no-unrelated-flakes rule).
>
> **ROOT-CAUSED 2026-07-20 — the Nightly Fuzz is CHRONICALLY RED on master across MULTIPLE INDEPENDENT
> jobs/targets, not one bug. A full triage of the recent red runs (via `gh run view --log`):**
>
> **engine-fuzz job** (SQL Engine + Record Layer Fuzz) — five distinct causes, four now handled on branch
> `fix-intersection-filter-cycle` (all both-gated where they touch the planner), NONE yet merged:
> - **CLASS 1** — Go-only filter/set-op commutation rules' cyclic-memo `GetCorrelatedTo` stack overflow.
>   Hits `FuzzPlanner_Idempotence`, `FuzzPlanner_InitialMemberPreserved`, `FuzzPlanner_MemoConsistency`,
>   `FuzzPlanner_Determinism` (07-19). FIXED by **RFC-185** (removes the four rules).
> - **CLASS 2** — no-BestMember stage-transition (merged/unfinalized group crosses REWRITING→PLANNING with
>   no finals, never re-explores). Hits `FuzzPlanner_WithBatchA_NoPanic` (07-19, seed 9bd9b3661b501312) and
>   `FuzzPlanner_PlanFullPipeline`. FIXED by **fuzz bug 2** (`Reference.AdvanceStagePreservingMembers`).
> - **CLASS 4** — `FuzzMessageTypeFromDescriptor` (07-16) stack overflow on a self-referential
>   nullable-array wrapper descriptor (`message M { repeated M values = 1; }`). FIXED (commit be1d377b7 —
>   guard-before-unwrap + thread visited).
> - **CLASS 3** — `FuzzGetCorrelatedToOfValue` (07-18). NO production bug (exhaustive proof: the walker is
>   correct); that red was an older revision or a fuzz-infra hiccup. The target was a FAKE safety net
>   (subset-only oracle) — strengthened to an equality oracle (commit 7e0803887).
> - `FuzzPipeline_NoPanic` (07-15) — planner; clean on branch at 120s (CLASS 1/2).
> All planner targets pass on the branch at/above the nightly's 90s per-target budget.
>
> **STILL OPEN — NOT on this branch, separate subsystems (a merge does NOT green these):**
> - **Binding Tester Stress** (07-14, 07-11, 07-08): `0/50 pass, 50 fail`, every seed exiting 1 in ~5s with
>   FDB ALIVE. 50/50 identical fast failures ⇒ almost certainly a HARNESS/ENV/build problem on the runner,
>   not 50 logic bugs. Needs its own look (run `just binding-stress` locally to confirm infra vs real).
> - **Differential Serialization Fuzzer** (07-14, 07-11): `FAIL: FuzzGetValueReply` — a real Go-vs-C++ WIRE
>   serialization divergence in `cmd/fdb-diff-oracle` (fdbgo GetValue reply). Its crash seed was not
>   uploaded. Needs the C++ oracle to reproduce; own fdbgo cycle.
>
> **ACTION: (1) merge the branch → clears the CLASS 1/2/3/4 + FuzzPipeline engine-fuzz reds; (2) then
> triage binding-stress (likely infra) and the diff-oracle FuzzGetValueReply wire divergence separately.**
> The window-skip "success" runs (short 5s/4m runs when the runner woke outside 00:00-07:00 UTC) mask how
> consistently the substantive runs fail.

## [x] INFRA — Nightly Fuzz false reds: Go stdlib leaks `-fuzztime` expiry as a failure — FIXED
Root-caused and fixed (`cmd/fuzzrun`). The nightly `Differential Serialization Fuzzer` reds were **not**
a wire divergence. Signature, from run `29311279420` (2026-07-14):
```
fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
```
5.65M execs, **zero** mismatches, no crasher persisted (hence the empty seed-artifact upload). The
`2026-07-11` red (`29142824495`) was **`FuzzEndpoint`** — a different target, byte-identical signature,
which is what rules out a per-type serialization bug.

**Root cause** — `$GOROOT/src/internal/fuzz/fuzz.go` (Go 1.26.5). The coordinator suppresses the
budget-expiry error with `if err == fuzzCtx.Err()`, where `fuzzCtx = context.WithCancel(ctx)` and `ctx`
carries the `-fuzztime` deadline. `context`'s `cancelCtx.cancel` closes the parent's done channel
*before* walking its children, so the coordinator can wake on `<-ctx.Done()` and read `fuzzCtx.Err()`
while the child is still uncancelled (`nil`). The comparison fails, `DeadlineExceeded` becomes `fuzzErr`,
and a clean full-budget run is reported as a test failure. Upstream: golang/go#72104 (NeedsInvestigation).

**Evidence chain:** (1) reproduced on a trivial dependency-free fuzz target, **1/80 runs** — nothing to do
with the oracle; (2) `GODEBUG=fuzzdebug=1` caught it live: `stop called at .../fuzz.go:228. stopping: false`
— line 228 is exactly the `case <-doneC: stop(ctx.Err())` deadline arm, and it is the *first* `stop`, so it
is the call that set `fuzzErr`; (3) the underlying context window measured directly at **0.025%** of
wakeups (`parent.Err() != child.Err()`), widened under real 4-worker fuzz load.

**Fix:** `cmd/fuzzrun` wraps each fuzz invocation, classifies the failure, and retries **only** a run that
exhibits the complete budget-expiry shape, once.

Recognition is a **positive whitelist, not a denylist** — this is the load-bearing design point. A first
draft retried anything containing `context deadline exceeded` minus a denylist, which both reviewers
correctly rejected as unsafe: that string is *also* what a timed-out FDB testcontainer reports, and every
package hosting a fuzz target starts one under a `context.WithTimeout` (CLAUDE.md mandates it). That
version would have retried real Docker flakes into green — the exact failure mode the
"no unrelated flakes" rule exists to prevent. A run is now benign only when **all** hold:
(1) it exited with a test-failure status (rejecting `timeout`-kill 124, OOM 137, signal, bazel build
error); (2) it emitted `fuzz: elapsed:` progress, proving budget was actually consumed; (3) it reported
exactly ONE failure and that failure is a `Fuzz` target; (4) that failure's ONLY detail line is
`context deadline exceeded`. Plus a hard-failure marker list covering the paths that produce no crasher
file — notably `failure while testing seed corpus entry:`, which Go reports via
`stop(errors.New(crasherMsg))` rather than a `crashError`, so it prints no "Failing input written to".

Note the retry is **not** load-bearing for safety, and must not be described as such: the "a persisted
crasher replays as a seed on the retry" argument holds only for the plain `go test` path, because under
`bazelisk test` the crasher lands in an ephemeral sandbox that is destroyed on exit (124 of 142 target
pairs). Safety comes from the classifier rejecting anything that isn't the exact shape.

The single most dangerous shape — found by review round 2, and invisible to the structural check — is a
**crasher found while the deadline fires during minimization**. `internal/fuzz/fuzz.go:149-164` persists
the crasher but wraps it in a `crashError` only `if err == nil`; under this race `err` is
`DeadlineExceeded`, so there is no `Failing input written to`, no `crasherMsg`, and hence no nested
`--- FAIL` — byte-identical to a benign expiry while a genuine finding sits on disk (and under bazel, in a
sandbox about to be destroyed). `-fuzzminimizetime` defaults to 60s against a 90s budget, so the window is
wide, not exotic. The `fuzz: minimizing` witness line (`fuzz.go:265`, logged immediately after
`c.crashMinimizing` is set) is what catches it. Revert-proven: removing the marker turns the regression
red.

The confirming retry is skipped past a per-job `-deadline` so a systematic race cannot blow
`timeout-minutes`. The cutoff is computed **per iteration** and reserves one run for every target still
queued (`STEP_END - (TOTAL - RAN + 1) * PER_TARGET`): a flat cutoff was not enough, because under a
systematic race the early targets double up and the remaining mandatory first attempts still overran —
engine-fuzz landed at ~79min against a 75min timeout, killing the job before `-summarize` could report
the very thing it exists to report. `STEP_END` is anchored to `JOB_START` (stamped into `$GITHUB_ENV` by each job's first
step) rather than to the fuzz step, because `timeout-minutes` runs from job start and the preamble — the
C++ oracle build especially — can eat an unpredictable slice of it.

Two properties are guarded because both fail *silently*: `STEP_END` must sit ABOVE the mandatory total
(`TOTAL × PER_TARGET`) or the reservation term goes permanently negative and disables retries with no
symptom; and `TOTAL` is **discovered**, so adding roughly a dozen engine fuzz targets — a couple of shifts
of ordinary work — walks the rotation into the timeout on its own. A pre-loop check **warns** on both — and warns
only. `PER_TARGET` is a seed estimate that self-corrects upward from the observed pace, and an estimate
must never drive a pass/fail decision: on a cold cache the arithmetic can say "does not fit" while the run
finishes comfortably, and a spuriously red nightly is the exact disease this item cures. The enforceable
bound is `timeout-minutes` itself; the warnings exist so the cause is already in the log when it fires.
The only things that set `FAILED` are a real target failure and the observed-race majority.
Skipping it **passes** rather than fails: the classifier is authoritative, so going red there would
re-introduce the very false red this tool removes — and would do it exactly when the race is most
frequent. Output is streamed rather than buffered, so a job killed mid-run still has the in-flight
target's log.

Individual raced runs pass, but `-racelog` tallies them and `-summarize` fails the job if a **majority**
of targets raced in one night: at ~1%/run that is not bad luck, it means the Go toolchain's fuzz
coordinator changed and this tool's assumptions need re-verifying before the gate can be trusted. Without
that, a systematic race would show up only as per-target warnings inside a green job — i.e. nowhere.

Wired into all three nightly jobs (`diff-fuzz`, `client-fuzz`, `engine-fuzz`) **and** `just verify`'s fuzz
smoke targets. Pinned by `cmd/fuzzrun/{classify,gate,runcommand,summary}_test.go` — the verbatim captured
output of both real nightly failures, real bazel framing (exit 3, verbose per `.bazelrc:23`), the
exit-code plumbing, the race tally, and the dangerous direction: eleven deadline-bearing outputs with no
crasher marker that must all still fail. Both minimization markers are independently revert-proven.

**Note for the query-engine nightly triage:** `engine-fuzz` shares this exposure, so any planner/metadata
nightly red whose only error line is `context deadline exceeded` with no crasher was the same false
positive, not a planner bug — re-check those before spending a shift on them.

> ## [ ] INFRA — stress-1M thresholds violated on MASTER (baseline rot; INVESTIGATE)
> Discovered by RFC-176 P2's stress gate (PR #453): on an idle box, **current master violates the
> "Stress test 1M baseline" Threshold column** — in_list 14.97ms (<10ms), needle_pk 5.4ms (<5ms),
> needle_filter 6.4ms (<5ms), pk lookups 5.0–7.2ms (<5ms) — vs May-baseline values of 10/2.0/2.4/1.5-1.7ms.
> The P2 branch was noise-identical to master on every violated row with all 23 EXPLAINs
> byte-identical (P2 exonerated; the gate fails on its own base). Repro:
> `bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed
> --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v" --nocache_test_results`
> — box `workstation`, Linux 7.0.10-arch1-1, idle, back-to-back master/branch runs; three-way table
> in PR #453 (comment "Quiet-box stress-1M: branch AND master control").
> **Decision path (in order):** (1) re-qualify the environment against the May baseline run —
> machine, Docker version, FDB image, kernel; if any changed, re-measure and UPDATE the baseline
> table + thresholds; (2) if the environment is unchanged, bisect May→HEAD on the violated rows
> (point-lookup latency, in_list). **Terminal state: the baseline table is re-qualified/updated OR
> the regressing commit is named.** A safety-net table nobody can pass measures nothing.

> ## RFC-176 P2 registrations (review of PR #453) — two follow-up cycles
>
> - **[ ] Replace the criterion-#17 cost tie-breaker hash with a canonical semantic total order**
>   (`planning_cost_model.go:350-358`, `costExprHash`/`concretePlanHash`/`deepHashCode`). Cost-tied
>   semantically-DISTINCT plans are decided by raw hash ordering, so ANY hash evolution re-rolls the
>   winner — PR #453 is the existence proof: RFC-176 P2's alias-invariant plan hashes flipped the
>   vector fold/decline winner and a pinned sentinel went red (fixed there by making the tie a COST
>   decision — vector scan CPU — but the tie-break CLASS remains for every other equal-cost pair).
>   Wanted: a canonical order over plan structure (operator kind, arity, discriminators, recursively),
>   not over hash values, so winners are stable under hash changes and alias renaming. Query-engine
>   cycle (Graefe).
> - **[ ] Predicate/selector semantic hash — retire rendering-keyed `HashCodeWithoutChildren`, the
>   RFC-176 §1 defect class one type-family over.** Five sites fold non-Value renderings into plan
>   identity hashes while their equality is structural: `predicates_filter.go` + `filter.go` +
>   `nested_loop_join.go` (per-predicate `pr.Explain()` vs `PredicateEquals`), `load_by_keys.go`
>   (`keysSource.String()` vs `keysSource.Equals`), `selector.go` (`planSelector.String()` vs
>   `planSelector.Equals`). equal⟹same-hash holds only while every rendering stays exactly injective
>   over the structural discriminators — the standing whack-a-mole RFC-176 killed for Values. Wanted:
>   `predicates.SemanticHashCode` (+ keysSource/planSelector hash methods) mirroring
>   `values.SemanticHashCode`, tightened in lockstep with `PredicateEquals`/`Equals`. Own cycle;
>   needs the same gates as RFC-176 P2 (property test, stress, corpus, task-count baseline).

> ## Cascades bug hunt (branch `hunt/cascades-bug-hunt`) — 9 confirmed bugs
>
> Multi-agent + differential hunt across the Cascades engine. 6 FIXED in this PR (red→green tests,
> full `sqldriver` suite green); 3 OPEN below (riskier — own focused Graefe cycle each).
>
> **FIXED (this PR):**
> - **[x] AGG-RESIDUAL (critical, wrong results).** `AggregateDataAccessRule` dropped any input filter
>   predicate that wasn't a grouping-key equality (non-group column, or inequality) — aggregate read over
>   ALL rows. `SELECT g,SUM(v) FROM ga WHERE f=1 GROUP BY g` returned the unfiltered sum. Fix: decline the
>   aggregate-index match when a residual isn't a grouping-key equality (Java compensation = impossible) →
>   StreamingAgg fallback. `rule_aggregate_data_access.go`. Pins: `TestFDB_AggIndexResidualDrop`,
>   `TestBugHunt_AggregateIndexResidualNotDropped`.
> - **[x] IN-LIMIT-NIL (critical, 0 rows / exec error).** `physicalLimitWrapper.WithChildren` gated relink on
>   `isLeafReplaceable`, so `… WHERE c IN (…) LIMIT k` over a Projection kept the eager nil-inner snapshot →
>   `Limit(Project(Fetch(<nil>)))` → 0 rows. Fix: always relink (the fetch wrapper's RFC-070 fix, applied to
>   limit). `physical_limit_wrapper.go`. Pins: `TestFDB_InListLimitReturnsRows`, `TestBugHunt_InListLimitNoNilInner`.
> - **[x] HAVING-PUSHDOWN (high, wrong results).** `predicateReferencesOnlyKeys` checked only the LHS, so
>   `HAVING g > SUM(v)` was pushed below the GroupBy onto the raw scan. Fix: also require the RHS comparand to
>   be key-only/constant. `rule_push_filter_through_groupby.go`. Pin: `TestBugHunt_HavingAggregateNotPushedBelowGroupBy`.
> - **[x] COUNT-COL-COVERING (high, COUNT returns 0).** scalar `COUNT(col)` force-marked the index scan COVERING
>   with zero columns → col read as NULL → COUNT=0. Fix: gate on a true COUNT(*) (mirror executor `isCountStar`).
>   `rule_implement_streaming_agg.go`. Pin: `TestBugHunt_CountColumnNotForcedCovering`.
> - **[x] DISTINCT-UNIONALL (high, duplicate rows).** `computeDistinctRecords` reported the no-dedup UNION ALL plan
>   (`RecordQueryUnionPlan`) as distinct → `SELECT DISTINCT` over UNION ALL elided the dedup. Fix: report it
>   non-distinct. `plan_properties.go`. Pin: `TestBugHunt_DistinctOverUnionAllKeepsDedup`.
> - **[x] CAST-ROUND (low, Java parity).** `CAST(double AS INT/BIGINT)` used pre-Java-7 `floor(x+0.5)` →
>   `0.49999999999999994` rounded to 1, `-0.5` to -1. Fix: faithful `java.lang.Math.round` bit-port.
>   `values/values.go`. Pin: `TestBugHunt_CastDoubleToIntJavaRounding`.
>
> **OPEN (riskier — separate Graefe cycles; repros in PR):**
> - **[x] COST-SELECTIVITY — FIXED (PR pending).** The equality bound was costed at `FilterSelectivity=0.5`
>   > `RangeSelectivity=0.33` — inverted, so a broad range index was costed cheaper than a selective equality
>   index. Introduced a distinct `EqualityBoundSelectivity=0.1` (< `RangeSelectivity`, kept separate from the
>   generic residual-`FilterSelectivity`) and wired it into the 3 equality-vs-range scan-cost sites
>   (`physical_wrapper.go` ×2, `planning_cost_model.go` scanLikeCost). `WHERE a=5 AND b>10` now drives off
>   `IDX_A([=])` not `IDX_B([<>])` (master proven to pick the wrong index). Pins: `TestCostSelectivity_EqualityBeatsRange`
>   (constant invariant sentinel) + `TestCostSelectivity_PrefersEqualityIndex` (plan). 1M stress before/after +
>   FuzzCostMonotonicity 1.3M green; only 1 cost-assertion test churned.
> - **[x] NULLS-ORDER — FIXED (PR pending).** Restored the NULLS axis to `RequestedSortOrder`
>   (`AscendingNullsLast`/`DescendingNullsFirst`), populated it from `SortKey.NullsFirst`, and made
>   `IsCompatibleWithRequestedSortOrder` + data-access `SatisfiesRequestedOrdering` null-aware (+ direction
>   sites use `IsAscending()`/`IsDescending()`). `ORDER BY b ASC NULLS LAST` now retains the InMemorySort.
>   Pins: `TestNullsOrder_ExplicitPlacementRetainsSort` (plan: single + multi-key) + `TestFDB_OrderByNullsLast`
>   (rows, both non-natural directions + multi-key). Full embedded + sqldriver green; an ad-hoc adversarial
>   review sweep (not committed regressions) found nothing.
> - **[x] VECTOR-PARTITION-INTERSECTION-K>1 (HIGH, wrong rows — was ACTIVE on master, pre-existing) — FIXED.**
>   Pin: `TestFDB_VectorSearch_MultiPartition_InequalityResidualK2` (`WHERE zone='z1' AND region>'r1' … <=2` now
>   returns `{21,22}`, was `{22}`). The fix is TWO pieces (piece 3 collapsed into piece 2 — the realization path
>   already existed): (1) exclude ALL `VectorIndexScanMatchCandidate` from the pk-intersection candidate set — one
>   condition in `pushDataAccessTasks` (planner.go ~628), the single home for the rule — since a vector scan is
>   distance-ordered (ordered-stream OR self-limiting per-partition), never pk-monotonic, so it can never be a valid
>   pk-keyed sorted-merge leg (the earlier draft's plan-level `isSelfLimitingVectorScan` intersector guards were
>   consolidated into this upstream candidate-level check — Torvalds/Graefe nit, provably equivalent);
>   (2) `compensationSafeForYield` `residualIsPartitionContiguous` exception — a residual over the partition columns
>   CONTIGUOUS immediately after the bound equality prefix selects whole partitions, so it composes safely as a Filter
>   above the self-limiting scan; `ImplementFilterRule` (which has NO gate against a Filter over a self-limiting vector
>   scan, only against index-only predicates) then realizes `Filter(region>'r1') → VectorScan(self-limiting rank<=2)`.
>   The DistanceRank stays in the scan binding (never leaks to the residual — rule_match_intermediate.go:470-483 keeps
>   it consumed), so the Filter carries only the non-index-only `region>'r1'`. Contiguity anchor keeps the
>   leading-column-unbound case (`region='r1'`, zone unbound) unplannable (`TrailingEqualityResidual` still green).
>   Plan field `RecordQueryVectorIndexPlan.partitionColumns` (not wire-serialized) carries the names for the check.
>   HISTORICAL (kept for context — root cause). Found by Graefe during RFC-167 Phase 4 review (PR #411). For a
>   partitioned vector index with a partition-key inequality (e.g.
>   `WHERE zone='z1' AND region>'m' AND <distance> <= K`, `PARTITION BY (zone,region)`), `WithPrimaryKeyIntersector`
>   emits `Intersection(VectorTopK, PrimaryRange)` keyed on the full pk — and it is the ONLY physical plan (the safe
>   `Filter(region>m, VectorTopK)` is blocked by `compensationSafeForYield` planner.go:734), so it is SELECTED. But the
>   multi-partition vector cursor delivers `(region, distance)` order, NOT pk order (`vector_index_maintainer.go:779-790,1103`),
>   and `executeIntersection` (executor.go:1710) feeds it into the pk-keyed sorted-merge (merge_cursor.go:405-467) with
>   no re-sort → for K>1 rows per partition whose distance-order ≠ doc_id-order, the merge DROPS rows. Worked example:
>   partition (z1,n) rows aaa(d=5),bbb(d=1),ccc(d=3): vector emits bbb,ccc,aaa; pk-range emits aaa,bbb,ccc; merge drops
>   aaa. Latent because only K=1 is tested (`vector_multipartition_e2e_fdb_test.go:140`, `<=1`). FIX (same two pieces
>   as RFC-167 Phase 4): the Java common-ordering gate in `WithPrimaryKeyIntersector` (drops this invalid intersection)
>   + refine `compensationSafeForYield` so a PARTITION-key-column residual over a per-partition vector top-k is safe
>   (selects whole partitions, never within-partition rows) → yields the correct `Filter`. PIN with a K>1 partition-
>   residual FDB rows test FIRST. **CONFIRMED empirically** (probe, dropped during cleanup): `WHERE zone='z1' AND
>   region>'r1' … <=2` plans to `Intersection(VectorIndexScan(rank<=2), Scan(DOCS,[=,<>]))` and returns `{22}` —
>   DROPS id 21 (correct is `{21,22}`; in r2 the nearest vector is id 22 (dist 1.20) but id 21 (dist 1.41) sorts
>   first by pk → the pk-merge advances past 21). **FIX ATTEMPTED (reverted) — it is THREE pieces, not two:**
>   (1) the pk-order gate in `WithPrimaryKeyIntersector` (per-leg `computeWrapperRichOrdering(leg).Satisfies(pkReq)`)
>   correctly drops the invalid vector intersection — VERIFIED; (2) the `compensationSafeForYield` partition-residual
>   exception (with a CONTIGUITY check: the residual columns must be exactly the partition columns immediately after
>   the bound equality prefix `len(prefixComparisons)` — else a leading partition col like `zone` is left unbound,
>   which must stay unplannable per `TrailingEqualityResidual`) — VERIFIED via the plan field
>   `RecordQueryVectorIndexPlan.partitionColumns` set in `ToScanPlan` from `columnNames[:partitionCount]`. BUT pieces
>   1+2 alone make BOTH the K=1 and K>1 inequality queries UNPLANNABLE ("index-only predicate … cannot be a residual
>   filter") — because **(3) the standalone `Filter(region>r1, VectorTopK)` plan is never GENERATED**: a partitioned
>   vector scan (`partitionCount>0`) returns `EmitsOrderedStream()==false` (vector_index_match_candidate.go:139), so
>   it is NOT excluded from the intersection (planner.go:628 RFC-156 guard) AND its only residual-bearing shape is the
>   intersection — there is no realization path building a `Filter` over a self-limiting vector scan (the compensation
>   is blocked/non-realizable, so only the intersection ever carries the residual). **The real fix (Graefe-corrected —
>   the earlier `Limit(k)→Filter→ordered-scan` framing was WRONG):** do NOT switch to ordered-stream + a global
>   `Limit(k)` — a global Limit over a fanout stream returns the k nearest rows across ALL surviving partitions and
>   DROPS whole partitions (e.g. `region>'r1'` spanning r2,r3 with per-partition `<=2` must return 2 from r2 AND 2 from
>   r3 = 4 rows; global `Limit(2)` returns the 2 nearest overall, possibly both from r2, dropping r3). That is the
>   deferred Phase E per-partition-top-k trap (`vector_index_match_candidate.go:307-310`) and a NEW wrong-rows bug.
>   Instead, **keep the scan SELF-LIMITING** (per-partition top-k stays enforced inside the maintainer's per-partition
>   HNSW search) and **realize a partition-contiguous `Filter` directly above it**: `Filter(region>r1) →
>   VectorScan(self-limiting per-partition top-k, fanout prefix=[z1,*])`. The self-limiting fanout returns top-k for
>   every region in z1 in region order; the partition-column Filter drops whole regions ≤ r1; survivors are exactly
>   top-k per region for region>r1. The index-only-residual error resolves naturally: the self-limiting scan still
>   consumes the DistanceRank in its binding (`rank<=k`), so only `region>r1` (non-index-only) remains for the Filter.
>   So Piece 3 is NOT "extend ordered-stream to partitioned" — it is "realize a partition-contiguous Filter over the
>   self-limiting per-partition scan" (Piece 2 already certifies its safety). THEN gate the intersection (pieces 1+2).
>   The K>1 pin must assert the winning plan is `Filter(...) → VectorIndexScan(... rank<=k ...)` (self-limiting), NOT
>   an ordered scan. RFC-046/156 vector-planning change — needs the K>1 FDB rows pin + the existing vector suite green + 1M stress.
> - **[~] PLAN-NONDETERMINISM (medium, flaky plans / cache churn) — RFC-167; Phase 0 + 1a done, rest designed.**
>   Phase 1a (inner-aware shell hash, `exprConcreteHash` in `costExprHash`) FIXES the headline multi-equality tie
>   (`a=5 AND b=7 AND c=9`), deterministic in-process AND cross-process, as pure tie-resolution (no plan change).
>   Decoupled finding: the guard-generalization + VALUE-RANGE ordering-gate (which re-rank value-range shells to the
>   Intersection) are a separate landing needing the full ordering machinery + 1M stress (see RFC-167 §4 IMPLEMENTATION
>   FINDING). **OQ#6 is RESOLVED and the vector half is DONE** (VECTOR-PARTITION-INTERSECTION-K>1 above, fixed): a
>   vector scan is distance-ordered and can NEVER be a valid pk-keyed intersection leg, so it is excluded from the
>   intersection candidate set entirely (one condition in `pushDataAccessTasks`) and its residual composes as a Filter
>   above the un-intersected self-limiting scan. That removed the false conflation an earlier crude per-leg gate hit
>   (dropping the vector leg is a TRUE positive, not a symptom of a bad gate). REMAINING (value-range only): whether a
>   VALUE-RANGE pk-merge is even reachable / wrong-rows or MaximumCoverageMatches prevents it (OQ#2), and the general
>   per-leg pk-order gate for value-range intersections following Java's `enumerateSatisfyingComparisonKeyValues`
>   (per-candidate-type participation) — the RFC-167 Phase 1b landing, needing the full ordering machinery + 1M stress.
>   Full design + verified root cause + phased plan in **`rfcs/167-cascades-plan-determinism.md`** (the deeper
>   layer is the RFC-070 nil-inner-shell architecture defeating Java's prune-to-one-concrete-member + planHash
>   tie-break; an orthogonal pk-intersector ordering bug — `intersector_primary_key.go` dropping requestedOrderings
>   — is a plausible wrong-rows risk that Phase 4 fixes). **Phase 0 (this change):** fixed the two map-iteration
>   sources: (1) `expressions/reference.go`
>   `partialMatchMap` is now iterated in first-insertion order (companion `partialMatchOrder` slice, mirrors
>   Java `LinkedHashMultimap`); (2) `cascades_generator.go` `metadataPlanContext.GetMatchCandidates` now sorts
>   `RecordMetaData.GetAllIndexes()` (a Go map) by index name. Pinned by `TestPlanDeterminism_EqualCostIndexTie`
>   (2 indexes on one column, 200 runs, one plan). **REMAINING (multi-phase, tracked):** a multi-equality tie
>   over several single-column indexes (`WHERE a=5 AND b=7 AND c=9` / idx_a,idx_b,idx_c) is still
>   nondeterministic. Root cause: nil-inner *shell* wrappers (Fetch + PredicatesFilter push-through templates)
>   are costed without their eventual inner → rank artificially cheap; and the extraction relink
>   `findPhysicalPlan` (physical_wrapper.go) resolves a shell's inner to the FIRST physical member of the child
>   reference by member-iteration order → on a cost tie the relinked index varies. Naive fixes tried and reverted:
>   excluding all shell types from `OptimizeGroupTask` selection deterministically picks the cost-cheapest REAL
>   plan, but that's an Intersection (e.g. `idx_a ∩ idx_b`) — a plan-shape regression vs the single-index plan
>   the shell relink produced, AND exposes intersection mis-costing. The complete fix needs consistent shell
>   handling + total-order tie resolution across selection AND `findPhysicalPlan` extraction, validated by 1M
>   stress (it changes index selection broadly). Rows are always correct → medium. `FuzzPlanner_Determinism`
>   misses both (doesn't exercise equal-index ties).
>
> **Latent (not reachable today):** `ValueIndexScanMatchCandidate.createsDuplicates` is a dead field (always
> false) so fan-out value-index access never emits the per-leg PK Distinct Java applies — but the embedded
> metadata builder never emits a `FanType.FanOut` value index, so no SQL query reaches it. Wire a guard if/when
> FanOut value indexes become expressible.
>
> ## RFC-164 — Port-fidelity drift: make this bug CLASS un-shippable (tracked workstream)
>
> Post-mortem of the hunt: every bug was a spot where Go reimplemented/simplified Java instead of porting 1:1,
> dropping an invariant — and CI stayed green because the test gap is *dimensional* (each feature tested alone,
> never in the combination that breaks it) and one test even pinned a bug. The deepest issue: port fidelity
> isn't enforced by anything automated; the one net that could catch drift (the differential) is hand-fed.
> Full analysis + acceptance criteria in `rfcs/164-port-fidelity-drift-detection.md` (v2, Graefe+Torvalds
> reviewed). **Execution order = ships × leverage** (cheap always-on class-killers first, then the heavy net);
> every found bug gets a committed minimized seed. Owner: query-engine cycle per WS.
> - **[ ] WS-2 — Structural plan invariants** (highest ROI; always-on, Go-only CI). Each must run clean across
>   the WHOLE existing corpus with ZERO runtime skip-lists (exemptions only as compile-time optional slots).
>   - [x] no `<nil>` child in the FINAL extracted plan — LANDED (`ValidatePlanInvariants`, plan-tree walk via
>     genuine-leaf classification; always-on in `PlanQueryForTest`; corpus-clean zero-skip-list; mutation-proven
>     vs IN-LIMIT; `FuzzPlanner_Invariants` 1M+ execs). Kills the ~20-wrapper relink class.
>   - [ ] `WithChildren(GetQuantifiers())` round-trip identity — most direct relink-class catch.
>   - [ ] correlation/quantifier-binding completeness (no dangling correlation).
>   - [ ] set-op comparison-key correctness; result-type/schema consistency; COVERING⊇referenced-fields (→ COUNT-COL).
>   - NOT "DistinctRecords⇒has-dedup-node" (unsound — unique/PK/agg/streaming-agg/intersection are distinct w/o a node) → runtime no-dup or WS-3.
> - **[ ] WS-3 — Single source of truth.** [ ] shared `isCountStar` + guard==consumer audit (cheap, first);
>   [ ] port Java's `RecordQueryPlanVisitor` for plan properties (compile-time exhaustiveness) retiring the
>   `plan_properties.go` switches; reconcile wrapper-held `unique`/`covering`. Graefe + Torvalds.
> - **[ ] WS-1 — Generative Go-vs-Java row-level differential** (highest coverage; heavy-infra, nightly lane).
>   ROW-drift only (plan-shape is WS-2/4); catches 7/10. Acceptance: [ ] schema engineered per bug (multi-key agg
>   index, nullable+NULL sort cols, covering/non-covering pair); [ ] engine-acceptance skew classification;
>   [ ] verify LIMIT path (JDBC setMaxRows? else IN-LIMIT drops to 6/10); [ ] corpus persistence + named lane +
>   budget; [ ] mutation proof. Honest effort ~1-2wk (or narrow first cut 2-3d) — NOT 1 day. Torvalds + /code-review.
> - **[ ] WS-4 — Property/metamorphic tests for Go-only paths.** [ ] cost monotonicity invariant (equality ≤ range);
>   [ ] determinism via a COMMITTED deterministic cost-tie seed (not random fuzz); [ ] CI grep banning bare
>   `range someMap` in plan code (defer the nogo analyzer). Graefe + Torvalds.
> - **[ ] WS-5 — Audit & enumerate Go-only divergences in `DIVERGENCES.md`** ("what Java invariant does this drop?").
>   Means "known reservoirs documented", not "all found". Graefe.
>
> Open hunt bugs: **NULLS-ORDER** is wrong-rows → fix DIRECTLY (extend `RequestedSortOrder` NULLS axis + thread
> through `Ordering.Satisfies`), pinned by WS-2/WS-1 — not "through" the audit. COST-SELECTIVITY + NONDETERMINISM
> ride WS-4 (pin-then-fix). Closing them WITH the nets that would have caught them is the test the workstream works.

> **[ ] BUG (query-engine, wrong results, Graefe-gated) — local residual silently DROPPED on GROUP-BY over a
> correlated join.** `SELECT o.id, COUNT(*) FROM o, t WHERE t.fk = o.id AND t.k = 5 GROUP BY o.id` plans
> IDENTICALLY with and without `AND t.k = 5` → `StreamingAgg(keys=[O.ID], InMemorySort(…, FlatMap(outer=Scan(T),
> inner=PredicatesFilter(Scan(O),[t.fk=o.id]))))`. The local `t.k=5` residual on the driving leg vanishes →
> COUNT over ALL matching `t`, not the `t.k=5` subset → **over-counts**. PRE-EXISTING (verified byte-identical on
> base 33b7307ce + HEAD — NOT the muzzle/Piece-1; found by Graefe's adversarial battery). Non-aggregate is fine
> (`FROM o,t WHERE t.fk=o.id AND t.k=5 AND …` correctly applies the residual on `Scan(T)`); the aggregate/GROUP-BY
> orientation drops it. Repro is the red→green sentinel. Suspected root: the residual-placement/orientation logic
> the GROUP-BY-over-FlatMap path shares with Phase-2b's `RewriteOuterJoinRule` work — confirm before assuming;
> may sequence alongside Piece 2. Graefe gates the fix.

Post RFC-115/116/117/118. The pure-Go client (`pkg/fdbgo`) is launch-ready on correctness + wire
compatibility; everything here is polish/parity/infra — **none gates adoption**. Priority order below;
details live in the phase/section the pointer names. Client items are fresh `fdb-client-engineer` RFC
cycles; query-engine items are `query-engine`/`todo-worker` cycles with a Graefe ACK gate.

1. **[x] C3 (conformance) — Ride their test designs: port FDB's adversarial workloads. COMPLETE.** Cycle /
   AtomicOps / ConflictRange / Serializability / FuzzApiCorrectness reimplemented as scenario +
   invariant specs driving the Go client against testcontainers + `SimTransport` (C4/RFC-118).
   **Increment 1 DONE:** Cycle workload — pure-client serializability oracle (RFC-119, PR #308).
   **Increment 2 DONE:** Cycle under injected wire faults via SimTransport (RFC-120, PR #309).
   **Increment 3 DONE:** Cycle under `process_behind (1037)` + `wrong_shard (1001)` faults (RFC-122,
   PR #320) — 1037 the fixed/QueueModel read row, 1001 its own relocate+invalidate ring-survival
   assertion (flake-free: budget exhaustion → retryable 1007).
   **Increment 4 DONE:** Cycle under a dropped commit reply → `commit_unknown_result (1021)` (RFC-123,
   PR #321) — the faithful commit-path fault (1021 is client-minted from an ambiguous RPC, so a dropped
   reply, not a synthetic error; `not_committed` deliberately NOT injected — unfaithful on an applied
   commit, already exercised by the workload's real conflicts). Drives `commitDummyTransaction` +
   `onError(1021)` self-conflicting retry; ring survives whether or not the dropped commit applied.
   **Increment 5 DONE:** AtomicOps workload (RFC-124, PR #322) — atomic-op + unique-per-attempt
   companion log in one txn; per-group `sum(log)==sum(ops)` oracle holds exactly, healthy AND under the
   commit-drop fault (proving atomic-op+log commit atomically even under ambiguous commits). A probe
   confirmed the same atomic op double-applies under 1021 (faithful — no idempotency IDs), which is why
   the fresh-per-attempt logKey is load-bearing. Serializability gap is already covered by Cycle.
   **Increment 6 DONE:** ConflictRange workload (RFC-125, PR #323) — a two-directional read-conflict-range
   oracle on key-selector getRange, driven through the real `fdb` facade. A concurrent writer (tr2) commits
   between a pinned reader's (tr3) read version and its commit; the oracle is `resultChanged ⟹ foundConflict`
   (under-conflict = `t.Fatalf`, the serializability teeth, revert-proven) with over-conflicts SAFE/counted
   (Go's getKey-then-range selector union is architecturally wider than C++'s combined `addConflictRange`).
   Proved NO under-conflict across the full offset/onEqual/reverse/limit space (deterministic: evaluated=120
   resultChanged=75); guard-key isolation (`maxOffset+1`, proven bound) keeps every resolution in-prefix.
   FDB-C-dev + Torvalds ACK (RFC + impl + delta), codex + @claude + CI green.
   **Increment 7 (FINAL) DONE:** FuzzApiCorrectness (RFC-126, PR #324). The RFC pivoted under review:
   the proposed error-contract fuzzer was NAK'd as padding (Go's error contract is already pinned at
   fixed points + differentials), so the `ExceptionContract` was used as an *audit checklist* — which
   surfaced real, libfdb_c-confirmed wire-contract divergences where Go silently accepted input
   libfdb_c/Java reject: `getRange` row `limit < -1` → `range_limits_invalid (2012)`;
   AddRead/WriteConflictRange + getRangeSplitPoints endpoint `> maxKey` → `key_outside_legal_range
   (2004)` (with the read/write `maxReadKey`-vs-`maxWriteKey` asymmetry); and the metric-op early-return
   precedence (inverted 2005 → cancelled 1025 → poison 2000 → timed_out 1031 → maxKey 2004). Each
   revert-proven + pinned by red→green differentials / deterministic unit tests. Also fixed a
   pre-existing flake in the RFC-121 conflict differentials (conservative-resolver false-positive 1020,
   proven via libfdb_c hitting it too → retry). FDB-C-dev + Torvalds + /code-review + codex + @claude +
   CI all green.
   **C3 COMPLETE:** Cycle (+ read/commit faults), AtomicOps, ConflictRange, FuzzApiCorrectness
   (error-contract axis), Serializability (via Cycle) all covered. Detail: "Native fdbgo client" → C3.
2. **[ ] RFC-056 continuation item 3 — ongoing `/hunt-divergences`.** Standing differential-axis hunt
   vs libfdb_c (atomic-op edges across `Atomic.h`, error-code/option semantics, key/tuple/versionstamp
   encoding). RFC-059→067 closed. Detail: conformance section, "Fresh differential axes".
   **Atomic-op axis hunted (2026-06-25): one concrete divergence found → RFC-149 — DELIVERED (PR #358).**
   The Min→MinV2 / And→AndV2 op-code upgrade lived only in the `fdb` facade, so `client.Transaction.Atomic`
   (and the `cmd/fdb-stacktester` binding tester) shipped legacy `Min(13)`/`And(6)` where libfdb_c ships
   `MinV2(18)`/`AndV2(19)` — diverged on absent-key fold. Fixed: the upgrade now lives in `client.Atomic`
   (the `RYW::atomicOp` analog), 1:1 with `ReadYourWrites.actor.cpp:2243-2248`, gated `apiVersionAtLeast(510)`
   with the API version threaded into `client.database` (mandatory-set → `api_version_unset` 2200). Pinned by
   a cgo in-txn-RYW + committed red→green differential + the 509/510 boundary. FDB-C-dev + Torvalds + codex +
   @claude all green. Next axes: option `defaultFor` matrix, versionstamp-offset edges (RFC-063 still Draft).
   **Full-surface hunt (2026-06-30, PR #403, branch `hunt/fdbgo-client-bughunt`):** two multi-agent
   discovery sweeps over 22 axes → adversarial verify (3 refuted) → **11 fixed red→green** (getKey
   system-key clamp HIGH; watch goroutine race+leak HIGH; RYW more-on-exactly-limit + spurious-1020;
   atomic invalid-opcode incl. silent ClearRange delete; AddReadConflict over-conflict; OnError-honors-
   timeout; Iterator Limit=-1; hedge QueueModel leak; watch legal-range/key-size; too_many_watches;
   StreamingModeExact-no-limit→2210). **Open (in the PR's severity table + `shifts/2026-06-30-fdbgo-client-
   bughunt.md`):** 3 architectural → **RFC-165** (watch-at-committed-version), **RFC-166** (reset() must
   clear non-persistent options — also closes snapshot-ryw-survives-reset), **RFC-167** (getKey isBackward
   shard-location, needs multi-SS/SimTransport) — all Draft, need FDB-C-dev ACK; plus bounded LOWs (atomic
   op-code precedence, oversized system-key Clear drop, buffer-pool sync.Pool race on SendFrame-error,
   SYSTEM_IMMEDIATE+GRV-cache [Go-intentional, needs FDB-C-dev adjudication], dummy-txn jitter law,
   api<520 versionstamp suffix, sendGetValue fallback error-masking). Review gauntlet owed before merge.
3. **[ ] C2-followup — confirm RFC-057's lazy iterator closed the go-vs-cgo 1007-rate** near the 5s
   MVCC edge (profiling, not a fix). Detail: conformance section, "C2-followup".
4. **[ ] Query-engine "one query path" unification.** Route `buildSelectShell`/SimpleTable builder +
   INSERT…SELECT through `visitSelectGroupBy`, delete the legacy builder (CLAUDE.md "no parallel
   pipelines" endgame). Graefe-gated. Detail: "vs Java" follow-ups (RFC-079b + RFC-084) + §7.6 history.
5. **[~] 7.7 — RFC-148 split into Phase 1 (RFC-148) + Phase 2 (RFC-150), both Graefe direction+text ACK'd.**
   - **[x] Phase 1 (RFC-148, Option A):** retire the `isSimpleResidualCompensation` **predicate-shape**
     allowlist (the rot) via `yieldUnknown` exploratory re-optimization; **keep** the inner-scan + index-only
     **safety** guards as `compensationSafeForYield` (a documented stand-in); `yieldUnknown` router + B4
     growth-keyed re-entry guard; `!refIsJoinLeg`/`matchBoundPrefixIsCorrelated` retained. Behavior-preserving
     (plandiff byte-identical + full suite green); rot-fix pinned by `TestPlanHarness_CompoundResidualUsesIndex`
     (OLD full-scanned an OR-residual; now `IndexScan`). Graefe ACK (Option A).
   - **[x] Phase 1 follow-up — index-only `ImplementFilterRule` gate (NAMED, Graefe condition) — DONE (RFC-151).**
     Root cause was NOT unconsumed match-level residual (the match already binds `DistanceRank` via
     `flattenConjuncts`) — it was a SCHEDULING coupling: `pushDataAccessTasks` ran inline before the matching
     rules seeded the ref's partial matches, so data-access consumption relied on `ImplementFilterRule`'s
     incidental physical-filter yield to re-trigger. Fix: `TransformExprTask` re-runs data-access when a rule
     grows the ref's partial-match set (Java `getNewPartialMatches()` reaction, `CascadesPlanner.java:1058`),
     so Java's `ImplementFilterRule` `!isIndexOnly()` gate (`ImplementFilterRule.java:62`) goes in cleanly;
     `compensationSafeForYield`'s index-only branch retired (redundant behind the gate). `validateNoIndexOnlyResidual`
     is **RETAINED as the catch-all backstop** — Graefe + Torvalds both reproduced a JOIN leak (the distance is a
     `Select` predicate → physical residual via `ImplementSimpleSelectRule`/NLJ, which the `ImplementFilterRule`
     gate doesn't see); a logical-side `Plan()` check handles the complementary non-physical case.
     **Sentinels (green):** `TestVectorPlan_QualifyPlansToVectorScan` (plans) + `MetricMismatch` (single-table clean
     error) + `MetricMismatchInJoinDoesNotLeak` (join clean error — the regression pin) +
     `TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual` (unplannable via the kept inner-scan guard).
   - **[ ] Follow-up — gate the remaining physical-filter builders to fully retire the net.** Gate
     `ImplementSimpleSelectRule` + the NLJ residual builder on `!isIndexOnly()` (mirror `ImplementFilterRule`),
     and retire `ImplementIndexScanRule`'s residual-skip loop, so NO builder can emit an index-only physical
     residual — only then is `validateNoIndexOnlyResidual` genuinely dead and removable (Graefe's design-#10 path).
   - **[~] Phase 2b (RFC-150) — split into Piece 1 (DONE) + Piece 2 (in progress).**
     - **[x] Piece 1 — B1 task-graph invariant + retire the `!refIsJoinLeg` muzzle (PR #363, 4 gates green).**
       Ported Java's structural property: `OptimizeInputs` is scheduled only for PHYSICAL/plan members
       (CascadesPlanner.java:524; both construction sites — ExploreGroup :744-748 + executeRuleCall :1064-1070),
       so a correlated leg is pruned to a winner ONLY as the inner child of the binding physical FlatMap, never
       standalone. Gated at the 3 rule-yield sites (`unified_tasks.go`); the 4th (swapped-quantifier impl yield)
       is intentionally NOT gated — load-bearing (gating it breaks `TestFDB_ArrayUnnestOrdinality`), and a
       correlated leg reaching it is harmless (downstream `compensationSafeForYield` + B1a guard). Muzzle +
       `refHasCorrelatedMatch` removed; `matchBoundPrefixIsCorrelated` kept (RFC-069 intersection). plandiff
       byte-identical; +1.1%/+2.0% interning baseline (faithful deferred-optimization timing). Graefe re-ACK +
       Torvalds + codex + @claude.
     - **[~] Piece 2 — retire `tryFlatMapPlan` (PATH A). Step 1-3 DONE (commit b8b3b6ad7); deletion blocked on
       INNER-multiway PATH-B coverage.**
       - **[x] RewriteOuterJoinRule + DefaultOnEmpty null-extension (the LEFT-OUTER enabler — the one shape PATH B
         genuinely couldn't do).** `NamedForEachNullOnEmptyQuantifier` ctor; `RewriteOuterJoinRule` (REWRITING +
         PLANNING) rewrites a CORRELATED LEFT OUTER into Java's nested form (ON-preds below the null-extension
         boundary in a correlated null-supplying SUBSEL, outer made INNER); `yieldGeneralFlatMap` wraps a
         null-on-empty inner in `DefaultOnEmptyPlan` (FlatMap stays a pure map, like Java). Guard: only rewrite
         when an ON-pred references the preserved leg (uncorrelated LEFT — ON FALSE/NULL — stays on the
         materialized NLJ). Row-count-proven: `TestFDB_LeftJoinCountSumPerDept`, `JoinWithLeftAndInnerCompare`,
         `OuterParity_Left` (3-way), `OuterParity_BooleanOn`; plandiff byte-identical (PATH A still competes).
       - **[ ] Make PATH B cover INNER join legs (multiway chain + PK probe), THEN delete `tryFlatMapPlan`.**
         Three layers root-caused over 3 DFS rounds (all validated fixes REVERTED — they only pay off together
         with the deepest layer + the deletion; re-apply as one Graefe-gated change). Disabling PATH A breaks
         `MultiwayJoinIndexProbe`, `MultiwayJoinOrder_Probe/Nway`, `JoinSelPred_Repro`.
         - **Layer 1 (multiway chain) — VALIDATED FIX.** `PartitionBinarySelectRule`'s idempotency guard
           (`rule_partition_binary_select.go:88-93`) blocks on *any* predicate-free 2-quant select in the group,
           so sibling bipartitions of an N-way join never partition → no chained index-probe. Narrow it to the
           SAME quantifier-alias-set as `sel` (Java has no such guard; relies on memo interning). Verified: 3-/4-way
           chain to byte-identical index-probe FlatMaps. Bumps `ChainInterningBaseline` (3-way 9095→11122, 4-way
           31210→46483, < 100k).
         - **Layer 2a (PK probe never generated) — VALIDATED FIX.** `matchSingleSourceAgainstSelect`
           (`rule_match_intermediate.go:350+`) only tries the predicate LHS (`cp.Operand`) as the candidate column,
           so a join pred with the leg's key on the RHS (`O.CUSTOMER_ID = C.ID` → customers PK on RHS) never SARGs
           the leg's PK. Fix: add `ComparisonType.Commute()` (=↔=, <↔>, <=↔>=) + a `bindOrientedComparison` that
           tries as-written then commuted (Java's Value matching is commutative). Verified: generates the
           `Scan(CUSTOMERS,[=corr])` PK probe that didn't exist.
         - **Layer 2b SOLVED + DESIGN-ACK'd (Graefe) — SARG the correlation as a sargable BOUND, not a residual.**
           The PK probe must be captured INSIDE the scan's ScanComparisons (residual-free) so it's a PHYSICAL leg
           member that bypasses `compensationSafeForYield` entirely (which only gates LOGICAL compensations). Java's
           bound-vs-residual line: `PredicateWithValueAndRanges.java:423-432` (`containsKey(alias) →
           noCompensationNeeded`). The 0-row guard is UNCHANGED — it still rejects the genuine residual-correlation
           PR-#201 shape; a sargable-bound correlation is the safe shape Java itself distinguishes. **D.1**
           (commutative SARG in `matchSingleSourceAgainstSelect` + mark the bound pred matched so no residual) +
           **D.2** (physical scan/index wrappers must surface ScanComparisons correlations — Go returns empty,
           a latent bug vs `RecordQueryScanPlan.java:299-302`) are VALIDATED: the unfiltered 2-way correlated join
           produces the bare physical PK probe `FlatMap(Scan(ORDERS), Scan(CUSTOMERS,[=corr]))`. Graefe ACK'd the design.
         - **Layer 2c (THE ACTUAL GATE — a COST-MODEL change, distinct RFC, Graefe-gated, PR-#201 perf surface).**
           Round 4 proof: with D.1 enabling correlated PK probes everywhere, the multiway chain gains an all-PK-probe
           candidate driving off the *largest* table (full scan, zero Fetches, all card-1). The cost model PREFERS
           it over the RFC-042 secondary-index chain driving from the small table, because the fetch-count /
           max-cardinality tiebreaks (`planning_cost_model.go:205/246/272`, criterion #2 + fetch heuristic) fire
           BEFORE `compareJoinOrdering`. Rows correct, but multiway tests fail the index-probe SHAPE (full-scan
           driver = perf regression). **D.1 cannot land standalone** — it makes multiway WORSE without the cost-model
           fix. Fix: make join-order costing prefer driving from the smallest/most-selective table — run
           `compareJoinOrdering` (total recursive join cost) BEFORE the structural fetch/card tiebreaks for
           join-wrapper pairs (or stop criterion #2 rewarding an all-PK chain whose outer is a full scan of the
           larger table). HIGH blast radius (every join plan) → its own RFC + Graefe ACK + full plandiff/row-count/
           1M-stress. Also: the JoinSelPred FILTERED leg (`o.id<10` sibling) doesn't reach
           `matchSingleSourceAgainstSelect` cleanly — a separate match-firing fix.
         Sequence to finish: cost-model RFC (Layer 2c) → re-apply Gap#1 + D.1 + D.2 → filtered-leg match-firing →
         delete `tryFlatMapPlan` (+ cleanup `leftOuter` flag). Keep `tryExistsFlatMap` (EXISTS). FULL OUTER stays
         on the materialized NLJ. All validated round-3/4 fixes were REVERTED (pay off only together with 2c).
         Detail: RFC-150 §3/§4.
   - **[ ] PROCESS HAZARD (found this shift) — the codex-review CLI can leave the repo on a detached HEAD,
     orphaning the branch tip.** Commit a567acb68 (a Torvalds F1 fix) was silently dropped this way — its content
     was not in HEAD's history afterward and had to be re-applied. After running `codex-review`, verify
     `git rev-parse HEAD` still points at the branch and `git status`/`git log` look sane before continuing.
6. **[ ] Parallelize `//conformance` off Ginkgo** [LOW PRIO]. Detail: "Test infra (low priority)".
7. **[~] Java target bump to 4.12.11.0 (from the 4.11 series; RFC-135).** Mechanical bump landed (pins + proto
   sync + regen + version-target docs; `record_query_plan.proto` removed `PVersionValue`/reserved tag
   38, `PExistsPredicate`→`PExistentialValuePredicate`, added `PExistsValue.value` +
   `PRecordQueryExplodePlan.with_ordinality` — all `gen/`-only on the Go side, schema pinned by
   `docscheck.TestPlanProtoSchemaMatches412`). **Behavioural parity = the R-items below, each its own
   RFC, landed one at a time. Verify Java 4.12 actually supports each before treating as parity vs
   allowed Go-extension.**
   - **[x] R1 — DONE (RFC-136, merged in PR #336 `2095a4a7b`).** metadata-evolution field renames
     (`allow{Field,DeprecatedFieldRenames,Undeprecating}` + `RenameFieldsVisitor`) vs Java
     `MetaDataEvolutionValidator`. Landed in the same change as the RFC-135 4.12 upgrade —
     `rename_fields_visitor.go` + all three flags + the `validateField`/`comparePrimaryKeys`/index rewrite.
     RFC-136 was just never flipped from Draft (now corrected). **Small residual follow-up — DONE (RFC-157).**
     The per-node-type shapes + error paths the follow-up named were already ported (stale TODO); RFC-157
     closed the only genuinely-missing axis: the `messageTypeForField` re-derivation at depth ≥ 2 + the
     dead `childSource==childTarget` short-circuit (6 specs; re-derivation behaviorally revert-proof,
     short-circuit branch-coverage). Gate: Torvalds + codex + @claude (all ACK / no findings).
   - **[x] R2 — DONE** — indexer 4.12 changes. **(a) DONE (RFC-137):** erase-indexing-metadata-after-readable —
     `markReadable` now erases scanned-records(1)/type-stamp(2)/heartbeat(7) per Java
     `eraseAllIndexingDataButTheLockAndRangeSet`; added `SetMarkReadable(bool)` (Java `buildIndex(markReadable)`
     parity) so build-state can be inspected pre-readable. Torvalds+codex ACK. **(b) DONE (RFC-138):**
     `SetEnforcedPostTransactionDelay(ms)` — fixed per-transaction delay replacing records-per-second
     when >0 (Java `setEnforcedPostTransactionDelay` #4229). **(c) DONE (RFC-139):** typed-record build-range
     preset (#4244) — `computeRecordsRange` (over the indexed types; null if any lacks a record-type PK
     prefix or is synthetic) + `maybePresetRecordsRange` marks the out-of-range gaps `[nil,begin)`+`[end,nil)`
     built before multi-target/mutual builds, with byte-exact `begin=low.Pack()` / `end=high.Pack()+0xff`
     bounds (Torvalds NAK caught strinc-vs-`0xff`; codex P1 caught the build loop couldn't unpack the
     `+0xff` end — fixed via `unpackRangeEndBoundary`/raw-boundary mark; codex P2 caught non-integer
     record-type keys — preset now gives up for them); added `RecordType.PrimaryKeyHasRecordTypePrefix()` +
     `IsSynthetic()`. **Follow-up (pre-existing, out of scope):** Go's `RecordTypeKeyExpression` only
     encodes integer record-type keys (`int/int32/int64`) and silently falls back to the message type
     NAME for string/bytes explicit `SetRecordTypeKey` — a wire divergence from Java (which encodes every
     key type); the R2c preset already guards against it but the encoding itself should be fixed.
     **N/A:** `SlidingWindowIndexMaintainer` (+163, #4233-adjacent) — pure metrics
     instrumentation for an HNSW window-decorator index type Go does not have; index-scrub rangeSet fix
     (#4226) — Go has no scrubber. Gate: Torvalds + codex + @claude.
   - **[x] R3 — DONE (RFC-140)** — parser grammar: `(AT atAlias=uid)?` on `atomTableItem` (#4112) +
     `functionNameKeyword: LEFT|RIGHT` moved out of `functionNameBase` into `scalarFunctionName` (#4272).
     Parser regenerated. LEFT/RIGHT remain function names but are rejected as identifiers/aliases; AT
     parses + `atAlias` captured but is **rejected** at every consumer (planner FROM/JOIN, aggregate-index
     DDL incl. its silently-dropped JOINs, semantic scope) with `ErrCodeUnsupportedQuery` until R5 binds
     it — codex caught 3 silent-drop holes (column collision, DDL, DDL-JOIN). Graefe + Torvalds + codex ACK.
   - **[x] R4** — EXISTS in the projection list (`PExistsValue.value`), RFC-141. Phase 1 (ExistsValue→ValueWithChild + ExistentialValuePredicate, WHERE-EXISTS) + Phase 2 (FirstOrDefault re-architecture + projected `SELECT EXISTS(...)` + structural reject-the-rest backstop). Graefe + Torvalds + codex (14 rounds) ACK; full `just test` green; pushed (PR #336).
     **Phase 1 DONE:** representation collapse to Java 4.12's single mechanism — `ExistsValue` is now an
     evaluable `ValueWithChild` over a `QuantifiedObjectValue` child (`eval = child != nil`);
     `ExistentialValuePredicate` replaces the deleted leaf-alias `ExistsPredicate`; 8 value-walk sites
     delegate to the child; the 4 join-rule detection sites use `IsExistentialPredicate`. WHERE-EXISTS +
     NOT-EXISTS suite green after the swap, 10× deterministic. codex caught 3 eval/detection subtleties
     (QOV outer-row fallback, non-NOT_NULL misclassification, typed-nil binding). Graefe+Torvalds+codex ACK.
     **Phase 2 DONE (single existential):** re-architected the existential join to Java's emergent shape —
     `ImplementNestedLoopJoinRule` wraps the existential inner in `FirstOrDefault(inner, NULL)` and uses it
     as the FlatMap inner; the FlatMap/NLJ cursors are now PURE MAPS (the `existsMode`/`notExistsMode` +
     `JoinExists`/`JoinNotExists` cursor short-circuits and the FlatMap plan's exists flags are deleted);
     WHERE-EXISTS is a residual `QOV IS NOT NULL` (NOT-EXISTS: `IS NULL`) filter above the FOD — Java's
     `toResidualPredicate`. walk.go produces the same `ExistsValue` for both positions (projection uses it
     directly, predicate bridges via `ExistsValueToQueryPredicate`); the translator registers projected-EXISTS
     subqueries (even with no WHERE) and FOLDS the projection into the existential `SelectExpression`'s result
     value so the boolean is computed by the FlatMap with the inner binding live (Java's `RETURN (q0.ID,
     exists(q1))`). Projected EXISTS / NOT-EXISTS / non-correlated / empty-subquery / join-subquery all green
     (`projected_exists_fdb_test.go`); WHERE-EXISTS + NOT-EXISTS suite green + 10× deterministic;
     `TestFDB_PlanShapeExistsFlatMap` rebaselined to the FlatMap(FirstOrDefault) shape.
     **Phase 2 DONE (ORDER BY / LIMIT + scalar subquery alongside the EXISTS):** the fold now sees THROUGH
     intervening `Sort`/`Limit` (`findExistsFilterUnderUnaryChain`) — the builder emits `Project(Sort(Filter))`,
     so the existential filter is not the project's direct input — folds the projection into the existential
     `SelectExpression`, then re-applies the sort/limit ON TOP (Java's `generateSort(generateSimpleSelect(
     output...), orderBys)`). ORDER BY on a column NOT in the SELECT output ports Java's
     `remainingOrderByExpressions` branch: append the missing sort column(s) to the folded projection, sort,
     re-project to drop them. And scalar-subquery collection (`t.scalarSubqueries`) was hoisted ABOVE the fold's
     early return so `SELECT id, EXISTS(...), (SELECT MAX(id) FROM t2) FROM t1` binds the scalar (was NULL).
     Pinned: `projected_exists_orderby_scalar_fdb_test.go` (ASC/DESC/LIMIT/NOT-EXISTS, non-selected ORDER BY col
     no-leak, scalar in both column positions) — each revert-proof (all-false / NULL without the fix). Full
     sqldriver + conformance green, EXISTS suite 10× deterministic. Graefe+Torvalds ACK.
     **Phase 2 FOLLOW-UP (computed ORDER BY over projected EXISTS — Graefe):** `sortSource.sortKeyName` only
     classifies a bare/qualified column reference; a *computed* ORDER BY expression (`ORDER BY a+b` where
     `a`,`b` aren't in the SELECT) is skipped rather than appended, so it silently mis-sorts. Java's
     `Expressions.difference` uses a semantic `canBeDerivedFrom` check that appends the non-derivable computed
     expression and sorts correctly. Port that: build the sort key's Value (the walker already can) and append
     the computed expression as an extra projection field, matching Java. Strictly narrower than the
     bare-column bug just fixed, zero wire impact, exotic shape — next item, not buried.
     **Phase 2 ROUND-3 (safety guard + 3 fold shapes — codex r3):** the fold pattern-matched plan shapes and
     SILENTLY fell through to a plan evaluating the projected ExistsValue ABOVE the FlatMap (dead binding →
     constant-false) for any unrecognized shape. Added a two-layer **safety guard** that bounds the long tail:
     (a) post-translation `query.CheckProjectedExistsFolded` requires every ExistsValue to be emitted by the
     SelectExpression that OWNS its existential quantifier (else clean `ErrCodeUnsupportedQuery`); (b)
     logical-level `findUnfoldableProjectedExists` + `validateGroupByProjection` EXISTS check reject shapes that
     drop the ExistsValue before translation (GROUP-BY-on-EXISTS, aggregate/distinct/union intervening). Plus the
     3 round-3 fixes: **(1)** projected EXISTS + JOIN in FROM no WHERE — `attachOrSynthesizeExistsFilter` now puts
     the synthesized filter UNDER the projection, `buildExistentialJoinSelect` flattens the join's 2 ForEach + the
     existential into one SelectExpression with the projection as result value, and `implementJoinWithExistential`
     uses the rebased projection as the FlatMap result (leg refs→merged-outer qualified keys, existential
     QOV→inner FOD) for a projected EXISTS over a join; **(2)** ORDER BY on the EXISTS alias — `pullUpSortKeyValue`
     pulls the sort key up to the folded output column (Java `OrderByExpression.pullUp`) so it sorts by the
     materialized boolean, not the raw ExistsValue re-applied above the FlatMap; **(3)** parenthesized
     `NOT (EXISTS(...))` — `existsAtomOf`/`existsAtomInExpressionAtom` unwrap the paren-wrap RecordConstructor to
     find the EXISTS atom (was NULL column). Revert-proof pins: `projected_exists_round3_fdb_test.go` (join-from
     no-leak + correct booleans, ORDER BY ASC/DESC ordering, paren + double-paren NOT, GROUP-BY-EXISTS clean
     reject, multi-existential clean reject). Full sqldriver + `pkg/recordlayer/query/...` + `pkg/relational/core/
     ...` green; EXISTS suite 10× deterministic. **Supported:** projected EXISTS/NOT-EXISTS (corr/non-corr/empty/
     join-subquery), + ORDER BY (incl. EXISTS-alias and non-selected col) / LIMIT / scalar subquery, + INNER JOIN
     in FROM, + paren/nested NOT, + PK/index fast-path. **Cleanly rejected:** multi-existential (>1 projected
     EXISTS or EXISTS in WHERE+SELECT), GROUP-BY/aggregate/DISTINCT/UNION intervening, GROUP-BY-on-EXISTS,
     outer-join FROM with projected EXISTS. Graefe+Torvalds review pending.
     **Round-4 (two more codex-found fold-bypass silent-wrong bugs, fixed):** the fold's early return in
     `translateProject` skips the downstream projection-processing branches; each skipped branch is a latent
     silent-wrong. Audited all bypasses; the two that were silently-wrong on SUPPORTED shapes are now fixed.
     **(1) projected EXISTS + CORRELATED scalar subquery** (`SELECT id, EXISTS(...), (SELECT v FROM t2 WHERE
     t2.fk = t1.id) FROM t1`): the early return bypassed the `translateProjectWithCorrelatedScalar` dispatch,
     leaving the correlated `ScalarSubqueryValue` unbound → that column read NULL every row. The existential
     SelectExpression and the correlated-scalar LEFT-OUTER-join select are incompatible structures (composing
     them is the multi-quantifier boundary the port rejects), so this shape is now CLEANLY REJECTED — both at
     the logical guard (`findUnfoldableProjectedExists`: a projected-EXISTS `LogicalProject` carrying
     `CorrelatedScalarSubqueries` → `ErrCodeUnsupportedQuery`) and defense-in-depth in `translateProject`
     (`len(CorrelatedScalarSubqueries) > 0` before the fold → nil). UNCORRELATED scalar + projected EXISTS
     still works (pre-evaluated, collected before the early return). **(2) QUALIFIED ORDER BY key**
     (`ORDER BY t1.col1 DESC`): the appended/pulled-up sort key was a flat `FieldValue "T1.COL1"` but the
     folded record carries the bare output column → key NULL every row → DESC silently fell to scan order.
     `sortKeyColumnName` + new `stripSortQualifier` now strip the single table qualifier so the appended
     remainingOrderBy column is bare and resolves against the outer scan row; `pullUpSortKeyValue` rebases a
     qualified `FieldValue` key onto the bare output column (only when a bare output field matches — a JOIN-FROM
     qualified output keeps its qualified key). Revert-proof pins: `projected_exists_round4_fdb_test.go`
     (qualified ORDER BY non-selected/selected DESC real ordering + ASC control; correlated-scalar clean-reject
     guard sentinel; uncorrelated-scalar still-works control). Full sqldriver + `pkg/recordlayer/query/...` +
     `pkg/relational/core/...` green; projected-EXISTS suite 10× deterministic. **Rejected (added R4):**
     projected EXISTS + correlated scalar subquery in the same SELECT.
     **Round-5 (two more codex-found silent-wrong regressions, fixed):** **(P1) `SELECT * … WHERE EXISTS(…)`
     reported the inner subquery's columns.** A plain WHERE-EXISTS is planned as an IDENTITY FlatMap (result
     value = the outer-row QOV, with a PredicatesFilter on top); `deriveColumnsFromFlatMap` only special-cased
     the PROJECTED-EXISTS RecordConstructor, then fell through to merging outer+inner columns → the driver
     advertised t1's AND t2's columns even though the cursor emits only the outer row. Fix: detect the
     identity-over-outer FlatMap (result value is a `QuantifiedObjectValue` whose correlation == `GetOuterAlias`)
     and return ONLY the outer plan's columns. **(P2) qualified ORDER BY over a JOIN sorted by the WRONG leg.**
     The round-4 fix stripped `t2.id`→bare `ID` for non-selected qualified keys; for a JOIN source the FlatMap
     merged outer row carries columns under BOTH bare (last-leg-wins) AND authoritative qualified `LEG.COL` keys
     (`mergeRows`), so the bare key picks the wrong leg. Fix: classify the fold's FROM source (`classifySortSource`);
     strip-to-bare ONLY for single-table sources; for a JOIN source keep the QUALIFIED key (`T2.SK`) — the
     appended remainingOrderBy field carries a qualified leg reference (`FieldValue{Field:COL, Child:QOV(LEG)}`)
     that the NLJ rule's `rebaseOuterLegValue` rewrites to the merged row's qualified key, and `pullUpSortKeyValue`
     keeps the qualified key so it resolves the correct leg. Single-table qualified/unqualified, join SELECTED
     and NOT-selected qualified ORDER BY all work; an unqualified ORDER BY of a column that collides across legs
     is rejected cleanly by the semantic analyzer (`42702: column reference is ambiguous`), never silently wrong.
     Revert-proof pins: `projected_exists_round5_fdb_test.go` — P1 `SELECT *`/`SELECT * NOT EXISTS` column-metadata
     tests; the full ORDER BY matrix {single-table, 2-table INNER JOIN}×{selected, NOT selected}×{qualified,
     unqualified}×DESC/ASC with colliding `sk`/`id` columns whose inverse leg orderings make a wrong-leg or no-op
     sort visibly fail. Full sqldriver + `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite
     10× deterministic.
     **Round-6 (two more codex-found silent-wrong regressions, fixed via the consistency root-cause):** both bugs
     came from the projected-EXISTS fold RECONSTRUCTING column-metadata and sort-key derivation piecemeal instead
     of REUSING the normal (non-EXISTS) projection path's logic. Root fix: unify both derivations with the normal
     path so they cannot diverge. **(P2a) ORDER BY a SELECT-list alias whose value is a simple column**
     (`SELECT col1 AS id, id AS x, EXISTS(...) FROM t1 ORDER BY x`): `upgradeSortKeyValues` resolves the alias `x`
     to the projected Value (`FieldValue{ID}`); the fold re-applies the sort ON TOP of the folded projection, but
     `pullUpSortKeyValue`'s FieldValue case returned BEFORE the output-field-value match the non-FieldValue case
     had, so the key resolved by NAME against the output record — reading field `ID` (= `col1 AS id`), the WRONG
     column. Fix: `pullUpSortKeyValue` now runs the output-field-value match (`pullUpToOutputField`, extracted as
     the shared helper) FIRST for EVERY key shape — the same key↔output-field correspondence the normal ORDER BY
     alias path uses — so an alias key pulls up to the output field it IS (`X`), not a same-named column; the
     name-based resolution is the fallback for keys appended via remainingOrderBy. **(P2b) column LABEL regression
     for qualified projections** (`SELECT t1.id, EXISTS(...) FROM t1 …`): `deriveColumnsFromFlatMap`'s folded
     branch set `Name = upper(f.Name)` and left `Label` empty, so the ResultSet exposed the qualified `T1.ID`,
     whereas the normal path keeps the qualified Name for lookup but sets the DISPLAY label to bare `ID`. Fix:
     extracted the normal path's per-column derivation into a shared `deriveProjectionColumnDef(value, alias,
     idx, descs)` helper (Name+Label+type+nullable) reused by BOTH `deriveColumnsFromProjection` AND the folded
     branch; `foldedFieldAlias` recovers the SELECT-list alias from the fold's RecordConstructor field (comparing
     BARE LEAVES so an unaliased qualified column `T1.ID` over value bare `ID` is correctly recognized as
     unaliased → label = bare leaf). A projected EXISTS now produces IDENTICAL label/type/nullability to a
     non-EXISTS control query. Revert-proof pins: `projected_exists_round6_fdb_test.go` — P2a ORDER BY by
     {column alias, expression alias, qualified col, bare col} with distinct values so a wrong-field sort fails
     loudly; P2b label/type/nullability parity with a non-EXISTS control for {bare, aliased, qualified,
     qualified-over-JOIN} columns asserted via `Columns()`/`ColumnTypes()`, plus a qualified-datum value-scan.
     Full sqldriver bazel suite + `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite 10×
     deterministic; all round-1..5 tests still green.
     **Round-7 (computed non-selected ORDER BY over projected EXISTS — codex):** `ORDER BY col1 + 1` where
     `col1 + 1` is NOT a SELECT output. `collectExtraSortColumns` can only append NAMED columns, so the sort
     re-applied above the folded FlatMap evaluated the expression against a record lacking its inputs → NULL
     every row → no-op sort (wrong order). Fix: `translateProjectOverExistsFilter` now BAILS the fold for any
     computed ORDER BY key absent from the projection (→ §8 guard cleanly rejects with `ErrCodeUnsupportedQuery`)
     instead of returning wrong rows. A SELECTED computed expression (ordered by its alias or matching an output
     field) still folds. Revert-proof pin: `projected_exists_round7_fdb_test.go`.
     **Round-8 (two more codex-found metadata/alias divergences, fixed via the alias-provenance root-cause):**
     both bugs came from the fold RE-DERIVING a projected column's alias/Name/Label from the FOLDED record instead
     of carrying the ORIGINAL `LogicalProject.Aliases` provenance. **(P1, SILENT-WRONG) explicit-alias==bare-leaf
     and unaliased-qualified over a JOIN.** `deriveColumnsFromFlatMap` used `foldedFieldAlias` to INFER alias-ness
     by bare-name equality, then `deriveProjectionColumnDef` re-derived the datum `Name` from the field VALUE
     (`ExplainValue`). For `t1.id AS id` the inferred-unaliased datum Name became `T1.ID` while the record is keyed
     by the alias `ID` → a Scan read NULL; for an unaliased `t2.id` over a JOIN the NLJ-rebased composite value
     (`FieldValue{Field:ID, Child:QOV}`) skipped the `Child==nil` bare-compare so the qualified `f.Name` leaked as
     a fake alias → label `T2.ID` not bare `ID`. Root fix: `foldedColumnDef` derives the column metadata DIRECTLY
     from the `RecordConstructorField.Name` — the SAME name the fold set and that `RecordConstructorValue.Evaluate`
     keys the executed row by (`out[f.Name]`): datum `Name = f.Name` (cannot diverge from the record key → never
     NULL), display `Label = bare leaf of f.Name` (Java's post-`clearQualifier` SELECT-list Identifier rule), type
     from the value. `foldedFieldAlias` deleted (no more inference). **(P2, label/type regression) hidden ORDER BY
     re-aliased every visible column.** When a non-selected sort column is appended, the cleanup re-projection that
     drops it force-aliased EVERY field to its datum Name (`projAliases[i] = name`), turning `SELECT t1.id` into an
     explicit alias `T1.ID` (qualified label leaked) and dropping the EXISTS column's BOOLEAN type. Fix: the cleanup
     now reuses the ORIGINAL `p.Aliases[i]` (""==unaliased, leaving unaliased fields unaliased) and preserves each
     value's type; `deriveColumnsFromProjection` additionally inherits a renamed pass-through column's type from its
     inner plan's same-named derived column (the alias is not a proto field, so the descriptor lookup couldn't type
     it). Revert-proof pins: `projected_exists_round8_fdb_test.go` — P1 explicit-alias-over-JOIN + unaliased-qualified
     value scan (reads NULL without the fix) and named-scan; a comprehensive `{bare, aliased, qualified, t1.id AS id
     over JOIN, t1.id unaliased over JOIN}` Name+Label+type+nullability parity matrix vs non-EXISTS controls + a
     non-NULL value scan each; P2 hidden-ORDER-BY label/type parity for {qualified, aliased, bare} columns vs TRUE
     non-EXISTS controls with the same hidden-sort shape. Full sqldriver bazel suite + `pkg/recordlayer/query/...`
     + `pkg/relational/core/...` green; EXISTS suite 10× deterministic; all round-1..7 tests still green.
     **Round-9 (a WHERE-EXISTS correctness REGRESSION + a metadata divergence — codex):** **(P1, SILENT-WRONG,
     regression of plain WHERE-EXISTS) the existential inner correlation collided with the outer source alias.**
     An alias-shadowing self-subquery (`SELECT id FROM t WHERE id > 1 AND EXISTS (SELECT 1 FROM t WHERE id = 1)`)
     gives the outer source alias and the existential inner correlation the SAME name (`T`), because the
     post-FlatMap re-architecture derived the inner correlation from `GetSourceAliases()[1]` = the subquery's
     SOURCE TABLE name. The FlatMap then bound BOTH the outer row and the FirstOrDefault inner under that one
     correlation (the inner CLOBBERED the outer → the pass-through row was NULL: `converting NULL to int64`),
     and an outer-only predicate (`id > 1`, correlated to the shared name) was MISCLASSIFIED as an inner join
     predicate and pushed below the FOD. The old semi-join cursor returned the outer row directly, so it worked
     before. Root fix (Java: every existential quantifier has its OWN unique correlation identity, never the
     source table's name): `existsInnerCorrelation` (cascades_translator.go) registers the existential inner
     under the UNIQUE existential quantifier alias (`esq.Alias`, from `UniqueCorrelationIdentifier()`) and
     rebases the join predicate onto it via `RebasePredicate`, so outer and inner correlations are distinct
     by construction and predicate classification stays correct. Guarded to a SINGLE-TABLE-scan inner
     (`existsInnerSafeToRename`): a JOIN inner or a nested-EXISTS inner (a `LogicalFilter` carrying its own
     `ExistsSubqueries`) produces MERGED rows keyed by the real leg aliases / carries internal source-alias
     references the rename can't reach, so those keep the source-alias (leg) routing — the alias-shadow clobber
     only arises for the single-alias-bound scan. Applied at all 3 build sites (`buildExistentialSelect`,
     `buildExistentialJoinSelect`, `translateJoinWithExists`). **(P2, metadata) unaliased computed select item
     named by expression text.** `SELECT id + 1, EXISTS(...) AS e FROM t` — the fold named the folded computed
     field with the expression TEXT (`ID + 1`), so `Rows.Columns()` reported `ID + 1` where the normal
     projection path exposes an unaliased non-field (computed) expression under the GENERATED positional name
     (`_0`); adding the EXISTS thus changed the public column name and broke positional references. Fix:
     `translateProjectOverExistsFilter` names an unaliased non-`FieldValue` (computed) column with the SAME
     positional `_i` the normal path uses (`deriveProjectionColumnDef`/`executeProjection`), so the folded
     column's record key + Name + Label are identical to the non-EXISTS control on every axis. Revert-proof pins:
     `exists_alias_shadow_fdb_test.go` (P1: WHERE-EXISTS, NOT-EXISTS, correlated, and projected alias-shadow self-
     subqueries — all returned NULL/wrong before the fix) and `exists_computed_column_fdb_test.go` (P2: column-name
     parity with a `SELECT id + 1` control read dynamically, + correct values). Full sqldriver bazel suite +
     `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite 10× deterministic; all
     round-1..8 + WHERE/NOT-EXISTS tests still green.
     **ROUND-10 (codex; two MORE silent-wrong bugs, both fixed at root, NO shape rejected):**
     **(P2a, silent-wrong) MULTI-TABLE EXISTS inner correlating to a NON-rightmost leg.**
     `EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)` — the existential inner is a multi-table (comma/JOIN)
     source; `existsInnerCorrelation` reports only the RIGHTMOST leg (t3), and the NLJ rule classified the
     correlation predicate by that single inner correlation. A predicate referencing t2 (non-rightmost) matched
     neither the inner-correlation test NOR "outer-only" correctly → it was evaluated with NO inner binding and
     DROPPED EVERY OUTER ROW (WHERE returned 0 rows; projected returned `false`/empty). Root fix
     (`rule_implement_nested_loop_join.go`): a predicate goes BELOW the FirstOrDefault iff it references ANY
     correlation OTHER than the FlatMap's outer leg(s) — i.e. it touches the inner subquery (`predicateTouchesInner`,
     variadic over the outer correlations). The FlatMap binds the outer row(s) under exactly the outer
     correlation(s); every other correlation is an inner leg (the existential source, or a multi-table FROM leg).
     Applied in BOTH existential-join methods: `implementExistentialSelect` (single outer) and the JOIN-in-FROM
     `implementJoinWithExistential` (two outer legs). Audit confirmed correct for 2-leg/3-leg, leftmost/rightmost
     correlation, inner-only join predicates, explicit `JOIN…ON` inner, NOT-EXISTS, non-correlated, outer-predicate
     combos, projected, and JOIN-in-FROM variants — NO shape needs rejection; the merged inner row resolves leg
     columns by qualified key and the live outer binding resolves the correlated column.
     **(P2b, silent-wrong ORDER) qualified ORDER BY key whose bare name collides with a SELECT alias.**
     `SELECT col1 AS id, EXISTS(...) FROM t1 ORDER BY t1.id` — the fold stripped `t1.id`→bare `ID`, which equals the
     SELECT-list alias `id` (= col1); the "already in output" check then matched the output ALIAS by name and the
     sort ordered by col1 instead of t1.id. Root fix (`cascades_translator.go`): output membership for a sort key is
     now VALUE-based (`sortKeyInOutput` / `sortKeySourceValue` — an output field must genuinely PROJECT the source
     column the key references, never merely share a bare name with an output alias); a non-projected qualified source
     key is appended as a hidden `remainingOrderBy` field NAMED BY ITS QUALIFIED PROVENANCE (`T1.ID`, collision-free
     with the output alias) carrying the source-column value, and `pullUpSortKeyValue` resolves the key by VALUE
     match (raw key first — SELECT-list aliases incl. the computed EXISTS boolean — then the source-column value).
     The bare-alias ORDER BY path (`upgradeSortKeyValues` sets `k.Value` to the projected value) is UNCHANGED.
     Revert-proof pins: `projected_exists_round10_fdb_test.go` — P2a {2-leg non-rightmost/rightmost, 3-leg,
     inner-join-pred, explicit JOIN…ON, NOT-EXISTS, outer-pred, projected, projected-JOIN-from, WHERE-JOIN-from}
     all asserting correct rows + single-table control; P2b qualified-`t1.id` ASC/DESC ordering (col1 sequence), the
     bare-alias-is-output-column control, and the selected-qualified pull-up control. Both reverts verified to fail
     the exact dimension. Full sqldriver bazel suite + `pkg/recordlayer/query/...` + `pkg/relational/core/...` green;
     EXISTS suite 10× deterministic; all round-1..9 + WHERE/NOT-EXISTS tests still green.
     **ROUND-11 (codex; the round-10 routing fix REGRESSED a scalar-subquery shape — two silent-wrong bugs, both
     fixed at root, NO shape rejected; §8h):** **(P1, silent-wrong) route by the KNOWN inner-leg set, not "any
     non-outer".** Round-10's `predicateTouchesInner` routed a predicate BELOW the FirstOrDefault iff it
     referenced ANY correlation other than the outer leg(s) — an ABSENCE test. An UNCORRELATED SCALAR SUBQUERY in
     a predicate (`price > (SELECT MAX(x) FROM t2)`) has its OWN `ScalarSubqueryValue` alias — non-outer yet NOT
     an inner table leg (a pre-evaluated EXTERNAL binding) — so the absence test pushed the scalar predicate below
     the FOD; alongside an empty NOT-EXISTS the FOD yields NULL, its IS-NULL residual admits every outer row, and
     the below-FOD scalar comparison never ran → the scalar predicate was SILENTLY DROPPED (`price > MAX(x) AND
     NOT EXISTS(empty)` returned every NOT-EXISTS-true row incl. those failing `price > MAX(x)`). Root fix
     (`rule_implement_nested_loop_join.go`): `collectInnerLegAliases(innerRef, innerCorr)` computes the existential
     inner's FROM-source-alias set (innerCorr ∪ all legs the subplan DECLARES — every `SelectExpression.GetSource
     Aliases()` + ForEach/Physical quantifier alias, never a value-tree binding), distinguishing multi-table (innerCorr
     IS a declared leg → return ALL legs, keeping round-10) from renamed single-table (innerCorr NOT declared → return
     {innerCorr} only, re-avoiding the round-9 alias-shadow leak by construction); `predicateReferencesInnerLeg`
     routes below the FOD iff the predicate's correlation set INTERSECTS that set — scalar-subquery / parameter /
     other external bindings stay OUTER and actually filter. Applied at both methods (`implementExistentialSelect`,
     `implementJoinWithExistential`). **(P2, silent-wrong) the projected-EXISTS fold dropped a WHERE-clause scalar
     subquery.** `SELECT id, EXISTS(...) FROM t1 WHERE price > (SELECT MAX(x) FROM t2)` — the fold's early return in
     `translateProject` bypasses `translateFilter`, the only place `f.ScalarSubqueries` is registered for
     pre-evaluation, so the WHERE scalar stayed unbound (NULL) → `price > NULL` → 0 rows. Fix:
     `translateProjectOverExistsFilter` now collects `f.ScalarSubqueries` (same fold-bypass class as round-4).
     Predicate-routing audit (outer-only, inner-leg single/multi-table, scalar-in-pred, NOT-EXISTS, projected,
     parameter-marker, projected+WHERE-scalar, correlated-scalar-rejected): each correct or cleanly-rejected, no
     silent-wrong. Revert-proof pins: `projected_exists_round11_fdb_test.go` — scalar+NOT-EXISTS (empty inner),
     scalar+EXISTS, scalar+multi-table-NOT-EXISTS, projected-EXISTS+WHERE-scalar, parameter-marker control, audit
     controls; dataset built so the scalar EXCLUDES a NOT-EXISTS-true row (id 0, price ≤ MAX) so a dropped scalar
     loudly INCLUDES it. Routing revert → scalar+NOT-EXISTS returns `[0 1 2 3 4]` (want `[2 4]`), scalar+EXISTS `[]`
     (want `[3]`); fold-collection revert → projected+WHERE-scalar `[]`. NLJ-rule change → **Graefe ACK needed**.
     Full sqldriver bazel suite + `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite 10×
     deterministic; all round-1..10 + WHERE/NOT-EXISTS tests still green.
     **Round-12 (the CONVERGENCE BACKSTOP — codex r12 found EXISTS in WRAPPED/NESTED positions silently wrong):**
     EXISTS can appear ANYWHERE in an expression tree, so point-handling each shape never converges. The fix is a
     comprehensive structural backstop: any EXISTS NOT in a directly-handled position is detected (typed predicate/
     parse tree, never `GetText`) and REJECTED cleanly with `ErrCodeUnsupportedQuery` — never silently mis-evaluated.
     Directly-handled = (WHERE) a direct existential / NOT-existential (`IsExistentialPredicate` /
     `IsNotExistentialPredicate`, top-level or single-NOT, incl. each AND conjunct); (SELECT) a top-level projected
     `EXISTS`/`NOT EXISTS` or its single paren/NOT wrapper. **(P1a wrapped WHERE EXISTS):** an existential buried
     under any other wrapper (`WHERE NOT (NOT EXISTS(...))`, `EXISTS(...) OR p`, deeper AND/OR/NOT) fell into the
     regular-predicate bucket where the empty FirstOrDefault's NULL default is never removed → every outer row
     silently passed. New `query.CheckBuriedExistentialPredicate` (post-translation, alongside
     `CheckProjectedExistsFolded`, run on BOTH the SELECT and DML planning paths — `DELETE/UPDATE … WHERE NOT (NOT
     EXISTS)` reuses the existential NLJ rule and was equally silent-wrong, matching every targeted row) walks every
     predicate-bearing expression; a top-level predicate that is not a direct existential but CONTAINS an
     `ExistentialValuePredicate` at any depth (`predicates.ContainsExistentialPredicate`) → reject. **(P1b nested projected EXISTS):** `CASE WHEN EXISTS(...) THEN ... ELSE ... END`, `EXISTS(...) AND x`,
     `(EXISTS(...) OR x)`, `NOT (EXISTS(...) AND x)` took the predicate path → the ExistsValue evaluated ABOVE the
     FlatMap with the binding dead → constant false / NULL. New `expr.NestedExistsProjectionError` (raised in
     `walkExpressionInner` in projection position when the SELECT item CONTAINS an EXISTS atom via
     `ContainsExistsAtom` but is not one of the 3 directly-foldable shapes via `isDirectlyFoldableProjectedExists`)
     — a DISTINCT error from `UnsupportedExpressionShapeError` (which callers swallow to the silent-wrong text
     path); the two projection callers convert it to `ErrCodeUnsupportedQuery`. Also corrected a fake-checkbox test
     (`TestFDB_SubqueryInCase`) that asserted `CASE WHEN EXISTS(...)` "works" while only checking `err==nil` and
     never validating the (all-ELSE, silent-wrong) rows → now pins the clean rejection. Revert-proof pins:
     `projected_exists_round12_fdb_test.go` — P1a {double-NOT, OR, buried-in-AND, NOT-of-AND} + P1b {CASE-WHEN, AND,
     NOT-of-AND, OR} guard sentinels (each FAILS if rows return) + controls (every directly-handled WHERE/SELECT
     shape still works, incl. a direct nested EXISTS in a subquery WHERE) + a DML sentinel/controls
     (`TestFDB_ProjectedExistsRound12_DML`) + JOIN-ON / ORDER-BY / WHERE-scalar sentinels+controls
     (`TestFDB_ProjectedExistsRound12_OtherPositions`) + DML/INSERT-SELECT WHERE-scalar sentinels+controls
     (`TestFDB_ProjectedExistsRound12_DMLScalar`) + an `expr.WhereExistsInScalarPosition` unit test
     (`where_exists_position_test.go`); `predicates.ContainsExistentialPredicate`
     unit-tested across wrapper depths. **Adversarial audit (other tree positions):** three more silent-wrong
     positions, all where the EXISTS is not a top-level boolean term: (a) JOIN ON (`ON EXISTS(...)`) — ON resolver
     has no SubqueryPlanner, EXISTS dropped, every joined row passed; (b) ORDER BY key (`ORDER BY EXISTS`, `ORDER BY
     CASE WHEN EXISTS`) — sort resolver has no SubqueryPlanner, key kept raw text, never evaluated → wrong ordering;
     (c) WHERE EXISTS BURIED in a scalar (`WHERE CASE WHEN EXISTS THEN 1 ELSE 0 END = 1`, `WHERE (EXISTS) = true`) —
     lowered into a CASE/comparison operand, folded to constant false → every row dropped. (a)+(b) via
     `expr.ContainsExistsAtom` (in `upgradeJoinOnPredicates` + the ORDER-BY validation, plan_visitor.go +
     logical_predicate.go); (c) via a new structural parse-tree walk `expr.WhereExistsInScalarPosition` (an EXISTS is
     directly-handled iff reached through ONLY boolean nodes AND/OR/NOT/paren; buried via any scalar node) — run on
     the SELECT WHERE (plan_visitor.go), the DML WHERE (`DELETE/UPDATE … WHERE <buried EXISTS>`, at the DML dispatch),
     and across an `INSERT … SELECT` subtree (`expr.AnyWhereExistsInScalarPosition`; the INSERT-SELECT body is rebuilt
     through a path that bypasses the per-statement guard). All rejected cleanly. HAVING-EXISTS already surfaced a
     clean "could not plan query"; EXISTS in an UPDATE SET value surfaces a clean type-mismatch. `ORDER BY
     <EXISTS-alias>`, a top-level WHERE EXISTS/NOT-EXISTS/AND-conjunct/paren, and a direct DELETE/INSERT…SELECT WHERE
     EXISTS are NOT rejected (preserved).
     Both backstops verified revert-proof (disabling them returns the
     silent-wrong rows). NLJ-rule reasoning change → **Graefe ACK** + **Torvalds ACK** (got both). Full sqldriver bazel suite +
     `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite 10× deterministic; all round-1..11
     + WHERE/NOT-EXISTS tests still green. **Final supported = exactly the directly-handled positions; cleanly
     rejected = ANY EXISTS elsewhere** — codex round-13 bar: NO silent-wrong EXISTS case.
     **Round-13 (convergence fix — boundary stop; the round-12 detectors must NOT descend into nested subqueries):**
     codex round-13 found the FINAL convergence issue — an OVER-rejection (not silent-wrong, so that surface stays
     closed). The round-12 structural detectors recursed into nested subqueries, so an EXISTS in a nested scalar / IN /
     derived-table subquery's OWN clause was mis-attributed to the outer expression and falsely rejected —
     `SELECT id, (SELECT MAX(id) FROM t2 WHERE EXISTS (SELECT 1 FROM t3)) FROM t1` failed with "projected EXISTS in this
     query shape is not yet supported". Fix: each subquery is classified in its OWN translation context; a shared
     `expr.introducesNestedQueryScope` helper (`SubqueryExpressionAtomContext` + `InListContext`) makes
     `ContainsExistsAtom` / `WhereExistsInScalarPosition` / `AnyWhereExistsInScalarPosition` /
     `isDirectlyFoldableProjectedExists` STOP at subquery boundaries (still match an `ExistsExpressionAtomContext` at the
     current level). The logical-/value-tree detectors were already boundary-safe (`ScalarSubqueryValue.Children()` is
     `nil`). Pinning the variants EXPOSED a real silent-wrong bug: the subquery-build path
     (`buildLogicalPlanForSelectWithCTECatalog_postBuild`) lacked the `WhereExistsInScalarPosition` guard the PlanVisitor
     path has, so a buried-scalar EXISTS in a nested subquery's own WHERE silently folded to constant-false (inconsistent
     with the standalone subquery, which rejects) — guard added there; and the WHERE-walk error handlers swallowed a
     nested `*api.Error` into text-fallback (generic "could not plan") — now propagated verbatim. Tests:
     `projected_exists_round13_fdb_test.go` (round-13 query + variants plan & return correct rows; buried-CASE-EXISTS
     subquery rejects in its own context matching standalone, for scalar/derived-table/EXISTS-subquery forms; controls
     for the genuine round-12 outer-level rejections) + detector unit pins in `where_exists_position_test.go`. Full
     sqldriver + yamsql + plandiff + query/core green; EXISTS suite 10× deterministic; round-1..12 still green.
     **Phase 2 FOLLOW-UP (computed ORDER BY over projected EXISTS — Graefe):** `sortSource.sortKeyName` only
     classifies a bare/qualified column reference; a *computed* ORDER BY expression (`ORDER BY a+b` where
     `a`,`b` aren't in the SELECT) is skipped rather than appended, so it silently mis-sorts. Java's
     `Expressions.difference` uses a semantic `canBeDerivedFrom` check that appends the non-derivable computed
     expression and sorts correctly. Port that: build the sort key's Value (the walker already can) and append
     the computed expression as an extra projection field, matching Java. Strictly narrower than the
     bare-column bug just fixed, zero wire impact, exotic shape — next item, not buried.
     **Phase 2 FOLLOW-UP (multi-existential, separate larger extension):** >1 existential quantifier in one
     query — multiple projected EXISTS, and EXISTS in WHERE *and* SELECT together (Java exists-in-select.yamsql
     lines 85, 94) — needs nested FlatMaps with intermediate record-bundling (`RETURN (..., exists(q5._0),
     exists(q5._1))`) and projection-QOV→bundled-field rewriting. This was NEVER supported in Go — multiple
     WHERE-EXISTS (`WHERE EXISTS(...) AND EXISTS(...)`) already "could not plan query" on master;
     `implementExistentialSelect` handles a single existential (2 quantifiers) only. Now CLEANLY REJECTED by the
     round-3 guard (was silently-wrong); out of scope for Phase 2 as a feature.
   - **[x] R5** — correlated array UNNEST in FROM (`FROM t, t.arr AS x`) + `AT ordinality`
     (`PRecordQueryExplodePlan.with_ordinality`, 1-based INT). Implemented in RFC-142: parser preserves
     uid segments + AT alias on comma sources (`select_parser.go`); a `LogicalUnnest` operator carries
     them to the translator, which classifies a comma source against the in-scope record types and lowers
     a correlated `FlatMap(outer, Explode(FieldValue{arr} over QOV(outer)), …, resultValue, false)` —
     reusing the existing NLJ-rule FlatMap path (the Explode guard now only fires on the uncorrelated
     IN-list shape). `ExplodeExpression`/`RecordQueryExplodePlan` gained `WithOrdinality` (folded into
     equals/hash/result-type; `executeExplode` emits a 2-field `{_0:element,_1:i+1}` record, 1-based,
     resetting per outer row); a name-based ordinal `FieldValue` (`ofOrdinalNumber` analog) binds AS→`_0`,
     AT→`_1`. AT on a non-array source converges on `ErrCodeWrongObjectType` (42809). Works: base unnest,
     ordinality, AT-only, NOT-NULL/nullable/empty/single arrays, string arrays, filter-on-element (ordinal
     preserved), filter-on-ordinal, alias name-collision (unnest shadows via a `Shadowing` ScopeSource).
     **Follow-ups (clean-rejected, never silently wrong):** multiple/chained unnests in one FROM (nested
     FlatMap merged-row threading), struct-array element field access, computed SELECT projection over the
     ordinal (driver-level column projection). Gate: **Graefe** + Torvalds.
     **Refactor follow-ups (acknowledged-not-blocking by Graefe/Torvalds in the R5 full-pass review):**
     - **Dedup the group-key-output-name helper.** The aggregate group-key output name is computed three
       times under three names — `aggKeyName` (executor), `aggregateGroupKeyOutputName` (embedded), and the
       `havingPredicatePushesBelowAggregate` mirror. Fold into ONE shared helper at the values/executor
       boundary. This is a pre-existing aggregate-naming wart the unnest shadowing merely exposed; the same
       pre/post-aggregate name mismatch was rediscovered three separate times during R5. (Graefe)
     - **Refactor the NLJ rule's `rebaseOuterLegRefsToMerged`** to call the translator's generic
       `mapPredicateValues` walk instead of carrying its own predicate-tree recursion — the same recursion
       lives on both sides of a package boundary. (Torvalds)
     - **Collapse `outerSourceIsCTE` + `outerSourceIsDerivedTable`** into one helper: they are always invoked
       together as a single `||` (the CTE/derived-output rejection), so the two-arm split is redundant. (Torvalds)
     - **Unify the two SELECT-build paths behind one driver.** There are two SELECT builders — `PlanVisitor`
       (top-level) and the catalog builder `buildLogicalPlanForSelectWithCTECatalog` (subquery/DML/derived) —
       which today share the identical unnest-aware helpers but are still two drivers. Rounds 25/28/30 each
       found the catalog path missing a step the top-level path had; the round-26 audit confirmed parity, but
       a single driver would make it *structurally impossible* to add an unnest-aware step to one and miss the
       other. (Graefe, R5 full-pass round-32)
     - **Reject general duplicate FROM range-variable aliases.** `FROM A AS X, B AS X` (two real tables, same
       alias) plans cleanly in Go but Java rejects it (`SemanticAnalyzer` forbids duplicate range-variable
       names); R5 added the unnest-specific `rejectDuplicateUnnestAlias` but the general case is a separate
       pre-existing divergence. (surfaced during R5 round-29)
   - **R5 final codex pass — DEFERRED to Jun 25 (codex quota exhausted).** R5 went through 32 codex rounds
     (all fixed + revert-proof-tested) + Graefe + Torvalds full-pass ACK; the codex quota hit its limit at
     round 33. Run one final `codex review --base <R4-commit>` on the committed R5 when the quota resets
     (Jun 25 ~09:52) to confirm codex-clean; address anything it finds before the umbrella PR merges.
   - **[x] R6** — `CARDINALITY()` function + index support (RFC-143). Phase 1 (scalar function, `e3adb2a4a`) +
     Phase 2 (cardinality index — function-key bridge, evaluator, DDL, planner-matching + general IS-NULL
     value-index ranges, `2c8e5a78d`). Graefe + Torvalds ACK both phases; final codex pass deferred to the
     Jun 25 quota reset (PR #336 stays draft). Gate: **Graefe** + Torvalds.
     - **[x] Phase 1 — the scalar function (no index).** `CARDINALITY(array) → nullable INT` wired SQL→
       `CardinalityValue` via a BY-NAME built-in dispatch at the `walkFunctionCall` leaf
       (`expr.walkUserDefinedScalarFunction` → `walkCardinality`; CARDINALITY parses as a bare-ID
       `UserDefinedScalarFunctionCall`). Fixed the 3 divergences in the orphaned `CardinalityValue`:
       `Type()` → `NullableInt` (Java `primitiveType(INT)`, nullable → metadata INTEGER, was NotNullLong);
       array-type validation at the walk site (non-array arg → `CANNOT_CONVERT_TYPE`/22000, matching the
       yamsql); `ExplainValue` renders `cardinality(<child>)`. Satellite gate `isAllowedFunction`
       (cascades_generator) accepts CARDINALITY by name (NOT added to the generic
       `IsCascadesSafeScalarFunction`/`evalScalarFunction` lists — it's a dedicated Value). Resolved array
       columns now carry an `ArrayType` (new `semantic.Column.IsArray` populated from the repeated-field
       descriptor; `expr.columnCascadesType`), so `isArray()` passes and metadata is correct. FDB tests in
       `array_cardinality_fdb_test.go` (count, WHERE `= N` / `IS [NOT] NULL`, ORDER BY, error, metadata,
       EXPLAIN). **§3a note:** Go writes plain-repeated arrays (no nullable-array wrapper), so an
       empty/unset array is wire-indistinguishable from NULL → reads as NULL → `CARDINALITY([])` is NULL,
       not 0 (Java's wrapper distinguishes them). The function is faithful; the empty-vs-NULL distinction
       is the latent §3a nullable-array-wrapper-WRITE gap (below), out of scope for Phase 1.
     - **[x] Phase 2 — index support** (the 4.12.3 delta; Graefe-gated planner matching). A `CARDINALITY()`
       index makes `WHERE CARDINALITY(arr) = N` / `IS [NOT] NULL` and `ORDER BY CARDINALITY(arr)` ASC/DESC
       use INDEX scans (EXPLAIN shows `IndexScan`, not a full `Scan` + `PredicatesFilter`/`InMemorySort`).
       - **Step 4 — evaluator:** `CardinalityFunctionKeyExpression` (`key_expression_cardinality.go`) embeds
         the generic `FunctionKeyExpression` (so it serialises to the identical `Function{name:"cardinality"}`
         proto, field 9, wire-compatible) and overrides `Evaluate` with Java's two protobuf fast paths
         (plain repeated field; nullable-array WRAPPER descent for Java-written records) plus the
         materialize-and-count fallback. `createsDuplicates()==false` (Java override), `ColumnSize()==1`.
         Empty/unset Go array → NULL key (§3a-consistent with the scalar).
       - **Step 6a — KeyExpression→Value bridge:** `ValueIndexScanMatchCandidate` carries a parallel
         `columnFunctions []string` + a `ColumnValue(i, base)` producing `CardinalityValue(FieldValue(col))`
         for a cardinality column (plain `FieldValue` otherwise). `ExpandValueIndex` (via the
         `columnValueProvider` interface) and `ComputeMatchedOrderingParts` both consult it, so candidate +
         query sides build the IDENTICAL Value. `metadataIndexDef.IndexColumnFunctions()` derives the tags
         from the index's `KeyExpression` (the recordlayer→cascades half of the bridge).
       - **Step 5 — index DDL:** `CREATE INDEX … AS SELECT CARDINALITY(arr) … ORDER BY` recognises the
         bare-ID CARDINALITY call by typed node and routes to `Builder.AddCardinalityIndex` →
         `CardinalityExpr(field(arr, Concatenate))` value index (`buildCardinalityIndex`).
       - **Step 6b — predicate matching:** `WHERE CARDINALITY = N` falls out of `valuesMatchColumn`; added a
         `*CardinalityValue` alias-invariant arm (mirrors the FieldValue / distance-row-number arms).
       - **Step 6c — ORDER-BY rework:** `rule_ordered_index_scan.go` now matches the sort key by Value-tree
         equality against the candidate's `ColumnValue` (the `sortKeyMatchesColumn` helper), not a
         `FieldValue`-name string — so `CardinalityValue` sort keys bind (incl. REVERSE). Plain-field
         ORDER BY unchanged.
       - **Value-index NULL ranges (Java-aligned, surfaced by the `IS [NOT] NULL` cases):** `IS NULL` is now
         a `[null]` EQUALITY range and `IS NOT NULL` a `(null,+inf)` INEQUALITY range for value-index
         matching — Java's `ScanComparisons.getComparisonType(IS_NULL)==EQUALITY` / `NOT_NULL==INEQUALITY`.
         `ComparisonRange.Merge` classifies them; `isSargableComparisonForMatch` admits them (only the
         index-match gate, not the base `isScanRangeCompatible` the NLJ path uses); the executor builds the
         `[null]`/(null,+inf) ranges (`IS NOT NULL` was already supported). This closed a general Go
         divergence (no value index bound `IS [NOT] NULL` before) — `TestPlanHarness_IsNull` updated to the
         now-correct index plan; full sqldriver suite green.
       - Tests: `array_cardinality_index_fdb_test.go` (EXPLAIN-asserted: `= N`, `IS [NOT] NULL`, ORDER BY
         ASC/DESC, covering, plain-field no-regression controls incl. plain `IS [NOT] NULL`),
         `cardinality_ddl_test.go` (DDL → `CardinalityFunctionKeyExpression` + catalog proto round-trip),
         `key_expression_cardinality_test.go` (evaluator fast paths + wrapper + wire round-trip). 10×
         deterministic. **Note:** nested-struct array (`tab2_index` in the yamsql) is blocked on STRUCT
         column support in the metadata builder (`buildCardinalityIndex` already builds the dotted-column
         nesting); lands with struct columns.
     - **[ ] §3a follow-up — nullable-array-wrapper WRITE.** Go's metadata builder emits a plain repeated
       field for both nullable and NOT-NULL arrays; it does not write Java's `message{ repeated values }`
       wrapper, so a Go-written NULL array can't be distinguished from an empty one. Closing this lets
       `CARDINALITY([])` be 0 (not NULL) for a non-null empty array, matching Java. Latent divergence
       (read path already unwraps Java-written wrappers via `unwrapWrappedArray`); separate from R6.
   - **[x] R6 follow-up — BITAND/BITOR/BITXOR registry drift.** The "unreachable" diagnosis became stale:
     all three dedicated bit-expression routes execute and are covered by `bitwise.yaml`. RFC-190.8 closes
     the real maintainability gap by moving their admission, evaluator operator, and declared result type
     into the shared scalar-function catalog; the dedicated grammar lowering remains intentional.
   - **[x] R7** — LEFT/RIGHT OUTER JOIN reclassification + 4.12 null/boolean fixes (RFC-144). The parity
     sweep (53 ported `join-tests-outer.yamsql` cases) found + fixed **6 real outer-join divergences**
     (JOIN USING → cartesian; chained outer joins + INNER-then-LEFT dropped NULL-padding; derived-table-
     on-right + derived-primary dropped ON/JOIN; RIGHT JOIN SELECT* col order). Materialized NLJ kept
     (Graefe ACK). Plus `EliminateNullOnEmptyRule` replacing the buggy `PullUpNullOnEmptyRule` (latent-rule
     hygiene — no SQL producer) with BC1 faithful `rejectsNull` + BC2 `ImplementSimpleSelectRule` exact-
     type tightening; null/boolean/folding verify-and-pin (3 documented benign/orthogonal gaps). Reclassify:
     LEFT/RIGHT OUTER now Java-aligned; FULL stays Go-only (Java rejects). Graefe + Torvalds ACK; final
     codex pass deferred to Jun 25 (PR #336 draft). Gate: **Graefe** + Torvalds.
     - **[ ] R7 follow-up (Torvalds) — JOIN USING typed-path lowering.** `synthesizeUsingOnExpr`
       (`select_parser.go`) builds the equi-join ON by splicing the raw uid text into `"<l>.col = <r>.col"`
       and re-parsing (works + documented for quoting round-trip, but deviates from Java's typed
       `resolveJoinUsingClause` → `resolveFunction("=")`). Replace the text-splice with typed Value/expr
       construction. Non-blocking; pre-existing style deviation.
     - **[ ] R7 follow-up (Torvalds) — USING `.asHidden()` + `SELECT * USING` test.** Go does not implement
       Java's right-column hiding for USING, so `SELECT * … USING (id)` emits the join column twice
       (honestly documented in the code comment, but untested). Implement `.asHidden()` for the USING
       right column AND add the `SELECT * USING` duplicate-column case to `outer_join_parity_fdb_test.go`.
   - **[~] R8** — conformance rebaseline from a live 4.12.11.0 run. **Partial in the bump PR:** the 7
     RFC-082 annotations 4.12 lifted were reclassified to keep the conformance gate green (4 Java bug-fixes
     → plain equivalence; `left_outer_join_basic` + `where_case_returns_bool_probe` lifted → plain
     equivalence; `bare_bool_where_rejected` → JavaSucceedsGoRejects). **Remaining:** full corpus re-sweep,
     reclassify cross-engine specs/comments encoding lifted 4.11 limits, flip `SQL_CONFORMANCE.md`
     (`CASCADES_DIVERGENCE.md` folded into `DIVERGENCES.md`, RFC-175 F1), clear the `DIVERGENCES.md`
     rebaseline banner. Gate: Torvalds + codex + @claude.

> **Prior wave closed:** D1 (RFC-118 SimTransport), B2 (RFC-109 escape hatch), the RFC-056 lazy GetKey
> iterator (RFC-057), the GRV-cache divergence (RFC-104), and B1/CI-off-the-box (untracked, owner
> decision). The `[x]` bullets below are that wave.

> **CI: the single self-hosted box is intentional — NOT a tracked problem.** We work locally + sequentially;
> the slowness during the RFC-115→117 merge wave was a one-off (four PRs squeezed through one runner at once).
> Don't re-file a "second / ephemeral runner" or "CI reproducibility off the box" item. (The §7 CI-volume
> tofu/cloud-init is fail-safe dead-ish code — `prevent_destroy` protects the box and nothing auto-applies —
> harmless to leave; revisit only if the box actually starts failing on disk.)

> **C3 (RFC-056 lazy GetKey iterator) — DONE (RFC-057):** `rywSegCursor` replaced the materializing
> `buildSegmentsLocked` (55,437× faster at N=100k, behavior-identical). The residual go-vs-cgo 1007-rate near
> the 5 s MVCC edge is characterized (RFC-056 #235, TODO `C2-followup`) as accepted perf/timing, not a wire
> bug. Don't re-file.

- [x] **D1 — `SimTransport`** (frame-level fault injection) — DONE (RFC-118; FDB-C-dev + Torvalds +
  /code-review ACK; PR gauntlet codex/@claude/CI pending push). One rule-driven proxy-frame loop
  (`simConn` + a per-frame intercept callback) consolidates the bespoke `wrongShardConn`/`dropReplyConn`;
  faithful inline-error injection via the `ErrorOr<reply>`(tag=value) channel real FDB uses for read
  errors (`types.MarshalErrorOrInlineError`). Closes the four C4 deferred Phase-0 test gaps below.
- [x] **B2 — libfdb_c escape hatch** — DONE (RFC-109, PR #295). `BackendDatabase` interface
  (`pkg/fdbgo/fdb/backend.go`) + a CGo-backed impl over `cgofdb` (`pkg/fdbgo/libfdbc/backend.go`),
  selected at BUILD time via the `libfdbc` build tag (`pkg/internal/fdbclient`, netgo/netcgo idiom) —
  NOT runtime config, because libfdb_c's network thread is process-global + unrecoverable so there is
  no live switch between backends anyway (FDB-C-dev + Torvalds vetted; hardened across 11 codex rounds).

> Shipped this session (stacked on `master`, merging bottom-up #303→#304→#305/#306):
> **RFC-116** (#305) GRV/watch/locate operation-span attribution; **RFC-117** (#306)
> `Optional<primitive-scalar>` wire codegen. Both FDB-C-dev + Torvalds + /code-review + codex green.

---

## Client launch-readiness — prioritized stack (2026-06-13)

The pure-Go FDB client (`pkg/fdbgo`) is the launch target. The RFC-010 wire-correctness audit
is essentially complete (14/15 + 1 false positive; RFC-050/051/052/072 + RYW RFC-055/056/057/058/
065/098 all Implemented; read-path reply-timeout shipped in PR #288). The items below are the
remaining launch-readiness work, ordered by priority — **Go-code correctness first, escape hatch
last** (it's a pre-launch safety net, not a blocker for adoption). Driven one at a time via the
`fdb-client-engineer` skill (RFC → FDB-C-dev + Torvalds + codex review → implement → review-clean),
each on its own stacked branch.

1. **[x] GRV cache parity — `USE_GRV_CACHE` opt-in (default off), client correctness.** DONE
   (RFC-104; also fixed the `updateMinAcceptable` MAX→MIN divergence = the filed "RFC-056 item 2").
   `M` ·
   fdb-client-review. Go's `grvCache` is ALWAYS-ON; C++ serves cached read versions only when the
   app sets `USE_GRV_CACHE` (gate `NativeAPI.actor.cpp:7505`, default false `:6148`). Demonstrated
   wrong answer: a Go txn served a cached version OLDER than a libfdb_c-committed seed → seed keys
   invisible. Add the `USE_GRV_CACHE` tx/db option; gate `tryCache` + the background refresher on
   it; match `:7504-7518`. Revisit RFC-096's cache-carried `locked` check if this closes. (Full
   detail in the "GRV cache is ALWAYS-ON" entry below.)
2. **[x] Retry-predicate fidelity — `fdb.IsRetryable` vs `client.isRetryable`.** DONE (RFC-105):
   no bug — pinned each to its C++ analog + deleted the dead 4th predicate `wire.FDBError.Retryable`.
   `S` ·
   fdb-client-review. The two predicates list different code sets. The fix is NOT naive unification:
   in C++ `fdb_error_predicate(RETRYABLE)` ≠ `Transaction::onError`'s set (1039 predicate-retryable
   but not onError-retried; 1006 the reverse). Make each match its OWN C++ predicate, share the
   per-code facts, pin both against the C++ source.
3. **[x] Resource limits / backpressure (multi-tenant launch safety).** DONE (RFC-106a) — clean
   tri-ACK (Graefe + Torvalds + codex), HEAD `a396227e`. `M` · query-engine-gated. Statement timeout
   (per-request ctx deadline → 54F01), scan-limit options wired to `ExecuteProperties` with Java
   semantics + `FailOnScanLimitReached`, `MAX_ROWS`/result-byte caps, SQLSTATE 54F01 mapping. The
   completeness work (9 codex rounds) swept the out-of-band/scan-limit dimension across every leaf
   cursor, buffered operator, DML path (atomic abort, no partial mutation), executor stream wrapper,
   value drain helper, and cursor iterator — none silently truncates. The per-query MEMORY byte budget
   is split to **RFC-106b** (deferred: needs every cardinality-growing buffer charged + a CI lint that
   also covers the out-of-band handling for new leaf cursors / drains). (TODO-production P1.9.)
4. **[x] Make CI gates real.** DONE (RFC-107) — Torvalds ACK + codex clean, HEAD `b1779f49`. `M`.
   New `nightly-stress.yml` (query-generated stress labels + no-op guard, latency reported not gated);
   `client-fuzz` job fuzzing all 23 `//pkg/fdbgo` Fuzz targets Bazel-natively (faithful to the cgo/
   MODULE.bazel patch) + the 8 unfuzzed diff-oracle reply types; `//pkg/fdbgo/client+transport+fdb`
   added to the PR `-race` gate. The review caught + fixed two silent-pass footguns: a `docker info`
   preflight on EVERY FDB-driving gate (else `FDB not available` skips → exit 0 → green with no
   coverage), and `steps.<id>.outcome != 'skipped'` guards so a skipped preflight can't publish an
   empty report. (Also fixed the `codex` CLI hang via a new `codexreview` tool in the codex-review
   skill — root cause: `codex exec` blocks on open stdin.) (TODO-production P1.6.)
5. **[~] CI reproducibility — off the single Hetzner box. UNTRACKED (owner decision, 2026-06-18).**
   The single self-hosted box is intentional: we work locally + sequentially; the RFC-115→117
   merge-wave slowness was a one-off (four PRs through one runner), not cache thrash (warm cache
   confirmed). Don't re-file a 2nd/ephemeral-runner or CI-reproducibility item. See the `# NEXT`
   CI note for the full rationale. Revisit only if the box actually starts failing on disk. (Was
   TODO-prod P1.8.)
6. **[x] libfdb_c escape hatch (Backend interface + CGo-backed impl) — DONE (RFC-109, PR #295).**
   `BackendDatabase` interface + a CGo-backed impl over `cgofdb`, selected at BUILD time via the
   `libfdbc` build tag (not runtime config: libfdb_c's network thread is process-global + unrecoverable
   so a live backend switch is impossible anyway). FDB-C-dev + Torvalds vetted; 11 codex rounds. (Was TODO-production P2.2.)

## Known gaps

- **[RFC-186 follow-up] PartitionSelectRule (≥3-way) null-on-empty axis is unprobed.**
  PartitionBinarySelectRule now declines to absorb predicates into a null-on-empty leg
  (post-box WHERE must not move inside a dissolved outer box; pinned in
  rule_partition_binary_select_test.go). The ≥3-way twin has no such guard, but every
  observed noe-carrying invocation across the box-join families exits at its `>= 3`
  quantifier gate (probed 2026-07-22: 108/108 hits were binary), so the axis is latent,
  not live. If a ≥3-quantifier noe-carrying select ever reaches it, the same placement
  question applies with an extra wrinkle: a noe leg peeled into a lower select away
  from the preserved sibling it null-supplies against would move the outer-join edge
  across a select boundary. Needs a probe + guard (or a proof it composes) before any
  producer of that shape lands.

- **[RFC-180 follow-up, pre-existing] Output-alias vs rendered-item name collision
  under the IMMEDIATE post-aggregate strip.** `SELECT player AS "SUM(SCORE)",
  SUM(score) AS s2 FROM scores GROUP BY player ORDER BY SUM(score) DESC` sorts by
  the ALIASED player column, not the aggregate — fails identically on pre-RFC-180
  master (verified at 58ee9daa7), so it is not a regression of the SortKey.BareRef
  work (whose deferred-strip variant is pinned green in aggregate_order_by_java).
  Root cause: the reshaping projection's output row carries alias-preferred column
  NAMES, and both upgradeSortKeyValues' colToIdx and translateSort's flat-column
  fallback bind sort keys BY NAME against that row — a delimited alias that spells
  another item's canonical rendering shadows it. Fix direction: positional binding
  (ordinal-baked ProjectedValues on the reshaping projection, and translateSort's
  fallback matching PROJECTION texts rather than alias-preferred names). Needs its
  own review cycle — translator field-naming surgery, not a gate tweak.


### [ ] query-engine (RFC-173, latent — surfaced by the 2-way EXISTS-in-ON review): F2-LEFT's isScanFamilyLeg is cteScope-BLIND — VERIFIED REACH, not silent-wrong (low priority)
`isScanFamilyLeg` (cascades_translator.go:3185) is a syntactic Scan-through-Filter walk with NO cteScope
resolution: a CTE-name scan whose body is a JOIN or an OPAQUE BOX (aggregate/union/sort) reads as
scan-family. The 2-way EXISTS-in-ON gate hit this (fixed with a cteScope-aware `scanFamilyLegCteAware`), but
`isScanFamilyLeg` is ALSO used by the F2-LEFT projected-EXISTS-over-LEFT-JOIN fold
(buildExistentialJoinSelect / existsLegBirthsPositional) to gate scan-leg-only LEFT boxes.
**VERIFIED empirically (Torvalds action item, resolved): a projected-EXISTS LEFT join whose preserved leg is
a CTE-backed JOIN gives `0AF00` (unplannable), and a CTE-backed AGGREGATE leg gives `42703` — both VISIBLE
errors, NEVER wrong rows.** The distinction from the 2-way gate: that gate built the ordinal seed DIRECTLY
(so a CTE box slipped through to wrong rows), whereas F2-LEFT folds through the EXECUTOR's `legIsOrdinalSafe`
gate, which rejects a CTE-backed non-scan leg → 0AF00/name-model, structurally never silent-wrong. So this is
a REACH gap (Go rejects visibly where Java may answer), NOT a latent correctness bug — priority downgraded.
Still worth the cteScope-aware unification for cleanliness (make scanFamilyLegCteAware the shared authority)
and to convert the 0AF00 into a fold where Java answers, but it is NOT a silent-wrong hazard. Not urgent.

### [x] query-engine (RFC-173 S4 — RESOLVED by Graefe FEASIBILITY ruling: the correlated-index EXISTS name path is PERMANENT / Java-correct — NOT a shortfall, NOT a cap blocker)
UN-BOOKED. The "ordinal-fold-over-index-matched-box" enhancement is **NOT ACHIEVABLE and NOT NEEDED**
(Graefe feasibility ruling, confirmed at 4 code sites). A WHERE-EXISTS correlating into a leg BURIED in an
inner join is the canonical semijoin; its good plan is a CORRELATED INDEX SCAN (`Scan(A,[=b.aid])` SARG'd
under the FlatMap) which REQUIRES NAME BINDING to flow the sibling comparand into A's index. `correlatedStep1`
(rule_implement_nested_loop_join.go:1973) IS the index-SARG signal; `buildCorrelatedFlatMapPlan` (:449)
passes the name-model RC straight through with NAMED correlations; `foldStep1Seed` (:1244) returns
gated=false the instant correlatedStep1 is set — no ordinal seed is ever born, deliberately. Baked
positional `ofOrdinal` refs cannot resolve against the box's name-keyed runtime row; re-birthing the box
ordinal BREAKS the SARG (BakedNameContextError). **The "ordinal twin of name resolution over an index-matched
box" IS name resolution.** RULING (Option a, ~0 net code): the correlatedStep1 name path is PERMANENT and
Java-correct — Java binds EVERY correlation by name (no positional-correlation concept); the ordinal seed is
a Go-only optimization for the INDEPENDENT-legs materialized-NLJ case, where no name binding is needed.
Rejected: (b) positional index-matching (no Java analog, architecturally divergent); (c) accept the
cross-product (throws away the index where it matters MOST — a regression, not a cap). **CONSEQUENCE FOR THE
ATOMIC CAP (task #16): it CANNOT delete NewAnchoredJoinRecord entirely — the correlated-index existential
shape KEEPS it, correctly (Java-aligned). The cap's premise "delete the name model in ONE commit" is
re-scoped: the correlatedStep1 name path survives; the cap deletes the name model only for the shapes that
do NOT need name binding (independent-legs materialized joins).** Pinned: TestFDB_RFC173_CorrelatedIndexExistsStaysIndexed
(EXPLAIN asserts the SARG'd `[=]` index scan, not the cross-product; + correct rows) — trips if a future
producer-retirement re-ordinalizes this shape and drops the index (the reverted commit-A wall).

### [x] query-engine (PRE-EXISTING): correlated `EXISTS(SELECT COUNT(*)/MAX(...) ...)` silently filters instead of using aggregate cardinality — FIXED 2026-07-10, completed for WHERE pagination/polarity by CQ-3 2026-07-24
**FIXED via the cardinality constant-fold, which succeeded where the wrap approach was reverted twice.**
A correlated WHERE-EXISTS over a NON-GROUPED aggregate (COUNT(*)/MAX/SUM, no GROUP BY / HAVING / QUALIFY /
windowed OVER) produces exactly one row before pagination. BuildExists now records a tri-state
`ExistsSubquery.KnownTruth`: the pre-pagination one-row proof is followed by the literal LIMIT/OFFSET
cardinality calculation, so no pagination / LIMIT>=1 OFFSET 0 is TRUE while LIMIT 0 / OFFSET>0 is FALSE.
`foldKnownExists` substitutes that truth in direct/top-level-AND positive and negated WHERE markers before
routing, dropping the existential quantifier entirely. This keeps the correlated-aggregate semi-join — and
therefore the JOINED-OUTER correlation-placement hazard that killed the wrap — out of the plan. Pagination
atoms still unresolved at planning time (public bound arguments are substituted before parsing) and
data-dependent positive OFFSET for non-global shapes (including GROUP BY) are 0AF00 typed declines.
Projected, nested-boolean, and JOIN-ON known-truth consumers are also typed declines until their separate
substitution paths exist; HAVING stays blanket-rejected. The gathered arity>=3 projection fast path folds
before bypassing `translateFilter`, and DML now shares SELECT's parse-tree window-aggregate rejection.
Pinned by classifier/fold/planner unit tests, live COUNT/MAX/SUM + joined-outer/gathered + DML FDB coverage,
yamsql aggregate-pagination cases, and uncorrelated controls.

<details><summary>original characterization + the two reverted wrap attempts (audit trail)</summary>

Baseline-confirmed on bebf23b0e.
An EXISTS whose inner SELECT is a **NON-GROUPED** AGGREGATE is ALWAYS TRUE: a non-grouped
`COUNT(*)`/`MAX(...)`/`SUM(...)` yields EXACTLY ONE row even over an empty (post-WHERE) input (`COUNT`→0,
`MAX`→NULL), so the existential is satisfied for every outer row. Java 4.12.11.0 keeps all outer rows; Go
treats it as correlated-filtering and drops the non-matching ones. Repro (live-verified): `SELECT p.v FROM
p, q WHERE q.qid = 5 AND EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id)` with p={(1,10),(2,20)},
e={eref=1} → Java `[[10],[20]]`, Go `[[10]]`; same with `MAX(eid)`.
**CONTROLLED CHARACTERIZATION (2026-07-10 probe, p={1,2,3} e={eref=1}) — corrects the proposed fix below:**
  - `corr-count` `EXISTS(SELECT COUNT(*) FROM e WHERE e.eref=p.id)` → Go `[1]`, want `[1,2,3]` — **BUG (live)**
  - `corr-max`   `EXISTS(SELECT MAX(e.eid) FROM e WHERE e.eref=p.id)` → Go `[1]`, want `[1,2,3]` — **BUG (live)**
  - `uncorr-count` `EXISTS(SELECT COUNT(*) FROM e)` → Go `[1,2,3]` — **CORRECT** (no correlation to filter on)
  - `corr-grouped` `EXISTS(SELECT COUNT(*) FROM e WHERE e.eref=p.id GROUP BY e.eref)` → Go `[1]` — **CORRECT**
    (a GROUPED aggregate emits ZERO rows over an empty group → EXISTS = has-match, must still filter).
The 2-leg existential-fold path (tryExistsFlatMap / implementExistentialSelect), NOT the N-way seed.
**CORRECTED FIX (the prior "aggregate/GROUPED existential inner must not route as semi-join" was WRONG — it
would make the grouped case always-true, a NEW bug; the grouped control proves grouped MUST filter):** the
rewrite applies to a **NON-GROUPED aggregate inner with NO HAVING only** (guaranteed exactly one row →
EXISTS always TRUE — a constant-fold of the existential). A GROUPED inner, or a HAVING that can empty the
single group, is NOT always-true and stays a semi-join. Mirror the correlated-scalar path's no-GROUP-BY
handling (RFC-047: "Empty input → 0 groups → NULL; vs no-GROUP-BY COUNT → 0"), which already distinguishes
these correctly — the EXISTS path just doesn't reuse that logic. GATED: query-engine change (EXISTS
routing) → Graefe design-ACK; multi-path (WHERE/projected × correlated/non-correlated) so NOT a one-line
inline fix. Regression test to add once fixed: the four controlled shapes above (bug + grouped/uncorrelated
controls) both polarities, a HAVING-empties-group control, vs a scalar-non-aggregate control.

**ATTEMPTED + REVERTED (2026-07-10, `5d4ff3711` reverted): root cause pinned, but the fix has subtle holes
codex caught — REQUIRES the scope-leak fix FIRST.** Root cause (EXPLAIN-verified, DEFINITIVE): the correlated
fallback `buildCorrelatedExists` (BuildExists, logical_predicate.go) rebuilds a correlated inner from
FROM+WHERE and DELIBERATELY DROPS the SELECT list — correct for `SELECT 1`, but it drops the
cardinality-forcing aggregate, so the plan is byte-identical to `EXISTS(SELECT 1 …)` and tests raw
row-existence. The attempted fix (wrap the rebuilt inner in a trivial `COUNT(*)`, correlation applied below
the aggregate) fixed the base WHERE/NOT-EXISTS cases + all controls, but **codex ultra found 3 P1 holes**:
  1. **LIMIT/OFFSET ignored** — `EXISTS(SELECT COUNT(*) … LIMIT 0)` emits ZERO rows, not one. The detector
     must also require no row-eliminating LIMIT (LIMIT 0) and no positive OFFSET (`sq.limit`/`sq.offset`).
  2. **SCOPE LEAK (the keystone — same root cause as the projected-42803 booked below)** — when the SELECT
     projects an EXISTS whose OWN subquery has an aggregate (`EXISTS(SELECT EXISTS(SELECT COUNT(*) FROM f)
     FROM e WHERE e.eref=p.id)`), the aggregate-detection scope leak harvests the NESTED COUNT(*) into the
     enclosing `sq.aggCols`, so the row-preserving middle SELECT is misclassified as an aggregate → wrongly
     always-true. The detector CANNOT trust `sq.aggCols`/`countStar` until the scope leak is fixed.
  3. **mixed predicate** — `lastJoinPredicateOuterOnly` means "CONTAINS an outer-only conjunct," not "ALL
     conjuncts outer-only"; a mixed nested-EXISTS predicate (`e.eref=p.id AND p.id=1`) leaves the inner ref
     ABOVE the aggregate → rejects matching rows. Split the predicate; apply inner-referencing conjuncts below.
  P2: don't COUNT(*) an unconditional existential (full inner scan per outer row) — CONSTANT-FOLD to a
  single-row source after validation.
**CORRECT SEQUENCING (the proper gated slice):** (a) fix the aggregate-detection SCOPE LEAK FIRST (the booked
projected-42803 below — the SAME bug) so the detector can trust the query-scope aggregate set; (b) then the
cardinality fix, guarding LIMIT/OFFSET (#1) + splitting mixed predicates (#3) + constant-folding (P2). Four-gate.

**RE-LANDED THEN REVERTED AGAIN (2026-07-10, `55e0c845f`..`89186f2b9` reverted).** The keystone (a) landed,
so the fix (b) was re-landed: helper `queryInnerIsUnconditionalOneRow` + wrap the correlated inner in
`LogicalAggregate([], COUNT(*))` with the correlation applied BELOW the aggregate. It passed Graefe ACK +
Torvalds ACK (after test-quality rounds) and all single-outer shapes — but **codex ultra found TWO more real
P1s (both reproduced), one a REGRESSION**:
  4. **WINDOWED aggregate in DML → silent-wrong.** `COUNT(*) OVER ()` is also an `AggregateWindowedFunctionContext`;
     `sq.countStar`/`aggCols` discard the OVER clause, so the helper wraps it always-true. Top-level SELECT
     rejects windowed aggregates (0AF00), but `planDML` does NOT: `UPDATE p SET x=1 WHERE EXISTS(SELECT
     COUNT(*) OVER () FROM e WHERE e.eref=p.id)` updates ALL p rows (repro: rows_affected=3, want 1). Fix:
     exclude windowed aggregates (check `OverClause() != nil`, mirror ddl.go:632 / cascades_generator.go:277).
  5. **JOINED OUTER source → REGRESSION (0AF00 / malformed ordinal plan).** When the enclosing SELECT reads
     from a JOIN (`SELECT p.id FROM p, g WHERE EXISTS(SELECT COUNT(*) FROM e WHERE e.eref=p.id)`), the EXISTS
     is handled by `translateJoinWithExists`, which expects the correlation in `ExistsSubquery.JoinPredicate`;
     clearing `lastJoinPredicate` (to place it below the aggregate) hides it → 0AF00 (cross) / "field ID not
     resolvable … malformed plan" (LEFT JOIN). This PLANNED before the fix (as the pre-existing [1]
     silent-wrong) → genuine regression. **CRITICAL: the ORIGINAL bug repro has a joined outer (`FROM p, q`),
     so the fix as-built did NOT even fix the reported shape — single-outer only.** A conservative
     decline-on-joined-outer (`len(p.outerScopes)>1`) would leave the original repro unfixed, so it's NOT a
     valid floor. The proper fix needs a joined-outer-aware correlation path: keep the correlation reachable
     by `translateJoinWithExists` (JoinPredicate) WHILE the aggregate sees it below — the correlation-placement
     tension codex named. THIS is the real remaining work for the cardinality fix; do it before re-landing.
Reverted; the keystone (a) stays landed + 4-gated. codex P1s #4/#5 are the precise remaining constraints.

**NEXT-SLICE MECHANISM — use CONSTANT-FOLD, NOT the wrap (the key redirection).** The wrap-and-clear approach
is fundamentally blocked on P1#5: `translateJoinWithExists` (cascades_translator.go:4945 — the delicate
ordinal-wedge/arity-2 flatten) expects the correlation in `ExistsSubquery.JoinPredicate`, but the aggregate
consumes the correlated column, so it MUST be below the aggregate; integrating an aggregate-wrapped
self-correlated inner into that flatten is deep translator work. **Sidestep it entirely: fold
`EXISTS(unconditional-one-row) → TRUE` and `NOT EXISTS(…) → FALSE`, dropping the ExistsSubquery.** No
ExistsSubquery ⇒ no JoinPredicate ⇒ no `translateJoinWithExists` involvement ⇒ P1#5 (joined-outer regression)
CANNOT arise; and the DML windowed path (P1#4) is guarded by the same detector. Graefe endorsed the fold as
valid (identical rows; a Go read-side optimization; wire-compat untouched — the plan-shape divergence from
Java's semi-join is allowed on the read side). BUILD: reuse `queryInnerIsUnconditionalOneRow` (non-grouped
aggregate; exclude GROUP BY / HAVING / QUALIFY / LIMIT 0 / OFFSET>0 / **windowed via `OverClause()!=nil`**);
fold at the predicate/value construction sites — WHERE-EXISTS (constant TRUE conjunct), projected ExistsValue
(constant TRUE), with NOT-EXISTS polarity (→ FALSE); HAVING position too. Then full four-gate. The fold is a
multi-site interception but each site is a local substitution, with NONE of the wrap's correlation-placement
or translator-integration risk.

**REFINED FOLD MECHANISM (architecture verified):** EXISTS is NOT a predicate node — `splitNonExistsPredicates`
(cascades_translator.go:5242) lowers it to an EXISTENTIAL QUANTIFIER (the semi-join), so the fold is NOT an
`ExistsValue→TRUE` predicate substitution. Instead: (1) front-end BuildExists sets an `AlwaysTrue` flag on the
ExistsSubquery when `queryInnerIsUnconditionalOneRow` (+ windowed `OverClause()` guard) — a small local
change, NO wrap/clear. (2) Translator: at the sites where `f.ExistsSubqueries` become Existential quantifiers
(:2314/:2541/:2558), for a POSITIVE WHERE-EXISTS simply SKIP emitting the quantifier for an AlwaysTrue esq
(EXISTS=TRUE ⇒ no filter, the other WHERE conjuncts stay) — a clean local skip, NO translateJoinWithExists.
NOT-EXISTS(AlwaysTrue) ⇒ FALSE ⇒ empty result (emit a contradiction filter — needs the negation-polarity of
the esq, which `splitNonExistsPredicates` tracks); projected ExistsValue(AlwaysTrue) ⇒ substitute a TRUE
literal in the result value (values.WalkValue exists; needs a bool-literal constructor + a value rewrite).
Positive-WHERE is the cleanest sub-case to land first; NOT-EXISTS + projected follow (or decline initially).
Four-gate each.
</details>

### [x] query-engine (PRE-EXISTING, surfaced fixing the EXISTS-aggregate fold): `parseLimitClause` silently ignores an unparseable LIMIT/OFFSET literal → the clause is dropped — FIXED
FIXED. `parseLimitClause` did `strconv.ParseInt(atom.GetText())` and left the -1 no-limit / 0-offset
sentinel on failure, so a syntactically-accepted but non-integer LIMIT literal (`LIMIT 0.0`, `LIMIT 0L`) was
SILENTLY DROPPED: `SELECT p.id FROM p LIMIT 0.0` returned ALL rows (correct is 0). Fix: a new `resolveLimitAtom`
helper rejects a `decimalLiteral` atom that fails ParseInt with a loud 42601 syntax error, while a
`preparedStatementParameter` (`LIMIT ?`) still returns unresolved (parameter binding unchanged — separate
concern). `parseLimitClause` now returns `(limit, offset, err)`, threaded through all callers (visitLimit +
its call site, the qualified-star rebuild re-read, and extractFromSimpleTable). Applies to BOTH the limit
and offset atom, so `LIMIT 1 OFFSET 1.0`
/ `OFFSET 1L` reject too. Also dropped the dead positional-`LIMIT a,b` fallback (the grammar is
`LIMIT limit=... (OFFSET offset=...)?` — both atoms are labeled, no positional form). Pinned:
`TestFDB_InvalidLimitLiteralRejected_RFC128` (standalone: bad literals → 42601, plus `LIMIT 0`→0 rows and
`LIMIT 2 OFFSET 3` positive controls) + the 42601 pins in
`exists_over_aggregate_fdb_test.go` (`limit_invalid_literal_rejected`,
`offset_invalid_literal_rejected`). CQ-3 separately fixed the valid positive-OFFSET execution path. Full
suite green.

### [x] query-engine (PRE-EXISTING residual): correlated `EXISTS` over a non-grouped aggregate ignored LIMIT/OFFSET and used plain row-existence — FIXED CQ-3 2026-07-24
The fallback no longer applies pagination to raw correlated rows. It proves the aggregate's exact one-row
output first and then evaluates literal pagination cardinality: `LIMIT 0` or any positive OFFSET yields a
known-empty subquery, while LIMIT>=1/OFFSET 0 preserves the row. The translator substitutes the resulting
EXISTS truth (including NOT inversion) and never builds the raw-row semi-join. The live discriminator seeds
two matching raw rows and checks OFFSET 1 — a bogus raw-row pagination fix would leave one row and fail.
Unknown runtime atoms and grouped positive OFFSET decline 0AF00; invalid literals remain 42601.

### [x] query-engine (PRE-EXISTING; KEYSTONE): aggregate-detection SCOPE LEAK via harvestAggregates — FIXED 2026-07-10 (`befc32a8e` → `3e51a55e6` → interface-arm fix), scalar + EXISTS + IN all closed
**FIXED.** `harvestAggregates` (select_parser.go) walked a projected expression's tree promoting aggregates
into the enclosing query's set but only stopped at SCALAR subquery atoms, not EXISTS — so `SELECT p.id,
EXISTS (SELECT COUNT(*) …) FROM p` leaked the inner COUNT(*) → spurious 42803 (AND broke the cardinality
fix's detector, codex P1#2). Fix: stop at the unifying NESTED-QUERY node instead of enumerating atom types.
Final guard: `case *antlrgen.QueryContext, antlrgen.IQueryExpressionBodyContext`. `*QueryContext` catches
scalar + EXISTS (both wrap a `query`) and also truncates the subquery's own `ctes?`; the INTERFACE
`IQueryExpressionBodyContext` catches a bare `queryExpressionBody` (an IN subquery: `InList` →
`queryExpressionBody`, concrete node `*QueryTermDefaultContext`/`*SetQueryContext` — NOT the bare
`*QueryExpressionBodyContext`, which is why an earlier concrete-type arm was DEAD and missed IN; codex's
catch). A real outer aggregate that merely CONTAINS a subquery (`HAVING COUNT(*) IN (…)`) is still harvested
(pre-order walk reaches it outside the subquery's query node). Gated: Graefe + Torvalds ACK (befc32a8e and
the 3e51a55e6 delta); codex found the dead concrete-arm bug on the delta → fixed. Pinned:
`exists_aggregate_scope_leak_fdb_test.go` (scalar/EXISTS/real-aggregate/IN). Note: an IN-subquery-of-aggregate
no longer 42803s — it now surfaces the HONEST, SEPARATE reach gap below (IN-subquery unsupported → 0AF00).

### [ ] query-engine (SHARED gap with Java, pre-existing, orthogonal to the scope leak): IN-subquery is unsupported → 0AF00
`x IN (SELECT …)` does not plan in this Cascades engine — `SELECT p.id, (p.id IN (SELECT eid FROM e)) FROM p`
0AF00s ("Cascades planner could not plan query") with NO aggregate involved, and WHERE-position
`… WHERE p.id IN (SELECT COUNT(*) FROM e)` 0AF00s too. So IN-subquery is a general unsupported feature,
NOT a scope-leak residual — the scope leak is closed (the IN case went from a misleading 42803
to this honest 0AF00). **Correction (measured against Java 4.12.11.0 source):** the earlier
"(Java supports it)" parenthetical here was WRONG — Java rejects the same grammar alternative.
`ExpressionVisitor.visitInPredicate` asserts `inList().queryExpressionBody() == null` with
`UNSUPPORTED_QUERY` ("IN predicate does not support nested SELECT"), and the earlier
`AstNormalizer.visitInPredicate` NPEs on it (`ParseHelpers.isConstant` dereferences a null
`ExpressionsContext`). So this is a SHARED gap, not a Java-parity gap: closing it would be a
net-new read-side extension (IN-subquery lowering to a semi-join), sizeable; NOT an RFC-173
blocker. Sentinel: `exists_aggregate_scope_leak_fdb_test.go` `in_subquery_scope_leak_closed` flips
when IN-subquery support lands (then assert the rows).

<details><summary>original keystone characterization (kept for the audit trail)</summary>

The parser's aggregate detection (select_parser, `sq.aggCols`/`countStar`) does NOT stop at subquery
boundaries: an aggregate inside a projected/nested subquery leaks into the ENCLOSING SELECT's aggregate set.
Two observable failures, ONE root cause:
  - **42803 on a projected EXISTS-of-aggregate**: `SELECT p.id, EXISTS (SELECT COUNT(*) FROM e WHERE
    e.eref = p.id) FROM p` → `42803: column "P.ID" must appear in the GROUP BY` — the outer `SELECT p.id …
    FROM p` is wrongly classified as an aggregate query. Confirmed independent (the UNCORRELATED form
    `SELECT p.id, EXISTS (SELECT COUNT(*) FROM e) FROM p` 42803s identically); a plain `EXISTS(SELECT 1 …)`
    projects fine. Java answers it (all-TRUE for the non-grouped-aggregate inner).
  - **misclassification that broke the EXISTS-over-aggregate fix** (codex P1 on `5d4ff3711`): `EXISTS(SELECT
    EXISTS(SELECT COUNT(*) FROM f) FROM e WHERE e.eref=p.id)` — the row-preserving middle SELECT gets a
    non-empty `sq.aggCols` from the nested COUNT(*), so any consumer trusting `sq.aggCols` (the cardinality
    fix's detector) misfires.
Fix: the aggregate-detection walk must scope to the CURRENT query (an EXISTS/scalar subquery is its own
query scope; its aggregates belong to IT). This is the sequencing PREREQUISITE for the EXISTS-over-aggregate
cardinality fix above. Query-engine/front-end → four-gate.

**CONFIRMED by reproduction 2026-07-10 (correcting an intermediate mis-call).** A salvage re-do of the
cardinality fix (with the codex P1#1 LIMIT/OFFSET guard + the P1#3 outer-only-predicate decline) was built
and tested against codex's exact shapes. Result: base cases + LIMIT-0 + mixed-predicate all PASS, but
`codex_nested_scope` (`EXISTS(SELECT EXISTS(SELECT COUNT(*) FROM f) FROM e WHERE e.eref=p.id)`) returned
`[1,2,3]` instead of `[1]` — the row-preserving middle IS misclassified as unconditionally-one-row. So
codex P1#2 is REAL (an intermediate hypothesis that it was a false positive — "checkCountStar/extractAggFunc
are structural so sq.aggCols can't leak" — was WRONG; something DOES populate the middle's aggregate set from
the nested COUNT(*), same mechanism as the projected-42803). The cardinality fix therefore CANNOT be made
sound by guarding its own detector — it is HARD-BLOCKED on this scope-leak fix. Do the scope leak FIRST
(pin down which harvest in `extractFromQueryTerm`/the SELECT-element loop descends into a projected
EXISTS/scalar element), which also fixes the projected-42803, THEN the cardinality fix. The re-do was
discarded (not committed) since it fails `codex_nested_scope`.
</details>

### [ ] front-end follow-on (Torvalds correlated-EXISTS review, recommended, non-blocking): extract `classifyJoinOn(...)` from `buildCorrelatedExists`
`buildCorrelatedExists` (pkg/relational/core/embedded/logical_predicate.go) is ~412 lines — mostly essential
complexity (INNER/LEFT/RIGHT/FULL × correlation × ordering × CTE × nested-subquery), not cruft (Torvalds
ACK'd it). The one clean extraction: pull the per-join ON classification loop body (~70 lines: node-vs-lift-vs-decline)
into `classifyJoinOn(...) (nodeOn, lifted, err)` — the densest, most independent, most nameable sub-decision;
would drop the function to ~340 and make the loop legible. Recommended, not a should-fix.

### [x] query-engine follow-on (Torvalds ACK book): existential compensation-tail extraction — RESOLVED by RFC-190.7
The original four-copy census became stale when RFC-190.1 retired the N-way arm. RFC-190.7
centralized the three surviving belowFOD → FirstOrDefault(NULL) → optional IS[NOT]NULL chains in
`buildExistsCompensationChain`, with fresh-vs-preserved bookkeeping aliases as an explicit mode.
Caller-specific outer-leg rebasing and FlatMap construction remain local; the PK and secondary-index
fast routes share one yield path without erasing their distinct decline behavior.

### [x] query-engine: a DERIVED-TABLE inner in a correlated EXISTS loses its body → wrong rows — FIXED 2026-07-08 (resolve-body path)
A correlated EXISTS whose inner FROM is a DERIVED TABLE (`(SELECT …) AS d`) entered via the `buildCorrelatedExists` fallback's no-WHERE/no-ON fast path (entered only because the ignored inner SELECT references an outer column) returned silent-wrong rows: the fast path rebuilt each derived source as `NewScan("d")` — a scan of the non-existent catalog table `d`, which the executor treats as EMPTY → EXISTS tested the wrong (empty) relation. Confirmed at BASELINE `a34e9e21d` and reproduced fresh (`[[1 false],[2 false]]` where correct is `[[1 true],[2 true]]`, d={1} non-empty) — PRE-EXISTING master bug, not introduced by the JOIN-ON work.

DEFECT CLASS (both source positions, found in review): the silent-wrong hit BOTH the PRIMARY source (`correlatedInnerPrimarySource`) AND a comma/JOIN **LEG** (`correlatedSubqueryJoinRight` → `NewScan(j.tableName)`). @claude's correctness gate caught the leg twin: `… EXISTS (SELECT … FROM ord a, (SELECT …) AS d) …` scanned leg `d` as EMPTY → `ord × ∅` = ∅ → EXISTS wrong. FIX: a shared `buildDerivedInnerCarrier` helper builds the derived BODY via `buildLogicalPlanForQueryBodyWithCTECatalog` and wraps it in the `LogicalCTE(alias)` carrier (mirroring `buildOuterPlanOnDerived` and the CTE-aware resolution); BOTH the primary and every leg route through it, so the inner FROM carries the subplan. A body that can't be planned declines loudly. Its SQLSTATE is FAITHFUL to the inner query and POSITION-INDEPENDENT: an undefined column in the derived body surfaces 42703 in BOTH the projected (`SELECT …, EXISTS`) and the WHERE (`WHERE EXISTS`) position (codex P2) — `buildDerivedInnerCarrier` returns the wrapped `*api.Error` UNWRAPPED so `mapPredicateWalkError` doesn't rewrite the WHERE-position code to 0A000 (it matches `CorrelatedExistsError`→0A000 before the raw api.Error); a body that returns no plan with no structured cause → 0A000. The EXISTS WHERE/ON path and the correlated-SCALAR path (`buildCorrelatedScalar`) build their inner SCOPE first (`ResolveTable`), so a derived source there declines loud (0A000) before any operator build — correct-or-conservative, no wrong rows; pinned so they never regress to a silent bare scan. Regressions: derived_inner_correlated_exists_fdb_test.go (17 bipolar-discriminating EXISTS subtests incl. real/derived-primary × derived-LEG poles + the helper's own undefined-column loud-decline branch pinned across projected, WHERE-primary, and WHERE-leg positions) and scalar_derived_source_decline_fdb_test.go (scalar derived-primary + derived-leg 0A000 pins). NOTE for future red-first on this repo: in-place `git checkout` of a source file + re-run bazel test is UNRELIABLE (action cache serves the stale library) — EDIT the library content (forces a content-hash rebuild), use a FRESH test file, or `bazelisk clean` to verify red-first.

### [x] query-engine: CORRELATED EXISTS with an explicit `JOIN..ON` inner DROPS the inner ON — FIXED 2026-07-08 (codex-surfaced on the N-way review; root-caused + fixed in the front-end; 7 codex rounds folded: INNER-drop, OUTER-fold, mixed-orderings ×2, nested-subquery, shadowing+SQLSTATE, CTE fast-path)
ROOT CAUSE (the filed framing was WRONG — it blamed the Cascades 2-leg fold arm; the real defect is UPSTREAM
in the SQL→logical front-end): `buildCorrelatedExists` (pkg/relational/core/embedded/logical_predicate.go)
rebuilt the inner FROM's join tree with `NewJoinWithPredicate(op, right, kind, nil)` — the ON hardcoded to
nil — and its no-WHERE early return dropped it entirely. So `EXISTS(SELECT 1 FROM e JOIN f ON f.fid=e.fid
WHERE e.eid=p.id)` produced a bare cross-product → EXISTS silently true over an empty inner join. This is why
the three Cascades-layer guard attempts all mis-scoped — wrong layer; the ON was gone before Cascades ran.
The scalar-subquery sibling `buildCorrelatedScalar` already walked the ON correctly; the EXISTS fallback was
never given the same treatment. FIX: guard the no-WHERE early return on `!anyOn`, then walk each `j.onExpr`
and AND it into the inner predicate stream so the existing qualify/splitOuterOnlyConjuncts routing enforces
an inner-inner ON below the FOD and lifts an ON-embedded correlation — same as a comma-join WHERE conjunct.
Broader than filed: also single-outer projected, correlation-in-ON-with-no-WHERE, and NOT-EXISTS variants.
Regression: correlated_exists_join_on_fdb_test.go (5 bipolar-discriminating subtests, red-first).
(RESOLVED — the arm-state note below is kept for the audit trail; the observable bug is fixed and pinned,
see the [x] entry above and the 2-leg-fold-arm bullet.) A correlated projected/WHERE EXISTS whose inner is
an **explicit inner join** — `EXISTS (SELECT 1 FROM e JOIN f ON f.fid=e.fid WHERE e.eid=p.id)` — USED TO
return EXISTS=true even when the inner join `e JOIN f` was EMPTY: the inner join's own **ON** predicate was
dropped through the correlation lift. Repro (Java 4.12.11.0 = `[[10 false]]`, Go returned `[[10 true]]`):
`SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.fid=e.fid WHERE e.eid=p.id) FROM p, q WHERE q.qid=p.id`
with `e(eid=1,fid=99), f(fid=88)` (no inner match). Now `[[10 false]]` on s4.
- **Historical N-way-arm state before RFC-190.1.** At this checkpoint,
  `implementNWayJoinWithExistential` fail-closed conservatively on any non-scan existential inner
  (`existInnerIsScanSafe`), declining both the buggy explicit-join case and the then-working
  multi-table comma-join case. RFC-190.1 later replaced the N-way route with guarded
  PartitionSelectRule decomposition and retired this arm; the old reach-gap statement is not a
  description of current code.
- **The 2-leg fold arm** (`implementJoinWithExistential`): the "PRE-EXISTING silent-wrong, NOT yet fixed,
  three guard attempts mis-scoped, needs deep RFC-141 work" framing was RESOLVED and is now STALE. The
  proper fix — ENFORCE the inner ON under correlation — landed FRONT-END at `de5354139` (in
  `buildCorrelatedExists`, "broader than filed", follow-up `3bf658c89`), NOT at the executor fold arm. The
  three fold-arm guard attempts mis-scoped because they were guarding the WRONG layer: a CORRELATED
  inner-JOIN EXISTS routes name-model (`buildCorrelatedExists`) by the permanent-wall ruling (correlatedStep1
  binds by name → it NEVER folds to `implementJoinWithExistential`), so the fold arm never sees a
  dropped-ON correlated case — the latent concern is UNREACHABLE, not unfixed. Empirically re-verified on
  s4 across 8 shapes (2-outer/single-outer × projected/WHERE, correlation-in-ON, NOT-EXISTS, 3-way inner):
  all correct. Comprehensively pinned incl. the LEFT-JOIN-ON dual in
  `TestFDB_CorrelatedExistsJoinOnEnforced` (correlated_exists_join_on_fdb_test.go). No live silent-wrong.

`GROUP BY <qualified-col> + <expr>` returns NULL for the key on ANY multi-table query:
`SELECT WSRC.SID + 1, COUNT(*) FROM WSRC, WAUX GROUP BY WSRC.SID + 1` → `K=<nil>` ("unresolved
reference WSRC.SID + 1"). Reproduces with NO unnest/gather/shadow, and identically on the pre-RFC-173
name-read wrap. A BARE qualified group key (`GROUP BY WAUX.WV`) resolves fine — only the COMPUTED
form breaks. Root cause: resolving a computed group key's qualified inner column reference against the
aggregate input. General pre-existing planner gap, NOT an RFC-173 residual (see
rfcs/173-ordinal-column-resolution.md §NOT-OUR-BUG #2). Sibling of the derived-table+GROUP-BY 42703
gap (§NOT-OUR-BUG). Fix: trace the computed-group-key operand resolution over the qualified reference.

### [ ] fdbgo/client: deferred-error vs cancelled precedence differs from libfdb_c on a BOTH-poisoned-AND-cancelled txn

C++ checks `deferredError` at every ThreadSafeTransaction op lambda (get :431, watch :654, commit
:669, …) BEFORE the underlying actor observes `resetPromise` — so a transaction that is both
poisoned (deferred 2000/2018) and Cancel()ed surfaces the deferred code from every op. Go's
uniform entry order is cancelled-first (checkCancelled → deferredErr → checkTimeout, in
ensureReadVersion/Commit/WatchSetup), so the same txn surfaces 1025. Observable ONLY on that
double-terminal corner; deferred-beats-timeout and deferred-beats-1034 are already C++-aligned
and pinned. Resolution needs a differential probe (poison, Cancel, Get on both clients — mind
that MultiVersionTransaction may reorder) and, if confirmed, a single swap of the two gates at
each entry point in one FDB-C-dev cycle.

### [ ] fdbgo/client: GRV reply's ProxyTagThrottledDuration is discarded — GetTagThrottledDuration undercounts vs libfdb_c

C++ accumulates the GRV reply's `proxyTagThrottledDuration` into the transaction state
(`NativeAPI.actor.cpp:7410`) in addition to the client-side per-1223-error constant add
(`:7761`). Go implements only the constant add (`nextBackoff`,
`proxyMaxTagThrottleDuration`); both GRV parsers (`grv.go` sendGRVRequest callers) discard
the parsed reply field, so `GetTagThrottledDuration` under-reports whenever the proxy
throttles the GRV itself. Surfaced by the FDB-C-dev review of PR #452 (RFC-175 C2), where
the field's comment falsely claimed reply-side accumulation — comment fixed there; the
wiring (batcher → per-waiter accumulation, mind GRV batching fan-out semantics) needs its
own small FDB-C-dev cycle with a differential pin against libfdb_c.

### [ ] fdbgo/client: rywDisabled is a plain bool read lock-free on every read-path gate — same reset-boundary class as timeoutNs/deadlineNs

Written by SetReadYourWritesDisable and applyOptionDefaults (both reset paths); read lock-free by
Get/getRangeDir/GetPipelined/WatchSetup (the 1034 gate) and Commit's ship-decision. The sanctioned
Reset-cancels-watches overlap lets a cancelled watch's teardown read it concurrently with reset()'s
re-application — the exact class fixed for timeoutNs/deadlineNs (atomics) on the reset boundary.
Same treatment (atomic.Bool or inclusion in a guarded options snapshot) in a small FDB-C-dev cycle;
surfaced by the PR #449 gauntlet as a fast-follow candidate.

### [x] planner: `LIMIT 0` returned ALL rows unless the inner was a bare table scan (Go-only LIMIT extension) — FIXED 2026-06-28

`SELECT id FROM t LIMIT 0` (bare scan) returned 0 rows, but `LIMIT 0` over any non-bare inner (WHERE / ORDER BY /
index) returned EVERY row. **Root cause:** `ZeroLimitRule` rewrote `Limit(0, X)` to
`NewFullUnorderedScanExpression(nil, UnknownType)`, believing nil record-types meant an empty source — but nil means
"scan ALL record types", i.e. a full table scan. The broken full-scan alternative won on cost over the correct
`Limit(0, …)` whenever the inner was more than a bare scan (the bare case kept `Limit(0, Scan)`). **Fix:** deleted
the broken Go-only `ZeroLimitRule` (Java has no LIMIT, so no reference). `LIMIT 0` now always lowers to
`RecordQueryLimitPlan(0)` via `ImplementLimitRule`, which the executor's limitEnvelopeCursor short-circuits to 0
rows. Regression: `limit_zero_fdb_test.go` (bare / WHERE / ORDER BY / index / aggregate / OFFSET shapes). The
pre-existing `TestFDB_LimitZeroReturnsNothing` only covered the bare case — a dimensional gap.

### [x] executor/types: cross-type numeric SARG on an INDEXED column — CLOSED (all four categories, 2026-07-26)

**FIXED (2026-06-28):** int-literal vs DOUBLE-indexed column (widenIntConstAgainstDouble), all six ops + IN.

**FIXED (2026-07-18, RFC-181 WS-T, previously mis-tracked as still-broken here):** the narrowing direction —
DOUBLE/FLOAT literal vs INT/LONG column. `narrowFloatConstAgainstInt` rewrites a non-integral bound to the
tightest integer predicate (`col > 1.5` ≡ `col >= 2`, sides mirrored when the constant is on the left,
out-of-int64-range bounds clamp to always-true/false). Pinned by `comparison_promotion_gate.yaml`. This TODO
entry said "STILL BROKEN" for eight weeks after it was actually fixed — the entry just never got updated in
place; a reminder that a stale gate note outlives the code it describes if nobody deletes it.

**FIXED (2026-07-26):** the two remaining categories, closing this item out:

1. **Indexed FLOAT (32-bit) columns** — SEVERE, zero rows for every comparison. `expr.narrowConstAgainstFloatColumn`
   (`pkg/relational/core/query/expr/expr.go`) is the FLOAT-column sibling of the two functions above, covering
   BOTH directions at once (int/long/double constant vs FLOAT column) because float32's 24-bit mantissa makes
   exactness the interesting question regardless of which side started narrower: exact → retype the bare
   constant to a genuine Go `float32` (same `Typ: colt` pattern as the other two); non-exact ordered bound →
   rewrite to the tightest float32 predicate via `float32Ceil`/`float32Floor`
   (`pkg/relational/core/query/expr/float32_bounds.go` — a "sortable float" bit-transform giving correct
   next-representable-value semantics, including across the zero crossing and float32 overflow to ±Inf);
   non-exact equality → left unpromoted (mismatched tuple type codes make the SARG naturally empty, which
   coincides with the true answer, same reasoning the int-narrowing fix already relied on). `ResolveIn` got the
   matching per-element treatment. The executor's tuple-packing boundary needed a companion fix
   (`coerceTupleElement` in `pkg/recordlayer/query/executor/executor.go`, replacing the narrower
   `uuidToTupleElement`): the row-domain convention deliberately keeps a FLOAT-typed value as float64 everywhere
   else (`tupleElementToRowValue`'s doc comment), but the FDB tuple encoder dispatches on the Go RUNTIME type
   (float32 → tuple code 0x20, float64 → 0x21), so a comparand whose declared type says FLOAT must be downcast
   to a genuine `float32` at the exact point it gets packed into the scan range — the mirror image of
   `tupleElementToRowValue`'s float32→float64 upcast on the read side.
2. **Column-vs-column cross-type joins** (`a.xbig BIGINT = bd.ydbl DOUBLE` / `... = bf.yflt FLOAT` via an index
   on the DOUBLE/FLOAT side) — previously empty. Neither operand is a compile-time constant here, so there is
   nothing to retype in place; `expr.promoteColumnColumnNumeric` instead wraps the narrower-typed operand in a
   `values.PromoteValue` toward `values.MaximumType`, mirroring Java's `RelOpValue.encapsulate`
   (`PromoteValue.inject` on both sides of a `RelOpValue`). `values.PromoteValue.Evaluate`'s numeric coercion
   already worked correctly by this point (the 2026-06-28 "EXPERIMENT FINDING" below, that Promote-wrapping a
   CONSTANT gets unwrapped/ignored by the matcher, turned out to be irrelevant here — that finding was about
   retyping a literal, which the sibling constant-side functions already handle without Promote; wrapping a
   genuine non-constant FieldValue/CorrelatedFieldValue works because there is no bare value to extract instead).
   The same `coerceTupleElement` packer fix from (1) makes the FLOAT variant of this case work at the wire
   boundary too.

**FIXED (2026-07-27) — a FIFTH category this item claimed did not exist.** The header above said "all four
categories"; a fifth was open the whole time, and the reason nobody saw it is that FLOAT and DOUBLE were not
actually distinguishable in the plan. `primitiveTypeToValueType` (`expr/walk.go`) mapped BOTH `AS FLOAT` and
`AS DOUBLE` onto `values.TypeFloat`, which is an alias for `NullableDouble` — a name that says FLOAT and IS
DOUBLE. So `CAST(x AS FLOAT)` built a DOUBLE-target cast, and `CastValue`'s FLOAT arm (shared with DOUBLE in
both implementations, `values.CastValue` and `functions.CastValue`) never rounded, never rejected NaN/Infinite
and never range-checked — all three of which Java's `DOUBLE_TO_FLOAT` does (`CastValue.java:166-175`), with
`STRING_TO_FLOAT` likewise parsing at binary32.

The user-visible result was wrong rows in BOTH directions from a single cast: `d = CAST(0.1 AS FLOAT)` MATCHED
a binary64 0.1 that the binary32 value is not equal to, while `f = CAST(0.1 AS FLOAT)` MISSED the binary32 row
that it IS equal to. Splitting the walker targets and the CAST arms then exposed the genuine fifth SARG
category — a FLOAT-typed constant probing a DOUBLE column packs tuple code 0x20 against 0x21 entries, so
`d = CAST(1.5 AS FLOAT)` returned nothing and `d > CAST(1.0 AS FLOAT)` returned every row (a 0x20 bound sorts
below every double). Closed by extending the constant-widening rule to FLOAT constants and renaming it
`widenConstAgainstDoubleColumn`, since it is no longer int-only.

Pinned by `cross_width_float_sarg_probe_test.go` (21 shapes, with EXPLAIN assertions so a silent fall back to a
residual filter cannot hide a SARG regression) and `cast_float_test.go` (the `functions.CastValue` copy is
reachable only from the system-table map-path evaluator, so it needs a direct unit test — a query-routed test
exercises the other implementation and passes regardless). All four fixes mutation-verified independently.

**The dimension that was unprobed:** every existing float test used values like `1.5`, exact in both binary32
and binary64, which cannot distinguish a cast that rounds from one that does nothing. `0.1` is the discriminator
and nothing used it. Two tests actively pinned the bug — `TestWalkExpression_CastFloat` asserted the SAME target
for `AS FLOAT` and `AS DOUBLE` (encoding the conflation as the expectation), and `TestCastValue/string_to_FLOAT`
asserted the unrounded value.

**A real second bug DFS'd in the same pass:** wrapping an `AggregateValue` operand in `PromoteValue` (e.g.
`HAVING SUM(int_col) > (SELECT AVG(v) FROM t)`, an inherent Long-vs-Double comparison since AVG is always
DOUBLE) broke `TestFDB_HavingSubqueryProbe` — `rewriteAggregateValuesInTree`
(`pkg/relational/core/embedded/logical_predicate.go`) is the bespoke tree-walk that replaces `AggregateValue`
nodes with typed FieldValue references into the aggregated row, and its type switch had no `*values.PromoteValue`
case (unlike `*values.CastValue`, which it already handled), so an aggregate wrapped one level down stayed
unrewritten and reached `AggregateValue.Evaluate` at row time via the residual filter path, which always errors
("aggregate function is not allowed here") since an aggregate has no per-row scalar semantics. Fixed by adding
the missing case, mirroring the existing `CastValue` one. Mutation-verified (reverting either half of this fix
turns the corresponding test red with the exact wrong-rows/error shown, restoring turns it green): see
`indexed_float_sarg_probe_test.go`, `cross_type_join_probe_test.go`, `TestFDB_HavingSubqueryProbe`.

**Fail-closed design note:** for ORDERED comparisons there is always a lossless tightest rewrite (ceil/floor in
the narrower type), so "fail closed to a residual filter" is never actually needed for those — CockroachDB's
own model was researched for comparison (`pkg/sql/opt/idxconstraint`, `pkg/sql/sem/tree/type_check.go`,
`pkg/sql/sem/tree/overload.go`) and turns out to be MORE conservative than what we ship: CRDB never tightens a
mixed-type bound at all, it just declines to build a span and falls back to a remaining filter whenever the
literal's resolved type differs from the indexed column's
(`pkg/sql/opt/idxconstraint/testdata/misc:197-210`, `a > 1.5` on an INT column → unconstrained span + remaining
filter, citing issue #4313). Java's model (comparand promotion to `MaximumType`, tightening the bound) is the
one we ported, per "wire compat is the hard line, Java is the reference" — only genuinely-lossy EQUALITY needs
the "leave unpromoted" fallback, and that fallback is safe for the structural reason above (cross-type tuple
bytes can never accidentally collide), not because of an explicit sargability gate.

Regression: `indexed_float_sarg_probe_test.go` (indexed FLOAT eq/range/IN/reversed/int-widen, plus the
mutation-sensitive 0.1-boundary cases proving the ceil/floor op-rewrite fires, not a naive round-and-keep-op),
`cross_type_join_probe_test.go` (BIGINT=DOUBLE, BIGINT=FLOAT, both directions, plus an ordered `>` join), both
now with `EXPLAIN`/`plan_contains`-style `IndexScan(...)` assertions proving the SARG actually fires rather than
silently degrading to a full scan. Full `just test` (all 56 top-level test targets) green; no plan-shape/golden
regression in explaindiff/memoinvariant/rowdiff/the sargability differential oracle.

Original history below, kept for the record (the "STILL BROKEN" / "EXPERIMENT FINDING" / "SEVERITY UPDATE"
framing describes the state as of 2026-06-28-through-07-18, superseded by the fixes above):

**FIXED (2026-06-28):** the common + severe direction — an INTEGER literal vs a DOUBLE indexed column, for both
comparison ops (`=,<>,<,<=,>,>=`, via `expr.ResolveComparison`→`widenIntConstAgainstDouble`) AND IN-lists (`d IN
(5,7)`, via `expr.ResolveIn`). The int constant(s) are widened to DOUBLE (`5`→`5.0`) when the other operand /
the LHS is a non-constant DOUBLE, so the SARG packs the right tuple type while the indexed column stays bare (index
still matched — verified with an EXPLAIN IndexScan assertion). Regression: `crosstype_const_sarg_fdb_test.go`. Full
53-target suite green (no plan-shape/result regression); Graefe + Torvalds ACK.

`SELECT id FROM bd WHERE ydbl = 5` (ydbl DOUBLE, indexed) returns 0 rows instead of 1 (5 promotes to 5.0,
which equals the stored 5.0). Same for a cross-type index-probe join `a.xbig(BIGINT) = bd.ydbl(DOUBLE)` → empty
instead of matching 5=5.0. **Root cause:** the index SARG packs the comparand in its NATIVE type
(int64 `5`), which encodes differently from the column's DOUBLE tuple element, so the probe misses the entries.
The RESIDUAL (non-index) path is CORRECT — `xbig = 5.0` matches via `cmpAny`'s runtime numeric coercion — and an
explicit `CAST(a.xbig AS DOUBLE) = bd.ydbl` works. Only the index-SARG path is wrong.

**EXPERIMENT FINDING (2026-06-28, saves the next implementer a dead end):** I tried the "obvious" Java-aligned
approach — make `PromoteValue.Evaluate` coerce (via `promoteConstant`) and wrap the narrower int operand in
`PromoteValue(floatType)` at `expr.ResolveComparison` (general: const + col-col + narrowing). It does NOT work for
the index SARG: the `uses_index_range_scan` EXPLAIN assertion still passed (plan shape unchanged) BUT `d = 5`
regressed to `[]` — i.e. the data-access matcher does NOT route the comparand through `PromoteValue.Evaluate` when
packing the index range; it extracts/packs the underlying value, bypassing the coercion. So the int 5 was packed,
not 5.0. (Reverted.) Conclusion: the working const fix uses a BARE coerced `ConstantValue{Value:5.0}` precisely
because the matcher packs `ConstantValue.Evaluate()` directly — a Promote wrapper is transparent-to-the-matcher and
gets unwrapped/ignored for a CONSTANT. (This is why the eventual col-vs-col fix above wraps a non-constant
FieldValue in Promote instead of trying to retype it — there is no bare value to extract there, so the matcher's
generic `Operand.Evaluate()` call genuinely runs `PromoteValue.Evaluate`.)

**SEVERITY UPDATE (broader + worse than first thought):** the gap is not limited to equality missing rows. With a
DOUBLE indexed column `d ∈ {5.0,7.0,10.0}` and INT literal comparands:
- `d = 5` → `[]` (misses; should be `{5.0}`) — equality, as documented.
- `d IN (5,7)` → `[]` (misses; should be `{5.0,7.0}`) — IN-list has the same bug.
- `d > 6` → `{5.0,7.0,10.0}` — returns 5.0 which is NOT > 6 (**WRONG ROWS**, not just missing).
- `d < 8` → `[]` — returns nothing though 5.0,7.0 ARE < 8 (**WRONG ROWS**).
- `d BETWEEN 5 AND 8` → `{5.0,7.0}` (CORRECT — inconsistent with `>`/`<`; likely a residual re-check on the
  closed range that the open inequalities skip).
- All `*.0` double-literal comparands and the residual path are correct.
The inequality cases are the worst: INT and DOUBLE are different FDB tuple type-codes and all doubles sort after all
ints, so an int-bound range over a double index degenerates to all-or-nothing.

**Scope note (good news):** the INSERT/UPDATE *store* side is CORRECT and wire-safe — an int literal written to a
DOUBLE column is widened and stored as `5.0` (verified: a double-typed index probe finds it; `insert_type_coercion_probe_test.go`),
and narrowing double→BIGINT is conformantly rejected (22000, no double→long promote). So the bug was confined to the
COMPARISON/SARG comparand promotion, not record storage — the wire format of stored records is fine.

### [x] query-engine: correlated scalar-subquery cardinality is enforced consistently — CQ-4 (found 2026-06-28, completed 2026-07-24)

Go chose the SQL-standard contract for its scalar-subquery extension: a second
row for one outer row is SQLSTATE 21000, not an implicit first-row choice. The
single-source projection and WHERE paths now share
`translateSingleSourceCorrelatedScalarJoin`, which materializes
`[outer..., scalar]` with LEFT/null-on-empty semantics. An unpaged inner whose
cardinality depends on data — raw rows, group-key-only groups, or real-aggregate
groups after HAVING — carries `CorrelatedScalarSubquery.StrictSingle`;
`ImplementNestedLoopJoinRule` lowers that to a strict
`RecordQueryFirstOrDefaultPlan`, and `executeFirstOrDefault` probes one extra row
inside each driving FlatMap evaluation. There is no synthetic grouped `LIMIT 1`,
so ORDER BY alone never selects a first group and the strict probe observes the
post-WHERE/GROUP-BY/HAVING/ORDER-BY result.
StrictSingle is itself a routing contract. Its sole physical authority accepts
exactly the translator-owned `LEFT OUTER [unflagged outer, StrictSingle-only
right]` shape and emits only the compensated FlatMap; every other flagged join
kind, orientation, or composition fails closed. Even if simplification erases
the inner's final syntactic dependency on the outer row (for example,
`c.parent_id = p.id OR 1 = 1`), no competing unwrapped materialized nested-loop
plan is emitted.
StrictSingle is also an optimizer-wide rewrite barrier: N-way/binary
partitioning, predicate pushdown (including filter-below-join), select
merge/split, OR-to-union, outer-join rewriting, simple-select, and IN
implementations all treat a flagged edge as opaque unless they own an explicit
preservation proof.

The WHERE consumer binds the LEFT-scalar row to one fresh typed QOV, rebases the
comparison (including IS NULL) onto it, filters above both the null-extension and
strict-cardinality barriers, then projects the scalar's private final slot away.
Only scalar-free top-level AND conjuncts whose correlations are empty or the
single outer binding are installed on the outer leg first. OR/NOT stay atomic
above the box. This makes cardinality per *eligible* outer row: `p.id=2 AND
scalar(...)` does not evaluate a known-bad `p.id=1`, while a bad eligible row
still raises 21000 even if exactly one of its inner values would satisfy the
comparison. A dead carrier removed by predicate simplification is retired
without evaluating its inner; an orphan/mismatched carrier rejects typed-loud.

Explicit pagination is never replaced by a hidden cap. LIMIT 0/1 (with exact
OFFSET) is accepted for multi-row-capable inners and clears StrictSingle because
the post-page result is proven <=1. A non-grouped real aggregate is intrinsically
<=1, so larger LIMITs are also exact and accepted (OFFSET 1 over its one row
becomes NULL). LIMIT >1 over raw rows/groups and planning-time-unresolved
LIMIT/OFFSET reject 0AF00 until a post-pagination scalar-collapse mode exists;
otherwise the non-strict LEFT join could fan one outer row into several.

Correct-or-loud composition boundaries are explicit: SELECT DISTINCT, window/QUALIFY,
group-key-only HAVING, a WHERE scalar over a multi-source outer, mixed
EXISTS/uncorrelated-scalar + correlated-scalar WHERE, projected EXISTS over a
scalar-bearing WHERE, simultaneous SELECT and WHERE correlated scalars, and DML
correlated scalars reject 0AF00. Real-aggregate HAVING is supported and its
0/1/2+-surviving-group behavior is pinned. The shared carrier participates in
logical children/attached plans, Cascades subquery planning, unary rebuilds, and
cluster gates, so it cannot be orphaned by an early fold or rewrite.

Coverage: `correlated_scalar_cardinality_plan_test.go`,
`correlated_scalar_cardinality_followup_fdb_test.go`, the flipped
`correlated_scalar_where_reject_fdb_test.go` boundary,
`quality_probes_test.go`, and `scalar_subquery_java.yaml`. The existing
non-correlated 21000 probes remain controls. This is a Go-only read-side
extension; Java's SQL grammar does not expose correlated scalar expressions.

**Future widening (not a correctness gap):** implement a post-pagination scalar
collapse/probe so data-dependent LIMIT >1 can preserve the written page and then
apply scalar cardinality semantics, rather than declining 0AF00.

### [ ] driver: NO read-your-writes inside an explicit transaction — SELECT auto-commits (divergence, found 2026-06-28)

Inside `BeginTx`, DML (INSERT/UPDATE/DELETE) joins the explicit FDB transaction (`runInTx` → `activeTx.rctx`) and
is atomic on Commit / undone on Rollback — correct. But **SELECT runs in a FRESH auto-commit transaction**
(`DB.Run`), NOT the explicit tx (cascades_generator.go: "DML joins an open explicit transaction (runInTx); SELECT
runs in a fresh auto-commit transaction (DB.Run)"; only `respectActiveTx` = `IsUpdate()` routes through the tx).
Consequences, confirmed by `tx_select_isolation_probe_test.go`:
- **No read-your-writes:** a SELECT in the tx does NOT see the same tx's uncommitted DML (`UPDATE v=777` then
  `SELECT v` → 100; `INSERT id=2` then `SELECT WHERE id=2` → no rows).
- **No read-write serialization:** an in-tx SELECT adds no read-conflict range, so a read-modify-write across two
  explicit txns does not raise 1020/40001 (last-writer-wins).

**Divergence from Java:** Java's relational driver (`setAutoCommit(false)`) reads through the same FDB transaction
and so DOES provide read-your-writes + read-conflict detection. This is a deliberate Go simplification (the
executor opens its own record store; binding SELECT to the user write-tx would add read-conflict ranges — the same
"spurious not_committed" hazard `cachedLoadSchema` already dodges for catalog reads). Fixing it = route the query
executor's scan through `activeTx.rctx` when one is open AND solve the spurious-conflict problem (snapshot vs
serializable reads) — a Cascades/executor + driver-tx architecture change (Graefe). Until then it's a real
read-modify-write footgun: a txn that reads then writes the same row sees stale data. Behavior pinned (flip the
probe's `no_read_your_writes_in_explicit_tx` assertion when in-tx reads land).

### [x] DDL error classification — duplicate-column + PK-over-unknown-column now clean 42-class errors (2026-06-28)

Invalid DDL was already REJECTED (fail-closed) but two cases surfaced a leaky INTERNAL error (`XX000` + raw
proto/metadata-builder internals) instead of a clean 42-class user error. Both fixed in `parseTableDefinition`
(ddl.go), validated BEFORE the proto-descriptor / metadata build:
- duplicate column (`..., x BIGINT, x STRING, ...`) → clean **42701** (was `XX000: protodesc.NewFile: descriptor
  "T.X" already declared`).
- PRIMARY KEY over an undefined column (`... PRIMARY KEY (nope)`) → clean **42703** (was `XX000: build
  RecordMetaData: ... field "NOPE" not found in message "T"`).
Pinned by `ddl_errors_probe_test.go` (asserts the clean codes). Other DDL errors were already clean (42F04
db-exists, 42F63 db-missing, 42601 no-PK, 42F59 dup-template).

### [ ] identifiers: quoted DDL column is created but unreferenceable by name (case-model divergence, found 2026-06-28)

Unquoted identifiers work correctly (case-insensitive, folded to upper case — `MyCol` resolves as
MyCol/mycol/MYCOL; pinned by `identifier_case_probe_test.go`). But a column declared with a QUOTED identifier is
mishandled:
- `CREATE TABLE t (id BIGINT NOT NULL, "KeepCase" BIGINT, PRIMARY KEY (id))` succeeds; `INSERT INTO t (id,
  "KeepCase") VALUES (1, 20)` succeeds; `SELECT *` shows the column as `KEEPCASE`.
- But NO explicit reference resolves it: `SELECT keepcase` / `KEEPCASE` / `KeepCase` / `"KEEPCASE"` / `"KeepCase"`
  all → `42703 column does not exist`. The column is effectively write-and-`SELECT *`-only — unreferenceable by
  name in SELECT/WHERE.

Root cause: an identifier-normalization mismatch across DDL storage, SELECT-* expansion, and explicit-reference
resolution for quoted identifiers. Java has a consistent case-sensitivity model — `SemanticAnalyzer.normalizeString
(string, caseSensitive)` ("taken as-is if caseSensitive, upper-cased otherwise") with an `isCaseSensitive` flag set
per-identifier by quoting — so quoted identifiers round-trip (created and queried by the same quoted name). Fix =
port Java's normalizeString/isCaseSensitive model so quoting consistently selects case-sensitive handling in DDL +
resolution + star-expansion. Niche (mixed-case / reserved-word column names are uncommon) but a real divergence;
deferred (threads through the catalog + semantic analyzer).

**Empirical diagnosis (CORRECTS the earlier framing — verify the real symptom before fixing):**
- The `SELECT *` "shows KEEPCASE" observation is a RED HERRING — that is the JDBC result-set *display* name
  (`jdbcColumnName`, select_helpers.go:29, upper-cases unquoted-looking labels; Java does the same). It is NOT the
  stored column name and not the bug.
- DDL does NOT fold the quoted case: `DOUBLE_QUOTE_ID` (RelationalLexer.g4:1332 = `'"' ~'"'+ '"'`) keeps the quotes,
  so `parseTableDefinition` → `StripIdentifierQuotes(Uid().GetText())` (ddl.go:677) yields `"KeepCase"`→`KeepCase`
  (preserved), and `builder.go:485` writes the proto field as `col.name` = `KeepCase`. So the on-disk proto field
  IS `KeepCase` (case preserved) — and that should be wire-faithful, but VERIFY what Java's descriptor names it.
- The real bug is in RESOLUTION: a `Fields().ByName(...)` lookup never matches the preserved-case field. Smoking gun
  = INCONSISTENT normalization across the lookup sites: `ByName(parseColRef(name).bare())` (preserved) at
  cascades_generator.go:2552/2627/2889 vs `ByName(strings.ToLower(name))` at :3493 vs the upper-folding SELECT
  column-ref path. Empirically `SELECT "KeepCase"`, `KeepCase`, `KEEPCASE`, `keepcase`, AND `"KEEPCASE"` ALL →
  42703 — i.e. no reference form (not even the exact stored/displayed name) resolves, which rules out a simple
  one-sided case fold and points at the scope/column-registration step, not the reference normalizer alone.
- Next-session fix: unify ALL column-resolution lookups on ONE Java-faithful `normalizeString` (quoted→as-is,
  unquoted→upper) AND ensure the record-type→semantic-scope column registration keys columns by that same form;
  then `SELECT "KeepCase"` resolves and `SELECT KeepCase` (unquoted→KEEPCASE) correctly does not. Add the e2e probe
  + verify the proto field name matches Java (wire) before landing.

### [x] dml: wire DML DRY RUN through to the dry-run store primitives (Java parity) — DONE (RFC-158)

`<DML> ... OPTIONS (DRY RUN)` now PREVIEWS the would-be-affected rows without committing, matching
Java (AstNormalizer.visitQueryOptions → Options.DRY_RUN → ExecuteProperties.setDryRun → the DML plans
branch to dryRunSave/DeleteRecordAsync). Replaces the former fail-closed reject (the data-loss
stopgap). Threading is STATEMENT-scoped (Torvalds NAK on the v1 connection-options design — that
would have gone sticky / never-fired, resurrecting the data-loss bug): `dmlHasDryRunOption` →
`cascadesPlan.dryRun` → `paginatingRows.dryRun` → `ExecuteProperties.DryRun`, where
executeInsert/Update/Delete branch onto `DryRunSaveRecord`/`DryRunDeleteRecord`. Existence checks
still fire (INSERT of an existing PK under DRY RUN → 23505, parity). EXPLAIN renders the plan (never
executes). Pinned by `dml_dry_run_fdb_test.go` (11 subtests incl. the no-sticky data-loss sentinel +
BeginTx). Graefe + Torvalds ACK (RFC).

### [x] dml: DryRunSaveRecord secondary-UNIQUE / intra-statement-PK preview scope — RESOLVED (matches Java, NOT a bug)

Graefe (RFC-158 review) + codex flagged that `DryRunSaveRecord` previews success for an INSERT that
the real path rejects on a secondary UNIQUE index, and (codex) for an intra-statement duplicate PK.
**Reading Java settled it as Java-faithful, not a divergence:** `FDBRecordStore.saveTypedRecord(isDryRun
=true)` EARLY-RETURNS at FDBRecordStore.java:578 — BEFORE `serializeAndSaveRecord` (staging) and
`updateSecondaryIndexes` (line 594). So Java's dry-run also validates only the PK existence check
against pre-statement state and skips secondary-index validation + intra-statement staging. Go matches
Java exactly. Adding secondary-index validation would make Go STRICTER than Java — a conformance
divergence (Go rejecting a DRY RUN that Java previews as success), forbidden by the conformance
principle. Pinned Java-faithful by `dml_dry_run_fdb_test.go::TestFDB_DmlDryRun_MatchesJavaLightweightValidation`
+ documented at `DryRunSaveRecord` (store_api.go). No action — do NOT "fix" it into a divergence.

### [ ] dml: INSERT ... SELECT arity mismatch leaks internal XX000 instead of a clean error (wrong-error, LOW-MED, found 2026-07-20 hunt)

An `INSERT ... SELECT` whose projection column count differs from the target
record's is NOT validated at plan time: `alignInsertSelectColumns`
(logical_predicate.go) and `checkInsertSelectPromotable` both loop over
`min(len(projections), len(target))` and silently truncate, so the mismatch
surfaces only at row-read as an internal XX000 ("result row carries no positional
output row aligned to column … — the plan's top operator did not emit an ordinal
output row", resultset.go:147). Repro (dst has 2 cols): `INSERT INTO dst SELECT
id, v, id FROM src` (too many) or `INSERT INTO dst SELECT id FROM src` (too few)
→ XX000. The VALUES path rejects the same mistake cleanly (42601/22000,
insert_cascades.go:82). No data corruption (nothing partial-inserts; too-few does
NOT NULL-fill — it also errors). Wrong-ERROR only.
FIX IS NOT A PLAN-TIME COUNT CHECK (attempted + reverted 2026-07-20): the output
WIDTH ≠ `len(proj.Projections)`. A RECORD/struct projection expands into several
target columns — e.g. `INSERT INTO DST SELECT "V" FROM T1, T1."ARR" AS "V", W`
projects ONE unnested element `V` that fills DST's 2 columns (pinned by
`TestFDB_ArrayUnnestDMLDuplicateAlias` control). Worse, that unnested element's
`Value.Type()` is NOT a `*values.RecordType`, so a type-based "all-scalar" guard
also false-positives. A correct check needs the source plan's DERIVED OUTPUT
SCHEMA (post-expansion column count), not the raw projection list — a real
semantic-analysis addition. Until then the XX000 stands (low severity). Do NOT
re-add a `len(proj.Projections)` check — it 42601s valid unnest/record-expansion
INSERT…SELECTs.

### [ ] dml: DELETE/UPDATE ... RETURNING silently ignored — Java supports it (divergence, found 2026-06-28)

The shared grammar carries `(RETURNING selectElements)?` on `deleteStatement` and
`updateStatement`, and **Java supports it** — `QueryVisitor.visitDeleteStatement:848` /
`visitUpdateStatement:882` build a `generateSelect` from the RETURNING selectElements
and return the affected rows as a result set. Go silently DROPS the clause: via `Query`
you hit the generic DML-via-Query guard (0A000 "INSERT/UPDATE/DELETE return a row
count, not rows"; connection.go:449) before RETURNING is ever processed; via `Exec` the
DELETE/UPDATE executes correctly but the RETURNING values never surface (count only).

NOT data loss (the DML is correct) — a Java-supported feature left unimplemented.
Fix = port Java's generateSelect-from-RETURNING (build the projection over the
deleted/updated rows) and wire a DML-returning-a-result-set through the driver Query
path (the path that currently rejects all DML with 0A000). Feature port, follow-up
scope. Pinned by returning_clause_probe_test.go (flip when implemented). INSERT
RETURNING is a 42601 — not in the INSERT grammar — so it's a separate, larger gap.

**Scope note (RFC-159 investigation):** this is a Graefe-gated **Cascades** change, not a small
fix. Java models RETURNING as a `generateSelect` (a logical SELECT / projection) wrapping the
mutation operator's output — so Go needs a `Project`-over-DML the Cascades planner can plan
(`Map`/`Project` over `RecordQueryDelete/UpdatePlan`), plus driver routing to send a DML-with-RETURNING
through the Query (rows) path rather than Exec (count). The Go DML executor already returns the
mutated rows as a cursor (`recordlayer.FromList(results)`), so the executor groundwork exists; the
work is the logical Project-over-DML + its physical wrapper + `IsUpdate()` routing. Its own RFC +
Graefe ACK.

### [x] ddl: in-template index/column errors wrap to 42F59, burying the specific SQLSTATE — DONE (RFC-161)

`createSchemaTemplate` (ddl.go) now PROPAGATES a structured `*api.Error` from
parseTableDefinition/parseIndexDefinition (42701 duplicate column, 42703 PK over unknown column,
0A000 unsupported INCLUDE, …) as its own SQLSTATE instead of masking it under the generic 42F59
(ErrCodeInvalidSchemaTemplate — the wrong code for a duplicate column). Confirmed real vs Java: its
`DdlVisitor` does not wrap in-template errors; `ExceptionUtil` maps each per type. A non-structured
parse error still wraps. Duplicate-template-NAME 42F59 (different path) unchanged. Pinned by
`include_clause_rejected_probe_test.go` (now 0A000, not 42F59) + `ddl_errors_probe_test.go` (42701/42703
now the outer code). Per-type Java code may still drift (acceptable DDL drift); the specific code is
strictly more correct than the false 42F59 wrapper.

Every error raised while parsing an index/column inside a `CREATE SCHEMA TEMPLATE` is
re-wrapped to outer SQLSTATE **42F59** with the specific code embedded in the message,
e.g. `42F59: index: 0A000: index "T_A": INCLUDE clause ...` (ddl.go:~145 wraps via
`%v`). So a `database/sql` caller doing SQLSTATE extraction sees 42F59, not the real
cause (0A000 / 42703 / 0A000-only-primitive / etc.). Pre-existing and shared by ALL
in-template index/column DDL errors (incl. the vector-INCLUDE and TEXT-type rejections),
so tests in this area assert the specific code via substring on the embedded text
(see include_clause_rejected_probe_test.go, which now pins BOTH 42F59 and the embedded
0A000). Verify against Java: does Java surface the specific ErrorCode for in-template
DDL failures, or also wrap? If Java surfaces the specific code, stop the 42F59 re-wrap
(propagate the inner SQLSTATE) so cross-engine SQLSTATE matching holds for DDL errors.

### [ ] ddl: implement covering indexes — CREATE INDEX ... INCLUDE (cols) (Java parity; found 2026-06-28)

`CREATE INDEX ... ON t (a) INCLUDE (b)` is currently REJECTED (0A000 "INCLUDE clause
(covering index) is not yet supported", ddl.go parseIndexDefinition) — a fail-closed
stopgap for what was a SILENT divergence: Go dropped the INCLUDE clause and created a
PLAIN index, while Java (DdlVisitor.java:249 → addValueColumn) creates a COVERING
(KeyWithValue) index. Same CREATE INDEX, different index structure across engines = a
wire/DDL-portability divergence. Regression: include_clause_rejected_probe_test.go.

Go's record layer ALREADY supports covering indexes — KeyWithValueExpression
(index_maintainer.go:107/217/362, "Matches Java's KeyWithValueExpression path"). The
gap is only the SQL→metadata DDL wiring: (1) Builder.AddIndex (core/metadata/builder.go)
needs an included-columns parameter; (2) build a KeyWithValueExpression root (key cols +
value cols) instead of a plain key expression when INCLUDE is present; (3) wire
def.IncludeClause().UidList() through parseIndexDefinition (ddl.go). Flip the reject +
the sentinel when implemented. Same applies to the indexAsSelect / vector paths' INCLUDE.

### [x] metadata: UUID columns are indexable end-to-end — RFC-162 DONE (item 1021, PR #397)

**DONE (PR #397, Graefe+Torvalds ACK'd).** UUID is now a first-class indexable primitive, write
side AND read side. A UUID flows through the Cascades value layer as a neutral `[16]byte` (decision
(b)); `tuple.UUID` only at the FDB wire boundary, canonical string only at the driver boundary.
`CREATE INDEX` + `WHERE v = '<uuid>'` (all comparison ops incl. IS [NOT] DISTINCT FROM), IN, ranges,
covering `SELECT v`, UUID PK, INL join on a UUID key, ORDER BY / DISTINCT / GROUP BY / merge-sort all
work; MIN/MAX over UUID is rejected identically to Java. Pinned by
`uuid_indexable_roundtrip_fdb_test.go` + friends. Historical design notes below.

`CREATE INDEX ... ON t (uuid_col)` fails with a leaky internal error: `XX000: build
RecordMetaData: ... index "T_V" validation failed: field "V" in "T" is a message
type; use Nest() to navigate into nested messages`. All other column types index fine
(TIMESTAMP, DATE, FLOAT, INTEGER, BOOLEAN, BIGINT, DOUBLE, STRING, BYTES — pinned in
indexable_types_probe_test.go). Fail-CLOSED (CREATE fails, no corruption).

Root cause: Go stores a UUID column as the `tuple_fields.UUID` proto MESSAGE
(cascades_generator.go:2978), and the record-layer index-maintainer validation
rejects message-typed index fields. Likely a Go DIVERGENCE, not a shared limit: Java
treats UUID as a first-class indexable PRIMITIVE — `DataType.Primitives.UUID` /
`Type.uuidType()` (SemanticAnalyzer.java:724, DataTypeUtils.java:152) — so a UUID
index works in Java even though storage is the same `tuple_fields.UUID` message. Fix
= teach the index path to treat the tuple_fields.UUID message as an indexable
primitive (it has a natural tuple encoding/ordering), matching Java; at minimum
replace the leaky XX000 with a clean user-facing SQLSTATE. Needs a record-layer /
metadata change + Java-alignment; sentinel pins the current XX000 (flip when fixed).

**DONE — RFC-162 (Graefe design + impl ACK'd; PR #397).** Sites 1-2 LANDED: the validator accepts the
tuple_fields.UUID message (`isTupleField`) and the maintainer writes the entry as a `tuple.UUID`
(`scalarToInterface`→`uuidMessageToTuple`), byte-identical to Java — pinned by
`recordlayer/uuid_key_encoding_test.go` (unit, wire format) + `indexable_types_probe` (CREATE INDEX +
INSERT succeed). REMAINING = the read side. Mapped to exact sites — it is ONE ATOMIC change (each piece alone regresses
something; UUID must flow as `tuple.UUID` end-to-end, string only at the driver boundary, per Graefe):
- **A — field read.** `query_result.go protoFieldToGo` (~:107) currently renders a UUID field via
  `uuidMessageToString`; change it to return the 16-byte `tuple.UUID` (reuse `uuidMessageToTuple`'s msb‖lsb
  layout) so the FILTER path compares `tuple.UUID == tuple.UUID`.
- **B — comparand. THE CRUX (substantial; Graefe impl review).** Two parts, both needed:
  - **B1 (insertion):** wrap the comparand in `PromoteValue(literal, NotNullUuid)` where a UUID column
    meets a STRING literal. NOTE: Go currently inserts NO comparison promotes at all — numeric promotion
    (`int_col = 5.0`) is handled by `cmpAny` at runtime, and `PromoteValue` is only ever built by
    tree-rebuild ops (map_field_values/replace/simplifier), never originally inserted for a comparison.
    So B1 is a NEW pattern in the predicate builder (the comparand evaluated as STRING-typed today — that
    is why the index probe packs a tuple string). `IsPromotable(String,Uuid)` already returns true
    (promotionMap, type.go:896), so the lattice agrees; the gap is the missing insertion.
  - **B2 (eval arm):** `PromoteValue.Evaluate` (values.go:2380) is a NO-OP today (delegates to Child).
    Give it the single String→UUID arm Graefe approved: when `Target` IsUuid and the child evaluates to a
    string, `uuid.Parse` → 16-byte `tuple.UUID`. With B1+B2 the index/PK probe, range/IN, and INL key all
    pack `tuple.UUID` from the value's type (no out-of-band masks).
- **C — materialization.** `cascades_generator.go paginatingRows` row build (~:1618) converts a
  `tuple.UUID` column value → canonical string at the driver boundary (the ONE place string appears),
  also covering the covering-index `tuple.UUID` from `IndexEntryObjectValue` — which stays a pure ordinal
  extractor (Graefe).
A without C regresses every `SELECT v`; B without A regresses the working full-scan `WHERE v=`. Land
together + Graefe IMPL review. Then flip the `indexable_types_probe` transitional sentinel to the full
round-trip + add the INL-join-key + MIN/MAX-ever + UUID-PK regressions. Until then, a UUID index must NOT
be queried by `WHERE v='…'`.

**ARCHITECTURE RESOLVED — Graefe DECISION: (b).** A UUID flows as a neutral `[16]byte` inside the
Cascades value layer (`values` stays wire-agnostic — NO `tuple` import there). The `tuple.UUID`
conversion lives ONLY at the wire boundaries that already import `tuple`: the scan-range packing
(`scanComparisonsToTupleRange`, executor) and the index-entry write (`scalarToInterface`, recordlayer —
already landed). Rationale: matches the `IndexEntryObjectValue` no-wire-coupling precedent + Java's
neutral `java.util.UUID`; `[16]byte` and `tuple.UUID` compare identically (unsigned big-endian) in
`cmpAny`, so zero semantic cost. CONCRETE TYPES this fixes:
- A: `protoFieldToGo` UUID field → `[16]byte` (parse from the msb/lsb message; reuse the msb‖lsb layout).
- B2: `PromoteValue.Evaluate` (values.go:2380) String→`[16]byte` via `uuid.Parse` (google/uuid is a
  NEUTRAL lib — allowed in values; only `fdbgo/fdb/tuple` is the banned wire dep).
- Executor scan-range packing: when the equality comparand evaluates to a `[16]byte`, append
  `tuple.UUID(that)` to the probe tuple (the [16]byte→tuple.UUID conversion at the wire boundary).
- C: `paginatingRows` materialization (cascades_generator.go ~:1618) `[16]byte` → canonical string.
- `cmpAny`: ensure `[16]byte == [16]byte` (filter path) — likely already byte-compares; verify.
Original design notes below.

**DESIGN READY — see RFC-162.** Prototyped end-to-end and PROVED the
approach (the index probe returns the right row), but it spans **6 sites across 3 packages** (incl. a
sensitive Cascades `values` file) and touches the **wire format** — so it needs a Graefe design review
first, not a rushed multi-site landing. WIRE VERIFIED: `tuple.UUID = msb‖lsb big-endian` =
Java `TupleUtil.encode(UUID)` = `0x30 + msb(8B BE) + lsb(8B BE)` (`UUID_CODE=0x30`), consistent with
`query_result.go:uuidMessageToString`. The 6 sites (with exact code) + the RFC-083/PromoteValue
open question are in RFC-162. Partial landing is UNSAFE (enabling the index without every probe/
projection site makes `SELECT v WHERE v=…` / covering `SELECT v` silently wrong — worse than the
current clean XX000). Stub to implement first: `key_expression_validate.go:isTupleField` (currently
`return false`).

### [x] query-engine: nested derived tables drop ALIAS-introduced column names beyond one level — STALE, already resolved

RESOLVED (stale entry — the sentinel now pins the WORKING behavior). The failing
case `SELECT x FROM (SELECT x FROM (SELECT a AS x FROM t) i) s` now resolves `x`
through any nesting depth and returns [10 20 30]:
`nested_derived_table_probe_test.go`'s `two_level_inner_alias` asserts success
(no 42703) and passes green. Identifier resolution keeps the OUTPUT column name
verbatim (no source-name reverse-map), so an inner alias is not buried under the
source column beyond one level. The original diagnosis below is retained as the
record of the investigation; the reach gap it describes no longer exists.

Derived tables (subquery in FROM) are supported and cross-engine-tested (plandiff
corpus has `FROM (SELECT … ) AS t` entries). But an alias introduced in an INNER
derived table is not visible TWO levels up:
- works: `SELECT x FROM (SELECT a AS x FROM t) i` (1-level alias)
- works: `SELECT a FROM (SELECT a FROM (SELECT a FROM t) i) s` (2-level, NO alias —
  the real column name `a` propagates through any depth)
- FAILS: `SELECT x FROM (SELECT x FROM (SELECT a AS x FROM t) i) s` → `42703 column
  "X" does not exist`; likewise `… (SELECT x AS y FROM (SELECT a AS x FROM t) i) …`.
Only an alias-introduced name is dropped at depth ≥2. Fail-CLOSED (clean 42703, not
wrong rows). Standard SQL allows it and Java supports derived tables, so this is most
likely a Go column-anchoring gap, not a shared limitation — confirm against Java.
Root cause direction: the nested derived-body column derivation
(cascades_translator.go derivedOutputColumns / legColumns, RFC-077 7.6) returns the
alias name for a 1-level LogicalProject body but does not propagate it when that
body is itself a derived table wrapped in another (the middle Project re-projects
the alias column, but the outer level can't resolve it). Sentinel:
nested_derived_table_probe_test.go (pins 1-level + 2-level-no-alias work, 2-level-
inner-alias → 42703; flip when fixed). Needs query-engine review. WORKAROUND: the gap
is specific to INLINE derived tables — the structurally-identical CTE chain
(`WITH c1 AS (SELECT a AS x FROM t), c2 AS (SELECT x FROM c1) SELECT x FROM c2`)
propagates the alias correctly (translateCTE registers the body under the CTE name),
pinned in cte_alias_propagation_probe_test.go. So the fix likely is to give the inline
derived body the same named-anchoring treatment translateCTE uses.

### [x] dml: UPDATE/DELETE with a nonexistent WHERE-column or table give generic 0AF00 (vs SELECT/INSERT's cleaner 42703/42F01) — DONE (RFC-159)

Fixed: (1) `buildWherePredicateForTableE` classifies the WHERE walk error via `mapPredicateWalkError`
(bare `ColumnNotFoundError` → 42703), matching SELECT; (2) explicit target-table existence check
(42F01) in `buildLogicalPlanForDelete/UpdateWithCatalog`, independent of WHERE. Verified real (red
probe: all 4 cases were 0AF00), pinned by `dml_where_undefined_probe_test.go` (6 subtests). Original
description below.


Sibling to the now-fixed "UPDATE SET undefined column → 42703" leak. Remaining DML
error-classification asymmetries (all have a SQLSTATE, so lower severity than the SET
leak which had none):
- `UPDATE t SET a=5 WHERE nope=1` (nonexistent WHERE column) → `0AF00: DML Cascades
  translation failed`, whereas `SELECT … WHERE nope=1` → clean 42703 ("column
  NONEXISTENT does not exist", pinned as error_undefined_column_where).
- `UPDATE notable …` / `DELETE FROM notable …` (nonexistent table) → `0AF00: DML
  Cascades translation failed`, whereas `INSERT INTO notable …` → clean `42F01:
  Unknown table`.
The WHERE/table resolution failure in the DML builder
(buildLogicalPlanForUpdateWithCatalog → upgradeDMLWhereWithCatalog /
buildWherePredicateForTableE, and the DELETE equivalent) collapses to a generic
0AF00 instead of surfacing the specific 42703/42F01 the SELECT/INSERT paths already
produce. Fix = thread the specific undefined-column / unknown-table error out of the
DML WHERE/table resolver (matching SELECT/INSERT), rather than mapping any failure to
"DML Cascades translation failed". Check Java's wording/SQLSTATE for parity.

### [x] executor: UPDATE of a PRIMARY KEY column → XXXXX is JAVA-FAITHFUL, not a bug — RESOLVED (RFC-160)

**Misframed (like the secondary-UNIQUE dry-run item).** `UPDATE t SET id=99 WHERE id=1` retargets the
save to the new PK (no record) → existence check fails → XXXXX. **Java is IDENTICAL:**
`RecordQueryUpdatePlan.saveRecordAsync` saves with `ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED`, and
`ExceptionUtil.recordCoreToRelationalException` maps the resulting `RecordDoesNotExistException` to the
DEFAULT `ErrorCode.UNKNOWN` (not in its RecordCoreException switch) — and `ErrorCode.UNKNOWN("XXXXX")`
== Go's `ErrCodeUnknown ("XXXXX")`. So the SQLSTATE matches Java byte-for-byte; a clean Go-only
"cannot update primary key" code (or relocation) would DIVERGE from Java, forbidden by the conformance
principle. No production change. Pinned Java-faithful by `update_primary_key_probe_test.go`
(`pk_update_rejected_xxxxx_matches_java` + no-corruption + non-PK-works). Do NOT "fix" it. Original
description below.


`UPDATE t SET id = <new> WHERE id = <old>` (id is the PK) fails with SQLSTATE XXXXX
(ErrCodeUnknown), message "record does not exist: executor: updating record: record
does not exist". Root cause: executor.go ~2474 applies the SET to the proto message
(including the PK field), then calls `SaveRecordWithOptions(msg,
RecordExistenceCheckErrorIfNotExistsOrTypeChanged)`, which computes the record key
from the NEW pk and fails the existence check (no record at the new pk). The code's
own comment (~2461) assumes "an UPDATE does not change the PK" — an assumption the
SET clause can violate. It is fail-CLOSED: the table is left UNCHANGED (no
corruption; verified). Right end-state: either a clean user-facing rejection
("cannot update primary key", proper 42-class SQLSTATE) or record relocation
(delete-old + insert-new), whichever matches Java — needs a Java-behavior check
(no PK-update handling found in fdb-relational's UPDATE visitor on a quick grep) and
an executor/builder change + review. Low severity (uncommon op, fail-closed) but a
leaky internal error. Sentinel: update_primary_key_probe_test.go (pins: rejected +
no data corruption + non-PK UPDATE still works).

### [x] query-engine: GROUP BY ignores SELECT-list COLUMN ORDER — FIXED by CQ-5 (found 2026-06-28, fixed 2026-07-24)

The aggregate keeps its execution-efficient keys-first native row as a private
ABI. An exact `OutputSlots` mapping plus the universal final Project now exposes
the SQL SELECT list in written order to every positional and named consumer.
Computed outputs, duplicate/colliding labels, INSERT…SELECT, UNION, derived
tables, metadata, qualified joined keys, and zero-call grouped correlated
scalars are covered. Exact ordinal provenance is validated at translation
boundaries; an unprovable mapping fails typed-loud rather than reading a
coincidentally in-range private slot. Sentinels include
`groupby_select_order_probe_test.go`,
`groupby_insert_select_fdb_test.go`,
`union_scalar_aggregate_alias_test.go`, and
`ordered_grouped_scalar_subquery_fdb_test.go`.

### [x] translation: subquery conjunct in a compound JOIN ON clause → CROSS PRODUCT (pre-existing) — FIXED (RFC-154, 2026-06-27)

`SELECT a.id, c.id FROM a JOIN b ON b.a_id=a.id LEFT JOIN c ON c.a_id=a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id=a.id+999)`
returned the CROSS PRODUCT `(1,50)(1,51)(2,50)(2,51)` instead of `(1,NULL)(2,NULL)`. **Root cause was NOT the
executor** (this entry's original guess of `passesJoinPredicates` was wrong): the conjunct was dropped at
SQL→logical translation. `upgradeJoinOnPredicates` installs no SubqueryPlanner, so `WalkPredicate` declined the
subquery shape, a permissive `continue` dropped the WHOLE ON predicate, and the translator ignores `OnText` once
`OnPredicate==nil` → cross product (NLJ with zero preds — EXPLAIN confirmed). **Fixed (RFC-154 Phase 1):**
fail-CLOSED — `expr.ContainsSubqueryAtom` rejects IN-subquery / scalar-subquery in ON with `0AF00` (Go, like Java,
supports neither anywhere); the silent `continue` now surfaces a clean error; `mapPredicateWalkError` shared by
WHERE+ON. **RFC-154 Phase 2a** additionally adds INNER `EXISTS`-in-ON support (Java parity). OUTER EXISTS-in-ON is
deferred behind a fail-closed rejection (Graefe-gated on the RFC-153 rebaser-correlation work). Pinned:
`subquery_in_on_crossproduct_fdb_test.go`, `exists_in_on_fdb_test.go`, `rfc153_joined_preserved_plan_test.go`,
`logical_predicate_test.go`. Graefe + Torvalds ACK.

### [x] ARCHITECTURE — eliminate the legacy embedded SQL interpreter (a "No parallel pipelines" violation, surfaced 2026-06-23 during R8) — DONE (RFC-145)

DONE via RFC-145: Phase 1 (`a966835c5`) detached the executor (severed 7 eval back-edges, re-routed
INFORMATION_SCHEMA to an executor-free system-table handler, stubbed the dead explain ExecFn; exit gate
`git grep execQueryBodyRows == 0` clean); Phase 2 deleted the island (~11.1k LOC, 39 files; the
compiler-as-oracle restored 3 functions the static audit wrongly classed island — genuinely shared with
the Cascades planner). Go now has ONE query path (Cascades). Graefe + Torvalds ACK both phases; codex +
@claude deferred to Jun 25 (PR #336). **Phase-3 follow-up (Torvalds, non-blocking):** residual vestigial
connection state the trim left — `EmbeddedConnection.validQualifiers` + `outerScopes` are now
read-but-never-written (their writers were island-only); `validQualifiers` is read by the kept
`eval_map.go:57` qualifier check (always-nil → branch never fires) and `outerScopes` by `scope.go:85`.
Removing them touches the kept map-path eval logic (behavior-preserving since both are always nil for the
kept consumers — single-source system-table WHERE + constant INSERT-VALUES never set them). Small,
separate cleanup. (`cteData`/`ctes` was the third such orphan — removed in Phase 2.) **→ RFC-147 — DONE.**
Deleted both fields + their resets, collapsed the always-nil branches in `eval_map.go`/`eval_proto.go`,
removed `scope.go` + `scope_test.go` (the only non-nil writer), and pinned the kept qualified→bare
fallback with `TestFDB_InfoSchema_SchemataWhere_QualifiedRef` (red→green: 42703 when disabled). Net
−111 LOC. Torvalds LGTM (collapse proven behavior-preserving; fixed an orphaned `resolveOuterColumn`
comment ref he caught).

Original writeup (kept for context):

`pkg/relational/core/embedded` still contains a complete hand-rolled SQL interpreter — `execSelect` →
`execSelectQuery` → `execSelectQueryFull` → `execSelectJoin` / `aggregateMapRows` / `cte_scan` /
`execUnion` (~3k+ lines across `select_query_full.go`, `join.go`, `aggregate.go`, `cte_scan.go`,
`union.go`, …) — that duplicates Cascades' WHERE/GROUP BY/HAVING/join/CTE/UNION/aggregate execution. This
directly violates CLAUDE.md "No parallel pipelines: Go has ONE query path (Cascades)."

**Current reachability (verified):** `connection.QueryContext` routes EVERY real `SELECT` through
`newCascadesGenerator(c).Plan` → `planSelectCascades` (`cascades_generator.go:206`). The interpreter is
reached only via two fallbacks in `planSelect`: (a) `referencesInformationSchema(q)` → `execSelect`
(`:172`) — INFORMATION_SCHEMA is itself a **Go-only extension Java rejects** (`DIVERGENCES.md`), so there
is no cross-engine reference for that path; and (b) `planSelectExplainOnly` → `execSelect` (`:216`) —
EXPLAIN rendering with no FDB. So the interpreter is **legacy/dead for real data queries** but still
compiled, still maintained, and ROTS: e.g. `aggregateMapRows`'s empty-implicit-group-under-HAVING still
mirrors the OLD Java 4.11 behaviour (returns 0 rows) while the Cascades path was fixed to 4.12
(agg_empty_count_having_passes). That rot is invisible because no real query exercises it.

**Fix:** route INFORMATION_SCHEMA system-table queries through Cascades (or a thin Cascades-backed
system-table scan), make explain-only rendering use the Cascades logical plan it already builds, then
DELETE the interpreter. This removes a large divergence surface + maintenance burden and forces any
INFORMATION_SCHEMA gap to be fixed in Cascades. Big, separate effort — query-engine-gated (Graefe +
Torvalds). Do NOT "keep the two aggregate executors in sync" — that is the anti-pattern; remove one.

### [x] relational/planner: bare boolean column as a single-table top-level WHERE predicate (`WHERE flag`) — DONE (RFC-146)

**DONE (RFC-146):** `walk.go` now lifts a bare boolean value to the COMPARISON form `value = TRUE` — the
byte-identical `ComparisonPredicate` that `flag = TRUE` produces — so `WHERE flag` and `WHERE flag = TRUE`
unify (same plan, semantic hash, and **index match** — Graefe's v1 NAK caught that a bare `ValuePredicate`
would never use a boolean index). NULL → `ConstantPredicate(TriUnknown)` (value-type detection, Java
`instanceof NullValue`); non-boolean → 42804 (clause-agnostic, shared WHERE/ON). The `isBareFieldPredicate`
translator guard is deleted (now dead). Pinned: `TestWalkPredicate_BareBooleanColumn` (structural
PredicateEquals + SemanticHashCode unify), `TestPlanHarness_BareBooleanWhere` (sargable IndexScan),
`TestPlanHarness_BareNonBooleanWhereRejected` (42804), `TestFDB_OuterParity_BooleanWhere` (e2e:
flag→[1], NOT flag→[2], id→42804), `TestWalkPredicate_BareNull`; corpus `bare_bool_where_rejected`→parity
`bare_bool_where`. Graefe ACK (RFC v2 + impl) + Torvalds. Original analysis below.

### (history) relational/planner: bare boolean column WHERE — surfaced by RFC-144 §3d, 2026-06-23

A bare boolean column as a single-table top-level WHERE predicate — `SELECT id FROM a WHERE flag` — fails with `0AF00: Cascades planner could not plan query`, even though: (a) the parser/resolver correctly lift it to `ValuePredicate(flag)` (`expr/walk.go` walkPredicatedExpression; `TestWalkPredicate_BareBooleanColumn` passes), (b) explicit comparisons work (`WHERE flag = TRUE`, `WHERE flag IS TRUE`), and (c) the SAME `ValuePredicate(flag)` shape plans fine inside a join ON clause (`SELECT a.id, b.name FROM a LEFT JOIN b ON a.flag` — pinned green in `TestFDB_OuterParity_BooleanOn`). Java 4.12 supports it: `Expression.Utils.toUnderlyingPredicate` (`fdb-relational-core/.../query/Expression.java:371-399`) lifts a bare boolean value to `ValuePredicate(value, EQUALS TRUE)` and rejects a non-boolean bare value with `DATATYPE_MISMATCH` (42804).

**Root cause corrected (RFC-146 research, 2026-06-25):** the gap is NOT the implement leg (the TODO's original hypothesis). `ImplementSimpleSelectRule` already builds a `RecordQueryPredicatesFilterPlan` from a top-level bare `ValuePredicate` — proven by deleting the guard. The actual bail-out is a conservative guard in the **translator**: `translateFilter` short-circuits to `nil` via `isBareFieldPredicate` (`cascades_translator.go:1687-1689` + helper `:2867`, added commit `85d0dd9f2`). Fix = mirror Java's single lift point: add the boolean type-assertion at `expr/walk.go:1334` (non-boolean → 42804, covers BOTH WHERE and ON), remove the guard, and propagate the type error hard (don't let `buildWherePredicateForTable` swallow it to `0AF00`). Do NOT just delete the guard — that makes `WHERE <non-boolean>` plan and silently return 0 rows instead of raising 42804. Graefe-gated. Pin: flip `TestFDB_OuterParity_BooleanWhere` + the `bare_bool_where_rejected` plandiff corpus entry. Detail: RFC-146.

### [~] fdbgo/client: GetAddressesForKey `ip:port` vs ip-only — MISFRAMED, NOT a real gap at API 730 (re-verified 2026-06-25)

**Original claim was wrong.** It said libfdb_c "defaults the address format to ip-only" and that `include_port_in_address` is "tx-only (no DB-default form)". Both are false against release-7.3:
- C++'s default is **API-version-gated**: `TransactionOptions::reset` sets `includePort = true` for any API version ≥ 630 (`NativeAPI.actor.cpp:6158-6164`); the format decision is `trState->options.includePort ? address.toString() : address.ip.toString()` (`:5747`). This project pins API version **730** everywhere (`libfdbc/backend.go:52`, `fdbclient/open_purego.go:12`), so libfdb_c returns `ip:port` **by default** — exactly what Go returns (`transaction.go:2167-2176`). **Go matches libfdb_c for the version it actually runs.**
- A DB-default form DOES exist: `transaction_include_port_in_address` (code 505, `defaultFor=23`).

The only residual divergence is at API 510–629 (option unset → C emits bare `ip`, Go emits `ip:port`; plus Go wrongly appends `:tls`/IPv6-brackets in the would-be ip-only branch). But Go's `fdb.APIVersion` explicitly **does not emulate version-gated behavior** (`database.go:29-30`), so faithfully emulating API < 630 here would contradict the client's stated design — and would *introduce* a regression at 730 if "default ip-only" were implemented literally. **Resolution: no RFC. Closed as not-a-gap at the pinned API.** If full API<630 parity is ever wanted, it's a small opt-in (honor the API-gated `includePort` + the 505 DB-default), scoped as "emulate API<630," not "Go returns the wrong default." Gate (if pursued): FDB-C-dev + Torvalds + codex.

### [x] recordlayer: legacy format-version-<6 record versions / unsplit records — DONE (2026-06-20)

Go now mirrors Java's `FDBRecordStore.useOldVersionFormat()` end-to-end. Record versions are
read/written in the legacy `RecordVersionKey = 8` subspace for stores below `SAVE_VERSION_WITH_RECORD`
(format 6), and unsplit records are read/written at the bare primary key (no `0` suffix) when
`omit_unsplit_record_suffix` is set — across load, scan, `scanRecordKeys`, `recordExists`, save,
update, delete, and `deleteRecordsWhere` (`store.omitUnsplitRecordSuffix()` / `store.useOldVersionFormat()`
derive the layout from the store header exactly as Java's `checkVersion()`). On open, Go performs
Java's transactional format upgrade (`maybeUpgradeFormatVersion` ⇒ `checkRebuild` /
`addConvertRecordVersions`): bumps `FormatVersion`, sets `omit_unsplit_record_suffix` for a
non-splitting store created before format 5, and moves versions from subspace 8 to the inline
`pk + -1` location when upgrading a splitting store past format 6. Previously Go accepted an old-format
header but only understood the modern inline layout, so it would **silently** miss a legacy store's
versions and unsplit records — a data-correctness bug on the wire-compat hard line. Pinned by
`pkg/recordlayer/legacy_format_test.go` (lays down each legacy layout in FDB and asserts byte-level
read/write/scan/delete/migration parity). Was surfaced by the RFC-131 doc-drift audit.

### [x] fdbgo/client: Get/GetRange over-conflict vs libfdb_c — RFC-121 DONE (PR #319; conflict-range audit 2026-06-19)

Two confirmed serializability-outcome divergences (both SAFE over-conflicts — Go aborted where C/Java
committed, never the reverse), now FIXED. **D1:** GetRange added the full requested `[begin,end)` read-
conflict, not clamped to the data actually returned on a limited/`more` read (C++ clamps to
`keyAfter(lastKey)` — ReadYourWrites.actor.cpp:271-274 / NativeAPI.actor.cpp:4576-4579). **D2:**
Get/GetRange added the read-conflict unconditionally, not skipping keys served by a local independent
write (RFC-058 had wired this RYW filter into GetKey only — ReadYourWrites.actor.cpp:328/342). Fix
routed Get/GetRange conflict generation through the RYW overlay + extent-clamp (`rangeConflictExtent`,
`conflictForKeyLocked`). **Plus a follow-up codex caught:** the streaming `RangeResult.Iterator()` read
later batches under snapshot (no conflict), which became an UNDER-conflict once D1 clamped the first
batch — fixed so every batch is a serializable read adding its own clamped conflict (the C-client
per-batch model). Pinned by red→green differentials + `FuzzDifferential_ConflictOutcome` (63k+ execs)
+ `TestDifferential_GetRangeIteratorConflict_RFC121`, all guarding the under-conflict direction at
`t.Fatalf` severity. Full gauntlet green (FDB-C-dev + Torvalds + /code-review + codex + @claude + CI).
`rfcs/121-get-getrange-conflict-ryw-clamp.md`.

### [ ] fdbgo/client: system-key DB-default applied to a tenant txn — tenant audit (2026-06-19); user-path FIXED

The tenant audit confirmed the WIRE path is byte-perfect (prefix = bigEndian64(id), prepend-at-commit,
TenantInfo, key-size all match C++). One behavioral divergence (#6) was FIXED: `SetReadSystemKeys`/
`SetAccessSystemKeys` on a tenant transaction now return invalid_option (2007), matching C++
`setOption` (NativeAPI.actor.cpp:7159-7171). **Remaining edge:** the DB-LEVEL default path is not
covered. `CreateTransaction` seeds DB defaults (incl. a READ_SYSTEM_KEYS/ACCESS_SYSTEM_KEYS DB
default) while `tenantId == NoTenantID`, and `SetTenantId` runs *after* — so a tenant txn created
under a DB that defaults system-key access silently keeps the flags, where C++ rejects. Fix needs a
check at `SetTenantId` time (reject if system-key flags already set) or at use time; `SetTenantId`
returns void today, so it's a signature/ordering change — deferred. Rare (a DB-wide system-key
default + tenants is unusual). Also documented in-code: the D3 `stripTenantPrefix` clamp divergence
(unreachable — the commit proxy guarantees prefixed boundaries; comment at `locality.go`).

### [ ] fdbgo/client: special-key-space (`\xff\xff/...`) unimplemented — locality audit D1 (2026-06-19)

Go has NO special-key-space module; every `\xff\xff/...` read hits the `maxReadKey()` gate and
returns `key_outside_legal_range` (2004). C++ `ReadYourWritesTransaction::get/getRange` intercept
`specialKeys.contains(key)` and route to `specialKeySpace` BEFORE the maxReadKey gate
(`ReadYourWrites.actor.cpp:1634-1637, 1716-1721`); `DatabaseContext` registers ~30 modules
(`NativeAPI.actor.cpp:1591, 1621-1815`): `\xff\xff/status/json`, `/cluster_file_path`,
`/connection_string`, `/worker_interfaces/`, `/transaction/conflicting_keys`,
`/transaction/{read,write}_conflict_range`, management/configuration, etc. All work via
libfdb_c/Java; all fail with 2004 in Go. It LOUDLY rejects (returns an error, not silent
corruption), but the entire surface is a feature gap. `REPORT_CONFLICTING_KEYS` already noted
elsewhere; this is the broader gap. The `\xff` system-key gating itself is faithful (maxReadKey =
`\xff`/`\xff\xff` matches C++ `getMaxReadKey`). The `SetSpecialKeySpace*`/`SetReportConflictingKeys`
option setters are silent no-ops (`fdb/options.go`). Low-frequency for a record-layer port, but it
is real cross-client surface. D2 (address `:tls`/IPv6 formatting) was FIXED; D3 (INCLUDE_PORT_IN_ADDRESS
no-op — matches api≥630 default, not a real divergence), D4 (`ParseClusterString` whitespace not
collapsed like C++ `trim()`), D5 (IPv6 coordinator round-trip not re-normalized in `ClusterFile.String`;
first-vs-last `@` split on malformed input) are low-impact edges.

### [ ] fdbgo/client: watch-path divergences (D1/D2/D3/D5) — found by the quality-grind watch audit (2026-06-19); D4 fixed

The watch audit fixed **D4** (WatchPoll now retries the SS poll-signals — watch_cancelled/process_behind/
timed_out/future_version — instead of breaking the watch). Four remaining, ranked:

- **D1 [concrete, fixable] — no `too_many_watches` (1032) limit.** C++ `Transaction::watch`
  (`NativeAPI.actor.cpp:5694`) calls `increaseWatchCounter()` (`:2175`) which throws `too_many_watches`
  when `outstandingWatches >= DEFAULT_MAX_OUTSTANDING_WATCHES = 1e4` (`ClientKnobs.cpp:120`, settable to
  `ABSOLUTE_MAX_WATCHES=1e6` via `MAX_WATCHES`); `decreaseWatchCounter()` runs when the watch resolves/
  errors (`:5679`). Go has NO outstanding-watch counter — watches are unbounded; 1032 is never thrown;
  `MAX_WATCHES` is a no-op. Fix: a `db.outstandingWatches atomic.Int64` + `maxOutstandingWatches`,
  increment at `WatchSetup` (return 1032 if at the limit), decrement on EVERY watch exit (fire/error/
  cancel) — the lifecycle is the tricky part. Test with a low limit via a `MAX_WATCHES` option.
- **D2 [architectural — RFC] — watch registered at READ version, not commit-gated.** C++ defers the
  SS-side watch to AFTER commit via `setupWatches()` in `commitAndWatch` (`NativeAPI.actor.cpp:6418`,
  `:6909`), at `committedVersion>0 ? committedVersion : readVersion`. Go's `WatchPoll` registers at
  `tx.readVersion` immediately, with ZERO commit coordination (`commitpath.go` has no watch handling).
  A Go watch is live before its transaction commits. Deep architectural gap.
- **D3 [architectural — RFC] — no RYW pending-write watch semantics.** C++ `RYWImpl::watch`
  (`ReadYourWrites.actor.cpp:1284`) keeps a `watchMap` + `triggerWatches`/`onChangeTrigger` so a watch
  on a key with a differing same-tx pending write fires IMMEDIATELY. Go folds the pending write into the
  baseline (via `tx.ryw.get`) but has no watchMap/immediate-fire — the watch's baseline becomes the
  post-write value and it long-polls for the *next* change (wrong fire point).
- **D5 [small] — cancel returns `context.Canceled`, not `transaction_cancelled` (1025); failed commit
  doesn't cancel watches; stale comment.** `reset()→cancelWatches()` cancels the watch *context*, so
  in-flight watches return `ctx.Err()` not an FDBError 1025 (C++ `resetPromise.sendError(1025)`). And
  (tied to D2) a failed commit never tears down the watch (C++ `cancelWatches(e)`, `:6926`). Also the
  comment at `transaction.go:1595` ("Watch() calls are NOT cancelled by Reset()") contradicts the actual
  `reset()→cancelWatches()` path — cleanup.

### [~] fdbgo/client: `makeSelfConflicting` (`\xFF/SC/<uuid>` synthetic conflict range at commit) — NON-tenant LANDED; tenant + idempotency-id add remain

**STATUS (landed):** The primary `makeSelfConflicting` port is DONE for **non-tenant** transactions
(`transaction.go` `maybeMakeSelfConflicting` — the `!intersects(write, read)` gate → `makeSelfConflictingLocked`,
placed after the read-only fast path + size check exactly as C++ commitMutations). The dummy-barrier key
picker (`intersectConflictRanges`) and the guard now share one C++-faithful sorted-merge `intersectRanges`
(1:1 with `intersects`, `NativeAPI.actor.cpp:6211` — O(n log n), not the old O(w·r) scan). Revert-proven by
`self_conflict_test.go` (wire-level: SC in both vectors for a non-tenant write-only commit; gated OFF for a
tenant commit; gated OFF when real ranges already intersect). **REMAINING:** (1) the **tenant** case —
`buildCommitTransactionRequest` prefixes the `\xFF/SC/` key with the tenant prefix (only `metadataVersion`
is exempt), so a faithful tenant port must either exempt the SC key or scope it inside the tenant keyspace;
skipped for now (gate: `tenantId == NoTenantID`) because the first attempt broke `TestDifferential_Tenant*`.
(2) the SECOND, idempotency-id-based `\xFF/SC/<idempotencyId>` add at `:6850-6856` (automatic-idempotency
feature — distinct, gate on `tr.idempotencyId`).

C++ `Transaction::commitMutations` adds a synthetic self-conflict range to a commit whose write
and read conflict ranges don't already intersect: `if (!causalWriteRisky &&
!intersects(write_conflict_ranges, read_conflict_ranges)) makeSelfConflicting()`
(`NativeAPI.actor.cpp:6858-6860`), where `makeSelfConflicting()` (`:5952`) pushes a single
`\xFF/SC/<deterministicRandom()->randomUniqueID()>` range into BOTH read and write conflict sets.
(There is a SECOND, idempotency-id-based `\xFF/SC/<idempotencyId>` add at `:6850-6856` for the
automatic-idempotency feature — distinct, gate on `tr.idempotencyId`.) Go has neither: a write-only
commit (read conflicts empty → no intersection) ships WITHOUT the synthetic range, and
`commitDummyTransaction`'s `intersectConflictRanges` (`commitpath.go:250-265`) falls back to
`writes[0].Begin` — a real user key — where C++'s dummy uses the synthetic key
(`NativeAPI.actor.cpp:6744-6750`).

**Two effects:** (a) Go's commit-request conflict-range vector diverges from libfdb_c for the same
write-only transaction (request-frame semantic difference — not persisted bytes, but affects the
resolver); (b) Go's commit_unknown_result dummy conflicts on a real user key, so a concurrent writer
of that key can false-conflict the dummy, where C++'s synthetic UUID key never collides with real
traffic. PARTIALLY mitigated today: Go's `OnError(1021/1039)` copies writeConflicts→readConflicts on
the RETRY (`transaction.go:1850`), so the retry is self-conflicting via a different mechanism — but
the original commit's wire shape and the dummy's key choice still diverge.

**Why a dedicated RFC, not a grind fix:** the commit_unknown_result ↔ makeSelfConflicting ↔
commitDummyTransaction interaction is subtle (each attempt mints a FRESH random UID, so it is NOT
simple retry-idempotency), it touches the commit path + wire shape, and it can't be cleanly
differential-tested at the data plane (conflict ranges go to the resolver, not storage — a
fault-injection test that triggers commit_unknown_result is needed). Port `makeSelfConflicting` +
the `intersects(write, read)` gate faithfully under FDB-C-dev DESIGN review; pin with a Go-side
commit-request unit test (write-only commit includes a `\xFF/SC/` range in both sets) + a
SimTransport commit_unknown_result behavioral test.

### [ ] fdbgo/client (#28, HIGH): commit ships the UNFOLDED mutation log; libfdb_c commits the COALESCED RYW write map → Go throws 2101 (`transaction_too_large`) on a transaction C++/Java commit fine — needs its own `fdb-client-engineer` RFC (commit-path materialization; verified vs cgo)

**App-breaking behavioral divergence** (not a wire-bytes-of-stored-data issue — the final DB state is
identical either way; the divergence is the committed mutation COUNT/SIZE, which trips Go's 2101 where
C++ stays under the limit). A transaction that increments ONE counter key 150k times (or overwrites one
key repeatedly) works on libfdb_c/Java and fails on Go.

**Root cause.** Go's `tx.Commit` marshals `tx.mutations` — the raw, unfolded append log (one entry per
`Set`/`Atomic` call). libfdb_c does NOT commit its append log: `ReadYourWritesTransaction::commit`
materializes the COALESCED **RYW write map** via `writeRangeToNativeTransaction`
(`ReadYourWrites.actor.cpp:1997-2071`, called from the commit actor at `:1392`). The write map folds
same-key writes at INSERT time:
- **SET/CLEAR** — last-writer-wins / clear-clears-the-stack (an absolute op resets the operation stack).
- **Same-type associative atomic op** (ADD, OR, AND, MIN, MAX, …) — `WriteMap::coalesceOver`
  (`WriteMap.cpp:480-495`) does `stack.poppush(coalesce(existing, new))`: it REPLACES the stack top with
  the single combined op. So 150k `ADD 1`s on one key collapse to a single `ADD 150000`. (Exceptions kept
  as a stack, NOT folded: `CompareAndClear`; a non-associative op whose operand SIZE differs; two DIFFERENT
  atomic-op types — those `stack.push` instead.)
`writeRangeToNativeTransaction` then emits clears FIRST (`:2004-2018`), then per-key the (folded) operation
stack `op[i]` for `i in 0..op.size()` (`:2035-2065`) — so the shipped mutation vector is the coalesced one.

**Fix shape (RFC-grade, high regression risk — this is the most critical wire path).** Commit from the
coalesced write map for the RYW-enabled path (mirror `writeRangeToNativeTransaction`): iterate Go's RYW
write map (`ryw.go` — `rywEntry`/operation-stack, the WriteMap.cpp analog already used for READS), emit
clear ranges first then the folded per-key ops, in place of marshaling `tx.mutations`. Scope to
`!rywDisabled` (the RYW-disabled path already commits its op log 1:1, matching C++ `:2291` `if
(options.readYourWritesDisabled) return tr.atomicOp(...)`). Port `coalesceOver`/`coalesceUnder` fold
semantics EXACTLY (associative-fold vs push, the operand-size and CompareAndClear/different-type
exceptions). Validate with a cgo differential over many op-combination shapes (repeated ADD, ADD-then-SET,
SET-then-CLEAR, mixed atomic types, non-associative-size-change) asserting byte-identical committed mutation
vectors, plus the 150k-increment 2101 regression (red before, green after). FDB-C-dev DESIGN review before
impl. Do NOT rush at a session tail — 2/2 commit-path fixes this session needed rework.

### [ ] fdbgo/client: transaction-level options are PRESERVED across `onError` retry; C++ resets them to DB defaults — needs its own RFC (found by the quality-grind options audit, 2026-06-19)

C++ `Transaction::resetImpl` (`NativeAPI.actor.cpp:6166`, called by `tr.reset()` on the RYW onError
path, `ReadYourWrites.actor.cpp:1417`) does `trState = trState->cloneAndReset(...)`, and
`cloneAndReset` (`:3515`) builds a FRESH `TransactionState` whose `options` are DB-default-constructed
— it copies the old options ONLY `if (!cx->apiVersionAtLeast(16))` (ancient APIs). So for every modern
app, a retry RESETS `priority`→DEFAULT, `causalReadRisky`→0 (grvFlags), `lockAware`→`cx->lockAware`,
tx-level `sizeLimit`→DB default, `tags`→empty, `snapshotRYWDisableCount`→DB default, then re-applies
ONLY the persistent options (timeout/retry_limit/max_retry_delay/auth_token, `persistent="true"` in
`fdb.options`). Go's `reset()` (`transaction.go:2481`, comment ~`:2528`) instead PRESERVES
priority/causalReadRisky/lockAware/readLockAware/sizeLimit/tags/snapshotRYWDisableCount — the comment
asserts this "matches C++", which `cloneAndReset` disproves.

Wire-visible on the retry: a transaction-level `SetPriorityBatch`/`SetCausalReadRisky`/`SetLockAware`
keeps sending its flags on the retry GRV/commit where libfdb_c reverts to the DB default.
**Why an RFC, not a grind fix:** the faithful fix re-seeds the tx-level options from the DB defaults on
reset (factor out CreateTransaction's seeding, call it from reset, preserve only the 4 persistent
options) — a change to the hot retry path with per-option DB-default subtleties (lockAware→cx default,
not false; causalReadRisky consistency), and the existing code deliberately chose the wrong behavior, so
it needs FDB-C-dev design review. Pin with a unit test (set a tx-level option → reset → assert reverted
to DB default; persistent options survive).

**Other options-audit findings (silent no-ops where C++ acts — `fdb/options.go`):** `REPORT_CONFLICTING_KEYS`
(sets `commit.report_conflicting_keys`; Go field exists at `committransactionref_generated.go` slot 4
but always false), transaction `TAG`/`AUTO_THROTTLE_TAG` (never populate the GRV/commit/read `Tags`
slot — tag throttling non-functional; also no `tag_too_long`/`too_many_tags` validation),
`READ_SERVER_SIDE_CACHE_*` + `READ_PRIORITY_*` (set `ReadOptions.cacheResult`/`.type`; Go no-ops),
`INITIALIZE_NEW_DATABASE` (forces readVersion=0), `USE_PROVISIONAL_PROXIES` (GRV flag bit 2). Per the
conformance principle, the silently-ignored ones should at least LOUDLY reject (UnsupportedOptionError)
rather than no-op — but each is a small feature, scoped separately.

**GRV / read-version audit (same grind) — NO consistency divergence found** (version-vector is OFF by
default, `ServerKnobs.cpp:39`, so Go's empty `ssLatestCommitVersions`/`maxVersion` is exactly correct;
read-version reuse, `read_snapshot`, 1007 aging all match). Latency/observability findings only:
- **Write-only commits omit `CAUSAL_READ_RISKY` on the commit-path GRV.** C++ `tryCommit` does
  `startTransaction(GetReadVersionRequest::FLAG_CAUSAL_READ_RISKY)` (`NativeAPI.actor.cpp:6578`) — a
  write-only/no-prior-read commit doesn't need full causal consistency for its `read_snapshot`. Go's
  commit path (`transaction.go:1507`) calls plain `ensureReadVersion` → `grvFlags()`, setting the flag
  only if the USER did. Effect: an extra TLog epoch-confirmation round-trip per write-only commit
  (latency/throughput, NOT consistency — the read_snapshot is equally valid). **Infra implication, why
  not a grind fix:** Go's `grvBatcherIndex` keys batchers only on the PRIORITY mask, NOT the risky flag
  (unlike C++'s `readVersionBatcher`, keyed by full flags) — so adding the flag would mix risky/non-risky
  GRVs in one batch. The faithful fix re-keys the GRV batcher on the risky flag + threads it through the
  commit-path `ensureReadVersion`; deliberate, FDB-C-dev-reviewed.
- `SetReadVersion` accepts `v<=0` / double-set silently where libfdb_c `setVersion` throws →
  `CATCH_AND_DIE` aborts the process (`NativeAPI.actor.cpp:5519`, `fdb_c.cpp:932`). Go's graceful
  defer-to-1007 is arguably BETTER (no panic in library code per CLAUDE.md) — leave as a documented,
  intentional divergence, don't copy the abort.
- Dropped GRV-reply observability (no consistency impact): `proxyTagThrottledDuration` (the
  `getTagThrottledDuration()` accumulator), the `metadataVersion` reply cache (Go does a real read of
  `\xff/metadataVersion` — correct, one extra round-trip), `midShardSize` (no clear-range cost estimator).

**Minor OnError/knob-audit findings (same grind, low priority — note, don't necessarily fix):**
hedge `secondDelay` uses a fixed `2.0×primary-latency` where C++ uses a runtime-adaptive
`secondMultiplier (≥1.0) × second-best latency + BASE_SECOND_REQUEST_TIME(0.5ms)`
(`loadbalance.go:70` vs `LoadBalance.actor.h:560`; p99 hedge timing only); GRV batcher lacks C++'s
`MAX_BATCH_SIZE=1000` force-flush (`NativeAPI.actor.cpp:7351`; >1000 concurrent GRVs/window wait the
full window); GRV `batchTime` floors at 100µs where C++ has no floor.

### [x] fdbgo/wire: register WatchValueRequest/Reply in the schema extractor (pre-existing gap, surfaced by RFC-115 §6) — DONE (branch `wire/watchvalue-extractor-registration`, stacked on #303)

`cmd/fdb-schema-extract/main.cpp` has no `extractType<WatchValueRequest>()` /
`extractType<WatchValueReply>()` (37 other types are registered). The committed
`pkg/fdbgo/wire/types/watchvalue*_generated.go` were produced out-of-band (commit `52c70585`),
so `just generate-wire-types` (which `rm`s `*_generated.go` then restores only extractor-emitted
types) DROPS them — a regen footgun. RFC-115 §6 restored them after its regen; the proper fix is
to register both in `main.cpp` (`extractType<WatchValueRequest>(outDir, "WatchValueRequest")`,
same for the reply) so a regen reproduces them. WatchValueReply also carries an inline
`Optional<Error>`, so re-emitting it picks up the §6 union fix too. Not caught by per-PR CI
(`just generate` ≠ `just generate-wire-types`). Verify the re-emitted bytes are wire-identical
to the committed files before landing.

**DONE.** Registered both in the extractor (`extract.h` REGISTER_FIELD_NAMES + `REGISTER_GO_TYPE(ReplyPromise<WatchValueReply>)`;
`main.cpp` `extractType<>`); a regen now PRODUCES them. The regen surfaced — and this branch also fixes — **two
deeper extractor wire bugs** the registration depended on:
1. **`Optional<UID>` mis-emitted as `[]byte`.** `scalar_traits<UID>` (flow/IRandom.h) ⇒ UID is a fixed 16-byte
   scalar, so `Optional<UID>` (the `debugID` on requests) must be `[16]byte` (a bare 16-byte OOL scalar behind
   the union RelativeOffset, C++ `SaveAlternative` flat_buffers.h:848), not a length-prefixed vector. Added an
   `Optional<scalar>` codegen path (restricted to UID — the lone fixed-array struct-scalar). Fixed `DebugID` on
   `WatchValueRequest`/`GetReadVersionRequest`/`CommitTransactionRequest`/`StorageServerInterface`/`TenantMapEntry`/
   `ReadOptions`. Verified byte-faithful vs the C++ oracle (un-skipped `debugID`: 4M+ execs, 0 mismatches).
   (Correction to the note above: `WatchValueReply` has NO `Optional<Error>` — it's just `{version int64, cached bool}`.)
2. **`ReadOptions` field-name mis-registration → a live client bug.** The old `REGISTER_FIELD_NAMES(ReadOptions,
   "type","cacheResult","lockAware")` mis-mapped the slots: C++ serialize order is
   `(type, cacheResult, debugID, consistencyCheckStartVersion, lockAware)`, so the generated "LockAware" (slot 2-3)
   was actually `debugID` (Optional<UID>) and the real `lockAware` is a bool at slot 6. The client
   (`readpath.go`) set the debugID field thinking it was lockAware → **lock-aware reads never actually requested
   lock-aware**. Fixed the registration (5 names, serialize order) + the client (`ReadOptions{LockAware: true}`);
   the round-trip unit tests now assert the real bool.

**Follow-up — DONE (RFC-117, commit `b5bdbc00`):** **`Optional<primitive-scalar>` codegen.**
`Optional<int64>`/`<Version>`/`<bool>` were mis-emitted as `[]byte`; the extractor now emits a typed bare
scalar (value encode/decode at the union RelativeOffset, shared with the Variant scalar arm). Regen flipped
only `ReadOptions.consistencyCheckStartVersion` `[]byte`→`int64`; un-skipped in `cmd/fdb-diff-oracle`
(`TestDiffReadOptions`, C++ byte-truth). The UID `[16]byte` array path is unchanged.

### [x] fdbgo/client: stamp the GRV/watch/locate requests with a trace SpanContext — DONE (RFC-116)

RFC-115 §4 stamped the per-op child SpanContext on reads + the tx span on commit, but the GRV,
watch, and getKeyServerLocations requests still carried a ZERO/raw SpanContext. **RFC-116** closes
all three, faithfully to the C++ (NOT the naive "thread a representative tx span" — that would put a
tx traceID on the GRV wire, which C++ never does):
- **GRV** is batched; the GetReadVersionRequest carries the `readVersionBatcher` **fresh-root** span
  (`NativeAPI.actor.cpp:7334/7345/7385/7238`), zero-traceID unsampled unless a sampled tx joins the
  batch (then a brand-new random root via `addLink`). Per-tx spans are local links, never on the wire.
- **locate** stamps the `getKeyLocation` child (`:3017/3037`, derived once in `refresh`, reused
  across proxy retries — `basicLoadBalance` reuse).
- **watch** stamps the `watchValue` child (`:3933/3965`, derived once in `WatchPoll`).
Closed codex's P2 on PR #303. Commits `16847239` (GRV), `a6f08a2a` (locate), `7fdfd24d` (watch).

### [x] fdbgo/client: read-path RPC reply timeout is retryable, not a terminal leak (C++ divergence) — FIXED (PR #288)

Shipped in PR #288 (merge `48106b7d`). `waitReply` (rpc.go) now returns an internal
`errReplyTimeout` sentinel (distinct from caller-ctx cancellation); the three read paths
(`getValue`/`getKey`/`getRange`) re-send on it (bounded by `maxReadTimeoutRetries=10`) and on
exhaustion surface a RETRYABLE `transaction_too_old` (1007) — matching libfdb_c's `loadBalance`,
which has NO per-read client timeout (re-sends a slow-but-alive server until reply or read-version
aging). `getKey` uses three separate budgets (timeout / shard / progress). The commit path keeps
its own `commit_unknown_result` semantics. Found by the 10M SPFresh soak (died at 4.9M records on
the old terminal leak). Pinned by `readpath_timeout_test.go` (deterministic via a reply-dropping
dialer). Gates: FDB C++ dev + Torvalds + codex + @claude all ACK on the final HEAD.

### [x] TOP — SPFresh churn flake on MASTER: live record not findable after concurrent churn (094.3 race)

**ROOT-CAUSED + FIXED on the 094.4 branch (PR #283): the csplit pause-window orphan.**
The fingerprint (`membership=[393217] fine 393217@cell 2 state=0` — membership and
posting entry both present, centroid ACTIVE, search still misses) is the capped-read
truncation shape: the query path fetches postings with a 4×Lmax+1 cap
(`spfresh_query.go`), while the invariant checks read uncapped. On master, a posting
that ballooned past the cap while a pending coarse split PAUSED fine-split issuance
(`spfreshCSplitPaused` skip in the insert probe) never got its split task re-filed —
it survived quiescence oversized, and any record whose entry sorted past the cap was
live-but-unfindable. Fixed by the pause-window repair (csplit move re-files split
tasks for moved oversized ACTIVE rows, commit a55fec70), pinned deterministically in
`spfresh_cascade_test.go` ("csplit move re-files split tasks…"). Verified: 45/45
focused runs green on the branch vs ~1-in-8 red on master. The churn test now also
asserts post-quiescence that every ACTIVE posting is within the 4×Lmax envelope (the
search-visibility bound) and its failure diag includes posting size vs cap + sidecar
presence, so either silent-miss shape self-diagnoses on any recurrence.

- [ ] **RFC-156 budget-exhaustion 5s-deadline stress is unverified programmatically.** `spfreshDefaultStreamCellBudget=512` / `spfreshDefaultStreamCandidateBudget=4000` are calibrated-by-comment, not pinned by a test asserting that ordered-stream search+materialize stays within the FDB 5s tx deadline on a large index + a pathologically selective residual filter (the heap/stream tests verify memory bounds and truncation honesty, not the wall-clock deadline).

### [x] CORRECTNESS FIXED — re-enumerated indexed multi-way joins (was: NULL / 0 rows)

**Symptom (fixed).** A 3-way *indexed chain* join planned through the RFC-042 L3
index-NLJ re-enumeration path returned wrong results that depended on the
FROM-order: one order returned 200 rows all-NULL, the opposite order returned 0
rows (correct is 200 rows, all `t1.id = 1`). 2-way joins and non-indexed *star*
3-way joins (`TestFDB_ThreeTableFrom`) were always correct.

**Root cause (pointer-level instrumented).** `PartitionSelectRule` misrouted the
*spanning* join predicate (e.g. `t3.t2_id = t2.id`, one alias in each partition
half) into the **lower** partition. Java's classification keys on
`uppersDependingOnLowersAliases`, computed from `getCorrelationOrder()` —
**quantifier** correlations. Go's flat-seed join quantifiers are independent
scans with **no quantifier-level correlations** (the joins are plain predicates),
so `uppersDependingOnLowers` is *always empty* and the spanning predicate always
fell to the "can do in lower" branch. That yields a degenerate **Case-1
cross-product** partition whose lower result is a `{_0}` literal placeholder
(discarding the real columns) and whose pushed-down filter evaluates against
unbound upper aliases → wrong rows. The physical FlatMap then merges via
`JoinMergeResultValue`, which cannot resolve columns nested under `_0` → NULL.

**Fix (shipped).** `PartitionSelectRule` now rejects the degenerate partition: a
predicate routed to the lower that references an UPPER alias cannot be evaluated
there, so the whole partition is skipped (`rule_partition_select.go`, "Reject
degenerate partitions" guard). The valid associativities — where the spanning
predicate stays at the join level — then win identically for every FROM-order.
Both orders now return 200 correct rows; deterministic; full suite green.
`multiway_join_index_probe_test.go` was a plan-shape-only fake checkbox (never
executed the query) — now retrofitted with **row-correctness** assertions for
both FROM-orders, which is the load-bearing check.

**Remaining (cost-optimality, NOT correctness) — RFC-042.** Under the big→small
FROM-order the re-enumerated `(t2⋈t3)` sub-product still prefers a cross-product
NLJ over the index probe (the index-probe alternative either loses on cost or
flows a sub-product result the parent predicate can't SARG), so that order
full-scans the 200-row T3 instead of index-probing it. Correct, just slower. Full
byte-identical FROM-order invariance for N≥3 (the `TestFDB_MultiwayJoinOrder_Probe`
goal) depends on closing this cost gap + FROM-order-deterministic winner selection.
Likely levers: the index-probe cardinality cost (criterion #2 — make the FlatMap
inner range over the index-scan wrapper so `maxDataAccessCardinality` reflects the
probe), and making re-enumerated sub-products flow a flat `JoinMergeResultValue`
so the index-probe variant is both cheaper AND resolvable.

- [ ] **Re-verify `joinOptimizationProbesScenario` (RFC-082 cross-engine exclusion) against RFC-042 (@claude flag).** The A3 builder is excluded from `crossEngineScenarios` with the note "Go's join enumeration is still non-deterministic on some arithmetic-predicate shapes — a 3-way / arithmetic-join can return a different ROW COUNT across runs." That row-count *nondeterminism* (a correctness flake) is NOT the item tracked above — line 11-40 is the now-FIXED FROM-order-dependent (but per-order deterministic) bug, and line 42 is cost-optimality (correct results, just slower). So either the exclusion note is stale (the row-count flake was the fixed PartitionSelectRule bug → the scenario may be re-enableable cross-engine now) or there is a genuinely-still-nondeterministic join-enum shape that needs its own root-cause. Verify with a focused multi-run of the probe shapes; if still nondeterministic, the Go-only yamsql coverage for `join_optimization_probes` is itself flaky (same code path) and must be pinned, not just excluded cross-engine. Out of scope for RFC-082 (conformance determinism); tracked here for the RFC-042 follow-up.

### vs Java (correctness/feature parity)

- [x] **Correlated filter without index.** Fixed in 56874f23 — ImplementFilterRule sets innerAlias on RecordQueryPredicatesFilterPlan. All correlated paths (scalar subquery, EXISTS, JOIN) work without indexes. 14+ integration tests verify.
- [x] **RIGHT/FULL OUTER JOIN.** Done in RFC-036. (The old "only LEFT OUTER" note was stale — RIGHT already worked via operand-swap normalization in `cascades_translator.go`, pinned by `TestFDB_RightJoin`.) FULL OUTER added as a Go-only query extension: Java's SQL layer has **no** outer joins at all (`visitOuterJoin` is a no-op, zero tests), so LEFT/RIGHT/FULL are all read-path-only extensions with **zero wire-format impact** — Java apps still read/write the same records. FULL OUTER is implemented exclusively by the materialized NLJ cursor (`streaming_cursors.go`): LEFT-OUTER outer loop + a `matchedInner` bitmap + a drain phase emitting unmatched inner rows NULL-padded on the left. Routed away from the correlated FlatMap path (cannot observe global inner-match state); FULL+EXISTS rejected with a clear error. 9 FDB integration tests (all four row classes, NULL-key 3VL, many-to-many, large-inner hash+drain, WHERE-above-join, determinism, RIGHT NULL-key regression). Graefe+Torvalds ACK.
- [x] **Correlated scalar subquery shapes widened.** Non-aggregate (ORDER BY + LIMIT), multi-table inner FROM (JOINs), multi-column validation, deep-walk replaceScalarSubqueryRef, and real-aggregate GROUP BY/HAVING are supported. Group-key-only HAVING and other unproven shapes reject typed-loud. CorrelatedExistsError propagation fixed.
- [ ] **No *general-purpose* window functions — and Java has none either.** Investigation (RFC-045): Java's relational layer has **no** general streaming window operator. The general `windowClause` is commented out in Java's grammar ("don't want to deal with them now"); `LAG`/`LEAD` are grammar tokens with **no** value class; `RankValue implements Value.IndexOnlyValue` (computable only from a rank/leaderboard index, never over a result set). The **only** working window function in Java is `ROW_NUMBER() OVER (... ORDER BY <distance>) <= K` via `QUALIFY`, used exclusively for **vector/HNSW K-NN search**. So "match Java's window functions" ≡ "finish the vector/HNSW relational parity" — tracked as **Phase 9** below. General windowing over plain tables would be a *Go-only extension Java lacks entirely* (allowed if wire-compat holds + deep tests), not parity — deferred, not in Phase 9.
- [x] **GROUP BY/HAVING in correlated scalar subqueries.** Done in RFC-047 and cardinality-corrected by CQ-4 — a Go-only read-side extension (Java rejects correlated scalar subqueries at the grammar level entirely; zero wire impact). The stale "PredicatePushDownRule AliasMap.Compose conflict" blocker no longer applies: GroupByExpression is already a push-down barrier (no case in `pushPredicateToExpression`) and the panicking `AliasMap.Compose` has no production callers. `buildCorrelatedScalar` builds GROUP BY (+ real-aggregate HAVING) into the inner plan and leaves an unpaged grouped result uncapped; the shared strict FirstOrDefault barrier raises 21000 if more than one post-HAVING group survives. Explicit LIMIT 0/1 is preserved exactly and intentionally selects at most one group. Empty input → 0 groups → LEFT-scalar NULL falls out naturally (vs no-GROUP-BY COUNT → 0). Group keys + aggregate operands resolve via the semantic scope (`ResolveIdentifier`), scalar column named with the bare operand to avoid an embedded-`.` qualifier mis-parse. 42803 is enforced via `validateGroupByProjection`; group-key-only HAVING, multi-column + EXISTS-in-HAVING, and unresolvable expr-arg/key shapes reject typed-loud. FDB integration probes pin StreamingAgg, empty→NULL contrast, expression group keys, join+GROUP BY, post-HAVING 0/1/2+, and exact 21000.
  - [x] **Follow-up: `ORDER BY` over grouped output in a correlated scalar subquery.** Done in RFC-085 and hardened by CQ-5. Group keys and selected aggregates are resolved through the semantic scope, structurally rebound to exact native `[keys..., calls...]` ordinals, sorted over the private aggregate row, and exposed through one exact one-field Project before a written LIMIT/OFFSET. The correlated lowering therefore sees `Limit(Project(Sort(Aggregate)))`, can reattach pagination per outer row, and always reads scalar ordinal 0 from a proven one-column row. Zero-call grouping uses the same contract. Qualified/unqualified single-source spellings are symmetric; joined same-bare keys remain distinct; an unselected aggregate or non-grouped field rejects 42803. Without LIMIT, a second group still raises 21000. Pinned by `ordered_grouped_scalar_subquery_fdb_test.go`, `correlated_scalar_cardinality_followup_fdb_test.go`, and `quality_probes_test.go`.
  - [x] **Follow-up (single-source): expression/constant-argument aggregate that meets a *differing* aggregate via HAVING in a correlated scalar subquery.** DONE — the addendum unified producer and consumer on **one** canonicaliser (`canonicalAggName`, called by both `buildCorrelatedScalar` and `rewriteAggregateValue`), so the two name schemes can no longer drift; the prior fail-safe rejection is gone for single-source. The last silent-wrong corner (nested-arithmetic args like `SUM((amount+10)*2)` returning NULL → dropped groups) was a *separate* root cause — an inverted `!isArith` guard in `translateAggregate` that preferred a lossy text reparse over the resolved operand — fixed in RFC-048 (4dc3276c): the resolved `AggregateOperands[i]` is now always the source of truth. Works now (single-source): `SELECT COUNT(1) … HAVING COUNT(*)` both directions; `SELECT SUM(a*2) … HAVING SUM(a*3)`; decimal-literal args (`SUM(a*1.5)`); nested-arith args (`SUM((a+10)*2)`). `COUNT(DISTINCT 1)` correctly still rejected (DISTINCT unsupported here). Pinned by `quality_probes_test.go` (count_constant_with_having_works, expression_aggregate_in_having_works, decimal_literal_aggregate_arg_in_having, nested_arithmetic_aggregate_arg_in_having). **Residual (join only):** over a JOIN an expression-argument aggregate in HAVING is still rejected (the operand binds to the wrong quantifier through the parser round-trip) — pinned by `join_expression_aggregate_in_having_rejected`.
- [x] **🚩 IN over an indexed column drops the outer projection (wrong result schema).** Fixed in **RFC-070**. Root cause was two defects: (1) `MergeProjectionAndFetchRule`'s fallback dropped the projection when the fetch's child was an InJoin (not a coverable index scan), leaking a bare `InJoin` ([ID,A]) into the root projection group where it won on cost; (2) `physicalProjectionWrapper`/`physicalFetchFromPartialRecordWrapper` `WithChildren` didn't relink a compound-join inner during extraction (left `Project([id], InJoin(<nil>))` / `Fetch(<nil>)`), because of an `isLeafReplaceable` gate — same gate RFC-069 removed from the in-memory sort wrapper. Fix: fallback retains the projection; the two transparent caps relink unconditionally. `SELECT id FROM t WHERE a IN (1,7)` → `Project([ID], InJoin(IndexScan(IDX_A,[=])))`; `SELECT id+100 ...` (was 0 rows) → `{101,107}`. Pinned by `TestFDB_INProj_OuterProjectionOverInJoin` (indexed+unindexed, multi-column, expression-projection, 8× determinism). Graefe+Torvalds ACK.
  - [ ] **Follow-up (RFC-070): `pushValue`-into-covering-result-value modeling gap.** Java's `MergeProjectionAndFetchRule` yields a bare `fetchPlan.getChild()` because `RecordQueryFetchFromPartialRecordPlan.pushValue` rewrites the projected value into the covering plan's own result value. Go's `WithCovering` only sets a flag (the scan still flows the full partial record), so Go compensates with a thin outer `Project`. Pushing the value into the covering result value would let both rule branches collapse to a bare child yield, matching Java. Cosmetic/architectural — current behaviour is correct.
  - [ ] **Follow-up (RFC-070): other transparent unary wrappers over joins.** `Map`, `Distinct`, `Limit`, `TypeFilter`, `FirstOrDefault`, `DefaultOnEmpty` still gate `WithChildren` on `isLeafReplaceable` and could exhibit the same nil-inner-over-join bug if a rule ever builds them with a placeholder inner over a join. Not currently reachable via SQL (projections route through `LogicalProjectionExpression`, not `Map`); the **blanket** gate removal is unsafe — it regressed `TestFDB_AggregateIndexUsage` by dropping the eq-filter on aggregation/DML wrappers (which embed filter semantics in their own plan). Each wrapper needs individual analysis if/when reachable.
- [x] **DML does not execute through Cascades (parallel pipeline).** Fixed as **P0.4** — all DML now executes through Cascades (`planDML`); the naive `execStatement` DML path is deleted. See P0.4.
- [x] **🚩 `INSERT … SELECT … GROUP BY` wrote the wrong columns (spurious 23505).** Fixed initially in RFC-084 and subsumed by CQ-5. Every aggregate SELECT now owns the same exact SELECT-order public Project, so INSERT consumes ordinary positional output; the insert-only `wrapBareAggregateInsertSource` coercion and its qualifier-based skip are deleted. COUNT(*), reordered/multiple aggregates, HAVING-only private calls, aliases, and qualified sources all use the universal boundary. Pinned by `groupby_insert_select_fdb_test.go`.
  - [x] **Follow-up (RFC-084): qualified aggregate operand on the insert-source path.** Fixed by structural operand resolution plus the CQ-5 exact public Project; qualified INSERT…SELECT variants now assert their rows.
  - [x] **Follow-up (RFC-084 / RFC-079): delete insert-only aggregate coercion.** Done by CQ-5: aggregate SELECT itself supplies the public Project and `wrapBareAggregateInsertSource` is gone. Whole-SimpleTable builder unification remains separately tracked below.
- [x] **🚩 Aggregate result-type derivation diverges from Java: `AVG(x)→DOUBLE`. DONE — RFC-083.** `AggregateValue.Type()` now types `AVG → NullableDouble` (function-determined, matching Java `AVG_*→DOUBLE`); SUM/MIN/MAX stay operand-derived, COUNT→LONG. The "ZERO new code / existing IsPromotable check" framing was **inaccurate** (no plan-time promotion check existed — `IsPromotable` had zero callers; the only enforcement was a runtime band-aid), so the fix is three coordinated parts: (A) the AVG `Type()` arm + collapse the duplicate AVG→DOUBLE SQL-name encodings onto it (`valueTypeName`/`aggregateResultType` route through `Type()`); (B) a **plan-time promotion guard** at the INSERT…SELECT chokepoint (`checkInsertSelectPromotable`, the first production `IsPromotable` caller) keyed on aggregate **provenance** — `LogicalProject.AggregateSlots` (captured pre-rewrite via `containsAggregate`) for computed exprs like `AVG(v)+1`, and name-resolution against the producing `LogicalAggregate` for bare `AVG(v)` (whose projection slot carries a nil value) — so `AVG→BIGINT` is rejected 22000 **even over an empty source** (emergent from the lattice, not a materialized float); `rewriteAggregateValue` now preserves `Typ: av.Type()` (was discarding it as UnknownType); (C) converge the runtime converters — remove `ConvertToProtoValue`'s whole-float→int64 coercion (VALUES double→BIGINT now rejects 22000), and give `goToProtoValue` the promotable INT/LONG→FLOAT/DOUBLE widenings + an **emergent 22000 fallthrough** (also fixes the adjacent `SUM(BIGINT)→DOUBLE` gap that used to error). Pinned: `values_test` AVG-type pins, flipped both `ConvertToProtoValue` whole-float unit tests, new `goToProtoValue` widening/reject tests, `avg_double_insert_fdb_test.go` (scalar/empty-source/`AVG+1`-empty/`→DOUBLE`/`SUM→BIGINT`/`SUM→DOUBLE`/plain-arith/VALUES double-reject/index-presence EXPLAIN). insert_select.yaml corpus corrected. Ripple guard holds (AVG never lowered to `Sum/Count` ArithmeticValue division; no aggregate index → streams). Full `just test` green. Graefe+Torvalds ACK'd RFC (v4) + impl.
  - [ ] **Follow-up (RFC-083): replace the guard + `AggregateSlots` marker with Java's `PromoteValue` projection nodes** — the single mechanism that both rejects-at-plan and widens-at-runtime, dissolving the dual lattice-encoding (guard + converters) and the load-bearing "aggregate-slot ⇒ guard" coupling (Graefe's end-state). Subsumes reliably typing `FieldValue`/`ArithmeticValue` projections, which then closes the **residual deferred cases**: bare-column `SELECT double_col → BIGINT` over an empty source, and `UPDATE … SET int_col = <double-expr>` — both currently rely on the runtime converter (correct for non-empty rows, miss the 0-row case).
  - [x] **Follow-up (RFC-083): GROUP BY-aggregate INSERT…SELECT source.** Closed by CQ-5's universal typed Project: the promotion guard and positional target alignment always see the SELECT-list layout; the former bare-aggregate source no longer exists.
  - [ ] **Adjacent (separate index-type bug): `GetIndexTypeName` hardcodes `MIN_EVER_LONG`/`MAX_EVER_LONG`** — MIN/MAX over a non-long operand needs `MIN_EVER_TUPLE` (Java `permuted_min/max`).
- [x] **🚩 TODO 7.6-union-remap — aggregate UNION branch with a mismatched output alias drops rows (pre-existing executor gap).** Fixed for STREAMING aggregates in **RFC-078**: (1) `executeUnorderedUnion` (executor_new_plans.go) now remaps later branches' columns to the first branch's names by position — it previously concatenated branch cursors with NO normalization at all (unlike the sibling concat `RecordQueryUnionPlan`/`executeUnionStreaming`); (2) `planColumnNamesWithMD` (executor.go) reports a `RecordQueryStreamingAggregationPlan`'s output names (group keys + alias-or-canonical) instead of descending through `GetInner()` to the input scan. `SELECT u.x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u` now returns both counts (was `[2, NULL]`). Pinned by `TestFDB_UnionAggregateColumnRemap`. Graefe + Torvalds ACK.
  - [x] **Follow-up (RFC-078/RFC-081): aggregate UNION join legs.** Historical physical-name reporting fixes remain, but CQ-5 removes the logical dependence on those private names: every SQL aggregate branch is Project-topped with an exact positional public schema. Ungrouped and grouped branches, including AggregateIndex/MultiIntersection realizations, normalize through that boundary.
    - [x] **Divergent aggregate spellings in grouped UNION branches.** Qualified `SUM(t.c)` and constant `COUNT(1)` are positive through the exact Project and are pinned by `TestFDB_UnionQualifiedAggregate` and `TestFDB_UnionGroupedCountConstant`. Unsupported aggregate forms such as DISTINCT remain explicit feature boundaries, not name-remap fallbacks.
  - [x] **(b) Go concat-union projection-alias — FIXED in RFC-079.** A UNION branch projecting a post-aggregate EXPRESSION with an alias (`SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b`, read by name) returned `[NULL,NULL]` — the legacy `buildSelectShell` builder (the UNION-branch path) built the post-agg projection with `nil` aliases, dropping the `AS x`. Fixed by extracting the projection-building loop into one shared `buildPostAggregateProjection` helper called by both `visitSelectGroupBy` (modern) and `buildSelectShell` (legacy) — one source of alias truth. Pinned by `TestFDB_UnionAggregateExprAlias` + `TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias`. Modern path plandiff byte-identical. Graefe + Torvalds ACK.
  - [ ] **Follow-up (RFC-087, Graefe): reject aggregate-in-scalar-context at PLAN time.** `WHERE COUNT(*) > 0` reaches `AggregateValue.Evaluate` at row eval; RFC-087 made it a clean runtime `AggregateEvalError` → 42803 (was an uncaught goroutine crash on master — Graefe confirmed). Java rejects this at semantic-analysis / plan time ("unable to eval an aggregation function with eval()"). Detect an aggregate in a per-row scalar predicate (WHERE / JOIN-ON / projection-not-under-GROUP BY) during planning and reject there, matching Java exactly. Runtime 42803 is the safety net; plan-time is the parity fix.
  - [ ] **Follow-up (RFC-087, Graefe): thread `ComparisonKeyFunc` error channel.** The 5 executor merge/sort comparison-key sites (`intersectionCompKeyFunc`, `multiIntersectionCompKeyFunc`, `mergeSortCursor.isBetter`/`extractKey`, executor.go:1391) `panic(err)` on a stray key-eval error — pre-existing behaviour (no recover before/after RFC-087), and keys are pre-projected field refs so the typed-error family is unreachable today. To make it airtight, give `ComparisonKeyFunc` an `error` return and thread it (ripples into wire-adjacent `merge_cursor.go`). Low priority — not reachable from current SQL.
  - [ ] **Follow-up (RFC-088, Graefe condition): converge `validateGroupByProjection`'s existence check onto the semantic resolver.** Java does NO standalone existence check for GROUP BY keys — `SemanticAnalyzer.resolveIdentifier` over the full multi-source scope already guarantees existence, and `validateGroupByAggregates` enforces only the algebraic 42803 rule (key must be grouped-or-aggregated). Go currently runs a SECOND, hand-rolled existence oracle (`tableFields` = union of all source descriptor field names, bare-name match) that is deliberately qualifier-blind, so it would false-ACCEPT a wrong-qualifier key (`e.dname` where dname is on the joined dept) — SAFE today ONLY because the precise resolver runs first at every call site (top-level `resolveColumnName` ~L1002; correlated-scalar GROUP-BY-key resolution in `buildCorrelatedScalar`), an ordering invariant now pinned by a code comment at `validateGroupByProjection` and by `TestFDB_GroupByWrongQualifierRejected`. End-state: route existence through `resolver.ResolveIdentifier` and leave `validateGroupByProjection` enforcing only 42803, removing the duplicate oracle and the ordering dependency.
  - [ ] **Cleanup (RFC-079 follow-up b): unify the SimpleTable logical builder onto `visitSelectGroupBy`.** The "one query path" endgame (CLAUDE.md "no parallel pipelines"). `buildSelectShell`/`buildLogicalPlanForSelect` is a second SELECT builder reached by plain-table SELECTs, derived tables, AND UNION branches; it has repeatedly drifted from the modern `visitSelectGroupBy` (the RFC-079 alias bug was one such drift). Route ALL of its callers through `PlanVisitor.visitSelectGroupBy` and delete the legacy builder. Larger than a single-bug fix (multiple callers, full regression surface) — Graefe's condition: must unify the WHOLE SimpleTable builder, not graft a special case onto the union entry.

### Beyond Java (Go-only improvements)

- [x] **Full Graefe Memo with cross-group merging.** Done in RFC-037 — union-find group merging (the Cascades-paper "merge two groups discovered to be one", §2 + §3.5), a Go-only extension beyond Java (which, like the pre-RFC Go memo, only interns at insertion time). `Reference` gains a monotonic `id` + `forwardedTo` + path-compressed `Canonical()`; every state-bearing method resolves the receiver to canonical, so a merged-away (loser) Reference transparently forwards — no in-flight task, Quantifier, or binding is rewritten. `GetRangesOver()` resolves at the single chokepoint (444 sites). `Memo.Integrate` hooks the REWRITING yield path: when a yielded expression equals a member of a different group, the two merge (survivor = lower id, deterministic), folding members + exploration state, repointing the topology index, invalidating correlation caches up the DAG, and recursively re-merging parents (paper's bottom-up recursion). Scoped to REWRITING (PLANNING winners/partial-matches embed raw References — guarded by `mergeable`); ancestor/descendant (idempotence) merges skipped to avoid DAG cycles. Wire compat untouched (read-path-only sharing). Merge fires through the real planner (`TestMemoMerge_FiresThroughRealPlanner`); 9 merge unit tests + determinism 50×/10×; 46/46 targets green; stress-1M unchanged. Graefe+Torvalds ACK (NAK'd v1 on in-flight-task stranding + cache staleness + index repoint + upward re-merge — all fixed in v2). **Reach caveat (honest):** the merge is correct and fires, but its practical reach is narrow today — the memo's interning/equivalence is alias-sensitive, and rule-rewritten equivalents mint fresh quantifier aliases, so equivalent sub-expressions intern to *different* child References and rarely surface as merge candidates (measured: exactly one merge on a K-branch equivalent UNION regardless of K; ~2% planner-time delta; no execution-time effect — same plan). Broad merging (and any real speedup / multi-way-join-order benefit) is **gated on alias-namespace unification (item 7.1 below)**; this PR lands the correct merge *infrastructure*, not a present-day perf win. Remaining (Future Work): **alias-normalized equivalence (7.1) — the lever**; reduction-rule-triggered merges (§3.6); PLANNING-phase merging; cost-model exploitation of shared sub-products for full N-way join-order optimality.
  - **PR-A landed the lever (RFC-038 epic / RFC-039 + RFC-040).** The reach caveat is now closed: the memo's structural-equivalence compare sites use alias-aware `expressions.MemoEqual` (faithful port of Java `Reference.isMemoizedExpression`) on top of the RFC-040 foundation (alias-aware `EqualsWithoutChildren` + alias-invariant `HashCodeWithoutChildren`). Rule-rewritten equivalents that differ only in fresh quantifier aliases now intern/merge into the SAME Reference — proven by `memo_activation_test` (K=6 alias-variant filters → 1 shared Reference, was K distinct). Zero plan-shape regression (plandiff conformance green), 10/10 deterministic, stress-1M before/after within noise. Graefe+Torvalds ACK. Still ahead in the epic: **PR-C** join-order enumeration (associativity/commutativity, capped) and **PR-D** cost selection + the e2e "multi-way join ordering proven" test (N-table join, EXPLAIN-pinned optimal order ≠ FROM-order, shared sub-products merged).
  - **PR-C scope corrected (RFC-074).** PR-C was framed as "efficient ≥5-way enumeration via sub-product interning (collapse the dual merge values)." **Measurement falsified the premise:** collapsing `JoinMergeResultValue`/`JoinMergeAllValue` to one canonical type does NOT reduce `distinctRefs`/`tasksRun` (N=5 stays 127k tasks / 171 refs) — the duality is a ~2× constant, not the exponential. The exponential is that logically-equivalent join sub-products land in SEPARATE memo References (even identical scans fork ×3) and never coalesce: `mergeable` (memo_merge.go:84) refuses once a group `HasWinnersOrMatches`, and `OptimizeGroup` interleaves `SetWinner` with `Integrate` yields, so a group holds a winner before its equivalent twin is born. RFC-074 now ships ONLY the **merge-value collapse** — a correct Go-only-divergence removal + prerequisite for single-type interning, **behavior-preserving (NOT a budget fix)**. Graefe re-ruled.
  - **PR-C2 — the actual ≥5-way budget fix (NEW, separate RFC).** Java does NOT solve the blowup by merging-under-winners (RFC-037 broad merge is a Go-only extension Java lacks); it **bounds the bipartition lattice at the source** via `shouldDeferCrossProducts` + `shouldJoinRightDeep` (Java `PartitionSelectRule.java:92,122`) and builds legs in a canonical interning-friendly form. Port/enable that pruning into `rule_partition_select.go` (the hooks exist — `ShouldJoinRightDeep`/`ShouldDeferCrossProducts` — verify defaults + why a pure connected chain isn't bounded). 1:1 Java-aligned. Do NOT decouple exploration from optimization (Java interleaves identically) and do NOT extend broad-merge-under-winners. Graefe-ruled.
- [x] **Correlated scalar subqueries.** Go-only extension — Java rejects at grammar level. Implemented via FlatMap with JoinTypeLeftOuter.

---

## Production readiness (Graefe review, 2026-05-28)

The Cascades architecture is solid — task stack, two-phase REWRITING+PLANNING, multi-criteria cost model, match-candidate infra all well-ported. The production risks are all at the **boundaries**: planner↔executor, executor↔runtime, system↔operator. Priority tiers below.

### P0 — fix before deploying anywhere (correctness/availability)

- [x] **🚩 P0.4 DML executes through Cascades.** Fixed in RFC-035 — all DML (INSERT VALUES/SELECT, UPDATE, DELETE) routes through `planDML` → Cascades executor; `planOne` no longer branches on exec mode and the naive `execStatement` DML path (`execInsert`/`execUpdate`/`execDelete`/`execInsertSelect`, `pkPushdownCursor`) is deleted. INSERT VALUES reuses the Explode operator (RecordConstructor→Array→Explode→Insert) with plan-time arity/NOT-NULL/coercion; UPDATE SET RHS resolves to Values; DELETE/UPDATE WHERE gets EXISTS/scalar-subquery support via `upgradeDMLWhereWithCatalog`; INSERT…SELECT maps projection→target positionally and materializes (Halloween-safe). `IsUpdate()` derived from physical plan type; `RowsAffected` counted (Java's countUpdates); DML respects explicit transactions via `runInTx`. Fixed a latent non-correlated-EXISTS semi-join bug that also affected SELECT. QueryContext rejects update plans before executing (use Exec) — documented divergence in DIVERGENCES.md. Corner-case tests in `dml_cascades_fdb_test.go`. Graefe+Torvalds ACK (direction + implementation).
- [x] **P0.1 NLJ memory bomb.** Fixed in PR #203 — `CollectAllBounded` with configurable materialization limit (default 100K rows) on all 6 `CollectAll` sites. `MaterializationLimitExceededError` typed error. All cursor leaks on error path fixed. 11 regression tests. RFC-028.
- [x] **P0.2 Plan cache serves wrong plans.** Fixed across RFC-029 + item 3c. RFC-029 keyed on the
  normalized SQL string (killing the uint64 FNV-64a collision), but the key had THREE residual
  non-injectivity bugs, all now closed (item 3c, `query_hash.go` + `planCacheKeyInput`): (1) the key
  was built from `q.GetText()`, which concatenates tokens with NO separator, so `SELECT AB FROM T`
  and `SELECT A B FROM T` both keyed `SELECTABFROMT` → now `canonicalTextOf` preserves token
  boundaries; (2) `normalizeSQL` uppercased inside DOUBLE-quoted / backtick delimited identifiers,
  which are case-SENSITIVE here (`"a"` vs `"A"` are distinct columns) → now case-preserved like
  single-quoted literals; (3) `collapseWhitespace` folded whitespace INSIDE string literals
  (`'a  b'` vs `'a b'`) and `stripComments` didn't guard delimited ids → both now quote-aware.
  The key is ALSO now schema-scoped (schema name + metadata version prefix): `SetSchema` mutates
  only the session schema, so the same SQL against a different schema previously returned a stale
  plan (Java's QueryCacheKey carries the schema template version; RFC-024). Pins:
  `plan_cache_key_test.go` (all three injectivity cases + schema/version scoping, red pre-fix).
  Optimization gap remaining (NOT a correctness bug): Java's AstNormalizer parameterizes literals
  (`x = 1` / `x = 2` share a plan); Go keys on literal text → more misses, never a wrong plan.
  Scalar subquery staleness was a non-issue: `scalarSubqueryBinding` stores plans not results,
  re-evaluated per page fetch. `QueryHash` retained for tests only.
- [x] **P0.3 No context cancellation in executor.** Fixed in RFC-030 — `ctx.Err()` checks at the top of every cursor OnNext loop and drain function (44 sites across 19 files). `autoContinuingCursor` was the worst offender (created new FDB transactions on cancelled contexts). All cursor combinators, executor cursors, utility drains, DML drains, legacy query path drains, and iterator adapters now respect Go context cancellation. 24 unit tests verify prompt cancellation.

### P1 — fix before relying on the optimizer for real workloads (plan quality)

- [x] **P1.1 Wire statistics from FDB.** Fixed in RFC-031 — `fetchTableStatistics` was already wired (nightshift-100) but had two bugs: used read-write transactions for read-only stats (wasted commit), and fabricated equal-distribution counts for intermingled schemas. Fixed: `FDBDatabase.RunRead()` for no-commit snapshot reads, dropped intermingled fallback (returns nil → safe DefaultStatistics). E2E FDB integration test proves full pipeline: count maintenance → stats read → cost model → plan selection → execution.
- [x] **P1.2 Complete QOV-based FieldValue migration.** Fixed in RFC-032 — all 10 `stripAlias*` calls deleted (8 NLJ rule, 2 PushFilterBelowJoinRule). Predicates now retain `FieldValue(QOV(correlationId), "column")`; filters use `PredicatesFilterPlanWithAlias` so the executor binds each row under its correlation alias and resolves via `evaluateCorrelated` — zero string manipulation. `executePredicatesFilter` binds the inner alias whenever present (was gated on params only). Root cause exposed: `PartitionBinarySelectRule` (Java inner-join rule) fired on LEFT OUTER joins, pushing nullable-side predicates pre-join; guarded with `JoinInner`. `mergeRows` string qualification untouched (operates on executor row maps, not planner Values — separate concern). All 46 targets pass; determinism verified.

### P2 — fix before scaling operations

- [x] **P2.1 Plan cache LRU is O(n) per hit.** Fixed in RFC-033 — replaced the slice-based LRU order tracking (linear scan + slice splice in `promote()` on every hit, under the lock) with a `container/list` doubly-linked list + `map[string]*list.Element`. Promote-on-hit/update and eviction are now O(1), matching Java's Caffeine-backed cache. `RWMutex` downgraded to `Mutex` (the read path always mutated the list, so the read lock was a lie). `BenchmarkPlanCache_HitLargeCache` confirms position-independent O(1) hits at maxSize=1024; all existing LRU-semantics tests pass unchanged + new interleaved-eviction test.
- [x] **P2.3 Intersection cursors don't resume mid-stream (codebase-wide).** Fixed in **RFC-071**. `DecodeIntersectionContinuation` (exact inverse of `buildIntersectionContinuation`) splits the per-child `IntersectionContinuation` proto into START/MID/END resume states; `executeIntersection` and `executeMultiIntersection` create each child cursor accordingly (fresh / resume-from-bytes / `Empty`) via the shared `buildIntersectionChildCursors`, then use `IntersectionResume`/`IntersectionMultiResume`. `started` is now tracked **per child** in `mergeChildState` (matching Java's `KeyedMergeCursorState`, not derived from cursor-level state) and seeded from the decode, so a resumed mid-stream child can't be re-encoded as START. The loud guard is dropped. Also fixed a latent continuation-timing bug the paged test caught: both cursors captured the continuation *after* the post-match advance, losing every other match on resume (`[2,4,6]`→`[2,6]`) — now captured before. Pinned by white-box paged-resume tests (no dup/loss, asymmetric exhaustion, no-common, 3-child, both cursors) + decode round-trip/error/nil tests in `intersection_resume_test.go`. Graefe+Torvalds+@claude+codex ACK (v1 NAK'd — Graefe caught a limit-before-first-row child silently terminating the intersection + held-match loss on out-of-band stops, driving the full Java `MergeCursorState` consume-model port; @claude caught `intersectionMultiCursor` returning bare END on a limit instead of checkpointing; codex caught a decode child-count validation gap for 1/2-child tokens). Surfaced by @claude + codex on PR #249; landed as PR #252.
  - [ ] **Follow-up (RFC-071, Go-only optimization beyond Java): skip re-scanning discarded non-matching rows on intersection resume.** Because the cached per-child continuation sits at the last *consumed* (matched) position (faithful to Java `MergeCursorState`), an out-of-band stop resumes a child from there and re-scans the non-matching rows discarded since its last match (bounded by the inter-match gap; the whole prefix-to-first-match for a never-matched child). Correct (no dup/no loss) and Java-faithful, but for very sparse intersections under a tight per-page limit the re-scan is wasted work and — pathologically — could fail to make progress within one page. Tracking the position just *before* the currently-held candidate (so resume re-reads only it) would eliminate the re-scan; this diverges from Java's model, so it's a Go-only read-path optimization, not parity. Flagged by codex on PR #252.
- [x] **P2.2 Operational debuggability.** Fixed in RFC-034 — `PlanGenerationLogger` hook (nil = silent) emits one `PlanGenerationInfo` per Cascades planning call: SQL (truncated, rune-safe), plan hash (`plans.PlanHash`), plan explain, planning duration, cache event (hit/miss/skip/inconclusive), cache size, slow-query flag, error. Go analog of Java's `RelationalLoggingUtil` + `PlanGenerator` finally block; wired into `planSelectCascades` (real query) and `planDML` via a shared `planLogScope` with a named-return defer. EXPLAIN re-entry suppressed via `logMetrics bool`. No scalar "estimated cost" — the Cascades cost model is a comparator, not a number (matches Java; logs plan hash + explain instead). Threshold default sourced from the canonical `api.OptLogSlowQueryThresholdMicros` (single source of truth); `OptLogQuery` left intentionally unwired (no SLF4J level concept in Go — handler owns level + sampling), documented at `options.go:55`. Sampling is the handler's responsibility. 11 unit tests + 2 FDB integration tests (DML Skip event, SELECT miss-then-hit through the public driver). Graefe ACK, Torvalds ACK.

---

## Stress comparison — RFC-173 Slice 2 W3b live flip (2026-07-02, same machine)

Master (`/tmp/fdb-master` worktree) vs branch `feat/rfc173-slice2-wedge` @ 5ead4e149 — the ordinal
wedge LIVE on every gated 2-way join. **No regression; branch faster on all heavy scans:**

| Subtest | master | branch (ordinal) |
|---|---|---|
| full_scan_count | 4.21s | 3.62s |
| order_by_pk_full | 4.72s | 4.12s |
| scan_all_narrow | 5.09s | 4.05s |
| scan_all_wide | 5.55s | 4.32s |
| full_scan_sparse_filter | 3.93s | 3.59s |
| join_10_outer | 0.02s | 0.04s (noise at this magnitude) |

## Stress test 1M baseline (2026-05-27)

**Run command:** `bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"`

**2026-07-25 (CQ-20 — PKScanOrdering drops the equality-bound PK prefix):**
baseline worktree at the pre-change commit `473d0d0af` vs the working tree,
same box, sequential fresh FDB containers, both runs `--nocache_test_results`.
All 23 subtests PASS on both sides. **Every EXPLAIN string and every
per-query row count identical** — diffed the stripped EXPLAIN/row lines (33
lines excluding the two bulk-load timing lines), empty diff. Total runtime
153.67s baseline vs 153.69s branch. Million-row scans baseline
3.51/3.22/3.36s vs branch 3.52/3.27/3.40s. Bulk loads noise-level equal
(customers 6.316s vs 6.368s; orders 129.68s vs 129.52s). Point lookups
4.96-5.01ms baseline vs 4.88-5.40ms branch, index equality 5.94 vs 5.68ms,
`in_list` 16.07 vs 14.97ms. The stress corpus contains no
Fixed-vs-Sorted-PK-scan-partitioning shape (no `WHERE pk-prefix IN (...)`
query mixed with an unbound scan of the same table in the same reference), so
this bounds collateral damage rather than measuring the fix; the fix itself
is covered by the yamsql `in_over_primary_scan_sarg` scenario against real
FDB and the corpus-wide `explaindiff` plan-shape diff (26/2633 changed, 13
shape flips, 0 regressions — see CQ-20/CQ-10f in the priority list above).

**2026-07-25 (CQ-10d — descending IN-union enumeration):** baseline worktree at
the pre-change commit `daad581f8` vs the working tree, same box, sequential
fresh FDB containers, branch run `--nocache_test_results`, re-run on the final
head. All 23 subtests PASS on both sides. **Every EXPLAIN string and every row
count identical** — the diff over all 35 measured lines, timings stripped, is
empty. Total runtime 153.13s baseline vs 155.55s branch. Million-row scans baseline
3.40/3.23/3.39/3.00s vs branch 3.50/3.26/3.41/3.03s. Bulk loads noise-level
equal (customers 6.296s vs 6.307s; orders 129.37s vs 129.39s). Point lookups
4.99-5.04ms baseline vs 4.81-5.00ms branch, index equality 5.71 vs 5.53ms,
`in_list` 16.33 vs 14.57ms. The largest single move is
`group_by_customer_having` (144.6 → 196.7ms), whose plan and 47271-row result
are byte-identical on both sides — a timing artefact, not a plan change. The
stress corpus contains no descending `ORDER BY` over an IN, so this bounds
collateral damage rather than measuring the fix; the fix itself is covered by
the yamsql scenario against real FDB. The pre-existing point-lookup threshold
caveat recorded under the RFC-176/P2 gate above still applies to this box.

**2026-07-25 (CQ-10c — criterion #6 credits a SARGed primary scan):** baseline
worktree at the pre-change commit `c8c32a1be` vs the working tree, same box,
sequential fresh FDB containers, both runs `--nocache_test_results`. All 23
subtests PASS on both sides with **identical row counts on all 22 measured
queries** and a **byte-identical EXPLAIN set** (10/10 lines; both diffs empty).
Total runtime 152.38s baseline vs 152.85s branch (+0.31%). Million-row scans
baseline 3.41/3.25/3.36/2.97s vs branch 3.31/3.27/3.37/2.96s. Bulk loads
noise-level equal (customers 6.262s vs 6.350s; orders 128.51s vs 128.95s).
Point lookups 4.83-5.51ms baseline vs 4.90-5.23ms branch, index equality 5.89
vs 5.59ms, `in_list` — the row this change is closest to — 16.27 vs 16.32ms.
Sub-second rows move within single-run noise in both directions
(order_by_pk_index_filter 10.59 vs 6.99ms, full_scan_filter 0.50 vs 0.57s) with
no order-of-magnitude shift. The stress corpus contains no
`WHERE pk IN (...) ORDER BY pk` query, so the plan change this item makes is
not exercised here; that shape is covered by the yamsql scenario instead, and
these numbers say only that nothing else regressed. NOTE: the pre-existing
point-lookup threshold caveat recorded under the RFC-176/P2 gate above still
applies to this box; these numbers do not re-qualify it.

**2026-07-25 (CQ-10a — comparison-granularity SARG fold):** no stress run.
Behaviour-neutral by measurement — 0 verdict changes in 48914 rung-6
evaluations across the SQL corpora, 112 of them with an intersection — and by
construction, since the comparison fold is a SUBSET of the alias fold, so it can
only move a verdict 0→1, and the one shape where it would (a record-typed IN,
which splits one binding across two comparands) does not exist in Go: the SQL
front end rejects multi-column IN outright. A run measuring a change that cannot
occur would report only box noise.

**2026-07-25 (CQ-10b — total-preorder primary-vs-index cost rung):** baseline
worktree at the pre-change commit `356ce2ab6` vs the working tree, same box,
sequential fresh FDB containers. All 23 subtests PASS on both sides with
**identical row counts** and a **byte-identical EXPLAIN set** (both diffs
empty). Total runtime 158.64s baseline vs 158.39s branch (-0.16%). Million-row
scans baseline 2.93/3.40/3.22/3.47/3.06s vs branch 2.90/3.28/3.16/3.38/2.95s.
Bulk loads noise-level equal (customers 6.315s vs 6.341s; orders 130.06s vs
130.15s). Point lookups 4.71-7.17ms baseline vs 4.73-7.34ms branch, index
equality 5.64 vs 5.84ms. Sub-second rows move within single-run noise in both
directions (full_scan_filter 0.63 vs 0.54s, group_by_customer_having 0.13 vs
0.19s) with no order-of-magnitude shift and no plan-shape change. NOTE: the
pre-existing point-lookup threshold caveat recorded under the RFC-176/P2 gate
above still applies to this box; these numbers do not re-qualify it.

**2026-07-25 (CQ-10 — antisymmetric IN-plan cost rung):** baseline worktree at
the pre-change commit `4fae8f215` vs the working tree, same box, sequential
fresh FDB containers. All 23 subtests PASS on both sides with **identical row
counts** and a **byte-identical EXPLAIN set** (verified by diffing the two runs'
EXPLAIN lines and row-count lines; both diffs empty). Total runtime 153.72s
baseline vs 152.39s branch (-0.87%). Million-row scans baseline
3.39/3.24/3.40/2.98s vs branch 3.44/3.23/3.38/2.95s. Bulk loads noise-level
equal (customers 6.356s vs 6.277s; orders 128.5-129.8s both sides). Point
lookups 4.73-5.36ms, index equality 5.66 vs 6.28ms, in_list 16.88 vs 15.81ms.
Sub-second rows move within single-run noise in both directions
(group_by_status 14.5 vs 24.3ms, group_by_customer_having 187.6 vs 175.5ms) with
no order-of-magnitude shift and no plan-shape change. The earlier
same-day run of the same branch content totalled 153.13s, bracketing the
baseline — consistent with noise rather than a directional effect. NOTE: the
pre-existing point-lookup threshold caveat recorded under the RFC-176/P2 gate
above still applies to this box; these numbers do not re-qualify it.

**2026-07-24 (RFC-190.5b — directional rich-order intersection parity):** the
recorded 190.5a checkpoint vs the current uncached run on the same host. All 23
subtests PASS with identical row counts. Total runtime is 151.71s checkpoint vs
150.95s current (-0.50%); comparable million-row scans are
3.38/3.29/3.40/2.99s vs 3.36/3.22/3.36/2.93s, respectively. Bulk loads remain
noise-level equal (customers 6.219s vs 6.240s; orders 127.736s vs 127.634s).
Point lookups are 4.76-5.17ms, index equality 6.19ms, and no throughput,
row-count, continuation, or plan-shape performance regression appears.

**2026-07-24 (RFC-190.5a — bounded generic k-way intersection reach):** exact
parent `bde66debe` vs working tree, same quiet box, sequential fresh FDB
containers. All 23 subtests PASS with identical row counts and byte-identical
EXPLAIN corpus (2,579/2,579). Total runtime 151.32s parent vs 151.71s branch
(+0.26%); million-row scans parent 3.40/3.22/3.41/2.96s vs branch
3.38/3.29/3.40/2.99s. Bulk loads were effectively identical (customers
6.224s vs 6.219s; orders 127.648s vs 127.736s). Early point probes on the
second fresh container showed the known 5–15ms floor jitter, while later
needle probes converged to 5.2–5.7ms on both sides; no sustained runtime,
throughput, row-count, or plan-shape regression.

**2026-07-22 (RFC-186 §2A-§2D — designated-final derivation, PK point-probe gate,
HintCost dispatch): back-to-back master worktree vs branch, same box, sequential
runs. All subtests PASS both sides; everything within single-run noise: full scans
master 3.15/3.40/3.66/3.13s vs branch 3.08/3.27/3.47/3.02s (branch equal-or-faster),
order_by_pk_full 3.40 vs 3.54s (+4%), point lookups/index-eq at the 10-30ms floor
both sides; index_amount_range 0.15 vs 0.23s (+53%) and group_by_customer_having 0.13 vs
0.20s (+54%) — small-absolute sub-second deltas from SINGLE runs each side, so
noise vs signal is NOT demonstrated for those two rows; no order-of-magnitude
shift anywhere and no plan-shape change in the suites. FOLLOW-UP: re-measure
those two subtests (3× each side) before reading them as a §2 effect.**

**2026-07-17 (RFC-180 D2+I3 — winner map deletion, requested-ordering retention,
root-operator rule index, exact integer comparators): branch (HEAD 0aea06b48),
all subtests PASS — every metric BEATS the recorded bands: full_scan_count 3.16s,
order_by_pk_full 3.29s, scan_all_narrow 3.31s / _wide 3.47s, sparse_filter 2.94s,
needles/in_list ~10ms. The ~85% planner task-count reduction (rule index) shows up
as faster end-to-end planning; no regression anywhere.**

**2026-07-19 (RFC-183 — fully-linked plans at rule time, memo-linkage repairs):**
baseline master vs branch, BOTH re-run on a quiet machine after the first
comparison proved confounded. Aggregate and join paths improved substantially;
everything else within +/-5% noise:

    SUM by status (aggregate index)   24.28ms -> 5.51ms   -77%
    GROUP BY status                   12.62ms -> 6.13ms   -51%
    GROUP BY status COUNT only        10.40ms -> 5.08ms   -51%
    JOIN 10 orders x customers        26.58ms -> 14.18ms  -47%
    GROUP BY customer HAVING         183.7ms -> 130.3ms   -29%
    PK lookups / scans / ORDER BY     within +/-5%

**MECHANISM RETRACTED — the speedup is NOT attributable to this branch.**

I originally recorded a mechanism here: AggregateDataAccessRule built its
two-leg multi-intersection with NIL quantifiers, so the memo could not cost the
aggregate-index legs, and that was "exactly the surface that moved". Both
reviewers independently refused it on the same ground: a costing fix changes
plan CHOICE, so if the winning plan is unchanged, no costing difference can
make execution 4x faster — and this branch reports ZERO plan drift. The two
claims were in tension and I did not notice.

Checked instead of argued. EXPLAIN, master vs branch, using the stress schema
INCLUDING its three aggregate indexes (my first attempt omitted them and so
compared the wrong plans entirely):

    SUM(amount) GROUP BY status   AggregateIndex(SUM, SUM_AMOUNT_BY_STATUS, [STATUS], ORDERS)
    COUNT(*)    GROUP BY status   AggregateIndex(COUNT, COUNT_BY_STATUS, [STATUS], ORDERS)
    SUM ... GROUP BY customer_id  PredicatesFilter(AggregateIndex(SUM, SUM_AMOUNT_BY_CUSTOMER, ...))

BYTE-IDENTICAL on both sides. The plans do not change, so the latency delta is
environment — caching, container warmth, run-to-run variance — not this work.
A second branch sample reproduces ~5.5ms for SUM, so the BRANCH side is stable;
there is exactly one master sample and it is the outlier.

WHAT THIS COMPARISON ACTUALLY ESTABLISHES: no regression. Nothing here supports
a performance claim, and the 2-4x table above must not be cited as one.

The FIRST attempt at this comparison showed a uniform 30-90% SLOWDOWN across
every query including full scans. That was pure machine load — the branch run
overlapped a full Bazel suite with FDB containers. Load average was still 11.75
when it looked "done"; instantaneous CPU idle (90%+) is the signal that
actually matters. Discarded rather than reported.

**2026-07-05 (RFC-173 item-1 commit 1 + fix round, PR #481 — dup-alias binding-id
mint + binding-keyed seed, dark):** baseline master `8c179a025` 161.68s total vs
branch `7f0f6848e` 165.14s (noise; branch equal-or-faster per metric: full scans
4.03–4.35s vs 4.28–4.49s, sparse filter 3.58s vs 3.77s, needles 5.6/6.3ms vs
5.4/7.7ms, IN-list 14.6ms vs 14.9ms). All 23 subtests PASS both sides. No
regression.

**2026-07-05 (RFC-173 item-1 c2+c3 — the front-end flip + SELECT-* star layout):**
branch (item-1 lift) 171.41s total, all 23 subtests PASS. Every metric within the
c1-baseline band (full_scan_count 3.75s, order_by_pk_full 4.60s, scan_all_narrow
4.13s / _wide 4.44s, sparse_filter 3.63s, group_by ~5ms, join_10 0.05s). The change
only adds a column-metadata derivation arm for duplicate-alias `SELECT *` (no
plan-shape / cost change for non-dup queries). No regression.

**2026-07-06 (RFC-173 item-3 c1+c2 — mixed-nesting LEFT widening: box roots + boxes as
legs): branch 154.35s total, all 23 subtests PASS — FASTER than the master baseline
(161.68s). Key metrics: full_scan_count 3.38s, order_by_pk_full 4.07s, scan_all_narrow
4.01s / _wide 4.28s, sparse_filter 3.55s, needles/in_list ~10-20ms. No regression.

**2026-07-06 (RFC-173 item-3 c4 — stranded-correlation keystone + review batch:
GetCorrelatedTo own-alias filter, SelectMerge surgical box-ref translation, twin
Legs alignment, ofOrdinal nullability flow): branch 160.51s total, PASS — faster
than the master baseline (161.68s), within noise of the c1+c2 row (154.35s). No
regression.

**2026-07-06 (RFC-173 unnest-residual c1 — box-leg owners gather, multi-segment
struct paths, SelectMerge/Explode arm, struct-column schema surface): branch
156.73s total, PASS — faster than the master baseline (161.68s); the NAK-round fix HEAD (proto-converter unification on the executor hot path) 160.53s, still clean. No regression.


**2026-07-06 (RFC-173 item-1 c4 — the review-round fixes: binding-keyed sort/group
keys, buried-EXISTS rebase, fold binding correlations):** branch 161.84s total, all
23 subtests PASS — equal to the master `8c179a025` baseline (161.68s) and faster
than the c2 run (171.41s). Key metrics inside every band: full_scan_count 3.72s,
order_by_pk_full 4.10s, scan_all_narrow 4.06s / _wide 4.34s, sparse_filter 3.61s,
needles/in_list ~10ms. No regression.

**2026-07-05 (RFC-173 item-2 commit 5b, PR #480 — cluster-gate rider transparency):**
baseline master `4f836f941` 156.14s total vs branch `bd802e83d` 156.32s (+0.1%, noise);
every subtest within measurement resolution (pk lookups 10–60ms both, index equality 20ms,
full scans 3.6–4.4s both sides). No regression.

| Query | Rows | Time | Threshold |
|-------|------|------|-----------|
| pk_lookup_first | 1 | 1.5ms | <5ms |
| pk_lookup_middle | 1 | 1.5ms | <5ms |
| pk_lookup_last | 1 | 1.7ms | <5ms |
| index_customer_eq (8 rows) | 8 | 9.1ms | <10ms |
| index_amount_range (100K rows) | 100017 | 196ms | |
| index_status_count | 1 | 362ms | |
| full_scan_count | 1000000 | 3.1s | ~3s/1M |
| full_scan_filter | 1 | 534ms | |
| group_by_status | 4 | 5.25s | |
| group_by_status_count_only | 4 | 1.9ms | |
| sum_by_status | 4 | 2.0ms | |
| group_by_customer_having | 47271 | 107ms | |
| join_10_outer | 10 | 4.1ms | |
| order_by_pk_full (1M rows) | 1000000 | 3.33s | ~3s/1M |
| order_by_pk_index_filter | 8 | 3.4ms | |
| scan_all_narrow (1M rows) | 1000000 | 3.33s | ~3s/1M |
| scan_all_wide (1M rows) | 1000000 | 3.66s | ~3s/1M |
| in_list | 46 | 10ms | <10ms |
| needle_in_haystack_pk | 1 | 2.0ms | <5ms |
| needle_in_haystack_filter | 1 | 2.4ms | <5ms |
| full_scan_sparse_filter | 97 | 3.0s | ~3s/1M |
| update_by_index | 8 | 4.0ms | |
| delete_single_row | 1 | 2.3ms | |

All 23 subtests PASS. Total: 170.7s (incl. bulk insert ~2:28).

---

## RFC-182 — generative row-soundness differential (2026-07-18 audit follow-ups)

P1 SHIPPED (branch rfc182-row-soundness): rowdiff harness + `cmd/sql-diff-stress` +
smoke; acceptance recorded in RFC §11 (500 pre-fix seeds → 23 catches, fixed tree
clean). The enabling wrong-rows fix (pk-intersection residual compensation +
adjusted-MaxMatchMap reader) landed in the same branch, pinned by
`TestFDB_IntersectionResidualCompensation` + corpus dimension.

- [x] **RFC-182 P2 (grammar half)** — IN/BETWEEN/LIKE/IS NULL leaves, LIMIT
  (§3 membership rule), SELECT DISTINCT, nullable sort keys + NULLS
  FIRST/LAST. Found 5 pre-existing bugs (RFC §12 table); 4 fixed + pinned,
  1 gate-pinned (see the nested-IN item below).
- [ ] **RFC-182 P2 (remainder)** — continuation-resume dimension, the
  minimizer + yamsql draft emitter (§6), the forced-alternative mode
  (disable dominant plan families so losing memo alternatives get
  row-checked), non-correlated EXISTS/IN if feasible.
- [x] **`LogicalProjectionExpression` identity ignores its output ALIASES**
  — closed by CQ-2 above. Both logical and physical projection identity now
  includes executor-visible output names, with memo non-collapse tests and
  alias-preserving elimination regressions. `NewRecordQueryProjectionPlan`
  remains for API/test compatibility, but now defensively copies its projection
  slice and shares the same immutable, schema-aware physical identity
  implementation.
- [ ] **Nested IN over an intersection extracts `InJoin(<nil>)`** (RFC-167
  shell instance). `WHERE b IN (…) AND c = ? AND a IN (…)`: the inner IN
  wrapper is never handed to WithChildren, so no per-wrapper relink reaches
  its nil-inner snapshot. PRE-EXISTING on master and LOUD (XX000, never
  wrong rows). Gate-pinned by
  `TestNestedIn_OverIntersection_GatePin` — that pin goes RED the day the
  planner learns the shape, and the author then replaces it with real
  plan + row assertions.
- [ ] **RFC-182 P3** — Oracle J (Java `runSql` via plandiff runner), java-compat
  profile, three-way triage, INFRA/DECLINE/MISMATCH signature ledger
  (Go=M/Java≠M entries ONLY — Go≠M always fails), nightly target; REQUIRED:
  extract the shared decline/divergence authority with the A3 skip list.
  Subsumes/coordinates with RFC-164 WS-1 (generative Go-vs-Java differential).
- [ ] **RFC-182 P4** — DISTINCT, GROUP BY/aggregates (Oracle M reimplementation
  honesty note in §7 — Java-first coverage), joins incl. correlated EXISTS/IN,
  vector/rank K>1 shapes. Nightly-scale empty-required-family bucket = hard fail.

Audit findings (2026-07-18 quality audit) still open, in recommended order:
- [ ] `plan_visitor.go:1116/:1133/:1156` discard `upgradeFirstFilter`'s success
  bool and `:1153` returns the unfiltered op when no predicate builder succeeds —
  make correct-or-loud; surface `planErr` at `cascades_generator.go:411`.
- [ ] `physical_vector_index_scan_wrapper.go:36-38` returns empty correlation
  (all 34 sibling wrappers propagate); memo leaf criterion `memo.go:522-542`
  requires ALL members quantifier-free where Java's Traversal uses ANY.
- [ ] DIVERGENCES.md ledger corrections: windowed candidate is DEAD (zero
  constructors), not "Aligned"; compensation-intersect note is now partially
  wired via the RFC-182 fix. The stale cost-model "16 criteria" table was
  corrected by RFC-190.
- [ ] Matcher arc (Graefe audit findings 1-3, one RFC): dependency-aware alias
  matcher (`AliasMap.findCompleteMatches`), MatchIntermediate permutation
  enumeration, retire the phantom `WithSwappedQuantifiers` double-fire.
- [ ] `ForMatchCompensation.Intersect` early-returns Impossible when either
  input is impossible; Java's WithSelectCompensation.intersect recomputes (an
  impossible function on a NON-shared predicate drops out and the fold can be
  possible — Compensation.java:762 case 2). Conservative: missed plans, never
  rows. (Graefe impl-review finding, rfc182-row-soundness.)
- [ ] Rule-registration hygiene: `RemoveRangeOneRule` dead in Java but registered
  in Go; `DecorrelateValuesRule` double-registered; `OrderedPrimaryScanRule`
  zero tests; `PredicateToLogicalUnionRule` REWRITING-vs-PLANNING phase.
- [ ] ~750 LOC verified-dead code sweep (windowed candidate, `in_source.go`,
  `rule_demorgan.go`, `IntersectionInfo` island, `derivations_evaluator.go`
  computed-never-read).

## Phase 8: Planner architecture cleanup (Graefe review findings)

### 8.1 Evaluate `pushDataAccessTasks` as CascadesRule — RESOLVED (keep procedural)

Graefe flagged this as procedural code that should be a rule. After investigation: **the procedural approach is architecturally correct.** `pushDataAccessTasks` operates on Reference-level PartialMatch state, not expression types — CascadesRules require expression-type pattern matching. Java uses explicit `TransformMatchPartition` task types for the same reason: this is task-level logic, not rule-level. Go's direct method call in `ExploreExprTask.Run()` is simpler and equivalent. No change needed.

### 8.2 Verify `promoteByDataAccessCost` heuristic absorbed — VERIFIED

`promoteByDataAccessCost` was deleted in eb94291a (dead code cleanup). Its heuristic (prefer lower-cardinality data access) IS absorbed into `PlanningCostModelLess` at `planning_cost_model.go:191–208` — Criterion #2: `maxDataAccessCardinality`, lower wins. This fires via `stampOrderingWinners(ref, costModel)` after every data access insertion. The cost model uses the same `findExpressionsByType` + `maxDataAccessCardinality` comparison. No heuristic was dropped.

### 8.3 Document `maxRoundsPerRef = 10` cap — DONE

Added comment at `unified_tasks.go:59` explaining: prevents divergence from rule cycles (A→B→A) that produce distinct-but-equivalent members. Java relies on memo dedup for fixpoint; Go's per-Reference dedup is weaker, so pathological rule interactions can produce new members indefinitely. 10 rounds >> typical 2–3 needed, safely under MaxTasks budget.

---

## Phase 7: Cascades alignment — close remaining Java divergences

### 7.1 Unify alias namespaces — DONE

Quantifier aliases now match table aliases at creation. Three band-aids removed: `rightAliasSet`, `planContainsJoin`, `collectPlanAliases` (−114 lines). Root-cause fix in `mergeRows`: bare inner keys overwrote qualified keys from nested joins (missing `!exists` guard). 46/46 tests, 15/15 determinism.

### 7.2 Port matching infrastructure for index intersections — DONE

`IndexIntersectionRule` deleted (Go-only REWRITING-phase rule). Replaced with match-based PLANNING-phase intersection via `WithPrimaryKeyIntersector` in `intersector_primary_key.go`. Wired into `pushDataAccessTasks` with guards: candidate cap (4), match cap (8), restricted-scan filter, idempotency via `hasIntersectionFinal`. Two regressions found and fixed: IS NULL correctness (zero-coverage matches created incorrect intersections, fixed by filtering to restricted scans only) and MaxTasks (intersection logic ran N times per Reference, fixed by idempotency guard). 46/46 tests, 10/10 determinism.

### 7.3 Convert remaining predicateReferencesAlias sites — DONE

All 8 `predicateReferencesAlias` calls in the NLJ rule converted to `GetCorrelatedToOfPredicate` correlation-set checks. Function deleted. Root-cause fix: `qualifyBareFieldValue` in EXISTS builder now produces QOV-based FieldValues instead of flat strings. `walkPredicateFieldValues`/`fieldValueAliasAndCol` survive in push-filter/push-projection rules (handle both QOV and flat FieldValues for unit test compatibility).

### 7.4 FlatMap wrapper correlation propagation — NOT NEEDED (Graefe confirmed)

Graefe confirmed: `GetCorrelatedToWithoutChildren()` returning empty is correct for BOTH joins AND correlated subqueries. Correlations flow through quantifier children in both cases. `JoinMergeResultValue.Children()` does NOT need QOV nodes.

For correlated scalar subqueries (Go-only extension, Java rejects at grammar level), the correct Cascades architecture is:
1. `ForEachNullOnEmpty` quantifier (already exists: `ForEachNullOnEmptyQuantifier`)
2. `RecordQueryFirstOrDefaultPlan` with NULL default (already exists)
3. Correlated `BuildScalar` fallback (needs: full inner plan with outer scope, correlation predicate extraction)
4. NLJ rule: detect NullOnEmpty → wrap inner with FirstOrDefault + FlatMap

NLJ wrapper correlation propagation (walks predicates) is already correct and active.

### 7.5 + 7.6 (HOLISTIC — RFC-077): Source-anchored join result + structural interning

**Bundled per maintainer decision (2026-06-04):** 7.5 (structural interning key) and 7.6
(source-anchored field pull-up) are two facets of ONE change — retire the opaque, name-keyed
join-merge apparatus (`JoinMergeResultValue`/`JoinMergeAllValue`, `composeFieldOverJoinMerge`,
the string `mergeQuantifierAlias`) for **anchored access**: the translator + re-enumeration emit
`RecordConstructorValue` of `FieldValue(QOV(legAlias), col)`, resolved by the existing
`composeFieldOverConstructor`. RFC-073 GATED 7.6 on 7.5 (a circular "anchor only the binary join =
split-brain"); doing them as one migration breaks that deadlock, and **7.5's structural interning
falls out for free** — the anchored RC is canonical (one type, alias-set-keyed), so it interns
structurally via RFC-039/040 `MemoEqual`, retiring the synthetic string `mergeQuantifierAlias`
(measured load-bearing today *because* the merge is opaque; anchoring removes that).

**Design unlock (RFC-077):** Go's `RecordConstructorValue.Evaluate` produces a NAME-keyed map
(`values.go:2148`), so Go uses **name-based anchored resolution** — NOT Java's full ordinal-substrate
machinery (`FieldValue.ofOrdinalNumber`). Smaller, cleaner, Go-adapted (the sanctioned
"diverge when strictly better + clean" path). `composeFieldOverConstructor` simplifies field
accesses at plan time so the RC rarely survives to runtime; consumers reading the old
bare+`ALIAS.COL` keys (`cascades_generator.go:1890` column derivation, `executor.go:1434 mergeRows`,
`streaming_cursors.go`) move to the anchored RC's field keys. This addresses Torvalds' RFC-073
NAK (the Evaluate-shape change) via the name-keyed-map + compile-time-simplification design.

7.5/7.6 history (the prior split, RFC-073's deferred analysis, the Graefe direction + Torvalds NAK)
is preserved in `rfcs/073-source-anchored-join-result.md`; RFC-077 supersedes it as the holistic
plan.

**Status update (2026-06-05):** F3 split the bundle (Graefe ruling: 7.5 now, 7.6 deferred on column
threading). 7.5 IMPLEMENTED — and the documented root-cause was CORRECTED by an implementation spike:
the interning was NOT defeated by an alias-sensitive candidate-narrowing hash (the hash is already
alias-invariant, RFC-074; `memoizeNonLeaf` already uses alias-aware `MemoEqual`). The real
alias-sensitive sites are `Reference.Insert`/`InsertFinal`, which dedup alias-IDENTITY only — a
Go-vs-Java divergence (Java's `containsInMemo` is alias-aware). Fix: a GATED alias-aware `MemoEqual`
dedup tier in `Insert`/`InsertFinal`, opted into via `SelectExpression.InternsAliasAware()` (merge
re-enumeration selects only — gating avoids over-deduping CTE column-rename selects, which silently
read NULL when collapsed because Go's column derivation resolves some references by quantifier-alias
IDENTITY, unlike Java's ordinal/group model; this is the RESOLUTION-model axis, NOT alias-namespace
naming, which 7.1 already unified). `mergeQuantifierAlias` +
`mergeAliasPrefix` deleted; the merge quantifier now gets a plain `uniqueId`. Verified by a
deterministic chain task-count gate (±2%, pinned 3-chain 8999 / 4-chain 30593; naive un-gated uniqueId
DOUBLES the 4-chain to 60044) + full suite green + 5× determinism. The opaque-type retirement
(JoinMergeAllValue/Seed/composeFieldOverJoinMerge) and anchored RC remain 7.6, deferred on column
threading (F3). See RFC-077 "Precise root-cause — CORRECTED".

**7.6 DONE (2026-06-05, RFC-077 v4):** column threading landed in the 7.6 core (#259); this follow-up
(a) anchors EVERY reachable join-leg shape — correlated scalar subqueries (incl. dotted scalarCol),
derived tables / aggregate subqueries / CTE references as join legs, recursive-CTE legs (outer +
recursive-branch self-reference), Sort/Distinct/Union/Aggregate legs — and (b) DELETES the opaque
`JoinMergeAllValue`/`JoinMergeSeedValue`/`Seed`/`composeFieldOverJoinMerge`, migrating all consumers
to the source-anchored `RecordConstructorValue`. Decisive root-cause: the core's `tableColumns` was
case-SENSITIVE while the SQL path upper-cases table names, so the core's anchoring was DORMANT
(`resolveRecordType` now case-insensitive). Proven no-fallback by a panic-probe over the entire SQL
production surface; chain budget gate unchanged (anchored interns identically); plandiff
byte-identical. See RFC-077 v4.

- [x] **7.5 + 7.6 (RFC-077) — DONE.** 7.5 merged (#258), 7.6 core merged (#259), 7.6 retirement
  (anchor-all + delete opaque types) on `feat/7.6b-retire-opaque-merge`.

### 7.7 Retire `ImplementIndexScanRule` — unify on the data-access/`Compensation` path (RFC-045 follow-up)

- [x] **DONE (RFC-076 v5, 2026-06-05).** `ImplementIndexScanRule` + both registrations + its 3 test
  files deleted; shared helpers extracted to `scan_match_helpers.go`. Sequence: 3b template-aware
  costing → 3a constraint-pass activation + stub-chain costing → deletion + **data-access compensation
  materialization** (the v3/v4 premise missed that the data-access path never materialized its residual
  `Compensation.apply` LOGICAL filter into a physical plan during PLANNING, so the index scan was
  dropped to a full scan for the indexed-eq + non-indexed-residual shape; `pushDataAccessTasks` now
  realizes the unambiguously-safe simple residual as a physical filter, guarded against IN / correlated
  / index-only / vector-or-aggregate-inner / join-leg shapes — see `isSimpleResidualCompensation` +
  `refHasCorrelatedMatch`). `validateNoIndexOnlyResidual` KEPT (still load-bearing). Full suite green,
  plandiff byte-identical, determinism 5×. The data-access/`Compensation` path is now the sole scan
  producer, as in Java. Original analysis retained below.
- [ ] **Follow-up (Graefe v5 ACK condition): replace the `isSimpleResidualCompensation` allowlist with
  Java's exploratory-yield re-optimization.** Java yields data-access compensations via
  `FinalYields.yieldUnknownExpression` — a non-`RecordQueryPlan` lands in the *exploratory* set and is
  re-optimized by the normal PLANNING loop, so EVERY compensation shape is realized uniformly. Go's
  `pushDataAccessTasks` only `InsertFinal`s, so `implementDataAccessCompensation` + the
  `isSimpleResidualCompensation` allowlist stand in for that primitive. The allowlist is correct and
  each exclusion is pinned today, but it will rot the moment a new compensation shape appears with no
  allowlist arm (falls through to the dead-final-member path → silent no-plan). The honest fix is a Go
  `yieldUnknown`/exploratory-insert that re-optimizes all compensations and shrinks the allowlist to
  nothing — BLOCKED on Go's compensation re-optimization correctly handling IN-explode / correlated /
  index-only shapes (a naive exploratory-insert re-breaks them today, which is why the allowlist exists).

Go reaches a physical index scan / filter via THREE producers that bypass `Compensation`: the
data-access/compensation match path (`predicate_multi_map.go`), the Go-only `ImplementIndexScanRule`
(a fusion of Java's `ImplementPhysicalScanRule` + candidate matching that iterates predicates
directly), and `ImplementFilterRule` (synthesizes a `RecordQueryPredicatesFilterPlan` over the inner
winner). Java has ONE path (`AbstractDataAccessRule` → `toEquivalentPlan`) and enforces "index-only
value can't be a residual" ONCE via `Compensation.isImpossible()`. Because Go's extra rules don't
route through `Compensation`, RFC-045 enforces the index-only compensatability guard at multiple
layers: `valueContainsUncompensatable` (match path) + the residual-skip loop in
`ImplementIndexScanRule.OnMatch` (implement-index path) + a final-plan validation
`validateNoIndexOnlyResidual` in `Planner.Plan` (the `ImplementFilterRule` leak can't be guarded at
the rule — removing its member collapses the filter Reference and breaks the data-access intersection
memo, so the leaking *final* plan is rejected with `UnplannableIndexOnlyResidualError` instead).
All are load-bearing and pinned (`TestVectorPlan_QualifyPlansToVectorScan`,
`TestImplementIndexScanRule_SkipsIndexOnlyResidual`, `TestVectorPlan_MetricMismatchDoesNotMatchVector`),
so there is **no live bug** — but the layering is a smell whose root is the duplicated paths. Root fix
(Graefe-endorsed): retire `ImplementIndexScanRule` and route `ImplementFilterRule`'s filter
implementation through a single data-access rule backed by `Compensation`, at which point the
implement-layer guard AND the final-plan validation delete themselves and the property is enforced
once, as in Java. See DIVERGENCES.md "ImplementIndexScanRule is a Go-only second index-scan path".
  - **RFC-076 v3 ACK'd (Graefe + Torvalds), committed `75bf8d17`. v2's leaf-matching diagnosis was
    FALSIFIED by empirical reproduction.** Disabling `ImplementIndexScanRule` + tracing shows the
    match infra fires correctly (leaf scan↔scan `EqualsWithoutChildren=true`; `matchSingleSourceAgainstSelect`
    binds the predicate to the candidate Placeholder; `pushDataAccessTasks` fires) — the gap is that
    every seed-match path builds its MatchInfo with `maxMatchMap=nil`, so `PartialMatch.PullUp`
    (`partial_match.go:117`) returns nil → `CompensateCompleteMatch` → `ImpossibleCompensation` →
    `DataAccessForMatchPartition` skips → ZERO scans. `ImplementIndexScanRule` is the SOLE producer.
    `ComputeMaxMatchMap` (`max_match_map.go:167`) exists but is never called by the seeds.
  - **WIP STASHED (`git stash list` → top of stack on this branch).** Implemented the data-access
    completion per the Graefe-confirmed Java recipe: wire `ComputeMaxMatchMap` into the seed paths
    (leaf uses an identity map over the candidate result value; intermediate uses query/candidate
    result values + `NewAliasMapValueEquivalence`), residual compensation (re-apply unmatched
    predicates as filters via `OfPredicateCompensation` — Java produces the match even when fully
    residual), an IN-sargable guard (an IN comparison is NOT a contiguous range — leave it to the
    explode/InJoin path), and per-ref `AdjustPartialMatchesForRef` in `pushDataAccessTasks` (matches
    are seeded in PLANNING exploration, after the dead phase-start `AdjustMatches`, so ordering parts
    are only computed at consume time). **Validated:** full cascades unit suite GREEN with the rule
    enabled; 12/16 cited shape tests green with the rule disabled.
  - **REMAINING (multi-shift, per-feature vs Java — bigger than v2 stated):** broad `just test`
    exposes that the new (Java-correct) matches diverge from the rule's plans: (1) Go cost/Pareto
    pruning lets a non-unique index beat the unique one + breaks index intersection (`plangen`
    `UniqueIndexPointLookupPreferred`, `EndToEnd_IndexIntersection`); (2) `wrapScanPlanWithCoverage`
    (`abstract_data_access_rule.go:345`) doesn't propagate the candidate `unique` flag that
    `OrderedIndexScanRule` sets; (3) vector index-only-residual: a metric mismatch no longer raises
    `UnplannableIndexOnlyResidualError` (4 `TestVectorPlan_*`); (4) **DELETE over-deletes** →
    `TestFDB_DeleteOldAndLowValue` panic (correctness); (5) sort-elim ordering parts now computed but
    the satisfaction→ordered-scan→`RemoveSort` chain is incomplete (4 `TestSortElim_*`); (6) covering
    full-index-scan vs table scan (`TestPlanHarness` covering/range). Grind each rule-disabled,
    red-first, aligned to Java/plandiff; do NOT one-off guess (a `boundCount==0` guard diverged from
    Java and broke a Java-aligned unit test). THEN retire the rule + guard + final-plan validation.
    `ImplementFilterRule` STAYS (faithful Java port). Separate PR from RFC-077.
  - **RFC-076 v4 (2026-06-04): step 1 DONE (5 correctness fixes, Graefe+Torvalds ACK), full retirement
    in progress.** The data-access path is now correct for every FDB-tested shape (dual-correlation
    joins, simple joins, aggregate eq-filter, vector residuals). Full rule retirement needs: (3a)
    activate the dormant ordering-constraint pass (`constraintOnly` never set true → `PushRequestedOrderingThrough*`
    inert); (3b) template-aware costing (a nil-inner `Fetch` shell hides its inner from the cost model
    → join-order flip on `TestFDB_JoinSelPred_Repro`). See RFC-076 "v4 amendment" for the sequenced plan
    + the ref-resolving (not magic-constant) 3b. `validateNoIndexOnlyResidual` STAYS (now load-bearing
    via the DistanceRank residual). **Step-2 cleanup TODO (file/do during retirement, by the retirement
    PR): stop SEEDING `AggregateIndexMatchCandidate` partial matches onto non-GroupBy refs** in the
    leaf/intermediate match rules, so the agg-skip type-switch — currently duplicated 4× (`planner.go:465`
    data-access boundary [new], `rule_implement_index_scan.go` [dies with the rule], `rule_streaming_agg_from_index.go`,
    `rule_aggregate_data_access.go`) — collapses to one. Torvalds flagged the boundary guard as a
    defensible transition shim, NOT the permanent design; the don't-seed fix is the root cause.

### 7.6 — MERGED into 7.5+7.6 (RFC-077)

7.6 (source-anchored field pull-up / retire `composeFieldOverJoinMerge`) is no longer a separate
item: it is the same change as 7.5 (anchored RC retires the opaque merge → structural interning
falls out). See the holistic **7.5 + 7.6 (RFC-077)** entry above. RFC-073's deferred analysis is
the historical record.

---

## Phase 9: Vector / HNSW relational SQL parity (RFC-045)

**Context.** The record-layer / Cascades core of vector search is already ported and FDB-tested:
the HNSW graph (`hnsw.go`), the index maintainer (`vector_index_maintainer.go`), RaBitQ
quantization (`pkg/rabitq`), HNSW stats (`hnsw_stats.go`), `vec_math.go` / `fht_kac_rotator.go`,
chaos verification (`chaos/verify_vector.go`), and integration tests
(`vector_index_test.go`, `rabitq_test.go`, `hnsw_stats_test.go`, `bench/sift_benchmark_test.go`).
The Cascades *values* (`value_row_number.go` + `value_*_distance_row_number.go` seeds,
`value_row_number_high_order.go`), the match candidate (`vector_index_match_candidate.go`, 232 LOC),
and a `DistanceRank` comparison stub all exist. The SQL **grammar** is complete:
`vectorIndexDefinition` (`CREATE VECTOR INDEX … USING HNSW … PARTITION BY … OPTIONS(…)`),
`qualifyClause`, `overClause`, `windowSpec`, `nonAggregateWindowedFunction(ROW_NUMBER …)`.

**The gap = the relational front-end + Cascades wiring** (the "just not relational bits"):

**Status: DONE (RFC-045, Graefe+Torvalds ACK).** 9.1–9.4 all landed, tested, green. The full
SQL vector K-NN read path works end-to-end: a partitioned HNSW index +
`SELECT … WHERE <partition> QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY
euclidean_distance(vec, q)) <= K` plans to a BY_DISTANCE vector index scan and executes
against real FDB returning the k nearest records (`TestFDB_VectorSearch_QualifyE2E`). Also
fixed a latent vector-scan PK-extraction bug. **Known follow-up:** an *unpartitioned* vector
index + WHERE-less QUALIFY does not yet match the candidate (Java's vector search is always
partitioned) — fails to plan rather than returning wrong results; revisit if needed.

- [x] **9.1 DDL: `CREATE VECTOR INDEX … USING HNSW … PARTITION BY … OPTIONS(…)`** → metadata vector
  `Index` (type `vector`, HNSW options). No `vectorIndexDefinition` handler exists in `pkg/relational`
  today. Wire-compat: the index metadata + on-disk HNSW format must match Java byte-for-byte (core
  already does; DDL must produce the same `Index` proto + options).
- [x] **9.2 Query front-end: `QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY <distance>(vec, q)) <= K`.** Done — walk.go builds DistanceValue + RowNumberValue; predicates.TransformRowNumberDistanceRankMaybe ports transformComparisonMaybe; QUALIFY lowers to a DistanceRank ComparisonPredicate.
  No `qualifyClause` handling and no window-function→Value visitor exist (`grep QualifyClause` → 0 hits;
  `extractFunctionNameFromCall` only returns the *name* string). Build the distance-specialized
  `RowNumberValue` (Euclidean / Cosine / Dot-product / EuclideanSquare) from the parse tree, fleshing
  out the seed value classes; port `RowNumberValue.transformComparisonMaybe` so `ROW_NUMBER() <= K`
  rewrites into a `DistanceRankValueComparison(queryVector, k, efSearch, isReturningVectors)`.
- [x] **9.3 Cascades wiring + vector physical plan.** Done — (9.3a) tryVectorIndexCandidate enumerates the candidate + ExpandVectorIndex builds the distance placeholder + valuesMatchColumn matches it; (9.3b) ToScanPlan splits partition prefix from the DistanceRank binding; (9.3c) RecordQueryVectorIndexPlan + executeVectorIndexScan dispatch BY_DISTANCE; physicalVectorIndexScanWrapper + the index-only compensatability guard (valueContainsUncompensatable via values.IsIndexOnly on the match path + the residual-skip loop in ImplementIndexScanRule) make the vector scan the sole physical winner — the DistanceRank predicate, being index-only, is never lowered to a residual filter, exactly as Java's match-then-implement does. Three pieces (Torvalds catch — not a single
  branch): **(9.3a)** add a vector branch to the match-candidate enumeration (next to
  `NewValueIndexScanMatchCandidate` at `plan_context_builder.go:46` + the metadata-driven builder in
  the embedded layer) so a `vector`-type index yields the candidate; **(9.3b)** rework
  `VectorIndexScanMatchCandidate.ToScanPlan` (`vector_index_match_candidate.go:200`, today a generic
  `NewRecordQueryIndexPlan`) to split partition-equality `ComparisonRange`s from the single
  distance-rank comparison (which rides as an *equality-shaped* range, à la Java
  `toVectorIndexScanComparisons`); **(9.3c)** introduce a vector-aware physical plan that threads
  query-vector/k/`ef_search`/`isReturningVectors` and at execution dispatches **BY_DISTANCE** via
  `ScanIndexByType`/`ScanVectorIndex` → `ScanByDistance` (`index_scan.go:338-345`) — without it the
  plan does a BY_VALUE scan that errors at `index_scan.go:269`.
- [x] **9.4 E2E proof.** Done — `TestFDB_VectorSearch_QualifyE2E` (sqldriver, real FDB): builds a partitioned vector schema, inserts vectors, EXPLAIN-pins the BY_DISTANCE vector scan for the full QUALIFY SQL query, executes it, and asserts the top-2 nearest records. (yamsql port + `ef_search`/OR-of-two-KNN/`42F21`-in-WHERE coverage remain as nice-to-have follow-ups.) Original plan: Port Java's `window-function-documentation-queries.yamsql` (KNN top-K via
  `QUALIFY`, `ef_search` option, `<`/`<=`, OR-of-two-KNN) as the Go conformance/yamsql scenario, plus an
  FDB integration test that `EXPLAIN`-pins the vector index scan (not a full-scan fallback) and asserts
  row + distance correctness. Window-functions-in-`WHERE` must error (Java: `42F21`).

Constraints to mirror from Java's `VectorIndexScanMatchCandidate`: exactly one distance-rank per query;
the index MUST be partitioned and the query MUST supply partition keys; the SQL distance fn MUST match the
index `metric`; ORDER BY must be ascending; `ROW_NUMBER()` is INDEX-ONLY (refuse without a matching index).
`@API(EXPERIMENTAL)` in Java — landed Jan–Mar 2026 (Java's 4.11 series).

- [x] **9.5 Multi-partition vector scan (partial partition prefix).** Done in RFC-046 — `vectorMultiPartitionCursor` ports Java's `flatMapPipelined(prefixSkipScan, scanSinglePartition)`: `findNextPartition` skip-scans the distinct partition prefixes, `searchOnePartition` runs one HNSW search per partition, per-partition top-K concatenated, full cross-partition `FlatMapContinuation` resume. Planner: `ComputeBoundParameterPrefixMap` keeps the equality prefix + always the DistanceRank binding (no nil-query-vector on a partial prefix); `parametersRequiredForBinding={distanceAlias}` (the full-prefix guard dropped, matching Java's `VectorIndexExpansionVisitor`). Partition inequality left unconsumed → residual (documented; endpoint-into-skip-scan is a perf follow-up). Graefe+Torvalds ACK. Pinned by `TestVectorPlan_PartialPrefixPlansMultiPartition`, `TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix`, FDB E2E `TestFDB_VectorSearch_MultiPartition_{Fanout,InequalityResidual,Pagination}`. DIVERGENCES.md "Vector scan multi-partition" closed.
- [x] **9.6 MULTIDIMENSIONAL prefix skip-scan cross-prefix resume (the R-tree analog of 9.5).** `prefixSkipScanCursor` (`multidimensional_index_maintainer.go`) previously hit `ReturnedRowLimit` by returning `ReturnLimitReached` with an opaque non-nil-but-empty `BytesContinuation{bytes: []byte{}}` — a placeholder that admitted cross-prefix resume was unsupported. `Scan()`'s own continuation parser gates on `len(continuation) > 0` (false for empty bytes), so a resumed page silently read that placeholder as "no continuation" and restarted prefix enumeration from scratch — an unbounded duplicate-row replay loop under pagination, reachable via the public `FDBRecordStore.ScanIndex`. Root cause: Java's `MultidimensionalIndexMaintainer.scan()` (`MultidimensionalIndexMaintainer.java:130-179`) DOES support cross-prefix resume — it always drives this shape through `RecordCursor.flatMapPipelined` (outer = `ChainedCursor` of distinct prefix Tuples via `prefixSkipScan`/`nextPrefixTuple`, :189-246; inner = one R-tree scan per prefix), and `FlatMapPipelinedCursor`'s continuation (`FlatMapPipelinedCursor.java:372-434`) pairs the current prefix position with the inner R-tree position exactly like 9.5's `vectorMultiPartitionCursor` already does for VECTOR. Fix: ported the same `FlatMapContinuation` (outer=prefix Tuple bytes, inner=raw `MultidimensionalIndexScanContinuation`) cross-prefix resume, with `priorPrefixBytes`/`currentPrefixBytes` tracking Java's `priorOuterContinuation`/`outerContinuation`. Row limiting moved OUT of the cursor entirely to match Java's `innerScanProperties.clearSkipAndLimit()` + outer `.skipThenLimit()` (:124,179) — `Scan()` now wraps the unlimited cross-prefix cursor in the existing `LimitRowsCursor`, so the row-limit boundary's continuation is simply the last-delivered row's own (now-correct) continuation; no separate encoding needed, and the empty-placeholder class of bug cannot recur. The aggregate RFC-106a scan/byte/time-budget mid-prefix stop is UNCHANGED (still a terminal `ScanLimitReachedError`, matching existing tested behavior) — deliberately out of scope for this fix; Java does not special-case it (FlatMapPipelinedCursor never distinguishes stop reasons when building its continuation), so making it resumable too is a legitimate but separate follow-up if ever needed. Regression: `multidimensional_index_test.go` "prefix skip-scan paginates across prefixes without duplicating or replaying rows" (6 partitions × 2 rows, `ReturnedRowLimit=2`, paginated via `AsListWithContinuation` against real FDB) — verified red on the pre-fix placeholder (would loop past the 50-page safety cap replaying prefix #1). Also fixed in passing: a lazy-init step this refactor added (touching `c.m`) ran before the `ctx.Err()` check, panicking on the zero-valued `&prefixSkipScanCursor{}` `index_scan_unit_test.go`'s ctx-cancellation sweep constructs — ctx is now checked first, unconditionally.

## Executor cursor-continuation correctness fixes

- [x] **Intersection cursors delivered a decided match only after a same-call read-ahead pull, discarding it on a read-ahead error.** `intersectionCursor`/`intersectionMultiCursor.OnNext` (`merge_cursor.go`) called `child.advance(ctx)` on every child INSIDE the `allMatch` branch, after building the match's continuation but before returning it — so a read-ahead failure (context cancellation/timeout between "match found" and "look ahead" — exactly what RFC-106a's budgets exist to trigger, or any transient child error) turned an already-decided, owed row plus a valid continuation into a hard query failure with the row never delivered. Java's `MergeCursor.onNext` (`MergeCursor.java:288-305`) never does this: it returns the value immediately after `resultStates.forEach(S::consume)`, and `consume()` (`MergeCursorState.java:76-81`) is pure bookkeeping (nulls the memoized `onNextFuture`, updates the cached continuation) — no I/O. The next child pull happens LAZILY, memoized behind `getOnNextFuture()`'s null-check (`MergeCursorState.java:66-74`), on the FOLLOWING top-level `onNext()` call. Fix: added `needsAdvance` to `mergeChildState` (Java's `onNextFuture == nil` sentinel) — `consume()` marks it, `advance()` clears it — and moved the pull to the top of the merge loop (mirroring Java's `whenAll(cursorStates)` at the top of `computeNextResultStates`'s loop body), removing the inline post-consume `advance()` calls from both the match branch and (restructured, not behaviorally changed — nothing is decided there, so immediate propagation still matches Java) the non-max discard loop. Regression: `TestIntersectionCursor_MatchDeliveredBeforeReadAheadError` / `TestIntersectionMultiCursor_MatchDeliveredBeforeReadAheadError` (`intersection_resume_test.go`) — two children each yield 42 once then error on the next pull; verified red on the pre-fix code (the match was never delivered, only the read-ahead error).
- [x] **Recursive-CTE continuation wrappers held live `*TempTable` pointers, unsafe to serialize any later than immediately.** `recursiveUnionContinuation`/`tempTableInsertContinuation` (`recursive_union_cursor.go`) captured the LIVE `*TempTable` (`c.scanTable`/`c.table`) at wrap time and deferred encoding to `ToBytes()`, relying on undocumented-as-structural caller discipline ("the pager serializes once per page, right after pulling the row"). That discipline holds for the sole current consumer (`paginatingRows.fetchPage` in `cascades_generator.go`, which drains fully and calls `ToBytes()` once with no interleaved `OnNext`) but is not enforced — it breaks the moment any consumer peeks, retries, or holds a row's continuation across more than one `OnNext` call, most severely across a recursion LEVEL TRANSITION: `recursiveUnionCursor.OnNext` swaps `scanTable`/`insertTable` and `Clear()`s the recycled object, then the next level's leg `Add()`s new, unrelated rows into that SAME object — silently changing what a still-unserialized older continuation would encode. Fix, structural rather than documented: `TempTable.Clear()`/`ReplaceList()` now always hand `tt.list` a FRESH backing array (`nil` / a fresh `append`) instead of truncating in place (`tt.list[:0]`) — the change that makes a plain captured slice header immune to any future mutation, since `Add` only ever appends past a captured length and nothing can now write into memory a snapshot already exposed. New `TempTable.Snapshot()` returns `tt.list` with NO copy (O(1) — the alternative to `GetList()`'s O(n) copy the original design's "eager marshal is O(table) per emitted row" comment was avoiding); both continuation wrappers now capture `Snapshot()` eagerly at wrap time instead of the live pointer. Regression: `TestTempTable_Snapshot_ImmuneToLaterAddAndClear` (`evaluation_context_test.go`, isolates the `Clear()`+`Add()` aliasing hazard at the `TempTable` level) and `TestRecursiveUnionCursor_HeldContinuationImmuneToLaterLevelTransition` (`recursive_union_cursor_test.go`, end-to-end through a real 2-level recursion, holding row 4's continuation across the level 1→2 transition before serializing it) — both verified red on the pre-fix code.

- [x] **IN-join/IN-union scan/byte/time limits never aggregated across legs — a many-legged IN-list could silently run one FDB transaction unbounded past the 5s hard wall.** `indexFetchCursor` (`executor.go`) checks only `ctx.Err()` before every `store.LoadRecord`; the actual limit enforcement lives one layer down, in the per-index-scan leaf cursors (`keyValueCursor` for primary scans, `indexCursor` for secondary-index scans, plus `recordKeyCursor`/`countKVCursor`/`bitmapKVCursor`/`rtreeScanCursor`/`textCursor`). Every one of those leaf cursors tracked `recordsScanned`/`bytesScanned`/`startTime` as LOCAL struct fields, freshly reset to zero/`time.Now()` at construction. `executeInJoin`/`executeInUnion` (`executor_new_plans.go`) construct a brand-new leaf cursor PER IN-VALUE (`ExecutePlan(ctx, p.GetInner(), store, boundCtx, cont, props.ClearSkipAndLimit())`), so for a shape where every leg individually stays under the configured `ScannedRecordsLimit`/`ScannedBytesLimit`/`TimeLimit` (e.g. an IN-list of many values each matching a handful of rows), NO leg ever tripped the limit — however large the IN-list, however long the aggregate scan ran. Reported symptom: ~100k rows across a large IN-list took ~8s in one transaction (over FDB's 5s hard limit); the transaction failed and `FDBDatabase.Run`'s retry loop reattempted the identical, equally-doomed transaction, silently, with no error ever surfacing.

  Root cause confirmed against Java: `CursorLimitManager(FDBRecordContext, ScanProperties)` (`CursorLimitManager.java:88-98`) pulls its `RecordScanLimiter`/`ByteScanLimiter` from `scanProperties.getExecuteProperties().getState()` — Java's `ExecuteState` (`ExecuteState.java:44-47`), a mutable object held BY REFERENCE inside the value-copied `ExecuteProperties`, so it survives `clearSkipAndLimit()` (which only zeroes skip/rowLimit, `ExecuteProperties.java:240-245`) and is SHARED across every leg of an IN-join/IN-union. The `TimeScanLimiter` is reconstructed fresh per leaf cursor but is anchored to `context.getTransactionCreateTime()` (`FDBRecordContext.java:187`, `CursorLimitManager.java:93`) — a single wall-clock reference for the WHOLE transaction, so a freshly-minted leaf cursor immediately sees itself over-budget once the aggregate transaction time crosses the limit, structurally identical in effect to sharing the counter. Go's leaf cursors instead captured `time.Now()` at their OWN construction (`store_api.go`/`store.go`/`index_scan.go`/`count_index_maintainer.go`/`bitmap_value_index_maintainer.go`/`multidimensional_index_maintainer.go`/`text_cursor.go`, each `startTime: time.Now()`), which is why the divergence never showed up on a single-cursor scan (Go's per-cursor accounting is equivalent to Java's there) but silently broke the moment a plan fanned out into many leaf cursors sharing one nominal budget.

  Fix: new `ScanLimiterState` (`recordlayer/scan_limiter_state.go`) ports Java's `ExecuteState.RecordScanLimiter`/`ByteScanLimiter` pair plus the transaction-anchored time reference into ONE struct, threaded as a new `ExecuteProperties.ScanState *ScanLimiterState` pointer field — mirroring the EXISTING `ExecuteProperties.State *ExecuteState` (RFC-130 memory budget) pattern: a pointer survives every value-copy (`WithX`, `ClearSkipAndLimit`, per-leg `innerProps`) for free, so `executeInJoin`/`executeInUnion`'s per-leg `props.ClearSkipAndLimit()` calls now share ONE counter set. `DefaultExecuteProperties()` mints a fresh `ScanLimiterState` on every call (never nil, matching Java's `@Nonnull ExecuteState`) — this is the ONE mint point `paginatingRows.executeProps()` already calls exactly once per page/transaction attempt (RFC-106a), so no extra plumbing was needed to get page-scoped (not statement-wide) sharing. Every leaf cursor now resolves its state via `resolveScanLimiterState(props)` (falls back to a private per-cursor instance when `ScanState` is nil — i.e. any `ExecuteProperties` built as a raw struct literal, preserving every existing single-cursor caller's behavior byte-for-byte) and checks the SHARED counter against the limit while keeping its own LOCAL `recordsScanned`/`recordsRead` as the free-initial-pass gate — Java's per-cursor `usedInitialPass` (`CursorLimitManager.java:66,134-149`: every base cursor is guaranteed at least one record before an out-of-band limit can stop it, "a query execution might overrun its scanned records limit by up to the number of base cursors in the cursor tree," `ExecuteState.java:80-86`). `MULTIDIMENSIONAL`'s `prefixSkipScanCursor` already had its OWN hand-rolled aggregation across the PREFIXES of one scan (9.6 above, predating this fix) — its per-prefix `rtreeScanCursor` is explicitly handed a nil `ScanState` (`perPrefixProps.ExecuteProperties.ScanState = nil`) so it keeps getting a private per-prefix counter that `prefixSkipScanCursor` folds itself, rather than double-charging against a now-shared outer counter. That pre-existing mechanism is narrower than `ScanLimiterState`, though, and is NOT extended to cross LEGS by this change — `prefixSkipScanCursor` itself is still constructed fresh (fresh zero totals) once per IN-join/IN-union leg, so a many-legged IN-list over a MULTIDIMENSIONAL index is NOT covered by this fix; filed as its own follow-up below (now closed — see the `[x]` MULTIDIMENSIONAL item further down).

  Regression: `TestFDB_RFC106a_INJoinScanLimitAggregatesAcrossLegs` / `TestFDB_RFC106a_INUnionScanLimitAggregatesAcrossLegs` (`resource_limits_fdb_test.go`) — 30 IN-values each matching exactly 1 row (`InJoin(Scan(ITEM,[=]))` / `InUnion(IndexScan(PAYLOAD_IDX,[=]))`, EXPLAIN-verified plan shapes), `ScannedRowsLimit=5`: fail mode must 54F01 (not silently return all 30 rows), paginate mode must still return all 30 rows across resumed pages (no data loss). Both verified RED on the pre-fix code (`err: <nil>`, all 30 rows returned unbounded) by reverting the `pkg/recordlayer` changes and rerunning — chosen over reproducing the literal reported 8s/100k-row wall-clock hang because it exercises the IDENTICAL code path (the `time.Since(c.scanState.StartTime())` check sits on the line immediately next to the `ScannedRecordsLimit` check in every leaf cursor touched) deterministically and fast, matching the CI-hang-is-nearly-as-bad-as-the-bug directive.

  **Scope of the behavior change is broader than IN-join/IN-union** — every plan shape that shares one `ExecuteProperties` value across multiple child `ExecutePlan` calls now honestly aggregates, where it previously didn't: nested-loop join inner cursors (a fresh inner cursor per outer row, `executor.go:2390`), recursive-CTE per-level legs (`recursive_union_cursor.go:313`, `c.props` set once at construction and reused every level), flat-map, union/intersection children, and scalar subqueries (`cascades_generator.go:1785` shares `props` across every scalar subquery evaluated for a page). This is correct Java semantics in every one of those shapes, not something specific to IN-join/IN-union — but it means some previously-succeeding small-scan-budget queries over one of those shapes now paginate more, or fail loud with 54F01 where the resume cost exceeds the budget. `TestFDB_RecursiveDFS_Continuation_ResumeAcrossPages` is ONE INSTANCE of that class, not a one-off: its scanned-rows budget (16) was tuned against the OLD per-leg-reset accounting — the recursive DFS join's per-level leg executions (`recursive_union_cursor.go:362`) previously got an effectively inflated ~16-per-level-visited budget; honestly aggregated, 16 no longer clears the resume floor for that tree shape. Measured directly (temporary `fetchPage` invocation counter + a sweep of `OptExecutionScannedRowsLimit` over {16,18,20,22,24,26,28,32,48,64,96,128,256} against this exact query/tree, real FDB): limits 16/18/20/22 all hit the liveness tripwire (0 pages of progress); **24 is the lowest value that clears the floor** (6 pages, correct result) — so the true floor for this tree is between 22 and 24. Page count falls monotonically as the limit rises (24→6 pages, 32→5, 48→3, 64→2) and **collapses to a single page at 96** (128 and 256 are also single-page). The constant is set to **32** — floor (24) + ~33% margin, NOT the floor itself — chosen so the test still exercises genuine multi-page resume (5 pages at 32, vs. single-page starting at 96). Any other small-budget fan-out query elsewhere in the corpus could see the same kind of tightening; none surfaced in the full suites run below, but it is a live risk class, not closed by this item. Full `pkg/recordlayer`, `pkg/recordlayer/query/executor`, `pkg/relational/sqldriver` (`:all`), and `pkg/relational/core/embedded` (`:all`) suites green against real FDB.

  **Not fixed here (filed below): `FDBDatabase.Run`'s retry loop has no attempt cap.** Investigated per the same report's request — Java's `FDBDatabase.run()` (`FDBDatabase.java:856-864`) always goes through `newRunner()` → `FDBDatabaseRunnerImpl`, whose `maxAttempts` defaults to **10** (`FDBDatabaseFactory.java:90`, with `initialDelayMillis=10`/`maxDelayMillis=1000` full-jitter exponential backoff, `ExponentialDelay.java`) and which stops retrying — surfacing the last error — once `currAttempt + 1 >= maxAttempts` (`FDBDatabaseRunnerImpl.java:196-222`). Go's `pkg/recordlayer.FDBDatabase.Run` (`database.go:205`) instead delegates straight to `pkg/fdbgo/fdb`'s `Database.Transact`, which is DELIBERATELY unbounded by default to match libfdb_c/the Apple Go binding's own raw-client semantics (`database.go:298-310` in `pkg/fdbgo/fdb`, explicitly documented and correct per "C++ is the spec for the FDB client" — do not change). The gap is that the RECORD LAYER never adds Java's OWN additional attempt cap on top, the way `FDBDatabaseRunnerImpl` does. Not the cause of the reported hang (a transaction that respects its scan/time budget, per the fix above, never reaches the "repeatedly-retried 8s-doomed transaction" state to begin with) — filed separately below because porting it means giving `pkg/recordlayer.FDBDatabase.Run`/`RunWithWeakReads`/`RunWithVersionstamp` their OWN attempt-counting loop with a FRESH transaction per attempt (Java's `TransactionalRunner.runAsync` opens one context and does NOT itself retry — all retry/backoff is `FDBDatabaseRunnerImpl`-owned), which changes the commit/retry contract for every Record Layer transaction in the codebase and needs its own review pass, not a rushed addition here.

- [x] **MULTIDIMENSIONAL index scans now aggregate scan/byte/time limits across IN-join/IN-union legs — closes the gap left open above.** `prefixSkipScanCursor` (`multidimensional_index_maintainer.go`) previously kept its OWN local `totalScanned`/`totalBytesScanned`/`startTime`, reset to zero every time the cursor was constructed — i.e. once per IN-join/IN-union leg — and explicitly nil'd `ScanState` for its per-prefix `rtreeScanCursor` children (`perPrefixProps.ExecuteProperties.ScanState = nil`) so they never touched the caller's shared `ScanLimiterState` either. Root cause confirmed against Java: `MultidimensionalIndexMaintainer.scan()` constructs exactly ONE `CursorLimitManager` (`MultidimensionalIndexMaintainer.java:125`) and reuses it BY REFERENCE for every prefix's R-tree scan the `flatMapPipelined` loop opens (the `OnRead`/`ItemSlotCursor` constructors closed over inside the lambda, :155,162) — so the underlying `RecordScanLimiter`/`ByteScanLimiter` (shared via `ExecuteState`, `ExecuteState.java:44-58`) aggregate across every prefix AND, because that state is the same object threaded through the whole transaction, across every leg too. The free-initial-pass gate (`usedInitialPass`) is scoped to that SAME single `CursorLimitManager` instance — granted once per top-level `scan()` call (i.e. once per LEG), never once per prefix. Prefix ENUMERATION reads are a separate matter: Java's `nextPrefixTuple` builds a brand-new `KeyValueCursor` (and so a brand-new, independent `CursorLimitManager`) on every call (`KeyValueCursorBase.java:359`), so each enumeration read keeps its own de-facto free pass, unconditional on the R-tree free-pass gate.

  Fix, ported 1:1: `prefixSkipScanCursor` now resolves ONE `scanState *ScanLimiterState` at construction (`resolveScanLimiterState`, shared with the caller's leg exactly like every other leaf cursor) and hands it UNCHANGED to every per-prefix `rtreeScanCursor` it opens (via a new `scanBoundPrefix` helper factored out of `Scan()`'s bound-prefix case, so the standalone non-skip-scan path and the per-prefix path share one construction site) — no more local totals to fold back in, since every read charges the SAME counter directly. A new `rtreeFreePassUsed bool` on `prefixSkipScanCursor`, threaded as `scanBoundPrefix`'s `freePassUsed *bool` parameter and consulted via `rtreeScanCursor.hadFreePass()`, reproduces Java's single-`CursorLimitManager`-per-leg free pass: nil (a cursor's own local `scanned>0`) for a standalone scan, a leg-shared pointer for every prefix within one skip-scan. `findNextPrefix`'s enumeration reads are left deliberately UNGATED (always attempt, matching `nextPrefixTuple`'s always-fresh `CursorLimitManager`) and now charge `c.scanState` directly instead of a local total. The old upfront "remaining budget ⇒ skip creating this prefix's cursor" guard and the redundant top-of-loop aggregate pre-check are both GONE — now redundant once the shared state makes the per-prefix cursor's own checks the single source of truth (emergent behavior over a special-case check, matching the codebase's design principle #10); `prefixSkipScanCursor`'s existing `IsOutOfBand()` → terminal `ScanLimitReachedError` conversion is unchanged, so the aggregate stop is still deliberately terminal, not resumable, exactly as before.

  One real bug surfaced by this refactor and fixed in the same pass: sharing the free-pass gate makes it possible for a freshly-opened per-prefix `rtreeScanCursor` to halt on its VERY FIRST check, before reading anything (`c.lastHV` still nil) — either a sibling prefix in the same leg already spent the shared free pass, or (records limit only) `FailOnScanLimitReached` denies the free pass outright, matching `CursorLimitManager.java:135-136` throwing immediately rather than ever returning gracefully. `buildContinuation()`'s existing nil-`lastHV` guard (added by 9.6 above, specifically to prevent an unresumable placeholder continuation from being handed back as if it could resume) then errored instead of halting cleanly. Fix: new `rtreeScanCursor.limitContinuation()` returns a literal `nil` continuation (not `&BytesContinuation{bytes: nil}`, which `NewResultNoNext` would reject as an illegal end-continuation for a non-`SourceExhausted` reason) when `c.lastHV == nil`, safe because the only two callers that can ever see this state — `noNextOrFail`'s fail-mode branch (`ScanLimitReachedError` carries no continuation field at all) and `prefixSkipScanCursor`'s `IsOutOfBand()` handling (which discards the inner continuation unconditionally) — both discard it regardless of what it contains.

  Regression: `TestFDB_RFC106a_INJoinScanLimitAggregatesAcrossLegs`'s MULTIDIMENSIONAL-index analog, `multidimensional_index_test.go` "prefix skip-scan aggregates the scanned-records budget across legs of a fan-out (RFC-106a)" — MULTIDIMENSIONAL has no SQL surface (confirmed: zero references under `pkg/relational`), so it drives `store.ScanIndex` directly N times reusing ONE `ScanProperties`/`*ScanLimiterState`, exactly the sharing mechanism `executeInJoin`/`executeInUnion` use for real IN-list legs. Verified RED on the pre-fix code (reverting only `multidimensional_index_maintainer.go`): "the aggregate scan budget across 10 individually-small legs (cap=10) never tripped". All 26 `MultidimensionalIndex` specs green post-fix, including both pre-existing single-leg aggregation tests (records/bytes) and the cross-prefix pagination byte-identical-to-unpaginated resume sweep (row limits 1..7 over uneven prefix sizes {1,4,2,5,1,3}) from 9.6/`a787bfd00` — unaffected by this change. Full `pkg/recordlayer`, `pkg/recordlayer/query/executor`, `pkg/relational/sqldriver` (`:all`), `pkg/relational/core/embedded` (`:all`), `pkg/relational/conformance/...`, `conformance/...`, and `pkg/docscheck` (`:all`) suites green.

- [ ] **Port Java's `FDBDatabaseRunner` default `maxAttempts=10` (+ full-jitter exponential backoff) into `pkg/recordlayer.FDBDatabase.Run`.** See the item above for the full citation trail (`FDBDatabase.java:856-864`, `FDBDatabaseFactory.java:90-92`, `FDBDatabaseRunnerImpl.java`'s `RunRetriable`, `TransactionalRunner.java`, `ExponentialDelay.java`). Today `FDBDatabase.Run` delegates entirely to `pkg/fdbgo/fdb`'s `Database.Transact`, which retries a retryable error INSIDE one call, unbounded by default (correct raw-client parity with libfdb_c — do not touch `pkg/fdbgo`). Java's Record Layer adds a SEPARATE, higher-level cap: `FDBDatabaseRunnerImpl` opens a fresh `FDBRecordContext` per attempt (via `TransactionalRunner.runAsync`, which does not retry on its own) and gives up after `maxAttempts` (default 10) retryable failures, surfacing the last error instead of continuing. Go currently has no Record-Layer-level attempt cap at all — a transaction that reliably fails the same way every attempt (for any reason, not just the scan/byte/time-limit gap fixed above) retries forever inside the client's own loop with no visibility or bound at the Record Layer. Port: add `MaxAttempts`/`InitialDelayMillis`/`MaxDelayMillis` fields (defaults 10/10/1000) to `FDBDatabaseFactory`/`FDBDatabase`; rewrite `Run`/`RunWithWeakReads`/`RunWithVersionstamp` to own their own attempt loop (create a transaction, run the closure, commit, classify the error as retryable via the existing `fdb.IsRetryable`-style predicate, full-jitter exponential delay between attempts, give up and return the last error past `maxAttempts`) INSTEAD OF delegating straight to `d.transactor.Transact`. This is a genuinely large, invasive change — it touches the commit/retry contract for every FDB transaction the Record Layer opens (virtually the whole codebase runs through `FDBDatabase.Run`) and needs its own design pass + review before landing, not a rushed addition alongside a leaf-cursor fix. Multi-shift effort; out of scope here.

## Native fdbgo client — conformance & differential testing (RFC-010 Phase 1+)

RFC-010 Phase 0 (the wire-correctness fires: #1 inline reply error, #2 wrong_shard_server code,
#3 pipelined retry, #5 hedge queue-model leak, #8 ErrorOr union parse) landed. These three items
close the testing/conformance gaps its prevention plan (P5/P7) calls for.

### RFC-010 audit findings (the original 15 — correctness fires)

The execution list for the Codex source audit (`TODO_client.md`); full detail + C-conformance
reasoning per item in `rfcs/010-native-client-correctness.md`. **14 landed, 0 open, 1 false positive**
(#6 conn-shutdown via RFC-050, #11 TLS via RFC-051 closed the last two; updated 2026-06-13).

- [x] **#1** inline `LoadBalancedReply.error` decoded on read parsers (Phase 0)
- [x] **#2** `ErrWrongShardServer` 1062 → 1001 + anti-self-confirming fault test (Phase 0)
- [x] **#3** pipelined `Get` shares full classify→invalidate→retry; 1006 surfaced correctly (Phase 0)
- [x] **#4** tenant commit builder uses a scratch `[]MutationRef` — no in-place mutation of `tx.mutations`, no double-prefix on rebuild (build-twice regression; Torvalds + FDB-C ACK)
- [x] **#5** hedge loser/timeout/cancel QueueModel deltas released (Phase 0)
- [x] **#6 — HIGH.** Conn shutdown — fixed in RFC-050. One `failConnection(err)` path (`sync.Once`: cancel ctx + close socket + `failAllPending`) is the single teardown, used by `Close`, `connectionMonitor` death, and `readLoop` read errors. **(1)** `SendFrame`/`Flush` now wait on `errCh` **or** `ctx.Done()` (and deliberately don't pool `errCh` on the `ctx.Done()` path — audit #13 stale-value hazard), so a sender whose frame is still queued when `writeLoop` exits no longer hangs forever. **(2)** `connectionMonitor` death now calls `failConnection` — adding the missing `conn.Close()` that unblocks `readLoop`'s blocking `Read` (the old bare `cancel()` leaked the fd + goroutine until the 10 s TCP keepalive). Single-delivery to a pending reply still comes from the pending-map + `pendingMu` + delete-as-you-go; `closeOnce` only guarantees the meaningful error wins. SimTransport scope: built the in-process `net.Pipe` fake-server test harness #6 needs (handshake + stall / go-silent / abrupt-close modes) and made the monitor cadence injectable (unexported `withMonitorCadence` on an unexported `dialWith`; public signatures unchanged); the full seeded multi-mode SimTransport is deferred to C4 (YAGNI). 6 deterministic in-process `-race` tests (the two core ones verified failing on the pre-fix code: stranded-sender hang + monitor-no-socket-close leak). FDB-C + Torvalds ACK.
- [x] **#7 — MEDIUM.** Honor the "methods safe for concurrent use" contract — fixed in RFC-049. Writers already appended under `conflictMu`; the unprotected readers/clears now do too: `Commit` validation + read-only check snapshot `mutations`/`len(writeConflicts)` under the lock and **thread that validated snapshot into the marshal** (so a `Set` racing `Commit` can't ship an *unvalidated* mutation to the proxy — FDB-C catch); `buildCommitTransactionRequest`/`commitDummyTransaction` snapshot the conflict headers under the lock (append-only + `conflictBuf`-only-grows ⇒ snapshot-and-release is race-free for them); `GetApproximateSize` iterates **under** the lock (not a released snapshot — it can race `Commit`'s in-contract auto-reset, which `[:0]`-reuses the backing arrays); `mutations[:0]` clears moved inside `conflictMu` in reset/postCommitReset; `addWriteConflict*` moved the `nextWriteNoConflict`/`writeConflictsDisabled` gate inside the lock (the one-shot flag is read+cleared on the `Set` path → two concurrent `Set`s raced). `Set`/`Clear`/`ClearRange`/`Atomic` now publish the mutation + its write-conflict range **atomically** under one `conflictMu` acquisition (codex catch — the old two-lock split let a `Commit` snapshot ship a mutation *without* its conflict range → a missed conflict; this also subsumes the `nextWriteNoConflict` fix and drops `Set` from two locks to one). Contract doc narrowed: option setters (`SetXxx`) + `Reset` are configure-before-use, not concurrent-safe (matches `fdb_transaction_set_option`); RYW lost-update stays documented-not-safe. 6 deterministic concurrency tests (verified failing on the pre-fix code) + tenant no-alias sentinel + validated-snapshot pin + Set-atomicity invariant. FDB-C + Torvalds + codex review.
- [x] **#8** `ReadErrorOr` parses the union tag (not field count); error code uint16 (Phase 0)
- [x] **#9** rename `isSystemKey` → `isSpecialKey` (tests `\xff\xff` special-key space; behavior unchanged)
- [x] **#10** decoupled `ACCESS_SYSTEM_KEYS` from `LOCK_AWARE` in `fdb/options.go` (C sets them
  independently — confirmed NativeAPI 7159 / RYW 2557 / TenantManagement). Facade no longer
  auto-sets lock-aware; each `fdb/database.go` tenant call site sets the exact C++ options (writes
  ACCESS+LOCK_AWARE; OpenTenant READ_SYSTEM_KEYS+READ_LOCK_AWARE; ListTenants
  READ_SYSTEM_KEYS+LOCK_AWARE). Behavior change: external callers
  relying on the implicit coupling must set `SetLockAware` explicitly (as a Java/CGo app must) — only
  observable on a *locked* DB; wire-safe (lock-aware is a commit flag, not persisted bytes).
  Pinned by `TestSetAccessSystemKeys_DoesNotImplyLockAware` (facade unit test, fails if the coupling returns).
- [x] **#11 — MEDIUM.** TLS wired end-to-end — fixed in RFC-051. `ParseClusterString` parses the `:tls` coordinator suffix (faithful to C++ `NetworkAddress::parse`: strip `(fromHostname)` then `:tls` when len>4; uniform-cluster, mixed rejected) → `ClusterFile.UseTLS`; `database` carries `tlsConfig *tls.Config` and `getOrDialConn` dials TLS; `resolveTLSConfig` loads `FDB_TLS_{CERTIFICATE,KEY,CA}_FILE` (→ `/etc/foundationdb/{cert,key}.pem` default) into a standard config, C++-precedence-faithful. **Go-idiomatic user-facing API (bradfitz review):** `transport.Dial(ctx, addr, *tls.Config, dialFn)` — the non-nil config is the *only* "use TLS" signal (nil = plaintext), so the silent-downgrade footgun is gone by construction (the `useTLS bool` + `DialWith`/`DialWithTLS` overloads + bespoke `transport.TLSConfig` are deleted); `fdb.OpenDatabase(clusterFile, WithTLSConfig(*tls.Config), WithDialFunc(...))` functional options, precedence explicit > `FDB_TLS_*` env > default; `upgradeTLS` clones + fills `ServerName`/`MinVersion` only if unset. 6 deterministic tests incl. a real in-process mutual-TLS handshake (FDB ConnectPacket inside the tunnel) + wrong-CA/missing-client-cert rejects. FDB-C + Torvalds + bradfitz ACK. Follow-ups: per-address TLS flag (dual-listen), `FDB_TLS_VERIFY_PEERS` rule DSL, `FDB_TLS_PASSWORD`/encrypted keys, FDB-TLS testcontainer e2e.
- [x] **#13 — LOW (concurrency-sensitive).** Fixed in **RFC-072**. The reply channel is now returned to the pool exactly on the no-send-can-race paths: `Release()` pools it on the success path (caller received, no `Cancel`); `Cancel()` pools iff it won the `delete` race and nils `h.ch` so `Release` never double-pools; `SendAndWait` pools on success and via `cancelPending` (delete + pool-iff-won) on timeout, leaving the rare race-loser to GC (it may hold a stale buffered value). The false "readLoop returns it after dispatch" comment is corrected — readLoop only delivers. Pinned by `reply_pool_test.go` (won/lost-race + success + no-double-pool, `-race`-clean) via a `putReplyChannel` seam (deterministic, not `sync.Pool`-reuse-dependent). Full multi-goroutine timeout-vs-delivery race coverage awaits `SimTransport` (C4). FDB-C + Torvalds ACK.
- [x] **#14 — LOW.** Monitor ping on a saturated `writeCh` — fixed in RFC-052. The send was already non-blocking (`select … default`), but the drop path returned a **closed** `done`, which the monitor read as `case <-replyCh:` "PING reply arrived → connection alive" — so a *stuck* connection (writeLoop blocked on an undrained socket ⇒ `writeCh` saturates) falsely passed as alive and never reached the `bytesReceived` liveness check (the one state where the monitor must act). Fix: the drop path returns **nil** (never selected in the monitor's `select`) so it falls through to the timer → `bytesReceived` kill — faithful to C++ `connectionMonitor`, whose liveness verdict is solely bytes-received (the ping-reply arm only restarts the cycle; C++ `Peer::send` is an unbounded buffer with no "couldn't send" path). Pinned by `TestSendPingWithReply_DropsToNilOnFullWriteCh` (verified failing on the pre-fix closed-`done`); the sent-path kill stays covered by `TestConn_MonitorDeathClosesSocket`. FDB-C + Torvalds ACK.
- [x] **#15** range-iterator next-begin via `keyAfter` helper that copies (no alias/scribble of `lastKey`); spare-capacity unit pin
- **#12 — FALSE POSITIVE.** Locality never panics (invariant guarantees non-empty); add a defensive guard at most.

We **cannot** run FDB's deterministic simulation: Sim2 is a hermetic single-threaded Flow event
loop with an in-memory network and no external socket, so a real Go client can't join it, and
server-side BUGGIFY edge-case injection exists only inside Sim2. But three of FDB's real,
externally-usable artifacts CAN be exercised against a testcontainer cluster our Go client
mutated. (Determinism for our OWN retry/LB/wire-error paths — `PendingGet.Resolve`'s
flush/transport/timeout arms, the codex 1006 drop-between-dial-and-send race, transparent
wrong-shard retry — comes from a seeded in-process `SimTransport` fake server behind
`transport.DialFunc`, extending the existing `wrongShardConn`; tracked as a separate Phase-1 item.)

- [x] **C1. Ride their oracle — FDB `ConsistencyCheck` after Go-client writes.** DONE
  (`pkg/fdbgo/conformance/consistencycheck_test.go`). `RunCluster(3, double, ssd)` →
  pure-Go client writes 1000 keys → wait replication-healthy → run FDB's one-shot
  `fdbserver -r consistencycheck` role → parse its JSON trace and assert it completed
  (`ConsistencyCheck_FinishedCheck`), examined data, and emitted **no** Severity-40
  inconsistency/`TestFailure` event. **Double redundancy is required** — under single
  redundancy the checker's replica comparison is a no-op (one copy per shard). Anti-vacuity:
  require `GetKeyValuesStream` reads (one per replica per shard) **>** `FirstValidServer`
  baselines (one per shard) — i.e. some single shard was read on ≥2 replicas, which a bare
  "≥2 reads total" count can't prove (N single-replica shards defeat it). `FirstValidServer`/
  `CheckCustomReplica` fire even under single redundancy and do NOT prove a comparison. The
  process exit code is unreliable (exits 0 even on inconsistency), so detection is by trace
  event: any Sev40 `ConsistencyCheck_*` (catch-all), the SevInfo `InconsistentStorageMetrics`,
  and Sev40 `TestFailure` reasons containing "inconsistent". Detection logic pinned by a
  deterministic unit test (`TestParseConsistencyTrace`) since the live run is always clean.

- [x] **C2. Ride their client — differential vs the official C binding (`libfdb_c`).** Landed in
  **RFC-053 (PR #231)**. Differential harness in `pkg/fdbgo/bench` (reuses the dual-client fixture):
  L2 write battery (byte-identical persisted state — Set shapes incl. exactly-VALUE_SIZE_LIMIT, every
  atomic on a missing key pinning the Min→MinV2/And→AndV2 upgrade, SetVersionstampedValue offset,
  key-at-KEY_SIZE_LIMIT boundary) and L3 read parity (GetRange chunking-invariance across
  StreamingModes/limits/reverse + GetKey selector parity, read-version-pinned). Proven to have teeth
  (reverting Min→MinV2 fails it byte-exactly). **Surfaced & fixed FOUR real client divergences**, each
  pinned with a fail-pre-fix test: SetVersionstampedKey spurious write-conflict range; client-side
  key/value size-limit enforcement (set/atomic reject at commit, clear clamps/drops); raw-access key
  limit set by ACCESS_SYSTEM_KEYS/READ_SYSTEM_KEYS (not just RAW_ACCESS); raw-access slack gated off
  for tenant txns. Reviewed by FDB-C-dev + Torvalds + codex (3 P2s) + @claude.
  **Follow-up RFC-054: `FuzzDifferential`** — random op sequences through both clients,
  byte-identical persisted state (RYW coalescing, atomic accumulation, clear/overwrite
  ordering); 40s burst = 8068 execs, 0 mismatches.
  **Follow-up RFC-055: RYW-read differential (Get/GetRange)** — found+fixed a getRange
  merge bug that dropped empty-value pending keys.
  <details><summary>original spec</summary>
  The C
  binding is the client FDB simulation-tests on every CI run, so matching it is the closest we get
  to inheriting that coverage (RFC-010 prevention P5, corrected). Run the SAME operations through
  our Go client and `libfdb_c` against the same testcontainer cluster. **CRITICAL: compare at the
  DATA plane, never the wire.** Request frames are legitimately NOT byte-identical — reply-promise
  UIDs, read/committed versions, trace/span IDs, GRV batching, mutation/conflict ordering, and
  range chunk boundaries all vary per client. So:
    - **Writes → byte-exact on PERSISTED bytes.** Write the same logical mutation via each client,
      read the raw keys/values back out of FDB, assert byte-identical: key/tuple encoding, value &
      record format, index entries, version at `pk+\xff`, split chunking, continuation-token bytes
      + magic `6773487359078157740`. This is the cross-client compatibility hard line — where
      byte-identity is both *required* (Java/Go share a cluster) and *achievable* (the persisted
      format is spec-fixed; control-plane randomness never touches it).
    - **Reads → semantic, control-plane excluded.** Same key/range + a pinned read version →
      compare returned value / merged KV set + order / error CODE (not message). Ignore reply
      tokens; don't compare the literal version number (compare the data it produced); merge range
      chunks before comparing. Under deliberate concurrency, compare error CLASSES, not exact codes.
    - **Continuations → mutually resumable** (a Go-produced continuation resumes correctly when fed
      back; byte-equal where the format is fully spec-pinned). Any *data-plane* byte difference is a
      real wire-compat bug, NOT a tolerance to normalize away.
  </details>

- [ ] **C2-followup. RYW key-selector + read-version correctness audit (RFC-056).** Remaining
  RYW read-resolution divergences from libfdb_c surfaced by the RFC-055 differential:
  (2) a go-vs-cgo read-version
  staleness asymmetry (go=`transaction_too_old(1007)` while cgo succeeds on the SAME pinned read
  version near the 5s MVCC edge). **Characterized (RFC-056 #235): PERF/TIMING, not a wire/
  behavioral divergence** — both clients correctly return 1007 once a read version genuinely ages
  past the 5s window; go just reaches that edge sooner under CPU starvation because its getKey
  does more per call (the materializing `buildSegmentsLocked` vs libfdb_c's lazy iterator), and
  the differential pins one version then issues 28 selectors on it. So behavioral identity HOLDS;
  the real fix is the lazy iterator (continuation item 1 below), which reduces the per-call work
  at the source. The differential is already robust (retries the transient 1007 with a fresh
  version via the canonical `gofdb.IsRetryable` predicate — `differential_getkey_ryw_test.go`).
  REMAINING: profile go-getKey 1007-rate vs cgo to confirm item-1 closes it. See rfcs/055.
  - [x] **(1) `Transaction.GetKey` ignores pending writes** — FIXED (RFC-056): faithful port of
    C++ `resolveKeySelectorFromCache` over a merged segment view (`pkg/fdbgo/client/ryw_getkey.go`:
    `rywSegmentIterator`/`buildSegmentsLocked` + `getKeyRYW`'s unknown-range server-read-remerge
    loop), wired into `Transaction.GetKey` (+ the base↔resolved RANGE read-conflict, fixing the
    old single-key conflict) and `Snapshot.GetKey` (writes visible by default via
    `includeWrites=!snapshotRYWDisabled`). A merged-GetRange shortcut was verified-WRONG on
    `{orEqual, offset>1}` — not used. Pinned by `ryw_getkey_test.go` + the
    `TestDifferential_GetKeyRYW` differential (pending Set/Clear/ClearRange vs libfdb_c) + corpus
    seeds. **Two deferred sub-edges, same root** (the rywCache doesn't preserve per-key op-type
    — it eagerly folds resolved atomics into plain entries and moves a matched CompareAndClear
    into the cleared list; faithfully closing either needs a write-map that retains op-type, like
    C++'s):
    (a) **phantom offset slot** — a PENDING atomic that resolves to no value (CompareAndClear, or
    an atomic on a locally-cleared range) is modeled as absent; libfdb_c keeps it as a "phantom"
    is_kv slot COUNTED in the offset walk. The getKey differential is scoped to non-atomic pending
    writes until then.
    (b) **conflict-range filtering** — C++ `updateConflictMap` SUBTRACTS independent-write/cleared
    segments from the getKey read-conflict (no DB read there). Go keeps the FULL base↔resolved
    range: it OVER-conflicts on those segments (extra retries, always SAFE) rather than risk a
    missed conflict on a folded dependent atomic (an UNSAFE under-conflict — a naive
    `!hasAtomics` filter was attempted and reverted after codex showed it drops the conflict for a
    Get-folded atomic). The full range is strictly better than the old single-key conflict (which
    under-conflicted). Exact filtering deferred with the op-type preservation above.
  - [x] **RYW applyAtomic on present-empty values** — FIXED: the chain conflated `nil` (absent)
    with present-empty, so a V2 op after `Xor(k,"")` took the absent→operand path (`Min(k,"0")`
    → 0x30 vs libfdb_c 0x00). The get/merge chains now keep present-empty non-nil (nil reserved
    for absent), mirroring C++ `Optional.present()`. Pinned by
    `TestRYWGetRange_V2AtomicOnPresentEmpty`.
  - (3) **versionstamped-pending read = unreadable.** A SetVersionstampedKey/Value pending on a
    key reads as ABSENT in Go pre-commit (Get→nil, GetRange→omit); C++ marks it `is_unreadable`
    and THROWS `accessed_unreadable`. Go has no unreadable state — approximated as absent,
    consistently across ALL base states: storage-absent, locally cleared, a pending plain Set,
    and a non-nil storage value the pending stamp shadows. `atomic()` refuses to eager-fold a
    versionstamp into a plain entry, and `resolveAtomics` short-circuits the chain to
    `unresolved` (terminal, dominant over cleared) so both read paths exclude the key and drop
    any stale storage value. Pinned by `TestRYW_VersionstampedAbsentNoPhantom` +
    `TestRYW_VersionstampedOverClearedOrPlainNoPhantom`. Full C++ parity (THROW on read) still
    needs an explicit unreadable concept — part of the RFC-056 audit.

- [ ] **RFC-056 continuation — ordered, ONE AT A TIME (do 1, then 2, then 3).** After the merged
  getKey-RYW core (#235), three follow-ups remain. Both 1 and 2 WILL be done (sequentially, not
  batched); 3 is the ongoing hunt.

  1. **[x] DONE (RFC-057).** Lazy `rywSegCursor` replaced the materializing
     `buildSegmentsLocked`: getKey cost is now FLAT in cache size — **57 ms / 39 MB →
     1 µs / 816 B at N=100k (55,437×)**, measured before/after (Torvalds's "no benchmark =
     no merge" gate). Behavior-identical: a 4000-state equivalence property-test oracled
     against the retained materializer + the RFC-056 differential + a 94k-exec fuzz burst,
     all green. `next`/`prev` are a single merged-boundary `skip` (no view desync). The
     original plan:
     **Lazy/windowed segment iterator for getKey-RYW.** `buildSegmentsLocked`
     (`pkg/fdbgo/client/ryw_getkey.go`) MATERIALIZES the whole merged-segment partition of
     [allKeysBegin, maxKey) — O(writes + cacheKeys) per resolution attempt — whereas libfdb_c's
     `RYWIterator` is LAZY (a steppable zip of the write-map + snapshot-cache sub-iterators).
     Port the lazy cursor (skip/next/prev computing each segment on demand, no full
     materialization), so getKey cost is bounded by the walk distance, not the cache size. This
     ALSO shrinks **item (2)** below: less work per getKey under heavy parallel-container load →
     less likely to drift past the 5s MVCC window mid-loop → fewer transient
     `transaction_too_old(1007)`. Validate with a profiling probe: go-getKey wall-clock +
     1007-rate vs libfdb_c, before/after; confirm resolution stays byte-identical
     (`TestDifferential_GetKeyRYW` + unit tests green). Then this de-flakes item (2) at the source
     rather than only via the differential's retry.

  2. **[x] DONE (RFC-058).** Op-type-preserving write-map closed BOTH sub-edges. Added `absent`
     (phantom) + `dependent` (DEPENDENT_WRITE, carried unchanged through folds like C++
     `isDependent()` reading the immutable stack bottom) to `rywEntry`; a matched CompareAndClear
     now stays a write-map entry (never moved to `cleared`). The differential **disproved the
     original framing of (a)**: getKey is a limit-1 range read in C++ (`read(GetKeyReq)` =
     `getRangeValue`/`getRangeValueBack`), so a phantom is COUNTED in the offset walk but SKIPPED
     at the landing — not "counted and landed on." Modeled as `segPhantom` (count-in-walk +
     directional skip-at-landing); the old `segEmpty` under-counted for offset>1, a naive `segKV`
     wrongly landed on it. Also fixed a pre-existing fold-path bug the same differential caught
     (`doMax(_,"")`→nil misread as absent by a later CompareAndClear). (b) Ported `updateConflictMap`
     (ReadYourWrites.actor.cpp:335) as `conflictRangesLocked` — the getKey read-conflict now
     SUBTRACTS INDEPENDENT writes + cleared ranges (safe now that op-type is preserved; the naive
     `!hasAtomics` filter codex NAK'd on #235 is impossible here). Proof: getKey differential
     re-enabled for pending CAC/atomics + 92k-exec fuzz (sub-edge a); a deterministic commit-order
     `TestDifferential_GetKeyConflict` whose INDEPENDENT/CLEARED cases FAIL without the filter and
     pass with it (sub-edge b). FDB-C-dev + Torvalds ACK on the RFC. Original (a)/(b) text:
     (a) **phantom-slot offset counting** — a PENDING atomic that resolves to no value
         (CompareAndClear, or an atomic on a locally-cleared range) is an `is_kv` "phantom" slot
         COUNTED in the getKey offset walk in libfdb_c, but Go currently models it as absent. With
         op-type preserved, count it. (Re-enable pending-atomic shapes in the getKey differential.)
     (b) **exact `updateConflictMap` conflict filtering** — getKey's read-conflict should SUBTRACT
         independent-write + cleared segments (no DB read there); Go currently keeps the
         conservative FULL base↔resolved range (safe over-conflict). With op-type preserved, the
         subtraction is safe (a naive `!hasAtomics` filter was UNSAFE — it dropped the conflict
         for a Get-folded dependent atomic; codex caught it on #235 → reverted). Port
         `updateConflictMap` (ReadYourWrites.actor.cpp:335) faithfully and pin with a conflict
         differential (concurrent write inside the range must conflict identically in both clients).

  3. **Fresh differential axes (`/hunt-divergences`).** Probe axes still uncompared vs libfdb_c:
     atomic-op edge cases across ALL of `Atomic.h` (empty / missing / present-empty operand per
     op); error-code + option semantics (RAW_ACCESS / ACCESS_SYSTEM_KEYS / snapshot-RYW); key
     encoding / tuple packing / versionstamp-offset validation. Each closed axis is more "absolute
     proof we're identical to the C client."
     - [x] **[RFC-059 — MERGED #238] RYW-disable-after-op poison.** Differential characterization
       corrected the earlier (imprecise) framing: NOT a per-read overlap check, NOT an
       option-set-time error. libfdb_c's `setOption(READ_YOUR_WRITES_DISABLE)` after any read or
       write throws `client_invalid_operation` deferred via `deferredError`, so the option call
       succeeds but EVERY subsequent op (regular + snapshot reads/GetKey, GetRange, GetReadVersion,
       GetEstimatedRangeSizeBytes, GetRangeSplitPoints, Commit) returns 2000 — the whole txn is
       poisoned. Go was silently permissive (returned 0). RFC-059 ports the poison
       (`Transaction.rywPoisonErr` set on disable-after-op, gated uniformly at `ensureReadVersion` +
       the metrics path; a `hadRead` signal covers the GetPipelined non-caching read). Pinned by
       `TestDifferential_RYWDisableAfterOp` + `TestCommit_RYWPoisonBeatsTimeout`. Reviewed by
       FDB-C++ dev + Torvalds + codex + @claude.
     - [x] **[RFC-060 — MERGED #239] tuple-codec byte-identity differential.** The tuple/key encoding is the wire
       hard line but has ZERO differential coverage vs libfdb_c's codec. `pkg/fdbgo/fdb/tuple` is a
       near-verbatim port (core encode/decode byte-identical by inspection) but adds go-only
       hot-path helpers (`PackWithPrefix`/`Pack1WithPrefix`/`Pack1ConcatWithPrefix`/
       `PackConcatWithPrefix`/`Packer.AppendInto`/`packerPool`) absent from libfdb_c that build the
       actual index/record keys on the wire. Prove `gotuple.Pack() == cgotuple.Pack()` across all
       type codes + boundaries (int size-limit boundaries, big.Int >8 bytes + leading-0xff
       zero-fill, float/double sign-bit flip, nil-escaping in bytes/strings/nested, versionstamp
       offset), the go-only helpers vs canonical `cgotuple.Pack()`, cross-client Unpack, and an
       end-to-end FDB wire round-trip. cgotuple is itself pinned to the cross-language
       `tuples.golden`, so this transitively pins go to the golden vectors.
     - [x] **[RFC-061 — MERGED #240] SNAPSHOT_RYW_ENABLE/DISABLE counter.** Found via the
       transaction-option-semantics survey, confirmed differentially: libfdb_c models snapshot
       RYW as an integer counter (ENABLE++, DISABLE--, bypass iff <=0, default 1), but Go used a
       boolean with `SetSnapshotRywEnable()` a no-op — so `disable→enable` left snapshot reads
       stuck bypassing RYW (go silently too permissive). Fixed: `snapshotRYWDisableCount int`
       (zero-value-safe inverse: DISABLE++, ENABLE--, bypass iff >0; preserved across reset as a
       persistent option). Pinned by `TestDifferential_SnapshotRYWReenable` (10 sequences, 3
       red→green + a counter-vs-boolean discriminator + negative-count axis + RYW-disable
       dominance).
     - [x] **[RFC-062 — MERGED #241] atomic-fold width/edge differential.** Atomic fold semantics
       are the wire hard line; the existing differential only used 8-byte operands on missing keys.
       Added a differential across operand/base widths + edge operands for all 12 ops. KEY finding
       (teeth-check): tx.Set/tx.Atomic ship RAW mutations (server folds at commit), so Go's
       client-side fold (doAdd/doMin/…) runs ONLY on in-txn reads — a commit-then-read-back test
       passed even with doAdd broken. Restructured to read WITHIN the txn (exercises the fold) +
       committed read-back (server fold/wire). Verify-and-pin (fold is a faithful port); teeth
       confirmed on doAdd (6 rows) + doByteMin (4 rows). Found+fixed a test-isolation bug (go/cgo
       shared a key → missing-key fold saw the other's committed value).
     - [x] **[RFC-063 — MERGED #242] versionstamp-mutation differential.** SetVersionstampedKey/Value
       were excluded from the fuzz differential; only a Go-only interop check + an offset-0 Value
       case existed. Added masked (10-byte stamp zeroed) go-vs-cgo differentials: VersionstampedKey
       (offset 0 / after-prefix / mid-key / binary), VSValueOffsets (non-zero offsets), tuple
       PackWithVersionstamp (offset + 2-byte user-version preservation), GetVersionstamp parity
       (10-byte, == materialized stamp), error/boundary (tight-valid offset+10==body vs off-by-one
       reject, negative, past-body, too-small, empty → 2000 go==cgo), multi-op. Mask offset is
       template-derived + length/surround/non-zero guards (Torvalds). Teeth: loosening
       validateVersionstampOffset by 1 reddens offbyone_reject. The differential CORRECTED a
       reviewer assumption: two versionstamped ops in one txn get the SAME stamp (txn-level, not
       per-op batch id; user differentiates via tuple user version).
       - [x] **Follow-up (tenant +8 offset) — DONE + found a BIGGER bug.** Built the tenant
         differential harness in `pkg/fdbgo/bench` (`differential_tenant_test.go`: shared tenant on
         both clients; TenantVersionstampedKey masked read-back + raw full-key +8 assertion,
         TenantVersionstampedValue value-offset-NOT-adjusted, TenantVersionstampErrors boundary).
         The +8 offset adjustment (`commitpath.go`) was already correct (go==cgo). But the harness
         immediately surfaced a REAL cross-client wire-compat divergence: the tenant `nameIndex` and
         `lastId` are `TupleCodec<int64_t>` (minimal-width); `tenant_crud.go` hard-coded the fixed
         9-byte form (`0x1C`+8) for both pack AND unpack, so a Go client could NOT open/list/delete a
         tenant created by libfdb_c/Java (`OpenTenant` failed "expected 9 bytes, got 2"), nor create
         a tenant after one (couldn't decode the C-written `lastId`). Fixed the codec to FDB's real
         minimal-width tuple-int encoding (Tuple.cpp:204-227); reads legacy 9-byte values too.
         Pinned by `TestDifferential_TenantCrossClientCRUD` (go↔cgo create/open/write/read/list) +
         `tenant_crud_internal_test.go` (FDB-spec vectors, round-trip, legacy decode, errors).
     - [x] **[RFC-064 — MERGED #243] explicit conflict-range API differential.** AddReadConflictRange/
       Key + AddWriteConflictRange/Key feed the resolver (isolation) but had no differential coverage
       (RFC-058 covered only getKey-DERIVED conflict ranges). Empirically NO divergence — edges
       (inverted→2005, empty→accept, oversized→accept) match go==cgo (the C++ NativeAPI source has no
       release inverted-check, but the C binding cgo uses returns 2005 — the differential is the spec,
       not the source). Pinned the conflict OUTCOME: read-conflict range/key (A fails 1020 iff probe
       inside, half-open r0 incl / r9 excl), write-conflict range/key (a concurrent reader fails iff
       inside A's write-conflict), snapshot-read-no-conflict, self-write+read-conflict. Reused RFC-058
       pinning (both A+B SetReadVersion(vSetup), transient→retry, fresh prefix/attempt, bounded) →
       flake-free (5 runs). Teeth: empty key-conflict range → key_exact_r5 diverges. Oversized
       committed-truncation is unobservable (keys > maxKeySize are unwritable).
     - [x] **[RFC-065] getKey boundary resolution — REAL BUG FIXED.** The existing
       getKey differentials cover the keyspace INTERIOR + clamp off-prefix results, masking the
       EDGES. A boundary probe found a real divergence: a BACKWARD selector (lastLess*) at/past
       maxReadKey (\xff) wrongly returned \xff itself instead of the greatest key < \xff. Root
       cause: resolveKeySelectorFromCache (ryw_getkey.go) short-circuited EVERY off-end seek to
       readThroughEnd, ignoring direction; C++ it.skip() clamps to the last segment and only sets
       readThroughEnd after the walk for offset>1. Fix: direction-aware off-end branch — forward
       keeps readThroughEnd; backward repositions onto the last segment and resolves backward.
       Pinned by TestDifferential_GetKeyBoundary (pinned-version differential: lastLess*(maxReadKey)
       asserted < maxReadKey, empty/large-offset/past-max edges). Teeth: re-introducing the
       unconditional shortcut reddens LLT/LLE_maxReadKey. Only the RYW path had it; rywDisabled
       delegates to the server.
     - [x] **[RFC-067 — MERGED #247] error-CODE differential → TRANSACTION_SIZE_LIMIT + 4 linked fixes.**
       A fresh error-CODE differential (`TestDifferential_ErrorCodes`, comparing the FDB error code
       each client returns for the same size/legal-range triggers) found a REAL write-path divergence:
       the Go client did NOT enforce `TRANSACTION_SIZE_LIMIT` by default — it committed >10 MB txns
       that libfdb_c rejects client-side with `transaction_too_large` (2101). C++ defaults every txn's
       sizeLimit to the 10 MB knob (NativeAPI:6133); Go's `0=disabled` default left no enforcement.
       Fix: default to the knob. Four more linked fixes surfaced via review + differential: (2) online-
       indexer lessen-work codes (Torvalds — wrong numbers, missing 2101, made latent-live by the
       limit; now matches Java `IndexingThrottle.lessenWorkCodes` 1:1); (3) commit-validation ORDER
       (codex — read-only fast path + per-mutation-before-size; then the full eager-vs-deferred model:
       key/value-size + versionstamp-offset are EAGER first-invalid-op-wins, txn-size DEFERRED; pinned
       by `TestDifferential_VersionstampValidationOrder`, 8 cases); (4) `metadataVersionKey` write
       contract (codex+FDB-C+++Torvalds — a blanket `continue` silently committed every illegal mvk
       mutation where libfdb_c returns 2000/2004; replaced with the exact C++ gate; pinned by
       `TestDifferential_MetadataVersionKey`, 8 cases); (5) size the VALIDATED snapshot not the live
       buffer (codex — a Set racing Commit could fail a small commit for an unshipped mutation; pinned
       by `TestApproximateCommitSize_SizesSnapshotNotLiveBuffer`). Also fixed a pre-existing
       differential-harness flake: pinned-version range reads now retry the transient 1007 (stale pin
       under parallel-container load) instead of `t.Fatalf` (pinned by
       `TestDifferential_PinnedRangeRetriesStaleVersion`). Reviewed clean by FDB-C++ dev + Torvalds +
       codex (per-commit deltas + full review) + @claude.

- [x] **GRV `locked` enforcement — DONE (RFC-096, FDB-C++ + Torvalds ACK on RFC; found by the
  RFC-095 reply ground-truth net).** The Go client silently read LOCKED databases where C++/Java
  refuse with `database_locked` (1038): `parseGetReadVersionReply` discarded `rep.locked`. Now
  enforced per C++ (`NativeAPI.actor.cpp:7425-7426`): `locked` threads from the batched GRV reply
  to every waiting transaction; the per-txn check at the `extractReadVersion` analog
  (transaction.go ensureReadVersion) returns 1038 unless `lockAware || readLockAware` (both C++
  options set `options.lockAware`, `:7077-7091`). The shared cache updates BEFORE the check (C++
  `:7409` precedes `:7425`), and — because Go's GRV cache is ALWAYS-ON unlike C++'s opt-in
  USE_GRV_CACHE (divergence filed below) — `locked` rides the cache (`grvCache.lastLocked`,
  stored only on version-CAS acceptance so a stale reply can't fail-open; Torvalds condition) and
  cache hits flow through the same per-txn check. Pinned by
  `TestFDB_DatabaseLocked_ReadPathEnforcement` (dedicated container; real `\xff/dbLocked` lock
  via the C++ `lockDatabase` mechanics; arms: fresh-fetch 1038, warm-cache 1038, LOCK_AWARE ok,
  READ_LOCK_AWARE ok, unlock+poll recovery) — revert-proven red without the check — plus the
  production-parser `locked` assert in the `GetReadVersionReply_locked` reply vector.

- [x] **GRV cache is ALWAYS-ON in Go; opt-in (USE_GRV_CACHE) in C++ — DONE (RFC-104).** Closed:
  the cache is now opt-in, default off. Cache READS are gated on the transaction's `useGrvCache`
  (`SetUseGrvCache`/USE_GRV_CACHE 1101; `SetSkipGrvCache`/SKIP_GRV_CACHE 1102, skip wins) at
  `grv.go:284` and the background refresher only starts on the first opted-in request
  (`grv.go:293`) — matching C++ `NativeAPI.actor.cpp:7504-7517` (gate `:7505`, default false
  `:6148`). The opted-in cached path fail-opens on `locked` exactly as C++ does (`:7514-7516`), so
  RFC-096's `lastLocked` ride-along — which existed ONLY to compensate for the previous always-on
  cache — was removed (`grv.go:38-45`). The RFC-098 wrong-answer (a default Go txn serving a
  version older than a libfdb_c-committed seed) no longer reproduces: a DEFAULT Go read now sees
  cgo-committed data directly. Pinned by `TestFDB_GRVCache_OptInOnly`,
  `TestFDB_GRVCache_RefresherStartsOnOptInMiss`, `TestFDB_GRVCache_SkipOverridesUse`
  (`client/grv_cache_optin_test.go`) + `TestDifferential_GRVCacheDefaultSeesCgoSeed`
  (`bench/differential_grvcache_test.go`). Differential-test causality comments already rewritten
  to "key-ownership hygiene, not a workaround" (`bench/differential_unreadable_test.go`).

- [ ] **C3. Ride their test designs — port FDB workloads as scenario + invariant specs.** FDB's
  `fdbserver/workloads/*.actor.cpp` (Cycle, AtomicOps, ConflictRange, Serializability,
  FuzzApiCorrectness, …) are unrunnable for us (Sim2-only), but each scenario + invariant is
  language-agnostic. Port the adversarial designs — e.g. Cycle: maintain a ring of pointer K/Vs,
  hammer it concurrently (+faults), verify the ring stays unbroken — to drive our client against
  testcontainers (and later `SimTransport`). Reimplement the harness; reuse the proven scenarios.
  Extends the existing `pkg/recordlayer/chaos` model-based approach + `cmd/fdb-binding-stress`.

- [x] **C4. Deferred Phase-0 test gaps — DONE (RFC-118 SimTransport).** All four closed with
  revert-proven regressions (`client/simtransport_test.go`, migrated `client/fault_test.go`):
    - **Inline `LoadBalancedReply.error` on `parseGetKeyReply` / `parseGetKeyValuesReply` / `parseGetValueReply`** —
      the `TestWrongShardServer_*` tests now inject through the faithful inline channel
      (`ErrorOr<reply>` tag=value + nested inline error, `types.MarshalErrorOrInlineError`), the way
      real FDB delivers a read wrong-shard. (RFC-115 §6 had already fixed the `Optional<Error>`
      marshal — the "generated writer mis-marshals" caveat above was stale.)
    - **`PendingGet.Resolve` flush-error arm** — a `Close()`d real conn → `Flush()` returns
      `errConnClosed` deterministically (`TestPipelinedGet_Resolve_FlushErrorRetries`).
    - **Range wrong-shard mid-scan (`more=true`), fwd+rev** — `flipMoreReply` forces a continuation,
      1001 injected on the continuation frame; asserts no dup/drop (`TestSimRangeWrongShardMidScan`).
    - **`future_version` (1009) / `process_behind` (1037) → QueueModel backoff** — inline 1009/1037
      on a read advances `failedUntil` + grows `futureVersionBackoff`
      (`TestSimInlineFutureVersion_QueueModelBackoff`; single-SS asserts QueueModel state, the cause).

---

## SELECT-path CURRENT_* statement-stability (small, follow-up)

- [ ] **Thread the statement clock through the executor's row eval contexts.** `values.StatementClock`
  (RFC-181 fold) makes CURRENT_TIMESTAMP / CURRENT_DATE statement-stable wherever the evalCtx carries
  a clock — the INSERT…VALUES fold passes one (`stmtClock`, insert_cascades.go). The SELECT path does
  NOT yet: a projection row evaluates against `*values.RowEvalContext` or (when no bindings exist) the
  BARE `values.OrdinalRow`, neither of which implements StatementClock, so each row's CURRENT_TIMESTAMP
  falls back to `time.Now()` and can drift across rows within one statement (SQL requires per-statement
  stability). Fix: stamp a statement time on `executor.EvaluationContext` at ExecutePlan entry, carry it
  through the With* copies, expose it from RowEvalContext AND the bare-row wrapper path
  (`rowEvalContextFor` / RowContextPositional — the bare OrdinalRow shape needs a wrapping or a
  clock-bearing twin). Pin with a plan over enough rows that a drift would be observable via
  two CURRENT_TIMESTAMP projections comparing unequal.

## Test infra (low priority)

- [ ] **Parallelize the whole `//conformance` suite via stdlib `t.Parallel` (drop Ginkgo). [LOW PRIO — RFC-082 follow-up]**

  **Goal.** Cut the Go↔Java conformance suite wall time (~122s today) by running *every* cross-engine
  check concurrently, uniformly — no bespoke fan-out. Today only the two SQL loops are parallel
  (each via its own hand-rolled goroutine pool); the ~40 FDB conformance families run serially.

  **Hard constraint: bazel-only.** CI is `bazelisk test //...`, which runs each `go_test` binary
  **once, directly** (serial invocation). So the only available parallelism is **in-process**.
  Ginkgo cannot parallelize in-process — its only parallel mode is the `ginkgo --procs=N` CLI, which
  spawns N worker *processes* (each would spin its own FDB container → the 290-failure resource
  exhaustion already observed) and runs **outside** `bazel test` (loses result caching + the Java
  server's bazel runfiles). Therefore the suite must move **off Ginkgo onto stdlib `testing` +
  `t.Parallel()`**, run with `-test.parallel=N` (bazel `go_test` honors this in-process, cached,
  runfiles intact). This also finally aligns the suite with the house rule ("All tests MUST call
  `t.Parallel()`") — it's the lone serial holdout.

  **Measured profile (121.6s wall, 112s in specs; `ginkgo-report.json` from a `--nocache_test_results`
  run):** container+DB startup ~10s (serial floor); `RunSql Harness` (SeedRunCorpus, ~1620 entries)
  36s — **already** 8-Java-server parallel; `yamsql A3` (859 specs) 20s — **already** 8-server
  parallel; **~40 FDB conformance families ≈ 56s — SERIAL, on the single global Java server.**

  **The load-bearing finding — the ceiling is JVM count, not Go concurrency.** The suite is
  Java-JVM-throughput-bound and JVM count is **memory-capped on CI** (16 JVMs is exactly what caused
  the earlier conformance CI timeout; 8 is the safe ceiling). The SQL work already runs 8-way — that
  56s combined is `total_java_work / 8_servers`; unifying the two pools into one does **not** speed it
  up (same work, same servers). So the **SQL floor is ~56s @ 8 JVMs**, and the rewrite's real win is
  folding the **56s serial FDB tail** (currently on *one* server, sequential) **into** that parallel
  window → **~122s → ~70-75s (~1.7x) @ 8 JVMs**. Beating ~70s needs **more JVMs** (memory), not more
  parallelism. "Everything is parallelizable" is true mechanically, but does not buy 8x here.

  **Approach (incremental, safe).** stdlib `Test*` funcs coexist with Ginkgo's `TestConformance` in
  one package (they share globals; Go runs the sequential Ginkgo blob first, then the `t.Parallel`
  batch together) — so migrate **family-by-family** with a green + spec/assertion-**count-parity**
  gate after each (silent coverage drops are the exact CLAUDE.md failure mode). Steps: (1) move
  container + Go DB + a pool of N Java servers into `TestMain` (all servers spawned before any test →
  preserves the "no JVM spawn during a query" GRV-lag discipline); Gomega assertions stay verbatim via
  `g := NewWithT(t)`; `BeforeEach` → a setup helper; nested `Describe` → flat test names / `t.Run`
  subtests. (2) Convert each FDB family (already UUID-tenant-isolated → inherently parallel-safe).
  (3) Convert A3 + SeedRunCorpus to `t.Run(..., t.Parallel())` subtests and **delete** the hand-rolled
  worker pools + `precomputed` map + `results[]` — this is the "stop special-casing A3" cleanup.
  (4) `-test.parallel=N` via the `go_test` `args`. Keep the FDB-1020 conflict-retry (shared catalog).
  Benchmark stays gated (`CONFORMANCE_RUN_BENCHMARK`). Query-engine-adjacent → needs Graefe +
  Torvalds + @claude + codex.

  **Cheaper alternative (no rewrite, ~zero risk, ~1.3x):** just raise the existing SQL pool 8→12
  (`CONFORMANCE_A3_POOL_SIZE` / `CONFORMANCE_SEED_PARALLELISM`) if the CI runner's memory allows —
  shaves the SQL floor without touching the green, reviewed suite. The FDB tail stays serial.

  **Why low prio.** The suite is green and freshly reviewed; ~1.7x for a ~32k-line mechanical rewrite
  of wire-compat-critical tests is a weak risk/reward, and the real speed lever (JVM count) is
  memory-bound regardless. Do the cheap JVM-count bump first if speed is ever urgent.

## Exploration: a second, FDB-native vector index (Go-only — NOT Java parity)

- [x] **Explore an FDB-native ANN index for a high-latency networked KV store — REALIZED by SPFresh (RFC-094).**
  *Status: the headline question ("build an FDB-native ANN index for this substrate, and which?") is
  answered — **SPFresh**, the top candidate below, is built, shipped, and SQL-exposed; the authoritative
  tracker is `rfcs/094-spfresh-status.md`. The OTHER candidates below (DiskANN/Vamana, batched beam
  search, atomic-append build) remain **parked alternatives/additions**, NOT blocked-on or
  needed-by SPFresh — future ideas on file, not open SPFresh work.* This is a deliberate Go-only extension, NOT a Java-parity item —
  Java has no such index, so it is allowed under "query reach may exceed Java" **only if** it ships as
  a separate index type with deep test coverage. **Wire-format tradeoff (must be stated up front):** a
  new on-disk graph/posting-list layout is *wire format*; Java's `VectorIndexMaintainer` cannot
  read/write it, so this index is **Go-built and Go-read only** — it forfeits cross-engine sharing for
  that index. That is the cost of admission, not a free lunch.

  **Motivation.** The existing HNSW index is now **100% Java-faithful** (the Go-only cross-transaction
  `sharedNodeCache` was removed for compliance — see `hnsw.go`). Being faithful, it inherits Java's
  latency profile on FDB: classic HNSW assumes O(1) RAM and does 50–200 *sequential, data-dependent*
  pointer-chasing reads per op; on FDB every hop is a ~0.3–0.5 ms round-trip, so search/build are
  round-trip-bound (block profile: `Transact` ~35% + `Commit` ~24% of build time; `fdbserver` <1 core;
  client ~7/24 cores). Java hides this with async `CompletableFuture` fan-out; Go's synchronous client
  cannot. The honest fix is not more caching bolted onto HNSW — it's an index whose *algorithm* fits a
  networked KV store.

  **Candidates (ranked by fit / payoff):**
  - **SPFresh** — *in-place incremental update for disk-based ANN* (LIRE/centroid-partitioned posting
    lists + lightweight rebalancing). Most interesting for THIS substrate: it directly targets the
    build/freshness + concurrent-writer problem we hit (the single-writer lock + FDB-1020 conflict
    storm on shared graph nodes). Posting-list partitions map cleanly onto FDB subspaces; updates are
    local to a partition → far less cross-writer contention than HNSW's shared adjacency mutation.
  - **DiskANN / Vamana** — single flat graph, higher degree + long-range edges → a search touches
    *fewer* nodes with *more* neighbors each, amortizing per-read latency. Pairs with PQ/**RaBitQ
    (already in-tree, `pkg/rabitq`)** for in-memory distance, fetching full vectors only for finalists.
  - **SPANN** — cluster + posting-list; turns the random-access graph walk into a few large
    `GetRange` reads (one round-trip for many keys — exactly what FDB is good at). Recall/locality
    tradeoff vs a navigable graph.
  - **Batched beam search** — *not a new index*: keep HNSW but expand the whole `ef` frontier in one
    batched multi-get per round instead of node-at-a-time, collapsing N sequential hops into log-depth
    batched rounds. **Wire-neutral** (no format change) → the cheapest real query-latency win and a
    good first step regardless of which index above we pick. Could even land on the existing HNSW.
  - **FDB-native build primitive — atomic-append neighbor lists.** If adding an edge is an FDB atomic
    mutation (no read-modify-write), writers don't register a read-conflict range on the neighbor →
    no 1020 storm → concurrent multi-writer build becomes correct *and* fast without the single-writer
    lock. Applicable to HNSW or a new index.

  **Outcome:** SPFresh was chosen, prototyped, and shipped (RFC-094) — that step is **done**. The one
  genuinely-still-open, wire-neutral idea from the candidates above is **batched beam search** on the
  existing HNSW (collapse N sequential hops into batched rounds — the cheapest query-latency win, no
  format change); DiskANN/Vamana and the atomic-append build primitive remain unscoped parked
  alternatives. None is open SPFresh work.

- [x] **fdbgo/wire: `TestPrecomputeSize_GetReadVersionRequest` never runs in CI and fails when run.**
  — DONE (RFC-095, wire ground-truth net repair). The hand test was stale (it omitted the 8-byte
  fake-root object C++ `save_helper` allocates) — deleted; the production serializer is pinned
  byte-exactly instead. The repair went much further than the original item; the net was dead on
  every axis and, once running, caught **three real bugs**: (1) generated marshal omitted the
  RelativeOffset for EMPTY vector-of-struct fields where C++ writes the shared-empty offset
  (`flat_buffers.h:964` unconditional write) — Go commit-request bytes diverged from libfdb_c;
  (2) `parseSplitRangeReply` decoded ZERO split points from every real reply (splitPoints is a
  FlatBuffers offset-vector, not an inline blob) — production `GetRangeSplitPoints` never worked,
  the e2e tolerated empty; (3) `parseCommitReply` read a conflict-shaped
  `CommitID{version: invalidVersion}` as a SUCCESSFUL commit (C++ throws not_committed,
  `NativeAPI.actor.cpp:6726`; latent — proxy only sends that shape under report_conflicting_keys).
  (`parseWaitMetricsReply`'s envelope-`UnmarshalFDB` was originally claimed as a 4th bug; Torvalds'
  mutation probe disproved it — correct by layout, ErrorOr's value offset coincides with FakeRoot's
  field 0; the rewrite to the canonical `ReadErrorOrInto` walk stands as hygiene only.)
  Also: extractor pins reply-promise tokens (deterministic vectors), emits reply-direction vectors
  for all 9 reply types the client parses (field-value asserted against the PRODUCTION parsers in
  `client/reply_ground_truth_test.go`), generator now reproduces the hand-fixes that lived in
  DO-NOT-EDIT files (KeyRangeRef swap-inversion, OOM cap), bazel data deps added + every skip in
  the net is now a Fatalf, orphan `wire/conformance_test.go` + dead justfile recipes deleted.

## SPFresh — tracked in RFC-094 (status)

All SPFresh tracking — current state, shipped work, open items, frozen
performance, and measured-negative levers — is consolidated in the authoritative
tracker **`rfcs/094-spfresh-status.md`**. The former "multi-tenant scale-out" and
"recall at scale" sections (every item closed) moved there; the SQL surface is
Phase 9 above (shipped).

Open work (detail + file:line in the RFC):
- **Tier 1:** SPFresh has no chaos/model-based fault coverage — the whole
  lifecycle incl. RFC-104 refinement is untested under injected faults and
  refiner-vs-rebalancer concurrency (highest-value gap); refresh
  `SPFRESH_OPERATIONS.md` for the refinement loop (stale wrt RFC-104).
- **Tier 2:** changelog chunking for >~267M-vector single-store builds
  (`spfresh_build.go:120`); a reference maintenance worker looping sweep+refine on
  a cadence (today they're library entry points a deployment must wire).
- **SQL nice-to-haves:** yamsql vector port, `ef_search` FDB behavioral test,
  OR-of-two-KNN execution test, window-in-`WHERE` `42F21` rejection.

## RFC-165 — ANSI SQL Core conformance backlog (read-side reach beyond the Java port)

Generated scorecard: `SQL_ANSI_CONFORMANCE.md` (Ledger A) tracks SQL:2023 Core (176 features per
PostgreSQL 18). `Go?` is derived from `# ansi:` corpus tags — never hand-typed. The **shared-gap**
rows (Java and Go both lack the feature) are this backlog; each is a wire-safe **read-side**
extension and ships under the full query-engine gate (Graefe + deep coverage). The **divergence**
rows (Java has it, Go rejects it) are RFC-164 port-fidelity bugs, not this list.

Shared ANSI gaps surfaced so far (extend by tagging more corpus + completing the roster, Phase 1):
- [ ] **E091-07 `COUNT(DISTINCT)` / DISTINCT quantifier** — rejected 0A000 in both engines. The
      single highest-value Core gap. (Pinned today as `# ansi-gap: E091-07` on `count_distinct.yaml`.)
- [ ] **E071-01 / E071-03 `UNION DISTINCT` / `EXCEPT DISTINCT`** — only `UNION ALL` works (E071-02).
      No Cascades dedup rule. (`# ansi-gap: E071-01` on `union.yaml`.)
- [ ] **E061-11 subqueries in `IN` predicate** — rejected 0AF00 in both. (`# ansi-gap: E061-11` on `subquery_in.yaml`.)
- [ ] **E021-04/05/06/08/09/11 string functions** (`CHARACTER_LENGTH`, `OCTET_LENGTH`, `SUBSTRING`,
      `UPPER`/`LOWER`, `TRIM`, `POSITION`) + **E021-07** string concatenation — no function-catalog
      entry in either engine (42883). A whole Core subfeature family.
- [ ] **E011-03 `DECIMAL`/`NUMERIC`** — no exact-decimal type; BIGINT division truncates.
- [ ] **E141-04/06/07 `FOREIGN KEY` / `CHECK` / column defaults** — only NOT NULL/UNIQUE/PK today.

**Phase 1 (continuing): tag the rest of the corpus.** Each `# ansi:`/`# ansi-gap:` tag on a yamsql
scenario moves a row from `untested` to a real status. The drift guard
(`TestAnsiLedgerEvidenceExists`) rejects a tag whose scenario lacks the matching outcome, so the
scoreboard can't lie. As Go closes a gap, flip happens automatically when the new feature's scenario
is tagged — never by hand-editing the doc.

**RFC-165 follow-ups (tracked, non-blocking):**
- [ ] **Verify the `Java?` roster facts against the live 4.12.11.0 server.** The `Java?` column in
      `ansi_roster.go` is currently a hand-authored frozen-version *assertion* (sourced from
      SQL_CONFORMANCE.md), structurally contained (it can't inflate the Go headline — see RFC-165 §4.6)
      but unverified. As A3 cross-engine coverage grows, diff each tagged feature's `Java?` against the
      conformance server so the fact becomes *verified*, not asserted, and flag any mismatch. Per
      Torvalds + Graefe review of PR #400.

### [ ] QP-REF-BIND: per-reference binding + the deferred ordinal classes (impl-review condition, W4-left)

One authority for three deferred pieces the W4-left review flagged (previously scattered as
"the 7.1 charter" — TODO 7.1 [alias-namespace unification] is DONE; this is the SUCCESSOR item):
1. **Per-reference ambiguity + fresh-id gating for duplicate FROM aliases** — Java's exact
   per-attribute 42702 at reference resolution (Go approximates at the FROM walk today; two
   marked divergence corners in the corpus: SELECT-*-over-duplicates, the predicated
   disjoint-column form).
   **DESIGN SUBSTRATE BANKED (2026-07-05, RFC § "QP-REF-BIND item 1 — design substrate"):
   19-shape live-Java probe verified every premise (SELECT * answers with duplicate labels;
   per-attribute WHERE binding; "Ambiguous reference X" byte-text for dup AND distinct
   aliases; qualified-star/table-row findFirst leftmost; the `..., a AS b` 42712 lazy quirk).
   Mechanism M1–M6: scope accepts duplicates + per-attribute qualified resolution; F3-ruled
   per-leg binding ids (later duplicates mint fresh); gate lift + binding-keyed seed; star
   layout fork F-A and message-unification fork F-B for the Graefe design ruling; ordering
   constraint — front-end acceptance and back-end binding never live separately
   (mis-pushdown wrong-rows hazard).
   **ITEM-1 COMPLETE (2026-07-05, PR #481): c1 (dark mint + binding-keyed seed, 34872539b +
   fix round) + c2+c3 (the lift — per-attribute resolution + FROM-walk 42702 retirement +
   SELECT-* star layout, 5860e3454) + the review-response (4e78ef2c2), Graefe ACK + Torvalds
   ACK on HEAD. Java's per-attribute model is LIVE: duplicate FROM aliases register per-leg,
   references resolve per-attribute (42702 at resolution, byte-equal to Java), SELECT * answers
   with Java's positional duplicate-column layout. All three flip corpus entries at parity
   (annotations deleted); dual-window + live-Java conformance + 1M stress green. Full record in
   RFC § "QP-REF-BIND item 1 — c2+c3 record". codex + @claude remain the PR-side gauntlet.**
   **c4 — the review round (2026-07-05): the PR gauntlet caught two REAL post-"COMPLETE"
   bugs (independently confirmed by both PR-side reviewers), both reproduced red-first and
   fixed; pinning their fold twin exposed a third. P1: dup-alias ORDER BY/GROUP BY keys kept
   the display alias while the gated join row is binding-keyed (silent wrong rows / NULL
   group keys) — sort+group keys now route through ResolveQualifiedProjection, group-key
   datum keyed by bare Field. P2: correlated EXISTS over an un-collapsed cross join — bisect
   pinned the buried-reference class as an ITEM-2 regression (worked pre-item-2) —
   duplicate-preserving outer scopes + gated flatten binding correlations +
   rebasePlanOuterRefsOrdinal (buried refs in the existential subplan → merged positional
   row, expression-gated, fail-closed verified). P3: the projected-EXISTS fold served NULL
   for a later dup leg's columns — buildExistentialJoinSelect + classifySortSource now speak
   bindings end-to-end. 7-shape FDB pin + 6 corpus entries live-verified + 2 dual-window
   declared-difference carve-outs (binding-qualified reads exist only positionally). Full
   record in RFC § "QP-REF-BIND item 1 — c4 record". Follow-on booked below: aggregate
   output metadata drift (labels/types) vs Java.
   MERGED (2026-07-06): PR #481 squashed to master 36c938f0a after c5 (the minted-binding
   loud-decline guard, RFC § c5 record) and c5b (P4e/P4f pins) — four-gate ACK on a789b66a9
   (architecture + code-quality + both PR-side reviewers), CI 6/6, stress at master parity
   (161.84s vs 161.68s). Item 3 UNBLOCKED.**
2. **Existential-flatten ordinalization** — translateJoinWithExists keeps the ANCHORED seed
   until the 2+1 select's data-access/correlated-FlatMap implementation paths bind legs
   POSITIONALLY (the ordinal seed was corpus-reverted twice: BakedNameContextError live).
   **DESIGN SUBSTRATE BANKED (RFC § "QP-REF-BIND item 2 — design substrate"): E2-validated
   executor binder; the W4-left rebase machinery is currently DEAD on live SQL (enclosure
   forced under EXISTS — the slice's commit-4 lift). DESIGN RULING: ACK with amendments
   (RFC § "design ruling + commit-1 record"). COMMIT 1 MERGED (PR #469, master 33291617d,
   four-gate ACK — Graefe, Torvalds after two converted NAKs, codex P1+P2 fixed-and-pinned,
   @claude ×4): the no-op existential residual (EXISTS == NOT EXISTS rows on
   LEFT+EXISTS) root-caused across four layers and fixed with the Java-shaped correlated
   step-1 (buildCorrelatedFlatMapPlan; the audited decline-only fix was insufficient —
   REWRITING promotion drops the unmerged member; full record in the RFC) + the 1+1 path's
   buried-leg rebase; unmasked matrix A–H pinned. DISCOVERED + FIXED on the branch:
   derived-alias EXISTS correlation 42703 (scope registration + the LogicalCTE alias
   carrier on all three derived arms) and the codex-caught CTE-shadow regression
   (cteShadowStack lexical scoping). LOUD-LIMITATION pins with exit gates (never wrong
   rows; flip to rows asserts): scalar-subquery-inside-EXISTS over a bare-scan outer
   (matrix class K — exit gate = the item-2 positional binders, commits 2–4);
   CTE-shadowed derived alias + EXISTS (buildDerivedTableSource resolves derived bodies
   against the catalog only — teach it CTE bodies); fetch-shell walk-terminators under
   planResultValue (same binder exit gate). The alias-unchecked frontier fallback
   follow-up (values.go evaluateCorrelated) dies with commits 2–4.**
   **CHARTER EXTENDED (post-W4-left sequencing ruling): absorbs the under-existential unnest
   class (`FROM t, t.arr AS v WHERE EXISTS(…)` — the W5 F4 rider's booking gap, closed here)
   and the EXISTS-rider clusterArity poison (a cluster whose leg filter/project carries
   exists subqueries) — same root cause, one slice, one review. The SCALAR-rider poison
   absorbs CONDITIONALLY: same binders → same slice; W4b-seed rework needed → immediate
   follow-on. Each absorbed class gets its own gate-reason string + dualwindow pins.**
   **ITEM-2 CHARTER COMPLETE (2026-07-05): commits 1–4 (PRs #469/#471/#472/#475) + 5a (#476,
   the structural exit gate) + the B/C/D/E wrong-rows batch (#478) + 5c (#479, class-K →
   rows) + 5b (#480, rider transparency) ALL MERGED, four-gate each; the EXISTS-rider and
   uncorrelated-scalar-rider poisons are LIFTED and the under-existential unnest is ordinal.
   Item 3 (below) now unblocked.**
3. **Mixed-nesting LEFT widening** — the joined-preserved class (clustered legs under a
   LEFT/RIGHT box) stays pinned residual until the flattened-cluster seed can name buried
   sources (the W4 dissolution ruling's scope). Retires the gate's :138-141 clustered-leg
   poison, the :102-113 enclosure guard, and ordinalEligible's LEFT/RIGHT leg-ineligibility;
   with items 1+2 it JOINTLY drives NewScalarSubqueryAnchoredRecord to zero callers.
   **MUST land AFTER item 2** (the enclosure guard names existential/unnest parents;
   retiring it before positional binders exist re-opens the mixed-nesting wrong-rows class).
   **IN FLIGHT (PR #483, feat/rfc173-item3): design ruling banked (three gate-arm commits,
   amendments A–J; the zero-callers claim STRICKEN — deletion rides S4; the FOURTH site
   recorded). c1 MERGED-to-branch 1ac0fe54f (S1 box roots + amendments C/D/E/F + the F-C
   guard; rfc153 plan pins verbatim green). c2 e0fcd2496 (S3 + clusterArity preserved+1;
   the RIGHT-box name-collision subs-only rule at all three layout sites; amendment G
   FULL-over-LEFT pin; pins re-cut per H). c3 in flight: LEFT-box-dup flip PINNED (P5 —
   the item-1 c5 loud class narrows as designed), records, exit gates.**

**Sequencing (Graefe ruling, banked in the RFC):** (riders ∥ item 2 ∥ item 1) → item 3 →
unnest-residual slice → S4. The riders are standalone and start immediately:
- [x] **Rider: bound `positionalTypeCache`** — MERGED (PR #468): wipe-at-cap 4096 with a
      miss-path mutex (exact bound, 8-worker -race churn pin; the lock-free variant
      transiently overshot — review finding).
- [x] **Rider: recursive-CTE leg remap hardening** — MERGED (PR #468): the read arm now
      fires from NAME-PROVENANCE classification (verbatim iff unaliased plain FieldValue),
      killing three garbage-correlation classes the string grammar misread (expression
      renderings, float literals, dotted quoted aliases — all red-first pinned + live-Java
      corpus entries). The FULL rendered-name-read retirement (ofOrdinalNumber at the
      insert boundary, Java's model) rides S4 with the name machinery; the quoted-dotted
      verbatim-Field residual is documented at the site (pre-existing, S4-scoped).
- [ ] **Unnest-residual completion slice** (books A3's W5 fail-open declines: box-leg
      owners, multi-segment `t.a.b` paths, CTE/derived rotation owners, chained unnests;
      under-existential arrives via item 2's binders; the BARE-TWIN duplicate-column decline
      rides until S4 — folded into the atomic commit per the circularity ruling, with the
      differential covering it name-model-side until then).
      **Progress (on `feat/rfc173-item3`, pending atomic-slice merge + gate re-ACK):**
      c1 (classes 1+2 — box-leg owners + multi-segment struct paths) DONE, both in-session
      gates ACK + codex re-confirm tracked for quota. c2 (class 3 — CTE/derived owners via
      body-projection→descriptor, positive whitelist, P2a-closed) DONE, both in-session
      gates ACK. c3 (class 4 — chained unnests `t.arr AS x, x.sub AS y`) DONE: nested
      FlatMap-over-FlatMap residual, all 7 Graefe conditions pinned
      (`rfc173_w5_chained_unnest_fdb_test.go` 11 subtests + `TestSelectMergeRule_ChainedUnnestBarrier`
      white-box). Two real bugs found + fixed by the FDB e2e: 3+-link enclosure collapse
      (chained dispatch de-gated from `!prevEnclosure`) and AT-on-chained-owner false 42809
      (`atOnNonArraySource` now recognizes `FindOwnerUnnest`). Remaining before merge: slice
      exit gates (dual-window, 1M stress, rfc153 verbatim, live-Java), codex on quota reset.
- [ ] **Rider: the minted-binding loud-decline class flips to rows** (item-1 c5 —
      the review-round guard): the declared-loud shapes over duplicate FROM aliases,
      each pinned in rfc173_item1_keybinding_exists_fdb_test.go (P4a–P4f) with the
      never-wrong-rows drain assert. Exit gates per shape: (a) leg-independent EXISTS
      over a minted-binding gated flatten (P4e) — flips when the executor's
      identity-FlatMap positional pass-through gate widens to key on the outer's own
      ordinal seed (probeOuterBakedType is the probe; flat_map_cursor.go documents the
      widening as the follow-on); (b) narrowed-off-the-gate flattens/joins
      (existential-alias collision P4a, arity ≠ 2 P4b, enclosure) — flip per-path as
      each learns the ordinal seed (item 3 / the N-way flatten); (c) correlated SCALAR
      subqueries over a dup outer (P4c/P4d) — flips when the scalar lowering speaks
      bindings (buildCorrelatedScalar's guard names the gap; label note: surfaces
      0A000 in SELECT position, wrapped 42703 in WHERE position); (d) the UNION face
      (P4f) — a dup-alias branch's per-attribute reference stays display-keyed and
      dies loud at the executor's ordinal guard; UPGRADE to a typed
      translation-time decline, then flip with the branch's ordinal seed. ALSO booked
      here: (arity-scope boundary) the dup-alias ARITY-3 correlated buried-EXISTS
      stays a LOUD ordinal decline (the c4 buried-reference rebase is arity-2 —
      implementJoinWithExistential's 2+1 shape), the N-way flatten slice widens it;
      (unnest owner) dup-alias unnest OWNER resolution is first-match-by-alias, not
      per-attribute (`q AS a, u AS a, a.arr AS e` → loud 42703 naming the wrong
      source) — classify vs live Java when the unnest-residual slice lands.
- [x] **Rider: aggregate output METADATA drift vs Java** (item-1 c4 probe finding —
      rows are parity, metadata is not; live-verified 4.12.11.0). DONE. Java rule
      (Expressions.getStructType): output name = `expression.getName()` bare (top-level
      clearQualifier) or `_N` positional if unnamed; type = the resolved
      `Value.getResultType()` off the flowed join-output record. Fixed (a)+(b) in
      `buildAggColumns`/`deriveColumnsFromAggregation`: (a) a QUALIFIED group key
      `d.dname` now labels the BARE `DNAME` (carried as ColumnDef.Label; the qualified
      Name stays the datum-lookup key the aggregate cursor writes — three-mirror
      agreement intact, values still resolve); (b) the group-key TYPE resolves against
      ALL join-leaf descriptors (allLeafDescriptors + descriptorForColumn), not just
      findLeafDescriptor's first leaf, so a far-leg key reports STRING/BIGINT not
      UNKNOWN. (c) is NOT a fix: plandiff ConformColumns already accepts Go's
      descriptive `COUNT(*)` against Java's anonymous `_N` when the type matches — a
      conformance-blessed wire-neutral read-side nicety; kept descriptive, pinned so an
      accidental relabel is caught. Pinned by `rfc173_rider2_agg_metadata_fdb_test.go`
      (6 subtests, red-first proven: D.DNAME + UNKNOWN drift reproduced with the fix
      disabled) + the value-flow pin.

### RFC-180 follow-ups (query-engine quality remediation)

- [ ] **Grouped-select ORDER BY widening (Graefe, be9e66c62 review):** a computed
      ORDER BY key over a grouped reshaping projection that is NOT a SELECT-list
      output currently declines typed 0AF00 (`translateSort` pull-up miss). Java
      widens the select with the missing expression and re-projects
      (`LogicalOperator.generateSelect`, `remainingOrderByExpressions`). Port the
      widening for the grouped path (the EXISTS-fold path already has its own
      instance booked under RFC-141 Phase 2 FOLLOW-UP). Pin: replace
      `TestGroupedOrderBy_UnderivableKeyDeclinesTyped` with a row-level pin.
- [ ] **Java-harness-verify the NULLS-default corpus flips (Graefe, efc07340e
      review):** the aggregate_null_edge / aggregate_with_null_groups /
      coalesce_in_join / distinct_patterns_java / order_by_nulls_java NULL-order
      pins were corrected from ParseHelpers.java source (ASC NULLS FIRST / DESC
      NULLS LAST). Close the provenance loop with a live cross-engine run (add
      the shapes to the plandiff corpus or run them via SqlPlanSteps).
- [ ] **Unify the two row-shape-transparency sets (Graefe nit):**
      `projectionOverAggregate` (translator) peels Filter/Sort/Limit;
      `underlyingGroupBy` peels Filter/Sort. One authority for "operators that
      pass the row through unchanged".
- [ ] **Grouped correlated EXISTS port (RFC-180 Y4):** Java plans
      `EXISTS(… GROUP BY … HAVING …)` (existential quantifier over a
      GroupByExpression); Go's correlated-EXISTS fallback rebuilds only
      FROM+WHERE and now declines TYPED 0AF00 (buildCorrelatedExists guard —
      before the guard it silently dropped the grouping and returned wrong
      rows, yamsql exists_with_aggregate). Port the aggregate into the
      rebuilt inner; restore the [Alice] rows pin.
- [ ] **Boolean-CASE WHERE predicate wrap (RFC-180 Y4):** Java wraps a
      boolean-typed non-BooleanValue (CASE/PickValue) used as a predicate in
      ValuePredicate(= TRUE) (Expression.java:371-400) and plans it as a
      residual filter; Go declines 0AF00. Port the wrap; restore the rows
      pins in case_when_in_java / case_exists_combo.
- [x] **Correlated scalar subquery in WHERE — scoped quantifier lowering
      (RFC-180 Y4 / CQ-4, extension):** a single correlated scalar over a
      single-source outer is materialized per outer row through the shared
      LEFT-scalar join, consumed by the WHERE predicate on a fresh typed row,
      cardinality-checked, and hidden by an outer-only projection. The
      `scalar_subquery_java` Bob/Dave/Eve rows pin is restored. Multi-source,
      mixed-subquery, projected-EXISTS, and dual SELECT+WHERE scalar
      compositions remain explicit 0AF00 boundaries.
- [ ] **Correlated scalar subquery in HAVING — quantifier lowering
      (RFC-180 Y4, extension):** outer HAVING still rejects typed 0AF00; its
      grouped-output row shape and aggregate-reference rewrite have not been
      proven compatible with the private scalar slot. This is distinct from a
      real-aggregate HAVING *inside* a correlated scalar subquery, which CQ-4
      supports and cardinality-checks after HAVING. Restore a dedicated HAVING
      rows pin only when that outer consumer is implemented.
- [ ] **Scalar subquery over a FROM-less SELECT (RFC-180 Y4, extension):**
      `SELECT (SELECT COUNT(*) FROM t) AS total` declines 0AF00 — the
      LogicalValues path carries no subquery plans. Restore rows pin when
      wired.
- [ ] **LIKE-prefix covering access path (RFC-180 Y4, plan-shape parity):**
      Java plans `WHERE name LIKE 'bl%'` over an indexed column as an
      UNBOUNDED covering index scan + residual LIKE + deferred FETCH
      (never a LIKE→range conversion — RangeConstraints.java:780). Go
      full-scans (rows correct). Implement the covering/filter-before-fetch
      path; then flip like_patterns_java's plan_not_contains pin to a
      covering plan_contains.
- [ ] **HAVING-EXISTS error-surface alignment (RFC-180 Y4):** Java rejects
      `SELECT COUNT(*) FROM t HAVING EXISTS(…)` at semantic analysis with
      42803 GROUPING_ERROR "Invalid reference to non-grouping expression …
      exists(q…)" (LogicalOperator.generateGroupBy →
      SemanticAnalyzer.isComposableFrom; live-probed). Go declines 0AF00 via
      the HavingExistsSubqueries planner gate. Align: reject at semantic
      analysis with 42803 + Java's message shape; flip the exists.yaml pin.
- [ ] **Comma join over a nested-shadowing CTE inside a CTE body
      (RFC-180 round-7 reach):** `WITH x(m,n) AS (…) SELECT p.m, p.n, q.m
      FROM x p, x q` inside a CTE body plans but dies at runtime with an
      ordinal-resolution error (P.N vs merged-row keys [P.M N Q.M] — the
      qualified/bare name-model seam, RFC-173 surface; stars and explicit
      columns both hit it). Single-source nested bodies work (pinned).
      Make the merged-row keys carry the qualified names the projection
      mints, or decline the shape at plan time.

### [x] N-way comma-join projected-EXISTS emitted a plan that could not execute — RESOLVED by RFC-190.1 (RFC-183 §15 finding)

**Resolution:** RFC-190.1 direct-emits the flat name-model
`[ForEach×N, Existential]` shape, uses a live-existential guard while
PartitionSelectRule decomposes it into ordinary binary NLJs, and retired
`implementNWayJoinWithExistential` atomically with that replacement. The
comma-join, explicit/buried-join, discriminating duplicate-column, four-leg,
WHERE/NOT-EXISTS, and SARG/stress regressions pin the replacement path.

**Historical RFC-183 comma-join diagnosis below (present tense in the original
record referred to that checkpoint, not current code).** At the RFC-183
checkpoint, `ImplementNestedLoopJoinRule.implementNWayJoinWithExistential` —
the N-WAY FLAT EXISTENTIAL arm — produced a plan for the comma-join reproducer
below that died at execution with:

    correlated FieldValue "V" (correlation "A") evaluated against an
    unbound/unrecognized context (*RowEvalContext (multi-leg row cannot serve a
    source-relative ordinal)) — no frontier row resolved (planner/executor bug)

Comma-join reproducer (a PROJECTED exists over >2 ForEach legs; a WHERE-EXISTS
does NOT reach this arm — it needs N>2 ForEach quantifiers plus a trailing
Existential in one flattened Select):

    SELECT a.v, EXISTS (SELECT 1 FROM d WHERE d.id = a.id)
    FROM a, b, c WHERE a.id = b.id AND b.id = c.id

PRE-EXISTING, not introduced by RFC-183: confirmed by reverting that RFC's memo
fix and re-running — identical failure. Instrumenting all 2407 corpus queries
counted ZERO arm firings, so that corpus neither exposed this comma-join crash
nor covered the distinct explicit-`JOIN…ON` path that already executed
correctly. The zero count did not establish that the arm as a whole had never
worked.

RFC-183 SHIPS NO REGRESSION HERE — proven by plan parity, recorded because the
commit titled "the N-way EXISTS local fix converts a crash into WRONG ROWS —
do not ship" is easy to misread as "the branch ships wrong rows". It does not:
that commit REVERTED the fix and changed only this TODO. The executed plan for
the reproducer is BYTE-IDENTICAL on master and on the RFC-183 branch —

    FlatMap(outer=PredicatesFilter(NestedLoopJoin(INNER,
      NestedLoopJoin(INNER, Scan(NA), Scan(NB)), Scan(NC)), [2 preds]),
      inner=FirstOrDefault(PredicatesFilter(Scan(ND), [1 preds])))

so the branch crashes exactly where master crashes (correct-or-loud), and
introduces NO silent wrong rows. The memo repoint changes costing/linkage, not
the extracted plan. Do NOT block RFC-183's merge on this bug, and do NOT
"resolve" it by applying the reverted `flatMapResult = rebased` — that is the
change that produces wrong rows.

Related but SEPARATE, already fixed on the RFC-183 branch: the same arm was
costing the whole N-way chain as `Scan(A)` — a memo-linkage bug. Pinned by
`TestNWayProjectedExists_OuterQuantifierMatchesExecutedPlan`
(pkg/relational/core/embedded). That fix does NOT make this comma-join plan
executable.

ALREADY TRIED AND REVERTED — TWICE, and the second attempt proved the fix is
ACTIVELY HARMFUL, not merely insufficient:

Rebasing the projected result value through `rebaseOuterLegValueOrdinal` (the
same treatment `joinPreds`/`existPreds` get, and the obvious candidate since
the RV is passed unrebased at rule_implement_nested_loop_join.go's
"passed through unrebased"). DIAGNOSED PROPERLY the second time — the
then-current code computed the rebase and then DISCARDED it
(`flatMapResult = projected`, not
the rebased value), and the rebase genuinely works: `A.V#1` (source-relative
ordinal) -> `q$N.V#3` (merged-relative). Applying `flatMapResult = rebased`
makes the projection RESOLVE, and a single-row query then EXECUTES correctly
against real FDB (`[10, true]`).

But a 3-row query then returns WRONG ROWS: `has_d` is TRUE for every row,
including id=2 which is absent from `nd` (correct is false). So the local fix
converts a LOUD CRASH into a SILENT WRONG-ROWS bug — the projection resolves
but the EXISTS correlation over the merged row evaluates wrong. That is
strictly worse (CLAUDE.md: "wrong rows green costs months"), which is why it is
reverted and must NOT be applied piecemeal.

WHAT THIS PROVES: the projection, the EXISTS correlation, and the ORDER BY key
are COUPLED through the merged-row name model. The three failure surfaces are
one defect:
  1. projection `a.v` — source-relative ordinal over the merged row (rebase
     makes it resolve but see above);
  2. the EXISTS `has_d` — evaluates wrong (always true) once the row is merged;
  3. ORDER BY `a.v` — the InMemorySort key stays source-relative above the
     projected FlatMap output (rule_implement_in_memory_sort.go bakes
     `sk.Value` as-is).
A local rebase touches only (1) and breaks (2). The fix must make the merged
multi-leg row present a coherent OUTPUT name model that all three resolve
against — which is exactly the qualified/bare seam, and a workstream, not a
rule tweak.

No yamsql scenario was pinned at that checkpoint: pinning the error string
would have promoted a defect to expected behaviour, and pinning the wrong rows
would have been worse. RFC-190.1 instead added row, shape, SARG, and stress
coverage for the replacement path.

LIKELY THE SAME ROOT CAUSE AS THE COMMA-JOIN-OVER-NESTED-SHADOWING-CTE ENTRY
above ("P.N vs merged-row keys [P.M N Q.M] — the qualified/bare name-model
seam, RFC-173 surface"). Dumping the FlatMap result value for the reproducer
gives

    RecordConstructorValue{Fields: [{Name: "A.V", ...}, {Name: "HAS_D", ...}]}

i.e. a QUALIFIED field name ("A.V") evaluated against a merged multi-leg row —
exactly the seam that entry describes. Treat the two as one defect until
proven otherwise; fixing the name model on merged rows plausibly closes both.
The guard that fires is values.go:902, keyed on
RootIsLegRelativeUnpinned() && rowIsMultiLeg() — deliberate correct-or-loud,
not the bug itself.

RFC-190.1 resolved this decision: the arm was removed only together with the
direct-emit + guarded-partition replacement, so the supported N-way shapes now
plan and execute rather than merely declining.

### [ ] RFC-183 residual: 32 no-quantifier memo edges (RFC-184 W2/W3)

RFC-183 drove genuine unreachable memo edges (a plan child its quantifier's
group cannot produce) from 158 to 0. It left 32 edges of a DIFFERENT class:
`scanPlanExpression`, the leaf adapter that reports no quantifiers while
wrapping a `TypeFilter(Scan)` that has children — so the memo models no edge
for that child. NOT a wrong-plan or wrong-rows defect today; the memo simply
does not see those children.

Ratcheted at a hard baseline of 32 by
`TestCorpusPlanReachability` (pkg/relational/conformance/explaindiff), which
FAILS if the count rises — so the class cannot grow unobserved.

Closing it is RFC-184's W2/W3 (`rfcs/184-plan-identity-structural-elimination.md`,
now on master), NOT a memoization change. Proven the hard way: retiring the
adapter for the bare plan drove the count to 0 but drifted 57 corpus queries /
49 shape flips — a point lookup became a full scan — because the adapter
supplies scan-comparison correlations and ordering/cost properties the bare
`PlanExprBase` does not. Plans must carry those properties first, verified
property-by-property. The inert half (GetRecordQueryPlan on all 41 plan types)
is already on the RFC-183 branch.

Do NOT "fix" this by re-retiring the adapter without the property work — that
is the change that caused the 49 shape flips.

### RFC-184 three-goal status (the stop-hook completion condition)

- **W1 — teach the differential harness cost + ordering: ✅ DONE.** rowdiff/cost.go
  + rowdiff/ordering.go, commits fcc197d75 (#505 cost/ordering axis) + 1be3d81c3
  (W1-finish ordering+statistics), both in HEAD ancestry.
- **W3 — centralize the 41 plan types' equality/hash: ✅ DONE (41/41).** All plans
  flow EqualsPlanWithoutChildren/HashCodeWithoutChildren through one structuralKey()
  builder (commits 721f71635 + 84e48fb30); every hand-copy eliminated, differ=0.
- **W2 — plans store children only as quantifiers, the wrapper layer dies: ✅ DONE.**
  ZERO physical*Wrapper structs remain (`grep 'type physical.*Wrapper struct'
  pkg/recordlayer/query/plan/cascades/ = 0`). The ref.Winner() set-op family
  collapsed at differ=0 (unordered_union 5022c0d7f, in_join+in_union a6bb74b07),
  then all 7 tail wrappers fell via CONSTRAINT-PRESERVING DISENTANGLE (freeze only
  the constraint-critical inner — correlation/ordering — in a detached FINAL ref;
  live edge for plain inners so a push-canonicalization can't strand a pre-push
  snapshot): first_or_default (74a07784e), predicates_filter (4e2e46dcc, +5
  Graefe-ACK'd sort-elisions), distinct (1b6fb461d), streaming_aggregation
  (cf0b18355), nested_loop_join+flat_map JOINTLY (ca3008b6b), in_memory_sort
  (b775b8572). The sort required a genuine cost-model fix (Graefe-ACK'd): the
  wrapper cost-over-first masked a latent bug where a redundant InMemorySort wins
  STRUCTURAL tie-breakers (depth +1, or a pushdown-split map/filter count) before
  the fewer-sorts comparison. RFC-184 initially gated 4 additional structural
  rungs when a sort was present; RFC-190 later proved the resulting 5-gate
  comparator non-transitive and superseded it with sort-transparent depth,
  unconditional Java rungs, and a promoted fewer-sorts comparison before the
  structural block. Enabling infra: the DML-aware explain-differ (51ee03bf5 — the SELECT-only
  differ was unsound; it let a fod DML corruption read clean) + the residual-
  correlation DML regression test (e2b2a7395). Every behavior change is Graefe-ACK'd;
  gated on memoinvariant unreachable=0 + rowdiff Ordering/Stats/Cost sweeps +
  yamsql-DML + FDB row-counts. NOTE the 32 ReasonNoQuantifier edges (all TypeFilter,
  via the scanPlanExpression composite adapter) are a SEPARATE RFC-183 residual, not
  a W2 wrapper — see the RFC-183-residual entry.
  THREE non-blocking follow-ups (NOT merge gates — Graefe + Torvalds ACK'd HEAD
  0e854e837 on PR #508 with these outstanding; (i)/(ii) are Graefe-named from the
  sort finale, (iii) is Graefe end-review finding #2):
    (i) root-cause why the unsplit-elided plan (Project(PredicatesFilter(Fetch(
        IndexScan)))) is ABSENT at the Project group for rowdiff seeds 132/214 — a
        W2 disentangled-capture search-completeness asymmetry, not a cost-model bug.
    (ii) ✅ DONE in RFC-190: hoisted the fewer-in-memory-sorts comparison above all
         affected structural rungs, made depth sort-transparent, and retired all
         5 Go-specific per-rung sort gates. Full rowdiff/yamsql/FDB sweep green.
    (iii) RecordQueryMapPlan/RecordQueryProjectionPlan inherit the empty
        PlanExprBase GetCorrelatedToWithoutChildren default while Java's
        RecordQueryMapPlan.getCorrelatedToWithoutChildren folds the result value.
        PRE-EXISTING (the retired physicalMapWrapper was equally empty — this PR
        changed nothing), but with the FlatMap resultValue-walk precedent now in
        place the fix is mechanical. Not a regression; a parity gap to close.

### [ ] Test infra — FDBCLIExec can hang 30 min on a stuck Docker exec (CI flake)

Surfaced by PR #508's end-gate CI: the `Cross-client wire differential` job
timed out at the 1800s Go test limit, stuck in FDB container init. Goroutine
dump: `FDBCLIExec → configureWithRetry → InitializeDatabase`, blocked on a
`chan receive` inside testcontainers' `Multiplexed()` output reader
(`exec/processor.go:124`) for 29 minutes — a Docker `fdbcli` exec whose output
stream never drained. `configureWithRetry` (foundationdb.go:444) only checks
`ctx.Err()` BETWEEN attempts, so it cannot interrupt a read already hung inside
`FDBCLIExec` (foundationdb.go:532); the passed `ctx` does not unblock the
Multiplexed stream read. One stuck exec ⇒ the full 30-min test timeout instead
of a fast retry. Re-running the job cleared it (transient Docker hang), but the
fragility is real and pre-existing. Fix: give `FDBCLIExec` a hard per-exec
deadline that actually abandons the Multiplexed read (run the exec+drain in a
goroutine, `select` on `ctx.Done()` + a short timer, and fail the attempt so
`configureWithRetry`'s backoff loop takes over). Out of scope for the planner
work; unrelated to any wire/query change.

### [ ] RFC-184 W2 — physical-wrapper collapse: 26 eliminated, deferred-winner tail remains

On branch `centralize-plan-equality-hash` (held for one super-thorough end
review per owner's one-big-PR ruling). Each `physical<Name>Wrapper` adapter
stored a plan's child edge a SECOND time (wrapper quantifier + plan snapshot);
collapsing makes the plan its own cascades expression carrying the LIVE memo
edge once, so the ordinal-binding "child stored twice" state is unrepresentable.

COLLAPSED (24, each `explain-differ differing=0`, full-hook green): scan, limit,
fetch, map, default_on_empty, explode, table_function, temp_table_scan,
temp_table_insert, vector_index_scan, index_scan, aggregate_index, values,
typefilter, insert, delete, update, intersection, merge_sort_union,
multi_intersection, recursive_level_union, recursive_dfs_join, union, projection.

ENABLER — DAG-aware plan extraction (plan_extraction.go): the `visited` map was
doing double duty — cycle guard (necessary) AND permanent de-dup (harmful).
Made it STACK-SCOPED (add on descent, `delete` on ascent, mirroring
extractTieBreakHash) so a SHARED sub-DAG (two UNION legs scanning t1) re-extracts
instead of dropping its 2nd child to nil. This unblocked union + projection.

DEFERRED-WINNER TAIL — the winner CRITERION is PLAN-SPECIFIC (breakthrough):
  Each of these plans ranges a child over a MULTI-MEMBER alternatives group.
  The DESIGN-C' INFRA (landed 5022c0d7f) makes this collapsible for plans whose
  correct child winner is the group's per-ordering winner (ref.Winner()):
    - planFromQuantifier consults ref.Winner() before the singleton panic;
    - planTypeFromQuantifier for GetResultType (type is member-invariant);
    - verifyNoShell + reachability tally count via GetQuantifiers not GetChildren.
  ✅ COLLAPSED on the infra (differ=0, memoinvariant+reachability+yamsql green):
    unordered_union (5022c0d7f), in_union + in_join (a6bb74b07) — the whole
    ref.Winner() set-op family.
  STILL WRAPPED — the per-plan empirical question is now ANSWERED: all four unary
  candidates REVERTED (each empirically tested on the infra, differ>0, tree left
  byte-identical). They are the genuine snapshot-decoupled tail; bare ref.Winner()
  floats their inner to the group's GLOBAL cost-winner, losing the frozen
  streaming-enabling / SARG-pinned member. Measured shifts:
    - distinct → differing=6 (distinct_join.yaml#1..3 + friends). The distinct-final
      rule yields distinct-over-EACH-inner-member (rule_implement_distinct_final.go
      :69,118), each snapshot-frozen so the cost model can pick
      distinct-over-(ordered inner) with STREAMING dedup. Collapse merges the
      InitialOf(m) singleton into the shared inner group → ref.Winner() floats all
      to distinct-over-(global winner), losing the streaming alternative.
    - streaming_agg → differing=3 (same shape: agg needs the grouping-key ordering
      to stream; float drops it).
    - in_memory_sort → re-sorts, wants cheapest-valid-ANY-ordering
      (findBestPhysicalPlan), not the per-ordering winner. BRACKETED earlier:
      ref.Winner() → 63 flips; no-Winner → 64 errors.
    - first_or_default → differing=3, predicates_filter → differing=16: NLJ-
      entangled SARG-push (constructed as inner legs inside the Graefe-gated NLJ
      rule); their float is SARG-push, not pure-ordering — collapse WITH nlj/flatmap.
  RESOLVED DIRECTION — mechanical collapse of the tail is EXHAUSTED; it is
  load-bearing architectural work. Two proposals tested + two empirical proofs
  (full ruling: scratchpad/w2-graefe-gated-remainder.md):
    - getWinnerForOrdering-at-extraction: Graefe NAK (cost-inversion; re-entangle).
    - relink-to-memo-winner (Graefe ACK'd as Java-faithful for SELECT): tested on
      distinct (cost-tied SELECT churn, differ=6 plan_regressions=0) AND on
      first_or_default — where it CORRUPTS DML ROWS. `DELETE ... WHERE EXISTS
      (correlated)` deleted ALL rows: the correlation lives in the SARG-pushed scan
      the fod snapshot froze; relinking to the non-SARG memo winner drops it.
  TWO GATE FINDINGS THAT BLOCK THE RE-BASELINE:
    - The explain-differ IS DML-BLIND — it dumps only SELECT stanzas (skips 252 DML
      stanzas). plan_regressions=0 does NOT cover DML. Only yamsql (real FDB) is a
      sound gate for the tail. The fod collapse read differ-clean while corrupting
      the DML DELETE/UPDATE-EXISTS path.
    - The tail wrappers' snapshots are LOAD-BEARING: correlation on DML paths
      (fod/predicates_filter), ordering preconditions (Streaming distinct/agg —
      relinking to a different-ordering winner is WRONG ROWS), join-order winners
      (rest). The single collapsed edge can't reproduce them; collapsing corrupts a
      differ-invisible path.
  So the tail is the correlation/ordering-PRESERVATION rework (the bare plan must
  REPORT the correlation/ordering its snapshot carried, or its inner group must be
  optimized under that correlation/ordering), a Graefe-designed owner-gated effort
  (its own RFC) — NOT RFC-184-W2 mechanical scope. Every further mechanical collapse
  attempt risks silent DML corruption; STOPPED pending the architectural design.
  The 2 dead structs (scan/filter) are removed (commit 2f1189b94).

NESTED_LOOP_JOIN + FLAT_MAP (joint) — GRAEFE DECISION, NOT deferred-winner:
  These two ARE Class B (singleton children; correlation flows through children,
  both wrappers' GetCorrelatedToWithoutChildren empty = plan default). The joint
  collapse was implemented + gated: build green, cascades+plans unit 2/2 (no
  PlanHash-nil — collapsing both together fixes the NLJ→FlatMap snapshot bridge),
  yamsql FDB conformance PASS (correct rows). BUT the corpus differ caught ONE
  shifted query, cte_error_codes.yaml#5 (a shared-CTE self-join):
    BEFORE: InJoin(PredicatesFilter(Scan(T1),[1 preds]), binding)   (residual filter)
    AFTER:  InJoin(Scan(T1,[=]), binding)                            (SARGed — better)
  plan_regressions=0; the AFTER is the canonical SARGed plan the cost model wants.
  Root cause: the NLJ wrapper's WithChildren keeps a plan SNAPSHOT (the residual-
  filter member); the collapsed NLJ honors the DAG-aware-extraction cost WINNER
  (the SARGed scan). So the wrapper is CURRENTLY MASKING a suboptimal plan on this
  query, and the collapse would fix it. This is not a regression — it's the
  collapse revealing the plan the cost model selected. But it breaks the
  differing=0 W2 soundness contract, so per STOP-on-shift it was reverted and
  flagged. Graefe must rule: (a) accept the improvement → land NLJ+FlatMap with
  differing=1 documented, or (b) require byte-identity first. The FromQuantifiers
  constructors + WithChildren are trivial to re-add; the 9-emitter repoint is
  mechanical once ruled. Recommend (a).
  INDEPENDENT CROSS-CONFIRMATION: a second session reproduced the full joint
  collapse from scratch and reached the identical result — same single shifted
  query (cte_error_codes.yaml#5), same before/after shapes, same root cause,
  unit + yamsql green. It additionally verified DETERMINISM on both sides
  (base-vs-base2 and cur-vs-cur2 each differing=0), so the flip is a stable,
  reproducible, benign improvement — NOT planning noise. Two independent
  derivations of the same conclusion strengthen recommendation (a).

WHY THE NAIVE FIX FAILS (proven, in_memory_sort): memoizing
getWinnerForOrdering(innerRef, PRESERVE) as a singleton child at YIELD would
satisfy the invariant — but RFC-069/076 DEFER the sort's inner winner to
EXTRACTION precisely because it is not stamped at sort-yield; getWinnerForOrdering
then falls back to findBestValidPhysicalExpr (yield-time cheapest = the
"yielded-first loser" RFC-069 warns against) → likely a cost-tied RFC-069/076
regression the differ may MASK. The real fix is architectural: either relax the
singleton invariant to resolve to the cost WINNER for deferred-winner plans (needs
the winner reachable from the plans package — ref.Winner() timing), or a
memo/reference construct that is "singleton for planning-time queries but defers
winner to extraction." Both touch the load-bearing singleton invariant → Graefe
gate applies. Full analysis + candidate designs A/B/C in the shift handover.

DESIGN C' ATTEMPTED EMPIRICALLY (in_memory_sort, then reverted — precise blocker
found): (1) planFromQuantifier consults ref.Winner() before the singleton panic;
(2) a new planTypeFromQuantifier (first-member, no panic) for GetResultType —
type is member-invariant for pass-through plans; (3) verifyNoShell (and every
planning-time STRUCTURAL "has children" check) uses GetQuantifiers, not
GetChildren (identity). RESULT: the singleton panic is GONE — TestSortElim_*
passes, the sort collapses. BUT the differ shifts 66 queries / 63 shape flips
(plan_regressions=0). ROOT CAUSE — the winner CRITERION is PLAN-SPECIFIC:
ref.Winner() is the group's winner for ITS requested ordering, but the sort
re-sorts so it wants the cheapest VALID member for NO ordering
(findBestPhysicalPlan, RFC-076) — the wrapper's WithChildren used exactly that.
ref.Winner() != findBestPhysicalPlan, so the collapsed sort bakes a dominated
ordered inner. So a UNIFORM planFromQuantifier→ref.Winner() is too coarse; the
deferred-winner rework needs PER-PLAN winner selection at extraction (sort:
cheapest-valid-any-ordering; set-op: per-leg; etc.), and that criterion lives in
the cascades pkg (findBestPhysicalPlan), which the plans-pkg WithChildren cannot
reach. The clean shape is a sort-specific extraction hook (like the LogicalSort
rebuildOrderedSpine that already exists) for the PHYSICAL sort — a Graefe-ACK'd
cascades change. Design C' infra (Winner-aware planFromQuantifier + type resolver
+ structural-check-via-quantifiers) is the RIGHT foundation; it needs the
per-plan winner hook on top.

SHARPENING (localizes the design): the sort's HintCost takes child costs as
PARAMS (does not walk GetInner) and HintOrdering computes from the sort KEYS
(not the inner). So the sort's cost/ordering are self-contained — the ONLY
planning-time GetInner/GetChildren callers are GetResultType (→ type resolver)
and verifyNoShell (→ structural GetQuantifiers). Therefore the PANIC is FULLY
fixable with those two changes alone; the GLOBAL ref.Winner() change in
planFromQuantifier is NOT needed for the panic and is what introduced the bias.
The 63-query shift is PURELY the extraction inner-selection: extraction
(extractBestPlanFromSelectorVisited) prefers innerRef.Winner() — an ordered
variant — while the wrapper's WithChildren OVERRODE it with
findBestPhysicalPlan (cheapest-valid, any ordering, RFC-076). The whole design
thus reduces to ONE localized change: a physical-sort case in the extraction
switch (rebuildWithFreshChildren / rebuildExpressionFromSelectorVisited) that
resolves the sort's inner via findBestPhysicalPlan, restoring the wrapper's
override. Everything else (the two structural/type fixes) is mechanical and
differ-neutral. That is the concrete Graefe-review unit.

BRACKETED EMPIRICALLY (two attempts, both reverted) — the findBestPhysicalPlan
extraction hook is MANDATORY, not optional:
  - WITH global planFromQuantifier→ref.Winner(): resolves the inner but to the
    WRONG winner (the group's ordering-winner, an ordered/dominated variant) →
    differing=66, shape_flips=63, plan_regressions=0 (correct rows, worse shape).
  - WITHOUT it (just the two mechanical fixes + collapse): TestSortElim_* still
    passes (panic gone — the sort's HintCost/HintOrdering don't walk GetInner),
    BUT extraction cannot resolve the multi-member inner at all →
    differing=109, plan_regressions=64, and 64 NEW plan errors (258→322).
  So the two uniform options bracket the answer: extraction MUST resolve the
  multi-member inner (some winner is needed — no-Winner gives plan errors) AND
  it must be findBestPhysicalPlan specifically (ref.Winner() gives wrong plans).
  The per-plan (sort-specific) extraction hook is the ONLY thing that works;
  the two structural/type fixes are its necessary companions. That is the
  precise, empirically-bounded Graefe-review unit for the physical sort.

### [x] FUZZ: GetCorrelatedTo stack-overflows on a cyclic reference graph (PRE-EXISTING) — FIXED by RFC-185

RESOLVED: the cyclic memo was constructed only by the Go-only filter/set-op
commutation rule family (Push/PullCommonFilter through/above Intersection and
Union). RFC-185 removed all four rules (both review gates ACK: RFC-review +
implementation-review). With no rule constructing a cyclic memo, GetCorrelatedTo
has no cyclic input; the four previously-crashing fuzz targets are clean at
670k-925k execs, zero corpus drift. GetCorrelatedTo's missing cycle-guard is
deliberately NOT added — a guard masks a cyclic memo (principle #9), and the
crash was the honest surfacing of a rule bug now removed at the source. If a
future rule reintroduces a cyclic memo, that is the bug to fix, not the guard to
add. The diagnosis below is retained as the record of how it was found.


`FuzzPlanner_MemoConsistency` and `FuzzPlanner_Determinism` crash with
`fatal error: stack overflow`, unbounded recursion at
`expressions/reference.go:752` — `GetCorrelatedTo` recursing through
`childRef.GetCorrelatedTo()` with NO visited-set / cycle guard.

REPRODUCER (4 bytes): `[]byte("\x7fyy1")` = [127, 121, 121, 49].

    go test ./pkg/recordlayer/query/plan/cascades/ \
      -run='^$' -fuzz='^FuzzPlanner_MemoConsistency$' -fuzztime=20s

PRE-EXISTING, not an RFC-183 regression — PROVEN: the identical seed
stack-overflows on pre-RFC-183 master (15dc17a82), and RFC-183 left
GetCorrelatedTo's body byte-unchanged (verified by `git diff 15dc17a82..HEAD`
on that function). Found by running the planner fuzz targets after the RFC-183
merge (fuzz is non-negotiable for planner changes — this is fuzz doing its
job).

ROOT CAUSE — the cycle is created during RULE APPLICATION, not by the harness:
`buildFuzzExpression` builds a strictly ACYCLIC tree (depth <= 3, fresh
`InitialOf` children), so a 1e9-byte recursion is impossible from the input
alone. Some rewrite rule in `exploreRewriting` produces a Reference that
transitively ranges over itself, and GetCorrelatedTo (no cycle guard) then
recurses forever.

THE OFFENDING RULE — IDENTIFIED. Instrumenting `Reference.Insert` to detect a
just-inserted member that transitively reaches its own reference points at:

  **`PullCommonFilterAboveIntersectionRule.OnMatch`
  (rule_pull_common_filter_above_intersection.go:59)**

That rule turns `Intersection(Filter([P],A), Filter([P],B))` into
`Filter([P], Intersection(A,B))` — the REVERSE of
PushFilterThroughIntersection. It builds `newX = Intersection(A,B)` from the
filters' inner quantifiers, calls `newXQ := ForEachQuantifier(
call.MemoizeExpression(newX))`, and yields `Filter([P], newXQ)`.

MECHANISM: `MemoizeExpression` INTERNS — it may return an EXISTING reference.
When `newX` interns to a reference that transitively reaches the reference
currently being explored (the one holding the original intersection), the
yielded Filter's quantifier points back into it and closes a cycle. This rule
and its inverse (PushFilterThroughIntersection) interning against each other in
the same memo group is the shape. GetCorrelatedTo then recurses forever on the
resulting cyclic Reference.

CONFIRMED — the crash needs the INVERSE PAIR. A subprocess experiment
(zz_inv, reverted) plans the seed three ways: all rules → CRASH; exclude
`PushFilterThroughIntersectionRule` → SURVIVES; exclude
`PullCommonFilterAboveIntersectionRule` → SURVIVES. So the two INVERSE rewrites
interning against each other in one memo group are what close the cycle —
neither alone does it.

DEEPER ROOT CAUSE — BOTH RULES ARE GO-ONLY. Java (4.12.11.0) has NO rule that
pushes a filter through, or pulls a filter above, an Intersection. Its
filter/intersection rules are ImplementFilterRule, ImplementIntersectionRule,
and filter PUSHDOWNS (PushDistinctBelowFilter, PushTypeFilterBelowFilter,
PushReferencedFieldsThroughFilter, PushFilterThroughFetch) — none through an
Intersection. Java filters intersections via the match-then-implement
data-access mechanism (AbstractDataAccessRule), NOT logical commutation. And
neither Go rule claims Java parity ("Ports Java's X"), unlike the ported rules
around them.

This is the query-engine skill's "Go-only rules are suspect" verbatim — the
same shape as the retired Go-only IndexIntersectionRule. The
Push/PullCommonFilter-Intersection PAIR is a Go invention, and its interning
interaction produces a cyclic memo Java's rule set cannot.

THE FIX (gated — query-engine change, Graefe + Torvalds), in Java-alignment
order of preference:
  1. REMOVE the Go-only rule pair (Push + PullCommonFilter through/above
     Intersection); verify no corpus query loses a needed plan (they
     shouldn't — Java plans these via data-access) and zero explain-diff drift
     except intended.
  2. If some optimization genuinely depends on them, keep them but guard the
     cycle-forming yield in PullCommonFilter (reject when MemoizeExpression
     returns a reference reachable from the current one).
  A bare visited-guard in GetCorrelatedTo is WRONG either way — it leaves the
  cyclic memo in place and masks the symptom (principle #9).

The crash seed is NOT committed (it would red CI for a bug whose correct fix is
gated); it is recorded above as an inline 4-byte reproducer instead.

### [ ] The fix is REMOVAL, not a guard — proven; and it is a Go-only FAMILY

Continued the DFS on the cyclic-reference crash by trying both candidate fixes
on the 4-byte seed. Decisive:

GUARD IS WRONG. Extending MemoizeExpression's existing direct-self-loop guard
(`ref.Canonical() == c.Reference.Canonical()` → return fresh InitialOf) to
TRANSITIVE reachability stops the stack overflow but converts it into
NON-CONVERGENCE: "exploration did not converge — possible non-terminating rule
interaction". Interning is what makes the inverse-rule fixpoint terminate;
declining it to break the cycle makes Push/PullCommonFilter ping-pong forever,
each pass adding a fresh member. Crash and non-termination are two faces of the
same problem — you cannot keep these rules AND have both a finite, acyclic
memo.

REMOVAL IS RIGHT. With the Push/PullCommonFilter-Intersection pair removed from
the rule registry, the same seed CONVERGES cleanly and instantly. Java reaches
the same plans via match-then-implement data access; the rules add nothing Java
lacks.

IT IS A FAMILY, not a pair. The same Go-only inverse shape exists for UNION:
`PushFilterThroughUnionRule` + `PullCommonFilterAboveUnionRule`. Java has no
filter-through/above-Union rule either. The Union pair is almost certainly the
same latent cyclic-memo / non-termination hazard and should be removed in the
same change (verify with a fuzz seed that reaches it).

THE FIX (RFC + Graefe ACK — this is an architectural decision, removing rule
families, not a local patch):
  - Remove PushFilterThroughIntersection + PullCommonFilterAboveIntersection
    and PushFilterThroughUnion + PullCommonFilterAboveUnion (4 rules, 4 tests,
    4 registry lines).
  - VERIFY zero explain-diff drift across the 2407-query corpus (Java plans
    these via data-access, so drift is expected to be zero; any drift is a
    query that leaned on the Go-only rule and must be re-examined).
  - Re-run the planner fuzz targets to confirm the crash class is gone.
  A guard in MemoizeExpression or GetCorrelatedTo is NOT the fix (guard →
  non-termination, proven above; GetCorrelatedTo guard → masks the cyclic memo).

All experiments were bounded and reverted; no code change is on this branch —
only this diagnosis.

### [ ] FUZZ sweep result: two distinct PRE-EXISTING planner crash classes

Swept the cascades planner/memo/extraction fuzz targets after the RFC-183
merge. Two distinct crash classes, BOTH pre-existing (neither is an RFC-183
regression), plus a clean set:

CLASS 1 — cyclic-memo stack overflow (GetCorrelatedTo, reference.go:752).
Confirmed on FuzzPlanner_MemoConsistency, _Determinism, _Idempotence,
_InitialMemberPreserved — all the full-exploration targets that run
DefaultExpressionRules on a seed reaching the Go-only Push/PullCommonFilter-
Intersection pair. Single root cause (the Go-only rule family, above); fixing
it clears this whole class.

CLASS 2 — "Plan succeeded but root Reference has no BestMember stamp"
(FuzzPlanner_PlanFullPipeline, planner_fuzz_test.go:115). DISTINCT from class 1
(instant assertion, not a stack overflow). Reproducing 34-byte seed
`311028b5ee5f305a`. PRE-EXISTING — proven: the identical seed fails on
pre-RFC-183 master (15dc17a82).
  SETTLED — it is a REAL planner bug, not an over-strict test. A deterministic
  search (not fuzzing) reproduces it at `b=[35 4 4 1]` = op 5 → UNION of two
  TypeFilter(Scan) children, i.e. `Union(TypeFilter(Scan), TypeFilter(Scan))`.
  The value `p.Plan` returns for it is a `*expressions.LogicalUnionExpression`
  — a LOGICAL, UNIMPLEMENTED expression — with nil error and no root winner.

  So `ExtractBestPlanFromSelector` fell back to a LOGICAL member because the
  root union has no PHYSICAL final (ImplementUnorderedUnion never produced one
  for this shape), and returned it instead of erroring. `Plan` therefore hands
  a caller a logical expression as if it were a physical plan — a downstream
  consumer expecting `plans.RecordQueryPlan` gets a logical node. NOT a
  canonicalization gap (HasWinner canonicalizes, reference.go:313,329); the
  root's winner is genuinely nil because nothing physical was ever chosen.

  REFUTED sub-question (b). The first note said "extraction must ERROR when the
  root has no physical plan." That was TESTED and is WRONG: adding an
  isPhysicalPlan check + error in extractBestPlanFromSelectorVisited (after the
  GetBest fallback) causes ZERO corpus drift but breaks four cascades unit
  tests — TestPlanner_GenerateDataAccess_NoMatchesIsNoOp,
  _PlanningPhase_AlwaysRuns, _GenerateDataAccess_BottomUp, _Plan_FullPipeline —
  which deliberately plan with LIMITED rule sets and RELY on Plan tolerantly
  returning a non-physical result (a bare FullUnorderedScan / LogicalFilter /
  LogicalDistinct that the isolation test never gave an implementation rule
  for). So Plan's tolerance of a non-physical root is INTENTIONAL, not the bug.
  Erroring universally would break rule-isolation testing.

  So the real question is only (a): WHY does the seed's union fail to implement?
  NARROWED to a precise TRIGGER and three REFUTED hypotheses:

  TRIGGER = ASYMMETRIC LEGS. buildFuzzExpression([35,4,4,1]) is NOT the
  symmetric union I first assumed; it is
  `Union(TypeFilter(TypeFilter(Scan)), TypeFilter(Filter(Scan)))` — one leg
  nests a TypeFilter, the other a Filter. Direct-construction isolation:
    - Union(Scan, Scan)                        -> implements
    - Union(TF(Scan), TF(Scan))                -> implements
    - Union(TF(TF(Scan)), TF(TF(Scan)))        -> implements (symmetric nested)
    - Union(TF(Filter(Scan)), TF(Filter(Scan)))-> implements (symmetric filter)
    - Union(TF(TF(Scan)), TF(Filter(Scan)))    -> does NOT implement  <-- seed
  Each leg implements STANDALONE; only the asymmetric UNION fails.

  REFUTED:
    (b) extraction-error — breaks four isolation tests (above).
    rule-selection — the seed fails under FULL DefaultExpressionRules too, not
      just the sparse selectRules(b) subset.
    planExprs[0]-ordering — ImplementUnorderedUnion collects childPlans from
      planExprs[0] only; changing it to scan the whole partition for a physical
      expr does NOT fix the seed, so it is not "physical present but not first."
      One asymmetric leg has NO physical expression in the partition at all.

  RESOLVED (commit "cascades: reset per-stage exploration on the no-finals
  stage boundary"). Root cause found via a full memo dump on the asymmetric
  shape: constant-true filter elimination makes `Filter([TriTrue],Scan) ≡ Scan`,
  so the right leg's group MERGES with the left leg's inner TypeFilter group.
  That merged group reaches the REWRITING→PLANNING boundary with ZERO final
  members (Go tolerates unfinalized groups via ExploreGroupTask's
  `len(FinalMembers())>0` guard; Java finalizes universally so never hits this).
  With no finals it cannot take AdvancePlannerStage (which would empty it), so
  it fell to the plain stage-set path that changed the stage but did NOT reset
  the per-stage exploration bookkeeping. The group kept its REWRITING
  `explorationDone` state → NeedsExploration=false → ExploreGroupTask returned
  early → ImplementTypeFilter never fired → no physical member → the union saw
  an empty leg partition and bailed → root had no winner while Plan() reported
  success. Fix: `Reference.AdvanceStagePreservingMembers` resets exploration
  state (explState, rounds, winner, planProperties, constraints epoch) while
  keeping members, so surviving logical members re-explore and implement in
  PLANNING. The dead no-reset SetStage method was removed. Pinned by
  asymmetric_union_planning_test.go + the `[]byte{35,4,4,1}` seed in
  FuzzPlanner_PlanFullPipeline; five planner fuzz targets clean at 45s each;
  full suite green. Third refuted hypothesis (planExprs[0]-ordering) was right
  that "one leg has NO physical expression" — the reason was the exploration
  reset, not the partition machinery.

CLEAN (no crash in the sweep): FuzzPlanner_E2E_NoPanic,
FuzzExtractBestPlan_SingletonInvariant, FuzzMemo_MemoizeInvariant,
FuzzPlanner_ProjectionPipeline_NoPanic — extraction, memo-invariant, and
projection paths are robust to these inputs.

Crash seeds NOT committed (they would red CI for gated bugs); recorded as
hashes/reproducers. All experiments reverted; tree clean.

## Phase 11: Optimizer invariants

- [ ] **CQ-38 (MED, semantics) — NaN comparison follows Java's total order, not
  IEEE.** MEASURED: `WHERE (v/z) = (v/z)` returns ALL rows (IEEE says none),
  `<> ` returns none (IEEE says all), `> 0` returns all (IEEE: every NaN
  comparison is false). Deliberate -- predicate comparison falls through to
  `values.CompareFloat64`, the Double.compare total order with NaN greatest, and
  the comment says "matching Java Double.equals". So for NaN, Go matches Java
  and BOTH diverge from the SQL standard, Postgres and CRDB -- the opposite
  posture from signed zero, where Go keeps IEEE and diverges from Java.
  NOT symmetric with the signed-zero case and not a simple "apply the same
  ruling": strict IEEE makes the comparator NON-TRANSITIVE and destroys the
  total order that ORDER BY, tuple key order, merge joins and dedup all share.
  Splitting PREDICATE comparison (IEEE) from ORDER BY (total order) is coherent
  -- it is exactly the split RFC-082 already documents for signed zero -- but it
  touches every comparison site and needs its own design gate.
  DISTINCT/GROUP BY treating two NaNs as one value is settled and would NOT
  change: value identity is tuple-key identity and every NaN packs identically.
  Current behaviour is pinned by `nan_comparison_semantics_test.go` so it cannot
  drift unobserved while the question is open.
  **Also corrected DIVERGENCES.md, which claimed Go returned FALSE for NaN
  self-equality "(IEEE, SQL standard)".** That was asserted, never measured, and
  backwards -- written by me earlier the same day while documenting Java's `=`
  bug. A false claim in the divergence register is worse than no claim.

(Header previously read "RFC-193/194 follow-through". Neither document was ever
committed — no such file exists in the repo or its history. The findings they
were meant to hold are recorded in the items below, which is why they survived
the drafts being lost. Do not reuse numbers 193/194 for unrelated documents: a
stale reference that resolves to the WRONG doc is worse than one that resolves
to nothing.)

- [ ] **CQ-29 (HIGH) — five cost estimates violate a proven cardinality bound.**
  Found by cross-checking the cost model against `CardinalitiesProperty` as an
  oracle; all five are pinned as self-cleaning exclusions in
  `pkg/recordlayer/query/plan/cascades/cardinality_cost_bound_test.go`, so fixing
  one fails the build until its exclusion is removed. Each is an operator whose
  guaranteed floor or ceiling the cost formula never special-cases:
  (1) `RecordQueryStreamingAggregationPlan` with NO grouping keys — the property
  proves max=1 (an ungrouped aggregate emits exactly one row) while `HintCost`
  computes `in * DistinctSelectivity` ≈ **700,000** for a 1e6-row child. Worst of
  the five: any parent operator above an ungrouped aggregate sees a full table
  where there is one row, which flips join ordering.
  (2) `RecordQueryDistinctPlan` and (3) `RecordQueryUnorderedPrimaryKeyDistinctPlan`
  — flat 0.7 multiplier, no floor, drops below a proven min of 1.
  (4) `RecordQueryDefaultOnEmptyPlan` — the property applies `child.Floor(1)`;
  `HintCost` passes the child through unchanged. Internally inconsistent:
  `FirstOrDefaultCost` DOES floor at 1 for identical reasoning.
  (5) `RecordQueryRecursiveLevelUnionPlan` — property proves `min = seed.Min`
  (UNION ALL always emits the seed); `recursiveCost` computes `seedCard*recCard`
  with no additive seed term, collapsing toward zero when the recursive leg
  estimates below 1. The formula's shape is questionable away from the boundary
  too (`seed=1000, rec=1` costs 1000 where the true total is ≥ 2000).

- [ ] **CQ-30 (HIGH) — the cost model keeps a private cardinality derivation.**
  Java has exactly one `CardinalitiesProperty`, consulted by both the cached
  properties map and `PlanningCostModel.compare`. Go has two independently-coded
  switches: `plan_properties.go`'s `computeCardinalities`/`cardinalitiesForRef`
  (weakens across ALL final members via `WeakenCardinalities`) and
  `planning_cost_model.go`'s criterion-2 walk
  (`findExpressionsByType`/`scanProvableMaxCard`/`indexProvableMaxCard`,
  descending via `bestPhysicalChild` — a SINGLE cost-tie-broken member). **The
  comparator that actually elects plans uses the narrower private one.** This
  matters because `OptimizeGroupTask` deliberately retains multiple final members
  per group (one winner per distinct requested ordering), so "the group's
  cardinality" is genuinely ambiguous and two disagreeing answers live side by
  side. Fix: route criterion 2 through `GetRefPlanPropertiesMap`/
  `computeCardinalities` and delete the duplicate. Specified in
  `rfcs/195-cost-must-not-contradict-proof.md`, which covers this together with
  CQ-29 — they are the same defect (cardinality with two homes) seen from the
  two sides, and fixing one without the other leaves the disagreement intact.

- [ ] **CQ-31 (HIGH) — retire `.Field` as an input to any DECISION.**
  Seven separate hand-rolled proofs of a semantic property by leaf-name
  comparison went wrong on this branch (`PushValueThroughFetch` per RFC-179 F12,
  `correlatedInnerField`, `correlatedFieldOf`, `fieldValueAliasAndCol`,
  `buriedLegOrdinalLayout`, `rebaseOuterLegValue`, and the unique-key proof), each
  found by a different route. Measured: **107 non-test `.Field` reads outside the
  values package, 98 of them DECISION-class**, and roughly half live in the SQL
  translator/generator layer (`cascades_translator.go` 30, `cascades_generator.go`
  11, `logical_predicate.go` 8). `FieldValue.Resolved` (the construction-time
  resolved accessor, Java's `ResolvedAccessor`) and `SemanticEqualsUnderAliasMap`
  already exist and are the correct inputs. CockroachDB assigns a column id during
  name resolution and the optimizer never sees a name again;
  `ColumnMeta.Alias` is documented as display-only. **Enforcement is the point** —
  a `pkg/docscheck`-style build check that `.Field` cannot feed a comparison
  outside an allowlisted display site, or an eighth instance is certain.
  (This item previously cited "RFC-193 §5.1". **That document was never
  committed** — no such file exists in the repo or its history, so the citation
  pointed at nothing. The measurements above are the actual specification; they
  were recorded here rather than in the uncommitted draft, which is why they
  survived. RFC number 193 remains unused and is NOT reused, so any stale
  reference elsewhere stays merely broken instead of silently resolving to an
  unrelated document.)

- [ ] **CQ-32 (MED) — two `MemoEqual` callers still bypass the alias gate.**
  `Memo.memoizeNonLeaf` and `Memo.refContains` (`memo.go:398-433`, `511-523`) call
  `expressions.MemoEqual` unconditionally, with no `InternsAliasAware` check — the
  same hazard fixed in `findEquivalentRef` and long-since fixed in
  `Reference.Insert`. NOT fixed because gating them breaks two deliberately-written
  tests (`TestMemoActivation_InternsAliasVariants`,
  `TestMemoActivation_BroadInterningCollapsesK` in `memo_activation_test.go`) that
  require alias-variant `Filter`s to intern into one shared Reference. RFC-077
  flagged this and never audited it. Genuine open question: either those tests
  encode a real requirement that the gate must accommodate, or they predate the
  alias-identity hazard and should change. Needs the audit, not a drive-by.

- [ ] **CQ-33 (MED) — `LIKE 'prefix%'` on an indexed column does a FULL SCAN.**
  `ResolveStartsWith` (`pkg/relational/core/query/expr/expr.go:876`) has **zero
  production callers**; `walk.go`'s LIKE arm always calls `ResolveLikeWithEscape`,
  and no rule rewrites a constant-prefix LIKE into `ComparisonStartsWith`.
  Measured by direct EXPLAIN: `s LIKE 'prefix%'` never produces an `IndexScan` at
  any table size tried (6–150 rows). Two consequences: a common query shape scans
  the whole table, and RFC-179's F11 fix (the `STARTS_WITH` arm in
  `scanComparisonsToTupleRange`) is unreachable from SQL and therefore provable
  only by unit test. This matches Java's non-exposure (`STARTS_WITH` is a
  QueryComponent-only API there), so it is a permitted read-side extension rather
  than a parity gap — and CockroachDB has exactly this optimisation
  (`idxconstraint/index_constraints.go`, the `LikeOp` case in
  `makeSpansForSingleColumnDatum`). Guard the rewrite on a constant prefix with no
  further wildcards and no ESCAPE ambiguity.

- [ ] **CQ-34 (MED) — the sargable gate and the range builder are kept in manual
  lockstep.** `isSargableComparisonForMatch` (`match_max_match_map.go:67`) decides
  what may be consumed into a scan range; `scanComparisonsToTupleRange`
  (`executor.go:693`) decides what actually becomes a bound. They are two
  functions in two files with no compiler-enforced tie, which is exactly the F11
  shape (a comparison accepted as sargable whose bound is then never applied —
  wrong rows, with the residual filter already removed). The inequality switch now
  has a loud default that errors on an unrecognised comparison, but **the
  equality-prefix loop (`executor.go:699-741`) has no equivalent** — it silently
  assumes anything not `IS NULL` is a plain `=`. CockroachDB makes this class
  structurally impossible: `idxconstraint` returns a `tight bool` from the same
  call that builds the span, and `RemainingFilters()` may only drop a predicate
  when that call said `tight` (`index_constraints.go:1288-1308`), with the
  fallthrough default being `unconstrained, tight=false`. Port the shape of that,
  or at minimum add the loud default to the equality-prefix loop.

- [ ] **CQ-35 (LOW) — `ComparisonType.IsEquality()` is authoritative-looking dead
  code.** `comparisons.go:106`, doc-commented as "useful for index-pushdown
  decisions", has **zero production callers** — only its own unit test. The real
  equality-range decision is a separate hardcoded check in `ComparisonRange.Merge`
  (`comparison_range.go:146`), and the two already disagree: `IsEquality()` also
  classifies `IN`, `IS NOT DISTINCT FROM`, `TEXT_CONTAINS_*` and
  `DISTANCE_RANK_EQUALS` as equality; `Merge` does not. Harmless today because
  nothing consults it, and precisely the "looks authoritative, isn't" trap that
  produces a real bug the day someone wires it up. Either delete it or make it the
  single source `Merge` consults.

- [ ] **CQ-36 (LOW) — `df24afebb` has no mutation verdict.** The branch-wide
  mutation audit reverted each correctness fix and confirmed the suite went red.
  `df24afebb` (threading `context` cancellation through the whole task-execution
  and plan-extraction call graph) could not be mechanically isolated — five later
  commits edit the same plumbing, `cascades_generator.go` alone conflicts in 7
  hunks, and `unified_tasks.go`'s conflict shows interleaved content from a
  different commit. It is **unknown**, not passed. Re-attempt once the surrounding
  churn settles, or write a directed test for cancellation mid-plan-extraction.

- [ ] **CQ-37 (MED) — the generative harnesses cannot emit two known bug shapes.**
  `rowdiff`'s `genExists` appends exactly ONE correlated `[NOT] EXISTS` to a
  single-table query, correlated on a bare BIGINT column against a fixed inner
  alias, and never nests an EXISTS inside an EXISTS, never gives the inner an
  explicit `JOIN…ON`, and never puts the outer query behind an aliased
  self-subquery that could collide names with the inner. Every bug in that class
  (TODO's alias-shadowing self-subquery entries; "CORRELATED EXISTS with an
  explicit JOIN..ON inner DROPS the inner ON", which took 7 codex rounds) was found
  by review, never by a harness — because the harness cannot produce the shape.
  Second shape, unresolved: whether an outer-join `WHERE`-vs-`ON` case was ever
  actually exercised by a seed. Add a directed template rather than relying on
  random weighting, so the question stops being ambiguous.
