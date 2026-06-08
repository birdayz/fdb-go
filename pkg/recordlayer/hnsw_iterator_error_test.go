package recordlayer

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// These pin the iterator-error sweep for the HNSW (vector-index) layer scans. Like
// the other cursors, hnsw's range scans end on iterator.Advance()==false, which the
// underlying *fdb.RangeIterator returns on BOTH clean exhaustion AND a transient FDB
// error (transaction_too_old 1007, timeout). The for-loop scans previously checked
// Get() only INSIDE the loop body, so a mid-scan error that ended the loop was
// swallowed → a silently PARTIAL layer cache / neighbor list (a corrupt-graph hazard
// that still commits). The Limit:1 "find any node" probes reported the misleading
// "no nodes at layer" on a transient error. Each now surfaces the stored Get() error.
//
// The hnswStorage.scan seam (nil in production) lets these tests inject a fake iterator
// that fails at the boundary, so the error paths are deterministic without a live FDB.
func hnswStorageWithFailingScan(injErr error) *hnswStorage {
	return &hnswStorage{
		dataSubspace: subspace.Sub("hnsw_test").Sub(int64(0)),
		cache:        make(map[string]*parsedNode),
		scan: func(_ fdb.ReadTransaction, _ fdb.Range, _ fdb.RangeOptions) rangeIterator {
			return &fakeRangeIterator{getErr: injErr}
		},
	}
}

func TestHNSW_PreloadLayer_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := hnswStorageWithFailingScan(injErr)
	if err := s.preloadLayer(nil, 0); !errors.Is(err, injErr) {
		t.Fatalf("preloadLayer must surface a mid-scan error (else a silently partial cache), got %v", err)
	}
}

func TestHNSW_PreloadLayerInlining_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := hnswStorageWithFailingScan(injErr)
	if err := s.preloadLayerInlining(nil, 0); !errors.Is(err, injErr) {
		t.Fatalf("preloadLayerInlining must surface a mid-scan error, got %v", err)
	}
}

func TestHNSW_LoadNodeLayerInlining_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := hnswStorageWithFailingScan(injErr)
	_, _, err := s.loadNodeLayerInlining(nil, 0, tuple.Tuple{int64(1)})
	if !errors.Is(err, injErr) {
		t.Fatalf("loadNodeLayerInlining must surface a mid-scan error (else a partial neighbor list), got %v", err)
	}
}

func TestHNSW_FindAnyNodeAtLayer_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := hnswStorageWithFailingScan(injErr)
	_, _, err := s.findAnyNodeAtLayer(nil, 0)
	if !errors.Is(err, injErr) {
		t.Fatalf("findAnyNodeAtLayer must surface the error, not report 'no nodes at layer', got %v", err)
	}
}

func TestHNSW_FindAnyNodeAtLayerInlining_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := hnswStorageWithFailingScan(injErr)
	_, _, err := s.findAnyNodeAtLayerInlining(nil, 0)
	if !errors.Is(err, injErr) {
		t.Fatalf("findAnyNodeAtLayerInlining must surface the error, not 'no nodes at layer', got %v", err)
	}
}
