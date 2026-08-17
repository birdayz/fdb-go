package values

import "testing"

// TestReanchorDoesNotBindOneSourceToAnothersSlot pins the rule that keeps a
// producer's own sources from being confused with each other.
//
// Two legs of a join can expose identically-named, identically-typed columns —
// `SELECT A.VAL, B.VAL FROM LEFTT A LEFT JOIN RIGHTT B` over a merged row
// [LID VAL RID VAL]. Matching by name+type alone then finds A's slot for B.VAL,
// and because that is the ONLY name match, a single-match guard cannot tell it
// from a correct answer: both output columns come to read A.VAL. Wrong values,
// silently, plus the null-supplying leg's nullability lost with them.
//
// The rule: while the producer OWNS the requested source, only ownership may
// select the slot. A same-named slot belonging to a different source is not
// weaker evidence — it is the wrong column, and no answer is the correct answer.
func TestReanchorDoesNotBindOneSourceToAnothersSlot(t *testing.T) {
	t.Parallel()

	left, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("A"), &RecordType{Fields: []Field{
		{Name: "LID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "VAL", Ordinal: 1, FieldType: NotNullLong},
	}})
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	// B also has VAL, but the producer below carries a DIFFERENT column of B.
	// That is the shape that reaches the fallback: B IS one of the producer's
	// sources, yet no slot of it answers this request, so ownership finds
	// nothing and only A's same-named slot remains.
	right, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("B"), &RecordType{Fields: []Field{
		{Name: "RID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "VAL", Ordinal: 1, FieldType: NotNullLong},
		{Name: "OTHER", Ordinal: 2, FieldType: NotNullLong},
	}})
	if err != nil {
		t.Fatalf("B: %v", err)
	}

	field := func(source QuantifiedObjectValue, ordinal int) Value {
		t.Helper()
		v, resolveErr := ResolveFieldOrdinals(source, []int{ordinal})
		if resolveErr != nil {
			t.Fatalf("field %d: %v", ordinal, resolveErr)
		}
		return v
	}

	// The producer flows [A.LID, A.VAL, B.RID, B.OTHER]. B is one of its
	// sources, but no slot of B answers a request for B.VAL — so ownership
	// yields nothing while A's VAL slot matches by name and type.
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "LID", Value: field(left, 0)},
		RecordConstructorField{Name: "VAL", Value: field(left, 1)},
		RecordConstructorField{Name: "RID", Value: field(right, 0)},
		RecordConstructorField{Name: "OTHER", Value: field(right, 2)},
	)
	producerType, ok := producer.Type().(*RecordType)
	if !ok {
		t.Fatalf("producer type is %T, want *RecordType", producer.Type())
	}
	target, err := NewOrdinalLayoutForCarrierType(producerType,
		[]OrdinalTileSpec{{Start: 0, Width: 4, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("target layout: %v", err)
	}

	// B.VAL — the read whose only NAME match is A's slot.
	reanchored, err := reanchorUnowned(field(right, 1), producer, target.Carrier())
	if err != nil {
		t.Fatalf("reanchor B.VAL: %v", err)
	}
	if fv, isField := AsFieldValue(reanchored); isField {
		if root, isRoot := AsQuantifiedObjectValue(fv.ChildValue()); isRoot &&
			root.Correlation() == target.Carrier().Correlation() {
			got := fv.Path().Ordinals()
			if len(got) == 1 && got[0] == 1 {
				t.Fatal("B.VAL reanchored onto slot 1 — A's column. Two legs' " +
					"same-named columns were matched by name, so both output columns " +
					"read the same source: wrong VALUES, not merely wrong metadata.")
			}
			t.Fatalf("B.VAL reanchored onto carrier slot %v; the producer carries no "+
				"slot of B answering this read, so the only correct outcomes are "+
				"declining (left source-relative) or B's own slot — never another "+
				"source's column", got)
		}
	}
	// Left source-relative is the other acceptable outcome: unresolved is
	// correct-or-loud, and the owner validates it. What must never happen is
	// resolution onto the WRONG slot, which the branch above rejects.
}

// TestReanchorStillBridgesANominallyRenamedSource pins the direction that makes
// the rule narrow rather than a blanket ban, because the blanket version was
// tried and it broke two working shapes.
//
// The name fallback exists for a logical source whose storage record differs
// only in nominal naming; there the candidate legitimately carries a DIFFERENT
// correlation than the request. Refusing every cross-correlation match kills
// that bridge. The rule only withdraws it while the producer owns the requested
// source itself — because then the correct slot is present and ownership finds
// it.
func TestReanchorStillBridgesANominallyRenamedSource(t *testing.T) {
	t.Parallel()

	rowType := &RecordType{Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "VAL", Ordinal: 1, FieldType: NotNullLong},
	}}
	stored, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("STORAGE"), rowType)
	if err != nil {
		t.Fatalf("stored: %v", err)
	}
	logical, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("LOGICAL"), rowType)
	if err != nil {
		t.Fatalf("logical: %v", err)
	}
	field := func(source QuantifiedObjectValue, ordinal int) Value {
		t.Helper()
		v, resolveErr := ResolveFieldOrdinals(source, []int{ordinal})
		if resolveErr != nil {
			t.Fatalf("field: %v", resolveErr)
		}
		return v
	}

	// The producer carries ONLY the storage-named source, so nothing here owns
	// LOGICAL and the nominal bridge is the best evidence available.
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "ID", Value: field(stored, 0)},
		RecordConstructorField{Name: "VAL", Value: field(stored, 1)},
	)
	producerType, ok := producer.Type().(*RecordType)
	if !ok {
		t.Fatalf("producer type is %T, want *RecordType", producer.Type())
	}
	target, err := NewOrdinalLayoutForCarrierType(producerType,
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("target layout: %v", err)
	}

	reanchored, err := reanchorUnowned(field(logical, 1), producer, target.Carrier())
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	fv, isField := AsFieldValue(reanchored)
	if !isField {
		t.Fatalf("reanchored is %T, want a FieldValue", reanchored)
	}
	root, isRoot := AsQuantifiedObjectValue(fv.ChildValue())
	if !isRoot || root.Correlation() != target.Carrier().Correlation() {
		t.Fatal("a nominally-renamed source was NOT bridged onto the producer's " +
			"carrier; the cross-source rule must stay narrow enough to leave this " +
			"working, or it trades one wrong answer for a lost one")
	}
	if got := fv.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("bridged to %v, want [1]", got)
	}
}
