package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryLimitPlan caps the result row count and optionally skips
// rows from an inner plan. Mirrors Java's fetch/limit plan operators.
type RecordQueryLimitPlan struct {
	inner  RecordQueryPlan
	limit  int64
	offset int64
}

func NewRecordQueryLimitPlan(inner RecordQueryPlan, limit, offset int64) *RecordQueryLimitPlan {
	return &RecordQueryLimitPlan{inner: inner, limit: limit, offset: offset}
}

func (p *RecordQueryLimitPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryLimitPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// GetInner exposes the single child so generic single-inner walkers
// (deriveColumnsFromPlan, findScanPlan, findIndexPlan, …) can descend
// through the limit — it is a row-count cap, transparent to column
// derivation and ordering. Without this the LIMIT plan, when it sits at
// the root (RFC-128 made the top-level LIMIT a real operator), is opaque
// to column derivation and the result columns resolve wrong.
func (p *RecordQueryLimitPlan) GetInner() RecordQueryPlan { return p.inner }

func (p *RecordQueryLimitPlan) GetLimit() int64  { return p.limit }
func (p *RecordQueryLimitPlan) GetOffset() int64 { return p.offset }

func (p *RecordQueryLimitPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryLimitPlan)
	if !ok {
		return false
	}
	return p.limit == o.limit && p.offset == o.offset
}

func (p *RecordQueryLimitPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("limit|"))
	b := [16]byte{}
	for i := 0; i < 8; i++ {
		b[i] = byte(p.limit >> (i * 8))
		b[8+i] = byte(p.offset >> (i * 8))
	}
	h.Write(b[:])
	return h.Sum64()
}

func (p *RecordQueryLimitPlan) Explain() string {
	if p.offset > 0 {
		return fmt.Sprintf("Limit(%d, offset=%d, %s)", p.limit, p.offset, p.inner.Explain())
	}
	return fmt.Sprintf("Limit(%d, %s)", p.limit, p.inner.Explain())
}

var _ RecordQueryPlan = (*RecordQueryLimitPlan)(nil)
