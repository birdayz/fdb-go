package values

import (
	"strings"
	"testing"
)

// The DOTTED discriminator is the whole census: it decides which derivations
// count as "the row a leg-table population would target". Both directions are
// driven, and so is the rendered-composite exclusion, because a bare dot test
// classifies an ordinary one-column leg as dotted and would report the generic
// path as a producer of a row it never derives.

func TestNameIsQualified(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		want bool
		why  string
	}{
		{"C.CV", true, "the shape the executor's dotted reader answers on"},
		{"I.QTY", true, "the correlated-scalar seed's inner scalar leg label"},
		{"ID", false, "a bare column"},
		{"SUM(QTY)", false, "an aggregate title with no dot"},
		{"COUNT(*)", false, "an aggregate title with no dot"},
		{"{_0: C.ID#0}", false, "a RENDERED COMPOSITE: a record type's rendering carries " +
			"the dots of every qualified column inside it, so a bare dot test would call " +
			"an ordinary one-column leg dotted"},
		{"[C.ID]", false, "a rendered array, same reason"},
		{".LEADING", false, "a leading dot names no qualifier"},
		{"TRAILING.", false, "a trailing dot names no leaf"},
	} {
		if got := nameIsQualified(c.name); got != c.want {
			t.Errorf("nameIsQualified(%q) = %v, want %v — %s", c.name, got, c.want, c.why)
		}
	}
}

// TestDottedRowTypeProducerCensus_ClassifiesBothDirections drives the RECORDER,
// not a re-implementation of it.
//
// It used to walk a local `nameIsQualified` loop over a locally built field list
// and assert on that, which is a test of the loop it just wrote: the recorder
// could stop calling the discriminator, or bump the wrong counter, and the test
// would stay green. The state is now separable from the process globals, so the
// real classification path runs here.
func TestDottedRowTypeProducerCensus_ClassifiesBothDirections(t *testing.T) {
	t.Parallel()
	var c dottedRowTypeCounters
	c.record([]Field{{Name: "C.CV", Ordinal: 0}})
	if c.Dotted != 1 || c.Plain != 0 {
		t.Fatalf("a LEG.COL row recorded as dotted=%d plain=%d, want 1/0; the census would report "+
			"the generic derivation path clean over exactly the rows it is meant to find",
			c.Dotted, c.Plain)
	}
	c.record([]Field{{Name: "CV", Ordinal: 0}})
	if c.Plain != 1 || c.Dotted != 1 {
		t.Fatalf("a bare-column row recorded as dotted=%d plain=%d, want dotted unchanged at 1 and "+
			"plain 1; the census would report the generic path as a producer of every ordinary record",
			c.Dotted, c.Plain)
	}
	// ONE qualified name anywhere in the row makes the row dotted — the executor's
	// dotted arm answers on the column, not on the whole row being qualified.
	c.record([]Field{{Name: "ID", Ordinal: 0}, {Name: "NAME", Ordinal: 1}, {Name: "O.I.QTY", Ordinal: 2}})
	if c.Dotted != 2 {
		t.Fatalf("a row with one qualified name among three did not record as dotted (dotted=%d, "+
			"want 2); every measured witness has exactly this shape", c.Dotted)
	}
	if want := []string{"[C.CV]", "[ID NAME O.I.QTY]"}; strings.Join(c.Witnesses, " ") != strings.Join(want, " ") {
		t.Fatalf("witnesses = %v, want %v — the witnesses ARE the answer the census returns; the "+
			"counts are only traffic", c.Witnesses, want)
	}
	// A repeat shape must not consume a witness slot: the cap is small and a
	// duplicate-filled witness list would hide the shapes that matter.
	c.record([]Field{{Name: "C.CV", Ordinal: 0}})
	if len(c.Witnesses) != 2 {
		t.Fatalf("a repeated dotted shape added a witness (%v); the cap of %d would then fill with "+
			"duplicates and the distinct shapes would be lost", c.Witnesses, dottedRowTypeWitnessCap)
	}
	if c.Dotted != 3 {
		t.Fatalf("a repeated dotted shape did not count toward DOTTED (dotted=%d, want 3); the "+
			"witness dedup must not dedup the traffic the floor reads", c.Dotted)
	}
}

// TestRecordDottedRowTypeDerivation_FeedsTheProcessCounters drives the exported
// recorder by name, so the split between the state type and the globals cannot
// silently detach.
//
// Not parallel: it owns the process counters for its duration.
func TestRecordDottedRowTypeDerivation_FeedsTheProcessCounters(t *testing.T) {
	ResetDottedRowTypeProducerCensus()
	t.Cleanup(ResetDottedRowTypeProducerCensus)

	RecordDottedRowTypeDerivation([]Field{{Name: "C.CV", Ordinal: 0}})
	RecordDottedRowTypeDerivation([]Field{{Name: "CV", Ordinal: 0}})
	dotted, plain, witnesses := DottedRowTypeProducerCensus()
	if dotted != 1 || plain != 1 {
		t.Fatalf("RecordDottedRowTypeDerivation landed DOTTED %d plain %d, want 1/1 — the recorder "+
			"is not feeding the counters the census reads", dotted, plain)
	}
	if len(witnesses) != 1 || witnesses[0] != "[C.CV]" {
		t.Fatalf("witnesses = %v, want [[C.CV]]", witnesses)
	}
	// The snapshot must be a COPY: handing back the live slice lets a caller
	// mutate the census, and under a concurrent plan it is a data race.
	witnesses[0] = "clobbered"
	if _, _, again := DottedRowTypeProducerCensus(); again[0] != "[C.CV]" {
		t.Fatalf("mutating a returned witness slice changed the census (%q); the snapshot shares "+
			"state with the recorder", again[0])
	}
	ResetDottedRowTypeProducerCensus()
	if d, p, w := DottedRowTypeProducerCensus(); d != 0 || p != 0 || len(w) != 0 {
		t.Fatalf("after reset the census reads DOTTED %d plain %d witnesses %v, want empty", d, p, w)
	}
}

// TestDottedRowTypeProducerCensus_DottedFloorWatchesTheFinding is the arm that
// did not exist while the finding lived in a comment.
//
// The finding is "DOTTED is not zero". The total-derivations floor cannot watch
// it, and this states why in the shape it would really arrive: plain traffic
// intact, dotted collapsed to zero.
func TestDottedRowTypeProducerCensus_DottedFloorWatchesTheFinding(t *testing.T) {
	t.Parallel()
	// The measured corpus proportions with DOTTED knocked out.
	collapsed := dottedRowTypeCounters{Dotted: 0, Plain: 157511}
	floor := &DottedRowTypeProducerFloor{Derivations: 100, Dotted: 50}

	var b strings.Builder
	if !assertDottedRowTypeProducerCensusState(&b, collapsed, floor) {
		t.Fatalf("DOTTED collapsed to zero with plain traffic intact and the census PASSED. That is "+
			"the exact state in which RFC-212 §1.1's placement decision has lost its evidence, and "+
			"the total floor cannot see it — 157511 clears 100 by three orders of magnitude.\n%s",
			b.String())
	}
	msg := b.String()
	if strings.Contains(msg, "derivation(s) over the whole") {
		t.Fatalf("the total-derivations floor also fired, so this case does not isolate the DOTTED "+
			"floor: %s", msg)
	}
	for _, want := range []string{"ALARM DIRECTION: collapse", "INVERTED", "683"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the DOTTED floor's failure text omits %q. A guard whose direction and measured "+
				"expectation are unstated gets relaxed to match the run rather than "+
				"reconciled:\n%s", want, msg)
		}
	}
	// The floor must PASS at its boundary and at the measured reading, or a healthy
	// corpus reds.
	for _, ok := range []dottedRowTypeCounters{
		{Dotted: 50, Plain: 157511},
		{Dotted: 683, Plain: 157511},
		{Dotted: 841, Plain: 157935},
		{Dotted: 681, Plain: 157699},
	} {
		var w strings.Builder
		if assertDottedRowTypeProducerCensusState(&w, ok, floor) {
			t.Fatalf("a healthy reading (DOTTED %d, plain %d) failed: %s", ok.Dotted, ok.Plain, w.String())
		}
	}
}

// TestDottedRowTypeProducerCensus_TotalFloorStillWatchesTheInstrument keeps the
// FIRST floor falsifiable after the second arrived, and pins that the two are
// independent: a run with healthy dotted traffic and no plain traffic at all is
// still a broken instrument.
func TestDottedRowTypeProducerCensus_TotalFloorStillWatchesTheInstrument(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	dark := dottedRowTypeCounters{Dotted: 0, Plain: 0}
	if !assertDottedRowTypeProducerCensusState(&b, dark, &DottedRowTypeProducerFloor{Derivations: 100}) {
		t.Fatalf("a census that recorded NOTHING passed the total floor: %s", b.String())
	}
	if !strings.Contains(b.String(), "RE-ARMS") {
		t.Fatalf("the total floor's failure does not say what a collapse re-arms: %q", b.String())
	}
	// Independence: only the total floor set, dotted healthy, plain gone.
	var c strings.Builder
	if !assertDottedRowTypeProducerCensusState(&c, dottedRowTypeCounters{Dotted: 60, Plain: 0},
		&DottedRowTypeProducerFloor{Derivations: 100, Dotted: 50}) {
		t.Fatalf("plain traffic vanished entirely and the census passed because DOTTED cleared its "+
			"own floor; the two populations must be judged separately: %s", c.String())
	}
}

// The floor's failure text has to say what a collapse re-arms. The census's
// finding is already in and it is NOT a zero — DOTTED came back 681, 683 and 841 over
// three full-corpus runs, so this path IS the producer of the dotted row. What the
// floor still guards is the READING: an unreached counter prints identically to a
// measured one, so a collapse would silently un-measure the producer set rather
// than report a quiet corpus.
func TestDottedRowTypeProducerCensus_FloorSaysWhatItReArms(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if !AssertDottedRowTypeProducerCensus(&b, &DottedRowTypeProducerFloor{Derivations: 1 << 30}) {
		t.Fatal("an unreachably high floor passed; the floor cannot fail and the zero " +
			"it guards is vacuous")
	}
	msg := b.String()
	if !strings.Contains(msg, "RE-ARMS") {
		t.Fatalf("the floor failure does not say what a collapse re-arms: %q", msg)
	}
	if !strings.Contains(msg, "refineRowTypes") {
		t.Fatalf("the floor failure does not name the reader that makes a second "+
			"unpopulated producer a plan-level conflict: %q", msg)
	}
}

// TestDottedRowTypeProducerCensus_NoFloorNeverFails pins the vacuity guard the
// narrowed-run wrapper depends on. Both floors must be withheld together — a
// version that withheld only Derivations would red every narrowed run on the
// DOTTED count instead, which is the same defect wearing the other floor.
func TestDottedRowTypeProducerCensus_NoFloorNeverFails(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if AssertDottedRowTypeProducerCensus(&b, nil) {
		t.Fatalf("a nil floor failed: %q", b.String())
	}
	if AssertDottedRowTypeProducerCensus(&b, &DottedRowTypeProducerFloor{}) {
		t.Fatalf("a zero floor failed: %q", b.String())
	}
	// Over EXPLICIT empty state, so the guard does not depend on what the process
	// counters happen to hold when this test runs.
	if assertDottedRowTypeProducerCensusState(&b, dottedRowTypeCounters{}, nil) {
		t.Fatalf("a nil floor failed over an empty census: %q", b.String())
	}
	if assertDottedRowTypeProducerCensusState(&b, dottedRowTypeCounters{}, &DottedRowTypeProducerFloor{}) {
		t.Fatalf("a zero-valued floor failed over an empty census: %q", b.String())
	}
	if b.Len() != 0 {
		t.Fatalf("a no-op assertion wrote %q; it must be silent", b.String())
	}
}
