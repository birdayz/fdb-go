package recordlayer

import "testing"

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
