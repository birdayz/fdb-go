package expressions

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TempTableInsertExpression is a logical expression for inserting
// query results into a TempTable. Has one ForEach quantifier (the
// data source) and a reference to the target temp table.
type TempTableInsertExpression struct {
	inner          Quantifier
	tempTableAlias values.CorrelationIdentifier
	owning         bool
	resultValue    values.Value
}

func NewTempTableInsertExpression(
	inner Quantifier,
	tempTableAlias values.CorrelationIdentifier,
	owning bool,
) (*TempTableInsertExpression, error) {
	if tempTableAlias.IsZero() || tempTableAlias == values.CurrentCorrelation() {
		return nil, fmt.Errorf("TempTableInsertExpression alias: expected an ordinary non-zero correlation")
	}
	flowed, err := requireFlowedResult("TempTableInsertExpression", inner)
	if err != nil {
		return nil, err
	}
	resultType, err := snapshotExpressionResultType("TempTableInsertExpression", flowed.FlowedType())
	if err != nil {
		return nil, err
	}
	return &TempTableInsertExpression{
		inner:          inner,
		tempTableAlias: tempTableAlias,
		owning:         owning,
		resultValue:    values.NewQueriedValue(nil, resultType.Type()),
	}, nil
}

func (e *TempTableInsertExpression) GetInner() Quantifier {
	return e.inner
}

func (e *TempTableInsertExpression) GetTempTableAlias() values.CorrelationIdentifier {
	return e.tempTableAlias
}

func (e *TempTableInsertExpression) IsOwning() bool {
	return e.owning
}

func (e *TempTableInsertExpression) GetResultValue() values.Value {
	return e.resultValue
}

func (e *TempTableInsertExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

func (e *TempTableInsertExpression) CanCorrelate() bool { return false }

func (e *TempTableInsertExpression) ChildrenAsSet() bool { return false }

func (e *TempTableInsertExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{e.tempTableAlias: {}}
}

func (e *TempTableInsertExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*TempTableInsertExpression)
	if !ok {
		return false
	}
	return e.tempTableAlias == o.tempTableAlias && e.owning == o.owning
}

func (e *TempTableInsertExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptableinsert|"))
	h.Write([]byte(e.tempTableAlias.Name()))
	if e.owning {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *TempTableInsertExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("TempTableInsertExpression", len(quantifiers), 1); err != nil {
		return nil, err
	}
	return NewTempTableInsertExpression(quantifiers[0], e.tempTableAlias, e.owning)
}

var _ RelationalExpression = (*TempTableInsertExpression)(nil)
