package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// authorityRow builds the discriminating frontier row: the name-keyed Datum is
// DELIBERATELY WRONG (V -> 999, plus a stale ID key the positional type doesn't
// carry) while the positional row is correct (V -> 42 at ordinal 0). Any
// resolution that reads the Datum yields 999; only ordinal resolution yields 42.
func authorityRow() QueryResult {
	return QueryResult{
		Datum: map[string]any{"V": int64(999), "ID": int64(999)},
		Positional: &PositionalRow{
			Type:  positionalTypeFromNames([]string{"V"}),
			Slots: []any{int64(42)},
		},
	}
}

// authorityInner binds the discriminating row into a temp table and returns the
// scan plan over it — the storeless inner used to drive each REAL dispatch site
// through ExecutePlan.
func authorityInner(t *testing.T, evalCtx *EvaluationContext, alias string) plans.RecordQueryPlan {
	t.Helper()
	corr := values.NamedCorrelationIdentifier(alias)
	tt := evalCtx.GetOrCreateTempTable(corr, nil)
	if err := tt.Add(authorityRow()); err != nil {
		t.Fatalf("temp table add: %v", err)
	}
	return plans.NewRecordQueryTempTableScanPlan(corr)
}

func authorityCollect(t *testing.T, p plans.RecordQueryPlan, evalCtx *EvaluationContext) []QueryResult {
	t.Helper()
	cursor, err := ExecutePlan(context.Background(), p, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	rows, err := CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rows
}

// TestFrontierOrdinalAuthority_RFC173Slice1 is the RFC-173 Slice 1 AUTHORITY
// PROOF, driven END-TO-END through ExecutePlan so each PRODUCTION dispatch site
// (executeProjection, executeFilter, executePredicatesFilter, executeMap) runs
// its real per-row code over a frontier row whose Datum is deliberately WRONG
// (V->999) and whose Positional is correct (V->42). If any site's
// `qr.Positional != nil` dispatch were deleted (the silently-dark-flip
// scenario), that site would read the Datum's 999 and its subtest here would
// fail — the whole suite otherwise stays green on agreeing rows, which is
// exactly why this test exists (Torvalds review catch: the previous version
// called the dispatch HELPER directly and would not have noticed a deleted
// production dispatch).
func TestFrontierOrdinalAuthority_RFC173Slice1(t *testing.T) {
	t.Parallel()
	fieldV := values.NewFlatFieldValue("V", values.UnknownType)

	t.Run("projection", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{fieldV}, authorityInner(t, evalCtx, "auth_proj"))
		rows := authorityCollect(t, proj, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		m := rows[0].Datum.(map[string]any)
		if m["V"] != int64(42) {
			t.Fatalf("executeProjection read %v — wants 42 (positional), not 999 (Datum): the production dispatch is not ordinal-authoritative", m["V"])
		}
	})

	t.Run("filter", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		// `V = 42` keeps the row ONLY under ordinal resolution (Datum says 999).
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.TypeInt},
		})
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{pred}, authorityInner(t, evalCtx, "auth_filter"))
		rows := authorityCollect(t, filter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executeFilter kept %d rows, want 1 — `V = 42` must be TRUE via positional (Datum's 999 would drop the row)", len(rows))
		}
	})

	t.Run("predicates_filter", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.TypeInt},
		})
		pfilter := plans.NewRecordQueryPredicatesFilterPlan(
			authorityInner(t, evalCtx, "auth_pfilter"), []predicates.QueryPredicate{pred})
		rows := authorityCollect(t, pfilter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executePredicatesFilter kept %d rows, want 1 — `V = 42` must be TRUE via positional", len(rows))
		}
	})

	t.Run("map", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		rc := values.NewRecordConstructorValue(values.RecordConstructorField{Name: "OUT", Value: fieldV})
		mp := plans.NewRecordQueryMapPlan(authorityInner(t, evalCtx, "auth_map"), rc)
		rows := authorityCollect(t, mp, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		m := rows[0].Datum.(map[string]any)
		if m["OUT"] != int64(42) {
			t.Fatalf("executeMap read %v — wants OUT=42 (positional), not 999 (Datum)", m["OUT"])
		}
	})

	t.Run("loud_miss_no_fallback", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		// ID exists ONLY in the (wrong) Datum, not in the [V] positional type: the
		// projection must LOUD-error (no name-map fallback, Graefe), never return 999.
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{values.NewFlatFieldValue("ID", values.UnknownType)},
			authorityInner(t, evalCtx, "auth_miss"))
		cursor, err := ExecutePlan(context.Background(), proj, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		_, err = CollectAll(context.Background(), cursor)
		var ore *values.OrdinalResolutionError
		if !errors.As(err, &ore) {
			t.Fatalf("frontier miss on ID must surface a loud OrdinalResolutionError through the cursor (no name-map fallback), got %v", err)
		}
	})
}
