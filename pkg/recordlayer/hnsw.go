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
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
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

	// SharedCacheMaxNodes enables a process-wide, cross-transaction node cache
	// (see sharedNodeCache) bounded to this many nodes. 0 (default) disables it.
	// Go-only extension: keeps insert throughput from collapsing as the graph
	// grows. Intended for single-writer indexing + read-only search.
	SharedCacheMaxNodes int
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
	// Fast path: read components straight from the stored bytes, no []float64.
	if dist, ok := vectorDistanceFromBytes(query, storedVecBytes, g.config.Metric); ok {
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

		// Decode the ≤M selected neighbor spans to tuple.Tuple once. The graph-write
		// path (save/prune) operates on tuple.Tuple; only this bounded set is decoded,
		// so the search traversal stays boxing-free.
		selectedPKs := make([]tuple.Tuple, len(selected))
		for i, nb := range selected {
			selectedPKs[i], err = decodeNestedPK(nb.pkSpan)
			if err != nil {
				return fmt.Errorf("hnsw insert: decode selected neighbor span at layer %d: %w", layer, err)
			}
		}

		// Build neighbor list for the new node at this layer.
		newNodeNeighbors := make([]tuple.Tuple, len(selectedPKs))
		copy(newNodeNeighbors, selectedPKs)

		// Save new node at this layer.
		g.storage.saveNodeLayerDispatch(tx, layer, primaryKey, vecBytes, newNodeNeighbors)

		// Add reverse connections to each neighbor.
		for _, nbPK := range selectedPKs {
			nbVecBytes, nbNeighborSpans, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, nbPK)
			if loadErr != nil {
				return fmt.Errorf("hnsw insert: load neighbor %v at layer %d for reverse connection: %w", nbPK, layer, loadErr)
			}

			// Decode this neighbor's own edge list (bounded by maxConn) back to
			// tuple.Tuple for the write path, then add the new node as a neighbor.
			nbNeighbors := make([]tuple.Tuple, 0, len(nbNeighborSpans)+1)
			for _, sp := range nbNeighborSpans {
				pk, derr := decodeNestedPK(sp)
				if derr != nil {
					return fmt.Errorf("hnsw insert: decode reverse neighbor span at layer %d: %w", layer, derr)
				}
				nbNeighbors = append(nbNeighbors, pk)
			}
			nbNeighbors = append(nbNeighbors, primaryKey)

			// Prune if over limit.
			if len(nbNeighbors) > maxConn {
				// Resolve vector bytes (at inlining layers, own vector is at layer 0).
				resolvedVec := g.storage.resolveVectorBytes(layer, nbPK, nbVecBytes)
				nbVec, decErr := g.decodeStoredVector(resolvedVec)
				if decErr != nil {
					return fmt.Errorf("hnsw insert: decode neighbor %v vector at layer %d for pruning: %w", nbPK, layer, decErr)
				}
				nbNeighbors, err = g.pruneNeighbors(tx, nbVec, nbNeighbors, maxConn, layer)
				if err != nil {
					return err
				}
			}

			g.storage.saveNodeLayerDispatch(tx, layer, nbPK, nbVecBytes, nbNeighbors)
		}

		if len(neighbors) > 0 {
			// Use nearest neighbor as entry point for next layer down.
			currentPK, err = decodeNestedPK(neighbors[0].pkSpan)
			if err != nil {
				return fmt.Errorf("hnsw insert: decode next-layer entry point: %w", err)
			}
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
		if e := hnswFatal(accessErr); e != nil {
			return e // transient read — abort/retry, don't treat as an empty graph
		}
		return nil // genuinely empty graph, nothing to delete
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
	// A future error is a real I/O failure (a missing key surfaces as data==nil below,
	// not an error) and must propagate so the tx retries rather than silently skipping a
	// layer and committing an incomplete delete. Record the first error but keep draining
	// the REMAINING futures before returning: leaving futures unresolved when the retry
	// loop resets/reuses the transaction violates the FDB tx contract and leaks the
	// pure-Go reply handles/timers.
	var futureErr error
	for _, f := range futures {
		data, err := f.future.Get()
		if err != nil {
			if futureErr == nil {
				futureErr = fmt.Errorf("hnsw delete: read layer %d: %w", f.layer, err)
			}
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
	if futureErr != nil {
		return futureErr
	}

	// Check existence at layer 0 (always compact, now served from cache).
	_, _, err := g.storage.loadNodeLayer(tx, 0, primaryKey)
	if err != nil {
		if e := hnswFatal(err); e != nil {
			return e // transient read — abort/retry, don't treat as already-deleted
		}
		return nil // already deleted or doesn't exist
	}

	// Delete from all layers and repair.
	for layer := 0; layer <= epLayer; layer++ {
		_, neighbors, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, primaryKey)
		if loadErr != nil {
			if e := hnswFatal(loadErr); e != nil {
				return e // transient read error — abort and retry, don't skip the layer
			}
			continue // not present at this layer
		}

		g.storage.deleteNodeLayerDispatch(tx, layer, primaryKey)

		// Repair: for each neighbor, reconnect.
		for _, nbSpan := range neighbors {
			nbPK, derr := decodeNestedPK(nbSpan)
			if derr != nil {
				return derr
			}
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
			// A transient scan error must abort the delete (so it retries) rather than
			// be read as "layer empty" — otherwise we'd pick a wrong/no entry point and
			// commit. A genuine empty layer (errHNSWNotPresent) is skipped to the next.
			if e := hnswFatal(scanErr); e != nil {
				return e
			}
			if foundPK != nil {
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
		if e := hnswFatal(err); e != nil {
			return nil, e // transient read — propagate so the caller retries
		}
		return nil, nil // genuinely empty graph
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
		pk, derr := decodeNestedPK(c.pkSpan)
		if derr != nil {
			return nil, derr
		}
		results[i] = hnswSearchResult{
			PrimaryKey: pk,
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
			if e := hnswFatal(err); e != nil {
				return nil, nil, e
			}
			break // no data at this layer
		}

		// Batch-fetch all neighbor vectors.
		// For inlining layers, neighbor vectors are already cached from the range read.
		batchResults := g.storage.loadNodeLayerBatchDispatch(tx, layer, neighbors)
		for _, r := range batchResults {
			if r.err != nil {
				if e := hnswFatal(r.err); e != nil {
					return nil, nil, e
				}
				continue
			}
			dist := g.computeDistance(query, r.vecBytes)
			if dist < bestDist {
				bestDist = dist
				bestPK, err = decodeNestedPK(r.span)
				if err != nil {
					return nil, nil, err
				}
				bestVecBytes = r.vecBytes
				changed = true
			}
		}
	}

	return bestPK, bestVecBytes, nil
}

// hnswCandidate represents a search candidate with its primary key, vector bytes, and distance.
type hnswCandidate struct {
	pkSpan   []byte    // neighbor PK as a nested-encoded span (decode to tuple.Tuple only at boundaries)
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
	// Entry point in span form (nested-encoded PK) so it dedups consistently with
	// neighbor spans pulled from node values.
	epSpan := nestPK(epPK)
	epSpanStr := string(epSpan)

	// Candidates (min-heap by distance) and visited set.
	backing := make(distHeap, 0, ef)
	candidates := &backing
	heap.Push(candidates, distItem{pkSpan: epSpan, dist: epDist, spanStr: epSpanStr})

	visited := make(map[string]bool, ef*2)
	visited[epSpanStr] = true

	// Pre-allocate results to expected capacity (ef).
	results := make([]hnswCandidate, 1, ef)
	results[0] = hnswCandidate{pkSpan: epSpan, vecBytes: epVecBytes, dist: epDist}

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
			if len(results) >= ef && closest.dist > results[len(results)-1].dist {
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

			if len(results) < ef || dist < results[len(results)-1].dist {
				// r.spanStr is already computed by loadNodeLayerBatch — no double Pack().
				heap.Push(candidates, distItem{pkSpan: r.span, dist: dist, spanStr: r.spanStr})
				// Binary-search insertion into sorted results (O(log n) find + O(n) shift).
				c := hnswCandidate{pkSpan: r.span, vecBytes: r.vecBytes, dist: dist}
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
		working = append(working, hnswCandidate{pkSpan: r.span, vecBytes: r.vecBytes, dist: dist})
	}

	return g.selectNeighbors(working, maxConn), nil
}

// pruneNeighbors re-selects the best maxConn neighbors for a node by computing distances.
// Uses the same heuristic as selectNeighbors (with optional extendCandidates).
func (g *hnswGraph) pruneNeighbors(tx fdb.ReadTransaction, nodeVec []float64, neighborPKs []tuple.Tuple, maxConn, layer int) ([]tuple.Tuple, error) {
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
		dist := g.computeDistance(nodeVec, r.vecBytes)
		candidates = append(candidates, hnswCandidate{pkSpan: r.span, vecBytes: r.vecBytes, dist: dist})
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
		pk, derr := decodeNestedPK(s.pkSpan)
		if derr != nil {
			return nil, derr
		}
		result[i] = pk
	}
	return result, nil
}

// repairNeighbor removes a deleted node from a neighbor's list and finds replacement connections.
func (g *hnswGraph) repairNeighbor(tx fdb.Transaction, layer int, neighborPK, deletedPK tuple.Tuple) error {
	nbVecBytes, nbNeighbors, err := g.storage.loadNodeLayerDispatch(tx, layer, neighborPK)
	if err != nil {
		if e := hnswFatal(err); e != nil {
			return e // transient read error — abort/retry, don't treat as absent
		}
		return nil // neighbor doesn't exist
	}

	// Work in span form (delete/repair is bounded, not the search hot path).
	deletedSpan := nestPK(deletedPK)
	neighborSpan := nestPK(neighborPK)

	// Remove deleted node from neighbor's list.
	filtered := make([][]byte, 0, len(nbNeighbors))
	for _, span := range nbNeighbors {
		if !bytes.Equal(span, deletedSpan) {
			filtered = append(filtered, span)
		}
	}

	// Batch-load filtered neighbors to get their neighbor lists.
	filteredBatch := g.storage.loadNodeLayerBatchDispatch(tx, layer, filtered)

	// Find candidates from neighbors-of-neighbors.
	candidateMap := make(map[string][]byte)
	for _, r := range filteredBatch {
		if r.err != nil {
			if e := hnswFatal(r.err); e != nil {
				return e
			}
			continue
		}
		for _, candidate := range r.neighbors {
			candidateKey := string(candidate)
			if !bytes.Equal(candidate, neighborSpan) && !bytes.Equal(candidate, deletedSpan) {
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
			if e := hnswFatal(r.err); e != nil {
				return e
			}
			continue
		}
		rPK, derr := decodeNestedPK(r.span)
		if derr != nil {
			return derr
		}
		resolvedCandidateVec := g.storage.resolveVectorBytes(layer, rPK, r.vecBytes)
		candidateVec, decErr := g.decodeStoredVector(resolvedCandidateVec)
		if decErr != nil {
			continue
		}
		dist := vectorDistance(nbVec, candidateVec, g.config.Metric)
		allCandidates = append(allCandidates, hnswCandidate{pkSpan: r.span, vecBytes: resolvedCandidateVec, vec: candidateVec, dist: dist})
		seen[string(r.span)] = true
	}

	// Collect unseen candidates from neighbors-of-neighbors.
	var newCandidatePKs [][]byte
	for key, span := range candidateMap {
		if seen[key] {
			continue
		}
		newCandidatePKs = append(newCandidatePKs, span)
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
			if e := hnswFatal(r.err); e != nil {
				return e
			}
			continue
		}
		rPK, derr := decodeNestedPK(r.span)
		if derr != nil {
			return derr
		}
		resolvedCandidateVec := g.storage.resolveVectorBytes(layer, rPK, r.vecBytes)
		candidateVec, decErr := g.decodeStoredVector(resolvedCandidateVec)
		if decErr != nil {
			continue
		}
		dist := vectorDistance(nbVec, candidateVec, g.config.Metric)
		allCandidates = append(allCandidates, hnswCandidate{pkSpan: r.span, vecBytes: resolvedCandidateVec, vec: candidateVec, dist: dist})
	}

	// Select best connections using the heuristic.
	maxConn := g.config.MMax
	if layer == 0 {
		maxConn = g.config.MMax0
	}

	selected := g.selectNeighbors(allCandidates, maxConn)

	newNeighbors := make([]tuple.Tuple, len(selected))
	for i, c := range selected {
		pk, derr := decodeNestedPK(c.pkSpan)
		if derr != nil {
			return derr
		}
		newNeighbors[i] = pk
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
	neighbors [][]byte // neighbor PKs as nested-encoded spans (Tuple{pk}.Pack())
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
//
// errHNSWNotPresent marks a genuine "this node/layer is absent" result, as opposed
// to a transient FDB failure (transaction_too_old, timeout) encountered while reading.
// Graph callers MUST distinguish the two: an absent node is skipped (the graph simply
// has no such edge), but a transient read error must propagate so the surrounding
// transaction retries — treating it as "absent" would commit a partial/corrupt graph.
// Wrap genuine not-found returns with this sentinel; leave real I/O errors unwrapped.
var errHNSWNotPresent = errors.New("hnsw: node not present")

// hnswFatal reports a load/scan error that the caller must NOT treat as a benign
// absent node: it returns the error for a real I/O failure (transaction_too_old,
// timeout, etc.) that must abort and retry the transaction, and nil when err is nil
// or a genuine errHNSWNotPresent "absent" result (safe to skip). Callers use:
//
//	if e := hnswFatal(err); e != nil { return <zero...>, e }
//	// ... then fall through to the existing skip/continue for the absent case.
func hnswFatal(err error) error {
	if err == nil || errors.Is(err, errHNSWNotPresent) {
		return nil
	}
	return err
}

type hnswStorage struct {
	dataSubspace   subspace.Subspace
	accessSubspace subspace.Subspace
	config         HNSWConfig
	cache          map[string]*parsedNode // FDB key → parsed node (nil = not found)
	shared         *sharedNodeCache       // optional cross-tx cache (nil = disabled)
	stats          *HNSWStats             // optional I/O counters (nil = no tracking)

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

func newHNSWStorage(ss subspace.Subspace, config HNSWConfig) *hnswStorage {
	s := &hnswStorage{
		dataSubspace:   ss.Sub(int64(0)),
		accessSubspace: ss.Sub(int64(1)),
		config:         config,
		cache:          make(map[string]*parsedNode),
	}
	maxNodes := config.SharedCacheMaxNodes
	if maxNodes == 0 {
		maxNodes = defaultSharedCacheNodes
	}
	if maxNodes > 0 {
		s.shared = getSharedNodeCache(string(ss.Bytes()), maxNodes)
	}
	return s
}

// cacheLookup checks the per-transaction cache, then the optional shared cross-
// transaction cache. A shared hit is mirrored into the per-tx cache so repeat
// intra-tx reads stay local.
func (s *hnswStorage) cacheLookup(key string) (*parsedNode, bool) {
	if n, ok := s.cache[key]; ok {
		return n, true
	}
	if s.shared != nil {
		if n, ok := s.shared.get(key); ok {
			s.cache[key] = n
			return n, true
		}
	}
	return nil, false
}

// cacheStore records a node read from committed FDB data in both caches.
// Negative results (node == nil) are kept per-tx only — "absent" is transient
// during a build and must not be shared across transactions.
func (s *hnswStorage) cacheStore(key string, node *parsedNode) {
	s.cache[key] = node
	if s.shared != nil && node != nil {
		s.shared.put(key, node)
	}
}

// cacheInvalidate drops a key from the shared cache on write. The per-tx cache
// keeps this transaction's own write for read-your-writes; the shared entry is
// dropped so other transactions re-read the committed value from FDB (and, if
// this tx aborts, the still-committed value).
func (s *hnswStorage) cacheInvalidate(key string) {
	if s.shared != nil {
		s.shared.invalidate(key)
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
	// The cache holds neighbors as nested-encoded spans (Tuple{pk}.Pack()).
	cacheNeighbors := make([][]byte, len(neighbors))
	for i, pk := range neighbors {
		cacheNeighbors[i] = nestPK(pk)
	}
	s.cache[string(key)] = &parsedNode{vecBytes: vectorBytes, neighbors: cacheNeighbors}
	s.cacheInvalidate(string(key))
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
func parseNodeValue(data []byte) (vectorBytes []byte, neighbors [][]byte, err error) {
	// elem 0: nodeKind (skip).
	p := 0
	n0 := tupleSkip(data[p:])
	if n0 < 0 {
		return nil, nil, fmt.Errorf("hnsw: truncated node value (nodeKind)")
	}
	p += n0

	// elem 1: vectorTuple = {vectorBytes} (nested). Children are stored verbatim,
	// so the content between the 0x05 marker and the nested terminator is exactly
	// the single bytes element's standalone encoding (which fastDecodeBytes
	// unescapes on its own). No extra un-nesting pass is needed.
	if p >= len(data) || data[p] != tcNested {
		return nil, nil, fmt.Errorf("hnsw: node value missing vector tuple")
	}
	n1 := tupleSkip(data[p:])
	if n1 < 0 || p+n1 > len(data) {
		return nil, nil, fmt.Errorf("hnsw: truncated vector tuple")
	}
	vecInner := data[p+1 : p+n1-1]
	if len(vecInner) > 0 {
		// NOTE: must NOT un-escape in place — `data` is the FDB value buffer, which
		// the pure-Go client's read-your-writes / snapshot caches retain and reuse.
		// fastDecodeBytes copies, which is required for correctness here.
		vb, _, berr := fastDecodeBytes(vecInner)
		if berr != nil {
			return nil, nil, fmt.Errorf("hnsw: decode vector bytes: %w", berr)
		}
		vectorBytes = vb
	}
	p += n1

	// elem 2: neighborList (nested). Optional — absent => no neighbors. Its
	// content is the concatenation of each neighbor's verbatim nested encoding
	// (== Tuple{pk}.Pack()), so each per-element span IS the fetch-key suffix.
	if p >= len(data) {
		return vectorBytes, nil, nil
	}
	if data[p] != tcNested {
		return nil, nil, fmt.Errorf("hnsw: node value neighbor list is not a tuple")
	}
	n2 := tupleSkip(data[p:])
	if n2 < 0 || p+n2 > len(data) {
		return nil, nil, fmt.Errorf("hnsw: truncated neighbor list")
	}
	neighbors, err = nestedPKSpans(data[p+1 : p+n2-1])
	if err != nil {
		return nil, nil, err
	}
	return vectorBytes, neighbors, nil
}

// decodeNestedPK turns a neighbor span (nested-encoded PK == Tuple{pk}.Pack())
// back into the primary-key tuple. Used only at bounded boundaries (the ≤k search
// results and the ≤M selected insert neighbors), never in the hot traversal loop.
func decodeNestedPK(span []byte) (tuple.Tuple, error) {
	t, err := fastUnpack(span)
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
	if data == nil {
		s.cache[cacheKey] = nil // cache negative result
		return nil, nil, fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
	}

	// Parse and cache the result.
	vectorBytes, neighbors, err = parseNodeValue(data)
	if err != nil {
		return nil, nil, err
	}
	s.cacheStore(cacheKey, &parsedNode{vecBytes: vectorBytes, neighbors: neighbors})
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
		if data == nil {
			s.cache[p.key] = nil // cache negative result
			results[p.idx].err = fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
			continue
		}
		vecBytes, neighbors, parseErr := parseNodeValue(data)
		if parseErr != nil {
			results[p.idx].err = parseErr
			continue
		}
		s.cacheStore(p.key, &parsedNode{vecBytes: vecBytes, neighbors: neighbors})
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
		if data == nil {
			s.cache[p.cacheKey] = nil
			results[p.idx].err = fmt.Errorf("hnsw: node not found at layer %d: %w", layer, errHNSWNotPresent)
			continue
		}
		vecBytes, neighbors, parseErr := parseNodeValue(data)
		if parseErr != nil {
			results[p.idx].err = parseErr
			continue
		}
		s.cacheStore(p.cacheKey, &parsedNode{vecBytes: vecBytes, neighbors: neighbors})
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
	// Advance()==false ends the loop on exhaustion OR a transient FDB error; check
	// Get() for the stored error so a mid-scan 1007/timeout surfaces instead of
	// silently leaving the layer cache PARTIALLY populated (a corrupt-graph hazard).
	if _, err := iter.Get(); err != nil {
		return fmt.Errorf("hnsw: preload layer %d scan: %w", layer, err)
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
	s.cacheInvalidate(string(key))
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
		// No access info written yet — a genuinely empty graph. Mark it absent (not
		// fatal) so Delete/Search skip cleanly; a transient get error (handled by the
		// caller before this) stays unwrapped and propagates. Corruption below is fatal.
		return nil, fmt.Errorf("hnsw: no entry point: %w", errHNSWNotPresent)
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
	data, getErr := s.getKey(tx, fdb.Key(key))
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

	ri := s.scanIter(tx, r, fdb.RangeOptions{Limit: 1})
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

	// Advance()==false: distinguish a transient FDB error from a genuinely empty
	// layer, so a 1007/timeout isn't reported as the misleading "no nodes at layer".
	if _, getErr := ri.Get(); getErr != nil {
		return nil, nil, fmt.Errorf("hnsw: find any node at layer %d: %w", layer, getErr)
	}
	return nil, nil, fmt.Errorf("hnsw: no nodes at layer %d: %w", layer, errHNSWNotPresent)
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
	// The whole index was cleared — drop every cached node so post-clear reads
	// don't resurrect deleted data.
	s.cache = make(map[string]*parsedNode)
	if s.shared != nil {
		s.shared.clear()
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
	// Cache holds neighbors as nested-encoded spans.
	cachedNeighbors := make([][]byte, len(neighbors))
	for i, pk := range neighbors {
		cachedNeighbors[i] = nestPK(pk)
	}
	s.cache[string(compactKey)] = &parsedNode{vecBytes: nil, neighbors: cachedNeighbors}
	s.cacheInvalidate(string(compactKey))
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
			return nil, nil, fmt.Errorf("hnsw: node not found at layer %d (inlining): %w", layer, errHNSWNotPresent)
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

		neighbors = append(neighbors, nestPK(nbPK))

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
	// Surface a transient FDB error that ended the scan via Advance()==false, so an
	// incomplete neighbor list isn't returned as a complete one (silent corruption).
	if _, err := iter.Get(); err != nil {
		return nil, nil, fmt.Errorf("hnsw: inlining scan layer %d: %w", layer, err)
	}

	if !foundAnyKV {
		// No KVs at all — node truly doesn't exist at this layer.
		s.cache[cacheKey] = nil
		return nil, nil, fmt.Errorf("hnsw: node not found at layer %d (inlining): %w", layer, errHNSWNotPresent)
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
	s.cacheInvalidate(string(prefix))
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
	// Surface a transient FDB error that ended the scan before populating the cache
	// from the (otherwise partial) edge map — a silent partial cache corrupts the graph.
	if _, err := iter.Get(); err != nil {
		return fmt.Errorf("hnsw: preload inlining layer %d scan: %w", layer, err)
	}

	// Populate cache: one entry per source node with its neighbor list.
	for sourceKey, edges := range nodeEdges {
		sourcePK := nodePKs[sourceKey]
		compactKey := s.dataSubspace.Pack(tuple.Tuple{int64(layer), sourcePK})
		if _, ok := s.cache[string(compactKey)]; ok {
			continue // already cached
		}

		neighbors := make([][]byte, len(edges))
		for i, e := range edges {
			neighbors[i] = nestPK(e.neighborPK)

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
	ri := s.scanIter(tx, r, fdb.RangeOptions{Limit: 1})
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

	// Advance()==false: surface a transient FDB error rather than the misleading
	// "no nodes at layer".
	if _, getErr := ri.Get(); getErr != nil {
		return nil, nil, fmt.Errorf("hnsw: find any node (inlining) at layer %d: %w", layer, getErr)
	}
	return nil, nil, fmt.Errorf("hnsw: no nodes at layer %d: %w", layer, errHNSWNotPresent)
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
				results[i].err = fmt.Errorf("hnsw: node not found at layer %d (inlining): %w", layer, errHNSWNotPresent)
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

// serializeVector serializes a float64 vector to bytes (DOUBLE format). The
// canonical codec lives in the leaf vectorcodec package so it can be shared with
// the Cascades values layer (distance-over-stored-column) without an import cycle.
func serializeVector(vec []float64) []byte {
	return vectorcodec.Serialize(vec)
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
	return vectorcodec.Deserialize(data)
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
	pkSpan  []byte // nested-encoded PK span
	dist    float64
	spanStr string // cached string(pkSpan) for the visited-set / dedup
}

func (h distHeap) Len() int           { return len(h) }
func (h distHeap) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h distHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *distHeap) Push(x any)        { *h = append(*h, x.(distItem)) }
func (h *distHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
