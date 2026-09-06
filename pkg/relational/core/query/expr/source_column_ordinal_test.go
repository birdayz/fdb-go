package expr

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/semantic"
)

// A source that states a flowed layout parallel to its SQL columns is
// resolved by POSITION: the SQL name selects the slot, and the layout names
// it as the plan flows it (a repeated bare leaf under its qualified datum
// key). A source whose layout is wider than its SQL list — the AT-only
// WITH ORDINALITY element in slot 0 — is resolved by name in that layout,
// which carries the exposed name. The two shapes are the two branches of
// sourceColumnOrdinal, and each is pinned with the value the other would get
// wrong.
func TestSourceColumnOrdinalResolvesByPositionOrByExposedName(t *testing.T) {
	t.Parallel()
	column := func(name string) semantic.Column {
		return semantic.Column{Id: semantic.FromNormalized(name), Type: "BIGINT", Nullable: true}
	}
	parallel := semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{"U"}, false),
			TableColumns: []semantic.Column{column("G"), column("G"), column("W")},
		},
		Alias:           semantic.FromNormalized("U"),
		CorrelationName: "U",
		FlowedColumns:   []semantic.Column{column("GA.G"), column("G"), column("W")},
	}
	ordinal, row, ok := sourceColumnOrdinal(parallel, "W")
	if !ok || ordinal != 2 || row == nil || row.Fields[ordinal].Name != "W" {
		t.Fatalf("parallel layout: W = (%d, %v, %v), want slot 2 of the flowed row", ordinal, row, ok)
	}
	if _, _, found := sourceColumnOrdinal(parallel, "GA.G"); found {
		t.Fatal("parallel layout: the flowed name GA.G is not an SQL name and must not resolve by name")
	}
	wider := semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{"O"}, false),
			TableColumns: []semantic.Column{column("ORD")},
		},
		Alias:           semantic.FromNormalized("O"),
		CorrelationName: "O",
		Shadowing:       true,
		FlowedColumns:   []semantic.Column{column("ELEMENT"), column("ORD")},
	}
	ordinal, row, ok = sourceColumnOrdinal(wider, "ORD")
	if !ok || ordinal != 1 || row == nil || row.Fields[ordinal].Name != "ORD" {
		t.Fatalf("wider layout: ORD = (%d, %v, %v), want slot 1 of the flowed row", ordinal, row, ok)
	}
}
