package values_test

import (
	"bytes"
	"math"
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestFloatPhysicalOrderDivergesFromLogicalOrder is the FOUNDATION fact behind
// the ordering-claim termination in ordering_claim.go. Everything else in this
// file, and the planner's refusal to elide a sort on a float column, rests on
// it — so it is pinned here rather than asserted in prose.
//
// Two orders are compared over the same values:
//
//   - PHYSICAL: the byte order of the FDB tuple-packed key, which is the order
//     an index or primary-key scan hands rows back in.
//   - LOGICAL: values.CompareFloat64, the Record Layer's ordering authority
//     (faithful to java.lang.Double.compare).
//
// They agree for every ordinary value, and they DISAGREE on NaN. If this test
// ever goes green with the divergence gone (e.g. the tuple layer starts
// canonicalizing NaN), the planner's termination becomes unnecessary and this
// test is the signal to revisit it.
func TestFloatPhysicalOrderDivergesFromLogicalOrder(t *testing.T) {
	t.Parallel()

	negNaN := math.Float64frombits(0xFFF8000000000001)
	posNaN := math.Float64frombits(0x7FF8000000000002)
	negZero := math.Copysign(0, -1)

	type val struct {
		name string
		v    float64
	}
	vals := []val{
		{"negNaN", negNaN},
		{"-Inf", math.Inf(-1)},
		{"-1.5", -1.5},
		{"-0.0", negZero},
		{"+0.0", 0.0},
		{"1.5", 1.5},
		{"+Inf", math.Inf(1)},
		{"posNaN", posNaN},
	}

	physical := append([]val(nil), vals...)
	sort.SliceStable(physical, func(i, j int) bool {
		return bytes.Compare(tuple.Tuple{physical[i].v}.Pack(), tuple.Tuple{physical[j].v}.Pack()) < 0
	})

	// The physical order is the tuple-encoding order: NaN sits in TWO disjoint
	// blocks, one at each end, because encoding flips every bit of a negative
	// double (sending negative NaN below -Inf) and only the sign bit of a
	// non-negative one (sending positive NaN above +Inf).
	wantPhysical := []string{"negNaN", "-Inf", "-1.5", "-0.0", "+0.0", "1.5", "+Inf", "posNaN"}
	for i, got := range physical {
		if got.name != wantPhysical[i] {
			names := make([]string, len(physical))
			for j, p := range physical {
				names[j] = p.name
			}
			t.Fatalf("physical (FDB tuple key) order = %v, want %v", names, wantPhysical)
		}
	}

	// The exact divergences, stated one pair at a time so a failure names which
	// half of the mechanism moved.
	//
	// A NEGATIVE NaN is the physically FIRST value and the logically LAST one.
	// This single pair is what makes an elided sort return wrong rows.
	if got := values.CompareFloat64(negNaN, math.Inf(-1)); got != 1 {
		t.Fatalf("CompareFloat64(negNaN, -Inf) = %d, want 1 (NaN ranks greatest)", got)
	}
	if bytes.Compare(tuple.Tuple{negNaN}.Pack(), tuple.Tuple{math.Inf(-1)}.Pack()) != -1 {
		t.Fatal("packed negNaN must sort BEFORE packed -Inf; the physical/logical " +
			"divergence this file exists for has disappeared")
	}

	// A POSITIVE NaN, by contrast, agrees with the comparator — it is physically
	// last and logically last. Data containing ONLY positive NaN would not
	// expose the bug, which is why a regression test must use a NEGATIVE NaN (or
	// both signs) to be able to detect it at all.
	if got := values.CompareFloat64(posNaN, math.Inf(1)); got != 1 {
		t.Fatalf("CompareFloat64(posNaN, +Inf) = %d, want 1", got)
	}
	if bytes.Compare(tuple.Tuple{posNaN}.Pack(), tuple.Tuple{math.Inf(1)}.Pack()) != 1 {
		t.Fatal("packed posNaN must sort AFTER packed +Inf")
	}

	// The second, independent defect: all NaN payloads are ONE logical value
	// spread across two disjoint physical ranges. A tie class that is not
	// contiguous cannot have any LATER sort column ordered within it, which is
	// why a float coordinate terminates the claim instead of merely being
	// reordered.
	if got := values.CompareFloat64(negNaN, posNaN); got != 0 {
		t.Fatalf("CompareFloat64(negNaN, posNaN) = %d, want 0 (all NaN is one "+
			"logical value)", got)
	}
	if bytes.Equal(tuple.Tuple{negNaN}.Pack(), tuple.Tuple{posNaN}.Pack()) {
		t.Fatal("negNaN and posNaN must pack to DIFFERENT keys; the tie-class " +
			"split this file exists for has disappeared")
	}

	// SIGNED ZERO IS NOT PART OF THIS BUG. -0.0 packs immediately before +0.0
	// and the comparator also ranks -0.0 below +0.0, so the two orders AGREE.
	// Pinned as a negative result: it is what licenses the fix to leave
	// signed-zero row order untouched, and it is the fact that would silently
	// change if CompareFloat64 ever started tying the two zeros.
	if got := values.CompareFloat64(negZero, 0.0); got != -1 {
		t.Fatalf("CompareFloat64(-0.0, +0.0) = %d, want -1 — signed zero must "+
			"stay consistent with tuple order, or it becomes part of this bug", got)
	}
	if bytes.Compare(tuple.Tuple{negZero}.Pack(), tuple.Tuple{0.0}.Pack()) != -1 {
		t.Fatal("packed -0.0 must sort BEFORE packed +0.0")
	}
}

// TestTypeTerminatesOrderingClaim pins which types end an ordering claim.
func TestTypeTerminatesOrderingClaim(t *testing.T) {
	t.Parallel()

	terminating := map[string]values.Type{
		"FLOAT":  values.NullableFloat,
		"DOUBLE": values.NullableDouble,
	}
	for name, typ := range terminating {
		if !values.TypeTerminatesOrderingClaim(typ) {
			t.Errorf("%s must terminate an ordering claim: its FDB key order "+
				"is not its logical order (NaN sits in two disjoint blocks)", name)
		}
	}

	// Types with no NaN have identical physical and logical order, so they must
	// NOT terminate — otherwise the fix silently deletes sort elimination for
	// every ordinary column.
	continuing := map[string]values.Type{
		"LONG":    values.NullableLong,
		"INT":     values.NullableInt,
		"STRING":  values.NullableString,
		"BOOLEAN": values.NullableBoolean,
	}
	for name, typ := range continuing {
		if values.TypeTerminatesOrderingClaim(typ) {
			t.Errorf("%s must NOT terminate an ordering claim", name)
		}
	}

	// A type we cannot identify does not terminate. The burden of proof is on
	// "this is a float"; see the doc comment for why the opposite default
	// deletes sort elimination wherever a layout is absent.
	if values.TypeTerminatesOrderingClaim(nil) {
		t.Error("a nil type must not terminate an ordering claim")
	}
}

// TestClaimableOrderingPrefixTerminatesAtFloat pins the prefix rule: an
// ordering claim runs up to, and NOT through, the first float coordinate.
func TestClaimableOrderingPrefixTerminatesAtFloat(t *testing.T) {
	t.Parallel()

	layout := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 2},
		{Name: "E", FieldType: values.NullableFloat, Ordinal: 3},
		{Name: "S", FieldType: values.NullableString, Ordinal: 4},
	})

	for _, tc := range []struct {
		name  string
		names []string
		want  int
	}{
		{"all non-float claimable", []string{"ID", "A", "S"}, 3},
		{"terminates at leading double", []string{"D", "ID"}, 0},
		{"terminates at leading float", []string{"E", "ID"}, 0},
		// The columns BEFORE the float keep their claim — the float is where
		// the claim stops, not something that voids it retroactively.
		{"claims prefix before double", []string{"A", "D", "ID"}, 1},
		{"claims prefix before float", []string{"ID", "A", "E", "S"}, 2},
		// Case-insensitive resolution, matching column resolution everywhere.
		{"lowercase name still resolves", []string{"d"}, 0},
		// A name the layout does not declare cannot be shown to be a float.
		{"unknown name does not terminate", []string{"NOSUCH", "A"}, 2},
	} {
		if got := values.ClaimableOrderingPrefix(layout, tc.names); got != tc.want {
			t.Errorf("%s: ClaimableOrderingPrefix(%v) = %d, want %d",
				tc.name, tc.names, got, tc.want)
		}
	}

	// No layout at all: nothing can be proven, so nothing terminates.
	if got := values.ClaimableOrderingPrefix(nil, []string{"D", "E"}); got != 2 {
		t.Errorf("with no layout, ClaimableOrderingPrefix = %d, want 2", got)
	}
}

// TestColumnCanExtendOrderingClaimAmbiguousName pins the ambiguous-name arm.
//
// A layout can declare a name twice: NewRecordType's duplicate check is
// case-SENSITIVE while column resolution is case-INSENSITIVE. If EITHER
// matching field is a float, the coordinate could be a float, so the claim
// terminates. Addressability of an ambiguous key is a separate contract
// enforced downstream by the unique-match rule, deliberately not duplicated
// here.
func TestColumnCanExtendOrderingClaimAmbiguousName(t *testing.T) {
	t.Parallel()

	floatDup := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "VAL", FieldType: values.NullableString, Ordinal: 1},
		{Name: "val", FieldType: values.NullableDouble, Ordinal: 2},
	})
	if values.ColumnCanExtendOrderingClaim(floatDup, "VAL") {
		t.Error("an ambiguous name with a DOUBLE among its matches must " +
			"terminate the claim — the coordinate could be the float one")
	}

	stringDup := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "VAL", FieldType: values.NullableString, Ordinal: 1},
		{Name: "val", FieldType: values.NullableString, Ordinal: 2},
	})
	if !values.ColumnCanExtendOrderingClaim(stringDup, "VAL") {
		t.Error("an ambiguous name with no float among its matches must NOT " +
			"terminate the claim here; ambiguity is the resolver's contract")
	}
}
