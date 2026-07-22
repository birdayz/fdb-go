package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestOrderingPartitionHash_NestedShadowsTopLevel pins RFC-187 S6 (finding 4):
// the ordering-partition hash must include the FULL accessor path, so an
// ordering on a nested field addr.city does not collapse into the same
// partition as an ordering on a same-leaf-named top-level column city — which
// would let GetOrdering report one member's order for the other and elide a
// required sort → wrong output order.
func TestOrderingPartitionHash_NestedShadowsTopLevel(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("T")
	flatCity := func() values.Value {
		return values.NewFieldValue(values.NewQuantifiedObjectValue(src), "CITY", values.UnknownType)
	}
	nestedAddrCity := func() values.Value {
		return values.NewFieldValue(
			values.NewFieldValue(values.NewQuantifiedObjectValue(src), "ADDR", values.UnknownType),
			"CITY", values.UnknownType)
	}
	ord := func(k values.Value) properties.Ordering {
		return properties.Ordering{IsKnown: true, Keys: []values.Value{k}, Descending: []bool{false}}
	}

	nestedH := orderingPartitionHash(ord(nestedAddrCity()))
	flatH := orderingPartitionHash(ord(flatCity()))
	if nestedH == flatH {
		t.Fatalf("ordering on nested addr.city and top-level city hash identically (%d) → would co-partition and elide sort against wrong column", nestedH)
	}

	// Consistency (equal ⟹ same hash): structurally-equal orderings must hash
	// the same, else orderingsEqual and the hash bucket disagree.
	if orderingPartitionHash(ord(flatCity())) != flatH {
		t.Fatal("two structurally-equal orderings on city hash differently (hash/equality inconsistency)")
	}
	if orderingPartitionHash(ord(nestedAddrCity())) != nestedH {
		t.Fatal("two structurally-equal orderings on addr.city hash differently")
	}

	// A different leaf must also differ.
	nameH := orderingPartitionHash(ord(values.NewFieldValue(values.NewQuantifiedObjectValue(src), "NAME", values.UnknownType)))
	if nameH == flatH {
		t.Fatal("orderings on distinct columns name and city hash identically")
	}
}
