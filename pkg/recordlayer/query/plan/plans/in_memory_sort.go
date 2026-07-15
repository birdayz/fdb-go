// Go extension — no Java equivalent.
//
// Java's Cascades has no physical sort operator; RemoveSortRule
// eliminates the sort via index ordering or fails the query.
// This plan materializes the inner result and sorts in memory.
package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SortKey is a sort key + direction for in-memory sorting. ValueExpr is
// REQUIRED (RFC-173): it carries the key's plan-time-baked Value, which the
// executor evaluates POSITIONALLY per row. The field-only form (a Field lookup
// with a nil ValueExpr) is no longer supported — the runtime name fallback was
// deleted, so the executor rejects a nil ValueExpr as a malformed plan (loud,
// never a name read; pinned by TestSortCursor_UnbakedKeyIsLoud). Field is
// DISPLAY-ONLY (Explain + ordering-hint name match). Every planner path that
// builds a SortKey sets ValueExpr unconditionally (rule_implement_in_memory_sort,
// rule_implement_streaming_agg).
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // REQUIRED: the plan-time-baked key Value, evaluated per row
}

// RecordQueryInMemorySortPlan materializes the inner plan's output and
// sorts it in memory.
//
// Go extension — Java's Cascades has no physical sort operator.
//
// Cascades still optimizes the inner plan (index scans, predicate
// pushdown, join ordering). Only the final sort is post-processed.
// The cost model ensures index-based sort elimination is preferred
// when an index exists.
type RecordQueryInMemorySortPlan struct {
	inner    RecordQueryPlan
	sortKeys []SortKey
}

func NewRecordQueryInMemorySortPlan(inner RecordQueryPlan, sortKeys []SortKey) *RecordQueryInMemorySortPlan {
	keys := make([]SortKey, len(sortKeys))
	copy(keys, sortKeys)
	return &RecordQueryInMemorySortPlan{inner: inner, sortKeys: keys}
}

func (p *RecordQueryInMemorySortPlan) GetInner() RecordQueryPlan { return p.inner }
func (p *RecordQueryInMemorySortPlan) GetSortKeys() []SortKey    { return p.sortKeys }

func (p *RecordQueryInMemorySortPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryInMemorySortPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryInMemorySortPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInMemorySortPlan)
	if !ok {
		return false
	}
	if len(p.sortKeys) != len(o.sortKeys) {
		return false
	}
	for i := range p.sortKeys {
		if p.sortKeys[i] != o.sortKeys[i] {
			return false
		}
	}
	return true
}

func (p *RecordQueryInMemorySortPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("inmemsort|"))
	for _, k := range p.sortKeys {
		h.Write([]byte(k.Field))
		if k.Desc {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	return h.Sum64()
}

func (p *RecordQueryInMemorySortPlan) Explain() string {
	keys := make([]string, len(p.sortKeys))
	for i, k := range p.sortKeys {
		dir := "ASC"
		if k.Desc {
			dir = "DESC"
		}
		keys[i] = k.Field + " " + dir
	}
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	return fmt.Sprintf("InMemorySort([%s], %s)", strings.Join(keys, ", "), inner)
}

var _ RecordQueryPlan = (*RecordQueryInMemorySortPlan)(nil)
