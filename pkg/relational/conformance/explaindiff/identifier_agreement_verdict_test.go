package explaindiff_test

import (
	"strings"
	"testing"
)

// TestIdentifierAgreementVerdict drives every arm of the gate's decision.
//
// The corpus run reaches exactly one of them — the pass arm — so without this
// the floor and the ceiling ship untested, and an alarm that has never fired is
// indistinguishable from one that cannot. Both guard a POPULATION rather than a
// result, and both fail OPEN if they are wrong: a floor that never trips lets an
// empty run read as clean, and a ceiling that never trips lets coverage drain
// away one unplannable statement at a time.
func TestIdentifierAgreementVerdict(t *testing.T) {
	t.Parallel()

	const ok = ""
	for _, tc := range []struct {
		name       string
		perturbed  int
		baseFailed int
		disagree   []string
		wantSubstr string
	}{
		{
			name:       "a clean run is the only pass",
			perturbed:  identifierAgreementFloor,
			baseFailed: identifierAgreementBaseFailCeiling,
			wantSubstr: ok,
		},
		{
			name:       "an empty population never reports clean",
			perturbed:  0,
			wantSubstr: "the gate is not measuring the corpus any more",
		},
		{
			name:       "one below the floor still trips it",
			perturbed:  identifierAgreementFloor - 1,
			wantSubstr: "floor is",
		},
		{
			name:       "baseline failures growing past the ceiling is a coverage loss",
			perturbed:  identifierAgreementFloor,
			baseFailed: identifierAgreementBaseFailCeiling + 1,
			wantSubstr: "did not plan even UNPERTURBED",
		},
		{
			name:       "a single disagreement fails, and is named",
			perturbed:  identifierAgreementFloor,
			disagree:   []string{"some_file.yaml#3 SELECT x"},
			wantSubstr: "some_file.yaml#3",
		},
		{
			name:       "the floor outranks a disagreement — an empty run cannot indict",
			perturbed:  1,
			disagree:   []string{"some_file.yaml#3"},
			wantSubstr: "the gate is not measuring the corpus any more",
		},
	} {
		got := identifierAgreementVerdict(tc.perturbed, tc.baseFailed, tc.disagree)
		switch {
		case tc.wantSubstr == ok && got != ok:
			t.Errorf("%s: want pass, got %q", tc.name, got)
		case tc.wantSubstr != ok && !strings.Contains(got, tc.wantSubstr):
			t.Errorf("%s: verdict %q does not contain %q", tc.name, got, tc.wantSubstr)
		}
	}
}
