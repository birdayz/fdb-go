package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PhysicalProperties captures the physical requirements a parent
// operator demands from a child group. Following Graefe 1995 §2,
// OptimizeGroup(group, properties) finds the cheapest plan in the
// group that satisfies the given properties. Winners are memoized
// per (group, properties) pair on Reference.
//
// This type is comparable (usable as map key) by design.
type PhysicalProperties struct {
	ordering physicalOrdering
}

type physicalOrdering struct {
	count int
	cols  [8]orderingColumn
	// overflow marks an ordering wider than the fixed column capacity.
	// The cols hold the TRUE first-8 prefix; the columns beyond it are
	// not representable. A requirement with overflow can NEVER be
	// satisfied (Satisfies returns false), so the enforcer sort is kept
	// and no winner is stamped — silently truncating the requirement
	// instead let a first-8-ordered plan elide the sort and drop the
	// 9th key's ordering.
	overflow bool
}

type orderingColumn struct {
	name string
	desc bool
}

// NoProperties is the zero-value PhysicalProperties — represents
// "no ordering required" (any plan is acceptable).
var NoProperties = PhysicalProperties{}

// IsEmpty returns true if no physical properties are required.
func (p PhysicalProperties) IsEmpty() bool {
	return p.ordering.count == 0
}

// OrderingFromSortKeys creates PhysicalProperties from a
// LogicalSortExpression's sort keys.
func OrderingFromSortKeys(keys []SortKey) PhysicalProperties {
	var o physicalOrdering
	for i, k := range keys {
		if i >= len(o.cols) {
			o.overflow = true
			break
		}
		o.cols[i] = orderingColumn{
			name: valueFieldName(k.Value),
			desc: k.Reverse,
		}
		o.count++
	}
	return PhysicalProperties{ordering: o}
}

// valueFieldName extracts the field name from a Value. For
// FieldValues, returns the Field name directly. For other Values,
// falls back to Name() (which is a debug string).
func valueFieldName(v values.Value) string {
	if fv, ok := v.(*values.FieldValue); ok {
		return fv.Field
	}
	return v.Name()
}

// OrderingCount returns the number of ordering columns.
func (p PhysicalProperties) OrderingCount() int {
	return p.ordering.count
}

// OrderingOverflowed reports whether the ordering exceeded the fixed
// column capacity — the stored columns are only a PREFIX of the real
// ordering. An overflowed value must never be used as a winner-map key:
// it no longer uniquely describes the provided ordering, so two
// different 9+-column orderings sharing a first-8 prefix would collide
// on it.
func (p PhysicalProperties) OrderingOverflowed() bool {
	return p.ordering.overflow
}

// OrderingFromNameDir creates PhysicalProperties from column
// name+direction pairs. Used by ordering-aware code outside the
// expressions package (e.g., physical wrappers that know their
// ordering via HintOrdering).
func OrderingFromNameDir(names []string, desc []bool) PhysicalProperties {
	var o physicalOrdering
	for i, name := range names {
		if i >= len(o.cols) {
			o.overflow = true
			break
		}
		d := false
		if i < len(desc) {
			d = desc[i]
		}
		o.cols[i] = orderingColumn{name: name, desc: d}
		o.count++
	}
	return PhysicalProperties{ordering: o}
}

// Satisfies returns true if this plan's properties satisfy the
// required properties. A plan satisfies requirements if it produces
// at least as many ordered columns in the same order.
func (p PhysicalProperties) Satisfies(required PhysicalProperties) bool {
	if required.IsEmpty() {
		return true
	}
	// A capacity-overflowed REQUIREMENT is unsatisfiable by construction:
	// its columns beyond the fixed prefix are unrepresentable, so no
	// provider can prove it covers them. The enforcer sort stays and no
	// winner is stamped. A provider-side overflow is fine — its cols are
	// the true prefix, which the normal prefix comparison handles.
	if required.ordering.overflow {
		return false
	}
	if p.ordering.count < required.ordering.count {
		return false
	}
	for i := 0; i < required.ordering.count; i++ {
		if p.ordering.cols[i] != required.ordering.cols[i] {
			return false
		}
	}
	return true
}
