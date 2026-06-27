package chaos

import (
	"context"
	"fmt"
	"strconv"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyBitmapValueIndexes checks that every BITMAP_VALUE index contains exactly
// the bitmap entries predicted by the model records.
//
// For each BITMAP_VALUE index:
//   - Compute expected bitmap entries from model records (evaluate expression,
//     extract grouping columns and position, compute aligned position + bit offset)
//   - Scan actual bitmap entries from the store via ScanIndexByType(BY_GROUP)
//   - Compare: missing entries, orphan entries, bit mismatches
func verifyBitmapValueIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeBitmapValue {
			continue
		}

		violations = append(violations, verifyOneBitmapIndex(ctx, store, model, md, idx)...)
	}

	return violations
}

// bitmapModelEntry represents the expected state of a single bitmap FDB entry.
type bitmapModelEntry struct {
	groupKey   tuple.Tuple    // leading grouping columns
	alignedPos int64          // aligned position (multiple of entrySize)
	bits       map[int64]bool // bit offsets that should be set within this entry
}

func verifyOneBitmapIndex(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	model *StoreModel,
	md *recordlayer.RecordMetaData,
	idx *recordlayer.Index,
) []Violation {
	var violations []Violation

	entrySize := int64(10000) // default
	if v, ok := idx.Options[recordlayer.IndexOptionBitmapValueEntrySize]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			entrySize = n
		}
	}

	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}
	groupingCount := gke.GetGroupingCount()

	// Build expected bitmap entries from model records.
	// Key: packed (groupKey + alignedPos) → set of bit offsets
	type entryKey struct {
		packed string
	}
	expectedBits := make(map[string]map[int64]bool) // packed key → bit offsets
	expectedKeys := make(map[string]tuple.Tuple)    // packed key → full tuple key

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
				Invariant:  "bitmap_eval_error",
				PrimaryKey: rec.PrimaryKey,
				Expected:   fmt.Sprintf("index %q evaluable", idx.Name),
				Actual:     err.Error(),
			})
			continue
		}

		for _, values := range tuples {
			if groupingCount >= len(values) {
				continue // no position column
			}

			positionRaw := values[groupingCount]
			if positionRaw == nil {
				continue
			}

			position, err := anyToInt64(positionRaw)
			if err != nil {
				continue
			}

			offset := bitmapFloorMod(position, entrySize)
			alignedPos := position - offset

			// Build key tuple: [groupKey..., alignedPos]
			fdbKey := make(tuple.Tuple, 0, groupingCount+1)
			for i := 0; i < groupingCount; i++ {
				fdbKey = append(fdbKey, values[i])
			}
			fdbKey = append(fdbKey, alignedPos)
			packed := string(fdbKey.Pack())

			if expectedBits[packed] == nil {
				expectedBits[packed] = make(map[int64]bool)
				expectedKeys[packed] = fdbKey
			}
			expectedBits[packed][offset] = true
		}
	}

	// Scan actual bitmap entries from the store.
	actualBitmaps := make(map[string][]byte) // packed key → raw bitmap bytes
	actualKeys := make(map[string]tuple.Tuple)

	cursor := store.ScanIndexByType(idx, recordlayer.IndexScanByGroup,
		recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "bitmap_scan_error",
				Expected:  fmt.Sprintf("index %q scannable", idx.Name),
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		packed := string(entry.Key.Pack())
		actualKeys[packed] = entry.Key

		if len(entry.Value) > 0 {
			if bitmapBytes, ok := entry.Value[0].([]byte); ok {
				actualBitmaps[packed] = bitmapBytes
			}
		}
	}
	_ = cursor.Close()

	// Compare: check expected entries exist with correct bits.
	for packed, bits := range expectedBits {
		actualBytes, exists := actualBitmaps[packed]
		if !exists {
			violations = append(violations, Violation{
				Invariant: "bitmap_entry_missing",
				Expected:  fmt.Sprintf("index %q entry %v with %d bits", idx.Name, expectedKeys[packed], len(bits)),
				Actual:    "not in store",
			})
			continue
		}

		// Check each expected bit is set.
		for bitOffset := range bits {
			byteIdx := bitOffset / 8
			bitIdx := bitOffset % 8
			if int(byteIdx) >= len(actualBytes) || (actualBytes[byteIdx]&(1<<bitIdx)) == 0 {
				violations = append(violations, Violation{
					Invariant: "bitmap_bit_missing",
					Expected:  fmt.Sprintf("index %q entry %v bit %d set", idx.Name, expectedKeys[packed], bitOffset),
					Actual:    "not set in store",
				})
			}
		}

		// Check no unexpected bits are set.
		for byteIdx, b := range actualBytes {
			for bitIdx := 0; bitIdx < 8; bitIdx++ {
				if (b & (1 << bitIdx)) != 0 {
					offset := int64(byteIdx)*8 + int64(bitIdx)
					if !bits[offset] {
						violations = append(violations, Violation{
							Invariant: "bitmap_bit_orphan",
							Expected:  fmt.Sprintf("index %q entry %v bit %d not set", idx.Name, expectedKeys[packed], offset),
							Actual:    "set in store but not in model",
						})
					}
				}
			}
		}
	}

	// Check for orphan entries (in store but not expected).
	for packed := range actualBitmaps {
		if _, exists := expectedBits[packed]; !exists {
			violations = append(violations, Violation{
				Invariant: "bitmap_entry_orphan",
				Expected:  fmt.Sprintf("index %q: no entry %v", idx.Name, actualKeys[packed]),
				Actual:    "exists in store but not in model",
			})
		}
	}

	return violations
}

// bitmapFloorMod computes the floor modulus matching Java's Math.floorMod.
// Go's % operator truncates toward zero; this always returns a non-negative result.
func bitmapFloorMod(x, y int64) int64 {
	return ((x % y) + y) % y
}

// anyToInt64 converts a value to int64 for bitmap position computation.
func anyToInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}
