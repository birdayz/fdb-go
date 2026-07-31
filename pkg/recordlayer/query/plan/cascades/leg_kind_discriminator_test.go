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
// must refuse a window that states no kind, and must refuse a NESTED one until
// it learns the fused two-step address.
//
// The nested arm matters even while nothing produces a nested window: it is the
// fail-CLOSED half of the sequencing. RFC-200 is explicit that the reader change
// is not sequenced after the producer change, because a nested window reaching
// this reader without the fused path bakes `Offset + legOrdinal` against a row
// where that is a different column — and `Offset + legOrdinal` is a perfectly
// valid merged ordinal, so nothing downstream can catch it.
func TestLegKind_ExistentialRebaseRefusesAnUnstatedKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		kind values.LegKind
		want bool // whether the rebase should succeed
		why  string
	}{
		{
			name: "flatRun — the control",
			kind: values.LegKindFlatRun,
			want: true,
			why: "Offset starts a run of the leg's own columns, so Offset+legOrdinal " +
				"is the leg's column. Without this arm passing, every decline below " +
				"holds for the uninteresting reason that the reader is broken",
		},
		{
			name: "UNSET — the invalid zero",
			kind: values.LegKindUnset,
			want: false,
			why: "a window reached the rebase without a stated kind. That is a PRODUCER " +
				"bug and it must surface as one; defaulting to flatRun would make the " +
				"discriminator a comment. A declined optimization is recoverable, a wrong " +
				"ordinal is not",
		},
		{
			name: "nested — fail-closed until the fused path exists",
			kind: values.LegKindNested,
			want: false,
			why: "Offset names ONE slot holding the whole leg row, so Offset+legOrdinal " +
				"addresses whatever follows that slot — a valid merged ordinal reading the " +
				"wrong column, which no downstream check can catch",
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
			if ok != tc.want {
				t.Fatalf("rebaseOuterLegValueOrdinal(kind=%v) ok=%t, want %t — %s",
					tc.kind, ok, tc.want, tc.why)
			}
			if !tc.want {
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
			if !isFV {
				t.Fatalf("rebase produced %T, want *FieldValue", out)
			}
			acc, single := fv.Resolved.Single()
			if !single || acc.Ordinal != 11 {
				t.Fatalf("flatRun rebase produced ordinal %+v, want 11 (offset 10 + leg-local 1)", fv.Resolved)
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
