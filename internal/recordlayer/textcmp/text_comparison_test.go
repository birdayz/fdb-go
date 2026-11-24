package textcmp

import (
	"errors"
	"testing"
)

func TestTextComparisonEval(t *testing.T) {
	t.Parallel()
	const doc = "The quick brown fox jumps over the lazy dog"
	cases := []struct {
		c    *TextComparison
		want *bool
	}{
		{NewTextComparison(TextContainsAll, "fox dog", "", ""), boolPtr(true)},
		{NewTextComparison(TextContainsAll, "fox cat", "", ""), boolPtr(false)},
		{NewTextComparison(TextContainsAny, "cat dog", "", ""), boolPtr(true)},
		{NewTextComparison(TextContainsPhrase, "quick brown fox", "", ""), boolPtr(true)},
		{NewTextComparison(TextContainsPhrase, "quick fox", "", ""), boolPtr(false)},
		{NewTextComparisonTokens(TextContainsPhrase, []string{"quick", "", "fox"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsPhrase, []string{"", ""}, "", ""), nil},
		{NewTextComparisonTokens(TextContainsPrefix, []string{"jum"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAnyPrefix, []string{"ca", "do"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAllPrefixes, []string{"qu", "la"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAllPrefixes, []string{"qu", "ca"}, "", ""), boolPtr(false)},
		{NewTextWithMaxDistanceComparison("quick fox", 1, "", ""), boolPtr(false)},
		{NewTextWithMaxDistanceComparison("quick fox", 2, "", ""), boolPtr(true)},
		{NewTextWithMaxDistanceComparison("quick dog", 0, "", ""), boolPtr(true)},
		{NewTextWithMaxDistanceComparison("quick cat", 0, "", ""), boolPtr(false)},
		{NewTextComparisonTokens(TextContainsAll, []string{""}, "", ""), nil},
	}
	for _, tc := range cases {
		got, err := tc.c.Eval(doc)
		if err != nil {
			t.Fatalf("%s: %v", tc.c, err)
		}
		if tc.want == nil {
			if got != nil {
				t.Fatalf("%s: got %v, want nil", tc.c, *got)
			}
		} else if got == nil || *got != *tc.want {
			t.Fatalf("%s: got %v, want %v", tc.c, got, *tc.want)
		}
	}
	if got, err := NewTextComparison(TextContainsAll, "fox", "", "").Eval(nil); err != nil || got != nil {
		t.Fatalf("nil Eval = %v, %v, want nil, nil", got, err)
	}
}

func TestTextComparisonErrorTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c     *TextComparison
		value any
	}{
		{NewTextComparison(TextContainsAll, "fox", "", ""), int64(1)},
		{NewTextComparisonTokens(TextContainsPrefix, []string{"a", "b"}, "", ""), "a b"},
		{NewTextComparison(TextContainsPrefix, "", "", ""), "a"},
	} {
		_, err := tc.c.Eval(tc.value)
		var argErr *ArgumentError
		if !errors.As(err, &argErr) {
			t.Fatalf("%s on %v: got %T (%v), want *ArgumentError", tc.c, tc.value, err, err)
		}
	}
	_, err := NewTextComparison(TextContainsAll, "fox", "missing-tokenizer", "").Eval("fox")
	var mdErr *MetaDataError
	if !errors.As(err, &mdErr) {
		t.Fatalf("missing tokenizer: got %T (%v), want *MetaDataError", err, err)
	}
	_, err = NewTextComparison(TextComparisonType(999), "fox", "", "").Eval("fox")
	var invErr *InvalidComparisonError
	if !errors.As(err, &invErr) {
		t.Fatalf("invalid type: got %T (%v), want *InvalidComparisonError", err, err)
	}
}
