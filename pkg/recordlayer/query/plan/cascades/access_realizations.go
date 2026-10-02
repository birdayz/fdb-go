package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// One consumption shares scan plans and compensated singletons with its
// intersections, like Java's bestMatchToPlanMap. Nothing outlives that batch.
type accessRealizations map[accessRealizationKey]*realizedAccess

type accessRealizationKey struct {
	match   PartialMatch
	reverse bool
}

type realizedAccess struct {
	scan           plans.RecordQueryPlan
	expression     expressions.RelationalExpression
	compensated    expressions.RelationalExpression
	singleComputed bool
	distinct       expressions.RelationalExpression
	distinctReady  bool
}

func (r accessRealizations) realize(access *SingleMatchedAccess) *realizedAccess {
	key := accessRealizationKey{access.GetPartialMatch(), access.IsReverseScanOrder()}
	if realized, ok := r[key]; ok {
		return realized
	}
	realized := &realizedAccess{scan: createScanForAccess(access)}
	r[key] = realized
	if realized.scan != nil {
		expr, err := wrapAccessScan(access, realized.scan)
		if err == nil {
			realized.expression = expr
		}
	}
	return realized
}

func (r accessRealizations) single(memoizer Memoizer, access *SingleMatchedAccess) expressions.RelationalExpression {
	realized := r.realize(access)
	if realized.singleComputed {
		return realized.compensated
	}
	realized.singleComputed = true
	expr := realized.expression
	compensation := access.GetCompensation()
	if expr == nil || compensation == nil || compensation.IsImpossible() {
		return nil
	}
	if compensation.IsNeeded() {
		forMatch, ok := compensation.(*ForMatchCompensation)
		if !ok || forMatch == nil {
			return nil
		}
		var applied bool
		expr, applied = forMatch.ApplyAllNeeded(memoizer, expr,
			func(realizedAlias values.CorrelationIdentifier) TranslationMap {
				return TranslationMapOfAliases(access.GetCandidateTopAlias(), realizedAlias)
			})
		if !applied {
			return nil
		}
	}
	realized.compensated = expr
	return expr
}

func (r accessRealizations) distinctPlan(access *SingleMatchedAccess) expressions.RelationalExpression {
	realized := r.realize(access)
	if realized.distinctReady {
		return realized.distinct
	}
	realized.distinctReady = true
	expr := realized.expression
	if expr == nil {
		return nil
	}
	if candidateCreatesDuplicates(access.GetPartialMatch().GetMatchCandidate()) {
		distinct, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(
			expressions.NewPhysicalQuantifier(expressions.FinalOfAtStage(expr, expressions.StageCanonical)))
		if err != nil {
			return nil
		}
		expr = distinct
	}
	realized.distinct = expr
	return expr
}
