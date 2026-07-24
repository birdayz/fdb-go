package embedded

import (
	"strings"
)

// colRef carries structured column identity: a (table, column) pair.
// Mirrors Java's Identifier{name, qualifier} / FieldValue(QOV(correlation), field).
// The table part is empty for unqualified references.
type colRef struct {
	table string // table alias or "" for unqualified
	col   string // bare column name
}

// parseColRef splits a flat "TABLE.COL" string into a structured colRef.
// Unqualified names produce colRef{"", "COL"}.
func parseColRef(s string) colRef {
	if dot := strings.LastIndex(s, "."); dot >= 0 {
		return colRef{table: s[:dot], col: s[dot+1:]}
	}
	return colRef{col: s}
}

// bare returns the unqualified column name.
func (r colRef) bare() string {
	return r.col
}

// isQualified returns true when the reference has a table qualifier.
func (r colRef) isQualified() bool {
	return r.table != ""
}

// isPlainQualifiedColumnReference distinguishes the qualifier dot in a column
// label from a dot rendered inside a function/expression label. Identifier
// quotes have already been stripped at this layer, so punctuation cannot be
// used as a general rejection criterion: a delimited machinery identifier may
// legitimately contain spaces, dashes, or operators. Parentheses, however,
// identify the rendered aggregate/function label at issue and are not part of
// a plain qualified column reference after parsing.
func isPlainQualifiedColumnReference(s string) bool {
	ref := parseColRef(s)
	if !ref.isQualified() || ref.table == "" || ref.col == "" {
		return false
	}
	return !strings.ContainsAny(s, "()")
}
