package query

// White-box pins for the GATHERED multi-source lateral-unnest
// translation (the flat (N+1)-quantifier select).
// The FDB e2e (array_unnest_ordinality_fdb_test.go multi-source families)
// proves rows; these prove the SEED and QUANTIFIER shapes plus the
// fail-open decline boundary directly.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/query/logical"
)

// newDisjointUnnestTranslator builds a translator over a CUSTOM two-table
// schema with fully DISJOINT column names (every pair of demo-proto tables
// shares PRICE, which the name-ambiguity gate rightly declines — the
// gather-admitting shape needs disjoint legs): SRC(SID, ARR) owns the array;
// AUX(XID, V) is the second source.
func newDisjointUnnestTranslator(t *testing.T) *cascadesTranslator {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("w5wb")
	b.AddTable("SRC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("SID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"SID"})
	b.AddTable("AUX", []metadata.ColumnSpec{
		metadata.NewColumnSpec("XID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewLongType(true), 2),
	}, []string{"XID"})
	b.AddTable("AUX2", []metadata.ColumnSpec{
		metadata.NewColumnSpec("YID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("W", api.NewLongType(true), 2),
	}, []string{"YID"})
	// SRC2 deliberately SHARES the column name SID with SRC — the box-involved
	// cross-leg dup fixture (a FULL box of SRC2 collides with a sibling SRC scan).
	b.AddTable("SRC2", []metadata.ColumnSpec{
		metadata.NewColumnSpec("SID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("W", api.NewLongType(true), 2),
	}, []string{"SID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return &cascadesTranslator{
		md:              tmpl.Underlying(),
		cteScope:        make(map[string]logical.LogicalOperator),
		cteExprScope:    make(map[string]expressions.RelationalExpression),
		cteColumnsScope: make(map[string][]values.Field),
	}
}

// gatheredFixture builds the canonical multi-source shape
// `FROM SRC s, AUX x, s.ARR AS EL [AT ord]` — the unnest join whose left is
// the 2-source comma cluster and whose owning source is the FIRST
// (non-rightmost) leg.
func gatheredFixture(asAlias, atAlias string) (*logical.LogicalJoin, *logical.LogicalUnnest) {
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: asAlias, AtAlias: atAlias}
	left := inner(scan("SRC", "s"), scan("AUX", "x"))
	return logical.NewJoin(left, u, logical.JoinInner, ""), u
}

// TestGatheredSeedShape pins the flat gathered translation: one
// select with N+1 quantifiers in FROM order; the Explode's collection is a
// BAKED reference to the OWNING source's own quantifier (the genuine
// per-source correlation — never the
// whole-cluster concat); the seed = full ordinal leg runs + the shared
// unnest-seed inner
// fields, asserted pristine for the full-baked AS+AT form.
func TestGatheredSeedShape(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	j, u := gatheredFixture("EL", "ORD")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	sel := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("a gated 2-source cluster + owned unnest must gather, got nil (declined)")
	}
	gathered, isSel := sel.(*expressions.SelectExpression)
	if !isSel {
		t.Fatalf("gathered = %T, want *SelectExpression", sel)
	}
	quants := gathered.GetQuantifiers()
	if len(quants) != 3 {
		t.Fatalf("gathered select has %d quantifiers, want 3 (S, X-aux, X in FROM order)", len(quants))
	}
	if quants[0].GetAlias().Name() != "S" || quants[1].GetAlias().Name() != "X" || quants[2].GetAlias() != innerCorr {
		t.Fatalf("quantifier order/aliases = [%s %s %s], want FROM order [S X EL] (leg aliases flow UPPER via sourceAlias)",
			quants[0].GetAlias(), quants[1].GetAlias(), quants[2].GetAlias())
	}

	// The Explode: its collection is baked ofOrdinal over the OWNER's own
	// typed QOV — correlation "O", frontier-pinned, at the owner-type index of
	// the classified field.
	explode := quants[2].GetRangesOver().Members()[0]
	exp, isExp := explode.(*expressions.ExplodeExpression)
	if !isExp {
		t.Fatalf("inner quantifier ranges over %T, want *ExplodeExpression", explode)
	}
	coll, isFV := exp.GetCollectionValue().(*values.FieldValue)
	if !isFV || coll.Resolved == nil || !coll.Resolved.FrontierPinned {
		t.Fatalf("collection = %T (baked=%v), want a frontier-pinned baked FieldValue", exp.GetCollectionValue(), isFV && coll != nil && coll.Resolved != nil)
	}
	qov, isQOV := coll.Child.(*values.QuantifiedObjectValue)
	if !isQOV || !strings.EqualFold(qov.Correlation.Name(), "s") {
		t.Fatalf("collection correlates to %v, want the OWNING source o (the per-source edge, never the cluster concat)", coll.Child)
	}
	ownerType := tr.ordinalLegType(scan("SRC", "s"))
	wantIdx, _ := ownerType.FieldIndex("ARR")
	if acc, single := coll.Resolved.Single(); !single || acc.Ordinal != wantIdx {
		t.Fatalf("collection baked at ordinal %v, want %d (FieldIndex of the classified field on the OWNER type)", coll.Resolved, wantIdx)
	}

	// The seed: full O-run + full C-run + element + ordinal — all baked (AS+AT
	// is the full-baked form) — and pristine per AssertOrdinalJoinSeed.
	rc, isRC := gathered.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		t.Fatalf("seed = %T, want the ordinal RC", gathered.GetResultValue())
	}
	values.AssertOrdinalJoinSeed(rc)
	cType := tr.ordinalLegType(scan("AUX", "x"))
	wantFields := len(ownerType.Fields) + len(cType.Fields) + 2
	if len(rc.Fields) != wantFields {
		t.Fatalf("seed has %d fields, want %d (O-run + TR-run + element + ordinal)", len(rc.Fields), wantFields)
	}
	if rc.Fields[wantFields-2].Name != "EL" || rc.Fields[wantFields-1].Name != "ORD" {
		t.Fatalf("inner field names = [%s %s], want [EL ORD] (the AS/AT aliases)",
			rc.Fields[wantFields-2].Name, rc.Fields[wantFields-1].Name)
	}
}

// TestGatheredDeclineBoundary pins the fail-open scope:
// every declined shape returns nil so the caller keeps the name-model path
// that handles it today.
func TestGatheredDeclineBoundary(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	innerCorr := values.NamedCorrelationIdentifier("EL")

	// (a) ON-carrying cluster GATHERS: the ON conjunct rides the flat select
	// baked through the cluster spine.
	uOn := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	onLeft := logical.NewJoinWithPredicate(scan("SRC", "s"), scan("AUX", "x"), logical.JoinInner,
		corrEq("x", "XID", "s", "SID"))
	jOn := logical.NewJoin(onLeft, uOn, logical.JoinInner, "")
	gotOn := tr.translateGatheredUnnestCluster(jOn, uOn, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if gotOn == nil {
		t.Fatal("an ON-carrying cluster must GATHER")
	}
	if len(gotOn.(*expressions.SelectExpression).GetPredicates()) == 0 {
		t.Fatal("the gathered ON-carrying select must carry the ON conjunct")
	}

	// (b) NAME-AMBIGUOUS bare-twin: a column name shared by two
	// NON-BOX legs (same table twice) GATHERS via the RAW seed — NOT a positional
	// wrap. Each plain leg is its own quantifier, so a qualified `s.SID`/`s2.SID` read
	// routes to its own scan (in the SELECT, an outer WHERE, and a cross-leg predicate
	// alike — an FDB integration test pins those rows). A wrap here would be wrong: its bare
	// pass-through key would make an outer filter resolve a qualified dup column to
	// first-match. (A BARE ambiguous reference errors 42702 at semantic analysis
	// before the translator, so only qualified reads reach here.) The raw seed is a
	// SelectExpression, no nested projection.
	uDup := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jDup := logical.NewJoin(inner(scan("SRC", "s"), scan("SRC", "s2")), uDup, logical.JoinInner, "")
	gotDup := tr.translateGatheredUnnestCluster(jDup, uDup, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if gotDup == nil {
		t.Fatal("cross-leg duplicate column names between two non-box legs must GATHER via the raw seed, not decline")
	}
	if _, ok := gotDup.(*expressions.SelectExpression); !ok {
		t.Fatalf("non-box dup-name gather must be the RAW seed SelectExpression (no wrap), got %T", gotDup)
	}

	// (b2) BOX-INVOLVED CROSS-LEG dup: a merge-opaque FULL box
	// leg BURYING SID collides with a sibling SRC scan's SID — the dup crosses legs with one
	// leg a box. The two SIDs live in DIFFERENT legs, so each keeps its own [Offset,Width)
	// window (the box's buried SID sub-window via finalizeSeedWindows, the scan's SID run),
	// and a qualified read routes by SLOT — it GATHERS the raw seed. The
	// discriminating FULL-NULL / cross-leg-predicate / ORDER BY rows are pinned e2e in
	// the sqldriver FDB integration suite.
	uBox := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	boxLeg := logical.NewJoin(scan("SRC2", "s2"), scan("AUX", "x"), logical.JoinFull, "")
	jBox := logical.NewJoin(inner(boxLeg, scan("SRC", "s")), uBox, logical.JoinInner, "")
	gotBox := tr.translateGatheredUnnestCluster(jBox, uBox, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if _, ok := gotBox.(*expressions.SelectExpression); !ok {
		t.Fatalf("a box-involved CROSS-LEG dup must GATHER the raw seed, got %T", gotBox)
	}

	// (b2-within) WITHIN-BOX dup: a
	// single merge-opaque FULL box whose concat carries the SAME column name TWICE (SRC2.W +
	// AUX2.W are both `W`) GATHERS. buriedLegBounds windows the two same-named buried
	// leaves at distinct slots, so a qualified SRC2.W / AUX2.W resolves to its own window;
	// the box's DOUBLY-null-fill (both W NULL on opposite unmatched rows) resolves through
	// the FULL-NULL substrate (e2e discriminating rows: the sqldriver FDB integration suite).
	uWithin := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	withinBox := logical.NewJoin(scan("SRC2", "s2"), scan("AUX2", "y"), logical.JoinFull, "")
	jWithin := logical.NewJoin(inner(withinBox, scan("SRC", "s")), uWithin, logical.JoinInner, "")
	gotWithin := tr.translateGatheredUnnestCluster(jWithin, uWithin, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if _, ok := gotWithin.(*expressions.SelectExpression); !ok {
		t.Fatalf("a WITHIN-box dup (two same-named columns in ONE box) must GATHER the raw seed, got %T", gotWithin)
	}

	// (b3) GROUPED non-box cross-leg dup GATHERS via the un-collapse: the SAME
	// two-scan bare-twin as case (b), UNDER AGGREGATE, returns
	// the RAW per-leg seed (a *SelectExpression, NOT the retired name-keyed wrap). The
	// ancestor GROUP-BY positionally bakes its keys over it (translateAggregate:
	// OrdinalSeedLegWindows for leg columns — a TRAILING-element bare-twin windows
	// cleanly). The collapse and its ALIAS.COL name keys are gone; the qualified dup
	// keys route by slot, not first-match. Same cluster as (b) — the ONLY difference is
	// underAggregate, and both now gather the raw seed.
	tr.underAggregate = true
	uGrouped := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jGrouped := logical.NewJoin(inner(scan("SRC", "s"), scan("SRC", "s2")), uGrouped, logical.JoinInner, "")
	gotGrouped := tr.translateGatheredUnnestCluster(jGrouped, uGrouped, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	tr.underAggregate = false
	if _, ok := gotGrouped.(*expressions.SelectExpression); !ok {
		t.Fatalf("a GROUPED non-box cross-leg dup must GATHER via the un-collapse (raw SelectExpression), got %T", gotGrouped)
	}

	// (c) SHADOWING GATHERS: the element alias
	// equaling an outer column name resolves correctly — the visitor
	// qualifies the shadowed bare projection and the span windows route the
	// qualified read to the ELEMENT leg (last-binding-wins preserved).
	uShadow := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "V"}
	jShadow := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uShadow, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jShadow, uShadow, values.NamedCorrelationIdentifier("V"), values.NotNullLong, "ARR", unnestTrailing); got == nil {
		t.Fatal("an element alias shadowing an outer column must GATHER")
	}

	// (d) UNGATED left cluster: flows name-model rows the baked collection
	// cannot consume. A gated outer box gathers as
	// ONE opaque leg (see TestOuterBoxLeftGathers); the still-
	// ungated class is a DUPLICATE-BINDING cluster, poisoned by the wedge
	// gate before any leg translates.
	uLeft := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jLeft := logical.NewJoin(
		inner(scan("SRC", "s"), scan("SRC", "s")),
		uLeft, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jLeft, uLeft, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("an UNGATED (duplicate-binding) cluster must DECLINE")
	}

	// (e) SINGLE-SOURCE left: the binary unnest-seed path owns N=1.
	uSingle := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jSingle := logical.NewJoin(scan("SRC", "s"), uSingle, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jSingle, uSingle, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("a single-source outer must DECLINE the gather (the binary seed owns it)")
	}

	// (f) The owner must be a gathered PLAIN leg: segment 0 naming NO leg
	// declines (an out-of-scope or derived owner).
	uNoOwner := &logical.LogicalUnnest{Segments: []string{"z", "ORDER_ID"}, Alias: "EL"}
	jNoOwner := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uNoOwner, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jNoOwner, uNoOwner, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("segment 0 naming no gathered leg must DECLINE")
	}

	// (g) A multi-segment path whose ROOT segment is not a column of the
	// owner window declines (VALID struct paths route
	// through the fused suffix; SRC has no column A, so the root lookup
	// misses).
	uDeep := &logical.LogicalUnnest{Segments: []string{"s", "A", "B"}, Alias: "EL"}
	jDeep := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uDeep, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jDeep, uDeep, innerCorr, values.NotNullLong, "A", unnestTrailing); got != nil {
		t.Fatal("a multi-segment path with a MISSING root column must DECLINE")
	}
}

// TestGatheredMixedElementSkipsAssert pins the NO-AT gathered form:
// a MIXED RC (baked outer runs + a direct bare-QOV element) — legitimately
// not AssertOrdinalJoinSeed-conformant, same rule as the binary unnest seed.
func TestGatheredMixedElementSkipsAssert(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	j, u := gatheredFixture("EL", "")
	sel := tr.translateGatheredUnnestCluster(j, u, values.NamedCorrelationIdentifier("EL"), values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("the no-AT gathered form must translate")
	}
	rc := sel.(*expressions.SelectExpression).GetResultValue().(*values.RecordConstructorValue)
	last := rc.Fields[len(rc.Fields)-1]
	if _, isQOV := last.Value.(*values.QuantifiedObjectValue); !isQOV {
		t.Fatalf("no-AT element field = %T, want the DIRECT bare QOV (Java's primitive branch; the mixed RC skips the seed assert)", last.Value)
	}
	if last.Name != "EL" {
		t.Fatalf("element field name = %q, want the AS alias EL", last.Name)
	}
}

// TestBakeNormalizesCorrelationCase pins the gather-boundary case
// authority: bakeGatedJoinPredicates matches leg aliases case-insensitively
// but must EMIT the baked node under the leg-alias case the gather minted
// (UPPER via sourceAlias) — a baked correlation in the reference's original
// case diverges from the quantifier it binds to, and downstream
// classification (exact-compare) silently misrouted a lowercase ON conjunct
// as deeply-correlated. Unreachable via production
// SQL (uppercased upstream) — pinned white-box.
func TestBakeNormalizesCorrelationCase(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	pu := tr.ordinalLegType(scan("SRC", "s"))
	aux := tr.ordinalLegType(scan("AUX", "x"))
	legTypes := map[string]bakeLegType{
		"S": {typ: pu, leafOffset: 0, leafTyp: pu},
		"X": {typ: aux, leafOffset: 0, leafTyp: aux},
	}
	// A cross-leg conjunct whose references carry LOWERCASE correlations.
	pred := corrEq("s", "SID", "x", "XID")
	baked := bakeGatedJoinPredicates([]predicates.QueryPredicate{pred}, legTypes)
	found := 0
	predicates.ReplaceValues(baked[0], func(v values.Value) values.Value {
		if fv, isFV := v.(*values.FieldValue); isFV && fv.Resolved != nil {
			qov := fv.Child.(*values.QuantifiedObjectValue)
			if n := qov.Correlation.Name(); n != strings.ToUpper(n) {
				t.Errorf("baked correlation %q keeps the reference's original case — must take the gather's UPPER case authority", n)
			}
			found++
		}
		return v
	})
	if found != 2 {
		t.Fatalf("expected both cross-leg references baked, got %d", found)
	}
}

// chainEqPredLocal builds a cross-leg equality conjunct over two named
// correlations (the white-box twin of the SQL WHERE/ON spelling).
func chainEqPredLocal(a, aCol, b, bCol string) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a)), aCol, values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b)), bCol, values.UnknownType),
		},
	)
}

// enclosedFixture builds the ENCLOSED shape `FROM SRC s, s.ARR AS EL, AUX x`
// — the unnest join buried as the LEFT leg of the enclosing cluster, the
// rotation's canonical input.
func enclosedFixture(asAlias, atAlias string) *logical.LogicalJoin {
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: asAlias, AtAlias: atAlias}
	unnestJoin := logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, "")
	return logical.NewJoin(unnestJoin, scan("AUX", "x"), logical.JoinInner, "")
}

// TestEnclosedRotation pins the rotation: the buried-unnest
// cluster rebuilds as Join(Join(plain legs, FROM order), Unnest) — the exact
// root form the gathered-cluster builder owns — and the full dispatch produces the
// same flat gathered select the trailing form does.
func TestEnclosedRotation(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	root := enclosedFixture("EL", "")
	rebuilt, u, elementType, fieldName, pos, ok := tr.rotateEnclosedUnnest(root)
	if !ok {
		t.Fatal("the enclosed 2-plain-leg cluster must classify and rotate, got ok=false")
	}
	if u == nil || fieldName != "ARR" || elementType == nil {
		t.Fatalf("classification: u=%v field=%q type=%v", u != nil, fieldName, elementType)
	}
	if _, isU := rebuilt.Right.(*logical.LogicalUnnest); !isU {
		t.Fatalf("rotation must place the unnest at root-right, got %T", rebuilt.Right)
	}
	lj, isJ := rebuilt.Left.(*logical.LogicalJoin)
	if !isJ {
		t.Fatalf("rotated left = %T, want the plain-leg join", rebuilt.Left)
	}
	ls, lok := lj.Left.(*logical.LogicalScan)
	rs, rok := lj.Right.(*logical.LogicalScan)
	if !lok || !rok || ls.Table != "SRC" || rs.Table != "AUX" {
		t.Fatalf("rotated plain legs = (%T, %T), want (Scan(SRC), Scan(AUX)) in FROM order", lj.Left, lj.Right)
	}
	if rebuilt.OnPredicate != nil || lj.OnPredicate != nil {
		t.Fatal("a pure comma cluster must rotate ON-free at both levels")
	}
	if pos != 1 {
		t.Fatalf("unnestPos = %d, want 1 (`FROM SRC, SRC.arr, AUX` — one plain leg precedes)", pos)
	}

	sel := tr.translateEnclosedUnnestGather(root)
	if sel == nil {
		t.Fatal("the enclosed dispatch must gather, got nil")
	}
	gathered, isSel := sel.(*expressions.SelectExpression)
	if !isSel {
		t.Fatalf("gathered = %T, want *SelectExpression", sel)
	}
	quants := gathered.GetQuantifiers()
	if len(quants) != 3 {
		t.Fatalf("gathered select has %d quantifiers, want 3", len(quants))
	}
	// FROM order preserved: the Explode sits at the unnest's position (after
	// SRC, before AUX) so a SELECT * expansion matches the user's FROM list.
	if quants[1].GetAlias().Name() != "EL" || quants[0].GetAlias().Name() != "S" || quants[1+1].GetAlias().Name() != "X" {
		t.Fatalf("quantifier order = [%s %s %s], want FROM order [S EL X]",
			quants[0].GetAlias(), quants[1].GetAlias(), quants[2].GetAlias())
	}
	// The seed fields mirror the same order: SRC's run, the element, AUX's run.
	rc := gathered.GetResultValue().(*values.RecordConstructorValue)
	names := make([]string, 0, len(rc.Fields))
	for _, f := range rc.Fields {
		names = append(names, f.Name)
	}
	want := []string{"SID", "ARR", "EL", "XID", "V"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("seed field order = %v, want FROM order %v", names, want)
	}
}

// TestEnclosedRotationONCollection pins the ON handling: every ON
// conjunct collected along the buried spine (including one referencing the
// ELEMENT alias) rides the rebuilt ROOT's OnPredicate — never the plain-leg
// chain, where the element is out of scope.
func TestEnclosedRotationONCollection(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	unnestJoin := logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, "")
	onPred := chainEqPredLocal("x", "XID", "s", "SID")
	root := logical.NewJoinWithPredicate(unnestJoin, scan("AUX", "x"), logical.JoinInner, onPred)

	rebuilt, _, _, _, _, ok := tr.rotateEnclosedUnnest(root)
	if !ok {
		t.Fatal("the ON-carrying enclosed cluster must rotate")
	}
	lj := rebuilt.Left.(*logical.LogicalJoin)
	if lj.OnPredicate != nil {
		t.Fatal("the rotated plain-leg chain must stay ON-free (the element may be referenced)")
	}
	got, isQP := rebuilt.OnPredicate.(predicates.QueryPredicate)
	if !isQP || got == nil {
		t.Fatalf("the collected ON conjunct must ride the rebuilt ROOT, got %T", rebuilt.OnPredicate)
	}
}

// TestEnclosedRotationDeclineBoundary pins the fail-open declines:
// (a) two buried unnests; (b) a single plain leg (nothing to enclose);
// (c) the element alias colliding with a leg AFTER the unnest in FROM order —
// the widened all-legs collision scope the rotation demands (the original
// gauntlet only sees the legs BEFORE the unnest); (d) a LEFT-kind root.
func TestEnclosedRotationDeclineBoundary(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	u2 := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "E2"}
	twoUnnests := logical.NewJoin(
		logical.NewJoin(enclosedFixture("EL", ""), u2, logical.JoinInner, ""),
		scan("AUX", "y"), logical.JoinInner, "")
	if _, _, _, _, _, ok := tr.rotateEnclosedUnnest(twoUnnests); ok {
		t.Error("two buried unnests must decline (chained/multi is out of scope)")
	}

	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	singleLeg := logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, "")
	if _, _, _, _, _, ok := tr.rotateEnclosedUnnest(singleLeg); ok {
		t.Error("the root form (unnest at root-right) must decline — translateUnnestJoin owns it")
	}

	uCollide := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "X"}
	collideRoot := logical.NewJoin(
		logical.NewJoin(scan("SRC", "s"), uCollide, logical.JoinInner, ""),
		scan("AUX", "x"), logical.JoinInner, "")
	if _, _, _, _, _, ok := tr.rotateEnclosedUnnest(collideRoot); ok {
		t.Error("an element alias colliding with a TRAILING leg alias must decline (all-legs scope)")
	}

	leftRoot := logical.NewJoin(
		logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, ""),
		scan("AUX", "x"), logical.JoinLeft, "")
	if _, _, _, _, _, ok := tr.rotateEnclosedUnnest(leftRoot); ok {
		t.Error("a LEFT-kind enclosing root must decline (inner-only rotation)")
	}
}

// TestEnclosedRotationONElementRewrite pins the review finding:
// a collected ON conjunct referencing the ELEMENT alias rides the rebuilt
// root's OnPredicate, and the builder must REWRITE it (rewriteUnnestPredicate
// — the WHERE merge arm's exact treatment) before baking: the Explode flows a
// bare scalar (no-AT), so an unrewritten `FieldValue(QOV(EL), "EL")`
// evaluates NIL and the join silently drops or misfilters every row.
func TestEnclosedRotationONElementRewrite(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	unnestJoin := logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, "")
	onPred := chainEqPredLocal("x", "V", "EL", "EL") // B.V = EL — references the element
	root := logical.NewJoinWithPredicate(unnestJoin, scan("AUX", "x"), logical.JoinInner, onPred)

	sel := tr.translateEnclosedUnnestGather(root)
	if sel == nil {
		t.Fatal("the ON-carrying enclosed cluster must gather")
	}
	gathered := sel.(*expressions.SelectExpression)
	var sawBareElementQOV, sawUnrewritten bool
	inspect := func(v values.Value) {
		if v == nil {
			return
		}
		values.WalkValue(v, func(node values.Value) bool {
			if fv, isFV := node.(*values.FieldValue); isFV {
				if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV &&
					strings.EqualFold(qov.Correlation.Name(), "EL") && strings.EqualFold(fv.Field, "EL") {
					sawUnrewritten = true
				}
			}
			if qov, isQOV := node.(*values.QuantifiedObjectValue); isQOV && strings.EqualFold(qov.Correlation.Name(), "EL") {
				sawBareElementQOV = true
			}
			return true
		})
	}
	for _, p := range gathered.GetPredicates() {
		if cp, isCP := p.(*predicates.ComparisonPredicate); isCP {
			inspect(cp.Operand)
			inspect(cp.Comparison.Operand)
		}
	}
	if sawUnrewritten {
		t.Fatal("the collected ON's element ref survived UNREWRITTEN (FieldValue(QOV(EL), EL)) — it evaluates NIL over the bare-scalar Explode (silent drop/misfilter)")
	}
	if !sawBareElementQOV {
		t.Fatal("the rewritten ON must reference the element as the bare QOV(EL) (the no-AT scalar collapse)")
	}
}

// TestEnclosedRotationThreeLegsMidUnnest pins the wider-cluster
// bookkeeping the 2-leg pins cannot (the review coverage ask): with THREE
// plain legs and the unnest strictly in the middle of the FROM list
// (`FROM SRC s, s.ARR AS EL, AUX x, AUX2 y`), the rotation reports the right
// unnestPos and the built select preserves FROM order in BOTH the quantifier
// list and the seed-field offsets (the per-leg run-width summation).
func TestEnclosedRotationThreeLegsMidUnnest(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	unnestJoin := logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, "")
	mid := logical.NewJoin(unnestJoin, scan("AUX", "x"), logical.JoinInner, "")
	root := logical.NewJoin(mid, scan("AUX2", "y"), logical.JoinInner, "")

	_, _, _, _, pos, ok := tr.rotateEnclosedUnnest(root)
	if !ok || pos != 1 {
		t.Fatalf("rotate: ok=%v pos=%d, want ok with unnestPos 1 (one plain leg precedes)", ok, pos)
	}

	sel := tr.translateEnclosedUnnestGather(root)
	if sel == nil {
		t.Fatal("the 3-plain-leg mid-unnest cluster must gather")
	}
	gathered := sel.(*expressions.SelectExpression)
	quants := gathered.GetQuantifiers()
	order := make([]string, 0, len(quants))
	for _, q := range quants {
		order = append(order, q.GetAlias().Name())
	}
	if strings.Join(order, ",") != "S,EL,X,Y" {
		t.Fatalf("quantifier order = %v, want FROM order [S EL X Y]", order)
	}
	rc := gathered.GetResultValue().(*values.RecordConstructorValue)
	names := make([]string, 0, len(rc.Fields))
	for _, f := range rc.Fields {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "SID,ARR,EL,XID,V,YID,W" {
		t.Fatalf("seed field order = %v, want [SID ARR EL XID V YID W] (element at its FROM offset, per-leg runs intact)", names)
	}
}

// TestBoxLegOwnerGathers pins unnest-residual class 1: an owner
// BURIED inside a box leg of the gathered cluster (`FROM (s LEFT x), y,
// s.ARR AS EL`) gathers — the collection bakes at the box quantifier's
// WINDOW (the box quantifier's correlation, the buried leaf's offset in the
// concat), never a leg-local ordinal over a quantifier that does not exist
// at this select.
func TestBoxLegOwnerGathers(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	box := logical.NewJoin(scan("SRC", "s"), scan("AUX", "x"), logical.JoinLeft, "")
	left := inner(box, scan("AUX2", "y"))
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL", AtAlias: "ORD"}
	j := logical.NewJoin(left, u, logical.JoinInner, "")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	sel := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("a buried box-leg owner must GATHER (class 1), got nil (declined)")
	}
	gathered := sel.(*expressions.SelectExpression)
	quants := gathered.GetQuantifiers()
	if len(quants) != 3 {
		t.Fatalf("quantifiers = %d, want 3 (box leg, y, EL)", len(quants))
	}
	explode := quants[2].GetRangesOver().Members()[0]
	exp, isExp := explode.(*expressions.ExplodeExpression)
	if !isExp {
		t.Fatalf("inner quantifier ranges over %T, want *ExplodeExpression", explode)
	}
	coll, isFV := exp.GetCollectionValue().(*values.FieldValue)
	if !isFV || coll.Resolved == nil || !coll.Resolved.FrontierPinned {
		t.Fatalf("collection = %T, want a frontier-pinned baked FieldValue", exp.GetCollectionValue())
	}
	qov := coll.Child.(*values.QuantifiedObjectValue)
	// The box quantifier's binding carries the leg's minted $BOX name; the
	// concat is 4 wide (SID, ARR, XID, V) with s's window at offset 0 —
	// ARR = concat ordinal 1.
	if qov.Correlation != quants[0].GetAlias() {
		t.Fatalf("collection correlates to %s, want the BOX leg quantifier %s (the box-leg window)", qov.Correlation, quants[0].GetAlias())
	}
	rt := qov.Typ.(*values.RecordType)
	if len(rt.Fields) != 4 {
		t.Fatalf("collection QOV type = %d fields, want the 4-wide box concat", len(rt.Fields))
	}
	if acc, single := coll.Resolved.Single(); !single || acc.Ordinal != 1 {
		t.Fatalf("collection ordinal = %v, want box-level 1 (offset 0 + ARR idx 1)", coll.Resolved.Accessors)
	}

	// Seed RC run layout: the leg runs must be the flat
	// concat the box + y contribute, in FROM order — box concat [SID ARR XID
	// V] baked over the box quantifier, then y's [YID W] baked over y, then
	// the element/ordinal run over EL. Assert the RUN structure, not just the
	// collection: a wrong run layout mis-windows every downstream read.
	rc := gathered.GetResultValue().(*values.RecordConstructorValue)
	wantNames := []string{"SID", "ARR", "XID", "V", "YID", "W", "EL", "ORD"}
	if len(rc.Fields) != len(wantNames) {
		t.Fatalf("seed RC has %d fields, want %d (box concat + y + element/ordinal)", len(rc.Fields), len(wantNames))
	}
	boxAlias := quants[0].GetAlias()
	yAlias := quants[1].GetAlias()
	for i, want := range wantNames {
		f := rc.Fields[i]
		if !strings.EqualFold(f.Name, want) {
			t.Fatalf("seed field %d = %q, want %q (FROM-order concat)", i, f.Name, want)
		}
		fv, ok := f.Value.(*values.FieldValue)
		if !ok || fv.Resolved == nil {
			t.Fatalf("seed field %d (%s) is %T, want a baked FieldValue", i, f.Name, f.Value)
		}
		child := fv.Child.(*values.QuantifiedObjectValue).Correlation
		switch {
		case i < 4 && child != boxAlias: // the 4-wide box concat run
			t.Fatalf("seed field %d (%s) correlates to %s, want the box leg %s", i, f.Name, child, boxAlias)
		case i >= 4 && i < 6 && child != yAlias: // y's run
			t.Fatalf("seed field %d (%s) correlates to %s, want leg y %s", i, f.Name, child, yAlias)
		case i >= 6 && child != innerCorr: // the element/ordinal run
			t.Fatalf("seed field %d (%s) correlates to %s, want the element %s", i, f.Name, child, innerCorr)
		}
	}
}

// TestOuterBoxLeftGathers pins the rule: a gated OUTER box as the unnest's LEFT gathers as ONE OPAQUE leg —
// never its legs into the flat select — plus the Explode. The padded row's
// NULL array explodes to zero rows downstream (Java's Explode-over-NULL).
// The box carries a REAL ON predicate: the box's gating ON must stay INSIDE
// the box leg and never hoist into the flat select's predicates (a hoisted
// copy re-applies the ON as a WHERE and converts LEFT to INNER — the padded
// row's NULL-supplied columns fail it and the row silently drops; the FDB
// row-matrix pin caught exactly that with an ON-free box here).
func TestOuterBoxLeftGathers(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	box := logical.NewJoinWithPredicate(scan("SRC", "s"), scan("AUX", "x"), logical.JoinLeft,
		chainEqPredLocal("x", "XID", "s", "SID"))
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL", AtAlias: "ORD"}
	j := logical.NewJoin(box, u, logical.JoinInner, "")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	sel := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("an outer box as unnest-left must GATHER as one leg (class 1), got nil")
	}
	gathered := sel.(*expressions.SelectExpression)
	quants := gathered.GetQuantifiers()
	if len(quants) != 2 {
		t.Fatalf("quantifiers = %d, want 2 (the OPAQUE box + EL — the box's legs are never gathered)", len(quants))
	}
	if got := len(gathered.GetPredicates()); got != 0 {
		t.Fatalf("flat select carries %d predicates, want 0 — the OUTER box's gating ON must stay inside the box, never hoist (LEFT→INNER corruption)", got)
	}
	coll := quants[1].GetRangesOver().Members()[0].(*expressions.ExplodeExpression).GetCollectionValue().(*values.FieldValue)
	qov := coll.Child.(*values.QuantifiedObjectValue)
	if qov.Correlation != quants[0].GetAlias() {
		t.Fatalf("collection correlates to %s, want the box quantifier %s", qov.Correlation, quants[0].GetAlias())
	}
	if acc, single := coll.Resolved.Single(); !single || acc.Ordinal != 1 {
		t.Fatalf("collection ordinal = %v, want box-level 1", coll.Resolved.Accessors)
	}
}

// TestDupAliasOwnerFirstMatch pins the owner-lookup discipline
// under DUPLICATE FROM aliases: the FIRST duplicate keeps the alias as its binding, later
// duplicates carry parser-minted ids the name lookup cannot reach, so
// `s.ARR` resolves to the FIRST leg's window and the collection correlates
// to THAT leg's binding — never the later duplicate's.
func TestDupAliasOwnerFirstMatch(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	first := scan("SRC", "s")
	second := scan("AUX", "s") // same SQL alias, distinct table
	second.Binding = "Q$DUP1"  // the parser's mint for a later duplicate
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL", AtAlias: "ORD"}
	j := logical.NewJoin(inner(first, second), u, logical.JoinInner, "")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	sel := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("a dup-alias cluster with distinct bindings must GATHER, got nil")
	}
	gathered := sel.(*expressions.SelectExpression)
	quants := gathered.GetQuantifiers()
	if len(quants) != 3 {
		t.Fatalf("quantifiers = %d, want 3", len(quants))
	}
	coll := quants[2].GetRangesOver().Members()[0].(*expressions.ExplodeExpression).GetCollectionValue().(*values.FieldValue)
	qov := coll.Child.(*values.QuantifiedObjectValue)
	if qov.Correlation.Name() != "S" {
		t.Fatalf("collection correlates to %s, want the FIRST duplicate's binding S (first-match by alias, correlate by binding)", qov.Correlation)
	}
}

// TestOpaqueBoxNestedClusterPredsStayInside pins the twin of the
// ON-hoist guard: a class-1 opaque OUTER box whose null side is itself a
// nested inner cluster (`SRC LEFT JOIN (AUX JOIN AUX2 ON x.XID = y.YID), s.ARR
// AS EL`) must NOT leak that nested cluster's ON into the flat gathered
// select. The ON is already enforced inside the box leg's own translation;
// re-applying it flat over the box's NULL-padded rows fails the null-supplied
// AUX/AUX2 slots and silently drops the preserved SRC row (LEFT→INNER). The
// gather's flat select carries ZERO predicates for this shape.
func TestOpaqueBoxNestedClusterPredsStayInside(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	innerCluster := logical.NewJoin(scan("AUX", "x"), scan("AUX2", "y"), logical.JoinInner, "")
	innerCluster.OnPredicate = chainEqPredLocal("x", "XID", "y", "YID")
	box := logical.NewJoin(scan("SRC", "s"), innerCluster, logical.JoinLeft, "")
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	j := logical.NewJoin(box, u, logical.JoinInner, "")
	sel := tr.translateGatheredUnnestCluster(j, u, values.NamedCorrelationIdentifier("EL"), values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("a nested-cluster opaque box must GATHER, got nil")
	}
	if n := len(sel.(*expressions.SelectExpression).GetPredicates()); n != 0 {
		t.Fatalf("opaque outer box leaked %d nested-cluster predicates into the flat select, want 0 (the box's ONs stay inside)", n)
	}
}

// TestGatheredOuterConjunctCoupling pins the 3-state verdict routing
// at translateGatheredUnnestCluster's OUTER-box arm: UNBAKEABLE declines (nil,
// name-model), while None AND BAKEABLE gather an ordinal seed — admitting
// bakeable conjuncts, so a regression of gather's `== boxConjUnbakeable`
// comparison back to the blanket `!= boxConjNone` decline breaks the Bakeable
// arm here. A DISTINCT-leg-column box can't bypass the narrowing via the gather
// (SRC/AUX share no column name → the name-ambiguity decline never fires and
// can't mask the verdict). The corresponding e2e rows are OVER-DETERMINED (the
// name-model fallback yields the same rows), so the decline-vs-gather DECISION
// must be pinned white-box, not by row output — the same discipline the
// ClusteredBoxSeedsOrdinal pin follows.
func TestGatheredOuterConjunctCoupling(t *testing.T) {
	t.Parallel()
	innerCorr := values.NamedCorrelationIdentifier("EL")

	// OUTER-box arm. None → must GATHER an ORDINAL seed (an unconditional/
	// over-broad decline would break the verdict-clear case — the very bug
	// path); BAKEABLE → must ALSO GATHER (the merge later bakes
	// the conjunct over the gather's recorded legTypes); UNBAKEABLE → must
	// DECLINE to name-model (the conjunct would merge by name over a positional
	// gather with no per-leg window → malformed). Both FULL and LEFT boxes are
	// tested, since LEFT shares the one-opaque-leg gather with FULL.
	for _, kind := range []logical.JoinKind{logical.JoinFull, logical.JoinLeft} {
		box := logical.NewJoinWithPredicate(scan("SRC", "s"), scan("AUX", "x"), kind,
			chainEqPredLocal("x", "XID", "s", "SID"))
		u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL", AtAlias: "ORD"}
		j := logical.NewJoin(box, u, logical.JoinInner, "")

		trClear := newDisjointUnnestTranslator(t)
		trClear.unnestBoxLegConjunct = boxConjNone
		clear := trClear.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
		if clear == nil {
			t.Fatalf("%v box, verdict None: must GATHER (got nil) — the decline must fire ONLY on Unbakeable", kind)
		}
		if _, ok := clear.(*expressions.SelectExpression).GetResultValue().(*values.RecordConstructorValue); !ok {
			t.Fatalf("%v box, verdict None: gathered seed must be the ordinal RC, got %T",
				kind, clear.(*expressions.SelectExpression).GetResultValue())
		}

		trBake := newDisjointUnnestTranslator(t)
		trBake.unnestBoxLegConjunct = boxConjBakeable
		baked := trBake.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
		if baked == nil {
			t.Fatalf("%v box, BAKEABLE: must GATHER (got nil) — B2 admits bakeable conjuncts; a `!= boxConjNone` blanket decline regressed", kind)
		}
		if _, ok := baked.(*expressions.SelectExpression).GetResultValue().(*values.RecordConstructorValue); !ok {
			t.Fatalf("%v box, BAKEABLE: gathered seed must be the ordinal RC, got %T",
				kind, baked.(*expressions.SelectExpression).GetResultValue())
		}
		if _, hasRecord := trBake.unnestGatherBoxLegTypes[j]; !hasRecord {
			t.Fatalf("%v box, BAKEABLE: gather must RECORD its legTypes for the merge-site bake (one-authority law), found no record", kind)
		}

		trSet := newDisjointUnnestTranslator(t)
		trSet.unnestBoxLegConjunct = boxConjUnbakeable
		if got := trSet.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
			t.Fatalf("%v box, UNBAKEABLE: must DECLINE to name-model (got %T) — the conjunct merges by name over a positional gather with no per-leg window; gathering malforms", kind, got)
		}
	}

	// INNER-cluster arm — NOT declined by the verdict (even Unbakeable): each
	// leg keeps its own positional window, so a leg conjunct resolves THROUGH
	// the gather. The anti-over-decline sentinel: an over-broad decline of the
	// INNER arm drops the gather optimization silently (the e2e {7,8} can't
	// catch it — name-model also yields {7,8}), so pin the DECISION and that
	// the seed is ORDINAL (AnchoredJoin==false), mirroring the
	// ClusteredBoxSeedsOrdinal pin.
	innerCluster := inner(scan("SRC", "s"), scan("AUX", "x"))
	uInner := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL", AtAlias: "ORD"}
	jInner := logical.NewJoin(innerCluster, uInner, logical.JoinInner, "")
	trInner := newDisjointUnnestTranslator(t)
	trInner.unnestBoxLegConjunct = boxConjUnbakeable
	got := trInner.translateGatheredUnnestCluster(jInner, uInner, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if got == nil {
		t.Fatal("INNER cluster, UNBAKEABLE: must STILL GATHER (got nil) — the verdict couples only the OUTER-box arm; over-declining the INNER cluster drops the gather optimization")
	}
	if _, ok := got.(*expressions.SelectExpression).GetResultValue().(*values.RecordConstructorValue); !ok {
		t.Fatalf("INNER cluster, UNBAKEABLE: gathered seed must be the ordinal RC, got %T",
			got.(*expressions.SelectExpression).GetResultValue())
	}
}
