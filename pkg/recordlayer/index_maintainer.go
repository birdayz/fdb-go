package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// IndexMaintainer handles index updates and scanning.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexMaintainer.
type IndexMaintainer interface {
	// Update updates the index for a record change.
	// oldRecord is nil for inserts, newRecord is nil for deletes.
	Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error

	// UpdateWhileWriteOnly updates the index during WRITE_ONLY state (index being built).
	// For idempotent indexes (VALUE), this is a pass-through to Update().
	// For non-idempotent indexes, checks if the record's PK is in the already-built range.
	// Matches Java's StandardIndexMaintainer.updateWhileWriteOnly().
	UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error

	// Scan scans the index within the given tuple range.
	// Matches Java's IndexMaintainer.scan().
	Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry]

	// DeleteWhere clears all index entries whose key starts with the given prefix.
	// Uses FDB range clears — no scanning. Called by DeleteRecordsWhere.
	// Matches Java's IndexMaintainer.deleteWhere().
	DeleteWhere(prefix tuple.Tuple)
}

// indexStoreContext provides the store methods needed by index maintainers.
// Avoids circular dependency by using an interface instead of *FDBRecordStore directly.
type indexStoreContext interface {
	isIndexWriteOnly(index *Index) bool
	isIndexReadableUniquePending(index *Index) bool
	addUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple)
	removeUniquenessViolations(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple)
	// isKeyInIndexBuildRange checks if a primary key is in the already-built range
	// of an index being built online. Used by non-idempotent index maintainers
	// (COUNT) during WRITE_ONLY to avoid double-counting.
	// Matches Java's StandardIndexMaintainer.addedRangeWithKey().
	isKeyInIndexBuildRange(index *Index, primaryKey tuple.Tuple) (bool, error)
}

// StandardIndexMaintainer handles VALUE index maintenance.
// Evaluates the index key expression against records, then sets/clears entries
// in the index subspace. Matches Java's StandardIndexMaintainer.
type StandardIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newStandardIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *StandardIndexMaintainer {
	return &StandardIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
	}
}

// UpdateWhileWriteOnly updates the index during WRITE_ONLY state.
// StandardIndexMaintainer is idempotent, so this is a pass-through to Update().
// Matches Java's StandardIndexMaintainer.updateWhileWriteOnly() + isIdempotent() = true.
func (m *StandardIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// Matches Java's StandardIndexMaintainer.update().
func (m *StandardIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
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
		oldEntries, newEntries = removeCommonEntries(m.index, oldEntries, newEntries)
	}

	// Remove old entries first so uniqueness checks see clean state
	isWriteOnlyOrUniquePending := m.store != nil && (m.store.isIndexWriteOnly(m.index) || m.store.isIndexReadableUniquePending(m.index))
	for i := range oldEntries {
		m.tx.Clear(fdb.Key(m.indexSubspace.Pack(indexEntryKey(m.index, oldEntries[i].key, oldEntries[i].primaryKey))))
		// Clean up violation entries on delete for WRITE_ONLY/READABLE_UNIQUE_PENDING indexes.
		// Matches Java's StandardIndexMaintainer.updateOneKeyAsync() remove path.
		if isWriteOnlyOrUniquePending && m.index.IsUnique() && m.store != nil {
			m.store.removeUniquenessViolations(m.index, oldEntries[i].key, oldEntries[i].primaryKey)
		}
	}

	// Add new entries
	emptyValue := tuple.Tuple{}.Pack()
	for i := range newEntries {
		entryTupleKey := indexEntryKey(m.index, newEntries[i].key, newEntries[i].primaryKey)
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

		m.tx.Set(fdb.Key(keyBytes), valueBytes)
	}

	return nil
}

// Scan scans index entries within the given tuple range.
// Creates a KeyValueCursor over the index subspace and maps KVs to IndexEntry.
// Matches Java's StandardIndexMaintainer.scan().
func (m *StandardIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// deleteWhereRange clears all index entries whose key starts with the given prefix
// in the specified subspace. Uses FDB PrefixRange to include the exact prefix key
// (important for ungrouped aggregate indexes).
// Shared implementation for all index maintainer types.
func deleteWhereRange(tx fdb.Transaction, indexSubspace subspace.Subspace, prefix tuple.Tuple) {
	key := indexSubspace.Pack(prefix)
	if pr, err := fdb.PrefixRange(key); err == nil {
		tx.ClearRange(pr)
	}
}

// DeleteWhere clears all index entries whose key starts with the given prefix.
// Matches Java's StandardIndexMaintainer.deleteWhere().
func (m *StandardIndexMaintainer) DeleteWhere(prefix tuple.Tuple) {
	deleteWhereRange(m.tx, m.indexSubspace, prefix)
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
// Matches Java's StandardIndexMaintainer.evaluateIndex().
func (m *StandardIndexMaintainer) evaluateIndex(record *FDBStoredRecord[proto.Message]) ([]indexEntry, error) {
	// Check predicate for sparse/filtered indexes
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
			// Split at splitPoint: key columns go in FDB key, value columns in FDB value.
			// Matches Java's StandardIndexMaintainer.evaluateIndex() KeyWithValueExpression path.
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
// Matches Java's StandardIndexMaintainer.checkUniqueness().
func (m *StandardIndexMaintainer) checkUniqueness(entry indexEntry) error {
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
		existingTuple, err := m.indexSubspace.Unpack(kv.Key)
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
		// Matches Java's StandardIndexMaintainer.checkUniqueness() which
		// calls addUniquenessViolation() for both conflicting PKs.
		if m.store != nil && m.store.isIndexWriteOnly(m.index) {
			m.store.addUniquenessViolation(m.index, entry.key, entry.primaryKey)
			m.store.addUniquenessViolation(m.index, entry.key, existingPK)
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
// Matches Java's StandardIndexMaintainer.checkKeyValueSizes().
func checkKeyValueSizes(index *Index, primaryKey tuple.Tuple, keyBytes, valueBytes []byte) error {
	if len(keyBytes) > KeySizeLimit {
		return &IndexKeySizeError{
			IndexName:  index.Name,
			PrimaryKey: primaryKey,
			KeySize:    len(keyBytes),
			Limit:      KeySizeLimit,
		}
	}
	if len(valueBytes) > ValueSizeLimit {
		return &IndexValueSizeError{
			IndexName:  index.Name,
			PrimaryKey: primaryKey,
			ValueSize:  len(valueBytes),
			Limit:      ValueSizeLimit,
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
// the indexed value. Matches Java's StandardIndexMaintainer.commonKeys optimization.
func removeCommonEntries(idx *Index, old, new []indexEntry) ([]indexEntry, []indexEntry) {
	packEntry := func(e indexEntry) string {
		// Include value in the comparison key for KeyWithValueExpression indexes.
		// Matches Java's IndexEntry.equals() which compares both key and value.
		s := string(indexEntryKey(idx, e.key, e.primaryKey).Pack())
		if e.value != nil {
			s += "|" + string(e.value.Pack())
		}
		return s
	}

	newSet := make(map[string]struct{}, len(new))
	for _, e := range new {
		newSet[packEntry(e)] = struct{}{}
	}

	common := make(map[string]struct{})
	var filteredOld []indexEntry
	for _, e := range old {
		p := packEntry(e)
		if _, ok := newSet[p]; ok {
			common[p] = struct{}{}
		} else {
			filteredOld = append(filteredOld, e)
		}
	}

	var filteredNew []indexEntry
	for _, e := range new {
		if _, ok := common[packEntry(e)]; !ok {
			filteredNew = append(filteredNew, e)
		}
	}

	return filteredOld, filteredNew
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
