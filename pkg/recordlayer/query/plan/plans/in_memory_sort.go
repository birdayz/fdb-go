// Go extension — no Java equivalent.
//
// Java's Cascades has no physical sort operator; RemoveSortRule
// eliminates the sort via index ordering or fails the query.
// This plan materializes the inner result and sorts in memory.
package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SortKey is a sort key + direction for in-memory sorting. ValueExpr is
// REQUIRED: it carries the key's plan-time-baked Value, which the
// executor evaluates POSITIONALLY per row. The field-only form (a Field lookup
// with a nil ValueExpr) is no longer supported — the runtime name fallback was
// deleted, so the executor rejects a nil ValueExpr as a malformed plan (loud,
// never a name read; pinned by TestSortCursor_UnbakedKeyIsLoud). Field is
// DISPLAY-ONLY (Explain + ordering-hint name match). Every planner path that
// builds a SortKey sets ValueExpr unconditionally (rule_implement_in_memory_sort,
// rule_implement_streaming_agg).
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // REQUIRED: the plan-time-baked key Value, evaluated per row
}

// RecordQueryInMemorySortPlan materializes the inner plan's output and
// sorts it in memory.
//
// Go extension — Java's Cascades has no physical sort operator.
//
// Cascades still optimizes the inner plan (index scans, predicate
// pushdown, join ordering). Only the final sort is post-processed.
// The cost model ensures index-based sort elimination is preferred
// when an index exists.
type RecordQueryInMemorySortPlan struct {
	PlanExprBase
	innerQ   expressions.Quantifier
	sortKeys []SortKey
}

func NewRecordQueryInMemorySortPlan(inner RecordQueryPlan, sortKeys []SortKey) *RecordQueryInMemorySortPlan {
	keys := make([]SortKey, len(sortKeys))
	copy(keys, sortKeys)
	return &RecordQueryInMemorySortPlan{innerQ: QuantifierOverPlan(inner), sortKeys: keys}
}

func (p *RecordQueryInMemorySortPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}
func (p *RecordQueryInMemorySortPlan) GetSortKeys() []SortKey { return p.sortKeys }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryInMemorySortPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultType returns the inner plan's result type: an in-memory sort
// reorders rows but preserves the inner's row shape, so it flows the inner's
// type through (matching pass-through plans like Filter / Fetch). Nil inner
// degrades to UnknownType.
func (p *RecordQueryInMemorySortPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

func (p *RecordQueryInMemorySortPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the sort keys by SEMANTIC identity (RFC-176 P2
// / F21): each key's plan-time-baked ValueExpr is compared via the plans-package
// semanticValueEquals (semantic Value equality under the empty alias map), not
// by pointer. Two independently-built sort keys over the same Value are the same
// plan — pointer identity would spuriously split them into distinct memo members
// (the incomplete-F21 case). Field is DISPLAY-ONLY but folded so an explain-name
// difference still separates identities; Desc / NullsFirst are the direction.
// structuralKey folds the InMemorySort identity: the ordered sort-key list via
// sortKeyEqual (display Field + Desc / NullsFirst + semantic ValueExpr). Drives
// both Equals and Hash.
func (p *RecordQueryInMemorySortPlan) structuralKey() *structuralKey {
	return newStructuralKey().SortKeys(p.sortKeys)
}

func (p *RecordQueryInMemorySortPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInMemorySortPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// sortKeyEqual reports semantic equality of two sort keys: display Field,
// direction (Desc / NullsFirst), and the semantic ValueExpr. Pairs with
// HashCodeWithoutChildren, which folds the identical set — preserving the
// equal⟹same-hash memo invariant.
func sortKeyEqual(a, b SortKey) bool {
	return a.Field == b.Field &&
		a.Desc == b.Desc &&
		a.NullsFirst == b.NullsFirst &&
		semanticValueEquals(a.ValueExpr, b.ValueExpr)
}

func (p *RecordQueryInMemorySortPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("inmemsort|")
}

func (p *RecordQueryInMemorySortPlan) Explain() string {
	keys := make([]string, len(p.sortKeys))
	for i, k := range p.sortKeys {
		dir := "ASC"
		if k.Desc {
			dir = "DESC"
		}
		keys[i] = k.Field + " " + dir
	}
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("InMemorySort([%s], %s)", strings.Join(keys, ", "), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryInMemorySortPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryInMemorySortPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryInMemorySortPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryInMemorySortPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInMemorySortPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
