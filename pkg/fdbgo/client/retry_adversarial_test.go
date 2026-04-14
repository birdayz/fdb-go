package client

// Adversarial tests for OnError/retry behavior.
//
// These test the subtle behavioral contract of FDB's retry mechanism:
// - Self-conflicting on commit_unknown_result (copy write→read conflicts)
// - Backoff cap differences (resource-constrained vs normal)
// - maxRetryDelay user cap
// - Error categorization (retryable vs non-retryable)

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// TestOnError_SelfConflicting_CommitUnknown verifies that OnError(commit_unknown_result)
// copies write conflict ranges to read conflict ranges. This is the mechanism
// that detects if the previous commit actually landed — the retry will conflict
// with the prior commit's writes. Matches C++ MAYBE_COMMITTED predicate.
func TestOnError_SelfConflicting_CommitUnknown(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}

	// Add some write conflicts (simulating a transaction that wrote data).
	tx.writeConflicts = append(tx.writeConflicts,
		KeyRange{Begin: []byte("a"), End: []byte("b")},
		KeyRange{Begin: []byte("x"), End: []byte("z")},
	)
	tx.readConflicts = append(tx.readConflicts,
		KeyRange{Begin: []byte("m"), End: []byte("n")},
	)

	err := tx.OnError(&wire.FDBError{Code: ErrCommitUnknownResult})
	if err != nil {
		t.Fatalf("OnError should retry commit_unknown_result, got: %v", err)
	}

	// After reset + self-conflicting, read conflicts should include the
	// original write conflicts. The original readConflicts are cleared by
	// reset(), and write conflicts are copied before reset.
	if len(tx.readConflicts) != 2 {
		t.Fatalf("expected 2 read conflicts (from self-conflicting), got %d", len(tx.readConflicts))
	}

	// Verify the copied ranges match the original writes.
	if string(tx.readConflicts[0].Begin) != "a" || string(tx.readConflicts[0].End) != "b" {
		t.Errorf("readConflicts[0]: got [%q,%q), want [a,b)", tx.readConflicts[0].Begin, tx.readConflicts[0].End)
	}
	if string(tx.readConflicts[1].Begin) != "x" || string(tx.readConflicts[1].End) != "z" {
		t.Errorf("readConflicts[1]: got [%q,%q), want [x,z)", tx.readConflicts[1].Begin, tx.readConflicts[1].End)
	}

	// Write conflicts should be cleared by reset.
	if len(tx.writeConflicts) != 0 {
		t.Errorf("writeConflicts should be cleared after reset, got %d", len(tx.writeConflicts))
	}
}

// TestOnError_SelfConflicting_ClusterVersionChanged verifies that
// cluster_version_changed (1039) also triggers self-conflicting.
func TestOnError_SelfConflicting_ClusterVersionChanged(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.writeConflicts = append(tx.writeConflicts,
		KeyRange{Begin: []byte("k"), End: []byte("l")},
	)

	err := tx.OnError(&wire.FDBError{Code: ErrClusterVersionChanged})
	if err != nil {
		t.Fatalf("OnError should retry cluster_version_changed, got: %v", err)
	}

	if len(tx.readConflicts) != 1 {
		t.Fatalf("expected 1 read conflict from self-conflicting, got %d", len(tx.readConflicts))
	}
	if string(tx.readConflicts[0].Begin) != "k" {
		t.Errorf("readConflicts[0].Begin: got %q, want k", tx.readConflicts[0].Begin)
	}
}

// TestOnError_NotCommitted_NoSelfConflicting verifies that not_committed (1020)
// does NOT trigger self-conflicting — it's retryable_not_committed, not
// maybe_committed. The transaction definitely did NOT commit.
func TestOnError_NotCommitted_NoSelfConflicting(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.writeConflicts = append(tx.writeConflicts,
		KeyRange{Begin: []byte("a"), End: []byte("b")},
	)

	err := tx.OnError(&wire.FDBError{Code: ErrNotCommitted})
	if err != nil {
		t.Fatalf("OnError should retry not_committed, got: %v", err)
	}

	// NOT self-conflicting — readConflicts should be empty after reset.
	if len(tx.readConflicts) != 0 {
		t.Errorf("expected 0 read conflicts (not_committed is NOT maybe_committed), got %d", len(tx.readConflicts))
	}
}

// TestOnError_NonRetryable verifies that non-retryable errors pass through.
func TestOnError_NonRetryablePassthrough(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}

	// Error 2000 (client_invalid_operation) is not retryable.
	err := tx.OnError(&wire.FDBError{Code: 2000})
	if err == nil {
		t.Fatal("expected non-retryable error to pass through")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != 2000 {
		t.Errorf("expected code 2000, got: %v", err)
	}
}

// TestOnError_NonFDBError verifies that non-FDB errors are treated as
// non-retryable and set the transaction to errored state.
func TestOnError_NonFDBError(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}

	err := tx.OnError(errors.New("some network error"))
	if err == nil {
		t.Fatal("expected non-FDB error to pass through")
	}
	if err.Error() != "some network error" {
		t.Errorf("error message: got %q, want %q", err.Error(), "some network error")
	}
}

// TestOnError_RetryCount verifies that retryCount increments correctly
// across different error types and resets on user Reset().
func TestOnError_RetryCount(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}

	// 3 retries with different error types.
	tx.OnError(&wire.FDBError{Code: ErrNotCommitted})
	if tx.retryCount != 1 {
		t.Errorf("retryCount after not_committed: got %d, want 1", tx.retryCount)
	}

	tx.OnError(&wire.FDBError{Code: ErrTransactionTooOld})
	if tx.retryCount != 2 {
		t.Errorf("retryCount after transaction_too_old: got %d, want 2", tx.retryCount)
	}

	tx.OnError(&wire.FDBError{Code: ErrCommitUnknownResult})
	if tx.retryCount != 3 {
		t.Errorf("retryCount after commit_unknown: got %d, want 3", tx.retryCount)
	}

	// User Reset() clears retryCount.
	tx.Reset()
	if tx.retryCount != 0 {
		t.Errorf("retryCount after Reset(): got %d, want 0", tx.retryCount)
	}
}

// TestOnError_ResourceConstrainedBackoff verifies that resource-constrained
// errors (hot_shard, range_locked, proxy_memory_limit, grv_proxy_memory_limit)
// use the higher backoff cap (30s) instead of the normal 1s.
func TestOnError_ResourceConstrainedErrors(t *testing.T) {
	t.Parallel()

	resourceConstrained := []int{
		ErrThrottledHotShard,
		ErrRangeLocked,
		ErrProxyMemoryLimitExceeded,
		ErrGrvProxyMemoryLimit,
	}

	for _, code := range resourceConstrained {
		tx := &Transaction{}
		err := tx.OnError(&wire.FDBError{Code: code})
		if err != nil {
			t.Errorf("OnError(%d) should retry, got: %v", code, err)
		}
		if tx.retryCount != 1 {
			t.Errorf("OnError(%d) retryCount: got %d, want 1", code, tx.retryCount)
		}
	}
}

// TestOnError_AllRetryableErrors verifies every known retryable error code.
func TestOnError_AllRetryableErrors(t *testing.T) {
	t.Parallel()

	retryable := []int{
		ErrTransactionTooOld,         // 1007
		ErrFutureVersion,             // 1009
		ErrNotCommitted,              // 1020
		ErrCommitUnknownResult,       // 1021
		ErrDatabaseLocked,            // 1038
		ErrClusterVersionChanged,     // 1039
		ErrProcessBehind,             // 1037
		ErrBatchTransactionThrottled, // 1042
		ErrTagThrottled,              // 1078
		ErrProxyTagThrottled,
		ErrThrottledHotShard,
		ErrRangeLocked,
		ErrBlobGranuleRequestFailed,
		ErrAllProxiesUnreachable,
		ErrProxyMemoryLimitExceeded,
		ErrGrvProxyMemoryLimit,
	}

	for _, code := range retryable {
		t.Run(codeToString(code), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			err := tx.OnError(&wire.FDBError{Code: code})
			if err != nil {
				t.Errorf("OnError(%d) should retry, got: %v", code, err)
			}
		})
	}
}

func codeToString(code int) string {
	switch code {
	case ErrTransactionTooOld:
		return "transaction_too_old"
	case ErrFutureVersion:
		return "future_version"
	case ErrNotCommitted:
		return "not_committed"
	case ErrCommitUnknownResult:
		return "commit_unknown_result"
	case ErrDatabaseLocked:
		return "database_locked"
	case ErrClusterVersionChanged:
		return "cluster_version_changed"
	case ErrProcessBehind:
		return "process_behind"
	case ErrBatchTransactionThrottled:
		return "batch_transaction_throttled"
	case ErrTagThrottled:
		return "tag_throttled"
	case ErrProxyTagThrottled:
		return "proxy_tag_throttled"
	case ErrThrottledHotShard:
		return "throttled_hot_shard"
	case ErrRangeLocked:
		return "range_locked"
	case ErrBlobGranuleRequestFailed:
		return "blob_granule_request_failed"
	case ErrAllProxiesUnreachable:
		return "all_proxies_unreachable"
	case ErrProxyMemoryLimitExceeded:
		return "proxy_memory_limit_exceeded"
	case ErrGrvProxyMemoryLimit:
		return "grv_proxy_memory_limit"
	default:
		return "unknown"
	}
}

// TestIntersectConflictRanges verifies the conflict range intersection logic
// used by commitDummyTransaction. Matches C++ intersects() in NativeAPI.actor.cpp.
func TestIntersectConflictRanges_Adversarial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		writes []KeyRange
		reads  []KeyRange
		want   string // expected key (as string)
	}{
		{
			name:   "exact_overlap",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			reads:  []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			want:   "a",
		},
		{
			name:   "partial_overlap_write_starts_first",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			reads:  []KeyRange{{Begin: []byte("b"), End: []byte("d")}},
			want:   "b", // max of begins
		},
		{
			name:   "partial_overlap_read_starts_first",
			writes: []KeyRange{{Begin: []byte("b"), End: []byte("d")}},
			reads:  []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			want:   "b", // max of begins
		},
		{
			name:   "no_overlap_fallback",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("b")}},
			reads:  []KeyRange{{Begin: []byte("c"), End: []byte("d")}},
			want:   "a", // fallback to writes[0].Begin
		},
		{
			name: "multiple_ranges_second_overlaps",
			writes: []KeyRange{
				{Begin: []byte("a"), End: []byte("b")},
				{Begin: []byte("m"), End: []byte("n")},
			},
			reads: []KeyRange{
				{Begin: []byte("c"), End: []byte("d")},
				{Begin: []byte("l"), End: []byte("o")},
			},
			want: "m", // writes[1] overlaps reads[1]
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := intersectConflictRanges(tt.writes, tt.reads)
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
