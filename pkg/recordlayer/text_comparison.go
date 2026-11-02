// Portions derived from FoundationDB Record Layer (Comparisons.java, Text.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"strings"
)

// TextComparisonType is the full-text subset of Java's Comparisons.Type
// (Comparisons.java:632-638).
type TextComparisonType int

const (
	TextContainsAll TextComparisonType = iota
	TextContainsAllWithin
	TextContainsAny
	TextContainsPhrase
	TextContainsPrefix
	TextContainsAllPrefixes
	TextContainsAnyPrefix
)

// String is the Java enum constant name.
func (t TextComparisonType) String() string {
	switch t {
	case TextContainsAll:
		return "TEXT_CONTAINS_ALL"
	case TextContainsAllWithin:
		return "TEXT_CONTAINS_ALL_WITHIN"
	case TextContainsAny:
		return "TEXT_CONTAINS_ANY"
	case TextContainsPhrase:
		return "TEXT_CONTAINS_PHRASE"
	case TextContainsPrefix:
		return "TEXT_CONTAINS_PREFIX"
	case TextContainsAllPrefixes:
		return "TEXT_CONTAINS_ALL_PREFIXES"
	case TextContainsAnyPrefix:
		return "TEXT_CONTAINS_ANY_PREFIX"
	}
	return fmt.Sprintf("TextComparisonType(%d)", int(t))
}

// IsEquality is Comparisons.Type.isEquality (Comparisons.java:632-638): the
// whole-token comparisons are equality comparisons, the prefix ones are not.
func (t TextComparisonType) IsEquality() bool {
	switch t {
	case TextContainsAll, TextContainsAllWithin, TextContainsAny, TextContainsPhrase:
		return true
	}
	return false
}

// TextComparison is Java's Comparisons.TextComparison together with its
// TextWithMaxDistanceComparison and TextContainsAllPrefixesComparison
// subclasses (Comparisons.java:2529-2900). The comparand is either a string,
// tokenized with the index's tokenizer when scanned, or a list of tokens used
// as given.
type TextComparison struct {
	typ       TextComparisonType
	tokenList []string
	tokenStr  *string

	// tokenizerName is the tokenizer the comparison requires ("" = any);
	// fallbackTokenizerName tokenizes it when evaluated against a record and
	// no tokenizer is required.
	tokenizerName         string
	fallbackTokenizerName string

	// maxDistance belongs to TEXT_CONTAINS_ALL_WITHIN.
	maxDistance int
	// strict, expectedRecords and falsePositivePercentage belong to
	// TEXT_CONTAINS_ALL_PREFIXES.
	strict                  bool
	expectedRecords         int64
	falsePositivePercentage float64
}

// NewTextComparison is Comparisons.TextComparison(type, String tokens,
// tokenizerName, fallbackTokenizerName). TEXT_CONTAINS_ALL_WITHIN and
// TEXT_CONTAINS_ALL_PREFIXES have their own constructors. An empty
// fallbackTokenizerName is the default tokenizer, as in Java's Text
// (Text.java:71).
func NewTextComparison(typ TextComparisonType, tokens string, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{typ: typ, tokenStr: &tokens, tokenizerName: tokenizerName, fallbackTokenizerName: fallbackOrDefault(fallbackTokenizerName)}
}

// NewTextComparisonTokens is Comparisons.TextComparison(type, List<String>
// tokens, tokenizerName, fallbackTokenizerName): the tokens are used as given,
// "" standing for a stop word.
func NewTextComparisonTokens(typ TextComparisonType, tokens []string, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{typ: typ, tokenList: append([]string{}, tokens...), tokenizerName: tokenizerName, fallbackTokenizerName: fallbackOrDefault(fallbackTokenizerName)}
}

// NewTextWithMaxDistanceComparison is Comparisons.TextWithMaxDistanceComparison
// (Comparisons.java:2751-2763): TEXT_CONTAINS_ALL_WITHIN.
func NewTextWithMaxDistanceComparison(tokens string, maxDistance int, tokenizerName, fallbackTokenizerName string) *TextComparison {
	c := NewTextComparison(TextContainsAllWithin, tokens, tokenizerName, fallbackTokenizerName)
	c.maxDistance = maxDistance
	return c
}

// NewTextWithMaxDistanceComparisonTokens is the List<String> constructor of
// Comparisons.TextWithMaxDistanceComparison.
func NewTextWithMaxDistanceComparisonTokens(tokens []string, maxDistance int, tokenizerName, fallbackTokenizerName string) *TextComparison {
	c := NewTextComparisonTokens(TextContainsAllWithin, tokens, tokenizerName, fallbackTokenizerName)
	c.maxDistance = maxDistance
	return c
}

// NewTextContainsAllPrefixesComparison is
// Comparisons.TextContainsAllPrefixesComparison (Comparisons.java:2826-2862).
// Text.containsAllPrefixes uses strict=true with
// ProbableIntersectionDefaultExpectedResults and
// ProbableIntersectionDefaultFalsePositivePercentage.
func NewTextContainsAllPrefixesComparison(tokenPrefixes string, strict bool, expectedRecords int64, falsePositivePercentage float64, tokenizerName, fallbackTokenizerName string) *TextComparison {
	c := NewTextComparison(TextContainsAllPrefixes, tokenPrefixes, tokenizerName, fallbackTokenizerName)
	c.strict, c.expectedRecords, c.falsePositivePercentage = strict, expectedRecords, falsePositivePercentage
	return c
}

// NewTextContainsAllPrefixesComparisonTokens is the List<String> constructor of
// Comparisons.TextContainsAllPrefixesComparison.
func NewTextContainsAllPrefixesComparisonTokens(tokenPrefixes []string, strict bool, expectedRecords int64, falsePositivePercentage float64, tokenizerName, fallbackTokenizerName string) *TextComparison {
	c := NewTextComparisonTokens(TextContainsAllPrefixes, tokenPrefixes, tokenizerName, fallbackTokenizerName)
	c.strict, c.expectedRecords, c.falsePositivePercentage = strict, expectedRecords, falsePositivePercentage
	return c
}

func fallbackOrDefault(name string) string {
	if name == "" {
		return DefaultTextTokenizerName
	}
	return name
}

// Type is getType.
func (c *TextComparison) Type() TextComparisonType { return c.typ }

// TokenizerName is getTokenizerName ("" when the comparison accepts any).
func (c *TextComparison) TokenizerName() string { return c.tokenizerName }

// FallbackTokenizerName is getFallbackTokenizerName.
func (c *TextComparison) FallbackTokenizerName() string { return c.fallbackTokenizerName }

// MaxDistance is TextWithMaxDistanceComparison.getMaxDistance.
func (c *TextComparison) MaxDistance() int { return c.maxDistance }

// IsStrict is TextContainsAllPrefixesComparison.isStrict.
func (c *TextComparison) IsStrict() bool { return c.strict }

// ExpectedRecords is TextContainsAllPrefixesComparison.getExpectedRecords.
func (c *TextComparison) ExpectedRecords() int64 { return c.expectedRecords }

// FalsePositivePercentage is
// TextContainsAllPrefixesComparison.getFalsePositivePercentage.
func (c *TextComparison) FalsePositivePercentage() float64 { return c.falsePositivePercentage }

// Comparand is getComparand: the token list, or else the token string.
func (c *TextComparison) Comparand() any {
	if c.tokenList != nil {
		return append([]string{}, c.tokenList...)
	}
	if c.tokenStr != nil {
		return *c.tokenStr
	}
	return nil
}

// String renders the comparison like Java's explain: TYPE comparand.
func (c *TextComparison) String() string {
	comparand := "null"
	if c.tokenList != nil {
		comparand = "[" + strings.Join(c.tokenList, ", ") + "]"
	} else if c.tokenStr != nil {
		comparand = *c.tokenStr
	}
	switch c.typ {
	case TextContainsAllWithin:
		return fmt.Sprintf("%s(%d) %s", c.typ, c.maxDistance, comparand)
	case TextContainsAllPrefixes:
		mode := "APPROXIMATELY"
		if c.strict {
			mode = "STRICTLY"
		}
		return fmt.Sprintf("%s((%s) %s", c.typ, mode, comparand)
	}
	return fmt.Sprintf("%s %s", c.typ, comparand)
}

// textTokenizerForEval is TextComparison.tokenize's tokenizer choice
// (Comparisons.java:2574-2577): the required tokenizer, else the fallback.
func (c *TextComparison) textTokenizerForEval() (TextTokenizer, error) {
	name := c.tokenizerName
	if name == "" {
		name = c.fallbackTokenizerName
	}
	return GetTextTokenizer(name)
}

// comparandTokens is getComparandTokens (Comparisons.java:2659-2668).
func (c *TextComparison) comparandTokens(tokenizer TextTokenizer) ([]string, error) {
	if c.tokenList != nil {
		return c.tokenList, nil
	}
	if c.tokenStr == nil {
		return nil, nil
	}
	tokens, err := tokenizer.TokenizeToList(*c.tokenStr, tokenizer.MaxVersion(), TokenizerModeQuery)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		tokens = []string{}
	}
	return tokens, nil
}

// Eval is TextComparison.eval (Comparisons.java:2602-2619): nil means
// unknown (a null value, or a comparand of only stop words). The value is
// tokenized in INDEX mode at the tokenizer's maximum version.
func (c *TextComparison) Eval(value any) (*bool, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("Text comparison applied against non-string value: %v", value)}
	}
	tokenizer, err := c.textTokenizerForEval()
	if err != nil {
		return nil, err
	}
	comparand, err := c.comparandTokens(tokenizer)
	if err != nil {
		return nil, err
	}
	if comparand == nil {
		return nil, nil
	}
	textTokens, err := tokenizer.TokenizeToList(text, tokenizer.MaxVersion(), TokenizerModeIndex)
	if err != nil {
		return nil, err
	}
	return c.evalComparison(textTokens, comparand)
}

// evalComparison is TextComparison.evalComparison (Comparisons.java:2579-2599)
// and TextWithMaxDistanceComparison.evalComparison (Comparisons.java:2766-2771).
func (c *TextComparison) evalComparison(text []string, comparand []string) (*bool, error) {
	switch c.typ {
	case TextContainsAll:
		return compareTextContainsAll(text, comparand), nil
	case TextContainsAllWithin:
		return compareTextContainsAllWithin(text, comparand, c.maxDistance), nil
	case TextContainsAny:
		return compareTextContainsAny(text, comparand), nil
	case TextContainsPhrase:
		return compareTextContainsPhrase(text, comparand), nil
	case TextContainsPrefix:
		if len(comparand) != 1 {
			return nil, &RecordCoreArgumentError{Message: "Cannot evaluate prefix comparison with multiple tokens"}
		}
		return compareTextContainsPrefix(text, comparand[0]), nil
	case TextContainsAnyPrefix:
		return compareTextContainsAnyPrefix(text, comparand), nil
	case TextContainsAllPrefixes:
		return compareTextContainsAllPrefixes(text, comparand), nil
	}
	return nil, &RecordCoreError{Message: fmt.Sprintf("Cannot evaluate text comparison of type: %s", c.typ)}
}

func boolPtr(b bool) *bool { return &b }

// compareTextContainsSingle is Comparisons.compareTextContainsSingle
// (Comparisons.java:358-372).
func compareTextContainsSingle(text []string, token string) *bool {
	if token == "" {
		return nil
	}
	for _, t := range text {
		if t != "" && t == token {
			return boolPtr(true)
		}
	}
	return boolPtr(false)
}

// compareTextContainsPrefix is Comparisons.compareTextContainsPrefix
// (Comparisons.java:374-388).
func compareTextContainsPrefix(text []string, prefix string) *bool {
	if prefix == "" {
		return nil
	}
	for _, t := range text {
		if t != "" && strings.HasPrefix(t, prefix) {
			return boolPtr(true)
		}
	}
	return boolPtr(false)
}

// comparandSet is Comparisons.getComparandSet (Comparisons.java:390-406),
// keeping first-occurrence order so the single-element case is the same element.
func comparandSet(comparand []string) []string {
	var set []string
	seen := map[string]bool{}
	for _, t := range comparand {
		if t != "" && !seen[t] {
			seen[t] = true
			set = append(set, t)
		}
	}
	return set
}

// compareTextContainsAll is Comparisons.compareTextContainsAll
// (Comparisons.java:408-431).
func compareTextContainsAll(text []string, comparand []string) *bool {
	set := comparandSet(comparand)
	if len(set) == 0 {
		return nil
	}
	if len(set) == 1 {
		return compareTextContainsSingle(text, set[0])
	}
	want := map[string]bool{}
	for _, t := range set {
		want[t] = true
	}
	matched := map[string]bool{}
	for _, t := range text {
		if t != "" && want[t] {
			matched[t] = true
			if len(matched) == len(set) {
				return boolPtr(true)
			}
		}
	}
	return boolPtr(false)
}

// compareTextContainsAllWithin is Comparisons.compareTextContainsAllWithin
// (Comparisons.java:433-478): a sliding window of the last maxDistance tokens.
func compareTextContainsAllWithin(text []string, comparand []string, maxDistance int) *bool {
	set := comparandSet(comparand)
	if len(set) == 0 {
		return nil
	}
	if len(set) == 1 {
		return compareTextContainsSingle(text, set[0])
	}
	want := map[string]bool{}
	for _, t := range set {
		want[t] = true
	}
	seen := map[string]int{}
	var window []string
	for _, t := range text {
		if t != "" && want[t] {
			seen[t]++
			if len(seen) == len(set) {
				return boolPtr(true)
			}
		}
		if len(window) == maxDistance {
			last := window[0]
			window = window[1:]
			if n, ok := seen[last]; ok {
				if n > 1 {
					seen[last] = n - 1
				} else {
					delete(seen, last)
				}
			}
		}
		window = append(window, t)
	}
	return boolPtr(false)
}

// compareTextContainsAny is Comparisons.compareTextContainsAny
// (Comparisons.java:480-498).
func compareTextContainsAny(text []string, comparand []string) *bool {
	set := comparandSet(comparand)
	if len(set) == 0 {
		return nil
	}
	if len(set) == 1 {
		return compareTextContainsSingle(text, set[0])
	}
	want := map[string]bool{}
	for _, t := range set {
		want[t] = true
	}
	for _, t := range text {
		if t != "" && want[t] {
			return boolPtr(true)
		}
	}
	return boolPtr(false)
}

// compareTextContainsAllPrefixes is Comparisons.compareTextContainsAllPrefixes
// (Comparisons.java:500-527).
func compareTextContainsAllPrefixes(text []string, comparand []string) *bool {
	set := comparandSet(comparand)
	if len(set) == 0 {
		return nil
	}
	if len(set) == 1 {
		return compareTextContainsPrefix(text, set[0])
	}
	matched := map[string]bool{}
	for _, t := range text {
		if t == "" {
			continue
		}
		for _, p := range set {
			if strings.HasPrefix(t, p) {
				matched[p] = true
			}
		}
		if len(matched) == len(set) {
			return boolPtr(true)
		}
	}
	return boolPtr(false)
}

// compareTextContainsAnyPrefix is Comparisons.compareTextContainsAnyPrefix
// (Comparisons.java:529-551).
func compareTextContainsAnyPrefix(text []string, comparand []string) *bool {
	set := comparandSet(comparand)
	if len(set) == 0 {
		return nil
	}
	if len(set) == 1 {
		return compareTextContainsPrefix(text, set[0])
	}
	for _, t := range text {
		if t == "" {
			continue
		}
		for _, p := range set {
			if strings.HasPrefix(t, p) {
				return boolPtr(true)
			}
		}
	}
	return boolPtr(false)
}

// compareTextContainsPhrase is Comparisons.compareTextContainsPhrase
// (Comparisons.java:553-614): leading and trailing stop words are dropped and
// an inner stop word matches any token.
func compareTextContainsPhrase(text []string, comparand []string) *bool {
	first := 0
	for first < len(comparand) && comparand[first] == "" {
		first++
	}
	if first == len(comparand) {
		return nil
	}
	last := len(comparand)
	for last > first && comparand[last-1] == "" {
		last--
	}
	comparand = comparand[first:last]
	if len(comparand) == 0 {
		return nil
	}
	if len(comparand) == 1 {
		return compareTextContainsSingle(text, comparand[0])
	}
	// Each element is the index of the next comparand token a partial match
	// expects; Java keeps the same queue of comparand iterators.
	var positions []int
	for _, t := range text {
		n := len(positions)
		for i := 0; i < n; i++ {
			next := positions[0]
			positions = positions[1:]
			if comparand[next] == "" || comparand[next] == t {
				if next+1 < len(comparand) {
					positions = append(positions, next+1)
				} else {
					return boolPtr(true)
				}
			}
		}
		if t == comparand[0] {
			positions = append(positions, 1)
		}
	}
	return boolPtr(false)
}
