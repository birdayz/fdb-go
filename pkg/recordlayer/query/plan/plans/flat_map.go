package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFlatMapPlan represents a correlated nested-loop join where
// for each outer row, the inner plan is re-executed with the outer row
// bound as a correlation. Mirrors Java's RecordQueryFlatMapPlan which
// uses FlatMapPipelinedCursor for execution.
//
// The key difference from RecordQueryNestedLoopJoinPlan: the inner plan
// is parameterized by the outer row via correlation bindings. This
// enables targeted index probes on the inner side (O(N×logM) vs O(N×M)).
type RecordQueryFlatMapPlan struct {
	outer      RecordQueryPlan
	inner      RecordQueryPlan
	outerAlias values.CorrelationIdentifier
	innerAlias values.CorrelationIdentifier
}

func NewRecordQueryFlatMapPlan(
	outer, inner RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
) *RecordQueryFlatMapPlan {
	return &RecordQueryFlatMapPlan{
		outer:      outer,
		inner:      inner,
		outerAlias: outerAlias,
		innerAlias: innerAlias,
	}
}

func (p *RecordQueryFlatMapPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryFlatMapPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.outer, p.inner}
}

func (p *RecordQueryFlatMapPlan) GetOuter() RecordQueryPlan                   { return p.outer }
func (p *RecordQueryFlatMapPlan) GetInner() RecordQueryPlan                   { return p.inner }
func (p *RecordQueryFlatMapPlan) GetOuterAlias() values.CorrelationIdentifier { return p.outerAlias }
func (p *RecordQueryFlatMapPlan) GetInnerAlias() values.CorrelationIdentifier { return p.innerAlias }

func (p *RecordQueryFlatMapPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFlatMapPlan)
	if !ok {
		return false
	}
	return p.outerAlias == o.outerAlias && p.innerAlias == o.innerAlias
}

func (p *RecordQueryFlatMapPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("flatmap|"))
	h.Write([]byte(p.outerAlias.Name()))
	h.Write([]byte{0})
	h.Write([]byte(p.innerAlias.Name()))
	return h.Sum64()
}

func (p *RecordQueryFlatMapPlan) Explain() string {
	return fmt.Sprintf("FlatMap(outer=%s, inner=%s)", p.outer.Explain(), p.inner.Explain())
}

var _ RecordQueryPlan = (*RecordQueryFlatMapPlan)(nil)
