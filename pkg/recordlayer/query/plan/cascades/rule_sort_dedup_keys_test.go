package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSortDedupKeysRule_RemovesDuplicateKey(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, Reverse: false},
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false}, // dup of [0]
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortDedupKeysRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newSort, ok := yielded[0].(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalSortExpression", yielded[0])
	}
	got := newSort.GetSortKeys()
	if len(got) != 2 {
		t.Fatalf("deduped keys len=%d, want 2", len(got))
	}
	// Order preserved: a, b.
	if fv, ok := got[0].Value.(*values.FieldValue); !ok || fv.Field != "a" {
		t.Errorf("got[0] = %v, want FieldValue(a)", got[0].Value)
	}
	if fv, ok := got[1].Value.(*values.FieldValue); !ok || fv.Field != "b" {
		t.Errorf("got[1] = %v, want FieldValue(b)", got[1].Value)
	}
}

func TestSortDedupKeysRule_AllUnique_NoFire(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}},
		{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortDedupKeysRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on all-unique keys, want 0", len(yielded))
	}
}

func TestSortDedupKeysRule_SameValueDifferentDirection_NotDuplicate(t *testing.T) {
	t.Parallel()
	// Sort([a ASC, a DESC]) — same Value, different Reverse. NOT
	// duplicates; both keys remain.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: true},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortDedupKeysRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on different-direction same-Value keys, want 0", len(yielded))
	}
}

func TestSortDedupKeysRule_CooperatesWithConstantKeysElim(t *testing.T) {
	t.Parallel()
	// Sort([1, 1, 2, 2], X) — duplicate constant keys.
	// SortDedupKeys collapses to Sort([1, 2], X).
	// SortConstantKeysElim then eliminates → X.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.ConstantValue{Value: int64(1), Typ: values.TypeUnknown}},
		{Value: &values.ConstantValue{Value: int64(1), Typ: values.TypeUnknown}},
		{Value: &values.ConstantValue{Value: int64(2), Typ: values.TypeUnknown}},
		{Value: &values.ConstantValue{Value: int64(2), Typ: values.TypeUnknown}},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	rules := []ExpressionRule{
		NewSortDedupKeysRule(),
		NewSortConstantKeysElimRule(),
	}
	progress, converged := exploreRewriting(NewPlanner(rules, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d", progress)
	}
	// Look for a bare Scan in the Reference.
	foundBareScan := false
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			foundBareScan = true
			break
		}
	}
	if !foundBareScan {
		t.Fatalf("two-rule cooperation didn't reach bare Scan — Reference has %d members", len(ref.Members()))
	}
}

func TestSortDedupKeysRule_DedupsConstantKeys(t *testing.T) {
	t.Parallel()
	// Sort([42, 42, 'x']) — duplicate ConstantValue(42) keys.
	// SortDedupKeys should catch them via Explain-text equality.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.ConstantValue{Value: int64(42), Typ: values.TypeUnknown}},
		{Value: &values.ConstantValue{Value: int64(42), Typ: values.TypeUnknown}},
		{Value: &values.ConstantValue{Value: "x", Typ: values.TypeUnknown}},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortDedupKeysRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	flat := yielded[0].(*expressions.LogicalSortExpression)
	if len(flat.GetSortKeys()) != 2 {
		t.Fatalf("deduped keys len=%d, want 2", len(flat.GetSortKeys()))
	}
}

func TestSortDedupKeysRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewSortDedupKeysRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
