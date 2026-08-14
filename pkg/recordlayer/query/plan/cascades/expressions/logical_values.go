package expressions

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalValuesExpression is a leaf source that produces a single row
// of constant values — the Cascades equivalent of SQL's VALUES (a, b, c).
// Zero quantifiers (it's a source, not a transformer).
type LogicalValuesExpression struct {
	columns     []values.Value
	resultValue *values.RecordConstructorValue
}

func NewLogicalValuesExpression(columns []values.Value) (*LogicalValuesExpression, error) {
	resultValue, err := values.ProjectionResultValue(columns, nil)
	if err != nil {
		return nil, fmt.Errorf("LogicalValuesExpression result: %w", err)
	}
	if _, err := snapshotExpressionResultType("LogicalValuesExpression", resultValue.Type()); err != nil {
		return nil, err
	}
	return &LogicalValuesExpression{columns: slices.Clone(columns), resultValue: resultValue}, nil
}

func (e *LogicalValuesExpression) GetColumns() []values.Value {
	return slices.Clone(e.columns)
}

func (e *LogicalValuesExpression) GetResultValue() values.Value {
	return e.resultValue
}

func (e *LogicalValuesExpression) GetQuantifiers() []Quantifier { return nil }

func (e *LogicalValuesExpression) CanCorrelate() bool { return false }

func (e *LogicalValuesExpression) ChildrenAsSet() bool { return false }

func (e *LogicalValuesExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (e *LogicalValuesExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalValuesExpression)
	if !ok {
		return false
	}
	if len(e.columns) != len(o.columns) {
		return false
	}
	// Alias-aware column-Value equality (RFC-040 040.2). Inert under the
	// memo's empty-alias path until PR-A.
	vm := aliases.ToValuesAliasMap()
	for i := range e.columns {
		if !values.SemanticEqualsUnderAliasMap(e.columns[i], o.columns[i], vm) {
			return false
		}
	}
	return true
}

func (e *LogicalValuesExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("values|"))
	var buf [8]byte
	for _, v := range e.columns {
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(v))
		h.Write(buf[:])
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *LogicalValuesExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("LogicalValuesExpression", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return e, nil
}

var _ RelationalExpression = (*LogicalValuesExpression)(nil)
