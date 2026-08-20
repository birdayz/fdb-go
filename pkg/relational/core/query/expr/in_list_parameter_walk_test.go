package expr_test

// A PREPARED PARAMETER among IN-list items, at the seam where one actually
// exists.
//
// This test exists because the test that was supposed to cover it could not.
// Through database/sql the driver never plans a parameter at all:
// substituteParams "replaces positional '?' placeholders in a query with SQL
// literal representations of the supplied driver values" (embedded/utilities.go)
// BEFORE the parser runs, so `x IN (?, 999)` reaches the engine as the constant
// list `x IN (5, 999)` and takes the plan-time fold exactly as it always did.
// A driver-level test of that shape is green whether or not a ParameterValue is
// handled at all — it is not testing what its name says.
//
// The walker is the seam that keeps the `?`. parseFirstWhereExpr parses SQL
// text directly, so the QUESTION token reaches walkPreparedStatementParameter
// and a real ParameterValue is built. That is the only place the claim can be
// checked, and the claim is load-bearing: ParameterValue is not
// IsConstantValue, so an IN list holding one takes the RUNTIME array fork, and
// if that fork mishandled it the result would be an unbound NULL item — a
// silently wrong answer rather than an error.
//
// The exported planner path (PlanRecordQueryWithMetadata + ParameterValue
// bindings) does reach this shape in production, which is why it is worth
// pinning rather than treating as unreachable — see promotableToUuid's note on
// the same distinction.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
)

// paramRow is an eval context that carries BOTH the row and a parameter
// binding, which is what a runtime IN list over a parameter needs: the LHS
// resolves from the row and the item resolves from the binder, in the same
// Evaluate call.
type paramRow struct {
	ordinalPredRow
	bound map[int]any
}

func (p paramRow) BindParameter(ordinal int, _ string) (any, bool) {
	v, ok := p.bound[ordinal]
	return v, ok
}

func TestWalkPredicate_InListWithParameterTakesTheRuntimeFork(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id IN (?, 999)")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected a ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != predicates.ComparisonIn {
		t.Fatalf("Type: got %v, want In", cp.Comparison.Type)
	}

	// THE CLAIM: a parameter makes the list non-constant, so it must NOT have
	// been folded at plan time. A folded operand here would mean the parameter
	// was read as its zero value before any binding existed.
	if values.IsConstantValue(cp.Comparison.Operand) {
		t.Fatalf("an IN list holding a parameter folded at PLAN time (operand %T). The "+
			"parameter has no value yet, so the fold can only have recorded a NULL — and the "+
			"query would then answer as though nothing had been bound",
			cp.Comparison.Operand)
	}
	if _, isArray := cp.Comparison.Operand.(*values.ArrayConstructorValue); !isArray {
		t.Fatalf("the runtime fork must build an ArrayConstructorValue, got %T — that is the "+
			"shape whose Evaluate returns the []any the IN comparison consumes",
			cp.Comparison.Operand)
	}

	// And it must EVALUATE against a binding. Two different bindings must give
	// two different answers: an unbound parameter answers the same both times,
	// which a single-binding assertion would accept.
	eval := func(id int64, bound any) predicates.TriBool {
		t.Helper()
		row := paramRow{
			ordinalPredRow: usersRow(map[string]any{"ID": id}),
			bound:          map[int]any{1: bound},
		}
		got, evalErr := pred.Eval(&values.RowEvalContext{Positional: row, Binder: row})
		if evalErr != nil {
			t.Fatalf("eval id=%d bound=%v: %v", id, bound, evalErr)
		}
		return got
	}

	for _, c := range []struct {
		id    int64
		bound any
		want  predicates.TriBool
	}{
		{id: 5, bound: int64(5), want: predicates.TriTrue},   // matches the parameter
		{id: 5, bound: int64(6), want: predicates.TriFalse},  // same row, different binding
		{id: 999, bound: int64(5), want: predicates.TriTrue}, // matches the literal item
		{id: 7, bound: int64(6), want: predicates.TriFalse},  // matches neither
	} {
		if got := eval(c.id, c.bound); got != c.want {
			t.Errorf("id=%d bound=%v: got %v, want %v — the same answer for two different "+
				"bindings means the parameter never resolved and was compared as NULL",
				c.id, c.bound, got, c.want)
		}
	}
}
