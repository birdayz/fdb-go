package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 QP-REF-BIND item 2, commit 5a — the under-existential unnest
// absorbed class. A single-source lateral unnest under WHERE-EXISTS used to be
// forced name-model (t.unnestUnderExistential declined the W4c ordinal seed,
// because the existential rebase read outer-leg refs by name and panicked on
// baked ofOrdinal refs). Commit 5a gates it ordinal like any other single-
// source unnest: the mixed seed now carries executor windows, so the EXISTS
// correlation's outer-leg refs stay LEG-RELATIVE and the executor rebases them
// positionally — no translator prediction.

// TestRFC173Item2C5a_UnnestUnderExistsGatesOrdinal proves the decline lift: a
// single-source unnest translated with unnestUnderExistential=true now seeds
// the ORDINAL RC (baked outer run + direct-QOV element), not the name-model
// anchored record. Pre-5a this returned the anchored seed.
func TestRFC173Item2C5a_UnnestUnderExistsGatesOrdinal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	// FROM Order o, o.tags AS X — single source with a real array column, under
	// an existential.
	j := logical.NewJoin(scan("Order", "o"),
		&logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"},
		logical.JoinInner, "")
	tr.unnestUnderExistential = true
	expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
	if expr == nil {
		t.Fatalf("translation failed: %v", tr.translateErr)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("unnest seed = %T, want an RC", sel.GetResultValue())
	}
	if rc.AnchoredJoin {
		t.Fatal("unnest under EXISTS seeded the ANCHORED (name-model) RC — the decline lift did not fire")
	}
	// Every OUTER field is a baked frontier-pinned ofOrdinal (the ordinal seed).
	outerType := tr.ordinalLegType(scan("Order", "o"))
	for i := 0; i < len(outerType.Fields); i++ {
		fv, isFV := rc.Fields[i].Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			t.Fatalf("outer field %d = %T, want a baked frontier-pinned ofOrdinal", i, rc.Fields[i].Value)
		}
	}
}

// TestRFC173Item2C5a_SeedWindowAuthority pins the structural invariant: BOTH the
// mixed no-AT seed and the fully-baked AS+AT seed yield executor windows
// (values.OrdinalSeedLegWindows non-nil), so the translator NEVER pre-rebases an
// ordinal EXISTS correlation — the executor's positional rebase is the ONE
// authority. The mixed seed's bare-QOV element gets its OWN synthesized 1-field
// window (the emergent fix that dissolved the shadow / outer-only / collision
// declines). This is the planner side of the cross-agreement invariant with the
// executor's unnestMixedSeedSpans.
func TestRFC173Item2C5a_SeedWindowAuthority(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outerCorr := values.NamedCorrelationIdentifier("o")
	innerCorr := values.NamedCorrelationIdentifier("X")
	outer := scan("Order", "o")

	// Mixed no-AT seed: now yields windows — the baked outer prefix PLUS a
	// synthesized 1-field element window at the last slot (keyed by the AS alias).
	mixed := tr.unnestOrdinalSeed(outer, outerCorr, innerCorr,
		&logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, values.NotNullString)
	mixedRC, ok := mixed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("mixed seed = %T, want an RC", mixed)
	}
	w, mt := values.OrdinalSeedLegWindows(mixedRC)
	if w == nil {
		t.Fatal("mixed no-AT seed yielded NO executor windows — the structural fix requires the synthesized element window (translator must not pre-rebase)")
	}
	n := len(mixedRC.Fields)
	elemWin, hasElem := w["X"]
	if !hasElem || elemWin.Offset != n-1 || len(elemWin.Typ.Fields) != 1 {
		t.Fatalf("mixed seed element window = %+v (present=%v), want a 1-field window at slot %d keyed by the AS alias X", elemWin, hasElem, n-1)
	}
	if mt == nil || len(mt.Fields) != n {
		t.Fatalf("mixed seed merged type = %v, want %d fields", mt, n)
	}

	// Fully-baked AS+AT seed: NON-nil windows (pristine path, unchanged).
	atSeed := tr.unnestOrdinalSeed(outer, outerCorr, innerCorr,
		&logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X", AtAlias: "O"}, values.NotNullString)
	atRC, ok := atSeed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("AT seed = %T, want an RC", atSeed)
	}
	if w, _ := values.OrdinalSeedLegWindows(atRC); w == nil {
		t.Fatal("fully-baked AS+AT seed yielded NO executor windows — it must be non-nil (pristine path)")
	}
}

// TestRFC173Item2C5a_MultiAliasOuterGatesOrdinal pins the RFC-173 S4 Step-B
// COUPLED TWO-AXIS FLIP. A merge-opaque FULL OUTER box has clusterArity 1 but
// binds TWO aliases; before Step B such an outer stayed NAME-MODEL under EXISTS
// because the ordinal rebase resolved a column by flat first-match and could not
// disambiguate the two legs' same-named columns. Step B lifts that: a
// fresh-gating OUTER box (boxGatesFresh) now BIRTHS a positional row (AXIS 1 —
// the box-outer translateRef clears the :167 enclosure) AND the seed is admitted
// (AXIS 2 — unnestExistsSeedSafe), so the ordinal seed FIRES and the per-leg
// windows (channels 1+2) disambiguate the dup-named legs by their [Start,Width)
// windows. The two axes flip TOGETHER through the one boxGatesFresh predicate;
// the FDB e2e (TestFDB_RFC173S4_C5a_FullOuterUnnestExists) proves the rows are
// correct-leg-bound (a qualified ref to the null-supplied leg resolves to THAT
// leg) and that the multi-source INNER cluster (`FROM A, B, A.arr AS x`) still
// stays name-model (the R1 hazard boxGatesFresh excludes via JoinInner).
func TestRFC173Item2C5a_MultiAliasOuterGatesOrdinal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	// FROM (Order o FULL OUTER JOIN Customer c), o.TAGS AS X — a 2-alias outer.
	outer := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
	j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, logical.JoinInner, "")
	tr.unnestUnderExistential = true
	expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
	if expr == nil {
		t.Fatalf("translation failed: %v", tr.translateErr)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("unnest seed = %T, want an RC", sel.GetResultValue())
	}
	if rc.AnchoredJoin {
		t.Fatal("fresh-gating FULL OUTER outer under EXISTS seeded the ANCHORED RC — Step B must birth it POSITIONAL (ordinal seed) so the per-leg windows disambiguate the dup-named legs")
	}
	// The ordinal seed must carry per-leg windows so the box's two legs
	// disambiguate positionally (channels 1+2). A nil window set would mean the
	// seed fell back to flat name-resolution — the very ambiguity Step B fixes.
	if w, _ := values.OrdinalSeedLegWindows(rc); w == nil {
		t.Fatal("Step-B ordinal seed yielded NO executor windows — the dup-named box legs cannot disambiguate without them")
	}
}

// TestRFC173Item2C5a_MultiSourceInnerClusterStaysNameModel is the R1 NEGATIVE
// pin for Step B: a MULTI-SOURCE INNER cluster (`FROM A, B, A.arr AS x`) is NOT
// a fresh-gating outer box (boxGatesFresh excludes JoinInner), so it stays
// NAME-MODEL under EXISTS. Admitting it would gate the cluster ordinal while its
// seed still declines the flattened multi-source outer — wrong rows (R1's
// :5117-5119 hazard). This must hold in the SAME commit as the FULL-box lift.
func TestRFC173Item2C5a_MultiSourceInnerClusterStaysNameModel(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	// FROM (Order o INNER JOIN Customer c), o.TAGS AS X — a 2-alias INNER outer.
	outer := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, "")
	j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, logical.JoinInner, "")
	tr.unnestUnderExistential = true
	expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
	if expr == nil {
		t.Fatalf("translation failed: %v", tr.translateErr)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("unnest seed = %T, want an RC", sel.GetResultValue())
	}
	if !rc.AnchoredJoin {
		t.Fatal("multi-source INNER cluster under EXISTS seeded the ORDINAL RC — boxGatesFresh must exclude JoinInner (R1: the seed still declines the multi-source outer, so an ordinal cluster gate is wrong rows)")
	}
}

// TestRFC173Item2C5a_BoxGatePredicates directly pins the two Step-B predicates,
// converting the R1 and AXIS-1-coupling guards into REAL regression sentinels.
// The AnchoredJoin/rows observables alone are MASKED: an INNER cluster's seed
// declines via clusterArity>=2 regardless of boxGatesFresh, and a LEFT/RIGHT
// box's seed likewise declines via clusterArity — so deleting the JoinInner
// exclusion or widening AXIS 1 back to boxGatesFresh would leave those tests
// green while re-introducing the "positional box under a name-model builder"
// wrong-rows defect. These direct assertions turn such a change RED:
//   - boxGatesFresh gates every OUTER box (LEFT/RIGHT/FULL) fresh, never INNER
//     (the R1 exclusion).
//   - boxOuterBirthsPositional (the AXIS-1 birth condition) is true ONLY for a
//     FULL box (clusterArity==1). A LEFT/RIGHT box gates fresh yet must NOT birth
//     positional (clusterArity>=2 → its seed stays name-model); an INNER cluster
//     neither gates fresh nor births.
func TestRFC173Item2C5a_BoxGatePredicates(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	tr.unnestUnderExistential = true
	mk := func(kind logical.JoinKind) logical.LogicalOperator {
		return logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), kind, "")
	}
	for _, tc := range []struct {
		name        string
		op          logical.LogicalOperator
		gatesFresh  bool
		birthsPosnl bool
	}{
		{"FULL", mk(logical.JoinFull), true, true},
		{"LEFT", mk(logical.JoinLeft), true, false},
		{"RIGHT", mk(logical.JoinRight), true, false},
		{"INNER", mk(logical.JoinInner), false, false},
	} {
		if got := tr.boxGatesFresh(tc.op); got != tc.gatesFresh {
			t.Errorf("boxGatesFresh(%s) = %v, want %v", tc.name, got, tc.gatesFresh)
		}
		if got := tr.boxOuterBirthsPositional(tc.op); got != tc.birthsPosnl {
			t.Errorf("boxOuterBirthsPositional(%s) = %v, want %v", tc.name, got, tc.birthsPosnl)
		}
	}

	// A FULL box with a CLUSTERED (INNER-join) LEG now ORDINALIZES (c5b): its
	// buried columns ARE concatenated and a buried EXISTS ref resolves by window
	// (verified e2e). So it ADMITS (gates + births positional). A regular WHERE
	// conjunct on a buried leg is separately declined by the outer-conjunct
	// narrowing.
	clusteredFull := logical.NewJoin(
		logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, "")
	if !tr.boxGatesFresh(clusteredFull) {
		t.Error("boxGatesFresh(clustered-INNER-leg FULL) = false, want true — an INNER-cluster buried leg's leaves are windowed (c5b)")
	}
	if !tr.boxOuterBirthsPositional(clusteredFull) {
		t.Error("boxOuterBirthsPositional(clustered-INNER-leg FULL) = false, want true — c5b admits the buried INNER cluster")
	}

	// A nested OUTER box buried INSIDE an admitted INNER cluster
	// (`((Order LEFT Customer) JOIN TypedRecord) FULL OUTER scan`) is ALSO admitted
	// — the peel is intentionally shallow, and the machinery recurses to
	// null-supply the nested LEFT box through the positional birth (verified). This
	// pins that legExposesBuriedOuterBox is NOT recursive — a future "tightening"
	// into a recursive exclusion would wrongly decline this working shape.
	nestedOuterInInner := logical.NewJoin(
		logical.NewJoin(
			logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""),
			scan("TypedRecord", "d"), logical.JoinInner, ""),
		scan("Order", "o2"), logical.JoinFull, "")
	if !tr.boxGatesFresh(nestedOuterInInner) {
		t.Error("boxGatesFresh(nested-OUTER-in-INNER FULL) = false, want true — an outer box buried inside an INNER cluster ordinalizes correctly (shallow peel)")
	}

	// EXCLUDED buried-OUTER-box legs: a DIRECT outer box (nested-FULL), and any
	// WRAPPED join (Filter/Project over a join) — ordinalLegType records buried
	// bounds only for a DIRECT LogicalJoin leg, so a wrapped inner cluster would
	// birth positional without its buried windows. The wrapper-peel remembers it
	// peeled: a wrapped join is excluded regardless of kind.
	leftBox := func() logical.LogicalOperator {
		return logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, "")
	}
	innerCluster := func() logical.LogicalOperator {
		return logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, "")
	}
	for _, tc := range []struct {
		name string
		leg  logical.LogicalOperator
	}{
		{"nested-FULL (direct outer)", logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")},
		{"Filter(LEFT-box)", logical.NewFilter(leftBox(), "1 = 1")},
		{"Project(LEFT-box)", logical.NewProject(leftBox(), []string{"order_id"}, []string{""})},
		{"Filter(INNER-cluster)", logical.NewFilter(innerCluster(), "1 = 1")}, // wrapped join, no buried windows
		{"Project(INNER-cluster)", logical.NewProject(innerCluster(), []string{"order_id"}, []string{""})},
		// A WRAPPED join buried INSIDE an admitted INNER cluster —
		// `(Filter(A JOIN B) JOIN C) FULL OUTER D`. The top leg is a direct INNER
		// cluster (admitted by kind), but its Filter-wrapped sub-join gets no
		// buried windows → excluded by the recursive hasWrappedBuriedJoin walk. A
		// shallow (non-recursive) INNER admit would wrongly let this birth positional.
		{"wrapped-join-in-INNER", logical.NewJoin(logical.NewFilter(innerCluster(), "1 = 1"), scan("TypedRecord", "e"), logical.JoinInner, "")},
	} {
		box := logical.NewJoin(tc.leg, scan("TypedRecord", "d"), logical.JoinFull, "")
		if tr.boxGatesFresh(box) {
			t.Errorf("boxGatesFresh(%s FULL OUTER scan) = true, want false — a wrapped join / direct outer box is not windowed here", tc.name)
		}
		if tr.boxOuterBirthsPositional(box) {
			t.Errorf("boxOuterBirthsPositional(%s FULL OUTER scan) = true, want false", tc.name)
		}
	}
}

// TestRFC173Item2C5b_ClusteredBoxSeedsOrdinal is the white-box pin that the
// clustered-INNER FULL box's SEED actually fires ORDINAL. The FDB e2e's {7,8} is
// OVER-DETERMINED (name-model yields it too via qualified keys — the prior
// commit's name-model clustered pin proved that), and BoxGatePredicates proves
// only that the GATE opens, not that unnestOrdinalSeed produced an ordinal RC. A
// revert at the seed-gate / AXIS-1 call site (not touching boxGatesFresh) would
// leave both green — this closes that silent gap, mirroring
// MultiAliasOuterGatesOrdinal for the clustered shape.
func TestRFC173Item2C5b_ClusteredBoxSeedsOrdinal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	tr.unnestUnderExistential = true
	// FROM ((Order o INNER JOIN Customer c) FULL OUTER JOIN TypedRecord d),
	// o.TAGS AS X — the clustered-INNER FULL box, NO outer conjunct.
	outer := logical.NewJoin(
		logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, "")
	j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, logical.JoinInner, "")
	expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
	if expr == nil {
		t.Fatalf("translation failed: %v", tr.translateErr)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("unnest seed = %T, want an RC", sel.GetResultValue())
	}
	if rc.AnchoredJoin {
		t.Fatal("clustered-INNER FULL box under EXISTS seeded the ANCHORED RC — c5b must birth it ORDINAL (a seed-gate revert the over-determined {7,8} e2e cannot catch)")
	}
	if w, _ := values.OrdinalSeedLegWindows(rc); w == nil {
		t.Fatal("clustered-INNER ordinal seed yielded NO executor windows — the buried leaves cannot resolve without them")
	}
}

// TestRFC173Item2C5a_ShadowAliasGatesOrdinal pins the emergent shadow-column fix.
// When the unnest AS alias equals an outer column name (`o.TAGS AS PRICE` over
// Order, which has a PRICE column), the mixed seed still GATES ORDINAL — it is NOT
// declined. The element gets its own synthesized 1-field window (keyed by the AS
// alias PRICE) DISTINCT from the outer PRICE column's window, so the element ref
// binds POSITIONALLY (executor hoist), never name-resolving over the duplicate
// "PRICE" columns. This is the structural fix that replaced the old shadow decline
// (previously this seeded ANCHORED). The two windows carry the same NAME but
// distinct correlations (outer table alias vs the element correlation), so there
// is no collision.
func TestRFC173Item2C5a_ShadowAliasGatesOrdinal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	// FROM Order o, o.TAGS AS PRICE — the AS alias shadows Order's PRICE column.
	j := logical.NewJoin(scan("Order", "o"),
		&logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "PRICE"},
		logical.JoinInner, "")
	tr.unnestUnderExistential = true
	expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
	if expr == nil {
		t.Fatalf("translation failed: %v", tr.translateErr)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("unnest seed = %T, want an RC", sel.GetResultValue())
	}
	if rc.AnchoredJoin {
		t.Fatal("shadowing AS alias under EXISTS seeded the ANCHORED RC — the structural fix must keep it ORDINAL and give the element its own positional window (no decline)")
	}
	// The element gets its OWN 1-field window at the last slot (offset n-1),
	// distinct from the outer PRICE column's window — so the element binds
	// positionally, never name-resolving over the duplicate "PRICE" columns.
	w, _ := values.OrdinalSeedLegWindows(rc)
	if w == nil {
		t.Fatal("shadowing mixed seed yielded NO windows — it must, so the element binds positionally")
	}
	hasElemWin := false
	for _, win := range w {
		if win.Offset == len(rc.Fields)-1 && len(win.Typ.Fields) == 1 {
			hasElemWin = true
		}
	}
	if !hasElemWin {
		t.Fatalf("no synthesized 1-field element window at slot %d — the element would name-resolve over the shadowed name", len(rc.Fields)-1)
	}
}

// TestRFC173S4_OrdinalSlotInLegWindow pins the per-leg-window rebase primitive —
// the FIRST of the two coupled channels the multi-alias-under-EXISTS lift needs
// (design review scoping; the second is the RULE-level below-FOD executor hoist).
// A qualified outer-leg ref into a MULTI-ALIAS outer (rt.Legs populated) resolves
// to the NAMED leg's slot, not the flat first-match, so two aliases' same-named
// columns disambiguate. A single-alias outer (no rt.Legs) keeps the flat lookup.
func TestRFC173S4_OrdinalSlotInLegWindow(t *testing.T) {
	t.Parallel()
	// Merged prefix of two legs A[0,2) and B[2,2), BOTH with dup-named ID + X —
	// built directly (NewRecordType would reject the duplicate field names).
	rt := &values.RecordType{
		Fields: []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 3},
		},
		Legs: []values.RecordTypeLeg{{Name: "A", Start: 0, Width: 2}, {Name: "B", Start: 2, Width: 2}},
	}
	cases := []struct {
		leg, field string
		want       int
		wantOK     bool
	}{
		{"A", "ID", 0, true},
		{"B", "ID", 2, true}, // the fix: NOT the flat first-match 0
		{"A", "X", 1, true},
		{"B", "X", 3, true},
		{"B", "ZZZ", 0, false}, // qualified ref to a column absent from B's window
		{"C", "ID", 0, false},  // qualifier NOT among the legs: LOUD decline, NOT flat first-match 0
	}
	for _, c := range cases {
		got, ok := ordinalSlotInLegWindow(rt, c.leg, c.field)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("ordinalSlotInLegWindow(%s.%s) = (%d,%v), want (%d,%v)", c.leg, c.field, got, ok, c.want, c.wantOK)
		}
	}
	// SINGLE-alias outer (no rt.Legs): flat FieldIndex, backward-compatible.
	single := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	if got, ok := ordinalSlotInLegWindow(single, "T", "V"); !ok || got != 1 {
		t.Errorf("single-leg ordinalSlotInLegWindow(T.V) = (%d,%v), want (1,true)", got, ok)
	}
	// MALFORMED window (negative Start): decline, never index at a negative slot.
	bad := &values.RecordType{
		Fields: []values.Field{{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0}},
		Legs:   []values.RecordTypeLeg{{Name: "A", Start: -1, Width: 2}},
	}
	if _, ok := ordinalSlotInLegWindow(bad, "A", "ID"); ok {
		t.Error("negative-Start leg window must decline, not panic or resolve")
	}
}

// TestRFC173S4_ThreeWayBoxCrossAgreement pins the 3-way cross-agreement the
// Step-B guard lift requires: channel-1's ordinalSlotInLegWindow (over
// ordinalLegType's box .Legs) and channel-2-values' OrdinalSeedLegWindows (over the
// baked box mixed seed) must resolve EVERY box-leaf column to the SAME absolute
// slot. The existing executor fixture pins channel-2-values <-> channel-2-executor;
// this closes channel-1 <-> channel-2-values, so all three walks agree. A drift is
// a silent wrong-alias slot on a dup-named column the instant the guard lifts. The
// box outer is built directly (bypassing unnestExistsSeedSafe, which still declines
// it end-to-end) — a white-box layout pin, not a dispatch change.
func TestRFC173S4_ThreeWayBoxCrossAgreement(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
	boxType := tr.ordinalLegType(box)
	if boxType == nil || len(boxType.Legs) < 2 {
		t.Fatalf("box ordinalLegType has no per-leg boundaries: %+v", boxType)
	}
	// The box mixed seed: the baked box concat over one box QOV + a bare-QOV element
	// (o.TAGS AS X). The box QOV is keyed by its rightmost leaf (sourceBinding "c").
	innerCorr := values.UniqueCorrelationIdentifier()
	u := &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}
	seedVal := tr.unnestOrdinalSeed(box, values.NamedCorrelationIdentifier("c"), innerCorr, u, values.NotNullString)
	seed, ok := seedVal.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("box mixed seed = %T, want an RC (translateErr=%v)", seedVal, tr.translateErr)
	}
	windows, _ := values.OrdinalSeedLegWindows(seed)
	if windows == nil {
		t.Fatal("channel-2 OrdinalSeedLegWindows declined the box mixed seed")
	}
	// For every box leaf + column: channel-1 slot == channel-2-values slot.
	for _, leg := range boxType.Legs {
		w, present := windows[leg.Name]
		if !present {
			t.Fatalf("box leaf %s absent from channel-2 windows %v", leg.Name, windows)
		}
		for _, f := range w.Typ.Fields {
			ch1, ok1 := ordinalSlotInLegWindow(boxType, leg.Name, f.Name)
			ci, okc := w.Typ.FieldIndex(f.Name)
			ch2 := w.Offset + ci
			if !ok1 || !okc || ch1 != ch2 {
				t.Fatalf("3-way DRIFT: leaf %s col %s — channel-1 slot %d (ok=%v) vs channel-2 slot %d", leg.Name, f.Name, ch1, ok1, ch2)
			}
		}
	}
}

// TestRFC173Item2C5a_OuterConjunctNarrowing pins the outer-conjunct regression
// narrowing at the TRANSLATOR level — the FDB e2e's {7,8} is OVER-DETERMINED
// (both the ordinal and the name-model path yield it), so only a white-box
// AnchoredJoin / detection assertion discriminates whether the ordinal path
// fired. Two independently-breakable halves:
//   - DETECTION (nonExistsConjunctRefsOuterLeg): a box-leg or flow-leg conjunct
//     TRIPS; an element-only conjunct does NOT (else a multi-alias box with an
//     element-only WHERE would SILENTLY fall to name-model, losing the ordinal
//     optimization with the answer unchanged — the masked over-decline).
//   - CONSUMPTION (the unnestOuterConjunctOnBoxLeg flag in
//     unnestExistsSeedSafe): flag SET → the FULL box seeds name-model
//     (AnchoredJoin); flag CLEAR → ordinal (the anti-over-decline positive pin).
func TestRFC173Item2C5a_OuterConjunctNarrowing(t *testing.T) {
	t.Parallel()

	// DETECTION sentinel. FOA/FOB are the box legs; X is the unnest element.
	boxAliases := map[string]struct{}{"FOA": {}, "FOB": {}}
	for _, tc := range []struct {
		name string
		pred predicates.QueryPredicate
		want bool
	}{
		{"box-leg", corrEq("FOA", "K", "FOC", "CK"), true},
		{"flow-leg", corrEq("FOB", "K", "FOC", "CK"), true},
		{"element-only", corrEq("X", "EL", "X", "EL2"), false},
		{"and-elements", predicates.NewAnd(corrEq("X", "EL", "X", "EL2"), corrEq("X", "A", "X", "B")), false},
		{"and-mixed", predicates.NewAnd(corrEq("X", "EL", "X", "EL2"), corrEq("FOA", "K", "FOC", "CK")), true},
		{"nil", nil, false},
	} {
		if got := nonExistsConjunctRefsOuterLeg(tc.pred, boxAliases); got != tc.want {
			t.Errorf("nonExistsConjunctRefsOuterLeg(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	// CONSUMPTION: the flag flips the FULL box from ordinal to name-model, in
	// BOTH the EXISTS and non-EXISTS paths (the flag is checked in
	// unnestExistsSeedSafe BEFORE the under-existential gate). FROM
	// (Order o FULL OUTER Customer c), o.TAGS AS X.
	seed := func(underExists, flag bool) *values.RecordConstructorValue {
		tr := newGateTranslator(t)
		tr.unnestUnderExistential = underExists
		tr.unnestOuterConjunctOnBoxLeg = flag
		outer := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
		j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, logical.JoinInner, "")
		expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
		if expr == nil {
			t.Fatalf("translation failed (underExists=%v flag=%v): %v", underExists, flag, tr.translateErr)
		}
		sel, ok := expr.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
		}
		rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
		if !ok {
			t.Fatalf("unnest seed = %T, want an RC", sel.GetResultValue())
		}
		return rc
	}
	for _, underExists := range []bool{true, false} {
		// flag CLEAR (element-only conjunct, or none): the box ORDINALIZES — the
		// anti-over-decline pin the {7,8} e2e structurally cannot provide.
		if seed(underExists, false).AnchoredJoin {
			t.Errorf("underExists=%v flag clear: FULL box seeded ANCHORED — the narrowing over-declined (element-only / no conjunct must still ordinalize)", underExists)
		}
		// flag SET (a box-leg conjunct): the box declines to NAME-MODEL. This must
		// hold for the NON-EXISTS path too (underExists=false) — the sibling
		// regression (a plain WHERE on a box leg over an ordinal box).
		if !seed(underExists, true).AnchoredJoin {
			t.Errorf("underExists=%v flag set: FULL box seeded ORDINAL — the narrowing did not fire (a box-leg conjunct must decline to name-model)", underExists)
		}
	}
}
