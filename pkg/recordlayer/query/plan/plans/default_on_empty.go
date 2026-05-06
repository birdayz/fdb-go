package plans

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryDefaultOnEmptyPlan returns the inner plan's rows if any
// exist, or a single row with the default value if the inner is empty.
// Mirrors Java's RecordQueryDefaultOnEmptyPlan.
type RecordQueryDefaultOnEmptyPlan struct {
	inner        RecordQueryPlan
	defaultValue values.Value
}

func NewRecordQueryDefaultOnEmptyPlan(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryDefaultOnEmptyPlan {
	return &RecordQueryDefaultOnEmptyPlan{inner: inner, defaultValue: defaultValue}
}

func (p *RecordQueryDefaultOnEmptyPlan) GetInner() RecordQueryPlan     { return p.inner }
func (p *RecordQueryDefaultOnEmptyPlan) GetDefaultValue() values.Value { return p.defaultValue }

func (p *RecordQueryDefaultOnEmptyPlan) GetResultType() values.Type {
	if p.inner != nil {
		return p.inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryDefaultOnEmptyPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryDefaultOnEmptyPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDefaultOnEmptyPlan)
	if !ok {
		return false
	}
	return values.ExplainValue(p.defaultValue) == values.ExplainValue(o.defaultValue)
}

func (p *RecordQueryDefaultOnEmptyPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("defaultonemptyplan|"))
	if p.defaultValue != nil {
		h.Write([]byte(values.ExplainValue(p.defaultValue)))
	}
	return h.Sum64()
}

func (p *RecordQueryDefaultOnEmptyPlan) Explain() string {
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	return "DefaultOnEmpty(" + inner + ")"
}

var _ RecordQueryPlan = (*RecordQueryDefaultOnEmptyPlan)(nil)
