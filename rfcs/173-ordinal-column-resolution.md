# RFC-173: Migrate join column resolution from name-based `AnchoredJoin` to Java's ordinal/group model

**Status:** review/ack state lives ONLY in the §10 checklists (the single source of truth —
this line deliberately does not restate it). Round 5 (adversarial content re-review, 2026-07-01)
is folded into this revision; **Slice 2 starts only when all four Round-5 boxes in §10 are
checked.** Progress: P1 merged (#423), P2 merged (#427),
P3 folded into Slice 3 (#429/#430), **Slice 1 MERGED (#437, `12516e33f`)** — all four gates ACKed
at the merge HEAD (Graefe impl+delta, Torvalds incl. mutation-testing the authority proof, codex,
@claude), ordinal resolution authoritative on the non-join frontier, §5 dual-window differential
standing (1617 entries), stress faster than pre-merge master (the `positionalTypeCache` repays the
window overhead); see the §4 Slice 1 execution log. **Slice 2 is next: its Round-5 boxes are all
checked; the name-burial inventory entry gate (§4 Slice 2) runs before any Slice 2 code.**
Each staged PR re-acked on its own HEAD.
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
  before the ordinal path goes live. **SATISFIED (`4d5095088`):** `BenchmarkDualEmission_Order`
  2318 ns/op vs map-only 1359 ns/op = **+71% migration-window per-row materialization**, after
  fixing the real finding the benchmark surfaced — `protoToPositional` re-derived the
  row-invariant `RecordType` per row (2.6× before the descriptor-keyed `positionalTypeCache`).
  With the cache the positional build (848 ns/op) is CHEAPER than the map build, so Slice 4's map
  retirement ends net-faster than pre-RFC-173. Slice 1 execution log:
  - **Step 1 — type the scan quantifier (DONE, dark).** `translateScan` types the base-table scan
    leaf with the table's canonical `RecordType` (`tableColumns`: proto-descriptor order, UPPER
    names — the same order/case `protoToPositional` gives the runtime row, so a plan-time ordinal
    matches the runtime slot by construction). Fixes a latent bug (`GetResultValue` discarded its own
    `flowedType`). **Scan-leaf match reconciliation (Graefe fork ruling — Fork B):** Java flows
    `Type.AnyRecord` (a constant TOP type) on BOTH the query scan (`RelationalExpression.
    fromRecordQuery`) and the candidate scan (`ExpansionVisitor.createBaseRef`); the concrete record
    type rides a `TypeFilter` ABOVE the scan, never the leaf, so `equalsWithoutChildren`'s flowedType
    term is inert (`AnyRecord==AnyRecord`) and **recordTypes names discriminate**. Go's `UnknownType`
    is the `AnyRecord` analog. Typing the query leaf broke that symmetry (concrete type on one leaf,
    `UnknownType` on the candidate → subsumption failed → full scan). Fix: `FullUnorderedScan.
    EqualsWithoutChildren` wildcards the flowedType term when either side is `UnknownType` (top
    subsumes concrete — the subsumption direction); two concrete types still compare structurally
    (query-side dedup preserved); hash stays names-only. **Fork A (concretely type both leaves) was
    REJECTED**: anti-Java, and makes index selection depend on two independently-built RecordTypes
    being byte-identical (drift → silent full scan, green CI, latent planner bug).
  - **Step 2a — Evaluate ordinal read path (DONE, dark, `f0037e3db`).** `values.OrdinalRow`
    interface (`Get(ordinal)`, `GetByName(name)`; structurally satisfied by `executor.PositionalRow`),
    `evaluateOrdinal` (no name-map fallback, loud `OrdinalResolutionError` on miss),
    `FieldValue.Evaluate` + `evaluateCorrelated` (refactored to `(any,error)`) ordinal branches.
    Unit-tested; no producer flows an `OrdinalRow` yet.
  - **Step 2b — producer flip (DONE, `f0b9e206c` — landed after the buried-reference precursor
    below cleared the gauntlet).** The non-join producers (scan/filter/projection/sort/map) flow the
    `PositionalRow` authoritatively on the `qr.Positional != nil` frontier gate (join producers
    `mergeRows`/`qualifyOuterRow` don't emit `Positional`, so merged rows are auto-excluded — a
    precise self-excluding gate, verified; producers re-emit it only when their INPUT carried one,
    so the frontier propagates and stops at the join/aggregate boundary). Wiring: a
    `Positional OrdinalRow` field on `RowEvalContext` (param/subquery frontier) + a bare-`OrdinalRow`
    case in `evaluateCorrelated` (the single-quantifier frontier row); `executeProjection` types the
    emitted positional row by OUTPUT names (alias-preferring `posNames`); `executeMap` dual-emits
    (closing the P2 gap). Per Graefe: **NO name-map fallback**; a runtime resolveOrdinal `!ok` is a
    **loud internal error** (42703 is caught at plan time), NOT a `SemanticException`. Sort named-key
    COMPARATORS prefer positional and fall back to Datum — comparator semantics, not FieldValue eval
    (ORDER BY may legitimately reference a source column that is only a Datum key during
    coexistence). Verified by the AUTHORITY PROOF (`TestFrontierOrdinalAuthority_RFC173Slice1`: a row
    whose Datum is deliberately WRONG and whose Positional is correct must resolve to the positional
    value — the guard against a silently-dark flip) + the §5 pins
    (`TestFDB_RFC173Slice1_NoSpuriousSort`, `TestFDB_RFC173Slice1_GroupByHavingOrderBy`, CTE-rename
    green via ordinal) + a projection e2e shadow test
    (`TestExecuteProjection_ShadowAndOutputNames_RFC173`, the @claude P2 carry-forward).
  - **⛔ BLOCKER (RESOLVED — found by the Step 2b spike, fixed by the 7-site precursor
    `a58c54c61`+`d8308f1d4`, gauntlet-passed: Graefe design+impl ACK, Torvalds ACK w/ nits fixed,
    codex clean; 4 RFC-082 divergences lifted, real-Java-conformance-validated): buried source references in
    derived-table / recursive-CTE column resolution.** The projection flip is correct in principle
    (Graefe's no-fallback surfaced a real divergence) but loud-errors on ~15 derived-table + recursive-CTE
    tests: `SELECT sub.v FROM (SELECT id AS v FROM a) sub` resolves the outer `sub.v` to a `FieldValue`
    referencing the **source** column `id`, not the derived **output** column `v`. The name map tolerates
    it — `executeProjection` writes the Datum under BOTH the source key (`projNames`, from
    `projectionColumnName`→`fv.Field`) and the alias key — so `Datum["ID"]` resolves; the ordinal model
    correctly rejects it (`GetByName("ID")` misses the `[V]`-typed positional row → loud). **Root cause:**
    Go's derived-table/CTE resolution (`cascades_translator.go` `cteColumnsScope`/`derivedOutputColumns`)
    "sees through" the alias to the underlying column; Java keeps the reference as the derived table's
    output column (ordinal 0). This is exactly the buried-alias divergence RFC-173 exists to fix — but it
    is a **translator/resolver change spanning derived tables + recursive CTEs**, needing Java study of
    derived-table column resolution + a Graefe ACK, not an inline fix. Simple single-table SELECTs and
    UNQUALIFIED CTE renames (`SELECT product FROM priced(product,cost)…`) already resolve cleanly. **Two
    coupled fixes Step 2b must carry:** (i) resolve qualified derived-table/CTE refs to the OUTPUT column
    positionally (translator); (ii) `executeProjection` must type the positional row by the OUTPUT names
    (alias-preferring `posNames`, separate from the Datum's source-keyed `projNames` — the flat CTE-rename
    case already needs this and it is verified sound).
    - **Precise map (research-confirmed).** *Java model:* `sub.v` = `FieldValue.ofOrdinalNumber(QOV(sub),
      ordinal-of-v)` over the derived quantifier's result, name `v` preserved — NEVER sees through to `id`
      (`Expressions.rewireQov` at `Expressions.java:88-95`; `SemanticAnalyzer.lookup`/`resolveIdentifier`
      returns the OUTPUT attribute verbatim, `SemanticAnalyzer.java:441-490`). Java is *already* the
      ordinal model. *Go burial site:* `expr.go:210-215` (`ResolveIdentifier`) + twin `:267-272`
      (`ResolveColumnShadowingQualified`): `ResolveColumnRef` returns the OUTPUT name `V`, then
      `ColumnAliasMap` (`semantic/scope.go:41-45`) swaps it to the SOURCE `ID`. *Why STRUCTURAL, not a
      one-liner:* the source-name convention is threaded through ~5 coordinated sites that must flip in
      lockstep — the two `expr.go` consumers; the three `ColumnAliasMap` producers in `logical_predicate.go`
      (`buildDerivedTableSource:321-343`, CTE-scope `:774-797`, `applyCTEColumnAliases:824-843`);
      `rewriteProjectionAliases` (`logical_predicate.go:1987-2001`) which rewrites the derived projection to
      emit under the SOURCE key; and the recursive-CTE temp-table normalization
      (`cascades_translator.go:3842-3914`, `:3949+`) which hard-codes source names *because* `ColumnAliasMap`
      reverse-maps predicates to source. The flip also touches the name-model coexistence path — hence the
      Graefe design ACK before implementation.
    - **Graefe design ACK (fix plan, blessed).** *Retire `ColumnAliasMap` entirely* — he traced every
      consumer; NO cascades pushdown or index-matching rule reads the reverse-map, it is pure divergence.
      *Land it as a NAME-MODEL-SAFE PRECURSOR* (decoupled from Step 2b's ordinal producer flip), so the
      full suite stays green on the name model first, then the flip lands on the clean convention. But it
      is **7 sites, not 5, and not free**: plain derived tables free-ride (executeProjection double-writes
      `V` and `ID`, `cascades_translator.go:3876`), but the **recursive-CTE temp-table sites
      (`cascades_translator.go:3842-3914`, `:3949+`) must be ACTIVELY re-keyed to OUTPUT names inside the
      precursor** — killing the reverse-map without re-keying makes a recursive predicate say `UP` while the
      temp datum says `PARENT` → cross-level miss → NULL rows; preserve the "never persist a qualified key
      into the temp table" invariant (`:3866-3874`). *Refinement:* `rewriteProjectionAliases`
      (`logical_predicate.go:1987`) is **DELETED**, not flipped — with output names retained there is nothing
      to rewrite, and deleting it makes `executeProjection` emit output-key-only, collapsing the last
      source-key assumption. *Gate before code:* pin a red→green recursive-CTE test proving **no qualified
      key lands in the temp table** post-flip. The 7 sites: expr.go `:211-215` + `:268-272` (drop the swap);
      the 3 `ColumnAliasMap` producers in `logical_predicate.go`; delete `rewriteProjectionAliases`; re-key
      both recursive-CTE translator sites.
  - **LOAD-BEARING INVARIANT (Graefe, required):** lazy resolveOrdinal-at-eval is semantically
    identical to Java's eager plan-time ordinal baking **only because nothing re-types the
    passed-through base record on the non-join frontier**. This invariant MUST hold; the moment a
    rule can re-type a `FieldValue`'s child between plan-finalize and eval (joins), lazy is unsafe.
    **Slices 2–3 commit to eager `FieldValue.ofOrdinalNumber` baking** for merged result values,
    where the RFC already uses it — do not extend lazy resolution past the non-join frontier.
  - **Faithful end state (Graefe, later slice, non-blocking):** the fully Java-faithful shape is
    `AnyRecord` on the scan leaf + concrete type on a `LogicalTypeFilterExpression` ABOVE it (as Java
    does), not a concrete type on the leaf. Slice 1 types the leaf directly as a pragmatic shortcut;
    Fork B is the correct reconciliation with that choice. A later slice may migrate to
    AnyRecord+TypeFilter if the leaf-typing shortcut ever costs more than it saves.
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
  harness. ~~Port the correlated-scalar-subquery 2-leg seed and single-source `UNNEST` here.~~
  **AMENDED (Graefe W4-deferral ruling, post-premise-correction): both move to Slice 3.** The
  original sentence was written on the same false premise (translation-time LEFT opacity) the
  W3b-1 re-ruling corrected: the correlated-scalar seed is a pre-rewrite `JoinLeftOuter` select —
  exactly the ephemeral object the gate must not classify (RewriteOuterJoinRule dissolves it) —
  and the unnest lowering runs on the `_N` name-emulation + RFC-142 dotted classifiers whose
  ordinal port needs S3's collapsed FieldPaths (an S2 port would force the chained-node
  representation the S3 representation ruling already rejected, or piecemeal-rewrite machinery S3
  deletes). **FINALIZED S2 WEDGE SCOPE (the definitive statement): pure INNER/CROSS 2-way clusters
  over NON-JOIN legs, plus FULL-outer boxes over non-join legs — with the W3b-2 gate-pin families
  as the boundary proof (2-way-under-3-way name-model; flattening-evasion clean-decline;
  GROUP-BY/HAVING-over-gated-join ordinal; dup-name SELECT *).** Everything else stays name-model
  behind the gate until S3.
  **Entry gate — name-burial inventory (≤1 shift, mandatory, Round 5): ✅ SATISFIED** — the
  full two-axis enumeration lives in **`173-name-burial-inventory.md`** (~95 sites: executor
  producers/consumers/positional-birth registry + planner/translator anchored-join machinery,
  dotted classifiers, name identity/interning, resolver plumbing — each slotted S2/S3/S4/S6).
  Key conclusions: the ordinal frontier dies exactly at `mergeRows`/`qualifyOuterRow`/
  `remapUnionColumnsByPosition`/aggregate-output (S2/S3 re-birth it there and MUST extend the
  `DisablePositionalEmission` oracle registry); `executeProjection` straddles (its name map can't
  fully delete until S3); the `AnchoredJoin` flag is the linchpin (S2 consumes, S3 identity/
  interning, S4 deletes — every read site enumerated). The Step 2b blocker proved these must be
  mapped up front, not discovered mid-slice.
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
  **IMPLEMENTATION CONTRACT (Graefe pre-code ruling, post-inventory — binding):**
  1. *Scoping gate = form (a), the translation-time cluster-arity walk; form (b) demoted to a
     LOUD ASSERT, never a decline.* A decline-barrier is disqualified by pin (a) itself: keeping
     `(a JOIN b)` boxes nested changes shapes/task counts and turns the RFC-077 interning baseline
     (Slice 3's gate) into noise. Arity algebra on the logical tree at the seed
     (`cascades_translator.go:3374`): `arity(LogicalJoin{INNER|CROSS}) = arity(L)+arity(R)`;
     a `LogicalScan` over a cteScope-registered non-recursive name = `arity(body)` after peeling
     `LogicalFilter`/`LogicalProject` (SelectMergeRule merges through Project via TranslationMap,
     `rule_select_merge.go:165-179`); outer joins (ChildrenAsSet opacity), aggregate, DISTINCT,
     sort/limit, union, recursive CTE, existential = opaque leaf of 1. Ordinal seed iff the MAXIMAL
     enclosing cluster has arity exactly 2; anything unclassifiable counts as >2 (fail toward
     name-model). Because the walk shadows the rule's mergeability predicate, drift is the risk:
     the assert lives in `SelectMergeRule`'s target loop (`:104-126`, beside the outer-opacity
     check) — a name-model parent about to merge an ORDINAL child is a loud planner error.
  2. *Eager representation = baked ordinal on FieldValue* (`*ResolvedAccessor{Ordinal int}`-shaped
     marker that Slice 3 widens to a multi-accessor path). Constructor (`ofOrdinalNumber(QOV(leg), i)`)
     derives the display name from the leg type (diagnostics/coexistence); `resolveOrdinal` returns
     the baked value when set; **identity for BAKED nodes is (name, ordinal)** — a refinement of
     name identity, not Slice 3's flip; baked vs lazy nodes compare UNEQUAL (worst case a missed
     dedup, never a conflation). Rationale: construction-time-resolved *names* (option b) violate
     the load-bearing lazy invariant after any rebuild AND cannot represent `SELECT * FROM a JOIN b`
     with same-named leg columns — two identical `FieldValue(QOV(join),"ID")` nodes would intern as
     ONE memo member → wrong plans; S2 makes that shape constructible for the first time, so
     **§5's duplicate-name identity pin pulls forward into Slice 2**. Option (c) (collapsed
     FieldPath now) drags Slice 3's compose-rule blast radius into the wedge — rejected; S2 needs
     only single-accessor ordinals.
  3. *`appendNullLeg` = N nil slots under the join's SINGLE merged type* (never ad-hoc per-row
     types); symmetric for the FULL-outer inner drain (`streaming_cursors.go:877`). Java analog is
     semantic, not structural: flatMap + `DefaultOnEmpty`, i.e. the null leg is the leg QOV bound
     to NULL and each `ofOrdinalNumber` evaluates to NULL. In `flat_map_cursor.go:181` evaluate the
     result value with the inner binding nil (the null extension falls out); `appendNullLeg` is the
     NLJ-cursor primitive (`executor.go:2167` replacement), PINNED observationally equivalent to
     evaluating the merged RC with `QOV(right)→nil`. Both are positional re-birth sites → extend
     the `DisablePositionalEmission` oracle registry (standing obligation).
  **Slice 2 execution log:**
  - **W1 (baked substrate, dark) — DONE.** `FieldValue.Resolved *ResolvedAccessor{Ordinal}` +
    `NewFieldValueOfOrdinal` (loud `OrdinalBakeError`); identity refinement per ruling #2 (baked =
    (name, ordinal), baked-vs-lazy UNEQUAL, lazy unchanged; hash mixes the ordinal for baked only);
    marker preserved through every copy site (withChildren / pullup / pushdown passthroughs —
    accessor pointer SHARED, replace-never-mutate pinned in the struct comment). §5 duplicate-name
    identity pin landed (raw `[ID,ID]` type, `ofOrdinal(0) ≠ ofOrdinal(1)`). Torvalds NAK round
    fixed three latent holes: (1) baked node over a NAME-keyed eval context is a loud
    `BakedNameContextError` at all seven name-read arms — `values.OracleBakedNameFallback` is the
    single TEST-ONLY bridge (twinned with `DisablePositionalEmission` behind
    `executor.SetNameModelOracle`, the only sanctioned write site; dies with the name map in S4);
    (2) `pullUpThroughRecordConstructor` was a THIRD RC name-lookup consumer — now bakes the
    matched output ordinal when the input is baked or the RC is dup-named; (3)
    `composeFieldOverConstructor` panics (Java IndexOutOfBounds) on an out-of-range bake over its
    OWN child RC; `PushDownValue` keeps the nil decline (external result value). Graefe ACK ×2,
    Torvalds NAK→ACK. W3 owes: real `executor.PositionalRow` eval pin; loud fall-through for a
    baked node over an *unrecognized* context type in `Evaluate`'s tail.
  **W3 PRE-CODE RULING (Graefe, binding) — downstream leg references over the positional merged
  row.** The hazard: window-era uppers reference LEGS directly (`FieldValue(QOV(A), "ID")`); a lazy
  leg reference over the MERGED positional row derives a LEG-relative ordinal from the leg type and
  reads the merged row at that slot — silently wrong. Ruling: **(a) LEG WINDOWS for the window,
  (b) plan-time rebase (Java's way) as the S3/S4 end state.** (a) makes lazy-over-positional
  CORRECT rather than forbidden — coexistence by construction, not by guard; (b) during the window
  would partially rewrite the machinery S3 deletes. Four binding conditions:
  1. Spans (per-leg offset/width) DERIVE from the ordinal RC at cursor construction — never stored
     as independent authority (dual-bookkeeping disease); assert `sum(widths) == len(rc.Fields)`.
  2. Leg windows are DECLARED WINDOW SCAFFOLDING: Java has no merged-row-with-leg-views (its
     uppers reference the join quantifier after plan-time rewriting); the windows exist only
     because window-era uppers still reference legs, and they DIE when uppers bake (S3 flip + S4
     deletions). Must not ossify into "the runtime shape of quantifier bindings."
  3. The window implements OrdinalRow COMPLETELY (Get leg-relative + GetByName leg-local) so it
     slots into the existing evaluateCorrelated binder arm — no new eval arm, loud-miss preserved.
  4. Red→green pin on the EXACT hazard: lazy leg ref over the merged row without windows misreads;
     with windows reads correctly.
  Also ruled: dedicated raw RC constructor (duplicates allowed — §5's shape is otherwise
  unconstructible; `NewRecordConstructorValue` mangles dup names); join predicates bake to
  (leg QOV, field ordinal) at the seed and evaluate against DIRECT per-leg bindings in the join
  loop (no window needed); cursor detection = structural probe (`ContainsBakedOrdinal` on the
  plan's result value, once at cursor construction — emergent from representation, nothing for S4
  to delete; better than a plan flag); single-merged-type property pinned; both row births in the
  oracle registry. Extra pins: spans-consistency assert; dup-named BOX-LEG shape
  (`a JOIN (b FULL JOIN c)` — both gate; the box output is dup-named; a name-model upper
  referencing an ambiguous leg column stays rejected/carved out since lazy GetByName over a
  dup-named window is first-match). W3 staging: **W3a** executor substrate DARK (leg windows,
  spans, merged-row birth behind the structural probe, appendNullLeg nil-binding evaluation,
  oracle registry, real-PositionalRow eval pins — hand-built plans, nothing emits ordinal RCs
  yet); **W3b** the seed flip (ordinal RC + baked predicates for gated joins) → live end-to-end +
  the full pin batch.
  - **W3a (executor substrate + cursor wiring, dark) — DONE** (36297a253+fd07e2f49 primitives with
    the decline-only spans probe / seed-time assertOrdinalJoinSeed split; 140799069+139c6cb94 cursor
    wiring with pre-adapted legs; all Graefe+Torvalds ACKed).
  - **W3b-1 (THE LIVE FLIP) — DONE (1aca8addd), suite 54/54.** Gated 2-way joins seed the ordinal RC
    (baked ofOrdinalNumber legs, declaration order, AssertOrdinalJoinSeed at the seed); CROSS-LEG
    predicate conjuncts bake, single-leg conjuncts stay lazy (pushdown fodder into name-model legs —
    sound by the load-bearing invariant: pushdown moves the predicate, never the leg type). Live
    fallout (7 classes, 4 caught by the drift asserts) and the resulting GATE CORRECTIONS:
    **PREMISE CORRECTION (Graefe re-ruling, W2 erratum (b) partially retracted): opacity is a
    property of the whole rule set ACROSS PHASES, not of translation-time structure —
    RewriteOuterJoinRule dissolves correlated LEFT boxes into INNER + null-on-empty during
    REWRITING, after which they merge freely (the RFC-153 joined-preserved machinery IS that).**
    Final wedge scope: pure inner/cross 2-way clusters over NON-JOIN legs + FULL-outer boxes over
    non-join legs. LEFT/RIGHT outer = poison (S3 ordinalizes the POST-REWRITE inner+nullOnEmpty
    shape — "the pre-rewrite LEFT box is an ephemeral object; gate where the shape is stable");
    LEFT preserved legs ENCLOSED / null-supplying legs fresh; JOIN legs categorically ineligible
    (nested bare concat erases buried aliases — S3 collapsed-FieldPath territory); dup leg aliases
    poison (proper fix = Java's unique quantifier aliases, S3/S4). Re-ruling conditions landed:
    RFC-153 shape pinned name-model e2e; RewriteOuterJoinRule-declines-FULL pinned (the gate
    premise must fail a test before it breaks); merge assert refined to select-children (filter
    children dissolve legally). Executor fallout: FlatMap outer binding = positional leg row (baked
    SARGs); coexistence Datum = bare+ALIAS.COL+TYPE.COL (mergeRows' exact key set); spanAwareRow
    dotted routing (alias-then-type); §7's dup-name SELECT * fix arrived early via driver-side
    positional reads (column-parallel guard + Datum fallback, both pinned); dualwindow caught the
    oracle-side bare-only Datum (oracleNameDatum reconstructs the anchored key set through the
    sanctioned bridge; CARVE-OUTS STILL EMPTY — executor bug, not model divergence). **Graefe ACK
    notes (both landed):** the driver-side positional read PARTIALLY DELIVERS §7's dup-name
    SELECT * fix for 2-way joins — S4's §7 pin planning must not double-count it; the
    bare-name-over-dup-row first-match(spanAware)-vs-last-wins(Datum) divergence is pinned
    unconstructible (TestRFC173S2_SpanAware_BareDupName_DivergencePin — reachable only if the
    resolver ever admits ambiguous bare references).
  - **W2 (cluster-arity gate + drift asserts, dark) — DONE.** `rfc173_cluster_gate.go`:
    `clusterArity` walk + `ordinalWedgeGate` recording per-seed decisions (`wedgeGate` map, W3
    consumes); enclosure flag `inInnerCluster` threaded through translateJoin legs (inner=enclosed,
    outer=fresh), the existential flattens (`translateJoinWithExists`/`buildExistentialSelect`/
    `buildExistentialJoinSelect`/projected-EXISTS), the correlated-scalar 2-leg seed (both legs
    enclosed until W4), `translateUnnestJoin` (whole subtree name-model until W4), recursive CTE
    (blanket enclosure until S4), with `translateOp`-entry resets at opaque boundaries and
    `translateSubqueryRef` rooting EXISTS subquery plans as fresh clusters. Drift asserts landed
    per ruling #1: SelectMergeRule target loop panics on an ORDINAL child merging into a
    multi-quantifier parent; `rebaseBuriedLowerReferences` panics on re-anchoring a BAKED node
    (the re-stamp treatment Graefe requested in the W1 review). **Two deliberate errata vs the
    contract's shorthand (both fail toward the name model; Graefe to confirm in the W2 review):**
    (a) filter/project with exists/scalar subqueries = POISON, not opaque leaf 1 — their selects
    DO merge (ChildrenAsSet true) and would drag existential/NullOnEmpty legs into the ≥3 partition
    machinery; (b) outer-join boxes are gated UNCONDITIONALLY (not only at maximal-cluster arity
    2) — they never merge either way, ruling #3 flips them in W3, and their legs root fresh
    clusters. Pins: arity-per-shape table, flattening-evasion (gate pin (b) planner half),
    enclosure-by-translation matrix (2-way gates; same join under 3-way does not; fresh clusters
    under outer legs/aggregates/EXISTS subqueries gate; EXISTS outer legs don't).
  - **W3b-2 (pin batch) + W5 (gauntlet) — DONE; SLICE 2 MERGED as PR #447 (squash 7f7100199,
    2026-07-02).** Full-branch four-gate trail: Graefe ACK, Torvalds ACK, codex ACK (planner +
    executor scoped passes after the full-diff run overflowed), @claude ACK — every reviewer
    caught ≥1 real bug across the rounds (fail-closed ON-drop, ordinalEligible CTE arm,
    RV-only LegTypes P1, FlatMap LegTypes widening, index-shaped covering rows), each fixed with
    a red→green pin. Stress: branch faster than master on all heavy scans. **Post-gauntlet merge
    saga:** master moved twice (PR #446 recursive-CTE alias frontier; PR #450 RFC-176 P1). #446
    had independently invented a SECOND baked-ordinal mechanism
    (`ResolvedOrdinal`/`HasResolvedOrdinal` — childless, quiet name-fallback, the recursive-CTE
    leg wrap). Merge kept both; then a dedicated reviewed commit (51e3327ca) unified onto
    `ResolvedAccessor` per a Graefe pre-code ruling: **`FrontierPinned` bit on the accessor**
    (set at the seed constructor, clear at the wrap constructor) carries the loud-guard
    contract — child-presence was NAKed (pullup/pushdown passthrough copies strip Child while
    sharing the accessor pointer, silently demoting loud nodes); the bit is excluded from
    identity/hash/Explain and dies in S4 with the guard. ExplainValue now renders `#ordinal` for
    ALL baked nodes (closed a latent dup-slot gap in the ExplainValue-keyed projection identity).
    Watch-item banked in the S3 substrate map: pinned/unpinned equal-(field,ordinal) nodes are
    identity-equal but guard-different (never co-occur in one tree today — recursive CTEs are
    gate-poison; re-check when S3 widens baked-node reach).
- **S3 STAGING RULING (Graefe, pre-code, BINDING — issued at S3 start on the substrate map):**
  - **W1 — separate mergeable PR, DARK.** FieldPath widening: WHOLESALE swap of
    `Resolved *ResolvedAccessor` to an immutable multi-accessor path (Java FieldValue.java:82-85 —
    one node, whole path; additive twin representation NAKed: "two representations of one fact
    drift"). `ResolvedAccessor` stays the per-step `{Field, Ordinal}` element (:645-754).
    **`FrontierPinned` lives on the PATH, once, not per-accessor** (the contract governs the root
    read context; N copies of a one-meaning bit desynchronize); excluded from identity/hash/Explain
    as ruled at S2; dies in S4. Replace-never-mutate carries over (`withSuffix` returns a new path,
    Java :525-534). **Path identity before the W3 flip: element-wise list equality on S2's
    (name, ordinal) pair** (Java list-equals :411-420 with the pair refining Java's ordinal-only
    :676-690 — a refinement can only under-dedup, never conflate). **Compose rule GATED to
    fully-baked paths in W1** (unreachable today — no chained-over-baked shapes in the S2 wedge —
    hence genuinely dark; compose-on-lazy rewrites every nested-access chain corpus-wide: memo
    identity, Explain, every rule matching chained FieldValues). Prove darkness: row-content
    differential + unchanged task counts. **Do NOT port ExpandFusedFieldValueRule** (Java never
    co-resides them: Compose in DefaultValueSimplificationRuleSet.java:43, Expand only in
    MaxMatchMapSimplificationRuleSet.java:50, stack-overflow warning at
    ComposeFieldValueOverFieldValueRule.java:41-43); instead W2/W3 must VERIFY max-match-map
    handles fused paths (prefix matching), pinned by the §5 no-spurious-sort EXPLAIN pin + a
    nested-field-index scan-still-chosen pin; port Expand only if that fails, into matching only.
  - **W2+W3 — ONE merge unit (multiple commits, one PR).** The coupling is INTERNING AUTHORITY,
    not soundness: merge-select interning keys on the `AnchoredJoin` RC marker (select.go:243-256);
    W2's positional unnamed merged row (PartitionSelectRule.java:284-291, Column.unnamedOf) has no
    marker — merging W2 alone drops merge selects to alias-identity dedup and reproduces the
    documented 29915→60044 task blowup (partition_select_interning_baseline_test.go:99-104). Do
    NOT bridge by stamping `AnchoredJoin=true` on the new RC (the marker carries correlation-hiding
    semantics, value_correlation.go:96-98, which S3 deletes). W2's positional row IS sound on
    (name, ordinal) identity (unnamed columns share names; ordinals distinguish), so the identity
    flip may be a LATER COMMIT than the merge-case rewrite — within the same PR. Compose widens to
    all FieldValues inside this PR. Every new positional-row birth site (N-way executor) extends
    the `DisablePositionalEmission` oracle gate (standing obligation).
  - **W4 — separate PR(s) after W2/W3, one per deferred item** (post-rewrite LEFT ordinalization,
    correlated-scalar seed, single-source unnest), each with its own pins. Gate Pins A/B rewrite
    at W4. **W5 — separate final PR** (RFC-142 multi-source unnest bipartition rewrite + gauntlet);
    its safety rests on W2/W3 merged and green first.
  - **Regression net (survive unchanged):** task-count baseline 8999/30593 ±2% + STAR wall-clock +
    shadow-delta; NextMergeAlias pins; rfc173_slice2_gate_pins_test.go
    GroupByHaving/DupNameStar/CoveringIndexLeg; CTE column-rename FDB tests; RFC-142's 16
    revert-proof pins; rfc173_unified_baked_test.go (mechanically adapted to path); row-content
    differential. **Delete WITH machinery (same commit):** rfc173_slice2_drift_assert_test.go +
    both tripwires (rule_partition_select.go:910-914, rule_select_merge.go:141-148) +
    rebaseBuriedLowerReferences/buildUpperResult/NewReEnumerationAnchoredRecord + the re-enum pins
    in value_anchored_join_record_test.go. **Rewrite:** rule_partition_select_test.go:157,193 →
    positional expectations; InternsAliasAware gate test → new merge-select marker (CTE-NULL
    protection intact).
- **Slice 3 — THE HARD CORE: N-way re-enumeration + interning, ordinal/group (ATOMIC)**
  (~~3~~ **4–5 shifts — resized honestly per the Graefe W4-deferral ruling: S3 additionally owns
  the three items below that S2's premise correction displaced**). Replace the name-based re-stamp
  machinery
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
- **Slice 4 — Retire `AnchoredJoin` (deletions)** (~2 shifts).
  **PREREQUISITE (Graefe W4b design-ACK condition):** before deleting the name model, the W4b
  correlated-scalar gate's name-model fallbacks must be eliminated — i.e. the InnerAlias/inner-leg
  aliasing must be resolved so that JOIN-inner and COMPUTED-scalar correlated subqueries can
  ordinalize (they currently stay name-model, see the W4b impl-correction notes). S4 cannot land
  while any correlated-scalar shape still depends on `NewScalarSubqueryAnchoredRecord`. Delete
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
  **Slice-4 kill list (Slice-1 gauntlet obligations — Graefe + Torvalds, recorded):**
  - The sort named-key COMPARATOR fallback (`sortKeyFromResult`/`compareByField`
    positional-first → Datum) is a documented coexistence tolerance, differential-blind — when the
    Datum retires it must **die loud**, not linger as a nil-yielding path.
  - The §5 dual-window differential (`pkg/relational/conformance/dualwindow`) + the
    `DisablePositionalEmission` oracle retire WITH the name map.
  - `legPhysicalOutputNames` / read-by-rendered-name in the recursive-CTE normalization must not
    outlive the window: the end state reads leg slot *i* by ordinal (`ofOrdinalNumber` over the leg
    quantifier, built in Slices 2–3).
  - Bound `positionalTypeCache` (descriptor-keyed sync.Map): a dynamicpb miss Stores forever — a
    slow leak in a long-lived multi-tenant process. Bound or evict before Slice 4 ships.
  - `recursiveRemapValues`' first-dot split turns a qualified COMPUTED physical name
    (`"(B.ID + 1)"`) into garbage (`QOV("(B")`) — pre-existing class, dies with the name machinery;
    until then the ordinal model loud-errors it and the differential watches.
  **Slices 2–3 standing obligation (Graefe):** every NEW positional-row birth site added for the
  join producers must extend the `DisablePositionalEmission` oracle gate, or the §5 differential
  silently loses coverage of the new frontier.
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

- **RFC-142 multi-source lateral `UNNEST`** (`FROM A, B, A.arr AS x`) — **no Java analog** (this is
  the W5 case; the *single-source* `FROM t, t.arr AS x` lowering DOES have a Java analog —
  `LogicalOperator.generateCorrelatedFieldAccess` — and is ported by W4c, see that section). Java's
  SQL has no lateral array unnest that participates in inner-join **re-enumeration**, so nothing in
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

## W4 — Post-rewrite LEFT ordinalization (design for Graefe ACK; PROPOSED, pre-impl)

W2/W3 (fulcrum PR #457) landed: INNER clusters + FULL-outer boxes gate into the
ordinal wedge; LEFT-outer boxes are POISONED (name-model), "pending a review
re-ruling on the corrected premise". This is that ruling. W4 is a separate,
stacked PR; W5 (multi-source unnest) still waits for W2/W3 merged.

### Established facts (verified against Java + Go source)
- **Java** (`RewriteOuterJoinRule`/`OuterJoinExpression`): a LEFT box dissolves
  into two plain `SelectExpression`s — an OUTER select with NO predicates and
  quantifiers `[preserved ForEach, forEachWithNullOnEmpty(innerRef)]`, reusing
  the box's own result value verbatim; an INNER select = the null-supplying leg
  carrying the ON-preds. Every null-supplying-alias reference in the result
  value must be NULLABLE-typed (`OuterJoinExpression` :117-125). The
  `nullOnEmpty` flag emits ONE all-NULL row when the inner select is empty;
  the outer result value evaluates over that NULL binding. Java is positional
  throughout — NO dedicated ordinal seed, NO special-casing; the dissolved
  outer select is just an ordinary select.
- **Go today** (`rule_rewrite_outer_join.go:172-179`): fires for a correlated
  LEFT with ON-preds, yields
  `NewSelectExpressionWithJoinType(sel.GetResultValue(), [preserved, nullOnEmptyQun], nil, aliases, JoinInner)`
  — result value is the NAME-MODEL anchored RC, IsNullOnEmpty true on leg 2.
- **Executor is ALREADY DONE**: `evaluateOrdinalJoinRow` + `bindLeg`'s
  nil-QueryResult → `(nil,true)` → NULL slots (contract ruling #3). A null-leg
  ordinal birth is exactly the null-on-empty semantics — no executor work.

### PROPOSED ruling
1. **Ordinalize AT THE REWRITE, not translateJoin.** The gate correctly poisons
   the pre-rewrite LEFT box (it isn't the stable shape); the stable shape is the
   dissolved INNER+nullOnEmpty select produced by `RewriteOuterJoinRule`. So the
   rewrite rule builds the ordinal seed. The whole dissolved shape becomes ONE
   ordinal seed: `RC(ofOrdinal(preserved, i) for preserved cols ++
   ofOrdinal(nullSupplying, j) for null-supplying cols)`, the null-supplying
   ordinals null-extending when the inner is empty (the executor's existing
   null-leg birth). The null-supplying subselect is NOT ordinalized separately —
   it's a filtered scan feeding the outer RC's null-supplying ordinals.
2. **Leg column types at the rewrite come from the ANCHORED RC being replaced**
   (Graefe Q1 ruling — decided). Go's `Quantifier.GetFlowedObjectValue` is
   UNTYPED (Java's is always typed), so quantifier flowed `RecordType`s are not
   a source at rewrite time — there aren't any. `sel.GetResultValue()` is the
   box's typed flowed row, the full per-leg concat carrying a flattened
   multi-table preserved cluster's columns verbatim, AND the exact contract the
   retained name-model dual reads — so deriving `ordinalLegType` from it keeps
   the §5 windows type-congruent.
3. **Nullability: apply the nullable wrap; it is identity-invisible** (Graefe Q2
   ruling — decided, with one check). The null-supplying leg's ordinal columns
   must be NULLABLE-typed (Java `OuterJoinExpression` :117-125). The executor
   null-leg emits nil regardless of declared nullability and the §5 oracle
   compares datum VALUES not type metadata, so the wrap perturbs neither
   `datumFromSpans` nor the oracle — but it is type-NECESSARY. **Check to honor
   in impl**: confirm the retained name-model dual already nullable-wraps the
   SAME null-supplying columns; if not, fix it in the same commit or the windows
   diverge on type.
4. **Ordinalization is GATED on BURIED-ELIGIBILITY** (Graefe Q3 ruling — the
   NAK correction; supersedes the earlier blanket "stop poisoning LEFT"). Only
   a dissolved LEFT whose null-supplying ON-preds correlate to the TOP-LEVEL
   preserved alias ordinalizes. If those correlations intersect
   `preservedProvidedAliases` BELOW the top alias (a BURIED source — the RFC-153
   `(A⋈B) LEFT JOIN C` with `C→A` shape), the preserved leg is NOT
   ordinal-eligible and the dissolved shape STAYS NAME-MODEL. Rationale:
   ordinalizing a flattened preserved cluster M=(A⋈B) to a bare concat erases
   the name `A`; the innerSelect's ON-pred stays name-model (ruling #1, it is
   the null-supplying leg's own select, not the ordinal seed) and has no span
   into the ordinal preserved leg, so `physicalProvidedAliases`/`rightDepsLeft`
   cannot rebase it and the RFC-153 correlated-FlatMap-probe path breaks
   (0-row/unresolvable — PR-#201-class). The fulcrum's span-recovery was built
   for translated TOPS, not a name-model ON-pred crossing the dissolution
   boundary into a buried ordinal.
5. **Relax the three W4 tripwires, in order after (1) produces the shape —
   SCOPED to the top-level-correlated (buried-eligible) case only:**
   - `rfc173_positional_merge.go:39-42` — the null-on-empty tripwire (a
     null-on-empty leg is now a legitimate ordinal shape for the buried-eligible
     dissolved LEFT).
   - `rfc173_cluster_gate.go:103-117` + `clusterArity:268-276` — a
     top-level-correlated dissolved LEFT gates; its preserved + null-supplying
     legs pass `ordinalEligible`. A BURIED-correlated dissolved LEFT stays
     poisoned (name-model) — the buried-eligibility test above is the new gate
     arm, keyed on the ON-preds' correlation depth into
     `preservedProvidedAliases`.
   - `rfc173_ordinal_seed.go:104-106` — the name-model-join-leg panic relaxes
     only where the preserved leg is a top-level-eligible ordinal cluster.
6. **Out of W4 scope**: `EliminateNullOnEmptyRule` (strips the flag when a
   predicate rejects null — a Go-side optimization Go doesn't have; not required
   for correctness). FULL-outer stays as-is (already gated, materialized NLJ).

### Gates
Query-engine change → Graefe ACK on THIS ruling before any impl (Q1/Q2 ACKed;
Q3 correction folded above — re-submitted for ACK), then the full four-gate
round (Graefe/Torvalds/codex/@claude) on the implementation. Regression net: the
RFC-153 joined-preserved plan test MUST stay name-model (the buried case) AND a
NEW top-level-correlated dissolved-LEFT pin MUST ordinalize; the null-on-empty
executor pins; Gate Pins A/B rewrite at W4; task-count baseline ±2%.

### W4 impl note — the buried-eligibility gate is SOURCE-COUNT, not ON-pred-alias
Graefe's Q3 ruling ("gate on whether the ON-preds correlate below the top
alias") has an ALIAS-COLLISION edge the first impl tripped: a flattened
preserved cluster's synthetic quantifier alias is `sourceAlias` = its RIGHTMOST
leaf's alias. So for `(a JOIN b) LEFT JOIN c ON c.bx_ref = b.bx`, the ON-pred
correlates to `b`, which EQUALS the preserved cluster's own alias "B" — an
`alias == topAlias` test wrongly reads it as top-level and ordinalizes, erasing
the a⋈b structure (the e2e caught it: `field "BX" not resolvable, row columns
[B.ID B.A_ID B.BX]`). The collision-safe gate keys on SOURCE COUNT:
`leftOrdinalizable` is true iff `preservedProvidedAliases(preserved) ==
{topAlias}` exactly (a single source, no buried). A multi-source preserved
cluster is NEVER eligible — ordinalizing it erases buried names regardless of
which alias the ON touches. Pins: TestRFC173W4_LeftOrdinalizable (source-count),
TestRFC173W4_RewriteYieldsOrdinalSeed_TopLevel (the ordinal seed shape),
TestFDB_RFC173_W4_TopLevelLeftOrdinalizes (e2e NULL-extension), RFC-153
JoinedPreservedMatrix/buried_other_leg (buried stays name-model, red before the
fix). No tripwire relaxation needed for the simple case (2-quantifier dissolved
LEFT → binary join impl, not PartitionSelectRule; the executor null-leg birth
was already done).

### W4 impl review round (PR #458): Graefe ACK, Torvalds NAK, codex P2, @claude findings — all resolved
- **Symmetric single-source gate** (Torvalds): the gate now requires BOTH legs
  single-source (singleSourceLeg each) PLUS the seed builder declines any
  anchored RC decomposing into != 2 legs. Dropped the dead preds param.
  (@claude argued the null side can't be a raw cluster at the SQL surface —
  extractJoinClause only accepts atom/subquery RHS — so the asymmetric gate was
  "sound not oversight"; the symmetric gate is strictly safer and the net
  confirms it doesn't over-reject derived-table null sides.)
- **Declaration order** (codex P2): a RIGHT JOIN normalizes to LEFT with swapped
  children but the anchored RC stays in SQL declaration order; the seed now
  walks the anchored RC in field order (aliases identify only the nullable leg),
  so SELECT */positional output is stable. Pin: SeedPreservesDeclarationOrder
  (null-leg-first, red with the old preserved-then-null emission).
- **Decline not panic** (Torvalds): the seed builder returns nil (name-model
  fallback) on malformation; the AssertOrdinalJoinSeed panic (translator
  altitude) is gone from the rule path.
- **Real e2e** (Torvalds/@claude no-fake-checkbox): the prior e2e's winning plan
  was the materialized LEFT NLJ (ordinal seed never executed). Added an index on
  the null-supplying side so the correlated dissolved FlatMap WINS, asserted the
  plan is the FlatMap (not NestedLoopJoin(LEFT OUTER)); for a single-source LEFT
  the rule yields only the ordinal seed as the dissolved form, so a FlatMap win
  IS the ordinal path executing.
- **Nullability reconciliation** (@claude vs Graefe): @claude flagged the retained
  name-model dual as not nullable-wrapping the null-supplying columns (a
  type-gap). RESOLVED — Graefe is right: fieldTypeForFD (cascades_translator.go:
  177-201) constructs EVERY anchored leg column nullable at the source
  ("Columns are nullable"), not via WithNullability, so @claude's grep missed
  it. The name-model dual IS nullable; the ordinal seed is congruent (preserved
  = fv.Typ, nullable in production; null-supplying explicitly wrapped). No gap,
  no fix needed. The RFC's "confirm the dual nullable-wraps" check is satisfied.

---

## W4b — Correlated-scalar 2-leg ordinal seed (design for Graefe ACK; PROPOSED, pre-impl)

The second W4 per-item PR (after W4-LEFT #458; stacked on it). Ordinalizes the
correlated-scalar-subquery-in-projection seed so it survives the S4 name-model
deletion, and folds in the RFC:807 at-most-one guard.

### PREMISE CORRECTION (verified against Java source, tag 4.12.11.0)
**Java does NOT support scalar subqueries in a projection list — at all, correlated
or not.** The grammar's `expressionAtom` (`RelationalParser.g4:1240-1251`) has no
`'(' query ')'` alternative; `query` appears in an expression position only in
`existsExpressionAtom` (:1228, a boolean) and `inList` (:1254, which `visitInPredicate`
rejects: `ErrorCode.UNSUPPORTED_QUERY` "IN predicate does not support nested SELECT",
`ExpressionVisitor.java:624`). So `SELECT c.name, (SELECT … WHERE … = c.id) FROM c` is
a PARSE ERROR in Java. **⇒ Go's correlated-scalar-in-projection is a READ-SIDE
EXTENSION, not a parity port** ("query reach is not the hard line" — CLAUDE.md; wire
compat is untouched, the extension only lets Go *express* more).

**Why it is still an RFC-173 item:** the name model is DELETED in S4. This extension's
seed is a NAME-MODEL `NewScalarSubqueryAnchoredRecord` today (`cascades_translator.go:
3177`), so it BREAKS at name-model deletion unless ordinalized now — an "extension that
rides along" (§ line 806-807). Java is still the *ordinal-shape* reference:
`convertToExpressions` (`LogicalOperator.java:358-372`) bakes each flowed leg column as
`FieldValue.ofOrdinalNumber(quantifier.getFlowedObjectValue(), colCount++)` — the pattern
the Go extension emits for both legs.

### Established facts (Go source)
- **Site** `translateProjectWithCorrelatedScalar` (`cascades_translator.go:3106`): builds a
  `JoinLeftOuter` `SelectExpression` over `[outerQ, innerQ]` with **nil top-level preds** —
  the correlation is baked into `innerRef` as a filter child, so `RewriteOuterJoinRule`
  does NOT fire (it guards on `len(preds)==0`). This is the correlated-LEFT-OUTER-FlatMap
  data-access shape, **distinct from W4-LEFT** (which ordinalizes the *dissolved* form). The
  seed here is built directly at translation, so W4b ordinalizes at the translator, not the
  rewrite. Result value = `NewScalarSubqueryAnchoredRecord(outerLeg, innerAlias, scalarCol)`
  (`value_anchored_join_record.go:121`): outer leg = its derivable columns; inner leg = ONE
  field `<innerAlias>.<scalarCol>` = `FieldValue(QOV(innerAlias), scalarCol)`. Both legs
  enclosed (`inInnerCluster=true`) → name-model in the W2 gate.
- **Null-on-empty** already correct: `innerQ` is the LEFT-OUTER null-supplying leg; an outer
  row with no inner match yields the scalar as NULL (Go's `IsNullOnEmpty` = Java's
  `Quantifier.forEachWithNullOnEmpty`, `Quantifier.java:317-338`; the only Java
  forEachWithNullOnEmpty is bare-aggregate group-by, `LogicalOperator.java:449-451`).
- **At-most-one today** (`logical_predicate.go:5995-6002`): when the user wrote a LIMIT it is
  respected; otherwise the inner is wrapped in a DEFAULT `LIMIT 1`. That default **silently
  truncates** a >1-row correlated subquery to an arbitrary first row (non-deterministic
  without ORDER BY) instead of raising the cardinality violation. The UNCORRELATED path
  (`EvaluateScalarSubquery`, `scalar_subquery.go`) already collects-2-and-errors **21000**;
  the correlated path is inconsistent with it and with the SQL standard. Java has NO guard
  (it can't express the feature), so this is an EXTENSION-QUALITY decision, no Java oracle.

### PROPOSED ruling
1. **Ordinal seed (the RFC-173 core).** Replace the name-model
   `NewScalarSubqueryAnchoredRecord` result value with an ordinal seed —
   `RC(ofOrdinal(QOV(outerAlias), i) for outer cols ++ ofOrdinal(QOV(innerAlias), 0))` (the
   inner exposes exactly one scalar column, ordinal 0). Follows `convertToExpressions`. The
   inner scalar ordinal is **NULLABLE-wrapped** (LEFT-OUTER empty → NULL; executor null-leg
   birth, contract ruling #3 — no executor change). Leg types derive from the anchored RC
   being replaced (same Q1-ruling source as W4-LEFT: quantifier flowed types are untyped at
   translation). `AssertOrdinalJoinSeed` tripwire on the constructed output;
   decline-not-panic (return the name-model RC) on any malformation.
2. **Single-source gate — OUTER LEG ONLY** (asymmetric, and this is the key divergence from
   W4-LEFT — flagged for Graefe). W4-LEFT gated BOTH legs because BOTH contribute multiple
   columns to the seed (a full row concat), so a cluster on either side erases buried names.
   W4b's seed is different: the OUTER contributes its columns, but the INNER contributes exactly
   ONE scalar column, referenced `ofOrdinal(QOV(innerAlias), 0)`. The inner's INTERNAL structure
   (even `(SELECT x FROM a JOIN b WHERE … = outer.id)`) never exposes sub-names in the seed — it
   is one positional scalar — so the inner needs NO single-source gate. Ordinal 0 is correct by
   the scalar-subquery contract (the inner projects exactly the scalar; `EvaluateScalarSubquery`
   enforces one column; Go flowed types are untyped at translation per the Q1 ruling, so a
   translation-time column-count assert isn't possible — the executor's existing one-column check
   is the backstop). **Gating the inner on single-source would be a latent bug**: it would leave
   every internally-joining scalar subquery name-model, which then BREAKS at the S4 name-model
   deletion — the exact failure this item exists to prevent. So: gate ONLY the OUTER on
   `singleSourceLeg` (a multi-table outer cluster `SELECT …, (subquery) FROM a JOIN b` stays
   name-model — buried-name erasure, same as W4-LEFT); ordinalize the inner unconditionally as
   ordinal 0.
3. **At-most-one guard (the RFC:807 rider, extension-quality).** When there is NO user LIMIT,
   STOP injecting the default `LIMIT 1`; instead enforce at-most-one in the correlated
   evaluation and raise **21000** on >1 rows, mirroring `EvaluateScalarSubquery`. An EXPLICIT
   user LIMIT is respected verbatim (a deliberate top-N/top-1, e.g. `… ORDER BY amount LIMIT
   1` — the common existing shape). Net effect: `SELECT (SELECT salary FROM emp e WHERE
   e.dept_id = dept.id) FROM dept` errors 21000 when a dept has >1 emp, instead of returning a
   non-deterministic salary. **This is a user-visible behavior change to the extension** (silent
   wrong → explicit error) — flagged for the four-gate round. Sequencing: this guard is
   independent of the ordinal seed (inner-plan change vs result-value change); per RFC:807
   "add it early", do it as the FIRST commit of the PR, the ordinal seed as the second.
   **Graefe scope question:** bundle both in this PR (RFC's stated intent), or split the guard
   to its own correctness PR? Both touch only `translateProjectWithCorrelatedScalar`'s
   neighborhood; I lean bundle-but-separate-commits.

### Out of scope
The uncorrelated scalar-subquery seed (pre-evaluated, `EvaluateScalarSubquery`) — it flows a
bound literal, not a join seed, so there is no anchored RC to ordinalize. GROUP BY / ORDER BY
inside the subquery (orthogonal; already handled).

### Gates & pins
Graefe design-ACK on THIS ruling before impl, then the four-gate round (Graefe/Torvalds/
codex/@claude) on the implementation. Pins: white-box ordinal-seed shape
(`TestRFC173W4b_ScalarSeedShape` — baked ofOrdinal refs, inner ordinal-0 nullable,
AssertOrdinalJoinSeed); **multi-source OUTER stays name-model** (`…_ClusteredOuterDeclines`);
**internally-joining INNER still ordinalizes** (`…_JoinedInnerStillOrdinalizes` — pins the
outer-only gate; a same-shape all-both-gates design would wrongly decline this); e2e correct
rows + EXPLAIN (`TestFDB_RFC173_W4b_CorrelatedScalarOrdinalizes`); **correlated multi-row →
21000** (`TestFDB_RFC173_W4b_CorrelatedScalarCardinality`, red before the guard);
explicit-user-LIMIT still returns top-1 (no regression). Task-count baseline ±2%.

### W4b design review: Graefe ACK (conditioned) + SPLIT scope ruling
Graefe design-ACKed the ordinal seed. Framing / translator altitude / **outer-only gate**
("the crux, and it holds") all validated. Two conditions folded into the impl plan:
1. **No dup ordinals.** `AssertOrdinalJoinSeed` forbids duplicate ordinals, so — UNLIKE the
   name-model `NewScalarSubqueryAnchoredRecord` (which emits bare + qualified + dotted forms
   per column) — the ordinal seed emits ONE field per column. (a) the inner RC field stays named
   EXACTLY `<innerAlias>.<scalarCol>` (what unconditional `replaceScalarSubqueryRef` emits)
   valued `ofOrdinal(QOV(inner),0)`, else lazy `composeFieldOverConstructor` misses it; (b) the
   e2e pin MUST exercise a **qualified** outer ref AND a **bare** one (not bare-only), proving
   single-source alias-normalization resolves both to the one ordinal field.
2. **SPLIT scope (Graefe overrides RFC:807 "bundle").** The at-most-one guard is an
   executor/evaluation-semantics change with NO Cascades-architecture content and a user-visible
   behavior flip — it lands as its OWN single-purpose PR **first** (its own correctness four-gate,
   independently revertable); the ordinal seed **stacks on top**. Order: (1) guard PR → (2) seed
   PR stacked on it. The guard needs no Graefe DESIGN-ACK (no architecture content) but still runs
   the four-gate on impl.

### W4b impl correction: the INNER also needs a JOIN gate (outer-only ruling amended)
The "outer-only gate; the inner needs no gate" ruling was empirically incomplete. Graefe's
premise — "the inner contributes one positional scalar regardless of its internal structure" — is
true of the SEED SHAPE (the inner is one field), but the executor's type-consistency machinery
(`widenLegTypesFromPlan`) keys on the leg ALIAS, not the seed field. A JOIN-inner
(`(SELECT COUNT(*) FROM orders o JOIN items i … WHERE o.customer_id = c.id)`) names its FIRST table
under the SAME alias `csq.InnerAlias` carries (`sq.tableAlias` = "o"), and that inner ordinal join
bakes a typed `QOV(O, 4-field)` leg. The ordinal seed would ALSO type the inner scalar leg as
`QOV(O, 1-field)` — two DIVERGENT typed QOVs for one alias, which `widenLegTypesFromPlan` rejects
("leg O carries DIVERGENT baked types (1 vs 4 fields)"). The name model dodged it by referencing
the inner through an UNTYPED QOV. `clusterArity` cannot detect this — it returns 1 for
`LogicalAggregate` without recursing into the join beneath a `COUNT(*)`/`SUM(...)`.

**Correction:** ordinalize only when `clusterArity(outer)==1` AND `!innerContainsJoin(csq.InnerPlan)`
(a recursive join walk of the inner). A join-inner stays name-model (correct today; the S4
name-model deletion must resolve the InnerAlias/inner-leg aliasing before it can ordinalize —
tracked as a W4b follow-up, not this item). Caught by three regressed FDB pins
(`aggregate_with_join`, `group_by_with_join_sum`, `group_by_unqualified_key_in_join`) that panicked
before the gate; pinned white-box by `TestRFC173W4b_ScalarSeed_InnerJoinGate`. Single-source
inners (incl. single-table aggregates) still ordinalize (`TestFDB_RFC173W4b_ScalarSubqueryOrdinalSeed`).
Re-submitted for Graefe design-ACK.

### W4b impl correction 2: the inner gate is THREE conditions, not one (computed-scalar + S4 prerequisite)
Shape-boundary probing (`TestFDB_RFC173W4b_ScalarInnerShapeProbe`) found a SECOND inner class the
ordinal seed cannot consume, beyond join-inners: a **COMPUTED scalar** (`(SELECT UPPER(ename) …)`,
`salary+1`, `CAST(x AS …)`). The inner flows the full NAME-MODEL source row and the expression is
evaluated ABOVE it, so `scalarCol` (a synthesized name like `UPPER(ENAME)`) is NOT a key in the
inner row — the ordinal leg adapter rejects it ("name-model leg row carries NONE of the leg type's
1 columns"). clusterArity/`innerContainsJoin` don't see this (the computed projection isn't in
`csq.InnerPlan` — it's applied above).

**Final gate** (all three required): `clusterArity(outer)==1 && !innerContainsJoin(inner) &&
innerScalarIsRowColumn(inner, scalarCol)`, where the last holds iff the inner AGGREGATES (collapses
to a scalar-keyed row) OR `scalarCol` is a stored column of the inner's single source. Plain-column
and aggregate scalars ordinalize; join and computed scalars stay name-model. Proven safe across
`{plain-column, AVG, MAX, UPPER, +1, CAST}` — all execute with no leg-adapter error.

**Graefe S4-prerequisite (design-ACK condition):** the S4 name-model deletion is GATED on resolving
the InnerAlias/inner-leg aliasing so that join-inner AND computed-scalar correlated subqueries can
ordinalize — S4 cannot land while any correlated-scalar shape still depends on name-model. Recorded
here as an S4 blocker.

---

## W4c — Single-source lateral UNNEST ordinal seed (design for Graefe ACK; PROPOSED, pre-impl)

The third W4 per-item PR (after W4-LEFT #458 and W4b #460/#461). Ordinalizes the single-source
lateral-unnest seed (`FROM t, t.arr AS x [AT ord]`) so it survives the S4 name-model deletion.
Multi-source unnest (`FROM A, B, A.arr AS x`) stays name-model — that is **W5**.

### PREMISE CORRECTION (verified against Java source, tag 4.12.11.0)
**The RFC's "RFC-142 multi-source lateral UNNEST — no Java analog" (§ line 773) and the S3-staging
framing of single-source unnest as a Go-only read-side extension are WRONG for the single-source
case.** Java implements `FROM t, t.arr AS x [AT ord]` in `LogicalOperator.generateCorrelatedFieldAccess`
(`LogicalOperator.java:306-355`), reached from `generateAccess` → `resolveCorrelatedIdentifier`
(:217-224), guarded to require an ARRAY-typed column (:309-311) and supporting the AT/ordinality
alias. **⇒ W4c is a genuine Java parity port of the ordinal FORMS**, not an extension riding along.
(W5's *multi-source* bipartition rewrite remains genuinely Go-only — Java has no lateral unnest that
participates in inner-join *re-enumeration*. The "no Java analog" note is correct for W5, wrong for
W4c.)

### Java's three-way branch (the ordinal-form spec)
`generateCorrelatedFieldAccess` (`LogicalOperator.java:318-331`) dispatches by the element shape:
1. **AT / WITH ORDINALITY** → `element = FieldValue.ofOrdinalNumber(flowedObjectValue, 0)`,
   `ordinal = ofOrdinalNumber(flowedObjectValue, 1)` over the 2-field `{element, INT}` record.
2. **primitive/scalar element** → reference `flowedObjectValue` (the whole QOV) **DIRECTLY** — no
   `ofOrdinalNumber`. `ofOrdinalNumber` on a scalar THROWS `FIELD_ACCESS_INPUT_NON_RECORD_TYPE`
   (`FieldValue.java:278`); Go's `NewFieldValueOfOrdinal` enforces the identical guard
   (`values.go:972-975`). The scalar IS the column — you cannot take an ordinal field of it.
3. **record element (no ordinality)** → `convertToExpressions` = `ofOrdinalNumber` per element field
   (field flattening). *Diverged from — see the struct question below.*

Go's Explode matches Java's flow (`explode.go:89` / `ExplodeExpression.java:113`,
`RecordQueryExplodePlan.java:141-145`): bare scalar without ordinality, `{_0,_1}` record with —
confirmed in the executor (`executor.go:3054` bare element; `:3064` `{_0:elem,_1:ord}` map keyed by
`OrdinalFieldName`).

### PROPOSED ruling
1. **Outer leg** (single-source `t`; gate `clusterArity(j.Left)==1`): ordinalize →
   `ofOrdinal(QOV(outer, ordinalLegType(outer)), i)` per column, exactly as W4b/W4-LEFT. A
   multi-source outer DECLINES to the name-model builder (`buildUnnestResultValue`) — that path is
   W5.
2. **WITH ORDINALITY inner**: bake `element = ofOrdinal(QOV(inner, ExplodeOrdinalityResultType(elementType)), 0)`
   and `ordinal = ofOrdinal(…, 1)`, replacing the S4-dying lazy `NewOrdinalFieldValue`. All fields
   baked → this half is a pristine ordinal seed and RUNS `AssertOrdinalJoinSeed`. **ZERO executor
   change**: the Explode already flows `Datum=map{_0:elem,_1:ord}`, which `adaptLegPositional`
   (`rfc173_ordinal_join.go:440-447`) matches by name (`matched=2`, never trips the `:448`
   zero-match guard). RC field **NAMES** are set to the user AS/AT aliases (`X`/`ORD`), NOT `_0`/`_1`
   (else `SELECT x` reports a column named `_0` — W4b precedent, `w4b_scalar_seed.go:68-71`).
3. **WITHOUT ORDINALITY inner** (bare scalar OR struct element): reference the element **DIRECTLY**
   as `NewQuantifiedObjectValueOfType(innerCorr, elementType)` — Java's primitive branch (branch 2).
   The result value is a **MIXED RC** (baked outer ofOrdinal run + a direct-QOV element field). See
   the struct question for why this covers struct elements too.
4. **Executor change (S4-PERMANENT — the one non-translator delta)**: the ordinal-birth binder
   raw-binds a bare-QOV leg. When a leg is referenced by a BARE QuantifiedObjectValue (not a
   FrontierPinned baked FieldValue) and its `QueryResult` carries a non-record/non-positional Datum
   (the RFC-142 bare-scalar element), bind that leg's **RAW Datum** instead of adapting it to an
   OrdinalRow (`bindLeg`/`legRows`/`birthLegBinder`, `rfc173_ordinal_join.go:846-869` — **NOT**
   `adaptLegPositional`, which dies at S4). Then `evaluateOrdinalJoinRow` evaluates `QOV(inner)`
   against the raw scalar and writes it into the positional slot. **Verified necessary by spike**:
   today the mixed RC's element positional slot births as an *empty* `PositionalRow` (birth
   force-adapts a nil-legType scalar leg via `adaptLegPositional`), correct ONLY through the
   coexistence Datum (`flat_map_cursor.go:300-306`) — so it is right today but **broken at S4** when
   the positional row becomes the sole authority. The raw-bind mirrors the existing name-model inner
   bind (`flat_map_cursor.go:317-325`) and the pushdown-WHERE bare-scalar arm
   (`executor_new_plans.go:413-428`) — an established pattern, not a new one. The leg-binding map
   value widens from `values.OrdinalRow` to `any` (or a parallel raw-leg map); the record-leg path
   and `widenLegTypesFromPlan`'s divergence assertions must stay intact.
5. **`AssertOrdinalJoinSeed` scope**: called ONLY on the all-baked WITH-ORDINALITY seed. The mixed
   no-AT RC has a bare-QOV field that fails the frontier-pin check (`ordinal_join_seed.go:31`), so
   the assert is WITH-ORDINALITY-conditional (diverging from W4b, which always asserts) — compensate
   with a dedicated white-box seed-shape pin so the lost tripwire is replaced.
6. **Metadata**: `deriveColumnsFromFlatMap` (`cascades_generator.go:2773`) keys the unnest arm on
   `rc.AnchoredJoin`; the non-anchored ordinal/mixed seed exits into the projected-fold arm — re-route
   so the element reports `elementType` (named by the AS alias) and the ordinal reports `INT NOT NULL`
   (named by the AT alias), not the leg-type `fv.Field`.
7. **Qualified `x.x` / `x.ord`**: the one-field-per-column ordinal shape forbids the name model's
   dotted-duplicate keys; resolve the bare form via the inner QOV correlation and stamp a virtual
   inner-leg source name (the AS alias) for the qualified form.

### OPEN QUESTION for Graefe — a principled + ANSI divergence from Java (per the "not-only-Java when it's principles-first and ANSI-needed" directive)
The **struct-array element** (`t.arr` of rows), no ordinality: Java's branch 3 **FLATTENS** the
struct's fields into separate ordinal columns (`convertToExpressions`); Go today binds the **WHOLE
struct** as one element (`unnestArrayElementType` returns `UnknownType`; `SELECT x` = the row,
`x.field` resolves by name — mirroring Postgres / the ANSI whole-composite binding). **PROPOSAL:
keep Go's whole-object behavior** — extend branch 2 (direct QOV) to struct elements, deliberately
NOT porting branch 3's flattening. Rationale: (a) preserves Go's current read surface (no RFC-142
row-pin churn); (b) matches common-SQL/ANSI whole-composite `UNNEST` binding; (c) the direct-QOV +
raw-bind executor path already carries a struct Datum (a `map`) verbatim, so it costs nothing extra.
This is the one place W4c is *not* a 1:1 Java port. **Graefe to rule ACK/NAK** on the divergence
(and whether struct-element field-flattening is instead a separate future item).

### Considered & rejected (the bare-scalar-element crux)
- **Gate no-AT to name-model** (zero executor change): rejected — defers the COMMON `FROM t, t.arr
  AS x` to S4 with **no representational blocker** (Java shows the clean direct-QOV form; the
  executor already raw-binds the scalar on the name-model path). Unlike W4b's join-inner (a genuine
  two-divergent-typed-QOVs-for-one-alias blocker), this is the CLAUDE.md-forbidden punt on the
  majority unnest shape.
- **Synthesized 1-field `{element}` record + `adaptLegPositional` slot-0 arm**: rejected — a TYPE
  FICTION (the Explode flows a bare scalar, not a record) contradicting both Java and Go's own
  `NewFieldValueOfOrdinal` guard, resting on the fragile "a non-map width-1 Datum is uniquely the
  unnest element" assumption (which **breaks struct-array elements** — a struct Datum IS a map), and
  bolted onto the S4-dying `adaptLegPositional`.
- **Change Explode to flow a `{_0:element}` record**: rejected — diverges from Java's Explode (bare
  scalar flow), and perturbs the name-model dual, the §5 oracle window, `rewriteUnnestPredicate`'s
  non-ordinality arm, and the RFC-142 revert-proof row pins during the coexistence window.

### Gates & pins
- Single-source gate `clusterArity(j.Left)==1` at the `buildUnnestResultValue` call site
  (`cascades_translator.go:1178`); multi-source → name-model (W5), pinned.
- White-box seed-shape pins: with-ordinality single-source → all-baked ordinal seed (no
  AnchoredJoin, passes AssertOrdinalJoinSeed); no-AT single-source → mixed RC (baked outer +
  direct-QOV element, no AnchoredJoin); multi-source → name-model AnchoredJoin.
- **Positional-authority pin (MANDATORY)**: flip `DisablePositionalEmission` and assert the element
  row-for-row — the spike proved the coexistence Datum MASKS the broken positional slot, so a
  Datum-only (S3) row assertion would ship the S4-critical raw-bind fix unverified.
- e2e (extend `array_unnest_ordinality_fdb_test.go`): scalar-array + struct-array non-ordinality;
  WITH ORDINALITY element+ordinal; empty array → no rows; qualified `x.x` / `x.ord`; the existing
  collision/shadow cases (`AS v` shadows a real `VAL`; `FROM t, t.arr AS v, u`) stay green.
- RFC-142's 16 revert-proof pins + the row-content differential survive unchanged.

### S4 impact
**NONE deferred.** The seed is fully S4-safe: outer + with-ordinality inner baked `ofOrdinal`; the
no-AT element a direct QOV (orthogonal to what S4 deletes — no `_N` emulation, no `AnchoredJoin`
marker) bound raw on the positional (S4-surviving) side by the birth-binder change. S4 inherits a
fully-ordinal, correct single-source lateral unnest. (Contrast W4b, which DID leave join-inner /
computed-scalar as S4 blockers.) Bonus: the raw-bind converges `datumFromPositional(pos)` with the
name Datum for the scalar case, letting the bare-QOV Datum-override (`flat_map_cursor.go:291-308`)
eventually be deleted at S4 — a simplification, not new scaffolding.

### Gates (process)
Query-engine change → **Graefe ACK on THIS ruling** (especially: the struct-element divergence Q;
the S4-permanent birth-binder raw-leg change vs the conservative gate) **before any impl**, then the
full four-gate round (Graefe/Torvalds/codex/@claude) on the implementation.
