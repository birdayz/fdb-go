package recordlayer

// The store's vector search entry points and the BY_DISTANCE scan contract
// (Java's VectorIndexScanBounds and VectorIndexScanOptions are core classes).
// The maintainers implementing them live in package vectorindex.

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// VectorDistanceScanRange creates a TupleRange encoding a BY_DISTANCE kNN query.
// This is the Go equivalent of Java's VectorIndexScanBounds. The query vector,
// k, and efSearch are encoded into TupleRange fields so they can be passed
// through the standard ScanIndexByType API.
//
// Usage:
//
//	store.ScanIndexByType(index, IndexScanByDistance,
//	    VectorDistanceScanRange(queryVec, 10, 200),
//	    nil, ForwardScan)
func VectorDistanceScanRange(queryVector []float64, k, efSearch int) TupleRange {
	return TupleRange{
		Low:          tuple.Tuple{vectorcodec.Serialize(queryVector)},
		High:         tuple.Tuple{int64(k), int64(efSearch)},
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeInclusive,
	}
}

// VectorDistanceScanRangeWithPrefix creates a TupleRange encoding a BY_DISTANCE
// kNN query scoped to a specific prefix partition. The prefix identifies which
// independent HNSW graph to search (e.g., a specific group_id value).
//
// Usage:
//
//	store.ScanIndexByType(index, IndexScanByDistance,
//	    VectorDistanceScanRangeWithPrefix(queryVec, 10, 200, tuple.Tuple{int64(42)}),
//	    nil, ForwardScan)
func VectorDistanceScanRangeWithPrefix(queryVector []float64, k, efSearch int, prefix tuple.Tuple) TupleRange {
	low := tuple.Tuple{vectorcodec.Serialize(queryVector)}
	for _, elem := range prefix {
		low = append(low, elem)
	}
	return TupleRange{
		Low:          low,
		High:         tuple.Tuple{int64(k), int64(efSearch)},
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeInclusive,
	}
}

// VectorDistanceScanRangeOrdered builds a BY_DISTANCE scan range for the RFC-156
// Phase B distance-ORDERED stream. It DECOUPLES the re-rank budget from the
// probe width: cRerank (the re-rank budget c) rides the High-tuple's c slot
// (index 3 of the SPFresh (k, kc, w, c, ε) contract), while efSearch stays the
// index's TUNED probe width (SPFresh kc=64, HNSW ef) rather than being forced up
// to the horizon. Threading c directly avoids the efSearch>0 path's 4×k re-rank
// inflation and kc override (rejected in Phase B review). The
// intermediate w slot is 0 ("use the index default fine-probe width"). HNSW
// reads only (k, efSearch) and ignores the extra slots.
//
// Who reads cRerank: ONLY the legacy self-limiting one-shot SPFresh path
// (ScanByDistance → searchCurrentGeneration, where c bounds the finalize
// re-rank). The STREAMING ordered path (IndexScanByDistanceOrderedStream →
// newOrderedStreamCursor) does NOT read slot 3 at all — its re-rank/candidate cap
// is the demand-driven stream budget (spfreshDefaultStreamCandidateBudget=4000 in
// defaultSPFreshStreamBudget), NOT this High-tuple slot. So cRerank/slot-3 is
// currently UNUSED by the streaming path; it is honoured only when this range
// feeds the one-shot reader, and is otherwise inert (carried for shape symmetry).
// Kept in the signature so the one-shot path stays expressible without a second
// builder; do not remove it just because the streaming path ignores it.
func VectorDistanceScanRangeOrdered(queryVector []float64, k, efSearch, cRerank int, prefix tuple.Tuple) TupleRange {
	low := tuple.Tuple{vectorcodec.Serialize(queryVector)}
	for _, elem := range prefix {
		low = append(low, elem)
	}
	return TupleRange{
		Low:          low,
		High:         tuple.Tuple{int64(k), int64(efSearch), int64(0), int64(cRerank)},
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeInclusive,
	}
}

// VectorIndexSearcher is the kNN surface of a VECTOR index maintainer, which
// package vectorindex implements; the store's vector search entry points reach
// it through any decorator (IndexMaintainerAs).
type VectorIndexSearcher interface {
	// SearchKNN is the k nearest neighbours of queryVector in the prefix's
	// partition, closest first, read at snapshot isolation.
	SearchKNN(prefix tuple.Tuple, queryVector []float64, k, efSearch int) ([]VectorSearchResult, error)
	// ScanVectorIndex is the BY_DISTANCE scan of the prefix's partition with
	// the full per-scan options.
	ScanVectorIndex(prefix tuple.Tuple, queryVector []float64, k int, opts VectorIndexScanOptions, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry]
}

// VectorSearchResult is a single result from a vector similarity search.
type VectorSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
}

// ScanVectorIndex scans a VECTOR index with BY_DISTANCE semantics, returning
// results as a cursor. This is the cursor-based API matching Java's
// VectorIndexMaintainer.scan(VectorIndexScanBounds, ...).
//
// Each result is an IndexEntry with Key = primaryKey and Value = tuple{distance}.
// Results are sorted by distance (closest first).
//
// For prefix-partitioned indexes, use ScanVectorIndexWithPrefix instead.
func (store *FDBRecordStore) ScanVectorIndex(
	index *Index,
	queryVector []float64,
	k int,
	efSearch int,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return store.ScanVectorIndexWithPrefix(index, nil, queryVector, k, efSearch, continuation, scanProperties)
}

// ScanVectorIndexWithPrefix scans a VECTOR index with BY_DISTANCE semantics,
// scoped to a specific prefix partition. Pass nil prefix for non-grouped indexes.
//
// Each result is an IndexEntry with Key = primaryKey and Value = tuple{distance}.
// Results are sorted by distance (closest first).
func (store *FDBRecordStore) ScanVectorIndexWithPrefix(
	index *Index,
	prefix tuple.Tuple,
	queryVector []float64,
	k int,
	efSearch int,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return store.ScanVectorIndexWithOptions(index, prefix, queryVector, k, VectorIndexScanOptions{EfSearch: positiveEfSearch(efSearch)}, continuation, scanProperties)
}

// HNSWEfSearch is HnswVectorIndexEngine.efSearch: the scan's option as given
// (even below k, or 0), else derived from the scan limit.
func HNSWEfSearch(option *int, k int) int {
	if option != nil {
		return *option
	}
	return min(max(4*k, 64), max(k, 400))
}

// positiveEfSearch adapts the int-valued Go entry points, where 0 means "not
// set", to the typed option.
func positiveEfSearch(efSearch int) *int {
	if efSearch <= 0 {
		return nil
	}
	return &efSearch
}

// VectorIndexScanOptions is Java's VectorIndexScanOptions: per-scan search
// knobs. A nil GuardiANN field keeps the SearchConfig default; each applies
// only to an index of its engine.
type VectorIndexScanOptions struct {
	// wirePresence retains explicit NULL options read from Java.
	// The map is immutable after decoding; struct copies may safely share it.
	wirePresence                            map[string]bool
	ReturnVectors                           *bool // nil defaults to !useRaBitQ
	EfSearch                                *int  // HNSW; nil derives it from k
	GuardiannCandidatePoolFactor            *float64
	GuardiannSearchMaxClusters              *int
	GuardiannSearchMinClustersBeforePruning *int
	GuardiannSearchDistanceRatioCutoff      *float64
	GuardiannCentroidEfRingSearch           *int
	GuardiannCentroidEfOutwardSearch        *int
	GuardiannSearchConcurrency              *int
}

// ScanVectorIndexWithOptions is ScanVectorIndexWithPrefix with the full set of
// per-scan options.
func (store *FDBRecordStore) ScanVectorIndexWithOptions(
	index *Index,
	prefix tuple.Tuple,
	queryVector []float64,
	k int,
	opts VectorIndexScanOptions,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	if !state.IsScannable() {
		return &errorCursor[*IndexEntry]{
			err: &IndexNotReadableError{IndexName: index.Name, CurrentState: state},
		}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	// Peel any decorator (e.g. a sliding window) before asking for the concrete
	// vector maintainer: a windowed vector index is still a vector index, and
	// asserting on the outermost type would answer "not a VECTOR index".
	vm, ok := IndexMaintainerAs[VectorIndexSearcher](maintainer)
	if !ok {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("index %q (type %s) is not a VECTOR index", index.Name, index.Type),
		}
	}
	return vm.ScanVectorIndex(prefix, queryVector, k, opts, continuation, scanProperties)
}

// SearchVectorIndex performs a k-nearest-neighbor search on a VECTOR index.
// Matches Java's VectorIndexMaintainer scan with VectorIndexScanBounds.
//
// For prefix-partitioned indexes, use SearchVectorIndexWithPrefix instead.
func (store *FDBRecordStore) SearchVectorIndex(
	index *Index,
	queryVector []float64,
	k int,
	efSearch int,
) ([]VectorSearchResult, error) {
	return store.SearchVectorIndexWithPrefix(index, nil, queryVector, k, efSearch)
}

// SearchVectorIndexWithPrefix performs a k-nearest-neighbor search on a VECTOR
// index, scoped to a specific prefix partition. Pass nil prefix for non-grouped indexes.
func (store *FDBRecordStore) SearchVectorIndexWithPrefix(
	index *Index,
	prefix tuple.Tuple,
	queryVector []float64,
	k int,
	efSearch int,
) ([]VectorSearchResult, error) {
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return nil, err
	}
	if !state.IsScannable() {
		return nil, &IndexNotReadableError{IndexName: index.Name, CurrentState: state}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	// Peel any decorator (e.g. a sliding window) before asking for the concrete
	// vector maintainer: a windowed vector index is still a vector index, and
	// asserting on the outermost type would answer "not a VECTOR index".
	vm, ok := IndexMaintainerAs[VectorIndexSearcher](maintainer)
	if !ok {
		return nil, fmt.Errorf("index %q (type %s) is not a VECTOR index", index.Name, index.Type)
	}
	return vm.SearchKNN(prefix, queryVector, k, efSearch)
}

// SearchVectorIndexRecords performs a kNN search and fetches the corresponding records.
//
// For prefix-partitioned indexes, use SearchVectorIndexRecordsWithPrefix instead.
func (store *FDBRecordStore) SearchVectorIndexRecords(
	ctx context.Context,
	index *Index,
	queryVector []float64,
	k int,
	efSearch int,
) ([]*FDBIndexedRecord, error) {
	return store.SearchVectorIndexRecordsWithPrefix(ctx, index, nil, queryVector, k, efSearch)
}

// SearchVectorIndexRecordsWithPrefix performs a kNN search scoped to a prefix
// partition and fetches the corresponding records.
func (store *FDBRecordStore) SearchVectorIndexRecordsWithPrefix(
	ctx context.Context,
	index *Index,
	prefix tuple.Tuple,
	queryVector []float64,
	k int,
	efSearch int,
) ([]*FDBIndexedRecord, error) {
	results, err := store.SearchVectorIndexWithPrefix(index, prefix, queryVector, k, efSearch)
	if err != nil {
		return nil, err
	}

	records := make([]*FDBIndexedRecord, 0, len(results))
	for _, r := range results {
		rec, err := store.LoadRecord(r.PrimaryKey)
		if err != nil {
			return nil, fmt.Errorf("search vector index records: load PK %v: %w", r.PrimaryKey, err)
		}
		if rec == nil {
			continue // record deleted between search and load, skip
		}
		records = append(records, &FDBIndexedRecord{
			IndexEntry: &IndexEntry{
				Index: index,
				Key:   r.PrimaryKey,
			},
			Record: rec,
		})
	}
	return records, nil
}
