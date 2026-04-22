package functions

import (
	"strings"

	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// StripIdentifierQuotes removes surrounding double-quote or backtick
// pairs from an identifier's raw parse text. Used before comparing
// identifier names.
func StripIdentifierQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '`' && s[len(s)-1] == '`')) {
		return s[1 : len(s)-1]
	}
	return s
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
