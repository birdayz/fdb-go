package semantic

import (
	"errors"
	"strings"
	"testing"
)

func buildScope(t *testing.T) (*Scope, Table, Table) {
	t.Helper()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("users", false))
	orders, _ := c.LookupTable(ParseQualifiedName("schema1.orders", false))

	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{
		Table:           users,
		Alias:           NewUnquoted("u"),
		CorrelationName: "u",
	}); err != nil {
		t.Fatalf("AddSource users: %v", err)
	}
	if err := s.AddSource(ScopeSource{
		Table:           orders,
		Alias:           NewUnquoted("o"),
		CorrelationName: "o",
	}); err != nil {
		t.Fatalf("AddSource orders: %v", err)
	}
	return s, users, orders
}

func TestScope_ResolveColumn_Unique(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)

	// `name` exists only on users (orders has order_id, user_id).
	col, src, err := s.ResolveColumn(NewUnquoted("name"))
	if err != nil {
		t.Fatalf("resolve name: %v", err)
	}
	if got, want := col.Id.Name(), "NAME"; got != want {
		t.Fatalf("col Id: got %q, want %q", got, want)
	}
	if got, want := src.Alias.Name(), "U"; got != want {
		t.Fatalf("source alias: got %q, want %q", got, want)
	}
}

func TestScope_ResolveColumn_NotFound(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)

	_, _, err := s.ResolveColumn(NewUnquoted("nonexistent"))
	if err == nil {
		t.Fatal("expected error")
	}
	var cnf *ColumnNotFoundError
	if !errors.As(err, &cnf) {
		t.Fatalf("expected ColumnNotFoundError, got %T", err)
	}
}

// Two sources both have a column of the same name → bare reference
// is ambiguous.
func TestScope_ResolveColumn_Ambiguous(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("users", false))
	// Add a second "users"-like table exposing the same column names.
	dup := &StaticTable{
		TableName: ParseQualifiedName("users_copy", false),
		TableColumns: []Column{
			{Id: NewUnquoted("name"), Type: "STRING"},
		},
	}
	s := NewScope(nil)
	_ = s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")})
	_ = s.AddSource(ScopeSource{Table: dup, Alias: NewUnquoted("d")})

	_, _, err := s.ResolveColumn(NewUnquoted("name"))
	if err == nil {
		t.Fatal("expected error for ambiguous column")
	}
	var ace *AmbiguousColumnError
	if !errors.As(err, &ace) {
		t.Fatalf("expected AmbiguousColumnError, got %T", err)
	}
	if ace.Matches != 2 {
		t.Fatalf("expected 2 matches, got %d", ace.Matches)
	}
	if len(ace.Sources) != 2 {
		t.Fatalf("expected 2 conflicting sources, got %d", len(ace.Sources))
	}
	// Error message should name the conflicting aliases so the user
	// can qualify.
	msg := err.Error()
	if !strings.Contains(msg, "U") || !strings.Contains(msg, "D") {
		t.Fatalf("error should name both aliases; got %q", msg)
	}
}

func TestScope_ResolveQualifiedColumn(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)

	col, src, err := s.ResolveQualifiedColumn(NewUnquoted("u"), NewUnquoted("name"))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got, want := col.Id.Name(), "NAME"; got != want {
		t.Fatalf("col: got %q, want %q", got, want)
	}
	if got, want := src.Alias.Name(), "U"; got != want {
		t.Fatalf("source: got %q, want %q", got, want)
	}
}

func TestScope_ResolveQualifiedColumn_UnknownSource(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)

	_, _, err := s.ResolveQualifiedColumn(NewUnquoted("unknown_alias"), NewUnquoted("name"))
	if err == nil {
		t.Fatal("expected error")
	}
	var snf *SourceNotFoundError
	if !errors.As(err, &snf) {
		t.Fatalf("expected SourceNotFoundError, got %T", err)
	}
	// Error should list available aliases for "did you mean?" UX.
	if len(snf.Available) != 2 {
		t.Fatalf("expected 2 available aliases, got %d", len(snf.Available))
	}
	msg := err.Error()
	if !strings.Contains(msg, "available:") {
		t.Fatalf("error should list available aliases; got %q", msg)
	}
}

func TestScope_ResolveQualifiedColumn_UnknownColumn(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)

	_, _, err := s.ResolveQualifiedColumn(NewUnquoted("u"), NewUnquoted("no_such_col"))
	if err == nil {
		t.Fatal("expected error")
	}
	var cnf *ColumnNotFoundError
	if !errors.As(err, &cnf) {
		t.Fatalf("expected ColumnNotFoundError, got %T", err)
	}
	if cnf.TableName.Name() != "USERS" {
		t.Fatalf("wrong table in error: %s", cnf.TableName)
	}
}

func TestScope_AddSource_DuplicateAlias(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("users", false))

	s := NewScope(nil)
	_ = s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")})
	err := s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")})
	if err == nil {
		t.Fatal("expected error for duplicate alias")
	}
	var dae *DuplicateAliasError
	if !errors.As(err, &dae) {
		t.Fatalf("expected DuplicateAliasError, got %T", err)
	}
}

// Child scope resolves to parent sources when not found locally.
func TestScope_ParentLookup(t *testing.T) {
	t.Parallel()
	parent, _, _ := buildScope(t)
	child := NewScope(parent)

	// `name` isn't in the child scope (empty); parent finds it.
	col, _, err := child.ResolveColumn(NewUnquoted("name"))
	if err != nil {
		t.Fatalf("parent lookup: %v", err)
	}
	if got, want := col.Id.Name(), "NAME"; got != want {
		t.Fatalf("col: got %q, want %q", got, want)
	}
}

func TestScope_AllSourcesRecursive(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("users", false))
	orders, _ := c.LookupTable(ParseQualifiedName("schema1.orders", false))

	parent := NewScope(nil)
	_ = parent.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")})
	child := NewScope(parent)
	_ = child.AddSource(ScopeSource{Table: orders, Alias: NewUnquoted("o")})

	all := child.AllSourcesRecursive()
	if len(all) != 2 {
		t.Fatalf("expected 2 sources across the chain, got %d", len(all))
	}
	// Inner-first: orders before users.
	if all[0].Alias.Name() != "O" {
		t.Fatalf("first source alias: got %q, want O (inner-first)", all[0].Alias.Name())
	}
	if all[1].Alias.Name() != "U" {
		t.Fatalf("second source alias: got %q, want U", all[1].Alias.Name())
	}
}

// Inner scope shadows outer — if both have the same column name,
// inner wins.
func TestScope_InnerShadowsOuter(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("users", false))
	orders, _ := c.LookupTable(ParseQualifiedName("schema1.orders", false))

	parent := NewScope(nil)
	_ = parent.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")})
	child := NewScope(parent)
	_ = child.AddSource(ScopeSource{Table: orders, Alias: NewUnquoted("o")})

	// orders has order_id; parent's users doesn't. Child must find it.
	_, src, err := child.ResolveColumn(NewUnquoted("order_id"))
	if err != nil {
		t.Fatalf("shadow lookup: %v", err)
	}
	if got, want := src.Alias.Name(), "O"; got != want {
		t.Fatalf("expected inner source, got %q", got)
	}
}

func TestScope_SourcesDefensiveCopy(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)
	srcs := s.Sources()
	srcs[0].Alias = NewUnquoted("HACKED")
	// Original should be unchanged.
	s2 := s.Sources()
	if s2[0].Alias.Name() == "HACKED" {
		t.Fatal("Sources() mutation leaked")
	}
}
