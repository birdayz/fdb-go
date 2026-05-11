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

// NewVectorIndexScanMatchCandidate constructs a match candidate for a
// vector similarity search index.
func NewVectorIndexScanMatchCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	parameters []values.CorrelationIdentifier,
	orderingAliases []values.CorrelationIdentifier,
	parametersRequiredForBinding []values.CorrelationIdentifier,
	flowedType values.Type,
	unique bool,
	primaryKeyColumns []string,
) *VectorIndexScanMatchCandidate {
	params := make([]values.CorrelationIdentifier, len(parameters))
	copy(params, parameters)
	ordering := make([]values.CorrelationIdentifier, len(orderingAliases))
	copy(ordering, orderingAliases)
	types := make([]string, len(recordTypes))
	copy(types, recordTypes)
	cols := make([]string, len(columnNames))
	copy(cols, columnNames)
	pkCols := make([]string, len(primaryKeyColumns))
	copy(pkCols, primaryKeyColumns)

	reqSet := make(map[values.CorrelationIdentifier]struct{}, len(parametersRequiredForBinding))
	for _, a := range parametersRequiredForBinding {
		reqSet[a] = struct{}{}
	}

	return &VectorIndexScanMatchCandidate{
		indexName:                    indexName,
		recordTypes:                  types,
		columnNames:                  cols,
		parameters:                   params,
		orderingAliases:              ordering,
		parametersRequiredForBinding: reqSet,
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
		c.traversal = ExpandValueIndex(c)
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

// ToScanPlan converts the matched prefix into a physical plan.
// Produces a RecordQueryIndexPlan wrapped in a
// FetchFromPartialRecordPlan.
//
// Ports Java's VectorIndexScanMatchCandidate.toEquivalentPlan().
func (c *VectorIndexScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	comps := make([]*predicates.ComparisonRange, len(c.parameters))
	for i, alias := range c.parameters {
		if cr, ok := prefixMap[alias]; ok {
			comps[i] = cr
		} else {
			comps[i] = predicates.EmptyComparisonRange()
		}
	}
	return plans.NewRecordQueryIndexPlan(
		c.indexName,
		comps,
		c.recordTypes,
		c.flowedType,
		reverse,
	)
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
