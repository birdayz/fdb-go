package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFirstOrDefaultPlan takes the first row from the inner
// plan, or returns a default value if the inner plan produces no
// rows. Mirrors Java's `RecordQueryFirstOrDefaultPlan`.
//
// When strict is set, the plan additionally enforces the SQL scalar-subquery
// cardinality rule: if the inner produces MORE THAN ONE row it is a cardinality
// violation (21000), not a silent truncation to the first row. This is the
// correlated-scalar-subquery barrier — a non-pushable, per-outer-row check that
// mirrors the uncorrelated path (executor.EvaluateScalarSubquery). A user-written
// LIMIT never sets strict: truncation is then the user's deliberate intent.
type RecordQueryFirstOrDefaultPlan struct {
	inner        RecordQueryPlan
	defaultValue values.Value
	strict       bool
}

// NewRecordQueryFirstOrDefaultPlan constructs a first-or-default plan
// over the given inner plan and default value.
func NewRecordQueryFirstOrDefaultPlan(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{
		inner:        inner,
		defaultValue: defaultValue,
	}
}

// NewRecordQueryFirstOrDefaultPlanStrict constructs a first-or-default plan that
// raises a cardinality violation (21000) when the inner yields more than one row.
func NewRecordQueryFirstOrDefaultPlanStrict(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{
		inner:        inner,
		defaultValue: defaultValue,
		strict:       true,
	}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryFirstOrDefaultPlan) GetInner() RecordQueryPlan { return p.inner }

// GetDefaultValue returns the fallback value used when the inner plan
// is empty.
func (p *RecordQueryFirstOrDefaultPlan) GetDefaultValue() values.Value { return p.defaultValue }

// IsStrict reports whether the plan enforces the at-most-one-row scalar-subquery
// cardinality rule (error 21000 on a second row).
func (p *RecordQueryFirstOrDefaultPlan) IsStrict() bool { return p.strict }

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

// EqualsWithoutChildren compares the default value by semantic Value identity
// (RFC-176 P2 — see semanticValueEquals).
func (p *RecordQueryFirstOrDefaultPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFirstOrDefaultPlan)
	if !ok {
		return false
	}
	return p.strict == o.strict && semanticValueEquals(p.defaultValue, o.defaultValue)
}

// HashCodeWithoutChildren mixes the class discriminator + default value's
// semantic hash (see writeValueHash).
func (p *RecordQueryFirstOrDefaultPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("firstordefaultplan|"))
	if p.strict {
		h.Write([]byte("strict|"))
	}
	writeValueHash(h, p.defaultValue)
	return h.Sum64()
}

// Explain renders FirstOrDefault(inner) (StrictFirstOrDefault when strict).
func (p *RecordQueryFirstOrDefaultPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	name := "FirstOrDefault"
	if p.strict {
		name = "StrictFirstOrDefault"
	}
	return fmt.Sprintf("%s(%s)", name, innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryFirstOrDefaultPlan)(nil)
