package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustCorrelationPlan[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPhysicalUnaryCurrentIsLocal(t *testing.T) {
	t.Parallel()
	scan, err := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	scan = mustCorrelationPlan(t, scan, err)
	for _, alias := range []values.CorrelationIdentifier{values.CurrentCorrelation(), values.NamedCorrelationIdentifier("_current"), values.NamedCorrelationIdentifier("foreign")} {
		for _, kind := range []string{"map", "filter", "legacy_filter", "projection", "aggregate"} {
			t.Run(kind+"/"+alias.Name(), func(t *testing.T) {
				t.Parallel()
				var root values.Value = scan.GetResultValue()
				var err error
				if alias != values.CurrentCorrelation() {
					root, err = values.NewQuantifiedObjectValue(alias, values.NotNullLong)
					root = mustCorrelationPlan(t, root, err)
				}
				var plan expressions.RelationalExpression
				switch kind {
				case "map":
					plan, err = NewRecordQueryMapPlan(scan, root)
				case "filter":
					plan, err = NewRecordQueryPredicatesFilterPlan(scan, []predicates.QueryPredicate{
						predicates.NewComparisonPredicate(root, predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
					})
				case "legacy_filter":
					plan, err = NewRecordQueryFilterPlan([]predicates.QueryPredicate{
						predicates.NewComparisonPredicate(root, predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
					}, scan)
				case "projection":
					plan, err = NewRecordQueryProjectionPlan([]values.Value{root}, scan)
				case "aggregate":
					plan, err = NewRecordQueryStreamingAggregationPlan(scan, []values.Value{root}, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				got := expressions.GetCorrelatedToOfExpression(plan)
				_, free := got[alias]
				wantFree := alias != values.CurrentCorrelation()
				if free != wantFree || len(got) != boolCorrelationCount(wantFree) {
					t.Fatalf("correlations=%v, want alias free=%t", got, wantFree)
				}
			})
		}
	}
}

func TestPhysicalInBindsOnlyItsSourceAliases(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"join", "chain", "union"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			scan, err := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
			scan = mustCorrelationPlan(t, scan, err)
			first, second := values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier()
			foreign, innerAlias := values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier()
			namedTwin := values.NamedCorrelationIdentifier(first.Name())
			makeFilter := func(aliases ...values.CorrelationIdentifier) *RecordQueryPredicatesFilterPlan {
				t.Helper()
				preds := make([]predicates.QueryPredicate, len(aliases))
				for i, alias := range aliases {
					value, err := values.NewQuantifiedObjectValue(alias, values.NotNullLong)
					value = mustCorrelationPlan(t, value, err)
					preds[i] = predicates.NewComparisonPredicate(value, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
				}
				filter, err := NewRecordQueryPredicatesFilterPlan(scan, preds)
				return mustCorrelationPlan(t, filter, err)
			}
			aliases := []values.CorrelationIdentifier{first, second, foreign, innerAlias, namedTwin}
			innerRef := expressions.FinalOf(makeFilter(aliases...))
			innerQ := expressions.NewPhysicalQuantifier(innerRef).WithAlias(innerAlias)
			var plan RecordQueryPlan
			switch kind {
			case "join", "chain":
				plan, err = NewRecordQueryInJoinPlanFromQuantifierWithBindingAlias(innerQ, first, false, false)
				if err == nil && kind == "chain" {
					plan, err = NewRecordQueryInJoinPlanWithBindingAlias(plan, second, false, false)
				}
			case "union":
				plan, err = NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(innerQ, []values.CorrelationIdentifier{first, second}, nil, false, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !plan.CanCorrelate() {
				t.Error("IN plan must declare that it anchors its source correlations")
			}
			rebuilt, err := plan.WithQuantifiers(plan.GetQuantifiers())
			rebuilt = mustCorrelationPlan(t, rebuilt, err)
			group := expressions.FinalOf(rebuilt)
			want := map[values.CorrelationIdentifier]struct{}{foreign: {}, innerAlias: {}, namedTwin: {}}
			if kind == "join" {
				want[second] = struct{}{}
			}
			assertCorrelations := func(got map[values.CorrelationIdentifier]struct{}) {
				t.Helper()
				if len(got) != len(want) {
					t.Errorf("IN correlations = %#v, want %#v", got, want)
					return
				}
				for alias := range want {
					if _, ok := got[alias]; !ok {
						t.Errorf("IN correlations = %#v, missing external %#v", got, alias)
					}
				}
			}
			assertCorrelations(expressions.GetCorrelatedToOfExpression(plan))
			assertCorrelations(expressions.GetCorrelatedToOfExpression(rebuilt))
			assertCorrelations(group.GetCorrelatedTo())
			additional := values.UniqueCorrelationIdentifier()
			if !innerRef.InsertFinal(makeFilter(append(aliases, additional)...)) {
				t.Fatal("fixture failed to add a new child correlation")
			}
			want[additional] = struct{}{}
			assertCorrelations(group.GetCorrelatedTo())
		})
	}
}

func boolCorrelationCount(present bool) int {
	if present {
		return 1
	}
	return 0
}

func TestPhysicalJoinBindsRuntimeAliases(t *testing.T) {
	t.Parallel()
	for _, flatMap := range []bool{false, true} {
		t.Run(map[bool]string{false: "materialized", true: "flat_map"}[flatMap], func(t *testing.T) {
			t.Parallel()
			scan, err := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
			scan = mustCorrelationPlan(t, scan, err)
			outer, inner, foreign := values.NamedCorrelationIdentifier("outer"), values.NamedCorrelationIdentifier("inner"), values.NamedCorrelationIdentifier("foreign")
			ov, err := values.NewQuantifiedObjectValue(outer, values.NotNullLong)
			ov = mustCorrelationPlan(t, ov, err)
			iv, err := values.NewQuantifiedObjectValue(inner, values.NotNullLong)
			iv = mustCorrelationPlan(t, iv, err)
			fv, err := values.NewQuantifiedObjectValue(foreign, values.NotNullLong)
			fv = mustCorrelationPlan(t, fv, err)
			result := values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "o", Value: ov},
				values.RecordConstructorField{Name: "i", Value: iv},
				values.RecordConstructorField{Name: "f", Value: fv},
			)
			predicate := predicates.NewComparisonPredicate(ov, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: iv})
			var plan expressions.RelationalExpression
			if flatMap {
				filtered, filterErr := NewRecordQueryPredicatesFilterPlanWithAlias(scan, []predicates.QueryPredicate{predicate}, inner)
				filtered = mustCorrelationPlan(t, filtered, filterErr)
				plan, err = NewRecordQueryFlatMapPlan(scan, filtered, outer, inner, result, false)
			} else {
				plan, err = NewRecordQueryNestedLoopJoinPlan(scan, scan, []predicates.QueryPredicate{predicate}, JoinInner, outer, inner, result)
			}
			if err != nil {
				t.Fatal(err)
			}
			rebuilt, err := plan.WithQuantifiers(plan.GetQuantifiers())
			rebuilt = mustCorrelationPlan(t, rebuilt, err)
			for _, candidate := range []expressions.RelationalExpression{plan, rebuilt} {
				got := expressions.GetCorrelatedToOfExpression(candidate)
				if _, exists := got[foreign]; !exists || len(got) != 1 {
					t.Fatalf("join correlations=%v, want only foreign projection binding", got)
				}
				qs := candidate.GetQuantifiers()
				if qs[0].GetAlias() != outer || qs[1].GetAlias() != inner {
					t.Fatal("memo edges do not declare the runtime binding aliases")
				}
			}
		})
	}
}
