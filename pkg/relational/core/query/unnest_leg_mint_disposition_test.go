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
// Its counterpart in the cascades NLJ rule (rebaseOuterLegRefsToMerged) DOES
// fire during planning — see TestRebaseOuterLegValue_LazyMintIsLive. The two
// producers are twins in shape and opposites in coverage; do not generalize
// one's measurement to the other.
func TestRebaseUnnestOuterLegPredicate_MintIsIntentional(t *testing.T) {
	t.Parallel()

	legAlias := values.NamedCorrelationIdentifier("T")
	mergedCorr := values.NamedCorrelationIdentifier("q$merged")
	outerLegs := map[string]struct{}{"T": {}}

	legRowType := values.Type(values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
	}))

	// `elem = T.ID` — an existential residual whose comparand reads an outer
	// table leg, the shape the rebase exists for.
	legRead := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(legAlias, legRowType), "ID", values.UnknownType)
	elemRead := values.NewFieldValue(
		values.NewQuantifiedObjectValue(mergedCorr), "ELEM", values.UnknownType)
	pred := predicates.NewComparisonPredicate(elemRead,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: legRead})

	out := rebaseUnnestOuterLegPredicate(pred, outerLegs, mergedCorr)

	cmp, ok := out.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("rebase must return a comparison predicate, got %T", out)
	}
	operand, ok := cmp.Comparison.Operand.(*values.FieldValue)
	if !ok {
		t.Fatalf("comparand must stay a FieldValue, got %T", cmp.Comparison.Operand)
	}
	if operand.Field != "T.ID" {
		t.Fatalf("outer-leg read was not re-keyed onto the merged row: Field=%q, want "+
			"%q. Leaving it bare leaves QOV(T) unbound under the existential's merged "+
			"binding — it evaluates NULL and the EXISTS drops rows that should match. "+
			"If this mint is being retired, the replacement must BIND the leg, not "+
			"decline it.", operand.Field, "T.ID")
	}
	qov, ok := operand.Child.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != mergedCorr {
		t.Fatalf("re-keyed read must hang off the MERGED correlation, got child %T (%v)",
			operand.Child, operand.Child)
	}

	// The element read (already on the merged correlation) must pass through
	// untouched — otherwise the rebase would double-qualify its own output, and
	// the mint's idempotence is what makes it safe at the several call sites
	// that may see an already-rebased predicate.
	lhs, ok := cmp.Operand.(*values.FieldValue)
	if !ok || lhs.Field != "ELEM" {
		t.Fatalf("a read already on the merged correlation must pass through "+
			"unchanged, got %#v", cmp.Operand)
	}

	// A correlation that is not an outer leg is none of this rebase's business.
	otherAlias := values.NamedCorrelationIdentifier("OTHER")
	otherRead := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(otherAlias, legRowType), "ID", values.UnknownType)
	otherPred := predicates.NewComparisonPredicate(elemRead,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: otherRead})
	otherOut := rebaseUnnestOuterLegPredicate(otherPred, outerLegs, mergedCorr)
	otherOperand := otherOut.(*predicates.ComparisonPredicate).Comparison.Operand.(*values.FieldValue)
	if otherOperand.Field != "ID" {
		t.Fatalf("a non-outer-leg correlation must be left alone, got Field=%q — "+
			"rebasing it would re-anchor a reference whose binding is somebody "+
			"else's", otherOperand.Field)
	}
}
