package factory

import "testing"

// TestSeedCandidatesDerivesExactlyOneFamily COUNTS the derivations instead of
// reading the code, because reading the code is what got this wrong.
//
// The shape this replaces —
//
//	cands, prefix := Candidates(seed), "fc"
//	if s.Nested { cands, prefix = NestedCandidates(seed), "fcn" }
//
// — reads as an either/or and is not one: the short declaration's right-hand
// side is evaluated before the `if`, so the flat derivation ran on every nested
// seed. Two separate readings of that code reached opposite verdicts about it
// and a call counter settled it in one run. Nothing here asserts a property of
// the source text; each arm asserts a NUMBER OF CALLS.
//
// The zero is the load-bearing assertion in each arm. A test that only checked
// which candidates came back would pass on the broken shape too — the flat
// result was computed and then discarded, so the RETURN VALUE was already
// correct and only the call count was not.
func TestSeedCandidatesDerivesExactlyOneFamily(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		nested     bool
		wantFlat   int
		wantNested int
		wantPrefix string
		wantSeen   uint64
	}{
		{
			name: "a flat sweep never derives the nested family",
			// wantNested 0: the mirror of the bug, and it was never broken —
			// asserted anyway so the fix cannot be "swap which one leaks".
			nested: false, wantFlat: 1, wantNested: 0, wantPrefix: "fc", wantSeen: 7,
		},
		{
			name: "a nested sweep never derives the flat family",
			// wantFlat 0 IS the regression. It read 1 before this fix.
			nested: true, wantFlat: 0, wantNested: 1, wantPrefix: "fcn", wantSeen: 7,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var flatCalls, nestedCalls int
			var flatSeed, nestedSeed uint64
			flat := func(s uint64) []Candidate {
				flatCalls++
				flatSeed = s
				return []Candidate{{Seed: s, QueryIndex: 100}}
			}
			nest := func(s uint64) []Candidate {
				nestedCalls++
				nestedSeed = s
				return []Candidate{{Seed: s, QueryIndex: 200}}
			}

			cands, prefix := seedCandidates(tc.nested, flat, nest, 7)

			if flatCalls != tc.wantFlat {
				t.Errorf("flat derivation called %d time(s), want %d — a derivation whose result is "+
					"discarded is a whole seed's generation and feature-vector computation done for nothing, "+
					"and it is invisible to any check that only looks at the returned candidates",
					flatCalls, tc.wantFlat)
			}
			if nestedCalls != tc.wantNested {
				t.Errorf("nested derivation called %d time(s), want %d", nestedCalls, tc.wantNested)
			}
			if flatCalls+nestedCalls != 1 {
				t.Errorf("%d derivations ran in total, want exactly 1 — the two families are alternatives, "+
					"and a seed materializes ONE schema", flatCalls+nestedCalls)
			}
			if prefix != tc.wantPrefix {
				t.Errorf("schema prefix %q, want %q", prefix, tc.wantPrefix)
			}
			// The family that DID run must have been handed the caller's seed,
			// or the counter above would be satisfied by a derivation of the
			// wrong seed entirely.
			seen := flatSeed + nestedSeed
			if seen != tc.wantSeen {
				t.Errorf("the derivation that ran was handed seed %d, want %d", seen, tc.wantSeen)
			}
			// And the candidates returned must come from that same family, so
			// the prefix and the rows cannot disagree about which one ran.
			wantQuery := 100
			if tc.nested {
				wantQuery = 200
			}
			if len(cands) != 1 || cands[0].QueryIndex != wantQuery {
				t.Errorf("candidates came from the wrong family: %+v", cands)
			}
		})
	}
}
