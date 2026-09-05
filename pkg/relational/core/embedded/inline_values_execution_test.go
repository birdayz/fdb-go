package embedded

import (
	"context"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

func executeInlineValuesPlan(t testing.TB, plan plans.RecordQueryPlan) []executor.QueryResult {
	t.Helper()
	cascades.FinalizePlan(plan)
	cursor, err := executor.ExecutePlan(context.Background(), plan, nil,
		executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("execute inline VALUES plan %s: %v", plan.Explain(), err)
	}
	defer cursor.Close()
	rows, err := executor.CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatalf("collect inline VALUES plan %s: %v", plan.Explain(), err)
	}
	return rows
}

func TestInlineValuesPhysicalLeafEmitsItsExactPublishedRow(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalForTest(
		`SELECT "id" FROM VALUES (1), (2) AS "values" ("id") ORDER BY "id"`,
		`CREATE TABLE anchor (id BIGINT, PRIMARY KEY (id))`, nil)
	if err != nil {
		t.Fatalf("plan inline VALUES: %v", err)
	}
	var explode *plans.RecordQueryExplodePlan
	var walk func(plans.RecordQueryPlan)
	walk = func(candidate plans.RecordQueryPlan) {
		if candidate == nil || explode != nil {
			return
		}
		if found, ok := candidate.(*plans.RecordQueryExplodePlan); ok {
			explode = found
			return
		}
		for _, child := range candidate.GetChildren() {
			walk(child)
		}
	}
	walk(plan)
	if explode == nil {
		t.Fatalf("physical plan contains no Explode: %s", plan.Explain())
	}
	layout, err := explode.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("explode layout: %v", err)
	}
	if !explode.GetElementType().Equals(layout.Carrier().FlowedType()) {
		t.Fatalf("explode element type = %v, layout carrier = %v",
			explode.GetElementType(), layout.Carrier().FlowedType())
	}
	// Production finalizes every cache-miss plan before execution. In
	// particular this stamps the inline row constructors with the plan's
	// synthetic protobuf descriptors; the direct harness otherwise leaves them
	// as name-keyed maps and would miss representation-only type drift.
	cascades.FinalizePlan(plan)
	cursor, err := executor.ExecutePlan(context.Background(), explode, nil,
		executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("execute explode: %v", err)
	}
	defer cursor.Close()
	rows, err := executor.CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatalf("collect explode with element=%v carrier=%v: %v",
			explode.GetElementType(), layout.Carrier().FlowedType(), err)
	}
	if len(rows) != 2 {
		t.Fatalf("explode rows = %d, want 2", len(rows))
	}
	fullCursor, err := executor.ExecutePlan(context.Background(), plan, nil,
		executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("execute full inline VALUES plan %s: %v", plan.Explain(), err)
	}
	defer fullCursor.Close()
	fullRows, err := executor.CollectAll(context.Background(), fullCursor)
	if err != nil {
		t.Fatalf("collect full inline VALUES plan %s: %v", plan.Explain(), err)
	}
	if len(fullRows) != 2 {
		t.Fatalf("full plan rows = %d, want 2", len(fullRows))
	}
}

func TestInlineValuesNestedDefinitionsFinalizeAndExecute(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalForTest(
		`SELECT B, C, W.X, W.Y, W.Z `+
			`FROM VALUES (1, 2.0, (3, 4, 'foo')), (10, 90.2, (5, 6.0, 'bar')) `+
			`AS A(B, C, W(X, Y, Z))`,
		`CREATE TABLE anchor (id BIGINT, PRIMARY KEY (id))`, nil)
	if err != nil {
		t.Fatalf("plan nested inline VALUES: %v", err)
	}

	var explode *plans.RecordQueryExplodePlan
	plans.Walk(plan, func(candidate plans.RecordQueryPlan) bool {
		if found, ok := candidate.(*plans.RecordQueryExplodePlan); ok {
			explode = found
		}
		return true
	})
	if explode == nil {
		t.Fatalf("nested inline plan contains no Explode: %s", plan.Explain())
	}
	element, ok := explode.GetElementType().(*values.RecordType)
	if !ok || len(element.Fields) != 3 {
		t.Fatalf("Explode element = %T %v, want exact B/C/W record", explode.GetElementType(), explode.GetElementType())
	}
	nested, ok := element.Fields[2].FieldType.(*values.RecordType)
	if !ok || nested.RecordName != "" || len(nested.Fields) != 3 {
		t.Fatalf("Explode W = %T %v, want anonymous RECORD<X,Y,Z>", element.Fields[2].FieldType, element.Fields[2].FieldType)
	}
	if nested.Fields[1].FieldType.Code() != values.TypeCodeDouble {
		t.Fatalf("Explode W.Y = %v, want promoted DOUBLE", nested.Fields[1].FieldType)
	}

	rows := executeInlineValuesPlan(t, plan)
	want := [][]any{
		{int64(1), float64(2), int64(3), float64(4), "foo"},
		{int64(10), float64(90.2), int64(5), float64(6), "bar"},
	}
	if len(rows) != len(want) {
		t.Fatalf("nested inline rows = %d, want %d\nplan: %s", len(rows), len(want), plan.Explain())
	}
	for i := range rows {
		if rows[i].Positional == nil || !reflect.DeepEqual(rows[i].Positional.Slots, want[i]) {
			t.Fatalf("nested inline row %d = %#v, want %#v\nplan: %s",
				i, rows[i].Positional, want[i], plan.Explain())
		}
	}
}

func TestProjectionlessExplodeColumnsUseFrozenExactRecordType(t *testing.T) {
	t.Parallel()
	nested := values.NewRecordType("NESTED", true, []values.Field{{
		Name: "N", FieldType: values.NotNullInt,
	}})
	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "quotedCase", FieldType: values.NullableLong},
		{Name: "ARR", FieldType: values.NewArrayType(false, values.NotNullString)},
		{Name: "NEST", FieldType: nested},
	})
	collection := &values.ConstantValue{
		Typ:   values.NewArrayType(false, rowType),
		Value: []any{},
	}
	explode, err := plans.NewRecordQueryExplodePlan(collection)
	if err != nil {
		t.Fatalf("construct projection-less record Explode: %v", err)
	}
	want := []executor.ColumnDef{
		{Name: "quotedCase", TypeName: "BIGINT", Nullable: api.ColumnNullable},
		{Name: "ARR", TypeName: "STRING", Nullable: api.ColumnNoNulls},
		{Name: "NEST", TypeName: "STRUCT", Nullable: api.ColumnNullable},
	}
	metadata := buildTestMetaData(t)
	if got := deriveColumnsFromPlan(explode, metadata); !reflect.DeepEqual(got, want) {
		t.Fatalf("projection-less Explode columns = %#v, want %#v", got, want)
	}

	// The plan froze this exact row at construction. Mutating the ordinary
	// collection Type graph afterwards must not rename/retype the published
	// metadata; this catches a regression to collection.Type() as authority.
	rowType.Fields[0].Name = "DRIFT"
	rowType.Fields[0].FieldType = values.NotNullString
	collection.Typ = values.NewArrayType(false, values.NewRecordType("", false, []values.Field{{
		Name: "FOREIGN", FieldType: values.NotNullDouble,
	}}))
	if got := deriveColumnsFromPlan(explode, metadata); !reflect.DeepEqual(got, want) {
		t.Fatalf("mutated collection changed frozen Explode columns = %#v, want %#v", got, want)
	}

	scalar, err := plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Typ: values.NewArrayType(false, values.NotNullLong), Value: []any{},
	})
	if err != nil {
		t.Fatalf("construct scalar Explode control: %v", err)
	}
	if got := deriveColumnsFromPlan(scalar, metadata); got != nil {
		t.Fatalf("scalar Explode invented projection-less record columns: %#v", got)
	}

	ordinalRow := values.NewRecordType("", false, []values.Field{{
		Name: "V", FieldType: values.NotNullLong,
	}})
	ordinal, err := plans.NewRecordQueryExplodePlanWithOrdinality(&values.ConstantValue{
		Typ: values.NewArrayType(false, ordinalRow), Value: []any{},
	}, true)
	if err != nil {
		t.Fatalf("construct ordinal Explode control: %v", err)
	}
	if got := deriveColumnsFromPlan(ordinal, metadata); got != nil {
		t.Fatalf("ordinality box was flattened as its record element: %#v", got)
	}
}

func TestInlineValuesLateralExplodeKeepsExactOuterBinding(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalForTest(
		`SELECT "values"."id", "val", "at" `+
			`FROM VALUES (1, [101]), (2, [201, 202, 203]) AS "values" ("id", "arr"), `+
			`"values"."arr" AS "val" AT "at"`,
		`CREATE TABLE anchor (id BIGINT, PRIMARY KEY (id))`, nil)
	if err != nil {
		t.Fatalf("plan lateral inline VALUES: %v", err)
	}
	var flatMap *plans.RecordQueryFlatMapPlan
	var outerExplode *plans.RecordQueryExplodePlan
	var innerExplode *plans.RecordQueryExplodePlan
	plans.Walk(plan, func(candidate plans.RecordQueryPlan) bool {
		switch typed := candidate.(type) {
		case *plans.RecordQueryFlatMapPlan:
			flatMap = typed
		case *plans.RecordQueryExplodePlan:
			if typed.IsWithOrdinality() {
				innerExplode = typed
			} else if arrayType, ok := typed.GetCollectionValue().Type().(*values.ArrayType); ok &&
				arrayType.ElementType.Code() == values.TypeCodeRecord {
				outerExplode = typed
			}
		}
		return true
	})
	if flatMap == nil || outerExplode == nil || innerExplode == nil {
		t.Fatalf("lateral plan = %s, want FlatMap(record Explode, ordinal Explode)", plan.Explain())
	}
	correlated := values.GetCorrelatedToOfValue(innerExplode.GetCollectionValue())
	if _, ok := correlated[flatMap.GetOuterAlias()]; !ok {
		t.Fatalf("FlatMap outer binding %q does not own inner collection correlations %v\nplan: %s",
			flatMap.GetOuterAlias().Name(), correlated, plan.Explain())
	}
	foundInJoin := false
	plans.Walk(plan, func(candidate plans.RecordQueryPlan) bool {
		if _, ok := candidate.(*plans.RecordQueryInJoinPlan); ok {
			foundInJoin = true
		}
		return true
	})
	if foundInJoin {
		t.Fatalf("record-valued relation source was lowered as scalar InJoin: %s", plan.Explain())
	}
	rows := executeInlineValuesPlan(t, plan)
	if len(rows) != 4 {
		t.Fatalf("lateral inline rows = %d, want 4\nplan: %s", len(rows), plan.Explain())
	}
}
