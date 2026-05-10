package cascades

import (
	"strings"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ValueIndexScanMatchCandidate represents a secondary index as a
// match candidate. Each index key column has a corresponding sargable
// alias; predicate matching binds comparisons to these aliases to
// determine which prefix of the index key can be used for a scan.
//
// Ports the seed-minimal surface of Java's
// `ValueIndexScanMatchCandidate`. The full Java version also carries
// a Traversal, index-value values, ordering aliases, etc.; those land
// as their consumer rules port.
type ValueIndexScanMatchCandidate struct {
	indexName       string
	recordTypes     []string
	columnNames     []string
	sargableAliases []values.CorrelationIdentifier
	flowedType      values.Type
	unique          bool

	traversalOnce sync.Once
	traversal     *Traversal
}

// NewValueIndexScanMatchCandidate constructs a match candidate for a
// secondary index. columnNames and sargableAliases must be parallel
// slices in index key column order (left-to-right): columnNames[i] is
// the field name for the i-th key column, sargableAliases[i] is the
// correlation identifier used for predicate binding.
func NewValueIndexScanMatchCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	sargableAliases []values.CorrelationIdentifier,
	flowedType values.Type,
	unique bool,
) *ValueIndexScanMatchCandidate {
	aliases := make([]values.CorrelationIdentifier, len(sargableAliases))
	copy(aliases, sargableAliases)
	types := make([]string, len(recordTypes))
	copy(types, recordTypes)
	cols := make([]string, len(columnNames))
	copy(cols, columnNames)
	return &ValueIndexScanMatchCandidate{
		indexName:       indexName,
		recordTypes:     types,
		columnNames:     cols,
		sargableAliases: aliases,
		flowedType:      flowedType,
		unique:          unique,
	}
}

// CandidateName returns the index name.
func (c *ValueIndexScanMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access via ExpandValueIndex. The
// traversal is stable once computed (sync.Once). Ports Java's
// ValueIndexScanMatchCandidate.getTraversal().
func (c *ValueIndexScanMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		c.traversal = ExpandValueIndex(c)
	})
	return c.traversal
}

// GetColumnNames returns the ordered column-name list (one per index
// key column, parallel to GetSargableAliases).
func (c *ValueIndexScanMatchCandidate) GetColumnNames() []string { return c.columnNames }

// GetSargableAliases returns the ordered parameter list (one per
// index key column).
func (c *ValueIndexScanMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.sargableAliases
}

// GetRecordTypes returns which record types this index covers.
func (c *ValueIndexScanMatchCandidate) GetRecordTypes() []string { return c.recordTypes }

// IsUnique reports whether the index enforces uniqueness.
func (c *ValueIndexScanMatchCandidate) IsUnique() bool { return c.unique }

// ComputeBoundParameterPrefixMap walks the sargable aliases in order
// and collects the longest prefix that satisfies index scan
// discipline:
//   - N equality-bound parameters (any number, including 0)
//   - followed by at most ONE inequality-bound parameter
//   - stops at the first unbound (empty) parameter or after the
//     first inequality
//
// Mirrors Java's default `MatchCandidate.computeBoundParameterPrefixMap`.
func (c *ValueIndexScanMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range c.sargableAliases {
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

// ToScanPlan converts the matched prefix into a physical plan. The
// plan is wrapped in a FetchFromPartialRecordPlan with a
// TranslateValueFunction that can translate FieldValues referencing
// covered index columns. This enables push-through rules (C-6) to
// push filters/maps below the fetch when they reference covered
// columns.
//
// Matches Java's ScanWithFetchMatchCandidate architecture where every
// index scan is wrapped in a Fetch that carries the translation
// function.
func (c *ValueIndexScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	comps := make([]*predicates.ComparisonRange, len(c.sargableAliases))
	for i, alias := range c.sargableAliases {
		if cr, ok := prefixMap[alias]; ok {
			comps[i] = cr
		} else {
			comps[i] = predicates.EmptyComparisonRange()
		}
	}
	indexPlan := plans.NewRecordQueryIndexPlan(
		c.indexName,
		comps,
		c.recordTypes,
		c.flowedType,
		reverse,
	)

	// Build the TranslateValueFunction for this index: translates
	// FieldValues whose field name matches a covered index column.
	translateFn := c.buildTranslateValueFunction()

	return plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan,
		translateFn,
		c.flowedType,
		plans.FetchIndexRecordsPrimaryKey,
	)
}

// buildTranslateValueFunction creates a TranslateValueFunction that
// can translate values from the full-record domain to the index-entry
// domain. A FieldValue is translatable if its field name matches one
// of the index's column names (case-insensitive).
//
// Ports the conceptual equivalent of Java's
// ScanWithFetchMatchCandidate.createTranslateValueFunction.
func (c *ValueIndexScanMatchCandidate) buildTranslateValueFunction() plans.TranslateValueFunction {
	coveredColumns := make(map[string]struct{}, len(c.columnNames))
	for _, col := range c.columnNames {
		coveredColumns[strings.ToUpper(col)] = struct{}{}
	}

	return func(value values.Value, sourceAlias, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		switch v := value.(type) {
		case *values.FieldValue:
			if _, covered := coveredColumns[strings.ToUpper(v.Field)]; covered {
				return values.NewFieldValue(
					values.NewQuantifiedObjectValue(targetAlias),
					v.Field, v.Typ,
				), true
			}
			return nil, false
		case *values.QuantifiedObjectValue:
			if v.Correlation == sourceAlias {
				return values.NewQuantifiedObjectValue(targetAlias), true
			}
			return value, true
		case *values.ConstantValue:
			return value, true
		default:
			return nil, false
		}
	}
}

var _ MatchCandidate = (*ValueIndexScanMatchCandidate)(nil)
