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

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// VectorQuantizer is an optional quantization strategy for HNSW vector storage.
// When set on HNSWConfig, HNSW uses it for encoding, distance estimation, and
// vector reconstruction instead of raw float64 storage.
//
// This interface allows quantization implementations (like RaBitQ) to live in
// separate packages, matching Java's architecture where RaBitQ is in fdb-extensions.
type VectorQuantizer interface {
	// Encode quantizes a float64 vector into compact bytes for storage.
	Encode(vector []float64) []byte

	// Distance estimates the distance between a raw query vector and stored
	// quantized bytes. Returns the estimated distance.
	Distance(query []float64, storedBytes []byte, numDimensions int) (float64, error)

	// Decode reconstructs an approximate float64 vector from stored quantized bytes.
	// Used for pairwise distance in the neighbor selection heuristic.
	Decode(storedBytes []byte, numDimensions int) ([]float64, error)

	// GetTypeByte returns the type ordinal byte used as the first byte of encoded data.
	// This is used to dispatch between raw and quantized storage.
	GetTypeByte() byte
}

// HNSWConfig configures an HNSW graph.
// Matches Java's com.apple.foundationdb.async.hnsw.Config.
type HNSWConfig struct {
	NumDimensions         int
	M                     int             // Connectivity factor (default 16)
	MMax                  int             // Max connections for non-zero layers (default M)
	MMax0                 int             // Max connections for layer 0 (default 2*M)
	EfConstruction        int             // Insertion search factor (default 200)
	Metric                VectorMetric    // Distance metric
	ExtendCandidates      bool            // Extend candidate set with 2nd-degree neighbors (default false)
	KeepPrunedConnections bool            // Retain pruned candidates to fill up to M (default false)
	EfRepair              int             // Max candidates for delete repair (default 64, 0 = no limit)
	UseInlining           bool            // Use inlining storage for layers > 0 (default false)
	Quantizer             VectorQuantizer // Optional quantizer (nil = raw float64 storage)

	// Concurrency limits — Java uses these to limit async pipeline parallelism.
	// Go's synchronous FDB model doesn't use these for concurrency control, but
	// we store and round-trip them so Java-written configs are preserved.
	// Matches Java's Config.maxNumConcurrentNodeFetches etc.
	MaxNumConcurrentNodeFetches         int // (0, 64], default 16 — node fetch parallelism in Java
	MaxNumConcurrentNeighborhoodFetches int // (0, 20], default 10 — neighborhood fetch parallelism in Java
	MaxNumConcurrentDeleteFromLayer     int // (0, 10], default 2  — layer deletion parallelism in Java
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

// SetStats attaches I/O counters for profiling. Pass nil to disable.
func (g *hnswGraph) SetStats(stats *HNSWStats) {
	g.storage.stats = stats
}

// buildTransform creates a transform from access info and the current config.
// Returns nil if no transform is needed (either no quantizer, or no centroid yet for Euclidean).
func (g *hnswGraph) buildTransform(info *hnswAccessInfo) *hnswTransform {
	if info == nil || !info.hasTransform() {
		return nil
	}
	normalize := g.config.Metric == VectorMetricCosine
	return newHNSWTransform(info.rotatorSeed, info.centroid, g.config.NumDimensions, normalize)
}

// encodeVectorBytes returns the bytes to store for a vector.
// When a quantizer is configured and a transform is active, the caller must apply the
// transform before calling this. Returns quantized bytes or raw DOUBLE-serialized bytes.
func (g *hnswGraph) encodeVectorBytes(vector []float64) []byte {
	if g.config.Quantizer != nil {
		return g.config.Quantizer.Encode(vector)
	}
	return serializeVector(vector)
}

// computeDistance computes the distance between a raw query vector and stored
// vector bytes. When a quantizer is configured and the stored bytes match its
// type byte, uses the quantizer for fast approximate distance. Otherwise
// deserializes and computes exact distance.
func (g *hnswGraph) computeDistance(query []float64, storedVecBytes []byte) float64 {
	if g.config.Quantizer != nil && len(storedVecBytes) > 0 && storedVecBytes[0] == g.config.Quantizer.GetTypeByte() {
		dist, err := g.config.Quantizer.Distance(query, storedVecBytes, g.config.NumDimensions)
		if err != nil {
			return math.Inf(1)
		}
		return dist
	}
	vec, err := deserializeVector(storedVecBytes)
	if err != nil {
		return math.Inf(1)
	}
	return vectorDistance(query, vec, g.config.Metric)
}

// decodeStoredVector extracts an approximate []float64 from stored vector bytes.
// For raw vectors (DOUBLE/FLOAT/HALF), this is exact deserialization.
// For quantized vectors, delegates to the configured quantizer's Decode method
// to reconstruct an approximate vector for pairwise distance computations
// in the neighbor selection heuristic.
func (g *hnswGraph) decodeStoredVector(storedVecBytes []byte) ([]float64, error) {
	if len(storedVecBytes) == 0 {
		return nil, fmt.Errorf("hnsw: empty vector bytes")
	}
	if g.config.Quantizer != nil && storedVecBytes[0] == g.config.Quantizer.GetTypeByte() {
		return g.config.Quantizer.Decode(storedVecBytes, g.config.NumDimensions)
	}
	return deserializeVector(storedVecBytes)
}

// Insert adds a vector to the HNSW graph.
// primaryKey identifies the record. vector is the float64 vector to index.
// Wire-compatible with Java's HNSW insert (compact + inlining node formats,
// deterministic layer assignment, FHT-KAC rotation for RaBitQ).
func (g *hnswGraph) Insert(tx fdb.Transaction, primaryKey tuple.Tuple, vector []float64) error {
	// Fire both existence check and access info read as parallel futures.
	// Existence check uses layer 0 (always compact format).
	existKey := g.storage.dataSubspace.Pack(tuple.Tuple{int64(0), primaryKey})
	existFuture := tx.Get(fdb.Key(existKey))
	accessKey := g.storage.accessSubspace.Pack(tuple.Tuple{})
	accessFuture := tx.Get(fdb.Key(accessKey))

	// Resolve existence check.
	existData, existErr := existFuture.Get()
	if existErr != nil {
		return fmt.Errorf("hnsw insert: existence check: %w", existErr)
	}
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
	accessData, accessErr := accessFuture.Get()
	if accessErr != nil {
		return fmt.Errorf("hnsw insert: access info read: %w", accessErr)
	}
	accessInfo, epErr := g.storage.parseAccessInfo(accessData)

	if epErr != nil {
		// No entry point — first node in the graph.
		return g.firstInsert(tx, primaryKey, vector, insertLayer)
	}

	// Build transform from access info (nil if no rotation configured).
	transform := g.buildTransform(accessInfo)

	// Transform vector for storage and search.
	// All vectors in the graph are stored in the transformed coordinate system.
	queryVec := vector
	if transform != nil {
		queryVec = transform.apply(vector)
	}
	vecBytes := g.encodeVectorBytes(queryVec)

	epLayer := accessInfo.layer
	epPK := accessInfo.pk
	epVecBytes := accessInfo.vectorBytes

	// Preload upper layers used in greedy descent (few nodes each).
	for layer := epLayer; layer > insertLayer && layer > 0; layer-- {
		if preloadErr := g.storage.preloadLayerDispatch(tx, layer); preloadErr != nil {
			return preloadErr
		}
	}

	// Greedy search from top to insertion layer.
	// Search uses the transformed query vector.
	var err error
	currentPK := epPK
	currentVecBytes := epVecBytes
	for layer := epLayer; layer > insertLayer; layer-- {
		currentPK, currentVecBytes, err = g.searchLayerGreedy(tx, queryVec, currentPK, currentVecBytes, layer)
		if err != nil {
			return err
		}
	}

	// Insert at each layer from min(insertLayer, epLayer) down to 0.
	for layer := min(insertLayer, epLayer); layer >= 0; layer-- {
		// Find ef_construction nearest neighbors at this layer.
		neighbors, err := g.searchLayerMulti(tx, queryVec, currentPK, currentVecBytes, g.config.EfConstruction, layer)
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
			selected, err = g.selectNeighborsHeuristic(tx, queryVec, neighbors, maxConn, layer)
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
		g.storage.saveNodeLayerDispatch(tx, layer, primaryKey, vecBytes, newNodeNeighbors)

		// Add reverse connections to each neighbor.
		for _, nb := range selected {
			nbVecBytes, nbNeighbors, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, nb.pk)
			if loadErr != nil {
				return fmt.Errorf("hnsw insert: load neighbor %v at layer %d for reverse connection: %w", nb.pk, layer, loadErr)
			}

			// Add the new node as a neighbor.
			nbNeighbors = append(nbNeighbors, primaryKey)

			// Prune if over limit.
			if len(nbNeighbors) > maxConn {
				// Resolve vector bytes (at inlining layers, own vector is at layer 0).
				resolvedVec := g.storage.resolveVectorBytes(layer, nb.pk, nbVecBytes)
				nbVec, decErr := g.decodeStoredVector(resolvedVec)
				if decErr != nil {
					return fmt.Errorf("hnsw insert: decode neighbor %v vector at layer %d for pruning: %w", nb.pk, layer, decErr)
				}
				nbNeighbors, err = g.pruneNeighbors(tx, nbVec, nbNeighbors, maxConn, layer)
				if err != nil {
					return err
				}
			}

			g.storage.saveNodeLayerDispatch(tx, layer, nb.pk, nbVecBytes, nbNeighbors)
		}

		if len(neighbors) > 0 {
			// Use nearest neighbor as entry point for next layer down.
			currentPK = neighbors[0].pk
			currentVecBytes = neighbors[0].vecBytes
		}
	}

	// If new node has layers above the entry point, save those layers too
	// and update the entry point.
	if insertLayer > epLayer {
		for layer := epLayer + 1; layer <= insertLayer; layer++ {
			g.storage.saveNodeLayerDispatch(tx, layer, primaryKey, vecBytes, nil)
		}
		accessInfo.layer = insertLayer
		accessInfo.pk = primaryKey
		accessInfo.vectorBytes = vecBytes
		g.storage.saveAccessInfo(tx, accessInfo)
	}

	return nil
}

// firstInsert handles the first-ever insertion into the graph.
// When a quantizer is enabled and the metric doesn't preserve translation (Cosine/DotProduct),
// initializes the FHT-KAC rotator immediately with a zero centroid.
// Matches Java's Insert.firstInsert().
func (g *hnswGraph) firstInsert(tx fdb.Transaction, primaryKey tuple.Tuple, vector []float64, insertLayer int) error {
	info := &hnswAccessInfo{
		layer:       insertLayer,
		pk:          primaryKey,
		rotatorSeed: -1,
	}

	queryVec := vector

	if g.config.Quantizer != nil && !g.config.Metric.satisfiesPreservedUnderTranslation() {
		// Cosine/DotProduct: activate rotation immediately.
		// Generate deterministic seed from primary key, matching Java's:
		//   SplittableRandom random = new SplittableRandom(splitMixLong(pk.hashCode()));
		//   rotatorSeed = random.nextLong();
		// SplittableRandom.nextLong() for the first call = splitMixLong(initialSeed)
		packed := primaryKey.Pack()
		h := javaHashCode(packed)
		initialSeed := splitMixLong(int64(h))
		info.rotatorSeed = splitMixLong(initialSeed)

		// Zero centroid = no translation, rotation only.
		info.centroid = make([]float64, g.config.NumDimensions)

		// Apply transform before encoding.
		transform := g.buildTransform(info)
		if transform != nil {
			queryVec = transform.apply(vector)
		}
	}

	vecBytes := g.encodeVectorBytes(queryVec)
	info.vectorBytes = vecBytes

	for layer := 0; layer <= insertLayer; layer++ {
		g.storage.saveNodeLayerDispatch(tx, layer, primaryKey, vecBytes, nil)
	}
	g.storage.saveAccessInfo(tx, info)
	return nil
}

// Delete removes a node from the HNSW graph and repairs neighbor connections.
// Wire-compatible with Java's HNSW delete (graph repair, entry point update).
//
// Optimization: for compact layers, pipelines the initial reads across all layers
// at once, reducing sequential round-trips from O(topLvl) to O(1) for the read phase.
// For inlining layers, uses preloadLayerDispatch (already done during search/insert).
// Repairs are still sequential (each needs neighbor data from FDB).
func (g *hnswGraph) Delete(tx fdb.Transaction, primaryKey tuple.Tuple) error {
	// Load access info first to get the max layer in the graph.
	// We scan from epLayer down to 0 to find where the node actually exists,
	// rather than computing topLayer from PK hash (which would be wrong if M
	// changed or the node was inserted with a different M).
	accessInfo, accessErr := g.storage.loadAccessInfo(tx)
	if accessErr != nil {
		return nil // empty graph, nothing to delete
	}
	epLayer := accessInfo.layer

	// Pipeline: fire all compact-layer reads at once.
	// Inlining layers use range reads via loadNodeLayerDispatch.
	type layerFuture struct {
		layer  int
		key    string
		future fdb.FutureByteSlice
	}
	var futures []layerFuture

	for layer := 0; layer <= epLayer; layer++ {
		if g.storage.isInliningLayer(layer) {
			continue // inlining layers use range reads, not single-key gets
		}
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

	// Resolve all compact-layer futures — FDB pipelines these reads into ~1 round-trip.
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

	// Check existence at layer 0 (always compact, now served from cache).
	_, _, err := g.storage.loadNodeLayer(tx, 0, primaryKey)
	if err != nil {
		return nil // already deleted or doesn't exist
	}

	// Delete from all layers and repair.
	for layer := 0; layer <= epLayer; layer++ {
		_, neighbors, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, primaryKey)
		if loadErr != nil {
			continue // not present at this layer
		}

		g.storage.deleteNodeLayerDispatch(tx, layer, primaryKey)

		// Repair: for each neighbor, reconnect.
		for _, nbPK := range neighbors {
			if repairErr := g.repairNeighbor(tx, layer, nbPK, primaryKey); repairErr != nil {
				return repairErr
			}
		}
	}

	// Update entry point if needed.
	if tupleEqual(accessInfo.pk, primaryKey) {
		// Find a replacement entry point by scanning for any remaining node,
		// starting from the highest layer and working down.
		var newEntryPK tuple.Tuple
		var newEntryVecBytes []byte
		newEntryLayer := 0

		for layer := accessInfo.layer; layer >= 0 && newEntryPK == nil; layer-- {
			// Find any node at this layer.
			foundPK, foundVec, scanErr := g.storage.findAnyNodeAtLayerDispatch(tx, layer)
			if scanErr == nil && foundPK != nil {
				newEntryPK = foundPK
				newEntryVecBytes = foundVec
				newEntryLayer = layer
			}
		}

		if newEntryPK != nil {
			// Preserve transform state (rotatorSeed, centroid) when updating entry point.
			accessInfo.layer = newEntryLayer
			accessInfo.pk = newEntryPK
			accessInfo.vectorBytes = newEntryVecBytes
			g.storage.saveAccessInfo(tx, accessInfo)
		} else {
			g.storage.clearAccessInfo(tx)
		}
	}

	return nil
}

// Search finds the k nearest neighbors to the query vector.
// Returns results sorted by distance (closest first).
func (g *hnswGraph) Search(tx fdb.ReadTransaction, query []float64, k, efSearch int) ([]hnswSearchResult, error) {
	accessInfo, err := g.storage.loadAccessInfo(tx)
	if err != nil {
		return nil, nil // empty graph
	}

	// Apply transform to query vector so it's in the same coordinate system
	// as the stored vectors. Matches Java's StorageTransform.transform(queryVector).
	transform := g.buildTransform(accessInfo)
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
	currentVecBytes := accessInfo.vectorBytes
	for layer := accessInfo.layer; layer > 0; layer-- {
		currentPK, currentVecBytes, err = g.searchLayerGreedy(tx, searchQuery, currentPK, currentVecBytes, layer)
		if err != nil {
			return nil, err
		}
	}

	// Search at layer 0 with efSearch.
	candidates, err := g.searchLayerMulti(tx, searchQuery, currentPK, currentVecBytes, max(efSearch, k), 0)
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
		_, neighbors, err := g.storage.loadNodeLayerDispatch(tx, layer, bestPK)
		if err != nil {
			break // no data at this layer
		}

		// Batch-fetch all neighbor vectors.
		// For inlining layers, neighbor vectors are already cached from the range read.
		batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, neighbors)
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

// hnswPrefetchCandidates is the number of candidates to pop from the heap
// per iteration and fetch edge lists for in parallel. Reduces serial FDB
// round-trips from N to N/prefetch. 4 balances I/O pipelining against
// over-fetching (popping candidates that would have been pruned).
const hnswPrefetchCandidates = 4

// searchLayerMulti finds the ef nearest neighbors at a given layer.
// Uses parallel candidate prefetching: pops up to hnswPrefetchCandidates
// from the heap per iteration, issues all edge-list reads as pipelined FDB
// futures, then batch-fetches all unvisited neighbor vectors.
func (g *hnswGraph) searchLayerMulti(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVecBytes []byte, ef, layer int) ([]hnswCandidate, error) {
	if epPK == nil {
		return nil, nil
	}

	epDist := g.computeDistance(query, epVecBytes)
	epPKBytes := string(epPK.Pack())

	// Candidates (min-heap by distance) and visited set.
	backing := make(distHeap, 0, ef)
	candidates := &backing
	heap.Push(candidates, distItem{pk: epPK, dist: epDist, pkBytes: epPKBytes})

	visited := make(map[string]bool, ef*2)
	visited[epPKBytes] = true

	// Pre-allocate results to expected capacity (ef).
	results := make([]hnswCandidate, 1, ef)
	results[0] = hnswCandidate{pk: epPK, vecBytes: epVecBytes, dist: epDist}

	// Pre-allocate buffers reused across iterations.
	poppedPKs := make([]tuple.Tuple, 0, hnswPrefetchCandidates)
	toFetch := make([]tuple.Tuple, 0, g.config.M*hnswPrefetchCandidates)

	for candidates.Len() > 0 {
		// Pop up to hnswPrefetchCandidates from the heap.
		poppedPKs = poppedPKs[:0]
		done := false
		for candidates.Len() > 0 && len(poppedPKs) < hnswPrefetchCandidates {
			closest := heap.Pop(candidates).(distItem)

			// Early termination: if closest unprocessed candidate is farther
			// than our worst result, all remaining candidates are too.
			if len(results) >= ef && closest.dist > results[len(results)-1].dist {
				done = true
				break
			}
			poppedPKs = append(poppedPKs, closest.pk)
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
				continue
			}
			for _, nbPK := range el.neighbors {
				key := string(nbPK.Pack())
				if visited[key] {
					continue
				}
				visited[key] = true
				toFetch = append(toFetch, nbPK)
			}
		}

		// Batch-read all unvisited neighbor vectors at once.
		// For inlining layers, neighbor vectors are already cached from the range read.
		batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, toFetch)
		for _, r := range batchResults {
			if r.err != nil {
				continue
			}
			dist := g.computeDistance(query, r.vecBytes)

			if len(results) < ef || dist < results[len(results)-1].dist {
				// r.pkBytes is already computed by loadNodeLayerBatch — no double Pack().
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
			var decErr error
			candidates[i].vec, decErr = g.decodeStoredVector(candidates[i].vecBytes)
			if decErr != nil {
				continue
			}
		}

		// Check if candidate is closer to query than to any already-selected neighbor.
		shouldSelect := true
		for j := range result {
			if result[j].vec == nil && result[j].vecBytes != nil {
				var decErr error
				result[j].vec, decErr = g.decodeStoredVector(result[j].vecBytes)
				if decErr != nil {
					continue
				}
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
		_, neighbors, err := g.storage.loadNodeLayerDispatch(tx, layer, c.pk)
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
	batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, toFetch)
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
	batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, neighborPKs)
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
	nbVecBytes, nbNeighbors, err := g.storage.loadNodeLayerDispatch(tx, layer, neighborPK)
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
	filteredBatch := g.storage.loadNodeLayerBatchDispatch(tx, layer, filtered)

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
	// For inlining layers, own vector is at layer 0.
	resolvedVec := g.storage.resolveVectorBytes(layer, neighborPK, nbVecBytes)
	nbVec, decErr := g.decodeStoredVector(resolvedVec)
	if decErr != nil {
		return fmt.Errorf("hnsw repair: decode neighbor %v vector at layer %d: %w", neighborPK, layer, decErr)
	}

	// Start with existing (filtered) neighbors (already in cache from batch above).
	seen := make(map[string]bool)
	var allCandidates []hnswCandidate
	for _, r := range filteredBatch {
		if r.err != nil {
			continue
		}
		resolvedCandidateVec := g.storage.resolveVectorBytes(layer, r.pk, r.vecBytes)
		candidateVec, decErr := g.decodeStoredVector(resolvedCandidateVec)
		if decErr != nil {
			continue
		}
		dist := vectorDistance(nbVec, candidateVec, g.config.Metric)
		allCandidates = append(allCandidates, hnswCandidate{pk: r.pk, vecBytes: resolvedCandidateVec, vec: candidateVec, dist: dist})
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
	newBatch := g.storage.loadNodeLayerBatchDispatch(tx, layer, newCandidatePKs)
	for _, r := range newBatch {
		if r.err != nil {
			continue
		}
		resolvedCandidateVec := g.storage.resolveVectorBytes(layer, r.pk, r.vecBytes)
		candidateVec, decErr := g.decodeStoredVector(resolvedCandidateVec)
		if decErr != nil {
			continue
		}
		dist := vectorDistance(nbVec, candidateVec, g.config.Metric)
		allCandidates = append(allCandidates, hnswCandidate{pk: r.pk, vecBytes: resolvedCandidateVec, vec: candidateVec, dist: dist})
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

	g.storage.saveNodeLayerDispatch(tx, layer, neighborPK, nbVecBytes, newNeighbors)
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
	dataSubspace   subspace.Subspace
	accessSubspace subspace.Subspace
	config         HNSWConfig
	cache          map[string]*parsedNode // FDB key → parsed node (nil = not found)
	stats          *HNSWStats             // optional I/O counters (nil = no tracking)
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
		int64(0),                 // COMPACT node kind
		tuple.Tuple{vectorBytes}, // vector bytes wrapped in tuple
		neighborList,             // neighbor PKs as nested tuples
	}
	tx.Set(fdb.Key(key), value.Pack())

	// Update cache with parsed data so subsequent reads skip tuple.Unpack.
	s.cache[string(key)] = &parsedNode{vecBytes: vectorBytes, neighbors: neighbors}
}

// parseNodeValue parses the raw FDB value bytes for a node into vector bytes
// and neighbor PKs. Factored out of loadNodeLayer for reuse by batch loading.
func parseNodeValue(data []byte) (vectorBytes []byte, neighbors []tuple.Tuple, err error) {
	t, err := fastUnpack(data)
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
		hnswStatCacheHit(s.stats)
		if cached == nil {
			return nil, nil, fmt.Errorf("hnsw: node not found at layer %d", layer)
		}
		return cached.vecBytes, cached.neighbors, nil
	}

	hnswStatGet(s.stats)
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
			hnswStatCacheHit(s.stats)
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

// edgeListResult holds the neighbors from an edge-list fetch.
type edgeListResult struct {
	neighbors []tuple.Tuple
	err       error
}

// loadEdgeListsBatch fetches edge lists for multiple nodes in parallel.
// For non-inlining layers, issues all FDB Gets as futures before resolving any,
// reducing serial round-trips from N to 1. For inlining layers (cached from
// preload), falls back to sequential cache lookups (already fast, no I/O).
func (s *hnswStorage) loadEdgeListsBatch(tx fdb.ReadTransaction, layer int, pks []tuple.Tuple) []edgeListResult {
	results := make([]edgeListResult, len(pks))

	if s.isInliningLayer(layer) {
		// Inlining layers: data cached from preload, no I/O to parallelize.
		for i, pk := range pks {
			_, neighbors, err := s.loadNodeLayerInlining(tx, layer, pk)
			results[i] = edgeListResult{neighbors: neighbors, err: err}
		}
		return results
	}

	// Non-inlining: issue all Gets as futures, then resolve.
	type pending struct {
		idx      int
		cacheKey string
		future   fdb.FutureByteSlice
	}
	var toFetch []pending

	for i, pk := range pks {
		key := s.dataSubspace.Pack(tuple.Tuple{int64(layer), pk})
		cacheKey := string(key)

		if cached, ok := s.cache[cacheKey]; ok {
			hnswStatCacheHit(s.stats)
			if cached == nil {
				results[i].err = fmt.Errorf("hnsw: node not found at layer %d", layer)
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
		if data == nil {
			s.cache[p.cacheKey] = nil
			results[p.idx].err = fmt.Errorf("hnsw: node not found at layer %d", layer)
			continue
		}
		vecBytes, neighbors, parseErr := parseNodeValue(data)
		if parseErr != nil {
			results[p.idx].err = parseErr
			continue
		}
		s.cache[p.cacheKey] = &parsedNode{vecBytes: vecBytes, neighbors: neighbors}
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

// hnswAccessInfo holds the parsed entry point metadata and transform state.
// Matches Java's com.apple.foundationdb.async.hnsw.AccessInfo.
type hnswAccessInfo struct {
	layer       int
	pk          tuple.Tuple
	vectorBytes []byte
	rotatorSeed int64     // -1 = no rotator, other = FhtKacRotator seed
	centroid    []float64 // nil = no centroid (negated centroid from Java)
}

// parseAccessInfo parses the entry point metadata from raw bytes.
// Wire format: Tuple.from(layer, primaryKey, vectorTuple, rotatorSeed, centroidOrNull)
func (s *hnswStorage) parseAccessInfo(data []byte) (*hnswAccessInfo, error) {
	if data == nil {
		return nil, fmt.Errorf("hnsw: no entry point")
	}

	t, unpackErr := fastUnpack(data)
	if unpackErr != nil {
		return nil, fmt.Errorf("hnsw: unpack access info: %w", unpackErr)
	}
	if len(t) < 3 {
		return nil, fmt.Errorf("hnsw: access info too short: %d elements", len(t))
	}

	info := &hnswAccessInfo{
		rotatorSeed: -1, // default: no rotator
	}

	// t[0] = entryPointLayer
	if l, ok := asInt64(t[0]); ok {
		info.layer = int(l)
	}

	// t[1] = entryPointPK (tuple)
	if pkVal, ok := t[1].(tuple.Tuple); ok {
		info.pk = pkVal
	}

	// t[2] = entryPointVectorTuple (tuple containing vector bytes)
	if vt, ok := t[2].(tuple.Tuple); ok && len(vt) > 0 {
		if vb, ok := vt[0].([]byte); ok {
			info.vectorBytes = vb
		}
	}

	// t[3] = rotatorSeed (int64)
	if len(t) > 3 {
		if seed, ok := asInt64(t[3]); ok {
			info.rotatorSeed = seed
		}
	}

	// t[4] = negatedCentroid (tuple containing vector bytes, or nil)
	if len(t) > 4 && t[4] != nil {
		if ct, ok := t[4].(tuple.Tuple); ok && len(ct) > 0 {
			if cb, ok := ct[0].([]byte); ok {
				centroidVec, decErr := deserializeVector(cb)
				if decErr == nil {
					info.centroid = centroidVec
				}
			}
		}
	}

	if info.pk == nil {
		return nil, fmt.Errorf("hnsw: access info has nil PK")
	}

	return info, nil
}

// hasTransform reports whether the access info has transform data (centroid + rotator).
// Matches Java's AccessInfo.canUseRaBitQ() — true when negatedCentroid is non-null.
func (a *hnswAccessInfo) hasTransform() bool {
	return a.centroid != nil
}

// loadAccessInfo reads the entry point metadata from FDB.
func (s *hnswStorage) loadAccessInfo(tx fdb.ReadTransaction) (*hnswAccessInfo, error) {
	hnswStatGet(s.stats)
	key := s.accessSubspace.Pack(tuple.Tuple{})
	data, getErr := tx.Get(fdb.Key(key)).Get()
	if getErr != nil {
		return nil, fmt.Errorf("hnsw: get access info: %w", getErr)
	}
	return s.parseAccessInfo(data)
}

// saveAccessInfo writes the entry point metadata.
// Wire-compatible with Java's StorageAdapter.writeAccessInfo:
// Tuple.from(layer, primaryKey, vectorTuple, rotatorSeed, centroidOrNull)
func (s *hnswStorage) saveAccessInfo(tx fdb.Transaction, info *hnswAccessInfo) {
	key := s.accessSubspace.Pack(tuple.Tuple{})

	// Serialize centroid as a vector tuple (or nil).
	var centroidElement any
	if info.centroid != nil {
		centroidElement = tuple.Tuple{serializeVector(info.centroid)}
	}

	value := tuple.Tuple{
		int64(info.layer),
		info.pk,
		tuple.Tuple{info.vectorBytes},
		info.rotatorSeed,
		centroidElement,
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
		keyTuple, unpackErr := fastSubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
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
		t, valErr := fastUnpack(kv.Value)
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
func (s *hnswStorage) saveNodeLayerInlining(tx fdb.Transaction, layer int, primaryKey tuple.Tuple, neighbors []tuple.Tuple) {
	// Clear all existing edges for this node at this layer.
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	r, err := fdb.PrefixRange(prefix)
	if err == nil {
		tx.ClearRange(r)
	}

	// If no neighbors, write a sentinel KV at the exact prefix key to mark node existence.
	// Without this, loadNodeLayerInlining's range read finds 0 KVs and returns "not found",
	// which breaks greedy descent when the entry point has layers above all other nodes.
	if len(neighbors) == 0 {
		tx.Set(fdb.Key(prefix), []byte{})
	}

	// Write each edge with the neighbor's vector.
	for _, nbPK := range neighbors {
		// Look up neighbor's vector from cache. During insert, neighbors were just
		// loaded by loadNodeLayer/loadNodeLayerBatch, so they're in cache.
		nbVecBytes := s.getVectorBytesFromCache(layer, nbPK)
		if nbVecBytes == nil {
			// Fallback: try layer 0 (compact format always has the vector).
			nbVecBytes = s.getVectorBytesFromCache(0, nbPK)
		}
		if nbVecBytes == nil {
			continue // neighbor not in cache — should not happen during normal operation
		}

		edgeKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey, nbPK})
		edgeValue := tuple.Tuple{nbVecBytes}.Pack()
		tx.Set(fdb.Key(edgeKey), edgeValue)
	}

	// Update the cache for this node so loadNodeLayerDispatch returns
	// neighbors from cache without hitting FDB.
	compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	// For inlining layers, vecBytes is nil (vector lives at layer 0).
	// Use empty slice (not nil) when no neighbors, so cache knows the node
	// exists but has no neighbors (nil neighbors means "unknown, needs range read").
	cachedNeighbors := neighbors
	if cachedNeighbors == nil {
		cachedNeighbors = []tuple.Tuple{}
	}
	s.cache[string(compactKey)] = &parsedNode{vecBytes: nil, neighbors: cachedNeighbors}
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
func (s *hnswStorage) loadNodeLayerInlining(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) ([]byte, []tuple.Tuple, error) {
	// Check cache first (populated by saveNodeLayerInlining, preloadLayerInlining,
	// or a prior loadNodeLayerInlining call).
	compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
	cacheKey := string(compactKey)
	if cached, ok := s.cache[cacheKey]; ok {
		hnswStatCacheHit(s.stats)
		if cached == nil {
			return nil, nil, fmt.Errorf("hnsw: node not found at layer %d (inlining)", layer)
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

	iter := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	var neighbors []tuple.Tuple
	foundAnyKV := false

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, nil, fmt.Errorf("hnsw: inlining get kv layer %d: %w", layer, err)
		}
		foundAnyKV = true

		// Unpack the key to get (layer, sourcePK, neighborPK).
		keyTuple, unpackErr := fastSubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
		if unpackErr != nil || len(keyTuple) < 3 {
			// Sentinel KV at (layer, pk) has only 2 elements — skip it.
			// This marks the node's existence with 0 neighbors.
			continue
		}

		nbPK, ok := keyTuple[2].(tuple.Tuple)
		if !ok {
			continue
		}

		// Value is tuple-packed neighbor vector bytes.
		valueTuple, valErr := fastUnpack(kv.Value)
		if valErr != nil || len(valueTuple) < 1 {
			continue
		}
		nbVecBytes, ok := valueTuple[0].([]byte)
		if !ok {
			continue
		}

		neighbors = append(neighbors, nbPK)

		// Cache the neighbor's data at this layer so loadNodeLayerBatch hits cache.
		// We don't know the neighbor's own neighbors yet, but we have its vector.
		nbCompactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), nbPK})
		nbCacheKey := string(nbCompactKey)
		if _, alreadyCached := s.cache[nbCacheKey]; !alreadyCached {
			// Store a partial cache entry with the vector but no neighbors.
			// This is enough for loadNodeLayerBatch to return vecBytes.
			s.cache[nbCacheKey] = &parsedNode{vecBytes: nbVecBytes, neighbors: nil}
		}
	}

	if !foundAnyKV {
		// No KVs at all — node truly doesn't exist at this layer.
		s.cache[cacheKey] = nil
		return nil, nil, fmt.Errorf("hnsw: node not found at layer %d (inlining)", layer)
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
func (s *hnswStorage) deleteNodeLayerInlining(tx fdb.Transaction, layer int, primaryKey tuple.Tuple) {
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

	iter := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return fmt.Errorf("hnsw: preload inlining layer %d get kv: %w", layer, err)
		}

		keyTuple, unpackErr := fastSubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
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

		valueTuple, valErr := fastUnpack(kv.Value)
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

	// Populate cache: one entry per source node with its neighbor list.
	for sourceKey, edges := range nodeEdges {
		sourcePK := nodePKs[sourceKey]
		compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), sourcePK})
		if _, ok := s.cache[string(compactKey)]; ok {
			continue // already cached
		}

		neighbors := make([]tuple.Tuple, len(edges))
		for i, e := range edges {
			neighbors[i] = e.neighborPK

			// Also cache each neighbor's vector.
			nbCompactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), e.neighborPK})
			nbCacheKey := string(nbCompactKey)
			if _, alreadyCached := s.cache[nbCacheKey]; !alreadyCached {
				s.cache[nbCacheKey] = &parsedNode{vecBytes: e.vecBytes, neighbors: nil}
			}
		}

		s.cache[string(compactKey)] = &parsedNode{vecBytes: nil, neighbors: neighbors}
	}

	return nil
}

// findAnyNodeAtLayerInlining finds any node present at the given inlining layer.
func (s *hnswStorage) findAnyNodeAtLayerInlining(tx fdb.ReadTransaction, layer int) (pk tuple.Tuple, vectorBytes []byte, err error) {
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	r, rangeErr := fdb.PrefixRange(prefix)
	if rangeErr != nil {
		return nil, nil, rangeErr
	}

	// Read just 1 KV to find any node.
	ri := tx.GetRange(r, fdb.RangeOptions{Limit: 1}).Iterator()
	if ri.Advance() {
		kv, getErr := ri.Get()
		if getErr != nil {
			return nil, nil, getErr
		}

		keyTuple, unpackErr := fastSubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
		if unpackErr != nil || len(keyTuple) < 2 {
			return nil, nil, fmt.Errorf("hnsw: inlining key too short at layer %d", layer)
		}

		if pkTuple, ok := keyTuple[1].(tuple.Tuple); ok {
			// For inlining layers, the node's own vector is at layer 0.
			// Return nil vectorBytes — the caller should have the entry point vector
			// from the access info, or can look it up at layer 0.
			return pkTuple, nil, nil
		}
	}

	return nil, nil, fmt.Errorf("hnsw: no nodes at layer %d", layer)
}

// --- Dispatch methods ---
// These choose between compact and inlining format based on layer and config.

// saveNodeLayerDispatch saves a node's layer data in the appropriate format.
func (s *hnswStorage) saveNodeLayerDispatch(tx fdb.Transaction, layer int, primaryKey tuple.Tuple, vectorBytes []byte, neighbors []tuple.Tuple) {
	if s.isInliningLayer(layer) {
		s.saveNodeLayerInlining(tx, layer, primaryKey, neighbors)
	} else {
		s.saveNodeLayer(tx, layer, primaryKey, vectorBytes, neighbors)
	}
}

// loadNodeLayerDispatch loads a node's layer data from the appropriate format.
func (s *hnswStorage) loadNodeLayerDispatch(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) (vectorBytes []byte, neighbors []tuple.Tuple, err error) {
	if s.isInliningLayer(layer) {
		return s.loadNodeLayerInlining(tx, layer, primaryKey)
	}
	return s.loadNodeLayer(tx, layer, primaryKey)
}

// loadNodeLayerBatchDispatch batch-loads nodes from the appropriate format.
// For inlining layers, individual loadNodeLayerInlining calls are still used but the
// neighbor vectors are already cached from the first range read.
func (s *hnswStorage) loadNodeLayerBatchDispatch(tx fdb.ReadTransaction, layer int, pks []tuple.Tuple) []nodeResult {
	if !s.isInliningLayer(layer) {
		return s.loadNodeLayerBatch(tx, layer, pks)
	}

	// For inlining layers, the cache is already populated by loadNodeLayerInlining/preloadLayerInlining.
	// We just need to look up each PK in the cache.
	results := make([]nodeResult, len(pks))
	for i, pk := range pks {
		results[i].pk = pk
		results[i].pkBytes = string(pk.Pack())

		compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), pk})
		if cached, ok := s.cache[string(compactKey)]; ok {
			if cached == nil {
				results[i].err = fmt.Errorf("hnsw: node not found at layer %d (inlining)", layer)
			} else {
				results[i].vecBytes = cached.vecBytes
				results[i].neighbors = cached.neighbors
			}
			continue
		}

		// Not cached yet — do a range read (this should be rare after preload).
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
func (s *hnswStorage) deleteNodeLayerDispatch(tx fdb.Transaction, layer int, primaryKey tuple.Tuple) {
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

// findAnyNodeAtLayerDispatch finds any node at a layer in the appropriate format.
func (s *hnswStorage) findAnyNodeAtLayerDispatch(tx fdb.ReadTransaction, layer int) (pk tuple.Tuple, vectorBytes []byte, err error) {
	if s.isInliningLayer(layer) {
		return s.findAnyNodeAtLayerInlining(tx, layer)
	}
	return s.findAnyNodeAtLayer(tx, layer)
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

// SerializeVector encodes a float64 vector into the on-disk byte format the
// HNSW vector index reads (RealVector.fromBytes). Exported so callers/tests can
// populate a record's vector column.
func SerializeVector(vec []float64) []byte { return serializeVector(vec) }

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
		return nil, fmt.Errorf("hnsw: RaBitQ vectors must be decoded via the VectorQuantizer interface, not deserializeVector")
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
	x += -0x61C8864680B583EB                             // 0x9e3779b97f4a7c15 as signed int64
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

func (h distHeap) Len() int           { return len(h) }
func (h distHeap) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h distHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *distHeap) Push(x any)        { *h = append(*h, x.(distItem)) }
func (h *distHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
