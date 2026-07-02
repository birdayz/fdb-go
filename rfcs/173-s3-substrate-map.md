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
