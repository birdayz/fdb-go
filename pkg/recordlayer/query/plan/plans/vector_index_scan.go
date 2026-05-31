package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryVectorIndexPlan is a K-nearest-neighbor scan over a VECTOR
// (HNSW) index. It is the physical plan the vector index match candidate
// emits for a query of the shape
//
//	SELECT ... FROM t
//	WHERE <partition keys = ...>
//	QUALIFY ROW_NUMBER() OVER (PARTITION BY <keys> ORDER BY <distance>(vec, q)) <= k
//
// Unlike RecordQueryIndexPlan (a BY_VALUE prefix scan), this plan executes a
// BY_DISTANCE scan: the partition-equality prefix selects the independent HNSW
// graph, and the graph is traversed for the k nearest neighbors of the query
// vector. Mirrors the scan Java's VectorIndexScanMatchCandidate lowers to
// (VectorIndexScanComparisons + a DistanceRankValueComparison).
//
// Leaf node — reads index entries (primaryKey + distance) directly from the
// HNSW subspace; a fetch step loads the base records.
type RecordQueryVectorIndexPlan struct {
	indexName string
	// prefixComparisons are the partition-key equality ranges that select the
	// HNSW partition (one per partition column, left-to-right).
	prefixComparisons []*predicates.ComparisonRange
	// queryVector evaluates to the search vector ([]float64 / []float32).
	queryVector values.Value
	// k evaluates to the number of nearest neighbors to return (top-K).
	k values.Value
	// efSearch is the HNSW search-quality knob (nil = index/engine default).
	efSearch *int
	// isReturningVectors requests the scan return vector payloads (nil = no).
	isReturningVectors *bool
	recordTypes        []string
	flowedType         values.Type
}

// NewRecordQueryVectorIndexPlan constructs a BY_DISTANCE vector index scan.
func NewRecordQueryVectorIndexPlan(
	indexName string,
	prefixComparisons []*predicates.ComparisonRange,
	queryVector values.Value,
	k values.Value,
	efSearch *int,
	isReturningVectors *bool,
	recordTypes []string,
	flowedType values.Type,
) *RecordQueryVectorIndexPlan {
	if flowedType == nil {
		flowedType = values.UnknownType
	}
	comps := make([]*predicates.ComparisonRange, len(prefixComparisons))
	copy(comps, prefixComparisons)
	return &RecordQueryVectorIndexPlan{
		indexName:          indexName,
		prefixComparisons:  comps,
		queryVector:        queryVector,
		k:                  k,
		efSearch:           efSearch,
		isReturningVectors: isReturningVectors,
		recordTypes:        dedupSortedStrings(recordTypes),
		flowedType:         flowedType,
	}
}

// GetIndexName returns the vector index name.
func (p *RecordQueryVectorIndexPlan) GetIndexName() string { return p.indexName }

// GetPrefixComparisons returns the partition-key equality ranges.
func (p *RecordQueryVectorIndexPlan) GetPrefixComparisons() []*predicates.ComparisonRange {
	return p.prefixComparisons
}

// GetQueryVector returns the search-vector Value.
func (p *RecordQueryVectorIndexPlan) GetQueryVector() values.Value { return p.queryVector }

// GetK returns the top-K Value.
func (p *RecordQueryVectorIndexPlan) GetK() values.Value { return p.k }

// GetEfSearch returns the HNSW ef_search knob (nil = default).
func (p *RecordQueryVectorIndexPlan) GetEfSearch() *int { return p.efSearch }

// IsReturningVectors reports whether the scan returns vector payloads.
func (p *RecordQueryVectorIndexPlan) IsReturningVectors() bool {
	return p.isReturningVectors != nil && *p.isReturningVectors
}

// GetRecordTypes returns the covered record types.
func (p *RecordQueryVectorIndexPlan) GetRecordTypes() []string { return p.recordTypes }

// GetResultType returns the flowed row type.
func (p *RecordQueryVectorIndexPlan) GetResultType() values.Type { return p.flowedType }

// GetChildren returns nil — vector scans are leaves.
func (p *RecordQueryVectorIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// EqualsWithoutChildren compares index name, prefix comparison shape, and
// the query-vector / k / ef_search node-info.
func (p *RecordQueryVectorIndexPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryVectorIndexPlan)
	if !ok || p.indexName != o.indexName {
		return false
	}
	if !typeEquals(p.flowedType, o.flowedType) {
		return false
	}
	if !eqIntPtr(p.efSearch, o.efSearch) || p.IsReturningVectors() != o.IsReturningVectors() {
		return false
	}
	if len(p.recordTypes) != len(o.recordTypes) {
		return false
	}
	for i := range p.recordTypes {
		if p.recordTypes[i] != o.recordTypes[i] {
			return false
		}
	}
	if len(p.prefixComparisons) != len(o.prefixComparisons) {
		return false
	}
	for i := range p.prefixComparisons {
		if p.prefixComparisons[i].GetRangeType() != o.prefixComparisons[i].GetRangeType() {
			return false
		}
	}
	return values.ValuesStructurallyEqual(p.queryVector, o.queryVector) &&
		values.ValuesStructurallyEqual(p.k, o.k)
}

// HashCodeWithoutChildren mixes index name + prefix comparison shape.
func (p *RecordQueryVectorIndexPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("vectorindexplan|"))
	h.Write([]byte(p.indexName))
	h.Write([]byte{0})
	for _, cr := range p.prefixComparisons {
		h.Write([]byte{byte(cr.GetRangeType())})
	}
	return h.Sum64()
}

// Explain renders a one-line label. The "VectorIndexScan" token is the
// EXPLAIN-pin anchor used by the conformance tests.
func (p *RecordQueryVectorIndexPlan) Explain() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("VectorIndexScan(%s, BY_DISTANCE, prefix=[", p.indexName))
	for i, cr := range p.prefixComparisons {
		if i > 0 {
			b.WriteString(", ")
		}
		switch cr.GetRangeType() {
		case predicates.ComparisonRangeEquality:
			b.WriteString("=")
		case predicates.ComparisonRangeInequality:
			b.WriteString("<>")
		default:
			b.WriteString("*")
		}
	}
	b.WriteString("], k=")
	b.WriteString(values.ExplainValue(p.k))
	if p.efSearch != nil {
		b.WriteString(fmt.Sprintf(", ef_search=%d", *p.efSearch))
	}
	b.WriteString(")")
	return b.String()
}

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

var _ RecordQueryPlan = (*RecordQueryVectorIndexPlan)(nil)
