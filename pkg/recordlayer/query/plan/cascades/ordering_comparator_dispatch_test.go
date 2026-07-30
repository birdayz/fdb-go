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

// TestOrderingComparatorsAreTransitiveAcrossTheUnknownDomain asserts the property
// type dispatch exists to buy, on the exact triple that used to break it.
//
// The retired dispatch asked whether both operands HAPPENED TO STATE an identity:
// identity decided when both did, and otherwise the pair fell through to the
// domain-blind structural comparison. That is not an equivalence relation. All
// three values below bake ordinal path [0]:
//
//	A = [0] in layout D1          states an identity
//	B = [0] in layout D2          states an identity
//	U = [0], layout UNKNOWN       states none
//
// Identity separates A from B. Under availability dispatch U fell through and
// compared EQUAL to both, so U≡A and U≡B with A≢B. A comparator that is not
// transitive makes every set built through it depend on INSERTION ORDER —
// including adjustedIntersectionOrdering's `seen` dedup, which decides the
// intersection's comparison keys and their bindings. Order-dependent comparison
// keys are a nondeterministic plan.
//
// Type dispatch closes it by declining U instead of bridging it: both operands
// *FieldValue means SameOrderingColumn decides and the decision is FINAL. The
// decline costs nothing in production — over the corpus neither comparator ever
// sees a FieldValue pair with a non-stating operand, and
// pkg/relational/conformance/explaindiff's ordering-census test keeps that
// residual pinned at ZERO.
//
// The assertion below is transitivity over the whole triple rather than the three
// pairs, because the three pairs are what availability dispatch also satisfied
// individually; only the closure exposes it.
func TestOrderingComparatorsAreTransitiveAcrossTheUnknownDomain(t *testing.T) {
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
	// The premise, asserted rather than assumed: the structural arm CANNOT tell
	// the domain-free bake from either stated layout, so the only thing that can
	// make the triple transitive is refusing to consult that arm for a FieldValue
	// pair. Without this the assertion below could pass on a triple the weaker
	// arm happened to separate anyway.
	for _, stated := range []values.Value{inD1, inD2} {
		if !values.ValuesStructurallyEqual(stated, unknownDomain) {
			t.Fatalf("test setup: the structural comparison SEPARATES %q from the "+
				"domain-free bake %q, so this triple no longer witnesses the "+
				"intransitivity type dispatch removed and every assertion below is "+
				"vacuous.",
				values.ExplainValue(stated), values.ExplainValue(unknownDomain))
		}
	}

	triple := []struct {
		name  string
		value values.Value
	}{
		{"[0] in D1", inD1},
		{"[0] in D2", inD2},
		{"[0] in an UNKNOWN layout", unknownDomain},
	}

	for _, c := range []struct {
		name string
		eq   func(a, b values.Value) bool
	}{
		{"intersectionValuesEqual", intersectionValuesEqual},
		{"orderingValuesEqual", orderingValuesEqual},
	} {
		// The separation the identity arm exists for. Stated first because a
		// comparator that equated these would make the transitivity closure below
		// pass while conflating two different columns.
		if c.eq(inD1, inD2) {
			t.Errorf("%s equates ordinal 0 of two DIFFERENT layouts (%q vs %q). "+
				"That is the conflation the identity arm refuses.",
				c.name, values.ExplainValue(inD1), values.ExplainValue(inD2))
			continue
		}

		for _, a := range triple {
			for _, b := range triple {
				for _, d := range triple {
					if !c.eq(a.value, b.value) || !c.eq(b.value, d.value) {
						continue
					}
					if c.eq(a.value, d.value) {
						continue
					}
					t.Errorf("%s is NOT TRANSITIVE: %s ≡ %s and %s ≡ %s, but "+
						"%s ≢ %s.\n\n"+
						"A comparator that is not an equivalence relation makes "+
						"orderingValueListContains (and the intersection's `seen` "+
						"dedup built on it) answer differently depending on the order "+
						"the list was built in, which is a nondeterministic plan. The "+
						"cause is a FieldValue pair reaching an arm below the identity "+
						"arm: dispatch on the TYPE, and decline the operand that "+
						"states no identity rather than bridging it.",
						c.name, a.name, b.name, b.name, d.name, a.name, d.name)
				}
			}
		}

		// The decline is TOTAL, not a weaker match: the domain-free bake is
		// unaddressable, so it is not even equal to itself. Pinned because it is
		// what makes the closure above hold — a reflexive-but-otherwise-declining U
		// would still be transitive here, and this says which of the two we have.
		if c.eq(unknownDomain, unknownDomain) {
			t.Errorf("%s equates the domain-free bake %q with ITSELF.\n\n"+
				"An unaddressable ordering key has no column identity to match on, so "+
				"the comparator has nothing to decide with; if this starts passing, a "+
				"structural or name arm is deciding FieldValue pairs again and the "+
				"transitivity above holds only by luck of which pairs were probed.",
				c.name, values.ExplainValue(unknownDomain))
		}
	}

	// The ROOT axis carries the SAME defect this test cured on the domain axis,
	// and it is not fixed. Probing only one axis is what let it through, so the
	// second axis is probed here, in the same file, from the same triple shape.
	assertRootAxisWitnessStillIntransitive(t)
}

// selfJoinRootCollision returns the three ordering keys of the root-axis
// witness: ONE childless key and TWO QOV-rooted keys over the same column of the
// same layout, read off two DIFFERENT quantifiers.
//
// This is a self-join's shape. `o.A` and `i.A` share a layout token (both
// quantifiers range over the same table, and OrdinalDomain is derived from the
// layout's CONTENT, so the same table yields the same token) and share an
// ordinal, and they are different columns. The childless key is what a candidate
// mint and a source-relative request produce.
func selfJoinRootCollision(t *testing.T) (childless, outerA, innerA values.Value) {
	t.Helper()

	row := values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
	domain := values.OrdinalDomainOfType(row)
	if !domain.IsKnown() {
		t.Fatalf("test setup: the self-join row has no domain token")
	}

	outer := values.NamedCorrelationIdentifier("O")
	inner := values.NamedCorrelationIdentifier("I")

	childless = values.NewFieldValueWithResolvedOrdinalInDomain(
		"A", 0, values.UnknownType, domain)
	outerA = values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(outer), "A", 0, values.UnknownType, domain)
	innerA = values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(inner), "A", 0, values.UnknownType, domain)

	// All three STATE a column identity. This is the property that makes the
	// witness invisible to the corpus census's FieldPairsDecided bucket, and it
	// is what distinguishes the root axis from the domain axis: no dispatch
	// change can reach this defect, because every pair below is already decided
	// by the FINAL identity arm.
	for _, v := range []values.Value{childless, outerA, innerA} {
		if !values.StatesOrderingColumn(v) {
			t.Fatalf("test setup: %q states NO column identity, so this triple is "+
				"the domain-axis witness again rather than the root-axis one; all "+
				"three keys must be decided by the identity arm for the root "+
				"asymmetry to be the only thing under test",
				values.ExplainValue(v))
		}
	}

	return childless, outerA, innerA
}

// assertRootAxisWitnessStillIntransitive records a defect that is REAL, MEASURED
// and NOT YET FIXED, in the same blocked-negative form the domain-axis witness
// used before its fix landed: it asserts the WRONG behaviour on purpose so the
// defect cannot be forgotten, and it goes RED the moment the fix arrives.
//
// values.SameOrderingColumn treats the ZERO correlation as a WILDCARD: a
// childless key bridges to a QOV-rooted one, while two DIFFERENT named
// quantifiers decline. In a self-join that is exactly the intransitive triple
// type dispatch removed from the domain axis:
//
//	C    = childless [0] in D(R)      states an identity
//	o.A  = [0] in D(R) off quantifier O   states an identity
//	i.A  = [0] in D(R) off quantifier I   states an identity
//
// C ≡ o.A (wildcard), C ≡ i.A (wildcard), o.A ≢ i.A (two named quantifiers are
// different columns). All three are decided INSIDE the final identity arm, so
// unlike the domain axis no dispatch change can reach this — and the corpus
// census lumps all three pairs into FieldPairsDecided, where they are invisible.
//
// THE FIX IS NOT TO DELETE THE WILDCARD. The childless bridge is load-bearing:
// a candidate mints its ordering keys childless while a request scoped to its
// owning quantifier does not, and removing the bridge would decline every such
// match. The fix is to give the childless side a real correlation — CQ-55-A2's
// correlation-space translation, which resolves a source-relative root to the
// quantifier it actually reads — at which point the wildcard has nothing left to
// do and the triple collapses.
//
// What keeps this a PIN rather than a live hazard is measured, not argued:
// pkg/relational/conformance/explaindiff's ordering-census test counts the
// childless↔QOV bridges whose comparison context also holds a second distinct
// QOV root — the population where this triple could actually form — and asserts
// that count is ZERO over the corpus.
//
// When A2 lands, both assertions below go RED. Delete this function and fold the
// root triple into the transitivity closure above.
func assertRootAxisWitnessStillIntransitive(t *testing.T) {
	t.Helper()

	childless, outerA, innerA := selfJoinRootCollision(t)

	for _, c := range []struct {
		name string
		eq   func(a, b values.Value) bool
	}{
		{"intersectionValuesEqual", intersectionValuesEqual},
		{"orderingValuesEqual", orderingValuesEqual},
	} {
		// The separation that is CORRECT and must not regress: two named
		// quantifiers over the same table are different columns.
		if c.eq(outerA, innerA) {
			t.Errorf("%s equates %q and %q — two DIFFERENT quantifiers over the "+
				"same table. That is a self-join conflation and a REGRESSION, not "+
				"progress on the intransitivity below.",
				c.name, values.ExplainValue(outerA), values.ExplainValue(innerA))
			continue
		}

		// The defect: the zero correlation bridges to BOTH of them.
		toOuter := c.eq(childless, outerA)
		toInner := c.eq(childless, innerA)
		if !toOuter || !toInner {
			t.Errorf("%s no longer bridges the childless key %q to BOTH named "+
				"quantifiers (%q: %v, %q: %v).\n\n"+
				"If that is because the childless root now carries a real "+
				"correlation (CQ-55-A2's correlation-space translation), the defect "+
				"this pin records is FIXED: delete assertRootAxisWitnessStillIntransitive "+
				"and fold this triple into the transitivity closure above. If instead "+
				"the wildcard was simply deleted, check that candidate-vs-request "+
				"ordering matches still form at all — the bridge is load-bearing "+
				"and removing it declines every source-relative candidate key.",
				c.name, values.ExplainValue(childless),
				values.ExplainValue(outerA), toOuter,
				values.ExplainValue(innerA), toInner)
		}
	}
}
