package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryInUnionPlan is the IN-union variant: the inner plan is
// executed once per IN-source value, and results are merge-sorted by
// comparison keys. Mirrors Java's RecordQueryInUnionOnValuesPlan.
type RecordQueryInUnionPlan struct {
	inner          RecordQueryPlan
	bindingNames   []string
	comparisonKeys []values.Value
	reverse        bool
	maxSize        int
	inSources      [][]any
}

func NewRecordQueryInUnionPlan(
	inner RecordQueryPlan,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
) *RecordQueryInUnionPlan {
	bn := make([]string, len(bindingNames))
	copy(bn, bindingNames)
	ck := make([]values.Value, len(comparisonKeys))
	copy(ck, comparisonKeys)
	return &RecordQueryInUnionPlan{
		inner:          inner,
		bindingNames:   bn,
		comparisonKeys: ck,
		reverse:        reverse,
	}
}

func NewRecordQueryInUnionPlanWithMaxSize(
	inner RecordQueryPlan,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) *RecordQueryInUnionPlan {
	p := NewRecordQueryInUnionPlan(inner, bindingNames, comparisonKeys, reverse)
	p.maxSize = maxSize
	return p
}

func (p *RecordQueryInUnionPlan) GetInner() RecordQueryPlan         { return p.inner }
func (p *RecordQueryInUnionPlan) GetBindingNames() []string         { return p.bindingNames }
func (p *RecordQueryInUnionPlan) GetComparisonKeys() []values.Value { return p.comparisonKeys }
func (p *RecordQueryInUnionPlan) IsReverse() bool                   { return p.reverse }
func (p *RecordQueryInUnionPlan) GetMaxSize() int                   { return p.maxSize }
func (p *RecordQueryInUnionPlan) GetInSources() [][]any             { return p.inSources }
func (p *RecordQueryInUnionPlan) SetInSources(sources [][]any)      { p.inSources = sources }

func (p *RecordQueryInUnionPlan) GetResultType() values.Type {
	if p.inner != nil {
		return p.inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryInUnionPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryInUnionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInUnionPlan)
	if !ok {
		return false
	}
	if p.reverse != o.reverse {
		return false
	}
	if len(p.bindingNames) != len(o.bindingNames) {
		return false
	}
	for i := range p.bindingNames {
		if p.bindingNames[i] != o.bindingNames[i] {
			return false
		}
	}
	return true
}

func (p *RecordQueryInUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("inunionplan|"))
	for _, bn := range p.bindingNames {
		h.Write([]byte(bn))
		h.Write([]byte{0})
	}
	if p.reverse {
		h.Write([]byte{1})
	}
	return h.Sum64()
}

func (p *RecordQueryInUnionPlan) Explain() string {
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	dir := "ASC"
	if p.reverse {
		dir = "DESC"
	}
	return fmt.Sprintf("InUnion(%s, bindings=[%s], %s)",
		inner, strings.Join(p.bindingNames, ", "), dir)
}

var _ RecordQueryPlan = (*RecordQueryInUnionPlan)(nil)
