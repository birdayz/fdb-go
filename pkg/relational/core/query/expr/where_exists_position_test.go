package expr_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
)

// parseWhere extracts the WHERE expression parse node from a single SELECT.
func parseWhere(t *testing.T, sql string) antlrgen.IExpressionContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	where := simple.FromClause().WhereExpr()
	if where == nil {
		t.Fatalf("no WHERE in %q", sql)
	}
	return where.Expression()
}

// parseProjectionItem extracts the parse node for the Nth SELECT-list item of a
// single SELECT — the parse-tree shape the projection-level EXISTS detectors see.
func parseProjectionItem(t *testing.T, sql string, n int) antlrgen.IExpressionContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	elems := simple.SelectElements().AllSelectElement()
	if n >= len(elems) {
		t.Fatalf("SELECT %q has %d elements, want index %d", sql, len(elems), n)
	}
	se, ok := elems[n].(*antlrgen.SelectExpressionElementContext)
	if !ok {
		t.Fatalf("SELECT element %d of %q is %T, not a plain expression", n, sql, elems[n])
	}
	return se.Expression()
}

// TestWhereExistsInScalarPosition pins the RFC-141 R4 round-12 WHERE backstop:
// a top-level boolean EXISTS term is directly-handled (false); an EXISTS buried
// in a scalar expression is reported (true).
func TestWhereExistsInScalarPosition(t *testing.T) {
	t.Parallel()
	const sub = "EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)"
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		// Directly-handled top-level boolean positions → NOT buried.
		{"top_level_exists", "SELECT id FROM t1 WHERE " + sub, false},
		{"top_level_not_exists", "SELECT id FROM t1 WHERE NOT " + sub, false},
		{"paren_exists", "SELECT id FROM t1 WHERE (" + sub + ")", false},
		{"paren_not_exists", "SELECT id FROM t1 WHERE NOT (" + sub + ")", false},
		{"exists_and_pred", "SELECT id FROM t1 WHERE " + sub + " AND id > 1", false},
		{"pred_and_not_exists", "SELECT id FROM t1 WHERE id > 1 AND NOT " + sub, false},
		{"exists_or_pred", "SELECT id FROM t1 WHERE " + sub + " OR id > 1", false},
		{"no_exists", "SELECT id FROM t1 WHERE id > 1 AND col1 < 5", false},
		// Buried in a scalar expression → reported.
		{"case_when_exists", "SELECT id FROM t1 WHERE CASE WHEN " + sub + " THEN 1 ELSE 0 END = 1", true},
		{"paren_exists_eq_true", "SELECT id FROM t1 WHERE (" + sub + ") = true", true},
		{"exists_under_arith_cmp", "SELECT id FROM t1 WHERE CASE WHEN " + sub + " THEN 1 ELSE 0 END + 1 > 1", true},
		{"buried_under_and", "SELECT id FROM t1 WHERE id > 0 AND (CASE WHEN " + sub + " THEN 1 ELSE 0 END = 1)", true},
		// RFC-141 R4 round-13: an EXISTS belonging to a NESTED subquery's OWN clause
		// is that subquery's concern, classified in its own translation context —
		// it must NOT be mis-attributed to the OUTER WHERE and reported as buried.
		// Before the boundary stop these were falsely reported true.
		{
			"nested_scalar_subquery_where_exists",
			"SELECT id FROM t1 WHERE (SELECT MAX(id) FROM t2 WHERE " + sub + ") > 5", false,
		},
		{
			"nested_scalar_subquery_buried_case_exists",
			"SELECT id FROM t1 WHERE (SELECT MAX(id) FROM t2 WHERE CASE WHEN " + sub + " THEN 1 ELSE 0 END = 1) > 5", false,
		},
		{
			"nested_in_subquery_where_exists",
			"SELECT id FROM t1 WHERE id IN (SELECT id FROM t2 WHERE " + sub + ")", false,
		},
		// A real outer-level buried EXISTS ALONGSIDE a nested-subquery EXISTS: the
		// outer CASE-EXISTS is still buried (true); the nested one is ignored.
		{
			"outer_buried_plus_nested_subquery",
			"SELECT id FROM t1 WHERE CASE WHEN " + sub + " THEN 1 ELSE 0 END = 1 " +
				"AND (SELECT MAX(id) FROM t2 WHERE " + sub + ") > 5", true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			where := parseWhere(t, c.sql)
			if got := expr.WhereExistsInScalarPosition(where); got != c.want {
				t.Fatalf("WhereExistsInScalarPosition(%q) = %v, want %v", c.sql, got, c.want)
			}
		})
	}
}

// TestContainsExistsAtom_SubqueryBoundary pins the RFC-141 R4 round-13 boundary
// stop: ContainsExistsAtom matches an EXISTS atom at the CURRENT query level but
// does NOT descend into a nested scalar / IN subquery — an EXISTS that is the
// nested subquery's own concern must not be attributed to the outer expression.
// This is the detector that gated the round-13 over-rejection
// (`SELECT id, (SELECT MAX(id) FROM t2 WHERE EXISTS(...)) FROM t1`).
func TestContainsExistsAtom_SubqueryBoundary(t *testing.T) {
	t.Parallel()
	const sub = "EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t2.id)"
	cases := []struct {
		name string
		sql  string
		// projItem selects which SELECT-list item the detector inspects.
		projItem int
		want     bool
	}{
		// The round-13 regression: a scalar subquery whose OWN WHERE has an EXISTS,
		// used as a projection item — the OUTER projection's detector must see NO
		// EXISTS at its level (the EXISTS belongs to the scalar subquery).
		{
			"scalar_subquery_where_exists_proj",
			"SELECT id, (SELECT MAX(id) FROM t2 WHERE " + sub + ") FROM t1", 1, false,
		},
		// A scalar subquery with a CASE-buried EXISTS in its WHERE — still no EXISTS
		// at the outer projection's level.
		{
			"scalar_subquery_buried_case_exists_proj",
			"SELECT id, (SELECT MAX(id) FROM t2 WHERE CASE WHEN " + sub + " THEN 1 ELSE 0 END = 1) FROM t1", 1, false,
		},
		// A bare projected EXISTS at the CURRENT level IS matched (true) — the
		// detector still finds the outer-level atom.
		{
			"bare_projected_exists",
			"SELECT id, EXISTS (SELECT 1 FROM t2) FROM t1", 1, true,
		},
		// A CASE at the outer level with an EXISTS at the outer level IS matched.
		{
			"outer_case_exists",
			"SELECT id, CASE WHEN EXISTS (SELECT 1 FROM t2) THEN 1 ELSE 0 END FROM t1", 1, true,
		},
		// An IN-subquery projection-position scalar whose subquery WHERE has an
		// EXISTS — boundary stop (no outer-level EXISTS). (`id IN (subquery)` as a
		// projected boolean.)
		{
			"in_subquery_where_exists_proj",
			"SELECT id, id IN (SELECT id FROM t2 WHERE " + sub + ") FROM t1", 1, false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			item := parseProjectionItem(t, c.sql, c.projItem)
			if got := expr.ContainsExistsAtom(item); got != c.want {
				t.Fatalf("ContainsExistsAtom(item %d of %q) = %v, want %v", c.projItem, c.sql, got, c.want)
			}
		})
	}
}
