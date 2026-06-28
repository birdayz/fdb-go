package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// IndexValidationResult contains the results of an index validation.
type IndexValidationResult struct {
	// MissingEntries are index entries that should exist (based on records) but don't.
	MissingEntries []IndexValidationEntry
	// OrphanedEntries are index entries that exist but have no corresponding record.
	OrphanedEntries []IndexValidationEntry
	// TotalRecordsScanned is the number of records checked.
	TotalRecordsScanned int
	// TotalEntriesScanned is the number of index entries checked.
	TotalEntriesScanned int
}

// IsValid returns true if no discrepancies were found.
func (r *IndexValidationResult) IsValid() bool {
	return len(r.MissingEntries) == 0 && len(r.OrphanedEntries) == 0
}

// IndexValidationEntry represents a single missing or orphaned index entry.
type IndexValidationEntry struct {
	IndexKey   tuple.Tuple
	PrimaryKey tuple.Tuple
}

// ValidateIndex checks the consistency of an index against the actual records.
// Detects missing entries (record exists but index entry missing) and
// orphaned entries (index entry exists but no corresponding record).
// Matches Java's StandardIndexMaintainer.validateEntries().
func (store *FDBRecordStore) ValidateIndex(ctx context.Context, index *Index) (*IndexValidationResult, error) {
	result := &IndexValidationResult{}

	// Phase 1: Scan all records and compute expected index entries
	expectedEntries := make(map[string]IndexValidationEntry)
	cursor := store.ScanRecords(nil, ForwardScan())
	for record, err := range Seq2(cursor, ctx) {
		if err != nil {
			return nil, fmt.Errorf("scan records for validation: %w", err)
		}
		result.TotalRecordsScanned++

		// Check if this record type should have entries in this index
		if !store.recordTypeHasIndex(record.RecordType, index) {
			continue
		}

		tuples, err := index.RootExpression.Evaluate(record, record.Record)
		if err != nil {
			continue // Skip records that can't be evaluated (e.g., missing field)
		}

		for _, values := range tuples {
			key := make(tuple.Tuple, len(values))
			for j, v := range values {
				key[j] = v
			}
			entryKey, err := indexEntryKey(index, key, record.PrimaryKey)
			if err != nil {
				return nil, fmt.Errorf("validate index %q: malformed entry for PK %v: %w", index.Name, record.PrimaryKey, err)
			}
			packed := string(entryKey.Pack())
			expectedEntries[packed] = IndexValidationEntry{
				IndexKey:   key,
				PrimaryKey: record.PrimaryKey,
			}
		}
	}

	// Phase 2: Scan all index entries and check against expected
	indexSub := store.indexSubspace(index)
	begin, end := indexSub.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}
	kvs, err := store.context.Transaction().GetRange(kr, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("scan index entries for validation: %w", err)
	}

	actualEntries := make(map[string]struct{}, len(kvs))
	colCount := index.RootExpression.ColumnSize()

	for _, kv := range kvs {
		result.TotalEntriesScanned++
		t, err := fastSubspaceUnpack(kv.Key, len(indexSub.Bytes()))
		if err != nil {
			continue
		}

		packed := string(tuple.Tuple(t).Pack())
		actualEntries[packed] = struct{}{}

		if _, found := expectedEntries[packed]; !found {
			// Orphaned entry — exists in index but not expected from records
			if len(t) > colCount {
				result.OrphanedEntries = append(result.OrphanedEntries, IndexValidationEntry{
					IndexKey:   tuple.Tuple(t[:colCount]),
					PrimaryKey: tuple.Tuple(t[colCount:]),
				})
			}
		}
	}

	// Phase 3: Find missing entries
	for packed, entry := range expectedEntries {
		if _, found := actualEntries[packed]; !found {
			result.MissingEntries = append(result.MissingEntries, entry)
		}
	}

	return result, nil
}

// recordTypeHasIndex checks if a record type has the given index.
func (store *FDBRecordStore) recordTypeHasIndex(rt *RecordType, index *Index) bool {
	for _, idx := range store.metaData.GetIndexesForRecordType(rt.Name) {
		if idx.Name == index.Name {
			return true
		}
	}
	for _, idx := range store.metaData.GetUniversalIndexes() {
		if idx.Name == index.Name {
			return true
		}
	}
	return false
}

// ScanRecords is needed by ValidateIndex — let's check if it exists already.
// It should scan all records. If it doesn't exist, we scan via the records subspace.
func (store *FDBRecordStore) scanAllRecords(ctx context.Context) ([]*FDBStoredRecord[proto.Message], error) {
	cursor := store.ScanRecords(nil, ForwardScan())
	return AsList(ctx, cursor)
}
