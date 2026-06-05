# RFC-077: Source-anchored join result + structural interning (holistic 7.5 + 7.6)

**Status:** Accepted (Graefe ACK + Torvalds ACK; step 5 corrected per Torvalds — the merge value is read at PLAN time, not by runtime `Evaluate`; Graefe's E2E conditions folded into the test plan)
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

### Revised sequence (consumer-migrate-before-delete, each plandiff-verified)

1. Add the anchored-RC builder (from leg result types, F1) + RC correlation handling (F2-i) +
   `composeFieldOverConstructor` name-miss guard test — no call-site change yet.
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
