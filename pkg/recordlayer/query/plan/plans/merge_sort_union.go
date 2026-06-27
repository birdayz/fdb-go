package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryMergeSortUnionPlan is the ordered (merge-sorted) union
// variant. Children must produce rows sorted by the comparison keys;
// the plan merges them maintaining that order. Optionally deduplicates
// rows that have equal comparison keys.
//
// Mirrors Java's RecordQueryUnionOnValuesPlan (which extends
// RecordQueryUnionPlan with comparison key values + reverse flag +
// distinct flag).
type RecordQueryMergeSortUnionPlan struct {
	inners           []RecordQueryPlan
	comparisonKeys   []values.Value
	reverse          bool
	removeDuplicates bool
}

func NewRecordQueryMergeSortUnionPlan(
	inners []RecordQueryPlan,
	comparisonKeys []values.Value,
	reverse bool,
	removeDuplicates bool,
) *RecordQueryMergeSortUnionPlan {
	copiedInners := make([]RecordQueryPlan, len(inners))
	copy(copiedInners, inners)
	copiedKeys := make([]values.Value, len(comparisonKeys))
	copy(copiedKeys, comparisonKeys)
	return &RecordQueryMergeSortUnionPlan{
		inners:           copiedInners,
		comparisonKeys:   copiedKeys,
		reverse:          reverse,
		removeDuplicates: removeDuplicates,
	}
}

func (p *RecordQueryMergeSortUnionPlan) GetInners() []RecordQueryPlan      { return p.inners }
func (p *RecordQueryMergeSortUnionPlan) GetComparisonKeys() []values.Value { return p.comparisonKeys }
func (p *RecordQueryMergeSortUnionPlan) IsReverse() bool                   { return p.reverse }
func (p *RecordQueryMergeSortUnionPlan) RemovesDuplicates() bool           { return p.removeDuplicates }

func (p *RecordQueryMergeSortUnionPlan) GetResultType() values.Type {
	if len(p.inners) == 0 {
		return values.UnknownType
	}
	return p.inners[0].GetResultType()
}

func (p *RecordQueryMergeSortUnionPlan) GetChildren() []RecordQueryPlan { return p.inners }

func (p *RecordQueryMergeSortUnionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMergeSortUnionPlan)
	if !ok {
		return false
	}
	return p.reverse == o.reverse && p.removeDuplicates == o.removeDuplicates &&
		len(p.comparisonKeys) == len(o.comparisonKeys)
}

func (p *RecordQueryMergeSortUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("mergesortunionplan|"))
	if p.reverse {
		h.Write([]byte{1})
	}
	if p.removeDuplicates {
		h.Write([]byte{2})
	}
	for _, k := range p.comparisonKeys {
		h.Write([]byte(values.ExplainValue(k)))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (p *RecordQueryMergeSortUnionPlan) Explain() string {
	parts := make([]string, len(p.inners))
	for i, inner := range p.inners {
		if inner == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = inner.Explain()
		}
	}
	dir := "ASC"
	if p.reverse {
		dir = "DESC"
	}
	dedup := ""
	if p.removeDuplicates {
		dedup = " DISTINCT"
	}
	return fmt.Sprintf("MergeSortUnion(%s, keys=[%d], %s%s)",
		strings.Join(parts, ", "), len(p.comparisonKeys), dir, dedup)
}

var _ RecordQueryPlan = (*RecordQueryMergeSortUnionPlan)(nil)
