package embedded

import (
	"database/sql/driver"
	"testing"
)

func BenchmarkValuesEqual_Int64(b *testing.B) {
	for b.Loop() {
		_ = valuesEqual(int64(42), int64(42))
	}
}

func BenchmarkValuesEqual_Float64(b *testing.B) {
	for b.Loop() {
		_ = valuesEqual(float64(3.14), float64(3.14))
	}
}

func BenchmarkValuesEqual_String(b *testing.B) {
	for b.Loop() {
		_ = valuesEqual("hello world", "hello world")
	}
}

func BenchmarkValuesEqual_MixedNumeric(b *testing.B) {
	for b.Loop() {
		_ = valuesEqual(int64(42), float64(42.0))
	}
}

func BenchmarkValuesEqual_Bytes(b *testing.B) {
	a := []byte("hello world this is a test")
	c := []byte("hello world this is a test")
	for b.Loop() {
		_ = valuesEqual(a, c)
	}
}

func BenchmarkValuesComparable_SameType(b *testing.B) {
	for b.Loop() {
		_ = valuesComparable(int64(1), int64(2))
	}
}

func BenchmarkValuesComparable_MixedNumeric(b *testing.B) {
	for b.Loop() {
		_ = valuesComparable(int64(1), float64(2.0))
	}
}

func BenchmarkValuesComparable_CrossType(b *testing.B) {
	for b.Loop() {
		_ = valuesComparable(int64(1), "hello")
	}
}

func BenchmarkNullSafeEqual_BothNil(b *testing.B) {
	for b.Loop() {
		_ = nullSafeEqual(nil, nil)
	}
}

func BenchmarkNullSafeEqual_NonNil(b *testing.B) {
	for b.Loop() {
		_ = nullSafeEqual(int64(42), int64(42))
	}
}

func BenchmarkRowKey_Simple(b *testing.B) {
	row := []driver.Value{int64(1), "hello", true}
	for b.Loop() {
		_ = rowKey(row)
	}
}

func BenchmarkRowKey_Wide(b *testing.B) {
	row := []driver.Value{int64(1), "hello", float64(3.14), true, nil, []byte{1, 2, 3}, "world", int64(42)}
	for b.Loop() {
		_ = rowKey(row)
	}
}

func BenchmarkSubstituteParams_None(b *testing.B) {
	for b.Loop() {
		_, _ = substituteParams("SELECT * FROM t", nil)
	}
}

func BenchmarkSubstituteParams_Three(b *testing.B) {
	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(42)},
		{Ordinal: 2, Value: "hello"},
		{Ordinal: 3, Value: nil},
	}
	for b.Loop() {
		_, _ = substituteParams("SELECT * FROM t WHERE id = ? AND name = ? AND deleted = ?", args)
	}
}
