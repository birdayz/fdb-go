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
// This is a STRUCTURE-ONLY port — no execution logic. The hash-set
// dedup belongs in the execution layer.
//
// WHEN WIRING THE EXECUTOR: a "hash set of PKs already seen" over unordered
// input is the exact cross-page re-admission hazard fixed for value-DISTINCT
// (TODO C5). Go auto-pages internally (a scan hitting the scanned-rows limit
// returns a client-facing continuation and a NEW Execute runs the next page),
// so a fresh-per-Execute seen-set re-admits a PK whose duplicate straddles a
// page boundary → wrong rows. The executor MUST either carry the seen-set
// through the continuation (mirror distinctHashCursor / gen.DistinctHashContinuation
// in executor/distinct_stream.go) or require PK-ordered input and stream. Do
// NOT build a fresh-per-page HashSet like Java's — Java only pages on explicit
// client resume; Go's internal paging makes that shape wrong.
type RecordQueryUnorderedPrimaryKeyDistinctPlan struct {
	PlanExprBase
	innerQ expressions.Quantifier
}

// NewRecordQueryUnorderedPrimaryKeyDistinctPlan constructs a PK-based
// distinct plan over the given inner plan.
func NewRecordQueryUnorderedPrimaryKeyDistinctPlan(inner RecordQueryPlan) *RecordQueryUnorderedPrimaryKeyDistinctPlan {
	return &RecordQueryUnorderedPrimaryKeyDistinctPlan{innerQ: QuantifierOverPlan(inner)}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
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

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
