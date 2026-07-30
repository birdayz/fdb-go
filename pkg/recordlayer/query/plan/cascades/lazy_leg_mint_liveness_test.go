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
	//
	// The OUTER half is built BAKED — a single accessor at the column's ordinal
	// in the leg's own row layout — because that is the shape production
	// produces. It used to be built lazy, which predates the resolver carrying
	// the row its correlated quantifier object flows
	// (Quantifier.java:801-803's `QuantifiedObjectValue.of(getAlias(),
	// getFlowedObjectType())`); measured over the real-FDB corpus, every one of
	// the arm's firings arrives with a resolved path in its leg's own domain and
	// none arrives lazy. A lazy fixture therefore drove the arm's DECLINE while
	// claiming to cover the path production takes.
	innerRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(existAlias, eType), "OUTER_ID", values.UnknownType)
	outerLegRef := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValueOfType(legA, aType), "ID", 0, values.UnknownType,
		values.OrdinalDomainOfType(aType))

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

// TestRebaseOuterLegValue_DerivableLegKeepsTheLegLocalRead pins the leg-match
// arm's disposition on a shape whose leg layouts ARE derivable and that the rule
// actually routes here: the read keeps its OWN leg correlation and its OWN
// leg-local ordinal, and no qualified merged-row key is minted anywhere.
//
// It is the retarget the version before it asked for by name. That version
// asserted the opposite — that the read was re-anchored onto the merge
// correlation as a lazy "LEG.COL" key — and said in its own failure text that a
// bake keyed on the reference's IDENTITY (never on its display name) was the one
// legitimate reason to change it. That is what happened: the reference now
// ARRIVES carrying a leg-local ordinal in its leg's own domain, because the
// resolver's correlated arm builds `QuantifiedObjectValue.of(alias,
// getFlowedObjectType())` the way Java's Quantifier.java:801-803 always has, so
// there is nothing left for the arm to mint and the qualified-name mint is
// deleted rather than kept as a fallback.
//
// The REACH is still what this test buys, and it is still pinned on a DERIVABLE
// shape: the layout is not what decides here, and only a derivable fixture keeps
// that honest — on an underivable one every assertion below would hold for the
// wrong reason.
func TestRebaseOuterLegValue_DerivableLegKeepsTheLegLocalRead(t *testing.T) {
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

	// NO qualified merged-row key is minted, lazy or baked. That string is the
	// RFC-197 channel and this arm no longer has it.
	var dotted []string
	for _, fv := range dottedLegRefsOf(yielded) {
		dotted = append(dotted, fv.Field)
	}
	if len(dotted) > 0 {
		t.Fatalf("qualified merged-row keys reached a yielded plan: %v.\n"+
			"  The leg-match arm's qualified mint is DELETED — a reference arriving here\n"+
			"  already carries its leg-local ordinal, so there is nothing to mint, and a\n"+
			"  fallback that spells a leg into a column name is how this channel survives\n"+
			"  the migration meant to end it. If a key is back, find the arm that emitted\n"+
			"  it rather than widening this expectation.", dotted)
	}

	// The read the arm exists to place SURVIVES on its own leg correlation, with
	// its own leg-local ordinal. This is the assertion that replaced "the whole
	// read moved to the merge correlation": the merged row's binder
	// (executor.bindMergedOuterLegs) binds every leg of the merged outer under
	// its OWN correlation, so the read resolves against leg L's window exactly as
	// it would have against an unmerged source.
	refs := legLocalLegRefsOf(yielded, legL)
	if len(refs) == 0 {
		t.Fatalf("no read correlated to leg L survives in any of the %d yielded plans, "+
			"on a shape whose leg layouts are both derivable (%v). The existential's "+
			"correlation predicate is E.OUTER_ID = L.ID, so that read is the one this "+
			"arm exists to place — its absence means either the arm stopped being "+
			"reached (re-derive the shape from OnMatch's len(quants)==3 dispatch) or the "+
			"read was moved somewhere this walk does not look.", len(yielded), layouts)
	}
	for _, fv := range refs {
		if !strings.EqualFold(fv.Field, "ID") {
			t.Errorf("a surviving leg-L read names %q, want the BARE column ID — a "+
				"qualified spelling on a leg-correlated read is the merged-row key under "+
				"another anchor", fv.Field)
		}
		if fv.Resolved == nil {
			t.Errorf("the surviving leg-L read %q carries NO ordinal. The pass-through's "+
				"whole justification is that the reference already states its column in "+
				"its own leg's domain; one that does not is a reference the arm should "+
				"have declined, and it will resolve at runtime by whatever its display "+
				"name spells.", fv.Field)
		}
	}
}

// TestRebaseOuterLegValue_DeclinesAReadThatStatesNoIdentity pins the arm's
// residue disposition on the two inputs that used to produce the qualified mint:
// a read with no resolved path at all, once with NO leg layout in hand and once
// with a layout that DECLARES the read's column.
//
// Both inputs are kept, and keeping both is the point: they differ on the layout
// and the disposition must not start depending on it. A layout answers "does
// this LEG have a row"; a bake needs "can this READ state an ordinal in it". The
// two came apart once already — LayoutAvailable reached 126 of 126 while
// IdentityInLegDomain was zero, and a migration step was scheduled against the
// proxy — so a fixture that has the layout and lacks the identity is the exact
// shape that must not silently start being rewritten.
//
// The former disposition was `QOV(merged)."L.ID"`: the read's DISPLAY NAME
// standing in for the identity it could not state, resolved at runtime by string
// against the merged row. That is the RFC-197 channel itself, and it is deleted
// rather than kept as a fallback — a fallback that spells a name is how such a
// channel survives the migration meant to end it. What is left is the truth: the
// arm cannot place this read, so it does not move it, and the defect stays
// visible at the producer that built the reference unresolved.
func TestRebaseOuterLegValue_DeclinesAReadThatStatesNoIdentity(t *testing.T) {
	t.Parallel()

	legA := values.NamedCorrelationIdentifier("L")
	merged := values.UniqueCorrelationIdentifier()
	aType := nljTestLayouts["OUTER"] // ID, CATEGORY

	for _, tc := range []struct {
		name     string
		legTypes map[values.CorrelationIdentifier]*values.RecordType
		because  string
	}{
		{"no leg layout", nil, "no leg layout was supplied at all"},
		{
			"layout declaring the column",
			map[values.CorrelationIdentifier]*values.RecordType{legA: aType},
			"a layout for leg L WAS supplied and declares ID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read := values.NewFieldValue(
				values.NewQuantifiedObjectValueOfType(legA, values.Type(aType)), "ID", values.UnknownType)
			out := rebaseOuterLegValue(read, []string{"L"}, merged, nil, tc.legTypes)
			if out != values.Value(read) {
				t.Fatalf("the arm rewrote a read that states NO identity to %v (%s).\n"+
					"  It has nothing to place the read BY except the display name, and\n"+
					"  minting a qualified merged-row key from that name is the channel this\n"+
					"  arm no longer has. A read arriving here unresolved is a PRODUCER\n"+
					"  defect — find the producer; do not restore the mint.", out, tc.because)
			}
		})
	}

	// The census's own classification of the three outcomes. The outcome the arm
	// STATES dominates the type it happens to hold — inverting that is how a
	// census comes to report a live channel as retired.
	if got := classifyLegLocalBake(legLocalBakeDeclined, values.Type(aType), "ID"); got != legLocalBakeClassDeclined {
		t.Fatalf("a DECLINED read with a layout declaring its column classified as %v, "+
			"want legLocalBakeClassDeclined. Folding a decline into a mint or a bake "+
			"loses the one residue this census asserts at zero.", got)
	}
	if got := classifyLegLocalBake(legLocalBakeBaked, values.Type(aType), "ID"); got != legLocalBakeClassBaked {
		t.Fatalf("a BAKED read classified as %v, want legLocalBakeClassBaked — the "+
			"outcome the arm states has to dominate the type it happens to hold", got)
	}
	// And the layout split still refines a MINT rather than replacing it. Minted
	// is now the merged re-anchor (Java's PartitionSelectRule.java:296-303 move),
	// dead-in-effect on the corpus but the one arm that still spells a leg into a
	// column name — so its reasons stay measured.
	if got := classifyLegLocalBake(legLocalBakeMinted, values.Type(aType), "ID"); got != legLocalBakeClassLayoutAvailable {
		t.Fatalf("a MINTED read with a layout declaring its column classified as %v, "+
			"want legLocalBakeClassLayoutAvailable", got)
	}
	if got := classifyLegLocalBake(legLocalBakeMinted, values.UnknownType, "ID"); got != legLocalBakeClassUntypedLeg {
		t.Fatalf("a MINTED read with no layout classified as %v, want untypedLeg", got)
	}
	if got := classifyLegLocalBake(legLocalBakeMinted, values.Type(aType), "NOSUCH"); got != legLocalBakeClassColumnAbsent {
		t.Fatalf("a MINTED read whose column the layout does not declare classified as "+
			"%v, want columnAbsent", got)
	}
}

// TestRebaseOuterLegValue_PassesThroughAnAlreadyCorrectLegLocalRead pins the
// shape the CORPUS actually produces, which the test above does not cover, and
// it is the retarget that test's predecessor asked for by name.
//
// The test above hands the arm a LAZY read (Resolved == nil). Production is the
// mirror image: MEASURED over the real-FDB sqldriver corpus, every firing of
// this arm arrives with a single accessor at a non-negative ordinal in a stated
// domain that IS the leg's own row layout, and none arrives lazy.
//
// It used to report `qovTypeDomainKnown=false` on all 126 of them — the ordinal
// was right and the reference's own quantifier object was UNTYPED, so
// legSlotIdentity's frontier was unknown and OrdinalIn failed closed. Typing the
// resolver's correlated mints (Quantifier.java:801-803's
// `QuantifiedObjectValue.of(getAlias(), getFlowedObjectType())`) moved the corpus
// from identityInLegDomain 0 to identityInLegDomain 126, and this arm from
// DEGRADING every such read — ordinal in, lazy dotted "LEG.COL" out — to handing
// it back untouched.
//
// The predecessor's instruction, followed here verbatim: assert the read passes
// through with its ordinal and its own leg alias intact, AND make sure the
// runtime binder binds that alias for the shape in question. The second half is
// not assertable at this scale, so it is pinned where it is observable —
// TestFDB_SelfJoinTwinLegCorrelatedRead runs a real self-join at FDB whose two
// legs share the column name, the leg-relative ordinal AND the ordinal domain,
// leaving the correlation as the only thing that can pick one, on rows where the
// two possible answers are complements.
//
// WHAT THAT PIN DOES NOT SHOW, measured and stated so it is not read as more
// than it is: the arm's product reaches NO winning plan. Replacing this
// pass-through with a deliberate bind to the TWIN leg leaves the whole real-FDB
// sqldriver corpus green, exactly as TestLazyLegMintReachesNoWinningPlan says of
// the mint it replaced. The candidate carrying it loses. So the disposition here
// is pinned at RULE level, and the runtime question it answers is one the corpus
// cannot currently ask.
func TestRebaseOuterLegValue_PassesThroughAnAlreadyCorrectLegLocalRead(t *testing.T) {
	t.Parallel()

	legA := values.NamedCorrelationIdentifier("L")
	merged := values.UniqueCorrelationIdentifier()
	aType := nljTestLayouts["OUTER"] // ID, CATEGORY
	legDomain := values.OrdinalDomainOfType(values.Type(aType))

	// The production shape: baked against the LEG's own layout, hung off a
	// quantifier object that STATES that same layout — which is what the
	// resolver now mints (its correlated arm carries the source's declared row).
	read := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValueOfType(legA, values.Type(aType)), "CATEGORY", 1, values.UnknownType, legDomain)

	// Premise 1: the read DOES state a correct leg-local identity — asked in the
	// leg's real layout, which is what a bake would index.
	id, stated := read.CorrelatedIdentityIn(legDomain)
	if !stated || id.Ordinal != 1 || id.Correlation != legA {
		t.Fatalf("the fixture is not the production shape: CorrelatedIdentityIn in the "+
			"leg's own layout gave (%+v, %v), want ordinal 1 on leg L. Every claim below "+
			"is about a read that already carries its ordinal; a fixture that does not "+
			"carry one tests something else.", id, stated)
	}

	// Premise 2: legSlotIdentity — the arm's OWN identity derivation — ANSWERS.
	// It reads its frontier off the child's stored type, so this is exactly what
	// typing the resolver's mints bought: measured, the corpus moved from
	// identityOtherDomain 126 / identityInLegDomain 0 to identityInLegDomain 126.
	if _, ok := legSlotIdentity(read); !ok {
		t.Fatal("legSlotIdentity DECLINED a read whose quantifier object states the " +
			"leg's own row. That decline used to be universal — the resolver minted " +
			"untyped children and all 126 corpus firings reported " +
			"qovTypeDomainKnown=false — and typing those mints is what removed it. A " +
			"decline here means the resolver stopped carrying the row, which puts the " +
			"whole identity channel back on the qualified name.")
	}

	// The pass-through: the SAME value comes back, so the ordinal, the domain and
	// the leg correlation are all intact by identity rather than by re-derivation.
	legTypes := map[values.CorrelationIdentifier]*values.RecordType{legA: aType}
	out := rebaseOuterLegValue(read, []string{"L"}, merged, nil, legTypes)
	if out != values.Value(read) {
		t.Fatalf("a read that ALREADY carried the right leg-local ordinal was rewritten "+
			"to %v.\n"+
			"  Whatever it was rewritten to, it can state at most what the read already\n"+
			"  stated, and the one thing this site used to produce — a lazy\n"+
			"  QOV(merged).\"L.CATEGORY\" — states strictly less: a name where an ordinal\n"+
			"  was. The merged RE-ANCHOR is the one legitimate rewrite here and it needs a\n"+
			"  merged layout, which this call path does not pass.", out)
	}
}
