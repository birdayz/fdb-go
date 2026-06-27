package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlaceholder_Construction(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("p1")
	val := &values.FieldValue{Field: "age"}
	p := NewPlaceholder(alias, val)

	if got := p.GetParameterAlias(); got != alias {
		t.Fatalf("ParameterAlias = %v, want %v", got, alias)
	}
	if got := p.GetValue(); got != val {
		t.Fatalf("Value = %v, want %v", got, val)
	}
}

func TestPlaceholder_DefaultComparisonRangeIsEmpty(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("p2")
	val := &values.FieldValue{Field: "name"}
	p := NewPlaceholder(alias, val)

	cr := p.GetComparisonRange()
	if cr == nil {
		t.Fatal("ComparisonRange is nil, want non-nil empty range")
	}
	if !cr.IsEmpty() {
		t.Fatalf("ComparisonRange.IsEmpty() = false, want true")
	}
}

func TestPlaceholder_WithRangeCreatesNewPlaceholder(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("p3")
	val := &values.FieldValue{Field: "salary"}
	original := NewPlaceholder(alias, val)

	// Build a non-empty equality range.
	eqComp := &Comparison{
		Type:    ComparisonEquals,
		Operand: &values.ConstantValue{Value: int64(42)},
	}
	eqRange := EmptyComparisonRange()
	result := eqRange.Merge(eqComp)
	if !result.Ok {
		t.Fatal("Merge failed unexpectedly")
	}

	updated := original.WithRange(result.Range)

	// Original must be unchanged.
	if !original.GetComparisonRange().IsEmpty() {
		t.Fatal("original ComparisonRange mutated by WithRange")
	}
	// Updated must carry the new range.
	if updated.GetComparisonRange().IsEmpty() {
		t.Fatal("updated ComparisonRange should not be empty")
	}
	if !updated.GetComparisonRange().IsEquality() {
		t.Fatal("updated ComparisonRange should be equality")
	}
	// Alias and value should be preserved.
	if updated.GetParameterAlias() != alias {
		t.Fatalf("WithRange changed alias: got %v, want %v", updated.GetParameterAlias(), alias)
	}
	if updated.GetValue() != val {
		t.Fatalf("WithRange changed value")
	}
}

func TestPlaceholder_IsConstraining(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("p4")
	val := &values.FieldValue{Field: "col"}
	p := NewPlaceholder(alias, val)

	// Empty range = not constraining.
	if p.IsConstraining() {
		t.Fatal("IsConstraining() = true for empty range, want false")
	}

	// Non-empty range = constraining.
	eqComp := &Comparison{
		Type:    ComparisonEquals,
		Operand: &values.ConstantValue{Value: "hello"},
	}
	eqRange := EmptyComparisonRange()
	result := eqRange.Merge(eqComp)
	if !result.Ok {
		t.Fatal("Merge failed")
	}
	constrained := p.WithRange(result.Range)
	if !constrained.IsConstraining() {
		t.Fatal("IsConstraining() = false for equality range, want true")
	}
}

func TestPlaceholder_Explain(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("param0")
	val := &values.FieldValue{Field: "score"}
	p := NewPlaceholder(alias, val)

	want := "Placeholder(param0, score)"
	if got := p.Explain(); got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}
}

func TestPlaceholder_ExplainNilValue(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("x")
	p := NewPlaceholder(alias, nil)

	want := "Placeholder(x, <nil>)"
	if got := p.Explain(); got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}
}

func TestPlaceholder_SatisfiesQueryPredicateInterface(t *testing.T) {
	t.Parallel()
	var _ QueryPredicate = (*Placeholder)(nil) // compile-time check
}

func TestPlaceholder_IsLeaf(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("leaf")
	val := &values.FieldValue{Field: "id"}
	p := NewPlaceholder(alias, val)

	if got := len(p.Children()); got != 0 {
		t.Fatalf("Children() len = %d, want 0", got)
	}
	if !IsLeafQueryPredicate(p) {
		t.Fatal("IsLeafQueryPredicate(Placeholder) = false, want true")
	}
}

func TestPlaceholder_EvalIsUnknown(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("e")
	p := NewPlaceholder(alias, &values.FieldValue{Field: "x"})
	if got, _ := p.Eval(nil); got != TriUnknown {
		t.Fatalf("Eval = %v, want TriUnknown", got)
	}
}

func TestPlaceholder_Negate(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("n")
	p := NewPlaceholder(alias, &values.FieldValue{Field: "y"})
	neg := p.Negate()
	if neg != p {
		t.Fatal("Negate() should return the same placeholder")
	}
}

func TestPlaceholder_GetCorrelatedTo_IncludesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("corr_alias")
	val := &values.FieldValue{Field: "z"}
	p := NewPlaceholder(alias, val)

	corr := p.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("GetCorrelatedTo() missing the parameter alias")
	}
}

func TestPlaceholder_GetCorrelatedTo_IncludesValueCorrelations(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("p_alias")
	qAlias := values.NamedCorrelationIdentifier("q_source")
	val := values.NewQuantifiedObjectValue(qAlias)
	p := NewPlaceholder(alias, val)

	corr := p.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("GetCorrelatedTo() missing parameter alias")
	}
	if _, ok := corr[qAlias]; !ok {
		t.Fatal("GetCorrelatedTo() missing value's quantifier correlation")
	}
	if len(corr) != 2 {
		t.Fatalf("GetCorrelatedTo() len = %d, want 2", len(corr))
	}
}

func TestPlaceholder_GetCorrelatedToOfPredicate_Integration(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("ph_alias")
	val := &values.FieldValue{Field: "col"}
	p := NewPlaceholder(alias, val)

	// Use the package-level walk function.
	corr := GetCorrelatedToOfPredicate(p)
	if _, ok := corr[alias]; !ok {
		t.Fatal("GetCorrelatedToOfPredicate(Placeholder) missing the parameter alias")
	}
}
