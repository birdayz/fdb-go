package yamsql

import "testing"

// TestClassifyTest pins the three coverage buckets at their boundaries. The
// drift guard (TestSQLCoverageUpToDate) exercises the full pipeline on the real
// corpus; this unit test pins the classification rule itself so a boundary
// change (e.g. a new feature-gap SQLSTATE) is a deliberate edit, not a silent
// reclassification of hundreds of cases.
func TestClassifyTest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		test Test
		want Outcome
	}{
		{"rows positive", Test{Query: "SELECT 1", Rows: [][]any{{int64(1)}}}, OutcomeSupported},
		{"empty result positive", Test{Query: "SELECT 1 WHERE 1=0", Rows: [][]any{}}, OutcomeSupported},
		{"dml step no rows no error", Test{Query: "INSERT INTO t VALUES (1)"}, OutcomeSupported},
		{"whitespace-only code is supported", Test{Query: "SELECT 1", ErrorCode: "  "}, OutcomeSupported},
		{"feature gap 0A000", Test{Query: "x", ErrorCode: "0A000"}, OutcomeUnsupported},
		{"feature gap 0AF00", Test{Query: "x", ErrorCode: "0AF00"}, OutcomeUnsupported},
		{"feature gap 0AF01", Test{Query: "x", ErrorCode: "0AF01"}, OutcomeUnsupported},
		{"undefined function 42883", Test{Query: "x", ErrorCode: "42883"}, OutcomeUnsupported},
		{"feature gap case-insensitive", Test{Query: "x", ErrorCode: "0af00"}, OutcomeUnsupported},
		{"error-path unknown column 42703", Test{Query: "x", ErrorCode: "42703"}, OutcomeErrorPath},
		{"error-path overflow 22003", Test{Query: "x", ErrorCode: "22003"}, OutcomeErrorPath},
		{"error-path unique violation 23505", Test{Query: "x", ErrorCode: "23505"}, OutcomeErrorPath},
		{"error-path type mismatch 42804", Test{Query: "x", ErrorCode: "42804"}, OutcomeErrorPath},
		// Legacy Java `error:` alias must classify identically (codex finding:
		// an `error:` pin left ErrorCode empty and was mis-counted as supported).
		{"legacy error: feature gap", Test{Query: "x", Error: "0A000"}, OutcomeUnsupported},
		{"legacy error: error-path", Test{Query: "x", Error: "23505"}, OutcomeErrorPath},
		{"legacy error: unsupported function", Test{Query: "x", Error: "42883"}, OutcomeUnsupported},
		{"error_code wins over legacy error", Test{Query: "x", ErrorCode: "0A000", Error: "23505"}, OutcomeUnsupported},
	}
	for _, c := range cases {
		if got := classifyTest(c.test); got != c.want {
			t.Errorf("%s: classifyTest = %v, want %v", c.name, got, c.want)
		}
	}
}
