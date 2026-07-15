package cascades

// Pins on the dotted-prefix bipartition classifiers (MergeSeedLegsOfValue /
// quantifierMergeSeedLegDeps), in both directions.
//
// A gathered unnest's Explode collection is a BAKED per-source reference —
// `ofOrdinal(QOV(owner, typ), FieldIndex(ARR))`, a bare column name with no
// dotted prefix — so the classifiers contribute NOTHING for this shape:
// bipartition validity instead comes from the quantifier's genuine
// correlation edge (the Explode's GetCorrelatedTo reports the OWNER —
// exactly Java's Quantifier.getCorrelatedTo edge).
//
// The classifiers can't be deleted, though: a multi-source unnest that
// declines to gather (duplicate column names) still resolves through a
// dotted name-model read — `FieldValue{Field:"A.ARR", Child:QOV(rightmost)}`
// — a live dotted producer. The residual-direction pin below is the
// reachability proof that keeps the classifiers in place.

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

// TestMergeSeedLegClassifier_GatheredVsResidual pins both directions of the
// dotted-prefix classifier: dead for a gathered (ordinal) collection, alive
// for a residual dotted read.
func TestMergeSeedLegClassifier_GatheredVsResidual(t *testing.T) {
	t.Parallel()

	// Gathered direction: the baked per-source collection carries NO dotted
	// prefix — the classifier is DEAD for this form, at the value level and
	// through the rule's quantifier-level wrapper.
	q, coll := gatheredExplodeQuantifier(t)
	if legs := values.MergeSeedLegsOfValue(coll); len(legs) != 0 {
		t.Fatalf("MergeSeedLegsOfValue(baked gathered collection) = %v, want empty — the dotted classifier must be DEAD for a gathered (ordinal) collection", legs)
	}
	if deps := quantifierMergeSeedLegDeps(q); len(deps) != 0 {
		t.Fatalf("quantifierMergeSeedLegDeps(gathered Explode) = %v, want empty (dead for a gathered collection)", deps)
	}

	// The replacement: the same quantifier's genuine correlation edge
	// reports the OWNER — bipartition keeps the Explode with its source
	// through GetCorrelatedTo, no dotted recovery needed (Java's
	// Quantifier.getCorrelatedTo edge).
	corr := q.GetRangesOver().GetCorrelatedTo()
	if _, hasOwner := corr[values.NamedCorrelationIdentifier("A")]; !hasOwner {
		t.Fatalf("gathered Explode GetCorrelatedTo = %v, want the OWNER A — the correlation edge that makes the classifier unnecessary here", corr)
	}

	// Residual direction: a name-model buried read — a DOTTED field off the
	// merged rightmost leg — still classifies. This is why the classifier
	// stays: a multi-source unnest that declines to gather (duplicate
	// column names) resolves through exactly this dotted read.
	buried := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B")),
		"A.ARR", values.UnknownType,
	)
	legs := values.MergeSeedLegsOfValue(buried)
	if _, hasA := legs[values.NamedCorrelationIdentifier("A")]; len(legs) != 1 || !hasA {
		t.Fatalf("MergeSeedLegsOfValue(residual dotted read) = %v, want {A} — the residual producer keeps the classifier alive", legs)
	}
}
