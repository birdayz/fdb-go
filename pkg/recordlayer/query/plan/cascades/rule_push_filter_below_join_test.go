package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

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
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	rv := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	sel := expressions.NewSelectExpressionWithJoinType(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		joinPreds,
		aliases,
		expressions.JoinInner,
	)
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := expressions.NewLogicalFilterExpression(filterPreds, selQ)
	return expressions.InitialOf(filter)
}

func TestPushFilterBelowJoin_SingleSidePredicate(t *testing.T) {
	t.Parallel()

	// Predicate: A.NAME = 'foo' — references only alias A.
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
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
		&values.FieldValue{Field: "A.ID", Typ: values.TypeInt},
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.FieldValue{Field: "B.ID", Typ: values.TypeInt},
		},
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (both-side predicate can't be pushed)", len(yielded))
	}
}

func TestPushFilterBelowJoin_MixedPredicates(t *testing.T) {
	t.Parallel()

	// Predicate 1: A.NAME = 'foo' — only side A.
	predA := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)
	// Predicate 2: A.ID = B.ID — both sides.
	predBoth := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "A.ID", Typ: values.TypeInt},
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.FieldValue{Field: "B.ID", Typ: values.TypeInt},
		},
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{predA, predBoth},
		nil,
		[]string{"A", "B"},
	)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
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
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	// Build with no aliases — rule should not fire.
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	rv := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	sel := expressions.NewSelectExpression(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
	)
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, selQ)
	ref := expressions.InitialOf(filter)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on no-alias join, want 0", len(yielded))
	}
}

func TestPushFilterBelowJoin_PushToSideB(t *testing.T) {
	t.Parallel()

	// Predicate: B.STATUS = 'active' — references only alias B.
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "B.STATUS", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
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
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{pred},
		nil,
		[]string{"A", "B"},
	)

	progress, converged := FixpointApply([]ExpressionRule{NewPushFilterBelowJoinRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
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

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (constant predicate has no field refs)", len(yielded))
	}
}

func TestPushFilterBelowJoin_LeftOuterJoin_Skips(t *testing.T) {
	t.Parallel()

	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)

	// Build LEFT OUTER join — rule should not fire.
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	rv := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	sel := expressions.NewSelectExpressionWithJoinType(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
		[]string{"A", "B"},
		expressions.JoinLeftOuter,
	)
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, selQ)
	ref := expressions.InitialOf(filter)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on LEFT OUTER join, want 0", len(yielded))
	}
}

func TestPushFilterBelowJoin_BothSidesPushed(t *testing.T) {
	t.Parallel()

	// Two predicates, each referencing a different side.
	predA := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "foo"),
	)
	predB := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "B.STATUS", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)

	ref := buildJoinTree(
		[]predicates.QueryPredicate{predA, predB},
		nil,
		[]string{"A", "B"},
	)

	yielded := FireExpressionRule(NewPushFilterBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	// All filter preds pushed — result is a bare Select.
	newSel, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *SelectExpression", yielded[0])
	}

	qs := newSel.GetQuantifiers()

	// Both sides should have filters.
	innerA := qs[0].GetRangesOver().Get()
	if _, ok := innerA.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalFilterExpression", innerA)
	}

	innerB := qs[1].GetRangesOver().Get()
	if _, ok := innerB.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("quantifier 1 inner = %T, want *LogicalFilterExpression", innerB)
	}
}
