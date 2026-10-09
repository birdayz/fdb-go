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
func NewRecordQueryFirstOrDefaultPlan(inner RecordQueryPlan, defaultValue values.Value) (*RecordQueryFirstOrDefaultPlan, error) {
	return NewRecordQueryFirstOrDefaultPlanFromQuantifier(QuantifierOverPlan(inner), defaultValue)
}

// NewRecordQueryFirstOrDefaultPlanStrict constructs a first-or-default plan that
// raises a cardinality violation (21000) when the inner yields more than one row.
func NewRecordQueryFirstOrDefaultPlanStrict(inner RecordQueryPlan, defaultValue values.Value) (*RecordQueryFirstOrDefaultPlan, error) {
	return NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier(QuantifierOverPlan(inner), defaultValue)
}

// NewRecordQueryFirstOrDefaultPlanFromQuantifier retains the supplied memo edge;
// its partition alternatives remain available until child optimization.
func NewRecordQueryFirstOrDefaultPlanFromQuantifier(innerQ expressions.Quantifier, defaultValue values.Value) (*RecordQueryFirstOrDefaultPlan, error) {
	return newRecordQueryFirstOrDefaultPlan(innerQ, defaultValue, false)
}

// NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier is the strict
// (at-most-one-row → 21000) form of NewRecordQueryFirstOrDefaultPlanFromQuantifier.
// It preserves BOTH the empty→default value AND the strict cardinality flag.
func NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier(innerQ expressions.Quantifier, defaultValue values.Value) (*RecordQueryFirstOrDefaultPlan, error) {
	return newRecordQueryFirstOrDefaultPlan(innerQ, defaultValue, true)
}

func newRecordQueryFirstOrDefaultPlan(innerQ expressions.Quantifier, defaultValue values.Value, strict bool) (*RecordQueryFirstOrDefaultPlan, error) {
	// Java's constructor adopts the default's root nullability, but its execution
	// returns a nonempty child unchanged, including a nullable child's SQL NULL.
	// Use the union of both alternatives (as DefaultOnEmpty does) so the exact
	// physical contract covers those actual values instead of understating
	// nullability or rejecting a valid child at the runtime binder.
	base, err := newDefaultResultPlanExprBase("RecordQueryFirstOrDefaultPlan", innerQ, defaultValue)
	if err != nil {
		return nil, err
	}
	return &RecordQueryFirstOrDefaultPlan{PlanExprBase: base, innerQ: innerQ, defaultValue: defaultValue, strict: strict}, nil
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryFirstOrDefaultPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier exposes the child binding without selecting a plan.
func (p *RecordQueryFirstOrDefaultPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the stable output carrier admitted from both result
// alternatives. A fabricated default cannot provide the child's source windows.
func (p *RecordQueryFirstOrDefaultPlan) GetResultValue() values.Value {
	return p.PlanExprBase.GetResultValue()
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryFirstOrDefaultPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return quantifierView(&p.innerQ)
}

// GetDefaultValue returns the fallback value used when the inner plan
// is empty.
func (p *RecordQueryFirstOrDefaultPlan) GetDefaultValue() values.Value { return p.defaultValue }

// IsStrict reports whether the plan enforces the at-most-one-row scalar-subquery
// cardinality rule (error 21000 on a second row).
func (p *RecordQueryFirstOrDefaultPlan) IsStrict() bool { return p.strict }

// GetResultType returns the reconciled child/default result type.
func (p *RecordQueryFirstOrDefaultPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

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
	return ok && p.keyFor(p).Equal(o.keyFor(o))
}

func (p *RecordQueryFirstOrDefaultPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.keyFor(p).Hash("firstordefaultplan|")
	p.storeStructuralHash(p, hash)
	return hash
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
func (p *RecordQueryFirstOrDefaultPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryFirstOrDefaultPlan", len(qs), 1); err != nil {
		return nil, err
	}
	cp := *p
	base, err := newDefaultResultPlanExprBase("RecordQueryFirstOrDefaultPlan", qs[0], p.defaultValue)
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	cp.innerQ = qs[0]
	return &cp, nil
}

// WithChildren relinks the extracted child while preserving the default and
// scalar-cardinality check.
func (p *RecordQueryFirstOrDefaultPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryFirstOrDefaultPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// reanchorInputValueToOutput preserves only proven child lineage across the
// default-producing boundary, including the root-nullability widening.
func (p *RecordQueryFirstOrDefaultPlan) reanchorInputValueToOutput(value values.Value) (values.Value, error) {
	return reanchorDefaultInputValueToOutput(p, selectedPlanFromQuantifier(p.innerQ), value)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryFirstOrDefaultPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
