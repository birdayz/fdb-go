package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestRebasePredicate_Comparison(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	new := values.NamedCorrelationIdentifier("new")
	p := &ComparisonPredicate{
		Operand: &values.QuantifiedObjectValue{Correlation: old},
		Comparison: Comparison{
			Type:    ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5)},
		},
	}
	result := RebasePredicate(p, values.AliasMap{old: new})
	cp, ok := result.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", result)
	}
	qov, ok := cp.Operand.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected operand to be *QuantifiedObjectValue, got %T", cp.Operand)
	}
	if qov.Correlation != new {
		t.Fatalf("expected rebased correlation %v, got %v", new, qov.Correlation)
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
	new := values.NamedCorrelationIdentifier("new")
	p := NewAnd(
		NewValuePredicate(&values.QuantifiedObjectValue{Correlation: old}),
		NewConstantPredicate(TriTrue),
	)
	result := RebasePredicate(p, values.AliasMap{old: new})
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
	if qov.Correlation != new {
		t.Fatalf("expected rebased correlation %v, got %v", new, qov.Correlation)
	}
}

func TestRebasePredicate_Not(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	new := values.NamedCorrelationIdentifier("new")
	p := NewNot(NewValuePredicate(&values.QuantifiedObjectValue{Correlation: old}))
	result := RebasePredicate(p, values.AliasMap{old: new})
	not, ok := result.(*NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", result)
	}
	vp := not.Child.(*ValuePredicate)
	qov := vp.Value.(*values.QuantifiedObjectValue)
	if qov.Correlation != new {
		t.Fatalf("expected rebased correlation %v, got %v", new, qov.Correlation)
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

func TestRebasePredicate_Nil(t *testing.T) {
	t.Parallel()
	result := RebasePredicate(nil, values.AliasMap{})
	if result != nil {
		t.Fatal("nil predicate should return nil")
	}
}
