package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// indexTypeValueWithQueue is Java's test-only ValueIndexMaintainerWithQueue
// type: an ordinary value index that also supports the pending write queue.
// Core tests exercise the queue through it; the vector index, the production
// queue-capable type, lives in another package.
const indexTypeValueWithQueue = "value_with_queue"

func init() { RegisterIndexMaintainerFactory(valueWithQueueFactory{}) }

// valueWithQueueFactory is ValueIndexMaintainerWithQueue.Factory; validation
// is the value index's.
type valueWithQueueFactory struct{}

func (valueWithQueueFactory) IndexTypes() []string { return []string{indexTypeValueWithQueue} }

func (valueWithQueueFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	return &standardIndexMaintainerWithQueue{
		StandardIndexMaintainer: *newStandardIndexMaintainer(state.Index, state.IndexSubspace, state.Transaction, state.Store),
	}, nil
}

func (valueWithQueueFactory) ValidateIndexOptions(*Index) error { return nil }

func (valueWithQueueFactory) ValidateChangedOptions(_, _ *Index, _ map[string]bool) error {
	return nil
}

func (valueWithQueueFactory) IsIdempotent(*Index) bool { return true }

// newValueWithQueueIndex is a value_with_queue index over root.
func newValueWithQueueIndex(name string, root KeyExpression) *Index {
	idx := NewIndex(name, root)
	idx.Type = indexTypeValueWithQueue
	return idx
}

// queuedIndexPrimaryKeys is the primary keys of a value_with_queue index's
// entries under prefix, in index order, read through the maintainer whatever
// the index's state: what maintenance and replay have written.
func queuedIndexPrimaryKeys(store *FDBRecordStore, index *Index, prefix tuple.Tuple) ([]tuple.Tuple, error) {
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	entries, err := AsList(context.Background(), maintainer.Scan(TupleRangeAllOf(prefix), nil, ForwardScan()))
	if err != nil {
		return nil, err
	}
	pks := make([]tuple.Tuple, len(entries))
	for i, e := range entries {
		pks[i] = e.PrimaryKey()
	}
	return pks, nil
}
