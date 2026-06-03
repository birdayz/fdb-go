package recordlayer

import (
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

func TestIsIndexIdempotent(t *testing.T) {
	t.Parallel()

	idxOfType := func(typ string) *Index {
		return &Index{Type: typ}
	}

	idempotent := []string{
		IndexTypeValue,
		IndexTypeMinEverLong,
		IndexTypeMaxEverLong,
		IndexTypeMinEverTuple,
		IndexTypeMaxEverTuple,
		IndexTypeMaxEverVersion,
		IndexTypeVersion,
		IndexTypePermutedMin,
		IndexTypePermutedMax,
	}
	for _, indexType := range idempotent {
		if !isIndexIdempotent(idxOfType(indexType)) {
			t.Errorf("expected %s to be idempotent", indexType)
		}
	}

	nonIdempotent := []string{
		IndexTypeCount,
		IndexTypeCountNotNull,
		IndexTypeCountUpdates,
		IndexTypeSum,
	}
	for _, indexType := range nonIdempotent {
		if isIndexIdempotent(idxOfType(indexType)) {
			t.Errorf("expected %s to be non-idempotent", indexType)
		}
	}

	// Unknown types should be conservative (non-idempotent).
	if isIndexIdempotent(idxOfType("UNKNOWN_TYPE")) {
		t.Error("unknown types should default to non-idempotent")
	}

	// RANK without CountDuplicates is idempotent.
	rankIdx := idxOfType(IndexTypeRank)
	if !isIndexIdempotent(rankIdx) {
		t.Error("RANK without CountDuplicates should be idempotent")
	}

	// RANK with CountDuplicates is NOT idempotent.
	// Matches Java's RankIndexMaintainer.isIdempotent() = !config.isCountDuplicates().
	rankWithDups := &Index{Type: IndexTypeRank, Options: map[string]string{
		IndexOptionRankCountDuplicates: "true",
	}}
	if isIndexIdempotent(rankWithDups) {
		t.Error("RANK with CountDuplicates=true should NOT be idempotent")
	}
}

func TestShouldLessenWork(t *testing.T) {
	t.Parallel()

	// FDB errors that should trigger limit reduction — Java
	// IndexingThrottle.lessenWorkCodes 1:1 (RFC-067), codes from
	// flow/error_definitions.h.
	lessenCodes := []int{
		1004, // timed_out
		1007, // transaction_too_old
		1020, // not_committed (conflict)
		1031, // transaction_timed_out
		2002, // commit_read_incomplete
		2101, // transaction_too_large
	}
	for _, code := range lessenCodes {
		err := fdb.Error{Code: code}
		if !shouldLessenWork(err) {
			t.Errorf("expected shouldLessenWork=true for FDB error code %d", code)
		}
		// Also works when wrapped.
		wrapped := fmt.Errorf("wrapped: %w", err)
		if !shouldLessenWork(wrapped) {
			t.Errorf("expected shouldLessenWork=true for wrapped FDB error code %d", code)
		}
	}

	// FDB errors that should NOT trigger limit reduction. Includes the codes
	// the OLD (buggy) list wrongly whitelisted (RFC-067): 1028 is
	// new_coordinators_timed_out, 1039 is cluster_version_changed, and 2501 is
	// not transaction_timed_out — none are in Java's lessenWorkCodes.
	noLessenCodes := []int{
		1009, // future_version (retryable but not workload-related)
		1028, // new_coordinators_timed_out (was wrongly whitelisted)
		1038, // database_locked
		1039, // cluster_version_changed (was wrongly whitelisted)
		1070, // not_writable (permanent)
		2000, // io_error
		2501, // not a lessen-work code (was wrongly whitelisted)
	}
	for _, code := range noLessenCodes {
		if shouldLessenWork(fdb.Error{Code: code}) {
			t.Errorf("expected shouldLessenWork=false for FDB error code %d", code)
		}
	}

	// Non-FDB errors should not trigger limit reduction.
	if shouldLessenWork(fmt.Errorf("some random error")) {
		t.Error("non-FDB errors should not trigger limit reduction")
	}
}
