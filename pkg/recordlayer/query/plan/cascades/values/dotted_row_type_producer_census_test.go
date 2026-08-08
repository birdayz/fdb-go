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

func TestDottedRowTypeProducerCensus_ClassifiesBothDirections(t *testing.T) {
	t.Parallel()
	// Driven through the recorder's pure input rather than the globals is not
	// possible here (the counters ARE the census), so this test asserts the
	// classification through a rendering of a locally built field list instead.
	dotted := []Field{{Name: "C.CV", Ordinal: 0}}
	plain := []Field{{Name: "CV", Ordinal: 0}}

	sawDotted := false
	for i := range dotted {
		sawDotted = sawDotted || nameIsQualified(dotted[i].Name)
	}
	sawPlain := false
	for i := range plain {
		sawPlain = sawPlain || nameIsQualified(plain[i].Name)
	}
	if !sawDotted {
		t.Fatal("a LEG.COL row did not classify as dotted; the census would report the " +
			"generic derivation path clean over exactly the rows it is meant to find")
	}
	if sawPlain {
		t.Fatal("a bare-column row classified as dotted; the census would report the " +
			"generic path as a producer of every ordinary record")
	}
}

// The floor's failure text has to say what a collapse re-arms. The census's
// finding is already in and it is NOT a zero — DOTTED came back 683 and 841 over
// two full-corpus runs, so this path IS the producer of the dotted row. What the
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

func TestDottedRowTypeProducerCensus_NoFloorNeverFails(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if AssertDottedRowTypeProducerCensus(&b, nil) {
		t.Fatalf("a nil floor failed: %q", b.String())
	}
	if AssertDottedRowTypeProducerCensus(&b, &DottedRowTypeProducerFloor{}) {
		t.Fatalf("a zero floor failed: %q", b.String())
	}
}
