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
			Type:  exactTestRowType(values.Field{Name: "V", FieldType: values.NullableLong}),
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
	return mustTempTableScan(t, evalCtx, corr)
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
// type at all must surface a loud runtime-shape ResolutionError — there is no
// fallback row model to silently paper over the miss.
func TestFrontierOrdinalAuthority(t *testing.T) {
	t.Parallel()

	t.Run("projection", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		inner := authorityInner(t, evalCtx, "auth_proj")
		fieldV := mustTestFieldOrdinal(t, inner.GetResultValue(), 0)
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{fieldV}, inner))
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
		inner := authorityInner(t, evalCtx, "auth_filter")
		fieldV := mustTestFieldOrdinal(t, inner.GetResultValue(), 0)
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.NullableLong},
		})
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{pred}, inner))
		rows := authorityCollect(t, filter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executeFilter kept %d rows, want 1 — `V = 42` must evaluate TRUE against the positional row", len(rows))
		}
	})

	t.Run("predicates_filter", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		inner := authorityInner(t, evalCtx, "auth_pfilter")
		fieldV := mustTestFieldOrdinal(t, inner.GetResultValue(), 0)
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.NullableLong},
		})
		pfilter := mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(
			inner, []predicates.QueryPredicate{pred}))
		rows := authorityCollect(t, pfilter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executePredicatesFilter kept %d rows, want 1 — `V = 42` must evaluate TRUE against the positional row", len(rows))
		}
	})

	t.Run("predicates_filter_declared_alias", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		inner := authorityInner(t, evalCtx, "auth_pfilter_declared")
		logicalAlias := values.NamedCorrelationIdentifier("T")
		logicalQOV, err := values.NewQuantifiedObjectValue(logicalAlias, inner.GetResultType())
		if err != nil {
			t.Fatalf("logical input QOV: %v", err)
		}
		fieldV := mustTestFieldOrdinal(t, logicalQOV, 0)
		pred := predicates.NewComparisonPredicate(fieldV, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42), Typ: values.NullableLong},
		})
		pfilter := mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
			inner, []predicates.QueryPredicate{pred}, logicalAlias))
		if got := pfilter.GetInnerQuantifier().GetAlias(); got == logicalAlias {
			t.Fatalf("test requires distinct physical and declared aliases, both are %q", got.Name())
		}
		rows := authorityCollect(t, pfilter, evalCtx)
		if len(rows) != 1 {
			t.Fatalf("executePredicatesFilter kept %d rows, want 1 — declared alias T must bind the exact input row", len(rows))
		}
	})

	t.Run("map", func(t *testing.T) {
		t.Parallel()
		evalCtx := EmptyEvaluationContext()
		inner := authorityInner(t, evalCtx, "auth_map")
		fieldV := mustTestFieldOrdinal(t, inner.GetResultValue(), 0)
		rc := values.NewRecordConstructorValue(values.RecordConstructorField{Name: "OUT", Value: fieldV})
		mp := mustExecutorConstruct(plans.NewRecordQueryMapPlan(inner, rc))
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
		// ID is declared by the scan but absent from its runtime [V] carrier.
		// Exact layout publication must reject that disagreement before the
		// projection can read an ambient ordinal or silently invent a value.
		declared := exactTestRowType(
			values.Field{Name: "V", FieldType: values.NullableLong},
			values.Field{Name: "ID", FieldType: values.NullableLong},
		)
		alias := values.NamedCorrelationIdentifier("auth_miss")
		if err := evalCtx.GetOrCreateTempTable(alias, nil).Add(authorityRow()); err != nil {
			t.Fatalf("temp table add: %v", err)
		}
		inner := mustExecutorConstruct(plans.NewRecordQueryTempTableScanPlan(alias, declared))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{mustTestFieldOrdinal(t, inner.GetResultValue(), 1)}, inner))
		cursor, err := ExecutePlan(context.Background(), proj, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		_, err = CollectAll(context.Background(), cursor)
		var resolutionErr *values.ResolutionError
		if !errors.As(err, &resolutionErr) {
			t.Fatalf("frontier miss on ID must surface a loud ResolutionError through the cursor (no fallback), got %v", err)
		}
		if resolutionErr.Code() != values.LayoutCarrierMismatch {
			t.Fatalf("frontier miss code = %v, want LayoutCarrierMismatch: %v", resolutionErr.Code(), err)
		}
	})
}
