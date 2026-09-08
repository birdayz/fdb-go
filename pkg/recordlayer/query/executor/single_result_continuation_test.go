package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"google.golang.org/protobuf/proto"
)

// TestSingleResultCursor_EmitsResumableContinuation pins the C6
// hardening: the emitted row carries the consumed-token continuation —
// never nil (banned by NewResultWithValue: a nil made every parent
// snapshot the child as START and replay the row on resume).
func TestSingleResultCursor_EmitsResumableContinuation(t *testing.T) {
	t.Parallel()
	c := newSingleResultCursor(QueryResult{})
	r, err := c.OnNext(context.Background())
	if err != nil || !r.HasNext() {
		t.Fatalf("OnNext: %v", err)
	}
	cont := r.GetContinuation()
	if cont == nil || cont.IsEnd() {
		t.Fatalf("single-result row must carry a resumable continuation, got %v", cont)
	}
	b, err := cont.ToBytes()
	if err != nil || len(b) == 0 {
		t.Fatalf("continuation bytes: %v (%d bytes)", err, len(b))
	}
	// Exhausted after the row.
	r2, err := c.OnNext(context.Background())
	if err != nil || r2.HasNext() || !r2.GetContinuation().IsEnd() {
		t.Fatalf("second OnNext must be SourceExhausted END, got %+v (%v)", r2, err)
	}
}

// TestFirstOrDefault_ConsumedTokenResumesEmpty pins the resume arm: a
// continuation equal to the consumed token means the single row was
// already emitted — the resumed cursor is EMPTY, and the inner plan is
// never re-executed (the nil inner would panic if it were).
func TestFirstOrDefault_ConsumedTokenResumesEmpty(t *testing.T) {
	t.Parallel()
	innerType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	inner := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, innerType, false))
	innerQ := expressions.NewPhysicalQuantifier(expressions.FinalOf(inner))
	p := mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlanFromQuantifier(
		innerQ,
		values.NewNullValue(innerType),
	))
	cur, err := executeFirstOrDefault(
		context.Background(), p, nil, EmptyEvaluationContext(),
		singleResultConsumedToken, recordlayer.DefaultExecuteProperties(),
	)
	if err != nil {
		t.Fatalf("executeFirstOrDefault: %v", err)
	}
	defer cur.Close()
	r, err := cur.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if r.HasNext() {
		t.Fatal("a consumed-token resume must be EMPTY — re-emitting duplicates the row")
	}
}

// TestFirstOrDefault_RecordNullKeepsExactCarrierAndAbsence pins the three
// independent facts an existential record leg needs: the empty arm publishes
// the exact declared record carrier, its whole object is SQL NULL, and a real
// row whose only field is SQL NULL remains a PRESENT record. Collapsing either
// state to an untyped `_0` scalar breaks the layout contract; inferring absence
// from nil slots makes the matched all-NULL mutation answer EXISTS incorrectly.
func TestFirstOrDefault_RecordNullKeepsExactCarrierAndAbsence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recordType := values.NewRecordType("", false, []values.Field{{
		Name: "NULL", FieldType: values.NullableLong, Ordinal: 0,
	}})

	empty := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{}, Typ: values.NewArrayType(false, recordType),
	}))
	matchedAllNull := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		values.NewNullValue(values.NullableLong),
	}))
	if !matchedAllNull.GetResultType().Equals(recordType) {
		t.Fatalf("matched control type = %s, want exact %s", matchedAllNull.GetResultType(), recordType)
	}

	for _, test := range []struct {
		name            string
		inner           plans.RecordQueryPlan
		wantCarrierNull bool
		wantExists      predicates.TriBool
	}{
		{name: "empty is a typed absent record", inner: empty, wantCarrierNull: true, wantExists: predicates.TriFalse},
		{name: "matched all-null record stays present", inner: matchedAllNull, wantCarrierNull: false, wantExists: predicates.TriTrue},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlan(
				test.inner, values.NewNullValue(recordType)))
			cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cursor.Close() }()
			result, err := cursor.OnNext(ctx)
			if err != nil || !result.HasNext() {
				t.Fatalf("FirstOrDefault row = (%v, %v), want one row", result, err)
			}
			row := result.GetValue().Positional
			layout, err := plan.ProvidedOutputLayout()
			if err != nil {
				t.Fatal(err)
			}
			if row == nil || row.Type == nil || !row.Type.Equals(recordType) ||
				row.Layout != layout || len(row.Slots) != len(recordType.Fields) {
				t.Fatalf("row carrier = (%v, %v, width %d), want exact type %s/layout %v/width %d",
					row, row.Layout, len(row.Slots), recordType, layout, len(recordType.Fields))
			}

			edge, err := values.NewQuantifiedObjectValue(
				values.NamedCorrelationIdentifier("EXISTENTIAL_RECORD"), recordType)
			if err != nil {
				t.Fatal(err)
			}
			rowCtx, err := frontierRowContext(row, nil, false, edge)
			if err != nil {
				t.Fatal(err)
			}
			whole, err := edge.Evaluate(rowCtx)
			if err != nil {
				t.Fatal(err)
			}
			if gotNull := whole == nil; gotNull != test.wantCarrierNull {
				t.Fatalf("whole edge object = %T %#v (null=%t), want null=%t",
					whole, whole, gotNull, test.wantCarrierNull)
			}
			exists := predicates.NewComparisonPredicate(
				edge, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
			gotExists, err := exists.Eval(rowCtx)
			if err != nil || gotExists != test.wantExists {
				t.Fatalf("edge IS NOT NULL = (%v, %v), want (%v, nil)", gotExists, err, test.wantExists)
			}

			// The FlatMap fold must consume the same whole-object absence
			// marker. Looking only at the exact-width shell makes an empty
			// existential look like a present all-NULL record and turns EXISTS
			// into an unconditional TRUE.
			innerAlias := values.NamedCorrelationIdentifier("FOLDED_EXISTENTIAL")
			existsValue := mustExecutorConstruct(values.NewExistsValue(innerAlias, recordType))
			foldValue := values.NewRawRecordConstructorValue(values.RecordConstructorField{
				Name: "H", Value: existsValue,
			})
			fold, err := newFlatMapCursorWithOuterProperties(
				recordlayer.FromList([]QueryResult{}), nil, plan, nil,
				EmptyEvaluationContext(), values.NamedCorrelationIdentifier("OUTER"), innerAlias,
				foldValue, recordlayer.ExecuteProperties{}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer fold.Close()
			folded, err := fold.computeResultLegs(
				QueryResult{Positional: NewPositionalRow(recordType)},
				&QueryResult{Positional: row})
			if err != nil {
				t.Fatal(err)
			}
			gotFolded, ok := folded.Positional.Get(0)
			wantFolded := test.wantExists == predicates.TriTrue
			if !ok || gotFolded != wantFolded {
				t.Fatalf("folded EXISTS = (%v, %v), want %v", gotFolded, ok, wantFolded)
			}

			// The ordinal-build FlatMap path has an independent leg adapter.
			// It must bind the same exact shell as NULL/PRESENT using presence,
			// never the shell pointer or the nil contents of its fields.
			build := &ordinalJoinBuild{Enabled: true, LegTypes: map[values.CorrelationIdentifier]*values.RecordType{
				innerAlias: recordType,
			}}
			legs, raw, err := build.legRows(
				values.NamedCorrelationIdentifier("BUILD_OUTER"), innerAlias,
				nil, &QueryResult{Positional: row})
			if err != nil {
				t.Fatal(err)
			}
			builtExists, err := existsValue.Evaluate(&values.RowEvalContext{
				Correlations: &buildLegBinder{legs: legs, raw: raw},
			})
			if err != nil || builtExists != wantFolded {
				t.Fatalf("ordinal-build EXISTS = (%v, %v), want (%v, nil)", builtExists, err, wantFolded)
			}
		})
	}
}

// TestFirstOrDefaultAsFlatMapOuterPreservesWholeObjectPresence pins the other
// FlatMap side. The outer cursor transports both states as an exact-width
// positional shell, so FlatMap must copy FirstOrDefault's carrier-presence
// proof into the correlated exact QOV binding. An empty record is absent and a
// matched record whose only field is NULL is still present.
func TestFirstOrDefaultAsFlatMapOuterPreservesWholeObjectPresence(t *testing.T) {
	t.Parallel()

	recordType := values.NewRecordType("", false, []values.Field{{
		Name: "V", FieldType: values.NullableLong, Ordinal: 0,
	}})
	empty := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{}, Typ: values.NewArrayType(false, recordType),
	}))
	matchedAllNull := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		values.NewNullValue(values.NullableLong),
	}))

	for _, test := range []struct {
		name       string
		inner      plans.RecordQueryPlan
		wantExists bool
	}{
		{name: "empty record stays absent", inner: empty, wantExists: false},
		{name: "matched all-null record stays present", inner: matchedAllNull, wantExists: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			outer := mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlan(
				test.inner, values.NewNullValue(recordType)))
			outerAlias := values.NamedCorrelationIdentifier("FOD_OUTER")
			innerAlias := values.NamedCorrelationIdentifier("FOD_PROBE")
			outerObject := mustTestQOV(t, outerAlias, recordType)
			exists := mustExecutorConstruct(values.NewExistsValue(outerAlias, recordType))
			probe := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{exists}))
			probeObject := mustTestQOV(t, innerAlias, probe.GetResultType())
			flatMap := mustExecutorConstruct(plans.NewRecordQueryFlatMapPlan(
				outer, probe, outerAlias, innerAlias, probeObject, false))

			// Seed a stale enclosing exact value for the same declaration. The
			// local FlatMap outer must replace it, including its absence bit.
			stale := NewPositionalRow(recordType)
			evalCtx, err := EmptyEvaluationContext().withQuantifiedBinding(outerObject, stale, false)
			if err != nil {
				t.Fatal(err)
			}
			cursor, err := ExecutePlan(
				context.Background(), flatMap, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cursor.Close() }()
			result, err := cursor.OnNext(context.Background())
			if err != nil || !result.HasNext() {
				t.Fatalf("FlatMap over FirstOrDefault = (%v, %v), want one row", result, err)
			}
			row := result.GetValue().Positional
			if row == nil || len(row.Slots) != 1 || row.Slots[0] != test.wantExists {
				t.Fatalf("FlatMap EXISTS row = %#v, want [%t]", row, test.wantExists)
			}
		})
	}
}

func TestFirstOrDefault_StrictRequestBoundary(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"direct", "map", "projection"} {
		for _, shape := range []string{"empty", "one", "two", "two_semantic_limit"} {
			for _, skip := range []int{0, 1} {
				for _, cap := range []int{0, 1} {
					t.Run(fmt.Sprintf("%s/%s/skip=%d/cap=%d", kind, shape, skip, cap), func(t *testing.T) {
						t.Parallel()
						ctx := context.Background()
						input := []any{}
						if shape != "empty" {
							input = append(input, int64(11))
						}
						if shape == "two" || shape == "two_semantic_limit" {
							input = append(input, int64(22))
						}
						var inner plans.RecordQueryPlan = mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
							Value: input, Typ: values.NewArrayType(false, values.NotNullLong),
						}))
						if shape == "two_semantic_limit" {
							inner = mustExecutorConstruct(plans.NewRecordQueryLimitPlan(inner, 1, 0))
						}
						var plan plans.RecordQueryPlan = mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlanStrict(inner,
							&values.ConstantValue{Value: int64(99), Typ: values.NotNullLong}))
						constant := &values.ConstantValue{Value: int64(42), Typ: values.NotNullLong}
						if kind == "map" {
							plan = mustExecutorConstruct(plans.NewRecordQueryMapPlan(plan, constant))
						} else if kind == "projection" {
							plan = mustExecutorConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{constant}, plan))
						}
						props := recordlayer.DefaultExecuteProperties().WithSkip(skip).WithReturnedRowLimit(cap)
						cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, props)
						if cursor != nil {
							defer cursor.Close()
						}
						if shape == "two" {
							var cardinality *api.Error
							if !errors.As(err, &cardinality) || cardinality.Code != api.ErrCodeCardinalityViolation {
								t.Fatalf("request must not hide second scalar row: error = %v, want 21000", err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						row, err := cursor.OnNext(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if skip > 0 {
							if row.HasNext() || row.GetNoNextReason() != recordlayer.SourceExhausted || !row.GetContinuation().IsEnd() {
								t.Fatalf("strict singleton must be skipped, including default: %v", row)
							}
							if token := continuationBytesForTest(t, row.GetContinuation()); len(token) != 0 {
								t.Fatalf("skipped singleton is END, not a resumable page: %x", token)
							}
							again, err := cursor.OnNext(ctx)
							if err != nil || again.HasNext() || !again.GetContinuation().IsEnd() {
								t.Fatalf("skipped singleton must remain exhausted: %v, %v", again, err)
							}
							return
						}
						want := int64(11)
						if shape == "empty" {
							want = 99
						}
						if kind != "direct" {
							want = 42
						}
						if !row.HasNext() {
							t.Fatalf("missing scalar row: %v", row)
						}
						if got, _ := row.GetValue().Positional.Get(0); got != want {
							t.Fatalf("scalar = %v, want %d", got, want)
						}
						token := continuationBytesForTest(t, row.GetContinuation())
						if len(token) == 0 || row.GetContinuation().IsEnd() {
							t.Fatal("scalar row must carry a consumed continuation")
						}
						end, err := cursor.OnNext(ctx)
						wantReason := recordlayer.SourceExhausted
						if cap == 1 {
							wantReason = recordlayer.ReturnLimitReached
						}
						if err != nil || end.HasNext() || end.GetNoNextReason() != wantReason {
							t.Fatalf("strict stop = %v, %v; want %v", end, err, wantReason)
						}
						if cap == 1 && (end.GetContinuation().IsEnd() || !bytes.Equal(token, continuationBytesForTest(t, end.GetContinuation()))) {
							t.Fatal("capped strict stop lost consumed row token")
						}
						resumed, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), token, props)
						if err != nil {
							t.Fatal(err)
						}
						defer resumed.Close()
						end, err = resumed.OnNext(ctx)
						if err != nil || end.HasNext() || end.GetNoNextReason() != recordlayer.SourceExhausted {
							t.Fatalf("consumed strict resume must exhaust: %v, %v", end, err)
						}
					})
				}
			}
		}
	}
}

func TestFirstOrDefault_EmptyRecordRequestMatchesJava(t *testing.T) {
	t.Parallel()
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil))
	first := mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlan(inner, nil))
	plan := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NotNullLong},
	}, first))
	// Java forwards skip to the child BEFORE first/default. The skipped child
	// is empty, so the default still flows through the transparent projection.
	ctx := context.Background()
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil,
		recordlayer.DefaultExecuteProperties().WithSkip(1).WithReturnedRowLimit(1))
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	row, err := cursor.OnNext(ctx)
	if err != nil || !row.HasNext() {
		t.Fatalf("Java first/default must still emit: %v, %v", row, err)
	}
	if got, _ := row.GetValue().Positional.Get(0); got != int64(42) {
		t.Fatalf("projected default = %v, want 42", got)
	}
}

func TestDefaultEvaluationContext(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"first", "strict", "all"} {
		for _, shape := range []string{"parameter", "correlation", "constructor", "record_correlation", "clock"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				ec := EmptyEvaluationContext().WithParams([]any{int64(99), int64(88)}).WithStatementTime(stamp)
				var fallback values.Value = &values.ParameterValue{Ordinal: 1, Typ: values.NullableLong}
				want := []any{int64(99)}
				switch shape {
				case "correlation":
					id := values.NamedCorrelationIdentifier("outer_default")
					fallback = mustExecutorConstruct(values.NewQuantifiedObjectValue(id, values.NullableLong))
					ec = ec.WithBinding(id, int64(99))
				case "constructor":
					fallback = values.NewRecordConstructorValue(
						values.RecordConstructorField{Name: "D", Value: fallback},
						values.RecordConstructorField{Name: "D", Value: &values.ParameterValue{Ordinal: 2, Typ: values.NullableLong}})
					want = []any{int64(99), int64(88)}
				case "record_correlation":
					rt := exactTestRowType(values.Field{Name: "D", FieldType: values.NullableLong})
					id := values.NamedCorrelationIdentifier("outer_record_default")
					fallback = mustExecutorConstruct(values.NewQuantifiedObjectValue(id, rt))
					ec = ec.WithBinding(id, &PositionalRow{Type: rt, Slots: []any{int64(99)}})
				case "clock":
					fallback = values.NewScalarFunctionValue("CURRENT_TIMESTAMP", values.NullableTimestamp)
					want = []any{"2026-01-02 03:04:05"}
				}
				inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
					Value: []any{}, Typ: values.NewArrayType(false, fallback.Type()),
				}))
				plan := defaultContextPlan(t, kind, inner, fallback)
				cur, err := ExecutePlan(ctx, plan, nil, ec, nil, recordlayer.DefaultExecuteProperties())
				if err != nil {
					t.Fatal(err)
				}
				defer cur.Close()
				row, err := cur.OnNext(ctx)
				if err != nil || !row.HasNext() {
					t.Fatalf("default row: %v, %v", row, err)
				}
				if got := row.GetValue().Positional.Slots; !reflect.DeepEqual(got, want) {
					t.Fatalf("default slots = %v, want %v", got, want)
				}
				if rt, record := plan.GetResultType().(*values.RecordType); record && !row.GetValue().Positional.Type.Equals(rt) {
					t.Fatalf("default carrier %v, want %v", row.GetValue().Positional.Type, rt)
				}
				cont, err := row.GetContinuation().ToBytes()
				if err != nil || len(cont) == 0 {
					t.Fatalf("default continuation %x, %v", cont, err)
				}
				resumed, err := ExecutePlan(ctx, plan, nil, ec, cont, recordlayer.DefaultExecuteProperties())
				if err != nil {
					t.Fatal(err)
				}
				defer resumed.Close()
				end, err := resumed.OnNext(ctx)
				if err != nil || end.HasNext() || end.GetNoNextReason() != recordlayer.SourceExhausted {
					t.Fatalf("default replay on consumed resume: %v, %v", end, err)
				}
			})
		}
	}
}

func defaultContextPlan(t testing.TB, kind string, inner plans.RecordQueryPlan, fallback values.Value) plans.RecordQueryPlan {
	t.Helper()
	switch kind {
	case "first":
		return mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlan(inner, fallback))
	case "strict":
		return mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlanStrict(inner, fallback))
	default:
		return mustExecutorConstruct(plans.NewRecordQueryDefaultOnEmptyPlan(inner, fallback))
	}
}

func TestDefaultEvaluationLazyError(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"first", "strict", "all"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%v", kind, empty), func(t *testing.T) {
				t.Parallel()
				input := []any{int64(11)}
				if empty {
					input = nil
				}
				inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
					Value: input, Typ: values.NewArrayType(false, values.NullableLong),
				}))
				fallback := mustExecutorConstruct(values.NewQuantifiedObjectValue(
					values.NamedCorrelationIdentifier("unbound_default"), values.NullableLong))
				plan := defaultContextPlan(t, kind, inner, fallback)
				cur, err := ExecutePlan(context.Background(), plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				var row recordlayer.RecordCursorResult[QueryResult]
				if err == nil {
					defer cur.Close()
					row, err = cur.OnNext(context.Background())
				}
				if empty {
					var resolution *values.ResolutionError
					if !errors.As(err, &resolution) || resolution.ErrorCode != values.UnboundCorrelation {
						t.Fatalf("empty default error %v, want UnboundCorrelation", err)
					}
				} else if err != nil || !row.HasNext() || row.GetValue().Positional.Slots[0] != int64(11) {
					t.Fatalf("nonempty child must not evaluate default: %v, %v", row, err)
				}
			})
		}
	}
}

func TestDefaultRecordMaterialization(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"first", "strict", "all"} {
		for _, shape := range []string{"null", "nil_positional", "nil_proto", "all_null", "positional", "proto", "nullable_array_proto", "wrong_proto", "duplicate_proto", "wrong_type", "short", "wide", "map", "empty_record"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				t.Parallel()
				rt := values.NewRecordType("", true, []values.Field{{Name: "D", FieldType: values.NullableLong, Ordinal: 0}})
				source := &PositionalRow{Type: rt, Slots: []any{int64(99)}}
				var datum any = source
				wantAbsent, wantError := false, false
				switch shape {
				case "null":
					datum, wantAbsent = nil, true
				case "nil_positional":
					datum, wantAbsent = (*PositionalRow)(nil), true
				case "nil_proto":
					datum, wantAbsent = (*gen.Order)(nil), true
				case "all_null":
					source.Slots[0] = nil
				case "proto", "nullable_array_proto", "wrong_proto", "duplicate_proto":
					message := &gen.Order{OrderId: proto.Int64(99), Tags: []string{"a"}}
					declared := PositionalTypeForDescriptor(message.ProtoReflect().Descriptor())
					fields := append([]values.Field(nil), declared.Fields...)
					if shape == "nullable_array_proto" {
						fields[3].FieldType = values.WithNullability(fields[3].FieldType, true)
					}
					if shape == "wrong_proto" {
						fields[0].FieldType = values.NullableString
						wantError = true
					}
					if shape == "duplicate_proto" {
						fields[1].Name = fields[0].Name
						wantError = true
					}
					rt = &values.RecordType{RecordName: "NAMED_DEFAULT", Nullable: true, Fields: fields}
					datum = message
				case "wrong_type":
					source.Type = values.NewRecordType("", true, []values.Field{{Name: "D", FieldType: values.NullableString, Ordinal: 0}})
					wantError = true
				case "short":
					source.Slots, wantError = nil, true
				case "wide":
					source.Slots, wantError = []any{int64(99), int64(88)}, true
				case "map":
					datum, wantError = map[string]any{"D": int64(99)}, true
				case "empty_record":
					rt = values.NewRecordType("", true, nil)
					source.Type, source.Slots = rt, nil
				}
				inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{}, Typ: values.NewArrayType(false, rt)}))
				plan := defaultContextPlan(t, kind, inner, &values.ParameterValue{Ordinal: 1, Typ: rt})
				cur, err := ExecutePlan(context.Background(), plan, nil, EmptyEvaluationContext().WithParams([]any{datum}), nil, recordlayer.DefaultExecuteProperties())
				var row recordlayer.RecordCursorResult[QueryResult]
				if err == nil {
					defer cur.Close()
					row, err = cur.OnNext(context.Background())
				}
				if wantError {
					var resolution *values.ResolutionError
					if !errors.As(err, &resolution) || resolution.ErrorCode != values.LayoutRuntimeShape {
						t.Fatalf("bad default shape: %v, want LayoutRuntimeShape", err)
					}
					return
				}
				if err != nil || !row.HasNext() {
					t.Fatalf("record default: %v, %v", row, err)
				}
				pos := row.GetValue().Positional
				if !pos.Type.Equals(rt) || len(pos.Slots) != len(rt.Fields) {
					t.Fatalf("default record carrier %v, want %v", pos, rt)
				}
				whole, absent, err := pos.wholeObjectBinding()
				if err != nil || absent != wantAbsent || (whole == nil) != wantAbsent {
					t.Fatalf("default presence: %v absent=%v err=%v, want absent=%v", whole, absent, err, wantAbsent)
				}
				if shape == "proto" || shape == "nullable_array_proto" || shape == "positional" {
					if pos.Slots[0] != int64(99) {
						t.Fatalf("record default first ordinal %v, want 99", pos.Slots[0])
					}
				}
				if shape == "nullable_array_proto" && !reflect.DeepEqual(pos.Slots[3], []any{"a"}) {
					t.Fatalf("nullable repeated default field = %#v, want [a]", pos.Slots[3])
				}
				if shape == "positional" {
					pos.Slots[0] = int64(1)
					if source.Slots[0] != int64(99) || source.Layout != nil {
						t.Fatal("materialization mutated the bound record")
					}
				}
			})
		}
	}
}

func TestDefaultOnEmptyPreservesNullChild(t *testing.T) {
	t.Parallel()
	rt := values.NewRecordType("", true, []values.Field{{Name: "D", FieldType: values.NullableLong, Ordinal: 0}})
	empty := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{}, Typ: values.NewArrayType(false, rt)}))
	child := mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlan(empty, values.NewNullValue(rt)))
	plan := mustExecutorConstruct(plans.NewRecordQueryDefaultOnEmptyPlan(child, values.NewNullValue(rt)))
	cur, err := ExecutePlan(context.Background(), plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	row, err := cur.OnNext(context.Background())
	if err != nil || !row.HasNext() {
		t.Fatalf("NULL child row: %v, %v", row, err)
	}
	whole, absent, err := row.GetValue().Positional.wholeObjectBinding()
	if err != nil || whole != nil || !absent {
		t.Fatalf("DefaultOnEmpty changed a NULL record into a present all-NULL record: %v, %v, %v", whole, absent, err)
	}
}

func TestDefaultEvaluationNilContext(t *testing.T) {
	t.Parallel()
	fallback := &values.ConstantValue{Value: int64(99), Typ: values.NullableLong}
	inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{}, Typ: values.NewArrayType(false, values.NullableLong)}))
	for _, kind := range []string{"first", "strict", "all"} {
		plan := defaultContextPlan(t, kind, inner, fallback)
		result, err := defaultResultFromValue(plan, fallback, nil)
		if err != nil || len(result.Positional.Slots) != 1 || result.Positional.Slots[0] != int64(99) {
			t.Fatalf("%s nil context: %v, %v", kind, result, err)
		}
	}
}

func FuzzDefaultRecordMaterialization(f *testing.F) {
	for _, seed := range []byte{0, 1, 2, 3, 4, 5, 6} {
		f.Add(seed, int64(99))
	}
	f.Fuzz(func(t *testing.T, shape byte, n int64) {
		t.Parallel()
		rt := values.NewRecordType("", true, []values.Field{{Name: "D", FieldType: values.NullableLong, Ordinal: 0}})
		inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{}, Typ: values.NewArrayType(false, rt)}))
		fallback := &values.ParameterValue{Ordinal: 1, Typ: rt}
		plan := defaultContextPlan(t, "first", inner, fallback)
		var datum any
		var absent, invalid bool
		switch shape % 7 {
		case 0:
			absent = true
		case 1:
			datum, absent = (*PositionalRow)(nil), true
		case 2:
			datum = &PositionalRow{Type: rt, Slots: []any{nil}}
		case 3:
			datum = &PositionalRow{Type: rt, Slots: []any{n}}
		case 4:
			datum, invalid = &PositionalRow{Type: rt, Slots: nil}, true
		case 5:
			datum, invalid = &PositionalRow{Type: rt, Slots: []any{n, n}}, true
		case 6:
			datum, invalid = n, true
		}
		result, err := defaultResultFromValue(plan, fallback, EmptyEvaluationContext().WithParams([]any{datum}))
		if invalid {
			var resolution *values.ResolutionError
			if !errors.As(err, &resolution) || resolution.ErrorCode != values.LayoutRuntimeShape {
				t.Fatalf("shape %d: %v, want LayoutRuntimeShape", shape%7, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		whole, gotAbsent, err := result.Positional.wholeObjectBinding()
		if err != nil || gotAbsent != absent || (whole == nil) != absent || !result.Positional.Type.Equals(rt) {
			t.Fatalf("shape %d: %v, absent=%v, error=%v", shape%7, whole, gotAbsent, err)
		}
		if shape%7 == 3 && result.Positional.Slots[0] != n {
			t.Fatalf("default value %v, want %d", result.Positional.Slots[0], n)
		}
	})
}
