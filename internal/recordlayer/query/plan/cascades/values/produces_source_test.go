package values

import "testing"

// TestProducesSourceSeesASourceBuriedInAnExpression pins the dimension that was
// unprobed: a producer slot that reads a source through an EXPRESSION rather
// than at the top level.
//
// producesSource is the discriminator between "this producer has no opinion
// about the requested source, so a nominally-renamed candidate is the best
// available evidence" and "this producer owns that source, so the correct slot
// is here and ownership is what finds it". Reading only the top level answered
// "no opinion" for a slot spelled `10 + A.ID`, which hands the decision back to
// the cross-source NAME path for a source the producer demonstrably reads —
// the same wrong-column shape the discriminator exists to prevent, reached
// through an expression instead of directly.
//
// Both directions are asserted. An arm that wrongly reports true would suppress
// the name path where it is the only available evidence; an arm that wrongly
// reports false is the wrong-column bug.
func TestProducesSourceSeesASourceBuriedInAnExpression(t *testing.T) {
	t.Parallel()

	row := &RecordType{Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
	}}
	a := mustTypedQOV(t, "A", row)
	b := mustTypedQOV(t, "B", row)

	aID, err := ResolveFieldOrdinals(a, []int{0})
	if err != nil {
		t.Fatalf("A.ID: %v", err)
	}
	konst := func(n int64) Value { return &ConstantValue{Value: n} }
	shallow := &ArithmeticValue{Op: OpAdd, Left: aID, Right: konst(10)}
	deep := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ArithmeticValue{Op: OpAdd, Left: aID, Right: konst(1)},
		Right: konst(2),
	}

	for _, c := range []struct {
		what     string
		slot     Value
		carriesA bool
	}{
		{"a bare source", a, true},
		{"a top-level field read", aID, true},
		{"a source under one arithmetic node — the arm that was blind", shallow, true},
		{"a source nested two levels down", deep, true},
		{"a slot reading a DIFFERENT source", b, false},
		{"a slot reading no source at all", konst(7), false},
	} {
		rc := NewRecordConstructorValue(RecordConstructorField{Name: "OUT", Value: c.slot})
		if got := producesSource(rc, a.Correlation()); got != c.carriesA {
			t.Errorf("producesSource(A) over %s = %v, want %v.\n"+
				"  false here hands the slot decision back to the cross-source NAME path "+
				"for a source this producer demonstrably reads; true where the producer "+
				"does NOT read it suppresses the name path where it is the only evidence.",
				c.what, got, c.carriesA)
		}
	}

	// B must stay unowned by a producer that only reads A, or the walk is
	// answering true regardless and every arm above passed for the wrong reason.
	onlyA := NewRecordConstructorValue(RecordConstructorField{Name: "OUT", Value: deep})
	if producesSource(onlyA, b.Correlation()) {
		t.Error("a producer reading only A reported that it carries B; the walk is answering " +
			"true regardless of the correlation asked about")
	}
}
