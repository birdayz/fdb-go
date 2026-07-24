package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryUnorderedPrimaryKeyDistinctPlan removes duplicate rows by
// means of a hash set of primary keys already seen. Unlike
// RecordQueryDistinctPlan (which deduplicates by full row), this plan
// deduplicates by primary key only — two rows with the same PK but
// different projected columns collapse to one.
//
// Mirrors Java's RecordQueryUnorderedPrimaryKeyDistinctPlan. This is a
// single-child plan: it wraps an inner plan and filters its output
// stream.
//
// Execution uses the same continuation-carried hash set as unordered
// value-DISTINCT, keyed by the packed QueryResult primary key. It deliberately
// has no streaming mode: the child is not required to be primary-key ordered.
type RecordQueryUnorderedPrimaryKeyDistinctPlan struct {
	PlanExprBase
	innerQ expressions.Quantifier
}

// NewRecordQueryUnorderedPrimaryKeyDistinctPlan constructs a PK-based
// distinct plan over the given inner plan.
func NewRecordQueryUnorderedPrimaryKeyDistinctPlan(inner RecordQueryPlan) *RecordQueryUnorderedPrimaryKeyDistinctPlan {
	return &RecordQueryUnorderedPrimaryKeyDistinctPlan{innerQ: QuantifierOverPlan(inner)}
}

// NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier constructs a
// primary-key distinct plan over the supplied live memo edge.
func NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(
	innerQ expressions.Quantifier,
) *RecordQueryUnorderedPrimaryKeyDistinctPlan {
	return &RecordQueryUnorderedPrimaryKeyDistinctPlan{innerQ: innerQ}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the plan's single child edge.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the child row unchanged.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// IsReverse delegates to the inner plan.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) IsReverse() bool {
	if c, ok := p.GetInner().(interface{ IsReverse() bool }); ok {
		return c.IsReverse()
	}
	return false
}

// GetResultType returns the inner plan's result type — PK-distinct
// doesn't reshape rows.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey has no parts — PK-distinct plans have no node-specific data
// beyond the concrete type. Mirrors Java where equalsWithoutChildren only
// checks `getClass() == otherExpression.getClass()`. The same key drives both
// EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) structuralKey() *structuralKey {
	return newStructuralKey()
}

func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryUnorderedPrimaryKeyDistinctPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mirrors Java's
// BASE_HASH("Record-Query-Unordered-Primary-Key-Distinct-Plan").
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("unorderedprimarykeyDistinctplan")
}

// Explain renders UnorderedPrimaryKeyDistinct(inner).
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("UnorderedPrimaryKeyDistinct(%s)", innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryUnorderedPrimaryKeyDistinctPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryUnorderedPrimaryKeyDistinctPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren is the extraction/relink hook. Relinking swaps only the child
// quantifier and preserves all node-local state.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) WithChildren(
	qs []expressions.Quantifier,
) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf(
			"RecordQueryUnorderedPrimaryKeyDistinctPlan.WithChildren: expected 1 child, got %d",
			len(qs),
		)
	}
	return p.WithQuantifiers(qs), nil
}

// WithInner returns a copy with a replacement singleton child.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) WithInner(
	inner RecordQueryPlan,
) *RecordQueryUnorderedPrimaryKeyDistinctPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
