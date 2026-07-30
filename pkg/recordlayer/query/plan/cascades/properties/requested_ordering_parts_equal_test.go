package properties

// PartsEqual compares two requested orderings' part lists through valuesEqual,
// which is a bare values.ValuesStructurallyEqual — DOMAIN-BLIND on baked
// FieldValues, exactly like the two comparators that were converted to
// identity-or-decline type dispatch. This file records why it was NOT converted
// with them, as a measured position rather than an oversight, and pins the fact
// that blocks the conversion so the block is visible instead of remembered.
//
// It is NOT unreachable. The population is genuinely multi-domain: requested
// ordering parts are baked against a folded projection's OUTPUT row, against a
// GROUP BY's output row, and against an input select's RECORD row, and the union
// rule pushes a parent's parts VERBATIM into every leg reference
// (rule_push_requested_ordering_through_union.go), so one reference's constraint
// set can hold a parent-space part beside a leg-space part. Two of those can share
// an ordinal and mean different columns.
//
// What it IS, is HARMLESS in a way the intersector's comparator was not, and that
// asymmetry is the whole reason for the different treatment:
//
//   - valuesEqual is TRANSITIVE. It is plain structural equality, which is an
//     equivalence relation. The comparators that were converted had become
//     INTRANSITIVE by dispatching on identity AVAILABILITY, and an intransitive
//     comparator makes the sets built through it depend on insertion order — a
//     nondeterministic plan. Nothing like that is happening here.
//   - Both callers are DEDUP predicates, not matchers. CombineRequestedOrderings
//     uses it to notice a duplicate push; requestedOrderingSetsEqual uses it to
//     notice an intersection already consumed. Over-equality costs a dropped
//     push — lost exploration, never a wrong answer.
//
// And converting it TODAY would be a regression, which is the pinned part below:
// LAZY parts are a real member of this population (the translator mints a
// domain-free carrier for an ORDER BY key it could not resolve), and two lazy
// parts match today by their name. Type dispatch declines a lazy pair, so
// CombineRequestedOrderings would stop recognising a duplicate push, report
// `changed`, and re-enqueue the reference for a constraint no-op — the exact
// re-exploration regression the constraint-push path documents as fixed.
//
// So the prerequisite is the same one the intersector needed and got: resolve the
// LAZY PRODUCER first, then the type dispatch is free. Until the translator's
// unresolved sort-key mint states a layout, this stays structural.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPartsEqualStillMatchesTwoLazyPartsByName pins the fact that blocks
// converting PartsEqual to identity-or-decline type dispatch.
//
// If this ever goes RED because two lazy parts stopped matching, the conversion
// is no longer blocked — but check WHY first. If lazy parts stopped EXISTING (the
// translator now bakes every sort key), the conversion is unblocked and should
// happen. If they still exist and merely stopped matching, the re-exploration
// regression described above is live and this is the bug report.
func TestPartsEqualStillMatchesTwoLazyPartsByName(t *testing.T) {
	t.Parallel()

	// Two independently minted LAZY carriers for the same column — the shape the
	// translator produces for an ORDER BY key it could not resolve against a
	// layout, and the shape two separate pushes into one reference produce.
	lazyA := values.NewFieldValue(nil, "REGION", values.UnknownType)
	lazyB := values.NewFieldValue(nil, "REGION", values.UnknownType)
	if values.StatesOrderingColumn(lazyA) || values.StatesOrderingColumn(lazyB) {
		t.Fatalf("test setup: a lazy carrier must state NO column identity, else " +
			"this is not the population that blocks the conversion")
	}

	partsA := []RequestedOrderingPart{{
		Value: lazyA, SortOrder: RequestedSortOrderAscending,
	}}
	partsB := []RequestedOrderingPart{{
		Value: lazyB, SortOrder: RequestedSortOrderAscending,
	}}

	if !PartsEqual(partsA, partsB) {
		t.Fatalf("PartsEqual no longer matches two LAZY parts naming the same "+
			"column (%q vs %q).\n\n"+
			"Today they match by name, and both callers depend on it: "+
			"CombineRequestedOrderings uses PartsEqual to notice a DUPLICATE push, "+
			"and losing that makes it report `changed` for a constraint no-op and "+
			"re-enqueue the reference — unbounded re-exploration, and the "+
			"constraint list grows per reference without bound. "+
			"requestedOrderingSetsEqual loses the same way and re-consumes the same "+
			"intersection on every pass.\n\n"+
			"If the translator now bakes every sort key so lazy parts no longer "+
			"EXIST, this test has done its job: PartsEqual can be converted to "+
			"identity-or-decline type dispatch like the ordering comparators, and "+
			"this test should be replaced by one asserting that.",
			values.ExplainValue(lazyA), values.ExplainValue(lazyB))
	}

	// The other direction, so the pin is about lazy pairs and not about
	// PartsEqual accepting everything: two lazy parts naming DIFFERENT columns
	// must not match.
	other := []RequestedOrderingPart{{
		Value:     values.NewFieldValue(nil, "AMOUNT", values.UnknownType),
		SortOrder: RequestedSortOrderAscending,
	}}
	if PartsEqual(partsA, other) {
		t.Fatalf("PartsEqual matches two lazy parts naming DIFFERENT columns; the " +
			"name comparison it still performs has stopped discriminating at all.")
	}
}

// TestPartsEqualIsDomainBlindOnBakedParts records the defect that is NOT being
// fixed, so the booking is a measured statement rather than a silence.
//
// The assertion is deliberately the WRONG-looking direction: it asserts the
// conflation HAPPENS. That is the honest shape for a booked defect — it pins the
// current behaviour, names the consequence, and goes red the moment someone
// fixes it, at which point the booking is discharged and this test should be
// inverted.
func TestPartsEqualIsDomainBlindOnBakedParts(t *testing.T) {
	t.Parallel()

	recordRow := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "STATUS", FieldType: values.NullableLong, Ordinal: 1},
	})
	aggregateRow := values.NewRecordType("", false, []values.Field{
		{Name: "CUSTOMER_ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "COUNT", FieldType: values.NullableLong, Ordinal: 1},
	})
	recordDomain := values.OrdinalDomainOfType(recordRow)
	aggregateDomain := values.OrdinalDomainOfType(aggregateRow)
	if recordDomain == aggregateDomain {
		t.Fatalf("test setup: the two layouts must produce different domain tokens")
	}

	inRecordRow := []RequestedOrderingPart{{
		Value: values.NewFieldValueWithResolvedOrdinalInDomain(
			"ID", 0, values.UnknownType, recordDomain),
		SortOrder: RequestedSortOrderAscending,
	}}
	inAggregateRow := []RequestedOrderingPart{{
		Value: values.NewFieldValueWithResolvedOrdinalInDomain(
			"CUSTOMER_ID", 0, values.UnknownType, aggregateDomain),
		SortOrder: RequestedSortOrderAscending,
	}}

	if !PartsEqual(inRecordRow, inAggregateRow) {
		t.Fatalf("PartsEqual now SEPARATES ordinal 0 of a record row from ordinal " +
			"0 of an aggregate output row.\n\n" +
			"That is the fix, and it is welcome — but it means the comparison is no " +
			"longer domain-blind, so the booking this test records is discharged. " +
			"Verify the two callers did not lose their LAZY-pair dedup on the way " +
			"(see TestPartsEqualStillMatchesTwoLazyPartsByName for why that " +
			"matters), then invert this test to assert the separation.")
	}
}
