package values

import (
	"strings"
	"testing"
)

// The UNIT PIN for the two census gates added beside the accessor-name-path and
// field-value-mint censuses.
//
// Their only consumer is the real-FDB sqldriver corpus, whose TestMain calls
// them once at the end of a whole run. That drives exactly the arms the corpus
// happens to reach and no others: a ceiling the corpus sits under is never seen
// to fire, a floor it clears is never seen to fire, and the vacuity guard that
// makes a narrowed run honest is exercised only by whoever happens to narrow.
// The first real firing of any of them then reads as a FINDING rather than as an
// untested branch — the most expensive way to discover a bug in an instrument.
//
// So every arm is driven here, over explicit state, with no dependence on what a
// corpus contains: the ceiling, the vacuity floor, and (for the mint census) the
// partition check. Both no-op shapes are driven too, because
// a gate that silently no-ops is a green from an empty set wearing a seventh face.

func intp(n int) *int { return &n }

// classes builds an accessor-path class vector with a given dotted-decline count
// and a given amount of ordinary traffic, so a test can move ONE arm at a time.
func classes(dotted, baked int) [accessorPathClassCount]int {
	var c [accessorPathClassCount]int
	c[AccessorPathDeclineDotted] = dotted
	c[AccessorPathOKAllBaked] = baked
	return c
}

func TestAccessorPathGates_NilIsANoOp(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	if assertAccessorPathCensusState(&w, classes(9999, 0), nil, nil) {
		t.Fatalf("nil gates FAILED. A narrowed run passes nil precisely because a whole-corpus\n"+
			"population claim has nothing it can honestly decide; output was %q", w.String())
	}
	if w.Len() != 0 {
		t.Fatalf("nil gates wrote %q, want silence", w.String())
	}
}

func TestAccessorPathGates_ZeroValueChecksNothing(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	// Every field unset: MinTotal 0, MaxDeclineDotted nil.
	// This is the shape the sqldriver wrapper degrades to when narrowed, and it
	// must not fire on a population that would trip every arm if enabled. The
	// population is therefore a TRIPPING one: over an empty census the test would
	// pass whether or not the zero-valued gates checked anything, which is the
	// vacuous green these gates exist to catch.
	if assertAccessorPathCensusState(&w, classes(9999, 0), nil, &AccessorPathGates{}) {
		t.Fatalf("an all-zero gates value FAILED on a population that would trip every "+
			"enabled arm: %q", w.String())
	}
}

func TestAccessorPathGates_VacuityFloorFires(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	// The dominant false positive: a ceiling of 0 over ZERO observations. The
	// ceiling below is satisfied; only the vacuity floor can tell that nothing
	// was measured.
	gates := &AccessorPathGates{MinTotal: 1000, MaxDeclineDotted: intp(1)}
	if !assertAccessorPathCensusState(&w, classes(0, 0), nil, gates) {
		t.Fatal("an EMPTY census passed. A ceiling over zero observations is a green from an " +
			"empty set: it reads as success while measuring nothing")
	}
	if !strings.Contains(w.String(), "ALARM DIRECTION: vacuity") {
		t.Fatalf("the vacuity failure does not name its direction: %q", w.String())
	}
}

func TestAccessorPathGates_VacuityFloorIsSatisfiedByTraffic(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	// Control: the floor must be satisfiable, or the test above passes for the
	// wrong reason and the gate is simply always-red. This is the MEASURED steady
	// state: heavy traffic, and the dotted arm at zero.
	gates := &AccessorPathGates{MinTotal: 1000, MaxDeclineDotted: intp(0)}
	if assertAccessorPathCensusState(&w, classes(0, 5000), nil, gates) {
		t.Fatalf("the measured steady state (0 dotted declines over 5000 calls) FAILED: %q", w.String())
	}
}

func TestAccessorPathGates_GrowthCeilingFires(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	gates := &AccessorPathGates{MinTotal: 1000, MaxDeclineDotted: intp(0)}
	// 4 declines is the PRE-FIX population, with the rendered-Explain witnesses
	// that named the producer.
	ws := map[string]int{"N.SK": 1, "q$510549.AID#0": 2, "q$510687.AID#0": 1}
	if !assertAccessorPathCensusState(&w, classes(4, 5000), ws, gates) {
		t.Fatal("4 dotted declines passed a ceiling of 0 — a regression straight back to the " +
			"pre-fix population would report nothing")
	}
	got := w.String()
	if !strings.Contains(got, "ALARM DIRECTION: growth") {
		t.Fatalf("the ceiling failure does not name its direction: %q", got)
	}
	if !strings.Contains(got, "q$510549.AID#0") {
		t.Fatalf("the ceiling failure does not print the witnesses, which are what name the "+
			"producer: %q", got)
	}
}

// TestAccessorPathGates_CeilingCatchesASingleReturningDecline is the arm that
// exists because the expected value MOVED.
//
// This gate was first written as a band [1,1] around a surviving `N.SK` decline.
// The whole corpus then measured ZERO, because a nested-path GROUP BY key is now
// rejected before the planner and the query that produced `N.SK` no longer plans.
// A collapse floor became unsatisfiable and the direction inverted: growth is the
// only alarm. So the case that must not slip through is ONE decline, not four —
// the old band would have accepted exactly that.
func TestAccessorPathGates_CeilingCatchesASingleReturningDecline(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	gates := &AccessorPathGates{MinTotal: 1000, MaxDeclineDotted: intp(0)}
	if !assertAccessorPathCensusState(&w, classes(1, 5000), map[string]int{"N.SK": 1}, gates) {
		t.Fatal("a SINGLE returning dotted decline passed. Zero is the steady state now, so " +
			"one is the regression — and it is exactly the value the gate's original [1,1] " +
			"band would have called normal")
	}
	got := w.String()
	if !strings.Contains(got, "ALARM DIRECTION: growth") {
		t.Fatalf("the ceiling failure does not name its direction: %q", got)
	}
	if !strings.Contains(got, "N.SK") {
		t.Fatalf("the ceiling failure does not print the witness, which is what tells a reader "+
			"WHICH retired producer came back: %q", got)
	}
}

func TestAssertAccessorPathCensus_ReadsTheProcessCounters(t *testing.T) {
	// Not parallel: it owns the process counters.
	was := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(true)
	ResetAccessorPathCensus()
	t.Cleanup(func() {
		ResetAccessorPathCensus()
		SetLegIdentityCensusEnabled(was)
	})

	// A pin over explicit state alone stays green if the exported entry point
	// stopped reading the real counters, or read a different class. This runs it.
	RecordAccessorPathCall(AccessorPathDeclineDotted)
	RecordAccessorPathDottedWitness("N.SK")
	RecordAccessorPathCall(AccessorPathOKAllBaked)

	var w strings.Builder
	if !AssertAccessorPathCensus(&w, &AccessorPathGates{MaxDeclineDotted: intp(0)}) {
		t.Fatal("AssertAccessorPathCensus did not see the dotted decline recorded through " +
			"RecordAccessorPathCall — the exported gate is not wired to the live counters")
	}
	if !strings.Contains(w.String(), "N.SK") {
		t.Fatalf("the live gate did not print the recorded witness: %q", w.String())
	}
	var w2 strings.Builder
	if AssertAccessorPathCensus(&w2, &AccessorPathGates{MinTotal: 2, MaxDeclineDotted: intp(1)}) {
		t.Fatalf("the live gate failed on the state it should accept (2 calls, 1 dotted): %q", w2.String())
	}
}

// ---- the MINT census's gate ----------------------------------------------

// mintCounts builds a mint class vector one arm at a time.
func mintCounts(rendered, bare int) [fieldMintClassCount]int {
	var c [fieldMintClassCount]int
	c[FieldMintLazyExplainRendered] = rendered
	c[FieldMintLazyBare] = bare
	return c
}

func TestFieldValueMintGates_NilIsANoOp(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	if assertFieldValueMintCensusState(&w, 0, mintCounts(99999, 0), nil, nil) {
		t.Fatalf("nil gates FAILED: %q", w.String())
	}
	if w.Len() != 0 {
		t.Fatalf("nil gates wrote %q, want silence", w.String())
	}
}

// TestFieldValueMintGates_ZeroValueChecksNothing is the mint counterpart of the
// accessor-path twin above, and it is the arm that had no pin at all.
//
// MaxExplainRendered is a *int, so a caller that sets only MinTotal — or nothing
// — leaves it nil, and the ceiling arm must decline rather than dereference it.
// Without this, deleting the nil guard keeps every other pin green: they all set
// the ceiling, so none of them ever reaches a nil to dereference. The population
// is a TRIPPING one, and its total matches the class sum so the unconditional
// partition check cannot redden this for an unrelated reason.
func TestFieldValueMintGates_ZeroValueChecksNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		gates *FieldValueMintGates
	}{
		{"every field unset", &FieldValueMintGates{}},
		{"only the vacuity floor set", &FieldValueMintGates{MinTotal: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var w strings.Builder
			// A nil MaxExplainRendered is not "a ceiling of zero" — it is NO ceiling,
			// so 99999 rendered mints must pass.
			if assertFieldValueMintCensusState(&w, 99999, mintCounts(99999, 0), nil, tc.gates) {
				t.Fatalf("a gates value with a nil MaxExplainRendered FAILED on a population "+
					"that would trip the ceiling if one were set: %q", w.String())
			}
		})
	}
}

func TestFieldValueMintGates_VacuityFloorFires(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	gates := &FieldValueMintGates{MinTotal: 1000, MaxExplainRendered: intp(0)}
	if !assertFieldValueMintCensusState(&w, 0, mintCounts(0, 0), nil, gates) {
		t.Fatal("an EMPTY mint census passed a ceiling of 0. `lazy-EXPLAIN-RENDERED = 0` over " +
			"zero observed mints is the absence of a population, not the absence of a shape")
	}
	if !strings.Contains(w.String(), "ALARM DIRECTION: vacuity") {
		t.Fatalf("the vacuity failure does not name its direction: %q", w.String())
	}
}

func TestFieldValueMintGates_CeilingFires(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	gates := &FieldValueMintGates{MinTotal: 1000, MaxExplainRendered: intp(0)}
	// 21865 is the measured pre-fix population, all of it from one line.
	if !assertFieldValueMintCensusState(&w, 30000, mintCounts(21865, 8135),
		[]string{"lazy-EXPLAIN-RENDERED :: q$50765.AID#0\n      plans/ordering.go:1005"}, gates) {
		t.Fatal("21865 lazy-EXPLAIN-RENDERED mints passed a ceiling of 0 — the regression the " +
			"gate exists for would report nothing")
	}
	got := w.String()
	if !strings.Contains(got, "ALARM DIRECTION: growth") {
		t.Fatalf("the ceiling failure does not name its direction: %q", got)
	}
	if !strings.Contains(got, "plans/ordering.go") {
		t.Fatalf("the ceiling failure does not print the captured origins, which are what name "+
			"the producer: %q", got)
	}
}

func TestFieldValueMintGates_SteadyStatePasses(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	gates := &FieldValueMintGates{MinTotal: 1000, MaxExplainRendered: intp(0)}
	if assertFieldValueMintCensusState(&w, 30000, mintCounts(0, 30000), nil, gates) {
		t.Fatalf("the post-fix steady state (0 rendered mints over 30000) FAILED: %q", w.String())
	}
}

// TestFieldValueMintGates_RetirementCeilingInvertsTheFloor drives the arm a
// corpus reaches once the lazy mint is gone from the tree rather than merely
// quiet, and it exists because BOTH obvious alternatives fail silently.
//
// Lowering MinTotal to 0 disarms the guard: a floor of zero is satisfied by
// every state, including a revival. Deleting MinTotal leaves the retirement
// unwatched. MaxTotal says the same population is now expected EMPTY, with the
// alarm pointing the other way — and being a ceiling it survives a -test.run
// filter, which a claim about the tree must.
//
// The two are opposite claims about one population, so a corpus sets one or the
// other; the last arm pins that both together is an incoherent configuration
// that reports BOTH failures rather than silently preferring one.
func TestFieldValueMintGates_RetirementCeilingInvertsTheFloor(t *testing.T) {
	t.Parallel()

	t.Run("an empty census passes the retirement ceiling", func(t *testing.T) {
		t.Parallel()
		var w strings.Builder
		gates := &FieldValueMintGates{MaxTotal: intp(0), MaxExplainRendered: intp(0)}
		if assertFieldValueMintCensusState(&w, 0, mintCounts(0, 0), nil, gates) {
			t.Fatalf("the retired steady state — zero mints — FAILED: %q", w.String())
		}
	})

	t.Run("a revival fires it", func(t *testing.T) {
		t.Parallel()
		var w strings.Builder
		gates := &FieldValueMintGates{MaxTotal: intp(0), MaxExplainRendered: intp(0)}
		if !assertFieldValueMintCensusState(&w, 1, mintCounts(0, 1), nil, gates) {
			t.Fatal("ONE lazy mint passed a retirement ceiling of 0. The hooks are reachable " +
				"only from a package-private fixture arm, so a count here is a producer coming " +
				"back, which is exactly what this ceiling is for")
		}
		got := w.String()
		if !strings.Contains(got, "RETIRED") || !strings.Contains(got, "ALARM DIRECTION: growth") {
			t.Fatalf("the retirement failure does not say it is a retirement, or does not name "+
				"its direction — the text is the whole value of an inverted guard: %q", got)
		}
	})

	t.Run("a floor of zero would NOT have caught it", func(t *testing.T) {
		t.Parallel()
		// The negative control for the whole mechanism. This is what "just lower
		// MinTotal" produces, and it passes the revival.
		var w strings.Builder
		gates := &FieldValueMintGates{MinTotal: 0, MaxExplainRendered: intp(0)}
		if assertFieldValueMintCensusState(&w, 1, mintCounts(0, 1), nil, gates) {
			t.Fatalf("a floor of zero caught the revival, which would mean MaxTotal is "+
				"unnecessary — check the assertion, not the expectation: %q", w.String())
		}
	})

	t.Run("floor and ceiling together are incoherent and both report", func(t *testing.T) {
		t.Parallel()
		var w strings.Builder
		gates := &FieldValueMintGates{MinTotal: 5000, MaxTotal: intp(0), MaxExplainRendered: intp(0)}
		if !assertFieldValueMintCensusState(&w, 1, mintCounts(0, 1), nil, gates) {
			t.Fatal("a configuration asserting both a floor of 5000 and a ceiling of 0 passed")
		}
		got := w.String()
		if !strings.Contains(got, "ALARM DIRECTION: vacuity") ||
			!strings.Contains(got, "ALARM DIRECTION: growth") {
			t.Fatalf("only one of the two contradictory gates reported. A caller that set both "+
				"has to see both, or it will fix one and re-run into the other: %q", got)
		}
	})
}

func TestFieldValueMintGates_PartitionBreakFires(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	// Classes sum to 30000 against an independent total of 30001: one mint took a
	// path no arm classified, and it could be of exactly the class the ceiling
	// watches. The ceiling and the floor are both satisfied here, so nothing but
	// the partition check can see it.
	gates := &FieldValueMintGates{MinTotal: 1000, MaxExplainRendered: intp(0)}
	if !assertFieldValueMintCensusState(&w, 30001, mintCounts(0, 30000), nil, gates) {
		t.Fatal("a BROKEN partition passed. An unclassified mint makes every class count above " +
			"it unreadable, including the one the ceiling is asserted on")
	}
	if !strings.Contains(w.String(), "bug in\n  THIS CENSUS") {
		t.Fatalf("the partition failure does not say the fault is in the instrument: %q", w.String())
	}
}

func TestAssertFieldValueMintCensus_ReadsTheProcessCounters(t *testing.T) {
	// Not parallel: it owns the process counters and the census gate flag.
	was := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(true)
	ResetFieldValueMintCensus()
	t.Cleanup(func() {
		ResetFieldValueMintCensus()
		SetLegIdentityCensusEnabled(was)
	})

	NoteFieldValueMint("q$50765.AID#0", false) // the rendered-Explain shape
	NoteFieldValueMint("SCORE", false)

	var w strings.Builder
	if !AssertFieldValueMintCensus(&w, &FieldValueMintGates{MaxExplainRendered: intp(0)}) {
		t.Fatal("AssertFieldValueMintCensus did not see a mint recorded through " +
			"NoteFieldValueMint — the exported gate is not wired to the live counters")
	}
	// The ORIGINS are a second wire, and passing nil for them at the call site
	// breaks nothing a count-only assertion can see. A ceiling that reddens
	// without naming a witness leaves the next reader with a number and no
	// producer to look at, which is most of the value of instrumenting the mint.
	if !strings.Contains(w.String(), "q$50765.AID#0") {
		t.Fatalf("the live gate did not print the captured origin for the mint it just "+
			"failed on: %q", w.String())
	}
	var w2 strings.Builder
	if AssertFieldValueMintCensus(&w2, &FieldValueMintGates{MinTotal: 2, MaxExplainRendered: intp(1)}) {
		t.Fatalf("the live gate failed on the state it should accept (2 mints, 1 rendered): %q", w2.String())
	}
}
