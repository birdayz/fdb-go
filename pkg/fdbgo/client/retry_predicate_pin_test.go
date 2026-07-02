package client

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// allKnownRetryCodes is the fixed enumeration the pin tests iterate so each
// assertion is "predicate(code) == (code ∈ expectedSet)" for EVERY code — an
// EXHAUSTIVE check, not a sampled one (RFC-105): a code spuriously added to
// (or dropped from) a predicate is caught whether or not it was hand-picked. Keep
// this list a superset of every code any retry predicate could classify.
var allKnownRetryCodes = []int{
	1000, 1001, 1004, 1006, 1007, 1008, 1009, 1010, 1011, 1015, 1020, 1021,
	1025, 1031, 1036, 1037, 1038, 1039, 1042, 1049, 1051, 1062, 1078, 1079,
	1101, 1200, 1213, 1223, 1235, 1242, 1500, 2000, 2002, 2004, 2006, 2007,
	2011, 2015, 2017, 2101,
}

// onErrorRetryableSet is the documented Go-onError-retryable set: C++
// Transaction::onError's reset+retry codes (NativeAPI.actor.cpp:7743-7747) + its
// version-delay codes (:7768) + the documented Go extensions. The pin asserts
// onErrorRetryable matches this EXACTLY. Because OnError guards on
// onErrorRetryable and client.commitDummyTransaction calls it, pinning this one
// function pins all three retry-loop sites (RFC-105 — derive, don't mirror).
var onErrorRetryableSet = map[int]bool{
	// C++ onError reset+retry (NativeAPI.actor.cpp:7743-7747):
	ErrNotCommitted:              true, // 1020
	ErrCommitUnknownResult:       true, // 1021
	ErrDatabaseLocked:            true, // 1038
	ErrProxyMemoryLimitExceeded:  true, // 1042 commit_proxy_memory_limit_exceeded
	ErrGrvProxyMemoryLimit:       true, // 1078
	ErrProcessBehind:             true, // 1037
	ErrBatchTransactionThrottled: true, // 1051
	ErrTagThrottled:              true, // 1213
	ErrBlobGranuleRequestFailed:  true, // 1079
	ErrProxyTagThrottled:         true, // 1223
	// C++ onError version-delay (:7768):
	ErrTransactionTooOld: true, // 1007
	ErrFutureVersion:     true, // 1009
	// Go extensions (documented in onErrorRetryable):
	ErrClusterVersionChanged: true, // 1039 — C++ retries in MVC layer (MultiVersionTransaction.actor.cpp:1740)
	ErrAllProxiesUnreachable: true, // 1200 — Go-internal Layer-2
	ErrThrottledHotShard:     true, // 1235 — FDB 7.4+
	ErrRangeLocked:           true, // 1242 — FDB 7.4+
}

// TestOnErrorRetryable_PinsOnErrorSet exhaustively pins onErrorRetryable to the
// Go-onError-retryable set. Flip any code → red. This is the drift sentinel for
// the retry loop AND commitDummyTransaction (both route through this function).
func TestOnErrorRetryable_PinsOnErrorSet(t *testing.T) {
	t.Parallel()
	if len(onErrorRetryableSet) != 16 {
		t.Fatalf("expected set has %d codes, want 16", len(onErrorRetryableSet))
	}
	for _, code := range allKnownRetryCodes {
		if got := onErrorRetryable(code); got != onErrorRetryableSet[code] {
			t.Errorf("onErrorRetryable(%d) = %v, want %v", code, got, onErrorRetryableSet[code])
		}
	}
}

// TestOnError_ClusterVersionChanged_RetriesSelfConflicting pins the Q1 ruling
// (RFC-105, FDB C++ dev): cluster_version_changed (1039) is retried by Go's
// OnError as MAYBE_COMMITTED self-conflicting — because C++ retries it in the
// MULTI-VERSION layer (MultiVersionTransaction.actor.cpp:1740), and Go has no MVC
// layer so OnError owns it. A future "fix" to the literal NativeAPI onError (which
// returns the error for 1039) would make this error out — this test goes red.
func TestOnError_ClusterVersionChanged_RetriesSelfConflicting(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.writeConflicts = append(tx.writeConflicts,
		KeyRange{Begin: []byte("a"), End: []byte("b")},
	)

	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrClusterVersionChanged})
	if err != nil {
		t.Fatalf("OnError should retry cluster_version_changed (1039), got: %v", err)
	}
	// Self-conflicting: the write range is deep-copied into readConflicts so the
	// retry detects whether the (maybe-committed) prior commit actually landed.
	if len(tx.readConflicts) != 1 {
		t.Fatalf("expected 1 self-conflicting read range, got %d", len(tx.readConflicts))
	}
	if string(tx.readConflicts[0].Begin) != "a" || string(tx.readConflicts[0].End) != "b" {
		t.Errorf("readConflicts[0] = [%q,%q), want [a,b)", tx.readConflicts[0].Begin, tx.readConflicts[0].End)
	}
}

// TestOnError_NonRetryable_ErrorsOut pins that a non-onErrorRetryable code (e.g.
// all_alternatives_failed 1006, which the deleted wire predicate wrongly retried)
// is NOT retried — OnError's guard returns it.
func TestOnError_NonRetryable_ErrorsOut(t *testing.T) {
	t.Parallel()
	for _, code := range []int{ErrAllAlternativesFailed, 2000} {
		tx := &Transaction{}
		if err := tx.OnError(context.Background(), &wire.FDBError{Code: code}); err == nil {
			t.Errorf("OnError(%d) returned nil (retried), want the error surfaced", code)
		}
	}
}
