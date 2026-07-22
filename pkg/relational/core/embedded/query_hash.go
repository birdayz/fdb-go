package embedded

import (
	"strconv"
	"strings"
	"unicode"

	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// planCacheScopeDelim separates the schema/version scope from the query text
// in a plan-cache key. It survives normalizeSQL (not whitespace, ToUpper-
// stable) and cannot occur in a schema name or SQL source, so the scope can
// never bleed into the query text.
const planCacheScopeDelim = "\x01"

// planCacheKeyInput builds the plan-cache lookup input for a query. It fixes
// two correctness bugs in the prior q.GetText() key:
//
//   - INJECTIVE: canonicalTextOf preserves token boundaries (it reads the
//     source span). GetText() concatenated tokens with NO separator, so
//     `SELECT AB FROM T` and `SELECT A B FROM T` both collapsed to
//     "SELECTABFROMT" and shared one cache entry — a wrong-plan bug.
//     PlanCache.normalizeSQL then case/whitespace/comment-folds the canonical
//     text, so equivalent spellings still share an entry.
//   - SCOPED: prefixing the schema identity + metadata version means a
//     SET SCHEMA switch (connection.go SetSchema mutates only the session
//     schema, never the cache) or a metadata-version bump can no longer return
//     a plan resolved against a different schema/table set. Java's
//     QueryCacheKey carries the schema template version for the same reason
//     (RFC-024). DDL on this connection already flushes the whole cache; this
//     scope is the additional guard for the SET SCHEMA case.
//
// NOTE (optimization gap, not a correctness bug): Java's AstNormalizer also
// PARAMETERIZES literals so `... WHERE x = 1` and `... WHERE x = 2` share a
// plan with different bindings. Go keys on the literal text, so those miss —
// more cache misses, never a wrong plan. Closing that is a separate reach item.
func planCacheKeyInput(schema string, metaDataVersion int, q antlrgen.IQueryContext) string {
	return schema + planCacheScopeDelim +
		strconv.Itoa(metaDataVersion) + planCacheScopeDelim +
		canonicalTextOf(q)
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
// escape (`”`, `""`, ` “ `) so an escaped quote stays inside the span.
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

// stripComments removes single-line (--) and block (/* */) comments.
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

		// Single-line comment: -- to end of line
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			i += 2
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

	inSpace := false
	var quote byte
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if quote != 0 {
			b.WriteByte(ch)
			if ch == quote {
				if i+1 < len(sql) && sql[i+1] == quote {
					b.WriteByte(sql[i+1])
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			inSpace = false
			b.WriteByte(ch)
			continue
		}
		if r := rune(ch); unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteByte(ch)
	}

	return b.String()
}
