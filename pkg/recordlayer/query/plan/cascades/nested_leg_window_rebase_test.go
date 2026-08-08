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
// A nested reference's Field is its struct ROOT (`N`), so the NAME arm FOUND it,
// baked `Offset+ordinal(N)` and DROPPED the `.SK` — a merged address that is a
// real column of the wrong type, which is the silent class the whole re-anchor
// exists to remove. Only the pinned arm declined. A pin driving one form would
// have reported the other as covered.
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
func nestedLegFixture(t *testing.T) (leg *values.RecordType, mergedQOV *values.QuantifiedObjectValue) {
	return nestedLegFixtureOfKnownness(t, true)
}

// nestedLegFixtureOfKnownness builds the same fixture with the leg's struct
// column either TYPED or UNKNOWN.
//
// Both are real. The unknown form is what the planner actually hands the rebase
// today: neither the merged row's per-slot types nor the leg window's carry a
// struct column's type — both state UNKNOWN — so a fixture that only ever states
// a type exercises the arm the production path never takes. That gap is not
// hypothetical: it hid a typed-nil dereference that crashed every real query on
// this path while all four typed arms passed.
func nestedLegFixtureOfKnownness(t *testing.T, known bool) (leg *values.RecordType, mergedQOV *values.QuantifiedObjectValue) {
	t.Helper()
	nst := values.NewRecordType("NST", true, []values.Field{
		{Name: "SK", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "CO", FieldType: values.NullableInt, Ordinal: 1},
	})
	var legColType values.Type = nst
	if !known {
		legColType = values.UnknownType
	}
	leg = values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "N", FieldType: legColType, Ordinal: 1},
	})
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	// Slot 11 is the one a DROPPED suffix lands on, and slot 10 the one the
	// nested kind's first step names. Give both the leg's own shape so the
	// truncated address stays a *valid* read — the pre-fix bug was silent
	// precisely because nothing downstream could reject it.
	mergedFields[11] = values.Field{Name: "N", FieldType: nst, Ordinal: 11}
	mergedFields[10] = values.Field{Name: "L", FieldType: leg, Ordinal: 10}
	mergedQOV = values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("M"), values.NewRecordType("", false, mergedFields))
	return leg, mergedQOV
}

// nestedLegRef is the nested reference `L.N.SK`: ONE FieldValue whose Field is
// the struct root and whose resolved path is the full [N@1, SK@0], stated in the
// LEG's own layout. This is the shape the resolver's fuseNestedAccessors emits
// and the shape the fold carries.
func nestedLegRef(leg *values.RecordType, pinned bool) *values.FieldValue {
	return nestedLegRefTo(leg, pinned, "SK", 0)
}

// nestedLegRefTo builds the same reference against a NAMED struct member.
//
// THE MEMBER IS A PARAMETER BECAUSE SK SITS AT STRUCT ORDINAL 0, and a fixture
// whose discriminating value equals a plausible mutation's target cannot
// discriminate. Forcing a fused suffix ordinal to 0 is a NO-OP against an SK
// reference, so this file stayed green under a mutation the values-level pins
// caught — this file's own stated rule, landing on its own fixture. The CO
// variant sits at ordinal 1, so a forced-to-zero suffix is visible here too.
func nestedLegRefTo(leg *values.RecordType, pinned bool, member string, memberOrdinal int) *values.FieldValue {
	return &values.FieldValue{
		Field: "N",
		Typ:   values.NullableInt,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		Resolved: &values.FieldPath{
			Accessors: []values.ResolvedAccessor{
				{Field: "N", Ordinal: 1},
				{Field: member, Ordinal: memberOrdinal},
			},
			FrontierPinned: pinned,
			Domain:         values.OrdinalDomainOfType(leg),
		},
	}
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
		known    bool
		wantPath []int
	}{
		{"flatRun/unpinned/typed — the NAME arm, which silently truncated", values.LegKindFlatRun, false, true, []int{11, 0}},
		{"flatRun/pinned/typed — the baked arm, which declined", values.LegKindFlatRun, true, true, []int{11, 0}},
		{"nested/unpinned/typed", values.LegKindNested, false, true, []int{10, 1, 0}},
		{"nested/pinned/typed", values.LegKindNested, true, true, []int{10, 1, 0}},
		// THE ARM THE PLANNER ACTUALLY TAKES. The leg states UNKNOWN for its
		// struct column, so the suffix cannot be re-derived and is carried — which
		// is correct, because a suffix ordinal indexes the struct's own declared
		// field order and no merge can restate that.
		{"flatRun/unpinned/UNKNOWN leg column type", values.LegKindFlatRun, false, false, []int{11, 0}},
		{"flatRun/pinned/UNKNOWN leg column type", values.LegKindFlatRun, true, false, []int{11, 0}},
		{"nested/unpinned/UNKNOWN leg column type", values.LegKindNested, false, false, []int{10, 1, 0}},
		{"nested/pinned/UNKNOWN leg column type", values.LegKindNested, true, false, []int{10, 1, 0}},
		// THE NON-ZERO STRUCT MEMBER. Every case above descends to SK at struct
		// ordinal 0, so a fused suffix forced to zero is indistinguishable from a
		// correct one here. CO sits at ordinal 1, which is what makes the last
		// step of the path an assertion rather than a coincidence.
		{"flatRun/unpinned/non-zero struct member", values.LegKindFlatRun, false, true, []int{11, 1}},
		{"flatRun/pinned/non-zero struct member", values.LegKindFlatRun, true, true, []int{11, 1}},
		{"nested/unpinned/non-zero struct member", values.LegKindNested, false, true, []int{10, 1, 1}},
		{"nested/pinned/non-zero struct member/UNKNOWN type", values.LegKindNested, true, false, []int{10, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leg, mergedQOV := nestedLegFixtureOfKnownness(t, tc.known)
			windows := map[values.CorrelationIdentifier]ordinalLegWindow{
				values.NamedCorrelationIdentifier("L"): {
					Kind: tc.kind, Offset: 10, Typ: leg,
					Alias: values.NamedCorrelationIdentifier("L"),
				},
			}
			member, memberOrdinal := "SK", 0
			if strings.Contains(tc.name, "non-zero struct member") {
				member, memberOrdinal = "CO", 1
			}
			ref := nestedLegRefTo(leg, tc.pinned, member, memberOrdinal)
			out, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV)
			if !ok {
				t.Fatalf("rebaseOuterLegValueOrdinal DECLINED a multi-accessor leg "+
					"reference (kind=%v pinned=%t). A nested ORDER BY key over a JOIN "+
					"cannot then be folded and the query is rejected 0AF00.", tc.kind, tc.pinned)
			}
			fv, isFV := out.(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				t.Fatalf("rebase produced %T (%v), want a baked *FieldValue", out, out)
			}
			got := make([]int, len(fv.Resolved.Accessors))
			for i, a := range fv.Resolved.Accessors {
				got[i] = a.Ordinal
			}
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
			if _, isRec := fv.Typ.(*values.RecordType); fv.Typ == nil || isRec {
				t.Fatalf("kind=%v pinned=%t produced result type %v — want the LEAF "+
					"column's scalar type. A record type here means the fusion inherited "+
					"the first step's type instead of recomputing from the fused path.",
					tc.kind, tc.pinned, fv.Typ)
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
			leg, mergedQOV := nestedLegFixture(t)
			windows := map[values.CorrelationIdentifier]ordinalLegWindow{
				values.NamedCorrelationIdentifier("L"): {
					Kind: tc.kind, Offset: 10, Typ: leg,
					Alias: values.NamedCorrelationIdentifier("L"),
				},
			}
			// `L.A` — leg-local ordinal 0, one accessor.
			ref := &values.FieldValue{
				Field:    "A",
				Typ:      values.NullableInt,
				Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
				Resolved: values.NewFieldPathOfSingleInDomain("A", 0, tc.pinned, values.OrdinalDomainOfType(leg)),
			}
			out, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV)
			if !ok {
				t.Fatalf("CONTROL DECLINED (kind=%v pinned=%t): a single-accessor leg "+
					"reference has always rebased. Every decline this file reports is "+
					"void until this passes.", tc.kind, tc.pinned)
			}
			fv := out.(*values.FieldValue)
			got := make([]int, len(fv.Resolved.Accessors))
			for i, a := range fv.Resolved.Accessors {
				got[i] = a.Ordinal
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantPath) {
				t.Fatalf("CONTROL path %v, want %v (kind=%v pinned=%t)", got, tc.wantPath, tc.kind, tc.pinned)
			}
		})
	}
}
