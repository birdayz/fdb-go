package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// legRef's exclusion is MACHINERY-OWNERSHIP, and machinery-ownership is the
// FRONTIER PIN. It used to be spelled "FrontierPinned OR multi-accessor", on the
// reading that only the walk's own output is multi-accessor — and that reading
// was refuted by trace: a user-written nested descent (`m.n.sk`) arrives as ONE
// UNPINNED FieldValue with a leg-relative root and the descent in its remaining
// accessors, so the arity half declined exactly the references the walk exists
// to rebase.
//
// legRef is the shared prologue of FIVE call sites, so the fix widened all of
// the un-masked ones at once. This test covers the three in ordinal_seed.go —
// the bake closure, predicateLegAliases, predicateRefsBuriedLeg — and drives
// each directly rather than inferring the widening from the end-to-end query,
// because two of them are COUNTERS whose move is invisible in a row set: a
// nested reference now counts toward the cross-leg test and toward the
// buried-leg test, which is what makes the conjunct reach the bake at all. The
// other two consumers get their own pins below
// (TestClassifyLegConjunct_SeesANestedBoxLegDescent and
// TestRebaseLegRefsToBox_DeclinesANestedDescent).
//
// The pinned-decline arms are the other direction and they matter as much: if
// the pin stopped excluding, a re-walk would re-bake an address that already
// indexes the composed row — adding the leg offset a second time. The
// MULTI-ACCESSOR PINNED arm is the one the old spelling could not distinguish
// from a user nested ref, and it is the reason the replacement had to key on the
// pin rather than merely drop the arity clause.
func TestLegRef_AdmitsANestedUnpinnedRefAndStillDeclinesMachineryOutput(t *testing.T) {
	t.Parallel()

	nestedType := &values.RecordType{Fields: []values.Field{{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong}}}
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "SK", Ordinal: 1, FieldType: values.NotNullLong},
		{Name: "N", Ordinal: 2, FieldType: nestedType},
	}}
	legValue := func(corr string, ordinals ...int) values.Value {
		return exactTestField(t, exactTestQOV(t, corr, legType), ordinals...)
	}
	nestedUnpinned := func(corr, _, _ string) values.Value { return legValue(corr, 2, 0) }
	flat := legValue("M", 1)
	pinned, err := values.ResolveOrdinalSeedField(exactTestQOV(t, "M", legType), 1)
	if err != nil {
		t.Fatalf("pinned seed field: %v", err)
	}
	dottedType := &values.RecordType{Fields: []values.Field{{Name: "M.N", Ordinal: 0, FieldType: values.NotNullLong}}}
	dotted := exactTestField(t, exactTestQOV(t, "S", dottedType), 0)

	for _, tc := range []struct {
		name    string
		value   values.Value
		wantKey string
		wantOK  bool
	}{
		{
			// The shape the arity clause refused. Two accessors, UNPINNED root:
			// its root ordinal addresses M's own window, so it must be counted
			// and re-baked here exactly like its single-accessor twin.
			name:    "nested user descent over a leg QOV",
			value:   nestedUnpinned("M", "N", "SK"),
			wantKey: "M", wantOK: true,
		},
		{
			// The single-accessor twin, unchanged — the arm that always worked
			// and whose behaviour the widening must not disturb.
			name:    "flat baked leg read",
			value:   flat,
			wantKey: "M", wantOK: true,
		},
		{
			// The walk's OWN output: NewFieldValueOfOrdinal over a composed
			// frontier. Its ordinal already indexes that row.
			name:   "machinery output, single accessor",
			value:  pinned,
			wantOK: false,
		},
		{
			// Under RFC-232 the exact QOV owns attribution. A literal dot in the
			// field's display name cannot erase or manufacture correlation
			// identity; this is a source-S reference because QOV(S) says so.
			name:    "literal-dotted field on an exact owner",
			value:   dotted,
			wantKey: "S", wantOK: true,
		},
	} {
		key, ok := legRef(tc.value)
		if ok != tc.wantOK || (tc.wantOK && key != tc.wantKey) {
			t.Errorf("%s: legRef = (%q, %v), want (%q, %v)", tc.name, key, ok, tc.wantKey, tc.wantOK)
		}
	}

	// ---- The two COUNTERS legRef also feeds. Their move is what lets a
	// nested-only conjunct reach the bake instead of staying lazy for pushdown.
	legTyp := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SK", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "N", FieldType: nestedType, Ordinal: 2},
	}}

	crossLeg := &predicates.ComparisonPredicate{
		Operand: nestedUnpinned("M", "N", "SK"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: nestedUnpinned("R", "N", "SK"),
		},
	}
	flatLegs := map[string]bakeLegType{
		"M": {typ: legTyp, leafTyp: legTyp},
		"R": {typ: legTyp, leafTyp: legTyp},
	}
	if got := predicateLegAliases(crossLeg, flatLegs); got != 2 {
		t.Errorf("predicateLegAliases over `m.n.sk = r.n.sk` = %d, want 2.\n"+
			"Below 2 the conjunct fails the cross-leg test and stays LAZY — which is how a "+
			"nested ON predicate reached execution still correlated to its leg", got)
	}

	buriedLegs := map[string]bakeLegType{
		// A BURIED leaf: no quantifier of its own, so a lazy reference to it has
		// nowhere to push into and MUST bake even as the only leg named.
		"M": {typ: legTyp, leafTyp: legTyp, bakeCorr: "BOX"},
		"R": {typ: legTyp, leafTyp: legTyp},
	}
	singleBuried := &predicates.ComparisonPredicate{
		Operand: nestedUnpinned("M", "N", "SK"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	if !predicateRefsBuriedLeg(singleBuried, buriedLegs) {
		t.Error("predicateRefsBuriedLeg missed a NESTED reference into a buried leg. " +
			"A buried reference left lazy evaluates QOV(buriedAlias) → unbound → NULL and " +
			"silently drops rows; the single-leg cross-leg test cannot catch it")
	}
}

// A FUSED nested node is DISPLAY-named after its LEAF, so the display name must
// never select the slot. This is not a hypothetical: it broke this fix.
//
// RFC-231 established that a fused reference carries ONE name and that it is the
// leaf's (`m.n.sk` renders as `SK`), because that is what Java's
// getLastFieldName returns. The bake here originally resolved the root with
// `leafTyp.FieldIndexUnique(fv.Field)`, which was correct only while fv.Field
// happened to be the struct ROOT. The moment the leaf naming landed, `SK`
// resolved against the leg window and FOUND the top-level `SK` column — a real
// field, a valid ordinal, the wrong one. That is the silent class: an ordinal
// does not fail the way an unresolvable name does.
//
// The fixture is the collision itself — a flat `SK` beside a struct `N` whose
// member is also `SK` — and the ordinals are DIFFERENT numbers so the right
// answer and the wrong one cannot coincide. Keying on the resolved path's ROOT
// ACCESSOR is what makes this answer `N`; keying on the display name answers the
// flat column.
func TestLegRefRootInWindow_ResolvesTheRootAccessorNotTheLeafDisplayName(t *testing.T) {
	t.Parallel()

	// Slot 1 is the decoy: a real flat column named exactly like the nested leaf.
	window := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SK", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "N", FieldType: &values.RecordType{Fields: []values.Field{
			{Name: "SK", FieldType: values.NotNullLong, Ordinal: 0},
		}}, Ordinal: 2},
	}}

	// The fused shape exactly as it is minted: DISPLAY name "SK" (the leaf),
	// resolved path rooted at "N".
	fused := exactTestFieldView(t, exactTestField(t, exactTestQOV(t, "M", window), 2, 0))
	idx, suffix, ok := legRefRootInWindow(fused, window)
	if !ok {
		t.Fatal("legRefRootInWindow declined a fused nested reference whose ROOT column " +
			"is present in the window")
	}
	if idx != 2 {
		t.Errorf("resolved the root to slot %d, want 2 (the struct column N).\n"+
			"Slot 1 is the flat SK — the fused node's DISPLAY name. Resolving by display "+
			"name lands on a real column of the wrong type, and no ordinal check downstream "+
			"can reject it.", idx)
	}
	if len(suffix) != 1 || suffix[0] != 0 {
		t.Errorf("suffix = %v, want one accessor SK — the descent must survive the "+
			"re-anchor, or the predicate compares the whole struct", suffix)
	}

	// The FLAT twin over the same window keeps the name lookup, so the fix is not
	// a blanket switch to accessors: a single-accessor node's display name IS its
	// column name, and it must still resolve to slot 1 rather than to N.
	flat := exactTestFieldView(t, exactTestField(t, exactTestQOV(t, "M", window), 1))
	flatIdx, flatSuffix, flatOK := legRefRootInWindow(flat, window)
	if !flatOK || flatIdx != 1 || len(flatSuffix) != 0 {
		t.Errorf("flat reference resolved to (%d, %v, %v), want (1, [], true) — the "+
			"root-accessor rule must not disturb the single-accessor path", flatIdx, flatSuffix, flatOK)
	}

	// An ABSENT root declines rather than falling back to the display name, which
	// is the property that keeps a decline from becoming a wrong-slot read.
	absentRoot := &values.RecordType{Fields: []values.Field{{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong}}}
	absentType := &values.RecordType{Fields: []values.Field{{Name: "NOPE", Ordinal: 0, FieldType: absentRoot}}}
	absent := exactTestFieldView(t, exactTestField(t, exactTestQOV(t, "M", absentType), 0, 0))
	if _, _, ok := legRefRootInWindow(absent, window); ok {
		t.Error("a fused reference whose ROOT column is absent from the window resolved " +
			"anyway — a fallback to the leaf display name would find the flat SK and " +
			"read the wrong column silently")
	}
}

// nestedLegDescent is a user-written `<corr>.<root>.<leaf>`: ONE UNPINNED
// FieldValue whose root accessor is the leg column and whose second descends
// inside it.
//
// IT SETS Field TO THE ROOT NAME, AND SQL NO LONGER PRODUCES THAT. The real
// mint, fuseNestedAccessors (expr/expr.go), sets `out.Field = leaf.Name` — a
// fused value carries ONE name and it is the LEAF's. So this fixture is
// deliberately kept at the historical shape ONLY because the three tests below
// it are about ADMISSION (does legRef see the reference, do the counters count
// it), and admission never reads Field except through legRef's dotted guard,
// which neither name trips.
//
// It is therefore NOT a fixture for anything that RESOLVES a slot, and that
// limit is why none of those three tests caught the display-name defect: with
// Field set to the root, the old FieldIndexUnique(fv.Field) lookup happened to
// be right. A fixture that cannot express the current node shape is a test that
// cannot fail. The resolution axis has its own fixture, built the way SQL
// actually mints it — see
// TestLegRefRootInWindow_ResolvesTheRootAccessorNotTheLeafDisplayName, whose
// node carries the LEAF display name over a root accessor and a colliding flat
// column.
func nestedLegDescent(t testing.TB, corr, root, leaf string) values.Value {
	t.Helper()
	nested := &values.RecordType{Fields: []values.Field{{Name: leaf, Ordinal: 0, FieldType: values.NotNullLong}}}
	leg := &values.RecordType{Fields: []values.Field{{Name: root, Ordinal: 0, FieldType: nested}}}
	return exactTestField(t, exactTestQOV(t, corr, leg), 0, 0)
}

// classifyLegConjunct is the FOURTH legRef consumer and the only one that
// decides a PLAN SHAPE: it is the gather-admission verdict for a WHERE conjunct
// over a box / flat cluster, and it is NOT pre-filtered, so it widened with the
// gate.
//
// THE WIDENING FIXED A WRONG ADMISSION, which is why this needs both directions
// rather than a single happy-path arm. While legRef refused a nested descent,
// `isRef` was false here and the reference fell through to the dotted-frontier
// arm, which requires `Child == nil` — so a nested box-leg reference matched
// NOTHING and the conjunct classified BAKEABLE by default, including when its
// root column does not exist in the window. The gather then admitted a shape
// whose reference the bake could not resolve. Now the reference is seen, and the
// verdict tracks the window: Bakeable when the ROOT column resolves uniquely,
// Unbakeable when it does not.
//
// The verdict asks the ROOT only, and that is correct rather than sloppy: it is
// the same question the bake asks in the same window, asked through the SAME
// function — both call legRefRootInWindow, so a verdict cannot say "resolves"
// about a different column than the bake reads. The two used to spell the lookup
// out separately, and that arrangement could not survive the fused-leaf naming:
// a display name is not the root's, so two copies would have had to be corrected
// in lockstep. The suffix is not the verdict's business — a descent into a
// column that resolves is the fuse's problem, and the fuse is correct-or-loud.
func TestClassifyLegConjunct_SeesANestedBoxLegDescent(t *testing.T) {
	t.Parallel()

	eqConst := func(operand values.Value) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(operand, predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)},
		})
	}
	classify := func(t *testing.T, pred predicates.QueryPredicate) boxConjVerdict {
		t.Helper()
		box := logical.NewJoin(scan("T4", "T4"), scan("T", "T"), logical.JoinLeft, "")
		u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
		f := &logical.LogicalFilter{Input: logical.NewJoin(box, u, logical.JoinInner, ""), Predicate: pred}
		j := f.Input.(*logical.LogicalJoin)
		return newChainedSpineTranslator(t).classifyBoxLegConjunct(
			j.Left.(*logical.LogicalJoin), j.Right.(*logical.LogicalUnnest), f.Predicate)
	}

	// The ROOT column exists in T4's buried window, so the reference resolves
	// and the gather may own the shape.
	if got := classify(t, eqConst(nestedLegDescent(t, "T4", "ID", "LEAF"))); got != boxConjBakeable {
		t.Errorf("classifyBoxLegConjunct over a nested descent whose ROOT column resolves = %d, "+
			"want Bakeable(%d)", got, boxConjBakeable)
	}

	// The DISCRIMINATING direction: the root column does not exist. Before the
	// gate widened, legRef refused this node, no arm matched it, and the conjunct
	// classified Bakeable — the gather admitting a reference the bake cannot
	// resolve. An always-Bakeable classifier passes the arm above and fails here.
	if got := classify(t, eqConst(nestedLegDescent(t, "T4", "NO_SUCH_COL", "LEAF"))); got != boxConjUnbakeable {
		t.Errorf("classifyBoxLegConjunct over a nested descent whose ROOT column is ABSENT "+
			"from the buried window = %d, want Unbakeable(%d). A Bakeable verdict here admits "+
			"the gather for a reference the bake cannot resolve — the wrong-slot class",
			got, boxConjUnbakeable)
	}
}

// TestRebaseLegRefsToBoxFusesAnExactNestedDescent pins the FIFTH legRef
// consumer. Its caller used to pre-filter on SourceRelativeBaked at the top of
// the walk, so a nested descent never reached the legRef call below it; the
// guard has since been widened and the descent is now fused rather than
// declined — see the end of this comment.
//
// THIS IS A DELIBERATE NARROWNESS, NOT AN OVERSIGHT, and the pin exists because
// the two are indistinguishable from the outside. The walk resolves ONE name
// (w.Typ.FieldIndexUnique(fv.Field)) and bakes a one-step address from it. TWO
// separate things go wrong if a nested descent is admitted without changing that
// code, and the FIRST is the one that is easy to miss:
//
//   - fv.Field is the LEAF's name, not the root's. A fused nested value carries
//     ONE name and it is the leaf's (RFC-231; fuseNestedAccessors sets
//     out.Field = leaf.Name), so the lookup does not resolve the struct the path
//     starts at — it resolves whatever flat column of the leg happens to share
//     the leaf's spelling. On `nt(id, sk, n gst)` with `gst(sk, …)`, `m.n.sk`
//     resolves to the top-level SK. A real column, a valid ordinal, the wrong
//     one, and no ordinal check downstream can reject it.
//   - even with the root resolved correctly, the address reaches the enclosing
//     STRUCT and the descent is still dropped.
//
// WHAT THIS ASSERTS NOW: the widening happened, and this is the replacement the
// retired pin demanded. The guard reaches legRef's own predicate, the root is
// derived from Accessors[0] via legRefRootInWindow, and Accessors[1:] are fused
// — so the address is the two-step one and the descent survives.
//
// The comment that stood here documented
// TestRebaseLegRefsToBox_DeclinesANestedDescent, the pin from BEFORE that
// widening, and it ended by instructing that the pin "must be REPLACED by one
// asserting the fused two-step address, not deleted — a deleted pin is how the
// halfway state ships." The replacement is this function; the instruction was
// followed and the doc was not, which is how it came to describe a decline
// above a test named for a fuse.
//
// WHAT RE-ARMS: doing only ONE half again. Only the fuse is the wrong-slot
// read; only the root derivation drops the descent.
func TestRebaseLegRefsToBoxFusesAnExactNestedDescent(t *testing.T) {
	t.Parallel()

	leg := values.NamedCorrelationIdentifier("L")
	nestedType := &values.RecordType{Fields: []values.Field{{Name: "SK", FieldType: values.NotNullLong, Ordinal: 0}}}
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "N", FieldType: nestedType, Ordinal: 0},
	}}
	// Leg L starts at merged offset 2, so a leg-relative ordinal and the merged
	// ordinal it would have to mean are different numbers — a leg at offset 0
	// would make a half-rebase invisible.
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "N", FieldType: nestedType, Ordinal: 2},
	}}
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		leg: {Kind: values.LegKindFlatRun, Offset: 2, Typ: legType, Alias: leg},
	}
	boxQOV := exactTestQOV(t, "$box", mergedType)

	nested := nestedLegDescent(t, "L", "N", "SK")
	out, ok := rebaseLegRefsToBox(nested, windows, mergedType, boxQOV)
	if !ok {
		t.Fatal("rebaseLegRefsToBox declined an exact nested leg descent whose full fused path is addressable")
	}
	fv, isFV := values.AsFieldValue(out)
	if !isFV {
		t.Fatalf("rebased nested reference = %T, want exact FieldValue", out)
	}
	if got := fv.Path().Ordinals(); len(got) != 2 || got[0] != 2 || got[1] != 0 {
		t.Fatalf("rebased nested path = %v, want merged-root plus suffix [2 0]", got)
	}
	owner, ownerOK := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !ownerOK || owner.Correlation() != boxQOV.Correlation() || !owner.FlowedType().Equals(mergedType) {
		t.Fatalf("rebased nested owner = %v, want exact box-output QOV", owner)
	}
}
