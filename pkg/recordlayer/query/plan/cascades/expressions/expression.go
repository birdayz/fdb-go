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
// This is the seed of Track B1 (RFC-022 §4.1). The minimal viable scope
// — interface + Quantifier + Reference + AliasMap + LogicalFilterExpression
// — gates B3 (Memo & references), B4 (Cost), B5 (Rules). Subsequent
// shifts will land the rest of the 8 logical operator subclasses
// (LogicalProjection, LogicalSort, LogicalUnion, LogicalDistinct,
// LogicalIntersection, LogicalTypeFilter, Select) and the 4 DML
// expressions (Insert, Update, Delete, TableFunction).
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
}

// SemanticEquals walks two expression trees and reports whether they
// are semantically equal under `aliases`. The walk:
//   - early-outs on identity, type mismatch, or
//     EqualsWithoutChildren disagreement;
//   - pairs the children positionally (the seed does NOT enumerate
//     alias permutations — that's a B2 enhancement); for each child
//     pair, recurses with `aliases` extended by binding the two
//     Quantifiers' aliases.
//
// Positional pairing is correct for operators where children have a
// canonical order (LogicalFilter has 1 child, LogicalSort has 1 child,
// LogicalUnion's children are positionally ordered). For commutative
// operators (LogicalIntersection) the planner is responsible for
// canonicalising child order before semantic-equals. The full
// alias-permutation matcher will land alongside MatchableSortExpression
// when B2/B3 need it.
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
	// Bind each child pair's aliases for the recursive call.
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
