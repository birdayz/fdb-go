package values

import (
	"strings"
	"testing"
)

// The qualifier recovery census's ASSERTION, exercised in every failure
// direction against synthetic counts.
//
// The decision is split from the process-global counters precisely so this is
// possible: a gate whose only exercise is the corpus run that happens to be
// green proves nothing about what it does when the corpus is not. Each direction
// below is a mutation somebody could ship — a divergence appearing, a site going
// dark, a stale zero-declaration outliving its condition, a witness list
// saturating — and a gate that silently tolerates any of them is decoration.

func assertReport(t *testing.T, counts [qualRecSiteCount][qualRecClassCount]int,
	witnesses [qualRecSiteCount][qualRecClassCount][]string,
	exp *QualifierRecoveryExpectations,
) (bool, string) {
	t.Helper()
	var sb strings.Builder
	failed := assertQualifierRecoveryCounts(&sb, counts, witnesses, exp, "unit")
	return failed, sb.String()
}

// TestQualRecAssert_CleanCensusPasses is the control. Without it every test
// below could pass against an assertion that fails unconditionally.
func TestQualRecAssert_CleanCensusPasses(t *testing.T) {
	t.Parallel()
	var counts [qualRecSiteCount][qualRecClassCount]int
	var wit [qualRecSiteCount][qualRecClassCount][]string
	counts[QualRecSiteExistsSortSplit][QualRecAgreed] = 44
	counts[QualRecSiteDisplayLabelStrip][QualRecAgreed] = 722

	failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
		Floors: &QualifierRecoveryFloors{
			Calls: [qualRecSiteCount]int{
				QualRecSiteExistsSortSplit:   4,
				QualRecSiteDisplayLabelStrip: 70,
			},
			// Both populated sites must declare a real split floor. A control
			// that floored only one would trip the STALE check on the other and
			// pass for the wrong reason.
			Split: [qualRecSiteCount]int{
				QualRecSiteExistsSortSplit:   4,
				QualRecSiteDisplayLabelStrip: 70,
			},
		},
	})
	if failed {
		t.Fatalf("a census with no divergence, healthy floors and unsaturated witnesses FAILED:\n%s", out)
	}
}

// TestQualRecAssert_DivergedFails pins the census's ONE hard zero.
//
// DIVERGED is not debt — it is a split that contradicts an identity the site was
// holding, i.e. a wrong answer — and it is the only class asserted at zero.
// MANUFACTURED deliberately is NOT, because it is expected to be non-zero at
// several sites and a zero asserted there would be false the day it was written.
func TestQualRecAssert_DivergedFails(t *testing.T) {
	t.Parallel()
	var counts [qualRecSiteCount][qualRecClassCount]int
	var wit [qualRecSiteCount][qualRecClassCount][]string
	counts[QualRecSiteProjQualVsScan][QualRecDiverged] = 1
	wit[QualRecSiteProjQualVsScan][QualRecDiverged] = []string{`"T.COL" vs identity "<unqualified>"`}

	failed, out := assertReport(t, counts, wit, nil)
	if !failed {
		t.Fatalf("a DIVERGED decision did not fail the gate. The site manufactured a "+
			"qualifier contradicting the identity it held, which at projQualVsScan "+
			"REJECTS a valid query with ErrCodeUndefinedColumn:\n%s", out)
	}
	if !strings.Contains(out, "CONTRADICTS") {
		t.Fatalf("the failure text does not name what went wrong:\n%s", out)
	}
	// The fix direction must be stated, and stated the RIGHT way round: this is
	// the one failure whose tempting local remedy (widen the zero) is known-wrong.
	if !strings.Contains(out, "THE FIX IS TO USE THE IDENTITY") {
		t.Fatalf("the failure text does not name the fix direction. A tripper who reads "+
			"only 'DIVERGED != 0' will relax the check, which is the one response the "+
			"measurement rules out:\n%s", out)
	}
}

// TestQualRecAssert_AllowedDivergedClearsOnlyDeclaredWitnesses pins the
// negative-control mechanism, in BOTH directions.
//
// The translator corpus's own fixtures drive DIVERGED — they are what prove the
// bucket is reachable, without which the hard zero is a zero nothing has shown
// can be non-zero. So that corpus clears it witness by witness. The half that
// matters is the second: an UNDECLARED witness must still fail, or the allowlist
// is a mute button.
func TestQualRecAssert_AllowedDivergedClearsOnlyDeclaredWitnesses(t *testing.T) {
	t.Parallel()
	declared := `"T2.SK" vs identity "T9"`
	exp := &QualifierRecoveryExpectations{
		AllowedDiverged: map[QualifierRecoverySite]map[string]struct{}{
			QualRecSiteExistsSortSplit: {declared: {}},
		},
	}

	t.Run("declared witness clears", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteExistsSortSplit][QualRecDiverged] = 2
		wit[QualRecSiteExistsSortSplit][QualRecDiverged] = []string{declared}
		if failed, out := assertReport(t, counts, wit, exp); failed {
			t.Fatalf("a DIVERGED witness the fixtures declare did not clear:\n%s", out)
		}
	})

	t.Run("undeclared witness still fails", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteExistsSortSplit][QualRecDiverged] = 2
		wit[QualRecSiteExistsSortSplit][QualRecDiverged] = []string{declared, `"REAL.COL" vs identity "OTHER"`}
		failed, out := assertReport(t, counts, wit, exp)
		if !failed {
			t.Fatalf("a DIVERGED witness NO fixture accounts for cleared anyway. The "+
				"allowlist is then a count tolerance, and a real producer divergence "+
				"hides inside the fixtures' budget:\n%s", out)
		}
		if !strings.Contains(out, "REAL.COL") {
			t.Fatalf("the failure names the site but not the unaccounted witness, so the "+
				"reader cannot tell which pair is the producer's:\n%s", out)
		}
	})

	t.Run("a site with no allowlist is unaffected", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteDisplayLabelStrip][QualRecDiverged] = 1
		wit[QualRecSiteDisplayLabelStrip][QualRecDiverged] = []string{declared}
		if failed, _ := assertReport(t, counts, wit, exp); !failed {
			t.Fatal("an allowlist declared for ONE site cleared a divergence at ANOTHER. " +
				"The allowlist is per-site precisely so a fixture's licence does not " +
				"travel to a site whose splits all come from producers")
		}
	})
}

// TestQualRecAssert_CallsFloorFails pins the DARK-site direction.
//
// A splitter at zero reads exactly like a splitter measured clean. Every zero
// this census asserts is worthless over an empty population, so a site whose
// traffic stopped must fail rather than report a tidy row of zeros.
func TestQualRecAssert_CallsFloorFails(t *testing.T) {
	t.Parallel()
	var counts [qualRecSiteCount][qualRecClassCount]int
	var wit [qualRecSiteCount][qualRecClassCount][]string
	counts[QualRecSiteDisplayLabelStrip][QualRecAgreed] = 3

	failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
		Floors: &QualifierRecoveryFloors{
			Calls: [qualRecSiteCount]int{QualRecSiteDisplayLabelStrip: 70},
		},
	})
	if !failed {
		t.Fatalf("a site at 3 calls against a floor of 70 passed. The site has gone DARK "+
			"and its zeros mean nothing:\n%s", out)
	}
	if !strings.Contains(out, "DARK") {
		t.Fatalf("the failure text does not say the site went dark:\n%s", out)
	}
}

// TestQualRecAssert_SplitFloorFails pins the floor that actually carries the
// weight.
//
// A site's total can stay healthy on CARRIED traffic alone while the splitting
// arms — the only arms this census is about — go to zero. projScopeClassify is
// the real instance: 11 of its 71 corpus calls are carried, and a Calls floor
// alone would stay green with all 60 splits gone.
func TestQualRecAssert_SplitFloorFails(t *testing.T) {
	t.Parallel()
	var counts [qualRecSiteCount][qualRecClassCount]int
	var wit [qualRecSiteCount][qualRecClassCount][]string
	counts[QualRecSiteProjScopeClassify][QualRecCarried] = 71 // healthy total...
	// ...and not one split.

	failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
		Floors: &QualifierRecoveryFloors{
			Calls: [qualRecSiteCount]int{QualRecSiteProjScopeClassify: 8},
			Split: [qualRecSiteCount]int{QualRecSiteProjScopeClassify: 6},
		},
	})
	if !failed {
		t.Fatalf("a site with 71 CARRIED calls and 0 splits passed a split floor of 6. "+
			"Its Calls floor is green on carried traffic alone, so this is the only "+
			"floor that says the measured arms are still reached:\n%s", out)
	}
	if !strings.Contains(out, "SPLITTING arm") {
		t.Fatalf("the failure text does not distinguish the split floor from the call "+
			"floor, which is the whole reason there are two:\n%s", out)
	}
}

// TestQualRecAssert_StaleZeroDeclarationFails pins the STALE direction — the
// check that makes a declared 0 a declaration rather than an absent floor.
//
// A site declared 0 means "measured empty over this corpus, covered by a unit
// wiring pin instead". The day the corpus starts driving it, that sentence stops
// being true, and nothing else in the gate would notice: the population grew,
// which every other check reads as health.
func TestQualRecAssert_StaleZeroDeclarationFails(t *testing.T) {
	t.Parallel()
	var counts [qualRecSiteCount][qualRecClassCount]int
	var wit [qualRecSiteCount][qualRecClassCount][]string
	counts[QualRecSiteDerivedUnnestSource][QualRecBare] = 13

	failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
		Floors: &QualifierRecoveryFloors{
			Calls: [qualRecSiteCount]int{QualRecSiteDerivedUnnestSource: 2},
			Split: [qualRecSiteCount]int{QualRecSiteDerivedUnnestSource: 0}, // WATCHED, NOT PROVEN
		},
	})
	if !failed {
		t.Fatalf("a site declaring a split floor of 0 — 'measured empty, covered by a unit "+
			"pin' — reported 13 splits and passed. The declaration has gone stale and "+
			"the gate absorbed it silently:\n%s", out)
	}
	if !strings.Contains(out, "DECLARATION GOING STALE") {
		t.Fatalf("the failure text reads as a defect rather than as a declaration to "+
			"re-read, which sends the tripper looking for a bug that is not there:\n%s", out)
	}
}

// TestQualRecAssert_SaturationFailsOnlyForAnomalyClasses pins the asymmetry, in
// both directions.
//
// A saturated witness list is a SUBSET that reads like a whole. Whether that is
// a failure depends on what the list is FOR, and the two answers are opposite —
// getting this backwards produces either a gate that goes red because the suite
// grew, or an anomaly report that quietly names the wrong spellings.
func TestQualRecAssert_SaturationFailsOnlyForAnomalyClasses(t *testing.T) {
	t.Parallel()
	full := make([]string, qualRecWitnessCap)
	for i := range full {
		full[i] = string(rune('a' + i%26))
	}

	t.Run("MANUFACTURED saturation fails", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteRecursiveRemap][QualRecManufactured] = 500
		wit[QualRecSiteRecursiveRemap][QualRecManufactured] = full
		failed, out := assertReport(t, counts, wit, nil)
		if !failed {
			t.Fatalf("a SATURATED anomaly witness list passed. Its listed spellings are "+
				"whichever arrived first, and they are what somebody acts on:\n%s", out)
		}
	})

	t.Run("AGREED saturation notes but does not fail", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteDisplayLabelStrip][QualRecAgreed] = 722
		wit[QualRecSiteDisplayLabelStrip][QualRecAgreed] = full
		failed, out := assertReport(t, counts, wit, nil)
		if failed {
			t.Fatalf("a saturated HEALTH witness list failed the gate. displayLabelStrip "+
				"agrees on 700+ calls across dozens of aliases, so this makes the gate go "+
				"red because the suite grew:\n%s", out)
		}
		if !strings.Contains(out, "TRUNCATED") {
			t.Fatalf("the truncation was not announced at all, so the listed spellings "+
				"read as the whole set:\n%s", out)
		}
	})
}

// TestClassifyQualifierRecovery_PartitionsByIdentityInHand pins the shared
// classifier, which is what stops six sites from drifting in how they bucket the
// same situation.
//
// The distinction that carries the census: an ABSENT identity and a PRESENT but
// unqualified one are different facts, and a classifier that collapsed them
// would report every un-captured producer as a DIVERGENCE — burying the real
// ones — or every genuine contradiction as mere debt, which is how the hard zero
// would pass with the misread continuing.
func TestClassifyQualifierRecovery_PartitionsByIdentityInHand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		in              string
		identity        string
		identityPresent bool
		want            QualifierRecoveryClass
		why             string
	}{
		{
			"no dot", "COL", "", false, QualRecBare,
			"nothing to slice, nothing manufactured",
		},
		{
			"dot, no counterparty", "T.COL", "", false, QualRecManufactured,
			"the site holds no identity at all — the HARD debt, not convertible locally",
		},
		{
			"dot, agreeing identity", "T.COL", "T", true, QualRecAgreed,
			"the split is redundant over this input: CONVERSION-READY",
		},
		{
			"dot, disagreeing identity", "T.COL", "OTHER", true, QualRecDiverged,
			"a wrong answer, not debt",
		},
		{
			"dot, present-but-unqualified identity", "T.COL", "", true, QualRecDiverged,
			"THE CANONICAL MISREAD: the parser saw ONE segment (a delimited \"T.COL\") " +
				"and the split manufactured a qualifier from it. Collapsing this with " +
				"the absent-identity case would file the census's sharpest finding as " +
				"ordinary debt",
		},
		{
			"leading dot", ".COL", "", false, QualRecBare,
			"no qualifier bytes to take; not a manufacture",
		},
		{
			"trailing dot", "T.", "", false, QualRecBare,
			"no leaf; the sites that split on this shape record their OWN verdict " +
				"rather than this one, and this test is what keeps the difference visible",
		},
		{
			"case-insensitive agreement", "T.COL", "t", true, QualRecAgreed,
			"qualifier comparison folds case, as every leg comparison on this path does",
		},
		{
			"deeper path", "A.B.C", "A.B", true, QualRecAgreed,
			"LAST-dot, matching splitQualifier's own documented reading of A.B.C as " +
				"qualifier A.B — the classifier must agree with the sites it classifies",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := ClassifyQualifierRecovery(tc.in, tc.identity, tc.identityPresent)
			if got != tc.want {
				t.Fatalf("ClassifyQualifierRecovery(%q, %q, %v) = %v, want %v.\n%s",
					tc.in, tc.identity, tc.identityPresent, got, tc.want, tc.why)
			}
		})
	}
}

// TestRecordQualifierRecovery_GateOff pins that the recorder is free when the
// census is off — the reason production never pays for this instrument.
func TestRecordQualifierRecovery_GateOff(t *testing.T) {
	// Not parallel: it flips the process-global gate.
	restore := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	defer SetLegIdentityCensusEnabled(restore)

	before, _ := QualifierRecoveryCensus()
	RecordQualifierRecovery(QualRecSiteRecursiveRemap, QualRecManufactured, "T.COL", "")
	after, _ := QualifierRecoveryCensus()
	if after != before {
		t.Fatal("the recorder counted with the census gate OFF. The gate exists so the " +
			"production resolution path never pays an atomic for an instrument, and a " +
			"recorder that ignores it moves that cost into every query")
	}
}

// TestQualRecAssert_RetiredSplitInvertsTheAlarm drives every arm of the
// retirement check, which is the newest and least-reached guard in this file.
//
// A retired arm is the case the ordinary floors cannot express. A floor watches
// for COLLAPSE: it says a population that should be large has gone small. Once
// an arm is deleted, zero is the steady state and the danger inverts to GROWTH —
// a split arriving means the rendered-name recovery came back. Lowering the
// floor to 0 turns it into the "watched, not proven" DECLARATION, which is a
// statement about a CORPUS and is dropped the moment a -test.run filter narrows
// one; a tree fact must not be. So the two live in different fields, and this
// pins that they behave differently.
func TestQualRecAssert_RetiredSplitInvertsTheAlarm(t *testing.T) {
	t.Parallel()

	retired := func(sites ...QualifierRecoverySite) (r [qualRecSiteCount]bool) {
		for _, s := range sites {
			r[s] = true
		}
		return r
	}

	t.Run("a split at a retired site fails", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteRecursiveRemap][QualRecManufactured] = 1
		failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
			RetiredSplit: retired(QualRecSiteRecursiveRemap),
		})
		if !failed {
			t.Fatalf("a RETIRED splitting arm reported a split and the gate stayed green. "+
				"That arm was deleted; a call at it is the rendered-name recovery coming "+
				"back, which is the whole event this guard exists for:\n%s", out)
		}
		if !strings.Contains(out, "RETIRED") {
			t.Fatalf("the failure never says the arm is RETIRED, so the reader is sent to "+
				"raise a floor on something that is supposed to be gone:\n%s", out)
		}
	})

	t.Run("every class but CARRIED counts as a split", func(t *testing.T) {
		t.Parallel()
		for _, c := range []QualifierRecoveryClass{
			QualRecAgreed, QualRecDiverged, QualRecManufactured,
			QualRecLeafOnly, QualRecBare, QualRecHeuristicDecline,
		} {
			var counts [qualRecSiteCount][qualRecClassCount]int
			var wit [qualRecSiteCount][qualRecClassCount][]string
			counts[QualRecSiteProjScopeClassify][c] = 1
			failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
				RetiredSplit: retired(QualRecSiteProjScopeClassify),
			})
			if !failed {
				t.Fatalf("a %s call at a retired site passed. Every class but CARRIED means a "+
					"rendered name was sliced, so every one of them is a revival:\n%s", c, out)
			}
		}
	})

	t.Run("CARRIED alone does not fire it", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteProjScopeClassify][QualRecCarried] = 73
		failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
			RetiredSplit: retired(QualRecSiteProjScopeClassify),
		})
		if failed {
			t.Fatalf("a site answering entirely on the CARRIED channel failed its retirement "+
				"check. CARRIED is the converted channel and is exactly what a retired "+
				"splitter is supposed to report:\n%s", out)
		}
	})

	t.Run("it survives the floors being dropped", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteRecursiveRemap][QualRecBare] = 3
		// Floors nil is what a -test.run filter passes. The whole reason
		// retirement is not a Split zero is that the zero would vanish here.
		failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
			Floors:       nil,
			RetiredSplit: retired(QualRecSiteRecursiveRemap),
		})
		if !failed {
			t.Fatalf("the retirement check was skipped when the floors were dropped. Under a "+
				"narrowed run that is the one direction that fails OPEN — the arm comes back "+
				"and nothing says so:\n%s", out)
		}
	})

	t.Run("a retired site is exempt from the stale-zero declaration", func(t *testing.T) {
		t.Parallel()
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteRecursiveRemap][QualRecBare] = 3
		failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{
			Floors:       &QualifierRecoveryFloors{},
			RetiredSplit: retired(QualRecSiteRecursiveRemap),
		})
		if !failed {
			t.Fatalf("the revival went unreported entirely:\n%s", out)
		}
		if n := strings.Count(out, "FAIL:"); n != 1 {
			t.Fatalf("one revival produced %d FAIL lines, want 1. The stale-zero declaration "+
				"and the retirement describe the same event with opposite instructions, so "+
				"reporting both tells the reader to raise a floor AND to delete the arm:\n%s",
				n, out)
		}
	})

	t.Run("without the declaration a retired site is unwatched", func(t *testing.T) {
		t.Parallel()
		// The negative control for the whole mechanism: the same counts, with the
		// site NOT declared retired and no floor on it, pass. That is what the
		// tree looked like before this field existed, and it is why an unfloored
		// zero is not a guard.
		var counts [qualRecSiteCount][qualRecClassCount]int
		var wit [qualRecSiteCount][qualRecClassCount][]string
		counts[QualRecSiteRecursiveRemap][QualRecBare] = 3
		failed, out := assertReport(t, counts, wit, &QualifierRecoveryExpectations{})
		if failed {
			t.Fatalf("the control failed, so every arm above may be passing for a reason "+
				"other than the one under test:\n%s", out)
		}
	})
}
