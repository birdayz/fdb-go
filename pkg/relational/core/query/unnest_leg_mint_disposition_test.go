package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRebaseUnnestOuterLegPredicate_MintIsIntentional pins the DISPOSITION of
// the second dotted merged-row producer, rebaseUnnestOuterLegPredicate's mint.
//
// The measured situation is unusual and easy to misread, so both halves are
// stated here:
//
//   - The mint fires on NO covered surface. A panic wired into it survives
//     pkg/relational/core/query, pkg/relational/core/embedded and the whole
//     real-FDB pkg/relational/sqldriver suite (forced re-run, nothing cached).
//   - That is NOT unreachability, and the mint is NOT a latent bug. For a
//     shape that does reach it, rewriting the outer-leg read onto the merged
//     unnest output is the CORRECT answer: leaving QOV(leg) in place makes the
//     leg reference unbound under the existential's merged binding, which
//     evaluates to NULL and silently drops every row an EXISTS should have
//     kept.
//
// So the mint stays, and it must NOT be converted into a loud decline: a
// decline would reject shapes the mint handles correctly, on the strength of a
// zero that only says "no test drives this path today". This test exists so
// that reasoning is a checked fact rather than a comment, and so a future
// reader who notices the zero coverage finds the intended behavior pinned
// instead of an unexercised branch that looks safe to delete.
//
// ITS COUNTERPART IS NO LONGER A TWIN. The cascades NLJ rule's leg-match arm
// (rebaseOuterLegValue, under rebaseOuterLegRefsToMerged) used to mint the same
// dotted `QOV(merged)."LEG.COL"` key that this one does, and the two were
// genuinely the same shape with opposite coverage. That mint is DELETED: the arm
// now either re-anchors by ORDINAL against a stated merged layout or hands the
// reference back on its own leg correlation — see
// TestRebaseOuterLegValue_DerivableLegKeepsTheLegLocalRead, which is the
// retarget of the test this comment used to name.
//
// So this mint is the LAST producer of the dotted key on the leg-rebase path,
// and the reasoning above stands on its own rather than by analogy: nothing
// else's measurement licenses it, and nothing else's retirement retires it.
func TestRebaseUnnestOuterLegPredicate_MintIsIntentional(t *testing.T) {
	t.Parallel()

	legAlias := values.NamedCorrelationIdentifier("T")
	mergedCorr := values.NamedCorrelationIdentifier("q$merged")
	outerLegs := map[string]struct{}{"T": {}}

	legRowType := values.Type(values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	}))
	mergedType := values.NewRecordType("MERGED", false, []values.Field{
		{Name: "ELEM", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "T.ID", FieldType: values.NotNullLong, Ordinal: 1},
	})

	// `elem = T.ID` — an existential residual whose comparand reads an outer
	// table leg, the shape the rebase exists for.
	legRead := exactTestField(t, exactTestQOV(t, legAlias.Name(), legRowType), 0)
	elemRead := exactTestField(t, exactTestQOV(t, mergedCorr.Name(), mergedType), 0)
	pred := predicates.NewComparisonPredicate(elemRead,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: legRead})

	out, rebased := rebaseUnnestOuterLegPredicate(pred, outerLegs, mergedCorr, mergedType, UnnestLegMintSiteAnchoredNonExists)
	if !rebased {
		t.Fatal("exact outer-leg rebase declined")
	}

	cmp, ok := out.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("rebase must return a comparison predicate, got %T", out)
	}
	operand, ok := values.AsFieldValue(cmp.Comparison.Operand)
	if !ok {
		t.Fatalf("comparand must stay a FieldValue, got %T", cmp.Comparison.Operand)
	}
	if operand.DisplayName() != "T.ID" {
		t.Fatalf("outer-leg read was not re-keyed onto the merged row: Field=%q, want "+
			"%q. Leaving it bare leaves QOV(T) unbound under the existential's merged "+
			"binding — it evaluates NULL and the EXISTS drops rows that should match. "+
			"If this mint is being retired, the replacement must BIND the leg, not "+
			"decline it.", operand.DisplayName(), "T.ID")
	}
	qov, ok := values.AsQuantifiedObjectValue(operand.ChildValue())
	if !ok || qov.Correlation() != mergedCorr {
		t.Fatalf("re-keyed read must hang off the MERGED correlation, got child %T (%v)",
			operand.ChildValue(), operand.ChildValue())
	}

	// The element read (already on the merged correlation) must pass through
	// untouched — otherwise the rebase would double-qualify its own output, and
	// the mint's idempotence is what makes it safe at the several call sites
	// that may see an already-rebased predicate.
	lhs, ok := values.AsFieldValue(cmp.Operand)
	if !ok || lhs.DisplayName() != "ELEM" {
		t.Fatalf("a read already on the merged correlation must pass through "+
			"unchanged, got %#v", cmp.Operand)
	}

	// A correlation that is not an outer leg is none of this rebase's business.
	otherAlias := values.NamedCorrelationIdentifier("OTHER")
	otherRead := exactTestField(t, exactTestQOV(t, otherAlias.Name(), legRowType), 0)
	otherPred := predicates.NewComparisonPredicate(elemRead,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: otherRead})
	otherOut, rebased := rebaseUnnestOuterLegPredicate(otherPred, outerLegs, mergedCorr, mergedType, UnnestLegMintSiteAnchoredNonExists)
	if !rebased {
		t.Fatal("non-outer exact read caused the rebase to decline")
	}
	otherOperand := exactTestFieldView(t, otherOut.(*predicates.ComparisonPredicate).Comparison.Operand)
	if otherOperand.DisplayName() != "ID" {
		t.Fatalf("a non-outer-leg correlation must be left alone, got Field=%q — "+
			"rebasing it would re-anchor a reference whose binding is somebody "+
			"else's", otherOperand.DisplayName())
	}
}

func TestRebaseUnnestOuterLegPredicate_NilCarrierDeclinesOnlyARealOuterLeg(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("T")
	elementAlias := values.NamedCorrelationIdentifier("X")
	innerAlias := values.NamedCorrelationIdentifier("U")
	outerLegs := map[string]struct{}{"T": {}}
	elementType := values.NewRecordType("ELEM", false, []values.Field{
		{Name: "EK", FieldType: values.NotNullLong, Ordinal: 0},
	})
	innerType := values.NewRecordType("U", false, []values.Field{
		{Name: "UK", FieldType: values.NotNullLong, Ordinal: 0},
	})
	outerType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	elementRead := exactTestField(t, exactTestQOV(t, elementAlias.Name(), elementType), 0)
	innerRead := exactTestField(t, exactTestQOV(t, innerAlias.Name(), innerType), 0)
	elementPredicate := predicates.NewComparisonPredicate(innerRead,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: elementRead})

	unchanged, ok := rebaseUnnestOuterLegPredicate(
		elementPredicate, outerLegs, elementAlias, nil, UnnestLegMintSiteJoinPredNotWindowed)
	if !ok || unchanged != elementPredicate {
		t.Fatalf("element-only predicate with no merged carrier = (%T, %t), want pointer-exact no-op",
			unchanged, ok)
	}

	outerRead := exactTestField(t, exactTestQOV(t, outerAlias.Name(), outerType), 0)
	outerPredicate := predicates.NewComparisonPredicate(innerRead,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerRead})
	if got, admitted := rebaseUnnestOuterLegPredicate(
		outerPredicate, outerLegs, elementAlias, nil, UnnestLegMintSiteJoinPredNotWindowed); admitted || got != outerPredicate {
		t.Fatalf("real outer-leg predicate without merged carrier = (%T, %t), want pointer-exact loud decline",
			got, admitted)
	}
}
