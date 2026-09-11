package query

import (
	"reflect"
	"strings"
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

// TestGatedLegBox_ConsumersSeeThroughTheFilter pins the three sites outside
// the seed that classify a leg as a box — the clustered-scalar pull-up's
// spans, the exact output labels, and the projected-EXISTS sort source — and
// the positional-box gate, all against a filtered inner cluster under an
// OUTER join: each must see the cluster, not one opaque leg.
func TestGatedLegBox_ConsumersSeeThroughTheFilter(t *testing.T) {
	t.Parallel()
	// A translator carries mutable enclosure state (inInnerCluster), so each
	// parallel subtest builds its own; sharing one across them is a data race
	// between ordinalLegColumns and gatesAsFreshCluster.
	existential, err := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier("q$e"), values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	mk := func() (*logical.LogicalJoin, *logical.LogicalFilter, *logical.LogicalJoin) {
		box := inner(scan("Order", "o"), scan("Customer", "c"))
		filtered := &logical.LogicalFilter{
			Input:            box,
			Predicate:        existential,
			ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("q$e"), Plan: scan("TypedRecord", "g")}},
		}
		return box, filtered, logical.NewJoinWithPredicate(filtered, scan("TypedRecord", "tr"), logical.JoinLeft, nil)
	}

	t.Run("cluster_pull_up_spans_the_buried_legs", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		box, _, outerFiltered := mk()
		outerBare := logical.NewJoinWithPredicate(box, scan("TypedRecord", "tr"), logical.JoinLeft, nil)
		bare := tr.buildClusterPullUp(outerBare)
		viaFilter := tr.buildClusterPullUp(outerFiltered)
		if bare == nil || viaFilter == nil {
			t.Fatalf("pull-ups must build: bare=%v filtered=%v", bare != nil, viaFilter != nil)
		}
		if !reflect.DeepEqual(bare.legs, viaFilter.legs) {
			t.Fatalf("spans differ:\n  bare:     %+v\n  filtered: %+v", bare.legs, viaFilter.legs)
		}
		for _, buried := range []string{"O", "C", "TR"} {
			if _, ok := viaFilter.legByBinding[buried]; !ok {
				t.Fatalf("leg %s must have a span through the filter; got %v", buried, viaFilter.legByBinding)
			}
		}
		if _, ok := viaFilter.legByBinding["C$BOX"]; ok {
			t.Fatal("the box mint must not be a span of its own: the buried legs are the reference names")
		}
	})

	t.Run("exact_leg_labels_are_the_cluster_s_unqualified_labels", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		box, filtered, _ := mk()
		bare, err := exactLogicalLegLabels(box, tr.md, nil)
		if err != nil {
			t.Fatal(err)
		}
		viaFilter, err := exactLogicalLegLabels(filtered, tr.md, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(bare) == 0 || !reflect.DeepEqual(bare, viaFilter) {
			t.Fatalf("labels differ:\n  bare:     %v\n  filtered: %v", bare, viaFilter)
		}
		for _, l := range viaFilter {
			if strings.Contains(l, ".") {
				t.Fatalf("a buried column must be labelled unqualified, got %q in %v", l, viaFilter)
			}
		}
	})

	t.Run("sort_source_collects_the_buried_bindings", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		box, _, outerFiltered := mk()
		outerBare := logical.NewJoinWithPredicate(box, scan("TypedRecord", "tr"), logical.JoinLeft, nil)
		rootBare := inner(outerBare, scan("Order", "o2"))
		rootFiltered := inner(outerFiltered, scan("Order", "o2"))
		bare := tr.classifySortSource(rootBare)
		viaFilter := tr.classifySortSource(rootFiltered)
		if !bare.isJoin || !viaFilter.isJoin || !reflect.DeepEqual(bare.legAliases, viaFilter.legAliases) {
			t.Fatalf("sort-source leg aliases differ:\n  bare:     %v\n  filtered: %v", bare.legAliases, viaFilter.legAliases)
		}
		if !reflect.DeepEqual(viaFilter.legAliases, []string{"O", "C", "TR", "O2"}) {
			t.Fatalf("leg aliases = %v, want the buried O and C, then TR and O2", viaFilter.legAliases)
		}
	})

	t.Run("positional_box_gate_admits_a_filtered_cluster_leg_and_still_excludes_a_projected_one", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		box, filtered, _ := mk()
		full := logical.NewJoinWithPredicate(filtered, scan("TypedRecord", "tr"), logical.JoinFull, nil)
		if !tr.boxGatesFresh(full) {
			t.Fatal("a FULL box over a FILTERED inner cluster leg must gate positional: the leg is windowed through gatedLegBox")
		}
		if legExposesBuriedOuterBox(filtered) {
			t.Fatal("a filtered inner cluster is not a wrapped join")
		}
		projected := logical.NewProject(box, []string{"o.order_id"}, []string{""})
		if !legExposesBuriedOuterBox(projected) {
			t.Fatal("a PROJECT-wrapped join is still excluded: gatedLegBox peels filters only")
		}
		if !hasWrappedBuriedJoin(inner(projected, scan("Customer", "c2"))) {
			t.Fatal("a projected join buried inside a cluster is still found")
		}
		if hasWrappedBuriedJoin(inner(filtered, scan("Customer", "c2"))) {
			t.Fatal("a filtered join buried inside a cluster is windowed, not wrapped")
		}
	})
}
