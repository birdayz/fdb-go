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
// distinctness, and dependency direction. The Go type is simpler: a
// yes/no accessor + the ordering-key Values (with per-key direction)
// when known.
//
// Ordering feeds plan selection through the cascades package's rich
// ordering derivation (computeWrapperRichOrdering lifts a member's
// HintOrdering into a RichOrdering; RichOrdering.Satisfies judges
// requirements on full Value identity and the four-state sort order)
// and through sort elimination at extraction (a Sort whose child has
// a member satisfying the requested keys extracts to that ordered
// child directly, dropping the in-memory sort).

package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	// NullsFirst is parallel to Keys. true = NULLs sort before non-NULLs
	// for that key. nil or shorter than Keys means the FDB-natural default
	// per key (ASC → nulls first, DESC → nulls last) — see NullsFirstAt.
	// Only "counterflow" producers (an explicit ORDER BY ... NULLS clause
	// that inverts the natural order, materialized by an in-memory sort)
	// need to populate this; natural-order producers (index scans) leave it
	// empty and NullsFirstAt derives the natural placement. Carrying it lets
	// a parent sort correctly decline to elide against a counterflow stream
	// (e.g. an inner `ASC NULLS LAST` does NOT satisfy an outer `ASC`).
	NullsFirst []bool
}

// DescendingAt reports whether key i is descending (false past the slice).
func (o Ordering) DescendingAt(i int) bool {
	return i < len(o.Descending) && o.Descending[i]
}

// NullsFirstAt reports the NULL placement for key i, defaulting to the
// FDB-natural order (ASC → nulls first, DESC → nulls last) when NullsFirst
// is unset for that key.
func (o Ordering) NullsFirstAt(i int) bool {
	if i < len(o.NullsFirst) {
		return o.NullsFirst[i]
	}
	return !o.DescendingAt(i) // natural: ASC→nulls first, DESC→nulls last
}

// EstimateOrdering returns the static ordering guarantee for an
// expression. Mirrors Java's OrderingProperty walk but in a much
// reduced form.
//
// Per-operator semantics:
//
//	FullUnorderedScan: IsKnown=false (FDB scan order is by primary
//	    key, but the LOGICAL unordered-scan expression is treated as
//	    truly unordered; ordered access is modeled by the physical
//	    ordered-scan wrappers, which advertise their ordering via
//	    OrderingHinter).
//	Sort: IsKnown=true, Keys = the sort keys.
//	Filter / Projection / TypeFilter: inherits child ordering (these
//	    operators preserve row order).
//	Distinct / Unique: inherits child ordering (dedup drops repeats
//	    without reordering survivors — see the case arms below).
//	Union / Intersection: IsKnown=false (logical concatenation loses
//	    order; ordered physical set operations advertise theirs via
//	    OrderingHinter).
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
		nullsFirst := make([]bool, 0, len(sks))
		for _, sk := range sks {
			keys = append(keys, sk.Value)
			desc = append(desc, sk.Reverse)
			nf := !sk.Reverse // natural default: ASC→nulls first, DESC→nulls last
			if sk.NullsFirst != nil {
				nf = *sk.NullsFirst
			}
			nullsFirst = append(nullsFirst, nf)
		}
		return Ordering{IsKnown: true, Keys: keys, Descending: desc, NullsFirst: nullsFirst}
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
		// Distinct preserves the inner's ordering — duplicate
		// elimination doesn't reorder rows; it just drops repeats, so
		// the inner's ordering is returned directly. Java's
		// OrderingProperty is more granular (it only preserves
		// orderings aligned with the dedup keys, which matters for its
		// equality-bound-key bookkeeping Go does not track here).
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
