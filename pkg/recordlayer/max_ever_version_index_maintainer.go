package recordlayer

import (
	"bytes"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// maxEverVersionIndexMaintainer handles MAX_EVER_VERSION index maintenance.
// Like MAX_EVER_TUPLE but version-aware: incomplete versionstamps use
// SET_VERSIONSTAMPED_VALUE mutations (via context), complete versionstamps
// use FDB BYTE_MAX atomic mutations.
//
// The index expression must have exactly 1 VersionKeyExpression in the grouped
// (aggregated) portion and none in the grouping portion.
//
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: tuple-packed grouped columns (including versionstamp)
//
// _EVER semantics: deleting a record does NOT revert the aggregate.
// Idempotent: applying the same mutation multiple times yields the same result.
//
// When multiple records with incomplete versionstamps (same transaction) share
// the same grouping key, the one with the maximum byte representation is kept.
// This ensures the maximum local version wins.
//
// Matches Java's AtomicMutationIndexMaintainer with AtomicMutation.MAX_EVER_VERSION.
type maxEverVersionIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.WritableTransaction
	recordContext *FDBRecordContext
	store         indexStoreContext
}

func newMaxEverVersionIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.WritableTransaction, recordContext *FDBRecordContext, store indexStoreContext) *maxEverVersionIndexMaintainer {
	return &maxEverVersionIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		recordContext: recordContext,
		store:         store,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts/updates: applies BYTE_MAX (complete) or SET_VERSIONSTAMPED_VALUE (incomplete).
// For deletes: NO-OP (_EVER = irreversible).
func (m *maxEverVersionIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if newRecord == nil {
		return nil // _EVER: deletes are no-ops
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

		if e.hasIncompleteVersionstamp {
			// Incomplete versionstamp: queue SET_VERSIONSTAMPED_VALUE with merge function.
			// The merge keeps the maximum by unsigned byte comparison, ensuring the
			// record with the highest local version wins within the same transaction.
			// Matches Java's updateVersionMutation with ByteArrayUtil.compareUnsigned remapper.
			m.recordContext.UpdateVersionMutation(
				MutationTypeSetVersionstampedValue,
				fdbKey,
				e.packedValue,
				func(oldValue, newValue []byte) []byte {
					if bytes.Compare(oldValue, newValue) < 0 {
						return newValue
					}
					return oldValue
				},
			)
		} else {
			// Complete versionstamp: standard BYTE_MAX atomic mutation.
			m.tx.ByteMax(fdb.Key(fdbKey), e.packedValue)
		}
	}

	return nil
}

// UpdateWhileWriteOnly passes through to Update(). MAX_EVER_VERSION is idempotent.
func (m *maxEverVersionIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// DeleteWhere clears all MAX_EVER_VERSION index entries whose key starts with the given prefix.
func (m *maxEverVersionIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// Scan scans MAX_EVER_VERSION index entries within the given tuple range.
// Returns IndexEntry where Key = grouping tuple and Value = tuple-decoded grouped columns.
// Uses the tuple value cursor (same as MAX_EVER_TUPLE).
func (m *maxEverVersionIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newTupleValueIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// versionEntry holds a grouping key and the packed value bytes for a MAX_EVER_VERSION entry.
type versionEntry struct {
	groupKey                  tuple.Tuple
	packedValue               []byte
	hasIncompleteVersionstamp bool
}

// evaluateEntries extracts (groupingKey, packed value) pairs from a record.
// The grouped columns (including versionstamp) are tuple-packed as the value.
// For incomplete versionstamps, PackWithVersionstamp is used to include the
// versionstamp offset bytes required by SET_VERSIONSTAMPED_VALUE.
func (m *maxEverVersionIndexMaintainer) evaluateEntries(record *FDBStoredRecord[proto.Message]) ([]versionEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := indexGroupingCount(m.index.RootExpression)
	var result []versionEntry
	for _, values := range tuples {
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}

		if groupingCount >= len(values) {
			continue // No aggregated column
		}

		// Build the value tuple from grouped (aggregated) columns.
		valueTuple := make(tuple.Tuple, len(values)-groupingCount)
		hasNull := false
		hasIncomplete := false
		for j := groupingCount; j < len(values); j++ {
			if values[j] == nil {
				hasNull = true
				break
			}
			valueTuple[j-groupingCount] = values[j]
			if vs, ok := values[j].(tuple.Versionstamp); ok {
				allFF := true
				for _, b := range vs.TransactionVersion {
					if b != 0xFF {
						allFF = false
						break
					}
				}
				if allFF {
					hasIncomplete = true
				}
			}
		}
		if hasNull {
			continue // Null values produce no mutation
		}

		var packed []byte
		if hasIncomplete {
			var err error
			packed, err = valueTuple.PackWithVersionstamp(nil)
			if err != nil {
				return nil, fmt.Errorf("packWithVersionstamp for MAX_EVER_VERSION value: %w", err)
			}
		} else {
			packed = valueTuple.Pack()
		}

		result = append(result, versionEntry{
			groupKey:                  groupKey,
			packedValue:               packed,
			hasIncompleteVersionstamp: hasIncomplete,
		})
	}
	return result, nil
}

func (m *maxEverVersionIndexMaintainer) aggregateIdentity() tuple.Tuple { return nil }
func (m *maxEverVersionIndexMaintainer) aggregate(accum, entry tuple.Tuple) tuple.Tuple {
	return maxAggregate(accum, entry)
}

var (
	_ IndexMaintainer = (*maxEverVersionIndexMaintainer)(nil)
	_ indexAggregator = (*maxEverVersionIndexMaintainer)(nil)
)
