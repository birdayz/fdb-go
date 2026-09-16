package semantic

import (
	"errors"
	"fmt"
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

	// Duplicate PLAIN aliases are ACCEPTED —
	// Java registers quantifiers freely (unique ids) and errors
	// per-ATTRIBUTE at reference resolution; the two sources are
	// distinguished by CorrelationName (the parser-minted binding id).
	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u"), CorrelationName: "U"}); err != nil {
		t.Fatalf("first source: %v", err)
	}
	if err := s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u"), CorrelationName: "Q$DUP1"}); err != nil {
		t.Fatalf("duplicate PLAIN alias must be accepted (per-attribute resolution owns the ambiguity), got %v", err)
	}
	if got := len(s.Sources()); got != 2 {
		t.Fatalf("sources = %d, want both duplicate legs registered", got)
	}

	// Virtual element sources follow the same attribute ambiguity rule.
	if err := s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u"), CorrelationName: "Q$DUP2", Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.ResolveColumn(NewUnquoted("name"))
	var ambiguous *AmbiguousColumnError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("three matching attributes must be ambiguous: %v", err)
	}
	if err := s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u"), CorrelationName: "Q$DUP3", Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ResolveColumn(NewUnquoted("name"))
	if !errors.As(err, &ambiguous) {
		t.Fatalf("two shadowing attributes must be ambiguous: %v", err)
	}
	s2 := NewScope(nil)
	if err := s2.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("x"), CorrelationName: "X", Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	if err := s2.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("x"), CorrelationName: "Q$DUP1"}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s2.ResolveColumn(NewUnquoted("name"))
	if !errors.As(err, &ambiguous) {
		t.Fatalf("element and later table attribute must be ambiguous: %v", err)
	}
	// Identity collisions are unsafe even under distinct display aliases.
	err = s2.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("different"), CorrelationName: "x"})
	var duplicate *DuplicateAliasError
	if !errors.As(err, &duplicate) {
		t.Fatalf("duplicate runtime identity: %v", err)
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

// TestUnnestAttributeDoesNotOverrideAnotherSource matches Java's attribute-list
// lookup: a FROM element and a table column with the same name are ambiguous,
// regardless of source order or whether their display aliases also coincide.
func TestUnnestAttributeDoesNotOverrideAnotherSource(t *testing.T) {
	t.Parallel()
	for _, tableAlias := range []string{"U", "V"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("table=%s/reverse=%t", tableAlias, reverse), func(t *testing.T) {
				t.Parallel()
				sources := []ScopeSource{
					{Alias: NewUnquoted(tableAlias), CorrelationName: "TABLE", Table: &StaticTable{TableColumns: []Column{{Id: NewUnquoted("v"), Type: "BIGINT"}}}},
					{Alias: NewUnquoted("v"), CorrelationName: "ELEMENT", Shadowing: true, Table: &StaticTable{TableColumns: []Column{{Id: NewUnquoted("v"), Type: "BIGINT"}}}},
				}
				if reverse {
					sources[0], sources[1] = sources[1], sources[0]
				}
				scope := NewScope(nil)
				for _, source := range sources {
					if err := scope.AddSource(source); err != nil {
						t.Fatal(err)
					}
				}
				_, _, err := scope.ResolveColumn(NewUnquoted("v"))
				var ambiguous *AmbiguousColumnError
				if !errors.As(err, &ambiguous) {
					t.Fatalf("duplicate attribute resolved with %v, want ambiguity", err)
				}
				if tableAlias == "U" {
					_, source, _, err := scope.ResolvePathNested([]Identifier{NewUnquoted("u"), NewUnquoted("v")})
					if err != nil || source.CorrelationName != "TABLE" {
						t.Fatalf("qualified table column = %+v / %v", source, err)
					}
				}
			})
		}
	}
}
