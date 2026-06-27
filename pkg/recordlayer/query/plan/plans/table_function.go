package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTableFunctionPlan delegates row-stream production to an
// underlying streaming Value (e.g. RangeValue). Leaf plan (no
// children). Mirrors Java's RecordQueryTableFunctionPlan.
type RecordQueryTableFunctionPlan struct {
	streamValue values.Value
}

func NewRecordQueryTableFunctionPlan(streamValue values.Value) *RecordQueryTableFunctionPlan {
	return &RecordQueryTableFunctionPlan{streamValue: streamValue}
}

func (p *RecordQueryTableFunctionPlan) GetStreamValue() values.Value { return p.streamValue }

func (p *RecordQueryTableFunctionPlan) GetResultType() values.Type {
	if p.streamValue == nil {
		return values.UnknownType
	}
	return p.streamValue.Type()
}

func (p *RecordQueryTableFunctionPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryTableFunctionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTableFunctionPlan)
	if !ok {
		return false
	}
	return p.streamValue == o.streamValue
}

func (p *RecordQueryTableFunctionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("tablefnplan|"))
	if p.streamValue != nil {
		h.Write([]byte(p.streamValue.Name()))
	}
	return h.Sum64()
}

func (p *RecordQueryTableFunctionPlan) Explain() string {
	if p.streamValue != nil {
		return fmt.Sprintf("TableFunction(%s)", p.streamValue.Name())
	}
	return "TableFunction(<nil>)"
}

var _ RecordQueryPlan = (*RecordQueryTableFunctionPlan)(nil)
