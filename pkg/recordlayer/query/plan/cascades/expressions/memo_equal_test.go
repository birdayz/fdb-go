package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestMemoEqual_InternsAliasVariants is the RFC-039/PR-A core property: two
// expressions identical except for their quantifier alias (referenced in
// node-info) are MemoEqual — because memoEqual builds the node's own quantifier
// alias map and feeds it to the (foundation's) alias-aware EqualsWithoutChildren.
// SemanticEquals (which passes only the empty incoming map at the top) does NOT
// see them equal — that's the gap memoEqual closes.
func TestMemoEqual_InternsAliasVariants(t *testing.T) {
	t.Parallel()
	scanRef := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	filter := func(k int64) RelationalExpression {
		q := ForEachQuantifier(scanRef)
		pred := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(q.GetAlias()),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: k}})
		return NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	}
	a := filter(1) // fresh alias q$N
	b := filter(1) // fresh alias q$M, same shape

	if !MemoEqual(a, b) {
		t.Fatal("alias-variant filters must be MemoEqual (the activation property)")
	}
	// Contrast: plain SemanticEquals (empty map at top) misses them — the gap.
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("precondition: SemanticEquals should NOT see alias-variants equal (top-level empty map) — test vacuous otherwise")
	}
	// Negative: different constant ⇒ not MemoEqual.
	if MemoEqual(a, filter(2)) {
		t.Fatal("filters with different constants must not be MemoEqual")
	}
}

// TestMemoEqual_DistinctOuterCorrelationsDoNotIntern pins the external-
// correlation guard (correlatedToMatches / expressionCorrelatedTo) — the whole
// reason that machinery exists, and the motivating case in Java's own comment
// (Reference.java:755-762: a node correlated to outer `a.x` must NOT be
// memo-equal to one correlated to outer `b.y`). Two filters identical in shape
// and alias-invariant hash, differing ONLY in which OUTER alias their predicate
// references, must stay DISTINCT — otherwise a correlated subquery referencing
// the wrong outer binding would be silently shared.
func TestMemoEqual_DistinctOuterCorrelationsDoNotIntern(t *testing.T) {
	t.Parallel()
	scanRef := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	// filterCorrelatedTo builds Filter(QOV(localQ) = QOV(outer), →scan): the
	// comparison operand QOV(outer) references an alias NOT bound by the
	// filter's own quantifier, so the filter is EXTERNALLY correlated to outer.
	filterCorrelatedTo := func(outer values.CorrelationIdentifier) RelationalExpression {
		q := ForEachQuantifier(scanRef)
		pred := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(q.GetAlias()),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewQuantifiedObjectValue(outer)})
		return NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	}
	a := filterCorrelatedTo(values.NamedCorrelationIdentifier("a"))
	b := filterCorrelatedTo(values.NamedCorrelationIdentifier("b"))

	// Precondition: the alias-invariant hash is EQUAL, so both reach the
	// correlatedToMatches guard. Without this, an unequal hash would short-
	// circuit MemoEqual and the test would prove nothing about the guard.
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("precondition: alias-invariant hash must be equal so the external-correlation guard is reached — test vacuous otherwise")
	}
	if MemoEqual(a, b) {
		t.Fatal("filters correlated to DIFFERENT outer aliases must NOT be MemoEqual (external-correlation guard)")
	}
	// Sanity: SAME outer alias ⇒ MemoEqual (guard passes; local quantifier is a
	// permitted alias variant). Proves the guard rejects on the correlation
	// difference, not on something incidental to the construction.
	if !MemoEqual(filterCorrelatedTo(values.NamedCorrelationIdentifier("a")),
		filterCorrelatedTo(values.NamedCorrelationIdentifier("a"))) {
		t.Fatal("filters correlated to the SAME outer alias must be MemoEqual")
	}
}
