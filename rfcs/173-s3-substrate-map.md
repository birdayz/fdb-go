# RFC-173 Slice 3 — Planning Substrate Map (recon, pre-S3)

Produced at the close of Slice 2 (PR #447), the S3 counterpart of
`173-name-burial-inventory.md`. Java tag 4.12.11.0. All anchors verified at
branch `feat/rfc173-slice2-wedge` HEAD ~416183558; re-verify anchors at S3
start (the tree moves).

## 1. Java reference

### FieldValue.java (774 lines) — the multi-accessor FieldPath model
- `:79` class FieldValue; fields `childValue` + `fieldPath` (`:82-85`) — ONE
  node holds a whole path, never chained nodes.
- `:214-217` equalsWithoutChildren → fieldPath.equals; `:220-222` hash.
- `:273-300` resolveFieldPath(inputType, accessors): per-accessor name-or-
  ordinal resolution → ResolvedAccessor.of(field, ordinal).
- `:321-323` ofFields; `:326-332` ofFieldsAndFuseIfPossible (eager collapse
  via fieldPath.withSuffix — the constructor-side twin of the compose rule).
- `:335-343` ofOrdinalNumber(+AndFuseIfPossible).
- `:373+` FieldPath: COMPARATOR by getFieldOrdinals (`:376-377`); equals
  `:411-420` list-equals over ResolvedAccessor; getFieldPrefix `:451`,
  isPrefixOf `:512`, withSuffix `:525-534`, ofSingle `:563`.
- `:604-640` Accessor (unresolved; equals = name AND ordinal — pre-resolution
  form, not memo identity).
- `:645-754` **ResolvedAccessor: equals `:676-685` / hashCode `:687-690` are
  ORDINAL-ONLY** — the node identity S3 flips Go to.

### ComposeFieldValueOverFieldValueRule.java (71 lines)
- Matchers `:47-51` fieldValue(fieldValue(anyValue)); onMatch `:57-69` yields
  FieldValue.ofFields(innerChild, innerPath.withSuffix(outerPath)). Inverse of
  ExpandFusedFieldValueRule — must not co-reside (stack overflow).

### PartitionSelectRule.java (347 lines) — the merge-rebase port target
- Case 1 `:266-273` / Case 2 `:276-282`: resultValue flows UNCHANGED over
  GraphExpansion.builder().
- **Merge case `:283-322`:**
  - `:284-291` joinedResultValue = RecordConstructorValue.ofColumns(
    Column.unnamedOf(QOV(q))...) — POSITIONAL, UNNAMED merged row (no names,
    no dotted keys — the exact opposite of NewReEnumerationAnchoredRecord).
  - `:296-303` TranslationMap.regularBuilder(): per lower alias
    .when(lowerAlias).then(FieldValue.ofOrdinalNumber(QOV(upperQ), index)) —
    the positional cross-boundary rebase.
  - `:307-312` predicate rebase via translateLeafPredicate(translationMap)
    (Go's rebaseBuriedLowerReferences analog).
  - `:314-315` resultValue.translateCorrelations(translationMap) (Go's
    buildUpperResult analog).
- GraphExpansion.java (594 lines): builder → buildSelectWithResultValue.
- SelectExpression.java `:425`, `:801-808`: pullUpAndComposeTranslationMaps /
  nestPullUp (translation-map composition across the matching pull-up).

## 2. Go re-stamp machinery to DELETE

### values/value_anchored_join_record.go (356 lines)
- AnchoredJoinLeg `:10-13`; NewAnchoredJoinRecord `:54-99` (bare+ALIAS.COL+
  dotted-verbatim, AnchoredJoin=true, last-leg-wins `:60-93`).
- NewScalarSubqueryAnchoredRecord `:121-132` (W4-deferred item 1's builder).
- anchoredColumnsByQuantifier `:155-185` (leftmostQOV(SimplifyValue) `:165`,
  name-sorted `:180-183`); leftmostQOV `:192-206`.
- ReEnumerationLeg `:217-220`; reEnumColumn `:226-231`.
- **NewReEnumerationAnchoredRecord `:259-339`** — dotted SRC.COL split
  `:287-294`, SRC.COL + last-occurrence-wins bare emission `:301-333`,
  name-sorted canonical order for interning `:276-277`. DELETE.
- allPrefixedBy `:344-355`.

### cascades/rule_partition_select.go
- OnMatch `:45`; <3 return `:49-51`; all-ForEach `:57-61`.
- Bipartition: computeTransitiveCorrelationOrder `:74` (def `:631`); 2^N loop
  `:87-90`; split `:91-116`; RFC-142 lowerDependsOnUpper `:127-155`; cycle
  `:157-186`; >1-uppers reject `:191-202`.
- isAnchoredJoinResult live-set branch `:243-253`; predicate classification +
  AddMergeSeedAliases `:260-303`; dedupAliases `:309` (def `:947`).
- upperLegs `:400-410`; **buildUpperResult `:426-440`** (calls
  NewReEnumerationAnchoredRecord `:431`; PANIC #1 `:437`); addUpper `:463-479`
  (calls rebaseBuriedLowerReferences `:476`).
- Case 1 `:481-494`; **merge case `:496-561`** (lower re-enum `:514`, PANIC #2
  `:518`, NextMergeAlias stability `:522-546`); Case 2 `:563-587`; Yield `:590`.
- **rebaseBuriedLowerReferences `:882-921`** — dotted-name string surgery;
  carries the S2 baked-node drift panic `:910-914` (keep the tripwire until
  the rewrite deletes the function).
- quantifierMergeSeedLegDeps def `:729` (wired at `:656`).
- Test pins: rule_partition_select_test.go:157,193;
  rfc173_slice2_drift_assert_test.go:179,186,195;
  values/value_anchored_join_record_test.go:251,282,368,403,410.

### Dotted-prefix classifiers (RFC-142 layer, S3 from-scratch rewrite)
- values/value_correlation.go: MergeSeedLegsOfValue `:27-51` (fv.Field[:dot]
  `:47`); leftmostQOVOfValue `:56-70`; GetCorrelatedToOfValue `:83-119`
  (anchored-RC hiding `:96-98`); GetCorrelatedToOfAnchoredJoinLegs `:132-151`.
- predicates/predicate_correlation.go: GetCorrelatedToOfPredicate `:22-53`;
  AddMergeSeedAliases `:69-108`.

## 3. The three W4-deferred items (current shape)

1. **Correlated scalar subquery** (cascades_translator.go):
   translateProjectWithCorrelatedScalar `:3104-3209` (S3 marker `:3107-3110`);
   NewScalarSubqueryAnchoredRecord `:3175-3179`;
   NewSelectExpressionWithJoinType(..., JoinLeftOuter) `:3181-3187`;
   replaceScalarSubqueryRef `:3211-3219` (name-model INNER.SCALARCOL rewrite).
   The ephemeral pre-rewrite LEFT select — ordinalize POST-rewrite.
2. **RewriteOuterJoinRule** (rule_rewrite_outer_join.go, 205 lines): OnMatch
   `:54-180`; gates `:61,:64-73,:79-82,:111-126,:131-141`; post-rewrite shape
   `:149-179` — ON-preds inside innerSelect below the null boundary,
   NamedForEachNullOnEmptyQuantifier `:157-160`, INNER 2-quantifier outer with
   NO predicates carrying sel.GetResultValue(). THIS is what S3 ordinalizes
   (FULL-outer bidirectional null-extension design attaches here, RFC §6
   `:725-740`). preservedProvidedAliases `:190-202`.
3. **Unnest _N emulation** (cascades_translator.go): translateUnnestJoin
   `:935`; anchored-RC builder ~`:1300-1361` (AnchoredJoin=true `:1359`);
   NewOrdinalFieldValue sites `:1331,:1355,:1405,:1412`;
   rewriteUnnestPredicate `:1383-1418`. NewOrdinalFieldValue def
   values/values.go:617-619 → OrdinalFieldName type.go:508-510 ("_"+N).

## 4. Interning tier (folded P3)

- expressions/select.go: EqualsWithoutChildren `:195-219` (alias-aware
  SemanticEqualsUnderAliasMap, ToValuesAliasMap `:206`);
  **InternsAliasAware `:243-256`** (gated to AnchoredJoin RCs; doc `:232-242`
  says widening blocks on THIS migration; S4 widens to all selects);
  HashCodeWithoutChildren `:261-273`.
- expressions/reference.go: Insert `:305-365` (3 tiers; alias-aware MemoEqual
  tier gated at `:358`); aliasAwareInterner `:367-371`; InsertFinal `:382+`
  (`:404`). MemoEqual memo_equal.go:26; AliasMap alias_map.go:7-26,184.
- FieldValue identity flip targets: values/values.go:203 (Resolved),
  `:217-219` (ResolvedAccessor{Ordinal int} — WIDEN to path per the
  representation ruling; immutability convention pin at `:209-216`),
  resolveOrdinal `:576-595`, NewFieldValueOfOrdinal `:638+`, oracle bridges
  `:233,:242-249`; map_field_values.go:251-277 (baked=(name,ordinal) `:274-276`,
  lazy=name `:277` — S3 flips); semantic_hash.go:107-117.
- P3 spike (branch feat/rfc173-p3-bijection-interning):
  reference.go:374-394 InternShadowObserver; reference_p3_shadow_test.go;
  p3_intern_shadow_corpus_test.go (observed/wouldDedup ≈259/9391; the dark→
  live delta assertion is S3's gap to close).
- Baseline pins in-tree: partition_select_interning_baseline_test.go:88
  (task counts 8999/30593), :115 (gate), :155/:185 (NextMergeAlias).

## 5. S2 machinery S3 inherits/removes
- rfc173_cluster_gate.go: ordinalWedgeGate `:44` / Decide `:53`, clusterArity
  `:240` (algebra `:263-304`); the drift tripwires (SelectMergeRule +
  rebaseBuriedLowerReferences:910-914) guard the window until N-way flips.
- rfc173_ordinal_seed.go: buildOrdinalJoinResultValue `:113`,
  bakeGatedJoinPredicates `:167`, ordinalLegColumns join-leg panic `:97`
  (single-accessor seed; S3 widens to multi-accessor FieldPaths).

## Cross-cutting facts (sequencing is the RFC's)
- The re-stamp trio (NewReEnumerationAnchoredRecord / buildUpperResult /
  rebaseBuriedLowerReferences) maps 1:1 onto Java
  PartitionSelectRule.java:283-322 (ofColumns-unnamed + TranslationMap →
  ofOrdinalNumber). Go has NO positional pullUpResultColumns/TranslationMap
  equivalent in the partition rule today — the rebase is dotted-name string
  surgery.
- ResolvedAccessor widening (single Ordinal → path) is the representation
  ruling's prerequisite for porting the compose rule.

## S2 residuals inherited by S3 (Graefe notes at the S2 close)
- LAZY analog of the codex P1 (b5ff0d4b7): an UNPUSHED single-leg lazy
  conjunct over a leg that BOTH the folded RV and every baked predicate
  dropped would bind zero-width — fails LOUDLY (OrdinalResolutionError),
  never silently, and requires pushdown not to fire. Re-examine when S3
  widens the reference model.
- The two evaluateCorrelated `return bound, nil` arms (recorded W3
  borderline): a baked field access over a non-OrdinalRow non-map binding
  bypasses bakedNameReadGuard — unreachable under the S2 wedge; re-examine at
  gate widening.
- Unification watch-item (Torvalds + Graefe at the post-merge fold,
  51e3327ca): a FrontierPinned and an unpinned baked node with equal
  (field, ordinal) are identity/hash-equal by design (the bit is an
  evaluation contract, not a value distinction) yet guard-differently on a
  name-keyed row — memo/CSE interning across the two shapes could silently
  swap loud for quiet. Only reachable when a plan is already buggy (the
  shapes never co-occur in one tree today: pinned nodes live in join
  seeds/pullup copies, unpinned in recursive-CTE wrap projections). The bit
  and the guard both die in S4; re-check the co-occurrence claim whenever S3
  widens where baked nodes can appear.

## W2 port detail (TranslationMap recon at W1 close; all anchors verified)

### Java mechanics the port must reproduce
- TranslationMap.java:39-73 contract: containsSourceAlias / applyTranslationFunction
  (sourceAlias, leafValue) → Value / definesOnlyIdentities. RegularTranslationMap.java:42-140
  immutable alias→TranslationFunction map; builder `.when(alias).then(fn)` (:145-228);
  `rebaseWithAliasMap` (:100-108) implements plain Value.rebase (Value.java:334-336).
- Application: Value.translateCorrelations (Value.java:347-368) = replaceLeavesMaybe with
  visitNewLeaves=false (TreeLike.java:242-262 — replacement leaves recorded in an identity
  set, never re-translated). Go already has the exact analog: values.ReplaceLeavesOnceMaybe
  (replace.go:93-117) — the correct substrate for the port.
- **THE FUSE-ON-REBUILD MECHANIC (load-bearing): Java FieldValue.withNewChild =
  ofFieldsAndFuseIfPossible (FieldValue.java:278-280, 326-331).** The map swaps a QOV leaf
  for ofOrdinalNumber(QOV(upper), i) — itself a FieldValue — and the PARENT FieldValue's
  rebuild fuses into ONE node: FieldValue(QOV(upper), [(_i, i)] ++ originalPath). Nested
  references collapse automatically; no map composition. Go's replace.go FieldValue arm
  (:351-359) deliberately does NOT fuse (keeps its path over the new child) — the W2 design
  must decide where the fuse lives (generic withChildren arm vs the translation function),
  under the W1 compose-gate constraint (fusing lazy chains changes plan shapes). GRAEFE
  QUESTION #1 for the W2 pre-code consult.
- Merge case (PartitionSelectRule.java:283-322): joined lower RV =
  RecordConstructorValue.ofColumns(Column.unnamedOf(QOV(q))…) — unnamed columns normalize
  to auto-generated names `_0.._n` (Type.java:2617-2660 normalizeFields; isAutoGenerated =
  `_`+numeric :2922; Column.planHash SKIPS auto-generated names, Column.java:93-95). Go's
  OrdinalFieldName ("_N") aligns. Upper preds: replaceLeavesMaybe via translateLeafPredicate
  (:307-312); upper RV = resultValue.translateCorrelations(map) (:314-315).

### Go substrate to reuse (verified semantics)
- values.Replace / ReplaceLeavesMaybe / ReplaceLeavesOnceMaybe (replace.go) — CoW, marker
  preserved through the FieldValue rebuild arm (:351-359 keeps Resolved).
- predicates.ReplaceValues (replace_values.go:17-90) — pointer-stable predicate spine.
- RebaseValue (rebase.go:22-88) — alias-map form only; the port adds the function form.
- MapFieldValues does NOT descend FieldValue children — wrong spine for this; use Replace.

### Consumers that break when the merge RV stops being an AnchoredJoin RC (W2/W3 kill/port list)
1. value_correlation.go:96 GetCorrelatedToOfValue anchored-RC correlation HIDING (without it
   ≥4-way STAR blows the task budget) — the positional merge RV needs an equivalent hiding
   property or an explicit replacement (Java: the merged select's own quantifier structure).
   GRAEFE QUESTION #2: what carries exploration-time correlation hiding post-flip?
2. value_correlation.go:132 GetCorrelatedToOfAnchoredJoinLegs (re-exposure walk) and :27
   MergeSeedLegsOfValue (dotted-prefix leg recovery) — die with the dotted model; replaced by
   real child correlations of ofOrdinalNumber refs.
3. predicate_correlation.go:88 AddMergeSeedAliases — partition classification re-exposure.
4. rule_partition_select.go:243 anchoredSeed keep-alive; :397 re-stamp decision; :736 RFC-142
   buried-dep re-exposure.
5. rule_match_intermediate.go:690-693 leg-alias re-collection during index matching.
6. rule_implement_nested_loop_join.go:786-792 outer-leg alias set FROM DOTTED FIELD NAMES;
   :1543-1558 projected leg-reference rebase.
7. expressions/select.go:252 InternsAliasAware gate (the W2+W3 coupling) + reference.go
   tiers :316/:358/:391/:404.
8. cascades_generator.go:2778 folded-projection column derivation; :2837-2848 SELECT-*
   columns from the anchored RC's BARE field names; :2876-2900 anchoredJoinFirstLeg column
   order — the SQL column-derivation layer must learn the positional merge RV (leg types,
   not field-name scraping).
9. physical_flat_map_wrapper.go:43-54 empty GetCorrelatedToWithoutChildren leans on hiding.
10. Identity/hash anchored-RC arms (map_field_values.go:349, semantic_hash.go:109-114) and
    marker-preservation sites become dead code to delete with the flip.

### Executor N-way surface
- ordinalJoinSpans run-derivation loop (rfc173_ordinal_join.go:91-103) is already N-generic;
  the ONLY 2-way constraints are the len(spans)!=2 check (:106-108) and coverage checks.
- twoLegBinder (:663-696) + pairBinder (streaming_cursors.go:738-744) are the hot-path
  pieces to generalize; birthLegBinder and evaluateOrdinalJoinRow are already N-generic.
- Runtime today: N-way = chained 2-way NLJs; rows via mergeRows (executor.go:2091-2146,
  three key families: bare pass-through, ALIAS.COL, TYPE.COL fallback) — the dying name
  machinery. W2 touch points: mergeRows+qualifyAlias/qualifyTypeFallback, NLJ emission
  sites (streaming_cursors.go:948-960 FULL drain, :1012-1045 hash, :1057-1090 linear,
  :1105-1115 null-padded outer), newNLJCursor (:687), flatMap computeResult
  (flat_map_cursor.go:218-304).
- Lateral-unnest FlatMap is the ONE runtime evaluator of an anchored RC (flat_map_cursor.go:293)
  — S3-W5 territory, must not break in W2.

## W2 pre-code ruling (Graefe, BINDING — issued at W1 close)

- **Q1 fuse placement: the generic rebuild arm** (replace.go FieldValue withChildren), gated
  both-baked — Java's fuse is a property of the rebuild (FieldValue.withNewChild =
  ofFieldsAndFuseIfPossible, :157-160/:326-331), not of the map; the translation function
  returns PLAIN ofOrdinalNumber (PartitionSelectRule.java:302). Both-baked is the DEFINITION
  of fusibility (vacuously always true in Java), self-widens as W2/W3 bake everything.
  Keep the simplifier compose rule too (Java keeps both); pin rebuild(fuse) ≡ compose(chain).
  Port over ReplaceLeavesOnceMaybe (never-retranslate identity set is load-bearing).
- **Q2 correlation hiding: emergent for joins, but the arm does NOT die in W2.** Java needs
  no hiding (the N-legged joined RV lives INSIDE the lower select where leg QOVs are owned;
  the upper RV references only newUpperQuantifier). The dotted model dies PER BIRTH SITE:
  joins W2/W3, scalar-subquery W4, unnest W5 (flat_map_cursor.go:293 is the one runtime
  anchored-RC evaluator — must not break). Hiding + re-exposure twins become inert on the
  join path and are deleted in W5. W2 adds a structural tripwire: join-only corpus contains
  ZERO AnchoredJoin RCs post-flip; task-count baseline proves emergent hiding suffices.
  (Corrects the staging ruling's delete-with-machinery scope — kill list is per-birth-site.)
- **Q3 merge-select recognition: STRUCTURAL, no marker.** RV is an RC whose every field is
  auto-generated-named (_i) with a bare QOV of a distinct owned ForEach quantifier, covering
  the select's quantifiers (PartitionSelectRule.java:284-291's exact shape — unconstructible
  from SQL, so the CTE-NULL pin holds by construction). W2/W3 gate = AnchoredJoin ||
  isPositionalMergeRow; recognition lands in the SAME COMMIT as the merge-case rewrite
  (every intra-PR commit holds the task baseline green); W3's authority flip is separable.
- **Q4 unnamed columns: literal _0.._N via OrdinalFieldName** (byte-identical to Java's
  normalized names). Include them in memo identity/hash/Explain (pure function of position —
  same equivalence either way; Explain inclusion keeps plan-cache keying injective). Java's
  planHash exclusion (Column.java:93-100) ports only if/when a planHash-byte surface exists.
- **Q5 column derivation: W2 scope, fulcrum commit, NO compatibility shim** (a name-keyed
  Datum shadow is two-representations-of-one-fact). cascades_generator.go:2837-2848 is the
  UNNEST path (untouched until W5); what flips is :2876-2900 leg ordering + join SELECT-*
  derivation — read leg types off the merge quantifier's flowed Type.Record.
- **Commit staging within the W2/W3 PR:** (1) TranslationMap + fuse arm, dark; (2) executor
  N-way positional support behind the wedge gate + DisablePositionalEmission extension,
  dark; (3) FULCRUM: rule rewrite + gate recognition + column derivation + executor
  enablement + re-stamp-trio/tripwire deletion, ONE commit, full net green; (4) W3 commits.
- **Extra conditions:** predicate rebase leaf-only via a translateLeafPredicate analog over
  the ReplaceValues spine; max-match-map fused-path verification (no-spurious-sort +
  nested-index-scan pins) is a W2 EXIT CRITERION; keep NextMergeAlias (7.5 ruling) — its
  stability pins survive the rewrite.

## Commit-2 shape note (nested rows, NOT flat concat — recorded before the executor work)

Java's merge row is a RECORD OF RECORDS: `_i` = leg i's WHOLE row
(Column.unnamedOf(QOV(leg))), and a buried reference fuses to the TWO-step
path [(_i, i), (col, j)] into the nested record — this is WHY S3 needs
multi-accessor FieldPaths at all. The S2 wedge's FLAT bare concatenation
(leg columns spread inline, per-leg ordinal runs, ordinalJoinSpans windows)
is the 2-way TRANSLATION-seed shape and stays; the PARTITION-RULE merge shape
is nested and coexists with it (Java likewise: translation RVs are flat/named,
merge RVs are nested/unnamed).

Executor consequence: the physical plans stay BINARY (PartitionSelectRule
recursively reduces N to 2 before implementation; a K-leg merge select is a
tree of binary NLJs whose inner levels were themselves merge-rewritten via
their own TranslationMaps). So commit 2 is NOT "generalize twoLegBinder to N
bindings" — it is a NESTED merge-row birth: evaluate the merge RV per output
pair over the existing two-binding binder (bare QOV field → the bound leg row,
possibly itself a nested Positional; translated ofOrdinal field → positional
read through the inner merged row — descendResolvedPath already descends
nested OrdinalRows), emit Positional = the nested row. New birth site extends
the DisablePositionalEmission oracle registry (standing obligation). The
existing evaluateOrdinalJoinRow per-field evaluation is the right spine.

## FULCRUM ruling (Graefe, BINDING — issued after commits 1+2)

- **Premise correction: the re-stamp trio does NOT die in the fulcrum.** Post-flip,
  name-model >=3-quantifier selects still reach the merge case: dissolved-LEFT
  clusters (poison at rfc173_cluster_gate.go:253-261; RewriteOuterJoinRule:172-178
  carries the anchored RV onto the INNER select) and multi-source unnest (W5).
  The trio dies in W5 with the last anchored birth site (per-birth-site kill,
  same as the W2-Q2 hiding-twins correction). The fulcrum deletes ONLY the
  SelectMergeRule drift assert (rule_select_merge.go:145-152) — its forbidden
  shape becomes legitimate.
- **ONE fulcrum commit, translator flip included:** gate widening (arity >= 2,
  cluster_gate:114-118) + N-leg FLAT seed at the maximal cluster root (Java
  flattens inner joins AT TRANSLATION, QueryVisitor.java:429-434 — do NOT seed
  nested binaries and lean on merge composition) + partition-rule positional ARM
  + interning-gate arm + column derivation (cascades_generator.go:2876-2900) +
  executor spans len!=2 → >=2 (rfc173_ordinal_join.go:106) + assert deletion.
  ARM ROUTING = the existing isAnchoredJoinResult probe (rule_partition_select
  :397): anchored → kept trio arm (poison shapes); else → the Java arm
  (PartitionSelectRule.java:283-322), parent-RV-agnostic. Three-commit staging
  REJECTED: a dark positional arm inside a live rule is a hedge; two arms over
  DISJOINT structurally-recognized populations with a fail-loud boundary is
  per-birth-site staging, not the NAKed dual-mechanism window.
- **SelectMergeRule: NO rewrite.** Already composes via translateValueCorrelations
  → ReplaceLeavesOnceMaybe (rule_decorrelate_values.go:396-434) — the spine that
  carries the commit-1 fuse arm, so baked-over-baked fuses free; lazy-over-RC via
  composeFieldOverConstructor. Add the positive pin (2-way baked child merging
  into 3-way parent, e2e). Consolidating the rule-side TranslationMap type onto
  values.TranslationMap = a SEPARATE mechanical commit in this PR, not fulcrum.
- **Two-shape model confirmed** (flat/named translation seeds; nested/unnamed
  merge RVs). Root-flattening ⟹ an inner join can never be a leg; a JOIN leg at
  translation = an outer-box boundary (W2: FULL only). ordinalLegColumns:97 panic
  LIFTS for GATED join legs (supply the box's flat concat type; ordinalLegType
  already tolerates dup names); REMAINS for name-model join legs.
  ordinalEligible: join legs eligible iff the leg itself gates. Pin
  `(a JOIN b) FULL JOIN c` with an upper a.id reference — the W3b live-catch shape.
- **LEFT boundary = the anchored-RV routing at :397** (no new gate condition):
  LEFT clusters stay translator-poison → dissolved selects arrive anchored →
  trio arm; W4 flips them by making LEFT gate-eligible. KEEP the :910-914
  tripwire re-purposed ("no baked node enters the anchored arm"); ADD its dual:
  the positional arm panics on IsNullOnEmpty quantifiers until W4.

## Fulcrum implementation decisions (WIP notes, delete when the commit lands)
- Gather = DIRECT inner-join nesting only (Java visitSimpleTable's envelope):
  legs are non-join operands; derived-table/filter boundaries stay legs and
  compose later via SelectMergeRule (legal post-assert-deletion). A
  derived-with-join leg keeps the S2 mixed-nesting decline via
  ordinalEligible's cteScope recursion — the whole cluster stays name-model
  (gate pin (b) obligation unchanged). clusterArity keeps its transparent
  model (drift asserts); the gate condition becomes arity >= 2.
- Nested non-inner joins are unreachable in a gated gather (clusterArity
  poisons LEFT; FULL contributes arity 1 as a LEG) — gather treats any
  non-inner nested join defensively as a leg.
- ordinalEligible join-leg arm → eligible iff the leg itself gates
  (ordinalWedgeGateDecide with inInnerCluster forced false — FULL legs root
  fresh clusters; Decide is side-effect-free). ordinalLegColumns for a gated
  join leg = the flat concat of ITS legs' ordinalLegColumns in rv order;
  panic remains for name-model join legs.

## Fulcrum WIP state 2 (supersedes the "derived stays declined" line above)
- FOUND MID-FLIGHT: probe/translate enclosure mismatch. ordinalEligible's join
  arm probes with enclosure FALSE, but leg translation ran ENCLOSED (legs of a
  gated parent hit the inInnerCluster gate check and went name-model) — a
  derived-with-join leg would be declared eligible yet translate name-model.
  RESOLUTION: legs of a GATED parent translate FRESH (inInnerCluster=false) in
  BOTH the gathered N-way path and the binary gated path. Post-fulcrum a
  derived body's inner join gates independently and SelectMergeRule composes
  it into the ordinal parent via translateValueCorrelations + the fuse arm
  (Java's model exactly). The enclosure flag stays TRUE only for the
  name-model parents that survive to W4/W5: existential flattens
  (translateJoinWithExists), recursive CTE, correlated-scalar seeds.
- The failing S2 scope pins are LEGITIMATE flips to rewrite to the new scope
  (not regressions): cascades_translator_test.go:478 (3-way now seeds ordinal
  flat), rfc173_cluster_gate_test.go:115 (flattening-evasion shape now gates,
  arity 4), :236 (FULL box with gated-join leg now gates — the ruling's
  demanded pin shape), :275 (FULL over 3-way leg: leg now gates itself →
  eligible → box gates), :421 WalkArmParity rows for inner_join/full_join/
  cte_nonrecursive_derived_join/cte_scoped_scan_join_body (all eligible now).
  Each rewrite must assert the POSITIVE ordinal behavior, not just delete.
- REMAINING fulcrum steps after the translator: partition-rule positional arm
  (route at isAnchoredJoinResult :397; IsNullOnEmpty tripwire), interning-gate
  arm (AnchoredJoin || IsPositionalMergeRC at select.go:243-256), column
  derivation (cascades_generator.go:2876-2900 — check how the S2 2-way seed
  derives SELECT-* and generalize), SelectMergeRule assert deletion
  (rule_select_merge.go:145-152) + positive compose pin, N-way e2e pins
  (FROM a,b,c rows; FULL-over-gated), full suite + task baseline + dualwindow.
- Also flip: executor TestRFC173S2_SeedAssert_MalformedPanics (values
  AssertOrdinalJoinSeed's exactly-2-runs case — 3 consecutive full runs are
  now LEGAL; keep the <2-runs and gap/reorder/partial-coverage panics) and
  the query-package TestTranslateJoin 3-way expectation (now ordinal flat
  seed with 3 quantifiers — assert the seed shape + baked cross-leg preds).
  Failing set at this WIP point: TestRFC173S2_SeedAssert_MalformedPanics
  (executor), TestRFC173S2_ClusterArity_FlatteningEvasion, TestTranslateJoin,
  TestRFC173S2_WedgeGate_Translation, TestRFC173S2_WalkArmParity (query).

## Fulcrum WIP state 3 — e2e fallout classes (translator+rule halves DONE, executor dispatch remains)
Done and green in the core packages: gate arity>=2, N-leg gather+flat seed
(translateGatheredInnerCluster), legsOfGatedJoin, pairwise dup poison,
ordinalEligible gate-probe arm, ordinalLegColumns gated-leg concat, enclosure
flip for gated parents, partition-rule positionalMergeCase
(rfc173_positional_merge.go: routing at !parentIsMerge, IsNullOnEmpty
tripwire, unnamed _i lower RC, TranslationMap rebase of upper preds+RV),
interning-gate IsPositionalMergeRC arm, SelectMergeRule assert deleted +
positive compose pin, S2 scope pins flipped (gate tests, WalkArmParity,
translator 3-way pin, seed-assert 3-run pin, evasion pin).

Full-suite fallout (sqldriver/dualwindow/conformance), three classes:
1. "baked FieldValue X#i evaluated against a non-positional row context —
   building scan range for PK comparisons": a baked SARG pushed into a leg
   scan gets a NAME-KEYED outer binding. The 3-way physical tree is
   NLJ(NLJ(a,b),c) / correlated scans; the outer binding for correlated inner
   plans must bind POSITIONAL (the S2 flatMap fix generalized) — find where
   scan-range building binds the outer row (executor scan-range builder +
   evaluation context correlation bindings) and route birthed positional rows.
2. "field A.ID / C.NAME not resolvable in the runtime row (ordinal -1, row
   columns [bare concat])": upper DOTTED LAZY references (Projection/Filter
   above the join) over a POSITIONAL row without windows. S2 resolved these
   via spanAwareRow over the pristine 2-leg seed's windows (WindowsOK); the
   N-way TOP select's RV post-merge is a TRANSLATED mixed shape → WindowsOK
   false → no windows → dotted GetByName fails loud on the bare concat.
   Direction: the merged positional row for translated tops needs either
   span recovery (LegTypes-driven windows from the mixed RV — spans per
   contiguous baked run over the same QOV) or dotted resolution routed to
   the dual Datum (name model) for lazy refs during coexistence. Check
   executeProjection/executeFilter's positional-first dispatch conditions.
3. rfc153_matrix FULL-over-gated-join: "A.ID not resolvable, row columns
   [ID FLAG ID A_ID BX ...]" — the FULL box's upper reads a dotted leg ref
   over the box's flat concat (the leg is now a gated join). Same family as
   class 2 (dotted-over-positional), FULL-box flavor.
Also: conformance plan_shape failures include ordinal 4 out-of-range
("PRODUCT_ID ordinal 4, row columns [ID CUSTOMER_ID PRODUCT_ID QTY]") — a
baked ref evaluated against a SINGLE-LEG row where the ordinal was baked
against the MERGED concat (offset not leg-relative): pushdown of a baked
cross-leg pred conjunct into a leg without re-baking — check
PushFilterBelowJoinRule/SARG extraction paths for baked refs (S2 only baked
cross-leg conjuncts precisely to avoid this; the N-way WHERE preds arrive as
ONE And that bakeGatedJoinPredicates bakes per-conjunct — verify the pushdown
of single-leg conjuncts didn't change, and that spanning conjuncts don't get
pushed).

## Fulcrum WIP state 4 — ALL FOUR FALLOUT CLASSES RESOLVED (+3 planner gaps found by the full net)

Every class from WIP state 3 is fixed, each with a red-verified regression pin:

1. **Class 1 (baked SARG vs name-keyed outer)** — root cause: the FlatMap
   merge-RC birth-disable (a W2 P2 stopgap written before the merge shape
   existed) left the outer leg name-keyed while inner-plan SARGs were baked.
   Fix: disable deleted; the merge birth's coexistence Datum puts each leg's
   own DATUM under the `_i` keys (bare-QOV arm in computeResultLegs — the §5
   oracle's exact shape). Pin: TestRFC173S3_MergeBirth_FlatMapBirths (flipped
   from the old decline pin).
2. **Class 4 (concat-relative bake onto single-table quantifiers)** — root
   cause: the WHERE-merge site paired sourceAlias(join.Left) (buried rightmost
   table) with ordinalLegType(join.Left) (whole-subtree concat). Fix:
   gatedJoinLegTypes over legsOfGatedJoin (the seed's own pairing). Pin:
   TestRFC173S3_WhereMergeBakesLegRelative (verified red: ordinal 8 vs 0).
3. **Classes 2+3 (dotted lazy uppers over translated tops / FULL-over-gated)**
   — resolved by SPAN RECOVERY, not per-consumer dispatch: ordinalJoinSpansOf
   generalizes the S2 probe with fused-path resolution through the plan's leg
   RVs (resolveSpanLeaf composes TRANSLATED merge slots — a deeper round's
   rebased refs — into the walk), collectJoinLegRVs recovers merged-away
   aliases from child merge RCs, spliceLegSpans recursively opens box legs
   (FULL-over-gated) with a width guard against box/leaf alias shadowing.
   Consumers keep probing downstreamLegWindows; the FlatMap cursor recovers
   spans at construction so datumFromSpans/oracleNameDatum emit the qualified
   ALIAS.COL keys again (oracleNameDatum is now SPAN-driven — the fused
   field's child QOV is the merge alias, never a user alias). Pins:
   TestRFC173S3_TranslatedTopSpans, TestRFC173S3_SpliceLegSpans, plus
   GatePinA/MultiwayJoinOrder_Nway e2e red→green.
   - Prerequisite found under this: positionalMergeCase's merge slots were
     UNTYPED (Go's Quantifier.GetFlowedObjectValue flows no type, Java's is
     always typed) — legRowTypes recovers each leg's row type from the
     select's own value surfaces and types the merge RC slots.

The full net (54 targets) then surfaced three more fulcrum-induced gaps, all
fixed:

4. **Oracle-side 0 rows on 3-way counts (dualwindow)** — a baked/fused ref
   over an UNBOUND merge quantifier read only the qualified `$M._0` Datum key;
   the merge row's oracle Datum carries `_i` BARE (no qualified form exists
   for a merge quantifier). Fix: baked paths fall through to the bare root
   key in the unbound-QOV Datum arm (lazy refs keep qualified-only — the
   cross-leg last-wins protection). Pin:
   TestFieldValueBaked_OracleUnboundMergeRead_RFC173S3 + the dualwindow
   differential (3 corpus entries were diverging).
5. **MaxTasks blowout on ≥4-way (0AF00)** — the ordinal model's seed and
   translated-upper selects are the successors of AnchoredJoin-marked selects
   but fell to alias-IDENTITY dedup. Fix: values.IsOrdinalJoinRV (every field
   pinned over ≥2 root quantifiers) as the third InternsAliasAware arm.
6. **Unimplementable N-ary cross products (0AF00)** — `FROM a, b, c`, the
   EXISTS body `(SELECT 1 FROM t2, t3, t4 WHERE t2.t1_id = t1.id)`, and the
   spanning-OR join all starved: the Go-only disconnected-lower guard ate the
   only bipartitions those selects have. Fixes, Java-checked
   (PartitionSelectRule.java:122-133 + SelectExpression.java:248): (a)
   DefaultPlannerConfiguration now defers cross products like Java (the
   component-aligned deferral gate was dead code); (b) the guard exempts
   multi-component selects (the deferral already restricted their splits to
   the unavoidable crosses); (c) lower connectivity is judged by ANY select
   conjunct touching two lower aliases (aliasesConnectedByPredicates over the
   full conjunct list — a spanning OR connects; binary equijoins unchanged, so
   the pinned task baselines hold EXACTLY: 11122/45306). Full guard deletion
   (pure Java) was tried and REVERTED: the anchored 4-chain baseline blows
   past 100k tasks — Go's memo cannot yet afford Java's exploration breadth;
   divergence documented at the guard.
   Pins: TestFDB_RFC173_PureCrossProduct (cross + EXISTS-cross),
   TestAliasesConnectedByPredicates (incl. the OR case),
   TestDefaultPlannerConfiguration_JavaDefaults (flipped from ZeroFields),
   conformance join_or_chained_predicates (was the last cross-engine
   divergence).
7. **NLJ merge cursors' Datum (oracle 0 rows on the spanning-OR join)** — the
   NLJ implementation of a merge select emitted mergeRows' flat Datum; the
   rebased upper references read `_i` root keys → all NULL on the oracle side
   (no positional row there). Fix: a positional-merge-RC NLJ swaps in the
   merge-shape Datum (mergeShapeDatum — slot `_i` = leg i's own Datum) at
   EMISSION time, after its own leg-baked predicates ran against the mergeRows
   keys; live and oracle sides identically (not birthActive-gated). The
   commit-2 NLJ pin's "untouched mergeRows" Datum expectation flipped — the
   fulcrum owns the merge Datum story and this is its settlement, same shape
   as the FlatMap side (class 1).
8. **Unnest AS/AT columns silently NULL (found by the widened exploration)** —
   the guard rework exposed a MISSING-EDGE hole: Go's
   Quantifier.GetCorrelatedTo is empty (registered divergence), so a lateral
   unnest's Explode→source dependency (quantifier-level, no predicate) was
   invisible to EVERY bipartition check — components, cycle,
   lower-depends-on-upper. The old predicate-only guard masked it by skipping
   all predicate-less lowers. Fixes: (a) computeTransitiveCorrelationOrder now
   recovers each quantifier's rangesOver Reference correlations to siblings
   (Java's Quantifier.getCorrelatedTo delegation, applied locally); (b) the
   pure-cross exemption additionally requires the lower to union SINGLETON
   components (lowerComponentsAreSingletons) — a multi-alias
   correlation-glued component (unnest + source) never enters a disconnected
   lower; W5 revisits when the unnest machinery goes ordinal. Pins:
   TestTransitiveCorrelationOrder_RangesOverEdges (mechanism) +
   TestFDB_ArrayUnnestOrdinality (behavior, the net that caught it).

## Fulcrum review round 1 (commit 757d64e30): Graefe ACK-with-conditions, Torvalds NAK, codex 2 findings — all addressed
- **Box-leg bake window** (Torvalds P1): bakeGatedJoinPredicates resolved bare
  names by whole-concat FieldIndex first-match — `(o FULL JOIN c) JOIN t ON
  c.price = …` silently baked ORDER's price. legTypes entries are now
  bakeLegType{typ, leafOffset, leafTyp}: names resolve within the rightmost
  LEAF's window (the alias names that leaf, matching sourceAlias) at its
  concat offset. Red-verified pin: TestRFC173S3_BoxLegBakeResolvesLeafLocal.
- **Hash-join fused-pred guard** (codex P1): fieldName declines multi-accessor
  paths — a fused `m._i.col` has no name-keyed hash key; the ≥100-row index
  keyed on its display name came up empty and silently dropped every match.
  Red-verified pin: TestRFC173S3_HashJoinDeclinesFusedPred.
- **Cursor-side splice for pristine seeds** (codex P2): a seed whose leg is a
  gated-join BOX kept the box-alias span, so datumFromSpans qualified the
  whole concat under one alias. The FlatMap cursor now splices DatumSpans (a
  NEW field — the Datum/oracle view) while Spans stays unspliced for the leg
  ADAPTER (the box binding flows the whole concat row). Red-verified pin:
  TestRFC173S3_FlatMapSeedBoxLegDatumSplice.
- **NLJ null-pad merge Datum** (Torvalds P2/Graefe cond. 4): handled rather
  than half-covered — the unmatched-outer emission swaps the merge shape in
  with the empty-map NULL leg (unreachable until LEFT gates in W4; ready).
- **Oracle bare-`_i` fallback tightened to FrontierPinned** (Graefe cond. 3):
  unpinned baked refs (recursive-CTE wrap) keep their historical NULL; pinned
  in TestFieldValueBaked_OracleUnboundMergeRead_RFC173S3's unpinned arm.
- Mojibake repaired (rfc173_cluster_gate.go), IsPositionalMergeRC/IsOrdinalJoinRV
  godoc placement fixed, stale "2-way"/"dark" docs refreshed (Graefe cond.
  1-2, Torvalds P3-5).
- Coverage repairs (Torvalds P6): the SelectMerge positive pin now asserts
  actual composition (child quantifiers spliced, retired alias unreferenced);
  the pure-cross EXISTS pin gained a genuine negative row.
- **@claude round additions**: the 🔴 buried-alias-erasure concern is REFUTED
  at runtime (the buried non-rightmost leg resolves through the spliced
  spans/leaf-qualified Datum — the FULL box's select never needs an `a`
  quantifier) and now explicitly pinned by the ruling's demanded e2e:
  TestFDB_RFC173_FullOverGatedBuriedRef (matched + left-only + right-only
  NULL-extended rows, buried `a.id` in both ON and SELECT). Also applied: the
  FULL-outer DRAIN emission (unmatched-inner) gets the merge-shape swap (all
  three NLJ emission paths now agree); spliceLegSpans carries the same
  defensive depth cap as resolveSpanLeaf; positionalMergeCase's stale
  returns-nil doc fixed (always yields; caller nil-check is defense);
  flat_map_cursor's pre-existing `→` mojibake and the gate file's stale
  "until Slice 3" comments repaired. The TranslationMap consolidation stays
  tracked as the separate mechanical commit (this map, pending ledger).

## S3-W3 commit A — the ordinal-only identity flip (LANDED)
Path element identity narrowed from the coexistence (Field, Ordinal) pair to
Java's ORDINAL-ONLY (ResolvedAccessor.equals = getOrdinal() alone,
FieldValue.java:675-689; list equality :411-420 — verified against source).
Coupled hash: the baked FieldValue hash folds ONLY the ordinal path
("fieldpath:#o…"; the display-name prefix would split the alias-mapped twins
the flip makes equal); lazy keeps the name bucket. The per-step Field
survives on the accessor for the name-model oracle reads
(descendResolvedPath/nameReadRootKey) and Explain rendering — dies with them
in S4. Pin flipped: TestRFC173S3_FusedPath_IdentityHashExplain's diff-name
arm now asserts EQUAL + hash-equal (was UNEQUAL — the refinement that could
only under-dedup). Task baselines held exactly (the pinned chains carry no
name-only twins). Remaining W3: (B) max-match-map fused-path verification
pins (no-spurious-sort EXPLAIN + nested-index-scan-chosen — the W2 exit
criterion; port ExpandFusedFieldValueRule into matching ONLY if they fail);
(C) the mechanical TranslationMap consolidation.

## S3-W3 commit B — max-match-map fused-path verification (the W2 exit criterion)
Probed, triggered, ported: fused-vs-chained max-matching returned NO match
(the probe went red exactly as the ruling anticipated), so
ExpandFusedFieldValueRule is ported into expandValueForMatching — MATCHING
ONLY, Java's exact placement (MaxMatchMapSimplificationRuleSet.java:50;
never the general simplifier, per the compose-co-residence stack-overflow
warning). The stale "doesn't apply to Go's FieldValue model" comment (true
pre-W1) is gone. Pins: rfc173_w3_max_match_fused_test.go — fused-vs-fused,
the NAME-DIVERGENT twin (the identity flip's dedup feeding matching), and
fused-vs-chained (red without the port); e2e
TestFDB_RFC173_SecondaryIndexThroughMerge (secondary-index probe survives
the merge machinery, ORDER BY variant carries exactly one sort — the
pre-fulcrum name model also sorted N-way tops, so "no spurious sort" means
no NEW sort; the §5 single-frontier pin stays the elision guard). Task
baselines held with the expansion in the matching loop. Remaining W3: (C)
the mechanical TranslationMap consolidation.

## S3-W3 review round: codex P2 — full-unchain (Torvalds+Graefe ACK, codex found the deeper axis)
The ExpandFusedFieldValueRule port split only the LAST accessor; a 3+-accessor
fused path's prefix stayed fused, and Go's matcher compares the child before
it could re-split (Java re-explores; Go doesn't) → a 3-accessor fused path
missed a fully-chained candidate. The 2-accessor pin didn't exercise it;
Torvalds and Graefe both ACKed without catching it — codex's specific-scenario
probing did. Fix: expandFusedFieldValue fully unchains in one step (Java's
re-explored end state, direct). Red-verified pin
TestRFC173W3_MaxMatchMap_FusedVsChained_ThreeAccessor.

## S3-W3 delta round: codex found a REGRESSION in the first fix (all-forms emission)
The full-unchain-only fix (d8970ad9e) resolved the 3-accessor CHAINED candidate
but REGRESSED the one-step-split candidate shape FV(FV(m,[_0,_0]),[C]) the
prior two-node split had matched (red-verified: match size 0). This is exactly
the case Graefe flagged as "narrower than Java's member set" and codex flagged
as a capability regression — the two reviewers disagreed on whether it was
reachable. Resolution (moots the disagreement): expandFusedFieldValue now emits
EVERY split form — a p-accessor fused prefix + chained suffix for each p in
[1,n-1] — Java's FULL re-explored member set (p=1 fully chained … p=n-1
one-step split; the fully-fused form is the caller's direct compare). Strictly
safe (more match forms never yield a wrong match), path depth is tiny.
Red-verified pins: TestRFC173W3_MaxMatchMap_FusedVsOneStepSplit +
_ThreeAccessor. Torvalds' stale caller-comment and Graefe's end-state-scoping
nits are subsumed: the doc now describes the full member set. Delta gates:
Torvalds ACK, Graefe ACK (verified the pin red himself), codex regression fixed.

## S3-W3 commit C (TranslationMap consolidation) — RESOLVED: NOT VIABLE as envisioned
@claude's design-concern (round 1): the values-side TranslationMap builder (W2,
2 users) and the pre-existing cascades-side one (99 users) share names/APIs
across packages. Investigated to a definitive conclusion — the consolidation is
architecturally IMPOSSIBLE, not merely deferred:
1. The two types sit on OPPOSITE sides of the import boundary (values cannot
   import cascades), so they cannot merge into ONE type. The cascades-side map
   also carries AliasMap/GetTargetAlias, which live in cascades.
2. The only signature-alignment path — narrow values.TranslationMap's
   ApplyTranslationFunction from Value to LeafValue so cascades.RegularTranslationMap
   satisfies it — is BLOCKED. TranslateCorrelations (translation_map.go:141)
   applies the fn to every ownCorrelationOfLeaf type: {QOV, QuantifiedRecordValue,
   ScalarSubqueryValue, ObjectValue, UnmatchedAggregateValue, ConstantObjectValue}.
   Only QuantifiedObjectValue implements LeafValue (leaf_value.go:27); the other
   FIVE do NOT (verified by grep for RebaseLeaf). So the values-side interface
   MUST accept the broader Value — it cannot be narrowed to LeafValue.
Conclusion: the two TranslationMaps are a LEGITIMATE Go-layering split (a
consequence of the package cycle Java has no equivalent for), not reducible
duplication. No consolidation commit. The builders are namespaced by package
(values.TranslationMapBuilder vs cascades.TranslationMapBuilder) and each builds
its own interface's impl; a same-name collision across packages is idiomatic Go,
not a defect. The RFC's "separate mechanical commit" framing assumed a mergeable
shape the import boundary + the LeafValue/Value domain difference invalidate.
