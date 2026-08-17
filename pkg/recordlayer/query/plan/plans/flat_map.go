package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFlatMapPlan represents a correlated nested-loop join where
// for each outer row, the inner plan is re-executed with the outer row
// bound as a correlation. Mirrors Java's RecordQueryFlatMapPlan which
// uses FlatMapPipelinedCursor for execution.
//
// The key difference from RecordQueryNestedLoopJoinPlan: the inner plan
// is parameterized by the outer row via correlation bindings. This
// enables targeted index probes on the inner side (O(N×logM) vs O(N×M)).
// LEFT-OUTER note: the plan carries NO leftOuter flag. LEFT-OUTER semantics are
// emergent from the inner being wrapped in DefaultOnEmpty (whose OrElse
// continuation makes the null-extension resume-safe), exactly like Java's
// RecordQueryFlatMapPlan — see rule_implement_nested_loop_join.go's lowering. An
// earlier in-memory leftOuter/innerHadMatch flag pair re-decided the extension per
// page and was the F2 spurious-null resume bug; it was removed as dead code.
//
// The two legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQueryFlatMapPlan`'s `Quantifier.Physical outerQuantifier` /
// `innerQuantifier`). The raw `outer`/`inner` pointers they replace were a
// second storage location for the same edges. They stay two separately-named
// fields rather than a slice because the accessors, the Explain rendering and
// the executor all address them by ROLE, not by position. RFC-183 P5 step 2.
type RecordQueryFlatMapPlan struct {
	PlanExprBase
	outerQ                       expressions.Quantifier
	innerQ                       expressions.Quantifier
	outerAlias                   values.CorrelationIdentifier
	innerAlias                   values.CorrelationIdentifier
	resultValue                  values.Value
	inheritOuterRecordProperties bool
	nullSupplyingInner           bool
}

func NewRecordQueryFlatMapPlan(
	outer, inner RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	inheritOuterRecordProperties bool,
) (*RecordQueryFlatMapPlan, error) {
	return newRecordQueryFlatMapPlanFromQuantifiers(
		QuantifierOverPlan(outer), QuantifierOverPlan(inner),
		outerAlias, innerAlias, resultValue, inheritOuterRecordProperties, false)
}

// NewRecordQueryFlatMapPlanFromQuantifiers builds a correlated FlatMap whose two
// legs are supplied memo quantifiers instead of snapshots over concrete plans.
// This makes the plan its own cascades expression carrying its child edges
// directly — the memo holds it without a physicalFlatMapWrapper (RFC-184 W2).
//
// The FlatMap is the correlation-BINDING operator (CanCorrelate=true): the outer
// leg binds the value the correlated inner leg re-reads per outer row. The
// emitter has already decided each leg's edge — a plain/self-contained outer
// carries the LIVE shared-group edge, while the SARG-pushed/correlated inner is
// frozen in a detached single-member final reference upstream (the FirstOrDefault
// / predicates-filter disentangle) — so this constructor is a pure unwrap that
// carries those edges verbatim. The outer/inner aliases and the result value are
// preserved so GetResultValue and the correlation propagation stay identical.
func NewRecordQueryFlatMapPlanFromQuantifiers(
	outerQ, innerQ expressions.Quantifier,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	inheritOuterRecordProperties bool,
) (*RecordQueryFlatMapPlan, error) {
	return newRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, innerQ, outerAlias, innerAlias, resultValue,
		inheritOuterRecordProperties, false)
}

// NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner builds a
// FlatMap whose inner edge may be absent and therefore supplies SQL NULLs. The
// fact is explicit at lowering; plans never rediscover it by traversing an
// executor-specific wrapper spine.
func NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner(
	outerQ, innerQ expressions.Quantifier,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	inheritOuterRecordProperties bool,
) (*RecordQueryFlatMapPlan, error) {
	return newRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, innerQ, outerAlias, innerAlias, resultValue,
		inheritOuterRecordProperties, true)
}

func newRecordQueryFlatMapPlanFromQuantifiers(
	outerQ, innerQ expressions.Quantifier,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	inheritOuterRecordProperties bool,
	nullSupplyingInner bool,
) (*RecordQueryFlatMapPlan, error) {
	var nullSupplying []values.QuantifiedObjectValue
	if nullSupplyingInner {
		innerSource, err := exactQOVForResultSource(innerAlias, resultValue)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan null-supplying inner: %w", err)
		}
		if innerSource != nil {
			// Java's Quantifier.pullUpResultColumnsWithNullability(true):
			// QuantifiedObjectValue.of(alias, type.withNullability(true)). A leg
			// this FlatMap null-extends flows a row that can BE null, and the
			// layout refuses to publish a null-supplying window over a row typed
			// NOT NULL — correctly, since that pair states two different facts
			// about the same leg. The result program is rewritten in the same
			// step so the program and the window never disagree about which
			// exact row the alias names.
			resultValue, innerSource, err = pullUpNullSupplyingSource(resultValue, innerSource)
			if err != nil {
				return nil, fmt.Errorf("RecordQueryFlatMapPlan null-supplying inner: %w", err)
			}
			nullSupplying = []values.QuantifiedObjectValue{innerSource}
		}
	}
	base, err := newPlanExprBaseForRetainedResult(
		"RecordQueryFlatMapPlan", resultValue, nullSupplying)
	if err != nil {
		return nil, err
	}
	base, err = flatMapBaseWithRetainedSources(
		base, outerQ, innerQ, outerAlias, innerAlias,
		resultValue, nullSupplying, nullSupplyingInner)
	if err != nil {
		return nil, err
	}
	return &RecordQueryFlatMapPlan{
		PlanExprBase:                 base,
		outerQ:                       outerQ,
		innerQ:                       innerQ,
		outerAlias:                   outerAlias,
		innerAlias:                   innerAlias,
		resultValue:                  resultValue,
		inheritOuterRecordProperties: inheritOuterRecordProperties,
		nullSupplyingInner:           nullSupplyingInner,
	}, nil
}

// pullUpNullSupplyingSource retypes one leg source as nullable and rewrites
// resultValue onto it, returning both so the caller cannot install one without
// the other.
//
// An already-nullable source is returned untouched, by pointer, so this is a
// no-op on every leg whose producer already stated the fact.
func pullUpNullSupplyingSource(
	resultValue values.Value,
	source values.QuantifiedObjectValue,
) (values.Value, values.QuantifiedObjectValue, error) {
	if source == nil || source.FlowedType() == nil || source.FlowedType().IsNullable() {
		return resultValue, source, nil
	}
	nullable, err := values.NewQuantifiedObjectValue(
		source.Correlation(), values.WithNullability(source.FlowedType(), true))
	if err != nil {
		return nil, nil, err
	}
	rewritten, err := values.TranslateLogicalSourceRoot(resultValue, source, nullable)
	if err != nil {
		return nil, nil, err
	}
	return rewritten, nullable, nil
}

// flatMapBaseWithRetainedSources preserves an exact source which
// a selected child materializer has already packed into its current carrier.
// The ordinary retained-result layout sees only the FlatMap's direct leg
// bindings. In a chained/multiway shape, however, an earlier UNNEST source can
// be addressed through the selected outer child and copied into the new result
// without its original QOV appearing directly in that result program. This is
// true for both a scalar element and a record element retained as one output
// object. Losing either source makes an enclosing correlated EXISTS conflate
// the element alias with its complete outer-row binding.
//
// The proof crosses both selected producer boundaries, copy-on-write:
//
//	child WindowSource -> exact child carrier -> FlatMap leg binding
//	  -> unique FlatMap result slot
//
// Only a one-slot output path with the source's exact type is published. A
// null-supplying FlatMap leg and a source which is itself null-supplying both
// decline: this constructor does not synthesize or transport match-presence
// state. Exploratory quantifiers have no selected child and keep the ordinary
// base layout.
func flatMapBaseWithRetainedSources(
	base PlanExprBase,
	outerQ, innerQ expressions.Quantifier,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	nullSupplying []values.QuantifiedObjectValue,
	nullSupplyingInner bool,
) (PlanExprBase, error) {
	baseLayout, err := base.ProvidedOutputLayout()
	if err != nil {
		return PlanExprBase{}, err
	}
	type selectedLeg struct {
		quantifier    expressions.Quantifier
		alias         values.CorrelationIdentifier
		nullSupplying bool
	}
	legs := []selectedLeg{
		{quantifier: outerQ, alias: outerAlias},
		{quantifier: innerQ, alias: innerAlias, nullSupplying: nullSupplyingInner},
	}
	// A record-valued UNNEST can be retained directly as one RC output slot.
	// That is already a complete object proof; unlike a scalar ordinal seed it
	// does not necessarily make newPlanExprBaseForRetainedResult choose the
	// retained-layout factory. Remember the exact selected leg here so the
	// no-additional-source path below still publishes that direct object window.
	// Exploratory edges, null-supplying legs, and logical/physical type drift do
	// not qualify.
	directSelectedRecord := false
	if rc, isRC := resultValue.(*values.RecordConstructorValue); isRC && rc != nil {
		for _, field := range rc.Fields {
			source, isSource := values.AsQuantifiedObjectValue(field.Value)
			if !isSource || source.FlowedType() == nil || source.FlowedType().Code() != values.TypeCodeRecord {
				continue
			}
			for _, leg := range legs {
				if leg.nullSupplying || leg.alias != source.Correlation() ||
					selectedPlanFromQuantifier(leg.quantifier) == nil {
					continue
				}
				selectedSource, sourceErr := leg.quantifier.RequireFlowedObjectValue()
				if sourceErr != nil {
					return PlanExprBase{}, fmt.Errorf(
						"RecordQueryFlatMapPlan retained direct source: %w", sourceErr)
				}
				if samePlanExactType(selectedSource.FlowedType(), source.FlowedType()) {
					directSelectedRecord = true
				}
			}
		}
	}
	// Hoisted out of the leg loop: the set is a property of the result program,
	// which does not change per leg.
	ownedByResult := producerOwnedCorrelations(resultValue)
	var additional []values.OrdinalOutputSource
	for _, leg := range legs {
		if leg.alias.IsZero() || leg.nullSupplying {
			continue
		}
		child := selectedPlanFromQuantifier(leg.quantifier)
		if child == nil {
			continue
		}
		childLayout, layoutErr := child.ProvidedOutputLayout()
		if layoutErr != nil {
			// Retained-source propagation is an optional proof layered on top of
			// ordinary FlatMap construction. Diagnostic/legacy physical fixtures
			// can deliberately carry a selected child whose plan base cannot state
			// an ordinal layout; such a child supplied no source authority before
			// this helper existed and must keep the ordinary base rather than make
			// the enclosing FlatMap newly unconstructable.
			continue
		}
		materializer, ok := descendantValueMaterializer(child)
		if !ok {
			continue
		}
		for _, source := range childLayout.WindowSources() {
			if source == nil || source.FlowedType() == nil {
				continue
			}
			childNullSupplying, nullErr := values.LayoutWindowNullSupplying(childLayout, source)
			if nullErr != nil {
				return PlanExprBase{}, fmt.Errorf(
					"RecordQueryFlatMapPlan retained source presence: %w", nullErr)
			}
			if childNullSupplying {
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
					"RecordQueryFlatMapPlan retained source leg binding: %w", reanchorErr)
			}
			var outputValue values.Value
			if resultRoot, identityResult := values.AsQuantifiedObjectValue(resultValue); identityResult {
				// A bare QOV result is an explicit identity edge, not a
				// RecordConstructor producer. ReanchorValueThroughProducer has no
				// fields to inspect in that case and correctly leaves childValue on
				// the declared FlatMap binding. Translate that exact declaration to
				// the admitted output carrier directly so retained scalar windows
				// survive an identity existential wrapper. Correlation + exact type
				// admission prevents a same-spelled scalar source from being
				// rewritten as the whole row.
				outputValue, reanchorErr = values.TranslateDeclaredEdgeRoot(
					childValue, resultRoot, baseLayout.Carrier())
			} else {
				outputValue, reanchorErr = values.ReanchorOwnedValueThroughProducer(
					childValue, resultValue, baseLayout.Carrier(), ownedByResult)
			}
			if reanchorErr != nil {
				return PlanExprBase{}, fmt.Errorf(
					"RecordQueryFlatMapPlan retained source output: %w", reanchorErr)
			}
			field, isField := values.AsFieldValue(outputValue)
			if !isField || field.ChildValue() != baseLayout.Carrier() ||
				field.Path() == nil || field.Path().Len() != 1 ||
				!samePlanExactType(field.ResultType(), source.FlowedType()) {
				continue
			}
			additional = append(additional, values.OrdinalOutputSource{
				Source: source, ObjectPath: field.Path().Ordinals(),
			})
		}
	}
	var directResultLayout values.OrdinalLayout
	if _, identityResult := values.AsQuantifiedObjectValue(resultValue); !identityResult &&
		(len(additional) != 0 || directSelectedRecord) {
		// Direct result-program windows are the nearest producer authority. An
		// optional child-proven source with the same correlation cannot be added
		// to the same OrdinalLayout: layouts deliberately identify windows by
		// exact correlation and reject two different object types under one name.
		// Keep the direct window in that case. Existential lowering can still
		// separate a whole-row/retained-source collision before constructing its
		// identity FlatMap; a materializing RC cannot publish both objects itself.
		//
		// Discover direct windows through the values-owned factory after candidate
		// collection. The ordinary base may be an identity layout when the RC has
		// no baked ordinal, and a top-level field-mode source would otherwise be
		// invisible until the final factory call where it conflicts too late.
		directLayout, directErr := values.NewFlatOrdinalLayoutForRetainedResult(
			resultValue, nullSupplying)
		if directErr != nil {
			return PlanExprBase{}, fmt.Errorf(
				"RecordQueryFlatMapPlan direct retained-source layout: %w", directErr)
		}
		directResultLayout = directLayout
		directResultCorrelations := make(map[values.CorrelationIdentifier]struct{})
		for _, source := range directLayout.WindowSources() {
			if source != nil && !source.Correlation().IsZero() {
				directResultCorrelations[source.Correlation()] = struct{}{}
			}
		}
		filtered := additional[:0]
		for _, candidate := range additional {
			if _, direct := directResultCorrelations[candidate.Source.Correlation()]; direct {
				continue
			}
			filtered = append(filtered, candidate)
		}
		additional = filtered
	}
	if len(additional) == 0 && !directSelectedRecord {
		if directResultLayout != nil && len(directResultLayout.WindowSources()) != 0 {
			return newPlanExprBaseForProvidedLayout(
				"RecordQueryFlatMapPlan", resultValue, directResultLayout)
		}
		return base, nil
	}
	var layout values.OrdinalLayout
	if _, identityResult := values.AsQuantifiedObjectValue(resultValue); identityResult {
		// A bare record-QOV result uses the ordinary identity carrier rather than
		// a RecordConstructorValue. Preserve that carrier and add only the
		// producer-proven one-slot windows. The generic retained-result factory
		// intentionally requires a record constructor because it discovers
		// sources by inspecting output fields; here every source and ObjectPath
		// has already crossed the selected child and exact declared-edge proofs
		// above.
		if len(baseLayout.WindowSources()) != 0 {
			return PlanExprBase{}, fmt.Errorf(
				"RecordQueryFlatMapPlan retained identity layout already publishes source windows")
		}
		recordType, isRecord := values.PhysicalCarrierType(baseLayout).(*values.RecordType)
		if !isRecord || recordType == nil {
			return PlanExprBase{}, fmt.Errorf(
				"RecordQueryFlatMapPlan retained identity result is not an exact record")
		}
		var tiles []values.OrdinalTileSpec
		if width := len(recordType.Fields); width > 0 {
			tiles = []values.OrdinalTileSpec{{
				Start: 0,
				Width: width,
				Kind:  values.OrdinalTileFlat,
			}}
		}
		windows := make([]values.OrdinalWindowSpec, len(additional))
		for i := range additional {
			windows[i] = values.OrdinalWindowSpec{
				Source:        additional[i].Source,
				ObjectPath:    append([]int(nil), additional[i].ObjectPath...),
				NullSupplying: additional[i].NullSupplying,
			}
		}
		layout, err = values.NewOrdinalLayout(baseLayout.Carrier(), tiles, windows)
	} else {
		layout, err = values.NewFlatOrdinalLayoutForRetainedResultWithSources(
			resultValue, nullSupplying, additional)
	}
	if err != nil {
		return PlanExprBase{}, fmt.Errorf(
			"RecordQueryFlatMapPlan retained source output layout: %w", err)
	}
	return newPlanExprBaseForProvidedLayout(
		"RecordQueryFlatMapPlan", resultValue, layout)
}

func (p *RecordQueryFlatMapPlan) GetResultType() values.Type { return p.resultValue.Type() }

// GetChildren returns the outer leg then the inner leg, dereferenced through
// the quantifiers. The pair is always two entries wide — a nil leg stays a nil
// entry rather than shrinking the arity, which is what the executor's
// positional child handling expects.
func (p *RecordQueryFlatMapPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.GetOuter(), p.GetInner()}
}

// GetQuantifiers reports the real leg quantifiers in GetChildren order
// (outer, inner), overriding PlanExprBase's none. That order is what
// WithQuantifiers indexes into.
func (p *RecordQueryFlatMapPlan) GetQuantifiers() []expressions.Quantifier {
	if p.outerQ.GetRangesOver() == nil || p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.outerQ, p.innerQ}
}

// WithQuantifiers atomically rebuilds the FlatMap over the replacement legs.
// outerAlias and innerAlias are the runtime binding identities; they therefore
// stay fixed even though extraction gives each child edge a fresh quantifier
// alias. The retained result program is re-resolved at those logical aliases
// against the selected children's exact carrier types. An alias-only shallow
// copy would retain a logical/previous-phase root type that the executor cannot
// bind to the selected physical row.
func (p *RecordQueryFlatMapPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryFlatMapPlan", len(qs), 2); err != nil {
		return nil, err
	}
	oldOuter, err := p.outerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithQuantifiers old outer input: %w", err)
	}
	oldInner, err := p.innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithQuantifiers old inner input: %w", err)
	}
	newOuter, err := qs[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithQuantifiers new outer input: %w", err)
	}
	newInner, err := qs[1].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithQuantifiers new inner input: %w", err)
	}
	if !oldOuter.FlowedType().Equals(newOuter.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryFlatMapPlan.WithQuantifiers outer input type changed from %s to %s",
			oldOuter.FlowedType(), newOuter.FlowedType())
	}
	if !oldInner.FlowedType().Equals(newInner.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryFlatMapPlan.WithQuantifiers inner input type changed from %s to %s",
			oldInner.FlowedType(), newInner.FlowedType())
	}

	// The PHYSICAL type of each new input, not the public one. The relink builds
	// a fresh QOV, and NewQuantifiedObjectValue snapshots its layout from the
	// type it is handed — so relinking through FlowedType (which withholds leg
	// boundaries by design) rebinds the result value to a row that has forgotten
	// where its legs start. The type-equality checks above are unaffected: legs
	// are not part of exact-type identity.
	relinked := p.resultValue
	relinked, err = relinkFlatMapResultSource(relinked, p.outerAlias, physicalSourceType(newOuter))
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithQuantifiers outer result source: %w", err)
	}
	relinked, err = relinkFlatMapResultSource(relinked, p.innerAlias, physicalSourceType(newInner))
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithQuantifiers inner result source: %w", err)
	}
	return newRecordQueryFlatMapPlanFromQuantifiers(
		qs[0], qs[1], p.outerAlias, p.innerAlias, relinked,
		p.inheritOuterRecordProperties, p.nullSupplyingInner)
}

func relinkFlatMapResultSource(
	resultValue values.Value,
	bindingAlias values.CorrelationIdentifier,
	selectedType values.Type,
) (values.Value, error) {
	declaration, err := exactQOVForResultSource(bindingAlias, resultValue)
	if err != nil {
		return nil, err
	}
	if declaration == nil {
		return resultValue, nil
	}
	// The selected child carrier states the physical row shape, while the
	// FlatMap result declaration can additionally state whole-object absence.
	// StrictFirstOrDefault is the important case: its positional carrier is a
	// non-null shell, but a result program that retains the inner whole row must
	// keep that source nullable because an empty inner is SQL NULL. Extraction
	// replaces the child quantifier without changing this semantic contract.
	//
	// Preserve only the declaration's top-level nullability. Every field path,
	// leaf type, nested nullability, and physical record name still comes from
	// the selected child; field-bearing uses are revalidated by
	// TranslateLogicalSourceRoot, while the replacement quantifier equality
	// checks above reject child structural drift. This nullability adjustment
	// does not authorize any other change.
	targetType := values.WithNullability(selectedType, declaration.FlowedType().IsNullable())
	target, err := values.NewQuantifiedObjectValue(bindingAlias, targetType)
	if err != nil {
		return nil, err
	}
	return values.TranslateLogicalSourceRoot(resultValue, declaration, target)
}

func (p *RecordQueryFlatMapPlan) GetOuter() RecordQueryPlan { return planFromQuantifier(p.outerQ) }

func (p *RecordQueryFlatMapPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

func (p *RecordQueryFlatMapPlan) GetOuterAlias() values.CorrelationIdentifier { return p.outerAlias }

func (p *RecordQueryFlatMapPlan) GetInnerAlias() values.CorrelationIdentifier { return p.innerAlias }

func (p *RecordQueryFlatMapPlan) GetResultValue() values.Value { return p.resultValue }

func (p *RecordQueryFlatMapPlan) InheritOuterRecordProperties() bool {
	return p.inheritOuterRecordProperties
}

// reanchorInputValueToOutput exposes the FlatMap's retained result program as
// the exact lineage from its leg bindings to its produced row. A FlatMap with
// an RC result is a row producer just like a materializing sort's input: after
// the cursor evaluates that RC, parents must address the resulting current row,
// not re-evaluate an old leg-relative field against whichever binding happens
// to share its ordinal.
func (p *RecordQueryFlatMapPlan) reanchorInputValueToOutput(value values.Value) (values.Value, error) {
	layout, err := p.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan output layout: %w", err)
	}
	reanchored := value
	outer := p.GetOuter()
	inner := p.GetInner()
	// A logical table source can retain its nominal record name (T1) while the
	// selected physical source window is the same exact row with an anonymous
	// top-level name. Normalize at the FlatMap's declared leg alias before
	// traversing either producer. Keeping the alias here is load-bearing: the
	// nested producer result uses it to distinguish T1.N.SK from another leg's
	// namesake field. The ordinary name-only bridge stays fail-closed for narrower
	// windows and all structural/nullability drift.
	if p.outerAlias != (values.CorrelationIdentifier{}) && outer != nil {
		outerLayout, layoutErr := outer.ProvidedOutputLayout()
		if layoutErr != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan outer layout: %w", layoutErr)
		}
		reanchored, err = values.TranslateLogicalSourceNameNormalizationToCorrelation(
			reanchored, p.outerAlias, values.PhysicalCarrierType(outerLayout))
		if err != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan outer source normalization: %w", err)
		}
	}
	if p.innerAlias != (values.CorrelationIdentifier{}) && inner != nil {
		innerLayout, layoutErr := inner.ProvidedOutputLayout()
		if layoutErr != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan inner layout: %w", layoutErr)
		}
		reanchored, err = values.TranslateLogicalSourceNameNormalizationToCorrelation(
			reanchored, p.innerAlias, values.PhysicalCarrierType(innerLayout))
		if err != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan inner source normalization: %w", err)
		}
	}
	// A chained FlatMap's outer may itself be a positional merge. Its exact
	// layout retains the buried leg identities as whole-object windows; map
	// those before matching the upper FlatMap's flattened result fields. The
	// ordering is load-bearing: matching S.NAME directly against the upper RC
	// sees R.NAME as the only one-step NAME and would use the generic unique-name
	// fallback before S's nested ownership is recovered.
	//
	_, currentRooted := values.GetCorrelatedToOfValue(reanchored)[values.CurrentCorrelation()]
	traverseOuter := !currentRooted
	traverseInner := !currentRooted
	if !currentRooted {
		correlations := values.GetCorrelatedToOfValue(reanchored)
		_, ownedByOuter := correlations[p.outerAlias]
		_, ownedByInner := correlations[p.innerAlias]
		// A declared FlatMap binding is direct lineage authority. Do not send an
		// inner-owned value through the outer producer (or vice versa): a projection
		// on the wrong leg can have a uniquely named field and would otherwise claim
		// it through ReanchorValueThroughProducer's intentional unique-slot fallback.
		// Values owned by both legs still traverse both; a buried descendant that
		// names neither direct binding also probes both so a nested producer can
		// recover its source window.
		if ownedByOuter != ownedByInner {
			traverseOuter = ownedByOuter
			traverseInner = ownedByInner
		}
	}
	// Reserved current names a physical row phase, not every current-shaped
	// row in the subtree. A parent Value can legitimately arrive on one exact
	// child carrier (for example the two-column outer projection of a four-column
	// FlatMap). Cross only that matching child's lineage; another current-rooted
	// leg with overlapping field names is not authority to rewrite it. The
	// FlatMap result producer below then maps the child-current field onto this
	// producer's current carrier.
	if currentRooted {
		if outer != nil {
			outerLayout, layoutErr := outer.ProvidedOutputLayout()
			if layoutErr != nil {
				return nil, fmt.Errorf("RecordQueryFlatMapPlan outer layout: %w", layoutErr)
			}
			traverseOuter = valueReferencesExactQOV(reanchored, outerLayout.Carrier())
		}
		if inner != nil {
			innerLayout, layoutErr := inner.ProvidedOutputLayout()
			if layoutErr != nil {
				return nil, fmt.Errorf("RecordQueryFlatMapPlan inner layout: %w", layoutErr)
			}
			traverseInner = valueReferencesExactQOV(reanchored, innerLayout.Carrier())
		}
	}
	if traverseOuter {
		if outer != nil {
			if outerMaterializer, ok := descendantValueMaterializer(outer); ok {
				reanchored, err = outerMaterializer.reanchorInputValueToOutput(reanchored)
				if _, materializesResult := p.resultValue.(*values.RecordConstructorValue); err == nil && materializesResult {
					reanchored, err = translateFlatMapChildOutputToBinding(
						reanchored, outer, p.outerAlias)
				}
			} else {
				outerLayout, layoutErr := outer.ProvidedOutputLayout()
				if layoutErr != nil {
					return nil, fmt.Errorf("RecordQueryFlatMapPlan outer layout: %w", layoutErr)
				}
				// A non-materializing join still publishes a retained result
				// program that maps its named source windows to output ordinals.
				// Normalize through that producer before asking the layout to bind
				// the result. The layout is exact and deliberately rejects a
				// logical anonymous O row against its named ORDERS source window;
				// the producer RC is the checked authority that proves O.ID is
				// output ordinal 0. Without this step an identity FlatMap and a
				// materializing sort discard O's source window while a parent keeps
				// the now-unbindable O root.
				// The ownership set comes from the CHILD's result program, which is
				// the producer here. This FlatMap's own outer edge is deliberately
				// not in it: that edge denotes the child's finished OUTPUT row, and
				// crossing it is translateFlatMapChildOutputToBinding's job below.
				reanchored, err = values.ReanchorOwnedValueThroughProducer(
					reanchored, outer.GetResultValue(), outerLayout.Carrier(),
					producerOwnedCorrelations(outer.GetResultValue()))
				if err != nil {
					return nil, fmt.Errorf("RecordQueryFlatMapPlan outer producer lineage: %w", err)
				}
				reanchored, err = values.ReanchorValueForLayout(
					reanchored, outerLayout.Carrier(), outerLayout)
				// Both lineage branches end on the CHILD's private current
				// carrier, so both owe the same crossing back to the binding the
				// result program addresses that row through. Only the
				// materializer branch used to pay it, and the omission is
				// invisible until the two legs have the SAME exact row: the
				// child-current field then name-matches BOTH legs' slots in the
				// result RC, the producer bridge declines rather than guess, and
				// the value arrives at the output layout still rooted on a
				// one-leg row it cannot address. Measured on
				// `SELECT x.id FROM (SELECT id FROM t) x JOIN (SELECT id FROM t) y
				// ON x.id = y.id`: root RECORD(ID) against target RECORD(ID,ID).
				if _, materializesResult := p.resultValue.(*values.RecordConstructorValue); err == nil && materializesResult {
					reanchored, err = translateFlatMapChildOutputToBinding(
						reanchored, outer, p.outerAlias)
				}
			}
			if err != nil {
				return nil, fmt.Errorf("RecordQueryFlatMapPlan outer lineage: %w", err)
			}
			// traverseInner was decided BEFORE this crossing, against the value
			// as it arrived. The crossing can re-root it onto the outer leg's
			// own row, and a value that now names only that row has been
			// ANSWERED — handing it to the inner leg's lineage asks that
			// materializer to map a foreign current row, which is a hard error
			// rather than a decline:
			//
			//   RecordQueryFlatMapPlan inner lineage: ... reanchor.field:
			//   current root and target have different exact types:
			//   root RECORD(ID,COL1,ID,COL1), target RECORD(ID,T1_ID,ID,T2_ID)
			//
			// on `SELECT t1.id FROM t1 JOIN t1 AS x ON x.id = t1.id
			//     WHERE EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)`,
			// where the outer join's row was offered to the EXISTS leg's join.
			// Re-ask the question the value can now answer. A value that names
			// the inner leg too still traverses both, and one the outer leg
			// could not claim is untouched here and still traverses the inner.
			if traverseInner && inner != nil && valueAnsweredByChild(reanchored, outer, p.outerAlias) {
				answeredByInner, innerErr := valueNamesChild(reanchored, inner, p.innerAlias)
				if innerErr != nil {
					return nil, fmt.Errorf("RecordQueryFlatMapPlan inner layout: %w", innerErr)
				}
				traverseInner = answeredByInner
			}
		}
	}
	// The inner can itself be a positional producer. Multiway join lowering
	// commonly chooses exactly this orientation: an outer scan and an inner
	// FlatMap whose result retains two nested whole-object legs. A parent
	// aggregate can still read either buried inner leg (P.PRICE, I.QTY).
	// Crossing only the outer lineage leaves both roots behind, and the upper
	// flattened RC can no longer recover which nested inner slot supplied
	// them. Normalize through the inner producer before matching the upper
	// result program. Values owned by the outer leg are preserved fail-closed
	// by the inner materializer.
	if traverseInner {
		if innerMaterializer, ok := descendantValueMaterializer(inner); ok {
			reanchored, err = innerMaterializer.reanchorInputValueToOutput(reanchored)
			if _, materializesResult := p.resultValue.(*values.RecordConstructorValue); err == nil && materializesResult {
				reanchored, err = translateFlatMapChildOutputToBinding(
					reanchored, inner, p.innerAlias)
			}
			if err != nil {
				return nil, fmt.Errorf("RecordQueryFlatMapPlan inner lineage: %w", err)
			}
		}
	}
	// The retained result program is the first authority for the FlatMap's
	// authored AS/AT bindings. This ordering is load-bearing when a user alias
	// spells a private Explode field name: AS "_1" AT "O" has logical slots
	// [_1,O], while the selected Explode carrier has physical slots [_0,_1]. If
	// O is normalized to physical _1 before consulting this program, the generic
	// producer matcher can mistake it for the authored element named _1.
	beforeResult := reanchored
	reanchored, err = values.ReanchorOwnedValueThroughProducer(
		reanchored, p.resultValue, layout.Carrier(),
		producerOwnedCorrelations(p.resultValue))
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan result lineage: %w", err)
	}
	// Some exploratory/result programs retain the selected Explode's physical
	// [_0,_1] row instead of authored AS/AT fields. Only when the retained result
	// program could not prove a mapping do we use that exact selected operator as
	// authority to normalize logical field names, then retry the same producer.
	// The bridge still requires correlation, record identity, width, nullability,
	// ordinals, and leaf types; foreign and structurally drifted roots remain
	// unchanged for the final layout check.
	if reanchored == beforeResult && inner != nil && !p.innerAlias.IsZero() &&
		selectedOrdinalityExplode(inner) {
		innerLayout, layoutErr := inner.ProvidedOutputLayout()
		if layoutErr != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan inner layout: %w", layoutErr)
		}
		reanchored, err = values.TranslateProjectionInputNameNormalizationToCorrelation(
			reanchored, p.innerAlias, values.PhysicalCarrierType(innerLayout))
		if err != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan inner ordinality lineage: %w", err)
		}
		reanchored, err = values.ReanchorOwnedValueThroughProducer(
			reanchored, p.resultValue, layout.Carrier(),
			producerOwnedCorrelations(p.resultValue))
		if err != nil {
			return nil, fmt.Errorf("RecordQueryFlatMapPlan normalized result lineage: %w", err)
		}
	}
	reanchored, err = values.ReanchorValueForLayout(
		reanchored, layout.Carrier(), layout)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan output carrier: %w", err)
	}
	return reanchored, nil
}

// translateFlatMapChildOutputToBinding crosses the exact boundary between a
// selected child materializer and the binding under which the FlatMap evaluates
// that child. A nested materializer returns a Value rooted at its private
// current carrier; the FlatMap result program addresses the same row through
// outerAlias/innerAlias. TranslatePhaseRoot is pointer-exact, so a foreign or
// merely same-shaped current row is left untouched for the final layout check.
func translateFlatMapChildOutputToBinding(
	value values.Value,
	child RecordQueryPlan,
	bindingAlias values.CorrelationIdentifier,
) (values.Value, error) {
	if value == nil || child == nil || bindingAlias.IsZero() {
		return value, nil
	}
	layout, err := child.ProvidedOutputLayout()
	if err != nil {
		return nil, err
	}
	binding, err := values.NewQuantifiedObjectValue(
		bindingAlias, values.PhysicalCarrierType(layout))
	if err != nil {
		return nil, err
	}
	return values.TranslatePhaseRoot(value, layout.Carrier(), binding)
}

// valueNamesChild reports whether value addresses child's row at all — either
// through the alias the FlatMap binds that leg under, or through the exact
// carrier the child publishes. Both spellings occur: a value arrives on the
// binding alias and leaves a lineage crossing on the carrier.
func valueNamesChild(
	value values.Value,
	child RecordQueryPlan,
	bindingAlias values.CorrelationIdentifier,
) (bool, error) {
	if value == nil || child == nil {
		return false, nil
	}
	if !bindingAlias.IsZero() {
		if _, named := values.GetCorrelatedToOfValue(value)[bindingAlias]; named {
			return true, nil
		}
	}
	layout, err := child.ProvidedOutputLayout()
	if err != nil {
		return false, err
	}
	return valueReferencesExactQOV(value, layout.Carrier()), nil
}

// valueAnsweredByChild reports whether value is now expressed on child's own
// row. A layout error is answered as "no": this is a narrowing question asked
// after a crossing already succeeded, and a child that cannot state a layout
// simply did not claim the value.
func valueAnsweredByChild(
	value values.Value,
	child RecordQueryPlan,
	bindingAlias values.CorrelationIdentifier,
) bool {
	named, err := valueNamesChild(value, child, bindingAlias)
	return err == nil && named
}

// selectedOrdinalityExplode recognizes the exact physical producer whose SQL
// binding names intentionally differ from its positional carrier. A predicate
// filter is transparent to that row shape; no other wrapper is admitted.
func selectedOrdinalityExplode(plan RecordQueryPlan) bool {
	for plan != nil {
		switch typed := plan.(type) {
		case *RecordQueryExplodePlan:
			return typed.IsWithOrdinality()
		case *RecordQueryPredicatesFilterPlan:
			plan = typed.GetInner()
		default:
			return false
		}
	}
	return false
}

// valueReferencesExactQOV reports whether value is rooted in the exact QOV
// handle published by one selected child. Correlation and structural type are
// insufficient here: both FlatMap legs can be reserved-current rows with the
// same column names, while only one owns the incoming physical phase.
func valueReferencesExactQOV(value values.Value, target values.QuantifiedObjectValue) bool {
	if value == nil || target == nil {
		return false
	}
	if root, ok := values.AsQuantifiedObjectValue(value); ok && root == target {
		return true
	}
	if field, ok := values.AsFieldValue(value); ok {
		if root, rootOK := values.AsQuantifiedObjectValue(field.ChildValue()); rootOK && root == target {
			return true
		}
	}
	for _, child := range value.Children() {
		if valueReferencesExactQOV(child, target) {
			return true
		}
	}
	return false
}

// structuralKey lists the fields that distinguish this FlatMap in the memo: the
// outer/inner correlation aliases, the inheritOuterRecordProperties flag, the
// resultValue, and the provided physical output layout. Children (the two
// legs) are excluded. Java folds
// inheritOuterRecordProperties into computeHashCodeWithoutChildren but omits it
// from equals — Go compares it in BOTH so the equal⟹same-hash memo invariant
// holds (two plans differing only in that flag null-extend differently; they
// are not interchangeable). The resultValue uses semantic Value identity — Java
// RecordQueryFlatMapPlan.equalsWithoutChildren is semanticEqualsForResults. The
// Raw plan equality keeps exact aliases; expression equality maps aliases in
// the result and layout after the children are paired. Hashing omits alias
// spellings and uses OrdinalLayout.AliasFreeHash so both modes share a bucket.
func (p *RecordQueryFlatMapPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		MappedAlias(p.outerAlias).
		MappedAlias(p.innerAlias).
		Bool(p.inheritOuterRecordProperties).
		Value(p.resultValue).
		Layout(p.admittedProvidedOutputLayout())
}

func (p *RecordQueryFlatMapPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFlatMapPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryFlatMapPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("flatmap|")
}

// Explain renders both legs UNGUARDED, exactly as the raw-pointer form did: a
// nil leg panics here rather than rendering "<nil>". That is deliberate — a
// FlatMap with a missing leg is not a renderable plan, and quietly printing a
// placeholder would hide it. Whether to soften this is a separate decision.
func (p *RecordQueryFlatMapPlan) Explain() string {
	return fmt.Sprintf("FlatMap(outer=%s, inner=%s)", p.GetOuter().Explain(), p.GetInner().Explain())
}

var (
	_ RecordQueryPlan                  = (*RecordQueryFlatMapPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryFlatMapPlan)(nil)
)

// EqualsWithoutChildren compares child-local aliases, the result program and
// its physical output layout through the memo's alpha-renaming.
func (p *RecordQueryFlatMapPlan) EqualsWithoutChildren(other expressions.RelationalExpression, aliases *expressions.AliasMap) bool {
	o, ok := other.(*RecordQueryFlatMapPlan)
	return ok && p.structuralKey().EqualUnderAliases(o.structuralKey(), aliases.ToValuesAliasMap())
}

// CanCorrelate reports that this operator anchors a correlation between its
// children (the outer leg binds the value the inner leg reads), mirroring physicalFlatMapWrapper.
func (p *RecordQueryFlatMapPlan) CanCorrelate() bool { return true }

// GetCorrelatedToWithoutChildren walks this plan's own result value, mirroring
// the way the NestedLoopJoin walks its predicates. A FlatMap is the
// correlation-binding operator (CanCorrelate=true): its merged/projected result
// value is the node's own information and may reference a genuinely-EXTERNAL
// correlation (an enclosing FlatMap's outer row) that is reachable through
// nothing but this value. The framework subtracts this node's own leg aliases
// (expressionCorrelatedTo), so only real external correlations survive — but
// they MUST be reported, or a correlation-driven rule would hoist this FlatMap
// as if it were self-contained and drop the correlation. The retired
// physicalFlatMapWrapper returned the empty set here and deferred exactly this
// propagation (RFC-184 W2); the bare plan owns its result value, so it does the
// walk the wrapper could not.
func (p *RecordQueryFlatMapPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for k := range values.GetCorrelatedToOfValue(p.resultValue) {
		out[k] = struct{}{}
	}
	return out
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The FlatMap carries its two legs as memo quantifiers, so the
// relink keeps the outer/inner runtime aliases but re-resolves their retained
// result roots against the replacement children's exact types, then rebuilds
// the provided output layout (including the null-supplying-inner marker). This
// replaces physicalFlatMapWrapper.WithChildren
// (RFC-184 W2), whose separate snapshot plan field held the yield-time children
// verbatim; the swap re-resolves to the memo winner instead. The correlated
// inner is preserved because the emitter already froze it into a private
// single-member reference upstream — extraction recurses through that frozen
// edge and never consults the shared exploratory group.
func (p *RecordQueryFlatMapPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("RecordQueryFlatMapPlan.WithChildren: expected 2 children, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryFlatMapPlan) GetRecordQueryPlan() RecordQueryPlan { return p }

// physicalSourceType is a quantifier's flowed row WITH its leg boundaries — the
// type to hand any construction that re-mints a QOV from it.
//
// FlowedType withholds boundaries so physical layout cannot reach the semantic
// surface. That is right for comparison and wrong for carriage: a re-mint
// snapshots its layout from the type it is given, so the public spelling
// launders the boundaries out and the rebound value flows a row that no longer
// says which source owns which slots.
func physicalSourceType(qov values.QuantifiedObjectValue) values.Type {
	if qov == nil {
		return nil
	}
	if record := values.PhysicalFlowedRecordTypeOf(qov); record != nil {
		return record
	}
	return qov.FlowedType()
}
