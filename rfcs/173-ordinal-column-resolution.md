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
  delete `producesMergedRows`/`bindAlias` suppression (the operator allowlist trap); delete
  `InternsAliasAware`'s AnchoredJoin arm only (`select.go:251` — the default-false arm at
  `select.go:274` is the CTE-rename NULL-read guard and survives for its own later slice, F2);
  ~~delete the fake `_<ordinal>` `OrdinalFieldName`~~ **STRUCK (post-W4-left sequencing
  ruling): `OrdinalFieldName` is load-bearing ORDINAL-model infrastructure** (materialized-
  scalar `_0`, WITH-ORDINALITY positions, positional-merge RC — Java's `_i` anonymous-column
  naming) and SURVIVES S4; fold the `LogicalProjection` that used to stack over the
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

### STRUCT-ARRAY element — narrow, ANSI-aligned divergence (Graefe-ACKed; premise corrected)
**Corrected premise (Graefe verified against `LogicalOperator.java:341-353`):** Java's struct
branch does BOTH — it flattens the element's fields via `convertToExpressions` (the `else` arm) AND
**prepends a whole-struct `EphemeralExpression` named by the alias**, so a struct element resolves as
the WHOLE struct (`item` → the row, used e.g. as a UDF argument `get_price(item)`) *and*, redundantly,
as flattened columns (`item.b` → a flat column). So Go's whole-object binding is **Java's own primary
struct binding minus the redundant flattened columns** — a far narrower divergence than "Go diverges
from Java's flattening." **Ruling: keep Go's whole-object binding** (extend branch 2 / direct-QOV to
struct elements; `x.field` resolves via a **nested `FieldValue` on the whole-struct QOV** rather than
a flattened column). Whole-composite is ANSI/Postgres-correct (`UNNEST(row[])` yields one composite
column; flattening requires `(x).*`). The direct-QOV + raw-bind executor path already carries a struct
Datum (a `map`) verbatim. Field-flattening, if ever wanted, is a separate future item.

### Graefe DESIGN-ACK (all four decisions; conditions binding on impl)
1. **Core shape / mixed RC — ACK.** A mixed RC is architecturally sound: an RCV's fields are
   independent Values, nothing in the ordinal model requires field homogeneity, and the "mixedness"
   is a property READ OFF the element type, not an imperative flag. Scalar-element-is-direct-QOV is
   FORCED (both Java and Go throw `FIELD_ACCESS_INPUT_NON_RECORD_TYPE` on `ofOrdinal` of a scalar).
2. **Raw-bind — ACK; the gate — NAK.** The raw-bind is the honest move (a direct-QOV element must
   evaluate against the leg's actual flowed Datum; it is the SAME bind the name path and pushdown arm
   already do; `OrdinalRow` is the wrong type for a non-record leg). **Condition:** discriminate the
   raw arm by leg SHAPE (bare-QOV over a non-record type), NEVER a per-plan flag; keep the record-leg
   `OrdinalRow` path and `widenLegTypesFromPlan`'s divergence asserts intact.
3. **Struct divergence — ACK** with the premise correction above (mandatory). **Conditions:** (a) RFC
   premise corrected [done above]; (b) `x.field` on a struct element — see the impl finding below; (c)
   explicitly pin `SELECT *` → ONE struct column (the intended ANSI divergence) — **DONE**
   (`array_unnest_struct_fdb_test.go`: `SELECT *` over a struct array yields the element as ONE column,
   not flattened); field-flattening is a separate future item.

   **Impl finding on (b) — `x.field` is a GENERAL engine gap, not a W4c concern.** Nested struct-field
   access is unimplemented ENGINE-WIDE, not just for unnest: `SELECT hdr.sku` on a REGULAR (non-array)
   struct column fails identically (`walkColumnRef` handles only 1–2 segments; `ResolveQualifiedColumn`
   does a flat lookup with no struct descent; the executor's `evaluateCorrelated` reads `map`/`OrdinalRow`,
   never a `proto.Message`). So W4c's struct element is consistent with the engine's general struct
   handling: `SELECT x` returns the WHOLE struct (the whole-object binding — pinned and working),
   `x.field` is unsupported the same way it is for any struct column. This does NOT regress struct-array
   unnest (it works as well as it did) and is NOT a bolt-on gap in W4c. Composite field extraction
   (whole-object vs field-access disambiguation across ALL struct columns) is a separate cross-cutting
   query-engine feature needing its own Graefe design ruling — deferred. Graefe's original (b) assumed
   Go had struct-field access; it does not, so the whole-object binding (c) is what W4c pins.
4. **Tripwire + pin — ACK.** WITH-ORDINALITY-conditional `AssertOrdinalJoinSeed` is correct (the
   mixed RC's direct-QOV field legitimately fails the `FrontierPinned` check; asserting would
   false-trip); the white-box seed-shape pin replaces it. **Condition:** the mandatory
   positional-authority pin (flip `DisablePositionalEmission`) must assert the element slot VALUE, not
   just row count.

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

### W4c impl correction: ENCLOSURE-decline — only ISOLATED single-source unnest ordinalizes
Impl surfaced that `clusterArity(j.Left)==1` is necessary but NOT sufficient (the same
narrowing W4b hit with the join-inner gate and W4-LEFT with buried-eligibility). An ordinal seed
that is ENCLOSED in a larger name-model composition is flattened by `SelectMergeRule` into a
name-model parent whose machinery **cannot consume a baked, non-anchored ordinal leg** and PANICS.
Three enclosure cases, all the established W2/W5 "enclosure = poison, name-model until W5" boundary —
the W4c gate declines all three (`clusterArity(j.Left)==1 && !prevEnclosure && !unnestUnderExistential`):
- **Multi-source OUTER** (`FROM A, B, A.arr AS x`): `clusterArity(j.Left) > 1` — flattening the outer
  cluster to a bare concat erases buried source names (W5).
- **Enclosed leg** (`FROM A, A.arr AS x, B`): the unnest is a leg of a larger multi-source join
  (`prevEnclosure`). A GROUP BY / aggregation over it re-enumerates via `PartitionSelectRule`, whose
  `NewReEnumerationAnchoredRecord` cannot resolve a non-anchored ordinal-seed leg (panic). A bare
  projection over such a leg WOULD work via the coexistence Datum's qualified keys, but the
  aggregation path forces the whole class name-model, and the two are **indistinguishable at
  unnest-lowering** (they differ only by a GROUP BY processed later). So the class stays name-model.
- **EXISTS-composed** (`… WHERE EXISTS(… unnest …)`): the unnest is the OUTER of a semi-join whose
  implementation rebases outer-leg refs NAME-model (`rebaseOuterLegValue`) and panics on baked refs.

**Consequence (Graefe to ACK):** W4c's ordinal reach narrows to the **ISOLATED** single-source
lateral unnest (`FROM t, t.arr AS x [AT ord]` as the whole FROM, or projected/filtered but not merged
into a larger cluster/aggregation/existential). Enclosing multi-source/aggregation/existential
compositions ordinalize in **W5** (multi-source unnest + N-way re-enumeration), which is before S4 —
so no new S4 blocker beyond the already-scheduled W5. The alternative (keep enclosed single-source
unnest ordinalized) requires `PartitionSelectRule` to support anchored re-enumeration over an
ordinal-seed leg — genuinely W5-scoped surgery, deferred. This is the same fail-closed decline
discipline Graefe endorsed for W4b (unrecognized/unconsumable shape → name-model, never a panic).

### W4c impl correction: ordinal-alias collision → bind the ordinality leg by PRODUCER CONTEXT
A WITH-ORDINALITY Explode flows a per-row Datum keyed by the INTERNAL `OrdinalFieldName` positions
(`_0`=element, `_1`=ordinal), while the seed names the inner leg type by the user AS/AT aliases. If a
user spells an alias `_0`/`_1` (`FROM t, t.arr AS "_1" AT "_0"` — valid quoted identifiers), a
NAME-based leg bind reads the wrong internal key and `SELECT "_1"` returns the ordinal. A
Datum-SHAPE discriminator (bind positionally iff the Datum keys are exactly `_0..n-1`) does NOT
suffice: a **name-model** leg (e.g. a UNION box) whose own columns are aliased `_0`/`_1` is
shape-identical yet must bind by NAME — and the fully-colliding `AS "_1" AT "_0"` is indistinguishable
from it by shape alone. **Ruling: disambiguate by PRODUCER CONTEXT.** `newFlatMapCursor` marks the
inner leg as an ordinality leg iff its inner plan IS a WITH-ORDINALITY Explode
(`innerIsOrdinalityExplode` — the FlatMap knows its producer), and the birth binds a marked leg
STRICTLY POSITIONALLY (`OrdinalityLegs`: slot i = Datum[`_i`]); `adaptLegPositional` reverts to pure
name-match, so a name-model leg aliased `_0`/`_1` binds correctly by name. The **§5 name-model
oracle** path (`DisablePositionalEmission`, test-only, bypasses `bindLeg`) needs the same producer
context: `oracleNameDatum` reads the baked element/ordinal fields BY NAME (`OracleBakedNameFallback`),
so the FlatMap rebinds the ordinality inner under its AS/AT alias names (positionally, `_i` → field-i
name) before the oracle evaluates — else the oracle read `V`/`O` as missing against the `_0`/`_1`
Explode Datum and returned wrong rows (a latent dualwindow gap). Pins:
`TestRFC173W4c_OrdinalAliasCollision` (the marked ordinality leg binds positionally for `AS "_1"`,
`AS "_1" AT "_0"`, AND a shape-identical name-model leg binds by name); `TestRFC173W4c_OracleOrdinality`
(the oracle returns the element/ordinal, not missing — red without the rebind); e2e
`SELECT "_1" … AS "_1" AT "O"` + the WHERE-on-ordinal composition.

### Gates (process)
Query-engine change → Graefe design-ACK **before impl** DONE, then the full four-gate round on the
impl — **ALL DONE. MERGED as PR #463** (rebase-merge; Graefe/Torvalds/codex/@claude all ACK'd the
final SHA; codex caught 3 real edge bugs across rounds — ordinal-alias collision → producer-context
disambiguation → §5 oracle-path gap — each fixed with a red→green pin). The enclosure-decline scope
narrowing was ACK'd by Graefe. **The entire W4 tier (W4-LEFT/W4b/W4c) is now merged.**

---

## Slice 3 — dispatch-authority flip: impl design (Graefe DESIGN-ACK on reframe; PRE-IMPL)

W4c merged (PR #463) — the entire **W4 tier is done** (W4-LEFT #458, W4b #460/#461, W4c #463). Slice 3
is the hard core (§4 slice order; §8 risk 1; §10 requires reviewer sign-off BEFORE the first impl
commit). **Graefe's design gauntlet reframed it** from an "atomic deletion" to a **dispatch-authority
flip + certification** slice: make the already-live positional/identity/ordinal-interning machinery
authoritative and pin it; the anchored trio and its machine are deleted in **Slice 4**. This section
is that (corrected) design.

### STATE RE-SYNC — the RFC's Slice-3 map is STALE; most of the "atomic core" already landed dark
A 6-agent code map (banked) found that the Slice-3 prose over-states remaining work. **Already landed
on HEAD, gated/dark in the coexistence window:**
- **(d) FieldValue node-identity flip (name→ordinal) for BAKED nodes** — `map_field_values.go`
  `av.Resolved.Equals` ordinal-only; `semantic_hash.go` folds `#ordinal` only (Java
  `ResolvedAccessor.equals` getOrdinal()-only, FieldValue.java:676-690). The RFC's cited anchors
  (`map_field_values.go:260-262`, `semantic_hash.go:108`) are STALE (now `ptrEqual` / an RC-hash arm).
  Only the LAZY-node `av.Field==bv.Field` arm survives — it dies at **full FieldValue baking (its own
  slice), NOT Slice 4** (Graefe F3 ruling: the lazy-identity arm fires only when BOTH nodes are lazy
  `Resolved==nil`, gated on the ~22 `NewFieldValue` sites all baking — a strictly broader axis than the
  anchored-seed flag; deleting it with the flag commit would break equal⟹same-hash on any surviving
  lazy node).
- **(f) Collapsed multi-accessor FieldPath + compose rule** — `FieldPath` is the collapsed
  `[]ResolvedAccessor` list (W1); `composeFieldOverField` ported + gated both-baked; the primary
  producer is the `withChildren` rebuild-fuse (`replace.go:369-373` = Java `withNewChild`). A buried
  leg ref is a COLLAPSED multi-accessor node, never chained `FieldValue(FieldValue(...))`.
  `ExpandFusedFieldValueRule` correctly NOT in the simplification ruleset (matching-only,
  `max_match_map.go`).
- **Positional re-enumeration** — `positionalMergeCase` (`rfc173_positional_merge.go`) is a live 1:1
  port of Java `PartitionSelectRule.java:283-322` (unnamed positional row `RC(_i: QOV(live_i))` + a
  `TranslationMap.When(live_i).Then(ofOrdinalNumber(QOV($m), i))`) — it runs TODAY on the
  `!parentIsMerge` (ordinal) arm.
- **Alias-bijection interning tier** (`MemoEqual`) + the two ORDINAL structural merge-select probes
  (`IsPositionalMergeRC`, `IsOrdinalJoinRV`) — live, additive/dark alongside the name-model
  `AnchoredJoin` arm of `InternsAliasAware` (`select.go:242-271`).
- The live task-count baseline is **`{3, 11122}` / `{4, 45306}` ±2%** (`partition_select_interning_baseline_test.go:259-260`),
  re-baselined by RFC-150/152 — the RFC's `8999/30593` is the OLD master number.

### The FLIP — reframed per Graefe's design gauntlet (DESIGN-ACK'd; trio deletion moved to S4)
> **Graefe NAK'd the v1 plan** (delete the re-stamp trio + `InternsAliasAware` AnchoredJoin arm IN
> Slice 3) and gave the correct reframe. The v1 premise — "widen the gate → `parentIsMerge` is never
> true → the trio is dead code" — is **FALSE**: anchored seeds stay CONSTRUCTIBLE after the widen.
> `NewScalarSubqueryAnchoredRecord` (computed-scalar / join-inner / clustered-outer correlated
> subqueries decline the ordinal wedge and seed anchored), the multi-source lateral-UNNEST bipartition
> (name-model until W5), and same-quantifier anchored selects all still reach the merge arm with
> `parentIsMerge=true`. **You cannot delete the consumer (trio) while its producers (anchored seeds)
> are live.** Deleting the `InternsAliasAware` AnchoredJoin arm with those seeds present reproduces the
> `29915→60044` interning blowup un-gated. So the trio + interning arm + the `AnchoredJoin` flag +
> seed constructors ALL die together in **Slice 4**; W5 stays a **separate post-green PR**. Slice 3 is
> a **dispatch-authority flip**, not a deletion.

**Slice 3 = make the already-live positional/identity/ordinal-interning machinery AUTHORITATIVE, and
certify it. The anchored arm + trio SURVIVE as the residual path** for name-model shapes (multi-source
unnest until W5; scalar-subquery / non-wedge anchored seeds until S4).

1. **Widen is ALREADY DONE.** `rfc173_cluster_gate.go:124-125` (`a := t.clusterArity(j)` / `if a >= 2`)
   already admits every maximal inner-join cluster to ordinal seeding; `positionalMergeCase` already
   re-collapses subsets. Slice 3 does **not** re-touch the gate — it makes the positional arm the
   **SOLE producer for every ordinal-seeding shape** and certifies that authority with the gate pins
   below. (No `parentIsMerge`-goes-dead claim; the anchored arm keeps firing for the residual shapes.)
2. **Node-identity + collapsed-FieldPath + compose** (items (d)/(f) in the re-sync above) are the
   authoritative baked-node behaviour in S3 — the lazy name-identity arm and the anchored trio remain
   only as the name-model residual, deleted in S4.
3. **Ordinal interning is authoritative in S3, additively.** The two ORDINAL structural probes
   (`IsPositionalMergeRC`, `IsOrdinalJoinRV`) are the interning key for every POSITIONAL merge-select.
   The `AnchoredJoin` arm of `InternsAliasAware` (`select.go:251`) **STAYS** — it is the interning key
   for the surviving anchored residual. Do NOT stamp `AnchoredJoin=true` on the positional RC
   (recognize the positional row STRUCTURALLY). Deleting that arm is **S4**, gated on the seeds being
   gone.
4. **`pullUpResultColumns` — `legRowTypes` is BLESSED as the Go bridge (Q4).** `positionalMergeCase`'s
   ad-hoc leg-type recovery is the accepted equivalent of Java's typed merge-quantifier pull-up
   (`Quantifier.java:759`) for S3 — **conditioned on the three Q4 pins** (below). A first-class typed
   pull-up is NOT required for S3; if a future N-way shape can't recover a leg type, THAT is when the
   typed surface gets ported (named owner: Slice 5, with the local-bind subtraction work).
5. **Author the two MISSING gate pins** (neither exists in code) — these are the load-bearing S3
   deliverables, authored IN the flip PR (Q6):
   - **STAR planning wall-clock** bound on a many-identical-legs corpus — `TestRFC173S3_OrdinalStarPlanningBudget`
     (`rfc173_s3_star_budget_test.go`). A fixed 4-way ordinal STAR (hub + 3 identical spokes, all live)
     pinned four ways: converges (no 100k cap), task-count `51377` ±2% (STAR-topology interning
     sentinel next to the CHAIN baseline), `MergeArmHits==0`, and wall-clock < a generous 5s ceiling
     (the per-Insert bijection-cost catch the count is BLIND to; ~90ms observed, catastrophe detector
     not micro-benchmark). Measured budget-IDENTICAL to the name-model star (representation-neutral).
   - **shadow-delta equivalence** — `TestRFC173S3_AliasAwareInterningShadowDelta`
     (`rfc173_s3_shadow_delta_test.go`), promoting the spike's `t.Logf` to EXACT assertions. The
     alias-aware interning tier is instrumented with a per-Reference dedup counter
     (`Reference.AliasAwareDedups`, summed by `Memo.AliasAwareDedups` — the "shadow") plus a
     test-only alias-identity baseline toggle (`SetDisableAliasAwareInterning`, race-free because the
     pin is non-parallel). 3-chain: shadow **exactly 4**, member-count delta **exactly 20** (68→88
     with the tier off — shadow < delta because each collapsed sub-product re-explodes). 4-chain:
     shadow **exactly 56**, and the tier is LOAD-BEARING — planning converges with it on, blows the
     100k budget with it off (the 29915→60044-class re-explosion the tier exists to prevent). Not
     the naive shadow==delta (cascade makes delta larger); the equivalence is exact shadow ↔ exact
     convergent delta ↔ load-bearing at scale.
6. **Plan-baseline audit is a HARD GATE (Q2)** — a composite of four concrete pins, so a representation
   flip is never mistaken for a plan change:
   - **merge-branch hit-count == 0** on the ordinal-seed corpus — the positional arm, not the anchored
     arm, produced every ordinal-seed plan. `TestRFC173S3_OrdinalSeedDispatchAuthority`
     (`rfc173_slice3_dispatch_authority_test.go`); the anchored control observes 4/42 hits (the 42
     matches the interning-baseline prose), every ordinal chain 0.
   - **plan-SHAPE byte-stable** on the N-way ordinal-seed corpus (arity ≥ 3, catalog-aware so the
     ordinal seed actually fires) — the Java-independent half of "plandiff byte-identical", planned 5×
     for determinism. `TestRFC173S3_OrdinalPlanShapeStable`
     (`plandiff/rfc173_slice3_ordinal_plan_stability_test.go`). This is the forward-looking Slice-4
     safety net: deleting the trio must NOT move these shapes.
   - **plandiff BYTE-IDENTICAL vs Java** — cross-engine half: `select_inner_join` in `SeedCorpus` pins
     the 2-way ordinal plan TREE == Java; the N-way inner-join `SeedRunCorpus` entries pin Go ROWS ==
     Java rows (a wrong ordinal plan yields wrong rows). Both green.
   - **task-count within ±2%** of `{3,11122}/{4,45306}` (`partition_select_interning_baseline_test.go`);
     any movement beyond that is a regression to root-cause, NOT a re-baseline (the flip changes
     representation, not exploration sharing — that was the v1 error).
7. **Extend the merge-marker tests to the S3-authoritative markers** (CORRECTED from v1: the reframe
   keeps the anchored path in S3, so its tests are RESIDUAL coverage that STAYS — the anchored-assertion
   rewrites move to Slice 4 with the trio deletion, per Q5). Done in S3: the `InternsAliasAware` gate
   test (`partition_select_interning_baseline_test.go:115`) now pins that ALL THREE merge markers opt in
   — the name-model `AnchoredJoin` (residual), the S3-authoritative `IsPositionalMergeRC` and
   `IsOrdinalJoinRV` — while the projSel non-merge branch keeps CTE-NULL protection intact. **Deferred to
   Slice 4** (they assert the surviving anchored residual, valid until the trio dies):
   `TestPartitionSelect_SeedMergeRestampedOverMergeQuantifier` (`rule_partition_select_test.go`, asserts
   the merge-case upper is an `AnchoredJoin` RC) and the re-stamp-assert sibling in
   `rfc173_slice2_drift_assert_test.go` (`TestRFC173S2_SelectMergeDriftAssert_Fires` positive compose
   pins STAY regardless).

### FORKS — Graefe's rulings (RESOLVED; DESIGN-ACK on the reframed text)
- **Q1 (SEQUENCING) — RESOLVED: (b), scoped, NOT (a).** Do **not** fold W5 into the flip (its safety
  rests on P2/P3 merged-and-green; folding its least-proven rewrite un-gated is exactly the risk the
  gauntlet exists to catch). Do **not** delete the trio in S3. Slice 3 is the dispatch-authority flip;
  the anchored arm survives as the residual for multi-source unnest (→ W5) and non-wedge anchored seeds
  (→ S4). W5 remains a separate post-green PR.
- **Q2 — RESOLVED: no re-baseline; it's a HARD equality gate.** The flip is a representation flip, so
  plandiff is BYTE-IDENTICAL and merge-branch hit-count on the ordinal-seed corpus is 0. Task-count
  stays within ±2% of `{3,11122}/{4,45306}`; anything past that is a regression, not a re-baseline.
  (v1's "the flip legitimately re-baselines exploration sharing" was wrong — the positional arm was
  already live; making it authoritative doesn't change what's explored, only which arm stamps it.)
- **Q3 — RESOLVED: no hide/expose replacement needed.** The unnamed positional row's QOVs are the
  select's OWN bound quantifiers, so their correlations subtract naturally in `GetCorrelatedToOfValue`
  — no AnchoredJoin-style hide/expose duality. The RFC-077 F2 duality stays only for the surviving
  anchored residual and dies with it in S4. Slice-5's local-bind subtraction carries the ≥4-way STAR
  budget for the positional path (verified by the S3 STAR wall-clock pin, item 5).
- **Q4 — RESOLVED: bless `legRowTypes` as the S3 bridge, under three pins** — `TestRFC173S3_LegRowTypesBridge`
  (`rfc173_s3_legrowtypes_test.go`). (i) **no-untyped-slot**: every live leg of the ordinal-seed corpus
  (chain 3/4-way, star with mixed leg widths hub[ID]+spokes[ID,HID]) recovers a concrete `*RecordType`
  — a `_UntypedSlotControl` negative control proves the pin has teeth (a leg referenced nowhere is
  recovered as absent → the untyped slot the pin forbids). (ii) **recovered == flowed**: the
  reconstruction equals the leg's authoritative flowed type — the NULLABLE record a typed pull-up over
  the merge quantifier's flowed QOV yields (Go's `QuantifiedObjectValue.Type` nullable-izes like
  Java's), asserted per-leg with no conflation/drop/width-drift. (iii) **named typed-pull-up owner** —
  Slice 5 ports the first-class typed surface if any N-way shape defeats ad-hoc recovery.
- **Q5 — RESOLVED: trio + `AnchoredJoin` flag + `NewAnchoredJoinRecord`/`NewScalarSubqueryAnchoredRecord`
  + the `InternsAliasAware` AnchoredJoin arm ALL die TOGETHER in S4.** They cannot be split — the flag,
  the seeds, and the consumer are one machine; deleting any subset while the rest is live either
  breaks a live seed or un-gates the interning blowup. S3 touches NONE of them.
- **Q6 — RESOLVED: this section IS the re-sync; the two gate pins are authored IN the flip PR.** Stale
  anchors corrected in the STATE RE-SYNC block above. The STAR wall-clock + shadow-delta pins ship in
  the flip PR (item 5).

### Gates (process)
Query-engine change → Graefe design-ACK on the reframed Q1-Q6 above — **DESIGN-ACK OBTAINED** on this
corrected text (first impl commit unblocked; his basis: the `ordinalWedgeGateDecide` declines keep
anchored seeds constructible, so the trio's producers are live and the consumer cannot die in S3).
Next: the §10 gauntlet (Graefe/Torvalds/codex/@claude sign-off) on the flip PR. The flip is a
certification + authority slice, not a deletion: a red gate pin (STAR wall-clock, shadow-delta,
plandiff-byte-identical, merge-branch-hit-count, or any Q4 pin) BLOCKS the slice. Deletions (trio,
flag, seeds, interning arm) are **Slice 4**, gated on S3 green. **S3 LANDED — MERGED as PR #464
(rebase `38454886a`, 2026-07-03); all four gates ACK'd the final HEAD, CI green.**

## Slice 4 — retire AnchoredJoin: boundary design (PRE-IMPL; for Graefe design gauntlet)

A 6-agent producer-retirement map of the AnchoredJoin machinery (81 prod + 73 test refs / 18 files)
found the decisive fact: **Slice 4 is a GATED DEMOLITION, not a free delete.** After S3 the flag axis
has ZERO individually-dead symbols except the §5 name-model oracle. The `AnchoredJoin` flag lives on
the SHARED `RecordConstructorValue` type, so it — and every flag-keyed branch — is undeletable while
ANY seed constructor survives, and FOUR seed producers are still live, each gated on a distinct
prerequisite W-item.

### KILLABLE IN S4 TODAY — NOTHING (Graefe ruling)
The §5 dual-window differential oracle (`executor/rfc173_ordinal_join.go:oracleNameDatum` + globals
`executor.DisablePositionalEmission`, `values.OracleBakedNameFallback`, `SetNameModelOracle` + caller
`flat_map_cursor.go:376` + the differential-harness tests) is **NOT killable today** — it is the
ordinal↔name-model differential NET, and with four producers mid-flip it is **load-bearing until the
LAST producer ordinalizes**. It dies IN the S4-final atomic commit, never before. (My caution was
right; Graefe reclassified it out of "killable now.") So there is NOTHING to delete before the S4-final
commit — every prerequisite is an ordinalization slice, not a deletion.

### SURVIVING RESIDUAL (S4 cannot delete — four live seed producers)
1. `NewScalarSubqueryAnchoredRecord` (`value_anchored_join_record.go:121`, live at
   `cascades_translator.go:3256`) — computed / JOIN-inner / clustered-outer correlated scalars → **W4b**.
2. `buildUnnestResultValue` AnchoredJoin (`cascades_translator.go:1405`, live at :1223) — multi-source
   / enclosed / under-existential lateral unnest → **W5**.
3. `buildJoinResultValue`→`NewAnchoredJoinRecord` (:660, live at :3606/:3751) — LEFT/RIGHT-OUTER box,
   EXISTS-over-join, mixed-nesting, dup-alias, recursive-CTE-enclosed joins → **W4-left + EXISTS +
   recursive-CTE**.
4. `NewReEnumerationAnchoredRecord` (:259) — GROUP-BY over any anchored parent; dies only when NO
   anchored seed remains (i.e. after 1–3).

Because these live, the WHOLE consumer set stays: the trio (`buildUpperResult`,
`rebaseBuriedLowerReferences`, merge arm, panics, FrontierPinned tripwire, `isAnchoredJoinResult`,
`MergeArmHits`), the 8 flag-keyed value-layer branches, the four executor consumers, and
`select.go:251` interning arm-1.

### FORKS — Graefe's rulings (RESOLVED; DESIGN-ACK on the boundary)
- **F1 — RESOLVED: PREDECESSOR slices.** W4b, W5, W4-left, EXISTS, recursive-CTE are EACH their own
  four-gate slice (each is a full ordinalization touching the executor — none rides free inside a
  "multi-step S4"). **S4 ≡ the single atomic final commit**: delete the flag, the trio, the three seeds
  + `NewReEnumerationAnchoredRecord`, the 8 value-layer flag branches, the four executor consumers, and
  `select.go:251` arm-1 — nothing else. **Canonical sequence: W4b → W5 → W4-left+EXISTS+recursive-CTE →
  S4-demolition.** Item 4 (`NewReEnumerationAnchoredRecord`) needs NO slice — it dies mechanically once
  1–3 leave no anchored parent.
- **F2 — RESOLVED: two axes, NOT coupled.** Deleting arm-1 (`select.go:251`) RIDES the flag commit:
  once the producers bake, every anchored shape flows through `IsPositionalMergeRC`/`IsOrdinalJoinRV`
  (`:262`/`:271`) which already return true — arm-1 is subsumed, safe to drop IN S4. The default
  `return false` (`select.go:274`, CTE-rename NULL-read guard) is a **SEPARATE, LATER widening — its
  OWN slice, NOT in S4** — gated on CTE column-rename ordinalization (`cascades_generator.go
  ~:2837/:2867`). Keep `return false` through the demolition; widen only when
  `TestFDB_CTEChainedColumnAliases`/`TestFDB_CascadesCTEColumnAliases` return renamed cols, not NULL.
- **F3 — RESOLVED: the MAP is correct; the RFC "(d)" text is a DOCUMENTATION BUG (fixed below).**
  `map_field_values.go:339` + hash twin `semantic_hash.go:138` fire only when BOTH nodes are lazy
  (`Resolved==nil`) — gated on the ~22 `NewFieldValue` sites all baking, a strictly BROADER axis than
  the anchored seeds. **Death gate = full FieldValue baking (its own slice), NOT S4.** The lazy arm
  SURVIVES the demolition; deleting it early breaks equal⟹same-hash on any surviving lazy node.
- **F4 — RESOLVED (confirmed):** delete only the trio's `leftmostQOV`
  (`value_anchored_join_record.go:192`) with the trio; KEEP `leftmostQOVOfValue`
  (`value_correlation.go:56`) — it feeds `MergeSeedLegsOfValue`/RFC-142 and dies in **W5**, not the
  flag commit.

### RISK (banked from the map)
- **WIRE: none.** The entire axis is read-side (column resolution / index selection) — no key encoding,
  record/index/version format, or continuation touched.
- **Fail-loud invariants to re-prove on deletion:** `MergeArmHits`==0, the FrontierPinned tripwire,
  panics L466/L562 die with the trio. `AssertOrdinalJoinSeed` does NOT catch name-model regressions —
  deletion must be gated on POSITIVE ordinal-coverage proof, not that assert.
- **Cost regression the flag prevents:** premature arm-1/Equals/hash deletion re-explodes interning
  29915→60044 (≥4-way STAR task-budget blowout) and conflates anchored/plain RCs → dropped buried
  columns → 0-row joins. (This is the exact class the S3 shadow-delta + dispatch pins now guard.)

### Gates (process)
Query-engine change → Graefe design-ACK on F1–F4 — **DESIGN-ACK OBTAINED** (boundary rulings folded
above; the S4-final atomic-demolition commit stays gated on the predecessor ordinalization slices).
The immediate EXECUTABLE next-step is not a deletion at all — it is the lowest prerequisite producer's
ordinalization (**W4b** correlated-scalar, per RFC §649-653 "S4 cannot land while any correlated-scalar
shape still depends on `NewScalarSubqueryAnchoredRecord`"), itself a full four-gate slice. Each
predecessor (W4b → W5 → W4-left+EXISTS+recursive-CTE) is its own slice; then the S4 demolition; the
CTE-rename `select.go:274` widening and the full-FieldValue-baking lazy-arm deletion are their OWN
later slices (F2/F3). The demolition is the LAST step of the flag axis.

## W4b (residual shapes) — correlated-scalar ordinalization: the S4-prerequisite completion

**Continues the W4b design above** (§"W4b — Correlated-scalar 2-leg ordinal seed", where the SIMPLE
case landed). **FRAMING (from that section's premise correction):** correlated-scalar-subquery-in-
projection is a Go-only READ-SIDE EXTENSION — Java does NOT parse a scalar subquery in a projection
list at all. So this is NOT a parity port; Java is only the **ordinal-SHAPE** reference (`convertToExpressions`
bakes `FieldValue.ofOrdinalNumber(quantifier.getFlowedObjectValue(), i)`, `LogicalOperator.java:358-372`).
The "read Java" below means *mirror Java's ordinal-shape mechanisms* (ordinal addressing, unique
quantifier ids), not "port a Java feature". The item exists because the extension's seed is a name-model
`NewScalarSubqueryAnchoredRecord` that BREAKS at S4 deletion unless ordinalized — an extension that
rides along.

The single live caller of `NewScalarSubqueryAnchoredRecord` is `cascades_translator.go:3256`, on the
DECLINE of the W4b ordinal gate at `:3251`. The SIMPLE case is ALREADY ordinalized
(`scalarSubqueryOrdinalSeed`, `:3253`): outer `clusterArity==1` AND `!innerContainsJoin` AND
`innerScalarIsRowColumn`. W4b must ordinalize the **three residual shapes** that still decline (each
with a concrete blocker to solve — read Java first per shape):

1. **Clustered / multi-table outer** (`clusterArity(p.Input) > 1`) — ordinalizing a flattened cluster
   erases buried source names (the same hazard the join wedge faces). Needs the outer leg's per-source
   columns recoverable positionally without the dotted `SRC.COL` names.
2. **JOIN-inner** (`innerContainsJoin(csq.InnerPlan)`) — the inner subquery's first table shares
   `csq.InnerAlias`, so the inner ordinal join's typed `QOV(InnerAlias)` collides with the seed's typed
   inner leg (`widenLegTypesFromPlan` divergent-baked-types). Needs InnerAlias/inner-leg disambiguation.
3. **Computed (non-row-column) scalar** (`!innerScalarIsRowColumn`) — the scalar is a computed
   expression, not a direct row column, so there is no single leg column to anchor/bake.

Each is a distinct sub-problem; the slice ordinalizes all three (or Graefe may split them). Deliverable:
`NewScalarSubqueryAnchoredRecord` has ZERO live callers → it joins the S4-demolition kill set. Gate:
Graefe design-ACK on the per-shape approach (read Java's correlated-scalar handling first) BEFORE impl,
then the §10 four-gate gauntlet. Coverage proof: correlated-scalar yamsql/FDB tests over all three
shapes return correct rows via the ordinal seed (EXPLAIN + rows), not the name-model fallback.

### W4b design (Java-grounded; for Graefe design gauntlet)
Java's model (confirmed by source read): a quantifier's SQL name and its correlation KEY are distinct —
`Quantifier.forEach` mints a globally-unique `CorrelationIdentifier` (`Quantifier.java:175/842`), every
column is addressed by ORDINAL (`pullUpResultColumns` → `FieldValue.ofOrdinalNumber(QOV(alias,rt), i)`,
`Quantifier.java:759-769`), and correlated refs rewrite to ordinals via `Value.pullUp`. Per shape:

- **Shape 3 (computed scalar) — TRIVIAL, do first.** Java's inner `resultValue` is a
  `RecordConstructorValue` of the SELECT items; a scalar subquery flows ONE column at ordinal 0, so
  `ofOrdinalNumber(innerQOV, 0)` reads it whether it's `c.name` or `c.a+c.b` (`SelectExpression.java:112-132`).
  Go's `innerScalarIsRowColumn` guard (`cascades_translator.go:3252`) is STRICTER than Java — DROP it;
  the ordinal seed's inner leg is `ofOrdinalNumber(QOV(innerAlias, innerType), 0)`.
- **Shape 1 (clustered outer) — MODERATE.** Java never name-erases the cluster: N distinct aliased
  operators, `SemanticAnalyzer.lookup` matches `t1.col` → `ofOrdinalNumber(QOV(t1), ord)`
  (`SemanticAnalyzer.java:442-490`). Go's flatten works ONLY if it retains a per-source→global-ordinal
  map over the flat cluster record type (the buried-reference map — reuse the join-wedge's, if the
  cluster infra already exposes it). The outer leg becomes `ofOrdinalNumber(clusterQOV, globalOrd)`.
- **Shape 2 (JOIN-inner) — HARD, entangled with TODO-7.1.** The `InnerAlias` collision is Go's
  namespace non-unification: Go keys the inner QOV by `NamedCorrelationIdentifier(InnerAlias)`, so a
  JOIN-inner whose first table shares that SQL alias collides. Java has NO collision — quantifier keys
  are unique generated ids, SQL names live only on `LogicalOperator.name` for lookup. Fix mirrors Java:
  give the inner quantifier a UNIQUE correlation id decoupled from the SQL alias.

### FORKS — Graefe's rulings (RESOLVED; DESIGN-ACK) + the impl plan
- **W1 — RESOLVED: do ALL THREE in W4b.** Because W2 is a local decouple (below), shape 2 needs no
  deferral, `NewScalarSubqueryAnchoredRecord` reaches ZERO callers within W4b, and **S4 gains NO new
  predecessor** — the roadmap holds (W4b → W5 → W4-left+EXISTS+recursive-CTE → S4). Three
  dependency-free commits, exit-gate = a **zero-callers assertion** on the constructor, then the §10
  four-gate gauntlet.
- **W2 — RESOLVED: W4b-LOCAL decouple, not TODO-7.1.** The `InnerAlias` collision has exactly TWO
  reference sites, both minted here: the seed's inner-leg QOV and `replaceScalarSubqueryRef`
  (`cascades_translator.go:3273-3276`). Give `innerQ` a fresh UNIQUE correlation id (as
  `namedQuantifier("")` already mints unnamed ids), thread it through BOTH sites, leave the internal
  first-table `o` untouched (correlation `o.cid=c.id`→outer `c` unaffected). At `widenLegTypesFromPlan`
  the collision dissolves: seed leg `QOV(freshId,1)` vs internal `QOV(o,N)` — distinct. **Impl
  condition: audit EVERY `csq.InnerAlias` consumer** (StrictSingle construction `:3210`, executor
  null-leg / strict-single birth) and thread the id consistently.
- **W3 — RESOLVED: REUSE, don't build.** `gatedJoinLegTypes(outerCluster)` returns
  `srcAlias → bakeLegType{typ, leafOffset, leafTyp}`; `leafOffset + leafTyp.FieldIndex(COL)` IS the
  per-source→global-ordinal of `QOV(outer)`'s flat concat (`rfc173_ordinal_seed.go:190-239`). Shape 1
  threads dotted `T1.COL` outer refs through that exact spine (as `bakeGatedJoinPredicates` does). Two
  conditions: (a) gate the outer on `ordinalWedgeGateDecide(outerCluster).Gated`, NOT merely
  `clusterArity>1` (an ungated outer flows name-model dotted rows → wrong positional read); (b) BAKE the
  outer projections (a cluster has no single `RecordName` span for one `ofOrdinal` run).

### IMPL PLAN (Graefe-ACK'd; three dependency-free commits, shape 3 → 1 → 2)
1. **Shape 3 (computed scalar) — FIRST, smallest.** NOT merely "drop the `innerScalarIsRowColumn`
   guard": the trap (impl-correction-2) is that the computed scalar is applied ABOVE the inner, so
   ordinal-0 of the flowed row is a SOURCE column, not the scalar. **Relocate the computation into the
   inner's `resultValue`** (mirroring Java's `SelectExpression.resultValue` = RC of select items, so
   ordinal-0 = the computed scalar); the guard drop is the consequence. White-box + FDB EXPLAIN/rows pin.
   - **Impl investigation (banked):** the construction is `logical_predicate.go:5900-5918` (non-aggregate
     `len(sq.projCols)==1`) — it sets `scalarCol` to the projection TEXT (`UPPER(ENAME)` → synthesized
     name) but adds NO inner `LogicalProject` computing it; `innerOp` stays the raw scan+filter. So
     shape 3 must ADD an inner projection whose single output IS the computed expression (ordinal-0),
     then the ordinal seed's inner leg reads `ofOrdinal(QOV(inner),0)`.
   - **⚠ FINDING (FDB-probed on master): computed correlated scalars are SILENTLY BROKEN today.**
     `SELECT (SELECT UPPER(c2.name) FROM customers c2 WHERE c2.id = c.id) FROM customers c WHERE c.id=1`
     returns **NULL**, not `ALICE` — the name-model path never computes the scalar (the synthesized name
     is not an inner-row key; the computation lands nowhere). So **shape 3 is a SILENT-NULL BUGFIX**, not
     merely an ordinal flip (this validates Graefe's "not just drop the guard" ruling and quantifies it).
     The shape-3 impl relocates the computation into the inner's `resultValue` so BOTH models resolve it,
     and ships the now-PASSING `…=ALICE` FDB test (red-first: NULL before the fix). The existing
     `ComputedGate` unit test only pinned the GATE decision, never end-to-end rows — the classic
     dimensional gap that let the silent-NULL ship green.
2. **Shape 1 (clustered outer).** Gate on `ordinalWedgeGateDecide(outer).Gated`; bake outer projections
   through `gatedJoinLegTypes` (`leafOffset + FieldIndex`). White-box + FDB pin.
3. **Shape 2 (JOIN-inner).** Fresh unique id for `innerQ` threaded through the two mint sites + every
   `csq.InnerAlias` consumer. White-box + FDB pin.
Exit gate: a test asserting `NewScalarSubqueryAnchoredRecord` has ZERO production callers.
*(Exit gate amended by impl correction 3 below: the constructor keeps exactly ONE caller — the
ungated-outer residual — until W4-left/W5 gate every cluster; the gate becomes "one caller, reachable
only for ungated outers, pinned".)*

### W4b impl correction 3 (shape 1): the clustered outer is BROKEN today, not name-model-working
**Ground truth (FDB-probed on master, `rfc173_w4b_clustered_outer_fdb_test.go`).** The shape-1 premise
— "clustered-outer correlated scalars work today via the name model; shape 1 flips them to ordinal" —
is FALSE. Probed matrix over `FROM customers c, extras e` variants:
- **(b)/(e) correlation to the RIGHTMOST leg** (comma or LEFT-join outer): **works**. Mechanism: the
  level-2 outer quantifier is named `sourceAlias(p.Input)` = the rightmost leaf, the executor binds the
  outer row under exactly that alias, and the inner's lazy `FieldValue(QOV(E), COL)` resolves through
  the binding (`values.go evaluateCorrelated`). This is the ONLY working clustered-outer class.
- **(a)/(g) comma cluster + NON-rightmost correlation**: **0AF00 — cannot plan** (with or without
  multi-leg projections). The inner is correlated to `C`, but the level-2 select's outer quantifier is
  `E` — `referenceIsCorrelatedTo` never matches, no NLJ implements, no winner.
- **(c) `JOIN..ON` cluster + non-rightmost correlation**: plans and returns **SILENT NULL** (want 100).
- **(d) LEFT-join outer + non-rightmost correlation**: plans and returns **SILENT NULL** (want 50).
So shape 1 is a **silent-NULL/unplannable BUGFIX + capability completion** — the exact pattern of
shape 3, one level up. Two design premises fall:
1. **The outer translates ENCLOSED today** (`translateProjectWithCorrelatedScalar` sets
   `t.inInnerCluster=true` around BOTH legs), so a csq outer cluster NEVER gates — the W3 ruling's
   "gate on `ordinalWedgeGateDecide(outer).Gated`" requires an **enclosure lift**: probe the gate with
   enclosure forced false (exactly `ordinalEligible`'s probe), and when Gated, translate the outer
   FRESH so it actually seeds ordinally.
2. **W1's "zero callers within W4b" is unreachable**: ungated outers (LEFT-box, unnest-outer,
   existential-carrying, dup-alias clusters) reach this site, and their RIGHTMOST-correlation class
   works today — deleting the name-model fallback would regress it. The anchored constructor keeps
   ONE caller (the ungated-outer residual) until W4-left/W5 gate every cluster; it dies in S4 as
   planned. S4 gains no new predecessor (W5 + W4-left already precede it).

**Amended shape-1 design (for ruling):**
- **(i) Enclosure lift, gated outers only.** Peel row-shape-preserving unaries (Filter without
  subqueries / Limit / Sort / Distinct) off `p.Input` to the cluster join J; probe
  `ordinalWedgeGateDecide(J)` enclosure-free. Gated → the outer leg translates FRESH (positional
  select); ungated → today's enclosed name-model translation stays.
- **(ii) Seed.** Outer leg = ONE full ordinal run over `QOV(sourceAlias(p.Input), concatType)`
  (`concatType` = `ordinalLegType(J)`), field names DOTTED `LEG.COL` walked per gathered leg — the
  level-2 output row stays name-addressable, so the shipped W4b level-3 projection mechanism (flat
  dotted `FieldValue` reads over the seed's named output) is unchanged. Inner leg unchanged
  (`ofOrdinal(QOV(inner),0)` named `INNER.SCALARCOL`, nullable). `AssertOrdinalJoinSeed` holds (two
  runs).
- **(iii) Correlation pull-up (Java `Value.pullUp` / `Quantifier.pullUpResultColumns` equivalent).**
  The inner plan's correlated leg refs `FieldValue(QOV(leg), COL)` rewrite to
  `ofOrdinalNumber(QOV(outerAlias, concatType), leafOffset + leafTyp.FieldIndex(COL))` through
  `gatedJoinLegTypes(J)` — the W3-blessed spine. Runtime resolves through the existing
  binding→OrdinalRow arm (`values.go` `evaluateCorrelated`); no executor change. The rewrite walks the
  single-source inner chain's value carriers (filter predicates, projected values, aggregate operands,
  sort keys) via `predicates.ReplaceValues`/`values.Replace`, building rewritten copies — an UNKNOWN
  inner node kind marks the refs unbakeable (decline, never a silent miss).
- **(iv) Decline policy (kills the silent-NULL class).** Non-rightmost leg refs that cannot bake
  (ungated outer, buried box-leg refs, unbakeable inner, inner-alias/leg-alias collision) →
  DECLINE nil (clean 0AF00), replacing today's silent NULLs; rightmost-only refs keep the name-model
  fallback ((b)/(e) unregressed). Every clustered-outer query is then CORRECT (ordinal) or LOUD
  (0AF00) — never silently NULL. **Scope (impl-review condition):** the LOUD guarantee covers
  PROVABLE refs — a non-exhaustive inner walk whose only non-rightmost refs hide in un-walked
  carriers falls back to today's behavior (the guard declines on definite refs only; unanalyzable
  shapes keep the pre-W4b status quo rather than newly rejecting working rightmost-corr queries).
- **(v) Bare projections over a cluster + csq** are not resolvable against the dotted seed output:
  rightmost-only correlation falls back to name-model (today's behavior); non-rightmost declines per
  (iv). Bare-unique widening is deferred (documented, not silent).

### W4b shape-2 impl notes (fresh-id decouple, landed)
Shape 2 landed per the W2 ruling: the ordinal seed's inner leg + inner quantifier + level-2
`sourceAliases` entry all carry a FRESH `UniqueCorrelationIdentifier` (`q$N`); the RC field NAME keeps
`<InnerAlias>.<scalarCol>` (the projection's read key, `replaceScalarSubqueryRef` unchanged). The
`innerContainsJoin` gate is deleted from BOTH dispatch paths (single-source + clustered) — the typed-QOV
collision it guarded is structurally impossible with distinct correlation keys. The clustered pull-up
walk gained a `Join.OnPredicate` carrier arm (JOIN-inners under clustered outers bake through the same
spine), and the inner-vs-cluster alias-shadowing decline generalizes to ALL inner-subtree source
aliases (a JOIN-inner's second table could shadow a cluster leg).
**Finding (executor):** the fresh-id quantifier exposed a routing gap in `spanAwareRow.GetByName`
(`rfc173_ordinal_join.go`): a flat DOTTED read ("O.AMOUNT") resolved ONLY through span-alias /
RecordName routing — with the inner span now `q$N`, the read missed even though the output type
literally carries the column (loud `OrdinalResolutionError`, caught by the shape-1 FDB matrix on the
first shape-2 run). Fixed by falling back to the row's OWN output naming after both routings miss
(ordered LAST — a leg alias always wins over a same-spelled literal; for seed-born rows both denote
the same value). Exit-gate status after shape 2: `NewScalarSubqueryAnchoredRecord` has exactly ONE
production call site (the translator's ungated-outer fallback), reachability pinned both directions
(`TestRFC173W4b_ClusteredDispatch_BothDirections`, `TestRFC173W4b_JoinInnerDispatch_Ordinal`).

**Graefe ruling on impl correction 3: DESIGN-ACK, all five amendments + exit-gate amendment, with
conditions.** (i) ACK — reuse the `ordinalEligible` enclosure-free probe verbatim; if Gated but the
fresh translation/seed fails, that case joins the (iv) DECLINE set (never an enclosed fallback into
the silent-NULL class). (ii) ACK — box-leg convention consistency outweighs a second convention.
(iii) ACK-with-conditions: (a) bake ALL legs' refs including the rightmost (no mixed-model lazy reads
against a concat OrdinalRow); (b) copies, never in-place mutation (the logical tree must survive a
decline-and-fallback re-translation); (c) the white-box pin must ENUMERATE the walked carrier kinds so
a new logical node fails the pin, not silently. (iv)/(v) ACK. Exit gate ACK with condition: the pin
asserts BOTH directions (constructor unreachable for gated outers AND reachable for the ungated
residual). Q-A: KEEP the 2-leg shape (the seed need not be canonical; exploration reaches the flat
form) — condition: PIN the SelectMergeRule interaction explicitly (if the gated outer leg merges into
the LEFT-outer select, the baked concat-QOV refs must compose to per-leg ordinals via
`translateValueCorrelations`, or the merge must decline; a wrong-ordinal read post-merge is exactly
the silent class this slice kills). Q-B: translation-time rewrite on copies is correct here because
the seed is born at the translator. Q-C: no §5 conflation hazard (ordinals authoritative, dotted
names disambiguate, both residual collisions decline-gated). **Impl detail adopted under the W2
principle (unique quantifier ids):** the level-2 outer quantifier of the ORDINAL path takes a FRESH
unique correlation id (not `sourceAlias`'s rightmost-leaf name) — the seed's concat QOV and the
baked pull-up refs key on it, which structurally rules out the alias-shadowing / DIVERGENT-baked-types
collision between the level-2 concat type and the cluster's own rightmost leg type.

## W5 — multi-source lateral UNNEST ordinalization (design for Graefe ACK; PROPOSED, pre-impl)

**Charter (S4-map residual item 2):** retire `buildUnnestResultValue`'s AnchoredJoin — the
multi-source / enclosed / under-existential lateral-unnest classes that W4c left name-model — and
with it kill `MergeSeedLegsOfValue` / `leftmostQOVOfValue` (the F4 ruling: these die in W5, not S4).
W4b is MERGED (PR #465): `NewScalarSubqueryAnchoredRecord` is down to the one pinned ungated-outer
residual caller. Canonical sequence: **W5 → W4-left+EXISTS+recursive-CTE → S4**.

### Go current state (precise map)
- `translateUnnestJoin` (`cascades_translator.go:946-1236`): sets `t.inInnerCluster=true` for the
  whole subtree (:958-960 — the outer cluster translates ENCLOSED, name-model); `outerAlias =
  sourceAlias(j.Left)` = the RIGHTMOST leg (:992); the **buried dotted read** at :1125-1134 —
  `arrayValue = FieldValue(QOV(outerCorr), "SEG0.FIELD")` when segment 0 is not the rightmost flow
  leg (the name-model linchpin W5 replaces); chained unnest rejected (:1101); W4c gate at :1219
  (`clusterArity(j.Left)==1 && !prevEnclosure && !t.unnestUnderExistential` → `unnestOrdinalSeed`,
  else `buildUnnestResultValue` at :1223). Always a 2-quantifier `JoinInner` select.
- `unnestOrdinalSeed` (`rfc173_w4c_unnest_seed.go:44-122`): the W4c single-source precedent — outer
  full ordinal run; WITH-ORDINALITY inner = 2-field `ofOrdinal(inner, 0/1)` (full-baked,
  AssertOrdinalJoinSeed); no-AT = direct-QOV element (mixed RC, raw-bound).
- `buildUnnestResultValue` (`cascades_translator.go:1277-1407`): the name-model builder W5 retires —
  anchored outer leg (dotted-only for multi-source via `legColumns(join)`), bare last-leg-wins twins,
  AS/AT shadowing, `rc.AnchoredJoin = true` (:1405).
- Poison arms keeping multi-source name-model: `ordinalWedgeGateDecide:56-61` (unnest join),
  `clusterArity:258-260` (unnest join = arityPoison → ANY FROM containing an unnest poisons the
  enclosing cluster), `gatherInnerClusterLegs:141-148` (stops at the unnest join),
  `ordinalEligible:152-166` (never eligible since it never gates).
- Dotted-prefix bipartition machinery (RFC-142) — the W5 kill set: `values.MergeSeedLegsOfValue`
  (`value_correlation.go:27-51`, sole production caller `quantifierMergeSeedLegDeps`,
  `rule_partition_select.go:796-808`, consumed at :705 in `computeTransitiveCorrelationOrder`);
  `leftmostQOVOfValue` (`value_correlation.go:56-70`). The `rangesOver` sibling-edge recovery
  (:721-727) is generic correlation plumbing and SURVIVES (it is what the ordinal model relies on).
- Name-model rebase machinery that dies with the dotted model: `rebaseUnnestOuterLegPredicate`
  (:2152-2185) + `unnestOuterLegAliases` (:799); `pushBuriedUnnestPredicateDown` (:1539) — the
  "buried" shape ceases to exist structurally if the enclosing cluster gathers the unnest.
- Executor: `RawLegs` (bare-QOV non-record element leg, raw-bound) and `OrdinalityLegs` (strictly
  positional `_0`/`_1`) are S4-PERMANENT per the W4c ruling — W5 reuses them as-is, zero new executor
  machinery expected. `deriveColumnsFromFlatMap`'s anchored arm (`cascades_generator.go:2957`) needs
  an ordinal-seed metadata twin for `SELECT *` over a multi-source unnest (currently UNPINNED — probe
  first, the W4b tradition).

### Java reference (the model to copy)
Java models `FROM A, B, A.arr AS x` as **ONE SelectExpression with N ForEach quantifiers** — never a
merged-row-under-the-rightmost-alias: the FROM traversal accumulates flat operators
(`QueryVisitor.visitTableSourceBase:376`), the unnest lowers via
`LogicalOperator.generateCorrelatedFieldAccess` (LogicalOperator.java:306-355) to an
`ExplodeExpression` whose collection is a `FieldValue` over `QOV(A's unique id)` — **the lateral
dependency is a genuine correlation** (`ExplodeExpression.computeCorrelatedToWithoutChildren` =
`collectionValue.getCorrelatedTo()`), visible to every Cascades rule; `Quantifier.forEach` mints a
fresh unique alias; output columns per the W4c spec (AT → `ofOrdinalNumber(flowed, 0/1)`; primitive →
the bare flowed QOV; struct → per-field ordinals). Bipartition validity is EMERGENT — Java has no
dotted-prefix classifiers because no source is ever buried. (Per the premise correction, Java has no
lateral unnest in inner-join re-enumeration — the multi-source rewrite is Go-only in reach, but the
MODEL to copy is exactly this.)

### The multi-source gap (why the W4c gate declines)
1. **Buried source-name erasure:** the array read is a name-model dotted key over the rightmost leg's
   QOV; a bare ordinal run over the flattened concat has nothing to resolve `A.` against — the W4b
   shape-1 hazard, solved there by dotted-named runs over a fresh concat QOV.
2. **Executor binding mismatch:** the FlatMap binds the merged outer row under the rightmost alias; a
   gated (positional) outer under a name-keyed dotted read is a mixed-model read; the reverse is a
   `BakedNameContextError`.
3. **Correlation invisibility:** `GetCorrelatedTo` on the Explode's collection reports only the
   rightmost leg; the genuine dependency on the buried source is recovered ONLY by the dotted-prefix
   classifiers — an ordinal seed under the name-model merge would emit refs those classifiers
   mis-handle (wrong bipartition → materialized Explode against an unbound row → silent zero rows).
4. **Anchored re-enumeration:** `PartitionSelectRule`'s anchored arm panics on a non-anchored ordinal
   leg (the `prevEnclosure` decline); the POSITIONAL arm (`positionalMergeCase`) already exists —
   whether it consumes an Explode-quantifier leg is a W5 impl question, not a from-scratch build.
5. **Existential rebase is name-model:** `translateUnnestExistsFilter` + `rebaseOuterLegValue`
   (panics on baked nodes) — the `unnestUnderExistential` decline.

### Proposed design (Java-grounded)
Stop building the unnest as a binary FlatMap-over-merged-outer: extend
`gatherInnerClusterLegs`/`translateGatheredInnerCluster` (`rfc173_ordinal_seed.go:137-322`) so a
lateral unnest contributes its Explode as an ORDINARY GATHERED QUANTIFIER — `FROM A, B, A.arr AS x` =
one select over `{QOV(A), QOV(B), ForEach(Explode)}`, the Explode's collection baked
`ofOrdinal(QOV(A, typA), FieldIndex(ARR))` — **a genuine correlation to A's own quantifier** (Java's
`generateCorrelatedFieldAccess` in ordinal form). Seed RV = flat ordinal leg runs (dotted `LEG.COL`
names per the W4b (ii) convention) + the W4c inner element/ordinal fields; the W4c seed becomes the
degenerate N=1 case. Bipartition validity becomes emergent (the `rangesOver` edge keeps A with its
Explode); the dotted classifiers go dead. Executor: inner leg rides the existing
`RawLegs`/`OrdinalityLegs`; the NLJ rule already pairs correlated Explodes.

### FORKS for Graefe ruling
- **Q1 — flat-at-translation (Option A, Java's shape) vs W4b-style binary concat-QOV seed (Option B,
  enclosure-lift + `clusterPullUp` reuse).** They converge post-SelectMergeRule; A does at translation
  what B defers to exploration, matches `generateSimpleSelect`, directly kills the buried-source
  concept; B is more incremental.
- **Q2 — interning classifier:** the mixed unnest RV (direct-QOV element field) FAILS `IsOrdinalJoinRV`
  (`values.go:469-487` FrontierPinned check) — the ordinalized unnest select drops out of alias-aware
  interning that AnchoredJoin gave it. Widen `IsOrdinalJoinRV` to admit bare-QOV fields (the
  `IsPositionalMergeRC` precedent), or accept identity dedup for unnest selects? Task-count pin
  required either way.
- **Q3 — does W5 own the under-existential class?** The S4 map assigns it to W5, but its blocker is
  the existential rebase the W4-left+EXISTS slice owns. Either W5 ordinalizes the existential-outer
  rebase for unnest, or the `unnestUnderExistential` decline survives W5 into the EXISTS slice
  (charter amendment).
- **Q4 — chained unnest** (`FROM t, t.a AS x, x.b AS y`): the flat model makes it natural (the second
  Explode correlates to the first's quantifier — Java supports the analog); lift the :1101 guard in
  W5 or keep it a separate extension?
- **Q5 — residual fallback:** do ungated outer clusters (LEFT-box / dup-alias / CTE-derived) keep a
  `buildUnnestResultValue` residual until W4-left (several such compositions WORK today name-model —
  decline-loud would regress them), or decline? (The W4b exit-gate amendment is the precedent for
  fail-open + pinned-residual.)

### Pins required (from the shape inventory)
Multi-source white-box seed pins (the missing analog of `rfc173_w4c_unnest_seed_test.go`);
byte-identical rows on the ENTIRE name-model corpus (`TestFDB_ArrayUnnestOrdinality` multi-source /
enclosed / existential families — R5-R31, P1a/P2a/P2b/P2c); the §6 mandatory execution pin (RFC-142
suite green under ordinal recovery); positional-authority pin (`DisablePositionalEmission` flip);
`MergeArmHits == 0` for ordinalized unnest shapes; task-count/STAR budget unchanged; a NEW
`SELECT *`-over-multi-source metadata pin. **PROBED (master):** `SELECT * FROM T1, U, T1."ARR1" AS
"VAL"` CANNOT PLAN today ("best expression is not a physical plan" — planner non-convergence on the
star-over-buried-unnest shape, while the same FROM with explicit projections works) — the star pin is
a FIX TARGET for W5, not a regression constraint.

### W5 design review: Graefe DESIGN-ACK (all five forks ruled; conditions folded)
- **Q1 — Option A (flat-at-translation) ACK.** Decisive: under Option B the Explode correlates to the
  WHOLE-CLUSTER concat QOV — strictly coarser than Java's per-source edge, over-constraining
  bipartition ({A, Explode} could never separate from B) and defeating the re-enumeration W5 exists to
  enable. A's per-source correlation makes the `rangesOver` recovery carry exactly Java's
  `Quantifier.getCorrelatedTo` edge — emergent validity. Conditions: (i) the `gatherInnerClusterLegs`
  unnest-stop becomes an Explode-leg contribution preserving FROM order; (ii) the P1/P2b
  alias-collision rejections survive verbatim; (iii) the Explode's collection bakes ONLY when its
  source is itself a gated ordinal leg — else decline (feeds Q5).
- **Q2 — widen `IsOrdinalJoinRV` NARROWLY, ACK.** Identity dedup is unacceptable (W5 puts unnest
  selects INTO re-enumeration — the 29915→60044 interning-blowout class). Admit a field that is a bare
  TYPED QuantifiedObjectValue (counts toward roots): a whole-leg reference is as position-determined
  as a FrontierPinned bake — no name resolution can hide in it; the CTE-rename decline rationale is
  untouched. Conditions: typed-QOV only; task-count/STAR pins before/after; a dedup-dimension pin (two
  alias-differing structurally-identical unnest selects intern to one).
- **Q3 — CHARTER AMENDMENT, ACK: the W4-left+EXISTS slice owns the under-existential class** (its
  blocker is `rebaseOuterLegValue`, owned there; EXISTS still precedes S4). **F4 RIDER (charter fix):**
  a multi-source under-existential unnest still emits dotted buried reads, so
  `MergeSeedLegsOfValue`/`leftmostQOVOfValue` CANNOT physically die in W5 — W5 makes them
  DEAD-FOR-GATED (pinned); physical deletion rides whichever slice kills the last dotted producer.
- **Q4 — chained unnest NAK for W5 scope:** gated on the engine-wide nested struct-field-access gap
  (the W4c cross-cutting deferral), not on the flat model. Keep the guard as a gather-time decline
  that doesn't foreclose the extension; file the slice after (or with) struct-field access.
- **Q5 — fail-open pinned residual, ACK** (the W4b exit-gate-amendment precedent; decline-loud would
  regress working compositions). `buildUnnestResultValue` keeps the ungated-outer + under-existential
  classes until W4-left/EXISTS. Exit gate pinned BOTH directions (ordinal shapes prove the seed path,
  white-box + MergeArmHits==0; residual shapes prove reachability + today's rows).
- **Hazards folded:** (1) the §5 dual-window oracle's producer-context rebind (`oracleNameDatum`) must
  extend to the gathered N-way Explode leg — MANDATORY pin (the exact latent class W4c caught);
  (2) `SELECT *` metadata probed (cannot plan on master — fix target); (3) Go binds the VISIBLE alias
  where Java mints fresh quantifier ids — acceptable ONLY because the alias-collision rejections
  exist; documented here as the standing justification.
- **Commit structure (dependency-free, each white-box + FDB pinned):** 1. gather extension + isolated
  multi-source (seed pin + R-family byte-identical rows); 2. enclosed-leg class + GROUP-BY
  re-enumeration via `positionalMergeCase`; 3. Q2 interning widening + STAR/task pins; 4. §5 oracle
  rebind + positional-authority pin + `SELECT *` metadata; 5. classifier dead-for-gated pins
  (physical delete deferred per the F4 rider). Exit gate both directions.

### W5 commit-2 design (investigation-grounded; for Graefe ruling)
**Probe-verified ground truth (all end-to-end, live FDB where applicable):**
- The case-A dotted-resolution mechanism over the flat bare-named output is LAZY dotted names +
  RUNTIME SPAN ROUTING: childless flat `FieldValue{"A.NAME"}` projections survive every rule
  untranslated (TranslationMap only rewrites correlation-bearing leaves), and resolve at
  `executeProjection` via `downstreamLegWindows` → `joinPlanSpans` → `ordinalJoinSpansOf(rv, legRVs)`
  (fused post-merge refs DESCEND the merge quantifier's child RV via `resolveSpanLeaf` to leaf legs)
  → `spanAwareRow.GetByName` alias→window→leg-local routing. NOT baked projections.
- The R18 divergence: `resolveSpanLeaf` descends only MULTI-accessor paths; the gathered MIXED
  (no-AT) element — a bare non-record QOV seed slot, translated by the merge into a SINGLE-accessor
  pinned ref over the merge quantifier — stops at the merge, yielding a partial-coverage span set →
  windows decline → the STRICT positional context → the loud name-miss. **The AS+AT (full-baked)
  ON-carrying gathered shape ALREADY WORKS end-to-end** (its element/ordinal refs fuse to
  multi-accessor paths that descend fully).
- **RETRACTION:** the commit-1 "spanning WHERE drops through the gathered re-enumeration" finding
  was a PHANTOM — non-discriminating seed data (`EL > WV` all-true) misread as a dropped predicate;
  with discriminating data the spanning conjunct classifies (spanning→upper), rewrites (bare QOVs
  ARE translated by TranslateLeafPredicates), and filters correctly through BOTH paths. The
  `predSpansUnnestAndLeg`/`forceUnnestResidual` fail-open guards a non-bug. The "P2a-vs-disjoint
  residual divergence" was equally phantom.
- Latent hazards: (1) gather-boundary alias case — gathered quantifiers are UPPER while
  `bakeGatedJoinPredicates` preserves the baked correlation's original case; a lowercase correlation
  mis-classifies the ON pred as deeply-correlated (unreachable via production SQL today — normalize
  + assert + pin); (2) `newOrdinalJoinBirth` derives spans with nil legRVs — the NLJ cursor (unlike
  the FlatMap's) never recovers them, so a translated NLJ top has WindowsOK=false and name-model
  reads of ITS OWN Datum on such shapes are an untested dimension.

**Proposed commit-2 scope (REVISED from the original "enclosed-leg + GROUP-BY re-enumeration"):**
1. **The span-derivation extension (the core; executor-only, ~40 lines):** when a SINGLE-accessor
   pinned ref's child is a merge quantifier (has a legRV) and the referenced slot is a bare
   NON-RECORD QOV, synthesize the 1-field element leg (alias = the slot QOV's correlation; legType =
   one field named by the RC field name — the existing `rfc173_ordinal_join.go:145-155` synthesis
   pattern). Probe-verified: with the windows, `GetByName("S.SID")` and bare `"EL"` both resolve.
   Windows are DECLARED SCAFFOLDING that dies in S4 — this extends existing scaffolding, adds no new
   eval arm.
2. **Lift the ON-carrying decline** (both unnest forms — AT already works, the mixed form works with
   #1); pin R18-class FDB (ON + dotted projections + element WHERE through the gathered path).
3. **Remove the phantom fail-open** (`predSpansUnnestAndLeg`, `forceUnnestResidual`) + pin the
   spanning matrix through the gathered path with discriminating data (done for the residual side).
4. **Alias-case normalization + assert at the gather boundary** (hazard 1) + a pin.
5. R16 shadowing / name-ambiguous dups STAY DECLINED in commit 2 (one cheap probe first: the visitor
   already qualifies shadowed bare projections to `V.V`, which #1's windows may route — if green,
   lift the shadow decline too; the bare-twin dup class genuinely needs ordinal projections and
   waits for the S4 compose direction).
6. The ENCLOSED-leg class (`FROM A, A.arr AS x, B` — prevEnclosure) moves to commit 3 (with the Q2
   interning widening); the original commit-2/3 split is re-drawn accordingly.

**Graefe ruling on the revised commit 2: DESIGN-ACK.** Q1 span-derivation ACK ("unnestMixedSeedSpans
lifted one level"; extends declared S4-scaffolding, no new eval arm, oracle untouched) with
conditions: (i) discriminate on the SLOT shape — a bare non-record QOV in the merge RC at the
accessor's ordinal, never the ref shape alone; (ii) the synthesized leg's alias = the slot QOV's
correlation (the AS alias), never the merge alias; (iii) the sole column named from the enclosing RC
field per the existing synthesis pattern, keeping the FrontierPinned/full-coverage gates; (iv) pin
the SHADOWING dimension through the gathered+merged path. Q2 ON-lift ACK conditioned on #1 landing
first with the R18-class FDB pin RED→GREEN (both forms; ON + dotted projections + element WHERE in
one query); the fail-open residual remains the fallback for underivable windows. Q3 remove the
phantom fail-open ACK, NO narrowed version (a guard for a retracted non-bug suppresses the
re-enumeration W5 exists for — principle 10); post-removal the spanning pin must assert the GATHERED
plan signature. Q4 normalize case AT THE GATHER (one case authority) + ASSERT at the bake (loud on a
case-insensitive leg-alias mismatch) + white-box pin (production SQL cannot reach it). Q5 revised
split ACK (enclosed-leg → commit 3 with the interning widening; R16/dup stay declined pending the
cheap shadow-qualification probe — lift shadow only if green; bare-twin waits for S4 compose;
exit-gate both-direction pins re-run per commit). Q6 the NLJ-birth nil-legRVs dimension is PIN-ONLY
in commit 2 and an S4-rider fix — it PROMOTES to commit-2 scope immediately if the pin exposes a
wrong ANSWER rather than a loud decline.

### W5 commit 3 (landed): interning widening + the ENCLOSED class

**3a — the Q2 interning widening** (per the Q5-revised split). `IsOrdinalJoinRV` admits a bare
TYPED `QuantifiedObjectValue` field toward its roots: a whole-leg reference is as
position-determined as a FrontierPinned bake (no name resolution can hide in it), so the gathered
unnest select — and, as the ruling's before/after condition anticipated, the post-translation MIXED
upper RVs — intern ALIAS-AWARE. Typed only: an untyped bare QOV carries no leg contract and keeps
declining (the CTE-rename/lazy rationale untouched). Measured: STAR sentinel re-baselined
51377→42788 (−17 % tasks, determinism 4×-stable), chain/dispatch/shadow-delta pins unchanged.
Typed/untyped classification pinned in the interning baseline test.

**3b — the ENCLOSED class** (`FROM A, A.arr AS x, B` — the unnest join buried as a LEG of the
enclosing inner cluster). Mechanism: **rotation to the root form**, not a second builder.
`gatherLegsWithBuriedUnnest` walks the inner spine (exactly one buried unnest; multi/existential/
non-inner declines), and `rotateEnclosedUnnest` rebuilds `Join(Join(plain legs, FROM order),
Unnest)` — inner-join-equivalent because the lateral dependency needs only the owner in scope. Two
scope decisions, both pinned white-box: (i) every collected ON conjunct rides the rebuilt ROOT's
OnPredicate — an ON may reference the ELEMENT alias, in scope only at the flat select the builder
folds the root ON into; the plain-leg chain stays ON-free (also keeping the gate probe on the pure
comma cluster the commit-1 corpus pinned); (ii) the P1/P2b collision minima widen to ALL plain legs
— after rotation the flat select binds legs AFTER the unnest in FROM order, which the original
before-the-unnest gauntlet scope never saw. Classification is minimal and fail-open: a decline
keeps the ORIGINAL tree, whose residual path still yields the faithful diagnostics. Seed-field
order places the element LAST (not at its FROM position) — observable only via
SELECT-*-over-multi-source, which cannot plan today (the commit-4 fix target).

Two filter-side consequences, both required for correctness: (a) `translateFilter`'s gathered merge
arm extends to the enclosed form (rewrite element refs + bake leg refs through the rotated plain
cluster's leg types — the root form's exact treatment, guarded by the gathered-select signature);
(b) `pushBuriedUnnestPredicateDown` STANDS DOWN when the enclosed gather fires — its restructured
tree (a Filter wrapping the unnest join) un-gathers the cluster, and a spanning conjunct (never
pushable) would land raw on the residual NLJ with the element unbound. That un-pushed-spanning
shape was a PRE-EXISTING SILENT 0-ROW class (`FROM A, A.arr AS x, B WHERE x > B.c` returned
nothing on master); the gathered path fixes it, pinned by a discriminating FDB e2e. With the
stand-down, ALL conjunct classes (element-only, cross-leg, spanning) reach the gathered select
uniformly, matching the root form. Declined shapes keep today's push semantics (fail-open).

### W5 commit 4 (landed): §5 oracle rebind for the gathered N-way + `SELECT *`

**The `SELECT *` fix target.** `SELECT * FROM A, B, A.arr AS x` PLANS since the gathered path
(it could not on master) but its column metadata leaked the partition sub-product's
planner-internal names (`[_0 _1 XID WV]`): `deriveColumnsFromJoin` merges the LEG subplans'
columns, and a FlatMap leg that is a positional-merge sub-product derives `_0`/`_1`. Fix: an
ordinal-top arm in `deriveColumnsFromJoin` — when the NLJ's RV is a raw baked-ordinal RC AND the
leg merge leaks a `_N` column, derive from the RV fields directly (each `f.Name` IS the
datum/positional key by construction; bare-name descriptor keying via `ordinalUnnestColumnDef`;
the MIXED element's type recovered from the Explode's collection element, which the partition
collapse erased). Scoped to the LEAK: every today-working join keeps byte-identical metadata.
Values then flow through the EXISTING §7 positional-aligned result-set read (the positional row
already carried `[SID WARR EL XID WV]` correctly). The enclosed form's FROM order is preserved
END-TO-END: the rotation now threads the unnest's FROM position (`unnestPos`) into the builder,
which inserts the element fields/quantifier at that position (the seed assert accepts runs in any
order; consumers resolve by span offset) — retiring the commit-3 element-last wart before it
became observable.

**The §5 oracle rebind (the MANDATORY hazard-1 pin).** A dedicated dual-mode FDB differential
(`TestFDB_RFC173W5_OracleDualWindow`; the gathered classes cannot ride the SQL-seeded dualwindow
corpus — no array literals) ran RED across the gathered family: projections read NIL and the
spanning WHERE dropped every row oracle-side — and the PLAIN 3-way merge-sub-product class
(`SELECT PA.AV, PB.BV FROM PA, PB, PC WHERE PA.AV > PB.BV`) was silently broken the SAME way,
undetected because the corpus never partitions a projection through a merge sub-product. Three
root causes, three fixes:
1. **The NLJ never recovers spans** (the Q6 nil-legRV dimension): `recoverOracleDatumSpans` — the
   NLJ twin of `newFlatMapCursor`'s legRV recovery — feeds ONLY `DatumSpans` (spliced); the
   adapter-side `Spans`/`WindowsOK` stay untouched (flipping them would re-route live legType
   lookups). `oracleNameDatum`'s qualified keys now gate on `DatumSpans` (identical for every
   FlatMap birth, where the two travel together).
2. **The NLJ's oracle Datum is mergeRows**, whose flat keys never carry a fused top's OUTPUT
   names: `oracleSwapFusedDatum` — the emission-time swap mirroring the mergeShapeDatum arm —
   reconstructs the output row per-field over the leg bindings (bare + ALIAS.COL from the
   recovered spans), at all four emission sites (hash/linear/LEFT-pad/FULL-drain; nil leg = NULL,
   ruling #3).
3. **The values raw-map arm** (NLJ predicates evaluate over the raw merged map): a PINNED fused
   ref through a merge quantifier roots at the bare `_i` slot key (the qualified form carries the
   merge alias's original case, which the upper-cased qualKey never matches). The arm is
   MERGE-SLOT-ONLY (`isOrdinalFieldName`): cut wider (any pinned rootKey), the dualwindow corpus
   caught it immediately — a pinned direct-leg ref reading the bare last-wins spill turned
   `a.k = b.k` into `k = k` (full cross product on the NULL-key entry). Lazy refs keep the
   qualified-only read; pinned+live keeps the loud BakedNameContextError. Pinned three-direction
   at the values level.
All seven differential queries (gathered five + plain-3-way two) now agree row-for-row; the full
1621-entry dualwindow corpus stays green.

### W5 commit 5 (landed): the F4-rider dead-for-gated classifier pins

`TestRFC173W5_ClassifierDeadForGated` pins all three directions: (i) GATED —
`MergeSeedLegsOfValue` on the gathered Explode's baked per-source collection is EMPTY (no dotted
prefix exists to recover), and the rule-level `quantifierMergeSeedLegDeps` wrapper agrees; (ii)
EMERGENT — the same quantifier's `GetCorrelatedTo` reports the OWNER (the genuine sibling edge
that replaces the dotted recovery, Java's `Quantifier.getCorrelatedTo`); (iii) RESIDUAL — the
name-model buried read (`FieldValue{Field:"A.ARR", Child:QOV(rightmost)}`) still classifies to
{A}, the reachability proof that keeps the classifier code alive for the decline and
under-existential classes until the W4-left+EXISTS slice retires the last dotted producer
(physical deletion deferred, per the F4 rider).

### W5 four-gate gauntlet (PR #466): five rounds, five real bugs, all red-first pinned

Round 1 (full-branch): Graefe ACK (rotation sound; stand-down correctly scoped; oracle separation
right; all ruled conditions verified); Torvalds ACK (4 cleanups); codex found P2 (the rotation's
collected element-referencing ON conjuncts baked UNREWRITTEN — silent drop/misfilter) and P3 (the
NLJ oracle span recovery skipped the splice for pristine births); the @claude gate found the
SELECT * metadata arm's NAME-keyed leak discriminator (misfires on a user column named `_0`) and
STRUCT elements mistyped BIGINT. Rounds 2-5 fixed every finding red-first: the ON rewrite
(rewriteUnnestPredicate on the rebuilt root's ON), the splice (FlatMap ordering) + its pin
(Torvalds proved the fix unpinned by reverting it — the pin now fails against the old guard), the
probe memoization (consume-once enclosedGatherCache, closing both reviews' purity flag), the
union-find fold, the STRUCTURAL leak discriminator (hasPositionalMergeLeg) — which codex then
proved STILL too broad on plain multi-way joins (positional-merge sub-products exist without
unnests; the NLJ-shaped fold keeps qualified names) → the arm now also requires the
Explode-bearing-FlatMap signature, with Graefe's adversarial both-direction probes confirming the
conjunction exact for constructible SQL; STRUCT element typing via the array column's proto
descriptor (source-level record typing deliberately deferred — it would flip RawLeg
classification mid-coexistence). Surfaced and booked, NOT swept: master's own equijoin multi-way
bare-name fold wart (worktree-verified pre-existing, independently reproduced by the Torvalds
gate), the arrayElementTypeNameFromDescs bare-name ambiguity nit, the enclosed+EXISTS residual
class pin, the 3-plain-leg mid-unnest rotation pin. Final tally on ca59ecbf1: Graefe ACK,
Torvalds ACK, codex clean, @claude ACK; 1M stress green; 1621-entry dualwindow corpus green.

## W4-left + EXISTS + recursive-CTE slice — design (for Graefe ruling)

**Charter.** Retire producer 3 (`buildJoinResultValue` → `NewAnchoredJoinRecord`) across its five
classes — LEFT/RIGHT-outer box, EXISTS-over-join, mixed nesting, dup-alias, recursive-CTE-enclosed
— plus the Q3-re-chartered under-existential unnest (blocker: `rebaseOuterLegRefsToMerged`). After
this slice the only surviving anchored producers are `NewReEnumerationAnchoredRecord` (dies in S4)
and the two PINNED residuals (W4b ungated-outer scalar; W5's ungated/dup declines) — so the F4
rider's PHYSICAL deletion of the dotted classifiers is chartered to ride this slice's exit if it
kills the last dotted producer, else S4.

**Java ground truth (4.12.11.0, agent-verified file:line).**
- LEFT OUTER: `QueryVisitor.wrapOperandsForOuterJoin` (fdb-relational :604-669) builds the result
  value AT TRANSLATION as a flat RCV of per-leg `FieldValue.ofOrdinalNumber` pull-ups where the
  null-supplying leg pulls through `QOV(alias, type.withNullability(true))`
  (`Quantifier.pullUpResultColumnsWithNullability`, core :790-799); `OuterJoinExpression` VERIFIES
  every null-side QOV is nullable (:111-132). Execution: `RewriteOuterJoinRule` (:79-147) → outer
  select over {preserved, `forEachWithNullOnEmpty`} reusing the SAME result value;
  `ImplementNestedLoopJoinRule` wraps null-on-empty legs in `RecordQueryDefaultOnEmptyPlan`.
- EXISTS: the existential leg NEVER appears in result columns (star expansion skips it); the
  select references it only via `ExistsValue.toQueryPredicate` → `ExistentialValuePredicate(QOV)`.
  Correlated inner refs to outer legs are PER-QUANTIFIER ordinal correlations — never a merged
  row. `ImplementNestedLoopJoinRule` yields FlatMap with `inheritOuterRecordProperties=true` for
  existential inners + `RecordQueryFirstOrDefaultPlan(NullValue)` (:313-316).
- Recursive CTE legs: `TempTableScanExpression.ofCorrelated` flows `QueriedValue(innerType)` —
  ordinal columns; every reference is a FRESH `Quantifier.forEach` over the shared Reference with
  outputs `rewireQov`'d by ordinal.
- Dup aliases: Java quantifiers ALWAYS mint `CorrelationIdentifier.uniqueId`; SQL-visible names
  are Expression qualifiers, never quantifier aliases.

**Go surface (agent-verified).** `buildJoinResultValue` lives at translateJoin's ungated arm
(:3760, declaration order) and translateJoinWithExists (:3905, UNCONDITIONAL, post-swap order).
Gate arms routing name-model: EXISTS-in-ON, pairwise dup-alias, mixed nesting (ordinalEligible),
LEFT/RIGHT (post-W3b "pending re-ruling"), enclosure, arity poison (exists/scalar-subquery
filters, recursive LogicalCTE legs). The dissolution-side ordinal seed EXISTS
(`ordinalSeedFromAnchoredLeft`, rfc173_w4_left_ordinal.go:49-117) for exactly-two single-source
legs. The existential implementation rebases outer-leg refs onto the MERGED row
(`rebaseOuterLegRefsToMerged` :832 → `QOV($m)."LEG.COL"`) with the FrontierPinned panic boundary
(:919-937). Recursive-CTE references ANCHOR via cteColumnsScope and already gate ordinal as
leaves; `LogicalCTE{Recursive:true}` in leg position is poison. `NewAnchoredJoinRecord` tolerates
dup legs by last-leg-wins.

**Proposed model (1:1 with Java where reachable).**
1. **LEFT/RIGHT (commit 1):** ordinalize AT TRANSLATION — translateJoin's LEFT arm builds the
   Java-shaped seed directly: per-leg baked runs with the null-supplying leg's QOV typed
   NULLABLE (`ordinalSeedFromAnchoredLeft`'s construction, generalized to gated-eligible legs and
   moved to the seed site); the gate's JoinLeft arm flips to Gated (the W3b re-ruling this
   section requests — the dissolved form is INNER+null-on-empty, which the ordinal machinery
   already implements; the "not opaque" finding becomes the reason it CAN gate, not the reason it
   cannot). RewriteOuterJoinRule keeps reusing the seed unchanged (Java-exact). The dissolution
   converter retires (dead-for-gated, deleted when the anchored input class dies).
2. **EXISTS-over-join (commit 2):** translateJoinWithExists builds the ordinal seed for the
   JOIN legs (the flat gathered seed when the outer is a gated cluster — the W5 builder;
   the binary ordinal seed otherwise) with existential quantifiers riding the SAME select
   (Java: existential legs excluded from the RV). The gate's EXISTS-in-ON arm flips for
   gated-eligible outers. `rebaseOuterLegRefsToMerged` goes DEAD-FOR-GATED: with per-leg baked
   refs the NLJ existential arm binds legs by correlation (twoLegBinder) — no merged row exists
   to rebase onto; the FrontierPinned panic boundary becomes the enforcement that the rebase
   never sees an ordinal seed. The under-existential unnest class (W5 Q3 re-charter) lifts with
   it: translateUnnestExistsFilter's `unnestUnderExistential` decline narrows to shapes whose
   existential SELECT is itself ungated.
3. **Recursive-CTE-enclosed (commit 3):** the recursive `LogicalCTE` leg-position poison narrows:
   a reference leg (TempTableScan-backed, cteColumnsScope-typed) is ordinal-eligible (Java:
   QueriedValue ordinal columns); only the DEFINITION expression stays opaque. Both-direction
   pins on the recursive corpus (the dualwindow recursive entries are the net).
4. **Dup-alias (commit 4):** mint FRESH correlation ids for later duplicate legs at the seed
   (Java-aligned: quantifier ids are never SQL names; the SQL alias stays the projection
   qualifier — the W5 gather already fresh-ids one shape, innerLegCorr). The gate's pairwise
   dup arm then admits the cluster. The name model's last-leg-wins is preserved OBSERVABLY by
   the visitor's qualification rules (last-binding-wins pins both directions).
5. **Mixed nesting** falls out: with 1-4 ordinalized, ordinalEligible's "leg contains a
   name-model join" arm has no producers left in the wedge except the pinned residuals —
   the arm stays as the residual guard (fail-open, Q5 discipline).

**Fork questions.**
- F1 (LEFT at-translation vs at-dissolution): Java builds ordinal at translation;
  the existing Go seed converts at dissolution. Proposal: at-translation (1:1), keeping the
  dissolution converter as the residual bridge until the anchored LEFT class is unreachable.
- F2 (EXISTS): retire-the-rebase-for-gated (proposed) vs extend-the-rebase-to-baked-refs
  (rejected: builds MORE name-model machinery). The existential select's RV = outer legs only
  (Java) — needs the executor's existential FlatMap to bind the inner existential leg for the
  PREDICATE while excluding it from the birth (the current birth already RawLeg-binds
  existential inners? — investigation item I1).
- F3 (dup-alias scope): fresh-ids for DUPLICATE legs only (proposed, minimal) vs all quantifiers
  (Java-exact, huge blast radius across every alias-keyed subsystem — rejected for the
  coexistence window; S4+ can revisit).
- F4 (classifier deletion): if commit 2 kills the last dotted producer
  (buildUnnestResultValue's under-existential arm), the W5 F4 rider's physical deletion of
  MergeSeedLegsOfValue/leftmostQOVOfValue rides THIS slice's exit gate.
- F5 (exit gate): both-direction pins per commit; the dualwindow corpus stays the net;
  MergeArmHits==0 extends to LEFT/EXISTS ordinalized shapes; 1M stress before/after;
  task budgets re-run (LEFT/EXISTS gating changes enumeration).

**Investigation items before commit 1:** I1 (executor existential-leg binding under an ordinal
birth), I2 (RIGHT normalization order at the seed — :3760 uses declaration order, :3905
post-swap; the ordinal seed must pick ONE authority), I3 (OuterJoinExpression-equivalent
nullability verify site in Go — the wrapper type does not exist; the verify lands on the seed
builder).

**Design ruling: DESIGN-ACK, all five forks, four conditions.** F1 at-translation ACK (the W3b
re-ruling GRANTED on the corrected premise: dissolution-into-INNER+null-on-empty is exactly why
LEFT can gate; RewriteOuterJoinRule must keep reusing the RV unchanged, Java-exact). F2 ACK
(RV = outer legs only; the merged-row rebase is a name-model artifact — extending it to baked
refs "would be building the wrong abstraction taller"; the FrontierPinned panic stays as frontier
police; the unnest lift rides the same gate predicate). F3 ACK as a COEXISTENCE measure only —
record in DIVERGENCES.md with the S4+ revisit; last-binding-wins needs both-direction pins
including SELECT * layout. F4/F5 ACK (producer audit at exit; task budgets re-run — gating
LEFT/EXISTS changes memo enumeration). Commit ordering ACK. Conditions:
(1) I2 is RULED, not investigated: Java assembles the RV in SOURCE order (left-then-right
regardless of join type; only preserved/null-supplying ROLES swap) — declaration order (:3760)
is the sole authority, and :3905's post-swap RV is a LATENT RIGHT-JOIN+EXISTS SELECT * column-
order divergence TODAY (fix in commit 2, pin both directions).
(2) I3 sharpened: nullability lives on the null-supplying QOV's RECORD TYPE
(type.withNullability(true)); the Verify keys on qov.getResultType().isNullable() — the
generalized seed types the QOV record-level nullable (the existing per-column wrap is not
Java's shape), and the verify lands on the seed builder.
(3) I1 (executor birth excluding the existential leg while the predicate still binds it) must
close BEFORE commit 2 merges — the one place correctness can silently invert (a 0-row class).
(4) F5 pins MUST include NOT EXISTS and NON-CORRELATED EXISTS shapes (the codebase's known
blind axis).

### W4-left commits 1-2 (landed) + I1 closure

**Commit 1** (single-source LEFT/RIGHT gate + at-translation seed) and **commit 2**
(EXISTS-over-join) are landed; see the commit messages for the mechanics. Two pre-existing bugs
fixed along the way, both worktree-verified master-identical and red-first pinned: the I2 latent
RIGHT+EXISTS SELECT * post-swap column order, and the OUTER-join flatten misclassifying WHERE
conjuncts as JOIN predicates (a preserved-side WHERE null-padded instead of filtering — silent
wrong rows). The W4b clustered-outer scalar decline class now ANSWERS (the pin flip its own
W4b-era text pre-chartered).

**I1 is closed by construction (condition 3):** the existential leg never reaches an ordinal
birth — the 2+1 implementation's step-1 NLJ carries only the seed's two ForEach legs (ordinal
birth as any gated join), and the existential level is a separate identity FlatMap
(RV = bare QOV over the merged row → birth disabled) with the FOD inner bound under the
existential correlation for its predicates. The condition's silent-invert hazard (a 0-row class)
is pinned e2e by the condition-4 matrix (correlated/NOT/non-correlated EXISTS over gated joins,
all row-verified). Coexistence note, booked for S4: the identity FlatMap passes the DATUM through
but drops the POSITIONAL row — sound today (downstream reads fall back to the merged Datum's
bare+qualified keys; names unique for the gated classes), but S4's positional-only world needs
the pass-through.

### W4-left commits 3-5 (landed) + the exit-gate producer audit

**Commit 3** — recursive-CTE truth pins (reference joins gate ordinal since the fulcrum; stale
header comments corrected; the definition-node poison is production-unreachable, probed).
**Commit 4** — column-aware duplicate-FROM-alias rejection with Java's 42702 (the F3 fresh-id
premise REVISED against LIVE Java conformance runs — per-attribute ambiguity, exact message
text byte-equal in the harness; two marked divergence corners + one parity entry; details in
DIVERGENCES.md). **Commit 5** — the exit gates.

**Producer audit (F4/F5 condition; post-commits-1-4 surviving anchored producers):**
1. `translateJoin`'s ungated arm — the JOINED-PRESERVED LEFT class (clustered legs; the W4
   dissolution ruling's scope) and ENCLOSED legs of existential/unnest flattens. PINNED residual.
2. `translateJoinWithExists` (the INNER flatten) — stays ANCHORED. The F2 ordinal seed for the
   flatten was cut and REVERTED under the dualwindow corpus (corr_exists_join_outer): the 2+1
   existential select also implements through data-access/correlated-FlatMap paths whose
   bindings are NAME maps — the seed's baked refs hit the loud BakedNameContextError on the
   LIVE side. Ordinalizing the flatten needs those paths' positional binders (booked). The
   gated existential classes that DO run ordinal arrive via the generic filter arm (gated
   LEFT/RIGHT boxes and gated clusters under buildExistentialSelect) with the implementation's
   ordinal rebase — landed in commit 2 and corpus-green.
3. `buildUnnestResultValue` — the W5 residual (unnest declines + under-existential). The DOTTED
   producer is therefore STILL LIVE → the W5 F4 rider resolves to: the dotted classifiers'
   physical deletion rides S4 (this slice did not kill the last producer).
4. `NewScalarSubqueryAnchoredRecord` — the W4b pinned residual (ungated-outer rightmost-corr).
5. `NewReEnumerationAnchoredRecord` — GROUP-BY over any anchored parent; dies after 1-4 (S4).

**Exit gates:** both-direction pins re-ran per commit (each commit's suites + the FDB matrices);
the 1621→1625-entry dualwindow corpus green (and it CAUGHT two over-reaches during the slice —
the flatten seed twice — exactly its job); task budgets (chain 11122/45306, STAR 42788) held
unchanged through every commit; 1M stress green on the slice head; the live Java conformance
harness green including the new dup-alias entries (byte-equal error text).

### W4-left gauntlet round 2 — the impl NAK cluster (three fix commits)

The impl reviews converged on the commit-4 dup-alias machinery and the outer-box gate. What
REPRODUCED and what didn't, verdict by verdict:

- **Dup-alias (both reviewers, four real defects, one commit):** later-pair three-way
  duplicates planned SILENTLY (first-source-only tracking — wrong rows); derived/CTE legs
  rejected with the garbage user-visible column `?`; an undefined table under a dup alias was
  masked 42F01→42702; the both-underivable corner had no defined message. The claim that
  derived/CTE dups "plan silently" did NOT reproduce (they rejected — with the garbage
  message). Fix: per-leg column derivation for EVERY leg kind (derived/CTE bodies via
  `fromLegColumnsUpper` + a threaded CTE registry), all-priors tracking, undefined-table pairs
  skipped. Two new PARITY corpus entries verified byte-equal against live Java; the CTE star
  corner extends the marked over-rejection divergence.
- **Mixed outer nesting (the NAK driver's strongest catch):** the demanded runtime pins
  immediately caught TWO PRE-EXISTING master bugs the plan-only probes could not see:
  unmatched `d LEFT JOIN e ON … JOIN c ON …` rows returned d.id AS e.id (the parent merge
  FABRICATED `E.*` keys from the box row's bare last-leg-wins keys), and the RIGHT variant
  nondeterministically panicked in the RFC-077 anchored re-enumeration. Fixes: merged rows
  (any dotted key) never seed `ALIAS.COL` fabrication; the re-enumeration DECLINES the
  unresolvable bipartition; the outer gate arms now respect enclosure (LEFT/RIGHT box legs of
  a name-model parent stay name-model — a FULL box leg is ordinal-eligible, so its parent
  gates and the composition is ordinal-over-ordinal, already pinned).
- **`ordinalSeedFromAnchoredLeft` deleted** — but NOT on the reviewer's "production-dead"
  premise, which the enclosure guard falsified: post-guard, enclosed dissolved boxes DO reach
  the converter (tripwire-verified), where it would re-create exactly the
  ordinal-under-name-model mix the guard prevents. Deleted as architecturally wrong (plus the
  I3 per-column-nullability contradiction), with the executor fix carrying the shape.
- **`translateJoinWithExists` dead OUTER arms** — collapsed into one INNER-only contract
  decline, pinned per kind; the misleading pin-f comment trimmed.

Method note for the record: first-round worktree baseline runs were VACUOUS (the copied pin
file was absent from the worktrees' explicit BUILD srcs — bare bazel PASS with "no tests to
run"), which briefly inverted the regression attribution; re-running with gazelle in each
worktree established the true baseline (master fails both mixed-nesting bugs).

## Post-W4-left S4 blocking inventory + the sequencing ruling (Graefe)

W4-left merged (PR #467, four-gate ACK). The compiled S4 blocking inventory, code-verified,
supersedes the "W4-left → S4" roadmap step — the W4-left producer audit's surviving anchored
producers plus the gauntlet-round-2 enclosure guard mean S4 sits behind prerequisite slices:

**Live anchored producers.** (A1) `translateJoin`'s ungated arm behind the gate's poison
ladder — EXISTS-in-ON (:69-74, item 2), dup leg aliases (:82-89, item 1), mixed nesting /
leg-ineligible LEFT/RIGHT (:90-100 + the :102-113 enclosure guard + :138-141 clustered-leg,
item 3), and the subquery-RIDER poison (`clusterArity` :324-332 — a cluster whose leg
filter/project carries exists/scalar subqueries). (A2) `translateJoinWithExists` — the INNER
2+1 flatten, anchored per the F2 scope note (seed twice corpus-reverted; the
data-access/correlated-FlatMap existential paths bind legs by NAME) — item 2. (A3)
`buildUnnestResultValue` residuals — under-existential unnests (the W5 F4 rider's re-charter
was NOT covered by the merged slice — booking gap, now closed into item 2) + W5's fail-open
declines (bare-twin duplicate cross-leg names, box-leg owners, multi-segment paths,
CTE/derived rotation owners, chained unnests). (A4) `NewScalarSubqueryAnchoredRecord` — one
production caller (the ungated-outer rightmost-correlation fallback); dies when items 1+2+3
JOINTLY zero the poison classes (not item 3 alone — the fallback arm also catches item-1/2
poison). (A5) `NewReEnumerationAnchoredRecord` — strictly downstream, dies mechanically (F1).

**The sequencing ruling (three questions):**
- **Bare-twin circularity → fold into the S4 atomic commit.** W5's duplicate-cross-leg-column
  decline waits on S4's ordinal-projection compose direction while S4 waited on the unnest
  residuals; the cycle cuts at S4 (the decline is fail-open name-model, costs nothing to
  ride; pulling positional duplicate-name coexistence forward would fork the coexistence
  regime and churn goldens twice). Condition: the S4 e2e matrix row-verifies the bare-twin
  unnest class, and the differential covers it name-model-side until then.
- **Item-2 charter absorption.** QP-REF-BIND item 2 formally absorbs the under-existential
  unnest class and the EXISTS-rider poison (same root cause: the existential machinery binds
  legs by name). The SCALAR-rider absorbs CONDITIONALLY — if the same positional binders
  unlock it, same slice, one review; if it needs W4b-seed rework it books as an immediate
  item-2 follow-on. Condition: each absorbed class gets its own gate-reason string and
  dualwindow pins.
- **Sequence: (riders ∥ item 2 ∥ item 1) → item 3 → unnest-residual slice → S4.**
  2-before-3 is load-bearing (the enclosure guard names existential/unnest parents; retiring
  it before positional binders exist re-opens the wrong-rows class). The riders — bound
  `positionalTypeCache` (a live leak on master today) and the recursive-CTE ordinal
  leg-normalization read — are standalone and start immediately.

**Standing S4 conditions:** the kill list is amended above (OrdinalFieldName struck —
load-bearing ordinal infrastructure; `select.go:274` survives for the CTE-rename slice); the
S4 exit gate proves zero anchored producers EMPIRICALLY (caller-free constructors, exhausted
decline reasons), never by inventory argument.

## ROADMAP STATE (authoritative; update this block as slices land)

**Done:** Slice 1 (non-join frontier + §5 oracle) · Slice 2 (wedge gate, 2-way seeds) ·
Slice 3 (fulcrum, N-way clusters) · W4b (clustered-outer correlated scalars, PR #465) ·
W5 (multi-source unnest gather, PR #466) · W4-left (LEFT/RIGHT + EXISTS + recursive-CTE,
PR #467, four-gate ACK) · S4-riders (positionalTypeCache bound + rcte remap provenance,
PR #468, four-gate ACK).

**← WE ARE HERE:** QP-REF-BIND item 2 (existential-flatten ordinalization) — design ruling
BANKED (ACK with amendments A–C + findings 4–5, below); commit 1 (the no-op existential
residual) IMPLEMENTED on feat/rfc173-qprefbind-item2 (full record below — root cause, audit,
and the post-audit correction: decline was insufficient, the fix is the Java-shaped
correlated step-1). NEXT STEP: land the derived-alias EXISTS-correlation fix (the F-class
42703 discovered by commit 1's matrix, below), then commit 2 (the executor binder). Item 1
(per-reference dup-alias binding) is parallelizable with item 2.

**Remaining, in ruling order:** (item 2 ∥ item 1) → item 3 (mixed-nesting LEFT widening; MUST
follow item 2) → unnest-residual completion slice → S4 atomic demolition (kill list above;
bare-twin decline-lift folded in; empirical zero-producers exit gate) → post-S4 separate
slices (CTE-rename select.go:274 widening; lazy name-identity deletion / full FieldValue
baking) → Slice 5 closure invariant → Slice 6 extensions → un-freeze RFC-164 (the /goal).

**Known follow-ons booked:** correlated-scalar-on-a-leg (item-2 split-hatch, below) ·
quoted-dotted verbatim-Field ambiguity (dies in S4, documented at recursiveRemapValues) ·
W5/W4-left S4 riders recorded in the kill list · the alias-unchecked frontier fallback
(values.go evaluateCorrelated OrdinalRow/Positional arms — "no binding ⇒ frontier-self"
never checks the QOV's correlation against the frontier quantifier; commit-1's audit flags
it as a CORRECT-or-LOUD violation; item-2 commits 2–4 replace these binding paths, the
follow-up verifies the class dies loudly) · EXPLAIN rendering divergence (ad-hoc Sprintf vs
Java's typed ExplainTokens; DIVERGENCES.md entry; post-RFC-173) · derived-alias EXISTS
correlation (F-class 42703, NEXT — see the commit-1 record below).

## QP-REF-BIND item 2 — design substrate (banked; for the Graefe slice ruling)

Research method: code trace + two live worktree experiments (E1 reproduced the twice-reverted
flatten-seed failure exactly; E2 validated a minimal executor binder end-to-end). The S4-riders
slice merged first (PR #468: bounded positionalTypeCache; the recursive-CTE leg remap classifies
name PROVENANCE — verbatim iff unaliased plain FieldValue — after three review rounds each
caught a string-shape hole; full four-gate ACK).

**Failing-path anatomy.** The 2+1 flatten (`translateJoinWithExists`,
cascades_translator.go:3829-3942) seeds the ANCHORED RV unconditionally (:3926) and never
consults the wedge gate. The only implementer is `implementJoinWithExistential`
(rule_implement_nested_loop_join.go:1389-1639): step-1 NLJ carries the seed RV; the existential
FlatMap's RV is the bare identity `QOV(mergedOuterCorr)` for WHERE-EXISTS (:1591). The executor
probes ordinal birth SOLELY from the FlatMap's own RV (flat_map_cursor.go:71) — identity RV ⇒
no birth ⇒ the outer row binds as the name-keyed Datum map (:234-243), and any baked ofOrdinal
ref over the outer alias dies at the BakedNameContextError arms (values.go:742 via
RowEvalContext, :817 via bare CorrelationBinder). E1: with the flatten seeded ordinally, the
thrower is the ordinal existential path's OWN plan (the NLJ births positionally; the FlatMap
above it drops to the name map) — a precision correction to the F2 scope note's "other
data-access paths" framing.

**LIVE CORRECTION to the W4-left record:** the ordinal existential rebase
(rfc173_w4left_existential.go) is currently DEAD on live SQL. The generic filter arm forces
enclosure under EXISTS (cascades_translator.go:2109-2119), and the gate's enclosure arms then
poison every join class beneath (:102-113 outer boxes, :143-147 inner clusters) — so every RV
reaching `implementJoinWithExistential` is anchored, `ordinalSeedLegWindowsOf` yields nil, and
the "gated existential classes run ordinal today" claim is false at HEAD. The machinery itself
is sound (E1: windows resolve and the rebase bakes correctly the moment a seed arrives); the
executor binding is what reverted the seed.

**Java model (agent-verified):** no 2+1 flatten exists — Java bipartitions N≥3 selects and
existentials ride partitions (ImplementNestedLoopJoinRule.java:96-98 matches exactly two
quantifiers). Existential leg = FirstOrDefault(inner, NULL) under the existential's own alias;
`ExistentialValuePredicate.toResidualPredicate()` → `QOV(alias) NOT_NULL`; existentials
contribute NO columns. Runtime binding is always alias-keyed with typed values and ordinal
field access (RecordQueryFlatMapPlan.java:122-149, FieldValue.java:163-175) — no name-map row
exists anywhere. The item-2 target in Go's two-level shape: the merged positional row is the
binding the inner leg sees.

**The E2-validated minimal binder:** in newFlatMapCursor, when birth is disabled, probe the
INNER plan for FrontierPinned refs over outerAlias (reusing widenLegTypesFromPlan,
rfc173_ordinal_join.go:857-927); if found, bind the outer via adaptLegPositional instead of the
Datum map (~20 lines). With the E1 seed this returned CORRECT rows on corr_exists_join_outer
end-to-end. A real slice adds: a gate arm for the flatten (dup-alias/eligibility); Positional
PASS-THROUGH on the identity FlatMap's output (the I1 booking — today the positional row dies
at the FlatMap boundary); the N-leg generalization (birthLegBinder is already map-shaped).

**Absorbed classes (ruling's absorption, resolved):** under-existential unnest
(translateUnnestExistsFilter, :2167-2271 — needs the W4c seed to stop declining under the flag
plus ordinal replacements for the two rebaseUnnestOuterLegPredicate sites) and the EXISTS-rider
clusterArity poison (:324-332) share the root cause — absorbed. The SCALAR-rider split-hatch
resolves: UNCORRELATED riders absorb (pre-evaluated scalar bindings are shape-agnostic);
CORRELATED-scalar-on-a-leg needs W4b-seed rework (a level-1 clusterPullUp variant) — the
"immediate follow-on" case.

**⚠ DISCOVERED BUG (pre-existing on master, unpatched — the item-2 slice's first red-first
obligation):** `SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id WHERE EXISTS
(SELECT 1 FROM badge b WHERE b.emp_id = e.id)` returns ALL depts, and the NOT EXISTS twin
returns the IDENTICAL rows — the existential residual is a NO-OP on the LEFT-join +
correlated-EXISTS-into-null-supplying-leg class when no other WHERE conjunct exists. Two
aggravators: the EXPLAIN'd winner differs from the executed winner, and the W4-left pin matrix
missed it because its LEFT variant carries a masking `d.id = 3` conjunct
(rfc173_w4left_exists_fdb_test.go:130-131). Root-cause with/before the slice; pin without
masking conjuncts + an EXPLAIN-vs-executed coherence check.

**Proposed slice plan (for ruling):** (1) root-cause + fix the no-op-residual bug, red-first
unmasked matrix; (2) the executor binder (E2) + identity-FlatMap positional pass-through;
(3) flatten gate arm + ordinal seed (rebase machinery already landed); (4) the enclosure lift
at :2109-2119 for gate-eligible inputs — making the W4-left rebase machinery live (closes the
correction above), per-class gate reasons + dualwindow pins; (5) absorbed classes
(under-existential unnest, EXISTS-rider, uncorrelated scalar riders). Exit gates: the
dualwindow two-phase corpus, the unmasked EXISTS matrix, MergeArmHits discipline, budgets,
1M stress.

## QP-REF-BIND item 2 — design ruling + commit-1 record

**Design ruling (banked): ACK with amendments.** (A) the commit-2 probe + identity-RV
pass-through go on the S4 kill list at booking time; the empirical zero-producers gate covers
them. (B) probe-positive + failed positional adaptation errors LOUDLY (adaptLegPositional's
zero-match tripwire), never a Datum fallback; widenLegTypesFromPlan's width-divergence panic
stays the single derivation path. (C) the bipartition question resolves no later than the
unnest-residual slice. (4) commit 4's enclosure lift routes gate-eligibility through
ordinalWedgeGateDecide/clusterArity — one authority, no parallel re-derivation. (5) commit 5
lands as 5a/5b/5c (one commit per absorbed class, per-class gate reasons + dualwindow pins).
Commit-1-first confirmed load-bearing: the dualwindow baseline for the LEFT+EXISTS class is
WRONG until the no-op residual is fixed.

**Commit 1 root cause (audited; the audit verified every layer at file:line).** The query
`dept d LEFT JOIN emp e ON e.dept_id = d.id WHERE [NOT] EXISTS(badge b: b.emp_id = e.id)`
returned ALL depts for BOTH polarities. Four layers:
1. **Translation** (correct): LEFT+EXISTS declines the 2+1 flatten by contract and routes to
   buildExistentialSelect — Select{ForEach(box), exist}; the box quantifier is named by
   sourceAlias(join) = the RIGHTMOST leg.
2. **Rewriting** (correct): RewriteOuterJoinRule dissolves the box (INNER + null-on-empty E
   over a subsel carrying the ON pred, correlated to D); SelectMergeRule merges the dissolved
   select into the existential parent → {D, E-noe, exist}. Java merges the analog (its
   javadoc condition (2) is STALE — the code rebases correlated siblings); no merge guard.
3. **Planning** (the defect): implementJoinWithExistential built a MATERIALIZED INNER step-1
   NLJ — never checking IsNullOnEmpty() (null-extension dropped; joinType read from the
   dissolved select = INNER) and never checking leg-to-leg correlation (the ON pred buried in
   the E-leg subsel embedded standalone). The ChildrenAsSet swapped firing
   (implementation_rule.go) yielded BOTH orders; cost picked E-outer. EXPLAIN = executed
   (deterministic); the substrate's EXPLAIN-vs-executed aggravator hypothesis is corrected.
4. **Execution** (the enabler): the orphaned QOV(D).ID inside the EMP leg resolved via the
   ALIAS-UNCHECKED frontier fallback (values.go evaluateCorrelated: "no binding ⇒ the
   frontier quantifier itself", OrdinalRow + Positional arms — neither checks the QOV's
   correlation) — D.ID read slot ID off the EMP row (name collision is load-bearing) →
   E.DEPT_ID = D.ID degenerated to emp.dept_id = emp.id → cross product → the residual then
   filtered per-row CORRECTLY → alice's pairs pass EXISTS, bob's pass NOT EXISTS → identical
   3-dept sets. Booked follow-up: make that fallback loud (dies with item-2 commits 2–4).

**Post-audit correction (implemented fix ≠ audited fix).** The audit ruled decline-only
(kill the merged member's implementation; the unmerged member carries). Empirically FALSE for
`SELECT *` roots: REWRITING promotes winners per group (AdvancePlannerStage: PLANNING's
members = REWRITING's finals), the merged select won the group, and with the decline the root
had NO winner → 0AF00 "could not plan" (W4-left pin f). The star probes proved named-column
variants plan (a Project group above carries the alternative) while bare-select roots die —
the "REWRITING pruning destroys PLANNING alternatives" trap. The implemented fix is the
Java-shaped lowering the audit called the eventual destination, pulled forward:
- buildCorrelatedFlatMapPlan factored out of yieldGeneralFlatMap (the per-quantifier-property
  lowering: correlated inner re-execution, DefaultOnEmpty for null-on-empty — Java's
  planPartitionToPhysical; the RFC-153 buried-rebase + fail-closed verifier ride along).
- implementJoinWithExistential's step-1 now keys on the correlation topology: independent
  legs keep the materialized NLJ (the INNER flatten path, unchanged); a null-on-empty or
  sibling-correlated leg orients INNER and takes the correlated FlatMap step-1. Declines
  remain only for genuinely unimplementable shapes: mutually-correlated legs, a null-extended
  leg forced OUTER, and EXPLODE legs (the unnest+EXISTS composition owns its dedicated nested
  lowering via translateUnnestExistsFilter; the merged variant's qualified-key addressing
  cannot reach a bare-keyed exploded element — declining restores the pre-fix winner).
- The 1+1 path (implementExistentialSelect) gained the missing BURIED-LEG rebase for its
  below-FOD predicates: a WHERE-EXISTS over a join box binds the box under ONE alias
  (rightmost-leg sourceAlias) while the EXISTS may correlate to ANY buried leg; the rebase
  authority is the outer plan's anchored result value's dotted prefixes
  (mergedOuterLegAliases — the merged-row key inventory), binding alias excluded (a
  single-table outer carries only bare keys). This fixed the B/C/D matrix classes
  (correlation into the preserved leg; mirrored declaration order; RIGHT twins), which were
  differently-wrong on master (cross-product rows) — wrapper-quantifier walks are NOT a
  buried-alias authority (a materialized-NLJ wrapper carries unnamed quantifiers).

**Commit-1 pins:** rfc173_item2_noop_residual_fdb_test.go — the unmasked matrix (A: the bug
class + the historical masked pin; B: correlation into the preserved leg; C: mirrored
declaration order; D: RIGHT twins; E: INNER regression guard + plan-shape assert; F:
derived-table leg with table-alias correlation; G: plain LEFT null-extension guard; H:
EXPLAIN determinism ×10 both polarities). The ID column-name collision across all three
tables is deliberate (it is what made the orphan resolve silently).

**Discovered during commit 1 (fixed on this branch, own commit):** correlated EXISTS
referencing a DERIVED-TABLE alias was rejected at translation — join form: 42703 `column
reference with qualifier "E" cannot be resolved`; single-source form: 42703 `no FROM source
aliased as E`. Two gaps, both fixed: buildOuterScopeSources never registered derived sources
(now via buildDerivedTableSource, the SELECT scope's own authority, for the primary source
and join legs), and the no-joins derived table dropped its alias from the logical tree
(op = innerOp — sourceAlias walked to the BASE table and the existential FlatMap bound the
outer under the wrong name; now wrapped in the same LogicalCTE(alias) the joins case uses).
Pinned by derived_exists_scope_fdb_test.go (single/join forms both polarities, either join
side, renamed projection body, guards).

**Commit-1 implementation reviews (banked).** Torvalds: ACK, four findings — all landed
(select-level-OUTER decline in the correlated arm; empty-alias backfill/decline; the
outerOnlyPreds asymmetry documented at the site — those preds resolve through the outer
row's qualified Datum keys, the masked-conjunct pin exercises it; the bug class's plan-shape
pin added). Graefe: ACK with conditions — the deviation from the audited decline-only fix is
RULED CORRECT (Java's stage promotion has the same one-winner property; Java implements the
shape rather than declining — the fix converges to Java). Conditions, all landed:
1. **DefaultOnEmpty/predicate placement (Java divergence, pre-existing in
   yieldGeneralFlatMap):** select-level WHERE-class predicates now filter ABOVE the
   null-extension (planPartitionToPhysical's order: wrap first, predicates above); the
   strict-single (scalar subquery) wrap keeps its predicates below (they are the subquery's
   own correlation). This surfaced a second latent hole: the WHERE conjunct arrives REBASED
   through the box's anchored record constructor (SelectMergeRule's multi-quantifier-child
   translation — the box quantifier is named by the rightmost leg), whose leg correlations
   the anchored RC HIDES — the pred split classified it outer-only and stranded it on the
   wrong leg. The split is now MERGE-SEED-AWARE (predicates.AddMergeSeedAliases — the same
   authority legReferencesAny uses). Matrix class I pins the conjunct classes
   (`WHERE e.fname='alice' AND [NOT] EXISTS`, the IS-NULL anti-join + NOT EXISTS): I2/I3
   returned cross-product rows on master.
2. **Projected-EXISTS × correlated step-1:** defensively declined (the fold is INNER-only
   upstream; the outer-join variant rejects before planning — matrix class J pins the clean
   rejection).
3. **Fail-closed rebase authority:** the 1+1 buried-leg rebase resolves the outer's result
   value through single-child wrappers (planResultValue); with NO authority and a below-FOD
   predicate referencing anything beyond the binding alias + the existential's own legs, the
   yield DECLINES (never the correlation-unchecked fallback).
Non-blocking Graefe notes booked: the NLJ arm's step-2 wrapper still ranges over leftExpr
only (pre-existing asymmetry; clean up with commits 2–4); the Explode decline re-creates the
decline+one-winner pattern for a class whose dedicated lowering owns it — P2c pins it, watch
it at the unnest-residual slice.

**Delta reviews (round 2, banked).** Torvalds ACK (4 findings) + Graefe ACK-with-condition +
codex P1 — all landed:
- **The rebuild-path alias loss (Torvalds finding 4 + codex P1):** a THIRD bare-innerOp
  derived arm lived in the plain builder (buildLogicalPlanForSelect), which the
  qualified-star rebuild re-enters — `SELECT e.* FROM (SELECT …) e WHERE EXISTS(… e.id)`
  failed loudly (42703 pre-scope-fix, 0AF00 mid-fix; never wrong rows). All THREE derived
  arms (visitor, catalog rebuild, plain rebuild) now carry the LogicalCTE(alias) wrapper,
  and the qualified-star + correlated-EXISTS class now returns CORRECT rows end-to-end
  (positive pin f in derived_exists_scope_fdb_test.go).
- **Graefe's condition — the fail-closed 0AF00 shape pinned with its exit gate:** a scalar
  subquery inside a correlated EXISTS body over a bare-scan outer hits the fail-closed
  decline (loud 0AF00; it silently returned ZERO rows before the guard — the scalar binding
  never resolved below the FOD). Matrix class K pins never-wrong-rows; the pin flips to the
  rows assert when the positional binders land (item-2 commits 2–4 — THE EXIT GATE for both
  this class and the correlation-unchecked fallback's loud replacement).
- planResultValue is now an explicit ROW-SHAPE-PRESERVING whitelist (filter/FOD/DoE), never
  a generic GetInner walk — a schema-changing plan (aggregation, projection) terminates the
  walk instead of handing the rebase a pre-aggregation schema as authority (Torvalds
  finding 1). Note: FETCH SHELLS are walk-terminators under the whitelist where the generic
  walk unwrapped them — a fetch-wrapped box winner now fails closed (loud plan loss, not
  silent misbinding); acceptable until the positional binders land (the same exit gate as
  class K). The fail-closed alias checks use EXACT identifier comparison (fails closed on
  a case mismatch; the fold failed open — finding 2). Merge-seed split coarseness accepted
  (over-routing to the correlated inner is semantically harmless — Graefe finding 2b).

**codex P2 (round 3) — a REGRESSION the wrapper introduced, caught and fixed:** the
LogicalCTE alias-carrier reuses cteScope, so a derived alias named like an enclosing
WITH-CTE (`WITH c AS (…) SELECT … FROM (SELECT * FROM c) c`) clobbered the outer binding:
registration overwrote without saving, and the body — translated LAZILY at scan
resolution — resolved its own name to the real table (delete-while-recursing), returning
ZERO rows where the base returned the CTE rows. Fixed with a proper LEXICAL-SCOPE shadow
stack (cteShadowStack): translateCTE pushes the shadowed outer binding (nil = unbound) and
restores it; every cteScope body expansion goes through inCTEDefiningScope, which pops one
level so the body's own name resolves against its DEFINING scope. Pinned: plain,
qualified-star, and EXISTS-correlated shadow forms (the EXISTS form is a loud 0AF00
limitation — buildDerivedTableSource resolves derived bodies against the CATALOG only, a
WITH-CTE body is not derivable there; never-wrong-rows pinned, booked with the
derived-alias follow-ons).
