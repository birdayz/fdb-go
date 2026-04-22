package embedded

import (
	"context"
	"strings"

	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// LIKE prefix pushdown: `WHERE col LIKE 'foo%'` narrows to the
// contiguous range `[prefix, strinc(prefix))` when the pattern is a
// pure literal prefix followed by a single trailing `%` — no `_`
// wildcards, no interior `%`, no ESCAPE clause. Any other pattern
// bails to the scan path, which applies likeMatch per row.
//
// Scope:
//   - Only `col LIKE 'foo%'` where "foo" contains neither `%` nor
//     `_`. Escaped wildcards (via an ESCAPE clause) are conservative
//     territory — MVP bails rather than handle the escape rules here.
//   - NOT LIKE bails (complement of a contiguous range isn't
//     contiguous).
//   - Empty-prefix patterns (`%`, `%foo`, `%foo%`) bail — the whole
//     range already covers them so narrowing is a no-op anyway.
//   - 0xFF-overflow prefixes (in theory — Go string LIKE prefixes
//     are valid UTF-8, where no byte is 0xFF, so the strinc always
//     has a well-defined successor).
//
// Integration:
//   - Feeds into tryPKRangeFromWhere / trySecondaryIndexRangeFromWhere
//     as another source of range bounds on a column. The range
//     detection loop adds `[prefix, strinc(prefix))` to the column's
//     bounds; the existing range-scan machinery then builds the
//     TupleRange.
//   - Scan path still applies the full LIKE predicate via
//     evalPredicate, so a pushdown that is a superset of the matching
//     rows (e.g. range plus post-filter) stays correct.

// extractColLikePrefixLiteral returns (colName, prefix, ok) when the
// leaf is exactly `col LIKE 'literal_prefix%'` with no other
// wildcards and no ESCAPE clause. The returned prefix is the literal
// substring before the trailing `%`.
func extractColLikePrefixLiteral(
	_ context.Context,
	_ *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (string, string, bool) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", "", false
	}
	if pred.Predicate() == nil {
		return "", "", false
	}
	like, ok := pred.Predicate().(*antlrgen.LikePredicateContext)
	if !ok {
		return "", "", false
	}
	if like.NOT() != nil {
		return "", "", false
	}
	// ESCAPE handling introduces per-escape rewrite rules; conservative
	// MVP bails rather than get it subtly wrong.
	if like.GetEscape() != nil {
		return "", "", false
	}

	colName, ok := extractColumnRef(pred.ExpressionAtom())
	if !ok {
		return "", "", false
	}

	patternTok := like.GetPattern()
	if patternTok == nil {
		return "", "", false
	}
	pattern := stripStringLiteralQuotes(patternTok.GetText())

	// Must end in `%`.
	if !strings.HasSuffix(pattern, "%") {
		return "", "", false
	}
	body := pattern[:len(pattern)-1]
	// The body must not contain `%` or `_` — those would make the
	// matching non-contiguous / single-char-wildcard.
	if strings.ContainsAny(body, "%_") {
		return "", "", false
	}
	// Empty prefix matches everything; scan is already the same.
	if body == "" {
		return "", "", false
	}
	return colName, body, true
}

// likePrefixStrinc returns the smallest string that is strictly
// greater than any string starting with `prefix`, viewed as a byte
// sequence. Equivalent to incrementing the last byte, or popping
// trailing 0xFF bytes and then incrementing. Returns ("", false) when
// every byte is 0xFF (no successor — the caller should use an open
// upper bound).
//
// Valid-UTF-8 prefixes always have a successor because 0xFF never
// appears in valid UTF-8 (UTF-8 uses 0x00-0x7F, 0xC0-0xFD historically
// with 0xF5-0xFF excluded from legal 4-byte sequences). So the
// "all-0xFF" branch is unreachable in practice — guard it anyway.
func likePrefixStrinc(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			out := make([]byte, i+1)
			copy(out, b[:i+1])
			out[i]++
			return string(out), true
		}
	}
	return "", false
}

// likePrefixToPKRangeBounds converts a LIKE-prefix match on a STRING
// column into a pkRangeBounds describing the half-open range
// `[prefix, strinc(prefix))`. Returns !ok when the prefix has no
// successor (all-0xFF) — unreachable for valid UTF-8 input.
func likePrefixToPKRangeBounds(prefix string) (pkRangeBounds, bool) {
	high, ok := likePrefixStrinc(prefix)
	if !ok {
		// Treat as "low bound only" open-high range. Safe: the scan
		// runs to the end of the record-type range and evalPredicate
		// post-filters.
		return pkRangeBounds{
			hasLow:       true,
			low:          prefix,
			lowInclusive: true,
		}, true
	}
	return pkRangeBounds{
		hasLow:        true,
		low:           prefix,
		lowInclusive:  true,
		hasHigh:       true,
		high:          high,
		highInclusive: false,
	}, true
}
