package cascades

// RFC-200 step 3b: the leg KIND is an explicit discriminator with an INVALID
// zero, and these are what make "invalid" mean something.
//
// A zero value that is merely undocumented-as-invalid is a zero value that means
// flatRun, because that is what the readers will do with it. The whole point of
// the discriminator is that no reader may infer the kind — not from
// len(Typ.Fields), not from whether the field type is a record, not from
// Width == 1, and not from the language's choice of zero. So the claim needing a
// test is not "flatRun works"; it is "unset DOES NOT work".
//
// The precedent is one field over on the same struct: deleting `Alias:` from two
// producers left the whole suite green, because the zero CorrelationIdentifier
// is a legal value nobody checked.

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// kindProbeFixture is a leg-local baked reference plus the merged row it would
// rebase onto: leg "L" of two columns, sitting at merged offset 10.
func kindProbeFixture(t *testing.T) (values.Value, *values.QuantifiedObjectValue, *values.RecordType) {
	t.Helper()
	leg := &values.RecordType{Fields: []values.Field{
		{Name: "A", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "B", FieldType: values.NullableInt, Ordinal: 1},
	}}
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	mergedQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("M"), &values.RecordType{Fields: mergedFields})
	ref := &values.FieldValue{
		Field:    "B",
		Typ:      values.NullableInt,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		Resolved: values.NewFieldPathOfSingle("B", 1, true),
	}
	return ref, mergedQOV, leg
}

// The planner's existential rebase — keyed reader #1, the corpus's heaviest —
// must produce a DIFFERENT ADDRESS per kind, and must refuse a window that
// states no kind at all.
//
// The two addresses are not two spellings of one thing. A flat window's Offset
// starts a run of the leg's own columns, so `Offset + legOrdinal` IS the column.
// A nested window's Offset names ONE slot holding the whole leg row, so the same
// arithmetic addresses whatever follows that slot — and it produces a perfectly
// valid merged ordinal, which is why nothing downstream can catch it and why the
// dispatch has to happen here.
func TestLegKind_ExistentialRebaseAddressesEachKindDifferently(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		kind     values.LegKind
		wantOK   bool
		wantPath []int // the resolved accessor ordinals, in order
		why      string
	}{
		{
			name: "flatRun — one step, offset plus the leg-local ordinal",
			kind: values.LegKindFlatRun, wantOK: true, wantPath: []int{11},
			why: "the leg's columns ARE slots 10 and 11 of the merged row, so its " +
				"second column is slot 11",
		},
		{
			name: "nested — the FUSED two-step address",
			kind: values.LegKindNested, wantOK: true, wantPath: []int{10, 1},
			why: "slot 10 holds the leg's WHOLE row, so the address is 'slot 10, then " +
				"leg-local 1'. This is Java's ofOrdinalNumberAndFuseIfPossible: " +
				"PartitionSelectRule rewrites the reference to ofOrdinalNumber over the " +
				"merge quantifier, and translateCorrelations' leaf swap then fuses the " +
				"enclosing accessor onto it via FieldPath.withSuffix. Composition is a " +
				"path FUSION, never a flattening — nothing is materialized and nothing " +
				"is re-offset",
		},
		{
			name: "UNSET — the invalid zero", kind: values.LegKindUnset, wantOK: false,
			why: "a window reached the rebase without a stated kind. That is a PRODUCER " +
				"bug and it must surface as one; defaulting to flatRun would make the " +
				"discriminator a comment. A declined optimization is recoverable, a wrong " +
				"ordinal is not",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref, mergedQOV, leg := kindProbeFixture(t)
			windows := map[values.CorrelationIdentifier]ordinalLegWindow{
				values.NamedCorrelationIdentifier("L"): {
					Kind: tc.kind, Offset: 10, Typ: leg,
					Alias: values.NamedCorrelationIdentifier("L"),
				},
			}
			out, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV)
			if ok != tc.wantOK {
				t.Fatalf("rebaseOuterLegValueOrdinal(kind=%v) ok=%t, want %t — %s",
					tc.kind, ok, tc.wantOK, tc.why)
			}
			if !tc.wantOK {
				// A decline must hand the tree back UNCHANGED. A half-rebased tree is
				// the one outcome worse than either answer: some references moved onto
				// the merged row and some did not, and nothing downstream knows which.
				if out != ref {
					t.Fatalf("a declined rebase returned a MODIFIED tree (%T) — the contract "+
						"is correct-or-loud, never a half-rebased tree", out)
				}
				return
			}
			fv, isFV := out.(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				t.Fatalf("rebase produced %T (%v), want a baked *FieldValue", out, out)
			}
			got := make([]int, len(fv.Resolved.Accessors))
			for i, a := range fv.Resolved.Accessors {
				got[i] = a.Ordinal
			}
			if len(got) != len(tc.wantPath) {
				t.Fatalf("kind=%v produced a %d-step path %v, want the %d-step %v — %s",
					tc.kind, len(got), got, len(tc.wantPath), tc.wantPath, tc.why)
			}
			for i := range got {
				if got[i] != tc.wantPath[i] {
					t.Fatalf("kind=%v produced path %v, want %v — %s", tc.kind, got, tc.wantPath, tc.why)
				}
			}
		})
	}
}

// Every window and every merged leg the derivation emits states a kind.
//
// This is the producer half. Without it the readers' declines above would be
// correct and unreachable: the derivation could stop stamping and every seed
// would quietly stop rebasing, which reads as "the optimization does not fire"
// rather than as a bug.
func TestLegKind_TheDerivationStampsEveryWindowAndEveryLeg(t *testing.T) {
	t.Parallel()

	t1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	t2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID", "T1_ID"), false)
	seed, _ := reconstructFoldStep1Seed(t1, t2,
		values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
	if seed == nil {
		t.Fatal("two scan legs must reconstruct an ordinal step-1 seed")
	}

	windows, merged := ordinalSeedLegWindowsOf(seed)
	if len(windows) == 0 {
		t.Fatal("the layout authority declined the reconstructed seed")
	}
	for alias, w := range windows {
		if w.Kind == values.LegKindUnset {
			t.Errorf("window for leg %q states NO kind. Every reader declines an unset "+
				"window, so an unstamped producer turns the whole ordinal path off "+
				"silently — it reads as 'this shape does not ordinalize' rather than as "+
				"a missing stamp.", alias.Name())
		}
	}
	for i, leg := range merged.Legs {
		if leg.Kind == values.LegKindUnset {
			t.Errorf("merged leg[%d] (%s) states NO kind. This table is what the "+
				"executor's runtime binders read, and its legs are REBASED (not "+
				"re-minted) as they move, so an unset kind propagates outward from "+
				"here rather than staying local.", i, leg.Name)
		}
	}
}

// A REBASE carries the kind; it does not re-mint one.
//
// planBuriedLegConcat is the planner's leg-boundary producer and the one whose
// output feeds finalizeSeedWindows, which carries `w.Kind` onto the merged leg
// table. If either end re-minted, a nested sub-leg would arrive at the executor
// described as flat — the same defect class as a rebase that re-mints an Alias,
// which is what NewRecordTypeLeg's positional constructor exists to prevent.
func TestLegKind_ThePlannerLegConcatStampsFlatRun(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, commit2RecType("T", "ID", "V"), false)
	_, legs, ok := planBuriedLegConcat(scan, values.NamedCorrelationIdentifier("T"), 0)
	if !ok || len(legs) != 1 {
		t.Fatalf("planBuriedLegConcat over a scan = (%d legs, ok=%t), want (1, true)", len(legs), ok)
	}
	if legs[0].Kind != values.LegKindFlatRun {
		t.Fatalf("a scan leaf's leg states kind %v, want flatRun — a scan's columns ARE "+
			"the concat's slots, one each, which is the definition of a flat run",
			legs[0].Kind)
	}
}

// RFC-200 step 3d': materializedNLJOrdinalLayoutMatches gates on the TOP-LEVEL
// RUN LIST, and the map cannot serve that question.
//
// The old gate was `len(windows) != 2 -> return true`, described as "the safe
// default". It is the PERMISSIVE answer: the function's own doc says declining
// is always safe, because the join-commutativity exploration ADDS an alternative
// candidate and never removes the non-swapped one. Returning true admits a
// swapped orientation whose baked ordinals no longer address the row the
// physical plan produces.
//
// THE FAIL-OPEN IS PRE-EXISTING, not introduced by the nested kind, and this
// test is the demonstration. finalizeSeedWindows ADDS a sub-window per buried
// leaf of a clustered BOX leg, so a 2-quantifier join with a box leg already
// reports more than two map entries today and already skips the check. The run
// list says two, because two legs tile the row.
func TestLegKind_TheOrientationGateCountsTilesNotMapEntries(t *testing.T) {
	t.Parallel()

	mk := func(alias string, cols ...string) *values.QuantifiedObjectValue {
		fields := make([]values.Field, len(cols))
		for i, c := range cols {
			fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
		}
		return values.NewQuantifiedObjectValueOfType(
			values.NamedCorrelationIdentifier(alias),
			values.NewRecordType("", false, fields))
	}
	bake := func(qov *values.QuantifiedObjectValue, i int) values.RecordConstructorField {
		fv, err := values.NewFieldValueOfOrdinal(qov, i)
		if err != nil {
			t.Fatalf("bake: %v", err)
		}
		return values.RecordConstructorField{Name: fv.Field, Value: fv}
	}

	// A plain 2-leg seed: two tiles, two map entries. The two questions coincide
	// here, which is why a fixture without a box leg cannot tell them apart.
	a := mk("A", "AID", "AV")
	b := mk("B", "BID")
	plain := values.NewRawRecordConstructorValue(bake(a, 0), bake(a, 1), bake(b, 0))
	if _, _, runs := ordinalSeedLegLayoutOf(plain); len(runs) != 2 {
		t.Fatalf("a plain 2-leg seed reports %d tiles, want 2 — the control is broken", len(runs))
	}

	// A 2-leg seed whose SECOND leg is a clustered BOX carrying two buried
	// leaves. Still TWO tiles; the map gains the buried leaves' sub-windows.
	boxTyp := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "BV", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "EID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	boxTyp.Legs = []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("B"), "B", 0, 2),
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("E"), "E", 2, 1),
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("E"), boxTyp)
	boxSeed := values.NewRawRecordConstructorValue(
		bake(a, 0), bake(a, 1), bake(boxQOV, 0), bake(boxQOV, 1), bake(boxQOV, 2))

	windows, _, runs := ordinalSeedLegLayoutOf(boxSeed)
	if len(runs) != 2 {
		t.Fatalf("a 2-leg seed with a BOX second leg reports %d TILES, want 2 — the run "+
			"list is what makes the orientation gate answerable, and it must count the "+
			"legs that tile the row rather than the addressable names", len(runs))
	}
	if len(windows) <= 2 {
		t.Fatalf("the map reports %d entries for the box seed, want MORE than 2.\n"+
			"  This test's whole point is that the map's count and the tile count "+
			"diverge for a box leg — finalizeSeedWindows adds a sub-window per buried "+
			"leaf. If they no longer diverge, the fixture has stopped reproducing the "+
			"shape the pre-existing fail-open fires on, and the assertion above holds "+
			"for the uninteresting reason that the two counts agree.", len(windows))
	}

	// And the tiles arrive in OFFSET order, which is what identifies which
	// physical leg occupies which slot without consulting a name. Map iteration
	// order is randomised, so an unsorted list would make the gate
	// nondeterministic — passing on one run and declining on the next.
	if runs[0].Offset != 0 || runs[1].Offset != 2 {
		t.Fatalf("tiles arrived at offsets (%d, %d), want (0, 2) in offset order",
			runs[0].Offset, runs[1].Offset)
	}
	if len(runs[0].Typ.Fields) != 2 || len(runs[1].Typ.Fields) != 3 {
		t.Fatalf("tile widths (%d, %d), want (2, 3) — the SECOND tile is the whole box "+
			"run, not its narrowed rightmost-leaf sub-window. If it is 1, the run list "+
			"is being read out of the map AFTER finalization replaced that tile, which "+
			"is exactly the thing the map cannot serve.",
			len(runs[0].Typ.Fields), len(runs[1].Typ.Fields))
	}
}

// The CONSUMER half of 3d': materializedNLJOrdinalLayoutMatches must actually
// DECLINE a swapped box-leg seed, where it used to fail open.
//
// The test above pins the derivation — that the run list counts tiles while the
// map counts addressable names. This one pins that the gate READS the run list,
// which is a separate fact: a gate that still counted map entries would go on
// skipping this shape with the derivation perfectly correct.
//
// THE SHAPE IS THE PRE-EXISTING FAIL-OPEN, not a nested one. A 2-quantifier join
// whose second leg is a clustered BOX already reports more than two map entries
// today, so `len(windows) != 2 → return true` already skipped it — which is why
// this change moves box-leg plans that have nothing to do with the merge row,
// and why it lands on its own sequencing step.
func TestOrientationGate_DeclinesASwappedBoxLegSeed(t *testing.T) {
	t.Parallel()

	legA := values.NewRecordType("", false, []values.Field{
		{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "AV", FieldType: values.NotNullLong, Ordinal: 1},
	})
	boxTyp := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "BV", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "EID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	boxTyp.Legs = []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("B"), "B", 0, 2),
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("E"), "E", 2, 1),
	}

	aQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), legA)
	boxQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("E"), boxTyp)
	bake := func(qov *values.QuantifiedObjectValue, i int) values.RecordConstructorField {
		fv, err := values.NewFieldValueOfOrdinal(qov, i)
		if err != nil {
			t.Fatalf("bake: %v", err)
		}
		return values.RecordConstructorField{Name: fv.Field, Value: fv}
	}
	// The seed's baked layout: leg A tiles slots [0,2), the box tiles [2,5).
	seed := values.NewRawRecordConstructorValue(
		bake(aQOV, 0), bake(aQOV, 1),
		bake(boxQOV, 0), bake(boxQOV, 1), bake(boxQOV, 2))

	aPlan := plans.NewRecordQueryScanPlan([]string{"A"}, legA, false)
	boxPlan := plans.NewRecordQueryScanPlan([]string{"E"}, boxTyp, false)

	// CORRECT orientation: outer is A, inner is the box — exactly what the seed's
	// ordinals were baked against.
	if !materializedNLJOrdinalLayoutMatches(seed, aPlan, boxPlan) {
		t.Fatal("the gate DECLINED the correctly-oriented seed. Declining is always " +
			"safe, so this is not a correctness bug — but it means the negative below " +
			"holds for the uninteresting reason that the gate declines everything, and " +
			"it costs every box-leg plan its commutativity alternative.")
	}

	// SWAPPED orientation: the same seed with the legs exchanged, which is what a
	// WithSwappedQuantifiers firing produces — it reuses resultValue UNCHANGED.
	// The seed's ordinals now address the wrong row.
	if materializedNLJOrdinalLayoutMatches(seed, boxPlan, aPlan) {
		t.Fatal("the gate ACCEPTED a SWAPPED box-leg seed.\n" +
			"  This is the pre-existing FAIL-OPEN: `len(windows) != 2 -> return true`\n" +
			"  read the MAP, and a box leg's buried sub-windows already push that count\n" +
			"  past 2, so this shape has always skipped the check. Returning true is the\n" +
			"  PERMISSIVE answer, not the safe one — the function's own doc says\n" +
			"  declining is always safe because commutativity ADDS a candidate and never\n" +
			"  removes the non-swapped one.\n" +
			"  WHAT IT ADMITS: a stale-cost-model victory can promote this orientation to\n" +
			"  the memo's winner, and every baked reference into the seed then reads the\n" +
			"  wrong slot or fails loud with an unbound correlation — a plan the query\n" +
			"  cannot execute.\n" +
			"  If this went green again, the gate is reading the MAP instead of the\n" +
			"  top-level RUN LIST.")
	}
}
