# RFC-077: Source-anchored join result + structural interning (holistic 7.5 + 7.6)

**Status:** Draft
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
5. **Consumer migration (Torvalds' concern).** Audit the runtime readers (`cascades_generator.go`
   column derivation, `executor.go mergeRows`, `streaming_cursors.go`): where the RC survives to
   runtime, they read the anchored RC's column-named keys instead of bare+`ALIAS.COL`. The
   name-keyed `Evaluate` keeps this a key-naming alignment, not an ordinal rewrite.

## Performance

No wire change (read-side only). Same plans; the anchored RC interns better than the opaque merge
(structural identity), so re-enumeration sub-product sharing improves or holds. plandiff
byte-identical at every arity; stress-1M within noise.

## Test plan

- **Anchored-value unit tests**: `field(RC(anchored cols), col)` simplifies to the leg FieldValue;
  the RC's `Evaluate` yields the correct column-named row; two equal anchored RCs intern (structural,
  no string alias).
- **Interning regression (7.5)**: re-enumerated sub-products over the same leg-set intern to one
  Reference WITHOUT `mergeQuantifierAlias` (the string scheme deleted).
- **Join correctness E2E**: the full FDB join suite green — multi-way chain + star, middle-table
  projection (`TestFDB_MultiwayJoinOrder_Nway`), the outer-column sentinel
  (`TestFDB_JoinMerge_OuterColumn_NotDropped`), `SELECT *` over joins.
- **No regression**: plandiff conformance green; determinism 10×; full `just test`; stress-1M.

## Risk / honesty

This is the largest Phase-7 change (it touches the translator, `PartitionSelectRule`, the value
package, the simplifier, and the runtime readers). The name-keyed design (not ordinal) keeps the
blast radius to *key-naming alignment + value-type swap*, not an executor data-model rewrite — but
the consumer audit (step 5) is the load-bearing risk: any reader still expecting bare+`ALIAS.COL`
keys must move to the anchored keys, or it silently reads nil. The E2E join suite + the
outer-column sentinel are the safety net; each consumer is migrated + pinned before the opaque
types are deleted. Done as its own focused PR (separate from RFC-076 / 7.7).
