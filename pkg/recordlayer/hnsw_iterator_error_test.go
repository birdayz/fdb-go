package recordlayer

import (
	"errors"
	"fmt"
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

// fakeRangeIteratorSeq returns nValid empty KVs (Advance()==true, Get()==nil) and
// THEN Advance()==false with Get()==getErr — modeling a transient error landing
// PART-WAY through a scan (rows already processed), so the post-loop Get() check must
// still surface it. (The position-0 fake only proves the check fires on an empty scan.)
type fakeRangeIteratorSeq struct {
	remaining int
	getErr    error
	onValid   bool
}

func (f *fakeRangeIteratorSeq) Advance() bool {
	if f.remaining > 0 {
		f.remaining--
		f.onValid = true
		return true
	}
	f.onValid = false
	return false
}

func (f *fakeRangeIteratorSeq) Get() (fdb.KeyValue, error) {
	if f.onValid {
		return fdb.KeyValue{}, nil
	}
	return fdb.KeyValue{}, f.getErr
}

func TestHNSW_PreloadLayer_SurfacesIteratorErrorAfterRows(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007) mid-scan")
	s := &hnswStorage{
		dataSubspace: subspace.Sub("hnsw_test").Sub(int64(0)),
		cache:        make(map[string]*parsedNode),
		scan: func(_ fdb.ReadTransaction, _ fdb.Range, _ fdb.RangeOptions) rangeIterator {
			return &fakeRangeIteratorSeq{remaining: 2, getErr: injErr}
		},
	}
	// The loop runs 2 iterations (empty KVs → skipped on parse), then Advance()==false
	// with a stored error; the post-loop check must surface it, not return nil.
	if err := s.preloadLayer(nil, 0); !errors.Is(err, injErr) {
		t.Fatalf("preloadLayer must surface an error that lands AFTER some rows, got %v", err)
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

// TestHNSWFatal pins the not-found-vs-fatal classification that the graph callers use:
// only a genuine errHNSWNotPresent "absent" result is skippable; any other (transient)
// error must propagate so the transaction aborts and retries.
func TestHNSWFatal(t *testing.T) {
	t.Parallel()
	if hnswFatal(nil) != nil {
		t.Fatal("nil is not fatal")
	}
	notFound := fmt.Errorf("hnsw: node not found at layer %d: %w", 3, errHNSWNotPresent)
	if hnswFatal(notFound) != nil {
		t.Fatal("a genuine absent node (errHNSWNotPresent) must be skippable, not fatal")
	}
	transient := errors.New("transaction_too_old (1007)")
	if got := hnswFatal(transient); got != transient {
		t.Fatalf("a transient error must be returned as fatal, got %v", got)
	}
}

// TestHNSW_DeleteRepair_PropagatesScanError is the operation-level proof of codex's
// P1: a transient scan error reaching a graph caller must ABORT the operation (so the
// tx retries), NOT be read as "neighbor doesn't exist" and skipped (which would commit a
// partially-repaired graph). The delete-repair candidate gather (findDeletionRepairCandidates)
// loads the deleted node's neighbors via loadNodeLayerInlining (inlining layer), whose scan
// we fail.
func TestHNSW_DeleteRepair_PropagatesScanError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := &hnswStorage{
		dataSubspace: subspace.Sub("hnsw_test").Sub(int64(0)),
		cache:        make(map[string]*parsedNode),
		config:       HNSWConfig{UseInlining: true, EfRepair: 64, M: 16},
		scan: func(_ fdb.ReadTransaction, _ fdb.Range, _ fdb.RangeOptions) rangeIterator {
			return &fakeRangeIterator{getErr: injErr}
		},
	}
	g := &hnswGraph{storage: s, config: s.config}
	deletedPK := tuple.Tuple{int64(2)}
	primary := [][]byte{nestPK(tuple.Tuple{int64(1)})}
	_, err := g.findDeletionRepairCandidates(fdb.Transaction{}, 1, deletedPK, primary, newSplittableRandomForKey(deletedPK))
	if !errors.Is(err, injErr) {
		t.Fatalf("delete repair must propagate a transient scan error (not skip as absent), got %v", err)
	}
}

// TestHNSW_DeleteRepair_SkipsAbsentNeighbor is the companion: a genuinely-absent
// neighbor (clean empty scan → errHNSWNotPresent) is still skipped (returns nil, empty
// candidate set), so the fix doesn't turn a normal "neighbor already gone" into a failure.
func TestHNSW_DeleteRepair_SkipsAbsentNeighbor(t *testing.T) {
	t.Parallel()
	s := &hnswStorage{
		dataSubspace: subspace.Sub("hnsw_test").Sub(int64(0)),
		cache:        make(map[string]*parsedNode),
		config:       HNSWConfig{UseInlining: true, EfRepair: 64, M: 16},
		scan: func(_ fdb.ReadTransaction, _ fdb.Range, _ fdb.RangeOptions) rangeIterator {
			return &fakeRangeIterator{} // Advance()==false, Get()==(zero, nil): clean empty
		},
	}
	g := &hnswGraph{storage: s, config: s.config}
	deletedPK := tuple.Tuple{int64(2)}
	primary := [][]byte{nestPK(tuple.Tuple{int64(1)})}
	cands, err := g.findDeletionRepairCandidates(fdb.Transaction{}, 1, deletedPK, primary, newSplittableRandomForKey(deletedPK))
	if err != nil {
		t.Fatalf("delete repair must skip a genuinely-absent neighbor (nil), got %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("absent neighbor should yield zero candidates, got %d", len(cands))
	}
}

// TestHNSW_Delete_PropagatesAccessInfoReadError pins the point-read swallow class
// Torvalds caught beyond the dispatch sites: Delete's first read (access info), its
// pipelined per-layer reads, and the layer-0 existence probe used to read EVERY error as
// "empty graph / already deleted" and return nil — so a transient transaction_too_old
// (1007) made Delete commit a silent no-op instead of retrying. The `get` seam injects
// the point-read failure (Delete's first read is access info, so this gates it).
func TestHNSW_Delete_PropagatesAccessInfoReadError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	s := &hnswStorage{
		dataSubspace:   subspace.Sub("hnsw_test").Sub(int64(0)),
		accessSubspace: subspace.Sub("hnsw_test").Sub(int64(1)),
		cache:          make(map[string]*parsedNode),
		get: func(_ fdb.ReadTransaction, _ fdb.Key) ([]byte, error) {
			return nil, injErr
		},
	}
	g := &hnswGraph{storage: s, config: s.config}
	err := g.Delete(fdb.Transaction{}, tuple.Tuple{int64(1)})
	if !errors.Is(err, injErr) {
		t.Fatalf("Delete must propagate a transient access-info read error (not commit a no-op), got %v", err)
	}
}

// TestHNSW_Delete_SkipsEmptyGraph is the companion: a genuinely empty graph (no access
// info → errHNSWNotPresent) is still a clean no-op delete (nil), not a spurious failure.
func TestHNSW_Delete_SkipsEmptyGraph(t *testing.T) {
	t.Parallel()
	s := &hnswStorage{
		dataSubspace:   subspace.Sub("hnsw_test").Sub(int64(0)),
		accessSubspace: subspace.Sub("hnsw_test").Sub(int64(1)),
		cache:          make(map[string]*parsedNode),
		get: func(_ fdb.ReadTransaction, _ fdb.Key) ([]byte, error) {
			return nil, nil // no access info written → empty graph
		},
	}
	g := &hnswGraph{storage: s, config: s.config}
	if err := g.Delete(fdb.Transaction{}, tuple.Tuple{int64(1)}); err != nil {
		t.Fatalf("Delete on a genuinely empty graph must be a clean no-op (nil), got %v", err)
	}
}
