package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TWO NEGATIVE RESULTS FROM THE REFUTED RFC-212 §1.1 ATTEMPT, PINNED BECAUSE
// THEY OUTLIVE THE TARGET THAT WAS WRONG.
//
// §1.1 proposed attaching a leg table to the correlated-scalar seed's record
// constructor and propagating it through RecordConstructorValue.Type(). It was
// built and measured INERT — the executor's dotted-hit count did not move — and
// reverted, because the reader takes `qov.Type()` and not the constructor's
// derived type. Neither fact below depended on that design, and both would have
// to be re-derived by anyone implementing the corrected one.

// THE `leg.start` HAZARD IS UNCONSTRUCTABLE, AND THIS IS WHY.
//
// clusteredOuterOrdinalSeed reads the SOURCE concat (pu.concatType) at
// leg.start+i while writing a NEW row whose fields it appends in order. Those
// are two coordinate systems, so a window emitted from leg.start looked like a
// silently wrong window — a wrong slot, hence a wrong row rather than a worse
// plan. That concern was promoted to a load-bearing finding and it is WRONG.
//
// values.AssertOrdinalJoinSeed groups the seed's baked references by the QOV
// CORRELATION they read, and every outer leg bakes over the same outerQOV — so
// the whole outer block is ONE run whose ordinals must be exactly 0..N-1
// ascending, no gaps, no reorders. The value at output position k is leg.start+i,
// so the contract forces leg.start+i == k. The two offsets are provably equal.
//
// Measured, not argued: a fixture whose legs start at 3 in the source does not
// fail an assertion, it PANICS one line after the seed is assembled —
//
//	panic: ordinal join seed malformed: leg OUTER run starts at field 0 with
//	baked ordinal 3, want 0 — run ordinals must be exactly 0..width-1 ascending
//
// This test is that fixture. It is the only thing standing between the seed
// contract and a class of wrong-row bug that is currently impossible.
func TestClusteredOuterSeed_NonContiguousLegsAreRejectedAtTheSeed(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a seed whose legs start at 3 in the SOURCE concat was ACCEPTED.\n" +
				"  WHAT THIS RE-ARMS: values.AssertOrdinalJoinSeed's ordinal-run contract —\n" +
				"  that a leg run's baked ordinals are exactly 0..width-1 ascending — is what\n" +
				"  forces leg.start+i to equal the position in the row being built. Every\n" +
				"  consumer that reads a leg window off this seed relies on those two numbers\n" +
				"  being the same, and nothing else checks it.\n" +
				"  With the contract relaxed, any code that emits a window from leg.start\n" +
				"  emits a window into a different row: a wrong slot, so a different column's\n" +
				"  value returned under the right column's name. Re-derive every offset\n" +
				"  expression in clusteredOuterOrdinalSeed before relying on either.")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "run ordinals must be exactly") {
			t.Fatalf("the seed was rejected, but NOT by the ordinal-run contract: %v.\n"+
				"  The reasoning above rests on that contract specifically. A different\n"+
				"  rejection may not survive a change to it, so the equality of the two\n"+
				"  offsets would no longer be guaranteed by what this test observes.", r)
		}
	}()

	legA := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "CV", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	legB := &values.RecordType{Fields: []values.Field{
		{Name: "QTY", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	const pad = 3
	var srcFields []values.Field
	for i := 0; i < pad; i++ {
		srcFields = append(srcFields, values.Field{
			Name: "PAD", FieldType: values.NotNullLong, Ordinal: len(srcFields),
		})
	}
	for _, n := range []string{"ID", "CV", "QTY"} {
		srcFields = append(srcFields, values.Field{
			Name: n, FieldType: values.NotNullLong, Ordinal: len(srcFields),
		})
	}
	pu := &clusterPullUp{
		outerCorr:  values.NamedCorrelationIdentifier("OUTER"),
		concatType: &values.RecordType{Fields: srcFields},
		legs: []clusterLegSpan{
			{binding: "C", start: pad, typ: legA},
			{binding: "I", start: pad + 2, typ: legB},
		},
	}
	_ = clusteredOuterOrdinalSeed(pu, values.NamedCorrelationIdentifier("q$77"), "S", "SUMV")
}

// PROPAGATION, NEVER INFERENCE — the line the corrected §1.1 must not cross
// either, whatever channel it targets.
//
// RecordConstructorValue.Type() could recognise a leg concat from its field
// NAMES: the dotted `LEG.COL` labels the seed mints are visibly one. It must not.
// Measured over the real-FDB sqldriver corpus, Type() runs ~158,000 times and
// several hundred of those derivations describe a dotted-shaped row for reasons
// that have nothing to do with leg windows — aggregate titles like
// `O.SUM(AMOUNT)`, qualified projections, `[ID NAME O._0]`. Inference would
// attach a table to all of them; and a leg table on a row no producer described
// is a refineRowTypes conflict against every sibling that derived it without one.
func TestRecordConstructorType_NeverInfersLegsFromDottedNames(t *testing.T) {
	t.Parallel()
	rc := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{
			Name: "C.CV", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		},
		values.RecordConstructorField{
			Name: "I.QTY", Value: &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong},
		},
		// The doubled qualifier the producer census witnessed as `[TID K C.C.CV]`,
		// which is a title that was already qualified being qualified again.
		values.RecordConstructorField{
			Name: "C.C.CV", Value: &values.ConstantValue{Value: int64(3), Typ: values.NotNullLong},
		},
	)
	rt, ok := rc.Type().(*values.RecordType)
	if !ok {
		t.Fatalf("type is %T, want *values.RecordType", rc.Type())
	}
	if len(rt.Legs) != 0 {
		t.Fatalf("a constructor stating NO leg table derived %d leg(s) from its field "+
			"NAMES.\n"+
			"  That is INFERENCE, and it is the one shape this design refuses regardless\n"+
			"  of which channel the retirement targets.\n"+
			"  WHAT THIS RE-ARMS: a leg table on every dotted-shaped row in the corpus,\n"+
			"  including the several hundred whose dots are aggregate titles rather than\n"+
			"  qualifiers — and with it a refineRowTypes conflict against every sibling\n"+
			"  that derived the same row without one, plus the four readers that DECLINE\n"+
			"  their layout when Legs becomes non-empty (ordinal_join.go:234 and :187,\n"+
			"  ordinal_seed_layout.go:391 and :528).", len(rt.Legs))
	}
}
