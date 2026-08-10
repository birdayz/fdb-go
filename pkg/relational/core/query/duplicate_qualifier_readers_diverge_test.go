package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TWO READERS OF ONE CHANNEL DISAGREE ABOUT A DUPLICATE QUALIFIER, and this
// pins that disagreement as a checked fact rather than a claim in a comment.
//
// When a qualifier names two legs, the translator's two leg-resolving readers
// take OPPOSITE dispositions:
//
//   - bakeDottedRefsToLegQOVWithRef's addKey POISONS the key (`layouts[key] =
//     nil`) and refuses to bake through it, so the read stays lazy and is loud
//     at evaluation.
//   - legWindowSlot takes the FIRST leg whose Name matches and resolves.
//
// WHY THIS TEST EXISTS RATHER THAN A LEDGER ENTRY. Two entries for
// legWindowSlot used to sit on the `.Field`-decides ratchet
// (pkg/docscheck/field_name_decision_test.go). They came off it when the
// first-dot re-split was removed, and the reason is mechanical and correct:
// that ratchet tracks decisions made from a *values.FieldValue's Field — a
// DISPLAY name — and the detector taints a callee parameter only when the
// argument derives from one (its escapesFieldName predicate). legWindowSlot's
// surviving production caller is bakeSegmentedColumnRef, which passes
// ref.Qualifier and ref.Bare: the PARSER's segments, not a rendered name. So
// the comparison is no longer a `.Field` decision and the detector reports "no
// decision found" for those keys. Re-adding them turns that gate red.
//
// The RISK, though, did not retire with the caller — and this is the point the
// mechanical retirement obscures. legWindowSlot still first-matches on a
// QUALIFIER, and that first-match has no upstream backing: SQLSTATE 42702 is
// ambiguous_column, produced by semantic.Scope.ResolveColumn's terminal
// AmbiguousColumnError (semantic/scope.go:271-274) for an ambiguous COLUMN
// reference, and it says nothing about two legs sharing a qualifier. No
// rejection of a repeated FROM-clause alias was found either — every
// ErrCodeDuplicateAlias (42712) producer in pkg/ is CTE-name duplication.
//
// So the risk is tracked in the two places that can actually hold it: the
// LEG-IDENTITY CENSUS, which legWindowSlot still records to
// (values.RecordDottedLegQualifier, live at the site), and this behavioural
// pin. A test is strictly stronger than a ledger line: it fails when the
// behaviour moves, which a note never does.
//
// WHAT CLOSES IT: matching the leg by IDENTITY rather than by Name, at which
// point neither first-match exists and the two readers cannot disagree. Until
// then, changing EITHER disposition should fail here and be a decision.
func TestDuplicateQualifier_ReadersDiverge(t *testing.T) {
	t.Parallel()

	// Two ForEach legs both bound to the SOURCE ALIAS "D". This is the shape no
	// upstream check was found to reject. twoLegSelect cannot express it (its
	// aliases are L and R), so the select is built here.
	lq := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("L"), expressions.InitialOf(legSelect("ID", "NAME")))
	rq := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("R"), expressions.InitialOf(legSelect("NAME", "ID")))
	outer := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		[]expressions.Quantifier{lq, rq}, nil, []string{"D", "D"})

	t.Run("legQOV baker POISONS the duplicate and declines", func(t *testing.T) {
		t.Parallel()
		ref := logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "D", Qualified: true}
		out := bakeDottedRefsToLegQOVWithRef(
			values.NewFlatFieldValue("D.NAME", values.UnknownType), ref, outer)
		fv, isFV := out.(*values.FieldValue)
		if !isFV || fv.Resolved != nil {
			t.Fatalf("the leg-QOV baker RESOLVED a qualifier carried by two legs, to %#v.\n\n"+
				"addKey poisons a duplicate key (layouts[key] = nil) precisely so an "+
				"ambiguous qualifier is never baked through — a read that cannot be "+
				"attributed to one leg must stay lazy and go loud at evaluation, never "+
				"pick one. If this disposition changed deliberately, it now AGREES with "+
				"legWindowSlot and the divergence below should have been closed too.", out)
		}
		// CONTROL: the same baker, the same shape, a UNIQUE qualifier — must
		// RESOLVE. Without this the decline above would also pass if the baker
		// were inert, if the legs carried no columns, or if the fixture never
		// registered a layout at all.
		uniq := expressions.NewSelectExpressionWithAliases(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
			[]expressions.Quantifier{lq, rq}, nil, []string{"D", "E"})
		got := bakeDottedRefsToLegQOVWithRef(
			values.NewFlatFieldValue("D.NAME", values.UnknownType), ref, uniq)
		if cfv, isFV := got.(*values.FieldValue); !isFV || cfv.Resolved == nil {
			t.Fatalf("control: a UNIQUE qualifier D failed to bake (%#v). The decline above "+
				"is then the baker being inert rather than the duplicate being poisoned, "+
				"and this test proves nothing", got)
		}
	})

	t.Run("legWindowSlot FIRST-MATCHES the duplicate", func(t *testing.T) {
		t.Parallel()
		// A flat row of two 2-column leg windows, both legs named "D".
		cols := []string{"ID", "NAME", "NAME", "ID"}
		legs := []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("L"), "D", 0, 2),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("R"), "D", 2, 2),
		}
		k, found := legWindowSlot("D", "NAME", cols, legs, values.DottedLegSiteFlatColumnBake)
		if !found {
			t.Fatalf("legWindowSlot declined a duplicate qualifier (found=%v). That would be "+
				"the RECONCILED behaviour — matching the leg-QOV baker's poisoning — and it "+
				"is the outcome this file exists to make a deliberate change rather than a "+
				"silent one. Update the divergence note above and in "+
				"pkg/docscheck/field_name_decision_test.go's translator bucket.", found)
		}
		if k != 1 {
			t.Fatalf("legWindowSlot resolved D.NAME to flat slot %d, want 1 — the FIRST leg "+
				"named D. This pins WHICH leg the first-match picks, so a reordering or a "+
				"switch to last-match is visible rather than silent.", k)
		}
		// CONTROL: the second leg genuinely holds a different column at that
		// name, so the first-match above is a real choice between two
		// candidates rather than an artefact of both legs being identical.
		if cols[1] == cols[3] {
			t.Fatal("control: the two legs' NAME columns landed on the same flat slot, so " +
				"the first-match had nothing to choose between and the assertion above is vacuous")
		}
	})
}
