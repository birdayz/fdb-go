package semantic

import (
	"testing"

	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Parse a SELECT and return the FullIdContext of its first FROM table.
func firstFromTableFullId(t *testing.T, sql string) antlrgen.IFullIdContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	query := sel.Query()
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		t.Fatalf("unexpected body: %T", query.QueryExpressionBody())
	}
	simple, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		t.Fatalf("unexpected query term: %T", body.QueryTerm())
	}
	sources := simple.FromClause().TableSources().AllTableSource()
	if len(sources) == 0 {
		t.Fatal("no table sources")
	}
	srcBase, ok := sources[0].(*antlrgen.TableSourceBaseContext)
	if !ok {
		t.Fatalf("unexpected source: %T", sources[0])
	}
	atomItem, ok := srcBase.TableSourceItem().(*antlrgen.AtomTableItemContext)
	if !ok {
		t.Fatalf("unexpected atom: %T", srcBase.TableSourceItem())
	}
	return atomItem.TableName().FullId()
}

func TestFromFullIdContext_BareTable(t *testing.T) {
	t.Parallel()
	ctx := firstFromTableFullId(t, "SELECT * FROM Users")
	q := FromFullIdContext(ctx, false)
	if got, want := q.String(), "USERS"; got != want {
		t.Fatalf("String: got %q, want %q", got, want)
	}
	if q.IsQualified() {
		t.Fatal("bare table should not be qualified")
	}
}

func TestFromFullIdContext_QualifiedTable(t *testing.T) {
	t.Parallel()
	ctx := firstFromTableFullId(t, "SELECT * FROM schema1.Users")
	q := FromFullIdContext(ctx, false)
	if got, want := q.String(), "SCHEMA1.USERS"; got != want {
		t.Fatalf("String: got %q, want %q", got, want)
	}
	if !q.IsQualified() {
		t.Fatal("schema1.Users should be qualified")
	}
	if got, want := q.Name(), "USERS"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
}

func TestFromFullIdContext_QuotedTable(t *testing.T) {
	t.Parallel()
	ctx := firstFromTableFullId(t, `SELECT * FROM "Users"`)
	q := FromFullIdContext(ctx, false)
	if got, want := q.Name(), "Users"; got != want {
		t.Fatalf("quoted leaf case preserved: got %q, want %q", got, want)
	}
	leaf := q.LeafIdentifier()
	if !leaf.WasQuoted() {
		t.Fatal("quoted leaf should report wasQuoted")
	}
}

func TestFromFullIdContext_Nil(t *testing.T) {
	t.Parallel()
	if q := FromFullIdContext(nil, false); !q.IsZero() {
		t.Fatalf("nil ctx should produce zero QualifiedName, got %q", q)
	}
}

func TestFromUidContext_Nil(t *testing.T) {
	t.Parallel()
	if id := FromUidContext(nil, false); !id.IsZero() {
		t.Fatalf("nil ctx should produce zero Identifier, got %q", id)
	}
}
