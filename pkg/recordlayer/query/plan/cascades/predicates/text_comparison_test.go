package predicates

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

func TestTextComparisonEvaluation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		typ      ComparisonType
		document any
		query    any
		distance int
		want     TriBool
	}{
		{"all normalization", ComparisonTextContainsAll, "CAFÉ, Straße!", "cafe straße", 0, TriTrue},
		{"all repeated terms", ComparisonTextContainsAll, "one two", []string{"one", "one"}, 0, TriTrue},
		{"raw tokens not normalized", ComparisonTextContainsAll, "HELLO", []string{"HELLO"}, 0, TriFalse},
		{"any", ComparisonTextContainsAny, "one two", "absent two", 0, TriTrue},
		{"any missing", ComparisonTextContainsAny, "one two", "absent", 0, TriFalse},
		{"phrase repeated", ComparisonTextContainsPhrase, "one two one", "one one", 0, TriFalse},
		{"phrase repeated adjacent", ComparisonTextContainsPhrase, "one one two", "one one", 0, TriTrue},
		{"phrase stopword slots", ComparisonTextContainsPhrase, "one other two", []string{"one", "", "two"}, 0, TriTrue},
		{"within inclusive", ComparisonTextContainsAllWithin, "one other two", "two one", 2, TriTrue},
		{"within too far", ComparisonTextContainsAllWithin, "one other two", "two one", 1, TriFalse},
		{"prefix", ComparisonTextContainsPrefix, "prefix prelude", []string{"pre"}, 0, TriTrue},
		{"prefix raw", ComparisonTextContainsPrefix, "prefix", []string{"PRE"}, 0, TriFalse},
		{"all prefixes", ComparisonTextContainsAllPrefixes, "alphabet beta", "alph bet", 0, TriTrue},
		{"overlapping prefixes", ComparisonTextContainsAllPrefixes, "alphabet", "a alph", 0, TriTrue},
		{"any prefix", ComparisonTextContainsAnyPrefix, "alphabet beta", "missing bet", 0, TriTrue},
		{"null field", ComparisonTextContainsAll, nil, "hello", 0, TriUnknown},
		{"empty terms", ComparisonTextContainsAny, "hello", "", 0, TriUnknown},
		{"stopword prefix token", ComparisonTextContainsPrefix, "hello", []string{""}, 0, TriUnknown},
		{"null query", ComparisonTextContainsPrefix, "hello", nil, 0, TriUnknown},
		{"stopword terms", ComparisonTextContainsAll, "hello", []string{"", ""}, 0, TriUnknown},
		{"empty document", ComparisonTextContainsAll, "", "hello", 0, TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Comparison{Type: tc.typ, TextMaxDistance: tc.distance}
			got, err := c.EvalAgainst(tc.document, tc.query)
			if err != nil || got != tc.want {
				t.Fatalf("got %v,%v want %v", got, err, tc.want)
			}
		})
	}
}

func TestTextComparisonRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ         ComparisonType
		left, right any
	}{{ComparisonTextContainsAll, "one", 123}, {ComparisonTextContainsAll, "one", []any{int64(1)}}} {
		_, err := (Comparison{Type: tc.typ}).EvalAgainst(tc.left, tc.right)
		var textErr *TextComparisonError
		if !errors.As(err, &textErr) {
			t.Fatalf("got %v for %#v", err, tc)
		}
	}
	// TextComparison.eval refuses a non-string value and a prefix that is not
	// one token (Comparisons.java:2588-2590, 2608-2611).
	for _, tc := range []struct {
		typ         ComparisonType
		left, right any
	}{{ComparisonTextContainsAll, 123, "one"}, {ComparisonTextContainsAll, 123, nil}, {ComparisonTextContainsPrefix, "one", []string{"one", "two"}}, {ComparisonTextContainsPrefix, "hello", ""}, {ComparisonTextContainsPrefix, "hello", []string{"", ""}}} {
		_, err := (Comparison{Type: tc.typ}).EvalAgainst(tc.left, tc.right)
		var argErr *recordlayer.RecordCoreArgumentError
		if !errors.As(err, &argErr) {
			t.Fatalf("got %v for %#v", err, tc)
		}
	}
	_, err := (Comparison{Type: ComparisonTextContainsAll, TextTokenizerName: "missing-tokenizer"}).EvalAgainst("one", "one")
	var metadataErr *recordlayer.MetaDataError
	if !errors.As(err, &metadataErr) {
		t.Fatalf("got %v", err)
	}
}
