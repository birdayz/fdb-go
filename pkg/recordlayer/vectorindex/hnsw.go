// Portions derived from FoundationDB Record Layer (InliningStorageAdapter.java,
// Insert.java, NodeReferenceWithVector.java, Config.java, and others),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"errors"
	"fmt"
	"math"

	"fdb.dev/internal/recordlayer/rabitq"
	"fdb.dev/internal/recordlayer/tuplefast"
	"fdb.dev/internal/recordlayer/vectorcodec"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

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
//     sequentially on the same fdb.WritableTransaction. There are no concurrent async
//     futures within a single transaction.
//   - Cross-transaction correctness is handled by FDB's serializable isolation.
//     Concurrent transactions modifying the same HNSW nodes conflict on shared
//     keys, FDB aborts one, and the retry loop re-executes. Insert is idempotent
//     (checks existence first), so retries are safe.
//
// In short: Java needs the lock because of its async concurrency model within a
// single transaction. Go's synchronous model eliminates the problem entirely.

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

// raBitQuantizerAdmits refuses an operation where Java constructs its
// RaBitQuantizer for it and the constructor refuses the configured extra-bit
// count: Primitives.quantizer (Primitives.java:174-182), which every insert
// into a non-empty graph, every delete of a present node and every search of a
// non-empty graph calls, constructs one once the access info can use RaBitQ
// (a centroid is established, or at once for a metric that is not
// translation-preserving), and Insert.firstInsert (Insert.java:274-285) for such
// a metric. Java's Config admits 1 to 15 extra bits and the quantizer 1 to 8
// (RaBitQuantizer.java:76), so 9 to 15 is refused exactly there: a Euclidean
// index serves inserts until its centroid is established, and a search of an
// empty graph or a delete of an absent node is served. Guava's
// checkArgument carries no message; Go's error names the range.
func (g *hnswGraph) raBitQuantizerAdmits(info *hnswAccessInfo) error {
	q, ok := g.config.Quantizer.(*rabitq.Quantizer)
	if !ok || q == nil || info == nil || !info.hasTransform() || rabitq.ValidNumExBits(q.NumExBits()) {
		return nil
	}
	return &recordlayer.IllegalArgumentError{Message: fmt.Sprintf("RaBitQ encodes 1 to 8 extra bits, not %d", q.NumExBits())}
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

// hnswVector carries the coordinate provenance that the encoding tag cannot
// express: access-info DOUBLE vectors are already transformed, whereas plain
// node/edge bytes are raw. Encoded vectors are always in current coordinates.
type hnswVector struct {
	data        []byte
	transformed bool
}

// computeDistance computes the distance between a query in current coordinates and stored
// vector bytes. When a quantizer is configured and the stored bytes match its
// type byte, uses the quantizer for fast approximate distance. Otherwise
// deserializes and computes exact distance.
func (g *hnswGraph) computeDistance(query []float64, storedVecBytes []byte) float64 {
	return g.vectorDistance(query, hnswVector{data: storedVecBytes})
}

func (g *hnswGraph) vectorDistance(query []float64, vector hnswVector) float64 {
	storedVecBytes := vector.data
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
	// Plain node/edge bytes retain raw coordinates and need the current storage
	// transform (CompactStorageAdapter.java:199). Access-info and new candidates
	// already carry transformed coordinates; their DOUBLE tag alone cannot tell
	// these representations apart. The query is in current coordinates.
	if g.opXform != nil && !vector.transformed {
		vec, err := deserializeVector(storedVecBytes)
		if err != nil {
			return math.Inf(1)
		}
		return vectorDistance(query, g.opXform.apply(vec), g.config.Metric)
	}
	// The query and vector already share coordinates.
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

// pairDistance is the operation's DistanceEstimator between two stored or
// candidate vectors (RaBitDistanceEstimator.distance): when exactly one is
// RaBitQ-encoded the other is the query of an estimate; otherwise the metric
// runs on the decoded vectors (an encoded pair on their reconstructions).
// a and b are av and bv decoded.
func (g *hnswGraph) pairDistance(a []float64, av hnswVector, b []float64, bv hnswVector) float64 {
	encoded := func(v hnswVector) bool {
		return g.config.Quantizer != nil && len(v.data) > 0 && v.data[0] == g.config.Quantizer.GetTypeByte()
	}
	switch aEnc, bEnc := encoded(av), encoded(bv); {
	case !aEnc && bEnc:
		return g.vectorDistance(a, bv)
	case aEnc && !bEnc:
		return g.vectorDistance(b, av)
	}
	return vectorDistance(a, b, g.config.Metric)
}

// decodeStoredVector extracts an approximate []float64 from stored vector bytes.
// For raw vectors (DOUBLE/FLOAT/HALF), this is exact deserialization.
// For quantized vectors, delegates to the configured quantizer's Decode method
// to reconstruct an approximate vector for pairwise distance computations
// in the neighbor selection heuristic.
func (g *hnswGraph) decodeStoredVector(storedVecBytes []byte) ([]float64, error) {
	return g.decodeVector(hnswVector{data: storedVecBytes})
}

func (g *hnswGraph) decodeVector(vector hnswVector) ([]float64, error) {
	storedVecBytes := vector.data
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
	// Lift raw plain node/edge bytes, but never transform an access-info or new
	// candidate vector twice. Pairwise distances require current coordinates.
	if g.opXform != nil && !vector.transformed {
		vec = g.opXform.apply(vec)
	}
	return vec, nil
}

// accessVectorBytes preserves encoded candidates and lifts only raw plain
// vectors. Access-info is not a node write: Java does not quantize this copy.
func (g *hnswGraph) accessVectorBytes(vector hnswVector) ([]byte, error) {
	if vector.transformed || g.opXform == nil ||
		(g.config.Quantizer != nil && len(vector.data) > 0 && vector.data[0] == g.config.Quantizer.GetTypeByte()) {
		return vector.data, nil
	}
	decoded, err := g.decodeVector(vector)
	if err != nil {
		return nil, err
	}
	return serializeVector(decoded), nil
}

// nodeVectorBytes matches the compact/inline adapters' quantizer.encode call.
// Already encoded bytes must survive unchanged; reconstructing and requantizing
// them changes both their calibration and their integer codes.
func (g *hnswGraph) nodeVectorBytes(vector hnswVector) ([]byte, error) {
	if g.config.Quantizer == nil || g.opXform == nil ||
		(len(vector.data) > 0 && vector.data[0] == g.config.Quantizer.GetTypeByte()) {
		return vector.data, nil
	}
	decoded, err := g.decodeVector(vector)
	if err != nil {
		return nil, err
	}
	return g.config.Quantizer.Encode(decoded), nil
}

// saveNodeLayer converts candidate representations at the write boundary. The
// storage cache sees only bytes actually written, never access-info bytes labeled
// as an ordinary plain node vector.
func (g *hnswGraph) saveNodeLayer(tx fdb.WritableTransaction, layer int, pk tuple.Tuple, vector hnswVector, neighbors []tuple.Tuple, neighborVectors []hnswVector) error {
	if !g.storage.isInliningLayer(layer) {
		data, err := g.nodeVectorBytes(vector)
		if err != nil {
			return err
		}
		g.storage.saveNodeLayer(tx, layer, pk, data, neighbors)
		return nil
	}
	encoded := make([][]byte, len(neighbors))
	for i, neighbor := range neighbors {
		var v hnswVector
		if i < len(neighborVectors) {
			v = neighborVectors[i]
		}
		if v.data == nil {
			v.data = g.storage.resolveVectorBytes(layer, neighbor, nil)
		}
		if v.data == nil {
			return fmt.Errorf("hnsw: no vector for neighbor %v at layer %d", neighbor, layer)
		}
		var err error
		encoded[i], err = g.nodeVectorBytes(v)
		if err != nil {
			return err
		}
	}
	return g.storage.saveNodeLayerInlining(tx, layer, pk, neighbors, encoded)
}

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

	t, unpackErr := tuplefast.Unpack(data)
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
func (s *hnswStorage) saveAccessInfo(tx fdb.WritableTransaction, info *hnswAccessInfo) {
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
func (s *hnswStorage) clearAccessInfo(tx fdb.WritableTransaction) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	tx.Clear(fdb.Key(key))
}

// clearAll removes all HNSW graph data (data + access info).
func (s *hnswStorage) clearAll(tx fdb.WritableTransaction) {
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
