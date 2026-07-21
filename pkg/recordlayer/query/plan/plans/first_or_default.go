package plans

import (
	"fmt"

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

// NewRecordQueryFirstOrDefaultPlanFromQuantifier builds a first-or-default whose
// child is a supplied memo quantifier instead of a snapshot over a single plan.
// This makes the plan its own cascades expression carrying its child edge
// directly — the memo holds it without a physicalFirstOrDefaultWrapper
// (RFC-184 W2).
//
// Unlike DefaultOnEmpty/InJoin (which range over the LIVE shared exploratory
// group and resolve via ref.Winner()), the FirstOrDefault emitter freezes a
// DISENTANGLED FINAL reference holding the CONSTRAINT-SATISFYING correlated
// inner (constraint-preserving disentangle). Its inner is the concrete
// correlated/SARG member — never the shared-group bare winner — so
// planFromQuantifier resolves the correlated inner and the correlation on the
// DML DELETE/UPDATE-WHERE-EXISTS path is preserved. The wrapper's second live
// edge (which floated to the bare winner and dropped the filter) is gone; the
// single frozen edge does both jobs.
func NewRecordQueryFirstOrDefaultPlanFromQuantifier(innerQ expressions.Quantifier, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{innerQ: innerQ, defaultValue: defaultValue}
}

// NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier is the strict
// (at-most-one-row → 21000) form of NewRecordQueryFirstOrDefaultPlanFromQuantifier.
// It preserves BOTH the empty→default value AND the strict cardinality flag.
func NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier(innerQ expressions.Quantifier, defaultValue values.Value) *RecordQueryFirstOrDefaultPlan {
	return &RecordQueryFirstOrDefaultPlan{innerQ: innerQ, defaultValue: defaultValue, strict: true}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryFirstOrDefaultPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single frozen memo
// edge the first-or-default ranges over. derivationsForFirstOrDefault reads its
// alias to translate the default value's correlation; since RFC-184 W2 the memo
// holds the bare plan (no physicalFirstOrDefaultWrapper whose innerQuant field it
// used to read), this exposes the same edge.
func (p *RecordQueryFirstOrDefaultPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the flowed object value of the child quantifier — a
// first-or-default passes its input's rows through (with an empty→default row),
// so its row identity IS the inner's. This is the identity
// physicalFirstOrDefaultWrapper.GetResultValue supplied (RFC-184 W2).
func (p *RecordQueryFirstOrDefaultPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
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

// structuralKey lists the fields that distinguish this plan in the memo: the
// strict flag and the default value (compared by semantic Value identity,
// RFC-176 P2 — see semanticValueEquals). Children are excluded; the same key
// drives both EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryFirstOrDefaultPlan) structuralKey() *structuralKey {
	return newStructuralKey().Bool(p.strict).Value(p.defaultValue)
}

func (p *RecordQueryFirstOrDefaultPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFirstOrDefaultPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryFirstOrDefaultPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("firstordefaultplan|")
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

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The first-or-default carries its child as a single frozen memo
// edge, so the relink is a quantifier swap: WithQuantifiers preserves the
// default value AND the strict flag, and GetInner re-resolves through the new
// singleton reference. This replaces physicalFirstOrDefaultWrapper.WithChildren
// (RFC-184 W2), whose separate snapshot plan field forced a constructor rebuild
// gated on isLeafReplaceable. Because the emitter already froze the
// correlated/SARG inner into a private single-member reference, extraction
// recurses through it faithfully — it never consults the shared exploratory
// group, so the correlation cannot be dropped.
func (p *RecordQueryFirstOrDefaultPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryFirstOrDefaultPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryFirstOrDefaultPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
