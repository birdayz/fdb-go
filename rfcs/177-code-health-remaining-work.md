# RFC-177: Code-health remaining work — deferred RFC-175/176 tracks, the owed C4 index-only RFC, and the registered client/infra gaps

**Status:** DRAFT — needs the standard gauntlet. Tracks A/B/C touch the Cascades engine or the
relational query surface → **Graefe ACK** on this RFC and on each implementation PR. Track D is
`pkg/fdbgo` → **FDB-C-dev ACK**. Torvalds + codex + @claude gate everything.
**Origin:** the RFC-175 (code-health remediation) and RFC-176 (semantic plan identity) campaigns
landed their main phases (RFC-175 A/B/E/F + C2/C3/D2 + B3/B5; RFC-176 P1/P2/P3). What remains is
(i) the RFC-175 tracks deliberately parked behind the RFC-173 freeze (C1, C5, D1); (ii) the two
RFC-176 §7 follow-ups (the cost tie-breaker order and the predicate/selector semantic hash); (iii)
the C4 index-only gating end-state, which RFC-175 registered as **needing its own RFC** — this is
that RFC; (iv) three `pkg/fdbgo` gaps registered in TODO.md "Known gaps" during the E1/E2/E3 work;
(v) the stress-1M baseline-rot investigation registered by RFC-176 P2's gate. This document is the
single home + execution order for all of it, so items get executed or explicitly rejected instead
of decaying in scattered TODO entries.
**Cross-refs:** RFC-175 (§2 C1/C4/C5/D1, §4 freeze sequencing), RFC-176 (§7 the two follow-ups),
RFC-173 (the active freeze — Track A's gate), RFC-151 (`ImplementFilterRule` `!isIndexOnly()` gate,
the C4 precedent), RFC-167 (plan determinism — D1's "nondeterminism is a bug" rule), DIVERGENCES.md
(C4 end-state; the fdbgo gaps' C++ cites).
**Effort:** Track B ≈ 1-2 shifts each; Track C (C4) ≈ 3-5 shifts, staged, Graefe per step; Track D
≈ 1 shift each; Track E ≈ 1 shift (requalify) or a bisect; Track A ≈ C1 1 shift / C5 1-2 / D1 2-3,
but **all three stay blocked until RFC-173 lands** (§4). Staged PRs, each independently green.

---

## 1. Problem

Two code-health campaigns are substantially done. Their un-executed remainder is real work with
real citations, currently living in three places that disagree about what is scheduled: RFC-175 §4
(the freeze table), RFC-176 §7 (the two identity follow-ups), and TODO.md's `# NEXT` / "Known gaps"
sections. Scattered registration is how work rots — a 30-line TODO entry reads as "handled" when it
is only "written down." This RFC consolidates the remainder into tracks by review gate and blocking
status, discharges the "C4 needs its own RFC" debt with a real staged design, and states — per item
— exactly what unblocks it. All citations below are verified against master @ 2026-07-03
(RFC-173 Slice 3 W1, `bf8809c72`; this branch's own doc commit sits one past it).

## 2. Inventory by track

### Track A — RFC-173-gated structural cleanup (Graefe / relational; STILL BLOCKED)

These three churn the exact files RFC-173 is actively migrating, or depend on plan shapes RFC-173
changes. RFC-175 §4 parked them behind the freeze; RFC-173 is at Slice 3 of ~6, so they remain
parked. Listed here with their unblock condition so they resume deterministically, not on a guess.

- **A1 (was RFC-175 C1) — split `logical_predicate.go`.** 6,050 lines (verified), whose name covers
  ~a fifth of it: the file also holds the DML builder surface (`buildLogicalPlanForInsertWithCatalog`,
  `…Update…`, `…Delete…`), UNION construction, and aggregate/HAVING rewriting. Mechanical split into
  `dml.go` / `aggregate.go` / `union.go` / `predicate.go`; functions are individually small, so it is
  tangling-by-aggregation, not spaghetti. **Unblock: after RFC-173 completes.** (Corrected from the
  first draft's "after Slice 4": `logical_predicate.go` has zero `AnchoredJoin` refs — that lives in
  `cascades_generator.go` — and the file is still edited in Slice 6 name-burial, E2/E4/D15-17. Splitting
  it before 173 stops touching it is the rebase poison this RFC warns of.)
- **A2 (was RFC-175 C5) — retire `LogicalAggregate`'s text/typed dual model.**
  `core/query/logical/operators.go:320-325` still carries `Aggregates []string` (:320) alongside
  `AggregateOperands []values.Value` (:322) with the **"nil slot = use text"** contract, and
  `Having string` (:324) alongside `HavingPredicate` (:325) (all verified present). The text fallback
  forces the two re-lexers `aggregateArgText` (cascades_translator.go:571) and `isBareColumnIdentifier`
  (:584) to re-parse a string that was an AST. End-state: operands + HAVING are always typed Values;
  the text fields die or become display-only; both re-lexers deleted. **Unblock:** RFC-173 Slice 3's
  RFC-088 HAVING-over-join work settles — it collides directly with the aggregate/HAVING rewriting at
  `cascades_translator.go:3234,3255`. (`AggregateOperands` is already populated today
  (`logical_predicate.go:1551`); the gate is the Slice-3 collision, not a not-yet-typed operand.)
- **A3 (was RFC-175 D1) — structured plan-shape assertions.**
  `sqldriver/plan_shape_conformance_test.go` is 19,588 lines with 46 `strings.Contains` plan-string
  assertions, some disjunctive (accept `NestedLoopJoin` OR `FlatMap` — a join-strategy regression
  passes silently). Move to structured assertions over the typed plan-operator tree; disjunctive pins
  become exact, and any surviving plan nondeterminism is itself a bug per RFC-167, fixed not tolerated.
  **Unblock:** plan shapes converge to Java after RFC-173 lands — re-pinning pre-173 shapes structurally
  would pin them twice. Earliest safe point: 173 complete.

### Track B — plan-identity completion (Graefe; EXECUTABLE NOW)

The two RFC-176 §7 follow-ups. RFC-176 killed rendering-keyed identity for Values across the ten plan
sites; these close the same defect class where it survives.

- **B1 — predicate/selector semantic hash.** Five plan sites still fold *renderings* into
  `HashCodeWithoutChildren` while their equality is structural — the RFC-176 §1 defect class one
  type-family over (equal⟹same-hash holds only while each rendering stays exactly injective over the
  structural discriminators; a standing whack-a-mole). Verified on master: `predicates_filter.go`,
  `filter.go`, `nested_loop_join.go` fold `pr.Explain()`; `load_by_keys.go` folds
  `keysSource.String()`; `selector.go` folds `planSelector.String()`. **Substrate already half-built:**
  `predicates.SemanticHashCode` exists (predicates/semantic_hash.go:21) — B1 is *wiring*, not
  green-field: point the three predicate sites at it, add `keysSource`/`planSelector` structural hash
  methods (mirroring their existing `Equals`), and tighten each in lockstep with its equality (the P1
  lesson: hash and equality must move together or equal⟹same-hash breaks). Same gate battery as
  RFC-176 P2 (property test, stress-1M, plandiff corpus, task-count baseline, tie-breaker-consumer
  audit). Note (Graefe): `predicates.SemanticHashCode`'s default arm falls back to `Explain()`
  (semantic_hash.go:54) for predicate types the switch doesn't reach — B1 should close that fallback
  for the exotic types too, or the whack-a-mole stays ajar inside `predicates/` even after the five
  `plans/` sites are wired.
- **B2 — canonical semantic total order for the cost tie-breaker.** When two plans are cost-tied AND
  criteria-tied, the winner is decided by raw hash ordering — `costExprHash` (planning_cost_model.go:350)
  / `deepHashCode` (:841). So ANY hash evolution re-rolls the winner among cost-tied semantically-distinct
  plans; RFC-176 P2 is the existence proof (its alias-invariant hashes flipped a pinned vector winner,
  fixed there by turning that specific tie into a *cost* decision — but the tie-break CLASS remains for
  every other equal-cost pair). Replace the hash ordering with a **canonical total order over plan
  structure** (operator kind, arity, discriminators, recursively) so winners are stable under hash
  changes and alias renaming. This is the deterministic-extraction property RFC-167 wants, made a
  first-class order instead of a hash accident. Gates: determinism 10×, stress, corpus, task-count.

### Track C — the C4 index-only gating end-state (Graefe) → **promoted to RFC-178**

RFC-175 C4 registered this as "needs its own RFC." A sketch of the staged plan lived here in the
first draft; the RFC-177 gauntlet (Graefe NAK + codex) proved a registry line was the wrong shape —
it scheduled retiring an already-retired rule (`ImplementIndexScanRule`, gone since RFC-076) and
deleting a *live* diagnostic (`findIndexOnlyLogicalResidual`, the intended-rejection path, not
net-scaffolding). A 3-5-shift, per-step-Graefe-gated engine refactor with those subtleties is a
standalone RFC. **See `rfcs/178-index-only-residual-emergent.md`** — the corrected design (two live
producers not three, `ImplementSimpleSelectRule` gating as a 1:1 Java port, keep-and-expand the
logical diagnostic, de-stale DIVERGENCES.md as Step 0, net retired last). Track C is discharged by
RFC-178; nothing further is owed here.

### Track D — `pkg/fdbgo` client gaps (FDB-C-dev; EXECUTABLE NOW)

Three gaps registered in TODO.md "Known gaps" during the RFC-175 E1/E2/E3 work, each with its C++
cite. All three are `pkg/fdbgo` → FDB-C-dev gate, C++ 7.3.77 at `/tmp/fdbsrc` is the spec.

- **D1 — `rywDisabled` lock-free on the reset boundary.** A plain bool written by
  `SetReadYourWritesDisable` + `applyOptionDefaults` (both reset paths) and read lock-free by every
  read-path gate incl. WatchSetup's 1034 check and Commit's ship-decision — the same reset-boundary
  shared-field class E1 already fixed for `timeoutNs`/`deadlineNs` (the sanctioned Reset-cancels-watches
  overlap can race the rewrite). Fix: `atomic.Bool`, matching the E1 treatment; `-race` hammer on the
  reset/read boundary.
- **D2 — GRV reply's `ProxyTagThrottledDuration` discarded.** C++ accumulates it into transaction state
  (`NativeAPI.actor.cpp:7410`) in addition to the client-side per-1213-error constant add (`:7761`); Go
  implements only the constant add and both GRV parsers discard the parsed reply field →
  `GetTagThrottledDuration` undercounts vs libfdb_c. Fix: accumulate the reply field, port the exact C++
  accumulation; differential-verify the accessor.
- **D3 — deferred-vs-cancelled precedence on a doubly-terminal txn.** C++ checks `deferredError` at every
  ThreadSafeTransaction op lambda BEFORE the actor observes `resetPromise`, so a txn both poisoned
  (2000/2018) and `Cancel()`ed surfaces the *deferred* code; Go's uniform entry order is cancelled-first,
  so it surfaces 1025. Observable only on that double-terminal corner (deferred-beats-timeout and
  deferred-beats-1034 are already aligned + pinned). Fix: a differential probe (poison, Cancel, Get on
  both clients — mind MultiVersionTransaction reordering) and, if confirmed, one swap of the two gates at
  each entry point, one FDB-C-dev cycle.

### Track E — stress-1M baseline requalification (infra)

Registered by RFC-176 P2's stress gate: current **master** violates the "Stress test 1M baseline"
Threshold column on an idle box (in_list 14.97ms vs <10ms; needles 5.4/6.4ms vs <5ms; pk lookups
5.0–7.2ms vs <5ms), while the P2 branch was noise-identical to master with byte-identical EXPLAINs
(the gate fails on its own base). A safety-net table nobody can pass measures nothing — a genuine
5→15ms point-lookup regression would now hide inside the already-red gate. **Decision path (Torvalds'
required shape):** (1) re-qualify the environment vs the May baseline run — machine, Docker, FDB
image, kernel; if any changed, re-measure and UPDATE the table + thresholds; (2) if unchanged, bisect
May→HEAD on the violated rows. **Terminal state: the baseline table is re-qualified/updated OR the
regressing commit is named.** (The adjacent TODO INFRA item — nightlies dispatching hours late — is
pre-existing, not part of this campaign's remainder, and is out of scope here.)

## 3. Non-goals

- No RFC-173 work, and no un-freezing it — Track A explicitly stays parked; this RFC only records the
  exact unblock signal per item so resumption is deterministic.
- No wire-format or record-layout change. Track D closes client-behavior gaps against the C++ spec
  (matching libfdb_c is closing a divergence, not changing format).
- Track C is planner *scheduling/gating* semantics, not cost-model or row-model changes — it does not
  touch RFC-176's cost tie-break (that's B2) or 173's rows.
- The `hunt/cascades-bug-hunt` open items (separate multi-agent hunt, own Graefe cycles) are not folded
  in — different provenance, already tracked.

## 4. Sequencing vs the RFC-173 freeze

| Track | Gate | 173 interaction | When |
|---|---|---|---|
| A1 (C1 split) | Graefe/relational | **direct** — 173 edits `logical_predicate.go` through Slice 6 | after 173 complete |
| A2 (C5 aggregate) | Graefe/relational | **direct** — Slice-3 RFC-088 HAVING-over-join collision | after 173 Slice 3 |
| A3 (D1 assertions) | Graefe/relational | plan shapes converge post-173 | after 173 complete |
| B1 (predicate hash) | Graefe | none — `plans/` + `predicates/`, not 173's files | **now** |
| B2 (tie-break order) | Graefe | none — cost model | **now** |
| C (RFC-178) | Graefe, staged | none — planner rule/gate structure (C3 de-noised these files un-blocked) | **now** (own RFC) |
| D1/D2/D3 (fdbgo) | FDB-C-dev | zero — different subsystem | **now** |
| E (stress baseline) | infra | none | **now** |

Executable immediately: **B, C (via RFC-178), D, E**. Blocked: **A** (all three), until the named 173
milestones. This is the same keep-the-lights-on lane the owner authorized for the RFC-175/176
execution — Tracks B–E are zero-conflict with 173; Track A would be rebase poison and waits.
Recommended first pickups (highest value / lowest risk): B1 (substrate already half-built), D1/D3
(small, C++-cited, close real divergences), E (an hour to requalify, re-arms a broken safety net). C
is the largest and runs under its own RFC-178, staged.

## 5. Acceptance criteria

- **A1:** `logical_predicate.go` < 1,500 lines, only predicate construction; DML/aggregate/union in
  their own files; zero semantic diff (pure move; full relational suite + plandiff corpus green).
- **A2:** `AggregateOperands` has no nil-slot-means-text contract; `HavingPredicate` always populated;
  `aggregateArgText` and `isBareColumnIdentifier` deleted; text fields display-only or gone.
- **A3:** plan-shape suite asserts on typed plan nodes; zero `strings.Contains` plan assertions and
  zero disjunctive pins remain (each exact, or the nondeterminism fixed per RFC-167).
- **B1:** the five sites hash via `predicates.SemanticHashCode` / new `keysSource`/`planSelector` hash
  methods; zero `Explain()`/`String()` calls inside any `HashCodeWithoutChildren` in `plans/`
  (greppable); equal⟹same-hash property test over predicate-bearing plans; RFC-176-P2 gate battery green.
- **B2:** the cost tie-breaker uses a structural canonical order, not `costExprHash`/`deepHashCode`
  ordering; a test pins that two cost-tied-and-criteria-tied plans order deterministically under a
  perturbed hash function; determinism 10×, stress, corpus, task-count green.
- **C:** discharged by RFC-178 — see its §5 (de-stale DIVERGENCES.md; gate `ImplementSimpleSelectRule`
  + NLJ; keep-and-expand `findIndexOnlyLogicalResidual`; retire the `validateNoIndexOnlyResidual`
  *physical net* last with a measured-zero-invocation proof; keep `residualIsPartitionContiguous`).
- **D1:** `rywDisabled` is `atomic.Bool`; `-race` reset/read hammer test (a bare `-race` clean is
  vacuous — it must race the two paths).
- **D2:** `GetTagThrottledDuration` matches libfdb_c on a GRV-throttled differential case; the reply
  field is accumulated per `NativeAPI.actor.cpp:7410`.
- **D3:** a differential probe establishes the true precedence; if Go diverges, the gate swap lands and a
  regression pins deferred-beats-cancel on the double-terminal txn.
- **E:** the baseline table is re-qualified with the current environment recorded, OR a bisect names the
  regressing commit; either way the Threshold column is passable on a documented box.

## 6. Risks

- **Track A resumption drift.** Blocked work accumulates rebase distance. Mitigation: the unblock signal
  is a specific 173 slice, not "173 done" hand-waving — A2 can start the moment Slice 3's flip settles,
  before Slices 5/6. Re-verify the §2 citations at pickup (they will have drifted).
- **C retiring the net too early.** The net is a correctness backstop; deleting it before C.1–C.3 fully
  gate the producers reintroduces the vector-KNN panic class (index-only `DistanceRank` reaching
  `Comparison.EvalAgainst`). Mitigation: net retired in the LAST PR only, gated on the full vector sentinel
  set; each earlier step keeps the net as belt-and-suspenders.
- **B1/B2 winner flips.** Any plan-identity or tie-break change can move memo winners (RFC-176 P2's
  lesson: the hash feeds plan logging + the cost tie-breaker). Mitigation: the RFC-176 P2 gate battery,
  incl. the non-memo hash-consumer audit and stress-1M against a **requalified** baseline (so Track E
  should land before B1/B2's stress gate, or B1/B2 use master-parity + identical-EXPLAIN as the gate the
  way P2 did).
- **D3 false divergence.** MultiVersionTransaction may reorder, making the probe read a wrong precedence.
  Mitigation: the probe runs against both clients on one cluster and is confirmed before any gate swap;
  no code change on an unconfirmed divergence.

## 7. Review log

- **Graefe — NAK → addressed (2026-07-03).** Six factual must-fixes, all folded: Track C's phantom
  C.3 (`ImplementIndexScanRule` retired by RFC-076), the invented `ImplementPhysicalScanRule` cite, the
  three-vs-two producer count, C.1-as-1:1-port, A1's wrong Slice-4 gate (→ after 173 complete), A2's
  unsupported "redefines typed operand" rationale (→ the RFC-088 HAVING-over-join collision). Track C
  promoted to RFC-178 with the corrected design; Track B ACKed (fallback nit noted in B1). Re-request
  after the fold.
- **Torvalds — ACK-with-nits (2026-07-03).** All citations verified against the tree. Nits folded:
  promote Track C to its own RFC (→ RFC-178), operators.go off-by-one (→ :320-325), stale SHA. Track A
  parking and D3 probe-then-swap confirmed legitimate method, not dodges.
- **codex — one P2 folded (2026-07-03).** Track C.4 deleting `findIndexOnlyLogicalResidual` with the net
  would regress metric-mismatch/no-index vector queries to a generic failure — the logical diagnostic is
  the intended-rejection path, not net-scaffolding. Corrected in RFC-178 (keep and expand it to the JOIN
  sentinel; retire only the physical net).
- (pending) FDB-C-dev — Track D (fdbgo) at execution time, per-PR.
- (pending) @claude — this RFC PR.
