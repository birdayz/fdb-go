package functions

import "strings"

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
