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
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		values.NamedCorrelationIdentifier("L"): {
			Offset: 0, Typ: legType, Alias: values.NamedCorrelationIdentifier("L"),
		},
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("$box"), mergedType)

	// A bare read whose name matches EXACTLY ONE merged column — the shape the
	// retired arm baked. It must survive unchanged so verification decides.
	bare := values.NewFlatFieldValue("UNIQUE_COL", values.UnknownType)
	out, bareOK := rebaseLegRefsToBox(bare, windows, mergedType, boxQOV)
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
	if bareOK {
		t.Fatal("a surviving bare read must DECLINE the wrap. Returning ok=true leaves it " +
			"to a downstream check that only exists on the RESULT-VALUE path " +
			"(wrapRVFullyBaked); on the PREDICATE path nothing looks for a lazy read, " +
			"because the post-walk only hunts QuantifiedObjectValues. Declining sends " +
			"the shape back to the name model, which is where it went before the wrap " +
			"existed.")
	}

	// A DOTTED frontier read ("LEG.COL", no child). Its arm used to slice the
	// qualifier out at the first dot and look the leg window up by that text — the
	// last text-keyed reader of the seed-window namespace, and the reason that
	// namespace was said to need one. It is gone for two reasons and both are
	// load-bearing: keying it by identity would mean minting a
	// CorrelationIdentifier out of a column name, and it was reached by NOTHING
	// (a panic wired into it is hit by no test in ./pkg/relational/... — the
	// sqldriver FDB corpus plus the explaindiff, plandiff, rowdiff, memoinvariant
	// and yamsql harnesses — nor in ./pkg/recordlayer/query/...).
	//
	// So it must neither bake nor pass: it declines, exactly like the bare read.
	dotted := values.NewFlatFieldValue("L.ID", values.UnknownType)
	dottedOut, dottedOK := rebaseLegRefsToBox(dotted, windows, mergedType, boxQOV)
	dottedFV, isDottedFV := dottedOut.(*values.FieldValue)
	if !isDottedFV {
		t.Fatalf("a dotted read must come back a FieldValue, got %T", dottedOut)
	}
	if dottedFV.Resolved != nil {
		t.Fatalf("a dotted frontier read was baked to merged slot #%d by splitting a "+
			"qualifier out of its DISPLAY name and keying the leg windows with the text. "+
			"That is a quantifier's identity decided by a substring of a column name.",
			dottedFV.Resolved.Root().Ordinal)
	}
	if dottedOK {
		t.Fatal("a surviving dotted read must DECLINE the wrap, for the same reason the " +
			"bare read does: on the predicate path nothing downstream is looking for it.")
	}

	// A QOV-shaped leg reference still bakes — the relaxation is not blanket,
	// and it is the arm that HAS a leg to key on, which is what carries every
	// reference that reaches this rewrite in practice.
	legRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("L"), legType),
		"ID", values.UnknownType,
	)
	legOut, legOK := rebaseLegRefsToBox(legRef, windows, mergedType, boxQOV)
	legFV, isLegFV := legOut.(*values.FieldValue)
	if !isLegFV || legFV.Resolved == nil {
		t.Fatalf("a QOV-shaped leg reference must still bake onto the box, got %v", legOut)
	}
	if !legOK {
		t.Fatal("a fully-rebased leg reference must NOT decline — a decline gate that " +
			"fires on the shapes the wrap exists for turns the whole path off silently")
	}
}

// TestRebaseLegRefsToBox_KeysByIdentityNotByFold is the reader half of the
// seed-window identity-keying conversion.
//
// The window lookup used to key on `strings.ToUpper(correlation.Name())`. On the
// corpus that fold agreed with the identity on all 92 lookups this site takes,
// which is exactly why it cannot be the detector: every leg the corpus produces
// here is already upper. The shape that separates them is a MACHINE-MINTED leg,
// whose correlation is lowercase by construction (UniqueCorrelationIdentifier)
// — fold it and the lookup misses the window filed under it, the reference is
// never rebased, and the wrap ships a correlation the box row cannot bind.
func TestRebaseLegRefsToBox_KeysByIdentityNotByFold(t *testing.T) {
	t.Parallel()

	machine := values.NamedCorrelationIdentifier("q$7") // a planner mint: lowercase
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
	}}
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
	}}
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		machine: {Offset: 0, Typ: legType, Alias: machine},
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("$box"), mergedType)

	ref := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(machine, legType), "ID", values.UnknownType)
	out, ok := rebaseLegRefsToBox(ref, windows, mergedType, boxQOV)
	fv, isFV := out.(*values.FieldValue)
	if !isFV || fv.Resolved == nil || !ok {
		t.Fatalf("a reference over a MACHINE-MINTED leg was not rebased (ok=%v, got %v). "+
			"Its correlation is lowercase, so a lookup that upper-folds before keying "+
			"misses the window filed under it — silently, because a miss here is "+
			"indistinguishable from a non-leg reference.", ok, out)
	}
	if got := fv.Resolved.Root().Ordinal; got != 0 {
		t.Fatalf("rebased to merged slot %d, want 0", got)
	}
}
