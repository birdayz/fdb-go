package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushOrderingThroughGroupBy_AllSortKeysMatchGroupKeys(t *testing.T) {
	t.Parallel()

	// Build: Sort(col1 ASC) → GroupBy(groupKeys=[col1], aggs=[SUM(col2)]) → Scan
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "col1", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "col2", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	// Expect: GroupBy(groupKeys=[col1], aggs=[SUM(col2)]) → Sort(col1 ASC) → Scan
	newGB, ok := yielded[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected *GroupByExpression, got %T", yielded[0])
	}
	innerRef := newGB.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner Reference is nil")
	}
	innerSort, ok := innerRef.Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below GroupBy, got %T", innerRef.Get())
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

func TestPushOrderingThroughGroupBy_SortPrefixOfGroupKeys(t *testing.T) {
	t.Parallel()

	// Sort(a ASC) → GroupBy(keys=[a, b]) → Scan
	// Expect: GroupBy → Sort(a ASC, b ASC) → Scan
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newGB := yielded[0].(*expressions.GroupByExpression)
	innerSort := newGB.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 2 {
		t.Fatalf("expected 2 sort keys (a + remaining b), got %d", len(sortKeys))
	}
	// First key: a ASC (from sort)
	if fv := sortKeys[0].Value.(*values.FieldValue); fv.Field != "a" {
		t.Fatalf("first sort key should be 'a', got %q", fv.Field)
	}
	if sortKeys[0].Reverse {
		t.Fatal("first sort key should be ASC")
	}
	// Second key: b ASC (remaining grouping key, default direction)
	if fv := sortKeys[1].Value.(*values.FieldValue); fv.Field != "b" {
		t.Fatalf("second sort key should be 'b', got %q", fv.Field)
	}
	if sortKeys[1].Reverse {
		t.Fatal("second sort key should be ASC (default)")
	}
}

func TestPushOrderingThroughGroupBy_SortDescPreserved(t *testing.T) {
	t.Parallel()

	// Sort(a DESC) → GroupBy(keys=[a]) → Scan
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: true},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	innerSort := yielded[0].(*expressions.GroupByExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !innerSort.GetSortKeys()[0].Reverse {
		t.Fatal("sort direction should be DESC (preserved from original)")
	}
}

func TestPushOrderingThroughGroupBy_SortKeyNotInGroupKeys(t *testing.T) {
	t.Parallel()

	// Sort(x ASC) → GroupBy(keys=[a]) → Scan
	// x is NOT a grouping key — rule should not fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, Reverse: false},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when sort key is not a grouping key, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughGroupBy_NonGroupByInner(t *testing.T) {
	t.Parallel()

	// Sort → Scan (no GroupBy) — rule should not fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when inner is not GroupBy, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughGroupBy_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	// Unsorted sort → GroupBy → Scan — rule should not fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.UnsortedLogicalSortExpression(gbQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughGroupBy_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	// Sort(COL1 ASC) → GroupBy(keys=[col1]) — case mismatch should still match.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "col1", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "col2", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType}, Reverse: false},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1 (case-insensitive match should work)", len(yielded))
	}
}

func TestPushOrderingThroughGroupBy_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	// Sort(a ASC, b DESC) → GroupBy(keys=[b, a, c]) → Scan
	// Expect: GroupBy → Sort(a ASC, b DESC, c ASC) → Scan
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "c", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
			{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, Reverse: true},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newGB := yielded[0].(*expressions.GroupByExpression)
	innerSort := newGB.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 3 {
		t.Fatalf("expected 3 sort keys, got %d", len(sortKeys))
	}
	// a ASC
	if fv := sortKeys[0].Value.(*values.FieldValue); fv.Field != "a" || sortKeys[0].Reverse {
		t.Fatalf("first key: want a ASC, got %s reverse=%v", fv.Field, sortKeys[0].Reverse)
	}
	// b DESC
	if fv := sortKeys[1].Value.(*values.FieldValue); fv.Field != "b" || !sortKeys[1].Reverse {
		t.Fatalf("second key: want b DESC, got %s reverse=%v", fv.Field, sortKeys[1].Reverse)
	}
	// c ASC (remaining)
	if fv := sortKeys[2].Value.(*values.FieldValue); fv.Field != "c" || sortKeys[2].Reverse {
		t.Fatalf("third key: want c ASC, got %s reverse=%v", fv.Field, sortKeys[2].Reverse)
	}
}

func TestPushOrderingThroughGroupBy_EmptyGroupKeys(t *testing.T) {
	t.Parallel()

	// Sort → GroupBy(keys=[]) → Scan — no grouping keys, rule should not fire.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, Reverse: false},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughGroupByRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire with empty grouping keys, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughGroupBy_FixpointTerminates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbQ := expressions.ForEachQuantifier(expressions.InitialOf(gb))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		gbQ,
	)
	ref := expressions.InitialOf(sort)

	progress, converged := FixpointApply([]ExpressionRule{NewPushOrderingThroughGroupByRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
