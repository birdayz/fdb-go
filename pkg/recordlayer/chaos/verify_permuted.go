package chaos

import (
	"context"
	"fmt"
	"strconv"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyPermutedIndexes checks PERMUTED_MIN and PERMUTED_MAX indexes:
//  1. Primary entries (IndexKey=2) must match model — same logic as VALUE indexes.
//  2. Permuted entries (IndexSecondarySpaceKey=3) must contain exactly one entry
//     per distinct grouping key, holding the current extremum (min or max value).
func verifyPermutedIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		switch idx.Type {
		case recordlayer.IndexTypePermutedMin, recordlayer.IndexTypePermutedMax:
		default:
			continue
		}

		isMax := idx.Type == recordlayer.IndexTypePermutedMax

		// --- Part 1: Primary entry verification (identical to VALUE) ---
		violations = append(violations, verifyPermutedPrimary(ctx, store, model, idx)...)

		// --- Part 2: Permuted (secondary) entry verification ---
		violations = append(violations, verifyPermutedSecondary(ctx, store, model, idx, isMax)...)
	}

	return violations
}

// verifyPermutedPrimary verifies the primary subspace entries of a PERMUTED index.
// These are standard VALUE-style entries: [indexedValues..., trimmedPK...].
func verifyPermutedPrimary(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, idx *recordlayer.Index) []Violation {
	var violations []Violation

	expected := make(map[string]tuple.Tuple)

	for _, rec := range model.Records {
		if !model.indexAppliesToType(idx, rec.TypeName) {
			continue
		}

		storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
			PrimaryKey: rec.PrimaryKey,
			RecordType: model.metadata.GetRecordType(rec.TypeName),
			Record:     rec.Message,
		}

		tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
		if err != nil {
			violations = append(violations, Violation{
				Invariant:  "permuted_primary_eval_error",
				PrimaryKey: rec.PrimaryKey,
				Expected:   fmt.Sprintf("index %q evaluable", idx.Name),
				Actual:     err.Error(),
			})
			continue
		}

		trimmedPK, err := idx.TrimPrimaryKey(rec.PrimaryKey)
		if err != nil {
			violations = append(violations, Violation{
				Invariant:  "permuted_primary_trim_pk_error",
				PrimaryKey: rec.PrimaryKey,
				Expected:   fmt.Sprintf("index %q pk trimmable", idx.Name),
				Actual:     err.Error(),
			})
			continue
		}

		for _, values := range tuples {
			entryKey := make(tuple.Tuple, 0, len(values)+len(trimmedPK))
			for _, v := range values {
				entryKey = append(entryKey, v)
			}
			entryKey = append(entryKey, trimmedPK...)
			expected[string(entryKey.Pack())] = entryKey
		}
	}

	// Scan actual primary entries.
	actual := make(map[string]tuple.Tuple)
	idxCursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

	for {
		result, err := idxCursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "permuted_primary_scan_error",
				Expected:  fmt.Sprintf("index %q primary scannable", idx.Name),
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		actual[string(entry.Key.Pack())] = entry.Key
	}
	_ = idxCursor.Close()

	for key, entryKey := range expected {
		if _, ok := actual[key]; !ok {
			violations = append(violations, Violation{
				Invariant: "permuted_primary_entry_missing",
				Expected:  fmt.Sprintf("index %q primary entry %v", idx.Name, entryKey),
				Actual:    "not in store",
			})
		}
	}

	for key, entryKey := range actual {
		if _, ok := expected[key]; !ok {
			violations = append(violations, Violation{
				Invariant: "permuted_primary_entry_orphan",
				Expected:  fmt.Sprintf("index %q: no primary entry %v", idx.Name, entryKey),
				Actual:    "exists in store but not in model",
			})
		}
	}

	if len(expected) != len(actual) {
		violations = append(violations, Violation{
			Invariant: "permuted_primary_entry_count",
			Expected:  fmt.Sprintf("index %q: %d primary entries", idx.Name, len(expected)),
			Actual:    fmt.Sprintf("%d entries", len(actual)),
		})
	}

	return violations
}

// verifyPermutedSecondary verifies the permuted (secondary) subspace entries.
// There should be exactly one entry per distinct grouping key, containing the
// current extremum (min or max value) for that group.
func verifyPermutedSecondary(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, idx *recordlayer.Index, isMax bool) []Violation {
	var violations []Violation

	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		violations = append(violations, Violation{
			Invariant: "permuted_secondary_expr_type",
			Expected:  fmt.Sprintf("index %q has GroupingKeyExpression", idx.Name),
			Actual:    fmt.Sprintf("%T", idx.RootExpression),
		})
		return violations
	}

	groupingCount := gke.GetGroupingCount()
	totalSize := groupingCount + gke.GetGroupedCount()

	permutedSize := 0
	if v, ok := idx.Options[recordlayer.IndexOptionPermutedSize]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			permutedSize = n
		}
	}
	permutePosition := groupingCount - permutedSize

	// Compute expected permuted entries from model records.
	// Group records by grouping key, find the extremum value per group.
	type groupEntry struct {
		groupPrefix tuple.Tuple // columns [0, permutePosition)
		groupSuffix tuple.Tuple // columns [permutePosition, groupingCount)
		value       tuple.Tuple // columns [groupingCount, totalSize)
	}

	// key: packed grouping key -> extremum entry
	extrema := make(map[string]*groupEntry)

	for _, rec := range model.Records {
		if !model.indexAppliesToType(idx, rec.TypeName) {
			continue
		}

		storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
			PrimaryKey: rec.PrimaryKey,
			RecordType: model.metadata.GetRecordType(rec.TypeName),
			Record:     rec.Message,
		}

		tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
		if err != nil {
			continue
		}

		for _, values := range tuples {
			if len(values) < totalSize {
				continue
			}

			groupKey := make(tuple.Tuple, groupingCount)
			for i := 0; i < groupingCount; i++ {
				groupKey[i] = values[i]
			}

			value := make(tuple.Tuple, totalSize-groupingCount)
			for i := groupingCount; i < totalSize; i++ {
				value[i-groupingCount] = values[i]
			}

			gkPacked := string(groupKey.Pack())
			existing, exists := extrema[gkPacked]
			if !exists {
				gp := make(tuple.Tuple, permutePosition)
				for i := 0; i < permutePosition; i++ {
					gp[i] = groupKey[i]
				}
				gs := make(tuple.Tuple, groupingCount-permutePosition)
				for i := permutePosition; i < groupingCount; i++ {
					gs[i-permutePosition] = groupKey[i]
				}
				extrema[gkPacked] = &groupEntry{
					groupPrefix: gp,
					groupSuffix: gs,
					value:       value,
				}
			} else {
				if shouldUpdate(existing.value, value, isMax) {
					existing.value = value
				}
			}
		}
	}

	// Build expected permuted keys: [groupPrefix..., value..., groupSuffix...]
	expected := make(map[string]tuple.Tuple)
	for _, ge := range extrema {
		permutedKey := make(tuple.Tuple, 0, len(ge.groupPrefix)+len(ge.value)+len(ge.groupSuffix))
		permutedKey = append(permutedKey, ge.groupPrefix...)
		permutedKey = append(permutedKey, ge.value...)
		permutedKey = append(permutedKey, ge.groupSuffix...)
		expected[string(permutedKey.Pack())] = permutedKey
	}

	// Scan actual permuted entries via ScanIndexByType with IndexScanByGroup.
	actual := make(map[string]tuple.Tuple)
	permCursor := store.ScanIndexByType(idx, recordlayer.IndexScanByGroup,
		recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

	for {
		result, err := permCursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "permuted_secondary_scan_error",
				Expected:  fmt.Sprintf("index %q permuted scannable", idx.Name),
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		actual[string(entry.Key.Pack())] = entry.Key
	}
	_ = permCursor.Close()

	// Diff: missing entries
	for key, entryKey := range expected {
		if _, ok := actual[key]; !ok {
			violations = append(violations, Violation{
				Invariant: "permuted_secondary_entry_missing",
				Expected:  fmt.Sprintf("index %q permuted entry %v", idx.Name, entryKey),
				Actual:    "not in store",
			})
		}
	}

	// Diff: orphan entries
	for key, entryKey := range actual {
		if _, ok := expected[key]; !ok {
			violations = append(violations, Violation{
				Invariant: "permuted_secondary_entry_orphan",
				Expected:  fmt.Sprintf("index %q: no permuted entry %v", idx.Name, entryKey),
				Actual:    "exists in store but not in model",
			})
		}
	}

	if len(expected) != len(actual) {
		violations = append(violations, Violation{
			Invariant: "permuted_secondary_entry_count",
			Expected:  fmt.Sprintf("index %q: %d permuted entries", idx.Name, len(expected)),
			Actual:    fmt.Sprintf("%d entries", len(actual)),
		})
	}

	return violations
}

// shouldUpdate returns true if newValue should replace oldValue.
// For MAX: newValue > oldValue. For MIN: newValue < oldValue.
// Uses tuple byte comparison (same as FDB ordering).
func shouldUpdate(oldValue, newValue tuple.Tuple, isMax bool) bool {
	oldPacked := oldValue.Pack()
	newPacked := newValue.Pack()
	for i := 0; i < len(oldPacked) && i < len(newPacked); i++ {
		if oldPacked[i] < newPacked[i] {
			return isMax // new > old: update for MAX
		}
		if oldPacked[i] > newPacked[i] {
			return !isMax // new < old: update for MIN
		}
	}
	if len(oldPacked) < len(newPacked) {
		return isMax
	}
	if len(oldPacked) > len(newPacked) {
		return !isMax
	}
	return false // equal
}
