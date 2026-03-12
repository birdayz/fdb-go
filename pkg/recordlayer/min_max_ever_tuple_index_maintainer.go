package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// MinMaxEverTupleIndexMaintainer handles MIN_EVER_TUPLE and MAX_EVER_TUPLE index maintenance.
// Uses FDB atomic BYTE_MIN/BYTE_MAX mutations to track the min/max value ever seen per grouping key.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: tuple-packed bytes (byte comparison, works for any tuple-encodable type)
//
// Unlike the _LONG variants which use unsigned integer comparison (FDB MIN/MAX on little-endian int64),
// the _TUPLE variants compare tuple-packed byte representations (FDB BYTE_MIN/BYTE_MAX), supporting
// strings, nested tuples, and multi-column values.
//
// _EVER semantics: deleting a record does NOT revert the aggregate.
// Idempotent: applying the same mutation multiple times yields the same result.
// Non-idempotent for grouping count >= 1 (COUNT_NOT_NULL-like behavior).
type MinMaxEverTupleIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
	isMax         bool // true = MAX_EVER_TUPLE, false = MIN_EVER_TUPLE
}

func newMinMaxEverTupleIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext, isMax bool) *MinMaxEverTupleIndexMaintainer {
	return &MinMaxEverTupleIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
		isMax:         isMax,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts: atomically applies BYTE_MIN/BYTE_MAX of the tuple-packed value to each grouping key.
// For deletes: NO-OP (_EVER = irreversible).
// For updates: applies BYTE_MIN/BYTE_MAX of the new value.
// Null values are skipped. Any tuple-encodable value is accepted (no negative restriction).
func (m *MinMaxEverTupleIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if newRecord == nil {
		return nil
	}

	entries, err := m.evaluateEntries(newRecord)
	if err != nil {
		return fmt.Errorf("evaluate %s index %q: %w", m.index.Type, m.index.Name, err)
	}

	for _, e := range entries {
		fdbKey := m.indexSubspace.Pack(e.groupKey)

		if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, e.packedValue); err != nil {
			return err
		}

		if m.isMax {
			m.tx.ByteMax(fdb.Key(fdbKey), e.packedValue)
		} else {
			m.tx.ByteMin(fdb.Key(fdbKey), e.packedValue)
		}
	}

	return nil
}

// UpdateWhileWriteOnly passes through to Update(). TUPLE _EVER indexes are idempotent.
func (m *MinMaxEverTupleIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// DeleteWhere clears all MIN/MAX_EVER_TUPLE index entries whose key starts with the given prefix.
func (m *MinMaxEverTupleIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// Scan scans MIN/MAX_EVER_TUPLE index entries within the given tuple range.
// Returns IndexEntry where Key = grouping tuple and Value = tuple-decoded value.
// Uses a dedicated cursor that decodes tuple-packed values (unlike countKVCursor which decodes int64).
func (m *MinMaxEverTupleIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newTupleValueIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// tupleEntry holds a grouping key and the tuple-packed value bytes for a TUPLE index entry.
type tupleEntry struct {
	groupKey    tuple.Tuple
	packedValue []byte
}

// evaluateEntries extracts (groupingKey, tuple-packed value) pairs from a record.
func (m *MinMaxEverTupleIndexMaintainer) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]tupleEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []tupleEntry
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		if groupingCount >= len(values) {
			continue // No aggregated column
		}

		// Build a tuple from the grouped (value) columns and pack it.
		// This matches Java's default case in getMutationParam: entry.getKey().pack()
		// where entry.getKey() contains the grouped value columns.
		valueTuple := make(tuple.Tuple, len(values)-groupingCount)
		hasNull := false
		for j := groupingCount; j < len(values); j++ {
			if values[j] == nil {
				hasNull = true
				break
			}
			valueTuple[j-groupingCount] = values[j]
		}
		if hasNull {
			continue // Null values produce no mutation
		}

		result = append(result, tupleEntry{
			groupKey:    groupKey,
			packedValue: valueTuple.Pack(),
		})
	}
	return result, nil
}


var _ IndexMaintainer = (*MinMaxEverTupleIndexMaintainer)(nil)
