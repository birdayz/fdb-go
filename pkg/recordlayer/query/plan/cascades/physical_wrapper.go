// Portions derived from FoundationDB Record Layer (RecordQueryScanPlan.java,
// RecordQueryPlanMatchers.java, PushFilterThroughFetchRule.java,
// ScanComparisons.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"encoding/binary"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// scanComparisonCorrelations returns the union of outer correlations referenced
// by the comparands of a set of scan ComparisonRanges — the correlations a
// physical (index or primary) scan probe carries. A correlated probe
// (`col = QOV(outer).x`) reports the outer alias; a literal/parameter range
// reports nothing. Used by the physical scan wrappers'
// GetCorrelatedToWithoutChildren (RFC-150 Phase-2b D.2): the data-access path
// can SARG a join predicate into a bare correlated PHYSICAL scan (no residual
// filter to carry the correlation), so unless the scan itself reports it, the
// physical probe looks uncorrelated and B1 join-leg detection / winner-stamping
// would mis-treat it. Java's RecordQueryScanPlan derives correlatedTo from its
// ScanComparisons the same way.
func scanComparisonCorrelations(comps []*predicates.ComparisonRange) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	collect := func(c *predicates.Comparison) {
		if c == nil || c.Operand == nil {
			return
		}
		for a := range values.GetCorrelatedToOfValue(c.Operand) {
			out[a] = struct{}{}
		}
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

// physicalPlanExpression is implemented by all physical-plan wrapper
// types. Lets implement rules discover physical plans in a Reference
// with a single interface assertion instead of per-type switches.
type physicalPlanExpression interface {
	expressions.RelationalExpression
	GetRecordQueryPlan() plans.RecordQueryPlan
}

// IsPhysicalIndexScan reports whether the given RelationalExpression is an index
// scan. Since RFC-184 W2 the memo holds the plan directly (no
// physicalIndexScanWrapper), so this is a type check on the plan.
//
// A COVERING index scan answers TRUE. It is an index scan — it reads the same
// physical index range and differs only in the row it reconstructs — and since
// RFC-220 it is the shape the memo actually holds for an index-backed access.
// A predicate named "is an index scan" that answered FALSE for the only index
// scan in the memo would be a trap for every caller.
func IsPhysicalIndexScan(expr expressions.RelationalExpression) bool {
	plan, isPlan := expr.(plans.RecordQueryPlan)
	if !isPlan {
		return false
	}
	_, ok := plans.IndexPlanOf(plan)
	return ok
}

// IsPhysicalIntersection reports whether the given RelationalExpression is an
// intersection. Since RFC-184 W2 the memo holds *plans.RecordQueryIntersectionPlan
// directly (no physicalIntersectionWrapper), so this is a bare type check.
func IsPhysicalIntersection(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryIntersectionPlan)
	return ok
}

// IsPhysicalMultiIntersection reports whether the given RelationalExpression is a
// multi-intersection. Since RFC-184 W2 the memo holds
// *plans.RecordQueryMultiIntersectionOnValuesPlan directly (no
// physicalMultiIntersectionWrapper), so this is a bare type check.
func IsPhysicalMultiIntersection(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryMultiIntersectionOnValuesPlan)
	return ok
}

// GetPhysicalMultiIntersectionPlan returns the plan if expr is a
// multi-intersection, nil otherwise. Since RFC-184 W2 the memo holds the bare
// plan, so this is a bare type assertion.
func GetPhysicalMultiIntersectionPlan(expr expressions.RelationalExpression) *plans.RecordQueryMultiIntersectionOnValuesPlan {
	p, ok := expr.(*plans.RecordQueryMultiIntersectionOnValuesPlan)
	if !ok {
		return nil
	}
	return p
}

// IsPhysicalFilter reports whether the given RelationalExpression is a physical
// predicates filter. Since RFC-184 W2 the memo holds
// *plans.RecordQueryPredicatesFilterPlan directly (no
// physicalPredicatesFilterWrapper), so this is a bare type check.
func IsPhysicalFilter(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryPredicatesFilterPlan)
	return ok
}

// IsPhysicalInsert reports whether the given RelationalExpression is an INSERT
// plan. Since RFC-184 W2 the memo holds *plans.RecordQueryInsertPlan directly
// (no physicalInsertWrapper), so this is a bare type check.
func IsPhysicalInsert(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryInsertPlan)
	return ok
}

// IsPhysicalDelete reports whether the given RelationalExpression is a DELETE
// plan. Since RFC-184 W2 the memo holds *plans.RecordQueryDeletePlan directly
// (no physicalDeleteWrapper), so this is a bare type check.
func IsPhysicalDelete(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryDeletePlan)
	return ok
}

// IsPhysicalUpdate reports whether the given RelationalExpression is an UPDATE
// plan. Since RFC-184 W2 the memo holds *plans.RecordQueryUpdatePlan directly
// (no physicalUpdateWrapper), so this is a bare type check.
func IsPhysicalUpdate(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryUpdatePlan)
	return ok
}

// IsPhysicalPredicatesFilter reports whether the given expression is a physical
// predicates filter. Since RFC-184 W2 the memo holds
// *plans.RecordQueryPredicatesFilterPlan directly (no
// physicalPredicatesFilterWrapper), so this is a bare type check.
func IsPhysicalPredicatesFilter(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryPredicatesFilterPlan)
	return ok
}

// IsPhysicalMap reports whether the given expression is a physical map. Since
// RFC-184 W2 the memo holds *plans.RecordQueryMapPlan directly (no
// physicalMapWrapper), so this is a bare type check on the plan.
func IsPhysicalMap(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryMapPlan)
	return ok
}

// IsPhysicalFetchFromPartialRecord reports whether the given
// RelationalExpression is a physical fetch. Since RFC-184 W2 the memo holds
// *plans.RecordQueryFetchFromPartialRecordPlan directly (no
// physicalFetchFromPartialRecordWrapper), so this is a bare type check on the
// plan.
func IsPhysicalFetchFromPartialRecord(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryFetchFromPartialRecordPlan)
	return ok
}

// IsPhysicalInJoin reports whether the given expression is an InJoin. Since
// RFC-184 W2 the memo holds *plans.RecordQueryInJoinPlan directly (no
// physicalInJoinWrapper), so this is a bare type check.
func IsPhysicalInJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryInJoinPlan)
	return ok
}

// IsPhysicalRecursiveLevelUnion reports whether the given RelationalExpression is
// a recursive level union. Since RFC-184 W2 the memo holds
// *plans.RecordQueryRecursiveLevelUnionPlan directly (no
// physicalRecursiveLevelUnionWrapper), so this is a bare type check.
func IsPhysicalRecursiveLevelUnion(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryRecursiveLevelUnionPlan)
	return ok
}

// IsPhysicalRecursiveDfsJoin reports whether the given RelationalExpression is a
// recursive DFS join. Since RFC-184 W2 the memo holds
// *plans.RecordQueryRecursiveDfsJoinPlan directly (no
// physicalRecursiveDfsJoinWrapper), so this is a bare type check.
func IsPhysicalRecursiveDfsJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryRecursiveDfsJoinPlan)
	return ok
}

// ExplainPhysicalPlan returns the Explain() string for a physical-plan
// expression, or empty string if the expression is not a physical plan.
func ExplainPhysicalPlan(expr expressions.RelationalExpression) string {
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return ""
	}
	p := ph.GetRecordQueryPlan()
	if p == nil {
		return ""
	}
	return p.Explain()
}

// findPhysicalPlan returns a physical member's underlying RecordQueryPlan, or
// nil if no physical plan has been yielded into ref yet.
//
// FINAL members are searched first. Java enumerates FINAL expressions only when
// it looks for a plan (RecordQueryPlanMatchers.java:115) — a plan is by
// definition a final expression there. Go's AllMembers() concatenates
// exploratory members BEFORE final ones (Reference.AllMembers), so a bare
// first-match scan over it inspects the exploratory set first and can return a
// promoted-but-dominated expression instead of the group's plan.
// FinalizeExpressionsRule promotes the SAME pointer into both sets, so an
// expression really can sit in each.
//
// The exploratory fallback is kept deliberately: rules call this DURING
// planning, before a group has been finalized, and returning nil there would
// silently decline a rule that has a perfectly good child to hand.
func findPhysicalPlan(ref *expressions.Reference) plans.RecordQueryPlan {
	if expr := findPhysicalExpr(ref); expr != nil {
		if ph, ok := expr.(physicalPlanExpression); ok {
			return ph.GetRecordQueryPlan()
		}
	}
	return nil
}

// physicalMembersForParentEnumeration returns EVERY physical member of ref that
// a parent construction should be fired over — the cardinality answer to
// findPhysicalExpr's single pick.
//
// Java fires an implementation rule once per (parent, child-member) pair, so a
// parent is built over every alternative its child offers and the memo's cost
// framework chooses among the resulting parents. Go's single pick meant a child
// member that was not the local winner was never lifted into a parent
// alternative — invisible to cost, because it was never constructed. Measured
// on RFC-220's defect query: a group held [FETCH, INDEX] together and only
// Filter(Index) was ever built.
//
// Same member-set policy as findPhysicalExpr: FINAL members first, exploratory
// as a deliberate fallback for rules firing mid-planning (see that function for
// why a finals-only tightening is not safe — 3821 references at these sites have
// zero finals).
//
// Order is preserved, NOT ranked. Ranking here would be the ordering-blind
// second optimizer findPhysicalExpr's comment forbids; the point of enumerating
// is that no choice is made at rule time at all.
//
// The resulting cross product is NOT capped per rule — references here hold up
// to 52 physical finals, and a caller looping over them yields once per member
// inside a SINGLE OnMatch. MaxNumMatchesPerRuleCall does not apply: its counter
// counts BINDINGS produced by the matcher (unified_tasks.go:350, :461, :560),
// and this loop produces none. The operative backstop is the much coarser
// Planner.MaxTasks / MaxTaskQueueSize, which fails the WHOLE plan with
// ErrPlannerCapHit rather than capping one rule's fan-out. Memoization keeps the
// enumerated subtrees singly represented, so the fan-out is in parent
// alternatives rather than in duplicated trees — but a caller adding a second
// enumerated child multiplies, and nothing here will stop it.
func physicalMembersForParentEnumeration(ref *expressions.Reference) []expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	var out []expressions.RelationalExpression
	for _, m := range ref.FinalMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, m := range ref.Members() {
		if _, ok := m.(physicalPlanExpression); ok {
			out = append(out, m)
		}
	}
	return out
}

// findPhysicalExpr returns a physical-plan expression from ref, FINAL members
// first. Used by implement rules to obtain the existing wrapper (already
// memoized in the inner Reference by a prior implement-rule fire) without
// re-wrapping from scratch.
//
// See findPhysicalPlan for why the final set is searched first and why the
// exploratory fallback stays.
//
// The exploratory fallback serves rules running before a group is finalized.
// Go retains alternatives with distinct physical properties (RFC-224), rather
// than requiring Java's singleton final set.
//
// Do not cost-rank this lookup: PlanningCostModelLess alone ignores requested
// ordering and can select an ascending child for ORDER BY ... DESC. OptimizeGroup
// chooses the winner under ordering constraints; extraction uses that winner.
func findPhysicalExpr(ref *expressions.Reference) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	for _, m := range ref.FinalMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	for _, m := range ref.Members() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	return nil
}

// bakedInnerPlan returns the concrete RecordQueryPlan that expr carries, for
// use as a parent's child AT RULE TIME.
//
// Java constructs a parent only from an already-memoized child: in
// PushFilterThroughFetchRule.java:197-225 the
// `Quantifier.physical(call.memoizePlan(innerPlan))` is evaluated as a
// constructor ARGUMENT, so no window exists in which a parent lacks its
// child. Go's rules must do the same — pass the child plan, never nil.
//
// Returns nil when expr carries no plan, or carries a structurally
// incomplete one. A rule that gets nil here declines to fire rather than
// yielding a plan with a hole in it; the alternative is the shell that
// extraction then has to repair.
func bakedInnerPlan(expr expressions.RelationalExpression) plans.RecordQueryPlan {
	pe, ok := expr.(physicalPlanExpression)
	if !ok {
		return nil
	}
	p := pe.GetRecordQueryPlan()
	// Structurally incomplete = a non-leaf carrying no children, per the
	// plan-invariant authority (isGenuineLeafPlan).
	if p == nil || (len(p.GetChildren()) == 0 && !isGenuineLeafPlan(p)) {
		return nil
	}
	return p
}

// writeHash64 writes a uint64 to the FNV hasher in big-endian byte order.
// Used by the data-access rule to fold a plan's HashCodeWithoutChildren into
// its structural hash.
func writeHash64(h hashWriter, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = h.Write(b[:])
}

// hashWriter is the minimal io.Writer surface fnv.New64a() returns.
type hashWriter interface {
	Write(p []byte) (n int, err error)
}

// physicalWrapperCostMultiplier is applied to each physical wrapper's
// inherited cost so cost-driven extraction prefers physical plans
// over their logical counterparts. 0.9 = "physical is 10% cheaper
// than logical" — enough to flip ordering on equally-shaped
// alternatives, small enough not to dominate the cost comparison
// with structurally-different alternatives.
const physicalWrapperCostMultiplier = properties.PhysicalWrapperCostMultiplier

// IsPhysicalAggregateIndex reports whether the expression is an aggregate index
// scan. Since RFC-184 W2 the memo holds *plans.RecordQueryAggregateIndexPlan
// directly (no physicalAggregateIndexWrapper), so this is a bare type check.
func IsPhysicalAggregateIndex(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryAggregateIndexPlan)
	return ok
}

// IsPhysicalStreamingAgg reports whether the given RelationalExpression is a
// physical streaming aggregation. Since RFC-184 W2 the memo holds
// *plans.RecordQueryStreamingAggregationPlan directly (no
// physicalStreamingAggWrapper), so this is a bare type check.
func IsPhysicalStreamingAgg(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryStreamingAggregationPlan)
	return ok
}

// IsPhysicalFlatMap reports whether the given RelationalExpression is a physical
// correlated FlatMap. Since RFC-184 W2 the memo holds
// *plans.RecordQueryFlatMapPlan directly (no physicalFlatMapWrapper), so this is
// a bare type check.
func IsPhysicalFlatMap(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryFlatMapPlan)
	return ok
}

// IsPhysicalNestedLoopJoin reports whether the given RelationalExpression is a
// physical nested-loop join. Since RFC-184 W2 the memo holds
// *plans.RecordQueryNestedLoopJoinPlan directly (no
// physicalNestedLoopJoinWrapper), so this is a bare type check.
func IsPhysicalNestedLoopJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryNestedLoopJoinPlan)
	return ok
}
