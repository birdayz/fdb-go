package embedded

import (
	"sort"
	"strings"

	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

// caseSensitiveIdentifiers rewrites every unquoted identifier of sql into its
// quoted form, `Table1` to `"Table1"`. Under the CASE_SENSITIVE_IDENTIFIERS
// connection option Java's normalizeString keeps an unquoted identifier as
// written instead of upper-casing it (SemanticAnalyzer.unquoteOrUpperCase),
// which is exactly what quoting it does, so the rest of the engine needs no
// option of its own. Identifiers are the grammar's `uid` nodes, the only place
// Java normalizes; keywords, literals and built-in function names are not
// uids and are untouched. A statement that does not parse is returned as it
// is, for the ordinary path to report.
func caseSensitiveIdentifiers(sql string) string {
	root, err := parser.Parse(sql)
	if err != nil || root == nil {
		return sql
	}
	type span struct{ start, stop int }
	var spans []span
	var walk func(antlr.Tree)
	walk = func(n antlr.Tree) {
		if u, ok := n.(*antlrgen.UidContext); ok {
			if u.DOUBLE_QUOTE_ID() == nil && u.GetStart() != nil && u.GetStop() != nil {
				spans = append(spans, span{u.GetStart().GetStart(), u.GetStop().GetStop()})
			}
			return
		}
		// A user-defined scalar call name is normalized like a uid
		// (ExpressionVisitor.visitUserDefinedScalarFunctionCall), but a
		// built-in reached through that rule resolves case-insensitively
		// before any user function (SqlFunctionCatalogImpl's lower-cased
		// synonyms), so its name is left as written.
		if f, ok := n.(*antlrgen.UserDefinedScalarFunctionNameContext); ok {
			if id := f.ID(); id != nil && !strings.EqualFold(id.GetText(), "CARDINALITY") && !strings.EqualFold(id.GetText(), "JAVA_CALL") {
				spans = append(spans, span{id.GetSymbol().GetStart(), id.GetSymbol().GetStop()})
			}
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			walk(n.GetChild(i))
		}
	}
	walk(root)
	if len(spans) == 0 {
		return sql
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	runes := []rune(sql)
	out := make([]rune, 0, len(runes)+2*len(spans))
	at := 0
	for _, s := range spans {
		if s.start < at || s.stop >= len(runes) || s.stop < s.start {
			return sql
		}
		out = append(out, runes[at:s.start]...)
		out = append(out, '"')
		out = append(out, runes[s.start:s.stop+1]...)
		out = append(out, '"')
		at = s.stop + 1
	}
	out = append(out, runes[at:]...)
	return string(out)
}
