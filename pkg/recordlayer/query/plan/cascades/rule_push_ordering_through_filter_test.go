package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushOrderingThroughFilter_SortKeysPassThrough(t *testing.T) {
	t.Parallel()

	// Sort(col1 ASC) → Filter(TRUE, Scan)
	// Expect: Filter(TRUE, Sort(col1 ASC, Scan))
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		filterQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughFilterRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newFilter, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected *LogicalFilterExpression, got %T", yielded[0])
	}
	innerRef := newFilter.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner Reference is nil")
	}
	innerSort, ok := innerRef.Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below Filter, got %T", innerRef.Get())
	}
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(sortKeys))
	}
	fv, ok := sortKeys[0].Value.(*values.FieldValue)
	if !ok || fv.Field != "col1" {
		t.Fatalf("expected sort key col1, got %v", sortKeys[0].Value)
	}
	if sortKeys[0].Reverse {
		t.Fatal("expected ASC sort key")
	}
}

func TestPushOrderingThroughFilter_PredicatesPreserved(t *testing.T) {
	t.Parallel()

	// Sort(a ASC) → Filter(x > 5 AND y = 3, Scan)
	// Expect: Filter(x > 5 AND y = 3, Sort(a ASC, Scan)) — both predicates preserved.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	p1 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5)),
	)
	p2 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "y", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(3)),
	)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p1, p2}, scanQ)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		filterQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughFilterRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newFilter := yielded[0].(*expressions.LogicalFilterExpression)
	preds := newFilter.GetPredicates()
	if len(preds) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(preds))
	}
}

func TestPushOrderingThroughFilter_DescPreserved(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: true},
		},
		filterQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughFilterRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	innerSort := yielded[0].(*expressions.LogicalFilterExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !innerSort.GetSortKeys()[0].Reverse {
		t.Fatal("sort direction should be DESC (preserved from original)")
	}
}

func TestPushOrderingThroughFilter_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.UnsortedLogicalSortExpression(filterQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughFilterRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughFilter_NonFilterDoesNotFire(t *testing.T) {
	t.Parallel()

	// Sort → Scan (no Filter) — rule should not fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughFilterRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when inner is not Filter, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughFilter_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
			{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, Reverse: true},
		},
		filterQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughFilterRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	innerSort := yielded[0].(*expressions.LogicalFilterExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 2 {
		t.Fatalf("expected 2 sort keys, got %d", len(sortKeys))
	}
	if fv := sortKeys[0].Value.(*values.FieldValue); fv.Field != "a" || sortKeys[0].Reverse {
		t.Fatalf("first key: want a ASC, got %s reverse=%v", fv.Field, sortKeys[0].Reverse)
	}
	if fv := sortKeys[1].Value.(*values.FieldValue); fv.Field != "b" || !sortKeys[1].Reverse {
		t.Fatalf("second key: want b DESC, got %s reverse=%v", fv.Field, sortKeys[1].Reverse)
	}
}

func TestPushOrderingThroughFilter_FixpointTerminates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		filterQ,
	)
	ref := expressions.InitialOf(sort)

	progress, converged := FixpointApply([]ExpressionRule{NewPushOrderingThroughFilterRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
