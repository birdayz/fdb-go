package plans

import (
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryProjectionPlan applies a projection (column selection /
// expression evaluation) over an inner plan's row stream. Mirrors
// Java's conceptual projection in RecordQueryFetchFromPartialRecordPlan
// / the MapPipelinedCursor mechanics. The seed models it as a distinct
// plan node for clarity.
type RecordQueryProjectionPlan struct {
	PlanExprBase
	projections []values.Value
	aliases     []string
	innerQ      expressions.Quantifier
}

func NewRecordQueryProjectionPlan(projections []values.Value, inner RecordQueryPlan) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: projections,
		innerQ:      QuantifierOverPlan(inner),
	}
}

func NewRecordQueryProjectionPlanWithAliases(projections []values.Value, aliases []string, inner RecordQueryPlan) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: projections,
		aliases:     aliases,
		innerQ:      QuantifierOverPlan(inner),
	}
}

func (p *RecordQueryProjectionPlan) GetProjections() []values.Value { return p.projections }
func (p *RecordQueryProjectionPlan) GetAliases() []string           { return p.aliases }

func (p *RecordQueryProjectionPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryProjectionPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// IsIdentity returns true if this projection passes all columns
// through unchanged (a QuantifiedObjectValue that references the
// inner's alias). An identity projection can be removed without
// changing the output shape.
func (p *RecordQueryProjectionPlan) IsIdentity() bool {
	if len(p.projections) != 1 {
		return false
	}
	_, ok := p.projections[0].(*values.QuantifiedObjectValue)
	return ok
}

func (p *RecordQueryProjectionPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryProjectionPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the projection lists by semantic Value
// identity (RFC-176 P2 — see semanticValueEquals): Java's model
// (RecordQueryMapPlan.equalsWithoutChildren → semanticEqualsForResults), where
// every semantic discriminator a projected Value carries — in particular a
// plan-time-resolved ordinal accessor (values.NewFieldValueWithResolvedOrdinal,
// the recursive-CTE duplicate-alias wrap; Java: distinct ofOrdinalNumber
// ordinals are distinct FieldPaths) — joins identity structurally. Two reads
// of duplicate-named slots differ ONLY by ordinal, so unifying them would let
// extraction pick a plan reading the WRONG slot.
//
// NOTE(explain format, RFC-176 P3): identity was previously keyed on the
// ExplainValue renderings, which therefore had to be injective over every
// semantic discriminator — the origin of the '#'-escape (raw '#' doubled,
// ordinal reads rendered "X#0"; PR #446 rounds 2-3). After P2 no identity
// code path reads a rendering; rendering is for humans, identity is
// structural. The escape is RETAINED as an explain-format guarantee —
// debugging output that collapses two different reads is still a bug — and
// its tests (TestFieldValue_ExplainOrdinalEscape, the plans-level
// TestProjectionPlan_Identity_OrdinalVsLiteralHashField) now pin exactly
// that, plus the matching injective discriminator in writeSemanticHash's
// FieldValue arm.
func (p *RecordQueryProjectionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryProjectionPlan)
	if !ok {
		return false
	}
	if len(p.projections) != len(o.projections) {
		return false
	}
	for i := range p.projections {
		if !semanticValueEquals(p.projections[i], o.projections[i]) {
			return false
		}
	}
	return true
}

func (p *RecordQueryProjectionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("projplan|"))
	for _, v := range p.projections {
		writeValueHash(h, v)
	}
	return h.Sum64()
}

func (p *RecordQueryProjectionPlan) Explain() string {
	var b strings.Builder
	b.WriteString("Project([")
	for i, v := range p.projections {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(values.ExplainValue(v))
	}
	b.WriteString("], ")
	if inner := p.GetInner(); inner != nil {
		b.WriteString(inner.Explain())
	} else {
		b.WriteString("<nil>")
	}
	b.WriteByte(')')
	return b.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryProjectionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryProjectionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryProjectionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryProjectionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}
