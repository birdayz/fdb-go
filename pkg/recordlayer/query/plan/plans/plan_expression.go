package plans

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// This file makes a RecordQueryPlan BE a RelationalExpression, which is what
// Java has and Go did not:
//
//	QueryPlan<T> extends PlanHashable, RelationalExpression   (QueryPlan.java:51)
//	RecordQueryPlan extends QueryPlan<FDBQueriedRecord<Message>>
//	                                                     (RecordQueryPlan.java:73)
//
// plan.go's package doc claimed the opposite — "physical and logical plan
// trees live in different namespaces in Java" — and the whole
// physical_*_wrapper.go layer, plus the nil-inner shell bug class, descends
// from that misreading. Java does keep plans in a separate PACKAGE (which
// this package mirrors correctly); it does not keep them in a separate
// HIERARCHY.
//
// STEP 1 of RFC-183 P5. Plans still store their children as raw
// RecordQueryPlan pointers, so GetQuantifiers reports none — see the method
// comment. Step 2 replaces that storage with a Quantifier over a Reference,
// at which point the parent->child edge is stored ONCE and the shell state
// becomes unrepresentable rather than merely absent.

// PlanExprBase supplies the RelationalExpression methods that are identical
// across every plan type. Embed it; override anything a specific plan needs
// to answer differently (a set-op overrides ChildrenAsSet, a correlating
// operator overrides CanCorrelate and GetCorrelatedToWithoutChildren).
//
// resultValue is populated by the owning plan's fallible constructor. Keeping
// it here gives plans that merely pass through a child one stable result
// identity without forcing every such plan to repeat a field. A zero value is
// intentionally invalid: struct literals that bypass construction no longer
// receive an untyped, freshly-minted compatibility QOV.
type PlanExprBase struct {
	resultValue               values.Value
	ordinalPhysicalProperties OrdinalPhysicalProperties
}

// newPlanExprBaseWithProperties is the one admission point for a concrete
// result carrier and its physical-property view. Keeping the result Value and
// property in one returned base prevents a constructor from publishing a
// layout through ProvidedOutputLayout without publishing the same layout to
// physical-property selection (or vice versa).
func newPlanExprBaseWithProperties(
	owner string,
	resultValue values.Value,
	required []OrdinalLayoutRequirement,
	provided values.OrdinalLayout,
) (PlanExprBase, error) {
	if resultValue == nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: value is nil", owner)
	}
	resultType, err := values.SnapshotExactType(resultValue.Type())
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: %w", owner, err)
	}
	if err := values.ValidateOrdinalLayoutAdmission(provided); err != nil {
		return PlanExprBase{}, fmt.Errorf("%s ordinal physical properties: %w", owner, err)
	}
	carrier := provided.Carrier()
	if !resultType.Type().Equals(carrier.FlowedType()) {
		return PlanExprBase{}, fmt.Errorf(
			"%s ordinal physical properties: carrier type %s disagrees with result type %s",
			owner, carrier.FlowedType(), resultType.Type())
	}
	properties, err := NewOrdinalPhysicalProperties(required, nil, provided)
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s ordinal physical properties: %w", owner, err)
	}
	return PlanExprBase{
		resultValue:               resultValue,
		ordinalPhysicalProperties: properties,
	}, nil
}

// newPlanExprBaseWithProvidedLayout publishes only an output layout. This is
// the honest default for leaves and output-producing operators whose retained
// Value closure has not yet been represented as an evaluation program.
func newPlanExprBaseWithProvidedLayout(
	owner string,
	resultValue values.Value,
	provided values.OrdinalLayout,
) (PlanExprBase, error) {
	return newPlanExprBaseWithProperties(owner, resultValue, nil, provided)
}

// newPassThroughPlanExprBase publishes the exact selected child layout as both
// the one required input and the provided output. It is intentionally usable
// only after a concrete child layout has been obtained; callers with a dynamic
// or unresolved child must not fabricate an input requirement from its type.
func newPassThroughPlanExprBase(
	owner string,
	resultValue values.Value,
	layout values.OrdinalLayout,
) (PlanExprBase, error) {
	requirement, err := RequireExactLayout(layout)
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s required input layout: %w", owner, err)
	}
	return newPlanExprBaseWithProperties(
		owner, resultValue, []OrdinalLayoutRequirement{requirement}, layout)
}

func newPlanExprBaseForType(owner string, flowedType values.Type) (PlanExprBase, error) {
	exactType, err := values.SnapshotExactType(flowedType)
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: %w", owner, err)
	}
	layout, err := newIdentityOutputLayout(exactType.Type())
	if err != nil {
		if isExactErasedRecord(exactType.Type()) {
			resultValue, resultErr := values.NewQuantifiedObjectValue(
				values.UniqueCorrelationIdentifier(), exactType.Type())
			if resultErr != nil {
				return PlanExprBase{}, fmt.Errorf("%s result Value: %w", owner, resultErr)
			}
			return PlanExprBase{resultValue: resultValue}, nil
		}
		return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, err)
	}
	return newPlanExprBaseWithProvidedLayout(owner, layout.Carrier(), layout)
}

func newPlanExprBaseForQuantifier(owner string, quantifier expressions.Quantifier) (PlanExprBase, error) {
	resultValue, err := quantifier.RequireFlowedObjectValue()
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: %w", owner, err)
	}
	selectedChild := selectedPlanFromQuantifier(quantifier)
	child := selectedChild
	if child == nil {
		child = planTypeFromQuantifier(quantifier)
	}
	if child != nil {
		layout, layoutErr := child.ProvidedOutputLayout()
		if layoutErr == nil {
			// A physical pass-through emits the child's carrier unchanged. Reuse
			// that exact current handle as both the result Value and provided
			// layout owner. Publishing the child-edge QOV here would claim a
			// source binding the preserved layout does not provide; evaluating it
			// against the emitted row would then fail UnboundCorrelation.
			if !resultValue.Type().Equals(layout.Carrier().FlowedType()) {
				return PlanExprBase{}, fmt.Errorf(
					"%s provided output layout: carrier type %s disagrees with child-edge type %s",
					owner, layout.Carrier().FlowedType(), resultValue.Type())
			}
			if selectedChild != nil {
				return newPassThroughPlanExprBase(owner, layout.Carrier(), layout)
			}
			// A live exploratory group has not selected one physical child yet.
			// Preserve the existing provided-layout view used for type/shape
			// planning, but do not turn one currently visible alternative into a
			// required input contract. Reconstruction after winner selection
			// atomically replaces this with the exact requirement above.
			return newPlanExprBaseWithProvidedLayout(owner, layout.Carrier(), layout)
		}
		var unavailable *OrdinalLayoutUnavailableError
		if !errors.As(layoutErr, &unavailable) || unavailable.Code != OrdinalLayoutDynamicCarrier {
			return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, layoutErr)
		}
		return PlanExprBase{resultValue: resultValue}, nil
	}
	// A physical quantifier without a selected child layout can state only the
	// ordinary identity carrier for its exact flowed type. It still must not
	// publish the edge QOV alongside a layout that cannot bind that alias.
	return newPlanExprBaseForType(owner, resultValue.Type())
}

func newPlanExprBaseForFirstQuantifier(owner string, quantifiers []expressions.Quantifier) (PlanExprBase, error) {
	if len(quantifiers) == 0 {
		return PlanExprBase{}, fmt.Errorf("%s result Value: at least one input quantifier is required", owner)
	}
	base, err := newPlanExprBaseForQuantifier(owner, quantifiers[0])
	if err != nil {
		return PlanExprBase{}, err
	}
	firstType := base.resultValue.Type()
	for i := 1; i < len(quantifiers); i++ {
		flowedType, err := quantifiers[i].GetFlowedObjectType()
		if err != nil {
			return PlanExprBase{}, fmt.Errorf("%s input quantifier %d: %w", owner, i, err)
		}
		if !firstType.Equals(flowedType) {
			return PlanExprBase{}, fmt.Errorf(
				"%s result Value: input quantifier 0 type %s disagrees with input quantifier %d type %s",
				owner, firstType, i, flowedType)
		}
	}
	if len(quantifiers) > 1 && base.ordinalPhysicalProperties != nil {
		// RequiredInputLayouts is positionally parallel to input edges. The
		// first-quantifier helper knows which layout the operator currently
		// publishes, but it has not established one compatible exact layout for
		// every remaining leg. Retaining the first child's single requirement
		// here would therefore publish a partial vector. N-ary owners gain their
		// requirements when they select and validate all concrete child layouts.
		layout := base.ordinalPhysicalProperties.ProvidedOutputLayout()
		base, err = newPlanExprBaseWithProvidedLayout(owner, base.resultValue, layout)
		if err != nil {
			return PlanExprBase{}, err
		}
	}
	return base, nil
}

func newPlanExprBaseForValue(owner string, resultValue values.Value) (PlanExprBase, error) {
	if resultValue == nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: value is nil", owner)
	}
	exactType, err := values.SnapshotExactType(resultValue.Type())
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: %w", owner, err)
	}
	layout, err := newIdentityOutputLayout(exactType.Type())
	if err != nil {
		if isExactErasedRecord(exactType.Type()) {
			return PlanExprBase{resultValue: resultValue}, nil
		}
		return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, err)
	}
	return newPlanExprBaseWithProvidedLayout(owner, resultValue, layout)
}

// newPlanExprBaseForProvidedLayout admits an output-producing plan whose
// physical carrier is more specific than the identity layout implied by its
// logical result type. The layout is validated by the values-owned factory
// before reaching this helper; this boundary checks the remaining owner
// invariant and keeps the original result program, whose source roots describe
// the windows the layout publishes.
func newPlanExprBaseForProvidedLayout(
	owner string,
	resultValue values.Value,
	layout values.OrdinalLayout,
) (PlanExprBase, error) {
	if resultValue == nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: value is nil", owner)
	}
	if err := values.ValidateOrdinalLayoutAdmission(layout); err != nil {
		return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, err)
	}
	resultType, err := values.SnapshotExactType(resultValue.Type())
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: %w", owner, err)
	}
	if !resultType.Type().Equals(layout.Carrier().FlowedType()) {
		return PlanExprBase{}, fmt.Errorf(
			"%s provided output layout: carrier type %s disagrees with result type %s",
			owner, layout.Carrier().FlowedType(), resultType.Type())
	}
	return newPlanExprBaseWithProvidedLayout(owner, resultValue, layout)
}

func newPlanExprBaseForRetainedResult(
	owner string,
	resultValue values.Value,
	nullSupplying []values.QuantifiedObjectValue,
) (PlanExprBase, error) {
	if resultValue == nil {
		return PlanExprBase{}, fmt.Errorf("%s result Value: value is nil", owner)
	}
	if !values.ContainsBakedOrdinal(resultValue) && !values.IsPositionalMergeRC(resultValue) {
		return newPlanExprBaseForValue(owner, resultValue)
	}
	if _, ok := resultValue.(*values.RecordConstructorValue); !ok {
		// The executor's ordinal-build bare arm emits the selected object or
		// scalar itself; it has no new flat carrier or retained-source windows.
		return newPlanExprBaseForValue(owner, resultValue)
	}
	layout, err := values.NewFlatOrdinalLayoutForRetainedResult(resultValue, nullSupplying)
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, err)
	}
	return newPlanExprBaseForProvidedLayout(owner, resultValue, layout)
}

// exactQOVForResultSource finds the one exact source root retained by a result
// program for alias. A missing source is valid: a completely projected-away
// null-supplying leg has no output window and therefore needs no presence bit.
func exactQOVForResultSource(
	alias values.CorrelationIdentifier,
	resultValue values.Value,
) (values.QuantifiedObjectValue, error) {
	var found values.QuantifiedObjectValue
	var conflict error
	values.WalkValue(resultValue, func(value values.Value) bool {
		qov, ok := values.AsQuantifiedObjectValue(value)
		if !ok || qov.Correlation() != alias {
			return true
		}
		if found != nil && !found.FlowedType().Equals(qov.FlowedType()) {
			conflict = fmt.Errorf(
				"correlation %s has conflicting exact types %s and %s",
				alias.Name(), found.FlowedType(), qov.FlowedType())
			return false
		}
		found = qov
		return true
	})
	if conflict != nil {
		return nil, conflict
	}
	return found, nil
}

func isExactErasedRecord(typ values.Type) bool {
	if typ == nil || typ.Code() != values.TypeCodeRecord {
		return false
	}
	_, concrete := typ.(*values.RecordType)
	return !concrete
}

// newIdentityOutputLayout builds the one-to-one physical representation used
// by result-producing plans until they state a more specialized layout. A
// record's ordinary fields are one exact flat tile; the zero-field record has
// no tiles. Non-record values use the scalar carrier. The values factories do
// the final exact/private validation, including rejecting erased records,
// placeholders, NULL, and relation types rather than publishing an unknown
// physical property.
func newIdentityOutputLayout(typ values.Type) (values.OrdinalLayout, error) {
	if typ != nil && typ.Code() == values.TypeCodeRecord {
		recordType, ok := typ.(*values.RecordType)
		if !ok || recordType == nil {
			// Route erased/foreign record shapes through the values-owned
			// validator so callers receive the stable layout error.
			return values.NewOrdinalLayoutForCarrierType(typ, nil, nil)
		}
		var tiles []values.OrdinalTileSpec
		if width := len(recordType.Fields); width > 0 {
			tiles = []values.OrdinalTileSpec{{
				Start: 0,
				Width: width,
				Kind:  values.OrdinalTileFlat,
			}}
		}
		return values.NewOrdinalLayoutForCarrierType(typ, tiles, nil)
	}
	return values.NewScalarOrdinalLayoutForCarrierType(typ)
}

// validateQuantifierArity rejects a reconstruction before the receiver can be
// copied or indexed. WithQuantifiers is a planner boundary: returning the old
// expression for a malformed child list hides a broken rewrite, while indexing
// a short list panics after the caller has already begun planning work.
func validateQuantifierArity(planName string, got, want int) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%w: %s requires %d, got %d", expressions.ErrQuantifierArity, planName, want, got)
}

// CanCorrelate reports whether this operator anchors a correlation between
// its children. False for the physical operators that simply consume their
// input; the join-shaped plans override.
func (PlanExprBase) CanCorrelate() bool { return false }

// ChildrenAsSet reports whether the children are commutative. False by
// default; the set operations override.
func (PlanExprBase) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the correlations this node's own
// information depends on. Empty by default — a plan that carries predicates
// or a result value referencing an outer quantifier must override, or
// correlation-driven rules will misclassify it.
func (PlanExprBase) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// GetQuantifiers returns no quantifiers.
//
// This is honest rather than lazy at this step: a plan's children really are
// raw RecordQueryPlan pointers right now, so there are no quantifiers to
// report, and synthesising throwaway ones per call would invent Reference
// identities that nothing else shares. Step 2 gives each plan real quantifier
// storage and this method goes away in favour of per-type accessors.
//
// Nothing traverses plans as expressions yet, so reporting none changes no
// behaviour today.
func (PlanExprBase) GetQuantifiers() []expressions.Quantifier { return nil }

// GetResultValue returns the exact, stable Value admitted by the owner
// constructor. It never resolves on demand and never mints a correlation.
func (b PlanExprBase) GetResultValue() values.Value {
	return b.resultValue
}

// OrdinalPhysicalProperties returns the immutable physical-property view
// admitted atomically with this plan's result Value. Dynamic exact carriers do
// not invent an unknown layout/property; zero or bypass-constructed bases are
// malformed in the same typed way as ProvidedOutputLayout.
func (b PlanExprBase) OrdinalPhysicalProperties() (OrdinalPhysicalProperties, error) {
	if properties, ok := AsOrdinalPhysicalProperties(b.ordinalPhysicalProperties); ok {
		return properties, nil
	}
	if b.resultValue != nil && isExactErasedRecord(b.resultValue.Type()) {
		return nil, &OrdinalLayoutUnavailableError{Code: OrdinalLayoutDynamicCarrier}
	}
	return nil, &OrdinalLayoutUnavailableError{Code: OrdinalLayoutMalformedPlan}
}

// admittedProvidedOutputLayout is the non-fallible read used only by physical
// identity builders. Malformed zero-value test adversaries retain their prior
// nil-layout identity instead of panicking; public consumers use the fallible
// methods above and below.
func (b PlanExprBase) admittedProvidedOutputLayout() values.OrdinalLayout {
	properties, ok := AsOrdinalPhysicalProperties(b.ordinalPhysicalProperties)
	if !ok {
		return nil
	}
	return properties.ProvidedOutputLayout()
}

// ProvidedOutputLayout returns the immutable layout admitted atomically with
// the result Value. Plan reconstruction either retains this layout for an
// output-producing node or rebuilds the base from its replacement input.
func (b PlanExprBase) ProvidedOutputLayout() (values.OrdinalLayout, error) {
	properties, err := b.OrdinalPhysicalProperties()
	if err != nil {
		return nil, err
	}
	return properties.ProvidedOutputLayout(), nil
}

// QuantifierOverPlan wraps a child plan in the Quantifier a parent plan
// stores it as — the Go spelling of Java's `Quantifier.physical(
// call.memoizePlan(childPlan))`.
//
// StageCanonical, not StagePlanned: the stage records how far the PLANNER has
// processed a reference and is a separate decision from which member set the
// expression belongs in (see FinalOfAtStage). A plan's child belongs in the
// FINAL set — it is a plan — but stamping StagePlanned here would change what
// ExploreGroupTask does with the reference.
//
// Returns the zero Quantifier for a nil child so a leaf-shaped construction
// does not fabricate an empty reference.
func QuantifierOverPlan(child RecordQueryPlan) expressions.Quantifier {
	if child == nil {
		return expressions.Quantifier{}
	}
	return expressions.NewPhysicalQuantifier(
		expressions.FinalOfAtStage(child, expressions.StageCanonical))
}

// QuantifiersOverPlans is QuantifierOverPlan across an N-ary plan's child
// list — the Go spelling of Java's
// `children.stream().map(c -> Quantifier.physical(call.memoizePlan(c)))`.
//
// ORDER IS LOAD-BEARING and preserved exactly. Several of the plans that use
// this report ChildrenAsSet — Union, Intersection, MergeSortUnion — but that
// flag is about when two EXPRESSIONS are equivalent, not about whether this
// plan may reshuffle its own legs: Comparator indexes its reference plan by
// position, Selector picks by position, and every Explain renders legs in
// order. Index i in must stay index i out.
//
// A nil child yields the zero Quantifier, matching QuantifierOverPlan, so a
// list containing one round-trips back to a nil at the same position rather
// than shrinking the arity.
func QuantifiersOverPlans(children []RecordQueryPlan) []expressions.Quantifier {
	qs := make([]expressions.Quantifier, len(children))
	for i, child := range children {
		qs[i] = QuantifierOverPlan(child)
	}
	return qs
}

// plansFromQuantifiers is planFromQuantifier across a child-quantifier list,
// the read side of QuantifiersOverPlans and positionally its exact inverse.
//
// Builds a fresh slice on every call rather than caching one: the quantifiers
// are the single storage location for the parent->child edges now, and a
// cached plan slice would reintroduce the second location this step exists to
// delete. The cost is a small allocation per GetChildren; the benefit is that
// the two can never disagree.
//
// Always non-nil for a non-nil argument length, so a 0-leg set operation still
// hands back the empty-not-nil slice its callers saw when the legs were stored
// raw.
func plansFromQuantifiers(qs []expressions.Quantifier) []RecordQueryPlan {
	children := make([]RecordQueryPlan, len(qs))
	for i, q := range qs {
		children[i] = planFromQuantifier(q)
	}
	return children
}

// planFromQuantifier dereferences a child quantifier to THE plan it ranges
// over. Ports Java's Quantifier.Physical.getRangesOverPlan():
//
//	(RecordQueryPlan) Iterables.getOnlyElement(
//	        getRangesOver().getFinalExpressions())
//
// Java makes that dereference total by pruning a group to one final. Go keeps
// multiple finals when distinct physical properties require them and makes
// the dereference total with a stamped OPTIMIZE winner instead. A detached
// final singleton is the exact-selection fallback, and an exploratory member
// is consulted only for an in-progress group that has not finalized yet.
// RFC-224's TestExtractionIsUnambiguous verifies the selected Reference path
// after the task stack drains; it deliberately does not assert Java's
// singleton-final mechanism.
//
// Two accommodations Java does not need, both temporary:
//   - a member may still be a physical WRAPPER rather than a plan while the
//     wrapper layer is being retired, so a member exposing
//     GetRecordQueryPlan() is unwrapped. Matched structurally because the
//     wrapper interface lives in the cascades package, which this package
//     cannot import.
//   - the exploratory set is consulted when the final set is empty, for
//     references built mid-planning before finalization.
//
// Both go away with the wrappers.
func planFromQuantifier(q expressions.Quantifier) RecordQueryPlan {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	// A stamped OPTIMIZE winner IS the answer for a group pruned to its
	// cost-cheapest member for its requested ordering: the deferred-winner case
	// where a plan (a set operation) ranges over a SHARED multi-member leg group
	// whose per-ordering winner is chosen at optimization, not at yield. For a
	// singleton group the winner is that one member, so consulting it here is
	// behavior-preserving; for a multi-member group it returns the cost winner
	// instead of tripping the getOnlyElement singleton guard below. This is what
	// lets a deferred-winner set-op carry each leg as ONE live quantifier over
	// the real group rather than a separate snapshot — the RFC-184 W2 goal.
	// The physical sort resolves through this same path: it re-sorts its input, so
	// it needs the cheapest-valid member for ANY ordering — not a per-ordering
	// winner — and ref.Winner() is exactly that, the OPTIMIZE-chosen
	// overall-cheapest member (ordering-specific selection lives elsewhere, in
	// getWinnerForOrdering). Before the wrapper collapse the sort resolved this
	// itself via findBestPhysicalPlan; the collapsed plan routes through Winner()
	// here and reaches the same member.
	if w := ref.Winner(); w != nil {
		if p := planOfMember(w); p != nil {
			return p
		}
	}
	if p := onlyPlanFromFinalMembers(ref.FinalMembers()); p != nil {
		return p
	}
	return firstPlanFromMembers(ref.Members())
}

// planTypeFromQuantifier resolves a child quantifier to ANY member's plan for
// TYPE queries (GetResultType) — distinct from planFromQuantifier's IDENTITY
// resolution. A pass-through / column-aligned plan (set operation) flows its
// inner's row SHAPE, and every alternative in a group shares that shape, so the
// first member answers. Identity needs the single winner; type does not. This
// lets a deferred-winner plan report its result type DURING planning — before
// OPTIMIZE stamps the winner on its still-multi-member leg group — without
// tripping the singleton invariant that identity resolution enforces.
func planTypeFromQuantifier(q expressions.Quantifier) RecordQueryPlan {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	if p := firstPlanFromMembers(ref.FinalMembers()); p != nil {
		return p
	}
	return firstPlanFromMembers(ref.Members())
}

// selectedPlanFromQuantifier returns a concrete physical selection, never an
// arbitrary exploratory alternative. A stored winner is authoritative; a
// final singleton is the pinned-plan shape. In-progress exploratory groups
// return nil even when they happen to contain one member, since that group may
// still grow before optimization.
func selectedPlanFromQuantifier(q expressions.Quantifier) RecordQueryPlan {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	if winner := ref.Winner(); winner != nil {
		return planOfMember(winner)
	}
	// A singleton in the FINAL lane is a concrete selection only when the
	// reference is detached (FinalOf/FinalOfAtStage): those references have no
	// exploratory members. A live memo group keeps its logical/canonical
	// members while implementation rules add finals. Seeing the first final in
	// that mixed group does not select it -- more access paths can still arrive
	// later in the same exploration round. Treating it as selected freezes every
	// pass-through parent to that first plan's exact layout (normally Scan), so
	// subsequently generated IndexScan/FlatMap alternatives are incompatible at
	// extraction even when they are cheaper.
	if len(ref.Members()) != 0 {
		return nil
	}
	// Multiple physical finals before optimization are alternatives, not a
	// selected child. Constructor-time result/layout derivation may inspect one
	// for its common type, but must not panic or pretend one is the winner; the
	// extracted/relinked parent will receive the actual selected singleton.
	var selected RecordQueryPlan
	for _, member := range ref.FinalMembers() {
		plan := planOfMember(member)
		if plan == nil || plan == selected {
			continue
		}
		if selected != nil {
			return nil
		}
		selected = plan
	}
	return selected
}

// selectedInputOrdinalLayout returns the exact physical layout of a concrete
// selected child. Live memo edges have no selected member yet, and legacy
// dynamic-record children cannot state an ordinal layout; both deliberately
// return ok=false so construction can remain exploratory until extraction
// relinks the parent over a concrete singleton.
func selectedInputOrdinalLayout(q expressions.Quantifier) (layout values.OrdinalLayout, ok bool, err error) {
	child := selectedPlanFromQuantifier(q)
	if child == nil {
		return nil, false, nil
	}
	layout, err = child.ProvidedOutputLayout()
	if err == nil {
		return layout, true, nil
	}
	var unavailable *OrdinalLayoutUnavailableError
	if errors.As(err, &unavailable) && unavailable.Code == OrdinalLayoutDynamicCarrier {
		return nil, false, nil
	}
	return nil, false, err
}

// reanchorCurrentValueForInput normalizes an executable Value program onto the
// selected child's exact carrier handle. Reserved-current roots move to that
// exact handle, and source-relative fields supplied by the child's layout map
// to their carrier ordinals.
// CurrentCorrelation names a physical row phase, not a globally interchangeable
// alias: a materializing child (for example InMemorySort) publishes a fresh
// current handle even when its row type is unchanged. Retaining the upstream
// handle would therefore make the program unbindable at runtime.
//
// Non-current roots absent from the layout are intentionally preserved. They
// can name a declared physical edge or an outer binding and are validated by
// the operator-specific admission path.
func reanchorCurrentValueForInput(
	value values.Value,
	inputQ expressions.Quantifier,
) (values.Value, error) {
	if value == nil {
		return nil, nil
	}
	layout, selected, err := selectedInputOrdinalLayout(inputQ)
	if err != nil {
		return nil, err
	}
	if !selected {
		return value, nil
	}
	target := layout.Carrier()
	if target == nil {
		return nil, fmt.Errorf("selected input layout has no carrier")
	}
	edge, err := inputQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}
	// A parent's physical edge denotes the selected child's OUTPUT phase. Map
	// that declaration before consulting materializer lineage: a sort's lineage
	// accepts values rooted in its INPUT phase, while an edge-rooted value is
	// already expressed at the sort output boundary.
	normalized, err := values.TranslateDeclaredEdgeRoot(value, edge, target)
	if err != nil {
		return nil, err
	}
	selectedChild := selectedPlanFromQuantifier(inputQ)
	// A materializer owns the complete input-to-output lineage. Give it the
	// source-relative program before the generic producer matcher runs: in a
	// chained FlatMap, a buried S.NAME must first cross the outer positional
	// merge's exact S window. Matching it directly against the upper flattened
	// RC sees only one top-level NAME (R.NAME) and would incorrectly select that
	// unrelated slot by the generic unique-name fallback.
	if materializer, ok := descendantValueMaterializer(selectedChild); ok {
		return materializer.reanchorInputValueToOutput(normalized)
	}
	producer := selectedChild.GetResultValue()
	if retained, ok := descendantRetainedResultProducer(selectedChild); ok {
		producer = retained
	}
	normalized, err = values.ReanchorValueThroughProducer(normalized, producer, target)
	if err != nil {
		return nil, err
	}
	normalized, err = values.ReanchorValueForLayout(normalized, target, layout)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// inputValueMaterializer exposes the plan-time lineage needed to cross an
// operator that intentionally drops its child's source windows. Its runtime
// output remains current-only; this method is solely a checked rewrite from a
// pre-bound input program to the materialized output carrier.
type inputValueMaterializer interface {
	reanchorInputValueToOutput(values.Value) (values.Value, error)
}

func childValueMaterializer(plan RecordQueryPlan) (inputValueMaterializer, bool) {
	materializer, ok := plan.(inputValueMaterializer)
	return materializer, ok
}

// descendantValueMaterializer walks only exact row-preserving unary edges. A
// wrapper such as LIMIT keeps its child's precise provided-layout handle, so a
// projection above LIMIT still needs the lineage authority of a materializing
// sort below it. The walk stops as soon as a wrapper changes the layout or row
// type; source-window provenance must never leak through a reshaping operator.
func descendantValueMaterializer(plan RecordQueryPlan) (inputValueMaterializer, bool) {
	for plan != nil {
		if materializer, ok := childValueMaterializer(plan); ok {
			return materializer, true
		}
		unary, ok := plan.(interface{ GetInner() RecordQueryPlan })
		if !ok {
			return nil, false
		}
		inner := unary.GetInner()
		if inner == nil {
			return nil, false
		}
		outerLayout, outerErr := plan.ProvidedOutputLayout()
		innerLayout, innerErr := inner.ProvidedOutputLayout()
		if outerErr != nil || innerErr != nil ||
			outerLayout.Carrier() != innerLayout.Carrier() ||
			!outerLayout.RawEqual(innerLayout) {
			return nil, false
		}
		plan = inner
	}
	return nil, false
}

// descendantRetainedResultProducer finds the positional result program hidden
// by exact pass-through unary wrappers. PredicatesFilter, LIMIT, and similar
// nodes preserve their child's exact layout carrier but publish that carrier as
// their own bare result Value. A parent projection still needs the underlying
// NLJ/other RC to prove that logical A.ID is output ordinal 0; the wrapper's bare
// current QOV contains no source-lineage information.
//
// The walk uses the same exact row/layout identity gate as materializer lookup.
// It cannot cross a reshaping or freshly materializing operator, so source
// windows never leak across a producer boundary.
func descendantRetainedResultProducer(plan RecordQueryPlan) (values.Value, bool) {
	for plan != nil {
		if result := plan.GetResultValue(); result != nil {
			if _, ok := result.(*values.RecordConstructorValue); ok {
				return result, true
			}
		}
		unary, ok := plan.(interface{ GetInner() RecordQueryPlan })
		if !ok {
			return nil, false
		}
		inner := unary.GetInner()
		if inner == nil {
			return nil, false
		}
		outerLayout, outerErr := plan.ProvidedOutputLayout()
		innerLayout, innerErr := inner.ProvidedOutputLayout()
		if outerErr != nil || innerErr != nil ||
			outerLayout.Carrier() != innerLayout.Carrier() ||
			!outerLayout.RawEqual(innerLayout) {
			return nil, false
		}
		plan = inner
	}
	return nil, false
}

// onlyPlanFromFinalMembers is Java's `Iterables.getOnlyElement` half of
// getRangesOverPlan: a FINALIZED group has exactly one answer, and a
// quantifier dereferencing it that finds two has no basis for picking. Java
// throws there; so do we.
//
// The guard cannot fire today, and the reason is provenance, not luck: every
// reference a plan's quantifier ranges over is minted by QuantifierOverPlan,
// which builds a FRESH FinalOfAtStage singleton per child. A plan therefore
// never points at a shared memo group, so its final set holds exactly the one
// member that was put there. The guard goes live the moment that changes —
// i.e. the moment plan quantifiers point at shared references — which is
// precisely when silently taking the first member would start choosing an
// arbitrary plan out of a set of real alternatives.
//
// Counted by DISTINCT plan, not by member: while the wrapper layer is being
// retired a group's final set can legitimately hold both a wrapper and the
// plan it wraps — two members, one answer, no ambiguity.
func onlyPlanFromFinalMembers(members []expressions.RelationalExpression) RecordQueryPlan {
	var found RecordQueryPlan
	for _, m := range members {
		p := planOfMember(m)
		if p == nil || p == found {
			continue
		}
		if found != nil {
			panic(fmt.Sprintf(
				"plan-invariant: quantifier ranges over a reference with %d plan-typed final members (%T and %T) — a plan's child reference must be a singleton",
				len(members), found, p))
		}
		found = p
	}
	return found
}

// firstPlanFromMembers serves the EXPLORATORY fallback, where a multi-member
// set is the normal, expected state: the group holds every alternative
// explored so far and has not been pruned to a winner. No getOnlyElement
// contract applies, so the first plan-typed member answers.
func firstPlanFromMembers(members []expressions.RelationalExpression) RecordQueryPlan {
	for _, m := range members {
		if p := planOfMember(m); p != nil {
			return p
		}
	}
	return nil
}

// planOfMember returns the plan a member IS, or the plan it wraps, or nil.
func planOfMember(m expressions.RelationalExpression) RecordQueryPlan {
	if p, ok := m.(RecordQueryPlan); ok {
		return p
	}
	if w, ok := m.(interface{ GetRecordQueryPlan() RecordQueryPlan }); ok {
		return w.GetRecordQueryPlan()
	}
	return nil
}

// planEqualsAsExpression is the shared body of every plan's
// EqualsWithoutChildren(RelationalExpression, *AliasMap).
//
// The alias map is unused while plans hold no quantifiers: alias-aware
// comparison exists to equate two expressions whose children are bound to
// differently-named quantifiers, and a plan currently has none. Step 2 makes
// it meaningful.
//
// Not a method on PlanExprBase because it needs the concrete receiver to
// reach its EqualsPlanWithoutChildren; each plan type spells a one-line
// override that calls this.
func planEqualsAsExpression(self RecordQueryPlan, other expressions.RelationalExpression) bool {
	op, ok := other.(RecordQueryPlan)
	return ok && self.EqualsPlanWithoutChildren(op)
}

// scanComparisonCorrelations returns the correlations a scan-shaped plan
// reaches through its comparison operands. Ported from the cascades package's
// helper of the same name, which the scan and index-scan physical wrappers used
// for GetCorrelatedToWithoutChildren; the wrappers are retired in a later step.
func scanComparisonCorrelations(comps []*predicates.ComparisonRange) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	collect := func(c *predicates.Comparison) {
		if c == nil || c.Operand == nil {
			return
		}
		for a := range values.GetCorrelatedToOfValue(c.Operand) {
			out[a] = struct{}{}
		}
		// A query-parameter (ConstantObjectValue) comparand is an execution constant
		// bound at run time, NOT a row correlation — its constant-pool alias appears
		// in GetCorrelatedToOfValue but must not make a `Scan(T,[k=?param])` look
		// join-correlated to planning (B1 leg detection) or to the
		// probe-fed-residual guard (compensationProbeCorrelations). Subtract any such
		// aliases — the value-level twin of deletePredicateConstantObjectAliases.
		values.WalkValue(c.Operand, func(node values.Value) bool {
			if cov, ok := node.(*values.ConstantObjectValue); ok {
				delete(out, cov.Alias)
			}
			return true
		})
	}
	for _, cr := range comps {
		if cr == nil || cr.IsEmpty() {
			continue
		}
		if cr.IsEquality() {
			collect(cr.GetEqualityComparison())
		} else if cr.IsInequality() {
			for _, c := range cr.GetInequalityComparisons() {
				collect(c)
			}
		}
	}
	return out
}
