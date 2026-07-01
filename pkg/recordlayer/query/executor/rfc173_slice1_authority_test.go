package executor

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestFrontierOrdinalAuthority_RFC173Slice1 is the RFC-173 Slice 1 AUTHORITY
// PROOF: it proves the non-join frontier resolves columns by ORDINAL against the
// positional row, NOT by name against the Datum map. The crafted row's name-keyed
// Datum is DELIBERATELY WRONG (V -> 999); only the positional row is correct
// (V -> 42 at ordinal 0). The projection, filter, and map resolution the executor
// uses (frontierRowContext + Value.Evaluate / predicate.Eval — the exact
// production code executeProjection/executeFilter/executePredicatesFilter/executeMap
// run per row) must return the POSITIONAL value. Were the flip silently dark
// (execution still reading the Datum), every assertion here would read 999 and the
// full suite would still pass — this test is the guard against that.
func TestFrontierOrdinalAuthority_RFC173Slice1(t *testing.T) {
	t.Parallel()

	// Positional row: renamed OUTPUT column V at ordinal 0 = 42. The name-keyed
	// Datum is deliberately wrong: V -> 999, plus a stale ID key the ordinal type
	// doesn't carry.
	pos := &PositionalRow{
		Type:  positionalTypeFromNames([]string{"V"}),
		Slots: []any{int64(42)},
	}
	badDatum := map[string]any{"V": int64(999), "ID": int64(999)}
	qr := QueryResult{Datum: badDatum, Positional: pos}

	fieldV := values.NewFlatFieldValue("V", values.UnknownType)
	emptyEC := EmptyEvaluationContext()

	// (1) PROJECTION path: the frontier row context is the bare positional row (no
	// binding context), and Value.Evaluate must read 42, not the Datum's 999.
	projCtx := frontierRowContext(qr.Positional, emptyEC, hasBindingContext(emptyEC))
	if _, isBare := projCtx.(*PositionalRow); !isBare {
		t.Fatalf("with no binding context the frontier must flow the bare positional row, got %T", projCtx)
	}
	got, err := fieldV.Evaluate(projCtx)
	if err != nil {
		t.Fatalf("projection eval: %v", err)
	}
	if got != int64(42) {
		t.Fatalf("projection read %v — wants 42 (positional), not 999 (Datum): ordinal resolution is NOT authoritative (silently-dark flip)", got)
	}

	// (2) FILTER path: predicate `V = 42` must be TRUE against the positional row.
	// If the frontier read the Datum, V would be 999 and the predicate FALSE.
	pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: int64(42), Typ: values.TypeInt},
	})
	res, err := pred.Eval(projCtx)
	if err != nil {
		t.Fatalf("filter eval: %v", err)
	}
	if res != predicates.TriTrue {
		t.Fatalf("filter `V = 42` = %v — the frontier resolved V to the Datum's 999, not the positional 42", res)
	}

	// (3) MAP path: a RecordConstructorValue (the Map plan's result value) over the
	// frontier row must carry the positional value.
	rc := values.NewRecordConstructorValue(values.RecordConstructorField{Name: "OUT", Value: fieldV})
	out, err := rc.Evaluate(projCtx)
	if err != nil {
		t.Fatalf("map eval: %v", err)
	}
	if m, ok := out.(map[string]any); !ok || m["OUT"] != int64(42) {
		t.Fatalf("map produced %v — wants OUT=42 (positional)", out)
	}

	// (4) WRAPPED (RowContextPositional) path: with a param present the frontier
	// wraps so an outer correlation resolves via the binder — but the frontier
	// quantifier's own column still resolves against Positional, ahead of the
	// name-keyed Datum. RowEvalContext.Positional must precede RowEvalContext.Datum.
	ecWithParam := EmptyEvaluationContext().WithParams([]any{int64(7)})
	wrapped := frontierRowContext(qr.Positional, ecWithParam, hasBindingContext(ecWithParam))
	if _, isRow := wrapped.(*values.RowEvalContext); !isRow {
		t.Fatalf("with a param present the frontier must wrap in *RowEvalContext, got %T", wrapped)
	}
	got, err = fieldV.Evaluate(wrapped)
	if err != nil {
		t.Fatalf("wrapped projection eval: %v", err)
	}
	if got != int64(42) {
		t.Fatalf("wrapped projection read %v — RowEvalContext.Positional must precede the name-keyed Datum", got)
	}

	// (5) A miss on the positional frontier is LOUD, never a silent Datum fallback
	// (Graefe: no name-map fallback). Referencing ID — absent from the [V] positional
	// type but present (=999) in the Datum — must raise OrdinalResolutionError, NOT
	// silently return 999.
	fieldID := values.NewFlatFieldValue("ID", values.UnknownType)
	var ore *values.OrdinalResolutionError
	if _, err = fieldID.Evaluate(projCtx); !errors.As(err, &ore) {
		t.Fatalf("frontier miss on ID must be a loud OrdinalResolutionError (no name-map fallback), got %v", err)
	}
}
