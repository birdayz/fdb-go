package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestExecuteInUnion_KnownEmptySourceSkipsInner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources [][]any
	}{
		{name: "only empty", sources: [][]any{{}}},
		{name: "unknown then empty", sources: [][]any{nil, {}}},
		{name: "empty then unknown", sources: [][]any{{}, nil}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bindings := make([]string, len(test.sources))
			for i := range bindings {
				bindings[i] = "binding"
			}
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				bindings,
				nil,
				false,
			))
			inUnion = inUnion.WithInSources(test.sources)

			ctx := context.Background()
			cursor, err := executeInUnion(
				ctx,
				inUnion,
				nil,
				nil,
				nil,
				recordlayer.ExecuteProperties{},
			)
			if err != nil {
				t.Fatalf("executeInUnion() error = %v", err)
			}
			result, err := cursor.OnNext(ctx)
			if err != nil {
				t.Fatalf("empty cursor OnNext() error = %v", err)
			}
			if result.HasNext() {
				t.Fatal("known-empty InUnion unexpectedly produced a row")
			}
			if !result.GetNoNextReason().IsSourceExhausted() {
				t.Fatalf("known-empty InUnion stopped with %v, want SourceExhausted", result.GetNoNextReason())
			}
		})
	}
}

func TestExecuteInUnion_MultiBindingSingletonBindsCompleteContext(t *testing.T) {
	t.Parallel()

	const (
		firstBinding  = "first"
		secondBinding = "second"
	)
	inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
		mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
			mustTestQOV(t, values.NamedCorrelationIdentifier(firstBinding), values.NotNullLong),
			mustTestQOV(t, values.NamedCorrelationIdentifier(secondBinding), values.NotNullLong),
		})),
		[]string{firstBinding, secondBinding},
		nil,
		false,
	))
	inUnion = inUnion.WithInSources([][]any{{int64(11)}, {int64(22)}})

	ctx := context.Background()
	cursor, err := executeInUnion(
		ctx,
		inUnion,
		nil,
		EmptyEvaluationContext(),
		nil,
		recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("executeInUnion() error = %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll() error = %v", err)
	}
	if len(results) != 1 || results[0].Positional == nil {
		t.Fatalf("results = %+v, want one positional row", results)
	}
	slots := results[0].Positional.Slots
	if len(slots) != 2 || slots[0] != int64(11) || slots[1] != int64(22) {
		t.Fatalf("bound slots = %#v, want [11 22]", slots)
	}
}

func TestExecuteInUnion_PreservesUniqueBindingIdentity(t *testing.T) {
	t.Parallel()
	binding := values.UniqueCorrelationIdentifier()
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		mustTestQOV(t, binding, values.NotNullLong),
	}))
	inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlanWithBindingAliases(
		inner, []values.CorrelationIdentifier{binding}, nil, false))
	inUnion = inUnion.WithInSources([][]any{{int64(11), int64(22)}})
	aliases := inUnion.GetBindingAliases()
	if len(aliases) != 1 || aliases[0] != binding {
		t.Fatalf("binding aliases = %v, want exact unique alias %v", aliases, binding)
	}
	if named := values.NamedCorrelationIdentifier(binding.Name()); named == binding {
		t.Fatal("fixture requires unique and named aliases with the same spelling to stay distinct")
	}

	cursor, err := executeInUnion(
		context.Background(), inUnion, nil, EmptyEvaluationContext(), nil,
		recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("executeInUnion: %v", err)
	}
	results, err := CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for i, want := range []int64{11, 22} {
		if results[i].Positional == nil || len(results[i].Positional.Slots) != 1 ||
			results[i].Positional.Slots[0] != want {
			t.Fatalf("result %d = %+v, want [%d]", i, results[i], want)
		}
	}
}

func TestExecuteInUnion_SingletonAppliesSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings []string
		sources  [][]any
	}{
		{
			name:     "one binding",
			bindings: []string{"a"},
			sources:  [][]any{{1}},
		},
		{
			name:     "two bindings",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1}, {2}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				test.bindings,
				nil,
				false,
			))
			inUnion = inUnion.WithInSources(test.sources)

			ctx := context.Background()
			cursor, err := executeInUnion(
				ctx,
				inUnion,
				nil,
				EmptyEvaluationContext(),
				nil,
				recordlayer.ExecuteProperties{Skip: 1},
			)
			if err != nil {
				t.Fatalf("executeInUnion() error = %v", err)
			}
			results, err := CollectAll(ctx, cursor)
			if err != nil {
				t.Fatalf("CollectAll() error = %v", err)
			}
			if len(results) != 0 {
				t.Fatalf("Skip=1 returned %d singleton rows, want 0", len(results))
			}
		})
	}
}

func TestExecuteInUnion_RejectsMismatchedDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings []string
		sources  [][]any
	}{
		{
			name:     "extra empty source",
			bindings: []string{"a"},
			sources:  [][]any{{1}, {}},
		},
		{
			name:     "missing source",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inUnion := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
				test.bindings,
				nil,
				false,
			))
			inUnion = inUnion.WithInSources(test.sources)
			if _, err := executeInUnion(
				context.Background(),
				inUnion,
				nil,
				EmptyEvaluationContext(),
				nil,
				recordlayer.ExecuteProperties{},
			); err == nil {
				t.Fatal("executeInUnion() accepted mismatched dimensions")
			}
		})
	}
}

func TestExecuteValues_RequestSkipLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		skip   int
		cap    int
		rows   int
		reason recordlayer.NoNextReason
	}{
		{"unlimited", 0, 0, 1, recordlayer.SourceExhausted},
		{"skip", 1, 0, 0, recordlayer.SourceExhausted},
		{"large_skip", math.MaxInt, 0, 0, recordlayer.SourceExhausted},
		{"negative_skip", -1, 0, 1, recordlayer.SourceExhausted},
		{"cap", 0, 1, 1, recordlayer.ReturnLimitReached},
		{"skip_then_cap", 1, 1, 0, recordlayer.SourceExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			plan := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{&values.ConstantValue{Value: int64(42), Typ: values.NotNullLong}}))
			props := recordlayer.ExecuteProperties{Skip: tc.skip, ReturnedRowLimit: tc.cap}
			cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, props)
			if err != nil {
				t.Fatal(err)
			}
			defer cursor.Close()
			var rowToken []byte
			for i := 0; ; i++ {
				r, err := cursor.OnNext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !r.HasNext() {
					if i != tc.rows || r.GetNoNextReason() != tc.reason {
						t.Fatalf("rows/stop = %d/%v, want %d/%v", i, r.GetNoNextReason(), tc.rows, tc.reason)
					}
					if tc.reason == recordlayer.ReturnLimitReached {
						stopToken, err := r.GetContinuation().ToBytes()
						if err != nil {
							t.Fatal(err)
						}
						if r.GetContinuation().IsEnd() || !bytes.Equal(stopToken, rowToken) {
							t.Fatalf("capped stop lost row continuation: row=%x stop=%x", rowToken, stopToken)
						}
						resumed, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), rowToken, props)
						if err != nil {
							t.Fatal(err)
						}
						defer resumed.Close()
						next, err := resumed.OnNext(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if next.HasNext() || next.GetNoNextReason() != recordlayer.SourceExhausted {
							t.Fatalf("resumed VALUES must exhaust without repeating its row: %v", next)
						}
					}
					break
				}
				if i >= tc.rows || r.GetValue().Positional.Slots[0] != int64(42) {
					t.Fatalf("unexpected VALUES row %d: %v", i, r.GetValue())
				}
				rowToken, err = r.GetContinuation().ToBytes()
				if err != nil {
					t.Fatal(err)
				}
				if len(rowToken) == 0 || r.GetContinuation().IsEnd() {
					t.Fatal("VALUES row lost its resumable list position")
				}
			}
		})
	}
}

func TestExecuteMappedValues_RequestSkipAvoidsEvaluation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"projection", "map"} {
		for _, skip := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/skip=%d", kind, skip), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil))
				division := &values.ArithmeticValue{
					Op:    values.OpDiv,
					Left:  &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
					Right: &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong},
				}
				var plan plans.RecordQueryPlan
				if kind == "projection" {
					plan = mustExecutorConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{division}, inner))
				} else {
					plan = mustExecutorConstruct(plans.NewRecordQueryMapPlan(inner, division))
				}
				cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil,
					recordlayer.DefaultExecuteProperties().WithSkip(skip).WithReturnedRowLimit(1))
				if err != nil {
					t.Fatal(err)
				}
				defer cursor.Close()
				row, err := cursor.OnNext(ctx)
				if skip == 0 {
					var zero *values.ArithmeticDivisionByZeroError
					if !errors.As(err, &zero) {
						t.Fatalf("unskipped division = %v, want ArithmeticDivisionByZeroError", err)
					}
				} else if err != nil || row.HasNext() || row.GetNoNextReason() != recordlayer.SourceExhausted {
					t.Fatalf("request-skipped row must not be evaluated: row=%v err=%v", row, err)
				}
			})
		}
	}
}

func TestExecuteMappedPages_ChildContinuation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"projection", "map"} {
		for _, filtered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/filter=%t", kind, filtered), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				eval := EmptyEvaluationContext()
				alias := values.NamedCorrelationIdentifier("mapped_source")
				typ := exactTestRowType(values.Field{Name: "V", FieldType: values.NullableLong})
				for id := int64(1); id <= 6; id++ {
					if err := eval.GetOrCreateTempTable(alias, nil).Add(QueryResult{Positional: &PositionalRow{Type: typ, Slots: []any{id}}}); err != nil {
						t.Fatal(err)
					}
				}
				var inner plans.RecordQueryPlan = mustTempTableScan(t, eval, alias)
				first := int64(2)
				if filtered {
					pred := predicates.NewComparisonPredicate(mustTestFieldOrdinal(t, inner.GetResultValue(), 0), predicates.Comparison{
						Type: predicates.ComparisonGreaterThan, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
					})
					inner = mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(inner, []predicates.QueryPredicate{pred}))
					first = 3
				}
				field := mustTestFieldOrdinal(t, inner.GetResultValue(), 0)
				var plan plans.RecordQueryPlan
				if kind == "projection" {
					plan = mustExecutorConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{field}, inner))
				} else {
					plan = mustExecutorConstruct(plans.NewRecordQueryMapPlan(inner, field))
				}
				readPage := func(p plans.RecordQueryPlan, continuation []byte, skip int, want []int64) []byte {
					t.Helper()
					cursor, err := ExecutePlan(ctx, p, nil, eval, continuation,
						recordlayer.DefaultExecuteProperties().WithSkip(skip).WithReturnedRowLimit(2))
					if err != nil {
						t.Fatal(err)
					}
					defer cursor.Close()
					var got []int64
					var last []byte
					for {
						row, err := cursor.OnNext(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if !row.HasNext() {
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("skip %d page = %v, want %v", skip, got, want)
							}
							if len(want) == 2 {
								if row.GetNoNextReason() != recordlayer.ReturnLimitReached || row.GetContinuation().IsEnd() || len(last) == 0 || !bytes.Equal(last, continuationBytesForTest(t, row.GetContinuation())) {
									t.Fatalf("capped page must retain last child token: %v, %x != %x", row, last, continuationBytesForTest(t, row.GetContinuation()))
								}
							} else if row.GetNoNextReason() != recordlayer.SourceExhausted || !row.GetContinuation().IsEnd() {
								t.Fatalf("short final page must exhaust: %v", row)
							}
							return continuationBytesForTest(t, row.GetContinuation())
						}
						v, _ := row.GetValue().Positional.Get(0)
						got = append(got, v.(int64))
						last = continuationBytesForTest(t, row.GetContinuation())
					}
				}
				continuation := readPage(plan, nil, 1, []int64{first, first + 1})
				childContinuation := readPage(inner, nil, 1, []int64{first, first + 1})
				if !bytes.Equal(continuation, childContinuation) {
					t.Fatalf("map wrapped child continuation: %x != %x", continuation, childContinuation)
				}
				readPage(plan, continuation, 0, []int64{first + 2, first + 3})
				want := []int64{first + 3}
				if !filtered {
					want = append(want, first+4)
				}
				readPage(plan, continuation, 1, want)
			})
		}
	}
}

func TestExecuteMappedTempTableInsert_DelegatesRequest(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"projection", "map"} {
		for _, owning := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owning=%t", kind, owning), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				eval := EmptyEvaluationContext()
				source := values.NamedCorrelationIdentifier("mapped_insert_source")
				target := values.NamedCorrelationIdentifier("mapped_insert_target")
				typ := exactTestRowType(values.Field{Name: "V", FieldType: values.NullableLong})
				for id := int64(1); id <= 3; id++ {
					if err := eval.GetOrCreateTempTable(source, nil).Add(QueryResult{Positional: &PositionalRow{Type: typ, Slots: []any{id}}}); err != nil {
						t.Fatal(err)
					}
				}
				inner := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(mustTempTableScan(t, eval, source), target, owning))
				field := mustTestFieldOrdinal(t, inner.GetResultValue(), 0)
				var plan plans.RecordQueryPlan
				if kind == "projection" {
					plan = mustExecutorConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{field}, inner))
				} else {
					plan = mustExecutorConstruct(plans.NewRecordQueryMapPlan(inner, field))
				}
				// Java MapPlan delegates the original request. TempTableInsertPlan
				// deliberately clears it: mapping must not invent an output cap.
				cursor, err := ExecutePlan(ctx, plan, nil, eval, nil,
					recordlayer.DefaultExecuteProperties().WithSkip(1).WithReturnedRowLimit(1))
				if err != nil {
					t.Fatal(err)
				}
				defer cursor.Close()
				for id := int64(1); id <= 3; id++ {
					row, err := cursor.OnNext(ctx)
					if err != nil || !row.HasNext() {
						t.Fatalf("inserted row %d: %v, %v", id, row, err)
					}
					if got, _ := row.GetValue().Positional.Get(0); got != id {
						t.Fatalf("inserted ID = %v, want %d", got, id)
					}
				}
				end, err := cursor.OnNext(ctx)
				if err != nil || end.HasNext() || end.GetNoNextReason() != recordlayer.SourceExhausted || !end.GetContinuation().IsEnd() {
					t.Fatalf("insert exhaustion: %v, %v", end, err)
				}
				if got := len(eval.GetOrCreateTempTable(target, nil).GetList()); got != 3 {
					t.Fatalf("materialized %d rows, want all 3", got)
				}
			})
		}
	}
}
