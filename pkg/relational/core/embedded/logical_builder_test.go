package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
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
	if got, want := op.Explain(""), "Scan(T)"; got != want {
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
	want := "Filter(id > 5)\n  Scan(T)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestBuildLogicalPlan_SelectColsOrder pins the SELECT-with-ORDER-BY
// shape (without LIMIT in this specific query). LIMIT/OFFSET is now
// supported as a Go extension via PlanVisitor.visitLimit.
func TestBuildLogicalPlan_SelectColsOrder(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT id, name FROM t ORDER BY id DESC")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	// Project at top (projCols set), then Sort, then Scan.
	want := "Project(ID, NAME)\n  Sort(ID DESC)\n    Scan(T)"
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
	want := "Aggregate(group=[], agg=[COUNT(*)])\n  Scan(T)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias is the RFC-079 regression at
// the logical-plan level: the legacy buildSelectShell builder — the path that builds
// UNION branches — must build the post-aggregate-EXPRESSION projection WITH its output
// alias. Before the fix it passed nil aliases (logical.NewProject(op, allProj, nil)),
// so a UNION branch's `COUNT(*)+1 AS x` lost the `x` alias and a by-name read of the
// union output returned NULL. Both SELECT builders now share
// buildPostAggregateProjection — the single source of alias truth.
func TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT COUNT(*)+1 AS x FROM a")
	op := buildLogicalPlanForSelect(sq)
	proj, ok := op.(*logical.LogicalProject)
	if !ok {
		t.Fatalf("expected top *logical.LogicalProject, got %T:\n%s", op, op.Explain(""))
	}
	found := false
	for _, a := range proj.Aliases {
		if strings.EqualFold(a, "X") {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-aggregate-expression projection must carry the AS alias (RFC-079); got Aliases=%v, plan:\n%s",
			proj.Aliases, op.Explain(""))
	}
	// The expression slot must be marked computed (needs Value resolution), parallel
	// to Projections.
	if len(proj.IsComputed) != len(proj.Projections) {
		t.Fatalf("IsComputed must be parallel to Projections: %d vs %d", len(proj.IsComputed), len(proj.Projections))
	}
}

// TestBuildLogicalPlan_PostAggExpr_NoSpuriousAlias pins the other direction: an
// unaliased post-aggregate expression must NOT be given an alias (hasAlias stays
// false → nil Aliases), so buildPostAggregateProjection only aliases when the SELECT
// list actually renamed the column.
func TestBuildLogicalPlan_PostAggExpr_NoSpuriousAlias(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT COUNT(*)+1 FROM a")
	op := buildLogicalPlanForSelect(sq)
	proj, ok := op.(*logical.LogicalProject)
	if !ok {
		t.Fatalf("expected top *logical.LogicalProject, got %T:\n%s", op, op.Explain(""))
	}
	for _, a := range proj.Aliases {
		if a != "" {
			t.Fatalf("unaliased post-aggregate expression must not be aliased; got Aliases=%v, plan:\n%s",
				proj.Aliases, op.Explain(""))
		}
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
	if got := op.Explain(""); !strings.Contains(got, "Aggregate(group=[DEPT], agg=[SUM(V)") {
		t.Fatalf("got %q, want Aggregate(group=[DEPT], agg=[SUM(V)...])", got)
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
	want := "Project(A.ID)\n" +
		"  InnerJoin(on a.id = b.a_id)\n" +
		"    Scan(A)\n" +
		"    Scan(B)"
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
	want := "LeftJoin(on a.id = b.a_id)\n" +
		"  Scan(A)\n" +
		"  Scan(B)"
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
	want := "RightJoin(on a.id = b.a_id)\n" +
		"  Scan(A)\n" +
		"  Scan(B)"
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
	want := "InnerJoin(on b.id = c.b_id)\n" +
		"  InnerJoin(on a.id = b.a_id)\n" +
		"    Scan(A)\n" +
		"    Scan(B)\n" +
		"  Scan(C)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// SELECT without FROM is rejected at parse time. fdb-relational
// 4.11.1.0's QueryVisitor.visitSimpleTable asserts a non-null FROM
// clause with `Assert.notNullUnchecked(fromClause(), UNSUPPORTED_QUERY,
// "query is not supported")`; Go's extractFromSimpleTable mirrors the
// rejection. Per project conformance principle: doesn't work in Java
// → doesn't work in Go. The LogicalValues builder shape stays in
// place for future use (e.g., VALUES (...) AS t(...)) but is no
// longer reachable from a bare SELECT.
func TestBuildLogicalPlan_ValuesNoFromRejected(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT 1 + 2")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	if sel == nil {
		t.Fatal("expected SELECT statement")
	}
	_, err = extractSelectParts(sel)
	if err == nil {
		t.Fatal("expected error from extractSelectParts on FROM-less SELECT")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("got code %s, want %s", apiErr.Code, api.ErrCodeUnsupportedQuery)
	}
	if apiErr.Message != "query is not supported" {
		t.Fatalf("got message %q, want %q", apiErr.Message, "query is not supported")
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
	for _, want := range []string{"CTE(ACTIVE_USERS)", "Filter(active = TRUE)", "Scan(USERS)", "Scan(ACTIVE_USERS)"} {
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
	if !strings.Contains(op.Explain(""), "RecursiveCTE(TREE)") {
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
	if strings.Index(got, "CTE(A)") > strings.Index(got, "CTE(B)") {
		t.Fatalf("expected CTE(A) before CTE(B), got %q", got)
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
	want := "UnionAll\n  Project(ID)\n    Scan(A)\n  Project(ID)\n    Scan(B)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// UNION (implicit DISTINCT): no quantifier.
func TestBuildLogicalPlan_UnionDistinctRejectsLikeCatalog(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM a UNION SELECT id FROM b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	op := buildLogicalPlanForQueryBody(sel.Query().QueryExpressionBody())
	if op != nil {
		t.Fatalf("expected nil for UNION DISTINCT (only UNION ALL supported), got %q", op.Explain(""))
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
	if got := op.Explain(""); !strings.Contains(got, "Scan(T)") || !strings.Contains(got, "Filter(id > 5)") {
		t.Fatalf("got %q, expected inner plan to contain Scan(T) and Filter(id > 5)", got)
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
	want := "Scan(T AS TBL)"
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
	want := "Delete(T)\n  Filter(id > 5)\n    Scan(T)"
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
	want := "Delete(T)\n  Scan(T)"
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
	want := "Update(T SET V=v + 1)\n  Filter(id = 5)\n    Scan(T)"
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
	want := "Update(T SET V=1, NAME='x')\n  Scan(T)"
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
	want := "Insert(T(ID, V))"
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
	want := "Insert(T)"
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
	want := "Insert(T(ID))\n  Project(ID)\n    Filter(id > 5)\n      Scan(SRC)"
	if got := op.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestAggArgQualified_ParseTreeTruth pins aggSelectCol.aggArgQualified: a
// dotted reference is qualified by FullId SEGMENT COUNT, and a delimited
// identifier containing a literal dot is ONE unqualified segment — the
// distinction a dot scan of the rendered name cannot make.
func TestAggArgQualified_ParseTreeTruth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sql       string
		qualified bool
	}{
		{`SELECT SUM(t.c) FROM t`, true},
		{`SELECT SUM(c) FROM t`, false},
		{`SELECT SUM("a.b") FROM t`, false},
	}
	for _, tc := range cases {
		sq := parseSelect(t, tc.sql)
		if len(sq.aggCols) != 1 {
			t.Fatalf("%s: %d aggCols, want 1", tc.sql, len(sq.aggCols))
		}
		if got := sq.aggCols[0].aggArgQualified; got != tc.qualified {
			t.Errorf("%s: aggArgQualified = %v, want %v", tc.sql, got, tc.qualified)
		}
	}
}

// TestGroupKeyStripClearsQualification pins the single-source strip: when the
// prefix is baked away the key must become BARE — stale qualification
// segments would chase a qualifier the runtime row no longer carries
// (corpus-caught during the GroupKey port).
func TestGroupKeyStripClearsQualification(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, `SELECT p.category, COUNT(*) FROM products p GROUP BY p.category`)
	if len(sq.groupBy) != 1 || !sq.groupBy[0].qualified {
		t.Fatalf("fixture must parse one qualified key, got %+v", sq.groupBy)
	}
	// The strip fires on the single-source shell path (stripPrefix = the
	// source alias) — drive it directly.
	op := buildSelectShell(logical.NewScan("products", "p"), sq, "P.")
	if op == nil {
		t.Fatal("build returned nil")
	}
	agg := findAggregate(op)
	if agg == nil {
		t.Fatal("no aggregate built")
	}
	k := agg.GroupKeys[0]
	if k.Qualified || k.Qualifier != "" {
		t.Fatalf("stripped key kept stale qualification: %+v", k)
	}
	if !strings.EqualFold(k.Display, "category") || !strings.EqualFold(k.Bare, "category") {
		t.Fatalf("stripped key = %+v, want bare CATEGORY", k)
	}
}

// TestOrderByKeySegments_ParseTreeTruth pins orderByClause/SortKey structured
// segments: qualification is FullId SEGMENT COUNT, and a delimited identifier
// containing a literal dot stays one bare segment (the retired consumers
// split renderings on the last dot).
func TestOrderByKeySegments_ParseTreeTruth(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, `SELECT c FROM t ORDER BY t.c, "a.b", c2`)
	if len(sq.orderBy) != 3 {
		t.Fatalf("want 3 keys, got %d", len(sq.orderBy))
	}
	if ob := sq.orderBy[0]; !ob.qualified || ob.qualifier != "T" || ob.bare != "C" {
		t.Fatalf("t.c segments = %+v", ob)
	}
	if ob := sq.orderBy[1]; ob.qualified || ob.bare != "a.b" {
		t.Fatalf(`"a.b" must be ONE bare segment, got %+v`, ob)
	}
	if ob := sq.orderBy[2]; ob.qualified || ob.bare != "C2" {
		t.Fatalf("c2 segments = %+v", ob)
	}
}

// TestSortKeySegments_AliasRebaseClearsStaleSegments pins the review-caught
// hole: the deferred-strip loop rebases an alias key's colName IN PLACE, so
// the key's parse-tree segments (Bare = the alias spelling) go stale — a
// source column spelled like the alias would silently mis-resolve. The
// rebase must clear the segments to the internal name.
func TestSortKeySegments_AliasRebaseClearsStaleSegments(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, `SELECT id AS v FROM t ORDER BY v`)
	// Force the deferred-strip shape the builder rebases under.
	sq.postSortStripProj = []string{"ID"}
	sq.postSortStripAliases = []string{"V"}
	op := buildSelectShell(logical.NewScan("t", ""), sq, "")
	sort, ok := op.(*logical.LogicalSort)
	if !ok {
		if s2, ok2 := findSortOp(op); ok2 {
			sort = s2
		} else {
			t.Fatalf("no sort in %T", op)
		}
	}
	k := sort.Keys[0]
	if k.Expr != "ID" {
		t.Fatalf("rebased key Expr = %q, want ID", k.Expr)
	}
	if k.Bare != "ID" || k.Qualified {
		t.Fatalf("rebased key kept stale segments: %+v (Bare must be the internal name, never the alias spelling)", k)
	}
}

// findSortOp walks the shell for the sort operator.
func findSortOp(op logical.LogicalOperator) (*logical.LogicalSort, bool) {
	for op != nil {
		if s, ok := op.(*logical.LogicalSort); ok {
			return s, true
		}
		ch := op.Children()
		if len(ch) == 0 {
			return nil, false
		}
		op = ch[0]
	}
	return nil, false
}
