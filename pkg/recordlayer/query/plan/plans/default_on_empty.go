package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryDefaultOnEmptyPlan returns the inner plan's rows if any
// exist, or a single row with the default value if the inner is empty.
// Mirrors Java's RecordQueryDefaultOnEmptyPlan.
type RecordQueryDefaultOnEmptyPlan struct {
	PlanExprBase
	innerQ       expressions.Quantifier
	defaultValue values.Value
}

func NewRecordQueryDefaultOnEmptyPlan(inner RecordQueryPlan, defaultValue values.Value) (*RecordQueryDefaultOnEmptyPlan, error) {
	return NewRecordQueryDefaultOnEmptyPlanFromQuantifier(QuantifierOverPlan(inner), defaultValue)
}

// NewRecordQueryDefaultOnEmptyPlanFromQuantifier builds a default-on-empty whose
// child is a LIVE memo quantifier (the implementation rule passes the live
// currentQuant) instead of a snapshot over a single plan. This makes the plan its
// own cascades expression carrying its child edge directly: the memo holds it
// without a physical wrapper, and GetInner / GetQuantifiers / GetResultValue all
// resolve through the one live edge (RFC-184 W2).
func NewRecordQueryDefaultOnEmptyPlanFromQuantifier(innerQ expressions.Quantifier, defaultValue values.Value) (*RecordQueryDefaultOnEmptyPlan, error) {
	base, err := newDefaultOnEmptyPlanExprBase(innerQ, defaultValue)
	if err != nil {
		return nil, err
	}
	return &RecordQueryDefaultOnEmptyPlan{PlanExprBase: base, innerQ: innerQ, defaultValue: defaultValue}, nil
}

// newDefaultOnEmptyPlanExprBase admits the two result alternatives together.
// Java verifies that the child and default types are identical after widening
// each root to nullable, then chooses whichever original type is nullable (or
// the child when both have the same nullability). The output is therefore not
// necessarily the child's type: a NOT NULL child plus a typed NULL default
// produces a nullable result.
//
// Physically, the operator still consumes its one child in that child's exact
// layout. Its provided layout is a fresh identity layout for the reconciled
// output type: the fabricated default row does not provide any source windows
// the child happened to carry, so forwarding those windows would be unsound.
func newDefaultOnEmptyPlanExprBase(
	innerQ expressions.Quantifier,
	defaultValue values.Value,
) (PlanExprBase, error) {
	const owner = "RecordQueryDefaultOnEmptyPlan"
	if defaultValue == nil {
		return PlanExprBase{}, fmt.Errorf("%s default Value: value is nil", owner)
	}

	childBase, err := newPlanExprBaseForQuantifier(owner, innerQ)
	if err != nil {
		return PlanExprBase{}, err
	}
	childType := childBase.GetResultValue().Type()
	defaultTypeHandle, err := values.SnapshotExactType(defaultValue.Type())
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s default Value: %w", owner, err)
	}
	defaultType := defaultTypeHandle.Type()
	if !values.WithNullability(childType, true).Equals(values.WithNullability(defaultType, true)) {
		return PlanExprBase{}, fmt.Errorf(
			"%s result Value: child type %s is incompatible with default type %s",
			owner, childType, defaultType)
	}

	resultType := childType
	if !childType.IsNullable() && defaultType.IsNullable() {
		resultType = defaultType
	}
	provided, err := newIdentityOutputLayout(resultType)
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, err)
	}
	childProperties, err := childBase.OrdinalPhysicalProperties()
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s required input layout: %w", owner, err)
	}
	requirement, err := RequireExactLayout(childProperties.ProvidedOutputLayout())
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s required input layout: %w", owner, err)
	}
	return newPlanExprBaseWithProperties(
		owner,
		provided.Carrier(),
		[]OrdinalLayoutRequirement{requirement},
		provided,
	)
}

func (p *RecordQueryDefaultOnEmptyPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// reanchorInputValueToOutput carries a value across the null-extension.
//
// This plan is the operator that makes an outer join's null-supplying side
// null-supplying: it republishes its child's row widened to nullable, so its
// carrier is a DIFFERENT handle with a DIFFERENT exact type. The generic
// descendant walk therefore stops here — correctly, since it may only cross
// wrappers that preserve the layout exactly — and a value naming a source
// buried inside the child (`b.bid` under `mb JOIN mc RIGHT JOIN ma`) reached the
// enclosing producer with no ownership proof available for it. Before the
// ownership gate, one output slot carrying the same accessor name answered on
// its behalf; that is right when the names are unique and a coin flip when they
// are not.
//
// So the crossing is stated instead of guessed: descend to the child's own
// lineage authority, then move the resulting root from the child's row onto this
// plan's widened row. Both halves are checked — a child with no materializer or
// a value the child cannot place comes back untouched, and the widening itself
// refuses anything that is not the same row.
func (p *RecordQueryDefaultOnEmptyPlan) reanchorInputValueToOutput(
	value values.Value,
) (values.Value, error) {
	inner := p.GetInner()
	if value == nil || inner == nil {
		return value, nil
	}
	materializer, ok := descendantValueMaterializer(inner)
	if !ok {
		return value, nil
	}
	crossed, err := materializer.reanchorInputValueToOutput(value)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryDefaultOnEmptyPlan inner lineage: %w", err)
	}
	innerLayout, err := inner.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryDefaultOnEmptyPlan inner layout: %w", err)
	}
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryDefaultOnEmptyPlan output layout: %w", err)
	}
	if innerLayout.Carrier() == outputLayout.Carrier() {
		return crossed, nil
	}
	widened, err := values.TranslateNullExtendedPhaseRoot(
		crossed, innerLayout.Carrier(), outputLayout.Carrier())
	if err != nil {
		return nil, fmt.Errorf("RecordQueryDefaultOnEmptyPlan null extension: %w", err)
	}
	return widened, nil
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// default-on-empty ranges over. derivationsForDefaultOnEmpty reads its alias to
// translate the default value's correlation; since RFC-184 W2 the memo holds the
// bare plan (no physicalDefaultOnEmptyWrapper whose innerQuant field it used to
// read), this exposes the same edge.
func (p *RecordQueryDefaultOnEmptyPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the stable exact carrier admitted from both result
// alternatives. It is nullable when either the child or default is nullable,
// matching Java's DerivedValue(child, default) result contract.
func (p *RecordQueryDefaultOnEmptyPlan) GetResultValue() values.Value {
	return p.PlanExprBase.GetResultValue()
}

func (p *RecordQueryDefaultOnEmptyPlan) GetDefaultValue() values.Value { return p.defaultValue }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryDefaultOnEmptyPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

func (p *RecordQueryDefaultOnEmptyPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

func (p *RecordQueryDefaultOnEmptyPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the default value by semantic Value identity
// (RFC-176 P2 — see semanticValueEquals).
func (p *RecordQueryDefaultOnEmptyPlan) structuralKey() *structuralKey {
	return newStructuralKey().Value(p.defaultValue)
}

func (p *RecordQueryDefaultOnEmptyPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDefaultOnEmptyPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryDefaultOnEmptyPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("defaultonemptyplan|")
}

func (p *RecordQueryDefaultOnEmptyPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return "DefaultOnEmpty(" + innerLabel + ")"
}

var (
	_ RecordQueryPlan                  = (*RecordQueryDefaultOnEmptyPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryDefaultOnEmptyPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryDefaultOnEmptyPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryDefaultOnEmptyPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryDefaultOnEmptyPlan", len(qs), 1); err != nil {
		return nil, err
	}
	cp := *p
	base, err := newDefaultOnEmptyPlanExprBase(qs[0], p.defaultValue)
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	cp.innerQ = qs[0]
	return &cp, nil
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the default-on-empty carries its child as a single LIVE memo
// edge, the relink is exactly a quantifier swap: WithQuantifiers preserves the
// default value, and GetInner re-resolves through the new singleton reference. This
// replaces physicalDefaultOnEmptyWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field forced a constructor rebuild gated on isLeafReplaceable — a
// single live child edge needs neither.
func (p *RecordQueryDefaultOnEmptyPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryDefaultOnEmptyPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryDefaultOnEmptyPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
