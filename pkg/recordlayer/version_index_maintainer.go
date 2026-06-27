package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// versionIndexMaintainer handles VERSION index maintenance.
// VERSION indexes store the record's commit version (Versionstamp) in the index key,
// enabling efficient queries by version ordering.
//
// Key difference from standardIndexMaintainer: when the entry key contains an
// incomplete versionstamp (record being saved in this transaction), the entry
// is written via SET_VERSIONSTAMPED_KEY mutation instead of a regular set.
//
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.indexes.VersionIndexMaintainer.
type versionIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.WritableTransaction
	recordContext *FDBRecordContext
	store         indexStoreContext
}

func newVersionIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.WritableTransaction, recordContext *FDBRecordContext, store indexStoreContext) *versionIndexMaintainer {
	return &versionIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		recordContext: recordContext,
		store:         store,
	}
}

// UpdateWhileWriteOnly delegates to Update. VERSION indexes are idempotent.
func (m *versionIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// Note: uniqueness is validated at Build() time in metadata validation, so no
// runtime check is needed here.
func (m *versionIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var oldEntries []indexEntry
	var newEntries []indexEntry

	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate version index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}

	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate version index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	// Skip the removeCommonEntries optimization for VERSION indexes.
	// The version always changes on update (new record gets a new versionstamp),
	// so there are never common entries. Additionally, removeCommonEntries calls
	// tuple.Pack() which panics on incomplete versionstamps.

	// Remove old entries
	for i := range oldEntries {
		entryTupleKey, err := indexEntryKey(m.index, oldEntries[i].key, oldEntries[i].primaryKey)
		if err != nil {
			return err
		}
		hasIncomplete := tupleHasIncompleteVersionstamp(entryTupleKey)
		if hasIncomplete {
			keyBytes, err := m.indexSubspace.PackWithVersionstamp(entryTupleKey)
			if err != nil {
				return fmt.Errorf("packWithVersionstamp for delete on index %q: %w", m.index.Name, err)
			}
			m.recordContext.RemoveVersionMutation(keyBytes)
		} else {
			keyBytes := m.indexSubspace.Pack(entryTupleKey)
			m.tx.ClearBytes(keyBytes)
		}
	}

	// Add new entries
	emptyValue := tuple.Tuple{}.Pack()
	for i := range newEntries {
		entryTupleKey, err := indexEntryKey(m.index, newEntries[i].key, newEntries[i].primaryKey)
		if err != nil {
			return err
		}
		hasIncomplete := tupleHasIncompleteVersionstamp(entryTupleKey)

		// For KeyWithValueExpression indexes, store the value portion in the FDB value.
		valueBytes := emptyValue
		if newEntries[i].value != nil {
			valueBytes = newEntries[i].value.Pack()
		}

		if hasIncomplete {
			keyBytes, err := m.indexSubspace.PackWithVersionstamp(entryTupleKey)
			if err != nil {
				return fmt.Errorf("packWithVersionstamp for insert on index %q: %w", m.index.Name, err)
			}
			if err := checkKeyValueSizes(m.index, newEntries[i].primaryKey, keyBytes, valueBytes); err != nil {
				return err
			}
			m.recordContext.AddVersionMutation(MutationTypeSetVersionstampedKey, keyBytes, valueBytes)
		} else {
			keyBytes := m.indexSubspace.Pack(entryTupleKey)
			if err := checkKeyValueSizes(m.index, newEntries[i].primaryKey, keyBytes, valueBytes); err != nil {
				return err
			}
			m.tx.SetBytes(keyBytes, valueBytes)
		}
	}

	return nil
}

// Scan scans index entries within the given tuple range.
// VERSION indexes only support BY_VALUE scanning (same cursor as standardIndexMaintainer).
// Scan type validation (rejecting BY_RANK etc.) is handled at the store level in
// ScanIndexByType, which checks the maintainer type before dispatching. Java's
// VersionIndexMaintainer.scan(IndexScanType, ...) throws if scanType != BY_VALUE.
func (m *versionIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// DeleteWhere clears all index entries whose key starts with the given prefix.
func (m *versionIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// evaluateIndex evaluates the index expression against a record to produce index entries.
// Reuses the same logic as standardIndexMaintainer.evaluateIndex.
func (m *versionIndexMaintainer) evaluateIndex(record *FDBStoredRecord[proto.Message]) ([]indexEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	kwv, isKeyWithValue := m.index.RootExpression.(*KeyWithValueExpression)
	entries := make([]indexEntry, len(tuples))
	for i, values := range tuples {
		if isKeyWithValue {
			keyPart, valuePart := kwv.SplitEvaluatedKey(values)
			key := make(tuple.Tuple, len(keyPart))
			for j, v := range keyPart {
				key[j] = v
			}
			val := make(tuple.Tuple, len(valuePart))
			for j, v := range valuePart {
				val[j] = v
			}
			entries[i] = indexEntry{key: key, primaryKey: record.PrimaryKey, value: val}
		} else {
			key := make(tuple.Tuple, len(values))
			for j, v := range values {
				key[j] = v
			}
			entries[i] = indexEntry{key: key, primaryKey: record.PrimaryKey}
		}
	}

	return entries, nil
}

// tupleHasIncompleteVersionstamp checks if any element in the tuple is an
// incomplete Versionstamp (TransactionVersion all 0xFF).
// Matches Java's Tuple.hasIncompleteVersionstamp().
func tupleHasIncompleteVersionstamp(t tuple.Tuple) bool {
	for _, elem := range t {
		if vs, ok := elem.(tuple.Versionstamp); ok {
			allFF := true
			for _, b := range vs.TransactionVersion {
				if b != 0xFF {
					allFF = false
					break
				}
			}
			if allFF {
				return true
			}
		}
	}
	return false
}
