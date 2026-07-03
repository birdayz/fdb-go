# RFC-178: Make index-only-residual rejection emergent — gate the physical-filter producers, retire the physical net, and de-stale DIVERGENCES.md

**Status:** DRAFT — Cascades engine change → **Graefe ACK** on this RFC and on each staged
implementation PR (per-step re-ack). Torvalds + codex + @claude gate everything.
**Origin:** RFC-175 registered this as C4 "needs its own RFC"; RFC-177 sketched it as a registry
line and its gauntlet (Graefe NAK + codex) proved the sketch was stale — it scheduled retiring an
already-retired rule and deleting a live diagnostic. This is the corrected, focused RFC, promoted out
of RFC-177 §Track C (now a one-line pointer here). All facts verified against master @ 2026-07-03
(`f7b5c9c85`) and Java `fdb-record-layer` at the repo root.
**Cross-refs:** RFC-151 (`ImplementFilterRule` `!isIndexOnly()` gate — the landed precedent),
RFC-076 (retired `ImplementIndexScanRule` — why C4's old "retire it" step is a no-op), RFC-045 (vector
K-NN — the feature whose index-only `DistanceRank` operand this whole net protects), DIVERGENCES.md
(§33-41, §376 — stale, corrected by this RFC).
**Effort:** 3-5 shifts, staged; each step its own PR + Graefe re-ack. The physical net retires in the
LAST PR only.

---

## 1. Problem

Java enforces "an index-only `Value` can never be an executable residual filter" as an **emergent
property**: every rule that could turn a matched predicate into a physical filter is gated on
`anyCompensatablePredicate()` at the point it builds, so an index-only predicate is simply never
built into a residual. The property holds by construction, in each producer, once.

Go enforces the same property with a **bolted-on catch-all net** — `validateNoIndexOnlyResidual`
(plan_executability.go:49) walks the *finished* physical plan and rejects any index-only residual
that slipped through, plus a logical-side companion `findIndexOnlyLogicalResidual` (:106) for the
complementary case (the gate leaves the best plan non-physical, so the physical walk sees nothing).
The net exists because two Go physical-filter producers are **not yet gated** and can emit the
residual the net then catches. The planner's own comment says so (planner.go:551 region): "Do NOT
remove that net until every such builder is gated/retired."

A net over the generic planner is strictly worse than per-producer gating: it runs on every plan, it
is a second place the property lives (drift risk), and — most concretely — it fired the vector K-NN
regression Graefe + Torvalds caught in the JOIN shape (`TestVectorPlan_MetricMismatchInJoinDoesNotLeak`),
which per-producer gating would have made structurally impossible. The end-state is Java's: gate the
remaining producers, then the physical net is dead code and deletion *proves* the gating is complete.

## 2. Current state (verified — and note DIVERGENCES.md lies about it)

**Already gated (no work):**
- `ImplementFilterRule` — carries Java's `all(anyCompensatablePredicate())` / `!isIndexOnly()` gate
  (RFC-151); index-only predicates return early, never built.
- The data-access / compensation match path — `DataAccessForMatchPartition`
  (abstract_data_access_rule.go:266) stamps impossibility once via `PredicateMultiMap.ofPredicate`
  (`isImpossible`, `PredicateMultiMap.java:181,190`), Java's single stamping point.

**Not gated — the two live producers this RFC targets:**
- `ImplementSimpleSelectRule` — builds a physical filter from a `SelectExpression`'s predicates (the
  JOIN shape, where the distance is a Select predicate, not a standalone `LogicalFilter`).
- The NLJ residual builder.

**`ImplementIndexScanRule` is NOT a producer — it was retired by RFC-076.** There is no type,
constructor, or file; only 16 stale comments and DIVERGENCES.md reference the name (verified:
`phase2_fuzz_test.go:12` "after RFC-076 retired ImplementIndexScanRule"; no
`type ImplementIndexScanRule`, no rule-set registration). RFC-177's C.3 "retire it as the last leak
path" was a phantom step — struck here.

**The two backstops, which are DIFFERENT and must not be conflated:**
- `validateNoIndexOnlyResidual` — the **physical net**. Walks the finished plan. Retires when the
  producers are gated (nothing can reach it).
- `findIndexOnlyLogicalResidual` — the **live logical diagnostic**. Surfaces the clean
  `UnplannableIndexOnlyResidualError` when an index-only predicate that no index serves is correctly
  left in the logical tree (metric-mismatch, no-such-index). This is the *intended-rejection* path,
  not a leak-catcher — it **STAYS** (RFC-177's C.4 wrongly scheduled it for deletion; codex caught it).

**DIVERGENCES.md is stale and must be corrected as part of this RFC** (it is the source RFC-177
inherited its errors from): §33-41 describes `ImplementIndexScanRule` as a live "second index-scan
path" and pins it to `TestImplementIndexScanRule_SkipsIndexOnlyResidual` (grep: does not exist); §37
invents a Java class `ImplementPhysicalScanRule` (Java scans come from `AbstractDataAccessRule`); §376
lists three physical-filter builders when only two are live. De-staling DIVERGENCES.md is a deliverable
(§3 step 0), not a side effect.

## 3. Design (staged; net retired LAST)

- **Step 0 — de-stale DIVERGENCES.md** (docs-only, unblocks honest reasoning for the rest). §33-41 →
  "`ImplementIndexScanRule` retired (RFC-076); two live ungated producers remain"; drop the phantom
  test cite and the invented `ImplementPhysicalScanRule`; §376 → two builders. Land first so the
  remaining steps cite a truthful map.
- **Step 1 — expand `findIndexOnlyLogicalResidual` to every shape the gates will leave logical** (the
  JOIN `SelectExpression`, and the NLJ residual shape) — BEFORE any gating (Graefe: the expand MUST
  precede the gate, or the gating PR's own sentinel goes red). Today it handles only
  `LogicalFilterExpression`. The expansion is **dormant until a gate fires**: `findIndexOnlyLogicalResidual`
  runs only when the best plan is non-physical (planner.go:246), and until a producer is gated the plan
  is still physical — so landing the expanded diagnostic first is safe *and* necessary, giving the clean
  `UnplannableIndexOnlyResidualError` path a home before the gate routes rejection to it. (Equivalent
  packaging: fold each shape's expansion into the same PR as the gate that needs it — the invariant is
  "no gate lands without its shape's diagnostic ready.")
- **Step 2 — gate `ImplementSimpleSelectRule` on `anyCompensatablePredicate()`.** A **1:1 port** of
  Java's literal rule, not an analogy: `ImplementSimpleSelectRule.java:90` binds
  `predicateMatcher = anyCompensatablePredicate()` (used via `all(...)` at :94). After this, no index-only
  predicate is built into a Select's physical filter; the JOIN sentinel
  (`TestVectorPlan_MetricMismatchInJoinDoesNotLeak`) must still reject — now via the Step-1 logical
  diagnostic, provably (disable the physical net in the test and confirm the reject still fires).
- **Step 3 — gate the NLJ residual builder identically** (its shape's diagnostic landed in Step 1).
- **Step 4 — retire `validateNoIndexOnlyResidual` (the PHYSICAL net) — LAST PR.** With both producers
  gated and the data-access path already stamping impossibility, no physical builder can emit an
  index-only residual; the physical net is unreachable. Deletion is the proof the gating is complete
  (Graefe's standing condition, PR #442: "retire the net LAST, keep the sentinels"). `findIndexOnlyLogicalResidual`
  is NOT deleted — it is the intended-rejection path.
- **Step 5 — reassess the vector special-cases.** `residualIsPartitionContiguous` (planner.go:603-604,
  690-696) **STAYS** — it carries a real correctness property (the pk-intersection drops rows for k>1, so
  a whole-partition residual composing above a self-limiting scan is the only correct shape), pinned by
  the K>1 partition-intersection sentinels; it is not net-scaffolding. Assess only `isNilInnerFetch` and
  the `*physicalVectorIndexScanWrapper`/`*physicalAggregateIndexWrapper` type-switches in
  `compensationSafeForYield`: keep what encodes cost/correctness, retire only what existed to feed the net.

## 4. Validation (per step, Graefe-gated)

Every step keeps the full vector sentinel set green: `TestVectorPlan_MetricMismatchDoesNotMatchVector`
(single-table), `TestVectorPlan_MetricMismatchInJoinDoesNotLeak` (JOIN — the regression the net was
retained for), `TestVectorPlan_QualifyPlansToVectorScan` (legit vector scan still plans), plus the K>1
partition-intersection pins. Plandiff cross-engine corpus: 0 new mismatches at each step. The net-retirement
PR (Step 4) additionally proves the physical net is unreachable — instrument it to count **catches** (times
`validateNoIndexOnlyResidual` actually returns a rejection, i.e. finds an index-only residual), NOT calls:
the net runs unconditionally on every plan (planner.go:238), so a call-counter never reads zero and would
prove nothing (Graefe + codex). Zero catches across the full suite + fuzz before deleting is the real proof.

## 5. Acceptance criteria

- **Step 0:** DIVERGENCES.md §33-41/§376 no longer describe `ImplementIndexScanRule` as live, cite no
  non-existent test, invent no Java class, list two producers.
- **Step 1:** `findIndexOnlyLogicalResidual` handles the JOIN `SelectExpression` shape (and the NLJ
  residual shape); a metric-mismatch query in each shape surfaces `UnplannableIndexOnlyResidualError`, not
  a generic failure, when the physical net is disabled in the test — i.e. the clean path exists BEFORE the
  gate that will depend on it.
- **Steps 2-3:** `ImplementSimpleSelectRule` and the NLJ builder return early on any index-only predicate
  (greppable `!isIndexOnly()` / `anyCompensatablePredicate` gate); the JOIN + single-table metric-mismatch
  sentinels reject via the Step-1 logical diagnostic (verify by temporarily disabling the physical net in
  the test and confirming they still reject) — each gating PR independently green.
- **Step 4:** `grep -rn "validateNoIndexOnlyResidual" pkg/` returns zero; the net-**catch** counter (non-nil
  rejections, not calls) read zero across suite+fuzz before deletion; `findIndexOnlyLogicalResidual` still
  present and pinned.
- **Step 5:** `residualIsPartitionContiguous` retained with its K>1 pins green; any retired special-case
  named with the reason it was net-only.

## 6. Risks

- **Net retired too early** → reintroduces the vector K-NN panic (index-only `DistanceRank` reaching
  `Comparison.EvalAgainst`). Mitigation: net retired in the LAST PR only, gated on the full sentinel set +
  the measured-zero-invocation proof.
- **Deleting the logical diagnostic** (RFC-177's original error) → metric-mismatch/no-index queries regress
  to a generic planning failure. Mitigation: Step 3 expands and keeps `findIndexOnlyLogicalResidual`; it is
  explicitly NOT part of the net retirement.
- **Winner/shape drift from gating** → a gated producer changes which plan wins. Mitigation: plandiff corpus
  + vector sentinels per step; determinism check on the JOIN shapes.

## 7. Review log

- **Graefe — NAK → addressed (2026-07-03).** RFC-177-gauntlet corrections folded from birth (phantom C.3
  struck, two-not-three producers, C.1 as 1:1 port, keep-the-logical-diagnostic, DIVERGENCES de-stale as
  Step 0). Re-review NAKed two design specifics, both fixed: (1) the diagnostic-expansion must PRECEDE the
  gates (else the gating PR's own JOIN sentinel goes red) — steps reordered so expand is Step 1; (2) the
  net-retirement proof must count **catches** (non-nil rejections), not calls (the net runs
  unconditionally) — §4/§5 corrected. Re-request after the fold.
- **codex — P2 (folded).** Same catch-vs-call vacuity as Graefe (2); plus RFC-177 top-matter still claimed
  to be the C4 RFC after the promotion — de-owned (title/origin/effort now point to RFC-178).
- (pending) Torvalds, @claude — this RFC PR and each implementation PR.
