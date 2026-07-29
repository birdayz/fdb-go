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
		{Name: "WARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

// ordinalBakedCollection is what the lowering emits today: the array addressed
// by its ORDINAL in the row of the quantifier that owns it.
func ordinalBakedCollection(t *testing.T, owner values.CorrelationIdentifier) values.Value {
	t.Helper()
	ownerQOV := values.NewQuantifiedObjectValueOfType(owner, ownerRowType())
	coll, err := values.NewFieldValueOfOrdinal(ownerQOV, 1)
	if err != nil {
		t.Fatalf("bake collection: %v", err)
	}
	return coll
}

// nameModelCollection is the shape the qualified-name channel produced: a read
// off the FLOW leg's merged row, with the OWNING leg packed into the name.
// GetCorrelatedTo can only report the flow leg from it.
func nameModelCollection(owner, flow values.CorrelationIdentifier) values.Value {
	return &values.FieldValue{
		Field: owner.Name() + ".WARR",
		Child: values.NewQuantifiedObjectValue(flow),
		Typ:   values.NotNullLong,
	}
}

// explodeQuantifierOver wraps a collection Value in the gathered-Explode
// quantifier shape the partition rules actually meet.
func explodeQuantifierOver(coll values.Value) expressions.Quantifier {
	explode := expressions.NewExplodeExpressionWithOrdinality(coll, false)
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("EL"), expressions.InitialOf(explode),
	)
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
	ownerQ := scanQuantifier("A")
	flowQ := scanQuantifier("B")

	t.Run("ordinal bake carries the owner edge", func(t *testing.T) {
		t.Parallel()
		el := explodeQuantifierOver(ordinalBakedCollection(t, owner))
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
		el := explodeQuantifierOver(nameModelCollection(owner, flow))
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
	ownerQ := scanQuantifier("A")
	flowQ := scanQuantifier("B")

	// dependsOn reports whether the enumerator ordered EL (index 2) after the
	// quantifier at index `on`.
	dependsOn := func(t *testing.T, coll values.Value, on int) bool {
		t.Helper()
		el := explodeQuantifierOver(coll)
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
	if !dependsOn(t, nameModelCollection(owner, flow), 1) {
		t.Fatal("fixture: the name-model Explode must depend on the FLOW leg (index 1), " +
			"or the assertion below is vacuous")
	}
	if dependsOn(t, nameModelCollection(owner, flow), 0) {
		t.Fatal("the name-model Explode depends on the OWNER (index 0) although its " +
			"collection references only the flow leg: the leg was recovered from the " +
			"dotted PREFIX of the display name. That recovery is deleted; the ordinal " +
			"bake supplies the edge structurally instead")
	}
}
