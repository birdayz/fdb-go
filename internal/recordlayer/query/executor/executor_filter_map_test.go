package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func TestExecuteValues_SingleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	plan := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(cols))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	declared, ok := plan.GetResultType().(*values.RecordType)
	if !ok {
		t.Fatalf("VALUES declared result type = %T, want record", plan.GetResultType())
	}
	emitted := results[0].Positional
	if emitted == nil || emitted.Type == nil {
		t.Fatalf("VALUES emitted row without a positional type: %#v", results[0])
	}
	if !emitted.Type.Equals(declared) {
		t.Fatalf("VALUES emitted type = %v, want its declared type %v", emitted.Type, declared)
	}
	if len(emitted.Type.Fields) != 1 || emitted.Type.Fields[0].FieldType == nil ||
		emitted.Type.Fields[0].FieldType.Code() != values.TypeCodeInt ||
		emitted.Type.Fields[0].FieldType.IsNullable() {
		t.Fatalf("VALUES emitted field type = %v, want exact INT NOT NULL", emitted.Type.Fields)
	}
	if emitted.Type.Fields[0].Name == "constant" {
		t.Fatal("VALUES re-derived the runtime label from ConstantValue.Name instead of using its declared output name")
	}

	row, ok := rowMapOK(results[0])
	if !ok {
		t.Fatalf("datum = %T, want map[string]any", results[0].Positional)
	}
	declaredName := declared.Fields[0].Name
	if row[declaredName] != int64(42) {
		t.Errorf("row[%q] = %v, want 42", declaredName, row[declaredName])
	}
}

func TestResultFromValue_RecordConstructorUsesDeclaredExactType(t *testing.T) {
	t.Parallel()
	rc := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "N",
			Value: &values.ConstantValue{Value: int64(7), Typ: values.NullableLong},
		},
		values.RecordConstructorField{
			Name:  "S",
			Value: &values.ConstantValue{Value: "x", Typ: values.TypeString},
		},
	)
	result, err := resultFromValue(rc, EmptyEvaluationContext().RowContext())
	if err != nil {
		t.Fatalf("resultFromValue: %v", err)
	}
	declared := rc.Type().(*values.RecordType)
	if result.Positional == nil || result.Positional.Type == nil ||
		!result.Positional.Type.Equals(declared) {
		t.Fatalf("record-constructor emitted type = %v, want declared %v", result.Positional, declared)
	}
	for i, field := range result.Positional.Type.Fields {
		if field.FieldType == nil || field.FieldType.Code() == values.TypeCodeUnknown {
			t.Fatalf("record-constructor field %d lost its exact type: %v", i, field)
		}
	}
}

func TestExecuteValues_EmptyColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (empty row)", len(results))
	}
}

func TestExecuteFilter_OverValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(cols))
	filterPlan := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	))

	cursor, err := ExecutePlan(ctx, filterPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (filter=TRUE passes all)", len(results))
	}
}

func TestExecuteFilter_RejectsAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(cols))
	filterPlan := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		inner,
	))

	cursor, err := ExecutePlan(ctx, filterPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (filter=FALSE rejects all)", len(results))
	}
}

func TestExecuteProjection_FieldExtraction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(100), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	projPlan := mustExecutorConstruct(newProjectionMapOverForTest(
		[]values.Value{
			&values.ConstantValue{Value: "projected", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		},
		inner,
	))

	cursor, err := ExecutePlan(ctx, projPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	row, _ := rowMapOK(results[0])
	if row["'projected'"] != "projected" {
		t.Errorf("projection result = %v, want 'projected'", row["'projected'"])
	}
}

func TestProjection_MultiColumnFieldValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rowType := exactTestRowType(
		values.Field{Name: "A", FieldType: values.NullableLong},
		values.Field{Name: "B", FieldType: values.TypeString},
		values.Field{Name: "C", FieldType: values.NotNullBoolean},
	)
	inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{
			map[string]any{"A": int64(1), "B": "hello", "C": true},
		},
		Typ: values.NewArrayType(false, rowType),
	}))
	root := inner.GetResultValue()

	projected := mustExecutorConstruct(newProjectionMapOverForTest(
		[]values.Value{
			mustTestFieldOrdinal(t, root, 0),
			mustTestFieldOrdinal(t, root, 1),
		},
		inner,
	))

	cursor, err := ExecutePlan(ctx, projected, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	datum, _ := rowMapOK(results[0])
	if datum["A"] != int64(1) {
		t.Errorf("A = %v, want 1", datum["A"])
	}
	if datum["B"] != "hello" {
		t.Errorf("B = %v, want 'hello'", datum["B"])
	}
	if _, hasC := datum["C"]; hasC {
		t.Error("C should not be in projected result")
	}
}

// --- shared ProjectionColumnName contract tests ---

func TestProjectionColumnName_FieldValue(t *testing.T) {
	t.Parallel()
	fv := mustNamedTestField(t, "MY_COL", values.TypeString)
	if got := values.ProjectionColumnName(fv); got != "MY_COL" {
		t.Fatalf("expected MY_COL, got %s", got)
	}
}

func TestProjectionColumnName_NonFieldValue(t *testing.T) {
	t.Parallel()
	cv := &values.ConstantValue{Value: int64(42), Typ: values.NullableLong}
	want := strings.ToUpper(values.ExplainValue(cv))
	if got := values.ProjectionColumnName(cv); got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestExecute_CompositeFilterSortLimitProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(99), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))

	filtered := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	))

	sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(filtered, nil))

	limited := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(sorted, 10, 0))

	projected := mustExecutorConstruct(newProjectionMapOverForTest(
		[]values.Value{
			&values.ConstantValue{Value: "result", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		},
		limited,
	))

	cursor, err := ExecutePlan(ctx, projected, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	row, _ := rowMapOK(results[0])
	if row["'result'"] != "result" {
		t.Errorf("composite pipeline result = %v, want 'result'", row["'result'"])
	}
}

func TestExecuteExplode_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(
		&values.ConstantValue{
			Value: []any{int64(1), int64(2), int64(3)},
			Typ:   values.NewArrayType(false, values.NullableLong),
		},
	))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, want := range []int64{1, 2, 3} {
		if rowScalar(results[i]) != want {
			t.Errorf("results[%d] = %v, want %d", i, rowScalar(results[i]), want)
		}
	}
}

func TestExecuteExplode_Nil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: nil,
		Typ:   values.NewArrayType(true, values.NullableLong),
	}))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (nil collection)", len(results))
	}
}

func TestExecuteExplode_ProtoDeclaredShape(t *testing.T) {
	t.Parallel()
	for _, nullable := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("duplicate=%v/nullable=%v", duplicate, nullable), func(t *testing.T) {
				t.Parallel()
				message := &demopb.Order{OrderId: proto.Int64(99)}
				original := PositionalTypeForDescriptor(message.ProtoReflect().Descriptor())
				fields := append([]values.Field(nil), original.Fields...)
				if duplicate {
					fields[1].Name = fields[0].Name
				}
				declared := &values.RecordType{RecordName: "OTHER_PROVENANCE", Nullable: nullable, Fields: fields}
				plan := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
					Value: []any{message}, Typ: values.NewArrayType(false, declared),
				}))
				cur, err := ExecutePlan(context.Background(), plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				var row recordlayer.RecordCursorResult[QueryResult]
				if err == nil {
					defer cur.Close()
					row, err = cur.OnNext(context.Background())
				}
				if duplicate {
					var resolution *values.ResolutionError
					if !errors.As(err, &resolution) || resolution.ErrorCode != values.LayoutCarrierMismatch {
						t.Fatalf("duplicate declared fields: %v, want LayoutCarrierMismatch", err)
					}
				} else if err != nil || !row.HasNext() || row.GetValue().Positional.Slots[0] != int64(99) {
					t.Fatalf("valid structural record with different provenance: %v, %v", row, err)
				}
			})
		}
	}
}

func TestExecuteTableFunction_StreamValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := mustExecutorConstruct(plans.NewRecordQueryTableFunctionPlan(
		&values.ConstantValue{
			Value: []any{int64(10), int64(20)},
			Typ:   values.NewArrayType(false, values.NullableLong),
		},
	))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if rowScalar(results[0]) != int64(10) || rowScalar(results[1]) != int64(20) {
		t.Errorf("results = %v, %v, want 10, 20", rowScalar(results[0]), rowScalar(results[1]))
	}
}

func TestExecuteTableFunction_Nil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := mustExecutorConstruct(plans.NewRecordQueryTableFunctionPlan(&values.ConstantValue{
		Value: nil,
		Typ:   values.NewArrayType(true, values.NullableLong),
	}))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}
