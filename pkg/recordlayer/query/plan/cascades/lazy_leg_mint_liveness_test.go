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

// legLocalLegRefsOf collects every FieldValue reachable from the yielded plans
// that reads a column off a LEG quantifier — the shape the leg-match arm now
// produces when it can state the leg's layout. Mirrors dottedLegRefsOf's walk
// exactly (same surfaces, same node kinds) so the two are comparable: a reference
// counted by one and not the other has genuinely changed form.
func legLocalLegRefsOf(yielded []expressions.RelationalExpression, leg values.CorrelationIdentifier) []*values.FieldValue {
	var out []*values.FieldValue
	visit := func(v values.Value) values.Value {
		if fv, ok := v.(*values.FieldValue); ok {
			if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV && qov.Correlation == leg {
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

// TestRebaseOuterLegValue_KeepsTheLegAliasAndBakesLegLocally pins what the
// leg-match arm produces for a shape whose leg layouts are derivable: the read
// stays on its OWN leg alias, baked to that leg's own ordinal, and NO qualified
// "LEG.COL" key over the merge correlation is minted anywhere.
//
// This is Java's shape. A FlatMap binds one correlation per quantifier
// (RecordQueryFlatMapPlan.java:135-140 over the parent chain at
// Bindings.java:116-134), so a leg-correlated read never has to be re-anchored: it
// is an alias plus an ordinal, and QuantifiedObjectValue.eval is a map lookup
// (QuantifiedObjectValue.java:84-85). Go had to pack the leg into a column NAME
// because its two-level NLJ→FlatMap lowering bound only the join's own alias, so
// the leg alias was unbound below the FlatMap. It is bound now
// (executor.bindMergedOuterLegs), and the read can keep it.
//
// The test this replaced asserted the OPPOSITE — that the arm mints the lazy
// qualified name — and it was right to, because that was what the arm did. Its
// failure message named this exact outcome as the one legitimate reason to
// retarget it ("If the shape now bakes instead, that is a real improvement —
// retarget this test deliberately"). Both directions still matter and both are
// still pinned: this test pins that a DERIVABLE shape converts, and
// TestRebaseOuterLegValue_QualifiedMintSurvivesWithoutALegLayout pins that the
// qualified mint is still what an UNDERIVABLE one gets. Neither may be inferred
// from the other, and the residue between them is measured rather than assumed
// (the leg-local bakeability census).
func TestRebaseOuterLegValue_KeepsTheLegAliasAndBakesLegLocally(t *testing.T) {
	t.Parallel()

	yielded := buildTwoLegExistentialSelect()
	if len(yielded) == 0 {
		t.Fatal("ImplementNestedLoopJoinRule yielded nothing for the two-leg + " +
			"trailing-Existential select: OnMatch no longer routes this shape to " +
			"implementJoinWithExistential, so this test no longer covers the " +
			"rebase arm it was written for. Re-derive the shape from OnMatch's " +
			"len(quants)==3 dispatch rather than deleting the test.")
	}

	// No qualified merged-row key, of either kind. This is the channel's death
	// for this shape, and it is the assertion that reds if the arm goes back to
	// re-anchoring.
	var lazy, baked []string
	for _, fv := range dottedLegRefsOf(yielded) {
		if fv.Resolved == nil {
			lazy = append(lazy, fv.Field)
		} else {
			baked = append(baked, fv.Field)
		}
	}
	if len(lazy) > 0 || len(baked) > 0 {
		t.Fatalf("a leg is still packed into a column NAME over a merge correlation "+
			"for a shape whose leg layouts ARE derivable from its leg plans: lazy %v, "+
			"baked %v.\nBoth legs of this select are Scan plans, so physicalLegRowTypes "+
			"states both layouts and the read has an ordinal to carry on its own alias. "+
			"A qualified key here means the leg-local arm stopped being tried, or the "+
			"layout derivation stopped covering scan legs.", lazy, baked)
	}

	// And the read that USED to become "L.ID" is now a leg-local baked read on L.
	legL := values.NamedCorrelationIdentifier("L")
	refs := legLocalLegRefsOf(yielded, legL)
	if len(refs) == 0 {
		t.Fatalf("no reference to leg L survives in any of the %d yielded plans. "+
			"The existential correlates to L.ID, so the read has to be SOMEWHERE — "+
			"finding neither a qualified merged key nor a leg-local read means the "+
			"reference was dropped, which is silent wrong rows, not a conversion.",
			len(yielded))
	}
	baseCorrelated := 0
	for _, fv := range refs {
		if fv.Resolved == nil {
			t.Fatalf("leg reference %q on L is UNBAKED. A lazy leg read has no runtime "+
				"name channel to fall back on — FieldValue.resolveOrdinal has no "+
				"name-derive arm — so it fails loud at evaluation. The leg-local arm "+
				"must bake or leave the read for the qualified mint, never emit this.",
				fv.Field)
		}
		if !strings.EqualFold(fv.Field, "ID") {
			continue
		}
		baseCorrelated++
		// The ordinal is L's OWN — ID is column 0 of (ID, CATEGORY). A merged-row
		// ordinal would also be 0 here, which is why the ALIAS assertion above is
		// the load-bearing half: this one guards the leg-local layout it indexes.
		if got := fv.Resolved.Root().Ordinal; got != 0 {
			t.Fatalf("leg-local read L.ID baked ordinal %d, want 0 — L's own row is "+
				"(ID, CATEGORY), so ID is its slot 0. A different ordinal means the "+
				"bake indexed some other layout than the leg it names.", got)
		}
	}
	if baseCorrelated == 0 {
		t.Fatalf("references to L survive (%d) but none reads ID: %v. The existential's "+
			"correlation predicate is E.OUTER_ID = L.ID, so an ID read on L is the one "+
			"reference this arm exists to place.", len(refs), refs)
	}
}

// TestRebaseOuterLegValue_QualifiedMintSurvivesWithoutALegLayout is the other
// direction, and it is not implied by the one above.
//
// The leg-local bake needs a LAYOUT to state an ordinal in, and that layout is read
// off the leg's chosen physical plan. Where the caller has no layout for the leg —
// measured as the live residue over the real-FDB corpus, where a leg's plan is a
// shape planBuriedLegConcat cannot reduce to a row type — the arm still falls
// through to the qualified "LEG.COL" mint over the merge correlation.
//
// Pinning it keeps two different things honest. The residue cannot silently
// disappear (which would mean reads are being dropped or mis-anchored rather than
// converted), and it cannot silently GROW back over shapes that used to convert.
func TestRebaseOuterLegValue_QualifiedMintSurvivesWithoutALegLayout(t *testing.T) {
	t.Parallel()

	legA := values.NamedCorrelationIdentifier("L")
	merged := values.UniqueCorrelationIdentifier()
	aType := values.Type(nljTestLayouts["OUTER"]) // ID, CATEGORY

	read := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(legA, aType), "ID", values.UnknownType)

	// No leg layouts at all: the arm has nothing to bake against.
	out := rebaseOuterLegValue(read, []string{"L"}, merged, nil, nil)
	fv, isFV := out.(*values.FieldValue)
	if !isFV {
		t.Fatalf("rebaseOuterLegValue returned %T, want a *values.FieldValue", out)
	}
	if fv.Resolved != nil {
		t.Fatalf("the read baked with NO leg layout supplied (ordinal %d). The bake's "+
			"only legitimate source of an ordinal is the leg's physical layout; baking "+
			"without one means it fell back on the reference's own display name or on "+
			"the child type, which is the name channel under another spelling.",
			fv.Resolved.Root().Ordinal)
	}
	if !strings.EqualFold(fv.Field, "L.ID") {
		t.Fatalf("layout-less fallback minted field %q, want the qualified L.ID key — "+
			"the merged row's binder resolves the leg by exactly this key, so a "+
			"different spelling binds a different leg or nothing", fv.Field)
	}
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV || qov.Correlation != merged {
		t.Fatalf("layout-less fallback anchored on %v, want the merge correlation %v — "+
			"a qualified key is only resolvable against the merged row", fv.Child, merged)
	}
}
