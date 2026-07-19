package plans

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"

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
	// innerAlias is identity: it is the correlation the predicates resolve
	// the current row under, so two filters differing only in binding alias
	// evaluate differently and must not collapse into one memo group (the
	// same field-class as the NLJ/FlatMap outer/inner aliases).
	if p.innerAlias != o.innerAlias {
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
func (p *RecordQueryPredicatesFilterPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("predicatesfilterplan|"))
	h.Write([]byte(p.innerAlias.Name()))
	h.Write([]byte{0})
	var buf [8]byte
	for _, pr := range p.predicates {
		binary.BigEndian.PutUint64(buf[:], predicates.SemanticHashCode(pr))
		h.Write(buf[:])
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

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryPredicatesFilterPlan) WithInner(inner RecordQueryPlan) *RecordQueryPredicatesFilterPlan {
	cp := *p
	cp.inner = inner
	return &cp
}
