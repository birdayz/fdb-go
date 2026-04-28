package expressions

import (
	"testing"
)

// TestSemanticEquals_UnionPermutedChildren proves that two
// LogicalUnions over the same set of children but in different orders
// compare semantically equal — the permutation enumerator finds the
// right pairing.
func TestSemanticEquals_UnionPermutedChildren(t *testing.T) {
	t.Parallel()
	leafA := &leafScan{name: "A"}
	leafB := &leafScan{name: "B"}
	leafC := &leafScan{name: "C"}
	build := func(order []*leafScan) *LogicalUnionExpression {
		qs := make([]Quantifier, len(order))
		for i, l := range order {
			qs[i] = ForEachQuantifier(InitialOf(l))
		}
		return NewLogicalUnionExpression(qs)
	}
	u1 := build([]*leafScan{leafA, leafB, leafC})
	u2 := build([]*leafScan{leafC, leafB, leafA}) // reverse order
	u3 := build([]*leafScan{leafB, leafA, leafC}) // mixed permutation
	if !SemanticEquals(u1, u2, EmptyAliasMap()) {
		t.Fatal("UNION over [A,B,C] != UNION over [C,B,A] — permutation enumerator broken")
	}
	if !SemanticEquals(u1, u3, EmptyAliasMap()) {
		t.Fatal("UNION over [A,B,C] != UNION over [B,A,C] — permutation enumerator broken")
	}
}

// TestSemanticEquals_UnionDifferentChildren — when there's no valid
// permutation, the enumerator must return false. UNION over (A,B) is
// NOT semantically equal to UNION over (A,C).
func TestSemanticEquals_UnionDifferentChildren(t *testing.T) {
	t.Parallel()
	leafA := &leafScan{name: "A"}
	leafB := &leafScan{name: "B"}
	leafC := &leafScan{name: "C"}
	build := func(order []*leafScan) *LogicalUnionExpression {
		qs := make([]Quantifier, len(order))
		for i, l := range order {
			qs[i] = ForEachQuantifier(InitialOf(l))
		}
		return NewLogicalUnionExpression(qs)
	}
	u1 := build([]*leafScan{leafA, leafB})
	u2 := build([]*leafScan{leafA, leafC})
	if SemanticEquals(u1, u2, EmptyAliasMap()) {
		t.Fatal("UNIONs with different children reported semantically equal")
	}
}

// TestSemanticEquals_IntersectionPermuted — INTERSECTION is also
// commutative; same property as UNION.
func TestSemanticEquals_IntersectionPermuted(t *testing.T) {
	t.Parallel()
	leafA := &leafScan{name: "A"}
	leafB := &leafScan{name: "B"}
	build := func(order []*leafScan) *LogicalIntersectionExpression {
		qs := make([]Quantifier, len(order))
		for i, l := range order {
			qs[i] = ForEachQuantifier(InitialOf(l))
		}
		return NewLogicalIntersectionExpression(qs, nil)
	}
	x1 := build([]*leafScan{leafA, leafB})
	x2 := build([]*leafScan{leafB, leafA})
	if !SemanticEquals(x1, x2, EmptyAliasMap()) {
		t.Fatal("INTERSECTION children commutativity broken")
	}
}

// TestSemanticEquals_PositionalDoesNotPermute — single-child
// expressions don't ChildrenAsSet, so SemanticEquals goes through the
// positional path. Property: a Filter over leafA should NOT match a
// Filter over leafB even if no permutation enumeration could rescue it.
func TestSemanticEquals_PositionalDoesNotPermute(t *testing.T) {
	t.Parallel()
	leafA := &leafScan{name: "A"}
	leafB := &leafScan{name: "B"}
	a := NewLogicalFilterExpression(nil, ForEachQuantifier(InitialOf(leafA)))
	b := NewLogicalFilterExpression(nil, ForEachQuantifier(InitialOf(leafB)))
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("positional walk fell into permutation mode for single-child operator")
	}
}

// TestPermute_AllPermutations enumerates [3]int permutations to
// confirm the helper produces all 6 in the expected sequence.
func TestPermute_AllPermutations(t *testing.T) {
	t.Parallel()
	got := [][]int{}
	indices := []int{0, 1, 2}
	permute(indices, 0, func(perm []int) bool {
		cp := make([]int, len(perm))
		copy(cp, perm)
		got = append(got, cp)
		return false // never accept; visit all
	})
	if len(got) != 6 {
		t.Fatalf("permute visited %d permutations, want 6 (3!)", len(got))
	}
}

// TestPermute_StopsOnFirstAccept — passing accept=true short-circuits
// the enumeration. Permute should not visit further permutations.
func TestPermute_StopsOnFirstAccept(t *testing.T) {
	t.Parallel()
	visited := 0
	indices := []int{0, 1, 2, 3} // 24 permutations
	permute(indices, 0, func(_ []int) bool {
		visited++
		return visited == 1 // accept on first call
	})
	if visited != 1 {
		t.Fatalf("permute visited %d permutations after accept on first, want 1", visited)
	}
}

// TestSemanticEquals_PermutationCap_FallsBackToPositional pins that
// when the child count exceeds MaxPermutationChildren, the walker
// falls back to positional pairing rather than burning O(N!) cycles
// on a query shape that effectively never appears in real workloads.
//
// Construction: build two LogicalUnion expressions over (N+1)
// distinct scans, where N=MaxPermutationChildren. Pair them in
// reverse order. Under permutation enumeration they'd be equal;
// under positional pairing they're NOT equal (because the leaves
// at each position differ).
func TestSemanticEquals_PermutationCap_FallsBackToPositional(t *testing.T) {
	t.Parallel()
	n := MaxPermutationChildren + 1
	mkUnion := func(reverse bool) *LogicalUnionExpression {
		qs := make([]Quantifier, n)
		for i := 0; i < n; i++ {
			idx := i
			if reverse {
				idx = n - 1 - i
			}
			scan := NewFullUnorderedScanExpression([]string{string(rune('A' + idx))}, nil)
			qs[i] = ForEachQuantifier(InitialOf(scan))
		}
		return NewLogicalUnionExpression(qs)
	}
	u1 := mkUnion(false)
	u2 := mkUnion(true)
	// With permutation enumeration these would be equal (UNION is
	// commutative). With positional fallback they are NOT — the
	// leaves at position 0 differ ("A" vs the last letter).
	if SemanticEquals(u1, u2, EmptyAliasMap()) {
		t.Fatalf("unions over %d-child reverse-paired set reported equal — cap fallback didn't trigger", n)
	}
	// Sanity: same-order pairing IS equal.
	u1Twin := mkUnion(false)
	if !SemanticEquals(u1, u1Twin, EmptyAliasMap()) {
		t.Fatalf("unions over %d identical-children sets reported unequal under positional fallback", n)
	}
}
