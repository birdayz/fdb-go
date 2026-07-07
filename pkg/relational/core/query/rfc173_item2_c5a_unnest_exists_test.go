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
