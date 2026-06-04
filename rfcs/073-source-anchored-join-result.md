# RFC-073: Source-anchored join result — retire `JoinMergeResultValue` / `composeFieldOverJoinMerge` (TODO 7.6)

**Status:** Deferred (RFC review: gated on 7.5 + larger blast radius than scoped; no correctness gain)
**Area:** Cascades query planner — join result value + field pull-up (translator, value simplifier, NLJ/partition-select rules, FlatMap execution)
**Reviewers:** Graefe (Cascades alignment — mandatory), Torvalds (code quality), codex, @claude

## Review outcome (do not implement as written)

Graefe **ACK on the diagnosis + core fix** (anchored `RecordConstructorValue` of
`FieldValue(QOV(legAlias), col)`, resolved by `composeFieldOverConstructor`, is the Java-faithful
fix) **but with a blocking sequencing condition**, and Torvalds **NAK on scope**. Both converge:

1. **End state must be pure anchored access** (no runtime qualified-key map). Keeping the
   bare+`ALIAS.COL` merged map (option a) just relocates the band-aid into
   `RecordConstructorValue.Evaluate` — and worse, Torvalds notes RC.Evaluate produces a *nested
   map keyed by column name only* (`values.go` `out[f.Name]=…`), a **different shape** from the
   flat dotted-key map every consumer reads. So P1's "byte-identical legacy row shape AND
   anchored RC" is self-contradictory: the consumers must move to anchored access, which is the
   big change — not a thin equivalence shim.
2. **Coupled to 7.5 (RFC-043 alias-merge interning), which is gated on the ≥5-way enumeration
   work.** The N-ary `JoinMergeAllValue` + `rule_partition_select.go` re-enumeration (~10 sites
   re-stamp/intern it; `semantic_hash.go`, `semantic_equals.go`, `map_field_values.go`,
   `value_correlation.go` key equality/hashing on these types) is the same opaque convention.
   Anchoring only the binary join while the N-ary twin stays opaque is a "split brain." Graefe:
   **order 7.5 before 7.6's retirement, or fold N-way anchoring in and gate on 7.5.**
3. **Additional consumers the Fix understated:** `cascades_generator.go:1890` reaches into
   `JoinMergeResultValue` fields directly; `executor.go:1434 mergeRows` + `streaming_cursors.go`
   encode the inner-wins-bare convention independently of the Value type. All must be ported, not
   "simplified," before any delete.
4. **No correctness gain (no live bug).** Torvalds: the principle-#10 win does not justify a
   plan+execution+interning blast radius **now**; defer unless the join translator is being
   touched anyway. Test gate must cover the N-way re-enumeration + interned-equality stability +
   self-join alias collision + ≥3-way plandiff, not just the binary path.

**Disposition:** Deferred behind 7.5 (→ ≥5-way enumeration). This RFC stands as the design + the
consumer map for when that prerequisite lands. The fix below is the intended *direction*; the
"Fix"/phasing sections are superseded by the sequencing above (do 7.5/N-way anchoring first; end
state = pure anchored access).

---


## Problem

A join's result value in Go is an **opaque** `JoinMergeResultValue{OuterAlias, InnerAlias}`
(and its N-ary sibling `JoinMergeAllValue`), built directly by the translator
(`cascades_translator.go:396,657,767`). It carries no per-field source; at runtime
`Evaluate` merges the two leg bindings into a map with both **bare** (`col`) and
**qualified** (`ALIAS.COL`) keys.

Because the result is opaque, a field pulled up through a join arrives as a **bare**
`FieldValue{Field, Child:nil}` with no owning-quantifier anchor. `composeFieldOverJoinMerge`
(`values/simplifier_value.go:228`) re-anchors it after the fact by **hard-coding the inner
side**: `FieldValue(JoinMergeResultValue, "x")` → `FieldValue(QOV(InnerAlias), "x")`. This is
sound *only* by an RFC-044 invariant (SelectMergeRule re-flows the merge under the inner
alias, so only inner-side bare fields reach the rule; outer fields arrive already
`ALIAS.`-qualified) plus a qualified-field fail-safe and an E2E sentinel
(`TestFDB_JoinMerge_OuterColumn_NotDropped`). **No live bug** — but it violates principle #10
(match the architectural property, not a downstream observable): the true fact "field `x`
belongs to leg L" is known when the join result is built, dropped, then *guessed* back from a
hard-coded assumption. The moment the invariant shifts, the guess silently picks the wrong leg.

The `bareColumnName` string-parsing in `rule_implement_nested_loop_join.go:1085` and the whole
bare-vs-`ALIAS.COL` FieldValue distinction exist for the same reason.

## Investigation (Java reference)

Java never has a bare join field. `Quantifier.pullUpResultColumns()`
(`Quantifier.java:747`) builds a join's result by anchoring each leg's fields to its source:
`FieldValue.ofOrdinalNumber(QuantifiedObjectValue.of(alias, recordType), i)`, and
`GraphExpansion.buildSelectExpression()` wraps them in `RecordConstructorValue.ofColumns(...)`.
Java's `FieldValue` *requires* a non-null `childValue` (always a `QuantifiedObjectValue` in the
join path). Field access above the join is resolved by `ComposeFieldValueOverRecordConstructorRule`
(`field(RC(.., x as name, ..), name) → x`), preserving `x`'s internal anchor. So the source is a
structural property of the value, never re-derived.

Go already has the pieces: `FieldValue.Child` is exactly the anchor slot (`nil` = legacy flat),
and `composeFieldOverConstructor` (`simplifier_value.go:247`) is the port of Java's compose rule.

## Fix

Build the join result the Java way and let the existing machinery resolve it:

1. **Translator** (`cascades_translator.go`): replace `NewJoinMergeResultValue(outer, inner)` (and
   the `JoinMergeAllValue` re-enumeration path) with a `RecordConstructorValue` whose columns are
   `FieldValue(QuantifiedObjectValue(legAlias), col)` — one per leg, named with the existing
   `ALIAS.COL` scheme so downstream references resolve unchanged.
2. **Field access above the join** composes through the new RecordConstructor via the existing
   `composeFieldOverConstructor` → a directly-anchored `FieldValue(QOV(legAlias), col)`. No guess.
3. **Retire**: `composeFieldOverJoinMerge`, `JoinMergeResultValue`, `JoinMergeAllValue`, the
   `bare`-field path (`pullUpThroughPassthrough` now anchors: `FieldValue{Child: QOV(alias)}`),
   and the `bareColumnName` string-parsing (becomes "return `fv.Field`" since `Child` is always set).

### Execution equivalence — the crux

The FlatMap executor produces each join row via `resultValue.Evaluate(nestedCtx)` where
`nestedCtx` binds each leg's alias (`flat_map_cursor.go:213`). `JoinMergeResultValue.Evaluate`
emits a map keyed by **both** bare and `ALIAS.COL` forms; **every** downstream consumer relies on
that shape: the projection/`Map`, WHERE-above-join, ORDER BY, partition-select re-enumeration
`mergeRows`, and NLJ probe rebasing. The migration must preserve the consumed row shape. Two
sub-questions to settle in implementation (and with Graefe):
- Does `RecordConstructorValue.Evaluate` over the anchored columns reproduce the same
  bare+qualified key set, or do the downstream consumers move to *anchored field access*
  (`FieldValue(QOV(legAlias), col)` reading the per-leg binding directly), making the
  merged-map qualified-key convention unnecessary? Java does the latter; the cleanest end state
  is to follow it, but it touches more consumers.
- `mergeRows` (re-enumeration) and the NLJ probe path currently encode the inner-wins-bare-key
  convention — both simplify once fields are anchored, but each must be verified, not assumed.

### Phasing (proposed)

This is cross-layer and "not urgent," so land it in reviewable phases rather than one mega-diff:
- **P1 — anchored result, equivalence-preserving.** Translator emits the anchored RecordConstructor;
  `RecordConstructorValue.Evaluate` reproduces the exact row shape `JoinMergeResultValue` produced
  (bare+qualified keys). Prove byte-for-byte row equivalence + plandiff-clean before removing
  anything. `JoinMergeResultValue`/`composeFieldOverJoinMerge` stay but become unreachable.
- **P2 — retire the band-aids.** Delete `composeFieldOverJoinMerge`, `JoinMergeResultValue`,
  `JoinMergeAllValue`, and the bare/qualified distinction once P1 proves nothing produces a bare
  join field; simplify `bareColumnName`, `pullUpThroughPassthrough`.

(Graefe to confirm phasing vs. atomic, and whether the end state should drop the qualified-key
merged map entirely in favor of pure anchored access.)

## Performance

No execution-cost change intended — same FlatMap/NLJ plans, same per-row work (a RecordConstructor
Evaluate vs. the merge Evaluate). Removes planner-side simplifier work (one fewer compose rule) and
the `bareColumnName` string parsing on the NLJ rebase path. stress-1M must be unchanged
(before/after, per the planner-change protocol).

## Test plan

- Existing sentinels stay green: `TestFDB_JoinMerge_OuterColumn_NotDropped`, the plandiff
  conformance suite (no plan-shape regression), `just test` 48/48, determinism 10×.
- New: column-collision joins (`a.x` + `b.x`), 3+-way re-enumerated joins (the `JoinMergeAllValue`
  path), WHERE/ORDER BY referencing both legs by qualified + bare name, self-join (same table both
  legs) — each asserting correct columns end-to-end and the anchored plan shape (no
  `JoinMergeResultValue` / no bare join field in the final plan).
- P1 equivalence: a property test (or differential vs. the retained `JoinMergeResultValue.Evaluate`)
  proving the anchored RecordConstructor yields an identical row map across random join shapes,
  before P2 deletes the old path.
- stress-1M before/after within noise.
