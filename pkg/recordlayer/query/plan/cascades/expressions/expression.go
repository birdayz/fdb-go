// Package expressions ports the Cascades-side relational expression
// hierarchy from Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions`.
//
// A RelationalExpression represents a node in the logical query plan
// tree — a stream of records with a known result Type. The hierarchy is
// the planner's working tree: each expression has zero or more children
// (modelled as Quantifiers ranging over References), a result Value
// describing the row shape it emits, and a small bundle of
// node-information fields specific to the operator.
//
// Track B1 (RFC-022 §4.1) seed shipped dayshift-58. Concrete operators:
//
//   - Logical (8): LogicalFilterExpression, LogicalProjectionExpression,
//     LogicalSortExpression, LogicalTypeFilterExpression,
//     LogicalDistinctExpression, LogicalUnionExpression,
//     LogicalIntersectionExpression, SelectExpression.
//   - DML (3): InsertExpression, UpdateExpression, DeleteExpression.
//   - Leaf (1): FullUnorderedScanExpression.
//
// Foundation types: Quantifier (ForEach kind), Reference (single-member
// equivalence class with EqualsWithoutChildren-and-children-aware
// dedup), AliasMap (CorrelationIdentifier bijection).
//
// Walk infrastructure: SemanticEquals (positional + permutation-aware
// for ChildrenAsSet operators with cap at MaxPermutationChildren=8).
//
// Optional interface: RelationalExpressionWithPredicates — implemented
// by LogicalFilterExpression and SelectExpression for generic predicate-
// walker rules.
//
// Remaining for full B1: TableFunctionExpression (gated on
// StreamingValue port), Java's TempTableInsert / TempTableScan /
// RecursiveUnion / Explode / GroupBy expressions (not in TODO.md's
// listed scope), TranslationMap-based rebasing for push-down rules
// (B5 follow-on), MaxMatchMap / partial-match infrastructure (B3
// follow-on).
package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RelationalExpression is the root interface for every node in the
// logical query plan tree. Implementations are immutable.
//
// Surface ported from Java's RelationalExpression:
//
//   - GetResultValue: the Value describing the rows this expression
//     emits. The Value's Type is necessarily a RelationType.
//   - GetQuantifiers: the children — every concrete operator returns
//     its inputs as a list of Quantifiers, in a stable order.
//   - CanCorrelate: whether this operator anchors a correlation (i.e.
//     whether evaluating one quantifier may bind values seen by
//     another). Defaults to false; SelectExpression and JOIN-shaped
//     expressions return true.
//   - GetCorrelatedToWithoutChildren: the set of CorrelationIdentifiers
//     this expression's node-information references (predicates,
//     projection list, sort key, etc.) — NOT including children's
//     correlations. Used by the planner to compute correlation order.
//   - EqualsWithoutChildren / HashCodeWithoutChildren: shape equality
//     of this node alone (predicate equality, type equality, …),
//     ignoring children. Two children are compared via SemanticEquals
//     under an alias map. Together they let the memo de-duplicate
//     equivalent expressions.
//
// The full Java surface (TranslationMap rewriting, MaxMatchMap,
// findMatches, PlannerGraph rendering, PartiallyOrderedSet correlation
// order) is deliberately not in the seed — these depend on combinatorics
// and rule machinery that lands in B2 / B3 / B5. They will be added as
// rules need them.
type RelationalExpression interface {
	// GetResultValue returns the Value whose Type describes the rows
	// this expression emits. For LogicalFilter this is the inner
	// Quantifier's flowed object value; for LogicalProjection it's a
	// RecordConstructor over the projection list; etc.
	GetResultValue() values.Value

	// GetQuantifiers returns the children of this expression in a
	// stable, defined order. The slice is read-only; callers must
	// not mutate it.
	GetQuantifiers() []Quantifier

	// CanCorrelate reports whether this expression anchors a
	// correlation between its quantifiers. For non-anchoring
	// expressions, evaluating one quantifier never binds values seen
	// by another. Defaults to false; only Select-shaped expressions
	// override.
	CanCorrelate() bool

	// GetCorrelatedToWithoutChildren returns the CorrelationIdentifiers
	// this expression's node-information depends on, NOT including
	// transitive correlations through children. Returned set is
	// read-only.
	GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{}

	// EqualsWithoutChildren reports whether this expression's
	// node-information matches `other`'s node-information, treating
	// CorrelationIdentifiers as equal under `aliases`. Children are
	// not consulted — that's the caller's job (typically by recursing
	// via SemanticEquals).
	EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool

	// HashCodeWithoutChildren is the structural hash of this node's
	// node-information, ignoring children. Must be consistent with
	// EqualsWithoutChildren under the empty alias map: x.Equals(y, ∅)
	// implies x.HashCode() == y.HashCode().
	HashCodeWithoutChildren() uint64

	// ChildrenAsSet reports whether this expression's children are
	// commutative — semantically equal regardless of order. Mirrors
	// Java's RelationalExpressionWithChildren.ChildrenAsSet marker
	// interface. When true, SemanticEquals enumerates child
	// permutations; when false, children are paired positionally.
	//
	// Default false. Overridden by LogicalUnion / LogicalIntersection
	// / SelectExpression — the operators whose children are SQL
	// set-bag-shaped or whose Java code marks them as ChildrenAsSet.
	ChildrenAsSet() bool

	// WithQuantifiers returns a copy of this expression with the
	// given quantifiers replacing the original children. The new
	// quantifiers must be in the same positional order as
	// GetQuantifiers(). Leaf expressions (no quantifiers) return
	// themselves.
	//
	// Ports Java's RelationalExpression.withQuantifiers.
	WithQuantifiers(quantifiers []Quantifier) RelationalExpression
}

// SemanticEquals walks two expression trees and reports whether they
// are semantically equal under `aliases`. The walk:
//   - early-outs on identity, type mismatch, or
//     EqualsWithoutChildren disagreement;
//   - if both sides report ChildrenAsSet, enumerates permutations of
//     `b`'s children against `a`'s; otherwise pairs positionally;
//   - for each candidate pairing, extends `aliases` by binding the
//     two Quantifiers' aliases and recurses into each pair.
//
// Permutation enumeration is O(N!), which is fine for N up to ~6
// (queries don't typically have more set-shaped children than that).
// The first matching permutation wins; if none match, returns false.
func SemanticEquals(a, b RelationalExpression, aliases *AliasMap) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !a.EqualsWithoutChildren(b, aliases) {
		return false
	}
	aQs := a.GetQuantifiers()
	bQs := b.GetQuantifiers()
	if len(aQs) != len(bQs) {
		return false
	}
	if len(aQs) == 0 {
		return true
	}
	// ChildrenAsSet must agree on both sides (marker is per-class, so
	// they must match if EqualsWithoutChildren passed — but explicit
	// guard keeps the contract local).
	//
	// Permutation enumeration is O(N!). Beyond MaxPermutationChildren
	// the cost gets prohibitive (8! = 40320, 12! = 479M); fall back to
	// positional pairing in that case. The planner is free to
	// canonicalise large commutative children before semantic-equals
	// to recover dedup precision; the seed prefers cheap-and-imprecise
	// to slow-and-correct on this rare case.
	if a.ChildrenAsSet() && b.ChildrenAsSet() && len(aQs) <= MaxPermutationChildren {
		return matchChildrenPermuted(aQs, bQs, aliases)
	}
	return matchChildrenPositional(aQs, bQs, aliases)
}

// MaxPermutationChildren caps the number of commutative children
// SemanticEquals will permutation-enumerate. Tuning rationale:
// 8! = 40,320 is the practical ceiling on per-call CPU cost
// (assuming ~1µs per inner-recursion). Real query shapes very rarely
// exceed 4-5 set-shaped children; the cap exists as a safeguard, not
// a bottleneck in normal usage.
const MaxPermutationChildren = 8

// matchChildrenPositional pairs children index-by-index and recurses.
func matchChildrenPositional(aQs, bQs []Quantifier, aliases *AliasMap) bool {
	pairs := make([]values.CorrelationIdentifier, 0, 2*len(aQs))
	for i := range aQs {
		pairs = append(pairs, aQs[i].GetAlias(), bQs[i].GetAlias())
	}
	composed := aliases.Compose(AliasMapOf(pairs...))
	for i := range aQs {
		if !SemanticEquals(aQs[i].GetRangesOver().Get(), bQs[i].GetRangesOver().Get(), composed) {
			return false
		}
	}
	return true
}

// matchChildrenPermuted enumerates permutations of bQs against aQs;
// returns true if any permutation yields a successful match. O(N!).
func matchChildrenPermuted(aQs, bQs []Quantifier, aliases *AliasMap) bool {
	n := len(aQs)
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	return permute(indices, 0, func(perm []int) bool {
		// Try this perm: aQs[i] pairs with bQs[perm[i]].
		pairs := make([]values.CorrelationIdentifier, 0, 2*n)
		for i := 0; i < n; i++ {
			pairs = append(pairs, aQs[i].GetAlias(), bQs[perm[i]].GetAlias())
		}
		// AliasMapOf panics on duplicate sources/targets. With a real
		// permutation each alias appears exactly once on each side, so
		// the bijection invariant holds — but defensively recover here
		// to skip over any pathological self-permuting tree.
		var composed *AliasMap
		func() {
			defer func() { _ = recover() }()
			composed = aliases.Compose(AliasMapOf(pairs...))
		}()
		if composed == nil {
			return false
		}
		for i := 0; i < n; i++ {
			if !SemanticEquals(aQs[i].GetRangesOver().Get(), bQs[perm[i]].GetRangesOver().Get(), composed) {
				return false
			}
		}
		return true
	})
}

// permute enumerates permutations of `arr` in place, calling `accept`
// on each. Stops at the first true return. Returns true iff any call
// returned true. Standard recursive Heap-style enumeration.
func permute(arr []int, k int, accept func(perm []int) bool) bool {
	if k == len(arr) {
		return accept(arr)
	}
	for i := k; i < len(arr); i++ {
		arr[k], arr[i] = arr[i], arr[k]
		if permute(arr, k+1, accept) {
			arr[k], arr[i] = arr[i], arr[k]
			return true
		}
		arr[k], arr[i] = arr[i], arr[k]
	}
	return false
}
