package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func TestExecutorTextComparisonErrorTypes(t *testing.T) {
	t.Parallel()

	type errKind int
	const (
		wantArg errKind = iota
		wantMeta
	)

	cases := []struct {
		name string
		val  any
		typ  values.Type
		cmp  predicates.Comparison
		kind errKind
	}{
		{
			name: "empty prefix",
			val:  "fox",
			typ:  values.TypeString,
			cmp: predicates.Comparison{
				Type:    predicates.ComparisonTextContainsPrefix,
				Operand: &values.ConstantValue{Value: "", Typ: values.NotNullString},
			},
			kind: wantArg,
		},
		{
			name: "multi-token prefix",
			val:  "a b",
			typ:  values.TypeString,
			cmp: predicates.Comparison{
				Type:    predicates.ComparisonTextContainsPrefix,
				Operand: values.LiteralValue([]string{"a", "b"}),
			},
			kind: wantArg,
		},
		{
			name: "non-string value",
			val:  int64(42),
			typ:  values.NotNullLong,
			cmp: predicates.Comparison{
				Type:    predicates.ComparisonTextContainsAll,
				Operand: &values.ConstantValue{Value: "fox", Typ: values.NotNullString},
			},
			kind: wantArg,
		},
		{
			name: "unknown tokenizer",
			val:  "fox",
			typ:  values.TypeString,
			cmp: predicates.Comparison{
				Type:              predicates.ComparisonTextContainsAll,
				Operand:           &values.ConstantValue{Value: "fox", Typ: values.NotNullString},
				TextTokenizerName: "missing-tokenizer",
			},
			kind: wantMeta,
		},
	}

	assertErr := func(t *testing.T, err error, kind errKind) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		switch kind {
		case wantArg:
			var argErr *recordlayer.RecordCoreArgumentError
			if !errors.As(err, &argErr) {
				t.Fatalf("err = %T (%v), want *recordlayer.RecordCoreArgumentError", err, err)
			}
		case wantMeta:
			var mdErr *recordlayer.MetaDataError
			if !errors.As(err, &mdErr) {
				t.Fatalf("err = %T (%v), want *recordlayer.MetaDataError", err, err)
			}
		}
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rowType := exactTestRowType(values.Field{Name: "BODY", FieldType: tc.typ})

			for _, carrier := range []struct {
				name string
				make func(inner plans.RecordQueryPlan, pred predicates.QueryPredicate) plans.RecordQueryPlan
			}{
				{
					name: "RecordQueryFilterPlan",
					make: func(inner plans.RecordQueryPlan, pred predicates.QueryPredicate) plans.RecordQueryPlan {
						return mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, inner))
					},
				},
				{
					name: "RecordQueryPredicatesFilterPlan",
					make: func(inner plans.RecordQueryPlan, pred predicates.QueryPredicate) plans.RecordQueryPlan {
						return mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(inner, []predicates.QueryPredicate{pred}))
					},
				},
			} {
				carrier := carrier
				t.Run(carrier.name, func(t *testing.T) {
					t.Parallel()
					alias := values.NamedCorrelationIdentifier("txt_err_" + tc.name + "_" + carrier.name)
					evalCtx := EmptyEvaluationContext()
					table := evalCtx.GetOrCreateTempTable(alias, nil)
					if err := table.Add(QueryResult{
						Positional: &PositionalRow{Type: rowType, Slots: []any{tc.val}},
						PrimaryKey: tuple.Tuple{"T", int64(1)},
					}); err != nil {
						t.Fatalf("seed temp table: %v", err)
					}
					scan := mustTempTableScan(t, evalCtx, alias)
					pred := predicates.NewComparisonPredicate(mustTestFieldOrdinal(t, scan.GetResultValue(), 0), tc.cmp)
					plan := carrier.make(scan, pred)
					cursor, err := ExecutePlan(context.Background(), plan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
					if err != nil {
						assertErr(t, err, tc.kind)
						return
					}
					defer cursor.Close()
					_, err = CollectAll(context.Background(), cursor)
					assertErr(t, err, tc.kind)
				})
			}

			t.Run("passesJoinPredicatesLegs", func(t *testing.T) {
				t.Parallel()
				pred := predicates.NewComparisonPredicate(mustTestFieldAt(t, rowType, 0), tc.cmp)
				combined := QueryResult{Positional: &PositionalRow{Type: rowType, Slots: []any{tc.val}}}
				_, err := passesJoinPredicatesLegs(combined, []predicates.QueryPredicate{pred}, EmptyEvaluationContext(), nil)
				assertErr(t, err, tc.kind)
			})

			t.Run("scanKeyFilter", func(t *testing.T) {
				t.Parallel()
				filter := &scanKeyFilter{
					components: []scanKeyFilterComponent{
						{position: 0, comparisons: []predicates.Comparison{tc.cmp}},
					},
				}
				_, err := filter.admits(tuple.Tuple{tc.val}, 0)
				assertErr(t, err, tc.kind)
			})
		})
	}
}
