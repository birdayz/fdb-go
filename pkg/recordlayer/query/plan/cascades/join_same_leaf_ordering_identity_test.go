package cascades

// Two quantifiers over the SAME table advertise BYTE-IDENTICAL provided ordering
// keys, and the join ordering property must still keep them apart.
//
// This is the dimension the RFC-190 FlatMap cases do not reach: every one of
// them gives its two legs DIFFERENT column names (`outer_key` vs `inner_key`),
// so any mechanism at all separates the two keys and none of them is under test.
// A self-join is the shape where the separation has to be earned -- both legs
// scan `T`, both provide the source-local `ID` at ordinal 0 of the same layout,
// and the only thing distinguishing them is which quantifier's row the key reads.
//
// Getting this wrong is not a lost optimisation. The concatenated ordering is
// what tells a merge join, a streaming aggregate or a sort-elision that rows
// arrive grouped by a key; conflating the inner leg's key with the outer's hands
// out an adjacency guarantee the stream does not have, and the operator reads
// rows it was promised were ordered and are not.
//
// THREE facts hold the separation, and this file exists because two of them were
// unpinned. They are stated as separate assertions so a failure names which one
// went:
//
//  1. each leg's ordering is pulled up into ITS OWN alias before the two are
//     concatenated (plan_properties.go, computeJoinRichOrdering);
//  2. that pull-up selects the result-constructor field OWNED BY that alias, not
//     the first field whose value happens to match (values/pullup.go,
//     `fieldOwnedByAlias`) -- with both legs reading the same column, the two RC
//     fields differ ONLY in their correlation, so a first-match pull-up would
//     attribute the inner leg's order to the outer leg's output column;
//  3. `CanBridgeOrderingValueRoots` refuses to bridge two QOV-rooted values
//     (accessor_name_path.go) -- already pinned in the values package, and
//     re-asserted here through the property so a change there is felt where it
//     matters.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// selfJoinLayout is a real record type, so the scan providers resolve their PK
// column to an ordinal in a KNOWN domain. Both legs get the same layout, which
// is the point: the domain and the ordinal are identical on both sides and
// cannot be what tells them apart.
func selfJoinLayout() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
}

// TestSelfJoinOrderingKeepsBothLegsApart is the collision, built deliberately.
func TestSelfJoinOrderingKeepsBothLegsApart(t *testing.T) {
	t.Parallel()

	layout := selfJoinLayout()
	pk := func() values.Value {
		return &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	}
	// A distinct outer is what makes the property CONCATENATE rather than
	// report the outer alone -- the arm where both legs' keys coexist in one
	// ordering set and can therefore collide.
	outer := plans.NewRecordQueryIndexPlan(
		"T_ID_IDX", nil, []string{"T"}, layout, false).
		WithIndexMetadata([]string{"ID"}, nil, false).
		WithStrictlySorted()
	inner := plans.NewRecordQueryScanPlan([]string{"T"}, layout, false).
		WithPrimaryKey([]values.Value{pk()})

	// Both legs provide the SAME key: same name, same ordinal, same domain.
	// Asserted rather than assumed -- if the providers ever stop agreeing here,
	// the rest of this test proves nothing.
	outerProvided := computeWrapperRichOrdering(outer).GetKeys()
	innerProvided := computeWrapperRichOrdering(inner).GetKeys()
	if len(outerProvided) != 1 || len(innerProvided) != 1 {
		t.Fatalf("test setup: legs provide %d and %d keys, want 1 each",
			len(outerProvided), len(innerProvided))
	}
	if !values.ValuesStructurallyEqual(outerProvided[0], innerProvided[0]) {
		t.Fatalf("test setup: the two legs provide DIFFERENT keys (%s vs %s), so "+
			"this test no longer exercises a same-leaf collision -- something "+
			"already distinguishes them and the separation is not being earned",
			values.ExplainValue(outerProvided[0]), values.ExplainValue(innerProvided[0]))
	}

	outerAlias := values.NamedCorrelationIdentifier("selfjoin_o")
	innerAlias := values.NamedCorrelationIdentifier("selfjoin_i")
	// The join's result value is where the two legs ARE distinguishable: the
	// translator emits each output column as a read off its own quantifier. The
	// two fields differ only in their correlation, which is exactly what makes a
	// first-match pull-up dangerous.
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "OID",
			Value: values.NewFieldValue(values.NewQuantifiedObjectValue(outerAlias), "ID", values.NotNullLong),
		},
		values.RecordConstructorField{
			Name:  "IID",
			Value: values.NewFieldValue(values.NewQuantifiedObjectValue(innerAlias), "ID", values.NotNullLong),
		},
	)
	flatMap := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		expressions.NamedPhysicalQuantifier(outerAlias, expressions.FinalOf(outer)),
		expressions.NamedPhysicalQuantifier(innerAlias, expressions.FinalOf(inner)),
		outerAlias, innerAlias, resultValue, false,
	)

	got := computeWrapperRichOrdering(flatMap)
	if got == nil {
		t.Fatal("join ordering property returned nil")
	}
	keys := got.GetKeys()

	// FACT 1: two keys survive. One key would mean the legs collapsed, and the
	// surviving key would carry ONE leg's direction while claiming to describe
	// both.
	if len(keys) != 2 {
		var rendered []string
		for _, k := range keys {
			rendered = append(rendered, values.ExplainValue(k))
		}
		t.Fatalf("self-join ordering has %d keys %v, want 2.\n"+
			"Both legs read the same column of the same table, so their provided "+
			"keys are identical; keeping them apart requires pulling each leg's "+
			"ordering up into its OWN alias before concatenation. One key means "+
			"that stopped happening, and the ordering now claims an adjacency "+
			"that holds for only one of the two legs.",
			len(keys), rendered)
	}

	// FACT 2: each key is attributed to the output column of ITS OWN leg. The
	// outer leg's order describes OID, the inner's describes IID. A first-match
	// pull-up would emit OID twice -- structurally distinct keys (different
	// aliases) that nonetheless name the wrong columns, which no key-count
	// assertion can see.
	// Built with the ordinal the pull-up bakes -- the field's position in the
	// result constructor -- because that ordinal IS the identity the key is
	// addressed by downstream. Naming the column without its slot would make the
	// assertion agree with a key that points at the other column.
	wantOuter := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(outerAlias), "OID", 0, values.NotNullLong)
	wantInner := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(innerAlias), "IID", 1, values.NotNullLong)
	if !values.ValuesStructurallyEqual(keys[0], wantOuter) {
		t.Fatalf("outer leg's ordering key = %s, want %s. The pull-up must select "+
			"the result field OWNED BY the outer alias, not the first field whose "+
			"value matches.",
			values.ExplainValue(keys[0]), values.ExplainValue(wantOuter))
	}
	if !values.ValuesStructurallyEqual(keys[1], wantInner) {
		t.Fatalf("inner leg's ordering key = %s, want %s. Attributing the inner "+
			"leg's order to the OUTER leg's output column is a wrong-column claim "+
			"that survives every key-count check.",
			values.ExplainValue(keys[1]), values.ExplainValue(wantInner))
	}

	// FACT 3: a request for the INNER leg's column alone is NOT satisfied. The
	// stream is ordered by (OID, IID) lexicographically; IID alone is not an
	// ordering it provides. This is the assertion that would go green if the key
	// identity ever stopped carrying the correlation -- the two keys would be
	// interchangeable and the inner-only request would match the outer's
	// leading position.
	innerOnly := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: wantInner, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	if got.Satisfies(innerOnly) {
		t.Fatalf("the self-join ordering claims to satisfy `ORDER BY %s` alone.\n"+
			"Rows arrive ordered by (%s, %s): the inner leg restarts its order "+
			"inside every outer row, so the inner column alone is not sorted. "+
			"A sort elided on this claim returns rows in the wrong order, and a "+
			"streaming aggregate grouping on it splits groups.",
			values.ExplainValue(wantInner),
			values.ExplainValue(wantOuter), values.ExplainValue(wantInner))
	}

	// The positive control, so the negative above cannot pass by the property
	// refusing everything: the outer prefix IS satisfied.
	outerPrefix := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: wantOuter, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	if !got.Satisfies(outerPrefix) {
		t.Fatalf("the self-join ordering does not satisfy `ORDER BY %s`, which the "+
			"outer leg's own order provides. The negative assertion above is "+
			"vacuous if the property refuses every request.",
			values.ExplainValue(wantOuter))
	}
}
