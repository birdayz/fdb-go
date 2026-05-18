package recordlayer

import (
	"fmt"
	"unsafe"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// emptyTuplePacked is the pre-computed packed form of an empty tuple.
// Used as the value for VALUE index entries (which store no value data).
var emptyTuplePacked = tuple.Tuple{}.Pack()

// SAFETY: The unsafe cast (*tuple.Tuple)(unsafe.Pointer(&[]any{})) in the
// insert fast path depends on tuple.Tuple being []TupleElement where
// TupleElement is a defined type with underlying type any. From tuple.go:
//   type TupleElement any   // defined type, underlying = interface{}
//   type Tuple []TupleElement
// Both []any and []TupleElement have identical memory layout (slice of
// 16-byte interface values). If TupleElement ever becomes a struct or
// constrained interface, the cast silently corrupts memory.
// No compile-time check can enforce this — verify on tuple.go changes.

// IndexMaintainer handles index updates and scanning.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexMaintainer.
type IndexMaintainer interface {
	// Update updates the index for a record change.
	// oldRecord is nil for inserts, newRecord is nil for deletes.
	Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error

	// UpdateWhileWriteOnly updates the index during WRITE_ONLY state (index being built).
	// For idempotent indexes (VALUE), this is a pass-through to Update().
	// For non-idempotent indexes, checks if the record's PK is in the already-built range.
	// Matches Java's standardIndexMaintainer.updateWhileWriteOnly().
	UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error

	// Scan scans the index within the given tuple range.
	// Matches Java's IndexMaintainer.scan().
	Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry]

	// DeleteWhere clears all index entries whose key starts with the given prefix.
	// Uses FDB range clears — no scanning. Called by DeleteRecordsWhere.
	// Matches Java's IndexMaintainer.deleteWhere().
	DeleteWhere(prefix tuple.Tuple) error
}

// indexStoreContext provides the store methods needed by index maintainers.
// Avoids circular dependency by using an interface instead of *FDBRecordStore directly.
type indexStoreContext interface {
	isIndexWriteOnly(index *Index) bool
	isIndexReadableUniquePending(index *Index) bool
	addUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple, existingKey tuple.Tuple) error
	removeUniquenessViolations(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error
	// isKeyInIndexBuildRange checks if a primary key is in the already-built range
	// of an index being built online. Used by non-idempotent index maintainers
	// (COUNT) during WRITE_ONLY to avoid double-counting.
	// Matches Java's standardIndexMaintainer.addedRangeWithKey().
	isKeyInIndexBuildRange(index *Index, primaryKey tuple.Tuple) (bool, error)

	// AcquireWriteLock acquires an exclusive lock for the given subspace key.
	// Used by tree-structured indexes (HNSW, R-tree) to serialize mutations.
	// Matches Java's FDBRecordContext.doWithWriteLock(LockIdentifier).
	AcquireWriteLock(key string)
	ReleaseWriteLock(key string)
	// AcquireReadLock acquires a shared lock for the given subspace key.
	// Used by tree-structured indexes to allow concurrent reads during scans.
	// Matches Java's FDBRecordContext.doWithReadLock(LockIdentifier).
	AcquireReadLock(key string)
	ReleaseReadLock(key string)
}

// standardIndexMaintainer handles VALUE index maintenance.
// Evaluates the index key expression against records, then sets/clears entries
// in the index subspace. Matches Java's standardIndexMaintainer.
type standardIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newStandardIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *standardIndexMaintainer {
	return &standardIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
	}
}

// UpdateWhileWriteOnly updates the index during WRITE_ONLY state.
// standardIndexMaintainer is idempotent, so this is a pass-through to Update().
// Matches Java's standardIndexMaintainer.updateWhileWriteOnly() + isIdempotent() = true.
func (m *standardIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// Matches Java's standardIndexMaintainer.update().
func (m *standardIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	// Fast path: insert-only — skip evaluateIndex wrapper,
	// common-entry filtering, and old-entries loop.
	if oldRecord == nil && newRecord != nil {
		_, isKWV := m.index.RootExpression.(*KeyWithValueExpression)
		if !isKWV && (m.index.Predicate == nil || m.index.Predicate(newRecord.Record)) {
			// Int64 fast path: avoids any boxing alloc for integer fields.
			if ie, ok := m.index.RootExpression.(Int64Evaluator); ok {
				val, ok, err := ie.EvaluateInt64(newRecord, newRecord.Record)
				if err == nil && ok {
					return m.insertInt64Entry(val, newRecord)
				}
			}
			// DirectPacker: encode fields straight into FDB key bytes.
			// Avoids scalarToInterface boxing — handles both single-field and
			// composite keys without going through any.
			if dp, ok := m.index.RootExpression.(DirectPacker); ok {
				pk := tuple.GetPacker()
				pk.Reset()
				if dp.PackDirect(pk, newRecord, newRecord.Record) {
					trimmedPK, pkErr := m.index.TrimPrimaryKey(newRecord.PrimaryKey)
					if pkErr == nil {
						pk.EncodeTuple(trimmedPK)
						var buf []byte
						keyBytes := pk.AppendInto(&buf, m.indexSubspace.Bytes())
						tuple.PutPacker(pk)
						if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, keyBytes, emptyTuplePacked); err != nil {
							return err
						}
						if m.index.IsUnique() {
							keyTuple, uErr := fastSubspaceUnpack(keyBytes, len(m.indexSubspace.Bytes()))
							if uErr != nil {
								return fmt.Errorf("unpack index key for uniqueness check: %w", uErr)
							}
							colCount := m.index.RootExpression.ColumnSize()
							if colCount > 0 && len(keyTuple) > colCount {
								entry := indexEntry{key: keyTuple[:colCount], primaryKey: newRecord.PrimaryKey, value: tuple.Tuple{}}
								if err := m.checkUniqueness(entry); err != nil {
									return err
								}
							}
						}
						m.tx.Set(fdb.Key(keyBytes), emptyTuplePacked)
						return nil
					}
				}
				tuple.PutPacker(pk)
			}
			if fe, ok := m.index.RootExpression.(FlatEvaluator); ok {
				values, err := fe.EvaluateFlat(newRecord, newRecord.Record)
				if err == nil {
					// Zero-alloc: reinterpret []any as tuple.Tuple (same layout).
					key := *(*tuple.Tuple)(unsafe.Pointer(&values))
					entry := indexEntry{key: key, primaryKey: newRecord.PrimaryKey, value: tuple.Tuple{}}
					return m.insertSingleEntry(entry, newRecord)
				}
			}
		}
	}

	var oldEntries []indexEntry
	var newEntries []indexEntry

	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}

	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	// Skip unchanged entries (optimization matching Java's skipUpdateForUnchangedKeys)
	if oldEntries != nil && newEntries != nil {
		var err error
		oldEntries, newEntries, err = removeCommonEntries(m.index, oldEntries, newEntries)
		if err != nil {
			return err
		}
	}

	// Remove old entries first so uniqueness checks see clean state
	isWriteOnlyOrUniquePending := m.store != nil && (m.store.isIndexWriteOnly(m.index) || m.store.isIndexReadableUniquePending(m.index))
	for i := range oldEntries {
		oldEntryKey, err := indexEntryKey(m.index, oldEntries[i].key, oldEntries[i].primaryKey)
		if err != nil {
			return err
		}
		m.tx.ClearBytes(m.indexSubspace.Pack(oldEntryKey))
		// Clean up violation entries on delete for WRITE_ONLY/READABLE_UNIQUE_PENDING indexes.
		// Matches Java's standardIndexMaintainer.updateOneKeyAsync() remove path.
		if isWriteOnlyOrUniquePending && m.index.IsUnique() && m.store != nil {
			if err := m.store.removeUniquenessViolations(m.index, oldEntries[i].key, oldEntries[i].primaryKey); err != nil {
				return err
			}
		}
	}

	// Add new entries
	emptyValue := emptyTuplePacked
	for i := range newEntries {
		entryTupleKey, err := indexEntryKey(m.index, newEntries[i].key, newEntries[i].primaryKey)
		if err != nil {
			return err
		}
		keyBytes := m.indexSubspace.Pack(entryTupleKey)

		// For KeyWithValueExpression indexes, store the value portion in the FDB value.
		// Otherwise, store empty tuple (standard VALUE index behavior).
		valueBytes := emptyValue
		if newEntries[i].value != nil {
			valueBytes = newEntries[i].value.Pack()
		}

		if err := checkKeyValueSizes(m.index, newEntries[i].primaryKey, keyBytes, valueBytes); err != nil {
			return err
		}

		if m.index.IsUnique() && !indexKeyContainsNull(newEntries[i].key) {
			if err := m.checkUniqueness(newEntries[i]); err != nil {
				return err
			}
		}

		m.tx.SetBytes(keyBytes, valueBytes)
	}

	return nil
}

// insertInt64Entry handles inserting a VALUE index entry for an integer field.
// Avoids the any boxing allocation by packing int64 directly.
func (m *standardIndexMaintainer) insertInt64Entry(val int64, record *FDBStoredRecord[proto.Message]) error {
	trimmedPK, err := m.index.TrimPrimaryKey(record.PrimaryKey)
	if err != nil {
		return err
	}

	var keyBytes []byte
	if len(trimmedPK) == 0 {
		keyBytes = tuple.Pack1WithPrefix(m.indexSubspace.Bytes(), val)
	} else {
		keyBytes = tuple.Pack1ConcatWithPrefix(m.indexSubspace.Bytes(), val, trimmedPK)
	}

	if err := checkKeyValueSizes(m.index, record.PrimaryKey, keyBytes, emptyTuplePacked); err != nil {
		return err
	}

	if m.index.IsUnique() {
		entry := indexEntry{key: tuple.Tuple{val}, primaryKey: record.PrimaryKey, value: tuple.Tuple{}}
		if err := m.checkUniqueness(entry); err != nil {
			return err
		}
	}

	m.tx.Set(fdb.Key(keyBytes), emptyTuplePacked)
	return nil
}

func (m *standardIndexMaintainer) insertScalarEntry(val any, record *FDBStoredRecord[proto.Message]) error {
	trimmedPK, err := m.index.TrimPrimaryKey(record.PrimaryKey)
	if err != nil {
		return err
	}

	var keyBytes []byte
	if len(trimmedPK) == 0 {
		keyBytes = tuple.Pack1WithPrefix(m.indexSubspace.Bytes(), tuple.TupleElement(val))
	} else {
		keyBytes = tuple.Pack1ConcatWithPrefix(m.indexSubspace.Bytes(), tuple.TupleElement(val), trimmedPK)
	}

	if err := checkKeyValueSizes(m.index, record.PrimaryKey, keyBytes, emptyTuplePacked); err != nil {
		return err
	}

	if m.index.IsUnique() && val != nil {
		entry := indexEntry{key: tuple.Tuple{tuple.TupleElement(val)}, primaryKey: record.PrimaryKey, value: tuple.Tuple{}}
		if err := m.checkUniqueness(entry); err != nil {
			return err
		}
	}

	m.tx.Set(fdb.Key(keyBytes), emptyTuplePacked)
	return nil
}

// insertSingleEntry handles the insert of a single VALUE index entry.
// Extracted from the Update loop to support the insert-only fast path.
func (m *standardIndexMaintainer) insertSingleEntry(entry indexEntry, record *FDBStoredRecord[proto.Message]) error {
	trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
	if err != nil {
		return err
	}

	// Pack index values + trimmed PK directly into the key, avoiding
	// the intermediate tuple allocation in indexEntryKey.
	var keyBytes fdb.Key
	if len(trimmedPK) == 0 {
		keyBytes = m.indexSubspace.Pack(entry.key)
	} else if len(entry.key) == 0 {
		keyBytes = m.indexSubspace.Pack(trimmedPK)
	} else {
		keyBytes = fdb.Key(tuple.PackConcatWithPrefix(m.indexSubspace.Bytes(), entry.key, trimmedPK))
	}

	if err := checkKeyValueSizes(m.index, entry.primaryKey, keyBytes, emptyTuplePacked); err != nil {
		return err
	}

	if m.index.IsUnique() && !indexKeyContainsNull(entry.key) {
		if err := m.checkUniqueness(entry); err != nil {
			return err
		}
	}

	m.tx.Set(keyBytes, emptyTuplePacked)
	return nil
}

// Scan scans index entries within the given tuple range.
// Creates a KeyValueCursor over the index subspace and maps KVs to IndexEntry.
// Matches Java's standardIndexMaintainer.scan().
func (m *standardIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// deleteWhereRange clears all index entries whose key starts with the given prefix
// in the specified subspace. Uses FDB PrefixRange to include the exact prefix key
// (important for ungrouped aggregate indexes).
// Shared implementation for all index maintainer types.
func deleteWhereRange(tx fdb.Transaction, indexSubspace subspace.Subspace, prefix tuple.Tuple) error {
	key := indexSubspace.Pack(prefix)
	pr, err := fdb.PrefixRange(key)
	if err != nil {
		return fmt.Errorf("deleteWhereRange: PrefixRange(%x): %w", key, err)
	}
	tx.ClearRange(pr)
	return nil
}

// DeleteWhere clears all index entries whose key starts with the given prefix.
// Matches Java's standardIndexMaintainer.deleteWhere().
func (m *standardIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// indexEntry represents a single index entry (indexed values + record primary key).
type indexEntry struct {
	key        tuple.Tuple
	primaryKey tuple.Tuple
	value      tuple.Tuple // Non-nil for KeyWithValueExpression covering indexes
}

// evaluateIndex evaluates the index expression against a record to produce index entries.
// Fans out when the expression returns multiple key tuples (e.g. repeated fields).
// If the index has a predicate and the record doesn't match, returns nil (no entries).
// Matches Java's standardIndexMaintainer.evaluateIndex().
func (m *standardIndexMaintainer) evaluateIndex(record *FDBStoredRecord[proto.Message]) ([]indexEntry, error) {
	// Check predicate for sparse/filtered indexes
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	kwv, isKeyWithValue := m.index.RootExpression.(*KeyWithValueExpression)

	// Fast path: EvaluateFlat for simple non-KeyWithValue, non-fan-out indexes.
	// Avoids the [][]any allocation from Evaluate.
	if !isKeyWithValue {
		if fe, ok := m.index.RootExpression.(FlatEvaluator); ok {
			values, err := fe.EvaluateFlat(record, record.Record)
			if err == nil {
				key := make(tuple.Tuple, len(values))
				for j, v := range values {
					key[j] = v
				}
				return []indexEntry{{key: key, primaryKey: record.PrimaryKey, value: tuple.Tuple{}}}, nil
			}
			// Fall through on error (e.g. fan-out)
		}
	}

	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}

	entries := make([]indexEntry, len(tuples))
	for i, values := range tuples {
		if isKeyWithValue {
			// Split at splitPoint: key columns go in FDB key, value columns in FDB value.
			// Matches Java's standardIndexMaintainer.evaluateIndex() KeyWithValueExpression path.
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

// checkUniqueness verifies no other record has the same index value.
// Scans the FULL prefix range (no limit) so FDB's read-conflict tracking
// covers the entire range, preventing concurrent inserts of conflicting entries.
// Java reads the full range too (no limit) and registers the scan as a
// commit check via addIndexUniquenessCommitCheck().
// Matches Java's standardIndexMaintainer.checkUniqueness().
func (m *standardIndexMaintainer) checkUniqueness(entry indexEntry) error {
	prefixKey := m.indexSubspace.Pack(entry.key)
	r, err := fdb.PrefixRange(prefixKey)
	if err != nil {
		return fmt.Errorf("prefix range for uniqueness check on index %q: %w", m.index.Name, err)
	}

	// No Limit — read the full range so FDB records a read conflict on the
	// entire prefix. With Limit:1, FDB only tracks conflict up to the first
	// key found, allowing concurrent inserts at higher keys to go undetected.
	kvs, err := m.tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return fmt.Errorf("uniqueness scan for index %q: %w", m.index.Name, err)
	}

	indexColCount := len(entry.key)

	for _, kv := range kvs {
		existingTuple, err := fastSubspaceUnpack(kv.Key, len(m.indexSubspace.Bytes()))
		if err != nil {
			return fmt.Errorf("unpack existing index entry: %w", err)
		}

		if len(existingTuple) <= indexColCount {
			continue
		}

		// Reconstruct full PK from the FDB entry key using getEntryPrimaryKey.
		// The raw existingTuple[indexColCount:] is the TRIMMED PK (deduped
		// components removed). Must use getEntryPrimaryKey to get full PK
		// for correct comparison and violation entries.
		// Matches Java's Index.getEntryPrimaryKey(indexEntry).
		existingPK := m.index.getEntryPrimaryKey(existingTuple)
		if tuplesEqual(existingPK, entry.primaryKey) {
			continue // Our own record — not a violation
		}

		// WRITE_ONLY indexes: write violation entries instead of throwing.
		// Matches Java's standardIndexMaintainer.checkUniqueness() which
		// calls addUniquenessViolation() for both conflicting PKs.
		if m.store != nil && m.store.isIndexWriteOnly(m.index) {
			if err := m.store.addUniquenessViolation(m.index, entry.key, entry.primaryKey, existingPK); err != nil {
				return err
			}
			if err := m.store.addUniquenessViolation(m.index, entry.key, existingPK, entry.primaryKey); err != nil {
				return err
			}
			return nil
		}
		return &RecordIndexUniquenessViolationError{
			IndexName:   m.index.Name,
			IndexKey:    entry.key,
			PrimaryKey:  entry.primaryKey,
			ExistingKey: existingPK,
		}
	}

	return nil
}

// checkKeyValueSizes validates that an index entry's key and value don't exceed
// FDB limits. Called on insert only (not delete).
// Matches Java's standardIndexMaintainer.checkKeyValueSizes().
func checkKeyValueSizes(index *Index, primaryKey tuple.Tuple, keyBytes, valueBytes []byte) error {
	if len(keyBytes) > keySizeLimit {
		return &IndexKeySizeError{
			IndexName:  index.Name,
			PrimaryKey: primaryKey,
			KeySize:    len(keyBytes),
			Limit:      keySizeLimit,
		}
	}
	if len(valueBytes) > valueSizeLimit {
		return &IndexValueSizeError{
			IndexName:  index.Name,
			PrimaryKey: primaryKey,
			ValueSize:  len(valueBytes),
			Limit:      valueSizeLimit,
		}
	}
	return nil
}

// IndexKeySizeError indicates an index entry key exceeds the FDB key size limit.
// Matches Java's FDBExceptions.FDBStoreKeySizeException.
type IndexKeySizeError struct {
	IndexName  string
	PrimaryKey tuple.Tuple
	KeySize    int
	Limit      int
}

func (e *IndexKeySizeError) Error() string {
	return fmt.Sprintf("index entry key too large for index %q (pk=%v): %d bytes exceeds limit %d",
		e.IndexName, e.PrimaryKey, e.KeySize, e.Limit)
}

// IndexValueSizeError indicates an index entry value exceeds the FDB value size limit.
// Matches Java's FDBExceptions.FDBStoreValueSizeException.
type IndexValueSizeError struct {
	IndexName  string
	PrimaryKey tuple.Tuple
	ValueSize  int
	Limit      int
}

func (e *IndexValueSizeError) Error() string {
	return fmt.Sprintf("index entry value too large for index %q (pk=%v): %d bytes exceeds limit %d",
		e.IndexName, e.PrimaryKey, e.ValueSize, e.Limit)
}

// indexKeyContainsNull returns true if any element of the index key is nil.
// Matches Java's IndexEntry.keyContainsNonUniqueNull(): when an index key
// component is null (from NullStandin.NULL), uniqueness checks are skipped.
func indexKeyContainsNull(key tuple.Tuple) bool {
	for _, v := range key {
		if v == nil {
			return true
		}
	}
	return false
}

// tuplesEqual compares two tuples by their packed byte representation.
func tuplesEqual(a, b tuple.Tuple) bool {
	return string(a.Pack()) == string(b.Pack())
}

// removeCommonEntries filters out entries that are identical in both old and new.
// This avoids unnecessary FDB mutations when a record update doesn't change
// the indexed value. Matches Java's standardIndexMaintainer.commonKeys optimization.
func removeCommonEntries(idx *Index, old, new []indexEntry) ([]indexEntry, []indexEntry, error) {
	packEntry := func(e indexEntry) (string, error) {
		// Include value in the comparison key for KeyWithValueExpression indexes.
		// Matches Java's IndexEntry.equals() which compares both key and value.
		ek, err := indexEntryKey(idx, e.key, e.primaryKey)
		if err != nil {
			return "", err
		}
		s := string(ek.Pack())
		if e.value != nil {
			s += "|" + string(e.value.Pack())
		}
		return s, nil
	}

	newSet := make(map[string]struct{}, len(new))
	for _, e := range new {
		p, err := packEntry(e)
		if err != nil {
			return nil, nil, err
		}
		newSet[p] = struct{}{}
	}

	common := make(map[string]struct{})
	var filteredOld []indexEntry
	for _, e := range old {
		p, err := packEntry(e)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := newSet[p]; ok {
			common[p] = struct{}{}
		} else {
			filteredOld = append(filteredOld, e)
		}
	}

	var filteredNew []indexEntry
	for _, e := range new {
		p, err := packEntry(e)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := common[p]; !ok {
			filteredNew = append(filteredNew, e)
		}
	}

	return filteredOld, filteredNew, nil
}

// RecordIndexUniquenessViolationError indicates a unique index constraint violation.
// Matches Java's RecordIndexUniquenessViolation.
type RecordIndexUniquenessViolationError struct {
	IndexName   string
	IndexKey    tuple.Tuple
	PrimaryKey  tuple.Tuple
	ExistingKey tuple.Tuple
}

func (e *RecordIndexUniquenessViolationError) Error() string {
	return fmt.Sprintf("uniqueness violation for index %q: value %v already exists for record %v (new record: %v)",
		e.IndexName, e.IndexKey, e.ExistingKey, e.PrimaryKey)
}
