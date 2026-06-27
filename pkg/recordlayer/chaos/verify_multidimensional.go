package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyMultidimensionalIndexes checks MULTIDIMENSIONAL indexes by comparing
// expected entries (computed from the model) against actual R-tree scan results.
// Comparison is set-based because R-tree scan returns items in Hilbert order,
// not FDB key order.
func verifyMultidimensionalIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeMultidimensional {
			continue
		}

		// Build expected entries from model records.
		expected := make(map[string]tuple.Tuple)

		for _, rec := range model.Records {
			if !model.indexAppliesToType(idx, rec.TypeName) {
				continue
			}

			storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
				PrimaryKey: rec.PrimaryKey,
				RecordType: md.GetRecordType(rec.TypeName),
				Record:     rec.Message,
			}

			tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "multidimensional_index_eval_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q evaluable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			trimmedPK, err := idx.TrimPrimaryKey(rec.PrimaryKey)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "multidimensional_index_trim_pk_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q pk trimmable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			for _, values := range tuples {
				// The scan returns Key = [prefix..., dims..., suffix..., trimmedPK...].
				// We mirror that construction here.
				entryKey := make(tuple.Tuple, 0, len(values)+len(trimmedPK))
				for _, v := range values {
					entryKey = append(entryKey, v)
				}
				entryKey = append(entryKey, trimmedPK...)
				expected[string(entryKey.Pack())] = entryKey
			}
		}

		// Scan actual R-tree entries via ScanIndex.
		actual := make(map[string]*recordlayer.IndexEntry)
		idxCursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

		for {
			result, err := idxCursor.OnNext(ctx)
			if err != nil {
				violations = append(violations, Violation{
					Invariant: "multidimensional_index_scan_error",
					Expected:  fmt.Sprintf("index %q scannable", idx.Name),
					Actual:    err.Error(),
				})
				break
			}
			if !result.HasNext() {
				break
			}
			entry := result.GetValue()
			actual[string(entry.Key.Pack())] = entry
		}
		_ = idxCursor.Close()

		// Diff: missing entries (in model but not in store).
		for key, entryKey := range expected {
			if _, ok := actual[key]; !ok {
				violations = append(violations, Violation{
					Invariant: "multidimensional_entry_missing",
					Expected:  fmt.Sprintf("index %q entry %v", idx.Name, entryKey),
					Actual:    "not in store",
				})
			}
		}

		// Diff: orphan entries (in store but not in model).
		for key, actualEntry := range actual {
			if _, ok := expected[key]; !ok {
				violations = append(violations, Violation{
					Invariant: "multidimensional_entry_orphan",
					Expected:  fmt.Sprintf("index %q: no entry %v", idx.Name, actualEntry.Key),
					Actual:    "exists in store but not in model",
				})
			}
		}

		// Value verification: for plain DimensionsKeyExpression (no KeyWithValue),
		// the value should be an empty tuple.
		for key, entryKey := range expected {
			actualEntry, ok := actual[key]
			if !ok {
				continue // already reported as missing
			}
			expectedValue := tuple.Tuple{}
			actualValue := actualEntry.Value
			if string(actualValue.Pack()) != string(expectedValue.Pack()) {
				violations = append(violations, Violation{
					Invariant: "multidimensional_entry_value",
					Expected:  fmt.Sprintf("index %q entry %v: value %v", idx.Name, entryKey, expectedValue),
					Actual:    fmt.Sprintf("value %v", actualValue),
				})
			}
		}

		// Count cross-check.
		if len(expected) != len(actual) {
			violations = append(violations, Violation{
				Invariant: "multidimensional_entry_count",
				Expected:  fmt.Sprintf("index %q: %d entries", idx.Name, len(expected)),
				Actual:    fmt.Sprintf("%d entries", len(actual)),
			})
		}
	}

	return violations
}
