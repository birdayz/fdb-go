package query

// RFC-173 W5 — white-box pins for the GATHERED multi-source lateral-unnest
// translation (the flat (N+1)-quantifier select, design ruling Q1 Option A).
// The FDB e2e (array_unnest_ordinality_fdb_test.go multi-source families)
// proves rows; these prove the SEED and QUANTIFIER shapes plus the commit-1
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
// shares PRICE, which the commit-1 name-ambiguity gate rightly declines — the
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

// TestRFC173W5_GatheredSeed_Shape pins the flat gathered translation: one
// select with N+1 quantifiers in FROM order; the Explode's collection is a
// BAKED reference to the OWNING source's own quantifier (the genuine
// per-source correlation the design ruling made decisive — never the
// whole-cluster concat); the seed = full ordinal leg runs + the W4c inner
// fields, asserted pristine for the full-baked AS+AT form.
func TestRFC173W5_GatheredSeed_Shape(t *testing.T) {
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
	if !isRC || rc.AnchoredJoin {
		t.Fatalf("seed = %T (anchored=%v), want the ordinal RC", gathered.GetResultValue(), isRC && rc.AnchoredJoin)
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

// TestRFC173W5_Gathered_DeclineBoundary pins the commit-1 fail-open scope:
// every declined shape returns nil so the caller keeps the name-model path
// that handles it today.
func TestRFC173W5_Gathered_DeclineBoundary(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	innerCorr := values.NamedCorrelationIdentifier("EL")

	// (a) ON-carrying cluster GATHERS (the commit-1 decline lifted with the
	// span-derivation extension): the ON conjunct rides the flat select baked
	// through the cluster spine.
	uOn := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	onLeft := logical.NewJoinWithPredicate(scan("SRC", "s"), scan("AUX", "x"), logical.JoinInner,
		corrEq("x", "XID", "s", "SID"))
	jOn := logical.NewJoin(onLeft, uOn, logical.JoinInner, "")
	gotOn := tr.translateGatheredUnnestCluster(jOn, uOn, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if gotOn == nil {
		t.Fatal("an ON-carrying cluster must GATHER (the commit-2 lift; R18 class)")
	}
	if len(gotOn.(*expressions.SelectExpression).GetPredicates()) == 0 {
		t.Fatal("the gathered ON-carrying select must carry the ON conjunct")
	}

	// (b) NAME-AMBIGUOUS: a column name shared by two legs (same table twice)
	// — bare resolution over the flat row would diverge from the name model's
	// last-binding-wins.
	uDup := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jDup := logical.NewJoin(inner(scan("SRC", "s"), scan("SRC", "s2")), uDup, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jDup, uDup, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("cross-leg duplicate column names must DECLINE (bare-name ambiguity)")
	}

	// (c) SHADOWING now GATHERS (the commit-2 lift): the element alias
	// equaling an outer column name resolves correctly — the visitor
	// qualifies the shadowed bare projection and the span windows route the
	// qualified read to the ELEMENT leg (last-binding-wins preserved).
	uShadow := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "V"}
	jShadow := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uShadow, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jShadow, uShadow, values.NamedCorrelationIdentifier("V"), values.NotNullLong, "ARR", unnestTrailing); got == nil {
		t.Fatal("an element alias shadowing an outer column must GATHER (the commit-2 shadow lift)")
	}

	// (d) UNGATED left cluster: flows name-model rows the baked collection
	// cannot consume. Re-fixtured (amendment-H) when the unnest-residual
	// slice lifted the LEFT-box decline (a gated outer box now gathers as
	// ONE opaque leg — TestRFC173UR_C1_OuterBoxLeft_Gathers): the still-
	// ungated class is a DUPLICATE-BINDING cluster, poisoned by the wedge
	// gate before any leg translates.
	uLeft := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jLeft := logical.NewJoin(
		inner(scan("SRC", "s"), scan("SRC", "s")),
		uLeft, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jLeft, uLeft, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("an UNGATED (duplicate-binding) cluster must DECLINE")
	}

	// (e) SINGLE-SOURCE left: the W4c binary path owns N=1.
	uSingle := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jSingle := logical.NewJoin(scan("SRC", "s"), uSingle, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jSingle, uSingle, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("a single-source outer must DECLINE the gather (the W4c binary seed owns it)")
	}

	// (f) The owner must be a gathered PLAIN leg: segment 0 naming NO leg
	// declines (an out-of-scope or derived owner).
	uNoOwner := &logical.LogicalUnnest{Segments: []string{"z", "ORDER_ID"}, Alias: "EL"}
	jNoOwner := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uNoOwner, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jNoOwner, uNoOwner, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatal("segment 0 naming no gathered leg must DECLINE")
	}

	// (g) A multi-segment path whose ROOT segment is not a column of the
	// owner window declines (the class-2 lift routes VALID struct paths
	// through the fused suffix; SRC has no column A, so the root lookup
	// misses — re-fixtured when the lift landed, amendment-H discipline).
	uDeep := &logical.LogicalUnnest{Segments: []string{"s", "A", "B"}, Alias: "EL"}
	jDeep := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uDeep, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jDeep, uDeep, innerCorr, values.NotNullLong, "A", unnestTrailing); got != nil {
		t.Fatal("a multi-segment path with a MISSING root column must DECLINE")
	}
}

// TestRFC173W5_Gathered_MixedElementSkipsAssert pins the NO-AT gathered form:
// a MIXED RC (baked outer runs + a direct bare-QOV element) — legitimately
// not AssertOrdinalJoinSeed-conformant, same rule as the W4c binary seed.
func TestRFC173W5_Gathered_MixedElementSkipsAssert(t *testing.T) {
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

// TestRFC173W5_BakeNormalizesCorrelationCase pins the gather-boundary case
// authority: bakeGatedJoinPredicates matches leg aliases case-insensitively
// but must EMIT the baked node under the leg-alias case the gather minted
// (UPPER via sourceAlias) — a baked correlation in the reference's original
// case diverges from the quantifier it binds to, and downstream
// classification (exact-compare) silently misrouted a lowercase ON conjunct
// as deeply-correlated during commit-2 bring-up. Unreachable via production
// SQL (uppercased upstream) — pinned white-box.
func TestRFC173W5_BakeNormalizesCorrelationCase(t *testing.T) {
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
// commit-3 rotation's canonical input.
func enclosedFixture(asAlias, atAlias string) *logical.LogicalJoin {
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: asAlias, AtAlias: atAlias}
	unnestJoin := logical.NewJoin(scan("SRC", "s"), u, logical.JoinInner, "")
	return logical.NewJoin(unnestJoin, scan("AUX", "x"), logical.JoinInner, "")
}

// TestRFC173W5_EnclosedRotation pins the commit-3 rotation: the buried-unnest
// cluster rebuilds as Join(Join(plain legs, FROM order), Unnest) — the exact
// root form the commit-1 builder owns — and the full dispatch produces the
// same flat gathered select the trailing form does.
func TestRFC173W5_EnclosedRotation(t *testing.T) {
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

// TestRFC173W5_EnclosedRotation_ONCollection pins the ON handling: every ON
// conjunct collected along the buried spine (including one referencing the
// ELEMENT alias) rides the rebuilt ROOT's OnPredicate — never the plain-leg
// chain, where the element is out of scope.
func TestRFC173W5_EnclosedRotation_ONCollection(t *testing.T) {
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

// TestRFC173W5_EnclosedRotation_DeclineBoundary pins the fail-open declines:
// (a) two buried unnests; (b) a single plain leg (nothing to enclose);
// (c) the element alias colliding with a leg AFTER the unnest in FROM order —
// the widened all-legs collision scope the rotation demands (the original
// gauntlet only sees the legs BEFORE the unnest); (d) a LEFT-kind root.
func TestRFC173W5_EnclosedRotation_DeclineBoundary(t *testing.T) {
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

// TestRFC173W5_EnclosedRotation_ONElementRewrite pins the review finding:
// a collected ON conjunct referencing the ELEMENT alias rides the rebuilt
// root's OnPredicate, and the builder must REWRITE it (rewriteUnnestPredicate
// — the WHERE merge arm's exact treatment) before baking: the Explode flows a
// bare scalar (no-AT), so an unrewritten `FieldValue(QOV(EL), "EL")`
// evaluates NIL and the join silently drops or misfilters every row.
func TestRFC173W5_EnclosedRotation_ONElementRewrite(t *testing.T) {
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

// TestRFC173W5_EnclosedRotation_ThreeLegsMidUnnest pins the wider-cluster
// bookkeeping the 2-leg pins cannot (the review coverage ask): with THREE
// plain legs and the unnest strictly in the middle of the FROM list
// (`FROM SRC s, s.ARR AS EL, AUX x, AUX2 y`), the rotation reports the right
// unnestPos and the built select preserves FROM order in BOTH the quantifier
// list and the seed-field offsets (the per-leg run-width summation).
func TestRFC173W5_EnclosedRotation_ThreeLegsMidUnnest(t *testing.T) {
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

// TestRFC173UR_C1_BoxLegOwner_Gathers pins unnest-residual class 1: an owner
// BURIED inside a box leg of the gathered cluster (`FROM (s LEFT x), y,
// s.ARR AS EL`) gathers — the collection bakes at the amendment-C WINDOW
// (the box quantifier's correlation, the buried leaf's offset in the
// concat), never a leg-local ordinal over a quantifier that does not exist
// at this select. Pre-slice this shape declined to the residual (the
// gatheredPlainLeg box decline).
func TestRFC173UR_C1_BoxLegOwner_Gathers(t *testing.T) {
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
		t.Fatalf("collection correlates to %s, want the BOX leg quantifier %s (amendment-C window)", qov.Correlation, quants[0].GetAlias())
	}
	rt := qov.Typ.(*values.RecordType)
	if len(rt.Fields) != 4 {
		t.Fatalf("collection QOV type = %d fields, want the 4-wide box concat", len(rt.Fields))
	}
	if acc, single := coll.Resolved.Single(); !single || acc.Ordinal != 1 {
		t.Fatalf("collection ordinal = %v, want box-level 1 (offset 0 + ARR idx 1)", coll.Resolved.Accessors)
	}
}

// TestRFC173UR_C1_OuterBoxLeft_Gathers pins the sibling lift (design ruling
// 1(i)): a gated OUTER box as the unnest's LEFT gathers as ONE OPAQUE leg —
// never its legs into the flat select — plus the Explode. The padded row's
// NULL array explodes to zero rows downstream (Java's Explode-over-NULL).
// The box carries a REAL ON predicate: the box's gating ON must stay INSIDE
// the box leg and never hoist into the flat select's predicates (a hoisted
// copy re-applies the ON as a WHERE and converts LEFT to INNER — the padded
// row's NULL-supplied columns fail it and the row silently drops; the FDB
// row-matrix pin caught exactly that with an ON-free box here).
func TestRFC173UR_C1_OuterBoxLeft_Gathers(t *testing.T) {
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

// TestRFC173UR_C1_DupAliasOwner_FirstMatch pins the owner-lookup discipline
// under DUPLICATE FROM aliases (design ruling 1(iii) — the item-1 binding
// model): the FIRST duplicate keeps the alias as its binding, later
// duplicates carry parser-minted ids the name lookup cannot reach, so
// `s.ARR` resolves to the FIRST leg's window and the collection correlates
// to THAT leg's binding — never the later duplicate's.
func TestRFC173UR_C1_DupAliasOwner_FirstMatch(t *testing.T) {
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
