package values

import (
	"strings"
	"testing"
)

// THE UNIT PIN FOR THE ATTRIBUTION CENSUS.
//
// The corpus reading is not a substitute for this and the difference is not
// theoretical here. Over two full uncached runs the corpus drove exactly TWO of
// this census's arms — all-unattributed once, all-attributed once — and never
// the middle one. That middle arm (owner matches, title does not) is what fires
// when a leg is RETITLED between mint and read, which is precisely what the
// RFC-212 §11.3 retitling does by definition. Shipping the conversion without
// this file would have moved the population into a branch no test had exercised,
// whose failure message would then have been read as a finding rather than as an
// untested branch.
//
// Every arm, both floors, and the collision guard are driven from EXPLICIT
// state, which is why the census's decisions were split away from its globals.

func mintedState(pairs map[string]mintedLeg, observed map[string]string) attributionState {
	return attributionState{minted: pairs, observed: observed}
}

// ARM 1 — ATTRIBUTED: owner is a minted inner leg and the title IS the name.
func TestAttribution_ArmAttributed(t *testing.T) {
	t.Parallel()
	s := mintedState(
		map[string]mintedLeg{"q$1": {producer: InnerLegProducerSingleSource, title: "I.QTY"}},
		map[string]string{"I.QTY": "q$1"},
	)
	a, u, minted, coll := classifyAttribution(s)
	if len(a) != 1 || len(u) != 0 {
		t.Fatalf("attributed=%v unattributed=%v, want exactly one attributed", a, u)
	}
	if minted != 1 || len(coll) != 0 {
		t.Fatalf("minted=%d collisions=%v", minted, coll)
	}
	if !strings.Contains(a[0], "scalarSubqueryOrdinalSeed") {
		t.Fatalf("the attributed row does not name its producer: %q.\n"+
			"  The producer is named PER ROW precisely because the header must not "+
			"name one; if the row drops it too, the report states no producer at all.", a[0])
	}
}

// ARM 2 — RETITLED: owner IS a minted inner leg, title is NOT the name.
//
// THIS IS THE ARM NO CORPUS RUN HAS EVER REACHED, and the one the §11.3
// retitling makes reachable. A name-only matcher cannot express it at all: with
// no owner key there is no state in which the identity matches and the string
// does not.
func TestAttribution_ArmRetitledBetweenMintAndRead(t *testing.T) {
	t.Parallel()
	s := mintedState(
		map[string]mintedLeg{"q$1": {producer: InnerLegProducerSingleSource, title: "QTY"}},
		map[string]string{"I.QTY": "q$1"},
	)
	a, u, _, _ := classifyAttribution(s)
	if len(a) != 0 || len(u) != 1 {
		t.Fatalf("attributed=%v unattributed=%v.\n"+
			"  A leg whose owner matches but whose TITLE does not must NOT count as\n"+
			"  attributed: the census's claim is that the producer named THIS name, and a\n"+
			"  different title is a different claim.", a, u)
	}
	if !strings.Contains(u[0], "retitled or") {
		t.Fatalf("the RETITLED arm does not distinguish itself from the third-producer "+
			"arm: %q.\n"+
			"  The two mean opposite things — one says the right producer renamed the leg,\n"+
			"  the other says a different producer owns it — and after the §11.3 retitling\n"+
			"  the first becomes the EXPECTED state. Reported as the second, it reads as a\n"+
			"  finding rather than as the conversion working.", u[0])
	}
	if strings.Contains(u[0], "NEITHER") {
		t.Fatalf("the RETITLED arm is being reported with the third-producer wording: %q", u[0])
	}
}

// ARM 3 — THIRD PRODUCER: owner was minted by neither seed.
func TestAttribution_ArmThirdProducer(t *testing.T) {
	t.Parallel()
	s := mintedState(
		map[string]mintedLeg{"q$1": {producer: InnerLegProducerClusteredOuter, title: "C.CV"}},
		map[string]string{"I.QTY": "q$999"},
	)
	a, u, _, _ := classifyAttribution(s)
	if len(a) != 0 || len(u) != 1 {
		t.Fatalf("attributed=%v unattributed=%v, want exactly one unattributed", a, u)
	}
	if !strings.Contains(u[0], "NEITHER") {
		t.Fatalf("the third-producer arm does not say so: %q", u[0])
	}
}

// The arms must not be reachable by NAME alone: a name that matches a minted
// title under a DIFFERENT owner is not attributed. This is the property that
// makes the census a measurement rather than a string comparison.
func TestAttribution_NameMatchUnderWrongOwnerIsNotAttributed(t *testing.T) {
	t.Parallel()
	s := mintedState(
		map[string]mintedLeg{"q$1": {producer: InnerLegProducerSingleSource, title: "I.QTY"}},
		map[string]string{"I.QTY": "q$2"}, // same NAME, different owner
	)
	a, u, _, _ := classifyAttribution(s)
	if len(a) != 0 {
		t.Fatalf("a name matching a minted TITLE under a different OWNER was attributed: %v.\n"+
			"  Attribution would then be a name match wearing an identity's clothes, and\n"+
			"  with hundreds of minted titles a corpus a spurious hit is a matter of time.\n"+
			"  WHAT THIS RE-ARMS: every attribution conclusion in RFC-212 §10 and §11.", a)
	}
	if len(u) != 1 || !strings.Contains(u[0], "NEITHER") {
		t.Fatalf("unattributed=%v, want the third-producer arm", u)
	}
}

// The OBSERVED floor: a partition over no names is vacuous.
func TestAttribution_ObservedFloorFires(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	s := mintedState(map[string]mintedLeg{"q$1": {title: "X"}}, nil)
	if !assertAttribution(&b, s, &DottedWitnessFloors{Observed: 1}) {
		t.Fatal("an EMPTY observed population passed the floor; the partition is then " +
			"vacuous and prints exactly like a decided one")
	}
	if !strings.Contains(b.String(), "RE-ARMS") {
		t.Fatalf("the floor failure does not say what a collapse re-arms: %q", b.String())
	}
}

// The MINTED floor — the direction round 1's whole finding rested on, read from
// a log rather than asserted.
func TestAttribution_MintedFloorFires(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	// Names observed, but NOTHING registered: every name reports unattributed,
	// vacuously, and reads identically to a real refutation.
	s := mintedState(nil, map[string]string{"I.QTY": "q$1"})
	if !assertAttribution(&b, s, &DottedWitnessFloors{Observed: 1, Minted: 1}) {
		t.Fatal("ZERO minted registrations passed the floor.\n" +
			"  With no registrations every observed name lands in the third-producer arm,\n" +
			"  so the census reports a clean refutation of whichever producer the RFC\n" +
			"  names — from an instrument that recorded nothing.")
	}
	msg := b.String()
	if !strings.Contains(msg, "inner-leg title(s) minted") {
		t.Fatalf("the minted floor did not fire (some other check did): %q", msg)
	}
	if !strings.Contains(msg, "demonstrably live") {
		t.Fatalf("the minted floor's message does not state why the direction matters: %q", msg)
	}
}

// Floors satisfied and no collisions: the census must pass.
func TestAttribution_HealthyStatePasses(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	s := mintedState(
		map[string]mintedLeg{"q$1": {producer: InnerLegProducerSingleSource, title: "I.QTY"}},
		map[string]string{"I.QTY": "q$1"},
	)
	if assertAttribution(&b, s, &DottedWitnessFloors{Observed: 1, Minted: 1}) {
		t.Fatalf("a healthy state failed: %q", b.String())
	}
	if assertAttribution(&b, s, nil) {
		t.Fatalf("nil floors failed: %q", b.String())
	}
}

// The OWNER COLLISION guard: a name answered under two owners has two answers,
// and first-writer-wins would print one of them as the whole truth.
func TestAttribution_OwnerCollisionIsReportedNotDropped(t *testing.T) {
	t.Parallel()
	ResetDottedWitnessAttribution()
	t.Cleanup(ResetDottedWitnessAttribution)

	RecordDottedArmAnswer("I.QTY", NamedCorrelationIdentifier("q$1"))
	RecordDottedArmAnswer("I.QTY", NamedCorrelationIdentifier("q$1")) // same owner: not a collision
	RecordDottedArmAnswer("I.QTY", NamedCorrelationIdentifier("q$2")) // DIFFERENT owner

	s := snapshotAttribution()
	if len(s.collisions) != 1 {
		t.Fatalf("collisions=%v, want exactly one.\n"+
			"  A repeat of the SAME owner is not a collision; a DIFFERENT owner is. The\n"+
			"  census's claim is per name, so a name with two owners has two answers and\n"+
			"  the report shows one.", s.collisions)
	}
	var b strings.Builder
	if !assertAttribution(&b, s, nil) {
		t.Fatal("an owner collision passed. The comment claiming a second owner 'would " +
			"itself be a finding' would then be describing something nothing reports.")
	}
	if !strings.Contains(formatAttribution(s), "OWNER COLLISIONS") {
		t.Fatalf("the collision is not rendered:\n%s", formatAttribution(s))
	}
}

// The HEADER must not name a producer. Guarded by a comment until now, which is
// how the header came to disagree with its own rows in the first place.
func TestAttribution_HeaderNamesNoProducer(t *testing.T) {
	t.Parallel()
	s := mintedState(
		map[string]mintedLeg{"q$1": {producer: InnerLegProducerSingleSource, title: "I.QTY"}},
		map[string]string{"I.QTY": "q$1"},
	)
	out := formatAttribution(s)
	header := out
	if i := strings.Index(out, "\n    "); i >= 0 {
		header = out[:i]
	}
	for _, producer := range []string{
		InnerLegProducerClusteredOuter.String(),
		InnerLegProducerSingleSource.String(),
	} {
		if strings.Contains(header, producer) {
			t.Fatalf("the header names producer %q:\n%s\n"+
				"  The header must name NONE: there are two seeds, and a header naming one\n"+
				"  states a conclusion its own rows can contradict — which it did, reporting\n"+
				"  'ATTRIBUTED to clusteredOuterOrdinalSeed' over rows naming\n"+
				"  scalarSubqueryOrdinalSeed. Substituting the currently-right name would go\n"+
				"  stale the first time a third producer attributed; omitting it removes the\n"+
				"  class.", producer, out)
		}
	}
	if !strings.Contains(out, InnerLegProducerSingleSource.String()) {
		t.Fatal("the producer appears nowhere in the report; it belongs on the ROW")
	}
}
