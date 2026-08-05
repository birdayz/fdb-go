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
func duecQuietNullPair() (c, aPrime []duecSample) {
	return duecSamplesMs(178, 181, 175, 183, 177, 180, 174, 179, 176),
		duecSamplesMs(174, 177, 176, 178, 172, 175, 173, 176, 171)
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

// TestDuecIsTransactionTooOld pins Detector A's recogniser against both carriers
// AND against the wrapping the relational layer actually applies.
//
// The wrapping half is the load-bearing one. The probe never sees a bare
// fdb.Error: connection.go's translateFDBCode turns 1007 into
// api.Error{Code: 40001} and keeps the original as the cause, and database/sql
// hands that through. A recogniser that only matched the bare error would report
// every real 1007 as healthy, and Detector A would be dead on the exact input it
// exists for.
func TestDuecIsTransactionTooOld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
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
		// The near misses. not_committed and transaction_timed_out are also
		// load symptoms and also map to a 4xxxx SQLSTATE, but neither proves
		// the measurement window was held-then-lost the way 1007 does, and
		// widening Detector A to "any retryable FDB error" would let it fire on
		// a genuine conflict bug and call it weather.
		{"not_committed (1020)", fdb.Error{Code: 1020}, false},
		{"transaction_timed_out (1031)", fdb.Error{Code: 1031}, false},
		{"an unrelated plain error", errors.New("syntax error"), false},
		{
			"a relational error with no FDB cause",
			api.NewError(api.ErrCodeUndefinedTable, "no such table"),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := duecIsTransactionTooOld(tc.err); got != tc.want {
				t.Fatalf("duecIsTransactionTooOld(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
