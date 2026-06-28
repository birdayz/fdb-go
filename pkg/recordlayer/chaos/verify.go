package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// Violation represents a single inconsistency between the model and the store.
type Violation struct {
	Invariant  string      // e.g., "record_count", "record_missing", "record_orphan"
	PrimaryKey tuple.Tuple // relevant PK (if applicable)
	Expected   any         // what the model says
	Actual     any         // what the store has
}

func (v Violation) String() string {
	if v.PrimaryKey != nil {
		return fmt.Sprintf("%s: pk=%v expected=%v actual=%v", v.Invariant, v.PrimaryKey, v.Expected, v.Actual)
	}
	return fmt.Sprintf("%s: expected=%v actual=%v", v.Invariant, v.Expected, v.Actual)
}

// Verify compares the store's actual state against the model's expected state.
// Returns all violations found. An empty slice means the store is consistent.
//
// Checks performed:
//  1. Record count (store.GetRecordCount() vs model.Count())
//  2. Record existence (every model record exists in store)
//  3. No orphans (every store record exists in model)
//  4. VALUE index entries
//  5. Atomic index values (COUNT, SUM, COUNT_UPDATES)
//  6. MIN/MAX_EVER index values
//  7. RANK index entries + ranked set consistency
//  8. PERMUTED_MIN/MAX index entries (primary + permuted subspace)
//  9. VERSION index entries (PK matching + versionstamp consistency)
//
// 10. MULTIDIMENSIONAL index entries (R-tree scan vs model, set-based)
// 11. VECTOR index entries (HNSW self-search + count + orphan check)
// 12. BITMAP_VALUE index entries (bitmap bits vs model records)
// 13. TEXT index entries (token→PK set vs model tokenization)
func Verify(store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation

	// 1. Record count (if counting is enabled in metadata)
	if model.metadata.GetRecordCountKey() != nil {
		actualCount, err := store.GetRecordCount()
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "record_count_error",
				Expected:  "no error",
				Actual:    err.Error(),
			})
		} else {
			expectedCount := model.Count()
			if actualCount != expectedCount {
				violations = append(violations, Violation{
					Invariant: "record_count",
					Expected:  expectedCount,
					Actual:    actualCount,
				})
			}
		}
	}

	// 2. Every model record must exist in the store
	for _, rec := range model.Records {
		loaded, err := store.LoadRecord(rec.PrimaryKey)
		if err != nil {
			violations = append(violations, Violation{
				Invariant:  "record_load_error",
				PrimaryKey: rec.PrimaryKey,
				Expected:   "loadable",
				Actual:     err.Error(),
			})
		} else if loaded == nil {
			violations = append(violations, Violation{
				Invariant:  "record_missing",
				PrimaryKey: rec.PrimaryKey,
				Expected:   "exists",
				Actual:     "nil",
			})
		}
	}

	// 3. No orphan records (every store record must exist in model)
	ctx := context.Background()
	cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
	defer func() { _ = cursor.Close() }()

	storeCount := 0
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "scan_error",
				Expected:  "no error",
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		rec := result.GetValue()
		storeCount++
		if !model.Has(rec.PrimaryKey) {
			violations = append(violations, Violation{
				Invariant:  "record_orphan",
				PrimaryKey: rec.PrimaryKey,
				Expected:   "not in store",
				Actual:     "exists in store but not in model",
			})
		}
	}

	// Cross-check: scan count should match model count
	// (catches scan bugs independent of the COUNT index)
	if int64(storeCount) != model.Count() {
		violations = append(violations, Violation{
			Invariant: "scan_count_mismatch",
			Expected:  model.Count(),
			Actual:    int64(storeCount),
		})
	}

	// 4. VALUE index entry verification
	violations = append(violations, verifyValueIndexes(ctx, store, model)...)

	// 5. Atomic index verification (COUNT, SUM, COUNT_UPDATES, MIN/MAX_EVER)
	violations = append(violations, verifyAtomicIndexes(ctx, store, model)...)

	// 6. MIN/MAX_EVER index verification
	violations = append(violations, verifyMinMaxEverIndexes(ctx, store, model)...)

	// 7. RANK index verification (B-tree entries + ranked set consistency)
	violations = append(violations, verifyRankIndexes(ctx, store, model)...)

	// 8. PERMUTED_MIN/PERMUTED_MAX index verification (primary + permuted entries)
	violations = append(violations, verifyPermutedIndexes(ctx, store, model)...)

	// 9. VERSION index verification (entry existence + versionstamp consistency)
	violations = append(violations, verifyVersionIndexes(ctx, store, model)...)

	// 10. MULTIDIMENSIONAL index verification (R-tree entries vs model, set-based)
	violations = append(violations, verifyMultidimensionalIndexes(ctx, store, model)...)

	// 11. VECTOR index verification (HNSW self-search + count)
	violations = append(violations, verifyVectorIndexes(ctx, store, model)...)

	// 11b. SPFresh (RFC-094) vector index verification (self-search completeness
	// + orphan + membership⊆postings structural integrity). Strict integrity:
	// the scenario drains the maintenance queue before Verify.
	violations = append(violations, verifyVectorSPFreshIndexes(store, model)...)

	// 12. BITMAP_VALUE index verification (bitmap bits vs model records)
	violations = append(violations, verifyBitmapValueIndexes(ctx, store, model)...)

	// 13. TEXT index verification (token→PK entries vs model)
	violations = append(violations, verifyTextIndexes(ctx, store, model)...)

	return violations
}

// verifyValueIndexes checks that every VALUE index contains exactly the entries
// predicted by the model. For each VALUE index:
//   - Compute expected entries from model records (evaluate index expression + trim PK)
//   - Scan actual entries from the store
//   - Missing or extra entries = violation
//   - For covering indexes (KeyWithValueExpression), also verifies value portions
func verifyValueIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeValue {
			continue
		}

		// Detect covering index (KeyWithValueExpression).
		kwv, isCovering := idx.RootExpression.(*recordlayer.KeyWithValueExpression)

		// Build expected entries from model records.
		type expectedEntry struct {
			key   tuple.Tuple // [keyColumns..., trimmedPK...]
			value tuple.Tuple // non-nil for covering indexes
		}
		expected := make(map[string]*expectedEntry)

		for _, rec := range model.Records {
			// Check if this index applies to this record type.
			if !model.indexAppliesToType(idx, rec.TypeName) {
				continue
			}

			// Evaluate the index expression against this record.
			storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
				PrimaryKey: rec.PrimaryKey,
				RecordType: md.GetRecordType(rec.TypeName),
				Record:     rec.Message,
			}

			tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "index_eval_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q evaluable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			trimmedPK, err := idx.TrimPrimaryKey(rec.PrimaryKey)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "index_trim_pk_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q pk trimmable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			for _, values := range tuples {
				var keyColumns []any
				var valueTuple tuple.Tuple

				if isCovering {
					// Split: key columns [0, splitPoint), value columns [splitPoint, end).
					keyPart, valuePart := kwv.SplitEvaluatedKey(values)
					keyColumns = keyPart
					valueTuple = make(tuple.Tuple, len(valuePart))
					for j, v := range valuePart {
						valueTuple[j] = v
					}
				} else {
					keyColumns = values
				}

				entryKey := make(tuple.Tuple, 0, len(keyColumns)+len(trimmedPK))
				for _, v := range keyColumns {
					entryKey = append(entryKey, v)
				}
				entryKey = append(entryKey, trimmedPK...)
				expected[string(entryKey.Pack())] = &expectedEntry{key: entryKey, value: valueTuple}
			}
		}

		// Scan actual index entries.
		type actualEntry struct {
			key   tuple.Tuple
			value tuple.Tuple
		}
		actual := make(map[string]*actualEntry)
		idxCursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

		for {
			result, err := idxCursor.OnNext(ctx)
			if err != nil {
				violations = append(violations, Violation{
					Invariant: "index_scan_error",
					Expected:  fmt.Sprintf("index %q scannable", idx.Name),
					Actual:    err.Error(),
				})
				break
			}
			if !result.HasNext() {
				break
			}
			entry := result.GetValue()
			actual[string(entry.Key.Pack())] = &actualEntry{key: entry.Key, value: entry.Value}
		}
		_ = idxCursor.Close()

		// Diff: missing entries (in model but not in store)
		for key, exp := range expected {
			if _, ok := actual[key]; !ok {
				violations = append(violations, Violation{
					Invariant: "index_entry_missing",
					Expected:  fmt.Sprintf("index %q entry %v", idx.Name, exp.key),
					Actual:    "not in store",
				})
			}
		}

		// Diff: orphan entries (in store but not in model)
		for key, act := range actual {
			if _, ok := expected[key]; !ok {
				violations = append(violations, Violation{
					Invariant: "index_entry_orphan",
					Expected:  fmt.Sprintf("index %q: no entry %v", idx.Name, act.key),
					Actual:    "exists in store but not in model",
				})
			}
		}

		// For covering indexes, verify value portions match.
		if isCovering {
			for key, exp := range expected {
				act, ok := actual[key]
				if !ok {
					continue // Already flagged as missing above.
				}
				if string(exp.value.Pack()) != string(act.value.Pack()) {
					violations = append(violations, Violation{
						Invariant: "index_entry_value_mismatch",
						Expected:  fmt.Sprintf("index %q entry %v value %v", idx.Name, exp.key, exp.value),
						Actual:    fmt.Sprintf("value %v", act.value),
					})
				}
			}
		}

		// Count cross-check
		if len(expected) != len(actual) {
			violations = append(violations, Violation{
				Invariant: "index_entry_count",
				Expected:  fmt.Sprintf("index %q: %d entries", idx.Name, len(expected)),
				Actual:    fmt.Sprintf("%d entries", len(actual)),
			})
		}
	}

	return violations
}
