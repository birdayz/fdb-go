package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"

	"fdb.dev/pkg/recordlayer"
)

// verifyAtomicIndexes checks COUNT, SUM, and COUNT_UPDATES index values
// against the model's expectations.
//
// For COUNT: expected value per grouping key = number of model records in that group.
// For SUM: expected value per grouping key = sum of field values in that group.
// For COUNT_UPDATES: expected value per grouping key = tracked cumulative events.
func verifyAtomicIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		switch idx.Type {
		case recordlayer.IndexTypeCount, recordlayer.IndexTypeCountNotNull:
			violations = append(violations, verifyCountIndex(ctx, store, model, md, idx)...)
		case recordlayer.IndexTypeSum:
			violations = append(violations, verifySumIndex(ctx, store, model, md, idx)...)
		case recordlayer.IndexTypeCountUpdates:
			violations = append(violations, verifyCountUpdatesIndex(ctx, store, model, idx)...)
		}
	}

	return violations
}

// verifyCountIndex verifies COUNT index values by computing expected counts
// from current model state.
func verifyCountIndex(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, md *recordlayer.RecordMetaData, idx *recordlayer.Index) []Violation {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}

	// Compute expected counts from model records.
	groupingCount := gke.GetGroupingCount()
	expected := make(map[string]int64) // packedGroupingKey -> count

	for _, rec := range model.Records {
		if !model.indexAppliesToType(idx, rec.TypeName) {
			continue
		}
		tuples, err := gke.Evaluate(nil, rec.Message)
		if err != nil {
			continue
		}
		for _, values := range tuples {
			gk := extractGroupingKey(values, groupingCount)
			expected[gk]++
		}
	}

	// Scan actual index entries and compare.
	return compareAtomicValues(ctx, store, idx, expected, "count_index")
}

// verifySumIndex verifies SUM index values by computing expected sums
// from current model state.
func verifySumIndex(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, md *recordlayer.RecordMetaData, idx *recordlayer.Index) []Violation {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}

	groupingCount := gke.GetGroupingCount()
	groupedCount := gke.GetGroupedCount()
	expected := make(map[string]int64) // packedGroupingKey -> sum

	for _, rec := range model.Records {
		if !model.indexAppliesToType(idx, rec.TypeName) {
			continue
		}
		tuples, err := gke.Evaluate(nil, rec.Message)
		if err != nil {
			continue
		}
		for _, values := range tuples {
			gk := extractGroupingKey(values, groupingCount)
			// The grouped (trailing) column at position groupingCount is the sum value.
			if groupingCount < len(values) && groupedCount > 0 {
				if v, ok := values[groupingCount].(int64); ok {
					expected[gk] += v
				}
			}
		}
	}

	return compareAtomicValues(ctx, store, idx, expected, "sum_index")
}

// verifyCountUpdatesIndex verifies COUNT_UPDATES index values against the model's
// cumulative event tracker.
func verifyCountUpdatesIndex(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, idx *recordlayer.Index) []Violation {
	prefix := idx.Name + ":"
	expected := make(map[string]int64)
	for key, count := range model.CountUpdates {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			gk := key[len(prefix):]
			expected[gk] = count
		}
	}

	return compareAtomicValues(ctx, store, idx, expected, "count_updates_index")
}

// compareAtomicValues scans an atomic index and compares values to expected.
func compareAtomicValues(ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, expected map[string]int64, invariantPrefix string) []Violation {
	var violations []Violation

	actual := make(map[string]int64)
	idxCursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

	for {
		result, err := idxCursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: invariantPrefix + "_scan_error",
				Expected:  fmt.Sprintf("index %q scannable", idx.Name),
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		gk := string(entry.Key.Pack())
		if len(entry.Value) > 0 {
			if v, ok := entry.Value[0].(int64); ok {
				actual[gk] = v
			}
		}
	}
	_ = idxCursor.Close()

	// Compare expected vs actual.
	for gk, expectedVal := range expected {
		actualVal, exists := actual[gk]
		if !exists {
			violations = append(violations, Violation{
				Invariant: invariantPrefix + "_missing",
				Expected:  fmt.Sprintf("index %q = %d", idx.Name, expectedVal),
				Actual:    "not in store",
			})
		} else if actualVal != expectedVal {
			violations = append(violations, Violation{
				Invariant: invariantPrefix + "_value_mismatch",
				Expected:  fmt.Sprintf("index %q = %d", idx.Name, expectedVal),
				Actual:    fmt.Sprintf("%d", actualVal),
			})
		}
	}

	// Check for unexpected entries in store.
	// Skip zero-value entries for non-ClearWhenZero indexes — FDB atomic ADD
	// leaves zero entries behind (this is correct behavior).
	for gk, actualVal := range actual {
		if _, exists := expected[gk]; !exists {
			if actualVal == 0 && !idx.IsClearWhenZero() {
				continue // expected: stale zero entry
			}
			violations = append(violations, Violation{
				Invariant: invariantPrefix + "_orphan",
				Expected:  fmt.Sprintf("index %q: no entry", idx.Name),
				Actual:    fmt.Sprintf("value=%d", actualVal),
			})
		}
	}

	return violations
}

// extractGroupingKey extracts the grouping portion (first groupingCount columns)
// from an evaluated key and returns it as a packed string.
func extractGroupingKey(values []any, groupingCount int) string {
	if groupingCount == 0 {
		return string(tuple.Tuple{}.Pack())
	}
	t := make(tuple.Tuple, groupingCount)
	for i := 0; i < groupingCount && i < len(values); i++ {
		t[i] = values[i]
	}
	return string(t.Pack())
}
