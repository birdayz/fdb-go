package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFilterPlan applies a list of QueryPredicates to an
// inner plan's row stream. Mirrors Java's `RecordQueryFilterPlan`.
//
// Seed surface: predicates list + inner plan. The plan's result
// type is the inner's result type (filter doesn't reshape rows).
//
// Note: physical filter (the row-by-row ANDed predicate evaluation)
// vs logical filter (the LogicalFilterExpression rule input) are
// separate concepts. ImplementFilterRule (B5 Batch A) lifts a
// LogicalFilter into this plan.
type RecordQueryFilterPlan struct {
	PlanExprBase
	predicates []predicates.QueryPredicate
	innerQ     expressions.Quantifier
}

// NewRecordQueryFilterPlan constructs a filter over the given
// predicates and inner plan.
func NewRecordQueryFilterPlan(preds []predicates.QueryPredicate, inner RecordQueryPlan) (*RecordQueryFilterPlan, error) {
	return NewRecordQueryFilterPlanFromQuantifier(preds, QuantifierOverPlan(inner))
}

func NewRecordQueryFilterPlanFromQuantifier(preds []predicates.QueryPredicate, innerQ expressions.Quantifier) (*RecordQueryFilterPlan, error) {
	base, err := newPlanExprBaseForQuantifier("RecordQueryFilterPlan", innerQ)
	if err != nil {
		return nil, err
	}
	return &RecordQueryFilterPlan{
		PlanExprBase: base,
		predicates:   append([]predicates.QueryPredicate(nil), preds...),
		innerQ:       innerQ,
	}, nil
}

// GetPredicates returns the predicate list (read-only).
func (p *RecordQueryFilterPlan) GetPredicates() []predicates.QueryPredicate { return p.predicates }

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryFilterPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryFilterPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultType returns the inner's result type (filter doesn't
// reshape rows).
func (p *RecordQueryFilterPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryFilterPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey lists the fields that distinguish this filter in the memo: the
// predicate list. Children are excluded. The same key drives both
// EqualsPlanWithoutChildren and HashCodeWithoutChildren, so the two can never
// disagree on which fields matter.
func (p *RecordQueryFilterPlan) structuralKey() *structuralKey {
	return newStructuralKey().Preds(p.predicates)
}

// EqualsPlanWithoutChildren compares the predicate list pairwise via
// PredicateEquals.
func (p *RecordQueryFilterPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFilterPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes the class discriminator + per-predicate
// predicates.SemanticHashCode (alias-invariant, coarser than the structural
// PredicateEquals — equal⟹same-hash holds by construction). NOT Explain()
// display text: renderings are for humans, carry no identity contract, and
// drift independently of equality.
func (p *RecordQueryFilterPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("filterplan|")
}

// Explain renders Filter([P1, P2], inner).
func (p *RecordQueryFilterPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("Filter([%d preds], %s)", len(p.predicates), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryFilterPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryFilterPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryFilterPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers atomically rebuilds the filter over the replacement child.
// Its predicates are executable programs over the child edge, so every
// embedded Value must move to the replacement alias before the new pass-through
// base is admitted.
func (p *RecordQueryFilterPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryFilterPlan", len(qs), 1); err != nil {
		return nil, err
	}
	oldInput, err := p.innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFilterPlan.WithQuantifiers old input: %w", err)
	}
	newInput, err := qs[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFilterPlan.WithQuantifiers new input: %w", err)
	}
	if !oldInput.FlowedType().Equals(newInput.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryFilterPlan.WithQuantifiers input type changed from %s to %s",
			oldInput.FlowedType(), newInput.FlowedType())
	}

	rebased := append([]predicates.QueryPredicate(nil), p.predicates...)
	if oldInput.Correlation() != newInput.Correlation() {
		aliasMap, mapErr := values.NewAliasMap([]values.AliasPair{{
			Source: oldInput.Correlation(), Target: newInput.Correlation(),
		}})
		if mapErr != nil {
			return nil, fmt.Errorf("RecordQueryFilterPlan.WithQuantifiers alias map: %w", mapErr)
		}
		for i, predicate := range rebased {
			rebased[i], err = predicates.RebasePredicateChecked(predicate, aliasMap)
			if err != nil {
				return nil, fmt.Errorf("RecordQueryFilterPlan.WithQuantifiers predicate %d: %w", i, err)
			}
		}
	}
	return NewRecordQueryFilterPlanFromQuantifier(rebased, qs[0])
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryFilterPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
