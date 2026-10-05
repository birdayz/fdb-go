package values

import (
	"testing"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// A CAST takes its operand's nullability (CastValue.Type, Java
// ExpressionVisitor.java:532), which is sound only if a CAST never produces
// NULL from a non-NULL operand: each of Java's CastValue operators converts or
// throws. This drives every (source, target) pair the cast table admits with
// non-NULL operands and requires a value or an error, never NULL.
func TestCastValue_NonNullOperandNeverCastsToNull(t *testing.T) {
	t.Parallel()

	prim := func(code TypeCode) Type { return NewPrimitiveType(code, false) }
	enum := NewEnumType("E", false, []EnumValue{{Name: "A", Number: 1}})
	record := NewRecordType("R", false, []Field{{Name: "A", FieldType: prim(TypeCodeLong), Ordinal: 0}})
	otherRecord := NewRecordType("S", false, []Field{{Name: "B", FieldType: prim(TypeCodeString), Ordinal: 0}})
	longArray := NewArrayType(false, prim(TypeCodeLong))
	doubleArray := NewArrayType(false, prim(TypeCodeDouble))
	vector := NewVectorType(false, 32, 2)

	sources := []struct {
		typ      Type
		carriers []any
	}{
		{prim(TypeCodeBoolean), []any{true, false}},
		{prim(TypeCodeInt), []any{int64(1), int64(0)}},
		{prim(TypeCodeLong), []any{int64(1), int64(1) << 40}},
		{prim(TypeCodeFloat), []any{float64(1.5)}},
		{prim(TypeCodeDouble), []any{float64(1.5), float64(1e300)}},
		{prim(TypeCodeString), []any{
			"1", "1.5", "true", "A", "x", "zz", "",
			"2024-01-01", "2024-01-01 00:00:00", "123e4567-e89b-12d3-a456-426614174000",
		}},
		{prim(TypeCodeBytes), []any{[]byte{1}}},
		{prim(TypeCodeUuid), []any{[16]byte{1}}},
		{prim(TypeCodeDate), []any{"2024-01-01"}},
		{prim(TypeCodeTimestamp), []any{"2024-01-01 00:00:00"}},
		{enum, []any{int64(1)}},
		{record, []any{map[string]any{"A": int64(1)}}},
		{longArray, []any{[]any{int64(1), nil}, []any{}}},
		{vector, []any{vectorcodec.SerializeAs(vectorcodec.TypeSingle, []float64{1, 2})}},
	}
	targets := []Type{
		prim(TypeCodeBoolean), prim(TypeCodeInt), prim(TypeCodeLong), prim(TypeCodeFloat),
		prim(TypeCodeDouble), prim(TypeCodeString), prim(TypeCodeBytes), prim(TypeCodeUuid),
		prim(TypeCodeDate), prim(TypeCodeTimestamp), enum, record, otherRecord,
		longArray, doubleArray, vector,
	}

	pairs := 0
	for _, source := range sources {
		for _, target := range targets {
			if !CastTypesDefined(source.typ, target) {
				continue
			}
			pairs++
			cast := NewCastValue(nil, target)
			for _, carrier := range source.carriers {
				out, err := cast.castEvaluated(carrier, source.typ)
				if err == nil && out == nil {
					t.Errorf("CAST(%#v AS %v) from %v is NULL; want a value or an error", carrier, target, source.typ)
				}
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no admitted cast pair was driven")
	}
}
