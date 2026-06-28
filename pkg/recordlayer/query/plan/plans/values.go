package plans

import (
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryValuesPlan is a leaf physical-plan that produces a single
// row of constant values — the physical counterpart of
// LogicalValuesExpression. Mirrors SQL's VALUES (a, b, c) at execution
// time.
type RecordQueryValuesPlan struct {
	columns []values.Value
}

func NewRecordQueryValuesPlan(columns []values.Value) *RecordQueryValuesPlan {
	return &RecordQueryValuesPlan{columns: columns}
}

func (p *RecordQueryValuesPlan) GetColumns() []values.Value { return p.columns }

func (p *RecordQueryValuesPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryValuesPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryValuesPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryValuesPlan)
	if !ok {
		return false
	}
	if len(p.columns) != len(o.columns) {
		return false
	}
	for i := range p.columns {
		if values.ExplainValue(p.columns[i]) != values.ExplainValue(o.columns[i]) {
			return false
		}
	}
	return true
}

func (p *RecordQueryValuesPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("valuesplan|"))
	for _, v := range p.columns {
		h.Write([]byte(values.ExplainValue(v)))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (p *RecordQueryValuesPlan) Explain() string {
	var b strings.Builder
	b.WriteString("Values(")
	for i, v := range p.columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(values.ExplainValue(v))
	}
	b.WriteByte(')')
	return b.String()
}

var _ RecordQueryPlan = (*RecordQueryValuesPlan)(nil)
