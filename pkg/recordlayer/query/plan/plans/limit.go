package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryLimitPlan caps the result row count and optionally skips
// rows from an inner plan. Mirrors Java's fetch/limit plan operators.
//
// limitValue is an OPTIONAL runtime row cap: when non-nil the executor
// evaluates it against the bound parameters at execution time and uses the
// result as the cap, ignoring the static `limit` field. It exists so a
// distance-ordered vector scan can be bounded by a PARAMETERIZED QUALIFY rank
// (`ROW_NUMBER() OVER (ORDER BY distance(...)) <= ?`): the K is unknown at plan
// time, so the cap must be carried as a Value (RFC-156). For a runtime limit the
// static `limit` is set to the no-cap sentinel (-1) so it is never mistaken for
// a literal LIMIT 0; the no-op-limit elimination / limit-merge rules decline on
// a non-nil limitValue rather than reading the sentinel.
type RecordQueryLimitPlan struct {
	inner      RecordQueryPlan
	limit      int64
	offset     int64
	limitValue values.Value
}

func NewRecordQueryLimitPlan(inner RecordQueryPlan, limit, offset int64) *RecordQueryLimitPlan {
	return &RecordQueryLimitPlan{inner: inner, limit: limit, offset: offset}
}

// NewRecordQueryLimitPlanWithValue builds a LIMIT whose row cap is a runtime
// Value, evaluated at execution against the bound parameters. The static limit
// is the no-cap sentinel (-1); only limitValue is consulted.
func NewRecordQueryLimitPlanWithValue(inner RecordQueryPlan, limitValue values.Value, offset int64) *RecordQueryLimitPlan {
	return &RecordQueryLimitPlan{inner: inner, limit: -1, offset: offset, limitValue: limitValue}
}

// GetLimitValue returns the optional runtime row-cap Value (nil for a static
// literal LIMIT).
func (p *RecordQueryLimitPlan) GetLimitValue() values.Value { return p.limitValue }

// WithInner returns a shallow copy bound to a new inner plan, preserving the
// cap (static or runtime). Used when an implementation rule rebuilds the wrapper
// around a folded leaf so the runtime limitValue is never dropped.
func (p *RecordQueryLimitPlan) WithInner(inner RecordQueryPlan) *RecordQueryLimitPlan {
	cp := *p
	cp.inner = inner
	return &cp
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
	if (p.limitValue == nil) != (o.limitValue == nil) {
		return false
	}
	if p.limitValue != nil && !values.ValuesStructurallyEqual(p.limitValue, o.limitValue) {
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
	if p.limitValue != nil {
		var v [8]byte
		hv := values.SemanticHashCode(p.limitValue)
		for i := 0; i < 8; i++ {
			v[i] = byte(hv >> (i * 8))
		}
		h.Write(v[:])
	}
	return h.Sum64()
}

func (p *RecordQueryLimitPlan) Explain() string {
	capStr := fmt.Sprintf("%d", p.limit)
	if p.limitValue != nil {
		capStr = values.ExplainValue(p.limitValue)
	}
	if p.offset > 0 {
		return fmt.Sprintf("Limit(%s, offset=%d, %s)", capStr, p.offset, p.inner.Explain())
	}
	return fmt.Sprintf("Limit(%s, %s)", capStr, p.inner.Explain())
}

var _ RecordQueryPlan = (*RecordQueryLimitPlan)(nil)
