package embedded

import (
	"strconv"
	"strings"
	"unicode"
)

// planCacheScopeDelim separates the schema/version scope from the query text
// in a plan-cache key. It cannot occur in a schema name or SQL source, so the
// scope can never bleed into the query text.
const planCacheScopeDelim = "\x01"

// planCacheScope builds the VERBATIM (un-normalized) plan-cache scope — the
// schema identity + metadata version — that PlanCache prepends to the
// normalized query text. A SET SCHEMA switch (connection.go SetSchema mutates
// only the session schema, never the cache) or a metadata-version bump then
// keys differently, so the same SQL against a different schema/table set can
// no longer return a stale plan. Java's QueryCacheKey carries the schema
// template version for the same reason (RFC-024).
//
// The scope is kept out of normalizeSQL deliberately: schema names are
// case-SENSITIVE (the catalog/session preserve them exactly), so normalizing
// the scope would fold `s` and `S` into one key — the very staleness bug this
// scope exists to prevent. PlanCache.Get/Put normalize ONLY the query text.
//
// The companion query text is canonicalTextOf(q): it preserves token
// boundaries (GetText() concatenated tokens with no separator, colliding
// `SELECT AB` with `SELECT A B` — a wrong-plan bug), and normalizeSQL then
// case/whitespace/comment-folds equivalent spellings so they still share.
//
// NOTE (optimization gap, not a correctness bug): Java's AstNormalizer also
// PARAMETERIZES literals so `... WHERE x = 1` and `... WHERE x = 2` share a
// plan with different bindings. Go keys on the literal text, so those miss —
// more cache misses, never a wrong plan. Closing that is a separate reach item.
func planCacheScope(schema string, metaDataVersion int) string {
	return schema + planCacheScopeDelim + strconv.Itoa(metaDataVersion)
}

// normalizeSQL strips comments, collapses whitespace, uppercases
// characters outside single-quoted string literals, and trims.
func normalizeSQL(sql string) string {
	sql = stripComments(sql)
	sql = collapseWhitespace(sql)
	sql = upperOutsideStrings(sql)
	sql = strings.TrimSpace(sql)
	return sql
}

// upperOutsideStrings uppercases only characters outside QUOTED spans,
// preserving case inside them. Three quote kinds are case-preserved and for
// the same reason — case is significant there:
//   - single quotes '...' — string literals (the value's case is data)
//   - double quotes "..." and backticks `...` — DELIMITED IDENTIFIERS, which
//     are case-SENSITIVE in this engine (StripIdentifierQuotes preserves their
//     case; only UNQUOTED identifiers fold to upper). Folding `"a"` and `"A"`
//     together here would give two distinct columns one cache key — a
//     wrong-plan bug in the same family as the GetText() non-injectivity.
//
// Each quote kind closes on its own delimiter and honours the doubled-quote
// escape (two of the same quote char in a row) so an escaped quote stays
// inside the span.
func upperOutsideStrings(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	var quote byte // 0 = outside; otherwise the open quote char
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if quote != 0 {
			b.WriteByte(ch)
			if ch == quote {
				// Doubled quote is an escape — stays inside the span.
				if i+1 < len(sql) && sql[i+1] == quote {
					b.WriteByte(sql[i+1])
					i++
					continue
				}
				quote = 0
			}
		} else if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			b.WriteByte(ch)
		} else {
			b.WriteRune(unicode.ToUpper(rune(ch)))
		}
	}
	return b.String()
}

// stripComments removes single-line (-- and MySQL #) and block (/* */)
// comments.
func stripComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	i := 0
	for i < len(sql) {
		// Block comment: /* ... */
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) {
				if sql[i] == '*' && sql[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			// If we ran off the end without finding */, just stop.
			if i >= len(sql) {
				break
			}
			// Replace the comment with a space so tokens don't merge.
			b.WriteByte(' ')
			continue
		}

		// Single-line comment: `--` or MySQL `#` to end of line. The lexer
		// routes both to the hidden channel (LINE_COMMENT), so q.GetText()
		// dropped them; canonicalTextOf keeps them (it reads the raw source
		// span), so they must be stripped here or a `# trace` comment change
		// would churn the cache for an otherwise-identical query.
		if (i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-') || sql[i] == '#' {
			if sql[i] == '#' {
				i++
			} else {
				i += 2
			}
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			// Replace the comment with a space so tokens don't merge.
			b.WriteByte(' ')
			continue
		}

		// Quoted span (string literal ' ', or delimited identifier " / `):
		// don't strip comment markers inside it — a `--` or `/*` in a value or
		// identifier is data, not a comment. Honours the doubled-quote escape.
		if sql[i] == '\'' || sql[i] == '"' || sql[i] == '`' {
			q := sql[i]
			b.WriteByte(sql[i])
			i++
			for i < len(sql) {
				if sql[i] == q {
					b.WriteByte(sql[i])
					i++
					if i < len(sql) && sql[i] == q { // escaped quote
						b.WriteByte(sql[i])
						i++
						continue
					}
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			continue
		}

		b.WriteByte(sql[i])
		i++
	}

	return b.String()
}

// collapseWhitespace replaces runs of whitespace with a single space, EXCEPT
// inside quoted spans, where whitespace is significant and preserved verbatim:
// a string literal 'a  b' is a different VALUE than 'a b', and a delimited
// identifier "a  b" a different NAME than "a b" — collapsing either would give
// distinct queries one cache key (a wrong-rows / wrong-plan collision). Honours
// the doubled-quote escape for each quote kind, matching upperOutsideStrings.
func collapseWhitespace(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	// Rune iteration (not bytes): a multibyte codepoint outside quotes — e.g.
	// U+00A0 NBSP — must be classified by unicode.IsSpace as a whole rune, not
	// have one of its UTF-8 bytes misfire the ASCII-space test. Quote chars are
	// ASCII, so rune comparison against them is exact.
	runes := []rune(sql)
	inSpace := false
	var quote rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			b.WriteRune(r)
			if r == quote {
				if i+1 < len(runes) && runes[i+1] == quote {
					b.WriteRune(runes[i+1])
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			quote = r
			inSpace = false
			b.WriteRune(r)
			continue
		}
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}

	return b.String()
}
