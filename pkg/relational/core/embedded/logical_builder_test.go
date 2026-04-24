package embedded

import (
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

// Aggregate / GROUP BY / COUNT — builder bails (returns nil). The
// naive Generator falls back to canonical-SQL Explain.
func TestBuildLogicalPlan_BailsOnAggregate(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT COUNT(*) FROM t")
	if op := buildLogicalPlanForSelect(sq); op != nil {
		t.Fatalf("expected nil for COUNT(*), got %s", op.Explain(""))
	}
}

// JOIN — builder bails.
func TestBuildLogicalPlan_BailsOnJoin(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT a.id FROM a INNER JOIN b ON a.id = b.a_id")
	if op := buildLogicalPlanForSelect(sq); op != nil {
		t.Fatalf("expected nil for JOIN, got %s", op.Explain(""))
	}
}

// SELECT without FROM — builder bails.
func TestBuildLogicalPlan_BailsOnNoFrom(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT 1 + 2")
	if op := buildLogicalPlanForSelect(sq); op != nil {
		t.Fatalf("expected nil for no-FROM, got %s", op.Explain(""))
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
