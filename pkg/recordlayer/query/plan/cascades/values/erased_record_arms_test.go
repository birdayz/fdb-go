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
// LayoutWithSeedLegs' anyRecord guard REDUNDANT rather than load-bearing.
//
// Dropping `|| carrierRow.anyRecord` from that guard changes no answer, because
// an erased record reports zero fields and this refusal stops the walk one step
// later. That is a negative result and it is exactly the kind that rots: the
// guard reads as though it decides something, so a later reader may relax this
// refusal — the thing actually doing the work — without seeing that it re-arms
// the guard above it.
func TestSeedTilingLegsRefusesAZeroWidthRow(t *testing.T) {
	t.Parallel()

	// A seed that WOULD tile if the width allowed it, so the refusal under test
	// is the width and not an unrelated decline. Built from two record-typed
	// quantifiers, which is the flat-run shape OrdinalSeedLegWindows recognises.
	leg := func(name string, n int) Type {
		fields := make([]Field, n)
		for i := range fields {
			fields[i] = Field{
				Name:      name + uitoa(uint64(i)),
				FieldType: &PrimitiveType{TypeCode: TypeCodeLong},
				Ordinal:   i,
			}
		}
		return &RecordType{RecordName: name, Fields: fields}
	}
	left, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("L"), leg("L", 2))
	if err != nil {
		t.Fatalf("left leg: %v", err)
	}
	right, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("R"), leg("R", 2))
	if err != nil {
		t.Fatalf("right leg: %v", err)
	}
	seed := NewRecordConstructorValue(
		RecordConstructorField{Name: "L", Value: left},
		RecordConstructorField{Name: "R", Value: right},
	)

	for _, width := range []int{0, -1} {
		if legs := SeedTilingLegs(seed, width); legs != nil {
			t.Errorf("SeedTilingLegs(seed, %d) returned %d legs; it must refuse a "+
				"non-positive width. LayoutWithSeedLegs' anyRecord guard is redundant "+
				"ONLY because of this refusal — relaxing it makes that guard the only "+
				"thing stopping an erased record from claiming a tiling", width, len(legs))
		}
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
