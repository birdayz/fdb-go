package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func plannerLimitFuzzRowType() values.Type {
	return values.NewRecordType("PlannerLimitFuzzRow", false, []values.Field{
		{Name: "x", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "c", FieldType: values.NullableLong, Ordinal: 1},
	})
}

func mustPlannerLimitFuzzConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct planner limit fuzz fixture: " + err.Error())
	}
	return value
}

func plannerLimitFuzzRoot(t testing.TB, q expressions.Quantifier) values.QuantifiedObjectValue {
	t.Helper()
	flowedType := mustPlannerLimitFuzzConstruct(q.GetFlowedObjectType())
	return mustPlannerLimitFuzzConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func plannerLimitFuzzField(t testing.TB, q expressions.Quantifier, ordinal int) values.Value {
	t.Helper()
	return mustPlannerLimitFuzzConstruct(values.ResolveFieldOrdinals(
		plannerLimitFuzzRoot(t, q), []int{ordinal}))
}

func plannerLimitFuzzProjectionValues(
	t testing.TB, q expressions.Quantifier, count, ordinal int,
) []values.Value {
	t.Helper()
	projected := make([]values.Value, count)
	for i := range projected {
		projected[i] = plannerLimitFuzzField(t, q, ordinal)
	}
	return projected
}

// FuzzPlanner_Limit_NoPanic exercises random LIMIT topologies to ensure
// the planner never panics. Tests LIMIT merge, no-op elimination, zero
// limit, push-through-projection, and physical implementation.
func FuzzPlanner_Limit_NoPanic(f *testing.F) {
	f.Add(int64(10), int64(0), int64(-1), int64(0), true, false)
	f.Add(int64(0), int64(0), int64(5), int64(3), false, true)
	f.Add(int64(-1), int64(0), int64(100), int64(50), true, true)
	f.Add(int64(5), int64(10), int64(3), int64(0), false, false)
	f.Add(int64(0), int64(0), int64(0), int64(0), false, false)
	f.Add(int64(-1), int64(20), int64(-1), int64(5), true, false)

	f.Fuzz(func(t *testing.T, outerLimit, outerOffset, innerLimit, innerOffset int64, addProjection, nestLimits bool) {
		scan := mustFullUnorderedScan(t, []string{"T"}, plannerLimitFuzzRowType())
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)

		var topExpr expressions.RelationalExpression

		if nestLimits {
			inner := mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(innerLimit, innerOffset, scanQ))
			innerRef := expressions.InitialOf(inner)
			innerQ := expressions.ForEachQuantifier(innerRef)

			if addProjection {
				proj := mustPlannerLimitFuzzConstruct(expressions.NewLogicalProjectionExpression(
					[]values.Value{plannerLimitFuzzField(t, innerQ, 0)},
					innerQ,
				))
				projRef := expressions.InitialOf(proj)
				projQ := expressions.ForEachQuantifier(projRef)
				topExpr = mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(outerLimit, outerOffset, projQ))
			} else {
				topExpr = mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(outerLimit, outerOffset, innerQ))
			}
		} else {
			if addProjection {
				proj := mustPlannerLimitFuzzConstruct(expressions.NewLogicalProjectionExpression(
					[]values.Value{plannerLimitFuzzField(t, scanQ, 0)},
					scanQ,
				))
				projRef := expressions.InitialOf(proj)
				projQ := expressions.ForEachQuantifier(projRef)
				topExpr = mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(outerLimit, outerOffset, projQ))
			} else {
				topExpr = mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(outerLimit, outerOffset, scanQ))
			}
		}

		ref := expressions.InitialOf(topExpr)

		rules := DefaultExpressionRules()
		p := NewPlanner(rules, EmptyPlanContext()).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		plan, _, err := p.Plan(ref)
		// We don't care about the specific result — just that it doesn't panic.
		_ = plan
		_ = err
	})
}

// FuzzPlanner_ProjectionPipeline_NoPanic exercises random projection
// topologies (with optional filter, sort, limit) through the full
// planner including physical implementation rules.
func FuzzPlanner_ProjectionPipeline_NoPanic(f *testing.F) {
	f.Add(uint8(3), true, true, true, int64(10))
	f.Add(uint8(1), false, false, false, int64(0))
	f.Add(uint8(5), true, false, true, int64(-1))
	f.Add(uint8(0), false, true, false, int64(100))

	f.Fuzz(func(t *testing.T, numCols uint8, addFilter, addSort, addLimit bool, limitVal int64) {
		cols := int(numCols%5) + 1
		scan := mustFullUnorderedScan(t, []string{"T"}, plannerLimitFuzzRowType())
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)

		var current expressions.RelationalExpression
		currentFirstOrdinal := 0

		if addFilter {
			current = mustPlannerLimitFuzzConstruct(expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
				scanQ,
			))
		} else {
			current = mustPlannerLimitFuzzConstruct(expressions.NewLogicalProjectionExpression(
				plannerLimitFuzzProjectionValues(t, scanQ, cols, 1), scanQ))
		}

		ref := expressions.InitialOf(current)
		q := expressions.ForEachQuantifier(ref)

		if addSort {
			sortExpr := mustPlannerLimitFuzzConstruct(expressions.NewLogicalSortExpression(
				[]expressions.SortKey{{Value: plannerLimitFuzzField(t, q, currentFirstOrdinal), Reverse: false}},
				q,
			))
			ref = expressions.InitialOf(sortExpr)
			q = expressions.ForEachQuantifier(ref)
		}

		if addLimit && limitVal >= 0 {
			limExpr := mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(limitVal, 0, q))
			ref = expressions.InitialOf(limExpr)
		} else {
			// Need a top-level ref
			topProj := mustPlannerLimitFuzzConstruct(expressions.NewLogicalProjectionExpression(
				plannerLimitFuzzProjectionValues(t, q, cols, currentFirstOrdinal), q))
			ref = expressions.InitialOf(topProj)
		}

		rules := DefaultExpressionRules()
		p := NewPlanner(rules, EmptyPlanContext()).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		plan, _, err := p.Plan(ref)
		_ = plan
		_ = err
	})
}

// FuzzPlanner_LimitOverUnion_NoPanic exercises LIMIT over UNION ALL
// topologies to validate the PushLimitThroughUnion rule doesn't diverge.
func FuzzPlanner_LimitOverUnion_NoPanic(f *testing.F) {
	f.Add(int64(10), int64(0), uint8(2))
	f.Add(int64(5), int64(3), uint8(3))
	f.Add(int64(0), int64(0), uint8(2))
	f.Add(int64(-1), int64(0), uint8(4))
	f.Add(int64(1), int64(100), uint8(2))

	f.Fuzz(func(t *testing.T, limit, offset int64, branches uint8) {
		numBranches := int(branches%4) + 2
		qs := make([]expressions.Quantifier, numBranches)
		for i := range qs {
			scan := mustFullUnorderedScan(t, []string{"T"}, plannerLimitFuzzRowType())
			qs[i] = expressions.ForEachQuantifier(expressions.InitialOf(scan))
		}
		union := mustPlannerLimitFuzzConstruct(expressions.NewLogicalUnionExpression(qs))
		unionRef := expressions.InitialOf(union)
		unionQ := expressions.ForEachQuantifier(unionRef)

		lim := mustPlannerLimitFuzzConstruct(expressions.NewLogicalLimitExpression(limit, offset, unionQ))
		ref := expressions.InitialOf(lim)

		rules := DefaultExpressionRules()
		p := NewPlanner(rules, EmptyPlanContext()).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		plan, _, err := p.Plan(ref)
		_ = plan
		_ = err
	})
}
