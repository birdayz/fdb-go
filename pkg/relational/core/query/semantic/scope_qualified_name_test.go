package semantic

import (
	"errors"
	"testing"
)

// TestScopeSourceNamedByItsQualifiedPath pins a source named by a
// template-qualified identifier (NamePath), as Java names an unaliased table
// operator `FROM T.W` by the whole identifier: `T.W.col` qualifies its columns,
// `W.col` and `W.*` name nothing, the doubled qualifier prepends the name's
// last segment to the whole name (`W.T.W.col`), and a descent goes through the
// path. Each shape is measured against the target, over table y (id, a struct
// column h) named `{T}.y`, in
// conformance/ws_f_table_qualifier_conformance_test.go.
func TestScopeSourceNamedByItsQualifiedPath(t *testing.T) {
	t.Parallel()
	table := &StaticTable{
		TableName: ParseQualifiedName("w", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("s"), Type: "RECORD", StructFields: []Column{{Id: NewUnquoted("f"), Type: "BIGINT"}}},
		},
	}
	scope := NewScope(nil)
	source := ScopeSource{Table: table, Alias: NewUnquoted("w"), NamePath: []Identifier{NewUnquoted("t"), NewUnquoted("w")}, CorrelationName: "W"}
	if err := scope.AddSource(source); err != nil {
		t.Fatal(err)
	}
	if source.NamedBy(NewUnquoted("w")) || source.NamedBy(NewUnquoted("t")) {
		t.Error("a source named by a qualified path is named by one of its segments")
	}
	for _, c := range []struct {
		name     string
		path     []Identifier
		column   string
		accessed int
	}{
		{"the whole name", segs("t", "w", "id"), "ID", 0},
		{"a descent through the whole name", segs("t", "w", "s", "f"), "S", 1},
		{"the doubled qualifier", segs("w", "t", "w", "id"), "ID", 0},
		{"a bare column", segs("id"), "ID", 0},
		{"a struct-relative descent", segs("s", "f"), "S", 1},
	} {
		col, _, accessors, err := scope.ResolvePathNested(c.path)
		if err != nil || col.Id.Name() != c.column || len(accessors) != c.accessed {
			t.Errorf("%s: %+v, %v, %v; want %s with %d accessors", c.name, col, accessors, err, c.column, c.accessed)
		}
	}
	for _, c := range []struct {
		name string
		path []Identifier
	}{
		{"the name's last segment", segs("w", "id")},
		{"the qualifier alone", segs("t", "id")},
		{"the doubled qualifier through a nested path", segs("w", "t", "w", "s", "f")},
	} {
		var notFound *SourceNotFoundError
		var noColumn *ColumnNotFoundError
		if _, _, _, err := scope.ResolvePathNested(c.path); !errors.As(err, &notFound) && !errors.As(err, &noColumn) {
			t.Errorf("%s: %v, want the reference refused", c.name, err)
		}
	}
	if _, err := NewAnalyzer(NewInMemoryCatalog(), false).ExpandQualifiedStar(scope, NewUnquoted("w")); err == nil {
		t.Error("w.* expanded a source named T.W")
	}
}

// TestScopePathQualifiedLookupFirst pins Java's two lookups
// (SemanticAnalyzer.resolveIdentifierMaybe): the qualified readings first, the
// struct-relative one only when they find nothing, so `x.f` over a table x
// with a column f and a struct column x holding f is the table's f (measured:
// `SELECT x.f FROM x`); both qualified readings together are ambiguous
// (`x.x.f`: the struct's f, and x's f by the doubled qualifier; measured
// 42702); and the declared case fold takes both kinds together, so a fold
// reaching a field both ways is ambiguous.
func TestScopePathQualifiedLookupFirst(t *testing.T) {
	t.Parallel()
	table := &StaticTable{
		TableName: ParseQualifiedName("x", false),
		TableColumns: []Column{
			{Id: NewUnquoted("f"), Type: "BIGINT"},
			{Id: NewUnquoted("x"), Type: "RECORD", StructFields: []Column{{Id: NewUnquoted("f"), Type: "STRING"}}},
		},
	}
	scope := NewScope(nil)
	if err := scope.AddSource(ScopeSource{Table: table, Alias: NewUnquoted("x"), CorrelationName: "X"}); err != nil {
		t.Fatal(err)
	}
	if col, _, accessors, err := scope.ResolvePathNested(segs("x", "f")); err != nil || col.Type != "BIGINT" || len(accessors) != 0 {
		t.Errorf("x.f = %+v, %v, %v; want the table's BIGINT f", col, accessors, err)
	}
	var ambiguous *AmbiguousColumnError
	if _, _, _, err := scope.ResolvePathNested(segs("x", "x", "f")); !errors.As(err, &ambiguous) {
		t.Errorf("x.x.f: %v, want ambiguous (the struct's f and the doubled qualifier's f)", err)
	}
	// Quoted lower case: no exact reading; folded, both the qualified reading
	// (x's F) and the struct-relative one (the struct x's F) answer.
	if _, _, _, err := scope.ResolvePathNested([]Identifier{NewUnquoted("x"), New(`"f"`, false)}); !errors.As(err, &ambiguous) {
		t.Errorf(`x."f": %v, want ambiguous (both readings fold to a field)`, err)
	}
}
