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

	// Scan scans the index within the given tuple range.
	// Matches Java's IndexMaintainer.scan().
	Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry]
}

// StandardIndexMaintainer handles VALUE index maintenance.
// Evaluates the index key expression against records, then sets/clears entries
// in the index subspace. Matches Java's StandardIndexMaintainer.
type StandardIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
}

func newStandardIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction) *StandardIndexMaintainer {
	return &StandardIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
	}
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
		oldEntries, newEntries = removeCommonEntries(oldEntries, newEntries)
	}

	// Remove old entries first so uniqueness checks see clean state
	for i := range oldEntries {
		m.tx.Clear(fdb.Key(m.indexSubspace.Pack(indexEntryKey(oldEntries[i].key, oldEntries[i].primaryKey))))
	}

	// Add new entries
	for i := range newEntries {
		entryTupleKey := indexEntryKey(newEntries[i].key, newEntries[i].primaryKey)
		keyBytes := m.indexSubspace.Pack(entryTupleKey)

		if m.index.IsUnique() {
			if err := m.checkUniqueness(newEntries[i]); err != nil {
				return err
			}
		}

		// VALUE index stores empty tuple as value
		m.tx.Set(fdb.Key(keyBytes), tuple.Tuple{}.Pack())
	}

	return nil
}

// Scan scans index entries within the given tuple range.
// Creates a KeyValueCursor over the index subspace and maps KVs to IndexEntry.
// Matches Java's StandardIndexMaintainer.scan().
func (m *StandardIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// indexEntry represents a single index entry (indexed values + record primary key).
type indexEntry struct {
	key        tuple.Tuple
	primaryKey tuple.Tuple
}

// evaluateIndex evaluates the index expression against a record to produce index entries.
// Fans out when the expression returns multiple key tuples (e.g. repeated fields).
// Matches Java's StandardIndexMaintainer.evaluateIndex().
func (m *StandardIndexMaintainer) evaluateIndex(record *FDBStoredRecord[proto.Message]) ([]indexEntry, error) {
	tuples, err := m.index.RootExpression.Evaluate(record.Record)
	if err != nil {
		return nil, err
	}

	entries := make([]indexEntry, len(tuples))
	for i, values := range tuples {
		key := make(tuple.Tuple, len(values))
		for j, v := range values {
			key[j] = v
		}
		entries[i] = indexEntry{key: key, primaryKey: record.PrimaryKey}
	}

	return entries, nil
}

// checkUniqueness verifies no other record has the same index value.
// Scans the index subspace for entries with the same index key but different primary key.
// Matches Java's StandardIndexMaintainer.checkUniqueness().
func (m *StandardIndexMaintainer) checkUniqueness(entry indexEntry) error {
	prefixKey := m.indexSubspace.Pack(entry.key)
	r, err := fdb.PrefixRange(prefixKey)
	if err != nil {
		return fmt.Errorf("prefix range for uniqueness check on index %q: %w", m.index.Name, err)
	}

	kvs, err := m.tx.GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return fmt.Errorf("uniqueness scan for index %q: %w", m.index.Name, err)
	}

	if len(kvs) == 0 {
		return nil
	}

	// Unpack the existing entry to extract its primary key
	existingTuple, err := m.indexSubspace.Unpack(kvs[0].Key)
	if err != nil {
		return fmt.Errorf("unpack existing index entry: %w", err)
	}

	// Primary key starts after the index key columns
	indexColCount := len(entry.key)
	if len(existingTuple) > indexColCount {
		existingPK := tuple.Tuple(existingTuple[indexColCount:])
		// If same PK, it's just our own record being updated — not a violation
		if !tuplesEqual(existingPK, entry.primaryKey) {
			return &RecordIndexUniquenessViolationError{
				IndexName:   m.index.Name,
				IndexKey:    entry.key,
				PrimaryKey:  entry.primaryKey,
				ExistingKey: existingPK,
			}
		}
	}

	return nil
}

// tuplesEqual compares two tuples by their packed byte representation.
func tuplesEqual(a, b tuple.Tuple) bool {
	return string(a.Pack()) == string(b.Pack())
}

// removeCommonEntries filters out entries that are identical in both old and new.
// This avoids unnecessary FDB mutations when a record update doesn't change
// the indexed value. Matches Java's StandardIndexMaintainer.commonKeys optimization.
func removeCommonEntries(old, new []indexEntry) ([]indexEntry, []indexEntry) {
	packEntry := func(e indexEntry) string {
		return string(indexEntryKey(e.key, e.primaryKey).Pack())
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
