package cascades

// The streaming-aggregate ADJACENCY decision must carry the root source, not
// just the column name.
//
// orderingSatisfiesGroupingKeys is what admits an ordered inner in place of an
// InMemorySort under a GroupBy. Its answer is not a row ORDER claim, it is an
// ADJACENCY claim: an aggregate handed a stream it was told is grouped and is
// not emits one output row per RUN, so a wrong yes is a wrong VALUE — a split
// group — and nothing downstream bounds it the way a distinct's dedup would.
//
// The predicate compares accessor NAME paths (values.ColumnNamePathsEqual), and
// values.AccessorNamePath stops at the root WITHOUT contributing it. So `o.a`
// and `i.a` over the same table produce the identical path ["A"] and compare
// EQUAL, while being different columns. That is the dimension these tests probe,
// and it is unreachable through SQL today for a structural reason worth naming:
// every plan that can hand this rule a KNOWN plain ordering is single-source,
// because both join plans report IsKnown=false. The proof therefore has to be
// made at the predicate, and it has to be made BEFORE some join starts reporting
// an ordering pulled up into its legs' aliases — at which point the collision is
// one rule away from live.
//
// The reachable instance of the same conflation was NOT here. It was a
// projection republishing its child's ordering keys verbatim, which shipped wrong
// rows through this very rule; that is pinned against real FDB by
// yamsql/testdata/streaming_agg_projection_layout.yaml and at the provider by
// plans/ordering_pullup_identity_test.go.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// adjacencyLayout is the table both quantifiers range over — the self-join shape
// where two provided keys are byte-identical and denote different columns.
func adjacencyLayout() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
	})
}

// legField reads column A off the named quantifier, baked exactly as the
// translator bakes a grouping key over a join leg.
func legField(t *testing.T, alias string) values.Value {
	t.Helper()
	domain := values.OrdinalDomainOfType(adjacencyLayout())
	if !domain.IsKnown() {
		t.Fatalf("test setup: the layout has no token")
	}
	return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
		"A", 1, values.NullableLong, domain)
}

// sourceLocalField is the same column read source-locally — the form every
// ordering provider in plans/ordering.go mints.
func sourceLocalField(t *testing.T) values.Value {
	t.Helper()
	domain := values.OrdinalDomainOfType(adjacencyLayout())
	if !domain.IsKnown() {
		t.Fatalf("test setup: the layout has no token")
	}
	return values.NewFieldValueWithResolvedOrdinalInDomain(
		"A", 1, values.NullableLong, domain)
}

// TestAdjacencyRefusesTwoDifferentQuantifiersSameColumnName is the net.
//
// Grouping by `i.a` while the stream is ordered by `o.a` is grouping by an
// UNORDERED column. The name path cannot see the difference; the root proof must.
func TestAdjacencyRefusesTwoDifferentQuantifiersSameColumnName(t *testing.T) {
	t.Parallel()

	groupKey := legField(t, "I")
	provided := legField(t, "O")

	// The premise, asserted rather than assumed: the name path — the whole of the
	// old predicate — says these are the same column.
	if !values.ColumnNamePathsEqual(groupKey, provided) {
		t.Fatalf("test setup: the accessor name paths of %q and %q already differ, "+
			"so this pair does not exercise the correlation-blind dimension",
			values.ExplainValue(groupKey), values.ExplainValue(provided))
	}

	ordering := properties.Ordering{IsKnown: true, Keys: []values.Value{provided}}
	if orderingSatisfiesGroupingKeys(ordering, []values.Value{groupKey}) {
		t.Fatalf("the adjacency decision ADMITS an ordering on %q as satisfying a "+
			"grouping key on %q.\n\n"+
			"These are two quantifiers over the same table: same column name, same "+
			"ordinal, same layout token, DIFFERENT columns. Admitting one for the "+
			"other tells the streaming aggregate its input is grouped by the key when "+
			"only the other leg is ordered, and it then emits one row per RUN -- a "+
			"split group, a wrong VALUE, with nothing downstream to bound it.",
			values.ExplainValue(provided), values.ExplainValue(groupKey))
	}
}

// TestAdjacencyAdmitsTheSameQuantifier is the control that keeps the net from
// being satisfiable by refusing everything.
func TestAdjacencyAdmitsTheSameQuantifier(t *testing.T) {
	t.Parallel()

	groupKey := legField(t, "O")
	provided := legField(t, "O")
	ordering := properties.Ordering{IsKnown: true, Keys: []values.Value{provided}}
	if !orderingSatisfiesGroupingKeys(ordering, []values.Value{groupKey}) {
		t.Fatalf("the adjacency decision REFUSES an ordering on %q for a grouping "+
			"key on %q -- the same column of the same quantifier. Refusing here is "+
			"not conservatism, it is every streaming aggregate over a join leg losing "+
			"its ordered inner.",
			values.ExplainValue(provided), values.ExplainValue(groupKey))
	}
}

// TestAdjacencyAdmitsTheSourceLocalBridge pins the seam that MUST stay open, in
// both directions, because it is the shape production actually presents: every
// provider mints source-local keys while a grouping key over a single-quantifier
// select stays rooted at that quantifier.
//
// This is the same childless/qualified bridge values.CanBridgeOrderingValueRoots
// crosses. Closing it would cost every streaming aggregate its ordered inner,
// which is why the root proof admits a source-local side rather than demanding
// correlation equality outright.
func TestAdjacencyAdmitsTheSourceLocalBridge(t *testing.T) {
	t.Parallel()

	local := sourceLocalField(t)
	qualified := legField(t, "O")

	for _, tc := range []struct {
		name     string
		groupKey values.Value
		provided values.Value
	}{
		{"qualified group key, source-local provider", qualified, local},
		{"source-local group key, qualified provider", local, qualified},
	} {
		ordering := properties.Ordering{IsKnown: true, Keys: []values.Value{tc.provided}}
		if !orderingSatisfiesGroupingKeys(ordering, []values.Value{tc.groupKey}) {
			t.Errorf("%s: the adjacency decision refuses provided %q for grouping "+
				"key %q. A source-local key is Go's canonical \"this operator's own "+
				"row\" root and bridges to a qualified read of that row; refusing it "+
				"loses the ordered inner on the common path.",
				tc.name, values.ExplainValue(tc.provided), values.ExplainValue(tc.groupKey))
		}
	}
}

// TestAdjacencyRefusesAnUnlocatableRoot pins the fail-closed edge: a value whose
// root this predicate cannot identify is not one an aggregate may act on.
func TestAdjacencyRefusesAnUnlocatableRoot(t *testing.T) {
	t.Parallel()

	// An arithmetic ordering key has no column root at all.
	opaque := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  sourceLocalField(t),
		Right: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
	}
	ordering := properties.Ordering{IsKnown: true, Keys: []values.Value{opaque}}
	if orderingSatisfiesGroupingKeys(ordering, []values.Value{sourceLocalField(t)}) {
		t.Fatalf("the adjacency decision admits provided %q, whose root cannot be "+
			"located, for a grouping key on a plain column",
			values.ExplainValue(opaque))
	}
}
