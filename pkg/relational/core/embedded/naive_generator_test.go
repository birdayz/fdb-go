package embedded

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"
)

// End-to-end tests for naiveGenerator's Plan.Explain() surface. These
// don't touch FDB — Plan() only parses + wraps the parse tree into a
// Plan; Execute would run against a real store but Explain is a pure
// function of the parse tree + the logical builder.

// helperPlan runs naiveGenerator.Plan() against a SQL string and
// returns the resulting Plan. A parse error fails the test.
func helperPlan(t *testing.T, sql string) interface {
	Explain() string
	IsUpdate() bool
} {
	t.Helper()
	g := &naiveGenerator{c: &EmbeddedConnection{}}
	p, err := g.Plan(context.Background(), sql)
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	return p
}

func TestNaiveGenerator_Explain_SelectStar(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT * FROM t")
	if got := p.Explain(); got != "Scan(T)" {
		t.Fatalf("got %q, want Scan(T)", got)
	}
	if p.IsUpdate() {
		t.Fatal("SELECT should not be an update plan")
	}
}

func TestNaiveGenerator_Explain_SelectWhere(t *testing.T) {
	t.Parallel()
	// Exercises WHERE + ORDER BY composition (no LIMIT in this query).
	// LIMIT/OFFSET is now supported as a Go extension.
	p := helperPlan(t, "SELECT id, name FROM users WHERE active = TRUE ORDER BY id")
	got := p.Explain()
	// Composition: Project → Sort → Filter → Scan.
	for _, want := range []string{
		"Project(ID, NAME)",
		"Sort(ID ASC)",
		"Filter(active = TRUE)",
		"Scan(USERS)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_JoinWithWhere(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT a.id FROM a INNER JOIN b ON a.id = b.a_id WHERE a.active = TRUE")
	got := p.Explain()
	for _, want := range []string{"Project(A.ID)", "Filter(a.active = TRUE)", "InnerJoin(on a.id = b.a_id)", "Scan(A)", "Scan(B)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_Delete(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "DELETE FROM t WHERE id > 5")
	got := p.Explain()
	for _, want := range []string{"Delete(T)", "Filter(id > 5)", "Scan(T)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
	if !p.IsUpdate() {
		t.Fatal("DELETE should be an update plan")
	}
}

func TestNaiveGenerator_Explain_Update(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "UPDATE users SET active = FALSE WHERE id = 5")
	got := p.Explain()
	for _, want := range []string{"Update(USERS SET ACTIVE=FALSE)", "Filter(id = 5)", "Scan(USERS)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_Insert(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "INSERT INTO t (id, name) VALUES (1, 'a')")
	if got := p.Explain(); !strings.Contains(got, "Insert(T(ID, NAME))") {
		t.Fatalf("got %q, want Insert(T(ID, NAME))", got)
	}
}

func TestNaiveGenerator_Explain_UnionAll(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT id FROM a UNION ALL SELECT id FROM b")
	got := p.Explain()
	if !strings.Contains(got, "UnionAll") {
		t.Fatalf("got %q, want UnionAll", got)
	}
	if !strings.Contains(got, "Scan(A)") || !strings.Contains(got, "Scan(B)") {
		t.Fatalf("got %q, want Scan(A) and Scan(B)", got)
	}
}

func TestNaiveGenerator_Explain_Aggregate(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT COUNT(*) FROM t")
	if got := p.Explain(); !strings.Contains(got, "Aggregate(group=[], agg=[COUNT(*)])") {
		t.Fatalf("got %q, want COUNT(*) aggregate", got)
	}
}

// DDL / TX still use the canonical-SQL fallback (builder returns
// nil for those shapes). Ensure the fallback still renders.
func TestNaiveGenerator_Explain_DDLFallback(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "CREATE SCHEMA /mydb/main WITH TEMPLATE tmpl")
	got := p.Explain()
	if !strings.HasPrefix(got, "DDL:") {
		t.Fatalf("got %q, want DDL:-prefixed explain", got)
	}
}

// EXPLAIN <SELECT> — the planExplain path returns a 1-row plan
// with a PLAN column. Plan.Explain() renders 'EXPLAIN: <inner>'.
func TestNaiveGenerator_Explain_ExplainSelect(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN SELECT * FROM t")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN:-prefix", got)
	}
	if !strings.Contains(got, "Scan(T)") {
		t.Fatalf("got %q, want inner Scan(T)", got)
	}
	if p.IsUpdate() {
		t.Fatal("EXPLAIN should not be an update plan (returns rows)")
	}
}

// EXPLAIN <SELECT … WHERE …> exercises the catalog-cold fallback
// (no metadata in the connection). Predicate text comes from the
// text builder.
func TestNaiveGenerator_Explain_ExplainSelectWithWhere(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN SELECT id FROM users WHERE active = TRUE")
	got := p.Explain()
	for _, want := range []string{"EXPLAIN: ", "Filter(active = TRUE)", "Scan(USERS)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("got %q, missing %q", got, want)
		}
	}
}

// EXPLAIN UPDATE — the EXPLAIN path is independent from execution,
// so update statements behind EXPLAIN don't actually run; their
// text-builder rendering is what's returned.
func TestNaiveGenerator_Explain_ExplainUpdate(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN UPDATE orders SET status = 'done' WHERE id = 1")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN:-prefix", got)
	}
	// Must not actually execute (no FDB available in this test) — if
	// the dispatcher mistakenly routed EXPLAIN UPDATE to execStatement
	// the test would panic / error before reaching this assertion.
	if p.IsUpdate() {
		t.Fatal("EXPLAIN UPDATE should not be an update plan — it returns plan-text rows, not commits")
	}
}

// EXPLAIN INSERT — same independent-from-execution shape.
func TestNaiveGenerator_Explain_ExplainInsert(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN INSERT INTO users (id, name) VALUES (1, 'alice')")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN:-prefix", got)
	}
}

// EXPLAIN DELETE — same.
func TestNaiveGenerator_Explain_ExplainDelete(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN DELETE FROM orders WHERE status = 'cancelled'")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN:-prefix", got)
	}
	if !strings.Contains(got, "Delete(ORDERS") {
		t.Fatalf("got %q, want inner Delete(ORDERS)", got)
	}
}

// EXPLAIN Execute — the planExplain path produces a 1-row driver.Rows
// with column "PLAN". Verify the row stream end-to-end.
func TestNaiveGenerator_Explain_ExplainProducesPlanRow(t *testing.T) {
	t.Parallel()
	g := &naiveGenerator{c: &EmbeddedConnection{}}
	p, err := g.Plan(context.Background(), "EXPLAIN SELECT * FROM users")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	res, err := p.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Rows == nil {
		t.Fatal("Execute: Rows should be non-nil for EXPLAIN")
	}
	cols := res.Rows.Columns()
	if len(cols) != 1 || cols[0] != "PLAN" {
		t.Fatalf("columns: got %v, want [PLAN]", cols)
	}
}

// Empty SQL: Plan returns a no-op update plan; Explain renders
// "empty" per the seed.
func TestNaiveGenerator_Explain_EmptyStatements(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "")
	if got := p.Explain(); got != "empty" {
		t.Fatalf("got %q, want empty", got)
	}
}

// helperPlanWithCachedMd is helperPlan but populates the connection's
// session SchemaCache with a generated api.Schema backed by md.
// Used to verify the warm-cache path through ExplainFn — Explain
// should render predicate trees instead of canonical-SQL text.
func helperPlanWithCachedMd(t *testing.T, sql string, md *recordlayer.RecordMetaData, dbPath, schemaName string) interface {
	Explain() string
	IsUpdate() bool
} {
	t.Helper()
	tmpl, err := metadata.NewRecordLayerSchemaTemplate(schemaName, md)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	schema := tmpl.GenerateSchema(dbPath, schemaName)
	sess := &session.Session{
		DBPath: dbPath,
		Schema: schemaName,
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey(dbPath, schemaName): schema,
		},
	}
	g := &naiveGenerator{c: &EmbeddedConnection{sess: sess}}
	p, err := g.Plan(context.Background(), sql)
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	return p
}

// buildExplainTestMd constructs a minimal RecordMetaData usable by
// the warm-cache Explain tests. Mirrors logical_predicate_test.go's
// fixture so the tests share schema shape.
func buildExplainTestMd(t *testing.T) *recordlayer.RecordMetaData {
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

// Direct unit tests for cachedMetaData — the read-only accessor
// naive_generator's ExplainFn delegates to. Negative-path coverage
// (nil session, missing key, wrong template type, nil schema) is
// what guards against future refactors silently breaking the
// "deterministic cold-cache fallback" contract.
func TestCachedMetaData_NilSession(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	if md := conn.cachedMetaData(); md != nil {
		t.Fatalf("expected nil on nil session, got %v", md)
	}
}

func TestCachedMetaData_EmptyCache(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{sess: &session.Session{
		DBPath:      "/main",
		Schema:      "public",
		SchemaCache: map[string]api.Schema{},
	}}
	if md := conn.cachedMetaData(); md != nil {
		t.Fatalf("expected nil on empty cache, got %v", md)
	}
}

func TestCachedMetaData_DifferentKey(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	tmpl, err := metadata.NewRecordLayerSchemaTemplate("public", md)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	// Cache populated for a different (dbPath, schema) — the active
	// schema's lookup should miss.
	conn := &EmbeddedConnection{sess: &session.Session{
		DBPath: "/main",
		Schema: "public",
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey("/other", "elsewhere"): tmpl.GenerateSchema("/other", "elsewhere"),
		},
	}}
	if got := conn.cachedMetaData(); got != nil {
		t.Fatalf("expected nil on key miss, got %v", got)
	}
}

func TestCachedMetaData_HappyPath(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	tmpl, err := metadata.NewRecordLayerSchemaTemplate("public", md)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	conn := &EmbeddedConnection{sess: &session.Session{
		DBPath: "/main",
		Schema: "public",
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey("/main", "public"): tmpl.GenerateSchema("/main", "public"),
		},
	}}
	got := conn.cachedMetaData()
	if got == nil {
		t.Fatal("expected non-nil RecordMetaData on warm cache")
	}
	// Identity check — the returned md should be the underlying one.
	if got != md {
		t.Errorf("expected returned md to be the same instance as the template's underlying")
	}
}

// nil schema in cache must NOT panic and must return nil. The
// accessor's nil-guard is defence-in-depth in case session
// init-order ever lands a sentinel nil value.
func TestCachedMetaData_NilSchemaInCache(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{sess: &session.Session{
		DBPath: "/main",
		Schema: "public",
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey("/main", "public"): nil,
		},
	}}
	if got := conn.cachedMetaData(); got != nil {
		t.Fatalf("expected nil on nil-schema entry, got %v", got)
	}
}

// Warm-cache Explain — SELECT WHERE renders the predicate via
// cascades.predicates.QueryPredicate.Explain() instead of canonical SQL text.
// Pins the round-trip through naive_generator → cachedMetaData →
// catalog-aware builder → LogicalFilter.Predicate.
func TestNaiveGenerator_Explain_WarmCache_SelectWhere(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	p := helperPlanWithCachedMd(t,
		"SELECT * FROM Order WHERE price > 5",
		md, "/main", "public")
	got := p.Explain()
	// PRICE > 5 (upper-cased) is the predicate-tree form.
	if !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected PRICE > 5 in warm-cache explain, got %q", got)
	}
}

// Cold cache (no SchemaCache entry for current schema): falls back
// to text-builder. Predicate-tree form does NOT appear because the
// catalog-aware builder declined.
func TestNaiveGenerator_Explain_ColdCache_FallsBackToText(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	tmpl, err := metadata.NewRecordLayerSchemaTemplate("public", md)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	// SchemaCache populated for a DIFFERENT (dbPath, schema) pair.
	sess := &session.Session{
		DBPath: "/main",
		Schema: "public",
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey("/other", "other-schema"): tmpl.GenerateSchema("/other", "other-schema"),
		},
	}
	g := &naiveGenerator{c: &EmbeddedConnection{sess: sess}}
	p, err := g.Plan(context.Background(), "SELECT * FROM Order WHERE price > 5")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := p.Explain()
	// Text builder uses lowercase column from canonical SQL.
	if !strings.Contains(got, "Filter(price > 5)") {
		t.Fatalf("expected canonical-text Filter, got %q", got)
	}
}

// DELETE WHERE picks up the warm-cache path too.
func TestNaiveGenerator_Explain_WarmCache_DeleteWhere(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	p := helperPlanWithCachedMd(t,
		"DELETE FROM Order WHERE price > 5",
		md, "/main", "public")
	got := p.Explain()
	if !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected predicate-tree form in DELETE explain, got %q", got)
	}
	if !p.IsUpdate() {
		t.Fatal("DELETE should be an update plan")
	}
}

// `EXPLAIN <query>` warm-cache path: the EXPLAIN dispatcher delegates
// to computeExplainText, which goes through buildLogicalPlanForQuery
// WithCatalog — same predicate-tree form as the bare SELECT case.
// Pins that EXPLAIN doesn't drop the catalog-aware path.
func TestNaiveGenerator_Explain_ExplainWarmCacheSelect(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	p := helperPlanWithCachedMd(t,
		"EXPLAIN SELECT * FROM Order WHERE price > 5",
		md, "/main", "public")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("expected EXPLAIN: prefix, got %q", got)
	}
	if !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected predicate-tree form (PRICE > 5), got %q", got)
	}
	if p.IsUpdate() {
		t.Fatal("EXPLAIN must not be an update plan even on warm cache")
	}
}

// EXPLAIN EXECUTE CONTINUATION → UNSUPPORTED_OPERATION. The
// describeObjectClause alternative `executeContinuationStatement`
// IS a *DescribeStatementsContext (so the type-assertion branch
// passes), but the four accessor methods (Query / Delete / Insert
// / Update) all return nil — computeExplainText returns "" and
// planExplain's empty-string guard fires.
//
// Pinned because the comment in planExplain explicitly calls out
// this corner; the test makes sure a future change that adds
// continuation-handling at the !ok branch (or adds a new accessor)
// doesn't accidentally let it through.
func TestNaiveGenerator_Explain_ExplainContinuationDeclines(t *testing.T) {
	t.Parallel()
	g := &naiveGenerator{c: &EmbeddedConnection{}}
	_, err := g.Plan(context.Background(),
		"EXPLAIN EXECUTE CONTINUATION X'00'")
	if err == nil {
		t.Fatal("EXPLAIN <continuation>: expected error, got nil plan")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T: %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUnsupportedOperation {
		t.Fatalf("expected ErrCodeUnsupportedOperation, got %q", apiErr.Code)
	}
}

// EXPLAIN UNION ALL — exercises the SetQuery alternative inside
// the inner query. Ensures EXPLAIN doesn't choke on compound query
// shapes.
func TestNaiveGenerator_Explain_ExplainUnion(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN SELECT id FROM a UNION ALL SELECT id FROM b")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN: prefix", got)
	}
	for _, want := range []string{"Scan(A)", "Scan(B)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("got %q, missing %q (UNION inner)", got, want)
		}
	}
}

// EXPLAIN over a CTE — `WITH … SELECT` flows through the same
// query-shape path. The text builder handles it; the EXPLAIN wrapper
// just delegates. Content check covers both the producer side
// (Filter on the inner CTE definition) and the consumer side
// (the outer SELECT references active_users), so the test catches
// a regression where either side silently disappears.
func TestNaiveGenerator_Explain_ExplainCTE(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "EXPLAIN WITH active_users AS (SELECT id FROM users WHERE active = TRUE) SELECT id FROM active_users")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN: prefix", got)
	}
	for _, want := range []string{"USERS", "ACTIVE_USERS"} {
		if !strings.Contains(got, want) {
			t.Fatalf("got %q, missing %q (CTE producer/consumer reference)", got, want)
		}
	}
}

// EXPLAIN over a WHERE with constant arithmetic — warm-cache
// catalog-aware path → cascades.values.SimplifyValue folds 1+2 to 3.
// The user-visible PLAN row should show the folded form, not the
// pre-fold tree.
func TestNaiveGenerator_Explain_ExplainConstantFold(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	p := helperPlanWithCachedMd(t,
		"EXPLAIN SELECT * FROM Order WHERE price > 1 + 2",
		md, "/main", "public")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("got %q, want EXPLAIN: prefix", got)
	}
	// Folded predicate: PRICE > 3 (cascades.values.SimplifyValue collapsed 1+2).
	if !strings.Contains(got, "PRICE > 3") {
		t.Fatalf("expected folded predicate (PRICE > 3) in EXPLAIN output, got %q", got)
	}
	// The unfolded form should NOT appear.
	if strings.Contains(got, "1 + 2") || strings.Contains(got, "(1 + 2)") {
		t.Fatalf("EXPLAIN still shows unfolded 1+2 — fold did not run: %q", got)
	}
}

// `EXPLAIN UPDATE` warm-cache path — verifies the catalog-aware
// builder fires for UPDATE inside EXPLAIN, AND that no actual
// mutation is attempted (would panic without an FDB connection).
func TestNaiveGenerator_Explain_ExplainWarmCacheUpdate(t *testing.T) {
	t.Parallel()
	md := buildExplainTestMd(t)
	p := helperPlanWithCachedMd(t,
		"EXPLAIN UPDATE Order SET price = 99 WHERE price > 5",
		md, "/main", "public")
	got := p.Explain()
	if !strings.HasPrefix(got, "EXPLAIN: ") {
		t.Fatalf("expected EXPLAIN: prefix, got %q", got)
	}
	if !strings.Contains(got, "PRICE > 5") {
		t.Fatalf("expected predicate-tree form (PRICE > 5), got %q", got)
	}
	if p.IsUpdate() {
		t.Fatal("EXPLAIN UPDATE must not be an update plan — returns rows, not RowsAffected")
	}
}
