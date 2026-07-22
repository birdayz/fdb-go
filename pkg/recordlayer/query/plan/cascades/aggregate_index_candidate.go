package cascades

import (
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// AggregateIndexMatchCandidate represents a pre-computed aggregate index
// in FDB (e.g., SUM, COUNT, MAX_EVER_LONG, MIN_EVER_LONG). Such indexes
// maintain running aggregates grouped by a set of key columns. A query
// like "SELECT region, SUM(amount) FROM t GROUP BY region" can be
// answered directly from a SUM index on (region, amount) without scanning
// any data rows.
//
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.AggregateIndexMatchCandidate`,
// carrying the surface AggregateDataAccessRule consumes.
type AggregateIndexMatchCandidate struct {
	indexName   string
	recordTypes []string
	groupCols   []string
	aggFunction expressions.AggregateFunction
	aggColumn   string
	aliases     []values.CorrelationIdentifier

	traversalOnce sync.Once
	traversal     *Traversal
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

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access via ExpandValueIndex (using the
// grouping columns as the index columns). The traversal is stable once
// computed (sync.Once).
func (c *AggregateIndexMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		c.traversal = ExpandValueIndex(c)
	})
	return c.traversal
}
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
	return stampIndexMetadata(c, plans.NewRecordQueryIndexPlan(c.indexName, comps, c.recordTypes, values.UnknownType, reverse))
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
		if !aggColumnMatches(k, c.groupCols[i]) {
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
	if c.aggFunction == expressions.AggCount {
		// Single source of truth for count-star (RFC-164 WS-3) — must match the
		// executor's group cursors and the translator's normalization.
		if expressions.IsCountStar(aggs[0]) {
			return c.aggColumn == ""
		}
		return c.aggColumn != "" && aggColumnMatches(aggs[0].Operand, c.aggColumn)
	}
	return aggColumnMatches(aggs[0].Operand, c.aggColumn)
}

// MatchesSingleAggregateOf reports whether this candidate's grouping
// keys match gb's grouping keys AND this candidate covers the aggregate
// at index aggIndex in gb's aggregate list. Used by the multi-aggregate
// intersection path: each candidate covers one aggregate while all
// share the same grouping columns.
func (c *AggregateIndexMatchCandidate) MatchesSingleAggregateOf(gb *expressions.GroupByExpression, aggIndex int) bool {
	keys := gb.GetGroupingKeys()
	if len(keys) != len(c.groupCols) {
		return false
	}
	for i, k := range keys {
		if !aggColumnMatches(k, c.groupCols[i]) {
			return false
		}
	}

	aggs := gb.GetAggregates()
	if aggIndex < 0 || aggIndex >= len(aggs) {
		return false
	}
	agg := aggs[aggIndex]
	if agg.Function != c.aggFunction {
		return false
	}
	if c.aggFunction == expressions.AggCount {
		// Single source of truth for count-star (RFC-164 WS-3) — must match the
		// executor's group cursors and the translator's normalization.
		if expressions.IsCountStar(agg) {
			return c.aggColumn == ""
		}
		return c.aggColumn != "" && aggColumnMatches(agg.Operand, c.aggColumn)
	}
	return aggColumnMatches(agg.Operand, c.aggColumn)
}

// aggColumnMatches reports whether a query grouping-key / aggregate-operand
// value denotes the aggregate index's declared column `col` — by full accessor
// PATH, not leaf name, so a nested `addr.city` grouping key never matches a
// same-leaf-named top-level `city` aggregate index (RFC-187 S4/S5/S8). The
// candidate carries single top-level column names today, so a multi-accessor
// (nested) query path does not match and the query falls back to a base-record
// StreamingAgg (correct rows, slower) — the transitional reject-nested until the
// candidate exposes real nested column paths end-to-end (construction, index
// expansion, and execution), tracked as the RFC-187 §3.2 follow-up.
func aggColumnMatches(v values.Value, col string) bool {
	return values.AccessorNamePathMatchesNames(v, []string{col})
}

var _ MatchCandidate = (*AggregateIndexMatchCandidate)(nil)
