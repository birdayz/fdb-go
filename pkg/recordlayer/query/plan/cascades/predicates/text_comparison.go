// Portions derived from FoundationDB Record Layer (Comparisons.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package predicates

import (
	"fmt"

	"fdb.dev/pkg/recordlayer"
)

// IsText reports whether the comparison is a full-text predicate.
func (t ComparisonType) IsText() bool {
	return t >= ComparisonTextContainsAll && t <= ComparisonTextContainsAnyPrefix
}

// TextComparisonError reports an invalid text predicate or comparand.
type TextComparisonError struct{ Message string }

func (e *TextComparisonError) Error() string { return e.Message }

// recordLayerTextType maps a text ComparisonType to the Record Layer's
// Comparisons.Type subset.
func recordLayerTextType(t ComparisonType) (recordlayer.TextComparisonType, bool) {
	switch t {
	case ComparisonTextContainsAll:
		return recordlayer.TextContainsAll, true
	case ComparisonTextContainsAllWithin:
		return recordlayer.TextContainsAllWithin, true
	case ComparisonTextContainsAny:
		return recordlayer.TextContainsAny, true
	case ComparisonTextContainsPhrase:
		return recordlayer.TextContainsPhrase, true
	case ComparisonTextContainsPrefix:
		return recordlayer.TextContainsPrefix, true
	case ComparisonTextContainsAllPrefixes:
		return recordlayer.TextContainsAllPrefixes, true
	case ComparisonTextContainsAnyPrefix:
		return recordlayer.TextContainsAnyPrefix, true
	}
	return 0, false
}

// RecordLayerTextComparison builds the Record Layer comparison
// (Comparisons.TextComparison and its TextWithMaxDistanceComparison and
// TextContainsAllPrefixesComparison subclasses) for this text comparison and
// an evaluated comparand. A string comparand is tokenized by whoever uses the
// comparison (the index tokenizer for a scan, the comparison's tokenizer for
// evaluation); a list of strings is used as given, "" standing for a stop
// word, as in Java's Text.containsAll(List). A nil comparand returns nil: Java
// evaluates a comparison without comparand tokens to null.
func (c Comparison) RecordLayerTextComparison(comparand any) (*recordlayer.TextComparison, error) {
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
	case recordlayer.TextContainsAllWithin:
		if isList {
			return recordlayer.NewTextWithMaxDistanceComparisonTokens(tokens, c.TextMaxDistance, tokenizer, fallback), nil
		}
		return recordlayer.NewTextWithMaxDistanceComparison(text, c.TextMaxDistance, tokenizer, fallback), nil
	case recordlayer.TextContainsAllPrefixes:
		// Text.containsAllPrefixes(..., strict) uses the probable
		// intersection's default sizing (Text.java:388-396).
		expected, fpp := recordlayer.ProbableIntersectionDefaultExpectedResults, recordlayer.ProbableIntersectionDefaultFalsePositivePercentage
		if isList {
			return recordlayer.NewTextContainsAllPrefixesComparisonTokens(tokens, c.TextStrictPrefix, expected, fpp, tokenizer, fallback), nil
		}
		return recordlayer.NewTextContainsAllPrefixesComparison(text, c.TextStrictPrefix, expected, fpp, tokenizer, fallback), nil
	}
	if isList {
		return recordlayer.NewTextComparisonTokens(typ, tokens, tokenizer, fallback), nil
	}
	return recordlayer.NewTextComparison(typ, text, tokenizer, fallback), nil
}

// evalText is Comparisons.TextComparison.eval (Comparisons.java:2602-2619),
// executed by recordlayer.TextComparison.Eval.
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
			return TriUnknown, &recordlayer.RecordCoreArgumentError{Message: fmt.Sprintf("Text comparison applied against non-string value: %v", left)}
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
