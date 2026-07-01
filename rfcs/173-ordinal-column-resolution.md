# RFC-173: Migrate join column resolution from name-based `AnchoredJoin` to Java's ordinal/group model

**Status:** ACKED THROUGH ROUND 4 (Graefe ✅ · Torvalds ✅ · codex ✅ · @claude ✅; see §10 review
log); **Round 5 (adversarial content re-review, 2026-07-01) folded into this revision — re-ack
in progress, state tracked in the §10 Round-5 checklist (the single source of truth); Slice 2
must not start until all four ack.** Progress: P1 merged (#423), P2 merged (#427),
P3 folded into Slice 3 (#429/#430), Slice 1 in flight (`feat/rfc173-slice1-ordinal-nonjoin` —
Step 2b blocked on the buried-reference precursor, Graefe-acked 7-site plan, recorded in that
branch's §4 Slice 1 log). Each staged PR re-acked on its own HEAD.
**Origin:** RFC-164 WS-2 (correlation-completeness). PR #420 proved the WS-2 invariant is
*blocked* on a root architectural divergence: Go resolves join columns **by name**, Java by
**(quantifier, field ordinal)**. This RFC is the root fix.
**Process (packaging — ADOPTED; owner may override):** ONE RFC (this document, of record).
Implementation lands as **staged merged PRs**, not one long-lived branch — resolving Torvalds'
NAK (a single ~25–30-shift branch rots against the churning memo and forces repeated Graefe
re-acks). The behaviour-preserving precursors (**P1, P2, P3, Slice 1**) each merge to master
independently (green + reviewed); the genuinely-atomic **Slice 3** lands as its own PR; the
remaining slices group by coherence (Slice 2's wedge with its boundary adapter; Slice 4's
deletions; Slice 5; Slice 6). Every PR is tracked to this one RFC and re-acked as it lands.
"One RFC" is preserved; the single-PR literal is dropped because it is the shape Torvalds showed
is actively harmful. (Owner asked for one PR; this adopts staged merged PRs per the reviewer NAK —
override if the literal single PR is required, noting that leaves Torvalds' NAK standing.)
**Cross-refs:** RFC-164 (port-fidelity), RFC-077 (join interning / CTE column-rename),
RFC-142 (lateral `UNNEST` + `WITH ORDINALITY`), RFC-036 (outer joins), RFC-081 (UNION-by-position).
**Paths:** executor references (`executor.go`, `executor_new_plans.go`, `flat_map_cursor.go`,
`streaming_cursors.go`) are under `pkg/recordlayer/query/executor/`; planner/value references under
`pkg/recordlayer/query/plan/cascades/` (`values/`, `expressions/`, `predicates/` subpackages).
Two exceptions: `cascades_translator.go` is under `pkg/relational/core/query/`,
`cascades_generator.go` under `pkg/relational/core/embedded/`.
**Effort (honest):** foundational — **~25–30 focused shifts** across 9 slices (the `FieldValue`
nil-`Child` leaf form + ~105 `FieldValue` sites, the resolver still emitting dotted names, and the
`OrdinalFieldName _0/_1` emulation make P1 heavier than a single shift), with a dual-representation
window that must not be left parked mid-flight.

---

## 1. Problem

This is a Java→Go port where wire/behaviour compatibility is the whole point, and the query
engine is a 1:1 port of Java Cascades. In one load-bearing place it is **not** a faithful port:
**how columns are resolved across a join.**

- **Java** resolves a column reference as `FieldValue(childValue, FieldPath)`, where `FieldPath`
  carries the **ordinal** position of the field in the child's `Type.Record`. A join's output
  `Type` is the structural concatenation of its quantifiers' `rangesOver` types; a buried leg
  column is `(quantifier ordinal in the group, field ordinal in the leg type)`. Resolution is
  positional. Correlations are genuine. The final top-level plan is **strictly correlation-closed
  by construction** (`getCorrelatedTo() == ∅`, no `__const` exception).
- **Go** resolves join columns **by name, end to end**. A join's result value is a
  `RecordConstructorValue` whose fields are keyed by upper-cased dotted `ALIAS.COL` strings (plus
  bare `COL` duplicates, last-leg-wins, plus dotted-verbatim keys for nested legs) —
  `values/value_anchored_join_record.go:54-99`, tagged by a single bool
  `RecordConstructorValue.AnchoredJoin` (`values.go:2349`). `FieldValue` stores no ordinal
  (`Field string` + `Child` — `values.go:185-189`; P1's dark `resolveOrdinal`,
  `values.go:340-353`, derives one on demand but name stays authoritative). At execution the join
  emits a `map[string]any` row keyed by that same bare+`ALIAS.COL`+`TYPE.COL` set
  (`executor.go` `mergeRows:2019-2081`, `qualifyAlias:2082`), and `FieldValue.Evaluate`
  resolves by string map lookup (`values.go:210-325`). **Node identity is name-based too:** memo
  interning compares `FieldValue`s by `av.Field == bv.Field` (`map_field_values.go:260-262`) and
  hashes `"field:"+Field` (`semantic_hash.go:108`).

### 1.1 Why this is the "cheap implementation" to retire

The name-based model is not a considered end-state — the codebase itself says so:

> `select.go:241-242`: widening alias-aware interning "is gated on migrating Go's column
> resolution to Java's ordinal/group model."
> `RFC-077:333`: same statement.

Everything downstream is scaffolding around one string contract, and it costs us:

1. **Non-closed plans / blocked WS-2 invariant.** Because a projection over a join references a
   *buried* leg alias resolved by name (`Project([A.ID], FlatMap(outer=Scan(A), …))`), the
   final plan reports free correlations (`{A}`) over a closed input. Java's plan is closed.
   RFC-164 WS-2's correlation-completeness invariant false-positives on **every** real
   join/IN/partition query for exactly this reason (PR #420) and cannot be made always-on until
   the plans are closed.
2. **An exploration-hiding / partition-re-exposure correlation duality** exists *only* because the
   anchored RC self-binds leg QOVs that name-resolution must hide from the global correlation
   order yet re-expose for predicate/quantifier classification (hiding: the `AnchoredJoin` skip
   inside `GetCorrelatedToOfValue`, `value_correlation.go:96-98`; re-exposure:
   `GetCorrelatedToOfAnchoredJoinLegs`, `value_correlation.go:132`,
   `predicates.AddMergeSeedAliases`, `rule_partition_select.go` `quantifierMergeSeedLegDeps`).
   Pure accidental complexity.
3. **It fights ANSI SQL.** ANSI column correspondence is scope-and-name with *positional*
   disambiguation — `UNION`/`INTERSECT`/`EXCEPT` match columns **by position, not name**;
   `JOIN … USING`/`NATURAL` coalesce; derived tables rename (`FROM (…) AS t(a,b)`); duplicate
   unqualified names are legal. A name-flattened model handles "distinct names joined by name"
   and needs a **special-case for every other ANSI rule**. The ordinal/group model represents
   all of it natively. Since long-term ANSI compatibility is a project goal, the clean-Java core
   and the ANSI-sound foundation are the **same** decision.
4. **An operator allowlist trap.** `producesMergedRows` / `bindAlias` suppression
   (`executor_new_plans.go:312-358`) is a hand-maintained set of "operators that emit merged
   rows." Any new merged-row operator (hash join, merge join) must be added by hand or it
   silently mis-resolves.
5. **The burial is not join-only (Round-5 correction to §1's framing).** The same
   see-through-the-alias divergence lives on the *single-quantifier* frontier: derived-table /
   recursive-CTE resolution (`ColumnAliasMap`) rewrites an output-column reference back to its
   *source* column, which the tolerant name map absorbs (the executor double-writes source and
   alias keys) and the ordinal model correctly rejects. Slice 1's Step 2b blocker found this
   empirically (~15 derived-table/recursive-CTE tests; Graefe-acked 7-site precursor, recorded on
   the Slice 1 branch). Consequence: the burial sites must be **enumerated up front, not
   discovered one blocker at a time** — §4 carries a name-burial inventory as a Slice 2 entry
   gate.

### 1.2 What we are NOT doing

Not the band-aid. RFC-164 WS-2's analysis surfaced a tempting "surgical" option: keep
`AnchoredJoin` and add an implementation-layer rule that folds `Project`-over-`FlatMap` into a
single `FlatMap`, closing the correlation *symptom* while leaving the name model underneath.
That is **doubly cheap** — it entrenches the model we want to retire and stacks a compensating
layer on top. Rejected as an end-state (see §9).

---

## 2. The knot: the executor row model is the critical path

The migration's hardest, non-decomposable core is **not** the planner — it is the runtime row
representation, and it forces everything else:

- `FieldValue.Field` is a bare string; the stored form has no ordinal (`values.go:185-189`).
- The merged join row is a `map[string]any` keyed by upper `ALIAS.COL`/`COL`/`TYPE.COL`
  (`executor.go` `mergeRows:2019-2081`).
- `FieldValue.Evaluate` does pure string map lookups (`values.go:210-325`).
- `FieldValue` **node identity** is the name: `EqualsWithoutChildren` compares `Field`
  (`map_field_values.go:260-262`) and the semantic hash is `"field:"+Field`
  (`semantic_hash.go:108`). Java's identity is ordinal-only (§3).
- The planner's anchored RC is *explicitly specified* to emit byte-for-byte the key set
  `mergeRows` physically writes (`value_anchored_join_record.go:22-53`).

You cannot move the planner to ordinal/group without simultaneously replacing **(a)** the
execution row (name-keyed map → positional/typed tuple), **(b)** `FieldValue` resolution
(name lookup → `FieldPath` ordinal against the input `Type`), and **(c)** `FieldValue` node
identity (name equality/hash → ordinal — or the memo conflates duplicate-named columns the
moment they can coexist; scheduled in Slice 3). And because the memo
**re-enumerates all joins at once**, the N-way flip cannot be sub-divided by arity beyond a
2-way wedge — the positional row, ordinal `FieldValue`, and alias-bijection interning must flip
**together, atomically** (Slice 3). This is why the migration must be **staged with dark,
shadow-built precursors proven first**, not a big-bang.

---

## 3. Destination (Java, tag 4.12.11.0)

- Column reference: `FieldValue(childValue, FieldPath)`; `FieldPath` = list of `ResolvedAccessor`
  carrying the ordinal in the child `Type` (`values/FieldValue.java`, `Type.java:2249-2311`).
- Identity: `ResolvedAccessor.equals`/`hashCode` compare the **ordinal only**
  (`FieldValue.java:676-690`) — the display name is not semantic. Two same-named columns at
  different ordinals are distinct; a rename does not change identity.
- Nested references stay **collapsed**: one `FieldValue` with a multi-accessor `FieldPath`;
  `FieldValue`-over-`FieldValue` is folded by
  `simplification/ComposeFieldValueOverFieldValueRule.java`. There is no chained-node form in the
  memo.
- Join output: structural `Type` = concatenation of the quantifiers' `rangesOver` types; a leg
  column is `(quantifier ordinal, field ordinal)`.
- Re-enumeration: `PartitionSelectRule` rebuilds result values via `TranslationMap` over
  quantifier ordinals — **not** by re-deriving dotted string keys.
- Interning: members dedup alias-aware **globally** via bijective `AliasMap` enumeration at
  `Reference.containsInMemo` (`Reference.java:996-1019`, `RelationalExpression.java:295-370`) —
  no name hazard, no `AnchoredJoin` special-case gate.
- Closure: `computeCorrelatedTo` subtracts locally-bound quantifier aliases when `canCorrelate`
  (`AbstractRelationalExpressionWithChildren.java:56-77`) — a buried column is a *real*
  `FieldValue` path with a *real* child correlation, so correlations are genuine and the top
  plan is closed by construction.

---

## 4. Staged plan — 3 dark precursors + 6 slices (staged merged PRs, one RFC)

Each precursor/slice ships green and is independently reviewable. Precursors ship **dark**
(computed but non-authoritative) and are certified by **execution-based pins** (see §5 — the
validation strategy the adversarial review corrected). Effort figures are rough.

### Precursors (dark, non-authoritative)

- **P1 — Ordinal `FieldPath` substrate on `FieldValue`** (~1 shift). Add a real `FieldPath`
  (`[]ResolvedAccessor` = ordinal + display name) alongside the bare `Field` string; implement
  positional evaluation against the child `Type.Record` and a `resolveFieldPath` name→ordinal
  derivation. **Name lookup stays authoritative.** `equals`/`hashCode` stay name-based for now
  (flipping early changes interning identity before P3 is ready). Hard part: the nil-`Child` leaf
  form has no child `Type` — thread `Type.Record` to construction sites or keep leaves on the
  name path.
  **AS LANDED (#423 — delta from this spec, recorded per Round 5):** no stored
  `[]ResolvedAccessor` — the ordinal is *derived on demand*: `resolveOrdinal()`
  (`values.go:340-353`) reads `Child.Type().(*RecordType).FieldIndex(f.Field)`; `NewRecordType`
  normalises `Fields[i].Ordinal == i`. Lazy derivation is sound **only on the non-join frontier**,
  where nothing re-types a `FieldValue`'s child between plan-finalize and eval (Graefe's
  load-bearing invariant, recorded on the Slice 1 branch); Slices 2–3 build merged-row references
  with **eager** `ofOrdinalNumber` baking, and Slice 3 carries the representation ruling for
  nested/buried paths (see Slice 3). The "for now" on `equals`/`hashCode` now has an owner:
  **Slice 3** (see the node-identity flip there).
- **P2 — Positional/typed runtime row in the executor** (~2 shifts, heaviest precursor). The
  NON-JOIN row producers (scans, index scans, covering index, projections) emit a typed positional
  row **alongside** the `map[string]any`; consumers still read the map; filters pass it through
  unchanged. **Scope note (gauntlet-agreed, PR #427):** the JOIN/lateral producers (`mergeRows`,
  `qualifyOuterRow`/`flatmap`, `explode`) and the outer-join **positional null-extension** primitive
  (`appendNullLeg` — the sound replacement for null-key-absence that kills the LEFT-JOIN
  bare-resolve hazard at `executor_new_plans.go:346-358`) move to **Slice 2/3**, which restructures
  those producers positional-native and consumes the primitive. Dual-emitting a positional row over
  the AnchoredJoin merge in P2 would be throwaway work Slice 3 deletes: "wire the mirror where it's
  a mirror; rewrite the join where it's a rewrite" (Graefe). Hard part: wide blast radius; dual
  emission doubles per-row materialization cost for the migration window — must be measured and
  bounded (**benchmark deferred to Slice 1**, when the ordinal path first goes live).
- **P3 — Alias-bijection structural interning** (~1.5 shifts). Implement `findMatches` over
  bijective `AliasMap`s at `Reference.Insert/InsertFinal`, extending the existing
  `SemanticEqualsUnderAliasMap`/`MemoEqual` machinery to Java's `containsInMemo` semantics. Runs
  **dark**. Hard part: prune the bijection enumeration by correlation/type as Java does, or
  interning gets expensive; certify it does **not** reintroduce the CTE column-rename NULL-read.
  **FOLDED INTO SLICE 3** (gauntlet call, PR #429: Graefe + Torvalds + codex ACK-with-fold; @claude
  n/a). The dark-shadow spike (a nil-in-prod `InternShadowObserver` hook at `Reference.Insert` +
  corpus measurement) proved the mechanism and quantified the win, but is transitional scaffolding
  deleted at the flip — it must land **with its Slice 3 consumer**, not stranded N shifts ahead.
  **Analysis banked for Slice 3:**
  - *Mechanism verified:* the shadow re-runs tier-3's exact predicate
    (`HashCodeWithoutChildren()==eHash && MemoEqual(m,e)`) minus the `aliasAware` gate, scoped to
    `!aliasAware`, so a `would=true` is precisely the extra dedup the global-bijection flip newly
    authorizes.
  - *Magnitude:* ≈259 extra dedups over 1500 planned fuzz corpus expressions (9391 non-opted-in
    Inserts). **Approximate, and an under-count** — it shadows `Insert` only, not `InsertFinal`, and
    live dedup mutates the member set later comparisons see (Graefe). Treat as an order-of-magnitude
    "before" baseline, not a pinned number.
  - *Slice 3 MUST assert the delta.* The spike's corpus test only `t.Logf`s 259 and fails on
    `observed==0` — an unasserted log, not a tracked measurement (Torvalds). The assertion that
    matters — shadow-predicted delta == the flip's *actual* member-count delta — is exactly what the
    spike omits; Slice 3 carries it as its dark→live equivalence pin.
  - *Safety is flip-live-gated, not shadowable.* The flip collapses two members differing only by an
    alias bijection, discarding one; anything resolving the discarded member's aliases **by identity**
    (the name model) is orphaned. The shadow counts the collapse but never exercises it, so the only
    thing that could break — external by-name resolution — is never touched. Certification is
    **staged** (Round-5 correction — the canonical interning sequence in Slice 3 supersedes older
    phrasings): the RFC-077 task-count baseline + the STAR planning wall-clock pin gate **Slice 3**
    (bijection tier built, authoritative for merge selects); the CTE-rename execution pins certify at
    **Slice 4**, when `InternsAliasAware` widens to all selects — they need the widened gate, not the
    merge-select flip. Spike code preserved on `feat/rfc173-p3-bijection-interning` for Slice 3 to
    reuse as its "before" harness.

### Slices

- **Slice 1 — Flip non-join resolution to ordinal** (~1 shift). Single-table
  scans/filters/projections/sorts (no merged row) make P1+P2 authoritative and retire the name
  map on that frontier. `AnchoredJoin` untouched. Reuse the inverted `producesMergedRows` test to
  find the safe frontier. Verify `UNION`/set-op (already positional,
  `remapUnionColumnsByPosition`) rides the ordinal row unchanged. **Exit obligation (Round 5):**
  run the dual-emission per-row cost benchmark deferred from P2 (§4 P2 scope note; §8 risk 5)
  before the ordinal path goes live.
- **Slice 2 — 2-way join ordinal output (the wedge)** (~2 shifts, floor). A 2-way join has exactly
  one bipartition, so `NewReEnumerationAnchoredRecord` **never fires** — only the seed matters
  (verified: `rule_partition_select.go:48` returns on <3 quantifiers; outer joins are always
  binary, `cascades_translator.go:3367`). Build the 2-way result value as the ordinal
  concatenation of the two legs' types (`FieldValue.ofOrdinalNumber(QOV(leg), i)`); executor emits
  the positional merged row; predicates resolve by `(quantifier ordinal, field ordinal)`.
  **Interning does NOT flip here** (Round-5 correction; supersedes the earlier "flip 2-way seed
  interning" phrasing) — the bijection tier lands whole in Slice 3 with the folded P3, per the
  canonical interning sequence there; Slice 2 joins keep name-model dedup. The wedge therefore
  proves the ordinal **result value + positional row + ordinal predicate resolution** on live join
  plans — not interning; that residual risk moves to Slice 3, mitigated by the banked spike
  harness. Port the correlated-scalar-subquery 2-leg seed and single-source `UNNEST` here.
  **Entry gate — name-burial inventory (≤1 shift, mandatory, Round 5):** before any Slice 2 code,
  enumerate every name-keyed row producer/consumer and alias-swap site and slot each into a slice.
  The Step 2b blocker proved these must be mapped up front, not discovered mid-slice. Known
  candidates to sweep: `projNames` source+alias double-writes, `qualifyTypeFallback`,
  `ambiguousColumnMarker`, the union name-recovery gates, the `RowEvalContext` param/subquery
  frontier, `ColumnAliasMap` (already being retired by the Slice 1 precursor).
  **Coexistence scoping (the corrected hard part, Round 5):** the ordinal↔name boundary is NOT
  just a row-format adapter. The name model *classifies leg dependencies by dotted-name prefixes*
  (`MergeSeedLegsOfValue` reads `fv.Field[:dot]`, `value_correlation.go:47`;
  `AddMergeSeedAliases`, `predicates/predicate_correlation.go:69`; `quantifierMergeSeedLegDeps`,
  `rule_partition_select.go:728`) — an ordinal 2-way leg nested under a name-model 3+-way merge
  emits references with **no dotted prefix and genuine (unhidden) correlations**, which those
  classifiers silently mis-handle (wrong bipartition validity, wrong predicate placement), a
  failure the row adapter cannot see. Ruling: **scope the ordinal path to 2-way joins that are
  not consumed as a leg of a name-model merge select** — and the gate must be
  **flattening-aware** (Graefe condition, Round 5): a naive translation-time arity check on the
  enclosing select is evadable, because `SelectMergeRule` (`rule_select_merge.go`) flattens
  inner-join-equivalent boxes during exploration — `FROM (a JOIN b) t1, (c JOIN d) t2` is 2-way
  at translation and 3+-way post-flattening. Gate on the **post-flattening arity of the
  transitive inner-join-equivalent cluster** (computable at translation by walking the cluster),
  or — if that proves fragile — make ordinal 2-way selects a **merge barrier** for the
  coexistence window (`SelectMergeRule` declines to flatten through an ordinal select). Mixed
  nesting stays name-model until Slice 3 flips N-way. The adapter then only bridges row format
  at the subquery/scan boundary; correlation-semantic bridging is explicitly out of scope — that
  hazard is *why* the gate exists, and the gate carries pins: (a) a 2-way join under a 3-way
  join must plan and execute name-model-identically before/after Slice 2, and (b) the flattening
  evasion shape (`FROM (a JOIN b) t1, (c JOIN d) t2`) must stay name-model end-to-end during the
  window.
- **Slice 3 — THE HARD CORE: N-way re-enumeration + interning, ordinal/group (ATOMIC)**
  (~3 shifts). Replace the name-based re-stamp machinery
  (`NewReEnumerationAnchoredRecord`/`anchoredColumnsByQuantifier`/`leftmostQOV`/`buildUpperResult`/
  `rebaseBuriedLowerReferences`) with positional rebuilds: `pullUpResultColumns` over the merge
  quantifier's flowed `Type` + a `TranslationMap` rebasing a buried leg reference to a `FieldValue`
  **ordinal path** (not string concatenation). Make alias-bijection interning authoritative for
  merge selects and delete name-sorted-RC identity dedup. Make the N-way positional merged row
  authoritative. Delete the two fail-loud re-stamp panics (an ordinal rebuild cannot fail to find a
  leg). **Atomic because the memo re-enumerates all joins** — P1/P2/P3 must be authoritative
  together. Hard part: RFC-142 multi-source lateral `UNNEST` bipartition-validity is a **from-scratch
  rewrite** (recover the buried source from the `FieldValue`'s real child correlation, not a dotted
  `'A.ARR'` prefix), and its safety rests entirely on P2/P3 being proven first.
  **Also atomic in this slice (Round-5 additions):**
  - **`FieldValue` node-identity flip (name → ordinal).** Java compares accessors by ordinal ONLY
    (`ResolvedAccessor.equals`/`hashCode`, `FieldValue.java:676-690`); Go compares by name
    (`EqualsWithoutChildren`, `map_field_values.go:260-262`; `semantic_hash.go:108`). P1
    deliberately deferred the flip ("for now") — Slice 3, which owns the interning flip, is the
    owner. It CANNOT slip past Slice 4: the moment duplicate bare names coexist positionally
    (§7's `SELECT *` fix), name-based identity conflates two genuinely different columns into one
    memo member → wrong plans. Today that failure is *unconstructible* (the name model dedups
    names first), which is exactly why no existing pin covers it; §5 adds the duplicate-name
    identity pin.
  - **Canonical interning sequence (single source of truth; supersedes all older phrasings):**
    **Slice 2** — no interning change (name-model dedup everywhere). **Slice 3** — bijection tier
    built (folded P3), authoritative for merge selects; gated by the task-count baseline, the STAR
    planning wall-clock pin, and the shadow-delta assertion. **Slice 4** — `InternsAliasAware`
    widened to ALL selects, gate deleted; CTE column-rename execution pins certify here.
  - **Representation ruling — nested/buried ordinal paths.** Java holds ONE `FieldValue` with a
    multi-accessor `FieldPath` and folds `FieldValue`-over-`FieldValue`
    (`ComposeFieldValueOverFieldValueRule.java`); Go's as-landed P1 derives ONE ordinal per node,
    so a buried leg reference would naïvely be a **chained** node pair
    (`FieldValue(FieldValue(QOV, legOrdinal), fieldOrdinal)`) — a different tree shape than the
    spec, visible to every ported rule pattern, simplification, and semantic compare. Ruling
    (default; Graefe confirms at Slice 3 start): **port Java's collapsed form** — store the
    multi-accessor path and port the compose rule — rather than enshrine chained nodes as a
    permanent divergence. Decided explicitly here, not implicitly by whoever writes the
    `TranslationMap` rebase.
- **Slice 4 — Retire `AnchoredJoin` (deletions)** (~2 shifts). Delete
  `value_anchored_join_record.go` entirely; delete `RecordConstructorValue.AnchoredJoin` and its
  preservation through `WithChildren`/`Replace`/simplifier/`Equals`/`semantic_hash`; delete the
  executor's bare/`ALIAS.COL`/`TYPE.COL` key writing and `qualifyAlias`/`qualifyTypeFallback`;
  delete `producesMergedRows`/`bindAlias` suppression (the operator allowlist trap); widen
  `InternsAliasAware` to **all** selects and delete the gate (`select.go:221-256`); delete the
  fake `_<ordinal>` `OrdinalFieldName`; fold the `LogicalProjection` that used to stack over the
  join. **Observable change:** `SELECT *` last-leg-wins bare-name collision is **fixed** (both
  duplicated columns coexist positionally) — a deliberate correctness improvement that moves
  goldens (§7). Hard part: output column order/reversal (`cascades_generator.go:2733-2876`) must
  now derive from result-value `Type` ordinals.
- **Slice 5 — Correlation-closure invariant always-on** (~1.5 shifts). Delete the
  exploration-hiding / re-exposure duality (§1.1 item 2). Make `computeCorrelatedTo` subtract
  locally-bound aliases when `canCorrelate` (Java parity). **Now** turn RFC-164 WS-2's
  correlation-completeness invariant always-on — it holds by construction. Hard part: confirm the
  genuine closure's local-bind subtraction is *exactly* Java's so the ≥4-way STAR correlation
  order does not reinflate past the task budget (the concern that motivated the hiding).
- **Slice 6 — Re-home extensions positionally + open ANSI headroom** (~1.5 shifts). Each Go-only
  extension proven sound on the ordinal substrate before its name path is deleted (see §6). Delete
  residual workarounds (`NextMergeAlias` plan-hash-stability hack, `ambiguousColumnMarker`
  sentinel, union name-recovery gates). Open — not necessarily implement — the now-native ANSI
  headroom: `JOIN USING`/`NATURAL`, derived-table `t(a,b)` renaming, positional set-op coercion,
  `INTERSECT`/`EXCEPT`.

---

## 5. Validation strategy (CORRECTED — the adversarial review's key catch)

**The naive "shadow-validate green by proving the dark precursors make *identical* decisions to
the name model, then flip" gate is self-defeating and must NOT be the safety mechanism.** The
failure classes that *motivate* the migration — CTE column-rename NULL-reads, RFC-142
buried-source, interning collapse under global bijection, `SELECT *` last-leg-wins — are
**plan-structure** changes. They do not exist to shadow-diff on today's (all name-based) plans;
the whole point is that the two models **must differ** on exactly those cases. A gate that
requires "identical decisions" can never go green on the cases that justify the work, and where
it is forced green it certifies nothing.

Safety is therefore established the way RFC-077 and RFC-142 established theirs — **by executing
under the resolution model with targeted, revert-proof pins**, not by dark differential:

1. **Row-content shadow (P2) is necessary but not sufficient.** Assert positional row ==
   name map field-for-field on today's plans — this catches row *corruption*, but is **blind to
   wrong-plan-too-few-rows** (RFC-142's failure class: correct rows when the plan is correct; the
   bug is a wrong plan). Keep it, but do not treat it as the certificate.
   **And keep it alive through the whole dual window (Round 5):** both representations are
   emitted until Slice 4 regardless (risk 5 pays that cost), so make the overhead earn its keep —
   in test builds, assert `ordinal result == name result` row-for-row across the yamsql/fuzz
   corpus at **every** slice, with explicit carve-outs for the enumerated known-different shapes
   (`SELECT *` collision, CTE column-rename, buried-reference). The item-2 pins certify the
   *known* differences; this differential catches the *unknown* ones — precisely the coverage
   class that would have caught Slice 1's Step 2b divergence before the spike tripped over it.
   The anti-dark-diff argument at the top of §5 applies to plan-shape gates, not to row agreement
   on shapes where the two models are supposed to agree.
2. **Per-slice execution pins are the certificate.** Each slice that flips authority is gated by
   executing the specific shapes that the model change makes different, and asserting the *new,
   correct* behaviour:
   - CTE column-rename: `TestFDB_CTEChainedColumnAliases` / `TestFDB_CascadesCTEColumnAliases`
     must return the renamed columns (not NULL) **under ordinal resolution**.
   - Interning: the `partition_select_interning_baseline_test.go` task-count baseline
     (8999/30593 ±2%) must hold under alias-bijection — proving shared sub-joins still collapse
     (no super-linear blowup) — **plus (Round 5) a planning wall-clock bound on a
     many-identical-legs STAR corpus**: bijection enumeration is combinatorial in same-typed
     quantifiers, and a task count can stay flat while per-`Insert` enumeration cost blows up —
     the task-count pin alone is blind to it. Both gate Slice 3.
   - **Duplicate-name identity (Round 5, required):** after Slice 3's `FieldValue` node-identity
     flip, a memo-level pin proving two same-named columns from different legs do NOT conflate
     (distinct ordinals ⇒ distinct members ⇒ both planned and both returned); exercised
     end-to-end at Slice 4 when `SELECT *` duplicate coexistence goes live. Today this failure is
     unconstructible — the name model dedups the names first — which is exactly why no existing
     pin covers the axis.
   - RFC-142: the 16-round codex revert-proof pins (buried `WHERE`, buried `GROUP BY`,
     table-first resolution, explicit-JOIN rejection, silent-zero-row, silent-wrong-grouping)
     must all pass under the ordinal buried-source recovery.
   - `SELECT *` collision: goldens updated to the both-columns-coexist result, reviewed as an
     intentional change.
   - **Ordering / distinctness property propagation (Graefe, required):** a per-slice EXPLAIN pin
     asserting **NO sort reappears on an index-ordered join**. Provided orderings are *Values*
     referencing columns; when a column's identity flips name→ordinal, the provided-ordering
     rebase (`pullUpOrderingFromSelectChild`) must stay consistent, or index-ordering match fails,
     `RemoveSortRule` stops firing, and a **spurious sort** appears — a plan-property regression the
     row-content shadow (item 1) is blind to. Slice 4 handles column ORDER
     (`cascades_generator.go`) but MUST also rebase ordering pull-up; every slice that flips a
     column's identity carries this pin.
   - **`GROUP BY`/`HAVING` over a JOIN (RFC-088, @claude-flagged):** `groupby_over_join_fdb_test.go`
     — a qualified joined-table group key (`d.dname`), a bare one (`dname` from `dept` in
     `emp JOIN dept`), a multi-key `GROUP BY` mixing a joined-table key with a first-table key, and
     `HAVING` over the grouped join output — must return the same correct grouped rows under ordinal
     resolution. Gated where the join's merged row becomes authoritative: **Slice 2** for the 2-way
     case, **Slice 3** for N-way. (Grouping keys ride the generic value path, so this is a
     ride-along, but it exercises exactly the name→ordinal flip on a merged row and must be pinned.)
3. **The 2-way wedge (Slice 2) is the real de-risk** — it runs the ordinal model on live join
   plans (result value + positional row + ordinal predicate resolution; interning stays
   name-model until Slice 3, per the canonical sequence) before the atomic N-way flip, so Slice 3
   lands on proven row/resolution mechanics and carries the interning risk itself, mitigated by
   the banked spike harness.

---

## 6. Go-only extensions — "clean Java" is INSUFFICIENT for two of them

The owner's hard constraint: extensions must keep working and be architecturally sound. Two have
**no Java reference** — porting Java faithfully does not cover them; we design them soundly.

- **RFC-142 multi-source lateral `UNNEST`** (`FROM t, t.arr AS x`) — **no Java analog.** Java's
  SQL has no lateral array unnest that participates in inner-join re-enumeration, so nothing in
  Java's ordinal model was ever required to keep an unprojected lateral-source array column live
  across a re-enumeration merge or to stop a bipartition stranding an `Explode` from its buried
  source. Today the name model recovers the source from a dotted `'A.ARR'` prefix
  (`value_correlation.go:47`, `MergeSeedLegsOfValue`).
  **Design (Go-native invariant, enforced BY the model — not a special case):** the `Explode` over
  the buried source array references its source via a *genuine* `FieldValue` ordinal path with a
  real child correlation to the source quantifier. The invariant — *an unprojected lateral-source
  column referenced by an `Explode` survives every re-enumeration bipartition that separates the
  `Explode` from its source* — then follows from the genuine-correlation model: a bipartition that
  stranded the `Explode` from its source would leave a **free correlation**, which the
  now-genuine correlation tracking (Slice 5, `computeCorrelatedTo` with local-bind subtraction)
  **rejects as an invalid bipartition**. So Slice 3's from-scratch recovery is precisely: for each
  bipartition, read the dependent `Explode`'s *real child correlation* (not a dotted string) and
  keep the referenced source ordinals live on the side that binds them. There is no re-exposure
  duality to port — the constraint is emergent. **Pin (mandatory, execution-based):** the RFC-142
  suite (buried `WHERE`, buried `GROUP BY`, table-first resolution, explicit-JOIN rejection,
  silent-zero-row, silent-wrong-grouping) must pass under the ordinal recovery — the row-content
  shadow is blind to the wrong-plan-too-few-rows failure this class produces, so it cannot certify
  this and execution pins are the only valid gate.
- **FULL OUTER JOIN** (RFC-036) — Java SQL has **no outer joins**; its `DefaultOnEmpty` is a
  LEFT-only per-outer-row `nullOnEmpty` on a `ForEach` quantifier and structurally cannot emit an
  inner row that matched no outer row. Go's FULL OUTER emits those via a `matchedInner` bitmap
  **second pass** (`streaming_cursors.go:653,868-877`).
  **Design (Go-native, no Java reference):** `FULL OUTER = LEFT ∪ unmatched-inner`, both expressed
  in the positional row. The LEFT half null-extends the **inner** leg's ordinal slots (via
  `DefaultOnEmpty` + `appendNullLeg`, built in **Slice 2/3** — see the P2 scope note in §4). The
  unmatched-inner half — the `matchedInner` second pass — must
  null-extend the **outer** leg's ordinal slots: fill the outer-leg ordinals with **typed NULLs**
  and the inner-leg ordinals with the inner row's values (the exact mirror of the LEFT direction).
  Dedup between the two passes rides the same bitmap. This is the one place the positional row's
  null-extension is **bidirectional**, and it has no Java oracle. **Pin (mandatory,
  order-sensitive):** the FULL OUTER execution tests assert row COUNT on *both* unmatched sides AND
  NULL PLACEMENT by direction — outer-side NULL for an unmatched inner row, inner-side NULL for an
  unmatched outer row — since a wrong null-direction is invisible to a set-based or count-only
  check.

Extensions that **ride along** (preserved, re-verified by their suites before name paths delete):
correlated scalar subquery (2-leg ordinal seed, Slice 2 — **and add the currently-missing
at-most-one guard early**, `TODO.md:1167-1179`, it is a correctness gap not cleanup); CTE
column-rename (fixed by global alias-bijection, Slice 4); UNION/set-op by position (already
positional — delete `aggregateNamesStableForUnion`/`unionBranchNormalizable` rather than migrate);
grouped-aggregate UNION-by-name as a join leg (columns come from the leg's `rangesOver` `Type`);
**`GROUP BY`/`HAVING` over a JOIN (RFC-088, @claude-flagged) — Go-only** (Java can't plan
multi-table joins, `UnableToPlanException`), so it has no Java analog *like* RFC-142/FULL OUTER,
but UNLIKE them it needs **no bespoke design**: grouping keys evaluate through the same generic
`FieldValue.Evaluate`/`row.Datum` path (`streaming_cursors.go:214` `computeGroupKey`, `:267`
`accumulateRow`), so it rides along once P1/P2 make ordinal resolution authoritative — it just must
be PINNED (§5), not left implicit.

**Resolve the Slice 3/Slice 5 contradiction now:** commit to the genuine-correlation model and
**delete** the buried-leg re-exposure recovery outright (proving the unprojected-lateral-source
survival invariant), rather than *porting a recovery onto the wrong correlation* in Slice 3 and
then deleting it in Slice 5. At most one of those was right; the destination says delete.

---

## 7. Observable behaviour changes (deliberate, reviewed)

- **`SELECT *` last-leg-wins collision is fixed.** Today a bare duplicated column name across legs
  keeps only the last leg's value (name-map collision). Under ordinals both coexist positionally.
  This is a correctness improvement and moves goldens — flagged, not silent.
- Everything else is row-identical by construction; plan *shape* converges toward Java
  (`Project`-over-`FlatMap` disappears where Java folds), which re-baselines ~25 physical
  EXPLAIN assertions (robust FlatMap-counting tests, the yamsql corpus, and logical-tree asserts
  are unaffected — verified in the RFC-164 WS-2 blast-radius analysis). **No wire/continuation/
  plan-hash impact** — `Map`/`Project` is continuation-transparent, no `Map`/`Projection`
  continuation proto exists, plan hashes are in-memory only.

---

## 8. Risks

1. **The knot is atomic for N-way** (Slice 3): P1+P2+P3 flip together or you get wrong rows or a
   memo that stops deduplicating (super-linear blowup with arity). Mitigation: precursors proven
   by execution pins; 2-way wedge first.
2. **Interning regression → plan blowup.** Alias-bijection must keep collapsing shared sub-joins;
   pinned by the task-count baseline + the STAR planning wall-clock bound (§5), which gate
   Slice 3's flip — a regression blocks the slice rather than shipping in it.
3. **Correlation-order budget.** Removing the exploration-hiding (Slice 5) is safe only if the
   local-bind subtraction is exactly Java's; a subtly-wrong subtraction reinflates ≥4-way STAR
   past the task budget.
4. **RFC-142 is a rewrite, not a port** — from-scratch buried-source recovery on genuine
   correlations; gates the hard core.
5. **Long dual-representation window** (P2 → Slice 4): the executor materializes both a name map
   and a positional row — real perf/memory overhead and a maintenance hazard **if parked
   mid-flight**. With staged merged PRs this window lives **on master across several merged PRs**
   (P2 through Slice 4), not on a side branch — that is the real, disclosed cost of incremental
   merge: bounded (Round-5 correction: the dual-emission cost benchmark is a **Slice 1 exit
   obligation** — P2's scope note deferred it, so until it runs the bound is a claim, not a
   measurement) and time-boxed (the P2→Slice 4 run must not stall — treat a parked dual-rep
   window on master as a release blocker), but it is overhead carried in production code for the
   duration, stated plainly. Mitigation-side upside: §5's dual-window corpus differential makes
   the same overhead pay rent as a live oracle.
6. **Estimates are floors (Round 5).** Slice 1 was "~1 shift" and, mid-slice, had already spawned
   a Graefe-acked 7-site buried-reference precursor before its producer flip could land. Budget
   every slice as a floor; risk 5's park-is-a-release-blocker rule is what keeps floor-slippage
   from becoming an indefinite dual-rep window.

---

## 9. Why not the band-aid (Option 2)

Keeping `AnchoredJoin` and folding `Project`-over-`FlatMap` at the implementation layer closes the
correlation *symptom* for joins with no wire impact and no N-way regression — but it **entrenches
the model the owner wants retired** and stacks a compensating layer on top. It leaves the ANSI
unsoundness, the operator allowlist trap, the exploration-hiding duality, and the CTE-rename NULL
hazard all in place, and it is debt this RFC's Slice 3/4 would later unwind. Rejected as an
end-state. (It remains a valid *stopgap* only if the WS-2 invariant were needed before this
migration — it is not.)

---

## 10. Reviewer sign-off (gauntlet — required before the first impl commit)

Query-engine change: Graefe-gated on BOTH the RFC and the implementation. This section tracks the
RFC-level ack; each impl slice re-requests after its commit (an ack only covers the HEAD it saw).

Rounds 1–4 (RFC v1–v5):
- [x] **Graefe** — ACK (ordinal/group destination + 9-slice staging + delete-not-port verified
  against Java 4.12.11.0; ordering-propagation pin added per his condition).
- [x] **Torvalds** — ACK (staging split real, §5 execution pins sound, both Go-only invariant
  designs implementable; stale "one PR" phrasings fixed).
- [x] **codex-review** — clean (doc-only, no defects).
- [x] **@claude** — ACK ("sound migration plan"; caught RFC-088 groupby-over-join + 2 citations,
  all folded in).

Round 5 (this revision — material content changes, full re-ack required before Slice 2 starts;
Slice 1 continues under its own already-acked plan):
- [x] **Graefe** — ACK, unconditional on `dcf493dae` (his one condition — the Slice 2 scoping
  gate must be flattening-aware against `SelectMergeRule` evasion — folded in and re-verified).
- [x] **Torvalds** — ACK on `dcf493dae` (three nits fixed: risk-2 pre-fold clause,
  `semantic_hash.go:108`, LEFT-JOIN hazard ref `:346-358`; Slice 1 benchmark exit obligation
  added per his suggestion).
- [x] **codex-review** — round 1 caught the Round-5 ack-state contradiction (header/log restated
  the checklist; fixed by making the checklist the single source of truth, `e7572be78`); delta
  re-review clean ("no actionable correctness issues", posted on PR #434).
- [x] **@claude** — ACK on `e7572be78` (PR #434: all six findings "sound and land consistently";
  13/14 citations exact; two cosmetic nits — the `GetCorrelatedToOfAnchoredJoinLegs` citation
  drift and the Slice 1 "scope unchanged" wording — folded into this revision).

**Acceptance for the RFC ack:** all four acked with no outstanding NAK, and §5's per-slice
execution pins are agreed as the certification mechanism (replacing the discredited dark-diff
gate). Implementation commits then land slice by slice (packaging per the owner ruling above),
re-acked as they go.

### Review log

**Round 1 (RFC v1, commit `0284ccc46`):**
- **Graefe — ACK (conditional).** Verified every load-bearing claim against Java 4.12.11.0:
  destination faithful, the `<3`-arity seam is a clean architectural boundary, Slice 3 atomicity is
  real, delete-not-port (§6) is correct, §5 execution-pins follow. **Condition:** add an
  ordering/distinctness property-propagation pin (a name→ordinal identity flip can break
  index-ordering match → `RemoveSortRule` stops firing → spurious sort, invisible to the
  row-content shadow). → **Addressed** in §5 (ordering pin) this revision.
- **codex — clean.** Doc-only diff, no actionable defects.
- **Torvalds — NAK (conditional): "right destination, wrong packaging, soft clock."** §5 sound;
  deletions safe; direction correct. Objections: (a) paths wrong → **fixed** (Paths note); (b)
  clock 25–30 not 15–20 → **fixed**; (c) the two Go-only invariants "named but undesigned" →
  **designed** in §6 this revision; (d) **the NAK proper:** the single long-lived PR rots + forces
  repeated re-acks — split behaviour-preserving precursors into separate merged PRs. → **Adopted**
  (Process note: staged merged PRs; owner may override).
- **@claude — "sound migration plan," one real §6 gap (not a NAK).** Found the missing
  name-resolution-dependent Go-only extension: **`GROUP BY`/`HAVING` over a JOIN (RFC-088)** — group
  keys resolve through the same `mergeRows`/`row.Datum` name-map this RFC retires, but it was unnamed
  in §5/§6. → **Addressed:** added to §6 ride-along list + a §5 execution pin
  (`groupby_over_join_fdb_test.go`). Also flagged two stale citations (`values.go` AnchoredJoin field
  → `:2321`; scalar-subquery guard TODO → `TODO.md:1125-1146`) → **fixed**. Confirmed §5
  execution-pins, delete-not-port, the two no-Java-mechanism extensions, and the `producesMergedRows`
  allowlist all check out.

**Round 2 (RFC v3):** packaging adopted as staged merged PRs; Round-1 items (ordering pin, Go-only
invariant designs, clock/paths) addressed. **Round 3 (RFC v4):** @claude fold-ins done (RFC-088
groupby-over-join pin; two citation fixes).

**Round 4 (RFC v5) — RFC-ACK COMPLETE (all four):**
- **Graefe ✅ ACK** (ordering pin met) · **codex ✅ clean** · **@claude ✅ "sound"** (gap + citations
  folded in) · **Torvalds ✅ ACK** — verified every §6 citation against live code, confirmed the
  packaging split is real (not cosmetic) and both invariant designs are implementable. His two
  must-fix doc defects (stale "one PR" phrasing in §4 header + risk #5) → **fixed** this revision;
  risk #5 now states plainly that the dual-rep window lives on master across the merged precursor
  PRs.

**Round 5 (RFC v6, this revision) — adversarial content re-review; re-ack state tracked in the
Round-5 checklist above (the single source of truth — this heading does not restate it).**
Independent full-content review (2026-07-01) after P1/P2 merged, P3 folded, Slice 1 mid-flight.
Findings folded in:
1. **`FieldValue` node-identity flip was created in P1 ("for now") and never scheduled** — now
   owned by Slice 3, with a §5 duplicate-name identity pin. It is a wrong-plans landmine the
   moment Slice 4 lets duplicate bare names coexist: Java identity is ordinal-only
   (`FieldValue.java:676-690`), Go's is name-only (`map_field_values.go:260-262`,
   `semantic_hash.go:108`), and name identity conflates duplicate-named columns in the memo.
2. **Slice 2 ↔ folded-P3 interning contradiction** (Slice 2 said "flip 2-way seed interning";
   the fold says the tier lands with its Slice 3 consumer) — resolved by the canonical interning
   sequence in Slice 3 (no flip in Slice 2); §5.3's wedge claim corrected; the fold paragraph's
   certification staging corrected (task-count + wall-clock at Slice 3; CTE-rename pins at
   Slice 4 with the widened gate).
3. **Coexistence is a correlation-semantics problem, not a row-format one** — the name model's
   dotted-prefix dependency classifiers mis-handle prefix-less ordinal legs. Resolved by the
   Slice 2 enclosing-arity scoping gate + its before/after pin.
4. **Name-burial is not join-only** (proven by Slice 1's Step 2b blocker) — §1.1 item 5 added;
   name-burial inventory added as a mandatory Slice 2 entry gate.
5. **Nested/buried path representation undecided** (Java collapsed `FieldPath` vs chained Go
   nodes) — ruling parked in Slice 3, default = port Java's collapsed form + compose rule.
6. **§5 strengthened:** dual-window corpus differential (row-level, carve-outs enumerated) and
   STAR planning wall-clock interning pin. **§8:** risk 5's benchmark claim corrected to a
   Slice 1 exit obligation; risk 6 (estimates are floors) added. P1's as-landed delta recorded;
   citation drift (mergeRows, qualifyAlias, `values.go` anchors, TODO.md guard ref,
   `accumulateRow`) and the two out-of-tree files in the Paths note fixed.

**Gate for Rounds 1–4 satisfied; Round 5 re-ack per the checklist above.** Slice 1 continues —
its re-ack **gating** is unchanged by Round 5 (it runs under its own already-acked plan), though
it did pick up the benchmark exit obligation (§4); **Slice 2 must not start until all four
Round-5 boxes are checked** — it consumes three Round-5 rulings (no-interning-flip, the scoping
gate, the entry-gate inventory).
