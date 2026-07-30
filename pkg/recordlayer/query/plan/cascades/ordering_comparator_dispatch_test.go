package cascades

// The two ordering-value comparators dispatch by VALUE TYPE: a pair of plain
// FieldValues is decided by column identity and by nothing else. This file is the
// net under that dispatch, and it exists because deleting the identity arm from
// intersectionValuesEqual used to leave the whole suite GREEN — fifteen of
// fifteen intersection tests passed with the gate gone, so nothing in the tree
// was testing the dimension the gate protects.
//
// The gap was DIMENSIONAL, not volumetric. Every existing intersection test
// builds its keys against ONE row layout, and inside one layout the identity arm
// and the domain-blind structural arm agree on every pair. The defect needs TWO
// layouts whose ordinals COLLIDE, and no test constructed that, so no test could
// see the gate's absence. The shape below is white-box for exactly that reason:
// two same-slot keys in two different layouts do arise from real plans (an
// aggregate's output row beside a record row), but a corpus query cannot isolate
// the single comparison, and a test that cannot isolate the defect is not a net
// for it.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// twoLayoutOrdinalCollision returns two baked ordering keys that occupy the SAME
// ordinal in DIFFERENT row layouts — the shape every assertion in this file
// needs. They are different columns of different rows by construction, and the
// only thing distinguishing them is the layout token.
func twoLayoutOrdinalCollision(t *testing.T) (recordRowKey, aggregateRowKey values.Value) {
	t.Helper()

	// A record row: ID at slot 0.
	recordRow := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "STATUS", FieldType: values.NullableLong, Ordinal: 1},
	})
	// A streaming aggregate's OUTPUT row: a different column at slot 0.
	aggregateRow := values.NewRecordType("", false, []values.Field{
		{Name: "CUSTOMER_ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "COUNT", FieldType: values.NullableLong, Ordinal: 1},
	})

	recordDomain := values.OrdinalDomainOfType(recordRow)
	aggregateDomain := values.OrdinalDomainOfType(aggregateRow)
	if !recordDomain.IsKnown() || !aggregateDomain.IsKnown() {
		t.Fatalf("test setup: a layout has no domain token (record known=%v, "+
			"aggregate known=%v)", recordDomain.IsKnown(), aggregateDomain.IsKnown())
	}
	if recordDomain == aggregateDomain {
		t.Fatalf("test setup: both layouts produced the SAME domain token, so " +
			"there is no cross-layout collision to test. The two column-name " +
			"lists must differ for OrdinalDomainOfType to separate them.")
	}

	recordRowKey = values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, recordDomain)
	aggregateRowKey = values.NewFieldValueWithResolvedOrdinalInDomain(
		"CUSTOMER_ID", 0, values.UnknownType, aggregateDomain)

	// The premise the whole file rests on, asserted rather than assumed: the
	// structural comparison CANNOT tell these apart. If this ever stops being
	// true the identity arm is no longer load-bearing and every assertion below
	// becomes vacuous.
	if !values.ValuesStructurallyEqual(recordRowKey, aggregateRowKey) {
		t.Fatalf("test setup: ValuesStructurallyEqual now SEPARATES ordinal 0 of "+
			"two different layouts (%q vs %q).\n\n"+
			"That is the conflation this file's gate exists to prevent, so if the "+
			"structural comparison started consulting the layout the gate is "+
			"redundant — but every test below would then pass for the wrong "+
			"reason. Find out what changed before deleting anything.",
			values.ExplainValue(recordRowKey), values.ExplainValue(aggregateRowKey))
	}

	return recordRowKey, aggregateRowKey
}

// TestIntersectionComparatorSeparatesSameSlotDifferentLayouts is the net for the
// identity arm of intersectionValuesEqual. Deleting that arm makes this test
// RED; before it existed, deleting the arm left the suite green.
func TestIntersectionComparatorSeparatesSameSlotDifferentLayouts(t *testing.T) {
	t.Parallel()

	recordRowKey, aggregateRowKey := twoLayoutOrdinalCollision(t)

	if intersectionValuesEqual(recordRowKey, aggregateRowKey) {
		t.Fatalf("intersectionValuesEqual says ordinal 0 of a record row (%q) and "+
			"ordinal 0 of an aggregate output row (%q) are the SAME COLUMN.\n\n"+
			"They are different columns of different rows. The comparator reached "+
			"the domain-blind structural arm, which compares ordinals and never "+
			"the layout they index, so any two rows whose slots line up are "+
			"conflated. Dispatch must be by TYPE: both operands are FieldValues, "+
			"so column identity decides and the decision is FINAL.",
			values.ExplainValue(recordRowKey), values.ExplainValue(aggregateRowKey))
	}

	// The other direction, so a gate that accidentally declines EVERYTHING does
	// not pass this file: the same ordinal in the SAME layout is the same column.
	sameLayoutTwin := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType,
		recordRowKey.(*values.FieldValue).Resolved.Domain)
	if !intersectionValuesEqual(recordRowKey, sameLayoutTwin) {
		t.Fatalf("intersectionValuesEqual refuses two keys stating the SAME "+
			"ordinal in the SAME layout (%q vs %q).\n\n"+
			"A comparator that declines everything satisfies the separation "+
			"assertion above while destroying every intersection merge. Both "+
			"halves have to hold.",
			values.ExplainValue(recordRowKey), values.ExplainValue(sameLayoutTwin))
	}
}

// TestFreePrimaryKeyProofSeparatesSameSlotDifferentLayouts carries the same gate
// through to the decision that CONSUMES it, so the net covers the consequence and
// not only the predicate.
//
// comparisonKeyContainsFreePrimaryKey proves that every primary-key column not
// already fixed by an equality appears in the intersection's comparison key. That
// proof is what makes the merge sound: the comparison key is how the legs are
// aligned, so a PK column missing from it means two distinct records compare
// equal and the intersection drops or duplicates rows. Under a domain-blind
// comparator the proof SUCCEEDS on a comparison key that does not contain the PK
// at all — it merely contains something at the same slot of another row.
func TestFreePrimaryKeyProofSeparatesSameSlotDifferentLayouts(t *testing.T) {
	t.Parallel()

	pkKey, foreignRowKey := twoLayoutOrdinalCollision(t)

	if comparisonKeyContainsFreePrimaryKey(
		[]values.Value{foreignRowKey}, // the comparison key: another row's slot 0
		[]values.Value{pkKey},         // the primary key: this row's slot 0
		nil,                           // nothing is equality-bound
	) {
		t.Fatalf("comparisonKeyContainsFreePrimaryKey accepted a comparison key "+
			"of %q as containing the primary-key column %q.\n\n"+
			"It does not contain it — the two agree only on their ORDINAL, in two "+
			"different row layouts. An intersection built on this proof aligns its "+
			"legs on a key that does not identify a record, which drops or "+
			"duplicates rows. This is the merge-soundness consequence of the "+
			"identity arm, not a second opinion about it.",
			values.ExplainValue(foreignRowKey), values.ExplainValue(pkKey))
	}
}

// TestNonFieldOrderingKeysStayWholeValueCompared pins the OTHER side of type
// dispatch: values that are not column reads must keep being matched as whole
// Values. The primary-key intersection's record-type discriminators are the
// population that depends on this — 126 of them in the measured corpus, none
// with a column identity by design — and routing them into an
// identity-or-decline arm would collapse every intersection ordering to empty.
func TestNonFieldOrderingKeysStayWholeValueCompared(t *testing.T) {
	t.Parallel()

	layout := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
	})
	domain := values.OrdinalDomainOfType(layout)
	base := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, domain)

	left := values.NewRecordTypeValue(base)
	right := values.NewRecordTypeValue(base)

	if values.OrderingFieldPair(left, right) {
		t.Fatalf("a RecordTypeValue pair is inside the FieldValue type class, so " +
			"the identity arm would decide it. A discriminator is not a column of " +
			"any row and has no ordinal to state; it must be compared as a whole " +
			"Value.")
	}
	if !intersectionValuesEqual(left, right) {
		t.Fatalf("intersectionValuesEqual refuses two structurally identical " +
			"record-type discriminators.\n\n" +
			"These carry no column identity by design, so an identity-or-decline " +
			"arm rejects them and every intersection's merged ordering comes out " +
			"empty. They must reach the whole-Value comparison.")
	}
}

// TestPartitionRedundancyProofSeparatesSameSlotDifferentLayouts is the net for
// the third consumer, orderingContainsEqualityValues — the proof that lets a
// partition be DROPPED because a smaller one already fixes the same equalities.
//
// It used to compare with bare structural equality while the `required` list it
// probes was DEDUPED by intersectionValuesEqual. A list built under one notion of
// "same value" and probed under another can report a member present that it does
// not hold, and here that direction is the dangerous one: the subpartition looks
// like it fixes an equality it does not, the larger partition is discarded, and
// at arity two the pair also enters badPairs and kills every superset. The plan
// is lost, not wrong — which is exactly why nothing went red.
func TestPartitionRedundancyProofSeparatesSameSlotDifferentLayouts(t *testing.T) {
	t.Parallel()

	requiredKey, foreignRowKey := twoLayoutOrdinalCollision(t)

	// A subpartition ordering that fixes the FOREIGN row's slot 0 and nothing
	// else. It does NOT fix the required column.
	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			foreignRowKey: {properties.FixedBinding(nil)},
		},
		[]values.Value{foreignRowKey},
		false,
	)

	var fixed []values.Value
	for v := range ordering.GetEqualityBoundValues() {
		fixed = append(fixed, v)
	}
	if len(fixed) != 1 {
		t.Fatalf("test setup: the ordering must expose exactly one "+
			"equality-bound value for this probe, got %d", len(fixed))
	}

	if orderingContainsEqualityValues(ordering, []values.Value{requiredKey}) {
		t.Fatalf("orderingContainsEqualityValues says an ordering fixing only %q "+
			"also fixes %q.\n\n"+
			"It does not — the two share an ORDINAL in two different row layouts. "+
			"The proof succeeded on a domain-blind comparison, so a partition gets "+
			"dropped as redundant against a subpartition that constrains a "+
			"different column, and at arity two the pair is sieved out and every "+
			"superset dies with it. This must compare through the same comparator "+
			"that DEDUPED the required list.",
			values.ExplainValue(foreignRowKey), values.ExplainValue(requiredKey))
	}

	// The positive direction, so a proof that refuses everything does not pass:
	// an ordering fixing the required column really does satisfy the proof.
	satisfying := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			requiredKey: {properties.FixedBinding(nil)},
		},
		[]values.Value{requiredKey},
		false,
	)
	if !orderingContainsEqualityValues(satisfying, []values.Value{requiredKey}) {
		t.Fatalf("orderingContainsEqualityValues refuses an ordering that fixes "+
			"exactly the required column %q. A proof that never succeeds makes "+
			"every partition look non-redundant and the sieve stops working.",
			values.ExplainValue(requiredKey))
	}
}

// TestOrderingComparatorsAreStillIntransitiveAcrossTheUnknownDomain records a
// defect that is REAL, MEASURED and NOT YET FIXED, so that it cannot be
// forgotten and cannot be rediscovered from scratch.
//
// Both comparators currently dispatch on whether both operands HAPPEN TO STATE an
// identity: identity decides when both do, and otherwise the pair falls through to
// the domain-blind structural comparison. That dispatch is not an equivalence
// relation. All three values below bake ordinal path [0]:
//
//	A = [0] in layout D1          states an identity
//	B = [0] in layout D2          states an identity
//	U = [0], layout UNKNOWN       states none
//
// Identity separates A from B. U falls through and compares EQUAL to both. So
// U≡A and U≡B with A≢B, and a comparator that is not transitive makes every set
// built through it depend on INSERTION ORDER — including
// adjustedIntersectionOrdering's `seen` dedup, which decides the intersection's
// comparison keys and their bindings. Order-dependent comparison keys are a
// nondeterministic plan.
//
// THE FIX IS DISPATCH BY VALUE TYPE: both operands *FieldValue means
// SameOrderingColumn decides and the decision is FINAL, with U declined instead of
// bridged. That is measured to be FREE in production — over the corpus, the
// decline residual at both comparators is ZERO and no pair changes its answer
// (pkg/relational/conformance/explaindiff/ordering_census_test.go).
//
// What blocks it is not the planner, it is this package's FIXTURES. The doubles in
// abstract_data_access_rule_test.go mint matched ordering parts and requested
// ordering parts as BARE NAMES (&values.FieldValue{Field: col}), modelling the
// world before the producers carried ordinals; real candidates route through
// bakeOrderingColumnIn against the row layout the scan flows. Type dispatch
// declines a name-only key, so 24 tests in this package go red — not because the
// planner regressed, but because the doubles double something that no longer
// exists. Making them state a layout requires the fixture's synthetic index
// columns to BE fields of a shared record row, which is a fixture redesign, not a
// call-site change.
//
// This test asserts the CURRENT, WRONG behaviour on purpose. When the fixtures are
// reworked and the dispatch flipped, it goes RED — at which point delete it and
// assert transitivity instead.
func TestOrderingComparatorsAreStillIntransitiveAcrossTheUnknownDomain(t *testing.T) {
	t.Parallel()

	inD1, inD2 := twoLayoutOrdinalCollision(t)
	unknownDomain := values.NewFieldValueWithResolvedOrdinal(
		"ID", 0, values.UnknownType)
	if values.StatesOrderingColumn(unknownDomain) {
		t.Fatalf("test setup: %q states a column identity, so it is not the "+
			"UNKNOWN-domain witness; NewFieldValueWithResolvedOrdinal must keep "+
			"minting domain-free bakes for the witness to exist",
			values.ExplainValue(unknownDomain))
	}

	for _, c := range []struct {
		name string
		eq   func(a, b values.Value) bool
	}{
		{"intersectionValuesEqual", intersectionValuesEqual},
		{"orderingValuesEqual", orderingValuesEqual},
	} {
		if c.eq(inD1, inD2) {
			t.Errorf("%s equates ordinal 0 of two DIFFERENT layouts. That is the "+
				"conflation the identity arm exists to refuse, and it is a "+
				"REGRESSION, not progress on the intransitivity below.", c.name)
			continue
		}
		if !c.eq(inD1, unknownDomain) || !c.eq(inD2, unknownDomain) {
			t.Errorf("%s no longer bridges the domain-free bake %q to BOTH stated "+
				"layouts (%q: %v, %q: %v).\n\n"+
				"If that is because dispatch is now by TYPE and the domain-free "+
				"value is DECLINED, the defect this test records is FIXED: delete "+
				"this test and assert transitivity directly. Confirm the fixtures "+
				"were reworked rather than the corpus getting luckier.",
				c.name, values.ExplainValue(unknownDomain),
				values.ExplainValue(inD1), c.eq(inD1, unknownDomain),
				values.ExplainValue(inD2), c.eq(inD2, unknownDomain))
		}
	}
}
