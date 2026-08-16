package values

import "testing"

// legBearingCarrierLayout builds a two-leg merged row — L at [0,2), R at [2,4) —
// the shape a joined box flows.
func legBearingCarrierLayout(t *testing.T) (OrdinalLayout, *RecordType) {
	t.Helper()

	leg := func(prefix string, nullable bool) *RecordType {
		return &RecordType{Nullable: nullable, Fields: []Field{
			{Name: prefix + "_ID", Ordinal: 0, FieldType: NotNullLong},
			{Name: prefix + "_V", Ordinal: 1, FieldType: NullableLong},
		}}
	}
	carrierType := &RecordType{Fields: []Field{
		{Name: "L_ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "L_V", Ordinal: 1, FieldType: NullableLong},
		{Name: "R_ID", Ordinal: 2, FieldType: NullableLong},
		{Name: "R_V", Ordinal: 3, FieldType: NullableLong},
	}}
	left, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("L"), leg("L", false))
	if err != nil {
		t.Fatalf("left: %v", err)
	}
	right, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("R"), leg("R", true))
	if err != nil {
		t.Fatalf("right: %v", err)
	}
	layout, err := NewOrdinalLayoutForCarrierType(carrierType,
		[]OrdinalTileSpec{{Start: 0, Width: 4, Kind: OrdinalTileFlat}},
		[]OrdinalWindowSpec{
			{Source: left, FieldPaths: [][]int{{0}, {1}}},
			{Source: right, FieldPaths: [][]int{{2}, {3}}, NullSupplying: true},
		})
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	return layout, carrierType
}

// TestPhysicalCarrierTypeCarriesLegsWhileFlowedTypeDoesNot pins the split that
// makes the physical accessors necessary, in BOTH directions at once.
//
// FlowedType must keep withholding boundaries — Type() delegates to it, so legs
// there would put physical layout on the semantic surface. PhysicalCarrierType
// must state them, because a re-mint snapshots its layout from the type it is
// handed, and the public spelling therefore launders boundaries out of every
// value rebuilt through it. A merged row that has forgotten its boundaries does
// not read as "no legs" downstream: it reads as ONE run over the whole concat
// keyed by the box's rightmost leaf, so `R.R_ID` resolves at runOffset+ordinal
// into L's slots.
//
// Asserting only one side is what makes this worth writing. A guard that
// checked FlowedType alone would pass with the physical accessor deleted; one
// that checked the physical side alone would pass with layout leaked onto the
// semantic surface.
func TestPhysicalCarrierTypeCarriesLegsWhileFlowedTypeDoesNot(t *testing.T) {
	t.Parallel()

	layout, _ := legBearingCarrierLayout(t)

	public, isRecord := layout.Carrier().FlowedType().(*RecordType)
	if !isRecord {
		t.Fatalf("carrier FlowedType is %T, want *RecordType", layout.Carrier().FlowedType())
	}
	if len(public.Legs) != 0 {
		t.Fatalf("FlowedType carries %d legs; layout must not reach the semantic surface "+
			"(Type() delegates here)", len(public.Legs))
	}

	physical, isRecord := PhysicalCarrierType(layout).(*RecordType)
	if !isRecord {
		t.Fatalf("PhysicalCarrierType is %T, want *RecordType", PhysicalCarrierType(layout))
	}
	if len(physical.Legs) != 2 {
		t.Fatalf("PhysicalCarrierType carries %d legs, want 2. Without them a re-mint "+
			"produces a row that has forgotten where each leg starts, and a qualified "+
			"read lands in the first leg's slots.", len(physical.Legs))
	}
	for i, want := range []struct {
		name  string
		start int
		width int
	}{{"L", 0, 2}, {"R", 2, 2}} {
		if got := physical.Legs[i]; got.Name != want.name || got.Start != want.start || got.Width != want.width {
			t.Errorf("leg %d = %s[%d,+%d), want %s[%d,+%d)",
				i, got.Name, got.Start, got.Width, want.name, want.start, want.width)
		}
	}

	// The two types describe the same row and must remain interchangeable
	// wherever rows are COMPARED — otherwise carrying layout would start
	// rejecting plans instead of just informing reads.
	if !physical.Equals(public) || !public.Equals(physical) {
		t.Error("physical and public carrier types compare unequal; legs must not " +
			"participate in type equality")
	}
	if _, err := SnapshotExactType(physical); err != nil {
		t.Errorf("physical carrier type is not exact-snapshottable: %v", err)
	}
}

// TestRemintThroughPhysicalTypePreservesLegs pins the actual mechanism the bug
// ran through: the re-mint.
//
// NewQuantifiedObjectValue snapshots its source layout from the type it is
// given, so which accessor a caller reaches for decides whether the rebuilt
// value still knows its own shape. This is the difference stated as a test, so
// a future caller reaching for the convenient spelling fails here rather than in
// a wrong-rows report.
func TestRemintThroughPhysicalTypePreservesLegs(t *testing.T) {
	t.Parallel()

	layout, _ := legBearingCarrierLayout(t)
	merged := NamedCorrelationIdentifier("M")

	throughPublic, err := NewQuantifiedObjectValue(merged, layout.Carrier().FlowedType())
	if err != nil {
		t.Fatalf("re-mint through FlowedType: %v", err)
	}
	if record := PhysicalFlowedRecordTypeOf(throughPublic); record != nil && len(record.Legs) != 0 {
		t.Fatalf("re-mint through the PUBLIC type kept %d legs; if this ever starts "+
			"preserving them, the physical accessors below are no longer load-bearing "+
			"and this whole split should be revisited", len(record.Legs))
	}

	throughPhysical, err := NewQuantifiedObjectValue(merged, PhysicalCarrierType(layout))
	if err != nil {
		t.Fatalf("re-mint through PhysicalCarrierType: %v", err)
	}
	record := PhysicalFlowedRecordTypeOf(throughPhysical)
	if record == nil || len(record.Legs) != 2 {
		t.Fatalf("re-mint through the physical type kept %v legs, want 2 — the boundaries "+
			"must survive being carried from one value to the next", record)
	}

	// Identity is the same either way: the two re-mints differ only in physical
	// information, so nothing that keys on a QOV can tell them apart.
	if !SemanticEqualsUnderAliasMap(throughPublic, throughPhysical, EmptyAliasMap()) ||
		SemanticHashCode(throughPublic) != SemanticHashCode(throughPhysical) {
		t.Error("carrying layout changed QOV semantic identity; it must not")
	}
}
