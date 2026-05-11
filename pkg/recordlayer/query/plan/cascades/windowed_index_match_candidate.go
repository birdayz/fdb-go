package cascades

import (
	"strings"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// WindowedIndexScanMatchCandidate represents a windowed (rank /
// leaderboard / time-series) index as a match candidate. Such indexes
// partition data by grouping keys, then order by a score within each
// group. Queries that bind the group columns and constrain the rank
// can be answered directly via a BY_RANK index scan.
//
// Key differences from ValueIndexScanMatchCandidate:
//   - Carries separate grouping aliases, a score alias, a rank alias,
//     and primary key aliases. The sargable set is
//     groupingAliases + [rankAlias]; the ordering set is
//     groupingAliases + [scoreAlias] + primaryKeyAliases.
//   - computeMatchedOrderingParts has special-case logic: when the
//     current parameter is the scoreAlias, the comparison range is
//     taken from the rankAlias binding instead, because binding rank
//     by equality also determines the score.
//   - computeOrderingFromScanComparisons treats the score ordinal
//     specially — the score column is fixed with an opaque equality
//     binding rather than the scan comparison.
//   - Implements ScanWithFetchMatchCandidate (covering-index push-through).
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.WindowedIndexScanMatchCandidate`.
type WindowedIndexScanMatchCandidate struct {
	indexName   string
	recordTypes []string
	columnNames []string

	// groupingAliases are the aliases for the grouping key columns.
	groupingAliases []values.CorrelationIdentifier

	// scoreAlias is the alias for the score placeholder in the match
	// candidate. The score is the value the index sorts by within each
	// group.
	scoreAlias values.CorrelationIdentifier

	// rankAlias is the alias for the rank placeholder. Rank is the
	// ordinal position within the group — binding rank by equality
	// implicitly binds the score.
	rankAlias values.CorrelationIdentifier

	// primaryKeyAliases are the aliases for the primary key columns
	// that follow the score in the full ordering.
	primaryKeyAliases []values.CorrelationIdentifier

	// indexKeyValues are the Values representing the index key columns
	// in the expanded graph. Used for covering-index push-through.
	indexKeyValues []values.Value

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

// NewWindowedIndexScanMatchCandidate constructs a match candidate for
// a windowed (rank) index.
func NewWindowedIndexScanMatchCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	groupingAliases []values.CorrelationIdentifier,
	scoreAlias values.CorrelationIdentifier,
	rankAlias values.CorrelationIdentifier,
	primaryKeyAliases []values.CorrelationIdentifier,
	indexKeyValues []values.Value,
	flowedType values.Type,
	unique bool,
	primaryKeyColumns []string,
) *WindowedIndexScanMatchCandidate {
	ga := make([]values.CorrelationIdentifier, len(groupingAliases))
	copy(ga, groupingAliases)
	pka := make([]values.CorrelationIdentifier, len(primaryKeyAliases))
	copy(pka, primaryKeyAliases)
	types := make([]string, len(recordTypes))
	copy(types, recordTypes)
	cols := make([]string, len(columnNames))
	copy(cols, columnNames)
	pkCols := make([]string, len(primaryKeyColumns))
	copy(pkCols, primaryKeyColumns)
	ikv := make([]values.Value, len(indexKeyValues))
	copy(ikv, indexKeyValues)

	return &WindowedIndexScanMatchCandidate{
		indexName:         indexName,
		recordTypes:       types,
		columnNames:       cols,
		groupingAliases:   ga,
		scoreAlias:        scoreAlias,
		rankAlias:         rankAlias,
		primaryKeyAliases: pka,
		indexKeyValues:    ikv,
		flowedType:        flowedType,
		unique:            unique,
		primaryKeyColumns: pkCols,
	}
}

// CandidateName returns the index name.
func (c *WindowedIndexScanMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access.
func (c *WindowedIndexScanMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		c.traversal = ExpandValueIndex(c)
	})
	return c.traversal
}

// GetColumnNames returns the ordered column-name list.
func (c *WindowedIndexScanMatchCandidate) GetColumnNames() []string { return c.columnNames }

// GetSargableAliases returns groupingAliases + [rankAlias]. Ports
// Java's WindowedIndexScanMatchCandidate.getSargableAliases().
func (c *WindowedIndexScanMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	result := make([]values.CorrelationIdentifier, 0, len(c.groupingAliases)+1)
	result = append(result, c.groupingAliases...)
	result = append(result, c.rankAlias)
	return result
}

// GetOrderingAliases returns groupingAliases + [scoreAlias] +
// primaryKeyAliases. Ports Java's orderingAliases() static helper.
func (c *WindowedIndexScanMatchCandidate) GetOrderingAliases() []values.CorrelationIdentifier {
	result := make([]values.CorrelationIdentifier, 0, len(c.groupingAliases)+1+len(c.primaryKeyAliases))
	result = append(result, c.groupingAliases...)
	result = append(result, c.scoreAlias)
	result = append(result, c.primaryKeyAliases...)
	return result
}

// GetRecordTypes returns the record types this index covers.
func (c *WindowedIndexScanMatchCandidate) GetRecordTypes() []string { return c.recordTypes }

// IsUnique reports whether the index enforces uniqueness.
func (c *WindowedIndexScanMatchCandidate) IsUnique() bool { return c.unique }

// GetBaseType returns the base record type.
func (c *WindowedIndexScanMatchCandidate) GetBaseType() values.Type { return c.flowedType }

// GetColumnSize returns the number of key columns.
func (c *WindowedIndexScanMatchCandidate) GetColumnSize() int { return len(c.columnNames) }

// CreatesDuplicates reports whether the index can produce duplicate
// entries per record.
func (c *WindowedIndexScanMatchCandidate) CreatesDuplicates() bool { return c.createsDuplicates }

// HasAndOrderedByRecordTypeKey reports whether the index key starts
// with the record type key. Always false for windowed indexes.
func (c *WindowedIndexScanMatchCandidate) HasAndOrderedByRecordTypeKey() bool { return false }

// GetSargableAliasesRequiredForBinding returns nil — windowed indexes
// do not require any specific aliases to be bound.
func (c *WindowedIndexScanMatchCandidate) GetSargableAliasesRequiredForBinding() []values.CorrelationIdentifier {
	return nil
}

// GetIndexKeyValues returns the index key Values.
func (c *WindowedIndexScanMatchCandidate) GetIndexKeyValues() []values.Value {
	return c.indexKeyValues
}

// GetScoreAlias returns the score alias.
func (c *WindowedIndexScanMatchCandidate) GetScoreAlias() values.CorrelationIdentifier {
	return c.scoreAlias
}

// GetRankAlias returns the rank alias.
func (c *WindowedIndexScanMatchCandidate) GetRankAlias() values.CorrelationIdentifier {
	return c.rankAlias
}

// GetGroupingAliases returns the grouping aliases.
func (c *WindowedIndexScanMatchCandidate) GetGroupingAliases() []values.CorrelationIdentifier {
	return c.groupingAliases
}

// GetPrimaryKeyAliases returns the primary key aliases.
func (c *WindowedIndexScanMatchCandidate) GetPrimaryKeyAliases() []values.CorrelationIdentifier {
	return c.primaryKeyAliases
}

// GetPrimaryKeyValues returns the primary key as a list of Values.
// Lazily computed. Returns nil if no PK columns.
func (c *WindowedIndexScanMatchCandidate) GetPrimaryKeyValues() []values.Value {
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

// PushValueThroughFetch attempts to translate a value from the
// full-record domain to the index-entry domain. Returns the
// translated value and true on success; nil and false otherwise.
// Implements ScanWithFetchMatchCandidate.
func (c *WindowedIndexScanMatchCandidate) PushValueThroughFetch(
	value values.Value,
	sourceAlias values.CorrelationIdentifier,
	targetAlias values.CorrelationIdentifier,
) (values.Value, bool) {
	fn := c.buildTranslateValueFunction()
	return fn(value, sourceAlias, targetAlias)
}

// buildTranslateValueFunction creates a TranslateValueFunction for
// the windowed index's covered columns.
func (c *WindowedIndexScanMatchCandidate) buildTranslateValueFunction() plans.TranslateValueFunction {
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

// ComputeBoundParameterPrefixMap walks the sargable aliases and
// collects the longest prefix satisfying index scan discipline.
func (c *WindowedIndexScanMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	sargable := c.GetSargableAliases()
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range sargable {
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
// Produces a RecordQueryIndexPlan (BY_RANK scan) wrapped in a
// FetchFromPartialRecordPlan.
//
// Ports Java's WindowedIndexScanMatchCandidate.toEquivalentPlan().
func (c *WindowedIndexScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	sargable := c.GetSargableAliases()
	comps := make([]*predicates.ComparisonRange, len(sargable))
	for i, alias := range sargable {
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

	// Wrap in a FetchFromPartialRecordPlan with the index's translate
	// function for covering-index push-through.
	translateFn := c.buildTranslateValueFunction()
	return plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan,
		translateFn,
		c.flowedType,
		plans.FetchIndexRecordsPrimaryKey,
	)
}

// String returns a human-readable label for debugging.
// Mirrors Java's WindowedIndexScanMatchCandidate.toString().
func (c *WindowedIndexScanMatchCandidate) String() string {
	return "Windowed[" + c.indexName + "]"
}

// OrderingAliases is a package-level helper that computes the ordering
// alias list for a windowed index: grouping + [score] + primaryKey.
// Ports Java's WindowedIndexScanMatchCandidate.orderingAliases().
func OrderingAliases(
	groupingAliases []values.CorrelationIdentifier,
	scoreAlias values.CorrelationIdentifier,
	primaryKeyAliases []values.CorrelationIdentifier,
) []values.CorrelationIdentifier {
	result := make([]values.CorrelationIdentifier, 0, len(groupingAliases)+1+len(primaryKeyAliases))
	result = append(result, groupingAliases...)
	result = append(result, scoreAlias)
	result = append(result, primaryKeyAliases...)
	return result
}

var (
	_ MatchCandidate                   = (*WindowedIndexScanMatchCandidate)(nil)
	_ WithPrimaryKeyMatchCandidate     = (*WindowedIndexScanMatchCandidate)(nil)
	_ WithBaseQuantifierMatchCandidate = (*WindowedIndexScanMatchCandidate)(nil)
	_ ScanWithFetchMatchCandidate      = (*WindowedIndexScanMatchCandidate)(nil)
	_ ValueIndexLikeMatchCandidate     = (*WindowedIndexScanMatchCandidate)(nil)
)
