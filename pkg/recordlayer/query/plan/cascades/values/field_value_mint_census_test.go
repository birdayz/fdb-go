package values

import (
	"fmt"
	"strings"
	"testing"
)

// The mint census and the ordering-bridge census, driven directly.
//
// Both of this pair's headline results are NEGATIVE — zero dotted constructor
// mints in 1 756 315, zero dotted values reaching the ordering bridge — and a
// negative result is worth exactly as much as the instrument that produced it.
// An always-zero classifier and an empty population print identically. These
// make the classifier a tested function so the zeros mean what they say.

func mintCensusOn(t *testing.T) {
	t.Helper()
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(true)
	ResetFieldValueMintCensus()
	ResetOrderingBridgeDottedCensus()
	t.Cleanup(func() {
		ResetFieldValueMintCensus()
		ResetOrderingBridgeDottedCensus()
		SetLegIdentityCensusEnabled(prev)
	})
}

// TestClassifyFieldMint pins the classifier, including the discriminator that
// separates the two dotted classes.
//
// The '#' test is the load-bearing one: it is what distinguishes a name someone
// composed from a qualifier (`T.CITY` — missing identity, ordinary debt) from a
// name that came out of a RENDERER (`q$1.AID#0` — display output fed back in as
// identity, a layering violation). Collapsing the two would have made the
// corpus's zero unreadable, because the interesting class would have been hidden
// inside the ordinary one.
func TestClassifyFieldMint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		field string
		baked bool
		want  FieldMintClass
	}{
		{"plain lazy name", "AID", false, FieldMintLazyBare},
		{"qualifier composed onto a leaf", "T.CITY", false, FieldMintLazyDotted},
		{"nested path arriving lazy", "ADDR.CITY", false, FieldMintLazyDotted},
		{"rendered Explain label", "q$50765.AID#0", false, FieldMintLazyExplainRendered},
		{"empty lazy name", "", false, FieldMintLazyEmpty},
		{"baked wins over every spelling", "q$1.AID#0", true, FieldMintBaked},
		{"baked plain", "AID", true, FieldMintBaked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyFieldMint(tc.field, tc.baked); got != tc.want {
				t.Fatalf("ClassifyFieldMint(%q, baked=%v) = %v, want %v.\n"+
					"The two dotted classes are separated by '#', which is the ordinal "+
					"discriminator explainValueOrdinals appends and nothing in a schema "+
					"ever contains. Collapsing them hides a layering violation inside "+
					"ordinary missing-identity debt.", tc.field, tc.baked, got, tc.want)
			}
		})
	}
}

// TestFieldMintCensus_PartitionHoldsAgainstIndependentTotal pins the property
// that lets the corpus zeros be believed: the total is counted independently of
// the classes, so a mint that no arm classified shows up as a partition failure
// rather than silently reducing a class to zero.
func TestFieldMintCensus_PartitionHoldsAgainstIndependentTotal(t *testing.T) {
	mintCensusOn(t)
	NoteFieldValueMint("AID", false)
	NoteFieldValueMint("T.CITY", false)
	NoteFieldValueMint("q$1.AID#0", false)
	NoteFieldValueMint("", false)
	NoteFieldValueMint("AID", true)

	total, counts := FieldValueMintCensus()
	if total != 5 {
		t.Fatalf("independent total = %d, want 5", total)
	}
	sum := 0
	for _, n := range counts {
		sum += n
	}
	if sum != total {
		t.Fatalf("classes sum to %d against an independent total of %d (%v).\n"+
			"The denominator is incremented before any classification precisely so "+
			"this can fail; a census whose total is the sum of its own classes cannot "+
			"detect an unclassified path and its zeros mean nothing.", sum, total, counts)
	}
	if counts[FieldMintLazyExplainRendered] != 1 || counts[FieldMintLazyDotted] != 1 {
		t.Fatalf("dotted classes = %v, want one of each — these are the only two "+
			"classes the corpus result rests on", counts)
	}
}

// TestFieldMintCensus_CapturesOriginForDottedOnly pins that a stack is taken for
// the classes that need one and NOT for the hot ones. Capturing a stack per
// lazy-bare mint would be a profiler: the corpus saw 437 249 of them.
func TestFieldMintCensus_CapturesOriginForDottedOnly(t *testing.T) {
	mintCensusOn(t)
	NoteFieldValueMint("AID", false)
	if got := FieldValueMintOrigins(); len(got) != 0 {
		t.Fatalf("a stack was captured for a lazy-BARE mint (%d origins). That class "+
			"ran 437 249 times on the corpus; capturing there turns a census into a "+
			"profiler.", len(got))
	}
	NoteFieldValueMint("q$1.AID#0", false)
	origins := FieldValueMintOrigins()
	if len(origins) != 1 || !strings.Contains(origins[0], "q$1.AID#0") {
		t.Fatalf("no origin captured for the Explain-rendered mint: %v\n"+
			"The class count says a rendering was stored as a name; only the stack "+
			"says who stored it, and that is the whole reason to instrument the mint "+
			"rather than the consumer.", origins)
	}
}

// TestFieldMintCensus_DisabledRecordsNothing pins the gate on the hot path.
func TestFieldMintCensus_DisabledRecordsNothing(t *testing.T) {
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	ResetFieldValueMintCensus()
	t.Cleanup(func() {
		ResetFieldValueMintCensus()
		SetLegIdentityCensusEnabled(prev)
	})
	NoteFieldValueMint("q$1.AID#0", false)
	if total, counts := FieldValueMintCensus(); total != 0 || counts != [fieldMintClassCount]int{} {
		t.Fatalf("the mint census recorded total=%d %v while DISABLED. FieldValue "+
			"construction ran 1 756 315 times on one corpus run; counting there in "+
			"production is not acceptable.", total, counts)
	}
}

// TestOrderingBridgeDottedCensus_OnlyCountsDottedAttempts pins the filter.
//
// This census exists to answer one question — does a flat-dotted value reach a
// comparison that ANSWERS rather than declines — and its corpus result is zero.
// If it counted undotted attempts too, that zero would be arithmetically
// impossible and the instrument would obviously be broken; if it counted none at
// all, the zero would be indistinguishable from the real finding. This pins the
// middle.
func TestOrderingBridgeDottedCensus_OnlyCountsDottedAttempts(t *testing.T) {
	mintCensusOn(t)
	NoteOrderingBridgeDotted("AID", "AID", true) // undotted: ignored
	NoteOrderingBridgeDotted("q$1.AID#0", "AID", false)
	NoteOrderingBridgeDotted("AID", "T.CITY", false)
	NoteOrderingBridgeDotted("T.CITY", "T.CITY", true)

	total, counts, ws := OrderingBridgeDottedCensus()
	if total != 3 {
		t.Fatalf("attempts = %d, want 3 (the undotted pair must not be counted; a "+
			"census that counted every bridge attempt could never report the zero "+
			"this one exists to report)", total)
	}
	if counts[BridgeDottedAnsweredFalse] != 2 || counts[BridgeDottedAnsweredTrue] != 1 {
		t.Fatalf("class vector %v, want 2 answered-false and 1 answered-true. "+
			"ANSWERED-FALSE is the suspect class: a value whose name is a rendering "+
			"reports 'different column' where the same column spelled plainly would "+
			"have bridged.", counts)
	}
	if len(ws) != 3 {
		t.Fatalf("witnesses = %v, want 3 pairs — the pair is what identifies which "+
			"two spellings failed to meet", ws)
	}
}

// TestOrderingBridgeDottedCensus_RareClassSurvivesTheCommonOne pins the per-class
// witness cap, which RFC-215 section 6.2 states as a law after the same bug was
// found and fixed in the mint census.
//
// ANSWERED-FALSE is the common class by this census's own thesis; ANSWERED-TRUE --
// a dotted comparison that SUCCEEDED -- is the rare and surprising one, and it is
// the reason to look at witnesses at all. Under a cap shared across classes, the
// common class fills the map and the finding is never recorded. Counts stay
// correct throughout, which is what makes the failure quiet: the numbers look
// right and the diagnosis is simply absent.
func TestOrderingBridgeDottedCensus_RareClassSurvivesTheCommonOne(t *testing.T) {
	mintCensusOn(t)
	// Flood the COMMON class well past the cap.
	for i := 0; i < fieldMintOriginCap*3; i++ {
		NoteOrderingBridgeDotted(fmt.Sprintf("q$%d.A#0", i), "A", false)
	}
	// One witness of the RARE class, arriving last — the worst case.
	NoteOrderingBridgeDotted("T.CITY", "T.CITY", true)

	_, counts, ws := OrderingBridgeDottedCensus()
	if counts[BridgeDottedAnsweredTrue] != 1 {
		t.Fatalf("the rare class COUNT was lost: %v", counts)
	}
	found := false
	for k := range ws {
		if strings.HasPrefix(k, BridgeDottedAnsweredTrue.String()+" :: ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rare class produced a COUNT but no WITNESS: %d witnesses, none "+
			"of class %v.\nA cap shared across classes lets %d common-class pairs "+
			"evict the one observation the census exists to explain. The counts stay "+
			"true, so nothing looks wrong — which is exactly how this went unnoticed "+
			"in the mint census until it was audited.",
			len(ws), BridgeDottedAnsweredTrue, fieldMintOriginCap*3)
	}
}

// TestOrderingBridgeDottedCensus_DisabledRecordsNothing pins its gate too.
func TestOrderingBridgeDottedCensus_DisabledRecordsNothing(t *testing.T) {
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	ResetOrderingBridgeDottedCensus()
	t.Cleanup(func() {
		ResetOrderingBridgeDottedCensus()
		SetLegIdentityCensusEnabled(prev)
	})
	NoteOrderingBridgeDotted("q$1.AID#0", "AID", false)
	if total, _, _ := OrderingBridgeDottedCensus(); total != 0 {
		t.Fatalf("the bridge census recorded %d attempts while DISABLED", total)
	}
}

// BenchmarkFieldValueMintGateOff measures what production pays for the mint
// census when it is disabled — the number the ruling asked for rather than
// asserted. FieldValue construction is hot (1 756 315 constructor mints in one
// corpus run), so "one atomic load" is a claim that needs a figure behind it.
func BenchmarkFieldValueMintGateOff(b *testing.B) {
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	b.Cleanup(func() { SetLegIdentityCensusEnabled(prev) })
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		NoteFieldValueMint("AID", false)
	}
}

// BenchmarkNewFlatFieldValueGateOff measures the same cost where it is actually
// paid: through the constructor, so the figure includes the call the hook adds
// rather than only the hook body.
func BenchmarkNewFlatFieldValueGateOff(b *testing.B) {
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	b.Cleanup(func() { SetLegIdentityCensusEnabled(prev) })
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewFlatFieldValue("AID", UnknownType)
	}
}
