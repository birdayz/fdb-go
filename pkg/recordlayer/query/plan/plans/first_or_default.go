package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFirstOrDefaultPlan takes the first row from the inner
// plan, or returns a default value if the inner plan produces no
// rows. Mirrors Java's `RecordQueryFirstOrDefaultPlan`.
type RecordQueryFirstOrDefaultPlan struct {
	inner        RecordQueryPlan
	defaultValue values.Value
}

// NewRecordQueryFirstOrDefaultPlan constructs a first-or-default plan
// over the given inner plan and default value.
func NewRecordQueryFirstOrDefaultPlan(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{
		inner:        inner,
		defaultValue: defaultValue,
	}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryFirstOrDefaultPlan) GetInner() RecordQueryPlan { return p.inner }

// GetDefaultValue returns the fallback value used when the inner plan
// is empty.
func (p *RecordQueryFirstOrDefaultPlan) GetDefaultValue() values.Value { return p.defaultValue }

// GetResultType returns the inner's result type.
func (p *RecordQueryFirstOrDefaultPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryFirstOrDefaultPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares the default value via ExplainValue.
func (p *RecordQueryFirstOrDefaultPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFirstOrDefaultPlan)
	if !ok {
		return false
	}
	return values.ExplainValue(p.defaultValue) == values.ExplainValue(o.defaultValue)
}

// HashCodeWithoutChildren mixes the class discriminator + default
// value's explain text.
func (p *RecordQueryFirstOrDefaultPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("firstordefaultplan|"))
	h.Write([]byte(values.ExplainValue(p.defaultValue)))
	return h.Sum64()
}

// Explain renders FirstOrDefault(inner).
func (p *RecordQueryFirstOrDefaultPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("FirstOrDefault(%s)", innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryFirstOrDefaultPlan)(nil)
