package cascades

// RFC-173 W5 commit 5 — the F4-rider DEAD-FOR-GATED pins on the dotted-prefix
// bipartition classifiers (MergeSeedLegsOfValue / quantifierMergeSeedLegDeps).
//
// The GATHERED unnest's Explode collection is a BAKED per-source reference —
// `ofOrdinal(QOV(owner, typ), FieldIndex(ARR))`, a bare column name with no
// dotted prefix — so the classifiers contribute NOTHING for a gated shape:
// bipartition validity is EMERGENT from the genuine correlation edge (the
// Explode's GetCorrelatedTo reports the OWNER — exactly Java's
// Quantifier.getCorrelatedTo edge). PHYSICAL DELETION IS DEFERRED (the F4
// rider): a multi-source unnest that DECLINES the gather (dup names) and the
// under-existential class still translate through the name-model residual,
// whose buried read `FieldValue{Field:"A.ARR", Child:QOV(rightmost)}` is a
// live dotted producer until the W4-left+EXISTS slice retires it — the
// residual-direction pin below is the reachability proof that keeps the code.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// gatheredExplodeQuantifier builds the W5 gathered Explode leg: collection =
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

// TestRFC173W5_ClassifierDeadForGated pins both directions of the F4 rider.
func TestRFC173W5_ClassifierDeadForGated(t *testing.T) {
	t.Parallel()

	// GATED direction: the baked per-source collection carries NO dotted
	// prefix — the classifier is DEAD for the gathered form, at the value
	// level and through the rule's quantifier-level wrapper.
	q, coll := gatheredExplodeQuantifier(t)
	if legs := values.MergeSeedLegsOfValue(coll); len(legs) != 0 {
		t.Fatalf("MergeSeedLegsOfValue(baked gathered collection) = %v, want empty — the dotted classifier must be DEAD for the gated form", legs)
	}
	if deps := quantifierMergeSeedLegDeps(q); len(deps) != 0 {
		t.Fatalf("quantifierMergeSeedLegDeps(gathered Explode) = %v, want empty (dead-for-gated)", deps)
	}

	// The EMERGENT replacement: the same quantifier's genuine correlation
	// edge reports the OWNER — bipartition keeps the Explode with its source
	// through GetCorrelatedTo, no dotted recovery needed (Java's
	// Quantifier.getCorrelatedTo edge).
	corr := q.GetRangesOver().GetCorrelatedTo()
	if _, hasOwner := corr[values.NamedCorrelationIdentifier("A")]; !hasOwner {
		t.Fatalf("gathered Explode GetCorrelatedTo = %v, want the OWNER A — the emergent sibling edge that replaces the classifier", corr)
	}

	// RESIDUAL direction (the F4 rider's reachability proof): the name-model
	// buried read — a DOTTED field off the merged rightmost leg — still
	// classifies, which is WHY the classifier survives W5 physically (the
	// decline/under-existential classes translate through it until the
	// W4-left+EXISTS slice kills the last dotted producer).
	buried := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B")),
		"A.ARR", values.UnknownType,
	)
	legs := values.MergeSeedLegsOfValue(buried)
	if _, hasA := legs[values.NamedCorrelationIdentifier("A")]; len(legs) != 1 || !hasA {
		t.Fatalf("MergeSeedLegsOfValue(residual dotted read) = %v, want {A} — the residual producer keeps the classifier alive (F4 rider)", legs)
	}
}
