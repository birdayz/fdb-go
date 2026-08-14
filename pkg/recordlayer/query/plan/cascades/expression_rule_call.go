package cascades

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// ExpressionRuleCall is the rule-invocation context used by
// RelationalExpression-shaped rules. Counterpart to the existing
// RuleCall (which targets QueryPredicate / Value rules) — split per
// type so each rule shape gets a strongly-typed Yield.
//
// Ports the consulted surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.CascadesRuleCall`
// (Java's class also covers partial-match yields, planner-phase
// plumbing, and traversal-state hooks). Go exposes:
//
//   - Bindings: the pattern matcher's results, keyed by matcher
//     identity (already provided by `matching.PlannerBindings`).
//   - Reference: the memo group whose member fired the rule. Yields
//     append to this Reference; the dedup happens via Reference.Insert.
//   - Context: the PlanContext (planner config + match candidates).
//   - Memo: the Memo for cross-Reference memoization (nil when running
//     outside the Planner, e.g. in standalone tests).
//   - Yield(expr): insert a new equivalent expression into the
//     Reference.
//   - MemoizeExpression(expr): find-or-create a Reference for a
//     sub-expression via the Memo. Falls back to InitialOf when no
//     Memo is present.
//   - Yielded(): the list of expressions yielded so far. Tests + the
//     planner's traversal driver consume this.
//
// Java's flavoured yields (exploratory / final / plan / unknown) map
// onto the single staged Yield here. The task-stack driver publishes the
// complete batch only after OnMatch succeeds, choosing Insert or
// InsertFinal by phase; the data-access path routes by physicality via
// the planner's yieldUnknown.
type ExpressionRuleCall struct {
	Bindings  *matching.PlannerBindings
	Reference *expressions.Reference
	Context   PlanContext
	// RunContext is the cancellation scope for this planner invocation. It is
	// nil only for standalone rule tests constructed outside Planner.
	RunContext  context.Context
	Constraints *ConstraintMap
	// Stats is the planner's table-cardinality provider, threaded so a
	// rule's internal cost comparisons (e.g. selecting the best physical
	// join child) use REAL cardinalities rather than the default
	// LeafScanCardinality. Nil on test/utility firing paths — CostModel()
	// falls back to the default-stats comparator. Without this, the NLJ
	// rule's findBestValidPhysicalExpr ranks join children with every table at
	// LeafScanCardinality, so child selection ties and commits to the
	// FROM-clause order instead of the cost-optimal one (RFC-041).
	Stats       properties.StatisticsProvider
	memo        *Memo
	yieldedExps []expressions.RelationalExpression
	err         error
}

// Fail records the first rule-body failure. Drivers must check Err before
// publishing any staged rule effects.
func (c *ExpressionRuleCall) Fail(err error) {
	if c == nil || err == nil || c.err != nil {
		return
	}
	c.err = err
}

// Err returns the first failure reported by the rule body.
func (c *ExpressionRuleCall) Err() error {
	if c == nil {
		return nil
	}
	return c.err
}

// CancellationErr reports whether the owning planning run was canceled.
// Standalone rule calls with no RunContext are non-cancelable.
func (c *ExpressionRuleCall) CancellationErr() error {
	if c == nil || c.RunContext == nil {
		return nil
	}
	return c.RunContext.Err()
}

// CostModel returns the comparator a rule should use for internal best-plan
// selection: stats-aware when the planner threaded statistics, else the
// default-stats comparator.
func (c *ExpressionRuleCall) CostModel() func(a, b expressions.RelationalExpression) bool {
	ctx := c.Context
	if c.Stats == nil {
		// Preserve the historical nil-context comparison while retaining only
		// an injected diagnostic sink.
		ctx = costModelDiagnosticsOnlyContext(ctx)
	}
	return NewPlanningCostModelLessWithContext(c.Stats, ctx)
}

// NewExpressionRuleCall builds a rule-call against a Reference + an
// already-computed binding set. Context defaults to EmptyPlanContext
// if nil — convenient for tests that don't depend on planner config.
func NewExpressionRuleCall(ref *expressions.Reference, bindings *matching.PlannerBindings, ctx PlanContext) *ExpressionRuleCall {
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	return &ExpressionRuleCall{
		Bindings:  bindings,
		Reference: ref,
		Context:   ctx,
	}
}

// NewExpressionRuleCallWithMemo builds a rule-call with a Memo for
// cross-Reference memoization. Used by the Planner's ApplyRulesTask.
func NewExpressionRuleCallWithMemo(ref *expressions.Reference, bindings *matching.PlannerBindings, ctx PlanContext, memo *Memo) *ExpressionRuleCall {
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	return &ExpressionRuleCall{
		Bindings:  bindings,
		Reference: ref,
		Context:   ctx,
		memo:      memo,
	}
}

// Yield stages expr in this invocation-local batch. It deliberately does not
// mutate the Reference or Memo: the driver first lets OnMatch finish, checks
// Err, validates the complete batch, and only then commits it. The bool reports
// commit-time deduplication is intentionally not observable from inside the
// rule body.
func (c *ExpressionRuleCall) Yield(expr expressions.RelationalExpression) {
	if expr == nil {
		panic("ExpressionRuleCall.Yield: nil expression")
	}
	if c.Err() != nil || c.CancellationErr() != nil {
		return
	}
	c.yieldedExps = append(c.yieldedExps, expr)
}

// MemoizeExpression finds or creates a Reference for a sub-expression.
// When a Memo is present (running inside the Planner), this checks if
// an existing Reference already holds a structurally-equivalent
// expression and returns it — enabling cross-Reference sharing.
// Without a Memo (standalone rule testing), falls back to
// expressions.InitialOf(expr).
//
// The current call's Reference (the one the rule is yielding into) is
// excluded from reuse to prevent self-referential cycles. This mirrors
// Java's guard: `Verify.verify(existingReference != this.root)`.
//
// Rules should use this instead of expressions.InitialOf when creating
// child References for yielded expressions. This is how the Cascades
// planner avoids redundant exploration of shared sub-trees.
// InsertReExploring inserts expr into ref through the memo's scheduled
// insert (epoch re-arm + re-round when ref's exploration already began).
// Rule code adding members to a reference it did not just create must use
// this instead of Reference.Insert.
func (c *ExpressionRuleCall) InsertReExploring(ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	if c.memo != nil {
		return c.memo.InsertReExploring(ref, expr)
	}
	return ref.Insert(expr)
}

func (c *ExpressionRuleCall) MemoizeExpression(expr expressions.RelationalExpression) *expressions.Reference {
	if c.memo != nil {
		ref := c.memo.MemoizeExpression(expr)
		// Compare canonical identities: after a cross-group merge (RFC-037)
		// the memoized Reference and c.Reference may be the same group
		// reached via different pointers. Reusing the rule's OWN group
		// would create a self-edge (the yielded expression ranging over
		// its own group — a cycle), so a fresh reference is minted — and
		// it MUST be REGISTERED with the memo: Java's no-reuse path is
		// addNewReference (CascadesRuleCall.memoizeExploratoryExpressions;
		// self-reuse is structurally impossible there and Verify-guarded).
		// An unregistered orphan is invisible to the topology and to task
		// scheduling — a yielded tree wired over it strands with a logical
		// member no exploration ever implements ("best expression is not a
		// physical plan"; the LEFT-box + unnest + EXISTS no-plan).
		if ref.Canonical() == c.Reference.Canonical() {
			fresh := expressions.InitialOf(expr)
			c.memo.ScheduleFreshReference(fresh)
			return fresh
		}
		return ref
	}
	return expressions.InitialOf(expr)
}

// GetRequestedOrderings returns the requested orderings for this
// Reference from the constraint map, if available. Returns nil if no
// ordering constraint is set or no constraint map is present.
func (c *ExpressionRuleCall) GetRequestedOrderings() []*properties.RequestedOrdering {
	orderings, ok := Get(c.Constraints, c.Reference, RequestedOrderingConstraintKey)
	if !ok {
		return nil
	}
	return orderings
}

// Yielded returns the expressions the rule has yielded so far,
// including duplicates that Reference.Insert filtered. Useful for
// rule-firing tests that want to assert on the rule's output without
// reaching into the Reference's member list.
func (c *ExpressionRuleCall) Yielded() []expressions.RelationalExpression {
	if c == nil || c.Err() != nil || len(c.yieldedExps) == 0 {
		return nil
	}
	return append([]expressions.RelationalExpression(nil), c.yieldedExps...)
}

// MemoizeFinalExpression creates a NEW Reference holding expr as its single
// FINAL member, without interning into the shared memo — Java's memoizePlan
// (Reference.ofFinalExpressions). The ImplementationRuleCall twin.
//
// MemoizeExpression above INTERNS: it asks the memo for a group and may hand
// back an existing one. That is right for a canonical logical expression, and
// wrong for a COMPENSATING physical operator a rule just built, which is not
// equivalent to anything already in the memo and must not be deduped against
// it.
func (c *ExpressionRuleCall) MemoizeFinalExpression(expr expressions.RelationalExpression) *expressions.Reference {
	return expressions.FinalOfAtStage(expr, expressions.StageCanonical)
}

// MemoizeMemberPlansFromOther mints a NEW reference holding only `members` —
// which must already be members of `source` — as final expressions. Ports
// Java's FinalMemoizer.memoizeMemberPlansFromOther (CascadesRuleCall.java:518 →
// Reference.newReferenceFromFinalMembers:587), whose contract is that the
// returned reference "is always newly created and never reused".
//
// This is the RESTRICTION an enumerating implementation rule needs, and it is
// what MemoizeExpression cannot give it. MemoizeExpression INTERNS: asked for a
// member, it hands back a group that already contains that member — which, for
// a member of the rule's own child group, is that whole child group. A rule
// that loops over N child members and memoizes each one therefore builds N
// structurally IDENTICAL parents over the same reference, and they collapse to
// one on insert. The loop enumerates nothing.
//
// Java cannot reach that state because it binds a PlanPartition and memoizes a
// reference restricted to that partition's plans; the restriction is not an
// optimization there, it is what makes the per-partition parent distinct.
//
// The source's per-plan property map is carried over RESTRICTED to the retained
// members. Copying it wholesale would leave the new reference reporting
// partitions (ToPlanPartitions walks the property map, not the member list) for
// plans it does not contain, which is the same conflation in the other
// direction.
func (c *ExpressionRuleCall) MemoizeMemberPlansFromOther(
	source *expressions.Reference,
	members []expressions.RelationalExpression,
) *expressions.Reference {
	// Expression implementation rules are already constructing physical parent
	// alternatives during PLANNING. Keep the restricted physical child at the
	// planned stage: revisiting a canonical-stage singleton promotes its chosen
	// final into the exploratory lane and clears the final lane before physical
	// rewrites run. A rewrite such as Fetch(Covering(Index)) -> Index then leaves
	// only the new Index final, so the supposedly restricted Fetch alternative
	// mutates underneath its parent and can never participate in root costing.
	// StagePlanned still allows the physical final to be explored; it merely
	// prevents the stage transition from deleting the selected final.
	return newRestrictedFinalReference(
		"MemoizeMemberPlansFromOther", source, members, expressions.StagePlanned)
}

// newRestrictedFinalReference is the ONE implementation behind both memoize-
// from-other entry points — this file's MemoizeMemberPlansFromOther and
// ImplementationRuleCall.MemoizeFinalExpressionsFromOther. They are twins of
// Java's two FinalMemoizer methods, and they had drifted: one restricted the
// property map to the retained members and the other copied the source's map
// wholesale. Two implementations of one rule is how that drift happened, so
// there is now one.
//
// caller names the entry point in any panic, because "which of the two" is the
// first thing anyone debugging this needs and the stack alone will not say it
// once both are one call deep.
func newRestrictedFinalReference(
	caller string,
	source *expressions.Reference,
	members []expressions.RelationalExpression,
	stage expressions.PlannerStage,
) *expressions.Reference {
	assertMembersOf(caller, source, members)

	var ref *expressions.Reference
	for i, m := range members {
		if i == 0 {
			if stage == expressions.StagePlanned && len(members) == 1 {
				ref = expressions.PinnedFinalOf(m)
			} else {
				ref = expressions.FinalOfAtStage(m, stage)
			}
		} else {
			ref.InsertFinal(m)
		}
	}
	if ref == nil {
		return &expressions.Reference{}
	}
	// The property map is carried over RESTRICTED to the retained members.
	// Copying it wholesale leaves the new reference reporting partitions for
	// plans it does not contain, because ToPlanPartitions walks the property
	// MAP rather than the member list — so a restricted reference would still
	// offer the whole source group to anything reading partitions, which is
	// precisely the conflation the restriction exists to remove.
	//
	// Java never faces the choice: Reference.of mints a FRESH propertiesMap and
	// runs finalExpressions.forEach(propertiesMap::add) (Reference.java:181-182),
	// so it carries properties for exactly the retained members by construction.
	if pm := GetRefPlanPropertiesMap(source); pm != nil {
		restricted := NewPlanPropertiesMap()
		for _, m := range members {
			props := pm.GetProperties(m)
			// A member with no entry is a HARD failure, never a skip. Java
			// COMPUTES the property per retained member and cannot be short of
			// one; Go COPIES, and a copy can miss.
			//
			// Skipping would mint a non-nil but member-short map, and
			// toPartitionsFromMap walks the MAP: the plan would then belong to
			// no partition and the alternative would vanish with nothing said.
			// That is the empty-set-reads-as-success shape, occurring inside
			// the fix for an empty-set-reads-as-success bug — so it fails loudly
			// or not at all. Measured zero firings across 15739 observed calls.
			if props == nil {
				panic(fmt.Sprintf(
					"%s: source reference has a plan-properties map but no entry for "+
						"retained member %T. Java computes the property per retained member "+
						"and cannot be short of one; copying can be, and a member-short map "+
						"makes ToPlanPartitions report NO partition for this plan — the "+
						"alternative disappears silently. Compute the source's properties "+
						"before restricting (computeRefPlanProperties), do not skip the entry",
					caller, m))
			}
			restricted.Set(m, props)
		}
		ref.SetPlanProperties(restricted)
	}
	return ref
}

// assertMembersOf enforces the precondition Java states as an assertion on
// Reference.newReferenceFromFinalMembers (Reference.java:588-589):
// `getFinalExpressions().containsAll(expressions)`. Both callers pass real
// members today, which is exactly why this is worth asserting — the invariant
// is one refactor away from being untrue and unchecked, and violating it builds
// a parent over a plan its own group never offered.
//
// Java's assert names the FINAL expressions specifically. That exact form is
// NOT portable to Go, and the reason is measured rather than argued. A probe
// counting every call to this function across the suite reported:
//
//	real planner (sqldriver, real FDB): 15147 calls, 797 pass a NON-final member
//	cascades unit fixtures:               592 calls,  11 non-final, 15 on a
//	                                                  reference still needing
//	                                                  exploration
//	both:                                          0 calls with a missing
//	                                                  property entry
//
// So `getFinalExpressions().containsAll(...)` would fire on roughly 5% of real
// planning calls. It is not a latent defect being caught: it is
// physicalMembersForParentEnumeration's documented fallback to EXPLORATORY
// members for a reference holding no finals yet — a group mid-planning, a state
// Java does not have because a Java caller always holds a PlanPartition and
// partitions are fed only by finals. The membership check is therefore against
// ALL members, which is the part of the invariant that is load-bearing: the
// parent must not range over a plan the source group does not contain. That
// form has zero violations across all 15739 observed calls.
//
// Java's second assert, `!needsExploration()`, is absent for the same measured
// reason: 15 firings, all in unit fixtures that mint groups through InitialOf
// (which leaves explorationNever set) — and zero on the real planner path. As a
// panic it would reject test fixtures rather than defects.
//
// The nil-property check inside MemoizeMemberPlansFromOther is the opposite
// case and IS a hard failure: zero firings across all 15739 calls, so it costs
// nothing today and its steady state is genuinely zero.
func assertMembersOf(caller string, source *expressions.Reference, members []expressions.RelationalExpression) {
	if source == nil || len(members) == 0 {
		return
	}
	all := source.AllMembers()
	for _, m := range members {
		found := false
		for _, s := range all {
			if s == m {
				found = true
				break
			}
		}
		if !found {
			panic(fmt.Sprintf(
				"%s: member %T is not a member of the source reference (%d members). The "+
					"restricted reference must hold a SUBSET of the group it is drawn from; a "+
					"parent built over a non-member ranges over a plan its own group never "+
					"offered (Java asserts the same precondition at Reference.java:588)",
				caller, m, len(all)))
		}
	}
}
