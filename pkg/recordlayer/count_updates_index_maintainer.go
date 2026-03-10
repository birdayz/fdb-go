package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// CountUpdatesIndexMaintainer handles COUNT_UPDATES index maintenance using FDB atomic ADD.
// Like COUNT, but with two key differences:
//  1. Deletes are no-ops — the count never decrements.
//  2. Updates always re-count (skipUpdateForUnchangedKeys = false) — even if the grouping
//     key doesn't change, the count increments.
//
// This tracks the total number of insert+update events, not the number of live records.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: little-endian int64 count
// Matches Java's AtomicMutationIndexMaintainer with COUNT_UPDATES mutation.
type CountUpdatesIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newCountUpdatesIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *CountUpdatesIndexMaintainer {
	return &CountUpdatesIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts: atomically adds +1 to each grouping key entry.
// For deletes: NO-OP — count never decrements.
// For updates: atomically adds +1 to each NEW grouping key entry (no common-key skip).
// Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys() with COUNT_UPDATES.
func (m *CountUpdatesIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	// Deletes are no-ops — Java's getMutationParam() returns null when remove=true.
	if newRecord == nil {
		return nil
	}

	newKeys, err := m.evaluateGroupingKeys(newRecord)
	if err != nil {
		return fmt.Errorf("evaluate count_updates index %q for new record: %w", m.index.Name, err)
	}

	// NOTE: No common-key optimization. Java's skipUpdateForUnchangedKeys() returns false
	// for COUNT_UPDATES — every insert/update increments, regardless of whether the
	// grouping key changed.
	for _, key := range newKeys {
		fdbKey := m.indexSubspace.Pack(key)
		if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, littleEndianInt64One); err != nil {
			return err
		}
		m.tx.Add(fdb.Key(fdbKey), littleEndianInt64One)
	}

	return nil
}

// UpdateWhileWriteOnly checks the index build range set before updating.
// COUNT_UPDATES is non-idempotent — blindly updating would cause double-counting.
// Matches Java's StandardIndexMaintainer.updateWriteOnlyByRecords().
func (m *CountUpdatesIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	// Deletes are already no-ops in Update(), but short-circuit here too.
	if newRecord == nil {
		return nil
	}
	return updateWhileWriteOnlyNonIdempotent(oldRecord, newRecord, m.index, m.store, "COUNT_UPDATES", m.Update)
}

// Scan scans COUNT_UPDATES index entries within the given tuple range.
// Reuses countKVCursor — identical wire format (little-endian int64 values).
func (m *CountUpdatesIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// evaluateGroupingKeys extracts the grouping key tuple(s) from a record.
func (m *CountUpdatesIndexMaintainer) evaluateGroupingKeys(record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	return evaluateGroupingKeys(m.index, record)
}

var _ IndexMaintainer = (*CountUpdatesIndexMaintainer)(nil)
