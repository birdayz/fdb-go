package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRFC190ExistsConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct RFC-190 EXISTS fixture: " + err.Error())
	}
	return value
}

func rfc190ExistsRowType() *values.RecordType {
	return values.NewRecordType("RFC190ExistsRow", false, []values.Field{{
		Name:      "K",
		FieldType: values.NullableLong,
	}})
}

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
			scan := mustRFC190ExistsConstruct(plans.NewRecordQueryScanPlan(
				[]string{"T"}, rfc190ExistsRowType(), false))
			call := NewExpressionRuleCall(expressions.InitialOf(scan), nil, nil)
			baseQ := expressions.NamedPhysicalQuantifier(
				baseAlias, call.MemoizeExpression(scan))
			innerObject := mustRFC190ExistsConstruct(values.NewQuantifiedObjectValue(
				innerCorrelation, rfc190ExistsRowType()))
			innerK := mustRFC190ExistsConstruct(values.ResolveFieldOrdinals(
				innerObject, []int{0}))
			belowFODPredicate := predicates.NewComparisonPredicate(
				innerK,
				predicates.Comparison{Type: predicates.ComparisonIsNotNull},
			)
			var belowFODPredicates []predicates.QueryPredicate
			if test.withBelowFOD {
				belowFODPredicates = []predicates.QueryPredicate{
					belowFODPredicate,
				}
			}

			finalQ := mustRFC190ExistsConstruct(buildExistsCompensationChain(
				call,
				baseQ,
				scan,
				innerCorrelation,
				belowFODPredicates,
				test.hasExistsFilter,
				test.negated,
				test.preserveAlias,
			))

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
				qov, ok := values.AsQuantifiedObjectValue(residual.Operand)
				if !ok {
					t.Fatalf("residual operand = %T, want quantified object",
						residual.Operand)
				}
				residualLayout, err := residualFilter.GetInner().ProvidedOutputLayout()
				if err != nil {
					t.Fatalf("residual input layout: %v", err)
				}
				if qov != residualLayout.Carrier() {
					t.Fatalf("residual operand = %v/%p, want exact FirstOrDefault carrier %v/%p",
						qov.Correlation(), qov, residualLayout.Carrier().Correlation(), residualLayout.Carrier())
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
				if len(preds) != 1 {
					t.Fatalf("below-FOD predicate count = %d, want 1", len(preds))
				}
				comparison, ok := preds[0].(*predicates.ComparisonPredicate)
				if !ok || comparison.Comparison.Type != predicates.ComparisonIsNotNull {
					t.Fatalf("below-FOD predicate = %T/%v, want IS NOT NULL comparison",
						preds[0], preds[0])
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

func TestRFC190ExistsResidualUsesWholeFODCarrierBesideSameAliasRetainedWindow(t *testing.T) {
	t.Parallel()
	bType := values.NewRecordType("BOOKS", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "AUTHOR_ID", FieldType: values.NullableLong},
	})
	wType := values.NewRecordType("AWARDS", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "BOOK_ID", FieldType: values.NullableLong},
		{Name: "PRIZE", FieldType: values.NullableString},
	})
	bAlias := values.NamedCorrelationIdentifier("B")
	// Reuse W as both the existential bookkeeping alias and a genuine buried
	// source alias. That is the collision the residual must not resolve through.
	wAlias := values.NamedCorrelationIdentifier("W")
	bScan := mustRFC190ExistsConstruct(plans.NewRecordQueryScanPlan(
		[]string{"BOOKS"}, bType, false))
	wScan := mustRFC190ExistsConstruct(plans.NewRecordQueryScanPlan(
		[]string{"AWARDS"}, wType, false))
	bQ := expressions.NamedPhysicalQuantifier(
		bAlias, expressions.FinalOfAtStage(bScan, expressions.StageCanonical))
	wQ := expressions.NamedPhysicalQuantifier(
		wAlias, expressions.FinalOfAtStage(wScan, expressions.StageCanonical))
	bRoot := mustRFC190ExistsConstruct(bQ.RequireFlowedObjectValue())
	wRoot := mustRFC190ExistsConstruct(wQ.RequireFlowedObjectValue())
	bID := mustRFC190ExistsConstruct(values.ResolveOrdinalSeedField(bRoot, 0))
	bAuthor := mustRFC190ExistsConstruct(values.ResolveOrdinalSeedField(bRoot, 1))
	wID := mustRFC190ExistsConstruct(values.ResolveOrdinalSeedField(wRoot, 0))
	wBook := mustRFC190ExistsConstruct(values.ResolveOrdinalSeedField(wRoot, 1))
	wPrize := mustRFC190ExistsConstruct(values.ResolveOrdinalSeedField(wRoot, 2))
	join := mustRFC190ExistsConstruct(plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
		bQ, wQ, nil, plans.JoinInner, bAlias, wAlias,
		values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "B_ID", Value: bID},
			values.RecordConstructorField{Name: "AUTHOR_ID", Value: bAuthor},
			values.RecordConstructorField{Name: "W_ID", Value: wID},
			values.RecordConstructorField{Name: "BOOK_ID", Value: wBook},
			values.RecordConstructorField{Name: "PRIZE", Value: wPrize}),
	))
	joinLayout := mustRFC190ExistsConstruct(join.ProvidedOutputLayout())
	if provided, provideErr := values.LayoutProvides(joinLayout, wRoot); provideErr != nil || !provided {
		t.Fatalf("join retained W/AWARDS window = (%v, %v), want (true, nil)", provided, provideErr)
	}
	call := NewExpressionRuleCall(expressions.InitialOf(join), nil, nil)
	baseQ := expressions.NamedPhysicalQuantifier(
		wAlias, call.MemoizeFinalExpression(join))
	finalQ := mustRFC190ExistsConstruct(buildExistsCompensationChain(
		call, baseQ, join, wAlias, nil, true, false, true))

	residualFilter, ok := rfc190FinalPlanForQuantifier(t, finalQ).(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("top plan = %T, want existential residual filter",
			rfc190FinalPlanForQuantifier(t, finalQ))
	}
	preds := residualFilter.GetPredicates()
	if len(preds) != 1 {
		t.Fatalf("residual predicate count = %d, want 1", len(preds))
	}
	comparison, ok := preds[0].(*predicates.ComparisonPredicate)
	if !ok || comparison.Comparison.Type != predicates.ComparisonIsNotNull {
		t.Fatalf("residual predicate = %T/%v, want IS NOT NULL", preds[0], preds[0])
	}
	whole, ok := values.AsQuantifiedObjectValue(comparison.Operand)
	if !ok {
		t.Fatalf("residual operand = %T, want exact whole-row QOV", comparison.Operand)
	}
	fod, ok := residualFilter.GetInner().(*plans.RecordQueryFirstOrDefaultPlan)
	if !ok {
		t.Fatalf("residual input = %T, want FirstOrDefault", residualFilter.GetInner())
	}
	fodLayout, err := fod.ProvidedOutputLayout()
	if err != nil {
		t.Fatal(err)
	}
	if whole != fodLayout.Carrier() {
		t.Fatalf("residual operand = %v/%p, want exact FOD carrier %v/%p",
			whole.Correlation(), whole, fodLayout.Carrier().Correlation(), fodLayout.Carrier())
	}
	provided, err := values.LayoutProvides(fodLayout, wRoot)
	if err != nil || !provided {
		t.Fatalf("FOD retained W/AWARDS window = (%v, %v), want (true, nil)", provided, err)
	}
	if whole == wRoot || whole.FlowedType().Equals(wRoot.FlowedType()) {
		t.Fatalf("whole residual collapsed onto narrow W/AWARDS window: whole=%v W=%v",
			whole.FlowedType(), wRoot.FlowedType())
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
