package fdb_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// BenchmarkIsRetryable_Retryable times the retryable-code path. The
// switch hits an early case for canonical 1021 (commit_unknown_result).
func BenchmarkIsRetryable_Retryable(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fdb.IsRetryable(1021)
	}
}

// BenchmarkIsRetryable_NonRetryable times the all-cases-miss path —
// the switch must fall through every case before returning false.
// Pins the upper bound on a per-error retry classification.
func BenchmarkIsRetryable_NonRetryable(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fdb.IsRetryable(2101) // transaction_too_large
	}
}

// BenchmarkErrorString_Known times the description lookup hot path.
// errorDescriptions has 343 entries; map lookup is O(1) but the
// comparison against `desc` and the fmt.Sprintf are visible in
// per-error logging.
func BenchmarkErrorString_Known(b *testing.B) {
	b.ReportAllocs()
	e := fdb.Error{Code: 1007}
	for i := 0; i < b.N; i++ {
		_ = e.Error()
	}
}

// BenchmarkErrorString_Unknown times the fall-through path (unknown
// code). Hits the !ok branch + the second fmt.Sprintf.
func BenchmarkErrorString_Unknown(b *testing.B) {
	b.ReportAllocs()
	e := fdb.Error{Code: 99999}
	for i := 0; i < b.N; i++ {
		_ = e.Error()
	}
}
