package api

import "testing"

// Baselines for DataType construction + comparison. These will seed
// decisions during Phase 4 (cascades) where Equal / WithNullable /
// Resolve land on the plan-cache hot path.

func BenchmarkIntegerTypeConstruct(b *testing.B) {
	// Singleton — should be zero-alloc.
	b.ReportAllocs()
	var sink *IntegerType
	for i := 0; i < b.N; i++ {
		sink = NewIntegerType(false)
	}
	_ = sink
}

func BenchmarkArrayTypeConstruct(b *testing.B) {
	// Composite — allocates per call.
	elem := NewIntegerType(false)
	b.ReportAllocs()
	var sink *ArrayType
	for i := 0; i < b.N; i++ {
		sink = NewArrayType(elem, false)
	}
	_ = sink
}

func BenchmarkStructTypeConstruct(b *testing.B) {
	fields := []StructField{
		NewStructField("a", NewLongType(false), 0),
		NewStructField("b", NewStringType(false), 1),
	}
	b.ReportAllocs()
	var sink *StructType
	for i := 0; i < b.N; i++ {
		sink = NewStructType("X", fields, false)
	}
	_ = sink
}

func BenchmarkIntegerTypeEqualSame(b *testing.B) {
	a, c := NewIntegerType(false), NewIntegerType(false)
	b.ReportAllocs()
	var sink bool
	for i := 0; i < b.N; i++ {
		sink = a.Equal(c)
	}
	_ = sink
}

func BenchmarkStructTypeEqualSame(b *testing.B) {
	fields := []StructField{
		NewStructField("a", NewLongType(false), 0),
		NewStructField("b", NewStringType(false), 1),
		NewStructField("c", NewIntegerType(true), 2),
	}
	x := NewStructType("T", fields, false)
	y := NewStructType("T", fields, false)
	b.ReportAllocs()
	var sink bool
	for i := 0; i < b.N; i++ {
		sink = x.Equal(y)
	}
	_ = sink
}

func BenchmarkStructTypeWithNullable(b *testing.B) {
	fields := []StructField{
		NewStructField("a", NewLongType(false), 0),
	}
	x := NewStructType("T", fields, false)
	b.ReportAllocs()
	var sink DataType
	for i := 0; i < b.N; i++ {
		// Alternating flip — half zero-alloc (same nullability), half
		// allocates. Average reflects both.
		if i%2 == 0 {
			sink = x.WithNullable(true)
		} else {
			sink = x.WithNullable(false)
		}
	}
	_ = sink
}

func BenchmarkIntegerTypeString(b *testing.B) {
	x := NewIntegerType(false)
	b.ReportAllocs()
	var sink string
	for i := 0; i < b.N; i++ {
		sink = x.String()
	}
	_ = sink
}

func BenchmarkStructTypeString(b *testing.B) {
	fields := []StructField{
		NewStructField("a", NewLongType(false), 0),
		NewStructField("b", NewStringType(true), 1),
	}
	x := NewStructType("Table1", fields, false)
	b.ReportAllocs()
	var sink string
	for i := 0; i < b.N; i++ {
		sink = x.String()
	}
	_ = sink
}
