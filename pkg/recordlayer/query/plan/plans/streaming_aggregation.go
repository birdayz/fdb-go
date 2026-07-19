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

// OutputColumnNames is the SINGLE naming authority for this plan's output row:
// grouping keys (in GROUP BY order) then aggregates (in aggregate order), each
// alias-preferring. The ordinal model bakes downstream references against
// this order, and the executor's aggregateCursor emits its PositionalRow with these
// exact names — so a reference over the aggregate resolves by Get(ordinal) (Java's
// getFieldValueForFieldOrdinals) instead of a spelling-sensitive name lookup.
func (p *RecordQueryStreamingAggregationPlan) OutputColumnNames() []string {
	return expressions.GroupByOutputColumnNames(p.groupingKeys, p.aggregates)
}

// OutputRecordType is OutputColumnNames as a RAW RecordType (ordinal == slice
// position; dup-name-safe). Flowed as the aggregate's result-value QOV type so the
// resolver BAKES downstream references to ordinals at plan time.
func (p *RecordQueryStreamingAggregationPlan) OutputRecordType() *values.RecordType {
	names := p.OutputColumnNames()
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.UnknownType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

func (p *RecordQueryStreamingAggregationPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryStreamingAggregationPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryStreamingAggregationPlan)
	if !ok {
		return false
	}
	if len(p.groupingKeys) != len(o.groupingKeys) {
		return false
	}
	for i, k := range p.groupingKeys {
		if !semanticValueEquals(k, o.groupingKeys[i]) {
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
		if !semanticValueEquals(a.Operand, o.aggregates[i].Operand) {
			return false
		}
	}
	return true
}

func (p *RecordQueryStreamingAggregationPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("streamagg|"))
	for _, k := range p.groupingKeys {
		writeValueHash(h, k)
	}
	for _, a := range p.aggregates {
		h.Write([]byte{byte(a.Function)})
		writeValueHash(h, a.Operand)
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
