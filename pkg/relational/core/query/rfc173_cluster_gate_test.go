package query

// RFC-173 Slice 2 W2 — pins for the translation-time cluster-arity scoping
// gate (implementation contract ruling #1). Two layers:
//
//   - clusterArity walk pins: the post-flattening arity computation per
//     logical shape, including the flattening-evasion shape (contract gate
//     pin (b)) and the poison classes;
//   - gate-decision pins driven through REAL translation
//     (TranslateToCascadesWithError over catalog metadata), proving the
//     enclosure flag classifies nested joins by their post-flattening
//     position — the walk alone cannot see upward.
//
// The gate is DARK in W2 (decisions recorded, unconsumed until W3), so these
// pins are pure planner-side and need no FDB.

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func newGateTranslator(t *testing.T) *cascadesTranslator {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return &cascadesTranslator{
		md:              md,
		cteScope:        make(map[string]logical.LogicalOperator),
		cteExprScope:    make(map[string]expressions.RelationalExpression),
		cteColumnsScope: make(map[string][]values.Field),
	}
}

func scan(table, alias string) *logical.LogicalScan { return logical.NewScan(table, alias) }

func inner(l, r logical.LogicalOperator) *logical.LogicalJoin {
	return logical.NewJoin(l, r, logical.JoinInner, "")
}

func TestRFC173S2_ClusterArity_Shapes(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	cases := []struct {
		name string
		op   logical.LogicalOperator
		want int
	}{
		{"scan", scan("Order", "o"), 1},
		{"two_way", inner(scan("Order", "o"), scan("Customer", "c")), 2},
		{"three_way_left_deep", inner(inner(scan("Order", "o"), scan("Customer", "c")), scan("TypedRecord", "t")), 3},
		{"three_way_right_deep", inner(scan("Order", "o"), inner(scan("Customer", "c"), scan("TypedRecord", "t"))), 3},
		{"outer_join_opaque", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), 1},
		{"outer_box_plus_scan", inner(logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), scan("TypedRecord", "t")), 2},
		{"filter_transparent", inner(logical.NewFilter(scan("Order", "o"), "x > 1"), scan("Customer", "c")), 2},
		{"project_transparent", inner(logical.NewProject(scan("Order", "o"), []string{"order_id"}, []string{""}), scan("Customer", "c")), 2},
		{"aggregate_opaque", inner(logical.NewAggregate(inner(scan("Order", "o"), scan("Customer", "c")), []string{"x"}, nil, nil, ""), scan("TypedRecord", "t")), 2},
		{"distinct_opaque", logical.NewDistinct(scan("Order", "o")), 1},
		{"sort_opaque", inner(logical.NewSort(inner(scan("Order", "o"), scan("Customer", "c")), nil), scan("TypedRecord", "t")), 2},
		{"union_opaque", logical.NewUnion([]logical.LogicalOperator{scan("Order", "o"), scan("Customer", "c")}, false), 1},
		{"filter_with_exists_poison", logical.LogicalOperator(&logical.LogicalFilter{
			Input:            scan("Order", "o"),
			ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("e")}},
		}), arityPoison},
		{"project_with_scalar_poison", logical.LogicalOperator(&logical.LogicalProject{
			Input:            scan("Order", "o"),
			Projections:      []string{"order_id"},
			ScalarSubqueries: []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("s")}},
		}), arityPoison},
	}
	for _, tc := range cases {
		if got := tr.clusterArity(tc.op); got != tc.want {
			t.Errorf("clusterArity(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestRFC173S2_ClusterArity_FlatteningEvasion is contract gate pin (b): the
// flattening-evasion shape `FROM (a JOIN b) t1, (c JOIN d) t2` is 2-way at
// translation but 4-way post-flattening (SelectMergeRule merges the derived
// bodies up). The walk must see THROUGH the cteScope-registered bodies.
func TestRFC173S2_ClusterArity_FlatteningEvasion(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	// Derived tables register their body in cteScope (translateCTE); model the
	// scope directly the way translation has it when the outer join is walked.
	tr.cteScope["T1"] = inner(scan("Order", "o"), scan("Customer", "c"))
	tr.cteScope["T2"] = inner(scan("TypedRecord", "t"), scan("Order", "o2"))

	evasion := inner(scan("t1", "t1"), scan("t2", "t2"))
	if got := tr.clusterArity(evasion); got != 4 {
		t.Fatalf("flattening-evasion cluster arity = %d, want 4 (2-way at translation, 4-way post-flattening)", got)
	}
	d := tr.ordinalWedgeGateDecide(evasion)
	if d.Gated {
		t.Fatalf("flattening-evasion shape must NOT gate ordinal: %+v", d)
	}

	// Control: ONE derived join consumed alone stays a maximal 2-way cluster.
	solo := scan("t1", "t1")
	if got := tr.clusterArity(solo); got != 2 {
		t.Fatalf("solo derived-join scan arity = %d, want 2", got)
	}

	// The scope must be restored after the walk (remove-while-deriving).
	if _, ok := tr.cteScope["T1"]; !ok {
		t.Fatal("clusterArity leaked the cteScope removal")
	}
}

// TestRFC173S2_WedgeGate_Translation drives gate decisions through REAL
// translation, pinning the enclosure classification the arity walk cannot see
// from a join looking upward:
//   - a maximal 2-way inner join gates ordinal;
//   - the SAME 2-way join nested under a 3-way inner cluster does NOT
//     (contract gate pin (a) precondition: it must remain name-model);
//   - a 2-way inner join under an OUTER-join leg roots a fresh cluster and
//     gates; the outer box itself gates (binary opaque, ruling #3);
//   - a 2-way join inside an aggregate (derived GROUP BY) roots a fresh
//     cluster and gates — the §5 GROUP-BY-over-2-way pin's planner half;
//   - a 2-way join under a WHERE-EXISTS outer leg is enclosed (existential
//     flatten) and does NOT gate, while a 2-way join INSIDE the EXISTS
//     subquery roots a fresh cluster and DOES.
func TestRFC173S2_WedgeGate_Translation(t *testing.T) {
	t.Parallel()

	// run translates root with a fresh md-backed translator and returns the
	// gate decision recorded for j's seed.
	run := func(t *testing.T, root logical.LogicalOperator, j *logical.LogicalJoin) wedgeGateDecision {
		t.Helper()
		tr := newGateTranslator(t)
		tr.translateRef(root)
		d, ok := tr.wedgeGate[j]
		if !ok {
			t.Fatalf("no gate decision recorded for the join (translation never reached its seed)")
		}
		return d
	}

	t.Run("maximal_two_way_gates", func(t *testing.T) {
		t.Parallel()
		j := inner(scan("Order", "o"), scan("Customer", "c"))
		if d := run(t, j, j); !d.Gated || d.Arity != 2 {
			t.Fatalf("maximal 2-way: %+v, want gated arity 2", d)
		}
	})

	t.Run("two_way_under_three_way_does_not", func(t *testing.T) {
		t.Parallel()
		nested := inner(scan("Order", "o"), scan("Customer", "c"))
		root := inner(nested, scan("TypedRecord", "t"))
		if d := run(t, root, nested); d.Gated {
			t.Fatalf("2-way nested under 3-way cluster must stay name-model: %+v", d)
		}
		// The outer 3-way seed itself must not gate either.
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || d.Gated {
			t.Fatalf("3-way root: %+v (ok=%v), want recorded and not gated", d, ok)
		}
	})

	t.Run("outer_box_and_fresh_cluster_under_it", func(t *testing.T) {
		t.Parallel()
		nested := inner(scan("Customer", "c"), scan("TypedRecord", "t"))
		root := logical.NewJoin(scan("Order", "o"), nested, logical.JoinLeft, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated {
			t.Fatalf("outer-join box: %+v (ok=%v), want gated (binary opaque, ruling #3)", d, ok)
		}
		if d, ok := tr.wedgeGate[nested]; !ok || !d.Gated {
			t.Fatalf("2-way under an outer leg roots a fresh cluster: %+v (ok=%v), want gated", d, ok)
		}
	})

	t.Run("aggregate_boundary_roots_fresh_cluster", func(t *testing.T) {
		t.Parallel()
		nested := inner(scan("Order", "o"), scan("Customer", "c"))
		agg := logical.NewAggregate(nested, []string{"o.order_id"}, []string{"COUNT(*)"}, []string{"cnt"}, "")
		root := inner(agg, scan("TypedRecord", "t"))
		if d := run(t, root, nested); !d.Gated || d.Arity != 2 {
			t.Fatalf("2-way under aggregate under a join: %+v, want gated arity 2 (aggregate is an opaque boundary)", d)
		}
	})

	t.Run("exists_outer_leg_enclosed_subquery_fresh", func(t *testing.T) {
		t.Parallel()
		// Outer leg: derived 2-way join. Subquery: its own 2-way join.
		outerJoin := inner(scan("Order", "o"), scan("Customer", "c"))
		subJoin := inner(scan("TypedRecord", "t"), scan("Order", "o2"))
		f := &logical.LogicalFilter{
			Input:            outerJoin,
			ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("e"), Plan: subJoin}},
		}
		tr := newGateTranslator(t)
		tr.translateRef(f)
		// The join+EXISTS flatten (translateJoinWithExists) encloses BOTH
		// ForEach legs — outerJoin here IS the flattened select's leg pair, so
		// its seed decision must be name-model? No: outerJoin is the join
		// being flattened WITH the existential — translateJoinWithExists
		// translates its legs directly and never seeds outerJoin itself. What
		// must hold: the SUBQUERY join roots a fresh cluster and gates.
		if d, ok := tr.wedgeGate[subJoin]; !ok || !d.Gated {
			t.Fatalf("2-way join inside an EXISTS subquery: %+v (ok=%v), want gated (fresh cluster)", d, ok)
		}
		// And if outerJoin was seeded at all (it is not, on the flatten path),
		// it must not have gated.
		if d, ok := tr.wedgeGate[outerJoin]; ok && d.Gated {
			t.Fatalf("join flattened with an existential must not gate: %+v", d)
		}
	})

	t.Run("derived_join_leg_under_exists_filter_enclosed", func(t *testing.T) {
		t.Parallel()
		// Non-join outer input carrying EXISTS: a derived table whose body is
		// a 2-way join. The generic buildExistentialSelect path encloses the
		// leg, so the body join must NOT gate.
		bodyJoin := inner(scan("Order", "o"), scan("Customer", "c"))
		sub := scan("TypedRecord", "t")
		cte := logical.NewCTE("d", bodyJoin, &logical.LogicalFilter{
			Input:            scan("d", "d"),
			ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("e"), Plan: sub}},
		}, false)
		tr := newGateTranslator(t)
		tr.translateRef(cte)
		if d, ok := tr.wedgeGate[bodyJoin]; !ok || d.Gated {
			t.Fatalf("derived join leg of a WHERE-EXISTS select: %+v (ok=%v), want recorded and NOT gated (enclosed by the existential flatten)", d, ok)
		}
	})
}
