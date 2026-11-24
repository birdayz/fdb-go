// Portions derived from FoundationDB Record Layer (Comparisons.java, Text.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import "fdb.dev/internal/recordlayer/textcmp"

// TextComparisonType is the full-text subset of Java's Comparisons.Type
// (Comparisons.java:632-638).
type TextComparisonType = textcmp.TextComparisonType

const (
	TextContainsAll         = textcmp.TextContainsAll
	TextContainsAllWithin   = textcmp.TextContainsAllWithin
	TextContainsAny         = textcmp.TextContainsAny
	TextContainsPhrase      = textcmp.TextContainsPhrase
	TextContainsPrefix      = textcmp.TextContainsPrefix
	TextContainsAllPrefixes = textcmp.TextContainsAllPrefixes
	TextContainsAnyPrefix   = textcmp.TextContainsAnyPrefix
)

// TextComparison includes Java's max-distance and all-prefixes variants (Comparisons.java:2529-2900).
// String comparands use the index tokenizer; token lists are used as given.
type TextComparison = textcmp.TextComparison

// NewTextComparison constructs a string comparison; empty fallback selects the default tokenizer.
// ALL_WITHIN and ALL_PREFIXES require their specialized constructors.
func NewTextComparison(typ TextComparisonType, tokens string, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return textcmp.NewTextComparison(typ, tokens, tokenizerName, fallbackTokenizerName)
}

// NewTextComparisonTokens uses supplied tokens verbatim; empty tokens stand for stop words.
func NewTextComparisonTokens(typ TextComparisonType, tokens []string, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return textcmp.NewTextComparisonTokens(typ, tokens, tokenizerName, fallbackTokenizerName)
}

// NewTextWithMaxDistanceComparison is Comparisons.TextWithMaxDistanceComparison
// (Comparisons.java:2751-2763): TEXT_CONTAINS_ALL_WITHIN.
func NewTextWithMaxDistanceComparison(tokens string, maxDistance int, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return textcmp.NewTextWithMaxDistanceComparison(tokens, maxDistance, tokenizerName, fallbackTokenizerName)
}

// NewTextWithMaxDistanceComparisonTokens is the List<String> constructor of
// Comparisons.TextWithMaxDistanceComparison.
func NewTextWithMaxDistanceComparisonTokens(tokens []string, maxDistance int, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return textcmp.NewTextWithMaxDistanceComparisonTokens(tokens, maxDistance, tokenizerName, fallbackTokenizerName)
}

// NewTextContainsAllPrefixesComparison matches Java's variant (Comparisons.java:2826-2862).
// Text.containsAllPrefixes uses strict=true and the default probable-intersection settings.
func NewTextContainsAllPrefixesComparison(tokenPrefixes string, strict bool, expectedRecords int64, falsePositivePercentage float64, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return textcmp.NewTextContainsAllPrefixesComparison(tokenPrefixes, strict, expectedRecords, falsePositivePercentage, tokenizerName, fallbackTokenizerName)
}

// NewTextContainsAllPrefixesComparisonTokens is the List<String> constructor of
// Comparisons.TextContainsAllPrefixesComparison.
func NewTextContainsAllPrefixesComparisonTokens(tokenPrefixes []string, strict bool, expectedRecords int64, falsePositivePercentage float64, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return textcmp.NewTextContainsAllPrefixesComparisonTokens(tokenPrefixes, strict, expectedRecords, falsePositivePercentage, tokenizerName, fallbackTokenizerName)
}
