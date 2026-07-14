package values

import (
	"errors"
	"testing"
)

// TestFieldValue_UnboundEvalContext_IsLoud pins the RFC-173 §F ruling: a
// FieldValue whose evaluation resolves to NOTHING — an UNRECOGNIZED non-nil
// context type, or a correlated reference whose correlation is UNBOUND with no
// frontier positional row — is a LOUD *UnboundEvalContextError, never a silent
// NULL, for pinned AND unpinned nodes alike. Post-cap production flows only
// OrdinalRow / *RowEvalContext(+Positional) / CorrelationBinder / nil, so
// reaching one of these tails is a planner/executor bug and silence would hide
// it. The two SANCTIONED shapes never reach here and stay quiet: a nil context
// (the appendNullLeg / ruling-#3 NULL), and a correlation that MATCHED a
// non-ordinal value (the birthLegBinder raw leg — see
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

	// A nil context stays the sanctioned NULL (ruling #3), never loud.
	if got, gotErr := flat.Evaluate(nil); got != nil || gotErr != nil {
		t.Fatalf("flat over nil = (%v, %v), want (nil, nil) — sanctioned NULL", got, gotErr)
	}
}
