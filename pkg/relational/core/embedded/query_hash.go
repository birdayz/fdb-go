package embedded

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// planCacheScopeDelim separates a scope component's LENGTH from the component
// bytes that follow it. Unlike a separator between components, it needs no
// property of the component bytes to be unambiguous: a length prefix is always
// digits, so the first delimiter after it ends the prefix, and the component is
// then consumed by count rather than by looking for the next delimiter.
const planCacheScopeDelim = "\x01"

// planCacheScope builds the VERBATIM (un-normalized) plan-cache scope — the
// database path + schema identity + metadata version + planner options — that
// PlanCache prepends to the normalized query text. A SET SCHEMA switch
// (connection.go SetSchema mutates only the session schema, never the cache)
// or a metadata-version bump then
// keys differently, so the same SQL against a different schema/table set can
// no longer return a stale plan. Java's QueryCacheKey carries the schema
// template version for the same reason (RFC-024).
//
// The database path is in the scope because a schema NAME is only unique
// within a database: `/tenant_a` and `/tenant_b` both having a `MAIN` schema
// is the ordinary multi-tenant shape, and those two schemas are different
// table sets resolving to different subspaces. Keying on the schema name
// alone would serve one tenant's compiled plan for the other's query the
// moment a single cache outlives one DBPath — exactly the cross-schema
// wrong-plan collision the rest of this scope exists to prevent. The session's
// schema cache already keys on (dbPath, schema) via session.SchemaCacheKey;
// the plan cache was the outlier.
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
// plannerOpts is the resolved planner-option signature (plannerOptions.
// cacheKeyPart): a plan built with PLAN_RIGHT_DEEP or a disabled rule set is
// NOT the plan the same SQL gets under the defaults, so it must not be served
// from the same key. Java's QueryCacheKey carries the whole PlannerConfiguration
// for exactly this reason.
//
// Injectivity is by CONSTRUCTION, not by a property of the components: every
// component is length-prefixed (decimal byte count, delimiter, then exactly
// that many bytes), so a decoder never infers a boundary from content and no
// component byte — the delimiter itself included — can be mistaken for one.
// This is the scheme cacheKeyPart already uses within the planner-option
// component, and values.ProjectionOutputIdentityKey uses for the same problem.
//
// Joining the components with a delimiter instead was NOT safe, and the corner
// was reachable: index names reach this key verbatim through cacheKeyPart's
// readable-index section, and an index name is a quotable SQL identifier rather
// than a restricted character class. Under a delimiter join,
//
//	planCacheScope("", "S", 0, "i2:\x010") == planCacheScope("", "S\x010\x01i2:", 0, "")
//
// — one schema/readable-index-set's compiled plan served for a DIFFERENT one.
// The readable-index view is in this key precisely so a plan built while an
// index was readable is not served after that index goes WRITE_ONLY, so a
// collision reinstates exactly that wrong-plan bug. Restricting the components
// instead would have meant proving a filter complete against every future
// component; length-prefixing cannot be defeated by a cleverer name.
//
// The empty plannerOpts case is still emitted (as a zero-length component)
// rather than dropped: an omitted component would make component COUNT
// content-dependent, which is the same ambiguity one level up.
//
// The cost on the plan-cache lookup path is four small decimal renderings
// (strconv.Itoa returns a shared constant for small values, so they do not
// allocate) into an EXACTLY pre-sized builder — one allocation of the same size
// the delimiter join used, since the length prefixes replace the separators
// byte-for-byte for components under ten bytes.
func planCacheScope(dbPath, schema string, metaDataVersion int, plannerOpts string) string {
	version := strconv.Itoa(metaDataVersion)
	var b strings.Builder
	b.Grow(lengthPrefixedSize(dbPath) + lengthPrefixedSize(schema) + lengthPrefixedSize(version) + lengthPrefixedSize(plannerOpts))
	writeLengthPrefixed(&b, dbPath)
	writeLengthPrefixed(&b, schema)
	writeLengthPrefixed(&b, version)
	writeLengthPrefixed(&b, plannerOpts)
	return b.String()
}

// writeLengthPrefixed appends one self-delimiting scope component: the byte
// count in canonical decimal (so no two counts render the same digits), the
// delimiter, then the bytes verbatim.
func writeLengthPrefixed(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteString(planCacheScopeDelim)
	b.WriteString(s)
}

// lengthPrefixedSize is the exact byte count writeLengthPrefixed will append,
// so the scope builder allocates once and never grows.
func lengthPrefixedSize(s string) int {
	digits := 1
	for n := len(s); n >= 10; n /= 10 {
		digits++
	}
	return digits + len(planCacheScopeDelim) + len(s)
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

	// ASCII fast path: iterate BYTES and decode a full rune only when a
	// multibyte lead byte is seen. This keeps ordinary ASCII SQL (the warm
	// cache-hit case) allocation-free — a []rune conversion would heap-allocate
	// on every hit — while still classifying a multibyte codepoint outside
	// quotes (e.g. U+00A0 NBSP) as a whole rune, not letting one of its UTF-8
	// bytes misfire the ASCII-space test. Quote chars are ASCII, so byte
	// comparison against them is exact.
	inSpace := false
	var quote byte
	for i := 0; i < len(sql); {
		ch := sql[i]
		if quote != 0 {
			b.WriteByte(ch)
			if ch == quote {
				if i+1 < len(sql) && sql[i+1] == quote {
					b.WriteByte(sql[i+1])
					i += 2
					continue
				}
				quote = 0
			}
			i++
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			inSpace = false
			b.WriteByte(ch)
			i++
			continue
		}
		if ch < utf8.RuneSelf {
			// ASCII: the byte IS the rune, so unicode.IsSpace(rune(ch)) is
			// exact and allocation-free.
			if unicode.IsSpace(rune(ch)) {
				if !inSpace {
					b.WriteByte(' ')
					inSpace = true
				}
			} else {
				inSpace = false
				b.WriteByte(ch)
			}
			i++
			continue
		}
		// Multibyte: decode the whole rune to classify whitespace correctly.
		r, size := utf8.DecodeRuneInString(sql[i:])
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			inSpace = false
			b.WriteString(sql[i : i+size])
		}
		i += size
	}

	return b.String()
}
