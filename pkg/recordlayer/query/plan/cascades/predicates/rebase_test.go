package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestRebasePredicate_Comparison(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := &ComparisonPredicate{
		Operand: &values.QuantifiedObjectValue{Correlation: old},
		Comparison: Comparison{
			Type:    ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5)},
		},
	}
	result := RebasePredicate(p, values.AliasMap{old: newAlias})
	cp, ok := result.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", result)
	}
	qov, ok := cp.Operand.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected operand to be *QuantifiedObjectValue, got %T", cp.Operand)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebasePredicate_ComparisonNoChange(t *testing.T) {
	t.Parallel()
	p := &ComparisonPredicate{
		Operand: &values.ConstantValue{Value: int64(1)},
		Comparison: Comparison{
			Type:    ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(2)},
		},
	}
	result := RebasePredicate(p, values.AliasMap{
		values.NamedCorrelationIdentifier("x"): values.NamedCorrelationIdentifier("y"),
	})
	if result != p {
		t.Fatal("comparison with no matching aliases should return same pointer")
	}
}

func TestRebasePredicate_And(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewAnd(
		NewValuePredicate(&values.QuantifiedObjectValue{Correlation: old}),
		NewConstantPredicate(TriTrue),
	)
	result := RebasePredicate(p, values.AliasMap{old: newAlias})
	and, ok := result.(*AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", result)
	}
	vp, ok := and.SubPredicates[0].(*ValuePredicate)
	if !ok {
		t.Fatalf("expected sub[0] to be *ValuePredicate, got %T", and.SubPredicates[0])
	}
	qov, ok := vp.Value.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected value to be *QuantifiedObjectValue, got %T", vp.Value)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebasePredicate_Not(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewNot(NewValuePredicate(&values.QuantifiedObjectValue{Correlation: old}))
	result := RebasePredicate(p, values.AliasMap{old: newAlias})
	not, ok := result.(*NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", result)
	}
	vp, ok2 := not.Child.(*ValuePredicate)
	if !ok2 {
		t.Fatalf("expected child to be *ValuePredicate, got %T", not.Child)
	}
	qov, ok3 := vp.Value.(*values.QuantifiedObjectValue)
	if !ok3 {
		t.Fatalf("expected value to be *QuantifiedObjectValue, got %T", vp.Value)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebasePredicate_Constant(t *testing.T) {
	t.Parallel()
	p := NewConstantPredicate(TriTrue)
	result := RebasePredicate(p, values.AliasMap{
		values.NamedCorrelationIdentifier("x"): values.NamedCorrelationIdentifier("y"),
	})
	if result != p {
		t.Fatal("constant predicate should return same pointer")
	}
}

func TestRebasePredicate_Or(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewOr(
		NewValuePredicate(&values.QuantifiedObjectValue{Correlation: oldAlias}),
		NewConstantPredicate(TriFalse),
	)
	result := RebasePredicate(p, values.AliasMap{oldAlias: newAlias})
	or, ok := result.(*OrPredicate)
	if !ok {
		t.Fatalf("expected *OrPredicate, got %T", result)
	}
	vp, ok := or.SubPredicates[0].(*ValuePredicate)
	if !ok {
		t.Fatalf("expected sub[0] to be *ValuePredicate, got %T", or.SubPredicates[0])
	}
	qov, ok := vp.Value.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected value to be *QuantifiedObjectValue, got %T", vp.Value)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebasePredicate_Exists(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewExistentialAlias(oldAlias)
	result := RebasePredicate(p, values.AliasMap{oldAlias: newAlias})
	ep, ok := result.(*ExistentialValuePredicate)
	if !ok {
		t.Fatalf("expected *ExistentialValuePredicate, got %T", result)
	}
	if ep.GetExistentialAlias() != newAlias {
		t.Fatalf("expected rebased alias %v, got %v", newAlias, ep.GetExistentialAlias())
	}
}

func TestRebasePredicate_ExistsNoChange(t *testing.T) {
	t.Parallel()
	p := NewExistentialAlias(values.NamedCorrelationIdentifier("other"))
	result := RebasePredicate(p, values.AliasMap{
		values.NamedCorrelationIdentifier("x"): values.NamedCorrelationIdentifier("y"),
	})
	if result != p {
		t.Fatal("exists with no matching alias should return same pointer")
	}
}

func TestRebasePredicate_Nil(t *testing.T) {
	t.Parallel()
	result := RebasePredicate(nil, values.AliasMap{})
	if result != nil {
		t.Fatal("nil predicate should return nil")
	}
}

func TestRebasePredicate_Placeholder(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("param_old")
	newAlias := values.NamedCorrelationIdentifier("param_new")
	oldValAlias := values.NamedCorrelationIdentifier("q_old")
	newValAlias := values.NamedCorrelationIdentifier("q_new")
	p := &Placeholder{
		ParameterAlias: oldAlias,
		Value:          &values.QuantifiedObjectValue{Correlation: oldValAlias},
		CompRange:      EmptyComparisonRange(),
	}
	result := RebasePredicate(p, values.AliasMap{oldAlias: newAlias, oldValAlias: newValAlias})
	ph, ok := result.(*Placeholder)
	if !ok {
		t.Fatalf("expected *Placeholder, got %T", result)
	}
	if ph.ParameterAlias != newAlias {
		t.Fatalf("expected ParameterAlias %v, got %v", newAlias, ph.ParameterAlias)
	}
	qov, ok := ph.Value.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QOV value, got %T", ph.Value)
	}
	if qov.Correlation != newValAlias {
		t.Fatalf("expected value correlation %v, got %v", newValAlias, qov.Correlation)
	}
}

func TestRebasePredicate_PlaceholderNoChange(t *testing.T) {
	t.Parallel()
	p := &Placeholder{
		ParameterAlias: values.NamedCorrelationIdentifier("param"),
		Value:          &values.FieldValue{Field: "X"},
		CompRange:      EmptyComparisonRange(),
	}
	result := RebasePredicate(p, values.AliasMap{
		values.NamedCorrelationIdentifier("other"): values.NamedCorrelationIdentifier("new"),
	})
	if result != p {
		t.Fatal("placeholder with no matching aliases should return same pointer")
	}
}
