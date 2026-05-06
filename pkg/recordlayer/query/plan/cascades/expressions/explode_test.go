package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestExplode_Construction(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)),
	})
	e := NewExplodeExpression(arr)
	if e.GetCollectionValue() != arr {
		t.Fatal("GetCollectionValue mismatch")
	}
	if got := e.GetQuantifiers(); len(got) != 0 {
		t.Fatalf("GetQuantifiers = %v, want empty", got)
	}
	if e.CanCorrelate() {
		t.Fatal("CanCorrelate = true, want false")
	}
}

func TestExplode_GetResultValueArrayElement(t *testing.T) {
	t.Parallel()
	// Array of NotNullLong → result type is QueriedValue of NotNullLong.
	arr := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)),
		values.LiteralValue(int64(2)),
	})
	e := NewExplodeExpression(arr)
	rv := e.GetResultValue()
	if !rv.Type().Equals(values.NotNullLong) {
		t.Fatalf("ResultValue type = %v, want NotNullLong (array element)", rv.Type())
	}
}

func TestExplode_GetResultValueNonArrayFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	// Non-array CollectionValue → result type falls back to UnknownType.
	e := NewExplodeExpression(values.LiteralValue(int64(1)))
	rv := e.GetResultValue()
	if !rv.Type().Equals(values.UnknownType) {
		t.Fatalf("ResultValue type = %v, want UnknownType (degenerate, non-array)", rv.Type())
	}
}

func TestExplode_GetResultValueNilCollection(t *testing.T) {
	t.Parallel()
	e := NewExplodeExpression(nil)
	rv := e.GetResultValue()
	if !rv.Type().Equals(values.UnknownType) {
		t.Fatalf("ResultValue type = %v, want UnknownType (nil collection)", rv.Type())
	}
}

func TestExplode_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayConstructorValue(values.NotNullLong, nil)
	e1 := NewExplodeExpression(arr)
	e2 := NewExplodeExpression(arr) // same pointer
	if !e1.EqualsWithoutChildren(e2, nil) {
		t.Fatal("two Explodes over same Value should be EqualsWithoutChildren")
	}
	// vs different expression type:
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	if e1.EqualsWithoutChildren(scan, nil) {
		t.Fatal("Explode should NOT equal Scan")
	}
}

func TestExplode_GetCorrelatedToFromCollectionValue(t *testing.T) {
	t.Parallel()
	// LiteralValue has no correlations — empty correlation set.
	e := NewExplodeExpression(values.LiteralValue(int64(1)))
	if got := e.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("GetCorrelatedTo over LiteralValue = %v, want empty", got)
	}
}

func TestExplode_HashCodeStable(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayConstructorValue(values.NotNullLong, nil)
	e := NewExplodeExpression(arr)
	h1 := e.HashCodeWithoutChildren()
	h2 := e.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren non-deterministic: %d vs %d", h1, h2)
	}
}

func TestExplode_HashCodeNilCollection(t *testing.T) {
	t.Parallel()
	e := NewExplodeExpression(nil)
	h := e.HashCodeWithoutChildren()
	// Non-zero — class discriminator constant.
	if h == 0 {
		t.Fatal("HashCodeWithoutChildren = 0, want non-zero class discriminator")
	}
}
