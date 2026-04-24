package embedded

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// parseSelect is a test helper that parses SQL and returns the
// parsed selectQuery for the first SELECT statement.
func parseSelect(t *testing.T, sql string) *selectQuery {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	if sel == nil {
		t.Fatalf("not a SELECT statement: %q", sql)
	}
	sq, err := extractSelectParts(sel)
	if err != nil {
		t.Fatalf("extractSelectParts %q: %v", sql, err)
	}
	return sq
}

func TestBuildLogicalPlan_SimpleSelectStar(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM t")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	if got, want := op.Explain(""), "Scan(t)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_SelectStarWhere(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM t WHERE id > 5")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	want := "Filter(id>5)\n  Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_SelectColsOrderLimit(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT id, name FROM t ORDER BY id DESC LIMIT 10")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	// Project at top (projCols set), then Limit, then Sort, then Scan.
	want := "Project(id, name)\n  Limit(10)\n    Sort(id DESC)\n      Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Bare COUNT(*): single-aggregate no-group-by.
func TestBuildLogicalPlan_CountStar(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT COUNT(*) FROM t")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Aggregate(group=[], agg=[COUNT(*)])\n  Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// GROUP BY with aggregate.
func TestBuildLogicalPlan_GroupBySum(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT dept, SUM(v) FROM t GROUP BY dept")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	// Project wraps the aggregate; keys list reflects the GROUP BY.
	if got := op.Explain(""); !strings.Contains(got, "Aggregate(group=[dept], agg=[SUM(v)") {
		t.Fatalf("got %q, want Aggregate(group=[dept], agg=[SUM(v)...])", got)
	}
}

// HAVING clause renders on the aggregate node.
func TestBuildLogicalPlan_GroupByHaving(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT dept FROM t GROUP BY dept HAVING SUM(v) > 100")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	if got := op.Explain(""); !strings.Contains(got, "having=") {
		t.Fatalf("got %q, want having=... in aggregate node", got)
	}
}

// INNER JOIN — builder emits a LogicalJoin.
func TestBuildLogicalPlan_InnerJoin(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT a.id FROM a INNER JOIN b ON a.id = b.a_id")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Project(a.id)\n" +
		"  InnerJoin(on a.id=b.a_id)\n" +
		"    Scan(a)\n" +
		"    Scan(b)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// LEFT JOIN variant.
func TestBuildLogicalPlan_LeftJoin(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM a LEFT JOIN b ON a.id = b.a_id")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "LeftJoin(on a.id=b.a_id)\n" +
		"  Scan(a)\n" +
		"  Scan(b)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// RIGHT JOIN variant.
func TestBuildLogicalPlan_RightJoin(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM a RIGHT JOIN b ON a.id = b.a_id")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "RightJoin(on a.id=b.a_id)\n" +
		"  Scan(a)\n" +
		"  Scan(b)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Multiple chained joins — two-level nesting.
func TestBuildLogicalPlan_ChainedJoins(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM a INNER JOIN b ON a.id = b.a_id INNER JOIN c ON b.id = c.b_id")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	// Left-nested: ((a JOIN b) JOIN c)
	want := "InnerJoin(on b.id=c.b_id)\n" +
		"  InnerJoin(on a.id=b.a_id)\n" +
		"    Scan(a)\n" +
		"    Scan(b)\n" +
		"  Scan(c)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// SELECT without FROM → LogicalValues.
func TestBuildLogicalPlan_ValuesNoFrom(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT 1 + 2")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	got := op.Explain("")
	if !strings.HasPrefix(got, "Values(") {
		t.Fatalf("got %q, want Values(...)", got)
	}
}

// WITH (CTE) — builder wraps the main query in a LogicalCTE.
func TestBuildLogicalPlan_CTE(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("WITH active_users AS (SELECT id FROM users WHERE active = TRUE) SELECT id FROM active_users")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQuery(sel.Query())
	if op == nil {
		t.Fatal("expected non-nil")
	}
	got := op.Explain("")
	for _, want := range []string{"CTE(active_users)", "Filter(active=TRUE)", "Scan(users)", "Scan(active_users)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

// WITH RECURSIVE — tag flips to RecursiveCTE.
func TestBuildLogicalPlan_RecursiveCTE(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("WITH RECURSIVE tree AS (SELECT id FROM base UNION ALL SELECT id FROM tree WHERE id > 0) SELECT id FROM tree")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQuery(sel.Query())
	if op == nil {
		t.Fatal("expected non-nil")
	}
	if !strings.Contains(op.Explain(""), "RecursiveCTE(tree)") {
		t.Fatalf("expected RecursiveCTE, got %q", op.Explain(""))
	}
}

// Multi-CTE WITH — outermost CTE sits at the root, innermost nests
// deepest in Main.
func TestBuildLogicalPlan_MultiCTE(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("WITH a AS (SELECT id FROM x), b AS (SELECT id FROM y) SELECT id FROM b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQuery(sel.Query())
	if op == nil {
		t.Fatal("expected non-nil")
	}
	// First CTE (`a`) is the outer wrap; second CTE (`b`) nests inside.
	got := op.Explain("")
	if strings.Index(got, "CTE(a)") > strings.Index(got, "CTE(b)") {
		t.Fatalf("expected CTE(a) before CTE(b), got %q", got)
	}
}

// UNION ALL: two SELECTs, quantifier ALL.
func TestBuildLogicalPlan_UnionAll(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM a UNION ALL SELECT id FROM b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQueryBody(sel.Query().QueryExpressionBody())
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "UnionAll\n  Project(id)\n    Scan(a)\n  Project(id)\n    Scan(b)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// UNION (implicit DISTINCT): no quantifier.
func TestBuildLogicalPlan_UnionDistinct(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM a UNION SELECT id FROM b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQueryBody(sel.Query().QueryExpressionBody())
	if op == nil {
		t.Fatal("expected non-nil")
	}
	if !strings.Contains(op.Explain(""), "UnionDistinct") {
		t.Fatalf("expected UnionDistinct, got %q", op.Explain(""))
	}
}

// Three-way UNION: grammar left-associates as
// SetQuery(SetQuery(A, B), C); builder flattens matching-quantifier
// nested unions into a single UnionAll with 3 inputs.
func TestBuildLogicalPlan_UnionThreeWay(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM a UNION ALL SELECT id FROM b UNION ALL SELECT id FROM c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQueryBody(sel.Query().QueryExpressionBody())
	if op == nil {
		t.Fatal("expected non-nil")
	}
	got := op.Explain("")
	// Flattened: should have 3 Scan leaves under a single UnionAll.
	if strings.Count(got, "Scan(") != 3 {
		t.Fatalf("expected 3 Scans, got %q", got)
	}
	if strings.Count(got, "UnionAll") != 1 {
		t.Fatalf("expected 1 UnionAll (flattened), got %q", got)
	}
}

// Derived table: FROM (SELECT ...) AS alias — builder recurses and
// the inner plan surfaces as-is.
func TestBuildLogicalPlan_DerivedTable(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT x FROM (SELECT id AS x FROM t WHERE id > 5) AS sub")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil")
	}
	// Outer Project wraps the inner plan (which is Project on Filter
	// on Scan). Seed: LogicalDerived doesn't exist yet; inner tree
	// surfaces directly.
	if got := op.Explain(""); !strings.Contains(got, "Scan(t)") || !strings.Contains(got, "Filter(id>5)") {
		t.Fatalf("got %q, expected inner plan to contain Scan(t) and Filter(id>5)", got)
	}
}

// Aliases carry through to the Scan node.
func TestBuildLogicalPlan_AliasedTable(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM t AS tbl")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	want := "Scan(t AS tbl)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Nil input: builder returns nil.
func TestBuildLogicalPlan_Nil(t *testing.T) {
	t.Parallel()
	if op := buildLogicalPlanForSelect(nil); op != nil {
		t.Fatalf("expected nil for nil input, got %T", op)
	}
	if op := buildLogicalPlanForDelete(nil); op != nil {
		t.Fatalf("expected nil for nil delete, got %T", op)
	}
	if op := buildLogicalPlanForUpdate(nil); op != nil {
		t.Fatalf("expected nil for nil update, got %T", op)
	}
	if op := buildLogicalPlanForInsert(nil); op != nil {
		t.Fatalf("expected nil for nil insert, got %T", op)
	}
}

// parseDelete returns the parsed DeleteStatementContext.
func parseDelete(t *testing.T, sql string) antlrgen.IDeleteStatementContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	stmt := root.Statements().AllStatement()[0]
	dml := stmt.DmlStatement()
	if dml == nil {
		t.Fatalf("not a DML statement: %q", sql)
	}
	return dml.DeleteStatement()
}

// parseUpdate returns the parsed UpdateStatementContext.
func parseUpdate(t *testing.T, sql string) antlrgen.IUpdateStatementContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	stmt := root.Statements().AllStatement()[0]
	dml := stmt.DmlStatement()
	if dml == nil {
		t.Fatalf("not a DML statement: %q", sql)
	}
	return dml.UpdateStatement()
}

func TestBuildLogicalPlan_Delete(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForDelete(parseDelete(t, "DELETE FROM t WHERE id > 5"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Delete(t)\n  Filter(id>5)\n    Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_DeleteNoWhere(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForDelete(parseDelete(t, "DELETE FROM t"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Delete(t)\n  Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_Update(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForUpdate(parseUpdate(t, "UPDATE t SET v = v + 1 WHERE id = 5"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Update(t SET v=v+1)\n  Filter(id=5)\n    Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_UpdateMultipleSets(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForUpdate(parseUpdate(t, "UPDATE t SET v = 1, name = 'x'"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Update(t SET v=1, name='x')\n  Scan(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// parseInsert returns the parsed InsertStatementContext.
func parseInsert(t *testing.T, sql string) antlrgen.IInsertStatementContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	stmt := root.Statements().AllStatement()[0]
	dml := stmt.DmlStatement()
	if dml == nil {
		t.Fatalf("not a DML statement: %q", sql)
	}
	return dml.InsertStatement()
}

func TestBuildLogicalPlan_InsertValues(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForInsert(parseInsert(t, "INSERT INTO t (id, v) VALUES (1, 2)"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	// VALUES form: no Source subtree at the logical level.
	want := "Insert(t(id, v))"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_InsertValuesNoColumnList(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForInsert(parseInsert(t, "INSERT INTO t VALUES (1, 2, 3)"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Insert(t)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildLogicalPlan_InsertSelect(t *testing.T) {
	t.Parallel()
	op := buildLogicalPlanForInsert(parseInsert(t, "INSERT INTO t (id) SELECT id FROM src WHERE id > 5"))
	if op == nil {
		t.Fatal("expected non-nil")
	}
	want := "Insert(t(id))\n  Project(id)\n    Filter(id>5)\n      Scan(src)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
