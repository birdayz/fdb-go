package embedded

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

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
	op := buildLogicalPlanForSelectWithCatalog(sq, md)
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
	op := buildLogicalPlanForSelectWithCatalog(sq, md)
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
	op := buildLogicalPlanForSelectWithCatalog(sq, nil)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil when md is nil")
	}
	if want := "Filter(id > 5)\n  Scan(t)"; op.Explain("") != want {
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
	op := buildLogicalPlanForSelectWithCatalog(sq, md)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil on catalog miss")
	}
	if want := "Filter(id > 5)\n  Scan(NoSuchTable)"; op.Explain("") != want {
		t.Fatalf("Explain: got %q, want %q", op.Explain(""), want)
	}
}

// A WHERE shape outside the walker's scope (scalar function call,
// for example) returns UnsupportedExpressionShapeError from the
// walker; the builder must fall back to PredicateText so Explain
// still renders.
func TestBuildLogicalPlanWithCatalog_UnsupportedShape(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	// UPPER(name) is a scalar function — not handled by WalkExpression.
	sq := parseSelect(t, "SELECT * FROM Order WHERE UPPER(price) = 'X'")
	op := buildLogicalPlanForSelectWithCatalog(sq, md)
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

// DELETE WHERE uses the catalog-aware path and emits a real
// QueryPredicate. Same structural shape as SELECT: LogicalDelete
// wraps Scan → Filter; the Filter carries the walked predicate.
func TestBuildLogicalPlanWithCatalog_DeleteWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	del := parseDelete(t, "DELETE FROM Order WHERE price > 5")
	op := buildLogicalPlanForDeleteWithCatalog(del, md)
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
	op := buildLogicalPlanForUpdateWithCatalog(upd, md)
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

// JOINs aren't wired through buildWherePredicate yet. The builder
// must notice and fall back to text rather than producing a broken
// single-source predicate that ignores the right-hand source.
func TestBuildLogicalPlanWithCatalog_JoinFallsBackToText(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE Order.price > 5")
	op := buildLogicalPlanForSelectWithCatalog(sq, md)
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
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil on JOIN shape — walker should have declined")
	}
}
