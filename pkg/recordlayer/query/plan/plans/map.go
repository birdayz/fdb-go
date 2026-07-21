package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryMapPlan applies a transformation value to each row
// produced by an inner plan. Mirrors Java's `RecordQueryMapPlan`.
//
// The resultValue defines the output shape — its Type() becomes the
// plan's result type, and at execution time each inner row is fed
// through the value's Evaluate to produce the output row.
type RecordQueryMapPlan struct {
	PlanExprBase
	innerQ      expressions.Quantifier
	resultValue values.Value
}

// NewRecordQueryMapPlan constructs a map plan over the given inner
// plan and result value.
func NewRecordQueryMapPlan(inner RecordQueryPlan, resultValue values.Value) *RecordQueryMapPlan {
	return &RecordQueryMapPlan{
		innerQ:      QuantifierOverPlan(inner),
		resultValue: resultValue,
	}
}

// NewRecordQueryMapPlanFromQuantifier builds a map whose child is a LIVE memo
// quantifier (the implementation rule passes a ForEachQuantifier over the
// freshly-memoized inner) instead of a snapshot over a single plan. This makes
// the map its own cascades expression carrying its child edge directly: the memo
// holds it without a physical wrapper, and GetInner / GetQuantifiers /
// OrderingSourceRef / GetResultValue all resolve through the one live edge
// (RFC-184 W2). resultValue is the PROJECTION (a RecordConstructor over the
// projection list), unchanged from NewRecordQueryMapPlan.
func NewRecordQueryMapPlanFromQuantifier(innerQ expressions.Quantifier, resultValue values.Value) *RecordQueryMapPlan {
	return &RecordQueryMapPlan{innerQ: innerQ, resultValue: resultValue}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryMapPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetInnerQuantifier returns the live child quantifier — the single memo edge
// the map ranges over. The PushMapThroughFetch rule matches a physical map in
// the memo and needs its inner GROUP (GetRangesOver) and alias to re-plan around
// it; since RFC-184 W2 the memo holds the bare plan (no physicalMapWrapper whose
// innerQuant field it used to read), this exposes the same edge.
func (p *RecordQueryMapPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryMapPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultValue returns the transformation value.
func (p *RecordQueryMapPlan) GetResultValue() values.Value {
	return p.resultValue
}

// GetResultType returns the result value's type.
func (p *RecordQueryMapPlan) GetResultType() values.Type {
	if p.resultValue == nil {
		return values.UnknownType
	}
	return p.resultValue.Type()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryMapPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey lists the fields that distinguish this map in the memo: the
// result value, compared by semantic Value identity (RFC-176 P2 — see
// semanticValueEquals), Java's RecordQueryMapPlan.equalsWithoutChildren →
// semanticEqualsForResults. Children are excluded. The same key drives both
// EqualsPlanWithoutChildren and HashCodeWithoutChildren, so the two can never
// disagree on which fields matter.
func (p *RecordQueryMapPlan) structuralKey() *structuralKey {
	return newStructuralKey().Value(p.resultValue)
}

func (p *RecordQueryMapPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMapPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryMapPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("mapplan|")
}

// Explain renders Map(inner, result).
func (p *RecordQueryMapPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	resultLabel := values.ExplainValue(p.resultValue)
	return fmt.Sprintf("Map(%s, %s)", innerLabel, resultLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryMapPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryMapPlan)(nil)
)

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryMapPlan) WithInner(inner RecordQueryPlan) *RecordQueryMapPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryMapPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryMapPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the map carries its child as a single LIVE memo edge, the
// relink is exactly a quantifier swap: WithQuantifiers preserves the projection
// result value, and GetInner re-resolves through the new singleton reference.
// This replaces physicalMapWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field forced a constructor rebuild gated on isLeafReplaceable —
// a single live child edge needs neither the snapshot nor the gate (extraction
// resolves the child group to its winner and hands it back through this swap).
func (p *RecordQueryMapPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryMapPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryMapPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
