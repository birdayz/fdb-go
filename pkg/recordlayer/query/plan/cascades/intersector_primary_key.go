package cascades

import (
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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
		_ []*RequestedOrdering,
	) *IntersectionResult {
		if len(accesses) < 2 {
			return NoViableIntersection()
		}

		pkValues := commonPrimaryKeyValues(accesses, ctx)

		var resultExprs []expressions.RelationalExpression

		for i := 0; i < len(accesses)-1; i++ {
			for j := i + 1; j < len(accesses); j++ {
				ai := accesses[i].Value
				aj := accesses[j].Value

				if ai.GetPartialMatch().GetMatchCandidate() == aj.GetPartialMatch().GetMatchCandidate() {
					continue
				}

				planI := createScanForAccess(ai)
				planJ := createScanForAccess(aj)
				if planI == nil || planJ == nil {
					continue
				}
				// A self-limiting per-partition vector top-k scan delivers
				// (partition, distance) order, NOT primary-key order, so it cannot
				// be a leg of a pk-keyed sorted-merge intersection: the max-key/
				// advance loop (merge_cursor.go) would drop rows whose distance rank
				// disagrees with their pk order (wrong rows for k>1). The safe shape
				// is a Filter above the un-intersected self-limiting scan
				// (compensationSafeForYield's partition-residual exception), so skip
				// this pair. (An ordered-stream vector scan is already excluded from
				// data-access intersection candidates upstream, planner.go.)
				if isSelfLimitingVectorScan(planI) || isSelfLimitingVectorScan(planJ) {
					continue
				}

				intersectionPlan := plans.NewRecordQueryIntersectionPlan(
					[]plans.RecordQueryPlan{planI, planJ}, pkValues)

				exprI := wrapAccessScan(ai, planI)
				exprJ := wrapAccessScan(aj, planJ)
				qI := expressions.ForEachQuantifier(expressions.InitialOf(exprI))
				qJ := expressions.ForEachQuantifier(expressions.InitialOf(exprJ))

				resultExprs = append(resultExprs,
					NewPhysicalIntersectionWrapper(intersectionPlan, []expressions.Quantifier{qI, qJ}))
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

						ci := ai.GetPartialMatch().GetMatchCandidate()
						cj := aj.GetPartialMatch().GetMatchCandidate()
						ck := ak.GetPartialMatch().GetMatchCandidate()
						if ci == cj || ci == ck || cj == ck {
							continue
						}

						planI := createScanForAccess(ai)
						planJ := createScanForAccess(aj)
						planK := createScanForAccess(ak)
						if planI == nil || planJ == nil || planK == nil {
							continue
						}
						// See the 2-way loop: a self-limiting vector scan is not
						// pk-ordered and must not be an intersection leg.
						if isSelfLimitingVectorScan(planI) || isSelfLimitingVectorScan(planJ) || isSelfLimitingVectorScan(planK) {
							continue
						}

						intersectionPlan := plans.NewRecordQueryIntersectionPlan(
							[]plans.RecordQueryPlan{planI, planJ, planK}, pkValues)

						exprI := wrapAccessScan(ai, planI)
						exprJ := wrapAccessScan(aj, planJ)
						exprK := wrapAccessScan(ak, planK)
						qI := expressions.ForEachQuantifier(expressions.InitialOf(exprI))
						qJ := expressions.ForEachQuantifier(expressions.InitialOf(exprJ))
						qK := expressions.ForEachQuantifier(expressions.InitialOf(exprK))

						resultExprs = append(resultExprs,
							NewPhysicalIntersectionWrapper(intersectionPlan,
								[]expressions.Quantifier{qI, qJ, qK}))
					}
				}
			}
		}

		if len(resultExprs) == 0 {
			return NoViableIntersection()
		}

		return NewIntersectionResult(
			NewRichOrdering(nil, nil, false),
			NoCompensation,
			resultExprs,
		)
	}
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

// isSelfLimitingVectorScan reports whether a scan plan is a self-limiting
// per-partition vector top-k scan (RecordQueryVectorIndexPlan not in ordered-
// stream mode). Such a scan emits its rows in (partition, distance) order, which
// is not primary-key-monotonic, so it cannot participate in the pk-keyed
// sorted-merge intersection (see the skip in WithPrimaryKeyIntersector).
func isSelfLimitingVectorScan(p plans.RecordQueryPlan) bool {
	v, ok := p.(*plans.RecordQueryVectorIndexPlan)
	return ok && !v.IsOrderedStream()
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
