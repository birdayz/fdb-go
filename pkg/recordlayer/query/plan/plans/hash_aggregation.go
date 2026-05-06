package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryHashAggregationPlan groups input rows by grouping keys
// and computes aggregates using a hash table. Unlike the streaming
// variant, this plan does NOT require sorted input — it materialises
// all groups in memory.
//
// Cost tradeoff: streaming aggregation is cheaper when the input is
// already sorted (O(1) memory, single-pass). Hash aggregation is the
// fallback when no ordering guarantee exists — it's O(groups) memory
// but still single-pass over input.
type RecordQueryHashAggregationPlan struct {
	inner        RecordQueryPlan
	groupingKeys []values.Value
	aggregates   []expressions.AggregateSpec
}

func NewRecordQueryHashAggregationPlan(
	inner RecordQueryPlan,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
) *RecordQueryHashAggregationPlan {
	return &RecordQueryHashAggregationPlan{
		inner:        inner,
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
	}
}

func (p *RecordQueryHashAggregationPlan) GetInner() RecordQueryPlan       { return p.inner }
func (p *RecordQueryHashAggregationPlan) GetGroupingKeys() []values.Value { return p.groupingKeys }
func (p *RecordQueryHashAggregationPlan) GetAggregates() []expressions.AggregateSpec {
	return p.aggregates
}

func (p *RecordQueryHashAggregationPlan) GetResultType() values.Type {
	return values.UnknownType
}

func (p *RecordQueryHashAggregationPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryHashAggregationPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryHashAggregationPlan)
	if !ok {
		return false
	}
	if len(p.groupingKeys) != len(o.groupingKeys) {
		return false
	}
	for i, k := range p.groupingKeys {
		if values.ExplainValue(k) != values.ExplainValue(o.groupingKeys[i]) {
			return false
		}
	}
	if len(p.aggregates) != len(o.aggregates) {
		return false
	}
	for i, a := range p.aggregates {
		if a.Function != o.aggregates[i].Function {
			return false
		}
		if values.ExplainValue(a.Operand) != values.ExplainValue(o.aggregates[i].Operand) {
			return false
		}
	}
	return true
}

func (p *RecordQueryHashAggregationPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("hashagg|"))
	for _, k := range p.groupingKeys {
		h.Write([]byte(values.ExplainValue(k)))
	}
	for _, a := range p.aggregates {
		h.Write([]byte{byte(a.Function)})
		h.Write([]byte(values.ExplainValue(a.Operand)))
	}
	return h.Sum64()
}

func (p *RecordQueryHashAggregationPlan) Explain() string {
	keys := make([]string, len(p.groupingKeys))
	for i, k := range p.groupingKeys {
		keys[i] = values.ExplainValue(k)
	}
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("HashAgg(keys=[%s], %s)", strings.Join(keys, ", "), innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryHashAggregationPlan)(nil)
