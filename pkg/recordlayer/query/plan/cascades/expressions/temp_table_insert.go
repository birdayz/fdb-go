package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TempTableInsertExpression is a logical expression for inserting
// query results into a TempTable. Has one ForEach quantifier (the
// data source) and a reference to the target temp table.
type TempTableInsertExpression struct {
	inner          Quantifier
	tempTableAlias values.CorrelationIdentifier
	owning         bool
}

func NewTempTableInsertExpression(
	inner Quantifier,
	tempTableAlias values.CorrelationIdentifier,
	owning bool,
) *TempTableInsertExpression {
	return &TempTableInsertExpression{
		inner:          inner,
		tempTableAlias: tempTableAlias,
		owning:         owning,
	}
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
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
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

func (e *TempTableInsertExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &TempTableInsertExpression{
		inner:          quantifiers[0],
		tempTableAlias: e.tempTableAlias,
		owning:         e.owning,
	}
}

var _ RelationalExpression = (*TempTableInsertExpression)(nil)
