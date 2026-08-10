package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// BOTH READERS OF ONE CHANNEL DECLINE AN AMBIGUOUS QUALIFIER, and this asserts
// that agreement.
//
// When a qualifier names two legs, the translator's two leg-resolving readers
// now take the SAME disposition — refuse:
//
//   - bakeDottedRefsToLegQOVWithRef's addKey POISONS the key (`layouts[key] =
//     nil`) and refuses to bake through it;
//   - legWindowSlot finds a second match and returns (0, false).
//
// Either way the read stays lazy and goes loud at evaluation rather than
// silently binding one of two candidate legs.
//
// THEY USED TO DISAGREE, and this file was written to characterise that: it
// asserted legWindowSlot resolved `D.NAME` to flat slot 1 — the FIRST leg named
// D — while addKey poisoned the identical shape. Documenting a disagreement is
// not the same as being safe from it. Two code paths that differ on whether a
// duplicate qualifier is resolvable are a latent wrong-column read waiting for
// the first caller to reach the permissive one, which is the bug class that
// shipped in #703 and produced wrong rows. So the permissive arm was removed
// instead of watched, and the assertions here are inverted rather than deleted.
//
// WHY DECLINING IS THE RIGHT HALF OF THE DISAGREEMENT. The first-match had no
// upstream backing. SQLSTATE 42702 is ambiguous_column, raised by
// semantic.Scope.ResolveColumn's terminal AmbiguousColumnError
// (semantic/scope.go:271-274) for an ambiguous COLUMN reference; it says
// nothing about two legs sharing a name. No rejection of a repeated FROM-clause
// alias was found either — every ErrCodeDuplicateAlias (42712) producer in pkg/
// is CTE-name duplication. Picking the first of two candidates was therefore a
// guess dressed as a fact, and Java's model agrees with the refusal: an
// Identifier resolving to more than one attribute is an error, not a choice.
//
// WHAT IT COST, measured before the change rather than argued: a probe
// panicking inside legWindowSlot whenever a qualifier matched more than one leg
// was reached ZERO times across ./pkg/relational/... and ./pkg/recordlayer/...
// The only hit was this file's own subtest, which drives the shape
// deliberately. The Java conformance corpus and the plan-shape golden are both
// unmoved by the change.
//
// WHY THIS LIVES HERE AND NOT ON THE `.Field` RATCHET. Two legWindowSlot entries
// used to sit on pkg/docscheck/field_name_decision_test.go's list. They came off
// it when the first-dot re-split was removed, and correctly so: that ratchet
// tracks decisions made from a *values.FieldValue's Field — a DISPLAY name —
// and taints a callee parameter only when the argument derives from one (its
// escapesFieldName predicate). legWindowSlot's surviving caller,
// bakeSegmentedColumnRef, passes the PARSER's segments, so the comparison is no
// longer a `.Field` decision and the detector reports "no decision found" for
// those keys. The risk outlived its tracker; a behavioural test is what can
// hold it, because a test fails when the behaviour moves and a ledger line
// never does.
func TestDuplicateQualifier_ReadersAgree(t *testing.T) {
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

	t.Run("legQOV baker POISONS the duplicate", func(t *testing.T) {
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

	t.Run("legWindowSlot DECLINES the duplicate", func(t *testing.T) {
		t.Parallel()
		// A flat row of two 2-column leg windows, both legs named "D".
		cols := []string{"ID", "NAME", "NAME", "ID"}
		legs := []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("L"), "D", 0, 2),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("R"), "D", 2, 2),
		}
		// INVERTED. This asserted `found == true` and `k == 1` — that
		// legWindowSlot took the FIRST leg named D and resolved to flat slot 1
		// — and the file characterised the two readers as DIVERGENT. That
		// divergence is now closed: legWindowSlot declines an ambiguous
		// qualifier exactly as addKey poisons one. The assertion is inverted
		// rather than deleted so the old behaviour cannot return unnoticed.
		if k, found := legWindowSlot("D", "NAME", cols, legs, values.DottedLegSiteFlatColumnBake); found {
			t.Fatalf("legWindowSlot RESOLVED a qualifier carried by two legs, to flat slot "+
				"%d. THE PERMISSIVE ARM HAS COME BACK. Picking one of two candidate legs by "+
				"position is a wrong-column read: nothing upstream rejects a repeated "+
				"FROM-clause alias (42702 is ambiguous_column, about a COLUMN reference, "+
				"and every 42712 producer in pkg/ is CTE-name duplication), so the first "+
				"match is a guess. The other reader of this channel declines the same "+
				"shape; a re-divergence here is the wrong-rows bug class that shipped in "+
				"#703.", k)
		}
		// CONTROL 1: the walk still RESOLVES when the qualifier is unambiguous,
		// so the decline above is the duplication being refused rather than
		// legWindowSlot having become inert.
		uniq := []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("L"), "D", 0, 2),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("R"), "E", 2, 2),
		}
		k, found := legWindowSlot("D", "NAME", cols, uniq, values.DottedLegSiteFlatColumnBake)
		if !found || k != 1 {
			t.Fatalf("control: an UNAMBIGUOUS qualifier D resolved to (%d,%v), want (1,true). "+
				"The decline above then proves only that the walk is dead, and this test is "+
				"vacuous. This control is also what reddens if the decline is OVER-TIGHTENED "+
				"into refusing every qualifier.", k, found)
		}
		// CONTROL 2: the two duplicate legs genuinely hold NAME at different
		// flat slots, so the refused case was a real choice between two
		// candidates rather than two spellings of one answer.
		if cols[1] == cols[3] {
			t.Fatal("control: the two legs' NAME columns landed on the same flat slot, so " +
				"the ambiguity had nothing to choose between and the refusal above is vacuous")
		}
	})
}
