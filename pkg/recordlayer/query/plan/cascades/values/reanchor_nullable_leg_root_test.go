package values

import "testing"

// TestReanchorCrossesANullabilityWidenedLegRoot pins that the ORDINAL proof
// still applies when a leg's two spellings differ only in the top-level
// nullable bit.
//
// An outer join pulls its output columns up with nullability (Java's
// Quantifier.pullUpResultColumnsWithNullability(true) mints
// QuantifiedObjectValue.of(alias, rowType.withNullability(true))), so the
// producer's leg root is NULLABLE while a lineage crossing mints the same leg
// from the child's own non-nullable carrier. One alias, two QOVs, one row —
// which is exactly what QuantifiedRowShapesAgree exists to say, and what every
// runtime QOV lookup already uses.
//
// Gating the ordinal proof on byte-equal root types instead threw that proof
// away and left ownership-by-NAME as the only evidence. Ownership names BOTH
// slots the moment a leg carries two same-named columns, so the crossing
// declined and the read stayed rooted on a box alias nothing binds at runtime
// (`exact QOV "LB$BOX" (…) has no declared runtime binding`, on
// `… la FULL OUTER JOIN lb … FULL OUTER JOIN cc …, la.arr AS x`).
//
// The two-K producer is the point: with one K, name ownership settles it and
// the strict gate is invisible.
func TestReanchorCrossesANullabilityWidenedLegRoot(t *testing.T) {
	t.Parallel()

	legRow := &RecordType{Fields: []Field{
		{Name: "AID", Ordinal: 0, FieldType: NullableLong},
		{Name: "K", Ordinal: 1, FieldType: NullableLong},
		{Name: "BID", Ordinal: 2, FieldType: NullableLong},
		{Name: "K", Ordinal: 3, FieldType: NullableLong},
	}}
	const legName = "LB$BOX"

	// The producer's spelling: the null-supplying leg, widened.
	widened, err := NewQuantifiedObjectValue(
		NamedCorrelationIdentifier(legName), WithNullability(legRow, true))
	if err != nil {
		t.Fatalf("widened leg: %v", err)
	}
	// The crossing's spelling: the child's own carrier row, not nullable.
	own, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier(legName), legRow)
	if err != nil {
		t.Fatalf("own leg: %v", err)
	}
	// Same shape, different alias — the leg identity check must still refuse it.
	foreign, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("OTHER$BOX"), legRow)
	if err != nil {
		t.Fatalf("foreign leg: %v", err)
	}

	field := func(source QuantifiedObjectValue, ordinal int) Value {
		t.Helper()
		v, resolveErr := ResolveFieldOrdinals(source, []int{ordinal})
		if resolveErr != nil {
			t.Fatalf("field %d: %v", ordinal, resolveErr)
		}
		return v
	}

	// Both output slots are named K and both are owned by the same leg, so
	// ownership alone cannot choose between them.
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "K", Value: field(widened, 1)},
		RecordConstructorField{Name: "K", Value: field(widened, 3)},
	)
	producerType, ok := producer.Type().(*RecordType)
	if !ok {
		t.Fatalf("producer type is %T, want *RecordType", producer.Type())
	}
	layout, err := NewOrdinalLayoutForCarrierType(producerType,
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("target layout: %v", err)
	}
	carrier := layout.Carrier()

	// carrierOrdinal returns the output slot a read landed on, or -1 when the
	// crossing declined and left the read on its own root.
	carrierOrdinal := func(t *testing.T, read Value) int {
		t.Helper()
		reanchored, reanchorErr := ReanchorValueThroughProducer(read, producer, carrier)
		if reanchorErr != nil {
			t.Fatalf("reanchor %s: %v", ExplainValue(read), reanchorErr)
		}
		fv, isField := AsFieldValue(reanchored)
		if !isField {
			return -1
		}
		root, isRoot := AsQuantifiedObjectValue(fv.ChildValue())
		if !isRoot || root.Correlation() != carrier.Correlation() {
			return -1
		}
		path := fv.Path().Ordinals()
		if len(path) != 1 {
			t.Fatalf("reanchor %s produced path %v, want one step", ExplainValue(read), path)
		}
		return path[0]
	}

	for _, tc := range []struct {
		name string
		read Value
		want int
	}{
		{
			// THE case: the widened/own pair, told apart from its same-named
			// sibling only by the ordinal.
			name: "own-root read of the first K",
			read: field(own, 1),
			want: 0,
		},
		{
			// The other K, so a fix that simply prefers the first owned slot
			// is not mistaken for a fix.
			name: "own-root read of the second K",
			read: field(own, 3),
			want: 1,
		},
		{
			// The CONTROL: the producer's own spelling. It crossed
			// throughout, and it is what says the arms above measure the
			// nullability difference rather than the two-K ambiguity.
			name: "widened-root read of the second K",
			read: field(widened, 3),
			want: 1,
		},
		{
			// Same row shape, different leg. Nullability is not a licence to
			// ignore identity: this must decline, not land on a K slot.
			name: "foreign leg of the same shape declines",
			read: field(foreign, 1),
			want: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := carrierOrdinal(t, tc.read); got != tc.want {
				t.Errorf("%s reanchored to carrier slot %d, want %d",
					ExplainValue(tc.read), got, tc.want)
			}
		})
	}
}

// TestQuantifiedRootsDenoteTheSameRow drives every arm of the ordinal proof's
// identity check directly.
//
// The corpus reaches the arms it happens to plan; this drives all of them,
// including the two that make the relaxation SAFE rather than merely useful —
// a foreign alias of the same shape, and a CURRENT root, which carries no
// correlation to identify it with (every phase spells it `_current`) so its
// exact type is the only thing separating two row phases.
func TestQuantifiedRootsDenoteTheSameRow(t *testing.T) {
	t.Parallel()

	row := &RecordType{Fields: []Field{
		{Name: "AID", Ordinal: 0, FieldType: NullableLong},
		{Name: "K", Ordinal: 1, FieldType: NullableLong},
	}}
	wider := &RecordType{Fields: []Field{
		{Name: "AID", Ordinal: 0, FieldType: NullableLong},
		{Name: "K", Ordinal: 1, FieldType: NullableLong},
		{Name: "EXTRA", Ordinal: 2, FieldType: NullableLong},
	}}
	named := func(name string, typ Type) *quantifiedObjectValue {
		t.Helper()
		v, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier(name), typ)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		exact, ok := v.(*quantifiedObjectValue)
		if !ok {
			t.Fatalf("%s is %T, want the package-owned QOV", name, v)
		}
		return exact
	}
	// A current carrier is owner-scoped and can only come from a layout, which
	// is the point: two independently built layouts over the same row type are
	// two DIFFERENT phases that happen to agree exactly.
	currentOver := func(typ *RecordType) *quantifiedObjectValue {
		t.Helper()
		layout, err := NewOrdinalLayoutForCarrierType(typ,
			[]OrdinalTileSpec{{Start: 0, Width: len(typ.Fields), Kind: OrdinalTileFlat}}, nil)
		if err != nil {
			t.Fatalf("carrier layout: %v", err)
		}
		exact, ok := layout.Carrier().(*quantifiedObjectValue)
		if !ok {
			t.Fatalf("carrier is %T, want the package-owned QOV", layout.Carrier())
		}
		return exact
	}

	for _, tc := range []struct {
		name             string
		candidate, given *quantifiedObjectValue
		want             bool
	}{
		{
			name:      "same alias, byte-equal rows",
			candidate: named("A", row),
			given:     named("A", row),
			want:      true,
		},
		{
			// The outer-join pairing: one alias, two spellings, one row.
			name:      "same alias, top-level nullability differs",
			candidate: named("A", WithNullability(row, true)),
			given:     named("A", row),
			want:      true,
		},
		{
			// Nullability is the ONLY difference the relaxation forgives.
			name:      "same alias, different arity",
			candidate: named("A", wider),
			given:     named("A", row),
			want:      false,
		},
		{
			name:      "different alias, identical rows",
			candidate: named("A", row),
			given:     named("B", row),
			want:      false,
		},
		{
			// Two phases that agree exactly ARE the same row for this check;
			// the caller has already established the ordinal path.
			name:      "current phases with equal exact rows",
			candidate: currentOver(row),
			given:     currentOver(row),
			want:      true,
		},
		{
			// And two that do not agree are not, with no alias to appeal to.
			name:      "current phases with different exact rows",
			candidate: currentOver(wider),
			given:     currentOver(row),
			want:      false,
		},
		{
			name:      "nil candidate",
			candidate: nil,
			given:     named("A", row),
			want:      false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := quantifiedRootsDenoteTheSameRow(tc.candidate, tc.given); got != tc.want {
				t.Errorf("quantifiedRootsDenoteTheSameRow = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReanchorCrossesANullabilityWidenedLegRootIntoANestedPath is the same
// proof one step deeper, on the arm that reads THROUGH a retained leg column.
//
// The two arms are separate code: a one-step read (`L.ID`) is answered by the
// name/ownership match, while a nested read (`L.N.B`) is answered by the
// PREFIX match, which carries the suffix past the producer slot. Only the
// first tolerated the widened/own root pair, so a query mixing the two got
// half an answer — `l.id` crossed onto the joined carrier and `l.n.b` stayed
// rooted on a leg the materializer had already dropped, failing at runtime
// with `exact QOV "L" … has no declared runtime binding`.
//
// That asymmetry is the dimension this file did not probe: it exercised the
// widened root only at depth one, where the prefix arm is never consulted.
func TestReanchorCrossesANullabilityWidenedLegRootIntoANestedPath(t *testing.T) {
	t.Parallel()

	nested := &RecordType{Nullable: true, Fields: []Field{
		{Name: "A", Ordinal: 0, FieldType: NullableLong},
		{Name: "B", Ordinal: 1, FieldType: NullableLong},
	}}
	legRow := &RecordType{Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "N", Ordinal: 1, FieldType: nested},
	}}

	leg := func(t *testing.T, name string, nullable bool) QuantifiedObjectValue {
		t.Helper()
		qov, err := NewQuantifiedObjectValue(
			NamedCorrelationIdentifier(name), WithNullability(legRow, nullable))
		if err != nil {
			t.Fatalf("%s leg (nullable=%v): %v", name, nullable, err)
		}
		return qov
	}
	// L is the NULL-SUPPLYING leg: the producer spells it widened, a lineage
	// crossing spells it from the leg's own non-nullable carrier. R is the
	// preserved leg, where both spellings already agree — it is the control
	// that says these arms measure the widening and not the nesting.
	lWidened := leg(t, "L", true)
	lOwn := leg(t, "L", false)
	rLeg := leg(t, "R", false)
	foreign := leg(t, "OTHER", false)

	path := func(t *testing.T, source QuantifiedObjectValue, ordinals ...int) Value {
		t.Helper()
		v, err := ResolveFieldOrdinals(source, ordinals)
		if err != nil {
			t.Fatalf("path %v: %v", ordinals, err)
		}
		return v
	}

	// The flattened two-leg row a join materializes: each leg's ID and its
	// whole N struct, L first. Reading L.N.B therefore means slot 1 then B.
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "ID", Value: path(t, lWidened, 0)},
		RecordConstructorField{Name: "N", Value: path(t, lWidened, 1)},
		RecordConstructorField{Name: "ID", Value: path(t, rLeg, 0)},
		RecordConstructorField{Name: "N", Value: path(t, rLeg, 1)},
	)
	producerType, ok := producer.Type().(*RecordType)
	if !ok {
		t.Fatalf("producer type is %T, want *RecordType", producer.Type())
	}
	layout, err := NewOrdinalLayoutForCarrierType(producerType,
		[]OrdinalTileSpec{{Start: 0, Width: 4, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("target layout: %v", err)
	}
	carrier := layout.Carrier()

	// carrierPath returns the ordinal path a read landed on, or nil when the
	// crossing declined and left the read on its own root.
	carrierPath := func(t *testing.T, read Value) []int {
		t.Helper()
		reanchored, reanchorErr := ReanchorValueThroughProducer(read, producer, carrier)
		if reanchorErr != nil {
			t.Fatalf("reanchor %s: %v", ExplainValue(read), reanchorErr)
		}
		fv, isField := AsFieldValue(reanchored)
		if !isField {
			return nil
		}
		root, isRoot := AsQuantifiedObjectValue(fv.ChildValue())
		if !isRoot || root.Correlation() != carrier.Correlation() {
			return nil
		}
		return fv.Path().Ordinals()
	}
	equal := func(got, want []int) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	for _, tc := range []struct {
		name string
		read Value
		want []int
	}{
		{
			// THE case: nested read, own-root spelling, widened producer slot.
			name: "own-root nested read of the null-supplying leg",
			read: path(t, lOwn, 1, 1),
			want: []int{1, 1},
		},
		{
			// The one-step read on the same leg. It already worked, and it is
			// what makes the failure above a HALF answer rather than a plain
			// decline — both live in one projection.
			name: "own-root one-step read of the null-supplying leg",
			read: path(t, lOwn, 0),
			want: []int{0},
		},
		{
			// The preserved leg, whose two spellings agree. Crossing here is
			// not evidence about the widening, which is why it is the control.
			name: "preserved leg nested read",
			read: path(t, rLeg, 1, 1),
			want: []int{3, 1},
		},
		{
			// The producer's own spelling of the widened leg.
			name: "widened-root nested read",
			read: path(t, lWidened, 1, 1),
			want: []int{1, 1},
		},
		{
			// Same row shape, different alias. Nullability tolerance must not
			// become identity tolerance: this declines rather than landing on
			// either leg's N.
			name: "foreign leg of the same shape declines",
			read: path(t, foreign, 1, 1),
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := carrierPath(t, tc.read); !equal(got, tc.want) {
				t.Errorf("%s reanchored to carrier path %v, want %v",
					ExplainValue(tc.read), got, tc.want)
			}
		})
	}
}
