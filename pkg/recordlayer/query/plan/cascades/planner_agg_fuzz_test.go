package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzPlanner_Aggregation_NoPanic exercises the planner with random
// aggregation topologies: varying GroupBy key counts, aggregate function
// combos, optional pre-filter, optional outer sort, optional DISTINCT,
// and optional HAVING filter. The goal is to stress the interaction of:
//   - ImplementStreamingAggregationRule
//   - PushFilterThroughGroupByRule
//   - DistinctOverGroupByElimRule
//   - ImplementSortRule (when streaming agg produces order)
//
// and ensure none of them panic or produce inconsistent state.
func FuzzPlanner_Aggregation_NoPanic(f *testing.F) {
	f.Add(byte(1), byte(0), byte(0), byte(0), byte(0))
	f.Add(byte(3), byte(2), byte(1), byte(1), byte(1))
	f.Add(byte(2), byte(4), byte(3), byte(2), byte(2))
	f.Add(byte(1), byte(1), byte(0), byte(3), byte(0))

	colPool := []string{"region", "status", "city", "amount", "id", "name", "date"}
	aggFuncs := []expressions.AggregateFunction{
		expressions.AggCount, expressions.AggSum, expressions.AggMin,
		expressions.AggMax, expressions.AggAvg,
	}

	f.Fuzz(func(t *testing.T, nKeys, nAggs, preFilter, outerOp, indexSeed byte) {
		numKeys := int(nKeys%4) + 1
		numAggs := int(nAggs%4) + 1

		groupKeys := make([]values.Value, numKeys)
		for i := range groupKeys {
			groupKeys[i] = &values.FieldValue{
				Field: colPool[i%len(colPool)],
				Typ:   values.UnknownType,
			}
		}

		aggs := make([]expressions.AggregateSpec, numAggs)
		for i := range aggs {
			aggs[i] = expressions.AggregateSpec{
				Function: aggFuncs[int(nAggs+byte(i))%len(aggFuncs)],
				Operand:  &values.FieldValue{Field: colPool[(i+3)%len(colPool)], Typ: values.UnknownType},
			}
		}

		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		innerQ := expressions.ForEachQuantifier(scanRef)

		// Optional pre-filter on a grouping key.
		if preFilter%3 == 1 {
			pred := predicates.NewComparisonPredicate(
				&values.FieldValue{Field: colPool[0], Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "x"),
			)
			filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, innerQ)
			filterRef := expressions.InitialOf(filter)
			innerQ = expressions.ForEachQuantifier(filterRef)
		}

		// Optional Sort (to enable streaming agg).
		if preFilter%3 == 2 {
			sortKeys := make([]expressions.SortKey, numKeys)
			for i := range sortKeys {
				sortKeys[i] = expressions.SortKey{Value: groupKeys[i]}
			}
			sort := expressions.NewLogicalSortExpression(sortKeys, innerQ)
			sortRef := expressions.InitialOf(sort)
			innerQ = expressions.ForEachQuantifier(sortRef)
		}

		gb := expressions.NewGroupByExpression(groupKeys, aggs, innerQ)
		var topRef *expressions.Reference

		switch outerOp % 4 {
		case 0:
			topRef = expressions.InitialOf(gb)
		case 1:
			// Outer sort on grouping keys.
			gbRef := expressions.InitialOf(gb)
			gbQ := expressions.ForEachQuantifier(gbRef)
			sortKeys := make([]expressions.SortKey, numKeys)
			for i := range sortKeys {
				sortKeys[i] = expressions.SortKey{Value: groupKeys[i]}
			}
			sort := expressions.NewLogicalSortExpression(sortKeys, gbQ)
			topRef = expressions.InitialOf(sort)
		case 2:
			// Distinct over GroupBy.
			gbRef := expressions.InitialOf(gb)
			gbQ := expressions.ForEachQuantifier(gbRef)
			distinct := expressions.NewLogicalDistinctExpression(gbQ)
			topRef = expressions.InitialOf(distinct)
		case 3:
			// HAVING filter (non-pushable).
			gbRef := expressions.InitialOf(gb)
			gbQ := expressions.ForEachQuantifier(gbRef)
			pred := predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "cnt", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5)),
			)
			havingFilter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, gbQ)
			topRef = expressions.InitialOf(havingFilter)
		}

		// Optionally provide an index that covers grouping keys.
		var ctx PlanContext
		if indexSeed%2 == 0 {
			cols := make([]string, numKeys)
			aliases := make([]values.CorrelationIdentifier, numKeys)
			for i := range cols {
				cols[i] = colPool[i%len(colPool)]
				aliases[i] = values.UniqueCorrelationIdentifier()
			}
			cand := NewValueIndexScanMatchCandidate(
				"idx_group",
				[]string{"T"},
				cols,
				aliases,
				values.UnknownType,
				false,
			)
			ctx = &indexTestPlanContext{candidates: []MatchCandidate{cand}}
		}

		rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
		p := NewPlanner(rules, ctx).WithImplementationRules(DefaultImplementationRules())
		p.MaxTasks = 50_000

		plan, _, err := p.Plan(topRef)
		if err != nil && err != ErrPlannerCapHit {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		if err == nil && plan == nil {
			t.Fatal("Plan succeeded but plan is nil")
		}
	})
}
