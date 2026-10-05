package values

import (
	"errors"
	"math"
	"testing"
)

func callSiteErrorOf(t *testing.T, err error) *CallSiteOptionError {
	t.Helper()
	var e *CallSiteOptionError
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a CallSiteOptionError", err)
	}
	return e
}

// Option coercion as CallSiteArguments.Option: only exactly-representable
// integers for INTEGER/LONG, any number for DOUBLE, only a boolean for
// BOOLEAN, text for STRING, a case-insensitive constant for an enum, and
// NULL never.
func TestCallSiteOptionCoercion(t *testing.T) {
	t.Parallel()
	integer := IntegerOption("i")
	if v, err := integer.Coerce(int64(7)); err != nil || v != 7 {
		t.Errorf("INTEGER 7 = %v, %v", v, err)
	}
	if v, err := integer.Coerce(int32(-3)); err != nil || v != -3 {
		t.Errorf("INTEGER int32 -3 = %v, %v", v, err)
	}
	for _, raw := range []any{int64(math.MaxInt32) + 1, int64(math.MinInt32) - 1} {
		if _, err := integer.Coerce(raw); err == nil || callSiteErrorOf(t, err).Detail != "option value is out of range for the option's type" {
			t.Errorf("INTEGER %v: %v", raw, err)
		}
	}
	for _, raw := range []any{1.0, "7", true} {
		if _, err := integer.Coerce(raw); err == nil || callSiteErrorOf(t, err).Detail != "option value is of an unexpected type" {
			t.Errorf("INTEGER %v (%T): %v", raw, raw, err)
		}
	}
	if v, err := LongOption("l").Coerce(int64(math.MaxInt64)); err != nil || v != math.MaxInt64 {
		t.Errorf("LONG max = %v, %v", v, err)
	}
	if v, err := DoubleOption("d").Coerce(int64(2)); err != nil || v != 2 {
		t.Errorf("DOUBLE from integer = %v, %v", v, err)
	}
	if _, err := DoubleOption("d").Coerce("2"); err == nil {
		t.Error("DOUBLE accepted text")
	}
	if v, err := BooleanOption("b").Coerce(true); err != nil || !v {
		t.Errorf("BOOLEAN true = %v, %v", v, err)
	}
	for _, raw := range []any{"true", int64(1)} {
		if _, err := BooleanOption("b").Coerce(raw); err == nil {
			t.Errorf("BOOLEAN accepted %v (%T)", raw, raw)
		}
	}
	if v, err := StringOption("s").Coerce("x"); err != nil || v != "x" {
		t.Errorf("STRING = %v, %v", v, err)
	}
	enum := EnumOption("e", "LOW", "HIGH")
	if v, err := enum.Coerce("high"); err != nil || v != "HIGH" {
		t.Errorf("ENUM high = %v, %v", v, err)
	}
	if _, err := enum.Coerce("medium"); err == nil {
		t.Error("ENUM accepted a value that is no constant")
	}
	if _, err := integer.Coerce(nil); err == nil || callSiteErrorOf(t, err).Detail != "option value must not be null" {
		t.Errorf("INTEGER NULL: %v", err)
	}
	if e := callSiteErrorOf(t, func() error { _, err := integer.Coerce("x"); return err }()); e.Code != CallSiteIncompatibleType || e.Option != "i" {
		t.Errorf("error = %+v", e)
	}
}

// The builder refuses NULL and a repeated name as it collects; resolution
// against a function's options refuses an unsupported name and coerces the
// rest.
func TestCallSiteOptionsBuildAndResolve(t *testing.T) {
	t.Parallel()
	b := NewCallSiteOptionsBuilder()
	if err := b.PutRaw("x", nil); err == nil || callSiteErrorOf(t, err).Code != CallSiteIncompatibleType {
		t.Errorf("NULL option: %v", err)
	}
	if err := b.PutRaw("hnswEfSearch", int64(10)); err != nil {
		t.Fatal(err)
	}
	if err := b.PutRaw("hnswEfSearch", int64(10)); err == nil || callSiteErrorOf(t, err).Code != CallSiteFunctionUndefined ||
		callSiteErrorOf(t, err).Detail != "option specified more than once" {
		t.Errorf("repeated option: %v", err)
	}
	opts := b.Build()
	args := PositionalCallSite().WithOptions(opts)
	if !args.HasOptions() || args.IsSimple() {
		t.Errorf("options not carried: %+v", args)
	}
	resolved, err := opts.Resolve("row_number", IntegerOption("hnswEfSearch"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetOption(resolved, IntegerOption("hnswEfSearch")); err != nil || !ok || v != 10 {
		t.Errorf("resolved hnswEfSearch = %v %v %v", v, ok, err)
	}
	b = NewCallSiteOptionsBuilder()
	_ = b.PutRaw("nope", int64(1))
	_, err = b.Build().Resolve("row_number", IntegerOption("hnswEfSearch"))
	if e := callSiteErrorOf(t, err); e.Code != CallSiteFunctionUndefined || e.Function != "row_number" || e.Option != "nope" ||
		e.Detail != "unsupported option for function" {
		t.Errorf("unsupported option: %+v", e)
	}
}

// row_number is encapsulated as RowNumberFn does: no positional arguments,
// the window's partitioning and ordering values, and its two options.
func TestEncapsulateRowNumber(t *testing.T) {
	t.Parallel()
	p := &ConstantValue{Typ: NotNullLong, Value: int64(1)}
	o := &ConstantValue{Typ: NotNullDouble, Value: 2.0}
	b := NewCallSiteOptionsBuilder()
	_ = b.PutRaw("hnswEfSearch", int64(40))
	_ = b.PutRaw("vectorReturnVectors", true)
	args := PositionalCallSite().
		WithWindow(WindowSpecification{Partitioning: []Value{p}, Ordering: []WindowOrderingPart{{Value: o}}}).
		WithOptions(b.Build())
	rn, err := EncapsulateRowNumber(args)
	if err != nil {
		t.Fatal(err)
	}
	if len(rn.PartitioningValues) != 1 || rn.PartitioningValues[0] != p || len(rn.ArgumentValues) != 1 || rn.ArgumentValues[0] != o ||
		rn.EfSearch == nil || *rn.EfSearch != 40 || rn.IsReturningVectors == nil || !*rn.IsReturningVectors {
		t.Errorf("row_number = %+v", rn)
	}
	if _, err := EncapsulateRowNumber(PositionalCallSite(p)); err == nil {
		t.Error("row_number accepted a positional argument")
	}
	if _, err := EncapsulateRowNumber(NamedCallSite([]string{"x"}, []Value{p})); err == nil {
		t.Error("row_number accepted named arguments")
	}
	b = NewCallSiteOptionsBuilder()
	_ = b.PutRaw("vectorReturnVectors", int64(1))
	if _, err := EncapsulateRowNumber(PositionalCallSite().WithOptions(b.Build())); err == nil ||
		callSiteErrorOf(t, err).Code != CallSiteIncompatibleType {
		t.Errorf("non-boolean RETURN_VECTORS: %v", err)
	}
}
