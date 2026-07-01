# RFC-174: Code-health remediation — correctness stragglers, process-noise purge, dead-architecture removal, god-file decomposition

**Status:** DRAFT — needs the standard gauntlet. Items touching the Cascades engine (§3 Track C)
need Graefe ACK; items touching `pkg/fdbgo` (§3 Track B) need the FDB-C-dev ACK; Torvalds +
codex + @claude gate everything. Two items already landed as owner-authorized open PRs:
A1 = **PR #436**, A2 = **PR #438** (§2).
**Origin:** 2026-07-02 whole-project quality assessment (three parallel subsystem reviews:
`pkg/fdbgo` A−, `pkg/relational` B+, Cascades planner C+; plus a live red-nightly-fuzz
investigation). The assessment's conclusion: the debt is overwhelmingly *presentational and
structural*, not correctness debt — but it is systemic, it violates this repo's own CLAUDE.md
rules, and two correctness items fell out of it. This RFC is the single registry of everything
found, so items get executed or explicitly rejected instead of evaporating.
**Cross-refs:** RFC-173 (the active freeze; sequencing in §4), RFC-150/151 (the planner
comment-archaeology this RFC relocates), RFC-105 (retry-predicate single-source pattern cited as
the house standard §2 F2 should follow), DIVERGENCES.md (already promises §2 C4's end-state).
**Effort (honest):** the sweeps (Track B/D hygiene) are ~1 shift each, mechanical. The
structural items (Track C) are 1–3 shifts each; C4 is a planner-semantics change that needs its
own focused RFC and is only *registered* here. Total ≈ 8–12 shifts spread across tracks that
mostly parallelize.

---

## 1. Problem

A full-project assessment (build green, master CI green, 1,604 commits / 4 reverts in 60 days)
found the correctness *machinery* — differential tests against libfdb_c, cross-engine SQL
conformance, 105 fuzz targets, typed errors, the `docs_consistency_test.go` CI guard — in
unusually good shape. What it also found:

1. **One live wire-facing panic** that nightly fuzz had been flagging red for two runs
   (06-29, 06-30) while the RFC-173 freeze forbade anyone from touching it.
2. **A dead parallel planner architecture** shipped in production source, violating the repo's
   own "no parallel pipelines" rule.
3. **Systemic hygiene violations of CLAUDE.md's own bans** — shift-tags in code comments,
   reviewer attributions, changelog-archaeology comments — at a scale (dozens of files) that
   individual review no longer catches.
4. **Structural debt**: god-files, god-functions, a text/typed dual data model that forces
   string re-lexing, and brittle stringly-typed plan assertions.

None of §3's hygiene/structure items changes wire behavior. The two correctness items are
already in flight. Everything else is inventoried below with verified citations, grouped into
tracks by review gauntlet, and sequenced against the RFC-173 freeze in §4.

## 2. Findings inventory

Line refs verified against master @ 2026-07-02 (`6100f34c6`). Counts are greppable — each
item's acceptance criterion in §5 is "the grep returns zero."

### Track A — correctness (owner-authorized, PRs in flight)

- **A1 — `orElseCursor` panics on unknown continuation state.**
  `pkg/recordlayer/cursor_combinators.go`: `OrElseWithContinuation` (~:162-193) parses an
  `OrElseContinuation` proto; an out-of-range `State` enum value (proto3 open enums — any wire
  byte can produce one) falls into the `default` switch arm, which sets `c.primary` but leaves
  `c.active` nil and keeps the unknown state. `OnNext` (:203) then sees `state != UNDECIDED`,
  skips the primary-probe branch, and nil-derefs in `advanceActive` (:228). Reproduced locally
  in 0.15 s; this is the red Nightly Fuzz of 06-29/06-30. Continuation tokens are external wire
  input — a panic here violates design principle #4 (explicit errors, never panic).
  **Second defect, same constructor:** `UnmarshalVT` failure is silently swallowed (the `else`
  branch restarts from `primaryFactory(nil)`), so a *corrupt* continuation silently restarts
  the cursor from scratch — a wrong-results divergence, strictly worse than the panic. Java
  throws on invalid continuations; Go must match. **Third defect found during the port:** an
  absent state field must default to UNDECIDED *and keep the inner continuation*
  (`OrElseCursor.java:76-83`); Go dropped it. All three fixed + deterministic regressions +
  committed fuzz corpus entry: **PR #436** (`fix/orelse-continuation-panic`).
- **A2 — delete the legacy planner architecture.**
  `planner.go:398 Explore()`, the legacy task types (`ExploreReferenceTask` /
  `TransformReferenceTask` / `SaturationCheckTask` / `OptimizeReferenceTask`, ~:988-1150),
  `exploreCount`, and `fixpoint.go:38 FixpointApply` are called by nothing in production —
  only tests. Two shipped drivers for one planner is the exact "parallel pipelines" failure
  mode CLAUDE.md forbids. Behavior-pinning tests get ported to the unified `Plan()` path;
  machinery-only tests die with the machinery. **PR #438** (`refactor/remove-legacy-planner`,
  net −987 LOC): triage table in the PR body; FixpointApply mentions swept per B5's A2 slice.
  **Watch item resolved:** one legacy-pinned property does NOT hold on the unified path —
  `FuzzCostMonotonicity`'s best-cost monotonicity. **Framing (per Graefe review):** cost
  monotonicity IS a Cascades invariant, group merging included — when child costs come from
  group winners, a merge takes the min of the merged winners and root best-cost is
  non-increasing. What breaks it in Go is not merging but `EstimateCost`'s **first-member
  approximation** (cost.go:24-31, :263-268): child References are costed at their first
  member, making cost a function of memo state rather than of the expression. Under that
  approximation the pin genuinely cannot hold; Java offers no analogous scalar invariant
  (`CascadesCostModel` is a comparator, no numeric cost). Reframed as `FuzzCostSanity`
  (finite/non-negative) with the triggering input as a seed. **Registered follow-up:** when
  child costing moves to winner-based (`BestMemberCostWith` already exists at cost.go:389; the
  package doc promises it), RESTORE the monotonicity pin — it is a free oracle — and retire
  `FuzzCostSanity`'s weaker half. Permanent docs must say "not an invariant of the
  first-member approximation", never "not a Cascades invariant under merging".

### Track B — process noise in permanent source (violates CLAUDE.md's explicit bans)

- **B1 — reviewer attributions in library code.** ~53 comment lines mentioning `codex` and 6
  mentioning `Torvalds` in `pkg/fdbgo` non-test source alone (e.g. `client/transaction.go:2202`
  "(codex: …)", `client/commitpath.go:60` "(audit #15)", `client/ryw.go:52` "(deliberate,
  reviewed)"), plus scattered `unified_tasks.go:356` "NOTE (Torvalds F2)"-style tags in the
  planner. Who flagged a line is git-blame's job. **Rule for the sweep: keep the reasoning,
  delete the attribution.** A comment that is *only* attribution dies; a comment whose
  reasoning is load-bearing keeps the reasoning sentence.
- **B2 — shift-tags in code comments.** Explicitly banned by CLAUDE.md ("Never put shift tags
  in code comments"), yet present in **29 files** under `pkg/`, worst offender
  `pkg/relational/conformance/plandiff/corpus.go` (24 tags). Named examples:
  `core/query/expr/expr.go:30` "(swingshift-47 seed)", `core/query/semantic/identifier.go:15`,
  `core/embedded/select_parser.go:808` and :1074 ("Pre-dayshift-40 Go emitted 0A000" — a
  git-history fact encoded as a comment). Sweep to zero; where the tag anchors a real WHY,
  rewrite the WHY without the tag.
- **B3 — changelog-archaeology comments in the planner.** `planner.go` carries essay-comments
  documenting the *history of fixes* rather than why the current code is correct:
  :504-535 is a ~30-line comment describing a band-aid ("muzzle") that **no longer exists**;
  "the rot" (:582), "ROT-FIX (RFC-150, post-B1a)" (:775), "codex P2" (:356). The history lives
  in RFC-150/151 and PR descriptions already. Relocate any still-load-bearing rationale into
  those RFCs; the in-source comment shrinks to the invariant + an RFC pointer.
- **B4 — essay-comments in fdbgo that belong in the RFCs they already cite.** `transaction.go`
  `readErr` field comment ≈40 lines (:332-371), `ryw.go` `unreadableRanges` ≈22 lines (:36-57).
  The content is correct and load-bearing — but at this density the WHY drowns the WHAT.
  Library code carries **250** `RFC-` references; the rationale has a home. Same rule as B3:
  field comment = contract + invariant + RFC pointer; the actor-semantics essay moves to the RFC.
- **B5 — stale header comments that lie.** `properties/ordering.go:17-20` claims "the seed
  makes no use of OrderingProperty — Cost ignores ordering… Sort/Distinct rules currently fire
  unconditionally" — false; the planner is saturated with ordering logic (`stampOrderingWinners`,
  `RequestedOrdering`, per-ordering winners). `properties/cost.go:25` justifies a design choice
  by "FixpointApply fires every rule on every Reference" — retired by A2. `cost.go:49` says
  "31-rule seed" while the ruleset is 42 (`default_rules_test.go:36` pins `expected = 42`).
  Fix or delete every comment that describes a retired driver or a wrong count; A2's PR sweeps
  the FixpointApply mentions, this item covers the rest (including the recurring "seed"
  hedging in shipped-and-tested code).

### Track C — structure (planner + relational; Graefe gauntlet)

- **C1 — `logical_predicate.go` is a 6,038-line god-file whose name lies.** It contains the
  entire DML builder surface — `buildLogicalPlanForInsertWithCatalog` (:3338), `…Update…`
  (:3231), `…Delete…` (:3043) — plus UNION construction (:4093) and aggregate/HAVING
  rewriting. "Predicate" is ~a fifth of the file. The functions are individually small; this is
  tangling by aggregation, so the split (≈ `dml.go`, `aggregate.go`, `union.go`,
  `predicate.go`) is mechanical. **Sequencing constraint: after RFC-173** — 173's slices churn
  exactly this surface, and a file split under an active migration is rebase poison (§4).
- **C2 — `Transaction` is a 61-field / 94-method god-object.** `client/transaction.go:177-450`
  fuses three C++ concepts (Transaction + ReadYourWritesTransaction + TransactionOptions). The
  API breadth is inherent to mirroring `fdb_transaction_*`; the struct breadth is not. Extract
  an embedded `txOptions` type for the ~42 trivial option accessors (`SetPriority`,
  `SetLockAware`, `SetCausalReadRisky`, `SetUseGrvCache`, …). Orthogonal to RFC-173 entirely.
- **C3 — planner god-functions.** `pushDataAccessTasks` (planner.go:459-658, ~200 lines) and
  `compensationSafeForYield` (:702-830, ~130 lines). B3's comment relocation alone shrinks
  both substantially; after it lands, re-measure and decompose what remains along the phase
  boundaries the comments already delineate. Do B3 first — splitting before de-noising moves
  the archaeology around instead of deleting it.
- **C4 — index-type special-cases bolted into the generic driver (REGISTERED, needs own RFC).**
  Vector-KNN / aggregate-index handling leaks into the generic winner/cost path:
  `isNilInnerFetch` guards (planner.go:202-208, :1164), type-switches on
  `*physicalVectorIndexScanWrapper` / `*physicalAggregateIndexWrapper` inside
  `compensationSafeForYield` (:732-765), `residualIsPartitionContiguous` (:854-932), and two
  catch-all backstops in `Plan()` (:355-372) — with the code's own admission at :698: "Do NOT
  remove that net until every such builder is gated/retired (TODO follow-up)."
  DIVERGENCES.md already states the end-state: gate `ImplementSimpleSelectRule` + the NLJ
  residual builder on `!isIndexOnly()`, retire `ImplementIndexScanRule`, then retire the
  `validateNoIndexOnlyResidual` net. That is planner semantics, not hygiene — it gets its own
  focused RFC + Graefe cycle when picked up. This RFC only pins it so it stops living as a
  code comment.
- **C5 — `LogicalAggregate`'s text/typed dual data model.**
  `core/query/logical/operators.go:321-324`: `Aggregates []string` ("SUM(a)") alongside
  `AggregateOperands []values.Value` with **"nil slot = use text"**, and `Having string`
  alongside `HavingPredicate`. The text fallback forces downstream re-lexing:
  `aggregateArgText` (cascades_translator.go:554) re-finds operands by scanning for parens;
  `isBareColumnIdentifier` (:567) re-implements an identifier lexer char-by-char. Parsing a
  string you already had as an AST, institutionalized by the node itself. End-state: operands
  and HAVING are always typed Values; the text fields become display-only (or die); both
  re-lexers are deleted. **Sequencing: after RFC-173** — 173's typed/positional rows change
  what "typed operand" means here; doing C5 first builds on the model 173 retires.

### Track D — test debt

- **D1 — plan-shape assertions are substring matches with loose OR-pins.**
  `sqldriver/plan_shape_conformance_test.go` (19.5k lines) asserts via
  `strings.Contains(plan, "IndexScan")` etc. Substring matching makes negative assertions
  fragile ("Scan" matches inside "IndexScan"), and disjunctive pins (:351, :540 accept
  `NestedLoopJoin` OR `FlatMap`) let a join-strategy regression pass silently. Move the suite
  to structured assertions over the plan operator tree (the explain path already walks typed
  plan nodes; assert on node types/shape, not the rendered string). Disjunctive pins become
  exact pins per scenario — where the planner is legitimately nondeterministic between two
  shapes, that nondeterminism is itself a bug per RFC-167 and gets fixed, not tolerated.
- **D2 — tautological rule-count pin.** `default_rules_test.go:36` pins `const expected = 42`
  with 14 lines of prose. len == 42 because we chose 42 protects nothing (and `cost.go:49`
  already rotted to "31"). Replace with an assertion that actually carries information — e.g.
  every rule in the set is registered exactly once and every exported rule type appears — or
  delete.

### Track E — fdbgo concurrency-contract nits (FDB-C-dev gauntlet)

- **E1 — `GetReadVersion` reads `tx.readVersion` lock-free.** transaction.go:2335-2340: bare
  `return tx.readVersion, nil` where every other site guards with `readVersionMu` (:660-701,
  readpath.go:267-269). Benign on 64-bit today; it is exactly the inconsistency the rest of the
  file goes out of its way to avoid, and the race detector will eventually flag it. Take the
  mutex.
- **E2 — two deferred-error fields, two concurrency contracts.** `rywPoisonErr` is read
  lock-free with the rationale "FDB transactions are not for concurrent use" (:654, :1618)
  while the *neighboring* `invalidAtomicOpErr` is a full `atomic.Pointer` *because* it can race
  `Commit` (:328-330). Pick one story for deferred-error fields and document it once; RFC-105's
  "single predicate so the two can never drift" is the house pattern to imitate.
- **E3 — `GetPipelined`'s bespoke fast path is a second RYW implementation (DEFERRED, pinned).**
  `transaction.go:748` `ErrNeedFullRYW` falls back to full RYW when a key has pending atomics —
  two code paths that must agree on merge semantics, and :373-378 documents a bug this already
  caused. Full unification is out of scope (it is the performance story); the remediation is
  differential coverage: ensure the libfdb_c differential exercises the
  pipelined-vs-full-RYW boundary cases (pending atomic on read key, range straddling a
  pipelined write) so drift is caught mechanically. Registered here so it stops being tribal
  knowledge.

### Track F — repo hygiene + enforcement

- **F1 — stale scratch files at repo root.** `cursor_sequence_example.md`,
  `example_usage.md` (both last touched 2025-08-04), `fdb_inspection.md` (2025-07-30). Delete
  or move under `docs/archive/`. `RFC_TRANSACTION_PAGINATION.md` at root predates `rfcs/` —
  renumber into `rfcs/` or archive. `CASCADES_DIVERGENCE.md` vs `DIVERGENCES.md`: one
  authoritative home (DIVERGENCES.md), the other becomes a pointer or is folded in.
- **F2 — turn the comment bans into a CI gate.** The B1/B2 violations reached 29 files because
  the ban lives in CLAUDE.md prose, not in CI — the exact failure mode design principle #10
  warns about (match the property, not the observable). Extend `docs_consistency_test.go` (or
  add a sibling `source_hygiene_test.go`) to fail on `(day|night|swing)shift-[0-9]+` and
  reviewer-attribution patterns (`(codex[:)]`, `(Torvalds`, `audit #N`) in non-test source
  comments. Lands in the same PR as the B1/B2 sweeps (gate + zero-state together, so it's
  born green and stays green).
- **F3 — codify the red-nightly freeze exception.** A1 sat red for three days because the
  RFC-173 freeze had no keep-the-lights-on carve-out, while CLAUDE.md simultaneously mandates
  "red CI is red — full stop." Resolve the contradiction in CLAUDE.md: a red nightly
  (fuzz/differential/conformance) is ALWAYS in scope regardless of any freeze — root-cause
  immediately, fix if small, or surface to the owner same-day if the fix is large. A freeze
  gates *new* work, never triage of a red safety net.

## 3. Non-goals

- No wire-format or record-layout changes anywhere in this RFC. (A1 *restores* Java's
  invalid-continuation error contract; that is closing a divergence, not changing format.)
- No relitigation of RFC-173 — this RFC sequences around it (§4) and two items (C1, C5)
  deliberately queue behind it.
- C4 and E3 are registered-not-executed: C4 needs its own Graefe RFC; E3's full unification is
  explicitly deferred in favor of differential coverage.
- No test deletions except where the tested machinery itself is deleted (A2) or the assertion
  is information-free (D2) — behavior-pinning coverage is ported, never dropped.

## 4. Sequencing vs the RFC-173 freeze

RFC-173 owns `pkg/relational/core/embedded`, the executor row model, and `FieldValue` — for
~25-30 shifts. Interaction per track:

| Track | Conflict with 173's surface | When |
|---|---|---|
| A (correctness) | none (cursor combinators; dead planner code) | **now — in flight, owner-authorized** |
| B1/B4, E1/E2 (fdbgo) | zero — different subsystem, different gauntlet | anytime; owner call whether during freeze |
| B2 (shift-tags), B5, D2, F1, F2 | comment/test/docs-only, trivial rebases | anytime; bundle B2+F2 in one PR |
| B3, C3 (planner comments → decompose) | low code-conflict, but planner churns under 173 slices | B3 early (comment-only rebases are cheap); C3 after B3 |
| C1, C5 (embedded/ + aggregate model) | **direct** — 173 churns these exact files | **after RFC-173** |
| C4 (index-only gating) | planner semantics; needs own RFC + Graefe | after 173, own cycle |
| D1 (structured plan assertions) | plan shapes converge to Java's *after* 173 | after 173 (asserting pre-173 shapes structurally = re-pinning twice) |
| F3 (CLAUDE.md carve-out) | process text only | **now** |

**Owner decision (2026-07-02, resolved):** the owner authorized executing this RFC ("work on
it") — Tracks B/E/F proceed during the RFC-173 freeze as keep-the-lights-on work; C1/C5/D1
remain queued behind 173 per the table above. F3 and F2 go first: they protect the freeze
itself (F3 closes the process gap that let A1 sit red; F2 stops the B-track violations from
regrowing while attention is on 173).

## 5. Acceptance criteria

- **A1:** fuzz target green for 90 s locally and in the next nightly; deterministic regression
  + committed corpus entry; corrupt-continuation now surfaces a typed error matching Java's.
- **A2:** `grep -rn "FixpointApply\|func (p \*Planner) Explore(" pkg/` → zero in non-test code;
  no behavioral test coverage lost (triage table in the PR).
- **B1/B2:** the F2 CI gate exists and passes — `(day|night|swing)shift-[0-9]+` and
  reviewer-attribution patterns return zero hits in non-test source comments.
- **B3/B4/B5:** no comment in `cascades/` or `pkg/fdbgo` describes retired code as current;
  `cost.go` rule-count references derived or deleted; relocated rationale landed in the RFCs
  cited (RFC-150/151 for B3; per-field RFC pointers for B4).
- **C1:** `logical_predicate.go` < 1,500 lines and contains only predicate construction; DML /
  aggregate / union builders in their own files; zero semantic diffs (pure move, verified by
  the full relational suite).
- **C2:** zero option-backing fields remain directly on `Transaction` — all live on the
  embedded `txOptions`, with the accessors as one-line delegates; the remaining direct fields
  are grouped per concern with one contract comment per group (not per field).
- **C3:** after B3 lands, `pushDataAccessTasks` and `compensationSafeForYield` are each ≤80
  lines or decomposed into named phase functions along the boundaries the current comments
  delineate; zero behavior change (full cascades + relational suites green, no plan-shape
  diffs).
- **C5:** `AggregateOperands` has no nil-slot-means-text contract; `aggregateArgText` and
  `isBareColumnIdentifier` deleted.
- **D1:** plan-shape suite asserts on typed plan nodes; zero disjunctive pins remain (each
  either exact or the nondeterminism fixed per RFC-167).
- **D2:** the `expected = 42` count pin is gone, replaced by assertions that carry
  information: every rule registered exactly once (no duplicates) and every exported rule type
  present in the set.
- **E1:** `GetReadVersion` reads `tx.readVersion` under `readVersionMu`; a concurrent
  GetReadVersion-vs-commit-path hammer test runs under `-race` (a bare "`-race` clean" is
  near-vacuous — the suite already is).
- **E2:** one documented contract for deferred-error fields, applied to both `rywPoisonErr`
  and `invalidAtomicOpErr`.
- **E3:** the libfdb_c differential suite gains named cases for the pipelined-vs-full-RYW
  boundary (pending atomic on the read key; range straddling a pipelined write); the PR
  closing E3 lists them. A deferral with no verifiable artifact is how deferrals evaporate.
- **F1:** repo root contains no stale scratch docs; one divergence doc.
- **F3:** CLAUDE.md contains the red-nightly carve-out.

## 6. Risks

- **Comment sweeps deleting load-bearing rationale.** Mitigation: the B-track rule is
  *relocate, then shrink* — reasoning moves to the cited RFC before the in-source essay is cut
  to invariant + pointer. Reviewers diff the RFC additions against the comment deletions.
- **File splits under active development.** C1/C5 queue behind 173 precisely for this; C3
  waits for B3. Nothing splits a file another in-flight PR is churning.
- **D1 over-pinning.** Structured assertions are stricter than substrings; expect a wave of
  pins that were passing only via looseness. Each such failure is triaged as
  regression-vs-was-never-right before adjusting the pin — the OR-pins exist because two plan
  shapes genuinely occurred, and RFC-167 says that itself is the bug.
- **F2 false positives** (e.g. a legitimate "codex" string in test fixtures). Gate scopes to
  non-test source comments; allowlist file for the genuinely legitimate hit, empty at birth.

## 7. Review log

- **Graefe — ACK-with-nits (2026-07-02, folded).** A2/C3/C4/C5/D1/§4 all ACK. Conditions on
  the cost-monotonicity retirement, both folded into §2 A2: (1) frame the break as Go's
  first-member approximation, NOT as "not a Cascades invariant under merging" (it is one);
  (2) register restoring the monotonicity pin when winner-based child costing lands.
- **Torvalds — ACK-with-nits (2026-07-02, folded).** All citations survived hostile
  spot-check. Nits folded: §5 criteria added for C3/D2/E1/E2/E3; C2 criterion made mechanical
  (zero option-backing fields on `Transaction`); §4 freeze paragraph resolved into the owner's
  actual decision. F2 comment-scope note: gate scopes to comments, not string literals
  (allowlist covers residue).
- (pending) FDB-C-dev — Tracks B1/B4/E (fdbgo surface) at execution time, per-PR.
- (pending) codex, @claude — PR #439.
