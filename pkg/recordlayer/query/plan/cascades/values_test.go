package cascades

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
	if nv.Type() != TypeInt {
		t.Fatal("Type should match constructor")
	}
	if nv.Name() != "null" {
		t.Fatal("Name should be 'null'")
	}
	if got := nv.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate: expected nil, got %v", got)
	}
	// Any context — NULL is context-independent.
	if got := nv.Evaluate(map[string]any{"x": 1}); got != nil {
		t.Fatalf("Evaluate w/ ctx: expected nil, got %v", got)
	}
	if len(nv.Children()) != 0 {
		t.Fatal("NullValue.Children: expected 0")
	}
}

func TestConstantValue_Evaluate(t *testing.T) {
	t.Parallel()
	c := &ConstantValue{Value: int64(42), Typ: TypeInt}
	if got := c.Evaluate(nil); got != int64(42) {
		t.Fatalf("constant int: got %v", got)
	}
	// Context is ignored for constants.
	if got := c.Evaluate(map[string]any{"x": 1}); got != int64(42) {
		t.Fatalf("constant ignores ctx: got %v", got)
	}
	// NULL literal.
	null := &ConstantValue{Value: nil, Typ: TypeInt}
	if got := null.Evaluate(nil); got != nil {
		t.Fatalf("NULL literal: got %v", got)
	}
}

func TestFieldValue_Evaluate(t *testing.T) {
	t.Parallel()
	f := &FieldValue{Field: "name", Typ: TypeString}
	row := map[string]any{"name": "Alice", "age": int64(30)}
	if got := f.Evaluate(row); got != "Alice" {
		t.Fatalf("field lookup: got %v", got)
	}
	// Missing field: NULL.
	missing := &FieldValue{Field: "nope", Typ: TypeString}
	if got := missing.Evaluate(row); got != nil {
		t.Fatalf("missing field: got %v", got)
	}
	// nil ctx.
	if got := f.Evaluate(nil); got != nil {
		t.Fatalf("nil ctx: got %v", got)
	}
	// Wrong ctx type.
	if got := f.Evaluate("not a map"); got != nil {
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
		got := av.Evaluate(map[string]any{"a": tc.a, "b": tc.b})
		if got != tc.want {
			t.Fatalf("op %v: got %v, want %v", tc.op, got, tc.want)
		}
	}

	// Division by zero returns nil (UNKNOWN-at-Value-layer).
	divZ := &ArithmeticValue{Op: OpDiv, Left: a, Right: b}
	if got := divZ.Evaluate(map[string]any{"a": int64(5), "b": int64(0)}); got != nil {
		t.Fatalf("div by zero: got %v", got)
	}

	// MOD by zero same nil-on-zero contract as Div.
	modZ := &ArithmeticValue{Op: OpMod, Left: a, Right: b}
	if got := modZ.Evaluate(map[string]any{"a": int64(5), "b": int64(0)}); got != nil {
		t.Fatalf("mod by zero: got %v", got)
	}

	// NULL propagation.
	sum := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := sum.Evaluate(map[string]any{"a": nil, "b": int64(1)}); got != nil {
		t.Fatalf("NULL lhs: got %v", got)
	}
	if got := sum.Evaluate(map[string]any{"a": int64(1), "b": nil}); got != nil {
		t.Fatalf("NULL rhs: got %v", got)
	}

	// Type mismatch returns nil.
	tm := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := tm.Evaluate(map[string]any{"a": "foo", "b": int64(1)}); got != nil {
		t.Fatalf("type mismatch: got %v", got)
	}
}

func TestBooleanValue(t *testing.T) {
	t.Parallel()
	tv := NewBooleanValue(true)
	if got := tv.Evaluate(nil); got != true {
		t.Fatalf("true literal: got %v", got)
	}
	fv := NewBooleanValue(false)
	if got := fv.Evaluate(nil); got != false {
		t.Fatalf("false literal: got %v", got)
	}
	// UNKNOWN literal.
	uv := &BooleanValue{Value: nil}
	if got := uv.Evaluate(nil); got != nil {
		t.Fatalf("UNKNOWN literal: got %v", got)
	}
	if tv.Type() != TypeBool {
		t.Fatal("BooleanValue.Type should be TypeBool")
	}
}

func TestCastValue(t *testing.T) {
	t.Parallel()
	// int → string
	strC := NewCastValue(&ConstantValue{Value: int64(42), Typ: TypeInt}, TypeString)
	if got := strC.Evaluate(nil); got != "42" {
		t.Fatalf("int→string: got %v", got)
	}

	// bool → int: true=1, false=0.
	boolToInt := NewCastValue(NewBooleanValue(true), TypeInt)
	if got := boolToInt.Evaluate(nil); got != int64(1) {
		t.Fatalf("true→int: got %v", got)
	}
	boolToInt = NewCastValue(NewBooleanValue(false), TypeInt)
	if got := boolToInt.Evaluate(nil); got != int64(0) {
		t.Fatalf("false→int: got %v", got)
	}

	// int → bool: 0=false, non-zero=true.
	intToBool := NewCastValue(&ConstantValue{Value: int64(0), Typ: TypeInt}, TypeBool)
	if got := intToBool.Evaluate(nil); got != false {
		t.Fatalf("0→bool: got %v", got)
	}
	intToBool = NewCastValue(&ConstantValue{Value: int64(7), Typ: TypeInt}, TypeBool)
	if got := intToBool.Evaluate(nil); got != true {
		t.Fatalf("7→bool: got %v", got)
	}

	// NULL propagates.
	nullC := NewCastValue(&ConstantValue{Value: nil, Typ: TypeInt}, TypeString)
	if got := nullC.Evaluate(nil); got != nil {
		t.Fatalf("NULL cast: got %v", got)
	}

	// Float source casts.
	// int → float
	intToFloat := NewCastValue(&ConstantValue{Value: int64(5), Typ: TypeInt}, TypeFloat)
	if got := intToFloat.Evaluate(nil); got != float64(5) {
		t.Fatalf("int→float: got %v", got)
	}
	// float → int (truncates toward zero)
	floatToInt := NewCastValue(&ConstantValue{Value: float64(3.9), Typ: TypeFloat}, TypeInt)
	if got := floatToInt.Evaluate(nil); got != int64(3) {
		t.Fatalf("3.9→int: got %v", got)
	}
	floatToIntNeg := NewCastValue(&ConstantValue{Value: float64(-3.9), Typ: TypeFloat}, TypeInt)
	if got := floatToIntNeg.Evaluate(nil); got != int64(-3) {
		t.Fatalf("-3.9→int: got %v", got)
	}
	// float → bool: 0.0 = false, non-zero = true
	floatToBool0 := NewCastValue(&ConstantValue{Value: float64(0), Typ: TypeFloat}, TypeBool)
	if got := floatToBool0.Evaluate(nil); got != false {
		t.Fatalf("0.0→bool: got %v", got)
	}
	floatToBoolNZ := NewCastValue(&ConstantValue{Value: float64(0.5), Typ: TypeFloat}, TypeBool)
	if got := floatToBoolNZ.Evaluate(nil); got != true {
		t.Fatalf("0.5→bool: got %v", got)
	}
	// float → string
	floatToStr := NewCastValue(&ConstantValue{Value: float64(3.14), Typ: TypeFloat}, TypeString)
	if got := floatToStr.Evaluate(nil); got != "3.14" {
		t.Fatalf("3.14→string: got %v", got)
	}
	// float → float (verbatim)
	floatToFloat := NewCastValue(&ConstantValue{Value: float64(2.5), Typ: TypeFloat}, TypeFloat)
	if got := floatToFloat.Evaluate(nil); got != float64(2.5) {
		t.Fatalf("float→float: got %v", got)
	}
	// NaN / Inf → nil for int target (out-of-range).
	nanToInt := NewCastValue(&ConstantValue{Value: float64(math.NaN()), Typ: TypeFloat}, TypeInt)
	if got := nanToInt.Evaluate(nil); got != nil {
		t.Fatalf("NaN→int: expected nil, got %v", got)
	}
	infToInt := NewCastValue(&ConstantValue{Value: float64(math.Inf(1)), Typ: TypeFloat}, TypeInt)
	if got := infToInt.Evaluate(nil); got != nil {
		t.Fatalf("+Inf→int: expected nil, got %v", got)
	}

	// Unknown conversion: int → bool via the reverse path is OK,
	// but string → int isn't wired in the seed (returns nil).
	strToInt := NewCastValue(&ConstantValue{Value: "3", Typ: TypeString}, TypeInt)
	if got := strToInt.Evaluate(nil); got != nil {
		t.Fatalf("string→int not yet wired: expected nil, got %v", got)
	}

	// Children wiring + Type.
	if len(strC.Children()) != 1 {
		t.Fatalf("cast children: expected 1, got %d", len(strC.Children()))
	}
	if strC.Type() != TypeString {
		t.Fatal("cast.Type should be Target")
	}
}

// --- AggregateValue ------------------------------------------------

var _ Value = (*AggregateValue)(nil)

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

	// COUNT(*) — no operand.
	cstar := NewAggregateValue(AggCountStar, nil)
	if got, want := cstar.Type(), TypeInt; got != want {
		t.Fatalf("COUNT(*) Type: got %v, want %v", got, want)
	}
	if len(cstar.Children()) != 0 {
		t.Fatalf("COUNT(*) children: expected 0, got %d", len(cstar.Children()))
	}
	if got, want := ExplainValue(cstar), "COUNT(*)"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}

	// MIN inherits operand type.
	minAge := NewAggregateValue(AggMin, &FieldValue{Field: "age", Typ: TypeInt})
	if minAge.Type() != TypeInt {
		t.Fatal("MIN should inherit operand type")
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
	_ = sum.Evaluate(map[string]any{"x": int64(5)})
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
	if q.Type() != TypeUnknown {
		t.Fatal("seed quantifier Type should be TypeUnknown")
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
	row, ok := q.Evaluate(ctx).(map[string]any)
	if !ok {
		t.Fatalf("expected map row, got %T", q.Evaluate(ctx))
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
	if got := q.Evaluate(ctx); got == nil {
		t.Fatal("single-source Evaluate should return the row")
	}
}

func TestQuantifiedObjectValue_Evaluate_NilContext(t *testing.T) {
	t.Parallel()
	q := NewQuantifiedObjectValue(NamedCorrelationIdentifier("t"))
	if got := q.Evaluate(nil); got != nil {
		t.Fatalf("nil ctx: got %v, want nil", got)
	}
}

func TestQuantifiedObjectValue_Evaluate_ForeignContextIsNil(t *testing.T) {
	t.Parallel()
	q := NewQuantifiedObjectValue(NamedCorrelationIdentifier("t"))
	// Unfamiliar context shape degrades to nil.
	if got := q.Evaluate(42); got != nil {
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
	if got, want := p.Evaluate(nil), int64(42); got != want {
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
	if r.Type() != TypeUnknown {
		t.Fatal("seed RecordConstructor should have Type TypeUnknown")
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
	out, ok := r.Evaluate(ctx).(map[string]any)
	if !ok {
		t.Fatalf("Evaluate: expected map, got %T", r.Evaluate(ctx))
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

func TestRecordConstructorValue_DuplicateFieldNamePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate field name")
		}
	}()
	_ = NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(2), Typ: TypeInt}},
	)
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
