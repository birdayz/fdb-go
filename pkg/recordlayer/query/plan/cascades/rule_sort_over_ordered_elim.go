package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// SortOverOrderedElimRule eliminates a LogicalSort when the input
// already produces rows in the requested order. This is the key
// optimization for "ORDER BY col" queries over an index that provides
// the same ordering — the sort becomes a no-op.
//
//	Sort([col ASC]) over IndexScan(... ordered by col)
//	→ IndexScan(... ordered by col)
//
// Matching: the sort's key list must be a prefix of the input's
// known ordering. E.g. if the index provides (a, b) ordering and
// the sort requests (a), the sort is redundant. If the sort requests
// (a, b, c) but the index only provides (a, b), the sort is NOT
// redundant.
//
// Direction: a reverse-index scan matches DESC sort keys.
type SortOverOrderedElimRule struct {
	matcher matching.BindingMatcher
}

func NewSortOverOrderedElimRule() *SortOverOrderedElimRule {
	return &SortOverOrderedElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("sort_over_ordered"),
	}
}

func (r *SortOverOrderedElimRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *SortOverOrderedElimRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	sortKeys := s.GetSortKeys()
	if len(sortKeys) == 0 {
		return
	}

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	for _, member := range innerRef.Members() {
		ordering := properties.EstimateOrdering(member)
		if !ordering.IsKnown {
			continue
		}
		if orderingSatisfies(ordering, sortKeys) {
			call.Yield(member)
			return
		}
	}
}

func orderingSatisfies(ordering properties.Ordering, sortKeys []expressions.SortKey) bool {
	if len(sortKeys) > len(ordering.Keys) {
		return false
	}
	for i, sk := range sortKeys {
		orderKey := ordering.Keys[i]
		if !sameFieldValue(sk.Value, orderKey) {
			return false
		}
		orderDesc := i < len(ordering.Descending) && ordering.Descending[i]
		if sk.Reverse != orderDesc {
			return false
		}
		// Index scans produce ASC → NULLS FIRST, DESC → NULLS LAST.
		// Only reject when NullsFirst is explicitly set to non-default.
		if sk.NullsFirst != nil {
			defaultNF := !sk.Reverse
			if *sk.NullsFirst != defaultNF {
				return false
			}
		}
	}
	return true
}

// sameFieldValue returns true if both values are FieldValues with
// the same field name (case-insensitive).
func sameFieldValue(a, b values.Value) bool {
	fa, okA := a.(*values.FieldValue)
	fb, okB := b.(*values.FieldValue)
	if !okA || !okB {
		return false
	}
	return eqFold(fa.Field, fb.Field)
}

// eqFold compares two strings case-insensitively (ASCII).
func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*SortOverOrderedElimRule)(nil)
