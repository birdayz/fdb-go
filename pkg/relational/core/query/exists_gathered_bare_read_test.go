package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A BARE frontier read ("COL", no leg, no ordinal) must NOT be baked onto a slot
// of the merged row.
//
// rebaseLegRefsToBox used to match such a read's DISPLAY name against the merged
// row's field names and bake on a unique hit, with a `matches != 1` guard as the
// mitigation. The guard is the admission: where the merged row has two
// same-named columns the name cannot answer, and where it has one the ordinal
// answers identically — so both went together (RFC-197 item 3), and the read is
// left for the caller's post-walk verification to decline.
//
// The arm was dead in production before it was removed: a panic wired into its
// unique-match point is reached by nothing — not the explaindiff corpus, not the
// //pkg/relational/sqldriver FDB suite, not any conformance harness. This test
// is what keeps that from being re-derived, and it is unit-level because no
// query reaches the arm to express it as SQL.
func TestRebaseLegRefsToBox_BareReadIsNotBakedByName(t *testing.T) {
	t.Parallel()

	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
	}}
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "UNIQUE_COL", FieldType: values.UnknownType, Ordinal: 1},
	}}
	windows := map[string]values.OrdinalSeedLegWindow{
		"L": {Offset: 0, Typ: legType},
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("$box"), mergedType)

	// A bare read whose name matches EXACTLY ONE merged column — the shape the
	// retired arm baked. It must survive unchanged so verification decides.
	bare := values.NewFlatFieldValue("UNIQUE_COL", values.UnknownType)
	out, _ := rebaseLegRefsToBox(bare, windows, mergedType, boxQOV)
	fv, isFV := out.(*values.FieldValue)
	if !isFV {
		t.Fatalf("a bare read must come back a FieldValue, got %T", out)
	}
	if fv.Resolved != nil {
		t.Fatalf("a bare frontier read was baked to merged slot #%d by matching its DISPLAY "+
			"name against the merged row's column list: a bare read names no leg, so nothing "+
			"says which column it means (RFC-197 item 3)", fv.Resolved.Root().Ordinal)
	}
	if fv != bare {
		t.Fatalf("a bare read must come back unchanged, got %v", fv)
	}

	// A QOV-shaped leg reference still bakes — the relaxation is not blanket,
	// and the arms that HAVE a leg to key on are what carry every reference
	// that reaches this rewrite in practice.
	legRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("L"), legType),
		"ID", values.UnknownType,
	)
	legOut, _ := rebaseLegRefsToBox(legRef, windows, mergedType, boxQOV)
	legFV, isLegFV := legOut.(*values.FieldValue)
	if !isLegFV || legFV.Resolved == nil {
		t.Fatalf("a QOV-shaped leg reference must still bake onto the box, got %v", legOut)
	}
}
