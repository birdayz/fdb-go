package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// buildTwoLegExistentialSelect assembles the shape OnMatch routes to
// implementJoinWithExistential: exactly two ForEach legs plus one trailing
// Existential in a single flat Select (`SELECT … FROM L, R WHERE EXISTS
// (SELECT 1 FROM E WHERE E.OUTER_ID = L.ID)`), with the existential's
// correlation predicate pointing at a LEG column.
//
// That last part is what makes the leg-match arm reachable: the step-2 FlatMap
// binds only the MERGED outer row, so a predicate reading QOV(L).ID has to be
// re-anchored onto the merge correlation before it can be lifted, and
// rebaseOuterLegRefsToMerged is what does the re-anchoring.
func buildTwoLegExistentialSelect() []expressions.RelationalExpression {
	legA := values.NamedCorrelationIdentifier("L")
	legB := values.NamedCorrelationIdentifier("R")
	existAlias := values.NamedCorrelationIdentifier("E")

	aType := values.Type(nljTestLayouts["OUTER"])  // ID, CATEGORY
	bType := values.Type(nljTestLayouts["SHADOW"]) // ID, NOTE
	eType := values.Type(nljTestLayouts["INNER"])  // ID, OUTER_ID

	newLeg := func(table string, rt values.Type) *expressions.Reference {
		ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{table}, rt))
		ref.InsertFinal(plans.NewRecordQueryScanPlan([]string{table}, rt, false))
		return ref
	}

	qA := expressions.NamedForEachQuantifier(legA, newLeg("OUTER", aType))
	qB := expressions.NamedForEachQuantifier(legB, newLeg("SHADOW", bType))
	qE := expressions.NamedExistentialQuantifier(existAlias, newLeg("INNER", eType))

	// E.OUTER_ID = L.ID — an inner↔outer correlation predicate, the only kind
	// existsInnerCorrelation lifts, and the one whose outer half must be
	// rebased onto the merged row.
	innerRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(existAlias, eType), "OUTER_ID", values.UnknownType)
	outerLegRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(legA, aType), "ID", values.UnknownType)

	sel := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(legA),
		[]expressions.Quantifier{qA, qB, qE},
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(innerRef,
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerLegRef}),
			predicates.NewExistentialAlias(existAlias),
		},
		[]string{"L", "R", "E"},
	)
	return FireExpressionRule(NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
}

// dottedLegRefsOf collects every FieldValue reachable from the yielded plans
// whose Field is DOTTED and whose child is a QuantifiedObjectValue — the exact
// signature rebaseOuterLegValue's leg-match arm emits. Nothing else in this
// scenario produces a dotted Field: both leaf row types declare only flat
// top-level columns, and the predicates are built here from bare names.
//
// The walk covers the predicate surfaces a rebased existential predicate lands
// on (the compensation chain's filter, the existential subplan's own
// predicates and scan bounds) plus every node's result value, since a
// projected EXISTS carries its rebased projection in the FlatMap's result
// value. A surface this misses makes the test FAIL, never pass vacuously —
// the safe direction for a liveness assertion.
func dottedLegRefsOf(yielded []expressions.RelationalExpression) []*values.FieldValue {
	var out []*values.FieldValue
	visit := func(v values.Value) values.Value {
		if fv, ok := v.(*values.FieldValue); ok && strings.Contains(fv.Field, ".") {
			if _, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
				out = append(out, fv)
			}
		}
		return v
	}
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, visit)
		}
	}
	collectRanges := func(crs []*predicates.ComparisonRange) {
		for _, cr := range crs {
			switch {
			case cr.IsEquality():
				collectComparison(cr.GetEqualityComparison())
			case cr.IsInequality():
				for _, c := range cr.GetInequalityComparisons() {
					collectComparison(c)
				}
			}
		}
	}
	for _, y := range yielded {
		rp, ok := y.(plans.RecordQueryPlan)
		if !ok {
			continue
		}
		plans.Walk(rp, func(p plans.RecordQueryPlan) bool {
			if rv := p.GetResultValue(); rv != nil {
				values.Replace(rv, visit)
			}
			switch t := p.(type) {
			case *plans.RecordQueryPredicatesFilterPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryFilterPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryNestedLoopJoinPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryScanPlan:
				collectRanges(t.GetScanComparisons())
			case *plans.RecordQueryIndexPlan:
				collectRanges(t.GetScanComparisons())
			}
			return true
		})
	}
	return out
}

// TestRebaseOuterLegValue_LazyMintIsLive pins that ImplementNestedLoopJoinRule
// REACHES rebaseOuterLegValue's leg-match arm, and that what the arm produces
// there is the LAZY qualified mint — FieldValue{Child: QOV(merged),
// Field: "LEG.COL"} with no resolved ordinal.
//
// It exists because the arm's own comment declared the whole arm
// "dead-in-effect TODAY … a panic wired into this lookup is reached only by
// TestRebaseOuterLegValue_OrdinalFirst". That was false. Only the
// legLayout != nil BAKE inside the arm is dead-in-effect; the enclosing arm is
// reached, and reached from the nil-layout call implementJoinWithExistential
// makes for non-windowed step-1 result values.
//
// Nothing else could catch a deletion of that mint. The arm is exercised
// during planning of real shapes (the buried/N-way/four-leg projected-EXISTS
// family in pkg/relational/sqldriver), but the physical candidate carrying its
// output LOSES on every one of them, and OptimizeGroup prunes each group's
// finals to the winner — so the mint is invisible both in the winning plan and
// in the post-planning memo, and every row-level test over those shapes stays
// green with the arm removed. Firing the rule directly is the repo's
// established way to pin formation past that pruning.
//
// The companion negative — that the mint reaches no WINNING plan today — is
// pinned by TestLazyLegMintReachesNoWinningPlan. The two together are the
// whole truth about this channel: the code is live, its product is not
// selected, and neither half may be inferred from the other.
func TestRebaseOuterLegValue_LazyMintIsLive(t *testing.T) {
	t.Parallel()

	yielded := buildTwoLegExistentialSelect()
	if len(yielded) == 0 {
		t.Fatal("ImplementNestedLoopJoinRule yielded nothing for the two-leg + " +
			"trailing-Existential select: OnMatch no longer routes this shape to " +
			"implementJoinWithExistential, so this test no longer covers the " +
			"rebase arm it was written for. Re-derive the shape from OnMatch's " +
			"len(quants)==3 dispatch rather than deleting the test.")
	}

	var lazy, baked []string
	for _, fv := range dottedLegRefsOf(yielded) {
		if fv.Resolved == nil {
			lazy = append(lazy, fv.Field)
		} else {
			baked = append(baked, fv.Field)
		}
	}

	if len(lazy) == 0 {
		t.Fatalf("no LAZY dotted leg reference over a merge correlation in any of "+
			"the %d yielded plans: rebaseOuterLegValue's leg-match arm was not "+
			"reached, or it no longer mints the lazy qualified name (baked dotted "+
			"refs found: %v).\nIf the shape now bakes instead, that is a real "+
			"improvement — retarget this test deliberately. Do NOT conclude the "+
			"arm is dead: its prose claimed exactly that once, and the claim was "+
			"measurably wrong.", len(yielded), baked)
	}

	// The key is dotted BY CONSTRUCTION (corr + "." + upper(field)). Assert the
	// leg half names the leg actually correlated to, so a change that keys the
	// merged row some other way cannot pass while silently changing what the
	// FlatMap inner's binder looks up.
	foundLeg := false
	for _, f := range lazy {
		if strings.EqualFold(f, "L.ID") {
			foundLeg = true
		}
	}
	if !foundLeg {
		t.Fatalf("the lazy mint fired but not for the correlated leg column: got %v, "+
			"want an L.ID key — the existential correlates to L.ID, and the merged "+
			"row's binder resolves it by exactly that qualified key", lazy)
	}
}
