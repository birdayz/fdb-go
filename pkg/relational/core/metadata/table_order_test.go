package metadata

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// MoveIndexedTablesToEnd replays Java's DdlVisitor (:559-564): each index
// clause, in clause order, moves its table to the end of the table order, so a
// table no index names keeps its declaration slot ahead of the indexed ones,
// which end in the order of each one's last index clause. Build then numbers
// the record type keys and registers the indexes (their versions) in that
// order. Without the call a builder keeps its caller's order.
func TestMoveIndexedTablesToEndIsJavasTableOrder(t *testing.T) {
	t.Parallel()
	build := func(move bool) *Builder {
		b := NewSchemaTemplateBuilder().SetName("order")
		for _, name := range []string{"A", "B", "C", "D"} {
			b.AddTable(name, []ColumnSpec{
				NewColumnSpec("ID", api.NewLongType(false), 1),
				NewColumnSpec("V", api.NewLongType(true), 2),
			}, []string{"ID"})
		}
		// Clauses in order: A, C, A. So B and D stay first; then C, then A
		// (its last clause is the latest).
		b.AddIndex("A", "A1", []string{"V"}, false)
		b.AddIndex("C", "C1", []string{"V"}, false)
		b.AddIndex("A", "A2", []string{"V"}, false)
		if move {
			b.MoveIndexedTablesToEnd()
		}
		return b
	}
	for _, c := range []struct {
		move    bool
		tables  []string
		indexes []string // in version order, from 2 (the template's first version is 1)
	}{
		{false, []string{"A", "B", "C", "D"}, []string{"A1", "A2", "C1"}},
		{true, []string{"B", "D", "C", "A"}, []string{"C1", "A1", "A2"}},
	} {
		b := build(c.move)
		var names []string
		for _, tbl := range b.tables {
			names = append(names, tbl.name)
		}
		if !reflect.DeepEqual(names, c.tables) {
			t.Fatalf("move=%t: table order %v, want %v", c.move, names, c.tables)
		}
		tmpl, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		md := tmpl.Underlying()
		for i, name := range c.tables {
			if got := md.GetRecordType(name).GetRecordTypeKey(); got != int64(i) {
				t.Errorf("move=%t: record type %s key %v, want %d", c.move, name, got, i)
			}
		}
		for i, name := range c.indexes {
			if got := md.GetIndex(name).AddedVersion; got != i+2 {
				t.Errorf("move=%t: index %s added at version %d, want %d", c.move, name, got, i+2)
			}
		}
	}
}
