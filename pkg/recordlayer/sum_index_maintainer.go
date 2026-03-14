package recordlayer

import (
	"fmt"
	"math"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// SumIndexMaintainer handles SUM index maintenance using FDB atomic ADD.
// The index stores the running sum of a field's value per grouping key.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: little-endian int64 sum
// Matches Java's AtomicMutationIndexMaintainer with SUM_LONG mutation.
type SumIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newSumIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *SumIndexMaintainer {
	return &SumIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
	}
}

// sumEntry holds a grouping key and the corresponding sum value for an index entry.
type sumEntry struct {
	groupKey tuple.Tuple
	sumValue int64
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts: atomically adds +value to each grouping key entry.
// For deletes: atomically adds -value to each grouping key entry.
// For updates: subtracts old values and adds new values.
// Null values are skipped (no mutation), matching Java's behavior.
// Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys() for SUM_LONG.
func (m *SumIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var oldEntries, newEntries []sumEntry

	if oldRecord != nil {
		entries, err := m.evaluateSumEntries(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate sum index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}

	if newRecord != nil {
		entries, err := m.evaluateSumEntries(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate sum index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	// Skip entries with identical grouping key AND sum value — no net change.
	// Matches Java's skipUpdateForUnchangedKeys() = true for SUM.
	if oldEntries != nil && newEntries != nil {
		oldEntries, newEntries = removeCommonSumEntries(oldEntries, newEntries)
	}

	clearWhenZero := m.index.IsClearWhenZero()

	for _, e := range oldEntries {
		if e.sumValue == math.MinInt64 {
			return fmt.Errorf("sum index %q overflow: cannot negate math.MinInt64", m.index.Name)
		}
		fdbKey := m.indexSubspace.Pack(e.groupKey)
		m.tx.Add(fdb.Key(fdbKey), encodeRecordCount(-e.sumValue))
		if clearWhenZero {
			m.tx.CompareAndClear(fdb.Key(fdbKey), littleEndianInt64Zero)
		}
	}

	for _, e := range newEntries {
		fdbKey := m.indexSubspace.Pack(e.groupKey)
		if newRecord != nil {
			if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, encodeRecordCount(e.sumValue)); err != nil {
				return err
			}
		}
		m.tx.Add(fdb.Key(fdbKey), encodeRecordCount(e.sumValue))
	}

	return nil
}

// removeCommonSumEntries removes entries with identical grouping key and sum value.
// This avoids no-op mutations when an update doesn't change the summed value.
func removeCommonSumEntries(old, new []sumEntry) ([]sumEntry, []sumEntry) {
	type entryKey struct {
		groupKey string
		sumValue int64
	}

	newSet := make(map[entryKey]int, len(new))
	for _, e := range new {
		k := entryKey{groupKey: string(e.groupKey.Pack()), sumValue: e.sumValue}
		newSet[k]++
	}

	common := make(map[entryKey]int)
	var filteredOld []sumEntry
	for _, e := range old {
		k := entryKey{groupKey: string(e.groupKey.Pack()), sumValue: e.sumValue}
		if newSet[k] > common[k] {
			common[k]++
		} else {
			filteredOld = append(filteredOld, e)
		}
	}

	var filteredNew []sumEntry
	for _, e := range new {
		k := entryKey{groupKey: string(e.groupKey.Pack()), sumValue: e.sumValue}
		if common[k] > 0 {
			common[k]--
		} else {
			filteredNew = append(filteredNew, e)
		}
	}

	return filteredOld, filteredNew
}

// UpdateWhileWriteOnly checks the index build range set before updating.
// SUM is non-idempotent — blindly updating would double-count values.
// Matches Java's StandardIndexMaintainer.updateWriteOnlyByRecords().
func (m *SumIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return updateWhileWriteOnlyNonIdempotent(oldRecord, newRecord, m.index, m.store, "SUM", m.Update)
}

// Scan scans SUM index entries within the given tuple range.
// Returns IndexEntry where Key = grouping tuple and Value = sum as tuple.
// DeleteWhere clears all SUM index entries whose key starts with the given prefix.
func (m *SumIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// Matches Java's AtomicMutationIndexMaintainer.scan() with BY_GROUP semantics.
func (m *SumIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	// Reuse countKVCursor — identical wire format (little-endian int64 values).
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// evaluateSumEntries extracts (groupingKey, sumValue) pairs from a record.
// The grouping key is the leading columns, the sum value is the first grouped column.
// Null sum values are skipped (matching Java's getMutationParam returning null for null).
func (m *SumIndexMaintainer) evaluateSumEntries(record *FDBStoredRecord[proto.Message]) ([]sumEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []sumEntry
	for _, values := range tuples {
		// Extract grouping key (leading columns)
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		// Extract sum value — first grouped (trailing) column
		if groupingCount >= len(values) {
			continue // No aggregated column — skip
		}
		rawValue := values[groupingCount]
		if rawValue == nil {
			continue // Null values produce no mutation (Java returns null from getMutationParam)
		}

		sumValue, err := toInt64(rawValue)
		if err != nil {
			return nil, fmt.Errorf("sum index %q: value at column %d: %w", m.index.Name, groupingCount, err)
		}

		result = append(result, sumEntry{groupKey: groupKey, sumValue: sumValue})
	}
	return result, nil
}


// toInt64 converts a numeric value (from proto field evaluation) to int64.
// Handles int64, int32, float64, float32 matching Java's Number.longValue().
func toInt64(v any) (int64, error) {
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
		return 0, fmt.Errorf("cannot convert %T to int64 for SUM index", v)
	}
}

var _ IndexMaintainer = (*SumIndexMaintainer)(nil)
