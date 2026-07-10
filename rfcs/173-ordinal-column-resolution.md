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
  **PREREQUISITE (Graefe W4b design-ACK condition) — DISCHARGED (W4b, PR #465).** The prerequisite
  as originally worded (JOIN-inner and COMPUTED-scalar correlated subqueries "currently stay
  name-model"; InnerAlias/inner-leg aliasing unresolved) is **no longer accurate** — those shapes
  already ordinalize. The `innerContainsJoin` (shape 2) and `innerScalarIsRowColumn`/
  `innerHasAggregate` (shape 3) gates are DELETED; the InnerAlias type-collision
  (`widenLegTypesFromPlan` twin-panic) is resolved by keying the inner leg on a FRESH
  `UniqueCorrelationIdentifier()` (Java's `CorrelationIdentifier.uniqueID()`) instead of the SQL
  alias — `cascades_translator.go:3834`, `rfc173_w4b_scalar_seed.go`. The COMPUTED scalar is
  materialized at inner ordinal `_0` in `buildCorrelatedScalar`. So the INNER side ordinalizes
  unconditionally now; what still touches `NewScalarSubqueryAnchoredRecord` is only the **OUTER**
  side, and only for UNGATED outer clusters (below).
  **Actual remaining S4 correlated-scalar work — a REACHABILITY PROOF + deletion, not an
  implementation.** `NewScalarSubqueryAnchoredRecord` has exactly ONE production call site:
  `cascades_translator.go:3860`, reached only when the single-source ordinal seed didn't fire AND
  the outer is NOT a gated cluster (a gated outer declines LOUDLY at :3849-3858). The genuinely
  ungated residual shapes are correlated scalars over an unnest-bearing / existential-ON outer
  (dup-alias outers already decline loudly in `buildCorrelatedScalar`, the binding-unaware gap).
  **Reachability baseline (established, feat/rfc173-next):** with :3860 instrumented as a loud
  marker, the FULL sqldriver + core-query + embedded suites pass — i.e. no EXISTING test reaches
  the anchored fallback. S4's exit gate must complete this into an EMPIRICAL zero-producers proof:
  construct each ungated-outer shape (unnest-outer, existential-ON-outer) and assert it either
  ordinalizes or declines cleanly (0AF00) — never :3860 — then delete the call site (replacing it
  with the loud decline already above it). NOTE: Java has NO scalar-subquery-in-projection at all
  (the grammar lacks `'(' query ')'` in expression position — verified 4.12.11.0), so this is a
  Go read-side EXTENSION; Java is the reference only for the ordinal SHAPE, not the feature.
  The mechanical deletions once :3860 is dead: Delete
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
  - **Item-2 commit-2 scaffolding (design-ruling amendment A, booked at landing):** the
    DISABLED-BIRTH probe (`probeOuterBakedType` + flatMapCursor's `outerBakedType` positional
    binding arm) and the identity-FlatMap positional PASS-THROUGH (the computeResultLegs
    identity-arm propagation + `unwrapToJoinPlan`'s identity-FlatMap passthrough case) —
    coexistence-window scaffolding: once the name model dies, every row and binding is
    positional, the probe decides nothing, and the pass-through is the only path. The
    empirical zero-producers gate covers them.
  **Slices 2–3 standing obligation (Graefe):** every NEW positional-row birth site added for the
  join producers must extend the `DisablePositionalEmission` oracle gate, or the §5 differential
  silently loses coverage of the new frontier.

  **S4 demolition DESIGN-ACK (Graefe, banked — verdict: ACK the sequencing + atomic-cap SHAPE;
  NOT-YET on firing the cap).** The F1 "S4 ≡ one ~2-shift atomic commit, producers near-dead"
  framing is SUPERSEDED — the anchored producers are the live DEFAULT fallback of a per-join binary
  dispatch (`gateDecision.Gated ? buildOrdinalJoinResultValue : buildJoinResultValue→
  NewAnchoredJoinRecord`, cascades_translator.go:4247-4262/:4527-4535; unnest :1223/:1461; group-by
  rule_partition_select.go:536/:469). They die only when the GATE (`ordinalWedgeGateDecide`,
  rfc173_cluster_gate.go:101) stops declining — i.e. S4 = make the gate ordinalize (or die loud) on
  EVERY class. Rulings:
  - **Sequencing (ACK):** incremental pre-cap ordinalization slices (item 1 ✓, item 2 ✓, item 3 +
    unnest-residual in flight) each retire ONE decline class with CI green and the producer still
    standing — the right model, keep it. The FINAL flag/trio/seeds/oracle deletion is IRREDUCIBLY
    ONE atomic commit (the `AnchoredJoin` bool is a single shared flag threaded through ~8
    value-layer branches + 5 rules + the executor oracle — no half-delete compiles).
  - **Decline classes, not producers.** GROUP A — CIRCULAR declines (`:143` leg-contains-name-join,
    `:157` outer-box-enclosed [item 3 retiring], `:190` inner-cluster-leg; + the bare-twin
    `w5_unnest_gather.go:97`): lift FREE and SIMULTANEOUSLY when the name model is gone, but are
    UNTESTABLE incrementally (force one to Gated and its still-name-model sibling panics
    ordinalLegColumns). GROUP B — genuinely-blocked (`:112` existential-on-join ✓ item 2; `:106`
    lateral-unnest = W4c/W5 + unnest-residual; `:128` dup-binding STAYS loud-decline, Java has no
    such shape; `:200` arity<2 = nothing). Producer #4 (NewReEnumerationAnchoredRecord) DIES
    MECHANICALLY (positionalMergeCase :536 + dispatch pin :544 already built) — no slice.
  - **Two BLOCKERS before the cap fires. B1: build the POSITIVE whole-gate force-ordinalize SPIKE
    HARNESS** (the twin of DisablePositionalEmission — there is no positive switch today; the
    circular Group-A classes have NO other proof surface). Flip `ordinalWedgeGateDecide` to
    unconditional Gated-or-loud, run the FULL corpus + dual-window differential + the Q4 FDB row
    matrix, prove green WITH ROW AGREEMENT. Graefe wants to design-ACK the harness design before it
    is built. **B2: item 3 + the unnest-residual slice must reach master** (they retire the last
    Group-B classes + the :157 guard).
    **B1 PROTOTYPE FINDING (feat/rfc173-next, scratch spike, reverted — PROMISING):** a whole-gate
    `forceOrdinalSpike` flag guarding the three circular declines (`&& !forceOrdinalSpike`), run over
    the FULL sqldriver corpus (real FDB): (1) `:157` fires **0×** — already DEAD (item 3 retired it);
    (2) `:143` fires 51×, `:190` fires 45× — heavily live; (3) the ENTIRE sqldriver corpus PASSES
    under forced ordinalization of `:143`/`:190` — no panics (incl. the RFC153 joined-preserved
    mixed-nesting matrix), all row/plan assertions hold. This partly CONTRADICTS the
    "untestable-incrementally / forcing one panics ordinalLegColumns" concern: forcing the WHOLE gate
    at once (no sibling stays name-model) ordinalizes CLEANLY — item 1/2/3's machinery appears
    already ready for `:143`/`:190`. **Graefe design-ACK'd (2nd round): retire `:143`/`:190` as a
    PRE-CAP NARROWING slice** (not a blanket delete — a fail-closed guard that gates iff the subtree
    fully ordinalizes, declines iff a genuinely-name-model leaf [dup-binding/unnest/scalar]; the
    ordinalEligible recursion is the shape). L1 does NOT gate them (a gated join never touches the
    name-keyed reads), so they go pre-cap. The CERTIFICATE is a spike-OFF-vs-ON ROW DIFFERENTIAL
    (NOT the dual-window, which degenerates on the new frontier — both its modes carry the ordinal
    RV). **B1-harness WALL (this session):** an ISOLATED off-vs-on differential could NOT be built —
    11 constructed join shapes (incl. group-by/distinct/aggregate/union/ORDER-BY over 3-way joins)
    fire `:143`/`:190` **zero** times (markers), because SelectMergeRule flattens the seed decisions
    away; the 51+45 corpus firings come from specific queries the gate cannot trace back to SQL (it
    works on logical operators, no query text). **Conclusion: the B1 certificate must be
    CORPUS-LEVEL** — run the real SQL corpus (sqldriver + embedded + core-query) twice, spike-off vs
    spike-on, assert identical result sets — not a hand-constructed shape set. That corpus-level
    off-vs-on differential harness + the fail-closed narrowing + EXPLAIN/ordering pins + fuzz is the
    next-phase B1 slice (codex-gated review). The committed spike flag was reverted (a flag with only
    a vacuous isolated harness is dead code); it re-lands with the corpus-level harness.
    **B1 NARROWING implementation requirement (found by attempting it — the gate contract pins
    caught a fail-open):** the `:190` arm is NOT purely circular — it conflates TWO enclosure
    reasons: (a) a CIRCULAR inner-cluster enclosure (the parent gates post-flattening at arity ≥ 3
    → the nested inner join should gate too — LIFT), and (b) a GENUINELY-name-model existential /
    unnest FLATTEN enclosure (the parent stays name-model → the nested inner join MUST keep
    declining — KEEP). A naive lift of `:190` gates (b) and fails the pins
    `TestRFC173S2_WedgeGate_Translation/derived_join_leg_under_exists_filter_enclosed` +
    `TestRFC173Item2C3_FlattenGateArm/enclosed_flatten_declines` ("a narrowing failed open"). So the
    fail-closed narrowing must DISTINGUISH enclosure reason: `t.inInnerCluster` is a single flag set
    at ~8 sites (cascades_translator.go:1058 unnest, :3751 existential/scalar, :4399 flatten, :4954,
    the genuinely-name-model group; :4151 the circular join-leg group). The slice must thread the
    reason (a new "name-model-flatten enclosure" bit distinct from "inner-cluster enclosure") so
    `:190` declines only (b). That enclosure-reason classification across the 8 sites + the
    corpus-level certificate + EXPLAIN/ordering pins + fuzz is the precisely-scoped B1 slice.
    **CORRECTION (traced the mechanism at cascades_translator.go:4151):
    `t.inInnerCluster = !gateDecision.Gated && (kind==Inner||kind==Left)` — the enclosure flag is
    set ONLY when the PARENT is name-model ("enclosure poisoning survives only for name-model
    parents"). So `:190` is NOT a circular class — it is a FAITHFUL SYMPTOM of a name-model parent:
    an inner join under a name-model parent MUST stay name-model (its positional rows would be read
    by-name → wrong rows). It CANNOT be lifted incrementally — a child gates iff its PARENT gates,
    which requires the name model to be GONE. This RE-CONFIRMS the original F1 framing over the
    second-round pre-cap-narrowing optimism (which read the sqldriver-green whole-gate spike too
    favorably — that worked only because the corpus's name-model parents were all circular AND the
    spike blanket-forced the declines off, not a safe fail-closed guard). **The join name-model
    retirement is genuinely ATOMIC: :143/:190 lift together, and only when every circular parent
    gates (= the name model deleted). There is no safe incremental :143/:190 narrowing.** So B1's
    real content is the CORPUS-LEVEL off-vs-on certificate (proving the whole-gate flip preserves
    rows) as the gate on the ATOMIC cap, not a separable pre-cap slice. The cap itself
    (flag+trio+seeds+oracle deletion, making all circular parents gate) remains codex-gated
    multi-shift work.
    **ROW-LEVEL CONFIRMATION (experiment, reverted):** lifting `:190` ALONE (keeping `:143`) fails the
    FDB row suite (sqldriver), not just the unit gate-decision pins — direct proof that gating an
    inner join under a still-name-model parent produces wrong rows. The whole-gate spike passed only
    because it lifted `:143`+`:190` TOGETHER (every parent gates → consistent). And even the atomic
    cap must KEEP `:190` for the genuine existential/unnest-flatten residuals (their parents stay
    name-model via `:112`/`:106`, which the cap does not remove) — so the cap needs the
    enclosure-reason distinction (circular inner-cluster → lift; existential/unnest flatten → keep),
    the 8-site classification, NOT a blanket skip. Net: S4 has no safe incremental step; it is the
    coupled atomic deletion + the enclosure-reason refinement + the corpus certificate, codex-gated.
  - **Exit gate = 3-layer PROOF, never an inventory:** (1) static caller-free grep; (2) dynamic
    loud-marker panic in each of the 4 constructors, full corpus + fuzz, assert ZERO dynamic hits;
    (3) exhausted-decline matrix — one constructed query per gate `Reason` string (rfc173_cluster_
    gate.go:106/112/128/143/157/190/200) asserting ordinal EXPLAIN or loud 0AF00, never a silent
    anchored plan. `AssertOrdinalJoinSeed` does NOT catch name-model regressions — rest on positive
    ordinal coverage.
  - **Bare-twin lift = MANDATORY row-level FDB e2e** (a plan-shape pin is structurally blind — the
    class was declined precisely because last-leg-wins and positional-coexist resolve to DIFFERENT
    datums): `SELECT * FROM a JOIN b` where both carry `ID` (and a `NAME` case) → assert BOTH columns,
    declaration order, BOTH source datums correct; + a memo duplicate-name-identity pin; + a
    column-ORDER pin (from Type ordinals); + an ordering-property pin (no spurious sort reappears on
    an index-ordered join after the name→ordinal flip).
  - **Landmines (cap conditions):** L1 — convert silent name-keyed reads to LOUD at the cap
    (sortKeyFromResult/compareByField, legPhysicalOutputNames, recursiveRemapValues first-dot split)
    — never nil-yielding. L2 — interning blowup: `select.go:251` arm-1 + Equals/semantic_hash flag
    branches survive every pre-cap slice, die only in the cap; gate on the interning baseline pin +
    a planning wall-clock bound on a many-identical-legs STAR corpus. L3 — CARVE-OUTS the cap must
    NOT delete: `OrdinalFieldName` (ordinal infra), `select.go:274` default-false arm (F2 CTE-rename),
    the lazy FieldValue identity arm (F3, gated on full FieldValue baking). L4 — verify
    positionalTypeCache still bounded (rider #468). L5 — the §5 dualwindow differential + oracle die
    WITH the name map (the Q3 dynamic marker must replace its coverage first).
  Re-request Graefe's ACK on the spike-harness DESIGN before building it, and on the atomic cap's
  actual diff (an ACK covers only the HEAD it reviewed).
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
**[SUPERSEDED — shipped in W4b (PR #465).]** Both inner shapes below (JOIN-inner and COMPUTED
scalar) now ordinalize; the gates described here as future work are DELETED. The COMPUTED scalar is
materialized at inner ordinal `_0` in `buildCorrelatedScalar`; the JOIN-inner keys its leg on a
fresh `UniqueCorrelationIdentifier()`. See the corrected Slice-4 PREREQUISITE block. The historical
analysis below is retained for provenance.

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
   `cascades_translator.go:3860`) — **NARROWED (W4b #465):** computed / JOIN-inner / clustered-outer
   correlated scalars now ORDINALIZE (inner-side done). The lone surviving producer is the
   UNGATED-OUTER residual (correlated scalar over an unnest-bearing / existential-ON outer); dup-alias
   outers decline loudly. No existing test reaches it — S4 completes the empirical zero-producers proof
   over the ungated-outer shapes, then deletes. See the corrected Slice-4 PREREQUISITE block.
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

## S4 producer-zeroing — commit 2 (projected-EXISTS-over-join) B1 design-ACK (Graefe)

**The residual.** `SELECT a.x, EXISTS(...) AS f FROM a JOIN b` routes through `buildExistentialJoinSelect`
(cascades_translator.go:2817), which sets `inInnerCluster=true` UNCONDITIONALLY (:2833) → the outer INNER
join stays name-model (the `:698` `buildJoinResultValue`/`NewAnchoredJoinRecord` producer). This is distinct
from the WHERE-EXISTS flatten (`translateJoinWithExists`), which ALREADY ordinalizes when the join gates
(F2-clean: existentials contribute no columns) — that is why lifting the gate's `:112 OnExistsSubqueries`
arm is a no-op (it fires only for EXISTS-in-ON, a different shape). The projected-EXISTS RV *references the
existential quantifier* (the `EXISTS` flag is an output column), so it is a PROJECTION, not a full seed.

**Two empirical corrections to the initial framing (verified in the executor).** (1) The gated-ordinal
projected-EXISTS *rebase* machinery is ALREADY built and live (`rule_implement_nested_loop_join.go`
:1943-2071 — `rebaseOuterLegRefsOrdinal`/`rebaseOuterLegValueOrdinal`/exist-QOV→existCorr), gated on
`ordinalWindows != nil`; and the `:1793` projected-fold decline is `correlatedStep1`-only. (2) The real
blocker is the WINDOWS SOURCE: the executor reads windows from `sel.GetResultValue()` (the projection),
which BOTH layout twins reject — `OrdinalSeedLegWindows` (a non-baked `ExistsValue` field + the
full-coverage invariant) and its executor twin `ordinalJoinSpansOf` (`rfc173_ordinal_join.go:216`
`if s.Width != len(s.LegType.Fields) { return false // partial leg coverage — a folded projection }`). A
projection is categorically not a full-coverage seed.

**Ruling: B1 (windows from the complete merged row), both twins UNCHANGED. B2 (relax full-coverage) NAK'd**
— it would force relaxing both fixture-pinned twins in lockstep, breaking merged-row completeness
(wrong-offset wrong-rows). Structure: **step-1 NLJ RV = the full ordinal leg-concat seed**
(`buildOrdinalJoinResultValue` over the two legs), the projection applied ONLY at **step-2** (leg refs as
`ofOrdinal` over `mergedOuterCorr`, `ExistsValue` over `existCorr`, live binding). Windows derive from the
complete seed (the rule reconstructs it, byte-identical to `translateJoin`'s seed; the cross-agreement
fixture guards the reconstruction). Q2 execution gap: the disabled-birth binder (`probeOuterBakedType`,
walks only the inner plan) must ALSO see baked outer refs in the FlatMap RV. I3 is a no-op (INNER-only
flatten, both legs non-null); I2 belongs to the scalar slice, not here.

**⚠️ Scoping wall (the F2 seed was REVERTED TWICE here — A2 above).** The 2+1 existential select also
implements through CORRELATED-FlatMap step-1 paths whose bindings are NAME maps; a baked seed hits the loud
`BakedNameContextError` live (the `corr_exists_join_outer` dualwindow entry). **Commit-2 B1 scope =
INDEPENDENT-LEGS projected-EXISTS only** (`correlatedStep1==false` → the materialized-NLJ branch, the
primary `t1 JOIN t2 ON t2.t1_id=t1.id` shape). The `:1793`/`:1784` correlated-step-1 declines STAY
(fail-closed); correlated projected-EXISTS defers to the booked correlated-FlatMap positional binders.
Getting this scope wrong is the third revert. **Impl-ACK deliverables:** (1) B1 structure landed, both twins
unchanged; (2) exec-binding confirmation (`probeOuterBakedType` fires on RV baked refs, proven by a
projected-EXISTS row flowing positionally e2e); (3) two cross-agreement fixture rows (both-accept
reconstructed seed / both-decline folded projection); (4) item-5 white-box sentinel + EXPLAIN + the
`projected_exists_*` matrix green under both windows (dualwindow — the net that caught both prior reverts).

**LANDED via C (the design-revision, not B1's rebase).** Two B1 impl attempts failed on the
twice-reverted data-form wall: the fold projection's leg refs are HETEROGENEOUS — dotted frontier
reads (`FieldValue{Field:"T1.ID", Child:nil}`, the common `SELECT t1.id` case) AND QOV refs —
which no rebase converts to positional. The revision (Graefe): **don't rebase the projection —
resolve it.** The step-1 NLJ births the full leg-concat seed (`reconstructFoldStep1Seed` from the
leg plans' `GetResultType`, gated by `foldStep1Seed` on independent, ordinal-safe SCAN legs — the
`legWindowRowContext` inputs need no translator change), and the step-2 FlatMap cursor evaluates the
folded projection over that positional row through `legWindowRowContext` — `spanAwareRow.GetByName`
resolves the dotted reads by splitting at the executor against typed `Legs` (not SQL text),
`legWindowBinder` the QOV refs, and the composed context binds the existential inner for the
`ExistsValue`. Both layout twins UNCHANGED (consulted only for the full-coverage step-1 seed). The
identity WHERE-EXISTS pass-through is excluded (`resultIsIdentityOuter`). Scope: independent scan-leg
folds (the common case); correlated folds (the F2 NAME-binder wall) and buried gated-box legs are
booked follow-ons. Pins: three white-box guards (`reconstructFoldStep1Seed` / `legIsOrdinalSafe` /
`foldStep1Seed` — the wiring gate the dualwindow differential is blind to, since a name-model revert
leaves both windows agreeing) + the `projected_exists_*` matrix + dualwindow, all green; no regression
across the exists / join / unnest / CTE / aggregate FDB suites. In-session gates GRANTED on HEAD
817191109 (impl 8e9ee26d9 + cursor pin 6a489a0c5 + code-quality polish 817191109): the query-engine
architect impl-ACK (verified line-by-line — resolve-don't-rebase, both twins untouched, the cursor
branch pinned discriminatingly) and the code-quality ACK (no CRITICAL/MAJOR; MINOR polish + index-leg
coverage folded in). @claude + codex remain as merge-gates (pending a pushed PR / quota). Scope:
independent scan-leg folds; correlated folds (the F2 name-binder wall) and buried gated-box legs are
booked as their own future design+impl slices.

## ROADMAP STATE (authoritative; update this block as slices land)

**Done:** Slice 1 (non-join frontier + §5 oracle) · Slice 2 (wedge gate, 2-way seeds) ·
Slice 3 (fulcrum, N-way clusters) · W4b (clustered-outer correlated scalars, PR #465) ·
W5 (multi-source unnest gather, PR #466) · W4-left (LEFT/RIGHT + EXISTS + recursive-CTE,
PR #467, four-gate ACK) · S4-riders (positionalTypeCache bound + rcte remap provenance,
PR #468, four-gate ACK) · item-2 commit 1 (PR #469 MERGED, master 33291617d, four-gate
ACK: Graefe design+audit+impl, Torvalds after two converted NAKs, codex clean with P1+P2
fixed-and-pinned, @claude clean ×4 — the no-op existential residual via the Java-shaped
correlated step-1, plus the review-found classes: Java predicate placement, merge-seed
split, derived-alias EXISTS with all three derived-arm alias carriers, the CTE-shadow
lexical-scoping regression) · **item-2 commit 2 (PR #471 MERGED, master 12c39e032,
four-gate ACK: Graefe ACK ×2 + Torvalds ACK ×2 (impl + adapted-row delta), codex P2
found→fixed-and-pinned→clean delta, @claude LGTM ×2 with the delta confirmed, CI 6/6 —
the E2 disabled-birth binder (walkBakedRefs + probeOuterBakedType + the loud positional
binding arm), the PROBE-GATED identity-FlatMap positional pass-through publishing the
ADAPTED row, and the unwrapToJoinPlan identity arm; two live catches in the commit-2
delta record: the flat-dotted name-model upper class and the mismatched-layout
covering-outer class) · **item-2 commit 3 (PR #472 MERGED, master f847f7c58, four-gate
ACK: Graefe ACK ×3 (impl + reverse-byte condition + the clusterArity→outerBoundAliases
authority ruling), Torvalds ACK ×2, codex P2+P3 found→fixed-and-pinned→clean delta,
@claude two findings-rounds resolved incl. the false-"deduped"-claim correction, CI
6/6 — translateJoinWithExists' gated flatten seeds the baked ordinal RC (existentials
carry no columns); TWO prerequisite bugs the c3 pins forced out: the nondeterministic
#17 cost tie-break (stablePlanHash — alias-blind per-node + order-sensitive fold, the
minted-q$N/commutative-XOR double blindness) and the order-DEPENDENT unnest array read
(qualified SEG0.FIELD on any multi-namespace outer — authority is outerBoundAliases,
not clusterArity, so the FULL-OUTER merge-opaque box is covered)).**

**item-2 commit 4 (PR #475 MERGED, master b96ba7d8a, four-gate ACK: Graefe ACK ×2
(core + the one-way-door P2 delta), Torvalds ACK ×2 (reverted the fix to prove the pin
real), codex P2 found→fixed-and-pinned→clean delta, @claude round-1 no-correctness +
both suggestions folded (RIGHT + FULL pins) + config-only confirm, CI 6/6 — the
enclosure lift making the W4-left ordinal existential rebase LIVE):** the generic
filter arm (cascades_translator.go
translateFilter) forced enclosure under EXISTS unconditionally, so a single-source
LEFT/RIGHT box under WHERE-EXISTS gated arityPoison (name model) and
rfc173_w4left_existential.go's rebaseOuterLegRefsOrdinal was DEAD. Commit 4 routes the
EXISTS enclosure through the ONE gate authority (existsOuterGatesFresh →
ordinalWedgeGateDecide, ruling condition 4): a gate-eligible LEFT/RIGHT box is NOT
enclosed, gates ordinal, and implementExistentialSelect's below-FOD ordinal rebase
fires (the 1+1 twin of the 2+1 implementJoinWithExistential rebase — binds the box
row's baked ordinals under the box's own outerCorr, resolved positionally by the
commit-2 disabled-birth probe). Buried/clustered/FULL boxes and non-join inputs keep
the name-model enclosure. **PLUS a PRE-EXISTING wrong-rows bug the class surfaced (bug
found → fixed → red-first pinned, confirmed on master):** the existential FAST PATH
(tryExistsFlatMap/buildExistsFlatMap) built `QOV(boxAlias).<bareCol>` for a correlation
into a BURIED box leg — matchJoinPKPredicate's deep-flowed arm accepted the buried ref
without rebasing, so a colliding column name (GID in both legs of `la LEFT JOIN lb`)
read the WRONG (rightmost) leg last-leg-wins. Fixed by declining the fast path when the
correlation targets a buried leg (outerValRefsBuriedLeg) → routes to the below-FOD
rebase, leg-correct for both name-model (qualified) and ordinal (baked) boxes.

**Review round (banked): codex P2 (nested enclosure), fixed-and-pinned.** The lift
`t.inInnerCluster = !existsOuterGatesFresh(f.Input)` cleared enclosure whenever the box
gated FRESH — but existsOuterGatesFresh probes with a FRESH position and cannot see an
AMBIENT enclosure, so a WHERE-EXISTS box that is itself a leg of a larger name-model
merge (prevEnclosure true) got un-enclosed and seeded ordinal, which the parent then
mis-binds. Graefe's "one-way door" applied to a nested context. Fixed:
`prevEnclosure || !existsOuterGatesFresh(f.Input)` — already-enclosed stays enclosed.
De Morgan: enclosure clears iff `!prevEnclosure && gates-fresh`. Red-first pinned; @claude
folded RIGHT-box-gates + FULL-box-excluded pins. **CI-resilience fix:** dualwindow_test
(a dual-model full-corpus FDB differential) was on the default size="medium" (300s)
while its FDB-container siblings are size="large" — it TIMED OUT (not failed) under a
contended runner (~31s local, ~10x stretch past 300s). Bumped to size="large"
(pre-existing config oversight, surfaced on this PR's CI).

**CORRECTION to the commit-4 pre-banking claim (verified empirically):** commit 4 flips
NONE of the PR #469 loud-limitation exit gates. Class K (scalar-in-EXISTS) is a
BARE-SCAN outer, not a box, so ordinalSeedLegWindowsOf yields nil and its scalar
binding STILL hits the below-FOD fail-closed 0AF00 decline (probed live: still
declines). The fetch-shell walk-terminators and the alias-unchecked frontier fallback
are likewise bare-outer / fetch-wrapped classes untouched by the box enclosure lift.
Those flip with the SCALAR-RIDER / absorbed-class work (commit 5) or when the
fail-closed decline itself is replaced — NOT here. Commit 4's observable is the LEFT/
RIGHT-box-under-EXISTS class running ordinal (dualwindow-equivalent today; S4-ready)
plus the fast-path wrong-rows fix.

**← WE ARE HERE:** QP-REF-BIND **item 1 — design substrate BANKED, awaiting the
Graefe design ruling** (see § "QP-REF-BIND item 1 — design substrate" at the end of
this file; forks F-A [star layout in-slice vs S4] and F-B [message unification scope]
need rulings BEFORE the first impl commit). Item 2 is CHARTER-COMPLETE (2026-07-05):
commits 1–4 (PRs #469/#471/#472/#475) + 5a (#476) + B/C/D/E (#478) + 5c (#479) +
5b (#480) ALL MERGED. The item-2 5a record follows: commit **5a MERGED** (PR #476, master 2dbe743d5,
four-gate tally across ELEVEN review rounds: architecture gate ACK round-10 substance +
round-11 delta — adversarial battery of 16,104 constructible RC shapes, 0 cross-agreement
disagreements; Java differential oracle 1632 corpus entries, 0 mismatches — code-quality gate
ACK round-11 after two NAK rounds whose findings were closed with revert-tested load-bearing
pins; codex CLEAN rounds 9/10/11; the @claude Action runs were starved by the CI-runner
infra failure, best-effort, no NAK). The saga's yield: the STRUCTURAL exit gate (translator
never predicts executor routing), a truly bit-for-bit cross-agreement invariant, ONE rebase
authority, and −110 lines of prediction apparatus. NEXT was the B/C/D/E batch — RESOLVED in
PR #478 (see the follow-ons block below); then 5b/5c.
5a decline-lift at translateUnnestJoin removes the blanket
`!unnestUnderExistential` guard but keeps a `unnestExistsSeedSafe` gate: the ordinal seed is
taken under EXISTS only for a SINGLE-ALIAS outer (outerBoundAliases==1, not clusterArity==1 —
a merge-opaque FULL OUTER box has arity 1 yet two aliases whose same-named columns the ordinal
rebase's FieldIndex cannot disambiguate → stays name-model). The EXISTS correlation is left
LEG-RELATIVE for an ordinal seed: EVERY ordinal seed — mixed no-AT AND fully-baked AS+AT — now
carries executor windows (OrdinalSeedLegWindows, accept-equivalent to the executor's
ordinalJoinSpans by construction), so the executor's below-FOD hoist rebases each inner-residual
outer ref POSITIONALLY; only an ANCHORED seed (a multi-alias outer, or an inner-scope collision)
rebases by name. ONE layout authority — round-10 DELETED the translator's ordinal pre-rebase
(rebaseUnnestOuterLegPredicateOrdinal), no more per-shape prediction. Executor
`tryExistsFlatMap`/`buildExistsFlatMap`
PRESERVE a FrontierPinned outer operand (correlatedFastPathOperand) instead of re-deriving it by
bare name — a name re-derivation misreads a shadowed/duplicate merged-row column (round-1
findings: shadowing dropped every row; the AT+equality fast path swallows the correlation so the
double-rebase was LIVE, not latent, on any AT+non-equality shape — R5e pins it). Four-gate:
Graefe + Torvalds ACK (round 2); codex round-2 flagged a PERF asymmetry (booked, below).
**5c MERGED (PR #479):** the below-FOD fail-closed rebase authority passes SCALAR-SUBQUERY
aliases through (structural ScalarSubqueryValue detection; root-context bindings, not buried
legs) — class-K flipped to rows (see the exit-gate flip note below). **5b IMPLEMENTED (this
branch):** rider subqueries are TRANSPARENT to the cluster gate — clusterArity and
ordinalEligible recurse through EXISTS-rider filters (the existential rides the
post-flattening merge, which the 2+1 flatten's seed threads; the leg's output boundary is the
identity RV — the source row) and through uncorrelated-scalar riders (root-context bindings);
a CORRELATED projection scalar still poisons (the W4b clusterPullUp rework, booked). Pins:
white-box red-first gate flips + the CTE-EXISTS-body-as-join-leg e2e shape, both polarities.
**RESOLVED in 5a (was a deferred perf item, turned out to be CORRECTNESS):** an AT+EQUALITY EXISTS
correlation (fully-baked seed → executor authority) was left leg-relative for the below-FOD rebase,
which ran AFTER tryExistsFlatMap. Deferred round-2 as a mere perf loss (the fast path declined the
parameterized scan → per-row full inner scan). Round-6 (codex) showed it is CORRECTNESS: when a
fast-path EQUALITY on the element/ordinal (`JU.ID = O`) is swallowed by the fast path, a SIBLING
inner-residual referencing the outer (`JU.K < MA.ID + 1000`) is left UNREBASED and MA.ID evaluates
against the inner row → non-deterministic wrong rows. Fixed by HOISTING the window rebase
(ordinalSeedLegWindowsOf + rebaseOuterLegRefsOrdinal) ABOVE the tryExistsFlatMap call (once, over
all regularPreds), so BOTH the fast path and the below-FOD path see baked positional refs — single
rebase authority, no double-rebase (the below-FOD window branch becomes a no-op, guarded by
windowsHoisted). This ALSO restores the AT index-scan perf. Pinned by R5r (AS+AT fast-path sibling
→ {20}); NOT a master regression (master was full-scan / name-model for both AT and no-AT).

**5a follow-ons (pre-existing wrong-rows bugs surfaced by the 5a review) — RESOLVED in the
B/C/D/E batch (PR #478), except the two booked residuals below:**
(B) **FIXED — NOT-EXISTS outer-only-conjunct pre-filter.** Not negation-blind ROUTING but
PLACEMENT: an outer-only conjunct from INSIDE the subquery's WHERE rode the JoinPredicate
channel into the polarity-blind outer pre-filter (`P ∧ ¬∃(Q)` instead of `¬∃(P∧Q)`).
buildCorrelatedExists now keeps subquery-origin outer-only conjuncts INSIDE the subquery
(a LogicalFilter on the inner plan — under the ∃ in both polarities); the lateral-unnest arm
makes the buried predicate BINDABLE in place (ordinal seed: baked ofOrdinals over the typed
merged QOV — rebaseUnnestOuterLegPredicateOrdinal returned for the BURIED channel, the one
place the executor's rule-level hoist cannot reach, so the translator is its single rebase
authority; anchored seed: the qualified LEG.COL rebase). Pins: the exists_semantics_probe
outer-only dimension (both polarities, mixed, sibling control) + R5t/R5u.
(C) **FIXED (uncorrelated) / LOUD (correlated) — scalar subquery inside EXISTS WHERE.** Two
stacked defects: buildCorrelatedExists DROPPED the nested planner's scalar plans (the
pre-evaluated binding lives in the ROOT evaluation context and is visible below the FOD too,
so a scalar-referencing subquery-internal conjunct buries under the ∃ like every other
outer-only conjunct — the round-1 review's NOT-EXISTS+scalar catch proved a
JoinPredicate-channel exception reproduces the pre-filter polarity bug; RFC-141 R4's outer
routing concerns SIBLING predicates outside the ∃ only); and — not subquery-specific — a PLAIN-COLUMN
aggregate arg (parser: aggArg text, NO aggExpr) never reached the resolver, so the lossy text
reparse kept a qualified arg as ONE opaque dotted FieldValue that key-missed bare-keyed rows →
MIN/MAX/SUM silently aggregated NULL. upgradeAggregateOperands now resolves plain-column args
via the semantic scope (ResolveIdentifier) with a bare-form slot-matcher fallback. A CORRELATED
scalar inside an EXISTS WHERE (per-inner-row — no evaluation path) now declines LOUDLY
(CorrelatedExistsError; was silent []). **Residual booked:** per-row correlated-scalar
evaluation under the semi-join (flips the loud pin to rows).
(D) **NOT A BUG — dissolved by analysis, pinned.** The booked expectation violated SQL scoping:
inside the subquery a bare column binds INNERMOST-first, so `U.ID = ID` with an inner table
that HAS an ID column is a tautology on non-null U.ID — 'all rows' is the CORRECT answer (the
booked want-{} was wrong). The genuine shadowed-element shape (element alias shadows an OUTER
column, no inner collision) already binds correctly through the structural fix's element
window. Pins: R5v (discriminating values prove the ELEMENT is read over shadowed TCOLL.VAL) +
R5w (the inner-collision scoping control).
(E) **LOUD (was silent) — multi-table-inner EXISTS with an element-referencing conjunct.**
The conjunct needs below-FOD evaluation with the element bound from the outer row; that
multi-table threading does not exist, and the shape silently returned []. translateUnnestExistsFilter
now declines it loudly (ErrCodeUnsupportedQuery). Pin: R5x (loud-or-correct-rows; fails on the
silent []). **Residuals booked:** the element-scoped multi-table threading (flips R5x to rows:
{10,11,12} / {20,21}), and the fast-path residual routers (rule_implement_nested_loop_join.go
:2073/:2117) still splitting on `{existInnerCorr}` membership alone (pre-RFC-141-R4 test).

**Booked structural refactor (both Graefe + Torvalds, round-4/5):** the translator's
existsHasOuterOnlyLegConjunct hand-maintains a PREDICTION of the NLJ rule's inner-vs-outer routing;
each divergence between the two copies has cost a review round (R5m/R5n were one such). Round-5
aligns the predicate EXACTLY to the rule's (references-outer-leg ∧ ¬references-inner-leg, inner
legs from esq.Plan — the complement of predicateReferencesInnerLeg), closing the known divergence.
DURABLE exit (principle 10, emergent > bolted-on): either (a) move the ordinal-decline decision
INTO the rule at the point of routing authority (CORRECT-or-LOUD), or (b) give the no-AT mixed seed
the same per-leg WINDOWS the AT seed carries (a partial outer-leg window over the baked prefix,
element excluded) so outer-routed predicates always bind positionally — then the flag AND the
detection predicate evaporate. (b) is blocked on OrdinalSeedLegWindows accepting a partial
(outer-run-only) window.
**★ SLICE EXIT GATE (Torvalds round-7, mandatory):** this refactor is NOT optional post-5a polish
— it is the EXIT GATE for the under-existential-unnest slice.
**★ DONE (option b) — the exit gate is met.** After codex round-8 (the inner-alias==output-alias
collision), the structural fix landed: values.OrdinalSeedLegWindows now accepts the MIXED no-AT
seed and synthesizes the whole-object element's OWN 1-field window (matching the executor's
unnestMixedSeedSpans, pinned bit-for-bit by the cross-agreement fixture — the invariant whose
absence caused every round). So the mixed seed carries executor windows too; the translator NEVER
pre-rebases an ordinal EXISTS correlation and NEVER predicts the rule's inner-vs-outer routing —
the executor's below-FOD hoist rebases every inner-residual outer ref POSITIONALLY, and the rule
routes by the renamed correlation identity. DELETED: existsHasOuterOnlyLegConjunct (the classifier),
the unnestExistsAnchored flag, the shadow-column decline. The shadow-column / outer-only-conjunct /
element-shadow / round-8 shapes are all handled positionally now (R5d/j/k/m/n/p/s green without any
decline). TWO scope gates SURVIVE in unnestExistsSeedSafe (genuine single-source boundaries, NOT
routing predictions): (1) a MULTI-ALIAS outer (needs the W5 leg-splice to positionalize; deferred);
(2) an EXISTS inner scanning a table aliased the SAME as an outer FROM leg (existsInnerScopeCollidesOuter
— a leg-relative outer ref would be captured by existsInnerCorrelation's inner-alias rename; stays
name-model). **Round-10 (the four-gate exit):** the residual ordinal pre-rebase
(rebaseUnnestOuterLegPredicateOrdinal + values.MergedSeedType + the executorHasWindows plumbing) is
DELETED — one rebase authority remains (anchored → name-model qualified key; ordinal → leg-relative
for the executor). The AT-ONLY partial-coverage seed it guarded is UNREACHABLE: the parser's
unnestAliases always defaults the AS alias to the last path segment, so every AT unnest is
fully-baked → windowed (a panic on the old arm fired 0/32 across the R5 EXISTS suite; the full R5 +
AT matrix stays green with the arm gone). And OrdinalSeedLegWindows is now ACCEPT-EQUIVALENT to the
executor by construction (mixed → exactly one outer leg, `len(windows) != 1` declines; pristine →
`len(windows) < 2` declines, mirroring ordinalJoinSpansOf) — the cross-agreement fixture locks the
DECLINE boundary (multi-leg / single-leg / record-element / lone-element decline in both walks) and
compares field TYPES, closing the drift the round-9 review measured (values accepting a shape the
executor declined while the pin stayed green). The pre-existing name-model bugs B/C/D/E are
RESOLVED in the PR #478 batch (fixed / loud / dissolved — see the follow-ons block); only their
two booked residuals (per-row correlated-scalar eval; multi-table element threading) precede S4.
**Class-K FLIPPED (commit 5c):** the fail-closed rebase authority now passes SCALAR-SUBQUERY
aliases through (structural ScalarSubqueryValue detection — a pre-evaluated ROOT-context
binding every below-FOD filter arm threads, not a buried leg), so scalar-in-EXISTS over a
bare-scan outer plans and returns rows; the matrix class-K pin is tightened to ROWS-ONLY
([alice]), retiring its loud 0AF00 arm. The fetch-shell / alias-unchecked exit gates still
flip with the remaining residuals (the fail-closed decline's replacement).
Commit-3/4 bookings still open (fold in as touched): the unwrap-arm/probe implicit
coupling wants a cross-check assertion if touched (@claude c2); a third
baked-QOV-extraction copy triggers forEachBakedQOVType (Torvalds c2); the
stablePlanNodeHash type-switch widening (remaining ~30 tag-only node types) if a
REWRITING/PLANNING EXPLAIN flip ever surfaces (@claude c3); and the below-FOD FULL-box
enclosure lift + the fast-path buried-leg optimization (declined for correctness in
commit 4, could be restored via a qualified/ordinal rebase in the fast path) if a perf
need arises. Item 1 (per-reference dup-alias binding) is parallelizable.

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

## Item-2 commit 2 — site plan (banked at branch open; IMPLEMENTED — see the delta record below)

The E2 binder + identity-FlatMap positional pass-through, concrete sites:
1. **One walker (design-ruling amendment B — single derivation path):** factor the
   baked-ref plan walk out of ordinalJoinBirth.widenLegTypesFromPlan
   (executor/rfc173_ordinal_join.go:857 — note it EARLY-RETURNS on disabled births today)
   into a shared walkBakedRefs(plan, collect); widenLegTypesFromPlan keeps its
   width-divergence panic on top.
2. **The probe:** in newFlatMapCursor (executor/flat_map_cursor.go:71 block), when
   !birth.enabled(), probe the INNER plan via the shared walker for FrontierPinned refs
   whose QOV correlation == outerAlias; a hit yields the typed RecordType → record on the
   cursor (outerBakedType).
3. **The binding (amendment B, loud):** at the outer-binding site (flat_map_cursor.go
   :234-243), outerBakedType non-nil ⇒ bind via adaptLegPositional(outerRow, type);
   adaptation failure is a LOUD error (zero-match tripwire), never the Datum fallback.
4. **The pass-through (I1):** in computeResult, identity-QOV(outerAlias) result value +
   outer row carrying Positional ⇒ emit the outer's Positional on the output QueryResult
   (today the positional row dies at the FlatMap boundary).
5. **Tests:** E2-style executor-level FDB tests constructing baked-inner/name-outer
   FlatMap shapes directly; e2e activation of the class-K / fetch-shell exit-gate pins
   arrives with commit 3's flatten seed (verify and document which pins flip WHEN).
6. **Amendment A booking:** the probe + pass-through go on the S4 kill list (dead
   scaffolding once the name model dies; the empirical zero-producers gate covers them).

### Commit-2 implementation delta record

All six steps landed as planned, plus TWO amendments the site plan missed — one
required companion and one live-caught gate:

1. **`unwrapToJoinPlan` gained an identity-FlatMap passthrough case.** The I1
   pass-through flows a MERGED outer's positional row (the E1/commit-3 shape: seed NLJ
   under the existential FlatMap) across the boundary, but `downstreamLegWindows`
   stopped its unwrap at the FIRST FlatMap — consumers above the identity FlatMap would
   have read the merged row through `frontierRowContext` with LEG-RELATIVE ordinals
   against ABSOLUTE slots (the W3 wrong-slot hazard, silent for any non-first leg). The
   identity-over-OUTER FlatMap is a row-preserving passthrough by construction (the
   cursor re-emits the outer row — qualifyOuterRow + the I1 positional), so the unwrap
   continues into `GetOuter()`; INNER-identity RVs and every other RV shape keep the
   FlatMap terminal. Pinned by TestRFC173Item2_UnwrapIdentityFlatMap (both bounds).

2. **The pass-through is PROBE-GATED (live catch, first full-suite run):** the site
   plan's "identity RV + outer row carrying Positional ⇒ emit" was too wide. A
   NAME-model existential (probe negative — lazy inner refs) has name-shaped uppers
   that read qualifyOuterRow's qualified Datum keys as FLAT DOTTED fields
   (`FieldValue{Field: "E.FNAME"}`); the unconditional pass-through flipped those
   consumers onto the ordinal path, where the dotted name loud-misses the bare-named
   positional row — TestFDB_CorrelatedExistsCrossJoin failed with
   `OrdinalResolutionError{Field: "E.FNAME", Available: [ID FNAME]}`. `outerBakedType
   != nil` is the ordinal-era discriminator, so the emission now rides the probe. NOT
   name-fallback leniency (banned): an ordinal-era shape whose probe is negative but
   whose uppers are baked fails LOUD downstream (BakedNameContextError) — widen the
   gate when commit 3 materializes such a shape, with the loud error as the tripwire.
   Pinned both ways (probe-positive emits verbatim; probe-negative emits none —
   executor unit + FDB e2e).

Implementation notes: the probe reuses the factored `walkBakedRefs` (amendment B's one
derivation path) with its own width-divergence panic; the binding arm mirrors the
enabled-birth arm's oracle gate (`!DisablePositionalEmission` — under the §5 oracle the
Datum binding stays and baked reads bridge via OracleBakedNameFallback, pinned by the
FDB oracle test); the pass-through is PROPAGATION, not a birth (no oracle gate needed —
the executeMap frontier-propagation precedent; a Datum-only outer emits nothing).

**Exit-gate pin verification (the "which flips WHEN" obligation):** commit 2 is
DARK-live — no live SQL path produces a disabled-birth FlatMap with baked inner refs
until commit 3 seeds the flatten, so the class-K / fetch-shell / alias-unchecked
loud-limitation pins booked on PR #469 do NOT flip with this commit (full suite green
with the pins unchanged). They flip with commit 3's seed.

RED-first record: TestIntegration_RFC173Item2_DisabledBirthBinder_BakedInner failed
pre-fix with the exact predicted BakedNameContextError (`baked FieldValue CUSTOMER_ID#0
evaluated against a non-positional row context`) and went green with the binder.

**codex round 1 (P2, fixed-and-pinned):** the pass-through published the outer's
ORIGINAL positional row even when its layout diverged from the baked type (a
covering-index outer [V, ID] under a baked [ID, V] QOV) — the binding arm adapted
correctly for the inner, but downstream baked ordinals above the FlatMap got the wrong
layout: a SILENT wrong-slot read, the exact class this RFC kills. The pass-through now
publishes the ADAPTED row (same derivation as the binding arm — same object on a
layout match, baked-layout synthesis on a mismatch, LOUD on failure), still gated on
the outer actually carrying a positional row (propagation, not a birth — a Datum-only
re-synthesis here would be an unregistered §5 birth site). Pinned red-first
(mismatched-layout subtest, TestRFC173Item2_ComputeResult_PassThrough).

**Impl reviews, round 2 on the adapted-row fix (banked): Graefe ACK + Torvalds ACK +
codex clean delta.** Single-derivation confirmed (pass-through and binding arm share
adaptLegPositional on identical inputs — pure, no drift); the propagation boundary
ruled principled (a probe-positive Datum-only outer must NOT synthesize — that would
be an unregistered §5 birth site; the loud downstream error is the correct commit-3
widening posture). Non-blocking notes banked for commit 3: (Graefe) a LOUD else-branch
on the pass-through's `adapted.(*PositionalRow)` assertion would match amendment B —
Torvalds' counter-analysis: the nil-skip already degrades LOUD downstream
(BakedNameContextError), so current behavior is CORRECT-or-LOUD; revisit if the
adapter ever returns another OrdinalRow type. (@claude) the unwrap arm's RV-shape gate
and the runtime probe gate are IMPLICITLY coupled (windowsOK only matters when
Positional is non-nil, which requires the probe positive on the same outer plan) —
worth a cross-check assertion if either side is touched again.

**Impl reviews, round 1 (banked): Graefe ACK + Torvalds ACK.** Both verified the
factoring behavior-identical, the probe a property derivation (not a smuggled flag),
the probe gate a real discriminator with both mismatch quadrants CORRECT-or-LOUD, and
the unwrap-arm/runtime-gate asymmetry closed by the consumers' `Positional != nil &&
windowsOK` conjunction. Forward bookings for commit 3: (Graefe, non-blocking) when the
pass-through gate widens, derive the shape's era from the OUTER plan's seed authority
(the joinPlanSpans authority the unwrap arm already uses), not by stretching the
inner-probe heuristic; (Torvalds) a THIRD copy of the baked-QOV extraction preamble
triggers the shared `forEachBakedQOVType` refactor, and the loud-widening commitment
(probe-negative + baked uppers ⇒ BakedNameContextError, never silent) is held.

## Item-2 commit 3 — implementation record (flatten gate arm + ordinal seed)

**Landed on the c3 branch in three pieces, each red-first:**

1. **Planner-determinism prerequisite (found by the c3 pins before the seed went in):**
   the c3 EXPLAIN-determinism pins caught the existential flatten's step-1 NLJ operand
   order flipping ACROSS PLANNINGS of the same query. Root cause, two layers: the #17
   cost tie-break's child fold was a commutative XOR (swapped operands hashed equal),
   and the per-node hash (HashCodeWithoutChildren) folds MINTED correlation
   identifiers (q$N — fresh every planning) both directly (FlatMap aliases) and via
   predicate Explain text — so the two cost-tied candidates ranked differently on
   every planning (debug-verified: both candidates coexist in one ref; their hashes
   change per planning; which is smaller is effectively random). Fix: `stablePlanHash`
   — per-node type tag + alias-blind stable content (predicates/values through the
   alias-blind SemanticHashCode) + ORDER-SENSITIVE child fold. Java-exact doctrine:
   planHash is alias-blind at every node (QuantifiedObjectValue.planHash folds
   BASE_HASH only) and an ordered list hash. HashCodeWithoutChildren untouched (memo
   identity — aliases are sometimes semantic there, e.g. TempTable). Pinned: unit
   (order sensitivity + minted-alias blindness) and e2e (EXPLAIN ×10 both polarities +
   plain-join control).

2. **The deterministic tie-break exposed a LATENT wrong-rows bug on master:** the
   lateral-unnest Explode read its array via the BARE merged-row key whenever the
   unnest source was the RIGHTMOST FROM leg — last-leg-wins, i.e. correct only while
   the join's execution operand order cooperated. Under the swapped operands,
   `SELECT X FROM PA, PB, PB.ARR AS X` exploded PA's array. Fix: every MERGED outer
   (cluster arity != 1) reads the QUALIFIED SEG0.FIELD key (order-independent, present
   for every leg); single-source outers keep the bare read. Pinned by the existing
   unnest matrix, which the deterministic tie-break now drives through the swapped
   order.

3. **The seed itself:** translateJoinWithExists consults ordinalWedgeGate (ONE
   authority, ruling condition 4) with two flatten-specific narrowings — arity
   EXACTLY 2 (the arm builds exactly two ForEach legs; a nested-cluster leg would
   drift the seed against SelectMergeRule's post-flattening arity) and no
   existential-alias collisions (fail toward the name model). Gated: legs translate
   FRESH (the translateJoin gated-parent convention), the RV is the baked ordinal RC
   over the two ForEach legs (existentials contribute NO columns — Java's model), and
   the COMBINED predicate list (ON + WHERE conjuncts + EXISTS correlation predicates)
   bakes via bakeGatedJoinPredicates — the baked correlation predicates are what flow
   into the existential FlatMap's inner plan and trigger the commit-2 binder. The
   W4-left F2 scope note is retired at the site. Proof the seed FIRES (the e2e
   EXPLAIN summary renders gated and anchored identically):
   rfc173_item2_c3_flatten_gate_test.go asserts the recorded gate decision AND the RV
   shape (FrontierPinned baked RC, never anchored) with all three narrowings pinned
   declining; rfc173_item2_c3_flatten_seed_fdb_test.go pins the e2e row matrix (star
   both polarities over dup-named legs, qualified projections, conjunct mix,
   uncorrelated scalar rider absorbed, second-leg correlation) + EXPLAIN determinism.
   One stale pin updated to the new contract
   (TestRFC173S2_WedgeGate_Translation/exists_outer_leg: the flattened 2-way join now
   GATES at arity 2).

**Exit-gate verification at the seed:** the full suite (incl. dualwindow, which now
exercises the LIVE seed — corr_exists_join_outer runs ordinal end-to-end) is green
with the PR #469 loud-limitation pins UNCHANGED: matrix class K (scalar-in-EXISTS)
routes through the 1+1 buildExistentialSelect path, untouched by the flatten seed —
it flips with commit 4's enclosure lift, as do the fetch-shell terminators. The
alias-unchecked frontier fallback's loud replacement remains booked on the item-2
completion follow-up.

**@claude round 1 on the c3 PR (banked; fixed-and-pinned vs booked):** RESOLVED
NOT-A-BUG (round 2, after a false "deduped" claim — corrected here): the "BUILD
duplicate" is srcs (:290) + embedsrcs (:814), two DIFFERENT attributes of the one
go_test — default_rules_test.go carries `//go:embed *.go` (the rule-registration
completeness check embeds the package's own sources), so gazelle correctly
maintains EVERY .go file in both lists; the round-2 sed "dedup" was a no-op gazelle
rightly reverted, and the commit message's "deduped" claim was WRONG. FIXED — the
stale W3b-assert citation in the arity-narrowing comment (the decline itself is the
safety mechanism; the S2 drift assert died at the S3 fulcrum); the dup-alias pin
matrix now covers all three collision axes (left leg, right leg, EXISTS-vs-EXISTS);
the MintedAliasBlind pin threads a predicate REFERENCING the minted alias (the inert
filterAlias scaffold exercised nothing); a translation-level unnest pin makes the
qualified-read fix order-INDEPENDENT of cost-model internals (the e2e matrix coverage
of the swapped order was contingent on the tie-break's outcome). BOOKED —
stablePlanNodeHash's type switch covers the 8 content-bearing node types of the
observed nondeterminism class; the ~30 remaining types fold as type tags (residual
ties resolve by the DETERMINISTIC first-arrived fallback — stable across plannings
now that hash values are). Widen the switch (StreamingAggregation grouping keys,
TypeFilter type lists, AggregateIndex identity) if an EXPLAIN flip is ever observed
on those shapes, or as S4-adjacent hygiene; deepHashCode's REWRITING-path
physical-member recursion (a child ref already holding an NLJ/FlatMap member) is
currently unreachable within one top-level pass (rewritingImplRules =
FinalizeExpressionsRule only) — the deepHashCode comment names the extension site if
that changes.

## QP-REF-BIND item 1 — design substrate (banked; for the Graefe design ruling)

Research method: five-agent code trace (Java SemanticAnalyzer + quantifier minting;
Go resolution map; the 0AF00 decline trace; the constraint inventory) + a 19-shape
LIVE-Java probe (conformance/rfc173_item1_probe_test.go, temporary — deleted once its
findings are corpus-pinned). The F3 lesson applied: every Java-behaviour premise below
is live-verified against the 4.12.11.0 conformance server, none assumed.

**Java spec (source receipts + live probe).** Java has NO FROM-level duplicate-alias
check (QueryVisitor.visitTableSources → bare fragment concat; the only DUPLICATE_ALIAS
raise is SemanticAnalyzer.findCteMaybe:180-181, firing LAZILY when a LATER FROM-source
identifier lookup finds two same-named operators — live: `FROM p AS a, p AS a, a AS b`
→ 42712 "found 'A' more than once"). EVERY quantifier mints a unique
CorrelationIdentifier (Quantifier.uniqueId; a second access to the same table goes
through withNewSharedReferenceAndAlias — shared memo Reference, SECOND fresh id,
rewireQov). The SQL alias exists only as the operator name + the qualifier prefix on
output Expression identifiers (newNamedOperator → output.withQualifier). Reference
resolution (SemanticAnalyzer.resolveIdentifier:413-424) is two-pass over ALL operators'
attributes — qualified-only, then bare — and asserts the candidate count: ≥2 → 42702
`Ambiguous reference %s` (the reference AS WRITTEN, normalize-uppercased; qualified
prints `A.ID`), 0 → 42703, ambiguity checked BEFORE existence per pass; an inner-scope
ambiguity is terminal (never falls through to parents). Unqualified `SELECT *` expands
ALL forEach operators in FROM order — duplicate output names allowed, no dedup
(expandStar case 1; StructType.from does no duplicate validation). Qualified star
`a.*` and the table-row fallback (`SELECT a`) are findFirst() — LEFTMOST leg wins
silently, never ambiguous.

Live probe (all 19 shapes; schema P1(id,v)/Q1(qid)/R1(id,rv) — P1,R1 share id):

| shape | Java live |
|---|---|
| `SELECT * FROM p, q, p` | ANSWERS, cols `[ID V QID ID V]` (dup labels), full cross product |
| CTE star `FROM w, w` / derived star `AS d, AS d` | ANSWERS, `[ID ID]` |
| `SELECT a.id FROM p AS a, q AS a WHERE a.id = 2` | ANSWERS `[[2]]` (per-attribute) |
| `SELECT a.qid … WHERE a.qid = 7` (2nd leg) | ANSWERS `[[7],[7]]` |
| `SELECT a.v FROM p AS a, r AS a` (share id; v unique) ± WHERE | ANSWERS |
| `a.id` over sharing legs (SELECT or WHERE) | 42702 `Ambiguous reference A.ID` |
| bare unique / bare ambiguous (dup AND distinct aliases) | ANSWERS / 42702 `Ambiguous reference ID` |
| `SELECT a.* FROM p AS a, q AS a` | ANSWERS first leg only `[[1,10],[2,20]]` |
| `SELECT a FROM p AS a, q AS a` | ANSWERS one STRUCT column A |
| ORDER BY / GROUP BY on `a.id` over sharing legs | 42702 `Ambiguous reference A.ID` |
| `JOIN … AS a ON a.v > 0` (unique ref) | ANSWERS |
| `p AS a LEFT JOIN q AS a ON a.qid = 7` | ANSWERS |

**Go anatomy (why the predicated disjoint form declines).** The decline is front-end:
`Scope.AddSource` (semantic/scope.go:88-96) rejects the second same-alias source →
`buildSelectScope` returns a NIL resolver (logical_predicate.go:1854-1856) → reference
resolution is SILENTLY DISABLED → the WHERE stays a text-only filter →
`translateFilter` returns nil (cascades_translator.go:1933-1935) → the generic 0AF00.
The FROM-walk approximation (cascades_generator.go:4087-4137) is the correctness
backstop; the gate's dup arm (rfc173_cluster_gate.go:114-128, arityPoison) keeps the
seed name-model. The alias-keyed collision inventory that makes a naive front-end lift
UNSAFE: both legs' quantifiers bind the SAME NamedCorrelationIdentifier (translateJoin
:3990-3995), the seed's bake maps key `strings.ToUpper(leg.alias)`
(rfc173_ordinal_seed.go:246,377), the anchored RC suffixes duplicate `A.COL` fields
`_2`, mergeRows' qualified namespace is last-writer-wins, and the NLJ partition keys
predicates by correlation id (rule_implement_nested_loop_join.go:466-476) — a
front-end-resolved `QOV(A).id` over two legs claiming `A` can be PUSHED INTO THE WRONG
LEG (wrong rows). **Ordering constraint: the front-end acceptance and the back-end
binding identity must never be live separately** — any intermediate state keeps a loud
decline between them.

**Mechanism (M1–M6).**
- **M1 — scope accepts duplicates; per-attribute resolution.** AddSource stops
  rejecting duplicate aliases (Java registers freely). `ResolveQualifiedColumn`
  collects across ALL alias-matching sources at the level: >1 carrying the column →
  `AmbiguousColumnError` (new qualified rendering), exactly 1 → THAT source, alias
  matches but none carries it → ColumnNotFound, no alias match → parent chain
  (unchanged). The bare path (ResolveColumn) already collects-and-errors; duplicate
  sources feed it with no structural change. The nil-resolver swallows in
  buildSelectScope / the :3040 join-WHERE variant / buildWherePredicateForJoins
  become dead for the dup class (other failure modes keep current behaviour).
- **M2 — per-leg binding ids (the F3-ruled coexistence mechanism).** Each FROM leg
  gets a binding correlation id at ONE mint authority readable by every scope builder
  AND the logical/translator side: the FIRST leg under an alias keeps the alias
  (today's id — zero change for every non-dup query); each LATER duplicate leg mints
  `values.UniqueCorrelationIdentifier()` (the W4b/EXISTS precedent). ScopeSource
  .CorrelationName carries it → expr.Resolver emits `QOV(bindingId).col` for
  references it resolves to a dup leg; the logical leg (LogicalScan / derived / CTE
  leg) carries the same id for translateJoin's quantifier minting. All-quantifiers
  fresh ids stay REJECTED until S4+ (the F3 ruling stands).
- **M3 — gate lift + binding-keyed seed.** The dup arm flips from arityPoison to
  gate-with-binding-ids for INNER clusters: clusterLeg gains the binding id;
  legTypes / typed QOVs / OrdinalSeedLegWindows / sourceAliases key by binding name
  (fresh ids keep ordinalJoinSpans' run detection distinct for adjacent same-alias
  legs for free). Classes poisoned for OTHER reasons (LEFT/RIGHT boxed legs → item 3,
  unnest, existential riders) keep their reasons: dup-alias references in them still
  42702 at the front end when ambiguous; per-attribute-RESOLVABLE references over a
  still-unbindable class DECLINE LOUDLY (0AF00) — never wrong rows (the LEFT-box dup
  class books as a new marked divergence: Java answers, live-verified above).
- **M4 — the SELECT-* duplicate-column layout (FORK F-A, needs the ruling).**
  Shared-column dup clusters need duplicate output names end-to-end: the raw ordinal
  RC (NewRawRecordConstructorValue — duplicates verbatim, the §5 primitive) +
  positional column defs (the deriveColumnsFromJoin ordinal-top arm as template),
  scoped to GATED DUP-ALIAS clusters only. (a) IN-SLICE (recommended): no coexistence
  fork — no existing shape emits duplicate names (the W5 bare-twin unnest class and
  its S4 ruling are untouched; this regime applies only to a class that today cannot
  run at all), and the star corpus entries flip to parity. (b) S4-DEFERRAL: star
  over shared-column dups gets an explicit loud translation decline (annotation
  flips 42702→0AF00, stays marked). Disjoint-column dup stars already answer today
  (per-leg qualified keys) and keep answering either way.
- **M5 — message unification (FORK F-B).** The AmbiguousColumnError mappings emit
  Java's exact `Ambiguous reference %s` (reference as written, case-normalized) at
  all six sites (logical_predicate.go:396,1463,1880; plan_visitor.go:599,802;
  eval_map.go:57) — live-verified as Java's text for dup AND distinct aliases, bare
  AND qualified, WHERE/ORDER BY/GROUP BY. Recommended over dup-only special-casing
  (no special cases; the live corpus byte-equal net catches any over-reach).
- **M6 — quirk parity + bookings.** Qualified star over dups: match Java's leftmost-
  wins silently + pin. Table-row projection (`SELECT a`): Go lacks the row form —
  book the divergence with the live-verified Java answer. `…, a AS b` alias-as-source:
  book message/code drift (Java 42712 lazy quirk vs Go 42F01). JOIN-ON dup resolution
  rides M1 (same fragment). The FROM-walk's RFC-142 unnest-alias half
  (:4138-4169) STAYS — a different rule, genuinely FROM-level.

**Commit plan (each four-gated, pinned SHAs).**
- **c1 (dark infrastructure):** M2 mint plumbing + M3 gate/seed keying, no
  SQL-observable change (front-end still nil-resolvers dup FROMs → the FROM-walk
  42702 / text-filter declines hold). White-box pins: gate-reason flip, binding-key
  seed tests, spans/windows distinctness.
- **c2 (the flip):** M1 + M5 + retire the FROM-walk 42702 half (the walk's
  undefined-table skip dimension transfers: validateTablesAndColumns still owns
  42F01 — resolution declines on unknowable tables). Observables: disjoint_where →
  `[[2]]` (corpus flip + the FDB pin flip at rfc173_w4left_dupalias_fdb_test.go:73);
  referenced classes keep 42702 with byte-equal text, now AT RESOLUTION (parity
  entries stay green through the move); new live-verified corpus entries for every
  probe shape (bare, second-leg, ORDER BY/GROUP BY, JOIN-ON, qualified star,
  table-row booking, 42712 booking, LEFT-box booking).
- **c3 (star, per F-A):** raw RC + positional defs for gated dup clusters →
  select_star + cte_star flip to parity (or the deferral bookkeeping if F-A=b).

**Exit gates.** Corpus: dup_from_alias_disjoint_where + dup_from_alias_select_star +
dup_from_alias_cte_star flip (annotations DELETED — the drift guard forces it);
dup_from_alias_referenced_unaliased/_aliased, three_way_later_pair, cte_referenced
stay byte-equal parity; undefined_table stays 42F01; generated_aggregate stays
message-drift with its GoErrorContains re-verified. Dualwindow green (dup classes
agree across both models or carve out with a citation). The live Java harness green
including every new entry (byte-equal error text). 1M stress before/after. Task
budgets unchanged. The A4 anchored-producer inventory re-audited (item 1's
contribution: dup-alias outer clusters leave the ungated-fallback class).

**SUBSTRATE ADDENDUM — the dual-engine probe run (Go baseline; two DISCOVERED BUGS,
pre-existing on master).** Re-running the 19 shapes through BOTH engines (the probe's
Go runner twin) surfaced:
- **⚠ WRONG ROWS on master:** `SELECT v FROM p AS a, q AS a` (bare unique reference,
  DISJOINT-column duplicate) — Java `[[10],[20]]`, Go silently `[[nil],[nil]]`. The
  FROM-walk approximation passes disjoint pairs, buildSelectScope nil-resolvers the dup,
  and the bare projection reads nothing — silent NULL VALUES, not a decline. The
  "clean 0AF00, never wrong rows" claim held only for the QUALIFIED predicated form;
  the BARE predicate-free twin was the unprobed dimension. Red-first obligation of c2
  (root-cause with the fix; the corpus entry dup_from_alias_referenced_aliased pinned
  only the qualified twin).
- **⚠ Silent-nil table-row projection:** `SELECT a FROM p AS a, q AS a` — Java answers
  one STRUCT column (leftmost leg, findFirst); Go answers `A(UNKNOWN)` with nil values.
  The row form is a pre-existing Go gap ACROSS all shapes (not dup-specific); book the
  divergence with the live-verified Java answer, pin Go's current shape or fix if the
  root cause is shared with the bare-nil bug above (decide at impl).
- Baseline facts that narrow the work: qualified star over dups is ALREADY byte-equal
  parity (both engines first-leg `[[1,10],[2,20]]` — M6's star quirk needs only a pin);
  ORDER BY / GROUP BY / shared-column 42702s are ALREADY byte-equal (`Ambiguous
  reference A.ID` both sides — the FROM-walk text matches because it names alias+first
  shared column, which coincides for these shapes); the BARE ambiguity messages are the
  ones M5 fixes (`Ambiguous reference ID` vs Go's FROM-walk `Ambiguous reference A.ID`
  for dup aliases / `column reference "ID" is ambiguous` for distinct aliases — both
  become byte-equal once the check moves to the reference); JOIN-ON and LEFT-JOIN dup
  shapes decline via the ON-clause source-resolution 0AF00 (loud, correct-or-loud
  holds there); `..., a AS b` is Go 42702 vs Java 42712 — both-reject code drift, book.

## QP-REF-BIND item 1 — design ruling (Graefe, on 77d06c33a): ACK-with-amendments

**Rulings on the forks:**
1. **M1–M6 overall — ACK.** Java premises verified in source by the reviewer
   (Quantifier.uniqueId minting incl. withNewSharedReferenceAndAlias's fresh id +
   rewireQov; per-attribute 42702 ambiguity-before-existence, inner-ambiguity
   terminal). M2's dup-legs-only fresh ids is the F3-ruled coexistence mechanism;
   all-quantifiers-unique stays REJECTED until S4+. First-leg-keeps-alias zeroes the
   blast radius for non-dup queries.
2. **F-A → IN-SLICE (option a).** The bare-twin/S4 coexistence ruling is scoped to
   W5's UNNEST class (fail-open name-model goldens); the dup-alias star class cannot
   run at all today and exists only in the gated regime (the raw RC already tolerates
   positional duplicates). S4-deferral would CREATE a new 42702→0AF00 divergence —
   backwards. **Condition:** the FDB pin asserts duplicate labels AND per-position
   values (first-leg vs later-leg values differ); column metadata stays a list;
   byte-equal to Java's `[ID V QID ID V]`.
3. **F-B → UNIFY-ALL, one carve-out.** Java's resolveIdentifier asserts all use
   "Ambiguous reference %s", but lookupAlias (SemanticAnalyzer.java:515-536) uses
   "Ambiguous alias %s" — classify each of the six Go sites against its Java path
   before unifying; any SELECT-list-alias lookup site keeps/gets the alias wording.
4. **Commit plan — ACK.** c1-dark acceptable (same slice, white-box pinned, the
   item-2 commit-1 precedent). **Binding condition:** c2 lands red-first LOUD-decline
   pins for every still-poisoned dup class (LEFT-box dup, unnest dup, rider dup)
   proving a resolver-emitted QOV(bindingId) over a name-model class structurally
   fails to translate (0AF00) — never binds by name into the wrong leg. The
   never-live-separately constraint made empirical.

**Amendments (binding on impl):**
- **(a) Qualified parent-fallthrough.** Java's resolveAcrossFragments falls to the
  PARENT fragment on a zero-match pass even when the alias exists locally without
  the column (correlated shadow shape). Live-probe; align or book with citation —
  item 3's LEFT widening trips this otherwise. (Probe shapes added:
  qual_parent_fallthrough / _ctl / _inner_ambig — results banked below.)
- **(b) One mint authority, structurally carried.** The binding id is minted ONCE at
  FROM-leg registration and READ everywhere — sourceAlias/legsOfGatedJoin must read
  the carried id, never re-derive the SQL alias (outer-box legs re-collide at item 3
  otherwise). Minted ids DETERMINISTIC per query (the #17 stablePlanHash lesson) —
  an atomic-counter q$N would make two plannings of the same SQL hash differently;
  use a FROM-position-keyed deterministic form.
- **(c)** DuplicateAliasError (scope.go:91) goes dead for FROM sources — delete or
  re-scope to the surviving RFC-142 unnest arm IN c2, not later.

The ruling covered 77d06c33a; the substrate addendum (97c164e4e — the dual-probe
baseline with the two discovered master bugs) goes to the reviewer as a delta with
the amendment-(a) probe results.

**Amendment-(a) probe results (live, both engines):** Java CONFIRMS the qualified
parent-fallthrough — `SELECT p.v FROM T_P1 AS p WHERE EXISTS (SELECT 1 FROM T_Q1 AS p
WHERE p.v = 10)` ANSWERS `[[10]]` in Java (inner fragment zero-match → parent; the
inner alias p existing WITHOUT the column does not stop the walk) while Go rejects
42703 `column "V" does not exist` — a THIRD discovered divergence, ALIGNED in M1's
rewrite (zero matches at a level → parent chain; SourceNotFound only when no alias
matches anywhere; ColumnNotFound only at chain exhaustion). The inner-ambiguity-
is-terminal twin is byte-equal parity in both engines TODAY (`Ambiguous reference
P.ID`; Java does NOT fall through past a local ambiguity — M1 keeps that). The
control (distinct inner alias) is parity. Note the fallthrough fix reaches beyond
the dup class (any correlated subquery whose inner alias lacks a referenced column);
it is Java-aligned by construction and rides c2 with its own red-first pin + corpus
entries (the three probe shapes).

**Delta-ACK (Graefe, on the 97c164e4e addendum + amendment-(a) probe):** the ruling
EXTENDS — (1) both discovered bugs are c2 red-first obligations; the bare-nil
wrong-rows finding CORRECTS the substrate's "decline, never wrong rows" premise (it
was already wrong rows on master for the bare-unique disjoint class) and RAISES the
never-live-separately stakes without reordering; (2) amendment-(a) ALIGN ACK
(zero-match falls to parent, ambiguity terminal — the reach beyond the dup class is
Java-aligned by construction, red-first pin + corpus entries satisfy the condition);
(3) the position-keyed mint ACK (`q$dupN` by FROM ordinal, first occurrence keeps
alias, carried structurally, sourceAlias display-only — deterministic per query, no
atomic counter); (4) NO pull-forward — c1-dark → c2 stands. **Contingency condition:
if the slice stalls after c1 merges, land a minimal loud decline for the
bare-ref-over-dup class immediately — wrong rows may not outlive the slice
boundary.** C1 SCOPE REFINEMENT (recorded here, tightening the substrate's plan): c1
carries the mint plumbing + binding-keyed seed/gate KEYING only, with the dup
poison arm INTACT — lifting it in c1 would flip the predicate-free disjoint class
(today name-model, answering) to the ordinal seed, an observable plan change in the
"dark" commit; the lift lands in c2 with the front-end, honoring
never-live-separately strictly.

## QP-REF-BIND item 1 — commit-1 record (PR #481)

**c1 (34872539b) four-gate round 1:** architecture gate **ACK-with-conditions**
(all four ruling conditions verified: darkness holds and is pinned; single mint
authority, structurally carried incl. the demotion restore; keying byte-identical
for non-dup queries — windows need no conversion, they derive from the RC's QOVs;
the fold-stable UPPER `Q$DUPN` mint RATIFIED over the ruling's lowercase spelling —
"same identity, better spelling"; the ruling's condition-3 form note is hereby
recorded: the final mint form is `Q$DUPN`, upper). Code-quality gate **ACK** (mint
coverage exhaustive — one `sq.joins` producer, extraCrossJoins fold in pre-mint;
builder arms symmetric across both logical builders; no clusterLeg literal outside
clusterLegOf; all four pins fail under plumbing deletion). codex **CLEAN** (no
actionable correctness issues; posted on the PR). @claude best-effort (single CI
slot).

**Converged findings → FIXED on the branch (the c1 fix commit):**
- **W5 gather alias-keyed consumers** (both gates; latent nil-deref + a false
  commit-message claim): rfc173_w5_unnest_gather.go quantifier correlations,
  sourceAliases, and the span-offset lookup now read leg.binding (matching the
  seed's map keys); gatheredPlainLeg returns the LEG so the owner is matched by
  its SQL alias (a name reference) but CORRELATED by its binding. Dark in c1 (the
  gather consults the one gate authority — pinned: a minted binding does NOT
  bypass the W5 gate); the c2 lift's red-first suite activates the e2e asserts.
- **Mint forgery guard** (both gates): a QUOTED user alias can spell a mint-shaped
  name (`AS "Q$DUP1"` — the lexer admits `$`); the mint now pre-collects the FULL
  leg-key namespace and bumps deterministically with `$` suffixes (still a pure
  function of the query text). Pinned incl. the forged-later-leg and
  forged-bump-chain shapes.
- Godoc restoration (the mint doc had displaced parseFromSource's), the
  present-tense overclaim ("scope builders read it" → the logical builders carry
  it today; c2 wires the scope builders), and the lowercase `q$dupN` comment nit.

**C2 OBLIGATIONS (architecture-gate condition 1 — convert or loud-decline-pin
each class at the lift):**
- rfc173_w4b_clustered_outer.go:547, :642 — clusterPullUp correlations off
  sourceAlias (the dup-alias outer-cluster class stays ungated/poisoned until its
  own conversion; the W4b scalar dispatch declines dup clusters defensively at
  legByAlias today).
- The mint-forgery pin rides (landed early in the c1 fix commit, above).
- Red-first LOUD-decline pins for every still-poisoned dup class (LEFT-box dup,
  unnest dup, rider dup): a resolver-emitted QOV(bindingId) over a name-model
  class must structurally fail to translate (0AF00), never bind by name into the
  wrong leg (ruling condition 4, the never-live-separately constraint made
  empirical).

**c1 round 2 (the fix commit 7f0f6848e) — all gates ACK:** architecture gate
**delta-ACK, conditions discharged** — the match-by-alias/correlate-by-binding
split ruled correct ("exactly the display/binding separation the ruling demands");
the early forgery guard ruled strictly safer ("the coexistence analog of
unforgeable uniqueIds, correctly scoped"). **One NEW c2 obligation:**
gatheredPlainLeg is first-match-wins on a duplicate owner alias — dark in c1
(gate-poisoned + pinned), but at the lift either the front end provably 42702s an
ambiguous unnest-source reference BEFORE translation reaches it, or the loop
declines on a second EqualFold match — correct-or-loud, never silent first-leg.
Code-quality gate **ACK on the delta** (docs no longer oversell; the bump loop's
determinism verified — membership-only, monotone, finite; the zero-value decline
hole ruled absent: one caller, bool-checked, and empty-binding legs are already
declined by the seed). codex **CLEAN round 2** (prior comment superseded). Branch
1M stress green (23/23; baseline comparison recorded in TODO.md).

## QP-REF-BIND item 1 — c2+c3 record (PR #481, commits 5860e3454 + 4e78ef2c2)

**c2+c3 (5860e3454) — the dup-alias lift.** The front-end flip (M1 per-attribute
resolution + M5 message unification + the FROM-walk 42702 retirement) and the
SELECT-* star layout (M4/F-A in-slice) landed together (the never-live-separately
constraint made them one atomic change; the FDB test asserts both). Key mechanics:
scope accepts duplicate PLAIN aliases and resolves references per-attribute
(1→bind, 0→parent fallthrough per amendment (a), ≥2→terminal 42702); the wedge
gate keys its pairwise dup check on the BINDING correlation so binding-distinguished
dup legs enter the ordinal seed; `deriveColumnsFromJoin` derives the star columns
from the ordinal RESULT VALUE (positional, duplicate-label safe) via
`mergedRVSequenceDiverges` when the name-model leg-merge's DISPLAY sequence diverges
from the RC's authoritative FROM-order sequence (the planner may regroup same-table
dup legs; the RV is order-invariant, the leg-merge is not).

**Exit gates all green:** dup_from_alias_{disjoint_where,select_star,cte_star} flipped
to parity (Go answers `[[2]]` / `[ID V QID ID V]` / `[ID ID]`, live-verified byte-equal
to Java — the annotations deleted); the error_ambiguous_column_join RFC-082 annotation
removed (M5 made Go's bare-ambiguity message byte-equal to Java's `Ambiguous reference
NAME`); referenced/three_way_later_pair/cte_referenced stay parity; undefined_table
stays 42F01; generated_aggregate stays message-drift. Dual-window differential green
(1632 entries, 1 pre-existing carve-out). Live-Java conformance green (54/54 in the
pre-commit). 1M stress green (23/23, no regression vs the c1 baseline).

**Four-gate status: Graefe ACK, Torvalds ACK** (both on HEAD 4e78ef2c2, after the
review-response commit). Graefe IMPLEMENTATION-ACK ruled all three seams correct
(binding-keyed gate, per-attribute resolution, RV-authoritative star derivation) with
two non-blocker debts, both discharged: (a) the divergence gate is a bridge toward
RV-authoritative-always (recorded as a follow-on), (b) the same-bare/different-binding
`FROM p, p` reorder pinned. Torvalds' four findings all resolved in 4e78ef2c2: the dead
`ambiguousColumnMarker` removed; the untested cross-scope-shadow decline traced (two
mechanisms by inner-scope arity — multi-source→plan-time 42703 CorrelatedShadowError,
single-source→runtime ordinal guard), both pinned, the bare fmt.Errorf typed; the
ORDER-BY-1 tolerance gate removed; `bindingOrAlias` deduped across every scope builder.
codex + @claude remain the PR-side gauntlet.

## QP-REF-BIND item 1 — c4 record (the codex round)

codex's PR review of the c2+c3 delta (HEAD 71974de67) surfaced two findings, both
REPRODUCED against FDB and both real; the item-1 "COMPLETE" claim was premature until
this round. Both fixed, red-first (`rfc173_item1_keybinding_exists_fdb_test.go` pinned the Java
answers before the fixes; live-Java probe verified every premise on 4.12.11.0).

**P1 — dup-alias sort/group keys kept the display alias while the gated join row is
binding-keyed** (silent wrong rows: `ORDER BY a.qid DESC LIMIT 1` over `p AS a, q AS a`
returned 5-not-9 plan-dependently; `GROUP BY a.qid` grouped every row under NULL). The
dimensional gap: the item-1 suite probed projections/WHERE/star/ambiguity but never
sort/group keys through the binding-namespace swap. Fix: qualified sort keys
(`qualifyShadowedSortKeys`) and aggregate group keys (`upgradeAggregateOperands`) route
through `ResolveQualifiedProjection` — the SAME helper the projection path uses, so the
three cannot diverge — and `buildAggColumns` keys the group-key datum by the bare
`FieldValue.Field` (the name the aggregate cursor writes), not the qualified
ExplainValue.

**P2 — correlated EXISTS over an un-collapsed cross join failed four ways** (42702 on
the dup outer scope; wrong-leg ordinal misses both directions; a leg-adapter W2-breach
on the dup second leg). Bisect: the buried-reference class REGRESSED AT ITEM-2 (worked
at 86ddd85d7, died at 8c179a025) — codex saw the dup-alias symptom, the root predates
item-1. Root cause: an EXISTS whose inner WHERE references ONLY outer legs keeps that
predicate buried in its own subplan (`existsInnerCorrelation` lifts only inner↔outer
correlation predicates), and the step-2 FlatMap binds ONLY the merged correlation — the
buried QOV(leg) was unbound, and the frontier fallback evaluated it against the inner
scan's own row. Fixes: (i) `buildOuterScopeSources` became a duplicate-preserving
FROM-ordered slice carrying `bindingOrAlias` (the alias-keyed map collapsed dup legs
last-wins; nested-EXISTS shadow semantics preserved by filtered append); (ii) the gated
flatten's quantifiers/source aliases carry the BINDING correlation (`sourceBinding`),
exactly as translateJoin's gated binary arm; (iii) `rebasePlanOuterRefsOrdinal` — the
plan-tree twin of `rebasePlanBuriedRefs` — rebases buried outer-leg references inside
the existential subplan onto the merged positional row (name-model plans take
`rebasePlanBuriedRefs`), gated on the EXPRESSION-level correlation set
(`legReferencesAny`, the same authority the step-1 orientation reads) and verified
fail-closed (`planReferencesAnyBuriedAlias`; both walks + the verifier learned
`RecordQueryProjectionPlan`, whose projection values are now inspected precisely).

**P3 — the projected-EXISTS fold served NULL for a later duplicate leg's columns**
(found by pinning the fold twin of P1 — the @claude PR review independently confirmed
codex's P1/P2 with the same call chains and its trace named the fold sort seam; the pin
then exposed the deeper serve bug: `SELECT a.qid, EXISTS(…) FROM p AS a, q AS a`
returned NULL qid on every row, sort or no sort). Root cause:
`buildExistentialJoinSelect` — the fold twin of translateJoinWithExists — still named
its quantifiers and source aliases by DISPLAY alias ([A, A]), so the resolver's
binding-qualified RV references (QOV(Q$DUP1).QID) bound nothing: the implementation
arm's `mergedOuterLegAliases` deduped to [A], the rebase left the reference untouched,
and the frontier fallback served NULL. Fix: the fold speaks BINDINGS end-to-end —
`sourceBinding` for its quantifiers/source aliases and for `classifySortSource`'s
legAliases (identical to the alias for every non-duplicate leg) — the name-model merged
row distinguishes duplicate legs exactly when the leg keys are the distinct bindings
(qualified `Q$DUP1.QID` merged-row keys; the RV rebase, hidden ORDER BY columns and
mergeRows all line up). Pinned: P1_fold_order_by_dup ([9 9 7 7 5 5] + non-NULL + the
EXISTS boolean).

**Exit gates:** the 7-shape FDB pin green (P1a/P1b/fold/P2×4, exact Java rows); 6 new
corpus entries live-verified (exists_crossjoin_buried_{first,second}_leg,
dup_from_alias_exists_{first,second}_leg, dup_from_alias_order_by_second_leg parity;
dup_from_alias_order_by_first_leg pinned JavaErrorsGoCorrect — Java cannot plan the
first-leg sort, Go orders correctly); full sqldriver + embedded + cascades + query
suites green; docscheck green; dual-window green with TWO new declared-difference
carve-outs (dup_from_alias_{exists,order_by}_second_leg — a later duplicate leg's
binding-qualified read exists only in the positional model; same class as the
recursive-CTE carve-out, retired with the name model in Slice 4); SeedRunCorpus live
cross-engine green. Booked follow-on (pre-existing, NOT this slice): aggregate output
METADATA drift vs Java — group-key label `A.QID` vs `QID` on distinct-alias qualified
keys, group-key type UNKNOWN vs BIGINT over joins, `COUNT(*)` label vs Java's `_1`
(probe-verified; rows are parity).

## QP-REF-BIND item 1 — c5 record (the minted-binding loud-decline guard)

The c4 round's re-review converged on one structural finding from both directions:
the architecture gate NAK'd (a minted-binding query that narrows OFF the wedge gate
reaches the display-keyed name model and serves silent NULLs — at the c2+c3
baseline the same shapes failed LOUD, so c4's binding changes inverted
correct-or-loud within the delta), and the PR-side review independently found the
correlated-SCALAR twin (the c4 duplicate-preserving outer scope feeds BuildScalar,
whose lowering is not binding-aware: `SELECT (SELECT a.id FROM q WHERE a.id = 1)
FROM p AS a, q AS a` went loud-0A000 → silent NULL). Both reproduced red-first; a
third face surfaced while pinning (leg-independent EXISTS over a minted-binding
GATED flatten: the executor's identity-FlatMap positional pass-through is
probe-gated on baked outer references inside the exists inner — a leg-independent
inner leaves the probe negative, the outer flows as the name Datum, and the lazy
minted-binding projection upper reads NULL; the pass-through site had booked
exactly this widening).

The guard: `mintedBindingLeg` (the subtree probe for a parser-minted duplicate
binding; deliberately descends CTE/derived bodies — the over-approximation's
failure direction is a loud decline) declines LOUDLY (typed —
ErrCodeUnsupportedQuery; the scalar path's CorrelatedExistsError, surfacing
0A000 in SELECT position and wrapped 42703 in WHERE position) at every
display-keyed sink a minted-binding query
can reach — translateJoin's name-model arm, translateJoinWithExists' narrowed
arms AND its gated leg-independent-EXISTS shape, buildCorrelatedScalar. The
projected-EXISTS fold needed the inverse fix: translateProjectOverExistsFilter no
longer pre-translates a JOIN input (buildExistentialJoinSelect re-translates the
legs itself; the wasted enclosed translation tripped the new guard for a shape
the binding-keyed fold serves fine). Never wrong rows: every declared-loud shape is
pinned with a drain assert (P4a–P4f — any served row must be non-NULL, so a
future flip is observed; P4e pins the gated leg-independent-EXISTS face, P4f
the UNION face, whose per-attribute branch reference stays display-keyed and
dies loud at the executor's ordinal guard — the typed translation-time decline
is the booked upgrade), and the flip obligations are booked as the TODO rider
with per-shape exit gates (the pass-through gate widening; per-path ordinal
seeds; the binding-aware scalar lowering; the UNION branch seed). The arity-2
scope boundary of the c4 buried-reference rebase and the dup-alias unnest-owner
first-match residual are booked in the same rider.

## QP-REF-BIND item 3 — design substrate (mixed-nesting LEFT widening)

**Mission.** Clustered legs under LEFT/RIGHT boxes go ordinal: retire (S1) the
gate's clustered-leg poison (`rfc173_cluster_gate.go:180-183` — "joined-preserved
class stays name-model until item 3"), (S2) the enclosure guard
(`rfc173_cluster_gate.go:144-156` — an outer box enclosed in a name-model
parent), and (S3) `ordinalEligible`'s LEFT/RIGHT leg-ineligibility
(`rfc173_cluster_gate.go:238-240`). With items 1+2 this drives
`NewScalarSubqueryAnchoredRecord` to ZERO callers (the one production caller is
the W4b fallback at `cascades_translator.go:3704`, reached exactly when the
outer is a clustered/ungated LEFT class) and flips the LEFT-box-dup 0AF00
booked divergence to Java-parity answers (the minted-binding loud-decline at
`:4024` retires for this class because the gated arm keys legs by binding).

**Java (the spec) has NONE of the three guards.** `QueryVisitor.
wrapOperandsForOuterJoin` builds the outer-join RV at translation as a flat RC
(`leftQun.getFlowedValues()` + `rightQun.pullUpResultColumnsWithNullability
(true)`, `rewireQov` BY ORDINAL); `RewriteOuterJoinRule` dissolves uniformly —
`buildInnerSelect` folds ON-preds into an already-select null-supplying side
with no clustered-leg branch; `ImplementNestedLoopJoinRule.
planPartitionToPhysical` lowers per-quantifier, alias-keyed (null-on-empty →
DefaultOnEmpty). The class is trivial there because quantifiers always mint
unique ids and every read is positional — a buried source is just another
quantifier. Go's guards exist solely because the name-model coexistence path
keys rows by display alias.

**Why the poisons can lift NOW.** The dissolution ruling's blocker (Graefe Q3:
"ordinalizing a flattened preserved cluster to a bare concat ERASES buried
names — the innerSelect ON-pred has no span into the ordinal preserved leg")
is dissolved by items 1+2 jointly: item 1 gives every leg a structural
identity independent of display names (bindings; per-leg seed windows keyed by
binding), and item 2's executor binders bind merged outers POSITIONALLY in the
correlated FlatMap (disabled-birth binder, ordinal existential rebase,
rebasePlanOuterRefsOrdinal) — a buried source is now nameable exactly the way
Java names it: by its quantifier's window in the merged positional row.

**Mechanism sketch (for the design ruling).**
- M1 (S3 lift): a LEFT/RIGHT box becomes ordinal-ELIGIBLE as a LEG. The parent
  seed types the box by its POST-DISSOLUTION arity (the W3b drift-assert class
  is the constraint: seed windows must agree with what SelectMergeRule
  produces after RewriteOuterJoinRule dissolves the box) — the box contributes
  its preserved legs' windows plus a NULLABLE-typed null-supplying window (the
  ruling's nullability decision; Java `OuterJoinExpression`:117-125).
- M2 (S1 lift): the LEFT box with a CLUSTERED leg gates; the flattened-cluster
  ordinal seed names buried sources per-leg (binding-keyed windows — item-1
  machinery), and the dissolved inner select's spanning ON-preds rebase onto
  the merged positional row (item-2 machinery, the buried-reference rebase).
- M3 (S2 lift): enclosure retirement — an outer box that is a leg of a GATED
  parent translates fresh (the S3-fulcrum convention already exempts gated
  parents); the guard survives ONLY for name-model parents until S4.
- M4: the W4b ordinal seed fires for clustered outers → the anchored-record
  fallback arm is unreachable → decline-only, then delete with S4 (fork F-C).

**Forks for the ruling.** F-A: lift order (S3→S1→S2 smallest-blast-radius
sequence vs one atomic flip — the never-live-separately constraint from item 1
may bind here too: M1's windows and M2's spans are consumed by the same seed).
F-B: does the buried-correlated shape (`(A⋈B) LEFT JOIN C ON c→A`,
`rfc153_joined_preserved_plan_test.go` pins the name-model index-probe FlatMap)
flip in-slice, or keep the Q3 buried-eligibility narrowing with the flip riding
S4? F-C: `NewScalarSubqueryAnchoredRecord` deletion timing (decline-only in
item 3, delete in S4 — or delete in-slice).

**Red-first obligations.** The W4b clustered-dispatch pins flip (Dir 2 anchored
→ ordinal; Dir 3 decline → rows); `rfc153_joined_preserved_plan_test` flips per
F-B; the LEFT-box-dup 0AF00 → live-Java-probed parity rows; the mixed-nesting
runtime FDB matrices (the `d.id-as-e.id` wrong-source class) stay green through
every intermediate commit; dual-window + 1M stress per the standard exit gates.

## QP-REF-BIND item 3 — design ruling (banked)

**Substrate corrections (ruled):** (1) the zero-callers claim is STRICKEN —
after S1+S3 the anchored fallback still serves genuinely-ungated outers
(unnest-join, existential-ON, unminted-dup, seed-declined single-source);
`NewScalarSubqueryAnchoredRecord` deletion rides S4. (2) A FOURTH site:
`clusterArity`'s LEFT/RIGHT poison arm (`rfc173_cluster_gate.go:354-362`) —
without flipping it to `clusterArity(preserved) + 1` (the null-supplying side
contributes exactly the null-on-empty quantifier — the rules' actual
mergeability, verified in both engines), S3 is dead code.

**F-A — sequenced, three commits, cut along GATE-ARM CLASSES** (not the
substrate's three sites). Never-live-separately binds WITHIN each commit
(gate-arm flip ↔ every consumer probing the one authority: translateJoin
seed, W4b csq dispatch, existsOuterGatesFresh, ordinalEligible); partial
states BETWEEN commits fail closed to the name model through the authority.
Atomic all-three flip REJECTED. **F-B — the buried-correlated shape flips
in-slice, commit 1**; the rfc153 typed plan pins (equality-bound probe, zero
materialized NLJ) stay VERBATIM green at every intermediate HEAD — the hard
performance gate; the Q3 narrowing retires with S1, no carve-out survives.
**F-C — neither offered option**: the constructor survives with its class
narrowed to genuinely-ungated outers; the fallback gains a gated-outer
LOUD-DECLINE guard (commit 1 — the hazard is live the moment S1 lands); Dir 2
re-fixtured onto a still-ungated class; deletion in S4.

**Amendments:** (A) the fourth site flips in commit 2. (B) nullability
through the window — ordinalLegColumns wraps the null-supplying window's
field types nullable (Java pullUpResultColumnsWithNullability(true)); the
second half of ruling I3. (C) buried-leg bake windows — legTypes gains an
entry per BURIED leg of a clustered box leg (binding → window at its offset
in the box concat) so cross-leg ON conjuncts spanning buried sources bake
positionally at translation (Java's collapseLeftSideOperators + rewireQov-by-
ordinal analog); predicateLegAliases currently misclassifies them single-leg.
(D) buildClusterPullUp's join-leg decline lifts in commit 1 (recurse through
gated box legs, per-buried-leg spans, null-supplying spans nullable) — part
of S1's atomic unit. (E) clustered NULL-SUPPLYING leg (`A LEFT JOIN (B⋈C)`)
gates at commit 1 — record-level nullable concat QOV, executor null birth
spans the whole window, red-first both directions + RIGHT. (F) EXISTS
auto-widening — existsOuterGatesFresh widens at commit 1 automatically;
red-first pins for EXISTS-over-clustered-box through the below-FOD rebase;
fixing the rebase in-slice is mandatory, narrowing the probe forbidden.
(G) FULL-over-LEFT goes live at commit 2 (red-first pin incl. drain births
over the nested nullable window); FULL's own null-supplying marking stays
booked. (H) pin RE-FIXTURING not deletion — the enclosure-guard pin needs a
genuinely name-model parent fixture; the guard itself survives VERBATIM to
S4 (the "S2 lift" is a scope narrowing that falls out of commits 1+2 —
no code deletion). (I) dispatch-authority corpus + the RIGHT-variant panic
class (rule_partition_select.go:475 must never see an ordinal-seeded box).
(J) reason-string sweep rides each retiring commit; the :4024 minted-binding
decline STAYS with narrowed class.

**Commit plan:** c1 = box roots with clustered legs (S1 + amendments C/D/E/F,
the F-C guard, the pin flips incl. LEFT-box-dup live-Java parity). c2 = boxes
in leg position (S3 + fourth site + amendments A/B/G/H/I, S2 comment
narrowing). c3 = residual disposition + exit gates (amendment J, records,
dual-window, 1M stress, live-Java batch). Graefe holds ACK until after c3.

## QP-REF-BIND item 3 — c1+c2 record

**c1 (1ac0fe54f) — box roots with clustered legs (S1 class).** The gate's
single-source condition retired; every consumer in the atomic unit: amendment C
(per-buried-leg bake windows, bakeCorr — cross-leg ON conjuncts spanning buried
sources bake positionally), amendment D (buildClusterPullUp recurses gated box
legs; W4b Dir 3 buried correlation flips decline→ordinal), amendment E's serve
side (RecordType.Legs boundaries recorded by ordinalLegType, carried through
the nullable wraps — dropping them there was the last silent link — and the
dotted name-model read resolves per-leg on positional-only rows via
spanAwareRow's buried arm + PositionalRow.GetByName's dotted bridge + merged
types carrying boundaries in BOTH derivation twins), amendment F
(existsOuterGatesFresh auto-widened, pinned through the below-FOD rebase), the
F-C gated-outer loud-decline guard at the anchored fallback, the positional
merge arm's null-on-empty tripwire → scoped decline (splice-not-collapse), and
the fold's no-pre-translate fix. The rfc153 typed plan pins stayed VERBATIM
green (the hard performance gate).

**c2 (e0fcd2496) — boxes in leg position (S3 + the fourth site).**
ordinalEligible admits LEFT/RIGHT boxes as legs; clusterArity's LEFT/RIGHT arm
= clusterArity(preserved) + 1 (amendment A — the rules' actual mergeability).
The round's live catch: the RIGHT-box NAME COLLISION — a box quantifier is
named by its rightmost leaf, which for a RIGHT box is the PRESERVED leg, so the
layout-boundary lists carried a box-run entry shadowing the buried entry of the
same name; the dotted bridge first-matched the run and read the null-supplying
leg's slot (`ORDER BY d.id` over the RIGHT-normalized mixed shape silently
sorted by e.id). Per the established bake-window rule ("the box's name means
THAT LEAF"), a clustered box run emits its SUBS ONLY in every layout list —
rcOutputType, ordinalJoinSpansOf, and the values twin. classifySortSource
collects buried bindings structurally. Pins re-cut per amendment H (the
enclosure-guard pin re-fixtured onto a dup-poisoned genuinely-name-model
parent; the guard survives verbatim); amendment G pinned (FULL-over-LEFT
serves with the whole-box drain pad).

**c3 flips banked so far:** the LEFT-box-dup booked divergence SERVES
(P5 pins: first leg [10 20] through the pad; second leg exactly one NULL pad)
— the item-1 c5 loud-decline class narrows exactly as designed.

**Correction to the c1 record (review round, Torvalds T1):** the c1 bullet
claiming boundaries were carried "in BOTH derivation twins" was PREMATURE —
the values twin (`OrdinalSeedLegWindows`) skipped the sub-window whose name
equals the box run's alias, dropping the rightmost LEAF's boundary from its
merged type while the executor twin emitted it. Fixed in c4 (the skip replaced
by window REPLACEMENT per the naming rule: the box's name means its rightmost
leaf), and the cross-agreement fixture now compares merged LEGS — the
dimension whose absence let the drift ship green.

## QP-REF-BIND item 3 — c4 record

**c4 — the stranded-correlation keystone + review batch.** The three-reviewer
NAK round's common root: after a dissolved LEFT box's child select merges up
(SelectMergeRule), references to the merged-away box alias in RETAINED
quantifiers' subtrees and in parent predicates re-bound BY NAME to the
same-named pulled-up LEG (Go's rightmost-leaf box naming + the dissolution's
alias reuse make the collision structural where Java's unique mints make it
impossible). Two Java-parity fixes:

1. `Reference.GetCorrelatedTo` now filters each member's own predicate/RV
   correlations by its own quantifier aliases —
   `AbstractRelationalExpressionWithChildren.computeCorrelatedTo` verbatim
   (plus the canCorrelate disjunct on the child filter). Without it the
   dissolved box's inner select looked FREE on the alias it binds and the
   merge's translation captured the inner binding (the 42804 wrong-window
   bake).
2. SelectMergeRule translates retained subtrees SURGICALLY (Java's
   `Quantifier.translateCorrelations` analog under Go's name collisions):
   only BAKED references whose QOV carries the box's CONCAT type (arity ==
   RC arity) collapse through the merged child's RC to the exact leg
   reference; LAZY and LEG-level references stay intact and re-bind by name
   to the pulled-up leg — substituting the RC under a lazy read would
   FieldIndex first-match across the dup-bare-named concat. Positional-seed
   children route via `OrdinalSeedLegWindows`; name-model children keep the
   TranslationMap path unchanged.

Batch: T1/codex#3 (values-twin leaf-window replacement + merged-LEGS
cross-agreement, above), codex#2 (`gatedJoinLegTypes` registers buried bake
windows), codex#4 (`spanAwareRow` box-alias reads window the LEAF — the
first-match dup trap pinned white-box), T2 (`nullBorn` deleted — dead),
T3/codex#1 (`WithNullability` carries `Legs`; both hand-rolled nullable
copies collapsed onto it), G1 (the aggregate matrix over mixed-nesting
clusters — both gate-arm classes × argument residence × orientations — the
previously-panicking `SELECT c.id, COUNT(e.id) … LEFT … JOIN … GROUP BY`
shape pins green), G3 (stale gate reasons → S4). The rfc153 typed plan pins
stayed VERBATIM green; the item-2 residual + enclosure-lift FDB pins
(previously red on this branch) are green both orientations.

**c4 fix round (review verdicts on the two c4 commits).** Torvalds ACK
(five red-first claims independently revert-proven; nits applied: the
box-naming comment now scopes the alias-collision to the dissolved-LEFT
regime, the executor's malformed-Legs bounds go NOT-FOUND instead of the
silent run-wide window, the agreement helper documents its leaf-named-box
accounting assumption). codex P2: multi-accessor baked paths over a merged
box alias were left stranded by the Single() guard — both the merge
callback and the rebuild's RC arm now collapse the ROOT accessor through
the RC and fuse the suffix onto the slot's baked reference (associative
path composition; white-box pin red-first). Graefe NAK, one finding: the
G1 matrix omitted the clustered NULL-SUPPLYING × enclosed cell, and the c4
keystone had converted that cell's base-state panic into silent zero
aggregates. Root cause: datumFromSpans was a FOURTH layout site missing
the subs-only rule — it emitted run-level ALIAS.COL keys for box spans, so
a lazy qualified aggregate operand (Datum["C2.RANK"], qualified-only by
design) read a key never written. Fixed with per-sub emission (the same
rule as the other three sites); the run-level emission for leaf-named box
spans was itself writing wrong-alias keys for every non-rightmost slot.
Pinned: the inversion cell red-first (exact zeros reproduced), plus the
leaf-arg, unenclosed, NULL-group-key, and row-level-enclosed cells as
plan-shape tripwires. @claude items: the CanCorrelate retention branch
pinned (union retains a branch's free correlation past a sibling-alias
coincidence), WithNullability-Legs + computeResultType nullability pinned
Docker-independent, and the first-member rebuild documented (the original
multi-member group stays reachable through the pre-merge expression).

## Unnest-residual completion slice — design (pre-implementation)

The W5 gather (flat (N+1)-quantifier select, ruling Q1) chartered four
fail-open decline classes to this slice. Reconnaissance against Java
4.12.11.0 (`LogicalOperator.generateCorrelatedFieldAccess` +
`SemanticAnalyzer.resolveCorrelatedIdentifier`): Java resolves a lateral
unnest's owner UNIFORMLY against the in-scope quantifiers — any dotted
depth, any owner kind (base table, join output, derived table) — and hands
the resolved multi-accessor `FieldValue` to `ExplodeExpression` directly.
Go's four declines are all narrower than Java's reach; classes 3-4 are
HARD 0A000 errors today ("not yet supported"), making them parity gaps,
not extensions.

**Class 1 — box-leg owners** (`gatheredPlainLeg` declines join legs;
`FROM (a JOIN b), c, a.arr AS x`): the collection bakes through the
amendment-C window the seed's legTypes already carry for every buried
leaf — `ofOrdinal(QOV(boxBinding, boxConcat), leafOffset + arrIdx)`.
Consumers (span windows, the flat seed, predicate bakes) already speak
box-level ordinals; the ONLY change is the owner lookup widening from
plain legs to any leg with a bake window. Also lifts the sibling decline
at the gather root: a gated OUTER box as the unnest's left
(`a LEFT b, a.arr AS x`) is a legs-carrying cluster like any other — the
Explode's owner correlation targets the box quantifier, and a
null-supplying owner's NULL array explodes to zero rows exactly as Java's
Explode over a NULL collection does (verify against live Java; pin both
polarities).

**Class 2 — multi-segment paths** (`len(u.Segments) != 2` declines;
`FROM t, t.a.b AS x`): Java's model IS the fused path
(`FieldValue.ofFields` — root accessor + suffix). The collection becomes
a multi-accessor baked FieldValue: root ordinal = FieldIndex(segment 1)
in the owner window, then per-segment descent through each intermediate
RECORD type (declining any segment that is not a record field — same
loud-vs-lazy rule as every bake). The S3-W2 fuse construction
(`NewFieldValueOfOrdinal` + `WithSuffix`) is the constructor; the
executor's `resolveSpanLeaf` multi-accessor walk already consumes fused
paths. Composes with class 1 (a buried owner's multi-segment path roots
at the box window's offset).

**Class 3 — CTE/derived owners** (HARD error today;
`FROM (SELECT ...) d, d.arr AS v`): Java supports this — the derived leg
is just another quantifier. Mechanism decision needed at design review:
  (a) NAME-MODEL residual arm — teach the residual binary FlatMap path to
      resolve the owner against the translated derived leg's OUTPUT
      columns (cteColumnsScope carries them), keeping the gather out of
      it entirely (no ordinalization of name-model legs; the unnest reads
      the owner row's Datum key). Smallest change, no model mixing; the
      Explode's collection is a LAZY FieldValue over the derived leg's
      quantifier — sound by the load-bearing lazy invariant.
  (b) Ordinalize derived legs first (S4-adjacent) and route through the
      gather. Rejected for this slice: it front-runs S4's demolition
      order and mixes models exactly where the enclosure guards forbid.
  Proposal: (a), with the 0A000 error deleted and replaced by row-pinned
  behavior; classify remaining unsupported sub-shapes (e.g. a derived
  output column that is not an array) against live Java before pinning
  error parity.

**Class 4 — chained unnests** (`x.sub AS y` — the gather's second-unnest
decline and the residual's chained guard; `FROM t, t.arr AS x, x.sub AS y`):
Java nests laterally without special casing. Mechanism: recursive
composition — the FIRST unnest translates (gather or residual), and the
SECOND treats the first's output as its outer scope, exactly as the
residual already nests FlatMaps for `translateUnnestExistsFilter`. The
gather path composes iteratively: each chained Explode is one more
quantifier whose collection bakes against the PRIOR element leg's window
(the element leg's type carries the sub-array column). Scope guard: only
INNER-comma chains; an under-existential chain stays with item-2's
binders.

**Sequencing:** three commits — c1 = classes 1+2 (both are gather-side
owner-lookup/constructor widenings sharing the legTypes window plumbing;
red-first FDB row pins per class, incl. the outer-box owner polarities);
c2 = class 3 (residual-arm owner resolution, error deletion, live-Java
classification of sub-shapes); c3 = class 4 (chained composition on both
paths) + the slice's exit gates (dual-window, stress, RFC record). The
bare-twin duplicate-column decline stays booked to S4 (circularity
ruling) — NOT this slice.

**Design review rulings (Graefe, DESIGN ACK — binding conditions):**

1. Class 1 ACK. (i) an OUTER box stays ONE opaque quantifier — the Kind
   decline lifts in LEG position only; the flat inner select never gathers
   an outer box's legs. (ii) The SelectMerge/Explode stranded-collection
   hole closes IN c1: translateQuantifierCorrelations rebuilds only
   SelectExpression members today, so an Explode-ranging sibling keeps a
   dangling reference when the box leg merges — extend the retained-
   quantifier translation to Explode members (collection through the
   bakedBoxRefCallback collapse; Java: ExplodeExpression.
   translateCorrelations), red-first pinned on a query where the box leg
   actually merges. (iii) Owner lookup: FROM-order first-match by alias,
   correlate by binding/bakeCorr from the CARRIED legTypes map, dup-alias
   (Q$DUP) pin.
2. Class 2 ACK. The blocker is the CLASSIFIER, not the gather:
   unnestArrayElementType declines multi-segment paths before the gather
   ever sees them (a hard UNDEFINED_COLUMN today), and
   rotateEnclosedUnnest carries a twin decline — widen both to
   per-segment proto-descriptor descent (record intermediates only,
   Java's STRUCT rule). Bad-path error surface live-Java classified.
3. Class 3 ACK on mechanism (a). Pins mechanism-agnostic (rows + error
   parity — no name-model plan-shape pins except marked coexistence
   markers) so S4 swaps the mechanism under a green suite; array-ness
   classification runs against the TRANSLATED derived leg's flowed
   cascades type (never the UnknownType name-scope columns — the P2a
   silent-wrong class); class-3 re-absorption is BOOKED into S4's
   demolition checklist.
4. Class 4: the gather-side iterative claim as first drafted is
   WITHDRAWN — struct/message array elements map to UnknownType
   (arrayFieldElementType), so a chained bake against the prior element
   window cannot fire, and typing them RecordType collides with the
   executor's load-bearing non-record element discriminator
   (rfc173_ordinal_join.go S3-merge exclusion). c3 ships the RESIDUAL
   recursive composition (the translateUnnestExistsFilter nesting
   pattern); the gather declines chains to it, pinned as such. A
   struct-element RecordType amendment (incl. the executor-guard rework)
   is a separately designed follow-on, not a parenthetical. INNER-comma
   scope guard stands; under-existential chains stay with item-2 binders.
5. Exit-gate amendments: class-1 red-first pins are PLAN-SHAPE red (the
   class works today via the residual — row pins are born green; EXPLAIN
   must prove the gathered flat select replaces the binary FlatMap);
   white-box seed-shape pin for box-leg + element runs incl. the
   dup-alias variant; the SelectMerge-fires-on-the-box-leg pin; a
   SELECT * expansion pin for box-leg + mid-list unnestPos. Dual-window,
   1M stress, rfc153 verbatim, live-Java error classification as listed.
   Verified in review: Explode over NULL → zero rows is Java's spec
   (RecordQueryExplodePlan:139), and the seed assert is untouched by
   collection values (it walks RC fields only).

**Class-2 implementation note (pre-code, for the impl review):** the design
review verified the CONSTRUCTOR (fused root+suffix) and the executor's
span-side multi-accessor walk, but the RUNTIME leaf descent has a gap the
review did not examine: Go materializes STRUCT columns as raw
proto.Message values (query_result.go scalarProtoToGo MessageKind arm),
and values.FieldValue.descendResolvedPath has no proto arm — a fused
suffix over a struct column dies at the default arm (loud
OrdinalResolutionError for pinned paths — fail-safe, never silent). The
values package is deliberately protobuf-free today; Java's FieldValue
evaluates MessageOrBuilder natively (FieldValue.eval →
MessageHelpers.getFieldOnMessage). Proposed: port the Java behavior — a
protoreflect descent arm in descendResolvedPath (field lookup by accessor
name, case-insensitive, scalar conversion mirroring the executor's
scalarProtoToGo) — accepting the protobuf import into values as Java
parity (the no-proto purity was a Go-side layering choice, not a
contract). The alternative (wrapping struct values at row
materialization) touches every row path for one consumer. To be ruled on
at the c1 implementation review.

**Class-2 error-parity booking:** a multi-segment path through a SCALAR
intermediate (`NST.NSID.NARR`) errors 42809 WrongObjectType ("join
correlation can occur only on a column of repeated (array) type") — the
classifier's present-but-not-unnestable arm. Pinned THAT it errors, not
the wording; classify against live Java 4.12.11.0 before the slice's
exit (Java's lookupNestedField returns empty for a non-STRUCT
intermediate and the resolution falls through — the error CLASS may
differ).

**Unnest-residual c1 review NAK round (codex + Graefe + Torvalds).**
Both in-session gates and codex NAK'd; every finding fixed, each red-first
where discriminable:

- **codex P1 / Graefe #1 (silent LEFT→INNER):** gatherInnerClusterPreds ran
  UNCONDITIONALLY for the opaque outer box, re-applying the box's nested
  inner-cluster ON flat over the NULL-padded rows and dropping the preserved
  row — the ON-hoist bug's twin. Guarded with JoinInner; pinned
  TestRFC173UR_C1_OpaqueBox_NestedClusterPredsStayInside (red-proven: the
  reverted guard leaks 1 predicate).
- **Torvalds #1 / Graefe #2 (the SelectMerge/Explode arm is dead + false
  claim):** Graefe proved the arm NEVER fires in the real pipeline — box legs
  are structurally unmergeable (outer boxes opaque per the ChildrenAsSet
  guard, inner boxes pre-flattened by legsOfGatedJoin). The arm is retained
  as DEFENSIVE (if a future rewrite makes box legs mergeable it prevents the
  stranding), with the comment corrected to say so and a white-box sentinel
  (TestSelectMergeRule_TranslatesExplodeSiblingCollection, a hand-built
  FireExpressionRule memo) that red-proves it — deleting the arm now fails a
  test, closing the no-sentinel gap. The false "SelectMerge stranded the
  residual's Explode collection" mechanism claim is corrected in the c1
  commit record and the FDB test comment (the box-leg form failed to plan
  pre-slice because the residual cannot resolve an owner buried in a box, not
  because of this arm).
- **codex P2 / Graefe #3 / Torvalds #2 (shape guard lie + self-ref + no
  tests):** the one-name-one-shape guard compared field COUNT only. Now full
  api.StructType.Equal (names + indexes + recursive types), with an identity
  short-circuit so a self-referential struct is accepted rather than
  mis-flagged mid-recursion. Five new unit tests
  (metadata/struct_column_test.go: nested descriptor, struct-in-struct, dup
  same-shape accept, dup different-shape REJECT red-proven, unnamed reject).
- **Graefe #4 (Java-parity claim wrong):** the builder emits struct
  descriptors PER-TABLE nested; Java emits FILE-LEVEL template-wide. Corrected
  to a documented DIVERGENCE (wire-safe: struct bytes are placement-
  independent, and DDL can't declare struct columns).
- **Graefe #5 (proto import ruling + drift):** the proto import into values
  is accepted (Java's MessageHelpers.getFieldOnMessage is exactly this
  layer). The lockstep duplication is collapsed: values exports
  ProtoScalarKindToRowValue + ProtoFieldToRowValue, the executor's
  scalarProtoToGo / protoFieldToGo delegate — ONE conversion, no drift. UUID
  now surfaces as the neutral [16]byte in the struct-descent path too
  (matched the executor's fieldProtoToGo, not the UUID-blind scalarProtoToGo
  the "lockstep" comment mis-named). The unset-explicit-default divergence
  from Java is documented at protoFieldByName.
- **Graefe #6 (pin gaps):** the box-leg seed RC run layout is now asserted
  (TestRFC173UR_C1_BoxLegOwner_Gathers extended); SELECT * over the box-leg
  shape, the class1×class2 composition, and the enclosed-form multi-segment
  rotation are FDB-pinned.
- **Graefe #7 (silent slot-0 risk):** name-addressed struct suffix accessors
  carry a LOUD sentinel ordinal (-1) — Get(-1) fails out-of-range rather than
  silently reading slot 0 if a struct ever materialized positionally.

## Unnest-residual c2 (class 3, CTE/derived owners) — mechanism amendment

Design ruling 3(ii) required classifying the array field against "the
TRANSLATED derived leg's flowed cascades type." Research (read-only trace,
verified) shows that flowed type is `UnknownType` in Go's coexistence model —
TWO independent erasures, either fatal to the literal mechanism:

1. `fieldTypeForFD` (cascades_translator.go:225-228) collapses EVERY repeated
   (and map) field to `UnknownType` at the scan leaf, so even a plain
   base-table scan flows its array column as `UnknownType` — array-ness is
   erased before any projection. (Java keeps `Type.Array` here; this is the
   Go-specific gap.)
2. `LogicalProjectionExpression.GetResultValue` (logical_projection.go:65-67)
   returns the inner quantifier's flowed object — a bare `QuantifiedObjectValue`
   stamped `UnknownType` — NOT a `RecordConstructorValue` of the projected
   column types. So `GetResultValue().Type()` is `UnknownType` wholesale (not
   even a name-resolvable RecordType).

The real element type survives in exactly one place a passthrough can reach:
the proto DESCRIPTOR, which is the mechanism the working single-table unnest
(`unnestArrayElementType`) already uses — it ignores flowed types and reads
`resolveRecordType(table).Descriptor` directly (its own comment notes the
UnknownType collapse). The P2a hazard the ruling names is real but has a
DIFFERENT root than assumed: `findOuterScanTable(j.Left, "d")` returns the
derived ALIAS name, so `resolveRecordType("D")` hits a wrong same-named base
table or misses — because the walk deliberately does not descend into
`LogicalCTE.Body`.

**Amended mechanism (honors 3(ii)'s INTENT — no P2a silent-wrong — through the
descriptor via the body, since the flowed type is unavailable):** replace the
blanket CTE/derived decline with body-projection resolution —
1. Locate the `LogicalCTE` leg in `j.Left` whose `Name == u.Segments[0]`
   (reuse the `outerSourceIsDerivedTable` walk to return the node).
2. In `cte.Body`, map the unnest field `u.Segments[1]` to a projection slot:
   output `Aliases[i]` else source `Projections[i]`, honoring `ColumnAliases`
   for `WITH c(a,b)`.
3. If that slot is a BARE non-computed passthrough of a plain column ref:
   resolve the body's OWN base scan (`findOuterScanTable(cte.Body, source)`)
   and classify via `unnestArrayElementType(bodyBaseTable, ...)` against the
   body's descriptor — the real `*ArrayType` from the CORRECT base table.
   P2a closed structurally: resolution goes through the body's actual scan,
   never the outer alias.
4. Otherwise (computed/aggregate/scalar-renamed/set-op output — the array
   type is not a base-table passthrough): a clean typed decline, never a
   base-table guess.

**Supported (currently rejected 0A000, Java plans them — parity restored):**
passthrough `(SELECT arr FROM t) AS d, d.arr`; aliased `SELECT arr AS a … c.a`;
`WITH c(a) AS …` column-rename; passthrough with intervening WHERE/ORDER BY/
LIMIT. **Declined (Go-specific loud decline, ruling-3-permitted honest
"unsupported" over silent-wrong):** computed array output (`array_agg`, array
literal), aggregate/set-op body where the column→base mapping is not a plain
passthrough. Error parity: scalar-source (`SELECT id AS arr`) → WRONG_OBJECT_TYPE
(matches Java's "repeated type" assert); field-absent → UNDEFINED_COLUMN;
non-passthrough computed → UNSUPPORTED_QUERY (Go conservative — Java has real
types and would plan; booked as the residual the RFC-173 end-state closes when
Path-B `columnCascadesType` types flow through the projection result).

**S4 rider (booked):** the end-state fix — flow `columnCascadesType`'s real
`*ArrayType` through `LogicalProjectionExpression`'s result (a
`RecordConstructorValue` of the projected values) so the derived leg's flowed
type carries array-ness — retires the c2 body-resolution mechanism AND the
computed-output declines. Part of the S4 type-model work.

**c2 design review rulings (Graefe DESIGN ACK — binding conditions):**
1. Body-resolution MUST re-apply the derived/CTE structural guard AT THE BODY
   LEVEL — a passthrough whose source is itself a LogicalCTE (derived-over-
   derived) declines, else `findOuterScanTable(cte.Body, source)` returns the
   INNER alias and resolveRecordType hits a wrong same-named base one nesting
   level down (residual P2a). Load-bearing, not advisory.
2. The passthrough classifier is a POSITIVE WHITELIST: recognize ONLY a bare
   (non-IsComputed) passthrough over a SINGLE unambiguous base scan; decline-
   by-default on every other body shape (set-op, aggregate, multi-scan join
   with an unqualified/ambiguous source, any computed slot). Never blacklist —
   an unrecognized shape must never fall through to a base-table guess.
3. Pin (a) each supported shape → plans + correct rows; (b) each declined
   shape → the stated loud code; (c) TWO P2a regressions — outer-alias
   same-named base table AND body-level nested same-named base table — each
   asserting a loud decline / correct classification, never wrong rows (the
   dimension that let the original P2a through, plus one level deeper).
4. Non-blocking follow-up (booked): align BOTH the single-table and the
   derived scalar-source paths to INVALID_COLUMN_REFERENCE (42F10) to match
   Java's generateCorrelatedFieldAccess (currently WRONG_OBJECT_TYPE 42809 —
   pre-existing, loud, same-42-class, no silent-wrong; don't split the two).
   Also noted: full 3(ii)-literal parity needs lifting the array-collapse in
   fieldTypeForFD too (a second erasure site independent of the projection
   pass-through) — part of the S4 type-model work.

## Unnest-residual c2 (class 3) — record

Implemented per the amended mechanism + Graefe's 4 conditions. A CTE/derived
unnest owner now resolves the array field through the body's projection to a
base-table array column (`classifyDerivedUnnestArray`,
rfc173_w5_derived_unnest.go): find the owner body (inline LogicalCTE or a
cteScope WITH-CTE), map the output name to a projection slot, and — for a BARE
passthrough over a SINGLE base scan — classify via the body's own descriptor
(the correct table, never the outer alias). A positive whitelist:
`bodyOutputProject`/`singleBaseScan` recurse only through row-shape-preserving
unary wrappers and decline-by-default on any join/aggregate/union/CTE
(condition 2); the `outerSourceIsCTE(scan.Table)` check declines nested
derived-over-derived (condition 1).

Dispositions (plan-level pinned, TestRFC173DerivedUnnest_Dispositions;
row-pinned, TestFDB_ArrayUnnestOrdinality class-3 block): passthrough / alias-
rename / CTE / intervening-WHERE PLAN (were 0A000 — Java parity restored);
scalar-source → 42809 WRONG_OBJECT_TYPE (Java's "repeated type"; the
pre-existing single-table path's code — condition 4's 42F10 alignment is the
booked non-blocking follow-up); computed/aggregate/set-op/multi-scan/nested →
0AF00 loud UNSUPPORTED_QUERY (honest over silent-wrong); absent → 42703. The
TWO P2a regressions (condition 3) are pinned both plan-level and are the
load-bearing cases — outer-alias same-named base table → 42809 via the body
(never base d.arr), body-level nested same-named base → 0AF00 (never base
inr) — no silent-wrong at either level. Class-3's more-precise scalar-source
disposition (42809 vs the old blanket 0AF00) re-fixtured 5 stale single-table
rejection pins to the codes the plan-level pins codify. Red-proven: the
pre-c2 tree returns 0A000 "not yet supported" for every supported shape.

## Unnest-residual c3 (class 4, chained unnests) — mechanism design

Research (read-only trace, verified) refines ruling 4's residual composition.
The real blocker is NOT the sibling guard (containsLateralUnnest, which fires
for `t.arr, t.arr2` — SAME table, stays rejected) but OWNER RESOLUTION: a
chained `FROM t, t.arr AS x, x.sub AS y` has the second unnest's owner `x` =
the FIRST unnest's element, so seg0=`X` is not a scan → findOuterScanTable
returns "" → unnestFallbackOrReject rebuilds with Scan("X.SUB") →
table-not-found. Class 4 fixes this path.

**Mechanism (residual recursive composition, gather declines):**
- The nested-FlatMap shape is ALREADY wired: translateUnnestJoin builds its
  outer from `translateRef(j.Left)`, which recurses through the left-deep join
  tree — so `SelectExpr_2(outer=SelectExpr_1(outer=Scan(t)))` falls out with no
  new nesting code. The EXISTS precedent (translateUnnestExistsFilter) is the
  template.
- The ONE new value-tree: the second unnest's collection is a MULTI-ACCESSOR
  FieldValue rooted at the owner alias + suffix — `FieldValue{Field:"SUB",
  Child:QOV("X"), Resolved:[{X,-1},{SUB,-1}]}` (root reads Datum["X"] = the
  element proto message; suffix "SUB" descends by name). Structurally the
  class-2 multi-segment construction, but that currently only builds for
  len(Segments)>2; a 2-chain is len==2 — generalize the constructor to run for
  a chained owner.
- RUNTIME: reuse class-2 struct-descent VERBATIM. A struct-array element flows
  as a raw proto.Message; descendResolvedPath's proto arm descends "SUB" by
  name (protoFieldByName), a repeated sub → []any the Explode iterates. NO
  executor change. Caveat: the root read must go through the name-keyed Datum
  path (Datum["X"] present via buildUnnestResultValue's addField), never the
  evaluateCorrelated raw-object arm.
- CLASSIFICATION: new recursive descriptor walk. arrayFieldElementType
  collapses message-array elements to UnknownType and outerTable=="" for a
  chained owner, so the existing classifier can't type x.sub. New:
  resolveChainedOwnerElementDescriptor — find the owner LogicalUnnest
  (findOwnerUnnest, Alias/AtAlias match, mirroring FindOuterScanTable),
  recursively resolve ITS owner's element message descriptor (base = scan →
  resolveRecordType.Descriptor; step = prior unnest → walk its array field to
  .Message()), then look up segments[1:] on it (reusing unnestArrayElementType's
  per-segment loop), final must be repeated. The descriptor is the only place
  the real type survives Go's UnknownType collapse (same finding as class 3).

**Guard changes:** (1) add the owner-unnest dispatch BEFORE the outerTable==""
fallback (cascades_translator.go:1096), gated `!t.unnestUnderExistential`; (2)
keep containsLateralUnnest for SIBLINGS (seg0 is a scan → never enters the
chained branch); (3) INNER-comma scope only — an under-existential chain keeps
item-2's binders (ruling 4 scope); (4) the gather naturally declines a chain
(legTypes[X] has no plain-leg window → ownerWindow.leafTyp==nil) → residual;
pin the plan-shape.

**N-chain (3+): fully recursive**, no 2-cap — nesting via translateRef
recursion, classification via the recursive descriptor walk, runtime via each
level binding its element under its alias.

**HIGHEST RISK — SelectMerge/name-reuse (mirrors the c4 stranded-correlation
keystone):** alias `X` is reused for SelectExpr_1's inner Explode quantifier
AND SelectExpr_2's outer quantifier. SelectMergeRule may try to merge
SelectExpr_1 up, materializing the correlated Explode1 against an unbound
context (the failure translateUnnestExistsFilter guards against) and/or
colliding the two X bindings. The chained composition must keep the inner
unnest UN-MERGED (EXISTS precedent) or Q$DUP-mint to disambiguate. RED-FIRST
white-box pin on a query where the merge would otherwise fire.

**Error parity:** scalar-element owner (`t.intarr AS x, x.sub`) → x is scalar,
no field sub → UNDEFINED_COLUMN 42703 (classify vs live Java 4.12.11.0 —
Java's resolveIdentifier miss; the RFC repeatedly books this ambiguity).
Struct-element non-array sub (`x.scalarfield AS y`) → present-but-scalar →
WRONG_OBJECT_TYPE 42809 (the single-table twin's code; 42F10 alignment booked
condition 4). Both loud, never silent-wrong.

**Pin matrix:** 2-chain struct→sub-array (red-first: today table-not-found);
3-chain (X/Y/Z name-reuse); WITH ORDINALITY each level; scalar-element →42703;
struct-non-array-sub →42809; sibling control → still UNSUPPORTED; gather
decline (residual not gathered); SelectMerge white-box (inner un-merged,
revert-proven); under-existential chain stays item-2 binders (scope pin); NULL
owner sub-array → zero rows; SELECT * over a 2-chain. Standard exit gates:
dual-window, 1M stress, rfc153 verbatim, live-Java classification.

**c3 design review rulings (Graefe DESIGN ACK — binding conditions):**
1. SelectMerge BARRIER mandatory — the "EXISTS precedent" does NOT transfer
   (SelectMergeRule's existential-skip protects EXISTS; a chained INNER-comma
   outer is a plain ForEach with no such guard, so the rule fires and strands
   the retained Explode's collection — PR#201 class). Add a correlation-aware
   barrier: decline to merge a ForEach target when a RETAINED sibling is
   free-correlated to it AND the target's child result is NAME-MODEL
   (positional-seed false, so the working Explode-rebase arm won't run). Keep
   the positional-seed rebase path unblocked. NOT Q$DUP alone. RED-first
   white-box pin on a hand-built memo forcing the merge (no broken alternative
   yielded + correct rows), revert-proven.
2. Gate the dispatch POSITIVELY on findOwnerUnnest(j.Left, seg0) matching a
   LogicalUnnest by Alias/AtAlias — never merely outerTable=="" (which also
   covers schema-qualified + derived-hidden); else fall through to
   unnestFallbackOrReject. Keep !unnestUnderExistential.
3. Build the chained accessor layout [{seg0,-1},{seg1,-1},…] with
   Child=QOV(sourceAlias(j.Left)); do NOT just lower the len(Segments)>2
   threshold (class-2's root is seg1; the chained root is seg0).
4. resolveChainedOwnerElementDescriptor bottoms out at a REAL-TABLE scan; a
   chain rooted at a CTE/derived/box owner declines LOUDLY (class-3×/class-1×
   composition is a separate follow-on), pinned as a loud decline.
5. Scalar-element owner → UNDEFINED_COLUMN 42703 (Java-verified:
   lookupNestedField soft-returns empty for a non-struct base → resolveIdentifier
   miss; the "live-Java" question is CLOSED). Struct-present-but-scalar sub →
   Java's INVALID_COLUMN_REFERENCE 42F10 (the array assert). ALIGN twin +
   chained TOGETHER to 42F10 NOW (one constant, message already matches) —
   never split the two; the single-table path (cascades_translator.go:1178)
   and the c2 derived scalar path flip too. Condition-4's 42F10 booking is
   thereby discharged.
6. Pin the gather-decline on the actual gates (findOuterScanTable=="" /
   containsLateralUnnest / plainLegs<2), not the "legTypes window" text.
7. Pin WITH ORDINALITY at each chain level (element-under-key is the proto
   message under ordinality) + a key-collision/shadow-precedence pin where the
   second element's bare name overlaps a first-level surfaced column.

**c3 IMPLEMENTED — all 7 conditions discharged (pending gate re-ACK on HEAD):**
The chained residual lowers as nested FlatMap-over-FlatMap (translateRef recurses
the left-deep join tree; the chained link's collection is a multi-accessor
FieldValue `[{seg0,-1},{seg1,-1}]` rooted at `QOV(sourceAlias(j.Left))`).
Two REAL bugs surfaced by the FDB e2e and fixed inline:
- **3+-link chain collapse:** a chained unnest's `translateUnnestJoin` flips
  `t.inInnerCluster=true` (its name-model enclosure bit) and holds it (defer)
  across the OUTER translation; a nested chained link then observed
  `prevEnclosure=true` and — under the old `!prevEnclosure` dispatch gate —
  declined its own chained dispatch, so the outer's `translateRef` returned nil
  and the whole chain 0AF00'd. Fix: the chained dispatch is gated on
  `isChainedUnnest` ALONE (drop `!prevEnclosure`) — a chained unnest is always
  name-model residual regardless of enclosure, and a base-table link nested in
  the outer still relies on the enclosure bit to stay name-model.
- **AT-on-chained-owner false reject:** the early `atOnNonArraySource` pass read
  a chained owner (`x.sub AS y AT o`, segment 0 = a prior unnest element) as a
  bare table source (`findOuterScanTable==""`) and raised 42809. Fix: recognize
  `FindOwnerUnnest(left, seg0) != nil` before the reject and leave it to the
  translator's per-case disposition (the translator's chained path supports AT).

Conditions → pins (`rfc173_w5_chained_unnest_fdb_test.go` unless noted):
(1) barrier — `TestSelectMergeRule_ChainedUnnestBarrier` (cascades, white-box,
RED-first revert-proven; barrier scoped to a free-correlated retained EXPLODE
sibling so a legit correlated SelectExpression-sibling merge still fires);
(2) positive `findOwnerUnnest` gate + `!unnestUnderExistential`; (3) accessor
layout via `chainedUnnestCollection`; (4) CTE-rooted chain → 0AF00 loud decline;
(5) scalar-element → 42703, present-scalar sub → 42F10 (twin at
array_unnest_ordinality_fdb_test.go:1370); (6) gather-decline — nested-FlatMap
plan-shape assert (`FlatMap(outer=FlatMap(` + exactly 2 Explode legs);
(7) WITH ORDINALITY inner + BOTH levels (independent ordinals) + shadow-
precedence (top-level `SUB` column shadowed by the element's `x.SUB`). Plus
3-chain rows, empty/NULL owner → zero rows, SELECT * columns.

## S4 CAP SCOPING — empirical (feat/rfc173-s4, experiment reverted)

Ran the join-name-model gate change (skip the three join declines :143/:157/:190) over the FULL
54-target suite. Result: **50 pass / 4 fail**, and EVERY FDB failure is a RECURSIVE CTE:
- sqldriver: TestFDB_RecursiveCTERename, RecursiveCTECrossJoin, RecursiveCTERekeyGate,
  CascadesRecursiveCTE(PostOrder), RecursiveCTEHierarchy, RecursiveCTEBasic,
  RFC130_RecursiveCTE(NoDoubleCharge/CrossLevel).
- dualwindow: recursive_cte_{tree_descendants,filtered_branch,linked_list,multi_column,
  subtree_from_internal_node,bounded_chain,depth_bounded_walk,wide_tree_count} — name-vs-ordinal
  divergence.
- conformance: recursive_cte_count errored + a stale RFC-082 golden (behavior-shift, updatable).
- query_test: the unit gate-decision pins (TestRFC173Item2C3_FlattenGateArm, WedgeGate) — EXPECTED
  (they assert the OLD decision; update with the change).

**Every NON-recursive-CTE join shape passes under this crude blanket-skip — BUT that is a coverage
artifact, not a license** (Graefe ruling af259dec): the FDB corpus doesn't stress "inner cluster
directly under an existential/unnest flatten," so the blanket-skip's over-lift of those residuals
went unpunished. The recursive-CTE break is real: `translateRecursiveCTE` blankets its whole
lowering with `t.inInnerCluster = true` (cascades_translator.go:4953-4955); its name-keyed
temp-table / `recursiveRemapValues` machinery reads by NAME, so ordinalizing interior joins
mis-reads them.

### GRAEFE RULING — (A), with a premise correction and the CORRECT gate condition

**Premise correction:** `NewReEnumerationAnchoredRecord` is the GENERAL N-way join partition
re-enumeration producer (PartitionSelectRule, rule_partition_select.go:469/567), NOT the recursive
frontier. Recursive-CTE machinery has ZERO trio references — it is firewalled from the ordinal join
machine by the `FrontierPinned` bit (values.go:411). Java has no AnchoredJoin at all (Go-only
bridge). So **(B) as posed is a phantom**: no single commit deletes the AnchoredJoin trio by
ordinalizing the recursive frontier, because the trio has THREE independent name-model blockers —
(1) recursive-body joins (4954 → NewAnchoredJoinRecord :698), (2) multi-source lateral unnest
(:1528), (3) correlated-scalar-subquery projection (NewScalarSubqueryAnchoredRecord :3860) — gated
by :106/:112/:401, none in the {:143,:157,:190} skip set. The trio dies at the CONVERGENCE of
R1∧R2∧R3, not at the recursive one.

**RULE: (A) — the pre-cap join-ordinalization slice — as the next stacked PR.** Correct gate
condition is a DISTINCT name-model-enclosure bit (NOT the blanket skip, which fails the existential
residual):
- `:143` unchanged (`ordinalEligible` already recurses with enclosure forced false → name-model
  constructs decline, plain cluster legs are eligible; the circular chain self-resolves once
  parents gate).
- `:190`/`:157` read a new `inNameModelEnclosure` bit set (defer-scoped) at the GENUINE name-model
  sites — :1058 (unnest), :2833/:2950/:3751 (existential), :2344 (existential cond), :4399 (flatten
  cond), :4954 (recursive) — NOT `inInnerCluster`. The circular site :4151 leaves the bit unchanged
  → inherits the ancestor's name-model-ness → circular class gates, genuine residuals stay
  name-model. DELETE `:157` (dead since item 3).
- Net residual after (A): {recursive-CTE, existential, lateral unnest, correlated-scalar,
  dup-binding}. Everything else gates.

**ACCEPTANCE GATE (hard, before impl-ACK):** (i) the two contested pins stay GREEN —
`TestRFC173S2_WedgeGate_Translation/derived_join_leg_under_exists_filter_enclosed` and
`TestRFC173Item2C3_FlattenGateArm/"enclosed flatten declines"` (they pin the existential residual;
a blanket skip that fails them = automatic NAK); (ii) the duplicate-bare-name IDENTITY pin —
`FieldValue` node identity is still name-based (map_field_values.go:260-262; semantic_hash.go:108),
so once dup bare names coexist positionally they can conflate into one memo member → wrong plans;
prove distinct memo members before shipping the SELECT-* win.

**Gate (ii) mechanism — CHARACTERIZED (S4 investigation, current line refs):** `writeSemanticHash`
for a `*FieldValue` (semantic_hash.go:120-139) is a HARD FORK on `Resolved`:
  - `Resolved != nil` (BAKED): identity is the ORDINAL PATH alone — `fieldpath:#<ord0>#<ord1>…`
    (the display Field name is NOT folded in). Two references to duplicate bare "X" that resolve to
    DIFFERENT slots hash+compare DISTINCT → separate memo members. SAFE.
  - `Resolved == nil` (LAZY): identity is the NAME BUCKET — `field:<Field>`. Two lazy references to
    bare "X" CONFLATE regardless of which leg they mean → one memo member → wrong plans. THE RISK.
The equality arm (map_field_values.go:314-337) mirrors this: baked → `Resolved.Equals` (ordinal),
lazy → name. So gate (ii) is NOT an open blocker — it reduces to a REQUIREMENT: the join
ordinalization must emit dup-bare-name references BAKED (ordinal-resolved), exactly as producer #2's
positional wrap does (`ofOrdinal(QOV, slot)` carries a Resolved ordinal path). A lazy name-only
`FieldValue{Field:"X"}` for a shadowed join column is the ONLY way to trip the conflation, so the
impl gate is "no lazy dup-bare-name node survives to the memo" — a checkable structural property, not
an unsolved memo-theory problem. The proof test: build the join-ordinalized RC for `FROM a, b` where
both have column X, assert the two X references carry distinct `Resolved` ordinal paths (hence
distinct `SemanticHashCode`), and an EXPLAIN of `SELECT * FROM a, b` shows both X columns.
**Additional constraint (found + pinned, `TestRFC173S4_DupBareNameMemoIdentity`):** `NewRecordType`
PANICS on a duplicate field name, so the ordinal join SEED TYPE cannot carry two bare "X" fields —
the seed must name dup slots DISTINCTLY (qualified, e.g. `A.X`/`B.X`) while the ordinal references
bind by slot. This is a hard reason Rule A must qualify-then-ordinalize rather than flow two bare
"X" names; the test pins baked-distinct / lazy-conflate identity as the regression sentinel for the
gate.

**After (A):** three independent residual slices (none gates another) — R1 recursive-CTE
ordinalization (lift the 4954 blanket, positional temp-table norm; Java already consumes the ordinal
model), R2 multi-source lateral unnest seed (:1528), R3 correlated-scalar-subquery seed (:3860).
**Trio demolition = the convergence commit after R1∧R2∧R3** — that, not "B," is the real atomic cap;
(A) is its load-bearing first stack.

**STATE UPDATE (S4 census, current HEAD):** ALL THREE residual slices have LANDED — R1 recursive
(:5060 comment "the recursive-CTE body is ORDINALIZED… lifting it surgically"), R2 gathered
multi-source unnest (:1528, fully retired this session across plain/box/N-way/shadow/AT-only/global-
operand, double four-gate-reviewed), R3 correlated-scalar (:3908-3977 "the name-model
NewScalarSubqueryAnchoredRecord fallback is RETIRED… the ordinal seed is the sole surviving
correlated-scalar seed; a decline loud-declines"). So the trio's THREE residual producers are dead.
What REMAINS live is NOT a fourth residual — it is the GENERAL binary-join ANCHORED branch:
`buildJoinResultValue` (:714) called at :4402 / :4650, the `!gated` arm that fires for the
NON-enclosure residual join classes (dup-binding, leg-contains-name-model-join, arity<2). The common
GATED join already takes the ordinal branch; this anchored arm is the last name-model join producer,
and it is EXACTLY the site Graefe's refinement 1 flags — ordinalizing it must first prove the
ordinal-leg-under-anchored-parent composition (or narrow it class-by-class producer-#2-style:
dup-binding first, its own executor pass-through already widened in S4 commit 4). The `:334`/`:1552`
AnchoredJoinRecord sites are the unnest/derived residual builders that back the loud declines. Net:
the atomic cap (task #16) is gated on retiring the :4402/:4650 anchored-residual arm, not on R1/R2/R3
(done). Producer #4 (`NewReEnumerationAnchoredRecord`, rule_partition_select.go:469) is NOT independent
(census correction): it RE-STAMPS a parent source-anchored join RC over the upper's immediate
quantifiers during PartitionSelectRule re-enumeration, firing ONLY when `isAnchoredJoinResult(parent)`
is true (`:435`, `parentIsMerge`). So it is the re-enumeration counterpart of producer #3's anchored
RC — once #3's join seed is ordinal, the parent is no longer an anchored merge and this re-stamp path
does not fire; PartitionSelectRule would instead need an ORDINAL re-enumeration path. #3 and #4 are
the two faces of the SAME AnchoredJoin machinery (seed + re-enumeration); they retire together, gated
on the name-model leg classes.

### DESIGN REFINEMENT (Graefe design review of Rule A, ACK-WITH-REFINEMENT) — S4
Gate (ii) ✓ (correct + pinned by `TestRFC173S4_DupBareNameMemoIdentity`); bit-over-blanket-skip ✓.
But (A) as a global GATE-CONDITION SWAP is NOT a clean shippable slice — three blocking refinements
before impl-ACK, and a re-scope to the proven per-site cadence:

1. **The anchored `else` branch is a GENUINE name-model producer, not a "circular site."** At
   `cascades_translator.go:4291` (join seed) and `:4650` (flatten builder) the `!gated` branch calls
   `buildJoinResultValue → NewAnchoredJoinRecord`. A join reaches it for NON-enclosure reasons
   (dup-binding, leg-contains-name-model-join, arity<2) and legitimately encloses its legs. So the
   RFC's "leave the bit unchanged at the circular site → inherits ancestor name-model-ness"
   UNDER-approximates: a plain-gateable join that is a leg of a `4291`-anchored residual would flip
   to ordinal UNDER a name-model-anchored parent — the exact MIRROR of the `:154`/`ordinalEligible`
   decline (proven wrong-answer hazard), and the reverse composition (anchored RC reading a
   positional leg by name) is NOWHERE proven safe. Reachable bare-bushy shape:
   `FROM a, a.arr AS u, (c JOIN d)` → `((a⋈u)⋈(c⋈d))`, the `(a⋈u)` leg ineligible so the top anchors,
   `(c⋈d)` the plain right leg that would flip. FIX: set `inNameModelEnclosure` in the anchored
   branch too — BUT then `inNameModelEnclosure ≡ inInnerCluster` for every reachable chain, so the
   gate swap changes NO live query (a no-op refactor). Hence the re-scope below.
2. **Extend the gate-(ii) proof to an EXTERNAL-reference path.** Seed-field baking controls the
   seed's own fields, not WHERE/GROUP BY/ORDER BY/correlated-outer references INTO the ordinalized
   join — the resolver can emit those lazy. Add a dup-column-join WHERE/GROUP BY pin (the surface
   producer #2 needed the named-projection wrap for), not just `SELECT *`.
3. **Migrate the flatten pin + add a real-lowering companion.** `TestRFC173Item2C3_FlattenGateArm/
   "enclosed flatten declines"` manually pokes `inInnerCluster = true`; under the arm-repoint it must
   read `inNameModelEnclosure` or it flips red. Migrate the poke AND add an EXISTS-over-inner-join
   under a real outer name-model parent so the existential residual is pinned through the PRODUCTION
   path, not a hand-set flag.

**RE-SCOPE — smallest safe first increment is the next GENUINE-SITE LIFT, producer-#2-style, NOT the
gate refactor.** R1 (recursive, :5060) ordinalized its body by surgically NOT setting `inInnerCluster`
at that one site — no new bit — and R2 retired producer #2 one narrow class at a time, each
four-gate-clean. Follow that: land **R3 correlated-scalar (`:3850`)** — one `t.inInnerCluster = true`,
one residual class (already flagged "RETIRED in S4 (R3)") — lifted surgically, and pin (a) rows
correct, (b) dup-bare-name memo identity on that shape, (c) EXPLAIN shows the ordinal join, (d) the
two existential pins + the anchored-residual composition (refinement 1) stay green. Introduce the
`inNameModelEnclosure` bit ONLY when a concrete site can't be lifted surgically the way R1 was — with
a failing shape driving it, not a speculative global swap. Stale line refs to refresh against HEAD:
`:1058/:2833/:2950/:3751/:2344/:4399/:4954/:4151` → `:1074/:2908/:3033/:3850/:4539/:5060/:4291`; R1
(recursive) has already LANDED, so the "After (A)" list above is stale on that item.

**MECHANISM confirming refinement 1 — why the composition is FUNDAMENTALLY unsafe (S4 deep-read).** The
anchored branch fires when `ordinalWedgeGate` returns `!Gated`, and (per `ordinalEligible`,
rfc173_cluster_gate.go:240-265) a join is non-gated precisely when a LEG cannot be ordinal-typed — a
NAME-MODEL leg (name-model unnest, dissolved-LEFT cluster, mixed derived nesting), a per-row
correlated-scalar project, or a recursive body. `NewAnchoredJoinRecord` reads its legs BY ALIAS
(`FieldValue(QOV(leg), col)` keyed on the display alias). So if Rule A flipped a gateable sibling leg
to ORDINAL under such an anchored parent, the parent's alias-keyed read would hit that leg's
POSITIONAL row by NAME and silent-NULL — the exact positional-seed-name-read defect producer #2's
named-projection wrap exists to cure. That is why the mix is unsafe and why the anchored branch is not
a "lift": it is the IRREDUCIBLE name-model residual for joins at least one of whose legs is itself a
name-model construct. Retiring it therefore does NOT reduce to a gate tweak — it requires ordinalizing
the LEG CLASSES that make a join non-gateable (name-model unnest legs beyond the gathered case,
dissolved-LEFT clusters), OR wrapping the anchored parent's ordinal legs in the same named-projection
layer producer #2 uses so the alias read resolves. Net: producer #3 is co-extensive with "ordinalize
every remaining name-model LEG class," gated on the same wrap/qualify discipline R2 proved — a
multi-slice effort, not a single lift. The atomic cap fires only once no SQL shape reaches :4402/:4650
with a name-model leg.

**ENDGAME STATE (2026-07-08, after the `:3234` + `:4051` retirements).** The UNCONDITIONAL `inInnerCluster = true` producers are now down to ONE hard blocker: `:1151` (`translateUnnestJoin`, the `:1074` non-gathered unnest producer), plus `:2083` (`translateOp`'s opaque-box save/restore) which is a RESTORE that only fires when the flag is already set — it DIES-WITH `:1151` (once no producer sets the flag, the `if t.inInnerCluster` guard never enters). The other `inInnerCluster` writes are the RFC-173-correct CONDITIONAL tree-property forms (`:3113`/`:3119` = `!existsLegBirthsPositional`, `:4515` = `!gateDecision.Gated && …`, `:4763` = `!gatedFlatten`) — not producers to retire. So the atomic cap (task #16) is now singularly gated on `:1151`, which is BLOCKED on the compose direction (the name-ambiguous bare-twin: `FROM A, B, A.arr AS x` with shared leg column names). Retired this session: `:2908` (conditionalized earlier), `:3033`=`:3234` (clean lift), `:3850`=`:4051` (outer-inherit + inner-fresh). See the LANDED notes below.

**`:1151` de-risk CONFIRMS it is genuinely LOAD-BEARING (NOT a clean lift like `:3234`/`:4051`).** Clearing it (`= false` probe) STRANDS the CHAINED unnest: `SELECT ID, Y FROM T4, T4.SARR AS X, X.SUB AS Y` (and the AT-ordinal variants) fails `field "T4.ID" not resolvable in the runtime row (ordinal -1, row columns [X]) — malformed plan` (TestFDB_RFC173ChainedUnnest). Without the enclosure the chained/non-gathered unnest legs gate ORDINAL under a name-model unnest builder whose ordinal seed does NOT carry the outer's own columns (`T4.ID`) → the outer projection reads an ordinal that isn't in the runtime row → malformed plan. So retiring `:1151` is NOT a producer-lift; it is the actual **compose-direction slice**: ordinalize the non-gathered/chained unnest SEED so it CARRIES the outer columns (the R2 gathered path already does this for the multi-source + aggregate cases; the residual is the chained-element unnest `X.SUB AS Y` and the name-ambiguous bare-twin). That is a substantial producer-#2-style slice (translator seed change + windowing), not the incremental setter-clear the other two were — the honest reason the atomic cap cannot fire yet.

**`:1151` SLICE 1 LANDED (2026-07-08) — the CHAINED-unnest ordinal seed (a producer CONVERSION, not the setter clear).** `translateChainedUnnestJoin` now gives the chained lateral unnest (`FROM t, t.arr AS x, x.sub AS y`) its own ORDINAL result-value seed when the FIRST link ordinalizes, replacing the name-model `buildUnnestResultValue`. A THIN composition of two existing builders (per the design steer): the result value is `unnestOrdinalSeed(j.Left, …)` (its outer positional run carries the first link's merged `[outer cols … element]` row positionally — a new `ordinalLegColumns` unnest-right-join arm supplies that leg type, `ordinalLegColumns(o.Left) ++ legColumns(unnest)`, guarded so the panic tripwire for genuine name-model joins is preserved); the collection is `unnestBakedRootCollection` generalized with a `rootSegmentIndex` param (0 = the chained OWNER-ALIAS root `x`, 1 = the existing single-source table-struct root, byte-identical). `SelectMergeRule`'s chained-unnest barrier was extended (`childRefIsPositionalUnnestSelect`) to keep an ORDINAL first link NESTED too (the positional-seed rebase cannot yet compose the retained Explode sibling's fused ofOrdinal+name-suffix collection — the deferred compose direction), narrower than a blanket positional-child check (a gated box still merges). The enclosure is cleared ONLY for the first link's own `translateRef`, and the seed/collection are built BEFORE that clear so a decline never strands an ordinal first link under the name-model builder. FAIL-OPEN: the bare-twin / CTE-rooted / 3+-link / enclosed / **any-ancestor-FILTER** cases keep the name-model residual. Certificate `sqldriver/rfc173_s4_chained_unnest_ordinal_fdb_test.go` (2-chain + AT-both-links + SELECT-star labels from the ordinal RC + shadow-column precedence + root-shadow (first-unnest-alias/outer-column collision) + outer-column-WHERE + element-WHERE + 3-link fail-open decline). Ordinal path FIRES for the UNFILTERED chained shape (independently verified — a name-model-fallback panic probe never trips for the unfiltered 2-chain; and a success-side panic probe fires for the unfiltered 2-chain but NOT for the filtered outer-WHERE shape). Two review-surfaced corrections folded (both real, both dimensional test-gaps the multi-gate caught): (1) codex P2 — the collection rooted by NAME (`FieldIndex(Segments[0])`) picked an OUTER column SHADOWING the owner alias; fixed to root at the ELEMENT's explicit slot (`len(ordinalLegColumns(firstBase))`). (2) codex P1 — an ordinal chained unnest under an OUTER-COLUMN WHERE (`… x.sub AS y WHERE t.id=1`) was `field "T.ID" not resolvable — malformed plan`, because the barrier that keeps the ordinal first link nested (to preserve the chained Explode) hides the outer columns from the ancestor filter's rebase; fixed by a DOWNWARD `chainedUnnestUnderFilter` suppress bit (set in translateFilter, read once in the chained ordinal gate) that declines to name-model under ANY ancestor filter — the name-model qualified name keys resolve the predicate. Carrying the outer-column predicate across the barrier positionally IS the Slice 2 compose direction.

RESIDENCY (honest): this narrows `:1151` for the UNFILTERED chained shape ONLY; the FILTERED chained case joins {bare-twin, Q5-residual} as Slice 2 scope. It does NOT clear `:1151` or unblock the atomic cap — any surviving fail-open to `buildUnnestResultValue` keeps the name model alive. **Slice 2 (the name-ambiguous bare-twin ordinal-projection compose + the outer-column-predicate positional rebase across the barrier + a Q5 census) remains the deferred frontier.** Fully 4-gated (Graefe design + impl ACK, @claude ACK, Torvalds ACK, codex clean) — with two review-surfaced correctness fixes folded (the P2 owner-alias shadow root + the P1 outer-column-filter suppress).

#### SLICE 2a LANDED (2026-07-08, Graefe design-ACK) — the NAME-AMBIGUOUS CROSS-LEG BARE-TWIN gathers via the RAW SEED (NOT the positional wrap — the wrap was tried, NAK'd for wrong-rows, and dropped). `FROM A, B, A.arr AS x` where two SEPARATE legs share a bare column name (`k`) previously DECLINED the gathered ordinal path at `rfc173_w5_unnest_gather.go:126-135` (dup-name → name-model). It now GATHERS via the RAW flat seed — NO decline, NO wrap. Each plain leg is its OWN quantifier in the flat select (`Scan(A)`, `Scan(B)`), so a QUALIFIED `A.k`/`B.k` routes to its own scan in the SELECT list, an outer WHERE, and a cross-leg predicate alike — there is no flat-row collision for qualified reads (the collision exists only for a BARE `k`, which errors 42702 at semantic analysis BEFORE the translator, Java-matching standard SQL). The `:126-135` any-dup decline became a NARROWER `declineBuriedDup` that fires ONLY for a box-involved dup; a NON-GROUPED non-box cross-leg dup falls through to the raw seed; the wrap `wrapGatheredPositionalKeys` is GROUP-BY-only again (its original purpose). **The GROUPED non-box cross-leg dup DECLINES to name-model** (`if t.underAggregate && nonBoxCrossLegDup { return nil }`) — the deferred grouped-bare-twin increment: the grouped path is stuck between the wrap (name-BLIND to qualifiers — the grouped ordinal resolution looks up bare `k`, not `A.k`/`B.k`, so a dup collapses to first-match and `WHERE B.k=200 GROUP BY A.k` drops every row, the identical outer-filter first-match class in the grouped path) and the raw seed (a GROUP BY over the positional seed reads a shadow-qualified key it lacks and groups every row under NULL, `A.k=<nil>` — the exact shadow-key defect the wrap was built to fix). Disambiguating a grouped dup needs the wrap's ordinal resolution to HONOR the qualifier (route `B.k` to the ALIAS.COL window key), a compose-direction change deferred with the box increment; declining restores master's pre-2a behavior (name-model resolves the qualified grouped read) — no regression. **Design history — why NOT the wrap.** The design-ACK'd blueprint was "gather via the positional wrap"; the impl was NAK'd by @claude with a real WRONG-ROWS bug (forbidden): `wrapGatheredPositionalKeys` emits per-leg window keys (`A.k`→slot1, `B.k`→slot4) AND a bare pass-through (`k`→slot1 = A's k, first-match); the SELECT projection resolves qualified `B.k` to the `B.k` window key, but the OUTER WHERE `PredicatesFilter` (built above the wrap) resolved qualified `B.k` to the bare `k` key (=A's k=100), so `WHERE B.k=200` evaluated 100=200 and dropped every row (same outer-filter first-match class codex caught on Slice 1). Forcing the wrap off exposed that the RAW seed already resolves ALL cases correctly (verified vs real FDB: `SELECT A.k,B.k`, `WHERE B.k=200→[7,8]`, `WHERE B.k=999→[]`, `WHERE A.k<>B.k→[7,8]`, `WHERE A.k=100→[7,8]`). So the wrap was both unnecessary AND the source of the bug; the raw seed is strictly simpler and correct. SCOPE (conservative, matching the de-risked shape): only NON-BOX cross-leg dups (two single-namespace scan legs) gather; a box-involved dup DECLINES — a box is ONE opaque quantifier whose buried column can't be qualified apart. Two decline classes: (i) WITHIN-ONE-LEG buried dup (a merge-opaque box's concat carrying two same-named columns, e.g. Order.PRICE + Customer.PRICE in a `(A FULL B)` box); (ii) CROSS-LEG dup where either leg peels to a box. Both are the later increment (2a'). The name-model fallback's OWN handling of a box-involved dup + comma-lateral unnest is a PRE-EXISTING gap (it cannot physicalize the shape — a malformed `LogicalProjectionExpression`, for every variant; it declined to name-model before 2a too) folded into 2a'. Certificate `sqldriver/rfc173_s4_baretwin_gather_fdb_test.go` (10 subtests): qualified bare-twin resolves by QUANTIFIER via the raw seed (single Project, no wrap); bare element X resolves; WHERE on a qualified bare-twin resolves on the FIRST leg AND a LATER leg AND a cross-leg predicate (the wrap-regression pins — `WHERE B.k=200`, `WHERE B.k=999`, `WHERE A.k<>B.k`); ORDER BY on the first AND a later leg's dup column (the outer-operator sibling axis); grouped bare-twin COUNT and grouped bare-twin + cross-leg WHERE (`WHERE B.k=200 GROUP BY A.k`) both DECLINE to name-model and answer correctly — pinning that the grouped path does NOT ship the outer-filter first-match wrong rows; bare-ambiguous 42702; non-ambiguous byte-identical raw seed; within-box buried dup declines to name-model; box-involved cross-leg dup declines AND fails-to-plan (safety: no wrong rows) rather than answering — flips green when 2a' lands. White-box `TestRFC173W5_Gathered_DeclineBoundary` case (b) two-scan dup gathers via the raw `SelectExpression`; case (b2) box-involved (FULL box + colliding scan) declines; case (b3) the SAME two-scan dup UNDER AGGREGATE declines (the grouped forward sentinel — the only difference from (b) is `underAggregate`; a plan-shape pin the E2E-rows tests can't provide, forcing re-certification when qualifier-honoring grouped resolution lands). Full sqldriver package + query white-box + docscheck green. **Fully 4-gated on HEAD 9f3a66573 (Graefe design + impl ACK, @claude ACK — 18 FDB probes incl. a multi-row later leg so a first-match misresolution surfaces as a wrong VALUE not just count, Torvalds ACK, codex clean).** The slice surfaced and pinned THREE real bugs: the wrap's outer-WHERE wrong-rows (@claude's NAK → the raw-seed pivot), the box-involved malformed fallback (→ decline + fails-to-plan safety pin), and the grouped bare-twin + cross-leg WHERE (Graefe's required probe → grouped decline + b3 forward sentinel). Narrows `:1151`'s bare-twin residency for the NON-GROUPED non-box cross-leg case; the box-involved dup (2a') + the GROUPED bare-twin (needs qualifier-honoring grouped ordinal resolution, folded with 2a') + outer-column-predicate rebase (2b) + Q5 census remain — Graefe's strategic note: {box dup, grouped bare-twin, outer-predicate rebase} all converge on ONE substrate (qualifier-honoring ordinal resolution — ordinal projections that route ALIAS.COL to leg windows); build it once and it closes all three, then Q5 + the `:1151` clear + the atomic cap. BOOKED non-blocking follow-ups for Slice 2 (both optimization/test-fidelity, not correctness): (a) clear `chainedUnnestUnderFilter` in `translateSubqueryRef` (next to the `inInnerCluster` clear) so a FILTER-LESS subquery under an outer filter keeps its ordinalization instead of being coarsely suppressed — fresh-scope discipline, correct either way; (b) a name-model-vs-ordinal plan-shape discriminator on the element-WHERE decline test (today the outer-column-WHERE malformed-without-suppress is the mechanism sentinel; the element-WHERE test guards rows only, since name-model and ordinal share the nested-FlatMap shape).

#### QUALIFIER-HONORING RESOLUTION LANDED (2026-07-09, Graefe design-ACK (C)/(A)/(H)) — the GROUPED gather UN-COLLAPSES; the wrap + ALIAS.COL name keys RETIRED. This is the ONE substrate Graefe flagged (the grouped bare-twin + the disjoint/shadow grouped element all converge on it). **Root cause of the deferral:** the `wrapGatheredPositionalKeys` layer collapsed the N per-leg quantifiers into ONE fresh name-keyed projection quantifier (a divergence from Java, which keeps the per-leg select under the group-by and pulls up by ordinal), then needed ALIAS.COL NAME keys — name-model residue — to disambiguate; and the grouped ordinal resolution was name-BLIND to qualifiers. **Fix (Graefe ruled (C) don't-collapse over (B) second-bake-path):** `translateGatheredUnnestCluster` returns the RAW per-leg seed for a grouped gather (no wrap), and `translateAggregate` POSITIONALLY BAKES the group keys / operands over it — the qualifier-honoring read that replaces the wrap's name key. The outer WHERE bakes for FREE (the input is now a `SelectExpression`, so the existing `bakeGatedJoinPredicates` fires — the proof (B)'s second bake path was never needed). **The slot-source (Graefe (A), one authority):** leg columns resolve via `values.OrdinalSeedLegWindows` (the SHARED layout authority, agreeing bit-for-bit with the executor's `unnestMixedSeedSpans` by `TestRFC173W4c_MixedSeedSpanLayoutCrossAgreement`). It was EXTENDED — with its executor twin, in lockstep, fixture re-pinned — to admit the multi-source gathered seed and then GENERALIZED to **element-ANYWHERE** (`FROM A, A.arr AS x, B` — the mid-list element splits the leg run; the walk windows the leading legs, steps over the element's slot, windows the trailing legs). Trailing is just "element at last, no trailing legs." **The element (Graefe (H), field-identity NOT a window):** the element's slot is its rc index, located via the seed's OWN element predicate `fieldValueReferencesInner` (one definition, no ad-hoc scan) — it is a single distinguished field, not a layout window that could drift (rc-index↔slot is the substrate the authority stands on). Resolution order: a QUALIFIED read whose alias names a LEG window wins (`U.V` is U's V, never a same-named element); a BARE read is element-first (an element AS alias shadowing a later leg column reads the element). Operand bake RECURSES the value tree (`values.Replace`) so `SUM(A.k+B.k)` bakes BOTH dup leaves (top-level-only = silent wrong rows); `valueReadsBakedOrdinal` (executor) is recursion-aware and per-value gated so the name-model group-by population reads Datum unchanged. **DELETED:** `wrapGatheredPositionalKeys`, `gatheredPositionalWrapEligible`, `gatheredBakeSlotMap`, the `gatheredBakeSlots` stash — the wrap and its ALIAS.COL name keys are gone. Sentinels flipped: `DeclineBoundary` (b3) now asserts the grouped bare-twin GATHERS (raw `SelectExpression`); the disjoint/shadow plan-shape assertions now assert the un-collapse (a baked-ordinal `StreamingAgg` key over the raw NLJ gather, NO wrap Project). Certificate: `rfc173_s4_baretwin_gather_fdb_test.go` grows the grouped bare-twin (resolves via un-collapse), grouped + cross-leg WHERE, compound operand `SUM(A.k+B.k)`=600 (recursion pin, top-level-only=400), and the MID-LIST element grouped bare-twin (works — the element-anywhere leg windows); the whole `TestFDB_ArrayUnnestOrdinality` family (disjoint `GROUP BY EL` + `SUM(WSRC.SID)` outer-leg aggregate, R19/R20 shadow, HAVING, LIKE ESCAPE) green on rows AND the wrap-gone plan. Full `bazelisk test //... -stress` green. **Closes the grouped bare-twin AND the disjoint/shadow grouped element** — the substrate is real and one-path. Next applications of the SAME substrate: box-dup (buried leaves + the FULL-NULL de-risk) and 2b (the chained SelectMergeRule barrier — a different structure), each its own de-risked, 4-gated increment; then Q5 + the `:1151` clear + the atomic cap. BAKE-GATE (codex P2, fixed): a gathered unnest that DECLINES to the name-model path still flows a `SelectExpression` with an Explode quantifier, but its RC is ANCHORED and emits no positional row — `translateAggregate` gates the positional bake on `!rc.AnchoredJoin` so a name-model fallback's grouped element/leg reads stay name-model (baking a name-model row would evaluate the ordinal against the Datum map and error); pinned by `grouped_over_name_model_fallback_does_not_bake`. BOOKED follow-up (Torvalds N2, non-blocking): `OrdinalSeedLegWindows` (planner) and `unnestMixedSeedSpans` (executor) are TWO element-anywhere walks kept bit-for-bit agreeing by the cross-agreement fixture — a real duplication (different key normalization `ToUpper` vs `String()`, different split-run rejection: map-dedup vs an explicit `seen`). The honest end-state is the executor deriving spans FROM the values authority instead of re-walking; collapse the two walks before any THIRD extension, not another parallel edit. **Fully 4-gated on HEAD 030eb9870 (Graefe design + impl ACK — verified the cross-agreement invariant untouched and every fix property-based; @claude ACK — 6 FDB-probed axes incl. two-elements + multi-leg-both-sides cross-agreement, no wrong rows; Torvalds ACK — "NAK lifted, ship it"; codex clean).** The multi-gate process surfaced two real defects this slice: codex's anchored-fallback bake gap (a gather declining to name-model still flows an Explode select whose anchored RC emits no positional row — baking there errors; gated on `!rc.AnchoredJoin`, pinned) and a dead fail-open decline that had drifted after leg translation into a latent double-registration trap (independently flagged by @claude's 131/0 instrumentation and Torvalds — deleted). The wrap and the ALIAS.COL name keys are gone; the grouped bare-twin, the disjoint/shadow grouped element (element at any position × leg-column aggregate), and the compound-operand recursion are all certified on rows AND the wrap-gone plan.

#### SLICE 2b LANDED (2026-07-09, FULLY 4-GATED on 60cbb0c08 — Graefe design + impl ACK, @claude ACK, Torvalds ACK, codex clean) — filtered-chained ORDINALIZES; the coarse `chainedUnnestUnderFilter` decline is RETIRED. A chained lateral unnest under an ancestor WHERE (`FROM t, t.SARR AS x, x.SUB AS y WHERE <pred>`) previously declined to the name-model residual (`buildUnnestResultValue` → `NewAnchoredJoinRecord`) via a COARSE downward suppress bit (`chainedUnnestUnderFilter`, set in translateFilter, read in the chained ordinal gate — Slice 1's `:1151` codex-P1 stopgap). It now ORDINALIZES for every non-subquery, non-OR filter shape, retiring 10+ name-model-caller invocations toward the atomic cap. The field + its set/save/restore + the gate are all DELETED. **Predicate placement is PER CONJUNCT (Graefe design-ACK'd the ⊆-outerLegs form over an enumerated x/y one):** `rebaseChainedOuterLegPredicate` splits the top-level AND (bakeConjuncts discipline); `chainedPredScanPushable(p, outerLegs)` gates on `GetCorrelatedToOfPredicate(p) ⊆ outerLegs` — a conjunct whose correlations are all outer BASE legs (`{t}`) is PUSHABLE-TO-SCAN (PushFilterBelowJoinRule sends it to `Scan(t)`), so leave its refs LAZY (`QOV(t)`, a SARG); a conjunct referencing ANY in-chain correlation (the first-link element x, the second-link element y) is NOT scan-pushable, stays at the inner Explode whose row is `QOV(mergedCorr)`'s merged `[t-cols ++ x]` row, so REBASE its outer-leg refs to the qualified `leg.col` key there. Structural (⊆-outerLegs) not enumerated: ONE branch, every in-chain correlation flows keep-rebase uniformly, no per-element drift. The gate-mirrors-rule coupling is verified END-TO-END (SARG on `Scan(t, [=])` for outer-only, inner `PredicatesFilter` for x/y-referencing / straddling `t.id = y`). **AXIS AUDIT (Graefe condition, "audit before removing a coarse decline"):** every NON-OR filter shape the coarse decline was catching — eq / AND / IN-list / BETWEEN / arithmetic / IS-NULL / NOT / 3-conjunct / straddling — row-verified correct on the ordinal path. **Two residuals decline to name-model (correct-or-loud).** **(1) An OR in the filter — @claude's correctness gate caught a REGRESSION in the first cut** (which ordinalized ORs): `(t.id=10 AND y>2) OR (t.id=3 AND y<5)` returned correct rows on the name-model parent but MALFORMED-PLANNED once ordinalized, because `rebaseChainedOuterLegPredicate` name-keys the whole OR (only the top-level AND is split), then NormalizePredicatesRule distributes it to CNF and PredicatePushDownRule pushes the extracted PURE-OUTER clause (`t.id=10 OR t.id=3`) to the first-link ORDINAL FlatMap where the name key strands (ordinal -1). A NARROW `chainedUnderOrFilter` bit — the disciplined successor to the retired coarse bit, set in translateFilter iff `predicateContainsOr(f.Predicate)`, read once at the chained gate — declines any OR-carrying filter to name-model (correct rows). Ordinalizing the OR needs the POSITIONAL bake (an `ofOrdinal` resolves on the ordinal row at BOTH the first-link FlatMap and the inner Explode, unlike a name key); a first attempt tripped a "leg X DIVERGENT baked types (5 vs 6 fields)" drift panic — the chained seed's merged type must be aligned with the `OrdinalSeedLegWindows` bake authority first (booked). **(2) A SCALAR SUBQUERY in the filter.** It rides the wedgeGate POSITIONAL bake (`rebaseUnnestOuterLegPredicateOrdinal`), a DIFFERENT path than the per-conjunct name-keyed rebase, and once the chained unnest ordinalizes it bakes the outer ref to ordinal -1 → a malformed plan. HEAD shipped it SILENT-WRONG `[]` (name-model swallowed it). Rejected LOUDLY now (Graefe ruling (C): typed `0A000` in translateFilter, gated `len(f.ScalarSubqueries) > 0 && filterInputHasChainedUnnest(f.Input)` — TYPED, no text match). **The gate WALKS the join spine** (not just the direct rightmost source), so a chained unnest buried behind a trailing table (`FROM t, t.a AS x, x.b AS y, z`) or a join leg is caught too — the direct-only first cut left that buried shape as the same silent-`[]` (Graefe's (i) altitude probe caught it; widened + pinned). It STOPS at relation boundaries (Project/CTE/Aggregate) so an ENCAPSULATED derived-table chained unnest is not falsely gated. **The audit surfaced a bigger PRE-EXISTING silent-wrong (booked, orthogonal):** scalar-subquery-in-a-WHERE-comparison returns `[]` for a PLAIN table too (`WHERE id = (SELECT MAX(id) FROM t2)` → `[]`) — it's a general scalar-subquery-in-WHERE hole (projection-position scalar subqueries work), NOT chained-specific; the chained case additionally would CRASH, so 2b's 0A000 prevents the silent-`[]` → crash regression. Java answers both → reach gap; the `0A000` sentinel flips green when the general feature lands. IN-subquery / EXISTS in a filter over a chained unnest fail 0AF00 UPSTREAM of the chained dispatch (orthogonal). Certificate `sqldriver/rfc173_s4_filtered_chained_fdb_test.go` (10 non-subquery shapes with SARG/inner-filter end-to-end plan asserts + `scalar_subquery_loud_0A000` + `scalar_subquery_buried_loud_0A000`); the two now-stale filtered subtests in `rfc173_s4_chained_unnest_ordinal_fdb_test.go` (which claimed "declines to name-model") are DELETED — filtered coverage lives in the new cert; that cert keeps the UNFILTERED ordinal path + the genuine 3-link clusterArity-poison decline. B1 corpus (1641) + full suite green. **The Slice-2a booked follow-ups:** (b) "name-model-vs-ordinal plan-shape discriminator on the element-WHERE decline test" is now VOID (the element-WHERE test is deleted; it ordinalizes, and the end-to-end SARG/inner-filter asserts in the new cert ARE the plan-shape discriminator). (a) "clear the suppress bit in translateSubqueryRef" is NOT void — the coarse `chainedUnnestUnderFilter` is deleted, but its subquery-scope leak MIGRATED to the narrow `chainedUnderOrFilter` (an OR-carrying OUTER filter's bit leaks into an inline-EXISTS subquery, over-declining its chained unnest to name-model — correct rows, missed opt); the translateSubqueryRef-style clear is re-booked with the positional-bake slice (Torvalds flagged the optimistic void-ing). Reviews (fully 4-gated on 60cbb0c08): Graefe design-ACK (⊆-outerLegs refinement + ruling (C) + retire-after-audit) → conditional impl-ACK on fffb88f53 (core + enclosing-correlation by-construction ACK'd; (i) altitude needed a probe) → finalized impl-ACK on dc7e3a5f8 ((i) altitude widened gate verified) + design-ACK on the narrow OR-decline (traced predicateContainsOr's De Morgan completeness himself). @claude NAK on fffb88f53 (the OR-ordinalization regression — the first cut ordinalized ORs and MALFORMED-PLANNED the nested-outer-conjunct shape `(t.id=10 AND y>2) OR (t.id=3 AND y<5)`, correct on the name-model parent; caught by "verify the rows, not the read") → ACK on dc7e3a5f8 (fix: the narrow `chainedUnderOrFilter` decline + the `or_over_chained_declines_correct_rows` regression cert on the axis the flat-OR cert missed; re-verified predicateContainsOr sound). Torvalds NAK on fffb88f53 (stale filtered subtests + no-op `rebaseUnnestOuterLegPredicateImpl` wrapper split + 42703 doc tour) → narrow NAK on dc7e3a5f8 (a LYING "no such predicate reaches here" comment — the OR DOES reach the rebase on the name-model-declined path) → ACK on 60cbb0c08 (comment corrected to the true name-model-seed mechanism, re-traced; scope-leak Finding B disclosed + re-booked). codex clean on both (its one P1 was a working-tree artifact of a throwaway probe). The multi-gate process flushed TWO real defects the first cut shipped — @claude's OR-ordinalization malformed-plan and Graefe's (i) buried-scalar-subquery silent-`[]` — plus Torvalds' lying-comment; "verify the rows, not the read" earned its keep. Residuals booked in TODO.md: ordinalize the OR via the positional bake (align the chained seed's merged type with the bake authority), the general scalar-subquery-in-WHERE feature (unblocks both plain + chained), the x.K scalar-sibling resolver gap (42703, orthogonal). Next: Q5 census + the `:1151` clear + the atomic cap.

#### SLICE 2c LANDED (2026-07-09, Graefe design-ACK) — the 2-chain OR ORDINALIZES via the POSITIONAL bake; `chainedUnderOrFilter` RETIRED. The 2b OR-decline interim is closed for the 2-chain. **Root cause of the 2b "leg X DIVERGENT baked types (5 vs 6 fields)" panic: a WRONG-TYPE-IN-THE-BAKE bug, NOT an `OrdinalSeedLegWindows` disagreement.** Dumped the types for `FROM T4, T4.SARR AS X, X.SUB AS Y`: the seed's OUTER leg run bakes over `QOV(outerCorr=X, ordinalLegType)` = a **5-field** type `[ID SARR SCARR SUB X]`; the full seed RC is 6 fields `[… X Y]`; `OrdinalSeedLegWindows` correctly derives the 6-field merged type. The first positional-bake attempt passed the **6-field merged type** as the QOV type, but the outer QOV is the **5-field ordinalLegType** (same correlation X) → drift tripwire. The fix: bake outer-col refs over `ordinalLegType(join.Left)` (the outer QOV's OWN type), NOT the merged type — an `ofOrdinal(QOV(X, ordinalLegType), slot)` matches the seed's outer run bit-for-bit and resolves on the ordinal row at every level CNF+pushdown lands the clause (Graefe confirmed structurally: a pushed pure-outer clause lands at X's ForEach = the first-link ordinal row = ordinalLegType). So `OrdinalSeedLegWindows` / the executor span / the cross-agreement fixture are UNTOUCHED — no authority extension, no lockstep, no fixture re-pin. `rebaseChainedOuterLegPredicate` now takes `ordType` + `ordinalSeed` + returns `(pred, ok)` (`!ok` → decline to name-model); over an ORDINAL seed the keep-branch bakes POSITIONALLY (`rebaseUnnestOuterLegPredicateOrdinal(p, ordType, ordType, …)`), over a NAME-MODEL seed it keeps the NAME-KEY rebase (the caller sets `ordinalSeed` from `sel`'s own RC — `!AnchoredJoin`). **This seed-form gate is LOAD-BEARING — codex + @claude both caught a first-cut regression** where the positional bake fired on the name-model FALLBACK (gated on `isChainedUnnest`, not on whether the seed ordinalized): a mixed-inner-ref clause (`t.id = z`, `(t.id=10 AND z<13) OR (t.id=20)`) over a 3+-link chain, OR a 2-CHAIN buried behind a trailing table (`FROM t, t.a AS x, x.b AS y, z WHERE t.id = y`), then rewrote outer refs to `ofOrdinal(QOV)` against the name-keyed row → `field … not resolvable, ordinal -1, malformed plan` (correct rows on the parent). The `ordinalSeed` check routes ANY name-model seed (3+-link clusterArity-declined, buried-2-chain, or `!ok` decline) to the NAME-KEY rebase, exactly as HEAD. Regression cert `TestFDB_RFC173S4_ThreeLinkFilteredNameModel` (3-link straddle-match / mixed-OR / and-inner / pure-outer-OR + the buried-2-chain straddle). `chainedUnderOrFilter` (field + set + gate) + `predicateContainsOr` DELETED — a decline retired; the subquery-scope leak the coarse bit carried is GONE with the flag, not merely booked. `:1161` narrows for FREE (line 404 clears the enclosure on the ordinal path; a declining shape keeps it — no strand). **SCOPE (Graefe (A), bounded — the disciplined incremental split):** the POSITIONAL bake fires for the ordinalizing 2-CHAIN only; every name-model chained seed keeps the name-key rebase (correct rows). A 3+-link chain declines EARLIER (clusterArity poison, untouched) and stays name-model CORRECT. Its ORDINALIZATION — lifting clusterArity + the mixed-inner-ref placement fix (the strand lives in `pushBuriedUnnestPredicateDown` + `rewriteUnnestPredicate`, baking the deepest element against the 2-chain row) — + the FULL `:1161` retire are the DEEPER-NESTING slice (booked), which inherits this validated substrate. Scalar-subquery stays the separate `:2720`/0A000 residual. **Audit (Graefe condition, before removing a decline):** 2-chain OR ordinalizes (`t.id=1 OR y=9`→{7,8,9}); 3-link OR stays name-model (`t.id=1 OR t.id=2` over `…SUBSTRUCT AS Y, Y.DEEP AS Z`→{11,12,13,20}, no strand); OR-with-scalar-subquery→0A000 first; 2-chain all-shapes ordinalize; 3-link = HEAD; B1 (1641) + query package green. Cert: the `or_over_chained_declines_correct_rows` subtest renamed to `…_ordinalizes` (a lying cert is a docscheck fail) + the NOT(OR)/NOT(AND) De Morgan comments fixed to the ordinalize reality. Reviews: Graefe design-ACK (bounded slice + audit) + impl-ACK on 7950a6ed0. codex NAK + @claude NAK on 7950a6ed0 — BOTH caught the same regression (the positional bake fired on the name-model chained fallback, stranding a mixed-inner-ref clause; @claude additionally found the buried-2-chain variant); Torvalds ACK on 7950a6ed0 but flagged the missing filtered-3-link-OR sentinel as unprobed — the exact axis the NAK hit. Fixed by the `ordinalSeed` seed-form discriminator + the `ThreeLinkFilteredNameModel` regression cert. **FULLY 4-GATED on the fixed HEAD `4cde13db2`:** Graefe re-ACK (owned that his impl-ACK missed a successful positional bake — `ok=true` — firing against a name-model seed, which the `!ok` fallback never catches; the fix routes on the seed's emergent form per principle #10); Torvalds ACK (Finding #1 confirmed load-bearing; `ordinalSeed` threading minimal, `!AnchoredJoin` an honest established discriminator = the EXISTS-path signal at :2666, cert data discriminating on all three faces plan/exec/rows); @claude ACK (re-ran the three regressing probes — all now return the parent's correct rows; the 2-chain OR slot-3 win intact; the `!AnchoredJoin` discriminator complete in BOTH directions — no reachable name-model plan presents as a non-AnchoredJoin RC, incl. the `unnestUnderExistential && chained` non-RC path which defaults to the safe name-key rebase); codex clean (no actionable correctness issues; ran both certs green). "Verify the rows, not the read" earned its keep again — the audit's pure-outer-OR sample missed the mixed-inner-ref name-model path.

#### SLICE 2d LANDED (2026-07-09, Graefe design-ACK; corrected same-day after a 3-gate NAK round) — LINEAR 3+-link chains ORDINALIZE at any depth, filtered or not; forks/box-bases decline; the boundary is pinned WHITE-BOX. **The record keeps the failure honestly: the first cut (272b3e855) over-claimed.** What it claimed: "filtered 3-link ordinalizes; the `Scan(T4,[=])` SARG is an ordinal-only discriminator; the 2c bake already places mixed clauses at depth 3." What was actually true (translate-level stderr tracing + a NEUTERED-GATE control run): only UNFILTERED 3/4-link chains ordinalized — every FILTERED 3+-link shape still ran the NAME-MODEL fallback, because `unnestExistsSeedSafe`'s box-leg-conjunct arm (`unnestOuterConjunctOnBoxLeg && len(outerBoundAliases)>1`, the c5a guard) fires on any chained firstBase (multi-alias by construction); the SARG is MODEL-INDEPENDENT (the lazy ⊆-outerLegs path precedes the seed-form fork, so both models SARG identically — the "discriminating" certs passed VERBATIM on the name-model parent, confirmed independently by a reviewer's parent-worktree control where all 7 EXPLAINs were byte-identical); and a second reviewer's adversarial probe found a REGRESSION the unscoped gate lift shipped: a FORK chain (`FROM T4, T4.SARR X, X.SUBSTRUCT Y, X.SUB W` — W's owner X is TWO links back) was admitted, rooted its collection at the PRECEDING link's element slot (`elementRootIdx`), and malformed-planned at `ordinal -1` (correct rows on the parent via name-model); with colliding field names it would have been SILENTLY WRONG. **The corrective commit (this HEAD):** (1) **owner-LINEARITY enforcement** — `translateChainedUnnestOrdinal` checks `u.Segments[0] == firstUnnest.Alias` (EqualFold) AND `chainedBaseOrdinalizes` runs over `j.Left` (NOT firstBase — walking firstBase alone would skip firstUnnest's own linearity, admitting a fork exactly one level below the dispatching link: `…, x.substruct AS y, x.substruct AS y2, y2.deep AS z`), with a per-level `un.Segments[0] == preceding link's Alias` check in the recursion; forks decline to name-model (correct by name). (2) **the box-leg-conjunct arm SCOPED, not bypassed** (Graefe form ruling): `unnestExistsSeedSafe(left, spineBase bool)` — the ONE arm becomes `flag && !spineBase && multiAlias`; only the chained gate passes `spineBase=chainedBaseOrdinalizes-admitted`, every other caller passes false, and every OTHER decline arm (existential today, anything later) stays live for spines automatically — so FILTERED linear spines now genuinely ordinalize (trace-verified; the depth-3 straddle `T4.ID=Z` resolves through the 2c positional bake FOR REAL this time). (3) **The boundary pinned at the only layer that can see it** — `rfc173_2d_chained_spine_seed_test.go` white-box pins assert the seed RC's `AnchoredJoin` flag off `translateChainedUnnestJoin` per shape: linear 2/3/4-link × filtered/unfiltered → ordinal; top-fork (±filter), MID-SPINE fork, box-base (±filter — the arm scoping must not leak the bypass to a genuine box) → name-model; twin-fork + 1-segment-mid-spine → LOUD upstream rejections (pinned as loud-not-silent). ONE-TIME FEATURE-OFF CONTROL run at introduction: gate neutered → all six linear ordinal pins FAIL (they discriminate by construction). Plus fork rows-certs e2e (`fork_projection`/`fork_filtered` → name-model rows) and honest cert rewrites (every "SARG ⇒ ordinal" claim deleted; the SQL certs are rows+placement pins and say so). **Scope/accounting (unchanged from the design-ACK):** box-base chains + `!ok` declines still reach `NewAnchoredJoinRecord` (chained producer NARROWS, zeros only with c5b); `:1161` narrows (box/cluster readers remain — cluster_gate.go:315,356, w4b:565); the deep-WHERE 42703 (4+-link AS / any AT in WHERE) is a PRE-EXISTING semantic-resolver gap booked separately (reproduces name-model, upstream of translation; clean projections resolve at every depth incl. AT). **Standing discipline adopted (Graefe ruling):** any cert offered as a model-discriminator comes with a neutered-feature control run, or it doesn't count — a cert that passes with the feature off pins nothing. Reviews: Graefe design-ACK (bounded gate lift) then NAK-round corrections — Torvalds NAK on 272b3e855 (independent parent-worktree control: certs pass verbatim, EXPLAINs byte-identical; lying cert names; the doc-placement botch fixed here) + @claude NAK (the fork kill, differentially verified vs parent; demanded linearity at BOTH walk levels) + Graefe corrective rulings (arm-scoping form, white-box pin strategy, one-corrective-commit). 4-gate on the corrective HEAD. **SECOND CORRECTIVE ROUND (same day) — two more gate kills on 59eb67aaa, both fixed + pinned:** (1) a P1 SILENT-WRONG: `clusterArity(FULL OUTER)==1` (merge-opaque) let a spine BOTTOMING IN A FULL BOX through the walk base case with spineBase=true, suppressing the box-leg-conjunct arm for GENUINE box legs — under a box-leg WHERE the first link kept a name-model seed while the chained link built an ordinal seed OVER it (the mismatch class): `FROM T4 A FULL OUTER JOIN T4 B ON A.ID+10=B.ID, A.SARR X, X.SUB Y WHERE A.ID=1` returned wrong A-side values (parent correct). FIX: the walk returns (admitted, pureSpine) — admission UNCHANGED (FULL-box bottoms stay admitted, pre-slice parity: unfiltered still ordinalizes), but pureSpine = `len(outerBoundAliases(bottom))==1` (the ARM'S OWN authority, not a structural box probe — any future single-arity multi-alias source stays conservatively impure); only pureSpine exempts the arm, so a box-leg WHERE declines the WHOLE chain coherently with the first link's gate. Pinned white-box (full_box_bottom walk rows: admitted-impure; full_box_bottom_filtered → name-model / unfiltered → ordinal seed-form) + e2e (`fullbox_bottom_boxleg_filter` — the exact repro, correct rows). (2) a FALSE COVERAGE CLAIM in the white-box file itself — the one_segment_mid_spine comment said the walk's Segments<2 check was "pinned directly in the walk test below"; it was NOT (a reviewer's control run neutering the check stayed green — the same disease this round exists to cure). FIX: the 1-segment walk case added (it discriminates: with the check gone, the linearity comparison passes vacuously and the walk admits). PLUS harvested reviewer probe dimensions into the cert: the depth-3 SHADOW-SLOT straddle `T4.SUB=Z` (the positive slot-3 pin — a slot-0/ID misread returns a different row set) ± OR, and AT-first/AT-mid ordinal rows (P=2 discriminates the mid ordinal); the walk unit test now also pins (admitted, pure) for full-box / 1-segment / AT shapes. **FULLY 4-GATED on the round-2 HEAD `c1f6e2059`:** Graefe re-ACK (admission byte-equivalent to the ACK'd form; the pureSpine authority choice is "the right one-authority move" — the arm's own outerBoundAliases measure, fail direction conservative; the P1 family named: every bug in this class is "two sites disagreeing about which model a row is in"); Torvalds ACK (four fresh adversarial controls, all discriminating — Control E revert fails EXACTLY full_box_bottom_filtered at unit level and reproduces the silent-wrong live e2e, A.ID served from the other leg's slot; two-bool return endorsed; nit for a future touch: rename the spineBase parameter to pureSpine); @claude ACK (coherence holds BY CONSTRUCTION — monotone admission, pureSpine IS the first-link gate's own predicate, reverse split impossible via prevEnclosure; three-way differential conclusive: kill on 59eb67aaa / fixed on c1f6e2059 / pre-slice parity byte-identical on the unfiltered FULL-box chain; 23-probe battery green; non-blocking harvest residue: P=2 first-link AT data + the fullbox boxlegB/3link/inner-only probe trio); codex clean ("the pure-spine distinction is propagated consistently and prevents filtered FULL-box-bottomed chains from mixing ordinal and name-model seeds"; suites pass). Three reviewers, three distinct real finds on one slice (the false-discriminator certs, the fork kill, the FULL-box P1) — the multi-gate compounding as designed.

#### FORK SLICE LANDED (2026-07-10, Graefe design-ACK) — FORK spines ORDINALIZE via OWNER-SLOT rooting; the 2d linearity declines RETIRED. Charter: a chained link whose owner is NOT the immediately preceding link (`FROM T4, T4.SARR X, X.SUBSTRUCT Y, X.SUB W` top-fork; `…, X.SUBSTRUCT Y, X.SUBSTRUCT Y2, Y2.DEEP Z` mid-spine fork) now takes the ordinal seed, ±filter, ±AT. **Mechanism (purely translation-side — no executor/birth/window coupling):** `chainedSpineWalk` peels the left-deep unnest-right spine ONCE (bottom-first links + admitted + pureSpine), generalizing ownership from "immediately preceding" to "resolves BY ALIAS to exactly ONE deeper link" — resolved INSIDE the same single walk (Graefe's one-authority requirement); `chainedOwnerElementSlot(links, ownerAlias)` roots the collection at the OWNER's element slot = `len(ordinalLegColumns(owner.join.Left))` — the LAYOUT LAW (pinned per AT-combination in `TestRFC173S4_Fork_OwnerElementSlot`): each link appends [element, AT?], so the element is always the FIRST column its link contributes — invariant under the owner's own AT (ordinal FOLLOWS the element), downstream links (append after), upstream ATs incl. a one-column AT-ONLY link (Graefe's added pin case; parser-unreachable — AS defaults to the last segment — pinned defensively). Slot-EXPLICIT rooting only (the shadowing law). **De-risk (all probes conclusive before code):** dup-owner aliases 42712-LOUD upstream (link/table/case-fold variants — `TestRFC173Fork_DupAliasRejected` pins it; the walk's exactly-one-match arm is DEFENSIVE fail-toward-name-model); layout dumped per AT combination; the SILENT axis reproduced under a linearity-bypass control — disjoint names strand LOUD (ordinal -1), COLLIDING names (E1{SUB2, SS[E2{SUB2}]}) return `[W:100]` SILENTLY (Y's field served for X's) — proving the colliding cert's teeth; consumer sweep: the chained rebase is linearity-AGNOSTIC (ordinalLegType accumulates per spine order; alias-set pushability; seed-form routing). **Controls run at introduction:** (i) full feature-off (walk ownership linear-only + dispatch tip-only) → ALL FOUR fork seed-form pins FAIL (top_fork ±filter, mid_spine_fork, fork_over_full_box_unfiltered); (ii) MIS-ROOT control (tip slot instead of owner slot — the pre-slice bug) → `TestFDB_RFC173Fork_CollidingSubfield/colliding_fork` FAILS with exactly `[map[W:100]]`. **Coupling (Graefe's REQUIRED cert):** fork admission × impure bottom — fork-over-FULL-box + box-leg WHERE declines the WHOLE chain to name-model via the untouched pureSpine arm, pinned white-box (`fork_over_full_box_filtered` → name-model / unfiltered → ordinal) + e2e (`fork_over_box_boxleg_filter` in the FullBoxChainedSpine cert). **Certs:** the 2d fork rows-certs RENAMED (`fork_projection_declines_namemodel` → `fork_projection_ordinalizes` + filtered + a NEW fork straddle `T4.ID=W`→{1,1}); the colliding-schema cert (fork/filtered/straddle {1,2} + the LINEAR mid-element read {100} — owner-slot must resolve Y for `Y.SUB2`, X for `X.SUB2`); the walk unit table extended (genuine_fork_spine admitted-pure-4-links; table_owned_mid_spine + orphan_owner + duplicate_alias_spine declined defensively; full_box rows unchanged). **Fold-ins:** the `spineBase` comment straggler (2d seed test) + the two pre-2d "declines earlier via clusterArity poison" doc-rot comments at the chained rebase — rewritten to the post-fork reality. **Scope unchanged:** box bases, the pureSpine arm, prevEnclosure, EXISTS-over-chained (0AF00 upstream), twin-fork (upstream-rejected, pinned loud). **Endgame accounting:** P1's chained residual narrows to {box-base, FULL-box-filtered, enclosed, defensive !ok} — next: the box-substrate slice (box-base + box-leg-conjunct TOGETHER, axis-coupled de-risk), circular arms last. Reviews: Graefe design-consult ACK (fork-first over box-substrate — risk ordering) + design-ACK finalized on the probe results (dup-owner ruling: decline, minted bindings don't apply — owner resolution is inherently SQL-name-based). 4-gate on the impl HEAD. **FULLY 4-GATED (impl 94f5b1ccc + comments-only ghost-fix e2bf12484):** Graefe impl-ACK (traced the walk directly — one-authority geometry correct, AT-only links excluded as owners, the slot helper "carries the layout law where it lives"; "the controls are the strongest this arc has produced"; "the de-risk-first discipline made the impl almost an afterthought"); @claude ACK (15-probe battery byte-identical parent↔fork on his data; NEW adversarial variants all correct — 3-branch fork, owner-at-first-link under 4 links, AT-dense fork with per-link ordinals; coherence closed by PREFIX-STABILITY — the level-k walk applies the identical ownership predicate over the identical deeper-set, and forward-reference owners are impossible by FROM-order visibility; independent teeth-check: neutering the slot helper in his worktree fails exactly the fork seed-form pins); Torvalds comments-only NAK → ACK on e2bf12484 (five adversarial controls all reproduced — Controls F/F2 prove BOTH feature halves pinned separately, Control G reproduces [W:100] verbatim; the NAK: SEVEN stale chainedBaseOrdinalizes ghosts incl. the walk's own godoc naming its dead predecessor — all rewritten, verified mechanically via -U0 comment-only diff filter); codex clean (fork admission validates ownership + owner-slot rooting preserves seed-safety; the comments delta confirmed docs-only). Unharvested probe extras booked for a future touch: threebranch_fork, fork_owner_first_link_4deep, at_dense_fork row pins, fork_over_fullbox_inner_only.

#### BOX-SUBSTRATE DE-RISK ARC (2026-07-10, Graefe-gated rounds 1-4) — the coarse lifts REJECTED by evidence; Outcome A (the chained AXIS-coupling) LANDED; Outcome B booked with the circular arms. **The empirical chain (every step trace/probe-verified, every wrong theory falsified in-round):** (R1) the channel-2 spike (arm softened + WHERE-merge bake) produced silent-wrong rows ({A.ID:20, B.ID:nil} for WHERE B.ID=20) — initially misattributed to "empty rt.Legs → flat fallback" (REFUTED: a layout probe shows windows ALREADY propagate through chained merged types incl. box bottoms; a prototyped bottom-only propagation BROKE the 2c certs — per-link windows are load-bearing for runtime qualified-name resolution — and was reverted; commit b8441025a's message carries the refuted claim, corrected here and in the hardening pin's HONEST REACHABILITY note: the ordinalSlotInLegWindow multi-alias-without-windows decline is a DEFENSIVE fail-closed law, no currently-reachable shape hits it). (R2) TRUE mechanism, trace-confirmed: the box-leg conjunct takes the LAZY scan-pushable fork ({B} ⊆ outerLegs), leaving a FOREIGN-correlation name read at the merged select — unpushable and unrebaseable by any planner rule, and the runtime bare-name fallback over an ordinal row first-matches across legs (A's slot 0). The name model is safe ONLY because its name-key rebase re-anchors to mergedCorr. (R3) The pureBottom lazy-fork gate (impure-bottom conjuncts always rebase) is CORRECT but INSUFFICIENT: the bake is altitude+slot-correct yet evaluates NULL — the box NLJ is not birthActive (the birth triggers on ContainsBakedOrdinal over the box's OWN result value; a name-model box emits no positional row, and adaptLegPositional's synthesis from the merged multi-namespace box Datum is unfaithful → silent NULL). gather:93 and the seed arm are a COUPLED PAIR (the gather comment says so): lifting either alone recreates the mismatch; lifting BOTH plus the bake STILL fails — two more uncoupled sites gate even the simple class (legIsOrdinalSafe rejects non-INNER legs, rule_implement_nested_loop_join.go:1106; the FULL NLJ is built with sel.GetResultValue() VERBATIM, :166). The null-supply semantics floor HOLDS throughout (padded rows excluded; TestFDB_RFC173_NullSupplyBarrier pins it; the pushdown rules never descend a null-supplying leg — PushFilterBelowJoinRule is JoinInner-only, pushIntoSelect = Java's push-by-translation). A FrontierPinned push-barrier spike proved the logical rules are NOT the mover (plans unchanged) — the relocation is IMPLEMENTATION-time (the below-FOD hoist). **LANDED (Outcome A):** the chained gate now requires `boxOuterBirthsPositional(bottom)` for an IMPURE bottom — the AXIS-1↔AXIS-2 coupling law ("either half alone is broken") extended to the chained path, closing a LATENT UNFILTERED mismatch: a nested outer box (`(A LEFT B) FULL C`) is walk-admitted (arity-1, merge-opaque) but boxGatesFresh-FALSE → pre-coupling the chained seed ordinalized over a box whose first-link translate kept the name-model record → positional reads met a name-keyed row → silent NULL/empty. Pinned: nested_box_bottom_unfiltered/filtered seed-form cases (feature-off control: removing the coupling flips the unfiltered pin to ordinal — discriminating). **BOOKED (Outcome B — the box-substrate ordinalization, sequenced WITH the circular-arm lifts, LAST before the cap):** the executor-birth mapper's 5-site checklist — the enclosure declines (cluster_gate :315/:356), legIsOrdinalSafe non-INNER widening, the FULL-NLJ ordinal-seed build, the arm/pureSpine re-scoping + pureBottom lazy-fork gate + WHERE-merge bake (one coupled commit, none standalone) — plus the below-FOD hoist leg-row rebase gap and the grouped-consumer row-loss (both reachable only under the lift). The executor's positional-birth machinery itself is COMPLETE for FULL boxes (NLJ null-leg births, adaptLegPositional passthrough, layout agreement ordinalLegType ≡ rcOutputType incl. Legs) — the gap is translate/planner coupling, not executor capability.

**THE PRECISE REMAINING-SLICE LIST (S4 deep-read, the actionable roadmap).** A name-model leg is
name-model because a name-model PARENT set `inInnerCluster`, tripping one of the THREE enclosure
poison gates in `ordinalWedgeGateDecide` (rfc173_cluster_gate.go): (154) `!ordinalEligible(leg)` —
"a leg contains a name-model join"; (167) `kind!=Inner && inInnerCluster` — "outer box enclosed in a
name-model parent"; (208) `inInnerCluster` (inner) — the inner enclosure guard. All three comments
say verbatim the residency "retires at S4… this guard dies with" the name-model parents. An
UN-enclosed LEFT/RIGHT/FULL box already GATES (:187/:206); an un-enclosed inner cluster gates. So the
anchored branch dies when `inInnerCluster` is NEVER set — i.e. when the parents that set it are all
ordinalized. Those parents are a FINITE, enumerable list — the `inInnerCluster = true` sites (verified at HEAD:
:1074, :1991-defer, :2908, :3033, :3850, plus the CONDITIONAL :4291-join and :4539-flatten which set
`!gated`; :5060 recursive already lifted). Per-site retire-condition VARIES (nuance, not a uniform
lift):
  - :5060 recursive body — ✅ the enclosure IS lifted (R1: the body ordinalizes, sub-joins classify).
  - :3850 correlated-scalar — the SEED producer is retired (R3) BUT this site still sets the enclosure
    for the OUTER's own translation (a correlated-scalar outer is legitimately name-model-enclosed);
    whether that residual enclosure still forces a real box name-model needs its own analysis — it is
    NOT simply "done."
  - **:1074 unnest** — the non-gathered name-model unnest parent (R2 ordinalized the GATHERED
    multi-source + aggregate cases; this is the residual).
  - **:2908 / :3033 existential** — `buildExistentialSelect` (S4 commit 2 zeroed the projected-EXISTS-
    over-join :698 producer; these are the remaining existential-flatten enclosure setters). **SCOPED
    (c5a-session): BLOCKED, not a clean lift.** :2908 is now `buildExistentialJoinSelect:3009`, :3033 is
    `translateProjectOverExistsFilter:3134`. S4 commit 2 already retired their SCAN-LEG residency at the
    EXECUTOR (`legIsOrdinalSafe`/`reconstructFoldStep1Seed`, "no translator change — scan legs are
    positional-capable regardless of enclosure"). What the two enclosures still carry is EXACTLY the two
    follow-ons commit 2 booked: (a) BURIED GATED-BOX legs (`legIsOrdinalSafe` rejects a join leg → the
    name-model `:698` fallback runs, kept correct by the enclosure) — blocked on BURIED-LEAF WINDOWING
    under the fold seed (the SAME wall as c5a's clustered-leg FULL box / c5b); (b) CORRELATED folds —
    blocked on the F2 name-binder wall (`BakedNameContextError`, reverted twice). No cheap
    conditional-lift like commit 4's `existsOuterGatesFresh` — `buildExistentialJoinSelect` FLATTENS the
    join into a name-model `ForEach(left)+ForEach(right)` builder, so lifting requires the builder to
    ADOPT an ordinal seed (a producer-#2 slice). Sequence AFTER buried-leaf windowing (c5b) lands.
  - **:4539 flatten** — the existential/outer flatten builder's enclosure (`!gatedFlatten`). Per the
    c5a-session scoping, `:1074` (non-gathered unnest) and this `:4539` are the more tractable NEXT
    clean slices than :2908/:3033 (which consume buried-leaf windowing first).
  - **BURIED-LEAF WINDOWING under a seed is the shared high-leverage prerequisite (c5b).** c5a's
    clustered-leg FULL box AND :2908/:3033's buried-gated-box residency both wait on ONE piece: concat a
    gated-box leg's BURIED-LEAF columns into the positional seed + resolve a qualified buried ref
    (`a.col` where `a` is inside `(a JOIN b) t1`) by its leaf `[Start,Width)` window (reuse the
    channels-1+2 per-leg-window rebase + the R2 gathered box-leg buried-leaf qualification — don't
    re-derive). Landing c5b unblocks multiple enclosure sites at once.
  - :4291-join is the join site itself (Graefe refinement 1 — the anchored `else` branch), not a
    separate parent.
Each site is ONE producer-#2-style slice: ordinalize that parent's seed (positional + named-projection
wrap where an anchored consumer reads it), pin rows + memo-identity + EXPLAIN + the existential pins,
four-gate it. When every `inInnerCluster` setter's name-model residency is retired, the three
enclosure guards never fire → every box/cluster gates → :4402/:4650 is unreachable → producers #3+#4
are dead → the atomic cap (task #16) deletes NewAnchoredJoinRecord / NewReEnumerationAnchoredRecord /
the name model. That is the whole remaining RFC-173 endgame — a concrete per-site worklist (verify each
site's residual empirically, since some, like R3's :3850, are partially retired already), not a
gate-theory question.

### NEXT SLICE — :2908/:3033 projected-EXISTS buried-gated-box ordinalization (design-ACK'd direction, re-scoped)
> **SUPERSEDED by the REVISED DESIGN at the end of this section.** The "port buried-leaf windowing to the
> fold for a bare INNER gated-box leg / three-part flip" direction below was ACK'd, then INVALIDATED by
> implementation (corrections #1 and #2): an INNER box is mergeable, so it FLATTENS into the fold (it has no
> opaque leg to window); the windowing is an opaque-OUTER-box mechanism with no fold entry. The actual
> mechanism is the N-WAY FLAT EXISTENTIAL GENERALIZATION — see the REVISED DESIGN. The windowing prose below
> is kept for the audit trail (it explains WHY the pivot happened) but is NOT the mechanism to build.
c5b landed buried-leaf windowing, which was the sequenced prerequisite for this slice. Empirical residual
map (verified against the code, not the stale line refs): :1074 unnest residual is largely BLOCKED (its
tractable-looking multi-source INNER case is the NAME-AMBIGUOUS bare-twin — shared leg columns trip the
gathered path's name-ambiguity decline — which waits for the S4 compose direction); :3850 correlated-
scalar is largely RETIRED (`translateClusteredOuterScalar` ordinalizes every multi-table outer). So
:2908/:3033 is the correctly-sequenced next slice.

#### LANDED (2026-07-08, Graefe design-ACK) — `:4051` (`:3850`, `translateProjectWithCorrelatedScalar`) enclosure producer RETIRED. The former single `t.inInnerCluster = true` under a `defer` conflated TWO architecturally distinct positions (Graefe split them): the OUTER (`p.Input`) and the correlated-scalar INNER (`csq.InnerPlan`). Principled form landed (not a blanket `=false`): the OUTER now INHERITS `prevEnclosure` (the force deleted — only a single-source or ungated outer reaches this path; a buried-join/multi-source outer is `clusterArity≠1` and declines LOUDLY 0AF00 regardless of the flag, so the force had no observable effect, and forcing `=false` would be a latent wrong assertion when the whole project is itself a name-model leg); the INNER now roots a FRESH cluster via `translateSubqueryRef` (a correlated-scalar subquery is NEVER merged into its parent — same class as the EXISTS inner — so it gates on its own arity, not the outer enclosure; this makes the two never-merged-subquery classes consistent and removes the defer's outer/inner conflation for good). De-risk: outer decomposition byte-identical `=true` vs the lift (single/plain-join/derived-single outer correct; derived-JOIN outer `0AF00` — the reach gap). INNER certificate (the blind spot Graefe caught — my outer decomposition held a plain-table inner): inner-JOIN scalar and inner-aggregate-over-JOIN answer CORRECT values (`[[100 7][200 NULL]]`) with the inner rooted fresh; StrictSingle still `21000` on a 2-inner-row scalar; an inner WHERE-EXISTS declines at the FRONT-END (`0A000`, no SubqueryPlanner) so the inner-EXISTS `:2467` enclosed-gather reader is not reachable via this path. Full suite 55/55. Certificate pinned in `sqldriver/rfc173_s4_4051_cleanlift_fdb_test.go`. Producer count: one fewer `inInnerCluster` setter toward the atomic cap. (The `translateSubqueryRef`-inner + outer-inherit form IS the principled end-state Graefe specified, so no follow-up booking is needed beyond the shared `inInnerCluster`→downward-parameter refactor already booked for `:3234`.)

#### LANDED (2026-07-08, Graefe design-ACK) — `:3234` (`:3033`, `translateProjectOverExistsFilter`) enclosure producer RETIRED as a CLEAN LIFT (`t.inInnerCluster = true` → `= false`). The de-risk below invalidated the prior "flatten-enabling conditional" design; Graefe then traced the flip's DIRECTION and ACK'd the lift because the enclosure was the SUPPRESSOR of the correct path, not its protector. For an ENCLOSED multi-source lateral-unnest derived body with a spanning conjunct (`FROM A, A.arr AS x, B WHERE x = B.bk`), `=true` SUPPRESSED translateFilter's enclosed-gather rotation (`:2211`'s `!t.inInnerCluster`) → spanning conjunct on an unbound element → silent-0; `=false` FIRES the rotation → the gathered path that the DIRECT (non-wrapped) form already answers correctly (CONTROL `[X=7]`). Today the derived-wrapped projected-EXISTS shape is discarded by the `0AF00` CTE-carrier reach gap (byte-identical either way — the certificate below), so clearing is safe now AND correct-direction when the gap closes. Certificate + forward sentinel pinned in `sqldriver/rfc173_s4_3234_cleanlift_fdb_test.go` (CONTROL `[X=7]` invariant + the `0AF00` decline that TRIPS when the reach gap closes, forcing the author to confirm the gathered rows). BOOKED (Graefe condition 3): the genuine decouple of `:2211`'s caller-contextual read is retiring the mutable `inInnerCluster` field for a downward enclosure-context parameter (Volcano required-property style, ~15 threading sites) — a separate architectural refactor, NOT a producer-retirement blocker; no clean local tree predicate exists (a filter's "merge leg vs fresh root" is caller state, not subtree-derivable). Producer count: one fewer `inInnerCluster` setter toward the atomic cap.

#### EMPIRICAL FINDING (2026-07-08) — the `:3234` (`:3033`, `translateProjectOverExistsFilter`) residual is NOT the flatten-enabling conditional the design hypothesized; it is a CLEAN-LIFT candidate over a SEPARATE `0AF00` reach gap. A prior design pass proposed a `:2908`-style conditional (`existsDerivedLegBirthsPositional`: clear the enclosure for an identity/star derived body over a bare-INNER cluster so `SelectMergeRule` flattens the CTE body into the fold → positional seed). A `forceOrdinalSpike` dual-run de-risk (probe in `pkg/relational/conformance/ordinalspike/`) **invalidated the premise**:
- The target shape `SELECT c, EXISTS(…) FROM (SELECT * FROM a JOIN b …) d [WHERE …]` declines `0AF00` ("Cascades planner could not plan query") in **BOTH** spike states (OFF=name-model, ON=whole-gate ordinal). So `:3234`'s enclosure is **not what blocks it**, and the hypothesized flatten **does not fire** under ordinalization — the design's must-verify-#1 is empirically FALSE.
- Decomposition (spike OFF): every ingredient plans **alone** — derived-over-join with no EXISTS `[[1 7][2 8]]`; projected-EXISTS over a plain table `[[1 true][2 false]]`; projected-EXISTS over a plain JOIN (the `:2908` arm) `[[1 true][2 false]]`; projected-EXISTS over a derived-**single**-table `[[1 true][2 false]]`. Only the **combination** projected/WHERE-EXISTS over a derived-over-**JOIN** `0AF00`s. That `0AF00` is a distinct REACH gap (the existential fold's generic path over a `LogicalCTE` carrier does not flatten the buried join), orthogonal to name-model demolition.
- Clearing `:3234` (`t.inInnerCluster = false` probe) left the FULL suite 55/55 and the decomposition rows byte-identical (`0AF00` unchanged for the derived-over-join shape; s1/s3 still correct) — so it reads as a CLEAN LIFT (producer retirement, no new enablement), NOT the `existsDerivedLegBirthsPositional` conditional (there is nothing to enable). **Open before impl (Graefe design-ACK required):** (a) prove the enclosure is truly non-load-bearing across EVERY `f.Input` shape, not just the CTE carrier — a `LogicalProject`/`LogicalFilter`-over-join `f.Input` that escapes `translateOp`'s opaque-cut is untested and could be load-bearing (positional-seed-flattened-into-name-model-fold hazard); (b) decide whether the derived-over-join `0AF00` is booked as a separate read-side reach extension. The `existsDerivedLegBirthsPositional` predicate + the SQL-shape pin list from the prior design are RETAINED for that reach-gap slice, not this one.

**Design correction (Graefe design-ACK): the load-bearing work is in the EXECUTOR, not the translator.**
The projected-EXISTS fold `buildExistentialJoinSelect` (cascades_translator.go:3027) passes a name-model
`resultValue` through; the ordinalization happens in the executor — `foldStep1Seed`
(rule_implement_nested_loop_join.go:1159) REPLACES the name-model RC with `reconstructFoldStep1Seed`'s
positional `step1RV`. So this is NOT a translator-resultValue flip (my initial framing was wrong); it is a
THREE-PART coupled flip spanning the translator↔executor boundary:
  1. **Translator AXIS 1** (buildExistentialJoinSelect): clear `inInnerCluster` at the leg `translateRef`
     (:3073/:3078) ONLY for a bare gated-box leg, so that leg's PLAN births positional (ordinal-capable),
     not a name-model plan the executor can't seed.
  2. **Executor seed production — THE LOAD-BEARING NEW WORK** (not a c5b reuse): widen `legIsOrdinalSafe`
     (:1084, today scan-family only, `default→false` on any JOIN/FlatMap leg) to ADMIT a bare gated-box
     leg plan — mirror c5b's bare-vs-wrapped distinction (`legExposesBuriedOuterBox`/`hasWrappedBuriedJoin`)
     as the PLAN-level analog (admit a bare nested join, exclude a wrapped/OUTER box); and extend
     `reconstructFoldStep1Seed` (:1120, today a flat scan-per-leg `ofOrdinal` run with NO `rt.Legs`) to
     emit the BURIED-LEAF-WINDOWED seed for a gated-box leg — port c5b's `buriedLegBounds` windowing to the
     plan level so the leg's concat carries `rt.Legs` windows.
  3. **Executor rebase — REUSED** (the only clean c5b reuse): `ordinalSeedLegWindowsOf(step1RV)` (:2052)
     + the below-FOD exist-pred hoist (:2057+) already resolve a buried exist-pred ref positionally by its
     `[Start,Width)` window ONCE the seed carries them.

**Mirror `translateJoinWithExists` (the :4655 `gatedFlatten` sibling), NOT c5a's unnest arm.** :4539 =
`translateJoinWithExists` is a DISTINCT, ALREADY-FLIPPED builder (WHERE-EXISTS-over-a-join) that already
does the coupled flip (`gatedFlatten = ordinalWedgeGateDecide(j).Gated`, `inInnerCluster = !gatedFlatten`,
ordinal seed via `buildOrdinalJoinResultValue`) over the SAME executor. It is the REFERENCE to mirror, not
a residual to close; its own `!gatedFlatten` residual (arity≠2 N-way / existential-alias collisions) is
separately blocked. c5a's unnest path is a DIFFERENT executor arm — `implementJoinWithExistential`
explicitly declines explode legs (:1803) and defers to `translateUnnestExistsFilter`.

**Two-site certificate.** The coupling predicate and B1 certificate SPAN the translator↔executor boundary
(logical `LogicalOperator` AXIS-1 vs physical `RecordQueryPlan` seed production) — they cannot be one Go
function. Pin an INVARIANT enforced at two sites with a red-first test that toggling EITHER gate alone
reddens (mirroring the c5a/c5b `ClusteredBoxSeedsOrdinal` / `OuterConjunctCoupling` coupled-flip
discipline). Skipping the executor half strands a translator-flipped positional box-leg plan under a
`foldStep1Seed` that still declines (gated=false → name-model step-1 over a positional plan) — the exact
positional-seed-under-name-model mismatch the whole c5a/c5b chain kept hitting.

**F2 wall (correlated folds, sub-class b) stays declined — enforced in the executor.** `foldStep1Seed`
gate (1) is `!correlatedStep1` (:1160; a correlated FlatMap binds legs by name → `BakedNameContextError`,
reverted twice on that wall). The translator AXIS-1 gate MUST MIRROR the executor's
`correlatedStep1`/ordinal-safe/independent-legs condition, or it births a positional box leg under a fold
the executor declines → strand.

**Sequencing (Graefe steer):** (1) executor seed-production widening LEADS — the shared wall both
existential builders hit; the translator flip is inert without it. (2) INNER buried-box FIRST.
**EMPIRICAL CORRECTION to the initial premise** (characterized against the code, not assumed): the INNER
buried-box projected EXISTS `SELECT p.v, EXISTS(…) FROM (p JOIN q) JOIN r` DECLINES cleanly (0AF00) TODAY
— it does NOT produce name-model rows. The buried box plans fine OUTSIDE a fold and a scan-leg fold works
(commit 2), but the fold over a NON-SCAN leg has no ordinal seed (legIsOrdinalSafe rejects the JOIN leg)
AND no name-model fallback in the fold path → decline. So it is a REACH-GAP ENABLEMENT (Java folds and
answers `[[10 true]]`; Go declines), same family as the LEFT buried box but WITHOUT null-extension — NOT a
demolition of a working name-model producer. The oracle is therefore (a) the dual-window cross-agreement
`ordinal == name-model` on the PRODUCED ordinal plan (it validates that reading the reconstructed
positional seed by ordinal windows equals reading it by name-model qualified keys — a property of the
ordinal plan, so it needs NO separate name-model plan and is available despite the e2e decline) for SEED
NAME-KEY-FAITHFULNESS, plus (b) Java 4.12.11.0 rows for CORRECTNESS. INNER-first still holds (no
null-extension → strictly simpler than LEFT), but the risk is HIGHER than "pure demolition with a built-in
row oracle" — there is no Go name-model row baseline; correctness rests on Java + the dual-window layout
certificate. Pinned as a reach-gap decline sentinel today (rfc173_s4_buriedbox_inner_exists_fdb_test.go).
**EMPIRICAL CORRECTION #2 (the buried-leaf-windowing design does NOT apply to an INNER box — it FLATTENS).**
An implementation attempt (executor `legIsOrdinalSafe`/`reconstructFoldStep1Seed` widening + translator
AXIS-1 enclosure lift) revealed, via instrumentation, that once AXIS-1 un-encloses a bare INNER box leg so
it births ordinal, `SelectMergeRule` FLATTENS it into the fold — the fold SelectExpression becomes a
4-quantifier `[ForEach, ForEach, ForEach, Existential]` (p, q, r flat), NOT the opaque
`[ForEach(box), ForEach(r), Existential]` the buried-leaf-windowing design assumed.
`implementJoinWithExistential` handles EXACTLY 2 ForEach legs + 1 Existential, so the 4-quantifier flatten
is unmatched → no plan → 0AF00 (the same decline, deeper cause). ROOT: an INNER box is NOT merge-opaque
(unlike c5a/c5b's FULL/OUTER boxes, which stay one quantifier BECAUSE `SelectMergeRule` cannot merge an
outer join); the buried-leaf windowing (`.Legs` sub-windows on an OPAQUE box QOV) is inherently an
OPAQUE-box mechanism. So the slices are MIS-PARTITIONED: the ordinalized INNER buried box is the **N-WAY
FLAT FOLD** (`≥3 ForEach + Existential` — the `:4539` `translateJoinWithExists` N-way residual Graefe noted
as "separately blocked: arity≠2"), which needs `implementJoinWithExistential` GENERALIZED to N legs; the
buried-leaf windowing applies to an OUTER (opaque) box leg (the `:2908/:3033` case proper). This FLIPS the
tractability read: the OUTER/opaque box (windowing, stays 3-quantifier) is the more direct slice; the INNER
box (flattens → N-way) requires the N-way fold generalization. The impl attempt was reverted; the finding
stands. Re-scoping pending Graefe design steer. (3) LEFT buried-box FOLLOW-ON —
closes the :3043 `0AF00` reach gap (Java answers `(a JOIN b) LEFT JOIN c` under projected EXISTS), higher
value + higher risk: needs NULL-EXTENSION in the reconstructed seed (the null-supplying leg NULL-filled;
c5b's nested-LEFT-in-INNER proved the executor null-supplies through the positional birth) and has NO Go
name-model oracle (it's `0AF00` today) → validate against Java 4.12.11.0 directly, not a dual-window
differential. (4) FULL/RIGHT stay declined (buildExistentialJoinSelect:3032, implementJoinWithExistential
:1780 — a FULL semi-join can't carry the drain). Java-alignment note: Java keeps both source aliases in a
SINGLE FlatMap and resolves leg refs by NAME (Go's two-level NLJ→FlatMap is the divergence), so the
ordinal seed is a Go-side positional adaptation of the two-level split, not a Java construct — Java
confirms the ROWS the fold must produce, not the mechanism; the correctness anchor is the cross-agreement
(ordinal == name-model/Java rows), which is why INNER-first (oracle available) is the proving ground.

#### REVISED DESIGN (the actual mechanism — Graefe re-scope, re-ACK pending): N-WAY FLAT EXISTENTIAL GENERALIZATION
> **LANDED at `bc54d6300`, four-gate CLOSED** (Graefe impl-ACK + @claude + Torvalds + codex). Plus the
> executor FOUNDATION `76985b2be`, which is **LIVE and LOAD-BEARING — NOT dormant** despite its commit
> message: the N-way fold's left-deep chain reads each accumulated-inner box through EXACTLY the plan-level
> buried-leaf windowing that commit added (`planBuriedLegConcat` / `legIsOrdinalSafe` NLJ arm /
> `reconstructFoldStep1Seed` `.Legs`). **GRAEFE CORRECTION (must-not-lose): the pivot's claim below — "no
> `.Legs` windowing for the fold / windowing retracted / stays on the opaque-OUTER-box path" — was WRONG.**
> The user's INNER box flattens (no single opaque leg), but the left-deep 2-ary NLJ chain creates its OWN
> accumulated-inner boxes that DO need `.Legs` sub-windows to split the buried legs into flat `P,Q,R` windows
> over the top merged row. Graefe's ORIGINAL three-part-flip (executor windowing) was the accurate design; the
> pivot over-corrected. A future reviewer must NOT delete `76985b2be` as "dead/dormant code" — it is the
> foundation the chain wires. `implementNWayJoinWithExistential` plans the `[ForEach×N, Existential]` fold via
> the recursive left-deep chain below; sentinel 0AF00→rows, discriminating pin `[[7 true],[8 false]]` (guards
> last-leg-wins), NOT-EXISTS pin `[20]`, 4-leg discriminating pin (depth-3 windowing), red-first two-site
> coupling verified, full suite green. Closes the projected-EXISTS fold AND the WHERE-EXISTS arity≠2 residual.
> codex P1 (correlated JOIN-inner EXISTS silent-wrong) fail-closed; the PRE-EXISTING 2-leg twin is filed
> (Known gaps) as a scheduled high-priority follow-on. OUTER buried box (FULL drain / LEFT null-extension)
> remains deferred.
The windowing direction above is CORRECT for the fold (Graefe correction — see the LANDED banner: the
pivot's "retracted" was wrong; the chain's accumulated-inner boxes use it). Verified against the code:
`SelectMergeRule` merges
INNER/CROSS but NEVER outer (`rule_select_merge.go:81` gates on `!sel.ChildrenAsSet()` — inner-equivalence
only; `:97` skips `IsNullOnEmpty`/outer quantifiers). So a bare INNER box under a projected-EXISTS fold is
MERGEABLE → it dissolves into flat top-level ForEach legs; the fold SelectExpression becomes
`[ForEach(p), ForEach(q), ForEach(r), Existential]` (N ForEach + 1 Existential). The dispatch
(`rule_implement_nested_loop_join.go:46-54`) matches EXACTLY `len==3` (2 ForEach + 1 Existential) or `==2`,
so the 4-quantifier flatten matches NO arm → `0AF00`. **The real target is generalizing the fold to N ForEach
legs**, not windowing an opaque leg an INNER box does not have.

**The two "blocked" narrowings do NOT wall the flat case (Graefe re-analysis):**
- **"Arity drift"** (`translateJoinWithExists:4644-4648`) was about a NESTED-CLUSTER leg — a 2-leg opaque
  CONCAT whose windows would disagree with post-flatten arity. The flat N-way fold has NO nested concat: p,
  q, r are flat SINGLE-SOURCE ForEach legs, each its own quantifier. The drift concern is a DIFFERENT
  sub-shape (the opaque-box windowing case, which belongs to OUTER boxes) and does not apply to flat arity>2.
- **"Indistinguishable correlations"** (existential-alias-collision, `:4664`) is a real but SOLVABLE
  constraint: the binding-keyed leg discipline (`Q$DUP`, `sourceBinding`/`legBinding`) already distinguishes
  N legs; the fold keys each of the N legs by its binding exactly as the 2-leg gated flatten does (`:4727`).

**Mechanism (compose existing pieces, don't re-derive):**
- **Step-1 N-way inner join:** `implementJoinWithExistential` builds a single 2-ary NLJ
  (`NewRecordQueryNestedLoopJoinPlan`, `:2004`) as the FlatMap outer. N legs need a LEFT-DEEP N-way NLJ
  chain. The N-way inner-join implementation ALREADY EXISTS — the gathered inner cluster
  (`translateGatheredInnerCluster`, `rfc173_ordinal_seed.go:389`) produces flat N-way selects that plan
  today; compose that N-way inner join with the existential FlatMap rather than re-derive an N-way NLJ.
- **Seed:** `reconstructFoldStep1Seed` (`:1120`) already iterates a leg SLICE — generalizing 2→N is
  mechanical. **[SUPERSEDED — Graefe correction: this "NO `.Legs` windowing" claim was WRONG.]** The
  as-built recursive chain DOES emit `.Legs` sub-windows: each chain level's `accPlan` is an accumulated
  ordinal BOX (a nested NLJ), and `reconstructFoldStep1Seed`→`planBuriedLegConcat` walks it to split its
  buried leaves into flat `P,Q,R` windows over the top merged row (the `.Legs` on the box QOV type that
  `finalizeSeedWindows` splices). A single top-level scan leg gets its own top-QOV window (no `.Legs`), but
  the box legs the chain creates NEED `.Legs`. See the LANDED banner.
- **Anchoring/rebase:** `mergedOuterLegAliases` (`:2020-2026`) is ALREADY N-aware (anchors the top-level
  aliases PLUS every buried alias); the below-FOD `rebaseOuterLegRefsOrdinal` threads through the N-way
  merged positional row.
- **Dispatch:** relax `:46-54` to admit `≥3 ForEach + 1 Existential` (or an N-ForEach + Existential arm).

**Two-site coupling (revised):** the arity relaxation spans the translator
(`buildExistentialJoinSelect`/`translateJoinWithExists` no longer restricted to arity-2) ↔ the executor
(`implementJoinWithExistential` N-leg dispatch + N-way step-1 chain). Red-first certificate: toggling EITHER
the dispatch arity OR the seed N-generalization back to 2 reddens.

**Sequencing:** the **3-FLAT-LEG case FIRST** — that IS the sentinel (`(p JOIN q) JOIN r` flattens to p,q,r),
the minimal extension of the working 2-leg fold; then arbitrary N. **One change closes TWO residuals:** the
projected-EXISTS fold (`buildExistentialJoinSelect`, after the INNER box flattens) AND the WHERE-EXISTS
flatten (`translateJoinWithExists` arity≠2). **Correctness gate (unchanged from the re-ACK, matters MORE for
N-way — more legs = more leg-bind ambiguity, the `NewAnchoredJoinRecord` last-leg-wins hazard):** a
DISCRIMINATING buried-leg fixture (a `p`-only projected column + a `p`-only EXISTS-correlated column, fixture
rows where a mis-bind to q/r FLIPS the answer) + JAVA-DERIVED expected rows (run the conformance server, not
hand-derived). **OUTER buried box stays DEFERRED** — FULL is structurally declined for the semi-join
(`:3032`/`:1780`, can't carry the drain); LEFT needs null-extension + above-join-WHERE placement. Lower value,
harder; not the next slice.

**IMPL PROGRESS (empirical refinement, design-ACK'd path, impl NOT yet landed — clean handoff state):**
Restored AXIS-1 (the enclosure lift in `buildExistentialJoinSelect` via `existsLegBirthsPositional`),
confirmed it fires and flattens the box to `[ForEach×N, Existential]` — then reverted it (inert alone until
the executor N-leg dispatch lands; don't ship a translator half that changes nothing observable). Two
empirical facts that NARROW the remaining work and de-risk the path:
  1. **A flat N-way INNER join PLANS today** — `SELECT p.v FROM p, q, r WHERE …` and
     `SELECT p.v FROM (p JOIN q) JOIN r` both return rows. So there is NO N-way-inner-join machinery to
     build (the earlier "the NLJ rule is 2-leg, so flat N-way is unplannable" worry was FALSE — some
     rule already decomposes a flat ≥3-ForEach inner select to binary NLJs). The ONLY gap is the N-way
     existential WRAP.
  2. **The existential wrap is the entire gap** — `SELECT p.v, EXISTS(…) FROM p, q, r WHERE …` (flat comma
     3-way) declines 0AF00 identically to the buried-box explicit-join form. So the shape to fix is
     `[ForEach×N, Existential]`, independent of comma-vs-explicit-join surface.
REMAINING IMPL (the actual next-context work): (a) RESTORE AXIS-1 (the `existsLegBirthsPositional` enclosure
lift — it survives the pivot, only the windowing was retracted); (b) relax the dispatch
(`rule_implement_nested_loop_join.go:46-54`) to route `≥N ForEach + trailing Existential`; (c) generalize
`implementJoinWithExistential`'s step-1 (currently a single 2-ary NLJ at `:2004`) and `reconstructFoldStep1Seed`
(`:1120`, iterates a 2-elem leg slice) to N legs — **[as-built: a left-deep 2-ary NLJ CHAIN with `.Legs`
windowing on each accumulated inner, NOT a reuse of external flat-N-way planning; the earlier "reuse rather
than a chain" framing was superseded by the impl]**; (d) the discriminating buried-leg fixture + Java-derived
rows; (e) the two-site
coupling certificate (AXIS-1 ↔ executor N-leg dispatch, red-first on toggling either back to 2); (f) four-gate.
The sentinel `rfc173_s4_buriedbox_inner_exists_fdb_test.go` flips 0AF00→rows when it lands.

**CONCRETE IMPL APPROACH — the RECURSIVE LEFT-DEEP CHAIN (reconciles the windowing; verified against the
planner constraints):** two further empirical facts pin the mechanism precisely:
  - The GROUPING escape does NOT work: `SELECT s.v, EXISTS(…) FROM (SELECT … FROM p,q,r …) AS s` DECLINES
    0AF00 — the planner INLINES the derived table back to the flat `[ForEach×N, Existential]`. So the N-way
    generalization is UNAVOIDABLE; the shape cannot be reduced to the working single-source fold by wrapping.
  - The step-1 N-way inner join is NOT built by reusing a pre-planned flat select (implementJoinWithExistential
    has only the N leg PLANS, not a memoized inner group). It is a LEFT-DEEP NLJ CHAIN, built RECURSIVELY:
    `step1([l0..lk]) = NLJ(step1([l0..lk-1]), lkPlan, preds, INNER, accAlias, lkAlias, levelSeed)`. The KEY
    insight: the accumulated inner `step1([l0..lk-1])` is itself an NLJ whose merged row is read by the next
    level POSITIONALLY through its `[Start,Width)` buried-leaf windows — i.e. it is exactly a "gated box leg"
    to the next level. So each level's `levelSeed` is `reconstructFoldStep1Seed([accPlan, lkPlan], …)` with
    the SAME `.Legs`-windowing (`planBuriedLegConcat` walking `accPlan`'s NLJ tree, `legIsOrdinalSafe` admitting
    the accumulated NLJ) that the reverted c5a/c5b-style windowing built. THE WINDOWING CODE IS NOT DEAD — it
    was applied to the wrong SHAPE (a single box leg that flattens); its correct home is the chain's
    accumulated inner. RESTORE `planBuriedLegConcat` + the `legIsOrdinalSafe` NLJ arm + `reconstructFoldStep1Seed`
    `.Legs` generalization, and drive them from the recursive chain.
  - Per-level predicate placement: put each join predicate at the SHALLOWEST level where all its referenced
    legs are bound; a predicate referencing a leg buried in the accumulated inner rebases to that leg's
    accumulated window (`rebaseOuterLegRefsOrdinal`, the SAME below-FOD rebase). Simplest-correct first: all
    join preds on the TOP level rebased to the full merged windows (cross-product-then-filter — correct, the
    cost model can push later); pin the discriminating fixture to prove correctness regardless of placement.
  - Step-2 (the existential FlatMap) is unchanged in STRUCTURE — it wraps the top-level N-way merged row;
    `mergedOuterLegAliases` is already N-aware and `rebaseOuterLegRefsOrdinal` already resolves buried
    exist-pred refs through the top windows.

### PER-LEG-WINDOW REBASE (channel 1 + channel 2) — LANDED + four-gate-reviewed; Step B coupling FOUND
The multi-alias-under-EXISTS ordinalization substrate is built + reviewed:
- **Channel 1** (translator): `ordinalSlotInLegWindow` resolves a qualified outer ref WITHIN its
  `rt.Legs` window (was flat first-match, qualifier-dropped) — four-gate-clean (43ef8a70d + hardening).
- **Channel 2 Step A** (values + executor): the mixed-seed branch of `OrdinalSeedLegWindows` +
  `unnestMixedSeedSpans` now emit the per-buried-leaf box windows (shared `finalizeSeedWindows` /
  `mergedLegsOfSpans`), so a box outer's dup-named columns disambiguate — four-gate-clean, behavior-
  preserving + dormant behind the guard, all `mergedType.Legs` consumers traced inert.
- **3-way cross-agreement fixture** (`TestRFC173S4_ThreeWayBoxCrossAgreement`): channel-1 ↔
  channel-2-values ↔ channel-2-executor agree on every box-leaf absolute slot.

**Step B (the guard lift) is COUPLED to an enclosure site — the deep finding.** The c5a target
(`(a FULL OUTER b), a.arr AS x WHERE EXISTS(…c.col…)`) is UNNEST-under-EXISTS (top join Inner, FULL
OUTER buried in the unnest's outer), so it is NOT blocked by the JOIN-under-EXISTS FULL+EXISTS reject
(buildExistentialJoinSelect:2924, which checks the EXISTS join's own kind). It reaches
`unnestExistsSeedSafe`. BUT `translateUnnestJoin` sets `inInnerCluster=true`, and the `:167` gate poison
("an OUTER box that is a LEG of an enclosing … unnest flatten stays name-model") makes the FULL OUTER
box OUTER a name-model Datum, not a positional producer. `unnestOrdinalSeed` bakes it POSITIONALLY, so
lifting `unnestExistsSeedSafe` alone ships a positional seed over a name-model box row → the
`adaptLegPositional` zero-match tripwire (Graefe's Q3). So Step B requires FIRST lifting the `:167`
enclosure poison for the box-under-unnest case (make the box outer ordinal/positional) — one of the
enclosure-site slices in the worklist above. Step B is NOT a standalone flip; it is entangled with the
:167 enclosure ordinalization. The substrate (channels 1+2) is correct + necessary regardless — it is
the layout the box-under-unnest needs once the box outer births positional.

**Step B DESIGN-ACK'd (ACK-WITH-REFINEMENT) — the COUPLED TWO-AXIS FLIP.** NOT a blanket `:1074`
lift (that NAKs: R1's :5117-5119 warns a broad lift breaks the multi-source INNER cluster
`FROM A,B,A.arr AS x` — it would gate that cluster ordinal while the seed still declines it, wrong
rows; also breaks the fallback rebuild :1530 and the chained-unnest path :1136). The ACK'd form is a
SURGICAL, COUPLED flip of TWO axes gated by ONE predicate:
  - `boxGatesFresh(j.Left)` — a NEW predicate mirroring `existsOuterGatesFresh` (:86-99): the input is
    an OUTER-join box that gates via `ordinalWedgeGateDecide` with enclosure forced FALSE (the ONE gate
    authority — no parallel re-derivation, query-engine ruling condition 4). Excludes JoinInner (the R1
    multi-source hazard).
  - AXIS 1 — box-outer enclosure at the `:1356` `translateRef(j.Left)` ONLY: `t.inInnerCluster =
    prevEnclosure || !boxGatesFresh(j.Left)` (keep `prevEnclosure ||` so an already-enclosed unnest
    keeps the box name-model, matching the seed's own `!prevEnclosure` gate at :1473). This lets the
    FULL box birth a POSITIONAL row (its ordinalJoinBirth NULL-fills the FULL drain; adaptLegPositional
    flows it through — traced sound).
  - AXIS 2 — `unnestExistsSeedSafe` (:864): admit the multi-alias box: `len(outerBoundAliases)==1 ||
    boxGatesFresh(left)`.
  - MUST flip TOGETHER in ONE commit via the SAME `boxGatesFresh` predicate — either half alone is
    broken (seed-gate-only → positional seed over name-model box → adaptLegPositional zero-match;
    :1356-only → name-model builder over a positional box). The B1-certificate coupled-flip discipline.
Refinements to close in the slice: (a) the array collection is name-addressed dotted (`"O.TAGS"` at
:1398-1426), resolving via the coexistence dotted bridge — the e2e MUST assert the unnest YIELDS
element rows, not just plan shape. (b) `legsOfGatedJoin` marks NEITHER FULL leg null-supplying
(:246-247, booked) — row values correct (NLJ + nil-leg binding), but columns typed NOT NULL vs Java's
all-FULL-columns-nullable; CLOSE it (mark both FULL legs record-level nullable, :489-501) OR pin
answer-invariant. (c) rewrite `TestRFC173Item2C5a_MultiAliasOuterDeclines` — its assertion INVERTS
anchored→ordinal, its doc block ("the decline is correct") stops being true.
E2E (FDB, real rows + plan): dup-named cols across A,B; a null-supplied SURVIVING row (A row no B
match, A.arr non-empty); `WHERE EXISTS` AND `WHERE NOT EXISTS` correlating on a QUALIFIED ref to the
null-supplied dup-named leg; assert exact rows + correct-leg bind + ordinal seed shape; a NEGATIVE pin
that `FROM A,B,A.arr AS x` (multi-source INNER cluster) stays name-model (the R1 hazard). Re-request
impl-ACK on the diff.

**Step B IMPLEMENTED + e2e-VERIFIED + impl-review round applied.** The coupled flip landed:
`boxGatesFresh` (rfc173_cluster_gate.go, the one gate authority widened to FULL, JoinInner excluded);
AXIS 2 in `unnestExistsSeedSafe` (`|| t.boxGatesFresh(left)`); AXIS 1 at the box-outer `translateRef`.

**Impl-review round (three gates): Graefe NAK→fixed, @claude NAK→fixed, Torvalds ACK.** Graefe caught a
SILENT WRONG-ANSWER: AXIS 1 gated on `boxGatesFresh` alone admits LEFT/RIGHT boxes, but only a FULL box
takes the ordinal seed (`clusterArity==1`; LEFT/RIGHT are preserved-side+1 ≥ 2). A LEFT/RIGHT box would
birth POSITIONAL while its seed stayed name-model → the name-model builder reads the box by absent
qualified LEG.COL keys → wrong rows (`(FOA LEFT FOB), FOA.arr WHERE EXISTS(…FOA.K…)` → 0 rows, should
be {7,8}; reproduced RED — loud unresolvable-field). FIX: AXIS 1 now gates on `boxOuterBirthsPositional`
= `clusterArity==1 && boxGatesFresh && unnestExistsSeedSafe` — the box births positional EXACTLY when
the seed ordinalizes it (couples the two axes on the seed's ACTUAL condition, folding the EXISTS
scope-collision guard). @claude caught that the R1 negative unit test was MASKED by `clusterArity` (an
INNER cluster's seed declines via arity regardless of the JoinInner guard) — added
`TestRFC173Item2C5a_BoxGatePredicates`, a direct white-box pin of `boxGatesFresh` (FULL/LEFT/RIGHT true,
INNER false) and `boxOuterBirthsPositional` (FULL true, LEFT/RIGHT/INNER false), so deleting the guard
or re-widening AXIS 1 turns red. Torvalds: extracted the thrice-copied fresh-cluster probe into
`gatesAsFreshCluster`. Added e2e pins: the LEFT regression (correct rows) and an IS-NULL-on-FULL-leg
probe (Graefe Q5 — the NOT-NULL type marker must not constant-fold `FOB.K IS NULL` to false; answer
{7,8} confirms answer-invariance for a null-CONSUMING predicate, not just equality-NULL propagation).
Pins: `TestRFC173Item2C5a_MultiAliasOuterGatesOrdinal` (the FULL box now seeds ORDINAL + carries
windows — the old `…Declines` inverted per refinement c) and `…_MultiSourceInnerClusterStaysNameModel`
(R1 negative). E2E `TestFDB_RFC173S4_C5a_FullOuterUnnestExists`: all rows correct across
EXISTS/NOT-EXISTS on BOTH the null-supplied leg (Q1/Q2 — the correct-leg-bind discriminators) and the
present leg (Q3/Q4), plus the multi-source INNER negative (R1). Refinement (a) closed — the unnest
YIELDS element rows {7,8}. Refinement (b) closed by PINNING ANSWER-INVARIANT (not the type change):
the FULL null-supplied leg's value is nil via the NLJ regardless of the NOT-NULL type marker, so the
`FOB.K`-correlated EXISTS evaluates correctly (documented at legsOfGatedJoin).

**FOLLOW-UP FINDING (clustered-leg FULL box — gate over-breadth, one level deeper).** Post-ACK
probing of the dimension @claude flagged (clustered-leg FULL box) DEMONSTRATED a loud regression:
`clusterArity(FULL)==1` UNCONDITIONALLY, so `(A JOIN B) FULL OUTER C` under unnest-under-EXISTS also
gated + birthed positional — but the positional seed concats only the RIGHTMOST leg's columns (`(FOA
JOIN FOD) FULL OUTER FOB` births only FOB's `[BID,K]`); the clustered leg's BURIED leaves (`FOD.K`)
are not windowed → `field "FOD.K" not resolvable ... malformed plan`. Pre-Step-B this shape stayed
name-model (3 aliases → declined) and answered correctly, so Step B's gate was too broad — the SAME
over-breadth class as the LEFT/RIGHT AXIS-1 finding, one level deeper. FIX (same shape as the LEFT/RIGHT
fix — narrow the gate): `boxGatesFresh` now EXCLUDES a box whose leg EXPOSES A BURIED JOIN
(`legExposesBuriedJoin`), so a clustered-leg FULL box stays name-model (correct via qualified keys).
The predicate PEELS the transparent wrappers the gate recurses through (LogicalFilter, non-scalar
LogicalProject) before checking for a join — a shallow `*LogicalJoin` type check MISSES `Filter(A JOIN
B) FULL OUTER C` (codex P2), and a `clusterArity>=2` proxy MISSES a nested FULL box leg (`A FULL B FULL
C` — clusterArity(FULL)==1) AND over-excludes an opaque derived-table CTE; the structural peel catches
both join cases while admitting opaque single-namespace legs (scan, aggregate/union, derived CTE).
Buried-leaf ordinalization under a FULL box is deferred to a follow-up item-3 slice. Fail-closed (loud
malformed plan, not silent wrong rows) confirms the decoupling tripwire works. Pins:
`TestRFC173Item2C5a_BoxGatePredicates` clustered-leg + Filter(join)/Project(join)/nested-FULL cases (all
boxGatesFresh/boxOuterBirthsPositional false) + the e2e `CLUSTERED/buried-leaf-FULL-box-stays-name-model`
(correct rows via name-model, red-first as malformed). Review round: Torvalds ACK, @claude ACK
(recommended the transparency-peel hardening), codex P2 (the wrapper slip) — all folded in.

**c5b LANDED (clustered-INNER FULL box ordinalizes — the buried-leaf machinery ALREADY WORKED).** The
"deferred item-3 slice" above turned out NOT to need new executor infra. A probe with
`legExposesBuriedJoin` lifted proved that `(A JOIN B) FULL OUTER C, A.arr AS x WHERE EXISTS(…FOD.K…)`
(buried FOD.K INSIDE EXISTS) resolves CORRECTLY — the box's positional birth concats the clustered
leg's buried columns and the executor below-FOD hoist resolves the buried ref by its [Start,Width)
window (channels 1+2). The earlier `[BID K]` "birth gap" was actually FOLLOW-UP FINDING #2 (the
outer-conjunct issue — the probe used `WHERE FOD.K=200`, outside EXISTS). So `legExposesBuriedJoin` was
an OVER-conservative defensive exclusion. c5b RELAXES it: renamed `legExposesBuriedOuterBox`, excludes
only a buried OUTER-box leg (Kind != JoinInner — nested LEFT/RIGHT/FULL, unverified), ADMITS a buried
INNER-cluster leg. The outer-conjunct clustered case is covered by the FINDING-#2 narrowing. Pins:
`BoxGatePredicates` inverted (clustered-INNER admits; Filter/Project(LEFT-box) + nested-FULL still
excluded) + e2e `c5b/clustered-buried-exists-resolves` and `c5b/clustered-buried-correct-leg` (the
`FOD.K>150` conjunct discriminates: a mis-bind to FOA.K=100/FOB.K=NULL → 0 rows). This unblocks the
buried-gated-box residency of :2908/:3033 (shared machinery).

**c5b review round (Graefe/Torvalds/@claude/codex).** Graefe PROBED a nested OUTER box buried INSIDE the
admitted INNER cluster (`((A LEFT B) JOIN C) FULL OUTER D`) with null-supply discriminators — it
ordinalizes CORRECTLY (the machinery recurses; the executor null-supplies through the positional
birth). So the shallow peel is CORRECT, NOT over-broad: `legExposesBuriedOuterBox` stays NON-recursive
(pinned by BoxGatePredicates `nested-OUTER-in-INNER` admit). codex P2: a WRAPPED join (`Filter/Project`
over a join) is admitted by the peel but `ordinalLegType` records buried bounds ONLY for a DIRECT
LogicalJoin leg → no windows → malformed; FIXED by peel-remembering (a peeled join excludes regardless
of kind; SQL-unreachable anyway). @claude (blocking): the {7,8} e2e is over-determined (name-model
yields it too), so added `TestRFC173Item2C5b_ClusteredBoxSeedsOrdinal` — a white-box `AnchoredJoin==false`
+ windows pin so a seed-gate/AXIS-1 revert (not touching boxGatesFresh) turns red. Graefe recs folded:
the FIRST buried leaf (FOA.K) discriminator e2e; the shallow-peel comment. Torvalds: doc wording (the
peel STOPS at the first join, does not "bottom out") + ACK. The nested-LEFT-in-INNER null-supply e2e is
a Graefe-recommended follow-on (the shape is unit-pinned as admitted).

**codex P2 (round 2, fixed): the outer-conjunct regression had a NON-EXISTS sibling.** FINDING #2's
narrowing was set ONLY in the EXISTS path (translateUnnestExistsFilter), but a FULL box under a PLAIN
(non-EXISTS) unnest with a WHERE on a box leg — `(a FULL b), a.arr AS x WHERE a.col=V` — ALSO
ordinalizes (AXIS 1 fires regardless of EXISTS) and merges the conjunct via the name-model rebase →
malformed. This was a LATENT c5a regression (the scan-legged shape, verified red) that c5b's clustered
admission surfaced. FIX: generalized the flag `unnestExistsOuterConjunctOnBoxLeg` → `unnestOuterConjunctOnBoxLeg`,
set it in BOTH the EXISTS and the non-EXISTS filter-over-unnest merge (translateFilter), and moved its
check in unnestExistsSeedSafe BEFORE the `!unnestUnderExistential` early return so it declines in either
path. Pins: e2e `NONEXISTS-CONJUNCT/scan-box-stays-name-model` + `.../clustered-box-stays-name-model`
(both red-first as malformed); the OuterConjunctNarrowing unit test now loops underExists={true,false}.

**codex P2 (round 3, fixed): a WRAPPED join buried INSIDE an admitted INNER cluster.** legExposesBuriedOuterBox
returned false at the top INNER join WITHOUT recursing, so `(Filter(A JOIN B) JOIN C) FULL OUTER D` —
a Filter-wrapped sub-join nested in the admitted INNER cluster — slipped through, but buriedLegBounds
records windows only for a DIRECT LogicalJoin leg → A/B get no windows → malformed. FIX: the INNER-admit
arm now recurses via hasWrappedBuriedJoin, which excludes a wrapped join at ANY depth while ADMITTING a
bare nested join (buriedLegBounds recurses through bare joins — Graefe verified `((A LEFT B) JOIN C)`
ordinalizes correctly). Pin: BoxGatePredicates `wrapped-join-in-INNER` exclude case (the bare
`nested-OUTER-in-INNER` admit pin stays green). SQL-unreachable, defensive — same family as round 1.

**codex P2 (round 4, fixed): the outer-conjunct narrowing was BYPASSED by the gathered path.** The
FINDING #2 narrowing (below) declines an ordinal box to name-model in `unnestExistsSeedSafe` — the BINARY
seed gate. But `translateGatheredUnnestCluster` is tried FIRST in `translateUnnestJoin`, and it handles a
FULL box as ONE OPAQUE leg. A box with NO shared leg-column name (`(A FULL B), a.arr WHERE a.col=V`,
DISTINCT A/B columns) does NOT trip the gathered path's name-ambiguous decline, so it gathers
positionally BEFORE the seed gate consults the flag → the box-leg conjunct merges by name with no per-leg
window → `field "a.col" not resolvable … ordinal -1` (malformed). The earlier NONEXISTS-CONJUNCT pins used
SHARED-column tables (FOA/FOB share K), which decline the gathered path on the dup name → binary → flag
honored, masking the gap (a SHARED-vs-DISTINCT-column dimension the pins missed). FIX: the gathered
OUTER-box arm declines to name-model when `unnestOuterConjunctOnBoxLeg` is set, coupling it to the binary
seed gate so the two paths decline in lockstep. An INNER cluster is NOT declined (each leg keeps its own
window → a leg conjunct resolves through the gather). Pins: e2e NONEXISTS-CONJUNCT/`noshare-fullbox-first-leg`
+ `/second-leg` (both red-first malformed) + `/inner-cluster-leg-resolves`.

**Review round 5 (@claude NAK — test completeness; Graefe + Torvalds ACK; codex clean — all folded).** The
round-4 fix was CORRECT (all four gates verified the logic + the airtight lockstep coupling), but pinned
only e2e, and the over-decline dimension was VACUOUS: the `inner-cluster-leg-resolves` rows {7,8} are
OVER-DETERMINED (the name-model fallback yields them too), so an INNER over-decline ships GREEN
(@claude + Graefe both mutation-proved this independently). FIX: added the white-box
`TestRFC173W5_Gathered_OuterConjunctCoupling` (on the `newDisjointUnnestTranslator` disjoint-column
harness) that pins the DECISION, not the rows — flag SET → OUTER FULL/LEFT box DECLINES (nil); flag CLEAR
→ gathers an ORDINAL seed (`AnchoredJoin==false`); flag SET → INNER cluster STILL gathers ordinal. Both
directions red-first-verified by mutation (delete the fix → the OUTER assertion reddens; over-decline the
INNER arm → the INNER assertion reddens — the exact mutation that previously left the suite green). The
`inner-cluster-leg-resolves` e2e comment no longer over-claims the over-decline-guard role (the white-box
pin owns it); Torvalds nit folded (the site comment says "non-EXISTS" — the gathered path is unreachable
under EXISTS). Non-blocking follow-ons carried forward: the nested-LEFT-in-INNER null-supply e2e, and the
positional-conjunct BAKE for BOTH entry points (binary + gathered) as FINDING #2's eventual deep fix.

**FOLLOW-UP FINDING #2 (outer WHERE conjunct on a box leg — a SHIPPED c5a regression).** Dimensional
probing (the c5a e2e only tested box-leg refs INSIDE EXISTS) found that a regular NON-EXISTS WHERE
conjunct referencing a box leg on an ORDINAL FULL box under unnest under EXISTS
(`(a FULL OUTER b), a.arr AS x WHERE a.col = V AND EXISTS(…)`) → `field "a.col" not resolvable
(ordinal -1, row columns [b's cols]) — malformed plan`. VERIFIED a regression: pre-c5a (parent
0d5ea3dcd, via worktree) the shape was name-model and answered correctly. ROOT CAUSE: the non-EXISTS
conjunct is merged into the unnest SELECT, where the box's output is NAME-KEYED (coexistence Datum) —
a positional bake there does NOT reach the executor's positional row the way an EXISTS-inner ref does
(the below-FOD hoist rebases those). An attempted ordinal rebase (rebaseUnnestOuterLegPredicateOrdinal
on the merged conjunct) turned the loud malformed plan into SILENT 0 rows (strictly worse) — reverted.
FIX (narrowing, same family as LEFT/RIGHT + clustered-leg): `unnestExistsSeedSafe` declines the
MULTI-alias box to name-model when the enclosing filter has a non-EXISTS conjunct on a box leg
(`unnestExistsOuterConjunctOnBoxLeg`, set in translateUnnestExistsFilter) — correct via qualified keys.
Single-source outers are UNAFFECTED (pristine prefix resolves a bare conjunct). Pins:
`OUTER-CONJUNCT/box-leg-stays-name-model` (red-first as malformed) + `OUTER-CONJUNCT/single-source-ok`.
FULL positional ordinalization of such a conjunct (routing unnest-SELECT box-leg predicates through the
executor positional mechanism) is the deferred follow-on.

**FINDING (name-model oracle carve-out for dup-named box seeds).** The §5 dual-window differential is
NOT a valid gate for this shape: the name-model oracle resolves dup-named box columns LAST-LEG-WINS
(the name-keyed Datum cannot keep two same-named legs distinct — the exact conflation RFC-173
eliminates). So the oracle reads a QUALIFIED first-leg ref (`FOA.K`) as the last leg's K and DIVERGES
from the positional path — while the positional path resolves each leg by its [Start,Width) window.
This is not a bug in the flip; it is the c4 §7 observable (correct rows alone don't distinguish
ordinal from name — the name model is the broken reference here). The e2e turns it into a POSITIVE
proof: toggling the oracle CHANGING the Q3 answer proves the positional seed fired (a name-model
revert would make them agree → red), and the asymmetry (oracle agrees on the LAST-leg Q2, diverges on
the FIRST-leg Q3) precisely characterizes the conflation. The name-model BUILDER (the pre-flip native
path) is correct here via qualified keys — so there is NO latent master wrong-answer; only the
test-only oracle mechanism conflates a positional dup-named seed, an enumerated carve-out.

## S4 R2 — gathered multi-source unnest GROUP BY ordinalization (LANDED: 43871b83b)

The first producer-retiring increment against R2 (multi-source lateral unnest, the :1528 producer):
a gathered multi-source unnest GROUP BY now ORDINALIZES for the plain-leg AS class instead of
declining to the name model.

### The bug (a shipped wrong answer)
`SELECT EL, COUNT(*) FROM WSRC, WSRC.WARR AS EL, WAUX GROUP BY EL` (disjoint columns → the
name-ambiguity gate does NOT decline it) GATHERS, but the positional seed exposes only BARE column
names. A grouped element reference is a correlated `FieldValue{Field:"EL", Child:QOV}` that qualifies
to the shadow-qualified "EL.EL" key — which the seed lacks — so `GROUP BY EL` grouped every row under
NULL. The seed CANNOT carry the shadow key: it is a strict positional contract (a duplicate field
panics the span run invariant "run ordinals must be 0..width-1 ascending").

### The fix (Graefe #1: "the collapse preserves the named projection the group-by consumes")
Keep the positional seed INTERNAL; place a NAMED-PROJECTION layer above it
(`wrapGatheredForGroupBy`, rfc173_w5_unnest_gather.go). Two load-bearing realizations, each reached
by falsifying the alternatives:
- it MUST be a `LogicalProjectionExpression`, NOT a `SelectExpression` — `SelectMergeRule` fuses a
  Select-over-Select and drops the shadow key (a Select is a `RelationalExpressionWithPredicates`
  merge target; a projection is not, and `ProjectionElim`/`ProjectionMerge` don't fire on a
  non-identity projection-over-join);
- its field VALUES NAME-read the inner seed (`FieldValue(QOV(inner), col)`, unpinned) so they do NOT
  re-trigger the positional-seed ordinal-join birth (`ContainsBakedOrdinal` false) — the projection
  RC can therefore carry the bare + `ALIAS.COL` + "EL.EL"/"EL.O" shadow keys the grouped read
  resolves against.
`underAggregate` (formerly the decline stopgap) now TRIGGERS the wrap; Graefe WITHDREW his "must
remove the flag" condition since it ordinalizes rather than entrenching the name model.

### Derivation — 4 falsified shortcuts (do NOT re-attempt)
1. executor nested-descent (the original R1-design premise): the group key is an un-rebased NAME
   ref, not `ofOrdinal(merge,i)` — nothing to descend.
2. value-collapse (rewrite the group key to `QOV(EL)`): `QOV` over the post-join merged row returns
   the WHOLE ROW, and it breaks every name-model group-by.
3. shadow key placed IN the positional seed (`unnestSeedInnerFields`): panics the span run invariant.
4. fused-Select projection layer: `SelectMergeRule` dissolves it; the shadow key is lost.

### The eligibility gate (four-gate review — codex found 4 edges, each declined to name-model)
`gatheredGroupByWrapEligible` — checked BEFORE leg translation (so an ineligible shape declines
without double-registering a leg's uncorrelated scalar subquery) — narrows the wrap to the
FAITHFULLY-COVERED plain-leg AS class. Declined to the name-model residual (correct rows, each pinned
by an e2e test):
- **BOX leg** (`leg.op` a `LogicalJoin`): the qualified twins would key the box binding over the
  concat type, not the leaf-source keys (`WAUX.WV`) a buried-leaf operand references → silent NULL.
  [Graefe blocking NAK / codex P1]
- **element/AT alias SHADOWS an outer column**: the bare "WV.WV" shadow read GetByName-resolves the
  OUTER column, not the element → wrong group. [codex P1] — INITIALLY declined; NOW ORDINALIZED by
  the positional element-first binding (see the roadmap's "positional shadow binding — ✅ LANDED").
- **AT-only unnest** (no AS): the SAME class — for `WSRC.WARR AT O`, `u.Alias` DEFAULTS to the array
  column "WARR" (diagnostic-confirmed), so the grouped ordinal qualifies as `WARR.O`. [codex P2] —
  INITIALLY declined; NOW ORDINALIZED by the same positional binding (the wrap re-exposes the ordinal
  under both bare "O" and the "WARR.O" shadow). The `u.Alias == ""` check is dead code (no reachable
  grouped gather has an empty Alias; a truly anonymous unnest declines earlier at
  unnestSeedInnerFields) and now only guards the offset arithmetic.
- **decline-after-leg-translation** double-registers a leg's scalar subquery — fixed by moving the
  eligibility check before leg translation. [codex P2]
Gate tally: Torvalds ACK; Graefe ACK (blocking cleared — the box-leg gate is EXACTLY coincident with
the failure class, both silent-NULL sources fire iff `leg.op` is a join); @claude ACK; codex
convergent over the four rounds.

### Follow-up roadmap (each removes a decline arm; all done → REMOVE `underAggregate`)
- **box-leg leaf-source qualification — ✅ LANDED (5b5630c55, four-gate-clean)**: the wrap
  RECURSIVELY walks a box leg's buried leaves (`emitLeafKeys` via `t.legsOfGatedJoin` — the SAME
  traversal `addBuriedBakeWindows` uses to populate `legTypes`, the structural symmetry is the
  correctness proof) and keys each operand by its OWN leaf alias over its `leafTyp` columns
  (`WAUX.WV`), NAME-read over the seed. The box-leg decline is removed; a buried-leaf bare-name
  duplicate is still declined by the name-ambiguity gate (which iterates the box's WHOLE concat, so
  buried duplicates ARE seen — the earlier "checks only top-level legs" concern was wrong, the
  concat covers them). Tested: box-leg grouped `SUM(WAUX.WV)` ordinalizes with correct rows AND a
  LEFT-box NO-MATCH null-extension (S=NULL). Graefe/Torvalds/@claude ACK, codex clean. Producer #2
  (:1528) now retires for BOTH plain-leg and box-leg grouped gathers. FOLLOW-UP coverage (TODO):
  nested box (box-in-box) and 3+-buried-leaf shapes are covered by the invariant (twin ≡ bare
  name-read) but unexercised — pin them.
- **positional shadow binding — ✅ LANDED (the shadow-collision AND AT-only decline arms, together)**:
  the wrap now binds EVERY re-exposed key POSITIONALLY (`ofOrdinal` over the seed's own type), so
  the two former decline classes both ordinalize. Root cause: when the element AS alias shadows an
  outer column of the same bare name (`WSRC, WAUX, WSRC.WARR AS WV GROUP BY WV`, WAUX has a WV
  column) the ORDINAL SEED carries BOTH under the bare name "WV" (ordinalJoinSeedFields does not
  shadow; the name-ambiguity gate checks leg-vs-leg only). A NAME-read of "WV" resolves the FIRST
  match = the OUTER WAUX.WV → WRONG rows (measured WV=5/6/7). The fix, three parts:
  * **ELEMENT-FIRST**: identify the element/ordinal seed fields via `fieldValueReferencesInner`
    (their VALUE reads the Explode inner correlation — a QOV(inner) for a scalar element, ofOrdinal-
    over-QOV(inner) for with-ordinality) and bind them by their OWN slots FIRST, so the bare name and
    the "EL.EL"/"EL.O" shadow twins WIN the element over any same-named outer column.
  * **POSITIONAL LEAF KEYS**: each leaf-source operand (`WAUX.WV`) binds at its own seed slot =
    `legStart + leafOffset + colIdx`, where `legStart` is the running sum of preceding legs' run
    widths PLUS `elemCount` once past the unnest's FROM position (`unnestPos`). This is
    order-INDEPENDENT: it is correct whether the outer precedes the element (disjoint) OR the element
    precedes the outer (ENCLOSED) — a name-read only worked in the disjoint order. `leafOffset` is
    box-concat-relative and composes: a box leg recurses its buried leaves at the SAME box `legStart`.
  * **POSITIONAL PASS-THROUGH**: remaining bare names bind by slot; a cross-leg duplicate keeps the
    FROM-order first match (the name-model's first-match discipline).
  * **Option (a) (shadow IN the seed) was falsified** — the outer bare column is a REAL leg column
    still needed qualified; dropping its seed field shifts every span window. Shadowing lives in the
    WRAP's namespace, not the seed. **Pinning is safe** — a LogicalProjectionExpression is not a join,
    so a baked ordinal here does NOT fire newOrdinalJoinBirth (empirically confirmed).
  * **AT-only falls out for free**: with no AS the element alias defaults to the ARRAY COLUMN
    ("WARR"), so the grouped ordinal qualifies as "WARR.O" — which the wrap now re-exposes as the
    ordinal's own slot under both the bare "O" and the "WARR.O" shadow. Same class, same fix.
  Tests (all plan-asserted, correct rows): disjoint shadow (element wins bare "WV"=7/8, WAUX.WV
  stays reachable as SUM=18), ENCLOSED shadow (element before the leg — the order the positional
  leaf offset exists for), AT-only GROUP BY O. `gatheredGroupByWrapEligible` now declines ONLY on an
  underivable leg type (needed for the offset arithmetic).
- **N-way — ✅ PINNED (95aadac66, test-only)**: a 3-plain-leg disjoint gather (WSRC,
  WSRC.WARR AS EL, WAUX, GW) with a grouped aggregate over the THIRD leg (SUM(GW.V)) ordinalizes
  (EL=7/8, S=2997), plan-asserted. The wrap already handled N legs; this pins it.
- **global-aggregate element/leaf OPERAND — ✅ LANDED (Graefe/@claude review catch)**: a GLOBAL
  aggregate (no GROUP BY) whose OPERAND references a column — `SUM(EL)`, `SUM(WAUX.WV)` over a
  gathered multi-source unnest — previously skipped the wrap (the trigger was `len(GroupKeys) > 0`)
  and name-read the bare positional seed → NULL (measured `SUM(EL)`=NULL, should be 45). This was
  PRE-EXISTING (present at base, both reviewers verified) — the same positional-seed-name-read defect
  the wrap cures, on the global path. Fix: `translateAggregate` now also triggers the wrap when
  `aggregateOperandReferencesColumn(a)` (any operand that is not COUNT(*)/a constant). A global
  COUNT(*) references nothing → keeps the flat seed (no wrap), which also preserves the
  duplicate-FROM-alias raw-gather shape exactly. Tests: `SUM(EL)`=45 and `SUM(WAUX.WV)`=36 ordinalize
  (1 Project — a global aggregate has no outer-SELECT Project), COUNT(*) stays flat (0 Project).
Producer #2 (:1528) is now retired for the gathered aggregate class across ALL its shapes (plain-leg,
box-leg, N-way, disjoint/enclosed shadow, AT-only, and GLOBAL element/leaf operands); the only
residual decline is an underivable leg type or a cross-leg unqualified-ambiguous reference (the
name-ambiguity gate, a distinct SQL-error concern). The trio demolition remains the R1∧R2∧R3
convergence commit.

### NOT-OUR-BUG (found via codex on the named-projection review) — GENERAL derived-table + GROUP BY
Codex flagged `SELECT E, COUNT(*) FROM (SELECT EL AS E FROM WSRC, WSRC.WARR AS EL, WAUX) AS D GROUP
BY D.E` → `42703: column "E" does not exist` as a regression of this increment. It is NOT this
increment, and NOT W5/unnest/ordinal at all — DIAGNOSTIC-NARROWED to a GENERAL bug: **GROUP BY over
ANY derived table fails 42703**. Proof (variations, all no-unnest):
  - `SELECT SID, COUNT(*) FROM WSRC GROUP BY SID` → OK (base table, no derived);
  - `SELECT SID, COUNT(*) FROM (SELECT SID FROM WSRC) AS D GROUP BY D.SID` → 42703 (simplest
    possible derived table, no alias, no unnest);
  - `SELECT E FROM (SELECT EL AS E FROM …unnest…) AS D` (no GROUP BY) → OK.
So the failing dimension is the pair {derived-table-in-FROM, GROUP BY}, independent of unnest,
alias, or qualification. The wrap never fires for these (`underAggregate=false` in the derived body,
diagnostic-confirmed) and the same 42703 reproduces at `a34e9e21d` (before the wrap existed). This
is a general pre-existing engine gap (grouping over a subquery-in-FROM), tracked separately from
RFC-173 — NOT a residual of the named-projection increment and NOT a blocker for it.

### NOT-OUR-BUG #2 (found via codex on the positional-wrap review) — GENERAL computed QUALIFIED group key
Codex flagged `GROUP BY WAUX.WV + 1` (a COMPUTED expression over a QUALIFIED column) as a wrong-answer
of the shadow increment (`K=<nil>`, "unresolved reference WAUX.WV + 1"). Attribution (probes, all
reverted): it is NOT shadow-specific and NOT gather-specific — **a computed qualified group key
returns NULL for ANY multi-table query**:
  - `SELECT WSRC.SID + 1, COUNT(*) FROM WSRC, WAUX GROUP BY WSRC.SID + 1` → `K=<nil>` (PLAIN join, NO
    unnest/gather at all);
  - the same `WSRC.SID + 1` fails identically on the parent commit's name-read wrap (472a86323);
  - a BARE qualified group key (`GROUP BY WAUX.WV`, no arithmetic) resolves correctly — only the
    COMPUTED form breaks.
So the failing dimension is {computed expression, qualified column reference, GROUP BY}, independent
of unnest/gather/shadow. A general pre-existing planner gap in resolving a computed group key's
qualified inner reference against the aggregate input — tracked separately, NOT a residual of the
positional-wrap increment and NOT a blocker. (Codex's "the patch introduces" framing was mis-attributed
to the shadow context; the bug pre-dates and is broader than any unnest.)
