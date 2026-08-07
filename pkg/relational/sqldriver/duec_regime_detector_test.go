package sqldriver_test

// The DISTINCT cost probe's two regime detectors, driven directly.
//
// A detector that never trips is worth nothing, and it is worth LESS than
// nothing when it guards a criterion, because the criterion then reads as
// enforced while abstaining on every run — or, mutated the other way, reads as
// enforced while being unreachable. Neither failure is visible from the probe:
// it passes identically whether the detector is perfect, permanently open, or
// permanently shut. So the detector is exercised here, on FABRICATED samples,
// with no FDB and no store — pure inputs, deterministic verdicts.
//
// The overload shapes are not invented. They are the numbers the probe itself
// printed on a box under load (24 cores, load average 148), on the run that
// motivated the detector: the null pair — two identical plans — reported 1.22x
// while B/D′ inverted to 1.035x against a 0.95 bound. The quiet shapes are
// RFC-210 §2.1's recorded in-transaction figures. Pinning the exact shape that
// broke is the point; a rounder synthetic would pass a detector too coarse to
// have caught the real one.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
)

// duecSamplesMs builds a series from per-rep millisecond durations.
func duecSamplesMs(ms ...int) []duecSample {
	out := make([]duecSample, 0, len(ms))
	for _, m := range ms {
		out = append(out, duecSample{rows: 100000, dur: time.Duration(m) * time.Millisecond})
	}
	return out
}

// The LOADED null pair, verbatim from the probe's own INTX log on the run that
// motivated this mechanism. Median per-rep ratio 1.221x on a pair whose true
// ratio is 1.0.
func duecLoadedNullPair() (c, aPrime []duecSample) {
	return duecSamplesMs(454, 479, 417, 592, 585, 410, 361, 838, 521),
		duecSamplesMs(415, 376, 664, 455, 479, 319, 407, 436, 428)
}

// The QUIET null pair, built from RFC-210 §2.1's recorded in-transaction
// figures (C 178 ms, A′ 174 ms) with the per-rep jitter a healthy box shows.
// Median per-rep ratio ~1.02x, inside the recorded 0.88-1.02 envelope.
//
// CAVEAT, AND IT MATTERS FOR ANYONE READING DISPERSION OFF THIS FIXTURE: only
// the two MEDIANS above are recorded data. The per-rep jitter is invented, and
// it is far calmer than a real quiet box — this fixture's per-rep max/min is
// 1.035, where seventeen measured runs on a quiet box span 1.113 to 2.084 (see
// the dispersion note in distinct_unique_elision_cost_probe_test.go). It is
// therefore a fine fixture for exercising the MEDIAN arm, which is all it was
// built for, and a badly misleading one for calibrating anything about spread.
// Deriving a dispersion bound from these numbers would have put it around 1.1
// and withheld on nearly every genuinely quiet run.
func duecQuietNullPair() (c, aPrime []duecSample) {
	return duecSamplesMs(178, 181, 175, 183, 177, 180, 174, 179, 176),
		duecSamplesMs(174, 177, 176, 178, 172, 175, 173, 176, 171)
}

// duecMeasuredQuietDispersion is the measured per-rep max/min of the null pair
// over seventeen consecutive runs on a quiet box, every one of which the median
// arm ACCEPTED. It is recorded as data rather than prose because it is the
// evidence that refuses a dispersion arm, and the next person to propose one
// should have to look at it.
//
// Anchors: this quiet band tops out at 2.084; the CI run on #638 that motivated
// the proposal — median 0.995x, accepted — measured 2.400. A 1.15x separation
// is not a band to place a bound in.
var duecMeasuredQuietDispersion = []float64{
	1.113, 1.133, 1.159, 1.245, 1.255, 1.256, 1.373, 1.391, 1.396,
	1.406, 1.439, 1.445, 1.520, 1.633, 1.673, 1.682, 2.084,
}

// duecCIBlindSpotRatios is the #638 CI run's per-rep null-pair array verbatim:
// the anticipated blind spot, median 0.995x (accepted), max/min 2.400.
var duecCIBlindSpotRatios = []float64{
	0.728, 1.271, 0.791, 1.747, 0.979, 1.153, 0.87, 1.082, 0.995,
}

// duecSecondBlindSpotRatios is a SECOND blind-spot observation, recorded on a
// heavily loaded box (load average 42) while four agents shared the machine.
// Quiet-gate verdict: null pair 0.956x, inside the 0.12 envelope, ACCEPTED —
// the same miss as #638, on a different run and a different tree.
//
// max/min 2.642, against #638's 2.400 and a quiet ceiling of 2.084.
//
// WHICH OBJECTION THIS ANSWERS, AND WHICH IT DOES NOT. The refusal below rested
// on two legs. The first was that a bound in (2.084, 2.400) would rest on a
// SINGLE failure observation — that leg is now half gone: there are two, and
// both sit above the quiet ceiling, so the failure population is no longer a
// single point. The second leg STANDS UNCHANGED: the quiet tail is not
// exhausted at seventeen runs. Run 16 was the first above 1.7 and the band
// reached 2.084 only at run 17, so the ceiling is still moving, and a bound
// placed under a moving ceiling fires on quiet runs later.
//
// The verdict is therefore unchanged, and it is worth being explicit that this
// datum was recorded because it COULD have changed it. Separation is now
// 2.642/2.084 = 1.268x, up from 1.151x and still under the 1.5x bar below.
var duecSecondBlindSpotRatios = []float64{
	0.581, 0.975, 1.145, 0.933, 0.956, 1.489, 0.9, 0.941, 1.535,
}

// TestDuecDispersionArmIsNotDerivable pins the NEGATIVE RESULT that keeps the
// dispersion arm out, so that "we looked and there was no bound" survives as a
// checkable fact rather than as a paragraph someone deletes.
//
// The proposal was principled: the median arm has a known blind spot (a run
// whose reps swing while the median lands near 1.0), #638's CI run hit it
// exactly, and the probe had been logging per-rep arrays for precisely this
// derivation. What the accumulated record then showed is that the two
// populations do not separate on any natural statistic.
//
// If a future change makes them separate — a lower-variance fixture, a quieter
// lane, more reps per run — this test fails, and that failure is the signal
// that the arm has become derivable. Until then it is the guard against sliding
// a threshold into a 15% gap and calling it measurement.
func TestDuecDispersionArmIsNotDerivable(t *testing.T) {
	t.Parallel()

	maxMin := func(rs []float64) float64 {
		lo, hi := rs[0], rs[0]
		for _, r := range rs {
			if r < lo {
				lo = r
			}
			if r > hi {
				hi = r
			}
		}
		return hi / lo
	}

	quietMax := duecMeasuredQuietDispersion[len(duecMeasuredQuietDispersion)-1]

	// BOTH recorded blind-spot runs are evaluated, and the test is stated against
	// the one CLOSEST to the quiet band. Taking the larger would flatter the
	// separation: a bound has to clear the nearest failure, not the friendliest.
	for _, obs := range []struct {
		name   string
		ratios []float64
	}{
		{"#638 CI run", duecCIBlindSpotRatios},
		{"loaded-box run (load avg 42)", duecSecondBlindSpotRatios},
	} {
		if d := maxMin(obs.ratios); d <= quietMax {
			t.Fatalf("blind-spot observation %q has dispersion %.3f, at or below the measured "+
				"quiet maximum (%.3f) — the populations have inverted, and a dispersion arm "+
				"would withhold on quiet runs while admitting the loaded one",
				obs.name, d, quietMax)
		}
	}

	failure := maxMin(duecCIBlindSpotRatios)
	if d := maxMin(duecSecondBlindSpotRatios); d < failure {
		failure = d
	}

	if failure <= quietMax {
		t.Fatalf("the #638 blind-spot run's dispersion (%.3f) is at or below the measured quiet "+
			"maximum (%.3f) — the two populations have inverted, and a dispersion arm would "+
			"withhold on quiet runs while admitting the loaded one", failure, quietMax)
	}

	// The separation is the whole question. A bound needs room between the
	// populations; 1.15x is not room, it is one unlucky quiet run.
	const duecDispersionSeparationNeeded = 1.5
	if sep := failure / quietMax; sep >= duecDispersionSeparationNeeded {
		t.Fatalf("quiet dispersion now tops out at %.3f against the blind-spot run's %.3f — a "+
			"separation of %.3fx, at or beyond the %.1fx this test treats as the point where a "+
			"bound stops being invented. The dispersion arm has become DERIVABLE: place it "+
			"between the two populations, wire it as a withholding arm beside the null-pair and "+
			"1007 detectors, and delete this test",
			quietMax, failure, sep, duecDispersionSeparationNeeded)
	}
}

func TestDuecRegimeVerdict_WithholdsUnderSyntheticOverload(t *testing.T) {
	t.Parallel()
	c, aPrime := duecLoadedNullPair()
	ok, why := duecRegimeVerdict(false, false, c, aPrime)
	if ok {
		med := duecMedianRatio(
			&duecSeries{samples: c}, &duecSeries{samples: aPrime}, duecDurOf)
		t.Fatalf("the regime detector ACCEPTED a run whose NULL PAIR measured %.3fx.\n"+
			"These are the probe's own C and A' durations from a box at load average "+
			"148, where B/D' inverted to 1.035x against its 0.95 bound. C and A' are "+
			"the same access path with no operator on either side, so 1.0 is their "+
			"only possible true value; a detector that calls %.3fx resolvable will "+
			"let the wall-clock criteria assert on exactly the runs that produced the "+
			"flake this mechanism exists to stop.", med, med)
	}
	// The reason must name the null pair and quote what it measured, because the
	// log line is the only artefact a CI reader gets from a withheld run.
	for _, want := range []string{"NULL PAIR", "1.221"} {
		if !strings.Contains(why, want) {
			t.Fatalf("the withholding reason does not mention %q: %s", want, why)
		}
	}
}

func TestDuecRegimeVerdict_WithholdsOnFabricatedTransactionTooOld(t *testing.T) {
	t.Parallel()
	// Detector A is independent of Detector B by construction, so it is driven
	// with a null pair Detector B is perfectly happy with. If the two were
	// entangled, this case would pass for the wrong reason.
	c, aPrime := duecQuietNullPair()
	if ok, _ := duecRegimeVerdict(false, false, c, aPrime); !ok {
		t.Fatalf("the quiet null pair must be acceptable on its own, or this case " +
			"cannot isolate Detector A")
	}
	ok, why := duecRegimeVerdict(false, true, c, aPrime)
	if ok {
		t.Fatal("the regime detector ACCEPTED a run in which a timed query died on " +
			"transaction_too_old. A 1007 proves the 5-second measurement window was " +
			"not held, so every duration taken in that window is a truncated sample " +
			"rather than a slow one, and no statistic over the survivors repairs it.")
	}
	if !strings.Contains(why, "transaction_too_old") {
		t.Fatalf("the withholding reason does not name the error that proved the "+
			"regime invalid: %s", why)
	}
}

func TestDuecRegimeVerdict_WithholdsUnderRaceInstrumentation(t *testing.T) {
	t.Parallel()
	// The precedent this mechanism was built on, kept reachable: -race must
	// still withhold, and must do so even when the box is otherwise pristine.
	c, aPrime := duecQuietNullPair()
	ok, why := duecRegimeVerdict(true, false, c, aPrime)
	if ok {
		t.Fatal("the regime detector ACCEPTED a race-instrumented run. Instrumentation " +
			"taxes both sides of every pair and compresses R3's margin toward 1.0 " +
			"(0.968x measured under -race); this is the original ruling and it must " +
			"survive the generalisation to load.")
	}
	if !strings.Contains(why, "race detector") {
		t.Fatalf("the withholding reason does not name the instrumentation: %s", why)
	}
}

func TestDuecRegimeVerdict_AcceptsAQuietRun(t *testing.T) {
	t.Parallel()
	// The other direction, and it is not decoration: a detector that always
	// trips makes the three wall-clock criteria UNREACHABLE, which is
	// indistinguishable from deleting them and looks identical in a green run.
	c, aPrime := duecQuietNullPair()
	ok, why := duecRegimeVerdict(false, false, c, aPrime)
	if !ok {
		med := duecMedianRatio(
			&duecSeries{samples: c}, &duecSeries{samples: aPrime}, duecDurOf)
		t.Fatalf("the regime detector WITHHELD on a healthy run (null pair %.3fx, "+
			"inside the %.2f quiet envelope RFC-210 §2.1 records): %s\n"+
			"A detector that never passes silently removes R3's three timing "+
			"criteria; they would then be green on a tree where R3 does not fire at "+
			"all.", med, duecNullPairQuietDev, why)
	}
	if why != "" {
		t.Fatalf("an accepted run must carry no withholding reason, got %q", why)
	}
}

// TestDuecRegimeVerdict_BoundEdges pins WHERE the bound sits, in both
// directions, so a change to duecNullPairQuietDev is a change someone made on
// purpose rather than a constant that drifted.
func TestDuecRegimeVerdict_BoundEdges(t *testing.T) {
	t.Parallel()
	// A null pair at exactly the recorded quiet extremes (0.88 and 1.02) must be
	// ACCEPTED — those are runs whose measurements RFC-210 §2.1 believed.
	// Anything wider than the envelope must be withheld.
	for _, tc := range []struct {
		name     string
		ratio    float64
		wantOK   bool
		whatItIs string
	}{
		{"quiet floor", 0.88, true, "the low extreme of the four recorded quiet runs"},
		{"quiet ceiling", 1.02, true, "the high extreme of the four recorded quiet runs"},
		{"just outside low", 0.87, false, "wider than any quiet run ever measured"},
		{"just outside high", 1.13, false, "wider than any quiet run ever measured"},
		{"observed flake", 1.221, false, "the loaded reproduction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			den := duecSamplesMs(200, 200, 200, 200, 200, 200, 200, 200, 200)
			num := make([]duecSample, 0, len(den))
			for range den {
				num = append(num, duecSample{
					dur: time.Duration(math.Round(200 * tc.ratio * float64(time.Millisecond))),
				})
			}
			ok, why := duecRegimeVerdict(false, false, num, den)
			if ok != tc.wantOK {
				t.Fatalf("null pair at %.3fx (%s): accepted=%v, want %v (%s)",
					tc.ratio, tc.whatItIs, ok, tc.wantOK, why)
			}
		})
	}
}

// TestDuecRegimeVerdict_WithholdsWithoutSamples pins the degenerate case: a run
// that produced no usable null-pair ratios has offered NO evidence that its
// clock resolves anything, so it must withhold rather than default to trusting
// itself.
func TestDuecRegimeVerdict_WithholdsWithoutSamples(t *testing.T) {
	t.Parallel()
	if ok, _ := duecRegimeVerdict(false, false, nil, nil); ok {
		t.Fatal("the regime detector ACCEPTED a run with no null-pair samples at all. " +
			"Absence of evidence is not evidence the box was quiet; the safe default " +
			"is to withhold.")
	}
	// A zero-duration denominator is dropped rather than dividing to +Inf, so a
	// pair of zeroes is the same no-evidence case and not an accidental pass.
	zeroes := []duecSample{{dur: 0}, {dur: 0}, {dur: 0}}
	if ok, _ := duecRegimeVerdict(false, false, zeroes, zeroes); ok {
		t.Fatal("the regime detector ACCEPTED a run whose null-pair denominators were " +
			"all zero")
	}
}

// TestDuecMeasurementWindowLost pins Detector A's recogniser against BOTH
// spellings of a lost window, against the wrapping the relational layer actually
// applies, and against the near misses that must not trip it.
//
// THE PRE-EMPTION CASE IS THE ONE THAT COST A CI RED. This recogniser originally
// knew only FDB's 1007, and inside an explicit transaction on this driver a 1007
// essentially never arrives: paginatingRows.preflightTxBudget stops the query at
// four seconds so FDB's five-second wall is never reached. The carrier that does
// arrive is api.TransactionTimeLimitError under a 40001, which the 1007-only
// recogniser read as healthy — so duecRunInTx fatalled and took every
// load-INDEPENDENT arm of the probe down with the timing. A detector that cannot
// see the condition it guards is worse than no detector: it reads as armed.
//
// The wrapping half is load-bearing for both spellings. The probe never sees a
// bare fdb.Error: connection.go's translateFDBCode turns 1007 into
// api.Error{Code: 40001} and keeps the original as the cause, and database/sql
// hands that through.
//
// THE 40001 NEAR MISS IS THE OTHER HALF OF THE CLAIM, and it is why this matches
// on the typed cause rather than on the SQLSTATE. A genuine read/write conflict
// surfaces as 40001 too. A recogniser widened to "the code was 40001" would
// withhold the timing criteria on every conflict — turning a real concurrency bug
// in the code under test into "the box was busy", which is precisely the
// rationalisation this repo forbids.
func TestDuecMeasurementWindowLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// --- spelling 1: the driver's own pre-emption (the common carrier) ---
		{
			"the driver's read-budget pre-emption, exactly as it is minted",
			api.NewTransactionTimeLimitError(4100*time.Millisecond, 4*time.Second),
			true,
		},
		{
			"the pre-emption behind a fmt.Errorf tail",
			fmt.Errorf("page fetch: %w",
				api.NewTransactionTimeLimitError(4100*time.Millisecond, 4*time.Second)),
			true,
		},

		// --- spelling 2: FDB's own 1007 (the residual carrier) ---
		// As the relational layer now delivers it: translateFDBCode's 1007 arm
		// attaches the SAME marker the pre-emption carries, so both producers
		// answer the one predicate and this test does not have to know that 1007
		// is a number.
		{
			"FDB's 1007, marked at translateFDBCode",
			api.MarkFDBTransactionTooOld(fdb.Error{Code: 1007}),
			true,
		},
		{"bare value-typed fdb.Error", fdb.Error{Code: 1007}, true},
		{"bare pointer-typed wire.FDBError", &wire.FDBError{Code: 1007}, true},
		{
			"as the relational layer delivers it",
			api.WrapError(api.ErrCodeSerializationFailure, "FDB transaction too old",
				fdb.Error{Code: 1007}),
			true,
		},
		{
			"behind a fmt.Errorf tail",
			fmt.Errorf("page fetch: %w",
				api.WrapError(api.ErrCodeSerializationFailure, "FDB transaction too old",
					fdb.Error{Code: 1007})),
			true,
		},

		// --- the near misses ---
		// not_committed and transaction_timed_out are also load symptoms and also
		// map to a 4xxxx SQLSTATE, but neither proves the measurement window was
		// held-then-lost, and widening Detector A to "any retryable FDB error"
		// would let it fire on a genuine conflict bug and call it weather.
		{"not_committed (1020)", fdb.Error{Code: 1020}, false},
		{"transaction_timed_out (1031)", fdb.Error{Code: 1031}, false},
		{
			"a genuine conflict, which shares the pre-emption's SQLSTATE",
			api.WrapError(api.ErrCodeSerializationFailure,
				"transaction not committed due to conflict", fdb.Error{Code: 1020}),
			false,
		},
		{
			"a bare 40001 carrying no cause at all",
			api.NewError(api.ErrCodeSerializationFailure, "serialization failure"),
			false,
		},
		{"an unrelated plain error", errors.New("syntax error"), false},
		{
			"a relational error with no FDB cause",
			api.NewError(api.ErrCodeUndefinedTable, "no such table"),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := duecMeasurementWindowLost(tc.err); got != tc.want {
				t.Fatalf("duecMeasurementWindowLost(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDuecUnmeasuredRatio pins the ACCEPTING door's own failure mode, which is
// the one the two detectors cannot see.
//
// Detectors A and B decide whether the wall clock may carry a criterion. Neither
// asks whether a criterion was actually EVALUATED. Every timing bound in the
// probe is spelled `ratio > bound`, and in Go `NaN > 0.95` is FALSE — so a run
// that produced no usable per-rep ratios passes all three bounds, logs the regime
// as ASSERTED, and is indistinguishable from a run that measured R3 correctly.
// That is the same "reads as enforced while asserting nothing" failure this file
// exists to prevent, arriving through the door nobody was watching.
//
// The +Inf cases are not decoration: duecPerRepRatios drops zero DENOMINATORS but
// nothing rejects a zero numerator or a partially-truncated series, and division
// is where a non-finite value gets in without anyone writing one down.
func TestDuecUnmeasuredRatio(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ratios  map[string]float64
		wantBad bool
	}{
		{"a fully measured run", map[string]float64{"B/D'": 0.873, "s1": 0.877, "s50": 0.933}, false},
		{"one ratio NaN", map[string]float64{"B/D'": math.NaN(), "s1": 0.877, "s50": 0.933}, true},
		{"every ratio NaN", map[string]float64{"B/D'": math.NaN(), "s1": math.NaN()}, true},
		{"positive infinity", map[string]float64{"B/D'": math.Inf(1)}, true},
		{"negative infinity", map[string]float64{"B/D'": math.Inf(-1)}, true},
		// A ratio of exactly 0 is a MEASUREMENT, not an absence: it means the
		// numerator finished instantly, which is a real (if alarming) result and
		// the bounds below are entitled to judge it.
		{"a zero ratio is measured, not missing", map[string]float64{"B/D'": 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name, v, bad := duecUnmeasuredRatio(tc.ratios)
			if bad != tc.wantBad {
				t.Fatalf("duecUnmeasuredRatio(%v) reported bad=%v (%s=%v), want %v.\n"+
					"A false negative here lets an ACCEPTED regime assert three "+
					"criteria over values that passed only because they are not "+
					"numbers; a false positive reds a healthy run.",
					tc.ratios, bad, name, v, tc.wantBad)
			}
		})
	}
}

// TestDuecRegimeVerdict_WithholdsOnTheDriverPreemption drives the WHOLE verdict —
// not just the recogniser — with the carrier CI actually produced, and pins that
// the withheld reason names it.
//
// The recogniser table above and this are not redundant: a recogniser that
// returns true is useless if the verdict never consults it, and the probe's own
// wiring (duecRunInTx -> sawWindowLost -> duecRegimeVerdict) is what turns one
// into the other. This drives the far end of that wire.
func TestDuecRegimeVerdict_WithholdsOnTheDriverPreemption(t *testing.T) {
	t.Parallel()
	c, aPrime := duecQuietNullPair()
	// Isolate Detector A: Detector B must be happy with this pair on its own.
	if ok, _ := duecRegimeVerdict(false, false, c, aPrime); !ok {
		t.Fatal("the quiet null pair must be acceptable on its own, or this case " +
			"cannot isolate Detector A")
	}
	windowLost := duecMeasurementWindowLost(
		api.NewTransactionTimeLimitError(4100*time.Millisecond, 4*time.Second))
	if !windowLost {
		t.Fatal("the recogniser does not see the driver's read-budget pre-emption, so " +
			"the verdict below cannot be driven by it. This is the exact blind spot " +
			"that let a loaded CI box fatal the probe instead of withholding its " +
			"wall-clock criteria.")
	}
	ok, why := duecRegimeVerdict(false, windowLost, c, aPrime)
	if ok {
		t.Fatal("the regime detector ACCEPTED a run whose transaction was pre-empted " +
			"for outliving its read budget. Every duration taken in that window is a " +
			"truncated sample rather than a slow one.")
	}
	// The log line is the only artefact a CI reader gets from a withheld run, so
	// it must name the mechanism rather than just say "load".
	for _, want := range []string{"LOST ITS MEASUREMENT WINDOW", "TransactionTimeLimitError"} {
		if !strings.Contains(why, want) {
			t.Fatalf("the withholding reason does not mention %q: %s", want, why)
		}
	}
}

// duecAssertWallClock reports whether this invocation opted into ASSERTING the
// wall-clock criteria. See duecWallClockEnv for why the default is off.
//
// Deliberately strict about what counts as opting in: only "1" or "true".
// Accepting any non-empty value would let a stray `DUEC_ASSERT_WALLCLOCK=0`
// turn the arms back on, which is the opposite of what the operator wrote.
func duecAssertWallClock() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(duecWallClockEnv))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// TestDuecWallClockOptIn pins the gate in BOTH directions, because a default-off
// gate that is stuck off is indistinguishable from a deleted assertion, and one
// stuck on defeats the change.
//
// The values are not decorative. "0" and "false" must NOT arm — an operator who
// writes DUEC_ASSERT_WALLCLOCK=0 is asking for the arms to stay off, and an
// any-non-empty check would silently do the reverse.
func TestDuecWallClockOptIn(t *testing.T) {
	for _, tc := range []struct {
		set  bool
		val  string
		want bool
	}{
		{set: false, want: false},
		{set: true, val: "", want: false},
		{set: true, val: "1", want: true},
		{set: true, val: "true", want: true},
		{set: true, val: "TRUE", want: true},
		{set: true, val: " 1 ", want: true},
		{set: true, val: "0", want: false},
		{set: true, val: "false", want: false},
		{set: true, val: "yes", want: false},
	} {
		name := "unset"
		if tc.set {
			name = fmt.Sprintf("%q", tc.val)
			t.Setenv(duecWallClockEnv, tc.val)
		}
		t.Run(name, func(t *testing.T) {
			if got := duecAssertWallClock(); got != tc.want {
				t.Fatalf("duecAssertWallClock() = %t, want %t for %s.\n"+
					"  Default-off is the whole change: unset must NOT arm, or every CI lane "+
					"goes back to asserting a quiet-machine instrument. And an explicit 0/false "+
					"must not arm either — an any-non-empty check would turn an operator's "+
					"opt-OUT into an opt-in.", got, tc.want, name)
			}
		})
	}
}
