package expressions

import (
	"encoding/binary"
	"hash/fnv"

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

func (f AggregateFunction) String() string {
	switch f {
	case AggCount:
		return "COUNT"
	case AggSum:
		return "SUM"
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	case AggAvg:
		return "AVG"
	default:
		return "UNKNOWN"
	}
}

// AggregateSpec describes one aggregate column in a GroupBy.
type AggregateSpec struct {
	Function    AggregateFunction
	Operand     values.Value
	Alias       string
	OperandName string // canonical operand text for result-map keying (e.g. "PRICE*QTY")
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

func (e *GroupByExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
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
	// Alias-aware grouping-key + aggregate-operand equality (RFC-040 040.2).
	// OperandName (alias-bearing canonical text) is intentionally not compared
	// — equality already ignored it, and it must stay out for alias-invariance.
	vm := aliases.ToValuesAliasMap()
	for i, k := range e.groupingKeys {
		if !values.SemanticEqualsUnderAliasMap(k, o.groupingKeys[i], vm) {
			return false
		}
	}
	for i, a := range e.aggregates {
		if a.Function != o.aggregates[i].Function {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(a.Operand, o.aggregates[i].Operand, vm) {
			return false
		}
	}
	return true
}

func (e *GroupByExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("grpby|"))
	var b [8]byte
	for _, k := range e.groupingKeys {
		binary.LittleEndian.PutUint64(b[:], values.SemanticHashCode(k))
		h.Write(b[:])
		h.Write([]byte("|"))
	}
	for _, a := range e.aggregates {
		binary.LittleEndian.PutUint64(b[:], uint64(a.Function))
		h.Write(b[:])
		binary.LittleEndian.PutUint64(b[:], values.SemanticHashCode(a.Operand))
		h.Write(b[:])
		h.Write([]byte("|"))
	}
	return h.Sum64()
}

func (e *GroupByExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	if len(quantifiers) == 0 {
		return e
	}
	return &GroupByExpression{
		inner:        quantifiers[0],
		groupingKeys: e.groupingKeys,
		aggregates:   e.aggregates,
	}
}

var _ RelationalExpression = (*GroupByExpression)(nil)
