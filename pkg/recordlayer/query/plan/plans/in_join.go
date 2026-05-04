package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryInJoinPlan executes its inner plan once for each value
// from an IN-source, binding the value to a correlation variable.
// The result is the concatenation of all inner executions.
//
// Mirrors Java's RecordQueryInJoinPlan hierarchy
// (InValuesJoin, InParameterJoin, InComparandJoin).
type RecordQueryInJoinPlan struct {
	inner       RecordQueryPlan
	bindingName string
	sorted      bool
	reverse     bool
	inValues    []any
}

func NewRecordQueryInJoinPlan(
	inner RecordQueryPlan,
	bindingName string,
	sorted bool,
	reverse bool,
) *RecordQueryInJoinPlan {
	return &RecordQueryInJoinPlan{
		inner:       inner,
		bindingName: bindingName,
		sorted:      sorted,
		reverse:     reverse,
	}
}

func (p *RecordQueryInJoinPlan) GetInner() RecordQueryPlan { return p.inner }
func (p *RecordQueryInJoinPlan) GetBindingName() string    { return p.bindingName }
func (p *RecordQueryInJoinPlan) IsSorted() bool            { return p.sorted }
func (p *RecordQueryInJoinPlan) IsReverse() bool           { return p.reverse }
func (p *RecordQueryInJoinPlan) GetInValues() []any        { return p.inValues }
func (p *RecordQueryInJoinPlan) SetInValues(vals []any)    { p.inValues = vals }

func (p *RecordQueryInJoinPlan) GetResultType() values.Type {
	if p.inner != nil {
		return p.inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryInJoinPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryInJoinPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInJoinPlan)
	if !ok {
		return false
	}
	return p.bindingName == o.bindingName && p.sorted == o.sorted && p.reverse == o.reverse
}

func (p *RecordQueryInJoinPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("injoinplan|"))
	h.Write([]byte(p.bindingName))
	if p.sorted {
		h.Write([]byte{1})
	}
	if p.reverse {
		h.Write([]byte{2})
	}
	return h.Sum64()
}

func (p *RecordQueryInJoinPlan) Explain() string {
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	dir := ""
	if p.sorted {
		if p.reverse {
			dir = " DESC"
		} else {
			dir = " ASC"
		}
	}
	return fmt.Sprintf("InJoin(%s, binding=%s%s)", inner, p.bindingName, dir)
}

var _ RecordQueryPlan = (*RecordQueryInJoinPlan)(nil)
