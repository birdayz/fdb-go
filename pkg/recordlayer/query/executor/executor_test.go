package executor

import (
	"context"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type unsupportedTestPlan struct{}

func (p *unsupportedTestPlan) GetResultType() values.Type           { return values.UnknownType }
func (p *unsupportedTestPlan) GetChildren() []plans.RecordQueryPlan { return nil }
func (p *unsupportedTestPlan) EqualsWithoutChildren(plans.RecordQueryPlan) bool {
	return false
}
func (p *unsupportedTestPlan) HashCodeWithoutChildren() uint64 { return 0 }
func (p *unsupportedTestPlan) Explain() string                 { return "unsupported" }

func TestExecuteValues_SingleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	plan := plans.NewRecordQueryValuesPlan(cols)
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

	row, ok := results[0].Datum.(map[string]any)
	if !ok {
		t.Fatalf("datum = %T, want map[string]any", results[0].Datum)
	}
	if row["constant"] != int64(42) {
		t.Errorf("row['constant'] = %v, want 42", row["constant"])
	}
}

func TestExecuteValues_EmptyColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryValuesPlan(nil)
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
	inner := plans.NewRecordQueryValuesPlan(cols)
	filterPlan := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	)

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
	inner := plans.NewRecordQueryValuesPlan(cols)
	filterPlan := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		inner,
	)

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

func TestExecuteLimit_CapsResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := plans.NewRecordQueryValuesPlan(cols)
	limitPlan := plans.NewRecordQueryLimitPlan(inner, 0, 0)

	cursor, err := ExecutePlan(ctx, limitPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (limit=0)", len(results))
	}
}

func TestExecuteDistinct_DedupsValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := plans.NewRecordQueryValuesPlan(cols)
	distinctPlan := plans.NewRecordQueryDistinctPlan(inner)

	cursor, err := ExecutePlan(ctx, distinctPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
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
}

func TestExecuteProjection_FieldExtraction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(100), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	projPlan := plans.NewRecordQueryProjectionPlan(
		[]values.Value{
			&values.ConstantValue{Value: "projected", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		},
		inner,
	)

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

	row := results[0].Datum.(map[string]any)
	if row["constant"] != "projected" {
		t.Errorf("projection result = %v, want 'projected'", row["constant"])
	}
}

func TestExecuteSort_OverValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	sortPlan := plans.NewRecordQuerySortPlan(nil, inner)

	cursor, err := ExecutePlan(ctx, sortPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
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
}

func TestExecuteUnion_ConcatenatesInners(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	b := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(2), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	union := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{a, b})

	cursor, err := ExecutePlan(ctx, union, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (one from each inner)", len(results))
	}
}

func TestExecuteUnion_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	union := plans.NewRecordQueryUnionPlan(nil)

	cursor, err := ExecutePlan(ctx, union, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestExecuteIntersection_CommonRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	b := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	intersection := plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{a, b}, nil)

	cursor, err := ExecutePlan(ctx, intersection, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (common row)", len(results))
	}
}

func TestExecuteIntersection_NoCommonRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	b := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(2), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	intersection := plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{a, b}, nil)

	cursor, err := ExecutePlan(ctx, intersection, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (no common rows)", len(results))
	}
}

func TestExecuteUnsupportedPlan_ReturnsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := &unsupportedTestPlan{}
	_, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err == nil {
		t.Fatal("expected error for unsupported plan type")
	}
}

func TestCollectAll_EmptyCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.Empty[QueryResult]()
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestEvaluationContext_WithBinding(t *testing.T) {
	t.Parallel()

	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("q1")
	ec2 := ec.WithBinding(id, map[string]any{"x": 42})

	if _, ok := ec.GetBinding(id); ok {
		t.Fatal("original context should not have binding")
	}
	v, ok := ec2.GetBinding(id)
	if !ok {
		t.Fatal("derived context should have binding")
	}
	m, ok := v.(map[string]any)
	if !ok || m["x"] != 42 {
		t.Fatalf("binding value = %v, want map[x:42]", v)
	}
}

func TestQueryResult_FromStoredRecord_NilSafe(t *testing.T) {
	t.Parallel()

	m := protoToMap(nil)
	if m != nil {
		t.Fatalf("protoToMap(nil) = %v, want nil", m)
	}
}

func TestCompareAny_Integers(t *testing.T) {
	t.Parallel()

	if compareAny(int64(1), int64(2)) >= 0 {
		t.Fatal("1 should be < 2")
	}
	if compareAny(int64(2), int64(1)) <= 0 {
		t.Fatal("2 should be > 1")
	}
	if compareAny(int64(1), int64(1)) != 0 {
		t.Fatal("1 should equal 1")
	}
}

func TestCompareAny_Strings(t *testing.T) {
	t.Parallel()

	if compareAny("a", "b") >= 0 {
		t.Fatal("'a' should be < 'b'")
	}
	if compareAny("b", "a") <= 0 {
		t.Fatal("'b' should be > 'a'")
	}
}

func TestCompareAny_NilHandling(t *testing.T) {
	t.Parallel()

	if compareAny(nil, nil) != 0 {
		t.Fatal("nil should equal nil")
	}
	if compareAny(nil, int64(1)) >= 0 {
		t.Fatal("nil should sort before non-nil")
	}
	if compareAny(int64(1), nil) <= 0 {
		t.Fatal("non-nil should sort after nil")
	}
}

func TestSortByKeys(t *testing.T) {
	t.Parallel()

	items := []QueryResult{
		{Datum: map[string]any{"NAME": "charlie", "AGE": int64(30)}},
		{Datum: map[string]any{"NAME": "alice", "AGE": int64(25)}},
		{Datum: map[string]any{"NAME": "bob", "AGE": int64(35)}},
	}

	sortByKeys(items, []string{"name"}, nil)

	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Datum.(map[string]any)["NAME"].(string)
	}
	if names[0] != "alice" || names[1] != "bob" || names[2] != "charlie" {
		t.Fatalf("sort by name = %v, want [alice bob charlie]", names)
	}
}

func TestExecute_CompositeFilterSortLimitProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(99), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	filtered := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	)

	sorted := plans.NewRecordQuerySortPlan(nil, filtered)

	limited := plans.NewRecordQueryLimitPlan(sorted, 10, 0)

	projected := plans.NewRecordQueryProjectionPlan(
		[]values.Value{
			&values.ConstantValue{Value: "result", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		},
		limited,
	)

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

	row := results[0].Datum.(map[string]any)
	if row["constant"] != "result" {
		t.Errorf("composite pipeline result = %v, want 'result'", row["constant"])
	}
}

func TestProjection_MultiColumnFieldValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{
			map[string]any{"A": int64(1), "B": "hello", "C": true},
		},
		Typ: values.UnknownType,
	})

	projected := plans.NewRecordQueryProjectionPlan(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
			&values.FieldValue{Field: "B", Typ: values.UnknownType},
		},
		inner,
	)

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
	datum := results[0].Datum.(map[string]any)
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

func TestScanComparisonsToTupleRange_Empty(t *testing.T) {
	t.Parallel()
	r, err := scanComparisonsToTupleRange(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Low != nil || r.High != nil {
		t.Fatalf("empty comparisons should give ALL range, got low=%v high=%v", r.Low, r.High)
	}
}

func TestScanComparisonsToTupleRange_EqualityOnly(t *testing.T) {
	t.Parallel()
	eq1 := predicates.EmptyComparisonRange()
	res := eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("alice")})
	if !res.Ok {
		t.Fatal("merge failed")
	}

	eq2 := predicates.EmptyComparisonRange()
	res2 := eq2.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(42))})
	if !res2.Ok {
		t.Fatal("merge2 failed")
	}

	r, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{res.Range, res2.Range}, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantPrefix := tuple.Tuple{"alice", int64(42)}
	if len(r.Low) != len(wantPrefix) {
		t.Fatalf("low=%v, want prefix %v", r.Low, wantPrefix)
	}
	for i, v := range wantPrefix {
		if r.Low[i] != v {
			t.Fatalf("low[%d]=%v, want %v", i, r.Low[i], v)
		}
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("lowEndpoint=%v, want RangeInclusive", r.LowEndpoint)
	}
}

func TestScanComparisonsToTupleRange_EqualityPlusInequality(t *testing.T) {
	t.Parallel()

	eq := predicates.EmptyComparisonRange()
	res := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("users")})
	if !res.Ok {
		t.Fatal("merge eq failed")
	}

	ineq := predicates.EmptyComparisonRange()
	gt := &predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(10))}
	res2 := ineq.Merge(gt)
	if !res2.Ok {
		t.Fatal("merge gt failed")
	}
	lt := &predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: values.LiteralValue(int64(100))}
	res3 := res2.Range.Merge(lt)
	if !res3.Ok {
		t.Fatal("merge lt failed")
	}

	r, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{res.Range, res3.Range}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(r.Low) != 2 || r.Low[0] != "users" || r.Low[1] != int64(10) {
		t.Fatalf("low=%v, want [users, 10]", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("lowEndpoint=%v, want RangeExclusive", r.LowEndpoint)
	}
	if len(r.High) != 2 || r.High[0] != "users" || r.High[1] != int64(100) {
		t.Fatalf("high=%v, want [users, 100]", r.High)
	}
	if r.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("highEndpoint=%v, want RangeExclusive", r.HighEndpoint)
	}
}

func TestScanComparisonsToTupleRange_InequalityOnly(t *testing.T) {
	t.Parallel()

	ineq := predicates.EmptyComparisonRange()
	gte := &predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(5))}
	res := ineq.Merge(gte)
	if !res.Ok {
		t.Fatal("merge gte failed")
	}

	r, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{res.Range}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(r.Low) != 1 || r.Low[0] != int64(5) {
		t.Fatalf("low=%v, want [5]", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("lowEndpoint=%v, want RangeInclusive (>=)", r.LowEndpoint)
	}
	if r.High != nil {
		t.Fatalf("high=%v, want nil (no upper bound)", r.High)
	}
	if r.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("highEndpoint=%v, want TreeEnd", r.HighEndpoint)
	}
}

func TestScanComparisonsToTupleRange_EmptyRangeStops(t *testing.T) {
	t.Parallel()

	eq := predicates.EmptyComparisonRange()
	res := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("x")})
	if !res.Ok {
		t.Fatal("merge failed")
	}

	empty := predicates.EmptyComparisonRange()

	r, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{res.Range, empty}, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantPrefix := tuple.Tuple{"x"}
	if len(r.Low) != 1 || r.Low[0] != "x" {
		t.Fatalf("low=%v, want prefix %v", r.Low, wantPrefix)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("lowEndpoint=%v, want RangeInclusive (prefix scan)", r.LowEndpoint)
	}
}

func TestScanComparisonsToTupleRange_LessThanOnly(t *testing.T) {
	t.Parallel()

	ineq := predicates.EmptyComparisonRange()
	lt := &predicates.Comparison{Type: predicates.ComparisonLessThanOrEq, Operand: values.LiteralValue(int64(50))}
	res := ineq.Merge(lt)
	if !res.Ok {
		t.Fatal("merge lte failed")
	}

	r, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{res.Range}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if r.Low != nil {
		t.Fatalf("low=%v, want nil (no lower bound but has exclusion mark)", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("lowEndpoint=%v, want RangeExclusive (Java convention for LT-only)", r.LowEndpoint)
	}
	if len(r.High) != 1 || r.High[0] != int64(50) {
		t.Fatalf("high=%v, want [50]", r.High)
	}
	if r.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("highEndpoint=%v, want RangeInclusive (<=)", r.HighEndpoint)
	}
}

func TestParameterBinding_ScanComparison(t *testing.T) {
	t.Parallel()

	param1 := values.NewParameterValue(1)
	cr := predicates.EmptyComparisonRange()
	res := cr.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: param1})
	if !res.Ok {
		t.Fatal("merge failed")
	}

	binder := EmptyEvaluationContext().WithParams([]any{int64(42)})
	r, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{res.Range}, binder)
	if err != nil {
		t.Fatal(err)
	}

	if len(r.Low) != 1 || r.Low[0] != int64(42) {
		t.Fatalf("low=%v, want [42] (param resolved via binder)", r.Low)
	}
	if len(r.High) != 1 || r.High[0] != int64(42) {
		t.Fatalf("high=%v, want [42] (param resolved via binder)", r.High)
	}
}

func TestParameterBinding_Filter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{
			map[string]any{"X": int64(10)},
			map[string]any{"X": int64(20)},
			map[string]any{"X": int64(30)},
		},
		Typ: values.UnknownType,
	})

	param1 := values.NewParameterValue(1)
	filter := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "X"},
				predicates.Comparison{
					Type:    predicates.ComparisonGreaterThan,
					Operand: param1,
				},
			),
		},
		inner,
	)

	evalCtx := EmptyEvaluationContext().WithParams([]any{int64(15)})
	cursor, err := ExecutePlan(ctx, filter, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (20 and 30 > 15)", len(results))
	}
	v0 := results[0].Datum.(map[string]any)["X"].(int64)
	v1 := results[1].Datum.(map[string]any)["X"].(int64)
	if v0 != 20 || v1 != 30 {
		t.Errorf("values = [%d, %d], want [20, 30]", v0, v1)
	}
}

func TestParameterBinding_Values(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	param1 := values.NewParameterValue(1)
	vplan := plans.NewRecordQueryValuesPlan([]values.Value{param1})

	evalCtx := EmptyEvaluationContext().WithParams([]any{int64(99)})
	cursor, err := ExecutePlan(ctx, vplan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
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
	datum := results[0].Datum.(map[string]any)
	if datum["param"] != int64(99) {
		t.Errorf("param = %v, want 99", datum["param"])
	}
}

func TestExecuteNestedLoopJoin_CrossJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	right := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: "hello", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
	})

	join := plans.NewRecordQueryNestedLoopJoinPlan(left, right, nil, plans.JoinCross)
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (1×1 cross)", len(results))
	}
	row := results[0].Datum.(map[string]any)
	if row["constant"] != "hello" {
		t.Errorf("constant = %v, want 'hello' (inner overwrites)", row["constant"])
	}
}

func TestExecuteNestedLoopJoin_InnerJoin_WithPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(5), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	right := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(5), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	join := plans.NewRecordQueryNestedLoopJoinPlan(
		left, right,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		plans.JoinInner,
	)
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
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
}

func TestExecuteNestedLoopJoin_InnerJoin_PredicateRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	right := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(2), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	join := plans.NewRecordQueryNestedLoopJoinPlan(
		left, right,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		plans.JoinInner,
	)
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (predicate rejects all)", len(results))
	}
}

func TestExecuteNestedLoopJoin_LeftOuter_NoInnerMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	right := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(2), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	join := plans.NewRecordQueryNestedLoopJoinPlan(
		left, right,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		plans.JoinLeftOuter,
	)
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (left outer preserves unmatched)", len(results))
	}
}

func TestExecuteHashAggregation_CountGroupBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	groupKeys := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
	}

	plan := plans.NewRecordQueryHashAggregationPlan(inner, groupKeys, aggs)
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
		t.Fatalf("got %d results, want 1 group", len(results))
	}
	row := results[0].Datum.(map[string]any)
	if row["COUNT(CONSTANT)"] != int64(1) {
		t.Errorf("COUNT = %v, want 1", row["COUNT(CONSTANT)"])
	}
}

func TestExecuteStreamingAggregation_NoGroups_Count(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
		{Function: expressions.AggSum, Operand: &values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
	}

	plan := plans.NewRecordQueryStreamingAggregationPlan(inner, nil, aggs)
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
	row := results[0].Datum.(map[string]any)
	if row["COUNT(CONSTANT)"] != int64(1) {
		t.Errorf("COUNT = %v, want 1", row["COUNT(CONSTANT)"])
	}
	sumVal, ok := row["SUM(CONSTANT)"].(float64)
	if !ok || sumVal != 10 {
		t.Errorf("SUM = %v, want 10.0", row["SUM(CONSTANT)"])
	}
}

func TestExecuteAggregation_EmptyInput_NoGroupKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
		}),
	)

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
	}

	plan := plans.NewRecordQueryHashAggregationPlan(inner, nil, aggs)
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
		t.Fatalf("got %d results, want 1 (COUNT over empty = 0)", len(results))
	}
	row := results[0].Datum.(map[string]any)
	if row["COUNT(CONSTANT)"] != int64(0) {
		t.Errorf("COUNT(empty) = %v, want 0", row["COUNT(CONSTANT)"])
	}
}

func TestExecuteExplode_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryExplodePlan(
		&values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}, Typ: values.UnknownType},
	)
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
		if results[i].Datum != want {
			t.Errorf("results[%d].Datum = %v, want %d", i, results[i].Datum, want)
		}
	}
}

func TestExecuteExplode_Nil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryExplodePlan(values.LiteralValue(nil))
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

func TestExecuteTempTable_InsertAndScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("cte1")

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	insertPlan := plans.NewRecordQueryTempTableInsertPlan(inner, alias, false)
	cursor, err := ExecutePlan(ctx, insertPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	inserted, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("insert collect: %v", err)
	}
	if len(inserted) != 1 {
		t.Fatalf("insert returned %d rows, want 1", len(inserted))
	}

	scanPlan := plans.NewRecordQueryTempTableScanPlan(alias)
	cursor2, err := ExecutePlan(ctx, scanPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	scanned, err := CollectAll(ctx, cursor2)
	if err != nil {
		t.Fatalf("scan collect: %v", err)
	}
	if len(scanned) != 1 {
		t.Fatalf("scan returned %d rows, want 1", len(scanned))
	}
	row := scanned[0].Datum.(map[string]any)
	if row["constant"] != int64(42) {
		t.Errorf("scanned value = %v, want 42", row["constant"])
	}
}

func TestExecuteTempTable_EmptyScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("empty_tt")

	scanPlan := plans.NewRecordQueryTempTableScanPlan(alias)
	cursor, err := ExecutePlan(ctx, scanPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestExecuteTempTable_MultipleInserts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("multi")

	for _, val := range []int64{1, 2, 3} {
		inner := plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: val, Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
		})
		insertPlan := plans.NewRecordQueryTempTableInsertPlan(inner, alias, false)
		cursor, err := ExecutePlan(ctx, insertPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("insert %d: %v", val, err)
		}
		_, err = CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("collect %d: %v", val, err)
		}
	}

	scanPlan := plans.NewRecordQueryTempTableScanPlan(alias)
	cursor, err := ExecutePlan(ctx, scanPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
}

func TestExecuteTableFunction_StreamValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryTableFunctionPlan(
		&values.ConstantValue{Value: []any{int64(10), int64(20)}, Typ: values.UnknownType},
	)
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
	if results[0].Datum != int64(10) || results[1].Datum != int64(20) {
		t.Errorf("results = %v, %v, want 10, 20", results[0].Datum, results[1].Datum)
	}
}

func TestExecuteTableFunction_Nil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryTableFunctionPlan(nil)
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

func TestTempTable_ClearAndReuse(t *testing.T) {
	t.Parallel()

	tt := NewTempTable()
	tt.Add(QueryResult{Datum: int64(1)})
	tt.Add(QueryResult{Datum: int64(2)})

	if len(tt.GetList()) != 2 {
		t.Fatalf("got %d items, want 2", len(tt.GetList()))
	}

	tt.Clear()
	if len(tt.GetList()) != 0 {
		t.Fatalf("after clear, got %d items, want 0", len(tt.GetList()))
	}

	tt.Add(QueryResult{Datum: int64(3)})
	if len(tt.GetList()) != 1 {
		t.Fatalf("after re-add, got %d items, want 1", len(tt.GetList()))
	}
}

func TestSortByKeys_Descending(t *testing.T) {
	t.Parallel()

	items := []QueryResult{
		{Datum: map[string]any{"AGE": int64(25)}},
		{Datum: map[string]any{"AGE": int64(35)}},
		{Datum: map[string]any{"AGE": int64(30)}},
	}

	sortByKeys(items, []string{"age"}, []bool{true})

	ages := make([]int64, len(items))
	for i, item := range items {
		ages[i] = item.Datum.(map[string]any)["AGE"].(int64)
	}
	if ages[0] != 35 || ages[1] != 30 || ages[2] != 25 {
		t.Fatalf("sort by age DESC = %v, want [35 30 25]", ages)
	}
}

func TestRecursiveLevelUnion_SingleLevel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")

	initial := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
		}),
		insertAlias, false,
	)
	recursive := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(nil),
		insertAlias, false,
	)

	plan := plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias)
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
}

func TestRecursiveLevelUnion_EmptyRecursive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")

	initial := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: "root", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		}),
		insertAlias, false,
	)

	recursive := plans.NewRecordQueryTempTableInsertPlan(
		plans.NewRecordQueryExplodePlan(nil),
		insertAlias, false,
	)

	plan := plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias)

	evalCtx := EmptyEvaluationContext()
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (initial only, recursive produces nothing)", len(results))
	}
}

func TestRecursiveDfsJoin_Preorder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	root := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: "A", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
	})
	child := plans.NewRecordQueryExplodePlan(nil)

	prior := values.NamedCorrelationIdentifier("prior")
	plan := plans.NewRecordQueryRecursiveDfsJoinPlan(root, child, prior, plans.DfsPreorder)

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
		t.Fatalf("got %d results, want 1 (leaf node with no children)", len(results))
	}
}

func TestRecursiveDfsJoin_Postorder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	root := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: "A", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
	})
	child := plans.NewRecordQueryExplodePlan(nil)

	prior := values.NamedCorrelationIdentifier("prior")
	plan := plans.NewRecordQueryRecursiveDfsJoinPlan(root, child, prior, plans.DfsPostorder)

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
		t.Fatalf("got %d results, want 1 (leaf node with no children)", len(results))
	}
}
