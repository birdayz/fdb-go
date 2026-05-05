// Package properties — OrderingProperty file.
//
// Ordering tracks whether an expression's output rows are produced
// in a deterministic, stable order — and if so, by which keys. This
// is the "second axis" of the cost model after Cardinality:
// expressions that produce already-ordered output let downstream
// operators skip an explicit Sort, and ORDER BY-aware queries can
// be lowered to plans that exploit existing index ordering.
//
// Java's `properties/OrderingProperty` is a sophisticated component
// that tracks per-key ordering directions, equality-bound keys,
// distinctness, and dependency direction. The seed here is much
// simpler: a yes/no accessor + the ordering-key Values when known.
// Production-grade ordering analysis lands when Batch B's index-
// access rules need it.
//
// Today the seed makes no use of OrderingProperty — Cost ignores
// ordering. The Sort/Distinct rules currently fire unconditionally;
// once OrderingProperty is plumbed through Cost, those rules can
// short-circuit when input is already sorted / distinct.

package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Ordering captures the deterministic-output-order property of a
// RelationalExpression. An IsKnown=true Ordering means the producer
// guarantees rows in Keys order; IsKnown=false means no order
// guarantee (typical for Union / FullScan / hash-distinct).
type Ordering struct {
	// IsKnown is true when the expression's output order is guaranteed.
	IsKnown bool
	// Keys are the ordering key Values (in priority order: rows are
	// sorted by Keys[0] first, then Keys[1], etc.). Empty when
	// IsKnown=false.
	Keys []values.Value
	// Descending is parallel to Keys. true = descending order for that
	// key. nil or shorter than Keys means all ascending.
	Descending []bool
}

// EstimateOrdering returns the static ordering guarantee for an
// expression. Mirrors Java's OrderingProperty walk but in a much
// reduced form.
//
// Per-operator semantics:
//
//	FullUnorderedScan: IsKnown=false (FDB scan order is by primary
//	    key, but the Cascades layer treats the unordered-scan
//	    expression as truly unordered — index-access rules will
//	    refine this when they land).
//	Sort: IsKnown=true, Keys = the sort keys.
//	Filter / Projection / TypeFilter: inherits child ordering (these
//	    operators preserve row order).
//	Distinct: inherits child ordering iff the inner is sorted by
//	    the distinct's grouping keys (else IsKnown=false). The seed
//	    conservatively returns IsKnown=false — Java's analysis is
//	    sharper when it has the grouping key set.
//	Union / Intersection: IsKnown=false (concat / merge loses order
//	    in the seed; merge-sort union is a future variant).
//	Insert / Update / Delete: inherits inner ordering (DML is
//	    pass-through).
//
// Returns the zero-value Ordering ({IsKnown=false, Keys=nil}) when
// the expression's ordering can't be determined.
func EstimateOrdering(e expressions.RelationalExpression) Ordering {
	switch v := e.(type) {
	case *expressions.LogicalSortExpression:
		sks := v.GetSortKeys()
		keys := make([]values.Value, 0, len(sks))
		desc := make([]bool, 0, len(sks))
		for _, sk := range sks {
			keys = append(keys, sk.Value)
			desc = append(desc, sk.Reverse)
		}
		return Ordering{IsKnown: true, Keys: keys, Descending: desc}
	case *expressions.LogicalFilterExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.LogicalProjectionExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.LogicalTypeFilterExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.InsertExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.UpdateExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.DeleteExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.LogicalDistinctExpression:
		// Distinct preserves the inner's ordering when the inner is
		// sorted — duplicate elimination doesn't reorder rows; it just
		// drops repeats. The seed conservatively returns the inner's
		// ordering directly. Java's analysis is sharper (only preserves
		// when the inner ordering aligns with the distinct grouping
		// keys) but for the seed-level use case this is sufficient.
		return inheritFromInner(v.GetInner())
	case *expressions.LogicalUniqueExpression:
		// Unique (PK-based dedup) preserves inner ordering — same
		// rationale as Distinct. The PK comparison drops duplicates
		// without reordering surviving rows.
		return inheritFromInner(v.GetInner())
	case *expressions.LogicalLimitExpression:
		return inheritFromInner(v.GetInner())
	case *expressions.GroupByExpression:
		// GroupBy does not preserve input ordering — output is one row
		// per group, in implementation-defined order.
		return Ordering{IsKnown: false}
	default:
		if hinter, ok := e.(OrderingHinter); ok {
			return hinter.HintOrdering()
		}
	}
	return Ordering{IsKnown: false}
}

// inheritFromInner returns the best ordering from any member of the
// inner Reference. A filter/projection/type-filter preserves the
// ordering of its child — and after planner exploration, the
// ordering-providing physical alternative may not be the first member.
func inheritFromInner(inner expressions.Quantifier) Ordering {
	ref := inner.GetRangesOver()
	if ref == nil {
		return Ordering{}
	}
	for _, m := range ref.Members() {
		o := EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return Ordering{}
}

// IsOrdered reports whether the expression has a known output order.
// Convenience wrapper over EstimateOrdering.
func IsOrdered(e expressions.RelationalExpression) bool {
	return EstimateOrdering(e).IsKnown
}

// OrderingHinter is the optional interface a RelationalExpression
// implements to advertise its output ordering. Used by physical
// wrappers (e.g. index scan) to declare that their output is ordered
// by specific keys without the ordering property needing to know
// every concrete wrapper type.
type OrderingHinter interface {
	HintOrdering() Ordering
}
