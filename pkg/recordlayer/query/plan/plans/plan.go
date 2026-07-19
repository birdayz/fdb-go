// Package plans is the physical-plan ("RecordQueryPlan") hierarchy
// the Cascades planner emits after Batch A rules implement logical
// expressions as concrete query operators.
//
// Mirrors Java's `com.apple.foundationdb.record.query.plan.plans`
// package. Java has 74 RecordQueryPlan classes; the seed ports the
// minimum set Batch A's first rules need:
//
//   - RecordQueryScanPlan — primary-key scan over a record type.
//   - RecordQueryFilterPlan — applies a QueryPredicate to an inner
//     plan's row stream.
//   - RecordQueryInMemorySortPlan — sorts an inner plan's row stream in
//     memory (Go extension; Java's Cascades eliminates the sort via index
//     ordering, RemoveSortRule/ImplementSortRule).
//
// The seed deliberately omits Java's full surface (Execute method,
// PlanHashable, continuation handling, complex covering-index
// machinery) — those land as the rule chain that produces these
// plans starts consuming them. The seed is the type structure +
// node-info equality so PrimaryScanRule / ImplementFilterRule /
// ImplementInMemorySortRule have a target to yield into.
//
// Why a separate sub-package vs cascades/expressions/: to mirror Java's
// package layout, so code review across the two languages stays tractable.
//
// This used to say something stronger and WRONG — that "physical and
// logical plan trees live in different namespaces in Java", so a
// RecordQueryPlan is not a RelationalExpression. Java says the opposite:
//
//	QueryPlan<T> extends PlanHashable, RelationalExpression  (QueryPlan.java:51)
//	RecordQueryPlan extends QueryPlan<…>              (RecordQueryPlan.java:73)
//
// Java separates the PACKAGE and unifies the HIERARCHY; the old comment
// conflated the two. That misreading is where the 23-file
// physical_*_wrapper.go layer came from — adapters existing only to present
// a plan as an expression — and with it the nil-inner "shell" bug class,
// since a wrapper and its wrapped plan each stored the parent->child edge
// and could disagree. RecordQueryPlan now embeds RelationalExpression
// directly (RFC-183 P5).
package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryPlan is the root interface for every physical plan
// node. Mirrors Java's `RecordQueryPlan` interface — implementations
// produce a record stream when executed against an FDBRecordStore.
//
// The seed exposes node-information accessors (GetResultType,
// GetChildren, EqualsPlanWithoutChildren, HashCodeWithoutChildren) and
// an Explain method for diagnostic rendering. Execute is NOT in the
// seed surface — wiring to FDBRecordStore is a follow-up shift gated
// on the rule chain being able to produce these plans end-to-end.
type RecordQueryPlan interface {
	// A plan IS a RelationalExpression — Java's
	// `QueryPlan<T> extends PlanHashable, RelationalExpression`
	// (QueryPlan.java:51), inherited by RecordQueryPlan
	// (RecordQueryPlan.java:73). Embedding it here is the Go spelling of
	// that `extends`, and it is what lets a plan be a memo member and hold
	// its child as a Quantifier instead of a raw pointer.
	//
	// Note this supplies HashCodeWithoutChildren, which both interfaces
	// declare with the same signature.
	expressions.RelationalExpression

	// GetResultType returns the rich Type of rows this plan emits.
	// Always a RelationType.
	GetResultType() values.Type

	// GetChildren returns this plan's input plans, in stable order.
	// Read-only; callers must not mutate.
	GetChildren() []RecordQueryPlan

	// EqualsPlanWithoutChildren reports whether this plan's node-
	// information matches `other`'s. Children are not consulted —
	// caller's job (typically by recursing into GetChildren).
	EqualsPlanWithoutChildren(other RecordQueryPlan) bool

	// HashCodeWithoutChildren returns the structural hash of this
	// node's node-information. Must be consistent with
	// EqualsPlanWithoutChildren: x.Equals(y) implies x.Hash() == y.Hash().
	HashCodeWithoutChildren() uint64

	// Explain returns a single-line human-readable label for this
	// plan node. Implementations should match Java's
	// `Plan.toString()` shape where reasonable.
	Explain() string
}

// Equals walks two plan trees and reports semantic equality —
// node-info match plus pairwise child equality. The plans seed
// doesn't have alias-aware comparison (no Quantifiers in the
// physical layer); positional pairing only.
//
// Returns true if both nil.
func Equals(a, b RecordQueryPlan) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !a.EqualsPlanWithoutChildren(b) {
		return false
	}
	ac := a.GetChildren()
	bc := b.GetChildren()
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if !Equals(ac[i], bc[i]) {
			return false
		}
	}
	return true
}

// Walk invokes `visit` on `p` and (if visit returns true) recursively
// on every reachable RecordQueryPlan via GetChildren. Returning false
// from `visit` short-circuits the walk for that subtree (siblings
// + ancestors continue).
//
// Counterpart to expressions.Walk for the logical side.
func Walk(p RecordQueryPlan, visit func(RecordQueryPlan) bool) {
	if p == nil {
		return
	}
	if !visit(p) {
		return
	}
	for _, c := range p.GetChildren() {
		Walk(c, visit)
	}
}

// Size returns the total node count of the plan tree rooted at `p`,
// including `p` itself. Returns 0 for nil.
func Size(p RecordQueryPlan) int {
	count := 0
	Walk(p, func(_ RecordQueryPlan) bool {
		count++
		return true
	})
	return count
}
