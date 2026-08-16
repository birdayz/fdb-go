package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// mustRebase is the checked rebase with the error asserted away, which is what
// these tests always meant. They used to call an error-less wrapper that
// returned nil on failure; every one of them would then have reported its own
// type assertion rather than the rebase failure, so the wrapper bought nothing
// here and cost a fail-open at its production call sites. It is gone; this is
// the replacement.
func mustRebase(t *testing.T, p QueryPredicate, aliases values.AliasMap) QueryPredicate {
	t.Helper()
	rebased, err := RebasePredicateChecked(p, aliases)
	if err != nil {
		t.Fatalf("RebasePredicateChecked: %v", err)
	}
	return rebased
}

func TestRebasePredicate_Comparison(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := &ComparisonPredicate{
		Operand: mustQOV(t, old),
		Comparison: Comparison{
			Type:    ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5)},
		},
	}
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{Source: old, Target: newAlias}))
	cp, ok := result.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", result)
	}
	qov, ok := values.AsQuantifiedObjectValue(cp.Operand)
	if !ok {
		t.Fatalf("expected operand to be QuantifiedObjectValue, got %T", cp.Operand)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
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
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{
		Source: values.NamedCorrelationIdentifier("x"),
		Target: values.NamedCorrelationIdentifier("y"),
	}))
	if result != p {
		t.Fatal("comparison with no matching aliases should return same pointer")
	}
}

func TestRebasePredicate_And(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewAnd(
		NewValuePredicate(mustQOV(t, old)),
		NewConstantPredicate(TriTrue),
	)
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{Source: old, Target: newAlias}))
	and, ok := result.(*AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", result)
	}
	vp, ok := and.SubPredicates[0].(*ValuePredicate)
	if !ok {
		t.Fatalf("expected sub[0] to be *ValuePredicate, got %T", and.SubPredicates[0])
	}
	qov, ok := values.AsQuantifiedObjectValue(vp.Value)
	if !ok {
		t.Fatalf("expected value to be QuantifiedObjectValue, got %T", vp.Value)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebasePredicate_Not(t *testing.T) {
	t.Parallel()
	old := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewNot(NewValuePredicate(mustQOV(t, old)))
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{Source: old, Target: newAlias}))
	not, ok := result.(*NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", result)
	}
	vp, ok2 := not.Child.(*ValuePredicate)
	if !ok2 {
		t.Fatalf("expected child to be *ValuePredicate, got %T", not.Child)
	}
	qov, ok3 := values.AsQuantifiedObjectValue(vp.Value)
	if !ok3 {
		t.Fatalf("expected value to be QuantifiedObjectValue, got %T", vp.Value)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebasePredicate_Constant(t *testing.T) {
	t.Parallel()
	p := NewConstantPredicate(TriTrue)
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{
		Source: values.NamedCorrelationIdentifier("x"),
		Target: values.NamedCorrelationIdentifier("y"),
	}))
	if result != p {
		t.Fatal("constant predicate should return same pointer")
	}
}

func TestRebasePredicate_Or(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := NewOr(
		NewValuePredicate(mustQOV(t, oldAlias)),
		NewConstantPredicate(TriFalse),
	)
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{Source: oldAlias, Target: newAlias}))
	or, ok := result.(*OrPredicate)
	if !ok {
		t.Fatalf("expected *OrPredicate, got %T", result)
	}
	vp, ok := or.SubPredicates[0].(*ValuePredicate)
	if !ok {
		t.Fatalf("expected sub[0] to be *ValuePredicate, got %T", or.SubPredicates[0])
	}
	qov, ok := values.AsQuantifiedObjectValue(vp.Value)
	if !ok {
		t.Fatalf("expected value to be QuantifiedObjectValue, got %T", vp.Value)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebasePredicate_Exists(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("old")
	newAlias := values.NamedCorrelationIdentifier("new")
	p := mustExistentialAlias(t, oldAlias)
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{Source: oldAlias, Target: newAlias}))
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
	p := mustExistentialAlias(t, values.NamedCorrelationIdentifier("other"))
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{
		Source: values.NamedCorrelationIdentifier("x"),
		Target: values.NamedCorrelationIdentifier("y"),
	}))
	if result != p {
		t.Fatal("exists with no matching alias should return same pointer")
	}
}

func TestRebasePredicate_Nil(t *testing.T) {
	t.Parallel()
	result := mustRebase(t, nil, nil)
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
		Value:          mustQOV(t, oldValAlias),
		CompRange:      EmptyComparisonRange(),
	}
	result := mustRebase(t, p, mustAliasMap(t,
		values.AliasPair{Source: oldAlias, Target: newAlias},
		values.AliasPair{Source: oldValAlias, Target: newValAlias},
	))
	ph, ok := result.(*Placeholder)
	if !ok {
		t.Fatalf("expected *Placeholder, got %T", result)
	}
	if ph.ParameterAlias != newAlias {
		t.Fatalf("expected ParameterAlias %v, got %v", newAlias, ph.ParameterAlias)
	}
	qov, ok := values.AsQuantifiedObjectValue(ph.Value)
	if !ok {
		t.Fatalf("expected QOV value, got %T", ph.Value)
	}
	if qov.Correlation() != newValAlias {
		t.Fatalf("expected value correlation %v, got %v", newValAlias, qov.Correlation())
	}
}

func TestRebasePredicate_PlaceholderNoChange(t *testing.T) {
	t.Parallel()
	p := &Placeholder{
		ParameterAlias: values.NamedCorrelationIdentifier("param"),
		Value:          predicateTestField(t, "X", values.NullableLong),
		CompRange:      EmptyComparisonRange(),
	}
	result := mustRebase(t, p, mustAliasMap(t, values.AliasPair{
		Source: values.NamedCorrelationIdentifier("other"),
		Target: values.NamedCorrelationIdentifier("new"),
	}))
	if result != p {
		t.Fatal("placeholder with no matching aliases should return same pointer")
	}
}
