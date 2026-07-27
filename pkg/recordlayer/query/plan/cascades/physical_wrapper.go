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

// valueCorrelationsNoParams returns the outer correlations a value references with
// query-parameter (ConstantObjectValue) aliases subtracted — the value-tree twin of the
// param exclusion scanComparisonCorrelations applies to a SARG comparand.
func valueCorrelationsNoParams(v values.Value) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	if v == nil {
		return out
	}
	for a := range values.GetCorrelatedToOfValue(v) {
		out[a] = struct{}{}
	}
	values.WalkValue(v, func(node values.Value) bool {
		if cov, ok := node.(*values.ConstantObjectValue); ok {
			delete(out, cov.Alias)
		}
		return true
	})
	return out
}

// dataAccessExprCorrelations collects the COMPLETE set of outer correlations a
// data-access subplan reports — SARG comparands (scan/index), residual filter
// predicates, and map result values — query-parameter aliases subtracted. Used by the
// plan-backed leaf expression scanPlanExpression so a SARGed PK-scan probe
// (`pk = QOV(outer).fk`) and an RFC-153 buried-merge-rebased FlatMap inner report the
// correlations their executable plan actually carries, instead of nil/stale (an
// under-reported correlation lets join-leg / winner / root bookkeeping treat a
// correlated probe as self-contained — a latent planning hazard, the same
// incomplete-coverage family as the fail-open verifier). Complete for the data-access
// node shapes scanPlanExpression wraps (a single PK scan; a fully-rebased recognized
// inner — the verifier declines any inner with an unrecognized node, so a rebased inner
// reaching this point contains only scan/index/filter/map/pass-through nodes).
func dataAccessExprCorrelations(p plans.RecordQueryPlan) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	if p == nil {
		return out
	}
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		switch sp := n.(type) {
		case *plans.RecordQueryScanPlan:
			for a := range scanComparisonCorrelations(sp.GetScanComparisons()) {
				out[a] = struct{}{}
			}
		case *plans.RecordQueryIndexPlan:
			for a := range scanComparisonCorrelations(sp.GetScanComparisons()) {
				out[a] = struct{}{}
			}
		case *plans.RecordQueryPredicatesFilterPlan:
			for _, pr := range sp.GetPredicates() {
				c := map[values.CorrelationIdentifier]struct{}{}
				for a := range predicates.GetCorrelatedToOfPredicate(pr) {
					c[a] = struct{}{}
				}
				deletePredicateConstantObjectAliases(pr, c)
				for a := range c {
					out[a] = struct{}{}
				}
			}
		case *plans.RecordQueryFilterPlan:
			for _, pr := range sp.GetPredicates() {
				c := map[values.CorrelationIdentifier]struct{}{}
				for a := range predicates.GetCorrelatedToOfPredicate(pr) {
					c[a] = struct{}{}
				}
				deletePredicateConstantObjectAliases(pr, c)
				for a := range c {
					out[a] = struct{}{}
				}
			}
		case *plans.RecordQueryMapPlan:
			for a := range valueCorrelationsNoParams(sp.GetResultValue()) {
				out[a] = struct{}{}
			}
		}
		return true
	})
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
// scan. Since RFC-184 W2 the memo holds *plans.RecordQueryIndexPlan directly (no
// physicalIndexScanWrapper), so this is a bare type check.
func IsPhysicalIndexScan(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryIndexPlan)
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

// findPhysicalExpr returns a physical-plan expression from ref, FINAL members
// first. Used by implement rules to obtain the existing wrapper (already
// memoized in the inner Reference by a prior implement-rule fire) without
// re-wrapping from scratch.
//
// See findPhysicalPlan for why the final set is searched first and why the
// exploratory fallback stays.
//
// The exploratory fallback still matters, but for a different reason than it
// used to. MemoizeFinalExpression now genuinely lands plans in the FINAL set
// (FinalOfAtStage), so the finals-first loop below is live at the
// push-through sites rather than inert. What the fallback covers is rules
// calling this MID-PLANNING, before a group has been finalized.
//
// A finals-ONLY tightening here is still not safe, and the reason is
// measured: at these call sites 3821 references have ZERO final members
// (against 2744 with exactly one), so refusing the exploratory set would make
// a large fraction of rules silently decline. That number is also why P5's
// terminal form is not reachable yet — see below.
//
// P5 BLOCKER, quantified. Java dereferences a quantifier straight to its plan:
// Quantifier.Physical.getRangesOverPlan() is
// Iterables.getOnlyElement(getRangesOver().getFinalExpressions()) — it REQUIRES
// exactly one final expression. Go does not have that property: 1186
// references here hold multiple finals (max 52), and 1125 hold multiple
// PHYSICAL finals, so getOnlyElement would throw on every one. Making plans
// hold quantifiers is therefore gated on universal prune-to-one-final-member,
// which unified_tasks.go's stage-boundary arm records was ATTEMPTED AND
// REVERTED: it lost canonical alternatives Go's PLANNING cannot re-derive
// (RFC-153 buried-leg, cross-join-EXISTS). The root gate is Java per-phase
// rule-set parity, tracked in DIVERGENCES.md.
//
// DO NOT make this cost-ranked. Picking the "cheapest" member here looks like
// an obvious improvement, but it is wrong, and measurably so: ranking with
// PlanningCostModelLess ignores the REQUESTED ORDERING, so a rule asking for a
// child gets whichever member is cheapest rather than one that satisfies the
// ordering its parent needs. Tried, and it turned `SELECT a, b FROM ab WHERE
// a = 1 ORDER BY a DESC` into an ASCENDING result (pinned by yamsql
// order_by_elimination#36) and moved 79 plan shapes.
//
// Cost is the memo's job, not a rule's. A rule wants A VALID CHILD; which
// alternative wins is decided by OptimizeGroup under the ordering constraints,
// and extraction then reads that winner through the ordering-aware winner
// lookup. A cost comparison at rule time is a second, ordering-blind optimizer
// running outside the cost framework.
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
