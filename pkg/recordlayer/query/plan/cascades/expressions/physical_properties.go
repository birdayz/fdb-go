package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

// OrderingFromNameDir creates PhysicalProperties from column
// name+direction pairs. Used by ordering-aware code outside the
// expressions package (e.g., physical wrappers that know their
// ordering via HintOrdering).
func OrderingFromNameDir(names []string, desc []bool) PhysicalProperties {
	var o physicalOrdering
	for i, name := range names {
		if i >= len(o.cols) {
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
