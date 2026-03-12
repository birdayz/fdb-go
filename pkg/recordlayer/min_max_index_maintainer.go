package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// MinMaxEverIndexMaintainer handles MIN_EVER_LONG and MAX_EVER_LONG index maintenance.
// Uses FDB atomic MIN/MAX mutations to track the minimum/maximum value ever seen per grouping key.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: little-endian int64 (unsigned comparison)
//
// _EVER semantics: deleting a record does NOT revert the aggregate. The stored value
// only ratchets in one direction (min goes lower, max goes higher). This matches
// Java's AtomicMutationIndexMaintainer with MIN_EVER_LONG/MAX_EVER_LONG mutation types.
//
// Idempotent: applying the same mutation multiple times yields the same result.
// Values must be non-negative (FDB MIN/MAX compare unsigned little-endian).
type MinMaxEverIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
	isMax         bool // true = MAX_EVER_LONG, false = MIN_EVER_LONG
}

func newMinMaxEverIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext, isMax bool) *MinMaxEverIndexMaintainer {
	return &MinMaxEverIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
		isMax:         isMax,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts: atomically applies MIN/MAX of the value to each grouping key entry.
// For deletes: NO-OP (_EVER = irreversible, the aggregate never decreases/increases back).
// For updates: applies MIN/MAX of the new value (old value's delete is a no-op).
// Null values are skipped. Negative values are rejected.
// Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys() for MIN_EVER_LONG/MAX_EVER_LONG.
func (m *MinMaxEverIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	// Deletes are no-ops for _EVER indexes — the aggregate is irreversible.
	// Java returns null from getMutationParam() when remove=true.
	if newRecord == nil {
		return nil
	}

	entries, err := m.evaluateEntries(newRecord)
	if err != nil {
		return fmt.Errorf("evaluate %s index %q: %w", m.index.Type, m.index.Name, err)
	}

	for _, e := range entries {
		fdbKey := m.indexSubspace.Pack(e.groupKey)
		param := encodeRecordCount(e.value) // little-endian int64

		if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, param); err != nil {
			return err
		}

		if m.isMax {
			m.tx.Max(fdb.Key(fdbKey), param)
		} else {
			m.tx.Min(fdb.Key(fdbKey), param)
		}
	}

	return nil
}

// UpdateWhileWriteOnly passes through to Update() for MIN/MAX _EVER indexes.
// These are idempotent — applying the same mutation multiple times is safe.
// No range set check needed. Matches Java's AtomicMutation.isIdempotent() = true.
func (m *MinMaxEverIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan scans MIN/MAX_EVER index entries within the given tuple range.
// Returns IndexEntry where Key = grouping tuple and Value = min/max as tuple.
// DeleteWhere clears all MIN/MAX_EVER_LONG index entries whose key starts with the given prefix.
func (m *MinMaxEverIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// Reuses countKVCursor — identical wire format (little-endian int64 values).
func (m *MinMaxEverIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// minMaxEntry holds a grouping key and the corresponding value for an index entry.
type minMaxEntry struct {
	groupKey tuple.Tuple
	value    int64
}

// evaluateEntries extracts (groupingKey, value) pairs from a record.
// Validates that values are non-negative (FDB MIN/MAX compare unsigned).
func (m *MinMaxEverIndexMaintainer) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]minMaxEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []minMaxEntry
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		if groupingCount >= len(values) {
			continue // No aggregated column
		}
		rawValue := values[groupingCount]
		if rawValue == nil {
			continue // Null values produce no mutation
		}

		val, err := toInt64(rawValue)
		if err != nil {
			return nil, fmt.Errorf("%s index %q: value at column %d: %w", m.index.Type, m.index.Name, groupingCount, err)
		}

		// Reject negative values — FDB MIN/MAX compare unsigned little-endian.
		// Matches Java's MIN_EVER_LONG/MAX_EVER_LONG validation.
		if val < 0 {
			return nil, fmt.Errorf("%s index %q: negative value %d not allowed for LONG variant", m.index.Type, m.index.Name, val)
		}

		result = append(result, minMaxEntry{groupKey: groupKey, value: val})
	}
	return result, nil
}


var _ IndexMaintainer = (*MinMaxEverIndexMaintainer)(nil)
