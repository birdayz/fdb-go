package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// AggregateFunction identifies an aggregate computation.
type AggregateFunction int

const (
	AggCount AggregateFunction = iota
	AggSum
	AggMin
	AggMax
	AggAvg
)

// AggregateSpec describes one aggregate column in a GroupBy.
type AggregateSpec struct {
	Function AggregateFunction
	Operand  values.Value
}

// GroupByExpression groups input rows by groupingKeys and computes
// aggregates over each group. Ports Java's GroupByExpression at the
// structural level needed for the Cascades planner.
//
// Java's version uses rich Value types (RecordConstructorValue for
// grouping, AggregateValue for aggregates). The seed simplifies:
// groupingKeys is a list of Values (typically FieldValues), aggregates
// is a list of function+operand pairs.
type GroupByExpression struct {
	groupingKeys []values.Value
	aggregates   []AggregateSpec
	inner        Quantifier
}

func NewGroupByExpression(
	groupingKeys []values.Value,
	aggregates []AggregateSpec,
	inner Quantifier,
) *GroupByExpression {
	return &GroupByExpression{
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
		inner:        inner,
	}
}

func (e *GroupByExpression) GetGroupingKeys() []values.Value { return e.groupingKeys }
func (e *GroupByExpression) GetAggregates() []AggregateSpec  { return e.aggregates }
func (e *GroupByExpression) GetInner() Quantifier            { return e.inner }
func (e *GroupByExpression) GetQuantifiers() []Quantifier    { return []Quantifier{e.inner} }
func (e *GroupByExpression) CanCorrelate() bool              { return false }
func (e *GroupByExpression) ChildrenAsSet() bool             { return false }

func (e *GroupByExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

func (e *GroupByExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (e *GroupByExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*GroupByExpression)
	if !ok {
		return false
	}
	if len(e.groupingKeys) != len(o.groupingKeys) {
		return false
	}
	if len(e.aggregates) != len(o.aggregates) {
		return false
	}
	for i, k := range e.groupingKeys {
		if values.ExplainValue(k) != values.ExplainValue(o.groupingKeys[i]) {
			return false
		}
	}
	for i, a := range e.aggregates {
		if a.Function != o.aggregates[i].Function {
			return false
		}
		if values.ExplainValue(a.Operand) != values.ExplainValue(o.aggregates[i].Operand) {
			return false
		}
	}
	return true
}

func (e *GroupByExpression) HashCodeWithoutChildren() uint64 {
	h := uint64(0x6770_6279) // "gpby"
	for _, k := range e.groupingKeys {
		h ^= uint64(len(values.ExplainValue(k))) * 31
	}
	for _, a := range e.aggregates {
		h ^= uint64(a.Function) * 37
		h ^= uint64(len(values.ExplainValue(a.Operand))) * 41
	}
	return h
}

var _ RelationalExpression = (*GroupByExpression)(nil)
