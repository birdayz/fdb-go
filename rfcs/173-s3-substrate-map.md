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
