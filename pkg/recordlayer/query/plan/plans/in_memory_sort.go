// Go extension — no Java equivalent.
//
// Java's Cascades has no physical sort operator; RemoveSortRule
// eliminates the sort via index ordering or fails the query.
// This plan materializes the inner result and sorts in memory.
package plans

import (
	"fmt"
	"hash/fnv"
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
	inner    RecordQueryPlan
	sortKeys []SortKey
}

func NewRecordQueryInMemorySortPlan(inner RecordQueryPlan, sortKeys []SortKey) *RecordQueryInMemorySortPlan {
	keys := make([]SortKey, len(sortKeys))
	copy(keys, sortKeys)
	return &RecordQueryInMemorySortPlan{inner: inner, sortKeys: keys}
}

func (p *RecordQueryInMemorySortPlan) GetInner() RecordQueryPlan { return p.inner }
func (p *RecordQueryInMemorySortPlan) GetSortKeys() []SortKey    { return p.sortKeys }

// GetResultType returns the inner plan's result type: an in-memory sort
// reorders rows but preserves the inner's row shape, so it flows the inner's
// type through (matching pass-through plans like Filter / Fetch). Nil inner
// degrades to UnknownType.
func (p *RecordQueryInMemorySortPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

func (p *RecordQueryInMemorySortPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares the sort keys by SEMANTIC identity (RFC-176 P2
// / F21): each key's plan-time-baked ValueExpr is compared via the plans-package
// semanticValueEquals (semantic Value equality under the empty alias map), not
// by pointer. Two independently-built sort keys over the same Value are the same
// plan — pointer identity would spuriously split them into distinct memo members
// (the incomplete-F21 case). Field is DISPLAY-ONLY but folded so an explain-name
// difference still separates identities; Desc / NullsFirst are the direction.
func (p *RecordQueryInMemorySortPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInMemorySortPlan)
	if !ok {
		return false
	}
	if len(p.sortKeys) != len(o.sortKeys) {
		return false
	}
	for i := range p.sortKeys {
		if !sortKeyEqual(p.sortKeys[i], o.sortKeys[i]) {
			return false
		}
	}
	return true
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
	h := fnv.New64a()
	h.Write([]byte("inmemsort|"))
	for _, k := range p.sortKeys {
		h.Write([]byte(k.Field))
		h.Write([]byte{0})
		if k.Desc {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		if k.NullsFirst {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		// Fold the semantic ValueExpr so equal⟹same-hash holds with
		// sortKeyEqual (which compares the ValueExpr semantically).
		writeValueHash(h, k.ValueExpr)
	}
	return h.Sum64()
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
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	return fmt.Sprintf("InMemorySort([%s], %s)", strings.Join(keys, ", "), inner)
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

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryInMemorySortPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}
