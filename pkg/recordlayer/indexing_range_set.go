package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
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
func (irs *IndexingRangeSet) FirstMissingRange(tr fdb.WritableTransaction) (*RangeSetRange, error) {
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
func (irs *IndexingRangeSet) ContainsKey(tr fdb.WritableTransaction, key []byte) (bool, error) {
	return irs.rangeSet.Contains(tr, key)
}

// InsertRange marks a range as built. Returns true if the database was modified.
func (irs *IndexingRangeSet) InsertRange(tr fdb.WritableTransaction, begin, end []byte, requireEmpty bool) (bool, error) {
	return irs.rangeSet.InsertRange(tr, begin, end, requireEmpty)
}

// ListMissingRanges returns all gaps in the range set.
func (irs *IndexingRangeSet) ListMissingRanges(tr fdb.WritableTransaction) ([]RangeSetRange, error) {
	return irs.rangeSet.MissingRanges(tr, nil, nil, 0)
}

// ListMissingRangesInBytes returns missing ranges within the given byte boundaries.
// Used by mutual indexing to check missing ranges within a fragment.
func (irs *IndexingRangeSet) ListMissingRangesInBytes(tr fdb.WritableTransaction, begin, end []byte) ([]RangeSetRange, error) {
	return irs.rangeSet.MissingRanges(tr, begin, end, 0)
}

// IsComplete returns true if all ranges have been built.
func (irs *IndexingRangeSet) IsComplete(tr fdb.WritableTransaction) (bool, error) {
	first, err := irs.FirstMissingRange(tr)
	if err != nil {
		return false, err
	}
	return first == nil, nil
}

// Clear removes all range tracking data.
func (irs *IndexingRangeSet) Clear(tr fdb.WritableTransaction) {
	irs.rangeSet.Clear(tr)
}
