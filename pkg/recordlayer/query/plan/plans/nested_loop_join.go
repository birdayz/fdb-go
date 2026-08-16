package plans

import (
	"bytes"
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func samePlanExactType(left, right values.Type) bool {
	leftHandle, leftErr := values.SnapshotExactType(left)
	rightHandle, rightErr := values.SnapshotExactType(right)
	return leftErr == nil && rightErr == nil &&
		bytes.Equal(leftHandle.CanonicalBytes(), rightHandle.CanonicalBytes())
}

// RecordQueryNestedLoopJoinPlan represents a nested-loop join of two
// child plans. For each row in the outer (left) plan, the inner (right)
// plan is evaluated and the join predicate is applied to the combined
// row. This is the simplest and most general join strategy — it handles
// all join types (inner, left, cross) without requiring ordered input.
//
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryFlatMapPlan`
// which is the underlying implementation of nested-loop joins in the
// Record Layer.
//
// The two legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQueryFlatMapPlan`'s outer/inner `Quantifier.Physical`). The raw
// `outer`/`inner` pointers they replace were a second storage location for the
// same edges. They stay two separately-named fields rather than a slice
// because the accessors and the join predicates address them by ROLE — the
// outer is the driving side — not by position. RFC-183 P5 step 2.
type RecordQueryNestedLoopJoinPlan struct {
	PlanExprBase
	outerQ      expressions.Quantifier
	innerQ      expressions.Quantifier
	predicates  []predicates.QueryPredicate
	joinType    JoinType
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
	resultValue values.Value
}

// JoinType distinguishes inner vs outer vs cross joins.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeftOuter
	JoinCross
	// Slots 3 and 4 were JoinExists / JoinNotExists, removed in RFC-141 Phase 2:
	// EXISTS is no longer a fused join mode — the existential semi-join is emergent
	// (FirstOrDefault-wrapped inner + a separate IS-NOT-NULL filter, matching Java).
	// The slots are kept blank so the subsequent iota values stay stable.
	_
	_
	// JoinFullOuter — FULL OUTER JOIN: every left row (matched or
	// NULL-padded right) plus every right row that matched no left row
	// (NULL-padded left). Go-only query extension; Java's SQL layer has
	// no outer joins. Appended (not inserted) to keep prior iota values
	// stable. Implemented only by the materialized nested-loop cursor,
	// never the correlated FlatMap path (which cannot observe global
	// inner-match state).
	JoinFullOuter
)

func (jt JoinType) String() string {
	switch jt {
	case JoinInner:
		return "INNER"
	case JoinLeftOuter:
		return "LEFT OUTER"
	case JoinCross:
		return "CROSS"
	case JoinFullOuter:
		return "FULL OUTER"
	}
	return "UNKNOWN"
}

// NewRecordQueryNestedLoopJoinPlan constructs a nested-loop join plan.
// outerAlias/innerAlias identify the two legs of the merged row this join emits:
// the executor qualifies merged-row keys by them and stamps them onto the row's
// leg boundaries, so they are what an alias-qualified column reference resolves
// through.
//
// They are CorrelationIdentifiers, not strings, because that is what they
// identify — a quantifier — and because the executor's leg boundaries compare
// them through values.SameLeg. Holding them as text meant the executor minted an
// identifier from a string at the plan boundary, and an exact comparison cannot
// protect against a forgery its own mint constructs: the mint decides the
// spelling, so the case-disjointness that keeps a quoted "Q$5" from binding a
// planner-minted q$5 was being re-decided at every consumer. Java holds the same
// thing typed end to end (RecordQueryFlatMapPlan carries Quantifier.Physical,
// not an alias string), and Go's own RecordQueryFlatMapPlan already did.
func NewRecordQueryNestedLoopJoinPlan(
	outer, inner RecordQueryPlan,
	joinPredicates []predicates.QueryPredicate,
	joinType JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
) (*RecordQueryNestedLoopJoinPlan, error) {
	return newRecordQueryNestedLoopJoinPlanFromQuantifiers(
		QuantifierOverPlan(outer), QuantifierOverPlan(inner), joinPredicates,
		joinType, outerAlias, innerAlias, resultValue)
}

// NewRecordQueryNestedLoopJoinPlanFromQuantifiers builds a nested-loop join whose
// two legs are supplied memo quantifiers instead of snapshots over concrete
// plans. This makes the plan its own cascades expression carrying its child
// edges directly — the memo holds it without a physicalNestedLoopJoinWrapper
// (RFC-184 W2). The materialized NLJ is uncorrelated (CanCorrelate=false), so
// both legs carry the LIVE shared-group edge the emitter memoized; the join
// predicates, join type, table aliases and result value are preserved so
// EqualsPlanWithoutChildren / GetCorrelatedToWithoutChildren stay identical.
func NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
	outerQ, innerQ expressions.Quantifier,
	joinPredicates []predicates.QueryPredicate,
	joinType JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
) (*RecordQueryNestedLoopJoinPlan, error) {
	return newRecordQueryNestedLoopJoinPlanFromQuantifiers(
		outerQ, innerQ, joinPredicates, joinType, outerAlias, innerAlias, resultValue)
}

func newRecordQueryNestedLoopJoinPlanFromQuantifiers(
	outerQ, innerQ expressions.Quantifier,
	joinPredicates []predicates.QueryPredicate,
	joinType JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
) (*RecordQueryNestedLoopJoinPlan, error) {
	var nullAliases []values.CorrelationIdentifier
	switch joinType {
	case JoinLeftOuter:
		nullAliases = []values.CorrelationIdentifier{innerAlias}
	case JoinFullOuter:
		nullAliases = []values.CorrelationIdentifier{outerAlias, innerAlias}
	}
	nullSupplying := make([]values.QuantifiedObjectValue, 0, len(nullAliases))
	for _, alias := range nullAliases {
		source, err := exactQOVForResultSource(alias, resultValue)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan null-supplying source: %w", err)
		}
		if source != nil {
			nullSupplying = append(nullSupplying, source)
		}
	}
	base, err := newPlanExprBaseForRetainedResult(
		"RecordQueryNestedLoopJoinPlan", resultValue, nullSupplying)
	if err != nil {
		return nil, err
	}
	base, err = nestedLoopJoinBaseWithRetainedScalarSources(
		base, outerQ, innerQ, outerAlias, innerAlias, joinType,
		resultValue, nullSupplying)
	if err != nil {
		return nil, err
	}
	preds := make([]predicates.QueryPredicate, len(joinPredicates))
	copy(preds, joinPredicates)
	return &RecordQueryNestedLoopJoinPlan{
		PlanExprBase: base,
		outerQ:       outerQ,
		innerQ:       innerQ,
		predicates:   preds,
		joinType:     joinType,
		outerAlias:   outerAlias,
		innerAlias:   innerAlias,
		resultValue:  resultValue,
	}, nil
}

// nestedLoopJoinBaseWithRetainedScalarSources preserves an exact scalar window
// copied out of a selected child materializer. A gathered join usually refers
// to the child through one record-valued physical edge, so the ordinary
// retained-result discovery sees only fields of that edge and loses a bare
// UNNEST element such as VAL. The proof below crosses both producer boundaries:
//
//	scalar child WindowSource -> exact child carrier -> NLJ leg binding
//	  -> unique NLJ result slot
//
// Only then is the scalar published as an ObjectPath on the NLJ output layout.
// An exploratory edge has no selected child authority and retains the ordinary
// base; extraction reconstructs the plan over pinned quantifiers and performs
// the proof then. The values-owned layout factory rejects duplicate/conflicting
// sources and invalid result paths atomically.
func nestedLoopJoinBaseWithRetainedScalarSources(
	base PlanExprBase,
	outerQ, innerQ expressions.Quantifier,
	outerAlias, innerAlias values.CorrelationIdentifier,
	joinType JoinType,
	resultValue values.Value,
	nullSupplying []values.QuantifiedObjectValue,
) (PlanExprBase, error) {
	baseLayout, err := base.ProvidedOutputLayout()
	if err != nil {
		return PlanExprBase{}, err
	}
	type selectedLeg struct {
		quantifier expressions.Quantifier
		alias      values.CorrelationIdentifier
	}
	legs := []selectedLeg{
		{quantifier: outerQ, alias: outerAlias},
		{quantifier: innerQ, alias: innerAlias},
	}
	var additional []values.OrdinalOutputSource
	for _, leg := range legs {
		if leg.alias.IsZero() {
			continue
		}
		child := selectedPlanFromQuantifier(leg.quantifier)
		if child == nil {
			continue
		}
		childLayout, layoutErr := child.ProvidedOutputLayout()
		if layoutErr != nil {
			return PlanExprBase{}, fmt.Errorf(
				"RecordQueryNestedLoopJoinPlan retained scalar child layout: %w", layoutErr)
		}
		materializer, ok := descendantValueMaterializer(child)
		if !ok {
			continue
		}
		for _, source := range childLayout.WindowSources() {
			if source == nil || source.FlowedType() == nil ||
				source.FlowedType().Code() == values.TypeCodeRecord {
				continue
			}
			childValue, reanchorErr := materializer.reanchorInputValueToOutput(source)
			if reanchorErr != nil || !valueReferencesExactQOV(childValue, childLayout.Carrier()) {
				continue
			}
			legBinding, bindingErr := values.NewQuantifiedObjectValue(
				leg.alias, values.PhysicalCarrierType(childLayout))
			if bindingErr != nil {
				return PlanExprBase{}, bindingErr
			}
			childValue, reanchorErr = values.TranslatePhaseRoot(
				childValue, childLayout.Carrier(), legBinding)
			if reanchorErr != nil {
				return PlanExprBase{}, fmt.Errorf(
					"RecordQueryNestedLoopJoinPlan retained scalar leg binding: %w", reanchorErr)
			}
			outputValue, reanchorErr := values.ReanchorValueThroughProducer(
				childValue, resultValue, baseLayout.Carrier())
			if reanchorErr != nil {
				return PlanExprBase{}, fmt.Errorf(
					"RecordQueryNestedLoopJoinPlan retained scalar output: %w", reanchorErr)
			}
			field, isField := values.AsFieldValue(outputValue)
			if !isField || field.ChildValue() != baseLayout.Carrier() ||
				field.Path() == nil || field.Path().Len() != 1 {
				continue
			}
			// The selected output slot must carry the exact source object. In
			// particular, a null-supplying join can widen the copied scalar slot;
			// that is no longer the same exact source and cannot publish the
			// child's non-null window. Decline the optimization instead of asking
			// the layout factory to reinterpret the produced value.
			if !samePlanExactType(field.ResultType(), source.FlowedType()) {
				continue
			}
			nullSupplyingSource := (joinType == JoinLeftOuter && leg.alias == innerAlias) ||
				joinType == JoinFullOuter
			if nullSupplyingSource {
				continue
			}
			additional = append(additional, values.OrdinalOutputSource{
				Source: source, ObjectPath: field.Path().Ordinals(),
			})
		}
	}
	if len(additional) == 0 {
		return base, nil
	}
	layout, err := values.NewFlatOrdinalLayoutForRetainedResultWithSources(
		resultValue, nullSupplying, additional)
	if err != nil {
		return PlanExprBase{}, fmt.Errorf(
			"RecordQueryNestedLoopJoinPlan retained scalar output layout: %w", err)
	}
	return newPlanExprBaseForProvidedLayout(
		"RecordQueryNestedLoopJoinPlan", resultValue, layout)
}

func (p *RecordQueryNestedLoopJoinPlan) GetResultType() values.Type { return p.resultValue.Type() }

// GetChildren returns the outer leg then the inner leg, dereferenced through
// the quantifiers. The pair is always two entries wide — a nil leg stays a nil
// entry rather than shrinking the arity.
func (p *RecordQueryNestedLoopJoinPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.GetOuter(), p.GetInner()}
}

// GetQuantifiers reports the real leg quantifiers in GetChildren order
// (outer, inner), overriding PlanExprBase's none. That order is what
// WithQuantifiers indexes into.
func (p *RecordQueryNestedLoopJoinPlan) GetQuantifiers() []expressions.Quantifier {
	if p.outerQ.GetRangesOver() == nil || p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.outerQ, p.innerQ}
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers, in
// GetQuantifiers order. The receiver is never mutated, which is what keeps a
// memoized plan safe to share.
func (p *RecordQueryNestedLoopJoinPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryNestedLoopJoinPlan", len(qs), 2); err != nil {
		return nil, err
	}
	if sameQuantifierEdge(qs[0], p.outerQ) && sameQuantifierEdge(qs[1], p.innerQ) {
		cp := *p
		cp.outerQ = qs[0]
		cp.innerQ = qs[1]
		return &cp, nil
	}
	return newRecordQueryNestedLoopJoinPlanFromQuantifiers(
		qs[0], qs[1], p.predicates, p.joinType,
		p.outerAlias, p.innerAlias, p.resultValue)
}

func sameQuantifierEdge(left, right expressions.Quantifier) bool {
	return left.GetAlias() == right.GetAlias() &&
		left.GetRangesOver() == right.GetRangesOver() &&
		left.Kind() == right.Kind() && left.IsNullOnEmpty() == right.IsNullOnEmpty()
}

func (p *RecordQueryNestedLoopJoinPlan) GetOuter() RecordQueryPlan {
	return planFromQuantifier(p.outerQ)
}

func (p *RecordQueryNestedLoopJoinPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}
func (p *RecordQueryNestedLoopJoinPlan) GetJoinType() JoinType { return p.joinType }
func (p *RecordQueryNestedLoopJoinPlan) GetOuterAlias() values.CorrelationIdentifier {
	return p.outerAlias
}

func (p *RecordQueryNestedLoopJoinPlan) GetInnerAlias() values.CorrelationIdentifier {
	return p.innerAlias
}

func (p *RecordQueryNestedLoopJoinPlan) GetResultValue() values.Value {
	return p.resultValue
}

func (p *RecordQueryNestedLoopJoinPlan) GetPredicates() []predicates.QueryPredicate {
	return p.predicates
}

// reanchorInputValueToOutput exposes this materializing join's retained result
// program as the checked source-to-carrier lineage used by an upper sort or
// projection. Logical table sources can differ from the physical retained
// windows only by their top-level record name; normalize that nominal detail at
// the declared leg alias, then let the exact producer program select the output
// ordinal. A foreign alias, narrower same-alias window, or structural drift
// remains untouched and is rejected by the final layout check.
func (p *RecordQueryNestedLoopJoinPlan) reanchorInputValueToOutput(value values.Value) (values.Value, error) {
	layout, err := p.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan output layout: %w", err)
	}
	// Extraction can ask the same selected materializer to relink an already
	// relinked program. Once every QOV leaf is rooted in this join's pointer-exact
	// output carrier, its ordinals are final: running the retained result program
	// again would reinterpret an output ordinal as a child-relative path. That is
	// wrong when two legs expose the same leaf name. For example, output P.K#1 can
	// be rematched to Q.K#4 merely because the result program has one one-step K.
	//
	// Pointer identity is the authority here. A separately minted current row,
	// even with the same exact type, must continue through the ordinary checked
	// child/producer path. ReanchorValueForLayout validates the admitted paths and
	// carrier contract while remaining copy-on-write.
	if valueReferencesOnlyExactQOV(value, layout.Carrier()) {
		reanchored, reanchorErr := values.ReanchorValueForLayout(
			value, layout.Carrier(), layout)
		if reanchorErr != nil {
			return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan exact output program: %w", reanchorErr)
		}
		return reanchored, nil
	}
	if p.hasRetainedChildMaterializer() &&
		!p.retainedChildrenDeclareBuriedCorrelations(value) {
		return value, nil
	}
	normalized := value
	for _, alias := range []values.CorrelationIdentifier{p.outerAlias, p.innerAlias} {
		if alias.IsZero() {
			continue
		}
		normalized, err = values.TranslateLogicalSourceNameNormalizationInValue(
			normalized, alias, p.resultValue)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan source %s normalization: %w", alias, err)
		}
	}
	normalized, err = p.reanchorRetainedChildValue(normalized)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan child lineage: %w", err)
	}
	normalized, err = values.ReanchorValueThroughProducer(
		normalized, p.resultValue, layout.Carrier())
	if err != nil {
		return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan result lineage: %w", err)
	}
	normalized, err = values.ReanchorValueForLayout(normalized, layout.Carrier(), layout)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan output carrier: %w", err)
	}
	return normalized, nil
}

// valueReferencesOnlyExactQOV reports whether value contains at least one QOV
// leaf and every such leaf is the same pointer-exact physical carrier. It is
// deliberately stronger than valueReferencesExactQOV: a mixed program that
// also reads an outer/foreign binding has not completed producer relinking and
// cannot take an idempotent materializer fast path.
func valueReferencesOnlyExactQOV(value values.Value, target values.QuantifiedObjectValue) bool {
	if value == nil || target == nil {
		return false
	}
	found := false
	onlyTarget := true
	values.WalkValue(value, func(node values.Value) bool {
		root, ok := values.AsQuantifiedObjectValue(node)
		if !ok {
			return true
		}
		found = true
		if root != target {
			onlyTarget = false
			return false
		}
		return true
	})
	return found && onlyTarget
}

// reanchorRetainedChildValue crosses exactly one selected child materializer
// before the join's own flattened result program is consulted. A gathered join
// commonly retains a FlatMap as one leg; values such as an UNNEST element X or
// a table field buried in that FlatMap are not direct leaves of the join result
// program. The child is the authority that can first map them onto its exact
// output carrier. That carrier is then translated to the join's declared
// runtime leg binding, which is the root used by p.resultValue.
//
// Buried values are probed against both children and a rewrite is accepted only
// when exactly one child proves ownership by returning a value rooted in its
// exact carrier. A direct top-level leg binding probes only that leg; two
// claiming children are ambiguous and leave the request untouched. The final
// layout/binding validation remains fail-closed.
func (p *RecordQueryNestedLoopJoinPlan) reanchorRetainedChildValue(value values.Value) (values.Value, error) {
	type childLeg struct {
		plan  RecordQueryPlan
		alias values.CorrelationIdentifier
	}
	legs := []childLeg{
		{plan: p.GetOuter(), alias: p.outerAlias},
		{plan: p.GetInner(), alias: p.innerAlias},
	}
	directCorrelations := values.GetCorrelatedToOfValue(value)
	_, directlyOwnedByOuter := directCorrelations[p.outerAlias]
	_, directlyOwnedByInner := directCorrelations[p.innerAlias]

	var selected values.Value
	for _, leg := range legs {
		if leg.plan == nil || leg.alias.IsZero() {
			continue
		}
		// A direct top-level leg binding is stronger ownership evidence than a
		// retained descendant's generic producer fallback. In a chained join the
		// outer child can expose D.NAME as its only NAME while the upper inner leg
		// exposes P.NAME. Probing P.NAME through that outer child would let the
		// unique-name fallback claim D.NAME before this join's own result program
		// can select the exact P-owned output slot. Buried sources name neither
		// top-level binding and continue to probe both children; a value genuinely
		// spanning both direct legs also keeps the existing ambiguity handling.
		if directlyOwnedByOuter != directlyOwnedByInner &&
			(leg.alias == p.outerAlias) != directlyOwnedByOuter {
			continue
		}
		materializer, ok := descendantValueMaterializer(leg.plan)
		if !ok {
			continue
		}
		if !legDeclaresBuriedCorrelations(
			leg.plan, value, p.outerAlias, p.innerAlias) {
			continue
		}
		candidate, candidateErr := materializer.reanchorInputValueToOutput(value)
		if candidateErr != nil {
			// This is an ownership probe. An unrelated child can reject a
			// same-spelled source; that is not authority to reject a value which
			// the other child may own.
			continue
		}
		childLayout, layoutErr := leg.plan.ProvidedOutputLayout()
		if layoutErr != nil {
			return nil, layoutErr
		}
		childCarrier := childLayout.Carrier()
		if childCarrier == nil || !valueReferencesExactQOV(candidate, childCarrier) {
			continue
		}
		binding, bindingErr := values.NewQuantifiedObjectValue(
			leg.alias, childCarrier.FlowedType())
		if bindingErr != nil {
			return nil, bindingErr
		}
		candidate, candidateErr = values.TranslatePhaseRoot(
			candidate, childCarrier, binding)
		if candidateErr != nil {
			return nil, candidateErr
		}
		if selected != nil {
			return value, nil
		}
		selected = candidate
	}
	if selected == nil {
		return value, nil
	}
	return selected, nil
}

func (p *RecordQueryNestedLoopJoinPlan) hasRetainedChildMaterializer() bool {
	for _, child := range []RecordQueryPlan{p.GetOuter(), p.GetInner()} {
		if _, ok := descendantValueMaterializer(child); ok {
			return true
		}
	}
	return false
}

func (p *RecordQueryNestedLoopJoinPlan) retainedChildrenDeclareBuriedCorrelations(value values.Value) bool {
	for correlation := range values.GetCorrelatedToOfValue(value) {
		if correlation == values.CurrentCorrelation() ||
			correlation == p.outerAlias || correlation == p.innerAlias {
			continue
		}
		if !planDeclaresRuntimeAlias(p.GetOuter(), correlation) &&
			!planDeclaresRuntimeAlias(p.GetInner(), correlation) {
			return false
		}
	}
	return true
}

func legDeclaresBuriedCorrelations(
	plan RecordQueryPlan,
	value values.Value,
	outerAlias, innerAlias values.CorrelationIdentifier,
) bool {
	for correlation := range values.GetCorrelatedToOfValue(value) {
		if correlation == values.CurrentCorrelation() ||
			correlation == outerAlias || correlation == innerAlias {
			continue
		}
		if !planDeclaresRuntimeAlias(plan, correlation) {
			return false
		}
	}
	return true
}

func planDeclaresRuntimeAlias(plan RecordQueryPlan, correlation values.CorrelationIdentifier) bool {
	if plan == nil || correlation.IsZero() {
		return false
	}
	if binary, ok := plan.(interface {
		GetOuterAlias() values.CorrelationIdentifier
		GetInnerAlias() values.CorrelationIdentifier
	}); ok && (binary.GetOuterAlias() == correlation || binary.GetInnerAlias() == correlation) {
		return true
	}
	if unary, ok := plan.(interface {
		GetInnerAlias() values.CorrelationIdentifier
	}); ok && unary.GetInnerAlias() == correlation {
		return true
	}
	for _, child := range plan.GetChildren() {
		if planDeclaresRuntimeAlias(child, correlation) {
			return true
		}
	}
	return false
}

// structuralKey lists the fields that distinguish this join in the memo: the
// join type, the outer/inner leg identifiers, the join predicate list, the
// result Value, and the provided physical output layout. Children (the two
// legs) are excluded. Raw plan equality keeps exact leg identifiers;
// expression equality maps them through the correspondence established by
// matching those children, including aliases inside predicates, result Values,
// and retained-source windows. Hashing omits alias spellings and uses the
// layout's alias-free hash, so alpha-equivalent joins enter the same bucket.
func (p *RecordQueryNestedLoopJoinPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Int(int(p.joinType)).
		MappedAlias(p.outerAlias).
		MappedAlias(p.innerAlias).
		Preds(p.predicates).
		Value(p.resultValue).
		Layout(p.admittedProvidedOutputLayout())
}

func (p *RecordQueryNestedLoopJoinPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryNestedLoopJoinPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren folds the structural discriminators. Predicates
// fold predicates.SemanticHashCode (alias-invariant, coarser than the
// structural PredicateEquals — equal⟹same-hash holds), NOT Explain()
// display text, which is for humans and carries no identity contract. The
// resultValue joins identity — the Java counterpart (RecordQueryFlatMapPlan)
// compares via semanticEqualsForResults; two joins differing only in the
// combined-row shape they emit are not interchangeable.
func (p *RecordQueryNestedLoopJoinPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("nljoin|")
}

func (p *RecordQueryNestedLoopJoinPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString("NestedLoopJoin(")
	sb.WriteString(p.joinType.String())
	if len(p.predicates) > 0 {
		sb.WriteString(fmt.Sprintf(", [%d preds]", len(p.predicates)))
	}
	sb.WriteString(", ")
	if outer := p.GetOuter(); outer != nil {
		sb.WriteString(outer.Explain())
	}
	sb.WriteString(", ")
	if inner := p.GetInner(); inner != nil {
		sb.WriteString(inner.Explain())
	}
	sb.WriteString(")")
	return sb.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryNestedLoopJoinPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryNestedLoopJoinPlan)(nil)
)

// EqualsWithoutChildren compares child-local aliases, predicates, the result
// program and its physical output layout through the memo's alpha-renaming.
func (p *RecordQueryNestedLoopJoinPlan) EqualsWithoutChildren(other expressions.RelationalExpression, aliases *expressions.AliasMap) bool {
	o, ok := other.(*RecordQueryNestedLoopJoinPlan)
	return ok && p.structuralKey().EqualUnderAliases(o.structuralKey(), aliases.ToValuesAliasMap())
}

// GetCorrelatedToWithoutChildren walks this plan's own predicates, mirroring
// physicalNestedLoopJoinWrapper. The predicates are this node's information — a
// correlation reached only through them would be invisible to
// correlation-driven rules if this returned the empty default.
func (p *RecordQueryNestedLoopJoinPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, pred := range p.GetPredicates() {
		for k := range predicates.GetCorrelatedToOfPredicate(pred) {
			out[k] = struct{}{}
		}
	}
	return out
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The join carries its two legs as memo quantifiers, so the relink
// is a positional quantifier swap: WithQuantifiers copies the receiver
// (preserving the predicates, join type, table aliases and result value) and
// re-resolves GetOuter/GetInner through the new references. This replaces
// physicalNestedLoopJoinWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field held the yield-time children verbatim; the swap
// re-resolves to the memo winner instead.
func (p *RecordQueryNestedLoopJoinPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan.WithChildren: expected 2 children, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryNestedLoopJoinPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
