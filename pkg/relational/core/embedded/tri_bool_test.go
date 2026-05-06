package embedded

import "testing"

func TestTriFromBool_True(t *testing.T) {
	t.Parallel()
	if triFromBool(true) != triTrue {
		t.Error("triFromBool(true) should be triTrue")
	}
}

func TestTriFromBool_False(t *testing.T) {
	t.Parallel()
	if triFromBool(false) != triFalse {
		t.Error("triFromBool(false) should be triFalse")
	}
}

func TestTriBool_IsTrue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		val  triBool
		want bool
	}{
		{triTrue, true},
		{triFalse, false},
		{triNull, false},
	}
	for _, tc := range tests {
		if tc.val.IsTrue() != tc.want {
			t.Errorf("(%d).IsTrue() = %v, want %v", tc.val, tc.val.IsTrue(), tc.want)
		}
	}
}

func TestTriBool_Not(t *testing.T) {
	t.Parallel()
	tests := []struct {
		val  triBool
		want triBool
	}{
		{triTrue, triFalse},
		{triFalse, triTrue},
		{triNull, triNull},
	}
	for _, tc := range tests {
		got := tc.val.Not()
		if got != tc.want {
			t.Errorf("(%d).Not() = %d, want %d", tc.val, got, tc.want)
		}
	}
}

func TestTriBool_DoubleNot(t *testing.T) {
	t.Parallel()
	for _, val := range []triBool{triTrue, triFalse, triNull} {
		if val.Not().Not() != val {
			t.Errorf("NOT NOT %d != %d", val, val)
		}
	}
}

func TestTriAnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b triBool
		want triBool
	}{
		// TRUE AND x
		{triTrue, triTrue, triTrue},
		{triTrue, triFalse, triFalse},
		{triTrue, triNull, triNull},
		// FALSE AND x — always FALSE (short-circuit)
		{triFalse, triTrue, triFalse},
		{triFalse, triFalse, triFalse},
		{triFalse, triNull, triFalse},
		// NULL AND x
		{triNull, triTrue, triNull},
		{triNull, triFalse, triFalse},
		{triNull, triNull, triNull},
	}
	for _, tc := range tests {
		got := triAnd(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("triAnd(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTriOr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b triBool
		want triBool
	}{
		// TRUE OR x — always TRUE (short-circuit)
		{triTrue, triTrue, triTrue},
		{triTrue, triFalse, triTrue},
		{triTrue, triNull, triTrue},
		// FALSE OR x
		{triFalse, triTrue, triTrue},
		{triFalse, triFalse, triFalse},
		{triFalse, triNull, triNull},
		// NULL OR x
		{triNull, triTrue, triTrue},
		{triNull, triFalse, triNull},
		{triNull, triNull, triNull},
	}
	for _, tc := range tests {
		got := triOr(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("triOr(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTriAnd_Commutative(t *testing.T) {
	t.Parallel()
	for _, a := range []triBool{triTrue, triFalse, triNull} {
		for _, b := range []triBool{triTrue, triFalse, triNull} {
			if triAnd(a, b) != triAnd(b, a) {
				t.Errorf("AND not commutative for (%d, %d)", a, b)
			}
		}
	}
}

func TestTriOr_Commutative(t *testing.T) {
	t.Parallel()
	for _, a := range []triBool{triTrue, triFalse, triNull} {
		for _, b := range []triBool{triTrue, triFalse, triNull} {
			if triOr(a, b) != triOr(b, a) {
				t.Errorf("OR not commutative for (%d, %d)", a, b)
			}
		}
	}
}

func TestTriDeMorgan(t *testing.T) {
	t.Parallel()
	for _, a := range []triBool{triTrue, triFalse, triNull} {
		for _, b := range []triBool{triTrue, triFalse, triNull} {
			// NOT (A AND B) = (NOT A) OR (NOT B)
			lhs := triAnd(a, b).Not()
			rhs := triOr(a.Not(), b.Not())
			if lhs != rhs {
				t.Errorf("De Morgan AND: NOT(%d AND %d)=%d, NOT(%d) OR NOT(%d)=%d",
					a, b, lhs, a, b, rhs)
			}
			// NOT (A OR B) = (NOT A) AND (NOT B)
			lhs = triOr(a, b).Not()
			rhs = triAnd(a.Not(), b.Not())
			if lhs != rhs {
				t.Errorf("De Morgan OR: NOT(%d OR %d)=%d, NOT(%d) AND NOT(%d)=%d",
					a, b, lhs, a, b, rhs)
			}
		}
	}
}

func TestTriConstants(t *testing.T) {
	t.Parallel()
	if triFalse != 0 {
		t.Errorf("triFalse = %d, want 0", triFalse)
	}
	if triTrue != 1 {
		t.Errorf("triTrue = %d, want 1", triTrue)
	}
	if triNull != 2 {
		t.Errorf("triNull = %d, want 2", triNull)
	}
}

func BenchmarkTriAnd(b *testing.B) {
	for b.Loop() {
		_ = triAnd(triNull, triTrue)
	}
}

func BenchmarkTriOr(b *testing.B) {
	for b.Loop() {
		_ = triOr(triNull, triFalse)
	}
}

func BenchmarkTriNot(b *testing.B) {
	for b.Loop() {
		_ = triNull.Not()
	}
}
