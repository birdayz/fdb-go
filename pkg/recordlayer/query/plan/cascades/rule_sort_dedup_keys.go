package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// SortDedupKeysRule removes structurally-duplicate sort keys from a
// LogicalSort.
//
//	Sort([k1, k2, k1, k3], X)  →  Sort([k1, k2, k3], X)
//
// SQL semantics: ORDER BY a, b, a is equivalent to ORDER BY a, b —
// the second `a` adds no ordering refinement (rows already in
// a-order remain in a-order regardless of subsequent keys repeating).
//
// Sound under the seed model: sort keys are compared by Explain text
// (the bridge in LogicalSortExpression.EqualsWithoutChildren), so
// "duplicate key" is decided structurally. Reverse flag matters: a
// key with the same Value but different Reverse is NOT a duplicate
// (different ordering direction).
//
// Termination: yields Sort with the deduped key list, REUSING the
// inner Quantifier. Pointer-identity dedup absorbs second fire.
type SortDedupKeysRule struct {
	matcher matching.BindingMatcher
}

// NewSortDedupKeysRule constructs the rule.
func NewSortDedupKeysRule() *SortDedupKeysRule {
	return &SortDedupKeysRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("logical_sort"),
	}
}

// Matcher returns the pattern.
func (r *SortDedupKeysRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the sort key list contains at least one
// duplicate-by-Value-and-Reverse pair.
func (r *SortDedupKeysRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	deduped, removed := dedupSortKeys(s.GetSortKeys())
	if !removed {
		return
	}
	call.Yield(expressions.NewLogicalSortExpression(deduped, s.GetInner()))
}

// dedupSortKeys returns a slice of `keys` where each (Value-Explain,
// Reverse) pair appears at most once, preserving original order.
// Returns the deduped slice + a boolean indicating whether any
// pruning happened.
func dedupSortKeys(keys []expressions.SortKey) ([]expressions.SortKey, bool) {
	seen := map[string]struct{}{}
	out := make([]expressions.SortKey, 0, len(keys))
	removed := false
	for _, k := range keys {
		// Cache key combines Explain text + Reverse flag so a key
		// with the same Value but opposite direction stays.
		dir := "f"
		if k.Reverse {
			dir = "t"
		}
		hk := dir + "|" + values.ExplainValue(k.Value)
		if _, dup := seen[hk]; dup {
			removed = true
			continue
		}
		seen[hk] = struct{}{}
		out = append(out, k)
	}
	return out, removed
}

var _ ExpressionRule = (*SortDedupKeysRule)(nil)
