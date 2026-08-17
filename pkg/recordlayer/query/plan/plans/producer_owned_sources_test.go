package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestProducerOwnedCorrelationsAdmitsExactlyItsTwoProvenSources drives every arm
// of the ownership set, because the corpus reaches only the arms real plans
// happen to produce and this set is what decides whether the producer bridge may
// claim a root at all.
//
// The set is the whole safety property: a root inside it is resolved by the
// bridge's correlation/name matching, and a root outside it comes back
// untouched. So an arm that wrongly ADMITS re-opens the false-accept path (a
// foreign leg's column answered under a same-spelled slot), and an arm that
// wrongly OMITS leaves a legitimate value unresolved. Both directions are
// checked here.
func TestProducerOwnedCorrelationsAdmitsExactlyItsTwoProvenSources(t *testing.T) {
	t.Parallel()

	row := values.NewRecordType("R", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	referenced := values.NamedCorrelationIdentifier("REFERENCED")
	foreign := values.NamedCorrelationIdentifier("FOREIGN")

	referencedSource := mustOrdinalLayoutQOV(t, referenced, row)
	referencedID, err := values.ResolveFieldOrdinals(referencedSource, []int{0})
	if err != nil {
		t.Fatalf("referenced field: %v", err)
	}
	producer := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: referencedID})

	owned := producerOwnedCorrelations(producer)

	for _, arm := range []struct {
		what        string
		correlation values.CorrelationIdentifier
		want        bool
		why         string
	}{
		{
			"the current carrier", values.CurrentCorrelation(), true,
			"a producer always owns its own output row; dropping it would leave every " +
				"already-resolved current-rooted program unplaceable",
		},
		{
			"a root the producer program itself references", referenced, true,
			"the producer literally reads this root, which is direct evidence — this is " +
				"what lets a nominal rename cross on a proof instead of on a spelling",
		},
		{
			"an unrelated correlation", foreign, false,
			"the producer never reads it, so no slot can denote it. Admitting it is the " +
				"false accept that reads another leg's column at execution",
		},
	} {
		if _, isOwned := owned[arm.correlation]; isOwned != arm.want {
			t.Errorf("owned[%s] = %v, want %v — %s", arm.what, isOwned, arm.want, arm.why)
		}
	}

	// Exactly two: the carrier plus the one root read. A third entry means some
	// arm started inventing evidence.
	if len(owned) != 2 {
		t.Errorf("a one-source producer owns %d correlations, want 2 (its carrier and the "+
			"root it reads); an extra entry is un-evidenced fallback surface", len(owned))
	}

	// The zero correlation is not a name; admitting it would let every value
	// carrying an unset alias match the same bucket.
	if _, isOwned := owned[values.CorrelationIdentifier{}]; isOwned {
		t.Error("the zero correlation entered the owned set; an unset alias is absence of " +
			"an owner, not an owner every unset value shares")
	}

	// A nil producer is a real state at construction time. The set must still
	// carry the carrier — otherwise the bridge declines its own output row —
	// and must not invent anything else.
	bare := producerOwnedCorrelations(nil)
	if _, isOwned := bare[values.CurrentCorrelation()]; !isOwned {
		t.Error("a nil producer dropped the current carrier from its own owned set")
	}
	if len(bare) != 1 {
		t.Errorf("a nil producer owns %d correlations, want exactly the current carrier",
			len(bare))
	}
}
