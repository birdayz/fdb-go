package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// InSourceKind distinguishes the three Java InJoin subclasses.
type InSourceKind int

const (
	InSourceValues    InSourceKind = iota // static value list (InValuesJoinPlan)
	InSourceParameter                     // runtime parameter binding (InParameterJoinPlan)
	InSourceComparand                     // comparand from correlated subquery (InComparandJoinPlan)
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
	sourceKind  InSourceKind
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

func (p *RecordQueryInJoinPlan) GetInner() RecordQueryPlan    { return p.inner }
func (p *RecordQueryInJoinPlan) GetBindingName() string       { return p.bindingName }
func (p *RecordQueryInJoinPlan) IsSorted() bool               { return p.sorted }
func (p *RecordQueryInJoinPlan) IsReverse() bool              { return p.reverse }
func (p *RecordQueryInJoinPlan) GetInValues() []any           { return p.inValues }
func (p *RecordQueryInJoinPlan) SetInValues(vals []any)       { p.inValues = vals }
func (p *RecordQueryInJoinPlan) GetSourceKind() InSourceKind  { return p.sourceKind }
func (p *RecordQueryInJoinPlan) SetSourceKind(k InSourceKind) { p.sourceKind = k }

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
	// bindingName is DELIBERATELY excluded: it is an internal correlation alias
	// minted by UniqueCorrelationIdentifier (a process-global counter), so two
	// structurally-identical InJoins that differ only in the arbitrary alias are
	// the SAME plan. Including it made every replanned IN-query non-equal and
	// differently-hashed → plan-cache churn + nondeterministic Explain (RFC-164
	// WS-4). Identity is alias-invariant; the real alias is retained on the field
	// for execution (GetBindingName), which is unaffected.
	return p.sorted == o.sorted && p.reverse == o.reverse
}

func (p *RecordQueryInJoinPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("injoinplan|"))
	// bindingName excluded — see EqualsWithoutChildren (alias-invariant identity).
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
	// The binding correlation alias (a process-global unique counter) is NOT
	// rendered — it varies per planning invocation and would make the Explain
	// nondeterministic (RFC-164 WS-4); "binding" marks its presence structurally.
	return fmt.Sprintf("InJoin(%s, binding%s)", inner, dir)
}

var _ RecordQueryPlan = (*RecordQueryInJoinPlan)(nil)
