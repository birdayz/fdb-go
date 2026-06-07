package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUdfValue_Type(t *testing.T) {
	t.Parallel()
	u := NewUdfValue("MY_FN", NotNullLong, nil, nil)
	if !u.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", u.Type())
	}
}

func TestUdfValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	u := NewUdfValue("MY_FN", nil, nil, nil)
	if !u.Type().Equals(UnknownType) {
		t.Fatalf("Type = %v, want UnknownType", u.Type())
	}
}

func TestUdfValue_Name(t *testing.T) {
	t.Parallel()
	u := NewUdfValue("MY_FN", NotNullLong, nil, nil)
	if got := u.Name(); got != "MY_FN" {
		t.Fatalf("Name = %q, want MY_FN", got)
	}
}

func TestUdfValue_ChildrenAreArgs(t *testing.T) {
	t.Parallel()
	a := LiteralValue(int64(7))
	b := LiteralValue("hello")
	u := NewUdfValue("MY_FN", NotNullLong, []Value{a, b}, nil)
	cs := u.Children()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatalf("Children = %v, want [a, b]", cs)
	}
}

func TestUdfValue_EvaluateCallsUserFn(t *testing.T) {
	t.Parallel()
	// UDF that sums two int64 args.
	sumFn := func(args []any) any {
		var s int64
		for _, a := range args {
			if i, ok := a.(int64); ok {
				s += i
			}
		}
		return s
	}
	u := NewUdfValue("SUM",
		NotNullLong,
		[]Value{LiteralValue(int64(3)), LiteralValue(int64(4))},
		sumFn)
	got, errEv0 := u.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != int64(7) {
		t.Fatalf("Evaluate = %v, want 7", got)
	}
}

func TestUdfValue_NilCallReturnsNil(t *testing.T) {
	t.Parallel()
	u := NewUdfValue("MY_FN", NotNullLong, []Value{LiteralValue(int64(1))}, nil)
	got, errEv0 := u.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil Call) = %v, want nil (placeholder mode)", got)
	}
}

func TestUdfValue_PassesNilForNilArg(t *testing.T) {
	t.Parallel()
	// UDF that returns the first arg unchanged — used to verify nil
	// propagates through.
	identity := func(args []any) any { return args[0] }
	u := NewUdfValue("IDENTITY",
		NullableLong,
		[]Value{nil}, // nil child arg
		identity)
	got, errEv0 := u.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil child) = %v, want nil", got)
	}
}

func TestUdfValue_EvaluatesEachChildOncePerCall(t *testing.T) {
	t.Parallel()
	// A counter Value to verify each child is evaluated exactly once.
	count := 0
	counter := &counterValue{onEvaluate: func() { count++ }, val: int64(99)}
	concat := func(args []any) any {
		// Return the count of args (verifies all args were collected).
		return int64(len(args))
	}
	u := NewUdfValue("CONCAT", NotNullLong, []Value{counter, counter, counter}, concat)
	got, errEv0 := u.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != int64(3) {
		t.Fatalf("Evaluate len = %v, want 3", got)
	}
	if count != 3 {
		t.Fatalf("counter eval count = %d, want 3", count)
	}
}

func TestUdfValue_WithChildrenPreservesMetadata(t *testing.T) {
	t.Parallel()
	called := false
	original := NewUdfValue("MY_FN", NotNullLong, []Value{LiteralValue(int64(1))},
		func(args []any) any {
			called = true
			return args[0]
		})
	rebuilt := original.WithChildren([]Value{LiteralValue(int64(42))})
	if rebuilt.Name() != "MY_FN" {
		t.Fatalf("rebuilt.Name = %q, want MY_FN", rebuilt.Name())
	}
	if !rebuilt.Type().Equals(NotNullLong) {
		t.Fatalf("rebuilt.Type = %v, want NotNullLong", rebuilt.Type())
	}
	got, errEv0 := rebuilt.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != int64(42) {
		t.Fatalf("rebuilt.Evaluate = %v, want 42 (Call body carried through)", got)
	}
	if !called {
		t.Fatalf("Call body wasn't carried through to rebuilt")
	}
}

func TestUdfValue_DefensiveCopyOfArgs(t *testing.T) {
	t.Parallel()
	args := []Value{LiteralValue(int64(1))}
	u := NewUdfValue("MY_FN", NotNullLong, args, nil)
	// Mutate caller's slice.
	args[0] = LiteralValue(int64(999))
	tmpEv0, errEv0 := u.Args[0].Evaluate(nil)
	require.NoError(t, errEv0)
	if v, _ := tmpEv0.(int64); v == 999 {
		t.Fatalf("Args aliased caller's slice — not defensively copied")
	}
}

// counterValue is a tiny test helper Value that counts its
// Evaluate invocations.
type counterValue struct {
	onEvaluate func()
	val        any
}

func (c *counterValue) Children() []Value { return []Value{} }
func (*counterValue) Name() string        { return "counter" }
func (*counterValue) Type() Type          { return NotNullLong }
func (c *counterValue) Evaluate(any) (any, error) {
	c.onEvaluate()
	return c.val, nil
}
