package values

import (
	"math"
	"testing"
)

// Static interface assertions.
var (
	_ Value = (*ConstantValue)(nil)
	_ Value = (*FieldValue)(nil)
	_ Value = (*ArithmeticValue)(nil)
	_ Value = (*BooleanValue)(nil)
	_ Value = (*CastValue)(nil)
	_ Value = (*NullValue)(nil)
)

func TestExplainValue(t *testing.T) {
	t.Parallel()
	if got := ExplainValue(nil); got != "" {
		t.Fatalf("nil: got %q", got)
	}
	if got := ExplainValue(&ConstantValue{Value: int64(42), Typ: TypeInt}); got != "42" {
		t.Fatalf("int const: got %q", got)
	}
	if got := ExplainValue(&ConstantValue{Value: "x", Typ: TypeString}); got != "'x'" {
		t.Fatalf("string const: got %q", got)
	}
	if got := ExplainValue(&ConstantValue{Value: nil, Typ: TypeInt}); got != "NULL" {
		t.Fatalf("NULL const: got %q", got)
	}
	if got := ExplainValue(&FieldValue{Field: "age", Typ: TypeInt}); got != "age" {
		t.Fatalf("field: got %q", got)
	}
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "a", Typ: TypeInt},
		Right: &ConstantValue{Value: int64(5), Typ: TypeInt},
	}
	if got := ExplainValue(arith); got != "(a + 5)" {
		t.Fatalf("arith: got %q", got)
	}
	if got := ExplainValue(NewBooleanValue(true)); got != "TRUE" {
		t.Fatalf("bool TRUE: got %q", got)
	}
	if got := ExplainValue(NewBooleanValue(false)); got != "FALSE" {
		t.Fatalf("bool FALSE: got %q", got)
	}
	if got := ExplainValue(&BooleanValue{Value: nil}); got != "NULL" {
		t.Fatalf("bool NULL: got %q", got)
	}
	if got := ExplainValue(NewNullValue(TypeString)); got != "NULL" {
		t.Fatalf("NullValue: got %q", got)
	}
	cast := NewCastValue(&ConstantValue{Value: int64(1), Typ: TypeInt}, TypeString)
	if got := ExplainValue(cast); got != "CAST(1 AS STRING)" {
		t.Fatalf("cast: got %q", got)
	}
}

func TestNullValue(t *testing.T) {
	t.Parallel()
	nv := NewNullValue(TypeInt)
	if nv.Type().Code() != TypeCodeLong {
		t.Fatal("Type should match constructor")
	}
	if nv.Name() != "null" {
		t.Fatal("Name should be 'null'")
	}
	if got := mustEvaluate(nv, nil); got != nil {
		t.Fatalf("Evaluate: expected nil, got %v", got)
	}
	// Any context — NULL is context-independent.
	if got := mustEvaluate(nv, map[string]any{"x": 1}); got != nil {
		t.Fatalf("Evaluate w/ ctx: expected nil, got %v", got)
	}
	if len(nv.Children()) != 0 {
		t.Fatal("NullValue.Children: expected 0")
	}
}

func TestConstantValue_Evaluate(t *testing.T) {
	t.Parallel()
	c := &ConstantValue{Value: int64(42), Typ: TypeInt}
	if got := mustEvaluate(c, nil); got != int64(42) {
		t.Fatalf("constant int: got %v", got)
	}
	// Context is ignored for constants.
	if got := mustEvaluate(c, map[string]any{"x": 1}); got != int64(42) {
		t.Fatalf("constant ignores ctx: got %v", got)
	}
	// NULL literal.
	null := &ConstantValue{Value: nil, Typ: TypeInt}
	if got := mustEvaluate(null, nil); got != nil {
		t.Fatalf("NULL literal: got %v", got)
	}
}

func TestFieldValue_Evaluate(t *testing.T) {
	t.Parallel()
	f := &FieldValue{Field: "name", Typ: TypeString}
	row := map[string]any{"name": "Alice", "age": int64(30)}
	if got := mustEvaluate(f, row); got != "Alice" {
		t.Fatalf("field lookup: got %v", got)
	}
	// Missing field: NULL.
	missing := &FieldValue{Field: "nope", Typ: TypeString}
	if got := mustEvaluate(missing, row); got != nil {
		t.Fatalf("missing field: got %v", got)
	}
	// nil ctx.
	if got := mustEvaluate(f, nil); got != nil {
		t.Fatalf("nil ctx: got %v", got)
	}
	// Wrong ctx type.
	if got := mustEvaluate(f, "not a map"); got != nil {
		t.Fatalf("wrong ctx type: got %v", got)
	}
}

func TestArithmeticValue_Evaluate(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}

	cases := []struct {
		op   ArithmeticOp
		a, b int64
		want any
	}{
		{OpAdd, 3, 4, int64(7)},
		{OpSub, 10, 3, int64(7)},
		{OpMul, 6, 7, int64(42)},
		{OpDiv, 20, 4, int64(5)},
		{OpMod, 20, 7, int64(6)},
		{OpMod, -20, 7, int64(-6)}, // Go truncated-toward-zero, matches MySQL/PostgreSQL
	}
	for _, tc := range cases {
		av := &ArithmeticValue{Op: tc.op, Left: a, Right: b}
		got := mustEvaluate(av, map[string]any{"a": tc.a, "b": tc.b})
		if got != tc.want {
			t.Fatalf("op %v: got %v, want %v", tc.op, got, tc.want)
		}
	}

	// Division by zero panics with ArithmeticDivisionByZeroError
	// (matches Java's ArithmeticException; executor recovers it).
	divZ := &ArithmeticValue{Op: OpDiv, Left: a, Right: b}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("div by zero: expected panic")
			} else if _, ok := r.(*ArithmeticDivisionByZeroError); !ok {
				t.Fatalf("div by zero: expected *ArithmeticDivisionByZeroError, got %T", r)
			}
		}()
		mustEvaluate(divZ, map[string]any{"a": int64(5), "b": int64(0)})
	}()

	// MOD by zero same panic contract as Div.
	modZ := &ArithmeticValue{Op: OpMod, Left: a, Right: b}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("mod by zero: expected panic")
			} else if _, ok := r.(*ArithmeticDivisionByZeroError); !ok {
				t.Fatalf("mod by zero: expected *ArithmeticDivisionByZeroError, got %T", r)
			}
		}()
		mustEvaluate(modZ, map[string]any{"a": int64(5), "b": int64(0)})
	}()

	// NULL propagation.
	sum := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := mustEvaluate(sum, map[string]any{"a": nil, "b": int64(1)}); got != nil {
		t.Fatalf("NULL lhs: got %v", got)
	}
	if got := mustEvaluate(sum, map[string]any{"a": int64(1), "b": nil}); got != nil {
		t.Fatalf("NULL rhs: got %v", got)
	}

	// Type mismatch panics with ScalarTypeMismatchError (Java-aligned).
	tm := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("type mismatch: expected panic")
			} else if _, ok := r.(*ScalarTypeMismatchError); !ok {
				t.Fatalf("type mismatch: expected *ScalarTypeMismatchError, got %T: %v", r, r)
			}
		}()
		mustEvaluate(tm, map[string]any{"a": "foo", "b": int64(1)})
	}()

	// Float arithmetic returns nil per the seed contract — int-only
	// Evaluate, full coercion waits on the Phase 4.0 Type hierarchy
	// Float arithmetic: both float or mixed int+float → float promotion.
	floatOp := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := mustEvaluate(floatOp, map[string]any{"a": float64(1.5), "b": float64(2.5)}); got != float64(4) {
		t.Fatalf("float arith: got %v, want 4.0", got)
	}
	mixedOp := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := mustEvaluate(mixedOp, map[string]any{"a": int64(1), "b": float64(2.5)}); got != float64(3.5) {
		t.Fatalf("mixed int/float arith: got %v, want 3.5", got)
	}
}

func TestBooleanValue(t *testing.T) {
	t.Parallel()
	tv := NewBooleanValue(true)
	if got := mustEvaluate(tv, nil); got != true {
		t.Fatalf("true literal: got %v", got)
	}
	fv := NewBooleanValue(false)
	if got := mustEvaluate(fv, nil); got != false {
		t.Fatalf("false literal: got %v", got)
	}
	// UNKNOWN literal.
	uv := &BooleanValue{Value: nil}
	if got := mustEvaluate(uv, nil); got != nil {
		t.Fatalf("UNKNOWN literal: got %v", got)
	}
	if tv.Type().Code() != TypeCodeBoolean {
		t.Fatal("BooleanValue.Type should be a boolean type")
	}
}

func TestCastValue(t *testing.T) {
	t.Parallel()
	// int → string. Pin both positive and negative — pre-fix the
	// negative path used `uitoa(uint64(i))` which reinterprets a
	// signed int64 as the corresponding unsigned, producing
	// "18446744073709551611" for -5 instead of "-5".
	strC := NewCastValue(&ConstantValue{Value: int64(42), Typ: TypeInt}, TypeString)
	if got := mustEvaluate(strC, nil); got != "42" {
		t.Fatalf("int→string: got %v", got)
	}
	negStrC := NewCastValue(&ConstantValue{Value: int64(-5), Typ: TypeInt}, TypeString)
	if got := mustEvaluate(negStrC, nil); got != "-5" {
		t.Fatalf("negative int→string: got %v, want \"-5\" (regression for uitoa(uint64(int64))) bug", got)
	}
	zeroStrC := NewCastValue(&ConstantValue{Value: int64(0), Typ: TypeInt}, TypeString)
	if got := mustEvaluate(zeroStrC, nil); got != "0" {
		t.Fatalf("zero→string: got %v", got)
	}
	minStrC := NewCastValue(&ConstantValue{Value: int64(-9223372036854775808), Typ: TypeInt}, TypeString)
	if got := mustEvaluate(minStrC, nil); got != "-9223372036854775808" {
		t.Fatalf("MIN_INT64→string: got %v", got)
	}

	// bool → int: true=1, false=0.
	boolToInt := NewCastValue(NewBooleanValue(true), TypeInt)
	if got := mustEvaluate(boolToInt, nil); got != int64(1) {
		t.Fatalf("true→int: got %v", got)
	}
	boolToInt = NewCastValue(NewBooleanValue(false), TypeInt)
	if got := mustEvaluate(boolToInt, nil); got != int64(0) {
		t.Fatalf("false→int: got %v", got)
	}

	// int → bool: 0=false, non-zero=true.
	intToBool := NewCastValue(&ConstantValue{Value: int64(0), Typ: TypeInt}, TypeBool)
	if got := mustEvaluate(intToBool, nil); got != false {
		t.Fatalf("0→bool: got %v", got)
	}
	intToBool = NewCastValue(&ConstantValue{Value: int64(7), Typ: TypeInt}, TypeBool)
	if got := mustEvaluate(intToBool, nil); got != true {
		t.Fatalf("7→bool: got %v", got)
	}

	// NULL propagates.
	nullC := NewCastValue(&ConstantValue{Value: nil, Typ: TypeInt}, TypeString)
	if got := mustEvaluate(nullC, nil); got != nil {
		t.Fatalf("NULL cast: got %v", got)
	}

	// Float source casts.
	// int → float
	intToFloat := NewCastValue(&ConstantValue{Value: int64(5), Typ: TypeInt}, TypeFloat)
	if got := mustEvaluate(intToFloat, nil); got != float64(5) {
		t.Fatalf("int→float: got %v", got)
	}
	// float → int (Java Math.round: floor(x+0.5))
	floatToInt := NewCastValue(&ConstantValue{Value: float64(3.9), Typ: TypeFloat}, TypeInt)
	if got := mustEvaluate(floatToInt, nil); got != int64(4) {
		t.Fatalf("3.9→int: got %v, want 4", got)
	}
	floatToIntNeg := NewCastValue(&ConstantValue{Value: float64(-3.9), Typ: TypeFloat}, TypeInt)
	if got := mustEvaluate(floatToIntNeg, nil); got != int64(-4) {
		t.Fatalf("-3.9→int: got %v, want -4", got)
	}
	// float → bool: 0.0 = false, non-zero = true
	floatToBool0 := NewCastValue(&ConstantValue{Value: float64(0), Typ: TypeFloat}, TypeBool)
	if got := mustEvaluate(floatToBool0, nil); got != false {
		t.Fatalf("0.0→bool: got %v", got)
	}
	floatToBoolNZ := NewCastValue(&ConstantValue{Value: float64(0.5), Typ: TypeFloat}, TypeBool)
	if got := mustEvaluate(floatToBoolNZ, nil); got != true {
		t.Fatalf("0.5→bool: got %v", got)
	}
	// float → string
	floatToStr := NewCastValue(&ConstantValue{Value: float64(3.14), Typ: TypeFloat}, TypeString)
	if got := mustEvaluate(floatToStr, nil); got != "3.14" {
		t.Fatalf("3.14→string: got %v", got)
	}
	// float → float (verbatim)
	floatToFloat := NewCastValue(&ConstantValue{Value: float64(2.5), Typ: TypeFloat}, TypeFloat)
	if got := mustEvaluate(floatToFloat, nil); got != float64(2.5) {
		t.Fatalf("float→float: got %v", got)
	}
	// NaN / Inf → panic with InvalidCastError for int target.
	for _, tc := range []struct {
		name string
		val  float64
	}{
		{"NaN→int", math.NaN()},
		{"+Inf→int", math.Inf(1)},
		{"-Inf→int", math.Inf(-1)},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s: expected panic, got nil", tc.name)
				}
				if _, ok := r.(*InvalidCastError); !ok {
					t.Fatalf("%s: expected InvalidCastError, got %T", tc.name, r)
				}
			}()
			cv := NewCastValue(&ConstantValue{Value: tc.val, Typ: TypeFloat}, TypeInt)
			mustEvaluate(cv, nil)
		}()
	}

	// Unknown conversion: int → bool via the reverse path is OK,
	// string → int: trims whitespace, parses decimal.
	strToInt := NewCastValue(&ConstantValue{Value: "3", Typ: TypeString}, TypeInt)
	if got := mustEvaluate(strToInt, nil); got != int64(3) {
		t.Fatalf("string→int: got %v, want 3", got)
	}
	strToIntWs := NewCastValue(&ConstantValue{Value: "  42  ", Typ: TypeString}, TypeInt)
	if got := mustEvaluate(strToIntWs, nil); got != int64(42) {
		t.Fatalf("string(ws)→int: got %v, want 42", got)
	}

	// bool → string. Match runtime functions.CastValue: lowercase.
	// Pre-this-shift the fold path returned nil while the runtime
	// returned "true"/"false" — fold-vs-runtime divergence on a
	// constant input.
	boolToStrTrue := NewCastValue(NewBooleanValue(true), TypeString)
	if got := mustEvaluate(boolToStrTrue, nil); got != "true" {
		t.Fatalf("TRUE→string: got %v, want \"true\"", got)
	}
	boolToStrFalse := NewCastValue(NewBooleanValue(false), TypeString)
	if got := mustEvaluate(boolToStrFalse, nil); got != "false" {
		t.Fatalf("FALSE→string: got %v, want \"false\"", got)
	}
	// bool → float. Mirrors runtime's CAST(b AS INT) AS FLOAT chain
	// in one step (TRUE→1.0, FALSE→0.0).
	boolToFloatT := NewCastValue(NewBooleanValue(true), TypeFloat)
	if got := mustEvaluate(boolToFloatT, nil); got != float64(1) {
		t.Fatalf("TRUE→float: got %v, want 1", got)
	}
	boolToFloatF := NewCastValue(NewBooleanValue(false), TypeFloat)
	if got := mustEvaluate(boolToFloatF, nil); got != float64(0) {
		t.Fatalf("FALSE→float: got %v, want 0", got)
	}

	// Children wiring + Type.
	if len(strC.Children()) != 1 {
		t.Fatalf("cast children: expected 1, got %d", len(strC.Children()))
	}
	if strC.Type().Code() != TypeCodeString {
		t.Fatal("cast.Type should be Target")
	}
}

// --- AggregateValue ------------------------------------------------

var _ Value = (*AggregateValue)(nil)

// ArithmeticOp.Symbol pins all five operators including OpMod
// (added this shift) and the unknown-Op fallback.
func TestArithmeticOp_Symbol(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op   ArithmeticOp
		want string
	}{
		{OpAdd, "+"},
		{OpSub, "-"},
		{OpMul, "*"},
		{OpDiv, "/"},
		{OpMod, "%"},
		{ArithmeticOp(99), "?"},
	}
	for _, tc := range cases {
		if got := tc.op.Symbol(); got != tc.want {
			t.Errorf("op %v: got %q, want %q", tc.op, got, tc.want)
		}
	}
}

func TestAggregateOp_Symbol(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op   AggregateOp
		want string
	}{
		{AggCount, "COUNT"},
		{AggCountStar, "COUNT(*)"},
		{AggSum, "SUM"},
		{AggMin, "MIN"},
		{AggMax, "MAX"},
		{AggAvg, "AVG"},
		{AggregateOp(999), "?AGG?"},
	}
	for _, tc := range cases {
		if got := tc.op.Symbol(); got != tc.want {
			t.Fatalf("op=%d: got %q, want %q", tc.op, got, tc.want)
		}
	}
}

func TestAggregateValue_Shape(t *testing.T) {
	t.Parallel()
	sum := NewAggregateValue(AggSum, &FieldValue{Field: "amount", Typ: TypeInt})
	if got := sum.Type(); got != TypeInt {
		t.Fatalf("SUM(int) Type: got %v, want TypeInt", got)
	}
	if len(sum.Children()) != 1 {
		t.Fatalf("SUM children: expected 1, got %d", len(sum.Children()))
	}
	if got, want := sum.Name(), "agg"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if got, want := ExplainValue(sum), "SUM(amount)"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}

	// COUNT(*) — no operand. Type is NotNullLong (zero on empty
	// groups, never NULL) — compare by code to ignore nullability.
	cstar := NewAggregateValue(AggCountStar, nil)
	if got := cstar.Type(); got.Code() != TypeCodeLong {
		t.Fatalf("COUNT(*) Type code: got %v, want %v", got.Code(), TypeCodeLong)
	}
	if len(cstar.Children()) != 0 {
		t.Fatalf("COUNT(*) children: expected 0, got %d", len(cstar.Children()))
	}
	if got, want := ExplainValue(cstar), "COUNT(*)"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}

	// MIN inherits operand type.
	minAge := NewAggregateValue(AggMin, &FieldValue{Field: "age", Typ: TypeInt})
	if minAge.Type().Code() != TypeCodeLong {
		t.Fatal("MIN should inherit operand type")
	}

	// AVG is ALWAYS DOUBLE, independent of operand type (Java AVG_*→DOUBLE).
	// AVG(BIGINT) is DOUBLE, not LONG — this is the load-bearing fix: it
	// makes DOUBLE→LONG assignability checks reject AVG into a BIGINT column.
	for _, opType := range []Type{TypeInt, NullableLong, NotNullLong, NullableFloat, NullableDouble} {
		avg := NewAggregateValue(AggAvg, &FieldValue{Field: "v", Typ: opType})
		if got := avg.Type().Code(); got != TypeCodeDouble {
			t.Fatalf("AVG(%v) Type code: got %v, want DOUBLE", opType.Code(), got)
		}
		if !avg.Type().IsNullable() {
			t.Fatalf("AVG(%v) Type should be nullable (NULL on empty group)", opType.Code())
		}
	}
}

// COUNT(*) + explicit operand is a programmer error.
func TestAggregateValue_CountStarWithOperandPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = NewAggregateValue(AggCountStar, &FieldValue{Field: "x", Typ: TypeInt})
}

// Non-COUNT(*) without operand is also a programmer error.
func TestAggregateValue_MissingOperandPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = NewAggregateValue(AggSum, nil)
}

// Evaluate panics — aggregates are multi-row. The panic message
// tells the caller which aggregator they should be using.
func TestAggregateValue_EvaluatePanics(t *testing.T) {
	t.Parallel()
	sum := NewAggregateValue(AggSum, &FieldValue{Field: "x", Typ: TypeInt})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from AggregateValue.Evaluate")
		}
	}()
	_ = mustEvaluate(sum, map[string]any{"x": int64(5)})
}

// --- QuantifiedObjectValue -----------------------------------------

var (
	_ Value      = (*QuantifiedObjectValue)(nil)
	_ Correlated = (*QuantifiedObjectValue)(nil)
)

func TestQuantifiedObjectValue_Shape(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("t")
	q := NewQuantifiedObjectValue(corr)

	if got, want := q.Name(), "quantifier"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if q.Type().Code() != TypeCodeUnknown {
		t.Fatal("seed quantifier Type should be UnknownType")
	}
	if len(q.Children()) != 0 {
		t.Fatal("quantifier has no Value children")
	}
	if got, want := ExplainValue(q), "t"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

func TestQuantifiedObjectValue_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("u")
	q := NewQuantifiedObjectValue(corr)

	set := q.GetCorrelatedTo()
	if len(set) != 1 {
		t.Fatalf("GetCorrelatedTo: expected 1 entry, got %d", len(set))
	}
	if _, ok := set[corr]; !ok {
		t.Fatal("correlation should be in the set")
	}
}

func TestQuantifiedObjectValue_Evaluate_MultiSource(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("t")
	q := NewQuantifiedObjectValue(corr)
	ctx := map[CorrelationIdentifier]map[string]any{
		corr:                            {"age": int64(30)},
		NamedCorrelationIdentifier("u"): {"other": "field"},
	}
	row, ok := mustEvaluate(q, ctx).(map[string]any)
	if !ok {
		t.Fatalf("expected map row, got %T", mustEvaluate(q, ctx))
	}
	if got, want := row["age"], int64(30); got != want {
		t.Fatalf("age: got %v, want %v", got, want)
	}
}

func TestQuantifiedObjectValue_Evaluate_SingleSource(t *testing.T) {
	t.Parallel()
	q := NewQuantifiedObjectValue(NamedCorrelationIdentifier("t"))
	// Single-source: the whole row IS the correlation's row.
	ctx := map[string]any{"age": int64(42)}
	if got := mustEvaluate(q, ctx); got == nil {
		t.Fatal("single-source Evaluate should return the row")
	}
}

func TestQuantifiedObjectValue_Evaluate_NilContext(t *testing.T) {
	t.Parallel()
	q := NewQuantifiedObjectValue(NamedCorrelationIdentifier("t"))
	if got := mustEvaluate(q, nil); got != nil {
		t.Fatalf("nil ctx: got %v, want nil", got)
	}
}

func TestQuantifiedObjectValue_Evaluate_ForeignContextIsNil(t *testing.T) {
	t.Parallel()
	q := NewQuantifiedObjectValue(NamedCorrelationIdentifier("t"))
	// Unfamiliar context shape degrades to nil.
	if got := mustEvaluate(q, 42); got != nil {
		t.Fatalf("unfamiliar ctx: got %v", got)
	}
}

func TestQuantifiedObjectValue_ZeroCorrelationPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on zero correlation")
		}
	}()
	_ = NewQuantifiedObjectValue(CorrelationIdentifier{})
}

// --- PromoteValue --------------------------------------------------

var _ Value = (*PromoteValue)(nil)

func TestPromoteValue_Shape(t *testing.T) {
	t.Parallel()
	child := &FieldValue{Field: "age", Typ: TypeInt}
	p := NewPromoteValue(child, TypeString)

	if got, want := p.Type(), TypeString; got != want {
		t.Fatalf("Type: got %v, want %v", got, want)
	}
	if len(p.Children()) != 1 {
		t.Fatalf("Children: expected 1, got %d", len(p.Children()))
	}
	if got, want := p.Name(), "promote"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if got, want := ExplainValue(p), "PROMOTE(age TO STRING)"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

func TestPromoteValue_EvaluateDelegatesToChild(t *testing.T) {
	t.Parallel()
	child := &ConstantValue{Value: int64(42), Typ: TypeInt}
	p := NewPromoteValue(child, TypeString)
	// Seed: Promote is a runtime no-op — child's evaluation shines through.
	if got, want := mustEvaluate(p, nil), int64(42); got != want {
		t.Fatalf("Evaluate: got %v, want %v", got, want)
	}
}

func TestPromoteValue_NilChildPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil child")
		}
	}()
	_ = NewPromoteValue(nil, TypeInt)
}

func TestPromoteValue_UnknownTargetPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on TypeUnknown target")
		}
	}()
	_ = NewPromoteValue(&ConstantValue{Value: int64(1), Typ: TypeInt}, TypeUnknown)
}

// --- RecordConstructorValue ----------------------------------------

var _ Value = (*RecordConstructorValue)(nil)

// Compile-time assertions that every other Value impl in this
// package satisfies Value. Post-G1 (swingshift-52), Value includes
// Type() Type — this list also pins that every impl returns rich
// Type without needing a separate Typed-interface assertion. New
// Value impls in this package MUST add themselves here.
var (
	_ Value = (*ConstantValue)(nil)
	_ Value = (*FieldValue)(nil)
	_ Value = (*NullValue)(nil)
	_ Value = (*ParameterValue)(nil)
	_ Value = (*ScalarFunctionValue)(nil)
	_ Value = (*ArithmeticValue)(nil)
	_ Value = (*BooleanValue)(nil)
	_ Value = (*CastValue)(nil)
	_ Value = (*QuantifiedObjectValue)(nil)
	// NotValue is in value_not_test.go.
)

func TestRecordConstructorValue_Shape(t *testing.T) {
	t.Parallel()
	r := NewRecordConstructorValue(
		RecordConstructorField{Name: "id", Value: &FieldValue{Field: "id", Typ: TypeInt}},
		RecordConstructorField{Name: "doubled", Value: &ArithmeticValue{
			Op:    OpMul,
			Left:  &FieldValue{Field: "x", Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		}},
	)

	if got, want := r.Name(), "record"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if r.Type().Code() != TypeCodeRecord {
		t.Fatal("RecordConstructor.Type should be a RecordType")
	}
	if len(r.Children()) != 2 {
		t.Fatalf("Children: got %d, want 2", len(r.Children()))
	}
}

func TestRecordConstructorValue_Evaluate(t *testing.T) {
	t.Parallel()
	r := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "id", Typ: TypeInt}},
		RecordConstructorField{Name: "b", Value: &ConstantValue{Value: "hello", Typ: TypeString}},
	)
	ctx := map[string]any{"id": int64(7)}
	out, ok := mustEvaluate(r, ctx).(map[string]any)
	if !ok {
		t.Fatalf("Evaluate: expected map, got %T", mustEvaluate(r, ctx))
	}
	if got, want := out["a"], int64(7); got != want {
		t.Fatalf("field a: got %v, want %v", got, want)
	}
	if got, want := out["b"], "hello"; got != want {
		t.Fatalf("field b: got %v, want %v", got, want)
	}
}

func TestRecordConstructorValue_Explain(t *testing.T) {
	t.Parallel()
	r := NewRecordConstructorValue(
		RecordConstructorField{Name: "id", Value: &FieldValue{Field: "id", Typ: TypeInt}},
		RecordConstructorField{Name: "lit", Value: &ConstantValue{Value: int64(42), Typ: TypeInt}},
	)
	if got, want := ExplainValue(r), "{id: id, lit: 42}"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

func TestRecordConstructorValue_DuplicateFieldNameDedup(t *testing.T) {
	t.Parallel()
	rcv := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(2), Typ: TypeInt}},
	)
	if len(rcv.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(rcv.Fields))
	}
	if rcv.Fields[0].Name != "a" {
		t.Fatalf("first field should be 'a', got %q", rcv.Fields[0].Name)
	}
	if rcv.Fields[1].Name != "a_2" {
		t.Fatalf("second field should be 'a_2', got %q", rcv.Fields[1].Name)
	}
}

func TestRecordConstructorValue_DefensiveCopy(t *testing.T) {
	t.Parallel()
	fields := []RecordConstructorField{
		{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
	}
	r := NewRecordConstructorValue(fields...)
	// Mutating the caller's slice must not change r.
	fields[0].Name = "HACKED"
	if r.Fields[0].Name != "a" {
		t.Fatalf("defensive copy leaked: got %q", r.Fields[0].Name)
	}
}

// --- WalkValue / ValueSize / ContainsAggregate ---------------------

func TestWalkValue_PreOrder(t *testing.T) {
	t.Parallel()
	// (a + b) * c — 5 nodes total.
	tree := &ArithmeticValue{
		Op: OpMul,
		Left: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &FieldValue{Field: "a", Typ: TypeInt},
			Right: &FieldValue{Field: "b", Typ: TypeInt},
		},
		Right: &FieldValue{Field: "c", Typ: TypeInt},
	}
	var visited int
	WalkValue(tree, func(Value) bool {
		visited++
		return true
	})
	if visited != 5 {
		t.Fatalf("expected 5 visits, got %d", visited)
	}
}

func TestWalkValue_SkipSubtree(t *testing.T) {
	t.Parallel()
	tree := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ArithmeticValue{Op: OpAdd, Left: &FieldValue{Field: "a"}, Right: &FieldValue{Field: "b"}},
		Right: &FieldValue{Field: "c", Typ: TypeInt},
	}
	var visited int
	WalkValue(tree, func(v Value) bool {
		visited++
		if _, ok := v.(*ArithmeticValue); ok && visited > 1 {
			return false // skip sub-arith children
		}
		return true
	})
	// Visits: outer arith, left arith (skipped children), right field. = 3.
	if visited != 3 {
		t.Fatalf("expected 3 visits after skip, got %d", visited)
	}
}

func TestWalkValue_NilSafe(t *testing.T) {
	t.Parallel()
	WalkValue(nil, func(Value) bool {
		t.Fatal("nil input should not invoke visit")
		return true
	})
}

func TestValueSize(t *testing.T) {
	t.Parallel()
	leaf := &FieldValue{Field: "x", Typ: TypeInt}
	if got := ValueSize(leaf); got != 1 {
		t.Fatalf("leaf: got %d, want 1", got)
	}
	tree := &ArithmeticValue{
		Op: OpMul,
		Left: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &FieldValue{Field: "a"},
			Right: &FieldValue{Field: "b"},
		},
		Right: &FieldValue{Field: "c"},
	}
	if got := ValueSize(tree); got != 5 {
		t.Fatalf("tree: got %d, want 5", got)
	}
	if got := ValueSize(nil); got != 0 {
		t.Fatalf("nil: got %d, want 0", got)
	}
}

func TestContainsAggregate(t *testing.T) {
	t.Parallel()
	plain := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "a", Typ: TypeInt},
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	}
	if ContainsAggregate(plain) {
		t.Fatal("scalar tree should not contain aggregate")
	}

	withAgg := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewAggregateValue(AggSum, &FieldValue{Field: "x", Typ: TypeInt}),
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	}
	if !ContainsAggregate(withAgg) {
		t.Fatal("tree with SUM should report aggregate")
	}

	if ContainsAggregate(nil) {
		t.Fatal("nil should not contain aggregate")
	}
}

// --- IsConstantValue ------------------------------------------------

func TestIsConstantValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Value
		want bool
	}{
		{"ConstantValue", &ConstantValue{Value: int64(5), Typ: TypeInt}, true},
		{"NullValue", NewNullValue(TypeInt), true},
		{"BooleanValue true", NewBooleanValue(true), true},
		{"BooleanValue nil", &BooleanValue{Value: nil}, true},
		{"FieldValue", &FieldValue{Field: "x", Typ: TypeInt}, false},
		{"QuantifiedObjectValue", NewQuantifiedObjectValue(NamedCorrelationIdentifier("t")), false},
		{"AggregateValue", NewAggregateValue(AggSum, &ConstantValue{Value: int64(1), Typ: TypeInt}), false},

		// Composite over all-constant children: true.
		{"arith(1, 2)", &ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		}, true},
		// Composite with one non-constant: false.
		{"arith(field, 2)", &ArithmeticValue{
			Op:    OpAdd,
			Left:  &FieldValue{Field: "x", Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		}, false},
		// Cast over constant: true.
		{"cast(constant)", NewCastValue(&ConstantValue{Value: int64(5), Typ: TypeInt}, TypeString), true},
		// Cast over field: false.
		{"cast(field)", NewCastValue(&FieldValue{Field: "x", Typ: TypeInt}, TypeString), false},

		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsConstantValue(tc.v); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// --- EvaluateConstant ---------------------------------------------

func TestEvaluateConstant(t *testing.T) {
	t.Parallel()

	if got, ok := EvaluateConstant(&ConstantValue{Value: int64(5), Typ: TypeInt}); !ok || got != int64(5) {
		t.Fatalf("ConstantValue: got (%v, %v), want (5, true)", got, ok)
	}
	if got, ok := EvaluateConstant(NewNullValue(TypeInt)); !ok || got != nil {
		t.Fatalf("Null: got (%v, %v), want (nil, true)", got, ok)
	}
	// Composite: 1 + 2 → 3.
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
	}
	if got, ok := EvaluateConstant(arith); !ok || got != int64(3) {
		t.Fatalf("1+2: got (%v, %v), want (3, true)", got, ok)
	}
	// Non-constant declines.
	if _, ok := EvaluateConstant(&FieldValue{Field: "x", Typ: TypeInt}); ok {
		t.Fatal("FieldValue should decline (not constant)")
	}
	// Nil safe.
	if _, ok := EvaluateConstant(nil); ok {
		t.Fatal("nil should decline")
	}
}

// TestScalarFunctionValue_Evaluate_NilArg pins the malformed-input
// guard: a ScalarFunctionValue carrying a nil Args[i] short-circuits
// to nil rather than dereferencing. Defensive against rule authors
// that build ScalarFunctionValue programmatically and forget to
// populate every slot.
func TestScalarFunctionValue_Evaluate_NilArg(t *testing.T) {
	t.Parallel()
	v := &ScalarFunctionValue{
		FuncName: "ABS",
		Args:     []Value{nil}, // deliberately malformed
		Typ:      TypeInt,
	}
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Evaluate with nil arg: got %v, want nil", got)
	}
}

// TestExplainTypeName pins the SQL-text rendering for each TypeCode
// the seed CAST/PROMOTE renderer covers + the unknown fall-through.
// Used by ExplainValue's CAST(_ AS X) renderer; if these strings
// change the plandiff hash baseline shifts.
func TestExplainTypeName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    Type
		want string
	}{
		{TypeInt, "INT"},
		{TypeString, "STRING"},
		{TypeBool, "BOOL"},
		{TypeFloat, "FLOAT"},
		{TypeUnknown, "UNKNOWN"},
		{nil, "UNKNOWN"},
		{NotNullBytes, "UNKNOWN"}, // out-of-renderer-set default arm
	}
	for _, tc := range cases {
		if got := explainTypeName(tc.t); got != tc.want {
			t.Fatalf("explainTypeName(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// TestAggregateValue_Type_UnknownOpIsUnknown pins the default arm
// of AggregateValue.Type — a hand-constructed AggregateValue with
// an out-of-range Op (e.g. AggInvalid or any future-but-unknown
// enum value) returns UnknownType rather than panicking. The proper
// constructor NewAggregateValue rejects AggInvalid, but this test
// pins the fall-through arm for raw struct-construction.
func TestAggregateValue_Type_UnknownOpIsUnknown(t *testing.T) {
	t.Parallel()
	a := &AggregateValue{Op: AggInvalid, Operand: &FieldValue{Field: "x", Typ: TypeInt}}
	if got := a.Type(); got != TypeUnknown {
		t.Fatalf("AggInvalid.Type() = %v, want UnknownType", got)
	}
}

// TestAggregateValue_Type_SumWithoutOperandFallsBackToLong pins the
// seed contract: SUM/MIN/MAX/AVG with no Operand defaults to
// NullableLong. Unusual shape (the proper constructor demands an
// operand) but the fall-through is part of the function's documented
// behavior.
func TestAggregateValue_Type_SumWithoutOperandFallsBackToLong(t *testing.T) {
	t.Parallel()
	a := &AggregateValue{Op: AggSum, Operand: nil}
	if got := a.Type(); got != NullableLong {
		t.Fatalf("Sum without operand: got %v, want NullableLong", got)
	}
}

// TestEvaluateConstant_InvariantPanicPropagates pins the RFC-091 contract: the old
// defence-in-depth recover() in EvaluateConstant is GONE. A genuine invariant panic
// during plan-time folding (a constant-looking Value whose Evaluate panics — an
// "impossible" shape IsConstantValue is meant to exclude) must PROPAGATE, not be
// silently swallowed. Plan-time folding runs under gen.Plan, so the db/sql boundary
// recover catches it (→ internal error) and the process stays safe without a second
// net here. User-reachable eval ERRORS, by contrast, decline-to-fold (so the error
// surfaces at runtime with the right SQLSTATE) — see fold_error_test.go.
func TestEvaluateConstant_InvariantPanicPropagates(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("EvaluateConstant must propagate an invariant panic (the db/sql boundary recover handles it), not swallow it")
		}
	}()
	EvaluateConstant(&panicValue{child: &ConstantValue{Value: int64(1), Typ: TypeInt}})
}

// panicValue is a test-only Value that looks constant (has a single
// ConstantValue child) but panics during Evaluate. Reproduces the
// "defence-in-depth" path EvaluateConstant guards against.
type panicValue struct {
	child Value
}

func (p *panicValue) Children() []Value { return []Value{p.child} }
func (p *panicValue) Evaluate(any) (any, error) {
	panic("test-only: Evaluate must not run")
}
func (p *panicValue) Type() Type   { return TypeUnknown }
func (p *panicValue) Name() string { return "panic-value" }

// --- ParameterValue -----------------------------------------------

// fakeBinder implements ParameterBinder for tests. Maps positional
// ordinals via Pos and named parameters via Named.
type fakeBinder struct {
	Pos   map[int]any
	Named map[string]any
}

func (b *fakeBinder) BindParameter(ordinal int, name string) (any, bool) {
	if name != "" {
		v, ok := b.Named[name]
		return v, ok
	}
	v, ok := b.Pos[ordinal]
	return v, ok
}

func TestParameterValue_Shape(t *testing.T) {
	t.Parallel()

	pos := NewParameterValue(1)
	if pos.Ordinal != 1 || pos.ParamName != "" {
		t.Fatalf("positional: got Ordinal=%d ParamName=%q", pos.Ordinal, pos.ParamName)
	}
	if pos.Type().Code() != TypeCodeUnknown {
		t.Fatalf("positional Type: want UnknownType, got %v", pos.Type())
	}
	if pos.Name() != "param" {
		t.Fatalf("Name: want 'param', got %q", pos.Name())
	}
	if len(pos.Children()) != 0 {
		t.Fatalf("Children: want 0, got %d", len(pos.Children()))
	}

	named := NewNamedParameterValue("foo")
	if named.Ordinal != 0 || named.ParamName != "foo" {
		t.Fatalf("named: got Ordinal=%d ParamName=%q", named.Ordinal, named.ParamName)
	}
}

func TestParameterValue_Evaluate_NoBinder(t *testing.T) {
	t.Parallel()

	pos := NewParameterValue(1)
	// Nil context → NULL (UNKNOWN).
	if got := mustEvaluate(pos, nil); got != nil {
		t.Fatalf("nil ctx: want nil, got %v", got)
	}
	// Row-only context (no binder capability) → NULL.
	if got := mustEvaluate(pos, map[string]any{"x": int64(5)}); got != nil {
		t.Fatalf("row ctx without binder: want nil, got %v", got)
	}
}

func TestParameterValue_Evaluate_WithBinder(t *testing.T) {
	t.Parallel()

	binder := &fakeBinder{
		Pos:   map[int]any{1: int64(42), 2: "hello"},
		Named: map[string]any{"foo": true, "bar": nil},
	}

	if got := mustEvaluate(NewParameterValue(1), binder); got != int64(42) {
		t.Fatalf("?1: want 42, got %v", got)
	}
	if got := mustEvaluate(NewParameterValue(2), binder); got != "hello" {
		t.Fatalf("?2: want 'hello', got %v", got)
	}
	if got := mustEvaluate(NewNamedParameterValue("foo"), binder); got != true {
		t.Fatalf(":foo: want true, got %v", got)
	}
	// Bound to NULL — binder reports (nil, true). Evaluate surfaces nil.
	if got := mustEvaluate(NewNamedParameterValue("bar"), binder); got != nil {
		t.Fatalf(":bar (NULL bound): want nil, got %v", got)
	}
	// Unbound → nil.
	if got := mustEvaluate(NewParameterValue(99), binder); got != nil {
		t.Fatalf("?99 unbound: want nil, got %v", got)
	}
	if got := mustEvaluate(NewNamedParameterValue("missing"), binder); got != nil {
		t.Fatalf(":missing unbound: want nil, got %v", got)
	}
}

func TestParameterValue_IsNotConstant(t *testing.T) {
	t.Parallel()

	if IsConstantValue(NewParameterValue(1)) {
		t.Fatal("?1 must not be constant")
	}
	if IsConstantValue(NewNamedParameterValue("foo")) {
		t.Fatal(":foo must not be constant")
	}
	// Composite containing a parameter is not constant either.
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: NewParameterValue(1),
	}
	if IsConstantValue(arith) {
		t.Fatal("1 + ?1 must not be constant")
	}
	if _, ok := EvaluateConstant(arith); ok {
		t.Fatal("EvaluateConstant on composite-with-param must decline")
	}
}

// --- ScalarFunctionValue ------------------------------------------

func TestScalarFunctionValue_Shape(t *testing.T) {
	t.Parallel()

	v := NewScalarFunctionValue("upper", TypeString,
		&ConstantValue{Value: "hi", Typ: TypeString})
	if v.FuncName != "UPPER" {
		t.Fatalf("FuncName: got %q, want 'UPPER'", v.FuncName)
	}
	if v.Type().Code() != TypeCodeString {
		t.Fatalf("Type: got %v, want STRING", v.Type())
	}
	if v.Name() != "scalarfn" {
		t.Fatalf("Name: got %q", v.Name())
	}
	if got := len(v.Children()); got != 1 {
		t.Fatalf("Children: got %d, want 1", got)
	}
	// Zero-arg form returns an empty (non-nil) slice — matcher contract.
	zero := NewScalarFunctionValue("CURRENT_TIMESTAMP", TypeString)
	if got := zero.Children(); got == nil || len(got) != 0 {
		t.Fatalf("zero-arg Children: got %v, want non-nil empty", got)
	}
}

func TestScalarFunctionValue_Evaluate(t *testing.T) {
	t.Parallel()

	str := func(s string) Value { return &ConstantValue{Value: s, Typ: TypeString} }
	bytesV := func(b []byte) Value { return &ConstantValue{Value: b, Typ: TypeString} }
	field := func(name string) Value { return &FieldValue{Field: name, Typ: TypeString} }
	row := map[string]any{"NAME": "Alice", "BLANK": "", "BIN": []byte{0xff, 0xfe}}

	cases := []struct {
		name string
		v    Value
		ctx  any
		want any
	}{
		{"UPPER literal", NewScalarFunctionValue("UPPER", TypeString, str("Hello")), nil, "HELLO"},
		{"LOWER literal", NewScalarFunctionValue("LOWER", TypeString, str("Hello")), nil, "hello"},
		{"UPPER over field", NewScalarFunctionValue("UPPER", TypeString, field("NAME")), row, "ALICE"},
		{"LENGTH ascii", NewScalarFunctionValue("LENGTH", TypeInt, str("hello")), nil, int64(5)},
		{"CHAR_LENGTH multibyte", NewScalarFunctionValue("CHAR_LENGTH", TypeInt, str("café")), nil, int64(4)},
		{"OCTET_LENGTH multibyte", NewScalarFunctionValue("OCTET_LENGTH", TypeInt, str("café")), nil, int64(5)},
		{"LENGTH bytes", NewScalarFunctionValue("LENGTH", TypeInt, bytesV([]byte{1, 2, 3})), nil, int64(3)},
		{"NULL propagates", NewScalarFunctionValue("UPPER", TypeString, &NullValue{Typ: TypeString}), nil, nil},
		{"unknown function declines", NewScalarFunctionValue("FROBNICATE", TypeString, str("x")), nil, nil},
		{"wrong arg type declines", NewScalarFunctionValue("UPPER", TypeString, &ConstantValue{Value: int64(5), Typ: TypeInt}), nil, nil},
		{"OCTET_LENGTH bytes", NewScalarFunctionValue("OCTET_LENGTH", TypeInt, field("BIN")), row, int64(2)},
		{"empty string LENGTH", NewScalarFunctionValue("LENGTH", TypeInt, field("BLANK")), row, int64(0)},
	}
	for _, tc := range cases {
		got := mustEvaluate(tc.v, tc.ctx)
		if got != tc.want {
			t.Fatalf("%s: got %v (%T), want %v (%T)", tc.name, got, got, tc.want, tc.want)
		}
	}
}

func TestScalarFunctionValue_IsConstant(t *testing.T) {
	t.Parallel()

	upperConst := NewScalarFunctionValue("UPPER", TypeString,
		&ConstantValue{Value: "hi", Typ: TypeString})
	if !IsConstantValue(upperConst) {
		t.Fatal("UPPER('hi') should be constant")
	}
	if got, ok := EvaluateConstant(upperConst); !ok || got != "HI" {
		t.Fatalf("EvaluateConstant(UPPER('hi')): got (%v, %v), want ('HI', true)", got, ok)
	}

	upperField := NewScalarFunctionValue("UPPER", TypeString,
		&FieldValue{Field: "name", Typ: TypeString})
	if IsConstantValue(upperField) {
		t.Fatal("UPPER(field) should not be constant")
	}

	// Zero-arg form: composite branch sees zero children, falls
	// through to "unknown leaf — conservatively not constant".
	zero := NewScalarFunctionValue("CURRENT_TIMESTAMP", TypeString)
	if IsConstantValue(zero) {
		t.Fatal("CURRENT_TIMESTAMP() should not be constant in the seed (no zero-arg pure marker)")
	}
}

func TestExplainValue_ScalarFunctionValue(t *testing.T) {
	t.Parallel()

	v := NewScalarFunctionValue("UPPER", TypeString,
		&FieldValue{Field: "NAME", Typ: TypeString})
	if got, want := ExplainValue(v), "UPPER(NAME)"; got != want {
		t.Fatalf("UPPER(NAME): got %q, want %q", got, want)
	}
	nested := NewScalarFunctionValue("LENGTH", TypeInt,
		NewScalarFunctionValue("LOWER", TypeString,
			&FieldValue{Field: "NAME", Typ: TypeString}))
	if got, want := ExplainValue(nested), "LENGTH(LOWER(NAME))"; got != want {
		t.Fatalf("nested: got %q, want %q", got, want)
	}
	zero := NewScalarFunctionValue("NOW", TypeString)
	if got, want := ExplainValue(zero), "NOW()"; got != want {
		t.Fatalf("zero-arg: got %q, want %q", got, want)
	}
}

func TestExplainValue_ParameterValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		v    Value
		want string
	}{
		{NewParameterValue(1), "?1"},
		{NewParameterValue(7), "?7"},
		{NewParameterValue(0), "?"}, // unnumbered `?` (walker seed; per-statement ordinal not yet wired)
		{NewNamedParameterValue("foo"), "?foo"},
		{NewNamedParameterValue("user_id"), "?user_id"},
	}
	for _, tc := range cases {
		if got := ExplainValue(tc.v); got != tc.want {
			t.Fatalf("ExplainValue(%v): got %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestAggregateValue_GetIndexTypeName(t *testing.T) {
	t.Parallel()
	cases := map[AggregateOp]string{
		AggCount:     "COUNT_NOT_NULL",
		AggCountStar: "COUNT",
		AggSum:       "SUM",
		AggMin:       "MIN_EVER_LONG",
		AggMax:       "MAX_EVER_LONG",
		AggAvg:       "",
		AggInvalid:   "",
	}
	for op, want := range cases {
		var operand Value
		if op != AggCountStar && op != AggInvalid {
			operand = &ConstantValue{Value: int64(1), Typ: NotNullLong}
		}
		v := &AggregateValue{Op: op, Operand: operand}
		if got := v.GetIndexTypeName(); got != want {
			t.Errorf("AggregateValue{Op=%v}.GetIndexTypeName() = %q, want %q", op, got, want)
		}
	}
}

func TestAggregateValue_ImplementsIndexableAggregate(t *testing.T) {
	t.Parallel()
	v := &AggregateValue{Op: AggSum, Operand: &ConstantValue{Value: int64(1), Typ: NotNullLong}}
	var _ IndexableAggregate = v
	iav, ok := Value(v).(IndexableAggregate)
	if !ok {
		t.Fatalf("AggregateValue should implement IndexableAggregate")
	}
	if iav.GetIndexTypeName() != "SUM" {
		t.Fatalf("via interface: GetIndexTypeName = %q, want SUM", iav.GetIndexTypeName())
	}
}

func TestIsNonEvaluable_AggregateValue(t *testing.T) {
	t.Parallel()
	v := &AggregateValue{Op: AggSum, Operand: &ConstantValue{Value: int64(1), Typ: NotNullLong}}
	if !IsNonEvaluable(v) {
		t.Fatal("AggregateValue should be NonEvaluable")
	}
}

func TestIsNonEvaluable_PlainValue(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: int64(7), Typ: NotNullLong}
	if IsNonEvaluable(v) {
		t.Fatal("ConstantValue should NOT be NonEvaluable")
	}
}

func TestIsNonEvaluable_NilValue(t *testing.T) {
	t.Parallel()
	if IsNonEvaluable(nil) {
		t.Fatal("nil should NOT be NonEvaluable")
	}
}

func TestIsNonEvaluable_IndexOnlyAggregate(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, nil)
	if !IsNonEvaluable(v) {
		t.Fatal("IndexOnlyAggregateValue should be NonEvaluable")
	}
}

func TestIsIndexOnly_RowNumberValue(t *testing.T) {
	t.Parallel()
	v := NewRowNumberValue(nil, nil, nil, nil)
	if !IsIndexOnly(v) {
		t.Fatal("RowNumberValue should be IndexOnly")
	}
}

func TestIsIndexOnly_DistanceRowNumberValue(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue(nil, nil)
	if !IsIndexOnly(v) {
		t.Fatal("DistanceRowNumberValue should be IndexOnly")
	}
}

func TestIsIndexOnly_IndexOnlyAggregateValue(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, nil)
	if !IsIndexOnly(v) {
		t.Fatal("IndexOnlyAggregateValue should be IndexOnly")
	}
}

func TestIsIndexOnly_PlainValue(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: int64(7), Typ: NotNullLong}
	if IsIndexOnly(v) {
		t.Fatal("ConstantValue should NOT be IndexOnly")
	}
}

func TestIsIndexOnly_NilValue(t *testing.T) {
	t.Parallel()
	if IsIndexOnly(nil) {
		t.Fatal("nil should NOT be IndexOnly")
	}
}

// --- evaluateCorrelated unit tests ---

func TestFieldValue_QOV_CorrelationBinder(t *testing.T) {
	t.Parallel()
	corrA := NamedCorrelationIdentifier("A")
	corrB := NamedCorrelationIdentifier("B")
	fv := NewFieldValue(NewQuantifiedObjectValue(corrA), "NAME", UnknownType)

	rc := &RowEvalContext{
		Datum: map[string]any{"irrelevant": "data"},
		Correlations: &testCorrelationBinder{bindings: map[CorrelationIdentifier]any{
			corrA: map[string]any{"NAME": "Alice", "ID": int64(1)},
			corrB: map[string]any{"NAME": "Bob", "ID": int64(2)},
		}},
	}
	got := mustEvaluate(fv, rc)
	if got != "Alice" {
		t.Fatalf("expected Alice, got %v", got)
	}
}

func TestFieldValue_QOV_CorrelationBinder_OtherTable(t *testing.T) {
	t.Parallel()
	corrB := NamedCorrelationIdentifier("B")
	fv := NewFieldValue(NewQuantifiedObjectValue(corrB), "NAME", UnknownType)

	rc := &RowEvalContext{
		Datum: map[string]any{"NAME": "wrong"},
		Correlations: &testCorrelationBinder{bindings: map[CorrelationIdentifier]any{
			NamedCorrelationIdentifier("A"): map[string]any{"NAME": "Alice"},
			corrB:                           map[string]any{"NAME": "Bob"},
		}},
	}
	got := mustEvaluate(fv, rc)
	if got != "Bob" {
		t.Fatalf("expected Bob, got %v", got)
	}
}

func TestFieldValue_QOV_FlatMap_QualifiedKey(t *testing.T) {
	t.Parallel()
	fv := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("EMP")), "NAME", UnknownType)

	merged := map[string]any{
		"NAME":      "wrong-bare",
		"EMP.NAME":  "Alice",
		"DEPT.NAME": "Engineering",
	}
	got := mustEvaluate(fv, merged)
	if got != "Alice" {
		t.Fatalf("expected Alice from EMP.NAME, got %v", got)
	}
}

// TestFieldValue_QOV_MergeQuantifier_AlreadyQualifiedField pins the RFC-069
// merged-Datum resolution fix. A re-enumerated N-way join collapses a buried
// table (T3) into a merge quantifier ($m) whose row flows that table's columns
// under their OWN qualified keys (T3.T2_ID), preserved verbatim by the
// source-anchored join RC / the executor's mergeRows — NOT re-prefixed with the merge alias. A
// FieldValue{Child: QOV($m), Field: "T3.T2_ID"} accessed against the merged
// Datum (map / RowEvalContext.Datum) must resolve "T3.T2_ID" directly; the naive
// qualKey = $m + "." + Field would invent the never-written key "$M.T3.T2_ID"
// and return nil → the 0-rows bug.
func TestFieldValue_QOV_MergeQuantifier_AlreadyQualifiedField(t *testing.T) {
	t.Parallel()
	merge := NamedCorrelationIdentifier("$M_2:T3_2:T4")
	fv := NewFieldValue(NewQuantifiedObjectValue(merge), "T3.T2_ID", UnknownType)

	merged := map[string]any{
		"T3.T2_ID": int64(7),
		"T4.T3_ID": int64(3),
		"ID":       int64(99), // bare key from the last-written table
	}

	// map[string]any context (the NLJ passesJoinPredicates path).
	if got := mustEvaluate(fv, merged); got != int64(7) {
		t.Errorf("map ctx: T3.T2_ID via $m = %v, want 7", got)
	}

	// RowEvalContext.Datum context (the PredicatesFilter path, no binding for $m).
	rc := &RowEvalContext{Datum: merged}
	if got := mustEvaluate(fv, rc); got != int64(7) {
		t.Errorf("RowEvalContext.Datum ctx: T3.T2_ID via $m = %v, want 7", got)
	}

	// A qualified field NOT present must still miss (no spurious fallback).
	missing := NewFieldValue(NewQuantifiedObjectValue(merge), "T9.X", UnknownType)
	if got := mustEvaluate(missing, merged); got != nil {
		t.Errorf("absent qualified key must resolve nil, got %v", got)
	}
}

func TestFieldValue_QOV_FlatMap_NoFallbackToBareKey(t *testing.T) {
	t.Parallel()
	fv := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("A")), "K", UnknownType)

	merged := map[string]any{
		"K":   int64(99),
		"B.K": int64(99),
	}
	got := mustEvaluate(fv, merged)
	if got != nil {
		t.Fatalf("expected nil (A.K not in map), got %v — bare key fallback must not happen", got)
	}
}

func TestFieldValue_QOV_NullKeyDisambiguation(t *testing.T) {
	t.Parallel()
	fvA := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("A")), "K", UnknownType)
	fvB := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("B")), "K", UnknownType)

	merged := map[string]any{
		"A.K": int64(10),
		"B.K": int64(20),
		"K":   int64(10),
	}
	gotA := mustEvaluate(fvA, merged)
	gotB := mustEvaluate(fvB, merged)
	if gotA != int64(10) {
		t.Errorf("A.K: expected 10, got %v", gotA)
	}
	if gotB != int64(20) {
		t.Errorf("B.K: expected 20, got %v", gotB)
	}
}

func TestFieldValue_QOV_NullFK_NoMatch(t *testing.T) {
	t.Parallel()
	fvA := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("A")), "K", UnknownType)
	fvB := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("B")), "K", UnknownType)

	merged := map[string]any{
		"B.K": int64(10),
		"K":   int64(10),
	}
	gotA := mustEvaluate(fvA, merged)
	gotB := mustEvaluate(fvB, merged)
	if gotA != nil {
		t.Fatalf("A.K absent from map → must be nil, got %v", gotA)
	}
	if gotB != int64(10) {
		t.Fatalf("B.K expected 10, got %v", gotB)
	}
}

func TestFieldValue_QOV_CorrelationIdMap(t *testing.T) {
	t.Parallel()
	corrE := NamedCorrelationIdentifier("EMP")
	fv := NewFieldValue(NewQuantifiedObjectValue(corrE), "SALARY", UnknownType)

	ctx := map[CorrelationIdentifier]map[string]any{
		corrE:                              {"SALARY": int64(100), "NAME": "Alice"},
		NamedCorrelationIdentifier("DEPT"): {"NAME": "Eng"},
	}
	got := mustEvaluate(fv, ctx)
	if got != int64(100) {
		t.Fatalf("expected 100, got %v", got)
	}
}

func TestFieldValue_QOV_MissingCorrelation_ReturnsNil(t *testing.T) {
	t.Parallel()
	fv := NewFieldValue(NewQuantifiedObjectValue(NamedCorrelationIdentifier("MISSING")), "COL", UnknownType)

	rc := &RowEvalContext{
		Datum: map[string]any{"COL": "should-not-return"},
		Correlations: &testCorrelationBinder{bindings: map[CorrelationIdentifier]any{
			NamedCorrelationIdentifier("OTHER"): map[string]any{"COL": "other"},
		}},
	}
	got := mustEvaluate(fv, rc)
	if got != nil {
		t.Fatalf("missing correlation should return nil, got %v", got)
	}
}

func TestFieldValue_NoChild_BackwardCompat(t *testing.T) {
	t.Parallel()
	fv := &FieldValue{Field: "NAME", Typ: UnknownType}

	row := map[string]any{"NAME": "Alice"}
	got := mustEvaluate(fv, row)
	if got != "Alice" {
		t.Fatalf("backward compat: expected Alice, got %v", got)
	}
}

func TestFieldValue_NoChild_QualifiedString_BackwardCompat(t *testing.T) {
	t.Parallel()
	fv := &FieldValue{Field: "EMP.NAME", Typ: UnknownType}

	row := map[string]any{"EMP.NAME": "Alice", "NAME": "wrong"}
	got := mustEvaluate(fv, row)
	if got != "Alice" {
		t.Fatalf("backward compat qualified: expected Alice, got %v", got)
	}
}

type testCorrelationBinder struct {
	bindings map[CorrelationIdentifier]any
}

func (b *testCorrelationBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	v, ok := b.bindings[id]
	return v, ok
}
