package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// LIKE prefix pushdown: `WHERE col LIKE 'foo%'` narrows to the
// contiguous range `[prefix, strinc(prefix))` when the pattern is a
// pure literal prefix followed by a single trailing `%` — no `_`
// wildcards, no interior `%`, no ESCAPE clause. Any other pattern
// bails to the scan path, which applies likeMatch per row.
//
// Scope:
//   - `col LIKE 'foo%'` where the pattern resolves (after ESCAPE
//     processing) to a pure literal prefix followed by a single
//     trailing `%`. No unescaped `_`, no interior unescaped `%`.
//   - ESCAPE clause is honoured: `LIKE 'foo\_%' ESCAPE '\'` narrows
//     on the literal prefix "foo_".
//   - NOT LIKE bails (complement of a contiguous range isn't
//     contiguous).
//   - Empty-prefix patterns (`%`, `%foo`, `%foo%`) bail — the whole
//     range already covers them so narrowing is a no-op anyway.
//   - No-wildcard patterns (`LIKE 'exact'`) bail too: they are
//     equality, and the scan path's likeMatch handles them trivially.
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
// leaf is exactly `col LIKE 'literal_prefix%'` (optionally with an
// ESCAPE clause) and the pattern resolves to a pure literal prefix
// followed by a single trailing `%` — no unescaped `_`, no interior
// unescaped `%`. The returned prefix is the decoded literal string
// (escape sequences applied).
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

	colName, ok := extractColumnRef(pred.ExpressionAtom())
	if !ok {
		return "", "", false
	}

	patternTok := like.GetPattern()
	if patternTok == nil {
		return "", "", false
	}
	pattern := functions.StripStringLiteralQuotes(patternTok.GetText())

	// Resolve the optional ESCAPE clause. Invalid escape char counts
	// (zero or more than one) bail — the scan path surfaces 22023
	// cleanly for malformed patterns.
	escape := rune(-1)
	if esc := like.GetEscape(); esc != nil {
		escStr := functions.StripStringLiteralQuotes(esc.GetText())
		runes := []rune(escStr)
		if len(runes) != 1 {
			return "", "", false
		}
		escape = runes[0]
	}

	prefix, prefixOk := likePatternToPrefix(pattern, escape)
	if !prefixOk {
		return "", "", false
	}
	return colName, prefix, true
}

// likePatternToPrefix walks a LIKE pattern and returns (decoded
// prefix, true) when the pattern has a non-empty literal prefix
// before the first unescaped wildcard (`%` or `_`). The prefix is
// the narrowing range low-bound; any wildcards and literal suffix
// after it are handled by the scan loop's post-filter likeMatch.
//
// Escape-char handling follows SQL: an escape char consumes the
// next char verbatim (including `%`, `_`, or another escape). A
// dangling escape at end-of-pattern bails to the scan path.
//
// Bail cases:
//   - Pattern starts with a wildcard (no literal prefix to narrow).
//   - Pattern has no wildcard at all (exact match — scan path
//     handles equality trivially, no range to narrow).
//   - Dangling escape at end of pattern.
//
// Exported shape for the fuzz target and future reuse by cursor-
// elimination logic.
func likePatternToPrefix(pattern string, escape rune) (string, bool) {
	runes := []rune(pattern)
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escape >= 0 && r == escape {
			if i+1 >= len(runes) {
				// Dangling escape — malformed, bail. Scan path
				// surfaces the error.
				return "", false
			}
			b.WriteRune(runes[i+1])
			i++
			continue
		}
		switch r {
		case '%', '_':
			// First unescaped wildcard terminates the literal prefix.
			if b.Len() == 0 {
				return "", false // pattern starts with wildcard
			}
			return b.String(), true
		default:
			b.WriteRune(r)
		}
	}
	// Pattern consumed with no wildcard — treat as exact match, no
	// range narrowing needed (scan path's likeMatch handles it).
	return "", false
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
// `[prefix, strinc(prefix))`. Falls back to "low bound only" when
// the prefix has no byte-level successor (all-0xFF — unreachable for
// valid UTF-8 input, but guarded): the scan runs to the end of the
// record-type range and evalPredicate post-filters.
//
// Always succeeds for non-empty prefixes; callers don't need to
// handle a failure case.
func likePrefixToPKRangeBounds(prefix string) pkRangeBounds {
	high, ok := likePrefixStrinc(prefix)
	if !ok {
		return pkRangeBounds{
			hasLow:       true,
			low:          prefix,
			lowInclusive: true,
		}
	}
	return pkRangeBounds{
		hasLow:        true,
		low:           prefix,
		lowInclusive:  true,
		hasHigh:       true,
		high:          high,
		highInclusive: false,
	}
}
