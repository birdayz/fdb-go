package values

import "testing"

// Benchmarks for the late-shift swingshift-59 Value ports — pin
// baseline allocation + ns/op numbers so future regressions surface
// in CI bench diff. Sized to run quickly under `just bench`.

func BenchmarkConditionSelectorValue_FirstTrueWins(b *testing.B) {
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true), // first TRUE — winner
		NewBooleanValue(true),
		NewBooleanValue(false),
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkConditionSelectorValue_AllFalse(b *testing.B) {
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(false),
		NewBooleanValue(false),
		NewBooleanValue(false),
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkPickValue_OverConditionSelector(b *testing.B) {
	// Full SQL CASE lowering: PickValue(ConditionSelector, alts).
	selector := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true),
		NewBooleanValue(false),
	})
	alts := []Value{
		LiteralValue("a"),
		LiteralValue("b"),
		LiteralValue("c"),
	}
	pick := NewPickValue(selector, alts, NotNullString)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pick.Evaluate(nil)
	}
}

func BenchmarkArrayConstructorValue_8Elements(b *testing.B) {
	v := NewArrayConstructorValue(NotNullLong, []Value{
		LiteralValue(int64(1)),
		LiteralValue(int64(2)),
		LiteralValue(int64(3)),
		LiteralValue(int64(4)),
		LiteralValue(int64(5)),
		LiteralValue(int64(6)),
		LiteralValue(int64(7)),
		LiteralValue(int64(8)),
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkArrayConstructorValue_Empty(b *testing.B) {
	v := NewArrayConstructorValue(NotNullLong, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkRangeValue_EvaluateAsStream_100Elements(b *testing.B) {
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(100)), LiteralValue(int64(1)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.EvaluateAsStream(nil)
	}
}

func BenchmarkRangeValue_Cardinality(b *testing.B) {
	v := NewRangeValue(LiteralValue(int64(0)), LiteralValue(int64(1000000)), LiteralValue(int64(1)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Cardinality()
	}
}

func BenchmarkRankValue_FromHarness(b *testing.B) {
	r := NewRankValue([]Value{LiteralValue("region")})
	row := map[string]any{"_rank": int64(5)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Evaluate(row)
	}
}

func BenchmarkRowNumberValue_FromHarness(b *testing.B) {
	r := NewRowNumberValue(nil, nil, nil, nil)
	row := map[string]any{"_row_number": int64(42)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Evaluate(row)
	}
}

func BenchmarkUdfValue_EvalSum2Args(b *testing.B) {
	sum := func(args []any) any {
		return args[0].(int64) + args[1].(int64)
	}
	v := NewUdfValue("SUM",
		NotNullLong,
		[]Value{LiteralValue(int64(3)), LiteralValue(int64(4))},
		sum)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkPatternForLikeValue_NoEscape(b *testing.B) {
	v := NewPatternForLikeValue(LiteralValue("a%b_c.d+e"), LiteralValue(nil))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkDistanceValue_Euclidean_64Dim(b *testing.B) {
	a := make([]float64, 64)
	c := make([]float64, 64)
	for i := range a {
		a[i] = float64(i) * 0.1
		c[i] = float64(64-i) * 0.1
	}
	v := NewDistanceValue(DistanceEuclidean, LiteralValue(a), LiteralValue(c))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkDistanceValue_Cosine_64Dim(b *testing.B) {
	a := make([]float64, 64)
	c := make([]float64, 64)
	for i := range a {
		a[i] = float64(i) * 0.1
		c[i] = float64(64-i) * 0.1
	}
	v := NewDistanceValue(DistanceCosine, LiteralValue(a), LiteralValue(c))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}
