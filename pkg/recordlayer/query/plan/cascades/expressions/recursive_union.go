package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TraversalStrategy defines how the recursive leg traverses results.
type TraversalStrategy int

const (
	TraversalAny TraversalStrategy = iota
	TraversalPreorder
	TraversalLevel
	TraversalPostorder
)

func (s TraversalStrategy) String() string {
	switch s {
	case TraversalAny:
		return "ANY"
	case TraversalPreorder:
		return "PREORDER"
	case TraversalLevel:
		return "LEVEL"
	case TraversalPostorder:
		return "POSTORDER"
	}
	return "UNKNOWN"
}

// RecursiveUnionExpression is the logical representation of a recursive
// union (SQL recursive CTE). Has two quantifiers: initial state (seed)
// and recursive state (iterative). The recursive leg executes
// repeatedly until it produces no more results (fix-point).
type RecursiveUnionExpression struct {
	initialState         Quantifier
	recursiveState       Quantifier
	tempTableScanAlias   values.CorrelationIdentifier
	tempTableInsertAlias values.CorrelationIdentifier
	traversalStrategy    TraversalStrategy
}

func NewRecursiveUnionExpression(
	initialState, recursiveState Quantifier,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
	strategy TraversalStrategy,
) *RecursiveUnionExpression {
	return &RecursiveUnionExpression{
		initialState:         initialState,
		recursiveState:       recursiveState,
		tempTableScanAlias:   tempTableScanAlias,
		tempTableInsertAlias: tempTableInsertAlias,
		traversalStrategy:    strategy,
	}
}

func (e *RecursiveUnionExpression) GetInitialState() Quantifier {
	return e.initialState
}

func (e *RecursiveUnionExpression) GetRecursiveState() Quantifier {
	return e.recursiveState
}

func (e *RecursiveUnionExpression) GetTempTableScanAlias() values.CorrelationIdentifier {
	return e.tempTableScanAlias
}

func (e *RecursiveUnionExpression) GetTempTableInsertAlias() values.CorrelationIdentifier {
	return e.tempTableInsertAlias
}

func (e *RecursiveUnionExpression) GetTraversalStrategy() TraversalStrategy {
	return e.traversalStrategy
}

func (e *RecursiveUnionExpression) PreOrderAllowed() bool {
	return e.traversalStrategy == TraversalAny || e.traversalStrategy == TraversalPreorder
}

func (e *RecursiveUnionExpression) PostOrderAllowed() bool {
	return e.traversalStrategy == TraversalAny || e.traversalStrategy == TraversalPostorder
}

func (e *RecursiveUnionExpression) DfsAllowed() bool {
	return e.PreOrderAllowed() || e.PostOrderAllowed()
}

func (e *RecursiveUnionExpression) LevelAllowed() bool {
	return e.traversalStrategy == TraversalAny || e.traversalStrategy == TraversalLevel
}

func (e *RecursiveUnionExpression) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (e *RecursiveUnionExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.initialState, e.recursiveState}
}

func (e *RecursiveUnionExpression) CanCorrelate() bool { return true }

func (e *RecursiveUnionExpression) ChildrenAsSet() bool { return false }

func (e *RecursiveUnionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (e *RecursiveUnionExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*RecursiveUnionExpression)
	if !ok {
		return false
	}
	if e.traversalStrategy != o.traversalStrategy {
		return false
	}
	scanMatch := e.tempTableScanAlias == o.tempTableScanAlias
	if !scanMatch && aliases != nil {
		if t, ok := aliases.GetTarget(e.tempTableScanAlias); ok {
			scanMatch = t == o.tempTableScanAlias
		}
	}
	insertMatch := e.tempTableInsertAlias == o.tempTableInsertAlias
	if !insertMatch && aliases != nil {
		if t, ok := aliases.GetTarget(e.tempTableInsertAlias); ok {
			insertMatch = t == o.tempTableInsertAlias
		}
	}
	return scanMatch && insertMatch
}

func (e *RecursiveUnionExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("recursiveunion|"))
	h.Write([]byte(e.tempTableScanAlias.Name()))
	h.Write([]byte{0})
	h.Write([]byte(e.tempTableInsertAlias.Name()))
	h.Write([]byte{0})
	h.Write([]byte{byte(e.traversalStrategy)})
	return h.Sum64()
}

func (e *RecursiveUnionExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &RecursiveUnionExpression{
		initialState:         quantifiers[0],
		recursiveState:       quantifiers[1],
		tempTableScanAlias:   e.tempTableScanAlias,
		tempTableInsertAlias: e.tempTableInsertAlias,
		traversalStrategy:    e.traversalStrategy,
	}
}

var _ RelationalExpression = (*RecursiveUnionExpression)(nil)
