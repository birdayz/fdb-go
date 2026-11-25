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
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// --- Inlining storage format ---
// Wire-compatible with Java's InliningStorageAdapter.
//
// Key: dataSubspace.Pack(Tuple{layer, sourcePK, neighborPK})
// Value: Tuple{neighborVecBytes}.Pack()
//
// Used for layers > 0 when UseInlining is enabled. Layer 0 always uses compact format.

// isInliningLayer returns true if the given layer should use the inlining storage format.
func (s *hnswStorage) isInliningLayer(layer int) bool {
	return s.config.UseInlining && layer > 0
}

// saveNodeLayerInlining writes a node's edges in inlining format.
// Each neighbor is a separate KV: (layer, pk, neighborPK) → tuple-packed neighborVector.
// The node's own vector is not stored at inlining layers — it's stored at layer 0 (compact).
// neighborVecs, when non-nil, is parallel to neighbors and carries each neighbor's stored
// vector bytes — the Go analog of Java's NodeReferenceWithVector, where the vector travels
// WITH the reference from search/selection to the write (InliningStorageAdapter writes
// nodeReference.getVector(), never a cache lookup). A nil entry falls back to the per-tx
// cache; an unresolvable vector is an ERROR, never a silently dropped edge.
func (s *hnswStorage) saveNodeLayerInlining(tx fdb.WritableTransaction, layer int, primaryKey tuple.Tuple, neighbors []tuple.Tuple, neighborVecs [][]byte) error {
	// Clear all existing edges for this node at this layer.
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	r, err := fdb.PrefixRange(prefix)
	if err == nil {
		tx.ClearRange(r)
	}

	// A node with no neighbors at an inlining layer writes NOTHING — matching Java's
	// InliningStorageAdapter, where BaseNeighborsChangeSet.writeDelta is a no-op for an
	// empty change set (Primitives.writeLonelyNodeOnLayer → writeNode → writeNodeInternal
	// only calls changeSet.writeDelta). A sentinel KV here would be a 2-element (layer, pk)
	// key; Java's inlining scanner parses every KV at a layer as a 3-element edge via
	// keyTuple.getNestedTuple(2) (InliningStorageAdapter.java:198/376), so a 2-element key
	// makes a Java reader sharing the cluster throw — a wire-format break.
	//
	// The lonely-node case (an entry point at layers above all other nodes) needs no
	// sentinel: loadNodeLayerInlining returns a node with an EMPTY neighbor list for an
	// empty range (Java's InliningStorageAdapter contract — absent and lonely are
	// indistinguishable at an inlining layer), so both same-tx (cache) and cross-tx
	// (range read) readers see "exists, no neighbors". Node existence is determined by
	// the layer-0 compact record / topLayer(pk), exactly as in Java — never by an
	// inlining edge.

	// Write each edge with the neighbor's vector. The vector comes from the caller
	// (threaded with the reference, like Java's NodeReferenceWithVector) or, for
	// neighbors that were read in this tx, from the per-tx cache.
	for i, nbPK := range neighbors {
		var nbVecBytes []byte
		if neighborVecs != nil {
			nbVecBytes = neighborVecs[i]
		}
		if nbVecBytes == nil {
			nbVecBytes = s.getVectorBytesFromCache(layer, nbPK)
		}
		if nbVecBytes == nil {
			// Fallback: try layer 0 (compact format always has the vector).
			nbVecBytes = s.getVectorBytesFromCache(0, nbPK)
		}
		if nbVecBytes == nil {
			// Never drop an edge silently: a skipped write here "succeeds" while leaving
			// the layer disconnected (it bit on cold inserts reaching a lonely entry's
			// layer before vectors were threaded through). Java cannot hit this — the
			// vector is part of the neighbor reference by construction.
			return fmt.Errorf("hnsw: save inlining node %v at layer %d: no vector for neighbor %v", primaryKey, layer, nbPK)
		}

		edgeKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey, nbPK})
		edgeValue := tuple.Tuple{nbVecBytes}.Pack()
		tx.Set(fdb.Key(edgeKey), edgeValue)
		s.recordKeyValueWritten(layer, edgeKey, edgeValue)

		// Record the vector so later same-tx saves can resolve it (merges into a
		// vector-less source entry — see cacheNeighborVector).
		s.cacheNeighborVector(layer, nbPK, nbVecBytes)
		cachedKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), nbPK})
		if cached := s.cache[string(cachedKey)]; cached != nil {
			cached.vecBytes = nbVecBytes
		}
	}
	s.recordNodeWritten(layer)

	// Update the cache for this node so loadNodeLayerDispatch returns
	// neighbors from cache without hitting FDB.
	compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	// Use empty slice (not nil) when no neighbors, so cache knows the node
	// exists but has no neighbors (nil neighbors means "unknown, needs range read").
	// Cache holds neighbors as nested-encoded spans.
	cachedNeighbors := make([][]byte, len(neighbors))
	for i, pk := range neighbors {
		cachedNeighbors[i] = nestPK(pk)
	}
	// Inlining storage has no own-vector at this layer, but the cache entry may carry
	// the node's vector learned from an edge value or a threaded neighborVec — PRESERVE
	// it (like the load path does). Writing vecBytes nil here clobbered the entry
	// point's just-merged vector between two reverse-edge saves of the same insert,
	// making the second save fail with "no vector for neighbor".
	var existingVec, additional []byte
	if existing, ok := s.cache[string(compactKey)]; ok && existing != nil {
		existingVec, additional = existing.vecBytes, existing.additional
	}
	s.cache[string(compactKey)] = &parsedNode{vecBytes: existingVec, neighbors: cachedNeighbors, additional: additional}
	return nil
}

// cacheNeighborVector records a neighbor's vector bytes at a layer. If the node is not
// cached, writes a partial (vector-only) entry; if it is cached WITHOUT a vector — e.g.
// it was loaded as a source, where inlining storage has no own-vector — the vector is
// MERGED into the existing entry. Skipping the merge loses the only copy of the vector
// a later save's cache fallback can reach (a node's vector arrives via edge values or
// threaded neighborVecs, and each arrival must stick).
func (s *hnswStorage) cacheNeighborVector(layer int, nbPK tuple.Tuple, vecBytes []byte) {
	key := string(s.dataSubspace.Pack(tuple.Tuple{int64(layer), nbPK}))
	if existing, ok := s.cache[key]; ok {
		if existing != nil && existing.vecBytes == nil {
			existing.vecBytes = vecBytes
		}
		return
	}
	s.cache[key] = &parsedNode{vecBytes: vecBytes, neighbors: nil}
}

// getVectorBytesFromCache looks up a node's vector bytes from the cache at a given layer.
func (s *hnswStorage) getVectorBytesFromCache(layer int, pk tuple.Tuple) []byte {
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), pk})
	if cached, ok := s.cache[string(key)]; ok && cached != nil {
		return cached.vecBytes
	}
	return nil
}

// resolveVectorBytes returns the vector bytes for a node. If vecBytes is non-nil, returns it.
// Otherwise looks up the vector from the cache, trying the given layer then falling back
// to layer 0 (where the compact format always stores the vector).
func (s *hnswStorage) resolveVectorBytes(layer int, pk tuple.Tuple, vecBytes []byte) []byte {
	if vecBytes != nil {
		return vecBytes
	}
	// Try current layer cache.
	if vb := s.getVectorBytesFromCache(layer, pk); vb != nil {
		return vb
	}
	// Fall back to layer 0 (compact format always has the vector).
	return s.getVectorBytesFromCache(0, pk)
}

// loadNodeLayerInlining reads a node's neighbors using inlining format.
// A single GetRange on (layer, pk, *) returns all neighbor PKs + their vectors.
// Also populates the cache with each neighbor's vector for subsequent distance lookups.
func (s *hnswStorage) loadNodeLayerInlining(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) ([]byte, [][]byte, error) {
	// Check cache first (populated by saveNodeLayerInlining, preloadLayerInlining,
	// or a prior loadNodeLayerInlining call).
	compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	cacheKey := string(compactKey)
	if cached, ok := s.cacheLookup(cacheKey); ok {
		hnswStatCacheHit(s.stats)
		if cached == nil {
			// Deleted in this tx (deleteNodeLayerInlining caches nil). Java's RYW range
			// read sees the cleared range and returns a node with an EMPTY neighbor list —
			// inlining storage cannot distinguish absent from lonely (see below).
			return nil, [][]byte{}, nil
		}
		// If we have neighbors, return immediately. If neighbors is nil, this was
		// a vector-only cache entry from an edge read — we need to fetch the actual
		// neighbor list via a range read below.
		if cached.neighbors != nil {
			return cached.vecBytes, cached.neighbors, nil
		}
	}

	// Range read: all KVs under (layer, pk, *).
	hnswStatRangeRead(s.stats)
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("hnsw: inlining prefix range layer %d: %w", layer, err)
	}

	iter := s.scanIter(tx, r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	var neighbors [][]byte
	foundAnyKV := false

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, nil, fmt.Errorf("hnsw: inlining get kv layer %d: %w", layer, err)
		}
		foundAnyKV = true
		s.recordKeyValueRead(layer, kv.Key, kv.Value)

		// Unpack the key to get (layer, sourcePK, neighborPK).
		keyTuple, unpackErr := tuplefast.SubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
		if unpackErr != nil || len(keyTuple) < 3 {
			// An inlining edge key is always 3 elements (layer, pk, neighborPK). A
			// shorter key is not a valid edge — skip defensively. (We no longer write a
			// 2-element sentinel; Java never does either, so this only guards malformed
			// or legacy data, never the normal path.)
			continue
		}

		nbPK, ok := keyTuple[2].(tuple.Tuple)
		if !ok {
			continue
		}

		// Value is tuple-packed neighbor vector bytes.
		valueTuple, valErr := tuplefast.Unpack(kv.Value)
		if valErr != nil || len(valueTuple) < 1 {
			continue
		}
		nbVecBytes, ok := valueTuple[0].([]byte)
		if !ok {
			continue
		}

		neighbors = append(neighbors, nestPK(nbPK))

		// Cache the neighbor's vector at this layer so loadNodeLayerBatch hits cache.
		// We don't know the neighbor's own neighbors yet, but we have its vector —
		// merged into a vector-less source entry if one exists (cacheNeighborVector).
		s.cacheNeighborVector(layer, nbPK, nbVecBytes)
	}
	// Surface a transient FDB error that ended the scan via Advance()==false, so an
	// incomplete neighbor list isn't returned as a complete one (silent corruption).
	if _, err := iter.Get(); err != nil {
		return nil, nil, fmt.Errorf("hnsw: inlining scan layer %d: %w", layer, err)
	}
	// An inlining node is read from its (possibly empty) edge range.
	s.recordNodeRead(layer)

	if !foundAnyKV {
		// Empty range — return a node with an EMPTY neighbor list, never an error.
		// This is Java's InliningStorageAdapter contract: inlining storage cannot
		// distinguish a node with no neighbors from an absent one (its Javadoc says so,
		// InliningStorageAdapter.java:94-95), and fetchNodeInternal/nodeFromRaw always
		// build a node from the (possibly empty) KV list (InliningStorageAdapter.java:
		// 106-118, 139-156). Node existence is determined by the layer-0 compact record,
		// never by an inlining edge. Returning errHNSWNotPresent here broke cold inserts:
		// a fresh tx whose insert level reaches a lonely entry point's layer selects that
		// entry as a neighbor (searchLayerMulti pre-seeds the entry into its results) and
		// the reverse-connection load propagated the error as fatal.
		empty := &parsedNode{vecBytes: nil, neighbors: [][]byte{}}
		s.cache[cacheKey] = empty
		return nil, empty.neighbors, nil
	}

	// Cache the full node. Preserve any existing vecBytes from a prior
	// vector-only cache entry (from edge reads).
	var existingVec []byte
	if existing, ok := s.cache[cacheKey]; ok && existing != nil {
		existingVec = existing.vecBytes
	}
	s.cache[cacheKey] = &parsedNode{vecBytes: existingVec, neighbors: neighbors}
	return existingVec, neighbors, nil
}

// deleteNodeLayerInlining clears outgoing edges for this node at the given
// inlining layer via a (layer, pk, *) prefix range clear. Incoming edges
// (where this node is a neighbor of others) are cleaned up by repairNeighbor.
func (s *hnswStorage) deleteNodeLayerInlining(tx fdb.WritableTransaction, layer int, primaryKey tuple.Tuple) {
	// Clear outgoing edges: (layer, pk, *).
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	r, err := fdb.PrefixRange(prefix)
	if err == nil {
		tx.ClearRange(r)
	}

	// Mark as deleted in cache.
	s.cache[string(prefix)] = nil
}

// preloadLayerInlining reads ALL nodes at the given inlining layer in a single GetRange,
// groups edges by source node, and populates the cache.
func (s *hnswStorage) preloadLayerInlining(tx fdb.ReadTransaction, layer int) error {
	hnswStatRangeRead(s.stats)
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return fmt.Errorf("hnsw: preload inlining layer %d prefix range: %w", layer, err)
	}

	// Group edges by source PK.
	type edgeInfo struct {
		neighborPK tuple.Tuple
		vecBytes   []byte
	}
	nodeEdges := make(map[string][]edgeInfo) // source PK packed bytes → edges
	nodePKs := make(map[string]tuple.Tuple)  // source PK packed bytes → source PK

	iter := s.scanIter(tx, r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return fmt.Errorf("hnsw: preload inlining layer %d get kv: %w", layer, err)
		}
		s.recordKeyValueRead(layer, kv.Key, kv.Value)

		keyTuple, unpackErr := tuplefast.SubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
		if unpackErr != nil || len(keyTuple) < 3 {
			continue
		}

		sourcePK, ok := keyTuple[1].(tuple.Tuple)
		if !ok {
			continue
		}
		nbPK, ok := keyTuple[2].(tuple.Tuple)
		if !ok {
			continue
		}

		valueTuple, valErr := tuplefast.Unpack(kv.Value)
		if valErr != nil || len(valueTuple) < 1 {
			continue
		}
		nbVecBytes, ok := valueTuple[0].([]byte)
		if !ok {
			continue
		}

		sourceKey := string(sourcePK.Pack())
		nodeEdges[sourceKey] = append(nodeEdges[sourceKey], edgeInfo{neighborPK: nbPK, vecBytes: nbVecBytes})
		nodePKs[sourceKey] = sourcePK
	}
	// Surface a transient FDB error that ended the scan before populating the cache
	// from the (otherwise partial) edge map — a silent partial cache corrupts the graph.
	if _, err := iter.Get(); err != nil {
		return fmt.Errorf("hnsw: preload inlining layer %d scan: %w", layer, err)
	}

	// Populate cache: one entry per source node with its neighbor list.
	for sourceKey, edges := range nodeEdges {
		s.recordNodeRead(layer)
		sourcePK := nodePKs[sourceKey]
		compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), sourcePK})
		if _, ok := s.cache[string(compactKey)]; ok {
			continue // already cached
		}

		neighbors := make([][]byte, len(edges))
		for i, e := range edges {
			neighbors[i] = nestPK(e.neighborPK)

			// Also cache each neighbor's vector (merging into a vector-less source
			// entry if one exists — cacheNeighborVector).
			s.cacheNeighborVector(layer, e.neighborPK, e.vecBytes)
		}

		s.cache[string(compactKey)] = &parsedNode{vecBytes: nil, neighbors: neighbors}
	}

	return nil
}

// --- Dispatch methods ---
// These choose between compact and inlining format based on layer and config.

// saveNodeLayerDispatch saves a node's layer data in the appropriate format.
// neighborVecs (optional, parallel to neighbors) carries each neighbor's stored vector
// bytes for inlining edge values — the Go analog of Java's NodeReferenceWithVector,
// where the vector travels with the reference from search/selection to the write.
// Compact writes don't need it (nil is fine). Returns an error if an inlining edge's
// vector cannot be resolved from the provided vecs or the per-tx cache.
func (s *hnswStorage) saveNodeLayerDispatch(tx fdb.WritableTransaction, layer int, primaryKey tuple.Tuple, vectorBytes []byte, neighbors []tuple.Tuple, neighborVecs [][]byte) error {
	if s.isInliningLayer(layer) {
		return s.saveNodeLayerInlining(tx, layer, primaryKey, neighbors, neighborVecs)
	}
	s.saveNodeLayer(tx, layer, primaryKey, vectorBytes, neighbors)
	return nil
}

// loadNodeLayerDispatch loads a node's layer data from the appropriate format.
func (s *hnswStorage) loadNodeLayerDispatch(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) (vectorBytes []byte, neighbors [][]byte, err error) {
	if s.isInliningLayer(layer) {
		return s.loadNodeLayerInlining(tx, layer, primaryKey)
	}
	return s.loadNodeLayer(tx, layer, primaryKey)
}

// loadNodeLayerBatchDispatch batch-loads nodes from the appropriate format.
// For inlining layers, individual loadNodeLayerInlining calls are still used but the
// neighbor vectors are already cached from the first range read.
func (s *hnswStorage) loadNodeLayerBatchDispatch(tx fdb.ReadTransaction, layer int, pks [][]byte) []nodeResult {
	if !s.isInliningLayer(layer) {
		return s.loadNodeLayerBatch(tx, layer, pks)
	}

	// For inlining layers, the cache is already populated by loadNodeLayerInlining/preloadLayerInlining.
	// We just need to look up each span in the cache (key == layerPrefix ++ span).
	layerPrefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	results := make([]nodeResult, len(pks))
	for i, span := range pks {
		results[i].span = span
		results[i].spanStr = string(span)

		compactKey := make([]byte, 0, len(layerPrefix)+len(span))
		compactKey = append(compactKey, layerPrefix...)
		compactKey = append(compactKey, span...)
		if cached, ok := s.cache[string(compactKey)]; ok {
			if cached == nil {
				// Deleted in this tx — Java's inlining fetch returns an empty node
				// (see loadNodeLayerInlining).
				results[i].neighbors = [][]byte{}
			} else {
				results[i].vecBytes = cached.vecBytes
				results[i].neighbors = cached.neighbors
			}
			continue
		}

		// Not cached yet — decode the span and do a range read (rare after preload).
		pk, derr := decodeNestedPK(span)
		if derr != nil {
			results[i].err = derr
			continue
		}
		vecBytes, neighbors, err := s.loadNodeLayerInlining(tx, layer, pk)
		if err != nil {
			results[i].err = err
		} else {
			results[i].vecBytes = vecBytes
			results[i].neighbors = neighbors
		}
	}
	return results
}

// deleteNodeLayerDispatch deletes a node's layer data in the appropriate format.
func (s *hnswStorage) deleteNodeLayerDispatch(tx fdb.WritableTransaction, layer int, primaryKey tuple.Tuple) {
	if s.isInliningLayer(layer) {
		s.deleteNodeLayerInlining(tx, layer, primaryKey)
	} else {
		s.deleteNodeLayer(tx, layer, primaryKey)
	}
}

// preloadLayerDispatch preloads a layer in the appropriate format.
func (s *hnswStorage) preloadLayerDispatch(tx fdb.ReadTransaction, layer int) error {
	if s.isInliningLayer(layer) {
		return s.preloadLayerInlining(tx, layer)
	}
	return s.preloadLayer(tx, layer)
}
