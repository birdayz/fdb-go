package embedded

import (
	"fmt"
	"strconv"
	"strings"

	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// jdbcColumnName transforms an internal projection name into the
// JDBC-style result-set metadata name that fdb-relational emits.
// Java uppercases unquoted identifiers, strips qualifiers from
// projected columns, and synthesises `_N` (zero-based position) for
// anonymous expression projections (CASE, COUNT(*), arithmetic, etc.).
//
// Rules:
//   - Bare simple identifier ("id", "name") → upper-cased ("ID").
//   - Qualified column ("t.id", "u.name") → strip qualifier, upper.
//   - Anything containing operators / parens / spaces → "_<pos>".
//
// Idempotent: applying twice yields the same result, so it's safe
// to chain through multiple staticRows wrappers (CTE → SELECT, UNION
// over already-transformed sources).
func jdbcColumnName(name string, position int) string {
	base := parseColRef(name).bare()
	if isSimpleIdentifier(base) {
		return strings.ToUpper(base)
	}
	return fmt.Sprintf("_%d", position)
}

// isSimpleIdentifier reports whether s matches `[A-Za-z_][A-Za-z0-9_]*`.
// Empty strings, leading digits, and any non-ASCII / punctuation
// characters fail the check.
func isSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// jdbcizeColumnNames returns a copy of cols with each entry transformed
// via jdbcColumnName. Used at the driver-output boundary so the
// internal column-name flow (used for ORDER BY resolution, alias
// remapping, etc.) stays unchanged.
func jdbcizeColumnNames(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = jdbcColumnName(c, i)
	}
	return out
}

// classifyPrimitiveType returns the canonical upper-case SQL type name
// from a ConvertedDataType parse node using typed ANTLR terminals.
func classifyPrimitiveType(cdt antlrgen.IConvertedDataTypeContext) string {
	pt := cdt.PrimitiveType()
	if pt == nil {
		return ""
	}
	p, ok := pt.(*antlrgen.PrimitiveTypeContext)
	if !ok {
		return ""
	}
	switch {
	case p.INTEGER() != nil:
		return "INTEGER"
	case p.BIGINT() != nil:
		return "BIGINT"
	case p.FLOAT() != nil:
		return "FLOAT"
	case p.DOUBLE() != nil:
		return "DOUBLE"
	case p.STRING() != nil:
		return "STRING"
	case p.BOOLEAN() != nil:
		return "BOOLEAN"
	case p.BYTES() != nil:
		return "BYTES"
	case p.UUID() != nil:
		return "UUID"
	case p.DATE() != nil:
		return "DATE"
	case p.TIMESTAMP() != nil:
		return "TIMESTAMP"
	}
	return ""
}

// resolveSelectListPosition maps a SQL-92 positional reference (e.g.
// `ORDER BY 2` or `GROUP BY 1`) to the matching output column name from
// the current SELECT list. `clause` is the SQL keyword used for the
// out-of-range error message ("ORDER BY" or "GROUP BY"). Accepts a
// positive integer literal (DecimalConstant wrapped in
// PredicatedExpression→ConstantExpressionAtom).
//
// Returns:
//   - (name, true, nil): positional reference resolved to an output column.
//   - ("", false, nil): the expression isn't a positional reference at all
//     (caller falls through to column / expression paths).
//   - ("", false, err): expression IS a positive integer literal but N is
//     out of range. Postgres / MySQL error on this instead of treating the
//     integer as a constant sort / group key, so we do the same.
func resolveSelectListPosition(clause string, expr antlrgen.IExpressionContext, projCols, projAliases []string, aggCols []aggSelectCol) (string, bool, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", false, nil
	}
	atom, ok := pred.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return "", false, nil
	}
	dec, ok := atom.Constant().(*antlrgen.DecimalConstantContext)
	if !ok {
		return "", false, nil
	}
	n, err := strconv.ParseInt(dec.DecimalLiteral().GetText(), 10, 64)
	if err != nil || n < 1 {
		return "", false, nil
	}
	listLen := len(projCols)
	if listLen == 0 {
		listLen = len(aggCols)
	}
	if int(n) > listLen {
		return "", false, api.NewErrorf(api.ErrCodeInvalidParameter,
			"%s position %d is out of range: SELECT list has %d entries", clause, n, listLen)
	}
	switch {
	case len(projCols) > 0:
		if int(n) <= len(projAliases) && projAliases[n-1] != "" {
			return projAliases[n-1], true, nil
		}
		return projCols[n-1], true, nil
	case len(aggCols) > 0:
		return aggCols[n-1].outName, true, nil
	}
	return "", false, nil
}
