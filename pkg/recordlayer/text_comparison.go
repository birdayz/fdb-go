// Portions derived from FoundationDB Record Layer (Comparisons.java, Text.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"errors"

	"fdb.dev/internal/recordlayer/textcmp"
)

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

// TranslateTextCmpError maps internal textcmp errors to public recordlayer error types.
func TranslateTextCmpError(err error) error {
	if err == nil {
		return nil
	}
	var rlArg *RecordCoreArgumentError
	var rlMD *MetaDataError
	var rlCore *RecordCoreError
	if errors.As(err, &rlArg) || errors.As(err, &rlMD) || errors.As(err, &rlCore) {
		return err
	}
	var argErr *textcmp.ArgumentError
	if errors.As(err, &argErr) {
		return &RecordCoreArgumentError{Message: argErr.Message}
	}
	var mdErr *textcmp.MetaDataError
	if errors.As(err, &mdErr) {
		return &MetaDataError{Message: mdErr.Message, Cause: err}
	}
	var invErr *textcmp.InvalidComparisonError
	if errors.As(err, &invErr) {
		return &RecordCoreError{Message: invErr.Message}
	}
	return err
}

// TextComparison includes Java's max-distance and all-prefixes variants (Comparisons.java:2529-2900).
// String comparands use the index tokenizer; token lists are used as given.
type TextComparison struct {
	inner *textcmp.TextComparison
}

// NewTextComparison constructs a string comparison; empty fallback selects the default tokenizer.
// ALL_WITHIN and ALL_PREFIXES require their specialized constructors.
func NewTextComparison(typ TextComparisonType, tokens string, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{inner: textcmp.NewTextComparison(typ, tokens, tokenizerName, fallbackTokenizerName)}
}

// NewTextComparisonTokens uses supplied tokens verbatim; empty tokens stand for stop words.
func NewTextComparisonTokens(typ TextComparisonType, tokens []string, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{inner: textcmp.NewTextComparisonTokens(typ, tokens, tokenizerName, fallbackTokenizerName)}
}

// NewTextWithMaxDistanceComparison is Comparisons.TextWithMaxDistanceComparison
// (Comparisons.java:2751-2763): TEXT_CONTAINS_ALL_WITHIN.
func NewTextWithMaxDistanceComparison(tokens string, maxDistance int, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{inner: textcmp.NewTextWithMaxDistanceComparison(tokens, maxDistance, tokenizerName, fallbackTokenizerName)}
}

// NewTextWithMaxDistanceComparisonTokens is the List<String> constructor of
// Comparisons.TextWithMaxDistanceComparison.
func NewTextWithMaxDistanceComparisonTokens(tokens []string, maxDistance int, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{inner: textcmp.NewTextWithMaxDistanceComparisonTokens(tokens, maxDistance, tokenizerName, fallbackTokenizerName)}
}

// NewTextContainsAllPrefixesComparison matches Java's variant (Comparisons.java:2826-2862).
// Text.containsAllPrefixes uses strict=true and the default probable-intersection settings.
func NewTextContainsAllPrefixesComparison(tokenPrefixes string, strict bool, expectedRecords int64, falsePositivePercentage float64, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{inner: textcmp.NewTextContainsAllPrefixesComparison(tokenPrefixes, strict, expectedRecords, falsePositivePercentage, tokenizerName, fallbackTokenizerName)}
}

// NewTextContainsAllPrefixesComparisonTokens is the List<String> constructor of
// Comparisons.TextContainsAllPrefixesComparison.
func NewTextContainsAllPrefixesComparisonTokens(tokenPrefixes []string, strict bool, expectedRecords int64, falsePositivePercentage float64, tokenizerName, fallbackTokenizerName string) *TextComparison {
	return &TextComparison{inner: textcmp.NewTextContainsAllPrefixesComparisonTokens(tokenPrefixes, strict, expectedRecords, falsePositivePercentage, tokenizerName, fallbackTokenizerName)}
}

// Type is getType.
func (c *TextComparison) Type() TextComparisonType { return c.inner.Type() }

// TokenizerName is getTokenizerName ("" when the comparison accepts any).
func (c *TextComparison) TokenizerName() string { return c.inner.TokenizerName() }

// FallbackTokenizerName is getFallbackTokenizerName.
func (c *TextComparison) FallbackTokenizerName() string { return c.inner.FallbackTokenizerName() }

// MaxDistance is TextWithMaxDistanceComparison.getMaxDistance.
func (c *TextComparison) MaxDistance() int { return c.inner.MaxDistance() }

// IsStrict is TextContainsAllPrefixesComparison.isStrict.
func (c *TextComparison) IsStrict() bool { return c.inner.IsStrict() }

// ExpectedRecords is TextContainsAllPrefixesComparison.getExpectedRecords.
func (c *TextComparison) ExpectedRecords() int64 { return c.inner.ExpectedRecords() }

// FalsePositivePercentage is
// TextContainsAllPrefixesComparison.getFalsePositivePercentage.
func (c *TextComparison) FalsePositivePercentage() float64 {
	return c.inner.FalsePositivePercentage()
}

// Comparand is getComparand: the token list, or else the token string.
func (c *TextComparison) Comparand() any { return c.inner.Comparand() }

// String renders the comparison like Java's explain: TYPE comparand.
func (c *TextComparison) String() string {
	if c == nil || c.inner == nil {
		return "<nil>"
	}
	return c.inner.String()
}

// Eval returns nil for null/all-stop-word inputs; otherwise it tokenizes in INDEX mode
// at the tokenizer's maximum version (Comparisons.java:2602-2619).
func (c *TextComparison) Eval(value any) (*bool, error) {
	res, err := c.inner.Eval(value)
	if err != nil {
		return nil, TranslateTextCmpError(err)
	}
	return res, nil
}
