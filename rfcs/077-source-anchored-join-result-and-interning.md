# RFC-077: Source-anchored join result + structural interning (holistic 7.5 + 7.6)

**Status:** 7.5 IMPLEMENTED (gated alias-aware `Reference.Insert`/`InsertFinal` interning retires
`mergeQuantifierAlias`; full suite green, chain task-count gate pinned) — re-review pending on the
CORRECTED root-cause (see "Precise root-cause — CORRECTED" below; the originally-ACK'd "candidate-narrowing
hash" mechanism was wrong). 7.6 DEFERRED (F3 column threading) per Graefe split. Earlier: Accepted
(Graefe ACK + Torvalds ACK; step 5 corrected per Torvalds — merge value read at PLAN time; Graefe's E2E
conditions folded into the test plan)
**Area:** Cascades query planner — join result values, field pull-up, memo interning
**Reviewers:** Graefe (Cascades alignment — mandatory; supersedes the deferred RFC-073), Torvalds (code quality), codex, @claude

Supersedes the deferred **RFC-073**. Bundles TODO **7.5** (structural interning key) and **7.6**
(source-anchored field pull-up) into ONE migration, because they are two facets of the same root
and RFC-073's review gated 7.6 on 7.5 — a circular dependency that one holistic change resolves.

## Problem

The Go join path is built on an **opaque, name-keyed merge** representation that Java does not have:
- `JoinMergeResultValue` (binary seed) / `JoinMergeAllValue` (N-ary re-enumeration): result values
  whose `Evaluate` produces a flat `map[string]any` with bare + `ALIAS.COL` keys.
- `composeFieldOverJoinMerge`: a band-aid that re-derives field→source anchoring AFTER the fact,
  hard-coding the inner leg (sound only by a structural invariant + a qualified-field fail-safe).
- `mergeQuantifierAlias`: a synthetic **string** quantifier alias (`$m_<len>:<name>…`) used as the
  re-enumeration's interning key, because the memo interns under the empty alias map and the opaque
  merge has no canonical structural identity. **Measured load-bearing** (replacing it with
  `uniqueId` 6×'d the chain task count) — *because* the merge is opaque.

Java instead anchors every projected field to its source quantifier: `RecordConstructorValue` of
`FieldValue(QuantifiedObjectValue(leg), col)`. There is no opaque merge, no after-the-fact
re-anchoring, no synthetic interning alias — the anchored record IS the canonical identity.

Two TODO items, one root:
- **7.6** = retire the opaque merge + `composeFieldOverJoinMerge` for anchored access.
- **7.5** = give the re-enumeration a structural interning key instead of the string alias.

Anchoring (7.6) makes the merge value canonical, so **7.5 falls out for free**: an anchored
`RecordConstructorValue` interns structurally via RFC-039/040 `MemoEqual` — the synthetic
`mergeQuantifierAlias` is no longer needed.

## Investigation

**RFC-073 (deferred) settled the direction; the review surfaced the blockers.** Graefe ACK'd:
translator emits `RecordConstructorValue` of `FieldValue(QOV(legAlias), col)`, resolved by the
**existing** `composeFieldOverConstructor` (`simplifier_value.go` — `field(RC(…, x as name, …),
"name") → x`); end state = pure anchored access. Torvalds NAK'd the *as-written* scope: the opaque
merge is read at runtime by direct consumers (`cascades_generator.go:1890` column derivation,
`executor.go:1434 mergeRows`, `streaming_cursors.go`), and a naive swap changes the `Evaluate`
shape under them. Both required 7.5 first (anchoring only the binary join while the N-ary
re-enumeration stays opaque is a split-brain).

**Design unlock — Go uses NAME-based anchoring, not Java's ordinal machinery.** Java rebases via
`FieldValue.ofOrdinalNumber(QOV(newUpper), i)` (ordinal-indexed records). Go's
`RecordConstructorValue.Evaluate` already produces a **name-keyed** `map[string]any`
(`values.go:2148`: `out[f.Name] = f.Value.Evaluate(...)`). So Go does NOT need the full
ordinal-substrate rewrite (no `FieldValue.ofOrdinalNumber`, no `mergeRows`/cursor ordinal records).
The anchored `RecordConstructorValue` evaluates to a name-keyed row whose keys are the projection's
column names — the same *kind* of structure consumers already read, just **canonically anchored**
instead of opaque. This is a clean, strictly-better Go adaptation (sanctioned: diverge from Java
when cleaner + wire-compat-neutral; this is pure read-side).

## Fix

1. **Anchored merge value.** The translator's join seeds and `PartitionSelectRule`'s re-enumeration
   emit `RecordConstructorValue` whose columns are `FieldValue(QOV(legAlias), col)` (one column per
   projected/live field), replacing `JoinMergeResultValue`/`JoinMergeAllValue`. The column NAME is
   the SQL-visible column (or `ALIAS.COL` for disambiguation) so name-based resolution is preserved.
2. **Compile-time simplification covers most queries.** `composeFieldOverConstructor` rewrites
   `field(RC(…), col)` → the anchored leg `FieldValue(QOV(leg), col)` during planning, so for
   projected-column queries the RC is simplified away and never reaches `Evaluate` — no runtime
   shape change at all. For `SELECT *`/flow-through the RC survives; its name-keyed `Evaluate`
   (`values.go:2148`) yields the column-named row the derivation/`mergeRows` path consumes.
3. **Structural interning (7.5).** With the canonical anchored RC, the re-enumeration's sub-products
   intern via `MemoEqual`/`HashCodeWithoutChildren` over the anchored structure — retire
   `mergeQuantifierAlias`. The merge quantifier gets a plain `uniqueId` (Java-style), interning via
   the now-canonical result value (no synthetic content-derived alias).
4. **Retire** `JoinMergeResultValue`, `JoinMergeAllValue`, `composeFieldOverJoinMerge`, and the
   bare/qualified-key apparatus + the `Seed` provenance bit (RFC-074) — all subsumed by anchored RC.
5. **Consumer migration — the merge value is read at PLAN time, not at runtime (Torvalds'
   correction).** The runtime `ALIAS.COL` keys are NOT built by the result value's `Evaluate`: they
   are built *physically in the cursor* by `executor.go:1434 mergeRows` + `qualifyOuterRow`, which
   qualify the outer/inner row maps independently of the merge value's type. So swapping the result
   value to an anchored RC does NOT touch the runtime row producer — the blast radius is the two
   **plan-time** consumers of the merge value:
   - **(a) Reversal signal — `joinResultValueIsReversed` (`cascades_generator.go:1897`).** It reads
     the binary seed's `Seed`+ordered `Aliases[0]` to decide whether SQL column order is opposite to
     the physical outer/inner assignment (so columns emit in SQL order). The anchored RC carries this
     signal *better*: its field declaration order IS the SQL column order (outer-leg cols then
     inner-leg cols, each anchored to `QOV(leg)`), so column ordering reads off the RC's anchored leg
     sequence directly. Migrate `joinResultValueIsReversed` (or fold it into the column-derivation)
     to read the RC's leg order — if the seed's order signal is dropped, column order flips silently.
   - **(b) SARG anchoring — `composeFieldOverJoinMerge` → `composeFieldOverConstructor`.** The latter
     (`simplifier_value.go:269-274`) **returns `nil` on a field-name miss** — the silent-nil landmine.
     The migration must guarantee every projected/live column the SARG/derivation pulls up has a
     matching field NAME in the anchored RC (incl. `ALIAS.COL` disambiguation for duplicate bare
     names), so `field(RC, col)` always resolves to a leg `FieldValue` and never collapses to nil.
   - **(c) `SELECT *`/flow-through.** Only here does the RC survive to `Evaluate`; its name-keyed
     row (`values.go:2148`) is the column-named map the derivation path already consumes. No ordinal
     rewrite. This case + an ambiguous-duplicate-name case are E2E-pinned before any deletion (below).

## Performance

No wire change (read-side only). Same plans; the anchored RC interns better than the opaque merge
(structural identity), so re-enumeration sub-product sharing improves or holds. plandiff
byte-identical at every arity; stress-1M within noise.

## Test plan

- **Anchored-value unit tests**: `field(RC(anchored cols), col)` simplifies to the leg FieldValue;
  the RC's `Evaluate` yields the correct column-named row; two equal anchored RCs intern (structural,
  no string alias). **Name-miss guard**: a unit test asserting that for every projected/live column
  the SARG/derivation pulls up, `composeFieldOverConstructor` over the anchored RC resolves to a leg
  `FieldValue` (never nil) — pins the `simplifier_value.go:269-274` silent-nil landmine shut.
- **Column-order regression (reversal signal, Torvalds (a))**: a join whose SQL leg order is opposite
  the physical outer/inner (the case `joinResultValueIsReversed` exists for) emits columns in SQL
  order via the anchored RC's leg sequence — pins that the reversal signal survives the migration.
- **Interning regression (7.5)**: re-enumerated sub-products over the same leg-set intern to one
  Reference WITHOUT `mergeQuantifierAlias` (the string scheme deleted).
- **Join correctness E2E**: the full FDB join suite green — multi-way chain + star, middle-table
  projection (`TestFDB_MultiwayJoinOrder_Nway`), the outer-column sentinel
  (`TestFDB_JoinMerge_OuterColumn_NotDropped`).
- **Graefe's pre-deletion conditions (must be green BEFORE the opaque types are deleted)**:
  (1) `SELECT *`/flow-through over a join — the RC survives to `Evaluate`, row is correct;
  (2) an **ambiguous-duplicate-name** join (same bare column on both legs) — `ALIAS.COL`
  disambiguation keeps both columns distinct, no silent collision;
  (3) **plandiff byte-identical at every arity** (2-way … N-way) before vs after.
- **No regression**: plandiff conformance green; determinism 10×; full `just test`; stress-1M.

## Risk / honesty

This is the largest Phase-7 change (it touches the translator, `PartitionSelectRule`, the value
package, and the simplifier). The name-keyed design (not ordinal) keeps the blast radius to
*value-type swap + plan-time consumer migration* — the runtime row producer (`mergeRows`/
`qualifyOuterRow`) builds `ALIAS.COL` keys physically in the cursor and is **untouched** by the
result-value swap (Torvalds' correction). The two load-bearing risks are both plan-time:
(a) the **reversal signal** `joinResultValueIsReversed` derives from the seed's ordered `Aliases` —
if not preserved in the anchored RC's leg order, column order flips silently; (b)
`composeFieldOverConstructor` **returns nil on a field-name miss** (`simplifier_value.go:269-274`) —
a missed name silently drops the column. Both are pinned by named regressions (column-order test +
name-miss guard) above. The E2E join suite + outer-column sentinel + Graefe's three pre-deletion
conditions (SELECT *, ambiguous-duplicate-name, plandiff byte-identical at every arity) are the
safety net; each consumer is migrated + pinned BEFORE the opaque types are deleted. Done as its own
focused PR (separate from RFC-076 / 7.7).

---

## v2 amendment — implementation findings (pre-implementation, before any code)

A read-only survey of the apparatus (every `New*JoinMerge*` site + all consumers) surfaced two
realities the "Fix" steps above underspecified. Neither changes the END STATE (anchored RC, sole
path) — they refine HOW, and they are the load-bearing risks to validate first.

### F1 — leg columns come from the legs' quantifier result TYPES, not from the construction site

The `Fix` says the translator/`PartitionSelectRule` "emit `RecordConstructorValue` whose columns are
`FieldValue(QOV(legAlias), col)` (one column per projected/live field)." But at every construction
site — `pkg/relational/core/query/cascades_translator.go:396/657/767` (the binary seed sites — this
file DOES exist and these are production callers; the seed-vs-exact gate is read at
`rule_partition_select.go:210`) and `rule_partition_select.go:370/437` (re-enumeration) — the code
has ONLY the leg ALIASES — the projected column list is not in hand (the real projection lives in the parent
`Project`; the seed deliberately "hides" it; the re-enumeration knows only the live alias set). The
columns ARE available, but indirectly: each leg quantifier ranges over a Reference whose result
**type** is a `RecordType` carrying the leg's column names/types (exactly Java's source-record
columns). Resolution: build the anchored RC by enumerating each leg's columns from
`quantifier.GetRangesOver()`'s result `RecordType` — `FieldValue(QOV(leg), col)` per column, named by
the column (or `ALIAS.COL` for cross-leg duplicates). Where a leg's result type is `UnknownType`
(not yet derived), fall back to the current opaque merge for that node and let a later pass anchor it
— but the seed sites have typed sources, so this is the common path. This keeps the name-miss guard
(F-(b)) satisfiable: the RC's field names are exactly the union of the legs' columns.

**Torvalds condition — the `UnknownType` fallback is a transient interim, NOT a permanent dual path.**
Step 4 deletes `JoinMergeAllValue` OUTRIGHT; a fallback that survives the deletion is a split-brain.
Acceptance gate (assert, don't hope): instrument the builder so a test fails if ANY seed/re-enum
site takes the `UnknownType` arm across the full suite. If a leg is genuinely untyped at a seed site,
that is a real type-derivation bug to root-cause, not paper over. After step 4 the fallback is gone,
so the gate's real purpose is to PROVE it was never needed before the deletion lands.

### F2 — the anchored RC REPORTS its leg correlations; the `Seed` bit's correlation-HIDING must be replicated, not just deleted

The RFC says the `Seed` provenance bit is "subsumed by anchored RC." For interning and the reversal
signal that holds (leg order + structural identity). But `Seed=true` ALSO does something the bare
anchored RC does NOT: `GetCorrelatedToOfValue` (`value_correlation.go:34-48`) returns NOTHING for a
seed, deliberately HIDING the leg aliases — measured load-bearing (reporting them is +~32% planner
tasks, tipping the ≥4-way STAR past budget; RFC-074). A `RecordConstructorValue` of
`FieldValue(QOV(leg), …)` naturally reports every leg alias (it IS correlated to them), which
reintroduces exactly that pressure. Two clean options, to be settled with Graefe:
  (i) **Anchored-seed correlation suppression** — when the RC is a join SEED (its columns are leg
  QOVs over the select's OWN immediate quantifiers, i.e. correlations the surrounding select already
  binds), exclude those self-bound leg aliases from the value's reported correlation set, mirroring
  the `Seed=true` suppression. The provenance is no longer a stored bit but a structural property
  (columns anchored to the select's own quantifiers ⇒ not external correlations) — strictly more
  honest than the bit. `predicate_correlation.go`'s `AddMergeSeedAliases` becomes "read the RC's
  anchored leg QOVs directly" (the buried-column classification it needs is now explicit in the RC).
  (ii) keep a thin provenance flag on the construction path only for the exploration-budget gate.
  (i) is preferred (no bit, structural). Validation: the ≥4-way STAR task count must not regress —
  add it to the test plan as a hard gate (not just plandiff).

  **Graefe caveat (ACK condition) — the `Seed` bit is DUAL-purpose; replicate BOTH halves.** Beyond
  exploration-time HIDING (`GetCorrelatedToOfValue` → nothing, for budget), the bit is RE-EXPOSED at
  partition time by `AddMergeSeedAliases` (`predicate_correlation.go`), which feeds the seed's leg
  aliases back into a predicate's correlation set so `PartitionSelectRule` does NOT misclassify a
  predicate reading a buried column as lower-only and push it below the merge to a leaf where the
  alias is unbound (the 0-row dual-correlation bug, RFC's fix #2). F2-(i)'s "read the RC's anchored
  leg QOVs directly" must reconstruct **both** halves: (1) the value's reported external-correlation
  set EXCLUDES self-bound leg QOVs (hiding), AND (2) partition-time predicate classification still
  SEES the buried leg aliases via the RC's anchored columns (re-exposure). Pin BOTH:
  `TestFDB_JoinMerge_OuterColumn_NotDropped` + the dual-correlation 0-row regression for re-exposure,
  the ≥4-way STAR task-count gate for hiding. A `RecordConstructorValue` exposes its leg QOVs in
  `Children()`/`Fields`, so partition-time re-exposure reads them structurally (no bit) — Graefe's
  "internally-bound ⇒ omit from getCorrelatedTo() is the CORRECT set, not a hack" confirms the
  exclusion is principled, not a workaround.

### F3 — leg columns are NOT available at the translator seed sites (blocks translation-time anchoring)

Step-2 implementation surfaced a blocker bigger than F1 assumed. F1 said "leg columns come from the
leg quantifier's result `RecordType`." They do not, at the translator: `translateScan`
(`cascades_translator.go:118`) emits `NewFullUnorderedScanExpression([Table], values.UnknownType)` —
the leg ref's result type is `UnknownType` with NO columns, and the `cascadesTranslator` struct has
NO catalog/metadata field (only `cteScope`/`cteExprScope`/`scalarSubqueries`). The SQL resolver knew
each leg's columns, but the logical plan DROPPED them: `LogicalScan` is `{Table, Alias}` only. So at
`NewJoinMergeSeedValue` (`cascades_translator.go:396/657/767`) there is no column source — neither the
logical plan nor the cascades ref type nor a catalog the translator can reach. **The anchored RC
cannot be built at translation time.** Step 1's builder (`NewAnchoredJoinRecord`, committed) is
correct and reusable; the open question is WHEN/WHERE it is called.

Three candidate approaches (architectural — Graefe to decide):
  - **(A) Thread columns to the translator.** Have the SQL resolver carry each leg's resolved output
    columns onto the logical plan (e.g. `LogicalScan.Columns`, populated at resolution), or give the
    translator a catalog handle so `translateScan` types the scan. Anchored RC built at the seed, per
    the original plan. Cost: changes the translator's currently catalog-free contract / the logical
    schema; columns flow earliest.
  - **(B) Anchor at a later column-aware pass.** Keep the opaque `JoinMergeAllValue` through
    EXPLORATION (it needs no columns — it merges all at `Evaluate`), and swap merge→anchored-RC in a
    rewrite once types/columns are derived. PROBLEM: `PartitionSelectRule` reads the merge during
    PLANNING, so the opaque type survives planning and is only retired at the very end — this does NOT
    retire it from the exploration/partition path (the 7.6 goal). Likely a non-starter for full
    retirement.
  - **(C) Split 7.5 from 7.6.** Do 7.5 (structural interning of the *opaque* merge — give it a
    content-canonical identity without the synthetic `mergeQuantifierAlias`) now, since it needs no
    columns; defer 7.6 (anchoring) until the resolver threads columns (A). The RFC bundled them
    because RFC-073 gated 7.6-on-7.5; F3 shows the reverse independence — 7.5 is doable alone, 7.6 is
    blocked on column availability. Honest partial: ships the interning win + removes the string-alias
    smell without the blocked anchoring.

Recommendation pending Graefe: (A) is the true end state (Java threads resolved columns); (C) is the
shippable increment if (A)'s resolver/logical-schema change is out of scope for this PR. (B) is
rejected (doesn't retire the opaque type from planning).

**Graefe F3 decision (ACK): (C). Ship 7.5 now; defer 7.6 to a column-threading follow-up; (B)
rejected.** Rationale: RFC-073 gated 7.6-on-7.5 on a *representation* dependency (anchoring makes the
merge canonical); F3 is an orthogonal *availability* dependency (7.6 needs columns, 7.5 does not) —
they compose, so the split is sound. (A) is the true end state but bundling the resolver/logical-schema
change with the anchoring is two failure surfaces under one plandiff; do (A) as its OWN first step of
the 7.6 follow-up, plandiff-neutral standalone (threading columns only enriches types), THEN anchor +
delete the opaque types on top. **Revised top-level sequence: [7.5 interning — THIS PR] → [(A) thread
resolved columns onto LogicalScan/translator, plandiff-neutral] → [anchor RC + delete JoinMergeAllValue/
Seed/composeFieldOverJoinMerge].** Condition on the 7.5-alone PR: it MUST keep the STAR ±2% task-count
gate — `mergeQuantifierAlias` was load-bearing (6× task count) precisely because the merge is opaque;
structural interning over the opaque value must reproduce that sub-product sharing or exploration
regresses. Pin the STAR baseline in the 7.5 PR.

### 7.5-alone scope (this PR)

Give the opaque `JoinMergeAllValue` a content-canonical structural identity so re-enumerated
sub-products SHARE one memo Reference WITHOUT the synthetic `mergeQuantifierAlias` string. The merge
value already has SET-based `SemanticEqualsUnderAliasMap`/`SemanticHashCode`; the remaining work is
making the re-enumeration's merge quantifier + its select intern structurally (the string alias is
currently the ref-sharing key — `rule_partition_select.go` builds the merge quantifier under
`mergeQuantifierAlias(live)` so identical live-sets reach the SAME Reference). Replace that with a
structural memo lookup (the merge value's canonical hash/eq) + a plain `uniqueId` quantifier, verified
by the STAR ±2% gate (sharing preserved) + plandiff byte-identical at every arity. 7.6 (anchoring) and
the opaque-type deletion are explicitly OUT of scope here (blocked on column threading, F3).

**Precise root-cause — CORRECTED after an implementation spike (the earlier hypothesis was wrong).**
The original guess (alias-SENSITIVE candidate-narrowing HASH in `memoizeNonLeaf`) does NOT hold against
the current code: `values.SemanticHashCode` is ALREADY alias-invariant for `JoinMergeAllValue` (RFC-074
folds only `len(Aliases)`+`Seed`, never the alias names) and `QuantifiedObjectValue` (`"qov"`, no
alias), and `memoizeNonLeaf` ALREADY uses alias-aware `MemoEqual`. So a `uniqueId` merge quantifier
does NOT change `HashCodeWithoutChildren`. The hash was a red herring.

The REAL alias-sensitive interning sites are **`Reference.Insert` and `Reference.InsertFinal`**
(`reference.go`). The upper merge select is not memoized via `memoizeNonLeaf` — it is **yielded** by the
rule, and the PLANNING yield path inserts it into BOTH member sets via `Reference.Insert` /
`Reference.InsertFinal`. Both dedup with two alias-IDENTITY tiers only: a fast
`EqualsWithoutChildren(…, EmptyAliasMap)`+pointer-identity path, then
`SemanticEquals(…, EmptyAliasMap)` — neither builds a quantifier-alias map, so two upper selects that
differ ONLY in the merge quantifier alias are treated as DISTINCT members → re-explored per
bipartition → super-linear blowup. `mergeQuantifierAlias` (stable per live-set) is a WORKAROUND that
makes those upper selects byte-identical so the alias-IDENTITY tiers dedup them. **This is a Go-vs-Java
divergence:** Java's `Reference.insert`/`containsInMemo` IS alias-aware (it is the very thing RFC-039's
`MemoEqual` ports); the Go port made `memoizeNonLeaf` alias-aware but left `Insert`/`InsertFinal`
alias-identity.

So the 7.5 fix is **make `Reference.Insert`/`InsertFinal` alias-AWARE for merge re-enumeration selects**:
add a third dedup tier `MemoEqual(m, e)` (ADD, not substitute — strictly additive, so it can only ever
dedup MORE than the identity tiers, preserving termination), GATED to expressions that opt in via the
new `SelectExpression.InternsAliasAware()` property. The merge quantifier then no longer needs the
synthetic stable string; `mergeQuantifierAlias` + `mergeAliasPrefix` are deleted.

**Merge alias = per-Memo DETERMINISTIC ordinal, NOT process-global `uniqueId` (codex P2).** A first cut
used `values.UniqueCorrelationIdentifier()` (process-global counter). codex flagged — and a probe
confirmed — that this regresses plan-hash DETERMINISM: the merge quantifier alias flows into
`RecordQueryNestedLoopJoinPlan.HashCodeWithoutChildren` (which folds raw source aliases) and thus into
`PlanHash` (plan-log identity, `plan_logging.go`) and the cost-model tiebreak (`deepHashCode`). With a
process-global counter, the SAME query planned after the counter has advanced (a long-lived process that
planned other queries) gets a DIFFERENT plan hash even though the plan is only alpha-renamed — master's
content-stable alias did not. (No correctness/wire/cache impact: the plan cache keys on normalized SQL
text, and continuation tokens do not use `PlanHash` — but per-process-history plan-identity churn is a
real regression.) Fix: mint the merge alias from a **per-Memo counter** (`Memo.NextMergeAlias` → `$mN`).
The Memo is created once per `Plan()`, so the same query mints the same alias sequence in the same
deterministic exploration order → a stable plan hash across plannings; yet distinct merge OCCURRENCES
within one plan still get distinct `$mN`, so the alias-aware `Insert` tier (not a stable string) is what
interns equivalent sub-products. Verified: `PlanHash` identical across two plannings separated by a
global-counter advance; chain task counts unchanged (8999 / 30593).

**Why GATED, not global (the over-dedup landmine — caught by the FDB suite).** A first cut made the
`MemoEqual` tier UNCONDITIONAL. That broke two CTE column-alias tests
(`TestFDB_CTEChainedColumnAliases`, `TestFDB_CascadesCTEColumnAliases`) with a silent-NULL column —
e.g. `WITH priced(product, cost) AS (SELECT name, price FROM Item) SELECT product FROM priced …` had
`product` read NULL. Root cause is NOT alias-namespace naming — item **7.1 is already DONE** (quantifier
and table alias naming was unified). It is the column-RESOLUTION model: Go's column derivation
(`cascades_generator`, e.g. the `scan.Alias`/`ssq.Alias` paths) resolves some references by
quantifier-alias IDENTITY, whereas Java resolves by group/ordinal and can therefore dedup members
alias-aware GLOBALLY (Graefe confirmed Java's `Reference.insert`/`containsInMemo` is alias-aware). An
alias-aware member collapse keeps a survivor whose quantifier aliases differ from the discarded
member's; any external structure that captured the discarded member's aliases by identity then
mis-resolves → the renamed column silently reads NULL. The merge re-enumeration is DIFFERENT: its merge
quantifier is planner-INTERNAL — `PartitionSelectRule` re-stamps all column access through the merge
value and rebases spanning predicates onto it, so NO external consumer resolves the merge alias by name.
So alias-aware interning is safe THERE and only there. The gate is a property derived from the
expression (`InternsAliasAware()` = "result value is a `JoinMergeAllValue`", the canonical marker of a
merge select — Graefe's "property on the expression, not an external heuristic"), and it is the minimal
change: it ADDS alias-aware dedup for merge selects and leaves every other expression's dedup exactly as
master (alias-identity). Widening the gate is gated on migrating Go's column resolution to Java's
ordinal/group model (a separate, large effort), NOT on 7.1 — until then the gate is the honest scope and
the correct architectural boundary (intern alias-aware only where aliases are planner-internal).

**Empirically verified (deterministic cascades harness, no FDB — `partition_select_interning_baseline_test.go`).**
The harness MUST configure the planner exactly as the SQL pipeline does
(`NewPlanner(DefaultExpressionRules()).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())`)
— `PartitionSelectRule` is PLANNING-only (`PlanningExplorationRules`, prepended by
`WithPlanningExpressionRules`), so a bare `NewPlanner(DefaultExpressionRules())` never fires the merge
re-enumeration. The shape is a **CHAIN**, not a star: a pure star never hits the ≥2-live merge branch
(all predicates run through the hub, so a connected ≥2-table lower always contains the hub and only the
hub is upper-referenced → single-live case). The 4-chain task count (~30k–60k) matches RFC-074's
"chain 64957" ballpark, confirming the harness drives the real path. Task counts (`tasksRun`):

| config | 3-chain | 4-chain |
|---|---|---|
| master (`mergeQuantifierAlias`) | 8999 | 29915 |
| naive `uniqueId`, alias-IDENTITY Insert (the trap) | 10312 | **60044** (≈2×) |
| `uniqueId` + alias-aware Insert ONLY | — | 43801 (partial) |
| `uniqueId` + alias-aware Insert AND InsertFinal (the fix) | 8999 (exact) | 30593 (+2.3%) |

The naive `uniqueId` alone DOUBLES the 4-chain count (confirming `mergeQuantifierAlias` was load-bearing
— the original RFC premise was directionally right even though its mechanism was wrong); the full fix
reproduces master's sub-product sharing (3-chain exact; 4-chain +2.3% with IDENTICAL merge-branch hit
count 42 — bounded, NOT super-linear). The blast radius on the cascades package is exactly ONE test
(`TestPartitionSelect_SeedMergeRestampedOverMergeQuantifier` detected the merge quantifier by the `"$m"`
name prefix; now detects it STRUCTURALLY — the merge quantifier's child holds a `JoinMergeAllValue`-result
select). The chain task-count gate (±2%, pinned at 8999/30593) replaces the deleted
`TestMergeQuantifierAlias_Injective` — a far stronger probe (it measures actual exploration sharing, not
a string-encoding property). Land it gated + plandiff-byte-identical. (Status: implemented; under review.)

### Revised sequence (consumer-migrate-before-delete, each plandiff-verified) — SUPERSEDED by F3 resolution

1. Add the anchored-RC builder (from leg result types, F1) + RC correlation handling (F2-i) +
   `composeFieldOverConstructor` name-miss guard test — no call-site change yet. **[DONE, committed 7f5c44f1.]**
2. Switch the binary seed sites (`cascades_translator.go`) to the anchored RC; migrate
   `joinResultValueIsReversed` to read the RC's leg order; verify plandiff byte-identical (2-way) +
   STAR task count + full suite.
3. Switch `PartitionSelectRule` re-enumeration to the anchored RC; retire `mergeQuantifierAlias`
   (interning now structural); verify plandiff at every arity + STAR task count.
4. Delete `JoinMergeAllValue`, `Seed`, `composeFieldOverJoinMerge`, `AddMergeSeedAliases`'s seed arm.
5. Full gates + stress-1M.

If F2 cannot be made budget-neutral, fall back to (ii) or pause the deletion (partial: anchored
binary seed only) rather than ship a task-budget regression.

### STAR task-count gate (Torvalds condition — concrete, not "must not regress")

Record the CURRENT ≥4-way STAR `tasksRun`/`distinctRefs` baseline (master) as a pinned number in the
test, and assert the post-change count is within **±2%**. A bare "must not regress" is a vibe;
plandiff is blind to the +32% exploration blowup (byte-identical plans, more tasks). The pinned
assertion is the only thing that catches an F2 correlation-suppression miss. Capture the baseline
before step 2 and check it into the regression at step 2 and step 3.

### v2 amendment review status

Graefe ACK (condition: F2-(i) reconstructs BOTH halves of the dual-purpose `Seed` — exploration-time
hiding AND partition-time re-exposure; pin the dual-correlation 0-row regression + STAR gate — folded
into F2 above). Torvalds ACK (conditions: precise construction-site coordinates incl. full path —
fixed in F1; `UnknownType` fallback acceptance gate + outright deletion at step 4 — folded into F1;
concrete STAR baseline+tolerance — this section). Both conditions are now in the amendment; implement
per the revised sequence.

---

## v3 amendment — 7.5 SHIPPED; 7.6 architecture settled (Graefe Option-B ruling)

**7.5 is DONE & MERGED (PR #258).** Gated alias-aware `Reference.Insert`/`InsertFinal` interning +
per-Memo collision-proof merge alias (`$m"N`) retired `mergeQuantifierAlias`. All four gates ACK'd.

**7.6 — the F3-A "type the scan" step was NAK'd by Graefe and is RETRACTED.** A first cut typed the
`FullUnorderedScanExpression` leaf from metadata. Graefe NAK'd it (verified against Java):
`RelationalExpression.java:133-145` and `ExpansionVisitor.java:105-109` build the scan leaf as
`Type.AnyRecord(false)` on BOTH the query and the candidate side; the concrete `RecordType` lives on
the `LogicalTypeFilterExpression` ABOVE the scan. Typing the scan is the divergence — it broke
index/PK leaf-matching (`matchLeafWithCandidate` requires the query scan to equal the untyped
candidate leaf), forcing a `leafScansSubsume` wildcard that Graefe rejected. **The scan stays
`UnknownType` (Go's `AnyRecord` analog); no leaf change.**

**Go vs Java structural divergence (verified):** Go collapsed Java's `Scan(AnyRecord)+TypeFilter(typed)`
into a bare `Scan([recordTypes], UnknownType)` — Go's production query base has NO `LogicalTypeFilter`
wrapper, and Go's `LogicalTypeFilterExpression` is type-LESS (carries only record-type NAMES; its own
doc defers the `resultType`). So Java's "type lives on the TypeFilter" has no Go analog in the query
base.

**Graefe ruling: OPTION B (build the anchored RC at the seed from metadata).** Rationale: the consumer
of the anchored RC is `composeFieldOverConstructor` (Java's `ComposeFieldValueOverRecordConstructorRule`),
which resolves `field(RC, name)` purely by the RC's field NAMES — it never consults a TypeFilter
result-type. Each leg's column type rides on `FieldValue(QOV(legAlias), col)` directly. So sourcing the
RC's columns from `md` at the seed (`md → RC`) is sufficient and is one fewer indirection than Java's
`md → RecordType → TypeFilter.resultType → RC`. **Option A** (un-collapse the query base + give
`LogicalTypeFilterExpression` a typed `resultType`) is the correct long-term home for index-pushdown
type-narrowing but buys nothing for 7.6 and is **demoted to its own future RFC**, not folded here.

**The four binding conditions on the Option-B ACK (must hold at impl review):**
1. **No leaf change** — `translateScan` keeps emitting `FullUnorderedScanExpression([table], UnknownType)`.
   Do not type the scan or add a TypeFilter typing layer (that is scope-creep back into A).
2. **Naming parity** — `NewAnchoredJoinRecord` emits the EXACT bare+qualified key set the opaque
   merge's `Evaluate` produced (qualified `ALIAS.COL` always; bare only when the column name is unique
   across legs). Pin with a test that diffs resolvable keys old-vs-new on a duplicate-bare-name
   multi-way join.
3. **Seed-bit retirement WITH PROOF** — `PartitionSelectRule` relies on the `Seed` flag to keep lower
   aliases live. The anchored RC names its sources honestly (every field is `FieldValue(QOV(leg), …)`),
   so `Seed` CAN retire — but only after proving `PartitionSelectRule` reads the RC's correlated-to set
   correctly (the F2 dual-correlation: exploration-time hiding + partition-time re-exposure). Demonstrate
   in the impl + pin the dual-correlation 0-row regression. Do not silently drop `Seed`.
4. **Option A stays a separate, demoted RFC item** (typed-TypeFilter result-type for index-pushdown
   type-narrowing) — not a 7.6 gate.

**Implementation plan (Option B):**
1. Thread `md` into the cascades translator (`TranslateToCascadesWithSubqueries(op, md)`; nil-md wrapper
   for catalog-free callers). `translateScan` UNCHANGED. (Subquery `:338` + DML `:641` callers stay
   nil-md for now; type their legs when 7.6 reaches them — Torvalds follow-up.)
2. Add a logical-op output-column derivation `legOutputColumns(op, md)` (scan → md columns; join →
   union with the anchored RC's qualified naming; filter → inner; project → projected; etc.), producing
   names CONSISTENT with `NewAnchoredJoinRecord` so nested-join legs compose. (No existing logical-schema
   helper — `deriveColumnsFrom*` are post-planning/physical.)
3. At `translateJoin`'s three seed sites, replace `NewJoinMergeSeedValue(leftAlias, rightAlias)` with
   `NewAnchoredJoinRecord([{leftAlias, legOutputColumns(left)}, {rightAlias, legOutputColumns(right)}])`.
4. Migrate the plan-time consumers: `joinResultValueIsReversed` → read the anchored RC's leg order;
   `composeFieldOverJoinMerge` → the existing `composeFieldOverConstructor`. F2: `PartitionSelectRule`
   correlated-to + `AddMergeSeedAliases` read the RC's anchored leg QOVs (hiding + re-exposure).
5. Delete `JoinMergeAllValue` / `Seed` / `composeFieldOverJoinMerge` only after every consumer is
   migrated + pinned. Gates: chain/STAR task-count, plandiff byte-identical at every arity,
   dual-correlation 0-row, `TestFDB_JoinMerge_OuterColumn_NotDropped`, `SELECT *`/flow-through,
   ambiguous-duplicate-name E2E.

**Status:** Option B approved by Graefe (consult); implementation starting from a clean branch off the
merged 7.5. Bring the implementation back for the impl-side Graefe ACK.
