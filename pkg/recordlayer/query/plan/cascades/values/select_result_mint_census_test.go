package values

import "testing"

// The mint census must be able to tell a typed select result value from an
// untyped one, and must spell WHICH type.
//
// This is the dimension the FlatMap producer census could not supply. That
// census attributes a value to the construction that handed it to a plan, and
// three of its sites merely flow sel.GetResultValue() verbatim — the same shape
// Java's three RecordQueryFlatMapPlan constructions have
// (ImplementNestedLoopJoinRule.java:187,201,214). Counting a courier's traffic
// as production is what booked this divergence against the wrong sites.
func TestSelectResultMintCensus_SeparatesTypedFromUntyped(t *testing.T) {
	t.Parallel()

	corr := NamedCorrelationIdentifier("A")
	rowType := &RecordType{Fields: []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "K", FieldType: NotNullLong, Ordinal: 1},
	}}

	for _, tc := range []struct {
		name  string
		qov   *quantifiedObjectValue
		typed bool
		spell string
	}{
		{
			// What Java always builds: overQuantifier.getFlowedObjectValue().
			name:  "a typed mint reports its ARITY",
			qov:   mustQOV(t, corr, rowType),
			typed: true,
			spell: "RecordType(2)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := quantifiedObjectValueCarriesAType(tc.qov); got != tc.typed {
				t.Fatalf("carriesAType = %t, want %t. This is the cut the whole divergence "+
					"is denominated in; a spelling that cannot make it reports the gap as "+
					"closed while every producer still stands", got, tc.typed)
			}
			if got := describeMintedQOVType(tc.qov); got != tc.spell {
				t.Fatalf("type spelling = %q, want %q. A boolean is equally compatible with "+
					"a real row and with a scalar; the arity is the fact", got, tc.spell)
			}
		})
	}
}
