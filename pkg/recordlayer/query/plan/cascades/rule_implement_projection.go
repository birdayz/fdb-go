package cascades

import (
	"bytes"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementProjectionRule implements a logical LogicalProjectionExpression
// as a physical RecordQueryProjectionPlan, gated on the inner Reference
// having at least one physical-plan member.
type ImplementProjectionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementProjectionRule() *ImplementProjectionRule {
	return &ImplementProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

func (r *ImplementProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementProjectionRule) OnMatch(call *ExpressionRuleCall) {
	proj := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	qs := proj.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	// Try covering merge: if inner has a Fetch wrapper and all
	// projected values can push through, yield a Projection over a
	// covering IndexScan — the Fetch is dropped, the Projection is
	// retained (see rule_merge_projection_and_fetch.go: Go's covering
	// plans carry the FULL partial-record result value, so dropping the
	// Projection would leak the whole record).
	//
	// This is an ExpressionRule from BatchAExpressionRules, so it fires
	// during PLANNING, the same phase as Java's
	// MergeProjectionAndFetchRule and the same phase as Go's own
	// MergeProjectionAndFetchRule implementation rule — the difference
	// between the two Go stampers is expression-rule versus
	// implementation-rule, not phase. Firing here means the covering
	// scan participates in sort elimination and cost comparison.
	projectedValues := proj.GetProjectedValues()
	for _, m := range innerRef.AllMembers() {
		fetchW, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan)
		if !ok {
			continue
		}
		fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
		if fetchInnerRef == nil {
			continue
		}
		innerQ := expressions.ForEachQuantifier(fetchInnerRef)
		srcAlias := qs[0].GetAlias()
		tgtAlias := innerQ.GetAlias()
		allPushable := true
		pushedValues := make([]values.Value, len(projectedValues))
		for i, v := range projectedValues {
			pushed, ok := fetchW.PushValue(v, srcAlias, tgtAlias)
			if !ok || pushed == nil {
				allPushable = false
				break
			}
			pushedValues[i] = pushed
		}
		if !allPushable {
			continue
		}
		// Every projected value pushes through the fetch, so the fetch goes and
		// its CHILD is used as-is (Java MergeProjectionAndFetchRule). Nothing is
		// stamped: the child is already Covering(IndexScan) from the access path.
		// The projection is RETAINED above it — Go's deliberate, permanent
		// divergence from Java's `yieldPlan(fetchPlan.getChild())`, see
		// rule_merge_projection_and_fetch.go and DIVERGENCES.md.
		//
		// The projection is its own cascades expression carrying the live
		// fetch-inner edge (RFC-184 W2).
		plan, err := plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
			pushedValues, proj.GetAliases(), proj.GetAliasMinted(), proj.GetOutputNames(), innerQ)
		if err != nil {
			call.Fail(err)
			return
		}
		plan, err = plan.WithAliasSources(proj.GetAliasSources())
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(plan)
	}

	// Normal path: construct one projection parent over every physical child
	// member. Java's rule matcher binds one PlanPartition at a time; selecting a
	// single local winner here loses alternatives before the parent-group cost
	// comparison can see them. That is especially visible for
	// Project(Filter(Fetch(Covering(Index)))) versus
	// Project(Filter(Index)): the covering tree wins at the root, but cannot win
	// if only the locally cheaper fetching child is ever wrapped.
	//
	// Each parent gets a newly minted reference restricted to the member it was
	// built over. MemoizeExpression would return the entire source group, making
	// every loop iteration the same Projection(group) expression and collapsing
	// the enumeration back to one member.
	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		orderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	}

	seen := make(map[expressions.RelationalExpression]bool)
	yieldForMember := func(member expressions.RelationalExpression) bool {
		if member == nil || seen[member] {
			return true
		}
		seen[member] = true
		if _, ok := member.(physicalPlanExpression); !ok {
			return true
		}
		// AggregateDataAccessRule already publishes its canonical aggregate row
		// through a physical Projection. The SQL projection immediately above it
		// is sometimes the exact field-by-field identity over that row. Wrapping
		// the already-materialized projection again is not merely noisy: it adds a
		// second row-owner boundary whose only program is [#0, #1, ...]. Reuse the
		// child in that one proved case.
		//
		// This is intentionally narrower than a general projection-elimination
		// rule. The child must itself be a Projection (the output-schema and alias
		// provenance authority we can compare), every outer slot must be the
		// corresponding one-step ordinal read from this logical edge, and the two
		// admitted relation types must be canonically identical. Display names are
		// never consulted. Reordered, narrowed, renamed, computed, foreign-root,
		// and scalar-QOV wrappers therefore keep their physical projection.
		if reusable, ok := exactPositionalProjectionReuse(proj, member); ok {
			call.Yield(reusable)
			return true
		}
		innerQ := expressions.NamedForEachQuantifier(qs[0].GetAlias(), call.MemoizeMemberPlansFromOther(
			innerRef, []expressions.RelationalExpression{member}))
		logicalEdge, err := qs[0].RequireFlowedObjectValue()
		if err != nil {
			call.Fail(err)
			return false
		}
		physicalEdge, err := innerQ.RequireFlowedObjectValue()
		if err != nil {
			call.Fail(err)
			return false
		}
		rebasedValues, err := rebaseProjectionValuesForPhysicalEdge(
			proj.GetProjectedValues(), logicalEdge, physicalEdge)
		if err != nil {
			call.Fail(err)
			return false
		}
		plan, err := plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
			rebasedValues, proj.GetAliases(), proj.GetAliasMinted(), proj.GetOutputNames(), innerQ)
		if err != nil {
			call.Fail(err)
			return false
		}
		plan, err = plan.WithAliasSources(proj.GetAliasSources())
		if err != nil {
			call.Fail(err)
			return false
		}
		call.Yield(plan)
		return true
	}

	for _, member := range physicalMembersForParentEnumeration(innerRef) {
		if !yieldForMember(member) {
			return
		}
	}

	// Keep the requested-ordering path even when the enumeration helper had to
	// fall back to a different member lane. It guarantees an ordering-satisfying
	// alternative is represented; the seen set prevents a duplicate when that
	// winner was already among the enumerated physical members.
	for _, ordering := range orderings {
		// satisfied deliberately DISCARDED (RFC-186 §2C): this wrapper is an
		// orderingDelegator — its ordering claim is re-derived through
		// OrderingSourceRef at lookup time and pinOrderedSpine declines
		// unsatisfied spines, so an unordered fallback yield here can never
		// be CLAIMED as ordered; it is the member the in-memory-sort
		// enforcer wraps (declining instead would empty the group — no plan).
		winner, _ := getWinnerForOrdering(innerRef, ordering, call.CostModel())
		if !yieldForMember(winner) {
			return
		}
	}
}

// exactPositionalProjectionReuse returns member only when removing logical
// changes no row value, schema, or alias provenance. Restricting member to a
// physical Projection is load-bearing: arbitrary plans do not expose the raw
// alias/provenance vector needed to prove that ResultSet metadata is unchanged.
func exactPositionalProjectionReuse(
	logical *expressions.LogicalProjectionExpression,
	member expressions.RelationalExpression,
) (*plans.RecordQueryProjectionPlan, bool) {
	physical, ok := member.(*plans.RecordQueryProjectionPlan)
	if !ok || physical == nil || logical == nil {
		return nil, false
	}

	logicalType, err := admitMemoExpression(logical)
	if err != nil {
		return nil, false
	}
	physicalType, err := admitMemoExpression(physical)
	if err != nil || !bytes.Equal(logicalType.CanonicalBytes(), physicalType.CanonicalBytes()) {
		return nil, false
	}

	projected := logical.GetProjectedValues()
	if len(projected) == 0 ||
		!slices.Equal(logical.GetOutputNames(), physical.GetOutputNames()) ||
		!sameProjectionMetadata(logical, physical, len(projected)) {
		return nil, false
	}

	edgeRoot, err := logical.GetInner().RequireFlowedObjectValue()
	if err != nil {
		return nil, false
	}
	layout, err := physical.ProvidedOutputLayout()
	if err != nil || layout == nil || layout.Carrier() == nil {
		return nil, false
	}
	carrier := layout.Carrier()
	for ordinal, value := range projected {
		field, isField := values.AsFieldValue(value)
		if !isField || field.Path() == nil {
			return nil, false
		}
		ordinals := field.Path().Ordinals()
		if len(ordinals) != 1 || ordinals[0] != ordinal {
			return nil, false
		}
		root, isRoot := values.AsQuantifiedObjectValue(field.ChildValue())
		if !isRoot {
			return nil, false
		}
		if root.Correlation() == values.CurrentCorrelation() {
			// Current names a row phase, so pointer comparison before selection is
			// invalid: the logical draft and the selected physical child carry
			// separate immutable handles. Resolve through the child's exact layout,
			// then require the selected carrier pointer and the same ordinal.
			normalized, normalizeErr := values.ReanchorValueForLayout(value, carrier, layout)
			normalizedField, normalizedFieldOK := values.AsFieldValue(normalized)
			if normalizeErr != nil || !normalizedFieldOK || normalizedField.Path() == nil ||
				!slices.Equal(normalizedField.Path().Ordinals(), []int{ordinal}) {
				return nil, false
			}
			normalizedRoot, normalizedRootOK := values.AsQuantifiedObjectValue(normalizedField.ChildValue())
			if !normalizedRootOK || normalizedRoot != carrier {
				return nil, false
			}
			continue
		}
		if !projectionRootIsThisInput(root, edgeRoot) {
			return nil, false
		}
	}
	return physical, true
}

// sameProjectionMetadata compares raw alias and provenance semantics slot by
// slot. A short vector means the documented default (empty alias / not minted),
// so nil and an explicit vector of defaults are equivalent. An extra entry is
// malformed and cannot license removal.
func sameProjectionMetadata(
	logical *expressions.LogicalProjectionExpression,
	physical *plans.RecordQueryProjectionPlan,
	slots int,
) bool {
	logicalAliases := logical.GetAliases()
	physicalAliases := physical.GetAliases()
	logicalMinted := logical.GetAliasMinted()
	physicalMinted := physical.GetAliasMinted()
	logicalSources := logical.GetAliasSources()
	physicalSources := physical.GetAliasSources()
	if len(logicalAliases) > slots || len(physicalAliases) > slots ||
		len(logicalMinted) > slots || len(physicalMinted) > slots ||
		len(logicalSources) > slots || len(physicalSources) > slots {
		return false
	}
	for i := 0; i < slots; i++ {
		logicalAlias := ""
		if i < len(logicalAliases) {
			logicalAlias = logicalAliases[i]
		}
		physicalAlias := ""
		if i < len(physicalAliases) {
			physicalAlias = physicalAliases[i]
		}
		if logicalAlias != physicalAlias {
			return false
		}
		logicalSlotMinted := i < len(logicalMinted) && logicalMinted[i]
		physicalSlotMinted := i < len(physicalMinted) && physicalMinted[i]
		if logicalSlotMinted != physicalSlotMinted {
			return false
		}
		logicalSource := values.ProjectionAliasSource{}
		if i < len(logicalSources) {
			logicalSource = logicalSources[i]
		}
		physicalSource := values.ProjectionAliasSource{}
		if i < len(physicalSources) {
			physicalSource = physicalSources[i]
		}
		if logicalSource != physicalSource {
			return false
		}
	}
	return true
}

func projectionRootIsThisInput(
	root values.QuantifiedObjectValue,
	edgeRoot values.QuantifiedObjectValue,
) bool {
	if root == nil || edgeRoot == nil {
		return false
	}
	if root.Correlation() != edgeRoot.Correlation() {
		return false
	}
	rootType, err := values.SnapshotExactType(root.FlowedType())
	if err != nil {
		return false
	}
	edgeType, err := values.SnapshotExactType(edgeRoot.FlowedType())
	return err == nil && bytes.Equal(rootType.CanonicalBytes(), edgeType.CanonicalBytes())
}

// rebaseProjectionValuesForPhysicalEdge translates the retained logical Value
// program onto the concrete quantifier edge selected by an implementation
// rule. The physical edge is deliberately minted independently from the
// logical edge; carrying the old QOV through would make execution either rely
// on an ambient positional fallback or (under exact binding) fail unbound.
func rebaseProjectionValuesForPhysicalEdge(
	projected []values.Value,
	source, target values.QuantifiedObjectValue,
) ([]values.Value, error) {
	rebased := make([]values.Value, len(projected))
	var err error
	for i, value := range projected {
		// Correlation names are conventional, not globally unique owners. A
		// joined-row edge is commonly named after its rightmost source, so an
		// alias-only rebase rewrites that source's exact QOV onto a differently
		// typed whole-row edge and erases the lineage a selected FlatMap needs.
		// Move only the declared edge (correlation plus exact type); retained
		// same-named source windows remain source-relative until materialization.
		rebased[i], err = values.TranslateDeclaredEdgeRoot(value, source, target)
		if err != nil {
			return nil, err
		}
		if rebased[i] == nil {
			return nil, &values.ResolutionError{
				ErrorCode: values.RewriteNilReplacement,
				Path:      "projection.edge",
				Detail:    "checked physical-edge rebase returned nil",
			}
		}
	}
	return rebased, nil
}

var _ ExpressionRule = (*ImplementProjectionRule)(nil)
