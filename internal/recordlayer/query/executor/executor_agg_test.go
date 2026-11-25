package executor

import (
	"context"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestAggMinMax_FloatNaNAndSignedZero pins F48: streaming MIN/MAX over FLOAT/DOUBLE
// must match Java's NumericAggregationValue MIN_D/MAX_D (= Math.min/Math.max), NOT a
// total-order comparator. NaN PROPAGATES into both extremes (order-independent) and
// -0.0 < +0.0. The old code used compareAny with native float </>, which left NaN
// order-dependently ignored and -0.0/+0.0 first-seen-wins.
// MIN and MAX are Math.min/Math.max line for line, so a NaN result is the NaN
// OPERAND, bits included, in either arrival order (RFC-257 WS-E 5.3: the
// target stores fff8000000000000 for MIN/MAX over 0.0/0.0's NaN, where Go once
// stored math.NaN()'s 7ff8000000000001). FLOAT keeps a signaling payload: it is
// never widened through a double.
func TestAggMinMax_ReturnsTheNaNOperandsBits(t *testing.T) {
	t.Parallel()
	for _, bits := range []uint64{0xfff8000000000000, 0x7ff8000000000000, 0x7ff0000000000001, 0xfff00000deadbeef} {
		nan := math.Float64frombits(bits)
		for _, other := range []float64{1.0, math.Inf(1), math.Inf(-1), math.Copysign(0, -1)} {
			for _, isMin := range []bool{true, false} {
				for _, order := range [][2]float64{{nan, other}, {other, nan}} {
					got := aggMinMax(aggMinMax(nil, order[0], isMin), order[1], isMin).(float64)
					if math.Float64bits(got) != bits {
						t.Errorf("min=%t over %v: %016x, want the NaN operand %016x", isMin, order, math.Float64bits(got), bits)
					}
				}
			}
		}
	}
	for _, bits := range []uint32{0xffc00000, 0x7fc00000, 0x7f800001} {
		nan := math.Float32frombits(bits)
		for _, isMin := range []bool{true, false} {
			for _, order := range [][2]float32{{nan, 2}, {2, nan}} {
				got := aggMinMax(aggMinMax(nil, order[0], isMin), order[1], isMin).(float32)
				if math.Float32bits(got) != bits {
					t.Errorf("float32 min=%t over %v: %08x, want the NaN operand %08x", isMin, order, math.Float32bits(got), bits)
				}
			}
		}
	}
	// Two NaN operands: Java returns the first (`if (a != a) return a`).
	a, b := math.Float64frombits(0xfff8000000000000), math.Float64frombits(0x7ff8000000000000)
	if got := javaMinF64(a, b); math.Float64bits(got) != 0xfff8000000000000 {
		t.Errorf("Math.min(NaN a, NaN b) = %016x, want a", math.Float64bits(got))
	}
	if got := javaMaxF64(b, a); math.Float64bits(got) != 0x7ff8000000000000 {
		t.Errorf("Math.max(NaN b, NaN a) = %016x, want b", math.Float64bits(got))
	}
}

func TestAggMinMax_FloatNaNAndSignedZero(t *testing.T) {
	t.Parallel()
	nan := math.NaN()

	// NaN propagates into MAX regardless of arrival order (native </> would keep 2.0
	// when NaN arrives second).
	if got := aggMinMax(aggMinMax(nil, 2.0, false), nan, false).(float64); !math.IsNaN(got) {
		t.Fatalf("MAX(2.0, NaN[second]) = %v, want NaN (Java Math.max propagates)", got)
	}
	if got := aggMinMax(aggMinMax(nil, nan, false), 2.0, false).(float64); !math.IsNaN(got) {
		t.Fatalf("MAX(NaN[first], 2.0) = %v, want NaN", got)
	}
	// NaN propagates into MIN too — must be NaN, NOT the smallest finite (this is why
	// CompareFloat64's NaN-greatest total order is WRONG for MIN).
	if got := aggMinMax(aggMinMax(nil, 2.0, true), nan, true).(float64); !math.IsNaN(got) {
		t.Fatalf("MIN(2.0, NaN[second]) = %v, want NaN (Java Math.min propagates, not smallest-finite)", got)
	}
	if got := aggMinMax(aggMinMax(nil, nan, true), 2.0, true).(float64); !math.IsNaN(got) {
		t.Fatalf("MIN(NaN[first], 2.0) = %v, want NaN", got)
	}

	// Signed zero: MIN(-0.0,+0.0) = -0.0, MAX(-0.0,+0.0) = +0.0 (native </> keeps
	// first-seen since -0.0 == +0.0).
	negZero := math.Copysign(0, -1)
	if got := aggMinMax(aggMinMax(nil, 0.0, true), negZero, true).(float64); !math.Signbit(got) {
		t.Fatalf("MIN(+0.0[first], -0.0) = %v (signbit=%v), want -0.0", got, math.Signbit(got))
	}
	if got := aggMinMax(aggMinMax(nil, negZero, false), 0.0, false).(float64); math.Signbit(got) {
		t.Fatalf("MAX(-0.0[first], +0.0) = %v (signbit=%v), want +0.0", got, math.Signbit(got))
	}

	// float32 mirrors (MIN_F/MAX_F).
	nan32 := float32(math.NaN())
	if got := aggMinMax(aggMinMax(nil, float32(2), false), nan32, false).(float32); !math.IsNaN(float64(got)) {
		t.Fatalf("float32 MAX(2, NaN) = %v, want NaN", got)
	}
	if got := aggMinMax(aggMinMax(nil, float32(2), true), nan32, true).(float32); !math.IsNaN(float64(got)) {
		t.Fatalf("float32 MIN(2, NaN) = %v, want NaN", got)
	}
	negZero32 := float32(math.Copysign(0, -1))
	if got := aggMinMax(aggMinMax(nil, float32(0), true), negZero32, true).(float32); !math.Signbit(float64(got)) {
		t.Fatalf("float32 MIN(+0.0, -0.0) = %v, want -0.0", got)
	}

	// Integer path (int64/int32/int, no NaN/signed-zero — Java MIN_I/MIN_L/MAX_I/MAX_L).
	if got := aggMinMax(aggMinMax(nil, int64(5), true), int64(3), true).(int64); got != 3 {
		t.Fatalf("MIN(5,3) = %d, want 3", got)
	}
	if got := aggMinMax(aggMinMax(nil, int64(5), false), int64(3), false).(int64); got != 5 {
		t.Fatalf("MAX(5,3) = %d, want 5", got)
	}
	// int32 and plain int widen to int64 (asInt64) and order correctly.
	if got := aggMinMax(aggMinMax(nil, int32(5), true), int32(3), true).(int32); got != 3 {
		t.Fatalf("MIN(int32 5,3) = %d, want 3", got)
	}
	if got := aggMinMax(aggMinMax(nil, int(5), false), int(3), false).(int); got != 5 {
		t.Fatalf("MAX(int 5,3) = %d, want 5", got)
	}
}

// TestAggMinMax_MixedNumericOperands pins the unpromoted-CASE regression: an
// operand like `MIN(CASE WHEN c THEN dbl_col ELSE 0 END)` delivers int64 on ELSE
// rows and float64 on THEN rows. Java never sees the mix (its encapsulation wraps
// the operand in PromoteValue, so MIN_D receives doubles only); Go promotes the
// pair inside aggMinMax to the widest float type present, computing exactly what
// Java computes over the promoted operand. Pre-fix, acc=int64 + val=float64
// PANICKED on the acc.(float64) assertion, and the opposite order SILENTLY
// DROPPED the int value (asInt64(float64) failed and the fold returned acc).
func TestAggMinMax_MixedNumericOperands(t *testing.T) {
	t.Parallel()

	// The panic order: acc int64 (ELSE first), val float64 (THEN second).
	if got := aggMinMax(int64(0), float64(1.5), false).(float64); got != 1.5 {
		t.Fatalf("MAX(int64(0), 1.5) = %v, want 1.5 (pre-fix: panic on acc.(float64))", got)
	}
	if got := aggMinMax(int64(0), float64(1.5), true).(float64); got != 0.0 {
		t.Fatalf("MIN(int64(0), 1.5) = %v, want 0 (double-promoted)", got)
	}
	// The silent-drop order: acc float64, val int64 — val must win MIN, not vanish.
	if got := aggMinMax(float64(1.5), int64(1), true).(float64); got != 1.0 {
		t.Fatalf("MIN(1.5, int64(1)) = %v, want 1 (pre-fix: int silently dropped, returned 1.5)", got)
	}
	if got := aggMinMax(float64(1.5), int64(7), false).(float64); got != 7.0 {
		t.Fatalf("MAX(1.5, int64(7)) = %v, want 7", got)
	}
	// NaN propagates through a mixed pair too (Java: promoted double NaN).
	if got := aggMinMax(int64(3), math.NaN(), true).(float64); !math.IsNaN(got) {
		t.Fatalf("MIN(int64(3), NaN) = %v, want NaN", got)
	}
	// FLOAT lane: float32 + int promotes to float32 (Java MIN_F over the
	// float-promoted operand), not float64.
	if got := aggMinMax(float32(2.5), int64(1), true).(float32); got != 1.0 {
		t.Fatalf("MIN(float32(2.5), int64(1)) = %v, want float32(1)", got)
	}
	if got := aggMinMax(int32(4), float32(2.5), false).(float32); got != 4.0 {
		t.Fatalf("MAX(int32(4), float32(2.5)) = %v, want float32(4)", got)
	}
	// Mixed float widths promote to DOUBLE (the float64 lane claims first).
	if got := aggMinMax(float32(2.5), float64(1.25), true).(float64); got != 1.25 {
		t.Fatalf("MIN(float32(2.5), float64(1.25)) = %v, want 1.25 (double lane)", got)
	}
	// Once promoted, the accumulator stays float across subsequent int rows.
	acc := aggMinMax(aggMinMax(aggMinMax(nil, int64(3), true), float64(1.5), true), int64(1), true)
	if got := acc.(float64); got != 1.0 {
		t.Fatalf("MIN over {3, 1.5, 1} mixed = %v, want 1.0", got)
	}
}

func TestExecuteStreamingAggregation_CountGroupBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))

	groupKeys := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
	}

	plan := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(inner, groupKeys, aggs))
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
	row, _ := rowMapOK(results[0])
	if row["COUNT(CONSTANT)"] != int64(1) {
		t.Errorf("COUNT = %v, want 1", row["COUNT(CONSTANT)"])
	}
}

func TestExecuteStreamingAggregation_NoGroups_Count(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
		{Function: expressions.AggSum, Operand: &values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
	}

	plan := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(inner, nil, aggs))
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
	row, _ := rowMapOK(results[0])
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

	inner := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
			&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
		})),
	))

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)}},
	}

	plan := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(inner, nil, aggs))
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
	row, _ := rowMapOK(results[0])
	if row["COUNT(CONSTANT)"] != int64(0) {
		t.Errorf("COUNT(empty) = %v, want 0", row["COUNT(CONSTANT)"])
	}
}

// --- asFloat64 unit tests (the SUM/AVG operand conversion) ---

// Every numeric carrier widens exactly; anything else is refused, never
// turned into a NaN that a SUM would then store (RFC-257 WS-E 5.3).
func TestAsFloat64(t *testing.T) {
	t.Parallel()
	for in, want := range map[any]float64{int64(42): 42, 3.14: 3.14, int(7): 7, int32(100): 100, float32(0.5): 0.5} {
		if v, ok := asFloat64(in); !ok || v != want {
			t.Errorf("asFloat64(%#v) = %v, %v; want %v", in, v, ok, want)
		}
	}
	for _, in := range []any{"hello", nil, true} {
		if v, ok := asFloat64(in); ok {
			t.Errorf("asFloat64(%#v) = %v, accepted", in, v)
		}
	}
}

// --- aggKeyName unit tests ---

func TestAggKeyName_FieldValue(t *testing.T) {
	t.Parallel()
	fv := mustNamedTestField(t, "status", values.TypeString)
	if got := aggKeyName(fv); got != "status" {
		t.Fatalf("expected status, got %s", got)
	}
}

func TestAggKeyName_NonFieldValue(t *testing.T) {
	t.Parallel()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}
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
		Function:    expressions.AggCount,
		Operand:     mustNamedTestField(t, "id", values.NullableLong),
		OperandName: "ID",
	}
	if got := aggResultName(agg); got != "COUNT(ID)" {
		t.Fatalf("expected COUNT(ID), got %s", got)
	}
}

func TestAggResultName_Sum(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function:    expressions.AggSum,
		Operand:     mustNamedTestField(t, "price", values.NullableLong),
		OperandName: "PRICE",
	}
	if got := aggResultName(agg); got != "SUM(PRICE)" {
		t.Fatalf("expected SUM(PRICE), got %s", got)
	}
}

func TestAggResultName_Min(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function:    expressions.AggMin,
		Operand:     mustNamedTestField(t, "price", values.NullableLong),
		OperandName: "PRICE",
	}
	if got := aggResultName(agg); got != "MIN(PRICE)" {
		t.Fatalf("expected MIN(PRICE), got %s", got)
	}
}

func TestAggResultName_Max(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function:    expressions.AggMax,
		Operand:     mustNamedTestField(t, "price", values.NullableLong),
		OperandName: "PRICE",
	}
	if got := aggResultName(agg); got != "MAX(PRICE)" {
		t.Fatalf("expected MAX(PRICE), got %s", got)
	}
}

func TestAggResultName_Avg(t *testing.T) {
	t.Parallel()
	agg := expressions.AggregateSpec{
		Function:    expressions.AggAvg,
		Operand:     mustNamedTestField(t, "price", values.NullableLong),
		OperandName: "PRICE",
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
		Function:    expressions.AggregateFunction(99),
		Operand:     mustNamedTestField(t, "x", values.NullableLong),
		OperandName: "X",
	}
	if got := aggResultName(agg); got != "AGG(X)" {
		t.Fatalf("expected AGG(X), got %s", got)
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
	c := newAggregateCursor(inner, nil, aggs, nil, nil) // nil groupingKeys → scalar mode
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	m, _ := rowMapOK(results[0])
	if m["COUNT(*)"] != int64(3) {
		t.Errorf("COUNT(*) = %v, want 3", m["COUNT(*)"])
	}
}

func TestAggregateCursor_ScalarOnEmpty(t *testing.T) {
	t.Parallel()

	// Java AggregateCursor.isNoRecords(): the BARE streaming-aggregate cursor
	// emits NOTHING on empty input — RecordCursorResult.exhausted(). The
	// COUNT(*)=0 default row is executeAggregation's OrElse ALTERNATIVE
	// (emptyScalarAggregateRow — Java's DefaultOnEmpty(StreamingAggregation)),
	// so a resume landing past the last input row can never re-fabricate it.
	inner := recordlayer.FromList([]QueryResult{})
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}},
	}
	c := newAggregateCursor(inner, nil, aggs, nil, nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("bare scalar aggregate cursor on empty input: got %d results, want 0 (Java isNoRecords)", len(results))
	}

	// The default row itself: the OrElse alternative's single-row payload.
	m, _ := rowMapOK(emptyScalarAggregateRow(aggs, nil))
	if m["COUNT(*)"] != int64(0) {
		t.Errorf("COUNT(*) default row = %v, want 0", m["COUNT(*)"])
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

	rowType := exactTestRowType(
		values.Field{Name: "amount", FieldType: values.NullableLong},
		values.Field{Name: "dept", FieldType: values.TypeString},
	)
	root := mustTestQOV(t, values.UniqueCorrelationIdentifier(), rowType)
	groupKeys := []values.Value{mustTestFieldOrdinal(t, root, 1)}
	aggs := []expressions.AggregateSpec{
		{
			Function:    expressions.AggSum,
			Operand:     mustTestFieldOrdinal(t, root, 0),
			OperandName: "AMOUNT",
		},
	}
	c := newAggregateCursor(inner, groupKeys, aggs, nil, nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d groups, want 2", len(results))
	}

	m0, _ := rowMapOK(results[0])
	if m0["dept"] != "A" {
		t.Errorf("group 0 key = %v, want A", m0["dept"])
	}
	if m0["SUM(AMOUNT)"] != int64(30) {
		t.Errorf("group 0 SUM = %v, want 30", m0["SUM(AMOUNT)"])
	}

	m1, _ := rowMapOK(results[1])
	if m1["dept"] != "B" {
		t.Errorf("group 1 key = %v, want B", m1["dept"])
	}
	if m1["SUM(AMOUNT)"] != int64(30) {
		t.Errorf("group 1 SUM = %v, want 30", m1["SUM(AMOUNT)"])
	}
}

func TestAggregateCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{qr("x", int64(1))})
	c := newAggregateCursor(inner, nil, nil, nil, nil)
	c.Close()

	result, err := c.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext after Close should not error, got: %v", err)
	}
	if result.HasNext() {
		t.Fatal("OnNext after Close should return no-next")
	}
}
