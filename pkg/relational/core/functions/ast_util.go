package functions

import (
	"strings"

	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// StripIdentifierQuotes normalizes an identifier's raw parse text to
// its canonical lookup form: quoted identifiers are stripped of their
// surrounding `"` or backticks and otherwise preserved case-for-case;
// unquoted identifiers are folded to upper case. Mirrors Java's
// SemanticAnalyzer.normalizeString (case-sensitive=false default).
// THE NAME UNDERSELLS IT: this is the NORMALIZER, not a quote stripper. An
// unquoted identifier comes back UPPER-FOLDED — Java's
// SemanticAnalyzer.normalizeString — so it is NOT idempotent and must be
// applied exactly ONCE, at the parse boundary.
//
// Applying it twice silently destroys every quoted identifier and is invisible
// for every unquoted one, because folding twice is folding once. That is not
// hypothetical: all three CTE column-alias captures called
// `StripIdentifierQuotes(FullIdToName(fid))`, and FullIdToName already applies
// it per segment — so `"x"` became `x` became `X`, and `WITH c("x")` published
// a column no reference could name. It survived a whole review lap because
// every site that CONSUMED the alias was faithful; the corruption was upstream
// of all of them, spelled as a function whose name promises a no-op.
func StripIdentifierQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '`' && s[len(s)-1] == '`')) {
		return s[1 : len(s)-1]
	}
	return strings.ToUpper(s)
}

// FullIdToName converts a FullId parse-tree node to a dot-separated,
// quote-stripped name. Used for table names in INSERT / UPDATE /
// DELETE and by the scalar function library when an argument is a
// qualified column reference.
func FullIdToName(fid antlrgen.IFullIdContext) string {
	uids := fid.AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = StripIdentifierQuotes(u.GetText())
	}
	return strings.Join(parts, ".")
}

// ResolveQualifiedTableName validates and strips a schema qualifier
// from a dotted table name. Ports Java's SemanticAnalyzer.tableExists
// qualifier validation (lines 189-207):
//
//   - No dot → returns the name as-is.
//   - One dot (schema.table) → validates qualifier matches schemaName
//     (case-insensitive), returns just the table name.
//   - Two+ dots → error (Java returns INTERNAL_ERROR).
//
// schemaName is the current schema context (e.g., from session).
// Returns (tableName, errCode, errMsg). errCode is "" on success.
func ResolveQualifiedTableName(dottedName, schemaName string) (string, error) {
	dot := strings.IndexByte(dottedName, '.')
	if dot < 0 {
		return dottedName, nil
	}
	qualifier := dottedName[:dot]
	rest := dottedName[dot+1:]

	if strings.IndexByte(rest, '.') >= 0 {
		return "", api.NewErrorf(api.ErrCodeInternalError,
			"multi-part qualified table name %q not supported", dottedName)
	}
	if !strings.EqualFold(qualifier, schemaName) {
		return "", api.NewErrorf(api.ErrCodeUndefinedDatabase,
			"Unknown database %s", qualifier)
	}
	return rest, nil
}
