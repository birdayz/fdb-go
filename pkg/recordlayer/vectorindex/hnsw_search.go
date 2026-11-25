// Portions derived from FoundationDB Record Layer (InliningStorageAdapter.java,
// Insert.java, NodeReferenceWithVector.java, Config.java, and others),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"bytes"
	"container/heap"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Search finds the k nearest neighbors to the query vector.
// Returns results sorted by distance (closest first).
func (g *hnswGraph) Search(tx fdb.ReadTransaction, query []float64, k, efSearch int) ([]hnswSearchResult, error) {
	return g.searchWithVectors(tx, query, k, efSearch, false)
}

func (g *hnswGraph) searchWithVectors(tx fdb.ReadTransaction, query []float64, k, efSearch int, includeVectors bool) ([]hnswSearchResult, error) {
	accessInfo, err := g.storage.loadAccessInfo(tx)
	if err != nil {
		if e := hnswFatal(err); e != nil {
			return nil, e // transient read — propagate so the caller retries
		}
		return nil, nil // genuinely empty graph
	}
	if err := g.raBitQuantizerAdmits(accessInfo); err != nil {
		return nil, err
	}

	// Apply transform to query vector so it's in the same coordinate system
	// as the stored vectors. Matches Java's StorageTransform.transform(queryVector).
	// opXform also lifts any plain (pre-centroid) stored vector at read time.
	transform := g.buildTransform(accessInfo)
	g.opXform = transform
	searchQuery := query
	if transform != nil {
		searchQuery = transform.apply(query)
	}

	// Preload upper layers (few nodes each) in one GetRange per layer.
	// This replaces ~2 round-trips per greedy iteration with zero after preload.
	for layer := accessInfo.layer; layer > 0; layer-- {
		if preloadErr := g.storage.preloadLayerDispatch(tx, layer); preloadErr != nil {
			return nil, preloadErr
		}
	}

	// Greedy descent from top layer to layer 1.
	currentPK := accessInfo.pk
	currentVector := hnswVector{data: accessInfo.vectorBytes, transformed: true}
	for layer := accessInfo.layer; layer > 0; layer-- {
		currentPK, currentVector, err = g.searchLayerGreedy(tx, searchQuery, currentPK, currentVector, layer)
		if err != nil {
			return nil, err
		}
	}

	// Search at layer 0 with efSearch as given: like Java's beam search, an
	// efSearch below k returns fewer than k results.
	candidates, err := g.searchLayerMulti(tx, searchQuery, currentPK, currentVector, efSearch, 0)
	if err != nil {
		return nil, err
	}

	// Return top-k.
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	results := make([]hnswSearchResult, len(candidates))
	for i, c := range candidates {
		pk, derr := decodeNestedPK(c.pkSpan)
		if derr != nil {
			return nil, derr
		}
		var vector []byte
		if includeVectors {
			vector = c.vector.data
			if transform != nil {
				decoded, err := g.decodeVector(c.vector)
				if err != nil {
					return nil, err
				}
				vector = serializeVector(transform.invertedApply(decoded))
			}
		}
		results[i] = hnswSearchResult{
			PrimaryKey: pk,
			Distance:   c.dist,
			Vector:     vector,
		}
	}
	return results, nil
}

// refetchEntryVector is Search.beamSearchLayer's refetch of its starting
// references: at a compact layer the entry's vector is read from the node
// stored there, so its distance is the one every other visit computes (the
// access info's or a higher layer's vector may be encoded differently). An
// inlining layer's greedy search starts from the reference as given
// (greedySearchInliningLayer); a missing node keeps it too.
func (g *hnswGraph) refetchEntryVector(tx fdb.ReadTransaction, epPK tuple.Tuple, epVector hnswVector, layer int) (hnswVector, error) {
	if epPK == nil || g.storage.isInliningLayer(layer) {
		return epVector, nil
	}
	vecBytes, _, err := g.storage.loadNodeLayerDispatch(tx, layer, epPK)
	if err != nil {
		if e := hnswFatal(err); e != nil {
			return hnswVector{}, e
		}
		return epVector, nil
	}
	return hnswVector{data: vecBytes}, nil
}

// searchLayerGreedy finds the single nearest neighbor at a given layer (greedy descent).
// Returns the best PK, its stored vector bytes, and error.
func (g *hnswGraph) searchLayerGreedy(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVector hnswVector, layer int) (tuple.Tuple, hnswVector, error) {
	epVector, err := g.refetchEntryVector(tx, epPK, epVector, layer)
	if err != nil {
		return nil, hnswVector{}, err
	}
	bestPK := epPK
	bestVector := epVector
	bestDist := g.vectorDistance(query, epVector)
	changed := true

	for changed {
		changed = false
		_, neighbors, err := g.storage.loadNodeLayerDispatch(tx, layer, bestPK)
		if err != nil {
			if e := hnswFatal(err); e != nil {
				return nil, hnswVector{}, e
			}
			break // no data at this layer
		}

		// Batch-fetch all neighbor vectors.
		// For inlining layers, neighbor vectors are already cached from the range read.
		batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, neighbors)
		for _, r := range batchResults {
			if r.err != nil {
				if e := hnswFatal(r.err); e != nil {
					return nil, hnswVector{}, e
				}
				continue
			}
			dist := g.computeDistance(query, r.vecBytes)
			if hnswDistLess(dist, bestDist) {
				bestDist = dist
				bestPK, err = decodeNestedPK(r.span)
				if err != nil {
					return nil, hnswVector{}, err
				}
				bestVector = hnswVector{data: r.vecBytes}
				changed = true
			}
		}
	}

	return bestPK, bestVector, nil
}

// hnswCandidate represents a search candidate with its primary key, vector bytes, and distance.
type hnswCandidate struct {
	pkSpan []byte     // neighbor PK as a nested-encoded span (decode to tuple.Tuple only at boundaries)
	vector hnswVector // vector bytes with coordinate provenance
	vec    []float64  // decoded vector (lazy, for heuristic pairwise distances)
	dist   float64
}

// hnswPrefetchCandidates is the number of candidates to pop from the heap
// per iteration and fetch edge lists for in parallel. Reduces serial FDB
// round-trips from N to N/prefetch. 4 balances I/O pipelining against
// over-fetching (popping candidates that would have been pruned).
const hnswPrefetchCandidates = 4

// hnswSearchCapacityHint bounds the up-front allocation of a layer search.
const hnswSearchCapacityHint = 1024

// searchLayerMulti finds the ef nearest neighbors at a given layer.
// Uses parallel candidate prefetching: pops up to hnswPrefetchCandidates
// from the heap per iteration, issues all edge-list reads as pipelined FDB
// futures, then batch-fetches all unvisited neighbor vectors.
func (g *hnswGraph) searchLayerMulti(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVector hnswVector, ef, layer int) ([]hnswCandidate, error) {
	if epPK == nil {
		return nil, nil
	}

	epVector, err := g.refetchEntryVector(tx, epPK, epVector, layer)
	if err != nil {
		return nil, err
	}
	epDist := g.vectorDistance(query, epVector)
	// Entry point in span form (nested-encoded PK) so it dedups consistently with
	// neighbor spans pulled from node values.
	epSpan := nestPK(epPK)
	epSpanStr := string(epSpan)

	// ef is a query option, not a capacity: Java's PriorityQueue(efSearch + 1)
	// fails for a huge one, so preallocation here is bounded.
	capHint := min(max(ef, 1), hnswSearchCapacityHint)
	// Java's beam polls only after an add, so it never drops below the entry.
	keep := max(ef, 1)

	// Candidates (min-heap by distance) and visited set.
	backing := make(distHeap, 0, capHint)
	candidates := &backing
	heap.Push(candidates, distItem{pkSpan: epSpan, dist: epDist, spanStr: epSpanStr})

	visited := make(map[string]bool, capHint*2)
	visited[epSpanStr] = true

	results := make([]hnswCandidate, 1, capHint)
	results[0] = hnswCandidate{pkSpan: epSpan, vector: epVector, dist: epDist}

	// Pre-allocate buffers reused across iterations. Spans are carried raw — no
	// per-neighbor tuple decode / re-pack in this loop (the allocation win).
	poppedPKs := make([][]byte, 0, hnswPrefetchCandidates)
	toFetch := make([][]byte, 0, g.config.M*hnswPrefetchCandidates)

	for candidates.Len() > 0 {
		// Pop up to hnswPrefetchCandidates from the heap.
		poppedPKs = poppedPKs[:0]
		done := false
		for candidates.Len() > 0 && len(poppedPKs) < hnswPrefetchCandidates {
			closest := heap.Pop(candidates).(distItem)

			// Early termination: if closest unprocessed candidate is farther
			// than our worst result, all remaining candidates are too.
			if len(results) >= ef && hnswDistLess(results[len(results)-1].dist, closest.dist) {
				done = true
				break
			}
			poppedPKs = append(poppedPKs, closest.pkSpan)
		}
		if done || len(poppedPKs) == 0 {
			break
		}

		// Parallel edge-list fetch: issue all FDB reads before resolving any.
		edgeLists := g.storage.loadEdgeListsBatch(tx, layer, poppedPKs)

		// Collect ALL unvisited neighbors across all popped candidates.
		toFetch = toFetch[:0]
		for _, el := range edgeLists {
			if el.err != nil {
				if e := hnswFatal(el.err); e != nil {
					return nil, e
				}
				continue
			}
			for _, nbSpan := range el.neighbors {
				// The span IS the visited key and the fetch-key suffix — no Pack().
				key := string(nbSpan)
				if visited[key] {
					continue
				}
				visited[key] = true
				toFetch = append(toFetch, nbSpan)
			}
		}

		// Batch-read all unvisited neighbor vectors at once.
		// For inlining layers, neighbor vectors are already cached from the range read.
		batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, toFetch)
		for _, r := range batchResults {
			if r.err != nil {
				if e := hnswFatal(r.err); e != nil {
					return nil, e
				}
				continue
			}
			dist := g.computeDistance(query, r.vecBytes)

			if len(results) < ef || hnswDistLess(dist, results[len(results)-1].dist) {
				// r.spanStr is already computed by loadNodeLayerBatch — no double Pack().
				heap.Push(candidates, distItem{pkSpan: r.span, dist: dist, spanStr: r.spanStr})
				// Binary-search insertion into sorted results (O(log n) find + O(n) shift).
				c := hnswCandidate{pkSpan: r.span, vector: hnswVector{data: r.vecBytes}, dist: dist}
				pos := sort.Search(len(results), func(i int) bool {
					return hnswDistLess(dist, results[i].dist)
				})
				results = append(results, hnswCandidate{})
				copy(results[pos+1:], results[pos:])
				results[pos] = c
				if len(results) > keep {
					results = results[:keep]
				}
			}
		}
	}

	return results, nil
}

// selectNeighbors selects the best maxConn neighbors from candidates.
// Uses the HNSW paper's Algorithm 4 heuristic: for metrics that satisfy
// triangle inequality (Euclidean), candidates are selected greedily to
// prefer diverse directions. For other metrics (Cosine, InnerProduct),
// falls back to simple distance-based selection.
// Matches Java's Primitives.selectCandidates().
func (g *hnswGraph) selectNeighbors(candidates []hnswCandidate, maxConn int) []hnswCandidate {
	// Java polls the candidates from a queue ordered by
	// NodeReferenceWithDistance.comparator (distance, then primary key) and
	// runs the heuristic even when they all fit: a candidate closer to an
	// already selected neighbour than to the query is dropped.
	sort.Slice(candidates, func(i, j int) bool {
		return hnswCandidateLess(candidates[i].dist, candidates[i].pkSpan, candidates[j].dist, candidates[j].pkSpan)
	})

	// Only apply the heuristic for metrics satisfying triangle inequality.
	// Matches Java: if (metric.satisfiesTriangleInequality()) { ... }
	if !g.config.Metric.satisfiesTriangleInequality() {
		return candidates[:min(maxConn, len(candidates))]
	}

	var result []hnswCandidate
	var discarded []hnswCandidate

	for i := range candidates {
		if len(result) >= maxConn {
			break
		}

		// Lazily decode vector for heuristic pairwise distance.
		if candidates[i].vec == nil && candidates[i].vector.data != nil {
			var decErr error
			candidates[i].vec, decErr = g.decodeVector(candidates[i].vector)
			if decErr != nil {
				continue
			}
		}

		// Check if candidate is closer to query than to any already-selected neighbor.
		shouldSelect := true
		for j := range result {
			if result[j].vec == nil && result[j].vector.data != nil {
				var decErr error
				result[j].vec, decErr = g.decodeVector(result[j].vector)
				if decErr != nil {
					continue
				}
			}
			distToSelected := g.pairDistance(candidates[i].vec, candidates[i].vector, result[j].vec, result[j].vector)
			if distToSelected < candidates[i].dist {
				shouldSelect = false
				break
			}
		}

		if shouldSelect {
			result = append(result, candidates[i])
		} else if g.config.KeepPrunedConnections {
			discarded = append(discarded, candidates[i])
		}
	}

	// Fill with pruned connections if keepPrunedConnections is enabled.
	if g.config.KeepPrunedConnections {
		for _, d := range discarded {
			if len(result) >= maxConn {
				break
			}
			result = append(result, d)
		}
	}

	return result
}

// selectNeighborsHeuristic extends the candidate set with 2nd-degree neighbors
// (when ExtendCandidates is true), then applies the heuristic selection.
// This is the full Algorithm 4 from the HNSW paper.
// Matches Java's Primitives.extendCandidatesIfNecessary() + selectCandidates().
func (g *hnswGraph) selectNeighborsHeuristic(tx fdb.ReadTransaction, query []float64, candidates []hnswCandidate, maxConn, layer int) ([]hnswCandidate, error) {
	working := make([]hnswCandidate, len(candidates))
	copy(working, candidates)

	// Extend with 2nd-degree neighbors.
	seen := make(map[string]bool, len(working))
	for _, c := range working {
		seen[string(c.pkSpan)] = true
	}

	// Collect all unseen 2nd-degree neighbor spans.
	var toFetch [][]byte
	for _, c := range candidates {
		cPK, derr := decodeNestedPK(c.pkSpan)
		if derr != nil {
			return nil, derr
		}
		_, neighbors, err := g.storage.loadNodeLayerDispatch(tx, layer, cPK)
		if err != nil {
			if e := hnswFatal(err); e != nil {
				return nil, e
			}
			continue
		}
		for _, nbSpan := range neighbors {
			key := string(nbSpan)
			if seen[key] {
				continue
			}
			seen[key] = true
			toFetch = append(toFetch, nbSpan)
		}
	}

	// Batch-fetch all 2nd-degree neighbors at once.
	batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, toFetch)
	for _, r := range batchResults {
		if r.err != nil {
			if e := hnswFatal(r.err); e != nil {
				return nil, e
			}
			continue
		}
		dist := g.computeDistance(query, r.vecBytes)
		working = append(working, hnswCandidate{pkSpan: r.span, vector: hnswVector{data: r.vecBytes}, dist: dist})
	}

	return g.selectNeighbors(working, maxConn), nil
}

// pruneNeighbors re-selects the best maxConn neighbors for a node by computing distances,
// with selectNeighbors' heuristic over the list (Primitives.pruneNeighborsIfNecessary).
func (g *hnswGraph) pruneNeighbors(tx fdb.ReadTransaction, nodeVec []float64, nodeVector hnswVector, neighborPKs []tuple.Tuple, maxConn, layer int) ([]tuple.Tuple, error) {
	// Bring the (bounded) neighbor PKs into span form for the batch dispatch.
	spans := make([][]byte, len(neighborPKs))
	for i, pk := range neighborPKs {
		spans[i] = nestPK(pk)
	}
	var candidates []hnswCandidate
	batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, spans)
	for _, r := range batchResults {
		if r.err != nil {
			if e := hnswFatal(r.err); e != nil {
				return nil, e
			}
			continue
		}
		neighbor := hnswVector{data: r.vecBytes}
		nbVec, derr := g.decodeVector(neighbor)
		if derr != nil {
			return nil, derr
		}
		dist := g.pairDistance(nbVec, neighbor, nodeVec, nodeVector)
		candidates = append(candidates, hnswCandidate{pkSpan: r.span, vector: neighbor, vec: nbVec, dist: dist})
	}

	// Never extended: Java's pruneNeighborsIfNecessary selects among the list
	// alone (extendCandidatesIfNecessary runs only for a new node's own
	// neighbours, Insert.java:510).
	selected := g.selectNeighbors(candidates, maxConn)

	// The kept neighbours stay in their list order: Java resolves the
	// selection as a change set over the list (resolveChangeSetFromNewNeighbors),
	// deleting the dropped ones; the selection order is not stored.
	keep := make(map[string]bool, len(selected))
	for _, s := range selected {
		keep[string(s.pkSpan)] = true
	}
	result := make([]tuple.Tuple, 0, len(selected))
	for i, pk := range neighborPKs {
		if keep[string(spans[i])] {
			result = append(result, pk)
		}
	}
	return result, nil
}

// hnswSearchResult is a search result.
type hnswSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
	Vector     []byte
}

// --- Heap for search candidates ---

// distHeap is a min-heap for search candidates.
type distHeap []distItem

type distItem struct {
	pkSpan  []byte // nested-encoded PK span
	dist    float64
	spanStr string // cached string(pkSpan) for the visited-set / dedup
}

func (h distHeap) Len() int           { return len(h) }
func (h distHeap) Less(i, j int) bool { return h[i].less(h[j]) }
func (h distHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *distHeap) Push(x any)        { *h = append(*h, x.(distItem)) }
func (h *distHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// hnswDistLess orders distances as Java's NodeReferenceWithDistance comparator
// does (Comparator.comparing over a boxed Double, i.e. Double.compare): a NaN
// distance, which a cosine over non-finite components yields, is the farthest,
// and -0.0 precedes 0.0.
func hnswDistLess(a, b float64) bool { return compareFloat64Java(a, b) < 0 }

func (d distItem) less(o distItem) bool { return hnswCandidateLess(d.dist, d.pkSpan, o.dist, o.pkSpan) }

// hnswCandidateLess is NodeReferenceWithDistance.comparator in full: distance
// by Double.compare, then primary key (a nested-encoded span sorts as its
// tuple does).
func hnswCandidateLess(aDist float64, aPK []byte, bDist float64, bPK []byte) bool {
	if c := compareFloat64Java(aDist, bDist); c != 0 {
		return c < 0
	}
	return bytes.Compare(aPK, bPK) < 0
}
