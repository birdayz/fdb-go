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

// vectorIndexMaintainer maintains a VECTOR index using an HNSW graph.
// Wire-compatible with Java's VectorIndexMaintainer.
type vectorIndexMaintainer struct {
	standardIndexMaintainer
	hnswSubspace subspace.Subspace
	hnswConfig   HNSWConfig
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
	return config
}

// Update handles insert/delete/update for the VECTOR index.
func (m *vectorIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate vector index %q for old record: %w", m.index.Name, err)
		}
		for _, entry := range entries {
			vector := extractVector(entry)
			if vector == nil {
				continue
			}
			storage := newHNSWStorage(m.hnswSubspace, m.hnswConfig)
			graph := NewHNSWGraph(storage, m.hnswConfig)
			if err := graph.Delete(m.tx, entry.primaryKey, vector); err != nil {
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
			vector := extractVector(entry)
			if vector == nil {
				continue
			}
			storage := newHNSWStorage(m.hnswSubspace, m.hnswConfig)
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

// Scan is not meaningful for VECTOR indexes — use SearchKNN instead.
// Returns an error cursor directing callers to use the proper scan method.
func (m *vectorIndexMaintainer) Scan(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return &errorCursor[*IndexEntry]{
		err: fmt.Errorf("VECTOR index %q must be queried with SearchKNN, not Scan", m.index.Name),
	}
}

// SearchKNN performs a k-nearest-neighbor search on the HNSW graph.
// Returns results sorted by distance (closest first).
func (m *vectorIndexMaintainer) SearchKNN(queryVector []float64, k, efSearch int) ([]VectorSearchResult, error) {
	storage := newHNSWStorage(m.hnswSubspace, m.hnswConfig)
	graph := NewHNSWGraph(storage, m.hnswConfig)

	results, err := graph.Search(m.tx, queryVector, k, efSearch)
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
func (m *vectorIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	storage := newHNSWStorage(m.hnswSubspace, m.hnswConfig)
	storage.clearAll(m.tx)
	return nil
}

// VectorSearchResult is a single result from a vector similarity search.
type VectorSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
}

// SearchVectorIndex performs a k-nearest-neighbor search on a VECTOR index.
// Matches Java's VectorIndexMaintainer scan with VectorIndexScanBounds.
func (store *FDBRecordStore) SearchVectorIndex(
	index *Index,
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
	return vm.SearchKNN(queryVector, k, efSearch)
}

// SearchVectorIndexRecords performs a kNN search and fetches the corresponding records.
func (store *FDBRecordStore) SearchVectorIndexRecords(
	ctx context.Context,
	index *Index,
	queryVector []float64,
	k int,
	efSearch int,
) ([]*FDBIndexedRecord, error) {
	results, err := store.SearchVectorIndex(index, queryVector, k, efSearch)
	if err != nil {
		return nil, err
	}

	records := make([]*FDBIndexedRecord, 0, len(results))
	for _, r := range results {
		rec, err := store.LoadRecord(r.PrimaryKey)
		if err != nil {
			continue // skip deleted records
		}
		if rec == nil {
			continue
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
