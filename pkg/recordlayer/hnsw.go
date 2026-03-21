package recordlayer

// Write locks (Java's LockIdentifier / LockRegistry) are NOT needed in Go.
//
// Java's VectorIndexMaintainer wraps HNSW insert/delete in doWithWriteLock()
// and scans in acquireReadLock(). These are purely in-memory, in-process async
// read/write locks (ConcurrentHashMap<LockIdentifier, AsyncLock>) that live on
// FDBRecordContext. They coordinate concurrent CompletableFuture chains sharing
// the same FDB transaction — e.g., pipelined updateIndexAsync calls that could
// otherwise interleave reads and writes to the same HNSW partition subspace.
//
// This problem does not exist in Go:
//   - Go's FDB bindings are synchronous. Each Insert/Delete/Search call runs
//     sequentially on the same fdb.Transaction. There are no concurrent async
//     futures within a single transaction.
//   - Cross-transaction correctness is handled by FDB's serializable isolation.
//     Concurrent transactions modifying the same HNSW nodes conflict on shared
//     keys, FDB aborts one, and the retry loop re-executes. Insert is idempotent
//     (checks existence first), so retries are safe.
//
// In short: Java needs the lock because of its async concurrency model within a
// single transaction. Go's synchronous model eliminates the problem entirely.

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// HNSWConfig configures an HNSW graph.
// Matches Java's com.apple.foundationdb.async.hnsw.Config.
type HNSWConfig struct {
	NumDimensions         int
	M                     int          // Connectivity factor (default 16)
	MMax                  int          // Max connections for non-zero layers (default M)
	MMax0                 int          // Max connections for layer 0 (default 2*M)
	EfConstruction        int          // Insertion search factor (default 200)
	Metric                VectorMetric // Distance metric
	ExtendCandidates      bool         // Extend candidate set with 2nd-degree neighbors (default false)
	KeepPrunedConnections bool         // Retain pruned candidates to fill up to M (default false)
	EfRepair              int          // Max candidates for delete repair (default 64, 0 = no limit)
	UseRaBitQ             bool         // Enable RaBitQ quantization for approximate distance (default false)
	RaBitQNumExBits       int          // Bits per dimension for quantization (1-8, default 4)

	// Concurrency limits — Java uses these to limit async pipeline parallelism.
	// Go's synchronous FDB model doesn't use these for concurrency control, but
	// we store and round-trip them so Java-written configs are preserved.
	// Matches Java's Config.maxNumConcurrentNodeFetches etc.
	MaxNumConcurrentNodeFetches          int // (0, 64], default 16 — node fetch parallelism in Java
	MaxNumConcurrentNeighborhoodFetches  int // (0, 20], default 10 — neighborhood fetch parallelism in Java
	MaxNumConcurrentDeleteFromLayer      int // (0, 10], default 2  — layer deletion parallelism in Java
}

// ValidateHNSWConfig validates the HNSW configuration.
// Matches Java's Config validation.
func ValidateHNSWConfig(c HNSWConfig) error {
	if c.NumDimensions < 1 {
		return fmt.Errorf("hnsw: numDimensions must be >= 1, got %d", c.NumDimensions)
	}
	if c.M < 4 || c.M > 200 {
		return fmt.Errorf("hnsw: m must be in [4, 200], got %d", c.M)
	}
	if c.MMax < 4 || c.MMax > 200 {
		return fmt.Errorf("hnsw: mMax must be in [4, 200], got %d", c.MMax)
	}
	if c.MMax0 < 4 || c.MMax0 > 300 {
		return fmt.Errorf("hnsw: mMax0 must be in [4, 300], got %d", c.MMax0)
	}
	if c.EfConstruction < 100 || c.EfConstruction > 400 {
		return fmt.Errorf("hnsw: efConstruction must be in [100, 400], got %d", c.EfConstruction)
	}
	return nil
}

// DefaultHNSWConfig returns a default HNSW configuration.
func DefaultHNSWConfig(numDimensions int) HNSWConfig {
	return HNSWConfig{
		NumDimensions:                       numDimensions,
		M:                                   16,
		MMax:                                16,
		MMax0:                               32,
		EfConstruction:                      200,
		EfRepair:                            64,
		Metric:                              VectorMetricEuclidean,
		RaBitQNumExBits:                     4,
		MaxNumConcurrentNodeFetches:         16,
		MaxNumConcurrentNeighborhoodFetches: 10,
		MaxNumConcurrentDeleteFromLayer:     2,
	}
}

// hnswGraph is an HNSW (Hierarchical Navigable Small World) graph stored in FDB.
// Wire-compatible with Java's com.apple.foundationdb.async.hnsw.HNSW.
type hnswGraph struct {
	storage *hnswStorage
	config  HNSWConfig
}

// NewHNSWGraph creates a new HNSW graph.
func NewHNSWGraph(storage *hnswStorage, config HNSWConfig) *hnswGraph {
	return &hnswGraph{
		storage: storage,
		config:  config,
	}
}

// encodeVectorBytes returns the bytes to store for a vector.
// When UseRaBitQ is enabled, returns RaBitQ-encoded bytes; otherwise returns
// the raw DOUBLE-serialized bytes.
func (g *hnswGraph) encodeVectorBytes(vector []float64) []byte {
	if g.config.UseRaBitQ {
		numExBits := g.config.RaBitQNumExBits
		if numExBits < 1 || numExBits > 8 {
			numExBits = 4
		}
		q := NewRaBitQuantizer(g.config.Metric, numExBits)
		return q.Encode(vector).ToBytes()
	}
	return serializeVector(vector)
}

// computeDistance computes the distance between a raw query vector and stored
// vector bytes. When the stored bytes are RaBitQ-encoded (type ordinal 3),
// uses the RaBitEstimator for fast approximate distance. Otherwise
// deserializes and computes exact distance.
func (g *hnswGraph) computeDistance(query []float64, storedVecBytes []byte) float64 {
	if len(storedVecBytes) > 0 && storedVecBytes[0] == vectorTypeRABITQ {
		numExBits := g.config.RaBitQNumExBits
		if numExBits < 1 || numExBits > 8 {
			numExBits = 4
		}
		encoded, err := EncodedVectorFromBytes(storedVecBytes, g.config.NumDimensions, numExBits)
		if err != nil {
			return math.Inf(1)
		}
		est := NewRaBitEstimator(g.config.Metric, numExBits)
		return est.Distance(query, encoded)
	}
	vec, err := deserializeVector(storedVecBytes)
	if err != nil {
		return math.Inf(1)
	}
	return vectorDistance(query, vec, g.config.Metric)
}

// decodeStoredVector extracts an approximate []float64 from stored vector bytes.
// For raw vectors (DOUBLE/FLOAT/HALF), this is exact deserialization.
// For RaBitQ-encoded vectors, this reconstructs an approximate vector from
// the quantized representation, suitable for pairwise distance computations
// in the neighbor selection heuristic.
func (g *hnswGraph) decodeStoredVector(storedVecBytes []byte) ([]float64, error) {
	if len(storedVecBytes) == 0 {
		return nil, fmt.Errorf("hnsw: empty vector bytes")
	}
	if storedVecBytes[0] != vectorTypeRABITQ {
		return deserializeVector(storedVecBytes)
	}

	numExBits := g.config.RaBitQNumExBits
	if numExBits < 1 || numExBits > 8 {
		numExBits = 4
	}
	encoded, err := EncodedVectorFromBytes(storedVecBytes, g.config.NumDimensions, numExBits)
	if err != nil {
		return nil, err
	}

	// Reconstruct approximate vector: un-center the quantized codes.
	// code[i] = signedLevel + sign * 2^numExBits, centered around cb.
	// xuc[i] = code[i] - cb approximates the direction of the original vector.
	// Scale by sqrt(fAddEx) / ||xuc|| to approximate the original magnitude.
	cb := float64(int(1)<<numExBits) - 0.5
	dims := encoded.NumDimensions()
	xuc := make([]float64, dims)
	var xucNormSqr float64
	for i := 0; i < dims; i++ {
		xuc[i] = float64(encoded.Encoded[i]) - cb
		xucNormSqr += xuc[i] * xuc[i]
	}

	// Scale to approximate original norm (sqrt(fAddEx) = ||original||).
	origNorm := math.Sqrt(encoded.FAddEx)
	xucNorm := math.Sqrt(xucNormSqr)
	if xucNorm > 0 && origNorm > 0 {
		scale := origNorm / xucNorm
		for i := range xuc {
			xuc[i] *= scale
		}
	}

	return xuc, nil
}

// Insert adds a vector to the HNSW graph.
// primaryKey identifies the record. vector is the float64 vector to index.
// Wire-compatible with Java's HNSW insert (COMPACT node format, deterministic layer assignment).
func (g *hnswGraph) Insert(tx fdb.Transaction, primaryKey tuple.Tuple, vector []float64) error {
	vecBytes := g.encodeVectorBytes(vector)

	// Fire both existence check and access info read as parallel futures.
	existKey := g.storage.dataSubspace.Pack(tuple.Tuple{int64(0), primaryKey})
	existFuture := tx.Get(fdb.Key(existKey))
	accessKey := g.storage.accessSubspace.Pack(tuple.Tuple{})
	accessFuture := tx.Get(fdb.Key(accessKey))

	// Resolve existence check.
	existData, _ := existFuture.Get()
	if existData != nil {
		// Node exists — populate cache with parsed data, delete and re-insert.
		if vb, nb, parseErr := parseNodeValue(existData); parseErr == nil {
			g.storage.cache[string(existKey)] = &parsedNode{vecBytes: vb, neighbors: nb}
		}
		if delErr := g.Delete(tx, primaryKey); delErr != nil {
			return delErr
		}
		// Re-read access info after delete (may have changed entry point).
		accessFuture = tx.Get(fdb.Key(accessKey))
	}

	// Determine insertion layer (deterministic per PK).
	insertLayer := topLayer(primaryKey, g.config.M)

	// Resolve access info.
	accessData, _ := accessFuture.Get()
	epLayer, epPK, epVecBytes, epErr := g.storage.parseAccessInfo(accessData)

	if epErr != nil {
		// No entry point — this is the first node.
		// Save node at all layers from 0 to insertLayer.
		for layer := 0; layer <= insertLayer; layer++ {
			g.storage.saveNodeLayer(tx, layer, primaryKey, vecBytes, nil)
		}
		g.storage.saveAccessInfo(tx, insertLayer, primaryKey, vecBytes)
		return nil
	}

	// Preload upper layers used in greedy descent (few nodes each).
	for layer := epLayer; layer > insertLayer && layer > 0; layer-- {
		if preloadErr := g.storage.preloadLayer(tx, layer); preloadErr != nil {
			return preloadErr
		}
	}

	// Greedy search from top to insertion layer.
	var err error
	currentPK := epPK
	currentVecBytes := epVecBytes
	for layer := epLayer; layer > insertLayer; layer-- {
		currentPK, currentVecBytes, err = g.searchLayerGreedy(tx, vector, currentPK, currentVecBytes, layer)
		if err != nil {
			return err
		}
	}

	// Insert at each layer from min(insertLayer, epLayer) down to 0.
	for layer := min(insertLayer, epLayer); layer >= 0; layer-- {
		// Find ef_construction nearest neighbors at this layer.
		neighbors, err := g.searchLayerMulti(tx, vector, currentPK, currentVecBytes, g.config.EfConstruction, layer)
		if err != nil {
			return err
		}

		// Select M best neighbors.
		maxConn := g.config.MMax
		if layer == 0 {
			maxConn = g.config.MMax0
		}
		var selected []hnswCandidate
		if g.config.ExtendCandidates {
			selected, err = g.selectNeighborsHeuristic(tx, vector, neighbors, maxConn, layer)
			if err != nil {
				return err
			}
		} else {
			selected = g.selectNeighbors(neighbors, maxConn)
		}

		// Build neighbor list for the new node at this layer.
		newNodeNeighbors := make([]tuple.Tuple, len(selected))
		for i, nb := range selected {
			newNodeNeighbors[i] = nb.pk
		}

		// Save new node at this layer.
		g.storage.saveNodeLayer(tx, layer, primaryKey, vecBytes, newNodeNeighbors)

		// Add reverse connections to each neighbor.
		for _, nb := range selected {
			nbVecBytes, nbNeighbors, loadErr := g.storage.loadNodeLayer(tx, layer, nb.pk)
			if loadErr != nil {
				continue
			}

			// Add the new node as a neighbor.
			nbNeighbors = append(nbNeighbors, primaryKey)

			// Prune if over limit.
			if len(nbNeighbors) > maxConn {
				nbVec, _ := g.decodeStoredVector(nbVecBytes)
				nbNeighbors, err = g.pruneNeighbors(tx, nbVec, nbNeighbors, maxConn, layer)
				if err != nil {
					return err
				}
			}

			g.storage.saveNodeLayer(tx, layer, nb.pk, nbVecBytes, nbNeighbors)
		}

		if len(neighbors) > 0 {
			// Use nearest neighbor as entry point for next layer down.
			currentPK = neighbors[0].pk
			currentVecBytes = neighbors[0].vecBytes
		}
	}

	// If new node has layers above the entry point, save those layers too.
	if insertLayer > epLayer {
		for layer := epLayer + 1; layer <= insertLayer; layer++ {
			g.storage.saveNodeLayer(tx, layer, primaryKey, vecBytes, nil)
		}
		g.storage.saveAccessInfo(tx, insertLayer, primaryKey, vecBytes)
	}

	return nil
}

// Delete removes a node from the HNSW graph and repairs neighbor connections.
// Wire-compatible with Java's HNSW delete (graph repair, entry point update).
//
// Optimization: pipelines the initial layer reads across all layers at once,
// reducing sequential round-trips from O(topLvl) to O(1) for the read phase.
// Repairs are still sequential (each needs neighbor data from FDB).
func (g *hnswGraph) Delete(tx fdb.Transaction, primaryKey tuple.Tuple) error {
	topLvl := topLayer(primaryKey, g.config.M)

	// Pipeline: fire all layer reads at once via loadNodeLayerBatch-style approach.
	// Build keys for all layers and fire Get() futures before resolving any.
	type layerFuture struct {
		layer  int
		key    string
		future fdb.FutureByteSlice
	}
	var futures []layerFuture

	for layer := 0; layer <= topLvl; layer++ {
		key := g.storage.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
		cacheKey := string(key)
		if _, ok := g.storage.cache[cacheKey]; ok {
			continue // already cached, no need to fetch
		}
		futures = append(futures, layerFuture{
			layer:  layer,
			key:    cacheKey,
			future: tx.Get(fdb.Key(key)),
		})
	}

	// Resolve all futures — FDB pipelines these reads into ~1 round-trip.
	for _, f := range futures {
		data, err := f.future.Get()
		if err != nil {
			continue
		}
		if data == nil {
			g.storage.cache[f.key] = nil // negative cache
			continue
		}
		vecBytes, neighbors, parseErr := parseNodeValue(data)
		if parseErr != nil {
			continue
		}
		g.storage.cache[f.key] = &parsedNode{vecBytes: vecBytes, neighbors: neighbors}
	}

	// Check existence at layer 0 (now served from cache).
	_, _, err := g.storage.loadNodeLayer(tx, 0, primaryKey)
	if err != nil {
		return nil // already deleted or doesn't exist
	}

	// Delete from all layers and repair (reads now come from cache).
	for layer := 0; layer <= topLvl; layer++ {
		_, neighbors, loadErr := g.storage.loadNodeLayer(tx, layer, primaryKey)
		if loadErr != nil {
			continue // not present at this layer
		}

		g.storage.deleteNodeLayer(tx, layer, primaryKey)

		// Repair: for each neighbor, reconnect.
		for _, nbPK := range neighbors {
			if repairErr := g.repairNeighbor(tx, layer, nbPK, primaryKey); repairErr != nil {
				return repairErr
			}
		}
	}

	// Update entry point if needed.
	epLayer, epPK, _, epErr := g.storage.loadAccessInfo(tx)
	if epErr != nil {
		return nil // no entry point
	}
	if tupleEqual(epPK, primaryKey) {
		// Find a replacement entry point by scanning for any remaining node,
		// starting from the highest layer and working down.
		var newEntryPK tuple.Tuple
		var newEntryVecBytes []byte
		newEntryLayer := 0

		for layer := epLayer; layer >= 0 && newEntryPK == nil; layer-- {
			// Find any node at this layer.
			foundPK, foundVec, scanErr := g.storage.findAnyNodeAtLayer(tx, layer)
			if scanErr == nil && foundPK != nil {
				newEntryPK = foundPK
				newEntryVecBytes = foundVec
				newEntryLayer = layer
			}
		}

		if newEntryPK != nil {
			g.storage.saveAccessInfo(tx, newEntryLayer, newEntryPK, newEntryVecBytes)
		} else {
			g.storage.clearAccessInfo(tx)
		}
	}

	return nil
}

// Search finds the k nearest neighbors to the query vector.
// Returns results sorted by distance (closest first).
func (g *hnswGraph) Search(tx fdb.ReadTransaction, query []float64, k, efSearch int) ([]hnswSearchResult, error) {
	epLayer, epPK, epVecBytes, err := g.storage.loadAccessInfo(tx)
	if err != nil {
		return nil, nil // empty graph
	}

	// Preload upper layers (few nodes each) in one GetRange per layer.
	// This replaces ~2 round-trips per greedy iteration with zero after preload.
	for layer := epLayer; layer > 0; layer-- {
		if preloadErr := g.storage.preloadLayer(tx, layer); preloadErr != nil {
			return nil, preloadErr
		}
	}

	// Greedy descent from top layer to layer 1.
	currentPK := epPK
	currentVecBytes := epVecBytes
	for layer := epLayer; layer > 0; layer-- {
		currentPK, currentVecBytes, err = g.searchLayerGreedy(tx, query, currentPK, currentVecBytes, layer)
		if err != nil {
			return nil, err
		}
	}

	// Search at layer 0 with efSearch.
	candidates, err := g.searchLayerMulti(tx, query, currentPK, currentVecBytes, max(efSearch, k), 0)
	if err != nil {
		return nil, err
	}

	// Return top-k.
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	results := make([]hnswSearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = hnswSearchResult{
			PrimaryKey: c.pk,
			Distance:   c.dist,
		}
	}
	return results, nil
}

// searchLayerGreedy finds the single nearest neighbor at a given layer (greedy descent).
// Returns the best PK, its stored vector bytes, and error.
func (g *hnswGraph) searchLayerGreedy(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVecBytes []byte, layer int) (tuple.Tuple, []byte, error) {
	bestPK := epPK
	bestVecBytes := epVecBytes
	bestDist := g.computeDistance(query, epVecBytes)
	changed := true

	for changed {
		changed = false
		_, neighbors, err := g.storage.loadNodeLayer(tx, layer, bestPK)
		if err != nil {
			break // no data at this layer
		}

		// Batch-fetch all neighbor vectors in one round-trip.
		batchResults := g.storage.loadNodeLayerBatch(tx, layer, neighbors)
		for _, r := range batchResults {
			if r.err != nil {
				continue
			}
			dist := g.computeDistance(query, r.vecBytes)
			if dist < bestDist {
				bestDist = dist
				bestPK = r.pk
				bestVecBytes = r.vecBytes
				changed = true
			}
		}
	}

	return bestPK, bestVecBytes, nil
}

// hnswCandidate represents a search candidate with its primary key, vector bytes, and distance.
type hnswCandidate struct {
	pk       tuple.Tuple
	vecBytes []byte    // stored vector bytes (raw or RaBitQ-encoded)
	vec      []float64 // decoded vector (lazy, for heuristic pairwise distances)
	dist     float64
}

// searchLayerMulti finds the ef nearest neighbors at a given layer.
func (g *hnswGraph) searchLayerMulti(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVecBytes []byte, ef, layer int) ([]hnswCandidate, error) {
	if epPK == nil {
		return nil, nil
	}

	epDist := g.computeDistance(query, epVecBytes)
	epPKBytes := string(epPK.Pack())

	// Candidates (min-heap by distance) and visited set.
	candidates := &distHeap{}
	heap.Push(candidates, distItem{pk: epPK, dist: epDist, pkBytes: epPKBytes})

	visited := make(map[string]bool, ef*2)
	visited[epPKBytes] = true

	// Pre-allocate results to expected capacity (ef).
	results := make([]hnswCandidate, 1, ef)
	results[0] = hnswCandidate{pk: epPK, vecBytes: epVecBytes, dist: epDist}

	// Pre-allocate toFetch to typical neighbor count (M).
	toFetch := make([]tuple.Tuple, 0, g.config.M)

	for candidates.Len() > 0 {
		closest := heap.Pop(candidates).(distItem)

		// Check if we've explored enough.
		if len(results) >= ef {
			farthestResult := results[len(results)-1].dist
			if closest.dist > farthestResult {
				break
			}
		}

		// Load the node's neighbors at this layer.
		_, neighbors, err := g.storage.loadNodeLayer(tx, layer, closest.pk)
		if err != nil {
			continue
		}

		// Collect unvisited neighbors — reuse toFetch slice.
		toFetch = toFetch[:0]
		for _, nbPK := range neighbors {
			key := string(nbPK.Pack())
			if visited[key] {
				continue
			}
			visited[key] = true
			toFetch = append(toFetch, nbPK)
		}

		// Batch-read all unvisited neighbors at once.
		batchResults := g.storage.loadNodeLayerBatch(tx, layer, toFetch)
		for _, r := range batchResults {
			if r.err != nil {
				continue
			}
			dist := g.computeDistance(query, r.vecBytes)

			if len(results) < ef || dist < results[len(results)-1].dist {
				heap.Push(candidates, distItem{pk: r.pk, dist: dist, pkBytes: r.pkBytes})
				// Binary-search insertion into sorted results (O(log n) find + O(n) shift).
				c := hnswCandidate{pk: r.pk, vecBytes: r.vecBytes, dist: dist}
				pos := sort.Search(len(results), func(i int) bool {
					return results[i].dist > dist
				})
				results = append(results, hnswCandidate{})
				copy(results[pos+1:], results[pos:])
				results[pos] = c
				if len(results) > ef {
					results = results[:ef]
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
	if len(candidates) <= maxConn {
		return candidates
	}

	// Sort candidates by distance (ascending).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].dist < candidates[j].dist
	})

	// Only apply the heuristic for metrics satisfying triangle inequality.
	// Matches Java: if (metric.satisfiesTriangleInequality()) { ... }
	if !g.config.Metric.satisfiesTriangleInequality() {
		return candidates[:maxConn]
	}

	var result []hnswCandidate
	var discarded []hnswCandidate

	for i := range candidates {
		if len(result) >= maxConn {
			break
		}

		// Lazily decode vector for heuristic pairwise distance.
		if candidates[i].vec == nil && candidates[i].vecBytes != nil {
			candidates[i].vec, _ = g.decodeStoredVector(candidates[i].vecBytes)
		}

		// Check if candidate is closer to query than to any already-selected neighbor.
		shouldSelect := true
		for j := range result {
			if result[j].vec == nil && result[j].vecBytes != nil {
				result[j].vec, _ = g.decodeStoredVector(result[j].vecBytes)
			}
			distToSelected := vectorDistance(candidates[i].vec, result[j].vec, g.config.Metric)
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
		seen[string(c.pk.Pack())] = true
	}

	// Collect all unseen 2nd-degree neighbor PKs.
	var toFetch []tuple.Tuple
	for _, c := range candidates {
		_, neighbors, err := g.storage.loadNodeLayer(tx, layer, c.pk)
		if err != nil {
			continue
		}
		for _, nbPK := range neighbors {
			key := string(nbPK.Pack())
			if seen[key] {
				continue
			}
			seen[key] = true
			toFetch = append(toFetch, nbPK)
		}
	}

	// Batch-fetch all 2nd-degree neighbors at once.
	batchResults := g.storage.loadNodeLayerBatch(tx, layer, toFetch)
	for _, r := range batchResults {
		if r.err != nil {
			continue
		}
		dist := g.computeDistance(query, r.vecBytes)
		working = append(working, hnswCandidate{pk: r.pk, vecBytes: r.vecBytes, dist: dist})
	}

	return g.selectNeighbors(working, maxConn), nil
}

// pruneNeighbors re-selects the best maxConn neighbors for a node by computing distances.
// Uses the same heuristic as selectNeighbors (with optional extendCandidates).
func (g *hnswGraph) pruneNeighbors(tx fdb.ReadTransaction, nodeVec []float64, neighborPKs []tuple.Tuple, maxConn, layer int) ([]tuple.Tuple, error) {
	var candidates []hnswCandidate
	batchResults := g.storage.loadNodeLayerBatch(tx, layer, neighborPKs)
	for _, r := range batchResults {
		if r.err != nil {
			continue
		}
		dist := g.computeDistance(nodeVec, r.vecBytes)
		candidates = append(candidates, hnswCandidate{pk: r.pk, vecBytes: r.vecBytes, dist: dist})
	}

	var selected []hnswCandidate
	if g.config.ExtendCandidates {
		var err error
		selected, err = g.selectNeighborsHeuristic(tx, nodeVec, candidates, maxConn, layer)
		if err != nil {
			return nil, err
		}
	} else {
		selected = g.selectNeighbors(candidates, maxConn)
	}

	result := make([]tuple.Tuple, len(selected))
	for i, s := range selected {
		result[i] = s.pk
	}
	return result, nil
}

// repairNeighbor removes a deleted node from a neighbor's list and finds replacement connections.
func (g *hnswGraph) repairNeighbor(tx fdb.Transaction, layer int, neighborPK, deletedPK tuple.Tuple) error {
	nbVecBytes, nbNeighbors, err := g.storage.loadNodeLayer(tx, layer, neighborPK)
	if err != nil {
		return nil // neighbor doesn't exist
	}

	// Remove deleted node from neighbor's list.
	filtered := make([]tuple.Tuple, 0, len(nbNeighbors))
	for _, pk := range nbNeighbors {
		if !tupleEqual(pk, deletedPK) {
			filtered = append(filtered, pk)
		}
	}

	// Batch-load filtered neighbors to get their neighbor lists.
	filteredBatch := g.storage.loadNodeLayerBatch(tx, layer, filtered)

	// Find candidates from neighbors-of-neighbors.
	candidateMap := make(map[string]tuple.Tuple)
	for _, r := range filteredBatch {
		if r.err != nil {
			continue
		}
		for _, candidate := range r.neighbors {
			candidateKey := string(candidate.Pack())
			if !tupleEqual(candidate, neighborPK) && !tupleEqual(candidate, deletedPK) {
				candidateMap[candidateKey] = candidate
			}
		}
	}

	// Build candidate list with distances.
	nbVec, _ := g.decodeStoredVector(nbVecBytes)

	// Start with existing (filtered) neighbors (already in cache from batch above).
	seen := make(map[string]bool)
	var allCandidates []hnswCandidate
	for _, r := range filteredBatch {
		if r.err != nil {
			continue
		}
		candidateVec, _ := g.decodeStoredVector(r.vecBytes)
		dist := vectorDistance(nbVec, candidateVec, g.config.Metric)
		allCandidates = append(allCandidates, hnswCandidate{pk: r.pk, vecBytes: r.vecBytes, vec: candidateVec, dist: dist})
		seen[string(r.pk.Pack())] = true
	}

	// Collect unseen candidates from neighbors-of-neighbors.
	var newCandidatePKs []tuple.Tuple
	for key, pk := range candidateMap {
		if seen[key] {
			continue
		}
		newCandidatePKs = append(newCandidatePKs, pk)
	}

	// Sample down new candidates if efRepair is set (matches Java's
	// shouldUseSecondaryCandidateForRepair() which limits repair exploration).
	if g.config.EfRepair > 0 && len(newCandidatePKs) > g.config.EfRepair {
		rand.Shuffle(len(newCandidatePKs), func(i, j int) {
			newCandidatePKs[i], newCandidatePKs[j] = newCandidatePKs[j], newCandidatePKs[i]
		})
		newCandidatePKs = newCandidatePKs[:g.config.EfRepair]
	}

	// Batch-fetch new candidates.
	newBatch := g.storage.loadNodeLayerBatch(tx, layer, newCandidatePKs)
	for _, r := range newBatch {
		if r.err != nil {
			continue
		}
		candidateVec, _ := g.decodeStoredVector(r.vecBytes)
		dist := vectorDistance(nbVec, candidateVec, g.config.Metric)
		allCandidates = append(allCandidates, hnswCandidate{pk: r.pk, vecBytes: r.vecBytes, vec: candidateVec, dist: dist})
	}

	// Select best connections using the heuristic.
	maxConn := g.config.MMax
	if layer == 0 {
		maxConn = g.config.MMax0
	}

	selected := g.selectNeighbors(allCandidates, maxConn)

	newNeighbors := make([]tuple.Tuple, len(selected))
	for i, c := range selected {
		newNeighbors[i] = c.pk
	}

	g.storage.saveNodeLayer(tx, layer, neighborPK, nbVecBytes, newNeighbors)
	return nil
}

// hnswSearchResult is a search result.
type hnswSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
}

// parsedNode holds pre-parsed node data to avoid repeated tuple.Unpack on cache hits.
type parsedNode struct {
	vecBytes  []byte
	neighbors []tuple.Tuple
}

// hnswStorage handles FDB storage of HNSW graph nodes.
// Wire-compatible with Java's COMPACT node format.
//
// Subspace layout:
//
//	Sub(0) — data: (layer, primaryKey) -> (nodeKind, vectorTuple, neighborsTuple)
//	Sub(1) — access info: () -> (entryPointLayer, entryPointPK, entryPointVectorTuple)
type hnswStorage struct {
	dataSubspace   subspace.Subspace
	accessSubspace subspace.Subspace
	config         HNSWConfig
	cache          map[string]*parsedNode // FDB key → parsed node (nil = not found)
}

func newHNSWStorage(ss subspace.Subspace, config HNSWConfig) *hnswStorage {
	return &hnswStorage{
		dataSubspace:   ss.Sub(int64(0)),
		accessSubspace: ss.Sub(int64(1)),
		config:         config,
		cache:          make(map[string]*parsedNode),
	}
}

// saveNodeLayer writes one layer's data for a node in COMPACT format.
// Key: dataSubspace.Pack(Tuple{layer, primaryKey})  (PK as nested tuple, matching Java)
// Value: Tuple.Pack(nodeKind, vectorTuple, neighborsTuple)
func (s *hnswStorage) saveNodeLayer(tx fdb.Transaction, layer int, primaryKey tuple.Tuple, vectorBytes []byte, neighbors []tuple.Tuple) {
	// Java uses Tuple.from(layer, primaryKey) where primaryKey is nested.
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})

	neighborList := make(tuple.Tuple, len(neighbors))
	for i, pk := range neighbors {
		neighborList[i] = pk
	}

	value := tuple.Tuple{
		int64(0),                  // COMPACT node kind
		tuple.Tuple{vectorBytes},  // vector bytes wrapped in tuple
		neighborList,              // neighbor PKs as nested tuples
	}
	tx.Set(fdb.Key(key), value.Pack())

	// Update cache with parsed data so subsequent reads skip tuple.Unpack.
	s.cache[string(key)] = &parsedNode{vecBytes: vectorBytes, neighbors: neighbors}
}

// parseNodeValue parses the raw FDB value bytes for a node into vector bytes
// and neighbor PKs. Factored out of loadNodeLayer for reuse by batch loading.
func parseNodeValue(data []byte) (vectorBytes []byte, neighbors []tuple.Tuple, err error) {
	t, err := tuple.Unpack(data)
	if err != nil {
		return nil, nil, fmt.Errorf("hnsw: unpack node layer: %w", err)
	}
	if len(t) < 3 {
		return nil, nil, fmt.Errorf("hnsw: node tuple too short: %d elements", len(t))
	}

	// t[0] = nodeKind (int64, 0 = COMPACT)
	// t[1] = vectorTuple (tuple containing vector bytes)
	// t[2] = neighborsTuple (tuple of neighbor PK tuples)

	switch vt := t[1].(type) {
	case tuple.Tuple:
		if len(vt) > 0 {
			if vb, ok := vt[0].([]byte); ok {
				vectorBytes = vb
			}
		}
	}

	switch nt := t[2].(type) {
	case tuple.Tuple:
		neighbors = make([]tuple.Tuple, 0, len(nt))
		for _, elem := range nt {
			if pkTuple, ok := elem.(tuple.Tuple); ok {
				neighbors = append(neighbors, pkTuple)
			}
		}
	}

	return vectorBytes, neighbors, nil
}

// loadNodeLayer reads one layer's data for a node.
// Returns vector bytes, neighbor PKs, and error (non-nil if not found).
// Uses the per-transaction cache to avoid re-reading the same node.
func (s *hnswStorage) loadNodeLayer(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) (vectorBytes []byte, neighbors []tuple.Tuple, err error) {
	// Java uses Tuple.from(layer, primaryKey) where primaryKey is nested.
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	cacheKey := string(key)

	// Check cache first — returns pre-parsed data, no tuple.Unpack needed.
	if cached, ok := s.cache[cacheKey]; ok {
		if cached == nil {
			return nil, nil, fmt.Errorf("hnsw: node not found at layer %d", layer)
		}
		return cached.vecBytes, cached.neighbors, nil
	}

	data, err := tx.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, nil, fmt.Errorf("hnsw: get node layer %d: %w", layer, err)
	}
	if data == nil {
		s.cache[cacheKey] = nil // cache negative result
		return nil, nil, fmt.Errorf("hnsw: node not found at layer %d", layer)
	}

	// Parse and cache the result.
	vectorBytes, neighbors, err = parseNodeValue(data)
	if err != nil {
		return nil, nil, err
	}
	s.cache[cacheKey] = &parsedNode{vecBytes: vectorBytes, neighbors: neighbors}
	return vectorBytes, neighbors, nil
}

// nodeResult holds the result of loading one node from FDB.
type nodeResult struct {
	pk        tuple.Tuple
	pkBytes   string // cached pk.Pack() to avoid repeated allocation
	vecBytes  []byte
	neighbors []tuple.Tuple
	err       error
}

// loadNodeLayerBatch fires all FDB Get() calls for the given PKs at once,
// then resolves them. FDB pipelines the reads, turning N sequential round-trips
// into 1 round-trip with N pipelined reads. Populates the per-transaction cache.
func (s *hnswStorage) loadNodeLayerBatch(tx fdb.ReadTransaction, layer int, pks []tuple.Tuple) []nodeResult {
	results := make([]nodeResult, len(pks))
	type pending struct {
		idx    int
		key    string
		future fdb.FutureByteSlice
	}
	var toFetch []pending

	for i, pk := range pks {
		results[i].pk = pk
		results[i].pkBytes = string(pk.Pack())
		key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), pk})
		cacheKey := string(key)

		if cached, ok := s.cache[cacheKey]; ok {
			if cached == nil {
				results[i].err = fmt.Errorf("hnsw: node not found at layer %d", layer)
			} else {
				results[i].vecBytes = cached.vecBytes
				results[i].neighbors = cached.neighbors
			}
			continue
		}

		// Fire the get — don't resolve yet.
		toFetch = append(toFetch, pending{idx: i, key: cacheKey, future: tx.Get(fdb.Key(key))})
	}

	// Resolve all outstanding futures. FDB pipelines these reads.
	for _, p := range toFetch {
		data, err := p.future.Get()
		if err != nil {
			results[p.idx].err = fmt.Errorf("hnsw: get node layer %d: %w", layer, err)
			continue
		}
		if data == nil {
			s.cache[p.key] = nil // cache negative result
			results[p.idx].err = fmt.Errorf("hnsw: node not found at layer %d", layer)
			continue
		}
		vecBytes, neighbors, parseErr := parseNodeValue(data)
		if parseErr != nil {
			results[p.idx].err = parseErr
			continue
		}
		s.cache[p.key] = &parsedNode{vecBytes: vecBytes, neighbors: neighbors}
		results[p.idx].vecBytes = vecBytes
		results[p.idx].neighbors = neighbors
	}

	return results
}

// preloadLayer reads ALL nodes at the given layer in a single GetRange call
// and populates the cache. For upper layers (layer > 0) with few nodes, this
// replaces multiple individual Get() round-trips with one range scan.
// For a 1K-node graph with M=16: layer 2 has ~4 nodes, layer 1 has ~62 nodes.
func (s *hnswStorage) preloadLayer(tx fdb.ReadTransaction, layer int) error {
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return fmt.Errorf("hnsw: preload layer %d prefix range: %w", layer, err)
	}
	iter := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return fmt.Errorf("hnsw: preload layer %d get kv: %w", layer, err)
		}
		cacheKey := string(kv.Key)
		if _, ok := s.cache[cacheKey]; ok {
			continue // already cached (e.g. from a save earlier in this tx)
		}
		vecBytes, neighbors, parseErr := parseNodeValue(kv.Value)
		if parseErr != nil {
			continue // skip unparseable entries
		}
		s.cache[cacheKey] = &parsedNode{vecBytes: vecBytes, neighbors: neighbors}
	}
	return nil
}

// deleteNodeLayer removes one layer's data for a node.
func (s *hnswStorage) deleteNodeLayer(tx fdb.Transaction, layer int, primaryKey tuple.Tuple) {
	// Java uses Tuple.from(layer, primaryKey) where primaryKey is nested.
	key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	tx.Clear(fdb.Key(key))

	// Mark as deleted in cache (nil *parsedNode = negative cache entry).
	s.cache[string(key)] = nil
}

// parseAccessInfo parses the entry point metadata from raw bytes.
// Returns layer, primary key, vector bytes, and error (non-nil if no entry point).
func (s *hnswStorage) parseAccessInfo(data []byte) (layer int, pk tuple.Tuple, vectorBytes []byte, err error) {
	if data == nil {
		return 0, nil, nil, fmt.Errorf("hnsw: no entry point")
	}

	t, unpackErr := tuple.Unpack(data)
	if unpackErr != nil {
		return 0, nil, nil, fmt.Errorf("hnsw: unpack access info: %w", unpackErr)
	}
	if len(t) < 3 {
		return 0, nil, nil, fmt.Errorf("hnsw: access info too short: %d elements", len(t))
	}

	// t[0] = entryPointLayer
	// t[1] = entryPointPK (tuple)
	// t[2] = entryPointVectorTuple (tuple containing vector bytes)

	if l, ok := asInt64(t[0]); ok {
		layer = int(l)
	}

	switch pkVal := t[1].(type) {
	case tuple.Tuple:
		pk = pkVal
	}

	switch vt := t[2].(type) {
	case tuple.Tuple:
		if len(vt) > 0 {
			if vb, ok := vt[0].([]byte); ok {
				vectorBytes = vb
			}
		}
	}

	if pk == nil {
		return 0, nil, nil, fmt.Errorf("hnsw: access info has nil PK")
	}

	return layer, pk, vectorBytes, nil
}

// loadAccessInfo reads the entry point metadata from FDB.
// Returns layer, primary key, vector bytes, and error (non-nil if no entry point).
func (s *hnswStorage) loadAccessInfo(tx fdb.ReadTransaction) (layer int, pk tuple.Tuple, vectorBytes []byte, err error) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	data, getErr := tx.Get(fdb.Key(key)).Get()
	if getErr != nil {
		return 0, nil, nil, fmt.Errorf("hnsw: get access info: %w", getErr)
	}
	return s.parseAccessInfo(data)
}

// saveAccessInfo writes the entry point metadata.
// Wire-compatible with Java's StorageAdapter.writeAccessInfo:
// Tuple.from(layer, primaryKey, vectorTuple, rotatorSeed, centroidOrNull)
func (s *hnswStorage) saveAccessInfo(tx fdb.Transaction, layer int, pk tuple.Tuple, vectorBytes []byte) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	value := tuple.Tuple{
		int64(layer),
		pk,
		tuple.Tuple{vectorBytes},
		int64(0), // rotatorSeed (default 0)
		nil,      // negatedCentroid (null = not computed)
	}
	tx.Set(fdb.Key(key), value.Pack())
}

// clearAccessInfo removes the entry point metadata.
func (s *hnswStorage) clearAccessInfo(tx fdb.Transaction) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	tx.Clear(fdb.Key(key))
}

// findAnyNodeAtLayer finds any node present at the given layer.
// Returns the first node found by scanning the layer prefix.
func (s *hnswStorage) findAnyNodeAtLayer(tx fdb.ReadTransaction, layer int) (pk tuple.Tuple, vectorBytes []byte, err error) {
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	r, rangeErr := fdb.PrefixRange(prefix)
	if rangeErr != nil {
		return nil, nil, rangeErr
	}

	ri := tx.GetRange(r, fdb.RangeOptions{Limit: 1}).Iterator()
	if ri.Advance() {
		kv, getErr := ri.Get()
		if getErr != nil {
			return nil, nil, getErr
		}

		// Extract primary key from the FDB key.
		// Key format: dataSubspace + (layer, nestedPK)
		keyTuple, unpackErr := s.dataSubspace.Unpack(kv.Key)
		if unpackErr != nil {
			return nil, nil, unpackErr
		}
		// keyTuple[0] = layer, keyTuple[1] = primaryKey (nested tuple)
		if len(keyTuple) > 1 {
			if pkTuple, ok := keyTuple[1].(tuple.Tuple); ok {
				pk = pkTuple
			}
		}

		// Parse the value for vector bytes.
		t, valErr := tuple.Unpack(kv.Value)
		if valErr != nil {
			return nil, nil, valErr
		}
		if len(t) >= 2 {
			if vt, ok := t[1].(tuple.Tuple); ok && len(vt) > 0 {
				if vb, ok := vt[0].([]byte); ok {
					vectorBytes = vb
				}
			}
		}

		return pk, vectorBytes, nil
	}

	return nil, nil, fmt.Errorf("hnsw: no nodes at layer %d", layer)
}

// clearAll removes all HNSW graph data (data + access info).
func (s *hnswStorage) clearAll(tx fdb.Transaction) {
	for _, ss := range []subspace.Subspace{s.dataSubspace, s.accessSubspace} {
		r, err := fdb.PrefixRange(ss.Bytes())
		if err != nil {
			continue
		}
		tx.ClearRange(r)
	}
}

// --- Vector serialization ---
// Wire-compatible with Java's VectorType.DOUBLE.

// serializeVector serializes a float64 vector to bytes.
// Format: byte 0 = type ordinal (0 for DOUBLE), bytes 1+ = big-endian IEEE 754 float64 values.
func serializeVector(vec []float64) []byte {
	buf := make([]byte, 1+8*len(vec))
	buf[0] = 2 // DOUBLE type ordinal — Java VectorType.DOUBLE.ordinal() = 2
	for i, v := range vec {
		binary.BigEndian.PutUint64(buf[1+i*8:], math.Float64bits(v))
	}
	return buf
}

// deserializeVector deserializes a vector from bytes, returning float64 values.
// Supports all three Java RealVector types (ordinals match Java's VectorType enum):
//   - Type 0: HALF (16-bit IEEE 754, 2 bytes per component)
//   - Type 1: SINGLE/FLOAT (32-bit IEEE 754, 4 bytes per component)
//   - Type 2: DOUBLE (64-bit IEEE 754, 8 bytes per component)
func deserializeVector(data []byte) ([]float64, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("hnsw: empty vector data")
	}
	typeOrdinal := data[0]
	payload := data[1:]

	switch typeOrdinal {
	case 0: // HALF (float16) — Java VectorType.HALF.ordinal() = 0
		numFloats := len(payload) / 2
		vec := make([]float64, numFloats)
		for i := 0; i < numFloats; i++ {
			bits := binary.BigEndian.Uint16(payload[i*2:])
			vec[i] = float64(halfToFloat32(bits))
		}
		return vec, nil
	case 1: // SINGLE (float32) — Java VectorType.SINGLE.ordinal() = 1
		numFloats := len(payload) / 4
		vec := make([]float64, numFloats)
		for i := 0; i < numFloats; i++ {
			vec[i] = float64(math.Float32frombits(binary.BigEndian.Uint32(payload[i*4:])))
		}
		return vec, nil
	case 2: // DOUBLE (float64) — Java VectorType.DOUBLE.ordinal() = 2
		numFloats := len(payload) / 8
		vec := make([]float64, numFloats)
		for i := 0; i < numFloats; i++ {
			vec[i] = math.Float64frombits(binary.BigEndian.Uint64(payload[i*8:]))
		}
		return vec, nil
	case 3: // RABITQ
		return nil, fmt.Errorf("hnsw: RaBitQ vectors must be decoded via EncodedVectorFromBytes, not deserializeVector")
	default:
		return nil, fmt.Errorf("hnsw: unsupported vector type ordinal %d", typeOrdinal)
	}
}

// halfToFloat32 converts an IEEE 754 half-precision (16-bit) float to float32.
func halfToFloat32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h & 0x3ff)

	switch {
	case exp == 0: // subnormal or zero
		if frac == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal: normalize
		for frac&0x400 == 0 {
			frac <<= 1
			exp--
		}
		exp++
		frac &= 0x3ff
		return math.Float32frombits(sign | ((exp + 112) << 23) | (frac << 13))
	case exp == 0x1f: // Inf or NaN
		if frac == 0 {
			return math.Float32frombits(sign | 0x7f800000)
		}
		return math.Float32frombits(sign | 0x7f800000 | (frac << 13))
	default: // normalized
		return math.Float32frombits(sign | ((exp + 112) << 23) | (frac << 13))
	}
}

// --- Layer assignment ---
// Wire-compatible with Java's deterministic layer assignment using SplitMix64 hash of PK.

// topLayer computes the deterministic layer assignment for a given primary key.
// Uses the HNSW level generation formula with SplitMix64 hash of the PK's Java-compatible hashCode.
func topLayer(primaryKey tuple.Tuple, m int) int {
	packed := primaryKey.Pack()
	h := javaHashCode(packed)
	u := 1.0 - splitMixDouble(int64(h))
	lambda := 1.0 / math.Log(float64(m))
	level := int(math.Floor(-math.Log(u) * lambda))
	if level < 0 {
		level = 0
	}
	return level
}

// splitMixLong applies the SplitMix64 hash function.
// Matches Java's SplittableRandom.mix64.
func splitMixLong(x int64) int64 {
	x += -0x61C8864680B583EB // 0x9e3779b97f4a7c15 as signed int64
	x = (x ^ int64(uint64(x)>>30)) * -0x40A7B892E31B1A47 // 0xbf58476d1ce4e5b9
	x = (x ^ int64(uint64(x)>>27)) * -0x6B2FB644ECCEEE15 // 0x94d049bb133111eb
	x = x ^ int64(uint64(x)>>31)
	return x
}

// splitMixDouble converts a SplitMix64 hash to a double in [0, 1).
func splitMixDouble(x int64) float64 {
	return float64(uint64(splitMixLong(x))>>11) * (1.0 / float64(int64(1)<<53))
}

// javaHashCode computes a Java-compatible hash code for byte array.
// Matches: int hash = 1; for (byte b : data) hash = 31 * hash + b;
// where b is treated as signed (int8).
func javaHashCode(data []byte) int32 {
	var hash int32 = 1
	for _, b := range data {
		hash = 31*hash + int32(int8(b))
	}
	return hash
}

// --- Heap for search candidates ---

// distHeap is a min-heap for search candidates.
type distHeap []distItem

type distItem struct {
	pk      tuple.Tuple
	dist    float64
	pkBytes string // cached pk.Pack() to avoid repeated allocation
}

func (h distHeap) Len() int            { return len(h) }
func (h distHeap) Less(i, j int) bool  { return h[i].dist < h[j].dist }
func (h distHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *distHeap) Push(x any)         { *h = append(*h, x.(distItem)) }
func (h *distHeap) Pop() any           { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
