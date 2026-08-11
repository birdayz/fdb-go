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

	nestedUnpinned := func(corr, root, leaf string) *values.FieldValue {
		return &values.FieldValue{
			Field: root,
			Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
			Typ:   values.NotNullLong,
			Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
				{Field: root, Ordinal: 2},
				{Field: leaf, Ordinal: 0},
			}},
		}
	}

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
			name: "flat baked leg read",
			value: &values.FieldValue{
				Field: "SK",
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
				Typ:   values.NotNullLong,
				Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
					{Field: "SK", Ordinal: 1},
				}},
			},
			wantKey: "M", wantOK: true,
		},
		{
			// The walk's OWN output: NewFieldValueOfOrdinal over a composed
			// frontier. Its ordinal already indexes that row.
			name: "machinery output, single accessor",
			value: &values.FieldValue{
				Field: "SK",
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
				Typ:   values.NotNullLong,
				Resolved: &values.FieldPath{
					FrontierPinned: true,
					Accessors:      []values.ResolvedAccessor{{Field: "SK", Ordinal: 4}},
				},
			},
			wantOK: false,
		},
		{
			// FUSED machinery output. Multi-accessor AND pinned — under the old
			// spelling the arity clause carried this decline, so dropping arity
			// without keying on the pin would have re-baked it.
			name: "machinery output, fused two-step",
			value: &values.FieldValue{
				Field: "SK",
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
				Typ:   values.NotNullLong,
				Resolved: &values.FieldPath{
					FrontierPinned: true,
					Accessors: []values.ResolvedAccessor{
						{Field: "N", Ordinal: 4},
						{Field: "SK", Ordinal: 0},
					},
				},
			},
			wantOK: false,
		},
		{
			// The merged-row qualified channel keeps its own decline: the child
			// names the MERGED row and the dot in the name carries the leg.
			name:   "flat-dotted merged-row read",
			value:  &values.FieldValue{Field: "M.N", Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("S")), Typ: values.NotNullLong},
			wantOK: false,
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
		{Name: "N", FieldType: values.NotNullLong, Ordinal: 2},
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
	fused := &values.FieldValue{
		Field: "SK",
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
		Typ:   values.NotNullLong,
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
			{Field: "N", Ordinal: 2},
			{Field: "SK", Ordinal: 0},
		}},
	}
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
	if len(suffix) != 1 || suffix[0].Field != "SK" {
		t.Errorf("suffix = %v, want one accessor SK — the descent must survive the "+
			"re-anchor, or the predicate compares the whole struct", suffix)
	}

	// The FLAT twin over the same window keeps the name lookup, so the fix is not
	// a blanket switch to accessors: a single-accessor node's display name IS its
	// column name, and it must still resolve to slot 1 rather than to N.
	flat := &values.FieldValue{
		Field: "SK",
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
		Typ:   values.NotNullLong,
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
			{Field: "SK", Ordinal: 1},
		}},
	}
	flatIdx, flatSuffix, flatOK := legRefRootInWindow(flat, window)
	if !flatOK || flatIdx != 1 || len(flatSuffix) != 0 {
		t.Errorf("flat reference resolved to (%d, %v, %v), want (1, [], true) — the "+
			"root-accessor rule must not disturb the single-accessor path", flatIdx, flatSuffix, flatOK)
	}

	// An ABSENT root declines rather than falling back to the display name, which
	// is the property that keeps a decline from becoming a wrong-slot read.
	absent := &values.FieldValue{
		Field: "SK",
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
		Typ:   values.NotNullLong,
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
			{Field: "NOPE", Ordinal: 0},
			{Field: "SK", Ordinal: 0},
		}},
	}
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
func nestedLegDescent(corr, root, leaf string) *values.FieldValue {
	return &values.FieldValue{
		Field: root,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
		Typ:   values.UnknownType,
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
			{Field: root, Ordinal: 0},
			{Field: leaf, Ordinal: 0},
		}},
	}
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
		f := b2BoxShapeWithPred(pred)
		j := f.Input.(*logical.LogicalJoin)
		return newChainedSpineTranslator(t).classifyBoxLegConjunct(
			j.Left.(*logical.LogicalJoin), j.Right.(*logical.LogicalUnnest), f.Predicate)
	}

	// The ROOT column exists in T4's buried window, so the reference resolves
	// and the gather may own the shape.
	if got := classify(t, eqConst(nestedLegDescent("T4", "ID", "LEAF"))); got != boxConjBakeable {
		t.Errorf("classifyBoxLegConjunct over a nested descent whose ROOT column resolves = %d, "+
			"want Bakeable(%d)", got, boxConjBakeable)
	}

	// The DISCRIMINATING direction: the root column does not exist. Before the
	// gate widened, legRef refused this node, no arm matched it, and the conjunct
	// classified Bakeable — the gather admitting a reference the bake cannot
	// resolve. An always-Bakeable classifier passes the arm above and fails here.
	if got := classify(t, eqConst(nestedLegDescent("T4", "NO_SUCH_COL", "LEAF"))); got != boxConjUnbakeable {
		t.Errorf("classifyBoxLegConjunct over a nested descent whose ROOT column is ABSENT "+
			"from the buried window = %d, want Unbakeable(%d). A Bakeable verdict here admits "+
			"the gather for a reference the bake cannot resolve — the wrong-slot class",
			got, boxConjUnbakeable)
	}
}

// TestRebaseLegRefsToBox_DeclinesANestedDescent pins the FIFTH legRef consumer,
// which is MASKED: its caller pre-filters on SourceRelativeBaked at the top of
// the walk, so a nested descent never reaches the legRef call below it.
//
// THIS IS A DELIBERATE NARROWNESS, NOT AN OVERSIGHT, and the pin exists because
// the two are indistinguishable from the outside. The walk resolves ONE name and
// bakes a one-step address; for a nested descent that name is the ROOT column,
// so admitting it without also fusing the suffix would bake the address of the
// enclosing struct and drop the descent — wrong rows, exactly the silent half of
// the defect the bake's own fix had to avoid.
//
// WHAT RE-ARMS IF THIS GOES GREEN THE OTHER WAY: widening the guard to legRef's
// own predicate (RootIsLegRelativeUnpinned) without landing the fuse beneath it
// at the same time. If you widen it, this pin must be REPLACED by one asserting
// the fused two-step address, not deleted — a deleted pin is how the halfway
// state ships.
func TestRebaseLegRefsToBox_DeclinesANestedDescent(t *testing.T) {
	t.Parallel()

	leg := values.NamedCorrelationIdentifier("L")
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "N", FieldType: values.UnknownType, Ordinal: 0},
	}}
	// Leg L starts at merged offset 2, so a leg-relative ordinal and the merged
	// ordinal it would have to mean are different numbers — a leg at offset 0
	// would make a half-rebase invisible.
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "X", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "Y", FieldType: values.UnknownType, Ordinal: 1},
		{Name: "N", FieldType: values.UnknownType, Ordinal: 2},
	}}
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		leg: {Kind: values.LegKindFlatRun, Offset: 2, Typ: legType, Alias: leg},
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("$box"), mergedType)

	nested := nestedLegDescent("L", "N", "SK")
	out, ok := rebaseLegRefsToBox(nested, windows, mergedType, boxQOV)
	if ok {
		t.Fatal("rebaseLegRefsToBox admitted a NESTED leg descent. If the guard was widened " +
			"on purpose, the fuse of Accessors[1:] must land in the same change and this pin " +
			"must be replaced by one asserting the FUSED two-step address. Admitting without " +
			"fusing bakes the enclosing struct's address and drops the descent — wrong rows")
	}
	fv, isFV := out.(*values.FieldValue)
	if !isFV {
		t.Fatalf("a declined reference must come back a FieldValue, got %T", out)
	}
	if fv.Resolved == nil || len(fv.Resolved.Accessors) != 2 {
		t.Fatalf("the declined node must come back UNTOUCHED (2 accessors), got %v — a "+
			"half-rewritten node is the shape the survivor check cannot classify", fv.Resolved)
	}
	if got := fv.Resolved.Root().Ordinal; got != 0 {
		t.Fatalf("the declined node's root ordinal moved to %d; it must stay LEG-relative (0). "+
			"A silently re-anchored root is a merged address that reads a foreign column", got)
	}
}
