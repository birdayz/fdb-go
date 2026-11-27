// Portions derived from FoundationDB Record Layer (Comparisons.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package predicates

import (
	"fmt"

	"fdb.dev/internal/recordlayer/textcmp"
)

// IsText reports whether the comparison is a full-text predicate.
func (t ComparisonType) IsText() bool {
	return t >= ComparisonTextContainsAll && t <= ComparisonTextContainsAnyPrefix
}

// IsTextPrefix reports a text comparison whose scan reads token prefixes.
func (t ComparisonType) IsTextPrefix() bool {
	return t == ComparisonTextContainsPrefix || t == ComparisonTextContainsAllPrefixes || t == ComparisonTextContainsAnyPrefix
}

// TextComparisonError reports an invalid text predicate or comparand.
type TextComparisonError struct{ Message string }

func (e *TextComparisonError) Error() string { return e.Message }

// recordLayerTextType maps a text ComparisonType to the Record Layer's
// Comparisons.Type subset.
func recordLayerTextType(t ComparisonType) (textcmp.TextComparisonType, bool) {
	switch t {
	case ComparisonTextContainsAll:
		return textcmp.TextContainsAll, true
	case ComparisonTextContainsAllWithin:
		return textcmp.TextContainsAllWithin, true
	case ComparisonTextContainsAny:
		return textcmp.TextContainsAny, true
	case ComparisonTextContainsPhrase:
		return textcmp.TextContainsPhrase, true
	case ComparisonTextContainsPrefix:
		return textcmp.TextContainsPrefix, true
	case ComparisonTextContainsAllPrefixes:
		return textcmp.TextContainsAllPrefixes, true
	case ComparisonTextContainsAnyPrefix:
		return textcmp.TextContainsAnyPrefix, true
	}
	return 0, false
}

// RecordLayerTextComparison is Java's Comparisons.TextComparison for an
// evaluated comparand: a string to tokenize, or a token list; nil for NULL.
func (c Comparison) RecordLayerTextComparison(comparand any) (*textcmp.TextComparison, error) {
	typ, ok := recordLayerTextType(c.Type)
	if !ok {
		return nil, &TextComparisonError{Message: fmt.Sprintf("not a text comparison: %v", c.Type)}
	}
	var tokens []string
	var text string
	isList := false
	switch v := comparand.(type) {
	case nil:
		return nil, nil
	case string:
		text = v
	case []string:
		tokens, isList = append([]string{}, v...), true
	case []any:
		tokens, isList = make([]string, len(v)), true
		for i, term := range v {
			s, ok := term.(string)
			if !ok {
				return nil, &TextComparisonError{Message: fmt.Sprintf("text query token has incompatible type: %T", term)}
			}
			tokens[i] = s
		}
	default:
		return nil, &TextComparisonError{Message: fmt.Sprintf("comparand for text query has incompatible type: %T", comparand)}
	}
	tokenizer, fallback := c.TextTokenizerName, c.TextAnalyzerName
	switch typ {
	case textcmp.TextContainsAllWithin:
		if isList {
			return textcmp.NewTextWithMaxDistanceComparisonTokens(tokens, c.TextMaxDistance, tokenizer, fallback), nil
		}
		return textcmp.NewTextWithMaxDistanceComparison(text, c.TextMaxDistance, tokenizer, fallback), nil
	case textcmp.TextContainsAllPrefixes:
		// Text.containsAllPrefixes(..., strict) uses the probable
		// intersection's default sizing (Text.java:388-396).
		expected, fpp := textcmp.ProbableIntersectionDefaultExpectedResults, textcmp.ProbableIntersectionDefaultFalsePositivePercentage
		if isList {
			return textcmp.NewTextContainsAllPrefixesComparisonTokens(tokens, c.TextStrictPrefix, expected, fpp, tokenizer, fallback), nil
		}
		return textcmp.NewTextContainsAllPrefixesComparison(text, c.TextStrictPrefix, expected, fpp, tokenizer, fallback), nil
	}
	if isList {
		return textcmp.NewTextComparisonTokens(typ, tokens, tokenizer, fallback), nil
	}
	return textcmp.NewTextComparison(typ, text, tokenizer, fallback), nil
}

// evalText is Comparisons.TextComparison.eval (Comparisons.java:2602-2619),
// executed by textcmp.TextComparison.Eval.
func (c Comparison) evalText(left, right any) (TriBool, error) {
	if left == nil {
		return TriUnknown, nil
	}
	comparison, err := c.RecordLayerTextComparison(right)
	if err != nil {
		return TriUnknown, err
	}
	if comparison == nil {
		// The value check precedes the null-comparand result.
		if _, ok := left.(string); !ok {
			return TriUnknown, &textcmp.ArgumentError{Message: fmt.Sprintf("Text comparison applied against non-string value: %v", left)}
		}
		return TriUnknown, nil
	}
	result, err := comparison.Eval(left)
	if err != nil || result == nil {
		return TriUnknown, err
	}
	if *result {
		return TriTrue, nil
	}
	return TriFalse, nil
}
