package cascades

// The corpus census asserts ZEROS: no FieldValue pair states no identity, no
// root-wildcard bridge is ambiguous, the non-field name bridge decides nothing.
// A zero is only evidence if the thing counting it CAN count. A detector that
// never fires reports the same zero as an absent hazard, and it reports it
// forever, silently, while the assertion built on it reads as coverage.
//
// So every zero the corpus test asserts gets a reachability pin here: the exact
// shape that makes the counter increment, constructed by hand, asserted nonzero.
// Together the two halves say something the corpus zero cannot say alone —
// "the instrument works AND production does not hit it".
//
// These pins pass their OWN census struct to classifyOrderingComparison rather
// than toggling the package counters. The package counters are shared, every test
// here runs in parallel, and a NONZERO assertion over shared counters has none of
// the monotonicity protection a zero assertion has.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// censusWitnessRow is the one row layout the pins below bake against.
func censusWitnessRow(t *testing.T) values.OrdinalDomain {
	t.Helper()
	domain := values.OrdinalDomainOfType(values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	}))
	if !domain.IsKnown() {
		t.Fatalf("test setup: the witness row has no domain token")
	}
	return domain
}

// TestRootWildcardCensusCountsTheSelfJoinWitness is the reachability pin for the
// root-asymmetry bucket. It feeds the census exactly the triple the comparators
// are intransitive over — one childless key and two keys off different
// quantifiers, all three over the same column of the same layout — and asserts
// every counter in the bucket fires.
//
// Without this, the corpus's rootWildcardIntransitive=0 would be indistinguishable
// from a detector that classifies nothing. With it, the corpus zero means what it
// claims: the hazard is real, the instrument sees it, and production does not
// produce it.
func TestRootWildcardCensusCountsTheSelfJoinWitness(t *testing.T) {
	t.Parallel()

	domain := censusWitnessRow(t)
	outer := values.NamedCorrelationIdentifier("O")
	inner := values.NamedCorrelationIdentifier("I")

	childless := values.NewFieldValueWithResolvedOrdinalInDomain(
		"A", 0, values.UnknownType, domain)
	outerA := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(outer), "A", 0, values.UnknownType, domain)
	innerA := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(inner), "A", 0, values.UnknownType, domain)

	// The list the childless key is scanned against holds BOTH quantifiers' keys.
	// That is the only arrangement in which the intransitivity costs anything: it
	// makes membership in the list depend on the order it was built in.
	context := []values.Value{outerA, innerA}

	var c OrderingComparisonCensus
	classifyOrderingComparison(
		&c, childless, outerA, values.CanBridgeOrderingValueRoots, context)

	if c.FieldPairsDecided != 1 {
		t.Fatalf("the witness pair did not land in FieldPairsDecided (got %d).\n\n"+
			"That bucket is where the root asymmetry HIDES — all three of its "+
			"values state a column identity, so no dispatch change reaches it. If "+
			"the pair stopped landing there the bucket below is measuring something "+
			"else entirely.", c.FieldPairsDecided)
	}
	if c.RootWildcardBridges != 1 {
		t.Fatalf("the census did not see a root-wildcard bridge between the "+
			"childless key %q and %q (got %d).\n\n"+
			"Either values.SameOrderingColumn stopped bridging the zero correlation "+
			"— in which case the root-axis witness in "+
			"ordering_comparator_dispatch_test.go is now RED and the defect is "+
			"FIXED — or the detector stopped recognising the shape, and every "+
			"root-wildcard zero the corpus asserts is vacuous.",
			values.ExplainValue(childless), values.ExplainValue(outerA),
			c.RootWildcardBridges)
	}
	if c.RootWildcardNoContext != 0 {
		t.Errorf("the census called a bridge context-less despite being handed a "+
			"%d-element context (got %d)", len(context), c.RootWildcardNoContext)
	}
	if c.RootWildcardContextRooted != 1 {
		t.Errorf("the census did not see a quantifier-rooted key in a context that "+
			"holds two of them (got %d). This counter is what distinguishes "+
			"'contexts hold one root' from 'contexts hold none' when the corpus "+
			"reports a zero, so a dead one makes that zero uninterpretable.",
			c.RootWildcardContextRooted)
	}
	if c.RootWildcardMultiRoot != 1 {
		t.Errorf("the census did not see a SECOND distinct quantifier root in a "+
			"context holding %q and %q (got %d)",
			values.ExplainValue(outerA), values.ExplainValue(innerA),
			c.RootWildcardMultiRoot)
	}
	if c.RootWildcardIntransitive != 1 {
		t.Errorf("the census did not classify the witness triple as INTRANSITIVE "+
			"(got %d).\n\n"+
			"This is the exact hazard counter — the one the corpus test asserts is "+
			"zero. It must fire on the triple it was written for, or that assertion "+
			"proves nothing.", c.RootWildcardIntransitive)
	}
}

// TestRootWildcardCensusIgnoresASingleRootContext is the other direction: the
// bucket must NOT fire when the context holds only the quantifier the bridge
// already matched. A detector that counts every bridge would report the corpus's
// 876 bridges as 876 hazards and the assertion could never be satisfied.
func TestRootWildcardCensusIgnoresASingleRootContext(t *testing.T) {
	t.Parallel()

	domain := censusWitnessRow(t)
	outer := values.NamedCorrelationIdentifier("O")

	childless := values.NewFieldValueWithResolvedOrdinalInDomain(
		"A", 0, values.UnknownType, domain)
	outerA := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(outer), "A", 0, values.UnknownType, domain)
	outerB := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(outer), "B", 1, values.UnknownType, domain)

	var c OrderingComparisonCensus
	classifyOrderingComparison(
		&c, childless, outerA, values.CanBridgeOrderingValueRoots,
		[]values.Value{outerA, outerB})

	if c.RootWildcardBridges != 1 {
		t.Fatalf("test setup: expected one root-wildcard bridge, got %d",
			c.RootWildcardBridges)
	}
	if c.RootWildcardContextRooted != 1 {
		t.Errorf("a context of two keys off quantifier O was not counted as rooted "+
			"(got %d)", c.RootWildcardContextRooted)
	}
	if c.RootWildcardMultiRoot != 0 || c.RootWildcardIntransitive != 0 {
		t.Errorf("a context holding only the MATCHED quantifier O was classified as "+
			"ambiguous (multiRoot=%d intransitive=%d).\n\n"+
			"There is no triple here: one root cannot disagree with itself. A "+
			"detector this loose makes the corpus assertion unsatisfiable and would "+
			"be silenced rather than fixed.",
			c.RootWildcardMultiRoot, c.RootWildcardIntransitive)
	}
}

// TestRootWildcardCensusCountsAMissingContext pins the instrument's own
// non-vacuity guard. A comparator call site added without threading the list it
// scans would shrink the population the ambiguity zero covers, silently. The
// corpus test asserts this counter is zero; this asserts it can be nonzero.
func TestRootWildcardCensusCountsAMissingContext(t *testing.T) {
	t.Parallel()

	domain := censusWitnessRow(t)
	childless := values.NewFieldValueWithResolvedOrdinalInDomain(
		"A", 0, values.UnknownType, domain)
	outerA := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("O")),
		"A", 0, values.UnknownType, domain)

	var c OrderingComparisonCensus
	classifyOrderingComparison(
		&c, childless, outerA, values.CanBridgeOrderingValueRoots, nil)

	if c.RootWildcardBridges != 1 {
		t.Fatalf("test setup: expected one root-wildcard bridge, got %d",
			c.RootWildcardBridges)
	}
	if c.RootWildcardNoContext != 1 {
		t.Errorf("a bridge classified with NO comparison context was not counted as "+
			"context-less (got %d).\n\n"+
			"The corpus test asserts this counter is zero, which is how it proves "+
			"every call site threads the list it scans. If the counter cannot fire, "+
			"that proof is empty and a site added without a context would pass.",
			c.RootWildcardNoContext)
	}
	if c.RootWildcardMultiRoot != 0 || c.RootWildcardIntransitive != 0 {
		t.Errorf("a context-less bridge was also classified as ambiguous "+
			"(multiRoot=%d intransitive=%d); it must be counted as UNMEASURED "+
			"instead of guessed either way",
			c.RootWildcardMultiRoot, c.RootWildcardIntransitive)
	}
}

// TestNonFieldBridgeOnlyCensusReachesTheCardinalityWrapper is the reachability pin
// for NonFieldBridgeOnly.
//
// orderingValuesEqual keeps the ordinal-free NAME bridge below its structural arm
// on the stated grounds that a CardinalityValue-wrapped pair still crosses it,
// even though the FieldValue class never can any more. The corpus reports that
// count as zero, and a zero is exactly what a DELETED bridge would report too. So
// the shape is constructed here: CARDINALITY of a field read off a quantifier
// against CARDINALITY of the same column read source-locally. Structural equality
// separates them (different child roots); the name bridge reconciles them.
//
// The pair is outside the FieldValue type class, which is the whole point — it is
// the population that justifies keeping the arm. If this pin ever goes red, the
// bridge is genuinely unreachable and the honest move is to DELETE it, not to
// weaken the corpus assertion.
func TestNonFieldBridgeOnlyCensusReachesTheCardinalityWrapper(t *testing.T) {
	t.Parallel()

	rooted := values.NewCardinalityValue(values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("O")),
		"A", values.NullableLong))
	sourceLocal := values.NewCardinalityValue(values.NewFieldValue(
		nil, "A", values.NullableLong))

	if values.OrderingFieldPair(rooted, sourceLocal) {
		t.Fatalf("a CardinalityValue pair is inside the FieldValue type class, so " +
			"the identity arm decides it and the name bridge below is genuinely " +
			"unreachable. Delete the bridge rather than counting it.")
	}
	if values.ValuesStructurallyEqual(rooted, sourceLocal) {
		t.Fatalf("test setup: structural equality already reconciles %q and %q, so "+
			"the bridge is not the deciding arm and this pin measures nothing",
			values.ExplainValue(rooted), values.ExplainValue(sourceLocal))
	}

	var c OrderingComparisonCensus
	classifyOrderingComparison(
		&c, rooted, sourceLocal, values.CanBridgeOrderingValueRoots, nil)

	if c.NonFieldPairs != 1 {
		t.Fatalf("the CardinalityValue pair was not counted outside the FieldValue "+
			"class (got %d)", c.NonFieldPairs)
	}
	if c.NonFieldBridgeOnly != 1 {
		t.Errorf("the name bridge did NOT decide the CardinalityValue pair %q vs "+
			"%q (got %d).\n\n"+
			"That is the only population left that reaches the bridge, and the "+
			"corpus asserts the count is zero there. With this pin red, the corpus "+
			"zero says the arm is DEAD rather than merely unexercised — at which "+
			"point remove the arm from orderingValuesEqual instead of keeping a "+
			"fallback that cannot fire.",
			values.ExplainValue(rooted), values.ExplainValue(sourceLocal),
			c.NonFieldBridgeOnly)
	}

	// The comparator itself must agree with the census's classification: the
	// census derives the arms independently, so a disagreement means one of the two
	// is describing a comparator that no longer exists.
	if !orderingValuesEqual(rooted, sourceLocal) {
		t.Errorf("the census says the name bridge decides %q vs %q EQUAL, but "+
			"orderingValuesEqual says unequal. The census derives its arms "+
			"independently of the site, so the two have drifted.",
			values.ExplainValue(rooted), values.ExplainValue(sourceLocal))
	}
}
