package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushOrderingThroughProjection_SortKeyMatchesField(t *testing.T) {
	t.Parallel()

	// Projection: [A AS col1, B AS col2]
	// Sort: [col1 ASC]
	// Expected: Projection([A AS col1, B AS col2], Sort([A ASC], Scan))
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
			&values.FieldValue{Field: "B", Typ: values.UnknownType},
		},
		[]string{"col1", "col2"},
		scanQ,
	)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType}, Reverse: false},
		},
		projQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughProjectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newProj, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected *LogicalProjectionExpression, got %T", yielded[0])
	}
	innerRef := newProj.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner Reference is nil")
	}
	innerSort, ok := innerRef.Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below Projection, got %T", innerRef.Get())
	}
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(sortKeys))
	}
	fv, ok := sortKeys[0].Value.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue sort key, got %T", sortKeys[0].Value)
	}
	if fv.Field != "A" {
		t.Fatalf("expected sort key field 'A' (pre-projection), got %q", fv.Field)
	}
	if sortKeys[0].Reverse {
		t.Fatal("expected ASC sort key")
	}
}

func TestPushOrderingThroughProjection_AliasResolution(t *testing.T) {
	t.Parallel()

	// Projection: [A+B AS total, C AS c]
	// Sort: [TOTAL ASC]
	// Expected: Projection([A+B AS total, C AS c], Sort([(A + B) ASC], Scan))
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	addExpr := &values.ArithmeticValue{
		Left:  &values.FieldValue{Field: "A", Typ: values.UnknownType},
		Right: &values.FieldValue{Field: "B", Typ: values.UnknownType},
		Op:    values.OpAdd,
	}
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{addExpr, &values.FieldValue{Field: "C", Typ: values.UnknownType}},
		[]string{"total", "c"},
		scanQ,
	)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "TOTAL", Typ: values.UnknownType}, Reverse: false},
		},
		projQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughProjectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newProj := yielded[0].(*expressions.LogicalProjectionExpression)
	innerSort := newProj.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(sortKeys))
	}
	// The pushed-down sort key should be the arithmetic expression A+B.
	explain := values.ExplainValue(sortKeys[0].Value)
	expectedExplain := values.ExplainValue(addExpr)
	if explain != expectedExplain {
		t.Fatalf("expected sort key %q, got %q", expectedExplain, explain)
	}
}

func TestPushOrderingThroughProjection_NoMatchDoesNotFire(t *testing.T) {
	t.Parallel()

	// Projection: [A AS col1]
	// Sort: [NONEXISTENT ASC]
	// Rule should NOT fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"col1"},
		scanQ,
	)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "NONEXISTENT", Typ: values.UnknownType}, Reverse: false},
		},
		projQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughProjectionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when sort key doesn't match, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughProjection_DescPreserved(t *testing.T) {
	t.Parallel()

	// Projection: [A AS a]
	// Sort: [A DESC]
	// Expected: sort pushed with DESC preserved.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"a"},
		scanQ,
	)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: true},
		},
		projQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughProjectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newProj := yielded[0].(*expressions.LogicalProjectionExpression)
	innerSort := newProj.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !innerSort.GetSortKeys()[0].Reverse {
		t.Fatal("sort direction should be DESC (preserved from original)")
	}
}

func TestPushOrderingThroughProjection_MultipleSortKeysAllMatch(t *testing.T) {
	t.Parallel()

	// Projection: [X AS a, Y AS b, Z AS c]
	// Sort: [A ASC, B DESC]
	// Expected: Sort([X ASC, Y DESC], Scan) below projection.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{
			&values.FieldValue{Field: "X", Typ: values.UnknownType},
			&values.FieldValue{Field: "Y", Typ: values.UnknownType},
			&values.FieldValue{Field: "Z", Typ: values.UnknownType},
		},
		[]string{"a", "b", "c"},
		scanQ,
	)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: false},
			{Value: &values.FieldValue{Field: "B", Typ: values.UnknownType}, Reverse: true},
		},
		projQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughProjectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newProj := yielded[0].(*expressions.LogicalProjectionExpression)
	innerSort := newProj.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 2 {
		t.Fatalf("expected 2 sort keys, got %d", len(sortKeys))
	}
	// X ASC
	if fv := sortKeys[0].Value.(*values.FieldValue); fv.Field != "X" || sortKeys[0].Reverse {
		t.Fatalf("first key: want X ASC, got %s reverse=%v", fv.Field, sortKeys[0].Reverse)
	}
	// Y DESC
	if fv := sortKeys[1].Value.(*values.FieldValue); fv.Field != "Y" || !sortKeys[1].Reverse {
		t.Fatalf("second key: want Y DESC, got %s reverse=%v", fv.Field, sortKeys[1].Reverse)
	}
}

func TestPushOrderingThroughProjection_FixpointTerminates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"a"},
		scanQ,
	)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: false},
		},
		projQ,
	)
	ref := expressions.InitialOf(sort)

	progress, converged := FixpointApply([]ExpressionRule{NewPushOrderingThroughProjectionRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
