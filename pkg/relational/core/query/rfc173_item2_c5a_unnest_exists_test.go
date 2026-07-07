package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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

// TestRFC173Item2C5a_MultiAliasOuterDeclines pins the MULTI-ALIAS-outer fix.
// A merge-opaque FULL OUTER box has clusterArity 1 but binds
// TWO aliases, so clusterArity==1 is NOT a valid proxy for "single source" here.
// The under-EXISTS ordinal rebase addresses the outer as one pristine prefix and
// resolves a column by its name in the outer leg type; with two aliases a
// correlation to a buried alias's column collides with the other alias's
// same-named column and bakes the wrong slot. unnestExistsSeedSafe keys on
// outerBoundAliases (the outer row's visible namespace count), so such an outer
// stays NAME-MODEL under EXISTS (anchored seed). Without the guard the seed goes
// ordinal (unnestOrdinalSeed bakes the whole merged prefix, blind to the alias
// count) — this test is red.
//
// This decline is CORRECT and NOT a stale "W5-not-built" gate (design review of a
// proposed S4 lift, NAK'd): the W5 gathered path DOES disambiguate same-named
// columns via qualified slots, but the three EXISTS-rebase channels do NOT consult
// it — rebaseUnnestOuterLegPredicateOrdinal resolves by BARE-name first-match
// (qualifier DROPPED), and the RULE-level below-FOD executor hoist assumes the
// single-alias pristine-prefix-at-offset-0 invariant. So the only well-formed
// multi-alias correlation (a QUALIFIED ref like c.ID; a bare one is 42702) is
// exactly the one the ordinal rebase mis-resolves. The name-model anchored path
// (qualified LEG.COL keys, rebaseUnnestOuterLegPredicate) is the Java-faithful
// correct handler. Lifting this guard REQUIRES first teaching both the rebase
// channel AND the executor hoist to route a qualified ref through ordinalLegType's
// rt.Legs [Start,Start+Width) windows (metadata already produced, consumed today
// only by OrdinalSeedLegWindows) — its own slice/ACK — THEN lifting the guard
// behind a yamsql e2e (qualified ref to a second-alias dup-named column + a FULL
// OUTER null-supplied row). Until that lands, the guard stays.
func TestRFC173Item2C5a_MultiAliasOuterDeclines(t *testing.T) {
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
	if !rc.AnchoredJoin {
		t.Fatal("multi-alias FULL OUTER outer under EXISTS seeded the ORDINAL RC — the ordinal rebase cannot disambiguate the two aliases' same-named columns; it must stay name-model")
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
