package cascades

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustPushFilterJoinConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct push-filter-below-join fixture: " + err.Error())
	}
	return value
}

func pushFilterJoinRowType(name string) *values.RecordType {
	return values.NewRecordType(name, false, []values.Field{
		{Name: "NAME", FieldType: values.NotNullString},
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "STATUS", FieldType: values.NotNullString},
	})
}

func pushFilterJoinScan(name string) *expressions.FullUnorderedScanExpression {
	return mustPushFilterJoinConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{name}, pushFilterJoinRowType(name)))
}

func pushFilterJoinQOV(alias, rowName string) values.QuantifiedObjectValue {
	return mustPushFilterJoinConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(alias), pushFilterJoinRowType(rowName)))
}

func pushFilterJoinField(alias, rowName, field string) values.Value {
	request := mustPushFilterJoinConstruct(values.FieldByName(field))
	return mustPushFilterJoinConstruct(values.ResolveFieldAccess(
		pushFilterJoinQOV(alias, rowName), []values.FieldRequest{request}))
}

func explorePushFilterJoinRewriting(
	planner *Planner,
	root *expressions.Reference,
) (int, bool) {
	if root == nil {
		return 0, true
	}
	if planner.memo == nil {
		planner.memo = NewMemo(root)
	}
	if planner.constraintMap == nil {
		planner.constraintMap = NewConstraintMap()
	}
	if planner.dataAccessConsumed == nil {
		planner.dataAccessConsumed = make(map[*expressions.Reference]int)
	}
	planner.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: root})
	planner.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: root})
	for len(planner.stack) > 0 {
		if planner.tasksRun >= planner.MaxTasks {
			return planner.tasksRun, false
		}
		planner.pop().Run(context.Background(), planner)
		planner.tasksRun++
		if planner.capErr != nil {
			return planner.tasksRun, false
		}
	}
	return planner.tasksRun, true
}

// Every column reference in this file is a QOV-rooted `FieldValue` — the shape
// the translator actually emits. They used to be childless values whose Field
// packed the qualifier into the name (`Field: "A.NAME"`), the legacy flat
// representation RFC-197's dotted bucket exists to remove. That mattered here
// because predicateSingleSide decides which SIDE of the join a predicate
// belongs to: reading the side out of the spelling meant these tests exercised
// a channel production no longer uses (0 of 1944 calls over the explaindiff
// corpus take a dotted read), while the QOV path the rule really runs on went
// unprobed. The rule now asks the value for its correlation, so the fixtures
// state one.
//
// buildJoinTree constructs:
//
//	Filter(filterPreds, Select(rv, [qA, qB], joinPreds, aliases))
//
// The Select has two ForEach quantifiers over scans of A and B.
func buildJoinTree(
	filterPreds []predicates.QueryPredicate,
	joinPreds []predicates.QueryPredicate,
	aliases []string,
) *expressions.Reference {
	scanA := pushFilterJoinScan("A")
	scanAQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("A"), expressions.InitialOf(scanA))
	scanB := pushFilterJoinScan("B")
	scanBQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("B"), expressions.InitialOf(scanB))

	rv := mustPushFilterJoinConstruct(scanAQ.RequireFlowedObjectValue())
	sel := mustPushFilterJoinConstruct(expressions.NewSelectExpressionWithJoinType(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		joinPreds,
		aliases,
		expressions.JoinInner,
	))
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(filterPreds, selQ))
	return expressions.InitialOf(filter)
}

func TestPushFilterBelowJoin_SingleSidePredicate(t *testing.T) {
	t.Parallel()

	// Predicate: A.NAME = 'foo' — references only alias A.
	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "NAME"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	// Result should be a Select (filter was completely pushed below).
	newSel, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *SelectExpression", yielded[0])
	}

	// The first quantifier should now range over a filter.
	qs := newSel.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("quantifier count %d, want 2", len(qs))
	}

	innerA := qs[0].GetRangesOver().Get()
	filterA, ok := innerA.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalFilterExpression", innerA)
	}
	if len(filterA.GetPredicates()) != 1 {
		t.Fatalf("pushed filter predicate count %d, want 1", len(filterA.GetPredicates()))
	}

	// The second quantifier should still be a raw scan (no filter pushed).
	innerB := qs[1].GetRangesOver().Get()
	if _, ok := innerB.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("quantifier 1 inner = %T, want *FullUnorderedScanExpression", innerB)
	}
}

func TestPushFilterBelowJoin_BothSidePredicate(t *testing.T) {
	t.Parallel()

	// Predicate: A.ID = B.ID — references both aliases.
	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "ID"),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: pushFilterJoinField("B", "B", "ID"),
		},
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (both-side predicate can't be pushed)", len(yielded))
	}
}

func TestPushFilterBelowJoin_MixedPredicates(t *testing.T) {
	t.Parallel()

	// Predicate 1: A.NAME = 'foo' — only side A.
	predA := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "NAME"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)
	// Predicate 2: A.ID = B.ID — both sides.
	predBoth := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "ID"),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: pushFilterJoinField("B", "B", "ID"),
		},
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{predA, predBoth},
		nil,
		[]string{"A", "B"},
	)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	// Result should be Filter([A.ID=B.ID], Select(rv, [qA_filtered, qB], ...))
	newFilter, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalFilterExpression", yielded[0])
	}
	if len(newFilter.GetPredicates()) != 1 {
		t.Fatalf("remaining filter predicates %d, want 1", len(newFilter.GetPredicates()))
	}

	innerSel := newFilter.GetInner().GetRangesOver().Get()
	sel, ok := innerSel.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner = %T, want *SelectExpression", innerSel)
	}

	// Side A should have the pushed filter.
	qs := sel.GetQuantifiers()
	innerA := qs[0].GetRangesOver().Get()
	if _, ok := innerA.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalFilterExpression", innerA)
	}

	// Side B should be untouched.
	innerB := qs[1].GetRangesOver().Get()
	if _, ok := innerB.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("quantifier 1 inner = %T, want *FullUnorderedScanExpression", innerB)
	}
}

func TestPushFilterBelowJoin_NoAliases(t *testing.T) {
	t.Parallel()

	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "NAME"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	// Build with no aliases — rule should not fire.
	scanA := pushFilterJoinScan("A")
	scanAQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("A"), expressions.InitialOf(scanA))
	scanB := pushFilterJoinScan("B")
	scanBQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("B"), expressions.InitialOf(scanB))

	rv := mustPushFilterJoinConstruct(scanAQ.RequireFlowedObjectValue())
	sel := mustPushFilterJoinConstruct(expressions.NewSelectExpression(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
	))
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred}, selQ))
	ref := expressions.InitialOf(filter)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on no-alias join, want 0", len(yielded))
	}
}

func TestPushFilterBelowJoin_PushToSideB(t *testing.T) {
	t.Parallel()

	// Predicate: B.STATUS = 'active' — references only alias B.
	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("B", "B", "STATUS"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newSel, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *SelectExpression", yielded[0])
	}

	qs := newSel.GetQuantifiers()

	// Side A should be untouched.
	innerA := qs[0].GetRangesOver().Get()
	if _, ok := innerA.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("quantifier 0 inner = %T, want *FullUnorderedScanExpression", innerA)
	}

	// Side B should have the pushed filter.
	innerB := qs[1].GetRangesOver().Get()
	if _, ok := innerB.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("quantifier 1 inner = %T, want *LogicalFilterExpression", innerB)
	}
}

func TestPushFilterBelowJoin_FixpointTerminates(t *testing.T) {
	t.Parallel()

	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "NAME"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	progress, converged := explorePushFilterJoinRewriting(
		NewPlanner([]ExpressionRule{NewPushFilterBelowJoinRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}

func TestPushFilterBelowJoin_ConstantPredicate_NoFieldRefs(t *testing.T) {
	t.Parallel()

	// A constant predicate has no FieldValue references — should not be pushed.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (constant predicate has no field refs)", len(yielded))
	}
}

func TestPushFilterBelowJoin_LeftOuterJoin_Skips(t *testing.T) {
	t.Parallel()

	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "NAME"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	// Build LEFT OUTER join — rule should not fire.
	scanA := pushFilterJoinScan("A")
	scanAQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("A"), expressions.InitialOf(scanA))
	scanB := pushFilterJoinScan("B")
	scanBQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("B"), expressions.InitialOf(scanB))

	rv := mustPushFilterJoinConstruct(scanAQ.RequireFlowedObjectValue())
	sel := mustPushFilterJoinConstruct(expressions.NewSelectExpressionWithJoinType(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
		[]string{"A", "B"},
		expressions.JoinLeftOuter,
	))
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred}, selQ))
	ref := expressions.InitialOf(filter)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on LEFT OUTER join, want 0", len(yielded))
	}
}

func TestPushFilterBelowJoin_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	scanARef := expressions.InitialOf(pushFilterJoinScan("A"))
	scanBRef := expressions.InitialOf(pushFilterJoinScan("B"))
	qA := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("A"), scanARef)
	qB := expressions.NamedForEachStrictSingleQuantifier(
		values.NamedCorrelationIdentifier("B"), scanBRef)
	sel := mustPushFilterJoinConstruct(expressions.NewSelectExpressionWithJoinType(
		mustPushFilterJoinConstruct(qA.RequireFlowedObjectValue()),
		[]expressions.Quantifier{qA, qB},
		nil,
		[]string{"A", "B"},
		expressions.JoinInner,
	))
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
			pushFilterJoinField("B", "B", "STATUS"),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
		)},
		filterQ,
	))

	yielded := mustFireExpressionRule(t,
		NewPushFilterBelowJoinRule(), expressions.InitialOf(filter))
	if len(yielded) != 0 {
		t.Fatalf("strict-single join yielded %d filter-push rewrite(s), want zero", len(yielded))
	}
}

func TestPushFilterBelowJoin_BothSidesPushed(t *testing.T) {
	t.Parallel()

	// Two predicates, each referencing a different side.
	predA := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "NAME"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)
	predB := predicates.NewComparisonPredicate(
		pushFilterJoinField("B", "B", "STATUS"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{predA, predB},
		nil,
		[]string{"A", "B"},
	)

	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	// All filter preds pushed — result is a bare Select.
	newSel, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *SelectExpression", yielded[0])
	}

	qs := newSel.GetQuantifiers()

	// Both sides should have filters and keep the aliases read by the Select.
	for i, alias := range []string{"A", "B"} {
		if qs[i].GetAlias() != values.NamedCorrelationIdentifier(alias) {
			t.Errorf("replacement edge %d changed owned alias %s to %s", i, alias, qs[i].GetAlias())
		}
	}
	innerA := qs[0].GetRangesOver().Get()
	if _, ok := innerA.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalFilterExpression", innerA)
	}

	innerB := qs[1].GetRangesOver().Get()
	if _, ok := innerB.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("quantifier 1 inner = %T, want *LogicalFilterExpression", innerB)
	}
}

func TestPushFilterBelowJoin_NestedDependencyStaysAboveJoin(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("B", false, []values.Field{
		{Name: "N", FieldType: values.NewRecordType("N", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong},
		})},
	})
	qov := mustPushFilterJoinConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("B"), rowType))
	nested := mustPushFilterJoinConstruct(values.ResolveFieldAccess(qov, []values.FieldRequest{
		mustPushFilterJoinConstruct(values.FieldByName("N")),
		mustPushFilterJoinConstruct(values.FieldByName("ID")),
	}))
	pred := predicates.NewComparisonPredicate(
		pushFilterJoinField("A", "A", "ID"),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: nested})
	if got := predicateSingleSide(pred, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B")); got != -1 {
		t.Errorf("A.ID = B.N.ID classified as side %d; both correlations must keep it above the join", got)
	}

	qA := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), expressions.InitialOf(pushFilterJoinScan("A")))
	qB := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("B"), expressions.InitialOf(
		mustPushFilterJoinConstruct(expressions.NewFullUnorderedScanExpression([]string{"B"}, rowType))))
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: mustPushFilterJoinConstruct(qA.RequireFlowedObjectValue())},
		values.RecordConstructorField{Name: "B", Value: mustPushFilterJoinConstruct(qB.RequireFlowedObjectValue())})
	join := mustPushFilterJoinConstruct(expressions.NewSelectExpressionWithJoinType(
		result, []expressions.Quantifier{qA, qB}, nil, []string{"A", "B"}, expressions.JoinInner))
	filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(expressions.InitialOf(join))))
	if yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), expressions.InitialOf(filter)); len(yielded) != 0 {
		t.Errorf("two-leg nested predicate yielded %d unsafe pushdown(s)", len(yielded))
	}

	pushable := predicates.NewComparisonPredicate(pushFilterJoinField("A", "A", "ID"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
	mixed := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pushable, pred}, expressions.ForEachQuantifier(expressions.InitialOf(join))))
	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), expressions.InitialOf(mixed))
	if len(yielded) != 1 {
		t.Fatalf("mixed predicates yielded %d alternatives, want one", len(yielded))
	}
	residual, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok || len(residual.GetPredicates()) != 1 || residual.GetPredicates()[0] != pred {
		t.Fatalf("nested two-leg predicate was not retained above the rewritten join: %T", yielded[0])
	}
	newJoin, ok := residual.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatal("residual no longer wraps a join")
	}
	pushed, ok := newJoin.GetQuantifiers()[0].GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok || len(pushed.GetPredicates()) != 1 || pushed.GetPredicates()[0] != pushable {
		t.Fatal("single-side predicate did not push into A")
	}
}

func TestPredicateSingleSide_CompleteCorrelations(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("R", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "N", FieldType: values.NewRecordType("N", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong},
		})},
	})
	root := func(alias string) values.QuantifiedObjectValue {
		return mustPushFilterJoinConstruct(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), rowType))
	}
	field := func(alias string, names ...string) values.Value {
		var path []values.FieldRequest
		for _, name := range names {
			path = append(path, mustPushFilterJoinConstruct(values.FieldByName(name)))
		}
		return mustPushFilterJoinConstruct(values.ResolveFieldAccess(root(alias), path))
	}
	eq := func(lhs, rhs values.Value) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(lhs, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: rhs})
	}
	constant := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	a := eq(field("A", "ID"), constant)
	rangeOnB := predicates.NewPredicateWithValueAndRanges(constant, []*predicates.RangeConstraints{
		predicates.NewRangeConstraints(nil, []predicates.Comparison{
			{Type: predicates.ComparisonLessThan, Operand: field("B", "N", "ID")},
		}),
	})
	for _, tc := range []struct {
		name string
		pred predicates.QueryPredicate
		corr []string
		want int
	}{
		{"left", a, []string{"A"}, 0},
		{"right", eq(field("B", "ID"), constant), []string{"B"}, 1},
		{"both_direct", eq(field("A", "ID"), field("B", "ID")), []string{"A", "B"}, -1},
		{"nested_right", eq(field("A", "ID"), field("B", "N", "ID")), []string{"A", "B"}, -1},
		{"nested_left", eq(field("B", "ID"), field("A", "N", "ID")), []string{"A", "B"}, -1},
		{"both_nested", eq(field("A", "N", "ID"), field("B", "N", "ID")), []string{"A", "B"}, -1},
		{"only_nested_left", eq(field("A", "N", "ID"), constant), []string{"A"}, 0},
		{"only_nested_right", eq(field("B", "N", "ID"), constant), []string{"B"}, 1},
		{"whole_rows", eq(root("A"), root("B")), []string{"A", "B"}, -1},
		{"non_field_sibling", predicates.NewAnd(a, predicates.NewValuePredicate(values.NewExistsValueWithChild(root("B")))), []string{"A", "B"}, -1},
		{"existential_predicate_sibling", predicates.NewAnd(a, predicates.MustNewExistentialValuePredicate(root("B"), predicates.Comparison{Type: predicates.ComparisonIsNotNull})), []string{"A", "B"}, -1},
		{"range_sibling", predicates.NewAnd(a, rangeOnB), []string{"A", "B"}, -1},
		{"range_right", rangeOnB, []string{"B"}, 1},
		{"not_or_sibling", predicates.NewNot(predicates.NewOr(a, rangeOnB)), []string{"A", "B"}, -1},
		{"external_only", eq(field("OUTER", "ID"), constant), []string{"OUTER"}, -1},
		{"left_and_external", eq(field("A", "ID"), field("OUTER", "ID")), []string{"A", "OUTER"}, 0},
		{"constant", predicates.NewConstantPredicate(predicates.TriTrue), nil, -1},
		{"nil", nil, nil, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			correlations := predicates.GetCorrelatedToOfPredicate(tc.pred)
			if len(correlations) != len(tc.corr) {
				t.Fatalf("fixture correlations = %v, want %v", correlations, tc.corr)
			}
			for _, alias := range tc.corr {
				if _, ok := correlations[values.NamedCorrelationIdentifier(alias)]; !ok {
					t.Fatalf("fixture does not depend on %s", alias)
				}
			}
			if got := predicateSingleSide(tc.pred, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B")); got != tc.want {
				t.Errorf("side = %d, want %d for correlations %v", got, tc.want, tc.corr)
			}
		})
	}
}

func TestPushFilterBelowJoin_AliasAgreement(t *testing.T) {
	t.Parallel()
	for _, aliases := range [][]string{{"A", "C"}, {"B", "A"}, {"A", "A"}, {"A"}} {
		t.Run(strings.Join(aliases, ","), func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(pushFilterJoinField("A", "A", "ID"),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
			ref := buildJoinTree([]predicates.QueryPredicate{pred}, nil, aliases)
			if yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), ref); len(yielded) != 0 {
				t.Errorf("source aliases %v do not state the owned A/B quantifiers, but yielded %d rewrite(s)", aliases, len(yielded))
			}
		})
	}
}

func TestPushFilterBelowJoin_NullOnEmptyBarrier(t *testing.T) {
	t.Parallel()
	for _, side := range []int{0, 1} {
		t.Run([]string{"left", "right"}[side], func(t *testing.T) {
			t.Parallel()
			qs := []expressions.Quantifier{
				expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), expressions.InitialOf(pushFilterJoinScan("A"))),
				expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("B"), expressions.InitialOf(pushFilterJoinScan("B"))),
			}
			qs[side] = expressions.NamedForEachNullOnEmptyQuantifier(qs[side].GetAlias(), qs[side].GetRangesOver())
			join := mustPushFilterJoinConstruct(expressions.NewSelectExpressionWithJoinType(
				mustPushFilterJoinConstruct(qs[0].RequireFlowedObjectValue()), qs, nil, []string{"A", "B"}, expressions.JoinInner))
			alias := []string{"A", "B"}[side]
			pred := predicates.NewComparisonPredicate(pushFilterJoinField(alias, alias, "ID"),
				predicates.Comparison{Type: predicates.ComparisonIsNull})
			filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(expressions.InitialOf(join))))
			if yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), expressions.InitialOf(filter)); len(yielded) != 0 {
				t.Errorf("null-on-empty edge yielded %d rewrite(s) moving IS NULL before null extension", len(yielded))
			}
		})
	}
}

func TestPushFilterBelowJoin_UniqueAliasIdentity(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	qA := expressions.NamedForEachQuantifier(alias, expressions.InitialOf(pushFilterJoinScan("A")))
	qB := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("B"), expressions.InitialOf(pushFilterJoinScan("B")))
	root := mustPushFilterJoinConstruct(qA.RequireFlowedObjectValue())
	field := mustPushFilterJoinConstruct(values.ResolveFieldAccess(root, []values.FieldRequest{
		mustPushFilterJoinConstruct(values.FieldByName("ID")),
	}))
	pred := predicates.NewComparisonPredicate(field, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	if got := predicateSingleSide(pred, alias, qB.GetAlias()); got != 0 {
		t.Fatalf("unique-kind dependency classified as %d, want left", got)
	}
	if got := predicateSingleSide(pred, values.NamedCorrelationIdentifier(alias.Name()), qB.GetAlias()); got != -1 {
		t.Fatalf("named alias impersonated unique-kind alias: side %d", got)
	}
	if got := predicateSingleSide(pred, alias, alias); got != -1 {
		t.Fatalf("duplicate owned identities must decline naturally, got side %d", got)
	}
	join := mustPushFilterJoinConstruct(expressions.NewSelectExpressionWithJoinType(
		root, []expressions.Quantifier{qA, qB}, nil, []string{alias.Name(), "B"}, expressions.JoinInner))
	filter := mustPushFilterJoinConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(expressions.InitialOf(join))))
	yielded := mustFireExpressionRule(t, NewPushFilterBelowJoinRule(), expressions.InitialOf(filter))
	if len(yielded) != 1 {
		t.Fatalf("unique-kind alias silently disabled pushdown: yielded %d, want one", len(yielded))
	}
	newJoin, ok := yielded[0].(*expressions.SelectExpression)
	if !ok || newJoin.GetQuantifiers()[0].GetAlias() != alias {
		t.Fatalf("pushed Select lost owned unique-kind identity: %T", yielded[0])
	}
	pushed, ok := newJoin.GetQuantifiers()[0].GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok || pushed.GetInner().GetAlias() != alias {
		t.Fatal("pushed filter lost owned unique-kind identity")
	}
}
