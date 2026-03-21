package recordlayer

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
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
	tx fdb.Transaction,
	store indexStoreContext,
) *vectorIndexMaintainer {
	config := parseHNSWConfig(index)
	return &vectorIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		hnswSubspace:            hnswSubspace,
		hnswConfig:              config,
		storageCache:            make(map[string]*hnswStorage),
	}
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
			// Java's EUCLIDEAN_SQUARE uses squared L2 (same as our Euclidean).
			config.Metric = VectorMetricEuclidean
		default:
			// EUCLIDEAN_METRIC and any other value defaults to Euclidean.
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
	if v, ok := index.Options["hnswUseRaBitQ"]; ok {
		config.UseRaBitQ = v == "true"
	}
	if v, ok := index.Options["hnswRaBitQNumExBits"]; ok {
		var numExBits int
		if n, _ := fmt.Sscanf(v, "%d", &numExBits); n == 1 && numExBits >= 1 && numExBits <= 8 {
			config.RaBitQNumExBits = numExBits
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
func (m *vectorIndexMaintainer) splitPrefixAndVector(entry indexEntry) (prefix tuple.Tuple, vector []float64) {
	if len(entry.value) > 0 {
		// KeyWithValue index: key is prefix, value is vector.
		return entry.key, tupleToVector(entry.value)
	}
	// Non-KWV index: no prefix, entire key is the vector.
	return nil, tupleToVector(entry.key)
}

// Update handles insert/delete/update for the VECTOR index.
// When the index has a prefix (via KeyWithValueExpression), each unique prefix
// value gets its own independent HNSW graph stored at a separate subspace.
func (m *vectorIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate vector index %q for old record: %w", m.index.Name, err)
		}
		for _, entry := range entries {
			prefix, vector := m.splitPrefixAndVector(entry)
			if vector == nil {
				continue
			}
			storage := m.getStorageForPrefix(prefix)
			graph := NewHNSWGraph(storage, m.hnswConfig)
			if err := graph.Delete(m.tx, entry.primaryKey); err != nil {
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
			prefix, vector := m.splitPrefixAndVector(entry)
			if vector == nil {
				continue
			}
			storage := m.getStorageForPrefix(prefix)
			graph := NewHNSWGraph(storage, m.hnswConfig)
			if err := graph.Insert(m.tx, entry.primaryKey, vector); err != nil {
				return err
			}
		}
	}

	return nil
}

// extractVector extracts float64 vector from an index entry.
// The vector is expected to be stored as the value portion of a KeyWithValue expression,
// or as sequential float64/int64 elements in the entry key.
func extractVector(entry indexEntry) []float64 {
	// Try entry value first (KeyWithValue covering index).
	if len(entry.value) > 0 {
		return tupleToVector(entry.value)
	}
	// Fall back to entry key.
	return tupleToVector(entry.key)
}

// tupleToVector converts tuple elements to a float64 vector.
// Handles both raw bytes (from KeyWithValueExpression on a bytes field) and
// numeric tuple elements (from expressions on int/float fields).
func tupleToVector(t tuple.Tuple) []float64 {
	if len(t) == 0 {
		return nil
	}
	// If the tuple contains a single bytes element, treat it as a serialized vector.
	// This is the common case for KeyWithValueExpression(field("vector_data"), 0)
	// where vector_data is a bytes proto field.
	if len(t) == 1 {
		if b, ok := t[0].([]byte); ok {
			vec, err := deserializeVector(b)
			if err == nil {
				return vec
			}
		}
	}
	vec := make([]float64, 0, len(t))
	for _, elem := range t {
		switch v := elem.(type) {
		case []byte:
			// Deserialize bytes as a vector.
			deserialized, err := deserializeVector(v)
			if err != nil {
				return nil
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
			return nil // non-numeric element
		}
	}
	return vec
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

	return m.scanByDistanceWithParams(prefix, queryVector, k, efSearch, continuation, scanProperties)
}

// scanByDistanceWithParams performs the actual kNN search and returns a cursor.
// prefix scopes the search to a specific prefix partition (nil for no prefix).
func (m *vectorIndexMaintainer) scanByDistanceWithParams(
	prefix tuple.Tuple,
	queryVector []float64,
	k, efSearch int,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	storage := m.getStorageForPrefix(prefix)
	graph := NewHNSWGraph(storage, m.hnswConfig)

	results, err := graph.Search(m.tx.Snapshot(), queryVector, k, efSearch)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}

	entries := make([]*IndexEntry, len(results))
	for i, r := range results {
		entries[i] = &IndexEntry{
			Index: m.index,
			Key:   r.PrimaryKey,
			Value: tuple.Tuple{r.Distance},
		}
	}

	return FromListWithContinuation(entries, continuation)
}

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

// SearchKNN performs a k-nearest-neighbor search on the HNSW graph.
// prefix scopes the search to a specific prefix partition (nil for no prefix).
// Returns results sorted by distance (closest first).
func (m *vectorIndexMaintainer) SearchKNN(prefix tuple.Tuple, queryVector []float64, k, efSearch int) ([]VectorSearchResult, error) {
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
		vResults[i] = VectorSearchResult{
			PrimaryKey: r.PrimaryKey,
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
	maintainer := store.getIndexMaintainer(index)
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
	maintainer := store.getIndexMaintainer(index)
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
