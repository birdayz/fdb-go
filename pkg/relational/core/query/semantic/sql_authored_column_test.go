package semantic

import (
	"errors"
	"testing"
)

// The relaxed pass folds a descriptor's spelling only. A column whose name the
// statement authored (Column.SQLAuthored) answers its exact spelling alone, in
// the unqualified and the qualified reading (both go through matchesColumn);
// a descriptor column of the same source keeps the fold.
func TestRelaxedPassSkipsSQLAuthoredColumns(t *testing.T) {
	t.Parallel()
	scope := NewScope(nil)
	if err := scope.AddSource(ScopeSource{Table: &StaticTable{
		TableName: ParseQualifiedName("d", false),
		TableColumns: []Column{
			{Id: New("e", true), Type: "BIGINT", SQLAuthored: true},
			{Id: New("keep", true), Type: "BIGINT"},
		},
	}, Alias: NewUnquoted("d"), CorrelationName: "D"}); err != nil {
		t.Fatal(err)
	}
	var notFound *ColumnNotFoundError
	if _, _, err := scope.ResolveColumn(NewUnquoted("e")); !errors.As(err, &notFound) {
		t.Fatalf("unquoted E over authored \"e\": %v, want not found", err)
	}
	if _, _, _, err := scope.ResolvePathNested(segs("d", "e")); !errors.As(err, &notFound) {
		t.Fatalf("D.E over authored \"e\": %v, want not found", err)
	}
	if c, _, err := scope.ResolveColumn(New("e", true)); err != nil || c.Id.Name() != "e" {
		t.Fatalf("exact \"e\": %v, %v", c.Id.Name(), err)
	}
	if c, _, err := scope.ResolveColumn(NewUnquoted("keep")); err != nil || c.Id.Name() != "keep" {
		t.Fatalf("unquoted KEEP over descriptor keep: %v, %v; want the fold", c.Id.Name(), err)
	}
	if _, ok := LookupColumnRelaxed(scope.Sources()[0].Table, NewUnquoted("e")); ok {
		t.Fatal("LookupColumnRelaxed folded an authored name")
	}

	// The collision the fold must REPORT: two descriptor columns that differ
	// only by case, and a reference matching neither exactly, are two
	// candidates, 42702, never a first match. One of the two authored leaves a
	// single descriptor candidate, which answers.
	collide := func(authored bool) *Scope {
		s := NewScope(nil)
		if err := s.AddSource(ScopeSource{Table: &StaticTable{
			TableName: ParseQualifiedName("q", false),
			TableColumns: []Column{
				{Id: New("kA", true), Type: "BIGINT", SQLAuthored: authored},
				{Id: New("Ka", true), Type: "BIGINT"},
			},
		}, Alias: NewUnquoted("q"), CorrelationName: "Q"}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	var ambiguous *AmbiguousColumnError
	if _, _, err := collide(false).ResolveColumn(New("ka", true)); !errors.As(err, &ambiguous) || ambiguous.Matches != 2 {
		t.Fatalf("two descriptor case-variants: %v, want ambiguous over 2", err)
	}
	if c, _, err := collide(true).ResolveColumn(New("ka", true)); err != nil || c.Id.Name() != "Ka" {
		t.Fatalf("one authored, one descriptor variant: %v, %v; want the descriptor Ka", c.Id.Name(), err)
	}
}
