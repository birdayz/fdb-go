package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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
	PlanExprBase
	innerQ       expressions.Quantifier
	defaultValue values.Value
	strict       bool
}

// NewRecordQueryFirstOrDefaultPlan constructs a first-or-default plan
// over the given inner plan and default value.
func NewRecordQueryFirstOrDefaultPlan(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{
		innerQ:       QuantifierOverPlan(inner),
		defaultValue: defaultValue,
	}
}

// NewRecordQueryFirstOrDefaultPlanStrict constructs a first-or-default plan that
// raises a cardinality violation (21000) when the inner yields more than one row.
func NewRecordQueryFirstOrDefaultPlanStrict(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{
		innerQ:       QuantifierOverPlan(inner),
		defaultValue: defaultValue,
		strict:       true,
	}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryFirstOrDefaultPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryFirstOrDefaultPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetDefaultValue returns the fallback value used when the inner plan
// is empty.
func (p *RecordQueryFirstOrDefaultPlan) GetDefaultValue() values.Value { return p.defaultValue }

// IsStrict reports whether the plan enforces the at-most-one-row scalar-subquery
// cardinality rule (error 21000 on a second row).
func (p *RecordQueryFirstOrDefaultPlan) IsStrict() bool { return p.strict }

// GetResultType returns the inner's result type.
func (p *RecordQueryFirstOrDefaultPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryFirstOrDefaultPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the default value by semantic Value identity
// (RFC-176 P2 — see semanticValueEquals).
func (p *RecordQueryFirstOrDefaultPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
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
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	name := "FirstOrDefault"
	if p.strict {
		name = "StrictFirstOrDefault"
	}
	return fmt.Sprintf("%s(%s)", name, innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryFirstOrDefaultPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryFirstOrDefaultPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryFirstOrDefaultPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryFirstOrDefaultPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}
