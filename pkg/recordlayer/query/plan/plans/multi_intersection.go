package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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
//
// The children are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `children []RecordQueryPlan` slice they replace was a second storage
// location for the same edges. RFC-183 P5 step 2.
type RecordQueryMultiIntersectionOnValuesPlan struct {
	PlanExprBase
	childQs       []expressions.Quantifier // N input plans (one per aggregate index)
	comparisonKey []values.Value           // grouping columns to match on
	resultValue   values.Value             // result constructor (grouping + aggregates)
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
	cpKeys := make([]values.Value, len(comparisonKey))
	copy(cpKeys, comparisonKey)
	return &RecordQueryMultiIntersectionOnValuesPlan{
		childQs:       QuantifiersOverPlans(children),
		comparisonKey: cpKeys,
		resultValue:   resultValue,
	}
}

// GetChildren returns the input plans, dereferenced through the quantifiers
// and in stream order — resultValue's pick-up columns are positional per
// stream.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetChildren() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

// GetComparisonKey returns the grouping-column values used to match
// rows across all input streams.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetComparisonKey() []values.Value {
	return p.comparisonKey
}

// GetResultValue returns the Value expression that constructs the merged
// output row, falling back to the FIRST stream's flowed object value when this
// plan carries none.
//
// The fallback is physicalMultiIntersectionWrapper's, adopted here now that
// the plan owns its child quantifiers: the wrapper answered
// innerQuants[0].GetFlowedObjectValue() because the plan had no quantifiers of
// its own to ask. It does now, so the wrapper holds no information the plan
// lacks and becomes deletable.
//
// The final arm defers to PlanExprBase rather than repeating its fresh
// stand-in, so a 0-stream plan answers exactly what every other plan answers.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetResultValue() values.Value {
	if p.resultValue != nil {
		return p.resultValue
	}
	if len(p.childQs) == 0 {
		return p.PlanExprBase.GetResultValue()
	}
	return p.childQs[0].GetFlowedObjectValue()
}

// GetResultType returns the result Value's type if a resultValue is
// set, or UnknownType otherwise.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetResultType() values.Type {
	if p.resultValue != nil {
		return p.resultValue.Type()
	}
	return values.UnknownType
}

// EqualsWithoutChildren matches MultiIntersectionOnValuesPlan with the same
// comparison key and resultValue by semantic Value identity (RFC-176 P2 —
// see semanticValueEquals).
func (p *RecordQueryMultiIntersectionOnValuesPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMultiIntersectionOnValuesPlan)
	if !ok {
		return false
	}
	if len(p.comparisonKey) != len(o.comparisonKey) {
		return false
	}
	for i, k := range p.comparisonKey {
		if !semanticValueEquals(k, o.comparisonKey[i]) {
			return false
		}
	}
	return semanticValueEquals(p.resultValue, o.resultValue)
}

// HashCodeWithoutChildren folds the type discriminator, comparison key
// values, and result value (semantic Value hashes — see writeValueHash).
func (p *RecordQueryMultiIntersectionOnValuesPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("multiintersectiononvaluesplan|"))
	for _, k := range p.comparisonKey {
		writeValueHash(h, k)
	}
	writeValueHash(h, p.resultValue)
	return h.Sum64()
}

// Explain renders MultiIntersection(child1, child2, ...; keys=[...]).
func (p *RecordQueryMultiIntersectionOnValuesPlan) Explain() string {
	children := p.GetChildren()
	parts := make([]string, len(children))
	for i, child := range children {
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

var (
	_ RecordQueryPlan                  = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryMultiIntersectionOnValuesPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real child quantifiers, overriding PlanExprBase's
// none. These are also what GetResultValue's nil-fallback reads.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers returns a copy ranging over the given child quantifiers —
// Java's copy-on-write withChildrenReferences. The receiver is never mutated,
// which is what keeps a memoized plan safe to share; the incoming slice is
// copied so the caller cannot alias the copy's storage either.
//
// The arity check matters more here than for a plain set operation:
// resultValue picks up one aggregate per stream by position, so a
// different-length child list would not describe the same row.
func (p *RecordQueryMultiIntersectionOnValuesPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
