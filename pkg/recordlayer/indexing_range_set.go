package recordlayer

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
)

// IndexingRangeSet wraps a RangeSet scoped to a specific index's build progress.
// Lives at store subspace [IndexRangeSpaceKey (6), indexSubspaceKey].
// Wire-compatible with Java's IndexingRangeSet.
type IndexingRangeSet struct {
	rangeSet *RangeSet
}

// NewIndexingRangeSet creates an IndexingRangeSet for the given index within the store's subspace.
// Matches Java's IndexingRangeSet.forIndexBuild(store, index).
func NewIndexingRangeSet(storeSubspace subspace.Subspace, index *Index) *IndexingRangeSet {
	ss := storeSubspace.Sub(IndexRangeSpaceKey, index.SubspaceTupleKey())
	return &IndexingRangeSet{
		rangeSet: NewRangeSet(ss),
	}
}

// FirstMissingRange returns the first gap in the range set, or nil if complete.
func (irs *IndexingRangeSet) FirstMissingRange(tr fdb.Transaction) (*RangeSetRange, error) {
	ranges, err := irs.rangeSet.MissingRanges(tr, nil, nil, 1)
	if err != nil {
		return nil, err
	}
	if len(ranges) == 0 {
		return nil, nil
	}
	return &ranges[0], nil
}

// ContainsKey checks if a primary key (tuple-packed bytes) is in the built range.
func (irs *IndexingRangeSet) ContainsKey(tr fdb.Transaction, key []byte) (bool, error) {
	return irs.rangeSet.Contains(tr, key)
}

// InsertRange marks a range as built. Returns true if the database was modified.
func (irs *IndexingRangeSet) InsertRange(tr fdb.Transaction, begin, end []byte, requireEmpty bool) (bool, error) {
	return irs.rangeSet.InsertRange(tr, begin, end, requireEmpty)
}

// ListMissingRanges returns all gaps in the range set.
func (irs *IndexingRangeSet) ListMissingRanges(tr fdb.Transaction) ([]RangeSetRange, error) {
	return irs.rangeSet.MissingRanges(tr, nil, nil, 0)
}

// IsComplete returns true if all ranges have been built.
func (irs *IndexingRangeSet) IsComplete(tr fdb.Transaction) (bool, error) {
	first, err := irs.FirstMissingRange(tr)
	if err != nil {
		return false, err
	}
	return first == nil, nil
}

// Clear removes all range tracking data.
func (irs *IndexingRangeSet) Clear(tr fdb.Transaction) {
	irs.rangeSet.Clear(tr)
}
