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
		// A non-EXISTS WHERE conjunct on a box leg
		// (unnestOuterConjunctOnBoxLeg) merges into the name-keyed unnest
		// SELECT. (The flag can also be set by the EXISTS filter path, but the
		// gathered path is unreachable under EXISTS — translateUnnestJoin gates
		// it on !unnestUnderExistential — so at this site the flag always
		// reflects a plain non-EXISTS WHERE.) A box gathered as ONE OPAQUE leg
		// (below) exposes no per-leg
		// window for that box leg, so a positional gather leaves the merged
		// conjunct unresolvable (`field "A.col" not resolvable … ordinal -1` —
		// malformed plan). Decline to the binary name-model path, where the
		// qualified key resolves; the binary seed gate (unnestExistsSeedSafe)
		// declines in the same step, so the two paths stay coupled. This decline is
		// independent of the (retired) declineBoxDup dup-narrowing: a WHERE conjunct on
		// an OPAQUE box leg has no per-leg window to bake into regardless of whether the
		// box's columns collide by name — it is the box-as-one-opaque-leg shape, not a
		// dup, that lacks the window.
		if t.unnestOuterConjunctOnBoxLeg {
			return nil
		}
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

	// EVERY dup column name GATHERS via the RAW seed (NO decline, NO wrap) — shared ACROSS
	// legs (the bare-twin `FROM A, B, A.arr AS x` with A,B both carrying `k`; the box-buried
	// cross-leg `FROM (A FULL C), B, C.arr AS x` with buried A.k dup-named with scan B.k) OR
	// twice WITHIN ONE box's concat (a `(A FULL B)` box carrying both A.k and B.k). Each
	// buried leaf gets its OWN [Offset,Width) window (buriedLegBounds / finalizeSeedWindows,
	// recursing through nested boxes), so a QUALIFIED read routes to its own SLOT, not
	// first-match, uniformly through every outer operator — SELECT / WHERE / GROUP BY /
	// ORDER BY / DISTINCT / cross-leg predicate / buried-element predicate (the qualifier-
	// honoring authority). A within-box dup's DOUBLY-NULL-fill (both same-named leaves NULL
	// on OPPOSITE unmatched rows) resolves through the FULL-NULL substrate, discriminated by
	// disjoint value sets. A BARE ambiguous reference (`SELECT k`, `GROUP BY k`) errors 42702
	// at semantic analysis BEFORE the translator, so only qualified reads reach here — NO dup
	// shape declines. A positional wrap is NOT needed and is actively WRONG: its bare
	// pass-through key first-matches a qualified dup column, dropping every row of
	// `WHERE B.k=200`. (The former declineBoxDup gate — cross-leg-to-box, then within-box —
	// is fully retired; both classes gather uniformly on the same buried-leaf-window +
	// FULL-NULL substrate.)
	// A GROUPED gather (underAggregate) UN-COLLAPSES to the raw per-leg seed (below)
	// — no name-keyed wrap — and the ancestor GROUP-BY POSITIONALLY BAKES its keys /
	// operands over it (translateAggregate: OrdinalSeedLegWindows for leg columns,
	// element-anywhere including the MID-LIST split-run; fieldValueReferencesInner for
	// the element). The grouped bare-twin's qualified dup keys route by slot (not
	// first-match), and the outer WHERE bakes for free (bakeGatedJoinPredicates fires
	// on the SelectExpression). This retires the collapse AND the ALIAS.COL name keys
	// (the name-model residue).

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
	return seedSel
}

// fieldValueReferencesInner reports whether a seed field's VALUE reads the unnest's
// Explode inner correlation (a bare QOV(inner) for a scalar element, or ofOrdinal
// over QOV(inner) for a with-ordinality element/ordinal). It is the seed's OWN
// definition of "which field is the element"; translateAggregate uses it to locate
// the ELEMENT/ordinal field(s) and bake a grouped element read POSITIONALLY at that
// slot (element-first — winning the bare namespace over a same-named outer column,
// the name-model shadow), and slotInGatheredSeed consults the resulting element slots.
func fieldValueReferencesInner(v values.Value, inner values.CorrelationIdentifier) bool {
	switch tv := v.(type) {
	case *values.QuantifiedObjectValue:
		return tv.Correlation == inner
	case *values.FieldValue:
		if qov, ok := tv.Child.(*values.QuantifiedObjectValue); ok {
			return qov.Correlation == inner
		}
	}
	return false
}

// bakeGatheredGroupValue rewrites a GROUP-BY key / aggregate operand Value into a
// POSITIONAL read over the un-collapsed gathered seed. It
// RECURSES the value tree (values.Replace) so a compound operand — `SUM(A.K+B.K)`,
// `SUM(A.K*2)` — bakes each nested FieldValue LEAF that names a seed slot (a
// top-level-only bake would leave a dup column in the operand reading first-match
// = silent wrong rows). Each leaf: a qualified (`FieldValue{Field:"K",Child:QOV("B")}`),
// flat-dotted (`FieldValue{Field:"A.K"}`), or bare/element (`FieldValue{Field:"EL"}`)
// column that names a slot becomes a resolved-ordinal read there; a literal, `*`, or a
// reference the seed doesn't carry passes through unchanged (name-model, resolved
// elsewhere). This is the group-by twin of bakeGatedJoinPredicates — the
// qualifier-honoring positional read that replaces the wrap's name key.
func bakeGatheredGroupValue(v values.Value, windows map[string]values.OrdinalSeedLegWindow, elementSlots map[string]int, seedQOV values.Value) values.Value {
	return values.Replace(v, func(node values.Value) values.Value {
		fv, isFV := node.(*values.FieldValue)
		if !isFV || fv.Resolved != nil {
			return node
		}
		alias, col := "", strings.ToUpper(fv.Field)
		if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
			alias = strings.ToUpper(qov.Correlation.Name())
		} else if dot := strings.IndexByte(col, '.'); dot >= 0 {
			alias, col = col[:dot], col[dot+1:]
		}
		if slot, ok := slotInGatheredSeed(windows, elementSlots, alias, col); ok {
			if baked, err := values.NewFieldValueOfOrdinal(seedQOV, slot); err == nil {
				// The positional read (Resolved ordinal) is authoritative; Field is
				// display-only. Preserve the ORIGINAL reference's display name so the
				// group-by OUTPUT column matches what the SELECT projects (`EL`, `A.K`).
				baked.Field = fv.Field
				return baked
			}
		}
		return node
	})
}

// slotInGatheredSeed resolves a group-by key / operand reference to its flat slot in
// the gathered seed. The ELEMENT wins FIRST (element-first, so an element AS alias
// shadowing a leg column reads the element): its slot is its rc index, located via
// the seed's OWN element predicate (fieldValueReferencesInner) — a single
// distinguished field, NOT a layout window (rc-index↔slot is the
// substrate, no drift). A qualified LEG column routes through its leg's
// [Offset,Width) window from the SHARED authority OrdinalSeedLegWindows (agreeing
// bit-for-bit with the executor spans by the cross-agreement fixture).
func slotInGatheredSeed(windows map[string]values.OrdinalSeedLegWindow, elementSlots map[string]int, alias, col string) (int, bool) {
	// A QUALIFIED read whose alias names a LEG window resolves to THAT leg's column —
	// the qualifier wins (`U.V` is U's V, never the element, even when the element AS
	// alias is also `V`). This precedes element-first so an explicit leg qualifier is
	// never shadowed by a same-named element.
	if alias != "" {
		if w, ok := windows[alias]; ok {
			if idx, found := w.Typ.FieldIndex(col); found {
				return w.Offset + idx, true
			}
		}
	}
	// A BARE / element-alias read: the ELEMENT wins the bare namespace (element-first —
	// an element AS alias shadowing a later leg column reads the element, matching the
	// name-model shadow the un-collapse preserves).
	if slot, ok := elementSlots[col]; ok {
		return slot, true
	}
	return 0, false
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
