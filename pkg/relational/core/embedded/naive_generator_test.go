package embedded

import (
	"context"
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
	if got := p.Explain(); got != "Scan(t)" {
		t.Fatalf("got %q, want Scan(t)", got)
	}
	if p.IsUpdate() {
		t.Fatal("SELECT should not be an update plan")
	}
}

func TestNaiveGenerator_Explain_SelectWhere(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT id, name FROM users WHERE active = TRUE ORDER BY id LIMIT 10")
	got := p.Explain()
	// Composition: Project → Limit → Sort → Filter → Scan.
	for _, want := range []string{
		"Project(id, name)",
		"Limit(10)",
		"Sort(id ASC)",
		"Filter(active = TRUE)",
		"Scan(users)",
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
	for _, want := range []string{"Project(a.id)", "Filter(a.active = TRUE)", "InnerJoin(on a.id = b.a_id)", "Scan(a)", "Scan(b)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_Delete(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "DELETE FROM t WHERE id > 5")
	got := p.Explain()
	for _, want := range []string{"Delete(t)", "Filter(id > 5)", "Scan(t)"} {
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
	for _, want := range []string{"Update(users SET active=FALSE)", "Filter(id = 5)", "Scan(users)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_Insert(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "INSERT INTO t (id, name) VALUES (1, 'a')")
	if got := p.Explain(); !strings.Contains(got, "Insert(t(id, name))") {
		t.Fatalf("got %q, want Insert(t(id, name))", got)
	}
}

func TestNaiveGenerator_Explain_UnionAll(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT id FROM a UNION ALL SELECT id FROM b")
	got := p.Explain()
	if !strings.Contains(got, "UnionAll") {
		t.Fatalf("got %q, want UnionAll", got)
	}
	if !strings.Contains(got, "Scan(a)") || !strings.Contains(got, "Scan(b)") {
		t.Fatalf("got %q, want Scan(a) and Scan(b)", got)
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
// cascades.QueryPredicate.Explain() instead of canonical SQL text.
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
