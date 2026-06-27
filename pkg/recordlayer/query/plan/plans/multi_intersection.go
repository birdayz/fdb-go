package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryMultiIntersectionOnValuesPlan merges N input streams where
// all streams are ordered by the same comparison key (grouping columns).
// For each group of rows where the comparison key matches across ALL
// streams, it produces one output row combining:
//   - Common values (grouping columns) — taken from any stream (they're identical)
//   - Pick-up values (aggregates) — one from each stream
//
// Mirrors Java's RecordQueryMultiIntersectionOnValuesPlan which extends
// RecordQueryIntersectionPlan and adds a resultValue that constructs the
// merged output row from quantifier bindings.
type RecordQueryMultiIntersectionOnValuesPlan struct {
	children      []RecordQueryPlan // N input plans (one per aggregate index)
	comparisonKey []values.Value    // grouping columns to match on
	resultValue   values.Value      // result constructor (grouping + aggregates)
}

// NewRecordQueryMultiIntersectionOnValuesPlan constructs an N-way
// multi-intersection. comparisonKey defines the row-equality key
// (grouping columns); resultValue is the Value expression that
// constructs the output row from quantifier bindings.
func NewRecordQueryMultiIntersectionOnValuesPlan(
	children []RecordQueryPlan,
	comparisonKey []values.Value,
	resultValue values.Value,
) *RecordQueryMultiIntersectionOnValuesPlan {
	cpChildren := make([]RecordQueryPlan, len(children))
	copy(cpChildren, children)
	cpKeys := make([]values.Value, len(comparisonKey))
	copy(cpKeys, comparisonKey)
	return &RecordQueryMultiIntersectionOnValuesPlan{
		children:      cpChildren,
		comparisonKey: cpKeys,
		resultValue:   resultValue,
	}
}

// GetChildren returns the input plans.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetChildren() []RecordQueryPlan {
	return p.children
}

// GetComparisonKey returns the grouping-column values used to match
// rows across all input streams.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetComparisonKey() []values.Value {
	return p.comparisonKey
}

// GetResultValue returns the Value expression that constructs the
// merged output row.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetResultValue() values.Value {
	return p.resultValue
}

// GetResultType returns the result Value's type if a resultValue is
// set, or UnknownType otherwise.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetResultType() values.Type {
	if p.resultValue != nil {
		return p.resultValue.Type()
	}
	return values.UnknownType
}

// EqualsWithoutChildren matches MultiIntersectionOnValuesPlan with
// same-length comparison key and same resultValue (by explain string,
// matching the existing pattern for value-level equality).
func (p *RecordQueryMultiIntersectionOnValuesPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMultiIntersectionOnValuesPlan)
	if !ok {
		return false
	}
	if len(p.comparisonKey) != len(o.comparisonKey) {
		return false
	}
	for i, k := range p.comparisonKey {
		if values.ExplainValue(k) != values.ExplainValue(o.comparisonKey[i]) {
			return false
		}
	}
	// Compare result value.
	pRV := ""
	if p.resultValue != nil {
		pRV = values.ExplainValue(p.resultValue)
	}
	oRV := ""
	if o.resultValue != nil {
		oRV = values.ExplainValue(o.resultValue)
	}
	return pRV == oRV
}

// HashCodeWithoutChildren hashes the type discriminator, comparison key
// values, and result value.
func (p *RecordQueryMultiIntersectionOnValuesPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("multiintersectiononvaluesplan|"))
	for _, k := range p.comparisonKey {
		h.Write([]byte(values.ExplainValue(k)))
		h.Write([]byte{0})
	}
	if p.resultValue != nil {
		h.Write([]byte(values.ExplainValue(p.resultValue)))
	}
	return h.Sum64()
}

// Explain renders MultiIntersection(child1, child2, ...; keys=[...]).
func (p *RecordQueryMultiIntersectionOnValuesPlan) Explain() string {
	parts := make([]string, len(p.children))
	for i, child := range p.children {
		if child == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = child.Explain()
		}
	}
	keys := make([]string, len(p.comparisonKey))
	for i, k := range p.comparisonKey {
		keys[i] = values.ExplainValue(k)
	}
	return fmt.Sprintf("MultiIntersection(%s; keys=[%s])",
		strings.Join(parts, ", "), strings.Join(keys, ", "))
}

var _ RecordQueryPlan = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
