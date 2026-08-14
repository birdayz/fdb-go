package cascades

// The corpus census asserts ZEROS: no FieldValue pair states no identity, and
// the non-field name bridge decides nothing. A zero is only evidence if the
// thing counting it CAN count. A detector that never fires reports the same
// zero as an absent hazard, and it reports it forever, silently, while the
// assertion built on it reads as coverage.
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

// censusWitnessRow is the one exact row layout the pins below resolve against.
func censusWitnessRow(t testing.TB) *values.RecordType {
	t.Helper()
	return values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
}

func censusWitnessField(
	t testing.TB,
	alias values.CorrelationIdentifier,
	ordinal int,
) values.Value {
	t.Helper()
	root, rootErr := values.NewQuantifiedObjectValue(alias, censusWitnessRow(t))
	root = mustConstruct(t, root, rootErr)
	field, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	return mustConstruct(t, field, err)
}

// TestNonFieldBridgeOnlyCensusReachesTheCardinalityWrapper is the reachability pin
// for NonFieldBridgeOnly.
//
// orderingValuesEqualIn keeps the ordinal-free NAME bridge below its structural arm
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

	rowType := censusWitnessRow(t)
	currentLayout, layoutErr := values.NewOrdinalLayoutForCarrierType(
		rowType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}},
		nil,
	)
	currentLayout = mustConstruct(t, currentLayout, layoutErr)
	currentField, currentFieldErr := values.ResolveFieldOrdinals(currentLayout.Carrier(), []int{0})
	currentField = mustConstruct(t, currentField, currentFieldErr)
	namedField := censusWitnessField(t, values.NamedCorrelationIdentifier("O"), 0)
	rooted := values.NewCardinalityValue(currentField)
	sourceLocal := values.NewCardinalityValue(namedField)

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
		&c, rooted, sourceLocal, values.CanBridgeOrderingValueRoots)

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
			"point remove the arm from orderingValuesEqualIn instead of keeping a "+
			"fallback that cannot fire.",
			values.ExplainValue(rooted), values.ExplainValue(sourceLocal),
			c.NonFieldBridgeOnly)
	}

	// The comparator itself must agree with the census's classification: the
	// census derives the arms independently, so a disagreement means one of the two
	// is describing a comparator that no longer exists.
	if !orderingValuesEqualIn(rooted, sourceLocal) {
		t.Errorf("the census says the name bridge decides %q vs %q EQUAL, but "+
			"orderingValuesEqualIn says unequal. The census derives its arms "+
			"independently of the site, so the two have drifted.",
			values.ExplainValue(rooted), values.ExplainValue(sourceLocal))
	}
}
