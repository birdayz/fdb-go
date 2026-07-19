package values

import (
	"testing"
)

// FuzzGetCorrelatedToOfValue pins three invariants:
//
//  1. Nil-safety: never panics on nil input.
//  2. Empty-on-leaves-without-correlation: pure ConstantValue trees
//     yield empty correlation set.
//  3. Soundness: the returned set is a subset of the
//     CorrelationIdentifiers actually present in the tree (no
//     fabrication).
//
// The fuzzer constructs small Value trees from a byte stream. Mirrors
// the predicate-side FuzzGetCorrelatedToOfPredicate test.
func FuzzGetCorrelatedToOfValue(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 2 {
			return
		}
		v, expected := buildFuzzValue(b, 0, 0)
		got := GetCorrelatedToOfValue(v)
		// EXACT equality, not subset. `expected` is the construction-time
		// ground truth (buildFuzzValue records each QuantifiedObjectValue's
		// alias WITHOUT ever calling GetCorrelatedToOfValue), so demanding
		// got == expected catches BOTH failure directions:
		//   - fabrication: got has an alias the tree never contained;
		//   - under-reporting: the walker forgot a node — e.g. it stopped
		//     recursing into ArithmeticValue.Right, or dropped the QOV case.
		// The former subset-only check (got ⊆ expected) silently passed every
		// under-reporting regression, making this a fake safety net.
		if len(got) != len(expected) {
			t.Fatalf("GetCorrelatedToOfValue = %v, want exactly %v", got, expected)
		}
		for k := range expected {
			if _, ok := got[k]; !ok {
				t.Fatalf("GetCorrelatedToOfValue = %v missing alias %v (want exactly %v)", got, k, expected)
			}
		}
		// Constants: pure literal tree → empty set (redundant with the
		// equality above, kept as an explicit boundary assertion).
		if _, ok := v.(*ConstantValue); ok && len(got) != 0 {
			t.Fatalf("ConstantValue yielded correlations: %v", got)
		}
	})
}

// buildFuzzValue builds a small Value tree from `b`. Returns the
// Value AND the set of aliases it actually references — soundness
// check.
func buildFuzzValue(b []byte, start, depth int) (Value, map[CorrelationIdentifier]struct{}) {
	if depth >= 4 || len(b) == 0 {
		return &ConstantValue{Value: int64(0), Typ: NullableLong}, map[CorrelationIdentifier]struct{}{}
	}
	op := b[start%len(b)] % 4
	switch op {
	case 0:
		return &ConstantValue{Value: int64(b[start%len(b)]), Typ: NullableLong}, map[CorrelationIdentifier]struct{}{}
	case 1:
		alias := NamedCorrelationIdentifier(string(rune('A' + b[start%len(b)]%26)))
		return NewQuantifiedObjectValue(alias), map[CorrelationIdentifier]struct{}{alias: {}}
	case 2:
		// Arithmetic over two random sub-values.
		l, ls := buildFuzzValue(b, (start+1)%len(b), depth+1)
		r, rs := buildFuzzValue(b, (start+2)%len(b), depth+1)
		out := mergeSets(ls, rs)
		return &ArithmeticValue{Op: OpAdd, Left: l, Right: r}, out
	default:
		// NotValue over a sub-value.
		c, cs := buildFuzzValue(b, (start+1)%len(b), depth+1)
		return NewNotValue(c), cs
	}
}

func mergeSets(a, b map[CorrelationIdentifier]struct{}) map[CorrelationIdentifier]struct{} {
	out := map[CorrelationIdentifier]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}
