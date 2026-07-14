package query

// White-box census pins for RFC-173 B2 sub-slice A (the filtered box unnest).
// The ORDINAL-FIRED proof: a BAKEABLE box-leg conjunct rides the gathered path
// and fires ZERO name-model producers; an UNBAKEABLE one (here: a reference the
// buried window cannot resolve) declines to the name-model lowering and fires
// producers — the discriminating control proving the verdict routes, not just
// passes. Rows/plan correctness is pinned e2e by
// TestFDB_RFC173B2_FilteredBoxUnnest (sqldriver).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// b2BoxShapeWithPred builds `FROM T4 LEFT JOIN T ON <none>, T4.SARR AS X WHERE
// <pred>` over the chained-spine fixture's catalog (T4 carries the SARR array;
// T is the other leg).
func b2BoxShapeWithPred(pred predicates.QueryPredicate) *logical.LogicalFilter {
	box := logical.NewJoin(scan("T4", "T4"), scan("T", "T"), logical.JoinLeft, "")
	u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
	j := logical.NewJoin(box, u, logical.JoinInner, "")
	return &logical.LogicalFilter{Input: j, Predicate: pred}
}

// b2FilteredBoxShape is b2BoxShapeWithPred with `WHERE T4.<col> = 10`.
func b2FilteredBoxShape(col string) *logical.LogicalFilter {
	return b2BoxShapeWithPred(predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), col, values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(10)},
		},
	))
}

func TestRFC173B2_FilteredBoxUnnestCensus(t *testing.T) {
	countProducers := func(t *testing.T, f *logical.LogicalFilter) (int, bool) {
		t.Helper()
		var n int
		SetProducerCensusObserver(func(ProducerCensusRecord) { n++ })
		defer SetProducerCensusObserver(nil)
		tr := newChainedSpineTranslator(t)
		sel := tr.translateFilter(f)
		return n, sel != nil
	}

	// BAKEABLE: T4.ID resolves in the box seed's buried window → the gather
	// admits, the merge bakes over the recorded legTypes → ZERO producers.
	t.Run("bakeable_conjunct_ordinalizes", func(t *testing.T) {
		n, ok := countProducers(t, b2FilteredBoxShape("ID"))
		if !ok {
			t.Fatalf("filtered box unnest failed to translate")
		}
		if n != 0 {
			t.Fatalf("bakeable box-leg conjunct fired %d name-model producer(s), want 0 (the gather must admit it)", n)
		}
	})

	// UNBAKEABLE (the discriminating control): a reference the buried window
	// cannot resolve (no such column) → the verdict declines pre-translation →
	// the name-model lowering fires producers. Proves the Bakeable path above is
	// the verdict ROUTING, not a vacuously-quiet observer.
	t.Run("unresolvable_conjunct_declines_name_model", func(t *testing.T) {
		n, ok := countProducers(t, b2FilteredBoxShape("NO_SUCH_COL"))
		if !ok {
			t.Fatalf("unbakeable filtered box unnest failed to translate (must fall to name-model, not nil)")
		}
		if n == 0 {
			t.Fatalf("unbakeable box-leg conjunct fired 0 producers — the decline path is dead or the observer is not wired")
		}
	})

	// PER-ARM classifier pins: each Unbakeable arm of classifyBoxLegConjunct
	// exercised DIRECTLY (the verdict is metadata-only by contract, callable
	// without translating). The census pair above proves the verdict ROUTES;
	// these prove each decline ARM is live.
	t.Run("classifier_unbakeable_arms", func(t *testing.T) {
		t4ID := func() values.Value {
			return values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), "ID", values.UnknownType)
		}
		eq := func(operand values.Value) predicates.Comparison {
			return predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
		}
		cases := []struct {
			name string
			pred predicates.QueryPredicate
		}{
			{"exists_value_operand", predicates.NewComparisonPredicate(
				t4ID(), eq(&values.ExistsValue{Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("EQ"))}))},
			{"foreign_correlation", predicates.NewComparisonPredicate(
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTERQ")), "K", values.UnknownType),
				eq(&values.ConstantValue{Value: int64(10)}))},
			{"dotted_frontier_read", predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "T4.ID", Typ: values.UnknownType},
				eq(&values.ConstantValue{Value: int64(10)}))},
		}
		classify := func(t *testing.T, f *logical.LogicalFilter) boxConjVerdict {
			t.Helper()
			j := f.Input.(*logical.LogicalJoin)
			return newChainedSpineTranslator(t).classifyBoxLegConjunct(
				j.Left.(*logical.LogicalJoin), j.Right.(*logical.LogicalUnnest), f.Predicate)
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := classify(t, b2BoxShapeWithPred(tc.pred)); got != boxConjUnbakeable {
					t.Fatalf("classifyBoxLegConjunct = %d, want Unbakeable(%d)", got, boxConjUnbakeable)
				}
			})
		}
		// SCALAR SUBQUERY operand is BAKEABLE (RFC-173 B): a ScalarSubqueryValue is
		// a LEAF (Children()==nil) — the bake closure leaves it untouched while the
		// sibling leg ref (T4.ID) ordinalizes; the subquery's result is bound by the
		// statement's scalar-subquery pre-eval pass (it was registered in
		// t.scalarSubqueries at predicate translation). So the conjunct bakes and the
		// gather owns the shape. e2e correctness: TestFDB_RFC173Slice3B2bFaceA
		// subquery_conjunct_* arms; ordinalization: census subquery_inner_conj.
		t.Run("scalar_subquery_operand_bakeable", func(t *testing.T) {
			pred := predicates.NewComparisonPredicate(
				t4ID(), eq(values.NewScalarSubqueryValue(values.NamedCorrelationIdentifier("SQ"))))
			if got := classify(t, b2BoxShapeWithPred(pred)); got != boxConjBakeable {
				t.Fatalf("classifyBoxLegConjunct = %d, want Bakeable(%d) for a scalar-subquery operand", got, boxConjBakeable)
			}
		})
		// BAKEABLE baseline over the same shape — proves the arms above are
		// discriminating (an always-Unbakeable classifier would pass them all).
		t.Run("bakeable_baseline", func(t *testing.T) {
			if got := classify(t, b2FilteredBoxShape("ID")); got != boxConjBakeable {
				t.Fatalf("classifyBoxLegConjunct = %d, want Bakeable(%d)", got, boxConjBakeable)
			}
		})
	})

	// SHAPE-driven classifier gates (vs the predicate-driven arms above). The
	// gate-first check delegates to gatesAsFreshCluster — the SAME wedge-gate
	// authority the gather itself runs — so classify and gather can never
	// diverge on shape admission; these pins nail the authority down on both
	// sides of the gate.
	t.Run("classifier_shape_gates", func(t *testing.T) {
		classify := func(t *testing.T, box *logical.LogicalJoin, legAlias string) boxConjVerdict {
			t.Helper()
			u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
			pred := predicates.NewComparisonPredicate(
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(legAlias)), "ID", values.UnknownType),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)}},
			)
			return newChainedSpineTranslator(t).classifyBoxLegConjunct(box, u, pred)
		}
		// DUP-ALIAS box (`T4 AS D LEFT T AS D` — two legs binding the SAME
		// correlation): the wedge gate poisons it (indistinguishable leg
		// correlations → name model), so classify must return Unbakeable from
		// the GATE-FIRST check. DISCRIMINATING for that check: without it, the
		// seed map derives, `D` finds a window, `ID` resolves → Bakeable → this
		// pin goes red. This is the pin for the classify-must-never-outrun-the-
		// gate hazard (a genuine name-model box reaching ordinalJoinSeedFields
		// is the panic class).
		t.Run("dup_alias_box_gate_first_declines", func(t *testing.T) {
			dup := logical.NewJoin(scan("T4", "D"), scan("T", "D"), logical.JoinLeft, "")
			if got := classify(t, dup, "D"); got != boxConjUnbakeable {
				t.Fatalf("dup-alias box classify = %d, want Unbakeable(%d) via the gate-first check", got, boxConjUnbakeable)
			}
		})
		// NESTED outer-box leg (`(T4 FULL T) FULL TB`): the wedge gate ADMITS
		// it (nested buried windows derive through the box-as-one-leg concat),
		// so the conjunct classifies BAKEABLE — pinned together with the zero-
		// producer census below and the e2e correct-rows pin
		// (TestFDB_RFC173B2_FilteredBoxUnnest/nested_box_conjunct). Note this
		// is the GATHER authority: boxGatesFresh (the BINARY-seed/birth gate)
		// still excludes nested box legs — the two gates differ by design.
		t.Run("nested_box_leg_classifies_bakeable", func(t *testing.T) {
			nested := logical.NewJoin(
				logical.NewJoin(scan("T4", "T4"), scan("T", "T"), logical.JoinFull, ""),
				scan("T", "TB"), logical.JoinFull, "")
			if got := classify(t, nested, "T4"); got != boxConjBakeable {
				t.Fatalf("nested-box-leg classify = %d, want Bakeable(%d) (the wedge gate admits nested boxes)", got, boxConjBakeable)
			}
			u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
			j := logical.NewJoin(nested, u, logical.JoinInner, "")
			pred := predicates.NewComparisonPredicate(
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), "ID", values.UnknownType),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)}},
			)
			n, ok := countProducers(t, &logical.LogicalFilter{Input: j, Predicate: pred})
			if !ok {
				t.Fatalf("nested filtered box unnest failed to translate")
			}
			if n != 0 {
				t.Fatalf("bakeable nested-box conjunct fired %d name-model producer(s), want 0", n)
			}
		})
	})

	// CLUSTERED-LEG box census arm: the box's LEFT leg is an INNER cluster
	// (T4 ⋈ T) under FULL, and the conjunct references the buried NON-OWNER
	// leaf (T.ID). Must classify Bakeable and ride the gathered ordinal path
	// (0 producers) — the e2e sibling (c5a's clustered-box-bakes-ordinal) has
	// path-independent rows, so THIS census arm is the routing discriminator
	// for the clustered dimension.
	t.Run("bakeable_clustered_conjunct_ordinalizes", func(t *testing.T) {
		innerCluster := inner(scan("T4", "T4"), scan("T", "T"))
		box := logical.NewJoin(innerCluster, scan("T", "TB"), logical.JoinFull, "")
		u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
		j := logical.NewJoin(box, u, logical.JoinInner, "")
		pred := predicates.NewComparisonPredicate(
			values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T")), "ID", values.UnknownType),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)}},
		)
		n, ok := countProducers(t, &logical.LogicalFilter{Input: j, Predicate: pred})
		if !ok {
			t.Fatalf("clustered filtered box unnest failed to translate")
		}
		if n != 0 {
			t.Fatalf("bakeable clustered buried-leg conjunct fired %d name-model producer(s), want 0 (the gather must admit it)", n)
		}
	})
}

// TestRFC173E1b_BakeGatedChecked_DriftDetection pins the DRIFT signal that backs the
// E-1b flat-arm safety net (review-caught: predicateRefsBuriedLeg alone was inert for a
// flat-leg survivor). The classifier pre-verifies FieldIndex before admitting, so drift
// is unreachable via production SQL — this is the white-box pin of the dimension no
// e2e shape can reach. A should-bake ref (≥2-leg or buried conjunct) whose leaf window
// has no such field → drift=true; a SINGLE-LEG non-buried ref is left lazy (never
// baked), so it is NOT drift even when its field is absent.
func TestRFC173E1b_BakeGatedChecked_DriftDetection(t *testing.T) {
	t.Parallel()
	aType := &values.RecordType{Fields: []values.Field{{Name: "K", FieldType: values.NotNullLong, Ordinal: 0}}}
	bType := &values.RecordType{Fields: []values.Field{{Name: "K", FieldType: values.NotNullLong, Ordinal: 0}}}
	abType := &values.RecordType{Fields: []values.Field{
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	// two TOP-LEVEL flat legs (leafTyp == typ per leg is the flat contract; here the
	// leaf windows are the per-leg types, offset into the merged concat).
	flatLegs := map[string]bakeLegType{
		"A": {typ: abType, leafOffset: 0, leafTyp: aType},
		"B": {typ: abType, leafOffset: 1, leafTyp: bType},
	}
	// one BURIED leg (bakeCorr set — a box leaf with no quantifier of its own).
	buriedLegs := map[string]bakeLegType{
		"A": {typ: abType, leafOffset: 0, leafTyp: aType, bakeCorr: "BOX"},
	}
	legFV := func(corr, field string) values.Value {
		return values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)), field, values.NotNullLong)
	}
	crossLeg := func(af, bg string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(legFV("A", af),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: legFV("B", bg)})
	}
	singleLeg := func(af string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(legFV("A", af),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}})
	}
	cases := []struct {
		name  string
		preds []predicates.QueryPredicate
		legs  map[string]bakeLegType
		want  bool
	}{
		// ≥2-leg conjunct, both refs resolve → baked clean, no drift.
		{"cross_leg_both_resolve", []predicates.QueryPredicate{crossLeg("K", "K")}, flatLegs, false},
		// ≥2-leg conjunct, B.MISSING is not in B's FLAT leaf window → should-bake
		// survivor → DRIFT (the exact gap predicateRefsBuriedLeg missed: bakeCorr=="").
		{"cross_leg_flat_survivor", []predicates.QueryPredicate{crossLeg("K", "MISSING")}, flatLegs, true},
		// single-leg non-buried A.K = const → LAZY, never baked → no drift.
		{"single_leg_lazy_resolvable", []predicates.QueryPredicate{singleLeg("K")}, flatLegs, false},
		// single-leg non-buried A.MISSING = const → still LAZY (the lazy exemption
		// holds regardless of the field) → no drift. Proves we don't over-flag lazy.
		{"single_leg_lazy_missing_field", []predicates.QueryPredicate{singleLeg("MISSING")}, flatLegs, false},
		// buried single-leg A.K → FORCED to bake (predicateRefsBuriedLeg), resolves → no drift.
		{"buried_resolves", []predicates.QueryPredicate{singleLeg("K")}, buriedLegs, false},
		// buried single-leg A.MISSING → forced to bake, FieldIndex fails → DRIFT.
		{"buried_survivor", []predicates.QueryPredicate{singleLeg("MISSING")}, buriedLegs, true},
		// an AND of a clean cross-leg and a drifting cross-leg → DRIFT (any conjunct).
		{"and_one_drifts", []predicates.QueryPredicate{predicates.NewAnd(crossLeg("K", "K"), crossLeg("K", "MISSING"))}, flatLegs, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, drift := bakeGatedJoinPredicatesChecked(c.preds, c.legs); drift != c.want {
				t.Errorf("drift = %v, want %v", drift, c.want)
			}
		})
	}
}
