# RFC-153 — Joined/derived-preserved-side LEFT OUTER: buried-merge correlation resolution

**Status:** Draft — needs a Graefe DESIGN ACK on the **approach choice (§4)** before implementation. Query-engine
(rewrite + possibly executor). Blocks PR #364 (held by user decision: fix before merge, not as a follow-up).

**Origin:** codex's 2nd P2 on #364. A genuine `tryFlatMapPlan`-retirement regression — but codex's suggested fix
(broaden the guard) was **verified to return WRONG ROWS**, so the correct fix is design-level. RFC-152 (the
cost-model materialization fix) is DONE and 4-gate-green at `e134911d7`; this is a separate, distinct issue.

## 1. The regression (verified real)

`SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.a_id = a.id` — the **preserved side is itself a
join** (`A⋈B`), and the LEFT-OUTER ON-predicate `c.a_id = a.id` correlates the null-supplying leg `C` to the
**buried** preserved source alias `A`, not to the synthetic merge quantifier `M` over `A⋈B`.

`RewriteOuterJoinRule`'s `correlated` guard (`rule_rewrite_outer_join.go:92-100`) tests only
`GetCorrelatedToOfPredicate(p)[preserved.GetAlias()]` = `[M]` → the predicate correlates to `A`, not `M` → guard
returns **false** → **the rewrite is skipped**. With `tryFlatMapPlan` gone (it had a bespoke deep-flowed-outer
branch for exactly this), the planner falls back to a materialized `NestedLoopJoin(LEFT OUTER, FlatMap(B,A-probe),
Scan(C))` — a **full re-scan of C** instead of the correlated index-probe `Scan(C,[a_id=A.id])`. Verified on FDB
(typed plans): joined-preserved → materialized NLJ over full `Scan(C)`; simple-preserved control `A LEFT JOIN C ON
C.a_id=A.id` → correctly probes; preserved-only control → materialized NLJ (RFC-152 invariant intact).

## 2. Why the focused guard fix is WRONG (the trap)

Broadening the guard to test the preserved leg's **provided** aliases (own ∪ buried sources, via the existing
`physicalProvidedAliases` machinery) makes the rule **fire** and produces a probe plan
`FlatMap(outer=FlatMap(B,A), inner=DefaultOnEmpty(Fetch(IndexScan(C_A_ID,[=]))))` — which *looks* right but
**executes wrong**:

- Materialized NLJ (parent, reverted): `(1,100),(2,NULL)` — CORRECT.
- Probe (naive guard fix): `(1,NULL),(2,NULL)` — **WRONG** (A=1 fails to find its matching C=100).

**Root cause of the wrong rows:** the rewritten C-probe reads `c.a_id = QOV(A).id` (correlated to buried `A`), but
the FlatMap cursor binds the merged `A⋈B` outer row under the **merge alias `M`**, not `A`. So `QOV(A).id` is
**unbound at runtime** → evaluates NULL → `c.a_id = NULL` → no match → spurious null-extension. The merged row
carries a *qualified* `A.id` key but there is **no binding for the bare alias `A`** that the correlation reads.

So a naive guard-broadening turns slow-but-CORRECT into fast-but-WRONG — strictly worse than the perf regression.
The guard is not the (whole) fix; the **correlation must be resolvable at execution**. Reverted; HEAD pristine.

## 3. Java reference (to determine before choosing)

Java's `RewriteOuterJoinRule.buildInnerSelect` rewires the **null-supplying** side's references via a
`TranslationMap` (`aliasRewire`: replace leaf refs to `nullSupplyingQun.alias` with `existingSelect.getResultValue()`).
The open Java question for §4: **how does Java's rewrite/execution resolve an ON-predicate that correlates to a
BURIED alias inside the PRESERVED side** (a join/derived preserved input)? Does Java's preserved quantifier flow a
result value through which `A.id` is accessed as a field (so there's no bare-`A` correlation at runtime), or does
Java bind buried source aliases in the join cursor? The implementer must read this before §4 is decided — the
Java-faithful answer picks the approach.

## 4. Design options (Graefe decides — this is the ACK gate)

**(a) Rewrite-side correlation rewiring** — the rewrite rewires buried-preserved references (`a.id`) into **field
accesses on the preserved merge quantifier's result value** (`M.A_id` / `FieldValue(QOV(M), …)`), analogous to
`buildInnerSelect`'s `aliasRewire` but for the **buried PRESERVED** side rather than the null-supplying side. After
rewiring, the C-probe reads a field of the bound merge row `M` (which IS bound) instead of an unbound bare `A`.
Pro: localized to the rule; no executor change; mirrors Java's translation discipline. Con: requires the merged
result value to expose the buried column as a resolvable field, and the index-probe SARG must still bind through it.

**(b) Execution-side buried-alias binding** — the FlatMap / NLJ cursor binds the merged outer row's **buried source
aliases** (`A`, `B`) in the correlation environment, so `QOV(A).id` resolves at runtime. Pro: any buried-correlation
plan just works, not only this rewrite. Con: executor change on the 0-row/correlation surface; broader blast radius;
must not perturb the `qualifyOuterRow` key model (RFC-077) we just stabilized.

Graefe picks (a) or (b) (or a hybrid) based on the §3 Java reading. Whichever — it earns its own impl ACK with a
**LEFT/FULL-OUTER null-extension row-count proof** (correctness is the bar; the naive fix failed exactly here).

### 4.1 Graefe DESIGN ACK — **approach (a), rewrite-side rewiring** (b rejected)

Graefe ACK'd **(a)**. Decisive grounds (he read both references himself):

- **Go already does (a) in production.** `rebaseOuterLegRefsToMerged` / `rebaseOuterLegValue` (RFC-142,
  `rule_implement_nested_loop_join.go:743,812`) already rewrites buried-leg refs `QOV(A).col` into field accesses
  on the merged row's qualified `"LEG.COL"` key under the merge correlation — exactly approach (a), shipping for
  the EXISTS-over-join path. RFC-153 (a) is the *same established pattern* applied to the buried-**preserved** side,
  not a new mechanism.
- **Java's discipline is rewrite-side.** `buildInnerSelect`'s `aliasRewire` is a `TranslationMap`
  (`.then((src,leaf) -> existingSelect.getResultValue())` via `translateCorrelations`) — translation, not
  execution binding. (a) mirrors Java; (b) would invent a *second, divergent* resolution mechanism.
- **The clincher — (a) reuses the machinery we just hardened.** After (a) rewires `c.a_id = QOV(A).id` →
  `c.a_id = QOV(M).A_id` (field of the bound merge row `M`), the comparand is correlated to the **outer** `M`, not
  the matched source `C`. So `comparandIndependentOfSource` (the RFC-150 comparand guard) sees it independent →
  **SARGs the correlated probe** `Scan(C,[a_id=<M.A_id>])`; `compensationProbeCorrelations` reports `M` as a probe
  correlation → the GRAEFE-2 probe-fed-residual guard treats C as a genuine correlated inner. (a) threads cleanly
  through SARG-as-bound + the comparand guard + the probe-correlation guard + anchored-merge rebasing — all
  verified on FDB this session. (b) routes around all of it, on the `qualifyOuterRow`/RFC-077 surface we stabilized
  twice this session.

**Impl conditions for (a):** ① the merged result value must expose the buried column (`A_id`) as a field
resolvable on `QOV(M)` (verify the anchored-merge result value carries it as a qualified key, and that the rewired
comparand SARGs into C's index); ② LEFT/FULL-OUTER null-extension row-count proof (joined-preserved `(1,100),(2,NULL)`,
its FULL-OUTER variant, simple-preserved + preserved-only RFC-152 controls — Graefe re-runs these at impl ACK);
③ typed-tree assertions only, plus the probe-shape (perf) assertion and the §5 correctness pin.

**SAFETY VALVE (Graefe, important):** this is a **perf** regression — correct rows ship today via the materialized
NLJ. If (a) proves hard to thread, the correct fallback is **NOT (b)** under merge pressure — it is **ship the
correct materialized NLJ and file the probe as a follow-up optimization**, and flag to the user. Do **not** put a
0-row-surface executor change on the critical path for a perf gate. A slow-but-correct plan already shipping beats a
rushed executor change on the surface we just fixed twice.

Final (a)/hybrid ACK pending the implementer's §3 Java corroboration (Graefe expects it to confirm, since
`aliasRewire` is rewrite-side). The impl earns its own ACK on the null-extension row-counts, which Graefe runs.

### 4.2 The LAYER — implementer hit a wall at the rewrite rule; Graefe ACK'd **(a-implement)**

The implementer corroborated (a) via the Java reading (`QueryVisitor.wrapOperandsForOuterJoin`/`rewireQov`,
fdb-relational-core, collapses `A⋈B` into one rewired operator **at translation**, so `A.id` is a merge `FieldValue`
by ordinal before the `OuterJoinExpression` exists) — then implemented (a) **at the rewrite rule** and hit a verified
wall: Go mints the synthetic merge alias `$m"N` at **PLANNING** (`PartitionSelect`, `memo.go:139`), *after*
`RewriteOuterJoinRule` runs. So rewiring the pushed pred onto `preserved.GetAlias()="B"` targets the pre-merge B-scan
row (no `"A.ID"` key) → still `(1,NULL),(2,NULL)`. The merge's own `rebaseBuriedLowerReferences`
(`rule_partition_select.go:889-906`) rewires buried refs onto `QOV($m)` but only *within* the partitioned select; the
LEFT-OUTER null-supplying inner SUBSEL is a **separate memoized reference**, never rewired. Safety valve invoked,
reverted (HEAD pristine `15d2ab340`), sentinel committed `924de2c31`.

**Graefe's layer call: (a-implement). Reject (a-translation) and (a-merge).** The principle is *rewire the
buried-preserved correlation onto the merge quantifier where the merge is established.* Java establishes the merge at
translation (SQL layer) → rewires there; **Go deliberately establishes it at PLANNING** (`PartitionSelect` mints
`$m`, a Cascades-core divergence) → the Cascades-faithful Go analog rewires **where `$m` exists: the
planner/implementation leg** (`yieldGeneralFlatMap`, `rule_implement_nested_loop_join.go:384-457`, where `outerCorr`
IS `$m`). Same layer + same `rebaseOuterLegRefsToMerged` machinery the **working** EXISTS-over-join path uses.
- **(a-translation) rejected** — imports Java's SQL-layer pre-collapse into Go's translator, fighting the intentional
  merge-at-planning architecture; big blast radius (every joined-preserved query, not just LEFT-OUTER) + a
  representational split. Faithfulness is to the *principle*, not Java's literal layer.
- **(a-merge) rejected** — couples `PartitionSelect` to LEFT-OUTER semantics AND reaches **across references**
  (mutating the separate memoized null-supplying SUBSEL when `$m` is born elsewhere) → breaks the Cascades memo model
  (refs are shared/interned; a rule yields a new expression, it doesn't mutate a sibling ref). Latent correctness
  hazard.

**Impl conditions (Graefe runs the row-counts at impl ACK):**
1. **Guard-broaden and rewire are ONE unit** — broadening the guard to fire on buried/provided preserved aliases is
   safe *only* because the implementation-layer rewire follows; the rewire MUST fire on **every** path the broadened
   guard admits. A broadened guard without a guaranteed rewire on some path IS the §2 wrong-rows trap.
2. Reuse `rebaseOuterLegRefsToMerged`/`rebaseOuterLegValue` (the EXISTS-path fn), not a new rewriter.
3. The C subplan arrives pre-built with the buried-`A` SARG baked in (the rewrite pushed the ON-pred into the C
   SUBSEL) → rewrite `innerPlan`'s SARG comparands (or rebase the C SUBSEL's predicates) at the point `outerCorr` is
   known, targeting the **authoritative qualified key** the hardened `qualifyOuterRow` writes (`QOV($m)."A.ID"`).
   After rewire the comparand is correlated to outer `$m` → `comparandIndependentOfSource` SARGs
   `Scan(C,[a_id=<$m.A_id>])`; `compensationProbeCorrelations` reports `$m` → probe-fed-residual guard treats C as a
   genuine correlated inner.
4. **Row-count matrix (Graefe runs):** joined-preserved `(1,100),(2,NULL)`; its FULL-OUTER variant; a **deeper**
   nesting (preserved side a 3-way join, correlation to the deepest buried alias); simple-preserved + preserved-only
   controls (RFC-152 invariant); a buried-correlation-to-the-OTHER-leg (`c.x = b.something`) variant (confirms the
   rewire targets the right qualified key). Correct rows AND the probe shape (perf). Typed-tree assertions only.

**Safety valve stands:** if (a-implement) doesn't thread cleanly, do NOT escalate to (a-translation)/(a-merge) under
merge pressure — ship the correct materialized NLJ (green, sentinel `924de2c31`) + file the probe as a perf
follow-up, and surface to the user. Perf-only; correct-but-slow shipping today beats a rushed cross-layer change.

## 5. Correctness sentinel (commit now, independent of the fix)

The subagent saved an FDB sentinel (`scratchpad/joined_preserved_outer_join_fdb_test.go.artifact`) asserting the
joined-preserved LEFT JOIN returns **correct rows** (`(1,100),(2,NULL)`). It PASSES now (materialized NLJ) and would
FAIL on a naive guard-broadening (the wrong-rows trap). Commit it as a standalone correctness pin — it is the
regression net the eventual fix must keep green (and guards against anyone re-attempting the naive guard fix). The
perf assertion (probe, not materialized NLJ) is added with the §4 fix.

## 6. Verification + gates

Correct rows on the joined-preserved LEFT OUTER (the bar the naive fix failed) + the probe plan (perf fixed) +
all LEFT/FULL-OUTER null-extension + correlated-EXISTS + RFC-152 pins green; full `//...` 53/53; 1M stress vs master
(the joined-preserved shape returns to probe); plandiff classified improvement/neutral; typed-tree assertions only.
Query-engine → Graefe ACK on **this RFC's approach choice** AND the impl, plus Torvalds + @claude + codex(`--post`).
Then codex's 2nd P2 is resolved and #364 merges.

## 7. Scope

Fixes a real `tryFlatMapPlan`-retirement perf regression for joined/derived-preserved-side LEFT OUTER (correct rows
today via materialized NLJ; the probe is the optimization `tryFlatMapPlan` used to provide). NOT a correctness bug
on master (rows are correct) — but the user elected to fix it before #364 merges (no-perf-regression gate). The
naive guard fix is explicitly rejected (§2): it trades a perf regression for a correctness regression.
