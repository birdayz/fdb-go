package expressions

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TempTableScanExpression is a leaf source that reads from a temp table
// identified by a correlation. No quantifiers (leaf node).
type TempTableScanExpression struct {
	tempTableAlias values.CorrelationIdentifier
	resultType     values.ExactTypeHandle
}

func NewTempTableScanExpression(alias values.CorrelationIdentifier, flowedType values.Type) (*TempTableScanExpression, error) {
	if alias.IsZero() || alias == values.CurrentCorrelation() {
		return nil, fmt.Errorf("TempTableScanExpression alias: expected an ordinary non-zero correlation")
	}
	resultType, err := snapshotExpressionResultType("TempTableScanExpression", flowedType)
	if err != nil {
		return nil, err
	}
	return &TempTableScanExpression{tempTableAlias: alias, resultType: resultType}, nil
}

func (e *TempTableScanExpression) GetTempTableAlias() values.CorrelationIdentifier {
	return e.tempTableAlias
}

func (e *TempTableScanExpression) GetResultValue() values.Value {
	return values.NewQueriedValue(nil, e.resultType.Type())
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
	return e.tempTableAlias == o.tempTableAlias &&
		typeEquals(e.resultType.Type(), o.resultType.Type())
}

func (e *TempTableScanExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptablescan|"))
	h.Write([]byte(e.tempTableAlias.Name()))
	h.Write([]byte{0})
	h.Write(e.resultType.CanonicalBytes())
	return h.Sum64()
}

func (e *TempTableScanExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("TempTableScanExpression", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return e, nil
}

var _ RelationalExpression = (*TempTableScanExpression)(nil)
