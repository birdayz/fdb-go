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

// authorityRow builds the frontier row whose sole value source is the
// positional row (V -> 42 at ordinal 0): every dispatch site can ONLY resolve
// V through the ordinal row (there is no other row model), so this pins that
// each REAL dispatch site actually reads it.
func authorityRow() QueryResult {
	return QueryResult{
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

// TestFrontierOrdinalAuthority drives each PRODUCTION dispatch site
// (executeProjection, executeFilter, executePredicatesFilter, executeMap)
// END-TO-END through ExecutePlan — not by calling the dispatch helper
// directly, which would not notice a deleted production dispatch — over a
// frontier row whose only value source is its Positional row (V=42). Each
// subtest reads or filters on V and asserts 42, pinning that the site
// genuinely resolves it off the ordinal row. The final subtest pins the
// complementary half: a reference to a column that ISN'T in the positional
// type at all must surface a loud OrdinalResolutionError — there is no
// fallback row model to silently paper over the miss.
func TestFrontierOrdinalAuthority(t *testing.T) {
	t.Parallel()
	fieldV := values.NewFieldValueWithResolvedOrdinal("V", 0, values.UnknownType)

	t.Run("projection", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{fieldV}, authorityInner(t, evalCtx, "auth_proj"))
		rows := authorityCollect(t, proj, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		m, _ := rowMapOK(rows[0])
		if m["V"] != int64(42) {
			t.Fatalf("executeProjection read %v, want 42 — the production dispatch did not resolve the positional row", m["V"])
		}
	})

	t.Run("filter", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.NullableLong},
		})
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{pred}, authorityInner(t, evalCtx, "auth_filter"))
		rows := authorityCollect(t, filter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executeFilter kept %d rows, want 1 — `V = 42` must evaluate TRUE against the positional row", len(rows))
		}
	})

	t.Run("predicates_filter", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.NullableLong},
		})
		pfilter := plans.NewRecordQueryPredicatesFilterPlan(
			authorityInner(t, evalCtx, "auth_pfilter"), []predicates.QueryPredicate{pred})
		rows := authorityCollect(t, pfilter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executePredicatesFilter kept %d rows, want 1 — `V = 42` must evaluate TRUE against the positional row", len(rows))
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
		m, _ := rowMapOK(rows[0])
		if m["OUT"] != int64(42) {
			t.Fatalf("executeMap read OUT=%v, want 42 — the production dispatch did not resolve the positional row", m["OUT"])
		}
	})

	t.Run("loud_miss_no_fallback", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		// ID is not a column of the row's [V] positional type: the projection
		// must LOUD-error, never silently invent a value.
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
			t.Fatalf("frontier miss on ID must surface a loud OrdinalResolutionError through the cursor (no fallback), got %v", err)
		}
	})
}
