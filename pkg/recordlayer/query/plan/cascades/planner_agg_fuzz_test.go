package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustAggFuzzConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct aggregation-fuzz fixture: " + err.Error())
	}
	return value
}

func aggFuzzRowType(columns []string) values.Type {
	fields := make([]values.Field, len(columns))
	for i, name := range columns {
		fieldType := values.NotNullString
		if name == "amount" || name == "id" {
			fieldType = values.NotNullLong
		}
		fields[i] = values.Field{Name: name, FieldType: fieldType, Ordinal: i}
	}
	return values.NewRecordType("AggregationFuzzRow", false, fields)
}

func aggFuzzRoot(q expressions.Quantifier) values.Value {
	flowedType := mustAggFuzzConstruct(q.GetFlowedObjectType())
	return mustAggFuzzConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func aggFuzzField(root values.Value, ordinal int) values.Value {
	return mustAggFuzzConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

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
		rowType := aggFuzzRowType(colPool)

		scan := mustAggFuzzConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, rowType))
		scanRef := expressions.InitialOf(scan)
		innerQ := expressions.ForEachQuantifier(scanRef)

		// Optional pre-filter on a grouping key.
		if preFilter%3 == 1 {
			inputRoot := aggFuzzRoot(innerQ)
			pred := predicates.NewComparisonPredicate(
				aggFuzzField(inputRoot, 0),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "x"),
			)
			filter := mustAggFuzzConstruct(expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{pred}, innerQ))
			filterRef := expressions.InitialOf(filter)
			innerQ = expressions.ForEachQuantifier(filterRef)
		}

		// Optional Sort (to enable streaming agg).
		if preFilter%3 == 2 {
			inputRoot := aggFuzzRoot(innerQ)
			sortKeys := make([]expressions.SortKey, numKeys)
			for i := range sortKeys {
				sortKeys[i] = expressions.SortKey{Value: aggFuzzField(inputRoot, i)}
			}
			sort := mustAggFuzzConstruct(expressions.NewLogicalSortExpression(sortKeys, innerQ))
			sortRef := expressions.InitialOf(sort)
			innerQ = expressions.ForEachQuantifier(sortRef)
		}

		inputRoot := aggFuzzRoot(innerQ)
		groupKeys := make([]values.Value, numKeys)
		for i := range groupKeys {
			groupKeys[i] = aggFuzzField(inputRoot, i%len(colPool))
		}

		aggs := make([]expressions.AggregateSpec, numAggs)
		for i := range aggs {
			function := aggFuncs[int(nAggs+byte(i))%len(aggFuncs)]
			// Every non-COUNT aggregate requires a numeric exact operand. Alternate
			// AMOUNT and ID while the function mix continues to vary independently.
			operandOrdinal := 3
			if i%2 == 1 {
				operandOrdinal = 4
			}
			aggs[i] = expressions.AggregateSpec{
				Function: function,
				Operand:  aggFuzzField(inputRoot, operandOrdinal),
			}
		}

		gb := mustAggFuzzConstruct(expressions.NewGroupByExpression(groupKeys, aggs, innerQ))
		var topRef *expressions.Reference

		switch outerOp % 4 {
		case 0:
			topRef = expressions.InitialOf(gb)
		case 1:
			// Outer sort on grouping keys.
			gbRef := expressions.InitialOf(gb)
			gbQ := expressions.ForEachQuantifier(gbRef)
			outputRoot := aggFuzzRoot(gbQ)
			sortKeys := make([]expressions.SortKey, numKeys)
			for i := range sortKeys {
				sortKeys[i] = expressions.SortKey{Value: aggFuzzField(outputRoot, i)}
			}
			sort := mustAggFuzzConstruct(expressions.NewLogicalSortExpression(sortKeys, gbQ))
			topRef = expressions.InitialOf(sort)
		case 2:
			// Distinct over GroupBy.
			gbRef := expressions.InitialOf(gb)
			gbQ := expressions.ForEachQuantifier(gbRef)
			distinct := mustAggFuzzConstruct(expressions.NewLogicalDistinctExpression(gbQ))
			topRef = expressions.InitialOf(distinct)
		case 3:
			// HAVING filter (non-pushable).
			gbRef := expressions.InitialOf(gb)
			gbQ := expressions.ForEachQuantifier(gbRef)
			outputRoot := aggFuzzRoot(gbQ)
			pred := predicates.NewComparisonPredicate(
				aggFuzzField(outputRoot, numKeys),
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5)),
			)
			havingFilter := mustAggFuzzConstruct(expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{pred}, gbQ))
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
			cand := newKnownDistinctValueIndexCandidate(
				"idx_group",
				[]string{"T"},
				cols,
				aliases,
				rowType,
				false,
				nil,
			)
			ctx = &indexTestPlanContext{candidates: []MatchCandidate{cand}}
		}

		rules := DefaultExpressionRules()
		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		p.MaxTasks = 50_000

		plan, _, err := p.Plan(topRef)
		if err != nil && !errors.Is(err, ErrPlannerCapHit) {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		if err == nil && plan == nil {
			t.Fatal("Plan succeeded but plan is nil")
		}
	})
}
