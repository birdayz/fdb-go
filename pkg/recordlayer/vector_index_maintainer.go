package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/rabitq"
)

// IndexOptionVectorNumDimensions specifies the number of vector dimensions.
// Matches Java's IndexOptions.HNSW_NUM_DIMENSIONS.
const IndexOptionVectorNumDimensions = "hnswNumDimensions"

// IndexOptionVectorMetric specifies the distance metric.
// Matches Java's IndexOptions.HNSW_METRIC.
const IndexOptionVectorMetric = "hnswMetric"

// IndexOptionVectorExtendCandidates controls whether the candidate set is extended
// with neighbors-of-neighbors during neighbor selection (2nd-degree exploration).
// Matches Java's IndexOptions.HNSW_EXTEND_CANDIDATES.
const IndexOptionVectorExtendCandidates = "hnswExtendCandidates"

// IndexOptionVectorKeepPrunedConnections controls whether pruned candidates are
// added back to fill up to M neighbors when the heuristic selection produces too few.
// Matches Java's IndexOptions.HNSW_KEEP_PRUNED_CONNECTIONS.
const IndexOptionVectorKeepPrunedConnections = "hnswKeepPrunedConnections"

// IndexOptionHNSWMaxNumConcurrentNodeFetches controls the maximum number of
// concurrent node fetches during search and modification operations.
// In Go's synchronous model this is not used for concurrency control, but is
// stored for Java round-trip compatibility.
// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_NODE_FETCHES.
const IndexOptionHNSWMaxNumConcurrentNodeFetches = "hnswMaxNumConcurrentNodeFetches"

// IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches controls the maximum number of
// concurrent neighborhood fetches during insert when neighbors are pruned.
// Stored for Java round-trip compatibility.
// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_NEIGHBORHOOD_FETCHES.
const IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches = "hnswMaxNumConcurrentNeighborhoodFetches"

// IndexOptionHNSWMaxNumConcurrentDeleteFromLayer controls the maximum number of
// concurrent layer deletions during deletion of a record.
// Stored for Java round-trip compatibility.
// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_DELETE_FROM_LAYER.
const IndexOptionHNSWMaxNumConcurrentDeleteFromLayer = "hnswMaxNumConcurrentDeleteFromLayer"

// IndexOptionHNSWM specifies the connectivity factor M for the HNSW graph.
// Matches Java's IndexOptions.HNSW_M.
const IndexOptionHNSWM = "hnswM"

// IndexOptionHNSWMMax specifies the maximum number of connections for non-zero layers.
// Matches Java's IndexOptions.HNSW_M_MAX.
const IndexOptionHNSWMMax = "hnswMMax"

// IndexOptionHNSWMMax0 specifies the maximum number of connections for layer 0.
// Matches Java's IndexOptions.HNSW_M_MAX_0.
const IndexOptionHNSWMMax0 = "hnswMMax0"

// IndexOptionHNSWEfConstruction specifies the search factor used during index construction.
// Matches Java's IndexOptions.HNSW_EF_CONSTRUCTION.
const IndexOptionHNSWEfConstruction = "hnswEfConstruction"

// IndexOptionHNSWUseInlining controls whether vector data is inlined into the HNSW node.
// Matches Java's IndexOptions.HNSW_USE_INLINING.
const IndexOptionHNSWUseInlining = "hnswUseInlining"

// IndexOptionHNSWEfRepair specifies the search factor used during repair operations.
// Matches Java's IndexOptions.HNSW_EF_REPAIR.
const IndexOptionHNSWEfRepair = "hnswEfRepair"

// IndexOptionHNSWUseRaBitQ enables RaBitQ quantization for approximate nearest neighbor.
// Matches Java's IndexOptions.HNSW_USE_RABITQ.
const IndexOptionHNSWUseRaBitQ = "hnswUseRaBitQ"

// IndexOptionHNSWRaBitQNumExBits specifies the number of extra bits for RaBitQ.
// Matches Java's IndexOptions.HNSW_RABITQ_NUM_EX_BITS.
const IndexOptionHNSWRaBitQNumExBits = "hnswRaBitQNumExBits"

// IndexOptionHNSWSampleVectorStatsProbability controls the probability of sampling vector stats.
// Runtime-only option, safe to change without rebuild.
// Matches Java's IndexOptions.HNSW_SAMPLE_VECTOR_STATS_PROBABILITY.
const IndexOptionHNSWSampleVectorStatsProbability = "hnswSampleVectorStatsProbability"

// IndexOptionHNSWMaintainStatsProbability controls the probability of maintaining stats.
// Runtime-only option, safe to change without rebuild.
// Matches Java's IndexOptions.HNSW_MAINTAIN_STATS_PROBABILITY.
const IndexOptionHNSWMaintainStatsProbability = "hnswMaintainStatsProbability"

// IndexOptionHNSWStatsThreshold specifies the minimum number of vectors for stats.
// Runtime-only option, safe to change without rebuild.
// Matches Java's IndexOptions.HNSW_STATS_THRESHOLD.
const IndexOptionHNSWStatsThreshold = "hnswStatsThreshold"

// vectorIndexMaintainer maintains a VECTOR index using an HNSW graph.
// Wire-compatible with Java's VectorIndexMaintainer.
//
// Prefix partitioning: when the index uses a KeyWithValueExpression with
// splitPoint > 0 (e.g., KWV(Concat(Field("group"), Field("vec")), 1)),
// each unique prefix (the key portion) gets an independent HNSW graph
// stored under hnswSubspace.Sub(prefix...). This matches Java's behavior
// where grouped key expressions produce per-prefix HNSW graphs.
type vectorIndexMaintainer struct {
	standardIndexMaintainer
	hnswSubspace subspace.Subspace
	hnswConfig   HNSWConfig
	storageCache map[string]*hnswStorage // subspace bytes → cached storage
}

func newVectorIndexMaintainer(
	index *Index,
	indexSubspace, hnswSubspace subspace.Subspace,
	tx fdb.WritableTransaction,
	store indexStoreContext,
) (*vectorIndexMaintainer, error) {
	config := parseHNSWConfig(index)
	// Validate the config (ranges + cross-field invariants), matching Java's Config
	// constructor which throws on an invalid config. Without this Go would silently build
	// a graph from a config Java rejects — e.g. m > mMax (new node selects more than the
	// pruning cap → churn) or efRepair < m.
	if err := ValidateHNSWConfig(config); err != nil {
		return nil, fmt.Errorf("vector index %q: %w", index.Name, err)
	}
	return &vectorIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		hnswSubspace:            hnswSubspace,
		hnswConfig:              config,
		storageCache:            make(map[string]*hnswStorage),
	}, nil
}

// parseHNSWConfig reads HNSW configuration from index options.
func parseHNSWConfig(index *Index) HNSWConfig {
	numDims := 128 // default
	if v, ok := index.Options[IndexOptionVectorNumDimensions]; ok {
		if n, _ := fmt.Sscanf(v, "%d", &numDims); n != 1 {
			numDims = 128
		}
	}
	config := DefaultHNSWConfig(numDims)
	if v, ok := index.Options[IndexOptionVectorMetric]; ok {
		switch v {
		case "COSINE_METRIC", "cosine":
			config.Metric = VectorMetricCosine
		case "DOT_PRODUCT_METRIC", "inner_product":
			config.Metric = VectorMetricInnerProduct
		case "EUCLIDEAN_SQUARE_METRIC":
			// Squared L2 (no sqrt), not a true metric — matches Java's
			// EUCLIDEAN_SQUARE_METRIC (MetricDefinition.EuclideanSquareMetric).
			config.Metric = VectorMetricEuclideanSquare
		default:
			// EUCLIDEAN_METRIC (true L2, sqrt) and any other value default to Euclidean.
			config.Metric = VectorMetricEuclidean
		}
	}
	if v, ok := index.Options[IndexOptionVectorExtendCandidates]; ok {
		config.ExtendCandidates = v == "true"
	}
	if v, ok := index.Options[IndexOptionVectorKeepPrunedConnections]; ok {
		config.KeepPrunedConnections = v == "true"
	}
	if v, ok := index.Options["hnswEfRepair"]; ok {
		var efRepair int
		if n, _ := fmt.Sscanf(v, "%d", &efRepair); n == 1 && efRepair >= 0 {
			config.EfRepair = efRepair
		}
	}
	if v, ok := index.Options["hnswUseInlining"]; ok {
		config.UseInlining = v == "true"
	}
	if v, ok := index.Options[IndexOptionHNSWSampleVectorStatsProbability]; ok {
		var p float64
		if n, _ := fmt.Sscanf(v, "%g", &p); n == 1 && p > 0 && p <= 1 {
			config.SampleVectorStatsProbability = p
		}
	}
	if v, ok := index.Options[IndexOptionHNSWMaintainStatsProbability]; ok {
		var p float64
		if n, _ := fmt.Sscanf(v, "%g", &p); n == 1 && p > 0 && p <= 1 {
			config.MaintainStatsProbability = p
		}
	}
	if v, ok := index.Options[IndexOptionHNSWStatsThreshold]; ok {
		var t int
		if n, _ := fmt.Sscanf(v, "%d", &t); n == 1 && t > 0 {
			config.StatsThreshold = t
		}
	}
	if v, ok := index.Options["hnswUseRaBitQ"]; ok && v == "true" {
		numExBits := 4
		if v, ok := index.Options["hnswRaBitQNumExBits"]; ok {
			var n int
			if cnt, _ := fmt.Sscanf(v, "%d", &n); cnt == 1 && n >= 1 && n <= 8 {
				numExBits = n
			}
		}
		config.Quantizer = rabitq.NewQuantizer(rabitq.Metric(config.Metric), numExBits)
	}
	if v, ok := index.Options[IndexOptionHNSWM]; ok {
		var m int
		if n, _ := fmt.Sscanf(v, "%d", &m); n == 1 && m >= 2 && m <= 128 {
			config.M = m
		}
	}
	if v, ok := index.Options[IndexOptionHNSWMMax]; ok {
		var mMax int
		if n, _ := fmt.Sscanf(v, "%d", &mMax); n == 1 && mMax >= 2 && mMax <= 256 {
			config.MMax = mMax
		}
	}
	if v, ok := index.Options[IndexOptionHNSWMMax0]; ok {
		var mMax0 int
		if n, _ := fmt.Sscanf(v, "%d", &mMax0); n == 1 && mMax0 >= 2 && mMax0 <= 512 {
			config.MMax0 = mMax0
		}
	}
	if v, ok := index.Options[IndexOptionHNSWEfConstruction]; ok {
		var efConstruction int
		if n, _ := fmt.Sscanf(v, "%d", &efConstruction); n == 1 && efConstruction >= 1 && efConstruction <= 2000 {
			config.EfConstruction = efConstruction
		}
	}
	// Concurrency limits — stored for Java round-trip compatibility.
	// Go's synchronous FDB model doesn't use these for concurrency control.
	// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_NODE_FETCHES etc.
	if v, ok := index.Options[IndexOptionHNSWMaxNumConcurrentNodeFetches]; ok {
		var n int
		if cnt, _ := fmt.Sscanf(v, "%d", &n); cnt == 1 && n > 0 && n <= 64 {
			config.MaxNumConcurrentNodeFetches = n
		}
	}
	if v, ok := index.Options[IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches]; ok {
		var n int
		if cnt, _ := fmt.Sscanf(v, "%d", &n); cnt == 1 && n > 0 && n <= 20 {
			config.MaxNumConcurrentNeighborhoodFetches = n
		}
	}
	if v, ok := index.Options[IndexOptionHNSWMaxNumConcurrentDeleteFromLayer]; ok {
		var n int
		if cnt, _ := fmt.Sscanf(v, "%d", &n); cnt == 1 && n > 0 && n <= 10 {
			config.MaxNumConcurrentDeleteFromLayer = n
		}
	}
	return config
}

// getSubspaceForPrefix returns the HNSW subspace scoped to the given prefix.
// If the prefix is empty (no grouping), returns the base hnswSubspace.
// Each unique prefix value gets its own independent HNSW graph.
func (m *vectorIndexMaintainer) getSubspaceForPrefix(prefix tuple.Tuple) subspace.Subspace {
	if len(prefix) == 0 {
		return m.hnswSubspace
	}
	// Convert tuple elements to []TupleElement for Sub() variadic call.
	args := make([]tuple.TupleElement, len(prefix))
	for i, v := range prefix {
		args[i] = v
	}
	return m.hnswSubspace.Sub(args...)
}

// getStorageForPrefix returns a cached hnswStorage for the given prefix subspace.
// Reuses existing storage (and its parsed node cache) within the same maintainer lifetime.
func (m *vectorIndexMaintainer) getStorageForPrefix(prefix tuple.Tuple) *hnswStorage {
	ss := m.getSubspaceForPrefix(prefix)
	key := string(ss.Bytes())
	if cached, ok := m.storageCache[key]; ok {
		return cached
	}
	storage := newHNSWStorage(ss, m.hnswConfig)
	m.storageCache[key] = storage
	return storage
}

// splitPrefixAndVector extracts the prefix (grouping key) and vector from an
// index entry. For KeyWithValueExpression indexes:
//   - entry.key = prefix columns (the key portion before splitPoint)
//   - entry.value = vector data (the value portion after splitPoint)
//
// For non-KWV indexes (e.g., Concat(Field("x"), Field("y"))):
//   - entry.key = the entire vector (no prefix)
//   - entry.value = nil
//
// NOTE: Java divergence — Java's VectorIndexMaintainer.getKeyWithValueExpression()
// at line 375 throws if the root expression is not a KeyWithValueExpression.
// We accept non-KWV expressions for backwards compatibility with existing tests
// that use Concat/Field directly as the root expression. This is more permissive
// than Java but functionally equivalent for non-grouped vector indexes.
func (m *vectorIndexMaintainer) splitPrefixAndVector(entry indexEntry) (prefix tuple.Tuple, vector []float64, err error) {
	if len(entry.value) > 0 {
		// KeyWithValue index: key is prefix, value is vector.
		vec, verr := tupleToVector(entry.value)
		return entry.key, vec, verr
	}
	// Non-KWV index: no prefix, entire key is the vector.
	vec, verr := tupleToVector(entry.key)
	return nil, vec, verr
}

// Update handles insert/delete/update for the VECTOR index.
// When the index has a prefix (via KeyWithValueExpression), each unique prefix
// value gets its own independent HNSW graph stored at a separate subspace.
//
// Primary keys are trimmed via Index.TrimPrimaryKey() before storing in the HNSW
// graph, matching Java's VectorIndexMaintainer.updateIndexKeys() which calls
// state.index.trimPrimaryKey(primaryKeyParts) at line 343.
func (m *vectorIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	// Each entry mutates exactly one per-prefix HNSW graph; serialize only that
	// graph, matching Java, which takes doWithWriteLock(LockIdentifier(rtSubspace))
	// where rtSubspace = indexSubspace.subspace(prefixKey) — a PER-PREFIX lock, not
	// a whole-index one. (The lock lives on the per-transaction context, so it only
	// orders mutations within a transaction; distinct prefix graphs never contend,
	// and neither do distinct transactions.) Locking the whole index here was a
	// Go-only over-serialization that blocked concurrent per-prefix builds.
	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate vector index %q for old record: %w", m.index.Name, err)
		}
		for _, entry := range entries {
			// Java's remove branch never decodes the vector — it removes by primary key
			// (graph.Delete keys on the PK) and only skips a null vectorBytes. So on
			// delete: skip a truly absent/null vector, but for a PRESENT vector —
			// decodable OR not — proceed to remove by PK. Decode-and-error belongs to
			// the insert path alone; erroring here would make a record saved-unindexed
			// by an older binary un-deletable, a Go-only divergence.
			prefix, vector, verr := m.splitPrefixAndVector(entry)
			if verr == nil && vector == nil {
				continue // absent/null vector — nothing was indexed, nothing to remove
			}
			trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
			if err != nil {
				return fmt.Errorf("trim primary key for vector index %q delete: %w", m.index.Name, err)
			}
			if err := m.withPrefixWriteLock(prefix, func(graph *hnswGraph) error {
				return graph.Delete(m.tx, trimmedPK)
			}); err != nil {
				return err
			}
		}
	}

	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate vector index %q for new record: %w", m.index.Name, err)
		}
		for _, entry := range entries {
			prefix, vector, verr := m.splitPrefixAndVector(entry)
			if verr != nil {
				return fmt.Errorf("vector index %q: decode vector for new record: %w", m.index.Name, verr)
			}
			if vector == nil {
				continue
			}
			trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
			if err != nil {
				return fmt.Errorf("trim primary key for vector index %q insert: %w", m.index.Name, err)
			}
			if err := m.withPrefixWriteLock(prefix, func(graph *hnswGraph) error {
				return graph.Insert(m.tx, trimmedPK, vector)
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// withPrefixWriteLock runs fn against the prefix's HNSW graph while holding the
// per-prefix write lock — the same scope as Java's
// doWithWriteLock(LockIdentifier(indexSubspace.subspace(prefixKey))). For an
// unprefixed index this is the index subspace itself.
func (m *vectorIndexMaintainer) withPrefixWriteLock(prefix tuple.Tuple, fn func(*hnswGraph) error) error {
	lockKey := string(m.getSubspaceForPrefix(prefix).Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)
	storage := m.getStorageForPrefix(prefix)
	return fn(NewHNSWGraph(storage, m.hnswConfig))
}

// tupleToVector converts tuple elements to a float64 vector. Returns:
//   - (nil, nil)   for an absent/null vector — an empty tuple or a null component —
//     which the caller skips (matches Java, which skips a null vector field);
//   - (nil, error) for a NON-null but UNDECODABLE vector (bad serialized bytes,
//     non-numeric element). Java's RealVector.fromBytes throws here and fails the
//     write; Go previously returned nil → the maintainer silently skipped it,
//     saving the record UNINDEXED (a vector search would miss the row). Surfacing
//     the error makes the write fail, matching Java and avoiding silent index
//     incompleteness.
//   - (vec, nil)   for a valid vector.
//
// Handles both raw bytes (KeyWithValueExpression on a bytes field) and numeric
// tuple elements (expressions on int/float fields).
func tupleToVector(t tuple.Tuple) ([]float64, error) {
	if len(t) == 0 {
		return nil, nil // absent vector — skip
	}
	// Single bytes element: a serialized vector (KeyWithValueExpression(field, 0)
	// on a bytes proto field). A non-null but undecodable payload is an error.
	if len(t) == 1 {
		if b, ok := t[0].([]byte); ok {
			vec, err := deserializeVector(b)
			if err != nil {
				return nil, fmt.Errorf("vector index: undecodable serialized vector: %w", err)
			}
			return vec, nil
		}
	}
	vec := make([]float64, 0, len(t))
	for _, elem := range t {
		switch v := elem.(type) {
		case nil:
			// A null component → treat the whole vector as absent (skip), not a
			// partial/undefined vector. Matches Java's null-vector handling.
			return nil, nil
		case []byte:
			deserialized, err := deserializeVector(v)
			if err != nil {
				return nil, fmt.Errorf("vector index: undecodable serialized vector element: %w", err)
			}
			vec = append(vec, deserialized...)
		case float64:
			vec = append(vec, v)
		case float32:
			vec = append(vec, float64(v))
		case int64:
			vec = append(vec, float64(v))
		case int:
			vec = append(vec, float64(v))
		default:
			return nil, fmt.Errorf("vector index: non-numeric element %T in vector key", elem)
		}
	}
	return vec, nil
}

// UpdateWhileWriteOnly handles updates during WRITE_ONLY state.
// VECTOR insert is idempotent (same PK replaces).
func (m *vectorIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan rejects the TupleRange-based scan API for VECTOR indexes.
// Matches Java's VectorIndexMaintainer.scan(IndexScanType, TupleRange, ...) which
// throws IllegalStateException("index maintainer does not support this scan api").
// Use ScanByDistance for kNN search, or SearchKNN for direct results.
func (m *vectorIndexMaintainer) Scan(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return &errorCursor[*IndexEntry]{
		err: fmt.Errorf("VECTOR index %q does not support TupleRange scan; use ScanVectorIndex with BY_DISTANCE", m.index.Name),
	}
}

// VectorScanBounds carries the parameters for a BY_DISTANCE kNN scan.
// Matches Java's VectorIndexScanBounds (query vector, k limit, efSearch, options).
type VectorScanBounds struct {
	QueryVector []float64 // The query vector for similarity search.
	K           int       // Number of nearest neighbors to return.
	EfSearch    int       // Search exploration factor (0 = auto from K).
}

// ScanByDistance performs a kNN search and returns results as a cursor of IndexEntry.
// Each IndexEntry has Key = primaryKey and Value = tuple{distance}.
// Matches Java's VectorIndexMaintainer.scan(VectorIndexScanBounds, ...) which
// returns a ListCursor of IndexEntry from kNearestNeighborsSearch.
//
// For prefix-partitioned indexes, the prefix is encoded as additional elements
// at the end of TupleRange.Low (after the query vector bytes).
func (m *vectorIndexMaintainer) ScanByDistance(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	// Extract VectorScanBounds from TupleRange.
	// Convention: Low = tuple{queryVectorBytes, prefix...} (serialized vector as []byte, followed by optional prefix elements),
	//             High = tuple{k, efSearch} (int64 values).
	if scanRange.Low == nil || len(scanRange.Low) < 1 {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("VECTOR BY_DISTANCE scan requires query vector in TupleRange.Low"),
		}
	}

	vecBytes, ok := scanRange.Low[0].([]byte)
	if !ok {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("VECTOR BY_DISTANCE scan: TupleRange.Low[0] must be []byte (serialized query vector)"),
		}
	}

	queryVector, err := deserializeVector(vecBytes)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("VECTOR BY_DISTANCE scan: invalid query vector: %w", err)}
	}

	// Extract optional prefix from remaining Low elements.
	var prefix tuple.Tuple
	if len(scanRange.Low) > 1 {
		prefix = tuple.Tuple(scanRange.Low[1:])
	}

	k := 10 // default
	efSearch := 0
	if scanRange.High != nil {
		if len(scanRange.High) >= 1 {
			if kVal, ok := asInt64(scanRange.High[0]); ok && kVal > 0 {
				k = int(kVal)
			}
		}
		if len(scanRange.High) >= 2 {
			if efVal, ok := asInt64(scanRange.High[1]); ok && efVal > 0 {
				efSearch = int(efVal)
			}
		}
	}

	if efSearch <= 0 {
		// Auto-compute efSearch from k, matching Java's heuristic.
		efSearch = min(max(4*k, 64), max(k, 400))
	}

	// Multi-partition fan-out (partial prefix) is dispatched inside
	// scanByDistanceWithParams — the shared chokepoint for both this entry point
	// and ScanVectorIndexWithPrefix — so it is not branched here.
	return m.scanByDistanceWithParams(prefix, queryVector, k, efSearch, continuation, scanProperties)
}

// partitionSize returns the number of leading partition (key) columns of the
// vector index — the KeyWithValueExpression split point — or 0 if the index is
// not a KeyWithValueExpression (unpartitioned). Mirrors Java's
// prefixSize = getKeyWithValueExpression(rootExpression).getSplitPoint().
func (m *vectorIndexMaintainer) partitionSize() int {
	if kwv, ok := m.index.RootExpression.(*KeyWithValueExpression); ok {
		return kwv.SplitPoint()
	}
	return 0
}

// scanByDistanceWithParams performs the actual kNN search and returns a cursor.
// prefix scopes the search to a specific prefix partition (nil for no prefix).
//
// Each IndexEntry matches Java's VectorIndexMaintainer.toIndexEntry():
//   - Key = (prefix..., trimmedPK...) — prefix prepended to the PK from HNSW
//   - Value = (vectorRawBytes) or (nil) — the vector data, or nil for RaBitQ
//
// Supports continuation-based pagination via VectorIndexScanContinuation protobuf,
// matching Java's continuation format.
func (m *vectorIndexMaintainer) scanByDistanceWithParams(
	prefix tuple.Tuple,
	queryVector []float64,
	k, efSearch int,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	// Multi-partition fan-out: when the index is partitioned (splitPoint > 0) but
	// the bound prefix is shorter than the full partition prefix, scan every
	// matching partition (prefix skip-scan + per-partition HNSW search), matching
	// Java's VectorIndexMaintainer.scan flatMapPipelined(prefixSkipScan,
	// scanSinglePartition) (RFC-046). Sited at this chokepoint so BOTH entry
	// points — ScanByDistance (executor) and ScanVectorIndexWithPrefix (direct
	// API) — fan out on a partial prefix instead of scanning one wrong subspace.
	if pSize := m.partitionSize(); pSize > 0 && len(prefix) < pSize {
		return m.newVectorMultiPartitionCursor(prefix, queryVector, k, efSearch, pSize, continuation, scanProperties)
	}

	entries, err := m.searchOnePartition(prefix, queryVector, k, efSearch)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	return m.newVectorSearchCursor(entries, continuation, prefix)
}

// searchOnePartition runs one HNSW kNN search for the single partition
// identified by the FULL partition prefix (nil/empty for an unpartitioned
// index) and returns the top-k entries in Java's toIndexEntry layout
// (Key = prefix...+trimmedPK, Value = nil). It is both the body of the
// single-partition scan and the per-partition inner of the multi-partition
// fan-out (RFC-046). Mirrors Java's VectorIndexMaintainer.kNearestNeighborSearch
// + toIndexEntry.
func (m *vectorIndexMaintainer) searchOnePartition(prefix tuple.Tuple, queryVector []float64, k, efSearch int) ([]*IndexEntry, error) {
	if len(queryVector) != m.hnswConfig.NumDimensions {
		return nil, fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.index.Name, m.hnswConfig.NumDimensions, len(queryVector))
	}
	storage := m.getStorageForPrefix(prefix)
	graph := NewHNSWGraph(storage, m.hnswConfig)

	results, err := graph.Search(m.tx.Snapshot(), queryVector, k, efSearch)
	if err != nil {
		return nil, err
	}

	// Convert to IndexEntry slice matching Java's toIndexEntry() format:
	// Key = (prefix..., trimmedPK...), Value = (vectorRawData | nil)
	entries := make([]*IndexEntry, len(results))
	for i, r := range results {
		// Build key: prepend prefix to the PK (which is already trimmed in HNSW).
		key := make(tuple.Tuple, 0, len(prefix)+len(r.PrimaryKey))
		key = append(key, prefix...)
		key = append(key, r.PrimaryKey...)

		// Value: Java puts vector raw bytes here (or null if returnVectors=false/RaBitQ).
		// Our hnswSearchResult doesn't carry vector bytes through search, so always nil.
		// This matches Java's behavior when RaBitQ is enabled or returnVectors=false.
		value := tuple.Tuple{nil}

		entries[i] = &IndexEntry{
			Index:      m.index,
			Key:        key,
			Value:      value,
			primaryKey: m.entryFullPK(key, prefix),
		}
	}
	return entries, nil
}

// entryFullPK reconstructs and pins the full primary key for a vector index
// entry from its key alone. IndexEntry.PrimaryKey()'s default getEntryPrimaryKey
// assumes the value-index key layout (indexValues[colSize] + pk) and mis-extracts
// a vector entry's key (prefix + trimmedPK), so the PK must be set explicitly.
//
// Deriving from the key (not the HNSW search result) is what lets RESUMED entries
// — reconstructed from a continuation with no record in hand — pin the same PK as
// fresh entries (codex Finding 3). key == (prefix..., trimmedPK...), so the
// non-component-positions PK is the key with the partition prefix stripped, which
// equals the fresh path's hnswSearchResult.PrimaryKey.
func (m *vectorIndexMaintainer) entryFullPK(key, prefix tuple.Tuple) tuple.Tuple {
	if m.index.HasPrimaryKeyComponentPositions() {
		return m.index.getEntryPrimaryKey(key)
	}
	if len(prefix) <= len(key) {
		return key[len(prefix):]
	}
	return key
}

// vectorSearchCursor is a cursor over vector search results that supports
// continuation tokens matching Java's VectorIndexScanContinuation protobuf.
//
// Java's approach: serialize ALL remaining result entries into the continuation.
// On resume, deserialize and replay from the saved list (no re-search needed).
// This matches VectorIndexMaintainer.java lines 182-197 (resume) and 516-528 (create).
type vectorSearchCursor struct {
	entries    []*IndexEntry
	allEntries []*IndexEntry // all entries for continuation encoding
	pos        int
	closed     bool
}

// newVectorSearchCursor creates a vector search cursor from search results.
// If continuation is non-nil, it is parsed as a VectorIndexScanContinuation
// protobuf (Java format). On resume, results are replayed from the continuation
// rather than re-searching. prefix is threaded through so resumed entries
// reconstruct the same pinned primary key as fresh ones.
func (m *vectorIndexMaintainer) newVectorSearchCursor(entries []*IndexEntry, continuation []byte, prefix tuple.Tuple) *vectorSearchCursor {
	if len(continuation) > 0 {
		// Resume from continuation: parse the proto and replay saved entries.
		resumed, innerPos := m.parseVectorScanContinuation(continuation, prefix)
		if resumed != nil {
			return &vectorSearchCursor{
				entries:    resumed,
				allEntries: resumed,
				pos:        innerPos,
			}
		}
	}
	return &vectorSearchCursor{
		entries:    entries,
		allEntries: entries,
		pos:        0,
	}
}

func (c *vectorSearchCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	// Honor a statement deadline / cancellation while emitting (RFC-106a). The
	// kNN search itself is bounded by k/efSearch, so this only guards the emit
	// loop; per-search cost is bounded by construction, not by a scan limit.
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	if c.closed || c.pos >= len(c.entries) {
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}
	entry := c.entries[c.pos]
	c.pos++

	// Encode continuation matching Java's Continuation class.
	// Includes ALL entries (for replay on resume) + inner position.
	cont := encodeVectorScanContinuation(c.allEntries, c.pos)
	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *vectorSearchCursor) Close() error {
	c.closed = true
	return nil
}

func (c *vectorSearchCursor) IsClosed() bool { return c.closed }

// encodeVectorScanContinuation creates a VectorIndexScanContinuation protobuf.
// Matches Java's Continuation.toByteString() which serializes all entries +
// the inner ListCursor continuation (position as packed int tuple).
func encodeVectorScanContinuation(entries []*IndexEntry, innerPos int) []byte {
	contProto := &gen.VectorIndexScanContinuation{}
	for _, e := range entries {
		contProto.IndexEntries = append(contProto.IndexEntries,
			&gen.VectorIndexScanContinuation_IndexEntry{
				Key:   e.Key.Pack(),
				Value: e.Value.Pack(),
			})
	}
	// Inner continuation: the ListCursor position as a packed int tuple.
	// Java's ListCursor continuation is just the position encoded as bytes.
	contProto.InnerContinuation = tuple.Tuple{int64(innerPos)}.Pack()

	data, err := contProto.MarshalVT()
	if err != nil {
		return nil
	}
	return data
}

// parseVectorScanContinuation parses a VectorIndexScanContinuation protobuf.
// Returns the saved entries and the inner cursor position.
// If parsing fails, returns nil (caller falls back to fresh search).
//
// Resumed entries are reconstructed to be INDISTINGUISHABLE from fresh ones
// (codex Finding 3): Index and the pinned full primary key are restored —
// derived from the persisted key via entryFullPK — so IndexEntry.PrimaryKey()
// returns the correct key on a resumed page instead of an empty tuple (which
// would fetch the wrong record / skip the remaining nearest rows).
func (m *vectorIndexMaintainer) parseVectorScanContinuation(data []byte, prefix tuple.Tuple) ([]*IndexEntry, int) {
	var contProto gen.VectorIndexScanContinuation
	if err := contProto.UnmarshalVT(data); err != nil {
		return nil, 0
	}

	entries := make([]*IndexEntry, 0, len(contProto.IndexEntries))
	for _, ie := range contProto.IndexEntries {
		key, err := fastUnpack(ie.GetKey())
		if err != nil {
			return nil, 0
		}
		value, err := fastUnpack(ie.GetValue())
		if err != nil {
			return nil, 0
		}
		entries = append(entries, &IndexEntry{
			Index:      m.index,
			Key:        key,
			Value:      value,
			primaryKey: m.entryFullPK(key, prefix),
		})
	}

	// Parse inner continuation (position).
	innerPos := 0
	if inner := contProto.GetInnerContinuation(); len(inner) > 0 {
		t, err := fastUnpack(inner)
		if err == nil && len(t) > 0 {
			if pos, ok := t[0].(int64); ok {
				innerPos = int(pos)
			}
		}
	}

	return entries, innerPos
}

// vectorMultiPartitionCursor fans a BY_DISTANCE scan out over all distinct
// partitions whose full partition prefix begins with partialPrefix, running one
// HNSW kNN search per partition and concatenating each partition's top-k. Ports
// Java's VectorIndexMaintainer.scan flatMapPipelined(prefixSkipScan,
// scanSinglePartition) for the partial-partition-prefix case (RFC-046).
//
// SQL semantics: ROW_NUMBER() OVER (PARTITION BY <keys> ...) <= k selects the
// top-k PER partition, so the union across partitions is intentionally unbounded
// — there is no global top-k re-merge. An outer SQL LIMIT, if present, rides in
// scanProperties.ExecuteProperties.ReturnedRowLimit and caps the TOTAL rows
// across partitions (Java's final skipThenLimit) — a quantity distinct from the
// per-partition k.
//
// Continuation is full cross-partition (Java-aligned): each delivered row emits
// FlatMapContinuation{OuterContinuation: pack(currentPrefix), InnerContinuation:
// <per-partition VectorIndexScanContinuation>}. On resume the saved partition is
// re-read first (its inner continuation replays the saved entries), then the
// skip-scan advances to the next distinct partition.
type vectorMultiPartitionCursor struct {
	m             *vectorIndexMaintainer
	queryVector   []float64
	k             int
	efSearch      int
	partialPrefix tuple.Tuple // the bound equality prefix (may be empty)
	partitionSize int

	// nextPartitionStart is the FDB key at/after which to look for the next
	// distinct partition prefix; rangeEnd bounds the skip-scan to partitions
	// under partialPrefix.
	nextPartitionStart fdb.Key
	rangeEnd           fdb.Key

	currentCursor *vectorSearchCursor
	currentPrefix tuple.Tuple
	// pendingInner seeds the first resumed partition's inner continuation.
	pendingInner []byte

	globalLimit      int
	totalDelivered   int
	lastContinuation []byte
	exhausted        bool
	closed           bool
}

// newVectorMultiPartitionCursor builds the fan-out cursor and, when resuming,
// seeds the skip-scan to re-read the saved partition first.
func (m *vectorIndexMaintainer) newVectorMultiPartitionCursor(
	partialPrefix tuple.Tuple,
	queryVector []float64,
	k, efSearch, partitionSize int,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	// Validate the query-vector dimension up front, before any partition is
	// matched. searchOnePartition checks this too, but only per matched
	// partition — so without this an invalid-length vector over a partial prefix
	// that matches NO partitions would return SourceExhausted instead of the
	// dimension error, unlike the full-prefix/unpartitioned paths which validate
	// before touching graph contents (codex P2). Validate once here for
	// consistent input validation regardless of how many partitions match.
	if len(queryVector) != m.hnswConfig.NumDimensions {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.index.Name, m.hnswConfig.NumDimensions, len(queryVector))}
	}

	// Enumeration subspace: partitions under partialPrefix (whole index if empty).
	base := m.getSubspaceForPrefix(partialPrefix)
	end, err := fdb.Strinc(base.Bytes())
	if err != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("VECTOR index %q multi-partition scan: %w", m.index.Name, err)}
	}

	c := &vectorMultiPartitionCursor{
		m:                  m,
		queryVector:        queryVector,
		k:                  k,
		efSearch:           efSearch,
		partialPrefix:      partialPrefix,
		partitionSize:      partitionSize,
		nextPartitionStart: fdb.Key(base.Bytes()),
		rangeEnd:           fdb.Key(end),
	}
	if scanProperties.ExecuteProperties.ReturnedRowLimit > 0 {
		c.globalLimit = scanProperties.ExecuteProperties.ReturnedRowLimit
	}

	// Resume: unpack the outer prefix and seed the skip-scan to re-read that
	// partition first (inclusive start), replaying its inner continuation. The
	// findNextPartition read then returns resumePrefix and advances past it, so
	// the next partition follows once the resumed one drains.
	if len(continuation) > 0 {
		var fm gen.FlatMapContinuation
		if uerr := fm.UnmarshalVT(continuation); uerr == nil && fm.OuterContinuation != nil {
			if resumePrefix, perr := fastUnpack(fm.OuterContinuation); perr == nil {
				c.nextPartitionStart = fdb.Key(m.getSubspaceForPrefix(resumePrefix).Bytes())
				c.pendingInner = fm.InnerContinuation
			}
		}
	}
	return c
}

func (c *vectorMultiPartitionCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		// Global row limit (outer SQL LIMIT) across all partitions. Return the
		// last emitted continuation (a valid mid-stream resume point), never an
		// end continuation (ReturnLimitReached must not be end).
		if c.globalLimit > 0 && c.totalDelivered >= c.globalLimit {
			cont := c.lastContinuation
			if len(cont) == 0 {
				cont = []byte{}
			}
			return NewResultNoNext[*IndexEntry](ReturnLimitReached, &BytesContinuation{bytes: cont}), nil
		}

		// Drain the current partition's cursor.
		if c.currentCursor != nil {
			res, err := c.currentCursor.OnNext(ctx)
			if err != nil {
				return RecordCursorResult[*IndexEntry]{}, err
			}
			if res.HasNext() {
				c.totalDelivered++
				cont, werr := c.wrapContinuation(res.GetContinuation())
				if werr != nil {
					return RecordCursorResult[*IndexEntry]{}, werr
				}
				c.lastContinuation = cont
				return NewResultWithValue(res.GetValue(), &BytesContinuation{bytes: cont}), nil
			}
			_ = c.currentCursor.Close()
			c.currentCursor = nil
		}

		if c.exhausted {
			return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
		}

		// Advance to the next distinct partition.
		fullPrefix, found, err := c.findNextPartition()
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		if !found {
			c.exhausted = true
			return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
		}
		c.currentPrefix = fullPrefix

		var entries []*IndexEntry
		var inner []byte
		// len() > 0, not != nil: an empty-but-non-nil InnerContinuation would pass
		// a nil check, make newVectorSearchCursor take the fresh path with nil
		// entries, and silently skip the resumed partition (@claude Finding 1).
		if len(c.pendingInner) > 0 {
			// Resumed partition: replay from the saved inner continuation; no
			// fresh HNSW search needed (matches Java's replay-from-continuation).
			inner = c.pendingInner
			c.pendingInner = nil
		} else {
			entries, err = c.m.searchOnePartition(fullPrefix, c.queryVector, c.k, c.efSearch)
			if err != nil {
				return RecordCursorResult[*IndexEntry]{}, err
			}
		}
		c.currentCursor = c.m.newVectorSearchCursor(entries, inner, fullPrefix)
	}
}

// wrapContinuation wraps a per-partition continuation in a FlatMapContinuation
// carrying the current full partition prefix as the outer continuation.
//
// Errors are PROPAGATED, never swallowed into a nil result: a nil continuation
// on a HasNext row reads downstream as end-of-scan, so swallowing a marshal
// failure here would silently truncate results (return fewer rows than exist
// with no error) — a silent-data-loss path (@claude review). In practice neither
// step fails — the inner is a BytesContinuation whose ToBytes is infallible, and
// FlatMapContinuation marshals two []byte fields — but we surface any failure as
// a cursor error rather than a short read.
func (c *vectorMultiPartitionCursor) wrapContinuation(inner RecordCursorContinuation) ([]byte, error) {
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("VECTOR index %q multi-partition continuation: inner: %w", c.m.index.Name, err)
	}
	fm := &gen.FlatMapContinuation{
		OuterContinuation: c.currentPrefix.Pack(),
		InnerContinuation: innerBytes,
	}
	data, err := fm.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("VECTOR index %q multi-partition continuation: marshal: %w", c.m.index.Name, err)
	}
	return data, nil
}

// findNextPartition reads one key at/after nextPartitionStart within the
// partialPrefix range, extracts the first partitionSize tuple elements as the
// next distinct full partition prefix, and advances nextPartitionStart past that
// partition's entire subspace. Mirrors Java's nextPrefixTuple / the multidim
// prefixSkipScanCursor.findNextPrefix.
func (c *vectorMultiPartitionCursor) findNextPartition() (tuple.Tuple, bool, error) {
	rng := fdb.KeyRange{Begin: c.nextPartitionStart, End: c.rangeEnd}
	kvs, err := c.m.tx.GetRange(rng, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: %w", c.m.index.Name, err)
	}
	if len(kvs) == 0 {
		return nil, false, nil
	}

	t, err := fastSubspaceUnpack(kvs[0].Key, len(c.m.hnswSubspace.Bytes()))
	if err != nil {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: unpack key: %w", c.m.index.Name, err)
	}
	// A key under the HNSW subspace with fewer than partitionSize leading
	// elements is malformed — surface it as an error rather than silently
	// terminating the fan-out (which would skip every partition after it).
	// The write path always writes full partition prefixes, so this never fires
	// in practice (@claude Finding 2).
	if len(t) < c.partitionSize {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: key has %d tuple elements, need partition prefix of %d",
			c.m.index.Name, len(t), c.partitionSize)
	}
	prefix := make(tuple.Tuple, c.partitionSize)
	copy(prefix, t[:c.partitionSize])

	// Advance nextPartitionStart past this partition's entire subspace.
	prefixEnd, err := fdb.Strinc(c.m.getSubspaceForPrefix(prefix).Bytes())
	if err != nil {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: %w", c.m.index.Name, err)
	}
	c.nextPartitionStart = fdb.Key(prefixEnd)
	return prefix, true, nil
}

func (c *vectorMultiPartitionCursor) Close() error {
	c.closed = true
	if c.currentCursor != nil {
		return c.currentCursor.Close()
	}
	return nil
}

func (c *vectorMultiPartitionCursor) IsClosed() bool { return c.closed }

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
		Low:          tuple.Tuple{serializeVector(queryVector)},
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
	low := tuple.Tuple{serializeVector(queryVector)}
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
// inflation and kc override (spfresh-reviewer / Torvalds Phase B NAK). The
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
	low := tuple.Tuple{serializeVector(queryVector)}
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

// SearchKNN performs a k-nearest-neighbor search on the HNSW graph.
// prefix scopes the search to a specific prefix partition (nil for no prefix).
// Returns results sorted by distance (closest first).
//
// The HNSW graph stores trimmed primary keys (via TrimPrimaryKey). This method
// reconstructs full primary keys using getEntryPrimaryKey so callers can use
// them directly with LoadRecord.
func (m *vectorIndexMaintainer) SearchKNN(prefix tuple.Tuple, queryVector []float64, k, efSearch int) ([]VectorSearchResult, error) {
	// Acquire read lock on HNSW subspace — prevent graph mutations during search.
	// Matches Java's VectorIndexMaintainer.scan() which acquires a read lock.
	lockKey := string(m.hnswSubspace.Bytes())
	m.store.AcquireReadLock(lockKey)
	defer m.store.ReleaseReadLock(lockKey)

	// Guard: query vector dimension must match the index's configured dimensions.
	if len(queryVector) != m.hnswConfig.NumDimensions {
		return nil, fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.index.Name, m.hnswConfig.NumDimensions, len(queryVector))
	}
	// Guard: if index has a prefix (KWV splitPoint > 0), caller MUST provide one.
	// Searching without a prefix on a grouped index returns empty (queries the
	// base subspace which has no data), silently producing wrong results.
	if len(prefix) == 0 {
		if kwv, ok := m.index.RootExpression.(*KeyWithValueExpression); ok && kwv.SplitPoint() > 0 {
			return nil, fmt.Errorf("VECTOR index %q is prefix-partitioned (splitPoint=%d): "+
				"use SearchVectorIndexWithPrefix to provide a prefix", m.index.Name, kwv.SplitPoint())
		}
	}
	storage := m.getStorageForPrefix(prefix)
	graph := NewHNSWGraph(storage, m.hnswConfig)

	results, err := graph.Search(m.tx.Snapshot(), queryVector, k, efSearch)
	if err != nil {
		return nil, err
	}

	vResults := make([]VectorSearchResult, len(results))
	for i, r := range results {
		// Reconstruct full PK from the trimmed PK stored in HNSW.
		// When primaryKeyComponentPositions is set (PK overlaps index expression),
		// some PK components are in the prefix (index key) and need reconstruction.
		// Otherwise, the PK in HNSW is already the full PK.
		var fullPK tuple.Tuple
		if m.index.HasPrimaryKeyComponentPositions() {
			entryKey := make(tuple.Tuple, 0, len(prefix)+len(r.PrimaryKey))
			entryKey = append(entryKey, prefix...)
			entryKey = append(entryKey, r.PrimaryKey...)
			fullPK = m.index.getEntryPrimaryKey(entryKey)
		} else {
			fullPK = r.PrimaryKey
		}

		vResults[i] = VectorSearchResult{
			PrimaryKey: fullPK,
			Distance:   r.Distance,
		}
	}
	return vResults, nil
}

// DeleteWhere clears all HNSW graph data for the given prefix.
// If the prefix is non-empty, only clears the HNSW graph for that prefix.
// If the prefix is empty, clears all HNSW graph data.
func (m *vectorIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	storage := m.getStorageForPrefix(prefix)
	storage.clearAll(m.tx)
	return nil
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
	if !store.IsIndexScannable(index.Name) {
		return &errorCursor[*IndexEntry]{
			err: &IndexNotReadableError{IndexName: index.Name, CurrentState: store.GetIndexState(index.Name)},
		}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	vm, ok := maintainer.(*vectorIndexMaintainer)
	if !ok {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("index %q (type %s) is not a VECTOR index", index.Name, index.Type),
		}
	}
	return vm.scanByDistanceWithParams(prefix, queryVector, k, efSearch, continuation, scanProperties)
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
	if !store.IsIndexScannable(index.Name) {
		return nil, &IndexNotReadableError{IndexName: index.Name, CurrentState: store.GetIndexState(index.Name)}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	vm, ok := maintainer.(*vectorIndexMaintainer)
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
