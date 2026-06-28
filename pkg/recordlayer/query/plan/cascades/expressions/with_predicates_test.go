package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRelationalExpressionWithPredicates_TypeAssertion pins the
// generic-rule contract: a caller can type-assert a
// RelationalExpression to RelationalExpressionWithPredicates and get
// the predicate list without knowing the concrete operator class.
func TestRelationalExpressionWithPredicates_TypeAssertion(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := ForEachQuantifier(InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)

	// LogicalFilterExpression — implements WithPredicates.
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	if got := getPredicatesGeneric(f); len(got) != 1 {
		t.Fatalf("LogicalFilter predicate count via WithPredicates = %d, want 1", len(got))
	}

	// SelectExpression — implements WithPredicates.
	s := NewSelectExpression(values.NewBooleanValue(true), []Quantifier{scanQ}, []predicates.QueryPredicate{pT})
	if got := getPredicatesGeneric(s); len(got) != 1 {
		t.Fatalf("Select predicate count via WithPredicates = %d, want 1", len(got))
	}

	// FullUnorderedScanExpression — does NOT implement WithPredicates.
	if got := getPredicatesGeneric(scan); got != nil {
		t.Fatalf("Scan should not implement WithPredicates, got %v", got)
	}

	// LogicalDistinct — does NOT implement WithPredicates.
	d := NewLogicalDistinctExpression(scanQ)
	if got := getPredicatesGeneric(d); got != nil {
		t.Fatalf("Distinct should not implement WithPredicates, got %v", got)
	}
}

// getPredicatesGeneric returns e's predicates if e implements
// RelationalExpressionWithPredicates, nil otherwise.
func getPredicatesGeneric(e RelationalExpression) []predicates.QueryPredicate {
	if wp, ok := e.(RelationalExpressionWithPredicates); ok {
		return wp.GetPredicates()
	}
	return nil
}

func TestCountPredicates(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := ForEachQuantifier(InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)

	// 0 for non-predicate-bearing.
	if got := CountPredicates(scan); got != 0 {
		t.Errorf("Scan CountPredicates=%d, want 0", got)
	}

	// Match predicate count for filter.
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pT, pT, pT}, scanQ)
	if got := CountPredicates(f); got != 3 {
		t.Errorf("Filter(3 predicates) CountPredicates=%d, want 3", got)
	}

	// Empty predicate list.
	f0 := NewLogicalFilterExpression(nil, scanQ)
	if got := CountPredicates(f0); got != 0 {
		t.Errorf("Filter([]) CountPredicates=%d, want 0", got)
	}
}

func TestHasPredicates(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := ForEachQuantifier(InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)

	if HasPredicates(scan) {
		t.Error("Scan HasPredicates=true, want false")
	}
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	if !HasPredicates(f) {
		t.Error("Filter([T]) HasPredicates=false, want true")
	}
	f0 := NewLogicalFilterExpression(nil, scanQ)
	if HasPredicates(f0) {
		t.Error("Filter([]) HasPredicates=true, want false (empty list)")
	}
}
