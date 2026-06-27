package expressions

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TempTableScanExpression is a leaf source that reads from a temp table
// identified by a correlation. No quantifiers (leaf node).
type TempTableScanExpression struct {
	tempTableAlias values.CorrelationIdentifier
}

func NewTempTableScanExpression(alias values.CorrelationIdentifier) *TempTableScanExpression {
	return &TempTableScanExpression{tempTableAlias: alias}
}

func (e *TempTableScanExpression) GetTempTableAlias() values.CorrelationIdentifier {
	return e.tempTableAlias
}

func (e *TempTableScanExpression) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (e *TempTableScanExpression) GetQuantifiers() []Quantifier { return nil }

func (e *TempTableScanExpression) CanCorrelate() bool { return false }

func (e *TempTableScanExpression) ChildrenAsSet() bool { return false }

func (e *TempTableScanExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{e.tempTableAlias: {}}
}

func (e *TempTableScanExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*TempTableScanExpression)
	if !ok {
		return false
	}
	return e.tempTableAlias == o.tempTableAlias
}

func (e *TempTableScanExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptablescan|"))
	h.Write([]byte(e.tempTableAlias.Name()))
	return h.Sum64()
}

func (e *TempTableScanExpression) WithQuantifiers(_ []Quantifier) RelationalExpression {
	return e
}

var _ RelationalExpression = (*TempTableScanExpression)(nil)
