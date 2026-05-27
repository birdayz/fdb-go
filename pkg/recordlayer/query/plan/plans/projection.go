package plans

import (
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryProjectionPlan applies a projection (column selection /
// expression evaluation) over an inner plan's row stream. Mirrors
// Java's conceptual projection in RecordQueryFetchFromPartialRecordPlan
// / the MapPipelinedCursor mechanics. The seed models it as a distinct
// plan node for clarity.
type RecordQueryProjectionPlan struct {
	projections []values.Value
	aliases     []string
	inner       RecordQueryPlan
}

func NewRecordQueryProjectionPlan(projections []values.Value, inner RecordQueryPlan) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: projections,
		inner:       inner,
	}
}

func NewRecordQueryProjectionPlanWithAliases(projections []values.Value, aliases []string, inner RecordQueryPlan) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: projections,
		aliases:     aliases,
		inner:       inner,
	}
}

func (p *RecordQueryProjectionPlan) GetProjections() []values.Value { return p.projections }
func (p *RecordQueryProjectionPlan) GetAliases() []string           { return p.aliases }

func (p *RecordQueryProjectionPlan) GetInner() RecordQueryPlan { return p.inner }

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
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryProjectionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryProjectionPlan)
	if !ok {
		return false
	}
	if len(p.projections) != len(o.projections) {
		return false
	}
	for i := range p.projections {
		if values.ExplainValue(p.projections[i]) != values.ExplainValue(o.projections[i]) {
			return false
		}
	}
	return true
}

func (p *RecordQueryProjectionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("projplan|"))
	for _, v := range p.projections {
		h.Write([]byte(values.ExplainValue(v)))
		h.Write([]byte{0})
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
	if p.inner != nil {
		b.WriteString(p.inner.Explain())
	} else {
		b.WriteString("<nil>")
	}
	b.WriteByte(')')
	return b.String()
}

var _ RecordQueryPlan = (*RecordQueryProjectionPlan)(nil)
