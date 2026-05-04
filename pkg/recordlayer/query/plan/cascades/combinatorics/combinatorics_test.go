package combinatorics

import (
	"sort"
	"testing"
)

func collectAll[T comparable](iter EnumeratingIterator[T]) [][]T {
	var result [][]T
	for {
		perm := iter.Next()
		if perm == nil {
			return result
		}
		cp := make([]T, len(perm))
		copy(cp, perm)
		result = append(result, cp)
	}
}

func containsPerm(perms [][]string, target []string) bool {
	for _, p := range perms {
		if len(p) != len(target) {
			continue
		}
		match := true
		for i := range p {
			if p[i] != target[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// --- PartiallyOrderedSet tests (mirroring Java's PartiallyOrderedSetTest) ---

func TestEligibleSetsImpossibleDependencies(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	deps.Put("a", "c")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)
	es := p.EligibleSet()
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected no eligible elements for circular deps, got %v", es.EligibleElements())
	}
}

func TestEligibleSetsFullDependencies(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	es := p.EligibleSet()
	expectEligible(t, es, "a")
	es = es.RemoveEligibleElements(es.EligibleElements())
	expectEligible(t, es, "b")
	es = es.RemoveEligibleElements(es.EligibleElements())
	expectEligible(t, es, "c")
	es = es.RemoveEligibleElements(es.EligibleElements())
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected empty after consuming all, got %v", es.EligibleElements())
	}
}

func TestEligibleSetsNoDependencies(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())
	es := p.EligibleSet()
	expectEligible(t, es, "a", "b", "c")
	es = es.RemoveEligibleElements(es.EligibleElements())
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected empty, got %v", es.EligibleElements())
	}
}

func TestEligibleSetsSomeDependencies(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("c", "a")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	es := p.EligibleSet()
	expectEligible(t, es, "a", "b")
	es = es.RemoveEligibleElements(es.EligibleElements())
	expectEligible(t, es, "c")
	es = es.RemoveEligibleElements(es.EligibleElements())
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected empty, got %v", es.EligibleElements())
	}
}

func TestEligibleSetsSomeDependencies2(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "a")
	deps.Put("d", "b")
	deps.Put("d", "c")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d"}, deps)

	es := p.EligibleSet()
	expectEligible(t, es, "a")
	es = es.RemoveEligibleElements(es.EligibleElements())
	expectEligible(t, es, "b", "c")
	es = es.RemoveEligibleElements(es.EligibleElements())
	expectEligible(t, es, "d")
	es = es.RemoveEligibleElements(es.EligibleElements())
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected empty, got %v", es.EligibleElements())
	}
}

func TestEligibleSetsEmpty(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{}, NewSetMultimap[string]())
	es := p.EligibleSet()
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected empty, got %v", es.EligibleElements())
	}
}

func TestEligibleSetsSingle(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a"}, NewSetMultimap[string]())
	es := p.EligibleSet()
	expectEligible(t, es, "a")
	es = es.RemoveEligibleElements(es.EligibleElements())
	if len(es.EligibleElements()) != 0 {
		t.Fatalf("expected empty, got %v", es.EligibleElements())
	}
}

func TestFilterElements(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("c", "b")
	deps.Put("c", "a")
	deps.Put("d", "c")
	deps.Put("e", "d")
	deps.Put("f", "d")
	deps.Put("g", "e")
	deps.Put("g", "f")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d", "e", "f", "g"}, deps)

	// filter to {a, b, c}: should keep deps c←a, c←b
	sub := p.FilterElements(func(s string) bool {
		return s == "a" || s == "b" || s == "c"
	})
	if sub.Size() != 3 {
		t.Fatalf("expected 3 elements, got %d", sub.Size())
	}
	if !sub.DependencyMap().Contains("c", "a") || !sub.DependencyMap().Contains("c", "b") {
		t.Fatalf("expected deps c←a and c←b, got %v", sub.DependencyMap())
	}

	// filter to {a}: no deps
	sub = p.FilterElements(func(s string) bool { return s == "a" })
	if sub.Size() != 1 {
		t.Fatalf("expected 1 element, got %d", sub.Size())
	}
	if !sub.DependencyMap().IsEmpty() {
		t.Fatalf("expected no deps, got %v", sub.DependencyMap())
	}

	// filter to {c,d,e,f,g}: all depend on a,b which are removed → cascading removal
	sub = p.FilterElements(func(s string) bool {
		return s == "c" || s == "d" || s == "e" || s == "f" || s == "g"
	})
	if sub.Size() != 0 {
		t.Fatalf("expected empty (cascading removal), got %d elements: %v", sub.Size(), sub.Set())
	}

	// filter to {a, e, g}: e depends on d (removed), g depends on e (removed) → only a
	sub = p.FilterElements(func(s string) bool {
		return s == "a" || s == "e" || s == "g"
	})
	if sub.Size() != 1 || sub.Set()[0] != "a" {
		t.Fatalf("expected {a}, got %v", sub.Set())
	}
}

func TestTransitiveClosure(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	tc := p.TransitiveClosure()
	if !tc.Contains("b", "a") {
		t.Fatal("expected b→a in transitive closure")
	}
	if !tc.Contains("c", "b") {
		t.Fatal("expected c→b in transitive closure")
	}
	if !tc.Contains("c", "a") {
		t.Fatal("expected c→a in transitive closure (transitivity)")
	}
}

func TestTransitiveClosureCircular(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("a", "b")
	deps.Put("b", "a")
	p := NewPartiallyOrderedSet([]string{"a", "b"}, deps)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on circular dependency")
		}
	}()
	p.TransitiveClosure()
}

func TestTransitiveClosureChained(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	deps.Put("d", "c")
	deps.Put("e", "d")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d", "e"}, deps)

	tc := p.TransitiveClosure()
	if !tc.Contains("b", "a") {
		t.Fatal("b→a")
	}
	if !tc.Contains("c", "a") || !tc.Contains("c", "b") {
		t.Fatal("c should depend on a and b")
	}
	if !tc.Contains("d", "a") || !tc.Contains("d", "b") || !tc.Contains("d", "c") {
		t.Fatal("d should depend on a, b, c")
	}
	if !tc.Contains("e", "a") || !tc.Contains("e", "b") || !tc.Contains("e", "c") || !tc.Contains("e", "d") {
		t.Fatal("e should depend on a, b, c, d")
	}
}

func TestTransitiveClosureNoDeps(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())
	tc := p.TransitiveClosure()
	if tc.Size() != 0 {
		t.Fatalf("expected empty closure for no deps, got size %d", tc.Size())
	}
}

func TestTransitiveClosureBranching(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	deps.Put("d", "b")
	deps.Put("e", "a")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d", "e"}, deps)

	tc := p.TransitiveClosure()
	if !tc.Contains("c", "a") || !tc.Contains("c", "b") {
		t.Fatal("c should depend on a and b")
	}
	if !tc.Contains("d", "a") || !tc.Contains("d", "b") {
		t.Fatal("d should depend on a and b")
	}
	if !tc.Contains("e", "a") {
		t.Fatal("e should depend on a")
	}
	if tc.Contains("e", "b") {
		t.Fatal("e should NOT depend on b")
	}
}

func TestTransitiveClosureDoublyUsed(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("c11", "c88")
	deps.Put("c11", "c53")
	deps.Put("c88", "c69")
	deps.Put("c88", "c53")
	p := NewPartiallyOrderedSet([]string{"c11", "c53", "c69", "c88"}, deps)

	tc := p.TransitiveClosure()
	if !tc.Contains("c88", "c69") || !tc.Contains("c88", "c53") {
		t.Fatal("c88 should depend on c69 and c53")
	}
	if !tc.Contains("c11", "c69") || !tc.Contains("c11", "c53") || !tc.Contains("c11", "c88") {
		t.Fatal("c11 should depend on c69, c53, c88")
	}
}

func TestDualOrder(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	p := NewPartiallyOrderedSet([]string{"a", "b"}, deps)

	dual := p.DualOrder()
	if !dual.DependencyMap().Contains("a", "b") {
		t.Fatal("expected inverted dep a←b in dual")
	}
	if dual.DependencyMap().Contains("b", "a") {
		t.Fatal("did not expect b←a in dual")
	}
}

func TestMapAll(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	deps.Put("d", "c")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d"}, deps)

	// map a→1, c→3, d→4 (b unmapped → cascade removes c,d since they depend on b)
	// but d also has direct chain a←b←c←d, removing b breaks c, removing c breaks d
	// only a survives (mapped to 1), d has dep on c which depends on b (removed)
	m := map[string]int{"a": 1, "c": 3, "d": 4}
	result := MapAll(p, m)
	if result.Size() != 1 {
		t.Fatalf("expected 1 element after cascade, got %d: %v", result.Size(), result.Set())
	}
}

func TestMapAllDiamond(t *testing.T) {
	t.Parallel()
	// Java Example 2: a←b, a←c, b←d, c←d (diamond)
	// map: a→a', b→b', d→d' (c unmapped)
	// Result: a'←b', b'←d' (c chain dropped, but a←b←d chain preserved)
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "a")
	deps.Put("d", "b")
	deps.Put("d", "c")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d"}, deps)

	m := map[string]string{"a": "a'", "b": "b'", "d": "d'"}
	result := MapAll(p, m)
	if result.Size() != 3 {
		t.Fatalf("expected 3 elements, got %d: %v", result.Size(), result.Set())
	}
	if !result.DependencyMap().Contains("b'", "a'") {
		t.Fatal("expected b'←a'")
	}
	if !result.DependencyMap().Contains("d'", "b'") {
		t.Fatal("expected d'←b'")
	}
}

func TestBuilder(t *testing.T) {
	t.Parallel()
	b := NewBuilder[string]()
	b.Add("a").Add("b").AddDependency("c", "a").AddDependency("c", "b")
	p := b.Build()

	if p.Size() != 3 {
		t.Fatalf("expected 3 elements, got %d", p.Size())
	}
	if !p.DependencyMap().Contains("c", "a") || !p.DependencyMap().Contains("c", "b") {
		t.Fatalf("expected deps c←a and c←b")
	}
}

func TestBuilderListWithDependencies(t *testing.T) {
	t.Parallel()
	b := NewBuilder[string]()
	b.AddListWithDependencies([]string{"a", "b", "c"})
	p := b.Build()

	if p.Size() != 3 {
		t.Fatalf("expected 3 elements, got %d", p.Size())
	}
	if !p.DependencyMap().Contains("b", "a") {
		t.Fatal("expected b←a")
	}
	if !p.DependencyMap().Contains("c", "b") {
		t.Fatal("expected c←b")
	}
}

// --- TopologicalSort tests (mirroring Java's TopologicalSortTest) ---

func TestTopologicalSortImpossibleDependencies(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	deps.Put("a", "c")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	iter := TopologicalOrderPermutations(p)
	result := iter.Next()
	if result != nil {
		t.Fatalf("expected no permutations for circular deps, got %v", result)
	}
}

func TestTopologicalSortFullDependencies(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	perms := collectAll(TopologicalOrderPermutations(p))
	if len(perms) != 1 {
		t.Fatalf("expected 1 permutation, got %d", len(perms))
	}
	expected := []string{"a", "b", "c"}
	for i, v := range perms[0] {
		if v != expected[i] {
			t.Fatalf("expected %v, got %v", expected, perms[0])
		}
	}
}

func TestTopologicalSortNoDependencies(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())

	perms := collectAll(TopologicalOrderPermutations(p))
	if len(perms) != 6 {
		t.Fatalf("expected 6 permutations of 3 elements, got %d", len(perms))
	}

	for _, expected := range [][]string{
		{"a", "b", "c"},
		{"a", "c", "b"},
		{"b", "a", "c"},
		{"b", "c", "a"},
		{"c", "a", "b"},
		{"c", "b", "a"},
	} {
		if !containsPerm(perms, expected) {
			t.Fatalf("missing permutation %v", expected)
		}
	}
}

func TestTopologicalSortSomeDependencies(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("c", "a")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	perms := collectAll(TopologicalOrderPermutations(p))
	if len(perms) != 3 {
		t.Fatalf("expected 3 permutations, got %d: %v", len(perms), perms)
	}

	for _, expected := range [][]string{
		{"a", "b", "c"}, {"a", "c", "b"}, {"b", "a", "c"},
	} {
		if !containsPerm(perms, expected) {
			t.Fatalf("missing permutation %v in %v", expected, perms)
		}
	}
}

func TestTopologicalSortEmpty(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{}, NewSetMultimap[string]())
	iter := TopologicalOrderPermutations(p)
	if iter.Next() != nil {
		t.Fatal("expected no permutations for empty set")
	}
}

func TestTopologicalSortSingle(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a"}, NewSetMultimap[string]())
	perms := collectAll(TopologicalOrderPermutations(p))
	if len(perms) != 1 {
		t.Fatalf("expected 1 permutation, got %d", len(perms))
	}
}

func TestTopologicalSortSkip(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())

	iter := TopologicalOrderPermutations(p)
	var perms [][]string
	for {
		next := iter.Next()
		if next == nil {
			break
		}
		cp := make([]string, len(next))
		copy(cp, next)
		perms = append(perms, cp)
		if next[0] == "b" && next[1] == "a" {
			iter.Skip(0)
		}
	}

	if len(perms) != 5 {
		t.Fatalf("expected 5 permutations after skip, got %d: %v", len(perms), perms)
	}

	for _, expected := range [][]string{
		{"a", "b", "c"},
		{"a", "c", "b"},
		{"b", "a", "c"},
		{"c", "a", "b"},
		{"c", "b", "a"},
	} {
		if !containsPerm(perms, expected) {
			t.Fatalf("missing permutation %v", expected)
		}
	}
}

func TestTopologicalSortSkip2(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d"}, NewSetMultimap[string]())

	iter := TopologicalOrderPermutations(p)
	var perms [][]string
	for {
		next := iter.Next()
		if next == nil {
			break
		}
		cp := make([]string, len(next))
		copy(cp, next)
		perms = append(perms, cp)
		if next[0] == "b" && next[1] == "a" {
			iter.Skip(1)
		}
	}

	// skip(1) after (b,a,...) skips remaining (b,a,...) perms but keeps (b,c,...), (b,d,...)
	// total: 24 - 1 = 23 (only b,a,d,c is skipped)
	if len(perms) != 23 {
		t.Fatalf("expected 23 permutations, got %d", len(perms))
	}

	if !containsPerm(perms, []string{"b", "a", "c", "d"}) {
		t.Fatal("should contain [b,a,c,d]")
	}
	if containsPerm(perms, []string{"b", "a", "d", "c"}) {
		t.Fatal("should NOT contain [b,a,d,c]")
	}
}

func TestTopologicalSortSkip3(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d"}, NewSetMultimap[string]())

	iter := TopologicalOrderPermutations(p)
	var perms [][]string
	for {
		next := iter.Next()
		if next == nil {
			break
		}
		cp := make([]string, len(next))
		copy(cp, next)
		perms = append(perms, cp)
		if next[0] == "b" && next[1] == "a" {
			iter.Skip(0)
		}
	}

	// skip(0) after (b,a,...) skips ALL remaining (b,...) perms
	// 6 perms per first element × 4 first elements = 24 total
	// kept: 6(a) + 1(b,a,c,d) + 6(c) + 6(d) = 19
	if len(perms) != 19 {
		t.Fatalf("expected 19 permutations, got %d", len(perms))
	}

	if !containsPerm(perms, []string{"b", "a", "c", "d"}) {
		t.Fatal("should contain [b,a,c,d]")
	}
	if containsPerm(perms, []string{"b", "a", "d", "c"}) {
		t.Fatal("should NOT contain [b,a,d,c]")
	}
	if containsPerm(perms, []string{"b", "c", "a", "d"}) {
		t.Fatal("should NOT contain [b,c,a,d]")
	}
}

func TestAnyTopologicalOrderPermutation(t *testing.T) {
	t.Parallel()
	deps := NewSetMultimap[string]()
	deps.Put("b", "a")
	deps.Put("c", "b")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, deps)

	result := AnyTopologicalOrderPermutation(p)
	if result == nil {
		t.Fatal("expected a result")
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(result))
	}
	// With full chain a←b←c, only valid ordering is [a,b,c]
	expected := []string{"a", "b", "c"}
	for i, v := range result {
		if v != expected[i] {
			t.Fatalf("expected %v, got %v", expected, result)
		}
	}
}

func TestAnyTopologicalOrderNoDeps(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())
	result := AnyTopologicalOrderPermutation(p)
	if result == nil {
		t.Fatal("expected a result")
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(result))
	}
}

func TestPermutations(t *testing.T) {
	t.Parallel()
	perms := collectAll(Permutations([]string{"a", "b", "c"}))
	if len(perms) != 6 {
		t.Fatalf("expected 6 permutations, got %d", len(perms))
	}
}

func TestSatisfyingPermutations(t *testing.T) {
	t.Parallel()
	p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())
	target := []string{"a", "b", "c"}
	iter := SatisfyingPermutations(p, target, func(perm []string) int {
		for i := range target {
			if i >= len(perm) || perm[i] != target[i] {
				return i
			}
		}
		return len(target)
	})
	perms := collectAll(iter)
	if len(perms) != 1 {
		t.Fatalf("expected 1 satisfying permutation, got %d: %v", len(perms), perms)
	}
	if !containsPerm(perms, target) {
		t.Fatalf("expected %v, got %v", target, perms)
	}
}

func TestSetMultimap(t *testing.T) {
	t.Parallel()
	m := NewSetMultimap[string]()
	m.Put("a", "b")
	m.Put("a", "c")
	m.Put("b", "c")

	if m.Size() != 3 {
		t.Fatalf("expected size 3, got %d", m.Size())
	}
	if !m.Contains("a", "b") || !m.Contains("a", "c") || !m.Contains("b", "c") {
		t.Fatal("missing expected entries")
	}
	if m.Contains("c", "a") {
		t.Fatal("should not contain c→a")
	}

	inv := m.Inverse()
	if !inv.Contains("b", "a") || !inv.Contains("c", "a") || !inv.Contains("c", "b") {
		t.Fatal("inverse missing entries")
	}

	entries := m.Entries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	cl := m.Clone()
	if cl.Size() != 3 {
		t.Fatalf("clone size mismatch")
	}
}

func TestPartiallyOrderedSetEqual(t *testing.T) {
	t.Parallel()
	deps1 := NewSetMultimap[string]()
	deps1.Put("b", "a")
	p1 := NewPartiallyOrderedSet([]string{"a", "b"}, deps1)

	deps2 := NewSetMultimap[string]()
	deps2.Put("b", "a")
	p2 := NewPartiallyOrderedSet([]string{"a", "b"}, deps2)

	if !p1.Equal(p2) {
		t.Fatal("expected equal")
	}

	deps3 := NewSetMultimap[string]()
	deps3.Put("a", "b")
	p3 := NewPartiallyOrderedSet([]string{"a", "b"}, deps3)
	if p1.Equal(p3) {
		t.Fatal("should not be equal with reversed deps")
	}
}

func TestTopologicalSortFourElementsWithDeps(t *testing.T) {
	t.Parallel()
	// b←c, b←d (c and d depend on b)
	deps := NewSetMultimap[string]()
	deps.Put("c", "b")
	deps.Put("d", "b")
	p := NewPartiallyOrderedSet([]string{"a", "b", "c", "d"}, deps)

	perms := collectAll(TopologicalOrderPermutations(p))
	// b must come before c and d, a can be anywhere → 8 valid orderings
	if len(perms) != 8 {
		t.Fatalf("expected 8 permutations, got %d: %v", len(perms), perms)
	}

	// verify all perms have b before c and b before d
	for _, perm := range perms {
		bIdx, cIdx, dIdx := -1, -1, -1
		for i, v := range perm {
			switch v {
			case "b":
				bIdx = i
			case "c":
				cIdx = i
			case "d":
				dIdx = i
			}
		}
		if bIdx >= cIdx || bIdx >= dIdx {
			t.Fatalf("b must precede c and d: %v", perm)
		}
	}
}

func TestTopologicalSortStability(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())
		perms := collectAll(TopologicalOrderPermutations(p))
		if len(perms) != 6 {
			t.Fatalf("iteration %d: expected 6 perms, got %d", i, len(perms))
		}
		// verify deterministic ordering
		expected := [][]string{
			{"a", "b", "c"},
			{"a", "c", "b"},
			{"b", "a", "c"},
			{"b", "c", "a"},
			{"c", "a", "b"},
			{"c", "b", "a"},
		}
		for j, exp := range expected {
			if !sliceEqual(perms[j], exp) {
				t.Fatalf("iteration %d: perm %d expected %v, got %v", i, j, exp, perms[j])
			}
		}
	}
}

func TestAnyTopologicalSortStability(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		p := NewPartiallyOrderedSet([]string{"a", "b", "c"}, NewSetMultimap[string]())
		result := AnyTopologicalOrderPermutation(p)
		expected := []string{"a", "b", "c"}
		if !sliceEqual(result, expected) {
			t.Fatalf("iteration %d: expected %v, got %v", i, expected, result)
		}
	}
}

func expectEligible(t *testing.T, es *EligibleSet[string], expected ...string) {
	t.Helper()
	got := es.EligibleElements()
	if len(got) != len(expected) {
		t.Fatalf("expected eligible %v, got %v", expected, setToSortedSlice(got))
	}
	for _, e := range expected {
		if _, ok := got[e]; !ok {
			t.Fatalf("expected %q in eligible set %v", e, setToSortedSlice(got))
		}
	}
}

func setToSortedSlice(s map[string]struct{}) []string {
	result := make([]string, 0, len(s))
	for k := range s {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
