package values

import "testing"

// TestCast_StringNumericTrimMatchesJava pins RFC-189 C2 (finding 12b, premise
// corrected): numeric CAST(string AS INT/LONG/DOUBLE) delegates to
// Integer/Long/Double.parseXxx(s.trim()) in Java. Java's String.trim() strips
// ONLY code points <= U+0020, whereas Go's strings.TrimSpace strips the full
// Unicode whitespace set — so CAST(NBSP+'5' AS INT) yielded 5 in Go but throws
// in Java. The fix (trimJavaWhitespace) matches Java's trim set: ASCII space
// still trims in both; NBSP (U+00A0) is not stripped, so the parse errors.
func TestCast_StringNumericTrimMatchesJava(t *testing.T) {
	t.Parallel()
	const nbsp = " " // U+00A0, > U+0020: Java String.trim() does NOT strip it

	targets := []struct {
		name string
		typ  Type
	}{
		{"INT", NullableInt},
		{"LONG", NullableLong},
		{"DOUBLE", NullableDouble},
	}

	for _, tc := range targets {
		tc := tc
		t.Run("ascii_space_trims_"+tc.name, func(t *testing.T) {
			t.Parallel()
			// ASCII space (U+0020) is trimmed in BOTH engines → cast succeeds.
			cv := NewCastValue(&ConstantValue{Value: "  5  ", Typ: TypeString}, tc.typ)
			if _, err := cv.Evaluate(nil); err != nil {
				t.Fatalf("CAST('  5  ' AS %s) must succeed (ASCII space trims in both engines): %v", tc.name, err)
			}
		})
		t.Run("nbsp_rejected_"+tc.name, func(t *testing.T) {
			t.Parallel()
			// NBSP is NOT stripped by Java's trim(), so the numeric parse fails.
			cv := NewCastValue(&ConstantValue{Value: nbsp + "5", Typ: TypeString}, tc.typ)
			if _, err := cv.Evaluate(nil); err == nil {
				t.Fatalf("CAST(NBSP+'5' AS %s) must ERROR (Java does not strip NBSP, U+00A0)", tc.name)
			}
		})
	}
}
