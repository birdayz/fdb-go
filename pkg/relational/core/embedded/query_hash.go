package embedded

import (
	"strconv"
	"strings"

	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"github.com/antlr4-go/antlr/v4"
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
// The scope is kept verbatim: schema names are case-SENSITIVE (the
// catalog/session preserve them exactly), so folding the scope would merge `s`
// and `S` into one key — the very staleness bug this scope exists to prevent.
//
// The companion query text is planCacheText(q), rendered from the tokens.
//
// NOTE (optimization gap, not a correctness bug): Java's AstNormalizer also
// PARAMETERIZES literals so `... WHERE x = 1` and `... WHERE x = 2` share a
// plan with different bindings. Go keys on the literal text, so those miss —
// more cache misses, never a wrong plan. Closing that is a separate reach item.
//
// THE LITERAL TEXT IN THIS KEY IS ALSO LOAD-BEARING FOR CORRECTNESS once RFC-257
// WS-J section 3.5 lets a plan match an index whose stored key holds a literal (an
// arithmetic or bitmap function key, `d & 1`, `bitmap_bucket_offset(id)`) against
// the query's literal by VALUE (today candidate construction declines those keys).
// Java makes such a plan reusable only under a QueryPlanConstraint that re-checks
// the constant (ConstantValueEquivalence, ValueEquivalence.java:351-395); Go has no
// such constraint (cascades/value_equivalence.go), and a plan is sound for another
// literal only because another literal is another key here. Parameterizing
// literals without porting that constraint would serve `d & 1` its plan for
// `d & 2`, and for the same reason a ParameterValue, whose `?` text is one key for
// every binding, must never match a stored literal.
//
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

// planCacheText renders a statement's plan-cache key text from its TOKENS, as
// Java's AstNormalizer.visitTerminal does: comments are never tokens; a keyword
// or punctuation token (one with a literal name in the lexer vocabulary) and an
// unquoted identifier are upper-cased; every other token (string, B64, hex and
// national literals, numbers, quoted identifiers) is kept byte for byte; tokens
// are joined by one space. A token never begins or ends with a space and holds
// one only inside quotes, so distinct token sequences render distinctly —
// `SELECT AB` / `SELECT A B`, `'a b'` / `'a' 'b'`, `B64'YWJj'` / `B64'ywjj'`.
func planCacheText(tree antlr.ParseTree) string {
	var b strings.Builder
	var walk func(antlr.Tree)
	walk = func(n antlr.Tree) {
		if term, ok := n.(antlr.TerminalNode); ok {
			tok := term.GetSymbol()
			if tok == nil || tok.GetTokenType() == antlr.TokenEOF {
				return
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			text := tok.GetText()
			if foldsInCacheText(tok.GetTokenType()) {
				text = strings.ToUpper(text)
			}
			b.WriteString(text)
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			walk(n.GetChild(i))
		}
	}
	walk(tree)
	return b.String()
}

var lexerLiteralNames = func() []string {
	return antlrgen.NewRelationalLexer(nil).LiteralNames
}()

// foldsInCacheText reports whether a token's spelling is case-insensitive: a
// keyword or punctuation token, or an unquoted identifier (which the analyzer
// folds to upper case). Anything else is data and is kept verbatim.
func foldsInCacheText(tokenType int) bool {
	if tokenType == antlrgen.RelationalLexerID {
		return true
	}
	return tokenType > 0 && tokenType < len(lexerLiteralNames) && lexerLiteralNames[tokenType] != ""
}
