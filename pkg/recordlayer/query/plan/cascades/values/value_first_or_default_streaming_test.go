package values

import "testing"

func TestFirstOrDefaultStreamingValue_TypeFromChild(t *testing.T) {
	t.Parallel()
	r := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(1)))
	v := NewFirstOrDefaultStreamingValue(r, LiteralValue(int64(-1)))
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong (from RangeValue child)", v.Type())
	}
}

func TestFirstOrDefaultStreamingValue_NilChildFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewFirstOrDefaultStreamingValue(nil, LiteralValue(int64(0)))
	if !v.Type().Equals(UnknownType) {
		t.Fatalf("Type = %v, want UnknownType (nil child fallback)", v.Type())
	}
}

func TestFirstOrDefaultStreamingValue_Name(t *testing.T) {
	t.Parallel()
	v := NewFirstOrDefaultStreamingValue(nil, nil)
	if got := v.Name(); got != "firstOrDefault" {
		t.Fatalf("Name = %q, want firstOrDefault", got)
	}
}

func TestFirstOrDefaultStreamingValue_Children(t *testing.T) {
	t.Parallel()
	r := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(5)), LiteralValue(int64(1)))
	d := LiteralValue(int64(-1))
	v := NewFirstOrDefaultStreamingValue(r, d)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != r || cs[1] != d {
		t.Fatalf("Children = %v, want [child, default]", cs)
	}
}

func TestFirstOrDefaultStreamingValue_NonEmptyRange(t *testing.T) {
	t.Parallel()
	// range(5, 10, 1) = [5, 6, 7, 8, 9]; first = 5
	r := NewRangeValue(LiteralValue(int64(5)), LiteralValue(int64(10)), LiteralValue(int64(1)))
	v := NewFirstOrDefaultStreamingValue(r, LiteralValue(int64(-1)))
	if got := mustEvaluate(v, nil); got != int64(5) {
		t.Fatalf("Evaluate non-empty = %v, want 5", got)
	}
}

func TestFirstOrDefaultStreamingValue_EmptyRangeReturnsDefault(t *testing.T) {
	t.Parallel()
	// range(0, 0, 1) is empty (begin == end); default kicks in.
	r := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(0)), LiteralValue(int64(1)))
	v := NewFirstOrDefaultStreamingValue(r, LiteralValue(int64(-1)))
	if got := mustEvaluate(v, nil); got != int64(-1) {
		t.Fatalf("Evaluate empty = %v, want -1 (default)", got)
	}
}

func TestFirstOrDefaultStreamingValue_NonRangeChildReturnsNil(t *testing.T) {
	t.Parallel()
	// Non-streaming child → placeholder returns nil.
	v := NewFirstOrDefaultStreamingValue(LiteralValue(int64(7)), LiteralValue(int64(-1)))
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Evaluate(non-RangeValue) = %v, want nil (placeholder)", got)
	}
}

func TestFirstOrDefaultStreamingValue_NilDefaultOnEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	r := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(0)), LiteralValue(int64(1)))
	v := NewFirstOrDefaultStreamingValue(r, nil)
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Evaluate empty + nil default = %v, want nil", got)
	}
}

func TestFirstOrDefaultStreamingValue_WithChildren(t *testing.T) {
	t.Parallel()
	original := NewFirstOrDefaultStreamingValue(
		NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(0)), LiteralValue(int64(1))),
		LiteralValue(int64(-1)))
	rebuilt := original.WithChildren([]Value{
		NewRangeValue(LiteralValue(int64(100)), LiteralValue(int64(105)), LiteralValue(int64(1))),
		LiteralValue(int64(-2)),
	})
	if got := mustEvaluate(rebuilt, nil); got != int64(100) {
		t.Fatalf("rebuilt.Evaluate = %v, want 100 (first of new range)", got)
	}
}
