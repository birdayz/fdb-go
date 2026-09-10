package query

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestGatedLegBox_FilterOverBoxIsTheBox pins that every seed-layout site
// classifies a gated leg by the join beneath its filters: the builder places
// a filter directly above an inner cluster when it folds that cluster's
// ON-clause EXISTS in place under an OUTER join (embedded/on_exists_fold.go),
// and the leg's row is still the cluster's concat. With the filter treated
// as a leaf the seed declared the leg as one opaque run under the rightmost
// leaf's name while the leg's own select flowed the cluster's buried legs —
// two members of one reference disagreeing on their result type.
func TestGatedLegBox_FilterOverBoxIsTheBox(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	box := inner(scan("Order", "o"), scan("Customer", "c"))
	existential, err := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier("q$e"), values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	filtered := &logical.LogicalFilter{
		Input:            box,
		Predicate:        existential,
		ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("q$e"), Plan: scan("TypedRecord", "g")}},
	}

	if got := gatedLegBox(filtered); got != box {
		t.Fatalf("gatedLegBox(Filter(box)) = %v, want the box", got)
	}
	if got := gatedLegBox(&logical.LogicalFilter{Input: filtered}); got != box {
		t.Fatal("gatedLegBox must see through stacked filters")
	}
	if gatedLegBox(scan("Order", "o")) != nil || gatedLegBox(&logical.LogicalFilter{Input: scan("Order", "o")}) != nil {
		t.Fatal("a scan, filtered or not, is not a box")
	}
	if gatedLegBox(logical.NewProject(box, []string{"o.order_id"}, []string{""})) != nil {
		t.Fatal("a projection over a join is a derived leg with its own row, not the box")
	}

	boxType := tr.ordinalLegType(box)
	filteredType := tr.ordinalLegType(filtered)
	if boxType == nil || filteredType == nil {
		t.Fatal("leg types must derive")
	}
	if !reflect.DeepEqual(boxType.Fields, filteredType.Fields) {
		t.Fatalf("fields differ:\n  box:      %v\n  filtered: %v", boxType.Fields, filteredType.Fields)
	}
	if len(boxType.Legs) != 2 || !reflect.DeepEqual(boxType.Legs, filteredType.Legs) {
		t.Fatalf("buried legs differ (want the box's two):\n  box:      %v\n  filtered: %v", boxType.Legs, filteredType.Legs)
	}
	if legBinding(filtered) != legBinding(box) || legBinding(box) != "C$BOX" {
		t.Fatalf("legBinding: filtered=%q box=%q, want both the box mint C$BOX", legBinding(filtered), legBinding(box))
	}
	boxOff, boxLeaf := tr.legBakeWindow(box)
	filteredOff, filteredLeaf := tr.legBakeWindow(filtered)
	if boxOff != filteredOff || !reflect.DeepEqual(boxLeaf, filteredLeaf) {
		t.Fatalf("legBakeWindow differs: box=(%d,%v) filtered=(%d,%v)", boxOff, boxLeaf, filteredOff, filteredLeaf)
	}
	if !reflect.DeepEqual(tr.ordinalLegColumns(box), tr.ordinalLegColumns(filtered)) {
		t.Fatal("ordinalLegColumns must be the box's concat through the filter")
	}

	// The buried bake windows an OUTER box registers for its legs are the
	// same whether the cluster leg is bare or filtered.
	outerBare := logical.NewJoinWithPredicate(box, scan("TypedRecord", "tr"), logical.JoinLeft, nil)
	outerFiltered := logical.NewJoinWithPredicate(filtered, scan("TypedRecord", "tr"), logical.JoinLeft, nil)
	bare := tr.gatedJoinLegTypes(outerBare)
	viaFilter := tr.gatedJoinLegTypes(outerFiltered)
	if len(bare) == 0 || !reflect.DeepEqual(bare, viaFilter) {
		t.Fatalf("gatedJoinLegTypes differ:\n  bare:     %v\n  filtered: %v", bare, viaFilter)
	}
	for _, buried := range []string{"O", "C"} {
		if _, ok := viaFilter[buried]; !ok {
			t.Fatalf("buried leg %s must have a bake window through the filter; got %v", buried, viaFilter)
		}
	}
}
