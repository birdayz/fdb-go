package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// AggregateIndexMatchCandidate represents a pre-computed aggregate index
// in FDB (e.g., SUM, COUNT, MAX_EVER_LONG, MIN_EVER_LONG). Such indexes
// maintain running aggregates grouped by a set of key columns. A query
// like "SELECT region, SUM(amount) FROM t GROUP BY region" can be
// answered directly from a SUM index on (region, amount) without scanning
// any data rows.
//
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.AggregateIndexMatchCandidate`
// but simplified to the structural needs of the seed.
type AggregateIndexMatchCandidate struct {
	indexName   string
	recordTypes []string
	groupCols   []string
	aggFunction expressions.AggregateFunction
	aggColumn   string
	aliases     []values.CorrelationIdentifier
}

// NewAggregateIndexMatchCandidate creates a candidate for an aggregate
// index. groupCols are the grouping key columns; aggFunction + aggColumn
// describe the pre-computed aggregate.
func NewAggregateIndexMatchCandidate(
	indexName string,
	recordTypes []string,
	groupCols []string,
	aggFunction expressions.AggregateFunction,
	aggColumn string,
) *AggregateIndexMatchCandidate {
	aliases := make([]values.CorrelationIdentifier, len(groupCols))
	for i := range aliases {
		aliases[i] = values.UniqueCorrelationIdentifier()
	}
	return &AggregateIndexMatchCandidate{
		indexName:   indexName,
		recordTypes: recordTypes,
		groupCols:   groupCols,
		aggFunction: aggFunction,
		aggColumn:   aggColumn,
		aliases:     aliases,
	}
}

func (c *AggregateIndexMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns nil — the seed AggregateIndexMatchCandidate does
// not yet build an expression tree for traversal-based matching.
func (c *AggregateIndexMatchCandidate) GetTraversal() *Traversal { return nil }
func (c *AggregateIndexMatchCandidate) GetColumnNames() []string { return c.groupCols }
func (c *AggregateIndexMatchCandidate) GetRecordTypes() []string { return c.recordTypes }
func (c *AggregateIndexMatchCandidate) IsUnique() bool           { return false }
func (c *AggregateIndexMatchCandidate) GetAggFunction() expressions.AggregateFunction {
	return c.aggFunction
}
func (c *AggregateIndexMatchCandidate) GetAggColumn() string { return c.aggColumn }

func (c *AggregateIndexMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.aliases
}

func (c *AggregateIndexMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range c.aliases {
		cr, ok := bindings[alias]
		if !ok || cr == nil {
			break
		}
		if !cr.IsEquality() {
			prefix[alias] = cr
			break
		}
		prefix[alias] = cr
	}
	return prefix
}

func (c *AggregateIndexMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	comps := make([]*predicates.ComparisonRange, 0, len(c.aliases))
	for _, alias := range c.aliases {
		cr, ok := prefixMap[alias]
		if !ok {
			break
		}
		comps = append(comps, cr)
	}
	return plans.NewRecordQueryIndexPlan(c.indexName, comps, c.recordTypes, values.UnknownType, reverse)
}

// MatchesGroupBy reports whether this aggregate index can directly satisfy
// the given GroupByExpression. Returns true when:
//   - The grouping keys match the index's groupCols
//   - The GroupBy has exactly one aggregate that matches the index's function + column
func (c *AggregateIndexMatchCandidate) MatchesGroupBy(gb *expressions.GroupByExpression) bool {
	keys := gb.GetGroupingKeys()
	if len(keys) != len(c.groupCols) {
		return false
	}
	for i, k := range keys {
		fv, ok := k.(*values.FieldValue)
		if !ok {
			return false
		}
		if !eqFold(fv.Field, c.groupCols[i]) {
			return false
		}
	}

	aggs := gb.GetAggregates()
	if len(aggs) != 1 {
		return false
	}
	if aggs[0].Function != c.aggFunction {
		return false
	}
	opFV, ok := aggs[0].Operand.(*values.FieldValue)
	if !ok {
		return false
	}
	return eqFold(opFV.Field, c.aggColumn)
}

var _ MatchCandidate = (*AggregateIndexMatchCandidate)(nil)
