// Portions derived from FoundationDB Record Layer (InliningStorageAdapter.java,
// Insert.java, NodeReferenceWithVector.java, Config.java, and others),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"fmt"

	"fdb.dev/internal/recordlayer/tuplefast"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// parsedNode holds pre-parsed node data to avoid repeated tuple.Unpack on cache hits.
type parsedNode struct {
	vecBytes  []byte
	neighbors [][]byte // neighbor PKs as nested-encoded spans (Tuple{pk}.Pack())
	// additional is the encoded fourth element of a Java 4.14 compact node
	// (its covering values), carried verbatim so a rewrite never drops it.
	additional []byte
}

// hnswStorage handles FDB storage of HNSW graph nodes.
// Supports two storage formats:
//   - COMPACT (default): (layer, pk) → (nodeKind, vectorTuple, neighborsTuple)
//   - INLINING (layers > 0 when UseInlining=true): (layer, pk, neighborPK) → neighborVector
//
// Wire-compatible with Java's CompactStorageAdapter and InliningStorageAdapter.
//
// Subspace layout:
//
//	Sub(0) — data: compact or inlining KVs
//	Sub(1) — access info: () -> (entryPointLayer, entryPointPK, entryPointVectorTuple)
type hnswStorage struct {
	dataSubspace    subspace.Subspace
	accessSubspace  subspace.Subspace
	samplesSubspace subspace.Subspace // SUBSPACE_PREFIX_SAMPLES (0x02): RaBitQ centroid-bootstrap samples
	config          HNSWConfig
	cache           map[string]*parsedNode // FDB key → parsed node (nil = not found)
	stats           *HNSWStats             // optional I/O counters (nil = no tracking)
	// timer counts node reads and writes on the store timer, as Java's
	// HnswVectorIndexEngine OnRead / OnWrite listeners do (nil = none).
	timer *recordlayer.StoreTimer

	// scan opens a range iterator for a layer scan. nil in production (uses the real
	// tx.GetRange().Iterator() via scanIter); tests set it to inject a fake iterator so
	// the Advance()/Get() error-surfacing paths are deterministically reachable without
	// a live FDB transaction. The local *fdb.RangeIterator and a test fake both satisfy
	// the rangeIterator seam (Advance/Get).
	scan func(tx fdb.ReadTransaction, r fdb.Range, opts fdb.RangeOptions) rangeIterator

	// get reads a single key. nil in production (uses the real tx.Get().Get() via
	// getKey); tests set it to inject a point-read failure or a not-found so the
	// transient-vs-absent handling of the single-key reads (access info, layer-0
	// existence probe) is deterministically reachable without a live FDB transaction.
	get func(tx fdb.ReadTransaction, key fdb.Key) ([]byte, error)

	// env is the simulation environment routing sample-key nonce entropy. Nil
	// means production (crypto/rand); the maintainer sets it from the record
	// context so a simulation run mints reproducible SAMPLES keys. The *recordlayer.Env
	// accessors are nil-safe, so leaving it unset keeps production byte-identical.
	env *recordlayer.Env
}

// scanIter opens a range iterator, honoring the test seam (s.scan) when set and
// otherwise using the real range read.
func (s *hnswStorage) scanIter(tx fdb.ReadTransaction, r fdb.Range, opts fdb.RangeOptions) rangeIterator {
	if s.scan != nil {
		return s.scan(tx, r, opts)
	}
	return tx.GetRange(r, opts).Iterator()
}

// getKey reads a single key, honoring the test seam (s.get) when set.
func (s *hnswStorage) getKey(tx fdb.ReadTransaction, key fdb.Key) ([]byte, error) {
	if s.get != nil {
		return s.get(tx, key)
	}
	return tx.Get(key).Get()
}

// The HNSW node counters of FDBStoreTimer.Counts, which Java's
// HnswVectorIndexEngine OnRead / OnWrite listeners record: nodes read and
// written, and their key-value bytes, split by layer 0 and the layers above.
var (
	countVectorNodeReads       = recordlayer.Event{Name: "vector_node_reads", Title: "intermediate nodes read", Kind: recordlayer.KindCount}
	countVectorNodeReadBytes   = recordlayer.Event{Name: "vector_node_read_bytes", Title: "intermediate node bytes read", Kind: recordlayer.KindSize}
	countVectorNode0Reads      = recordlayer.Event{Name: "vector_node0_reads", Title: "intermediate nodes read", Kind: recordlayer.KindCount}
	countVectorNode0ReadBytes  = recordlayer.Event{Name: "vector_node0_read_bytes", Title: "intermediate node bytes read", Kind: recordlayer.KindSize}
	countVectorNodeWrites      = recordlayer.Event{Name: "vector_node_writes", Title: "intermediate nodes written", Kind: recordlayer.KindCount}
	countVectorNodeWriteBytes  = recordlayer.Event{Name: "vector_node_write_bytes", Title: "intermediate node bytes written", Kind: recordlayer.KindSize}
	countVectorNode0Writes     = recordlayer.Event{Name: "vector_node0_writes", Title: "intermediate nodes written", Kind: recordlayer.KindCount}
	countVectorNode0WriteBytes = recordlayer.Event{Name: "vector_node0_write_bytes", Title: "intermediate node bytes written", Kind: recordlayer.KindSize}
)

// recordKeyValueRead is OnRead.onKeyValueRead: the generic index-load counters
// (VectorIndexInstrumentation.recordKeyValueRead) and the layer's node bytes.
// A missing value reads as zero bytes.
func (s *hnswStorage) recordKeyValueRead(layer int, key, value []byte) {
	if s.timer == nil {
		return
	}
	s.timer.Increment(recordlayer.CountLoadIndexKey)
	s.timer.IncrementBy(recordlayer.CountLoadIndexKeyBytes, int64(len(key)))
	s.timer.IncrementBy(recordlayer.CountLoadIndexValueBytes, int64(len(value)))
	bytes := countVectorNodeReadBytes
	if layer == 0 {
		bytes = countVectorNode0ReadBytes
	}
	s.timer.IncrementBy(bytes, int64(len(key)+len(value)))
}

// recordNodeRead is OnRead.onNodeRead.
func (s *hnswStorage) recordNodeRead(layer int) {
	if layer == 0 {
		s.timer.Increment(countVectorNode0Reads)
	} else {
		s.timer.Increment(countVectorNodeReads)
	}
}

// recordKeyValueWritten is OnWrite.onKeyValueWritten.
func (s *hnswStorage) recordKeyValueWritten(layer int, key, value []byte) {
	if s.timer == nil {
		return
	}
	s.timer.Increment(recordlayer.CountSaveIndexKey)
	s.timer.IncrementBy(recordlayer.CountSaveIndexKeyBytes, int64(len(key)))
	s.timer.IncrementBy(recordlayer.CountSaveIndexValueBytes, int64(len(value)))
	bytes := countVectorNodeWriteBytes
	if layer == 0 {
		bytes = countVectorNode0WriteBytes
	}
	s.timer.IncrementBy(bytes, int64(len(key)+len(value)))
}

// recordNodeWritten is OnWrite.onNodeWritten.
func (s *hnswStorage) recordNodeWritten(layer int) {
	if layer == 0 {
		s.timer.Increment(countVectorNode0Writes)
	} else {
		s.timer.Increment(countVectorNodeWrites)
	}
}

func newHNSWStorage(ss subspace.Subspace, config HNSWConfig) *hnswStorage {
	s := &hnswStorage{
		dataSubspace:    ss.Sub(int64(0)),
		accessSubspace:  ss.Sub(int64(1)),
		samplesSubspace: ss.Sub(int64(2)),
		config:          config,
		cache:           make(map[string]*parsedNode),
	}
	return s
}

// cacheLookup checks the per-transaction node cache. This is a read-your-writes
// memo bounded to the lifetime of one transaction — equivalent to Java's per-
// operation nodeCache (Insert.java) plus FDB's own transaction read-your-writes:
// within a tx, a node written by saveNodeLayer reads back its written value.
func (s *hnswStorage) cacheLookup(key string) (*parsedNode, bool) {
	n, ok := s.cache[key]
	return n, ok
}

// cacheStore records a node read from FDB in the per-transaction cache so repeat
// reads within the same transaction skip the round-trip and the tuple.Unpack.
func (s *hnswStorage) cacheStore(key string, node *parsedNode) {
	s.cache[key] = node
}

// saveNodeLayer writes one layer's data for a node in COMPACT format.
// Key: dataSubspace.Pack(Tuple{layer, primaryKey})  (PK as nested tuple, matching Java)
// Value: Tuple.Pack(nodeKind, vectorTuple, neighborsTuple)
func (s *hnswStorage) saveNodeLayer(tx fdb.WritableTransaction, layer int, primaryKey tuple.Tuple, vectorBytes []byte, neighbors []tuple.Tuple) {
	// Java uses Tuple.from(layer, primaryKey) where primaryKey is nested.
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})

	neighborList := make(tuple.Tuple, len(neighbors))
	for i, pk := range neighbors {
		neighborList[i] = pk
	}

	value := tuple.Tuple{
		int64(0),                 // COMPACT node kind
		tuple.Tuple{vectorBytes}, // vector bytes wrapped in tuple
		neighborList,             // neighbor PKs as nested tuples
	}
	var additional []byte
	if existing := s.cache[string(key)]; existing != nil {
		additional = existing.additional
	}
	packed := append(value.Pack(), additional...)
	tx.Set(fdb.Key(key), packed)
	s.recordKeyValueWritten(layer, key, packed)
	s.recordNodeWritten(layer)

	// Update cache with parsed data so subsequent reads skip tuple.Unpack.
	// The cache holds neighbors as nested-encoded spans (Tuple{pk}.Pack()).
	cacheNeighbors := make([][]byte, len(neighbors))
	for i, pk := range neighbors {
		cacheNeighbors[i] = nestPK(pk)
	}
	s.cache[string(key)] = &parsedNode{vecBytes: vectorBytes, neighbors: cacheNeighbors, additional: additional}
}

// parseNodeValue parses the raw FDB value bytes for a node into vector bytes
// and neighbor PKs. Factored out of loadNodeLayer for reuse by batch loading.
// parseNodeValue decodes a COMPACT node value:
//
//	Pack({ nodeKind:int, {vectorBytes}, neighborList:{pk1, pk2, ...} })
//
// It returns the vector bytes and the neighbor primary keys as raw nested-encoded
// byte spans (== Tuple{pk}.Pack()), NOT as decoded tuple.Tuple values. Each span
// is directly usable as a fetch-key suffix (layerPrefix ++ span) and as a
// visited-set key; it is decoded back to a tuple.Tuple only at result/insert
// boundaries. Avoiding the per-PK tuple decode + element boxing here is the bulk
// of the search-path allocation savings — Java pays the equivalent Object[] cost,
// but Go's GC is far more sensitive to the interface-boxing churn.
func parseNodeValue(data []byte) (vectorBytes []byte, neighbors [][]byte, additional []byte, err error) {
	// elem 0: nodeKind (skip).
	p := 0
	n0 := tuplefast.Skip(data[p:])
	if n0 < 0 {
		return nil, nil, nil, fmt.Errorf("hnsw: truncated node value (nodeKind)")
	}
	p += n0

	// elem 1: vectorTuple = {vectorBytes} (nested). Children are stored verbatim,
	// so the content between the 0x05 marker and the nested terminator is exactly
	// the single bytes element's standalone encoding (which fastDecodeBytes
	// unescapes on its own). No extra un-nesting pass is needed.
	if p >= len(data) || data[p] != tuplefast.TypeCodeNested {
		return nil, nil, nil, fmt.Errorf("hnsw: node value missing vector tuple")
	}
	n1 := tuplefast.Skip(data[p:])
	if n1 < 0 || p+n1 > len(data) {
		return nil, nil, nil, fmt.Errorf("hnsw: truncated vector tuple")
	}
	vecInner := data[p+1 : p+n1-1]
	if len(vecInner) > 0 {
		// NOTE: must NOT un-escape in place — `data` is the FDB value buffer, which
		// the pure-Go client's read-your-writes / snapshot caches retain and reuse.
		// fastDecodeBytes copies, which is required for correctness here.
		vb, _, berr := tuplefast.DecodeBytes(vecInner)
		if berr != nil {
			return nil, nil, nil, fmt.Errorf("hnsw: decode vector bytes: %w", berr)
		}
		vectorBytes = vb
	}
	p += n1

	// elem 2: neighborList (nested). Optional — absent => no neighbors. Its
	// content is the concatenation of each neighbor's verbatim nested encoding
	// (== Tuple{pk}.Pack()), so each per-element span IS the fetch-key suffix.
	if p >= len(data) {
		return vectorBytes, nil, nil, nil
	}
	if data[p] != tuplefast.TypeCodeNested {
		return nil, nil, nil, fmt.Errorf("hnsw: node value neighbor list is not a tuple")
	}
	n2 := tuplefast.Skip(data[p:])
	if n2 < 0 || p+n2 > len(data) {
		return nil, nil, nil, fmt.Errorf("hnsw: truncated neighbor list")
	}
	neighbors, err = tuplefast.NestedPKSpans(data[p+1 : p+n2-1])
	if err != nil {
		return nil, nil, nil, err
	}
	p += n2

	// elem 3 (optional): the additional (covering) values tuple.
	if p < len(data) {
		n3 := tuplefast.Skip(data[p:])
		if n3 < 0 || p+n3 > len(data) {
			return nil, nil, nil, fmt.Errorf("hnsw: truncated additional values")
		}
		additional = append([]byte(nil), data[p:p+n3]...)
	}
	return vectorBytes, neighbors, additional, nil
}

// decodeNestedPK turns a neighbor span (nested-encoded PK == Tuple{pk}.Pack())
// back into the primary-key tuple. Used only at bounded boundaries (the ≤k search
// results and the ≤M selected insert neighbors), never in the hot traversal loop.
func decodeNestedPK(span []byte) (tuple.Tuple, error) {
	t, err := tuplefast.Unpack(span)
	if err != nil {
		return nil, fmt.Errorf("hnsw: decode neighbor span: %w", err)
	}
	if len(t) != 1 {
		return nil, fmt.Errorf("hnsw: neighbor span decoded to %d elements, want 1", len(t))
	}
	pk, ok := t[0].(tuple.Tuple)
	if !ok {
		return nil, fmt.Errorf("hnsw: neighbor span element is not a tuple (%T)", t[0])
	}
	return pk, nil
}

// nestPK encodes a primary-key tuple into its nested span form (== Tuple{pk}.Pack()),
// the inverse of decodeNestedPK. Used to bring an externally-supplied entry-point or
// inlining-layer PK into the span representation the traversal uses.
func nestPK(pk tuple.Tuple) []byte {
	return tuple.Tuple{pk}.Pack()
}

// loadNodeLayer reads one layer's data for a node.
// Returns vector bytes, neighbor PKs, and error (non-nil if not found).
// Uses the per-transaction cache to avoid re-reading the same node.
func (s *hnswStorage) loadNodeLayer(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) (vectorBytes []byte, neighbors [][]byte, err error) {
	// Java uses Tuple.from(layer, primaryKey) where primaryKey is nested.
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	cacheKey := string(key)

	// Check cache first — returns pre-parsed data, no tuple.Unpack needed.
	if cached, ok := s.cacheLookup(cacheKey); ok {
		hnswStatCacheHit(s.stats)
		if cached == nil {
			return nil, nil, fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
		}
		return cached.vecBytes, cached.neighbors, nil
	}

	hnswStatGet(s.stats)
	data, err := s.getKey(tx, fdb.Key(key))
	if err != nil {
		return nil, nil, fmt.Errorf("hnsw: get node layer %d: %w", layer, err)
	}
	s.recordKeyValueRead(layer, key, data)
	if data == nil {
		s.cache[cacheKey] = nil // cache negative result
		return nil, nil, fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
	}
	s.recordNodeRead(layer)

	// Parse and cache the result.
	var additional []byte
	vectorBytes, neighbors, additional, err = parseNodeValue(data)
	if err != nil {
		return nil, nil, err
	}
	s.cacheStore(cacheKey, &parsedNode{vecBytes: vectorBytes, neighbors: neighbors, additional: additional})
	return vectorBytes, neighbors, nil
}

// nodeResult holds the result of loading one node from FDB.
type nodeResult struct {
	span      []byte // nested-encoded PK span (fetch-key suffix == visited key)
	spanStr   string // cached string(span) for map keys
	vecBytes  []byte
	neighbors [][]byte // this node's neighbor PKs, as nested-encoded spans
	err       error
}

// loadNodeLayerBatch fires all FDB Get() calls for the given PKs at once,
// then resolves them. FDB pipelines the reads, turning N sequential round-trips
// into 1 round-trip with N pipelined reads. Populates the per-transaction cache.
// loadNodeLayerBatch fetches the given neighbor spans at a layer. Each span is a
// nested-encoded PK; the FDB key is the per-layer prefix concatenated with the
// span (proven byte-identical to dataSubspace.Pack({layer, pk})), so no PK decode
// or re-pack happens here.
func (s *hnswStorage) loadNodeLayerBatch(tx fdb.ReadTransaction, layer int, pks [][]byte) []nodeResult {
	results := make([]nodeResult, len(pks))
	// Per-layer key prefix: dataSubspace.Pack({layer}) ++ span == full node key.
	layerPrefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	type pending struct {
		idx    int
		key    string
		future fdb.FutureByteSlice
	}
	var toFetch []pending

	for i, span := range pks {
		results[i].span = span
		key := make([]byte, 0, len(layerPrefix)+len(span))
		key = append(key, layerPrefix...)
		key = append(key, span...)
		cacheKey := string(key)
		results[i].spanStr = string(span)

		if cached, ok := s.cacheLookup(cacheKey); ok {
			hnswStatCacheHit(s.stats)
			if cached == nil {
				results[i].err = fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
			} else {
				results[i].vecBytes = cached.vecBytes
				results[i].neighbors = cached.neighbors
			}
			continue
		}

		// Fire the get — don't resolve yet.
		toFetch = append(toFetch, pending{idx: i, key: cacheKey, future: tx.Get(fdb.Key(key))})
	}

	// Count as 1 batch round-trip (all Gets pipelined).
	if len(toFetch) > 0 {
		hnswStatBatchGet(s.stats)
	}

	// Resolve all outstanding futures. FDB pipelines these reads.
	for _, p := range toFetch {
		data, err := p.future.Get()
		if err != nil {
			results[p.idx].err = fmt.Errorf("hnsw: get node layer %d: %w", layer, err)
			continue
		}
		s.recordKeyValueRead(layer, []byte(p.key), data)
		if data == nil {
			s.cache[p.key] = nil // cache negative result
			results[p.idx].err = fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
			continue
		}
		s.recordNodeRead(layer)
		vecBytes, neighbors, additional, parseErr := parseNodeValue(data)
		if parseErr != nil {
			results[p.idx].err = parseErr
			continue
		}
		s.cacheStore(p.key, &parsedNode{vecBytes: vecBytes, neighbors: neighbors, additional: additional})
		results[p.idx].vecBytes = vecBytes
		results[p.idx].neighbors = neighbors
	}

	return results
}

// edgeListResult holds the neighbors from an edge-list fetch.
type edgeListResult struct {
	neighbors [][]byte // neighbor PKs as nested-encoded spans
	err       error
}

// loadEdgeListsBatch fetches edge lists for multiple nodes in parallel.
// For non-inlining layers, issues all FDB Gets as futures before resolving any,
// reducing serial round-trips from N to 1. For inlining layers (cached from
// preload), falls back to sequential cache lookups (already fast, no I/O).
// loadEdgeListsBatch fetches the neighbor lists of the given source-node spans.
func (s *hnswStorage) loadEdgeListsBatch(tx fdb.ReadTransaction, layer int, pks [][]byte) []edgeListResult {
	results := make([]edgeListResult, len(pks))

	if s.isInliningLayer(layer) {
		// Inlining layers: data cached from preload, no I/O to parallelize.
		// Few nodes live up here, so decoding the source span is negligible.
		for i, span := range pks {
			pk, derr := decodeNestedPK(span)
			if derr != nil {
				results[i] = edgeListResult{err: derr}
				continue
			}
			_, neighbors, err := s.loadNodeLayerInlining(tx, layer, pk)
			results[i] = edgeListResult{neighbors: neighbors, err: err}
		}
		return results
	}

	// Non-inlining: issue all Gets as futures, then resolve.
	layerPrefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	type pending struct {
		idx      int
		cacheKey string
		future   fdb.FutureByteSlice
	}
	var toFetch []pending

	for i, span := range pks {
		key := make([]byte, 0, len(layerPrefix)+len(span))
		key = append(key, layerPrefix...)
		key = append(key, span...)
		cacheKey := string(key)

		if cached, ok := s.cacheLookup(cacheKey); ok {
			hnswStatCacheHit(s.stats)
			if cached == nil {
				results[i].err = fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
			} else {
				results[i].neighbors = cached.neighbors
			}
			continue
		}

		// Fire the Get without waiting — FDB pipelines these.
		toFetch = append(toFetch, pending{idx: i, cacheKey: cacheKey, future: tx.Get(fdb.Key(key))})
	}

	if len(toFetch) > 0 {
		hnswStatBatchGet(s.stats)
	}

	// Resolve all futures.
	for _, p := range toFetch {
		data, err := p.future.Get()
		if err != nil {
			results[p.idx].err = fmt.Errorf("hnsw: get node layer %d: %w", layer, err)
			continue
		}
		s.recordKeyValueRead(layer, []byte(p.cacheKey), data)
		if data != nil {
			s.recordNodeRead(layer)
		}
		if data == nil {
			s.cache[p.cacheKey] = nil
			results[p.idx].err = fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
			continue
		}
		vecBytes, neighbors, additional, parseErr := parseNodeValue(data)
		if parseErr != nil {
			results[p.idx].err = parseErr
			continue
		}
		s.cacheStore(p.cacheKey, &parsedNode{vecBytes: vecBytes, neighbors: neighbors, additional: additional})
		results[p.idx].neighbors = neighbors
	}

	return results
}

// preloadLayer reads ALL nodes at the given layer in a single GetRange call
// and populates the cache. For upper layers (layer > 0) with few nodes, this
// replaces multiple individual Get() round-trips with one range scan.
// For a 1K-node graph with M=16: layer 2 has ~4 nodes, layer 1 has ~62 nodes.
func (s *hnswStorage) preloadLayer(tx fdb.ReadTransaction, layer int) error {
	hnswStatRangeRead(s.stats)
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return fmt.Errorf("hnsw: preload layer %d prefix range: %w", layer, err)
	}
	iter := s.scanIter(tx, r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return fmt.Errorf("hnsw: preload layer %d get kv: %w", layer, err)
		}
		s.recordKeyValueRead(layer, kv.Key, kv.Value)
		s.recordNodeRead(layer)
		cacheKey := string(kv.Key)
		if _, ok := s.cache[cacheKey]; ok {
			continue // already cached (e.g. from a save earlier in this tx)
		}
		vecBytes, neighbors, additional, parseErr := parseNodeValue(kv.Value)
		if parseErr != nil {
			continue // skip unparseable entries
		}
		s.cache[cacheKey] = &parsedNode{vecBytes: vecBytes, neighbors: neighbors, additional: additional}
	}
	// Advance()==false ends the loop on exhaustion OR a transient FDB error; check
	// Get() for the stored error so a mid-scan 1007/timeout surfaces instead of
	// silently leaving the layer cache PARTIALLY populated (a corrupt-graph hazard).
	if _, err := iter.Get(); err != nil {
		return fmt.Errorf("hnsw: preload layer %d scan: %w", layer, err)
	}
	return nil
}

// deleteNodeLayer removes one layer's data for a node.
func (s *hnswStorage) deleteNodeLayer(tx fdb.WritableTransaction, layer int, primaryKey tuple.Tuple) {
	// Java uses Tuple.from(layer, primaryKey) where primaryKey is nested.
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	tx.Clear(fdb.Key(key))

	// Mark as deleted in cache (nil *parsedNode = negative cache entry).
	s.cache[string(key)] = nil
}
