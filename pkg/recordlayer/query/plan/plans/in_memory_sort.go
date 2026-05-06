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

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// SortKey is a column + direction for in-memory sorting.
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // when non-nil, evaluate per-row instead of field lookup
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
