package plans

import (
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
// Deliberately a zero-size struct: embedding it must not change any plan's
// memory layout, its construction, or its hash.
type PlanExprBase struct{}

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

// GetResultValue returns a value standing for the rows this plan emits.
//
// Matches what the physical wrappers return — a fresh QuantifiedObjectValue —
// so behaviour is unchanged as the wrapper layer is retired. The rich row
// TYPE remains available through GetResultType.
//
// FOUR PLANS SHADOW THIS with their own resultValue field: Map, FlatMap,
// NestedLoopJoin and MultiIntersectionOnValues. That is the richer answer and
// matches Java (RecordQueryFlatMapPlan.getResultValue returns the result
// value), but those fields are unguarded constructor parameters, and the
// surrounding code nil-checks them (map.go, multi_intersection.go), so nil
// looks reachable — a hazard once step 2 starts dereferencing this while
// traversing plans as expressions.
//
// Measured before relying on it: instrumenting all four to report a nil
// resultValue found ZERO occurrences across the 2407-query corpus and the
// entire Go test suite. The nil checks are defensive, not reachable, so no
// guard is added here — one that never fires would be noise, and papering
// over the accessor is the wrong layer anyway. If step 2 ever needs the
// invariant enforced, the place is construction (Java's resultValue is
// @Nonnull), not this method.
//
// One asymmetry to carry into step 2: physicalMultiIntersectionWrapper's
// nil-fallback returns the FIRST INNER's flowed object value, not a fresh
// stand-in — so a blanket non-nil guard here would silently change that
// wrapper's answer. The plan cannot reproduce that fallback yet because it
// has no quantifiers; that is precisely what step 2 fixes.
func (PlanExprBase) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
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
// Java can say getOnlyElement because a group is pruned to one final
// expression by the time anything dereferences it. That property holds here
// too — verified across the whole corpus and pinned by
// TestOneFinalPlanPerReference — which is what makes this step possible at
// all; without it a quantifier would have no single answer and the parent
// would need a second stored pointer, which is exactly the duplication that
// produced the nil-inner shell class.
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
	if p := planFromMembers(ref.FinalMembers()); p != nil {
		return p
	}
	return planFromMembers(ref.Members())
}

func planFromMembers(members []expressions.RelationalExpression) RecordQueryPlan {
	for _, m := range members {
		if p, ok := m.(RecordQueryPlan); ok {
			return p
		}
		if w, ok := m.(interface{ GetRecordQueryPlan() RecordQueryPlan }); ok {
			if p := w.GetRecordQueryPlan(); p != nil {
				return p
			}
		}
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
