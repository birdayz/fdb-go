package cascades

// Two decisions sit on the sort-elision path -- RichOrdering.Satisfies, and
// ImplementSortRule's `allCovered` loop that runs AFTER it says yes -- and this
// file pins where they agree and where they still do not.
//
// It previously recorded the opposite conclusion, and the correction is the
// point. The prediction was that teaching the providers to carry an ordinal
// would DIVERGE the two decisions: a provider that starts carrying its ordinal
// starts rendering `ID#0` where the request still renders `ID`, the coverage
// loop stops matching, and the sort survives a request the ordering property
// agreed was satisfied. That reasoning assumed the requested side was a bare
// name.
//
// MEASURED over the explaindiff corpus (2481 queries), counting how often the
// property said SATISFIED and the coverage loop then disagreed:
//
//	                        providers name-only    providers carry identity
//	  coverage AGREED                70                      922
//	  coverage DISAGREED            900                       47
//
// Exactly backwards. The REQUESTED side was already baked with its ordinal; it
// was the PROVIDER that rendered the bare name, so the coverage loop was failing
// 93% of the time BEFORE the conversion. Teaching the providers did not break the
// coverage check, it repaired it -- 900 disagreements down to 47 -- and the
// plan-shape golden moved 25 lines, not the 15657 the divergence theory implied.
//
// So the atomicity constraint this file was written to record is discharged, in
// the opposite direction from the one predicted: the two decisions are no longer
// coupled through a shared bake state. What survives is narrower, and the two
// tests below separate it:
//
//   - SAME correlation space: both sides state the same ordinal in the same
//     layout, both decisions resolve it, and they AGREE. This is the end state.
//   - DIFFERENT correlation space (a request rooted at a quantifier meeting a
//     source-local provided key): the property still answers yes -- through the
//     root-path bridge, which crosses the childless/qualified boundary by NAME --
//     while the coverage loop compares `C.ID#0` against `ID#0` and says no. That
//     is the whole of the residual, it carries 2445 of the corpus's key
//     resolutions, and closing it is a TRANSLATION problem (push the request into
//     the provider's space before matching, as Java does), not a naming one.
//
// The first test is the regression net: if a provider ever stops carrying its
// ordinal, it goes red and the 93% disagreement is back.
//
// ---------------------------------------------------------------------------
// THE CENSUS, AND WHERE IT LIVES
// ---------------------------------------------------------------------------
//
// This file is named as the harness for the CQ-55 corpus numbers, and it can only
// be half of one. A corpus census has to PLAN the corpus, the corpus harness
// (pkg/relational/conformance/explaindiff) imports this package, and an internal
// test in this package therefore cannot import it back — the cycle is
// structural, not stylistic. So the split is:
//
//   - the corpus-driven counts live in
//     pkg/relational/conformance/explaindiff/ordering_census_test.go, next to the
//     harness that can run the corpus, reading counters this package exports from
//     ordering_comparison_census.go;
//   - the white-box pins that need no corpus live here and in
//     ordering_comparator_dispatch_test.go.
//
// MEASURED AT THE TWO COMPARATOR SITES (this pass, one isolated corpus pass, 2485
// queries — the corpus has grown from the 2481 the numbers above were taken on).
// These are the AT-SITE counts, which no earlier census covered; the prior pass
// counted the merge path, the set membership and the resolving keyFor cells, and
// those are different cells:
//
//	                                  orderingValuesEqual   intersectionValuesEqual
//	  comparisons                            8659                    862
//	  both operands plain FieldValue         8658                    403
//	  ... and both state an identity         8658                    403
//	  DECLINE RESIDUAL                          0                      0
//	  non-FieldValue pairs                      1                    459
//	  decided by the NAME bridge alone           0                      0
//
// The decline residual is the number type dispatch depends on: a FieldValue pair
// where either side states no identity is DECLINED, with no fallthrough, so a
// nonzero residual is a lost set-operation merge. It was NOT zero when this pass
// started — 43 pairs at the intersection site, 12 of which the retired
// availability dispatch matched, every one of them a domained index key against a
// LAZY primary-key column. The three producers minting those lazy keys
// (TranslatePrimaryKeyToValues via the metadata index def, the PlanContext
// name-only fallback, and the primary-scan candidate's own PK values) are all
// layout-free by construction, so the bake went where the layout first exists:
// bakeIntersectionPrimaryKeyValues, over the partition's one common record row.
//
// The `decided by the NAME bridge alone` column is why the intersector's bridge
// arm is gone. At that site it is not merely unused, it is unreachable —
// CanBridgeOrderingFieldValues requires both operands to be FieldValues, and that
// class now returns from the identity arm above it.
//
// NUMBERS CARRIED FROM EARLIER PASSES, not re-measured here, with their source so
// the provenance is not lost: the merge/set-membership/keyFor zeros and the 61-of-61
// merge-path string decisions going to zero are recorded in commit cb2c8e6ec; the
// deleted ordinal-free bridge arm's 5289 → 0 is recorded at
// properties/rich_ordering.go, and its justification — that every pair the deleted
// arm could resolve the surviving root arm still resolves — is pinned by
// properties.TestOrderingKeyForStillResolvesAnUnstatedProvidedKeyByName rather
// than by a count.
//
// THE GROUPBY DECOMPOSITION, AND ITS NEGATIVE RESULT. The requested-ordering push
// through a GROUP BY was changed in the same pass as the identity work, and the
// question asked of it was whether its effect on these counts is just
// EXPLORATION VOLUME — more or fewer comparisons of the same kind. Re-censused
// with ONLY that change reverted and everything else in place:
//
//	                                  orderingValuesEqual   intersectionValuesEqual
//	  comparisons                       8659 -> 8675             862 -> 862
//	  DECLINE RESIDUAL                     0 -> 5                  0 -> 0
//	  ... of which matched before           0 -> 3                  0 -> 0
//	  plan-shape golden                    clean -> 3 lines differ
//
// It is NOT volume. Reverting it changes DECISIONS: five FieldValue pairs arrive
// with no identity, three of which used to match, and the plan-shape golden moves
// — the same query that motivated the change loses Fetch(IndexScan(IDX_CUST_STATUS))
// for InMemorySort(Scan(ORDERS)). So the two changes are COUPLED, not
// independent: pushing the GROUPING KEY (input space, domained) instead of the
// request part (output space) is a PRECONDITION of the decline residual being
// zero at orderingValuesEqual, because an output-space part has no identity the
// candidate side can meet. That coupling is the finding; it is not an
// exploration-volume artifact and must not be recorded as one.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// identityDecisionsLayout is the row both sides of each test resolve against.
func identityDecisionsLayout() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
}

// sortCoverageAgrees reproduces ImplementSortRule's `allCovered` loop over one
// provided ordering and one requested part, in the shape the rule computes it:
// a set built from the REQUESTED parts' renderings, probed with the renderings
// of the PROVIDED keys.
func sortCoverageAgrees(
	ordering *properties.RichOrdering,
	requested *properties.RequestedOrdering,
) bool {
	sortValueNames := map[string]struct{}{}
	for _, part := range requested.GetParts() {
		sortValueNames[values.ExplainValue(part.Value)] = struct{}{}
	}
	for _, key := range ordering.GetOrderingKeys() {
		if _, inSort := sortValueNames[values.ExplainValue(key)]; !inSort {
			return false
		}
	}
	return true
}

// TestOrderingSatisfactionAndSortCoverageAgreeInOneCorrelationSpace is the end
// state, and the net that keeps it.
//
// Both sides state ordinal 0 of the same layout. The provider carries it because
// it resolved its metadata column list against the row it flows; the request
// carries it because the translator baked it. Both decisions must resolve the
// pair, and they must give the SAME answer -- a property that says "satisfied"
// followed by a coverage loop that says "not covered" is a sort that survives a
// request the planner already proved was met.
func TestOrderingSatisfactionAndSortCoverageAgreeInOneCorrelationSpace(t *testing.T) {
	t.Parallel()

	layout := identityDecisionsLayout()
	domain := values.OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		t.Fatalf("test setup: %v has no layout token", layout)
	}

	// The provided side comes from a REAL provider, not a hand-built value. That
	// is what makes this a net rather than a restatement: a provider that stops
	// resolving its column list against the layout it flows puts the coverage
	// disagreement straight back, and a hand-built provided key could never
	// notice.
	ordering := plans.NewRecordQueryIndexPlan(
		"IDX_ID", nil, []string{"T"}, layout, false).
		WithIndexMetadata([]string{"ID"}, nil, false).
		WithStrictlySorted().
		HintRichOrdering()
	keys := ordering.GetKeys()
	if len(keys) != 1 {
		t.Fatalf("test setup: provider yielded %d ordering keys, want 1", len(keys))
	}
	provided := keys[0]

	// The requested side as the translator actually hands it over: baked, with
	// the same ordinal in the same layout. Measured over the corpus this is the
	// overwhelmingly common shape -- the bare-name request the old theory assumed
	// is essentially absent.
	requestedValue := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, domain)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedValue,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)

	if !ordering.Satisfies(requested) {
		t.Fatalf("the ordering property does not satisfy a request stating the "+
			"SAME ordinal in the SAME layout as the provided key (provided %q, "+
			"requested %q). These are the same column by every element of its "+
			"identity; refusing here is not conservatism, it is a lost elision on "+
			"the common path.",
			values.ExplainValue(provided), values.ExplainValue(requestedValue))
	}

	if !sortCoverageAgrees(ordering, requested) {
		t.Fatalf("ImplementSortRule's coverage loop DISAGREES with the ordering "+
			"property on a pair stating the same ordinal in the same layout "+
			"(provided %q, requested %q).\n\n"+
			"This is the 93%% disagreement the providers' name-only keys used to "+
			"cause: the property says the request is met, the coverage loop then "+
			"refuses to yield the strictly-sorted plan, and the sort survives "+
			"anyway. Measured, that was 900 of 970 corpus decisions; it is 47 once "+
			"the provider carries the ordinal the request already carried. A "+
			"provider that stops carrying it puts all 900 back.",
			values.ExplainValue(provided), values.ExplainValue(requestedValue))
	}
}

// TestCrossCorrelationIsTheWholeResidualDisagreement pins what is LEFT, so the
// remaining work is stated as a measured gap rather than a suspicion.
//
// The request is rooted at a quantifier; the provided key is source-local. Every
// element of the column identity agrees -- same ordinal, same layout -- and only
// the correlation differs, which is exactly what a translation step exists to
// reconcile. Until one runs, the property answers yes through the root-path
// bridge (a NAME comparison across the childless/qualified boundary) while the
// coverage loop compares the two renderings and answers no.
//
// Both halves are asserted. If the property ever stops answering yes here, 2445
// corpus key resolutions become misses and sorts appear; if the coverage loop
// ever starts agreeing, the residual is closed and this test should be replaced
// by one asserting the translation.
func TestCrossCorrelationIsTheWholeResidualDisagreement(t *testing.T) {
	t.Parallel()

	layout := identityDecisionsLayout()
	domain := values.OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		t.Fatalf("test setup: %v has no layout token", layout)
	}

	provided := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, domain)
	requestedValue := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("C")),
		"ID", 0, values.UnknownType, domain)

	providedIdent, providedOK := values.OrderingIdentityOf(provided)
	requestedIdent, requestedOK := values.OrderingIdentityOf(requestedValue)
	if !providedOK || !requestedOK {
		t.Fatalf("test setup: identities unavailable (provided ok=%v, requested "+
			"ok=%v); the point of this test is that they agree on everything "+
			"EXCEPT the correlation", providedOK, requestedOK)
	}
	if providedIdent.Ordinal != requestedIdent.Ordinal {
		t.Fatalf("test setup: ordinals differ (%d vs %d), so this is an ordinal "+
			"disagreement and not the correlation-space gap",
			providedIdent.Ordinal, requestedIdent.Ordinal)
	}
	if providedIdent.Correlation == requestedIdent.Correlation {
		t.Fatalf("test setup: both sides carry the same correlation, so there is " +
			"no correlation-space gap left to describe")
	}

	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			provided: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{provided},
		true,
	)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedValue,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)

	if !ordering.Satisfies(requested) {
		t.Fatalf("the ordering property no longer satisfies a request whose value "+
			"names the same column of the same layout through a quantifier "+
			"(provided %q, requested %q).\n\n"+
			"That is a real change to the sort-elision contract: this bridge "+
			"carries 2445 of the corpus's key resolutions, and it must keep "+
			"answering yes until a translation step puts the request into the "+
			"provider's correlation space -- which is what Java does before "+
			"matching, and what replaces this bridge.",
			values.ExplainValue(provided), values.ExplainValue(requestedValue))
	}

	if sortCoverageAgrees(ordering, requested) {
		t.Fatalf("ImplementSortRule's coverage loop now AGREES with the ordering "+
			"property across the correlation boundary (provided %q, requested "+
			"%q).\n\n"+
			"Good news, and it retires this test -- but only if they agree because "+
			"the request was TRANSLATED into the provider's space, not because two "+
			"renderings lined up again. Check which, then replace this test with "+
			"one asserting the translation itself.",
			values.ExplainValue(provided), values.ExplainValue(requestedValue))
	}
}
