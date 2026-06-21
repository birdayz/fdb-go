package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
	inner      RecordQueryPlan
	predicates []predicates.QueryPredicate
	innerAlias values.CorrelationIdentifier
}

// NewRecordQueryPredicatesFilterPlan constructs a predicates filter
// over the given inner plan and predicate list.
func NewRecordQueryPredicatesFilterPlan(inner RecordQueryPlan, preds []predicates.QueryPredicate) *RecordQueryPredicatesFilterPlan {
	return &RecordQueryPredicatesFilterPlan{
		inner:      inner,
		predicates: append([]predicates.QueryPredicate(nil), preds...),
	}
}

// NewRecordQueryPredicatesFilterPlanWithAlias constructs a predicates
// filter that binds the current row as a correlation under innerAlias
// before evaluating predicates. Mirrors Java's evalFilter which calls
// context.withBinding(CORRELATION, getInner().getAlias(), queryResult).
func NewRecordQueryPredicatesFilterPlanWithAlias(inner RecordQueryPlan, preds []predicates.QueryPredicate, alias values.CorrelationIdentifier) *RecordQueryPredicatesFilterPlan {
	return &RecordQueryPredicatesFilterPlan{
		inner:      inner,
		predicates: append([]predicates.QueryPredicate(nil), preds...),
		innerAlias: alias,
	}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryPredicatesFilterPlan) GetInner() RecordQueryPlan { return p.inner }

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
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryPredicatesFilterPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares the predicate list pairwise via
// PredicateEquals.
func (p *RecordQueryPredicatesFilterPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryPredicatesFilterPlan)
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

// HashCodeWithoutChildren mixes the class discriminator + per-
// predicate Explain-rendered text.
func (p *RecordQueryPredicatesFilterPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("predicatesfilterplan|"))
	for _, pr := range p.predicates {
		if pr == nil {
			h.Write([]byte("<nil>"))
		} else {
			h.Write([]byte(pr.Explain()))
		}
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders PredicatesFilter(inner, [pred1, pred2, ...]).
func (p *RecordQueryPredicatesFilterPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("PredicatesFilter(%s, [%d preds])", innerLabel, len(p.predicates))
}

var _ RecordQueryPlan = (*RecordQueryPredicatesFilterPlan)(nil)
