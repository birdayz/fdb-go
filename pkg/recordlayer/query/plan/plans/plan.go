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
//   - RecordQuerySortPlan — sorts an inner plan's row stream.
//
// The seed deliberately omits Java's full surface (Execute method,
// PlanHashable, continuation handling, complex covering-index
// machinery) — those land as the rule chain that produces these
// plans starts consuming them. The seed is the type structure +
// node-info equality so PrimaryScanRule / ImplementFilterRule /
// ImplementSortRule (all B5 Batch A) have a target to yield into.
//
// Why a separate sub-package vs cascades/expressions/: physical and
// logical plan trees live in different namespaces in Java. A
// RelationalExpression is logical (rule input); a RecordQueryPlan
// is physical (rule output, executor input). Mixing them risks
// pattern-match confusion in rules. Separation also matches Java's
// package layout — code review across languages stays tractable.
package plans

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

// RecordQueryPlan is the root interface for every physical plan
// node. Mirrors Java's `RecordQueryPlan` interface — implementations
// produce a record stream when executed against an FDBRecordStore.
//
// The seed exposes node-information accessors (GetResultType,
// GetChildren, EqualsWithoutChildren, HashCodeWithoutChildren) and
// an Explain method for diagnostic rendering. Execute is NOT in the
// seed surface — wiring to FDBRecordStore is a follow-up shift gated
// on the rule chain being able to produce these plans end-to-end.
type RecordQueryPlan interface {
	// GetResultType returns the rich Type of rows this plan emits.
	// Always a RelationType.
	GetResultType() values.Type

	// GetChildren returns this plan's input plans, in stable order.
	// Read-only; callers must not mutate.
	GetChildren() []RecordQueryPlan

	// EqualsWithoutChildren reports whether this plan's node-
	// information matches `other`'s. Children are not consulted —
	// caller's job (typically by recursing into GetChildren).
	EqualsWithoutChildren(other RecordQueryPlan) bool

	// HashCodeWithoutChildren returns the structural hash of this
	// node's node-information. Must be consistent with
	// EqualsWithoutChildren: x.Equals(y) implies x.Hash() == y.Hash().
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
	if !a.EqualsWithoutChildren(b) {
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
