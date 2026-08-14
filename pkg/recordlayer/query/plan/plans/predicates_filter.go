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
func NewRecordQueryPredicatesFilterPlan(inner RecordQueryPlan, preds []predicates.QueryPredicate) (*RecordQueryPredicatesFilterPlan, error) {
	return NewRecordQueryPredicatesFilterPlanFromQuantifier(QuantifierOverPlan(inner), preds)
}

// NewRecordQueryPredicatesFilterPlanWithAlias constructs a predicates
// filter that binds the current row as a correlation under innerAlias
// before evaluating predicates. Mirrors Java's evalFilter which calls
// context.withBinding(CORRELATION, getInner().getAlias(), queryResult).
func NewRecordQueryPredicatesFilterPlanWithAlias(inner RecordQueryPlan, preds []predicates.QueryPredicate, alias values.CorrelationIdentifier) (*RecordQueryPredicatesFilterPlan, error) {
	return NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(QuantifierOverPlan(inner), preds, alias)
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
func NewRecordQueryPredicatesFilterPlanFromQuantifier(innerQ expressions.Quantifier, preds []predicates.QueryPredicate) (*RecordQueryPredicatesFilterPlan, error) {
	return NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(innerQ, preds, values.CorrelationIdentifier{})
}

// NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier is the
// binding-alias form of NewRecordQueryPredicatesFilterPlanFromQuantifier. It
// preserves BOTH the predicate list AND the innerAlias the current row is bound
// under during predicate evaluation.
func NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(innerQ expressions.Quantifier, preds []predicates.QueryPredicate, alias values.CorrelationIdentifier) (*RecordQueryPredicatesFilterPlan, error) {
	base, err := newPlanExprBaseForQuantifier("RecordQueryPredicatesFilterPlan", innerQ)
	if err != nil {
		return nil, err
	}
	input, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan input: %w", err)
	}
	normalized := make([]predicates.QueryPredicate, len(preds))
	for i, predicate := range preds {
		normalized[i], err = translatePredicatePhysicalEdge(predicate, input.Correlation(), input)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan predicate %d: %w", i, err)
		}
		if !alias.IsZero() {
			normalized[i], err = translatePredicatePhysicalEdge(normalized[i], alias, input)
			if err != nil {
				return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan predicate %d binding alias: %w", i, err)
			}
			normalized[i], err = translateOrdinalityBindingNames(normalized[i], innerQ, alias)
			if err != nil {
				return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan predicate %d ordinality binding: %w", i, err)
			}
		}
		normalized[i], err = reanchorPredicateForInputWithAlias(normalized[i], innerQ, alias)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan predicate %d input carrier: %w", i, err)
		}
	}
	return &RecordQueryPredicatesFilterPlan{
		PlanExprBase: base,
		innerQ:       innerQ,
		predicates:   normalized,
		innerAlias:   alias,
	}, nil
}

// translateOrdinalityBindingNames crosses the one physical boundary where a
// logical source intentionally gives different names to every slot of the same
// exact positional row. SQL AS/AT aliases name an Explode WITH ORDINALITY row,
// while the physical carrier remains RECORD<_0 element, _1 ordinal>. A pushed
// predicate must keep the logical binding correlation (the executor binds that
// declared alias), but its exact row type must be the physical carrier type.
//
// The selected child and WITH ORDINALITY flag are admission authority. The
// values bridge additionally requires exact positional width, leaf types,
// nullability, record identity, and a real field-name difference. A plain
// Explode, exploratory child, current/foreign root, or structural drift is a
// pointer-stable decline.
func translateOrdinalityBindingNames(
	predicate predicates.QueryPredicate,
	inputQ expressions.Quantifier,
	bindingAlias values.CorrelationIdentifier,
) (predicates.QueryPredicate, error) {
	selected, ok := selectedPlanFromQuantifier(inputQ).(*RecordQueryExplodePlan)
	if !ok || selected == nil || !selected.IsWithOrdinality() {
		return predicate, nil
	}
	layout, err := selected.ProvidedOutputLayout()
	if err != nil {
		return nil, err
	}
	return predicates.TransformEmbeddedValuesChecked(
		predicate,
		func(value values.Value) (values.Value, error) {
			return values.TranslateProjectionInputNameNormalizationToCorrelation(
				value, bindingAlias, layout.Carrier().FlowedType())
		},
	)
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
	return p.PlanExprBase.GetResultValue()
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
	return p.GetResultValue().Type()
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
func (p *RecordQueryPredicatesFilterPlan) WithInner(inner RecordQueryPlan) (*RecordQueryPredicatesFilterPlan, error) {
	relinked, err := p.WithQuantifiers([]expressions.Quantifier{QuantifierOverPlan(inner)})
	if err != nil {
		return nil, err
	}
	return relinked.(*RecordQueryPredicatesFilterPlan), nil
}

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryPredicatesFilterPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers atomically moves every predicate Value that belongs to the
// physical child edge onto the replacement edge, then reconstructs the
// pass-through base. innerAlias is a distinct logical binding identity and is
// preserved; correlations rooted there (or in outer scopes) are deliberately
// absent from the old-edge alias map and remain unchanged.
func (p *RecordQueryPredicatesFilterPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryPredicatesFilterPlan", len(qs), 1); err != nil {
		return nil, err
	}
	oldInput, err := p.innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan.WithQuantifiers old input: %w", err)
	}
	newInput, err := qs[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan.WithQuantifiers new input: %w", err)
	}
	if !oldInput.FlowedType().Equals(newInput.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryPredicatesFilterPlan.WithQuantifiers input type changed from %s to %s",
			oldInput.FlowedType(), newInput.FlowedType())
	}

	rebased := make([]predicates.QueryPredicate, len(p.predicates))
	for i, predicate := range p.predicates {
		rebased[i], err = translatePredicatePhysicalEdge(predicate, oldInput.Correlation(), newInput)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan.WithQuantifiers predicate %d: %w", i, err)
		}
	}
	return NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
		qs[0], rebased, p.innerAlias)
}

// translatePredicatePhysicalEdge moves every exact declaration of the old
// child row onto the selected replacement edge. Logical join predicates can
// still carry the source's nominal record name while the physical scan emits
// the same row as an anonymous executor carrier. An alias-only rebase preserves
// that stale nominal type and creates a QOV the runtime edge cannot bind.
//
// Matching the complete row shape, except for that phase-local top-level
// record name, is what keeps this bridge narrow. In particular, a retained
// source window may conventionally share the edge correlation while exposing a
// narrower type; it is not the child row and remains untouched.
func translatePredicatePhysicalEdge(
	predicate predicates.QueryPredicate,
	source values.CorrelationIdentifier,
	newInput values.QuantifiedObjectValue,
) (predicates.QueryPredicate, error) {
	return predicates.TransformEmbeddedValuesChecked(
		predicate,
		func(value values.Value) (values.Value, error) {
			return values.TranslateLogicalSourceNameNormalization(value, source, newInput)
		},
	)
}

// reanchorPredicateForInput crosses the selected child's checked producer and
// layout lineage for every Value embedded in a predicate. A residual filter can
// sit directly above a materialized NLJ/FlatMap whose exact source windows use
// physical record names (BOOKS, AWARDS) while the logical predicate still uses
// anonymous B/W rows. The child's retained result program is the authority that
// proves which output ordinal each source field owns; an alias-only edge rewrite
// cannot prove that and leaves an unbindable exact QOV behind.
//
// Exploratory inputs remain unchanged because reanchorCurrentValueForInput
// declines when no concrete selected child/layout exists. Foreign/outer roots
// are likewise preserved fail-closed by the producer and layout reanchoring.
func reanchorPredicateForInputWithAlias(
	predicate predicates.QueryPredicate,
	inputQ expressions.Quantifier,
	innerAlias values.CorrelationIdentifier,
) (predicates.QueryPredicate, error) {
	owned := map[values.CorrelationIdentifier]struct{}{
		values.CurrentCorrelation(): {},
	}
	if !innerAlias.IsZero() {
		owned[innerAlias] = struct{}{}
	}
	if input, err := inputQ.RequireFlowedObjectValue(); err == nil {
		owned[input.Correlation()] = struct{}{}
	}
	if selected := selectedPlanFromQuantifier(inputQ); selected != nil {
		producer := selected.GetResultValue()
		if retained, ok := descendantRetainedResultProducer(selected); ok {
			producer = retained
		}
		for correlation := range values.GetCorrelatedToOfValue(producer) {
			owned[correlation] = struct{}{}
		}
		// A chained materializer can retain a source through more than one
		// producer boundary. The nearest FlatMap result is then expressed in its
		// direct leg bindings (X/Y), while the selected outer FlatMap is the exact
		// authority that proves a buried table declaration (T4) flows into X.
		// Inventory only result programs reached through selected materializer
		// chains. Arbitrary child predicates/comparisons are not declarations and
		// must not make a foreign correlation eligible for producer reanchoring.
		addSelectedMaterializerRetainedCorrelations(selected, owned)
	}
	return predicates.TransformEmbeddedValuesChecked(
		predicate,
		func(value values.Value) (values.Value, error) {
			allOwned := true
			for correlation := range values.GetCorrelatedToOfValue(value) {
				if _, ok := owned[correlation]; !ok {
					allOwned = false
					break
				}
			}
			if allOwned {
				return reanchorCurrentValueForInput(value, inputQ)
			}
			return reanchorOwnedPredicateValueForInput(value, inputQ, owned)
		},
	)
}

// addSelectedMaterializerRetainedCorrelations records the correlations that a
// selected materializer chain actually publishes in its retained result
// programs. It follows only exact row-preserving unary wrappers and direct
// materializer children, mirroring descendantValueMaterializer's admission.
//
// The map is intentionally correlation-only: eligibility merely permits the
// selected materializer to attempt its existing checked rewrite. That rewrite
// still requires the declaration's exact whole-row type, field path, leaf type,
// and unique producer lineage, so a same-spelled wrong-type root remains
// unchanged and is rejected by the runtime binder rather than guessed.
func addSelectedMaterializerRetainedCorrelations(
	plan RecordQueryPlan,
	owned map[values.CorrelationIdentifier]struct{},
) {
	for plan != nil {
		if _, materializes := childValueMaterializer(plan); materializes {
			for correlation := range values.GetCorrelatedToOfValue(plan.GetResultValue()) {
				if correlation.IsZero() || correlation == values.CurrentCorrelation() {
					continue
				}
				owned[correlation] = struct{}{}
			}
			for _, child := range plan.GetChildren() {
				if _, selected := descendantValueMaterializer(child); selected {
					addSelectedMaterializerRetainedCorrelations(child, owned)
				}
			}
			return
		}

		unary, ok := plan.(interface{ GetInner() RecordQueryPlan })
		if !ok {
			return
		}
		inner := unary.GetInner()
		if inner == nil {
			return
		}
		outerLayout, outerErr := plan.ProvidedOutputLayout()
		innerLayout, innerErr := inner.ProvidedOutputLayout()
		if outerErr != nil || innerErr != nil ||
			outerLayout.Carrier() != innerLayout.Carrier() ||
			!outerLayout.RawEqual(innerLayout) {
			return
		}
		plan = inner
	}
}

// reanchorOwnedPredicateValueForInput is the mixed-correlation path. Predicate
// operands can combine an input field with an outer correlation; running the
// generic producer bridge over that tree would let a one-slot input producer
// claim the foreign field by its unique accessor name. The selected child's
// retained result remains the lineage authority, but only roots already proved
// to belong to this filter input may cross it.
func reanchorOwnedPredicateValueForInput(
	value values.Value,
	inputQ expressions.Quantifier,
	owned map[values.CorrelationIdentifier]struct{},
) (values.Value, error) {
	layout, selected, err := selectedInputOrdinalLayout(inputQ)
	if err != nil || !selected {
		return value, err
	}
	target := layout.Carrier()
	if target == nil {
		return nil, fmt.Errorf("selected predicate input layout has no carrier")
	}
	edge, err := inputQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}
	normalized, err := values.TranslateDeclaredEdgeRoot(value, edge, target)
	if err != nil {
		return nil, err
	}
	selectedChild := selectedPlanFromQuantifier(inputQ)
	producer := selectedChild.GetResultValue()
	if retained, ok := descendantRetainedResultProducer(selectedChild); ok {
		producer = retained
	}
	for correlation := range values.GetCorrelatedToOfValue(producer) {
		if correlation.IsZero() || correlation == values.CurrentCorrelation() {
			continue
		}
		normalized, err = values.TranslateLogicalSourceNameNormalizationInValue(
			normalized, correlation, producer)
		if err != nil {
			return nil, err
		}
	}
	normalized, err = values.ReanchorOwnedValueThroughProducer(
		normalized, producer, target, owned)
	if err != nil {
		return nil, err
	}
	return values.ReanchorValueForLayout(normalized, target, layout)
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The filter carries its child as a single frozen memo edge, so the
// relink checked-rebases child-edge predicate Values while preserving the
// distinct logical binding alias, and GetInner re-resolves through the new
// singleton reference.
// This replaces physicalPredicatesFilterWrapper.WithChildren (RFC-184 W2), whose
// separate snapshot plan field forced a constructor rebuild gated on
// isLeafReplaceable. Because the emitter already froze the concrete inner into a
// private single-member reference, extraction recurses through it faithfully — it
// never consults a shared exploratory group, so a correlated inner is preserved.
func (p *RecordQueryPredicatesFilterPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryPredicatesFilterPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
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
