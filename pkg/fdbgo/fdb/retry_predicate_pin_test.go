package fdb

import "testing"

// allKnownRetryCodes is the fixed enumeration the pin test iterates so the
// assertion is "IsRetryable(code) == (code ∈ expectedSet)" for EVERY code — an
// EXHAUSTIVE check, not a sampled one (RFC-105).
var allKnownRetryCodes = []int{
	1000, 1001, 1004, 1006, 1007, 1008, 1009, 1010, 1011, 1015, 1020, 1021,
	1025, 1031, 1036, 1037, 1038, 1039, 1042, 1049, 1051, 1062, 1078, 1079,
	1101, 1200, 1213, 1223, 1235, 1242, 1500, 2000, 2002, 2004, 2006, 2007,
	2011, 2015, 2017, 2101,
}

// fdbErrorPredicateRetryable is the EXACT C++ fdb_error_predicate(RETRYABLE) set
// (bindings/c/fdb_c.cpp:78-94) = MAYBE_COMMITTED ∪ RETRYABLE_NOT_COMMITTED. This
// is a cross-client wire contract: a Go app querying fdb.IsRetryable must get
// libfdb_c's exact answer. It deliberately EXCLUDES the onError-only code 1079
// (blob_granule_request_failed) and the Go-only/forward-compat codes 1200/1235/
// 1242 — those are retried by the loop (client.onErrorRetryable), NOT reported by
// the predicate, mirroring C++ where onError ⊋ fdb_error_predicate (RFC-105).
var fdbErrorPredicateRetryable = map[int]bool{
	// MAYBE_COMMITTED (fdb_c.cpp:83-85):
	1021: true, // commit_unknown_result
	1039: true, // cluster_version_changed
	// RETRYABLE_NOT_COMMITTED (fdb_c.cpp:86-93):
	1020: true, // not_committed
	1007: true, // transaction_too_old
	1009: true, // future_version
	1038: true, // database_locked
	1078: true, // grv_proxy_memory_limit_exceeded
	1042: true, // commit_proxy_memory_limit_exceeded
	1051: true, // batch_transaction_throttled
	1037: true, // process_behind
	1213: true, // tag_throttled
	1223: true, // proxy_tag_throttled
}

// TestIsRetryable_PinsFDBErrorPredicate exhaustively pins fdb.IsRetryable to the
// 12-code C++ fdb_error_predicate(RETRYABLE) set. Adding or dropping any code
// (e.g. leaking the onError-only 1079, or a forward-compat 1235) turns this red.
func TestIsRetryable_PinsFDBErrorPredicate(t *testing.T) {
	t.Parallel()
	if len(fdbErrorPredicateRetryable) != 12 {
		t.Fatalf("expected set has %d codes, want 12 (fdb_error_predicate)", len(fdbErrorPredicateRetryable))
	}
	for _, code := range allKnownRetryCodes {
		if got := IsRetryable(code); got != fdbErrorPredicateRetryable[code] {
			t.Errorf("IsRetryable(%d) = %v, want %v (must equal fdb_error_predicate)", code, got, fdbErrorPredicateRetryable[code])
		}
	}
}

// onErrorRetrySet is the EXACT set fdb_transaction_on_error retries (resets + backs off):
// the fdb_error_predicate set PLUS the onError-only / Go-extension codes (1079 from C++
// Transaction::onError, 1039 via the MVC layer — already in the predicate set — and the
// Go-internal 1200, FDB-7.4+ 1235/1242). MUST mirror client.onErrorRetryable
// (commitpath.go:231); the libfdb_c backend uses fdb.IsOnErrorRetryable to decide whether a
// libfdb_c OnError has a backoff worth ctx-bounding.
var onErrorRetrySet = map[int]bool{
	1007: true, 1009: true, 1020: true, 1021: true, 1037: true, 1038: true,
	1039: true, 1042: true, 1051: true, 1078: true,
	1079: true, // blob_granule_request_failed — the key divergence from IsRetryable
	1200: true, // all_proxies_unreachable (Go-internal Layer-2)
	1213: true, 1223: true,
	1235: true, // transaction_throttled_hot_shard (FDB 7.4+)
	1242: true, // transaction_rejected_range_locked (FDB 7.4+)
}

// TestIsOnErrorRetryable_PinsOnErrorSet exhaustively pins fdb.IsOnErrorRetryable and asserts
// it is a STRICT SUPERSET of IsRetryable — the difference being exactly the onError-only /
// Go-extension codes. The headline case: IsOnErrorRetryable(1079) is true while
// IsRetryable(1079) is false, so the libfdb_c backend ctx-bounds 1079's backoff.
func TestIsOnErrorRetryable_PinsOnErrorSet(t *testing.T) {
	t.Parallel()
	for _, code := range allKnownRetryCodes {
		if got := IsOnErrorRetryable(code); got != onErrorRetrySet[code] {
			t.Errorf("IsOnErrorRetryable(%d) = %v, want %v (must equal client.onErrorRetryable)", code, got, onErrorRetrySet[code])
		}
		// Superset invariant: every IsRetryable code is also OnError-retryable.
		if IsRetryable(code) && !IsOnErrorRetryable(code) {
			t.Errorf("IsOnErrorRetryable must be a superset of IsRetryable, but %d is retryable yet not onError-retryable", code)
		}
	}
	if !IsOnErrorRetryable(1079) || IsRetryable(1079) {
		t.Fatalf("1079 must be onError-retryable (true) but not IsRetryable (false); got onError=%v predicate=%v",
			IsOnErrorRetryable(1079), IsRetryable(1079))
	}
}
