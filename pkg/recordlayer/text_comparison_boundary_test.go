package recordlayer

import (
	"strings"
	"testing"
)

func TestTextWithMaxDistanceComparisonZeroDistanceEval(t *testing.T) {
	t.Parallel()
	// At distance zero Java retains every token after polling an empty queue;
	// all distinct terms must occur, without adjacency or ordering (Comparisons.java:433-478).
	cases := []struct {
		name   string
		tokens []string
		value  any
		want   *bool
	}{
		{"adjacent", []string{"alpha", "beta"}, "alpha beta", boolPtr(true)},
		{"separated", []string{"alpha", "beta"}, "alpha one two three beta", boolPtr(true)},
		{"reversed", []string{"alpha", "beta"}, "beta one two alpha", boolPtr(true)},
		{"repeated", []string{"alpha", "beta", "gamma"}, "alpha beta alpha gamma", boolPtr(true)},
		{"missing", []string{"alpha", "beta"}, "alpha one two alpha", boolPtr(false)},
		{"empty value", []string{"alpha", "beta"}, "", boolPtr(false)},
		{"null value", []string{"alpha", "beta"}, nil, nil},
		{"single term", []string{"alpha"}, "one alpha two", boolPtr(true)},
		{"single term missing", []string{"alpha"}, "one two", boolPtr(false)},
		{"duplicate comparand", []string{"alpha", "alpha"}, "alpha", boolPtr(true)},
		{"stop words around terms", []string{"", "alpha", "", "beta", ""}, "alpha gap beta", boolPtr(true)},
		{"only stop words", []string{"", ""}, "alpha beta", nil},
		{"empty comparand", []string{}, "alpha beta", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comparisons := []*TextComparison{
				NewTextWithMaxDistanceComparison(strings.Join(tc.tokens, " "), 0, "", ""),
				NewTextWithMaxDistanceComparisonTokens(tc.tokens, 0, "", ""),
			}
			for _, comparison := range comparisons {
				got, err := comparison.Eval(tc.value)
				if err != nil {
					t.Fatal(err)
				}
				if tc.want == nil {
					if got != nil {
						t.Fatalf("%s Eval(%v) = %v, want unknown", comparison, tc.value, *got)
					}
				} else if got == nil || *got != *tc.want {
					t.Fatalf("%s Eval(%v) = %v, want %v", comparison, tc.value, got, *tc.want)
				}
			}
		})
	}
}

func TestTextWithMaxDistanceComparisonPositiveDistanceBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		distance int
		want     bool
	}{
		{0, true},
		{1, false},
		{2, true},
	} {
		comparison := NewTextWithMaxDistanceComparison("alpha beta", tc.distance, "", "")
		got, err := comparison.Eval("alpha gap beta")
		if err != nil || got == nil || *got != tc.want {
			t.Fatalf("distance %d: result=%v, err=%v, want %v", tc.distance, got, err, tc.want)
		}
	}
}
