package cascades

import (
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// WithPrimaryKeyIntersector returns an IntersectorFunc that creates
// physical intersection plans from pairs of compatible partial matches
// using the primary key as the comparison key.
//
// Creates RecordQueryIntersectionPlan directly (physical, not logical)
// wrapped in PhysicalIntersectionWrapper. This avoids the task cascade
// that would occur if LogicalIntersectionExpression were inserted and
// then explored — fresh child References trigger re-exploration loops.
func WithPrimaryKeyIntersector(ctx PlanContext) IntersectorFunc {
	return func(
		accesses []Vectored[*SingleMatchedAccess],
		_ []*properties.RequestedOrdering,
	) *IntersectionResult {
		if len(accesses) < 2 {
			return NoViableIntersection()
		}

		pkValues := commonPrimaryKeyValues(accesses, ctx)
		if len(pkValues) == 0 {
			return NoViableIntersection()
		}
		// RFC-181 P0.1: only PK-monotone legs may feed the strict pk-sorted
		// merge; a single incompatible access disqualifies itself (the
		// remaining pairs still form below).
		compatible := accesses[:0:0]
		for _, a := range accesses {
			if accessCompatibleWithPKMerge(a.Value, pkValues) {
				compatible = append(compatible, a)
			}
		}
		accesses = compatible
		if len(accesses) < 2 {
			return NoViableIntersection()
		}

		var resultExprs []expressions.RelationalExpression

		for i := 0; i < len(accesses)-1; i++ {
			for j := i + 1; j < len(accesses); j++ {
				ai := accesses[i].Value
				aj := accesses[j].Value

				// SAME-INDEX guard by candidate NAME, not object identity:
				// the context can hold two candidate objects for one index
				// (and epoch-driven re-matching seeds both), and an
				// intersection of an index with itself is the same row set
				// twice — a nonsensical self-intersection.
				if ai.GetPartialMatch().GetMatchCandidate().CandidateName() ==
					aj.GetPartialMatch().GetMatchCandidate().CandidateName() {
					continue
				}

				planI := createScanForAccess(ai)
				planJ := createScanForAccess(aj)
				if planI == nil || planJ == nil {
					continue
				}

				bakedKeys := bakedIntersectionKeys(pkValues, []plans.RecordQueryPlan{planI, planJ})
				if bakedKeys == nil {
					continue
				}
				intersectionPlan := plans.NewRecordQueryIntersectionPlan(
					[]plans.RecordQueryPlan{planI, planJ}, bakedKeys)

				exprI := wrapAccessScan(ai, planI)
				exprJ := wrapAccessScan(aj, planJ)
				qI := expressions.ForEachQuantifier(expressions.InitialOf(exprI))
				qJ := expressions.ForEachQuantifier(expressions.InitialOf(exprJ))

				expr, viable := compensateIntersection(
					[]*SingleMatchedAccess{ai, aj},
					NewPhysicalIntersectionWrapper(intersectionPlan, []expressions.Quantifier{qI, qJ}))
				if !viable {
					continue
				}
				resultExprs = append(resultExprs, expr)
			}
		}

		// Cap at 3-way: 4-way intersections have diminishing returns
		// (each additional leg adds scan I/O but rarely improves
		// selectivity beyond 3 independent predicates) and the
		// candidate cap of 4 already limits the input size.
		if len(accesses) >= 3 {
			for i := 0; i < len(accesses)-2; i++ {
				for j := i + 1; j < len(accesses)-1; j++ {
					for k := j + 1; k < len(accesses); k++ {
						ai := accesses[i].Value
						aj := accesses[j].Value
						ak := accesses[k].Value

						// Same NAME-based guard as the pair loop: the context
						// can hold two candidate OBJECTS for one index, so
						// object identity under-detects and an A/A/B triple
						// would keep the redundant same-index arm.
						ni := ai.GetPartialMatch().GetMatchCandidate().CandidateName()
						nj := aj.GetPartialMatch().GetMatchCandidate().CandidateName()
						nk := ak.GetPartialMatch().GetMatchCandidate().CandidateName()
						if ni == nj || ni == nk || nj == nk {
							continue
						}

						planI := createScanForAccess(ai)
						planJ := createScanForAccess(aj)
						planK := createScanForAccess(ak)
						if planI == nil || planJ == nil || planK == nil {
							continue
						}

						bakedKeys := bakedIntersectionKeys(pkValues, []plans.RecordQueryPlan{planI, planJ, planK})
						if bakedKeys == nil {
							continue
						}
						intersectionPlan := plans.NewRecordQueryIntersectionPlan(
							[]plans.RecordQueryPlan{planI, planJ, planK}, bakedKeys)

						exprI := wrapAccessScan(ai, planI)
						exprJ := wrapAccessScan(aj, planJ)
						exprK := wrapAccessScan(ak, planK)
						qI := expressions.ForEachQuantifier(expressions.InitialOf(exprI))
						qJ := expressions.ForEachQuantifier(expressions.InitialOf(exprJ))
						qK := expressions.ForEachQuantifier(expressions.InitialOf(exprK))

						expr, viable := compensateIntersection(
							[]*SingleMatchedAccess{ai, aj, ak},
							NewPhysicalIntersectionWrapper(intersectionPlan,
								[]expressions.Quantifier{qI, qJ, qK}))
						if !viable {
							continue
						}
						resultExprs = append(resultExprs, expr)
					}
				}
			}
		}

		if len(resultExprs) == 0 {
			return NoViableIntersection()
		}

		return NewIntersectionResult(
			properties.NewRichOrdering(nil, nil, false),
			NoCompensation,
			resultExprs,
		)
	}
}

// compensateIntersection ports the compensation clause of Java's
// WithPrimaryKeyDataAccessRule.createIntersectionAndCompensation
// (WithPrimaryKeyDataAccessRule.java:135-139, 166, 191-194): the per-leg
// compensations fold via the intersection monoid; an impossible fold
// declines the combination outright (Java builds no expression for it — a
// leg residual that cannot be reapplied must never be silently dropped),
// and a needed fold wraps the intersection with the compensated residual
// predicates. Every leg's candidate-top alias rebases onto the realized
// base alias, exactly Java's matchedToRealizedTranslationMap (:233-242).
//
// Without this, `WHERE a=? AND b=? AND c=?` over idx(a), idx(b) planned to
// a bare Intersection(idx_a, idx_b) and the c residual VANISHED — wrong
// rows, confirmed live against FDB (see the pinned regression tests).
func compensateIntersection(
	accesses []*SingleMatchedAccess,
	intersectionExpr expressions.RelationalExpression,
) (expressions.RelationalExpression, bool) {
	comps := make([]Compensation, 0, len(accesses))
	for _, a := range accesses {
		comps = append(comps, a.GetCompensation())
	}
	comp := IntersectCompensations(comps)
	if comp.IsImpossible() {
		return nil, false
	}
	if !comp.IsNeeded() {
		return intersectionExpr, true
	}
	fmc, ok := comp.(*ForMatchCompensation)
	if !ok {
		// A needed compensation only the ForMatch form can reapply;
		// anything else cannot be realized here — decline like Java's
		// impossible arm rather than drop the residual.
		return nil, false
	}
	return fmc.ApplyAllNeeded(intersectionExpr, func(realizedAlias values.CorrelationIdentifier) TranslationMap {
		b := NewTranslationMapBuilder()
		for _, a := range accesses {
			topAlias := a.GetCandidateTopAlias()
			if topAlias.IsZero() {
				continue
			}
			b.When(topAlias).Then(func(_ values.CorrelationIdentifier, leafValue values.LeafValue) values.Value {
				return leafValue.RebaseLeaf(realizedAlias)
			})
		}
		return b.Build()
	}), true
}

func commonPrimaryKeyValues(accesses []Vectored[*SingleMatchedAccess], ctx PlanContext) []values.Value {
	if len(accesses) == 0 {
		return nil
	}

	var commonTypes []string
	for _, v := range accesses {
		types := v.Value.GetPartialMatch().GetMatchCandidate().GetRecordTypes()
		if len(types) == 0 {
			return nil
		}
		if commonTypes == nil {
			commonTypes = types
		} else if !slices.Equal(commonTypes, types) {
			return nil
		}
	}

	if len(commonTypes) != 1 {
		return nil
	}

	pkCols := ctx.GetPrimaryKeyColumns(commonTypes[0])
	if len(pkCols) == 0 {
		return nil
	}

	result := make([]values.Value, len(pkCols))
	for i, col := range pkCols {
		result[i] = &values.FieldValue{
			Field: strings.ToUpper(col),
			Typ:   values.UnknownType,
		}
	}
	return result
}

// bakedIntersectionKeys resolves the name-only pk comparison keys against
// the legs' flowed row layout. EVERY leg must flow the same RecordType
// (the commonPrimaryKeyValues gate already pins one record type; the
// layout check is leg-order-agnostic so a single layout-less leg —
// whatever its slot — declines the candidate). The ordinal row model has
// no runtime name-resolution fallback: an unbaked FieldValue fails LOUD
// at merge time (OrdinalResolutionError), so a comparison key that
// cannot bake is a plan-time DECLINE of the intersection candidate,
// never a runtime error. Returns nil when any key stays unbaked.
func bakedIntersectionKeys(pkValues []values.Value, legs []plans.RecordQueryPlan) []values.Value {
	var rowType *values.RecordType
	for _, leg := range legs {
		rt, isRT := leg.GetResultType().(*values.RecordType)
		if !isRT {
			return nil
		}
		if rowType == nil {
			rowType = rt
			continue
		}
		if !rowType.Equals(rt) {
			return nil
		}
	}
	baked := bakeMergeComparisonKeys(pkValues, nil, rowType)
	for _, k := range baked {
		fv, isFV := k.(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			return nil
		}
	}
	return baked
}

// accessCompatibleWithPKMerge reports whether an access's scan emission is
// PK-MONOTONIC — the precondition of the strict pk-sorted intersection
// merge (merge_cursor.go advances non-maximal legs past rows forever, so a
// non-monotone leg silently DROPS intersection rows). Ports the substance
// of Java's isCompatibleComparisonKey gate
// (AbstractDataAccessRule.java:1145-1152 with comparison keys fixed to the
// common primary key, the only comparison key this intersector builds):
// walking the leg's matched ordering parts, the FREE (non-equality-bound)
// sequence must BEGIN with exactly the primary-key values that are not
// equality-bound in this leg, in pk order, each ascending — an
// inequality-bound index column (`a > 5`) is a free NON-pk part at the
// front, exactly the emission (`a, pk`) whose pk sequence interleaves.
// Trailing free parts beyond the pk are harmless: the pk is a unique key,
// so once the full free-pk prefix is present the order is already total.
// Reverse legs decline outright — the executor's merge compares forward
// (executeIntersection hardcodes reverse=false).
func accessCompatibleWithPKMerge(access *SingleMatchedAccess, pkValues []values.Value) bool {
	if access.IsReverseScanOrder() {
		return false
	}
	// Case-FOLDED comparison keys: commonPrimaryKeyValues upper-cases the
	// pk columns while the candidate's pk-suffix parts carry FieldNames()
	// verbatim — comparing un-normalized renderings made a lowercase pk
	// name miss and silently over-decline EVERY intersection. (Rendering-
	// string identity here is the bridge pattern WS-N Phase C retires;
	// tolerable now because both sides are same-frame flat FieldValues.)
	key := func(v values.Value) string { return strings.ToUpper(values.ExplainValue(v)) }
	parts := access.GetPartialMatch().GetMatchInfo().GetMatchedOrderingParts()
	equalityBound := make(map[string]struct{})
	for _, op := range parts {
		if op.GetComparisonRange().IsEquality() {
			equalityBound[key(op.GetValue())] = struct{}{}
		}
	}
	var expect []string
	for _, pv := range pkValues {
		k := key(pv)
		if _, bound := equalityBound[k]; !bound {
			expect = append(expect, k)
		}
	}
	freeIdx := 0
	for _, op := range parts {
		if op.GetComparisonRange().IsEquality() {
			continue
		}
		if freeIdx >= len(expect) {
			break // trailing free parts after the full pk prefix: harmless
		}
		if op.GetMatchedSortOrder().IsAnyDescending() {
			return false
		}
		if key(op.GetValue()) != expect[freeIdx] {
			return false
		}
		freeIdx++
	}
	return freeIdx == len(expect)
}

func createScanForAccess(access *SingleMatchedAccess) plans.RecordQueryPlan {
	pm := access.GetPartialMatch()
	candidate := pm.GetMatchCandidate()
	matchInfo := pm.GetMatchInfo()
	regularInfo := matchInfo.GetRegularMatchInfo()
	bindings := regularInfo.GetParameterBindingMap()
	prefix := candidate.ComputeBoundParameterPrefixMap(bindings)
	return candidate.ToScanPlan(prefix, access.IsReverseScanOrder())
}
