package wire

import "testing"

// TestFDBError_Retryable_Canonical pins the 12 canonical retryable
// codes — same set as fdb.IsRetryable; this is the lower bound.
func TestFDBError_Retryable_Canonical(t *testing.T) {
	t.Parallel()
	for _, code := range []int{
		// MAYBE_COMMITTED
		1021, 1039,
		// RETRYABLE_NOT_COMMITTED
		1007, 1009, 1020, 1037, 1038, 1042, 1051, 1078, 1213, 1223,
	} {
		e := &FDBError{Code: code}
		if !e.Retryable() {
			t.Errorf("code %d expected retryable=true (canonical), got false", code)
		}
	}
}

// TestFDBError_Retryable_WireSideAdditions pins the four wire-level
// additions documented in reader.go's Retryable doc — these are the
// reasons the wire predicate is a SUPERSET of fdb.IsRetryable.
func TestFDBError_Retryable_WireSideAdditions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		why  string
	}{
		{1006, "all_alternatives_failed (Layer 2 retry)"},
		{1200, "all_proxies_unreachable (Go-internal)"},
		{1235, "transaction_throttled_hot_shard (FDB 7.4+)"},
		{1242, "transaction_rejected_range_locked (FDB 7.4+)"},
	}
	for _, tc := range cases {
		e := &FDBError{Code: tc.code}
		if !e.Retryable() {
			t.Errorf("code %d (%s) expected retryable=true, got false", tc.code, tc.why)
		}
	}
}

// TestFDBError_Retryable_NonRetryable pins codes that MUST NOT be
// retryable — critical for users who set transaction timeouts or
// who rely on definitive failure modes.
func TestFDBError_Retryable_NonRetryable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		why  string
	}{
		{1031, "transaction_timed_out — NEVER retryable"},
		{1025, "transaction_cancelled"},
		{2101, "transaction_too_large (client error)"},
		{2018, "invalid_mutation_type"},
		{2131, "tenant_not_found"},
		{4000, "unknown_error (4xxx internal)"},
		{6000, "permission_denied (6xxx auth)"},
	}
	for _, tc := range cases {
		e := &FDBError{Code: tc.code}
		if e.Retryable() {
			t.Errorf("code %d (%s) expected retryable=false, got true", tc.code, tc.why)
		}
	}
}

// TestFDBError_Retryable_Unknown pins the fail-safe default — unknown
// codes return false so a hypothetical FDB-future code never gets
// silently retried.
func TestFDBError_Retryable_Unknown(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 99999, -1, 9999, 5000} {
		e := &FDBError{Code: code}
		if e.Retryable() {
			t.Errorf("unknown code %d expected retryable=false, got true", code)
		}
	}
}
