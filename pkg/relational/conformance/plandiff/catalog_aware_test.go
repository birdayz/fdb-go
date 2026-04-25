package plandiff

// Tests for the catalog-aware Go plan-harness path (RFC-022 §4.-1
// Phase 3). NewExplainOnlyGeneratorWithSchema parses a CREATE SCHEMA
// TEMPLATE body, builds a synthetic in-memory schema cache, and
// routes WHERE / DELETE / UPDATE shapes through
// buildLogicalPlanFor*WithCatalog so cascades.predicates.QueryPredicate trees
// appear in Explain output. These tests pin both the routing and the
// observable difference vs the text-only baseline.

import (
	"context"
	"strings"
	"testing"
)

// TestGoEngine_CatalogAwarePathDiffersFromTextOnly pins the
// observable difference between the text-only and catalog-aware
// rendering: the catalog path re-renders the WHERE clause via
// cascades.predicates.QueryPredicate.Explain (parens-wrapped, comparison-
// flattened), the text path emits the canonical source-text.
//
// We deliberately don't pin the exact catalog-aware rendering — the
// rule simplifier may change it — only that it CHANGES, which proves
// the catalog-aware path is the one that fired.
func TestGoEngine_CatalogAwarePathDiffersFromTextOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()
	const schema = "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY(id))"
	const sql = "SELECT id FROM Item WHERE val = 5"

	textOnly := eng.Plan(ctx, Query{Name: "text", SQL: sql})
	if textOnly.Err != nil {
		t.Fatalf("text-only plan errored: %v", textOnly.Err)
	}
	catalog := eng.Plan(ctx, Query{Name: "catalog", SQL: sql, SchemaTemplate: schema})
	if catalog.Err != nil {
		t.Fatalf("catalog-aware plan errored: %v\n  schema: %s", catalog.Err, schema)
	}

	if textOnly.Tree == catalog.Tree {
		t.Fatalf("expected catalog-aware Tree to differ from text-only — same shape implies the catalog path didn't fire.\n  tree:\n%s",
			textOnly.Tree)
	}
	if !strings.Contains(catalog.Tree, "Filter(") {
		t.Fatalf("catalog tree missing Filter(...) shell — wrong rendering path?\n  tree:\n%s", catalog.Tree)
	}
}

// TestGoEngine_CatalogAware_AcceptsBareSchemaBody pins the auto-wrap
// behaviour: the harness's existing corpus passes a bare CREATE TABLE
// body (mirroring conformance/sql_plan_steps.java#planSql which wraps
// into CREATE SCHEMA TEMPLATE on the Java side). The Go side wraps
// the same shape rather than rejecting it.
func TestGoEngine_CatalogAware_AcceptsBareSchemaBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()

	cases := []struct {
		name string
		ddl  string
	}{
		{"bare-table", "CREATE TABLE Item (id BIGINT NOT NULL, name STRING, PRIMARY KEY(id))"},
		{"explicit-template-header", "CREATE SCHEMA TEMPLATE my_T " +
			"CREATE TABLE Item (id BIGINT NOT NULL, name STRING, PRIMARY KEY(id))"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := eng.Plan(ctx, Query{Name: tc.name, SQL: "SELECT id FROM Item", SchemaTemplate: tc.ddl})
			if r.Err != nil {
				t.Fatalf("plan errored: %v\n  ddl: %s", r.Err, tc.ddl)
			}
			if r.Tree == "" {
				t.Fatalf("plan tree empty")
			}
		})
	}
}

// TestGoEngine_CatalogAware_RejectsMalformedDDL pins the error path:
// malformed schema DDL surfaces as a goEngine error rather than a
// silent fall-through to the text-only path. A silent fallback would
// hide schema-side bugs in the corpus and make catalog-divergence
// invisible.
func TestGoEngine_CatalogAware_RejectsMalformedDDL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()
	r := eng.Plan(ctx, Query{
		Name:           "bad",
		SQL:            "SELECT 1",
		SchemaTemplate: "THIS IS NOT VALID SQL",
	})
	if r.Err == nil {
		t.Fatalf("expected error on malformed schema DDL, got tree: %q", r.Tree)
	}
}

// TestGoEngine_CatalogAware_DerivedTable_FoldsWhere pins the
// derived-table WHERE path: `FROM (SELECT id, val FROM Item) AS x
// WHERE val = 5` should route through buildLogicalPlanFor*WithCatalog
// + the new buildDerivedTableSource synthesis, so the rendered
// Filter body is a cascades.QueryPredicate tree, not the source-
// text fallback.
//
// Pre-this-shift the buildWherePredicate helper bailed on
// sq.derivedQuery != nil and the WHERE rendered as PredicateText.
// The synthesised virtual ScopeSource lets the walker resolve `val`
// against the inner table's BIGINT column type without needing the
// derived query to materialise its rows.
func TestGoEngine_CatalogAware_DerivedTable_FoldsWhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()
	const schema = "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY(id))"
	const sqlBare = "SELECT id FROM Item WHERE val = 5"
	const sqlDerived = "SELECT id FROM (SELECT id, val FROM Item) AS x WHERE val = 5"

	bare := eng.Plan(ctx, Query{Name: "bare", SQL: sqlBare, SchemaTemplate: schema})
	if bare.Err != nil {
		t.Fatalf("bare-table plan errored: %v", bare.Err)
	}
	derived := eng.Plan(ctx, Query{Name: "derived", SQL: sqlDerived, SchemaTemplate: schema})
	if derived.Err != nil {
		t.Fatalf("derived-table plan errored: %v\n  schema: %s", derived.Err, schema)
	}
	// Both bare and derived shapes should now route through the
	// catalog-aware path. We don't pin exact tree shape (rule
	// simplifier may evolve), only that the derived case got past
	// the buildWherePredicate decline.
	if !strings.Contains(derived.Tree, "Filter(") {
		t.Fatalf("derived plan missing Filter(...) shell: %s", derived.Tree)
	}
}

// TestGoEngine_CatalogAware_DerivedTable_DeclinesComplex pins the
// fallback path: derived queries the seed Type system can't reason
// about (computed projections, joins, SELECT *, aggregates) decline
// the catalog-aware path and fall back to text. The Plan still
// succeeds — only the WHERE-tree richness drops.
func TestGoEngine_CatalogAware_DerivedTable_DeclinesComplex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()
	const schema = "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY(id))"

	cases := []struct {
		name string
		sql  string
	}{
		// Computed projection — type unknown without Type hierarchy.
		{"computed-proj", "SELECT id FROM (SELECT id, val + 1 AS v FROM Item) AS x WHERE v > 0"},
		// SELECT * — would require expanding to inner table's columns.
		{"select-star", "SELECT id FROM (SELECT * FROM Item) AS x WHERE val = 5"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := eng.Plan(ctx, Query{Name: tc.name, SQL: tc.sql, SchemaTemplate: schema})
			if r.Err != nil {
				t.Fatalf("plan errored: %v\n  sql: %s", r.Err, tc.sql)
			}
			if r.Tree == "" {
				t.Fatalf("plan tree empty")
			}
			// Should still produce a plan tree even if WHERE rendered
			// as text fallback.
		})
	}
}

// TestGoEngine_CatalogAware_StatementlessDDL pins the
// nil-RootContext.Statements() branch in buildSchemaTemplateFromDDL.
// `RootContext.Statements()` legitimately returns nil for inputs that
// parse as a Root with no Statements child (whitespace-only,
// semicolon-only, etc.); the Phase-3 first cut crashed with a
// nil-pointer dereference on the error message because the branch
// read len(stmts.AllStatement()) before nil-checking. Round-2 review
// on PR #115. These cases must surface as a clean error, never
// panic.
//
// (Empty SchemaTemplate is a different code path: buildGoGenerator
// short-circuits to the text-only constructor without calling
// buildSchemaTemplateFromDDL at all, so it doesn't exercise the bug.)
func TestGoEngine_CatalogAware_StatementlessDDL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()
	cases := []struct {
		name string
		ddl  string
	}{
		{"whitespace-only", "   \n\t  "},
		{"semicolon-only", ";"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			r := eng.Plan(ctx, Query{Name: tc.name, SQL: "SELECT 1", SchemaTemplate: tc.ddl})
			if r.Err == nil {
				t.Fatalf("expected error on DDL %q, got tree: %q", tc.ddl, r.Tree)
			}
		})
	}
}
