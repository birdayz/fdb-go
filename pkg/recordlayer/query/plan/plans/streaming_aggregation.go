package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryStreamingAggregationPlan groups input rows by grouping
// keys and computes aggregates over each group in a streaming fashion.
// The plan requires that the inner plan produces rows already sorted
// by the grouping keys — no materialisation needed.
//
// Mirrors Java's RecordQueryStreamingAggregationPlan: the streaming
// operator reads sorted input and emits one output row per change in
// the grouping-key combination. When the inner is NOT ordered by
// grouping keys, ImplementStreamingAggregationRule does not fire —
// a sort is needed first, or the hash-aggregate path (future) is
// used instead.
type RecordQueryStreamingAggregationPlan struct {
	inner        RecordQueryPlan
	groupingKeys []values.Value
	aggregates   []expressions.AggregateSpec
}

func NewRecordQueryStreamingAggregationPlan(
	inner RecordQueryPlan,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
) *RecordQueryStreamingAggregationPlan {
	return &RecordQueryStreamingAggregationPlan{
		inner:        inner,
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
	}
}

func (p *RecordQueryStreamingAggregationPlan) GetInner() RecordQueryPlan       { return p.inner }
func (p *RecordQueryStreamingAggregationPlan) GetGroupingKeys() []values.Value { return p.groupingKeys }
func (p *RecordQueryStreamingAggregationPlan) GetAggregates() []expressions.AggregateSpec {
	return p.aggregates
}

func (p *RecordQueryStreamingAggregationPlan) GetResultType() values.Type {
	return values.UnknownType
}

func (p *RecordQueryStreamingAggregationPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryStreamingAggregationPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryStreamingAggregationPlan)
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

func (p *RecordQueryStreamingAggregationPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("streamagg|"))
	for _, k := range p.groupingKeys {
		h.Write([]byte(values.ExplainValue(k)))
	}
	for _, a := range p.aggregates {
		h.Write([]byte{byte(a.Function)})
		h.Write([]byte(values.ExplainValue(a.Operand)))
	}
	return h.Sum64()
}

func (p *RecordQueryStreamingAggregationPlan) Explain() string {
	keys := make([]string, len(p.groupingKeys))
	for i, k := range p.groupingKeys {
		keys[i] = values.ExplainValue(k)
	}
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("StreamingAgg(keys=[%s], %s)", strings.Join(keys, ", "), innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryStreamingAggregationPlan)(nil)
