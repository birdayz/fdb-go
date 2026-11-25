package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestFrontierContextIsNotAUniformCarrier pins the reason a per-plan type
// repository is BAKED onto the plan's values (RFC-204 §4.5) instead of being
// carried on the evaluation context the way Java carries it.
//
// Java can carry it on the context because Java has exactly ONE
// EvaluationContext type, so RecordConstructorValue.eval can say
// `context.getTypeRepository()` and always be answered
// (RecordConstructorValue.java:113-114). Go has no such uniform context: the
// frontier deliberately flows the BARE row when no param / scalar-subquery /
// correlation binding is in play, because resolving by ordinal against the row
// itself is the whole point of the ordinal model — wrapping it would buy
// nothing and would change a dispatch a large body of census assertions is
// built around.
//
// That is what this test pins, and it is a NEGATIVE result: it exists to prove
// the thing that CANNOT be done, so that the design decision resting on it is
// re-examined if it ever stops being true.
//
// IF THIS TEST GOES RED because frontierRowContext started returning a
// RowEvalContext unconditionally, the contexts have been unified and the
// context carrier becomes VIABLE again — at which point the bake should be
// reconsidered in favour of Java's shape, since the only reason to diverge was
// the absence of a uniform carrier.
func TestFrontierContextIsNotAUniformCarrier(t *testing.T) {
	t.Parallel()

	// A plain frontier row: no leg boundaries, no bindings. This is the
	// commonest shape on the non-join frontier and the one that measurably
	// reaches RecordConstructorValue.Evaluate most often.
	row := NewPositionalRow(positionalTypeFromNames([]string{"_0", "_1"}))
	row.Set(0, int64(1))
	row.Set(1, "a")

	got, err := frontierRowContext(row, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// The load-bearing assertion: what comes back IS the row, not a context
	// wrapping it. A value evaluated against this has no binding surface at
	// all, so there is nowhere to hang a type repository.
	if _, isRowEval := got.(*values.RowEvalContext); isRowEval {
		t.Fatal("frontierRowContext wrapped a bindingless frontier row in a " +
			"*values.RowEvalContext; the eval contexts have been unified, so the " +
			"context carrier for the type repository is viable again — see RFC-204 §4.5")
	}
	if got != values.OrdinalRow(row) {
		t.Fatalf("frontierRowContext returned %T, want the bare *PositionalRow itself", got)
	}
}

// TestFrontierContextCarrierVariants pins the FULL set of shapes
// frontierRowContext can hand to Value.Evaluate. The count is the point: a
// carrier the record constructor could rely on would have to be common to
// every one of them, and these do not share a binding interface.
//
// Kept alongside the test above because that one proves only the bare-row arm;
// a change that wrapped ONLY the bindingless arm would flip that test while
// leaving the surface just as non-uniform.
func TestFrontierContextCarrierVariants(t *testing.T) {
	t.Parallel()

	plain := NewPositionalRow(positionalTypeFromNames([]string{"_0"}))
	plain.Set(0, int64(1))

	t.Run("bindingless frontier flows the bare row", func(t *testing.T) {
		t.Parallel()
		got, err := frontierRowContext(plain, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.(values.OrdinalRow); !ok {
			t.Fatalf("got %T, want a bare values.OrdinalRow", got)
		}
	})

	t.Run("a binding context wraps into RowEvalContext", func(t *testing.T) {
		t.Parallel()
		ec := EmptyEvaluationContext().WithParams([]any{int64(7)})
		got, err := frontierRowContext(plain, ec, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.(*values.RowEvalContext); !ok {
			t.Fatalf("got %T, want *values.RowEvalContext", got)
		}
	})
}
