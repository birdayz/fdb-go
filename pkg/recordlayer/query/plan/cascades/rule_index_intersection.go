package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// IndexIntersectionRule explores the possibility of intersecting
// multiple index scans when no single index covers all predicates.
//
//	Filter([p1, p2, p3], Scan)
//	  → Intersection(
//	       Filter([p1], Scan),    // idx_a covers p1
//	       Filter([p2,p3], Scan)  // idx_b covers p2,p3
//	    )
//
// The intersection is on primary-key columns (or the common ordering
// key) — rows must appear in ALL children to be emitted. Each child
// is a filter containing only the predicates that child's candidate
// covers, which downstream ImplementIndexScanRule converts to index
// scans.
//
// The rule only fires when:
//   - At least 2 candidates each produce a non-empty prefix
//   - Their consumed predicate sets are disjoint (no overlap)
//   - Together they cover ALL predicates (no residual)
//
// This is conservative: Java's AbstractDataAccessRule does N-way
// intersection over any subset. We start with the simple 2-way
// full-coverage case and can generalize later.
//
// Java equivalent: the intersection planning in
// AbstractDataAccessRule.createIntersectionAndCompensation().
type IndexIntersectionRule struct {
	matcher matching.BindingMatcher
}

func NewIndexIntersectionRule() *IndexIntersectionRule {
	return &IndexIntersectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("filter_for_intersection"),
	}
}

func (r *IndexIntersectionRule) Matcher() matching.BindingMatcher { return r.matcher }

type intersectionCandidateMatch struct {
	cand     MatchCandidate
	consumed []int
	preds    []predicates.QueryPredicate
}

func (r *IndexIntersectionRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)

	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	scan := findFullScan(innerRef)
	if scan == nil {
		return
	}

	candidates := call.Context.GetMatchCandidates()
	if len(candidates) < 2 {
		return
	}

	preds := f.GetPredicates()
	if len(preds) < 2 {
		return
	}

	scanTypes := scan.GetRecordTypes()

	var matches []intersectionCandidateMatch

	for _, cand := range candidates {
		if !recordTypesOverlap(scanTypes, cand.GetRecordTypes()) {
			continue
		}

		colNames := cand.GetColumnNames()
		aliases := cand.GetSargableAliases()
		if len(colNames) != len(aliases) {
			continue
		}

		colToIdx := buildColumnIndex(colNames)
		bindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
		poisoned := make(map[values.CorrelationIdentifier]bool)
		var consumed []int

		for i, p := range preds {
			cp, ok := p.(*predicates.ComparisonPredicate)
			if !ok {
				continue
			}
			fv, ok := cp.Operand.(*values.FieldValue)
			if !ok {
				continue
			}
			colIdx, found := colToIdx[strings.ToUpper(fv.Field)]
			if !found {
				continue
			}
			alias := aliases[colIdx]
			if poisoned[alias] {
				continue
			}
			cr := bindings[alias]
			if cr == nil {
				cr = predicates.EmptyComparisonRange()
			}
			res := cr.Merge(&cp.Comparison)
			if !res.Ok {
				delete(bindings, alias)
				poisoned[alias] = true
				continue
			}
			bindings[alias] = res.Range
			consumed = append(consumed, i)
		}

		prefix := cand.ComputeBoundParameterPrefixMap(bindings)
		if len(prefix) == 0 {
			continue
		}

		// Determine which predicates are actually in the prefix.
		var inPrefix []int
		for _, idx := range consumed {
			cp := preds[idx].(*predicates.ComparisonPredicate)
			fv := cp.Operand.(*values.FieldValue)
			colIdx := colToIdx[strings.ToUpper(fv.Field)]
			alias := aliases[colIdx]
			if _, ok := prefix[alias]; ok {
				inPrefix = append(inPrefix, idx)
			}
		}

		if len(inPrefix) == 0 {
			continue
		}

		consumedPreds := make([]predicates.QueryPredicate, len(inPrefix))
		for i, idx := range inPrefix {
			consumedPreds[i] = preds[idx]
		}

		matches = append(matches, intersectionCandidateMatch{
			cand:     cand,
			consumed: inPrefix,
			preds:    consumedPreds,
		})
	}

	if len(matches) < 2 {
		return
	}

	// Try all pairs of candidates: require disjoint consumed sets.
	// Full coverage → bare intersection.
	// Partial coverage → Filter(residual, Intersection).
	for i := 0; i < len(matches)-1; i++ {
		for j := i + 1; j < len(matches); j++ {
			mi := matches[i]
			mj := matches[j]

			if !disjointSets(mi.consumed, mj.consumed) {
				continue
			}

			totalConsumed := len(mi.consumed) + len(mj.consumed)
			if totalConsumed == 0 {
				continue
			}

			legI := buildFilterLeg(call, scan, mi.preds)
			legJ := buildFilterLeg(call, scan, mj.preds)

			qI := expressions.ForEachQuantifier(call.MemoizeExpression(legI))
			qJ := expressions.ForEachQuantifier(call.MemoizeExpression(legJ))

			intersection := expressions.NewLogicalIntersectionExpression(
				[]expressions.Quantifier{qI, qJ},
				nil,
			)

			if totalConsumed == len(preds) {
				call.Yield(intersection)
			} else {
				consumedSet := make(map[int]bool, totalConsumed)
				for _, idx := range mi.consumed {
					consumedSet[idx] = true
				}
				for _, idx := range mj.consumed {
					consumedSet[idx] = true
				}
				var residual []predicates.QueryPredicate
				for idx, p := range preds {
					if !consumedSet[idx] {
						residual = append(residual, p)
					}
				}
				intrQ := expressions.ForEachQuantifier(call.MemoizeExpression(intersection))
				call.Yield(expressions.NewLogicalFilterExpression(residual, intrQ))
			}
		}
	}

	// N-way intersection (K≥3): try all disjoint subsets of size K that
	// together cover all predicates. Cap at 4-way to avoid combinatorial
	// explosion.
	if len(matches) >= 3 {
		r.tryNWayIntersection(call, scan, matches, preds)
	}
}

func (r *IndexIntersectionRule) tryNWayIntersection(
	call *ExpressionRuleCall,
	scan *expressions.FullUnorderedScanExpression,
	matches []intersectionCandidateMatch,
	preds []predicates.QueryPredicate,
) {
	n := len(matches)
	maxK := 4
	if n < maxK {
		maxK = n
	}

	for k := 3; k <= maxK; k++ {
		r.chooseK(call, scan, matches, preds, k, 0, nil)
	}
}

func (r *IndexIntersectionRule) chooseK(
	call *ExpressionRuleCall,
	scan *expressions.FullUnorderedScanExpression,
	matches []intersectionCandidateMatch,
	preds []predicates.QueryPredicate,
	k, start int,
	chosen []int,
) {
	if len(chosen) == k {
		if !allDisjoint(matches, chosen) {
			return
		}
		totalConsumed := 0
		for _, idx := range chosen {
			totalConsumed += len(matches[idx].consumed)
		}
		if totalConsumed == 0 {
			return
		}

		legs := make([]expressions.Quantifier, k)
		for i, idx := range chosen {
			leg := buildFilterLeg(call, scan, matches[idx].preds)
			legs[i] = expressions.ForEachQuantifier(call.MemoizeExpression(leg))
		}

		intersection := expressions.NewLogicalIntersectionExpression(legs, nil)

		if totalConsumed == len(preds) {
			call.Yield(intersection)
		} else {
			consumedSet := make(map[int]bool, totalConsumed)
			for _, idx := range chosen {
				for _, pi := range matches[idx].consumed {
					consumedSet[pi] = true
				}
			}
			var residual []predicates.QueryPredicate
			for pi, p := range preds {
				if !consumedSet[pi] {
					residual = append(residual, p)
				}
			}
			intrQ := expressions.ForEachQuantifier(call.MemoizeExpression(intersection))
			call.Yield(expressions.NewLogicalFilterExpression(residual, intrQ))
		}
		return
	}

	for i := start; i < len(matches); i++ {
		r.chooseK(call, scan, matches, preds, k, i+1, append(chosen, i))
	}
}

func buildFilterLeg(call *ExpressionRuleCall, scan *expressions.FullUnorderedScanExpression, preds []predicates.QueryPredicate) expressions.RelationalExpression {
	freshScan := expressions.NewFullUnorderedScanExpression(
		scan.GetRecordTypes(), scan.GetFlowedType(),
	)
	scanRef := call.MemoizeExpression(freshScan)
	q := expressions.ForEachQuantifier(scanRef)
	return expressions.NewLogicalFilterExpression(preds, q)
}

// disjointSets returns true if a and b share no elements.
func disjointSets(a, b []int) bool {
	set := make(map[int]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, exists := set[v]; exists {
			return false
		}
	}
	return true
}

// allDisjoint returns true if all chosen matches have pairwise
// disjoint consumed predicate sets.
func allDisjoint(matches []intersectionCandidateMatch, chosen []int) bool {
	seen := make(map[int]struct{})
	for _, idx := range chosen {
		for _, pi := range matches[idx].consumed {
			if _, exists := seen[pi]; exists {
				return false
			}
			seen[pi] = struct{}{}
		}
	}
	return true
}

var _ ExpressionRule = (*IndexIntersectionRule)(nil)
