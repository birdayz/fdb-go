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
	"strings"
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
		// LEFT OUTER (item-3 amendment A): the dissolved select merges as
		// preserved + the null-on-empty quantifier — arity = preserved + 1.
		// FULL OUTER stays the genuinely opaque box (never rewritten/merged).
		{"left_outer_preserved_plus_one", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), 2},
		{"left_outer_box_in_cluster", inner(logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), scan("TypedRecord", "t")), 3},
		{"full_outer_opaque", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, ""), 1},
		{"full_outer_box_plus_scan", inner(logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, ""), scan("TypedRecord", "t")), 2},
		{"filter_transparent", inner(logical.NewFilter(scan("Order", "o"), "x > 1"), scan("Customer", "c")), 2},
		{"project_transparent", inner(logical.NewProject(scan("Order", "o"), []string{"order_id"}, []string{""}), scan("Customer", "c")), 2},
		{"aggregate_opaque", inner(logical.NewAggregate(inner(scan("Order", "o"), scan("Customer", "c")), []string{"x"}, nil, nil, ""), scan("TypedRecord", "t")), 2},
		{"distinct_opaque", logical.NewDistinct(scan("Order", "o")), 1},
		{"sort_opaque", inner(logical.NewSort(inner(scan("Order", "o"), scan("Customer", "c")), nil), scan("TypedRecord", "t")), 2},
		{"union_opaque", logical.NewUnion([]logical.LogicalOperator{scan("Order", "o"), scan("Customer", "c")}, false), 1},
		{"filter_with_exists_rider_transparent", logical.LogicalOperator(&logical.LogicalFilter{
			Input:            scan("Order", "o"),
			ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("e")}},
		}), 1}, // commit 5b: the existential rides the merge; no ForEach added
		{"project_with_scalar_rider_transparent", logical.LogicalOperator(&logical.LogicalProject{
			Input:            scan("Order", "o"),
			Projections:      []string{"order_id"},
			ScalarSubqueries: []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("s")}},
		}), 1}, // commit 5b: uncorrelated scalar = root-context binding, transparent
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
	// S3 fulcrum: the shape GATES -- arity >= 2 is the whole wedge now. What
	// the S2 pin guarded (a 2-way seed mis-typed by post-translation
	// flattening) is legal composition today: the derived bodies seed their
	// own ordinal RCs and SelectMergeRule composes them via
	// translateValueCorrelations + the fuse arm.
	d := tr.ordinalWedgeGateDecide(evasion)
	if !d.Gated || d.Arity != 4 {
		t.Fatalf("flattening shape must gate at arity 4 (S3 fulcrum): %+v", d)
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
//   - a maximal 2-way join under a WHERE-EXISTS filter GATES (the item-2
//     commit-3 flatten gate arm — the 2+1 select seeds the baked ordinal
//     RC), and a 2-way join INSIDE the EXISTS subquery roots a fresh
//     cluster and gates too.
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

	t.Run("three_way_gathers_flat", func(t *testing.T) {
		t.Parallel()
		// S3 fulcrum: the 3-way root GATES (arity 3) and translates FLAT —
		// the nested inner join is GATHERED into the root's select, so it
		// never seeds at all (no recorded decision IS the invariant: a
		// gathered join has no translateJoin of its own).
		nested := inner(scan("Order", "o"), scan("Customer", "c"))
		root := inner(nested, scan("TypedRecord", "t"))
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated || d.Arity != 3 {
			t.Fatalf("3-way root: %+v (ok=%v), want gated arity 3 (flat N-way seed)", d, ok)
		}
		if d, ok := tr.wedgeGate[nested]; ok {
			t.Fatalf("gathered nested join must not seed separately, got recorded decision %+v", d)
		}
	})

	t.Run("left_outer_gated_both_legs_fresh", func(t *testing.T) {
		t.Parallel()
		// Item-3 S1: (a) the LEFT box GATES at the root even with a clustered
		// preserved leg (the Q3 narrowing retired); (b) legs of a GATED parent
		// translate FRESH (the S3-fulcrum convention), so the 2-way inner in
		// the PRESERVED leg gates independently as its own root; (c) the
		// NULL-SUPPLYING leg's inner keeps gating fresh (the null-on-empty
		// subselect is never merged).
		preserved := inner(scan("Customer", "c"), scan("TypedRecord", "t"))
		root := logical.NewJoin(preserved, scan("Order", "o"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated {
			t.Fatalf("LEFT-outer box: %+v (ok=%v), want recorded and GATED (item-3 S1)", d, ok)
		}
		if d, ok := tr.wedgeGate[preserved]; !ok || !d.Gated {
			t.Fatalf("2-way in the PRESERVED leg: %+v (ok=%v), want GATED (fresh leg of a gated parent, S3-fulcrum convention)", d, ok)
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

	t.Run("full_box_over_gated_join_leg_gates", func(t *testing.T) {
		t.Parallel()
		// S3 fulcrum (the ruling's demanded pin shape): `(a JOIN b) FULL JOIN
		// c` -- the FULL box's join leg GATES ITSELF, so the leg is eligible
		// (its output is a typed flat ordinal concat; multi-accessor
		// FieldPaths resolve upper references through it) and the FULL box
		// gates too. Both decisions recorded.
		nested := inner(scan("Customer", "c"), scan("TypedRecord", "t"))
		root := logical.NewJoin(scan("Order", "o"), nested, logical.JoinFull, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated {
			t.Fatalf("FULL box with a GATED join leg: %+v (ok=%v), want gated (S3 fulcrum)", d, ok)
		}
		if d, ok := tr.wedgeGate[nested]; !ok || !d.Gated {
			t.Fatalf("2-way under a FULL leg roots a fresh cluster: %+v (ok=%v), want gated", d, ok)
		}
	})

	t.Run("enclosed_outer_box_stays_name_model", func(t *testing.T) {
		t.Parallel()
		// Review finding (mixed outer nesting): an outer box that is a LEG
		// of an enclosing name-model join must NOT gate — the parent's merge
		// binds leg rows by name, so a positional box row under it reads the
		// wrong source (`d LEFT JOIN e ON … JOIN c ON …` returned d.id as
		// e.id, the FDB runtime pins). The INNER arm has carried this
		// enclosure guard since Slice 2; the outer arms shipped without it.
		// Item-3 S3: a LEFT/RIGHT box leg is ordinal-ELIGIBLE, so a plain
		// inner parent GATES and the box translates FRESH — the enclosure
		// guard survives VERBATIM (amendment H) but needs a GENUINELY
		// name-model parent to fire: a white-box scan alias spelling the
		// box's MINTED binding (E$BOX) collides in the gate's dup arm —
		// poison — so the box under that parent stays enclosed and
		// name-model. (Unreachable from SQL: $ is mint-namespace only.)
		for _, kind := range []logical.JoinKind{logical.JoinLeft, logical.JoinRight} {
			box := logical.NewJoin(scan("Order", "d"), scan("Customer", "e"), kind, "")
			root := inner(box, scan("TypedRecord", "E$BOX"))
			tr := newGateTranslator(t)
			tr.translateRef(root)
			d, ok := tr.wedgeGate[box]
			if !ok {
				t.Fatalf("kind %v: no gate decision recorded for the enclosed box", kind)
			}
			if d.Gated {
				t.Fatalf("kind %v: enclosed outer box gated (%+v), want name-model (enclosure guard)", kind, d)
			}
			if !strings.Contains(d.Reason, "enclosed") {
				t.Fatalf("kind %v: reason %q, want the enclosure-guard reason", kind, d.Reason)
			}
		}
	})

	t.Run("rfc153_joined_preserved_gates", func(t *testing.T) {
		t.Parallel()
		// RFC-173 item 3 commit 1 (S1): the joined-preserved family —
		// `a JOIN b LEFT JOIN c` — GATES at the box root. The design ruling
		// retired the single-source condition (the flattened preserved
		// cluster's buried sources are nameable per-leg since items 1+2:
		// binding-keyed windows + positional binders), so the box takes the
		// ordinal seed and its inner(a,b) preserved leg translates as a
		// gated cluster leg. Pre-item-3 both stayed name-model (the Q3
		// buried-names narrowing).
		ab := inner(scan("Order", "o"), scan("Customer", "c"))
		root := logical.NewJoin(ab, scan("TypedRecord", "t"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated {
			t.Fatalf("joined-preserved LEFT box: %+v (ok=%v), want recorded and GATED (item-3 S1)", d, ok)
		}
	})

	t.Run("mixed_nesting_leg_ineligible", func(t *testing.T) {
		t.Parallel()
		// S3 fulcrum: `(A JOIN B JOIN C) FULL JOIN D` -- the 3-way inner leg
		// GATES ITSELF (arity 3 >= 2), so it is eligible and the FULL box
		// gates over it; the leg type is the 3-leg flat concat.
		threeWay := inner(inner(scan("Order", "o"), scan("Customer", "c")), scan("TypedRecord", "t"))
		root := logical.NewJoin(threeWay, scan("Order", "o2"), logical.JoinFull, "")
		tr := newGateTranslator(t)
		tr.translateRef(root)
		if d, ok := tr.wedgeGate[root]; !ok || !d.Gated {
			t.Fatalf("FULL box over a GATED 3-way leg: %+v (ok=%v), want gated (S3 fulcrum)", d, ok)
		}

		// The DERIVED-TABLE-wrapped join leg: post-fulcrum the derived body
		// join gates itself (LogicalCTE transparency now reports ELIGIBLE for
		// a gating body -- the PR-447 arm still guards NAME-MODEL bodies).
		derivedJoin := logical.NewCTE("d",
			inner(scan("Customer", "c4"), scan("TypedRecord", "t4")),
			logical.NewScan("d", ""), false)
		root3 := logical.NewJoin(scan("Order", "o4"), derivedJoin, logical.JoinFull, "")
		tr3 := newGateTranslator(t)
		tr3.translateRef(root3)
		if d, ok := tr3.wedgeGate[root3]; !ok || !d.Gated {
			t.Fatalf("FULL box over a DERIVED-wrapped GATING join leg: %+v (ok=%v), want gated (S3 fulcrum)", d, ok)
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
		// The SUBQUERY join roots a fresh cluster and gates, unchanged.
		if d, ok := tr.wedgeGate[subJoin]; !ok || !d.Gated {
			t.Fatalf("2-way join inside an EXISTS subquery: %+v (ok=%v), want gated (fresh cluster)", d, ok)
		}
		// RFC-173 item-2 commit 3: the flatten CONSULTS the gate and a
		// maximal 2-way INNER join under WHERE-EXISTS now GATES — the flat
		// 2+1 select seeds the baked ordinal RC (the flatten-gate pins in
		// rfc173_item2_c3_flatten_gate_test.go assert the seed itself; this
		// pin holds the recorded decision). Pre-commit-3 the flatten never
		// consulted the gate and its legs were unconditionally enclosed.
		if d, ok := tr.wedgeGate[outerJoin]; !ok || !d.Gated || d.Arity != 2 {
			t.Fatalf("flattened 2-way join under EXISTS must gate at arity 2 (item-2 commit 3): %+v (ok=%v)", d, ok)
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
		// HAVING-EXISTS over a 2-way join input (review W2 matrix addition):
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

// TestRFC173S2_WalkArmParity is the dimensional-gap pin from the @claude
// PR-447 catch (review hardening request): ordinalEligible and clusterArity
// are the SAME classification asked two questions, and the LogicalCTE arm
// existed in one but not the other — each walk had been verified against the
// RULE it shadows, never against its sibling. This table asserts the pair's
// answers for EVERY logical operator type; adding an operator type or an arm
// to one walk without updating the table (and deliberately deciding the other
// walk's treatment) fails here instead of waiting for the next reviewer.
//
// The two walks legitimately DIVERGE on two classes (asserted as such):
//   - joins: ineligible as LEGS categorically (nesting = S3), while
//     clusterArity still counts inner joins (arity 2) for the SEED walk;
//   - unknown/DML/values leaves (the defaults): eligible as legs (their rows
//     are single-namespace) but arity-poison (an unclassifiable CLUSTER
//     fails toward the name model).
func TestRFC173S2_WalkArmParity(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	tr.cteScope["BODYJOIN"] = inner(scan("Order", "ox"), scan("Customer", "cx"))

	cases := []struct {
		name         string
		op           logical.LogicalOperator
		wantEligible bool
		wantArity    int
	}{
		{"scan", scan("Order", "o"), true, 1},
		{"cte_scoped_scan_join_body", scan("bodyjoin", "b"), true, 2}, // S3 fulcrum: body join gates; composition is legal
		{"filter_plain", logical.NewFilter(scan("Order", "o"), "x > 1"), true, 1},
		{"filter_exists", &logical.LogicalFilter{Input: scan("Order", "o"), ExistsSubqueries: []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("e")}}}, true, 1}, // commit 5b rider lift
		{"project_plain", logical.NewProject(scan("Order", "o"), []string{"order_id"}, []string{""}), true, 1},
		{"project_scalar_subq", &logical.LogicalProject{Input: scan("Order", "o"), Projections: []string{"order_id"}, ScalarSubqueries: []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("s")}}}, true, 1}, // commit 5b rider lift
		{"inner_join", inner(scan("Order", "o"), scan("Customer", "c")), true, 2},                                                                                                                                            // S3 fulcrum: eligible iff the leg itself gates
		{"left_join", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""), true, 2},                                                                                                             // item-3 S3+A: eligible as a leg; arity preserved+1
		{"full_join", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, ""), true, 1},                                                                                                             // S3 fulcrum: FULL gates unconditionally -> eligible; cluster: opaque box of 1
		{"cte_nonrecursive_derived_join", logical.NewCTE("d", inner(scan("Order", "o"), scan("Customer", "c")), logical.NewScan("d", ""), false), true, 2},                                                                   // S3 fulcrum: derived body join gates
		{"cte_recursive", logical.NewCTE("r", logical.NewUnion([]logical.LogicalOperator{scan("Order", "o"), scan("Order", "o2")}, false), logical.NewScan("r", ""), true), false, arityPoison},
		{"aggregate", logical.NewAggregate(scan("Order", "o"), []string{"x"}, nil, nil, ""), true, 1},
		{"distinct", logical.NewDistinct(scan("Order", "o")), true, 1},
		{"sort", logical.NewSort(scan("Order", "o"), nil), true, 1},
		{"limit", logical.NewLimit(scan("Order", "o"), 5, 0), true, 1},
		{"union", logical.NewUnion([]logical.LogicalOperator{scan("Order", "o"), scan("Customer", "c")}, false), true, 1},
		{"values_default_arm", logical.NewValues([]string{"(1)"}, []string{"a"}), true, arityPoison}, // the documented default divergence
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tr.ordinalEligible(tc.op); got != tc.wantEligible {
				t.Errorf("ordinalEligible(%s) = %v, want %v", tc.name, got, tc.wantEligible)
			}
			if got := tr.clusterArity(tc.op); got != tc.wantArity {
				t.Errorf("clusterArity(%s) = %d, want %d", tc.name, got, tc.wantArity)
			}
		})
	}
}

// TestRFC173S3_WhereMergeBakesLegRelative pins the WHERE-merge bake site
// against the nested-binary mismatch: for Filter(join(join(o,c),t)) the
// merged cross-leg conjunct `c.customer_id = t.id` must bake LEG-RELATIVE
// ordinals over each table's OWN type (gatedJoinLegTypes — the gathered-leg
// pairing the seed's quantifiers use). Pairing sourceAlias(join.Left) ("c",
// the buried rightmost table of the left subtree) with
// ordinalLegType(join.Left) (the whole {o,c} concat) baked CONCAT-relative
// ordinals onto the single-table quantifier C — out of range at runtime, or
// silently the WRONG column when FieldIndex first-matches an earlier leg's
// duplicate name.
func TestRFC173S3_WhereMergeBakesLegRelative(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	nested := inner(scan("Order", "o"), scan("Customer", "c"))
	root := inner(nested, scan("TypedRecord", "t"))
	pred := &predicates.ComparisonPredicate{
		Operand: values.NewFieldValue(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("c")),
			"CUSTOMER_ID", values.NotNullLong),
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("t")),
				"ID", values.NotNullLong),
		},
	}
	filter := logical.NewFilterWithPredicate(root, pred, "c.customer_id = t.id")

	ref := tr.translateRef(filter)
	if ref == nil {
		t.Fatal("Filter over the gated 3-way cluster must translate")
	}
	sel, ok := ref.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected the merged SelectExpression, got %T", ref.Members()[0])
	}

	// The expected leg types: each table's OWN type, the seed's pairing.
	customerType := tr.ordinalLegType(scan("Customer", "c"))
	orderType := tr.ordinalLegType(scan("Order", "o"))
	if customerType == nil || orderType == nil {
		t.Fatal("leg types must derive from metadata")
	}
	wantOrd, found := customerType.FieldIndex("CUSTOMER_ID")
	if !found {
		t.Fatal("CUSTOMER_ID missing from Customer's own type")
	}

	var bakedC *values.FieldValue
	for _, p := range sel.GetPredicates() {
		predicates.ReplaceValues(p, func(v values.Value) values.Value {
			fv, isFV := v.(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				return v
			}
			if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV && qov.Correlation.Name() == "C" {
				bakedC = fv
			}
			return v
		})
	}
	if bakedC == nil {
		t.Fatal("the merged WHERE conjunct's `c` reference must be BAKED (cross-leg conjunct over a gated join)")
	}
	acc, single := bakedC.Resolved.Single()
	if !single {
		t.Fatalf("want a single-accessor bake, got %v", bakedC.Resolved)
	}
	if acc.Ordinal != wantOrd {
		t.Fatalf("baked ordinal = %d, want %d (CUSTOMER_ID leg-relative in Customer's OWN type; a concat-relative bake is off by Order's width %d)",
			acc.Ordinal, wantOrd, len(orderType.Fields))
	}
	qovC := bakedC.Child.(*values.QuantifiedObjectValue)
	rt, isRT := qovC.Type().(*values.RecordType)
	if !isRT || len(rt.Fields) != len(customerType.Fields) {
		t.Fatalf("baked QOV(c) type width = %v, want Customer's own width %d (not the {o,c} concat %d)",
			qovC.Type(), len(customerType.Fields), len(orderType.Fields)+len(customerType.Fields))
	}
}

// TestRFC173S3_BoxLegBakeResolvesLeafLocal pins the bake window for a BOX leg
// (review catch on the fulcrum): in `(o FULL JOIN c) JOIN t ON c.price = t.id`
// the FULL box enters the binary seed as ONE leg NAMED after its rightmost
// leaf ("c", sourceAlias) and TYPED as the {o,c} concat. The bare reference
// `c.price` addresses THAT LEAF's column — but Order also has a PRICE column
// earlier in the concat, so a whole-concat FieldIndex first-match silently
// baked ORDER's price (wrong column, right range: undetectable at runtime).
// The bake must resolve within the rightmost leaf's own window and offset
// into the concat.
func TestRFC173S3_BoxLegBakeResolvesLeafLocal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
	pred := &predicates.ComparisonPredicate{
		Operand: values.NewFieldValue(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("c")),
			"PRICE", values.NotNullLong),
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("t")),
				"ID", values.NotNullLong),
		},
	}
	root := logical.NewJoinWithPredicate(box, scan("TypedRecord", "t"), logical.JoinInner, pred)

	ref := tr.translateRef(root)
	if ref == nil {
		t.Fatal("the gated join over a FULL box leg must translate")
	}
	sel, ok := ref.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected the seed SelectExpression, got %T", ref.Members()[0])
	}

	orderType := tr.ordinalLegType(scan("Order", "o"))
	customerType := tr.ordinalLegType(scan("Customer", "c"))
	if orderType == nil || customerType == nil {
		t.Fatal("leg types must derive from metadata")
	}
	leafIdx, found := customerType.FieldIndex("PRICE")
	if !found {
		t.Fatal("PRICE missing from Customer's own type")
	}
	firstMatch, found := orderType.FieldIndex("PRICE")
	if !found {
		t.Fatal("the collision premise needs PRICE on Order too")
	}
	wantOrd := len(orderType.Fields) + leafIdx

	var bakedC *values.FieldValue
	for _, p := range sel.GetPredicates() {
		predicates.ReplaceValues(p, func(v values.Value) values.Value {
			fv, isFV := v.(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				return v
			}
			if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV && qov.Correlation.Name() == "C$BOX" { // item-3 c4: the box quantifier is MINTED (leaf+$BOX)
				bakedC = fv
			}
			return v
		})
	}
	if bakedC == nil {
		t.Fatal("the ON conjunct's `c` reference must be BAKED (cross-leg over the gated join)")
	}
	acc, single := bakedC.Resolved.Single()
	if !single {
		t.Fatalf("want a single-accessor bake, got %v", bakedC.Resolved)
	}
	if acc.Ordinal == firstMatch {
		t.Fatalf("baked ordinal %d is the whole-concat FIRST MATCH (Order's PRICE) — the silent wrong-column bake", acc.Ordinal)
	}
	if acc.Ordinal != wantOrd {
		t.Fatalf("baked ordinal = %d, want %d (Customer's PRICE leaf-local at the box offset %d)", acc.Ordinal, wantOrd, len(orderType.Fields))
	}
}

// TestRFC173Item3_GatedJoinLegTypes_BuriedConsistency pins the two
// predicate-bake legTypes maps in lockstep (the boundary-consistency class):
// the WHERE-pred map (gatedJoinLegTypes) must register the SAME buried bake
// windows for a clustered box leg as the seed's own map
// (ordinalJoinSeedFields via addBuriedBakeWindows). A WHERE cross-leg
// conjunct naming a buried source (`WHERE a.x = c.y` over `(A⋈B) LEFT C`)
// bakes through this map; without the buried entries predicateLegAliases
// counts it single-leg and the reference stays a grandchild-correlated lazy
// read — the exact divergence class the seed map closed at amendment C.
func TestRFC173Item3_GatedJoinLegTypes_BuriedConsistency(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	// (SRC ⋈ AUX) LEFT AUX2 — the preserved leg is a cluster burying SRC.
	j := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")),
		scan("AUX2", "y"), logical.JoinLeft, "")
	if d := tr.ordinalWedgeGateDecide(j); !d.Gated {
		t.Fatalf("fixture must gate, got %+v", d)
	}
	legTypes := tr.gatedJoinLegTypes(j)
	// The buried source S must have a window: box-corr = the cluster's
	// binding (its rightmost leaf X), offset 0 within the cluster concat.
	s, ok := legTypes["S"]
	if !ok {
		t.Fatalf("gatedJoinLegTypes lacks the BURIED leg S: %v — the WHERE-pred bake map diverged from the seed's (amendment C)", keysOf(legTypes))
	}
	if s.bakeCorr == "" || s.leafOffset != 0 || s.leafTyp == nil || len(s.typ.Fields) <= len(s.leafTyp.Fields) {
		t.Fatalf("buried S window = {corr %q offset %d leaf %v concat %v} — want a box-corr window into the cluster concat", s.bakeCorr, s.leafOffset, s.leafTyp, s.typ)
	}
	// And it must AGREE with the seed map's entry bit-for-bit.
	_, seedTypes := tr.ordinalJoinSeedFields(tr.legsOfGatedJoin(j))
	seedS, ok := seedTypes["S"]
	if !ok {
		t.Fatal("seed legTypes lacks buried S — fixture invalid")
	}
	if s.bakeCorr != seedS.bakeCorr || s.leafOffset != seedS.leafOffset ||
		len(s.typ.Fields) != len(seedS.typ.Fields) || len(s.leafTyp.Fields) != len(seedS.leafTyp.Fields) {
		t.Fatalf("WHERE-pred buried window %+v disagrees with the seed's %+v", s, seedS)
	}
}
