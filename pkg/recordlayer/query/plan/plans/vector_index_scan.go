package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	// k evaluates to the K in the QUALIFY rank predicate (ROW_NUMBER() <op> K).
	// The number of rows scanned is derived from k AND rankType — see
	// adjustedLimit / Java's VectorIndexScanBounds.getAdjustedLimit:
	// LESS_THAN → k-1, LESS_THAN_OR_EQUAL → k.
	k values.Value
	// rankType is the distance-rank comparison operator
	// (ComparisonDistanceRankLessThan or ...LessThanOrEq). It determines the
	// scan limit relative to k; EQUALS is rejected upstream and never reaches
	// here.
	rankType predicates.ComparisonType
	// efSearch is the HNSW search-quality knob (nil = index/engine default).
	efSearch *int
	// isReturningVectors requests the scan return vector payloads (nil = no).
	isReturningVectors *bool
	recordTypes        []string
	flowedType         values.Type
	// partitionColumns are the partition-key column names in key order
	// (columnNames[:partitionCount]). Carried for the planner's
	// partition-residual safety check (compensationSafeForYield): a residual
	// over these columns, contiguous immediately after the bound equality
	// prefix, selects WHOLE partitions and so composes safely as a Filter above
	// a self-limiting per-partition top-k scan. Not wire-serialized and not a
	// distinguishing plan property (fully determined by indexName) — excluded
	// from Equals/HashCode.
	partitionColumns []string
	// orderedStream selects the VBASE distance-ordered mode (RFC-156 Phase B).
	// When true the scan does NOT self-limit to k: it emits its bounded
	// re-ranked horizon (the engine's candidate pool / re-rank budget c) in
	// ascending distance order, and a Filter + Limit ABOVE the scan compose the
	// final "k nearest matching rows" (Limit(k) → Filter(residual) → ordered
	// scan). Distance ordering is INTRINSIC to this node — it is parameter-
	// dependent on the query vector and is NOT a generic Cascades Ordering
	// property (RemoveSortRule is untouched) and NOT a generic sort operator.
	// When false (the default) the scan is self-limiting: it performs search(k)
	// and returns the top-k, exactly as before — restored for the no-residual /
	// partition-only cases by SinkLimitIntoVectorScanRule folding a Limit(k)
	// directly above the scan into this mode (byte-for-byte the legacy path).
	orderedStream bool
}

// NewRecordQueryVectorIndexPlan constructs a BY_DISTANCE vector index scan.
func NewRecordQueryVectorIndexPlan(
	indexName string,
	prefixComparisons []*predicates.ComparisonRange,
	queryVector values.Value,
	k values.Value,
	rankType predicates.ComparisonType,
	efSearch *int,
	isReturningVectors *bool,
	recordTypes []string,
	flowedType values.Type,
) *RecordQueryVectorIndexPlan {
	if flowedType == nil {
		flowedType = values.UnknownType
	}
	// Default to <= (top-K) when unspecified — the common QUALIFY shape.
	if rankType != predicates.ComparisonDistanceRankLessThan &&
		rankType != predicates.ComparisonDistanceRankLessThanOrEq {
		rankType = predicates.ComparisonDistanceRankLessThanOrEq
	}
	comps := make([]*predicates.ComparisonRange, len(prefixComparisons))
	copy(comps, prefixComparisons)
	return &RecordQueryVectorIndexPlan{
		indexName:          indexName,
		prefixComparisons:  comps,
		queryVector:        queryVector,
		k:                  k,
		rankType:           rankType,
		efSearch:           efSearch,
		isReturningVectors: isReturningVectors,
		recordTypes:        dedupSortedStrings(recordTypes),
		flowedType:         flowedType,
	}
}

// GetRankType returns the distance-rank comparison operator (LessThan or
// LessThanOrEq). Used by the executor to derive the scan limit from k.
func (p *RecordQueryVectorIndexPlan) GetRankType() predicates.ComparisonType {
	return p.rankType
}

// IsOrderedStream reports whether the scan runs in VBASE distance-ordered mode
// (emits its re-ranked horizon in distance order, does NOT self-limit to k).
// See the orderedStream field doc. RFC-156 Phase B.
func (p *RecordQueryVectorIndexPlan) IsOrderedStream() bool { return p.orderedStream }

// WithOrderedStream returns a copy of the plan in distance-ordered (non-self-
// limiting) mode. The k binding is retained for the SinkLimitIntoVectorScanRule
// fold and cost estimation, but the executor ignores it in this mode.
func (p *RecordQueryVectorIndexPlan) WithOrderedStream() *RecordQueryVectorIndexPlan {
	if p.orderedStream {
		return p
	}
	c := *p
	c.orderedStream = true
	return &c
}

// WithSelfLimiting returns a copy of the plan in self-limiting (top-k) mode.
// SinkLimitIntoVectorScanRule produces this when a Limit(k) sits DIRECTLY above
// an ordered-stream scan with no intervening residual Filter — restoring the
// legacy one-shot search(k) path.
func (p *RecordQueryVectorIndexPlan) WithSelfLimiting() *RecordQueryVectorIndexPlan {
	if !p.orderedStream {
		return p
	}
	c := *p
	c.orderedStream = false
	return &c
}

// GetIndexName returns the vector index name.
func (p *RecordQueryVectorIndexPlan) GetIndexName() string { return p.indexName }

// GetPrefixComparisons returns the partition-key equality ranges.
func (p *RecordQueryVectorIndexPlan) GetPrefixComparisons() []*predicates.ComparisonRange {
	return p.prefixComparisons
}

// GetPartitionColumns returns the partition-key column names in key order.
func (p *RecordQueryVectorIndexPlan) GetPartitionColumns() []string { return p.partitionColumns }

// WithPartitionColumns returns a copy of the plan carrying the partition-key
// column names (columnNames[:partitionCount]). Set by the match candidate's
// ToScanPlan so the planner can certify a partition-column residual as safe.
func (p *RecordQueryVectorIndexPlan) WithPartitionColumns(cols []string) *RecordQueryVectorIndexPlan {
	c := *p
	c.partitionColumns = append([]string(nil), cols...)
	return &c
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
	if p.rankType != o.rankType {
		return false
	}
	if p.orderedStream != o.orderedStream {
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
	h.Write([]byte{byte(p.rankType)})
	if p.orderedStream {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
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
	b.WriteString("], ")
	if p.orderedStream {
		// Distance-ordered mode: the scan emits its re-ranked horizon in
		// distance order and does NOT consume k — a Limit ABOVE applies k. The
		// "ordered" token (no "rank<="/"rank<") is the EXPLAIN-pin that k is not
		// sunk into the scan.
		b.WriteString("ordered")
	} else {
		if p.rankType == predicates.ComparisonDistanceRankLessThan {
			b.WriteString("rank<")
		} else {
			b.WriteString("rank<=")
		}
		b.WriteString(values.ExplainValue(p.k))
	}
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
