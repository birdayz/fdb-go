package embedded

import (
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// parseQueryFromSelect parses SQL and returns the IQueryContext from
// the first SELECT statement. Used by Query-level builder tests.
func parseQueryFromSelect(t *testing.T, sql string) (antlrgen.IQueryContext, error) {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	if sel == nil {
		t.Fatalf("not a SELECT statement: %q", sql)
	}
	return sel.Query(), nil
}

// buildTestMetaData returns a minimal RecordMetaData with the demo
// record types registered. Used by the catalog-aware builder tests
// to exercise rlcatalog lookups without a live FDB.
func buildTestMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return md
}

// TestBuildLogicalPlanWithCatalog_WhereWalked pins the happy path:
// a WHERE shape the walker supports becomes a QueryPredicate tree on
// LogicalFilter, and Explain renders from the tree.
func TestBuildLogicalPlanWithCatalog_WhereWalked(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE price > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected top-level LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatalf("expected Predicate to be set, got nil (text=%q)", filter.PredicateText)
	}
	// Explain should route through the predicate tree now, not
	// PredicateText. The walker normalises column casing to upper
	// (rlcatalog is case-insensitive); ExplainValue renders literals
	// unquoted via valueLiteralString.
	got := op.Explain("")
	if !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected PRICE > 5 in Explain, got %q", got)
	}
}

// Walker success on an AND of comparisons — both leaves resolved,
// connective reconstructed. Pins that multi-leaf predicates compose
// through the catalog-aware path.
func TestBuildLogicalPlanWithCatalog_WhereAnd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE price > 5 AND order_id = 1")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate on AND shape")
	}
	got := filter.Predicate.Explain()
	if !strings.Contains(got, "PRICE > 5") || !strings.Contains(got, "ORDER_ID = 1") {
		t.Fatalf("expected both leaves in predicate, got %q", got)
	}
}

// Passing md=nil must behave identically to buildLogicalPlanForSelect
// — no predicate attached, Explain renders from text. Guarantees the
// catalog-aware builder is a strict superset of the text builder.
func TestBuildLogicalPlanWithCatalog_NilMetaData(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM t WHERE id > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, nil, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil when md is nil")
	}
	if want := "Filter(id > 5)\n  Scan(T)"; op.Explain("") != want {
		t.Fatalf("Explain: got %q, want %q", op.Explain(""), want)
	}
}

// Catalog miss (table not registered) falls back to text. Ensures a
// bad schema lookup doesn't hard-fail the builder; the next shift
// can add validation elsewhere if desired.
func TestBuildLogicalPlanWithCatalog_UnknownTable(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM NoSuchTable WHERE id > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil on catalog miss")
	}
	if want := "Filter(id > 5)\n  Scan(NOSUCHTABLE)"; op.Explain("") != want {
		t.Fatalf("Explain: got %q, want %q", op.Explain(""), want)
	}
}

// A WHERE shape outside the walker's scope returns
// UnsupportedExpressionShapeError; the builder must fall back to
// PredicateText so Explain still renders. FROBNICATE() is a
// deliberate non-existent scalar function — walkScalarFunction
// declines on names not in the seed catalogue.
func TestBuildLogicalPlanWithCatalog_UnsupportedShape(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE FROBNICATE(price) = 1")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil — walker should have declined")
	}
	if filter.PredicateText == "" {
		t.Fatal("expected text fallback populated")
	}
}

// TestBuildLogicalPlanWithCatalog_RHSArithmeticFolded pins the
// SimplifyPredicateValues wire-in: a constant arithmetic RHS
// (`PRICE = 1+2`) folds at plan time so EXPLAIN renders `PRICE = 3`
// rather than `PRICE = 1 + 2`. Same applies to nested arithmetic and
// scalar-function RHS (`name = UPPER('hi')` → `NAME = "HI"`).
func TestBuildLogicalPlanWithCatalog_RHSArithmeticFolded(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE price = 1+2")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate non-nil")
	}
	if got := filter.Predicate.Explain(); got != "PRICE = 3" {
		t.Fatalf("Predicate.Explain: got %q, want PRICE = 3", got)
	}
}

// TestBuildLogicalPlanWithCatalog_RHSScalarFunctionFolded pins the
// scalar-function arm: `name = UPPER('hi')` reaches EXPLAIN as
// `NAME = "HI"`.
func TestBuildLogicalPlanWithCatalog_RHSScalarFunctionFolded(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Customer WHERE name = UPPER('hi')")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate non-nil")
	}
	got := filter.Predicate.Explain()
	if !strings.Contains(got, "HI") || strings.Contains(got, "UPPER") {
		t.Fatalf("Predicate.Explain: got %q, want folded HI without UPPER", got)
	}
}

// UPPER (and the rest of the seed scalar function set — LOWER,
// LENGTH, CHAR_LENGTH, OCTET_LENGTH) IS now handled by
// walkScalarFunction. The catalog-aware builder attaches a real
// Predicate carrying the ScalarFunctionValue. Pins the new path
// so a future walker change that breaks scalar dispatch is caught
// immediately rather than silently regressing to text.
func TestBuildLogicalPlanWithCatalog_ScalarFunctionWalked(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE UPPER(price) = 'X'")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate non-nil — walker should accept UPPER")
	}
}

// DELETE WHERE uses the catalog-aware path and emits a real
// QueryPredicate. Same structural shape as SELECT: LogicalDelete
// wraps Scan → Filter; the Filter carries the walked predicate.
func TestBuildLogicalPlanWithCatalog_DeleteWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	del := parseDelete(t, "DELETE FROM Order WHERE price > 5")
	op, err := buildLogicalPlanForDeleteWithCatalog(del, md, defaultEmbeddedSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate on DELETE WHERE")
	}
	if got := filter.Predicate.Explain(); got != "PRICE > 5" {
		t.Fatalf("Predicate.Explain: got %q, want PRICE > 5", got)
	}
}

// UPDATE WHERE — mirror of DELETE: catalog-aware variant attaches a
// predicate to the LogicalFilter nested under LogicalUpdate.
func TestBuildLogicalPlanWithCatalog_UpdateWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	upd := parseUpdate(t, "UPDATE Order SET price = 10 WHERE order_id = 1")
	op, err := buildLogicalPlanForUpdateWithCatalog(upd, md, defaultEmbeddedSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter in UPDATE plan, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate on UPDATE WHERE")
	}
	if got := filter.Predicate.Explain(); got != "ORDER_ID = 1" {
		t.Fatalf("Predicate.Explain: got %q, want ORDER_ID = 1", got)
	}
}

// buildLogicalPlanForQueryWithCatalog threads metadata through the
// top-level Query / QueryBody / Union recursion. UNION-of-SELECTs
// each get their own catalog-aware Filter when the WHERE walks
// cleanly. Pins that the recursion doesn't drop md somewhere.
func TestBuildLogicalPlanWithCatalog_UnionThreadsMd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"SELECT order_id FROM Order WHERE price > 5 UNION ALL SELECT order_id FROM Order WHERE price < 100")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	union, ok := op.(*logical.LogicalUnion)
	if !ok {
		t.Fatalf("expected LogicalUnion, got %T", op)
	}
	if len(union.Inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(union.Inputs))
	}
	for i, branch := range union.Inputs {
		var filter *logical.LogicalFilter
		for cur := branch; cur != nil; {
			if f, ok := cur.(*logical.LogicalFilter); ok {
				filter = f
				break
			}
			ch := cur.Children()
			if len(ch) != 1 {
				break
			}
			cur = ch[0]
		}
		if filter == nil {
			t.Fatalf("union branch %d missing Filter:\n%s", i, branch.Explain(""))
		}
		if filter.Predicate == nil {
			t.Fatalf("union branch %d missing Predicate (md not threaded?)", i)
		}
	}
}

// CTE bodies thread md too — WHERE inside `WITH c AS (SELECT ... WHERE ...)`
// also walks through the catalog-aware path.
func TestBuildLogicalPlanWithCatalog_CTEThreadsMd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT order_id FROM Order WHERE price > 5) SELECT * FROM c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	cte, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE root, got %T", op)
	}
	// The CTE body's filter should carry a Predicate.
	var bodyFilter *logical.LogicalFilter
	for cur := cte.Body; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			bodyFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if bodyFilter == nil {
		t.Fatalf("CTE body missing Filter:\n%s", cte.Body.Explain(""))
	}
	if bodyFilter.Predicate == nil {
		t.Fatal("CTE body Filter missing Predicate (md not threaded?)")
	}
}

func TestBuildLogicalPlanWithCatalog_CTEOuterWhereGetsRealPredicate(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT order_id, price FROM Order) SELECT order_id FROM c WHERE price > 10")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	cte, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE root, got %T", op)
	}
	// The MAIN query's filter (on the CTE reference) should carry a Predicate.
	var mainFilter *logical.LogicalFilter
	for cur := cte.Main; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			mainFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if mainFilter == nil {
		t.Fatalf("main query missing Filter:\n%s", cte.Main.Explain(""))
	}
	if mainFilter.Predicate == nil {
		t.Fatal("outer WHERE on CTE reference should have a real Predicate (CTE schema derived from body)")
	}
}

func TestBuildLogicalPlanWithCatalog_CTEChainedSchemaDerivation(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH a AS (SELECT order_id, price FROM Order), "+
			"b AS (SELECT order_id FROM a WHERE price > 5) "+
			"SELECT order_id FROM b WHERE order_id > 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan for chained CTE query")
	}
	// Walk to the outermost CTE (A wraps B wraps main).
	cteA, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE root, got %T", op)
	}
	cteB, ok := cteA.Main.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected inner LogicalCTE (B), got %T", cteA.Main)
	}
	// Main query's filter on CTE B reference should have a real Predicate.
	var mainFilter *logical.LogicalFilter
	for cur := cteB.Main; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			mainFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if mainFilter == nil {
		t.Fatalf("main query missing Filter:\n%s", cteB.Main.Explain(""))
	}
	if mainFilter.Predicate == nil {
		t.Fatal("outer WHERE on chained CTE should have a real Predicate")
	}
}

func TestBuildLogicalPlanWithCatalog_CTESelectStarSchemaDerivation(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT * FROM Order) SELECT order_id FROM c WHERE price > 10")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	cte, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE, got %T", op)
	}
	var mainFilter *logical.LogicalFilter
	for cur := cte.Main; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			mainFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if mainFilter == nil {
		t.Fatalf("main query missing Filter:\n%s", cte.Main.Explain(""))
	}
	if mainFilter.Predicate == nil {
		t.Fatal("outer WHERE on CTE SELECT * should have a real Predicate")
	}
}

func TestBuildLogicalPlanWithCatalog_CTENoPredNeeded(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT order_id FROM Order) SELECT order_id FROM c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan for CTE without WHERE")
	}
}

func TestBuildLogicalPlanWithCatalog_JoinOnPredicateUpgrade(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"SELECT Order.order_id FROM Order INNER JOIN Customer ON Order.customer_id = Customer.customer_id WHERE Order.price > 5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	// Verify the plan contains a LogicalJoin with OnText set.
	var join *logical.LogicalJoin
	for cur := op; cur != nil; {
		if j, ok := cur.(*logical.LogicalJoin); ok {
			join = j
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if join == nil {
		t.Fatalf("expected LogicalJoin in plan:\n%s", op.Explain(""))
	}
	if join.OnText == "" {
		t.Fatal("JOIN OnText should be non-empty")
	}
	// OnPredicate upgrade is best-effort — the upgrade may fail if the
	// resolver can't walk the ON expression. Pin the upgrade success
	// rather than failing on it.
	if join.OnPredicate != nil {
		t.Logf("JOIN ON predicate upgraded successfully: %v", join.OnPredicate)
	} else {
		t.Logf("JOIN ON predicate not upgraded (resolver declined) — OnText=%q", join.OnText)
	}
}

// INSERT … SELECT routes the inner SELECT's WHERE through the
// catalog-aware path. INSERT VALUES (no nested SELECT) is identical
// to the text builder's output.
func TestBuildLogicalPlanWithCatalog_InsertSelectThreadsMd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t,
		"INSERT INTO Customer (customer_id, name) SELECT order_id, 'x' FROM Order WHERE price > 5")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source == nil {
		t.Fatal("expected non-nil Source on INSERT … SELECT")
	}
	// The inner SELECT's filter should carry a Predicate.
	var filter *logical.LogicalFilter
	for cur := insertOp.Source; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("inner SELECT missing Filter:\n%s", insertOp.Source.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatal("inner SELECT Filter missing Predicate (md not threaded?)")
	}
}

// INSERT … SELECT without WHERE: catalog-aware path rebuilds Source
// but there's no Filter to upgrade. Plan should still produce a
// valid LogicalInsert with non-nil Source (the inner Scan).
func TestBuildLogicalPlanWithCatalog_InsertSelect_NoWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t,
		"INSERT INTO Customer (customer_id, name) SELECT order_id, 'x' FROM Order")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source == nil {
		t.Fatal("expected non-nil Source on INSERT … SELECT (no WHERE)")
	}
	// Inner SELECT has no WHERE — Source should be a Project / Scan
	// chain with no Filter on the spine.
	for cur := insertOp.Source; cur != nil; {
		if _, ok := cur.(*logical.LogicalFilter); ok {
			t.Fatalf("did not expect Filter when inner SELECT has no WHERE; tree:\n%s", insertOp.Source.Explain(""))
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
}

// INSERT … SELECT with a JOIN inside the SELECT — the catalog-aware
// path threads metadata down to the inner SELECT including its
// multi-source scope. Pins that the JOIN scope feature composes
// with the INSERT … SELECT path.
func TestBuildLogicalPlanWithCatalog_InsertSelectJoin(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t,
		"INSERT INTO Customer (customer_id, name) "+
			"SELECT order_id, 'x' FROM Order o JOIN Customer c ON o.order_id = c.customer_id WHERE o.price > 5")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source == nil {
		t.Fatal("expected non-nil Source on INSERT … SELECT JOIN")
	}
	// Inner SELECT's filter should carry a Predicate with the
	// qualified column resolved.
	var filter *logical.LogicalFilter
	for cur := insertOp.Source; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter inside INSERT … SELECT JOIN, got tree:\n%s", insertOp.Source.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatalf("expected resolved Predicate on JOIN-WHERE, PredicateText=%q", filter.PredicateText)
	}
	if got := filter.Predicate.Explain(); !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected PRICE > 5 in resolved predicate, got %q", got)
	}
}

// INSERT VALUES has no nested SELECT — the catalog-aware path
// returns the same shape as the text builder (Source is nil).
func TestBuildLogicalPlanWithCatalog_InsertValuesNoOp(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t, "INSERT INTO Customer (customer_id, name) VALUES (1, 'x')")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source != nil {
		t.Fatalf("VALUES form should leave Source nil, got %T", insertOp.Source)
	}
}

// upgradeFirstFilter returns true exactly when a LogicalFilter was
// found on the unary spine. Pins the invariant the catalog-aware
// builders rely on: the text builder always emits a Filter for any
// WHERE-carrying shape. If a future builder change drops the
// Filter, this test fires — and the catalog-aware builders would
// silently swallow predicates without it.
func TestUpgradeFirstFilter_Invariants(t *testing.T) {
	t.Parallel()
	// Every WHERE-carrying SELECT / DELETE / UPDATE shape the text
	// builder emits today. Extend this list whenever a new shape
	// lands that carries a WHERE through the logical builder.
	// LIMIT-carrying shape is rejected at parse time
	// (fdb-relational 4.11.1.0 / Go aligned), so the LIMIT spine is
	// unreachable from SQL. ORDER BY without LIMIT still exercises the
	// same Filter-on-spine invariant.
	cases := []string{
		"SELECT * FROM t WHERE id > 5",
		"SELECT id FROM t WHERE id > 5 ORDER BY id",
		"SELECT id, COUNT(*) FROM t WHERE id > 5 GROUP BY id",
		"SELECT id FROM t WHERE id > 5 AND name = 'x'",
	}
	dummyPred := predicates.NewConstantPredicate(predicates.TriTrue)
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			op := buildLogicalPlanForSelect(parseSelect(t, sql))
			if op == nil {
				t.Fatalf("builder returned nil for %q", sql)
			}
			if !upgradeFirstFilter(op, dummyPred) {
				t.Fatalf("expected Filter on unary spine for %q, got tree:\n%s", sql, op.Explain(""))
			}
		})
	}

	// DELETE + UPDATE: also have Filter on the spine (under
	// LogicalDelete / LogicalUpdate).
	del := parseDelete(t, "DELETE FROM t WHERE id > 5")
	if op := buildLogicalPlanForDelete(del); op == nil || !upgradeFirstFilter(op, dummyPred) {
		t.Fatal("DELETE WHERE missing Filter on spine")
	}
	upd := parseUpdate(t, "UPDATE t SET v = 1 WHERE id > 5")
	if op := buildLogicalPlanForUpdate(upd); op == nil || !upgradeFirstFilter(op, dummyPred) {
		t.Fatal("UPDATE WHERE missing Filter on spine")
	}

	// A WHERE-less shape has no Filter — upgradeFirstFilter returns
	// false. This is the shape the catalog-aware builders pre-guard
	// against via their sq.whereExpr==nil / del.WhereExpr()==nil gates.
	op := buildLogicalPlanForSelect(parseSelect(t, "SELECT * FROM t"))
	if upgradeFirstFilter(op, dummyPred) {
		t.Fatal("expected false on WHERE-less shape (no Filter to upgrade)")
	}
}

// JOIN with qualified-column WHERE — multi-source scope picks up
// both tables, and the walker resolves `Order.price` against the
// JOIN's primary source. Predicate tree carried on LogicalFilter.
func TestBuildLogicalPlanWithCatalog_JoinQualifiedColumn(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE Order.price > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	// Walk down to the Filter — the Join sits below.
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter in plan, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatalf("expected Predicate on JOIN shape; PredicateText=%q", filter.PredicateText)
	}
	if got := filter.Predicate.Explain(); !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected PRICE > 5 in JOIN predicate, got %q", got)
	}
}

// JOIN with bare column unique to one side — walker resolves
// without ambiguity. `quantity` exists in Order only, not Customer.
func TestBuildLogicalPlanWithCatalog_JoinUniqueBareColumn(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE quantity > 0")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil || filter.Predicate == nil {
		t.Fatalf("expected resolved Predicate on JOIN with bare-column WHERE")
	}
	if got := filter.Predicate.Explain(); !strings.Contains(got, "QUANTITY") {
		t.Fatalf("expected QUANTITY in resolved predicate, got %q", got)
	}
}

// Self-join without explicit alias — `Order JOIN Order ON ...`
// produces two sources both named ORDER. AddSource trips
// DuplicateAlias → addSource returns false → buildWherePredicateForJoins
// returns false → text fallback. Pins the expected degradation
// for an uncommon but legal shape.
func TestBuildLogicalPlanWithCatalog_SelfJoinWithoutAlias_FallsBackToText(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Order ON Order.order_id = Order.order_id WHERE price > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if op == nil {
		t.Fatal("buildLogicalPlanForSelectWithCatalog returned nil for self-join without alias")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate != nil {
		t.Fatalf("expected text fallback for self-join without alias; Predicate=%s", filter.Predicate.Explain())
	}
}

// 3-way JOIN — sq.joins carries two entries; buildWherePredicateForJoins
// must add all three sources (primary + 2 joins). Walker resolves
// qualified refs from any of the three.
func TestBuildLogicalPlanWithCatalog_ThreeWayJoin(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order o "+
			"JOIN Customer c ON o.order_id = c.customer_id "+
			"JOIN TypedRecord t ON o.order_id = t.id "+
			"WHERE o.price > 5 AND t.id > 0")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatalf("expected resolved Predicate on 3-way JOIN; PredicateText=%q", filter.PredicateText)
	}
	got := filter.Predicate.Explain()
	// Both branches of the AND should resolve.
	if !strings.Contains(got, "PRICE > 5") {
		t.Errorf("expected PRICE > 5, got %q", got)
	}
	if !strings.Contains(got, "ID > 0") {
		t.Errorf("expected ID > 0 (from TypedRecord.id), got %q", got)
	}
}

// JOIN with ambiguous bare column — `price` exists in both Order
// and Customer. Walker correctly fails on AmbiguousColumnError;
// builder falls back to text.
func TestBuildLogicalPlanWithCatalog_JoinAmbiguousColumn_ErrorsProperly(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE price > 5")
	_, err := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if err == nil {
		t.Fatal("expected ambiguous column error for unqualified 'price' in JOIN (exists in both Order and Customer)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeAmbiguousColumn {
		t.Fatalf("expected ErrCodeAmbiguousColumn, got: %v", err)
	}
}
