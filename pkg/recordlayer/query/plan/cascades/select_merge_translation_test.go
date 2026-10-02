package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSelectMergeTranslationPreservesScopeAndMembers(t *testing.T) {
	t.Parallel()
	for _, capturing := range []bool{false, true} {
		name := "shadowed"
		if capturing {
			name = "capture_avoided"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			from := values.NamedCorrelationIdentifier("BOX")
			to := values.NamedCorrelationIdentifier("LEG")
			bound := from
			if capturing {
				bound = to
			}
			local := expressions.NamedForEachQuantifier(bound, expressions.InitialOf(selectMergeScan(t)))
			rv := values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "LOCAL", Value: selectMergeFlowed(t, local)},
				values.RecordConstructorField{Name: "OUTER", Value: selectMergeQOV(t, from, selectMergeTestRowType())},
			)
			sel := selectMergeSelectWithAliases(t, rv, []expressions.Quantifier{local}, nil, []string{"local"})
			ref := expressions.InitialOf(sel)
			tr := newSelectMergeTranslation(NewExpressionRuleCall(ref, nil, nil))
			tr.add(from, selectMergeQOV(t, to, selectMergeTestRowType()), false)
			translated, err := tr.reference(ref)
			if err != nil {
				t.Fatal(err)
			}
			if !capturing {
				if translated != ref {
					t.Fatal("locally bound source was rewritten")
				}
				return
			}
			got := translated.Get().(*expressions.SelectExpression)
			if got.GetQuantifiers()[0].GetAlias() == to {
				t.Fatal("local binder captured the substituted outer leg")
			}
			fields := got.GetResultValue().(*values.RecordConstructorValue).Fields
			localQOV, _ := values.AsQuantifiedObjectValue(fields[0].Value)
			outerQOV, _ := values.AsQuantifiedObjectValue(fields[1].Value)
			if localQOV.Correlation() != got.GetQuantifiers()[0].GetAlias() || outerQOV.Correlation() != to {
				t.Fatalf("translated output lost binding ownership: %s", values.ExplainValue(got.GetResultValue()))
			}
			if free := translated.GetCorrelatedTo(); len(free) != 1 {
				t.Fatalf("free correlations = %#v, want the outer leg", free)
			} else if _, found := free[to]; !found {
				t.Fatalf("outer leg not free: %#v", free)
			}
		})
	}
}

func TestSelectMergeTranslationPreservesLanesFlagsAndSharedGraphs(t *testing.T) {
	t.Parallel()
	from := values.NamedCorrelationIdentifier("BOX")
	to := values.NamedCorrelationIdentifier("LEG")
	local := expressions.ForEachQuantifier(expressions.InitialOf(selectMergeScan(t)))
	outerField := selectMergeFieldOrdinals(t, selectMergeQOV(t, from, selectMergeTestRowType()), 0)
	filter := selectMergeFilter(t, []predicates.QueryPredicate{predicates.NewComparisonPredicate(outerField, literalCmp(predicates.ComparisonEquals, int64(1)))}, local)
	ref := expressions.InitialOf(filter)
	ref.InsertFinal(selectMergeSelect(t, selectMergeFlowed(t, local), []expressions.Quantifier{local}, filter.GetPredicates()))
	tr := newSelectMergeTranslation(NewExpressionRuleCall(ref, nil, nil))
	tr.add(from, selectMergeQOV(t, to, selectMergeTestRowType()), false)
	qs := []expressions.Quantifier{
		expressions.NamedForEachNullOnEmptyQuantifier(values.UniqueCorrelationIdentifier(), ref),
		expressions.NamedForEachStrictSingleQuantifier(values.UniqueCorrelationIdentifier(), ref),
		expressions.ExistentialQuantifier(ref),
		expressions.NewPhysicalQuantifier(ref),
	}
	var shared *expressions.Reference
	for _, q := range qs {
		translated, err := tr.quantifier(q)
		if err != nil {
			t.Fatal(err)
		}
		if translated.GetAlias() != q.GetAlias() || translated.Kind() != q.Kind() || translated.IsNullOnEmpty() != q.IsNullOnEmpty() || translated.IsStrictSingle() != q.IsStrictSingle() {
			t.Fatalf("changed quantifier identity/flags: %#v -> %#v", q, translated)
		}
		child := translated.GetRangesOver()
		if shared != nil && child != shared {
			t.Fatal("shared graph was duplicated")
		}
		shared = child
		if child == ref || child.Stage() != ref.Stage() || len(child.Members()) != 1 || len(child.FinalMembers()) != 1 {
			t.Fatal("translation lost member lanes or stage")
		}
		if free := child.GetCorrelatedTo(); len(free) != 1 {
			t.Fatalf("translated child correlations = %#v", free)
		} else if _, found := free[to]; !found {
			t.Fatalf("translated child lacks target correlation: %#v", free)
		}
	}
	if _, stillOriginal := ref.GetCorrelatedTo()[from]; !stillOriginal {
		t.Fatal("translation mutated the source graph")
	}
}

func TestSelectMergeTranslationTranslatesAggregatePrograms(t *testing.T) {
	t.Parallel()
	from := values.NamedCorrelationIdentifier("BOX")
	to := values.NamedCorrelationIdentifier("LEG")
	local := expressions.ForEachQuantifier(expressions.InitialOf(selectMergeScan(t)))
	outerField := selectMergeFieldOrdinals(t, selectMergeQOV(t, from, selectMergeTestRowType()), 0)
	group, err := expressions.NewGroupByExpression([]values.Value{outerField}, []expressions.AggregateSpec{{Function: expressions.AggMax, Operand: outerField, Alias: "MAX_OUTER"}}, local)
	if err != nil {
		t.Fatal(err)
	}
	ref := expressions.InitialOf(group)
	if _, free := ref.GetCorrelatedTo()[from]; !free {
		t.Fatal("aggregate operands/grouping keys hide their outer dependency")
	}
	tr := newSelectMergeTranslation(NewExpressionRuleCall(ref, nil, nil))
	tr.add(from, selectMergeQOV(t, to, selectMergeTestRowType()), false)
	translated, err := tr.reference(ref)
	if err != nil {
		t.Fatal(err)
	}
	got := translated.Get().(*expressions.GroupByExpression)
	for _, value := range []values.Value{got.GetGroupingKeys()[0], got.GetAggregates()[0].Operand} {
		correlations := values.GetCorrelatedToOfValue(value)
		if len(correlations) != 1 {
			t.Fatalf("aggregate program correlations = %#v", correlations)
		}
		if _, found := correlations[to]; !found {
			t.Fatalf("aggregate program retained dissolved alias: %s", values.ExplainValue(value))
		}
	}
	if got.GetAggregates()[0].Alias != "MAX_OUTER" {
		t.Fatal("translation lost aggregate metadata")
	}
}

func TestSelectMergeTranslationUnaryInputDoesNotBindItsOwnAlias(t *testing.T) {
	t.Parallel()
	from := values.NamedCorrelationIdentifier("BOX")
	to := values.NamedCorrelationIdentifier("LEG")
	local := expressions.ForEachQuantifier(expressions.InitialOf(selectMergeScan(t)))
	filter := selectMergeFilter(t, []predicates.QueryPredicate{predicates.NewComparisonPredicate(
		selectMergeFieldOrdinals(t, selectMergeQOV(t, from, selectMergeTestRowType()), 0), literalCmp(predicates.ComparisonEquals, int64(1)))}, local)
	input := expressions.NamedForEachQuantifier(from, expressions.InitialOf(filter))
	projection, err := expressions.NewLogicalProjectionExpression([]values.Value{selectMergeFlowed(t, input)}, input)
	if err != nil {
		t.Fatal(err)
	}
	ref := expressions.InitialOf(projection)
	tr := newSelectMergeTranslation(NewExpressionRuleCall(ref, nil, nil))
	tr.add(from, selectMergeQOV(t, to, selectMergeTestRowType()), false)
	translated, err := tr.reference(ref)
	if err != nil {
		t.Fatal(err)
	}
	got := translated.Get().(*expressions.LogicalProjectionExpression)
	qov, _ := values.AsQuantifiedObjectValue(got.GetProjectedValues()[0])
	if qov.Correlation() != from {
		t.Fatal("projection's local row binding changed")
	}
	if _, dangling := translated.GetCorrelatedTo()[from]; dangling {
		t.Fatal("unary input incorrectly shadowed the external BOX binding")
	}
	if _, free := translated.GetCorrelatedTo()[to]; !free {
		t.Fatal("translated unary input lost the external LEG binding")
	}
}
