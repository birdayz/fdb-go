package plans

import (
	"fmt"
	"hash/fnv"

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
	// bindingNames are internal correlation aliases minted by UniqueCorrelation-
	// Identifier (a process-global counter); only their COUNT (the number of IN
	// columns) is structural. Comparing the arbitrary names made every replanned
	// IN-union plan non-equal → plan-cache churn + nondeterministic Explain (RFC-164
	// WS-4, same class as RecordQueryInJoinPlan). Alias-invariant identity; the real
	// names are retained on the field for execution (GetBindingNames).
	return len(p.bindingNames) == len(o.bindingNames)
}

func (p *RecordQueryInUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("inunionplan|"))
	// bindingNames excluded — only their COUNT is structural (see EqualsWithoutChildren).
	h.Write([]byte{byte(len(p.bindingNames))})
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
	// The binding correlation aliases (process-global counters) are not rendered —
	// only the COUNT of IN bindings is structural (RFC-164 WS-4; see in_join.go).
	return fmt.Sprintf("InUnion(%s, bindings=%d, %s)", inner, len(p.bindingNames), dir)
}

var _ RecordQueryPlan = (*RecordQueryInUnionPlan)(nil)
