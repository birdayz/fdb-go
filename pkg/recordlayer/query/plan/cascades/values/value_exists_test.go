package values

import "testing"

func TestExistsValue_CompositeShape(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("subq")
	v := NewExistsValue(alias)
	// RFC-141: ExistsValue is now a transparent composite over a
	// QuantifiedObjectValue child carrying the correlation.
	ch := v.Children()
	if len(ch) != 1 {
		t.Fatalf("ExistsValue should have 1 child (the QOV), got %d", len(ch))
	}
	qov, ok := ch[0].(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("child should be *QuantifiedObjectValue, got %T", ch[0])
	}
	if qov.Correlation != alias {
		t.Fatalf("child QOV correlation = %v, want %v", qov.Correlation, alias)
	}
	if v.GetChild() != v.Value {
		t.Fatal("GetChild should return the Value field")
	}
}

func TestExistsValue_TypeIsNotNullBoolean(t *testing.T) {
	t.Parallel()
	v := NewExistsValue(NamedCorrelationIdentifier("x"))
	if !v.Type().Equals(NotNullBoolean) {
		t.Fatalf("Type=%v, want NotNullBoolean", v.Type())
	}
}

func TestExistsValue_EvaluateChildNonNull(t *testing.T) {
	t.Parallel()
	// EXISTS is true iff the child quantifier's object is non-null
	// (Java: getChild().eval() != null). Drive the child QOV through a
	// CorrelationBinder: a bound existential object ⇒ TRUE, an unbound
	// one (the subplan yielded no row) ⇒ child evals to nil ⇒ FALSE.
	alias := NamedCorrelationIdentifier("subq")
	v := NewExistsValue(alias)

	bound := staticBinder{alias: {"col": 1}}
	got, err := v.Evaluate(bound)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if got != true {
		t.Fatalf("Evaluate (bound row) = %v, want true", got)
	}

	unbound := staticBinder{}
	got, err = v.Evaluate(unbound)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if got != false {
		t.Fatalf("Evaluate (no row) = %v, want false", got)
	}

	// Revert-proof: a RowEvalContext carrying an outer row (Datum) but NO binding
	// for the existential alias must be FALSE — never the outer-row fallback. The child QOV's
	// `ctx.Datum` shim would otherwise return the outer row, making EXISTS wrongly report TRUE
	// for an empty subquery. ExistsValue.Evaluate looks up the existential binding directly.
	outerRowNoBinding := &RowEvalContext{Datum: map[string]any{"outer_col": 42}}
	got, err = v.Evaluate(outerRowNoBinding)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if got != false {
		t.Fatalf("Evaluate (outer row, unbound existential) = %v, want false (no outer-row fallback)", got)
	}

	// And a RowEvalContext that DOES bind the existential alias to a row ⇒ TRUE.
	boundRow := &RowEvalContext{
		Datum:        map[string]any{"outer_col": 42},
		Correlations: &testCorrelationBinder{bindings: map[CorrelationIdentifier]any{alias: map[string]any{"col": 1}}},
	}
	got, err = v.Evaluate(boundRow)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if got != true {
		t.Fatalf("Evaluate (outer row + bound existential) = %v, want true", got)
	}

	// Revert-proof: a binder that binds the existential alias to a TYPED nil
	// (a nil map[string]any boxed into `any`) must be FALSE. A bare `bound != nil` reports TRUE
	// for a typed nil; isNilBinding treats it as absent.
	var nilRow map[string]any // typed nil
	typedNil := &testCorrelationBinder{bindings: map[CorrelationIdentifier]any{alias: nilRow}}
	got, err = v.Evaluate(typedNil)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if got != false {
		t.Fatalf("Evaluate (typed-nil binding) = %v, want false (not a phantom row)", got)
	}
}

func TestExistsValue_CorrelatedToAlias(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("subq")
	v := NewExistsValue(alias)
	cs := v.GetCorrelatedTo()
	if len(cs) != 1 {
		t.Fatalf("CorrelatedTo size = %d, want 1", len(cs))
	}
	if _, ok := cs[alias]; !ok {
		t.Fatalf("CorrelatedTo missing alias %v", alias)
	}
}

func TestExistsValue_WithNewChild(t *testing.T) {
	t.Parallel()
	v := NewExistsValue(NamedCorrelationIdentifier("a"))
	newChild := NewQuantifiedObjectValue(NamedCorrelationIdentifier("b"))
	v2 := v.WithNewChild(newChild)
	if v2.GetChild() != newChild {
		t.Fatal("WithNewChild should swap the child")
	}
	if v.GetChild() == newChild {
		t.Fatal("WithNewChild should not mutate the original")
	}
}
