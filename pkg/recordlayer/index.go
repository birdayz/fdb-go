package recordlayer

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Index type constants matching Java's IndexTypes.
const (
	IndexTypeValue        = "value"
	IndexTypeCount        = "count"
	IndexTypeCountNotNull = "count_not_null"
	IndexTypeCountUpdates = "count_updates"
	IndexTypeSum          = "sum"
	IndexTypeMaxEverLong  = "max_ever_long"
	IndexTypeMinEverLong  = "min_ever_long"
)

// Index option keys matching Java's IndexOptions.
const (
	IndexOptionUnique        = "unique"
	IndexOptionClearWhenZero = "clearWhenZero"
)

// IndexPredicate is a function that determines whether a record should be indexed.
// Return true to include the record in the index, false to skip it.
// Matches Java's Index predicate concept for sparse/filtered indexes.
type IndexPredicate func(msg proto.Message) bool

// Index represents a secondary index definition.
// Matches Java's com.apple.foundationdb.record.metadata.Index.
type Index struct {
	Name           string
	Type           string
	RootExpression KeyExpression
	subspaceKey    interface{}
	Options        map[string]string
	AddedVersion   int
	LastModifiedVersion int

	// Predicate filters which records are included in this index.
	// If nil, all records are indexed. If set, only records where
	// Predicate returns true are indexed (sparse/filtered index).
	Predicate IndexPredicate

	// primaryKeyComponentPositions tracks overlap between index key and primary key.
	// Each element corresponds to a primary key component:
	//   >= 0: the component already appears at that position in the index key (deduplicated)
	//   < 0:  the component is NOT in the index key (appended to the entry)
	// nil means no overlap (all PK components are appended as-is).
	// Matches Java's Index.primaryKeyComponentPositions.
	primaryKeyComponentPositions []int
}

// NewIndex creates a VALUE index with the given name and root key expression.
// Matches Java's new Index(name, rootExpression) which defaults to IndexTypes.VALUE.
func NewIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeValue,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewCountIndex creates a COUNT index with the given name and root key expression.
// COUNT indexes use FDB atomic ADD to maintain counts per grouping key.
// Matches Java's new Index(name, rootExpression, IndexTypes.COUNT).
func NewCountIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeCount,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewSumIndex creates a SUM index with the given name and root key expression.
// SUM indexes use FDB atomic ADD to maintain running sums per grouping key.
// The expression must include at least one grouped (aggregated) column.
// Matches Java's new Index(name, rootExpression, IndexTypes.SUM).
func NewSumIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeSum,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMaxEverLongIndex creates a MAX_EVER_LONG index with the given name and root key expression.
// MAX_EVER_LONG indexes use FDB atomic MAX to track the maximum value seen per grouping key.
// Values must be non-negative (unsigned comparison). Deletes are no-ops (_EVER = irreversible).
// Matches Java's new Index(name, rootExpression, IndexTypes.MAX_EVER_LONG).
func NewMaxEverLongIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMaxEverLong,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewMinEverLongIndex creates a MIN_EVER_LONG index with the given name and root key expression.
// MIN_EVER_LONG indexes use FDB atomic MIN to track the minimum value seen per grouping key.
// Values must be non-negative (unsigned comparison). Deletes are no-ops (_EVER = irreversible).
// Matches Java's new Index(name, rootExpression, IndexTypes.MIN_EVER_LONG).
func NewMinEverLongIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeMinEverLong,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewCountNotNullIndex creates a COUNT_NOT_NULL index with the given name and root key expression.
// Like COUNT, but skips entries where the key contains a null value (nil element).
// Matches Java's new Index(name, rootExpression, IndexTypes.COUNT_NOT_NULL).
func NewCountNotNullIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeCountNotNull,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// NewCountUpdatesIndex creates a COUNT_UPDATES index with the given name and root key expression.
// Like COUNT, but deletes are no-ops (count never decrements) and updates always re-count
// (skipUpdateForUnchangedKeys = false). Tracks total insert+update events.
// Matches Java's new Index(name, rootExpression, IndexTypes.COUNT_UPDATES).
func NewCountUpdatesIndex(name string, rootExpression KeyExpression) *Index {
	return &Index{
		Name:           name,
		Type:           IndexTypeCountUpdates,
		RootExpression: rootExpression,
		subspaceKey:    name,
		Options:        make(map[string]string),
	}
}

// SubspaceTupleKey returns the key used to identify this index's subspace within
// the IndexKey (2) subspace. Defaults to the index name.
// Matches Java's Index.getSubspaceTupleKey().
func (idx *Index) SubspaceTupleKey() interface{} {
	return idx.subspaceKey
}

// SetSubspaceKey overrides the default subspace key (index name).
// Matches Java's Index.setSubspaceKey().
func (idx *Index) SetSubspaceKey(key interface{}) *Index {
	idx.subspaceKey = key
	return idx
}

// IsUnique returns whether this index enforces a uniqueness constraint.
// Matches Java's Index.isUnique() which checks IndexOptions.UNIQUE_OPTION.
func (idx *Index) IsUnique() bool {
	v, ok := idx.Options[IndexOptionUnique]
	return ok && v == "true"
}

// IsClearWhenZero returns whether this index should clear entries when values reach zero.
// When true, atomic ADD mutations are followed by CompareAndClear(zero) to remove
// stale zero-value entries. Applies to COUNT, COUNT_NOT_NULL, and SUM indexes.
// Matches Java's IndexOptions.CLEAR_WHEN_ZERO.
func (idx *Index) IsClearWhenZero() bool {
	v, ok := idx.Options[IndexOptionClearWhenZero]
	return ok && v == "true"
}

// SetClearWhenZero enables or disables the clear-when-zero behavior.
// Matches Java's IndexOptions.CLEAR_WHEN_ZERO.
func (idx *Index) SetClearWhenZero(clear bool) *Index {
	if clear {
		idx.Options[IndexOptionClearWhenZero] = "true"
	} else {
		delete(idx.Options, IndexOptionClearWhenZero)
	}
	return idx
}

// SetPredicate sets a filter predicate for sparse/filtered indexes.
// Only records where the predicate returns true will have index entries.
func (idx *Index) SetPredicate(p IndexPredicate) *Index {
	idx.Predicate = p
	return idx
}

// SetUnique marks this index as enforcing uniqueness.
func (idx *Index) SetUnique() *Index {
	idx.Options[IndexOptionUnique] = "true"
	return idx
}

// indexEntryKey builds the FDB tuple for an index entry.
// Format: (indexedValues..., trimmedPrimaryKeyValues...).
// When the index has primaryKeyComponentPositions, PK components that already
// appear in the index key are omitted (deduplicated). This matches Java's
// FDBRecordStoreBase.indexEntryKey() which calls Index.trimPrimaryKey().
func indexEntryKey(idx *Index, indexValues tuple.Tuple, primaryKey tuple.Tuple) tuple.Tuple {
	trimmed := idx.trimPrimaryKey(primaryKey)
	entry := make(tuple.Tuple, 0, len(indexValues)+len(trimmed))
	entry = append(entry, indexValues...)
	entry = append(entry, trimmed...)
	return entry
}

// trimPrimaryKey removes PK components that already appear in the index key.
// Returns the remaining PK components that need to be appended to the index entry.
// Matches Java's Index.trimPrimaryKey().
func (idx *Index) trimPrimaryKey(primaryKey tuple.Tuple) tuple.Tuple {
	if idx.primaryKeyComponentPositions == nil {
		return primaryKey
	}
	trimmed := make(tuple.Tuple, 0, len(primaryKey))
	for i, pos := range idx.primaryKeyComponentPositions {
		if pos < 0 && i < len(primaryKey) {
			trimmed = append(trimmed, primaryKey[i])
		}
	}
	return trimmed
}

// getEntryPrimaryKey reconstructs the full primary key from an index entry key.
// When primaryKeyComponentPositions is set, some PK components come from the
// index key portion and some from the appended portion.
// Matches Java's Index.getEntryPrimaryKey().
func (idx *Index) getEntryPrimaryKey(entryKey tuple.Tuple) tuple.Tuple {
	colSize := keyExpressionColumnSize(idx.RootExpression)
	if idx.primaryKeyComponentPositions == nil {
		if colSize < len(entryKey) {
			return entryKey[colSize:]
		}
		return tuple.Tuple{}
	}

	pk := make(tuple.Tuple, len(idx.primaryKeyComponentPositions))
	after := colSize
	for i, pos := range idx.primaryKeyComponentPositions {
		if pos >= 0 && pos < len(entryKey) {
			pk[i] = entryKey[pos]
		} else if after < len(entryKey) {
			pk[i] = entryKey[after]
			after++
		}
	}
	return pk
}
