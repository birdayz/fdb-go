package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestRFC190BuildExistsCompensationChainAliasContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		preserveAlias     bool
		withBelowFOD      bool
		hasExistsFilter   bool
		negated           bool
		residualType      predicates.ComparisonType
		wantFilterCount   int
		wantAliasDistinct bool
	}{
		{
			name:              "fresh_positive",
			withBelowFOD:      true,
			hasExistsFilter:   true,
			residualType:      predicates.ComparisonIsNotNull,
			wantFilterCount:   2,
			wantAliasDistinct: true,
		},
		{
			name:            "preserved_negative",
			preserveAlias:   true,
			withBelowFOD:    true,
			hasExistsFilter: true,
			negated:         true,
			residualType:    predicates.ComparisonIsNull,
			wantFilterCount: 2,
		},
		{
			name:          "preserved_projected_exists",
			preserveAlias: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			innerCorrelation := values.NamedCorrelationIdentifier(
				"inner_" + test.name)
			baseAlias := innerCorrelation
			if !test.preserveAlias {
				baseAlias = values.NamedCorrelationIdentifier(
					"base_" + test.name)
			}
			scan := plans.NewRecordQueryScanPlan(
				[]string{"T"}, values.UnknownType, false)
			call := NewExpressionRuleCall(expressions.InitialOf(scan), nil, nil)
			baseQ := expressions.NamedPhysicalQuantifier(
				baseAlias, call.MemoizeExpression(scan))
			belowFODPredicate := predicates.NewComparisonPredicate(
				values.NewFieldValue(
					values.NewQuantifiedObjectValue(innerCorrelation),
					"K",
					values.UnknownType,
				),
				predicates.Comparison{Type: predicates.ComparisonIsNotNull},
			)
			var belowFODPredicates []predicates.QueryPredicate
			if test.withBelowFOD {
				belowFODPredicates = []predicates.QueryPredicate{
					belowFODPredicate,
				}
			}

			finalQ := buildExistsCompensationChain(
				call,
				baseQ,
				scan,
				innerCorrelation,
				belowFODPredicates,
				test.hasExistsFilter,
				test.negated,
				test.preserveAlias,
			)

			top := rfc190FinalPlanForQuantifier(t, finalQ)
			aliases := []values.CorrelationIdentifier{finalQ.GetAlias()}
			filterCount := 0
			if test.hasExistsFilter {
				residualFilter, ok := top.(*plans.RecordQueryPredicatesFilterPlan)
				if !ok {
					t.Fatalf("top plan = %T, want existential residual filter", top)
				}
				filterCount++
				if residualFilter.GetInnerAlias() != innerCorrelation {
					t.Fatalf("residual binding alias = %v, want %v",
						residualFilter.GetInnerAlias(), innerCorrelation)
				}
				preds := residualFilter.GetPredicates()
				if len(preds) != 1 {
					t.Fatalf("residual predicate count = %d, want 1", len(preds))
				}
				residual, ok := preds[0].(*predicates.ComparisonPredicate)
				if !ok {
					t.Fatalf("residual predicate = %T, want comparison", preds[0])
				}
				if residual.Comparison.Type != test.residualType {
					t.Fatalf("residual comparison = %v, want %v",
						residual.Comparison.Type, test.residualType)
				}
				qov, ok := residual.Operand.(*values.QuantifiedObjectValue)
				if !ok {
					t.Fatalf("residual operand = %T, want quantified object",
						residual.Operand)
				}
				if qov.Correlation != innerCorrelation {
					t.Fatalf("residual operand correlation = %v, want %v",
						qov.Correlation, innerCorrelation)
				}
				aliases = append(aliases,
					residualFilter.GetInnerQuantifier().GetAlias())
				top = residualFilter.GetInner()
			}

			fod, ok := top.(*plans.RecordQueryFirstOrDefaultPlan)
			if !ok {
				t.Fatalf("plan below residual = %T, want FirstOrDefault", top)
			}
			aliases = append(aliases, fod.GetInnerQuantifier().GetAlias())

			belowFOD := fod.GetInner()
			if test.withBelowFOD {
				belowFODFilter, ok := belowFOD.(*plans.RecordQueryPredicatesFilterPlan)
				if !ok {
					t.Fatalf("plan below FirstOrDefault = %T, want predicate filter",
						belowFOD)
				}
				filterCount++
				if belowFODFilter.GetInnerAlias() != innerCorrelation {
					t.Fatalf("below-FOD binding alias = %v, want %v",
						belowFODFilter.GetInnerAlias(), innerCorrelation)
				}
				preds := belowFODFilter.GetPredicates()
				if len(preds) != 1 || preds[0] != belowFODPredicate {
					t.Fatalf("below-FOD predicates = %v, want supplied predicate %v",
						preds, belowFODPredicate)
				}
				aliases = append(aliases,
					belowFODFilter.GetInnerQuantifier().GetAlias())
				belowFOD = belowFODFilter.GetInner()
			}
			if _, ok := belowFOD.(*plans.RecordQueryScanPlan); !ok {
				t.Fatalf("plan below compensation chain = %T, want scan",
					belowFOD)
			}
			if filterCount != test.wantFilterCount {
				t.Fatalf("predicate-filter count = %d, want %d",
					filterCount, test.wantFilterCount)
			}

			if test.wantAliasDistinct {
				for i := range aliases {
					for j := i + 1; j < len(aliases); j++ {
						if aliases[i] == aliases[j] {
							t.Fatalf("fresh aliases[%d] and [%d] both equal %v: %v",
								i, j, aliases[i], aliases)
						}
					}
				}
			} else {
				for i, alias := range aliases {
					if alias != innerCorrelation {
						t.Fatalf("preserved alias[%d] = %v, want %v: %v",
							i, alias, innerCorrelation, aliases)
					}
				}
			}
		})
	}
}

func rfc190FinalPlanForQuantifier(
	t *testing.T,
	quantifier expressions.Quantifier,
) plans.RecordQueryPlan {
	t.Helper()

	ref := quantifier.GetRangesOver()
	if ref == nil {
		t.Fatal("quantifier has no reference")
	}
	members := ref.FinalMembers()
	if len(members) != 1 {
		t.Fatalf("quantifier final-member count = %d, want 1", len(members))
	}
	plan, ok := members[0].(plans.RecordQueryPlan)
	if !ok {
		t.Fatalf("quantifier final member = %T, want physical plan", members[0])
	}
	return plan
}
