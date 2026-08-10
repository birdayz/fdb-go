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
			Kind:   values.LegKindFlatRun,
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
// corpus that fold agreed with the identity on all 92 lookups this site took —
// a dated point measurement, reproduced since by the STANDING seed-window reader
// census, which reports the same 92 at boxLegRef and floors it so the site
// cannot go dark unnoticed. That agreement is exactly why the corpus cannot be
// the detector: every leg it produces here is already upper. The shape that
// separates them is a MACHINE-MINTED leg,
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
		machine: {Kind: values.LegKindFlatRun, Offset: 0, Typ: legType, Alias: machine},
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

// TestRebaseLegRefsToBox_ChildlessSourceRelativeBakeDeclines pins the OTHER
// childless flavor: a SOURCE-RELATIVE BAKED read with no child.
//
// The walk's entry guard admits such a node deliberately — `fv.Resolved != nil
// && !fv.SourceRelativeBaked()` is what returns early, so a source-relative bake
// falls THROUGH to be rebased, exactly like its lazy twin. But rebasing needs a
// correlation to select a window with, and a CHILDLESS node has none: the
// QOV-shaped arm never sees it and it reaches the childless tail untouched.
//
// The decline predicate there used to be `fv.Resolved == nil`, which is true for
// the lazy flavor and FALSE for this one. So this node passed through with its
// ordinal intact and ok=true — and that ordinal is LEG-RELATIVE. Against the box
// row it addresses whatever column happens to sit at that slot of the merged
// concat: not a fail-open to the name model, but a silently different column.
//
// The layout below makes the two readings DISTINGUISHABLE, which is the whole
// requirement on the fixture: leg L starts at merged offset 2, so a leg-relative
// ordinal 0 and the merged ordinal it should mean (2) are different numbers. A
// leg at offset 0 would make the bug invisible — the wrong answer and the right
// one would coincide.
//
// The sibling check three functions down (wrapRVFullyBaked) has always used the
// correct predicate for this same question. The asymmetry was the defect.
func TestRebaseLegRefsToBox_ChildlessSourceRelativeBakeDeclines(t *testing.T) {
	t.Parallel()

	legType := &values.RecordType{Fields: []values.Field{
		{Name: "V", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "W", FieldType: values.UnknownType, Ordinal: 1},
	}}
	// L occupies merged slots [2,4) — NOT [0,2) — so leg-relative 0 and merged 2
	// are distinguishable.
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "A0", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "A1", FieldType: values.UnknownType, Ordinal: 1},
		{Name: "V", FieldType: values.UnknownType, Ordinal: 2},
		{Name: "W", FieldType: values.UnknownType, Ordinal: 3},
	}}
	legL := values.NamedCorrelationIdentifier("L")
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		legL: {Kind: values.LegKindFlatRun, Offset: 2, Typ: legType, Alias: legL},
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("$box"), mergedType)

	// CONTROL: the QOV-shaped spelling of this very reference rebases to merged
	// slot 2 and does NOT decline. Without this arm the assertion below would
	// pass just as well against a wrap that declines everything.
	qovRef := values.NewFieldValue(values.NewQuantifiedObjectValue(legL), "V", values.UnknownType)
	qovOut, qovOK := rebaseLegRefsToBox(qovRef, windows, mergedType, boxQOV)
	qovFV, isFV := qovOut.(*values.FieldValue)
	if !isFV || qovFV.Resolved == nil {
		t.Fatalf("control: the QOV-shaped leg reference must rebase, got %v", qovOut)
	}
	if got := qovFV.Resolved.Root().Ordinal; got != 2 {
		t.Fatalf("control: QOV-shaped L.V rebased to merged slot %d, want 2", got)
	}
	if !qovOK {
		t.Fatal("control: a fully-rebased leg reference must not decline")
	}

	// The probe: the SAME column, spelled as a childless source-relative bake at
	// its LEG-relative ordinal 0.
	childlessBaked := values.NewFieldValueWithResolvedOrdinal("V", 0, values.UnknownType)
	if !childlessBaked.SourceRelativeBaked() {
		t.Fatal("fixture: the probe node must be SourceRelativeBaked, or it never enters " +
			"the walk and this test proves nothing")
	}
	if childlessBaked.Child != nil {
		t.Fatal("fixture: the probe node must be CHILDLESS, or it takes the QOV arm")
	}
	out, ok := rebaseLegRefsToBox(childlessBaked, windows, mergedType, boxQOV)
	if ok {
		got := -1
		if fv, isFV := out.(*values.FieldValue); isFV && fv.Resolved != nil {
			got = fv.Resolved.Root().Ordinal
		}
		t.Fatalf("a CHILDLESS source-relative baked read returned ok=true with ordinal %d; "+
			"want a DECLINE (or a rebase to 2).\n"+
			"  The walk admits this node so it can be REBASED, but it carries no correlation,\n"+
			"  so no window can be selected for it and the QOV arm never sees it. Passing it\n"+
			"  through ships a LEG-RELATIVE ordinal against the BOX row: slot %d of the merged\n"+
			"  concat is %q, while the reference names leg L's %q at merged slot 2.\n"+
			"  Gate the decline on CHILDLESS-NESS, not on `Resolved == nil` — that predicate\n"+
			"  is true only for the lazy flavor, and wrapRVFullyBaked already gets it right.",
			got, got, mergedType.Fields[0].Name, "V")
	}
}

// TestRebaseLegRefsToBox_DupNamedBoxWindowFirstMatches holds the shape behind
// the wrap's one recorded FieldIndex blind-spot entry.
//
// The rebase resolves a column by NAME inside the window the reference's own
// correlation selected. The identity fixes WHICH row, which is the half the two
// deleted text arms got wrong — but it does not make the window unambiguous. A
// CLUSTERED BOX run window concatenates every buried leaf's columns, so two
// leaves' same-named columns share one window and FieldIndex first-matches
// between them.
//
// Measured: the ambiguous case is UNREACHED — a panic wired at this call on "the
// selected window holds more than one field named fv.Field" is hit by nothing
// over the real-FDB sqldriver corpus or ./pkg/relational/core/... . This test
// exists because that is a negative result, and a negative result recorded only
// in prose is one nobody re-checks. It pins the shape so the debt entry names a
// real hazard, and it pins the CURRENT answer so a guard cannot be added
// silently.
//
// The sibling reader already has the fix: left_outer_existential.go's
// leg-relative arm carries the reference's already-BAKED ordinal precisely so an
// opaque box leg's duplicate buried names cannot remap it. When a reference
// arrives here carrying its leg-local ordinal too, this site stops resolving by
// name and the blind-spot entry retires.
func TestRebaseLegRefsToBox_DupNamedBoxWindowFirstMatches(t *testing.T) {
	t.Parallel()

	// A clustered BOX run window: two buried leaves, both carrying `K`, filed as
	// ONE window under the box quantifier's own correlation.
	boxRun := &values.RecordType{Fields: []values.Field{
		{Name: "K", FieldType: values.UnknownType, Ordinal: 0}, // leaf A's K
		{Name: "K", FieldType: values.UnknownType, Ordinal: 1}, // leaf B's K
	}}
	boxLeg := values.NamedCorrelationIdentifier("C$BOX")
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "PAD", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "K", FieldType: values.UnknownType, Ordinal: 1},
		{Name: "K", FieldType: values.UnknownType, Ordinal: 2},
	}}
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		boxLeg: {Kind: values.LegKindFlatRun, Offset: 1, Typ: boxRun, Alias: boxLeg},
	}
	boxQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("$box"), mergedType)

	ref := values.NewFieldValue(values.NewQuantifiedObjectValue(boxLeg), "K", values.UnknownType)
	out, ok := rebaseLegRefsToBox(ref, windows, mergedType, boxQOV)
	if ok {
		fv, isFV := out.(*values.FieldValue)
		slot := -1
		if isFV && fv.Resolved != nil {
			slot = fv.Resolved.Root().Ordinal
		}
		t.Fatalf("a dup-named box-run window resolved `K` to merged slot %d — it must "+
			"DECLINE.\n"+
			"  Two buried leaves both carry `K` in one window, so no answer here is "+
			"distinguishable from a guess: slot 1 and slot 2 are both real merged "+
			"columns of the same type, and nothing downstream can reject the wrong one.\n"+
			"  The name lookup this site used to make was deleted outright; if it "+
			"resolves again, a first-match scan came back.", slot)
	}
	// The bare-read blind spot is CLOSED, and this test now guards the closure
	// rather than the hazard. What made the old answer look reasonable was that
	// the resolution stayed inside the window (offset 1) instead of scanning the
	// merged concat from 0 — a correct DOMAIN with an arbitrary choice inside it.
	// A correct domain is not a correct answer.
}
