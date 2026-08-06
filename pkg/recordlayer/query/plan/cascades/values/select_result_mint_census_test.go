package values

import (
	"strings"
	"testing"
)

// The mint census must be able to tell a typed select result value from an
// untyped one, and must spell WHICH type.
//
// This is the dimension the FlatMap producer census could not supply. That
// census attributes a value to the construction that handed it to a plan, and
// three of its sites merely flow sel.GetResultValue() verbatim — the same shape
// Java's three RecordQueryFlatMapPlan constructions have
// (ImplementNestedLoopJoinRule.java:187,201,214). Counting a courier's traffic
// as production is what booked this divergence against the wrong sites.
func TestSelectResultMintCensus_SeparatesTypedFromUntyped(t *testing.T) {
	t.Parallel()

	corr := NamedCorrelationIdentifier("A")
	rowType := &RecordType{Fields: []Field{{Name: "ID", Ordinal: 0}, {Name: "K", Ordinal: 1}}}

	for _, tc := range []struct {
		name  string
		qov   *QuantifiedObjectValue
		typed bool
		spell string
	}{
		{
			// What Java always builds: overQuantifier.getFlowedObjectValue().
			name:  "a typed mint reports its ARITY",
			qov:   NewQuantifiedObjectValueOfType(corr, rowType),
			typed: true,
			spell: "RecordType(2)",
		},
		{
			// What the translator actually builds, and what Java cannot express.
			name:  "a bare mint reports UNKNOWN, not an arity",
			qov:   NewQuantifiedObjectValue(corr),
			typed: false,
			spell: "UNKNOWN",
		},
		{
			// An explicit UnknownType is untyped for the same reason the implicit
			// one is: the placeholder is the absence of a type, not a type. A
			// `Typ != nil` spelling would call this typed, which is the tautology
			// that let a whole population read as converted on day one.
			name:  "an EXPLICIT UnknownType is untyped too",
			qov:   NewQuantifiedObjectValueOfType(corr, UnknownType),
			typed: false,
			spell: "UNKNOWN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := quantifiedObjectValueCarriesAType(tc.qov); got != tc.typed {
				t.Fatalf("carriesAType = %t, want %t. This is the cut the whole divergence "+
					"is denominated in; a spelling that cannot make it reports the gap as "+
					"closed while every producer still stands", got, tc.typed)
			}
			if got := describeMintedQOVType(tc.qov); got != tc.spell {
				t.Fatalf("type spelling = %q, want %q. A boolean is equally compatible with "+
					"a real row and with a scalar; the arity is the fact", got, tc.spell)
			}
		})
	}
}

// The mint census's assertion arms, driven from counter states.
//
// The polarity of the untyped floor is the arm that matters and it reads
// backwards: an untyped QOV is a Java divergence, so the count going DOWN is the
// good direction and must still fail — a closing gap and a darkening site are
// indistinguishable from a smaller number.
func TestSelectResultMintCensus_AssertionArmsGoRed(t *testing.T) {
	t.Parallel()

	base := func() SelectResultMintCounters {
		var c SelectResultMintCounters
		c.Calls = [SelectResultMintSiteCount]int{100}
		c.UntypedQOV = [SelectResultMintSiteCount]int{100}
		return c
	}
	floors := &SelectResultMintFloors{
		Calls:           [SelectResultMintSiteCount]int{10},
		UntypedQOVFloor: [SelectResultMintSiteCount]int{10},
	}

	t.Run("a calibrated state PASSES", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		if assertSelectResultMintCounters(&b, base(), floors) {
			t.Fatalf("the calibrated state must pass its own gate, or every red below is "+
				"measuring the gate rather than the population:\n%s", b.String())
		}
	})

	t.Run("the untyped population DISAPPEARING is RED", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.UntypedQOV[SelectResultMintExistsSelect] = 0
		c.TypedQOV[SelectResultMintExistsSelect] = 100
		var b strings.Builder
		if !assertSelectResultMintCounters(&b, c, floors) {
			t.Fatal("the untyped mint population going to zero must fail. It is a floor on a " +
				"DIVERGENCE: Java cannot build an untyped QOV at all, so a silent drop is " +
				"either the gap closing or the site going dark, and the two look identical " +
				"from a smaller number.")
		}
	})

	t.Run("the site going DARK is RED", func(t *testing.T) {
		t.Parallel()
		var c SelectResultMintCounters
		var b strings.Builder
		if !assertSelectResultMintCounters(&b, c, floors) {
			t.Fatal("a mint site making zero constructions must fail — otherwise every zero " +
				"recorded beside it reads as an absent SHAPE when it is an absent population.")
		}
	})

	t.Run("a partition gap is RED", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.Calls[SelectResultMintExistsSelect] = 200
		var b strings.Builder
		if !assertSelectResultMintCounters(&b, c, floors) {
			t.Fatal("typed + untyped + other must sum to calls. A gap is an arm that returns " +
				"before recording, and it makes every number beside it a subset printing as " +
				"a total.")
		}
	})
}

// The recorder's gate must come FIRST, and the census must stay empty with it off.
//
// This is the cost property, not a correctness one: the recorder builds a
// Sprintf'd shape spelling per select expression, and with the census disabled
// nothing consumes it. A recorder that classifies first and files second is
// still CORRECT and still pays.
func TestSelectResultMintCensus_GateOffRecordsNothing(t *testing.T) {
	// NOT parallel: it drives the process-global census flag, which every other
	// census on this path reads. The qualifier-recovery gate test does the same
	// for the same reason.
	restore := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	defer SetLegIdentityCensusEnabled(restore)

	before := SelectResultMintCensus()
	rv := NewQuantifiedObjectValue(NamedCorrelationIdentifier("GATEOFF"))
	RecordSelectResultMint(SelectResultMintExistsSelect, rv)
	after := SelectResultMintCensus()

	if after.Calls != before.Calls {
		t.Fatalf("calls moved %v -> %v with the census OFF. The gate is the first statement "+
			"of the recorder precisely so that a disabled census costs one atomic load; a "+
			"recorder that files anyway is measuring runs nobody asked for",
			before.Calls, after.Calls)
	}
	if _, minted := SelectResultMintOriginOf(rv); minted {
		t.Fatal("a value recorded with the census OFF registered an origin. The origin map " +
			"is what makes producer attribution a measurement rather than an inference, and " +
			"it must not grow — unboundedly, keyed by every select result value ever built — " +
			"on a path nobody is measuring.")
	}
}

// The counters must be a SNAPSHOT, maps included.
//
// The arrays copy by assignment and the maps inside them do not, so returning
// the struct by value alone hands the caller a live view of state the planner is
// still writing. That defeats the reason the assertion takes an explicit state,
// and under a concurrent planner it is not a stale number — it is a fatal
// concurrent map iteration and map write.
func TestSelectResultMintCensus_SnapshotDeepCopiesShapes(t *testing.T) {
	// NOT parallel: it records into the process-global census.
	restore := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(true)
	defer SetLegIdentityCensusEnabled(restore)

	RecordSelectResultMint(SelectResultMintExistsSelect,
		NewQuantifiedObjectValue(NamedCorrelationIdentifier("SNAP1")))
	snap := SelectResultMintCensus()
	shapes := snap.Shapes[SelectResultMintExistsSelect]
	if len(shapes) == 0 {
		t.Fatal("the mint recorded no shape, so this test is asserting about an empty map")
	}
	var key string
	for k := range shapes {
		key = k
		break
	}
	was := shapes[key]

	RecordSelectResultMint(SelectResultMintExistsSelect,
		NewQuantifiedObjectValue(NamedCorrelationIdentifier("SNAP2")))

	if got := snap.Shapes[SelectResultMintExistsSelect][key]; got != was {
		t.Fatalf("shape %q in a SNAPSHOT moved %d -> %d when the census was written again. "+
			"The snapshot shares the live map, so a failure message renders state that has "+
			"moved since the assertion read it, and a concurrent planner turns the same "+
			"sharing into a fatal concurrent map iteration and map write", key, was, got)
	}
}
