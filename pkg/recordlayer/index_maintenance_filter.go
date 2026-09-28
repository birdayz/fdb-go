package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// IndexValues is Java's IndexMaintenanceFilter.IndexValues: which of a
// record's index entries an index maintains.
type IndexValues int

const (
	// IndexValuesAll maintains every entry.
	IndexValuesAll IndexValues = iota
	// IndexValuesNone maintains none: the index is maintained as if the
	// record had no entries.
	IndexValuesNone
	// IndexValuesSome maintains the entries MaintainIndexValue admits.
	IndexValuesSome
)

// IndexMaintenanceFilter is Java's IndexMaintenanceFilter
// (IndexMaintenanceFilter.java), a store option: per index and record, which
// index entries are maintained. An index's predicate is applied first; a record
// it refuses is IndexValuesNone whatever the filter says
// (IndexMaintenanceUtils.getFilterTypeForRecord). Set it with
// StoreBuilder.SetIndexMaintenanceFilter; the default is
// IndexMaintenanceFilterNormal.
type IndexMaintenanceFilter interface {
	// MaintainIndex is Java's maintainIndex.
	MaintainIndex(index *Index, record proto.Message) IndexValues
	// MaintainIndexValue is Java's maintainIndexValue, asked of each entry
	// only when MaintainIndex returned IndexValuesSome. The entry's Key is the
	// evaluated index key, without the primary key, and its Value the
	// evaluated value (a key-with-value index's value columns), as Java's
	// IndexEntry is when the filter sees it.
	MaintainIndexValue(index *Index, record proto.Message, entry *IndexEntry) bool
}

type normalMaintenanceFilter struct{}

func (normalMaintenanceFilter) MaintainIndex(*Index, proto.Message) IndexValues {
	return IndexValuesAll
}

func (normalMaintenanceFilter) MaintainIndexValue(*Index, proto.Message, *IndexEntry) bool {
	return true
}

type noNullsMaintenanceFilter struct{}

func (noNullsMaintenanceFilter) MaintainIndex(*Index, proto.Message) IndexValues {
	return IndexValuesSome
}

func (noNullsMaintenanceFilter) MaintainIndexValue(index *Index, _ proto.Message, entry *IndexEntry) bool {
	return !keyContainsNonUniqueNull(index.RootExpression, entry.Key)
}

var (
	// IndexMaintenanceFilterNormal is Java's NORMAL: every entry of every
	// record, the default.
	IndexMaintenanceFilterNormal IndexMaintenanceFilter = normalMaintenanceFilter{}
	// IndexMaintenanceFilterNoNulls is Java's NO_NULLS: no entry whose key
	// holds a NullStandin.NULL (IndexEntry.keyContainsNonUniqueNull).
	IndexMaintenanceFilterNoNulls IndexMaintenanceFilter = noNullsMaintenanceFilter{}
)

// maintenanceFilterOf is the filter of the store a maintainer belongs to, the
// default when it has none.
func maintenanceFilterOf(store indexStoreContext) IndexMaintenanceFilter {
	if store == nil {
		return IndexMaintenanceFilterNormal
	}
	return store.indexMaintenanceFilter()
}

// indexValuesFor is Java's IndexMaintenanceUtils.getFilterTypeForRecord
// (IndexMaintenanceUtils.java:37-66): a record the index's predicate refuses
// is IndexValuesNone, and otherwise the store's filter decides. A maintainer's
// fast path, which evaluates no entry list, runs only for IndexValuesAll.
func indexValuesFor(store indexStoreContext, index *Index, record *FDBStoredRecord[proto.Message]) IndexValues {
	if index.Predicate != nil && !index.Predicate(record.Record) {
		return IndexValuesNone
	}
	return maintenanceFilterOf(store).MaintainIndex(index, record.Record)
}

// keepMaintainedEntries is the IndexValuesSome arm of Java's
// filteredIndexEntries (StandardIndexMaintainer.java:367-383): the entries the
// store's filter admits, in order. Under any other value it returns entries.
func keepMaintainedEntries(store indexStoreContext, index *Index, record *FDBStoredRecord[proto.Message], values IndexValues, entries []indexEntry) []indexEntry {
	if values != IndexValuesSome {
		return entries
	}
	filter := maintenanceFilterOf(store)
	kept := make([]indexEntry, 0, len(entries))
	for _, e := range entries {
		if filter.MaintainIndexValue(index, record.Record, &IndexEntry{Index: index, Key: e.key, Value: e.value}) {
			kept = append(kept, e)
		}
	}
	return kept
}

// maintainedKeyTuples evaluates an index whose entries are its whole
// evaluated keys (the atomic indexes: Java's IndexEntry(index, key)) and keeps
// those the store's filter admits: nil under IndexValuesNone, every key under
// IndexValuesAll, the admitted ones under IndexValuesSome.
func maintainedKeyTuples(store indexStoreContext, index *Index, record *FDBStoredRecord[proto.Message], values IndexValues) ([][]any, error) {
	if values == IndexValuesNone {
		return nil, nil
	}
	tuples, err := index.RootExpression.Evaluate(record, record.Record)
	if err != nil || values != IndexValuesSome {
		return tuples, err
	}
	filter := maintenanceFilterOf(store)
	kept := make([][]any, 0, len(tuples))
	for _, t := range tuples {
		key := make(tuple.Tuple, len(t))
		for i, v := range t {
			key[i] = v
		}
		if filter.MaintainIndexValue(index, record.Record, &IndexEntry{Index: index, Key: key, Value: tuple.Tuple{}}) {
			kept = append(kept, t)
		}
	}
	return kept, nil
}

// writeOnlyBuildSource: see indexStoreContext.
func (store *FDBRecordStore) writeOnlyBuildSource(index *Index) (*Index, error) {
	stamp, err := store.LoadIndexingTypeStamp(index)
	if err != nil || stamp == nil {
		return nil, err
	}
	switch stamp.GetMethod() {
	case gen.IndexBuildIndexingStamp_BY_RECORDS, gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS,
		gen.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS:
		return nil, nil
	case gen.IndexBuildIndexingStamp_BY_INDEX:
		// Java: Tuple.fromBytes(sourceIndexSubspaceKey).get(0), then
		// RecordMetaData.getIndexFromSubspaceKey, which throws on a miss.
		key, err := tuple.Unpack(stamp.GetSourceIndexSubspaceKey())
		if err != nil || len(key) == 0 {
			return nil, &RecordCoreError{Message: "unable to update write-only index with current type stamp", IndexName: index.Name}
		}
		source := store.metaData.GetIndexFromSubspaceKey(key[0])
		if source == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("Unknown index subspace key %v", key[0])}
		}
		return source, nil
	default:
		return nil, &RecordCoreError{Message: "unable to update write-only index with current type stamp", IndexName: index.Name}
	}
}

// sourceIndexEntryKey: see indexStoreContext. The source of a BY_INDEX build
// is a VALUE index, whose maintainer reads a record's entries through
// filteredIndexEntries, as every maintainer does in Java.
func (store *FDBRecordStore) sourceIndexEntryKey(source *Index, record *FDBStoredRecord[proto.Message]) (tuple.Tuple, error) {
	if record == nil {
		return nil, nil
	}
	maintainer, err := store.getIndexMaintainer(source)
	if err != nil {
		return nil, err
	}
	filtered, ok := maintainer.(interface {
		filteredIndexEntries(*FDBStoredRecord[proto.Message]) ([]indexEntry, error)
	})
	if !ok {
		return nil, &RecordCoreError{Message: "index cannot be used as source index", IndexName: source.Name}
	}
	entries, err := filtered.filteredIndexEntries(record)
	if err != nil {
		return nil, err
	}
	switch len(entries) {
	case 0:
		// A maintenance filter or the source's predicate can leave a record
		// with no entry even where the key expression yields one.
		return nil, nil
	case 1:
		return indexEntryKey(source, entries[0].key, record.PrimaryKey)
	default:
		return nil, &RecordCoreError{Message: "index produced incorrect number of entries for use as source index", IndexName: source.Name}
	}
}
