package properties

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ExtractBestPlan walks the Reference DAG rooted at `ref` and returns
// a fresh RelationalExpression tree where every reachable Reference
// is a singleton holding the cost-cheapest member chosen under
// DefaultStatistics. Children are extracted recursively.
//
// Use case: callers that have just run FixpointApply on a Reference
// and want to materialise a single best plan for execution / display
// / serialisation. Without this, the extracted "plan" is the
// Reference DAG with multiple alternative members still attached;
// downstream consumers have no easy way to lock in one shape.
//
// Behaviour:
//   - Returns nil if `ref` is nil or empty.
//   - The returned expression is structurally fresh — its
//     Quantifiers range over freshly-allocated single-member
//     Reference objects. Pointer identity with the input tree's
//     References is NOT preserved.
//   - Cycle-safe: a visited set guards against infinite recursion
//     through cyclic Reference DAGs (e.g. recursive CTE expression
//     trees where the RecursiveUnion's quantifiers could form
//     back-edges through the Memo).
//   - Switch-on-concrete-type for constructor dispatch — each
//     concrete RelationalExpression type the seed exposes has an
//     arm. Adding a new type requires extending this switch.
//
// Returns an error if any reachable expression is of a type
// unknown to this extractor — surfacing the missing arm rather
// than silently dropping or panicking.
//
// See ExtractBestPlanWith for the stats-bound variant.
func ExtractBestPlan(ref *expressions.Reference) (expressions.RelationalExpression, error) {
	return ExtractBestPlanWith(ref, DefaultStatistics{})
}

// ExtractBestPlanWith is ExtractBestPlan driven by a specific
// StatisticsProvider. Stats flow into per-Reference best-member
// selection so different table cardinalities can flip which
// alternative wins.
//
// Pass nil for stats to use DefaultStatistics (equivalent to
// ExtractBestPlan).
func ExtractBestPlanWith(ref *expressions.Reference, stats StatisticsProvider) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if stats == nil {
		stats = DefaultStatistics{}
	}
	return extractBestPlanWithVisited(ref, stats, make(map[*expressions.Reference]bool))
}

// extractBestPlanWithVisited is the cycle-guarded inner loop for
// ExtractBestPlanWith. The visited map prevents infinite recursion
// when the Reference DAG contains back-edges (e.g. recursive CTE
// expression trees).
func extractBestPlanWithVisited(ref *expressions.Reference, stats StatisticsProvider, visited map[*expressions.Reference]bool) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if visited[ref] {
		return nil, nil
	}
	visited[ref] = true
	less := CostLessWith(stats)
	var best expressions.RelationalExpression
	if finals := ref.FinalMembers(); len(finals) > 0 {
		best = bestFrom(finals, less)
	} else {
		best = ref.GetBest(less)
	}
	if best == nil {
		return nil, nil
	}
	return rebuildExpressionVisited(best, stats, visited)
}

// BestMemberSelector is the optional interface a planner implements
// to expose its OPTIMIZE-chosen best member per Reference.
// ExtractBestPlanFromSelector consults the selector first; falls
// back to cost-comparator selection when the selector reports no
// stored choice.
//
// Used by the cascades.Planner via ExtractBestPlanFromSelector to
// avoid recomputing CostLess for every Reference (planner's
// OPTIMIZE phase already did the work).
type BestMemberSelector interface {
	// BestMember returns the chosen best member for `ref`, or nil
	// if the selector has no stored choice.
	BestMember(ref *expressions.Reference) expressions.RelationalExpression
	// HasBestMember reports whether the selector has a stored
	// choice for `ref` (distinguishes "no choice" from "chose nil").
	HasBestMember(ref *expressions.Reference) bool
}

// ExtractBestPlanFromSelector returns a fresh tree where every
// Reference's best member comes from `sel` if available; falls
// back to CostLess+stats otherwise.
//
// Use this when the caller has a pre-populated selector (e.g. the
// cascades.Planner after Explore). It avoids repeating the OPTIMIZE
// work that already happened during Explore.
//
// Pass nil sel to fall back to ExtractBestPlanWith(ref, stats)
// (no selector path).
func ExtractBestPlanFromSelector(ref *expressions.Reference, sel BestMemberSelector, stats StatisticsProvider) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if stats == nil {
		stats = DefaultStatistics{}
	}
	return extractBestPlanFromSelectorVisited(ref, sel, stats, make(map[*expressions.Reference]bool))
}

// extractBestPlanFromSelectorVisited is the cycle-guarded inner loop
// for ExtractBestPlanFromSelector. The visited map prevents infinite
// recursion when the Reference DAG contains back-edges (e.g.
// recursive CTE expression trees where RecursiveUnion quantifiers
// form cycles through the Memo).
func extractBestPlanFromSelectorVisited(ref *expressions.Reference, sel BestMemberSelector, stats StatisticsProvider, visited map[*expressions.Reference]bool) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if visited[ref] {
		return nil, nil
	}
	visited[ref] = true

	// Per-properties winner path (Graefe 1995 §2): if the Reference
	// has a physical winner stamped for NoProperties, use it directly.
	// Non-physical winners fall through to the legacy extraction which
	// navigates FinalMembers to find the physical plan.
	if w := ref.Winner(expressions.NoProperties); w != nil && isPhysicalPlan(w) {
		return rebuildExpressionFromSelectorVisited(w, sel, stats, visited)
	}

	var best expressions.RelationalExpression
	if sel != nil && sel.HasBestMember(ref) {
		best = sel.BestMember(ref)
	}
	if !isPhysicalPlan(best) {
		if finals := ref.FinalMembers(); len(finals) > 0 {
			if fb := bestPhysicalFrom(finals); fb != nil {
				best = fb
			} else if fb := bestFrom(finals, CostLessWith(stats)); fb != nil {
				best = fb
			}
		}
	}
	if best == nil {
		best = ref.GetBest(CostLessWith(stats))
	}
	if best == nil {
		return nil, nil
	}

	// Sort elimination via per-properties winners (Graefe 1995 §2):
	// if the best expression is a LogicalSort and the child Reference
	// has a winner for the sort's ordering, skip the sort and use the
	// ordered winner directly.
	if sortExpr, ok := best.(*expressions.LogicalSortExpression); ok {
		if childWinner := sortWinnerFromChild(sortExpr, sel, stats, visited); childWinner != nil {
			return childWinner, nil
		}
	}

	return rebuildExpressionFromSelectorVisited(best, sel, stats, visited)
}

// sortWinnerFromChild checks if a LogicalSort's child Reference has
// an ordering-specific winner that satisfies the sort's keys. If yes,
// returns the rebuilt winner (sort eliminated). If no, returns nil.
func sortWinnerFromChild(sortExpr *expressions.LogicalSortExpression, sel BestMemberSelector, stats StatisticsProvider, visited map[*expressions.Reference]bool) expressions.RelationalExpression {
	childRef := sortExpr.GetInner().GetRangesOver()
	if childRef == nil {
		return nil
	}
	sortKeys := sortExpr.GetSortKeys()
	if len(sortKeys) == 0 {
		return nil
	}
	requiredProps := expressions.OrderingFromSortKeys(sortKeys)
	if requiredProps.IsEmpty() {
		return nil
	}
	winner := childRef.Winner(requiredProps)
	if winner == nil || !isPhysicalPlan(winner) {
		return nil
	}
	rebuilt, err := rebuildExpressionFromSelectorVisited(winner, sel, stats, visited)
	if err != nil {
		return nil
	}
	return rebuilt
}

// rebuildExpressionFromSelectorVisited is the same switch-based
// rebuilder as rebuildExpression but recurses through
// extractBestPlanFromSelectorVisited to consult the selector at
// every Reference, with cycle detection.
func rebuildExpressionFromSelectorVisited(e expressions.RelationalExpression, sel BestMemberSelector, stats StatisticsProvider, visited map[*expressions.Reference]bool) (expressions.RelationalExpression, error) {
	if e == nil {
		return nil, nil
	}
	freshChildren := make([]expressions.Quantifier, 0, len(e.GetQuantifiers()))
	for _, q := range e.GetQuantifiers() {
		inner, err := extractBestPlanFromSelectorVisited(q.GetRangesOver(), sel, stats, visited)
		if err != nil {
			return nil, err
		}
		var freshRef *expressions.Reference
		if inner == nil {
			freshRef = &expressions.Reference{}
		} else {
			freshRef = expressions.InitialOf(inner)
		}
		freshChildren = append(freshChildren, expressions.ForEachQuantifier(freshRef))
	}
	return rebuildWithFreshChildren(e, freshChildren)
}

func bestFrom(members []expressions.RelationalExpression, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	if len(members) == 0 {
		return nil
	}
	best := members[0]
	for _, m := range members[1:] {
		if less(m, best) {
			best = m
		}
	}
	return best
}

// rebuildExpressionVisited returns a fresh RelationalExpression of the
// same concrete type as `e`, with each Quantifier's Reference replaced
// by a singleton Reference holding the recursively-extracted best plan
// of the original Reference under `stats`. The visited map provides
// cycle detection.
func rebuildExpressionVisited(e expressions.RelationalExpression, stats StatisticsProvider, visited map[*expressions.Reference]bool) (expressions.RelationalExpression, error) {
	if e == nil {
		return nil, nil
	}
	// Recurse into each Quantifier first — collect fresh
	// Quantifiers for the new expression's children.
	freshChildren := make([]expressions.Quantifier, 0, len(e.GetQuantifiers()))
	for _, q := range e.GetQuantifiers() {
		inner, err := extractBestPlanWithVisited(q.GetRangesOver(), stats, visited)
		if err != nil {
			return nil, err
		}
		var freshRef *expressions.Reference
		if inner == nil {
			// Empty / nil inner Reference — shouldn't happen for a
			// valid plan, but defensive: keep the Quantifier shape
			// with an empty Reference. Caller can detect via
			// ref.Members().
			freshRef = &expressions.Reference{}
		} else {
			freshRef = expressions.InitialOf(inner)
		}
		freshChildren = append(freshChildren, expressions.ForEachQuantifier(freshRef))
	}
	return rebuildWithFreshChildren(e, freshChildren)
}

// rebuildWithFreshChildren is the switch-on-type rebuilder shared
// by rebuildExpression and rebuildExpressionFromSelector.
func rebuildWithFreshChildren(e expressions.RelationalExpression, freshChildren []expressions.Quantifier) (expressions.RelationalExpression, error) {
	switch ex := e.(type) {

	case *expressions.FullUnorderedScanExpression:
		// Leaf — no children, return a fresh scan with the same
		// record-type set + flowed Type.
		return expressions.NewFullUnorderedScanExpression(
			ex.GetRecordTypes(), ex.GetFlowedType(),
		), nil

	case *expressions.LogicalFilterExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalFilterExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalFilterExpression(
			ex.GetPredicates(), freshChildren[0],
		), nil

	case *expressions.LogicalProjectionExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalProjectionExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalProjectionExpression(
			ex.GetProjectedValues(), freshChildren[0],
		), nil

	case *expressions.LogicalSortExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalSortExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalSortExpression(
			ex.GetSortKeys(), freshChildren[0],
		), nil

	case *expressions.LogicalDistinctExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalDistinctExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalDistinctExpression(freshChildren[0]), nil

	case *expressions.LogicalTypeFilterExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalTypeFilterExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalTypeFilterExpression(
			ex.GetRecordTypes(), freshChildren[0],
		), nil

	case *expressions.LogicalUnionExpression:
		return expressions.NewLogicalUnionExpression(freshChildren), nil

	case *expressions.LogicalIntersectionExpression:
		return expressions.NewLogicalIntersectionExpression(
			freshChildren, ex.GetComparisonKeyValues(),
		), nil

	case *expressions.SelectExpression:
		return expressions.NewSelectExpressionWithJoinType(
			ex.GetResultValue(), freshChildren, ex.GetPredicates(),
			ex.GetSourceAliases(), ex.GetJoinType(),
		), nil

	case *expressions.InsertExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("InsertExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewInsertExpression(
			freshChildren[0], ex.GetTargetRecordType(), ex.GetTargetType(),
		), nil

	case *expressions.UpdateExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("UpdateExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewUpdateExpression(
			freshChildren[0], ex.GetTargetRecordType(), ex.GetTransforms(),
		), nil

	case *expressions.DeleteExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("DeleteExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewDeleteExpression(
			freshChildren[0], ex.GetTargetRecordType(),
		), nil

	default:
		// Unknown concrete type — try the optional WithChildren
		// interface; if implemented, use it to rebuild with fresh
		// child Quantifiers (preserves the strict singleton
		// invariant). Otherwise fall back to opaque-passthrough,
		// which keeps the pipeline running but loses the singleton
		// guarantee for that subtree.
		if rebuilder, ok := e.(WithChildren); ok {
			return rebuilder.WithChildren(freshChildren)
		}
		return e, nil
	}
}

// WithChildren is the optional interface a RelationalExpression
// implements to support generic plan extraction. ExtractBestPlan's
// default arm calls WithChildren(freshChildren) to construct a
// fresh expression of the same concrete type with the supplied
// quantifiers.
//
// Concrete RelationalExpression types in the seed do NOT implement
// WithChildren — their constructors take operator-specific args
// (predicates, sort keys, etc.) that the generic rebuilder doesn't
// have. Instead, ExtractBestPlan has explicit switch arms for those
// types. The interface exists for OPAQUE wrappers (e.g. cascades-
// internal physical-plan adapters) that want to participate in
// extraction without forcing a switch-arm extension.
//
// Mirrors Java's `RelationalExpressionWithChildren.withNewChildren`
// for the seed-relevant subset.
type WithChildren interface {
	// WithChildren returns a fresh expression of the same concrete
	// type, using the supplied quantifiers in place of the originals.
	// Returns an error if the quantifier count or shape doesn't match
	// what the type expects.
	WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error)
}

type physicalPlanHolder interface {
	GetRecordQueryPlan() plans.RecordQueryPlan
}

func isPhysicalPlan(e expressions.RelationalExpression) bool {
	if e == nil {
		return false
	}
	_, ok := e.(physicalPlanHolder)
	return ok
}

func bestPhysicalFrom(members []expressions.RelationalExpression) expressions.RelationalExpression {
	for _, m := range members {
		if isPhysicalPlan(m) {
			return m
		}
	}
	return nil
}
