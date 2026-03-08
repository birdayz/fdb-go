package recordlayer

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// Index type constants matching Java's IndexTypes.
const (
	IndexTypeValue = "value"
)

// Index option keys matching Java's IndexOptions.
const (
	IndexOptionUnique = "unique"
)

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

// SetUnique marks this index as enforcing uniqueness.
func (idx *Index) SetUnique() *Index {
	idx.Options[IndexOptionUnique] = "true"
	return idx
}

// indexEntryKey builds the FDB tuple for an index entry.
// Format: (indexedValues..., primaryKeyValues...).
// Matches Java's FDBRecordStoreBase.indexEntryKey() — for the simple case
// where no primary key component positions are set (no trimming).
func indexEntryKey(indexValues tuple.Tuple, primaryKey tuple.Tuple) tuple.Tuple {
	entry := make(tuple.Tuple, 0, len(indexValues)+len(primaryKey))
	entry = append(entry, indexValues...)
	entry = append(entry, primaryKey...)
	return entry
}
