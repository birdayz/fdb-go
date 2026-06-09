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
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math"
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
	MMax                  int             // Max connections for non-zero layers (default 16 — Java DEFAULT_M_MAX, a constant, NOT derived from M; setting M > 16 requires setting MMax too, like Java)
	MMax0                 int             // Max connections for layer 0 (default 32 — Java DEFAULT_M_MAX_0, a constant, NOT derived from M)
	EfConstruction        int             // Insertion search factor (default 200)
	Metric                VectorMetric    // Distance metric
	ExtendCandidates      bool            // Extend candidate set with 2nd-degree neighbors (default false)
	KeepPrunedConnections bool            // Retain pruned candidates to fill up to M (default false)
	EfRepair              int             // Delete-repair beam width; Java requires [m, 400] (default 64; 0 means "use default")
	UseInlining           bool            // Use inlining storage for layers > 0 (default false)
	Quantizer             VectorQuantizer // Optional quantizer (nil = raw float64 storage)

	// RaBitQ centroid-bootstrap stats (Java Config). When RaBitQ is enabled and no centroid
	// is yet established, inserts sample vectors into the SAMPLES subspace, periodically roll
	// them up, and once StatsThreshold vectors have accumulated, establish the rotated centroid.
	SampleVectorStatsProbability float64 // prob of sampling each inserted vector (default 0.5)
	MaintainStatsProbability     float64 // prob of rolling up samples on an insert (default 0.05)
	StatsThreshold               int     // accumulated count needed to establish the centroid (default 1000)

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
	// Cross-field invariants — Java Config constructor (Config.java:88-92):
	//   Preconditions.checkArgument(m <= mMax, ...)
	//   Preconditions.checkArgument(mMax <= mMax0, ...)
	//   Preconditions.checkArgument(efRepair >= m && efRepair <= 400, ...)
	if c.M > c.MMax {
		return fmt.Errorf("hnsw: m (%d) must be <= mMax (%d)", c.M, c.MMax)
	}
	if c.MMax > c.MMax0 {
		return fmt.Errorf("hnsw: mMax (%d) must be <= mMax0 (%d)", c.MMax, c.MMax0)
	}
	if c.EfRepair < c.M || c.EfRepair > 400 {
		return fmt.Errorf("hnsw: efRepair must be in [m, 400] = [%d, 400], got %d", c.M, c.EfRepair)
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
		EfRepair:                            defaultEfRepair,
		Metric:                              VectorMetricEuclidean,
		MaxNumConcurrentNodeFetches:         16,
		MaxNumConcurrentNeighborhoodFetches: 10,
		MaxNumConcurrentDeleteFromLayer:     2,
		SampleVectorStatsProbability:        0.5,  // Java DEFAULT_SAMPLE_VECTOR_STATS_PROBABILITY
		MaintainStatsProbability:            0.05, // Java DEFAULT_MAINTAIN_STATS_PROBABILITY
		StatsThreshold:                      1000, // Java DEFAULT_STATS_THRESHOLD
	}
}

// hnswGraph is an HNSW (Hierarchical Navigable Small World) graph stored in FDB.
// Wire-compatible with Java's com.apple.foundationdb.async.hnsw.HNSW.
type hnswGraph struct {
	storage *hnswStorage
	config  HNSWConfig

	// opXform is the storage transform of the in-flight operation (the rotation +
	// centroid that maps a raw vector into the graph's current coordinate system).
	// Set once at the start of each Insert/Search/Delete from the freshly-read
	// AccessInfo; nil when no centroid is established yet (or the index has no
	// quantizer). It is the read-side analog of Java's StorageTransform, which
	// CompactStorageAdapter.compactNodeFromTuples applies to every fetched vector
	// (CompactStorageAdapter.java:199): a vector stored *plain* (pre-centroid, noOp
	// quantizer) must be lifted by opXform before it is compared, so plain and
	// RaBitQ-encoded nodes — "a mix of vectors" (Insert.java:360) — are always
	// compared in one coordinate system.
	//
	// A field, not a threaded parameter, because — unlike Java's single reused Hnsw
	// object — Go builds a fresh hnswGraph per operation (NewHNSWGraph in
	// withPrefixWriteLock / per Search), driven single-goroutine within one FDB
	// transaction, so there is no concurrent reader/writer to race on it.
	opXform *hnswTransform
}

// defaultEfRepair is Java's DEFAULT_EF_REPAIR (Config.java:41). Java forbids efRepair=0
// outright (its precondition is efRepair ∈ [m, 400], Config.java:91-92) — 0 is not a
// sentinel for "unlimited". The maintainer path starts from DefaultHNSWConfig and
// validates, so it never carries 0; only a direct struct literal can omit the field.
const defaultEfRepair = 64

// NewHNSWGraph creates a new HNSW graph.
func NewHNSWGraph(storage *hnswStorage, config HNSWConfig) *hnswGraph {
	// Normalize an omitted EfRepair (Go zero value) to the default, matching Java's
	// builder default. Left at 0 it would drive the delete-repair sample rate
	// (efRepair - |primaries|) / numCandidates negative the moment any primary neighbor
	// exists, skipping every secondary repair candidate and silently losing graph
	// connectivity on delete.
	if config.EfRepair == 0 {
		config.EfRepair = defaultEfRepair
	}
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

// encodeVectorBytes returns the bytes to store for a vector. transformActive reports
// whether the vector was stored under an active storage transform (an established
// centroid, or the immediate rotation used for translation-non-preserving metrics).
//
// This mirrors Java's quantizer selection (Insert.java:196-198, 262-278): with RaBitQ
// the quantizer is the noOp quantizer until a centroid exists — so pre-centroid vectors
// are stored *plain* (recoverable, lifted by the storage transform at read) — and only
// the real RaBitQuantizer once the transform is active. The caller must already have
// applied the transform to `vector`. Returns quantized bytes only when both a quantizer
// is configured and the transform is active; otherwise raw DOUBLE-serialized bytes.
func (g *hnswGraph) encodeVectorBytes(vector []float64, transformActive bool) []byte {
	if g.config.Quantizer != nil && transformActive {
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
		// RaBitQ estimates distance in SQUARED L2 space. The true-L2 Euclidean metric
		// surfaces sqrt to stay consistent with the raw path (vectorDistanceFromBytes)
		// and with Java's EuclideanMetric (a RaBitQ index and a raw index must report
		// the same distance). EUCLIDEAN_SQUARE keeps the squared estimate.
		//
		// Clamp to >= 0 before sqrt: the RaBitQ estimate is approximate and can be
		// slightly NEGATIVE near zero distance (e.g. a self/near-self match). True L2 is
		// always >= 0; without the clamp sqrt(neg)=NaN, which sorts as "not nearest" and
		// drops the self match (chaos vector_index_self_search_miss). A negative squared
		// estimate correctly meant "≈0, nearest" pre-sqrt, so clamp preserves that.
		if g.config.Metric == VectorMetricEuclidean {
			return math.Sqrt(math.Max(0, dist))
		}
		return dist
	}
	// Plain (non-quantized) stored vector. When a storage transform is active (a
	// centroid has been established), this vector was stored *before* the centroid
	// under the noOp quantizer, so it is still in raw coordinates — lift it into the
	// current coordinate system before comparing, exactly as Java applies the current
	// StorageTransform to a fetched plain vector (CompactStorageAdapter.java:199).
	// query is already in that system (Search/Insert transformed it up front).
	if g.opXform != nil {
		vec, err := deserializeVector(storedVecBytes)
		if err != nil {
			return math.Inf(1)
		}
		return vectorDistance(query, g.opXform.apply(vec), g.config.Metric)
	}
	// No active transform: query and stored vector share raw coordinates already.
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
		// Quantized: already in the current coordinate system (the centroid is
		// established once and never changes), so it is directly comparable.
		return g.config.Quantizer.Decode(storedVecBytes, g.config.NumDimensions)
	}
	vec, err := deserializeVector(storedVecBytes)
	if err != nil {
		return nil, err
	}
	// Plain vector stored before the centroid (noOp quantizer) — lift it into the
	// current coordinate system so pairwise distances against quantized neighbors are
	// consistent (Java applies the current StorageTransform to every fetched vector).
	if g.opXform != nil {
		vec = g.opXform.apply(vec)
	}
	return vec, nil
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
	g.opXform = transform

	// Transform vector for storage and search.
	// All vectors in the graph are stored in the current coordinate system: when a
	// transform is active the vector is rotated/translated and quantized; otherwise it
	// is stored plain (and lifted at read once a centroid exists), matching Java's
	// noOp-until-centroid quantizer choice.
	queryVec := vector
	if transform != nil {
		queryVec = transform.apply(vector)
	}
	vecBytes := g.encodeVectorBytes(queryVec, transform != nil)

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

		// The NEW node connects to up to M neighbors (Java Insert.insertIntoLayer:
		// selectCandidates(..., getConfig().getM(), ...) — getM() is layer-independent).
		// maxConn (MMax, or MMax0 at layer 0) is the cap for PRUNING existing neighbors
		// when a reverse edge overflows them, NOT for the new node's own selection.
		// Selecting maxConn here was a Go-only divergence: with the default MMax0=32 > M=16
		// it gave the new node up to 32 layer-0 edges vs Java's 16 — a denser graph.
		maxConn := g.config.MMax
		if layer == 0 {
			maxConn = g.config.MMax0
		}
		var selected []hnswCandidate
		if g.config.ExtendCandidates {
			selected, err = g.selectNeighborsHeuristic(tx, queryVec, neighbors, g.config.M, layer)
			if err != nil {
				return err
			}
		} else {
			selected = g.selectNeighbors(neighbors, g.config.M)
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

	// RaBitQ centroid bootstrap: sample this vector and, once StatsThreshold have accumulated,
	// establish the rotated centroid (Java Insert.addToStatsIfNecessary). The random is
	// PK-seeded per insert (Primitives.random(primaryKey)); queryVec is the vector in the
	// current (identity, pre-centroid) transform space, which is what Java samples.
	if err := g.addToStatsIfNecessary(tx, accessInfo, queryVec, newSplittableRandomForKey(primaryKey)); err != nil {
		return err
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

	// Quantize only when a transform is active (Cosine/DotProduct: immediate rotation).
	// For a translation-preserving metric (Euclidean) the first node is pre-centroid, so
	// it is stored plain — matching Java's noOp quantizer until a centroid is sampled.
	vecBytes := g.encodeVectorBytes(queryVec, info.hasTransform())
	info.vectorBytes = vecBytes

	for layer := 0; layer <= insertLayer; layer++ {
		g.storage.saveNodeLayerDispatch(tx, layer, primaryKey, vecBytes, nil)
	}
	g.storage.saveAccessInfo(tx, info)
	return nil
}

// --- RaBitQ centroid bootstrap (Java Insert.addToStatsIfNecessary + StorageAdapter samples) ---

// aggregatedVector is one SAMPLES entry: a partial sum of `count` raw vectors. Java AggregatedVector.
type aggregatedVector struct {
	count int
	vec   []float64
}

// addToStatsIfNecessary samples the inserted vector toward establishing a RaBitQ centroid,
// matching Java Insert.addToStatsIfNecessary. Active only when RaBitQ is enabled and no centroid
// is yet established. transformedVec is the new vector in the current (identity, pre-centroid)
// transform space. The random is the insert's PK-seeded SplittableRandom, consumed in Java's
// order (sample, maintain, then the rotator seed at the transition).
func (g *hnswGraph) addToStatsIfNecessary(tx fdb.Transaction, info *hnswAccessInfo, transformedVec []float64, random *splittableRandom) error {
	if g.config.Quantizer == nil || info == nil || info.hasTransform() {
		return nil // not RaBitQ, or centroid already established
	}
	if random.nextDouble() < g.config.SampleVectorStatsProbability {
		if err := g.storage.appendSampledVector(tx, 1, transformedVec); err != nil {
			return err
		}
	}
	if random.nextDouble() >= g.config.MaintainStatsProbability {
		return nil
	}
	// Roll up: consume up to 50 samples, aggregate, append the partial back.
	samples, err := g.storage.consumeSampledVectors(tx, 50)
	if err != nil {
		return err
	}
	agg := aggregateVectors(samples)
	if agg.count == 0 {
		return nil
	}
	if err := g.storage.appendSampledVector(tx, agg.count, agg.vec); err != nil {
		return err
	}
	if agg.count < g.config.StatsThreshold {
		return nil
	}
	// Establish the centroid: rotatorSeed = random.nextLong(); centroid = -mean; rotate; transition.
	rotatorSeed := random.nextLong()
	rotator := newFhtKacRotator(rotatorSeed, g.config.NumDimensions, 10)
	centroid := make([]float64, len(agg.vec))
	for i, v := range agg.vec {
		centroid[i] = v * (-1.0 / float64(agg.count))
	}
	rotatedCentroid := rotator.apply(centroid)

	// Re-express the entry node vector in the new (rotated + translated) coordinate system, so the
	// entry node is always in the internal system while data vectors may be a mix (Java comment).
	normalize := g.config.Metric == VectorMetricCosine
	transform := newHNSWTransform(rotatorSeed, rotatedCentroid, g.config.NumDimensions, normalize)
	entryVec, derr := g.decodeStoredVector(info.vectorBytes)
	if derr != nil {
		return fmt.Errorf("hnsw stats: decode entry vector: %w", derr)
	}
	info.rotatorSeed = rotatorSeed
	info.centroid = rotatedCentroid
	// The entry node's AccessInfo copy is now expressed in the freshly-established
	// coordinate system, so it is quantized (transform active). entryVec was decoded
	// with opXform still nil (this insert is pre-centroid), i.e. as the raw vector, so
	// applying the new transform lifts it correctly. The entry node's data-subspace
	// bytes stay plain and are lifted at read like any other pre-centroid vector.
	info.vectorBytes = g.encodeVectorBytes(transform.apply(entryVec), true)
	g.storage.saveAccessInfo(tx, info)
	return g.storage.deleteAllSampledVectors(tx)
}

// aggregateVectors sums the partial vectors and their counts. Java Insert.aggregateVectors.
func aggregateVectors(samples []aggregatedVector) aggregatedVector {
	var sum []float64
	count := 0
	for _, s := range samples {
		if sum == nil {
			sum = make([]float64, len(s.vec))
		}
		for i, v := range s.vec {
			if i < len(sum) {
				sum[i] += v
			}
		}
		count += s.count
	}
	return aggregatedVector{count: count, vec: sum}
}

// appendSampledVector writes one SAMPLES entry. Key: samplesSubspace.Pack(count, uniqueBytes);
// value: Tuple{serializeVector(vec)}. Matches Java StorageAdapter.appendSampledVector — the
// per-entry count is in the key, the (raw) vector in the value. The unique key element is random
// (Java uses UUID.randomUUID); it is ignored on read, so any unique value works and does not
// affect the order-independent aggregate.
func (s *hnswStorage) appendSampledVector(tx fdb.Transaction, count int, vec []float64) error {
	var uniq [16]byte
	if _, err := cryptorand.Read(uniq[:]); err != nil {
		return fmt.Errorf("hnsw stats: sample key entropy: %w", err)
	}
	key := s.samplesSubspace.Pack(tuple.Tuple{int64(count), uniq[:]})
	value := tuple.Tuple{serializeVector(vec)}.Pack()
	tx.Set(fdb.Key(key), value)
	return nil
}

// consumeSampledVectors reads up to numMax SAMPLES entries (snapshot, reverse) and CLEARS each
// (only the consumed keys take a read-conflict, not the whole range). Java consumeSampledVectors.
func (s *hnswStorage) consumeSampledVectors(tx fdb.Transaction, numMax int) ([]aggregatedVector, error) {
	r, err := fdb.PrefixRange(s.samplesSubspace.Bytes())
	if err != nil {
		return nil, fmt.Errorf("hnsw stats: samples range: %w", err)
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: numMax, Reverse: true, Mode: fdb.StreamingModeIterator}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("hnsw stats: read samples: %w", err)
	}
	out := make([]aggregatedVector, 0, len(kvs))
	for _, kv := range kvs {
		av, perr := s.aggregatedVectorFromRaw(kv.Key, kv.Value)
		if perr != nil {
			continue
		}
		if cerr := tx.AddReadConflictKey(fdb.Key(kv.Key)); cerr != nil {
			return nil, cerr
		}
		tx.Clear(fdb.Key(kv.Key))
		out = append(out, av)
	}
	return out, nil
}

// deleteAllSampledVectors clears the whole SAMPLES subspace. Java deleteAllSampledVectors.
func (s *hnswStorage) deleteAllSampledVectors(tx fdb.Transaction) error {
	r, err := fdb.PrefixRange(s.samplesSubspace.Bytes())
	if err != nil {
		return fmt.Errorf("hnsw stats: samples range: %w", err)
	}
	tx.ClearRange(r)
	return nil
}

// aggregatedVectorFromRaw decodes one SAMPLES entry: count from key[0], vector from the value.
func (s *hnswStorage) aggregatedVectorFromRaw(key, value []byte) (aggregatedVector, error) {
	keyTuple, err := s.samplesSubspace.Unpack(fdb.Key(key))
	if err != nil || len(keyTuple) < 1 {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: unpack sample key")
	}
	count, ok := keyTuple[0].(int64)
	if !ok {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: sample count not int64")
	}
	valTuple, err := tuple.Unpack(value)
	if err != nil || len(valTuple) < 1 {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: unpack sample value")
	}
	vb, ok := valTuple[0].([]byte)
	if !ok {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: sample vector not bytes")
	}
	vec, derr := deserializeVector(vb)
	if derr != nil {
		return aggregatedVector{}, derr
	}
	return aggregatedVector{count: int(count), vec: vec}, nil
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
	// Lift pre-centroid plain vectors into the current coordinate system during repair
	// distance computations (see opXform). nil when no centroid is established.
	g.opXform = g.buildTransform(accessInfo)
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

	// Delete from each layer (0..epLayer), repairing the deleted node's neighbors against
	// a SHARED candidate set with deterministic, PK-seeded sampling — matching Java
	// Delete.deleteFromLayers/deleteFromLayer. The random is seeded once from the deleted
	// key (Primitives.random(primaryKey)) and threaded across layers so the sampling is
	// reproducible (the previous per-neighbor repair used Go-map iteration + global
	// rand.Shuffle and was non-deterministic).
	random := newSplittableRandomForKey(primaryKey)
	var newEntryPK tuple.Tuple
	var newEntryVecBytes []byte
	newEntryLayer := 0
	for layer := 0; layer <= epLayer; layer++ {
		_, neighbors, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, primaryKey)
		if loadErr != nil {
			if e := hnswFatal(loadErr); e != nil {
				return e // transient read error — abort and retry, don't skip the layer
			}
			continue // not present at this layer
		}
		// neighbors aliases the per-tx cache entry; copy before the repair mutates the cache.
		nbCopy := make([][]byte, len(neighbors))
		copy(nbCopy, neighbors)

		entryPK, entryVec, derr := g.deleteFromLayerRepair(tx, layer, primaryKey, nbCopy, random)
		if derr != nil {
			return derr
		}
		// Java returns one potential entry per layer (the first repair candidate) and picks
		// the first non-null from the highest layer down — i.e. the highest layer that had a
		// candidate. Overwriting each layer yields exactly that.
		if entryPK != nil {
			newEntryPK = entryPK
			newEntryVecBytes = entryVec
			newEntryLayer = layer
		}
	}

	// Update the entry point if we deleted it.
	if tupleEqual(accessInfo.pk, primaryKey) {
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

// repairCandidate is a node in the shared deletion-repair candidate set: its primary key,
// nested-span form, resolved vector bytes (for re-saving) and decoded vector (for distance),
// and its current neighbor spans (to remove the deleted node and to extend with repaired ins).
type repairCandidate struct {
	pk        tuple.Tuple
	span      []byte
	vecBytes  []byte
	vec       []float64
	neighbors [][]byte
}

// findDeletionRepairCandidates builds the shared repair candidate set for a delete at one
// layer: the deleted node's existing neighbors (PRIMARY, always kept) plus their neighbors
// (SECONDARY) sampled deterministically at rate (efRepair - |primary|)/numCandidates via the
// PK-seeded random. Matches Java Delete.findDeletionRepairCandidates + Primitives.neighbors/
// findNeighborReferences (a LinkedHashSet: primary first, then distinct non-primary neighbors
// in discovery order; one random.nextDouble() per non-primary candidate, in that order).
func (g *hnswGraph) findDeletionRepairCandidates(tx fdb.ReadTransaction, layer int, deletedPK tuple.Tuple, deletedNeighbors [][]byte, random *splittableRandom) ([]repairCandidate, error) {
	deletedSpan := nestPK(deletedPK)

	// PRIMARY = the deleted node's neighbors, EXCLUDING the deleted node itself. Java's
	// shouldUsePrimaryCandidateForRepair rejects a reference to the node being deleted
	// (Primitives.findNeighborReferences seeds the set with the initial node, then the
	// predicate filters it out) — so a stale self-reference never re-enters the candidate
	// set and gets re-saved. We exclude it directly when forming the primary set.
	primarySet := make(map[string]bool, len(deletedNeighbors))
	var primarySpans [][]byte
	for _, s := range deletedNeighbors {
		if bytes.Equal(s, deletedSpan) || primarySet[string(s)] {
			continue
		}
		primarySet[string(s)] = true
		primarySpans = append(primarySpans, s)
	}

	// PRIMARY nodes loaded so we can walk their neighbors for the secondary set.
	primaryBatch := g.storage.loadNodeLayerBatchDispatch(tx, layer, primarySpans)

	// Ordered ref set (LinkedHashSet): primary first, then distinct non-primary
	// neighbors-of-primary (excluding the deleted node).
	refs := make([][]byte, 0, len(primarySpans))
	refsSeen := make(map[string]bool, len(primarySpans))
	for _, s := range primarySpans {
		refs = append(refs, s)
		refsSeen[string(s)] = true
	}
	for _, r := range primaryBatch {
		if r.err != nil {
			if e := hnswFatal(r.err); e != nil {
				return nil, e
			}
			continue // primary neighbor absent — skip its sub-neighbors
		}
		for _, nb := range r.neighbors {
			key := string(nb)
			if primarySet[key] || refsSeen[key] || bytes.Equal(nb, deletedSpan) {
				continue
			}
			refs = append(refs, nb)
			refsSeen[key] = true
		}
	}

	numCandidates := len(refs)
	var sampleRate float64
	if numCandidates > 0 {
		sampleRate = float64(g.config.EfRepair-len(primarySpans)) / float64(numCandidates)
	}

	// Sample: primary (initials) always accepted (no random call); others accepted if
	// sampleRate >= 1 or random.nextDouble() < sampleRate — in ref order, matching Java.
	var selectedSpans [][]byte
	for _, ref := range refs {
		if primarySet[string(ref)] {
			selectedSpans = append(selectedSpans, ref)
			continue
		}
		if bytes.Equal(ref, deletedSpan) {
			continue
		}
		if sampleRate >= 1 || random.nextDouble() < sampleRate {
			selectedSpans = append(selectedSpans, ref)
		}
	}

	// Load the selected candidates; filterExisting (skip absent ones). Preserve selectedSpans
	// order so the candidate set (and the first-candidate entry choice) is deterministic.
	batch := g.storage.loadNodeLayerBatchDispatch(tx, layer, selectedSpans)
	resByKey := make(map[string]nodeResult, len(batch))
	for _, r := range batch {
		resByKey[string(r.span)] = r
	}
	var candidates []repairCandidate
	for _, span := range selectedSpans {
		r, ok := resByKey[string(span)]
		if !ok || r.err != nil {
			if ok && r.err != nil {
				if e := hnswFatal(r.err); e != nil {
					return nil, e
				}
			}
			continue // absent candidate — filtered out
		}
		pk, derr := decodeNestedPK(r.span)
		if derr != nil {
			return nil, derr
		}
		resolved := g.storage.resolveVectorBytes(layer, pk, r.vecBytes)
		vec, decErr := g.decodeStoredVector(resolved)
		if decErr != nil {
			continue
		}
		candidates = append(candidates, repairCandidate{pk: pk, span: r.span, vecBytes: resolved, vec: vec, neighbors: r.neighbors})
	}
	return candidates, nil
}

// deleteFromLayerRepair deletes deletedPK from one layer and repairs its neighbors against
// the shared candidate set, then prunes and persists. Returns the first repair candidate as
// Java's potential new entry node (Delete.deleteFromLayer).
func (g *hnswGraph) deleteFromLayerRepair(tx fdb.Transaction, layer int, deletedPK tuple.Tuple, deletedNeighbors [][]byte, random *splittableRandom) (tuple.Tuple, []byte, error) {
	deletedSpan := nestPK(deletedPK)
	candidates, err := g.findDeletionRepairCandidates(tx, layer, deletedPK, deletedNeighbors, random)
	if err != nil {
		return nil, nil, err
	}

	// changeSet: candidate span → its mutable neighbor spans (deleted node removed up front,
	// Java initializeCandidateChangeSetMap → DeleteNeighborsChangeSet). `changed` tracks which
	// candidates were modified so we only re-write those (Java: changeSet.hasChanges()).
	changeSet := make(map[string][][]byte, len(candidates))
	candByKey := make(map[string]repairCandidate, len(candidates))
	changed := make(map[string]bool, len(candidates))
	order := make([]string, 0, len(candidates))
	for _, c := range candidates {
		key := string(c.span)
		nbs := make([][]byte, 0, len(c.neighbors))
		removed := false
		for _, nb := range c.neighbors {
			if bytes.Equal(nb, deletedSpan) {
				removed = true
				continue
			}
			nbs = append(nbs, nb)
		}
		changeSet[key] = nbs
		candByKey[key] = c
		order = append(order, key)
		if removed {
			changed[key] = true
		}
	}

	// Repair each primary neighbor P of the deleted node against the shared candidate set:
	// select M diverse candidates for P (Java repairInsForNeighborNode → selectCandidates(M))
	// and add P to each selected candidate's neighbor list (reconnecting P into the graph).
	for _, pSpan := range deletedNeighbors {
		p, ok := candByKey[string(pSpan)]
		if !ok {
			continue // primary neighbor not a (still-existing) candidate
		}
		cands := make([]hnswCandidate, 0, len(candidates))
		for _, c := range candidates {
			if bytes.Equal(c.span, pSpan) {
				continue // not P itself
			}
			cands = append(cands, hnswCandidate{pkSpan: c.span, vecBytes: c.vecBytes, vec: c.vec, dist: vectorDistance(p.vec, c.vec, g.config.Metric)})
		}
		selected := g.selectNeighbors(cands, g.config.M)
		for _, sc := range selected {
			scKey := string(sc.pkSpan)
			if !containsSpan(changeSet[scKey], pSpan) {
				changeSet[scKey] = append(changeSet[scKey], pSpan)
				changed[scKey] = true
			}
		}
	}

	// Prune each modified candidate to MMax/MMax0 (Java pruneNeighborsIfNecessary).
	maxConn := g.config.MMax
	if layer == 0 {
		maxConn = g.config.MMax0
	}
	for _, key := range order {
		if !changed[key] || len(changeSet[key]) <= maxConn {
			continue
		}
		c := candByKey[key]
		pkList := make([]tuple.Tuple, 0, len(changeSet[key]))
		for _, sp := range changeSet[key] {
			pk, derr := decodeNestedPK(sp)
			if derr != nil {
				return nil, nil, derr
			}
			pkList = append(pkList, pk)
		}
		prunedPKs, perr := g.pruneNeighbors(tx, c.vec, pkList, maxConn, layer)
		if perr != nil {
			return nil, nil, perr
		}
		pruned := make([][]byte, len(prunedPKs))
		for i, pk := range prunedPKs {
			pruned[i] = nestPK(pk)
		}
		changeSet[key] = pruned
	}

	// Delete the node, then persist every modified candidate.
	g.storage.deleteNodeLayerDispatch(tx, layer, deletedPK)
	for _, key := range order {
		if !changed[key] {
			continue
		}
		c := candByKey[key]
		nbPKs := make([]tuple.Tuple, 0, len(changeSet[key]))
		for _, sp := range changeSet[key] {
			pk, derr := decodeNestedPK(sp)
			if derr != nil {
				return nil, nil, derr
			}
			nbPKs = append(nbPKs, pk)
		}
		g.storage.saveNodeLayerDispatch(tx, layer, c.pk, c.vecBytes, nbPKs)
	}

	// New entry node = the first candidate (Java returns the first of candidateReferencesMap,
	// which preserves the candidate insertion order; guaranteed to exist).
	if len(order) > 0 {
		first := candByKey[order[0]]
		return first.pk, first.vecBytes, nil
	}
	return nil, nil, nil
}

// containsSpan reports whether spans contains the given span (byte-equal).
func containsSpan(spans [][]byte, span []byte) bool {
	for _, s := range spans {
		if bytes.Equal(s, span) {
			return true
		}
	}
	return false
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
	dataSubspace    subspace.Subspace
	accessSubspace  subspace.Subspace
	samplesSubspace subspace.Subspace // SUBSPACE_PREFIX_SAMPLES (0x02): RaBitQ centroid-bootstrap samples
	config          HNSWConfig
	cache           map[string]*parsedNode // FDB key → parsed node (nil = not found)
	stats           *HNSWStats             // optional I/O counters (nil = no tracking)

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

		// Unpack the key to get (layer, sourcePK, neighborPK).
		keyTuple, unpackErr := fastSubspaceUnpack(kv.Key, len(s.dataSubspace.Bytes()))
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

// splittableRandom is a stateful port of java.util.SplittableRandom (seeded form,
// fixed GOLDEN_GAMMA). HNSW delete uses it for deterministic, PK-seeded sampling of
// repair candidates — matching Java's Primitives.random(primaryKey) =
// new SplittableRandom(splitMixLong(primaryKey.hashCode())). splitMixLong(x) already
// computes mix64(x + GOLDEN_GAMMA) — exactly SplittableRandom.nextLong() for state x —
// so nextLong returns splitMixLong(seed) and then advances seed by GOLDEN_GAMMA.
type splittableRandom struct {
	seed int64
}

// newSplittableRandomForKey seeds the RNG from a primary key, matching Java's
// Primitives.random(pk): new SplittableRandom(splitMixLong(pk.hashCode())).
func newSplittableRandomForKey(primaryKey tuple.Tuple) *splittableRandom {
	return &splittableRandom{seed: splitMixLong(int64(javaHashCode(primaryKey.Pack())))}
}

func (r *splittableRandom) nextLong() int64 {
	result := splitMixLong(r.seed) // mix64(seed + GOLDEN_GAMMA) == Java nextLong() for this seed
	r.seed += -0x61C8864680B583EB  // advance seed by GOLDEN_GAMMA (nextSeed)
	return result
}

// nextDouble returns a double in [0, 1), matching SplittableRandom.nextDouble() =
// (nextLong() >>> 11) * 0x1.0p-53.
func (r *splittableRandom) nextDouble() float64 {
	return float64(uint64(r.nextLong())>>11) * (1.0 / float64(int64(1)<<53))
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
