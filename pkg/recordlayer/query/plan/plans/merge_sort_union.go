package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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
//
// The legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `inners []RecordQueryPlan` slice they replace was a second storage location
// for the same edges. RFC-183 P5 step 2.
type RecordQueryMergeSortUnionPlan struct {
	PlanExprBase
	childQs          []expressions.Quantifier
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
	copiedKeys := make([]values.Value, len(comparisonKeys))
	copy(copiedKeys, comparisonKeys)
	return &RecordQueryMergeSortUnionPlan{
		childQs:          QuantifiersOverPlans(inners),
		comparisonKeys:   copiedKeys,
		reverse:          reverse,
		removeDuplicates: removeDuplicates,
	}
}

// GetInners returns the legs, dereferenced through the quantifiers and in
// merge order — which the merge itself depends on.
func (p *RecordQueryMergeSortUnionPlan) GetInners() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

func (p *RecordQueryMergeSortUnionPlan) GetComparisonKeys() []values.Value { return p.comparisonKeys }
func (p *RecordQueryMergeSortUnionPlan) IsReverse() bool                   { return p.reverse }
func (p *RecordQueryMergeSortUnionPlan) RemovesDuplicates() bool           { return p.removeDuplicates }

func (p *RecordQueryMergeSortUnionPlan) GetResultType() values.Type {
	if len(p.childQs) == 0 {
		return values.UnknownType
	}
	return planFromQuantifier(p.childQs[0]).GetResultType()
}

func (p *RecordQueryMergeSortUnionPlan) GetChildren() []RecordQueryPlan { return p.GetInners() }

// EqualsWithoutChildren compares reverse + removeDuplicates flags and the
// comparison keys (semantic Value identity — see semanticValueEquals). The
// keys join equality per RFC-176 §1: before P2, equality checked only the key
// COUNT while the hash folded the full keys — different-key plans compared
// equal yet hashed apart, a live plan-level equal⟹same-hash violation.
func (p *RecordQueryMergeSortUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMergeSortUnionPlan)
	if !ok {
		return false
	}
	if p.reverse != o.reverse || p.removeDuplicates != o.removeDuplicates {
		return false
	}
	if len(p.comparisonKeys) != len(o.comparisonKeys) {
		return false
	}
	for i, k := range p.comparisonKeys {
		if !semanticValueEquals(k, o.comparisonKeys[i]) {
			return false
		}
	}
	return true
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
		writeValueHash(h, k)
	}
	return h.Sum64()
}

func (p *RecordQueryMergeSortUnionPlan) Explain() string {
	inners := p.GetInners()
	parts := make([]string, len(inners))
	for i, inner := range inners {
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

var (
	_ RecordQueryPlan                  = (*RecordQueryMergeSortUnionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryMergeSortUnionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryMergeSortUnionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real leg quantifiers, overriding PlanExprBase's
// none.
func (p *RecordQueryMergeSortUnionPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers —
// Java's copy-on-write withChildrenReferences. The receiver is never mutated,
// which is what keeps a memoized plan safe to share; the incoming slice is
// copied so the caller cannot alias the copy's storage either.
func (p *RecordQueryMergeSortUnionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// ChildrenAsSet reports that the legs of this set operation are commutative,
// mirroring physicalMergeSortUnionWrapper.
func (p *RecordQueryMergeSortUnionPlan) ChildrenAsSet() bool { return true }
