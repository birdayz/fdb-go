package values

import (
	"fmt"
	"testing"
)

// TestExtendAliasMapChain pins the one-pair extension a backtracking match
// builds: every pair stays reachable in both directions however long the
// chain, a conflicting pair is refused against any link, a repeated pair
// returns the map it extends, and the chain flattens past maxAliasChain.
func TestExtendAliasMapChain(t *testing.T) {
	t.Parallel()
	pair := func(i int) AliasPair {
		return AliasPair{
			Source: NamedCorrelationIdentifier(fmt.Sprintf("s%d", i)),
			Target: NamedCorrelationIdentifier(fmt.Sprintf("t%d", i)),
		}
	}
	m := EmptyAliasMap()
	const n = 3*maxAliasChain + 2
	for i := 0; i < n; i++ {
		next, ok, err := ExtendAliasMap(m, []AliasPair{pair(i)})
		if err != nil || !ok {
			t.Fatalf("extend %d: ok=%t err=%v", i, ok, err)
		}
		if chained, isChain := next.(*extendedAliasMap); isChain && chained.depth > maxAliasChain {
			t.Fatalf("extend %d: chain depth %d past %d", i, chained.depth, maxAliasChain)
		}
		m = next
		if got := AliasMapSize(m); got != i+1 {
			t.Fatalf("extend %d: size %d", i, got)
		}
		for j := 0; j <= i; j++ {
			p := pair(j)
			if target, ok := m.Target(p.Source); !ok || target != p.Target {
				t.Fatalf("after %d: Target(%s)=%v,%t", i, p.Source.Name(), target, ok)
			}
			if source, ok := m.Source(p.Target); !ok || source != p.Source {
				t.Fatalf("after %d: Source(%s)=%v,%t", i, p.Target.Name(), source, ok)
			}
		}
		for _, conflict := range []AliasPair{
			{Source: pair(0).Source, Target: NamedCorrelationIdentifier("other")},
			{Source: NamedCorrelationIdentifier("other"), Target: pair(i).Target},
		} {
			if back, ok, err := ExtendAliasMap(m, []AliasPair{conflict}); err != nil || ok || back != m {
				t.Fatalf("after %d: conflict %v accepted (ok=%t err=%v)", i, conflict, ok, err)
			}
		}
		if again, ok, err := ExtendAliasMap(m, []AliasPair{pair(0)}); err != nil || !ok || again != m {
			t.Fatalf("after %d: repeating a pair did not return the map (ok=%t err=%v)", i, ok, err)
		}
	}
	seen := map[AliasPair]bool{}
	RangeAliasPairs(m, func(p AliasPair) bool {
		if seen[p] {
			t.Fatalf("pair %v ranged twice", p)
		}
		seen[p] = true
		return true
	})
	if len(seen) != n {
		t.Fatalf("ranged %d pairs, want %d", len(seen), n)
	}
}
