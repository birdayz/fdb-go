package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// NewStandardIndexMaintainer is the StandardIndexMaintainer a maintainer
// implemented outside this package embeds, as Java's maintainers extend it.
func NewStandardIndexMaintainer(state IndexMaintainerState) *StandardIndexMaintainer {
	var store IndexStoreContext
	if state.Store != nil {
		store = state.Store
	}
	return newStandardIndexMaintainer(state.Index, state.IndexSubspace, state.Transaction, store)
}

// Index is the maintained index (Java's state.index).
func (m *StandardIndexMaintainer) Index() *Index { return m.index }

// IndexSubspace is the index's primary subspace (Java's state.indexSubspace).
func (m *StandardIndexMaintainer) IndexSubspace() subspace.Subspace { return m.indexSubspace }

// Transaction is the transaction the maintainer writes through (Java's
// state.transaction).
func (m *StandardIndexMaintainer) Transaction() fdb.WritableTransaction { return m.tx }

// Store is the record store the index belongs to (Java's state.store).
func (m *StandardIndexMaintainer) Store() IndexStoreContext { return m.store }

// NewEvaluatedIndexEntry is the entry an index key expression evaluates to
// for one record.
func NewEvaluatedIndexEntry(key, value, primaryKey tuple.Tuple) EvaluatedIndexEntry {
	return EvaluatedIndexEntry{key: key, value: value, primaryKey: primaryKey}
}

// Key is the entry's index key, without the primary key.
func (e EvaluatedIndexEntry) Key() tuple.Tuple { return e.key }

// Value is the entry's value tuple (non-nil for a covering index).
func (e EvaluatedIndexEntry) Value() tuple.Tuple { return e.value }

// PrimaryKey is the record's primary key.
func (e EvaluatedIndexEntry) PrimaryKey() tuple.Tuple { return e.primaryKey }

// NewIndexEntry is Java's IndexEntry(index, key, value, primaryKey): an entry
// whose primary key is known rather than extracted from its key.
func NewIndexEntry(index *Index, key, value, primaryKey tuple.Tuple) *IndexEntry {
	return &IndexEntry{Index: index, Key: key, Value: value, primaryKey: primaryKey}
}

// NewErrorCursor is a cursor whose every OnNext fails with err: a scan that
// cannot start.
func NewErrorCursor[T any](err error) RecordCursor[T] {
	return &errorCursor[T]{err: err}
}

// FilterCursor is Java's RecordCursor.filter: the elements of inner that
// satisfy predicate.
func FilterCursor[T any](inner RecordCursor[T], predicate func(T) bool) RecordCursor[T] {
	return &filterCursor[T]{inner: inner, predicate: predicate}
}
