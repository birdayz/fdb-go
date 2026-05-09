package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushOrderingThroughDistinct_SortKeysPassThrough(t *testing.T) {
	t.Parallel()

	// Sort(col1 ASC) → Distinct(Scan)
	// Expect: Distinct(Sort(col1 ASC, Scan))
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctQ := expressions.ForEachQuantifier(expressions.InitialOf(distinct))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		distinctQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughDistinctRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newDistinct, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("expected *LogicalDistinctExpression, got %T", yielded[0])
	}
	innerRef := newDistinct.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner Reference is nil")
	}
	innerSort, ok := innerRef.Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below Distinct, got %T", innerRef.Get())
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

func TestPushOrderingThroughDistinct_DistinctPreserved(t *testing.T) {
	t.Parallel()

	// Verify the yielded expression is still a Distinct (not lost).
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctQ := expressions.ForEachQuantifier(expressions.InitialOf(distinct))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		distinctQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughDistinctRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.LogicalDistinctExpression); !ok {
		t.Fatalf("expected *LogicalDistinctExpression, got %T", yielded[0])
	}
}

func TestPushOrderingThroughDistinct_DescPreserved(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctQ := expressions.ForEachQuantifier(expressions.InitialOf(distinct))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: true},
		},
		distinctQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughDistinctRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	innerSort := yielded[0].(*expressions.LogicalDistinctExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !innerSort.GetSortKeys()[0].Reverse {
		t.Fatal("sort direction should be DESC (preserved from original)")
	}
}

func TestPushOrderingThroughDistinct_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctQ := expressions.ForEachQuantifier(expressions.InitialOf(distinct))
	sort := expressions.UnsortedLogicalSortExpression(distinctQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughDistinctRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughDistinct_NonDistinctDoesNotFire(t *testing.T) {
	t.Parallel()

	// Sort → Scan (no Distinct) — rule should not fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughDistinctRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when inner is not Distinct, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughDistinct_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctQ := expressions.ForEachQuantifier(expressions.InitialOf(distinct))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
			{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, Reverse: true},
		},
		distinctQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughDistinctRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	innerSort := yielded[0].(*expressions.LogicalDistinctExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
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

func TestPushOrderingThroughDistinct_FixpointTerminates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctQ := expressions.ForEachQuantifier(expressions.InitialOf(distinct))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		distinctQ,
	)
	ref := expressions.InitialOf(sort)

	progress, converged := FixpointApply([]ExpressionRule{NewPushOrderingThroughDistinctRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
