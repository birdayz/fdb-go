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
//
// It returns the two leg QUANTIFIERS alongside the yielded plans so a test can
// assert what the layout derivation says about this exact shape. Without that,
// "the mint still fires" is unfalsifiable: it would also hold for a shape whose
// layouts are underivable, which is the case that proves nothing.
func buildTwoLegExistentialSelect() ([]expressions.RelationalExpression, expressions.Quantifier, expressions.Quantifier) {
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
	return FireExpressionRule(NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel)), qA, qB
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

// TestRebaseOuterLegValue_DerivableLegStillMintsTheQualifiedName pins the
// leg-match arm's disposition on a shape whose leg layouts ARE derivable: the read
// is re-anchored onto the merge correlation as a qualified "LEG.COL" key anyway.
//
// The layout is not what decides here, and pinning that on a DERIVABLE shape is
// the only way to keep it true. There was briefly a leg-local bake at this arm
// that resolved the read's display NAME against the derived leg type to mint an
// ordinal; it was deleted because a display name may not decide a column's
// identity (RFC-197), and because a lookup by name is precisely the conflation the
// `.Field` decision gate exists for and precisely the shape that gate cannot see.
// Java's own re-anchor keys on the quantifier's ORDINAL POSITION, never on a name
// (PartitionSelectRule.java:296-303), and Go's ordinal authority in this function
// is the identity-keyed layout lookup — pinned separately by
// TestRebaseOuterLegValue_OrdinalFirst, including the two ways it can be wrong (a
// sibling leg's identical column answering, and the wrong slot answering).
//
// The test this replaced asserted the bake. Its failure message named a
// conversion as the one legitimate reason to retarget it, which is why it is
// retargeted here rather than deleted: the shape it reaches is the one the rule
// actually routes, and that reach is worth keeping. What changed is the expected
// disposition, and the reason is a correctness argument, not a measurement.
func TestRebaseOuterLegValue_DerivableLegStillMintsTheQualifiedName(t *testing.T) {
	t.Parallel()

	yielded, qA, qB := buildTwoLegExistentialSelect()
	if len(yielded) == 0 {
		t.Fatal("ImplementNestedLoopJoinRule yielded nothing for the two-leg + " +
			"trailing-Existential select: OnMatch no longer routes this shape to " +
			"implementJoinWithExistential, so this test no longer covers the " +
			"rebase arm it was written for. Re-derive the shape from OnMatch's " +
			"len(quants)==3 dispatch rather than deleting the test.")
	}

	// PRECONDITION, asserted rather than assumed: both legs state a layout. On a
	// shape whose layouts are underivable the mint would be the only thing the arm
	// COULD emit, and the assertion below would hold for the wrong reason.
	legL := values.NamedCorrelationIdentifier("L")
	legR := values.NamedCorrelationIdentifier("R")
	layouts := legRowTypesFromQuantifiers(
		legRowTypeSource{Quantifier: qA, Alias: legL},
		legRowTypeSource{Quantifier: qB, Alias: legR},
	)
	for _, leg := range []values.CorrelationIdentifier{legL, legR} {
		rt, ok := layouts[leg]
		if !ok || rt == nil || len(rt.Fields) == 0 {
			t.Fatalf("leg %s states no row layout, so this shape cannot pin that the "+
				"mint fires DESPITE a layout — it would fire for want of one. Both legs "+
				"range over a typed scan reference, so GetFlowedObjectType must answer; "+
				"a decline here means the faithful instrument stopped covering the "+
				"simplest possible leg. layouts = %v", leg.Name(), layouts)
		}
	}
	if _, found := layouts[legL].FieldIndex("ID"); !found {
		t.Fatalf("leg L's layout does not declare ID (%v). The correlation predicate "+
			"reads L.ID, so a layout that cannot place it makes the precondition "+
			"vacuous.", layouts[legL].Fields)
	}

	// The qualified merged-row key IS minted, and it is LAZY: nothing bakes an
	// ordinal from the layout above.
	var lazy, baked []string
	for _, fv := range dottedLegRefsOf(yielded) {
		if fv.Resolved == nil {
			lazy = append(lazy, fv.Field)
		} else {
			baked = append(baked, fv.Field)
		}
	}
	if len(baked) > 0 {
		t.Fatalf("a merged-row qualified key is BAKED: %v. The only ordinal authority "+
			"in this arm is the identity-keyed legLayout lookup, and this call path "+
			"passes no layout — a baked key here means an ordinal was minted from "+
			"something else, and the only other thing in hand is the display name.",
			baked)
	}
	if len(lazy) == 0 {
		t.Fatalf("no qualified merged-row key was minted in any of the %d yielded "+
			"plans, on a shape whose leg layouts are both derivable (%v). Either the "+
			"arm stopped being reached — re-derive the shape from OnMatch's "+
			"len(quants)==3 dispatch — or a leg-local bake came back. If a bake came "+
			"back, it may key on the reference's IDENTITY and nothing else; a bake "+
			"keyed on the column's display name is what this test was retargeted "+
			"from.", len(yielded), layouts)
	}
	var sawLegLKey bool
	for _, f := range lazy {
		if strings.EqualFold(f, "L.ID") {
			sawLegLKey = true
		}
	}
	if !sawLegLKey {
		t.Fatalf("the minted keys are %v, none of them L.ID. The existential's "+
			"correlation predicate is E.OUTER_ID = L.ID, so that read is the one this "+
			"arm exists to place, and the merged row's binder resolves it by exactly "+
			"this spelling.", lazy)
	}

	// And no LEG-LOCAL read on L survives: the whole read moved to the merge
	// correlation. A surviving leg-correlated read below the FlatMap would be
	// unbound at the point the predicate is lifted, which is the silent-NULL
	// failure the re-anchor exists to prevent.
	if refs := legLocalLegRefsOf(yielded, legL); len(refs) > 0 {
		var fields []string
		for _, fv := range refs {
			fields = append(fields, fv.Field)
		}
		t.Fatalf("reads still correlated to leg L survive in a yielded plan: %v. The "+
			"step-2 FlatMap binds the MERGED row, so a leg-correlated read lifted into "+
			"it resolves against a binding the planner did not put there.", fields)
	}
}

// TestRebaseOuterLegValue_QualifiedMintSurvivesWithoutALegLayout is the other
// direction. It is no longer a different OUTCOME from the test above — both mint —
// and that is the point of keeping both: the two inputs differ (layout present vs
// absent) and the disposition must not start depending on which.
//
// It is also the unit-level statement of the same fact, reached without firing a
// rule, so a change that alters the arm shows up here as well as in the plan.
func TestRebaseOuterLegValue_QualifiedMintSurvivesWithoutALegLayout(t *testing.T) {
	t.Parallel()

	legA := values.NamedCorrelationIdentifier("L")
	merged := values.UniqueCorrelationIdentifier()
	aType := values.Type(nljTestLayouts["OUTER"]) // ID, CATEGORY

	read := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(legA, aType), "ID", values.UnknownType)

	// No leg layouts at all: nothing to state an ordinal in even in principle.
	out := rebaseOuterLegValue(read, []string{"L"}, merged, nil, nil)
	assertQualifiedLegMint(t, out, merged, "L.ID",
		"no leg layout was supplied at all")
}

// TestRebaseOuterLegValue_TypedLegStillMintsAndCensusRecordsTheMint is F8's
// behavioural half, and it is the axis the two tests above cannot cover: a leg
// whose layout is present AND declares the read's column, handed to the arm
// directly.
//
// This is the state CQ-63 creates for every leg. If the mint survives it — which
// it must, until the bake returns keyed on identity — then the census must record
// a MINT for it and not a convertible read, or the acceptance instrument will
// report the channel retired on the day the legs get types and nothing else
// changes.
func TestRebaseOuterLegValue_TypedLegStillMintsAndCensusRecordsTheMint(t *testing.T) {
	t.Parallel()

	legA := values.NamedCorrelationIdentifier("L")
	merged := values.UniqueCorrelationIdentifier()
	aType := nljTestLayouts["OUTER"] // ID, CATEGORY

	read := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(legA, values.Type(aType)), "ID", values.UnknownType)

	legTypes := map[values.CorrelationIdentifier]*values.RecordType{legA: aType}
	out := rebaseOuterLegValue(read, []string{"L"}, merged, nil, legTypes)
	assertQualifiedLegMint(t, out, merged, "L.ID",
		"a layout for leg L WAS supplied and declares ID")

	// The census's own classification of that firing. The outcome dominates: a
	// minted read with a resolvable layout is LAYOUT-AVAILABLE — a mint that could
	// have been an ordinal — and never Baked.
	if got := classifyLegLocalBake(legLocalBakeMinted, values.Type(aType), "ID"); got != legLocalBakeClassLayoutAvailable {
		t.Fatalf("a MINTED read with a layout declaring its column classified as %v, "+
			"want legLocalBakeClassLayoutAvailable. Recording it as Baked (or dropping "+
			"it) would let the census report the qualified-name channel deletable while "+
			"the mint is the only thing the arm emits.", got)
	}
	if got := classifyLegLocalBake(legLocalBakeBaked, values.Type(aType), "ID"); got != legLocalBakeClassBaked {
		t.Fatalf("a BAKED read classified as %v, want legLocalBakeClassBaked — the "+
			"outcome the arm states has to dominate the type it happens to hold", got)
	}
	// And the untyped/absent split still refines a mint rather than replacing it.
	if got := classifyLegLocalBake(legLocalBakeMinted, values.UnknownType, "ID"); got != legLocalBakeClassUntypedLeg {
		t.Fatalf("a MINTED read with no layout classified as %v, want untypedLeg", got)
	}
	if got := classifyLegLocalBake(legLocalBakeMinted, values.Type(aType), "NOSUCH"); got != legLocalBakeClassColumnAbsent {
		t.Fatalf("a MINTED read whose column the layout does not declare classified as "+
			"%v, want columnAbsent", got)
	}
}

// assertQualifiedLegMint is the shared shape assertion: the arm produced a LAZY
// qualified "LEG.COL" FieldValue over the merge correlation, with no baked ordinal.
func assertQualifiedLegMint(t *testing.T, out values.Value, merged values.CorrelationIdentifier, wantField, because string) {
	t.Helper()
	fv, isFV := out.(*values.FieldValue)
	if !isFV {
		t.Fatalf("rebaseOuterLegValue returned %T, want a *values.FieldValue (%s)", out, because)
	}
	if fv.Resolved != nil {
		t.Fatalf("the read baked ordinal %d (%s). The only ordinal authority in this "+
			"arm is the identity-keyed legLayout lookup, and no layout was passed on "+
			"this path — a bake here was minted from the reference's display name or "+
			"its child type, which is the name channel under another spelling.",
			fv.Resolved.Root().Ordinal, because)
	}
	if !strings.EqualFold(fv.Field, wantField) {
		t.Fatalf("minted field %q, want the qualified %s key (%s) — the merged row's "+
			"binder resolves the leg by exactly this key, so a different spelling binds "+
			"a different leg or nothing", fv.Field, wantField, because)
	}
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV || qov.Correlation != merged {
		t.Fatalf("mint anchored on %v, want the merge correlation %v (%s) — a qualified "+
			"key is only resolvable against the merged row", fv.Child, merged, because)
	}
}

// TestRebaseOuterLegValue_DegradesAnAlreadyCorrectLegLocalRead pins the shape
// the CORPUS actually produces, which neither test above covers.
//
// The two above hand the arm a LAZY read (Resolved == nil) over a TYPED leg QOV.
// Production is the mirror image of that, and the difference is the whole
// finding. MEASURED over the real-FDB sqldriver corpus, all 126 firings of this
// arm report:
//
//	accessors=1 rootOrdinal>=0 pathDomainKnown=true
//	qovTypeDomainKnown=false legTypeDomainKnown=true legTypeDomainMatches=true
//
// The read is already BAKED — single accessor, non-negative ordinal, and a
// stated domain that IS the leg's own row layout — while the QuantifiedObjectValue
// child it hangs off is UNTYPED, because that is how the resolver mints it
// (expr.go's correlated arm builds NewQuantifiedObjectValue, whose Typ is
// UnknownType).
//
// Two consequences, and this test exists to keep both from being re-argued:
//
//   - legSlotIdentity DECLINES every one of them. It derives its frontier from
//     fv.Child's stored type, so an untyped child makes the frontier unknown and
//     OrdinalIn fails closed — even though the ordinal beside it is correct and
//     indexes exactly the layout the leg flows. The bakeability census's
//     layoutAvailable 126/126 is therefore NOT the bake's precondition: the leg
//     having a layout and the read being able to state an ordinal in it are
//     different questions, and only the first is answered.
//   - the arm DEGRADES the read. A correct leg-local ordinal reference goes in;
//     a LAZY dotted "LEG.COL" name over the merge correlation comes out. So
//     CQ-53 phase 2's job at this site is not to re-land a bake — there is
//     nothing to re-land, the reference already carries the ordinal — it is to
//     stop destroying the one that arrives.
//
// WHEN THIS GOES RED it means the degradation stopped, which is the goal. Do not
// relax it: RETARGET it to assert the read passes through with its ordinal and
// its own leg alias intact, and make sure the runtime binder
// (bindMergedOuterLegs) binds that alias for the shape in question — the read is
// only correct if the leg alias resolves to the leg's window at eval time.
func TestRebaseOuterLegValue_DegradesAnAlreadyCorrectLegLocalRead(t *testing.T) {
	t.Parallel()

	legA := values.NamedCorrelationIdentifier("L")
	merged := values.UniqueCorrelationIdentifier()
	aType := nljTestLayouts["OUTER"] // ID, CATEGORY
	legDomain := values.OrdinalDomainOfType(values.Type(aType))

	// The production shape: baked against the LEG's own layout, hung off an
	// UNTYPED quantifier object exactly as the resolver mints it.
	read := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(legA), "CATEGORY", 1, values.UnknownType, legDomain)

	// Premise 1: the read DOES state a correct leg-local identity — asked in the
	// leg's real layout, which is what a bake would index.
	id, stated := read.CorrelatedIdentityIn(legDomain)
	if !stated || id.Ordinal != 1 || id.Correlation != legA {
		t.Fatalf("the fixture is not the production shape: CorrelatedIdentityIn in the "+
			"leg's own layout gave (%+v, %v), want ordinal 1 on leg L. Every claim below "+
			"is about a read that already carries its ordinal; a fixture that does not "+
			"carry one tests something else.", id, stated)
	}

	// Premise 2: legSlotIdentity — the arm's OWN identity derivation — declines it
	// anyway, because it reads the frontier off the untyped child.
	if _, ok := legSlotIdentity(read); ok {
		t.Fatal("legSlotIdentity ANSWERED for a read whose quantifier object is " +
			"untyped. The corpus measures the opposite on all 126 firings " +
			"(qovTypeDomainKnown=false), and that decline is the entire reason the " +
			"identity-keyed bake reports zero convertible reads while the layout census " +
			"reports 126 available layouts. If this now answers, the QOV children got " +
			"typed upstream — re-measure the census before concluding anything else.")
	}

	// The degradation itself: ordinal in, lazy dotted name out.
	legTypes := map[values.CorrelationIdentifier]*values.RecordType{legA: aType}
	out := rebaseOuterLegValue(read, []string{"L"}, merged, nil, legTypes)
	assertQualifiedLegMint(t, out, merged, "L.CATEGORY",
		"a read that ALREADY carried the right leg-local ordinal was re-anchored "+
			"onto the merge correlation as a lazy qualified name")
}
