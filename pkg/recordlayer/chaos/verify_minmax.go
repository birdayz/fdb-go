package chaos

import (
	"context"

	"fdb.dev/pkg/recordlayer"
)

// verifyMinMaxEverIndexes checks MIN_EVER_LONG and MAX_EVER_LONG index values
// against the model's tracked minimum/maximum-ever values.
func verifyMinMaxEverIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		switch idx.Type {
		case recordlayer.IndexTypeMaxEverLong:
			violations = append(violations, verifyMaxEverIndex(ctx, store, model, idx)...)
		case recordlayer.IndexTypeMinEverLong:
			violations = append(violations, verifyMinEverIndex(ctx, store, model, idx)...)
		}
	}

	return violations
}

// verifyMaxEverIndex verifies MAX_EVER_LONG index values against the model's
// tracked maximum-ever values.
func verifyMaxEverIndex(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, idx *recordlayer.Index) []Violation {
	prefix := idx.Name + ":"
	expected := make(map[string]int64)
	for key, val := range model.MaxEver {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			gk := key[len(prefix):]
			expected[gk] = val
		}
	}
	return compareAtomicValues(ctx, store, idx, expected, "max_ever_index")
}

// verifyMinEverIndex verifies MIN_EVER_LONG index values against the model's
// tracked minimum-ever values.
func verifyMinEverIndex(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, idx *recordlayer.Index) []Violation {
	prefix := idx.Name + ":"
	expected := make(map[string]int64)
	for key, val := range model.MinEver {
		if !model.minEverInitialized[key] {
			continue
		}
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			gk := key[len(prefix):]
			expected[gk] = val
		}
	}
	return compareAtomicValues(ctx, store, idx, expected, "min_ever_index")
}
