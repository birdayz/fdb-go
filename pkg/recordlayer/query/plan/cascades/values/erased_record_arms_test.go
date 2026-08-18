package values

import "testing"

// The erased record — Java's Type.AnyRecord, carried here as an exactType with
// code RECORD and anyRecord set — is the shape two arms of this package special-
// case, and neither special case was observed by any test.
//
// The two arms are not the same kind of claim, and the difference is the point:
// one is DEFENCE that changes no answer today, the other decides an output. A
// single "pin the anyRecord arms" test that treated them alike would give the
// first one a green it does not earn and the second one no scrutiny.

// TestSeedTilingLegsRefusesAZeroWidthRow pins the fact that makes
// LayoutWithSeedLegs' anyRecord guard REDUNDANT rather than load-bearing:
// SeedTilingLegs yields nothing for a zero-width row. Dropping
// `|| carrierRow.anyRecord` changes no answer because an erased record reports
// zero fields and lands here.
//
// It pins the OUTCOME, not a particular guard, and that distinction was
// measured rather than chosen. Two independent checks produce it — the
// `width <= 0` refusal at the top, and `cursor != width` at the bottom, which a
// zero width can never satisfy because every leg has positive width — so
// relaxing EITHER alone leaves the answer unchanged and this test green.
// Relaxing both together makes SeedTilingLegs hand back 2 legs for a row of no
// columns, and this test reddens naming the count.
//
// Recorded because the obvious single-guard mutation is not a valid vacuity
// probe here, and reading a green from it as "the test is vacuous" would be as
// wrong as reading it as "the guard is load-bearing". The first version of this
// test WAS vacuous, for a different reason: its seed produced zero windows, so
// SeedTilingLegs returned nil at every width including its own.
func TestSeedTilingLegsRefusesAZeroWidthRow(t *testing.T) {
	t.Parallel()

	// The seed must be one OrdinalSeedLegWindows actually recognises, or the
	// refusal under test never runs and the whole test is vacuous. It is a flat
	// RUN of baked FieldValue leaves per leg — one per column, ordinal-addressed
	// off that leg's quantifier — not a record constructor whose fields are the
	// quantifiers themselves. The first version of this test used the latter,
	// produced ZERO windows, and stayed green under the very mutation it existed
	// to catch, while its own comment claimed the seed "WOULD tile if the width
	// allowed it".
	leg := func(corr CorrelationIdentifier, cols ...string) []RecordConstructorField {
		t.Helper()
		fields := make([]Field, len(cols))
		for i, c := range cols {
			fields[i] = Field{Name: c, FieldType: NotNullLong, Ordinal: i}
		}
		qov := mustQOV(t, corr, &RecordType{Fields: fields})
		out := make([]RecordConstructorField, len(cols))
		for i, c := range cols {
			fv, err := newFieldValueOfOrdinal(qov, i)
			if err != nil {
				t.Fatalf("baking %s.%s: %v", corr.Name(), c, err)
			}
			fv.Field = c
			out[i] = RecordConstructorField{Name: c, Value: fv}
		}
		return out
	}
	seed := NewRawRecordConstructorValue(append(
		leg(NamedCorrelationIdentifier("L"), "A1", "A2"),
		leg(NamedCorrelationIdentifier("R"), "B1")...)...)

	// THE POSITIVE CONTROL, asserted first and in the same test: at its true
	// width this seed DOES tile. Without it, every assertion below passes for a
	// seed that tiles at no width at all.
	const width = 3
	tiled := SeedTilingLegs(seed, width)
	if len(tiled) != 2 {
		t.Fatalf("SeedTilingLegs(seed, %d) returned %d legs, want 2 — the seed does "+
			"not tile at its own width, so the refusals below measure nothing",
			width, len(tiled))
	}

	for _, w := range []int{0, -1} {
		if legs := SeedTilingLegs(seed, w); legs != nil {
			t.Errorf("SeedTilingLegs(seed, %d) returned %d legs; it must refuse a "+
				"non-positive width. A row of no columns cannot be TILED, and that is "+
				"what makes LayoutWithSeedLegs. anyRecord guard redundant — with this "+
				"answer changed, that guard is the only thing left stopping an erased "+
				"record from claiming a leg table", w, len(legs))
		}
	}
	// A width the seed does not fill is refused too, which is the arm that shows
	// the refusal is about the WIDTH rather than about the seed being unusable.
	if legs := SeedTilingLegs(seed, width+1); legs != nil {
		t.Errorf("SeedTilingLegs(seed, %d) returned %d legs for a seed that tiles %d "+
			"slots; a partial tiling must be refused", width+1, len(legs), width)
	}
}

// TestErasedRecordIsNotSpelledAsAZeroFieldRecord pins the arm that DOES decide an
// output. describeMintedQOVType renders a record's arity; the erased record has
// no field list, and printing RecordType(0) for it would make it read as the
// concrete zero-field unit record — two shapes the exact channel keeps
// deliberately apart, since finishCanonical encodes the anyRecord bit precisely
// so equality cannot collapse them.
//
// This one is not defence: dropping `&& !qov.flowed.anyRecord` changes the census
// text, and the census is what a reader consults when a mint looks wrong.
func TestErasedRecordIsNotSpelledAsAZeroFieldRecord(t *testing.T) {
	t.Parallel()

	erasedHandle, err := SnapshotExactType(anyRecordType{})
	if err != nil {
		t.Fatalf("snapshotting the erased record failed: %v", err)
	}
	unitHandle, err := SnapshotExactType(&RecordType{})
	if err != nil {
		t.Fatalf("snapshotting the unit record failed: %v", err)
	}
	erased := &quantifiedObjectValue{
		correlation: NamedCorrelationIdentifier("E"),
		flowed:      erasedHandle.(*exactType),
	}
	unit := &quantifiedObjectValue{
		correlation: NamedCorrelationIdentifier("U"),
		flowed:      unitHandle.(*exactType),
	}

	if got, want := describeMintedQOVType(erased), TypeCodeRecord.String(); got != want {
		t.Errorf("describeMintedQOVType(erased) = %q, want %q — the erased record has "+
			"no field list to count", got, want)
	}
	if got, want := describeMintedQOVType(unit), "RecordType(0)"; got != want {
		t.Errorf("describeMintedQOVType(unit record) = %q, want %q", got, want)
	}
	if describeMintedQOVType(erased) == describeMintedQOVType(unit) {
		t.Error("the erased record and the concrete zero-field record now spell the " +
			"same, so the census cannot tell apart two shapes the exact channel keeps " +
			"deliberately distinct")
	}

	// Both still carry a type as far as the census's typed/untyped split is
	// concerned, so the spelling above is the ONLY thing distinguishing them
	// there — which is why it is worth a test of its own.
	if !quantifiedObjectValueCarriesAType(erased) || !quantifiedObjectValueCarriesAType(unit) {
		t.Error("both erased and unit records must count as carrying a type; if one " +
			"stops, the spelling assertions above are measuring a different partition")
	}
}
