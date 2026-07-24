package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzGetCorrelatedToOfPredicate pins three invariants:
//
//  1. Nil-safety: never panics on a nil input or nil sub-tree.
//  2. Empty-on-leaves-without-correlation: a predicate whose
//     leaves are pure ConstantPredicates returns empty set.
//  3. Exactness: the returned correlation set is exactly the
//     CorrelationIdentifiers actually present in the tree.
//
// The fuzzer builds small predicate trees from a byte stream;
// alternates ConstantPredicate / ValuePredicate(QuantifiedObject(alias)) /
// And/Or/Not at random nesting levels.
func FuzzGetCorrelatedToOfPredicate(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{0xff, 0x00})
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 2 {
			return
		}
		p, expectedAliases := buildFuzzPredicate(b, 0, 0)
		got := GetCorrelatedToOfPredicate(p)
		// Exactness in both directions catches both fabricated aliases and
		// under-reporting.
		if len(got) != len(expectedAliases) {
			t.Fatalf("correlations = %v, want exactly %v", got, expectedAliases)
		}
		for k := range got {
			if _, ok := expectedAliases[k]; !ok {
				t.Fatalf("reported alias %v not in constructed set %v", k, expectedAliases)
			}
		}
		for k := range expectedAliases {
			if _, ok := got[k]; !ok {
				t.Fatalf("missing constructed alias %v from reported set %v", k, got)
			}
		}
		// If the predicate is a ConstantPredicate (no correlations),
		// got must be empty.
		if _, ok := p.(*ConstantPredicate); ok && len(got) != 0 {
			t.Fatalf("ConstantPredicate yielded correlations: %v", got)
		}
	})
}

// buildFuzzPredicate builds a small QueryPredicate tree from `b`,
// indexed at `start`, recursion bounded by `depth`. Returns the
// predicate AND the exact set of CorrelationIdentifiers it references.
func buildFuzzPredicate(b []byte, start, depth int) (QueryPredicate, map[values.CorrelationIdentifier]struct{}) {
	if depth >= 4 || len(b) == 0 {
		return NewConstantPredicate(TriTrue), map[values.CorrelationIdentifier]struct{}{}
	}
	op := b[start%len(b)] % 6
	switch op {
	case 0:
		return NewConstantPredicate(TriTrue), map[values.CorrelationIdentifier]struct{}{}
	case 1:
		// ValuePredicate referencing a fresh alias.
		alias := values.NamedCorrelationIdentifier(string(rune('A' + b[start%len(b)]%26)))
		v := values.NewQuantifiedObjectValue(alias)
		return NewValuePredicate(v), map[values.CorrelationIdentifier]struct{}{alias: {}}
	case 2:
		// AndPredicate with two children.
		c1, set1 := buildFuzzPredicate(b, (start+1)%len(b), depth+1)
		c2, set2 := buildFuzzPredicate(b, (start+2)%len(b), depth+1)
		out := mergeCorrSets(set1, set2)
		return NewAnd(c1, c2), out
	case 3:
		// OrPredicate with two children.
		c1, set1 := buildFuzzPredicate(b, (start+1)%len(b), depth+1)
		c2, set2 := buildFuzzPredicate(b, (start+2)%len(b), depth+1)
		out := mergeCorrSets(set1, set2)
		return NewOr(c1, c2), out
	case 4:
		// NotPredicate over a child.
		c, set := buildFuzzPredicate(b, (start+1)%len(b), depth+1)
		return NewNot(c), set
	default:
		// Compound predicate with a nil subtree. The historical helper skipped
		// nil children; delegated GetCorrelatedTo implementations must preserve
		// that compatibility.
		c, set := buildFuzzPredicate(b, (start+1)%len(b), depth+1)
		if b[(start+1)%len(b)]%2 == 0 {
			return NewAnd(nil, c), set
		}
		return NewOr(c, nil), set
	}
}

func mergeCorrSets(a, b map[values.CorrelationIdentifier]struct{}) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}
