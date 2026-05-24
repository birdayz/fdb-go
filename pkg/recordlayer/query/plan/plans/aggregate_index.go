package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryAggregateIndexPlan wraps an index scan that reads from
// an aggregate index (e.g. SUM, COUNT) and reconstructs records from
// the index entries. This is a leaf plan (no children — the wrapped
// RecordQueryIndexPlan is a structural field, not a child in the plan
// tree sense). Mirrors Java's RecordQueryAggregateIndexPlan.
//
// Fields:
//   - indexPlan: the underlying index scan plan.
//   - recordTypeName: the base record type name (for metadata lookup).
//   - resultType: the rich Type of the aggregated result row.
//   - aggregateFunction: the name of the aggregate function
//     (e.g. "SUM", "COUNT", "MIN", "MAX").
type RecordQueryAggregateIndexPlan struct {
	indexPlan         *RecordQueryIndexPlan
	recordTypeName    string
	resultType        values.Type
	aggregateFunction string
	groupCols         []string
	aggColumn         string
}

// NewRecordQueryAggregateIndexPlan constructs an aggregate index plan.
func NewRecordQueryAggregateIndexPlan(
	indexPlan *RecordQueryIndexPlan,
	recordTypeName string,
	resultType values.Type,
	aggregateFunction string,
) *RecordQueryAggregateIndexPlan {
	if resultType == nil {
		resultType = values.UnknownType
	}
	return &RecordQueryAggregateIndexPlan{
		indexPlan:         indexPlan,
		recordTypeName:    recordTypeName,
		resultType:        resultType,
		aggregateFunction: aggregateFunction,
	}
}

// WithGroupColumns sets the grouping and aggregate column names for
// the executor to map index entries to result rows.
func (p *RecordQueryAggregateIndexPlan) WithGroupColumns(groupCols []string, aggColumn string) *RecordQueryAggregateIndexPlan {
	p.groupCols = groupCols
	p.aggColumn = aggColumn
	return p
}

// GetGroupCols returns the grouping column names.
func (p *RecordQueryAggregateIndexPlan) GetGroupCols() []string { return p.groupCols }

// GetAggColumn returns the aggregate column name.
func (p *RecordQueryAggregateIndexPlan) GetAggColumn() string { return p.aggColumn }

// GetIndexPlan returns the underlying index plan.
func (p *RecordQueryAggregateIndexPlan) GetIndexPlan() *RecordQueryIndexPlan { return p.indexPlan }

// GetRecordTypeName returns the base record type name.
func (p *RecordQueryAggregateIndexPlan) GetRecordTypeName() string { return p.recordTypeName }

// GetAggregateFunction returns the aggregate function name.
func (p *RecordQueryAggregateIndexPlan) GetAggregateFunction() string { return p.aggregateFunction }

// GetIndexName returns the index name from the underlying plan.
func (p *RecordQueryAggregateIndexPlan) GetIndexName() string {
	return p.indexPlan.GetIndexName()
}

// IsReverse delegates to the underlying index plan.
func (p *RecordQueryAggregateIndexPlan) IsReverse() bool {
	return p.indexPlan.IsReverse()
}

// GetResultType returns the aggregate result type.
func (p *RecordQueryAggregateIndexPlan) GetResultType() values.Type { return p.resultType }

// GetChildren returns nil — this is a leaf plan. The wrapped index
// plan is a structural field, not a child (mirrors Java where
// RecordQueryAggregateIndexPlan implements
// RecordQueryPlanWithNoChildren).
func (p *RecordQueryAggregateIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// EqualsWithoutChildren compares index plan, record type name, and
// result type.
func (p *RecordQueryAggregateIndexPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryAggregateIndexPlan)
	if !ok {
		return false
	}
	if p.recordTypeName != o.recordTypeName {
		return false
	}
	if p.aggregateFunction != o.aggregateFunction {
		return false
	}
	// Compare the embedded index plan structurally.
	if !p.indexPlan.EqualsWithoutChildren(o.indexPlan) {
		return false
	}
	return true
}

// HashCodeWithoutChildren mixes index plan hash, record type, and
// aggregate function.
func (p *RecordQueryAggregateIndexPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("aggregateindexplan|"))
	h.Write([]byte(p.indexPlan.GetIndexName()))
	h.Write([]byte{0})
	h.Write([]byte(p.recordTypeName))
	h.Write([]byte{0})
	h.Write([]byte(p.aggregateFunction))
	return h.Sum64()
}

// Explain renders AggregateIndex(function, indexName, recordType).
func (p *RecordQueryAggregateIndexPlan) Explain() string {
	return fmt.Sprintf("AggregateIndex(%s, %s, %s)",
		p.aggregateFunction, p.indexPlan.GetIndexName(), p.recordTypeName)
}

var _ RecordQueryPlan = (*RecordQueryAggregateIndexPlan)(nil)
