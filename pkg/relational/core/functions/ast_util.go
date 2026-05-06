package functions

import (
	"strings"

	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// StripIdentifierQuotes normalizes an identifier's raw parse text to
// its canonical lookup form: quoted identifiers are stripped of their
// surrounding `"` or backticks and otherwise preserved case-for-case;
// unquoted identifiers are folded to upper case. Mirrors Java's
// SemanticAnalyzer.normalizeString (case-sensitive=false default).
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
