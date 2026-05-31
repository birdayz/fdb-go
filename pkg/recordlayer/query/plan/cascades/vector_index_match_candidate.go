package cascades

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// VectorIndexScanMatchCandidate represents a vector similarity search
// index as a match candidate. Recognises K-nearest-neighbour queries
// driven by distance-based ranking predicates and maps them to a
// vector index scan.
//
// Key differences from ValueIndexScanMatchCandidate:
//   - Carries ordering aliases separately from sargable parameters
//     (vector indexes have a different binding discipline).
//   - Sargable aliases include partition key columns; a subset
//     (parametersRequiredForBinding) must be bound.
//   - The scan plan it produces uses a vector distance comparison.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.VectorIndexScanMatchCandidate`.
type VectorIndexScanMatchCandidate struct {
	indexName   string
	recordTypes []string
	columnNames []string

	// parameters are the sargable aliases. Maps to Java's `parameters`.
	parameters []values.CorrelationIdentifier

	// orderingAliases define the ordering the index provides. Maps to
	// Java's `orderingAliases`.
	orderingAliases []values.CorrelationIdentifier

	// parametersRequiredForBinding is the subset of sargable aliases that
	// must be bound for the candidate to be valid. Maps to Java's
	// `parametersRequiredForBinding`.
	parametersRequiredForBinding map[values.CorrelationIdentifier]struct{}

	// partitionCount is the number of leading (partition-prefix) columns —
	// the KeyWithValue split point. columnNames[:partitionCount] are the
	// partition columns; columnNames[partitionCount] is the vector column.
	partitionCount int
	// distanceAlias is the sargable alias of the distance placeholder (the
	// slot the DistanceRank predicate binds to).
	distanceAlias values.CorrelationIdentifier
	// metric selects which DistanceRowNumberValue the distance placeholder uses.
	metric values.DistanceOperator

	flowedType values.Type
	unique     bool

	// createsDuplicates mirrors Java's
	// index.getRootExpression().createsDuplicates().
	createsDuplicates bool

	traversalOnce sync.Once
	traversal     *Traversal

	primaryKeyValuesOnce sync.Once
	primaryKeyValues     []values.Value
	primaryKeyColumns    []string
}

// NewVectorIndexScanMatchCandidate constructs a match candidate for a vector
// similarity search index. columnNames are all index columns in key order
// (partition columns followed by the single vector column); partitionCount is
// the KeyWithValue split point; metric selects the distance function. The
// sargable aliases (one per partition column + one distance alias) and the
// required-for-binding set (the index-only distance alias) are minted here,
// mirroring Java's VectorIndexExpansionVisitor.
func NewVectorIndexScanMatchCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	partitionCount int,
	metric values.DistanceOperator,
	flowedType values.Type,
	unique bool,
	primaryKeyColumns []string,
) *VectorIndexScanMatchCandidate {
	types := make([]string, len(recordTypes))
	copy(types, recordTypes)
	cols := make([]string, len(columnNames))
	copy(cols, columnNames)
	pkCols := make([]string, len(primaryKeyColumns))
	copy(pkCols, primaryKeyColumns)
	if partitionCount < 0 || partitionCount > len(cols) {
		partitionCount = 0
	}

	// One sargable alias per partition column; those are also the ordering
	// aliases. Plus one distance alias (the DistanceRank slot).
	ordering := make([]values.CorrelationIdentifier, partitionCount)
	params := make([]values.CorrelationIdentifier, 0, partitionCount+1)
	for i := 0; i < partitionCount; i++ {
		a := values.UniqueCorrelationIdentifier()
		ordering[i] = a
		params = append(params, a)
	}
	distanceAlias := values.UniqueCorrelationIdentifier()
	params = append(params, distanceAlias)

	return &VectorIndexScanMatchCandidate{
		indexName:                    indexName,
		recordTypes:                  types,
		columnNames:                  cols,
		parameters:                   params,
		orderingAliases:              ordering,
		parametersRequiredForBinding: map[values.CorrelationIdentifier]struct{}{distanceAlias: {}},
		partitionCount:               partitionCount,
		distanceAlias:                distanceAlias,
		metric:                       metric,
		flowedType:                   flowedType,
		unique:                       unique,
		primaryKeyColumns:            pkCols,
	}
}

// CandidateName returns the index name.
func (c *VectorIndexScanMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access.
func (c *VectorIndexScanMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		c.traversal = ExpandVectorIndex(c)
	})
	return c.traversal
}

// GetColumnNames returns the ordered column-name list.
func (c *VectorIndexScanMatchCandidate) GetColumnNames() []string { return c.columnNames }

// GetSargableAliases returns the sargable parameter aliases. Ports
// Java's VectorIndexScanMatchCandidate.getSargableAliases().
func (c *VectorIndexScanMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.parameters
}

// GetSargableAliasesRequiredForBinding returns the parameter aliases
// that must be bound for the candidate to be valid.
func (c *VectorIndexScanMatchCandidate) GetSargableAliasesRequiredForBinding() []values.CorrelationIdentifier {
	result := make([]values.CorrelationIdentifier, 0, len(c.parametersRequiredForBinding))
	for a := range c.parametersRequiredForBinding {
		result = append(result, a)
	}
	return result
}

// GetOrderingAliases returns the ordering aliases.
func (c *VectorIndexScanMatchCandidate) GetOrderingAliases() []values.CorrelationIdentifier {
	return c.orderingAliases
}

// GetRecordTypes returns the record types this index covers.
func (c *VectorIndexScanMatchCandidate) GetRecordTypes() []string { return c.recordTypes }

// IsUnique reports whether the index enforces uniqueness.
func (c *VectorIndexScanMatchCandidate) IsUnique() bool { return c.unique }

// GetBaseType returns the base record type.
func (c *VectorIndexScanMatchCandidate) GetBaseType() values.Type { return c.flowedType }

// GetColumnSize returns the number of key columns.
func (c *VectorIndexScanMatchCandidate) GetColumnSize() int { return len(c.columnNames) }

// CreatesDuplicates reports whether the index can produce duplicate
// entries per record.
func (c *VectorIndexScanMatchCandidate) CreatesDuplicates() bool { return c.createsDuplicates }

// HasAndOrderedByRecordTypeKey reports whether the index key starts
// with the record type key. Always false for vector indexes.
func (c *VectorIndexScanMatchCandidate) HasAndOrderedByRecordTypeKey() bool { return false }

// GetPrimaryKeyValues returns the primary key as a list of Values.
// Lazily computed. Returns nil if no PK columns.
func (c *VectorIndexScanMatchCandidate) GetPrimaryKeyValues() []values.Value {
	c.primaryKeyValuesOnce.Do(func() {
		if len(c.primaryKeyColumns) == 0 {
			return
		}
		pkVals := make([]values.Value, len(c.primaryKeyColumns))
		for i, col := range c.primaryKeyColumns {
			pkVals[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
		}
		c.primaryKeyValues = pkVals
	})
	return c.primaryKeyValues
}

// ComputeBoundParameterPrefixMap walks the sargable aliases and
// collects the longest prefix satisfying index scan discipline.
func (c *VectorIndexScanMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range c.parameters {
		cr, ok := bindings[alias]
		if !ok || cr == nil || cr.IsEmpty() {
			return prefix
		}
		switch cr.GetRangeType() {
		case predicates.ComparisonRangeEquality:
			prefix[alias] = cr
		case predicates.ComparisonRangeInequality:
			prefix[alias] = cr
			return prefix
		default:
			return prefix
		}
	}
	return prefix
}

// ToScanPlan converts the matched bindings into a vector (BY_DISTANCE) scan
// plan. It separates the partition-key equality bindings (which form the HNSW
// partition prefix) from the single DistanceRank binding (which carries the
// query vector + k + ef_search). Ports Java's
// VectorIndexScanMatchCandidate.toEquivalentPlan / toVectorIndexScanComparisons.
func (c *VectorIndexScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	_ bool,
) plans.RecordQueryPlan {
	// Partition prefix: the equality ranges of the partition aliases, in order.
	prefixComps := make([]*predicates.ComparisonRange, c.partitionCount)
	for i := 0; i < c.partitionCount; i++ {
		if cr, ok := prefixMap[c.parameters[i]]; ok && cr != nil {
			prefixComps[i] = cr
		} else {
			prefixComps[i] = predicates.EmptyComparisonRange()
		}
	}

	// Distance binding: extract the DistanceRank comparison (query vector + k +
	// ef_search + the rank operator) bound to the distance alias.
	var queryVector, k values.Value
	var efSearch *int
	var isReturningVectors *bool
	rankType := predicates.ComparisonDistanceRankLessThanOrEq
	if cr, ok := prefixMap[c.distanceAlias]; ok {
		if drc := extractDistanceRankComparison(cr); drc != nil {
			queryVector = drc.QueryVector
			k = drc.Operand
			efSearch = drc.EfSearch
			isReturningVectors = drc.IsReturningVectors
			rankType = drc.Type
		}
	}

	return plans.NewRecordQueryVectorIndexPlan(
		c.indexName,
		prefixComps,
		queryVector,
		k,
		rankType,
		efSearch,
		isReturningVectors,
		c.recordTypes,
		c.flowedType,
	)
}

// extractDistanceRankComparison pulls the DistanceRank comparison out of a
// bound ComparisonRange (it rides as an equality- or inequality-shaped range
// depending on how ComparisonRange.Merge classified it).
func extractDistanceRankComparison(cr *predicates.ComparisonRange) *predicates.Comparison {
	if cr == nil {
		return nil
	}
	switch cr.GetRangeType() {
	case predicates.ComparisonRangeEquality:
		if c := cr.GetEqualityComparison(); c != nil && isDistanceRankType(c.Type) {
			return c
		}
	case predicates.ComparisonRangeInequality:
		for _, c := range cr.GetInequalityComparisons() {
			if c != nil && isDistanceRankType(c.Type) {
				return c
			}
		}
	}
	return nil
}

func isDistanceRankType(t predicates.ComparisonType) bool {
	switch t {
	case predicates.ComparisonDistanceRankEquals,
		predicates.ComparisonDistanceRankLessThan,
		predicates.ComparisonDistanceRankLessThanOrEq:
		return true
	default:
		return false
	}
}

// String returns a human-readable label for debugging.
// Mirrors Java's VectorIndexScanMatchCandidate.toString().
func (c *VectorIndexScanMatchCandidate) String() string {
	return "vector[" + c.indexName + "]"
}

var (
	_ MatchCandidate                   = (*VectorIndexScanMatchCandidate)(nil)
	_ WithPrimaryKeyMatchCandidate     = (*VectorIndexScanMatchCandidate)(nil)
	_ WithBaseQuantifierMatchCandidate = (*VectorIndexScanMatchCandidate)(nil)
	_ ValueIndexLikeMatchCandidate     = (*VectorIndexScanMatchCandidate)(nil)
)
