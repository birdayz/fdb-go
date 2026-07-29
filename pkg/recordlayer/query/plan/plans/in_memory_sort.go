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

// NewRecordQueryInMemorySortPlanFromQuantifier builds an in-memory sort whose
// child is a supplied memo quantifier instead of a snapshot over a single plan.
// This makes the plan its own cascades expression carrying its child edge
// directly — the memo holds it without a physicalInMemorySortWrapper (RFC-184 W2).
//
// The sort RE-SORTS its input, so it does not care what order the child provides:
// unlike the ordering-DELEGATOR wrappers (which pin an ordered spine), it only
// needs the cheapest VALID child member for ANY ordering. When the emitter hands
// it the LIVE shared-group edge (ForEachQuantifier(innerRef)), GetInner resolves
// through planFromQuantifier → innerRef.Winner() — the group's OPTIMIZE-chosen
// cheapest member (unified_tasks.OptimizeGroupTask stamps the overall cost winner,
// ordering-agnostic). Cost (concretePlanCounts walks GetChildren → GetInner) and
// extraction (rebuild recurses the same edge) therefore resolve the SAME member,
// closing the cost-over-first / extract-over-best gap the wrapper carried
// (plan_expression.go's planFromQuantifier note). The sort keys are copied so the
// provided ordering (HintOrdering) stays stable across relinks.
func NewRecordQueryInMemorySortPlanFromQuantifier(innerQ expressions.Quantifier, sortKeys []SortKey) *RecordQueryInMemorySortPlan {
	keys := make([]SortKey, len(sortKeys))
	copy(keys, sortKeys)
	return &RecordQueryInMemorySortPlan{innerQ: innerQ, sortKeys: keys}
}

func (p *RecordQueryInMemorySortPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// sort ranges over. Since RFC-184 W2 the memo holds the bare plan (no
// physicalInMemorySortWrapper whose innerQuant field was read), this exposes the
// same edge for derivations and extraction.
func (p *RecordQueryInMemorySortPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
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
// (the incomplete-F21 case). Desc / NullsFirst are the direction.
//
// SortKey.Field is DISPLAY-ONLY and is NOT folded (RFC-197 item 3). It used to be,
// "so an explain-name difference still separates identities" — but a display-name
// difference IS NOT an identity difference, and folding it splits one plan into
// two memo members whenever two producers render the same baked key under
// different names. The ordinal-addressed ValueExpr is the identity; the string
// beside it is what Explain prints.
// structuralKey folds the InMemorySort identity: the ordered sort-key list via
// sortKeyEqual (Desc / NullsFirst + semantic ValueExpr). Drives both Equals and
// Hash.
func (p *RecordQueryInMemorySortPlan) structuralKey() *structuralKey {
	return newStructuralKey().SortKeys(p.sortKeys)
}

func (p *RecordQueryInMemorySortPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInMemorySortPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// sortKeyEqual reports semantic equality of two sort keys: the direction
// (Desc / NullsFirst) and the semantic ValueExpr — the plan-time-baked key, whose
// ordinals are the column identity. The display Field is excluded (RFC-197 item
// 3), exactly as Java excludes the per-accessor name from ResolvedAccessor
// equality (FieldValue.java:676-685). Pairs with HashCodeWithoutChildren, which
// folds the identical set — preserving the equal⟹same-hash memo invariant.
func sortKeyEqual(a, b SortKey) bool {
	return a.Desc == b.Desc &&
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

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The sort carries its child as a single memo edge, so the relink is
// a quantifier swap: WithQuantifiers copies the receiver (preserving the sort
// keys) and re-resolves GetInner through the new singleton reference. This
// replaces physicalInMemorySortWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field forced a findBestPhysicalPlan constructor rebuild. Because
// extraction hands qs[0] the child group's already-resolved winner (a singleton
// holding the cheapest member), a pure quantifier swap resolves the exact plan
// the cost model costed — no separate best-member re-pick is needed.
func (p *RecordQueryInMemorySortPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInMemorySortPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
