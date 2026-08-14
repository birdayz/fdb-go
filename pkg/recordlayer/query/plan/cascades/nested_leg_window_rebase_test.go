package cascades

// The leg-window re-anchor for a MULTI-ACCESSOR (nested) leg reference.
//
// RFC-218 fixed the projected-EXISTS fold's nested sort key for a SINGLE-TABLE
// source: the key carries its resolved path and the re-anchor re-states the root
// ordinal in the layout the fold evaluates against. Over a JOIN the same key must
// travel through `rebaseOuterLegValueOrdinal`, whose contract this file pins.
//
// TWO ARMS, TWO DIFFERENT PRE-FIX FAILURES, and conflating them is how this
// survives a lap. A leg reference reaches the rebase in two forms and the walk
// dispatches on the path's frontier pin, not on its arity:
//
//	pinned  (FrontierPinned) -> the baked arm, which called Resolved.Single()
//	unpinned                 -> the NAME arm, which looked up Field in the leg type
//
// A nested reference's Field was its struct ROOT (`N`) when this was written, so
// the NAME arm FOUND it, baked `Offset+ordinal(N)` and DROPPED the `.SK` — a
// merged address that is a real column of the wrong type, which is the silent
// class the whole re-anchor exists to remove. Only the pinned arm declined. A
// pin driving one form would have reported the other as covered.
//
// The mint now names a fused reference after its LEAF, which does NOT retire
// this: the NAME arm is wrong for a nested reference either way. It looks up ONE
// segment in a flat leg type, so with the leaf it finds a same-named FLAT column
// where one exists and drops the descent just as silently. The fixtures below
// keep the root spelling because that is the shape that produced the measured
// break; a name-arm bake of a multi-accessor path is the defect, not the
// particular segment it was named after.
//
// Every case below calls the production symbol by name. The controls are not
// decoration: a decline whose control also declines is uninterpretable, because a
// harness defect and a genuine refusal are the same observation.

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// nestedLegFixture builds a leg `L` = [A, N] where N is the struct nst(SK, CO),
// plus a 16-column merged row. The leg sits at merged offset 10.
//
// N is at leg-local ordinal 1 and SK at struct-local ordinal 0, so the two
// numbers are DISTINCT — a fused path that dropped or duplicated a step is
// visible in the ordinals rather than hidden behind a coincidence.
func nestedLegFixture(
	t *testing.T,
	kind values.LegKind,
) (leg *values.RecordType, mergedQOV values.QuantifiedObjectValue) {
	t.Helper()
	nst := values.NewRecordType("NST", true, []values.Field{
		{Name: "SK", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "CO", FieldType: values.NullableInt, Ordinal: 1},
	})
	leg = values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "N", FieldType: nst, Ordinal: 1},
	})
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	// A flat-run seed exposes the leg's columns at 10..11; a nested seed exposes
	// the entire leg in slot 10. Build the exact physical row for the requested
	// kind so the resolver's result-type check proves the address as well as its
	// ordinals.
	if kind == values.LegKindNested {
		mergedFields[10] = values.Field{Name: "L", FieldType: leg, Ordinal: 10}
	} else {
		mergedFields[10] = values.Field{Name: "A", FieldType: values.NullableInt, Ordinal: 10}
		mergedFields[11] = values.Field{Name: "N", FieldType: nst, Ordinal: 11}
	}
	var err error
	mergedQOV, err = values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("M"), values.NewRecordType("", false, mergedFields))
	mergedQOV = mustConstruct(t, mergedQOV, err)
	return leg, mergedQOV
}

// nestedLegRef is the nested reference `L.N.SK`: ONE FieldValue whose resolved
// path is the full [N@1, SK@0], stated in the LEG's own layout — the shape the
// resolver's fuseNestedAccessors emits and the shape the fold carries.
//
// nestedLegRefTo builds the same reference against a NAMED struct member.
//
// THE MEMBER IS A PARAMETER BECAUSE SK SITS AT STRUCT ORDINAL 0, and a fixture
// whose discriminating value equals a plausible mutation's target cannot
// discriminate. Forcing a fused suffix ordinal to 0 is a NO-OP against an SK
// reference, so this file stayed green under a mutation the values-level pins
// caught — this file's own stated rule, landing on its own fixture. The CO
// variant sits at ordinal 1, so a forced-to-zero suffix is visible here too.
func nestedLegRefTo(t testing.TB, leg *values.RecordType, pinned bool, memberOrdinal int) values.Value {
	t.Helper()
	root, rootErr := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L"), leg)
	root = mustConstruct(t, root, rootErr)
	if !pinned {
		resolved, err := values.ResolveFieldOrdinals(root, []int{1, memberOrdinal})
		return mustConstruct(t, resolved, err)
	}
	suffix, suffixErr := values.FieldByOrdinal(memberOrdinal)
	suffix = mustConstruct(t, suffix, suffixErr)
	resolved, err := values.ResolveOrdinalSeedAccess(root, 1, []values.FieldRequest{suffix})
	return mustConstruct(t, resolved, err)
}

// TestNestedLegWindow_MultiAccessorRefRebasesOntoTheMergedRow drives
// rebaseOuterLegValueOrdinal over both leg kinds and both reference forms.
//
// THE EXPECTED ADDRESSES, and why each is the only correct one:
//
//	flatRun: the leg's columns ARE merged slots 10 and 11, so `N` is slot 11 and
//	         the address is "slot 11, then struct-local 0" = [11 0].
//	nested:  slot 10 holds the leg's WHOLE row, so the address is "slot 10, then
//	         leg-local 1, then struct-local 0" = [10 1 0].
//
// A path of [11] alone is the pre-fix NAME-arm answer: it reads the struct
// instead of its member. It is not an error and not an empty result — it is the
// wrong column, ordered plausibly.
func TestNestedLegWindow_MultiAccessorRefRebasesOntoTheMergedRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		kind     values.LegKind
		pinned   bool
		wantPath []int
	}{
		{"flatRun/unpinned", values.LegKindFlatRun, false, []int{11, 0}},
		{"flatRun/pinned", values.LegKindFlatRun, true, []int{11, 0}},
		{"nested/unpinned", values.LegKindNested, false, []int{10, 1, 0}},
		{"nested/pinned", values.LegKindNested, true, []int{10, 1, 0}},
		// THE NON-ZERO STRUCT MEMBER. Every case above descends to SK at struct
		// ordinal 0, so a fused suffix forced to zero is indistinguishable from a
		// correct one here. CO sits at ordinal 1, which is what makes the last
		// step of the path an assertion rather than a coincidence.
		{"flatRun/unpinned/non-zero struct member", values.LegKindFlatRun, false, []int{11, 1}},
		{"flatRun/pinned/non-zero struct member", values.LegKindFlatRun, true, []int{11, 1}},
		{"nested/unpinned/non-zero struct member", values.LegKindNested, false, []int{10, 1, 1}},
		{"nested/pinned/non-zero struct member", values.LegKindNested, true, []int{10, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leg, mergedQOV := nestedLegFixture(t, tc.kind)
			windows := map[values.CorrelationIdentifier]ordinalLegWindow{
				values.NamedCorrelationIdentifier("L"): {
					Kind: tc.kind, Offset: 10, Typ: leg,
					Alias: values.NamedCorrelationIdentifier("L"),
				},
			}
			memberOrdinal := 0
			if strings.Contains(tc.name, "non-zero struct member") {
				memberOrdinal = 1
			}
			ref := nestedLegRefTo(t, leg, tc.pinned, memberOrdinal)
			out, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV)
			if !ok {
				t.Fatalf("rebaseOuterLegValueOrdinal DECLINED a multi-accessor leg "+
					"reference (kind=%v pinned=%t). A nested ORDER BY key over a JOIN "+
					"cannot then be folded and the query is rejected 0AF00.", tc.kind, tc.pinned)
			}
			fv, isFV := values.AsFieldValue(out)
			if !isFV || fv.Path() == nil {
				t.Fatalf("rebase produced %T (%v), want an exact resolved FieldValue", out, out)
			}
			got := fv.Path().Ordinals()
			if fmt.Sprint(got) != fmt.Sprint(tc.wantPath) {
				t.Fatalf("kind=%v pinned=%t produced path %v, want %v.\n"+
					"  A path one step SHORT than wanted is the truncation bug: the root "+
					"was re-anchored and the nested suffix dropped, so the reference reads "+
					"the STRUCT where the user asked for one of its members. That address "+
					"is a real merged column, so nothing downstream rejects it.",
					tc.kind, tc.pinned, got, tc.wantPath)
			}
			// The RESULT TYPE must be the leaf column's, never the struct's. A
			// fusion that inherits the first step's type reports the whole record
			// as the type of a single-column read, and a sort built on it compares
			// records.
			resultType := fv.ResultType()
			if _, isRec := resultType.(*values.RecordType); resultType == nil || isRec {
				t.Fatalf("kind=%v pinned=%t produced result type %v — want the LEAF "+
					"column's scalar type. A record type here means the fusion inherited "+
					"the first step's type instead of recomputing from the fused path.",
					tc.kind, tc.pinned, resultType)
			}
		})
	}
}

// CONTROLS. Without these the declines above are uninterpretable: a fixture
// defect and a genuine refusal produce the same observation.
//
// The single-accessor reference is the shape that has always worked, in both
// forms, under both kinds. If any of these fails the fixture is broken and every
// verdict in this file is void.
func TestNestedLegWindow_SingleAccessorControlsStillRebase(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		kind     values.LegKind
		pinned   bool
		wantPath []int
	}{
		{"flatRun/unpinned", values.LegKindFlatRun, false, []int{10}},
		{"flatRun/pinned", values.LegKindFlatRun, true, []int{10}},
		{"nested/unpinned", values.LegKindNested, false, []int{10, 0}},
		{"nested/pinned", values.LegKindNested, true, []int{10, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leg, mergedQOV := nestedLegFixture(t, tc.kind)
			windows := map[values.CorrelationIdentifier]ordinalLegWindow{
				values.NamedCorrelationIdentifier("L"): {
					Kind: tc.kind, Offset: 10, Typ: leg,
					Alias: values.NamedCorrelationIdentifier("L"),
				},
			}
			// `L.A` — leg-local ordinal 0, one accessor.
			root, rootErr := values.NewQuantifiedObjectValue(
				values.NamedCorrelationIdentifier("L"), leg)
			root = mustConstruct(t, root, rootErr)
			var ref values.Value
			var refErr error
			if tc.pinned {
				ref, refErr = values.ResolveOrdinalSeedField(root, 0)
			} else {
				ref, refErr = values.ResolveFieldOrdinals(root, []int{0})
			}
			ref = mustConstruct(t, ref, refErr)
			out, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV)
			if !ok {
				t.Fatalf("CONTROL DECLINED (kind=%v pinned=%t): a single-accessor leg "+
					"reference has always rebased. Every decline this file reports is "+
					"void until this passes.", tc.kind, tc.pinned)
			}
			fv, isFV := values.AsFieldValue(out)
			if !isFV || fv.Path() == nil {
				t.Fatalf("CONTROL produced %T, want exact resolved FieldValue", out)
			}
			got := fv.Path().Ordinals()
			if fmt.Sprint(got) != fmt.Sprint(tc.wantPath) {
				t.Fatalf("CONTROL path %v, want %v (kind=%v pinned=%t)", got, tc.wantPath, tc.kind, tc.pinned)
			}
		})
	}
}
