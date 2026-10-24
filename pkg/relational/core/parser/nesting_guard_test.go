package parser

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func isNestingError(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == api.ErrCodeSyntaxError &&
		strings.Contains(ae.Message, "nesting exceeds maximum depth")
}

// The guard refuses parenthesis nesting deeper than maxParenNesting, and the
// limit itself parses.
func TestParse_NestingGuardBoundary(t *testing.T) {
	t.Parallel()
	nested := func(depth int) string {
		return "SELECT " + strings.Repeat("(", depth) + "1" + strings.Repeat(")", depth) + " FROM t"
	}
	if _, err := Parse(nested(maxParenNesting)); err != nil {
		t.Fatalf("depth %d: %v, want it parsed", maxParenNesting, err)
	}
	if _, err := Parse(nested(maxParenNesting + 1)); !isNestingError(err) {
		t.Fatalf("depth %d: %v, want the nesting error", maxParenNesting+1, err)
	}
	// Unbalanced closing brackets do not lower the count below zero.
	if _, err := Parse("SELECT 1) " + nested(maxParenNesting+1)); !isNestingError(err) {
		t.Fatalf("stray ')' then depth %d: %v, want the nesting error", maxParenNesting+1, err)
	}
}

// A parenthesis inside a string literal, a quoted identifier or a comment is
// part of that token, not nesting.
func TestParse_NestingGuardIgnoresParensInsideTokens(t *testing.T) {
	t.Parallel()
	many := strings.Repeat("(", 2*maxParenNesting)
	for name, sql := range map[string]string{
		"string literal":    "SELECT * FROM t WHERE s = '" + many + "'",
		"quoted identifier": `SELECT "` + many + `" FROM t`,
		"block comment":     "SELECT 1 /* " + many + " */ FROM t",
		"line comment":      "SELECT 1 FROM t -- " + many + "\n",
		"closing in literal": "SELECT * FROM t WHERE s = '" + strings.Repeat(")", 2*maxParenNesting) + "' AND " +
			strings.Repeat("(", 3) + "s = 'x'" + strings.Repeat(")", 3),
	} {
		if _, err := Parse(sql); err != nil {
			t.Errorf("%s: %v, want it parsed", name, err)
		}
	}
}
