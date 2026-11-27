// Portions derived from FoundationDB Record Layer (Type.java,
// SemanticAnalyzer.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rowdiff

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

// existsSQL renders the correlated [NOT] EXISTS clause. tbl is the outer table
// name (also the inner table); the inner aliases it `r` and correlates against
// the unqualified outer `tbl.<col>`.
func existsSQL(e *ExistsSpec, tbl string) string {
	var b strings.Builder
	if e.Negated {
		b.WriteString("NOT ")
	}
	fmt.Fprintf(&b, "EXISTS (SELECT 1 FROM %s AS r WHERE r.%s %s %s.%s",
		tbl, strings.ToLower(e.CorrCol), opSQL(e.CorrOp), tbl, strings.ToLower(e.CorrCol))
	if e.Inner != nil {
		fmt.Fprintf(&b, " AND r.%s %s %s",
			strings.ToLower(e.Inner.Col), opSQL(e.Inner.Op), renderLiteral(e.Inner.Lit))
	}
	b.WriteString(")")
	return b.String()
}

// scalarSubSQL renders `<outerCol> <op> (SELECT <agg> FROM tbl [WHERE <filter>])`.
func scalarSubSQL(s *ScalarSubSpec, tbl string) string {
	inner := &AggSpec{Func: s.Func, Col: s.Col}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (SELECT %s FROM %s",
		strings.ToLower(s.OuterCol), opSQL(s.Op), aggExprSQL(inner), tbl)
	if s.Filter != nil {
		fmt.Fprintf(&b, " WHERE %s %s %s",
			strings.ToLower(s.Filter.Col), opSQL(s.Filter.Op), renderLiteral(s.Filter.Lit))
	}
	b.WriteString(")")
	return b.String()
}

// SQL renders one query under a projection (nil = SELECT *).
func (c *Case) SQL(q Query, projection []string) string {
	if q.Agg != nil {
		return c.aggSQL(q)
	}
	if q.Union != nil {
		return c.unionSQL(q, projection)
	}
	if q.Derived != nil {
		return c.derivedSQL(q, projection)
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	if q.Distinct {
		b.WriteString("DISTINCT ")
	}
	appendSelectProjection(&b, q, projection)
	b.WriteString(" FROM ")
	tbl := strings.ToLower(c.Table.Name)
	appendFromClause(&b, tbl, q)
	appendWhereClause(&b, tbl, q)
	appendOrderLimitClause(&b, q)
	return b.String()
}

func appendSelectProjection(b *strings.Builder, q Query, projection []string) {
	if projection == nil {
		b.WriteString("*")
	} else if q.Join != nil || q.ThreeWay != nil {
		// Qualified columns, each with a unique alias (l_id, r_a, …) so no
		// two output columns share a name.
		outs := make([]string, len(projection))
		for i, p := range projection {
			outs[i] = fmt.Sprintf("%s AS %s", strings.ToLower(p), joinOutputAlias(p))
		}
		b.WriteString(strings.Join(outs, ", "))
	} else {
		outs := make([]string, len(projection))
		for i, p := range projection {
			outs[i] = nestedProjectionSQL(p)
		}
		b.WriteString(strings.Join(outs, ", "))
	}
}

func appendFromClause(b *strings.Builder, tbl string, q Query) {
	switch {
	case q.ThreeWay != nil:
		s := q.ThreeWay
		if s.Comma {
			fmt.Fprintf(b, "%s AS l, %s AS m, %s AS r", tbl, tbl, tbl)
		} else {
			rJoin := "JOIN"
			if s.ROuter {
				rJoin = "LEFT JOIN"
			}
			fmt.Fprintf(b, "%s AS l JOIN %s AS m ON l.%s = m.%s %s %s AS r ON m.%s = r.%s",
				tbl, tbl, strings.ToLower(s.LMLeft), strings.ToLower(s.LMRight),
				rJoin, tbl, strings.ToLower(s.MRMid), strings.ToLower(s.MRRight))
		}
	case q.Join == nil:
		b.WriteString(tbl)
	case q.Join.LeftOuter:
		fmt.Fprintf(b, "%s AS l LEFT JOIN %s AS r ON l.%s = r.%s",
			tbl, tbl, strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	case q.Join.RightOuter:
		fmt.Fprintf(b, "%s AS l RIGHT JOIN %s AS r ON l.%s = r.%s",
			tbl, tbl, strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	case q.Join.Inner:
		fmt.Fprintf(b, "%s AS l JOIN %s AS r ON l.%s = r.%s",
			tbl, tbl, strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	default:
		// Comma cross-join; the equality moves into the WHERE below.
		fmt.Fprintf(b, "%s AS l, %s AS r", tbl, tbl)
	}
}

func appendWhereClause(b *strings.Builder, tbl string, q Query) {
	// WHERE: the comma-join form carries the join equality as its first
	// conjunct, ahead of any generated filters.
	joinEq := ""
	if q.Join != nil && !q.Join.Inner {
		joinEq = fmt.Sprintf("l.%s = r.%s",
			strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	}
	if q.ThreeWay != nil && q.ThreeWay.Comma {
		s := q.ThreeWay
		joinEq = fmt.Sprintf("l.%s = m.%s AND m.%s = r.%s",
			strings.ToLower(s.LMLeft), strings.ToLower(s.LMRight),
			strings.ToLower(s.MRMid), strings.ToLower(s.MRRight))
	}
	// Build the WHERE as a list of top-level AND conjuncts: the join equality
	// (comma-join queries), the boolean predicate tree, and the appended
	// subquery filters (EXISTS, scalar comparison). Each appended subquery is a
	// separate top-level conjunct in the oracle, so when one follows the
	// predicate tree the tree is parenthesized — otherwise a top-level OR in it
	// would capture the trailing `AND <subquery>` (AND binds tighter than OR),
	// a rendering-only divergence.
	{
		var conj []string
		if joinEq != "" {
			conj = append(conj, joinEq)
		}
		switch {
		case q.WhereOverride != nil:
			// The override replaces ONLY the predicate-tree conjunct. The join
			// equality and the appended subquery filters stay, so a TLP triple
			// built over a comma join or an EXISTS query still partitions: those
			// conjuncts are identical in all four renderings, so they factor out
			// of the partition property exactly as a FROM clause does.
			if *q.WhereOverride != "" {
				conj = append(conj, *q.WhereOverride)
			}
		case q.Where != nil:
			var wb strings.Builder
			renderBool(&wb, q.Where)
			w := wb.String()
			if q.Exists != nil || q.ScalarSub != nil {
				w = "(" + w + ")"
			}
			conj = append(conj, w)
		}
		if q.Exists != nil {
			conj = append(conj, existsSQL(q.Exists, tbl))
		}
		if q.ScalarSub != nil {
			conj = append(conj, scalarSubSQL(q.ScalarSub, tbl))
		}
		if len(conj) > 0 {
			b.WriteString(" WHERE ")
			b.WriteString(strings.Join(conj, " AND "))
		}
	}
}

func appendOrderLimitClause(b *strings.Builder, q Query) {
	if len(q.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, k := range q.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			if k.Qual != "" {
				b.WriteString(strings.ToLower(k.Qual) + ".")
			}
			b.WriteString(strings.ToLower(k.Col))
			if k.Desc {
				b.WriteString(" DESC")
			}
			switch k.Nulls {
			case NullsFirst:
				b.WriteString(" NULLS FIRST")
			case NullsLast:
				b.WriteString(" NULLS LAST")
			}
		}
	}
	if q.Limit > 0 {
		fmt.Fprintf(b, " LIMIT %d", q.Limit)
		if q.Offset > 0 {
			fmt.Fprintf(b, " OFFSET %d", q.Offset)
		}
	}
}

// aggExprSQL renders the aggregate call.
func aggExprSQL(a *AggSpec) string {
	col := strings.ToLower(a.Col)
	switch a.Func {
	case AggCountStar:
		return "COUNT(*)"
	case AggCountCol:
		return "COUNT(" + col + ")"
	case AggSum:
		return "SUM(" + col + ")"
	case AggMin:
		return "MIN(" + col + ")"
	default:
		return "MAX(" + col + ")"
	}
}

// unionSQL renders `SELECT <proj> FROM t WHERE <l> UNION [ALL] SELECT <proj>
// FROM t WHERE <r>`. Both branches carry the SAME projection so their output
// columns align.
func (c *Case) unionSQL(q Query, projection []string) string {
	u := q.Union
	tbl := strings.ToLower(c.Table.Name)
	branch := func(where *BoolNode) string {
		var b strings.Builder
		b.WriteString("SELECT ")
		if projection == nil {
			b.WriteString("*")
		} else {
			outs := make([]string, len(projection))
			for i, p := range projection {
				outs[i] = nestedProjectionSQL(p)
			}
			b.WriteString(strings.Join(outs, ", "))
		}
		fmt.Fprintf(&b, " FROM %s", tbl)
		if where != nil {
			b.WriteString(" WHERE ")
			renderBool(&b, where)
		}
		return b.String()
	}
	op := "UNION"
	if u.All {
		op = "UNION ALL"
	}
	return branch(u.Left) + " " + op + " " + branch(u.Right)
}

// derivedSQL renders `SELECT <proj> FROM (SELECT * FROM t WHERE <inner>) d
// [WHERE <outer>] [ORDER BY id]`. The outer references derived columns
// unqualified (d is the only source).
func (c *Case) derivedSQL(q Query, projection []string) string {
	d := q.Derived
	tbl := strings.ToLower(c.Table.Name)
	// The subquery body: `SELECT * FROM t [WHERE inner]`.
	var sub strings.Builder
	fmt.Fprintf(&sub, "SELECT * FROM %s", tbl)
	if d.Inner != nil {
		sub.WriteString(" WHERE ")
		renderBool(&sub, d.Inner)
	}

	var b strings.Builder
	if d.Cte {
		fmt.Fprintf(&b, "WITH d AS (%s) ", sub.String())
	}
	b.WriteString("SELECT ")
	if q.Distinct {
		b.WriteString("DISTINCT ")
	}
	if projection == nil {
		b.WriteString("*")
	} else {
		outs := make([]string, len(projection))
		for i, p := range projection {
			outs[i] = nestedProjectionSQL(p)
		}
		b.WriteString(strings.Join(outs, ", "))
	}
	if d.Cte {
		b.WriteString(" FROM d")
	} else {
		fmt.Fprintf(&b, " FROM (%s) d", sub.String())
	}
	if d.Outer != nil {
		b.WriteString(" WHERE ")
		renderBool(&b, d.Outer)
	}
	if len(q.OrderBy) > 0 {
		b.WriteString(" ORDER BY id")
	}
	return b.String()
}

// aggSQL renders an aggregate query. Both output columns are aliased so the
// harness's name-keyed rows have stable, unique keys regardless of how the
// engine spells a computed column.
func (c *Case) aggSQL(q Query) string {
	a := q.Agg
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, key := range a.GroupBy {
		// Alias each key to its output column (g, g1, g2, …) so the harness's
		// name-keyed rows line up with aggOutputCols / the oracle.
		fmt.Fprintf(&b, "%s AS %s, ", strings.ToLower(key), strings.ToLower(groupKeyCol(i)))
	}
	fmt.Fprintf(&b, "%s AS agg FROM %s", aggExprSQL(a), strings.ToLower(c.Table.Name))
	if q.Where != nil {
		b.WriteString(" WHERE ")
		renderBool(&b, q.Where)
	}
	if len(a.GroupBy) > 0 {
		lowered := make([]string, len(a.GroupBy))
		for i, key := range a.GroupBy {
			lowered[i] = strings.ToLower(key)
		}
		fmt.Fprintf(&b, " GROUP BY %s", strings.Join(lowered, ", "))
		if a.HavingOn && a.Having != nil {
			fmt.Fprintf(&b, " HAVING %s %s %s", aggExprSQL(a), opSQL(a.Having.Op), renderLiteral(a.Having.Lit))
		}
	}
	return b.String()
}

// joinOutputAlias turns a qualified column ("L.ID") into a unique output
// alias ("l_id").
func joinOutputAlias(qualified string) string {
	return strings.ToLower(strings.ReplaceAll(qualified, ".", "_"))
}

// PredicateSQL renders a boolean predicate tree as a SQL expression, the same
// rendering `Case.SQL` puts in the WHERE clause. Exported so the RFC-201 §5.5
// ternary-logic-partitioning oracle can build the three branches of the
// partition — `p`, `NOT (p)`, `(p) IS NULL` — from the one tree that produced
// the query under test, rather than re-deriving the predicate from SQL text.
// A nil tree renders as the empty string.
func PredicateSQL(n *BoolNode) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	renderBool(&b, n)
	return b.String()
}

func renderBool(b *strings.Builder, n *BoolNode) {
	if n.Not {
		b.WriteString("NOT (")
		defer b.WriteString(")")
	}
	if n.Leaf != nil {
		p := n.Leaf
		col := strings.ToLower(p.Col)
		if p.Qual != "" {
			col = strings.ToLower(p.Qual) + "." + col
		}
		// NOT IN renders inline; every other negated leaf wraps in NOT (…).
		negWrap := p.Negated && p.Op != predicates.ComparisonIn
		if negWrap {
			b.WriteString("NOT (")
		}
		switch {
		case p.Cast != nil:
			fmt.Fprintf(b, "CAST(%s AS STRING) %s %s",
				strings.ToLower(p.Cast.Col), opSQL(p.Op), renderLiteral(p.Lit))
		case p.NumFn != nil:
			col := strings.ToLower(p.NumFn.Col)
			switch p.NumFn.Fn {
			case NumFnMod:
				fmt.Fprintf(b, "MOD(%s, %d) %s %s", col, p.NumFn.Mod, opSQL(p.Op), renderLiteral(p.Lit))
			case NumFnCoalesce:
				fmt.Fprintf(b, "COALESCE(%s, %d) %s %s", col, p.NumFn.Default, opSQL(p.Op), renderLiteral(p.Lit))
			default:
				fmt.Fprintf(b, "ABS(%s) %s %s", col, opSQL(p.Op), renderLiteral(p.Lit))
			}
		case p.StrFn != nil:
			switch p.StrFn.Fn {
			case StrFnSubstr:
				fmt.Fprintf(b, "SUBSTR(%s, %d, %d) %s %s",
					strings.ToLower(p.StrFn.Col), p.StrFn.Start, p.StrFn.Length, opSQL(p.Op), renderLiteral(p.Lit))
			case StrFnConcat:
				fmt.Fprintf(b, "CONCAT(%s, %s) %s %s",
					strings.ToLower(p.StrFn.Col), renderLiteral(p.StrFn.Suffix), opSQL(p.Op), renderLiteral(p.Lit))
			default:
				fmt.Fprintf(b, "%s(%s) %s %s",
					strFnSQL(p.StrFn.Fn), strings.ToLower(p.StrFn.Col), opSQL(p.Op), renderLiteral(p.Lit))
			}
		case p.Case != nil:
			cs := p.Case
			fmt.Fprintf(b, "(CASE WHEN %s %s %s THEN %s ELSE %s END) %s %s",
				strings.ToLower(cs.When.Col), opSQL(cs.When.Op), renderLiteral(cs.When.Lit),
				caseArmSQL(cs.ThenCol, cs.ThenLit), caseArmSQL(cs.ElseCol, cs.ElseLit),
				opSQL(p.Op), renderLiteral(p.Lit))
		case p.HasArith:
			col2 := strings.ToLower(p.ArithCol2)
			if p.Qual != "" {
				col2 = strings.ToLower(p.Qual) + "." + col2
			}
			fmt.Fprintf(b, "(%s %s %s) %s %s", col, arithSQL(p.ArithOp), col2, opSQL(p.Op), renderLiteral(p.Lit))
		case p.Bitwise:
			col2 := strings.ToLower(p.BitCol2)
			if p.Qual != "" {
				col2 = strings.ToLower(p.Qual) + "." + col2
			}
			fmt.Fprintf(b, "(%s %s %s) %s %s", col, bitSQL(p.BitOp), col2, opSQL(p.Op), renderLiteral(p.Lit))
		case p.IsBetween:
			fmt.Fprintf(b, "%s BETWEEN %s AND %s", col, renderLiteral(p.Lit), renderLiteral(p.BetweenHi))
		case p.Op == predicates.ComparisonIsNull:
			fmt.Fprintf(b, "%s IS NULL", col)
		case p.Op == predicates.ComparisonIsNotNull:
			fmt.Fprintf(b, "%s IS NOT NULL", col)
		case p.Op == predicates.ComparisonIn:
			lits := make([]string, len(p.InList))
			for i, v := range p.InList {
				lits[i] = renderLiteral(v)
			}
			kw := "IN"
			if p.Negated {
				kw = "NOT IN"
			}
			fmt.Fprintf(b, "%s %s (%s)", col, kw, strings.Join(lits, ", "))
		case p.Op == predicates.ComparisonLike:
			fmt.Fprintf(b, "%s LIKE %s", col, renderLiteral(p.Lit))
		case p.RhsCol != "":
			rhs := strings.ToLower(p.RhsCol)
			if p.RhsQual != "" {
				rhs = strings.ToLower(p.RhsQual) + "." + rhs
			}
			fmt.Fprintf(b, "%s %s %s", col, opSQL(p.Op), rhs)
		default:
			fmt.Fprintf(b, "%s %s %s", col, opSQL(p.Op), renderLiteral(p.Lit))
		}
		if negWrap {
			b.WriteString(")")
		}
		return
	}
	sep := " AND "
	if !n.And {
		sep = " OR "
	}
	for i, kid := range n.Kids {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString("(")
		renderBool(b, kid)
		b.WriteString(")")
	}
}

func opSQL(op predicates.ComparisonType) string {
	switch op {
	case predicates.ComparisonEquals:
		return "="
	case predicates.ComparisonNotEquals:
		return "<>"
	case predicates.ComparisonLessThan:
		return "<"
	case predicates.ComparisonLessThanOrEq:
		return "<="
	case predicates.ComparisonGreaterThan:
		return ">"
	case predicates.ComparisonGreaterThanEq:
		return ">="
	}
	panic(fmt.Sprintf("rowdiff: unrenderable comparison op %d", op))
}

// caseArmSQL renders a CASE arm: the column when set, else the literal.
func caseArmSQL(col string, lit int64) string {
	if col != "" {
		return strings.ToLower(col)
	}
	return renderLiteral(lit)
}

// strFnSQL renders a string function's SQL name.
func strFnSQL(fn StrFnKind) string {
	switch fn {
	case StrFnUpper:
		return "UPPER"
	case StrFnLower:
		return "LOWER"
	case StrFnLength:
		return "LENGTH"
	case StrFnTrim:
		return "TRIM"
	}
	panic(fmt.Sprintf("rowdiff: unrenderable string fn %d", fn))
}

// arithSQL renders an arithmetic operator. Only OpSub is generated today; the
// others are handled so a future extension (with overflow handling) is a
// one-line generator change, not a renderer change.
func arithSQL(op values.ArithmeticOp) string {
	switch op {
	case values.OpAdd:
		return "+"
	case values.OpSub:
		return "-"
	case values.OpMul:
		return "*"
	}
	panic(fmt.Sprintf("rowdiff: unrenderable arith op %d", op))
}

// bitSQL renders a bitwise op name (BITAND/BITOR/BITXOR) as its SQL infix
// operator (&/|/^ — the RelationalParser bitOperator forms).
func bitSQL(op string) string {
	switch op {
	case "BITAND":
		return "&"
	case "BITOR":
		return "|"
	case "BITXOR":
		return "^"
	}
	panic(fmt.Sprintf("rowdiff: unrenderable bit op %q", op))
}

// renderColumnLiteral renders a value written into a column of type t. A
// FLOAT column takes a FLOAT, and the only literal a float renders to is a
// DOUBLE, which no promotion takes to FLOAT (PromoteValue has no
// DOUBLE_TO_FLOAT; both engines refuse it with 22000), so a FLOAT value is
// written through CAST(… AS FLOAT). The generated value is already a float32
// widened (genRows), so the narrowing cast is exact.
func renderColumnLiteral(t ColType, v any) string {
	if t == ColFloat && v != nil {
		return "CAST(" + renderLiteral(v) + " AS FLOAT)"
	}
	return renderLiteral(v)
}

func renderLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("%d", t)
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case float64:
		return renderFloatLiteral(t)
	}
	panic(fmt.Sprintf("rowdiff: unrenderable literal %T", v))
}

// renderFloatLiteral formats v as a SQL REAL_LITERAL. The grammar
// (RelationalLexer.g4) requires a decimal point or an exponent — a bare
// integral value like "9007199254740992" lexes as a DECIMAL_LITERAL (a
// different literal TYPE, BIGINT not DOUBLE/FLOAT), so an integral float
// value needs an explicit ".0" appended. strconv's shortest round-trip
// format ('f', -1, 64) is used so the literal reparses to the EXACT same
// float64 bit pattern generated (load-bearing for the float32 width-
// narrowing boundary values, whose exact decimal expansion has many
// significant digits — e.g. float64(float32(0.1))). The sign is handled by
// FormatFloat itself, including -0.0 (renders "-0", then ".0" is appended),
// which already round-trips through this codebase's SQL surface (see
// plandiff/seedcorpus/corpus.go's "double_negative_zero" cases).
func renderFloatLiteral(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		panic(fmt.Sprintf("rowdiff: NaN/Infinity has no SQL literal spelling (%v) — see the ColType doc comment", v))
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
