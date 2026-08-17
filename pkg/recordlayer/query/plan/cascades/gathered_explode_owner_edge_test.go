package cascades

// Pins the correlation edge that carries a lateral UNNEST's dependency on the
// source its array lives in, AT THE CONSUMER that reads it.
//
// A bipartition that separates an Explode from that source materializes the
// Explode against a row where the array column is unbound — zero rows. The edge
// that forbids it used to be reconstructed from a STRING: the collection was
// minted as `FieldValue{Field:"A.WARR", Child:QOV(B)}` over the merged row, and
// a helper sliced "A" off the field name to re-add the dependency
// GetCorrelatedTo could not see. That reconstruction decided a quantifier's
// identity by text, so it lost the edge for a leg addressed by a minted binding
// and invented one for any sibling sharing the alias.
//
// The collection is now an ordinal bake over the OWNER's own quantifier, so the
// dependency IS the collection's correlation — Java's Quantifier.getCorrelatedTo
// edge, nothing reconstructed.
//
// These tests run the two REAL consumers, `computeTransitiveCorrelationOrder`
// (PartitionSelectRule's bipartition-validity input) and
// `buildQuantifierDependencyOrder` (the match enumerator's), because those are
// the two call sites the string recovery was wired into. A values-package
// assertion that a baked FieldValue correlates to its owner would prove the
// wrong thing: it holds whether or not the consumers ever see the edge.
//
// The second arm of each test is the one that makes the DELETION load-bearing.
// It feeds the OLD name-model collection and asserts the owner dependency is
// ABSENT — i.e. that nothing in the engine still reconstructs it from the
// prefix. Restore the string recovery (`values.MergeSeedLegsOfValue` plus
// `quantifierMergeSeedLegDeps`, and their calls here) and that arm goes RED,
// which is the point: it pins that the structural edge is the SOLE source of
// this dependency, so the name-model producer can never be reintroduced with
// the recovery quietly covering for it.
//
// Producer side: TestExplodeCollectionsAreOrdinalBaked (pkg/relational/core/query)
// pins that the translator emits no other collection shape on any routing arm.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ownerRowType is the OWNER leg's row: the array sits at ordinal 1.
func ownerRowType() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "WARR", FieldType: values.NewArrayType(false, values.NotNullLong), Ordinal: 1},
	})
}

// ordinalBakedCollection is what the lowering emits today: the array addressed
// by its ORDINAL in the row of the quantifier that owns it.
func ordinalBakedCollection(t *testing.T, owner values.CorrelationIdentifier) values.Value {
	t.Helper()
	ownerQOV, err := values.NewQuantifiedObjectValue(owner, ownerRowType())
	ownerQOV = mustConstruct(t, ownerQOV, err)
	coll, err := values.ResolveOrdinalSeedField(ownerQOV, 1)
	return mustConstruct(t, coll, err)
}

// nameModelCollection is the shape the qualified-name channel produced: a read
// off the FLOW leg's merged row, with the OWNING leg packed into the name.
// The exact fixture gives the flow row a real field with that legacy dotted
// display name. Its admitted FieldValue still correlates only to the flow leg,
// so any owner edge can only have been reconstructed from display text.
func nameModelCollection(
	t testing.TB,
	owner, flow values.CorrelationIdentifier,
) values.Value {
	t.Helper()
	flowType := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: owner.Name() + ".WARR", FieldType: values.NewArrayType(false, values.NotNullLong), Ordinal: 1},
	})
	flowQOV, err := values.NewQuantifiedObjectValue(flow, flowType)
	flowQOV = mustConstruct(t, flowQOV, err)
	collection, err := values.ResolveFieldOrdinals(flowQOV, []int{1})
	collection = mustConstruct(t, collection, err)
	field, ok := values.AsFieldValue(collection)
	if !ok || field.DisplayName() != owner.Name()+".WARR" {
		t.Fatalf("legacy display-name fixture = %T %v, want %q", collection, collection, owner.Name()+".WARR")
	}
	return collection
}

// explodeQuantifierOver wraps a collection Value in the gathered-Explode
// quantifier shape the partition rules actually meet.
func explodeQuantifierOver(t testing.TB, coll values.Value) expressions.Quantifier {
	t.Helper()
	explode, err := expressions.NewExplodeExpressionWithOrdinality(coll, false)
	explode = mustConstruct(t, explode, err)
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("EL"), expressions.InitialOf(explode),
	)
}

func gatheredOwnerScanQuantifier(t testing.TB, name string) expressions.Quantifier {
	t.Helper()
	scan := mustFullUnorderedScan(t, []string{name}, ownerRowType())
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name), expressions.InitialOf(scan))
}

// TestGatheredExplodeOwnerEdgeReachesPartitionOrder runs the bipartition
// classifier's own correlation-order input. If the first arm goes red, a
// bipartition can separate an Explode from its array source again and the
// zero-rows shape is back. If the SECOND arm goes red, the string recovery is
// alive again and is masking a name-model producer.
func TestGatheredExplodeOwnerEdgeReachesPartitionOrder(t *testing.T) {
	t.Parallel()

	owner := values.NamedCorrelationIdentifier("A")
	flow := values.NamedCorrelationIdentifier("B")

	t.Run("ordinal bake carries the owner edge", func(t *testing.T) {
		t.Parallel()
		// A planner owns its mutable Reference graph. Give each parallel arm a
		// fresh graph too; sharing these quantifiers races Reference's lazy
		// correlation cache and does not model production ownership.
		ownerQ := gatheredOwnerScanQuantifier(t, "A")
		flowQ := gatheredOwnerScanQuantifier(t, "B")
		el := explodeQuantifierOver(t, ordinalBakedCollection(t, owner))
		order := computeTransitiveCorrelationOrder(
			[]expressions.Quantifier{ownerQ, flowQ, el})
		if _, dep := order[el.GetAlias()][owner]; !dep {
			t.Fatalf("EL's dependencies = %v, want the OWNER A — without that edge a "+
				"bipartition separates the Explode from the source its array lives in "+
				"and it materializes against an unbound column (zero rows)",
				order[el.GetAlias()])
		}
		if _, dep := order[owner][el.GetAlias()]; dep {
			t.Fatalf("A depends on EL: %v — the edge must point owner→consumer only",
				order[owner])
		}
	})

	t.Run("name-model collection has NO owner edge to recover", func(t *testing.T) {
		t.Parallel()
		ownerQ := gatheredOwnerScanQuantifier(t, "A")
		flowQ := gatheredOwnerScanQuantifier(t, "B")
		el := explodeQuantifierOver(t, nameModelCollection(t, owner, flow))
		order := computeTransitiveCorrelationOrder(
			[]expressions.Quantifier{ownerQ, flowQ, el})
		if _, dep := order[el.GetAlias()][flow]; !dep {
			t.Fatalf("fixture: EL must correlate to the FLOW leg B (%v), or the assertion "+
				"below is vacuous", order[el.GetAlias()])
		}
		if _, dep := order[el.GetAlias()][owner]; dep {
			t.Fatalf("EL depends on the OWNER A (%v) although its collection only "+
				"references B: the leg was recovered from the dotted PREFIX of the "+
				"display name. That recovery is deleted — a quantifier's identity is "+
				"not decided by text — so this edge must not exist, and the ordinal "+
				"bake is what supplies it instead",
				order[el.GetAlias()])
		}
	})
}

// TestGatheredExplodeOwnerEdgeReachesMatchEnumerator is the same pair against
// the OTHER consumer the recovery was wired into. Two call sites means two
// places a reintroduction can hide, so both are pinned rather than one standing
// in for the other.
func TestGatheredExplodeOwnerEdgeReachesMatchEnumerator(t *testing.T) {
	t.Parallel()

	owner := values.NamedCorrelationIdentifier("A")
	flow := values.NamedCorrelationIdentifier("B")
	ownerQ := gatheredOwnerScanQuantifier(t, "A")
	flowQ := gatheredOwnerScanQuantifier(t, "B")

	// dependsOn reports whether the enumerator ordered EL (index 2) after the
	// quantifier at index `on`.
	dependsOn := func(t *testing.T, coll values.Value, on int) bool {
		t.Helper()
		el := explodeQuantifierOver(t, coll)
		order := buildQuantifierDependencyOrder(
			[]expressions.Quantifier{ownerQ, flowQ, el})
		if !order.ok {
			t.Fatal("dependency order did not build — the fixture stopped exercising the enumerator")
		}
		_, dep := order.transitive[2][on]
		return dep
	}

	if !dependsOn(t, ordinalBakedCollection(t, owner), 0) {
		t.Fatal("the ordinal-baked Explode does not depend on its OWNER (index 0) in the " +
			"match enumerator's order — the enumerator may then bind the Explode before " +
			"the source its array lives in")
	}
	if !dependsOn(t, nameModelCollection(t, owner, flow), 1) {
		t.Fatal("fixture: the name-model Explode must depend on the FLOW leg (index 1), " +
			"or the assertion below is vacuous")
	}
	if dependsOn(t, nameModelCollection(t, owner, flow), 0) {
		t.Fatal("the name-model Explode depends on the OWNER (index 0) although its " +
			"collection references only the flow leg: the leg was recovered from the " +
			"dotted PREFIX of the display name. That recovery is deleted; the ordinal " +
			"bake supplies the edge structurally instead")
	}
}
