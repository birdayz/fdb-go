package executor

// WHICH CHANNEL carries the leg type adaptLegPositional adapts against, measured
// rather than reasoned about — because the obvious answer is wrong and a
// conversion aimed at it would miss the live path entirely.
//
// The obvious answer: the seed is a RecordConstructorValue, so the leg type must
// be RecordConstructorValue.Type(), which builds `&RecordType{Nullable: true,
// Fields: fields}` from the RC field NAMES and no leg table. Restore a leg table
// there and the reader gains an identity to select by.
//
// The live path does not go through it. `ordinalJoinBuild.legType` consults
// `b.Spans` FIRST whenever WindowsOK, and the spans come from `resolveSpanLeaf`,
// which takes the leg type from the baked reference's `qov.Type()` — the
// quantifier's OWN flowed type, not the RC field label. So the RC field name
// never becomes a leg-type column name on this path, and a leg table attached to
// the RC's derived type would not be read by this reader at all.
//
// That matters because it relocates the whole question. The dotted names that DO
// reach the reader arrive as `qov.Type()` field names, which means a producer put
// them into a quantifier's flowed type — and the correlated-scalar seed does
// exactly that: the inner leg is `RECORD<scalarCol>` where scalarCol is
// `csq.ScalarCol`, the subquery's OUTPUT LABEL. Measured span tables from
// TestFDB_CorrelatedScalarJoinInner:
//
//	spans=["C"@0+2:[ID NAME]  "q$204"@2+1:[I.QTY]]
//	spans=["C"@0+2:[ID NAME]  "q$223"@2+1:[SUM(QTY)]]
//	spans=["C"@0+2:[ID NAME]  "q$80"@2+1:[COUNT(*)]]
//	spans=["C"@0+2:[ID NAME]  "q$726"@2+1:[NAME]]
//
// One channel, four labels. `SUM(QTY)` and `COUNT(*)` are plainly titles;
// `I.QTY` is a title that happens to contain a dot, and it is the only one the
// dotted arm answers. So the four "dotted hits" are not leg-qualified references
// with an identity waiting to be keyed on — they are column titles, and there is
// no leg being referenced for an identity to name.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSpanLegType_ComesFromTheQuantifierTypeNotTheFieldLabel pins the relocation.
//
// The RC field names are deliberately made to DISAGREE with the quantifier's own
// column names. If the span's leg type followed the labels, the conversion target
// would be the RC's derived type; it follows the quantifier, so it is not.
func TestSpanLegType_ComesFromTheQuantifierTypeNotTheFieldLabel(t *testing.T) {
	t.Parallel()

	legA := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	legB := &values.RecordType{Fields: []values.Field{{Name: "W", FieldType: values.NotNullLong, Ordinal: 0}}}
	qovA := mustTestQOV(t, values.NamedCorrelationIdentifier("A"), legA)
	qovB := mustTestQOV(t, values.NamedCorrelationIdentifier("B"), legB)

	// Labels that differ from every column name the quantifiers declare.
	mk := func(qov values.QuantifiedObjectValue, ord int, label string) values.RecordConstructorField {
		fv := mustExecutorConstruct(values.ResolveOrdinalSeedField(qov, ord))
		return values.RecordConstructorField{Name: label, Value: fv}
	}
	rc := values.NewRawRecordConstructorValue(
		mk(qovA, 0, "A.ID"), mk(qovA, 1, "LABEL_NOT_A_COLUMN"), mk(qovB, 0, "SUM(B.W)"),
	)

	spans, _, ok := ordinalJoinSpansOf(rc, nil)
	if !ok {
		t.Fatal("the seed did not derive spans — the fixture stopped being a leg concat")
	}
	if len(spans) != 2 {
		t.Fatalf("derived %d spans, want 2 (one per quantifier)", len(spans))
	}

	// THE POINT: the leg types carry the QUANTIFIERS' column names, not the
	// labels above.
	if got := typeFieldNames(spans[0].LegType); got[0] != "ID" || got[1] != "V" {
		t.Fatalf("span A's leg type is %v, want [ID V] — the RC labels were "+
			"[A.ID LABEL_NOT_A_COLUMN], so if the leg type followed the LABEL the "+
			"conversion target would be RecordConstructorValue.Type(). It follows "+
			"qov.Type() (resolveSpanLeaf), which is a different channel and a "+
			"different producer.", got)
	}
	if got := typeFieldNames(spans[1].LegType); len(got) != 1 || got[0] != "W" {
		t.Fatalf("span B's leg type is %v, want [W] — a rendered label %q reached the "+
			"leg-type channel, which is the failure this test exists to detect",
			got, "SUM(B.W)")
	}
}

// TestSpanLegType_ALabelInTheQUANTIFIERTypeDoesReachTheReader is the other half,
// and it is the shape the corpus actually produces.
//
// The label does not reach the leg type via the RC field name — but it DOES reach
// it when a producer names a quantifier's own flowed column with a label. The
// correlated-scalar seed builds its inner leg as RECORD<csq.ScalarCol>, so the
// subquery's output title becomes a leg-type column name, and a title containing
// a dot then reaches rowSlotForLegColumn's dotted arm indistinguishable from a
// leg-qualified reference.
//
// This is the entry point any retirement of that arm has to close, and it is a
// producer that names a TYPE by a LABEL — not a reader that reads a name.
func TestSpanLegType_ALabelInTheQUANTIFIERTypeDoesReachTheReader(t *testing.T) {
	t.Parallel()

	outer := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NAME", FieldType: values.NotNullString, Ordinal: 1},
	}}
	// The correlated-scalar seed's inner leg, as the producer builds it: ONE
	// field named by the subquery's output label.
	innerLabelled := &values.RecordType{Fields: []values.Field{{Name: "I.QTY", FieldType: values.NotNullLong, Ordinal: 0}}}

	qovOuter := mustTestQOV(t, values.NamedCorrelationIdentifier("C"), outer)
	qovInner := mustTestQOV(t, values.NamedCorrelationIdentifier("q$204"), innerLabelled)

	mk := func(qov values.QuantifiedObjectValue, ord int) values.RecordConstructorField {
		fv := mustExecutorConstruct(values.ResolveOrdinalSeedField(qov, ord))
		view, ok := values.AsFieldValue(fv)
		if !ok {
			t.Fatalf("ResolveOrdinalSeedField returned %T, want exact FieldValue", fv)
		}
		return values.RecordConstructorField{Name: view.DisplayName(), Value: fv}
	}
	rc := values.NewRawRecordConstructorValue(mk(qovOuter, 0), mk(qovOuter, 1), mk(qovInner, 0))

	spans, _, ok := ordinalJoinSpansOf(rc, nil)
	if !ok {
		t.Fatal("the seed did not derive spans")
	}
	if len(spans) != 2 {
		t.Fatalf("derived %d spans, want 2", len(spans))
	}
	got := typeFieldNames(spans[1].LegType)
	if len(got) != 1 || got[0] != "I.QTY" {
		t.Fatalf("the inner scalar leg type is %v, want [I.QTY].\n"+
			"  This fixture reproduces the measured corpus span "+
			"`\"q$204\"@2+1:[I.QTY]`. If it no longer holds, the producer has "+
			"stopped naming the inner leg's flowed column with the subquery's "+
			"OUTPUT LABEL — which is exactly the change that stops rendered titles "+
			"reaching rowSlotForLegColumn's dotted arm. Re-measure the leg-column "+
			"provenance census before assuming the arm still has a population.", got)
	}

	// And the title is indistinguishable from a reference AT THE READER: it has a
	// dot, so the arm splits it, and the qualifier `I` will name a leg of any
	// source row that happens to carry one.
	merged := &values.RecordType{
		Fields: []values.Field{
			{Name: "ID", Ordinal: 0},
			{Name: "CUSTOMER_ID", Ordinal: 1},
			{Name: "ID", Ordinal: 2},
			{Name: "ORDER_ID", Ordinal: 3},
			{Name: "QTY", Ordinal: 4},
		},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("O"), "O", 0, 2),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("I"), "I", 2, 3),
		},
	}
	slot, resolved := rowSlotForLegColumn(merged, "I.QTY", values.CorrelationIdentifier{})
	if resolved != legColumnBound || slot != 4 {
		t.Fatalf("the OUTPUT LABEL %q resolved to (%d,%v), want (4,true) — this is not a "+
			"claim that resolving it is correct, it is the pin that it HAPPENS, so the "+
			"day a conversion changes it the change is visible", "I.QTY", slot, resolved)
	}
}
