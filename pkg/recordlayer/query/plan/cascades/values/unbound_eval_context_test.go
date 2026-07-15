package values

import (
	"errors"
	"testing"
)

// TestFieldValue_UnboundEvalContext_IsLoud pins the rule that a FieldValue
// whose evaluation resolves to NOTHING — an UNRECOGNIZED non-nil context
// type, or a correlated reference whose correlation is UNBOUND with no
// frontier positional row — is a LOUD *UnboundEvalContextError, never a
// silent NULL, for pinned AND unpinned nodes alike. Production flows only
// OrdinalRow / *RowEvalContext(+Positional) / CorrelationBinder / nil, so
// reaching one of these tails is a planner/executor bug and silence would
// hide it. Two shapes stay quiet instead: a nil context (the appendNullLeg
// NULL), and a correlation that MATCHED a non-ordinal value (a raw leg
// binding, e.g. the executor's birthLegBinder — see
// TestFieldValue_UnpinnedNonOrdinalBinding_IsSilent).
func TestFieldValue_UnboundEvalContext_IsLoud(t *testing.T) {
	t.Parallel()
	type weirdCtx struct{ x int }

	assertUnbound := func(site string, _ any, err error) {
		t.Helper()
		var uce *UnboundEvalContextError
		if !errors.As(err, &uce) {
			t.Fatalf("%s: want a loud *UnboundEvalContextError, got %v", site, err)
		}
	}

	// (1) A flat (unpinned) FieldValue over an unrecognized non-nil context.
	flat := NewFlatFieldValue("A", UnknownType)
	v, err := flat.Evaluate(weirdCtx{x: 1})
	assertUnbound("flat over struct ctx", v, err)

	// (2) A correlated FieldValue whose correlation is UNBOUND — a RowEvalContext
	// with a binder that lacks this correlation and no Positional row (dangling ref).
	corr := NamedCorrelationIdentifier("q")
	lazy := NewFieldValue(NewQuantifiedObjectValue(corr), "COL", UnknownType)
	otherBinder := &ordEvalBinder{
		id:    NamedCorrelationIdentifier("other"),
		bound: &fakeOrdinalRow{names: []string{"COL"}, slots: []any{int64(1)}},
	}
	v, err = lazy.Evaluate(&RowEvalContext{Correlations: otherBinder})
	assertUnbound("correlated over unbound RowEvalContext", v, err)

	// (3) A correlated FieldValue over an unrecognized non-nil context.
	v, err = lazy.Evaluate(weirdCtx{x: 2})
	assertUnbound("correlated over struct ctx", v, err)

	// (4) A BARE CorrelationBinder (not wrapped in a *RowEvalContext) that lacks
	// this correlation — the CorrelationBinder-unbound tail (direct coverage).
	v, err = lazy.Evaluate(otherBinder)
	assertUnbound("correlated over unbound bare CorrelationBinder", v, err)

	// A nil context stays NULL, never loud.
	if got, gotErr := flat.Evaluate(nil); got != nil || gotErr != nil {
		t.Fatalf("flat over nil = (%v, %v), want (nil, nil)", got, gotErr)
	}
}
