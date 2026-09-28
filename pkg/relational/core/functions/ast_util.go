package functions

import (
	"strings"

	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// NormalizeIdentifier normalizes an identifier's raw parse text to its
// canonical lookup form: a quoted identifier is stripped of its surrounding `"`
// or backticks and otherwise preserved case-for-case; an unquoted one is folded
// to UPPER. Mirrors Java's SemanticAnalyzer.normalizeString
// (case-sensitive=false default).
//
// IT IS NOT IDEMPOTENT, and must be applied exactly ONCE, at the parse
// boundary. Applying it twice silently destroys every quoted identifier while
// being invisible for every unquoted one, because folding twice is folding
// once.
//
// It used to be called StripIdentifierQuotes, and that name is why this needs
// saying: all three CTE column-alias captures wrote
// `StripIdentifierQuotes(FullIdToName(fid))` — and FullIdToName already applies
// it per segment. So `"x"` became `x` became `X`, and `WITH c("x")` published a
// column no reference could name. Every site that CONSUMED the alias was
// faithful; the corruption was upstream of all of them, spelled as a function
// whose name promised a no-op. Renamed so the double application reads as
// obviously wrong at the call site rather than as a defensive strip.
func NormalizeIdentifier(s string) string {
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
		parts[i] = NormalizeIdentifier(u.GetText())
	}
	return strings.Join(parts, ".")
}

// A table name carries at most one qualifier, and the qualifier is the name of
// the connection's SCHEMA TEMPLATE, compared exactly with both sides
// normalized (an unquoted identifier upper-cased, a quoted one verbatim): Java's
// SemanticAnalyzer reads a table in its metadata catalog, which is the schema
// template, and tableExists and getTable accept a qualifier only when it
// equals metadataCatalog.getName(). The schema's own name is not a qualifier.
// Measured against the target: with schema S over template T, `S.w` is refused
// and `T.w`, `t.w` and `"T".w` resolve (conformance/
// ws_f_table_qualifier_conformance_test.go).
//
// The two readings below are Java's two lookups. A statement's target (INSERT,
// UPDATE, DELETE) is required to be a table and refuses a bad qualifier
// itself; a FROM source only asks whether it names a table and, when it does
// not, goes on to the next reading (a CTE, a view, a correlated field), so its
// refusal belongs to the caller.

// ResolveTargetTablePath resolves a statement's target table as Java's
// SemanticAnalyzer.getTable does: more than one qualifier is an internal
// error, "Unknown table <path>"; a qualifier other than templateName is 42F00
// "Unknown schema template <qualifier>". It returns the table's name, the last
// segment; whether that table exists is the caller's check (42F01).
func ResolveTargetTablePath(path []string, templateName string) (string, error) {
	if err := checkTablePath(path); err != nil {
		return "", err
	}
	if len(path) > 2 {
		return "", api.NewErrorf(api.ErrCodeInternalError, "Unknown table %s", strings.Join(path, "."))
	}
	if len(path) == 2 && path[0] != templateName {
		return "", api.NewErrorf(api.ErrCodeUndefinedDatabase, "Unknown schema template %s", path[0])
	}
	return path[len(path)-1], nil
}

// ResolveSourceTablePath is the qualifier half of Java's
// SemanticAnalyzer.tableExists, for a FROM source: it reports the table name a
// path names when the path is unqualified or qualified by templateName, and
// false otherwise, so the caller can go on to the source's other readings. A
// qualified source that is no table ends, in Java, in the correlated-field
// reading, whose refusal is UnknownSourceReferenceError. An empty path or
// segment is no user's spelling but a carrier that lost its name: an internal
// error.
func ResolveSourceTablePath(path []string, templateName string) (string, bool, error) {
	if err := checkTablePath(path); err != nil {
		return "", false, err
	}
	switch {
	case len(path) == 1:
		return path[0], true, nil
	case len(path) == 2 && path[0] == templateName:
		return path[1], true, nil
	default:
		return "", false, nil
	}
}

// ResolveQualifiedTableName is ResolveSourceTablePath over a dotted string, for
// the callers that still hold a joined name. It cannot tell a quoted dot from a
// qualifier, which is why parse-derived callers pass segments.
func ResolveQualifiedTableName(dottedName, templateName string) (string, bool) {
	if dottedName == "" {
		return "", false
	}
	name, ok, err := ResolveSourceTablePath(strings.Split(dottedName, "."), templateName)
	return name, ok && err == nil
}

// NonArrayCorrelationError is Java's refusal of a correlated FROM item that is
// not an array (LogicalOperator.generateCorrelatedFieldAccess,
// INVALID_COLUMN_REFERENCE), in its wording; typeText is the column's type as
// Java's DataType.toString renders it (rowstruct.NonArrayCorrelationError).
func NonArrayCorrelationError(typeText string) error {
	return api.NewErrorf(api.ErrCodeInvalidColumnReference,
		"join correlation can occur only on column of repeated type, not %s type", typeText)
}

// UnknownSourceReferenceError is Java's answer for a qualified FROM source
// that names no table: SemanticAnalyzer.resolveCorrelatedIdentifier finds no
// column of an outer source by that path, UNDEFINED_COLUMN "Unknown reference
// <path>", the path printed as Java's Identifier prints it (segments joined by
// dots, without quotes).
func UnknownSourceReferenceError(path []string) error {
	return api.NewErrorf(api.ErrCodeUndefinedColumn, "Unknown reference %s", strings.Join(path, "."))
}

func checkTablePath(path []string) error {
	if len(path) == 0 {
		return api.NewError(api.ErrCodeInternalError, "table identifier path is empty")
	}
	for _, part := range path {
		if part == "" {
			return api.NewError(api.ErrCodeInternalError, "table identifier path contains an empty segment")
		}
	}
	return nil
}
