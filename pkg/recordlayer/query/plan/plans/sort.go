package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQuerySortPlan sorts an inner plan's row stream by a list of
// sort keys. Mirrors Java's `RecordQuerySortPlan`.
//
// The sort key reuses the logical-side `expressions.SortKey` shape
// (Value + reverse flag) — physical and logical sort specifications
// share the same descriptor type since the only difference is which
// plan tree they live in.
type RecordQuerySortPlan struct {
	sortKeys []expressions.SortKey
	inner    RecordQueryPlan
}

// NewRecordQuerySortPlan constructs a sort plan over the given keys
// and inner plan. sortKeys is copied.
func NewRecordQuerySortPlan(sortKeys []expressions.SortKey, inner RecordQueryPlan) *RecordQuerySortPlan {
	copied := make([]expressions.SortKey, len(sortKeys))
	copy(copied, sortKeys)
	return &RecordQuerySortPlan{
		sortKeys: copied,
		inner:    inner,
	}
}

// GetSortKeys returns the sort key list (read-only).
func (p *RecordQuerySortPlan) GetSortKeys() []expressions.SortKey { return p.sortKeys }

// GetInner returns the wrapped inner plan.
func (p *RecordQuerySortPlan) GetInner() RecordQueryPlan { return p.inner }

// GetResultType returns the inner's result type — sort doesn't
// reshape rows.
func (p *RecordQuerySortPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQuerySortPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares sort key Values (via ExplainValue)
// + reverse flags pairwise.
func (p *RecordQuerySortPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQuerySortPlan)
	if !ok {
		return false
	}
	if len(p.sortKeys) != len(o.sortKeys) {
		return false
	}
	for i := range p.sortKeys {
		if p.sortKeys[i].Reverse != o.sortKeys[i].Reverse {
			return false
		}
		if values.ExplainValue(p.sortKeys[i].Value) != values.ExplainValue(o.sortKeys[i].Value) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes the class discriminator + per-key
// rendered text + reverse flags.
func (p *RecordQuerySortPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("sortplan|"))
	for _, k := range p.sortKeys {
		h.Write([]byte(values.ExplainValue(k.Value)))
		h.Write([]byte{0})
		if k.Reverse {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	return h.Sum64()
}

// Explain renders Sort([k1, k2 DESC], inner).
func (p *RecordQuerySortPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("Sort([%d keys], %s)", len(p.sortKeys), innerLabel)
}

var _ RecordQueryPlan = (*RecordQuerySortPlan)(nil)
