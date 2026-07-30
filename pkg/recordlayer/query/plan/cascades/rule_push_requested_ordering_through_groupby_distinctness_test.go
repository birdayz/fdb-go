package cascades

// The requested-ordering push through a GROUP BY builds the child's ordering
// request out of the grouping keys. Java fixes two things about the ordering it
// builds that Go used to forward from the request above:
// PushRequestedOrderingThroughGroupByRule.java:167-168 constructs it with
// `pushedRequestedOrdering.getDistinctness()` — always PRESERVE_DISTINCTNESS,
// because pushDown's only implementation returns that (RequestedOrdering.java:236,
// and preserve() at :289) — and with exhaustive HARDCODED to false.
//
// Go forwarded `reqOrd.GetDistinctness()` and `reqOrd.IsExhaustive()`. Both are
// constraints on the CHILD that Java never imposes, and both are invisible to a
// test that only checks which keys come out, which is why the dimension went
// unprobed: every existing test on this rule asserts the key list.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestGroupByOrderingPushIsNeverDistinctAndNeverExhaustive probes the two axes
// the key list cannot show.
//
// It also pins WHY Java's distinctness gate at :161 is not ported. That gate
// (`!pushedRequestedOrdering.isDistinct() || requiredOrderingValues.isEmpty()`)
// suppresses the push entirely when the ordering is DISTINCT and grouping keys
// are left over. It is unreachable in Java because the left disjunct is a
// tautology — and it is unreachable here for the same reason only as long as the
// synthesized ordering takes PRESERVE_DISTINCTNESS. If someone restores the
// forwarding, the gate becomes live, its absence becomes a real divergence, and
// this test is what says so.
func TestGroupByOrderingPushIsNeverDistinctAndNeverExhaustive(t *testing.T) {
	t.Parallel()

	inputRow := values.NewRecordType("", false, []values.Field{
		{Name: "REGION", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "AMOUNT", FieldType: values.NullableLong, Ordinal: 1},
	})
	domain := values.OrdinalDomainOfType(inputRow)

	region := values.NewFieldValueWithResolvedOrdinalInDomain(
		"REGION", 0, values.UnknownType, domain)
	amount := values.NewFieldValueWithResolvedOrdinalInDomain(
		"AMOUNT", 1, values.UnknownType, domain)

	// Two grouping keys, a request naming only the first. The leftover key is
	// what makes Java's :161 gate's right disjunct false, so this is the exact
	// shape where a restored `isDistinct()` forwarding would change behaviour.
	groupingKeys := []values.Value{region, amount}

	request := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     region,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessDistinct,
		true, // exhaustive
	)
	if !request.IsDistinct() {
		t.Fatalf("test setup: the request must be DISTINCT for this to probe the " +
			"distinctness axis at all")
	}
	if !request.IsExhaustive() {
		t.Fatalf("test setup: the request must be exhaustive for this to probe " +
			"the exhaustive axis at all")
	}

	got := synthesizeGroupByOrdering(request, groupingKeys)
	if got == nil {
		t.Fatalf("the rule refused to synthesize an ordering for a request naming " +
			"one of two grouping keys. That is the compatible case — the leftover " +
			"key is appended with ANY — so a nil here means the match itself broke " +
			"and neither axis below is being probed.")
	}
	if len(got.GetParts()) != len(groupingKeys) {
		t.Fatalf("synthesized %d parts for %d grouping keys; streaming "+
			"aggregation needs every grouping key present, and the leftover-append "+
			"is what this test's DISTINCT-with-leftovers shape depends on",
			len(got.GetParts()), len(groupingKeys))
	}

	if got.IsDistinct() {
		t.Errorf("the ordering pushed to the group-by's child is DISTINCT.\n\n" +
			"Java builds it with the PUSHED ordering's distinctness " +
			"(PushRequestedOrderingThroughGroupByRule.java:167), and pushDown " +
			"always yields PRESERVE_DISTINCTNESS (RequestedOrdering.java:236, :289) " +
			"— so Java NEVER pushes a distinct ordering here, whatever the request " +
			"above asked for. Forwarding the request's distinctness imposes a " +
			"constraint on the child that Java does not, and it simultaneously " +
			"makes Java's unreachable :161 gate reachable, turning its absence " +
			"from a correct omission into a divergence.")
	}

	if got.IsExhaustive() {
		t.Errorf("the ordering pushed to the group-by's child is EXHAUSTIVE.\n\n" +
			"Java hardcodes false at " +
			"PushRequestedOrderingThroughGroupByRule.java:168. Forwarding the " +
			"request's flag asks the child to enumerate orderings Java never asks " +
			"it for.")
	}
}
