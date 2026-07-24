# RFC-190 — Cascades quality audit v2

Status: **Implemented — final whole-RFC review pending.** Graefe and Torvalds ACKed the RFC design,
and every numbered implementation item (190.1–190.14, including 190.11-FU and the bundled latent
closures) is complete with its regression, golden, and validation evidence below. The final
whole-PR Graefe/Torvalds delta review and current-head `@claude` review remain the release gate.
The accepted 190.1 deliverable is the N-way-path convergence; its separately scoped two-way and
gathered-cluster follow-ons are recorded below and are not silently claimed as part of this RFC.
190.1 was **materially re-designed** after its original delete premise proved false (the arm's
"never produced an executable plan" gravestone is a lie — deleting it regresses working FDB tests).
The new 190.1 (converge on Java's two-rule architecture via a guarded `PartitionSelectRule`) carries
a fresh **Graefe ACK** from a two-round adversarial design dialogue (round 1 NAK: a wrong-rows
merge-case hole; round 2 ACK: the live-existential guard verified airtight) **and a Torvalds ACK**
(three impl-discipline conditions folded: Step 1+2 atomic, Step 3 helper-reference proof, Step 5 1M
stress gate; LOC corrected to arm 388 / wrap 477).

**190.1 MILESTONE COMPLETE (Step 1 core + the arm retirement).** The
direct-emit + guard + arm-retirement landed atomically; the full N-way matrix returns correct rows
(comma-join projected EXISTS was crash→`[100|true,200|false,300|true]`; buried_inner PK no longer
panics; `_Discriminating`, WHERE-EXISTS, NOT-EXISTS, 4-leg all green), the plan-shape golden is
BYTE-IDENTICAL (zero corpus drift), and the full 1165-test sqldriver sweep is green. Two things the
design did NOT anticipate, both folded and flagged for the review lap:
- **A pre-existing executor bug the new shape exposed (`isIdentityInnerRV`).** A FlatMap result value
  can be an identity pass-through of the INNER quantifier, not only the outer; `flat_map_cursor.go`
  had `isIdentityOuterRV` but no inner mirror, so when the cost model assigned `PartitionSelectRule`'s
  Case-2 flowed alias to the physical inner leg, `computeResultLegs` scalar-wrapped a whole row → the
  E-scan's PK-column SARG (`P.ID#0`) read that wrapper's slot 0 and got the nested record (unencodable
  → panic; the non-PK `P.K#2` case got lucky). Fixed by adding the symmetric inner-identity branch —
  a principled completion of a missing case, benefits any Case-2 select the cost model flips inner.
- **Scoped bail, not full removal (reviewed deviation from the pure design).** Removing
  the `existentialCount==1` bail ENTIRELY raced the working Go-only 2-way arm (2 ForEach + 1 Existential)
  and yielded malformed plans on 7 existing tests. Scoped to `existentialCount==1 && foreachCount<=2`
  (keep the working 2-way case on the arm; partition only the genuine N-way, `foreachCount>2`). The
  guard's correctness proof is unaffected (the 2-way case never reaches it). Full 2-way convergence
  (retire the 2-way arm too) is a tracked follow-on; the milestone review accepted the scoped bail
  as RFC-190's correct scoped endpoint.

**190.1 MILESTONE REVIEW LAP: Graefe ACK + Torvalds ACK on HEAD `95598761f`** (1M stress green =
Torvalds condition C satisfied). Graefe: direct-emit faithful to `QueryVisitor.java:429-434`, guard
airtight, executor fix principled (cost-based join direction is rightly free to place the flowed
alias inner; the outer-only `computeResultLegs` was a genuine gap), arm retirement correct. Graefe
ACKed the SCOPED BAIL as the correct endpoint for RFC-190's N-way milestone: keeping the Go-only
2-way arm follows "correct-or-decline beats convergence-at-any-cost" because dropping it ships
malformed plans while Go's `positionalMergeCase` cannot yet decompose a 2-alias existential
correlation. That ACK does not claim the separate full two-way convergence work is finished.
Torvalds ACKed the milestone and required the same follow-ups to remain explicit.
**Post-RFC follow-ons (§190.1-FU; tracked, not RFC-190 completion blockers):**
- **FU-1 (Graefe's condition):** fix `positionalMergeCase` to faithfully decompose a 2-alias
  existential correlation, THEN retire the Go-only 2-way existential arm too (`implementJoinWithExistential`'s
  2-leg fold) so `PartitionSelectRule`+binary-NLJ is the single path for ALL existential-join arities
  (full Java two-rule convergence). Until then the scoped bail (`existentialCount==1 && foreachCount<=2`)
  keeps the working 2-way arm.
- **FU-2 (Torvalds):** rename `qualifyOuterPositional`→`qualifyPositional` (it already serves the inner
  side, `streaming_cursors.go:1444` + the new `isIdentityInnerRV` branch — pre-existing naming smell).
- **FU-3 (Torvalds):** extract the ~12 lines the direct-emit branch shares with the AXIS-1 tail
  (`cascades_translator.go`) into a helper.
- **FU-4 (Step 3):** retire the WHERE-EXISTS gathered-cluster wrap (`translateExistsOverGatheredCluster`,
  477 lines) once cells 3/4/8 are served by the partitioned path — separable, non-urgent.
Branch: `feat/rfc190-cascades-quality-audit` · one branch, one PR.
Reviewers: Graefe (Cascades alignment) + Torvalds (code quality) on RFC and impl.

## Problem

A 2026-07-23 full-engine quality assessment (5 subsystem review agents cross-verified against
Java 4.12.11.0) graded the engine architecture A−, cost B, rule fidelity B+, tests B+, code B.
The wire-compat hard line is clean — every finding is read/optimization-path. This RFC grinds the
concrete residuals in one milestone, severity order (correctness first). Items are independent;
each lands its own commit(s) + regression test. Grouped for one review lap + one PR.

## Items

### 190.1 (HIGH) — EXISTS over an N-way join: converge the N-way path on Java's two-rule architecture

**Premise correction (the original delete plan was WRONG).** The first RFC-190 draft (Graefe+Torvalds
ACK) rested on the arm's gravestone comment: *"HAS NEVER PRODUCED AN EXECUTABLE PLAN."* **That comment
is FALSE.** Implementing the delete broke two FDB tests (`TestFDB_BuriedInnerJoinProjectedExists` +
`_Discriminating` → `0AF00: could not plan query`): the arm `implementNWayJoinWithExistential`
(`rule_implement_nested_loop_join.go:~2607`) DOES produce correct working plans (`[[10 true]]`,
Java-verified in-test) for the **explicit-`JOIN…ON`** projected-EXISTS-over-3-way shape. It only
crashes for the **comma-join** shape (RFC-173 ordinal tripwire). Deleting it is a net regression.
So 190.1 is not a dead-code delete; it is a real architecture fix — re-designed principles-first and
re-reviewed (this design carries a fresh **Graefe ACK** via a two-round adversarial design dialogue
that caught and closed a wrong-rows hole before any code; needs Torvalds ACK on the revised RFC).

**The functionality (form × projection × correlation).** EXISTS over a ≥3-way join, comma-join AND
explicit-JOIN surface forms, projected (`SELECT …, EXISTS(…)`) AND WHERE (`… WHERE EXISTS(…)`),
correlated AND uncorrelated. Java plans EVERY such shape through exactly two rules — `PartitionSelectRule`
(reduce an ≥3-quantifier `SelectExpression` to binary sub-selects; `PartitionSelectRule.java:63` admits
ANY quantifier via `all(anyQuantifier())`, existential included) and `ImplementNestedLoopJoinRule`
(matches EXACTLY 2 quantifiers; existential inner → `FirstOrDefault(NULL)` + existential-flag `FlatMap`,
`.java:187,313-316`). Java has **no N-way arm and no positional-merged-row join construct**.

**Root diagnosis.** Go carries a Go-only positional-ordinal join pipeline — the N-way arm AND a second
mechanism `translateExistsOverGatheredCluster` (`exists_gathered_cluster_wrap.go`, the WHERE-EXISTS
path) — that exists ONLY because Go's `PartitionSelectRule` (`rule_partition_select.go`) **refuses to
partition existential selects**: the `existentialCount==1` bail (`:67-69`) and the projected-multi
decline (`:70-86`). Both Go-only constructs are workarounds for that refusal. The code's own comment
admits it (`:57-60`): *"subsuming that Go-only arm into partitioning is separable … that migration is
its own slice."* This is that slice.

**Fix — Option A: make `PartitionSelectRule` partition existential selects the Java way (guarded).**
Replace the over-broad `existentialCount==1` bail with a **targeted per-bipartition guard**, so the flat
`[ForEach×N, Existential]` select decomposes into binary sub-selects Go's existing existential-FlatMap
path (`implementExistentialSelect`) already implements — SARG-preserving (no cross-product), projected
`ExistsValue` riding through unchanged — **retiring both the N-way arm and (follow-on) the wrap**.

The guard (design dialogue, Graefe-verified airtight): **reject any bipartition whose LIVE set
(`lowersCorrelatedToByUppers`, computed at `rule_partition_select.go:~415`) contains an existential
quantifier.** Rationale: that live set is exactly what `positionalMergeCase` collapses into positional
ordinals (`:550`, ≥2 live) and what Case-2 flows as the lower's own row (`:559`, ==1 live) — and Go's
merge/Case-2 machinery **cannot represent a projected existential as a positional ordinal** (a real Go
constraint Java lacks — Graefe's NAK, sustained). A **projected** δ in a lower is necessarily live
(`ExistsValue.GetCorrelatedTo`→δ, `value_exists.go:107`) → caught. A **WHERE** δ is never live (its
`ExistsValue` is a predicate classified to `lowerPredicates` at `:403`, never in the live set) →
admitted as a correct Case-1/Case-2 semi-join filter — so the guard **preserves the working WHERE
multi-EXISTS peel (cell 8)**. Case-1 (`:537`) flows only `LiteralValue(1)`, never a live alias, so it
needs no guarding. Graefe verified `:550`/`:559` are the ONLY sites that flow/collapse a lower alias,
that the guard's key is complete, and that no correct plan is lost (the complementary δ-upper split is
always co-enumerated, chaining to the terminal binary `[ForEach, Existential]`).

**Hazard proof (Graefe-ACK'd):** under the guard, no existential is ever a member of the live set at the
case dispatch → none is collapsed by `:550` or flowed by `:559`; a projected δ is always upper →
survives to the terminal binary select → `ImplementNestedLoopJoinRule` implements it as
`FirstOrDefault(NULL)` + existential flag. Graefe's `{a,δ}` counterexample (which reached
`positionalMergeCase` under the naive delete) is now rejected before `:550`. No wrong-rows path exists.

**Migration — CORRECTED after implementation probing (the RFC's original "Step 1 = stop the
un-enclosure, near-flag-flip" estimate was WRONG; proven empirically).** Go ordinalizes N-way
existential clusters at TRANSLATION time (positional seeds via the cluster-gate machinery,
`cluster_gate.go:399-419`, porting Java `QueryVisitor.java:429-434`). Two probes established the
truth: (i) the guard alone, on the existing ORDINAL seed, converts the comma-join crash into SILENT
WRONG ROWS (always-true EXISTS — δ→a mis-wires as an ordinal) → **the guard is UNSAFE on an ordinal
seed and CANNOT ship as a standalone safety net**; (ii) merely flipping `inInnerCluster=true` marks
the box name-model-enclosed but nothing ordinalizes it → `0AF00: join did not ordinalize`
(`cluster_gate.go:399`), breaking the WORKING explicit-JOIN shape too. So the emit, the guard, and
the arm-retirement are **ONE ATOMIC commit** — they cannot be separated.

The correct Step 1 is **direct-emit**: a Java-faithful port of `QueryVisitor.java:429-434` — dissolve
the ≥3-way INNER cluster into a flat NAME-model `[ForEach×N, Existential]` select (each leg a plain
alias-bound ForEach), **bypassing** `translateRef`-on-the-box and thus the ordinalization machinery
(which stays intact for every other shape). Java has no ordinal seed; its flat select is alias-bound —
direct-emit is that, ported. `PartitionSelectRule` then decomposes it BY ALIAS (its entire logic is
alias-keyed: `aliasToQ`, `computeTransitiveCorrelationOrder` from `GetCorrelatedTo`), so δ→a stays a
name correlation and the wrong-rows mis-wire is gone; the dup-column discriminator is *trivially* safe
(distinct QOVs, no positional last-leg-wins hazard).

- **Step 0** — correct the false gravestone (`rule_implement_nested_loop_join.go:2907`). No behavior
  change. (The RED comma-join FDB test lands in the atomic commit below, not as a separate red commit —
  no-red-commit rule.)
- **Step 1 (ATOMIC: direct-emit + guard + arm-retirement, ~250-400 lines, three call sites):**
  1. Add `gatherInnerClusterOnPredicates(j)` beside `gatherInnerClusterLegs` (`ordinal_seed.go`) —
     walk the inner-join tree collecting each node's `OnPredicate` (`gatherInnerClusterLegs` discards
     nested ONs — for `(p JOIN q ON q.qid=p.id) JOIN r ON r.rid=p.id` only the top ON is gathered; the
     buried `q.qid=p.id` must be recovered). Comma-joins carry predicates in the WHERE
     (`splitNonExistsPredicates`) — already gathered.
  2. In `buildExistentialJoinSelect` (`cascades_translator.go:4084`), add the `clusterHasBoxLeg(j)`
     branch BEFORE the AXIS-1 block: gather legs, decline (`return nil`) on `mintedBindingLeg` (dup-alias,
     mirroring the wrap `exists_gathered_cluster_wrap.go:71`); translate each `leg.op` (with
     `inInnerCluster=false`) as `NamedForEachQuantifier(NamedCorrelationIdentifier(leg.binding), ref)`,
     `sourceAliases=leg.binding`; predicates = nested ONs + `splitNonExistsPredicates(f.Predicate)` +
     `extractExistsPredicates`; append each `esq` as `NamedExistentialQuantifier` with
     `existsInnerCorrelation`'s joinPred; `return NewSelectExpressionWithAliases(resultValue, quants,
     preds, sourceAliases)`.
  3. Remove the `existentialCount==1` bail (`rule_partition_select.go:67-69`) + add the live-existential
     guard after `:415` (`continue` if any live-set member is existential). KEEP the `>=2` projected
     decline (`:70-86`) as the scoped cell-7 reach-gap; keep `applyExistentialSourceAliases`.
  4. Delete the arm's `≥3` dispatch (`rule_implement_nested_loop_join.go:53-58`) + `implementNWay­JoinWithExistential`
     (~388 lines) **in the SAME commit** — its matcher also matches `[ForEach×N, Existential]`, so on the
     name-model select BOTH it and PartitionSelectRule fire; the arm would re-ordinalize into a competing
     crash. **Torvalds condition A:** grep-prove `planBuriedLegConcat`/`reconstructFoldStep1Seed`/
     `legIsOrdinalSafe`/`collectInnerLegAliases` are still referenced by retained paths (kept); delete
     only `existInnerIsScanSafe`/`nwayOuterProbe` (arm-only).
  *Validate: comma AND explicit projected EXISTS → `[[10 true]]`; `_Discriminating` → `[[7 true],[8
  false]]`; `FourLegJoinDiscriminating`; cells 3/4/8 (WHERE) still green via the wrap; a static unit
  test asserting no yielded lower carries a live existential.*
- **Step 2 — SARG + stress gate (same commit or immediate follow):** `EXPLAIN` asserting each
  decomposed binary join SARGs its inner (index/PK probe, NOT a merged-row `PredicatesFilter` over a
  cross-product) + **1M stress before/after (Torvalds condition C)** — the arm's cross-product was
  O(N³); the SARG'd decomposition must not resurface as a regression (point-lookup <5ms, index-equality
  <10ms thresholds hold) + full-corpus explain-diff against the committed golden (all flips Java-verified).
- **Step 3 (separable follow-on, same PR or later)** — retire `translateExistsOverGatheredCluster`
  (477 lines, the WHERE-EXISTS wrap) once cells 3/4/8 are demonstrably served by the partitioned
  name-model WHERE-EXISTS select. NOT urgent — WHERE rides the wrap, untouched by the arm-retirement.
  Gated by full-corpus explain-diff + preservation of scalar-subquery pre-eval registration.

**Cell 7 (multi projected EXISTS, `SELECT …, EXISTS(…) x, EXISTS(…) y`) — honest scoped decline, NOT
claimed fixed.** Keep Go's `existentialCount>=2` projected decline: it strands cleanly (0AF00, never
wrong rows), backed by the guard. **Java parity is asymmetric (Graefe correction):** for a *same-leg*
sibling correlation Java's `PartitionSelectRule.java:234-243` also rejects, but for a *cross-leg*
correlation (δ1→a, δ2→b) Java falls into its Case-3 merge and *does* attempt the reduction (correctness
unverified). So Go's decline is **conservative**, not symmetric-with-Java; it never ships wrong rows.
Book cell 7's full support (nested, not flat, translation) as a follow-on.

**Why Option A over the alternatives (Graefe's terms):** (B) fix-the-crash-in-the-arm keeps a Go-only
rule matching ≥3 quantifiers that builds a SARG-destroying cross-product Java never emits; (C) extend
the wrap to correlate projected EXISTS reinvents the FlatMap existential path over a positional box
(already tried → wrong rows, correlation dropped). Only (A) restores logical/physical separation (the
join is *explored* as logical sub-selects, SARG/join-order emergent), rides EXISTS as the emergent
`FirstOrDefault`+flag property rather than bolted-on positional plumbing, and deletes the ~388-line arm
(+ the 477-line wrap in Step 4) to converge Go onto Java's exact two-rule architecture.

**Test plan:** the full form×projection×correlation matrix as FDB rows tests (Java-4.12.11.0 semantics),
the buried-leg discriminators (all legs share column `k`; a mis-bind flips value AND boolean), an
explicit hazard-bipartition probe on Graefe's `{a,δ}` shape (correct rows + plan shape shows
`FlatMap`/`FirstOrDefault`, not a merged-row δ projection), a NOT-EXISTS/below-FOD-filtered variant
that proves the *guard* (not the coincidental positive-case fallback) carries correctness, SARG
plan-shape assertions, and full-corpus explain-diff gating Steps 3-4.

### 190.2 (MED) — cost-comparator transitivity

Before 190.2, `planning_cost_model.go` gated five structural rungs on `opsA.inMemorySortCount == 0 &&
opsB.inMemorySortCount == 0` (`:286` typeFilterDepth, `:302` fetchDepth, `:315` distinctDepth,
`:320` unmatchedField, `:337` mapFilter). A conditionally-skipped rung broke transitivity.
**Concrete cycle** (the RED regression): plans A, B both sort-free and tied on
rungs 1–5; A beats B on the gated `typeFilterDepth` rung (`:286`); B beats A on the ungated
`fetchCount` rung (`:306`) — which never evaluates for the A,B pair because `:286` short-circuits.
Introduce C carrying a sort with `fetchCount == fetchB < fetchA`, tied to both on rungs 1–5:
`compare(A,C)` skips `:286`, so fetchCount fires → C<A; `compare(B,C)` ties fetchCount, falls to
`inMemorySortCount` (`:365`) → B<C. Result: **A<B, B<C, C<A** — a cycle. Winner selection is a
linear min-scan (`winner_lookup.go`), so an intransitive relation has no well-defined minimum and
the winner depends on member iteration order — the exact nondeterminism the hash tie-break exists
to kill.

**Fix (scoped to the Go-invented sort-gate class — Graefe condition):** the five per-rung sort
gates are a Go-specific band-aid for "a sorted plan winning a structural rung before criterion #12
(fewer sorts) can drop it" — the real intent is *fewer sorts is high-priority*. Hoist the
`inMemorySortCount` rung ABOVE the structural depth rungs (but below rungs 1–5) — partition sorted
vs sort-free first, then apply the structural rungs uniformly within each class — and drop the five
per-rung sort gates. **This is a real reprioritization, not a safe no-op** (Torvalds): sort-count
moves from rung ~12 (deliberately placed *before* the phantom-prone scalar fallback, `:351-364`) to
rung ~1, so it now dominates the 7 structural rungs; and between two plans that BOTH carry a sort,
the de-gated structural rungs newly apply where they previously abstained. For sort-free pairs
nothing Java-relevant reorders (the gate was already `true`); the sorted-pair cases are the behavior
change. Graefe owns this on merit (ACKed) — every resulting corpus flip is Java-verified via
per-commit explain-diff, none waved through as "obviously safe". **Do NOT touch the `both-index-scans` conditional rung
(`:291`)** — it is Java-faithful (Java `PlanningCostModel.java:208`, the fetch rungs only apply when
both plans are index-based), so it stays for parity even though it is technically also conditional.
The claim is therefore "removes the Go-specific sort-gate intransitivity," **not** "provably total"
(the Java-inherited index-scan conditional is retained by design). Verify no unexplained corpus flip
via explain-diff; Java-verify any flip.
Regression: `TestCostModel_SortGateCycleRegression` — a property test over generated op-profiles
asserting antisymmetry + transitivity, **scoped to the sort-class fix** (the A/B/C sort-gate cycle
above), RED on the pre-190.2 comparator and GREEN on the implementation. The test must not flag a cycle arising solely from the Java-faithful
`:291` index-scan conditional as a Go defect.

**Implementation/review outcome.** Landed in `cebcbd94b`: sort-invariant concrete depth, the five
Go-specific sort gates removed, `inMemorySortCount` promoted before the sort-blind structural block,
and Java's missing primary-scan `unmatchedFieldsCount` branch ported. The regression corpus covers
34 real plans / 35,904 ordered triples, including two sort-bearing cycles and a deliberate comparator
tie; yamsql 338/338, the full
hook/live Java row conformance, and 1M stress are green. The review delta also makes the logical memo
fallback (`expressionDepthRec`) sort-transparent; its dedicated test is RED on `cebcbd94b` and GREEN
with the fold.

The one unexplained golden flip was root-caused rather than waved through. For
`union_aggregate_java#3`, the old `UnorderedUnion` and new Go `Union` candidates tie on scan count,
covering-index count, sort count, residuals, and unmatched fields. The newly ungated fetch-depth rung
decides: old depth 5, new depth 4, because the indexed arm in the new candidate has one fewer
projection. Neither sort transparency nor the new scan unmatched-field arm causes the flip. Go's
`RecordQueryUnionPlan` is not an ordered merge: it has no comparison keys or ordering hint, executes
through `executeUnionStreaming`/`ConcatCursors`, and shares the unordered plan's union cost. The
partial `StreamingAgg` already existed in both goldens. Live Java rejects the exact grouped
aggregate-over-union query as unable to plan; Java's rules and analogous upstream fixtures select
`ImplementUnorderedUnionRule` for a bare logical union. Verdict: a benign wrapper-shallower concat
winner, not redundant ordering, so no comparator special case is warranted.

**Follow-on (taxonomy, outside the 190.2 cost fix):** Go currently has two bare concat UNION ALL
plans/rules: Java-aligned `RecordQueryUnorderedUnionPlan`/`ImplementUnorderedUnionRule` and the
Go-specific `RecordQueryUnionPlan`/`ImplementUnionRule`. Retire or rename the duplicate path in a
separate architecture change; until then it is documented as an extension, never as Java's ordered
union.

**190.2 FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.** The complete `just test`
gate is green (56/56 targets).

### 190.3 (MED, narrow) — partial-PK prefix scan mispriced as a point probe

`plans/cost.go:34` prices a scan as cardinality=1 when `numBound > 0 && allEquality && numBound ==
len(comps)`. But `comps = GetScanComparisons()` is the **bound prefix only** — a composite-PK scan
bound on just the first column has `numBound == len(comps) == 1` and is priced as a 1-row point
probe. The old ctx-aware path rejected a provably partial key only under the strict join-ordering
policy; the metadata-free advisory adapter and the separate logical cardinality helper still
accepted the prefix. Criterion #2 can abstain for two-unbounded comparisons, leaving a
scalar-fallback window where the mispriced prefix beats a genuinely cheaper plan.

**Implemented:** `RecordQueryScanPlan` already carried the full `primaryKeyVals` shape, stamped by
`PrimaryScanRule`, `OrderedPrimaryScanRule`, and `PrimaryScanMatchCandidate.ToScanPlan`, and
preserved by both copy builders and every comparison rewrite. No second metadata field was needed.
`properties.EqualityBoundsCoverKey` is now the shared fail-closed comparison/arity gate. The plan
stamp is authoritative; only an unstamped, exactly-one-record-type scan may fall back to
`PlanContext`. Unknown arity, multi-type ambiguity, a partial prefix, gaps, or ranges never take the
1-row shortcut. The RFC-186 `strictPKGate` advisory/strict split was removed, so direct
`HintCost`, concrete/adapter costing, logical operator counting, and cardinality derivation agree.

RED→GREEN coverage:

- `TestScanHintCost_PartialPKPrefixNotPointProbe`: exact selectivity prices for stamped partial,
  unstamped, and range scans; stamped full equality stays cardinality 1.
- `TestPKFullyEqualityBound`: stamped and ctx-fallback partial/full, stamp-over-conflicting-ctx,
  known-arity empty/range, unknown metadata, and multi-type abstention.
- `TestScanPlanExpressionHintCost_StampedPartialPKPrefixNotPointProbe`: the real
  `PrimaryScanMatchCandidate` → `TypeFilter` → `scanPlanExpression` production path, partial and
  full controls.
- `TestScanProvableMaxCard_UsesStampedPKArity`,
  `TestLogicalCounts_PrimaryKeyContextFallback`, and the existing whole-plan cardinality matrix:
  logical memo descent uses its available context fallback, while both it and `Cardinalities` keep
  composite prefixes unbounded.

Validation: parent-vs-current explain corpus is 2,579/2,579 identical (zero flips/regressions);
post-change 1M FDB stress passed all 23 subtests with correct row counts.

**190.3 FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.** The complete
`just test` gate is green (56/56 targets).

### 190.x-bundled (cheap correctness latents) — closed

- **Finding 5 (scalar signed-zero):** already landed as its own RFC-189 commit `8fcb1a426`.
  `constantValuesEqual` has scalar float32/float64 arms, so Go's native −0.0==+0.0 no longer
  violates the `%v` semantic-hash partition. RFC-190 review found the original raw-bit
  implementation's Java-parity claim was incomplete: Java `Float.floatToIntBits` /
  `Double.doubleToLongBits` canonicalizes every NaN encoding. The scalar arms now compare
  canonical Java bits (signed zeros unequal, all NaNs equal); direct vector-slice carriers retain
  their explicitly raw-bit identity. `TestConstantValue_SignedZeroEqualsIsHashConsistent` covers
  both widths, distinct signed-zero hash buckets, ordinary equality, and distinct-payload/sign NaNs.
- **Finding 8 (merge cycle guard):** already landed as its own RFC-189 commit `eac6ef9ab`.
  `reachable` walks `AllMembers()` and
  `TestMemoMerge_SkipsCyclicMergeThroughFinal` constructs the exact distinct-final-only ancestor
  edge that a `Members()` walk missed, proving the merge is declined before it can create a cycle.

Both focused regressions are green 20×. The stale RFC-190/TODO copies were reconciled here rather
than duplicating already-landed code. Parent-vs-current explain corpus is 2,579/2,579 identical
(zero flips/regressions); full `just test` is green (56/56 targets).

**190.x-bundled FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.**

### 190.4 (Graefe-reruled) — MatchIntermediateRule partial subsumption

**The original MED premise and one-step fix are NAK.** Executable Java does not generally take “a
query Select with fewer quantifiers” and bind candidate extras as compensation. Its
`ComputingMatcher` enumerates dependency-sound, equal-sized subsets from *both* quantifier sets,
then dispatches each partial bijection to expression-specific `subsumedBy`. Candidate omissions
are never compensation. Current `SelectExpression.subsumedBy` requires every query and candidate
`ForEach` to participate; only existential legs may remain unmatched. The old Example-2 prose is
stale where it conflicts with those executable gates.

A faithful eager enumeration has
`sum(C(n,k) * C(m,k) * k!, k=1..min(n,m))` mappings (1,441,728 at 8×8), before child-match and
predicate-mapping products. Go also currently drops the metadata and multiplicity that Java needs:
the structural path does not retain child `PartialMatch`es in `RegularMatchInfo`, and
`AddPartialMatchForCandidate` collapses every result with the same `(queryExpression,
candidateRef)`. The work is therefore split at Graefe's design gate:

#### 190.4a — guarded dead candidate-existential reach

Extend the existing single-source index matcher only when there is exactly one candidate
`ForEach` to pair with the query `ForEach` and every other candidate quantifier is an existential
that is provably dead: its alias is absent from the candidate result, every candidate predicate,
and the selected leg's dependency set. Reject outer/full join semantics, unmatched candidate
`ForEach` legs, physical legs, edge incompatibility, and every unmapped non-tautological candidate
predicate. Retain the selected child match in `RegularMatchInfo`.

Regression coverage uses an index-like multi-quantifier candidate with the dead existential in
both leading and trailing positions and proves the placeholder equality binding plus usable
compensation. Negative twins cover an extra `ForEach`, filtering and result-producing
existentials, a selected leg dependent on the skipped existential, an unmapped candidate filter,
and outer query/candidate Selects. This is a bounded safe reach improvement, **not** the full Java
partial matcher.

Validation: the index-like multi-quantifier regression was genuinely RED before implementation and
is green 20×; the Cascades package and full `just test` gate are green (56/56 targets).
Parent-vs-current explain corpus is byte-identical (2,579/2,579; zero flips). **190.4a FINAL
REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.**

#### 190.4b — matcher metadata, enumeration, and identity infrastructure

Port dependency-convex/topological partial-bijection enumeration; branch over all compatible child
matches; atomically merge alias/parameter maps; retain every selected child match; and implement
the relevant `RegularMatchInfo.tryMerge` semantics for ordering, grouping, roll-up, and
constraints. Replace pair-only partial-match dedup with a stable semantic fingerprint so distinct
subset mappings survive while exact refires still collapse. Use deterministic full/exact-first
search with per-attempt limits of 40,320 visited search states and 64 unique emitted partial
matches; exhaustion is a safe optimization miss, never a relaxed or positional match.

The merge path also ports constant-aware recursive group-value pull-up (nested constructors,
field-prefix/suffix compensation, and ambiguity rejection) and requires singleton candidate
references wherever result-dependent metadata is adjusted. Focused regressions are green 20×;
the affected uncached Bazel targets are 3/3 green; full `just test` is 56/56.
**190.4b FINAL REVIEW: Graefe ACK + Torvalds ACK + independent Codex ACK.**

#### 190.4c — Select predicate/result/compensation semantics

Port current Java's Select-specific semantic gate: complete `ForEach` coverage on both sides;
dependency-safe unmatched query existentials; the owning existential-predicate rule for matched
query existentials (including existential-to-`ForEach` pairings); conflict-safe predicate
implication/placeholder bindings and candidate-predicate coverage; composed child pull-ups;
result-value coverage; and correct possible/impossible compensation state. Close with
multi-match-dedup, dependency, child-conflict, and end-to-end empty/duplicate cardinality tests.

**Implemented.** The staged Select port now requires complete `ForEach` coverage on both sides,
checks unmatched existential dependencies and owning predicates (including existential-to-`ForEach`
pairings), merges predicate/parameter/child metadata without conflicts, pulls result Values through
the composed children, and distinguishes possible, impossible, and not-needed compensation.
Existential-to-`ForEach` cardinality repair is a required primary-key `Unique` over the exact pinned
physical access, and the executor carries its dedup state in continuations.

The production reach proof is a correlated primary array source:
`EXISTS (SELECT 1 FROM R.TAGS E WHERE E >= 8)`. Metadata carries the record-layer root AST into a
Java-shaped Field/Then/Nesting fan-out candidate graph with `Explode`; leaf matching enumerates the
outer correlation; the SQL scope preserves the explicit element alias and lowers its primitive
element to the whole Explode QOV. The exact winning plan is
`Project([ID#0], UnorderedPrimaryKeyDistinct(Fetch(IndexScan(T_TAGS, [<>]))))` — no base
`Scan`, residual `Filter`, or execution-time `Explode` fallback.

The review lap closed every fail-open boundary it found. Fan-out elements may bind scan ranges but
cannot cover their source arrays; whole-record Fetch is retained; duplicate-producing positions do
not claim flat/PK ordering; raw ordered-scan, streaming-aggregate, correlated-EXISTS, count-covering,
and unique-DISTINCT shortcuts require the relevant cardinality/semantic proof. Unknown metadata,
duplicate signals without a structural FAN_OUT root, unsupported or contradictory function roots,
scalar nesting (including scalar nesting beside a fan-out sibling), and unsupported nullable-wrapper
shapes all decline. Function keys cannot masquerade as bare fields for coverage or plan ordering;
only their independently stored, non-colliding PK suffix remains coverable.

The real FDB regression discriminates empty arrays, nonmatches, and two distinct matching fan-out
keys for one primary key; unpaged and page-size-1 execution return IDs 2/3/4 exactly once. Every page
uses a fresh FDB transaction and serialized continuation. A separate `COUNT(*)` proves the fan-out
index is not used as a base-record cardinality substitute and returns all five records. Focused
fan-out/function/continuation tests are race-clean and green 10×; all affected Go and Bazel targets
are green; parent-vs-current explain corpus is 2,579/2,579 identical (zero flips/regressions); full
`just test` is 56/56.

**190.4c FINAL REVIEW: three independent Codex audits ACK** (candidate/Java-shape parity,
shortcut/cardinality safety, and SQL→Cascades→real-FDB semantics). **190.4 is complete.**

### 190.5 (MED) — index-intersection reach

`intersector_primary_key.go:94` caps k-way at 3 (`planner.go:662,709` caps ≤4 candidates/≤8
matches); PK-only comparison key; `plans/intersection.go:90` carries no reverse flag → no
reverse-ordered (`DESC`) intersections. Java (`AbstractDataAccessRule.java:434-524`): unbounded
k-way via `ChooseK` + ordering-compat sieve, any common ordering, `isReversed`. **Fix:** lift the
k-way cap to Java's ChooseK enumeration (bounded by candidate count, not a magic 3); carry the
comparison-key direction + reverse flag into `RecordQueryIntersectionPlan`. Graefe-gated.
Regression: a 4-way intersection + a `DESC` intersection plan-shape test.

#### 190.5a — bounded generic k-way reach

The magic 3-way loop is gone. The forward-PK-safe intersector now enumerates `ChooseK` subsets
from 2 through the four restricted candidates, records structurally incompatible pairs in a sieve,
and stops when no size-k subset can share the current merge contract. The eight-match cap is applied
after adjusted/raw twins collapse; the four-candidate cap counts only useful restricted candidates,
so the unrestricted primary scan does not hide a four-secondary-index intersection. Re-entry is
keyed by the exact maximum-coverage match set plus requested orderings, allowing later and
same-cardinality adjusted matches without rebuilding an unchanged input.

Unlike Java, this slice deliberately retains all viable bounded subsets. Java evicts a pair/triple
only after `isPartitionRedundant` proves that the larger intersection adds useful filtering; Go
does not yet carry that proof, so unconditional maximal pruning could replace a useful pair with a
redundant extra scan. Production SQL and real FDB regressions prove one selected four-leg
intersection and exact rows against four three-of-four decoys. The comparison key remains
forward-only primary key in 190.5a. Focused planner tests are race-clean and repeat-green; all
three affected Bazel targets pass; parent-vs-current EXPLAIN is 2,579/2,579 byte-identical; the
1M FDB stress comparison passes all 23 subtests with identical row counts and 151.32s vs 151.71s
total runtime; full `just test` is 56/56.

#### 190.5b — rich directional comparison-key parity

The intersector now follows Java 4.12.11's common-ordering path instead of requiring a hard-coded
forward primary-key sequence. Each match carries its real top-to-top translation map and translated
requested orderings. Intersection merge combines equality bindings, normalizes dependencies through
newly fixed values, enumerates comparison keys from the resulting partial order, and requires every
non-fixed common-primary-key component. Structural-PK and baked-layout checks fail closed before a
plan is admitted.

The semantic comparison ordering is preserved through plan construction, copies, and provided
ordering; its executable comparison values plus `reverse` participate in identity/hash and executor
cursor creation, while hint contracts and fetch/set-operation rewrites retain the same contract.
Fan-out candidates receive Java's per-leg unordered primary-key distinct wrapper before the ordered
intersection. Redundancy uses proof-grade maximum cardinality and unmatched-ID metadata; useful
larger intersections evict immediate subpartitions only when they produce a replacement expression,
while impossible compensation retains the usable smaller plan. The logical
`ImplementIntersectionRule` uses the corresponding forward/ascending ordering gate.

The executable boundary is deliberate: Go currently evaluates only natural flat field keys, so an
all-ASC key runs forward and an all-DESC key runs in reverse. Mixed/counterflow null directions,
`ToOrderedBytesValue`, non-flat keys, ambiguous same-name layouts, and stale baked ordinals decline
as optimization misses. If any leg exposes structural common-primary-key Values, every leg must;
mixed structural/name-only providers conservatively decline rather than equate uncertain layouts.
Multi-record-type legs remain the separately documented layout gap. `PartialMatch.prepareForUnification`
parity is also outside this ordering slice.

Production SQL and real FDB pin exact descending rows, tie order, decoys, serialized continuation,
and a low scan budget for the reverse intersection. Focused tests are race-clean and repeat-green;
all affected Go and Bazel targets pass. The 2,579-entry parent-vs-current EXPLAIN census has 2,573
identical plans and six reviewed Java-faithful winners: five reverse-index sort eliminations and one
ordered composite-index streaming aggregation, with zero plan regressions or recoveries. Their
three row corpora pass all 58 cases, and the checked-in plan-shape golden records the six changes.
The uncached 1M FDB gate passes all 23 subtests in 150.95s vs the 190.5a checkpoint's 151.71s; full
`just test` is 56/56.

One safe architectural residue remains: Go's explicit cross-candidate pass runs after candidate-local
single accesses have already been yielded, so its private intersection-info map cannot evict those
singleton alternatives as Java's shared map does. This only widens the legal plan space; a future
partition-level assembler must build singles and intersections once, share the eviction map, and
flatten survivors before yielding. Post-hoc memo deletion is not safe. This is recorded in
`DIVERGENCES.md` and does not hold 190.5 open.

**190.5 is complete. FINAL REVIEW: two independent Codex audits ACK.**

### 190.6 (architectural — Graefe ruling) — NLJ ordering-obliviousness (localized, not systemic)

Investigation corrected the premise: the join/set-op rules are **not** systemically
ordering-oblivious. `ImplementDistinctUnionRule` (`:67,93`), `ImplementInJoinRule` (`:102`),
`ImplementInUnionRule` (`:233`) and the unary Filter/Projection/Limit rules all consult
`GetRequestedOrderings()` / pick the ordered child via `getWinnerForOrdering(ordering)`. The gap is
**one rule**: `ImplementNestedLoopJoinRule` is the lone ordering-sensitive Cascades rule that never
calls `GetRequestedOrderings()` — it picks both legs by cost only (`:98-107`, `:697,707`
`PreserveOrdering`). It ports none of Java's `ImplementNestedLoopJoinRule.java:180-217` Case 1/2a/2b
outer-ordering enumeration.

Two disjoint input sets, two conclusions:
- **(A) `ORDER BY` with no satisfying index anywhere** — Java's Cascades declines (`RemoveSortRule.java:108`
  yields nothing → `UnableToPlan`; `RecordQuerySortPlan` exists but is **legacy-planner-only**,
  `RecordQueryPlanner.java:315` — the *only* construction site). Go's `ImplementInMemorySortRule`
  (RFC-001) produces correct sorted rows. This is a clean sanctioned read-side SUPERSET — keep it.
  **Documentation corrected:** Java has a physical sort class, but its *Cascades* planner never
  constructs it; Go's in-memory sort remains a sanctioned fallback, not a replacement for
  Java-faithful ordered enumeration in rules that can satisfy the request without a sort.
- **(B) `JOIN … ORDER BY <outer-indexed-col>` where the ordered index is not the cheapest outer scan**
  — BOTH engines handle it. Java Case 2a picks the outer partition that *satisfies* the ordering
  (even if pricier) → sort-free streamed `FlatMap`. Go picks the cheapest outer → `ImplementSortRule`
  finds no satisfying partition → declines → in-memory sort wins by default → materialized O(n log n)
  sort. Same rows, strictly worse plan. The physical sort **masks** this (correct rows ship green) —
  the exact latent-quality failure mode.

**Implementation:** the NLJ rule is now
`RequestedOrdering`-sensitive and the bottom-up sort boundary can construct the same variants after
both child groups finish. Both sites use Java 4.12.11's **source-order partition** matrix, not a
single global best-child shortcut:

- **Case 1 (max-one outer):** roll all qualifying outers together; retain the cheapest satisfying
  inner overall, or the cheapest inner in every satisfying source-order partition for an
  exhaustive request.
- **Case 2a (outer satisfies alone):** roll satisfying outers together unless the request is
  `DISTINCT`, in which case retain the cheapest outer from every satisfying source-order
  partition. Exhaustiveness deliberately does not widen this case. Pair with the cheapest eligible
  inner.
- **Case 2b (distinct outer + inner):** retain every distinct-outer source-order partition that
  does not satisfy alone and whose concatenation with the inner can satisfy. Pair each outer with
  the cheapest satisfying inner overall, or every satisfying inner source-order partition for an
  exhaustive request.

The ordinary cost-best FlatMap remains available. Each ordered variant freezes **both** chosen legs
in private exact-final singleton references; the FlatMap rich-ordering property reports Case
1/2a/2b order only for those exact edges, preventing extraction from silently relinking an ordered
leg to the shared group's cheaper unordered winner after the sort disappears. Compensation,
FirstOrDefault, and DefaultOnEmpty chains are rebuilt around the selected source legs instead of
swapping a buried leaf.

The value translation needed to make the property sound is also in this slice. Ordering pull-up/
push-down preserves partial-order dependencies and translatable fixed-comparison bindings. The
safe value-root bridge accepts only source-local flat/baked fields versus a single QOV root with the
same complete accessor path; it refuses ordinal collisions, nested-path ambiguity, and unrelated
roots. For projected EXISTS only, `inheritOuterRecordProperties` enables an ordering-only
record-constructor lens that qualifies direct correlation-free outer fields. It neither changes the
executable result value nor attributes inner/literal fields (or fields of an ordinary FlatMap) to
the outer.

Ordered-leg discovery first uses retained physical alternatives, then reuses the existing ordered
primary/index-scan rules and data-access partial matches in private candidate space. A pruned
unbounded forward primary scan may safely recover its reverse alternative; bounded scans decline
rather than synthesize changed scan semantics. Every assembled candidate is checked again against
the final rich ordering. If no legal access path exists—or value ownership cannot be proved—Go's
sanctioned `RecordQueryInMemorySortPlan` remains the correct fallback.

The paired yamsql regression adds an indexed `JOIN … ORDER BY <outer-indexed-col>` that must not
contain `InMemorySort` and an index-less twin that must contain it. Projected-EXISTS ASC/DESC/LIMIT/
NOT-EXISTS row tests exercise the same translation and recovery seam against real FDB.

**Final parent-to-worktree EXPLAIN census:** old=2,579, new=2,581, identical=2,491,
differing=90. The two extra entries are the new queries. Among the 88 comparable shape flips, 87
remove outer/unary sort enforcers; one already-sorted fixture changes shape because its fixture now
declares the new index. There are no plan-error regressions or recoveries. The checked-in golden is
byte-identical to the reviewed after-corpus.

Focused ordering/property/enumeration tests pass 20×. The Cascades subtree, yamsql scenario, and
projected-EXISTS/round4/round5 real-FDB targets are race-clean. Yamsql passes 5/5; real FDB pins
exact ASC, DESC, LIMIT, and NOT-EXISTS rows, reverse scans, and the unindexed in-memory-sort
fallback. `just generate`, generated feature/SQL ledgers, `just lint`, and the full `just test`
suite pass (56/56).

The uncached 1M FDB stress gate passes all 23 subtests with exact row counts. Its planner-query
timings are 3.393s (`order_by_pk_full`), 3.300s (`scan_all_narrow`), 3.480s
(`scan_all_wide`), and 3.060s (`sparse`), versus 3.36s/3.22s/3.36s/2.93s at the prior checkpoint
(about 1–4%, within observed run noise); the join query is 17ms. Total runtime is 162.58s versus
150.95s, with the delta dominated by bulk insertion rather than the planner queries.

**190.6 is complete. FINAL REVIEW: two independent Codex audits ACK.** Their actionable findings
were closed before the final gates: mismatched baked ordinals now fail closed across the QOV/
source-local root bridge, and regressions pin all complementary Case 1/2a/2b modes, key-map
collision, ordinal mismatch, and ambiguous two-QOV roots.

### 190.7 (MED-HIGH) — de-duplicate the existential-join family

The audit's original four-copy census was stale after 190.1 retired the N-way arm. Three source
sites remained, reached by four behavioral routes: the direct existential-select fallback, the
join-fold arm, and the primary-key and secondary-index fast paths through `yieldExistsFlatMap`.
All three repeated below-FOD filter → `FirstOrDefault(NULL)` → optional `QOV(inner) IS [NOT] NULL`
chains now use `buildExistsCompensationChain`.

The helper makes the load-bearing alias contract explicit. The direct fallback advances with a
fresh physical bookkeeping alias at every wrapper, preventing memo interning from collapsing two
filters; completed correlated paths preserve the real inner correlation so the bound alias can be
subtracted. Callers still own their different base memo group, concrete correlated plan, outer-leg
rebasing, FlatMap construction, and PK-return-false versus index-continue decline behavior, so the
extraction does not blur their control flow or memo boundaries.

`buildExistsFlatMap` was removed. Its primary-key scan construction was folded into
`tryExistsFlatMap`, while comparison-range construction and inner/outer residual classification are
shared with the secondary-index route. `buildCorrelatedFlatMapPlan` intentionally remains separate:
it uses ForEach quantifiers, strict FirstOrDefault and DefaultOnEmpty variants, compensation, and
different predicate-ordering semantics.

The focused alias-contract regression pins fresh and preserved chains, positive and negative
residual polarity, exact QOV correlation, predicate retention, and the zero-below-FOD projected
route; it passes 20×. The affected Cascades subtree, 26/26 yamsql cases, and targeted real-FDB
direct/join/PK/index routes pass, including race coverage. The before/after plan corpus is
byte-identical: old=2,581, new=2,581, identical=2,581, differing=0. `just generate`, `just lint`,
and the full `just test` suite pass (56/56).

**190.7 is complete. FINAL REVIEW: two independent Codex audits ACK.** The only review gap—direct
coverage of a nil below-FOD predicate set plus predicate/QOV identity assertions—was closed before
the final gates.

### 190.8 (LOW) — scalar-function semantics: one source of truth

The original tri-layer diagnosis was stale after RFC-181 deleted the INSERT evaluator. Plan-time
constant folding (`EvaluateConstant`), INSERT VALUES, and ordinary SELECT runtime all already call
`ScalarFunctionValue.Evaluate`, so rounding, `ABS(MinInt64)`, and LENGTH-on-bytes cannot diverge by
phase. `embedded/scalar_functions.go` is now only the INFORMATION_SCHEMA map interpreter and must
remain semantically separate: its Java-compatibility surface, time carriers/parsing, arity handling,
comparison domain, and SQLSTATE mapping intentionally differ.

The live drift risk was three independent name switches: values evaluator dispatch and Cascades
admission, the expression walker's result typing, and the map interpreter's smaller name gate. A
values-owned catalog now binds each of the 56 spellings to a canonical operator, result-type
strategy, and independent route capabilities:

- 53 Cascades-safe spellings;
- 49 generic `ScalarFunctionCall` spellings (including internal NULLIF/IF/IIF, which remain unsafe);
- 12 generic legacy-map spellings (COALESCE, GREATEST/LEAST, and nine date parts);
- dedicated BIT* and CURRENT_* entries that remain outside the generic-call grammar route.

Aliases share operator and result strategy. `evalScalarFunction`, lazy `ScalarFunctionValue`
dispatch, generic result inference, dedicated BIT*/CURRENT_* typing, and the legacy-map admission
switch all consult the catalog. The map path switches on a catalogued compatibility operator but
keeps its local bodies, avoiding a semantic widening.

The consolidation exposed a real P1 carrier/type contradiction. `COALESCE(3, 1.5)` and
`GREATEST(3, 1.5)` were statically DOUBLE but returned `int64(3)`; FLOOR/POWER could do the same.
Downstream arithmetic then used runtime-carrier dispatch, making `/ 2` silently return integer `1`
instead of `1.5`. The same invariant affected mixed CASE (`PickValue`) and `PromoteValue`. Numeric
results now conform to the declared FLOAT/DOUBLE carrier, LONG→FLOAT converts directly (no
int64→float64→float32 double rounding), constant COALESCE simplification preserves the common
carrier, and `ArithmeticValue` selects its DOUBLE lane from static types as Java does.

The same audit found an independent ROUND fast-path error: integral inputs returned before reading
negative or NULL precision. Exact integer rounding now works without conversion through float64,
including negative ties and overflow detection at the int64 boundaries. Floating ROUND clamps
precision to the supported SQL window and preserves a large finite value when temporary decimal
scaling would otherwise overflow.

The catalog tests snapshot all four capability sets, every operator's evaluator reachability,
aliases, result strategies, route boundaries, and legacy-map membership. Unit regressions cover
lazy/runtime/constant-fold carrier agreement, FLOAT single rounding, Pick/Promote, and static DOUBLE
arithmetic. `numeric_functions.yaml` passes 47/47 with constant and row-dependent COALESCE,
GREATEST/LEAST, FLOOR, POWER, MOD, mixed CASE division, and ROUND edge witnesses. The existing plan
corpus is byte-identical (2,581/2,581); the golden adds only the nineteen new queries.

Race validation exposed an adjacent pre-existing parser defect: ANTLR's generated lexer/parser
constructors shared mutable package-global DFA and prediction-context state. Each public parse route
now leases an exclusive warmed state bundle from a bounded pool, then detaches the returned read-only
tree before returning the lease. Concurrent valid/error parsing and retained-tree traversal are
race-pinned; the full plan corpus remains byte-identical with no measurable steady-state parse
regression.

`just generate`, `just lint`, and full `just test` pass (56/56).

**190.8 is complete. FINAL REVIEW: two independent Codex audits ACK.**

### 190.9 (LOW) — NLJ cursor twin inner-loops

The hash-probe and linear-scan paths carried near-identical inner-loop bodies, so any future change
to predicate evaluation, ordinal binding, match bookkeeping, positional output, or join-type
handling could silently land in only one path.

Both paths now feed one candidate-position loop through an explicit view:

- ordinary linear scans and failed hash probes use `(nil, len(innerRows))`, where nil means identity
  mapping from candidate position to physical inner index;
- a successful hash probe uses `(bucket, len(bucket))`;
- a hash miss and the pre-existing nil-outer-row decline remain `(nil, 0)`.

This corrects the RFC's initial suggestion to reuse `allInnerIndices()`: materializing an identity
slice would add unbudgeted O(inner rows) memory to every linear join. The nil-backed identity view
adds no allocation and also deletes the old failed-probe-only identity allocation. The single loop
retains the original operation order. `lastEmittedInnerIndex` and `repositionInner` now map through
the same view, preserving bucket-relative hash continuations and row-relative linear/degraded
continuations. FULL OUTER still marks the actual physical inner index before its unmatched-inner
drain.

New regressions pin sparse hash buckets and every continuation split, PK-based repositioning after a
new earlier match shifts a bucket, a positive time.Time-to-string failed probe whose matches sit at
dispersed physical indices, a hash miss with null padding, and FULL OUTER hash matches/drain
ordering. The executor is race-clean and 20× repeat-green, the large-inner real-FDB FULL OUTER route
passes, and the complete 2,600-entry plan golden is byte-identical.

`just generate`, `just lint`, and full `just test` pass (56/56).

**190.9 is complete. FINAL REVIEW: two independent Codex audits ACK.**

### 190.10 (MED) — behavioral tests for the 12 untested rules

The original 113/125 result was a text/reference census, not an executable direct-coverage proof:
eight rules had no references, four were exercised only indirectly, and subsequent tests made part
of that snapshot stale. The implemented gate derives the exact 125-rule universe from every
production rule set and parses the package's test sources. A rule is covered only when its canonical
constructor is called from a function Go itself can discover as a test. Comments, helper-only calls,
bare identifiers/types, malformed test signatures, unrelated selectors, and locally shadowed
constructors cannot satisfy it; correctly imported external-package tests can.

Thirteen isolated positive tests close every missing or ambiguous direct-coverage case:
`SplitSelectExtractIndependentQuantifiersRule`, `OrderedPrimaryScanRule`,
`PushRequestedOrderingThrough{Select,SelectExistential,InLikeSelect,RecursiveUnion}Rule`,
`PushInJoinThroughFetchRule`, `PushUnorderedUnionThroughFetchRule`, `RemoveProjectionRule`,
`SinkLimitIntoVectorScanRule`, `ImplementLimitRule`, `ImplementInMemorySortRule`, and
`FinalizeExpressionsRule`. They assert the actual yielded plan or constraint, including alias and
correlation translation, static-values versus runtime-parameter IN metadata, exact union/fetch
child relinking, projection identity, vector limit sinking, and final-member promotion. Existing
direct coverage for `MergeProjectionAndFetchRule` is retained and now enforced by the same gate.

The completeness gate was red-verified by replacing the ordered-primary constructor call with an
opaque lookup: it failed with exactly `OrderedPrimaryScanRule`, then returned green when restored.
Focused tests, race validation, and repeated execution pass; the complete 2,600-entry plan golden is
byte-identical. `just generate`, `just lint`, and full `just test` pass (56/56).

**190.10 is complete. FINAL REVIEW: two independent Codex audits ACK.**

### 190.11 (MED) — cost-model test suite

The original “3 tests + 1 fuzz” census was stale: `planning_cost_model_test.go` alone already had 25
tests, and `FuzzCostSanity` checks scalar-cost finiteness rather than
`PlanningCostModelLess`. A source audit enumerated 22 ordered PLANNING decision slots (20 conceptual
criteria, with total-fetch count, fetch depth, and explicit-Fetch count separately decisive). Fifteen
slots already had full-comparator winner coverage; seven did not.

Seventeen focused tests now close every missing winner rung:

- residual predicates before data-access count; data-access count before the Go sort extension; sort
  before type-filter count; and type-filter count before depth;
- recursive DFS before sort, plus a same-shape FlatMap winner that reverses when the two input-table
  statistics are swapped;
- the root-IN penalty before later tiers, a separately isolated nested-InJoin count, the configured
  primary/index preference, and the strict-SARG-superset branch;
- total fetches before fetch depth, explicit Fetch count before unmatched fields, NLJ top-level
  predicate count, DefaultOnEmpty count, and the scalar-cost fallback; and
- the two previously helper-only REWRITING decisions: residual-conjunct count and predicate depth.

Late-rung fixtures are intentionally adversarial: where applicable, scalar cost or the final stable
hash prefers the loser, so removing the asserted rung turns the test red instead of accidentally
passing at fallback. The existing sort-cycle regression is strengthened into a scoped strict-weak-
ordering gate over 34 homogeneous plans: irreflexivity, antisymmetry, totality up to stable identity,
35,904 ordered triples, tie substitutability, and permutation-independent minimum selection across
all 68 cyclic forward/reverse orders. The scope is deliberate. Java's heterogeneous root-IN
`flipFlop` short circuit can report `+1` in both orientations for a SARGed InJoin(index) versus an
unsarged InJoin(primary); 190.11 does not make a false global antisymmetry claim.

The audit also surfaced a separate production defect: `firstMemberCostMemoised` walks only
exploratory `Reference.Members()` even though `Reference.Get()` falls back to final members. A
one-line probe changed six golden plans—two filter/aggregate shapes and four IN shapes, the latter
exposing likely `InUnion` underpricing. That experiment was reverted from this test-only milestone
and booked as 190.11-FU for Java tracing, direct regression coverage, and per-flip review.

Focused tests, race validation, and repeated execution pass; the full 2,600-entry plan golden remains
byte-identical. `just generate`, `just lint`, and full `just test` pass (56/56).

**190.11 is complete. FINAL REVIEW: two independent Codex audits ACK.**

### 190.11-FU (MED) — finals-only child cost and InUnion fanout

The booked production follow-up is complete. `firstMemberCostMemoised` now selects through
`Reference.Get()`: an exploratory member still wins when present, while a finals-only pinned child
falls back to its first final member instead of receiving the defensive unknown-subtree cost.
`BestRefCostWith` and the recursive `BestMemberCostWith` walk now honor their separate “best”
contracts across `AllMembers()`. This restores Go's existing reference-selection contract; it is
not a literal port of Java's singleton-only `Reference.get()`.

The raw one-line lookup probe was not safe to bless by itself. Four of its six golden flips selected
an `InUnion` that repeatedly executed a full scan because the operator priced the number of binding
dimensions, not the number of executions, and charged the child CPU only once. The repaired model
uses the Cartesian product of literal source sizes, substitutes ten for each runtime-unknown
dimension while preserving known factors, repeats child CPU per execution, and saturates overflow.
Exact fanout one returns the child cost unchanged because execution bypasses the union wrapper;
exact zero returns zero cost and the executor now returns an empty cursor without executing the
child. An absent outer source slice remains the existing pass-through constructor shape. A known
empty dimension dominates an unknown one in either order, which is a sound Go precision extension
over Java's generic unknown-times-zero cardinality treatment because the Go executor enforces that
short circuit.

Direct regressions pin finals-only, exploratory-plus-final, multiple-final, and recursive best-cost
selection; exact, unknown, mixed, zero, and one-combination fanout; cardinality propagation; the
known-empty executor path; and a full-comparator case in which one filtered full scan must beat two
`InUnion` executions. The 2,600-query explain-diff contains exactly two changes, both moving a
key-only HAVING predicate below `StreamingAgg` and its sort. The four repeated-full-scan IN shapes
from the unsafe probe no longer flip. Go deliberately supports this key-only GroupBy pushdown, so
the two improvements are audited Go behavior rather than claimed Java parity.

Focused affected-package tests, race validation, and 20× repeated regressions pass. `just generate`,
`just lint`, `just build`, and full `just test` pass (56/56).

**190.11-FU is complete. FINAL REVIEW: two independent Codex audits ACK.**

### 190.12 (MED) — committed plan-shape golden

`explaindiff` renders every corpus query's plan but its baselines are explicitly NOT committed
(`explaindiff_test.go:166`), so cross-commit plan drift is caught only by manual before/after.
**Fix:** commit the rendered baseline as a checked-in golden + a CI test that diffs against it and
fails on any un-blessed plan change (re-bless intentionally, like a snapshot test). This is the
standing net that would have caught every silent plan-quality regression this audit found.

**190.12 is complete.** The golden landed as the first implementation commit and remained the
per-commit drift gate for every subsequent RFC-190 change.

### 190.13 (LOW) — doc rot

Fix stale comments that actively mislead: `plandiff.go:10`/`runsql.go:84` "Java engine is stubbed"
(it's LIVE in CI); `abstract_data_access_rule.go:~29` "no containment pruning yet" (it DOES prune
via `findContainingAccess`); `plans/distinct.go:38` + 3 sites "cross-page-buggy" (fixed 2026-07-20,
TODO C5). No code change — comments only; but they send the next reader down phantom paths.

**190.13 is complete.** Final whole-RFC audit also removed the accidentally tracked terminal log
and corrected the two surviving pre-190.1 comments that still described the retired N-way arm.

### 190.14 (LOW) — cost-model diagnostics + library stderr

`countConcreteNode`/`concreteResidualPredicates` are plain type-switches with no unpriced-type
detector (unlike the cost path's `warnUnpricedPlanType`) → a future plan type is silently uncounted
for criterion #3 with zero diagnostic. And `warnUnpricedPlanType` writes to `os.Stderr` from
library code (`planning_cost_model.go:1452`). **Fix:** add an unpriced-type fallthrough warning to
both walks; route the warning through an injected logger, not `os.Stderr`.

The completed fix uses one exhaustive `classifyConcretePlan` taxonomy for all 41 production plan
types. Every type takes an explicit position on both operator-count contribution and residual-CNF
contribution. The concrete and logical count walks dispatch from that taxonomy but retain their
pre-existing policy differences: the logical fallback still uses plan-local index metadata,
descends through a PK filter, counts each multi-intersection aggregate leg, and treats text access
as neutral. This separation is deliberate—reusing the concrete contribution wholesale would have
activated new metadata and folding criteria on logical candidates and changed winners in a
diagnostics milestone. Unknown types warn in both paths. Explicit default arms also warn if a future
taxonomy enum is added without either a concrete/logical count policy or residual policy, closing
the subtler “classified but still silently neutral” failure mode. Folded concrete count subtrees
receive a diagnostics-only classification pass so a future child beneath MultiIntersection cannot
hide behind its intentional one-access fold.

`WithCostModelDiagnostics(ctx, logger)` is the opt-in injection point. It returns an outermost
`PlanContext` decorator carrying the logger and a wrapper-scoped `sync.Map`; warnings deduplicate
once per stable `(walk, reflect.Type)` key under concurrent comparisons. Walk values are structured
snake-case tokens (`operator_counts`, `residual_predicates`, `join_ordering_cost`). The logger's
WARN gate is checked before the dedupe key is consumed, nil explicitly masks an inner sink, and
messages contain only walk, type, and fallback metadata—never `Explain`, query literals, or schema
text. There is no default logger and no `os.Stderr` write. Reusing a wrapper shares its dedupe
scope; constructing another wrapper creates an independent scope.

Planner plumbing preserves behavior as well as observability. `NewPlanner`,
`ExpressionRuleCall.CostModel`, and `ImplementationRuleCall.CostModel` retain only the diagnostic
sink over `EmptyPlanContext` while statistics are nil, exactly matching their historical
no-context comparator. Once statistics are supplied, the real wrapped context is used as before.
Tests prove both the unchanged primary-vs-index verdict and actual unknown-plan warning delivery
through all three paths.

Twelve parallel regressions cover concurrent once-only emission across all three walks, independent
wrapper scopes, nil masking, dynamically disabled WARN levels, known-neutral silence, concrete and
logical fallback detection, folded children, invalid future count/residual kinds, comparator
plumbing, the logical-policy compatibility boundaries, and the exact 41-type taxonomy. Focused,
race, and 20× repeated tests pass. The checked-in 2,600-query plan-shape golden is byte-identical;
`just generate`, `just lint`, `just build`, and full `just test` pass (56/56).

**190.14 is complete. FINAL REVIEW: adversarial Codex audit ACK; second independent audit ACK.**

## Commit sequencing & the one-PR decision

Owner directive: **one branch, one PR** for the whole audit. Torvalds's NAK concern — "one
explain-diff over the whole PR shows flips from many causes, un-bisectable" — is valid and is
resolved by **commit discipline within the single PR**, not by splitting it:

1. **190.12 (plan-shape golden) lands as the FIRST commit.** It is the standing net every later
   commit is measured against — and the safety net for the 190.7/190.9 refactors (whose "behavior
   preserving" claim is otherwise only manual before/after).
2. Then the **zero-intended-flip** group (190.13 docs, 190.10/190.11 tests, 190.7/190.8/190.9
   refactors) — each commit must show the golden UNCHANGED. Any flip here is a bug in the "refactor".
3. Then **correctness** (190.1, 190.2, 190.3), one logical change per commit. Bundled findings 5/8
   already had discrete RFC-189 commits (`8fcb1a426`, `eac6ef9ab`); RFC-190 only reconciles their
   stale ledger entries and folds the signed-zero review delta.
4. Then the **Graefe-gated reach** items (190.4a/190.4b/190.4c, 190.5, 190.6), one staged
   logical change per commit.

**Per-flip gate at COMMIT granularity, not PR granularity:** every plan-affecting commit is
explain-diff'd against its OWN PARENT (not the whole PR vs master), so each flip is attributable to
exactly one change and Java-verified before the next commit. `git bisect` works at commit level, so
one PR with this ordering is as attributable as three PRs. If the owner later prefers a physical
split, the commit boundaries above are already the cut lines. (Surface this reconciliation to the
owner; proceed with one PR unless they direct otherwise.)

## Performance

No item regresses steady-state planning. 190.2/190.3 change cost *ordering* (not cost of
computing it) — validated by explain-diff + the 1M stress test (row counts + latencies unchanged
except where a flip is Java-verified). 190.7/190.9 are behavior-preserving refactors. 190.8 replaces
name switches with constant-time catalog lookups and corrects numeric carrier selection without a
plan-shape change. Its bounded parser-state free lists retain at most eight warmed lexer/parser
bundles; checkout/return alone takes the mutex, parsing remains parallel, retained heap under the
48-worker probe fell from 67.5 MB to 18.9 MB, and the full corpus time stayed at the shared-global
baseline. 190.9 replaces two branches with one inlined nil-backed candidate view and removes the
old degraded-probe identity-slice allocation. 190.12 adds a CI test, no runtime cost. 190.5 enlarges
intersection enumeration under the existing candidate cap.
190.6 retains one cheapest exact expression per Java-required source-order partition instead of
forming the child cross-product; its final EXPLAIN census shows only the intended sort
eliminations, and the 1M stress gate passes all 23 subtests with exact row counts.

## Test plan

Every item lands with a regression test that is RED before the fix (correctness items) or pins the
new behavior (fidelity/refactor items). Milestone gate: full 56-target suite green on every commit,
1M stress no-regression for 190.2/190.3/190.5/190.6, explain-diff reviewed for every plan flip.
