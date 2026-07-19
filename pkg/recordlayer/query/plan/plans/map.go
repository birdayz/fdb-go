package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryMapPlan applies a transformation value to each row
// produced by an inner plan. Mirrors Java's `RecordQueryMapPlan`.
//
// The resultValue defines the output shape — its Type() becomes the
// plan's result type, and at execution time each inner row is fed
// through the value's Evaluate to produce the output row.
type RecordQueryMapPlan struct {
	inner       RecordQueryPlan
	resultValue values.Value
}

// NewRecordQueryMapPlan constructs a map plan over the given inner
// plan and result value.
func NewRecordQueryMapPlan(inner RecordQueryPlan, resultValue values.Value) *RecordQueryMapPlan {
	return &RecordQueryMapPlan{
		inner:       inner,
		resultValue: resultValue,
	}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryMapPlan) GetInner() RecordQueryPlan { return p.inner }

// GetResultValue returns the transformation value.
func (p *RecordQueryMapPlan) GetResultValue() values.Value { return p.resultValue }

// GetResultType returns the result value's type.
func (p *RecordQueryMapPlan) GetResultType() values.Type {
	if p.resultValue == nil {
		return values.UnknownType
	}
	return p.resultValue.Type()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryMapPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares the result value by semantic Value identity
// (RFC-176 P2 — see semanticValueEquals), Java's
// RecordQueryMapPlan.equalsWithoutChildren → semanticEqualsForResults.
func (p *RecordQueryMapPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMapPlan)
	if !ok {
		return false
	}
	return semanticValueEquals(p.resultValue, o.resultValue)
}

// HashCodeWithoutChildren mixes the class discriminator + result value's
// semantic hash (see writeValueHash).
func (p *RecordQueryMapPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("mapplan|"))
	writeValueHash(h, p.resultValue)
	return h.Sum64()
}

// Explain renders Map(inner, result).
func (p *RecordQueryMapPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	resultLabel := values.ExplainValue(p.resultValue)
	return fmt.Sprintf("Map(%s, %s)", innerLabel, resultLabel)
}

var _ RecordQueryPlan = (*RecordQueryMapPlan)(nil)

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryMapPlan) WithInner(inner RecordQueryPlan) *RecordQueryMapPlan {
	cp := *p
	cp.inner = inner
	return &cp
}
