package expressions

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	distinct             bool // UNION DISTINCT (bare UNION) for cycle detection
	resultValue          values.Value
}

func NewRecursiveUnionExpression(
	initialState, recursiveState Quantifier,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
	strategy TraversalStrategy,
) (*RecursiveUnionExpression, error) {
	return newRecursiveUnionExpression(initialState, recursiveState, tempTableScanAlias, tempTableInsertAlias, strategy, false)
}

// NewRecursiveUnionExpressionDistinct creates a RecursiveUnionExpression
// with UNION DISTINCT semantics (deduplication for cycle detection).
func NewRecursiveUnionExpressionDistinct(
	initialState, recursiveState Quantifier,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
	strategy TraversalStrategy,
) (*RecursiveUnionExpression, error) {
	return newRecursiveUnionExpression(initialState, recursiveState, tempTableScanAlias, tempTableInsertAlias, strategy, true)
}

func newRecursiveUnionExpression(
	initialState, recursiveState Quantifier,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
	strategy TraversalStrategy,
	distinct bool,
) (*RecursiveUnionExpression, error) {
	initialResult, err := requireFlowedResult("RecursiveUnionExpression initial state", initialState)
	if err != nil {
		return nil, err
	}
	recursiveResult, err := requireFlowedResult("RecursiveUnionExpression recursive state", recursiveState)
	if err != nil {
		return nil, err
	}
	if !values.FlowedTypesEqual(initialResult, recursiveResult) {
		return nil, fmt.Errorf("RecursiveUnionExpression result: initial type %v disagrees with recursive type %v", initialResult.FlowedType(), recursiveResult.FlowedType())
	}
	resultType, err := snapshotExpressionResultType("RecursiveUnionExpression", initialResult.FlowedType())
	if err != nil {
		return nil, err
	}
	return &RecursiveUnionExpression{
		initialState:         initialState,
		recursiveState:       recursiveState,
		tempTableScanAlias:   tempTableScanAlias,
		tempTableInsertAlias: tempTableInsertAlias,
		traversalStrategy:    strategy,
		distinct:             distinct,
		resultValue: values.NewDerivedValueWithType(
			[]values.Value{initialResult, recursiveResult},
			resultType.Type(),
		),
	}, nil
}

func (e *RecursiveUnionExpression) IsDistinct() bool { return e.distinct }

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
	return e.resultValue
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
	if e.traversalStrategy != o.traversalStrategy || e.distinct != o.distinct {
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
	if e.distinct {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *RecursiveUnionExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("RecursiveUnionExpression", len(quantifiers), 2); err != nil {
		return nil, err
	}
	return newRecursiveUnionExpression(
		quantifiers[0],
		quantifiers[1],
		e.tempTableScanAlias,
		e.tempTableInsertAlias,
		e.traversalStrategy,
		e.distinct,
	)
}

var _ RelationalExpression = (*RecursiveUnionExpression)(nil)
