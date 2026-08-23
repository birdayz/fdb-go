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

// exactDemoRef resolves a demo-schema field against its real flowed type.
//
// `col` is the column as the DESCRIPTOR spells it — lower/snake for the demo
// .proto — because a leg's flowed type carries the descriptor's own names and
// the lookup is exact.
//
// THE MISS IS LOUD, AND IT HAS TO BE. This used to fall through silently to a
// synthetic NotNullLong field whenever the column did not resolve, with a
// comment asking the reader to keep every genuine column spelled the
// descriptor's way — a rule nothing enforced, in a helper whose whole job is to
// produce a REAL typed reference. It failed exactly that way and generated
// greens across this file until an unrelated change moved the descriptor
// spelling and the fallbacks started mattering.
//
// A caller that WANTS the synthetic field says so by going through corrEq's
// demoRefOrForeign, which chooses per side — so the deliberate misses are
// visible at their own call site and a typo here is not.
func exactDemoRef(t *testing.T, alias, col string) values.Value {
	t.Helper()
	v, ok := demoRef(t, alias, col)
	if !ok {
		t.Fatalf("exactDemoRef(%q, %q): no such column on the demo schema — spell it "+
			"as the DESCRIPTOR does (lower/snake)", alias, col)
	}
	return v
}

func demoRef(t *testing.T, alias, col string) (values.Value, bool) {
	t.Helper()
	table := "Order"
	switch strings.ToUpper(col) {
	case "CUSTOMER_ID", "NAME", "EMAIL":
		table = "Customer"
	case "ID", "VAL_INT32", "VAL_INT64":
		table = "TypedRecord"
	}
	tr := newGateTranslator(t)
	typ := tr.ordinalLegType(scan(table, alias))
	if typ != nil {
		if ordinal, ok := typ.FieldIndexUnique(col); ok {
			return exactTestField(t, exactTestQOV(t, alias, typ), ordinal), true
		}
	}
	return nil, false
}

// corrEq builds the exact correlated equality
// `<innerAlias>.<innerCol> = <leg>.<col>`.
// Either side may name a column the demo schema does not have — several
// callers build a predicate over invented aliases on purpose (a foreign scope,
// an unnest element, a box leg) and only care that the shape reaches the gate.
// So each side takes the real reference when there is one and the synthetic
// field otherwise, chosen HERE rather than inside exactDemoRef: a typo in a
// genuine column still fails loudly at every other call site.
func corrEq(t *testing.T, innerAlias, innerCol, legAlias, legCol string) *predicates.ComparisonPredicate {
	return &predicates.ComparisonPredicate{
		Operand: demoRefOrForeign(t, innerAlias, innerCol),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: demoRefOrForeign(t, legAlias, legCol),
		},
	}
}

func demoRefOrForeign(t *testing.T, alias, col string) values.Value {
	t.Helper()
	if v, ok := demoRef(t, alias, col); ok {
		return v
	}
	return exactTestNamedField(t, alias, col, values.NotNullLong)
}

// exactScalarProjection gives a direct-tree correlated-scalar fixture the
// same one-column materialization boundary the SQL builder emits. A bare scan
// or join is a multi-column row and is not itself a scalar result under the
// RFC-232 exact-row contract.
func exactScalarProjection(t *testing.T, input logical.LogicalOperator, alias, col string) *logical.LogicalProject {
	t.Helper()
	project := logical.NewProject(input, []string{strings.ToUpper(col)}, []string{""})
	project.ProjectedValues = []values.Value{exactDemoRef(t, alias, col)}
	return project
}

// clusteredCSQ assembles the csq the dispatch tests feed: inner
// `SELECT order_id FROM Order SQ WHERE SQ.order_id = <leg>.<col>`.
func clusteredCSQ(t *testing.T, legAlias, legCol string) logical.CorrelatedScalarSubquery {
	filtered := logical.NewFilterWithPredicate(
		scan("Order", "SQ"), corrEq(t, "SQ", "order_id", legAlias, legCol), "")
	return logical.CorrelatedScalarSubquery{
		Alias:      values.UniqueCorrelationIdentifier(),
		InnerPlan:  exactScalarProjection(t, filtered, "SQ", "order_id"),
		InnerAlias: "SQ",
		// The SQL builder mints ScalarCol from values.ProjectionColumnName over
		// the materialized output value, so for a stored column it is the
		// descriptor's own spelling. Upper here would model a name production
		// no longer emits.
		ScalarCol:    "order_id",
		StrictSingle: true,
	}
}

// clusteredProject wraps a csq over the given outer with dotted projections.
func clusteredProject(t *testing.T, outer logical.LogicalOperator, csq logical.CorrelatedScalarSubquery) *logical.LogicalProject {
	t.Helper()
	return &logical.LogicalProject{
		Input:                      outer,
		Projections:                []string{"o.order_id", "(subq)"},
		Aliases:                    []string{"", ""},
		ProjectedValues:            []values.Value{exactDemoRef(t, "o", "order_id"), values.NewScalarSubqueryValue(csq.Alias, values.NullableLong)},
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
	if len(pu.legs) != 2 || pu.legs[0].binding != "O" || pu.legs[1].binding != "C" {
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
		corrEq(t, "SQ", "order_id", "c", "customer_id"),
		corrEq(t, "SQ", "price", "o", "order_id"),
		corrEq(t, "SQ", "OTHER", "z", "ZCOL"),
	)
	orig := logical.NewFilterWithPredicate(scan("Order", "SQ"), pred, "")

	rebuilt, ok := rebuildInnerWithValues(orig, pu.bake)
	if !ok || pu.missed {
		t.Fatalf("rebuild ok=%v missed=%v, want clean bake", ok, pu.missed)
	}

	countBaked := func(op logical.LogicalOperator) (baked, sourceLeg int) {
		f := op.(*logical.LogicalFilter)
		predicates.ReplaceValues(f.Predicate, func(v values.Value) values.Value {
			fv, isFV := values.AsFieldValue(v)
			if !isFV {
				return v
			}
			qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
			if !isQOV {
				return v
			}
			if qov.Correlation() == pu.outerCorr {
				baked++
				return v
			}
			a := strings.ToUpper(qov.Correlation().Name())
			if a == "C" || a == "O" {
				sourceLeg++
			}
			return v
		})
		return
	}
	baked, sourceLeg := countBaked(rebuilt)
	if baked != 2 || sourceLeg != 0 {
		t.Errorf("rebuilt: %d baked over the fresh concat QOV, %d source-leg refs — want 2 baked (ALL legs, rightmost included), 0 source stragglers", baked, sourceLeg)
	}
	origBaked, origLazy := countBaked(orig)
	if origBaked != 0 || origLazy != 2 {
		t.Errorf("ORIGINAL tree mutated: %d baked / %d lazy leg refs, want 0/2 (copies, never in-place)", origBaked, origLazy)
	}

	// The baked ordinals land at legStart+idx of the CONCAT, not leg-relative.
	// The lookup is EXACT against the leg's flowed type, which carries the
	// descriptor's own spelling — and a missed lookup must fail here rather
	// than silently yield 0, which for a first-column reference is
	// indistinguishable from the right answer.
	cIdx, foundC := pu.legByBinding["C"].typ.FieldIndexUnique("customer_id")
	if !foundC {
		t.Fatalf("fixture drift: Customer's leg type has no customer_id (%v)", pu.legByBinding["C"].typ)
	}
	wantC := pu.legByBinding["C"].start + cIdx
	found := false
	predicates.ReplaceValues(rebuilt.(*logical.LogicalFilter).Predicate, func(v values.Value) values.Value {
		if fv, isFV := values.AsFieldValue(v); isFV {
			if got := fv.Path().Ordinals(); len(got) == 1 && got[0] == wantC {
				found = true
			}
		}
		return v
	})
	if !found {
		t.Errorf("no baked ref at global ordinal %d (C.CUSTOMER_ID = legStart %d + idx %d)", wantC, pu.legByBinding["C"].start, cIdx)
	}

	// A leg ref to a column ABSENT from the leg's type flips missed → decline.
	pu2 := tr.buildClusterPullUp(j)
	_, ok = rebuildInnerWithValues(
		logical.NewFilterWithPredicate(scan("Order", "SQ"), corrEq(t, "SQ", "price", "c", "NO_SUCH_COL"), ""),
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
		return exactDemoRef(t, "c", "customer_id")
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
		if _, isFV := values.AsFieldValue(v); isFV {
			return &values.ConstantValue{Value: int64(0)}
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

// TestClusterFieldResolvableSpansBothNamingAuthorities drives
// clusterFieldResolvable's two arms directly, because nothing else in this
// package reaches it: it is the arm for a projection that carries no resolved
// Value, and every fixture here supplies one.
//
// Its two operands are minted by DIFFERENT authorities. A flat projection NAME
// arrives normalized at the parse boundary — unquoted folded UPPER — while the
// seed's own names carry the leg row's descriptor spelling and the subquery's
// output title. So an exact comparison answers "unresolvable" for a column that
// is plainly in the row, and the whole clustered-outer ordinal path declines a
// query it can serve.
//
// The negative arms are what keep the positives honest: a predicate that
// answered true unconditionally would satisfy every positive arm below on its
// own, so at 4 positive and 3 negative arms the pin fails in both directions.
func TestClusterFieldResolvableSpansBothNamingAuthorities(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	pu := tr.buildClusterPullUp(inner(scan("Order", "o"), scan("Customer", "c")))
	if pu == nil {
		t.Fatal("pull-up spine")
	}
	// The inner scalar key: a folded source alias joined to the SUBQUERY's own
	// output title. The title is whatever the inner select produced, so it can
	// arrive in either case, and the descriptor spelling is the one an exact
	// comparison against a parse-folded projection name cannot reach.
	const innerKey = "SQ.order_id"

	for _, tc := range []struct {
		field string
		want  bool
		why   string
	}{
		{"O.ORDER_ID", true, "a parse-folded reference to a descriptor-spelled leg column"},
		{"O.order_id", true, "the seed's own spelling of that same column"},
		{"C.customer_id", true, "the second leg, so the walk is not stopping at leg one"},
		{"SQ.ORDER_ID", true, "a parse-folded reference to the inner scalar key"},
		{"O.NO_SUCH_COLUMN", false, "a leg that exists cannot resolve a column it does not declare"},
		{"Z.ORDER_ID", false, "a qualifier naming no leg at all"},
		{"ORDER_ID", false, "a bare name: the level-2 row publishes dotted keys only"},
	} {
		if got := clusterFieldResolvable(tc.field, pu, innerKey); got != tc.want {
			t.Errorf("clusterFieldResolvable(%q) = %v, want %v — %s", tc.field, got, tc.want, tc.why)
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
	seed := clusteredOuterOrdinalSeed(pu, values.UniqueCorrelationIdentifier(), "SQ", "ORDER_ID", values.NullableLong)
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
		leg, isLeg := pu.legByBinding[f.Name[:dot]]
		if !isLeg {
			t.Fatalf("outer field %d name %q: prefix is not a leg alias", i, f.Name)
		}
		fv := exactTestFieldView(t, f.Value)
		path := fv.Path().Ordinals()
		if len(path) != 1 || path[0] != i {
			t.Fatalf("outer field %d baked at path %v, want [%d] (one full run)", i, path, i)
		}
		idx, found := leg.typ.FieldIndexUnique(f.Name[dot+1:])
		if !found || leg.start+idx != i {
			t.Fatalf("outer field %d name %q does not map back to global ordinal %d via its leg span", i, f.Name, i)
		}
		qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !ok || qov.Correlation() != pu.outerCorr {
			t.Fatalf("outer field %d baked over %T, want the fresh concat QOV %s", i, fv.ChildValue(), pu.outerCorr)
		}
	}
	last := rc.Fields[wantOuter]
	if last.Name != "SQ.ORDER_ID" {
		t.Errorf("inner field name = %q, want SQ.ORDER_ID", last.Name)
	}
	ifv := exactTestFieldView(t, last.Value)
	if !ifv.ResultType().IsNullable() {
		t.Errorf("inner scalar must be NULLABLE (LEFT-OUTER null-fill), got %s", ifv.ResultType())
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
	gated := clusteredProject(t, inner(scan("Order", "o"), scan("Customer", "c")), clusteredCSQ(t, "c", "customer_id"))
	// Rightmost is O; projections reference o.order_id (dotted) + the csq.
	gated.Projections = []string{"c.customer_id", "(subq)"}
	gated.ProjectedValues[0] = exactDemoRef(t, "c", "customer_id")
	expr := tr.translateProjectWithCorrelatedScalar(gated)
	if expr == nil {
		t.Fatalf("gated cluster + first-leg correlation must translate (it was 0AF00 on the name model): %v", tr.translateErr)
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
	flipped := clusteredProject(t, left, clusteredCSQ(t, "c", "customer_id"))
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
	dupUngated := clusteredProject(t, inner(scan("Order", "x"), scan("Customer", "x")), clusteredCSQ(t, "x", "customer_id"))
	if expr2b := tr2b.translateProjectWithCorrelatedScalar(dupUngated); expr2b != nil {
		t.Fatal("dup-poisoned outer + correlated scalar must decline LOUDLY (there is no anchored fallback), got a translation")
	}
	if tr2b.translateErr == nil || !strings.Contains(tr2b.translateErr.Error(), "duplicate leg bindings") {
		t.Fatalf("dup-poisoned outer decline = %v, want the ungated-cause (gate reason: duplicate leg bindings) decline", tr2b.translateErr)
	}

	// Direction 3: the gated joined-preserved outer + NON-rightmost (buried)
	// correlation translates ordinal too — the buried-leg spans name it.
	tr3 := newGateTranslator(t)
	buried := clusteredProject(t, left, clusteredCSQ(t, "o2", "order_id"))
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
	gatedLeft := clusteredProject(t, singleLeft, clusteredCSQ(t, "c", "customer_id"))
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
		corrEq(t, "o", "order_id", "SQ", "id"))
	csq := logical.CorrelatedScalarSubquery{
		Alias: values.UniqueCorrelationIdentifier(),
		InnerPlan: exactScalarProjection(t,
			logical.NewFilterWithPredicate(innerJoin, corrEq(t, "SQ", "id", "c", "customer_id"), ""),
			"SQ", "id"),
		InnerAlias:   "SQ",
		ScalarCol:    "id",
		StrictSingle: true,
	}
	p := clusteredProject(t, inner(scan("Order", "o"), scan("Customer", "c")), csq)
	p.Projections = []string{"c.customer_id", "(subq)"}
	p.ProjectedValues[0] = exactDemoRef(t, "c", "customer_id")

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
			corrEq(t, "I", "id", "SQ", "order_id")),
		corrEq(t, "SQ", "order_id", "c", "customer_id"), "")
	csq := logical.CorrelatedScalarSubquery{
		Alias:        values.UniqueCorrelationIdentifier(),
		InnerPlan:    exactScalarProjection(t, joinInner, "SQ", "order_id"),
		InnerAlias:   "SQ",
		ScalarCol:    "order_id",
		StrictSingle: true,
	}
	p := &logical.LogicalProject{
		Input:                      scan("Customer", "c"),
		Projections:                []string{"c.name", "(subq)"},
		Aliases:                    []string{"", ""},
		ProjectedValues:            []values.Value{exactDemoRef(t, "c", "name"), values.NewScalarSubqueryValue(csq.Alias, values.NullableLong)},
		IsComputed:                 []bool{false, true},
		CorrelatedScalarSubqueries: []logical.CorrelatedScalarSubquery{csq},
	}
	expr := tr.translateProjectWithCorrelatedScalar(p)
	if expr == nil {
		t.Fatalf("single-source outer + JOIN-inner must translate: %v", tr.translateErr)
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
	lastField := exactTestFieldView(t, rc.Fields[len(rc.Fields)-1].Value)
	lastQOV, ok := values.AsQuantifiedObjectValue(lastField.ChildValue())
	if !ok || lastQOV.Correlation() != innerQCorr {
		t.Fatalf("seed inner leg owner = %T, want quantifier correlation %s", lastField.ChildValue(), innerQCorr)
	}
}
