package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestHoistInterleavedSpineLinks pins the FROM-order normalization ahead of
// the spine machinery: a link joins its owner link across an unrelated item, a
// source owning a later unnest joins the bottom box ahead of another table's
// links, a source that reads an element it would pass stays where it is, and
// an already-adjacent FROM is left alone.
func TestHoistInterleavedSpineLinks(t *testing.T) {
	t.Parallel()
	order := func(op logical.LogicalOperator) string {
		var names []string
		for {
			j, isJoin := op.(*logical.LogicalJoin)
			if !isJoin {
				names = append([]string{sourceAlias(op)}, names...)
				return strings.Join(names, ",")
			}
			names = append([]string{sourceAlias(j.Right)}, names...)
			op = j.Left
		}
	}
	hoist := func(t *testing.T, j *logical.LogicalJoin, want string) {
		t.Helper()
		got, moved := hoistInterleavedSpineLinks(j)
		if want == "" {
			if moved {
				t.Fatalf("%s moved to %s, want it left alone", order(j), order(got))
			}
			return
		}
		if !moved {
			t.Fatalf("%s was not moved, want %s", order(j), want)
		}
		if order(got) != want {
			t.Fatalf("%s moved to %s, want %s", order(j), order(got), want)
		}
	}

	// FROM T4, T4.SARR AS X, T4 AS H, X.SUBSTRUCT AS Y
	l1, _ := link(t, scan("T4", "T4"), "T4", "SARR", "X")
	sep, _ := link(t, inner(l1, scan("T4", "H")), "X", "SUBSTRUCT", "Y")
	hoist(t, sep, "T4,X,Y,H")

	// FROM T4, T4.SARR AS X, X.SUBSTRUCT AS Y, T4 AS H: already adjacent.
	adj, _ := link(t, l1, "X", "SUBSTRUCT", "Y")
	hoist(t, inner(adj, scan("T4", "H")), "")

	// FROM T4, T4.SARR AS X, T4 AS H, H.SARR AS V
	second, _ := link(t, inner(l1, scan("T4", "H")), "H", "SARR", "V")
	hoist(t, second, "T4,H,X,V")

	// The same, H reading X's element: it may not pass X.
	elem := &values.RecordType{Fields: []values.Field{{Name: "K", Ordinal: 0, FieldType: values.NullableLong}}}
	x, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("X"), elem)
	if err != nil {
		t.Fatal(err)
	}
	readsX := &logical.LogicalFilter{Input: scan("T4", "H"), Predicate: predicates.NewValuePredicate(x)}
	lateral, _ := link(t, inner(l1, readsX), "H", "SARR", "V")
	hoist(t, lateral, "")

	// D reads H, which reads X: D must not pass H on its way to the bottom.
	h, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("H"), elem)
	if err != nil {
		t.Fatal(err)
	}
	readsH := &logical.LogicalFilter{Input: scan("T4", "D"), Predicate: predicates.NewValuePredicate(h)}
	indirect, _ := link(t, inner(inner(l1, readsX), readsH), "D", "SARR", "V")
	hoist(t, indirect, "")

	// The same dependency when the first FROM item is itself an unnest.
	standalone := l1.Right.(*logical.LogicalUnnest)
	firstItem, _ := link(t, inner(inner(standalone, readsX), readsH), "D", "SARR", "V")
	hoist(t, firstItem, "")

	// H precedes the crossed links: D can move without moving ahead of H.
	before, _ := link(t, inner(scan("T4", "T4"), scan("T4", "H")), "T4", "SARR", "X")
	valid, _ := link(t, inner(before, readsH), "D", "SARR", "V")
	hoist(t, valid, "T4,H,D,X,V")
}
