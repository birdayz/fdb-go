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

// TestMemoEqual_ChildrenAsSet_PermutationBranch exercises the ChildrenAsSet
// permutation path of matchChildrenInMemo — load-bearing for join-order
// enumeration (PR-C) and otherwise unexercised (LogicalFilter et al. report
// ChildrenAsSet=false → positional path only). LogicalUnion is ChildrenAsSet
// (UNION is commutative), so two unions over the same child SET in swapped
// order must be MemoEqual via the permutation match. Distinct scans (T, U)
// make the positional permutation [0,1] FAIL — only [1,0] matches — so the
// test genuinely drives the permute fallback, not just the first attempt.
func TestMemoEqual_ChildrenAsSet_PermutationBranch(t *testing.T) {
	t.Parallel()
	scanT := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	scanU := InitialOf(NewFullUnorderedScanExpression([]string{"U"}, values.UnknownType))
	union := func(refs ...*Reference) RelationalExpression {
		qs := make([]Quantifier, len(refs))
		for i, r := range refs {
			qs[i] = ForEachQuantifier(r)
		}
		return NewLogicalUnionExpression(qs)
	}
	a := union(scanT, scanU)
	b := union(scanU, scanT) // swapped child order ⇒ only the permutation branch can match

	if !MemoEqual(a, b) {
		t.Fatal("ChildrenAsSet union with swapped child order must be MemoEqual (permutation branch)")
	}
	// Negative: a different child SET (two T's, no U) ⇒ no permutation matches.
	if MemoEqual(union(scanT, scanT), a) {
		t.Fatal("union over a different child set must NOT be MemoEqual")
	}
}

// TestMemoEqual_OuterJoinNotChildrenAsSet pins REVIEW.md #215: SelectExpression's
// ChildrenAsSet marker must reflect actual commutability, not be true for every
// join. A LEFT/FULL OUTER join is NOT invariant under quantifier permutation
// (NULL-extension is directional: `A LEFT JOIN B` != `B LEFT JOIN A`), so swapped
// outer joins must NOT be MemoEqual — otherwise matchChildrenInMemo permutes them
// and memoizeNonLeaf interns the two directions into one Reference. INNER/CROSS
// stay commutative. Distinct scans (T1, T2) make the positional permutation [0,1]
// fail, so MemoEqual can only succeed via the ChildrenAsSet permutation branch —
// the exact branch the fix gates on join type.
func TestMemoEqual_OuterJoinNotChildrenAsSet(t *testing.T) {
	t.Parallel()
	scanT1 := InitialOf(NewFullUnorderedScanExpression([]string{"T1"}, values.UnknownType))
	scanT2 := InitialOf(NewFullUnorderedScanExpression([]string{"T2"}, values.UnknownType))
	mkJoin := func(jt JoinType) *SelectExpression {
		q1 := NamedForEachQuantifier(values.NamedCorrelationIdentifier("T1"), scanT1)
		q2 := NamedForEachQuantifier(values.NamedCorrelationIdentifier("T2"), scanT2)
		rv := values.NewJoinMergeAllValue(q1.GetAlias(), q2.GetAlias())
		return NewSelectExpressionWithJoinType(rv, []Quantifier{q1, q2}, nil, []string{"T1", "T2"}, jt)
	}

	// INNER is commutative: swapped order interns (drives the permutation branch,
	// since distinct scans make positional [0,1] fail). This is the positive
	// control — it proves the permutation machinery works and that the negatives
	// below fail because of the join-type narrowing, not an incidental mismatch.
	innerAB := mkJoin(JoinInner)
	innerBA := innerAB.WithSwappedQuantifiers()
	if !MemoEqual(innerAB, innerBA) {
		t.Fatal("INNER join with swapped quantifiers must be MemoEqual (commutative, permutation branch)")
	}

	// LEFT OUTER is directional: swapped order must NOT intern.
	leftAB := mkJoin(JoinLeftOuter)
	leftBA := leftAB.WithSwappedQuantifiers()
	// Precondition: node-info hash is equal (joinType/resultValue/predicates match),
	// so MemoEqual is actually reached and returns false on the child/permutation
	// path — not short-circuited by the hash gate. Test is vacuous otherwise.
	if leftAB.HashCodeWithoutChildren() != leftBA.HashCodeWithoutChildren() {
		t.Fatal("precondition: swapped LEFT OUTER joins must share node-info hash — test vacuous otherwise")
	}
	if MemoEqual(leftAB, leftBA) {
		t.Fatal("swapped LEFT OUTER joins must NOT be MemoEqual (directional — ChildrenAsSet must be false)")
	}

	// FULL OUTER keeps the original left/right column layout (translator: no swap),
	// so it is likewise not permutation-invariant.
	fullAB := mkJoin(JoinFullOuter)
	fullBA := fullAB.WithSwappedQuantifiers()
	if MemoEqual(fullAB, fullBA) {
		t.Fatal("swapped FULL OUTER joins must NOT be MemoEqual (ChildrenAsSet must be false)")
	}
}
