package cascades

// RichOrdering.orderingKeyFor is NOT the only name-keyed identity decision on
// the sort-elision path, and this file exists because that was assumed and is
// false.
//
// The ordering property addresses its set by a rendered string, and CQ-55
// converts that match to a column identity. The conversion looked separable
// from its providers: teach the providers to hand out keys that carry an
// ordinal and a domain, and the string bridges in orderingKeyFor go dead on
// their own. Measured over the plandiff corpus, that half is exactly true --
// resolving the index-scan and primary-scan providers against the layout they
// flow drove the normalized-name bridge from 7307 uses to 6, and drove the
// structural-identity hit rate from 90 to 8219, with every test green.
//
// Over the explaindiff corpus the same change kept ordering satisfaction flat
// (63005 satisfied before, 63245 after; 50070 misses before, 49870 after) and
// still moved the plan-shape golden by 15657 lines, every one of them a sort
// APPEARING. Satisfaction had not dropped, so the sorts did not come from the
// ordering property refusing anything. They came from here.
//
// ImplementSortRule decides two further things by RENDERED NAME, after
// RichOrdering has already said yes (rule_implement_sort.go, the
// `sortValueNames` / `eqBoundNames` sets and the `allCovered` loop over
// GetOrderingKeys). Those sets are built with values.ExplainValue from the
// REQUESTED parts and probed with values.ExplainValue of the PROVIDED keys. A
// provider that starts carrying its ordinal starts rendering `ID#0` where the
// request still renders `ID`, the coverage loop stops matching, and the
// strictlySorted plan is never yielded -- so the sort survives a request the
// ordering property agreed was satisfied.
//
// The consequence for the conversion is a sequencing constraint, and it is the
// point of this file: the providers, orderingKeyFor, and these coverage sets
// are ONE atomic change. Converting the providers alone does not lose
// satisfaction, which is what makes the failure mode so quiet -- the property
// keeps agreeing while the plan quietly grows a sort.
//
// This test pins the DISAGREEMENT while it exists. It goes red the moment the
// two decisions converge, which is the signal that the atomicity constraint has
// been discharged and this file can go.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestOrderingSatisfactionAndSortCoverageDoNotAgreeOnIdentity(t *testing.T) {
	t.Parallel()

	layout := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
	domain := values.OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		t.Fatalf("test setup: %v has no layout token", layout)
	}

	// The PROVIDED key as a provider that knows its layout would mint it: the
	// column's ordinal in the row the scan flows, plus the token naming that
	// row. This is the shape CQ-55 moves every provider to.
	provided := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, domain)

	// The REQUESTED key as a sort expression carries it today: a bare name.
	requestedValue := values.NewFlatFieldValue("ID", values.UnknownType)

	// The two renderings differ, which is the whole mechanism. If this ever
	// stops being true the rest of the test is vacuous, so it is asserted
	// rather than assumed.
	providedRendering := values.ExplainValue(provided)
	requestedRendering := values.ExplainValue(requestedValue)
	if providedRendering == requestedRendering {
		t.Fatalf("test setup: both sides render as %q, so no rendering "+
			"difference exists to disagree about", providedRendering)
	}

	rich := properties.NewRichOrdering(
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

	// DECISION ONE: the ordering property. It reconciles the two
	// representations through one of orderingKeyFor's bridge arms and agrees
	// the request is satisfied. Which arm was measured by deleting them one at
	// a time: the normalized same-column arm is NOT the one that carries this
	// pair -- removing it alone changes nothing here, and the full-accessor-path
	// root arm is what answers. Both have to go for the answer to change, which
	// is why the item deletes them together rather than one at a time.
	if !rich.Satisfies(requested) {
		t.Fatalf("the ordering property no longer satisfies a request whose "+
			"value names the same column in a different representation "+
			"(provided %q, requested %q).\n"+
			"That is a real change to the sort-elision contract, not a test "+
			"nit: this is the bridge CQ-55 replaces with a column identity, "+
			"and it must keep answering yes until the identity replaces it.",
			providedRendering, requestedRendering)
	}

	// DECISION TWO: ImplementSortRule's strictlySorted coverage check, which
	// runs AFTER the above says yes and is keyed on the rendering rather than
	// on the ordering property. Reproduced here in the shape the rule computes
	// it, over the same two values.
	sortValueNames := map[string]struct{}{requestedRendering: {}}
	covered := true
	for _, key := range rich.GetOrderingKeys() {
		if _, inSort := sortValueNames[values.ExplainValue(key)]; !inSort {
			covered = false
			break
		}
	}

	if covered {
		t.Fatalf("the ordering property and ImplementSortRule's coverage check "+
			"now AGREE on this pair (provided %q, requested %q).\n\n"+
			"That is good news and it retires this test -- but only if they "+
			"agree because both decide on column identity, not because a "+
			"rendering happened to line up again. Check which, then delete "+
			"this file and the sequencing constraint it records: the "+
			"providers, orderingKeyFor and these coverage sets no longer have "+
			"to move together.",
			providedRendering, requestedRendering)
	}

	// The disagreement itself, stated as the thing under contract: one
	// decision said satisfied, the other said not covered, about the same two
	// values. Every sort in the 15657-line golden movement is this gap.
	t.Logf("ordering property: SATISFIED; sort coverage: NOT COVERED "+
		"(provided %q vs requested %q) -- the two identity decisions on the "+
		"sort-elision path disagree, so they must be converted together",
		providedRendering, requestedRendering)
}
