package recordlayer

import (
	"fmt"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

func TestIsIndexTypeIdempotent(t *testing.T) {
	t.Parallel()

	idempotent := []string{
		IndexTypeValue,
		IndexTypeRank,
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
		if !isIndexTypeIdempotent(indexType) {
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
		if isIndexTypeIdempotent(indexType) {
			t.Errorf("expected %s to be non-idempotent", indexType)
		}
	}

	// Unknown types should be conservative (non-idempotent).
	if isIndexTypeIdempotent("UNKNOWN_TYPE") {
		t.Error("unknown types should default to non-idempotent")
	}
}

func TestShouldLessenWork(t *testing.T) {
	t.Parallel()

	// FDB errors that should trigger limit reduction (transaction too big/slow).
	lessenCodes := []int{
		1007, // transaction_too_old
		1020, // not_committed (conflict)
		1028, // transaction_too_large
		1031, // timed_out
		1039, // commit_read_incomplete
		2501, // transaction_timed_out
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

	// FDB errors that should NOT trigger limit reduction.
	noLessenCodes := []int{
		1009, // future_version (retryable but not workload-related)
		1038, // database_locked
		1070, // not_writable (permanent)
		2000, // io_error
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
