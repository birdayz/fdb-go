package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryPredicatesFilterPlan applies a list of QueryPredicates to
// an inner plan's row stream. Mirrors Java's
// `RecordQueryPredicatesFilterPlan`.
//
// Unlike RecordQueryFilterPlan (which also takes QueryPredicates),
// this variant is produced by ImplementSimpleSelectRule and models the
// Cascades-era predicate-filter operator that works with the richer
// predicate hierarchy (ValuePredicate, ExistentialValuePredicate, etc.) rather
// than the legacy comparison-based filter.
type RecordQueryPredicatesFilterPlan struct {
	PlanExprBase
	innerQ     expressions.Quantifier
	predicates []predicates.QueryPredicate
	innerAlias values.CorrelationIdentifier
}

// NewRecordQueryPredicatesFilterPlan constructs a predicates filter
// over the given inner plan and predicate list.
func NewRecordQueryPredicatesFilterPlan(inner RecordQueryPlan, preds []predicates.QueryPredicate) *RecordQueryPredicatesFilterPlan {
	return &RecordQueryPredicatesFilterPlan{
		innerQ:     QuantifierOverPlan(inner),
		predicates: append([]predicates.QueryPredicate(nil), preds...),
	}
}

// NewRecordQueryPredicatesFilterPlanWithAlias constructs a predicates
// filter that binds the current row as a correlation under innerAlias
// before evaluating predicates. Mirrors Java's evalFilter which calls
// context.withBinding(CORRELATION, getInner().getAlias(), queryResult).
func NewRecordQueryPredicatesFilterPlanWithAlias(inner RecordQueryPlan, preds []predicates.QueryPredicate, alias values.CorrelationIdentifier) *RecordQueryPredicatesFilterPlan {
	return &RecordQueryPredicatesFilterPlan{
		innerQ:     QuantifierOverPlan(inner),
		predicates: append([]predicates.QueryPredicate(nil), preds...),
		innerAlias: alias,
	}
}

// NewRecordQueryPredicatesFilterPlanFromQuantifier builds a predicates filter
// whose child is a supplied memo quantifier instead of a snapshot over a single
// plan. This makes the plan its own cascades expression carrying its child edge
// directly — the memo holds it without a physicalPredicatesFilterWrapper
// (RFC-184 W2).
//
// The emitter freezes a DISENTANGLED FINAL reference holding the filter's
// concrete inner member (constraint-preserving disentangle), so
// planFromQuantifier resolves that concrete member — never the shared-group
// winner. The predicate list is preserved.
func NewRecordQueryPredicatesFilterPlanFromQuantifier(innerQ expressions.Quantifier, preds []predicates.QueryPredicate) *RecordQueryPredicatesFilterPlan {
	return &RecordQueryPredicatesFilterPlan{
		innerQ:     innerQ,
		predicates: append([]predicates.QueryPredicate(nil), preds...),
	}
}

// NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier is the
// binding-alias form of NewRecordQueryPredicatesFilterPlanFromQuantifier. It
// preserves BOTH the predicate list AND the innerAlias the current row is bound
// under during predicate evaluation.
func NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(innerQ expressions.Quantifier, preds []predicates.QueryPredicate, alias values.CorrelationIdentifier) *RecordQueryPredicatesFilterPlan {
	return &RecordQueryPredicatesFilterPlan{
		innerQ:     innerQ,
		predicates: append([]predicates.QueryPredicate(nil), preds...),
		innerAlias: alias,
	}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryPredicatesFilterPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// filter ranges over. derivationsForPredicatesFilter reads its alias to translate
// the predicates' correlations; since RFC-184 W2 the memo holds the bare plan (no
// physicalPredicatesFilterWrapper whose innerQuant field it used to read), this
// exposes the same edge.
func (p *RecordQueryPredicatesFilterPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the flowed object value of the child quantifier — a
// filter passes its input's rows through unchanged, so its row identity IS the
// inner's. This is the identity physicalPredicatesFilterWrapper.GetResultValue
// supplied (RFC-184 W2).
func (p *RecordQueryPredicatesFilterPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryPredicatesFilterPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetInnerAlias returns the correlation alias under which the current
// row is bound during predicate evaluation. Zero value means no binding.
func (p *RecordQueryPredicatesFilterPlan) GetInnerAlias() values.CorrelationIdentifier {
	return p.innerAlias
}

// GetPredicates returns the predicate list (read-only).
func (p *RecordQueryPredicatesFilterPlan) GetPredicates() []predicates.QueryPredicate {
	return p.predicates
}

// GetResultType returns the inner's result type (filter doesn't
// reshape rows).
func (p *RecordQueryPredicatesFilterPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryPredicatesFilterPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the predicate list pairwise via
// PredicateEquals.
// structuralKey lists the fields that distinguish this filter in the memo: the
// binding alias and the predicate list. Children are excluded. innerAlias is
// identity — it is the correlation the predicates resolve the current row
// under, so two filters differing only in binding alias evaluate differently
// and must not collapse into one memo group (the same field-class as the
// NLJ/FlatMap outer/inner aliases).
func (p *RecordQueryPredicatesFilterPlan) structuralKey() *structuralKey {
	return newStructuralKey().Alias(p.innerAlias).Preds(p.predicates)
}

func (p *RecordQueryPredicatesFilterPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryPredicatesFilterPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes the class discriminator + per-predicate
// predicates.SemanticHashCode (alias-invariant, coarser than the structural
// PredicateEquals — equal⟹same-hash holds by construction). NOT Explain()
// display text: renderings are for humans, carry no identity contract, and
// drift independently of equality.
func (p *RecordQueryPredicatesFilterPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("predicatesfilterplan|")
}

// Explain renders PredicatesFilter(inner, [pred1, pred2, ...]).
func (p *RecordQueryPredicatesFilterPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("PredicatesFilter(%s, [%d preds])", innerLabel, len(p.predicates))
}

var (
	_ RecordQueryPlan                  = (*RecordQueryPredicatesFilterPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryPredicatesFilterPlan)(nil)
)

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryPredicatesFilterPlan) WithInner(inner RecordQueryPlan) *RecordQueryPredicatesFilterPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryPredicatesFilterPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryPredicatesFilterPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The filter carries its child as a single frozen memo edge, so the
// relink is a quantifier swap: WithQuantifiers preserves the predicate list and
// binding alias, and GetInner re-resolves through the new singleton reference.
// This replaces physicalPredicatesFilterWrapper.WithChildren (RFC-184 W2), whose
// separate snapshot plan field forced a constructor rebuild gated on
// isLeafReplaceable. Because the emitter already froze the concrete inner into a
// private single-member reference, extraction recurses through it faithfully — it
// never consults a shared exploratory group, so a correlated inner is preserved.
func (p *RecordQueryPredicatesFilterPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetCorrelatedToWithoutChildren walks this plan's own predicates, mirroring
// physicalPredicatesFilterWrapper. The predicates are this node's information — a
// correlation reached only through them would be invisible to
// correlation-driven rules if this returned the empty default.
func (p *RecordQueryPredicatesFilterPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, pred := range p.GetPredicates() {
		for k := range predicates.GetCorrelatedToOfPredicate(pred) {
			out[k] = struct{}{}
		}
	}
	return out
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryPredicatesFilterPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
