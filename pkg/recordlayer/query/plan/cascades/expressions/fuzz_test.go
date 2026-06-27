package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzSemanticEquals_Properties pins three invariants of
// SemanticEquals across randomly-shaped expression trees:
//
//  1. Reflexivity: SemanticEquals(a, a, ∅) is always true.
//  2. Symmetry: SemanticEquals(a, b, ∅) == SemanticEquals(b, a, ∅).
//  3. Hash consistency: if SemanticEquals returns true under empty
//     aliases, both expressions' HashCodeWithoutChildren MUST match.
//     (HashCodeWithoutChildren is per-node — only constrained when
//     EqualsWithoutChildren agrees, which it must when SemanticEquals
//     does.)
//
// The fuzzer generates two trees from the same byte stream; each tree
// is constructed from a few base building blocks (Scan, Filter,
// Projection, Distinct, Union over Scans). Some flag bits make the
// two trees agree, others make them diverge — we expect either both
// or neither tree to satisfy each property.
func FuzzSemanticEquals_Properties(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{0xff, 0x00, 0xaa, 0x55})
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		a := buildFuzzTree(b, 0)
		c := buildFuzzTree(b, len(b)/2)

		// 1. Reflexivity.
		if !SemanticEquals(a, a, EmptyAliasMap()) {
			t.Fatalf("reflexivity broken: a != a, a=%T", a)
		}
		if !SemanticEquals(c, c, EmptyAliasMap()) {
			t.Fatalf("reflexivity broken: c != c, c=%T", c)
		}

		// 2. Symmetry.
		ab := SemanticEquals(a, c, EmptyAliasMap())
		ba := SemanticEquals(c, a, EmptyAliasMap())
		if ab != ba {
			t.Fatalf("symmetry broken: a==c=%v but c==a=%v, a=%T c=%T", ab, ba, a, c)
		}

		// 3. Hash consistency: when a and c are SemanticEquals true,
		// their HashCodeWithoutChildren MUST match. (Note: a and c
		// might be deeply different but at the root EqualsWithoutChildren
		// agrees — that's what HashCodeWithoutChildren is constrained
		// against.) Pin only when SemanticEquals reports true.
		if ab && a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
			t.Fatalf("hash inconsistency: SemanticEquals(a,c) but HashCodeWithoutChildren differ (a=%d c=%d, a=%T c=%T)",
				a.HashCodeWithoutChildren(), c.HashCodeWithoutChildren(), a, c)
		}
	})
}

// buildFuzzTree constructs a small RelationalExpression tree from a
// byte stream. Tree shape selected by the byte at index `start`,
// recursion bounded — never panics, never returns nil.
func buildFuzzTree(b []byte, start int) RelationalExpression {
	if len(b) == 0 {
		return NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	}
	op := b[start%len(b)] % 5
	switch op {
	case 0:
		return NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	case 1:
		return NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	case 2:
		// Filter over Scan
		scan := NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
		q := ForEachQuantifier(InitialOf(scan))
		pred := predicates.NewConstantPredicate(predicates.TriTrue)
		return NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	case 3:
		// Distinct over Scan
		scan := NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
		q := ForEachQuantifier(InitialOf(scan))
		return NewLogicalDistinctExpression(q)
	default:
		// Union over two Scans (commutative — exercises permutation path)
		a := NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
		b := NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
		return NewLogicalUnionExpression([]Quantifier{
			ForEachQuantifier(InitialOf(a)),
			ForEachQuantifier(InitialOf(b)),
		})
	}
}

// FuzzWalk_Termination pins that Walk(e, visit) terminates on any
// random expression tree built from a byte stream — no infinite loop
// even on shapes that share References across multiple Quantifiers.
//
// The key invariant: visit is called at most O(N) times where N is
// the static node count of the constructed tree. Since the seed
// constructs trees with bounded depth (<=3) and bounded fan-out, the
// expected node count is small.
func FuzzWalk_Termination(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 2 {
			return
		}
		expr := buildFuzzTree(b, 0)
		visited := 0
		const cap = 1024
		Walk(expr, func(_ RelationalExpression) bool {
			visited++
			if visited > cap {
				t.Fatalf("Walk exceeded %d visits — possible infinite loop on tree shape", cap)
			}
			return true
		})
		// Sanity: Size returns the same count.
		if got := Size(expr); got != visited {
			t.Fatalf("Walk visited %d but Size=%d — disagreement on node count", visited, got)
		}
	})
}

// FuzzAliasMap_BijectionInvariant pins that AliasMap maintains its
// bijection invariant under arbitrary Compose chains: for every
// (s,t) binding, GetTarget(s) == t AND GetSource(t) == s.
func FuzzAliasMap_BijectionInvariant(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		// Build a sequence of Compose calls.
		m := EmptyAliasMap()
		for i := 0; i+1 < len(b) && i < 16; i += 2 {
			s := values.NamedCorrelationIdentifier(string(rune('A' + b[i]%26)))
			tgt := values.NamedCorrelationIdentifier(string(rune('a' + b[i+1]%26)))

			// Skip if this binding would conflict with existing ones.
			if m.ContainsSource(s) {
				continue
			}
			if m.ContainsTarget(tgt) {
				continue
			}
			candidate := AliasMapOf(s, tgt)
			func() {
				defer func() { _ = recover() }()
				m = m.Compose(candidate)
			}()
		}
		// Bijection invariant: every (s, t) round-trips.
		for s := range m.forward {
			tgt, ok := m.GetTarget(s)
			if !ok {
				t.Fatalf("forward map has %v but GetTarget says missing", s)
			}
			revS, ok := m.GetSource(tgt)
			if !ok || revS != s {
				t.Fatalf("bijection broken: %v→%v but GetSource(%v)=%v,%v", s, tgt, tgt, revS, ok)
			}
		}
		for tgt := range m.reverse {
			s, ok := m.GetSource(tgt)
			if !ok {
				t.Fatalf("reverse map has %v but GetSource says missing", tgt)
			}
			revT, ok := m.GetTarget(s)
			if !ok || revT != tgt {
				t.Fatalf("bijection broken: rev %v→%v but GetTarget(%v)=%v,%v", tgt, s, s, revT, ok)
			}
		}
	})
}
