package querycensus

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The mint census's assertion arms, driven from counter states.
//
// The untyped arm's polarity INVERTED once. While Go could mint an untyped QOV —
// a value Java cannot build at all — the count going DOWN was the failing
// direction, because a closing gap and a darkening site are indistinguishable
// from a smaller number. NewQuantifiedObjectValue now requires an exact type, so
// zero is the steady state and GROWTH is what the arm watches.
func TestSelectResultMintCensus_AssertionArmsGoRed(t *testing.T) {
	t.Parallel()

	base := func() SelectResultMintCounters {
		var c SelectResultMintCounters
		c.Calls = [values.SelectResultMintSiteCount]int{100}
		c.TypedQOV = [values.SelectResultMintSiteCount]int{100}
		return c
	}
	floors := &SelectResultMintFloors{
		Calls: [values.SelectResultMintSiteCount]int{10},
	}

	t.Run("a calibrated state PASSES", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		if assertSelectResultMintCounters(&b, base(), floors) {
			t.Fatalf("the calibrated state must pass its own gate, or every red below is "+
				"measuring the gate rather than the population:\n%s", b.String())
		}
	})

	t.Run("an untyped mint REVIVING is RED", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.UntypedQOV[values.SelectResultMintExistsSelect] = 1
		c.TypedQOV[values.SelectResultMintExistsSelect] = 99
		var b strings.Builder
		if !assertSelectResultMintCounters(&b, c, floors) {
			t.Fatal("a single untyped mint must fail. NewQuantifiedObjectValue requires an " +
				"exact type, so one here means a mint path has appeared that reaches " +
				"around that requirement — and the ordinal model rests on every QOV " +
				"carrying the row it addresses.")
		}
		if !strings.Contains(b.String(), "want 0") {
			t.Fatalf("failure message does not state the expectation: %s", b.String())
		}
	})

	t.Run("the untyped zero holds on a NARROWED run", func(t *testing.T) {
		t.Parallel()
		// nil floors is the narrowed-run configuration (-test.run drops the
		// calibrations). The revival alarm must survive it: a gate that only
		// watches a retired population on full runs stops watching it exactly when
		// someone is iterating on the code that would revive it.
		c := base()
		c.UntypedQOV[values.SelectResultMintExistsSelect] = 1
		c.TypedQOV[values.SelectResultMintExistsSelect] = 99
		var b strings.Builder
		if !assertSelectResultMintCounters(&b, c, nil) {
			t.Fatalf("the untyped-QOV zero must hold with no floors:\n%s", b.String())
		}
	})

	t.Run("the site going DARK is RED", func(t *testing.T) {
		t.Parallel()
		// The CALL floor is now the only floor on this census, so dropping the call
		// count below it is the whole perturbation. It used to need care: the
		// untyped floor subsumed the call floor at every admissible state, so a
		// naive zeroing of both never reached this arm at all.
		var c SelectResultMintCounters
		var b strings.Builder
		if !assertSelectResultMintCounters(&b, c, floors) {
			t.Fatal("a mint site making zero constructions must fail — otherwise every " +
				"zero recorded beside it reads as an absent SHAPE when it is an absent " +
				"population.")
		}
		if !strings.Contains(b.String(), "gone dark") {
			t.Fatalf("the CALL floor must be the arm that fired:\n%s", b.String())
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
	rv := mustQOV(t, values.NamedCorrelationIdentifier("GATEOFF"))
	RecordSelectResultMint(values.SelectResultMintExistsSelect, rv)
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

	RecordSelectResultMint(values.SelectResultMintExistsSelect,
		mustQOV(t, values.NamedCorrelationIdentifier("SNAP1")))
	snap := SelectResultMintCensus()
	shapes := snap.Shapes[values.SelectResultMintExistsSelect]
	if len(shapes) == 0 {
		t.Fatal("the mint recorded no shape, so this test is asserting about an empty map")
	}
	var key string
	for k := range shapes {
		key = k
		break
	}
	was := shapes[key]

	RecordSelectResultMint(values.SelectResultMintExistsSelect,
		mustQOV(t, values.NamedCorrelationIdentifier("SNAP2")))

	if got := snap.Shapes[values.SelectResultMintExistsSelect][key]; got != was {
		t.Fatalf("shape %q in a SNAPSHOT moved %d -> %d when the census was written again. "+
			"The snapshot shares the live map, so a failure message renders state that has "+
			"moved since the assertion read it, and a concurrent planner turns the same "+
			"sharing into a fatal concurrent map iteration and map write", key, was, got)
	}
}

// mustQOV builds the same empty-row QOV as the values package's helper.
func mustQOV(t testing.TB, correlation values.CorrelationIdentifier) values.Value {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(correlation, &values.RecordType{Fields: []values.Field{}})
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%q): %v", correlation, err)
	}
	return qov
}
