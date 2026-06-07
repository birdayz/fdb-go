package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	functions "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
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
	if row["'PROJECTED'"] != "projected" {
		t.Errorf("projection result = %v, want 'projected'", row["'PROJECTED'"])
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

func TestCompareAny_Float64(t *testing.T) {
	t.Parallel()
	if compareAny(float64(1.5), float64(2.5)) >= 0 {
		t.Fatal("1.5 should be < 2.5")
	}
	if compareAny(float64(2.5), float64(1.5)) <= 0 {
		t.Fatal("2.5 should be > 1.5")
	}
	if compareAny(float64(3.14), float64(3.14)) != 0 {
		t.Fatal("3.14 should equal 3.14")
	}
}

func TestCompareAny_Bool(t *testing.T) {
	t.Parallel()
	if compareAny(false, true) >= 0 {
		t.Fatal("false should be < true")
	}
	if compareAny(true, false) <= 0 {
		t.Fatal("true should be > false")
	}
	if compareAny(true, true) != 0 {
		t.Fatal("true should equal true")
	}
	if compareAny(false, false) != 0 {
		t.Fatal("false should equal false")
	}
}

func TestCompareAny_MixedTypes(t *testing.T) {
	t.Parallel()
	if compareAny(int64(1), "hello") != 0 {
		t.Fatal("mismatched types should return 0")
	}
	if compareAny(float64(1.0), int64(1)) != 0 {
		t.Fatal("float64 vs int64 should return 0 (no cross-type)")
	}
	if compareAny(true, int64(1)) != 0 {
		t.Fatal("bool vs int64 should return 0")
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
	if row["'RESULT'"] != "result" {
		t.Errorf("composite pipeline result = %v, want 'result'", row["'RESULT'"])
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

	// Upper-only `<= 50` excludes nulls: low is the NULL boundary (one nil
	// tuple element) RANGE_EXCLUSIVE, not TreeStart. Mirrors Java ScanComparisons.
	if len(r.Low) != 1 || r.Low[0] != nil {
		t.Fatalf("low=%v, want [null] (null boundary)", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("lowEndpoint=%v, want RangeExclusive (null boundary)", r.LowEndpoint)
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

	join := plans.NewRecordQueryNestedLoopJoinPlan(left, right, nil, plans.JoinCross, "", "", nil)
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
		"", "",
		nil,
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
		"", "",
		nil,
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
		"", "",
		nil,
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

func TestExecuteStreamingAggregation_CountGroupBy(t *testing.T) {
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

	plan := plans.NewRecordQueryStreamingAggregationPlan(inner, groupKeys, aggs)
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
	sumVal, ok := row["SUM(CONSTANT)"].(int64)
	if !ok || sumVal != 10 {
		t.Errorf("SUM = %v (%T), want int64(10)", row["SUM(CONSTANT)"], row["SUM(CONSTANT)"])
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

func TestProtoToMap_AllSetFields(t *testing.T) {
	t.Parallel()
	order := &gen.Order{
		OrderId:  proto.Int64(42),
		Price:    proto.Int32(199),
		Quantity: proto.Int32(5),
	}
	m := protoToMap(order)

	if m["ORDER_ID"] != int64(42) {
		t.Errorf("ORDER_ID = %v, want 42", m["ORDER_ID"])
	}
	if m["PRICE"] != int64(199) {
		t.Errorf("PRICE = %v, want 199", m["PRICE"])
	}
	if m["QUANTITY"] != int64(5) {
		t.Errorf("QUANTITY = %v, want 5", m["QUANTITY"])
	}
}

func TestProtoToMap_UnsetFieldsOmitted(t *testing.T) {
	t.Parallel()
	order := &gen.Order{
		OrderId: proto.Int64(1),
	}
	m := protoToMap(order)

	if m["ORDER_ID"] != int64(1) {
		t.Errorf("ORDER_ID = %v, want 1", m["ORDER_ID"])
	}
	if _, exists := m["PRICE"]; exists {
		t.Errorf("PRICE should not be present for unset field, got %v", m["PRICE"])
	}
	if _, exists := m["QUANTITY"]; exists {
		t.Errorf("QUANTITY should not be present for unset field, got %v", m["QUANTITY"])
	}
}

func TestProtoToMap_NilMessage(t *testing.T) {
	t.Parallel()
	m := protoToMap(nil)
	if m != nil {
		t.Errorf("protoToMap(nil) = %v, want nil", m)
	}
}

func TestGoToProtoValue_Int32(t *testing.T) {
	t.Parallel()
	order := (&gen.Order{}).ProtoReflect().Descriptor()
	fd := order.Fields().ByName("price") // int32 field

	v, err := goToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatalf("goToProtoValue int64→int32: %v", err)
	}
	if got := int32(v.Int()); got != 42 {
		t.Errorf("got %d, want 42", got)
	}

	v, err = goToProtoValue(fd, int32(99))
	if err != nil {
		t.Fatalf("goToProtoValue int32→int32: %v", err)
	}
	if got := int32(v.Int()); got != 99 {
		t.Errorf("got %d, want 99", got)
	}
}

func TestGoToProtoValue_Int64(t *testing.T) {
	t.Parallel()
	order := (&gen.Order{}).ProtoReflect().Descriptor()
	fd := order.Fields().ByName("order_id") // int64 field

	v, err := goToProtoValue(fd, int64(123))
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if got := v.Int(); got != 123 {
		t.Errorf("got %d, want 123", got)
	}
}

// goToProtoValue must implement the promotable widenings Java's lattice allows
// (INT/LONG→FLOAT/DOUBLE), matching ConvertToProtoValue — a SUM(BIGINT) into a
// DOUBLE/FLOAT column flows an int64 and must widen, not error.
func TestGoToProtoValue_IntToDoubleWidens(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	v, err := goToProtoValue(typed.Fields().ByName("val_double"), int64(60))
	if err != nil {
		t.Fatalf("int64 → DOUBLE should widen, got: %v", err)
	}
	if got := v.Float(); got != 60.0 {
		t.Errorf("got %v, want 60.0", got)
	}
}

func TestGoToProtoValue_IntToFloatWidens(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	v, err := goToProtoValue(typed.Fields().ByName("val_float"), int64(7))
	if err != nil {
		t.Fatalf("int64 → FLOAT should widen, got: %v", err)
	}
	if got := v.Float(); got != 7.0 {
		t.Errorf("got %v, want 7.0", got)
	}
}

// A float64 (DOUBLE) into an integer column is NOT promotable (no DOUBLE→LONG
// edge); goToProtoValue's fallthrough must emit the verbatim 22000
// SemanticException, matching Java + the sibling ConvertToProtoValue — not a
// generic Go error.
func TestGoToProtoValue_DoubleToIntRejects22000(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	_, err := goToProtoValue(typed.Fields().ByName("val_int64"), float64(20.0))
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCannotConvertType {
		t.Fatalf("float64 → BIGINT: want 22000 (CannotConvertType), got %v", err)
	}
}

// The fallthrough is emergent: any genuinely incompatible assignment (e.g.
// string → integer) yields the same verbatim 22000, aligning with the sibling
// converter rather than the old generic fmt.Errorf.
func TestGoToProtoValue_StringToIntRejects22000(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	_, err := goToProtoValue(typed.Fields().ByName("val_int64"), "nope")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCannotConvertType {
		t.Fatalf("string → BIGINT: want 22000 (CannotConvertType), got %v", err)
	}
}

func TestGoToProtoValue_String(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_string")

	v, err := goToProtoValue(fd, "hello")
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if got := v.String(); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestGoToProtoValue_Bool(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_bool")

	v, err := goToProtoValue(fd, true)
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if !v.Bool() {
		t.Error("expected true")
	}
}

func TestGoToProtoValue_Double(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_double")

	v, err := goToProtoValue(fd, 3.14)
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if got := v.Float(); got != 3.14 {
		t.Errorf("got %f, want 3.14", got)
	}
}

func TestGoToProtoValue_TypeError(t *testing.T) {
	t.Parallel()
	order := (&gen.Order{}).ProtoReflect().Descriptor()
	fd := order.Fields().ByName("price")

	_, err := goToProtoValue(fd, "not_a_number")
	if err == nil {
		t.Fatal("expected error for string→int32, got nil")
	}
}

func TestGoToProtoValue_Float(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_float")

	v, err := goToProtoValue(fd, float64(2.5))
	if err != nil {
		t.Fatalf("goToProtoValue float64→float32: %v", err)
	}
	if got := float32(v.Float()); got != 2.5 {
		t.Errorf("got %f, want 2.5", got)
	}

	v, err = goToProtoValue(fd, float32(1.5))
	if err != nil {
		t.Fatalf("goToProtoValue float32→float32: %v", err)
	}
	if got := float32(v.Float()); got != 1.5 {
		t.Errorf("got %f, want 1.5", got)
	}
}

func TestGoToProtoValue_Overflow(t *testing.T) {
	t.Parallel()
	order := (&gen.Order{}).ProtoReflect().Descriptor()
	int32Field := order.Fields().ByName("price")
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	float32Field := typed.Fields().ByName("val_float")

	t.Run("int32 overflow from int64", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(2147483648))
		if err == nil {
			t.Fatal("expected error for int64 value exceeding int32 max, got nil")
		}
		var overflowErr *NumericRangeOverflowError
		if !errors.As(err, &overflowErr) {
			t.Errorf("expected *NumericRangeOverflowError, got %T: %v", err, err)
		}
	})

	t.Run("int32 underflow from int64", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(-2147483649))
		if err == nil {
			t.Fatal("expected error for int64 value below int32 min, got nil")
		}
		var overflowErr *NumericRangeOverflowError
		if !errors.As(err, &overflowErr) {
			t.Errorf("expected *NumericRangeOverflowError, got %T: %v", err, err)
		}
	})

	t.Run("int32 boundary accept max", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(2147483647))
		if err != nil {
			t.Fatalf("expected no error for int32 max boundary, got: %v", err)
		}
	})

	t.Run("int32 boundary accept min", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(-2147483648))
		if err != nil {
			t.Fatalf("expected no error for int32 min boundary, got: %v", err)
		}
	})

	t.Run("float32 overflow from float64", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(float32Field, float64(math.MaxFloat32*2))
		if err == nil {
			t.Fatal("expected error for float64 value exceeding float32 range, got nil")
		}
		var overflowErr *NumericRangeOverflowError
		if !errors.As(err, &overflowErr) {
			t.Errorf("expected *NumericRangeOverflowError, got %T: %v", err, err)
		}
	})

	t.Run("float32 boundary accept max", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(float32Field, float64(math.MaxFloat32))
		if err != nil {
			t.Fatalf("expected no error for float32 max boundary, got: %v", err)
		}
	})
}

func TestGoToProtoValue_ConsistentWithConvertToProtoValue(t *testing.T) {
	t.Parallel()

	orderDesc := (&gen.Order{}).ProtoReflect().Descriptor()
	typedDesc := (&gen.TypedRecord{}).ProtoReflect().Descriptor()

	cases := []struct {
		name string
		fd   protoreflect.FieldDescriptor
		val  any
	}{
		{"int64_to_int32", orderDesc.Fields().ByName("price"), int64(42)},
		{"int64_to_int64", orderDesc.Fields().ByName("order_id"), int64(42)},
		{"float64_to_float", typedDesc.Fields().ByName("val_float"), float64(2.5)},
		{"float64_to_double", typedDesc.Fields().ByName("val_double"), float64(3.14)},
		{"string_to_string", typedDesc.Fields().ByName("val_string"), "hello"},
		{"bool_to_bool", typedDesc.Fields().ByName("val_bool"), true},
		{"bytes_to_bytes", typedDesc.Fields().ByName("val_bytes"), []byte{1, 2, 3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := goToProtoValue(tc.fd, tc.val)
			if err != nil {
				t.Fatalf("goToProtoValue: %v", err)
			}
			want, err2 := functions.ConvertToProtoValue(tc.fd, tc.val)
			if err2 != nil {
				t.Fatalf("ConvertToProtoValue: %v", err2)
			}
			// Bytes fields return []byte which is not comparable via !=; handle separately.
			if tc.fd.Kind() == protoreflect.BytesKind {
				if !bytes.Equal(got.Bytes(), want.Bytes()) {
					t.Errorf("goToProtoValue = %v, ConvertToProtoValue = %v", got, want)
				}
			} else if got.Interface() != want.Interface() {
				t.Errorf("goToProtoValue = %v, ConvertToProtoValue = %v", got, want)
			}
		})
	}
}

func TestGoToProtoValue_Bytes(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_bytes")

	data := []byte{0x01, 0x02, 0x03}
	v, err := goToProtoValue(fd, data)
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	got := v.Bytes()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("got %v, want [1 2 3]", got)
	}
}

// --- scanComparisonsToTupleRange unit tests ---

func eqRange(val any) *predicates.ComparisonRange {
	r := predicates.EmptyComparisonRange()
	c := predicates.NewLiteralComparison(predicates.ComparisonEquals, val)
	res := r.Merge(&c)
	if !res.Ok {
		panic("merge failed for equality")
	}
	return res.Range
}

func ineqRange(comps ...predicates.Comparison) *predicates.ComparisonRange {
	r := predicates.EmptyComparisonRange()
	for i := range comps {
		res := r.Merge(&comps[i])
		if !res.Ok {
			panic("merge failed for inequality")
		}
		r = res.Range
	}
	return r
}

func TestScanComparisons_Empty(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeTreeStart || tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected TupleRangeAll, got low=%d high=%d", tr.LowEndpoint, tr.HighEndpoint)
	}
	if tr.Low != nil || tr.High != nil {
		t.Fatalf("expected nil tuples, got low=%v high=%v", tr.Low, tr.High)
	}
}

func TestScanComparisons_EmptySlice(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeTreeStart || tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected TupleRangeAll, got low=%d high=%d", tr.LowEndpoint, tr.HighEndpoint)
	}
}

func TestScanComparisons_SingleEquality(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{eqRange("alice")}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive || tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected inclusive/inclusive, got low=%d high=%d", tr.LowEndpoint, tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != "alice" {
		t.Fatalf("expected low=[alice], got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != "alice" {
		t.Fatalf("expected high=[alice], got %v", tr.High)
	}
}

func TestScanComparisons_MultiEquality(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange("alice"),
		eqRange(int64(42)),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != int64(42) {
		t.Fatalf("expected low=[alice 42], got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(42) {
		t.Fatalf("expected high=[alice 42], got %v", tr.High)
	}
}

func TestScanComparisons_EqualityThenEmpty(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange("alice"),
		predicates.EmptyComparisonRange(),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tr.Low) != 1 || tr.Low[0] != "alice" {
		t.Fatalf("expected prefix [alice], got low=%v", tr.Low)
	}
}

func TestScanComparisons_GreaterThanNoPrefix(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected high TreeEnd, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != int64(10) {
		t.Fatalf("expected low=[10], got %v", tr.Low)
	}
	if tr.High != nil {
		t.Fatalf("expected nil high, got %v", tr.High)
	}
}

func TestScanComparisons_GreaterThanOrEqNoPrefix(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(10))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected low inclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected high TreeEnd, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != int64(10) {
		t.Fatalf("expected low=[10], got %v", tr.Low)
	}
}

func TestScanComparisons_LessThanNoPrefix(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(50))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// An upper-only range must EXCLUDE NULL index entries (NULL sorts first;
	// `a < 50` is UNKNOWN, not TRUE, on NULL). The low bound is therefore the
	// NULL boundary — one nil tuple element, RANGE_EXCLUSIVE — not TreeStart,
	// which would sweep nulls in. Mirrors Java ScanComparisons.
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low RangeExclusive (null boundary) for LT-only, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected high exclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != nil {
		t.Fatalf("expected low=[null] (null boundary), got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != int64(50) {
		t.Fatalf("expected high=[50], got %v", tr.High)
	}
}

func TestScanComparisons_LessThanOrEqNoPrefix(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, int64(50))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Upper-only range excludes nulls via the NULL boundary low (see LT case).
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low RangeExclusive (null boundary) for LTE-only, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != nil {
		t.Fatalf("expected low=[null] (null boundary), got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != int64(50) {
		t.Fatalf("expected high=[50], got %v", tr.High)
	}
}

func TestScanComparisons_NullComparand_EmptyRange(t *testing.T) {
	t.Parallel()
	// `a < NULL` (and >, >=, <=) is UNKNOWN for every row (SQL 3VL) →
	// unsatisfiable → empty result. Must be an empty range (begin == end),
	// NOT the null-boundary low with an unbounded high (which would strinc to
	// an inverted FDB range begin > end). Regression for the Codex P2 finding.
	for _, typ := range []predicates.ComparisonType{
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
	} {
		tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
			ineqRange(predicates.Comparison{Type: typ, Operand: &values.NullValue{}}),
		}, nil)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", typ, err)
		}
		// Empty range: Low == High with inclusive/exclusive endpoints → begin == end.
		if len(tr.Low) != len(tr.High) {
			t.Fatalf("%v: expected empty range (Low==High), got Low=%v High=%v", typ, tr.Low, tr.High)
		}
		if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive ||
			tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
			t.Fatalf("%v: expected empty range endpoints (Inclusive/Exclusive on equal bounds), got low=%d high=%d",
				typ, tr.LowEndpoint, tr.HighEndpoint)
		}
	}
}

func TestScanComparisons_BetweenGTAndLT(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10)),
			predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(50)),
		),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected high exclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != int64(10) {
		t.Fatalf("expected low=[10], got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != int64(50) {
		t.Fatalf("expected high=[50], got %v", tr.High)
	}
}

func TestScanComparisons_BetweenGTEAndLTE(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(10)),
			predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, int64(50)),
		),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected low inclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive, got %d", tr.HighEndpoint)
	}
}

func TestScanComparisons_EqualityPrefixThenGT(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange("alice"),
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive (prefix default), got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != int64(10) {
		t.Fatalf("expected low=[alice 10], got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != "alice" {
		t.Fatalf("expected high=[alice] (prefix only), got %v", tr.High)
	}
}

func TestScanComparisons_EqualityPrefixThenLT(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange("alice"),
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(50))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With an equality prefix [alice] and an upper-only `a < 50`, the low bound
	// is the NULL boundary for the next column: [alice, null] RANGE_EXCLUSIVE,
	// which excludes rows where x=alice and a IS NULL. Mirrors Java
	// ScanComparisons (baseTuple + null element, exclusive).
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low RangeExclusive (null boundary after prefix), got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected high exclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != nil {
		t.Fatalf("expected low=[alice null] (prefix + null boundary), got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(50) {
		t.Fatalf("expected high=[alice 50], got %v", tr.High)
	}
}

func TestScanComparisons_EqualityPrefixThenBetween(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange("alice"),
		ineqRange(
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(10)),
			predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, int64(50)),
		),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected low inclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != int64(10) {
		t.Fatalf("expected low=[alice 10], got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(50) {
		t.Fatalf("expected high=[alice 50], got %v", tr.High)
	}
}

func TestScanComparisons_IsNotNullNoPrefix(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		ineqRange(predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive (IS NOT NULL sets low exclusive), got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected high TreeEnd, got %d", tr.HighEndpoint)
	}
	// IS NOT NULL is the pure NULL boundary: low is one nil tuple element,
	// RANGE_EXCLUSIVE — the scan starts strictly after all null entries
	// (Java: lowItem null, RANGE_EXCLUSIVE). A nil low tuple would scan from
	// the index start and wrongly include nulls.
	if len(tr.Low) != 1 || tr.Low[0] != nil {
		t.Fatalf("expected low=[null] (null boundary), got %v", tr.Low)
	}
}

func TestScanComparisons_MultiEqualityThenInequality(t *testing.T) {
	t.Parallel()
	tr, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange("alice"),
		eqRange(int64(1)),
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, float64(3.14))),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tr.Low) != 3 || tr.Low[0] != "alice" || tr.Low[1] != int64(1) || tr.Low[2] != float64(3.14) {
		t.Fatalf("expected low=[alice 1 3.14], got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(1) {
		t.Fatalf("expected high=[alice 1] (prefix only), got %v", tr.High)
	}
}

// --- mergeRows unit tests ---

func TestMergeRows_BothMaps(t *testing.T) {
	t.Parallel()
	outer := QueryResult{
		Datum:      map[string]any{"A": 1, "B": 2},
		PrimaryKey: tuple.Tuple{int64(1)},
	}
	inner := QueryResult{
		Datum:      map[string]any{"C": 3, "D": 4},
		PrimaryKey: tuple.Tuple{int64(2)},
	}
	merged := mergeRows(outer, inner, "", "")
	m := merged.Datum.(map[string]any)
	if m["A"] != 1 || m["B"] != 2 || m["C"] != 3 || m["D"] != 4 {
		t.Fatalf("unexpected merged datum: %v", m)
	}
	if merged.PrimaryKey[0] != int64(1) {
		t.Fatalf("PrimaryKey should come from outer, got %v", merged.PrimaryKey)
	}
}

func TestMergeRows_InnerOverridesOuter(t *testing.T) {
	t.Parallel()
	outer := QueryResult{Datum: map[string]any{"K": "outer"}}
	inner := QueryResult{Datum: map[string]any{"K": "inner"}}
	merged := mergeRows(outer, inner, "", "")
	m := merged.Datum.(map[string]any)
	if m["K"] != "inner" {
		t.Fatalf("inner should override outer on key conflict, got %v", m["K"])
	}
}

func TestMergeRows_NonMapDatum(t *testing.T) {
	t.Parallel()
	outer := QueryResult{Datum: "string-datum", PrimaryKey: tuple.Tuple{int64(1)}}
	inner := QueryResult{Datum: map[string]any{"C": 3}}
	merged := mergeRows(outer, inner, "", "")
	if merged.Datum != "string-datum" {
		t.Fatalf("expected outer datum passthrough, got %v", merged.Datum)
	}
}

// --- toFloat64 unit tests ---

func TestToFloat64_Int64(t *testing.T) {
	t.Parallel()
	if v := toFloat64(int64(42)); v != 42.0 {
		t.Fatalf("expected 42.0, got %v", v)
	}
}

func TestToFloat64_Float64(t *testing.T) {
	t.Parallel()
	if v := toFloat64(float64(3.14)); v != 3.14 {
		t.Fatalf("expected 3.14, got %v", v)
	}
}

func TestToFloat64_Int(t *testing.T) {
	t.Parallel()
	if v := toFloat64(int(7)); v != 7.0 {
		t.Fatalf("expected 7.0, got %v", v)
	}
}

func TestToFloat64_Int32(t *testing.T) {
	t.Parallel()
	if v := toFloat64(int32(100)); v != 100.0 {
		t.Fatalf("expected 100.0, got %v", v)
	}
}

func TestToFloat64_Unsupported(t *testing.T) {
	t.Parallel()
	v := toFloat64("hello")
	if !math.IsNaN(v) {
		t.Fatalf("expected NaN for string, got %v", v)
	}
}

func TestToFloat64_Nil(t *testing.T) {
	t.Parallel()
	v := toFloat64(nil)
	if !math.IsNaN(v) {
		t.Fatalf("expected NaN for nil, got %v", v)
	}
}

// --- aggKeyName unit tests ---

func TestAggKeyName_FieldValue(t *testing.T) {
	t.Parallel()
	fv := &values.FieldValue{Field: "status", Typ: values.TypeString}
	if got := aggKeyName(fv); got != "STATUS" {
		t.Fatalf("expected STATUS, got %s", got)
	}
}

func TestAggKeyName_NonFieldValue(t *testing.T) {
	t.Parallel()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	got := aggKeyName(cv)
	want := strings.ToUpper(values.ExplainValue(cv))
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

// --- aggResultName unit tests ---

func TestAggResultName_Count(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function: expressions.AggCount,
		Operand:  &values.FieldValue{Field: "id", Typ: values.TypeInt},
	}
	if got := aggResultName(agg); got != "COUNT(ID)" {
		t.Fatalf("expected COUNT(ID), got %s", got)
	}
}

func TestAggResultName_Sum(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function: expressions.AggSum,
		Operand:  &values.FieldValue{Field: "price", Typ: values.TypeInt},
	}
	if got := aggResultName(agg); got != "SUM(PRICE)" {
		t.Fatalf("expected SUM(PRICE), got %s", got)
	}
}

func TestAggResultName_Min(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function: expressions.AggMin,
		Operand:  &values.FieldValue{Field: "price", Typ: values.TypeInt},
	}
	if got := aggResultName(agg); got != "MIN(PRICE)" {
		t.Fatalf("expected MIN(PRICE), got %s", got)
	}
}

func TestAggResultName_Max(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function: expressions.AggMax,
		Operand:  &values.FieldValue{Field: "price", Typ: values.TypeInt},
	}
	if got := aggResultName(agg); got != "MAX(PRICE)" {
		t.Fatalf("expected MAX(PRICE), got %s", got)
	}
}

func TestAggResultName_Avg(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function: expressions.AggAvg,
		Operand:  &values.FieldValue{Field: "price", Typ: values.TypeInt},
	}
	if got := aggResultName(agg); got != "AVG(PRICE)" {
		t.Fatalf("expected AVG(PRICE), got %s", got)
	}
}

func TestAggResultName_NilOperand(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{Function: expressions.AggCount}
	if got := aggResultName(agg); got != "COUNT(?)" {
		t.Fatalf("expected COUNT(?), got %s", got)
	}
}

func TestAggResultName_UnknownFunction(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function: expressions.AggregateFunction(99),
		Operand:  &values.FieldValue{Field: "x", Typ: values.TypeInt},
	}
	if got := aggResultName(agg); got != "AGG(X)" {
		t.Fatalf("expected AGG(X), got %s", got)
	}
}

// --- distinctKey unit tests ---

func TestDistinctKey_WithDatum(t *testing.T) {
	t.Parallel()
	pk := tuple.Tuple{int64(42)}
	qr := QueryResult{PrimaryKey: pk, Datum: map[string]any{"A": 1}}
	key := distinctKey(qr)
	if key == "" {
		t.Fatal("expected non-empty key from datum map")
	}
	qr2 := QueryResult{PrimaryKey: tuple.Tuple{int64(99)}, Datum: map[string]any{"A": 1}}
	if distinctKey(qr) != distinctKey(qr2) {
		t.Fatal("same datum values should produce same distinct key regardless of PK")
	}
}

func TestDistinctKey_NilPrimaryKey(t *testing.T) {
	t.Parallel()
	qr := QueryResult{Datum: map[string]any{"A": 1}}
	key := distinctKey(qr)
	expected := "A=int:1"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func TestDistinctKey_Deterministic(t *testing.T) {
	t.Parallel()
	// With multiple keys, the output must be sorted and stable regardless
	// of map iteration order.
	qr := QueryResult{Datum: map[string]any{"B": 2, "A": 1, "C": 3}}
	key1 := distinctKey(qr)
	key2 := distinctKey(qr)
	if key1 != key2 {
		t.Fatalf("non-deterministic: %q vs %q", key1, key2)
	}
	expected := "A=int:1|B=int:2|C=int:3"
	if key1 != expected {
		t.Fatalf("expected %q, got %q", expected, key1)
	}
}

// --- intersectionCompKeyFunc unit tests ---

func TestIntersectionCompKeyFunc_NoKeyVals_WithPK(t *testing.T) {
	t.Parallel()
	pk := tuple.Tuple{int64(7)}
	qr := QueryResult{PrimaryKey: pk, Datum: map[string]any{"X": 1}}
	var keyErr error
	fn := intersectionCompKeyFunc(nil, &keyErr)
	got := fn(qr)
	if keyErr != nil {
		t.Fatalf("unexpected key eval error: %v", keyErr)
	}
	if len(got) != 1 || got[0] != int64(7) {
		t.Fatalf("expected PK tuple {7}, got %v", got)
	}
}

func TestIntersectionCompKeyFunc_NoKeyVals_NoPK(t *testing.T) {
	t.Parallel()
	qr := QueryResult{Datum: map[string]any{"X": 1}}
	var keyErr error
	fn := intersectionCompKeyFunc(nil, &keyErr)
	got := fn(qr)
	if keyErr != nil {
		t.Fatalf("unexpected key eval error: %v", keyErr)
	}
	if len(got) != 1 {
		t.Fatalf("expected single-element tuple, got %v", got)
	}
	if _, ok := got[0].(string); !ok {
		t.Fatalf("expected string element, got %T", got[0])
	}
}

func TestIntersectionCompKeyFunc_WithKeyVals(t *testing.T) {
	t.Parallel()
	qr := QueryResult{Datum: map[string]any{"NAME": "alice", "AGE": int64(30)}}
	keyVals := []values.Value{
		&values.FieldValue{Field: "NAME", Typ: values.TypeString},
		&values.FieldValue{Field: "AGE", Typ: values.TypeInt},
	}
	var keyErr error
	fn := intersectionCompKeyFunc(keyVals, &keyErr)
	got := fn(qr)
	if keyErr != nil {
		t.Fatalf("unexpected key eval error: %v", keyErr)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != int64(30) {
		t.Fatalf("expected {alice, 30}, got %v", got)
	}
}

// --- compareValues unit tests ---

func TestCompareValues_NullHandling(t *testing.T) {
	t.Parallel()
	if compareValues(nil, nil) != 0 {
		t.Fatal("nil == nil should be 0")
	}
	if compareValues(nil, int64(1)) >= 0 {
		t.Fatal("nil < non-nil")
	}
	if compareValues(int64(1), nil) <= 0 {
		t.Fatal("non-nil > nil")
	}
}

func TestCompareValues_NumericTypes(t *testing.T) {
	t.Parallel()
	if compareValues(int64(1), int64(2)) >= 0 {
		t.Fatal("1 < 2")
	}
	if compareValues(int64(2), int64(1)) <= 0 {
		t.Fatal("2 > 1")
	}
	if compareValues(int64(42), float64(42.0)) != 0 {
		t.Fatal("int64(42) == float64(42.0)")
	}
	if compareValues(float64(3.14), int64(3)) <= 0 {
		t.Fatal("3.14 > 3")
	}
}

func TestCompareValues_CrossTypeNotEqual(t *testing.T) {
	t.Parallel()
	// int vs string must NOT return 0 (the NaN bug would make them "equal")
	cmp := compareValues(int64(42), "hello")
	if cmp == 0 {
		t.Fatal("int64(42) should not equal string 'hello'")
	}
	cmp2 := compareValues(float64(3.14), "world")
	if cmp2 == 0 {
		t.Fatal("float64(3.14) should not equal string 'world'")
	}
}

func TestCompareValues_Strings(t *testing.T) {
	t.Parallel()
	if compareValues("abc", "def") >= 0 {
		t.Fatal("abc < def")
	}
	if compareValues("xyz", "abc") <= 0 {
		t.Fatal("xyz > abc")
	}
	if compareValues("same", "same") != 0 {
		t.Fatal("same == same")
	}
}

// --- passesJoinPredicates unit tests ---

func TestPassesJoinPredicates_Empty(t *testing.T) {
	t.Parallel()
	qr := QueryResult{Datum: map[string]any{"A": 1}}
	ok, err := passesJoinPredicates(qr, nil, EmptyEvaluationContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("empty predicates should pass")
	}
}

func TestPassesJoinPredicates_MatchingPredicate(t *testing.T) {
	t.Parallel()
	qr := QueryResult{Datum: map[string]any{"PRICE": int64(100)}}
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "PRICE", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100)),
	)
	ok, err := passesJoinPredicates(qr, []predicates.QueryPredicate{pred}, EmptyEvaluationContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("matching predicate should pass")
	}
}

func TestPassesJoinPredicates_NonMatchingPredicate(t *testing.T) {
	t.Parallel()
	qr := QueryResult{Datum: map[string]any{"PRICE": int64(100)}}
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "PRICE", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(999)),
	)
	ok, err := passesJoinPredicates(qr, []predicates.QueryPredicate{pred}, EmptyEvaluationContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("non-matching predicate should fail")
	}
}

// --- projectionColumnName unit tests ---

func TestProjectionColumnName_FieldValue(t *testing.T) {
	t.Parallel()
	fv := &values.FieldValue{Field: "MY_COL", Typ: values.TypeString}
	if got := projectionColumnName(fv); got != "MY_COL" {
		t.Fatalf("expected MY_COL, got %s", got)
	}
}

func TestProjectionColumnName_NonFieldValue(t *testing.T) {
	t.Parallel()
	cv := &values.ConstantValue{Value: int64(42), Typ: values.TypeInt}
	want := strings.ToUpper(values.ExplainValue(cv))
	if got := projectionColumnName(cv); got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

// --- fieldFromDatum unit tests ---

func TestFieldFromDatum_MapFound(t *testing.T) {
	t.Parallel()
	datum := map[string]any{"NAME": "alice", "AGE": int64(30)}
	if v := fieldFromDatum(datum, "name"); v != "alice" {
		t.Fatalf("expected alice, got %v", v)
	}
}

func TestFieldFromDatum_MapNotFound(t *testing.T) {
	t.Parallel()
	datum := map[string]any{"NAME": "alice"}
	if v := fieldFromDatum(datum, "MISSING"); v != nil {
		t.Fatalf("expected nil, got %v", v)
	}
}

func TestFieldFromDatum_NonMap(t *testing.T) {
	t.Parallel()
	if v := fieldFromDatum("not-a-map", "NAME"); v != nil {
		t.Fatalf("expected nil for non-map, got %v", v)
	}
}

func TestFieldFromDatum_CaseInsensitive(t *testing.T) {
	t.Parallel()
	datum := map[string]any{"PRICE": int64(100)}
	if v := fieldFromDatum(datum, "price"); v != int64(100) {
		t.Fatalf("expected 100, got %v (case-insensitive lookup via ToUpper)", v)
	}
}

// ----- EvaluationContext (additional coverage) ------------------------------

func TestEmptyEvaluationContext_NoBindings(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	if ec == nil {
		t.Fatal("expected non-nil context")
	}
	_, ok := ec.GetBinding(values.NamedCorrelationIdentifier("anything"))
	if ok {
		t.Fatal("empty context should have no bindings")
	}
}

func TestEvaluationContext_WithParams(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	ec2 := ec.WithParams([]any{int64(10), "hello"})

	v, ok := ec2.BindParameter(1, "")
	if !ok || v != int64(10) {
		t.Fatalf("param 1: got %v, %v, want 10, true", v, ok)
	}
	v, ok = ec2.BindParameter(2, "")
	if !ok || v != "hello" {
		t.Fatalf("param 2: got %v, %v, want hello, true", v, ok)
	}

	_, ok = ec.BindParameter(1, "")
	if ok {
		t.Fatal("original context should not have params")
	}
}

func TestEvaluationContext_BindParameter_Bounds(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{int64(1)})

	_, ok := ec.BindParameter(0, "")
	if ok {
		t.Fatal("ordinal 0 should fail (1-based)")
	}
	_, ok = ec.BindParameter(2, "")
	if ok {
		t.Fatal("ordinal 2 should fail (only 1 param)")
	}
	_, ok = ec.BindParameter(-1, "")
	if ok {
		t.Fatal("negative ordinal should fail")
	}
}

func TestEvaluationContext_WithBinding_DoesNotMutateParent(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id1 := values.NamedCorrelationIdentifier("a")
	id2 := values.NamedCorrelationIdentifier("b")
	ec1 := ec.WithBinding(id1, "val1")
	ec2 := ec1.WithBinding(id2, "val2")

	if _, ok := ec1.GetBinding(id2); ok {
		t.Fatal("ec1 should not see ec2's binding")
	}
	if v, ok := ec2.GetBinding(id1); !ok || v != "val1" {
		t.Fatal("ec2 should inherit ec1's bindings")
	}
}

func TestEvaluationContext_WithParams_CopiesBindings(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("x")
	ec = ec.WithBinding(id, "kept")
	ec2 := ec.WithParams([]any{int64(42)})

	v, ok := ec2.GetBinding(id)
	if !ok || v != "kept" {
		t.Fatal("WithParams should preserve existing bindings")
	}
	v, ok = ec2.BindParameter(1, "")
	if !ok || v != int64(42) {
		t.Fatal("WithParams should set params")
	}
}

func TestEvaluationContext_RowContext(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{int64(99)})
	datum := map[string]any{"col": "hello"}
	rc := ec.RowContext(datum)
	if rc.Datum["col"] != "hello" {
		t.Fatal("RowContext should pass through datum")
	}
	v, ok := rc.Binder.BindParameter(1, "")
	if !ok || v != int64(99) {
		t.Fatal("RowContext's binder should use the EvalContext's params")
	}
}

func TestEvaluationContext_RowContext_CorrelationBinding(t *testing.T) {
	t.Parallel()
	id := values.NamedCorrelationIdentifier("explode_q1")
	ec := EmptyEvaluationContext().WithBinding(id, int64(42))
	datum := map[string]any{"col": "hello"}
	rc := ec.RowContext(datum)
	if rc.Correlations == nil {
		t.Fatal("RowContext should pass through correlation binder")
	}
	v, ok := rc.Correlations.GetCorrelationBinding(id)
	if !ok || v != int64(42) {
		t.Fatalf("expected correlation binding 42, got %v (ok=%v)", v, ok)
	}
	qov := values.NewQuantifiedObjectValue(id)
	result := mustEvaluate(qov, rc)
	if result != int64(42) {
		t.Fatalf("QOV.Evaluate(RowEvalContext) = %v, want 42", result)
	}
}

// ----- TempTable (additional coverage) --------------------------------------

func TestTempTable_AddAndGetList(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(QueryResult{Datum: int64(1)})
	tt.Add(QueryResult{Datum: int64(2)})

	list := tt.GetList()
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
	if list[0].Datum != int64(1) || list[1].Datum != int64(2) {
		t.Errorf("unexpected contents: %v %v", list[0].Datum, list[1].Datum)
	}
}

func TestTempTable_GetListReturnsSnapshot(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(QueryResult{Datum: int64(1)})
	snap := tt.GetList()
	tt.Add(QueryResult{Datum: int64(2)})

	if len(snap) != 1 {
		t.Fatal("snapshot should not grow when new items added")
	}
	if len(tt.GetList()) != 2 {
		t.Fatal("temp table should now have 2 items")
	}
}

func TestTempTable_EmptyList(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	list := tt.GetList()
	if len(list) != 0 {
		t.Fatal("new temp table should be empty")
	}
}

func TestEvaluationContext_GetOrCreateTempTable(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("tt1")
	tt1 := ec.GetOrCreateTempTable(id)
	tt1.Add(QueryResult{Datum: int64(1)})

	tt2 := ec.GetOrCreateTempTable(id)
	if len(tt2.GetList()) != 1 {
		t.Fatal("second GetOrCreateTempTable should return same instance")
	}
}

func TestEvaluationContext_GetOrCreateTempTable_DistinctIDs(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id1 := values.NamedCorrelationIdentifier("tt1")
	id2 := values.NamedCorrelationIdentifier("tt2")

	tt1 := ec.GetOrCreateTempTable(id1)
	tt1.Add(QueryResult{Datum: int64(1)})

	tt2 := ec.GetOrCreateTempTable(id2)
	if len(tt2.GetList()) != 0 {
		t.Fatal("different IDs should create distinct temp tables")
	}
}

// ----- goToProtoValue (enum — extends existing int/string/bool/float/double/bytes tests) ---

func TestGoToProtoValue_EnumField(t *testing.T) {
	t.Parallel()
	msg := &gen.TypedRecord{}
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("val_enum")
	pv, err := goToProtoValue(fd, int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if int64(pv.Enum()) != 2 {
		t.Fatalf("expected enum 2, got %d", pv.Enum())
	}
}

// ----- sortByKeys (multi-key — extends existing single-key tests) -----------

func TestSortByKeys_MultipleKeys(t *testing.T) {
	t.Parallel()
	items := []QueryResult{
		{Datum: map[string]any{"A": int64(2), "B": int64(1)}},
		{Datum: map[string]any{"A": int64(1), "B": int64(2)}},
		{Datum: map[string]any{"A": int64(1), "B": int64(1)}},
	}
	sortByKeys(items, []string{"A", "B"}, []bool{false, false})

	d0 := items[0].Datum.(map[string]any)
	d1 := items[1].Datum.(map[string]any)
	d2 := items[2].Datum.(map[string]any)
	if d0["A"] != int64(1) || d0["B"] != int64(1) {
		t.Errorf("row 0: got A=%v B=%v, want 1,1", d0["A"], d0["B"])
	}
	if d1["A"] != int64(1) || d1["B"] != int64(2) {
		t.Errorf("row 1: got A=%v B=%v, want 1,2", d1["A"], d1["B"])
	}
	if d2["A"] != int64(2) {
		t.Errorf("row 2: got A=%v, want 2", d2["A"])
	}
}

// ----- CollectAll (multi-item — extends existing empty test) ----------------

func TestCollectAll_MultipleItems(t *testing.T) {
	t.Parallel()
	items := []QueryResult{
		{Datum: int64(1)},
		{Datum: int64(2)},
		{Datum: int64(3)},
	}
	cursor := recordlayer.FromList(items)
	results, err := CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3, got %d", len(results))
	}
	for i, r := range results {
		if r.Datum != int64(i+1) {
			t.Errorf("item %d: got %v, want %d", i, r.Datum, i+1)
		}
	}
}

// =============================================================================
// scalarProtoToGo — exhaustive coverage of every protoreflect.Kind
// =============================================================================

func TestScalarProtoToGo_Bool(t *testing.T) {
	t.Parallel()
	got := scalarProtoToGo(protoreflect.BoolKind, protoreflect.ValueOfBool(true))
	if got != true {
		t.Errorf("got %v, want true", got)
	}
	got = scalarProtoToGo(protoreflect.BoolKind, protoreflect.ValueOfBool(false))
	if got != false {
		t.Errorf("got %v, want false", got)
	}
}

func TestScalarProtoToGo_Int32Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := scalarProtoToGo(kind, protoreflect.ValueOfInt32(42))
			if got != int64(42) {
				t.Errorf("got %v (%T), want int64(42)", got, got)
			}
			got = scalarProtoToGo(kind, protoreflect.ValueOfInt32(-1))
			if got != int64(-1) {
				t.Errorf("got %v, want int64(-1)", got)
			}
		})
	}
}

func TestScalarProtoToGo_Int64Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := scalarProtoToGo(kind, protoreflect.ValueOfInt64(math.MaxInt64))
			if got != int64(math.MaxInt64) {
				t.Errorf("got %v, want MaxInt64", got)
			}
		})
	}
}

func TestScalarProtoToGo_Uint32Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := scalarProtoToGo(kind, protoreflect.ValueOfUint32(math.MaxUint32))
			if got != int64(math.MaxUint32) {
				t.Errorf("got %v (%T), want int64(%d)", got, got, uint32(math.MaxUint32))
			}
		})
	}
}

func TestScalarProtoToGo_Uint64Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := scalarProtoToGo(kind, protoreflect.ValueOfUint64(12345))
			if got != int64(12345) {
				t.Errorf("got %v (%T), want int64(12345)", got, got)
			}
		})
	}
}

func TestScalarProtoToGo_Float(t *testing.T) {
	t.Parallel()
	got := scalarProtoToGo(protoreflect.FloatKind, protoreflect.ValueOfFloat32(3.14))
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("got type %T, want float64", got)
	}
	if f < 3.13 || f > 3.15 {
		t.Errorf("got %f, want ~3.14", f)
	}
}

func TestScalarProtoToGo_Double(t *testing.T) {
	t.Parallel()
	got := scalarProtoToGo(protoreflect.DoubleKind, protoreflect.ValueOfFloat64(2.71828))
	if got != float64(2.71828) {
		t.Errorf("got %v, want 2.71828", got)
	}
}

func TestScalarProtoToGo_String(t *testing.T) {
	t.Parallel()
	got := scalarProtoToGo(protoreflect.StringKind, protoreflect.ValueOfString("hello"))
	if got != "hello" {
		t.Errorf("got %v, want hello", got)
	}
	got = scalarProtoToGo(protoreflect.StringKind, protoreflect.ValueOfString(""))
	if got != "" {
		t.Errorf("got %v, want empty string", got)
	}
}

func TestScalarProtoToGo_Bytes(t *testing.T) {
	t.Parallel()
	data := []byte{0xDE, 0xAD}
	got := scalarProtoToGo(protoreflect.BytesKind, protoreflect.ValueOfBytes(data))
	b, ok := got.([]byte)
	if !ok {
		t.Fatalf("got type %T, want []byte", got)
	}
	if len(b) != 2 || b[0] != 0xDE || b[1] != 0xAD {
		t.Errorf("got %x, want DEAD", b)
	}
}

func TestScalarProtoToGo_Enum(t *testing.T) {
	t.Parallel()
	got := scalarProtoToGo(protoreflect.EnumKind, protoreflect.ValueOfEnum(2))
	if got != int64(2) {
		t.Errorf("got %v (%T), want int64(2)", got, got)
	}
}

// =============================================================================
// protoFieldToGo — list, scalar, and message fields
// =============================================================================

func TestProtoFieldToGo_ScalarField(t *testing.T) {
	t.Parallel()
	order := &gen.Order{Price: proto.Int32(42)}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("price")
	got := protoFieldToGo(fd, refl.Get(fd))
	if got != int64(42) {
		t.Errorf("got %v (%T), want int64(42)", got, got)
	}
}

func TestProtoFieldToGo_RepeatedStringField(t *testing.T) {
	t.Parallel()
	order := &gen.Order{Tags: []string{"a", "b", "c"}}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("tags")
	got := protoFieldToGo(fd, refl.Get(fd))
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("got type %T, want []any", got)
	}
	if len(arr) != 3 {
		t.Fatalf("got len %d, want 3", len(arr))
	}
	if arr[0] != "a" || arr[1] != "b" || arr[2] != "c" {
		t.Errorf("got %v, want [a b c]", arr)
	}
}

func TestProtoFieldToGo_EmptyRepeated(t *testing.T) {
	t.Parallel()
	order := &gen.Order{Tags: []string{}}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("tags")
	if refl.Has(fd) {
		got := protoFieldToGo(fd, refl.Get(fd))
		arr := got.([]any)
		if len(arr) != 0 {
			t.Errorf("expected empty slice, got %v", arr)
		}
	}
}

func TestProtoFieldToGo_MessageField(t *testing.T) {
	t.Parallel()
	flower := &gen.Flower{Type: proto.String("rose")}
	order := &gen.Order{Flower: flower}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("flower")
	got := protoFieldToGo(fd, refl.Get(fd))
	msg, ok := got.(*gen.Flower)
	if !ok {
		t.Fatalf("got type %T, want *gen.Flower", got)
	}
	if msg.GetType() != "rose" {
		t.Errorf("got %q, want rose", msg.GetType())
	}
}

// =============================================================================
// protoToMap — comprehensive field-type coverage
// =============================================================================

func TestProtoToMap_TypedRecord_AllKinds(t *testing.T) {
	t.Parallel()
	rec := &gen.TypedRecord{
		Id:          proto.Int64(1),
		ValInt32:    proto.Int32(32),
		ValInt64:    proto.Int64(64),
		ValSint32:   proto.Int32(-32),
		ValSint64:   proto.Int64(-64),
		ValSfixed32: proto.Int32(320),
		ValSfixed64: proto.Int64(640),
		ValFloat:    proto.Float32(1.5),
		ValDouble:   proto.Float64(2.5),
		ValBool:     proto.Bool(true),
		ValString:   proto.String("test"),
		ValBytes:    []byte{0x01},
	}
	m := protoToMap(rec)

	checks := map[string]any{
		"ID":           int64(1),
		"VAL_INT32":    int64(32),
		"VAL_INT64":    int64(64),
		"VAL_SINT32":   int64(-32),
		"VAL_SINT64":   int64(-64),
		"VAL_SFIXED32": int64(320),
		"VAL_SFIXED64": int64(640),
		"VAL_BOOL":     true,
		"VAL_STRING":   "test",
	}
	for key, want := range checks {
		got, ok := m[key]
		if !ok {
			t.Errorf("missing key %q", key)
			continue
		}
		if got != want {
			t.Errorf("%s: got %v (%T), want %v (%T)", key, got, got, want, want)
		}
	}

	if b, ok := m["VAL_BYTES"].([]byte); !ok || len(b) != 1 || b[0] != 0x01 {
		t.Errorf("VAL_BYTES: got %v, want [01]", m["VAL_BYTES"])
	}

	// Float fields widen to float64.
	fv, ok := m["VAL_FLOAT"].(float64)
	if !ok {
		t.Fatalf("VAL_FLOAT type %T, want float64", m["VAL_FLOAT"])
	}
	if fv < 1.4 || fv > 1.6 {
		t.Errorf("VAL_FLOAT: got %f, want ~1.5", fv)
	}
	dv := m["VAL_DOUBLE"].(float64)
	if dv != 2.5 {
		t.Errorf("VAL_DOUBLE: got %f, want 2.5", dv)
	}
}

func TestProtoToMap_RepeatedField(t *testing.T) {
	t.Parallel()
	order := &gen.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{"x", "y"},
	}
	m := protoToMap(order)
	tags, ok := m["TAGS"].([]any)
	if !ok {
		t.Fatalf("TAGS type %T, want []any", m["TAGS"])
	}
	if len(tags) != 2 || tags[0] != "x" || tags[1] != "y" {
		t.Errorf("TAGS = %v, want [x y]", tags)
	}
}

func TestProtoToMap_MessageField(t *testing.T) {
	t.Parallel()
	order := &gen.Order{
		OrderId: proto.Int64(1),
		Flower:  &gen.Flower{Type: proto.String("tulip")},
	}
	m := protoToMap(order)
	flower, ok := m["FLOWER"].(*gen.Flower)
	if !ok {
		t.Fatalf("FLOWER type %T, want *gen.Flower", m["FLOWER"])
	}
	if flower.GetType() != "tulip" {
		t.Errorf("got %q, want tulip", flower.GetType())
	}
}

func TestProtoToMap_EnumField(t *testing.T) {
	t.Parallel()
	blue := gen.Color_BLUE
	rec := &gen.TypedRecord{
		Id:      proto.Int64(1),
		ValEnum: &blue,
	}
	m := protoToMap(rec)
	got, ok := m["VAL_ENUM"]
	if !ok {
		t.Fatal("missing VAL_ENUM")
	}
	if got != int64(gen.Color_BLUE) {
		t.Errorf("VAL_ENUM = %v, want %d", got, gen.Color_BLUE)
	}
}

func TestProtoToMap_BytesField(t *testing.T) {
	t.Parallel()
	order := &gen.Order{
		OrderId:    proto.Int64(1),
		VectorData: []byte{0xCA, 0xFE},
	}
	m := protoToMap(order)
	b, ok := m["VECTOR_DATA"].([]byte)
	if !ok {
		t.Fatalf("VECTOR_DATA type %T, want []byte", m["VECTOR_DATA"])
	}
	if len(b) != 2 || b[0] != 0xCA || b[1] != 0xFE {
		t.Errorf("VECTOR_DATA = %x, want CAFE", b)
	}
}

func TestProtoToMap_UpperCaseKeys(t *testing.T) {
	t.Parallel()
	order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(99)}
	m := protoToMap(order)
	for key := range m {
		if key != strings.ToUpper(key) {
			t.Errorf("key %q is not upper-case", key)
		}
	}
}

// =============================================================================
// goToProtoValue — gap coverage: uint32, uint64, int→int64
// =============================================================================

func TestGoToProtoValue_Int32FromInt(t *testing.T) {
	t.Parallel()
	fd := (&gen.Order{}).ProtoReflect().Descriptor().Fields().ByName("price")
	v, err := goToProtoValue(fd, int(77))
	if err != nil {
		t.Fatal(err)
	}
	if int32(v.Int()) != 77 {
		t.Errorf("got %d, want 77", v.Int())
	}
}

func TestGoToProtoValue_Int64FromInt(t *testing.T) {
	t.Parallel()
	fd := (&gen.Order{}).ProtoReflect().Descriptor().Fields().ByName("order_id")
	v, err := goToProtoValue(fd, int(999))
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != 999 {
		t.Errorf("got %d, want 999", v.Int())
	}
}

func TestGoToProtoValue_TypeErrors(t *testing.T) {
	t.Parallel()
	typed := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	tests := []struct {
		name  string
		field string
		val   any
	}{
		{"bool_from_string", "val_bool", "true"},
		{"int32_from_bool", "val_int32", true},
		{"int64_from_string", "val_int64", "42"},
		{"float_from_string", "val_float", "3.14"},
		{"double_from_bool", "val_double", false},
		{"string_from_int", "val_string", 42},
		{"bytes_from_int", "val_bytes", 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fd := typed.Fields().ByName(protoreflect.Name(tc.field))
			_, err := goToProtoValue(fd, tc.val)
			if err == nil {
				t.Errorf("expected error for %T → %s", tc.val, tc.field)
			}
		})
	}
}

// =============================================================================
// protoToMap + goToProtoValue round-trip: read → map → write → read
// =============================================================================

func TestProtoRoundTrip_AllScalarKinds(t *testing.T) {
	t.Parallel()
	rec := &gen.TypedRecord{
		Id:          proto.Int64(7),
		ValInt32:    proto.Int32(-42),
		ValInt64:    proto.Int64(math.MaxInt64),
		ValSint32:   proto.Int32(math.MinInt32),
		ValSint64:   proto.Int64(math.MinInt64),
		ValSfixed32: proto.Int32(12345),
		ValSfixed64: proto.Int64(-99999),
		ValFloat:    proto.Float32(1.5),
		ValDouble:   proto.Float64(math.Pi),
		ValBool:     proto.Bool(true),
		ValString:   proto.String("round-trip"),
		ValBytes:    []byte{0xAB, 0xCD},
	}

	m := protoToMap(rec)

	dst := &gen.TypedRecord{}
	refl := dst.ProtoReflect()
	desc := refl.Descriptor()

	fieldMap := map[string]any{
		"id":           m["ID"],
		"val_int32":    m["VAL_INT32"],
		"val_int64":    m["VAL_INT64"],
		"val_sint32":   m["VAL_SINT32"],
		"val_sint64":   m["VAL_SINT64"],
		"val_sfixed32": m["VAL_SFIXED32"],
		"val_sfixed64": m["VAL_SFIXED64"],
		"val_float":    m["VAL_FLOAT"],
		"val_double":   m["VAL_DOUBLE"],
		"val_bool":     m["VAL_BOOL"],
		"val_string":   m["VAL_STRING"],
		"val_bytes":    m["VAL_BYTES"],
	}

	for name, val := range fieldMap {
		fd := desc.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("no field %q", name)
		}
		pv, err := goToProtoValue(fd, val)
		if err != nil {
			t.Fatalf("goToProtoValue(%s, %v): %v", name, val, err)
		}
		refl.Set(fd, pv)
	}

	if dst.GetId() != 7 {
		t.Errorf("Id: got %d, want 7", dst.GetId())
	}
	if dst.GetValInt32() != -42 {
		t.Errorf("ValInt32: got %d, want -42", dst.GetValInt32())
	}
	if dst.GetValInt64() != math.MaxInt64 {
		t.Errorf("ValInt64: got %d, want MaxInt64", dst.GetValInt64())
	}
	if dst.GetValSint32() != math.MinInt32 {
		t.Errorf("ValSint32: got %d, want MinInt32", dst.GetValSint32())
	}
	if dst.GetValSint64() != math.MinInt64 {
		t.Errorf("ValSint64: got %d, want MinInt64", dst.GetValSint64())
	}
	if dst.GetValBool() != true {
		t.Error("ValBool: got false, want true")
	}
	if dst.GetValString() != "round-trip" {
		t.Errorf("ValString: got %q, want round-trip", dst.GetValString())
	}
	if dst.GetValDouble() != math.Pi {
		t.Errorf("ValDouble: got %f, want Pi", dst.GetValDouble())
	}
}

// =============================================================================
// FromStoredRecord — integration of protoToMap into QueryResult construction
// =============================================================================

func TestFromStoredRecord(t *testing.T) {
	t.Parallel()
	order := &gen.Order{
		OrderId:  proto.Int64(42),
		Price:    proto.Int32(199),
		Quantity: proto.Int32(3),
	}
	rec := &recordlayer.FDBStoredRecord[proto.Message]{
		Record:     order,
		PrimaryKey: tuple.Tuple{int64(42)},
	}
	qr := FromStoredRecord(rec)

	m, ok := qr.Datum.(map[string]any)
	if !ok {
		t.Fatalf("Datum type %T, want map[string]any", qr.Datum)
	}
	if m["ORDER_ID"] != int64(42) {
		t.Errorf("ORDER_ID = %v, want 42", m["ORDER_ID"])
	}
	if m["PRICE"] != int64(199) {
		t.Errorf("PRICE = %v, want 199", m["PRICE"])
	}
	if qr.PrimaryKey[0] != int64(42) {
		t.Errorf("PrimaryKey = %v, want [42]", qr.PrimaryKey)
	}
	if qr.Record != rec {
		t.Error("Record pointer mismatch")
	}
}

// TestMergeRows_ChainedJoin verifies that mergeRows does not clobber
// already-qualified keys when the outer row is itself a merged NLJ
// result. Regression test for the join_chained conformance failure
// where the third row of a 3-way join returned dept.name instead of
// emp.name in the first projection column.
func TestMergeRows_ChainedJoin(t *testing.T) {
	t.Parallel()

	// Simulate the output of the first NLJ: emp(3, Carol, 10) JOIN dept(10, Engineering).
	// The inner (dept) overwrites bare keys ("ID", "NAME") because it runs second.
	firstNLJ := QueryResult{
		Datum: map[string]any{
			"ID":          int64(10),     // dept's ID (overwrote emp's)
			"NAME":        "Engineering", // dept's NAME (overwrote emp's)
			"DEPT_ID":     int64(10),
			"EMP.ID":      int64(3), // emp's qualified key
			"EMP.NAME":    "Carol",  // emp's qualified key
			"EMP.DEPT_ID": int64(10),
			"DEPT.ID":     int64(10),
			"DEPT.NAME":   "Engineering",
		},
	}
	project := QueryResult{
		Datum: map[string]any{
			"ID":     int64(102),
			"EMP_ID": int64(3),
		},
	}

	// The second NLJ merges firstNLJ (outer, alias="DEPT") with project (inner, alias="PROJECT").
	merged := mergeRows(firstNLJ, project, "DEPT", "PROJECT")
	m, ok := merged.Datum.(map[string]any)
	if !ok {
		t.Fatalf("merged.Datum type = %T, want map[string]any", merged.Datum)
	}

	// The critical check: EMP.NAME must still be "Carol", not "Engineering".
	// Before the fix, re-qualifying the bare key "NAME" (= "Engineering") under
	// outerType "EMP" overwrote the correct "EMP.NAME" = "Carol".
	if v := m["EMP.NAME"]; v != "Carol" {
		t.Errorf("EMP.NAME = %v, want Carol", v)
	}
	if v := m["DEPT.NAME"]; v != "Engineering" {
		t.Errorf("DEPT.NAME = %v, want Engineering", v)
	}
	if v := m["EMP.ID"]; v != int64(3) {
		t.Errorf("EMP.ID = %v, want 3", v)
	}
	if v := m["PROJECT.ID"]; v != int64(102) {
		t.Errorf("PROJECT.ID = %v, want 102", v)
	}
	if v := m["PROJECT.EMP_ID"]; v != int64(3) {
		t.Errorf("PROJECT.EMP_ID = %v, want 3", v)
	}
}

// TestMergeRows_DerivedTableAlias verifies that mergeRows correctly
// qualifies keys under the derived table alias (e.g. "SQ1") rather
// than the underlying table name.
func TestMergeRows_DerivedTableAlias(t *testing.T) {
	t.Parallel()

	// Derived table output: (SELECT ida AS x FROM a) AS sq1
	// executeProjection produces {IDA: 1, X: 1}
	outer := QueryResult{
		Datum: map[string]any{
			"IDA": int64(1),
			"X":   int64(1),
		},
	}
	inner := QueryResult{
		Datum: map[string]any{
			"IDB": int64(4),
		},
	}

	merged := mergeRows(outer, inner, "SQ1", "B")
	m, ok := merged.Datum.(map[string]any)
	if !ok {
		t.Fatalf("merged.Datum type = %T, want map[string]any", merged.Datum)
	}

	if v := m["SQ1.X"]; v != int64(1) {
		t.Errorf("SQ1.X = %v, want 1", v)
	}
	if v := m["B.IDB"]; v != int64(4) {
		t.Errorf("B.IDB = %v, want 4", v)
	}
}

// ---------------------------------------------------------------------------
// mergeSortCursor tests
// ---------------------------------------------------------------------------

// qr is a shorthand to build a QueryResult with a map datum.
func qr(kvs ...any) QueryResult {
	m := make(map[string]any, len(kvs)/2)
	for i := 0; i < len(kvs)-1; i += 2 {
		m[kvs[i].(string)] = kvs[i+1]
	}
	return QueryResult{Datum: m}
}

// collectMergeSortCursor drains a mergeSortCursor and returns all results.
func collectMergeSortCursor(t *testing.T, c *mergeSortCursor) []QueryResult {
	t.Helper()
	ctx := context.Background()
	var out []QueryResult
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext error: %v", err)
		}
		if !r.HasNext() {
			break
		}
		out = append(out, r.GetValue())
	}
	return out
}

// fieldVal returns the int64 at key k from a QueryResult datum.
func fieldVal(t *testing.T, r QueryResult, k string) int64 {
	t.Helper()
	m, ok := r.Datum.(map[string]any)
	if !ok {
		t.Fatalf("datum type %T, want map[string]any", r.Datum)
	}
	v, ok := m[k]
	if !ok {
		t.Fatalf("key %q missing from datum %v", k, m)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("key %q = %T, want int64", k, v)
	}
	return n
}

func newMergeSortCursor(
	cursors []recordlayer.RecordCursor[QueryResult],
	compKeys []values.Value,
	reverse bool,
	dedup bool,
) *mergeSortCursor {
	return &mergeSortCursor{
		cursors:   cursors,
		compKeys:  compKeys,
		reverse:   reverse,
		dedup:     dedup,
		peeked:    make([]QueryResult, len(cursors)),
		hasPeeked: make([]bool, len(cursors)),
		exhausted: make([]bool, len(cursors)),
	}
}

func TestMergeSortCursor_TwoSortedInputs(t *testing.T) {
	t.Parallel()

	// Left:  id=1, id=3, id=5
	// Right: id=2, id=4, id=6
	// Expected merged ASC: 1,2,3,4,5,6
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(3)),
		qr("id", int64(5)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(4)),
		qr("id", int64(6)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false, // ascending
		false, // no dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}
	expected := []int64{1, 2, 3, 4, 5, 6}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_Deduplication(t *testing.T) {
	t.Parallel()

	// Both inputs have overlapping keys: 1,2,3 and 2,3,4
	// With dedup, should produce 1,2,3,4
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(3)),
		qr("id", int64(4)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false, // ascending
		true,  // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4; values: %v", len(results), results)
	}
	expected := []int64{1, 2, 3, 4}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_Reverse(t *testing.T) {
	t.Parallel()

	// Left:  id=5, id=3, id=1 (descending)
	// Right: id=6, id=4, id=2 (descending)
	// Expected merged DESC: 6,5,4,3,2,1
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(5)),
		qr("id", int64(3)),
		qr("id", int64(1)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(6)),
		qr("id", int64(4)),
		qr("id", int64(2)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		true,  // reverse (descending)
		false, // no dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}
	expected := []int64{6, 5, 4, 3, 2, 1}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_EmptyInputs(t *testing.T) {
	t.Parallel()

	// Both inputs empty.
	left := recordlayer.FromList([]QueryResult{})
	right := recordlayer.FromList([]QueryResult{})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestMergeSortCursor_ZeroCursors(t *testing.T) {
	t.Parallel()

	// No cursors at all.
	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		nil,
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestMergeSortCursor_SingleInputPassthrough(t *testing.T) {
	t.Parallel()

	// Single input: should just pass through in order.
	input := recordlayer.FromList([]QueryResult{
		qr("id", int64(10)),
		qr("id", int64(20)),
		qr("id", int64(30)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{input},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	expected := []int64{10, 20, 30}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_NullComparisonKeys(t *testing.T) {
	t.Parallel()

	// NULL values in the comparison key. compareValues treats nil < non-nil.
	// Left:  id=nil, id=3
	// Right: id=1, id=nil (note: not properly sorted but tests nil handling)
	//
	// With ascending: nil < 1 < 3 < nil-second
	// Since nil < any, left's nil comes first, then right's 1,
	// then left's 3, then right's nil.
	// But right's second nil is NOT less than 3 (nil < 3 = true),
	// so it should come before 3? Let's trace carefully.
	//
	// Actually left=[nil, 3], right=[1, nil]
	// Peek: left=nil, right=1. isBetter(nil, 1): compareValues(nil, 1)=-1, cmp<0 → true → pick left(nil)
	// Peek: left=3, right=1. isBetter(3, 1): compareValues(3, 1)=1, cmp<0 → false → pick right(1)
	// Peek: left=3, right=nil. isBetter(3, nil): compareValues(3, nil)=1, cmp<0 → false.
	//   isBetter(nil, 3): compareValues(nil, 3)=-1, cmp<0 → true → pick right(nil)
	// Peek: left=3, right exhausted. Pick left(3).
	// Result: nil, 1, nil, 3
	left := recordlayer.FromList([]QueryResult{
		qr("id", nil),
		qr("id", int64(3)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", nil),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}

	// Verify nil values come first and non-nil values are ordered.
	// Expected order: nil, 1, nil, 3
	m0 := results[0].Datum.(map[string]any)
	if m0["id"] != nil {
		t.Errorf("results[0].id = %v, want nil", m0["id"])
	}
	if fieldVal(t, results[1], "id") != 1 {
		t.Errorf("results[1].id = %v, want 1", results[1].Datum)
	}
	m2 := results[2].Datum.(map[string]any)
	if m2["id"] != nil {
		t.Errorf("results[2].id = %v, want nil", m2["id"])
	}
	if fieldVal(t, results[3], "id") != 3 {
		t.Errorf("results[3].id = %v, want 3", results[3].Datum)
	}
}

func TestMergeSortCursor_UnequalLengthInputs(t *testing.T) {
	t.Parallel()

	// Left:  id=1, id=5
	// Right: id=2, id=3, id=4, id=6, id=7
	// Expected merged ASC: 1,2,3,4,5,6,7
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(5)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(3)),
		qr("id", int64(4)),
		qr("id", int64(6)),
		qr("id", int64(7)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 7 {
		t.Fatalf("got %d results, want 7", len(results))
	}
	expected := []int64{1, 2, 3, 4, 5, 6, 7}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_DedupWithAllDuplicates(t *testing.T) {
	t.Parallel()

	// Both inputs have the same keys: 1,2,3
	// With dedup, should produce 1,2,3
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		true, // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3; values: %v", len(results), results)
	}
	expected := []int64{1, 2, 3}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_ThreeInputs(t *testing.T) {
	t.Parallel()

	// Three sorted inputs merged.
	a := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(4)),
		qr("id", int64(7)),
	})
	b := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(5)),
		qr("id", int64(8)),
	})
	ch := recordlayer.FromList([]QueryResult{
		qr("id", int64(3)),
		qr("id", int64(6)),
		qr("id", int64(9)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{a, b, ch},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 9 {
		t.Fatalf("got %d results, want 9", len(results))
	}
	for i := 0; i < 9; i++ {
		want := int64(i + 1)
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_MultipleComparisonKeys(t *testing.T) {
	t.Parallel()

	// Sort by (group ASC, id ASC). Both inputs sorted by (group, id).
	left := recordlayer.FromList([]QueryResult{
		qr("group", int64(1), "id", int64(1)),
		qr("group", int64(1), "id", int64(3)),
		qr("group", int64(2), "id", int64(1)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("group", int64(1), "id", int64(2)),
		qr("group", int64(2), "id", int64(2)),
		qr("group", int64(3), "id", int64(1)),
	})

	compKeys := []values.Value{
		values.NewFlatFieldValue("group", values.TypeInt),
		values.NewFlatFieldValue("id", values.TypeInt),
	}
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		compKeys,
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}

	type pair struct{ group, id int64 }
	expected := []pair{
		{1, 1}, {1, 2}, {1, 3}, {2, 1}, {2, 2}, {3, 1},
	}
	for i, want := range expected {
		gotG := fieldVal(t, results[i], "group")
		gotI := fieldVal(t, results[i], "id")
		if gotG != want.group || gotI != want.id {
			t.Errorf("results[%d] = (%d,%d), want (%d,%d)", i, gotG, gotI, want.group, want.id)
		}
	}
}

func TestMergeSortCursor_OneEmptyOneNonEmpty(t *testing.T) {
	t.Parallel()

	// Left is empty, right has data.
	left := recordlayer.FromList([]QueryResult{})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if fieldVal(t, results[0], "id") != 1 {
		t.Errorf("results[0].id = %d, want 1", fieldVal(t, results[0], "id"))
	}
	if fieldVal(t, results[1], "id") != 2 {
		t.Errorf("results[1].id = %d, want 2", fieldVal(t, results[1], "id"))
	}
}

func TestMergeSortCursor_StringComparisonKeys(t *testing.T) {
	t.Parallel()

	// Sort by string comparison key.
	left := recordlayer.FromList([]QueryResult{
		qr("name", "alice"),
		qr("name", "charlie"),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("name", "bob"),
		qr("name", "dave"),
	})

	compKey := values.NewFlatFieldValue("name", values.TypeString)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
	expectedNames := []string{"alice", "bob", "charlie", "dave"}
	for i, want := range expectedNames {
		m := results[i].Datum.(map[string]any)
		got := m["name"].(string)
		if got != want {
			t.Errorf("results[%d].name = %q, want %q", i, got, want)
		}
	}
}

func TestMergeSortCursor_CloseIdempotent(t *testing.T) {
	t.Parallel()

	input := recordlayer.FromList([]QueryResult{qr("id", int64(1))})
	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{input},
		[]values.Value{compKey},
		false,
		false,
	)

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !c.IsClosed() {
		t.Error("IsClosed = false after Close")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestMergeSortCursor_ReverseDedup(t *testing.T) {
	t.Parallel()

	// Reverse + dedup. Inputs sorted descending with overlaps.
	// Left:  id=5, id=3, id=1
	// Right: id=4, id=3, id=2
	// Merged DESC without dedup: 5,4,3,3,2,1
	// With dedup: 5,4,3,2,1
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(5)),
		qr("id", int64(3)),
		qr("id", int64(1)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(4)),
		qr("id", int64(3)),
		qr("id", int64(2)),
	})

	compKey := values.NewFlatFieldValue("id", values.TypeInt)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		true, // reverse
		true, // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5; values: %v", len(results), results)
	}
	expected := []int64{5, 4, 3, 2, 1}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// collectCursor drains any RecordCursor[QueryResult] and returns all results.
// ---------------------------------------------------------------------------

func collectCursor(t *testing.T, c recordlayer.RecordCursor[QueryResult]) []QueryResult {
	t.Helper()
	ctx := context.Background()
	var out []QueryResult
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext error: %v", err)
		}
		if !r.HasNext() {
			break
		}
		out = append(out, r.GetValue())
	}
	return out
}

// ===========================================================================
// aggregateCursor continuation round-trip
// ===========================================================================

func TestAggregateContinuation_RoundTrip_SumCount(t *testing.T) {
	t.Parallel()

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: values.NewFlatFieldValue("amount", values.TypeInt)},
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
	}

	gs := &groupState{
		keyVals: []any{int64(42), "group-a"},
		count:   7,
		counts:  []int64{5, 7},
		sums:    []float64{123.5, 0},
		sumsI:   []int64{100, 0},
		allInt:  []bool{false, true},
		mins:    []any{int64(1), nil},
		maxs:    []any{int64(50), nil},
	}

	innerCont := recordlayer.NewBytesContinuation([]byte{0xDE, 0xAD})
	groupKey := "test-group-key"

	encoded, err := encodeAggregateContinuation(innerCont, groupKey, gs.keyVals, gs, aggs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotGroupKey, gotGS, err := decodeAggregateContinuation(encoded, len(aggs))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(gotInner, []byte{0xDE, 0xAD}) {
		t.Errorf("innerContinuation = %x, want dead", gotInner)
	}
	if gotGroupKey != groupKey {
		t.Errorf("groupKey = %q, want %q", gotGroupKey, groupKey)
	}
	if gotGS == nil {
		t.Fatal("decoded groupState is nil")
	}
	if gotGS.count != 7 {
		t.Errorf("count = %d, want 7", gotGS.count)
	}
	if gotGS.counts[0] != 5 || gotGS.counts[1] != 7 {
		t.Errorf("counts = %v, want [5, 7]", gotGS.counts)
	}
	if gotGS.sums[0] != 123.5 {
		t.Errorf("sums[0] = %f, want 123.5", gotGS.sums[0])
	}
	if gotGS.sumsI[0] != 100 {
		t.Errorf("sumsI[0] = %d, want 100", gotGS.sumsI[0])
	}
	if gotGS.allInt[0] != false || gotGS.allInt[1] != true {
		t.Errorf("allInt = %v, want [false, true]", gotGS.allInt)
	}
	if gotGS.mins[0] != int64(1) {
		t.Errorf("mins[0] = %v, want 1", gotGS.mins[0])
	}
	if gotGS.maxs[0] != int64(50) {
		t.Errorf("maxs[0] = %v, want 50", gotGS.maxs[0])
	}

	// keyVals: JSON round-trips int64 through float64; the decoder
	// converts back to int64 when lossless.
	if len(gotGS.keyVals) != 2 {
		t.Fatalf("keyVals len = %d, want 2", len(gotGS.keyVals))
	}
	if gotGS.keyVals[0] != int64(42) {
		t.Errorf("keyVals[0] = %v (%T), want int64(42)", gotGS.keyVals[0], gotGS.keyVals[0])
	}
	if gotGS.keyVals[1] != "group-a" {
		t.Errorf("keyVals[1] = %v, want \"group-a\"", gotGS.keyVals[1])
	}
}

func TestAggregateContinuation_NilGroupState(t *testing.T) {
	t.Parallel()

	innerCont := recordlayer.NewBytesContinuation([]byte{0x01})
	encoded, err := encodeAggregateContinuation(innerCont, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotGroupKey, gotGS, err := decodeAggregateContinuation(encoded, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(gotInner, []byte{0x01}) {
		t.Errorf("innerContinuation = %x, want 01", gotInner)
	}
	if gotGroupKey != "" {
		t.Errorf("groupKey = %q, want empty", gotGroupKey)
	}
	if gotGS != nil {
		t.Errorf("expected nil groupState, got %+v", gotGS)
	}
}

func TestAggregateContinuation_FloatMinMax(t *testing.T) {
	t.Parallel()

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggMin, Operand: values.NewFlatFieldValue("price", values.TypeFloat)},
	}
	gs := &groupState{
		keyVals: []any{"x"},
		count:   3,
		counts:  []int64{3},
		sums:    []float64{9.75},
		sumsI:   []int64{0},
		allInt:  []bool{false},
		mins:    []any{float64(1.25)},
		maxs:    []any{float64(5.0)},
	}

	encoded, err := encodeAggregateContinuation(nil, "k", gs.keyVals, gs, aggs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, _, gotGS, err := decodeAggregateContinuation(encoded, 1)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fmt.Sprintf("%v", gotGS.mins[0]) != "1.25" {
		t.Errorf("mins[0] = %v, want 1.25", gotGS.mins[0])
	}
	if fmt.Sprintf("%v", gotGS.maxs[0]) != "5" && fmt.Sprintf("%v", gotGS.maxs[0]) != "5.0" {
		t.Errorf("maxs[0] = %v, want 5 or 5.0", gotGS.maxs[0])
	}
}

// ===========================================================================
// memorySortCursor continuation round-trip
// ===========================================================================

func TestSortContinuation_RoundTrip(t *testing.T) {
	t.Parallel()

	buf := []QueryResult{
		qr("name", "alice", "age", int64(30)),
		qr("name", "bob", "age", int64(25)),
		qr("name", "carol", "age", int64(35)),
	}
	// Give the second entry a PrimaryKey to verify PK round-trip.
	buf[1].PrimaryKey = tuple.Tuple{"pk", int64(2)}

	innerCont := recordlayer.NewBytesContinuation([]byte{0xCA, 0xFE})

	encoded, err := encodeSortContinuation(innerCont, buf)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotBuf, err := decodeSortContinuation(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(gotInner, []byte{0xCA, 0xFE}) {
		t.Errorf("innerContinuation = %x, want cafe", gotInner)
	}
	if len(gotBuf) != 3 {
		t.Fatalf("buf len = %d, want 3", len(gotBuf))
	}

	// Check datum values.
	for i, want := range []string{"alice", "bob", "carol"} {
		m := gotBuf[i].Datum.(map[string]any)
		if m["name"] != want {
			t.Errorf("buf[%d].name = %v, want %q", i, m["name"], want)
		}
	}
	// Ages: JSON round-trips through float64 → int64 conversion.
	for i, want := range []int64{30, 25, 35} {
		m := gotBuf[i].Datum.(map[string]any)
		if m["age"] != want {
			t.Errorf("buf[%d].age = %v (%T), want %d", i, m["age"], m["age"], want)
		}
	}

	// PrimaryKey round-trip.
	if gotBuf[1].PrimaryKey == nil {
		t.Fatal("buf[1].PrimaryKey is nil after round-trip")
	}
	if len(gotBuf[1].PrimaryKey) != 2 {
		t.Fatalf("buf[1].PrimaryKey len = %d, want 2", len(gotBuf[1].PrimaryKey))
	}
	if gotBuf[1].PrimaryKey[0] != "pk" {
		t.Errorf("buf[1].PrimaryKey[0] = %v, want \"pk\"", gotBuf[1].PrimaryKey[0])
	}
}

func TestSortContinuation_EmptyBuffer(t *testing.T) {
	t.Parallel()

	encoded, err := encodeSortContinuation(nil, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotBuf, err := decodeSortContinuation(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotInner != nil {
		t.Errorf("innerContinuation = %x, want nil", gotInner)
	}
	if len(gotBuf) != 0 {
		t.Errorf("buf len = %d, want 0", len(gotBuf))
	}
}

// ===========================================================================
// nljCursor — Close safety
// ===========================================================================

func TestNLJCursor_CloseIdempotent(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
	})
	inner := []QueryResult{qr("val", int64(10))}

	c := newNLJCursor(outer, inner, plans.JoinInner, "", "", nil, EmptyEvaluationContext())

	if c.IsClosed() {
		t.Fatal("cursor should not be closed before Close()")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !c.IsClosed() {
		t.Fatal("cursor should be closed after Close()")
	}

	// Second close must not panic or error.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !c.IsClosed() {
		t.Fatal("cursor should remain closed")
	}
}

func TestNLJCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{qr("id", int64(1))})
	inner := []QueryResult{qr("val", int64(10))}

	c := newNLJCursor(outer, inner, plans.JoinInner, "", "", nil, EmptyEvaluationContext())
	c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected error from OnNext after Close")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %q, want it to mention 'closed'", err)
	}
}

// ===========================================================================
// nljCursor — empty inner
// ===========================================================================

func TestNLJCursor_EmptyInner_InnerJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})

	c := newNLJCursor(outer, nil, plans.JoinInner, "", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("INNER join with empty inner: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyInner_LeftJoin(t *testing.T) {
	t.Parallel()

	outerRows := []QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	}
	outer := recordlayer.FromList(outerRows)

	c := newNLJCursor(outer, nil, plans.JoinLeftOuter, "L", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("LEFT join with empty inner: got %d results, want 2", len(results))
	}
}

func TestNLJCursor_EmptyInner_NotExists(t *testing.T) {
	t.Parallel()

	outerRows := []QueryResult{
		qr("id", int64(10)),
		qr("id", int64(20)),
	}
	outer := recordlayer.FromList(outerRows)

	c := newNLJCursor(outer, nil, plans.JoinNotExists, "O", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("NOT EXISTS with empty inner: got %d results, want 2", len(results))
	}
}

func TestNLJCursor_EmptyInner_Exists(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	})

	c := newNLJCursor(outer, nil, plans.JoinExists, "O", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("EXISTS with empty inner: got %d results, want 0", len(results))
	}
}

// ===========================================================================
// nljCursor — empty outer
// ===========================================================================

func TestNLJCursor_EmptyOuter_InnerJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1)), qr("val", int64(2))}

	c := newNLJCursor(outer, inner, plans.JoinInner, "", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("INNER join with empty outer: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyOuter_LeftJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1))}

	c := newNLJCursor(outer, inner, plans.JoinLeftOuter, "L", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("LEFT join with empty outer: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyOuter_CrossJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1)), qr("val", int64(2))}

	c := newNLJCursor(outer, inner, plans.JoinCross, "", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("CROSS join with empty outer: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyOuter_Exists(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1))}

	c := newNLJCursor(outer, inner, plans.JoinExists, "", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("EXISTS with empty outer: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyOuter_NotExists(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1))}

	c := newNLJCursor(outer, inner, plans.JoinNotExists, "", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("NOT EXISTS with empty outer: got %d results, want 0", len(results))
	}
}

// ===========================================================================
// nljCursor — basic INNER join producing correct cross product
// ===========================================================================

func TestNLJCursor_InnerJoin_CrossProduct(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("a", int64(1)),
		qr("a", int64(2)),
	})
	inner := []QueryResult{
		qr("b", int64(10)),
		qr("b", int64(20)),
	}

	c := newNLJCursor(outer, inner, plans.JoinInner, "", "", nil, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	// 2 outer x 2 inner = 4 results (no predicate filtering).
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
}

func TestNLJCursor_InnerJoin_PredicateFilters(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("x", int64(1)),
		qr("x", int64(2)),
	})
	inner := []QueryResult{
		qr("y", int64(10)),
		qr("y", int64(20)),
	}

	// Predicate that always rejects: INNER join should produce 0 rows.
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)}
	c := newNLJCursor(outer, inner, plans.JoinInner, "", "", preds, EmptyEvaluationContext())
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (predicate rejects all)", len(results))
	}
}

// ===========================================================================
// concatCursor
// ===========================================================================

func TestConcatCursor_MultipleCursors(t *testing.T) {
	t.Parallel()

	c1 := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	})
	c2 := recordlayer.FromList([]QueryResult{
		qr("id", int64(3)),
	})
	c3 := recordlayer.FromList([]QueryResult{
		qr("id", int64(4)),
		qr("id", int64(5)),
	})

	cc := newConcatCursor([]recordlayer.RecordCursor[QueryResult]{c1, c2, c3})
	defer cc.Close()

	results := collectCursor(t, cc)
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5", len(results))
	}
	for i, want := range []int64{1, 2, 3, 4, 5} {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestConcatCursor_EmptyFirst(t *testing.T) {
	t.Parallel()

	empty := recordlayer.FromList([]QueryResult{})
	nonempty := recordlayer.FromList([]QueryResult{
		qr("id", int64(7)),
		qr("id", int64(8)),
	})

	cc := newConcatCursor([]recordlayer.RecordCursor[QueryResult]{empty, nonempty})
	defer cc.Close()

	results := collectCursor(t, cc)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if fieldVal(t, results[0], "id") != 7 {
		t.Errorf("results[0].id = %d, want 7", fieldVal(t, results[0], "id"))
	}
}

func TestConcatCursor_AllEmpty(t *testing.T) {
	t.Parallel()

	e1 := recordlayer.FromList([]QueryResult{})
	e2 := recordlayer.FromList([]QueryResult{})

	cc := newConcatCursor([]recordlayer.RecordCursor[QueryResult]{e1, e2})
	defer cc.Close()

	results := collectCursor(t, cc)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestConcatCursor_NoCursors(t *testing.T) {
	t.Parallel()

	cc := newConcatCursor([]recordlayer.RecordCursor[QueryResult]{})
	defer cc.Close()

	results := collectCursor(t, cc)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestConcatCursor_CloseIdempotent(t *testing.T) {
	t.Parallel()

	c1 := recordlayer.FromList([]QueryResult{qr("id", int64(1))})
	c2 := recordlayer.FromList([]QueryResult{qr("id", int64(2))})

	cc := newConcatCursor([]recordlayer.RecordCursor[QueryResult]{c1, c2})

	if cc.IsClosed() {
		t.Fatal("should not be closed initially")
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !cc.IsClosed() {
		t.Fatal("should be closed after Close()")
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ===========================================================================
// memorySortCursor — end-to-end via constructor
// ===========================================================================

func TestMemorySortCursor_SortsCorrectly(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("NAME", "carol", "AGE", int64(35)),
		qr("NAME", "alice", "AGE", int64(30)),
		qr("NAME", "bob", "AGE", int64(25)),
	})

	c := newMemorySortCursor(inner, []string{"AGE"}, []bool{false}) // ASC
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	expected := []int64{25, 30, 35}
	for i, want := range expected {
		got := fieldVal(t, results[i], "AGE")
		if got != want {
			t.Errorf("results[%d].AGE = %d, want %d", i, got, want)
		}
	}
}

func TestMemorySortCursor_SortsDescending(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("X", int64(1)),
		qr("X", int64(3)),
		qr("X", int64(2)),
	})

	c := newMemorySortCursor(inner, []string{"X"}, []bool{true}) // DESC
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	expected := []int64{3, 2, 1}
	for i, want := range expected {
		got := fieldVal(t, results[i], "X")
		if got != want {
			t.Errorf("results[%d].X = %d, want %d", i, got, want)
		}
	}
}

func TestMemorySortCursor_EmptyInput(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{})
	c := newMemorySortCursor(inner, []string{"x"}, nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestMemorySortCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{qr("x", int64(1))})
	c := newMemorySortCursor(inner, []string{"x"}, nil)
	c.Close()

	result, err := c.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext after Close should not error, got: %v", err)
	}
	if result.HasNext() {
		t.Fatal("OnNext after Close should return no-next")
	}
}

// ===========================================================================
// aggregateCursor — end-to-end with real data
// ===========================================================================

func TestAggregateCursor_SingleGroup_CountStar(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("v", int64(10)),
		qr("v", int64(20)),
		qr("v", int64(30)),
	})

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
	}
	c := newAggregateCursor(inner, nil, aggs) // nil groupingKeys → scalar mode
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	m := results[0].Datum.(map[string]any)
	if m["COUNT(*)"] != int64(3) {
		t.Errorf("COUNT(*) = %v, want 3", m["COUNT(*)"])
	}
}

func TestAggregateCursor_ScalarOnEmpty(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{})
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}},
	}
	c := newAggregateCursor(inner, nil, aggs)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("scalar aggregate on empty input: got %d results, want 1", len(results))
	}
	m := results[0].Datum.(map[string]any)
	if m["COUNT(*)"] != int64(0) {
		t.Errorf("COUNT(*) on empty = %v, want 0", m["COUNT(*)"])
	}
}

func TestAggregateCursor_GroupedSum(t *testing.T) {
	t.Parallel()

	// Two groups: dept "A" (values 10, 20) and dept "B" (values 30).
	// Input MUST be sorted by grouping key.
	inner := recordlayer.FromList([]QueryResult{
		qr("dept", "A", "amount", int64(10)),
		qr("dept", "A", "amount", int64(20)),
		qr("dept", "B", "amount", int64(30)),
	})

	groupKeys := []values.Value{values.NewFlatFieldValue("dept", values.TypeString)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: values.NewFlatFieldValue("amount", values.TypeInt)},
	}
	c := newAggregateCursor(inner, groupKeys, aggs)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d groups, want 2", len(results))
	}

	m0 := results[0].Datum.(map[string]any)
	if m0["DEPT"] != "A" {
		t.Errorf("group 0 key = %v, want A", m0["DEPT"])
	}
	if m0["SUM(AMOUNT)"] != int64(30) {
		t.Errorf("group 0 SUM = %v, want 30", m0["SUM(AMOUNT)"])
	}

	m1 := results[1].Datum.(map[string]any)
	if m1["DEPT"] != "B" {
		t.Errorf("group 1 key = %v, want B", m1["DEPT"])
	}
	if m1["SUM(AMOUNT)"] != int64(30) {
		t.Errorf("group 1 SUM = %v, want 30", m1["SUM(AMOUNT)"])
	}
}

func TestAggregateCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{qr("x", int64(1))})
	c := newAggregateCursor(inner, nil, nil)
	c.Close()

	result, err := c.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext after Close should not error, got: %v", err)
	}
	if result.HasNext() {
		t.Fatal("OnNext after Close should return no-next")
	}
}

// ===========================================================================
// customSortCursor — pluggable comparator
// ===========================================================================

func TestCustomSortCursor_ReverseSort(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("N", int64(1)),
		qr("N", int64(3)),
		qr("N", int64(2)),
	})

	sortFn := func(buf []QueryResult) error {
		sortByKeys(buf, []string{"N"}, []bool{true})
		return nil
	}
	c := newCustomSortCursor(inner, sortFn)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, want := range []int64{3, 2, 1} {
		got := fieldVal(t, results[i], "N")
		if got != want {
			t.Errorf("results[%d].N = %d, want %d", i, got, want)
		}
	}
}

func TestCustomSortCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{qr("n", int64(1))})
	c := newCustomSortCursor(inner, func([]QueryResult) error { return nil })
	c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected error from OnNext after Close")
	}
}

func TestCustomSortCursor_BufferLimitExceeded(t *testing.T) {
	t.Parallel()

	// Create an inner cursor with more rows than the limit.
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	inner := recordlayer.FromList(rows)

	c := newCustomSortCursor(inner, func(buf []QueryResult) error {
		sortByKeys(buf, []string{"n"}, nil)
		return nil
	})
	c.maxBuf = 5 // limit to 5 rows
	defer c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected SortBufferExceededError")
	}
	var bufErr *SortBufferExceededError
	if !errors.As(err, &bufErr) {
		t.Fatalf("expected *SortBufferExceededError, got %T: %v", err, err)
	}
	if bufErr.Limit != 5 {
		t.Errorf("limit = %d, want 5", bufErr.Limit)
	}
	if bufErr.Rows != 5 {
		t.Errorf("rows = %d, want 5", bufErr.Rows)
	}
}

func TestMemorySortCursor_BufferLimitExceeded(t *testing.T) {
	t.Parallel()

	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	inner := recordlayer.FromList(rows)

	c := newMemorySortCursor(inner, []string{"n"}, nil)
	c.maxBuf = 5
	defer c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected SortBufferExceededError")
	}
	var bufErr *SortBufferExceededError
	if !errors.As(err, &bufErr) {
		t.Fatalf("expected *SortBufferExceededError, got %T: %v", err, err)
	}
	if bufErr.Limit != 5 {
		t.Errorf("limit = %d, want 5", bufErr.Limit)
	}
	if bufErr.Rows != 5 {
		t.Errorf("rows = %d, want 5", bufErr.Rows)
	}
}

// --- CollectAllBounded regression tests ---

func TestCollectAllBounded_UnderLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 5)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	results, err := CollectAllBounded(context.Background(), cursor, 10, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("got %d results, want 5", len(results))
	}
}

func TestCollectAllBounded_ExactlyAtLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	_, err := CollectAllBounded(context.Background(), cursor, 10, "test")
	if err == nil {
		t.Fatal("expected MaterializationLimitExceededError at exactly limit rows")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("expected *MaterializationLimitExceededError, got %T: %v", err, err)
	}
	if mlErr.Limit != 10 {
		t.Errorf("limit = %d, want 10", mlErr.Limit)
	}
	if mlErr.Context != "test" {
		t.Errorf("context = %q, want %q", mlErr.Context, "test")
	}
}

func TestCollectAllBounded_OverLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 20)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	_, err := CollectAllBounded(context.Background(), cursor, 10, "nested loop join inner side")
	if err == nil {
		t.Fatal("expected MaterializationLimitExceededError")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("expected *MaterializationLimitExceededError, got %T: %v", err, err)
	}
	if mlErr.Limit != 10 {
		t.Errorf("limit = %d, want 10", mlErr.Limit)
	}
	if mlErr.Context != "nested loop join inner side" {
		t.Errorf("context = %q, want %q", mlErr.Context, "nested loop join inner side")
	}
	if !strings.Contains(mlErr.Error(), "10 rows") {
		t.Errorf("error message missing row count: %q", mlErr.Error())
	}
	if !strings.Contains(mlErr.Error(), "adding an index") {
		t.Errorf("error message missing actionable advice: %q", mlErr.Error())
	}
}

func TestCollectAllBounded_EmptyCursor(t *testing.T) {
	t.Parallel()
	cursor := recordlayer.FromList([]QueryResult{})

	results, err := CollectAllBounded(context.Background(), cursor, 5, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestCollectAllBounded_LimitOne(t *testing.T) {
	t.Parallel()
	rows := []QueryResult{qr("n", int64(1)), qr("n", int64(2))}
	cursor := recordlayer.FromList(rows)

	_, err := CollectAllBounded(context.Background(), cursor, 1, "test")
	if err == nil {
		t.Fatal("expected MaterializationLimitExceededError with limit=1 and 2 rows")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("expected *MaterializationLimitExceededError, got %T: %v", err, err)
	}
	if mlErr.Limit != 1 {
		t.Errorf("limit = %d, want 1", mlErr.Limit)
	}
}

func TestCollectAllBounded_OneBelowLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 9)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	results, err := CollectAllBounded(context.Background(), cursor, 10, "test")
	if err != nil {
		t.Fatalf("unexpected error with 9 rows and limit 10: %v", err)
	}
	if len(results) != 9 {
		t.Errorf("got %d results, want 9", len(results))
	}
}

func TestMaterializationLimitExceededError_ErrorsAs(t *testing.T) {
	t.Parallel()
	orig := &MaterializationLimitExceededError{Limit: 42, Context: "buffered union branch"}
	wrapped := fmt.Errorf("executor: %w", orig)

	var mlErr *MaterializationLimitExceededError
	if !errors.As(wrapped, &mlErr) {
		t.Fatal("errors.As failed on wrapped MaterializationLimitExceededError")
	}
	if mlErr.Limit != 42 {
		t.Errorf("limit = %d, want 42", mlErr.Limit)
	}
	if mlErr.Context != "buffered union branch" {
		t.Errorf("context = %q, want %q", mlErr.Context, "buffered union branch")
	}
}

func TestGetMaterializationLimit_Default(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties()
	if props.GetMaterializationLimit() != recordlayer.DefaultMaterializationLimit {
		t.Errorf("default = %d, want %d", props.GetMaterializationLimit(), recordlayer.DefaultMaterializationLimit)
	}
}

func TestGetMaterializationLimit_Custom(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties().WithMaterializationLimit(500)
	if props.GetMaterializationLimit() != 500 {
		t.Errorf("custom = %d, want 500", props.GetMaterializationLimit())
	}
}

func TestGetMaterializationLimit_ZeroFallsBackToDefault(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties().WithMaterializationLimit(0)
	if props.GetMaterializationLimit() != recordlayer.DefaultMaterializationLimit {
		t.Errorf("zero = %d, want default %d", props.GetMaterializationLimit(), recordlayer.DefaultMaterializationLimit)
	}
}

func TestGetMaterializationLimit_NegativeFallsBackToDefault(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties().WithMaterializationLimit(-1)
	if props.GetMaterializationLimit() != recordlayer.DefaultMaterializationLimit {
		t.Errorf("negative = %d, want default %d", props.GetMaterializationLimit(), recordlayer.DefaultMaterializationLimit)
	}
}
