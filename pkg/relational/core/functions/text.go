package functions

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// javaDoubleToString matches Java's Double.toString(double); the
// contract (decimal inside [1e-3, 1e7), scientific outside, Infinity
// spellings) lives in ONE place — values.JavaDoubleToString.
func javaDoubleToString(n float64) string {
	return values.JavaDoubleToString(n)
}

// javaFloatToString is Float.toString — the float32 sibling.
func javaFloatToString(n float32) string {
	return values.JavaFloatToString(n)
}

// StripStringLiteralQuotes removes a single pair of surrounding
// single-quotes and unescapes doubled-quote ” sequences to a single
// quote. Used by every SQL string literal that reaches our code via
// the parser (the parser leaves the literal's raw source text,
// including quotes).
func StripStringLiteralQuotes(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "''", "'")
}
