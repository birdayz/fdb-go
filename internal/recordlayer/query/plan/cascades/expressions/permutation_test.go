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
		return mustExpression(NewLogicalUnionExpression(qs))
	}
	u1 := build([]*leafScan{leafA, leafB, leafC})
	for i, order := range [][]*leafScan{
		{leafA, leafB, leafC},
		{leafA, leafC, leafB},
		{leafB, leafA, leafC},
		{leafB, leafC, leafA},
		{leafC, leafA, leafB},
		{leafC, leafB, leafA},
	} {
		u2 := build(order)
		if !SemanticEquals(u1, u2, EmptyAliasMap()) {
			t.Fatalf("UNION permutation %d reported semantically unequal", i)
		}
		if duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{u1}, u2); !duplicate {
			t.Fatalf("prepared admission missed UNION permutation %d", i)
		}
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
		return mustExpression(NewLogicalUnionExpression(qs))
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
		return mustExpression(NewLogicalIntersectionExpression(qs, nil))
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
	a := mustExpression(NewLogicalFilterExpression(nil, ForEachQuantifier(InitialOf(leafA))))
	b := mustExpression(NewLogicalFilterExpression(nil, ForEachQuantifier(InitialOf(leafB))))
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("positional walk fell into permutation mode for single-child operator")
	}
}

func TestSemanticEquals_LargePermutedUnion(t *testing.T) {
	t.Parallel()
	n := MaxPermutationChildren + 1
	mkUnion := func(reverse bool) *LogicalUnionExpression {
		qs := make([]Quantifier, n)
		for i := 0; i < n; i++ {
			idx := i
			if reverse {
				idx = n - 1 - i
			}
			scan := mustExpression(NewFullUnorderedScanExpression(
				[]string{string(rune('A' + idx))}, testRecordType()))
			qs[i] = ForEachQuantifier(InitialOf(scan))
		}
		return mustExpression(NewLogicalUnionExpression(qs))
	}
	u1 := mkUnion(false)
	u2 := mkUnion(true)
	if !SemanticEquals(u1, u2, EmptyAliasMap()) || !MemoEqual(u1, u2) {
		t.Fatalf("unions over %d-child reverse-paired set must retain commutative memo identity", n)
	}
	if duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{u1}, u2); !duplicate {
		t.Fatalf("prepared admission missed %d-child reverse-paired union", n)
	}
	// Same-order pairing remains equal.
	u1Twin := mkUnion(false)
	if !SemanticEquals(u1, u1Twin, EmptyAliasMap()) {
		t.Fatalf("unions over %d identical-children sets reported unequal under positional fallback", n)
	}
}
