package query

// White-box pins for the clustered-outer correlated-scalar ordinal path: the
// pull-up spine, the dotted seed shape, the carrier-kind enumeration,
// non-mutation, and the BOTH-DIRECTION dispatch pin (ordinal reachable for
// gated outers / loud decline for the ungated dup-poisoned class, since the
// name-model anchored fallback was retired / decline for the silent-NULL
// class). The FDB end-to-end matrix proves rows + plan stability.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// corrEq builds the correlated equality `<innerAlias>.<innerCol> = <leg>.<col>`
// in the exact value shape the builder emits (lazy FieldValues over untyped
// QOVs — qualifyBareFieldValue's output).
func corrEq(innerAlias, innerCol, legAlias, legCol string) *predicates.ComparisonPredicate {
	return &predicates.ComparisonPredicate{
		Operand: values.NewFieldValue(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(innerAlias)),
			innerCol, values.NotNullLong),
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(legAlias)),
				legCol, values.NotNullLong),
		},
	}
}

// clusteredCSQ assembles the csq the dispatch tests feed: inner
// `SELECT order_id FROM Order SQ WHERE SQ.order_id = <leg>.<col>`.
func clusteredCSQ(legAlias, legCol string) logical.CorrelatedScalarSubquery {
	return logical.CorrelatedScalarSubquery{
		Alias: values.UniqueCorrelationIdentifier(),
		InnerPlan: logical.NewFilterWithPredicate(
			scan("Order", "SQ"), corrEq("SQ", "ORDER_ID", legAlias, legCol), ""),
		InnerAlias:   "SQ",
		ScalarCol:    "ORDER_ID",
		StrictSingle: true,
	}
}

// clusteredProject wraps a csq over the given outer with dotted projections.
func clusteredProject(outer logical.LogicalOperator, csq logical.CorrelatedScalarSubquery) *logical.LogicalProject {
	return &logical.LogicalProject{
		Input:                      outer,
		Projections:                []string{"o.order_id", "(subq)"},
		Aliases:                    []string{"", ""},
		ProjectedValues:            []values.Value{nil, &values.ScalarSubqueryValue{Alias: csq.Alias}},
		IsComputed:                 []bool{false, true},
		CorrelatedScalarSubqueries: []logical.CorrelatedScalarSubquery{csq},
	}
}

// TestClusteredPullUp_Spine pins buildClusterPullUp: FROM-order leg spans
// over the flat concat, a FRESH outer correlation (never a leg alias — the
// unique-ids principle that rules out a divergent-baked-types collision),
// and the drift guard.
func TestClusteredPullUp_Spine(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	j := inner(scan("Order", "o"), scan("Customer", "c"))
	pu := tr.buildClusterPullUp(j)
	if pu == nil {
		t.Fatal("a gated 2-table cluster must build a pull-up spine")
	}
	if len(pu.legs) != 2 || pu.legs[0].alias != "O" || pu.legs[1].alias != "C" {
		t.Fatalf("legs = %+v, want FROM-order [O, C]", pu.legs)
	}
	if pu.legs[0].start != 0 || pu.legs[1].start != len(pu.legs[0].typ.Fields) {
		t.Fatalf("leg starts = %d/%d, want 0/%d (running concat offsets)",
			pu.legs[0].start, pu.legs[1].start, len(pu.legs[0].typ.Fields))
	}
	if got, want := len(pu.concatType.Fields), len(pu.legs[0].typ.Fields)+len(pu.legs[1].typ.Fields); got != want {
		t.Fatalf("concat width = %d, want %d", got, want)
	}
	corr := strings.ToUpper(pu.outerCorr.Name())
	if corr == "O" || corr == "C" || !strings.HasPrefix(pu.outerCorr.Name(), "q$") {
		t.Fatalf("outer correlation = %q, want a FRESH unique id (q$N), never a leg alias", pu.outerCorr.Name())
	}
}

// TestClusteredPullUp_BakesAllLegs pins the Java Value.pullUp equivalent:
// refs to EVERY leg — first AND rightmost — bake to
// ofOrdinal(QOV(fresh, concat), legStart+idx); refs to non-leg aliases pass
// through; the ORIGINAL tree is never mutated (a decline must be able to
// re-translate the unpoisoned fallback).
func TestClusteredPullUp_BakesAllLegs(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	j := inner(scan("Order", "o"), scan("Customer", "c"))
	pu := tr.buildClusterPullUp(j)
	if pu == nil {
		t.Fatal("pull-up spine")
	}

	// One ref per leg (C = non-rightmost... FROM-order first leg is O; the
	// RIGHTMOST leg is C) + one enclosing-scope ref that must survive lazy.
	pred := predicates.NewAnd(
		corrEq("SQ", "ORDER_ID", "c", "CUSTOMER_ID"),
		corrEq("SQ", "NEXT_ID", "o", "ORDER_ID"),
		corrEq("SQ", "OTHER", "z", "ZCOL"),
	)
	orig := logical.NewFilterWithPredicate(scan("Order", "SQ"), pred, "")

	rebuilt, ok := rebuildInnerWithValues(orig, pu.bake)
	if !ok || pu.missed {
		t.Fatalf("rebuild ok=%v missed=%v, want clean bake", ok, pu.missed)
	}

	countBaked := func(op logical.LogicalOperator) (baked, lazyLeg int) {
		f := op.(*logical.LogicalFilter)
		predicates.ReplaceValues(f.Predicate, func(v values.Value) values.Value {
			fv, isFV := v.(*values.FieldValue)
			if !isFV {
				return v
			}
			if fv.Resolved != nil {
				if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV && qov.Correlation == pu.outerCorr {
					baked++
				}
				return v
			}
			if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
				a := strings.ToUpper(qov.Correlation.Name())
				if a == "C" || a == "O" {
					lazyLeg++
				}
			}
			return v
		})
		return
	}
	baked, lazyLeg := countBaked(rebuilt)
	if baked != 2 || lazyLeg != 0 {
		t.Errorf("rebuilt: %d baked over the fresh concat QOV, %d lazy leg refs — want 2 baked (ALL legs, rightmost included), 0 lazy stragglers", baked, lazyLeg)
	}
	origBaked, origLazy := countBaked(orig)
	if origBaked != 0 || origLazy != 2 {
		t.Errorf("ORIGINAL tree mutated: %d baked / %d lazy leg refs, want 0/2 (copies, never in-place)", origBaked, origLazy)
	}

	// The baked ordinals land at legStart+idx of the CONCAT, not leg-relative.
	cIdx, _ := pu.legByAlias["C"].typ.FieldIndex("CUSTOMER_ID")
	wantC := pu.legByAlias["C"].start + cIdx
	found := false
	predicates.ReplaceValues(rebuilt.(*logical.LogicalFilter).Predicate, func(v values.Value) values.Value {
		if fv, isFV := v.(*values.FieldValue); isFV && fv.Resolved != nil {
			if acc, single := fv.Resolved.Single(); single && acc.Ordinal == wantC {
				found = true
			}
		}
		return v
	})
	if !found {
		t.Errorf("no baked ref at global ordinal %d (C.CUSTOMER_ID = legStart %d + idx %d)", wantC, pu.legByAlias["C"].start, cIdx)
	}

	// A leg ref to a column ABSENT from the leg's type flips missed → decline.
	pu2 := tr.buildClusterPullUp(j)
	_, ok = rebuildInnerWithValues(
		logical.NewFilterWithPredicate(scan("Order", "SQ"), corrEq("SQ", "X", "c", "NO_SUCH_COL"), ""),
		pu2.bake)
	if !ok || !pu2.missed {
		t.Errorf("unmappable leg column: ok=%v missed=%v, want ok=true missed=true (caller declines)", ok, pu2.missed)
	}
}

// TestClusteredCarrierEnumeration pins the pull-up walk's carrier
// enumeration. Every enumerated node kind's value slots are VISITED (a leg
// ref planted in each is collected), and every kind outside the enumeration
// reports exhaustive=false so the ordinal path DECLINES rather than silently
// skipping values. A new logical node kind falls to the default arm and
// fails SAFE — the query keeps its prior (name-model) behavior instead of
// mis-baking, at the cost of silently narrowing ordinal coverage (and of the
// decline guard not seeing refs inside the new kind). This pin documents the
// enumerated set; extend the rebuild arms AND this table together.
func TestClusteredCarrierEnumeration(t *testing.T) {
	t.Parallel()

	legRef := func() values.Value {
		return values.NewFieldValue(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("c")),
			"CUSTOMER_ID", values.NotNullLong)
	}
	legPred := func() predicates.QueryPredicate {
		return &predicates.ComparisonPredicate{
			Operand:    legRef(),
			Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
		}
	}
	outer := map[string]struct{}{"C": {}}

	enumerated := []struct {
		kind string
		op   logical.LogicalOperator
	}{
		{"Filter.Predicate", logical.NewFilterWithPredicate(scan("Order", "SQ"), legPred(), "")},
		{"Project.ProjectedValues", &logical.LogicalProject{
			Input: scan("Order", "SQ"), Projections: []string{"x"}, Aliases: []string{""},
			ProjectedValues: []values.Value{legRef()}, IsComputed: []bool{true},
		}},
		{"Aggregate.GroupKeys.Value", &logical.LogicalAggregate{
			Input: scan("Order", "SQ"), GroupKeys: []logical.GroupKey{{Display: "x", Bare: "x", Value: legRef()}},
		}},
		{"Aggregate.AggregateOperands", &logical.LogicalAggregate{
			Input: scan("Order", "SQ"), Calls: []logical.AggregateCall{{Func: "SUM", Operand: "x", BareColumn: true}}, Aliases: []string{""},
			AggregateOperands: []values.Value{legRef()},
		}},
		{"Aggregate.HavingPredicate", &logical.LogicalAggregate{
			Input: scan("Order", "SQ"), Calls: []logical.AggregateCall{{Func: "COUNT", Operand: "*", Star: true}}, Aliases: []string{""},
			HavingPredicate: legPred(),
		}},
		{"Sort.Keys.Value", logical.NewSort(scan("Order", "SQ"),
			[]logical.SortKey{{Expr: "x", Value: legRef()}})},
		{"Limit.LimitValue", &logical.LogicalLimit{Input: scan("Order", "SQ"), Limit: -1, LimitValue: legRef()}},
		{"Distinct(passthrough)", logical.NewDistinct(
			logical.NewFilterWithPredicate(scan("Order", "SQ"), legPred(), ""))},
		{"Join.OnPredicate", logical.NewJoinWithPredicate(
			scan("Order", "SQ"), scan("TypedRecord", "sq2"), logical.JoinInner, legPred())},
		{"Join(children recursed)", inner(
			logical.NewFilterWithPredicate(scan("Order", "SQ"), legPred(), ""), scan("TypedRecord", "sq2"))},
	}
	for _, c := range enumerated {
		refs, exhaustive := collectClusterOuterRefs(c.op, outer, map[string]struct{}{"SQ": {}})
		if !exhaustive {
			t.Errorf("%s: exhaustive=false, want the carrier enumerated", c.kind)
		}
		if _, hit := refs["C"]; !hit {
			t.Errorf("%s: planted leg ref NOT collected — the pull-up would silently skip this carrier", c.kind)
		}
	}

	// Aggregate-LOCAL carriers are WALK-ONLY: the collector sees refs there
	// (asserted above), but a REWRITE into one marks the chain un-rebuildable —
	// aggregate operands evaluate against the aggregate's inner input row,
	// which never carries the level-2 outer binding, so a baked ref there is
	// unresolvable at runtime (review finding). The enumeration table above
	// cannot distinguish walk-semantics from rewrite-semantics; this pin does.
	rewriteAll := func(v values.Value) values.Value {
		if fv, isFV := v.(*values.FieldValue); isFV && fv.Resolved == nil {
			return &values.FieldValue{Field: fv.Field, Typ: values.UnknownType}
		}
		return v
	}
	aggLocal := &logical.LogicalAggregate{
		Input: scan("Order", "SQ"), Calls: []logical.AggregateCall{{Func: "SUM", Operand: "x", BareColumn: true}}, Aliases: []string{""},
		AggregateOperands: []values.Value{legRef()},
	}
	if _, ok := rebuildInnerWithValues(aggLocal, rewriteAll); ok {
		t.Error("a REWRITE into an aggregate-local carrier must mark the chain un-rebuildable (ok=false) — the ordinal path declines")
	}
	if _, ok := rebuildInnerWithValues(
		logical.NewFilterWithPredicate(scan("Order", "SQ"), legPred(), ""), rewriteAll); !ok {
		t.Error("the same rewrite through a NON-aggregate carrier must rebuild (the walk-only rule is aggregate-local)")
	}

	// Outside the enumeration → exhaustive=false (decline direction).
	unenumerated := []struct {
		kind string
		op   logical.LogicalOperator
	}{
		{"Union", logical.NewUnion([]logical.LogicalOperator{scan("Order", "SQ"), scan("Order", "SQ2")}, false)},
		{"Filter+ExistsRider", &logical.LogicalFilter{
			Input:     scan("Order", "SQ"),
			Predicate: legPred(), ExistsSubqueries: []logical.ExistsSubquery{{}},
		}},
		{"Project+SubqueryRider", &logical.LogicalProject{
			Input:       scan("Order", "SQ"),
			Projections: []string{"x"}, ScalarSubqueries: []logical.ScalarSubquery{{}},
		}},
	}
	for _, c := range unenumerated {
		if _, exhaustive := collectClusterOuterRefs(c.op, outer, map[string]struct{}{"SQ": {}}); exhaustive {
			t.Errorf("%s: exhaustive=true for an un-enumerated carrier — refs could hide there and mis-bake", c.kind)
		}
	}
}

// TestClusteredSeed_Shape pins the dotted seed: one FULL ordinal run over the
// fresh concat QOV with fields named LEG.COL in FROM order, then the single
// nullable inner leg named INNER.SCALARCOL at ordinal 0.
func TestClusteredSeed_Shape(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	pu := tr.buildClusterPullUp(inner(scan("Order", "o"), scan("Customer", "c")))
	if pu == nil {
		t.Fatal("pull-up spine")
	}
	seed := clusteredOuterOrdinalSeed(pu, values.UniqueCorrelationIdentifier(), "SQ", "ORDER_ID")
	if seed == nil {
		t.Fatal("gated cluster must seed")
	}
	rc, isRC := seed.(*values.RecordConstructorValue)
	if !isRC {
		t.Fatalf("seed = %T, want *RecordConstructorValue", seed)
	}
	values.AssertOrdinalJoinSeed(rc)

	wantOuter := len(pu.concatType.Fields)
	if len(rc.Fields) != wantOuter+1 {
		t.Fatalf("%d fields, want %d (concat %d + 1 inner scalar)", len(rc.Fields), wantOuter+1, wantOuter)
	}
	for i, f := range rc.Fields[:wantOuter] {
		dot := strings.IndexByte(f.Name, '.')
		if dot <= 0 {
			t.Fatalf("outer field %d name = %q, want DOTTED LEG.COL", i, f.Name)
		}
		leg, isLeg := pu.legByAlias[f.Name[:dot]]
		if !isLeg {
			t.Fatalf("outer field %d name %q: prefix is not a leg alias", i, f.Name)
		}
		fv := f.Value.(*values.FieldValue)
		acc, _ := fv.Resolved.Single()
		if acc.Ordinal != i {
			t.Fatalf("outer field %d baked at ordinal %d, want %d (one full run)", i, acc.Ordinal, i)
		}
		idx, found := leg.typ.FieldIndex(f.Name[dot+1:])
		if !found || leg.start+idx != i {
			t.Fatalf("outer field %d name %q does not map back to global ordinal %d via its leg span", i, f.Name, i)
		}
		qov := fv.Child.(*values.QuantifiedObjectValue)
		if qov.Correlation != pu.outerCorr {
			t.Fatalf("outer field %d baked over %s, want the fresh concat QOV %s", i, qov.Correlation, pu.outerCorr)
		}
	}
	last := rc.Fields[wantOuter]
	if last.Name != "SQ.ORDER_ID" {
		t.Errorf("inner field name = %q, want SQ.ORDER_ID", last.Name)
	}
	ifv := last.Value.(*values.FieldValue)
	if !ifv.Typ.IsNullable() {
		t.Errorf("inner scalar must be NULLABLE (LEFT-OUTER null-fill), got %s", ifv.Typ)
	}
}

// TestClusteredDispatch_BothDirections pins the exit-gate dispatch, both
// directions:
//   - GATED cluster outer → the ORDINAL seed (anchored constructor
//     unreachable);
//   - an UNGATED outer + non-rightmost correlation → DECLINE (nil), the
//     CORRECT-or-LOUD replacement for the silent NULL.
func TestClusteredDispatch_BothDirections(t *testing.T) {
	t.Parallel()

	seedOf := func(t *testing.T, expr expressions.RelationalExpression) *values.RecordConstructorValue {
		t.Helper()
		proj, isProj := expr.(*expressions.LogicalProjectionExpression)
		if !isProj {
			t.Fatalf("translated = %T, want *LogicalProjectionExpression", expr)
		}
		sel, isSel := proj.GetQuantifiers()[0].GetRangesOver().Members()[0].(*expressions.SelectExpression)
		if !isSel {
			t.Fatalf("projection input is not the level-2 SelectExpression")
		}
		rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue)
		if !isRC {
			t.Fatalf("level-2 result value = %T, want RC", sel.GetResultValue())
		}
		return rc
	}

	// Direction 1: gated comma cluster, correlation to the FIRST leg.
	tr := newGateTranslator(t)
	gated := clusteredProject(inner(scan("Order", "o"), scan("Customer", "c")), clusteredCSQ("c", "CUSTOMER_ID"))
	// Rightmost is O; projections reference o.order_id (dotted) + the csq.
	gated.Projections = []string{"c.customer_id", "(subq)"}
	expr := tr.translateProjectWithCorrelatedScalar(gated)
	if expr == nil {
		t.Fatal("gated cluster + first-leg correlation must translate (it was 0AF00 on the name model)")
	}
	rc := seedOf(t, expr)
	values.AssertOrdinalJoinSeed(rc)

	// Direction 2: the JOINED-PRESERVED outer GATES — the pull-up names its
	// buried legs and the seed is ORDINAL, never anchored. Rightmost
	// correlation.
	tr2 := newGateTranslator(t)
	left := logical.NewJoin(
		inner(scan("Order", "o2"), scan("Order", "o")),
		scan("Customer", "c"), logical.JoinLeft, "")
	flipped := clusteredProject(left, clusteredCSQ("c", "CUSTOMER_ID"))
	expr2 := tr2.translateProjectWithCorrelatedScalar(flipped)
	if expr2 == nil {
		t.Fatalf("gated joined-preserved outer + rightmost correlation must translate ordinal, err=%v", tr2.translateErr)
	}
	_ = seedOf(t, expr2)

	// Direction 2b: a GENUINELY-UNGATED outer (dup leg bindings poison the
	// gate) + a correlated scalar. There is no name-model anchored-record
	// fallback anymore — no SQL query could reach it anyway (the binder
	// rejects the `FROM x, x` duplicate table alias; only this direct-tree
	// white-box path builds it). The shape is genuinely ambiguous (which `x`
	// does the correlation mean?), so it declines LOUDLY (correct-or-loud)
	// instead of seeding an anchored record over indistinguishable legs.
	tr2b := newGateTranslator(t)
	dupUngated := clusteredProject(inner(scan("Order", "x"), scan("Customer", "x")), clusteredCSQ("x", "CUSTOMER_ID"))
	if expr2b := tr2b.translateProjectWithCorrelatedScalar(dupUngated); expr2b != nil {
		t.Fatal("dup-poisoned outer + correlated scalar must decline LOUDLY (there is no anchored fallback), got a translation")
	}
	if tr2b.translateErr == nil || !strings.Contains(tr2b.translateErr.Error(), "duplicate leg bindings") {
		t.Fatalf("dup-poisoned outer decline = %v, want the ungated-cause (gate reason: duplicate leg bindings) decline", tr2b.translateErr)
	}

	// Direction 3: the gated joined-preserved outer + NON-rightmost (buried)
	// correlation translates ordinal too — the buried-leg spans name it.
	tr3 := newGateTranslator(t)
	buried := clusteredProject(left, clusteredCSQ("o2", "ORDER_ID"))
	if expr3 := tr3.translateProjectWithCorrelatedScalar(buried); expr3 == nil {
		t.Fatalf("gated outer + buried (non-rightmost) correlation must translate ordinal, err=%v", tr3.translateErr)
	} else {
		_ = seedOf(t, expr3)
	}

	// Direction 4: a SINGLE-SOURCE LEFT box outer GATES — the scalar dispatch
	// routes it through the ordinal seed exactly like the comma cluster of
	// direction 1 (the null-supplying role rides the leg marking; pinned
	// end-to-end in the FDB LEFT matrix).
	tr4 := newGateTranslator(t)
	singleLeft := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, "")
	gatedLeft := clusteredProject(singleLeft, clusteredCSQ("c", "CUSTOMER_ID"))
	expr4 := tr4.translateProjectWithCorrelatedScalar(gatedLeft)
	if expr4 == nil {
		t.Fatal("gated single-source LEFT outer + correlated scalar must translate ordinal")
	}
	_ = seedOf(t, expr4)
}

// TestInnerOwnAliasNotOuterRef pins the classifier's skip set: an inner that
// JOINS a table the outer FROM also binds (same alias — here the inner joins
// Order AS o while the outer's FIRST leg is Order o) references its OWN copy
// under SQL scoping. This query IS correctly classified (the only outer ref
// is the rightmost leg C), so the ordinal dispatch declines on the alias
// shadow and the shape reaches the GATED loud decline: anchored alias-keyed
// reads over a positional row are the mixed-model class. Misclassifying
// those refs as outer-leg refs would instead flip nonRightmost and
// spuriously EARLY-decline, masking that path.
func TestInnerOwnAliasNotOuterRef(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	// Inner: SELECT sq.order_id FROM TypedRecord sq JOIN Order o ON o.order_id
	// = sq.next_id WHERE sq.id = c.customer_id — its own "o" shadows the
	// outer's first leg; the only OUTER ref is to C (the rightmost leg).
	innerJoin := logical.NewJoinWithPredicate(
		scan("TypedRecord", "SQ"), scan("Order", "o"), logical.JoinInner,
		corrEq("o", "ORDER_ID", "SQ", "NEXT_ID"))
	csq := logical.CorrelatedScalarSubquery{
		Alias: values.UniqueCorrelationIdentifier(),
		InnerPlan: logical.NewFilterWithPredicate(
			innerJoin, corrEq("SQ", "ID", "c", "CUSTOMER_ID"), ""),
		InnerAlias:   "SQ",
		ScalarCol:    "ORDER_ID",
		StrictSingle: true,
	}
	p := clusteredProject(inner(scan("Order", "o"), scan("Customer", "c")), csq)
	p.Projections = []string{"c.customer_id", "(subq)"}

	expr := tr.translateProjectWithCorrelatedScalar(p)
	if expr != nil {
		t.Fatal("gated outer + inner-own-alias shadow: the ordinal dispatch declines and the guard must fail LOUD (anchored alias-keyed reads over a positional row are the mixed-model class)")
	}
	if tr.translateErr == nil || !strings.Contains(tr.translateErr.Error(), "ordinal dispatch declined") {
		t.Fatalf("want the typed gated-outer decline, got %v", tr.translateErr)
	}
}

// TestJoinInnerDispatch_Ordinal pins the JOIN-inner correlated-scalar path
// end-to-end at the translator: a JOIN-inner correlated scalar over a
// single-source outer takes the ORDINAL seed (the former innerContainsJoin
// gate is gone), and the inner quantifier carries a fresh unique
// correlation — never the SQL alias whose typed-QOV collision motivated the
// gate.
func TestJoinInnerDispatch_Ordinal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	joinInner := logical.NewFilterWithPredicate(
		logical.NewJoinWithPredicate(scan("Order", "SQ"), scan("TypedRecord", "i"), logical.JoinInner,
			corrEq("I", "ID", "SQ", "NEXT_ID")),
		corrEq("SQ", "ORDER_ID", "c", "CUSTOMER_ID"), "")
	csq := logical.CorrelatedScalarSubquery{
		Alias:        values.UniqueCorrelationIdentifier(),
		InnerPlan:    joinInner,
		InnerAlias:   "SQ",
		ScalarCol:    "ORDER_ID",
		StrictSingle: true,
	}
	p := &logical.LogicalProject{
		Input:                      scan("Customer", "c"),
		Projections:                []string{"c.name", "(subq)"},
		Aliases:                    []string{"", ""},
		ProjectedValues:            []values.Value{nil, &values.ScalarSubqueryValue{Alias: csq.Alias}},
		IsComputed:                 []bool{false, true},
		CorrelatedScalarSubqueries: []logical.CorrelatedScalarSubquery{csq},
	}
	expr := tr.translateProjectWithCorrelatedScalar(p)
	if expr == nil {
		t.Fatal("single-source outer + JOIN-inner must translate")
	}
	proj := expr.(*expressions.LogicalProjectionExpression)
	sel := proj.GetQuantifiers()[0].GetRangesOver().Members()[0].(*expressions.SelectExpression)
	rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		t.Fatalf("JOIN-inner seeded %T, want the ORDINAL seed RC", sel.GetResultValue())
	}
	values.AssertOrdinalJoinSeed(rc)
	innerQCorr := sel.GetQuantifiers()[1].GetAlias()
	if innerQCorr.Name() == "SQ" || !strings.HasPrefix(innerQCorr.Name(), "q$") {
		t.Fatalf("inner quantifier correlation = %q, want a fresh unique id (q$N), never the SQL alias (the widenLegTypesFromPlan collision)", innerQCorr.Name())
	}
	// The seed's inner leg must be keyed by the SAME fresh id (quantifier and
	// leg reference must agree for the executor's binding).
	lastQOV := rc.Fields[len(rc.Fields)-1].Value.(*values.FieldValue).Child.(*values.QuantifiedObjectValue)
	if lastQOV.Correlation != innerQCorr {
		t.Fatalf("seed inner leg keyed %s but the quantifier carries %s — binding mismatch", lastQOV.Correlation, innerQCorr)
	}
}
