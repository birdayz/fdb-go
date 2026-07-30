package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The under-existential unnest. A single-source lateral unnest under
// WHERE-EXISTS used to be forced name-model (t.unnestUnderExistential
// declined the ordinal seed, because the existential rebase read outer-leg
// refs by name and panicked on baked ofOrdinal refs). It now gates ordinal
// like any other single-source unnest: the mixed seed carries executor
// windows, so the EXISTS correlation's outer-leg refs stay LEG-RELATIVE and
// the executor rebases them positionally — no translator prediction.

// TestUnnestUnderExistsGatesOrdinal proves the decline lift: a single-source
// unnest translated with unnestUnderExistential=true seeds the ORDINAL RC
// (baked outer run + direct-QOV element), never the name-model anchored
// record.
func TestUnnestUnderExistsGatesOrdinal(t *testing.T) {
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
	// Every OUTER field is a baked frontier-pinned ofOrdinal (the ordinal seed).
	outerType := tr.ordinalLegType(scan("Order", "o"))
	for i := 0; i < len(outerType.Fields); i++ {
		fv, isFV := rc.Fields[i].Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			t.Fatalf("outer field %d = %T, want a baked frontier-pinned ofOrdinal", i, rc.Fields[i].Value)
		}
	}
}

// TestSeedWindowAuthority pins the structural invariant: BOTH the
// mixed no-AT seed and the fully-baked AS+AT seed yield executor windows
// (values.OrdinalSeedLegWindows non-nil), so the translator NEVER pre-rebases an
// ordinal EXISTS correlation — the executor's positional rebase is the ONE
// authority. The mixed seed's bare-QOV element gets its OWN synthesized 1-field
// window (the emergent fix that dissolved the shadow / outer-only / collision
// declines). This is the planner side of the cross-agreement invariant with the
// executor's unnestMixedSeedSpans.
func TestSeedWindowAuthority(t *testing.T) {
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

// TestMultiAliasOuterGatesOrdinal pins the coupled fix that lets a
// multi-alias OUTER box gate under EXISTS. A merge-opaque FULL OUTER box has
// clusterArity 1 but binds TWO aliases; naively rebasing such an outer under
// EXISTS resolves a column by flat first-match and cannot disambiguate the
// two legs' same-named columns. The fix couples two conditions on the SAME
// boxGatesFresh predicate: a fresh-gating OUTER box (1) BUILDS a positional
// row (the box-outer translateRef clears its enclosure) AND (2) has its seed
// admitted (unnestExistsSeedSafe), so the ordinal seed FIRES and the per-leg
// windows disambiguate the dup-named legs by their [Start,Width) windows.
// Both conditions flip TOGETHER through the one boxGatesFresh predicate; the
// sqldriver FDB full-outer-unnest-exists scenario proves the rows are
// correct-leg-bound (a qualified ref to the null-supplied leg resolves to
// THAT leg) and that the multi-source INNER cluster (`FROM A, B, A.arr AS
// x`) still stays name-model (boxGatesFresh excludes it via JoinInner).
func TestMultiAliasOuterGatesOrdinal(t *testing.T) {
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
	// The ordinal seed must carry per-leg windows so the box's two legs
	// disambiguate positionally. A nil window set would mean the seed fell
	// back to flat name-resolution — the very ambiguity this fix closes.
	if w, _ := values.OrdinalSeedLegWindows(rc); w == nil {
		t.Fatal("ordinal seed yielded NO executor windows — the dup-named box legs cannot disambiguate without them")
	}
}

// TestMultiSourceInnerClusterDeclines is the negative control: a
// MULTI-SOURCE INNER cluster (`FROM A, B, A.arr AS x`) is NOT a fresh-gating
// outer box (boxGatesFresh excludes JoinInner), so its binary seed DECLINES
// under EXISTS. Admitting it would gate the cluster ordinal while its seed
// still declines the flattened multi-source outer — wrong rows. There is no
// name-model fallback anymore, so the decline is a LOUD nil, never a silent
// name-model seed.
func TestMultiSourceInnerClusterDeclines(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	// FROM (Order o INNER JOIN Customer c), o.TAGS AS X — a 2-alias INNER outer.
	outer := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, "")
	j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, logical.JoinInner, "")
	tr.unnestUnderExistential = true
	if expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)); expr != nil { //nolint:errcheck // fixture
		t.Fatalf("multi-source INNER cluster under EXISTS must DECLINE (correct-or-loud; the name-model fallback is deleted), got %T", expr)
	}
}

// TestBoxGatePredicates directly pins the two gating predicates as REAL
// regression sentinels. The AnchoredJoin/rows observables alone are MASKED:
// an INNER cluster's seed declines via clusterArity>=2 regardless of
// boxGatesFresh, and a LEFT/RIGHT box's seed likewise declines via
// clusterArity — so deleting the JoinInner exclusion, or widening the build
// condition back to plain boxGatesFresh, would leave those tests green while
// re-introducing the "positional box under a name-model builder" wrong-rows
// defect. These direct assertions turn such a change RED:
//   - boxGatesFresh gates every OUTER box (LEFT/RIGHT/FULL) fresh, never INNER.
//   - boxOuterBuildsPositional (the build condition) is true ONLY for a FULL
//     box (clusterArity==1). A LEFT/RIGHT box gates fresh yet must NOT build
//     positional (clusterArity>=2 → its seed stays name-model); an INNER cluster
//     neither gates fresh nor builds.
func TestBoxGatePredicates(t *testing.T) {
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
		buildsPosnl bool
	}{
		{"FULL", mk(logical.JoinFull), true, true},
		{"LEFT", mk(logical.JoinLeft), true, false},
		{"RIGHT", mk(logical.JoinRight), true, false},
		{"INNER", mk(logical.JoinInner), false, false},
	} {
		if got := tr.boxGatesFresh(tc.op); got != tc.gatesFresh {
			t.Errorf("boxGatesFresh(%s) = %v, want %v", tc.name, got, tc.gatesFresh)
		}
		if got := tr.boxOuterBuildsPositional(tc.op); got != tc.buildsPosnl {
			t.Errorf("boxOuterBuildsPositional(%s) = %v, want %v", tc.name, got, tc.buildsPosnl)
		}
	}

	// A FULL box with a CLUSTERED (INNER-join) LEG ORDINALIZES: its buried
	// columns ARE concatenated and a buried EXISTS ref resolves by window
	// (verified end-to-end). So it ADMITS (gates + builds positional). A
	// regular WHERE conjunct on a buried leg is separately declined by the
	// outer-conjunct narrowing.
	clusteredFull := logical.NewJoin(
		logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, "",
	)
	if !tr.boxGatesFresh(clusteredFull) {
		t.Error("boxGatesFresh(clustered-INNER-leg FULL) = false, want true — an INNER-cluster buried leg's leaves are windowed")
	}
	if !tr.boxOuterBuildsPositional(clusteredFull) {
		t.Error("boxOuterBuildsPositional(clustered-INNER-leg FULL) = false, want true — the buried INNER cluster is admitted")
	}

	// A nested OUTER box buried INSIDE an admitted INNER cluster
	// (`((Order LEFT Customer) JOIN TypedRecord) FULL OUTER scan`) is ALSO admitted
	// — the peel is intentionally shallow, and the machinery recurses to
	// null-supply the nested LEFT box through the positional build (verified). This
	// pins that legExposesBuriedOuterBox is NOT recursive — a future "tightening"
	// into a recursive exclusion would wrongly decline this working shape.
	nestedOuterInInner := logical.NewJoin(
		logical.NewJoin(
			logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, ""),
			scan("TypedRecord", "d"), logical.JoinInner, "",
		),
		scan("Order", "o2"), logical.JoinFull, "",
	)
	if !tr.boxGatesFresh(nestedOuterInInner) {
		t.Error("boxGatesFresh(nested-OUTER-in-INNER FULL) = false, want true — an outer box buried inside an INNER cluster ordinalizes correctly (shallow peel)")
	}

	// EXCLUDED buried-OUTER-box legs: a DIRECT outer box (nested-FULL), and any
	// WRAPPED join (Filter/Project over a join) — ordinalLegType records buried
	// bounds only for a DIRECT LogicalJoin leg, so a wrapped inner cluster would
	// build positional without its buried windows. The wrapper-peel remembers it
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
		// shallow (non-recursive) INNER admit would wrongly let this build positional.
		{"wrapped-join-in-INNER", logical.NewJoin(logical.NewFilter(innerCluster(), "1 = 1"), scan("TypedRecord", "e"), logical.JoinInner, "")},
	} {
		box := logical.NewJoin(tc.leg, scan("TypedRecord", "d"), logical.JoinFull, "")
		if tr.boxGatesFresh(box) {
			t.Errorf("boxGatesFresh(%s FULL OUTER scan) = true, want false — a wrapped join / direct outer box is not windowed here", tc.name)
		}
		if tr.boxOuterBuildsPositional(box) {
			t.Errorf("boxOuterBuildsPositional(%s FULL OUTER scan) = true, want false", tc.name)
		}
	}
}

// TestClusteredBoxSeedsOrdinal is the white-box pin that the clustered-INNER
// FULL box's SEED actually fires ORDINAL. The end-to-end rows alone are
// OVER-DETERMINED (the name-model path yields the same rows too, via
// qualified keys), and TestBoxGatePredicates proves only that the GATE
// opens, not that unnestOrdinalSeed produced an ordinal RC. A revert at the
// seed-gate call site (not touching boxGatesFresh) would leave both green —
// this closes that silent gap, mirroring TestMultiAliasOuterGatesOrdinal for
// the clustered shape.
func TestClusteredBoxSeedsOrdinal(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	tr.unnestUnderExistential = true
	// FROM ((Order o INNER JOIN Customer c) FULL OUTER JOIN TypedRecord d),
	// o.TAGS AS X — the clustered-INNER FULL box, NO outer conjunct.
	outer := logical.NewJoin(
		logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, "",
	)
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
	if w, _ := values.OrdinalSeedLegWindows(rc); w == nil {
		t.Fatal("clustered-INNER ordinal seed yielded NO executor windows — the buried leaves cannot resolve without them")
	}
}

// TestShadowAliasGatesOrdinal pins the emergent shadow-column fix.
// When the unnest AS alias equals an outer column name (`o.TAGS AS PRICE` over
// Order, which has a PRICE column), the mixed seed still GATES ORDINAL — it is NOT
// declined. The element gets its own synthesized 1-field window (keyed by the AS
// alias PRICE) DISTINCT from the outer PRICE column's window, so the element ref
// binds POSITIONALLY (executor hoist), never name-resolving over the duplicate
// "PRICE" columns. This is the structural fix that replaced the old shadow decline
// (previously this seeded ANCHORED). The two windows carry the same NAME but
// distinct correlations (outer table alias vs the element correlation), so there
// is no collision.
func TestShadowAliasGatesOrdinal(t *testing.T) {
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

// TestOrdinalSlotInLegWindow pins the per-leg-window rebase primitive — one of
// two coordinating mechanisms the multi-alias-under-EXISTS fix needs; the
// other is the executor's below-FOD hoist rule. A qualified outer-leg ref
// into a MULTI-ALIAS outer (rt.Legs populated) resolves to the NAMED leg's
// slot, not the flat first-match, so two aliases' same-named columns
// disambiguate. A single-alias outer (no rt.Legs) keeps the flat lookup.
func TestOrdinalSlotInLegWindow(t *testing.T) {
	t.Parallel()
	// Merged prefix of two legs A[0,2) and B[2,2), BOTH with dup-named ID + X —
	// built directly (NewRecordType would reject the duplicate field names).
	rt := &values.RecordType{
		Fields: []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 3},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 4},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 5},
		},
		// The legs STATE their identities: the resolver asks an identity question, so
		// a fixture that supplied only text would be testing a code path that no
		// longer exists.
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("A"), "A", 0, 2),
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("B"), "B", 2, 2),
			// A PLANNER-MINTED leg: lowercase identity, UPPER text. The two namespaces
			// are disjoint by construction and this leg is where that matters.
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("q$5"), "Q$5", 4, 2),
		},
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
		// A case-variant of a leg's identity is a DIFFERENT leg. The alias namespaces
		// are deliberately case-disjoint, and this resolver decides which slot a
		// qualified read probes, so a fold here reads another leg's same-named column.
		{"b", "ID", 0, false},
		// The FORGERY shape, which is the one a text comparison cannot decline. Leg
		// "q$5" below has a MINTED (lowercase) identity and carries the UPPER fold as
		// its Name — the real shape, since the text channel is upper and
		// UniqueCorrelationIdentifier mints lowercase. A quoted user alias "Q$5" is a
		// different leg, but it equals that leg's NAME exactly, so any comparison
		// against Name accepts it and the read lands in the minted leg's window.
		//
		// This is the case that makes the identity conversion load-bearing rather than
		// cosmetic: the plain-text and upper-folded forms both MATCH here, and only
		// values.SameLeg against the leg's own identifier declines.
		{"Q$5", "ID", 0, false},
		{"q$5", "ID", 4, true}, // the minted leg's OWN identity still resolves
	}
	for _, c := range cases {
		got, ok := ordinalSlotInLegWindow(rt, values.NamedCorrelationIdentifier(c.leg), c.field, true)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("ordinalSlotInLegWindow(%s.%s) = (%d,%v), want (%d,%v)", c.leg, c.field, got, ok, c.want, c.wantOK)
		}
	}
	// SINGLE-alias outer (no rt.Legs): flat FieldIndex, backward-compatible.
	single := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	if got, ok := ordinalSlotInLegWindow(single, values.NamedCorrelationIdentifier("T"), "V", false); !ok || got != 1 {
		t.Errorf("single-leg ordinalSlotInLegWindow(T.V) = (%d,%v), want (1,true)", got, ok)
	}
	// MALFORMED window (negative Start): decline, never index at a negative slot.
	bad := &values.RecordType{
		Fields: []values.Field{{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0}},
		Legs:   []values.RecordTypeLeg{{Name: "A", Start: -1, Width: 2}},
	}
	if _, ok := ordinalSlotInLegWindow(bad, values.NamedCorrelationIdentifier("A"), "ID", true); ok {
		t.Error("negative-Start leg window must decline, not panic or resolve")
	}
	// THE FAIL-CLOSED HARDENING, both sides. A MULTI-alias prefix that arrives
	// WITHOUT leg windows must DECLINE — the flat fallback would first-match a
	// dup-named column across aliases (a qualified B.ID resolving to A's slot 0).
	// REACHABILITY NOTE: on the chained/box paths Legs DO propagate
	// (buriedLegBounds handles unnest-right joins and boxes), so no currently
	// reachable shape hits this decline; it is a defensive fail-closed law
	// guarding other paths and future propagation gaps, not a live-path fix.
	// (A distinct bug can produce a similar-looking symptom — WHERE B.ID=20
	// returning {A.ID:20, B.ID:null} — via the lazy scan-pushable fork leaving
	// a foreign-correlation name read; that is a different mechanism from the
	// window-less decline pinned here.)
	// The SAME window-less type resolves flat when the caller is single-alias —
	// the legitimate pristine-prefix path must NOT break.
	noLegs := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SUB", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "SUB", FieldType: values.NotNullLong, Ordinal: 3},
	}}
	if _, ok := ordinalSlotInLegWindow(noLegs, values.NamedCorrelationIdentifier("B"), "ID", true); ok {
		t.Error("multi-alias prefix WITHOUT windows must decline loudly, never flat-match")
	}
	if got, ok := ordinalSlotInLegWindow(noLegs, values.NamedCorrelationIdentifier("A"), "ID", false); !ok || got != 0 {
		t.Errorf("single-alias window-less flat path = (%d,%v), want (0,true) — the hardening must not break it", got, ok)
	}
}

// TestThreeWayBoxCrossAgreement pins the 3-way cross-agreement a multi-alias
// box requires between three independent computations of the same slot:
// ordinalSlotInLegWindow (over ordinalLegType's box .Legs — the planner's
// leg-window walk) and OrdinalSeedLegWindows (over the baked box mixed
// seed — the seed-window builder) must resolve EVERY box-leaf column to the
// SAME absolute slot. A separate executor fixture pins the seed-window
// builder against the executor's own span computation; this test closes the
// planner-walk <-> seed-window leg, so all three agree. A drift here is a
// silent wrong-alias slot on a dup-named column. The box outer is built
// directly (bypassing unnestExistsSeedSafe, which still declines it
// end-to-end) — a white-box layout pin, not a dispatch change.
func TestThreeWayBoxCrossAgreement(t *testing.T) {
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
		t.Fatal("OrdinalSeedLegWindows declined the box mixed seed")
	}
	// For every box leaf + column: the leg-window walk's slot == the
	// seed-window builder's slot.
	for _, leg := range boxType.Legs {
		w, present := windows[leg.Name]
		if !present {
			t.Fatalf("box leaf %s absent from the seed windows %v", leg.Name, windows)
		}
		for _, f := range w.Typ.Fields {
			legWalkSlot, ok1 := ordinalSlotInLegWindow(boxType, leg.Alias, f.Name, true)
			ci, okc := w.Typ.FieldIndex(f.Name)
			seedWinSlot := w.Offset + ci
			if !ok1 || !okc || legWalkSlot != seedWinSlot {
				t.Fatalf("3-way DRIFT: leaf %s col %s — leg-window walk slot %d (ok=%v) vs seed-window slot %d", leg.Name, f.Name, legWalkSlot, ok1, seedWinSlot)
			}
		}
	}
}

// TestOuterConjunctNarrowing pins the outer-conjunct regression narrowing at
// the TRANSLATOR level — the end-to-end rows are OVER-DETERMINED (both the
// ordinal and the name-model path yield the same rows), so only a white-box
// AnchoredJoin / detection assertion discriminates whether the ordinal path
// fired. Two independently-breakable halves:
//   - DETECTION (nonExistsConjunctRefsOuterLeg): a box-leg or flow-leg conjunct
//     TRIPS; an element-only conjunct does NOT (else a multi-alias box with an
//     element-only WHERE would SILENTLY fall to name-model, losing the ordinal
//     optimization with the answer unchanged — the masked over-decline).
//   - CONSUMPTION (the unnestBoxLegConjunct flag in
//     unnestExistsSeedSafe): flag SET → the FULL box seeds name-model
//     (AnchoredJoin); flag CLEAR → ordinal (the anti-over-decline positive pin).
func TestOuterConjunctNarrowing(t *testing.T) {
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

	// CONSUMPTION: the flag flips the FULL box from ordinal to a LOUD decline
	// (there is no name-model fallback left), in BOTH the EXISTS and
	// non-EXISTS paths (the flag is checked in unnestExistsSeedSafe BEFORE
	// the under-existential gate). FROM (Order o FULL OUTER Customer c),
	// o.TAGS AS X.
	translate := func(underExists, flag bool) expressions.RelationalExpression {
		tr := newGateTranslator(t)
		tr.unnestUnderExistential = underExists
		if flag {
			tr.unnestBoxLegConjunct = boxConjUnbakeable
		} else {
			tr.unnestBoxLegConjunct = boxConjNone
		}
		outer := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
		j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"}, logical.JoinInner, "")
		return tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
	}
	for _, underExists := range []bool{true, false} {
		// flag CLEAR (element-only conjunct, or none): the box ORDINALIZES — the
		// anti-over-decline pin the {7,8} e2e structurally cannot provide.
		expr := translate(underExists, false)
		if expr == nil {
			t.Errorf("underExists=%v flag clear: FULL box declined — the narrowing over-declined (element-only / no conjunct must still ordinalize)", underExists)
		} else if sel, ok := expr.(*expressions.SelectExpression); !ok {
			t.Errorf("underExists=%v flag clear: unnest expr = %T, want a SelectExpression", underExists, expr)
		} else if _, ok := sel.GetResultValue().(*values.RecordConstructorValue); !ok {
			t.Errorf("underExists=%v flag clear: unnest seed = %T, want the ordinal RC", underExists, sel.GetResultValue())
		}
		// flag SET (a box-leg conjunct): the box DECLINES LOUDLY. This must
		// hold for the NON-EXISTS path too (underExists=false) — the sibling
		// regression (a plain WHERE on a box leg over an ordinal box).
		if expr := translate(underExists, true); expr != nil {
			t.Errorf("underExists=%v flag set: FULL box translated (%T) — the narrowing did not fire (a box-leg conjunct must decline loudly)", underExists, expr)
		}
	}
}
