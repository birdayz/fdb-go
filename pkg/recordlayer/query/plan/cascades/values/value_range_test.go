package values

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRangeValue_Type(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(1)))
	rt, ok := v.Type().(*RecordType)
	if !ok || len(rt.Fields) != 1 || rt.Fields[0].Name != "ID" || !rt.Fields[0].FieldType.Equals(NotNullLong) {
		t.Fatalf("Type = %v, want (ID LONG)", v.Type())
	}
}

func TestRangeValue_Name(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(1)))
	if got := v.Name(); got != "range" {
		t.Fatalf("Name = %q, want range", got)
	}
}

func TestRangeValue_Children(t *testing.T) {
	t.Parallel()
	begin := LiteralValue(int64(0))
	end := LiteralValue(int64(10))
	step := LiteralValue(int64(2))
	v := NewRangeValue(begin, end, step)
	cs := v.Children()
	if len(cs) != 3 || cs[0] != begin || cs[1] != end || cs[2] != step {
		t.Fatalf("Children = %v, want [begin, end, step]", cs)
	}
}

func TestRangeValue_EvaluateIsPlaceholder(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(1)))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate = %v, want nil (streaming Value)", got)
	}
}

func TestRangeValue_EvaluateAsStream_BasicAscending(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(5)), LiteralValue(int64(1)))
	got := v.EvaluateAsStream(nil)
	want := []int64{0, 1, 2, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EvaluateAsStream = %v, want %v", got, want)
	}
}

func TestRangeValue_EvaluateAsStream_StepBy2(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(2)))
	got := v.EvaluateAsStream(nil)
	want := []int64{0, 2, 4, 6, 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EvaluateAsStream = %v, want %v", got, want)
	}
}

// Java's checkValidRange refuses a negative begin/end and a non-positive step.
func TestRangeValue_BoundsRejectsJavaInvalidRanges(t *testing.T) {
	t.Parallel()
	for _, b := range [][3]int64{{10, 0, -1}, {-1, 4, 1}, {0, -1, 1}, {1, 4, 0}} {
		v := NewRangeValue(LiteralValue(b[0]), LiteralValue(b[1]), LiteralValue(b[2]))
		_, _, _, err := v.Bounds(nil)
		var rbe *RangeBoundsError
		require.ErrorAs(t, err, &rbe, "bounds %v", b)
	}
}

func TestRangeValue_EvaluateAsStream_StepZeroReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(5)), LiteralValue(int64(0)))
	if got := v.EvaluateAsStream(nil); got != nil {
		t.Fatalf("EvaluateAsStream(step=0) = %v, want nil (infinite-loop guard)", got)
	}
}

func TestRangeValue_EvaluateAsStream_BeginEqualsEndEmpty(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(5)), LiteralValue(int64(5)), LiteralValue(int64(1)))
	got := v.EvaluateAsStream(nil)
	if len(got) != 0 {
		t.Fatalf("EvaluateAsStream(begin=end) = %v, want empty", got)
	}
}

func TestRangeValue_EvaluateAsStream_NonIntChildReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue("not-int"), LiteralValue(int64(5)), LiteralValue(int64(1)))
	if got := v.EvaluateAsStream(nil); got != nil {
		t.Fatalf("EvaluateAsStream(string begin) = %v, want nil", got)
	}
}

func TestRangeValue_Cardinality_Constants(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(1)))
	got, ok := v.Cardinality()
	if !ok {
		t.Fatalf("Cardinality not known despite all-constant children")
	}
	if got != 10 {
		t.Fatalf("Cardinality = %d, want 10", got)
	}
}

func TestRangeValue_Cardinality_StepBy2(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(10)), LiteralValue(int64(2)))
	got, ok := v.Cardinality()
	if !ok {
		t.Fatalf("Cardinality not known")
	}
	if got != 5 {
		t.Fatalf("Cardinality = %d, want 5", got)
	}
}

func TestRangeValue_Cardinality_NonInt(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue("not-int"), LiteralValue(int64(5)), LiteralValue(int64(1)))
	if _, ok := v.Cardinality(); ok {
		t.Fatalf("Cardinality returned ok=true for non-int begin")
	}
}

func TestRangeValue_Cardinality_StepZero(t *testing.T) {
	t.Parallel()
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(5)), LiteralValue(int64(0)))
	if _, ok := v.Cardinality(); ok {
		t.Fatalf("Cardinality returned ok=true for step=0")
	}
}
