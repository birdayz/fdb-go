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
)

// kindProbeFixture is a leg-local baked reference plus the merged row it would
// rebase onto: leg "L" of two columns, sitting at merged offset 10.
func kindProbeFixture(t *testing.T) (values.Value, values.QuantifiedObjectValue, *values.RecordType) {
	t.Helper()
	leg := values.NewRecordType("Leg", false, []values.Field{
		{Name: "A", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "B", FieldType: values.NullableInt, Ordinal: 1},
	})
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	mergedFields[10].FieldType = leg
	mergedType := values.NewRecordType("Merged", false, mergedFields)
	mergedQOV, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("M"), mergedType)
	mergedQOV = mustConstruct(t, mergedQOV, err)
	legQOV, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("L"), leg)
	legQOV = mustConstruct(t, legQOV, err)
	ref, err := values.ResolveOrdinalSeedField(legQOV, 1)
	ref = mustConstruct(t, ref, err)
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
			fv, isFV := values.AsFieldValue(out)
			if !isFV || fv.Path() == nil {
				t.Fatalf("rebase produced %T (%v), want an exact baked FieldValue", out, out)
			}
			got := fv.Path().Ordinals()
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

// finalizeSeedWindows ADDS a sub-window per buried leaf of a clustered BOX leg,
// so a 2-quantifier join with a box leg reports MORE than two map entries. That
// divergence between "how many legs tile the row" and "how many names address
// it" is live and is what this pins.
//
// It used to be pinned through the top-level RUN LIST, which existed for one
// consumer: the orientation gate materializedNLJOrdinalLayoutMatches, whose old
// form was `len(windows) != 2 -> return true` — the PERMISSIVE answer, admitting
// a swapped orientation whose baked ordinals no longer address the row the
// physical plan produces. That gate and its rule arm are retired (RFC-235), and
// the run-list accessor went with them rather than staying as a capability no
// production caller reaches. The map half survives because finalizeSeedWindows
// still runs.
func TestLegKind_BoxLegGainsSubWindowsPerBuriedLeaf(t *testing.T) {
	t.Parallel()

	mk := func(alias string, cols ...string) values.QuantifiedObjectValue {
		fields := make([]values.Field, len(cols))
		for i, c := range cols {
			fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
		}
		qov, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(alias),
			values.NewRecordType("", false, fields))
		return mustConstruct(t, qov, err)
	}
	bake := func(qov values.QuantifiedObjectValue, i int) values.RecordConstructorField {
		value, err := values.ResolveOrdinalSeedField(qov, i)
		fv := mustConstruct(t, value, err)
		field, ok := values.AsFieldValue(fv)
		if !ok {
			t.Fatalf("bake produced %T, want exact FieldValue", fv)
		}
		return values.RecordConstructorField{Name: field.DisplayName(), Value: fv}
	}

	// THE CONTROL. A plain 2-leg seed has no buried leaves, so the map reports
	// exactly one entry per leg. Without this the assertion below holds for the
	// uninteresting reason that the map is simply large.
	a := mk("A", "AID", "AV")
	b := mk("B", "BID")
	plain := values.NewRawRecordConstructorValue(bake(a, 0), bake(a, 1), bake(b, 0))
	plainWindows, _ := ordinalSeedLegWindowsOf(plain)
	if len(plainWindows) != 2 {
		t.Fatalf("a plain 2-leg seed reports %d map entries, want 2 — the control is "+
			"broken, so the box comparison below measures nothing", len(plainWindows))
	}

	// A 2-leg seed whose SECOND leg is a clustered BOX carrying two buried
	// leaves. Two legs still tile the row; the MAP gains the leaves' sub-windows.
	boxTyp := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "BV", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "EID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	boxTyp.Legs = []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("B"), "B", 0, 2),
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("E"), "E", 2, 1),
	}
	boxQOVValue, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("E"), boxTyp)
	boxQOV := mustConstruct(t, boxQOVValue, err)
	boxSeed := values.NewRawRecordConstructorValue(
		bake(a, 0), bake(a, 1), bake(boxQOV, 0), bake(boxQOV, 1), bake(boxQOV, 2))

	boxWindows, _ := ordinalSeedLegWindowsOf(boxSeed)
	if len(boxWindows) <= len(plainWindows) {
		t.Fatalf("the box seed reports %d map entries and the plain seed %d — the box "+
			"must report MORE. finalizeSeedWindows adds a sub-window per buried leaf; "+
			"if the counts no longer diverge it has stopped descending into a "+
			"clustered box leg, and every addressable buried name has silently lost "+
			"its window.", len(boxWindows), len(plainWindows))
	}
}
