package cascades

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ExtractBestPlan walks the Reference DAG rooted at `ref` and returns
// a fresh RelationalExpression tree where every reachable Reference
// is a singleton holding the cost-cheapest member chosen under
// DefaultStatistics. Children are extracted recursively.
//
// Use case: callers holding an explored Reference (rule-generated
// alternatives attached) who want to materialise a single best plan
// for execution / display / serialisation. Without this, the
// extracted "plan" is the Reference DAG with multiple alternative
// members still attached; downstream consumers have no easy way to
// lock in one shape.
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
//     concrete RelationalExpression type has an arm. Adding a new
//     type requires extending this switch.
//
// Returns an error if any reachable expression is of a type
// unknown to this extractor — surfacing the missing arm rather
// than silently dropping or panicking.
//
// See ExtractBestPlanWith for the stats-bound variant.
func ExtractBestPlan(ref *expressions.Reference) (expressions.RelationalExpression, error) {
	return ExtractBestPlanWith(ref, properties.DefaultStatistics{})
}

// ExtractBestPlanWith is ExtractBestPlan driven by a specific
// StatisticsProvider. Stats flow into per-Reference best-member
// selection so different table cardinalities can flip which
// alternative wins.
//
// Pass nil for stats to use DefaultStatistics (equivalent to
// ExtractBestPlan).
func ExtractBestPlanWith(ref *expressions.Reference, stats properties.StatisticsProvider) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	return extractBestPlanWithVisited(ref, stats, make(map[*expressions.Reference]bool))
}

// extractBestPlanWithVisited is the cycle-guarded inner loop for
// ExtractBestPlanWith. The visited map prevents infinite recursion
// when the Reference DAG contains back-edges (e.g. recursive CTE
// expression trees).
func extractBestPlanWithVisited(ref *expressions.Reference, stats properties.StatisticsProvider, visited map[*expressions.Reference]bool) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if visited[ref] {
		return nil, nil
	}
	// Stack-scoped, not permanent: mark the ref while its subtree is on the
	// recursion stack (so a back-edge — recursive-CTE cycle — still short-
	// circuits above), then unmark on the way up. A permanent mark also
	// de-dups a SHARED sub-DAG, returning nil the second time the same ref is
	// reached as a legitimately-separate child (e.g. two UNION legs both
	// scanning t1) — which drops that child. Extraction produces a TREE from a
	// memoized DAG; each reference must re-extract. Mirrors extractTieBreakHash.
	visited[ref] = true
	defer delete(visited, ref)
	best := ref.GetBest(tieBrokenLess(properties.CostLessWith(stats)))
	if best == nil {
		return nil, nil
	}
	return rebuildExpressionVisited(best, stats, visited)
}

// tieBrokenLess wraps a cost comparator into a TOTAL order for the
// selector-less extraction path (ExtractBestPlan / ExtractBestPlanWith).
// GetBest iterates the members slice, so a merely cost-partial comparator
// lets the winner depend on member INSERTION order — the same
// nondeterminism class the planner's selector path closes with its
// plan-hash tie-break (PlanningCostModel criterion #17, costLessFor).
// This package cannot reach the planner's physical plan hash (layering),
// so ties resolve by extractTieBreakHash — structural, expressions-only,
// and member-order invariant.
func tieBrokenLess(less func(a, b expressions.RelationalExpression) bool) func(a, b expressions.RelationalExpression) bool {
	return func(a, b expressions.RelationalExpression) bool {
		// The recursive-CTE operator choice (RecursiveDfsJoin's charge-once vs
		// RecursiveLevelUnion's double-charge) is a STRUCTURAL memory-safety
		// precedence, not a cost margin — DFS must be preferred BEFORE cost is
		// consulted, so the preference survives cost-model changes (a
		// cardinality-proportional cost margin washes out once low-cardinality
		// point-lookup recursion legs are costed correctly). This mirrors the
		// OPTIMIZE path (planningCostModelCompareWith consults compareRecursiveCTE
		// first). Inert for every non-recursive pair — compareRecursiveCTE
		// returns 0 unless both sides are recursive-CTE physical plans, which
		// co-occur in exactly one memo group.
		if c := compareRecursiveCTE(a, b); c != 0 {
			return c < 0
		}
		if less(a, b) {
			return true
		}
		if less(b, a) {
			return false
		}
		ha := extractTieBreakHash(a, map[*expressions.Reference]bool{})
		hb := extractTieBreakHash(b, map[*expressions.Reference]bool{})
		if ha != hb {
			return ha < hb
		}
		// HashCodeWithoutChildren can be DELIBERATELY coarser than
		// structural equality (the scan hash is names-only so wildcard
		// matching buckets typed and untyped scans together) — two
		// structurally distinct members can hash equal. Fall back to the
		// flowed RESULT TYPE's stable SQL rendering, which carries
		// exactly the content such hashes omit. Members equal on hash
		// AND type rendering expose identical equality-visible content —
		// the memo would have merged genuinely equal ones.
		return extractTieBreakTypeKey(a) < extractTieBreakTypeKey(b)
	}
}

// extractTieBreakTypeKey renders an expression's flowed result type as
// the deterministic secondary tie-break key. NOT the result value's
// explain rendering — GetResultValue can mint per-call unique
// correlation identifiers, which would poison determinism.
func extractTieBreakTypeKey(e expressions.RelationalExpression) string {
	rv := e.GetResultValue()
	if rv == nil {
		return ""
	}
	if t := rv.Type(); t != nil {
		return t.String()
	}
	return ""
}

// extractTieBreakHash is a deterministic structural hash over the
// expression DAG: the node's schema-neutral tie-break hash folded
// ORDER-SENSITIVELY over each quantifier's reference hash (quantifier order is
// structural), where a reference hashes as the COMMUTATIVE (XOR) fold of its
// members — so the value never depends on member insertion order. Memo identity
// remains schema-aware. Back-edges (recursive CTE references) are skipped via
// the visited guard.
func extractTieBreakHash(e expressions.RelationalExpression, visited map[*expressions.Reference]bool) uint64 {
	if e == nil {
		return 0
	}
	h := tieBreakNodeHash(e)
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil || visited[ref] {
			continue
		}
		visited[ref] = true
		var rh uint64
		for _, m := range ref.AllMembers() {
			rh ^= extractTieBreakHash(m, visited)
		}
		delete(visited, ref)
		h = h*0x100000001b3 ^ (rh*0x517cc1b727220a95 + 0x6c62272e07bb0142)
	}
	return h
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
// cascades.Planner after PlanWithContext). It avoids repeating the OPTIMIZE
// work that already happened during planning.
//
// Pass nil sel to fall back to ExtractBestPlanWith(ref, stats)
// (no selector path).
func ExtractBestPlanFromSelector(ref *expressions.Reference, sel BestMemberSelector, stats properties.StatisticsProvider) (expressions.RelationalExpression, error) {
	return ExtractBestPlanFromSelectorContext(context.Background(), ref, sel, stats)
}

// ExtractBestPlanFromSelectorContext is the cancelable form of
// ExtractBestPlanFromSelector. It polls ctx throughout recursive extraction
// so a canceled planning run does not continue rebuilding a large plan DAG.
func ExtractBestPlanFromSelectorContext(
	ctx context.Context,
	ref *expressions.Reference,
	sel BestMemberSelector,
	stats properties.StatisticsProvider,
) (expressions.RelationalExpression, error) {
	if ctx == nil {
		return nil, ErrPlannerNilContext
	}
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	return extractBestPlanFromSelectorVisited(ctx, ref, sel, stats, make(map[*expressions.Reference]bool))
}

// extractBestPlanFromSelectorVisited is the cycle-guarded inner loop
// for ExtractBestPlanFromSelector. The visited map prevents infinite
// recursion when the Reference DAG contains back-edges (e.g.
// recursive CTE expression trees where RecursiveUnion quantifiers
// form cycles through the Memo).
func extractBestPlanFromSelectorVisited(
	ctx context.Context,
	ref *expressions.Reference,
	sel BestMemberSelector,
	stats properties.StatisticsProvider,
	visited map[*expressions.Reference]bool,
) (expressions.RelationalExpression, error) {
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	if ref == nil || len(ref.AllMembers()) == 0 {
		return nil, nil
	}
	if visited[ref] {
		return nil, nil
	}
	// Stack-scoped, not permanent — see extractBestPlanWithVisited: the mark
	// guards a recursion-stack back-edge (recursive-CTE cycle), but must be
	// lifted on the way up so a SHARED sub-DAG reached as two separate children
	// re-extracts instead of dropping to nil.
	visited[ref] = true
	defer delete(visited, ref)

	// OPTIMIZE-winner path: if the Reference has a physical winner
	// stamped, use it directly. Non-physical winners fall through to
	// the legacy extraction which navigates Members to find the
	// physical plan.
	w := ref.Winner()
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	if w != nil && isPhysicalPlan(w) {
		return rebuildExpressionFromSelectorVisited(ctx, w, sel, stats, visited)
	}

	var best expressions.RelationalExpression
	if sel != nil {
		hasBest := sel.HasBestMember(ref)
		if err := plannerContextErr(ctx); err != nil {
			return nil, err
		}
		if hasBest {
			best = sel.BestMember(ref)
			if err := plannerContextErr(ctx); err != nil {
				return nil, err
			}
		}
	}
	if !isPhysicalPlan(best) {
		best = ref.GetBest(costLessFor(sel, stats))
		if err := plannerContextErr(ctx); err != nil {
			return nil, err
		}
	}
	if best == nil {
		return nil, nil
	}

	// Sort elimination: if the best expression is a LogicalSort and the
	// selector can name a child member whose ordering satisfies the
	// sort's keys, skip the sort and use that member directly. The
	// satisfaction judgment lives with the selector (the planner), which
	// runs it on the rich Value + sort-order representation — this
	// package deliberately has no ordering model of its own.
	if sortExpr, ok := best.(*expressions.LogicalSortExpression); ok {
		childWinner, err := sortWinnerFromChild(ctx, sortExpr, sel, stats, visited)
		if err != nil {
			return nil, err
		}
		if childWinner != nil {
			return childWinner, nil
		}
	}

	return rebuildExpressionFromSelectorVisited(ctx, best, sel, stats, visited)
}

// TieBrokenCostSelector is the optional extension of BestMemberSelector a
// selector implements to supply a TOTAL-ORDER cost comparator for the
// extraction fallbacks. The scalar CostLessWith is not total (Cost ties are
// routine), and GetBest resolves ties by member insertion order — which
// shifts across plannings. The planner supplies its hash-tie-broken
// comparator through this seam; without it the fallbacks keep the raw
// comparator (test-only paths).
type TieBrokenCostSelector interface {
	TieBrokenCostLess(stats properties.StatisticsProvider) func(a, b expressions.RelationalExpression) bool
}

func costLessFor(sel BestMemberSelector, stats properties.StatisticsProvider) func(a, b expressions.RelationalExpression) bool {
	if tb, ok := sel.(TieBrokenCostSelector); ok {
		if less := tb.TieBrokenCostLess(stats); less != nil {
			return less
		}
	}
	return properties.CostLessWith(stats)
}

// SortElisionSelector is the optional extension of BestMemberSelector a
// selector implements to enable extraction-time sort elimination.
type SortElisionSelector interface {
	// OrderedChildWinner returns a physical member of childRef whose
	// ordering satisfies sortExpr's keys, or nil (sort must stay).
	OrderedChildWinner(sortExpr *expressions.LogicalSortExpression, childRef *expressions.Reference) expressions.RelationalExpression
	// OrderingSourceRef reports whether expr merely PRESERVES its
	// input's order, returning the child group the ordering flows from.
	// Extraction pins that group when eliding a sort (see
	// rebuildOrderedSpine).
	OrderingSourceRef(expr expressions.RelationalExpression) (*expressions.Reference, bool)
}

// sortWinnerFromChild asks the selector for a child member that already
// provides the sort's ordering. If one exists, returns the member rebuilt
// with its ordering spine PINNED (sort eliminated). If not — or the
// selector doesn't implement SortElisionSelector, or the spine cannot be
// pinned — returns nil and the sort stays.
func sortWinnerFromChild(
	ctx context.Context,
	sortExpr *expressions.LogicalSortExpression,
	sel BestMemberSelector,
	stats properties.StatisticsProvider,
	visited map[*expressions.Reference]bool,
) (expressions.RelationalExpression, error) {
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	elider, ok := sel.(SortElisionSelector)
	if !ok {
		return nil, nil
	}
	childRef := sortExpr.GetInner().GetRangesOver()
	if childRef == nil {
		return nil, nil
	}
	winner := elider.OrderedChildWinner(sortExpr, childRef)
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	if winner == nil || !isPhysicalPlan(winner) {
		return nil, nil
	}
	rebuilt, err := rebuildOrderedSpine(ctx, winner, sortExpr, elider, sel, stats, visited, map[*expressions.Reference]bool{})
	if err != nil {
		if cancelErr := plannerContextErr(ctx); cancelErr != nil {
			return nil, cancelErr
		}
		// Sort elision is optional. Preserve the historical fallback for a
		// non-cancellation rebuild failure: keep the explicit sort.
		return nil, nil
	}
	if rebuilt == nil {
		return nil, nil
	}
	return rebuilt, nil
}

// rebuildOrderedSpine rebuilds a sort-elision winner with its ordering
// spine pinned. An order-PRESERVING wrapper (OrderingSourceRef) derives
// its ordering from a child group that may hold BOTH the satisfying
// ordered member and a cheaper unordered one; the generic rebuild
// (rebuildExpressionFromSelectorVisited) resolves every group to its
// overall winner, which would relink the wrapper to the unordered member
// AFTER the sort above it was already discarded — silently unordered
// output. So along the spine the child group is resolved via
// OrderedChildWinner and recursively pinned into a fresh singleton
// Reference; the first self-providing node (an ordered scan) ends the
// spine and its own children extract generically. Any level that cannot
// be pinned (no satisfying member, or a cyclic reference revisits a
// pinned group) returns nil — the caller keeps the sort, which is always
// order-correct.
func rebuildOrderedSpine(
	ctx context.Context,
	e expressions.RelationalExpression,
	sortExpr *expressions.LogicalSortExpression,
	elider SortElisionSelector,
	sel BestMemberSelector,
	stats properties.StatisticsProvider,
	visited map[*expressions.Reference]bool,
	pinned map[*expressions.Reference]bool,
) (expressions.RelationalExpression, error) {
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	srcRef, delegates := elider.OrderingSourceRef(e)
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	freshChildren := make([]expressions.Quantifier, 0, len(e.GetQuantifiers()))
	for _, q := range e.GetQuantifiers() {
		if err := plannerContextErr(ctx); err != nil {
			return nil, err
		}
		childRef := q.GetRangesOver()
		if delegates && childRef == srcRef && srcRef != nil {
			if pinned[srcRef] {
				return nil, nil // cycle on the spine — decline elision
			}
			pinned[srcRef] = true
			om := elider.OrderedChildWinner(sortExpr, srcRef)
			if err := plannerContextErr(ctx); err != nil {
				return nil, err
			}
			if om == nil || !isPhysicalPlan(om) {
				return nil, nil // spine broken — decline elision
			}
			inner, err := rebuildOrderedSpine(ctx, om, sortExpr, elider, sel, stats, visited, pinned)
			if err != nil {
				return nil, err
			}
			if inner == nil {
				return nil, nil
			}
			freshChildren = append(freshChildren, expressions.ForEachQuantifier(expressions.InitialOf(inner)))
			continue
		}
		inner, err := extractBestPlanFromSelectorVisited(ctx, childRef, sel, stats, visited)
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
	rebuilt, err := rebuildWithFreshChildren(e, freshChildren)
	if cancelErr := plannerContextErr(ctx); cancelErr != nil {
		return nil, cancelErr
	}
	if err != nil || rebuilt == nil {
		return rebuilt, err
	}
	// A pin that did not reach the EXECUTABLE plan is not a pin — the same
	// verification pinOrderedSpine applies at rule time: WithChildren may
	// keep its ORIGINAL concrete plan when the pinned inner is not
	// leaf-replaceable, leaving the quantifier pointing at the pinned
	// member while GetRecordQueryPlan() still executes the OLD child. On a
	// delegating spine node, verify the rebuilt wrapper's concrete plan
	// embeds the pinned child's plan; decline the elision otherwise (the
	// sort stays — always order-correct).
	if delegates && srcRef != nil {
		rp, ok1 := rebuilt.(physicalPlanHolder)
		// Verify against the SPINE quantifier's pinned plan specifically —
		// current delegators are single-quantifier, but taking "the last
		// quantifier with a plan" would verify the wrong child the moment a
		// multi-quantifier delegator appears. The spine child was rebuilt
		// at the same position as the original srcRef quantifier.
		var pinnedChildPlan plans.RecordQueryPlan
		origQs := e.GetQuantifiers()
		newQs := rebuilt.GetQuantifiers()
		for i, q := range origQs {
			if q.GetRangesOver() != srcRef || i >= len(newQs) {
				continue
			}
			if inner := newQs[i].GetRangesOver().Get(); inner != nil {
				if ip, ok := inner.(physicalPlanHolder); ok {
					pinnedChildPlan = ip.GetRecordQueryPlan()
				}
			}
			break
		}
		if !ok1 || pinnedChildPlan == nil || !planEmbedsDirectChild(rp.GetRecordQueryPlan(), pinnedChildPlan) {
			return nil, nil
		}
	}
	return rebuilt, nil
}

// planEmbedsDirectChild reports whether child is among plan's immediate
// concrete children (pointer identity — WithChildren embeds the exact plan
// object it relinked to). The properties-side twin of the cascades
// package's planHasDirectChild.
func planEmbedsDirectChild(plan, child plans.RecordQueryPlan) bool {
	if plan == nil || child == nil {
		return false
	}
	for _, c := range plan.GetChildren() {
		if c == child {
			return true
		}
	}
	return false
}

// rebuildExpressionFromSelectorVisited is the same switch-based
// rebuilder as rebuildExpression but recurses through
// extractBestPlanFromSelectorVisited to consult the selector at
// every Reference, with cycle detection.
func rebuildExpressionFromSelectorVisited(
	ctx context.Context,
	e expressions.RelationalExpression,
	sel BestMemberSelector,
	stats properties.StatisticsProvider,
	visited map[*expressions.Reference]bool,
) (expressions.RelationalExpression, error) {
	if err := plannerContextErr(ctx); err != nil {
		return nil, err
	}
	if e == nil {
		return nil, nil
	}
	freshChildren := make([]expressions.Quantifier, 0, len(e.GetQuantifiers()))
	for _, q := range e.GetQuantifiers() {
		if err := plannerContextErr(ctx); err != nil {
			return nil, err
		}
		inner, err := extractBestPlanFromSelectorVisited(ctx, q.GetRangesOver(), sel, stats, visited)
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
	rebuilt, err := rebuildWithFreshChildren(e, freshChildren)
	if cancelErr := plannerContextErr(ctx); cancelErr != nil {
		return nil, cancelErr
	}
	return rebuilt, err
}

// rebuildExpressionVisited returns a fresh RelationalExpression of the
// same concrete type as `e`, with each Quantifier's Reference replaced
// by a singleton Reference holding the recursively-extracted best plan
// of the original Reference under `stats`. The visited map provides
// cycle detection.
func rebuildExpressionVisited(e expressions.RelationalExpression, stats properties.StatisticsProvider, visited map[*expressions.Reference]bool) (expressions.RelationalExpression, error) {
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

// PlanRebuildError marks a failure to rebuild an expression with fresh child
// quantifiers during plan extraction — an arity mismatch between an expression
// and the children the memo produced for it. Like a yield-invariant violation
// it is memo corruption, never a statement about the user's query.
//
// It exists so a caller can identify the FAMILY without parsing the message,
// which the SQL layer withholds because it names Go types.
type PlanRebuildError struct{ Cause error }

// Error returns the underlying rebuild message unchanged.
func (e *PlanRebuildError) Error() string { return e.Cause.Error() }

// Unwrap returns the underlying rebuild error.
func (e *PlanRebuildError) Unwrap() error { return e.Cause }

// rebuildWithFreshChildren is the switch-on-type rebuilder shared
// by rebuildExpression and rebuildExpressionFromSelector.
func rebuildWithFreshChildren(e expressions.RelationalExpression, freshChildren []expressions.Quantifier) (rebuilt expressions.RelationalExpression, err error) {
	// One deferred tag covers every arity failure in this function AND every
	// error returned by a WithChildren implementer through the default arm —
	// the family is "the rebuild failed", regardless of which of the 30-odd
	// expression and plan types reported it.
	defer func() {
		if err != nil {
			err = &PlanRebuildError{Cause: err}
		}
	}()
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
		// WithAliases: this is the extraction rebuild, so dropping the alias
		// vector here loses the output column names of every rebuilt
		// projection — the same defect PushLimitThroughProjectionRule had.
		return expressions.NewLogicalProjectionExpressionWithAliases(
			ex.GetProjectedValues(), ex.GetAliases(), freshChildren[0],
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
// The concrete RelationalExpression types do NOT implement
// WithChildren — their constructors take operator-specific args
// (predicates, sort keys, etc.) that the generic rebuilder doesn't
// have. Instead, ExtractBestPlan has explicit switch arms for those
// types. The interface exists for OPAQUE wrappers (e.g. cascades-
// internal physical-plan adapters) that want to participate in
// extraction without forcing a switch-arm extension.
//
// Mirrors Java's `RelationalExpressionWithChildren.withNewChildren`
// for that wrapper subset.
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
