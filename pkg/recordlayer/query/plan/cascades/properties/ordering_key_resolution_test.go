package properties

// What orderingKeyFor may and may not equate.
//
// The ordering set is still addressed by a rendered string, so this function is
// where a requested ordering key meets a provided one. Every arm it has is a
// claim that two renderings denote the same column, and the arms differ in how
// much of the column's identity they are willing to drop to say yes.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// resolutionThreeColumnRow is a layout wide enough that a column's ordinal and
// a one-column layout's ordinal 0 cannot coincide.
func resolutionThreeColumnRow() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "COL", FieldType: values.NullableLong, Ordinal: 2},
	})
}

func resolutionOneColumnRow() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "COL", FieldType: values.NullableLong, Ordinal: 0},
	})
}

func orderingOverOneKey(key values.Value) *RichOrdering {
	return NewRichOrdering(
		map[values.Value][]OrderingBinding{
			key: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{key},
		true,
	)
}

// TestOrderingKeyForRefusesTwoStatedOrdinals is the conflation net.
//
// Two values that have both STATED an ordinal are decided by their ordinals and
// their layouts, never by their spelling. The pair here shares only its display
// name: ordinal 2 of a three-column row against ordinal 0 of a one-column row.
// Resolving it would report a provided ordering on one column as satisfying a
// request for another, and the sort would be elided against an order the plan
// does not provide.
//
// This is what makes the remaining name comparison in orderingKeyFor safe rather
// than merely small: CanBridgeOrderingFieldValues refuses outright when both
// sides carry a resolved path, so the name is consulted only where one side has
// no ordinal to compare.
func TestOrderingKeyForRefusesTwoStatedOrdinals(t *testing.T) {
	t.Parallel()

	provided := values.NewFieldValueWithResolvedOrdinalInDomain(
		"COL", 0, values.UnknownType,
		values.OrdinalDomainOfType(resolutionOneColumnRow()))
	requested := values.NewFieldValueWithResolvedOrdinalInDomain(
		"COL", 2, values.UnknownType,
		values.OrdinalDomainOfType(resolutionThreeColumnRow()))

	if values.ColumnNameValue(provided) != values.ColumnNameValue(requested) {
		t.Fatalf("test setup: the two must share their ordinal-FREE rendering "+
			"(%q vs %q), or no name comparison would fire on them",
			values.ColumnNameValue(provided), values.ColumnNameValue(requested))
	}
	if values.ExplainValue(provided) == values.ExplainValue(requested) {
		t.Fatal("test setup: the two must differ in their full rendering, or " +
			"the exact arm resolves them and nothing is being tested")
	}

	if key, ok := orderingOverOneKey(provided).orderingKeyFor(requested); ok {
		t.Fatalf("orderingKeyFor resolved %q to the provided key %q.\n\n"+
			"These are DIFFERENT columns: ordinal 2 of a three-column row and "+
			"ordinal 0 of a one-column row. They agree on their display name and "+
			"nothing else, so the only comparison that says yes is one that "+
			"erases the ordinal on purpose. A provided ordering on one column "+
			"then satisfies a request for another and the enforcer sort is "+
			"elided against an order that does not exist.",
			values.ExplainValue(requested), key)
	}

	// The exact arm must still work, so this cannot pass by refusing everything.
	same := values.NewFieldValueWithResolvedOrdinalInDomain(
		"COL", 0, values.UnknownType,
		values.OrdinalDomainOfType(resolutionOneColumnRow()))
	if _, ok := orderingOverOneKey(provided).orderingKeyFor(same); !ok {
		t.Fatal("orderingKeyFor no longer resolves a requested key that states " +
			"the SAME ordinal in the SAME layout as the provided key — that is " +
			"the common path and every elision depends on it")
	}
}

// TestOrderingKeyForStillResolvesAnUnstatedProvidedKeyByName records what is
// LEFT, and it is deliberately an assertion of the CURRENT state rather than of
// a desired one.
//
// A separate ordinal-free same-column arm was deleted from orderingKeyFor, and
// it is easy to read that as "the name bridge is gone". It is not. The
// quantifier-root arm below it begins by calling CanBridgeOrderingFieldValues —
// the very same ordinal-free name comparison — so a provided key that states no
// ordinal is still reachable from a requested key that does, purely by spelling.
// The deletion removed a redundant fast path; the channel is one call deeper.
//
// 1824 corpus resolutions still run through that arm, so this must keep
// answering yes until the request is TRANSLATED into the provider's correlation
// space before matching (what Java does, and what replaces the arm). When that
// lands, this test goes red — and the right response is to delete it, not to
// restore the bridge.
func TestOrderingKeyForStillResolvesAnUnstatedProvidedKeyByName(t *testing.T) {
	t.Parallel()

	// The provider states no ordinal at all: the fail-closed shape a candidate
	// falls back to when it cannot name the layout it flows.
	provided := values.NewFieldValue(nil, "COL", values.UnknownType)
	requested := values.NewFieldValueWithResolvedOrdinalInDomain(
		"COL", 2, values.UnknownType,
		values.OrdinalDomainOfType(resolutionThreeColumnRow()))

	if _, ok := values.OrderingIdentityOf(provided); ok {
		t.Fatal("test setup: the provided key must state NO identity, or this " +
			"is not the shape the name comparison is reached for")
	}

	if _, ok := orderingOverOneKey(provided).orderingKeyFor(requested); !ok {
		t.Fatalf("orderingKeyFor no longer resolves a baked request (%q) against "+
			"a provider that states no ordinal (%q).\n\n"+
			"If this changed because the requested ordering is now pushed into "+
			"the provider's space before matching, the name comparison has been "+
			"RETIRED and this test should be deleted along with it.\n\n"+
			"If it changed because the comparison was merely tightened, 1824 "+
			"corpus key resolutions just became misses and sorts appear over "+
			"plans that were already ordered.",
			values.ExplainValue(requested), values.ExplainValue(provided))
	}
}
