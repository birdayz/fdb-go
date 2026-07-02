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
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
		// LEFT OUTER: POISON — RewriteOuterJoinRule dissolves the box into an
		// INNER + null-on-empty select during REWRITING, so translation-time
		// opacity is a false premise (the W3b flip's live catch). FULL OUTER
		// is the genuinely opaque box (never rewritten, never merged).
		{"left_outer_poison", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), arityPoison},
		{"left_outer_box_poisons_cluster", inner(logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), scan("TypedRecord", "t")), arityPoison},
		{"full_outer_opaque", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, ""), 1},
		{"full_outer_box_plus_scan", inner(logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, ""), scan("TypedRecord", "t")), 2},
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

	t.Run("left_outer_not_gated_null_leg_fresh_preserved_enclosed", func(t *testing.T) {
		t.Parallel()
		// The W3b premise correction: a LEFT box is dissolved by
		// RewriteOuterJoinRule during REWRITING, so (a) the box itself must
		// NOT gate; (b) its PRESERVED (left) leg flattens into the rewritten
		// select → a join there is ENCLOSED (name-model); (c) its
		// NULL-SUPPLYING (right) leg becomes the never-merged null-on-empty
		// subselect → a join there roots a FRESH cluster and gates.
		preserved := inner(scan("Customer", "c"), scan("TypedRecord", "t"))
		root := logical.NewJoin(preserved, scan("Order", "o"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || d.Gated {
			t.Fatalf("LEFT-outer box: %+v (ok=%v), want recorded and NOT gated (dissolved by RewriteOuterJoinRule)", d, ok)
		}
		if d, ok := tr.wedgeGate[preserved]; !ok || d.Gated {
			t.Fatalf("2-way in the PRESERVED leg: %+v (ok=%v), want NOT gated (flattens into the rewritten select)", d, ok)
		}

		nullSide := inner(scan("Customer", "c2"), scan("TypedRecord", "t2"))
		root2 := logical.NewJoin(scan("Order", "o2"), nullSide, logical.JoinLeft, "")
		tr2 := newGateTranslator(t)
		tr2.translateRef(root2)
		if d, ok := tr2.wedgeGate[nullSide]; !ok || !d.Gated {
			t.Fatalf("2-way in the NULL-SUPPLYING leg: %+v (ok=%v), want gated (fresh cluster — the null-on-empty subselect is never merged)", d, ok)
		}
	})

	t.Run("full_outer_box_gates_scan_legs_only", func(t *testing.T) {
		t.Parallel()
		// FULL OUTER over SCAN legs is the genuinely opaque box: never
		// rewritten, never merged — it gates (ruling #3's FULL drain wired).
		root := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated {
			t.Fatalf("FULL-outer box over scans: %+v (ok=%v), want gated (genuinely opaque)", d, ok)
		}
	})

	t.Run("join_legs_ineligible_nested_still_gates", func(t *testing.T) {
		t.Parallel()
		// ANY join leg makes the parent ineligible in the S2 wedge — a
		// nested ordinal box's bare concat ERASES buried aliases (an upper
		// `a.id` through `(a JOIN b) FULL JOIN c` has no span to resolve
		// against; nesting is S3's collapsed-FieldPath work — contract
		// ruling #2: S2 is single-accessor only). The nested 2-way itself
		// still gates (fresh cluster under the FULL leg) and is consumed as
		// a name-model leg through its dual-emitted Datum.
		nested := inner(scan("Customer", "c"), scan("TypedRecord", "t"))
		root := logical.NewJoin(scan("Order", "o"), nested, logical.JoinFull, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || d.Gated {
			t.Fatalf("FULL box with a JOIN leg: %+v (ok=%v), want NOT gated (join legs are S3 territory)", d, ok)
		}
		if d, ok := tr.wedgeGate[nested]; !ok || !d.Gated {
			t.Fatalf("2-way under a FULL leg roots a fresh cluster: %+v (ok=%v), want gated", d, ok)
		}
	})

	t.Run("rfc153_joined_preserved_stays_name_model", func(t *testing.T) {
		t.Parallel()
		// Graefe re-ruling condition 1: the EXACT shape whose live fallout
		// broke the LEFT-outer opacity premise — `a JOIN b LEFT JOIN c` (the
		// RFC-153 joined-preserved family) — pinned name-model END TO END at
		// the gate: the LEFT box does not gate (dissolved by
		// RewriteOuterJoinRule post-translation), and the inner(a,b) in its
		// PRESERVED leg does not gate either (it flattens into the rewritten
		// select — the machinery the drift assert caught it inside).
		ab := inner(scan("Order", "o"), scan("Customer", "c"))
		root := logical.NewJoin(ab, scan("TypedRecord", "t"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || d.Gated {
			t.Fatalf("joined-preserved LEFT box: %+v (ok=%v), want recorded and NOT gated", d, ok)
		}
		if d, ok := tr.wedgeGate[ab]; !ok || d.Gated {
			t.Fatalf("joined-preserved inner(a,b): %+v (ok=%v), want recorded and NOT gated (preserved leg flattens post-rewrite)", d, ok)
		}
	})

	t.Run("mixed_nesting_leg_ineligible", func(t *testing.T) {
		t.Parallel()
		// `(A JOIN B JOIN C) FULL JOIN D`: the FULL box's left leg is a
		// name-model 3-way — the box must NOT gate (an ordinal seed cannot
		// type a name-model merged-row leg; mixed nesting stays name-model
		// until S3). Caught live by ordinalLegColumns' mis-scope panic.
		threeWay := inner(inner(scan("Order", "o"), scan("Customer", "c")), scan("TypedRecord", "t"))
		root := logical.NewJoin(threeWay, scan("Order", "o2"), logical.JoinFull, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || d.Gated {
			t.Fatalf("FULL box over a name-model 3-way leg: %+v (ok=%v), want NOT gated (leg ineligible)", d, ok)
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

	t.Run("having_exists_untranslatable", func(t *testing.T) {
		t.Parallel()
		// HAVING-EXISTS over a 2-way join input (Graefe W2 matrix addition):
		// translateAggregate REJECTS HavingExistsSubqueries entirely (Java has
		// no support either), so no expression exists for the existential
		// quantifiers to land in — the drift assert is unreachable from this
		// shape by construction. The nested join is seeded (and gated: the
		// aggregate boundary roots a fresh cluster) before the rejection, but
		// the plan dies with it.
		nested := inner(scan("Order", "o"), scan("Customer", "c"))
		agg := logical.NewAggregate(nested, []string{"o.order_id"}, []string{"COUNT(*)"}, []string{"cnt"}, "")
		agg.HavingPredicate = &predicates.ComparisonPredicate{
			Operand:    &values.FieldValue{Field: "CNT"},
			Comparison: predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: &values.ConstantValue{Value: int64(0)}},
		}
		agg.HavingExistsSubqueries = []logical.ExistsSubquery{
			{Alias: values.NamedCorrelationIdentifier("e"), Plan: scan("TypedRecord", "t")},
		}
		tr := newGateTranslator(t)
		if ref := tr.translateRef(agg); ref != nil {
			t.Fatal("HAVING-EXISTS must be untranslatable (Java parity) — a plan here would put existential quantifiers above the aggregate with no gate coverage")
		}
	})
}
