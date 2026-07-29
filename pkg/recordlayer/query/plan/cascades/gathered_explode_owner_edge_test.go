package cascades

// Pins the correlation edge that carries a lateral UNNEST's dependency on the
// source its array lives in.
//
// A bipartition that separates an Explode from that source materializes the
// Explode against a row where the array column is unbound — zero rows. The edge
// that forbids it used to be reconstructed from a STRING: the collection was
// minted as `FieldValue{Field:"A.ARR", Child:QOV(B)}` over the merged row, and a
// classifier sliced "A" off the field name to re-add the dependency
// GetCorrelatedTo could not see. That reconstruction decided a quantifier's
// identity by text, so it lost the edge for a leg addressed by a minted binding
// and invented one for any sibling sharing the alias.
//
// The collection is now an ordinal bake over the OWNER's own quantifier, so the
// dependency IS the collection's correlation — Java's Quantifier.getCorrelatedTo
// edge, nothing reconstructed. This test pins that edge; its producer-side twin
// (TestExplodeCollectionsAreOrdinalBaked, pkg/relational/core/query) pins that
// the translator emits no other collection shape.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// gatheredExplodeQuantifier builds a gathered Explode leg: collection =
// baked ofOrdinal over the OWNER's own typed QOV (the per-source edge).
func gatheredExplodeQuantifier(t *testing.T) (expressions.Quantifier, *values.FieldValue) {
	t.Helper()
	ownerType := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "WARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	ownerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), ownerType)
	coll, err := values.NewFieldValueOfOrdinal(ownerQOV, 1)
	if err != nil {
		t.Fatalf("bake collection: %v", err)
	}
	explode := expressions.NewExplodeExpressionWithOrdinality(coll, false)
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("EL"), expressions.InitialOf(explode),
	), coll
}

// TestGatheredExplodeCorrelatesToOwner pins that the ordinal-baked collection
// carries the owner dependency structurally — the fact that made the dotted
// recovery unnecessary. If this goes red, a bipartition can separate an Explode
// from its array source again and the zero-rows shape is back.
func TestGatheredExplodeCorrelatesToOwner(t *testing.T) {
	t.Parallel()

	q, coll := gatheredExplodeQuantifier(t)

	// The collection itself reports the owner: it is addressed by the owner's
	// ordinal, not by a name that has to be re-parsed to find the owner.
	if corr := values.GetCorrelatedToOfValue(coll); len(corr) != 1 {
		t.Fatalf("baked collection GetCorrelatedTo = %v, want exactly the OWNER — a collection correlating to anything else cannot carry the dependency", corr)
	}
	if _, hasOwner := values.GetCorrelatedToOfValue(coll)[values.NamedCorrelationIdentifier("A")]; !hasOwner {
		t.Fatal("baked collection does not correlate to its owner A")
	}

	// And it survives to the quantifier level, which is what the bipartition
	// rules consult (Java Quantifier.getCorrelatedTo).
	corr := q.GetRangesOver().GetCorrelatedTo()
	if _, hasOwner := corr[values.NamedCorrelationIdentifier("A")]; !hasOwner {
		t.Fatalf("gathered Explode GetCorrelatedTo = %v, want the OWNER A — the edge a bipartition must not cross", corr)
	}
}
