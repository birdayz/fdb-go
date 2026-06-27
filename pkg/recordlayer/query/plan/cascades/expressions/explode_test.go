package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// TestExplode_WithOrdinalityDistinct pins Graefe's RFC-142 concern: an ordinal
// and a non-ordinal Explode over the SAME array Value must NOT be conflated by
// EqualsWithoutChildren / HashCodeWithoutChildren — they produce different
// result shapes (a 2-field record vs the bare element), so the memo must keep
// them distinct (mirrors Java hashing/equals on (collectionValue, withOrdinality)).
func TestExplode_WithOrdinalityDistinct(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)),
	})
	plain := NewExplodeExpression(arr)
	ord := NewExplodeExpressionWithOrdinality(arr, true)

	if plain.EqualsWithoutChildren(ord, nil) {
		t.Fatal("ordinal and non-ordinal Explode over the same array must NOT be equal")
	}
	if ord.EqualsWithoutChildren(plain, nil) {
		t.Fatal("equality must be symmetric: ordinal != non-ordinal")
	}
	if plain.HashCodeWithoutChildren() == ord.HashCodeWithoutChildren() {
		t.Fatal("ordinal and non-ordinal Explode must hash differently")
	}
	if plain.GetWithOrdinality() || !ord.GetWithOrdinality() {
		t.Fatal("GetWithOrdinality flag mismatch")
	}

	// Two ordinal Explodes over the same array ARE equal.
	ord2 := NewExplodeExpressionWithOrdinality(arr, true)
	if !ord.EqualsWithoutChildren(ord2, nil) {
		t.Fatal("two WITH ORDINALITY Explodes over the same array should be equal")
	}
}

// TestExplode_OrdinalityResultType pins the WITH ORDINALITY result type: an
// anonymous 2-field record (element, INT NOT NULL), keyed _0 / _1.
func TestExplode_OrdinalityResultType(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)),
	})
	ord := NewExplodeExpressionWithOrdinality(arr, true)
	rt, ok := ord.GetExplodeResultType().(*values.RecordType)
	if !ok {
		t.Fatalf("ordinality result type = %T, want *RecordType", ord.GetExplodeResultType())
	}
	if len(rt.Fields) != 2 {
		t.Fatalf("ordinality record has %d fields, want 2", len(rt.Fields))
	}
	if rt.Fields[0].Name != values.OrdinalFieldName(0) || rt.Fields[1].Name != values.OrdinalFieldName(1) {
		t.Fatalf("ordinality field names = %q,%q, want _0,_1", rt.Fields[0].Name, rt.Fields[1].Name)
	}
	if rt.Fields[1].FieldType.Code() != values.TypeCodeInt || rt.Fields[1].FieldType.IsNullable() {
		t.Fatalf("ordinal field type = %v, want INT NOT NULL", rt.Fields[1].FieldType)
	}

	// Non-ordinal: bare element type (NotNullLong), not a record.
	plain := NewExplodeExpression(arr)
	if _, isRec := plain.GetExplodeResultType().(*values.RecordType); isRec {
		t.Fatal("non-ordinal Explode result type must be the bare element, not a record")
	}
}
