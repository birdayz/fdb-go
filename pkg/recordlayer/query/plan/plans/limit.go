package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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
	PlanExprBase
	innerQ     expressions.Quantifier
	limit      int64
	offset     int64
	limitValue values.Value
}

func NewRecordQueryLimitPlan(inner RecordQueryPlan, limit, offset int64) *RecordQueryLimitPlan {
	return &RecordQueryLimitPlan{innerQ: QuantifierOverPlan(inner), limit: limit, offset: offset}
}

// NewRecordQueryLimitPlanWithValue builds a LIMIT whose row cap is a runtime
// Value, evaluated at execution against the bound parameters. The static limit
// is the no-cap sentinel (-1); only limitValue is consulted.
func NewRecordQueryLimitPlanWithValue(inner RecordQueryPlan, limitValue values.Value, offset int64) *RecordQueryLimitPlan {
	return &RecordQueryLimitPlan{innerQ: QuantifierOverPlan(inner), limit: -1, offset: offset, limitValue: limitValue}
}

// GetLimitValue returns the optional runtime row-cap Value (nil for a static
// literal LIMIT).
func (p *RecordQueryLimitPlan) GetLimitValue() values.Value { return p.limitValue }

// WithInner returns a shallow copy bound to a new inner plan, preserving the
// cap (static or runtime). Used when an implementation rule rebuilds the wrapper
// around a folded leaf so the runtime limitValue is never dropped.
func (p *RecordQueryLimitPlan) WithInner(inner RecordQueryPlan) *RecordQueryLimitPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryLimitPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

func (p *RecordQueryLimitPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryLimitPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// GetInner exposes the single child so generic single-inner walkers
// (deriveColumnsFromPlan, findScanPlan, findIndexPlan, …) can descend
// through the limit — it is a row-count cap, transparent to column
// derivation and ordering. Without this the LIMIT plan, when it sits at
// the root (RFC-128 made the top-level LIMIT a real operator), is opaque
// to column derivation and the result columns resolve wrong.
func (p *RecordQueryLimitPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

func (p *RecordQueryLimitPlan) GetLimit() int64  { return p.limit }
func (p *RecordQueryLimitPlan) GetOffset() int64 { return p.offset }

// structuralKey lists the fields that distinguish this LIMIT in the memo: the
// static cap, the offset, and the optional runtime cap Value. Children are
// excluded (structural identity is without-children). The same key drives both
// EqualsPlanWithoutChildren and HashCodeWithoutChildren, so the two can never
// disagree on which fields matter. This is a behaviour-preserving refactor: the
// runtime cap keeps its original ValuesStructurallyEqual primitive (StructVal)
// and its SemanticHashCode hash, exactly as the hand-rolled pair had them.
func (p *RecordQueryLimitPlan) structuralKey() *structuralKey {
	k := newStructuralKey().Int64(p.limit).Int64(p.offset)
	if p.limitValue != nil {
		k.StructVal(p.limitValue)
	}
	return k
}

func (p *RecordQueryLimitPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryLimitPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryLimitPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("limit|")
}

func (p *RecordQueryLimitPlan) Explain() string {
	capStr := fmt.Sprintf("%d", p.limit)
	if p.limitValue != nil {
		capStr = values.ExplainValue(p.limitValue)
	}
	inner := p.GetInner()
	if p.offset > 0 {
		return fmt.Sprintf("Limit(%s, offset=%d, %s)", capStr, p.offset, inner.Explain())
	}
	return fmt.Sprintf("Limit(%s, %s)", capStr, inner.Explain())
}

var (
	_ RecordQueryPlan                  = (*RecordQueryLimitPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryLimitPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryLimitPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryLimitPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryLimitPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
