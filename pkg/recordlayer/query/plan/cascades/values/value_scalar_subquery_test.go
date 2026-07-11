package values

import (
	"errors"
	"testing"
)

// TestScalarSubqueryValue_Evaluate pins the correct-or-loud runtime contract:
// an ABSENT binding (nobody pre-evaluated the subquery) is a loud
// *UnboundScalarSubqueryError, while a PRESENT nil binding is the legitimate
// zero-rows SQL NULL. Before the error existed, an absent binding silently
// evaluated to NULL — every comparison against it degraded to UNKNOWN and rows
// vanished with no signal (the plan-harness zero-rows bug: the embedded
// harness planned but never executed the query's scalar subqueries).
func TestScalarSubqueryValue_Evaluate(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("SQ")
	v := NewScalarSubqueryValue(alias)

	t.Run("absent_binding_is_loud", func(t *testing.T) {
		t.Parallel()
		for name, ctx := range map[string]any{
			"nil_map":   &RowEvalContext{},
			"empty_map": &RowEvalContext{ScalarSubqueries: map[CorrelationIdentifier]any{}},
			"other_key": &RowEvalContext{ScalarSubqueries: map[CorrelationIdentifier]any{NamedCorrelationIdentifier("OTHER"): int64(1)}},
			// The executor's no-bindings filter paths pass the raw Datum map (or
			// a bare scalar row) as the row context — a REAL runtime read that
			// can never carry the binding, so it must be loud too (this was the
			// silent arm that kept the plan-harness zero-rows bug invisible on
			// plain scan filters).
			"raw_datum_map":  map[string]any{"K": int64(100)},
			"bare_scalar":    int64(7),
			"string_context": "row",
		} {
			_, err := v.Evaluate(ctx)
			var unbound *UnboundScalarSubqueryError
			if !errors.As(err, &unbound) {
				t.Fatalf("%s: err = %v, want *UnboundScalarSubqueryError", name, err)
			}
			if unbound.Alias != alias {
				t.Fatalf("%s: error alias = %v, want %v", name, unbound.Alias, alias)
			}
		}
	})

	t.Run("present_nil_is_sql_null", func(t *testing.T) {
		t.Parallel()
		got, err := v.Evaluate(&RowEvalContext{ScalarSubqueries: map[CorrelationIdentifier]any{alias: nil}})
		if err != nil {
			t.Fatalf("present-nil binding must be legit zero-rows NULL, got err %v", err)
		}
		if got != nil {
			t.Fatalf("present-nil binding = %v, want nil", got)
		}
	})

	t.Run("present_value_returned", func(t *testing.T) {
		t.Parallel()
		got, err := v.Evaluate(&RowEvalContext{ScalarSubqueries: map[CorrelationIdentifier]any{alias: int64(42)}})
		if err != nil || got != int64(42) {
			t.Fatalf("bound value = (%v, %v), want (42, nil)", got, err)
		}
	})

	// The nil context is the ONE plan-time speculative probe (Comparison.Eval's
	// constant-RHS contract, rule fold probes): no binding can exist before
	// execution, and subquery values are correlated so compile-time comparison
	// classification defers them — nil stays non-committal, never an error.
	t.Run("plan_time_nil_context_stays_nil", func(t *testing.T) {
		t.Parallel()
		got, err := v.Evaluate(nil)
		if got != nil || err != nil {
			t.Fatalf("nil ctx = (%v, %v), want (nil, nil)", got, err)
		}
	})
}
