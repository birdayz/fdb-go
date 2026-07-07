package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 W5 — the GATHERED multi-source lateral unnest (design-ACK'd,
// flat-at-translation ruling Q1).
//
// `FROM A, B, A.arr AS x [AT o]` translated as Java translates it
// (LogicalOperator.generateCorrelatedFieldAccess + generateSimpleSelect): ONE
// flat select over {QOV(A), QOV(B), ForEach(Explode)} where the Explode's
// collection is a BAKED reference to the OWNING source's own quantifier —
// `ofOrdinal(QOV(A, typA), FieldIndex(ARR))` — a genuine per-source
// correlation every Cascades rule can see. This replaces the name-model
// binary FlatMap-over-merged-outer whose buried dotted read
// (`FieldValue(QOV(rightmost), "A.ARR")`) required the dotted-prefix
// bipartition classifiers; with the per-source edge, bipartition validity is
// EMERGENT (the rangesOver sibling-edge keeps A with its Explode — exactly
// Java's Quantifier.getCorrelatedTo edge; the whole-cluster concat
// alternative was ruled OUT for over-constraining re-enumeration).
//
// The single-source W4c binary seed remains the N=1 path (this builder is
// N>=2 only); the name-model builder remains the fail-open residual for every
// decline (ruling Q5: several ungated/residual compositions work today —
// decline-loud would regress them).

// unnestTrailing is the unnestPos for the ROOT form: the unnest follows every
// plain leg in FROM order (`FROM A, B, A.arr AS x`), so the element fields and
// the Explode quantifier append last.
const unnestTrailing = -1

// translateGatheredUnnestCluster builds the flat (N+1)-quantifier select for
// a multi-source lateral unnest. The caller (translateUnnestJoin) has already
// run the FULL classification/validation gauntlet — metadata, CTE/derived
// rejection, array-field typing, chained-unnest guard, alias-collision (P1)
// and AS/AT-distinct (P2b) rejections — so every input reaching this point is
// a VALID unnest; nil here means DECLINE (the caller falls back to the binary
// name-model path), never an error.
//
// unnestPos is the unnest's FROM position (the number of plain legs BEFORE
// it): the enclosed form (`FROM A, A.arr AS x, B`) places the element
// fields/quantifier mid-list so a `SELECT *` expansion preserves the user's
// FROM order — the seed assert accepts leg runs in any order, and every
// downstream consumer resolves by span offset, not run position.
func (t *cascadesTranslator) translateGatheredUnnestCluster(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	innerCorr values.CorrelationIdentifier,
	elementType values.Type,
	fieldName string,
	unnestPos int,
) expressions.RelationalExpression {
	leftJoin, isJoin := j.Left.(*logical.LogicalJoin)
	if !isJoin {
		return nil // single-source outer — the W4c binary path owns it
	}
	// The left cluster must be a GATED-if-fresh cluster (the enclosure-free
	// probe, side-effect-free Decide — the ordinalEligible pattern): an
	// ungated cluster flows name-model rows the baked collection read cannot
	// consume (ruling Q1 condition iii feeds Q5's fail-open residual).
	prev := t.inInnerCluster
	t.inInnerCluster = false
	d := t.ordinalWedgeGateDecide(leftJoin)
	t.inInnerCluster = prev
	if !d.Gated {
		return nil
	}

	var legs []clusterLeg
	if leftJoin.Kind != logical.JoinInner {
		// A gated OUTER box as the unnest's left (unnest-residual class 1,
		// design ruling 1(i)): the box is ONE OPAQUE leg — its legs are
		// never gathered into the flat inner select (the flat seed has no
		// arm for null-supplying roles; the box quantifier carries them,
		// and its buried leaves stay addressable through the amendment-C
		// bake windows). The unnest is the user's own INNER comma lateral
		// over the box: a null-supplying owner's NULL array explodes to
		// zero rows and drops that padded row — Java's Explode-over-NULL
		// spec (RecordQueryExplodePlan: null collection → List.of()), not
		// a LEFT violation.
		legs = []clusterLeg{clusterLegOf(leftJoin, false)}
	} else {
		legs = t.legsOfGatedJoin(leftJoin)
	}
	fields, legTypes := t.ordinalJoinSeedFields(legs)
	if fields == nil {
		return nil // a leg untranslatable — same decline rule as the seed
	}

	// NAME-AMBIGUOUS decline (fail-open residual): a column name shared by two
	// LEGS (the name model's last-leg-wins bare twin) would resolve
	// DIFFERENTLY over the flat row — the bare-twin class waits for the S4
	// compose direction (ordinal projections). The element/ordinal alias
	// SHADOWING an outer column (the R16 class) is LIFTED (commit 2): the
	// visitor qualifies shadowed bare projections (`WV` → `WV.WV`) and the
	// span windows route the qualified read to the ELEMENT leg — probed green
	// through the gathered path with the correct last-binding-wins semantics.
	// Per RUN, not per map entry: legTypes also carries the amendment-C
	// BURIED windows (each with the whole box CONCAT as its typ) — iterating
	// entries would count a box's columns once per buried leaf and
	// self-collide. One leg = one run = one column set.
	seen := map[string]struct{}{}
	for _, leg := range legs {
		for _, f := range legTypes[leg.binding].typ.Fields {
			n := strings.ToUpper(f.Name)
			if _, dup := seen[n]; dup {
				return nil
			}
			seen[n] = struct{}{}
		}
	}

	// GROUP-BY wrap eligibility — decided HERE, BEFORE any leg is TRANSLATED
	// (translateRef below has side effects: an uncorrelated scalar subquery in a
	// derived/project/filter leg registers on the translator). Declining late — after
	// leg translation — would let the name-model fallback translate the same legs
	// again and double-register/evaluate that subquery. So a grouped gather whose
	// wrap can't faithfully cover the shape declines to name-model here, with the
	// same pre-leg-translation discipline the earlier decline stopgap had.
	if t.underAggregate && !gatheredGroupByWrapEligible(u, legs, legTypes) {
		return nil
	}

	// The OWNING source (unnest-residual class 1, design ruling 1(iii)): the
	// legTypes map IS the owner index — a PLAIN leg's own window keyed by
	// its binding (== UPPER alias for the FROM-order first match; a later
	// duplicate's minted binding is unreachable by name, the item-1
	// first-match discipline), and every BURIED leaf of a box leg via its
	// amendment-C window (bakeCorr = the box quantifier, leafOffset = the
	// leaf's slot in the concat). The collection bakes at the WINDOW:
	// ofOrdinal(QOV(corr, window type), leafOffset + arrIdx) — for a plain
	// leg that degenerates to the old leg-local bake verbatim (offset 0,
	// own type, own binding).
	seg0 := strings.ToUpper(u.Segments[0])
	ownerWindow, isOwner := legTypes[seg0]
	if !isOwner || ownerWindow.typ == nil || ownerWindow.leafTyp == nil || len(u.Segments) < 2 {
		return nil
	}
	// The ROOT column of the collection path: for a single-segment path the
	// classifier's proto-derived name; for a MULTI-SEGMENT path
	// (unnest-residual class 2) the FIRST field segment — the remaining
	// segments ride as a FUSED suffix (Java's lookupNestedField →
	// ofFieldsAndFuseIfPossible shape) and descend the struct value at eval
	// through FieldValue's proto-message arm. Suffix accessors are
	// NAME-addressed (the proto descent resolves by field name); the
	// classifier has already validated every intermediate as a singular
	// record field.
	rootField := strings.ToUpper(fieldName)
	if len(u.Segments) > 2 {
		rootField = strings.ToUpper(u.Segments[1])
	}
	arrIdx, found := ownerWindow.leafTyp.FieldIndex(rootField)
	if !found {
		return nil
	}
	ownerCorr := seg0
	if ownerWindow.bakeCorr != "" {
		ownerCorr = strings.ToUpper(ownerWindow.bakeCorr)
	}
	ownerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(ownerCorr), ownerWindow.typ)
	collection, err := values.NewFieldValueOfOrdinal(ownerQOV, ownerWindow.leafOffset+arrIdx)
	if err != nil {
		return nil
	}
	if len(u.Segments) > 2 {
		suffix := make([]values.ResolvedAccessor, 0, len(u.Segments)-2)
		for _, seg := range u.Segments[2:] {
			// NAME-addressed: the struct descent (FieldValue's proto-message
			// arm) resolves each suffix step by field NAME. Ordinal is the
			// LOUD sentinel -1 — a struct materializes as a proto message, not
			// a positional row, so the ordinal is never consulted; should one
			// ever reach the OrdinalRow descent arm, Get(-1) fails
			// out-of-range (a clean error) rather than silently reading slot 0.
			suffix = append(suffix, values.ResolvedAccessor{Field: strings.ToUpper(seg), Ordinal: -1})
		}
		fused := collection.Resolved.WithSuffix(&values.FieldPath{Accessors: suffix})
		collection = &values.FieldValue{Field: strings.ToUpper(fieldName), Typ: collection.Typ, Child: collection.Child, Resolved: fused}
	}
	// The baked node carries the leg type's field TYPE for the array column;
	// the classifier's proto-derived element type is authoritative for the
	// Explode (ordinalLegType columns are best-effort for derived shapes).
	collection.Typ = values.NewArrayType(true, elementType)

	// Legs translate FRESH (the S3 fulcrum: legs of a GATED parent gate
	// independently); the Explode is one more ordinary quantifier, correlated
	// to its owner by the baked collection itself.
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = false
	quantifiers := make([]expressions.Quantifier, 0, len(legs)+1)
	sourceAliases := make([]string, 0, len(legs)+1)
	for _, leg := range legs {
		ref := t.translateRef(leg.op)
		if ref == nil {
			t.inInnerCluster = prevEnclosure
			return nil
		}
		quantifiers = append(quantifiers, expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(leg.binding), ref,
		))
		sourceAliases = append(sourceAliases, leg.binding)
	}
	t.inInnerCluster = prevEnclosure

	// The unnest's FROM position: quantifier index + seed-field offset (the
	// sum of the preceding legs' run widths). Out-of-range = trailing.
	if unnestPos < 0 || unnestPos > len(legs) {
		unnestPos = len(legs)
	}
	fieldsAt := 0
	for i := 0; i < unnestPos; i++ {
		// BINDING-keyed, matching ordinalJoinSeedFields' map — an alias
		// lookup would nil-miss a duplicate leg's entry and panic on .typ
		// (the seed and this consumer must share ONE key discipline).
		fieldsAt += len(legTypes[legs[i].binding].typ.Fields)
	}

	explode := expressions.NewExplodeExpressionWithOrdinality(collection, u.AtAlias != "")
	explodeQ := expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode))
	quantifiers = append(quantifiers[:unnestPos:unnestPos],
		append([]expressions.Quantifier{explodeQ}, quantifiers[unnestPos:]...)...)
	sourceAliases = append(sourceAliases[:unnestPos:unnestPos],
		append([]string{innerCorr.Name()}, sourceAliases[unnestPos:]...)...)

	// Seed RV: the flat ordinal leg runs + the W4c unnest inner fields (the
	// element/ordinal branch shared with the single-source seed) at the
	// unnest's FROM position. The assert runs only for the full-baked shape
	// (AS+AT); the mixed direct-QOV element and the AT-only partial run
	// legitimately skip it — exactly the W4c rule.
	innerFields, fullBaked, ok := unnestSeedInnerFields(innerCorr, u, elementType)
	if !ok {
		return nil // degenerate no-AS/no-AT — name-model residual
	}
	fields = append(fields[:fieldsAt:fieldsAt],
		append(innerFields, fields[fieldsAt:]...)...)
	rc := values.NewRawRecordConstructorValue(fields...)
	if fullBaked {
		values.AssertOrdinalJoinSeed(rc)
	}

	// Predicates: the cluster root's OWN ON conjunct (gatherInnerClusterPreds
	// deliberately skips its argument's own ON — "the root's own ON is the
	// caller's") + the nested inner joins' ON conjuncts + the unnest join's
	// (a comma join carries none itself, but the ROTATION parks every
	// collected enclosed-spine conjunct here), cross-leg conjuncts baked
	// through the same legTypes the seed used. The root ON may reference the
	// ELEMENT/ordinal alias (that is WHY the rotation parks it at the root),
	// so it takes rewriteUnnestPredicate — the WHERE merge arm's exact
	// treatment: the Explode flows a bare scalar (no-AT) or `_0`/`_1` fields,
	// and an unrewritten `FieldValue(QOV(EL), "EL")` evaluates NIL, silently
	// dropping or misfiltering every row (review finding, pinned). The
	// LEFT-cluster ONs cannot reference the element (SQL scope: bound after
	// them) and stay unrewritten.
	var preds []predicates.QueryPredicate
	// INNER root only: an inner cluster root's ON is WHERE-equivalent and the
	// gather (via gatherInnerClusterPreds' "the root's own ON is the caller's"
	// contract) must hoist it here. A NON-inner root is the class-1 OPAQUE
	// OUTER box: its ON is the outer join's GATING condition, already carried
	// inside the box leg's own translation — hoisting it too would re-apply it
	// as a flat-select filter and silently convert LEFT to INNER (the padded
	// row's NULL-supplied columns fail the re-applied ON and the row drops).
	if leftJoin.Kind == logical.JoinInner {
		if qp, isQP := leftJoin.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
			preds = append(preds, qp)
		}
	}
	if j.OnPredicate != nil {
		if qp, isQP := j.OnPredicate.(predicates.QueryPredicate); isQP {
			preds = append(preds, rewriteUnnestPredicate(qp, u))
		}
	}
	// INNER root only — the SAME opaque-box barrier as the ON-hoist guard
	// above, for the NESTED cluster ONs. A non-inner root is the class-1
	// opaque OUTER box; its whole internal predicate spine (e.g. the nested
	// inner cluster ON of `SRC LEFT JOIN (AUX JOIN AUX2 ON x.XID = y.YID)`)
	// is already enforced inside the box leg's own translation. Re-collecting
	// those here and re-applying them flat over the box's NULL-padded rows
	// fails on the null-supplied slots and silently drops the preserved row
	// (LEFT→INNER — the ON-hoist bug's twin). Only an INNER root's nested ONs
	// are WHERE-equivalent and belong on the flat select.
	if leftJoin.Kind == logical.JoinInner {
		preds = append(preds, gatherInnerClusterPreds(leftJoin)...)
	}
	preds = bakeGatedJoinPredicates(preds, legTypes)

	seedSel := expressions.NewSelectExpressionWithJoinType(
		rc,
		quantifiers,
		preds,
		sourceAliases,
		expressions.JoinInner,
	)
	if !t.underAggregate {
		return seedSel
	}
	// A GROUP BY consumes this gather (and the shape passed the early
	// gatheredGroupByWrapEligible gate above). The positional seed exposes only bare
	// column names, so a grouped `FieldValue{Field:"EL", Child:QOV}` qualifies to the
	// shadow "EL.EL" key the seed lacks and reads NULL (the shipped wrong answer). Keep
	// the positional seed INTERNAL and place a NAMED-PROJECTION layer above it (Java's
	// "the collapse preserves the named projection the group-by consumes"). It MUST be
	// a LogicalProjectionExpression — NOT a SelectExpression, which SelectMergeRule
	// would fuse away, losing the shadow key. The projection NAME-reads the seed
	// (unpinned FieldValue — no positional-seed birth) and re-exposes bare + "EL.EL"
	// shadow keys the grouped element read resolves against. RFC-173 S4.
	return t.wrapGatheredForGroupBy(seedSel, rc, u, elementType, legs, legTypes)
}

// gatheredGroupByWrapEligible reports whether a grouped gather's leg shapes can be
// FAITHFULLY re-exposed by the named-projection wrap — checked BEFORE leg
// translation so an ineligible shape declines to name-model without double side
// effects. FALSE (decline) for:
//   - an AT-only unnest (no AS): the grouped ordinal reference qualifies through the
//     array column (`GROUP BY O` resolves as `WARR.O`, not `O.O`), uncovered;
//   - an element/ordinal AS/AT alias that SHADOWS an outer column of the same bare
//     name: the shadow "WV.WV" would NAME-READ the seed and GetByName resolves the
//     FIRST match (the outer column, not the element) → wrong group.
//
// BOX legs ARE covered (the wrap walks their buried leaves and keys by leaf-source
// alias); a buried-leaf bare-name duplicate is already declined by the caller's
// name-ambiguity gate (which iterates the box's whole concat). Both remaining
// classes keep the working name model until positional shadow binding lands.
func gatheredGroupByWrapEligible(u *logical.LogicalUnnest, legs []clusterLeg, legTypes map[string]bakeLegType) bool {
	if u.Alias == "" {
		return false
	}
	elemAlias := strings.ToUpper(u.Alias)
	atAlias := strings.ToUpper(u.AtAlias)
	for _, leg := range legs {
		lt, ok := legTypes[leg.binding]
		if !ok || lt.typ == nil {
			return false
		}
		// lt.typ is the leg's own type for a plain leg, or the whole CONCAT for a
		// box leg — either way it enumerates every bare column the element/ordinal
		// alias could shadow.
		for _, f := range lt.typ.Fields {
			n := strings.ToUpper(f.Name)
			if n == elemAlias || (atAlias != "" && n == atAlias) {
				return false
			}
		}
	}
	return true
}

// wrapGatheredForGroupBy places a NAMED-PROJECTION layer above the positional
// gathered seed so a GROUP BY over the gather resolves. See the caller for why it
// must be a LogicalProjectionExpression (survives SelectMergeRule) whose values
// NAME-read the seed (unpinned, so no positional-seed birth). The caller has
// already checked gatheredGroupByWrapEligible (before leg translation), so this
// only runs for a faithfully-coverable plain-leg shape.
func (t *cascadesTranslator) wrapGatheredForGroupBy(
	seedSel expressions.RelationalExpression,
	rc *values.RecordConstructorValue,
	u *logical.LogicalUnnest,
	elementType values.Type,
	legs []clusterLeg,
	legTypes map[string]bakeLegType,
) expressions.RelationalExpression {
	innerAlias := values.UniqueCorrelationIdentifier()
	innerQOV := values.NewQuantifiedObjectValue(innerAlias)
	seen := map[string]struct{}{}
	var projected []values.Value
	var aliases []string
	add := func(name string, v values.Value) {
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		projected = append(projected, v)
		aliases = append(aliases, name)
	}
	// Pass through every seed column by NAME (reads the inner seed's output).
	for _, f := range rc.Fields {
		add(f.Name, values.NewFieldValue(innerQOV, f.Name, f.Value.Type()))
	}
	// The ALIAS.COL qualified twins the aggregate OPERANDS need (`SUM(WSRC.SID)`):
	// each LEAF source's columns keyed by that leaf's OWN alias, value = the SAME
	// inner name-read as the bare column (the seed exposes every leaf column by name
	// in FROM order). For a BOX leg (a clustered join consumed as one opaque leg) the
	// operands reference the BURIED leaf aliases (`WAUX.WV`), NOT the box binding — so
	// walk the box's leaves and key each by its own leaf alias/type (leafTyp), never
	// the box concat. The name-ambiguity gate above already declined any cross-leaf
	// (incl. buried) bare-name duplicate, so a name-read is unambiguous.
	var emitLeafKeys func(op logical.LogicalOperator, binding string)
	emitLeafKeys = func(op logical.LogicalOperator, binding string) {
		if bj, isBox := op.(*logical.LogicalJoin); isBox {
			for _, sub := range t.legsOfGatedJoin(bj) {
				emitLeafKeys(sub.op, sub.binding)
			}
			return
		}
		lt, ok := legTypes[binding]
		if !ok || lt.leafTyp == nil {
			return
		}
		alias := strings.ToUpper(binding)
		for _, f := range lt.leafTyp.Fields {
			col := strings.ToUpper(f.Name)
			add(alias+"."+col, values.NewFieldValue(innerQOV, col, f.FieldType))
		}
	}
	for _, leg := range legs {
		emitLeafKeys(leg.op, leg.binding)
	}
	// The SHADOW-qualified element/ordinal keys the grouped read needs — same inner
	// name-read as the bare element, so it resolves the scalar element (not the whole
	// row the falsified value-collapse hit, not the missing "EL.EL"). AT-only unnests
	// (no AS) are declined upstream (gatheredGroupByWrapEligible), so u.Alias is set
	// here and the leg qualifier is always the AS alias.
	if u.Alias != "" {
		el := strings.ToUpper(u.Alias)
		add(el+"."+el, values.NewFieldValue(innerQOV, el, elementType))
		if u.AtAlias != "" {
			at := strings.ToUpper(u.AtAlias)
			add(el+"."+at, values.NewFieldValue(innerQOV, at, values.NotNullInt))
		}
	}
	innerQ := expressions.NamedForEachQuantifier(innerAlias, expressions.InitialOf(seedSel))
	return expressions.NewLogicalProjectionExpressionWithAliases(projected, aliases, innerQ)
}

// gatherLegsWithBuriedUnnest walks j's direct inner-join spine collecting the
// PLAIN legs (FROM order), every nested ON conjunct, and exactly ONE buried
// unnest (a nested `Join(L, Unnest)` — the `FROM A, A.arr AS x, B` enclosed
// class). ok=false when there is no unnest or more than one — the caller then
// leaves the original tree to today's paths. A NON-INNER (or existential-
// rider) nested join is NOT a decline: the walk absorbs that whole subtree as
// ONE OPAQUE plain leg (never decomposing across it), which is exactly what
// makes rotating the remaining inner legs safe — an unnest buried UNDER such
// a subtree is invisible here and stays on the residual path.
func gatherLegsWithBuriedUnnest(j *logical.LogicalJoin) (plainLegs []logical.LogicalOperator, preds []predicates.QueryPredicate, uLeft logical.LogicalOperator, u *logical.LogicalUnnest, unnestPos int, ok bool) {
	ok = true
	var walk func(op logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if !ok {
			return
		}
		nj, isJoin := op.(*logical.LogicalJoin)
		if !isJoin || nj.Kind != logical.JoinInner || len(nj.OnExistsSubqueries) > 0 {
			plainLegs = append(plainLegs, op)
			return
		}
		if un, isUn := nj.Right.(*logical.LogicalUnnest); isUn {
			if u != nil {
				ok = false // a second unnest — out of scope (chained/multi)
				return
			}
			walk(nj.Left)
			u = un
			uLeft = nj.Left
			unnestPos = len(plainLegs) // FROM position: legs before it are walked
			if qp, isQP := nj.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
				preds = append(preds, qp)
			}
			return
		}
		walk(nj.Left)
		walk(nj.Right)
		if qp, isQP := nj.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
			preds = append(preds, qp)
		}
	}
	walk(j)
	if !ok || u == nil {
		return nil, nil, nil, nil, 0, false
	}
	return plainLegs, preds, uLeft, u, unnestPos, true
}

// rotateEnclosedUnnest probes the ENCLOSED unnest class (`FROM A, A.arr AS x,
// B` — the unnest join buried as a LEG of a larger inner cluster) and, when
// it classifies, ROTATES the cluster to the root form the commit-1 builder
// owns: Join(Join(plain legs, FROM order), Unnest). The rotation is
// inner-join-equivalent — the lateral dependency needs only the owner in
// scope, and every collected ON conjunct rides the rebuilt ROOT's
// OnPredicate (a conjunct may reference the ELEMENT/ordinal alias, `... INNER
// JOIN B ON B.K = EL`, which is only in scope at the flat select the builder
// folds the root ON into; an ON-free left also keeps the gate probe on the
// pure comma cluster the commit-1 corpus pinned).
//
// The classification is MINIMAL and fail-open (ok=false — the caller keeps
// the ORIGINAL tree, whose residual path still produces the faithful
// diagnostics), mirroring translateUnnestJoin's gauntlet with DECLINES
// instead of errors. Seed-field order places the element LAST (not at its
// FROM position) — observable only via SELECT-*-over-multi-source, which
// cannot plan today (the banked commit-4 fix target).
func (t *cascadesTranslator) rotateEnclosedUnnest(j *logical.LogicalJoin) (rebuilt *logical.LogicalJoin, u *logical.LogicalUnnest, elementType values.Type, fieldName string, unnestPos int, ok bool) {
	if t.md == nil || j.Kind != logical.JoinInner || len(j.OnExistsSubqueries) > 0 {
		return nil, nil, nil, "", 0, false
	}
	if _, rootUnnest := j.Right.(*logical.LogicalUnnest); rootUnnest {
		return nil, nil, nil, "", 0, false // the root form — translateUnnestJoin owns it
	}
	plainLegs, preds, uLeft, u, unnestPos, gok := gatherLegsWithBuriedUnnest(j)
	if !gok || len(plainLegs) < 2 {
		return nil, nil, nil, "", 0, false
	}

	// Classification against the unnest's OWN scope (uLeft — the sources
	// before it in FROM order). Multi-segment paths pass through: the
	// gathered builder bakes them as fused root+suffix collections
	// (unnest-residual class 2), and the classifier below validates every
	// intermediate segment.
	if len(u.Segments) < 2 {
		return nil, nil, nil, "", 0, false
	}
	outerTable := findOuterScanTable(uLeft, u.Segments[0])
	if outerTable == "" {
		return nil, nil, nil, "", 0, false
	}
	if t.outerSourceIsCTE(outerTable) || outerSourceIsDerivedTable(uLeft, u.Segments[0]) {
		return nil, nil, nil, "", 0, false
	}
	elementType, fieldName, isArray, _ := t.unnestArrayElementType(outerTable, u.Segments[1:])
	if !isArray {
		return nil, nil, nil, "", 0, false
	}
	if containsLateralUnnest(uLeft) {
		return nil, nil, nil, "", 0, false
	}

	newLeft := plainLegs[0]
	for _, leg := range plainLegs[1:] {
		newLeft = logical.NewJoin(newLeft, leg, logical.JoinInner, "")
	}
	if _, isLJ := newLeft.(*logical.LogicalJoin); !isLJ {
		return nil, nil, nil, "", 0, false
	}

	// P1/P2b collision minima against ALL plain legs — the flat select binds
	// every leg alias (including legs AFTER the unnest in FROM order), which
	// is the root-form gauntlet's scope after the rotation. The residual
	// rejects these faithfully when this declines.
	bound := outerBoundAliases(newLeft)
	collide := func(name string) bool {
		if name == "" {
			return false
		}
		if strings.EqualFold(name, sourceAlias(newLeft)) {
			return true
		}
		_, isBound := bound[strings.ToUpper(name)]
		return isBound
	}
	if collide(u.Alias) || collide(u.AtAlias) {
		return nil, nil, nil, "", 0, false
	}
	if u.Alias != "" && u.AtAlias != "" && strings.EqualFold(u.Alias, u.AtAlias) {
		return nil, nil, nil, "", 0, false
	}

	var onPred predicates.QueryPredicate
	if len(preds) > 0 {
		onPred = preds[0]
		if len(preds) > 1 {
			onPred = predicates.NewAnd(preds...)
		}
	}
	return logical.NewJoinWithPredicate(newLeft, u, logical.JoinInner, onPred), u, elementType, fieldName, unnestPos, true
}

// translateEnclosedUnnestGather is the W5 commit-3 dispatch for the enclosed
// unnest class: rotate (rotateEnclosedUnnest), then hand the root form to the
// shared gathered builder. Fail-open — nil falls back to the caller's
// name-model paths.
func (t *cascadesTranslator) translateEnclosedUnnestGather(j *logical.LogicalJoin) expressions.RelationalExpression {
	if t.inInnerCluster || t.unnestUnderExistential {
		return nil
	}
	// The translateFilter probe already built this select — consume it
	// (once) instead of translating the cluster a second time.
	if sel, cached := t.enclosedGatherCache[j]; cached {
		delete(t.enclosedGatherCache, j)
		return sel
	}
	rebuilt, u, elementType, fieldName, unnestPos, ok := t.rotateEnclosedUnnest(j)
	if !ok {
		return nil
	}
	return t.translateGatheredUnnestCluster(rebuilt, u, unnestSourceCorrelation(u), elementType, fieldName, unnestPos)
}
