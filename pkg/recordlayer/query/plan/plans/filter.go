package plans

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"

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
func NewRecordQueryFilterPlan(preds []predicates.QueryPredicate, inner RecordQueryPlan) *RecordQueryFilterPlan {
	return &RecordQueryFilterPlan{
		predicates: append([]predicates.QueryPredicate(nil), preds...),
		innerQ:     QuantifierOverPlan(inner),
	}
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
func (p *RecordQueryFilterPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryFilterPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the predicate list pairwise via
// PredicateEquals.
func (p *RecordQueryFilterPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFilterPlan)
	if !ok {
		return false
	}
	if len(p.predicates) != len(o.predicates) {
		return false
	}
	for i := range p.predicates {
		if !predicates.PredicateEquals(p.predicates[i], o.predicates[i]) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes the class discriminator + per-predicate
// predicates.SemanticHashCode (alias-invariant, coarser than the structural
// PredicateEquals — equal⟹same-hash holds by construction). NOT Explain()
// display text: renderings are for humans, carry no identity contract, and
// drift independently of equality.
func (p *RecordQueryFilterPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("filterplan|"))
	var buf [8]byte
	for _, pr := range p.predicates {
		binary.BigEndian.PutUint64(buf[:], predicates.SemanticHashCode(pr))
		h.Write(buf[:])
	}
	return h.Sum64()
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

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryFilterPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}
