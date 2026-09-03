# TODO.md — completed and retired entries (archived 2026-08-31)

**This is history, not a work list. Nothing in this file is queued.**

`TODO.md` had grown to 20,861 lines, of which 12,367 described work that was already done. Under
this project's execution rule — "pick the lowest-numbered unchecked item" — a finished entry costs
nothing to leave in place, and that is exactly why they accumulated until the open backlog was
under 40% of the file. This is the split: `TODO.md` now holds open work only, grouped by area, and
every completed entry moved here verbatim.

**Read this file when you need the reasoning behind a finished change** — why a design was rejected,
what a measurement was taken over, which hypothesis was refuted and by what. That reasoning is the
reason these entries are archived rather than deleted; several of them record a premise that was
believed, measured, and found false, which is the part a git log message never carries.

Entries appear in their original file order. A `##` heading is re-emitted whenever the enclosing
section changes, so an entry always shows which section it came from.

## Live work that was lifted OUT of completed entries

Twenty-four unchecked items were found nested inside entries marked `- [x]`. That is the failure
CLAUDE.md names explicitly: a live defect written into the prose of a completed item is unreachable,
because nothing will ever pick it up. All of them were lifted into the appropriate section of
`TODO.md` rather than archived with their parents — the RFC-173 slice residuals, the RFC-070 /
RFC-083 / RFC-087 / RFC-088 follow-ups, the RFC-071 intersection-resume optimization, and the
DISTINCT cross-page dedup cost work.

## Deliberately retired, NOT completed

Four unchecked boxes remain below. Each was retired because a later, kept entry supersedes it, and
each is left in place so the reasoning survives:

- **`identifiers: Go OVER-resolves a quoted DDL column`** — the entry says so itself: "this entry is
  kept for the measurement history, not as a description of today's code". The live framing is in
  `DIVERGENCES.md` ("Identifier resolution: Go over-resolves case, Java compares exactly") and
  RFC-237 §3.3, and the divergence is carried in `TODO.md` under "What RFC-237 did NOT close".
- **`CQ-88 — port the Java-sanctioned per-type statistics source`** — implemented. RFC-236 shipped
  offline collected statistics; `TODO.md` keeps that entry, including its priced, still-open
  residue (histograms / NDV / MCV, automatic collection, plan-cache invalidation on drift).
- **`B1 — THREE approaches tried`** and **`B1 first attempt (bespoke helper)`** — both are marked
  `(superseded)` in their own text and record refuted approaches, not pending work.

---

# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.12.11.0**. FDB wire protocol: **7.3.77**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types, 9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection with per-ordering properties.

---

## DONE — RFC-199 deterministic simulation testing (DST) revival

Branch **`feat/dst-revival`**, PR #462 — **MERGED as `8cc1f72ff`.** The header used to read
"IN PROGRESS" and the body "Not merged: the review lap is the gate"; the review lap happened and
the merge landed. `pkg/dst` and `pkg/simfdb` are in the tree.

- [x] **RFC-199 DST — Tier 0/1/2 MERGED (`8cc1f72ff`, #462).** `pkg/dst`
  (seeded Clock/Randomness/Buggify behind a nil-means-production `Env` seam),
  `pkg/simfdb` (deterministic in-memory MVCC `fdb.BackendDatabase` with SSI, RYW, 12
  atomic ops, true rollback and seeded fault injection), and the Tier-2 hunt harnesses
  (`pkg/simfdb/hunt`, `cmd/dst-hunt`, `cmd/dst-generate`). The RFC was drafted and
  ACK'd as 179; master reassigned that number, so the document is now
  `rfcs/199-deterministic-simulation-testing.md`.

  **Why this matters beyond DST:** SimFDB is the acceptance harness for the RFC-198
  explicit-transaction work (see the read-your-writes item below, which now carries it
  as a gate). RFC-198 has to prove a set of conflict verdicts is correct, and conflict
  verdicts are exactly what a real cluster makes unreproducible.

  Four real SimFDB conflict defects were fixed during the revival — all of them
  under- or over-conflict against real FDB, i.e. the class that would make the harness
  certify a wrong answer: a committed handle keeping its read version, `GetRange`
  computing conflict extents from raw selector anchors instead of resolved keys,
  selector resolution not taking the `GetKey` conflict range it costs on the real
  backend, and `more` reported false when a range read exactly met its row limit. The
  last two were found by extending the live differential's selector-range axis, which
  had swept 400 scenarios across a range axis and a selector axis without ever
  crossing them.

- [x] **F4 — re-landed. The revert's reproducer does not reproduce at the commit it
  reverts.** `de1da5f17` ported four client-fidelity gaps; `302ea8a67` reverted all four
  because ONE of them — M5, `accessed_unreadable`(1036) on a read that reaches a pending
  versionstamped write — was reported to turn the Tier-2 hunt red:

  ```
  hunt: seed=42 FAILED after 9 ops (3 faults)
    error: op 8 saveNew: load old record version for index update:
           failed to load record version: accessed_unreadable (1036)
    reproduce: hunt.Run(42, cfg)          (also seed 0, at op 24)
  ```

  Three explanations, none settled: **(a)** the record layer would get 1036 against a real
  cluster too, and no existing test reaches it because only the hunt runs many
  version-writing ops inside one transaction; **(b)** it sets `BYPASS_UNREADABLE` somewhere
  the sim does not model — grep finds no such call; **(c)** the ordering that makes it safe
  on real FDB is one the sim does not reproduce.

  **Timing evidence, and it points at (c).** MEASURED: `flushVersionMutations`
  (`database.go:850-863`) is what turns a queued `AddVersionMutation` into an actual
  `tx.SetVersionstampedKey/Value`, and it defers every one of them to just before commit.
  There are SIX call sites, not seven — the seventh `grep` hit is the definition itself —
  and ALL SIX are commit-adjacent: `database.go:253`, `:330`, `:374`, `:719`
  (`CommitWithHooks`), `:1067` (`CommitWithVersionstamp`) and `runner.go:174` each sit
  immediately after `runCommitChecks()` and immediately before the commit. The
  record-version key the hunt died on goes through that queue (`store_version.go:52`,
  `:78`), so nothing the record layer reads mid-transaction can reach a key the REAL client
  would have marked unreadable. That is the shape of a sim application-point divergence,
  not of a live record-layer bug.

  Two sites do bypass the queue and write a versionstamped value mid-transaction:
  `database.go:1025` (`SetMetaDataVersionStamp`) and `spfresh_storage.go:520`. The first
  already handles the error — `GetMetaDataVersionStamp` documents "On ACCESSED_UNREADABLE
  errors (FDB code 1036), marks the stamp as dirty and returns nil" — which is further
  evidence that the record layer's real 1036 exposure is known and handled, and that the
  hunt's failure is somewhere else.

  INFERRED, and the thing to instrument first: `postCommitReset` is what clears the sim's
  unreadable set, and it runs on only two paths (`conflict.go:50` read-only fast path,
  `:173` success). Every error return deliberately leaves the handle untouched. The failing
  hunt run took THREE faults. So a handle retried after an injected commit fault would carry
  an unreadable set forward that no real client has. Whoever runs the container arm should
  instrument exactly that: when the sim's unreadable set becomes non-empty relative to the
  record layer's read, and whether it survives a failed commit and a retry.

  **RESOLVED — the premise was wrong, and the ruling is (a), not (c).** MEASURED: at
  `de1da5f17` itself, `HUNT_SEED=42 go test ./pkg/simfdb/hunt/ -run '^TestHuntSeed$'` runs
  all 300 ops and PASSES (`seed=42 OK (300 ops, 107 faults, fp=88d14b36…)`), as does seed 0
  (`300 ops, 105 faults, fp=6ceec5a2…`). The fingerprints are byte-identical to the re-landed
  branch. The revert was taken on a reproducer that never held at the commit it reverted;
  1088 seeds × 300 ops under the full fault profile produce zero 1036.

  Instrumentation of the unreadable-set lifecycle (536 MARK, 0 GET-HIT, 0 SCANCAP, 80
  Reset-on-non-empty per run) refutes the hypothesised carry-across-a-faulted-retry: every
  mark comes from `flushVersionMutations`, which is commit-adjacent at all SIX call sites
  (`database.go:309,413,462,879,1237`, `runner.go:334`; the seventh grep hit is the
  definition at `:1011`), so no read can follow a mark within an attempt. Across attempts
  `SimDB.Transact` reuses the handle but routes every retry through `OnError`→`Reset()`,
  which nils the set — matching the real client (`client/ryw.go:183-184` in `reset()`, reached
  from `Transaction.OnError`) and C++, where the unreadable bits live in the `WriteMap`
  (`WriteMap.h:75-76`) that `resetRyow()` reconstructs wholesale
  (`ReadYourWrites.actor.cpp:2704-2706`) on the unique retry path (`:1521`, `:2754`). After a
  failed commit at modern API versions C++ leaves the map intact (`:1413-1423`) but
  `commitStarted` stays true, so every access yields `used_during_commit`(2017), never 1036
  (`:2781-2787`) — "unreadable survives a failed commit and is observable on the retry" is
  unreachable in a real client.

  (b) is dead: the record layer never sets `BYPASS_UNREADABLE` (no non-test hit in
  `pkg/recordlayer/`, `pkg/relational/`). (a) is where the residual risk lives — a read
  landing on a versionstamped key would get 1036 against a real cluster
  (`RYWIterator.cpp:45-46`, `:75-76`) and nothing here reaches that shape — but it is
  unreached for a structural reason, not by seed luck.

  Three defects in `de1da5f17` itself were found and fixed while re-landing: its
  `SetBypassUnreadable` never assigned the field it documented (the whole bypass path was
  dead, and the commit is red on its own test); its present-empty fix landed on `store.rangeAt`
  while master has since added `store.rangeAtLimited`, which the lazy range path actually
  calls — a semantic conflict git merged cleanly; and grafting the cap onto master's lazy
  range pipeline exposed that persisting the capped bounds back into the scan bounds swallows
  the 1036 at a batch boundary. M6's size-before-too-old ORDERING was already on master; only
  its overhead constants (`sizeofMutationRef`/`sizeofKeyRangeRef`) were missing. M8a is
  unchanged.

## DST findings

The query-engine defects the RFC-199 Tier-2 hunts surfaced, and what closed each. This is
the writeup `pkg/simfdb/hunt/metamorphic/testdata/findings/README.md`,
`pkg/simfdb/hunt/metamorphic/corpus.go` and RFC-199 §7a cite. Every entry here is a real
wrong-answer defect found by a SimFDB-backed oracle, not by a hand-written test — which is
the point: none of them had an owner or a reproducer before the hunt ran.

The oracles are two. The **SQL-pagination oracle** (`pkg/simfdb/hunt/sqlpage`) runs a query
unpaged, then re-runs it at a scanned-rows limit of 1 so every row forces a continuation
round-trip, and demands the two agree. The **metamorphic oracle**
(`pkg/simfdb/hunt/metamorphic`) runs sets of queries that must be equivalent under SQL
three-valued logic and demands they return the same relation; scenarios carry `ordered: true`
when the equivalence is over an ordered sequence rather than a multiset.

- [x] **Streaming DISTINCT dropped its dedup state across a resume.** The hash DISTINCT
  operator kept its seen-set in memory per page, so a resume across a scanned-rows boundary
  re-admitted values it had already emitted: `SELECT DISTINCT` returned duplicate rows when
  paginated, even with `ORDER BY`. GROUP BY was unaffected (different dedup machinery), which
  is why no existing test saw it. Fixed by carrying the seen-set through the continuation
  (`executeHashDistinct` over `gen.DistinctHashContinuation`). Pinned by
  `TestDistinctContinuationDedups` (`pkg/simfdb/hunt/sqlpage/distinct_continuation_test.go`),
  which checks GROUP BY over the same data alongside it so a regression names which operator
  broke; bare DISTINCT is back in the oracle's query sweep.

- [x] **Multi-value `IN` and `UNION ALL` errored 54F01 instead of resuming.** A multi-value
  `IN (a, b)` plans as an InJoin over a concat of per-value index scans, and `UNION ALL` plans
  as a `RecordQueryUnionPlan` over that same concat combinator. The concat had no continuation
  of its own, so a branch stopped mid-scan by the execution scanned-rows limit could not mint a
  resumable token and the statement failed. Java's `InJoinCursor` resumes. Fixed by having the
  concat serialize {active-branch index, that branch's child continuation}. Pinned as
  correctness (paged result equals unpaged as a multiset) by `TestInJoinContinuation` and
  `TestUnionAllContinuation`, plus `TestInJoinLimit` for the composition with `LIMIT` — a
  per-branch limit would let each branch emit up to LIMIT rows, so the limit is cleared on each
  inner branch and applied once at the concat, matching `executeInUnion` and Java's
  `RecordQueryInJoinPlan`.

- [x] **Aggregate-index MIN/MAX dropped an all-NULL group.** SQL `MIN`/`MAX` mapped to a
  `MIN_EVER_LONG`/`MAX_EVER_LONG` index, which skips NULL values — so a group whose every row
  has a NULL measure had no index entry at all and vanished from the result, while the
  scan-backed plan for the identical query returned it with a NULL extremum. An index must not
  change which groups exist. Fixed by mapping to `PERMUTED_MIN`/`PERMUTED_MAX`, matching Java's
  `NumericAggregationValue.Min`/`.Max`. Pinned by
  `pkg/simfdb/hunt/metamorphic/aggindex_sum_nullgroup_test.go` and the
  `aggindex-minmax-allnull-group-dropped` scenario, whose two queries differ only by a no-op
  `WHERE id IS NOT NULL` over a NOT NULL primary key — identical rows, so identical groups.

- [x] **`GROUP BY … ORDER BY` placed NULLs last while `DISTINCT … ORDER BY` placed them
  first.** The same relation under the same `ORDER BY a` came back in two different orders
  depending on which operator produced the distinct values. NULL placement is a property of the
  ORDER BY clause (ASC ⇒ NULLS FIRST, the Java default, and what FDB tuple encoding yields on
  the plain-scan path), not of the chosen plan, so this was an internal inconsistency in the
  GROUP BY aggregation+sort path. Fixed; the seed corpus now asserts the equivalence directly
  in both directions (`groupby-orderby-null-first-matches-distinct`,
  `groupby-orderby-desc-null-last-matches-distinct`, and the multi-key variant, all with
  `Ordered: true` — a multiset comparison is structurally blind to this defect).

**Status of the reproducer corpus (measured 2026-07-30):** all three scenarios under
`pkg/simfdb/hunt/metamorphic/testdata/findings/` now judge clean — `3 scenarios, 10 groups |
0 INEQUIVALENCE (real) | 0 errored | 0 setup-err` — and `go test
./pkg/simfdb/hunt/metamorphic/... ./pkg/simfdb/hunt/sqlpage/...` passes. They are kept as
durable reproducers rather than deleted: each one is the exact shape that broke, and the
`ordered: true` flags are what make them able to express the defect at all.

## CI capacity — declare heavy targets' cost instead of remembering it per call site

## CI flake — `bazelscaleset` remote tests race their own fake `ssh` binary

## Wire divergence — first-or-default continuation bytes (blocked on Java issue 3220)

## FDB client — database transaction defaults are a struct, not C++'s ordered option list

## FDB client — conflicting-keys readback (debugging surface + skew instrument)

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
  `WithInValues` — sorts a copy with `sort.SliceStable` (Java's `List.sort`
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

- [x] **CQ-27 (MED, found extending the sargability differential oracle to
  indexed DOUBLE/FLOAT columns, 2026-07-26; FIXED 2026-07-27) — an indexed
  DOUBLE/FLOAT column's equality/IN/range SARG DISAGREES with the full-scan
  residual-filter path on a value stored as -0.0 (negative zero).**
  The scan-range builder of the day (`scanComparisonsToTupleRange`, executor.go
  — since retired as a dead twin, RFC-217; the live builder is
  `bindScanComparisonsToRangeSet`, `scan_range_binding.go:256`) packed a
  comparand into the raw
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
  **Fix (live code: `bindScanComparisonsToRangeSetWithTerminalWidening`,
  `scan_range_binding.go:273`, reached through `bindScanComparisonsToRangeSet`
  at `:256`; the fix originally landed in the since-retired
  `scanComparisonsToTupleRange`, RFC-217):** the binder widens a
  zero-valued FLOAT/DOUBLE
  bound to cover BOTH adjacent keys wherever it terminates the range: an
  equality (or IN-list per-element sub-probe) that is the LAST comparison
  widens to an inclusive `[-0.0, +0.0]` subtree; `>=`/`<` canonicalize their
  bound to whichever of the two adjacent keys makes the IEEE-correct
  endpoint (`>=` pins to -0.0, `<` pins to -0.0 as the exclusive stop); `>`
  and `<=` are canonicalized too (pin to +0.0) for symmetry, closing a
  latent `col > -0.0`-literal gap the bug report didn't call out.
  (HISTORICAL, as of CQ-27's fix: a zero-valued equality that is NOT the
  terminal comparison in a composite index's equality prefix — more columns
  follow it — was at that time deliberately left UNWIDENED, which is what
  CQ-28 below was filed against. That is NO LONGER current behaviour: the
  live binder emits BOTH signed zeros as range-set alternatives for the
  non-terminal case too, `scan_range_binding.go:390-406`. See the CQ-28
  entry's own HISTORICAL-STATE note.) `expr.promoteColumnColumnNumeric`
  (`pkg/relational/core/query/expr/expr.go`) — a SEPARATE bug in the same
  introducing commit — no longer wraps an INT-vs-LONG column-vs-column
  comparison in `PromoteValue` (`sharesIntegerWireEncoding`): the two codes
  pack to byte-identical tuple encodings, and the wrapper was defeating the
  SARG matcher's `AccessorNamePath` (only unwraps `*FieldValue` chains),
  degrading a BIGINT=INTEGER point lookup to a residual full scan.
  **RE-INVESTIGATED AND REFUTED — do not re-open as "item #28".** A later
  board carried this INT/LONG sub-finding forward as a live item, "Stop
  INTEGER-to-LONG promotion killing index probes". Two things are wrong with
  that line. (1) The number: this text is CQ-**27**; CQ-28 is the zero-valued
  FLOAT/DOUBLE item further down and has nothing to do with INT/LONG. (2) The
  tense: the defect was already fixed by the `sharesIntegerWireEncoding` skip
  above, landed in `41ecb95d1` (#517) and on master. Measured at
  `f648bb96e` on live FDB: all 25 cells of
  `pkg/relational/sqldriver/int_long_sarg_matrix_probe_test.go` (literal and
  correlated-column comparands × both directions × `=`,`>`,`>=`,`<`,`<=`)
  produce an `IndexScan` with correct rows. The cells are split into two
  NAMED groups so the count is honest at a glance rather than resting on a
  comment: `mutation_proven` (10) go RED when the guard is removed;
  `inert_regression` (15) stay GREEN with both guards removed and are
  coverage against OTHER regressions, which their failure message says.
  Verified per group, not asserted — dropping both guards yields exactly
  `10 FAIL mutation_proven / 15 PASS inert_regression`, and dropping only
  `sharesIntegerWireEncoding` reddens exactly the 5 `correlated_bigint`
  cells while `long_literal` stays green behind the `IsConstantValue` early
  return. The item's "both directions" framing is also wrong:
  `maximumType(INT,LONG)` is LONG, so the wrapper always lands on the INT
  side and costs the probe ONLY when the INT side is the indexed column —
  `bigintCol = 5` promotes the literal and is structurally immune.
  Java 4.12.11.0 is the loser here, not the spec to converge on: it injects
  the promote unconditionally (`RelOpValue.java:209,217-218`; no INT/LONG
  fast path in `PromoteValue.inject`, `PromoteValue.java:444-449`; no
  simplification rule removes it) and its own checked-in baseline shows the
  probe lost — `sql-functions.metrics.yaml:204`,
  `ISCAN(T1_IDX1 <,>) | FILTER promote(_.COL3 AS LONG) EQUALS promote(@c9 AS LONG)`.
  Go's guard is therefore a sanctioned read-side extension, recorded in
  DIVERGENCES.md ("INT-vs-LONG stays sargable in Go"). The Java half of that
  comparison is INFERRED from source plus that golden, not from a Java run.
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
  (HISTORICAL STATE — superseded by the FIXED/CQ-83 notes below; the live
  binder `bindScanComparisonsToRangeSetWithTerminalWidening`
  (`scan_range_binding.go:273`) now also handles the NON-terminal case, by
  emitting both signed zeros as range-set ALTERNATIVES at `:390-406` rather
  than leaving the column un-widened.) As of CQ-28's filing,
  the CQ-27 fix — then in `scanComparisonsToTupleRange`, since retired,
  RFC-217 — only widened a zero bound at the
  position that terminates the range (nothing pinned after it), because
  widening a MIDDLE column requires either a genuine two-way range union
  (not expressible as a single `TupleRange` low/high pair — verified: the
  interval `[(-0.0,5), (+0.0,5)]` is not the 2-key set `{(-0.0,5),(+0.0,5)}`,
  it also admits every `(-0.0, x>5)` and `(+0.0, x<5)`) or teaching the SARG
  matcher (`abstract_data_access_rule.go`/`match_candidate_index.go`) to
  keep a residual filter for the abandoned trailing predicates when it
  decides a composite equality prefix is "fully sarg'd, no residual
  needed" — infra that did not exist at CQ-28's filing. Needs its own RFC (this is
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
  → **The correlated gap is CLOSED 2026-08-01 by CQ-83 (execution-time probe
  split, `zeroFork` in the executor) — see CQ-83 for the mechanism and pins.**
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
  CQ-28 — it blocks CQ-28 and is user-visible on its own.**
  → **NOW BOOKED AS ITS OWN ITEM: CQ-75.** It was written into the prose of an
  item that is marked `- [x]`, which makes it unreachable work under this file's
  execution rule ("pick the lowest-numbered UNCHECKED item"). The text stays here
  as the measurement record; CQ-75 is where it gets done. (The sentence above was
  also physically corrupt — two blank lines had swallowed the words after
  "independent of" — so the defect was hiding inside a broken sentence inside a
  closed item.) Over rows
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
  item. → **NOW BOOKED AS ITS OWN ITEM: CQ-76.** This one literally asked for its
  own item and never got one; "trailing equality" appeared nowhere else in this
  file, so the request was invisible from the moment it was written.
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

### [x] Finding 8 (MED, latent hang) — merge cycle guard walks all members — DONE
Landed in `eac6ef9ab`: `reachable` traverses `AllMembers()` rather than relying on the undocumented
"every final is exploratory" invariant. `TestMemoMerge_SkipsCyclicMergeThroughFinal` constructs an
ancestor edge visible only through a distinct final member and proves `mergeable` declines the
self-cycle.

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

# ✅ FREEZE LIFTED — RFC-173 landed. The banner below is HISTORY, not a directive.

**The freeze is OVER and this heading is kept only as the record of it.** RFC-173's endgame is
DONE (item 4 at the "Name-string/ordinal seam" entry above: the deletion pass is complete —
`QueryResult.Datum`, the `AnchoredJoin` flag+producer, `NewAnchoredJoinRecord`,
`buildUnnestResultValue` and the dual-window oracle machinery are all gone from the tree). The
work that followed it — RFC-195, RFC-197, RFC-199 through RFC-203 — all merged with this banner
still standing, which is the proof it stopped being true long before it was corrected.

Why this mattered enough to fix rather than delete: measured when the banner was lifted,
**83 of the 93 then-open items in this file sat BELOW this line** — under a directive that says
"Do NOT pick up any item below", i.e. nearly the whole backlog. A stale freeze does
not read as stale; it reads as an instruction, and the execution rule for this file is "pick the
lowest-numbered unchecked item". Deleting it would erase the lesson, so it is lifted in place.

---

**Owner directive (2026-07-01), SUPERSEDED — pause ALL other project work until RFC-173 lands.** Do NOT pick up
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
- [x] **Slice 1 non-join ordinal — MERGED (#437, `12516e33f`), all four gates ACKed at the merge
  HEAD** (Graefe impl+delta ACK · Torvalds fix-first→ACK incl. MUTATION-TESTING the authority proof ·
  codex clean (P2 fixed; delta finding = documented Go≥1.22 loopvar false positive) · @claude "nothing
  blocks merge"). Ordinal resolution authoritative on the non-join frontier; buried-reference
  reverse-map retired (4 RFC-082 divergences lifted, real-Java-validated); §5 dual-window differential
  standing (1617 entries, first catch fixed: recursive-CTE computed-column silent-wrong, known-red
  lock shrank); dual-emission benchmark satisfied (+71% window, `positionalTypeCache` → Slice 4 ends
  net-faster); 1M stress FASTER than pre-merge master (scan_all_wide 4.52s vs 5.57s — the cache repays
  the window). Full execution log: RFC §4 Slice 1.
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
    **TestFDB_CorrelatedIndexExistsStaysIndexed** — `FROM a,b,c WHERE b.aid=a.id AND c.aid=a.id AND
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

  - [x] **F2-LEFT DONE, re-established by RFC-235 after briefly regressing.** Retiring the NLJ
    rule's three-quantifier arm removed the only path that planned this shape; RFC-235 then
    restored it the way Java has it, by BOXING the outer join into one quantifier so the
    enclosing select is binary. `TestFDB_ProjectedExistsOverLeftJoin` passes all five dims,
    `TestFDB_LeftJoinExistsResidual` and `TestFDB_KeyBindingAndBuriedExists` pass in full, and
    `conformance/projected_exists_left_join_java_probe_test.go` keeps the Java-parity claim
    MEASURED against the live 4.12.11.0 JVM instead of inherited. See RFC-235 §16.

    ORIGINAL ENTRY, whose mechanism description is still accurate:
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
>        GUESSER DISPOSITIONS (measured, not assumed — the RFC's N-F4 list is
>        partly stale): (i) descriptorForColumn was the ONLY one producing wrong
>        client metadata. Its first-match arm is GONE: it now declines when the
>        candidate descriptors DISAGREE (SQL type or cardinality) and keeps
>        first-match only where every candidate answers identically, so the
>        choice is unobservable. Scoping the decline to disagreement is what
>        keeps a join whose legs share a NOT-NULL PK name reporting what it
>        reported before — an unconditional decline moved nullability and turned
>        TestFDB_OuterParity_NullSupplyingNullability red. (ii) legPlanFor is NOT
>        a guesser: it is already UNIQUE-match-or-decline on the correlation and
>        then reads types POSITIONALLY by ordinal from the resolved leg — already
>        the Java discipline. (iii) qualifyAndMergeColumns derives no types; it
>        merges per-leg ColumnDefs positionally in leg order and qualifies the
>        datum-key Name so same-named legs stay distinct. That is a deliberate
>        divergence and it is SAFER than Java, which keeps duplicate names and
>        resolves getXxx(label) by first-match (RowStruct.java:328-339).
>        (iv) innerByName SURVIVES — the handoff's func-level grep missed it
>        because it is a LOCAL map in deriveColumnsFromProjection, not a func.
>        It is a last-wins name map, still in scope for the phase.
>        STALE BOOKING: the arrayElementTypeNameFromDescs first-match nit booked
>        above no longer exists — that function is gone.
>        REMAINING (unstarted, review-gated): colref.go still has 28 production
>        consumers; deleting it and flipping ResultColumn*ForPlan to positional
>        is the D3 core and needs the query-engine gate.
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
> - [x] Slice 5 DONE — and its own gating rationale was REFUTED, which is the
>       part worth keeping. The entry said both arms had live PRODUCERS and that
>       "killing before the producers would delete live defenses for
>       constructible shapes (RFC-142 zero-rows / misclassification hazards)".
>       Both halves are now settled and neither held:
>       * fieldValueAliasAndCol went under RFC-197 item 2, with its dup-alias
>         producer, without waiting for Phase B unique quantifier aliases.
>       * MergeSeedLegsOfValue did NOT defend a live shape. The producer it was
>         said to defend — the enclosed-unnest name-model residual's dotted
>         merged read — could not escape translateUnnestJoin: the function's
>         only success return is guarded by `resultValue != nil`, and the sole
>         assignment leaving that non-nil also overwrites innerQ with the
>         ORDINAL-baked Explode. Every other path nils it and raises 0AF00, so
>         the name-model quantifier was unreachable BY CONSTRUCTION, not merely
>         unprobed. Measured alongside: the query and embedded suites are
>         byte-identical with the deleted producer restored.
>       The zero-rows hazard is real and is now carried structurally — the
>       collection is an ordinal bake over its OWNER's quantifier, so
>       GetCorrelatedTo reports the dependency directly. Pinned at BOTH
>       consumers by TestGatheredExplodeOwnerEdgeReachesPartitionOrder and
>       TestGatheredExplodeOwnerEdgeReachesMatchEnumerator
>       (pkg/recordlayer/query/plan/cascades/gathered_explode_owner_edge_test.go),
>       whose name-model arms assert the owner dependency is ABSENT for a dotted
>       collection — restoring the string recovery turns them red, which is what
>       stops this deletion from being reversible in silence. Producer side:
>       TestExplodeCollectionsAreOrdinalBaked
>       (pkg/relational/core/query/explode_collection_ordinal_test.go).
>       Lesson recorded rather than deleted: "dead-in-effect but the producer
>       lives" was the wrong question. The producer DID live in the source and
>       was unreachable in the graph, and only reading the return guard settled
>       it — the probe runs the entry cited could not, because a probe over
>       unreachable code is silent for the same reason a probe over absent code
>       is.
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
>   **→ DUPLICATE. This is the same failure as `CQ-47`, which is the live booking** (later run dates,
>   07-24/07-25, same `0/50 pass, 50 fail`, same ~5.2s uniform timing, same FDB-ALIVE conclusion).
>   Deduped 2026-08-01: triage it under CQ-47, not here. Kept as the earlier sighting — the 07-08
>   date matters, because it means this predates the fake-green window bug rather than being caused
>   by it.
> - **Differential Serialization Fuzzer** (07-14, 07-11): `FAIL: FuzzGetValueReply` — a real Go-vs-C++ WIRE
>   serialization divergence in `cmd/fdb-diff-oracle` (fdbgo GetValue reply). Its crash seed was not
>   uploaded. Needs the C++ oracle to reproduce; own fdbgo cycle.
>   **→ NOT BOOKED ANYWHERE ELSE.** Unlike the binding-stress line above, this one has no CQ item;
>   it is a stated Go-vs-C++ *wire* divergence, which is the hard line. It is also on the
>   `fuzz-diff` lane, which the nightly reconciler reports has NEVER recorded a genuine run — so
>   nothing has re-tested it since. Book it when the lane is revived (see `road-to-prod.md`, B1).
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
     **N/A:** index-scrub rangeSet fix (#4226) — Go has no scrubber. Gate: Torvalds + codex + @claude.
     ~~`SlidingWindowIndexMaintainer` (+163, #4233-adjacent) — pure metrics instrumentation for an
     HNSW window-decorator index type Go does not have~~ **SUPERSEDED** — that "N/A" rested on Go not
     having the index type, which is no longer true: the whole decorator is ported (keyspace 10,
     `slidingWindowIndexMaintainer`), and its instrumentation came with it.
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
         An empty/unset plain repeated array keys 0, not NULL — a repeated field is always an array and
         an empty one is `[]` (Java: `Key.Evaluated.scalar(getRepeatedFieldCount(...))`, no zero case).
         A NULL key arises only where the array can be ABSENT: an absent wrapper, or a null parent on a
         deeper nesting, both of which reach the fallback.
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
     - **[x] §3a follow-up — nullable-array-wrapper WRITE.** Closed in two halves. RFC-204 P1 landed the
       wrapper write side, so a Go-written NULLABLE array now distinguishes a stored NULL (absent wrapper)
       from a stored `[]` (present wrapper, empty list). The CARDINALITY half is closed too, but NOT by the
       wrapper — this item mis-framed it. A NOT NULL array is FLAT repeated in Java as well, so no wrapper
       was ever coming; `CARDINALITY([])` was 0-not-NULL all along and Go simply special-cased zero into a
       NULL key on both the count and the read. Both special cases are gone (CQ-89): the row builder reads
       a repeated field before any presence test, and the key evaluator keys the count verbatim.
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


### [x] query-engine (RFC-173 S4 — RESOLVED by Graefe FEASIBILITY ruling: the correlated-index EXISTS name path is PERMANENT / Java-correct — NOT a shortfall, NOT a cap blocker)
UN-BOOKED. The "ordinal-fold-over-index-matched-box" enhancement is **NOT ACHIEVABLE and NOT NEEDED**
(Graefe feasibility ruling, confirmed at 4 code sites). A WHERE-EXISTS correlating into a leg BURIED in an
inner join is the canonical semijoin; its good plan is a CORRELATED INDEX SCAN (`Scan(A,[=b.aid])` SARG'd
under the FlatMap) which REQUIRES NAME BINDING to flow the sibling comparand into A's index. All three sites
below are in `rule_implement_nested_loop_join.go`, named by SYMBOL rather than by line because the line
numbers this ruling first carried (`:1973`, `:449`, `:1244`) had all rotted by the time anyone re-read it:
the `correlatedStep1` predicate IS the index-SARG signal; `buildCorrelatedFlatMapPlan`
passes the name-model RC straight through with NAMED correlations; `foldStep1Seed` returns
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
do NOT need name binding (independent-legs materialized joins).** Pinned: TestFDB_CorrelatedIndexExistsStaysIndexed
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
   (`coerceTupleElementForKey` in `pkg/recordlayer/query/executor/executor.go:1041` — landed under the name
   `coerceTupleElement`, wrapping rather than replacing the narrower `uuidToTupleElement`, which it still
   delegates to at `:1042`): the row-domain convention deliberately keeps a FLOAT-typed value as float64 everywhere
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
   The same `coerceTupleElementForKey` packer fix from (1) makes the FLOAT variant of this case work at the wire
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

### [ ] identifiers: Go OVER-resolves a quoted DDL column that Java resolves only by its exact spelling (RFC-181 WS-N Phase D residual)

**REWRITTEN 2026-08-05 — the previous framing was REFUTED by measurement.** This entry used to claim
that a quoted DDL column was "created but unreferenceable by name", with *no* reference form
resolving it. That is false, and was false in both directions: the column resolves, and Go resolves
*more* spellings than Java, not fewer. The old claim had never been measured against the Java server;
it is the folklore case the watch-list contract exists to catch (road-to-prod.md entry 12, CQ-80).

Measured on both engines (`conformance/quoted_identifier_case_java_probe_test.go`), for
`CREATE TABLE qcase (id BIGINT, "KeepCase" BIGINT, plain BIGINT, PRIMARY KEY (id))`:

**HALF OF THIS ENTRY IS NOW STALE — read the corrections before the tables.**
RFC-237 landed and moved two of the facts below:

1. The label column in the table is FIXED. Go reported `KEEPCASE` for
   `SELECT "KeepCase"`; it now reports `KeepCase`, agreeing with Java, and the
   JVM probe asserts the agreement rather than the divergence.
2. "Where it lives" below names `rlcatalog.recordTypeTable.LookupColumn`'s
   `foldedIndex` and `StripIdentifierQuotes`. Neither exists: `LookupColumn` is
   exact, the relaxed pass moved to the SCOPE (`semantic/scope.go`), and the
   function is `NormalizeIdentifier`. The over-determination argument the
   entry rests on was measured against that old shape.

What SURVIVES is the divergence itself — Go still resolves spellings Java
rejects — and its current framing, with the current mechanism and the current
remediation, is in `DIVERGENCES.md` ("Identifier resolution: Go over-resolves
case, Java compares exactly") plus RFC-237 §3.3. Read those; this entry is kept
for the measurement history, not as a description of today's code.

| reference | Java | Go |
|---|---|---|
| `SELECT "KeepCase"` | ACCEPT, label `KeepCase`, 42 | ACCEPT, label `KeepCase`, 42 (was `KEEPCASE`; RFC-237) |
| `SELECT KeepCase` | REJECT 42703 | ACCEPT 42 |
| `SELECT "KEEPCASE"` | REJECT 42703 | ACCEPT 42 |
| `SELECT "keepcase"` | REJECT 42703 | ACCEPT 42 |
| `SELECT "PLAIN"` (unquoted DDL) | ACCEPT 7 | ACCEPT 7 |
| `SELECT "plain"` (unquoted DDL) | REJECT 42703 | ACCEPT 7 |

So the divergence is that Go's resolution is case-INSENSITIVE where Java treats quoting as
case-PRESERVING. Go accepts references Java rejects; it never rejects one Java accepts.

**Wire compat is intact, and that is MEASURED, not inferred** (`ddl_quoted_case_wire_test.go`): the
stored proto descriptor keeps `KeepCase` verbatim off the real `CREATE TABLE` path while the
unquoted sibling folds to `PLAIN`, so a Go-created and a Java-created table carry the same field
name and each engine still reads the other's records. This is a read-side name-resolution
divergence only.

**Where it lives.** The permissive step is the case-insensitive fallback in
`rlcatalog.recordTypeTable.LookupColumn` (`foldedIndex`), whose comment scopes it to raw-proto
metadata that never saw DDL normalization but which also swallows the quoted-case distinction.
Removing that fallback flips all three folded spellings to 42703 (verified as the mutation behind
the pin). Note the fold is **over-determined** across the read-side name model: mutating
`PositionalTypeForDescriptor`, the `cascades_translator` field naming, `StripIdentifierQuotes`, or
`expr.sourceColumnOrdinal`'s `EqualFold` each left the SQL-visible behaviour unchanged. A fix must
therefore be a name-model change, not a one-site patch — which is RFC-181 WS-N Phase D (the
case-preserving row layout), already cited by `ddl.go` as the reason case-colliding quoted columns
are rejected at CREATE.

**The fix, unchanged in direction by the rewrite:** port Java's case-sensitivity model —
`SemanticAnalyzer.normalizeString(string, caseSensitive)` ("taken as-is if caseSensitive,
upper-cased otherwise") with an `isCaseSensitive` flag set per-identifier by quoting — so quoting
consistently selects case-sensitive handling in resolution and star-expansion. Then
`SELECT "KeepCase"` resolves (as it already does) and `SELECT KeepCase` / `"KEEPCASE"` /
`"keepcase"` correctly do NOT (as they currently, wrongly, do). Niche — mixed-case quoted column
names are uncommon, and the failure mode is over-acceptance rather than wrong rows or wrong bytes —
but a real divergence. Gated on the WS-N name model because of the over-determination noted above.

**What the earlier diagnosis got right, kept because it saves the next reader the re-derivation:**
DDL does not fold the quoted case — `DOUBLE_QUOTE_ID` (RelationalLexer.g4 = `'"' ~'"'+ '"'`) keeps
the quotes, so `StripIdentifierQuotes` yields `"KeepCase"`→`KeepCase` and the proto field is written
preserved. That prediction is now confirmed by `ddl_quoted_case_wire_test.go` against the real DDL
path rather than asserted from the source. **What it got wrong:** the conclusion that "no reference
form resolves", including the exact spelling. Every reference form resolves; the exact one also
resolves on Java. The old bullet listing the inconsistent `ByName` lookup sites was reasoning toward
a symptom that does not exist.

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
(`uuidToTupleElement`, reached from the scan-range binder through `coerceTupleElementForKey`,
`executor.go:985`/`:1041` — the decision was written against the since-retired
`scanComparisonsToTupleRange`, RFC-217) and the index-entry write (`scalarToInterface`,
`recordlayer/key_expression.go:350` —
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

### [x] fdbgo/client (#28, HIGH) — SHIPPED: commit ships the UNFOLDED mutation log; libfdb_c commits the COALESCED RYW write map → Go throws 2101 (`transaction_too_large`) on a transaction C++/Java commit fine — needs its own `fdb-client-engineer` RFC (commit-path materialization; verified vs cgo)

**DONE.** The commit path materializes the coalesced write-conflict snapshot:
`pkg/fdbgo/client/commitpath.go:343` — "`writeSnap` is the caller-supplied write-conflict snapshot —
**coalesced for a RYW commit (#28)**, or the raw op-log ranges for a `rywDisabled` commit — so the
shipped conflict set matches what Commit sized." The box stayed unchecked after the fix landed.

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

### vs Java (correctness/feature parity)

- [x] **Correlated filter without index.** Fixed in 56874f23 — ImplementFilterRule sets innerAlias on RecordQueryPredicatesFilterPlan. All correlated paths (scalar subquery, EXISTS, JOIN) work without indexes. 14+ integration tests verify.
- [x] **RIGHT/FULL OUTER JOIN.** Done in RFC-036. (The old "only LEFT OUTER" note was stale — RIGHT already worked via operand-swap normalization in `cascades_translator.go`, pinned by `TestFDB_RightJoin`.) FULL OUTER added as a Go-only query extension: Java's SQL layer has **no** outer joins at all (`visitOuterJoin` is a no-op, zero tests), so LEFT/RIGHT/FULL are all read-path-only extensions with **zero wire-format impact** — Java apps still read/write the same records. FULL OUTER is implemented exclusively by the materialized NLJ cursor (`streaming_cursors.go`): LEFT-OUTER outer loop + a `matchedInner` bitmap + a drain phase emitting unmatched inner rows NULL-padded on the left. Routed away from the correlated FlatMap path (cannot observe global inner-match state); FULL+EXISTS rejected with a clear error. 9 FDB integration tests (all four row classes, NULL-key 3VL, many-to-many, large-inner hash+drain, WHERE-above-join, determinism, RIGHT NULL-key regression). Graefe+Torvalds ACK.
- [x] **Correlated scalar subquery shapes widened.** Non-aggregate (ORDER BY + LIMIT), multi-table inner FROM (JOINs), multi-column validation, deep-walk replaceScalarSubqueryRef, and real-aggregate GROUP BY/HAVING are supported. Group-key-only HAVING and other unproven shapes reject typed-loud. CorrelatedExistsError propagation fixed.
- [x] **GROUP BY/HAVING in correlated scalar subqueries.** Done in RFC-047 and cardinality-corrected by CQ-4 — a Go-only read-side extension (Java rejects correlated scalar subqueries at the grammar level entirely; zero wire impact). The stale "PredicatePushDownRule AliasMap.Compose conflict" blocker no longer applies: GroupByExpression is already a push-down barrier (no case in `pushPredicateToExpression`) and the panicking `AliasMap.Compose` has no production callers. `buildCorrelatedScalar` builds GROUP BY (+ real-aggregate HAVING) into the inner plan and leaves an unpaged grouped result uncapped; the shared strict FirstOrDefault barrier raises 21000 if more than one post-HAVING group survives. Explicit LIMIT 0/1 is preserved exactly and intentionally selects at most one group. Empty input → 0 groups → LEFT-scalar NULL falls out naturally (vs no-GROUP-BY COUNT → 0). Group keys + aggregate operands resolve via the semantic scope (`ResolveIdentifier`), scalar column named with the bare operand to avoid an embedded-`.` qualifier mis-parse. 42803 is enforced via `validateGroupByProjection`; group-key-only HAVING, multi-column + EXISTS-in-HAVING, and unresolvable expr-arg/key shapes reject typed-loud. FDB integration probes pin StreamingAgg, empty→NULL contrast, expression group keys, join+GROUP BY, post-HAVING 0/1/2+, and exact 21000.
  - [x] **Follow-up: `ORDER BY` over grouped output in a correlated scalar subquery.** Done in RFC-085 and hardened by CQ-5. Group keys and selected aggregates are resolved through the semantic scope, structurally rebound to exact native `[keys..., calls...]` ordinals, sorted over the private aggregate row, and exposed through one exact one-field Project before a written LIMIT/OFFSET. The correlated lowering therefore sees `Limit(Project(Sort(Aggregate)))`, can reattach pagination per outer row, and always reads scalar ordinal 0 from a proven one-column row. Zero-call grouping uses the same contract. Qualified/unqualified single-source spellings are symmetric; joined same-bare keys remain distinct; an unselected aggregate or non-grouped field rejects 42803. Without LIMIT, a second group still raises 21000. Pinned by `ordered_grouped_scalar_subquery_fdb_test.go`, `correlated_scalar_cardinality_followup_fdb_test.go`, and `quality_probes_test.go`.
  - [x] **Follow-up (single-source): expression/constant-argument aggregate that meets a *differing* aggregate via HAVING in a correlated scalar subquery.** DONE — the addendum unified producer and consumer on **one** canonicaliser (`canonicalAggName`, called by both `buildCorrelatedScalar` and `rewriteAggregateValue`), so the two name schemes can no longer drift; the prior fail-safe rejection is gone for single-source. The last silent-wrong corner (nested-arithmetic args like `SUM((amount+10)*2)` returning NULL → dropped groups) was a *separate* root cause — an inverted `!isArith` guard in `translateAggregate` that preferred a lossy text reparse over the resolved operand — fixed in RFC-048 (4dc3276c): the resolved `AggregateOperands[i]` is now always the source of truth. Works now (single-source): `SELECT COUNT(1) … HAVING COUNT(*)` both directions; `SELECT SUM(a*2) … HAVING SUM(a*3)`; decimal-literal args (`SUM(a*1.5)`); nested-arith args (`SUM((a+10)*2)`). `COUNT(DISTINCT 1)` correctly still rejected (DISTINCT unsupported here). Pinned by `quality_probes_test.go` (count_constant_with_having_works, expression_aggregate_in_having_works, decimal_literal_aggregate_arg_in_having, nested_arithmetic_aggregate_arg_in_having). **Residual (join only):** over a JOIN an expression-argument aggregate in HAVING is still rejected (the operand binds to the wrong quantifier through the parser round-trip) — pinned by `join_expression_aggregate_in_having_rejected`.
- [x] **🚩 IN over an indexed column drops the outer projection (wrong result schema).** Fixed in **RFC-070**. Root cause was two defects: (1) `MergeProjectionAndFetchRule`'s fallback dropped the projection when the fetch's child was an InJoin (not a coverable index scan), leaking a bare `InJoin` ([ID,A]) into the root projection group where it won on cost; (2) `physicalProjectionWrapper`/`physicalFetchFromPartialRecordWrapper` `WithChildren` didn't relink a compound-join inner during extraction (left `Project([id], InJoin(<nil>))` / `Fetch(<nil>)`), because of an `isLeafReplaceable` gate — same gate RFC-069 removed from the in-memory sort wrapper. Fix: fallback retains the projection; the two transparent caps relink unconditionally. `SELECT id FROM t WHERE a IN (1,7)` → `Project([ID], InJoin(IndexScan(IDX_A,[=])))`; `SELECT id+100 ...` (was 0 rows) → `{101,107}`. Pinned by `TestFDB_INProj_OuterProjectionOverInJoin` (indexed+unindexed, multi-column, expression-projection, 8× determinism). Graefe+Torvalds ACK.
- [x] **DML does not execute through Cascades (parallel pipeline).** Fixed as **P0.4** — all DML now executes through Cascades (`planDML`); the naive `execStatement` DML path is deleted. See P0.4.
- [x] **🚩 `INSERT … SELECT … GROUP BY` wrote the wrong columns (spurious 23505).** Fixed initially in RFC-084 and subsumed by CQ-5. Every aggregate SELECT now owns the same exact SELECT-order public Project, so INSERT consumes ordinary positional output; the insert-only `wrapBareAggregateInsertSource` coercion and its qualifier-based skip are deleted. COUNT(*), reordered/multiple aggregates, HAVING-only private calls, aliases, and qualified sources all use the universal boundary. Pinned by `groupby_insert_select_fdb_test.go`.
  - [x] **Follow-up (RFC-084): qualified aggregate operand on the insert-source path.** Fixed by structural operand resolution plus the CQ-5 exact public Project; qualified INSERT…SELECT variants now assert their rows.
  - [x] **Follow-up (RFC-084 / RFC-079): delete insert-only aggregate coercion.** Done by CQ-5: aggregate SELECT itself supplies the public Project and `wrapBareAggregateInsertSource` is gone. Whole-SimpleTable builder unification remains separately tracked below.
- [x] **🚩 Aggregate result-type derivation diverges from Java: `AVG(x)→DOUBLE`. DONE — RFC-083.** `AggregateValue.Type()` now types `AVG → NullableDouble` (function-determined, matching Java `AVG_*→DOUBLE`); SUM/MIN/MAX stay operand-derived, COUNT→LONG. The "ZERO new code / existing IsPromotable check" framing was **inaccurate** (no plan-time promotion check existed — `IsPromotable` had zero callers; the only enforcement was a runtime band-aid), so the fix is three coordinated parts: (A) the AVG `Type()` arm + collapse the duplicate AVG→DOUBLE SQL-name encodings onto it (`valueTypeName`/`aggregateResultType` route through `Type()`); (B) a **plan-time promotion guard** at the INSERT…SELECT chokepoint (`checkInsertSelectPromotable`, the first production `IsPromotable` caller) keyed on aggregate **provenance** — `LogicalProject.AggregateSlots` (captured pre-rewrite via `containsAggregate`) for computed exprs like `AVG(v)+1`, and name-resolution against the producing `LogicalAggregate` for bare `AVG(v)` (whose projection slot carries a nil value) — so `AVG→BIGINT` is rejected 22000 **even over an empty source** (emergent from the lattice, not a materialized float); `rewriteAggregateValue` now preserves `Typ: av.Type()` (was discarding it as UnknownType); (C) converge the runtime converters — remove `ConvertToProtoValue`'s whole-float→int64 coercion (VALUES double→BIGINT now rejects 22000), and give `goToProtoValue` the promotable INT/LONG→FLOAT/DOUBLE widenings + an **emergent 22000 fallthrough** (also fixes the adjacent `SUM(BIGINT)→DOUBLE` gap that used to error). Pinned: `values_test` AVG-type pins, flipped both `ConvertToProtoValue` whole-float unit tests, new `goToProtoValue` widening/reject tests, `avg_double_insert_fdb_test.go` (scalar/empty-source/`AVG+1`-empty/`→DOUBLE`/`SUM→BIGINT`/`SUM→DOUBLE`/plain-arith/VALUES double-reject/index-presence EXPLAIN). insert_select.yaml corpus corrected. Ripple guard holds (AVG never lowered to `Sum/Count` ArithmeticValue division; no aggregate index → streams). Full `just test` green. Graefe+Torvalds ACK'd RFC (v4) + impl.
  - [x] **Follow-up (RFC-083): GROUP BY-aggregate INSERT…SELECT source.** Closed by CQ-5's universal typed Project: the promotion guard and positional target alignment always see the SELECT-list layout; the former bare-aggregate source no longer exists.
- [x] **🚩 TODO 7.6-union-remap — aggregate UNION branch with a mismatched output alias drops rows (pre-existing executor gap).** Fixed for STREAMING aggregates in **RFC-078**: (1) `executeUnorderedUnion` (executor_new_plans.go) now remaps later branches' columns to the first branch's names by position — it previously concatenated branch cursors with NO normalization at all (unlike the sibling concat `RecordQueryUnionPlan`/`executeUnionStreaming`); (2) `planColumnNamesWithMD` (executor.go) reports a `RecordQueryStreamingAggregationPlan`'s output names (group keys + alias-or-canonical) instead of descending through `GetInner()` to the input scan. `SELECT u.x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u` now returns both counts (was `[2, NULL]`). Pinned by `TestFDB_UnionAggregateColumnRemap`. Graefe + Torvalds ACK.
  - [x] **Follow-up (RFC-078/RFC-081): aggregate UNION join legs.** Historical physical-name reporting fixes remain, but CQ-5 removes the logical dependence on those private names: every SQL aggregate branch is Project-topped with an exact positional public schema. Ungrouped and grouped branches, including AggregateIndex/MultiIntersection realizations, normalize through that boundary.
    - [x] **Divergent aggregate spellings in grouped UNION branches.** Qualified `SUM(t.c)` and constant `COUNT(1)` are positive through the exact Project and are pinned by `TestFDB_UnionQualifiedAggregate` and `TestFDB_UnionGroupedCountConstant`. Unsupported aggregate forms such as DISTINCT remain explicit feature boundaries, not name-remap fallbacks.
  - [x] **(b) Go concat-union projection-alias — FIXED in RFC-079.** A UNION branch projecting a post-aggregate EXPRESSION with an alias (`SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b`, read by name) returned `[NULL,NULL]` — the legacy `buildSelectShell` builder (the UNION-branch path) built the post-agg projection with `nil` aliases, dropping the `AS x`. Fixed by extracting the projection-building loop into one shared `buildPostAggregateProjection` helper called by both `visitSelectGroupBy` (modern) and `buildSelectShell` (legacy) — one source of alias truth. Pinned by `TestFDB_UnionAggregateExprAlias` + `TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias`. Modern path plandiff byte-identical. Graefe + Torvalds ACK.

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
**2026-07-31 (CQ-67 / RFC-200 — the NESTED merged leg, steps 3a–3d′):**
baseline worktree at the branch point `719f6c8b0` vs the branch head, SAME
FILESYSTEM (sibling worktree under `.claude/worktrees/`), sequential fresh FDB
containers.

All subtests PASS on both sides. **Every row count identical on every shape**,
and **every EXPLAIN string identical** (10 lines, diffed, empty). Totals
151.64s → 152.16s (**+0.34%**).

Per-shape, before → after: PK lookups 5.105/5.061/4.973ms → 5.138/4.879/5.338ms;
idx_customer eq 5.894 → 5.992ms; idx_amount range (100017 rows) 133.04 →
142.08ms; idx_status count 332.54 → 339.08ms; full-scan filter 485.32 →
519.68ms; GROUP BY status 5.656 → 4.953ms; GROUP BY COUNT-only 5.307 → 4.530ms;
SUM by status 5.376 → 5.277ms; GROUP BY customer HAVING (47271 rows) 137.40 →
141.89ms; **JOIN 10×customers 15.25 → 25.61ms**; ORDER BY PK full (1M) 3.355 →
3.445s; ORDER BY PK + index filter 11.060 → 7.571ms; scan-all ordered (1M) 3.180
→ 3.338s; scan-all wide (1M) 3.360 → 3.410s; IN-list 15.28 → 15.87ms; PK needle
5.258 → 5.444ms; PK+filter needle 5.806 → 6.156ms; sparse filter 2.917 → 3.014s;
UPDATE by index 8.724 → 8.826ms; DELETE single 6.544 → 6.052ms. Bulk inserts
6.259 → 6.325s (100k) and 2m8.289 → 2m8.135s (1M).

**PARITY WAS THE EXPECTATION, and it is what was measured.** Zero plan movement
had already been established at 3d and 3d′ independently — the full suite's
goldens, plandiff and explaindiff are green, and the orientation-gate census
shows all 72 newly-checked firings MATCHING (see CQ-67) — so any timing move
here is load, not planning.

**The one outlier, JOIN at +67.9%, is CLOSED AS VARIANCE WITH DATA rather than
by argument.** Three further uncached runs per side:

| | run 1 | run 2 | run 3 | run 4 | range |
|---|---|---|---|---|---|
| before (`719f6c8b0`) | 15.25 | 29.97 | 26.85 | 23.94 | **15.2–30.0ms** |
| after (branch) | 25.61 | 27.36 | 26.90 | 32.83 | **25.6–32.8ms** |

The distributions OVERLAP, and the baseline's 15.25ms is the outlier LOW of its
own four samples — the original comparison put the branch's median against the
baseline's fastest run. It is a **10-row** shape at ~25ms, the smallest-N
measurement in the table, and its EXPLAIN is byte-identical across both sides.

**METHODOLOGY CAVEAT, recorded because it affects how these absolutes should be
read:** the filesystem was at **96% utilisation** during both runs (40G free of
932G). CLAUDE.md's own recipe warns that ext4 point-lookup latency degrades
sharply above ~95% and reports as a planner regression. Both sides ran on that
same filesystem, so the COMPARISON is sound — that is exactly what the
same-filesystem rule buys — but the absolute figures are inflated relative to a
healthy disk and should not be compared against rows measured at lower
utilisation.

**2026-07-31 (CQ-53 phase 2c — the seed-window map is keyed by leg identity):**
baseline worktree at the phase-2 head `6f9b718e2` vs the branch, same box,
sequential fresh FDB containers, `go test -tags stress`.

All 23 subtests PASS on both sides. **Every row count and every label
identical** (diffed the 35 measurement lines with the timing column stripped —
empty diff), and **every EXPLAIN string identical** (10 lines, diffed, empty).
No plan in the corpus moved, which is the load-bearing result: the change is
planner/translator identity plumbing plus the deletion of a bake arm measured
unreachable.

Timings within run-to-run noise. Two rows looked otherwise on the first pair
and are recorded because they did: `full scan filter amount>5000` 506ms base vs
694ms branch, and `idx_status count pending` 358ms vs 402ms. A second branch run
put them at 521ms and 353ms — inside the base. Reported rather than dropped, so
nobody re-derives the scare from the first pair alone.

**2026-07-31 (CQ-53 phase 2 — leg-correlated reads keep the ordinal they
arrive with; the qualified-name mint, `legRole` and the text alias half die):**
baseline worktree at the phase-1 merge `457c18ba8` vs the branch, same box,
sequential fresh FDB containers, both runs `--nocache_test_results`.

All 23 subtests PASS on both sides, and **every per-query row count is
identical** — `1` on the three PK lookups, `8` on index equality, `100017`,
`47271`, `10`, `1000000` on all three million-row scans, `46`, `97`, and the
rest; diffed line-for-line, empty diff.

Timings within run-to-run noise on a shared machine: full scan 3.51s base vs
3.38s branch, sparse filter 2.97s base vs 2.92s branch, point lookups
4.93..5.07ms on both sides. The base run was cold (721 build actions) and the
branch run warm (9), so total elapsed (220s vs 160s) is a cache artifact and
not a measurement — the per-query numbers above are.

This is the expected shape: the change is planner/executor identity plumbing
and the arm it touches produces no winning plan on the corpus (the wrong-leg
mutation leaves the suite green), so no stress query's plan can move.

**2026-07-29 (RFC-197 item 2 escape bucket — review fold: correlation guards
made load-bearing, leg comparison unified on case-folding `values.SameLeg`):**
baseline worktree at `master` = `e5477c9f2`, which IS the branch point
(merge-base == master), so the comparison isolates the branch's 9 commits.
Same box, alternating master/branch, fresh FDB container per run, all runs
`--nocache_test_results`. **n=3 per side** — not the usual 1, because the
first pair showed `UPDATE by index` non-overlapping (master 8.93/9.84 vs
branch 10.44/11.41) and n=2 cannot tell a 1.5ms regression from a cold-cache
artifact. The third pair dissolved it: master 8.585ms vs branch 8.607ms, and
the branch's minimum lands 0.02ms off the master's. Ranges overlap on every
row where the branch is the slower side.

All 23 subtests PASS on all six runs. **Every EXPLAIN string identical across
all four compared runs and every per-query row count identical** — the
load-bearing result, since the change is entirely cost-model/planner proof
code: no plan in the corpus moved.

Two rows have non-overlapping n=3 ranges and BOTH favour the branch — `PK
lookup id=0` (master 4.98..6.08 vs branch 4.88..4.97) and `scan all rows
ordered` (master 3259..3379ms vs branch 3152..3225ms). These are run-order
warmth (master r1 was the coldest run of the six), NOT a speedup the change
earns; recorded so nobody reads them as one.

Thresholds: index equality 5.69..5.73ms PASSES (<10ms); million-row scans
3.15..3.45s all ~3s PASSES. Point lookups 4.88..5.41ms and `in_list`
14.79..15.31ms still violate their <5ms / <10ms thresholds — on BOTH sides,
with the branch at or below master on every one of those rows. That is the
pre-existing baseline-vs-threshold gap already booked above (the entry
beginning "Stress test 1M baseline Threshold column"), unchanged and
unaffected by this branch.

**2026-07-31 (RFC-195 — the cost model must not contradict what the property
layer proves):** cost-model change, so the comparison is mandatory. Both sides
run in the SAME worktree via a save-diff / `git apply -R` / `git apply` cycle
rather than a second checkout — identical path, identical filesystem, identical
Bazel cache by construction, which is a stronger version of the
sibling-path-worktree rule (a cross-filesystem baseline is what produced the
2.3x phantom regression). Baseline is the branch point `f50cee43e`.

All 23 subtests PASS on both sides. **Every per-query row count identical** —
diffed all 22 measured row lines, empty diff. Total runtime 152.70s baseline vs
153.82s branch (+0.7%). Million-row scans baseline 2.99/3.37/3.20/3.33/2.95s vs
branch 2.87/3.40/3.26/3.33/2.92s — the largest single move is
`full_scan_count` at −0.12s (branch FASTER), and no subtest moves by more than
0.12s in either direction. Point lookups 5.26ms, index equality 5.77ms,
`in_list` 15.39ms on the branch: unchanged, and still against the pre-existing
threshold gap already booked above (the entry beginning "Stress test 1M
baseline Threshold column"), which this branch neither improves nor worsens.

Both runs used `--nocache_test_results`, so neither side could have been served
from the Bazel test cache — a cost-model change alters the test binary's inputs
and would produce a fresh cache key anyway, but stating it removes the question.

**Re-measured after the implementation review** (the DFS-join bound moves that
operator's cardinality off zero, so the comparison was re-run on the final
head). Post-review branch 306.96s. Read against the 152.70s baseline above this
looks like a 2x regression, and it is NOT: the box was carrying other agents'
full suites, load average 35 → 65 across the two runs. Root-caused by the
controlled experiment rather than waved off as contention — the BASELINE was
re-run immediately afterwards under the same load and came in at **319.18s**,
i.e. SLOWER than the branch. Matched-load per-subtest deltas are at-or-faster on
the branch everywhere (`order_by_pk_full` 9.05→5.86, `scan_all_narrow`
8.35→6.90, `full_scan_count` 6.74→6.26; nothing slower by more than 0.01s), and
every row count is identical across the matched pair. Wall-clock on a shared box
is only meaningful against a same-load baseline; the 152.70/153.82 pair above is
the quiet-box measurement and this pair is the loaded-box one.

Interpretation, stated because the null result is easy to misread: the clamp
DOES fire on real SQL — `SELECT COUNT(*) FROM orders` is priced at 700000 rows
before and 1 after, measured by
`pkg/relational/core/embedded/cardinality_clamp_reachable_test.go` — but no
plan CHOICE moves, on this corpus or in the stress suite. The corpus-wide
`explaindiff` agrees: 2643/2643 entries identical, 0 shape flips, 0
regressions — re-run on the post-review head, DFS-join bound included, and
still clean. Costs change substantially; rankings do not, because in these
shapes the corrected operator has no competing alternative whose order flips.
"Clean" here means low-risk, not inert, and the reachability test is what
separates those two readings.

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
- [x] **`LogicalProjectionExpression` identity ignores its output ALIASES**
  — closed by CQ-2 above. Both logical and physical projection identity now
  includes executor-visible output names, with memo non-collapse tests and
  alias-preserving elimination regressions. `NewRecordQueryProjectionPlan`
  remains for API/test compatibility, but now defensively copies its projection
  slice and shares the same immutable, schema-aware physical identity
  implementation.
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

- [x] **Thread the statement clock through the executor's row eval contexts. — DONE.** The executor's
  `EvaluationContext` now carries `statementTime` (`WithStatementTime` / `StatementNow`, implementing
  `values.StatementClock`); `cascades_generator.go` stamps it once per execution from the session
  clock BEFORE scalar subqueries evaluate; every RowEvalContext construction site sets `Clock`; and
  the bare-OrdinalRow frontier wraps exactly when the operator's values reference the CURRENT_*
  family (`values.DependsOnStatementClock` over values, `predicates.DependsOnStatementClock` over the
  shared predicate spine, plus the aggregate group-key/operand probe) — zero cost for the other
  99.99% of plans. Pinned red→green by `sqldriver/current_timestamp_stability_test.go`
  (boundary-straddling 10k-row loops on the projection AND predicates-filter shapes; cross-statement
  control mutation-proven against a frozen session clock). Original booking follows. `values.StatementClock`
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

## RFC-165 — ANSI SQL Core conformance backlog (read-side reach beyond the Java port)

Generated scorecard: `SQL_ANSI_CONFORMANCE.md` (Ledger A) tracks SQL:2023 Core (176 features per
PostgreSQL 18). `Go?` is derived from `# ansi:` corpus tags — never hand-typed. The **shared-gap**
rows (Java and Go both lack the feature) are this backlog; each is a wire-safe **read-side**
extension and ships under the full query-engine gate (Graefe + deep coverage). The **divergence**
rows (Java has it, Go rejects it) are RFC-164 port-fidelity bugs, not this list.

Shared ANSI gaps surfaced so far (extend by tagging more corpus + completing the roster, Phase 1):
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

- [x] **Correlated scalar subquery in WHERE — scoped quantifier lowering
      (RFC-180 Y4 / CQ-4, extension):** a single correlated scalar over a
      single-source outer is materialized per outer row through the shared
      LEFT-scalar join, consumed by the WHERE predicate on a fresh typed row,
      cardinality-checked, and hidden by an outer-only projection. The
      `scalar_subquery_java` Bob/Dave/Eve rows pin is restored. Multi-source,
      mixed-subquery, projected-EXISTS, and dual SELECT+WHERE scalar
      compositions remain explicit 0AF00 boundaries.
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

## Phase 11: Optimizer invariants

- [x] **CQ-29 (HIGH) — five cost estimates violate a proven cardinality bound.**
  DONE via RFC-195. SEVEN shapes in the end: the random-combo generator found a
  sixth (`typeFilter` over an exactly-one child), and the implementation review
  found a seventh — `RecordQueryRecursiveDfsJoinPlan` proved NOTHING, so the
  identical zero-collapse fixed on the level union stayed live on the DFS join,
  which is the alternative the cost model PREFERS (the level union carries a
  strictly larger buffer term by construction). Measured on identical children:
  `FlatMap(scan, dfsJoin)` costed 0 rows against `FlatMap(scan, levelUnion)` at
  1e6. Both recursive plans now share ONE bound (`recursiveSeedBound`), matching
  Java's single `visitRecursiveUnionExpression`; Java's own PLAN-level DFS arm
  returns the looser `unknownMaxCardinality`, a divergence taken deliberately in
  the sound direction and verified against Go's executor (`recursive_cursor.go`
  emits every root row).

  All seven now clamp to the proven boundary: ungrouped aggregation 700000→1,
  recursive-level-union 0→1, recursive-DFS-join 0→1, defaultOnEmpty 0→1, both
  distincts 0.7→1, typeFilter 0.5→1. The six
  `addExcluded` registrations are deleted and so is the exclusion MECHANISM —
  `cardinality_cost_bound_test.go` now holds the invariant over every shape with
  ZERO exclusions and no way to add one. Measured reachable from real SQL
  (`SELECT COUNT(*) FROM orders`: 700000→1) by
  `pkg/relational/core/embedded/cardinality_clamp_reachable_test.go`; corpus
  plan-diff over 2643 entries is CLEAN (costs move, plan CHOICES do not).
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

## Phase 12: Audit bookings — found-but-unbooked

Every item below was DISCOVERED by an audit and had no TODO entry. The audit's
standing rule is that a finding without a booking is a finding that evaporates,
so each is recorded here with its evidence refs, a size, and what "done" means.
None is speculative: each was re-verified against the tree before booking.

- [x] **RFC-218 remainder — the leg-window re-anchor. THE BOOKING UNDERSTATED IT:
  the decline was only ONE of the rebase's two arms, and the other silently
  returned WRONG ROWS.** · was M, actually L · RFC-222 · **query-engine change —
  needs a Graefe ACK before merge**
  `rebaseOuterLegValueOrdinal` dispatches on a reference's frontier PIN, and arity
  is orthogonal to it, so a multi-accessor reference took whichever arm its pin
  selected. PINNED it declined (`Single()`) — the booked behaviour. UNPINNED it took
  the NAME arm, looked up `fv.Field` (for a nested reference, the struct ROOT), found
  it, baked the merged address of the whole struct and DROPPED the descent. That is a
  real merged column of the wrong type, so nothing downstream rejects it.
  REACHED FROM SQL WITH NO `ORDER BY` AND NO PROJECTED `EXISTS`, measured on master
  with a row-preserving inner join that cannot change the answer:
  `WHERE EXISTS (... t2.t1_id = n.co)` returned `[]` where the single-table form
  returned `[1 3]`, and its `NOT EXISTS` twin returned `[1 2 3]` where the
  single-table form returned `[2]`.
  FIXED by dispatching on ARITY above the pin: re-anchor the root in the leg's own
  layout (`ReAnchorRootInto`), bake the merged address per leg KIND, then fuse the
  remaining accessors with `FieldPath.WithSuffix` — Java's `ofFieldsAndFuseIfPossible`
  (`FieldValue.java:525-534`). The root is DERIVED and the suffix CARRIED — and the
  carried suffix's identity is its NAME, not its ordinal: a struct column materialises
  as a `proto.Message`, whose descent arm reads the per-step name and never the ordinal
  (`values.go`'s standing divergence from Java, pinned by
  `TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal`). Measured one mutation
  per site × field: forcing either suffix ORDINAL is undetectable end-to-end, while
  swapping either suffix NAME reds — the wrong-rows pin at the rebase, both sort arms in
  opposite directions at the translator. An earlier version of this booking justified the
  design by "a suffix ordinal indexes a struct's own declared field order"; that is true
  and nearly irrelevant, because nothing on the reachable paths reads it.
  Neither the merged row nor the leg window carries a struct column's type (both state
  UNKNOWN, measured), so a design requiring the type declines every real nested reference.
  DONE, all four parts: the JOIN arm plans; `nested_key_over_a_join_declines_cleanly`
  is replaced by COLUMN assertions (`n.sk`→[3 2 1] and `n.co`→[1 2 3], opposite orders
  so the wrong member is visible); the arms mutation-red INDEPENDENTLY — reverting the
  rebase reds only the wrong-rows pin, reverting the translator reds only the sort arms;
  and `existsSortSplit` stays at DIVERGED 0 with its population REMOVED rather than the
  zero widened (the multi-accessor key is settled by identity and never reaches the
  rendered-name split).
  The corpus stanza was converted per record: the `0AF00` decline became a row
  assertion, `n.co` was added beside it, and the wrong-rows shape got its own file
  (`nested_correlation_over_a_join.yaml`) so the fold-coverage census is not inflated
  by entries the fold never sees. The coverage gate's decline count was RECONCILED,
  not relaxed — it was a floor and is now a ceiling, because zero is the steady state
  and a reappearing decline means a shape stopped planning.

- [x] **CQ-40 — two `LikeMatch` implementations disagreed. THE BOOKING HAD THE
  DIRECTION BACKWARDS: the "canonical" matcher was the wrong one.** · was S,
  actually M · **query-engine change — needs a Graefe ACK before merge**
  Unified onto one matcher AND corrected it against Java. The booking's DONE
  criterion ("delete `functions.LikeMatch`, adapt the map path to
  `values.LikeMatch`") would, executed literally, have PROPAGATED a live
  wrong-rows defect onto INFORMATION_SCHEMA instead of removing one.
  MEASURED on FDB, `SELECT B1 FROM B WHERE B2 LIKE 'Z' ESCAPE 'Z'`:
  the engine path returned `[]`; the map path returned the matching row. Java's
  own corpus settles it — `yaml-tests/.../like.yamsql:92` runs
  `B2 NOT LIKE 'Z' ESCAPE 'Z'` and excludes both `'Z'` rows, so
  `'Z' LIKE 'Z' ESCAPE 'Z'` is TRUE there. The SHADOW matcher agreed with Java;
  the canonical one did not.
  Root cause: `values.LikeMatch`'s trailing-escape rule ("MALFORMED → no match")
  was never Java's. `PatternForLikeValue` installs exactly two escape entries
  (`<esc>_`, `<esc>%`); an escape rune anywhere else falls through to the
  ordinary per-character rules and is a literal. So Java also has no
  escaped-escape (`a\\b` is two backslashes) and escape-before-an-ordinary-char
  escapes nothing — two further divergences the same rule hid. Go's OWN
  `values.sqlPatternToRegex` already implemented Java's rule correctly, so the
  package contradicted itself.
  Why nothing caught it: the doc comment cited `Comparisons.likeMatcher` as the
  spec — no such symbol exists in 4.12.11.0 — and `FuzzLikeMatchEscape`'s oracle
  hard-coded `return false // trailing escape — malformed`, i.e. it restated the
  implementation instead of modelling Java. The oracle is now built the way Java
  builds it (replacement-table translation to a regex), so it is an independent
  check; 38.6M execs, 0 mismatches.
  Landed: `functions.LikeMatch` deleted, `eval_predicate_map.go` routed through
  `values.LikeMatch` (sentinel changed from `-1` to `0`), the matcher corrected,
  oracle and truth tables re-pinned with Java citations. Pinned end-to-end by
  `pkg/relational/sqldriver/like_escape_parity_fdb_test.go` (both paths).
  NEWLINE AXIS — RESOLVED (measured against a real JDK). Java's path is
  `PatternForLikeValue.eval` → `^…$` → `Pattern.compile` with NO flags →
  `.find()` (LikeOperatorValue.java:93-99). Measured table (Go was wrong on
  all five before the fix; now pinned by
  `TestLikeMatch_JavaNewlineSemantics`):

  | expr | Java | old Go |
  |---|---|---|
  | `'a\nb' LIKE 'a_b'` | false | true |
  | `'a\nb' LIKE 'a%b'` | false | true |
  | `'\n' LIKE '_'` | false | true |
  | `'abc\n' LIKE 'abc'` | true | false |
  | `'a'+U+2028 LIKE 'a'` | true | false |
  | `'\n' LIKE '%'` | true | true (agreement — the half-fix sentinel) |

  Resolution: `values.LikeMatch` now rejects Java's five line terminators
  (`\n`, `\r`, U+0085, U+2028, U+2029) in `_`/`%` and accepts one FINAL
  terminator (`\r\n` as a unit; `$` never matches between its `\r` and `\n`),
  matching default-mode `Pattern$Dollar`. The fuzz oracle models exactly
  that (no `(?s)`; explicit terminator class + final-terminator trim) —
  dropping `(?s)` alone would have broken `'\n' LIKE '%'` since RE2's `$`
  is end-of-text. `TestLikeMatch_CrossCheckSQLPatternToRegex` binds
  `values.LikeMatch` to `values.sqlPatternToRegex` over an exhaustive
  ASCII grid including terminators.
  Retiring the rest of the shadow-evaluator family (`CompareValues`,
  `CastValue`, `ApplyMathOp`, `IsTruthy`) remains the follow-on, and the
  adaptation shape is now established. Original booking below.

  MEASURED divergence (probed 2026-07-29 over `escape='\'`):

  | pattern | string | `values.LikeMatch` | `functions.LikeMatch` |
  |---|---|---|---|
  | `a\` | `a\` | false | **true** |
  | `\` | `\` | false | **true** |
  | `ab\` | `ab\` | false | **true** |
  | `%\` | `x\` | false | **true** |

  `pkg/recordlayer/query/plan/cascades/values/like_match.go:23` is the CANONICAL
  matcher: its doc comment names Java's `Comparisons.likeMatcher` as the spec, it
  is fuzzed against a regex oracle (`FuzzLikeMatch` / `FuzzLikeMatchEscape`), and
  its trailing-escape rule ("MALFORMED → no match", `like_match.go:21-22`) is
  itself a fuzz-found bug fix. `pkg/relational/core/functions/like.go:8` is a
  second, independent implementation that never got that fix — its escape arm
  requires `len(p) >= 2` (`like.go:21`) and so falls through to the literal
  `default` arm, matching the escape rune against itself. It is reached from
  `pkg/relational/core/embedded/eval_predicate_map.go:88`, the map-backed
  INFORMATION_SCHEMA `WHERE` evaluator — so a `LIKE` against catalog metadata
  answers differently from the same `LIKE` against a table.
  This is one member of a SHADOW EVALUATOR family in `pkg/relational/core/functions`
  — `CompareValues`, `CastValue`, `ApplyMathOp`, `IsTruthy` — a straight "no
  parallel pipelines" violation (CLAUDE.md: "Java has one query path; Go has one
  query path"). The map path re-derives semantics the Value tree already owns.
  DONE = `functions.LikeMatch` deleted, `eval_predicate_map.go` adapted to
  `values.LikeMatch`, and the trailing-escape case pinned ON THE MAP PATH (an
  INFORMATION_SCHEMA `WHERE ... LIKE 'x\' ESCAPE '\'` scenario, not a unit test on
  the helper — the defect is that the map path uses the wrong helper, so a test
  that calls the helper directly cannot express it). Retiring the rest of the
  shadow-evaluator family is the follow-on, and should be booked separately once
  this one establishes the adaptation shape.

- [x] **CQ-41 — `API_PARITY.md` contradicted `options.go`; now machine-checked.** · S
  Premise CONFIRMED verbatim. Doc corrected and gated by `pkg/docscheck`'s
  `TestAPIParityTablesMatchOptionsGo`, which parses every option body in
  `options.go` with go/ast, classifies it from the STATEMENTS alone (comments
  cannot vote — a comment disagreeing with its own body is the defect class
  here), and fails on any disagreement with the page's three tables, in both
  directions. Anti-vacuity: floors on both parsed sets, a "no rejects found"
  guard, and a verified mutation showing a renamed receiver fails loudly
  ("parsed only 0 … the parse is broken, not the doc") rather than passing over
  an empty set.
  The gate found MORE than the booking listed: `SpanParent` was filed as a no-op
  while its body forwards to the transaction; the shorthand rows
  `ReadPriorityHigh`/`Low`/`Normal` and `ReadServerSideCacheEnable`/`Disable`
  named no real method, so three options were listed nowhere; and the two
  DB-level rejects were unnamed on the page. All corrected. Runfiles wired via
  `exports_files` so the gate is not vacuous under Bazel.

  Original booking text follows.
  `pkg/fdbgo/fdb/API_PARITY.md:56` and `:62` list `ReportConflictingKeys` and
  `BypassStorageQuota` under "**Accepted but ignored — no-op (fails safe)**".
  Both REJECT: `options.go:255` returns `&UnsupportedOptionError{Option:
  "report_conflicting_keys"}` and `options.go:362` returns
  `&UnsupportedOptionError{Option: "bypass_storage_quota"}` — each with a comment
  explicitly saying it "fails UNSAFE if ignored". The doc states the exact
  opposite of the code for two options, and the code's own comments name
  `SetReportConflictingKeys` as the precedent the others follow.
  The "Rejected" table (`API_PARITY.md:44-49`) lists 3 entries; `options.go` has
  SEVEN rejecting sites (`:255`, `:273`, `:285`, `:362`, `:372`, `:476`, `:480` —
  the last two on `DatabaseOptions`). A caller reading the doc to decide whether
  the pure-Go backend is safe for their workload gets a wrong answer.
  DONE = doc corrected AND a `pkg/docscheck` gate that parses `options.go`,
  classifies every option body as reject / no-op / honored, and asserts the
  classification matches `API_PARITY.md`'s tables — with a no-op guard on the
  parse, so a broken parser fails loudly instead of passing vacuously. Same shape
  as `TestNightlyWindowGatesAreReconciled`. A one-time doc correction without the
  gate re-rots on the next option added.

- [x] **CQ-42 — `SetSpecialKeySpaceRelaxed` / `SetSpecialKeySpaceEnableWrites`:
  decision taken — REJECT both.** · S
  Premise CONFIRMED (both were bare `return nil`, no comment). Decided against
  the C++ spec rather than by defaulting again.
  `special_key_space_enable_writes` is not a permission bit: it sets
  `options.specialKeySpaceChangeConfiguration` (`ReadYourWrites.actor.cpp:2607-2610`),
  which makes commit run `specialKeySpace->commit(ryw)` (`:1356`) — the step that
  translates writes to `\xff\xff/management/...` into real configuration
  mutations. Without it C++ raises `special_keys_write_disabled` (2114). The
  pure-Go client has neither the module nor that commit step, so a silent nil
  told a caller their configuration change was enabled while it silently never
  happened — the fail-open direction, unambiguously.
  `special_key_space_relaxed` relaxes C++'s one-module-per-read restriction
  (`special_keys_cross_module_read` 2112 / `special_keys_no_module_found` 2113);
  with no module there is nothing to relax and the reads it is relaxed FOR
  cannot succeed either.
  Decisive internal precedent: `SetReportConflictingKeys` is rejected precisely
  BECAUSE the special-key read-back module is absent. Rejecting that while
  silently accepting the two options that configure the same absent module is
  not a position the file can hold.
  Recorded in `API_PARITY.md`'s Rejected table with the reasoning, commented at
  both call sites, enforced by CQ-41's docscheck gate, and pinned by the
  existing reject table in `fdb_test.go` (which previously asserted the
  opposite, in the "stubs return nil" list). NOTE: `pkg/simfdb/options.go`
  no-ops these — it no-ops all five other rejects too, so it is a uniformly
  permissive test double, not a new inconsistency.
  Original booking text follows.
  `options.go:258-264`: both are two-line `return nil` with NO comment, while
  every other option in that file carries a fail-safe/fail-unsafe rationale.
  `API_PARITY.md:64-65` lists them as accepted no-ops with the parenthetical "the
  special-key-space module itself is absent". That parenthetical is the whole
  question and it is left hanging: if the module is absent, then a caller enabling
  relaxed access or writes to it is asking for something that cannot happen, and
  the silent `nil` tells them it did. Whether that is fail-safe (nothing to
  corrupt) or fail-unsafe (the caller proceeds believing writes are enabled) is a
  real decision that was never taken — it just inherited the default.
  DONE = the keep-or-reject decision made and RECORDED in `API_PARITY.md` with its
  reasoning, and the chosen behaviour commented at both call sites. If "keep as
  no-op" is the answer, that is a fine answer — but it has to be a decision, not
  an absence. Gate it with CQ-41's docscheck so the classification is enforced.

- [x] **CQ-43 — package doc added, but THE BOOKING'S PREMISE WAS FALSE and two
  shipped godoc comments were asserting it.** · S
  The documentation GAP was real (`pkg/fdbgo/doc.go` did not exist); the
  DIVERGENCE the item asked to document does not.
  MEASURED against the C++ spec: libfdb_c's per-transaction defaults are
  `timeoutInSeconds = 0.0` and `maxRetries = -1`
  (`ReadYourWrites.actor.cpp:2078-2082`), and `resetTimeout()` arms the
  `timebomb` only when the timeout is non-zero (`:1576-1578`); `fdb.options`
  says "If set to 0, will disable all timeouts". libfdb_c has NO internal
  timeout to differ from — the unbounded default is MATCHED, not divergent.
  Second false claim: `Open` does not block until the caller's ctx cancels —
  `fdb/database.go:139-153` bounds bootstrap at 60s when no deadline is
  supplied, so Go is STRICTER than libfdb_c, which waits forever.
  The RFC contradicted itself: `:161-162` says the unbounded default "matches
  C++" 50 lines above the P1.5 line that calls it "a real difference from
  libfdb_c's internal timeouts". A third supporting claim (P0.2, "only the
  caller's ctx bounds a stuck read") was already retired by RFC-112.
  This was not merely an absence: `fdb/key.go` and `fdb/database.go` both
  SHIPPED the false "unlike libfdb_c" framing in godoc. Corrected.
  Landed: `pkg/fdbgo/doc.go` stating the true contract (unbounded by default as
  in libfdb_c; pick `SetTimeout`, `SetRetryLimit`, or a deadline ctx via
  `TransactCtx` — the ctx being Go's EXTRA bound, not a substitute for a missing
  C++ mechanism), a README section, the two godoc corrections, and
  `rfcs/prod-readiness-go-client.md` P1.5 marked CLOSED-AS-MISFRAMED with the
  C++ citations rather than silently rewritten.
  Pinned by BEHAVIOUR, not prose: `client/unbounded_default_pin_test.go` asserts
  a default transaction still grants a retry at `retryCount=1_000_000` and that
  each of the three documented bounds actually terminates the loop. Mutation
  check: introducing an internal cap turns it RED with a message naming the doc
  files to update alongside.
  Original booking text follows.
  This is P1.5 of `rfcs/prod-readiness-go-client.md:210-212`, the one P1 item of
  that punch-list still open: *"Document the bounded-context requirement in
  godoc/README: with no internal max-retry, a `Transact`/`Open` against a down
  cluster blocks until the caller's `ctx` cancels — a real difference from
  `libfdb_c`'s internal timeouts. Migrators MUST pass bounded contexts."*
  VERIFIED still absent: `pkg/fdbgo/README.md` mentions `ctx` only in a dial-func
  code sample (`:204`, `:213`) and a chaos-test row (`:229`); there is no
  `pkg/fdbgo/doc.go` at all. The RFC itself repeats the requirement three times
  (`:161-162` "makes a bounded `ctx` on every `Transact` mandatory, not optional";
  `:164-165` "callers MUST pass bounded contexts"; `:227` in the Recommendation)
  — the knowledge exists, it just never reached the package a migrator imports.
  DONE = a `pkg/fdbgo/doc.go` package comment plus a README section stating the
  requirement and naming the divergence from `libfdb_c` explicitly, so `go doc
  fdb.dev/pkg/fdbgo` surfaces it.

- [x] **CQ-45 (HIGH, ROOT-CAUSED, PIN LANDED) — the 07-17 stress
  `exists_subquery` failure: an aggregate index was built into a record-fetching
  scan.** · S (pin only)
  **Nightly Stress 2026-07-17, run `29560169002`** —
  `TestFDB_Stress_10K/exists_subquery` FAILED with, verbatim:
  `EXISTS subquery: record not found from index entry
  (index_name=SUM_AMOUNT_BY_CUSTOMER, primary_key=(), index_key=(0))`.
  REPRODUCED DETERMINISTICALLY at the failing SHA `0e7104f71`. MECHANISM:
  `ImplementNestedLoopJoinRule.tryExistsFlatMap`'s "try secondary indexes" loop
  (`rule_implement_nested_loop_join.go:2629` at that SHA) matched match candidates
  by first-column NAME with no index-TYPE check.
  `AggregateIndexMatchCandidate.GetColumnNames()` returns the GROUPING KEY, so a
  SUM aggregate index matched on name and was built into a record-fetching
  `RecordQueryIndexPlan`, bypassing the enumerator's `IsAtomicMutationIndex` gate.
  `getEntryPrimaryKey` (`pkg/recordlayer/index.go:596`) returns an EMPTY tuple for
  the short aggregate entry → `LoadRecord` nil → the loud
  `IndexOrphanBehavior.ERROR` above. LOUD in every measured shape; NO silent wrong
  rows — aggregate entries are always shorter than the root `ColumnSize`, so a
  garbage fetch is unreachable. NOT index inconsistency.
  RULED OUT: the 2026-07-14 red (run `29311393053`) is unrelated — flat 30.00s
  timeouts across `TestFDB_RawIngestBench` / `RawReadScaling` /
  `SaveRecordBatchScaling` / `SaveRecordPerRowScaling`, infra collapse, no such
  error anywhere in the log.
  FIXED SINCE, INCIDENTALLY, by `bde66debe` (2026-07-24, a FAN_OUT-cardinality
  commit that never mentions aggregates): it added
  `candidatePlainFieldColumnsForShortcut`, whose `*ValueIndexScanMatchCandidate`
  type assertion HAPPENS to exclude aggregates. That is luck, not design — see
  CQ-46, which is the real fix.
  DONE — `aggregate_index_shortcut_gate_test.go` LANDED, committed in `c2ad0f445`
  (#524) at `pkg/recordlayer/query/plan/cascades/aggregate_index_shortcut_gate_test.go`
  (`TestAggregateCandidateDeclinedByRawIndexShortcuts`,
  `TestAggregateCandidateToScanPlanIsARawFetchingIndexPlan`). This line read
  "written, currently uncommitted in the main tree" for the whole interval after
  the file was committed — the stale half of a DONE condition nobody re-read.
  This item is the pin, not the fix; **the fix is CQ-46, which stays open.** Note this was the LAST genuine stress run there has been — the 12 nights
  after it were fake-green window skips (fixed by
  `.github/workflows/nightly-reconcile.yml`), so a reproducible planner fault sat
  behind a wall of green check marks for twelve days.

- [x] **CQ-47 (HIGH) — binding-stress 0/50, every seed, un-root-caused.** · under
  investigation
  **Nightly Fuzz binding-stress, runs `30072919663` (07-24) and `30147568953`
  (07-25)** — `binding-stress: 0/50 pass, 50 fail, 0 FDB deaths (1000 ops/seed)`.
  EVERY seed failed with `exit status 1 (FDB=ALIVE, ~5.2s)`, identical timing
  across all 50. A uniform per-seed failure with the cluster ALIVE is a systematic
  harness or binary break, NOT a data-dependent bug — look at what changed in
  `cmd/fdb-binding-stress` / `cmd/fdb-stacktester` or the bindingtester harness
  around 07-23, not at seed-specific behaviour.
  DONE = root-caused, fixed, pinned. Reproduce locally first:
  `bazelisk run //cmd/fdb-binding-stress -- -seeds 1 -seed-start 1`.

  **DONE 2026-08-05.** Root cause: `python_fdb_tar` was built without
  `--dereference`. Bazel stages an external repo's srcs as symlinks into the
  execroot, so the archive stored absolute paths into the builder's own
  output_base instead of file content. It therefore unpacked correctly only on
  the tree that produced it — which is exactly why the lane never reproduced
  locally while failing on every CI runner. Off that tree every member dangles,
  `fdb/` unpacks as a directory with no importable `__init__.py`, and Python does
  not error: it resolves `import fdb` to an empty implicit namespace package. The
  tester then died on the first attribute it touched,
  `AttributeError: module 'fdb' has no attribute 'api_version'`, identically for
  all 50 seeds with the cluster healthy. The 08-04 nightly (job 91913623773)
  printed the tell directly — `Python FDB client: None`, i.e. `fdb.__file__` was
  None — after #583 improved the per-seed reporting.
  The preflight #583 added to catch this class did not: a bare `import fdb`
  succeeds on a namespace package, so the guard was fake. It now rejects a
  `__file__`-less package explicitly and touches `fdb.api_version`.
  Pinned by `TestPinnedPythonClientTarCarriesContentNotSymlinksIntoTheBuildTree`
  (tar carries content, no link members) and
  `TestPreflightRejectsAnEmptyNamespacePackage` (stages the exact broken shape).
  Both go red under independent reverts of their own direction; the pre-existing
  `TestPinnedPythonClientIsImportableNotTheCMakeTemplateTree` stays green on the
  broken tar, since it only asserted entry *names* — that is the dimension that
  was unprobed. Measured after the fix: api 100/100 × 1000 ops, directory
  30/30 × 500 ops, 0 FDB deaths.
  NOT part of this failure, seen once in the same 08-04 job and still unexplained:
  seed 1 died at container start with `unable to apply cgroup configuration …
  Message recipient disconnected from message bus` (FDB=DEAD) — a runner-side
  systemd/dbus fault, not a harness or client defect.

- [x] **CQ-48 (MED) — docs authority reconciliation: five living documents assert
  states the code has left behind.** · M
  Booked as ONE item because they share a single root cause — a document that
  claims authority ("scorecard", "verdict", "still genuinely OPEN") and is then
  never re-derived becomes actively misleading, worse than no document. Each site
  was re-verified 2026-07-29:
  - `rfcs/prod-readiness-go-client.md` — 7 of its 9 punch-list items are closed,
    but the Verdict, the per-section Scorecard and the "Top 3 to close" (`:230-231`:
    cluster-file re-watch, `SetTimeout`→RPC-ctx, the `API_PARITY.md` honest-options
    split) all still assert the gaps as open. The counts quoted in §4 (`:169-176`)
    are all low against the current tree.
  - `PRODUCTION_READINESS.md` — carries an authority claim; last substantive touch
    2026-06-27.
  - `TODO-production.md:725-737` — P2.3 states *"Verified 2026-06-24: all six still
    OPEN in TODO.md"*. At least three are closed: `wrapBareAggregateInsertSource`
    was DELETED by CQ-5 (so `:732-733` describes a function that no longer exists),
    and `:735`'s bare-`GROUP BY` `INSERT…SELECT` guard went with it.
  - the `#28` client item (`commit ships the UNFOLDED mutation log`)
    is still `- [ ]`, but it shipped: `pkg/fdbgo/client/commitpath.go:343` reads
    *"coalesced for a RYW commit (#28)"* and `transaction.go:1778-1780` implements
    the coalescing. (This bullet cited `TODO.md:7222`; the item was at `:7498`. A
    line-number citation into a 13k-line file that nobody re-derives is itself the
    staleness this item is about — CLOSED by checking the box.)
  - the freeze banner (*"⛔ ALL WORK FROZEN — sole priority is
    RFC-173"*, owner directive dated 2026-07-01) is still the first thing a reader
    hits, while the tree is demonstrably running RFC-197 work. A stale freeze
    banner is the most expensive kind of staleness: it tells the next person not to
    start.
  DONE = each of the five re-derived against the current tree and corrected, the
  `#28` checkbox closed, and the freeze banner either removed or re-confirmed with a
  current date. Where a doc's claim is mechanically checkable (item counts, "still
  open" assertions that name a function), add a `pkg/docscheck` assertion rather
  than a fresh hand-written claim — the existing `docs_consistency_test.go` is the
  precedent, and CQ-41's options gate is the same pattern. A doc corrected by hand
  is stale again in a month; a doc with a gate is not.

  **DONE 2026-08-01.** `road-to-prod.md` is now the named authority and was rewritten
  against the tree; `PRODUCTION_READINESS.md` and `rfcs/prod-readiness-go-client.md`
  carry redirect headers with their flatly-false claims corrected in place
  (`FEATURE_MATRIX.md` is not deferred — it exists and is drift-guarded; the client
  RFC's two P0s are closed in code; its `libfdb_c` pin was two patch versions stale);
  `rfcs/198-…`'s status line said "awaiting joint-review ACK" after its review
  completed. The `#28` box is closed, the freeze banner is lifted in place, and
  `TestProductionStatusAuthority` (`pkg/docscheck/docs_consistency_test.go`) is the
  gate this entry asked for: it pins the redirect headers, adds `road-to-prod.md` to
  `livingDocs` so the version anchors cover it, and fails if a doc other than
  `road-to-prod.md` claims to be the authoritative status page.
  **Found while doing it, and booked rather than buried:** CQ-75 and CQ-76 (two live
  measured defects written into checked-item prose), CQ-77 (`frl-pin-bump`, which the
  audit listed as booked and was not booked anywhere) and CQ-78 (RFC-203's
  implementation, commissioned by a merged RFC and owned by no item).

- [x] **CQ-53 (MED/L, M/L, executor-gated, query-engine review gate) — give the
  FlatMap inner binder Java's parent-chained per-alias bindings, and the
  merged-row `leg.col` string channel dies with its five readers.**
  **DONE — closed by CQ-67 (merged as #549), which this entry already named as
  its entire remaining work ("the REMAINING WORK OF THIS ITEM IS NOW CQ-67 …
  CQ-53 closes when CQ-67 closes; it carries no separate remainder").** The last
  genuinely-dotted debt sites are readers of ONE channel, and the channel is
  executor-side. (The title used to say "teach the FlatMap inner binder
  leg-local windows"; that plan is corrected below and the correction is the
  point of this item.)

  The producers rewrite a leg-correlated read into a merged-correlated read with
  the leg packed into the NAME:
  `rule_implement_nested_loop_join.go:2854` (`qualField := corr + "." +
  strings.ToUpper(fv.Field)`) and `cascades_translator.go:3579`
  (`values.NewFieldValue(mergedQOV, leg+"."+strings.ToUpper(fv.Field), fv.Typ)`)
  both turn `QOV(leg).COL` into `QOV(merged)."LEG.COL"`, because the FlatMap
  inner's binder resolves the merged row by that key.

  The structural form ALREADY EXISTS on the other path: `executor.go:2719`
  `concatLegPositionals` stores leg WINDOWS (`RecordType.Legs`) rather than
  dotted names, and `executor.go:2788` `spansFromMergedLegs` binds `QOV(leg).col`
  leg-locally over the merged row for the join-predicate path. So this is not a
  missing capability.

  **The plan below is a CORRECTION of the one first booked here.** "Bind
  leg-locally through `RecordType.Legs`" reads as structural and is not:
  `values.RecordTypeLeg` is `{Name string // UPPER binding of the source; Start;
  Width}` (`pkg/recordlayer/query/plan/cascades/values/type.go:372-376`), so a
  binder that selects a leg by `Legs[i].Name` is still deciding a quantifier's
  identity by text. It would move the string from the FieldValue's `Field` into
  the row type's leg table and change nothing about what can go wrong — the same
  defect this whole item exists to remove, one indirection further away.

  Java does not do this, in either of the two places it could have. Read both
  before starting:

  1. **It never merges for binding at all.** `RecordQueryFlatMapPlan.executePlan`
     (`fdb-record-layer-core/.../query/plan/plans/RecordQueryFlatMapPlan.java:135-140`)
     binds the OUTER result under the outer quantifier's alias
     (`context.withBinding(Bindings.Internal.CORRELATION, outerQuantifier.getAlias(),
     outerResult)`), then binds the INNER result under the inner alias on a
     context CHAINED off that one (`:139-140`). Each quantifier keeps its own
     row under its own alias; nothing is concatenated and nothing is
     re-addressed. `QuantifiedObjectValue.eval`
     (`.../cascades/values/QuantifiedObjectValue.java:84-85`) is then a map
     lookup — `context.getBinding(Bindings.Internal.CORRELATION, alias)` — so a
     leg-correlated read needs no rewriting on the way in.
  2. **Where a merged row IS unavoidable it is UNNAMED and ORDINAL, rewritten
     EAGERLY.** `PartitionSelectRule.java:283-315` collapses ≥2 live lowers into
     `RecordConstructorValue.ofColumns(...)` over `Column::unnamedOf` of each
     leg's `QuantifiedObjectValue` (`:284-291`), then builds a `TranslationMap`
     mapping each collapsed alias to `FieldValue.ofOrdinalNumber(QOV(newQuantifier),
     index)` (`:296-303`) and applies it to the upper predicates and the result
     value at CONSTRUCTION time (`:307-315`). The columns carry no names, so
     there is nothing for a later reader to match; the ordinal is fixed once,
     where the layout is in hand.
  3. **And it REJECTS rather than represent.** Where the lateral correlations
     conflict, Java declines the partitioning outright: more than one lower
     correlated-to by the uppers (`:161-167`), or an upper-correlated lower that
     is not the same one the upper aliases correlate to (`:234-243`).

  So CQ-53's plan is:

  - **Parent-chained per-alias bindings first.** Teach the FlatMap inner binder
    Java's chained `EvaluationContext`: one binding per quantifier alias, inner
    chained off outer. A `QOV(leg).COL` read then resolves without any rewrite,
    and both producers (`rule_implement_nested_loop_join.go:2854`,
    `cascades_translator.go:3579`) delete outright rather than being re-keyed.
    This is the bulk of the work and it removes the channel rather than
    relocating it.
  - **Nested UNNAMED ordinal record + eager translation ONLY where merging is
    unavoidable**, following `PartitionSelectRule.java:283-315`. Go already has
    this port: `positional_merge.go` builds the unnamed positional merge row and
    rebases every upper reference through
    `TranslationMap.When(live_i).Then(ofOrdinalNumber(QOV(merge), i))`, and
    `rule_partition_select.go:616` is the same rule's Case-1 unnamed column
    (`AddColumn("", LiteralValue(1))` — Java's `addResultValue(LiteralValue.ofScalar(1))`,
    `PartitionSelectRule.java:264`). The device exists; the merged-row binder is
    the site that has not been taught it.
  - **Decline where lateral correlations conflict**, per `:161-167` / `:234-243`,
    instead of inventing a representation that can express the conflict.
  - **No leg-name channel.** `RecordTypeLeg.Name` may keep serving diagnostics
    and layout derivation, but it must never be what selects the leg a reference
    binds to. If the implementation finds itself matching on it, the first bullet
    was skipped.

  The load-bearing negative result, now PINNED rather than prose:
  `TestLegRef_DeclinesAMergedRowQualifiedRead`
  (`pkg/relational/core/query/cluster_ref_attribution_test.go`). `legRef`
  (`ordinal_seed.go:759-769`) declines any dotted name BEFORE asking the child,
  and that guard is a mitigation, not defensiveness: delete it and
  `FieldValue{Field:"A.ID", Child:QOV(S)}` reports leg `"S"` — the merged
  correlation returned as a direct leg reference, measured by mutation. So the
  five readers (`ordinal_seed.go:761`,
  `rule_implement_nested_loop_join.go:2332`, `left_outer_existential.go:112`,
  `box_conjunct.go:149`, `accessor_name_path.go:61`) cannot be "cleaned up"
  first. Producer-first, as RFC-197 item 6 requires.

  Executor + NLJ rule, so it takes a Graefe-gated RFC and a review lap of its
  own. Closing it takes the RFC-197 `dotted` bucket from 6 to 1
  (`cascades_generator.go:4122`, the group-key qualification probe, is the only
  non-merged-row reader left).

  PARTIALLY IMPLEMENTED (branch `feat/cq53-flatmap-binder`), and the remainder
  is BLOCKED ON CQ-63 by measurement, not by judgement. What landed:

  - **A runtime binder that keeps every merged leg alias resolvable**
    (`executor.bindMergedOuterLegs`, `flat_map_cursor.go`). LANDED, AND **LIVE
    IN PRODUCTION** — the earlier "NO NON-TEST CONSUMER" wording here was wrong
    and read as dormant. Measured over one full `sqldriver` run: **15,641 leg
    windows bound across 288 distinct shapes**, once per OUTER ROW, on every
    clustered-box gather (`FROM A FULL OUTER JOIN B ON …, A.ARR AS X` and
    friends). The trigger is any merged outer row carrying a leg table; it is
    independent of the dotted mint, which is why
    `TestLazyLegMintReachesNoWinningPlan` measuring zero dotted merged-row keys
    did NOT imply the binder was inactive. Those are different questions and
    only one of them was being asked.

    The binder DOES have a reader, and an earlier version of this entry was
    wrong about it: it said the 3 lookups over a full run were "all for one
    single-leg unnest alias and none on a box-gather shape". Measured with the
    row's leg count stamped by the binder itself, **all 3 are on a MULTI-LEG
    merged row** — the `foldable_colliding_answers` shape in
    `TestFDB_ExistsInnerShadow`, `SELECT OT."K" FROM ST, OT WHERE EXISTS
    (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(1, ST."C") = 1)`. The fold is
    what makes it the reader: the colliding reference never survives into the
    join predicate, so the inner is planned as its own two-source merge. The
    near-identical cousin `colliding_plain` binds the identical windows and
    reads NONE of them, so the shape had to be measured, not guessed.

    That reader is NOT load-bearing: neutering the binder entirely, and
    separately binding every leg to the WRONG window, both leave the whole
    suite green, and the redundancy pin ran that exact query down BOTH
    resolution routes in one process
    (`EvaluationContext.WithMergedLegReadBypass`), asserting the rows agree.
    So the suite's greenness was "the reads are redundant", not "the bindings
    are correct". THE ORDINAL MODEL THEN REMOVED THE READS: a leg reference
    resolves by baked slot, so the binder's windows are built and consulted by
    nobody, the redundancy license is retired, and the standing pin is
    `TestFDB_MergedLegBinding_NothingReadsTheBinder` — which asserts the shape
    still BINDS and is still not read, so the zero is over a shape that ran. The box-gather half is pinned by
    `TestFDB_MergedLegBinding_LiveBoxGatherShape`; the standing numbers are
    reported by `executor.FormatMergedLegBindingCensus` from the sqldriver
    `TestMain`.

    The absence of a load-bearing reader is CAUSAL, so this is an ACTIVATION
    path and not dead weight to delete: the binder's only planner-side producer
    is the leg-local bake (deleted in this same branch by review ruling, see
    the next bullet, and returning with CQ-63), and a channel whose only
    producer is absent cannot have a consumer that depends on it. Scope of the
    claim: the **sqldriver corpus**, the one place the census gate is enabled —
    the census is process-global and self-corrects the moment another harness
    turns the gate on.

    Both facts are enforced, not narrated. `assertMergedLegBindingCensus` in
    the sqldriver `TestMain` floors total reads at ≥1 (a dead read counter
    would otherwise manufacture the headline "0 READ" beside a full bind tally)
    and asserts the activation criterion: *a read on a MULTI-LEG merged row,
    outside a reader shape PROVEN redundant in this run, implies
    `LegLocalBakeCensus.Baked > 0`*. The one exclusion is a registration the
    redundancy pin makes on its passing path only, so a divergence between the
    two routes fails the pin, empties the registry, and turns those same reads
    into a red gate in the same run.

    **Criterion amended on post-ACK evidence. RULED SOUND, AND INERT — the
    standing question is CLOSED and its premise was stale.** The double-ACK'd
    form was a bare *merged-shape read ⇒ Baked > 0*, resting on the "3
    single-leg-unnest reads" claim above. That claim is refuted. What this entry
    then said — that the bare form is "unsatisfiable on this tree (3 multi-leg
    reads with Baked = 0 by construction)" — is ALSO wrong, and in the opposite
    direction: `Baked` counts the PASS-THROUGH, which is 174 of 174, so the bare
    implication is not unsatisfiable but TRIVIALLY SATISFIED. Its antecedent
    holds and its consequent holds, always, for reasons that have nothing to do
    with each other.

    The amended form (proven-redundant exclusion) was reviewed and is sound as
    machinery: the exclusion is keyed on full read identity, the registration
    happens only on the pin's passing path so a divergence empties the registry,
    and `partitionMergedRowReads` is pure. But sound is not the same as useful.
    The criterion can only fire if the PASS-THROUGH goes silent while merged-row
    reads persist, which is a state no current shape produces. It is a gate
    guarding a door nobody walks through.

    **The gap that matters is elsewhere, and it has no instrument at all.** The
    load-bearing claim on this path is "the bindings are not load-bearing" — and
    what supports it is a HAND-RUN mutation: bind every leg to the wrong window,
    observe the suite stays green. Nothing in CI did that. The redundancy pin ran
    the reader shape down both routes via
    `EvaluationContext.WithMergedLegReadBypass` and compared rows, which proves
    the two ROUTES agree; it did not bind a wrong window, so it could not notice
    the day a binding started to matter. THAT GAP IS CLOSED:
    `TestFDB_MergedLegBinding_WrongWindowsAreUnobservable` stands the wrong-window
    mutation as a test — every window rotated onto a SIBLING leg's span, rows
    asserted unchanged, with an engagement floor so it cannot go green by ceasing
    to reach the binder. The redundancy pin beside it is now
    `TestFDB_MergedLegBinding_NothingReadsTheBinder`, since under the ordinal
    model there is no read to route twice.

    Consequence for CQ-53's remainder: the shadowing and first-claim-wins
    semantics are, on the shapes that run, UNOBSERVABLE — and the
    join's-own-alias skip never engages there at all (the box gathers under a
    minted `B$BOX` alias, which is never a leg alias). Their justification is
    consistency with the leg table's other readers, not observed behaviour. See
    DIVERGENCES.md.

    The per-outer-row allocation storm this path carried has been removed
    (13→5 allocations on the dominant shape, 49→11 on a 6-leg one, ~3x wall
    clock), so a reader-less path on the row-rate path now costs close to
    nothing while its retirement is settled.

    It is ALSO a divergence, not the port the earlier wording claimed. Java's
    `RecordQueryFlatMapPlan.java:135-140` binds the chained outer→inner PAIR,
    two correlations, never a set of siblings — and where Java's planner
    collapses several sources into one quantifier it re-anchors sibling
    references **by ordinal** (`PartitionSelectRule.java:296-303`:
    `when(lowerAlias).then(FieldValue.ofOrdinalNumber(QOV(newUpper), index))`),
    which DELETES the sibling alias rather than keeping it bound. Keeping N
    sibling aliases live at runtime is a Go-only widening of the binding
    namespace, booked in DIVERGENCES.md with its retirement condition (the
    ordinal re-anchor covering every shape that reaches this cursor).
    Precedence — first-claim wins among legs, a leg shadows an enclosing
    binding without destroying it — is defined and pinned there too, because
    the widening is what forces Go to have an answer Java never needs.
  - **The leg-local bake is DELETED, not landed.** It minted the leg-local
    ordinal by resolving the reference's DISPLAY NAME against the leg's row
    type (`legType.FieldIndex(ToUpper(fv.Field))`) — a name deciding a column's
    identity, RFC-197's forbidden move, in the `.Field` gate's own documented
    blind spot (a type lookup by name is neither a comparison nor a map key).
    Every leg-correlated read falls through to the qualified mint; the
    identity-keyed `legSlotIdentity`→`legLayout` lookup is the only ordinal
    authority in that function. The bake returns with CQ-63, keyed on identity.
  - **The layout derivation is rerouted onto the FAITHFUL instrument** —
    `expressions.Quantifier.GetFlowedObjectType` (Java's
    `Quantifier.java:806-810`, what `translateCorrelations` rebases against),
    with the bespoke physical-plan walk kept as a documented SUBORDINATE and
    counted. `physicalLegRowTypes` is gone; `legRowTypesFromQuantifiers`
    replaces it.

  THREE PREMISES OF THIS ENTRY ARE REFUTED, all by reading the two sides:

  1. "Both producers DELETE OUTRIGHT once the binder is parent-chained." They
     cannot, and "parent-chained" mis-states what Java's binder is. Java's
     FlatMap chains exactly the outer→inner PAIR
     (`RecordQueryFlatMapPlan.java:135-140` over `Bindings.java:116-134`); a
     SIBLING leg is not kept bound at all, it is re-anchored by ORDINAL against
     the merged quantifier and its alias ceases to exist
     (`PartitionSelectRule.java:296-303`'s `translateCorrelations` entries).
     So there is no Java binder shape that, once ported, makes the name
     deletable — the deletion Java gets comes from the ordinal rewrite, and
     that rewrite needs a stated row type. Measured at the mint itself over the
     real-FDB corpus: **126 firings, 0 with a typed leg quantifier**. The
     reference carries an UNTYPED QOV, so there is no domain to state the
     ordinal in.
  2. "Port PartitionSelectRule's REJECTION of conflicting lateral correlations."
     Already ported: `rule_partition_select.go:395` is Java's `:161-167`, and
     `:548-555` is Java's `:234-243`.
  3. "The nested UNNAMED ordinal record with eager TranslationMap rewriting is
     the thing to build." Already built and Java-faithful:
     `positional_merge.go`'s `positionalMergeCase` is
     `PartitionSelectRule.java:283-315` 1:1 (`OrdinalFieldName(i)` for
     `Column::unnamedOf`, `.When(live_i).Then(ofOrdinalNumber(QOV(merge), i))`).

  WHERE IT STOPS, MEASURED ON THE FAITHFUL INSTRUMENT. The whole census is
  routed through `Quantifier.GetFlowedObjectType` now; the earlier numbers came
  from the bespoke plan walk and one of them was simply wrong. Real-FDB corpus:

  ```
  total 126 (baked 0, minted 126)
  minted residue: untypedLeg 92, columnAbsent 0, layoutAvailable 34
  legs (846 derivations): flowed 656, walkOnly 108, underivable 82, memberDisagreement 0
  ```

  Read as FIRINGS vs DISTINCT WITNESSES, which are different facts: 126 read
  firings over 19 distinct witnesses (14 `UNTYPED-LEG`, 5
  `LAYOUT-AVAILABLE-BUT-MINTED`); 82 underivable-leg firings over just **3**
  distinct witnesses, every one a `*plans.RecordQueryFlatMapPlan` whose result
  value is a bare untyped `*values.QuantifiedObjectValue`. The residue is
  narrow and it is CQ-63 verbatim.

  **The 32 in the previous revision of this entry was wrong.** Re-measured at
  branch HEAD on the unmodified instrument it is **122**, and on the faithful
  instrument **82** — the quantifier's flowed type closes 40 of the walk's 122
  declines. 82 is CQ-63's acceptance baseline.

  Neither derivation subsumes the other, which is why the walk stays as a
  documented subordinate: it answers for 108 legs the quantifier cannot, and
  those are not exotic — plain `RecordQueryScanPlan` and
  `RecordQueryPredicatesFilterPlan` legs whose LOGICAL reference members carry
  an untyped result value while the physical scan is typed. That is CQ-63 seen
  from the other side.

  Deriving the layout from a SEEDED result value was implemented and removed.
  Its decline is pinned
  (`TestOrdinalSeedLegWindows_DeclinesANoLayoutFlatMapLeg`) — and the pin
  records something sharper than a decline, see the hazard below.

  So the ORDER changes: **CQ-63 now gates the rest of CQ-53**, not the reverse.
  Until a FlatMap leg can state its row type, 92 of 126 leg-correlated reads
  have no honest alternative to the qualified name, and therefore
  `RecordTypeLeg.Name` cannot retire and the seed-window text keys cannot
  either. The seed-window keys have a second, independent blocker of their own:
  `exists_gathered_cluster_wrap.go:131` and `unnest_gather.go:365` SPLIT a
  dotted reference and look the window up by the qualifier TEXT, so identity
  keying there requires minting an identifier from a name — the exact forgery
  RFC-197 exists to remove. Those producers are SQL-translator-side dotted
  names, a different channel from the NLJ mint, and they retire on their own
  producer-first path.

  **THAT SECOND BLOCKER IS REFUTED, and it was never live.** Both sites were
  instrumented per lookup and the whole real-FDB corpus driven through them:
  each is reached ZERO times. Not quiet — UNREACHABLE, confirmed by a panic
  wired into each and hit by nothing in `./pkg/relational/...` (the sqldriver
  FDB corpus plus the explaindiff, plandiff, rowdiff, memoinvariant and yamsql
  harnesses) nor `./pkg/recordlayer/query/...`. The arms above them carry every
  reference that reaches those walks, because they are the ones with a leg to
  key on. The seed-window namespace had no text-only reader to serve, and the
  measurement is what said so; the argument above could not have.

  Stress 1M before/after (`bdf70fb2f` vs branch): all 22 measurements
  row-identical; timings within run-to-run noise on a shared machine.

  ---

  **PHASE 2 STATE (measured at branch HEAD, whole real-FDB sqldriver corpus).**
  Two of the three numbers this entry was written around have moved, and one of
  them inverted, so the entry's own premises are restated here rather than left
  to be re-derived from prose above.

  WHAT LANDED:

  - The resolver's correlated quantifier-object mints CARRY the source's
    declared row, as Java's `Quantifier.java:801-803` always has. The flowed-type
    decision is ONE authority (`expr.flowedTypeFor`) reached by all three mints —
    `ResolveIdentifier`'s correlated arm, `ResolveColumnShadowingQualified` and
    `ResolveQualifiedProjection` — because they are reached by SQL that differs
    by one FROM item and a per-site decision was made differently at two of them.
  - `rebaseOuterLegValue`'s NAME-KEYED bake is deleted, permanently. It minted a
    leg-local ordinal by resolving a display name against the leg's row type,
    RFC-197's forbidden move. Nothing replaced it because nothing needs to: the
    reference ARRIVES carrying its ordinal.
  - The FlatMap leg binding's datum unwrap is keyed on the VALUE's own result
    type, uniformly on both sides, as `QuantifiedObjectValue.java:82-95` /
    `RecordQueryFlatMapPlan.java:135,:140` are. The `legRole` side-flag is gone.
  - `mergedOuterLegAliases` emits leg IDENTITIES only; the parallel source-alias
    TEXT half is deleted (measured: corpus, embedded suite and rowdiff goldens
    all green without it).

  THE NUMBERS, and what changed about each:

  These are point measurements over the real-FDB sqldriver corpus, and the
  corpus population moves whenever a query is added anywhere in that suite —
  three successive writings of this block were stale within the same change.
  Re-measure rather than trusting the digits:
  `go test ./pkg/relational/sqldriver/ -count=1 -v | grep "leg-local bakeability"`.
  What is durable is the SHAPE — every residue at zero — not the totals.

  - `underivableLegs` **0 of 960** (was 82 of 846). CQ-63's stated gate is
    CLOSED — every leg states its row.
  - leg-local bake census: **total 174, baked 174, mergedReAnchor 0, declined 0**;
    ALL reads by own identity: **identityInLegDomain 174, otherDomain 0,
    lazyNameOnly 0** (was: 126 reads, all re-anchored, identityInLegDomain **0**).
    So "92 of 126 leg-correlated reads have no honest alternative to the
    qualified name", above, is REFUTED: it is 0 of 174. The identity cut is now
    CROSS-CHECKED against the outcome cut (`IdentityInLegDomain == Baked +
    MergedReAnchor`, `IdentityOtherDomain + LazyNameOnly == Declined`), because
    both cuts partition the same denominator and neither constrains the other:
    filing a constant identity class at one census call site inverted this
    number end to end with every partition still exact and no gate red.
  - leg-column provenance: **calls 52, dotted hits 4, all identity-available,
    0 unstated, 0 diverged.** The executor's last dotted reader answers four
    times in the whole corpus.

  WHAT REMAINS, and it is NOT what this entry predicted:

  - The blocker is no longer a missing type. It is that Go's two-level
    NLJ→FlatMap lowering reaches `rebaseOuterLegValue` **without a stated merged
    layout** on the EXISTS-over-join and RFC-153 buried-leg paths, so there is no
    merged ordinal to re-anchor to and the pass-through is the honest answer.
    `mergedReAnchor 0 of 174` measures exactly that. Closing it means giving
    those two paths a merged layout, which is the "parent-chained per-alias
    bindings" bullet above — still the right plan, now unblocked.
  - **The seed-window text keys are GONE** (phase 2c). `OrdinalSeedLegWindows`
    returns `map[CorrelationIdentifier]OrdinalSeedLegWindow`; the upper-folded
    namespace does not exist. Measured first, per lookup, over the whole
    real-FDB corpus at every keyed reader of that map:

    ```
    existentialRebase        calls 962 | identityAgreesHit 461 | identityAgreesMiss 501
    existentialDeclineProbe  calls 0
    boxLegRef                calls  92 | identityAgreesHit  62 | identityAgreesMiss  30
    boxDottedSplit           calls 0
    boxSurvivorQOV           calls 184 | identityAgreesMiss 184
    boxSurvivorCorrelation   calls   2 | identityAgreesMiss   2
    gatheredGroupSlot        calls 160 | identityAgreesHit 160
    ```

    That table is a DATED POINT MEASUREMENT and the census behind it retired
    with the text namespace it measured — its classes were all about text
    versus identity, and there is no text side left. What replaced it is a
    NARROWED STANDING instrument, `seed_window_reader_census.go`, wired into
    the sqldriver `TestMain`: per read it records only hit/miss plus the two
    DECLINE classes, floors each of the five surviving readers, and hard-zeros
    the declines. It reproduces the populations above exactly:

    ```
    existentialRebase        reads 962 | hit 461 | miss 501
    boxLegRef                reads  92 | hit  62 | miss  30
    boxSurvivorQOV           reads 184 | miss 184
    boxSurvivorCorrelation   reads   2 | miss   2
    gatheredGroupSlot        reads 160 | hit 160
    ```

    The floors are what the deletion took away: every claim here has the shape
    "this class is EMPTY", and an unreached site prints that identically to a
    site measured clean, so a change that silenced a reader would read GREEN.
    Mutation-checked — deleting the `gatheredGroupSlot` recorder reds the
    corpus run with `gatheredGroupSlot reached 0 reads, want >= 16`. The two
    hard zeros are `QUALIFIED-NO-IDENTITY` (a group-by reference stating a
    qualifier with no correlation) and `CHILDLESS-BAKED` (a source-relative
    baked read with no child reaching the box rebase's tail); each failure
    message names what a non-zero re-arms.

    1400 lookups; every blocking class EMPTY (no TEXT-ONLY-HIT, no
    IDENTITY-ONLY-HIT, no DIVERGED). The two text-only sites are the zeros, and
    the panic probe above says they are unreachable rather than quiet — so the
    box wrap's dotted arm was DELETED (a childless read now DECLINES the wrap to
    the name model, which is stricter than the pass-through it replaces: the
    predicate path had nothing downstream looking for a surviving lazy read),
    and the decline probe's `NamedCorrelationIdentifier(key)` mint died with the
    key. **That "stricter" claim was FALSE for one sub-case when first written
    and is now true.** The decline was gated on `Resolved == nil`, which holds
    for a LAZY childless read and not for a SOURCE-RELATIVE BAKED one — and the
    walk admits the latter deliberately, so it can be rebased. A childless
    source-relative bake therefore got neither: no rebase (no correlation to
    select a window with) and no decline, so a LEG-relative ordinal shipped
    against the BOX row — a different column, not a fall-back. The gate is now on
    childless-ness, matching `wrapRVFullyBaked`, which had the right predicate
    all along; the asymmetry between the two was the defect. Pinned by
    `TestRebaseLegRefsToBox_ChildlessSourceRelativeBakeDeclines`, whose fixture
    puts the leg at merged offset 2 so the leg-relative and merged answers are
    different numbers. What the conversion buys beyond tidiness is measured by
    `TestOrdinalSeedLegWindows_CaseDisjointLegsAreTwoWindows`: an upper-folded
    key namespace collapses a machine-minted `q$5` and a quoted `Q$5` into one
    key, and the seed then DECLINES entirely — a two-leg join losing its ordinal
    layout because two unrelated quantifiers were spelled alike.
  - **`RecordTypeLeg.Name` does NOT retire, and the surviving consumer is
    named and measured.** Its two families are down to one: the DOTTED-TEXT
    consumers, which are live. `executor.ordinal_join`'s dotted arm answers 4
    times over the corpus (`C.CV`, `I.QTY`, `O.ID`, all over legs that also
    state an identity), and the translator's THREE dotted bakers take 110 match
    attempts with 101 matches — measured by a new census, the translator twin of
    the executor's leg-column provenance census. Three, not two:
    `bakeDottedRefsToLegQOV` has a MULTI-ForEach arm and a SINGLE-ForEach arm
    making the same decision on the same counterparty, and only the first was
    counted because the census was built around a map read while the second
    compares one layout's key directly. The third is now instrumented and
    measured **UNREACHED** — 0 over the corpus, with a panic at its match point
    hit by nothing across `./pkg/relational/core/...` nor the explaindiff and
    plandiff harnesses — so it is left standing, deliberately UNFLOORED, with
    both hard zeros gating it. Same class as the two arms already deleted from
    the box wrap; a candidate for the same treatment, not taken here because
    removing it is a behaviour change this census has only just begun watching.
    None of the three readers can be re-keyed
    and the reason is structural, not a shortfall: each is guarded on
    `Child != nil || Resolved != nil → bail`, so it only ever sees a lazy
    carrier minted from parsed text. One of them is worse than that — its layout
    map registers each leg under the scan TABLE name as well as the alias
    (`FROM PA AS "s"` answering `PA."ID"`, measured: 1 such match), and a table
    is not any quantifier's identity, so that map cannot be re-keyed even in
    principle. The successor step is **CQ-52**, which gives the counterparty the
    parser's qualifier/leaf segments instead of a joined string. Not a new item:
    CQ-52 is already booked and already names these sites.
  - `bindMergedOuterLegs` now HAS a live planner-side producer (the
    pass-through, 174 of 174), so DIVERGENCES.md's "a channel whose only producer
    is absent" framing is retired there. The reader is still not load-bearing,
    but for a weaker reason than before: the candidate carrying it loses on every
    covered shape, rather than there being nothing to carry. Measured directly at
    the winner: making arm 2 panic proves it fires during planning of the
    `TestLazyLegMintReachesNoWinningPlan` shapes, while RENAMING its output
    leaves every qualifying read in every winning plan unchanged — so the arm's
    product is built and then loses, and the positive sentinel there is watching
    reads that reached the winner by other routes.

  Stress 1M phase-2 before/after (`457c18ba8` base vs branch): 23/23 subtests
  PASS on both sides, every row count identical (`1`, `8`, `100017`, `47271`,
  `10`, `1000000`, `46`, `97`, …); timings within run-to-run noise on a shared
  machine (full scan 3.51s vs 3.38s, sparse filter 2.92s vs 2.97s).

  ---

  **PHASE 3 (2026-07-31, branch `feat/cq53-parent-chained-binder`, base
  `f50cee43e`). The NLJ mint is DELETED. The parent-chained binder work is NOT
  done, and THREE MORE PREMISES OF THIS ENTRY ARE REFUTED BY MEASUREMENT** —
  including the two the phase-3 plan was built on. Read the refutations before
  planning the next attempt; the plan above targets the wrong sites.

  Every number below is one run of the whole real-FDB sqldriver corpus at base
  `f50cee43e` (`go test ./pkg/relational/sqldriver/ -count=1 -v`), the standing
  censuses plus a call-site probe added for this question and removed after.

  WHAT LANDED:

  - **`rule_implement_nested_loop_join.go:2854`'s dotted mint is GONE.**
    `qualField := corr + "." + strings.ToUpper(fv.Field)` is deleted; the merged
    re-anchor (ARM 1) now names its node `values.OrdinalFieldName(ord)`. That is
    Java, precisely: `PartitionSelectRule.java:296-303` re-anchors a collapsed
    alias through `FieldValue.ofOrdinalNumber`, and `FieldValue.java:335-338`
    builds it from `new Accessor(null, ordinalNumber)` — the accessor's name is
    NULL, because the sibling alias has ceased to exist and there is nothing
    left to spell. Pinned by `TestRebaseOuterLegValue_OrdinalFirst`, which now
    asserts EQUALITY against the ordinal rendering (mutation-checked: restoring
    the mint reds it with `got "A.A_ID", want "_3"`).
  - **The RFC-197 ratchet gained a PRODUCER arm, and it was blind to producers
    entirely.** Measured, not inferred: with the mint above restored verbatim,
    `pkg/docscheck` stayed GREEN. `corr + "." + fv.Field` is a `+` BinaryExpr,
    and every arm of `scanFieldDecisions` watched a name being READ —
    comparisons, switch tags, map keys, composite-literal keys, escapes. RFC-197
    orders this migration PRODUCER-FIRST and nothing was counting producers.
    The new arm flattens the `+` chain (left-nesting hides the separator from an
    in-place operand test — the first version of the arm reported nothing on the
    whole tree and read as clean) and reports one site per line.

  THE RATCHET COUNTS WENT **UP**, and that is the honest result. `dotted` 9 → 14,
  `contract` 15 → 16, total 46 → 52. One mint died and five became countable:

  | site | what it mints |
  |---|---|
  | `cascades_translator.go:3598` | CQ-53's SURVIVING producer — `QOV(leg).COL` → `QOV(merged)."LEG.COL"` on the unnest-merge path |
  | `cascades_translator.go:886` | registers the QUALIFIED spelling of a group key as an alias of its output ordinal |
  | `cascades_translator.go:978` | the READ side of that same alias table |
  | `cascades_generator.go:3073` | `CORR.FIELD` for the null-born nullability upgrade. **RETIRED**, and this row's stated MECHANISM was wrong even when it was live — kept visible rather than deleted with the entry, because the correction lived only on the debt entry that retiring the site removed. The composed string was never "the key into the null-supplying-window metadata map": that map is keyed by protoreflect `FullName`, and the string went to `descriptorForColumn`, which matches by BARE name across the join leaves and uses the qualifier only to tie-break against the DESCRIPTOR's name — a table, never a correlation — so it could not separate legs at all. Converted to structural leg addressing (leg plan + leg-relative ordinal), which is how Java identifies the reference (`ResolvedAccessor` compares `getOrdinal()` alone) |
  | `logical_predicate.go:9340` | the correlated-scalar column key, qualified or bare depending on a scoping test made elsewhere |
  | `values.go:1720` (`contract`) | `FieldPath.toString` rendering — debt through `ColumnNameValue`, not through `ExplainValue` beside it |

  **WHAT THE MINT ARM COUNTS, AND WHAT IT CANNOT — read the six above with this
  bound or the tally lies.** The arm inherits the whole gate's scope: it fires on
  a `.` joined to `FieldValue.Field` or to an identifier tainted from it. Three
  consequences, and the first is the one that matters:

  - **The two producers this phase MEASURED as the live executor-side channel are
    OUTSIDE the count.** `scalar_subquery_seed.go:83` joins a plain `scalarCol`
    string parameter; `clustered_outer_scalar.go:493` joins
    `leg.typ.Fields[i].Name`, a `.Name` selector off a schema field. Neither
    operand is a `FieldValue`, so neither is a decision the gate can see. The
    same holds for their siblings at `clustered_outer_scalar.go:510`, `:774` and
    `cascades_translator.go:7036`. `dotted (14)` therefore means "fourteen
    `.Field`-scoped dotted sites", NOT "every dotted producer in the tree".
  - **One shape is structurally undetectable rather than merely out of scope.**
    `values.go:1713` accumulates path steps into a SLICE (`steps[i] = …`) and
    joins them with `strings.Join`, so there is no `+ "." +` node to match and
    the taint tracker cannot follow it either — `taint()` requires an
    `*ast.Ident` on the left of an assignment and `steps[i]` is an `IndexExpr`.
    Its sibling at `:1720` IS counted, because that one concatenates. A
    heuristic keyed on `strings.Join`'s separator argument would fire on every
    path-joining helper in the tree; the bound is stated instead of guessed at.
  - Widening to `.Name` selectors is the obvious next step and it is NOT free:
    `.Name` is among the most common field spellings in the tree (schema fields,
    protobuf descriptors, quantifier bindings), so the arm would need the type
    discriminator the gate deliberately does not use on its shallow tier. That
    trade is documented at `scanFieldDecisions`' `decides` closure and is a
    change to the gate's precision policy, not an extension of this arm.

  THE REFUTATIONS. Each quotes the premise it kills:

  1. **"The `:2854` producer deleted ⇒ leg-column provenance 4 dotted hits → 0."**
     A NON-SEQUITUR: the two are different channels. `:2854`'s only consumer is
     ARM 1, and ARM 1 returned ZERO times over the corpus (`mergedReAnchor 0 of
     174`), so the mint produced NOTHING that reached the executor. The
     executor's four dotted hits come from the CORRELATED-SCALAR ORDINAL SEED
     builders, which name their RC fields `LEG.COL` literally:
     `scalar_subquery_seed.go:83` and `clustered_outer_scalar.go:493`. Measured
     by instrumenting both producers and matching their output against the
     census witnesses — `C.CV` and `O.ID` are minted VERBATIM by
     `scalarSubqueryOrdinalSeed`; `I.QTY` arrives as the `scalarCol` half of its
     `O.I.QTY`. Those leg-type names are what `adaptLegPositional` feeds to
     `rowSlotForLegColumn`. The executor's dotted arm therefore retires with the
     SCALAR-SEED representation, producer-first, and not with any NLJ work.
     Deleting `:2854` moved the provenance census by zero, as it had to.
  2. **"Go reaches `rebaseOuterLegValue` without a stated merged layout on the
     EXISTS-over-join AND RFC-153 buried-leg paths."** Half wrong, and the
     wrong half is the one the plan named first. Split by call site:

     ```
     EXISTS(implementJoinWithExistential)  174   <- ALL of the 174
     BURIED (buildCorrelatedFlatMapPlan)     0
     ```

     The RFC-153 buried path DOES state a layout — `buriedLegOrdinalLayout`
     answered on 314 of 362 firings (the 48 declines are a NLJ/Projection/Union
     outer, not a FlatMap) — and it contributes NOTHING to the 174 because no
     leg-correlated leaf reaches the match arm there at all. There is no buried
     -leg work in this item.
  3. **"Closing it means giving those two paths a merged layout, which is the
     parent-chained per-alias bindings bullet — still the right plan, now
     unblocked."** It is NOT unblocked, and the blocker is not the one booked.
     On the EXISTS path the merged layout IS `ordinalSeedLegWindowsOf(step1RV)`;
     it is nil exactly when `foldStep1Seed` declined. Decline census, per firing:

     ```
     DECLINE correlatedStep1                                    108
     DECLINE reconstruct nil (left=FlatMap right=Scan)           77
     DECLINE reconstruct nil (left=Scan  right=FlatMap)          77
     DECLINE rv does not reference exist alias                  200
     ACCEPT                                                      78
     ```

     and the step-1 result values that reach the pass-through are:

     ```
     RC(1) [ExistsValue:1]                     x26
     RC(2) [ExistsValue:1 FV-multi:1]          x48
     RC(2) [ExistsValue:1 FV-unpinned:1]       x80
     ```

     So the residue is a FOLDED PROJECTION CARRYING AN ExistsValue, not a leg
     concat — there is nothing for `OrdinalSeedLegWindows` to read because the
     value is not a merged row at all. The 154 `reconstruct nil` firings are
     `legIsOrdinalSafe` declining a **FlatMap leg**, and what those FlatMap legs
     flow is the sharp finding:

     ```
     rv=*values.QuantifiedObjectValue        x94   (identity pass-through)
     rv=RC(2) [QOV-record:2]                 x60   (a nested UNNAMED merge)
     ```

     **The 60 are Java's own merged row.** `RC(_i: QOV(leg_i))` is
     `PartitionSelectRule.java:283-291`'s
     `RecordConstructorValue.ofColumns(... Column::unnamedOf)` verbatim — Go
     builds it in `positional_merge.go`'s `positionalMergeCase`. The layout
     authority cannot read it: `OrdinalSeedLegWindows` accepts only the FLAT
     concat seed (every field a frontier-pinned single-accessor `FieldValue`
     over a leg QOV), and a bare whole-leg QOV field is rejected unless it is a
     non-record mixed-seed element. So Go already PRODUCES Java's merge shape on
     this path and its own layout authority declines it.

  WHAT THE NEXT ATTEMPT IS, stated so it is not re-derived: teach the layout
  authority to read the NESTED UNNAMED merge row (`RC(_i: QOV(leg_i))`) as a leg
  layout, then let `legIsOrdinalSafe`/`planBuriedLegConcat` admit such a leg so
  `reconstructFoldStep1Seed` can build the step-1 seed and the ordinal rebase
  (`rebaseOuterLegRefsOrdinal`) takes over from the pass-through. That is a
  change to the PHYSICAL ROW LAYOUT CONTRACT between planner and executor
  (nested-per-leg vs flat concat), so the executor's span twin has to agree
  bit-for-bit and the cross-agreement fixture is the gate. It is NOT started
  here: it needs the Graefe-ACK'd RFC that comes BEFORE implementation, and the
  `correlatedStep1` arm above it carries a documented history of two reverts
  (a correlated FlatMap binds legs by NAME; a baked seed there hits the loud
  `BakedNameContextError`).

  NOT DONE, and each is a consequence of the refutations rather than a choice:
  the executor's `rowSlotForLegColumn` dotted arm SURVIVES (its producer is the
  scalar seed, not the NLJ mint); `bakeDottedRefsToLegQOV`'s family survives
  (re-measured this run: `flatColumnBake` 102 attempts / 98 matches,
  `legQOVBake` 4 / 3 + 1 via table name — CQ-52 is still its successor step);
  `mergedReAnchor` is still 0 of 174, so the redundancy exclusion is still
  vacuous and the wrong-window mutation is still GREEN — that mutation is no
  longer somebody's by-hand edit, it STANDS as
  `TestFDB_MergedLegBinding_WrongWindowsAreUnobservable`; and `RecordTypeLeg.Name`
  does NOT retire — its contract at `values/type.go:372` is unchanged, because
  the reader the phase-3 plan expected to kill was never fed by the mint it
  deleted.

  Baseline censuses at `f50cee43e`, for the next attempt to diff against:

  ```
  leg-local bakeability: total 174 (baked 174, mergedReAnchor 0, declined 0);
    legs: flowed 960, underivable 0; identityInLegDomain 174, otherDomain 0, lazyNameOnly 0
  merged-leg binding: 15677 windows bound over 299 shapes; 6 READ (all excused as proven-redundant)
    [now 15689 / 303 / 12 — the wrong-window instrument runs the reader shape
     twice more and binds one execution's windows rotated, so +6 reads and
     +4 bind shapes are ITS doing, not a corpus change]
  leg-column provenance: calls 52 (flatHit 40, notDotted 8); dotted HITS available 4, unstated 0, diverged 0
  translator dotted leg qualifier: flatColumnBake 102 (98 match), legQOVBake 4 (3 match, 1 via table name)
  seed-window readers: existentialRebase 962 (461/501), boxLegRef 92 (62/30),
    boxSurvivorQOV 184, boxSurvivorCorrelation 2, gatheredGroupSlot 160
  ```

  **Stress 1M phase-3 before/after (`f50cee43e` base vs branch `4dccc50f0`),
  2026-07-31.** 24/24 subtests PASS on both sides; every measured line identical
  once timings are normalised (37 lines); row-count multiset identical
  (`1`×9, `4`×3, `8`×3, `10`, `46`, `97`×2, `47271`, `100000`, `100017`,
  `1000000`×4). Timings within run-to-run noise, all thresholds met:

  | | base | branch |
  |---|---|---|
  | PK lookup id=0 / N-2 / N-1 | 5.13 / 5.09 / 4.86 ms | 4.86 / 4.87 / 4.84 ms |
  | idx_customer eq | 5.66 ms | 5.71 ms |
  | ORDER BY PK (full, 1M) | 3.33 s | 3.25 s |
  | scan all rows wide (1M) | 3.38 s | 3.32 s |
  | full scan sparse filter | 2.98 s | 2.91 s |

  **METHODOLOGY BUG FOUND WHILE RUNNING IT, and it invalidates the workflow
  CLAUDE.md documents.** The first comparison showed the branch 2.3x SLOWER on
  every point lookup — PK lookups 11.5–17.4 ms against the base's 4.9–6.6 ms,
  `idx_customer eq` 13.1 ms against 5.7 ms — reproduced on three consecutive
  branch runs and therefore not noise. It was not the change either: the two
  sides differed in CHECKOUT LOCATION, not only in commit. The base worktree sat
  on the root disk and the branch tree in `/home`, which was **96–100% full**,
  and ext4 point-lookup latency degrades sharply at that utilisation. Controlled
  by checking the SAME branch HEAD out to a second worktree on the root disk
  (source verified byte-identical): 4.86 / 4.87 / 4.84 ms — base parity. The
  table above is that same-disk, same-load pair.

  The documented recipe (`git worktree add /tmp/fdb-master master`, compare
  against the branch in `~/projects/...`) puts the two sides on DIFFERENT
  FILESYSTEMS by construction, so any run of it while either disk is near full
  measures the disks and reports it as a planner regression. Both sides must be
  on the same filesystem. Note also that `/tmp` here is a 32 G tmpfs, too small
  for a worktree — use a root-disk path.

  Nothing on an executed path changed, which is why parity was the expected
  result: the only production edit is ARM 1's display NAME, on an arm measured
  to return zero times over the whole corpus. If a future change makes ARM 1
  fire, that change owns its own stress run.

  **Left open by phase 3. The first is now CLOSED and its entry is kept as the
  record of what was built; the second is still open and is stated so it is not
  mistaken for done:**

  1. **CLOSED — the standing WRONG-WINDOW instrument is
     `TestFDB_MergedLegBinding_WrongWindowsAreUnobservable`**
     (`pkg/relational/sqldriver/merged_leg_wrong_window_fdb_test.go`). What it
     gates: the claim "the merged-leg bindings are not load-bearing on any
     covered shape", which until now rested half on a mutation somebody ran by
     hand and nothing in CI performed. It runs the corpus's one merged-row reader
     shape twice in one process — correct windows, then every window rotated onto
     a SIBLING leg's span via `EvaluationContext.WithMergedLegWrongWindows` — and
     asserts the rows agree and are Java's. It reds the day a read starts
     depending on which slots its window covers, and its message names the
     procedure (exclusion out of the census criterion, binder correctness becomes
     a real invariant, DIVERGENCES.md's shadowing/first-claim-wins semantics need
     re-justifying).

     Rotation, not the by-hand "every window at offset 0": leg 0 already starts at
     0, so the constant left `ST` — the leg the corpus's reader actually reads —
     aimed CORRECTLY. The by-hand mutation was weaker than its own description.

     Two floors stop it passing vacuously — the shape still reads a binder window
     (one shape, >0 reads), and the read RESOLVED THROUGH a misaimed window (>0
     under the hook, exactly 0 without it). Windows-bound-wrong was rejected as
     the floor: a merged row nothing looks up can be misaimed all day.

     Mutation-checked in three directions, each red on its own: hook neutered →
     `THE WRONG-WINDOW ARM NEVER ENGAGED … reads through a MISAIMED window: map[]`
     with the reads floor still satisfied at 3 (this is the discrimination
     check — the reads are not a side effect of the hook); the misaimed FLAG set
     without moving the span → the executor-side pin
     `TestMisaimMergedLegWindows_ServesTheSiblingsSlots` reds with
     `leg A ordinal 0 read 10, want 22`; the binder binding nothing → floor 1 reds
     with `reads this execution made: map[]`.

     `RegisterRedundantMergedLegReader` now holds a SET of proof names per shape,
     because this shape has two live proofs perturbing it differently (decline vs
     misaim) and last-wins would have dropped whichever finished second.
  2. **The MINT arm was NOT widened to `.Name` selectors** (resolution (b), not
     (a)). The bound is stated at the arm and in the table above instead. The
     reason is a real cost, not a shrug: `.Name` is one of the most common field
     spellings in the tree, so catching `leg.typ.Fields[i].Name` without drowning
     the bucket needs the TYPE DISCRIMINATOR the gate's shallow tier
     deliberately does without — and that tier's precision trade is itself pinned
     by two fixtures, so changing it is a policy change with its own mutation
     matrix. Widening on spelling alone was not attempted because the measurement
     that would license it does not exist yet.

  **STATUS 2026-07-31 — phase 3a is MERGED (PR #544, master `7ce79fe38`), and
  the REMAINING WORK OF THIS ITEM IS NOW `CQ-67`.** The merge carries the three
  post-ACK census commits as well as the phase's body, and both review gates
  confirmed the final head rather than an earlier one. Nothing above is revised
  by this note — the refutations, the baseline censuses, and the two
  left-open entries all still read as written.

  What 3a landed is the INSTRUMENT LAYER the next step is denominated in, not a
  plan movement: the `foldStep1Seed` outcome census with its independently
  counted denominator, the read→firing mapping, and the producer pins. The step
  the entry's "WHAT THE NEXT ATTEMPT IS" paragraph describes — teach the layout
  authority to read `RC(_i: QOV(leg_i))` as a leg layout, then let
  `legIsOrdinalSafe`/`planBuriedLegConcat` admit such a leg — was designed under
  `rfcs/200-positional-merge-leg-windows.md` (merged, #543) and is booked as
  **CQ-67**. CQ-53 closes when CQ-67 closes; it carries no separate remainder.

- [x] **CQ-60 (S/M) — the ordering-comparator type-dispatch flip is measured
  FREE in production (decline residual 0 at both sites) but blocked by 24
  fixture tests minting bare-name FieldValue doubles** DONE: flip landed — type dispatch at both comparators, transitivity closure asserted (27 triples + D1/D2 separation + total decline), census zero, golden untouched, stress identical; the real fixture blocker was the missing orderingKeyLayoutProvider on the test double, not the bare names; two pre-existing gaps fixed in passing (M5 same-index name guard untested on master — now red under mutation; M16 badPairs sieve recorded as performance-only with its limit documented).
  (abstract_data_access_rule_test.go:181,:195 pattern) that model the
  pre-identity world. Redesign the fixtures so synthetic index columns are
  fields of a shared record row (stating layouts), then land the flip:
  both-*FieldValue -> identity-or-DECLINE, no fallthrough. The D1/D2/UNKNOWN
  transitivity witness test already exists and reds under availability
  dispatch. The census machinery (ordering_comparison_census.go +
  explaindiff/ordering_census_test.go) is the acceptance instrument.

- [x] **CQ-61 (M/L, executor binding contract, gated) — RecordTypeLeg.Name
  becomes a CorrelationIdentifier; leg identity stops being text.** DONE
  (merged #535): typed leg identity threaded to all 13 construction sites
  (`NewRecordTypeLeg`, docscheck-enforced), the leg-identity census with
  per-site population floors in the sqldriver TestMain, the NLJ plan retyped
  to carry `CorrelationIdentifier` leg identities, `GetFlowedObjectType` /
  `GetFlowedObjectValueTyped` ported from `Quantifier.java:801-803`, and the
  ordinal-join build's `Bare` arm. Two defects found and fixed while folding
  (bugs 15/16: untyped positional-merge slots → zero rows; the non-RC join
  result-value refusal). CQ-63/64 booked from the residue. The remaining text
  channels — the dotted `LEG.COL` column channel and the seed-window map's
  upper-folded text keys — are CQ-53's work, and `RecordTypeLeg.Name` retires
  with them. The
  CQ-53 investigation refuted its plan's foundation: concatLegPositionals
  "stores leg windows" but every consumer SELECTS the window by name string
  (ordinal_join.go:1419 GetCorrelationBinding text-match;
  executor.go:2805 re-minting identity via NamedCorrelationIdentifier;
  values/type.go:372) — verbatim the shape CQ-53's own correction rejected
  as hypothetical while it was the production binder. The retyping: leg
  identity sourced from the plan's quantifier alias at construction, never
  re-minted from text; ~13 construction sites, ~8 readers across 5
  packages; DELETES (not retypes) the two readers that exist only to serve
  the dotted channel (ordinal_join.go:722 rowSlotForLegColumn — gate-
  invisible because its carrier is values.Field.Name, not FieldValue.Field;
  cascades_translator.go:5901, listed under translator). Also touches
  values/ordinal_seed_layout.go:143-170 (name-matched) and adaptLegPositional,
  whose own comment concedes Java's end state (translateCorrelations
  rebinds ordinals against the physical quantifier's flowed type). This is
  step 0's pattern applied to leg identity: representation first, consumers
  after. CQ-53 IS GATED ON THIS ITEM — its entry's premise
  (concatLegPositionals-is-structural) is refuted and it must not be
  implemented on the string-keyed binder under a green gate.

  IMPLEMENTED + FOLD LAP DONE (branch feat/cq61-leg-identity-typed).
  RecordTypeLeg carries Alias, every reader whose counterparty is a
  correlation compares through values.SameLeg, and the leg identity is
  threaded rather than re-minted. What the fold lap added over the first
  implementation:
  - The census gate is real and IN CI: it moved to the sqldriver TestMain,
    unconditional, asserting per-site population FLOORS as well as the zeros.
    The previous form reported Total 0 at six of the eight sites when run
    alone (measured; expressionOutputLegs reported 4 and NLJ plan alias 370),
    so its zeros were vacuous. Whole-suite population — VARIES run to run,
    since several sites sit inside Cascades rules and the memo may explore a
    rule once or many times per query: rowLegsBinder 285 and buriedLegWindow
    567 are the only two that repeat exactly; text-vs-identity has measured
    3115 / 3270 / 3320, hoist ~1000-1052, finalizeSeedWindows ~1287-1311,
    expressionOutputLegs ~3239-3263, NLJ plan alias ~80k,
    ordinalSlotInLegWindow 105. The floors sit an order of magnitude below, so
    they detect COLLAPSE rather than that variance.
  - values.NewRecordTypeLeg takes the identity as its first positional
    parameter (omitting it is now a compile error, not a zero-identity leg),
    all 13 production sites converted, and a docscheck AST scan forbids the
    composite literal outside the constructor. SameLeg declines an UNSTATED
    identifier — SameLeg(zero, zero) used to be true, so two omissions agreed
    and bound.
  - RecordQueryNestedLoopJoinPlan holds its leg identities as
    CorrelationIdentifiers, threaded from the select's quantifiers; the
    executor's boundary mints are deleted. The "proto-visible" objection is
    refuted: Go marshals no plan through the plan protos, the memo structural
    key takes the identifiers' raw Name(), plan_shape.golden is
    byte-identical. Deleting that path's ToUpper removed a forgery generator
    (ToUpper of a minted lowercase q$N is Q$N, the spelling SameLeg exists to
    exclude) — the plan and its own seed disagreed by case on the 12 of ~80k
    firings where the source-alias slice carries a re-minted name. (The 12 is
    stable across runs; the denominator is not — 79040 / 79960 / 80856
    measured.)
  - ordinalSlotInLegWindow converted (the last Group-A folding reader), and
    finalizeSeedWindows converted too. Name's retirement condition is now
    crisp: dotted-text consumers plus the seed-window map keys, nothing else.
  - THE ACCEPTANCE INSTRUMENT WAS MEASURING A DIFFERENT PROGRAM, and that is
    what let finalizeSeedWindows be written up as unconvertible. The census
    took two STRINGS, so the four readers that compare IDENTITIES all recorded
    the leg's text against the counterparty's text — a pair that scores EXACT
    where the shipped comparison DECLINES. Fixed: converted sites now record
    the pair they evaluate AND the verdict of the predicate they replaced
    (RecordLegIdentityConversion), because fold-only cannot see a
    decline→match flip (it is byte-equal, so it lands in ExactEqual). A site's
    census channel is OBSERVED, so an instrument that drifts from its
    comparison is reported instead of believed. The translator package
    (//pkg/relational/core/query) gained its own census harness; its corpus
    drives the planning-time readers the SQL suite barely touches.
  - MEASURED with the true pair, real-FDB corpus, all four converted sites:
    retiredVerdictDivergent 0, foldOnly 0, unstated 0 — rowLegsBinder 285,
    buriedLegWindow 567, hoist 1052, ordinalSlotInLegWindow 105, and
    finalizeSeedWindows 1311. The conversion is representation-only on
    production traffic, measured on the DECISION rather than inferred from
    separate zeros.
  - THE "finalizeSeedWindows CANNOT CONVERT" JUSTIFICATION IS WITHDRAWN, and
    so is the claim that "the earlier reproduction that found it green was the
    one that missed" — that claim was wrong. The cited red
    ("3-way DRIFT: leaf C col ORDER_ID — leg-window walk slot 0 (ok=false)")
    reproduces exactly, and its cause is the FIXTURE:
    TestThreeWayBoxCrossAgreement hand-minted the box correlation as lowercase
    "c" where production mints NamedCorrelationIdentifier(sourceAlias(box)) =
    "C", and its own comment mis-stated that spelling as the sourceBinding.
    The two identifiers finalizeSeedWindows compares are not "legitimately
    different — box quantifier vs leaf"; the sourceBinding convention is
    precisely that for the rightmost leaf they are the SAME identifier, both
    upper-folded by the one sourceAlias chokepoint. The fixture now derives the
    correlation the way production does, the site is converted, and the premise
    has a deterministic pin of its own
    (TestBoxCorrelationIsItsRightmostLeafIdentity, red in BOTH directions under
    mutation: unfolding sourceAlias, and folding only the leaf producer).

  FOUND WHILE FOLDING, NOW FIXED. A 3-way comma join with a projected EXISTS
  whose legs are tied by equijoin predicates returned NO ROWS, and for TWO
  independent pre-existing reasons (both verified at this branch's base commit).
  Both fixes are in; the earlier writeup of this item mis-located the defect and
  is superseded below.
  DEFECT 1 — UNTYPED POSITIONAL-MERGE SLOTS (zero rows, NO error, the worse of
  the two). PartitionSelectRule's single-live-lower arm gave its lower select an
  UNTYPED flowed row (`Quantifier.GetFlowedObjectValue()`), and a later
  positional-merge round derived the collapsed legs' row types by SCAVENGING the
  select's own value surfaces (positional_merge.go legRowTypes) — which finds
  nothing precisely when the result value is itself an untyped flowed row. The
  merge slots went UNKNOWN, so the equijoin operand pushed into the B leg's scan
  could not bake to a pinned ordinal; a source-relative operand evaluates to NULL
  against the build-bound row, the scan matched nothing, and the lowest join
  emitted zero rows silently. Java never has this: the QUANTIFIER carries its own
  row type (Quantifier.java:801-803 — `getFlowedObjectValue()` is
  `QuantifiedObjectValue.of(alias, getFlowedObjectType())`, always typed). Fixed
  by porting that: `Quantifier.GetFlowedObjectType` /
  `GetFlowedObjectValueTyped` (expressions/quantifier.go), used as the AUTHORITY
  for the merge slots, with legRowTypes kept only as the fallback for a reference
  that carries no typed result value yet. GetFlowedObjectType also ports Java's
  member-agreement VERIFY (Reference.java:504-513 reduces over every member with
  Verify.verify(left.equals(right))), which Go had cited without performing —
  reading members[0] picks a row shape by memo insertion order, and that shape is
  the merge slot's type. A disagreement is now an explicit error and the merge
  declines the rule. The OTHER site that flows an untyped row
  (rule_partition_select.go:645, the Case-2 arm) is NOT part of this item and is
  booked as CQ-63 with its measured evidence: typing it drifts 7151 golden lines
  and regresses four suites, which is a second defect downstream, not a reason to
  leave it untyped.
  DEFECT 2 — the middle FlatMap's result value is a bare baked
  `ofOrdinal(QOV(merge), 0)` and `executor.newOrdinalJoinBuild` refused to build
  an ordinal join whose result value was not a RecordConstructorValue. That
  refusal was itself the bug, not a guard: Java imposes no RC requirement
  anywhere — ImplementNestedLoopJoinRule.java:187,201,214 pass
  selectExpression.getResultValue() VERBATIM in all three arms, and
  PartitionSelectRule.java:281,319 legitimately MINTS the bare shape (a
  single-live-lower select flows one leg's whole row; a later merge round
  translates that bare QOV into `ofOrdinal(QOV(merge), i)`). Fixed with the
  build's `Bare` arm: the build stays ENABLED (so the legs bind and the outer is
  adapted to the merge layout the inner's pushed SARGs read by ordinal) and the
  one value is evaluated — a row flows through AS ITSELF, a scalar wraps into the
  1-slot row. The previously-measured "decline the build" fix is still wrong and
  for the recorded reason (zero rows), and re-wrapping the flowed row is wrong
  too (measured: `ordinal resolution: field "K" not resolvable ... row columns
  [_0]`); both are pinned.
  GATES: the plan-level file's old invariant (`ContainsBakedOrdinal ⟹ RC`) was
  FALSE and is gone with its debt list. It is replaced by two gates that are
  true and each red without its fix — `TestPositionalMergeRowSlotsAreTyped`
  (every positional-merge slot flows a TYPED leg row; red with defect 1
  reintroduced) and
  `TestJoinResultValueWithBakedOrdinalsIsRCOrWholeValueReference` (a baked
  non-RC join result value must be the WHOLE value, a single baked reference over
  a leg QOV) — plus vacuity guards on both so a planner change that stops
  producing either shape cannot leave them silently trivial.
  `TestFDB_CommaJoin3ProjectedExistsWithEquijoins` is now a plain row assertion
  rendering rows POSITIONALLY (a name-keyed rendering passes with two columns
  swapped), and `TestOrdinalJoinBuild_Constructor` pins the Bare arm's
  construction and its row-vs-scalar output shape.

- [x] **CQ-63 (S/M, bug fifteen: something downstream depends on the ABSENCE
  of a type) — PartitionSelectRule's Case-2 lower select flows an UNTYPED
  result value, and typing it regresses four suites.** · **DONE, merged
  `61847c503` (#537) — "the flowed value is typed, as Java's always is; bugs
  seventeen and eighteen die".** The fix is in the tree:
  `pkg/recordlayer/query/plan/cascades/rule_partition_select.go:663` now reads
  `flowedValue, flowedErr := aliasToQ[lowerAlias].GetFlowedObjectValueTyped()`,
  with the Java citation this entry demanded. This file already recorded the
  item's own gate as met 950 lines earlier — "`underivableLegs` **0 of 960**
  (was 82 of 846). CQ-63's stated gate is … CLOSED — every leg states its row" —
  so TODO.md contradicted its own checkbox in two places at once.
  Java's quantifier
  always carries its own row type
  (Quantifier.java:801-803 — `getFlowedObjectValue()` is
  `QuantifiedObjectValue.of(alias, getFlowedObjectType())`), so no Java site
  flows an untyped row. Go has one left: rule_partition_select.go:645, the
  Case-2 (≥2-live-lowers) arm's `Quantifier.GetFlowedObjectValue()`.

  Its sibling — the SINGLE-live-lower arm feeding the positional merge's slot
  types — was the CQ-61 defect and is FIXED (the merge slots take the
  quantifier as the authority via GetFlowedObjectValueTyped). This item is the
  residual: the other call site, which stays untyped.

  MEASURED, and the measurement is the reason this is its own item rather than
  a one-line follow-on. Typing the Case-2 lower select's result value as well:
  drifts 7151 plan-shape golden lines, inserts a spurious Map over a filter,
  and regresses TestFDB_JoinMerge_OuterColumn_NotDropped,
  TestFDB_ArrayUnnestOrdinality/gathered_flat_multi-source_unnest, yamsql
  join_three_way_predicate and rowdiff seed 5 to ZERO ROWS. The
  merge-slot site alone drifts no goldens.

  That is not a reason to leave it untyped — it is EVIDENCE OF A SECOND
  DEFECT. Four independent suites cannot depend on a row type being absent
  unless something downstream is keying on untypedness: a rule whose match
  condition is "the result value has no row type", a bake that declines when
  it can resolve, or an equality/interning path that distinguishes the typed
  and untyped QOV as different expressions (the memo has interned on the
  untyped form — ~40 GetResultValue implementations return it). Find WHICH,
  by bisecting the four regressions against the golden drift; the spurious Map
  over a filter is the most legible lead, since a Map appearing means some
  rule started matching that did not before.

  DO NOT close this by typing the site and re-blessing 7151 golden lines. The
  golden movement is the symptom. Fix the downstream dependence, then the
  typing should be inert — and if it is not, the remaining drift needs a
  per-line justification.

  Not gated on CQ-53: it touches no leg identity and no dotted channel.

  ESCALATED — CQ-53 IS GATED ON THIS ITEM, and the gate is measured. The
  qualified `LEG.COL` channel cannot retire while a leg's quantifier states no
  row layout, because a leg-correlated read then has no ordinal it can honestly
  carry on its own alias. Measured over the real-FDB corpus **on the faithful
  instrument** (`Quantifier.GetFlowedObjectType`, Java's
  `Quantifier.java:806-810`): 92 of 126 such reads fall through to the name, and
  they trace to **82** underivable-leg derivations over 3 distinct witnesses,
  every one a `RecordQueryFlatMapPlan` flowing a bare untyped quantifier object.

  (An earlier revision of this line said 32. That number was wrong. On the
  unmodified branch-HEAD instrument the same corpus measures 122; routing
  through the quantifier's flowed type closes 40 of those. **82 is the
  baseline.**)

  This item is therefore no longer a residual cleanup — it is the thing that
  unblocks the largest remaining RFC-197 channel, and it should run BEFORE the
  rest of CQ-53. The `leg-local bakeability` census (with its `NO-LAYOUT`
  witnesses) is the acceptance instrument: when this item lands,
  `underivableLegs` should go to 0 and `layoutAvailable` should approach
  `minted`. Note `layoutAvailable`, NOT `baked` — `baked` counts reads the arm
  actually converted, and the leg-local bake arm is deleted (it minted an
  ordinal from a display name). Restoring it, keyed on IDENTITY rather than
  name, is part of this item's landing.

  A HAZARD MEASURED WHILE ESTABLISHING THE BASELINE, and the reason the
  seeded-result-value escape was removed. `values.isMixedSeedElement` admits any
  bare `QuantifiedObjectValue` whose type is not a `RecordType` — the arm for
  Java's `isPrimitive()` whole-object scalar element. An UNTYPED quantifier
  object is not a `RecordType` either, so **an untyped leg is indistinguishable
  from a genuinely scalar element**: a 2-slot record constructor of bare untyped
  QOVs is ACCEPTED by `values.OrdinalSeedLegWindows` and yields two ONE-column
  windows. A leg flowing a whole multi-column row would silently get a 1-column
  layout. Not live today (the escape is not wired, and `legRowTypesFromQuantifiers`
  never consults the seed authority), and measured/pinned by
  `TestOrdinalSeedLegWindows_DeclinesANoLayoutFlatMapLeg`. Tightening
  `isMixedSeedElement` is NOT free — it changes seed acceptance planner-wide and
  the executor twin (`unnestMixedSeedSpans`/`ordinalJoinSpans`) has to move with
  it or the cross-agreement invariant breaks — so it wants its own measurement
  pass. It is listed here because it is the same defect as this item (an untyped
  quantifier read as something it is not) and should be fixed with it.

- [x] **CQ-67 (HIGH/L, L, executor-gated, query-engine review gate) — implement
  RFC-200: the layout authority learns the NESTED merged leg, and CQ-53's 60
  `reconstruct-nil / positional-merge` declines become ACCEPTs.** The design is
  **DONE — merged as #549.** Closing this item also closed CQ-53, whose
  remaining work this was. The design is
  `rfcs/200-positional-merge-leg-windows.md` (merged, #543) and it is ACK'd;
  nothing here re-decides it. **Closing this item CLOSES CQ-53** — that entry's
  "WHAT THE NEXT ATTEMPT IS" paragraph is this work verbatim, and CQ-53 carries
  no other remainder.

  The defect in one line: Go's planner already BUILDS Java's merged row
  (`positional_merge.go`'s `positionalMergeCase`, which is
  `PartitionSelectRule.java:283-291`'s `RecordConstructorValue.ofColumns(...
  Column::unnamedOf)` verbatim) and Go's executor already evaluates it, and Go's
  own layout authority declines to read it — so 60 firings fall back to the
  identity pass-through and keep the runtime name channel alive.

  **Line numbers: RFC-200 was written against `f50cee43e`, and PR #544 shifted
  its `rule_implement_nested_loop_join.go` references below ~`:3700` by +22.**
  Verified on `7ce79fe38`: `foldStep1Seed`'s validation is `:2277` (unchanged),
  `materializedNLJOrdinalLayoutMatches` `:2186` (unchanged),
  `legIsOrdinalSafe` `:1956`, `planBuriedLegConcat` `:1997`, but
  `implementJoinWithExistential`'s `ordinalSeedLegWindowsOf(step1RV)` is now
  `:3781` (RFC says `:3759`), its three rebase sites `:3803` / `:3863` / `:3926`,
  the seed-decision denominator call `:3668` (RFC says `:3646`), and the bare-QOV
  mint `:3901` (RFC says `:3879`). Re-derive before citing; do not trust the
  RFC's numbers mechanically.

  The pieces, each of which the RFC decides and this item only builds:

  - **A NARROW OPT-IN ENTRY POINT, and the shared accept boundary does NOT
    move** (§3). `values.OrdinalSeedLegWindows` keeps its exact accept set and
    gains a sibling that recognizes `IsPositionalMergeRC` at the head and emits
    nested sub-windows. The narrow entry DECLINES, fail-closed, any seed carrying
    a nested leg — top-level-only windows would silently lack sub-windows a flat
    box leg would have had. **Exactly THREE sites opt in**, all reading the same
    `step1RV`: `foldStep1Seed`'s validation of the seed it just built (it must
    accept what it constructs), `implementJoinWithExistential`'s
    `ordinalWindows, mergedRowType` (which feeds the three rebase sites), and
    `materializedNLJOrdinalLayoutMatches` (it must see the windows to check the
    orientation at all). The other fourteen callers consume the derivation as a
    nil/non-nil PREDICATE, where population IS meaning — §3's table proves each
    one's answer is unchanged on every input it can see. **If one of those
    fourteen turns out to need the nested windows, that is a STOP, not a
    widening**: widening the narrow entry to accommodate it re-opens every row of
    that table, so halt and return to the RFC's reviewers with the site.
  - **The nested window KIND is an explicit discriminator with an INVALID zero
    value** (§2). `kindUnset` is the zero and every reader declines or panics on
    it, because Go's zero would otherwise mean `flatRun` by language default —
    the exact inference the section forbids. The kind lives on two carriers
    (`OrdinalSeedLegWindow.Kind` and `values.RecordTypeLeg`), excluded from
    identity as `RecordType.Legs` already is. **`NewRecordTypeLeg`'s positional
    signature grows** to carry it: the positional constructor IS the compile-time
    defence — its own doc records that deleting `Alias:` from two producers left
    the whole suite green — so a kind that could be omitted reproduces that
    failure one field over. All 13 non-test producers stamp or CARRY explicitly;
    the three REBASE sites carry (a rebase that re-mints a kind is the same defect
    class as one that re-mints an `Alias`). `Width`'s doc is corrected to "its
    slot count in the carrying type", which is what every reader already assumes.
  - **`legIsOrdinalSafe` and `planBuriedLegConcat` move in LOCKSTEP** (§4) —
    same node census by construction, so a FlatMap arm in one without the other
    is a layout the reconstruction cannot build. The arm is CONDITIONAL on the
    FlatMap's result value being the positional merge, recognized by the FULL
    structural recognizer and never by the looser "every field is a bare typed
    QOV": `leg_layout_derivation_test.go:103-109` pins that a NAMED typed-leg RC
    is still not a merge row, and it stays green only because its fixture names
    the fields `"A"`/`"B"`. That pin's failure message is updated in the same
    change to say what it now guards.
  - **3d′ — the pre-existing FAIL-OPEN in `materializedNLJOrdinalLayoutMatches`
    is FIXED, on its own sequencing step** (§9). `len(windows) != 2 → return
    true` is the PERMISSIVE answer, not "the safe default"; the function's own doc
    says declining is always safe because commutativity ADDS a candidate. It is
    already reachable today (a box leg's sub-windows already push the count past
    2) and the new population would make skipping UNIVERSAL. The fix is a
    TOP-LEVEL RUN LIST alongside the map — the planner twin of the executor's
    `ordinalJoinSpansOf` spans — because `finalizeSeedWindows`' rightmost-leaf
    case REPLACES a box run's entry with a narrower sub-window, so "the windows
    that tile the row" is not recoverable from the map after the fact. This moves
    plans that have nothing to do with the merge row, so it carries its OWN
    golden and stress burden and lands separately from 3d; the diffs must be
    readable on their own.

  **Gate (a) — an end-to-end WRONG-ROWS fixture, mutated in FOUR directions,
  each separately.** Cross-agreement of two walks is necessary and not
  sufficient: two walks of the same wrong model agree perfectly. Distinct leg
  widths, at least one duplicate column NAME across legs (so a name fallback
  cannot rescue a wrong ordinal), projected value drawn from a non-first leg at a
  non-zero leg-local ordinal. Directions: (1) nested read as flat; (2) flat read
  as nested; (3) **leg orientation** — legs swapped relative to the seed's baked
  layout, expected observable a LOUD unbound-correlation error or an unexecutable
  plan, not wrong rows, because `:2141-2150` claims the ordinal read is "either
  right or loud"; **if that direction produces silently wrong rows, that claim is
  REFUTED and it is a finding in its own right — it gets a line in RFC-200 and a
  report before the gate is called satisfied**; (4) the census-invisible
  bare-column arm of `slotInGatheredSeed` (`unnest_gather.go:467-471`), which
  ranges every window and takes `w.Offset+idx`, allowed to contribute a `hits++`.
  **The outcomes are recorded in the fixture's own doc comment**, naming per
  direction what goes red and what re-arms if it stops — the discipline
  `leg_layout_derivation_test.go` already applies to its negatives. A mutation
  result that lives only in a PR description is a measurement that evaporates.

  **Gate (b) — the 1M stress before and after, at 3d AND 3d′ separately**, real
  plan movement expected at both, every moved `EXPLAIN` golden and plandiff
  record justified line by line WITH ROW COUNTS. Never blanket re-blessed.
  Results land in this file's baseline table. Run both sides on the SAME
  FILESYSTEM — CLAUDE.md's recipe was corrected for exactly this after a
  disk-utilisation artefact read as a 2.3x planner regression (CQ-53's
  methodology note).

  **Gate (c) — census EQUALITIES, not floors.** These are PREDICTIONS; a measured
  deviation is a reportable finding and must not be absorbed by relaxing the
  assertion. Post-3d the `foldStep1Seed` outcome census must satisfy
  `ACCEPT == 138` (78 + 60), `DECLINE reconstruct-nil == 94` (154 − 60, all
  bare-QOV), `DECLINE correlatedStep1 == 108` (unchanged — the wall),
  `DECLINE rv-no-exist-ref == 200` (unchanged), and the four classes must sum to
  a denominator of `540` **counted independently** at
  `implementJoinWithExistential`'s seed-decision call site, never by summing the
  four class counters (summing counters incremented inside the function they
  partition is true by construction and gates nothing). "Up to 60" is deleted:
  the prediction is 60. Alongside: `Declined` stays 0, both seed-window hard
  zeros stay 0, all five reader floors hold, `IdentityInLegDomain == Baked +
  MergedReAnchor` holds, `MergeSlotTypeDisagreements` stays 0, the executor's
  leg-column provenance census does NOT move (movement there means this touched
  the scalar-seed channel, which it must not), and `rule_select_merge.go:234`'s
  `len(w) > 2` is re-measured explicitly — under the separate entry point it must
  be unchanged, and that is the check that the narrow boundary held in practice
  and not only on paper. Note `MergedReAnchor` may stay 0 on complete success and
  is therefore BLIND to this change; nothing may be denominated in it.

  **The two 3a measurement deliverables, which do not exist today and are
  produced FIRST:** (i) the **read→firing mapping** — how many of
  `LegLocalBakeCensus`' 174 reads occur under a firing the outcome census
  classifies `reconstruct-nil / positional-merge`. Gate (d) is denominated in
  that number and asserts its post-change value is 0, with the remainder
  reported. (ii) whether **all 60** declined positional-merge legs satisfy
  `IsPositionalMergeRC` and not merely "two bare QOVs over record types" — today
  that is an INFERENCE, because the removed probe recorded value shapes, not
  field names, and §1 makes the full recognizer load-bearing.

  **Plus one addition that lands BEFORE everything above, because the conversion
  leans on the leg-table-ignoring pin:
  `TestLegColumnOwner_TheDestinationLegTypeCarriesNoLegTable`
  (`pkg/recordlayer/query/executor/leg_column_owner_selection_test.go:142`) must
  gain its hash twin.** Its doc comment states "RecordType.Legs is layout
  metadata that Equals and Hash ignore", and that is the precondition the whole
  conversion leans on — but only `Equals` is asserted (`:163-168`). The hash side
  is unpinned prose. Measured on `7ce79fe38`: `values.RecordType` has NO `Hash`
  method at all (`grep -i` for a hash function in
  `pkg/recordlayer/query/plan/cascades/values/type.go` returns nothing), so the
  first deliverable is to NAME the identity channel the memo actually keys a
  record type on and pin THAT — the doc comment names a method that does not
  exist, which is itself the finding. A channel that started reading `Legs` would
  corrupt memo lookups far more quietly than a failed equality: two structurally
  identical types would hash apart, splitting a memo group instead of raising
  anything. Same failure message discipline as the existing half — name what
  re-arms.

  DONE = the three opt-in sites read the nested entry, the 60 firings ACCEPT,
  gates (a)–(d) all satisfied and recorded, 3d and 3d′ landed as separate
  sequencing steps with their own goldens, and CQ-53 marked `[x]` against this
  item. Executor + NLJ rule + a physical row-layout contract between planner and
  executor, so: Graefe-gated, one impl lap at phase completion, cross-agreement
  fixture is the bit-for-bit gate.

  ---

  **IMPLEMENTED on `feat/rfc200-nested-leg-windows`, steps 3a–3d′, awaiting the
  joint review lap.** Measured state:

  **Gate (c) — EVERY prediction landed EXACTLY.** The `foldStep1Seed` outcome
  census is now a standing instrument (it replaced a probe that had been deleted)
  with its denominator counted INDEPENDENTLY at the seed-decision call site:

  | | before | after | predicted |
  |---|---|---|---|
  | ACCEPT | 78 | **138** | 138 (78+60) HIT |
  | DECLINE correlatedStep1 | 108 | 108 | unchanged HIT |
  | DECLINE rv-no-exist-ref | 202 | 202 | unchanged HIT |
  | DECLINE reconstruct-nil | 154 | **94** | 94 (154−60) HIT |
  | — of which bare-QOV | 94 | 94 | 94 HIT |
  | — of which positional-merge | 60 | **0** | 0 HIT |
  | DECLINE windows-nil | 0 | 0 | HIT |
  | denominator | 542 | 542 | unchanged HIT |
  | firings with BOTH legs unsafe | 0 | 0 | HIT |

  This entry states the denominator as `540` and rv-no-exist-ref as `200`;
  measured they are `542` / `202`. The `+2` is **corpus growth, identified rather
  than assumed**: the merged-leg reader-shape fixture became shared between the
  redundancy pin and the wrong-window mutation pin, so its WHERE-EXISTS query is
  planned twice where it was planned once, and that test alone contributes
  exactly 2 firings, both in that class. Every load-bearing number is exact. (The
  committed gate values are higher again — 546 / 140 / 96 — because this branch's
  own gate-(a) fixture adds four more firings; the delta is attributed at the
  assertion.)

  **Gate (d) is VACUOUS, and that CONTRADICTS this entry's framing.** The
  read→firing mapping (3a deliverable (i)) did not exist and is now a standing
  cut: `legRebaseOrigin` carries the firing's class AND its refused-leg SHAPE down
  to the leg-match arm, because the two censuses count different events (reads
  vs rule firings) and their totals cannot be apportioned into one another by
  arithmetic. Measured: **all 174 leg-local reads sit under BARE-QOV firings and
  NONE under a positional-merge firing — already true BEFORE the change.** So the
  gate holds vacuously, and **the nested window converts 60 firings while removing
  ZERO pass-through reads.** `LegLocalBakeCensus` still reports 174/174 taking the
  leg-alias pass-through.

  **CONSEQUENCE: the read-axis retirement of the runtime binding-namespace
  widening (`executor.bindMergedOuterLegs`) moves ENTIRELY to CQ-68.** All 174 of
  DIVERGENCES.md's reads belong to the 94-firing bare-QOV block. This item was a
  necessary STRUCTURAL step — the layout authority can now express a nested row at
  all, and the executor twin agrees with it bit-for-bit — but on the read axis the
  divergence is exactly where it was. DIVERGENCES.md is updated with the
  60-closed / 94-open / 108-permanent decomposition. (The open block re-measured to
  102 under CQ-68, whose TYPING premise that entry then refuted: the block is a
  SHAPE residue and every leg in it is already typed.)

  **3a deliverable (ii): all 60 declined positional-merge legs DO satisfy
  `IsPositionalMergeRC`**, not merely "two bare QOVs over record types" — the
  RC-not-a-merge bucket measured 0 and the witness is one distinct shape,
  `x60 FlatMap rv=positional-merge RC(2)`. The inference is now a measurement.

  **The hash twin: the doc comment named a method that does not exist.**
  `values.RecordType` has no `Hash`. The identity channels the memo ACTUALLY keys
  a record type on were read and pinned instead — `SemanticHashCode`,
  `SemanticEqualsUnderAliasMap`, `EqualsWithoutChildren`, and `String()` (the
  EXPLAIN-golden channel) — over all three carriers a leg-table-bearing type
  travels on. Four mutations, each RED separately. Mutation 2 exposed that
  `SemanticEqualsUnderAliasMap` INTERCEPTS correlation-bearing leaves before
  `EqualsWithoutChildren`, so they are independent copies of one rule and one
  stays green while the other breaks — which is why both are asserted.

  **3d′ closed the fail-open and moved NO plan, with the reason MEASURED rather
  than assumed.** The gate now counts TILES via a top-level run list the walk
  returns (the map cannot serve it — the rightmost-leaf case replaces a box run's
  tile with a narrower sub-window). An orientation-gate census separates
  live-and-agreeing from latent: `calls 438 (not-a-seed 96, tiled-by-2 342,
  tiled-by-other 0); of the tiled-by-2: unverifiable 84, matched 197, declined 61;
  firings where the MAP count differs from the TILE count — the population 3d′
  moves — 72, of which DECLINED 0.` **72 firings became newly checkable and ALL 72
  MATCH**; the 61 declines were already being checked. `MapCountDiffers` is
  floored as the live/latent discriminator. A SECOND fail-open (84 "unverifiable"
  — a leg plan that cannot state its row shape) is left standing deliberately and
  is now counted rather than merely true.

  **Gate (b): stress before/after recorded** in this file's baseline table (same
  filesystem, sibling worktree, branch point `719f6c8b0`). Row counts identical
  on every shape, every EXPLAIN byte-identical, totals +0.34%.

  **Gate (a) is NOT SATISFIED, and the reason was RE-DIAGNOSED after the first
  review lap — the earlier text here was wrong and is replaced rather than
  amended.**

  It recorded that the shape reaching the nested window "does not execute". That
  was true of the fixture as first written — a PREDICATE-FREE three-source comma
  join — and false of the shape the corpus actually produces. Re-pointed at the
  corpus's own form (three sources WITH EQUIJOINS, as
  `TestFDB_CommaJoinProjectedExists_UnequalLegWidths` carries), the query RUNS
  and returns correct rows on all four addresses.

  **What is actually unmeasurable is the READER arm, and it is now measured as
  LATENT.** The seed-window reader census reports `NESTED-HIT 0` at both keyed
  readers over the whole corpus, and mutating the fused two-step address back to
  flat `Offset + legOrdinal` leaves the end-to-end probe GREEN. The nested
  acceptance is live at the LAYOUT — ACCEPT 78 to 138 exactly as predicted, and
  `existentialRebase` 962 to 1086, which is RFC-200 section 6's own growth
  prediction confirmed — but every window those reads select is a FLAT top-level
  run. A nested SUB-window is only selected by a reference to a leg buried INSIDE
  the merge, and no corpus query produces one.

  So gate (a)'s four mutation directions are not writable until `NESTED-HIT` is
  non-zero. The fixture, the two-source flat control and the row assertions are
  in `pkg/relational/sqldriver/nested_merge_leg_wrong_rows_fdb_test.go`; the
  predicate-free failure is pinned separately there as CQ-70's reproducer.

  **RULED: the latent reader arm MERGES.** It clears a strictly higher bar than
  3d′ set — correct, cross-agreement-pinned on both entries, unit-pinned on both
  arms, and instrumented — and holding correct instrumented code hostage to
  CQ-68/CQ-70 is the deferral pattern this project forbids.

  **REOPEN TRIGGER — THIS ITEM IS NOT FINISHED, IT IS PARKED ON A TRIPWIRE.**

  > **When the seed-window reader census reports a non-zero `NESTED-HIT` at any
  > site, CQ-67 REOPENS and gate (a) becomes the work.**

  **THE TRIGGER'S NAMED ROUTE WAS REFUTED; THE TRIGGER ITSELF STANDS.** This
  paragraph said "whoever lands CQ-68 is the most likely person to trip it:
  typing the 94 bare-QOV result values". CQ-68 was run and its premise refuted —
  the population is 102, it is 100% TYPED (real `RecordType`s: arity 1-3 on the
  FlatMap legs, 1-4 counting the `RecordQueryNestedLoopJoinPlan`-legged declines),
  and typing cannot convert one of them because `legOrdinalSafety` refuses on
  `values.IsPositionalMergeRC`, a `*RecordConstructorValue` assertion no
  `QuantifiedObjectValue` satisfies at any typing. `bare` meant identity
  PASS-THROUGH, not untyped.

  The route that actually reaches a leg buried INSIDE the merge — the only thing
  that selects a nested sub-window — is the SHAPE conversion: making the declined
  leg flow `RC(_i: QOV(leg_i))`. **That is CQ-95's work, and CQ-95 is who must
  read this paragraph before starting.** It is here, and not only in a test file,
  so it cannot be missed by someone who never opens the census.

  The trigger is ASSERTED, not printed: `SeedWindowReaderFloors.NestedHitMustBeZero`
  is armed in the sqldriver corpus harness, so activation day is a RED test whose
  failure message carries the whole hand-over (the fixture, the four mutation
  directions, the "either right or loud" refutation clause, and how to clear the
  flag). Without it, activation would change nothing visible and gate (a) would
  stay unwritten BY DEFAULT rather than by decision.

  On trip: write gate (a)'s four directions against
  `pkg/relational/sqldriver/nested_merge_leg_wrong_rows_fdb_test.go` (already
  built, already carrying distinct leg widths, a cross-leg duplicate column name
  and a non-first-leg non-zero-ordinal projection), record the outcomes in that
  fixture's own doc comment, clear `NestedHitMustBeZero`, floor `NESTED-HIT`
  instead, and mark gate (a) satisfied.

  **VERIFICATION ARTIFACT — the branch's own merge gate, recorded rather than
  asserted.** The merge commit `2d0c73ff2` used `--no-verify` on the ground that
  "the merged HEAD gets a full run immediately after, and that run is the gate",
  which left no artifact. Recorded here instead: `bazelisk test //pkg/...` at the
  merged head `2d0c73ff2` on 2026-07-31 reported **62 of 62 targets passing**,
  and every other commit on this branch ran the full pre-commit suite. A branch
  that pins twelve mutations as tests may not exempt its own gate from evidence.

- [x] **CQ-68 — REFUTED AND CLOSED. The residue is a SHAPE residue, not a typing
  gap; there was never anything here to type, and the target moves to CQ-95.**

  **What was measured** (one real-FDB sqldriver corpus run, every number a run
  rather than a reading):

  - The population is **102**, not 94, and it is **100% TYPED**. Every declined
    leg carries a real `RecordType`: arity 2 ×30, arity 3 ×66, arity 1 ×4 on
    FlatMap legs, plus arity 4 ×2 on a `RecordQueryNestedLoopJoinPlan` leg — so
    1-3 across the FlatMap legs and 1-4 across the whole declined population.
    Zero untyped.
  - **Typing could not convert one of them at any typing.** `legOrdinalSafety`'s
    FlatMap arm refuses on `!values.IsPositionalMergeRC(pl.GetResultValue())`,
    and `IsPositionalMergeRC` opens with a `*RecordConstructorValue` type
    assertion that a `QuantifiedObjectValue` fails unconditionally.
  - **Producer attribution is measured, not inferred.** Every FlatMap-legged
    decline originates at `buildCorrelatedFlatMapPlan`, attributed by the result
    value's own identity; that site emits **UNTYPED-QOV 0** across 25,406
    constructions. The two NLJ-legged declines report `origin=UNRECORDED`,
    correctly — an NLJ result value never passes a FlatMap constructor.
  - **Java is not diverged from on this population.** Java's select result value
    is `overQuantifier.getFlowedObjectValue()` — a typed QOV
    (`GraphExpansion.java:401`, `Quantifier.java:801-803`). Go's is too. Java has
    no `legOrdinalSafety` equivalent because it has no ordinal seed; the shape
    gap is Go-side architecture, not a port gap.

  **The original premise, quoted as history:** "the RFC-200 residue: 94 FlatMap
  result values are a BARE UNTYPED QOV where Java types unconditionally, and they
  are the LARGEST addressable block, not the one CQ-67 closes. `94 > 60`." The
  error is one word: **`bare` in `legOrdinalSafety`'s own comment meant identity
  PASS-THROUGH, not UNTYPED**, and reading it as untyped is where this item came
  from. That misreading is now pinned closed from two directions — the outcome
  census's witness spells the flowed TYPE (arity), not a boolean, and the
  producer census asserts the residue producer's untyped count at a hard zero.
  **Do not re-open this item by re-reading `bare` as `untyped`.**

  **THE TARGET SURVIVES ON CQ-95.** The residue converts only by giving the
  declined leg a positional-merge result value — making
  `buildCorrelatedFlatMapPlan`'s accumulated-inner leg flow `RC(_i: QOV(leg_i))`
  where it flows the identity QOV. Different change, different risk surface, its
  own RFC and Graefe ACK. Two live Java divergences found in passing are booked
  separately as **CQ-96** and **CQ-97**; the reachability deliverable landed and
  is described under CQ-95's inheritance block.

  <details><summary>Original entry, retained for its measurements and its
  history</summary>

  Gated on CQ-67 landing — the two touch the same
  `foldStep1Seed` decline population and the census equalities in CQ-67's gate
  (c) are stated against a 94 that has not moved.

  **THIS ITEM'S PREMISE IS NOW STRONGER THAN `94 > 60`, and the strengthening is
  measured.** CQ-67's implementation produced the read→firing mapping that did
  not exist when either entry was written: **all 174 of `LegLocalBakeCensus`'
  leg-local pass-through reads sit under firings whose refused leg is a BARE QOV,
  and ZERO under a positional-merge firing.** That was already true before CQ-67
  and is still true after it.

  So the 94 are not merely the larger block by firing count — **they carry the
  ENTIRE read population**, and CQ-67 removed none of it. The retirement condition
  for the runtime binding-namespace widening
  (`executor.bindMergedOuterLegs`, DIVERGENCES.md) therefore rests on THIS item
  alone on the read axis; CQ-67 advanced it by zero reads. DIVERGENCES.md now
  states that decomposition (60 closed / 102 open / 108 permanent — the open block
  was booked at 94 and re-measured; see CQ-68, which REFUTED its own typing
  premise), and CQ-67's
  gate (d) is documented as holding VACUOUSLY.

  The instrument that answers this is standing, not a probe: `legRebaseOrigin`
  threads the firing's class and refused-leg SHAPE to the leg-match arm, the cut
  is a partition assertion in the bakeability census, and
  `Step1ReconstructNilMustBeZero` is wired ON — so when this item moves the 94,
  the reads it removes are counted at the site rather than argued.

  **BEFORE STARTING: THIS ITEM CAN REOPEN CQ-67.** Typing the 94 result values is
  the most likely way to produce a reference that reaches a leg buried INSIDE the
  merge — which is the only thing that selects a NESTED sub-window, and therefore
  the trigger that makes RFC-200's gate (a) writable. CQ-67 merged with its
  nested reader arm correct and UNREACHED (`NESTED-HIT 0` corpus-wide), parked on
  an armed tripwire: `SeedWindowReaderFloors.NestedHitMustBeZero`. If this item
  trips it the suite goes RED with the full hand-over in the failure message, and
  **gate (a) becomes part of this item's work** rather than a surprise. Read
  CQ-67's REOPEN TRIGGER paragraph first; it names the fixture and the four
  mutation directions.

  Java types the flowed object value unconditionally
  (`Quantifier.java:801-803`). FOUR non-test `RecordQueryFlatMapPlan`
  constructions in Go can emit a bare untyped one:
  `rule_implement_nested_loop_join.go:3952`, fed by `:3901`'s `flatMapResult :=
  values.Value(values.NewQuantifiedObjectValue(mergedOuterCorr))`; and `:1383`
  (`buildCorrelatedFlatMapPlan`, passing its `resultValue` parameter through),
  `:1797` (`implementExistentialSelect`), `:4196` (`yieldExistsFlatMap`) — the
  latter three flowing `sel.GetResultValue()` essentially verbatim. A FIFTH site
  feeds them: `cascades_translator.go:4097`, where the SQL translator mints
  exactly a bare untyped QOV as a select result value. (Line numbers verified on
  `7ce79fe38`; RFC-200 cites the pre-#544 numbering.)

  **Two facts are explicitly UNMEASURED and are this item's FIRST deliverables,
  before any producer is touched:**

  1. **Producer attribution of the 94.** The census witness
     (`describeSeedEscape`) records the declined leg's result-value SHAPE, not
     its producer, so no line in the tree is currently known to emit them.
     Structural reading says NOT `:3901` — the 94 are legs of a 3-quantifier
     join+EXISTS, the accumulated side of an N-way join, which
     `yieldGeneralFlatMap`/`buildCorrelatedFlatMapPlan` build. That is a
     hypothesis and the item starts by making it a measurement.
  2. **Reachability of `correlatedStep1 && ordinalWindows != nil`.** RFC-200
     could not establish it BY READING. It matters because `:3901` and the
     FlatMap construction at `:3952` execute on **both** the correlated and the
     materialized arm — the `correlatedStep1` block only selects `step1Expr` —
     so typing them contacts the `correlatedStep1` wall, and that arm carries a
     documented two-revert history (a correlated FlatMap binds legs by NAME, and
     a baked ordinal against a name-keyed row context raises
     `values.BakedNameContextError`). Measure the wall contact before writing
     the conversion, not after it reds.

  **The two measured risks, which are why this is not a one-line typing sweep:**

  - **`GetFlowedObjectType`'s silent-untyped-member semantics.** Today an untyped
    member "cannot contradict a typed one — it reports nothing"
    (`expressions/quantifier.go:322-328`). Typed, such members PARTICIPATE and
    can raise `MemberResultTypeDisagreementError` (`:344-346`), which declines
    the whole partition-select collapse (`positional_merge.go:79-96`,
    `rule_partition_select.go:663`, `rule_partition_binary_select.go:246-256`)
    and the leg derivation — where `DisagreeingLegs` and `UnderivableLegs` are
    asserted as HARD ZEROS. A newly-typed member that disagrees with a sibling
    turns a currently-green census gate red. Typing also propagates into
    positional-merge slot types via `positional_merge.go:79`, moving
    `leg_layout_derivation_test.go:112-141`, which pins BOTH directions of that
    classification.
  - **The Java-verbatim form degenerates at the site that needs it.** Building
    the quantifier and taking `GetFlowedObjectValueTyped()`
    (`expressions/quantifier.go:577-586`) derives from `GetFlowedObjectType`, a
    reporting pass-through, so on the bare-QOV path it is a no-op: `mergedRowType`
    is non-nil exactly when `ordinalWindows` is, which on that path holds only
    when `sel.GetResultValue()` is ALREADY a pristine seed. A type IS derivable
    from `existLegRowTypes`, but only for the materialized arm and only by
    constructing a concat that does not exist at that site. So the port is not
    "call the typed constructor" — establishing WHERE the type comes from is part
    of the work.

  DONE = the five producers type their result value the way Java does, the two
  first deliverables are standing measurements rather than prose, the hard zeros
  and the leg-derivation classification pins are re-argued against their new
  populations (not relaxed), and the 94 `reconstruct-nil` declines are accounted
  for — either accepted or re-classified with the reason measured. Planner-wide
  typing change touching the executor's row contract: Graefe-gated RFC of its
  own, with the stress/golden comparison.

  **RE-VERIFIED AT `041838856`. Four corrections; the premise STRENGTHENS.**
  - **The number is 102, not 94.** The live gate is
    `sqldriver/embedded_fdb_test.go:253-266`: denominator 572, ACCEPT 160,
    `ReconstructNil` 102, `ReconstructNilBareQOV` 102, `ReconstructNilMerge` 0.
    The RFC-200 gate-(a) fixtures added 30 firings (`+8` reconstruct-nil, all
    bare-QOV) — attributed at `:236-252`. `102 > 60` holds a fortiori. The
    prose in `leg_local_bake_census.go:130,148` and DIVERGENCES.md's
    "60 closed / 94 open / 108 permanent" carry the stale 94 and need restating.
    `ReconstructNilMerge` is still hard 0, so CQ-67 held.
  - **All five producer sites confirmed live**, at rotated line numbers:
    `rule_implement_nested_loop_join.go` `:4124` (the one true MINT, was `:3901`),
    `:4175` (was `:3952`), `:1383` and `:1797` (unmoved), `:4419` (was `:4196`);
    `cascades_translator.go:4166` (was `:4097`). The file is at
    `pkg/recordlayer/query/plan/cascades/`, NOT under a `rules/` directory.
  - **"Type it at the FlatMap" is the WRONG PORT.** Java's
    `RecordQueryFlatMapPlan` does not construct a result value either — it stores
    what it is handed (`RecordQueryFlatMapPlan.java:91,100,205`), and all three
    constructions in `ImplementNestedLoopJoinRule.java:187,201,214` flow
    `selectExpression.getResultValue()` verbatim, exactly as Go's `:1383`/`:1797`/
    `:4419` do. The divergence is UPSTREAM: Java's select result value is built by
    `GraphExpansion.java:401` as `overQuantifier.getFlowedObjectValue()`, which is
    typed unconditionally. So the producer to fix is whatever fills
    `sel.GetResultValue()` — and Java's guarantee is structural, not disciplinary:
    `QuantifiedObjectValue.of` has NO untyped overload
    (`QuantifiedObjectValue.java:187`) and `getFlowedObjectType` is a
    `Verify.verify` + `requireNonNull` (`Quantifier.java:801-810`). Java cannot
    express Go's bare QOV at all.
  - **`describeSeedEscape` is not the classifier of the 102** — `classifyDeclinedLeg`
    (`fold_step1_seed_census.go`) is; deliverable (1) instruments that.
    Deliverable (2) is cheaper than booked: on the correlated arm
    `ordinalWindows != nil` iff `sel.GetResultValue()` is already a pristine
    ordinal seed, so the conjunction is structurally REACHABLE (not dead), and
    measuring it is one counter at the `ordinalSeedLegWindowsAcceptingNestedOf`
    call site.

  **One prerequisite defect is FIXED ahead of this item** (see the commit adding
  `quantifiedObjectValueIsTyped`): the witness printed `typed=%t` from
  `Typ != nil`, which no constructible QOV can make false — `NewQuantifiedObjectValue`
  stamps `UnknownType` and `NewQuantifiedObjectValueOfType` degrades nil to it, and
  `UnknownType` is a non-nil `*PrimitiveType`. So the instrument reported
  `typed=true` for all 102, and a typing sweep would have read as complete on the
  day it started. Pinned by
  `TestFoldStep1Census_BareQOVWitnessReportsAdmittedExactTypes` in both
  directions.

  ---

  **THE PREMISE IS REFUTED. THIS ITEM DOES NOT CONVERT AND MUST NOT BE
  ATTEMPTED AS WRITTEN.** Measured over the whole real-FDB sqldriver corpus at
  `6692ac268`; every number below is a run, not a reading.

  The item is stated as "the 102 FlatMap result values are a BARE UNTYPED QOV
  where Java types unconditionally" with DONE = "the five producers type their
  result value the way Java does". **The population is 100% TYPED, and typing
  cannot convert it in principle.** Four independent measurements, each pinned:

  1. **Every declined leg carries a real `RecordType`.** The outcome census's
     witness now spells the flowed TYPE rather than a boolean:
     `RecordType(2) ×30`, `RecordType(3) ×66`, `RecordType(1) ×4` on FlatMap
     legs, plus `RecordType(4) ×2` on a `RecordQueryNestedLoopJoinPlan` leg.
     Zero untyped. The prerequisite fix above made `typed=%t` honest; it did not
     make it INFORMATIVE, and `typed=true` is equally compatible with a real row
     and with a scalar. The arity is the fact. Pinned by
     `TestFlatMapProducerCensus_WitnessSpellsTheFlowedType` in both directions
     (RED when the arity spelling is removed: witness prints `rvtype=RECORD`).

  2. **Typing cannot convert a single one of them, structurally.**
     `legOrdinalSafety`'s FlatMap arm refuses on
     `!values.IsPositionalMergeRC(pl.GetResultValue())`, and
     `IsPositionalMergeRC` opens with a `*RecordConstructorValue` type
     assertion. A `QuantifiedObjectValue` fails it at any typing whatsoever.
     The residue is a SHAPE residue: the leg states one opaque row where the
     layout authority needs a positional merge. The code comment at that arm
     already said "bare identity QOV" — **`bare` there means IDENTITY
     PASS-THROUGH, not UNTYPED**, and reading it as untyped is where this item
     came from.

  3. **Producer attribution is now MEASURED, and confirms the item's own
     structural hypothesis while removing its fix.** All four non-test
     `RecordQueryFlatMapPlan` constructions are instrumented, and each declined
     leg is attributed by its result value's own IDENTITY (not by matching arity
     histograms, which closes only the arities unique to one site). Every
     FlatMap-legged decline reports `origin=buildCorrelatedFlatMapPlan`. That
     site emits **UNTYPED-QOV 0** across 25,644 constructions. (SUPERSEDED
     FIGURE, kept as the reading of the day: that total is a RULE-FIRING count
     and is not deterministic — later runs read 25,406 and 25,048. The
     **UNTYPED-QOV 0** is the claim and it has held on every run.) The two NLJ-legged
     declines report `origin=UNRECORDED`, correctly — an NLJ result value never
     passes a FlatMap constructor. Pinned by
     `TestFlatMapProducerCensus_OriginIsMeasuredNotInferred` (three directions:
     attributed / ambiguous / unrecorded).

  4. **Java is not diverged from on this population.** Java's select result
     value is `overQuantifier.getFlowedObjectValue()` — a TYPED QOV
     (`GraphExpansion.java:401`, `Quantifier.java:801-803`). Go's is also a typed
     QOV here. Java's `legOrdinalSafety` equivalent does not exist because Java
     has no ordinal seed; the shape gap is Go-side architecture, not a port gap.

  **WHAT SURVIVES, RELOCATED — a real Java divergence on a DIFFERENT
  population.** Three of the four sites DO hand FlatMap an untyped QOV:
  `implementExistentialSelect` **1,685**, `yieldExistsFlatMap` **269**,
  `implementJoinWithExistential` (the `:4124` mint) **249**. (SUPERSEDED FIGURES,
  kept as the reading of the day, and superseded TWICE over: these are
  rule-firing counts and are not deterministic — `implementExistentialSelect`
  later read 1,609 — and the ATTRIBUTION is wrong besides. The first two sites
  flow `sel.GetResultValue()` verbatim and build nothing; the author is the
  translator mint, measured at 1,086. See CQ-96, which owns the corrected
  population.) Java cannot express
  any of them — `QuantifiedObjectValue.of` has no untyped overload
  (`QuantifiedObjectValue.java:187`), `Quantifier.getFlowedObjectType` is a
  `Verify.verify` + `requireNonNull` (`Quantifier.java:801-810`). **None of those
  values reaches the decline classifier**, so typing them moves this item's 102
  by ZERO and moves DIVERGENCES.md's 174 reads by zero. It is floored in the
  producer census (a floor on a DIVERGENCE — the drop direction fails too,
  because a shrinking count cannot be told from a darkening site) so the
  population stays counted while it stands.

  A fifth divergence was found in passing and was NOT actioned here:
  `RecordQueryFlatMapPlan.GetResultType()` returned `values.UnknownType`
  unconditionally, where Java derives it from the result value
  (`RelationalExpression.java:195-196`). CLOSED SINCE by RFC-232 -- flat_map.go
  now returns `p.resultValue.Type()`; see CQ-97. Kept as the record of what was
  found, not as a live divergence.

  **THE CQ-67 REOPEN TRIGGER DID NOT FIRE.** `NestedHitMustBeZero` stayed armed
  and green across every corpus run here. No conversion was made, so no reference
  reaching a leg inside the merge was produced. CQ-67 stays parked exactly as it
  was; gate (a) is not this item's work after all.

  **THE CENSUS EQUALITIES DID NOT MOVE.** denominator 572, ACCEPT 160,
  correlatedStep1 108, rv-no-exist-ref 202, reconstruct-nil 102, bare-QOV 102,
  merge 0 — identical before and after, as they must be for a change that adds
  only instruments. `ReconstructNilMerge` still hard 0, so CQ-67 still holds.

  **ONE OF THIS WORK'S OWN ASSERTIONS WAS REFUTED BY ITS FIRST RUN**, which is
  the reason it is recorded rather than quietly corrected: the producer census
  was written asserting UNTYPED-QOV == 0 at EVERY site, on the reading that Go
  should never build what Java cannot express. Three sites failed it immediately,
  in bulk. The blanket zero was a wish; the zero that is actually defended is the
  one at the residue's own producer, and it is a sharper claim than the one it
  replaced.

  **WHAT REPLACES THIS ITEM.** The residue is convertible only by giving the
  declined leg a positional-merge result value — i.e. by making
  `buildCorrelatedFlatMapPlan`'s accumulated-inner leg flow
  `RC(_i: QOV(leg_i))` where it currently flows the identity QOV. That is a
  different change with a different risk surface from the one booked here, it
  contacts the same `correlatedStep1` wall (108, permanent), and it needs its own
  RFC and Graefe ACK. **Do not re-open this item by re-reading `bare` as
  `untyped`** — the producer census's zero exists to make that re-reading go red.

  </details>

- [x] **CQ-97 (MED, query-engine — needs its own RFC + Graefe ACK):
  `RecordQueryFlatMapPlan.GetResultType()` returns `values.UnknownType`
  unconditionally where Java derives it from the result value.** Found while
  refuting CQ-68 and explicitly not attempted there; it is a planner-wide typing
  change with its own blast radius, and stating it in a closing paragraph is how
  a live divergence becomes invisible.
  
  **DONE — closed by RFC-232; found still open during a #762 sweep.**
  `plans/flat_map.go` `GetResultType()` returns `p.resultValue.Type()`, which is
  the derivation from the result value this entry asked for. The text below is the
  ORIGINAL finding, kept as the record of what was wrong.

  **Clause 4 (the re-run count) was NOT done when this box was first ticked,
  and is done here.** The entry itself demands a re-count, so the box could
  not close on the derivation alone. Re-measured with the entry's own recipe:

      grep -rn "\.GetResultType()" --include="*.go" pkg/ | grep -v _test | wc -l
      -> 43

  Booked at 33, re-measured at 50, and 43 today. The command is written out
  rather than the number asserted, because that is the only form of this claim
  anyone can check. NOTE for whoever re-runs it: keep `--include` QUOTED, and do
  not port the diagnosis -- what decides it is the GLOB MODE, not the shell.
  Where an unmatched wildcard is passed through literally (bash by default) grep
  runs and the count is TRUE. Where it is REJECTED (zsh here, fish, and bash
  under `failglob`) the command never runs at all: "no matches found" goes to
  STDERR and a pipeline capturing only stdout sees nothing and reports 0. Do not
  try to identify the shell either -- `echo $0` is empty under fish. Settle any
  zero the way the rest of this repo does, with a positive control in the same
  invocation.

  **Go (AS FOUND; closed since by RFC-232):** `plans/flat_map.go` returned the
  `UnknownType` singleton with no reference to the plan's result value.
  **Java:**
  `RelationalExpression.java:195-196` derives the result type from the result
  value, so a `RecordQueryFlatMapPlan` states the row it actually flows.

  **A STANDING WORKAROUND IS ALREADY BUILT ON THE DIVERGENCE, and it is what
  makes this a booking rather than a note.** `rule_implement_nested_loop_join.go`
  says at its FlatMap arm that "the PLAN WALK is the route rather than a
  preference: `RecordQueryFlatMapPlan.GetResultType()` returns
  `values.UnknownType`, exactly as `RecordQueryNestedLoopJoinPlan`'s does, which
  is why the reconstruction's own comment says GetResultType cannot be used for a
  join leg" — i.e. `planBuriedLegConcat` walks the plan to its scan leaves
  precisely because the type it should be able to ask for is a stub. A comment
  describing a substitute for a capability Java has is a standing admission that
  the capability is the real answer.

  **THE BLAST RADIUS WAS BOOKED AT 33 AND IS 50. RE-MEASURED HERE, and re-count
  it again before sizing** — it is a point measurement with nothing keeping it
  true, which is the failure mode the census family on this path exists to
  prevent. `grep -rn "\.GetResultType()" --include="*.go" pkg/ | grep -v _test`
  → **50** non-test call sites.

  The cut that matters is not the total. **21 of the 50 are one-line FORWARDERS
  in `plans/`** — `filter.go:58`, `distinct.go:107`, `typefilter.go:75`,
  `union.go:61`, `in_join.go:121` and the rest all being `return
  inner.GetResultType()` or its quantifier-mediated twin. Those do not read a
  FlatMap type; they PROPAGATE whatever their child states, so today they
  propagate the stub and after the fix they propagate the real row, with no
  change of their own. The sites that decide something on the answer are the
  remaining 29, concentrated in `cascades/` (6 in
  `rule_push_set_operation_through_fetch.go`, 4 in
  `rule_implement_nested_loop_join.go`) and 4 in `query/executor/`. That is where
  the RFC's argument has to be made, and it is a much smaller surface than either
  the booked 33 or the measured 50 suggests.

  DONE = `RecordQueryFlatMapPlan.GetResultType` derives from its result value as
  Java does, `RecordQueryNestedLoopJoinPlan`'s equivalent stub is resolved or
  argued in the same RFC, the `planBuriedLegConcat` walk is either retired or its
  comment restated as a deliberate choice rather than a workaround, and the
  affected call sites are enumerated from a re-run count.

- [x] **CQ-71 (HIGH/L, L, DDL + wire-compat, query-engine review gate) — the
  VALUE index written in materialized-view syntax (`CREATE INDEX x AS SELECT
  cols FROM t [ORDER BY cols]`) is unimplemented.** Measured by the RFC-201
  Phase-1 corpus run (CQ-69.1): **42 of 238 vendored files — the single largest
  blocker in the corpus, larger than struct types.** Go's
  `parseIndexDefinition` routes every `IndexAsSelectDefinitionContext` to
  `parseAggregateIndexDefinition` (`pkg/relational/core/embedded/ddl.go:236`),
  which recognises only an aggregate select element and a `CARDINALITY(...)`
  value index. A plain `select col from t order by col` therefore dies with
  `42F59: select element must contain an aggregate function`, and the
  multi-column form with `exactly one aggregate expression required in SELECT`.

  Reproducers, all vendored and already counted as
  `unsupported-DDL:value-index-as-select` in the Phase-1 ledger — the smallest
  is `documentation-queries/in-operator-queries.yamsql` (`create index price_idx
  as select price from products order by price`); `union.yamsql` and
  `index-ddl-values-only.yamsql` carry the multi-column and multi-table shapes.

  **NOT a small fix, and the reason is wire compat, not size.** The index this
  DDL creates must have the same key expression Java's
  `MaterializedViewIndexGenerator` emits, or Go and Java write DIFFERENT index
  entries for the same declaration — a divergence on the hard line, in the one
  direction that corrupts a shared cluster silently rather than loudly. So it
  needs Java read first (which of SELECT order vs ORDER BY order fixes the key,
  how ASC/DESC and expression elements are folded, what happens to columns that
  appear in one and not the other), an RFC, and a Graefe ACK before
  implementation. `Builder.AddIndex(table, name, cols, unique)` already exists,
  so the *implementation* is likely modest; the specification is the work.

  DONE = the 42 files move out of `unsupported-DDL:value-index-as-select` in the
  CQ-69.1 ledger, the index key expression is asserted byte-identical to Java's
  for each corpus shape, and the pinned ledger is updated in the same commit.

  PROGRESS (RFC-202 S1-S3 landed): the value/version AND aggregate arms of
  the generator are live — `pkg/relational/core/query/ddl` consumes the
  logical plan for every AS-SELECT form; `parseAggregateIndexDefinition` and
  its parse-tree extractors are DELETED. The
  `unsupported-DDL:value-index-as-select` class (once 42 files, the corpus's
  largest) is DELETED from the ledger: corpus pass 32 → 46 (S2) → 47 (S3,
  composite-aggregates passes), fail stays 0. S3 also closed, measured and
  pinned: the COUNT GroupBy(EmptyKey(), …) stored-metadata byte divergence
  (GroupAll — no Empty child, Java
  MaterializedViewIndexGenerator.java:408-412), LEGACY_EXTREMUM_EVER honored
  (min/max_ever_long), UNIQUE on aggregate AS-SELECT honored, permuted
  min/max with ORDER-BY-derived permutedSize, bitmap_construct_agg index
  metadata, MIN_EVER/MAX_EVER/BITMAP_CONSTRUCT_AGG recognized by the
  aggregate front end, BITMAP_BUCKET_OFFSET/BITMAP_BIT_POSITION scalar
  functions (walker injects Java's 10000 entry-size literal), and Java's
  INSERT column-list reordering semantics
  (parseRecordFieldsUnderReorderings — unknown named columns silently
  ignored; armed by composite-aggregates' setup the moment its DDL passed).
  bitmap-aggregate-index.yamsql is booked engine-gap:planner-declines (the
  bitmap aggregate QUERY surface, out of RFC-202 scope);
  index-ddl-aggregates-only.yamsql waits on CREATE VIEW
  (unsupported-DDL:other).

  S4 landed `__ROW_VERSION` (the version-storing catalog, the VERSION index
  arm, the runtime read; engine-gap:row-version-pseudocolumn deleted). S5
  landed the predicate/sparse arm: getTopLevelPredicate ported end to end —
  DNF normalization WITHOUT simplification (Java normalize(), not
  normalizeAndSimplify: absorption is wire-visible), IndexPredicate.isSupported,
  the dnf-yields-no-ranges fallback to the conjunction, and the
  IndexPredicate/IndexComparison PoJo serialization with Java's Integer
  narrowing at the literal boundary (ParseHelpers.parseDecimal) — plus the
  RangeConstraints canBeUsedInScanPrefix admission gate the Go builder was
  missing. Corpus pass 47 → 52, fail 0. Fixed in the same stage, each
  mutation-checked RED: the planner treating a sparse index as full (the
  candidate now carries its predicate — ValueIndexExpansionVisitor.java:138-162's
  conservative arm; candidatePreservesBaseRecordCardinality blocks the
  compensation-free shortcuts; adjustMatchForSelect bails on non-tautology
  constants per SelectExpression.java:617-620 — boolean-ddl's empty WHERE NULL
  index was served as the whole table); the generator rendering FOLDED
  display names into stored metadata for quoted-DDL columns (storageNames:
  descriptor-by-ordinal, both arms + predicate paths); and the USING-join
  desugar re-folding normalized quoted aliases (join-tests-outer). Booked
  with pinned signatures: USE INDEX hints parsed-and-ignored
  (sparse-index-tests → go-accepts; Java threads AccessHints,
  QueryVisitor.java:679-681 — closing it = porting AccessHints into the
  candidate matcher), ORDER BY on a LEFT JOIN's null-extended side
  (join-tests-outer → go-accepts; Go's sanctioned in-memory-sort fallback
  answers what Java 0AF00s), recursive-cte progressed to
  engine-gap:nested-recursive-with. NOT ported (negative pin
  TestSparseIndexCandidate_ImpliedQueryFallsBack names the re-arm): Java's
  placeholder extra-ranges arm (:146-158) that lets an IMPLIED query still
  match a sparse index — Go conservatively falls back to a scan (correct
  rows, narrower reach; sparse-index-tests' COVERING explains are the
  witness).

  S6 landed the ON-source front end: `CREATE INDEX i ON t(cols…) INCLUDE
  (cols…)` is a FRONT END over the same generator
  (OnSourceIndexGenerator.java:172-229 — projection = keys ++ (INCLUDE \
  keys), ORDER BY from the per-column order clauses, delegate). The S0
  fail-closed ON-source orderClause and INCLUDE rejections retired; wire
  gate TestFDB_OnSourceIndex_WireEntries pins the covering key/value split
  and the DESC column's hand-derived TupleOrdering bytes; the twin test
  pins byte-identical roots for the two DDL forms.
  documentation-queries/index-documentation-queries PASSES (52→53); the
  three index-ddl* files progressed to CREATE VIEW (unsupported-DDL:other).

  S7 landed planner D10, both live defects: the KeyWithValue split now
  bounds the sargable/ordering surface (key part) while the VALUE part
  covers (candidate valueColumnNames → plan → executor reads the entry
  VALUE tuple; the wrong-column-set defect pinned from Java-authored proto
  bytes), and order-function columns carry ToOrderedBytesValue with the
  wrapper's direction (createsDuplicates delegates through the wrapper —
  Java's OrderFunctionKeyExpression override :83-86). EXPLAIN-pinned in
  rfc202_generated_index_plans.yaml + rfc202_version_index_ordered.yaml:
  covering INCLUDE scan with NO fetch, ORDER BY DESC as the DESC index's
  forward scan (ASC as its reverse) with no InMemorySort, WHERE on the
  wrapped column NOT claiming the encoded index, VERSION index COVERING for
  the ordered pseudo-field query, and twin-plan EXPLAIN equality. Still
  open (measured, results-only pin in row_version_index_plan_fdb_test.go):
  the unfiltered-unordered covering preference (Go Scan, Java COVERING) — a
  data-access enumeration gap independent of D10.

  S8 landed D11 at the STORED bytes: the live JVM persists each shape into
  the shared catalog and Go compares the raw MetaData proto per index —
  root_expression, type, OPTIONS, PREDICATE
  (conformance/index_ddl_metadata_conformance_test.go; the summary route
  gained options+predicate too). It caught and fixed TWO wire divergences:
  Go omitted `unique=false` (Java's setUnique stores both polarities,
  RecordLayerIndex.java:216-218), and the sparse comparand's stored type
  follows the COMPARISON's promoted type — the COLUMN's — refuting S5's
  ParseHelpers-narrowing model (BIGINT long_value / INTEGER int_value /
  DOUBLE double_value; a strictly-wider literal is Java's promote(col)
  shape and REJECTS in both engines — measured live). All 6 shapes
  byte-identical across engines.

  S9: ledger stands at pass=53 fail=0 skip=185 (re-pinned in S6; S7/S8
  moved no files), full conformance_java suite green. DONE: the
  value-index-as-select class is deleted, every corpus shape's stored
  metadata is asserted byte-identical against the live JVM, and the
  generated indexes are PLANNABLE with EXPLAIN pins. Booked elsewhere, not
  regressions: AccessHints/USE INDEX (signature-pinned),
  nested-recursive-with, CREATE VIEW (holds the three index-ddl* files),
  the sparse implied-query placeholder ranges (negative pin), and the
  unfiltered covering preference (results-only pin).

- [x] **CQ-80 (MED, test-contract) — four watch-list entries claim a pin that
  does not exist or cannot fail. DONE — all four closed, two of them REFUTED
  by the measurement.** · S/M
  `road-to-prod.md`'s watch-list states its own contract: *"Every entry is a
  committed test asserting CURRENT behavior; red means fixed."* Verifying that
  contract found four entries that break it. They are MARKED in the watch-list
  rather than removed — an unpinned divergence is more dangerous than a pinned
  one — and the code fixes are this item.
  - [x] **Entry 4, `CURRENT_TIMESTAMP` drifts across rows within one statement
    (SELECT path) — DONE.** Fixed via the "SELECT-path CURRENT_* statement-
    stability" item above (statement clock stamped on the executor
    EvaluationContext, clock-bearing frontier wrap). Pinned red→green by
    `sqldriver/current_timestamp_stability_test.go`; the watch-list entry is
    retired to RESOLVED.
  - [x] **Entry 2, INSERT of NULL into a PRIMARY KEY column silently stores 0 —
    DONE, and the divergence itself REFUTED.** Measurement showed Go already
    stores Java's tuple null (duplicate-NULL collides 23505; `id=0` does NOT
    collide; the row reads back NULL and is visible via IS NULL) — Java raises
    no error for this shape (DdlVisitor.java:156-161,
    ExpressionVisitor.java:1053-1075, functions.yamsql:34). The
    log-and-return fake pin is deleted; the real Java-parity pins live in
    `sqldriver/null_pk_java_parity_test.go`, red under both the stores-0 and
    the rejects-NULL mutations. The watch-list entry is retired to REFUTED.
  - [x] **Entry 8, `UNION ALL` + trailing `ORDER BY` — DONE, divergence
    CONFIRMED.** Go's side was already pinned; the Java side rested on a prose
    record of a live probe in `DIVERGENCES.md`. Now measured every run by
    `conformance/union_trailing_orderby_java_probe_test.go`: Java orders the
    right leg only, Go the combined result. *Measured while pinning:* Java's
    `UNION ALL` does not concatenate its legs in a fixed order (`[1 6 2 5]` one
    run, `[2 1 6 5]` the next), so the pin asserts PER-LEG subsequences — each
    leg is a PK scan and its own relative order is stable. The
    "Java did not sort the union" check is restricted to fixtures where no
    legal right-leg-only interleaving can equal the combined sort (DESC over PK
    legs; ASC over a non-key column whose natural order descends). ASC over PK
    legs structurally cannot carry that claim and is marked as such.
  - [x] **Entry 12, quoted DDL column unreferenceable by name — DONE, and the
    divergence REFUTED AND INVERTED.** `SELECT "KeepCase"` returns the value on
    Go exactly as on Java, in projection and in a predicate — the column IS
    referenceable. The real divergence runs the OTHER way: Go over-resolves
    (`KeepCase`, `"KEEPCASE"`, `"keepcase"` all bind) where Java raises 42703
    for every spelling but the exact one. Pinned by
    `conformance/quoted_identifier_case_java_probe_test.go`; wire compat
    measured intact by
    `pkg/relational/core/embedded/ddl_quoted_case_wire_test.go` (the stored
    descriptor keeps `KeepCase` off the real `CREATE TABLE` path). See the
    identifier-divergence item above, rewritten to the measured direction.
  **Booked 2026-08-01.** Deliberately not fixed in the pass that found them:
  that pass was docs-only, and four test fixes across three packages are a code
  lap with its own review. Recorded here in full so the next fixer does not have
  to re-derive which four and why. **Entries 2 and 4 landed 2026-08-01; entries
  8 and 12 landed 2026-08-05.** Two of the four (2 and 12) were REFUTED by the
  act of measuring them — which is the argument for the section contract rather
  than an embarrassment to it. Every entry now cites the pin that would red if
  its behaviour changed.
  DONE = each of the four has a committed test that goes RED with the
  divergence present, per "EVERY PROOF GETS COMMITTED AS A TEST"; entry 12's
  mixed-case residue is MEASURED before it is pinned (it may be closed already);
  and the watch-list's ⚠ markers are removed as each lands.

- [x] **CQ-81 (HIGH, red master) — two green PRs made master red: #555's new
  nightly workflow did not satisfy #556's new window gate.** · **FIXED in the B6
  pass.**
  `nightly-factory.yml` (added by #555) declared both of its window gates in the
  OLD inlined `[ "$HOUR" -ge 0 ] && [ "$HOUR" -lt 10 ]` form. #556 landed
  `TestNightlyWindowAdmitsMeasuredLandings` and
  `TestNightlyWindowGateShellMatchesDeclaredBand`, which require every band to be
  declared as machine-readable `WINDOW_START`/`WINDOW_END`. Neither PR's CI ran
  the other's files, so both were green and `a1d281a63` was RED.
  **Not cosmetic.** The band those two gates carried is the exact one #556
  measured as discarding evening landings as "daytime" and throwing away a 10:22
  allocation for being 22 minutes late — so the newest safety net had been wired
  with the fake-green shape that hid a reproducible planner fault for twelve
  days. A brand-new net inheriting the defect the old nets were just cured of is
  the most expensive kind of regression, because it looks like coverage.
  FIX: both gates converted to the wrapping 18:00–12:00 band used by every other
  `hetzner-fdb` job; `measuredLandings` entries added for
  `nightly-factory.yml/corpus` and `/batch`, **pooled from the shared runner
  queue and labeled as pooled** rather than passed off as these jobs' own history
  (all eleven windowed jobs run on one self-hosted slot, so the allocation hour
  is a property of the queue, not the job — and the oracles' distinct afternoon
  hour is deliberately excluded); the two no-op guards tightened `9 → 11`, since
  at 9 they would have tolerated silently losing the two new jobs.
  Mutation-checked: reverting the workflow hunk alone turns both tests RED.
  **Residual for whoever next touches these entries:** replace the pooled hours
  with each job's OWN `started_at` hours once it has scheduled runs. The pooled
  prior is honest but weaker than the rest of that map, and it says so in place.
  **The generalizable finding, worth more than the fix:** the merge queue does
  not run a gate one PR adds against the files a concurrent PR adds. Two green
  PRs, one red master. Nothing in CI covers that axis today.

- [x] **CQ-83 (HIGH, wrong rows) — DONE 2026-08-01, awaiting Graefe/Torvalds
  lap (branch `fix/rfc196-correlated-zero-composite`) — the correlated half of
  CQ-28: a correlated float `=` on a NON-terminal composite index column missed
  `-0.0`/`+0.0`** (road-to-prod watch-list entry 3; RFC-196's "known gap,
  deliberately uncovered"). `t.v = o.k AND t.w = 5` against index `(v, w)`
  probed only the outer key's own signed zero: the comparand is opaque at match
  time, so CQ-28's constant-only prefix termination never applied, and the
  wanted key set {(-0.0,5), (+0.0,5)} is not a contiguous interval, so neither
  a single probe nor a widened one can serve it.
  **Mechanism — execution-time probe split (executor, no plan change).**
  (SYMBOLS AS OF LANDING — every name in this paragraph has since been
  replaced. `scanComparisonsToTupleRange`/`...Ranges` were retired (RFC-217);
  the live binder is `bindScanComparisonsToRangeSet`
  (`scan_range_binding.go:256`), a `zeroFork` is now a per-component
  `alternatives` pair emitted at `scan_range_binding.go:390-406`, and
  `multiRangeScanCursor` is now `scanRangeSetCursor`
  (`scan_range_set_cursor.go:200`). The unit pins moved from
  `zero_fork_scan_ranges_test.go` to `scan_range_binding_test.go` /
  `scan_range_set_cursor_test.go`; the sentinel
  `correlated_zero_composite_sentinel_test.go` kept its name. The behaviour
  described below is unchanged.)
  The binder already evaluates the correlated comparand
  per-probe with the value in hand; the only missing piece was that one
  `TupleRange` cannot express the two-key union. In the live code the binder
  records, for each non-terminal zero-float equality, a two-element
  `alternatives` pair on that component (`scan_range_binding.go:390-406`);
  the choices expand to one range per signed-zero combination (2^k for k such
  components; on a FORWARD scan the alternatives are emitted in ascending key
  order, and on a REVERSE scan descending — the `if reverse` branch emits
  `+0, -0` at `scan_range_binding.go:402-403` — so odometer order tracks scan
  direction). `scanRangeSetCursor` walks them as a LAZY
  mixed-radix odometer (`scan_range_set_cursor.go:405`,
  `advanceScanRangeChoices`) — one child cursor open at a time, advanced only
  on `SourceExhausted` (`:307`) — rather than materializing a concatenated
  chain, and wraps each child's continuation so resume is exact. The WRAPPER is
  **Go-only** and deliberately so: `ScanRangeSetContinuation`
  (`proto/relational/scan_range_set_continuation.proto`, RFC-208 §3) is a
  Go-internal read-side execution detail that Java has no counterpart for, which
  is why it lives in the relational proto set and not the Apple mirror. Only its
  embedded `inner_continuation` — the active physical leaf scan cursor's own
  token, carried through opaquely — is the Java-compatible part. Nothing on the
  wire-compat hard line changes: no stored key, no record/index format, and no
  continuation format shared with the Java Record Layer. Disjoint ranges make
  the union duplicate-free with NO dedup layer — the IN-rewrite direction
  CQ-28 twice reverted stays dead. The full composite prefix stays sarg'd:
  no de-sarg, no residual filter, plan shape unchanged (EXPLAIN-pinned).
  Wired into all FOUR scan-comparison consumers: PK scan
  (`executor.go:268`), index scan (`:383`), vector-index partition prefix
  (`:655`), aggregate index scan incl. permuted (`executor_new_plans.go:105`). Terminal-zero widening (CQ-27) and
  constant-zero match-time termination (CQ-28) are untouched.
  **Java citation.** Java needs none of this: `Comparisons.compareEquals`
  (Comparisons.java:241-247) → `toClassWithRealEquals(...).equals(...)` →
  `Double.equals` (bit identity), so a Java probe matches exactly one signed
  key and single-range `ScanComparisons.toTupleRange` is self-consistent. Go's
  `=` is ruled IEEE (DIVERGENCES.md "Java's float `=` is bit identity"; the
  CQ-28 RULING 2026-07-28), so Go must return both zeros' rows.
  **Pins.** `correlated_zero_composite_sentinel_test.go` fired on its own
  terms and now asserts the CORRECT rows: both correlation directions
  (+0.0 outer ↔ stored -0.0 and vice versa), exact multiset (duplicates
  fail), in-between guard rows on BOTH sides (naive widening fails), terminal
  and constant-zero controls, correlated-nonzero control, unindexed-table
  residual-path agreement (sargability differential principle), and an
  EXPLAIN pin that the composite index probe survives. Unit pins (originally
  in `zero_fork_scan_ranges_test.go`, since split across two files as the
  symbols moved): alternatives expansion — order, signs, float32 typing,
  cartesian 2-component, reverse order — in `scan_range_binding_test.go`
  (`TestBindScanComparisonsToRangeSet_NonTerminalFloatingZero:42`,
  `_ReverseZeroOrder:108`, `_TwoZerosCartesianWithoutMaterialization:135`),
  terminal-stays-single in `_TerminalZeroUsesOneInclusiveRange:175`; and
  `scanRangeSetCursor` odometer ordering plus continuation resume at every stop
  point in `scan_range_set_cursor_test.go`
  (`...OdometerOrderAndSingleLiveChild:258`, `...ContinuationSweep:496`) —
  those two construct `recordlayer.ForwardScan()`, so they pin the FORWARD
  odometer only. The file DOES have a reverse test — `:879` builds
  `ScanProperties{Reverse: true}`, passes it at `:893` and asserts propagation at
  `:908-909` — but that checks the property reaches the leaf, not the odometer's
  ordering. (An earlier draft said "every `newScanRangeSetCursor` call in that
  file" is forward; that was a `grep -c ReverseScan` result, and the struct
  literal at `:879` does not contain that token. A grep proving a negative is
  only as good as the spellings it knows.)
  The reverse direction is covered where the direction is actually decided, in
  the binder: `_ReverseZeroOrder:108` above passes `reverse=true` and asserts the
  alternatives come out `+0` then `-0`.
  One claim from the original list is NOT carried over and has no
  replacement: "single-range projection errors loudly on forks" pinned a
  single-range projection helper that no longer exists (the range SET is now
  the only shape a consumer receives), so there is nothing left to error on
  and nothing covers it.
  **Mutation-verified, three directions, all quoted RED:** fork disabled →
  `returned [5], want [1 5]` / `returned [1], want [1 5]`; naive contiguous
  widening → `returned [1 3 4 5], want [1 5]`; overlapping probes →
  `returned [1 1], want [1 5]`.
  **Residue, deliberately NOT lifted here:** the rowdiff/sarg-oracle
  generators' safety invariant (no FLOAT/DOUBLE in a non-terminal
  composite-index position) no longer guards a Go bug; lifting it means
  extending the cross-engine corpus where Java's bit-identity `=` diverges by
  design, i.e. new divergence classifications — its own corpus lap. Their
  comments now say so.

- [ ] **CQ-88 (MED/L, cost model — statistics source, RFC-204 fallout) — port
  the Java-sanctioned per-type statistics source: COUNT-type index reads +
  `CardinalitiesProperty`.** RFC-204 P1 removed the record count key from
  relational templates (the stored bytes must match Java's
  `RecordMetadataSerializer`, which never sets one; Java core marks
  `getRecordCountKey` `@API(DEPRECATED)`, superseded by COUNT-type indexes).
  That made `fetchTableStatistics` (cascades_generator.go) dead on every
  SQL-created schema: the cost model now plans on its 1e6 default for every
  table. Measured consequences, each pinned with a re-arm comment:
  `multiway_join_order_probe_test.go` no longer proves driving from the 1-row
  table (relaxed to "not from the 200-row one");
  `pkg/simfdb/hunt/golden/testdata/joins.golden` re-blessed with the flipped
  join order; `statistics_fdb_test.go` renamed to
  `TestFDB_DefaultStatisticsPlanSelection` (it proves size-independent index
  selection, NOT a stats pipeline). The replacement, exactly as Java does it:
  (a) read per-type row counts from COUNT-type aggregate indexes when the
  schema declares them (`IndexTypes.COUNT`, grouped by record type);
  (b) port `CardinalitiesProperty` (fdb-record-layer-core
  query/plan/cascades/properties/CardinalitiesProperty.java) so the planner
  derives cardinality bounds structurally where no index exists.
  DONE = the join-order probe's exact-driver assertion is restored
  (drive from the 1-row table) with a COUNT index declared in its DDL, and
  the joins golden decision is re-derived from real counts.

- [x] **CQ-90 (on-disk migration for the CQ-89 cardinality key change) —
  CLOSED: UNREACHABLE IN PRODUCTION, on fresh-store grounds.**
  Not closed because a migration was performed or even proven — closed because
  the hazard cannot occur in production. The product is pre-release, so every
  production store is created fresh by a post-CQ-89 binary, and a stale `0x00`
  cardinality entry can only exist on a store some PRE-fix binary wrote. The
  conditional go/no-go this item booked is therefore moot: there is no fleet to
  migrate.
  **Where the hazard does survive**, and where the recipe still applies:
  carried-forward dev/test data, and external adopters who ran a pre-CQ-89 Go
  binary. The recipe now lives as a reference appendix in `road-to-prod.md`
  rather than as a pending production action, and watch-list entry 18 is
  RETIRED on these grounds.
  **The premise, stated so it can be re-checked:** this rests entirely on "no
  production store predates CQ-89". Promote a pre-fix store into production —
  a restored dev backup, a carried-forward cluster, an adopter's existing data
  — and the hazard is LIVE again and the appendix recipe becomes mandatory for
  its cardinality indexes. That is a deployment fact, not a code fact, which is
  why it is written down rather than inferred.
  **WHAT THIS ITEM ACTUALLY DELIVERED**, and why the tests stay: engine
  behaviour on the path EVERY future index addition takes under a live tenant.
  (a) The inline rebuild's uniqueness semantics, wrong in Go in two opposite
  directions before they were right (below). (b) `StoreBuilder.SetFormatVersion`
  — Java's rolling-upgrade capability Go lacked entirely — plus the per-feature
  format gates that pinning made reachable, which are WIRE-COMPAT fixes.
  (c) Pins on the DISABLED / `OnlineIndexer` / clear-on-disable /
  whole-schema-bump behaviours. None of that is migration tooling and none of it
  retires with entry 18. Pinned by
  `pkg/relational/sqldriver/cardinality_stale_key_rebuild_fdb_test.go` and the
  `StoreBuilder_FormatVersion` specs.

  **NO automatic engine-side bump — decided by reading Java, not by punting.**
  Java has NO mechanism that force-rebuilds an index because the library's own
  key derivation changed between releases. The trigger is purely
  meta-data-driven (`checkVersion` → `getIndexesToBuildSince(oldMetaData
  Version)` → each index's `lastModifiedVersion`), and `FormatVersion.java:65-66`
  states it while contrasting the record-count key, which IS store-header-
  detected: "Unlike indexes, the RecordCountKey does not have a
  lastModifiedVersion, and thus the store detects that the counts need to be
  rebuilt by checking the key in the StoreHeader." The contract that IS
  documented is aimed at schema authors (`Index.java:614-619`): "Any record
  store older than this will need to have the index rebuilt." A Go-only
  auto-bump would make Go emit metadata bytes Java's builder never produces —
  a wire divergence in the exact direction CQ-89 set out to remove. So the
  engine-side deliverable is the rebuild path proven for THIS migration, plus
  the recipe.

  **THE RECIPE.** Raise the CARDINALITY index's `LastModifiedVersion` past the
  stored metadata version and raise the metadata version to match. Leave
  `AddedVersion` ALONE — `MetaDataEvolutionValidator` requires it unchanged
  (`metadata_evolution_validator.go:482-487`; Java
  `MetaDataEvolutionValidator.java:633-637`, the
  `oldIndex.getAddedVersion() != newIndex.getAddedVersion()` check in
  `validateIndex`; NOT `:543-546`, which is the separate new-index branch) and
  only `LastModifiedVersion` drives the rebuild. Bump ONLY the affected index:
  every index in
  `indexesToBuild` is excluded from the record-count sources that pick the
  policy (Java's `IndexQueryabilityFilter`, `FDBRecordStore.java:4841`), so
  bumping the whole schema drags any COUNT index along with it, the count
  degrades to `MaxInt64`, and every index lands DISABLED regardless of how
  small the store is. Next store open, `checkVersion` reconciles.

  **MEASURED, and one of these corrects a common assumption.**
  - ≤ `MAX_RECORDS_FOR_REBUILD` (200): rebuilt INLINE in the open transaction,
    stale entries gone, unaffected cardinalities untouched.
  - \> 200: left DISABLED — and a DISABLED index is not scannable, so nothing
    can read stale answers from it in the window before the backfill. An
    explicit `OnlineIndexer` run completes it and returns it to READABLE.
  - **The inline arm needs a COUNT index to be reachable at all.** Without one,
    `getRecordCountForRebuildPolicy` has no cheap count, falls through to an
    emptiness probe and reports `MaxInt64` for ANY non-empty store — so a
    three-record store still lands DISABLED. Relational SQL schemas declare no
    COUNT index today, which makes DISABLED + `OnlineIndexer` the path they
    all take. The test declares one deliberately to reach the inline arm.

  **The uniqueness sub-hazard leaves the index UNUSABLE, not the store — and
  getting there took two wrong answers, both recorded here so neither is tried
  again.** The build is THREE-staged. While the index is WRITE_ONLY
  `checkUniqueness` RECORDS a violation rather than throwing
  (`index_maintainer.go:493-504`; Java's
  `standardIndexMaintainer.checkUniqueness` calls `addUniquenessViolation` once
  per conflicting PK, `StandardIndexMaintainer.java:497-498`), so the scan
  completes. Java's inline rebuild then calls the ONE-ARGUMENT
  `markIndexReadable(index)` (`FDBRecordStore.java:4602` → `:3767-3768`,
  `allowUniquePending=FALSE`) and `checkAndUpdateBuiltIndexState` (`:3821`)
  throws `RecordIndexUniquenessViolation` (`:3856-3861`) — so the index is NOT
  made readable and NOT parked in `READABLE_UNIQUE_PENDING`. **But
  `rebuildIndex` SWALLOWS that throw**: the chain ends in
  `.handle((b, t) -> { if (t != null) logExceptionAsWarn(...); …; return null; })`
  (`:4602-4615`), and a `CompletableFuture` handle returning normally completes
  normally. Java pins the net effect in its own test,
  `FDBRecordStoreUniqueIndexTest.addUniqueIndexViaCheckVersion:615-627`: the
  index is WRITE_ONLY, `scanUniquenessViolations` holds every conflicting PK,
  and the transaction COMMITS.
  So the operator-visible outcome is: store opens fine, migration ran (stale
  `0x00` entries gone), index left WRITE_ONLY hence NOT scannable, violations
  durable and naming both records. That is safe precisely because a
  non-scannable index means queries fall back to base scans and no read is
  wrong; propagating instead would turn any metadata evolution meeting bad data
  into a STORE-OPEN OUTAGE.
  **Two wrong answers were shipped in this item before that one.** First, Go
  called `MarkIndexReadableOrUniquePending` unconditionally on both the inline
  and online paths — no policy, no format gate — silently downgrading a
  violation into a pending state nobody opted into. Fixing that, the second
  attempt made the inline path PROPAGATE the throw, which is the outage
  described above; it was reached by reading `markIndexReadable` and stopping
  before the `.handle` that consumes it. Both are now pinned against, and the
  swallow is cited at the call site so the next reader does not re-litigate it.
  `READABLE_UNIQUE_PENDING` is reachable in Java core from ONE caller,
  `IndexingBase.java:324`, gated on
  `IndexingPolicy.shouldAllowUniquePendingState` (`OnlineIndexer.java:1117`):
  an opt-in defaulting FALSE (`:1220`, javadoc "allow=false (default, backward
  compatible): throw an exception") that ALSO requires format version >=
  `READABLE_UNIQUE_PENDING` (`FormatVersion.java:145`).
  **Go diverged**: it called `MarkIndexReadableOrUniquePending`
  unconditionally on both the inline path (`store_builder.go`) and the online
  path (`online_indexer.go`), with no policy and no format gate, under a
  comment claiming it matched Java. So a uniqueness violation was silently
  downgraded to a pending state nobody opted into. Fixed here: the inline path
  now throws, and `OnlineIndexerBuilder.SetAllowUniquePendingState` ports the
  policy with Java's default and format-version gate. Both SIDES of the policy
  are pinned, since a flag tested only in its ON position is not a policy.
  FOUR pre-existing specs that had encoded the divergence were corrected in
  the same change rather than left to rot.
  **A second divergence fell out of covering the format-version conjunct.**
  The gate is an AND, and its second half needs a store BELOW format 9 — which
  Go could not produce: `maybeUpgradeFormatVersion` unconditionally raised any
  store to `formatVersionCurrent` on every open, so a forced-down header was
  undone by the next open. Java does not do that; the target comes off the
  BUILDER (`FDBRecordStoreBase.BaseBuilder.setFormatVersion`, :2245/:2266),
  whose javadoc gives the reason: during a rolling upgrade you pin every
  instance to the OLD format so none starts writing a layout the
  not-yet-upgraded instances cannot read. Go now has
  `StoreBuilder.SetFormatVersion` (and `OnlineIndexerBuilder.SetFormatVersion`,
  since Java gets it free by reusing the caller's record-store builder), the
  upgrade targets it instead of a constant, and it is a ceiling that never
  downgrades. Default behaviour is unchanged. Pinned by
  `TestFDB_CardinalityUniquePendingBlockedByFormatVersion`.

  Original booking, kept for the reasoning it records:

  CQ-89 changed the CARDINALITY() index key for an EMPTY
  non-nullable (flat repeated) array from a NULL key to the integer key 0.
  MEASURED, with the repo's own tuple codec: the entry key went from
  `indexSubspace ‖ 0x00 ‖ pk` to `indexSubspace ‖ 0x14 ‖ pk`. The write path
  (`index_maintainer.go:217-223`) suppresses neither, so BOTH forms are real
  KVs on disk.
  **Consequence on an existing store.** Records written by a Go binary
  PREDATING that commit carry the `0x00` entry. After upgrade nothing rewrites
  them, so an index-backed `CARDINALITY(a) = 0` misses those rows while a full
  scan returns them, and `CARDINALITY(a) IS NULL` returns rows on a column
  whose type forbids NULL — the index/base-table split CQ-89 set out to
  remove, re-created in the opposite direction until the index is rebuilt.
  **Scope is narrow, and one direction is strictly improved.** Only a
  CARDINALITY index over a NOT NULL array column with empty arrays is
  affected; the base-table read change stores nothing and needs no migration.
  Java-written data was always keyed 0, so Go-vs-Java divergence is REDUCED by
  the fix — the stale entries are Go-vs-Go.
  **A second, sharper hazard on UNIQUE cardinality indexes.** The maintainer
  skips the uniqueness check when the entry key contains a NULL
  (`index_maintainer.go`'s `!indexKeyContainsNull` guard; Java:
  `StandardIndexMaintainer.java:471`). Under the OLD key, two records with
  empty NOT NULL arrays both keyed NULL and were therefore never checked — so
  an existing store may already hold two such records. Under the NEW key both
  key 0, a state the unique index considers impossible; a rebuild would fail,
  and until then the invariant is silently false. Reachable only
  programmatically today (`builder.go:606-608` honours `unique` for the
  cardinality arm, but its only producer, `AddCardinalityIndex`, never sets
  the flag and no SQL DDL route reaches it), so this is latent rather than
  shipped — pinned by `TestFDB_ArrayCardinalityUniqueIndex`. It becomes live
  the moment a UNIQUE cardinality DDL form lands.
  **RULING (owner, made — DOCUMENT, DO NOT FORCE).** No
  `LastModifiedVersion` bump. Rationale: the product is pre-production —
  road-to-prod is still open — so every existing cluster carrying
  old-Go-written data is dev/test. Forcing metadata churn on every schema
  with a cardinality index is the heavier tool for a hazard whose blast
  radius today is internal. The migration is published instead: the PR body
  and `road-to-prod.md`'s watch-list both carry the note that old Go-written
  cardinality entries are stale under the new key, that an index rebuild
  (`OnlineIndexer`) clears them, and that until rebuilt an index-backed
  `CARDINALITY(a) = 0` may disagree with a full scan on rows written by
  pre-fix Go. The uniqueness sub-hazard above is published with it: a rebuild
  can surface a pair the old NULL key let bypass the check, and that
  surfacing is the CORRECT loud outcome, not a rebuild bug.
  **WHAT REMAINS BOOKED, and its trigger.** This item stays open as a
  CONDITIONAL go/no-go, not as a filed-and-forgotten note: **if this ships to
  a real (non-dev) cluster before the affected indexes have been rebuilt, the
  forced-rebuild mechanism has to be reconsidered** — the ruling above rests
  entirely on "every affected store is dev/test", and that premise expires on
  first production deployment. DONE = either (a) road-to-prod closes with the
  affected indexes rebuilt and this is struck, or (b) a production deployment
  is scheduled while stale entries can still exist, in which case the
  `LastModifiedVersion` bump lands with a test
  (`metadata_evolution_validator.go:488-504` already implements Java's
  `allowIndexRebuilds` contract, so the mechanism exists — only the decision
  to use it is pending).
  → Resolved by NEITHER branch as written: both assumed a production store could
  carry pre-fix entries. It cannot — production stores are all created fresh by
  post-CQ-89 binaries, so the trigger is unreachable rather than fired, and
  entry 18 retires on that ground. The engine work the investigation produced
  (rebuild-path uniqueness parity, `SetFormatVersion` + feature gates, the
  behaviour pins) stands on its own and is listed above.

- [x] **CQ-86 (MED, result surfacing): a COMPUTED record reaches the driver as
  a bare `map[string]any`, so it is not an `api.Struct` and nothing downstream
  can describe it as one.** DONE via RFC-204 §4.5.1's PLAN-TIME DESCRIPTOR
  BAKE. `cascades.FinalizePlan` (`plan_finalize.go`) builds ONE
  `values.NewTypeProtoRepository()` per plan and stamps every reachable
  `*RecordConstructorValue` with `MessageDescriptorFor(rc.Type())`; a stamped
  constructor's `Evaluate` builds a `dynamicpb` message POSITIONALLY, as Java's
  `RecordConstructorValue.eval` does. The descriptor rides on the VALUE rather
  than the evaluation context (Java's carrier) because Go has no uniform
  context — see §4.5.1 and `TestFrontierContextIsNotAUniformCarrier`. Stamping
  runs on the plan-cache MISS path only; the hit path shares one plan pointer
  across concurrent pages, so a later write would race.
  The descriptor EMITTER the entry called "the real work" already existed
  (`values/proto_type.go`), so what was missing was only the per-plan walk.
  Both predicted consumer breakages were real and are fixed:
  `explodeElementRow` gained a message arm laying the row out in DECLARED
  field order, and `protoFieldByName` gained the ESCAPED-name lookup it never
  had — a field whose identifier `ToProtoBufCompliantName` mangles (`a$b` →
  `a__1b`) previously resolved to nothing and read back as a silent NULL.
  NOT DONE, and deliberately so: the bake stops at a DML plan
  (`feedsAWrite`). A constructor feeding a WRITE must keep the map, because
  the target's descriptor governs there and Go binds to it at coercion time
  (`BuildStructMessage`); baking its own inferred descriptor loses width
  promotion, the anonymous-positional bind, NOT NULL and the arity check.
  MEASURED both ways — removing the guard breaks multi-row `INSERT … VALUES`
  across 6+ corpus files.
  `functions.yamsql` did NOT close: `engine-gap:struct-query` drops 2 → 1 as
  predicted, but the file RE-BOOKS to `engine-gap:dml-returning-result-set`.
  Clearing the struct blocker exposed two further gaps — LEAST/GREATEST
  argument admission (FIXED here: Java's two-step fold-then-operator-map check,
  22000 vs 22F00, which Go had neither of) and DML RETURNING, which is
  unimplemented and is what the file now rests on.

  Original booking follows. The query is
  otherwise CORRECT — `SELECT COALESCE(null, (1, 1.0, 'a', true))` plans and
  evaluates to `{_0: int64(1), _1: float64(1.0), _2: "a", _3: true}`, pinned in
  `pkg/relational/sqldriver/record_constructor_expression_fdb_test.go` — so
  this is purely the boundary conversion. `materializeDriverValue`
  (`cascades_generator.go`) is Java's `RowStruct.getObject` Types.STRUCT arm
  (RowStruct.java:184-197) and converts a `protoreflect.ProtoMessage` into a
  `rowstruct.MessageStruct`; a computed record never reaches that arm because
  it is not a message. THE JAVA ANSWER IS ALREADY HALF-WRITTEN IN GO: Java's
  `RecordConstructorValue.eval` builds a dynamic proto Message from the record
  type (RecordConstructorValue.java:113-139) — the SAME structural walk
  `values.BuildStructMessage` already performs on the DML side — so in Java a
  computed record IS a message and the existing conversion fires unchanged.
  Go's `RecordConstructorValue.Evaluate` returns a map instead.
  MEASURED, live JVM, `conformance/record_constructor_java_probe_test.go`
  probe `bare_record_literal`: JAVA `OK cols=[_0(STRUCT)]`
  rows=`[map[__unsupported__:com.apple.foundationdb.relational.api.ImmutableRowStruct]]`
  (the harness cannot render a Java struct's contents, but the TYPE is STRUCT);
  GO `OK cols=[_0(STRUCT)] rows=[[map[_0:1 _1:1 _2:a _3:true]]]` — a raw map.
  Booked `engine-gap:struct-query` in
  `pkg/relational/conformance/javacorpus/gaps.go` at the row-shape mismatch.
  WHY IT IS NOT A ONE-LINER: `RecordConstructorValue` is also the whole-row
  projection carrier, so changing `Evaluate` to return a message changes the
  representation every projection flows. Either that conversion is done for
  every projection row (matching Java, and the honest end state) or the
  boundary is given the record TYPE so it can order the map —
  `executor.ColumnDef` is a flat type string today (CQ-74), so it cannot carry
  one.

  **CORRECTION — "reuse `BuildStructMessage`, the conversion then fires
  unchanged" DOES NOT WORK AS WRITTEN, and this is the blocker.**
  `BuildStructMessage(md, fields, convert)` takes a
  `protoreflect.MessageDescriptor` as its FIRST argument and walks it. Both
  existing callers get `md` from the TARGET COLUMN's descriptor — the stored
  table schema (`pkg/relational/core/functions/proto_value.go:363` via
  `fd.Message()`, `pkg/recordlayer/query/executor/executor.go:3586` likewise).
  A COMPUTED record has no target column and no stored descriptor, and
  `RecordConstructorValue.Type()` (`values.go:4277-4289`) returns an ANONYMOUS
  `*RecordType`. So there is nothing to pass as `md`.

  Java does not have this problem because it SYNTHESISES the descriptor:
  `RecordConstructorValue.eval` calls
  `typeRepository.newMessageBuilder(getResultType())`
  (RecordConstructorValue.java:113-139), which reaches
  `TypeRepository.Builder.addTypeIfNeeded` → `Type.Record.defineProtoType`
  (TypeRepository.java:498-503, Type.java:2339-2356) — emit a `DescriptorProto`
  per record type, recursively, into a `FileDescriptorProto`.
  **Go has no equivalent.** `values.TypeRepository`
  (`values/type.go:1618-1697`) is a name→`Type` map with no protobuf surface
  at all — no `newMessageBuilder`, no `DescriptorProto`, no `dynamicpb`. The
  only descriptor-emitting machinery in the repo is DDL-time
  (`pkg/relational/core/metadata/builder.go:659-978`,
  `tableTypeRepo.defineStruct` at :905 explicitly "Mirrors
  Type.Record.defineProtoType"), and it is unusable here: it works from
  `api.DataType` / `api.StructType`, there is no `values.Type` ↔ `api.DataType`
  bridge, and `defineStruct` errors on an empty name — which is exactly what a
  computed record has.

  SO THE REAL WORK IS: port `Type.Record.defineProtoType` against
  `values.Type` — a `values.RecordType` → `protoreflect.MessageDescriptor`
  emitter living in the `values` package (nested records recursively, arrays
  as repeated + the `NullableArrayWrapper` identity, UUID as the
  `com.apple.foundationdb.record.UUID` message dependency, deterministic
  synthetic type names, and a cache so it is not rebuilt per row). Only then
  does `BuildStructMessage` have an `md` and the existing
  `materializeDriverValue` ProtoMessage arm fire unchanged.

  SECOND BLAST RADIUS, also not in the original booking: two consumers read
  the map SHAPE and break if `Evaluate` starts returning a message.
  `executor.explodeElementRow` (`executor.go:3796-3827`) type-asserts
  `elem.(map[string]any)` at :3806, and `FieldValue.descendResolvedPath`
  (`values/values.go:825-869`) handles `OrdinalRow` and `proto.Message` but
  DELIBERATELY refuses a map (:840-845 documents the refusal as "never a
  silent name read"). Both need an arm before the representation flips.
  DONE = `functions.yamsql` clears its struct-query blocker, `struct-query`
  reaches 1, and a computed record reads back through the driver as an
  `api.Struct` whose attribute ORDER matches the constructor's declaration
  order. NOT "`functions.yamsql` passes": clearing the struct blocker only
  moves the file to its NEXT rejection, and it lands on DML RETURNING
  (`engine-gap:dml-returning-result-set`, pinned in `gaps.go` at
  `update C set st = coalesce(st, null) where c1 = 4 returning "new".st`),
  which is a different workstream. The file passing is CQ-72's bar, not this
  item's.

### RFC-220 residue — what the three review passes surfaced and this PR does NOT close

- [x] **CQ-96 — CLOSED as a CLASS, not instance-by-instance.** `IndexScanCarrier`
  (sealed, so `RecordQueryAggregateIndexPlan` cannot satisfy it structurally —
  it emits one row per GROUP) plus `plans.IndexPlanOf`, plus an AST gate in
  `pkg/docscheck` that fails the build on any non-test `*RecordQueryIndexPlan`
  type test lacking a covering arm.

  The census is stated as a BEFORE and an AFTER measured by the same instrument,
  because a single after-figure cannot show how much was fixed and an earlier
  write-up of this item recited a split (29 correct / 21 blind) that neither
  figure supports. Both lines below are the gate's own `t.Logf`, the before one
  taken by exporting the pre-fix commit and dropping in only the gate:

  ```
  before: 55 index-plan type tests examined = 23 covered + 5 allowlisted + 27 blind
  after:  54 index-plan type tests examined = 49 covered + 5 allowlisted +  0 blind
  ```

  So 27 sites were blind, not 21. The population itself drops by one because two
  adjacent `extractIndexPlan` assertions collapsed into a single `IndexPlanOf`
  call — which is why the two totals are not required to match, and why the gate
  now logs the full decomposition rather than a lone blind count: 0 blind is
  exactly what a scan that found nothing also prints. The gate refuses a
  zero-site scan (the never-ran state), and its allowlist is keyed on
  file+enclosing-function rather than line numbers, with a test failing any entry
  that matches nothing — a line-keyed allowlist rotted inside this very change.

  Two of the 27 were fail-OPEN, not precision: `planReferencesAnyBuriedAlias`
  (the post-rebase verifier read "no buried reference" and licensed the probe it
  exists to decline) and `dataAccessExprCorrelations` (under-reported SARG
  correlations — a correlated probe read as self-contained).

  Why the class exists at all: coveringness became a plan TYPE, so a
  `*RecordQueryCoveringIndexPlan` wrapping an index plan does not match
  `case *plans.RecordQueryIndexPlan`, and the walk does not descend into the
  wrapped scan because it is a structural FIELD rather than a child.

  Two more of the 27 were CORRECTNESS rather than precision:
  `stampNodeLocalValues` never reaching a covering plan's comparands (an
  unstamped `RecordConstructorValue` evaluates NAME-keyed instead of
  field-number-keyed), and `probeOuterBakedType` returning nil for a covering
  outer.

  Earlier passes at this item quoted hand-run grep counts (first "~14 sites",
  then "52 non-test sites"). Both are superseded and are deliberately not kept
  here: a stale count sitting next to a measured one is indistinguishable from a
  disagreement, and the gate's logged census is now the only figure anyone
  should quote. That is also the point of closing this as a CLASS — an
  instance-by-instance fix leaves every newly written type switch a fresh latent
  instance, which is what the gate, not a grep, prevents.

- [x] **CQ-101 (query-engine): `RecordQueryLimitPlan` swallows the row its inner
  now states, so a LIMIT above a projection re-hides what RFC-226 exposed.**
  
  **DONE — closed by RFC-232; found still open during a #762 sweep.** `plans/limit.go`
  `GetResultType()` returns `p.GetResultValue().Type()`, so a LIMIT forwards the row its
  inner states instead of answering the singleton. The entry stayed unchecked after the
  fix landed elsewhere, which is how it came to schedule work that no longer existed.
  The measurement below is the ORIGINAL finding, kept as the record of what was wrong.
  MEASURED at RFC-226 while pinning `distinctKeyColumns`:

  ```
  scan(ID,A,B) -> Projection([A] AS RENAMED) -> LimitPlan
  LimitPlan.GetResultType() == *values.PrimitiveType (UnknownType)
  ```

  `limit.go:` `GetResultType()` is `return values.UnknownType` — a flat stub, not
  a forward. A LIMIT cannot alter a row type and its inner is one call away, so
  this is the sharpest remaining entry in the RFC-213 stub inventory
  (`pkg/docscheck/result_type_stub_census_test.go`, which already lists it and is
  the ratchet that will notice when it goes).

  CONSEQUENCE, measured rather than argued: the wrapper-over-projection pin
  (`cascades/distinct_key_columns_wrapper_test.go`) had to use a
  PredicatesFilter, because a Limit cannot transmit the flip at all. Any
  consumer reading a row type through a LIMIT still sees "unstated" and stays on
  its fail-closed path.

  NOT FIXED INSIDE RFC-226 ON PURPOSE, and this is a STOP rather than a
  deferral of tiny work. The edit is three lines; the RISK is not. Flipping a
  stub changes what every fail-open consumer above it does, and RFC-226 §1c is
  the measured proof that those consumers cannot be enumerated by a call-site
  census — the one that broke reads `GetResultType()` on a different node's plan
  through a helper. So this needs its own role-differential (leg, subquery
  source, query root) exactly as RFC-226 §5 now prescribes, not a rider on a
  change whose own §1 was refuted once already.

- [x] **CQ-102 (SMALL, query surface): a COMPUTED aggregate argument types its
  output UNKNOWN, so arithmetic over it across a derived-table boundary demotes
  to Integer.** CLOSED. `SELECT G + 1 FROM (SELECT MIN(col2 + 1) AS G FROM t1)
  AS Y` now reports BIGINT, matching Java's LONG, and the residual pin that
  asserted the wrong answer has been folded into
  `TestFDB_AggregateOutputTypeCrossesTheDerivedBoundary`'s table as two ordinary
  arms (the outer arithmetic and the inner read).

  The fix was the ordering change this item prescribed, arrived at from the
  other end: `buildDerivedTableSourceFromTerm`'s aggregate arm now takes its
  types from the body's own exact result row — `ExactLogicalResultType`, the
  same derivation the translator runs — instead of from `aggOutputCols`, which
  runs before any scope exists and therefore had no resolver to walk the
  argument expression with. `aggOutputCols` remains the output-NAME authority;
  the manual schema stays only as the fallback for a body that cannot prove a
  complete representable row.

  Two restrictions came off with it, both of which were dropping exactly-
  derivable rows: the exact path used to be attempted only for a post-aggregate
  EXPRESSION (`SUM(v)*2`), never for an aggregate over a computed ARGUMENT, and
  never for a JOINED body at all. One UNKNOWN column makes the WHOLE derived row
  inexact, so a perfectly-known grouping key beside it stopped resolving too —
  which is why this also closed `ORDER BY key "TOTAL_VALUE" has no resolved
  Value` on `SELECT sub.category, sub.total_value FROM (... SUM(price * qty) AS
  total_value ... GROUP BY category) sub ORDER BY sub.total_value`, and the same
  shape over a joined body.

- [x] **A member predicate over a correlated UNNEST binding returns ZERO ROWS**
  · M · found while auditing name-keyed reads of a fused nested reference
  (RFC-231 §7)

  ```
  CREATE TYPE AS STRUCT item (sku STRING, qty BIGINT)
  CREATE TABLE orders (order_id BIGINT, items item ARRAY, PRIMARY KEY (order_id))
  INSERT INTO orders VALUES (1, [('x', 10), ('y', 20)]), (2, [('z', 30)])

  SELECT order_id FROM orders, orders.items AS i WHERE i.sku = 'x'
    -> rows=0, err=<nil>          MUST be [1]
  ```

  The projection of the same reference is CORRECT — `SELECT i.sku FROM orders,
  orders.items AS i` returns `[x y z]` — so the descent resolves and the element
  flows; it is the PREDICATE path that drops the rows, silently and with no
  error. The existing coverage of this shape
  (`TestStructDDL_UnnestStructArrayFieldAccess`) asserts only that the query
  PLANS, which is why it has stayed green.

  THE THREE-WAY MEASUREMENT, RECORDED BECAUSE IT IS THE ONLY THING THAT KEEPS
  THIS FROM BEING MIS-DIAGNOSED. This defect and RFC-231's unnest
  element-substitution wrong-read sit at the SAME binding and are
  observationally identical — on master the substitution arm would ALSO have
  yielded zero rows here, because it replaced `i.sku` with the whole element and
  no struct equals `'x'`. Measured on all three:

  ```
  origin/master (root mint, no arity gate)   rows=0
  RFC-231 rev 1 (leaf mint, no arity gate)   rows=0     <-- THE DISCRIMINATOR
  RFC-231 rev 2 (leaf mint, arity gate)      rows=0
  ```

  Rev 1 is the discriminator: the substitution no longer fires there (`Field`
  became `SKU`, which is not the alias `I`, measured `MATCHED=false`), and the
  rows are still zero. So the substitution arm is NOT the cause, and the two
  findings are independent.

  WITHOUT THIS ROW, the likely failure mode is concrete and expensive: someone
  re-diagnoses this as RFC-231's wrong read and "fixes" it by reverting the mint
  to name a fused value after its struct ROOT — which restores a wrong read
  (every `i.<member>` matching the alias again), re-breaks the arithmetic-operand
  metadata RFC-231 fixed, and still returns zero rows.

  DONE means: the query returns `[1]`, the mechanism is named at its site, and
  the regression asserts ROWS rather than plannability — with the colliding-alias
  and multi-member shapes covered, not just the one spelling above.

  RESOLVED. The discriminator above held: neither the mint nor the arity gate was
  the cause, and the fix touches neither. The cause was one layer lower, in
  `FieldValue.evaluateCorrelated` — when a correlation binds a DATUM rather than
  an OrdinalRow, both correlated arms returned that datum WHOLE and dropped every
  accessor past the root. A struct unnest element binds exactly that way (the leg
  adapter unwraps the bare-scalar `_0` carrier to its datum, `isBareScalarRow`),
  so `/I/SKU` served the whole element; against `= 'x'` it is never equal, hence
  zero rows rather than a wrong one. The drop was invisible to every
  single-accessor reference, which is why only a DESCENT exposed it. Both arms now
  apply the remainder via `descendResolvedPath`, a no-op when the path has none —
  so the scalar-element datum convention is unchanged. Java has no equivalent arm
  at all: `FieldValue.eval` resolves the whole FieldPath against the child's
  flowed Message (FieldValue.java:169).

  Pinned by `TestFDB_UnnestMemberPredicateServesRows` (rows for the STRING member,
  the non-leading BIGINT member, and a member predicate beside a sibling
  projection) and by the unit twin `TestFieldValue_DatumBinding_AppliesPathRemainder`,
  which drives the two correlated arms SEPARATELY plus the single-accessor
  no-remainder case. Mutating each arm alone reddens only its own subtest. The
  colliding-alias shape stays refused 42702 and is already pinned by
  `TestFDB_NestedMemberSpelledLikeItsUnnestAliasIsRefused`.

  The plan-only test that let this ship (`TestStructDDL_UnnestStructArrayFieldAccess`)
  cannot assert rows — its package has no store — and now points at its row twin,
  and back.

  The suspected residual in the OTHER representation is NOT a defect, and the
  investigation that establishes it is worth more than the guard it rejects. An
  UNBAKED `FieldValue` (no `Resolved` path) over a datum binding has no remainder
  to apply and serves the datum WHOLE, which reads like the same silent-wrong one
  layer over. Making it loud was tried and REVERTED: the whole-datum read is a
  LIVE, CORRECT convention, and the change broke
  `TestFieldValue_UnpinnedNonOrdinalBinding_IsSilent` — a sentinel written
  precisely so that a change to this arm is a deliberate red->green edit rather
  than silent drift. It did its job.

  The mechanism, because this will be re-proposed: the sort-key leg fallback
  mints an unbaked `NewFieldValue(qov, col, …)` when a leg's layout is not
  derivable (`cascades_translator.go`), and for a leg bound to a datum the whole
  datum is the right answer. What keeps a MEMBER reference out of that arm is not
  a guard but a property — a member reference is BAKED, arriving with the path
  whose remainder gets applied. So the protection is "members are baked"; a guard
  could not have distinguished the two cases anyway, since both present as an
  unbaked node over a datum. Pinned from the unnest side by the fourth arm of
  `TestFieldValue_DatumBinding_AppliesPathRemainder`, which points at the
  pinned/unpinned sibling so the pair cannot drift.

  Recorded where a test cannot express it: `TestJoinUnnestExistsPlanSmoke`'s
  unnest arms are plan-only AND safe, but only because `existsGatherSchemaMetadata`
  declares `ARR` with a SCALAR `INTEGER` element — no reference over such an
  element can carry a second accessor, so the descent is a no-op by construction.
  That is the REASON, and it is what survives: adding a struct-array column to
  that shared schema silently re-arms this class across those plan-only arms. The
  smoke test now says so and asks for a row twin instead of another entry in its
  list.
- [x] **The rowdiff nightly ate a two-runner CI fleet for ~5h a night for six
  consecutive nights, and the two instruments built to explain that could not
  run during it** · M · found while root-causing the 2026-08-11 runner wedge ·
  FIXED, pinned, and the framing it was reported under was wrong
  **NOT the "Race detector (SQL layer + client + cascades)" job, and nothing to
  do with `-race`.** The wedged command
  (`sqldriver_test --test.run=TestFDB_RowDiff_Smoke --test.v` under
  `--test_timeout=17400`) is verbatim the "Run rowdiff deep sweep" step of
  `.github/workflows/nightly-rowdiff.yml`. The race lane sets no `--test_timeout`
  and never runs a rowdiff target.
  **NOT a deadlock and NOT an unbounded seed loop** — the other two candidate
  framings. MEASURED from the timeout panic's goroutine dump, which the incident
  report believed was never captured but which Go printed into the CI log of run
  31350088492 (`panic: test timed out after 4h50m0s`, `running tests:
  TestFDB_RowDiff_Smoke/random_seeds (4h50m0s)` — ONE test, no parallel
  interleaving with `TestFDB_RowDiff_Paging`, which the filter never selected).
  The live goroutine sat in
  `client.(*database).getOrDialConn` -> `transport.dialWith` ->
  `net.(*netFD).connect` in `[IO wait]`: a TCP connect blackholing against an FDB
  address that had stopped answering. Forward progress, one seed per 60s context
  deadline, through a range it could no longer measure anything with.
  THREE defects, all fixed here:
  1. **The seed loop had exactly one exit condition — seed exhaustion.** A dead
     cluster therefore cost the full budget every time. Fixed with an INFRA
     circuit breaker (10 consecutive INFRA seeds = the cluster is gone, stop) and
     a wall-clock `ROWDIFF_BUDGET`, both leaving via `break` so the reporting
     path always runs. Turns a ~5h wedge into a ~10min red that names the cause.
  2. **`ROWDIFF_TALLY` and the vacuity floor both run AFTER the loop**, so the
     alarm's SIGQUIT killed them. MEASURED: the 08-10 log contains no
     ROWDIFF_TALLY line at all, and the workflow summary greps for one — the
     instruments built to distinguish "the cluster died" from "the engine
     returned wrong rows" could not speak during the only failure they existed
     for. `--test_timeout` is now only a backstop; the in-test budget is the
     control, and `pkg/docscheck`'s `TestRowdiffSweepBudgetBoundsTheTimeout`
     fails the build if a sweep drops its budget or lets the timeout back in
     front of it.
  3. **The 18000-seed range never fit its own timeout, even on a healthy
     cluster.** The workflow asserted ~1.5 seeds/s. MEASURED across the healthy
     prefix of four nights (31350088492 / 31290367277 / 31234733111 /
     31143737482): 0.94, 1.03, 1.07, 1.15 seeds/s — ~1.05. 18000 seeds at 1.05/s
     is 4h46m against a 4h50m timeout, four minutes of margin.
     THE FIRST ATTEMPT AT THIS FIX WAS ITSELF WRONG, and the way it was wrong is
     the durable lesson: it re-sized the seed count against a measured rate and
     made budget exhaustion an ERROR. Re-measuring after #721 (nested cases)
     landed showed the paged lane's per-seed cost had moved 3.73x — measured
     back-to-back on one tree, un-paged 0.864 seeds/s vs paged 0.232 — so the
     freshly-chosen count was already stale and `ROWDIFF_ABORTED` would have
     become the nightly steady state. A guard that fires on healthy runs is the
     failure this file warns about repeatedly, reintroduced by the fix for it.
     THE SHIPPED DESIGN inverts it: **the clock sizes the night, the seed count
     is only a CEILING**, and budget exhaustion is a normal logged termination
     rather than a red. What keeps that honest is a per-lane COVERAGE FLOOR
     (`ROWDIFF_MIN_SEEDS`, 2000 un-paged / 500 paged) — a truncated night is
     fine, a night that measured almost nothing is not, and without the floor a
     tenfold cost regression would walk 200 seeds and report green forever. The
     floor is per lane because per-seed cost is; one number sized for the fast
     lane fails the slow one for doing its job. This survives the next generator
     change, which no absolute seed count does.
  ALSO FIXED: the PAGING sweep step inherited the default "skip if a previous
  step failed", so it reported `skipped` on all six nights while the job still
  recorded a genuine-run heartbeat — nightly-reconcile saw a live net over a half
  that never ran. It is now `!cancelled()`-guarded and independent.
  EXPOSURE IS SCOPED TO THIS JOB, not a fleet-wide pattern. From
  `for f in .github/workflows/nightly-*.yml; do grep -oE "test_timeout=[0-9]+" $f; done`
  the per-test budgets are 60, 300, 900, 1200, 3600 and — before this change —
  17400. rowdiff's was ~5x the next largest, which is why one wedge here cost
  ~5h of a two-runner fleet and why no sibling nightly can do the same. The
  other jobs need no equivalent change.
  PINNED: `TestRowdiffSweepStopsWhenTheClusterDies` drives every arm of the
  stopping policy from explicit state — both fire arms, both disabled arms, the
  precedence arm, the SEVERITY of each stop, and the load-bearing negatives
  (scattered INFRA must never accumulate; a zero budget must mean unbounded
  rather than instant abort). `TestRowdiffCoverageFloor` drives the floor in
  both directions, including the negative that makes it safe to ship: the
  25-seed PR smoke slice walked everything it was asked for and must not trip a
  thousands-scale floor. `TestRowdiffSweepBudgetBoundsTheTimeout` +
  `TestRowdiffSweepBudgetGateArms` pin the workflow ordering, the presence of
  the floor, and the gate's own comparison. EIGHT independent mutations were
  each confirmed RED across the two designs; the final four redden DISJOINT
  arms (severity, floor-collapse, floor-ceiling, workflow-declaration).

- [x] **A sixth `legRef`-adjacent site rebased an outer-leg reference with no
  arity guard and discarded the nested suffix — REAL WHEN BOOKED, CLOSED BY
  `db9d87d7c` BEFORE THIS BRANCH REBASED ONTO IT. Kept as the audit trail, and
  because the attached element-channel lead needed a measurement.**
  · found during the RFC-228 nested-ON fix (PR #725)
  `rebaseUnnestOuterLegPredicateOrdinal`
  (`pkg/relational/core/query/cascades_translator.go` — note the path: the file
  is under `core/query/`, NOT `core/embedded/`) took any `FieldValue` with a
  `QuantifiedObjectValue` child on an outer leg and baked it by NAME, with no
  arity guard. For a nested reference the name lookup succeeded on the ROOT
  column and returned a fresh SINGLE-accessor address, so `Accessors[1:]` was
  dropped and the predicate compared the enclosing STRUCT.
  **THAT IS NO LONGER TRUE, AND THE FIX IS UPSTREAM OF THIS ENTRY.**
  `db9d87d7c` (#724) added, immediately above the slot lookup:
  ```go
  if fv.Resolved != nil && len(fv.Resolved.Accessors) > 1 {
      ok = false
      return node
  }
  ```
  and both callers bail on `!ok`, so the shape now DECLINES loudly instead of
  shipping a truncated path. Verified against the two bases rather than
  re-derived: the guard is ABSENT in that function at `ddc2914ba` — the base the
  booking was AUTHORED on, where the finding was correct — and at `ffc31689c`,
  and PRESENT at `db9d87d7c`.
  Re-check with `git log -S'len(fv.Resolved.Accessors) > 1' -- <that file>`
  rather than by reading the function again.
  **THE MASKING RELATION AND ITS FIX-ORDER ARE THEREFORE RETIRED.** This entry
  previously claimed the safety net at `unnestExistsRefSurvivesUnbaked` had a hole
  masked by the sixth site, so the two had to be fixed together. With the site
  declining, there is nothing to mask; the "fix them together" constraint
  described a state that no longer exists and must not be carried forward.
  **THE ATTACHED LEAD WAS MEASURED AND DOES NOT REPRODUCE.** The ELEMENT channel
  has the same unguarded shape: `bakeUnnestElementRefOrdinal` skips an unpinned
  multi-accessor ref (keyed `!SourceRelativeBaked()`) and, unlike the outer-leg
  sibling, sets NO failure flag; `unnestExistsRefSurvivesUnbaked` then skips it
  on the same key as "safe", on a comment still asserting that multi-accessor
  nodes are machinery-owned. On paper a nested ELEMENT reference survives unbaked
  while the net declares the tree clean. MEASURED: it cannot be reached, because
  a member reference on a STRUCT element is refused during resolution INSIDE an
  EXISTS subquery — `42703: column "EK" does not exist` for the flat form and
  `42703: column "DK" does not exist` for the nested one. The rule never looks at
  arity, so the nested case is refused together with its flat twin, one step
  before the bake.
  **THAT UNREACHABILITY IS AN ACCIDENT OF A BUG, NOT A GUARANTEE, and the 42703
  is itself a Go-only divergence that is OWED A FIX.** Java answers both forms
  from inside an EXISTS body — `valid-identifiers.yamsql:221` (flat member,
  `-> [{2}]`) and `:226` (nested member, `-> [{1}]`) — because
  `SemanticAnalyzer.resolveAcrossFragments` (`SemanticAnalyzer.java:383-401`)
  walks to the root fragment arity-blind and subquery-blind, and
  `LogicalOperator.generateCorrelatedFieldAccess` (`LogicalOperator.java:307-354`)
  emits one Expression per struct field for a struct-array element. Go's refusal
  is a DROPPED ARGUMENT at one call site:
  `existsSubqueryPlanner.addCorrelatedJoinScopeSource`
  (`pkg/relational/core/embedded/logical_predicate.go:9302`) calls
  `unnestVirtualScopeSource(j)` — `unnestVirtualScopeSourceWithElement(j, nil)` —
  so the element column is UNKNOWN with no StructFields, while the sibling
  `unnestScopeSourceAdder` passes `unnestElementStructFields(scope, j)` and types
  it RECORD. That is why the same reference answers OUTSIDE an EXISTS. The scope
  fix is dispatched SEPARATELY (it is pre-existing on master and a behaviour
  change, so it does not belong in the PR that found it).
  THE TWO ARE ONE CHANGE WHEN IT LANDS: repairing the scope re-arms this site the
  same day, so whoever fixes `logical_predicate.go:9302` must also give
  `bakeUnnestElementRefOrdinal` and `unnestExistsRefSurvivesUnbaked` what
  `ordinal_seed.go`'s bake got — resolve the root from `Accessors[0]`, fuse
  `Accessors[1:]`, or decline with `ok=false`, never skip.
  PINNED, as a divergence sentinel rather than a blessing, by
  `TestFDB_UnnestElementMemberInExistsConvertedSentinel` (renamed from
  `TestFDB_UnnestElementMemberInExistsDivergesFromJava` when it was converted)
  (`pkg/relational/sqldriver/unnest_element_member_in_exists_fdb_test.go`), which
  drives a bare SCALAR element through the same buried-conjunct path first so a
  green cannot come from a family that stopped planning, and whose failure
  message says the refusal lifting is GOOD NEWS and instructs the reader to
  CONVERT the test to assert Java's rows — never to delete it.
  DONE: the wrong-rows half is fixed upstream. This entry closes when the scope
  divergence is fixed and the element bake/net are corrected in the same change,
  with the sentinel converted to a row assertion.
  **CLOSED — all three conditions met together, in the entry two below this one.**
  Two corrections to the prose above, which must not be carried forward:
  (a) the scope divergence was NOT one dropped argument at
  `logical_predicate.go:9302`. That site is real but is only ONE of THREE
  fields-less mints, and it is not the one the `SELECT … FROM t.arr AS x` shape
  reaches — that shape is served by a FOURTH, hand-rolled inline mint inside
  `tryBuildCorrelatedPrimaryUnnest` which never calls the shared helper, so a
  census of the helper's call sites cannot see it.
  (b) "it cannot be reached" understated the risk once it was: with the scope
  repaired, the element bake did not merely admit an unbaked nested ref — the
  outer-only conjunct channel returned ZERO ROWS SILENTLY (`EK|` for a query
  whose answer is `EK|10`), measured, and the net reported the tree clean. The
  fix keys both on `RootIsLegRelativeUnpinned()` and fuses `Accessors[1:]`, which
  is what this entry prescribed.
  The sentinel was CONVERTED, not deleted, and now asserts rows.
  (c) the present-tense prose above — "on a comment still asserting that
  multi-accessor nodes are machinery-owned" — described the state at the time and
  is no longer true. That claim was carried in THREE places, not one: the two
  walks' comments, `FieldValue.SourceRelativeBaked`'s own doc
  (`values.go`), and `TestSourceRelativeBaked`, whose assertion message stated it
  outright. All three are corrected; the test keeps its assertion, loses its
  reason, and gains the paired `RootIsLegRelativeUnpinned` check that makes the
  `false` discriminating instead of ambiguous.
  (d) THE MUTATION RECORD FOR THE SILENT ARM WAS STATED IMPRECISELY, and the
  correction strengthens the result rather than weakening it. The shipped commit
  said mutation M4 — "restore `SourceRelativeBaked` in the bake" — reddens the two
  outer-only arms as a SILENT `EK|`. Reverting the BAKE ALONE does not do that: it
  fails LOUD with `0AF00: Cascades planner could not plan query`, because the
  WIDENED WATCHDOG (`unnestExistsRefSurvivesUnbaked`, now keyed on
  `RootIsLegRelativeUnpinned`) catches the unbaked reference and declines. The
  silent `EK|` requires reverting BOTH sites (call it M4b: bake AND net together).
  THAT THE TWO MUTATIONS DIFFER IN KIND IS THE POINT, and it is the strongest
  evidence in the change that the net and the bake no longer share a blind spot:
  before the fix both keyed on the same single-accessor predicate, so the net
  could not see what the bake missed and the failure was silent; after it,
  breaking the bake alone is caught BY the net and converted into a loud decline.
  A net that only reproduces the original silence when it is itself disabled is a
  net that is actually independent.
  Also load-bearing, and measured under M4b: the converted sentinel's flat arm
  reports `flat member = [], want [1]`. The extra `u.uk = 10` data row added
  during the conversion is what makes that `want` non-empty — without it the arm
  would assert `[]` against `[]` and pass with the defect fully present.
---

- [x] **The unnest element's struct fields reached only ONE of the four scopes that bind it, so a member reference resolved outside EXISTS and raised 42703 inside it.**
  MEASURED on master `3179bd6984d028d6324e3f7a3324feb60414aaa9`:
  `SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = 20)`
  → `ERROR: 42703: column "EK" does not exist`, while
  `SELECT x.ek FROM t, t.arr AS x` answers. Java answers both
  (`valid-identifiers.yamsql:221,226` drive the flat and the two-level member on
  a lateral struct-element alias from inside an EXISTS body; Java gets there
  structurally because the unnest quantifier's flowed object type IS the element
  type — `LogicalOperator.generateCorrelatedFieldAccess`).
  Go carries the element's fields onto a VIRTUAL one-column source instead, and
  only `unnestScopeSourceAdder` (the SELECT scope) was passing them. THREE other
  mints were not, each independently pinned by a mutation that reddens its own
  arm and nothing else:
  `buildOuterScopeSources` (a correlated subquery's OUTER scope),
  `addCorrelatedJoinScopeSource` (a correlated subquery's inner JOIN leg), and
  a FOURTH, hand-rolled inline mint inside
  `existsSubqueryPlanner.tryBuildCorrelatedPrimaryUnnest` that does not call the
  shared helper at all — the one a census of the helper's call sites cannot see.
  The three remaining `unnestVirtualScopeSource(j)` (fields-less) call sites are
  CORRECT — they consume only the binding's TOP-LEVEL column NAMES (star
  expansion and the column-name census), which the element's fields do not
  change. Cited BY SYMBOL, because the line numbers rotted the first time this
  was written: all three live in `logical_predicate.go`, in
  `expandQualifiedStars`, `expandBareStarForRowVersion` and
  `expandProjQualifier`. Re-derive the lines rather than trusting any written
  here:
  `grep -n "unnestVirtualScopeSource(j)" pkg/relational/core/embedded/logical_predicate.go`
  A PRIOR REVISION OF THIS ENTRY CITED `:7719, :8101, :8337`, WHICH ARE WRONG BY
  EXACTLY +200 and now point at unrelated code. They were correct when measured
  and rotted when this branch rebased onto a change that grew the same file — a
  line number in a re-check instruction is a trap precisely because it is right
  when written. Census command for the full set:
  `grep -rn "unnestVirtualScopeSource" --include='*.go' .`
  Fixing resolution armed two paths that were masked behind the refusal, both
  fixed in the same change:
  (a) the correlated-primary member reached the executor as an unbound leaf —
  it is now rebased onto the Explode's flowed element
  (`rebaseUnnestElementMemberOntoExplode`), which is the datum-binding contract
  the runtime already implements;
  (b) an outer-only conjunct naming an element MEMBER inside a correlated EXISTS
  body returned ZERO ROWS SILENTLY. `bakeUnnestElementRefOrdinal` skipped it and
  `unnestExistsRefSurvivesUnbaked` declared the tree clean, because BOTH keyed on
  `SourceRelativeBaked()`, which additionally demands a SINGLE accessor and so
  waved a two-accessor member path through. Both now key on
  `RootIsLegRelativeUnpinned()`, and the bake keeps the accessors below the root.
  The scalar-element twin was never affected (its ref is single-accessor) and is
  pinned as a control.
  Pinned by `pkg/relational/sqldriver/unnest_element_member_in_exists_fdb_test.go`
  (15 row-asserting arms; 5 mutations, and the three scope-site mutations redden
  three DISJOINT arm sets).
  The projection gate this change did NOT close is booked as its own UNCHECKED
  item immediately below — deliberately not as prose inside this completed one,
  where nothing would ever pick it up.

- [x] **Phase 12 STEP 1 landed, and the guard belongs at OUTPUT CONSTRUCTION —
  the post-aggregate-only reading was wrong, and so were both of my reasons for
  it.** One block; the corrections matter more than the fix.

  **(1) WHERE THE GUARD GOES.** `groupByOutputConstructionPullUp`
  (`pkg/relational/core/embedded/logical_predicate.go`) refuses two SEMANTICALLY
  equal grouping keys with 42702 at aggregate construction. That is Java:
  `LogicalOperator.java:454` pulls the grouping expressions up against the
  GroupByExpression's own result value through the ASSERTING `Expressions.pullUp`
  (`Expressions.java:112`), so two equal keys yield two entries for one key and
  `size() == 1` raises. It fires BEFORE the SELECT-list
  (`QueryVisitor.java:301`) and HAVING (`:303`) pull-ups and **independently of
  whether any post-aggregate reference exists**. The name-based gate
  (`groupKeysEquivalent`) is untouched.

  **(2) TWO REFUTED CLAIMS OF MINE, both recorded here because both were written
  down and believed.**
  - *"A projected reference reaches `bindPostAggregateValueToNativeOrdinals`."*
    FALSE. Instrumented, that site is consulted ZERO times for the projected
    join spelling; the reference is bound by `buildAggregateOutputSlots` by a
    NAME predicate (`A.R.V.Z`→key 0, `R.V.Z`→key 1). My own shipped test
    contradicted the comment I wrote.
  - *"The `java_42702_go_plans` probes stay open because a bare `SELECT COUNT(*)`
    carries no post-aggregate reference for any pull-up to guard."* FALSE, and
    refuted by this repo's own probe data in the same document:
    `join_qualified_vs_bare` IS a bare `SELECT COUNT(*)` and Java 42702s it. A
    construction-time pull-up needs no reference.

  **(3) THE "TWO PREDICATES, TWO VERDICTS" SEAM IS GONE, not documented.** One
  query shape no longer gets a refusal from its HAVING spelling and an answer
  from its projected one. That incoherence had no Java counterpart.

  **CLOSED, measured:** all six spellings of the join arm — projected, HAVING,
  and two carrying NO post-aggregate reference at all (the shapes that prove the
  guard is at construction) — now raise 42702.
  `under_a_join_two_equal_keys_are_refused_42702_at_output_construction`. Also
  still closed: the correlated-scalar-subquery duplicate
  (`repeated_equivalent_single_source_keys_are_refused_42702`), which the live
  JVM refuses per `join_control_single_source` = `both_42702`.

  **NOT CLOSED, and the guard cannot close it:** the parenthesised computed twin.
  `(c1 + 1)` is a RecordConstructorValue and `c1 + 1` an ArithmeticValue, so no
  semantic matcher equates them at construction either — measured, that arm still
  plans. It needs the NORMALIZATION step, not a guard, and the same is true of
  `paren_twin_aggonly` / `paren_twin_proj` / `paren_twin_having` / `cmp_twin`.
  Do not credit the construction guard with those four.

  **THE THREE POST-AGGREGATE GUARDS ARE NOW UNREACHABLE FROM SQL, and are kept
  and unit-driven rather than deleted.** Measured over the whole
  `//pkg/relational/sqldriver` target **at 6158 subtests**: the three sites are
  consulted **797 times** (binder 414, computed 360, FieldValue walk 23) and NOT
  ONE consultation sees more than one match — the construction guard refuses
  first, exactly as Java's ordering predicts. They are kept because Java keeps
  its assert at every pull-up site and because one normalization change separates
  "no duplicate reaches here" from "one does"; deleting them would re-arm silent
  first-match at three sites at once. Every arm is driven from
  `pkg/relational/core/embedded/group_key_pull_up_guard_test.go` (8 tests), so
  unreachable never means untested.

  **FIRST-VS-LAST: fixed, and NOT observable in the message.** All three walks
  kept their LAST matching key while the collector documented "keeps only the
  FIRST" — two directions in one mechanism. Now all take the first, matching
  Java's Assert throwing on the first ambiguous sub-expression. The choice cannot
  be pinned by asserting the reported column: the name derives from the matched
  key's column and matching requires the same column, so every key in an
  ambiguous set renders the same name. What IS pinned is the slot on a unique
  match (asserted as a value, not "something bound") and the collector's
  first-wins contract.

  **Mutation-checked, DISJOINT arms**, each confirmed landed before running:
  construction guard → reddens the join arm and the scalar-subquery arm;
  `matchCount > 1` → the FieldValue-walk unit arm only; `keyMatches > 1` → the
  binder unit arm only; `matches > 1` → the computed-walk unit arm only.

  **STILL OPEN for the normalization step:** the four paren/cmp probes
  (`paren_twin_aggonly`, `paren_twin_proj`, `paren_twin_having`, `cmp_twin`),
  MEASURED against the live JVM as still `java_42702_go_plans` in the same run
  that closed the join one — Go answers all four.

  **`join_qualified_vs_bare` MOVED TO `both_42702`, measured against the live
  JVM, not asserted from the Go side.** The cross-engine probe reddened on the
  pre-commit hook the moment the construction guard landed, with its own message
  naming the remedy, and the expectation was moved in the same commit. It is a
  bare `SELECT COUNT(*)` with no projected key and no HAVING — the shape the
  post-aggregate-only guards provably could not reach — so it is also the
  cleanest evidence that the guard belongs at construction.

- [x] **The former `groupByOutputOrdinals` last-wins store and its
  `groupByOutputBaker` consumer are retired.** Their pre-RFC-232 corpus
  measurement found no consultation on a collided name; RFC-232 then removed
  the compatibility channel altogether and now carries each aggregate output
  as an exact ordinal value.

  The live mutation-sensitive guards are
  `//pkg/relational/core/embedded:embedded_test -test.run='TestGroupKeyPullUpGuard_(ConstructionKeepsTwoQuantifiersApart|ExactBoundaryBinderRefusesAMultiMatch)'`.
  They prove distinct owners do not collapse and an ambiguous exact boundary
  declines, without depending on a display-name map or a retired test helper.


## factorycorpus/full stalls at 6x its runtime, and master cannot see it
- [x] **A pushed LIMIT hid the covering-index rewrite.** `SELECT id FROM rp
  WHERE region = 'eu' ORDER BY plan DESC LIMIT 1` planned a FETCHING index scan
  where a covering one was available. Not a cost bug and not an index-matching
  bug — an EXPLORATION gap. FIXED by deleting the Go-only
  `PushLimitThroughProjectionRule` (rule + tests + rule-set entry), which is
  what Java's structure already implies: Java carries a row limit in
  `ExecuteProperties.setReturnedRowLimit()` at execution and has no
  limit-pushing planner rule at all, so there is nothing to push and nothing to
  prune against.

  ```
  before: Project([_current.ID#0], Limit(1, IndexScan(IDX_REGION_PLAN, [=, *]) REVERSE))
  after:  Limit(1, Project([_current.ID#0], IndexScan(IDX_REGION_PLAN, [=, *] COVERING) REVERSE))
  ```

  The mechanism, because it generalises: the rule ran in REWRITING, and
  `OptimizeGroupTask`'s partition-retention block is gated to PLANNING, so
  REWRITING pruned to the single pushed survivor and the un-pushed
  `LogicalLimit(Projection)` — whose inner group holds the covering winner —
  never reached the phase where the covering rewrite runs. Instrumented at
  `ImplementLimitRule`: with the push enabled it ran exactly ONCE, over the
  pushed `Limit(IndexScan)`, and never over the original. The cost model was
  never consulted; the better member was ABSENT, not outranked. Any REWRITING
  rule that rebuilds a node above a prunable group can do this again.

  Pinned in `embedded.TestLimitOverProjectionKeepsTheCoveringRewrite` (the
  reachability half, no FDB needed) and in `order_by_elimination.yaml`, whose
  COVERING expectation is restored. Blast radius was four items: that entry,
  `limit_join.yaml` test[2] (now pins `Limit(1, ` — its intent is the
  `plan_not_contains: NestedLoopJoin` beside it, and the `FlatMap` it named was
  a nesting detail the push happened to expose), the rule-type census, and the
  probe test above, converted from a measured-negative to a positive pin.

  LATENT, NOT NEW: record NAMES leaving exact-type identity (Java's
  `Type.Record.equals` compares typeCode, nullability and fields only) changed
  which members the memo ADMITS and therefore the order rules fire in — the
  traversal walked into a hole that was already there. Three other
  `order_by_elimination` entries moved at the same time and were pure
  LIMIT/PROJECT nesting ties; two of them pinned OPPOSITE nestings for the same
  query modulo `DESC`, which is what a pinned tie looks like. Those are now
  pinned on what the query determines (no `InMemorySort`, the reverse scan).

- [x] **The RFC-201 factory corpus plan-shape re-bless — DONE, and the tool's
  generator lookup is fixed.** `cmd/factory-rebless-plan-shapes` matched every
  committed scenario against `factory.Candidates(seed)`, the DEFAULT generator,
  while the corpus records a generator NAME per file — so every file from a
  non-default generator was compared against an unrelated candidate and the tool
  refused on the first one with a "feature vector moved" report of a family
  change that never happened. It now buckets by (generator, seed) and resolves
  through `factory.CandidatesForGenerator`, the same lookup
  `determinism_test.go` uses; `recipe_lookup_test.go` pins both halves.

  With the correct lookup the drift is **245 of 8150 scenarios across 38 family
  files**, and the whole corpus diff is **490 lines: 245 `# plan-shape:` and 245
  `# dedup-key:` headers, nothing else** — no query, schema, setup or frozen
  row moved, which is what says this was a rendering change and not a rows
  regression. `TestFDB_FactoryCorpusFull` re-executes the re-blessed corpus
  against a real cluster.

  Authorized by `retirements/2026-08-16-rfc232-exact-ordinal-resolution.json`
  (base commit 2e4c5ebec). Two causes are recorded there: sort keys now render
  from the Value the plan evaluates rather than the spelling it was constructed
  with, and a nested read on an outer join's null-supplying leg now reanchors
  onto the joined carrier. The second was a live BUG the drift concealed — see
  `TestReanchorCrossesANullabilityWidenedLegRootIntoANestedPath`.

## RFC-232 plans an IN-list query ~7x slower than master, and the memo is why

Not per-row and not the chosen plan: `SELECT id, val FROM t WHERE val IN (…)
ORDER BY id` produces a byte-identical plan on both trees, and the branch takes
**37.3 ms/op against master's 5.3** on `BenchmarkInListExecution`
(pkg/simfdb/hunt/sqlhunt, file byte-identical on both trees). Measured with the
SIMULATOR EQUALISED — master carrying this branch's two pkg/simfdb allocation
commits — so the difference is the engine.

Planning is on the hot path at all because **the plan cache never survives a
`database/sql` round trip**: `ResetSession` invalidates it and the pool calls
that on every connection reuse, so every query re-plans. True on BOTH trees.
That is a separate, pre-existing defect and a large one; it is what makes
planning cost user-visible at all.

The chain, each step reproducible:

- pprof: the branch's timed loop is ~100% `cascadesGenerator.Plan`.
- The planner's OWN counter (`p.tasksRun`): the SELECT runs **2271 tasks vs
  master's 734** (3.09x). The INSERT in the same run is **41 on both** — a
  built-in control showing this is shape-specific, not global.
- Task mix scales uniformly (TransformExpr 1989/825, ExploreGroup 1745/494,
  OptimizeGroup 612/129, InitiatePlannerPhase 8/8), which is the signature of a
  bigger memo rather than one rule misfiring.
- Memo members that HASH EQUAL but compare UNEQUAL, counted in
  `PreparedMemberDuplicateWithHashes` on both trees (probe presence verified on
  each): branch **1203**, master **105**. By type, branch/master —
  ProjectionPlan 828/45, InJoinPlan 144/0, InMemorySortPlan 108/18,
  PredicatesFilterPlan 90/0, FetchFromPartialRecord 33/0, LogicalFilter 0/42.
  Projections are 69% of the branch's.

RULED OUT by experiment, each reverted: semantic HASH granularity (coarsening
the QOV hash to master's bare tag changed the task count by ZERO), and QOV
semantic EQUALITY on the type axis (both ignoring the exact type entirely and
using `exactRowShapesAgree` changed it by ZERO). The over-discrimination is not
Value-level type identity.

RESOLVED — and the duplicate-member reading above was the SYMPTOM, not the
cause. The projection duplicates are real (198 distinct hash-equal-but-unequal
projection pairs on the small reproducer, zero on master), but canonicalising
their anchoring removes all 198 and moves the benchmark by ~4%. They were
downstream of a much larger population.

The cause is an ordering-space mismatch that RFC-232 exposed. Java rebases a
sort's ordering values from the inner quantifier's alias onto
`Quantifier.current()` before pushing the constraint
(`PushRequestedOrderingThroughSortRule.java:77-85`, and again inside
`RequestedOrdering.pushDown`); Go pushed the sort keys verbatim. That was
invisible while Go's sort keys carried no correlation — an unrooted key rebases
to nothing and pushes down through anything — and became live the moment
FieldValues carried an exact root.

The request then arrived at the child rooted at a correlation nothing below the
sort has heard of, the select below could not express it over its result,
declined every part, and returned **Preserve**. A Preserve request is satisfied
by EVERY access path, so the zero-prefix gate in data access stopped discarding
useless full index scans: `WHERE cat IN (...) ORDER BY id` kept IDX_VAL — an
index on a column the query neither orders by nor filters on — as a candidate
beside IDX_CAT and the primary scan.

- [x] Fixed in `requestedOrderingAtInnerCurrent`, applied at all three push
      sites (`PushRequestedOrderingThroughSortRule`, `ImplementSortRule`,
      `ImplementInMemorySortRule`), plus the select push-down's mirror-image
      half: it passed the CHILD's alias as the upper base of `Value.pushDown`
      where Java passes `Quantifier.current()`. Measured on the IN-list shape:
      planner tasks 2247 -> 829 (master 734), memo members 42 -> 26 (master 25),
      IDX_VAL kept 58 -> 0 (master 0), wall clock 39.5ms -> 9.9ms (master 5.25).
      `TestOrderingRequestSurvivesSortThroughSelectToScan` drives sort ->
      select -> scan in one test and mutation-checks every arm.
## RFC-232 still costs 1.26-1.7x master on three benchmarks, and what is left is measured

**SUPERSEDED for the RATIOS — read "RFC-232 overhead after the row-path and merge
campaign" at the end of this file for the current numbers, which cover nine
benchmarks and the 1M stress suite rather than three benchmarks. The "what is
left" list below is still live: those items were not what the campaign closed.**

The branch's planning and per-row overhead was ground down from up to 7.5x to
the numbers below. Each side was measured with the SIMULATOR EQUALISED (master
carrying this branch's two `pkg/simfdb` allocation commits), so the comparison
is of the engine.

| workload | at branch start | now | master | ratio |
|---|---|---|---|---|
| `TestStatsInvariant_PurePlannerSweep` (pure planner) | 253s | 194s | 151s | 1.28x |
| `BenchmarkInListExecution` (plan + execute one query) | 39.5ms | 8.9ms | 5.25ms | 1.70x |
| `BenchmarkScanAllWide` (20k rows, 30 iterations) | 89.5ms | 82.5ms | 64.8ms | 1.27x |
| allocated bytes, planner sweep | 238GB | ~124GB | 102GB | 1.22x |

The wall clock tracks allocated bytes almost exactly on this workload (GC mark
is ~44% of samples on BOTH trees), so allocation is the lever and the remaining
gap is the remaining allocation delta. What closed: interning the exact types
(primitives statically, composites through a probe that runs before the node is
built), shrinking `part` from 312 to 96 bytes, a real singleton for the empty
alias map, a list instead of a hash map for per-row edge bindings, and moving
the read-only type readers onto the shared thawed graph.

What is left, in order, all of it branch-only unless noted:

- `exactType.thaw` — 13.8GB, of which 90% arrives through the PUBLIC
  `QuantifiedObjectValue.Type()`. That accessor is pinned to return a fresh
  graph by `TestRFC232QOVSnapshotsAndDefensivelyThawsItsType` and the pin is
  right, so the saving has to come from the CALLERS. The three biggest are
  `values.PullUpValue` (1.75GB), `plans.newPlanExprBaseWithProperties` (1.24GB)
  and `cascades.admitMemoExpression` (0.94GB), and all three are
  thaw-then-re-snapshot round trips: they want the exact handle the value
  already carries. An `ExactTypeOfValue(Value) (ExactTypeHandle, bool)` that
  returns `qov.flowed` directly removes both halves at once, and is now exactly
  equivalent because interning makes the round trip return the same object.
## RFC-232 overhead after the row-path and merge campaign

Supersedes the ratios in "RFC-232 still costs 1.26-1.7x master on three
benchmarks" above.

**Population of the "now" column, because the first attempt at this table was
confounded and the confound was invisible:** baseline is the branch's true
MERGE-BASE `7d0435536`, not an older master — an earlier baseline sat at
`9a39b5006`, three commits behind, which put the branch side on Go 1.26.6 and the
master side on 1.26.5. `MODULE.bazel` derives the Bazel Go SDK with
`go_sdk.from_file(go_mod = "//:go.mod")`, so a `go.mod` version bump reaches the
benchmark binaries and that was a compiler difference sitting inside the ratio.
Both sides are now Go 1.26.6, with the SIMULATOR EQUALISED (the baseline carrying
this branch's two `pkg/simfdb` allocation commits) so the comparison is of the
engine, built sequentially from distinct binaries (md5-checked distinct) and run
sequentially at `-test.benchtime=1s -test.count=3`.

Re-measured that way, every ratio below held to within 0.04 of the confounded
first attempt — so the confound was real but not material. That is now measured
rather than assumed. Caveat on the statistics: at n=3 benchstat reports `~` for
every row because p=0.100 is the floor for a 3+3 Mann-Whitney, so the evidence
here is the tight spread (time ±1-3%, alloc/op ±0%), not a significance test.

**The 1M stress figures were re-measured at the merge-base too, and one of them was
REFUTED — the only claim in this campaign whose conclusion the confound actually
changed.** `group_by_customer_having` was booked at **0.98x** (parity with master).
At the true merge-base it is **1.54x**. Two samples per side, very tight: baseline
0.49s / 0.51s, branch 0.78s / 0.76s.

The reason is the part the earlier caveat got wrong. It assumed the confound could
only shift a ratio by the ~0.04 the micro-benchmarks showed. But the stale baseline
was missing #750 and #751, which changed `cascades_generator.go` by 266 lines — and
master got materially FASTER on this particular query as a result. So 0.98x was not a
noisy version of the truth; it was measured against a slower master. **A confound is
not a bounded error term.** It moved one row by 0.56x while leaving the other nine
within 0.04, and there was no way to know which from the size of the others.

The whole-suite figure did survive: **baseline 174.58s, branch 178.00s = 1.020x**
(samples 173.99/175.17 vs 177.86/178.14), against the 1.026x booked earlier.

Conditions, so this can be seen to go stale: baseline `7d0435536` + the `pkg/simfdb`
equalisation patch, both sides Go 1.26.6, built and run SEQUENTIALLY, load average
2.1-3.6 throughout on 24 cores, `--nocache_test_results`, 24 `=== RUN` lines confirmed
on every run. Sequential execution is what makes the ratio valid under non-zero
background load: both sides see the same machine.

Ratios are branch / master. "Before" is the head at the start of this campaign,
which is where the superseded block's numbers were taken.

| benchmark | ratio before | ratio now | branch allocs/op vs master |
|---|---|---|---|
| `BenchmarkScanAllWide` | 1.31x | **1.17x** | 645k vs 684k — below |
| `BenchmarkScanOneColumn` | — | **1.17x** | 645k vs 684k — below |
| `BenchmarkScanOrdered` | — | **1.17x** | 648k vs 686k — below |
| `BenchmarkPlanInList` | 1.39x | **1.25x** | 750k vs 778k — below |
| `BenchmarkIndexRange` | 1.33x | **1.25x** | 232k vs 257k — below |
| `BenchmarkAggregateGroupsPlain` | 2.07x | **1.26x** | 161k vs 168k — below |
| `BenchmarkAggregateGroupsHaving` | 2.03x | **1.35x** | 176k vs 177k — below |
| `BenchmarkScanFilterSparse` | 1.63x | **1.53x** | 14.3k vs 9.3k — **ABOVE** |
| `BenchmarkInListExecution` | 1.70x | **1.56x** | 82.9k vs 57.3k — **ABOVE** |
| `group_by_customer_having` (1M stress) | 1.88x | **1.54x** | — |

Whole-suite wall clock on the 1M stress test, at the merge-base and n=2: **master
174.58s, branch 178.00s = 1.020x**.

Allocation COUNTS are below master on **7 of the 9** sqlhunt benchmarks — which is
what the table above shows, row by row. Read the count off the table, not off this
sentence.

One of those seven is not worth leaning on: `AggregateGroupsHaving` is 176k vs 177k,
a 0.6% gap, well inside the `±0%`/`~` noise this entry declares two paragraphs up.
So **six** are below by a margin that means anything, and the seventh is below only
by sign. State it that way rather than picking one number.

A draft of this entry said "6 of 9" and claimed the re-measurement had REFUTED an
earlier 7-of-9. That was wrong in both halves: the count is 7, and the earlier 7 was
never refuted — the noise-margin judgement about `AggregateGroupsHaving` was being
silently folded into a count while the table beside it still said "below". The
correction was the error, not the thing it corrected.

The two genuinely above are `ScanFilterSparse` and `InListExecution`, and they are
also the two worst time ratios (1.53x, 1.56x) — which is one story, not two: both are
dominated by PLANNING rather than by row throughput, so the row-path work in this
campaign could not touch them. They are the workloads the plan-time-rebind milestone
below is aimed at. What closed the rest: minting a scan/projection row already carrying its plan's layout so the
output boundary takes an identity fast path instead of copying every row; one
per-row allocation for the frontier binding holder instead of three; a
`RecordCursorResult` that holds its value inline instead of boxing it once per row
per cursor level; a two-entry comparison-key program cache on the merge legs; and
eliding the compensation projection when an aggregate leaf already publishes the
GROUP BY row.

Those nine benchmarks are the instrument this whole campaign was measured with, and
until `bench-ci` gained `//pkg/simfdb/hunt/sqlhunt:sqlhunt_test` NOTHING ran them —
`bazelisk test` never passes `-test.bench`. They are gating there now.

- [x] Re-measure the two 1M stress figures at the true merge-base `7d0435536` —
      DONE, n=2 per side, and it REFUTED one of them. `group_by_customer_having` is
      1.54x, not the 0.98x parity booked from the stale baseline; the suite total held
      at 1.020x. The entry above carries the numbers and the reason. Note for next
      time: the prediction in this item — "the conclusion survives either way, so this
      is about the NUMBERS" — was WRONG, and wrong in the direction that matters. A
      confound is not a bounded error term just because it was small on nine other
      rows.

### Two more milestones the review surfaced, both port-faithful

- [x] **Memoize the structural key / structural hash per plan object.** SHIPPED — see
      the resolution paragraph at the end of this item before reading the analysis
      below, which is preserved as the reasoning that led there and contains one
      superseded claim called out in place. Java computes
      each expression's structural hash ONCE per object —
      `Suppliers.memoize(this::computeHashCodeWithoutChildren)` at
      `cascades/expressions/AbstractRelationalExpression.java:43`, exposed as
      `final int hashCodeWithoutChildren()` (`:58`), with `RelationalExpression`
      short-circuiting `semanticEquals` on it — and its `equalsWithoutChildren`
      allocates nothing. **Go has 132 non-test `structuralKey()` call sites and ZERO
      memoization** (measured: `grep -rn 'structuralKey()' pkg/ --include='*.go' |
      grep -v _test.go | wc -l` = 132; `sync.Once|cachedHash|hashOnce|memoHash` in
      `pkg/recordlayer/query/plan/plans/` = 0 non-test, positive control 23 non-test
      `sync.Once` in `pkg/recordlayer/` — 27 if tests are counted, and the two numbers
      are over different populations, so the non-test one is the control that matches
      the zero). Every `Equal()` and every `Hash()` rebuilds a key.

      **PREREQUISITE NOBODY HAD BOOKED, found by checking the precondition rather than
      accepting it: Go's plans are NOT uniformly immutable after construction, and the
      memo cannot be a plain field on the shared base.** Two `RecordQueryAggregateIndexPlan`
      builders wrote to the receiver and returned it, alone among 57 siblings that do
      `cp := *p` — and one of them, `WithLiveGroupsOnly`, writes a field that IS folded
      into `structuralKey`. Fixed to copy, pinned by
      `TestAggregateIndexBuildersCopyRatherThanMutateIdentity`, but the shape of the
      problem is what matters here:

      Those 57 `cp := *p` copies are the real obstacle. A memo held on `PlanExprBase`
      would be inherited by every shallow copy — and `WithXxx` exists precisely to
      change identity-bearing fields (`scanComparisons`, `liveGroupsOnly`,
      `keyComponentTypes`), so the copy's inherited key would describe the pre-change
      plan. The memo would then serve a stale identity and the dedup would intern two
      structurally different plans as one, which is exactly the failure the
      `liveGroupsOnly` key comment warns about. There is no compiler help: forget one
      copy site and it fails silently. An `atomic.Pointer` memo makes it louder but not
      safer — `go vet`'s copylocks would reject all 57 copies, and the package imports
      no `sync/atomic` today.

      Java does not face this because its plans have no `WithXxx` mutators at all;
      `Suppliers.memoize` sits on an object built once by a constructor. So the design
      question posed here was whether Go routes every plan copy through one helper that
      clears the memo, or drops the copy-builders in favour of constructors.

      An earlier draft of this item ALSO asserted, a few lines below, that "Go's plans
      are immutable after construction, which is the same precondition
      `Suppliers.memoize` relies on". That directly contradicted the prerequisite
      recorded above and was the older, superseded text — written before the two
      `RecordQueryAggregateIndexPlan` builders were found writing their receiver. It is
      removed rather than left to be read as agreement, since a reader arriving at it
      first would conclude the precondition already held.

      The other half of that paragraph stands and is not superseded: this campaign's
      answer to the term was to SHRINK the key — `part` 312 -> 96 bytes,
      `structuralKeyInlineParts` 8 -> 4 — a real ~6x win on a cost **Java does not pay
      at all**. Do NOT delete `plan_structural_key_size_test.go`: a memoized key still
      gets built once per object, so its size still matters, just less.

      **RESOLUTION — the answer was NEITHER of the two options above.** No copy helper,
      no move to constructors. The memo is a `*hashMemoCell` on `PlanExprBase` holding
      an `atomic.Pointer[hashMemoState]`, where the state pairs a hash with the plan it
      was computed FOR. An atomic reached through a pointer is never itself copied, so
      `cp := *p` keeps compiling and `go vet`'s copylocks has nothing to reject; the
      cell is shared by the copy, and correctness comes from an owner check enforced on
      BOTH sides — a copy that finds the cell foreign-owned computes its hash and
      declines to store, because an unconditional store would make the two evict each
      other on every comparison. The cell starts empty, and laziness is a correctness
      requirement rather than an optimisation: the `WithQuantifiers` rebuild paths write
      `drivingAlias` / `outputNameOverrides` / `distinctProofIndexName` — all in the key
      — onto an already-constructed plan.

      **The immutability precondition is now TRUE AND ENFORCED, which it was not when
      this item was written.** Three further in-place setters (then named `SetInValues`,
      `SetSourceKind`, `SetInSources`; now `WithXxx`) turned up beyond the two aggregate-index ones and
      were converted to copying builders, and `pkg/docscheck`'s
      `TestMemoIdentityTypesNeverWriteTheirReceiver` now ratchets it at zero over the 67
      types whose fields ARE memo identity. So the precondition the older text asserted
      on faith is the thing that finally holds — by gate, not by assumption. That
      matters specifically for this memo: an in-place write leaves the pointer
      identical, so the owner check passes and serves a pre-mutation hash for
      post-mutation content, which is the one staleness an identity check structurally
      cannot see.

      Measured 1.184x -> 1.150x against `7d0435536` on the pure-planner sweep (2.8%).
      That baseline is no longer the merge-base and the ratio is not "vs master" any
      more; see the re-measurement at the end of this file. Reviewed and ACK'd.

## Every self-hosted CI job dies at `actions/checkout` with HTTP 429, nightlies included

Found while trying to get PR #752 green; it is repo-wide and not specific to that
branch. **Not a test failure** — the jobs never reach a build step. Zero `DATA RACE`,
zero test output, no code executed.

The signature is "died at an ACTION-TARBALL download", not a fixed log length, and
that distinction matters because the first shape found was uniform and the second was
not. Most failures are a 21-line log: three `429 (Too Many Requests)` from
`codeload.github.com` for `actions/checkout`, then `Failed to download archive ...
after 3 attempts`. But a 24-line variant *cleared* `actions/checkout` on retry and then
died the same way on `actions/setup-go`, and its warnings include a **503 Service
Unavailable** as well as 429s. GitHub's own GraphQL API was returning 503s in the same
window (a `gh pr comment` failed that way), so the TRIGGER was a platform-wide
degradation rather than a steady-state rate limit on this repo.

EXPOSURE SCALES WITH HOW MANY ACTIONS A JOB FETCHES, which is why one job kept
looking like the "real" failure while its siblings passed. `ci.yml` has 8 `uses:`
against 2 each in `claude.yml` and `hosted-smoke.yml`, and its `Build, Lint & Test`
job pulls `checkout` + `setup-go` + `upload-artifact` — three independent chances to
lose the dice roll. Observed exactly that: one run cleared `checkout`, cleared
`setup-go` on a retry, then died on `upload-artifact`. So a job dying while its
siblings pass is NOT evidence that the failure is specific to that job's work.

That does not change the fix, and it is the reason the fix is worth doing: a runner
with a local action cache is immune to a codeload incident, whereas one that refetches
per job fails every job for the duration. Do not expect the 21-line log as a
fingerprint — match on the download failure, whichever action it names.

**IT IS NOT A CLEAN RUNNER SPLIT, and an earlier draft of this entry said it was.** That
draft claimed `ubuntu-latest` jobs were "green on every run" because "GitHub-hosted
runners resolve actions through an internal cache". Refuted: `hosted-smoke.yml`, which
is `runs-on: ubuntu-latest`, died on `actions/setup-go` with the same three 429s at 36
log lines. **GitHub-hosted runners fetch action tarballs from codeload too.** The
observation behind the wrong claim was real — hosted jobs did pass far more often — but
the explanation was too strong, and it was built from a run-level status list that
happened to show successes.

| workflow | `runs-on` | `uses:` per file | observed |
|---|---|---|---|
| `hosted-smoke.yml` | `ubuntu-latest` | 2 | mostly green, **but has died on the 429** |
| `claude.yml` | `hetzner-fdb-vm` | 2 | dies |
| `ci.yml`, `nightly-*.yml` | `hetzner-fdb-vm` | 8 | dies most often |

What actually predicts exposure is the NUMBER of action tarballs a job fetches, not the
runner. Six sequential `gh run rerun --failed` attempts produced 24 consecutive job
failures and got WORSE rather than clearing, so retrying is not the remedy — each
attempt is another codeload request. What eventually worked was retrying patiently
across ~40 minutes as the platform recovered.

The fix below is therefore justified more narrowly than that draft implied: it is not
"self-hosted is uniquely broken", it is that **a self-hosted runner is the only one we
can give a local action cache to.** GitHub's runners are not ours to configure.

**Three consequences, and the second and third are the expensive ones.**

1. PR CI cannot go green on any branch while the window is open. It DID go green on
   this branch once the platform recovered — 6/6 checks on eec351505 — so the outage is
   transient rather than permanent; the point is that it consumed most of a shift and
   the retries had to be classified by hand to avoid reading an infra death as a test
   failure.
2. **The `@claude` review gate is silently unavailable.** `claude.yml` is
   `issue_comment`-triggered and runs on the self-hosted VM, so a review request posts
   the comment, the workflow fires, and it dies at checkout — leaving a PR comment with
   no reply. That reads exactly like "the reviewer has not got to it yet". Confirmed on
   run 32036682884: fired, `completed/failure`, 22-line log, 3 429s.
3. **Every nightly safety net is down** — fuzz, rowdiff, factory, oracles, coverage,
   stress, libfdbc differential are all `hetzner-fdb-vm`. A green-looking absence of
   nightly failures currently means the nightlies are not running, which is the
   fail-open direction.

### Measured and REJECTED: the group_by_customer_having residual is not one term

Profiled the 1.54x query's proxy (`BenchmarkAggregateGroupsHaving`, plan+execute, 3s,
CPU + alloc profiles both sides at the merge-base) to find the lever before building
anything. There isn't one. `go tool pprof -diff_base` over a 10.92s sample shows NO
node above ~2% of total: the largest positive flat deltas are `RecordType.Equals`
(+0.14s, 1.28%), `initEdgeObjectBinder` (+0.10s), `mapResultCursor.OnNext` (+0.05s),
against a per-op gap of ~4.1ms. **The regression is diffuse across the row path, not
concentrated**, which is why the campaign's wins came from removing whole allocations
rather than from speeding any one function.

Two things tried and rejected, recorded so they are not retried:

- **A pointer-identity fast path on `RecordType.Equals`.** The reasoning was good and
  the measurement killed it: the comparison is structural and recursive and runs per row
  per edge in `initEdgeObjectBinder`, and the branch's operands are interned
  (`SharedFlowedType`) where master's are not, so it looked like a differential win.
  Measured: `AggregateGroupsHaving` 16.098ms -> 16.179ms, i.e. noise, and no benchmark
  moved. Reverted. `RecordType.Equals` at 1.28% of samples cannot pay for a 34% gap.
- **Instrumenting that comparison to count pointer coincidence** returned
  `sameObject=0 different=0` across the whole executor package — the record arm never
  fired in those tests, so the profiled `Equals` cost comes from elsewhere. Anyone
  re-opening this should find the real caller first rather than assuming the edge binder.

What this means for the milestone above: structural-key memoization is still the right
next lever for the PLANNING-dominated benchmarks (`ScanFilterSparse` 1.53x,
`InListExecution` 1.56x, whose allocation counts are ABOVE master), but it should not be
expected to close `group_by_customer_having`, which is execution-dominated and diffuse.
Those are two different problems and this entry exists so they are not conflated.

### Structural-hash memo: rolled out to all 42 plan types, measured on the right population

Design ACK'd (helper-free variant: a constructor-allocated cell reached by pointer,
owner-checked on read AND write). All 42 `HashCodeWithoutChildren` declarations in
`plans` now route through it — verified by walking each declaration body, not by a grep
count, since a looser grep reports 46 by counting doc-comment lines.

Measured on `TestStatsInvariant_PurePlannerSweep`, which is the population this term was
measured over in the first place — the SQL benchmarks under-show it because the cost
lives in the memo's member loop, not in any one query. Same machine, sequential,
`--nocache_test_results`:

| tree | samples | mean | vs `7d0435536` |
|---|---|---|---|
| `7d0435536` (merge-base AT THE TIME) | 149.04, 149.95 | **149.50s** | — |
| pre-memo (`f4f7d7bc5`) | 177.14, 176.79 | **176.97s** | 1.184x |
| all 42 memoized | 172.51, 171.48, 171.85 | **171.95s** | **1.150x** |

Within-group spreads are 0.2-0.6%, well under the 2.8% the memo moves, so this is signal
rather than noise. The ratio against `7d0435536` goes 1.184x -> 1.150x.

**THE "vs master" READING OF THAT LAST COLUMN IS DEAD — do not carry it forward.**
The column compares against `7d0435536`, which was the merge-base when these rows
were taken and is not any more: `f4f7d7bc5` has since merged and master is
`d31bf28e0`. So the 1.184x baseline for the memo is now MASTER's own code, and
against the current merge-base the branch is 0.958x — faster, not slower. The
re-measurement is the last entry in this file; read it before quoting any number
from this table.

Why it is only 2.8% when `newStructuralKey` was measured at 8.9GB: the memo removes the
key rebuild on a HIT, and a hit requires the same plan OBJECT to be hashed twice. The
memo's member loop does exactly that — it re-hashes each stored member against every
probe — but a plan that is hashed once and discarded, which is most of what a planner
sweep constructs, never gets a second read. The remaining term is therefore key
construction for plans that are compared once, and that is not memoizable; it is the
`part`/`structuralKey` size work this campaign already did.

- [x] Next lever for the planner sweep — the copies — INVESTIGATED, PRICED, AND
      REJECTED. Do not re-derive this; the numbers are below.

      The hypothesis was that a `cp := *p` rebuild shares its original's cell and so
      never memoizes, leaving the memo dark on exactly the plans a rewrite rule touches
      most. **The census refutes it by a factor of three.** Instrumenting the cell over
      `TestStatsInvariant_PurePlannerSweep`:

      ```
      reads=3071048  HIT=2223470 (72.4%)  MISS_EMPTY=649360 (21.1%)  MISS_FOREIGN=198218 (6.5%)
                     storeOK=649360        storeDeclined=198218
      ```

      The residue is dominated by ONCE-HASHED OBJECTS (21.1%), not by dark copies
      (6.5%). Those are unmemoizable by construction — a hit requires the same object to
      be hashed twice — so the 2.8% the memo buys is at the CEILING, not below it. Total
      hash-call cost is roughly `2.8 / 0.724 ~= 3.9%` of planner time (assumes uniform
      per-call cost and negligible lookup cost, so: an estimate, not a proof). Converting
      every foreign miss to a hit is therefore worth `0.065 x 3.9% ~= 0.25%`.

      **Read 0.25% as a GROSS benefit, not a net one, in both directions.** The 2.8% it
      is derived from is itself a NET measurement — hits saved a hash, but every read
      now pays a lookup and every miss pays a store plus an allocation — so `2.8/0.724`
      UNDERSTATES gross hash cost. And the lever's own cost is unpriced: one extra cell
      plus a state allocation at each of ~57 copy sites. The true net is below 0.25% and
      could plausibly be negative. That does not weaken the rejection, it strengthens it.

      A quarter of one percent at best, against loosening the field whose write-once
      discipline is the entire reason no atomic sits on the plan struct, across ~57 copy
      sites. Rejected by measurement rather than by taste.

      **Corrected invariant wording, since the strict phrasing would misdirect whoever
      revisits this:** the requirement on `PlanExprBase.hashMemo` is NOT "written only in
      the constructor" — it is "never written after the object may be SHARED".
      Publication safety, not immutability. A builder doing
      `cp := *p; cp.hashMemo = newHashMemoCell(); return &cp` writes to an object no
      other goroutine can observe yet and would need no atomic. So the hook EXISTS; it is
      the price that rules it out, not the mechanics. The same correction is recorded at
      `newHashMemoCell` in `plan_structural_hash_memo.go`, which points back here.

      The census itself is pinned as a test rather than left as a deleted probe:
      `TestMemoStoreDecisionIsDeterminedByReadClassification` (plans) asserts the read
      classification is TOTAL and the store decision fully determined by it — the pairing
      `storeOK == MISS_EMPTY` and `storeDeclined == MISS_FOREIGN` that held to the unit
      over three million operations.

      **A first draft of that test also claimed to detect the memo going DARK, and it
      did not. The claim was wrong and is recorded here because it is the instructive
      part.** Remove the memo read AND make `storeStructuralHash` decline whenever the
      cell is populated — the second half is behaviour-preserving on its own — and every
      census assertion still passed against a fully dark memo. The observer inferred
      "hit" from `before.owner == plan`, the same state the memo itself consults, so it
      could never disagree with the code it audited; and its value check was vacuous
      because a recompute is deterministic and returns the identical hash. Textbook
      paired-assertion vacuity, in a test written to prevent exactly that.

      What actually witnesses the read is `TestAMemoizedReadIsServedFromTheCell`: it
      plants a hash the recompute CANNOT produce, under the plan's own ownership, and
      requires that value back. Under the same dark-memo mutation all six other memo
      tests pass and only this one fails. Separately,
      `TestMemoIsCorrectUnderConcurrentSharers` exercises the dimension the atomic
      exists for and nothing covered — several goroutines hashing plans that share one
      cell — and `storeStructuralHash` now uses CompareAndSwap so the no-live-race claim
      holds globally rather than per-observation.

      The ROUTING is a separate failure mode and has its own gate:
      `TestEveryPlanRoutesItsStructuralHashThroughTheMemo` (docscheck) ratchets 42 of 42
      plan types, because the wiring was hand-applied at 42 sites and an unrouted type
      returns a CORRECT hash — a pure performance regression a green suite cannot see.

- [x] **Stop thawing a whole Type graph to answer a boolean.** DONE via RFC-233,
      but read the closing note appended at the END of this file first: this
      item's framing — that the waste is in the comparisons — was REFUTED by the
      after-profile. The comparisons were converted and bought 0.3% of allocation
      volume; the wall-clock win came from the two internal derivations. The
      original measurement below stands; the diagnosis of WHERE it sat did not.
      MEASURED, not
      suspected: `exactType.thaw` is the SINGLE LARGEST allocator in the planner at
      **8.87 GB of 112.01 GB (7.92%)** over `TestStatsInvariant_PurePlannerSweep`
      (`go test -memprofile`, 171s run, branch `perf/plan-structural-hash-memo`
      @ `ed4045adb`). For context the next two are `GetCorrelatedToOfValue.func1`
      at 8.17 GB and `newStructuralKey` at 8.10 GB.

      The planner is ALLOCATION-BOUND, which is why this is the right target and why
      the diffuse CPU profile found nothing: on the same run `runtime.gcDrain` is
      **42.72% cumulative**, `scanSpan` 31.61%, `mallocgc` 16.78%. No application
      node exceeds ~2.3% flat. Cutting allocation is the only lever that moves a
      profile shaped like that.

      `thaw` rebuilds an ordinary Type graph from an INTERNED, immutable exact
      handle — an identical graph every time. Its callers are the hot ones:
      `QOV.FlowedType` (28.5% of thaw), `exactType.Type` (28.0%),
      `physicalFlowedRecordType` (14.5%), `LayoutWithSeedLegs` (12.1%),
      `fieldValue.Type` (9.8%).

      **The waste is concentrated in comparisons.** Stated in OCCURRENCES, because
      `grep -c` counts LINES and 50 of these lines carry two calls — which is exactly
      the `a.FlowedType().Equals(b.FlowedType())` shape at issue. Over non-test
      sources under `pkg/`: 141 lines / 191 occurrences total (91 lines with one call,
      50 with two), of which 33 lines are `Equals`-shaped and 13 are `== nil` /
      `!= nil` existence checks. `QuantifiedRowShapesAgree` is a SEPARATE population,
      not a subset: 16 real call sites, only 10 of which also carry a `.FlowedType()`
      call. Each `a.FlowedType().Equals(b.FlowedType())` allocates TWO complete graphs
      to produce one bool.

      **The answer is `exactTypesEqual`, NOT handle pointer identity.** Identity is
      strictly STRICTER than `Type.Equals` and substituting it is wrong in the
      plan-changing direction: interning keys on the record/enum NAME while
      `RecordType.Equals` and `EnumType.Equals` deliberately ignore it (matching
      Java), so two Equals-equal records hold two handles and identity reports them
      different. `exactTypesEqual` compares canonical bytes whose encoding excludes
      the name precisely because `Equals` does, so it is Equals-equivalent by
      construction while keeping the O(1) same-pointer fast path.
      `exactRowShapesAgree` is the corresponding twin for `QuantifiedRowShapesAgree`.

      **`thaw` IS already memoized — for shared readers.** `exactType.thawCache` +
      `thawShared()` cache the thawed graph and ship publicly as
      `SharedFlowedType`/`SharedExactType`. What cannot be memoized is the PUBLIC
      promise that `Type()`/`FlowedType()` hand back a graph the caller may mutate:
      `rfc232_qov_exact_identity_test.go` mutates a `Type()` result
      (`first.RecordName`, `first.Fields[0].Name`) and requires a later `Type()` to be
      unaffected. So the fix is to stop CALLING `thaw` on the comparison paths, and to
      route read-only non-comparison readers through `SharedFlowedType`.

      The pattern to follow already exists in this file's own neighbourhood:
      `OrdinalDomainOfQuantified` takes the handle via `exactTypeOfValue`, answers from
      it, memoizes on the node (`exactType.ordinalDomain`), and falls back to the long
      way only for a foreign QOV view. `thawShared()` exists for the internal
      non-allocating case. So this is extending an established mechanism, not inventing
      one.

      The design is `rfcs/233-a-type-comparison-must-not-build-a-type.md`; read it
      before touching this item, and keep the two in sync — the RFC's §2 census and
      the numbers above are the same measurement and rot together. Query-engine
      change: needs a Graefe ACK on both RFC and implementation before merge, per the
      review-cadence rule. Not for PR #754, which is green and already ACK'd by three
      gates — this is its own change.

### The planner-sweep ratio was booked against a baseline that is no longer the merge-base

`1.150x vs master` is stale, and stale in the way that matters: the number was
right when it was measured and its MEANING inverted underneath it, because master
moved.

The table under "Structural-hash memo" states its baseline honestly as
`7d0435536` — that WAS the merge-base then. PR #752 has since merged and the
merge-base is `d31bf28e0`, 64 commits later. The decisive detail is that one row
of that table, `branch, pre-memo (f4f7d7bc5)`, names a commit that is now ON
MASTER: `f4f7d7bc5` is the second parent of `d31bf28e0`. So the whole 1.184x the
memo was clawing back is master's code, not the branch's.

Re-measured on the same machine, sequentially, same toolchain (`go.mod` and
`MODULE.bazel` are byte-identical across the range, so the Bazel Go SDK cannot
differ), `go test -count=1` on `TestStatsInvariant_PurePlannerSweep`:

| tree | samples | mean |
|---|---|---|
| `7d0435536` (the OLD merge-base) | 151.223, 151.249 | **151.24s** |
| `d31bf28e0` (the CURRENT merge-base) | 178.635, 178.029, 177.817, 178.140, 178.383, 178.013, 177.976 | **178.14s** |
| branch `cbbb8f850` | 170.653, 170.627, 170.676 | **170.65s** |

**Master itself is 1.178x slower on this sweep than the baseline every ratio in
that table was taken against.** That is the RFC-232 exact-resolution overhead —
already booked, and the whole subject of this campaign. It simply landed on
master while the branch was in flight, which is why the branch's number looked
like a regression it never had.

**Against the CURRENT merge-base the branch is 0.958x — 4.2% FASTER than the
master it forks from.** Taken as three ADJACENT base/head pairs so both sides see
the same machine within minutes of each other: 0.957, 0.959, 0.959. Within-side
spread is 0.03% on the branch and 0.46% on the base.

One sample is reported and excluded: a 178.904s branch run taken while the
machine was at load 4-5. It stands alone against six branch samples in
170.6-172.4 and against a base side that did NOT move under the same load, so it
is an outlier rather than a load effect — but it is written down here because a
discarded sample nobody can see is indistinguishable from one that was never
taken.

The generalizable lesson is not the stale-baseline rule already in CLAUDE.md.
That rule says the baseline must BE the merge-base at measurement time, and this
measurement obeyed it. What decays is a RATIO's MEANING: a ratio is a fact about
two trees, and naming one of them "master" makes it a fact about a moving
reference. Write the ratio against a SHA and say what that SHA was at the time,
or the number silently starts describing a comparison nobody made.

### RFC-233 closed: what it bought, and what its premise got wrong

Both gates ACK'd (Graefe on the implementation after two NAK laps; Torvalds on the
implementation after one). Codex is externally blocked until 2026-08-20 05:30 by a
usage limit — verified by probe, not assumed.

**What landed.** Non-test `.FlowedType()` occurrences under `pkg/` went 191 -> 102
(141 lines -> 83); `Equals`-shaped 33 -> 0; existence checks 13 -> 0;
`QuantifiedRowShapesAgree` call sites 16 -> 1. `thaw` went 8.87 GB -> 7.23 GB and
205M -> 175.7M objects. Wall clock **0.958x vs merge-base `d31bf28e0`** — the
branch is 4.2% FASTER than the master it forks from, taken as three adjacent
base/head pairs.

**What the premise got wrong, since it is the more useful half.** RFC-233 §2 said
"the waste is concentrated in comparisons" and built the case on a census of 33
`Equals`-shaped lines. Converting all of them moved total allocation by 0.3%. A
census counts SITES; it says nothing about the allocation attributable to them.
`go tool pprof -peek 'thaw$'` answers the second question directly, takes one
command, and would have shown before any code was written that thaw's callers are
`Type()` and two internal derivations. Most of the wall-clock win came from
`LayoutWithSeedLegs`, which was thawing a whole graph recursively to read
`len(record.Fields)`.

The generalisable form, and it is not the same as the existing "scope every
count" rule: **a census locates work; only a profile prices it.** Never let a
census stand in for the pricing when the profile that motivated the RFC can
answer it directly.

**Two defects fell out of the work, both fixed and pinned rather than filed.**
`QuantifiedRowShapesAgree` was PARTIAL — it normalised via `WithNullability`,
which refuses to flip RELATION, NONE and ANY, and so panicked on three type
classes, asymmetrically (only when the left operand happened to be nullable). And
a CTE reference's column-alias list renamed IN PLACE on a slice `legColumns` hands
back shared from `cteColumnsScope`, so one reference rewrote the definition's
schema for every later reader.

**Follow-on is RFC-234** (`rfcs/234-a-type-is-immutable-so-stop-rebuilding-it.md`),
which takes the target the profile actually names: `Type()`/`FlowedType()`'s
defensive rebuild, 72.1% of thaw at ~127M objects per sweep. It needs its own
Graefe+Torvalds lap before implementation.

### RFC-234 landed: the branch is 6.7% faster than its merge-base

`Type()`/`FlowedType()`/`fieldValue.Type()` return the shared thawed graph, and
`pkg/linters/typeimmutable` — a nogo analyzer, so it fails the BUILD with full
type information on every package including tests — makes the immutability that
licenses it an enforced rule rather than a census reading.

| tree | samples | mean |
|---|---|---|
| merge-base `d31bf28e0` | 178.070, 178.214, 178.033 | **178.11s** |
| branch `1cd9c3c22` | 166.158, 166.303, 166.008 | **166.16s** |

**0.933x**, paired ratios 0.9331 / 0.9332 / 0.9325. RFC-234 alone is worth 2.6%
(170.65s → 166.16s). Allocation: `exactType.thaw` 175.7M → 46.8M objects
(−73.3%), total 1.525B → 1.400B (−8.18%).

**The gate found what the suite could not.** Two mutations of accessor results in
`rfc232_field_value_test.go` stayed GREEN under the accessor flip, because
`FieldType()` and `ResultType()` still thaw privately — latent, and armed by this
RFC's own follow-on. Also a variadic `fields ...Field` numbered in place, which
writes the caller's slice whenever a caller spreads an existing one.

**What is left, priced in objects.** After this, `thaw` is 46.8M and its callers
are `physicalFlowedRecordType` (68.9%, 32.0M — thaws then WRITES leg boundaries,
so it needs the no-layout path split out before it can share) and
`resolveAgainstQOV` (22.4%, 10.5M — cold interning misses, irreducible). The next
lever after that is the correlatedTo/children cluster:
`GetCorrelatedToOfPredicate` at 96.0M objects cumulative and
`GetCorrelatedToOfValue` at 68.4M, both of which **Java memoizes** on
`AbstractValue`, `AbstractQueryPredicate` and `AbstractRelationalExpression` and
Go does not memoize at all. That is a straight Java-alignment gap and the largest
remaining one.

---

## Three identifier models coexist, and quoted names fall between them — CLOSED

**CLOSED by RFC-237** (`rfcs/237-a-name-is-normalized-once-at-the-parse-boundary.md`).
There is ONE model now: a name is normalized once, at the parse boundary
(quoted keeps its case, unquoted folds UPPER), and carried VERBATIM everywhere
after it; lookup is EXACT at a scope level, with an unambiguous case-insensitive
second pass at that same level before falling to the parent.

What that turned into, against the two measurements this entry was written
around:

- `SELECT q1."id" FROM q1 JOIN (SELECT "id","k" FROM q2) d ON q1."k" = d."k"`
  answers `[[1]]`, matching Java. Pinned in
  `quoted_identifier_columns.yaml` and as plain AGREEMENT (not as a pinned
  divergence) in `JoinUsingQuotedIdentifierJavaProbe`.
- `SELECT *` over a mixed-case quoted DDL column reports `[ID KeepCase PLAIN]`
  — byte-identical to Java, where Go reported `[ID KEEPCASE PLAIN]`. So does
  the explicit projection of that column. Pinned in
  `QuotedIdentifierCaseJavaProbe` and, for a fast harness, in
  `quoted_identifier_labels.yaml`.

The four ruled-out attempts recorded here were all correct as refutations and
all incomplete for the same reason: each moved ONE authority. The change that
worked moved every authority at once, which is why it needed the RFC.

---

## An identifier-agreement harness: perturb the QUERY, oracle on the edge check

RFC-237 closed a defect class by censusing `strings.ToUpper` call sites. That
instrument is the wrong one, and the evidence is the shape of what it missed
rather than the count:

- **`usingSource.owns` had no fold to find.** The census cleared the file, and
  the defect was then INTRODUCED by the fix — a relaxed lookup dropped into a
  cross-source adjudicator. A census of folds cannot see a defect whose
  signature is "correct call, wrong adjudication level".
- **the EXISTS/FlatMap projection arm was a fold and the census still missed
  it**, because the census was scoped to the files already being edited.
- **the CTE double-normalize had no `ToUpper` at the call site at all.** It was
  `StripIdentifierQuotes(FullIdToName(fid))`, where the inner call already
  normalizes. No sweep for a fold can find a fold spelled as a no-op.

Three reviewers found three disjoint live sites in one change. That is not
review working; it is a population that was never bounded.

**THE RIGHT INSTRUMENT** turns "did I find every fold?" — unanswerable — into
"does any authority disagree?", which is checkable. Two design constraints,
both established the expensive way and neither obvious:

1. **It must perturb QUERY identifiers, not only DDL ones.** The first sketch
   of this harness was "run the corpus with every DDL identifier quoted and
   case-shifted". That exercises descriptor → catalog → projection, and it
   would have been BLIND to the double-normalize: `WITH c("x")` is a SQL-TEXT
   construct that no amount of schema perturbation generates. Perturbing query
   identifiers is a materially different and less well-defined instrument, and
   that difficulty is the point rather than a reason to skip it.
2. **The oracle is the executor's own consistency check, not just labels.**
   `edge lookup X: read as RECORD(…), declared RECORD(…)` is what actually
   fired on three of these — it compares two naming authorities at runtime and
   is the only thing positioned to notice when they disagree. Label comparison
   catches the rest. Assert BOTH: the check never fires, and every reported
   label matches the authored spelling verbatim.

**BUILT, RUN, AND CLEAN — under RFC-237 §8, not deferred.** The reasoning
above for deferring it ("it will surface an unknown number of further
disagreements") was the deferral: an unknown number is a reason to go and
count, not a reason to schedule. Counted, it was **31, all one class**, and
fixing them was smaller than this write-up.

`TestIdentifierAgreementOverCorpus`
(`pkg/relational/conformance/explaindiff/identifier_agreement_test.go`) is the
gate. Constraint 1 above held and is what the built version does: it perturbs
QUERY identifiers off `UidContext` nodes, never DDL and never SQL text.

Constraint 2 did NOT survive contact, and the correction matters more than the
original claim. The `edge lookup` oracle needs EXECUTION, and a plan-text
comparison turned out to be both cheaper and strictly earlier — it caught all
31 without FDB, without rows, and without a running store. What the built gate
cannot see is written out in RFC-237 §8.2, measured by mutation rather than
asserted; the headline is that a perturbation of unquoted → quoted-UPPER is
blind to a FOLD, because those two spellings are exactly the pair a fold cannot
tell apart. That axis is covered by the yamsql `columns:` arms and the unit
pins, and the three instruments do not subsume one another.

---

## The rowdiff QOV binding failure — FIXED

**Root cause: `InComparisonToExplodeRule`'s single-element collapse re-minted
the inner quantifier.** The collapse (`col IN (v)` → `col = v`, reached whenever
the IN list DEDUPLICATES to one value) built a fresh `ForEachQuantifier` over
the inner reference and rebased the predicates onto its new alias. The
expression it yielded is an alternative in the SAME memo group as the original
filter, and a `LogicalFilterExpression`'s result value is a QOV over its inner
quantifier's alias — so that alternative published a DIFFERENT result
correlation from the group's other members. Any correlation held from outside
the group then resolved against an alias the chosen alternative does not carry,
and the executor failed to bind it.

The multi-element path in the same rule mints quantifiers safely because it does
not yield them bare: it wraps them in a `SelectExpression` whose own result
value re-exports the inner filter, keeping the new aliases encapsulated.
`FilterDropTruePredicatesRule` is the closer analogue to the collapse branch —
same inner, rewritten predicates — and it already reused `f.GetInner()`. The fix
makes the collapse do the same; the rebase went with the mint, since the
predicates already carry the original alias.

Localized by disabling one rule at a time on the failing query: of 15
candidates, only `InComparisonToExplodeRule` made it run clean.

It is an EXECUTION failure, not a planning one — the message comes from
`quantifiedObjectValue.Evaluate` (`values.go:5426`). Planning every affected
shape through `embedded.PlanQueryForTest` returns no error at all, so a
plan-only probe reports "cannot reproduce" against the live defect.

Verified against the independent Oracle M reference, not merely by the absence
of an error: seed 88001928 went from `comparisons=21 mismatch=1 mismatchRows=3`
to `comparisons=24 mismatch=0` — the three previously-failing queries now
execute AND return the reference rows.

Pinned by `TestFDB_QOVBindingMinimalShape`
(`pkg/relational/conformance/factory/qov_binding_shape_test.go`), which drives a
24-arm cross and now requires ZERO arms to error. Note the guard's direction is
INVERTED from the one it was born with: while the defect was live it floored the
count at 3 and watched for the shape moving; zero is now the steady state, so
the alarm is GROWTH — any arm erroring means the mint came back. Reverting the
fix reddens exactly the three arms below, which is how the guard was verified.

### The shape it had while live (kept so a future non-zero is readable)

The original entry below recorded the trigger as "an OUTER join, a
DUPLICATE-valued `IN` list, and an INDEXED column of the NULL-PADDED side",
measured over LEFT JOIN only. Re-measured over the full 24-arm cross that was
**wrong on two of its three clauses**: the discriminator is the join clause's
RIGHT-HAND relation whichever side that is (under `RIGHT JOIN` it is the
PRESERVED side and still failed), and the index is load-bearing only for `LEFT
JOIN` — `RIGHT … r.s IN ('b ','b ')` failed with no index on `s` at all. The
three failing arms were `LEFT r.c dup`, `RIGHT r.c dup`, `RIGHT r.s dup`.

The IN-list arity was narrower still: only a list of exactly TWO identical
elements failed. `IN (7)`, `IN (7,7,7)`, `IN (7,1,7)` and `IN (7,7) AND r.id > 0`
all ran clean, on fresh connections and in both list orders.

### Original entry (superseded, kept for provenance)

`ROWDIFF_SEED_START=3495589 ROWDIFF_SEEDS=1` reproduced on master; the sweep was
red on it. All three of the seed's failing variants differ only in their SELECT
list and fail identically with

```
resolution error 46 at qov.binding: exact QOV "q$NN" (T_RD row type) has no
declared runtime binding
```

Minimized against a 6-row `t_rd` with indexes on `a` and `d` (one run, all ten
shapes measured, harness preserved at
`scratchpad/qov_probe_test.go.txt` in the session that took it — reconstruct it
from this table, which is the part that matters):

| shape | result |
| --- | --- |
| `LEFT JOIN … WHERE r.a IN (1, 1)` | **ERR** |
| the same with no `ORDER BY` | **ERR** |
| the same ordering only the preserved side | **ERR** |
| `… WHERE r.a IN (1)` | OK |
| `… WHERE r.a IN (1, 2)` | OK |
| `… WHERE r.a = 1` | OK |
| `INNER JOIN … WHERE r.a IN (1, 1)` | OK |
| `LEFT JOIN … WHERE l.a IN (1, 1)` (preserved side) | OK |
| `LEFT JOIN … WHERE r.c IN (1, 1)` (`c` is NOT indexed) | OK |
| `LEFT JOIN …` with no `WHERE` | OK |

**The shape above is SUPERSEDED — it was measured over LEFT JOIN only, and two
of its three clauses are wrong.** A rowdiff sweep of seeds 88000001..88002326
hit the same defect at seed 88001928 (`ROWDIFF_SEED_START=88001928
ROWDIFF_SEEDS=1` reproduces it standalone in ~5s) on a RIGHT JOIN whose
predicate reads the PRESERVED side — an arm the table above records as OK.

Re-measured over the full 24-arm cross (LEFT/RIGHT/INNER x predicate on `l`/`r`
x indexed/unindexed column x duplicate/distinct `IN`), exactly 3 arms fail:

| arm | result |
| --- | --- |
| `LEFT  … WHERE r.c IN (7, 7)` (`c` indexed) | **ERR** |
| `RIGHT … WHERE r.c IN (7, 7)` (`c` indexed) | **ERR** |
| `RIGHT … WHERE r.s IN ('b ','b ')` (`s` NOT indexed) | **ERR** |
| every `INNER` arm | OK |
| every arm whose `IN` list is DISTINCT | OK |
| every arm whose predicate reads `l` | OK |
| `LEFT  … WHERE r.s IN ('b ','b ')` (`s` NOT indexed) | OK |

Corrected reading:

- **Not the NULL-PADDED side.** It is the join clause's RIGHT-HAND relation
  `r`, whichever side that is. Under `RIGHT JOIN` `r` is PRESERVED and still
  fails; under `LEFT JOIN` the preserved `l` is clean. Every `l.*` arm runs, in
  all three join types.
- **Not always an INDEXED column.** `RIGHT … r.s IN ('b ','b ')` fails with no
  index on `s`. The index is load-bearing only for `LEFT JOIN`.
- The two surviving clauses hold: OUTER-ness and the DUPLICATE `IN` value.

Pinned by `TestFDB_QOVBindingMinimalShape` in
`pkg/relational/conformance/factory/qov_binding_shape_test.go`, which drives all
24 arms and asserts the count, so this description cannot drift from the code
again. Note it is an EXECUTION failure, not a planning one — the message comes
from `quantifiedObjectValue.Evaluate` (`values.go:5426`), and planning every arm
through `embedded.PlanQueryForTest` returns no error at all, so a plan-only
probe passes against the live defect.

Neither the `ORDER BY` nor the projection participates. The failing QOV is a
planner-MINTED `q$N` over the whole `T_RD` row, which is the same class as
`pkg/relational/sqldriver/outer_join_nested_field_binding_fdb_test.go` — "the
leg was bound under a DIFFERENT exact QOV than the one the projection holds" —
with the mint coming from the duplicate-`IN` plan rather than from a nested
field read.

## [x] Record-type names print as SQL identifiers across the whole `frl` CLI

`frl stats` decoded every record-type name it printed; nothing else did, so one
table was `MY$TABLE` under one command and `MY__1TABLE` under another and a
script crossing two commands silently missed. Closed by decoding at every render
boundary: `meta_types_describe.go`'s `sortedRecordTypeNames` (which also backs
the `not found -- available: ...` message in `lookupRecordType`, so a typo now
offers names the operator can type) and its `writeRecordTypeDescription` /
`writeRecordTypeDescriptionJSON` `Name` fields, `index.go`'s
`recordTypeNames`, `meta.go`'s `writeTypesList`/`writeTypesListJSON`,
`meta_diff.go`'s `diffRecordTypes`, `record.go`'s `writeRecordAsJSON`
(`record_type`), and the two record-type arms of `completion.go`. `frl sql`'s
`\d` table list, column list, PK line and "available:" message go through the
same helpers, pinned e2e in `sql_identifier_names_integration_test.go`.
COLUMN names are the same namespace and were raw in four more files until a
source gate went in — `TestFRLRenderersDecodeNamesThroughHelpers`
(`pkg/docscheck`) fails the build on a renderer that reaches a stored name
through one of the spellings it knows: `.MetadataName()`, `.FieldNames()`,
`rt.Name`, `.RecordType.Name`. It is a NARROWING, not a closure — a name reached
any other way is still invisible to it, which is exactly how
`rec.RecordType.Name` leaked past the gate's first version into `record scan
-o json`'s `record_type`, the field the guide tells operators to pipe into
`--type`.

**The first census MISSED two of those**, and the way it missed is the lesson:
it enumerated `RecordTypes()` call sites, but `writeRecordTypeDescription` takes
a `*RecordType` ARGUMENT and `writeRecordAsJSON` reads `rec.RecordType.Name`, so
neither appears in that grep. A census keyed on how a value is OBTAINED cannot
find the sites that receive it already obtained. Sweep by what is PRINTED.

Round-tripping holds ONLY under two guards, and neither is optional.
`GetRecordType` resolves either namespace, but its DIRECT-key step answers
first. So (a) a decoded name is shown only when `encode(decode(s)) == s`,
otherwise it would resolve to nothing (`__0Order` -> `__Order` -> `__Order`,
never back), and (b) under a declared collision the STORED names are shown
instead, because the decoded spelling of one type is the stored key of another.
Round-tripping is NOT the same as safe to offer: `MY__01TABLE` round-trips and
its decode is exactly the colliding key. Pinned by
`TestGetRecordTypeResolvesAUserIdentifier`,
`TestGetRecordTypeMisResolvesAnAmbiguousPair`, `TestEscapeIsNotABijection`,
`TestCompletionFallsBackToStoredNamesWhenAmbiguous`, and, for the whole CLI
surface,
`TestRecordTypeNamesRenderAsSQLIdentifiers`
(`cmd/frl/internal/cmd/record_type_names_user_facing_test.go`), whose eight arms
were each shown to redden under reversal of their own conversion.

**Four surfaces are deliberately NOT converted**, and each has a reason that
outlives this entry:
- **INDEX names** — `GetIndex` (`metadata.go:1484`) is a raw map lookup with no
  escape fallback, so a decoded index name would not resolve when passed back.
  Decide index-name policy on its own terms; do not assume it matches.
- **Context names** — local to the CLI, never a record type.
- **The `Proto message:` line of `frl meta types describe`** (`proto_message` in
  JSON) — that field reports the protobuf DESCRIPTOR full name, so the escaped
  spelling is the right answer there. Its `Name:` field one line above IS
  decoded, which makes this the one command that prints both namespaces on
  purpose; the test asserts per-field rather than over the whole blob, since a
  whole-output check would either miss a `Name` regression or forbid the proto
  name that belongs there.
- **SYNTHETIC type names** — Java stores these verbatim from
  `addJoinedRecordType`, and this port never creates one (metadata_proto.go only
  `proto.Clone`s them), so `MY__1JOINED` is genuinely ambiguous between a
  literal name and the escaping of `MY$JOINED`. Decoding would invent a
  declaration the operator cannot find. Pinned by
  `TestSyntheticRefusalErrorNamesTypesVerbatim` and
  `TestStats_SyntheticTypeNamesAreRenderedVerbatim`.

**Trap for anyone extending this**: `ToUserIdentifier` is NOT idempotent, and
the chain is NOT bounded at two — each decode peels ONE escape level, so
`MY__001TABLE` -> `MY__01TABLE` -> `MY__1TABLE` -> `MY$TABLE`. Decode exactly
once, at the boundary, and sort AFTER decoding (storage `[A__0B, A__1B]` prints
as `[A__B, A$B]`). Both pinned in `protoname_test.go` and
`TestStats_ListsAreSortedByTheDecodedName`.

---

## [x] Go returned a raw `broken_promise` (1100) where C++ absorbs it — FIXED

A dying proxy answers the RPC with an error INSIDE the `ErrorOr<>` reply, so it
reached the reply parser and missed every transport mapping (those all mint
1030). 1100 is retryable under none of Go's three predicates, so a GRV or commit
issued while a proxy was dying failed TERMINALLY where C++ retries or converts.

Fixed on this branch. `pkg/fdbgo/client/inband_reply_error.go` carries the
`basicLoadBalance` rule once (`LoadBalance.actor.h:812-830`): 1100 and 1030 are
one class, next-alternative at `AtMostOnce::False` (GRV,
`NativeAPI.actor.cpp:3865`), convert to maybe-delivered at `AtMostOnce::True`
(commit, `:6638-6643`) because a commit must never be re-sent. Twelve cases
pinned plus a real `ErrorOr<CommitID>` body through `parseCommitReply`.

Also corrected the comments that would have misled the next reader:
`transport/conn.go` attributed commit's arm to `loadBalance`, which the commit
path never calls; `readpath.go` claimed the commit path "never consults" the
predicate; `grv.go` called broken_promise a transport error only.

## The CNF normalizer wired into the planner treats NOT as a leaf; Java's does not

- [x] Unify Go's two normal-form implementations on the exact Java port, so
  `NormalizePredicatesRule` normalizes through a `NOT` over a connective.
  DONE — RFC-240, five commits: the RFC plus a write-path golden captured before
  the rewrite, the absorption tie-break, the cost sites moved to the negate-aware
  metric, the strict normal form itself, and the deletion of the rule's state
  map. `normalizeCNF(AND(NOT(AND(a,b)), c))` now returns
  `(NOT a OR NOT b) AND c`. Verified against Java's own
  `BooleanPredicateNormalizerTest` cases, ported.

  Two things found while doing it that the entry below did not anticipate.
  First, the cost model was reading the SAME quantity through the negate-blind
  walk: Java's `NormalizedResidualPredicateProperty.countNormalizedConjuncts` is
  `getMetrics(p).getNormalFormFullSize()`, so `designated_final.go` and
  `planning_cost_model.go` were a mis-port, not an independent proxy — the
  RFC's first draft proposed freezing that number under a new name and both
  review gates rejected it. Second, the rule carried an unbounded identity-keyed
  set standing in for a termination property Java gets from the algebra.

Java's `BooleanPredicateNormalizer` has ONE `isInNormalForm` — a `NOT` over
anything that is not a variable is NOT in normal form
(`BooleanPredicateNormalizer.java:284-289`) — and ONE
`toNormalized(predicate, negate)` carrying a negate flag down through AND/OR,
which is De Morgan. Both `normalize` and `normalizeAndSimplify`, and both CNF
and DNF modes, route through them.

Go split that into two implementations that disagree:

| file | normal-form test / conversion | NOT over a connective |
|---|---|---|
| `normalize_dnf_exact.go` | `isInDNFStrict` + `toDNFNegated` | pushed (matches Java) |
| `rule_normalize_predicates.go` | `isInCNF` / `isInDNF` + `toCNFNormalized` / `toDNFNormalized` | treated as a LEAF |

`isLeafPredicate` (`rule_normalize_predicates.go:168-175`) returns true for a
`NotPredicate`, so `AND(NOT(AND(a, b)), c)` reads as already-CNF and
`normalizeCNF` declines. `NormalizePredicatesRule` — registered in the default
pipeline at `default_rules.go:148` and `:178`, and a port of the Java rule that
calls `normalizeAndSimplify` in CNF mode — therefore leaves the `NOT` standing
where Java produces `(NOT a OR NOT b) AND c`.

REACHABLE, not latent. The SQL resolver builds a plain `NotPredicate`
(`expr.ResolveNot`, `pkg/relational/core/query/expr/expr.go:1874-1879`) and
applies no De Morgan of its own, so the `NOT` arrives at the planner intact.
`WHERE NOT (cat = 'A' OR cat = 'B')` is already in the corpus
(`pkg/relational/conformance/yamsql/testdata/complex_where_java.yaml:62`).

The consequence is plan quality, not wrong rows: the CNF shape is what
`PredicateToLogicalUnionRule` needs to split a disjunction across index
accesses, so a negated conjunction stays one opaque residual over a scan in Go
and becomes independently matchable conjuncts in Java.

MEASURED AND PINNED, so this entry cannot rot into an unverifiable claim:
`TestNormalForm_NotOverConnective_TwoImplementationsDisagree`
(`pkg/recordlayer/query/plan/cascades/normal_form_not_handling_test.go`) asserts
the current disagreement on one input and asserts that the exact port pushes the
same `NOT` through. It FAILS when this item is done, holding the whole
measurement, and its doc comment points back here.

Nothing has to be invented: the correct algorithm already exists in-tree as the
DNF exact port. The work is generalizing it over Java's `Mode` (major/minor =
AND/OR or OR/AND) and retiring the lax pair, which is the "no parallel
pipelines" rule applied to one file.

GATED, and that is why this is filed rather than fixed: closing it changes the
planner's canonical predicate shape for every query containing a `NOT` over a
connective — plans, goldens, the explaindiff corpus — which is a query-engine
change and needs an RFC plus a Graefe ACK before merge.

DONE when: one normal-form test and one negate-carrying conversion serve CNF and
DNF and both entry points; `normalizeCNF` returns `(NOT a OR NOT b) AND c` for
the input above; the pin test is replaced by that assertion; and the plan-shape
diff has had its review lap.

---

## Doc comments naming a different function than the one they document

- [x] Fix the 23 remaining SUBJECT-class mismatches. DONE — the detector reports
  ZERO across the tree, and that zero is mutation-verified: introducing one
  mismatch makes it report exactly that one. All 40 are fixed, each by reading
  the function rather than by pattern.
- [x] The detector is now a `pkg/docscheck` gate. DONE —
  `TestDocCommentNamesItsOwnFunction` in
  `pkg/docscheck/doc_comment_names_its_function_test.go`, reading 6260 documented
  of 13284 scanned test functions with 0 citations wrong. It got its own file
  rather than joining the citation-gate file: those gates share an extraction
  pipeline over Markdown text, and this one shares nothing with them but the
  inventory helper.

  The shell walk did not survive the port, and shouldn't have. It compared the
  first cited name in a contiguous comment BLOCK; the gate uses `go/ast`, so
  godoc's own attachment rule decides what documents what — which is the rule the
  MISPLACED-BLOCK class was violating in the first place.

  Three things the port changed, each because a test caught it rather than
  because it was designed in:

  - The first cut matched `^(Test|Fuzz|Benchmark)[A-Za-z0-9_]*`, which reads the
    English words "Test that…" and "Benchmark for…" as citations. Seven false
    positives. The `[A-Z_]` continuation is what separates a name from a word.
  - It read `Doc.List[0].Text` and trimmed `//` by hand, which is silently blind
    to a `/* */` doc block. Zero such docs exist today across the 2239 test files
    that declare a test function — so the blindness would never have surfaced.
    `Doc.Text()` handles both spellings.
  - Two hand-predicted expectations in the fixture test were both off by one
    (`scanned` 6 vs 5, `documented` 5 vs 4), and a documented "known false
    positive" about subtest names was simply false: the name truncates at the
    slash and resolves to the parent test, which on its own function is a match.

  Four arms, four mutations, each proven to have landed before its verdict was
  read: misattributed fires (line 220 arm), phantom fires (line 226 arm),
  killing comment attachment trips the DOCUMENTED floor — without which that
  mutation reports "0 citations wrong" and passes green — and emptying the scan
  set trips the SCANNED floor. A fifth mutation, reverting the marker stripping,
  reddens the block-comment arm of `TestScanFileForDocNames_WalkExclusions`.

  The gate's scope sentence lists what it does NOT check FIRST, from shapes
  driven against the code: a comment that never names its function, a function
  with no comment, a name appearing anywhere but first, a method or non-test
  declaration, and every line after the first.

`pkg/docscheck`'s citation gates — `TestEveryAuthorityDocTestCitationResolves`,
`TestTodoTestCitationDriftIsReported` — scan MARKDOWN authority docs and
test-filter flags. They do not scan GO SOURCE COMMENTS, which assert what the
tree contains far more often than the Markdown does.

A detector for the unambiguous class (godoc convention: `// TestFoo does X`
directly above `func TestFoo`, so a leading name that differs is always a
defect) found **40**. The script is
`scratchpad/mismatched_doc.sh` in this shift's scratchpad; it walks up the
contiguous comment block above every `func Test|Fuzz|Benchmark` and compares the
block's first cited name against the function's.

FIXED so far (17): the 16 PREFIX cases, where the cited name is a strict prefix
of the function's — a disambiguating suffix was added
(`TestEmptyKeyValue` -> `TestEmptyKeyValue_Limits`) and the body still describes
the test, so a name-only rewrite is correct. Plus one misplaced block, below.

REMAINING (23), which need READING, and must not be fixed mechanically. A
name-only rewrite there gives a wrong body a matching name and turns a
grep-detectable defect into an invisible one — measured: rewriting all 40
produced exactly that on `winner_lookup_test.go:353`, where the body describes
`ImplementFilterRule` and the function is
`TestGetWinnerForOrdering_PreserveOnRefWithMultiplePhysical`. Three sub-classes:

1. SYNONYM RENAME — body still fits, name-only fix is safe after confirming it.
   `TestFDB_EmptyTableOperations` -> `TestFDB_EmptyTableOps`,
   `TestFDB_InsertThenUpdateThenVerify` -> `TestFDB_CRUDCycle`,
   `TestFDB_SumWithWhereAndGroupBy` -> `TestFDB_SumFilteredGrouped`,
   `TestFDB_JoinCountGroupByWithHaving` -> `TestFDB_JoinCountGroupByHaving`,
   `TestFDB_SelectWithAlias` -> `TestFDB_SelectWithColumnAlias`,
   `TestFDB_JoinSumGroupByWithOrderBySum` -> `TestFDB_JoinSumGroupOrderSum`,
   `TestFDB_OrderByMultipleWithLimit` -> `TestFDB_OrderByThreeColumnsLimit`.

2. OPPOSITE OR DIFFERENT CLAIM — the prose asserts something the body does not,
   which is the severe class. Same shape as the two already fixed
   (`rule_ordered_index_scan_test.go`, where the comment said a DESC sort is NOT
   satisfied above a function asserting `IsReverse()`).
   `TestRebaseLegRefsToBox_DeclinesANestedDescent` on
   `...FusesAnExactNestedDescent`;
   `TestComputePrimaryKey_IndexScanIsNilPendingStructuralPK` on
   `...IndexScanStructuralPK`;
   `TestOrderingComparatorsAreTransitiveAcrossTheUnknownDomain` on
   `...AcrossExactLayouts`;
   `TestDefaultFolder_PartialFoldComposesViaSimplify` on
   `...PartialFoldDoesNotReturnOk`;
   `TestShuffleIsCollectionsShuffle` on `TestShuffleIsDeterministic`;
   `TestSatisfiesRequestedOrdering_AdmitsQualifiedRequestAgainstLocalCandidate`
   on `...AdmitsRequestAgainstSameExactRoot`.

3. MISPLACED BLOCK — a doc block separated from its function by a later
   insertion, so godoc attaches it to the wrong one. `values_java_inspired_test.go`
   was this and is FIXED: `TestArithmeticValue_OverflowPanics`'s block had been
   stranded above `TestArithmeticValue_DivMinWrapsLikeJava` while
   `OverflowPanics` itself carried none. Others in the 23 may be this rather
   than a rename; the tell is that the cited name resolves elsewhere in the same
   file.

DONE when: the detector reports zero, every fix was made by reading the function
rather than by pattern, and either the detector is a docscheck gate at a zero
floor or there is a recorded reason it cannot be.

---

## IntersectCompensations was order-dependent, and a Go-only field was why — FIXED

Was STOP-level: the fix turned on how a Go-only obligation should interact with
a ported identity element, which is a design call rather than a mechanical port.
The decision and its measurements are recorded under FIXED below. The analysis
above is kept as written, because the first hypothesis it records was incomplete
and the way it was incomplete is the useful part.

**The defect.** `IntersectCompensations` is a LEFT fold: `result =
ImpossibleCompensation; for each leg { result = intersectTwo(result, leg) }`.
Three legs therefore fold as `((I·a)·b)·c`, and reordering them folds
differently unless `intersectTwo` is associative. It is commutative and NOT
associative, so the result depends on the order the planner enumerated the
intersection legs.

The same three legs give three incompatible answers:

| leg order | result |
|---|---|
| `[NoCompensation, plain, pkDistinct]` | needed, possible, not-for-filtering |
| `[NoCompensation, pkDistinct, plain]` | needed, **IMPOSSIBLE**, for-filtering |
| `[plain, pkDistinct, NoCompensation]` | **NOT NEEDED** |

"impossible" discards a usable intersection. "not needed" drops the
primary-key-distinct obligation, which is a cardinality correction — losing it
returns DUPLICATE ROWS. `intersectTwo`'s own comment states the invariant being
violated: a leg that needs it "cannot lose it merely because the other leg has
no filter or result residual".

**Measured scope** (at the five-shape corpus this was taken over; the corpus has
since grown to six and the figure moves with it). There were 96 disagreeing
permutation pairs, and EVERY ONE involves `requiresPrimaryKeyDistinct`. Zero
triples disagree without it. That field is a **Go-only extension** —
`Compensation.java` has no equivalent — so Java's fold is unaffected and this is
not a mis-port. It is an extension interacting badly with a ported identity.

**Mechanism.** `ForMatchCompensation.Intersect` ORs the flag
(compensation.go:1087), then returns the bare `ImpossibleCompensation` singleton
when the intersected child is impossible (compensation.go:1105), discarding it.
That would be harmless if impossible propagated — but `intersectTwo` treats
Impossible as the intersection IDENTITY (`impossible ∩ X = X`, matching Java's
`reduce(impossibleCompensation, Compensation::intersect)`), so the impossible
result is ABSORBED rather than poisoning the fold, and the obligation is simply
gone. The singleton is fieldless, so it cannot carry the obligation across the
identity arm.

**Reachability.** `requiresPrimaryKeyDistinct` is set in production by
`PartialMatchImpl.GetCompensation` (partial_match.go:379), and
`intersector_primary_key.go:657` and `:1149` fold one compensation per
intersection leg. A 3+-leg intersection where one leg carries the obligation and
another needs no compensation reaches this.

**FIXED.** The root cause was not the discarded flag on the impossible path —
that was one loss of two, and fixing it alone left the reproducer green. The
fold's absorbing arm tested `IsNeeded`, and a primary-key-distinct obligation
makes a compensation "needed", so `NoCompensation ∩ pkDistinct` produced a
PK-distinct-only compensation that was NO LONGER ABSORBING. "Some leg filters
exactly" is a property of the whole intersection, but a LEFT fold only keeps it
if the accumulator does; after one step it was gone, the next leg went through
the full `Intersect`, and its residual came back — while the same legs in
another order kept absorbing.

Three changes, each measured:

- `intersectTwo` absorbs on `!IsNeededForFiltering() && !IsFinalNeeded()`, the
  pair Java's `WithSelectCompensation.intersect` uses at its own absorbing point
  (`Compensation.java:771-774`), rather than on `IsNeeded`. A PK-distinct-only
  compensation reports neither, so it stays absorbing and the property survives
  the fold. `IsNeededForFiltering` ALONE is not the test and an earlier draft of
  this line said it was: it excludes the RESULT compensation too, so on its own
  it swallows a leg whose predicates are fully matched but whose result value
  must be re-projected — wrong columns, not a lost optimization. Reverting the
  predicate reddens the laws. (Violation counts are deliberately not quoted here:
  they are a function of the corpus size, which the test logs. A "151" recorded
  against the five-shape corpus was both miscounted — it omitted the
  commutativity subtest — and then invalidated by a sixth shape being added,
  which moved the same mutation to 233. Run the test.)
- `ForMatchCompensation.IsNeeded` recurses with the child's PRE-FINAL need
  instead of the child's full `IsNeeded`. Java counts the child's RESULT
  compensation (`Compensation.java:528-533`) and nothing can ever apply one:
  `ForMatch.applyFinal` reads its own function and does not recurse, in both
  engines. So Java reports "needed" for a compensation that applies nothing, and
  that shape made both folds order-dependent — it collapses to `NoCompensation`
  in one grouping and survives as needed in another, while
  `intersector_primary_key.go` and `abstract_data_access_rule.go` both branch on
  `IsNeeded`. A seventh corpus shape exposes it: 30 law violations against the
  Java-literal spelling. The narrowing drops the child's result term and nothing
  else — a nested primary-key-distinct obligation still counts, and still
  applies. This is a DELIBERATE divergence from Java, argued at the call site,
  and the reachability fact it rests on is pinned by
  `TestForMatchCompensation_AChildsResultCompensationIsUnreachable`.
- The absorbing arm reduces BOTH operands and keeps the union of their
  obligations, via `unionPrimaryKeyDistinctObligations`. Discarding either
  side's obligation reddens the laws. The helper also unions the two sides'
  MATCHED QUANTIFIERS, as Java does (`Compensation.java:781-782`), and unions
  the COMPENSATED ALIASES, which Java does NOT — Java takes one side under an
  invariant it declines to check (`Compensation.java:801`, "both compensated
  aliases must be identical, but too expensive to check"), so the union is a
  deliberate widening of an unverified assertion. Neither half is covered by the
  laws, whose shape comparison is five booleans and can observe neither a
  quantifier set nor an alias set, and both are stated rather than presented as
  measured. The arm's CHILD slot recurses, so nested obligations survive the
  fold; rebuilding with `NoCompensation` there keeps the top obligation and
  drops every nested one, which returns duplicate rows.

`ForMatchCompensation.Intersect` also no longer returns the bare
`ImpossibleCompensation` singleton when it holds an obligation, since the
singleton is fieldless and the identity arm would absorb the obligation with it.

Both folds now satisfy every law in `compensation_monoid_test.go`: commutative,
associative, permutation-independent over three legs, each with its documented
identity, and impossible propagating through union only. The reproducer that
pinned the broken answers is deleted, as its own failure message instructed, and
the UnionCompensations-only guard on the associativity and permutation subtests
is removed so both folds are held to them.
