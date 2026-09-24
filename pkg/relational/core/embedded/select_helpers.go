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

// resolveSelectListPosition maps a SQL-92 positional reference (e.g.
// `ORDER BY 2` or `GROUP BY 1`) to the matching output column name from
// the current SELECT list. `clause` is the SQL keyword used for the
// out-of-range error message ("ORDER BY" or "GROUP BY"). Accepts a
// positive integer literal (DecimalConstant wrapped in
// PredicatedExpression→ConstantExpressionAtom).
//
// Returns:
//   - (name, n, true, nil): positional reference resolved to an output column;
//     n is the 1-based SELECT-list position (the output ordinal + 1).
//   - ("", 0, false, nil): the expression isn't a positional reference at all
//     (caller falls through to column / expression paths).
//   - ("", 0, false, err): expression IS a positive integer literal but N is
//     out of range. Postgres / MySQL error on this instead of treating the
//     integer as a constant sort / group key, so we do the same.
func resolveSelectListPosition(clause string, expr antlrgen.IExpressionContext, projCols, projAliases []string, aggCols []aggSelectCol, countStar bool) (string, int, bool, error) {
	n, positional := selectListPosition(expr)
	if !positional {
		return "", 0, false, nil
	}
	listLen := len(projCols)
	orderedAggCols := aggregateColumnsInSelectOrder(aggCols)
	if listLen == 0 {
		listLen = len(orderedAggCols)
	}
	if listLen == 0 && countStar {
		listLen = 1
	}
	if int(n) > listLen {
		return "", 0, false, api.NewErrorf(api.ErrCodeInvalidParameter,
			"%s position %d is out of range: SELECT list has %d entries", clause, n, listLen)
	}
	switch {
	case len(projCols) > 0:
		if int(n) <= len(projAliases) && projAliases[n-1] != "" {
			return projAliases[n-1], int(n), true, nil
		}
		return projCols[n-1], int(n), true, nil
	case len(orderedAggCols) > 0:
		ac := aggCols[orderedAggCols[n-1]]
		name, alias, _ := aggregateProjectionItem(ac, func(s string) string { return s })
		if alias != "" {
			name = alias
		}
		return name, int(n), true, nil
	case countStar:
		return "COUNT(*)", int(n), true, nil
	}
	return "", 0, false, nil
}

// selectListPosition recognizes the positive integer literal used by Go's
// positional GROUP BY and ORDER BY extensions, without consulting list width.
func selectListPosition(expr antlrgen.IExpressionContext) (int64, bool) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return 0, false
	}
	atom, ok := pred.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return 0, false
	}
	dec, ok := atom.Constant().(*antlrgen.DecimalConstantContext)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(dec.DecimalLiteral().GetText(), 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func hasPositionalOrderBy(simpleTable *antlrgen.SimpleTableContext) bool {
	if simpleTable.OrderByClause() == nil {
		return false
	}
	for _, order := range simpleTable.OrderByClause().AllOrderByExpression() {
		if _, ok := selectListPosition(order.Expression()); ok {
			return true
		}
	}
	return false
}
