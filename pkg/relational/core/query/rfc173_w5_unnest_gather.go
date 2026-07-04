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

// translateGatheredUnnestCluster builds the flat (N+1)-quantifier select for
// a multi-source lateral unnest. The caller (translateUnnestJoin) has already
// run the FULL classification/validation gauntlet — metadata, CTE/derived
// rejection, array-field typing, chained-unnest guard, alias-collision (P1)
// and AS/AT-distinct (P2b) rejections — so every input reaching this point is
// a VALID unnest; nil here means DECLINE (the caller falls back to the binary
// name-model path), never an error.
func (t *cascadesTranslator) translateGatheredUnnestCluster(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	innerCorr values.CorrelationIdentifier,
	elementType values.Type,
	fieldName string,
) expressions.RelationalExpression {
	leftJoin, isJoin := j.Left.(*logical.LogicalJoin)
	if !isJoin {
		return nil // single-source outer — the W4c binary path owns it
	}
	// The left cluster must be a GATED-if-fresh inner cluster (the enclosure-
	// free probe, side-effect-free Decide — the ordinalEligible pattern): an
	// ungated cluster flows name-model rows the baked collection read cannot
	// consume (ruling Q1 condition iii feeds Q5's fail-open residual).
	prev := t.inInnerCluster
	t.inInnerCluster = false
	d := t.ordinalWedgeGateDecide(leftJoin)
	t.inInnerCluster = prev
	if !d.Gated {
		return nil
	}

	legs := t.legsOfGatedJoin(leftJoin)
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
	seen := map[string]struct{}{}
	for _, lt := range legTypes {
		for _, f := range lt.typ.Fields {
			n := strings.ToUpper(f.Name)
			if _, dup := seen[n]; dup {
				return nil
			}
			seen[n] = struct{}{}
		}
	}

	// The OWNING source: segment 0 must be a gathered PLAIN leg (a box leg's
	// alias names only its rightmost leaf — a baked read against the box concat
	// would need the buried offset; decline, fail-open) and the classified
	// array field must be a column of that leg's own type. Multi-segment field
	// paths (`t.a.b`) decline: the bake addresses ONE ordinal of the leg type.
	seg0 := strings.ToUpper(u.Segments[0])
	ownerType, isPlainLeg := gatheredPlainLegType(t, legs, seg0)
	if !isPlainLeg || len(u.Segments) != 2 {
		return nil
	}
	arrIdx, found := ownerType.FieldIndex(strings.ToUpper(fieldName))
	if !found {
		return nil
	}
	ownerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(seg0), ownerType)
	collection, err := values.NewFieldValueOfOrdinal(ownerQOV, arrIdx)
	if err != nil {
		return nil
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
			values.NamedCorrelationIdentifier(leg.alias), ref,
		))
		sourceAliases = append(sourceAliases, leg.alias)
	}
	t.inInnerCluster = prevEnclosure

	explode := expressions.NewExplodeExpressionWithOrdinality(collection, u.AtAlias != "")
	quantifiers = append(quantifiers, expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode)))
	sourceAliases = append(sourceAliases, innerCorr.Name())

	// Seed RV: the flat ordinal leg runs + the W4c unnest inner fields (the
	// element/ordinal branch shared with the single-source seed). The assert
	// runs only for the full-baked shape (AS+AT); the mixed direct-QOV element
	// and the AT-only partial run legitimately skip it — exactly the W4c rule.
	innerFields, fullBaked, ok := unnestSeedInnerFields(innerCorr, u, elementType)
	if !ok {
		return nil // degenerate no-AS/no-AT — name-model residual
	}
	fields = append(fields, innerFields...)
	rc := values.NewRawRecordConstructorValue(fields...)
	if fullBaked {
		values.AssertOrdinalJoinSeed(rc)
	}

	// Predicates: the cluster root's OWN ON conjunct (gatherInnerClusterPreds
	// deliberately skips its argument's own ON — "the root's own ON is the
	// caller's") + the nested inner joins' ON conjuncts + the unnest join's
	// (a comma join — normally none), cross-leg conjuncts baked through the
	// same legTypes the seed used.
	var preds []predicates.QueryPredicate
	if qp, isQP := leftJoin.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
		preds = append(preds, qp)
	}
	if j.OnPredicate != nil {
		if qp, isQP := j.OnPredicate.(predicates.QueryPredicate); isQP {
			preds = append(preds, qp)
		}
	}
	preds = append(preds, gatherInnerClusterPreds(leftJoin)...)
	preds = bakeGatedJoinPredicates(preds, legTypes)

	return expressions.NewSelectExpressionWithJoinType(
		rc,
		quantifiers,
		preds,
		sourceAliases,
		expressions.JoinInner,
	)
}

// gatheredPlainLegType resolves a gathered PLAIN (non-box) leg's flowed type
// by alias. A box leg declines: its alias names the rightmost leaf while its
// type is the whole concat — a first-position bake would read the wrong slot.
func gatheredPlainLegType(t *cascadesTranslator, legs []clusterLeg, alias string) (*values.RecordType, bool) {
	for _, leg := range legs {
		if !strings.EqualFold(leg.alias, alias) {
			continue
		}
		if _, isJoin := leg.op.(*logical.LogicalJoin); isJoin {
			return nil, false
		}
		typ := t.ordinalLegType(leg.op)
		return typ, typ != nil
	}
	return nil, false
}
