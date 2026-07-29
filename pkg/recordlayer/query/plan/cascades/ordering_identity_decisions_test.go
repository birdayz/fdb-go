package cascades

// Two decisions sit on the sort-elision path -- RichOrdering.Satisfies, and
// ImplementSortRule's `allCovered` loop that runs AFTER it says yes -- and this
// file pins where they agree, where they still do not, and by WHICH mechanism
// each pair is resolved.
//
// The file previously recorded the opposite conclusion, and the correction is
// part of the record. The prediction was that teaching the providers to carry an
// ordinal would DIVERGE the two decisions: a provider that starts rendering
// `ID#0` where the request still renders `ID` stops matching in the coverage
// loop, and the sort survives a request the ordering property agreed was
// satisfied. That reasoning assumed the requested side was a bare name. It was
// not -- the REQUESTED side was already baked, and it was the PROVIDER rendering
// the bare name, so the conversion repaired the coverage loop instead of
// breaking it (measured over the explaindiff corpus: 900 disagreements down to
// 47, against a plan-shape golden that moved 25 lines rather than the 15657 the
// divergence theory implied).
//
// WHAT THIS FILE PINS, and why it is a table rather than the two numbers.
//
// The 900/47 census was evidence for a CLASSIFICATION: which representation
// pairs each decision resolves, and through which of the three mechanisms. The
// numbers themselves are a property of one corpus at one commit and cannot be
// asserted from a unit test; the classification can, and it is the thing the
// numbers were evidence FOR. So the census is narrowed here into one row per
// class, each row asserting all three facts -- which mechanism resolves the pair,
// whether the ordering property says satisfied, and whether the coverage loop
// agrees. A class whose verdict moves fails with its own name attached.
//
// The corpus tallies behind the table, re-measured at this commit with stderr
// counters at each exit of `RichOrdering.orderingKeyFor` and each arm of
// `orderingValuesEqual`, over `explaindiff.GenerateBaseline` (2489 queries):
//
//	orderingKeyFor            exact 10150   normalized 76   root-path 2445   missed 10017
//	orderingValuesEqual       structural 0                  root-path 4189
//
// Read honestly, that is a LARGE residual, not a closed one: 2521 of the 12671
// resolutions orderingKeyFor makes are name-keyed -- 19.9%, through BOTH bridge
// arms -- and every one of the 4189 successful comparisons in
// SatisfiesRequestedOrdering's own path is name-keyed, with zero resolved
// structurally. "The normalized-name bridge is essentially dead" is true of that
// ONE arm (76 uses, 0.6%) and says nothing about the root-path arm beside it,
// which carries 19.3% on its own and is reachable from both decision paths. The
// residual is a correlation-space mismatch, and closing it is a TRANSLATION
// problem -- push the request into the provider's space before matching, as Java
// does -- not a naming one.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// identityDecisionsLayout is the row both sides of each test resolve against.
func identityDecisionsLayout() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
}

// sortCoverageAgrees runs ImplementSortRule's ACTUAL coverage decision.
//
// It builds the two key sets through the rule's own helpers and calls the rule's
// own predicate, so this cannot drift from what the planner does. The earlier
// version of this file reimplemented the loop and dropped the `inEq` disjunct
// (rule_implement_sort.go's mixed-binding arm) -- so it tested its own copy, the
// equality-bound class below was invisible to it, and its retirement trigger
// could never fire on a change to the real loop.
func sortCoverageAgrees(
	ordering *properties.RichOrdering,
	requested *properties.RequestedOrdering,
) bool {
	return sortCoverageAllCovered(
		ordering,
		sortRequestedNames(requested.GetParts()),
		sortEqualityBoundNames(ordering),
	)
}

// requestOf wraps one value as an ascending requested ordering.
func requestOf(v values.Value) *properties.RequestedOrdering {
	return properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     v,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
}

// sortedOrderingOf wraps one value as a provided ascending ordering.
func sortedOrderingOf(v values.Value) *properties.RichOrdering {
	return properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			v: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{v},
		true,
	)
}

// resolutionMechanism names which of the three paths in
// RichOrdering.orderingKeyFor can resolve a (requested, provided) pair. Asserted
// per class so a row cannot silently change WHICH channel carries it -- the
// difference between "resolved structurally" and "resolved by name" is the whole
// subject of this file.
type resolutionMechanism string

const (
	mechExact      resolutionMechanism = "exact rendering"
	mechNormalized resolutionMechanism = "normalized-name bridge"
	mechRootPath   resolutionMechanism = "root-path bridge"
	mechNone       resolutionMechanism = "no mechanism"
)

// mechanismFor classifies a pair the same way orderingKeyFor's arms are ordered:
// exact rendering first, then the normalized-name bridge, then the
// full-accessor-path root bridge.
func mechanismFor(requested, provided values.Value) resolutionMechanism {
	if values.ExplainValue(requested) == values.ExplainValue(provided) {
		return mechExact
	}
	if values.CanBridgeOrderingFieldValues(requested, provided) {
		return mechNormalized
	}
	if values.CanBridgeOrderingValueRoots(requested, provided) {
		return mechRootPath
	}
	return mechNone
}

// TestOrderingDecisionClasses is the narrowed census: one row per
// representation class the two decisions can meet in, each row asserting the
// mechanism that resolves it and both decisions' verdicts.
func TestOrderingDecisionClasses(t *testing.T) {
	t.Parallel()

	layout := identityDecisionsLayout()
	domain := values.OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		t.Fatalf("test setup: %v has no layout token", layout)
	}
	otherDomain := values.OrdinalDomainOfColumnNames([]string{"ID", "V", "W"})
	if !otherDomain.IsKnown() || otherDomain == domain {
		t.Fatalf("test setup: need a SECOND, distinct layout token")
	}

	baked := func(name string, ordinal int, d values.OrdinalDomain) values.Value {
		return values.NewFieldValueWithResolvedOrdinalInDomain(
			name, ordinal, values.UnknownType, d)
	}
	lazy := func(name string) values.Value {
		return &values.FieldValue{Field: name, Typ: values.UnknownType}
	}
	qualified := func(alias, name string, ordinal int) values.Value {
		return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
			name, ordinal, values.UnknownType, domain)
	}

	for _, tc := range []struct {
		// name states the class, and the failure message quotes it.
		name string
		// requested and provided are the two independently constructed values.
		requested, provided values.Value
		// mechanism is which arm of orderingKeyFor can carry the pair.
		mechanism resolutionMechanism
		// satisfies is the ordering property's verdict.
		satisfies bool
		// covered is the coverage loop's verdict, which runs after.
		covered bool
		// why records what a change to this row would mean.
		why string
	}{
		{
			name:      "same identity, both source-local",
			requested: baked("ID", 0, domain),
			provided:  baked("ID", 0, domain),
			mechanism: mechExact,
			satisfies: true,
			covered:   true,
			why: "the end state: both sides state the same ordinal in the same " +
				"layout, both decisions resolve it structurally, and they agree. " +
				"This is the class the provider conversion moved 900 corpus " +
				"decisions INTO.",
		},
		{
			name:      "provider still lazy, request baked",
			requested: baked("ID", 0, domain),
			provided:  lazy("ID"),
			mechanism: mechNormalized,
			satisfies: true,
			covered:   false,
			why: "the class the provider conversion moved OUT of, and the reason " +
				"the two decisions used to disagree 93% of the time: the property " +
				"bridges the bake-state seam by NAME while the coverage loop " +
				"compares `ID#0` against `ID` and refuses. A provider that stops " +
				"carrying its ordinal puts every one of those back.",
		},
		{
			name:      "same identity, request rooted at a quantifier",
			requested: qualified("C", "ID", 0),
			provided:  baked("ID", 0, domain),
			mechanism: mechRootPath,
			satisfies: true,
			covered:   false,
			why: "the WHOLE of the residual, and it is large -- 2445 of the " +
				"corpus's 12671 orderingKeyFor resolutions, plus all 4189 " +
				"successful comparisons in SatisfiesRequestedOrdering's own path. " +
				"Every element of the identity agrees except the correlation, " +
				"which is exactly what a translation step exists to reconcile. " +
				"Until one runs the property must keep answering yes; if the " +
				"coverage loop ever starts agreeing, check that it agrees because " +
				"the request was TRANSLATED into the provider's space and not " +
				"because two renderings lined up again.",
		},
		{
			name:      "same name and layout, DIFFERENT ordinals",
			requested: baked("ID", 0, domain),
			provided:  baked("ID", 1, domain),
			mechanism: mechNone,
			satisfies: false,
			covered:   false,
			why: "two different slots of ONE layout. No arm may bridge these: " +
				"within a single layout a name maps to exactly one ordinal, so " +
				"disagreeing ordinals mean disagreeing columns. Admitting the " +
				"pair is the ordinal conflation the domain token exists to " +
				"prevent.",
		},
		{
			name:      "same name and ordinal, DIFFERENT layouts",
			requested: baked("ID", 0, otherDomain),
			provided:  baked("ID", 0, domain),
			mechanism: mechExact,
			satisfies: true,
			covered:   true,
			why: "the seam the ordinal domain does NOT close today, recorded " +
				"rather than glossed: both sides render `ID#0`, so the EXACT arm " +
				"carries the pair before any domain is consulted, and the two " +
				"layouts are never compared. A projection narrowing a layout is " +
				"the common source (4 such pairs in the corpus, all genuinely the " +
				"same column). Closing it means comparing identities instead of " +
				"renderings, which is what the ordering-set conversion is for.",
		},
	} {
		if got := mechanismFor(tc.requested, tc.provided); got != tc.mechanism {
			t.Errorf("class %q: resolved by %q, want %q (requested %q, provided %q).\n\n"+
				"The CHANNEL changed, even if the verdict did not. %s",
				tc.name, got, tc.mechanism,
				values.ExplainValue(tc.requested), values.ExplainValue(tc.provided), tc.why)
		}
		ordering := sortedOrderingOf(tc.provided)
		requested := requestOf(tc.requested)
		if got := ordering.Satisfies(requested); got != tc.satisfies {
			t.Errorf("class %q: RichOrdering.Satisfies = %v, want %v "+
				"(requested %q, provided %q).\n\n%s",
				tc.name, got, tc.satisfies,
				values.ExplainValue(tc.requested), values.ExplainValue(tc.provided), tc.why)
		}
		if got := sortCoverageAgrees(ordering, requested); got != tc.covered {
			t.Errorf("class %q: ImplementSortRule's coverage loop = %v, want %v "+
				"(requested %q, provided %q).\n\n"+
				"A property that says SATISFIED followed by a coverage loop that "+
				"says NOT COVERED is a sort that survives a request the planner "+
				"already proved was met. %s",
				tc.name, got, tc.covered,
				values.ExplainValue(tc.requested), values.ExplainValue(tc.provided), tc.why)
		}
	}
}

// TestSortCoverageCountsMixedBindingKeys pins the `inEq` disjunct of the real
// coverage loop.
//
// Getting this test right took a mutation to establish, and the first attempt is
// worth recording because it is the trap the disjunct sits in. An
// equality-PREFIXED scan (`WHERE status = 'x' ORDER BY id`, provided STATUS fixed
// + ID sorted) does NOT reach the disjunct at all: GetOrderingKeys excludes an
// all-fixed key, so STATUS never enters the loop and ID is covered by the request
// alone. Deleting `inEq` leaves that shape — and the whole explaindiff corpus —
// green.
//
// The shape that reaches it is a MIXED binding: ONE key carrying both a fixed and
// a sorted binding, which composition produces (concatenating a leg whose key is
// equality-bound with one where the same key is sorted). GetOrderingKeys INCLUDES
// it, because not all its bindings are fixed, while GetEqualityBoundValues also
// includes it, because at least one is. So the key is in the loop and is not in
// the request, and only `inEq` covers it.
//
// Constructed directly rather than through a provider, because no single provider
// mints a mixed binding — the composition operators do. That is also why the
// corpus cannot see this axis, which is exactly why it needs its own test rather
// than trusting the golden.
func TestSortCoverageCountsMixedBindingKeys(t *testing.T) {
	t.Parallel()

	domain := values.OrdinalDomainOfType(identityDecisionsLayout())
	if !domain.IsKnown() {
		t.Fatalf("test setup: the layout has no token")
	}
	mixed := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, domain)
	requestedOnly := values.NewFieldValueWithResolvedOrdinalInDomain(
		"V", 1, values.UnknownType, domain)

	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	merged := predicates.EmptyComparisonRange().Merge(&cmp)
	if !merged.Ok {
		t.Fatalf("test setup: could not build an equality comparison range")
	}
	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			mixed: {
				properties.FixedBinding(merged.Range),
				properties.SortedBinding(properties.ProvidedSortOrderAscending),
			},
			requestedOnly: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{mixed, requestedOnly},
		true,
	)

	// The premise, asserted so the test cannot silently stop reaching the
	// disjunct the way its first version did.
	inOrderingKeys := false
	for _, k := range ordering.GetOrderingKeys() {
		if values.ExplainValue(k) == values.ExplainValue(mixed) {
			inOrderingKeys = true
		}
	}
	if !inOrderingKeys {
		t.Fatalf("test setup: the mixed-binding key %q is NOT in GetOrderingKeys, "+
			"so it never enters the coverage loop and this test does not reach the "+
			"inEq disjunct", values.ExplainValue(mixed))
	}
	if _, isEqBound := sortEqualityBoundNames(ordering)[values.ExplainValue(mixed)]; !isEqBound {
		t.Fatalf("test setup: the mixed-binding key %q is not equality-bound, so "+
			"inEq cannot be what covers it", values.ExplainValue(mixed))
	}

	// The request names ONLY the other key. The mixed key is therefore covered
	// by inEq or not at all.
	requested := requestOf(requestedOnly)
	if _, inSort := sortRequestedNames(requested.GetParts())[values.ExplainValue(mixed)]; inSort {
		t.Fatalf("test setup: the request names the mixed key %q, so inSort covers "+
			"it and inEq is not under test", values.ExplainValue(mixed))
	}

	if !sortCoverageAgrees(ordering, requested) {
		t.Fatalf("the coverage loop refuses a provided ordering whose only " +
			"unrequested key carries a MIXED binding (one fixed, one sorted).\n\n" +
			"The `inEq` disjunct is what covers it: at least one binding is fixed, " +
			"so the key does not constrain anything the sort must reproduce. " +
			"Dropping the disjunct refuses every composed ordering that fixes a key " +
			"on one side and sorts it on the other, and the strictly-sorted yield is " +
			"lost with it.")
	}
}
