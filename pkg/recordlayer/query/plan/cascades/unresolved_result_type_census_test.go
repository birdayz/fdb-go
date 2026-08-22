package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The UNIT PIN for the unresolved-result-type census (RFC-213 §7).
//
// The census's only consumer is the real-FDB sqldriver corpus, whose TestMain
// calls the assertion once at the end of a whole run. That arrangement drives
// exactly the arms the corpus happens to reach and no others: a floor the corpus
// clears by three orders of magnitude is never seen to fail, a classification
// branch no query reaches is never seen at all, and the vacuity guard that makes
// a narrowed run a no-op is exercised only by whoever happens to narrow. The
// first real firing of any of them then reads as a FINDING rather than as an
// untested branch, which is the most expensive way to discover a bug in an
// instrument.
//
// So every class, every floor and the vacuity guard are driven here, over
// explicit state, with no dependence on what a corpus contains.
//
// Two populations are guarded, and the distinction is the point of the second
// floor: MinReads catches the census going VACUOUS (no traffic, so "unresolved is
// 0" is unreadable), MinSites catches it going DEAD (traffic from one hot
// consumer while the rest report nothing). TestUnresolvedResultTypeCensus_
// OneHotSiteDoesNotSatisfyTheSiteFloor is the case that exists only because the
// two are separate.

// nonUnknownType is a types.Type whose Code is not TypeCodeUnknown, i.e. a type a
// consumer can actually use.
func nonUnknownType(t *testing.T) values.Type {
	t.Helper()
	typ := values.NewPrimitiveType(values.TypeCodeString, false)
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		t.Fatalf("the RESOLVED fixture is itself unknown (code %v) — every 'resolved' case below "+
			"would then be testing the unresolved arm and would pass for the wrong reason", typ)
	}
	return typ
}

func TestClassifyResultTypeReadUnresolved_DrivesBothClasses(t *testing.T) {
	t.Parallel()
	resolved := nonUnknownType(t)
	for _, c := range []struct {
		name string
		typ  values.Type
		want bool
		why  string
	}{
		{
			"a usable type is resolved", resolved, false,
			"the ordinary case; if this classified unresolved the census would report the whole corpus as defective",
		},
		{
			"the UnknownType singleton is unresolved", values.UnknownType, true,
			"the GetResultType stubs RFC-232 retired returned exactly this",
		},
		{
			"a nil type is unresolved", nil, true,
			"a consumer handed nothing got nothing usable, and counting it resolved would shrink the measured payoff",
		},
		{
			"an EQUIVALENT unknown built elsewhere is unresolved", values.NewPrimitiveType(values.TypeCodeUnknown, false), true,
			"the discriminator is the type CODE, not pointer identity against the singleton: an unknown minted " +
				"anywhere else is just as unusable, and keying on the shared variable would count it resolved",
		},
		{
			"a distinct pointer EQUAL to the singleton is unresolved", values.NewPrimitiveType(values.TypeCodeUnknown, true), true,
			"UnknownType is itself {Unknown, nullable}, so this value is equal to it but is a DIFFERENT pointer; " +
				"a discriminator written as t == values.UnknownType would call it resolved",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyResultTypeReadUnresolved(c.typ); got != c.want {
				t.Fatalf("classifyResultTypeReadUnresolved(%v) = %v, want %v — %s", c.typ, got, c.want, c.why)
			}
		})
	}
}

// TestRecordResultTypeRead_FeedsTheProcessCounters drives the REAL recorder by
// name.
//
// A pin that exercised only the counters type would stay green if
// recordResultTypeRead stopped calling it, or classified with a different
// discriminator, or wrote to a different map — the exact mutation a local copy of
// the body survives. This one runs the production function.
//
// It is not parallel: it owns the process counters for its duration, and the gate
// flag is process-wide. Every other test in this file is over explicit state and
// stays parallel.
func TestRecordResultTypeRead_FeedsTheProcessCounters(t *testing.T) {
	was := values.LegIdentityCensusEnabled()
	values.SetLegIdentityCensusEnabled(true)
	ResetUnresolvedResultTypeCensus()
	t.Cleanup(func() {
		ResetUnresolvedResultTypeCensus()
		values.SetLegIdentityCensusEnabled(was)
	})

	recordResultTypeRead("siteA", nonUnknownType(t))
	recordResultTypeRead("siteA", values.UnknownType)
	recordResultTypeRead("siteB", nil)

	res, unres := UnresolvedResultTypeCensus()
	if res["siteA"] != 1 || unres["siteA"] != 1 || unres["siteB"] != 1 {
		t.Fatalf("recordResultTypeRead did not land its reads where the census reads them: "+
			"resolved %v, unresolved %v", res, unres)
	}
	if res["siteB"] != 0 {
		t.Fatalf("a nil type was counted RESOLVED at siteB (%d) — the payoff RFC-213 is denominated "+
			"against would be understated by exactly the nil reads", res["siteB"])
	}

	// The gate is the recorder's FIRST statement, and a census that kept counting
	// with the gate off would cost every planner call a lock and a map write.
	values.SetLegIdentityCensusEnabled(false)
	recordResultTypeRead("siteC", values.UnknownType)
	if _, unres := UnresolvedResultTypeCensus(); unres["siteC"] != 0 {
		t.Fatalf("recordResultTypeRead counted a read with LegIdentityCensusEnabled() false "+
			"(siteC = %d); the gate is meant to return before any work", unres["siteC"])
	}
}

func TestUnresolvedResultTypeCounters_TotalsCountReadsAndSitesSeparately(t *testing.T) {
	t.Parallel()
	var c unresolvedResultTypeCounters
	if reads, sites := c.totals(); reads != 0 || sites != 0 {
		t.Fatalf("a zero-value counters struct totalled %d read(s) over %d site(s), want 0/0", reads, sites)
	}
	resolved := nonUnknownType(t)
	c.record("hot", resolved)
	c.record("hot", values.UnknownType)
	c.record("cold", values.UnknownType)
	reads, sites := c.totals()
	if reads != 3 || sites != 2 {
		t.Fatalf("totals() = %d read(s) over %d site(s), want 3 over 2 — a site tally that counted "+
			"reads, or a read tally that counted sites, would make one of the two floors meaningless",
			reads, sites)
	}
	// A site present in the map with a zero count is not a REPORTING site. This is
	// the shape a future reset-per-site or pre-registration would produce, and
	// counting it would let the site floor pass over an instrument that went dark.
	c.Resolved["registered_but_silent"] = 0
	if _, sites := c.totals(); sites != 2 {
		t.Fatalf("a site keyed with a zero count was counted as reporting (sites = %d, want 2); the "+
			"site floor would then be satisfied by registration rather than by traffic", sites)
	}
}

// countersWith builds explicit state: n reads at each named site, all unresolved.
func countersWith(t *testing.T, perSite int, sites ...string) unresolvedResultTypeCounters {
	t.Helper()
	var c unresolvedResultTypeCounters
	for _, s := range sites {
		for i := 0; i < perSite; i++ {
			c.record(s, values.UnknownType)
		}
	}
	return c
}

func TestAssertUnresolvedResultTypeCensusState_DrivesEveryFloor(t *testing.T) {
	t.Parallel()
	resolved := nonUnknownType(t)
	for _, c := range []struct {
		name       string
		state      unresolvedResultTypeCounters
		floors     *UnresolvedResultTypeFloors
		wantFailed bool
		wantIn     string
		why        string
	}{
		{
			name:   "both floors cleared",
			state:  countersWith(t, 10, "a", "b", "c"),
			floors: &UnresolvedResultTypeFloors{MinReads: 30, MinSites: 3},
			why:    "the exact boundary must PASS; an off-by-one here reds a healthy corpus",
		},
		{
			name:       "one read short of the read floor",
			state:      countersWith(t, 10, "a", "b"),
			floors:     &UnresolvedResultTypeFloors{MinReads: 21},
			wantFailed: true,
			wantIn:     "20 classified read(s), want >= 21",
			why:        "the read floor's boundary in the failing direction",
		},
		{
			name:       "one site short of the site floor",
			state:      countersWith(t, 1000, "a", "b"),
			floors:     &UnresolvedResultTypeFloors{MinSites: 3},
			wantFailed: true,
			wantIn:     "2 deciding site(s) reported, want >= 3",
			why:        "2000 reads clear any plausible read floor; only the site floor can see this",
		},
		{
			name:       "both floors missed reports both",
			state:      countersWith(t, 1, "a"),
			floors:     &UnresolvedResultTypeFloors{MinReads: 100, MinSites: 5},
			wantFailed: true,
			wantIn:     "deciding site(s) reported",
			why:        "a first-failure-wins assertion would hide the second collapse behind the first",
		},
		{
			name:   "a zero read floor is not checked",
			state:  unresolvedResultTypeCounters{},
			floors: &UnresolvedResultTypeFloors{MinReads: 0, MinSites: 0},
			why:    "the narrowed-run shape: an all-zero floors value decides nothing",
		},
		{
			name: "resolved reads count toward the floors too",
			state: func() unresolvedResultTypeCounters {
				var c unresolvedResultTypeCounters
				c.record("a", resolved)
				c.record("b", resolved)
				return c
			}(),
			floors: &UnresolvedResultTypeFloors{MinReads: 2, MinSites: 2},
			why: "the floors guard TRAFFIC, not the defect — a census that counted only unresolved " +
				"reads would red the moment RFC-213 started working, which is the opposite of the alarm",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var w strings.Builder
			got := assertUnresolvedResultTypeCensusState(&w, c.state, c.floors)
			if got != c.wantFailed {
				t.Fatalf("failed = %v, want %v — %s\noutput: %s", got, c.wantFailed, c.why, w.String())
			}
			if c.wantIn != "" && !strings.Contains(w.String(), c.wantIn) {
				t.Fatalf("failure text does not contain %q — %s\noutput: %s", c.wantIn, c.why, w.String())
			}
			if !c.wantFailed && w.Len() != 0 {
				t.Fatalf("a passing census wrote %q; a gate that always writes teaches its reader to "+
					"ignore the output", w.String())
			}
		})
	}
}

// TestUnresolvedResultTypeCensus_OneHotSiteDoesNotSatisfyTheSiteFloor is why
// MinSites exists at all.
//
// It states the failure in the shape it would really arrive: the corpus keeps
// hammering one consumer, the read total stays enormous, and every other deciding
// site has stopped reporting. A single-population census calls that healthy.
func TestUnresolvedResultTypeCensus_OneHotSiteDoesNotSatisfyTheSiteFloor(t *testing.T) {
	t.Parallel()
	// The measured shape: this one consumer contributed 13651 of the corpus's 15074
	// classified reads, so it alone clears any plausible read floor.
	hot := countersWith(t, 13651, "predicatesFilterIsFullPKPointProbe")
	floors := &UnresolvedResultTypeFloors{MinReads: 1000, MinSites: 5}

	var w strings.Builder
	if !assertUnresolvedResultTypeCensusState(&w, hot, floors) {
		t.Fatalf("a census reporting 13651 reads from ONE of five deciding sites passed. The read "+
			"floor cannot see this — that is the entire reason the site floor is a separate "+
			"population — so with it satisfied the census would go on speaking for four consumers "+
			"it stopped observing.\noutput: %s", w.String())
	}
	if strings.Contains(w.String(), "classified read(s), want") {
		t.Fatalf("the read floor also fired, so this case does not isolate the site floor: %s", w.String())
	}
	if !strings.Contains(w.String(), "ALARM DIRECTION: collapse") {
		t.Fatalf("the failure text does not name the alarm direction. A floor whose direction is "+
			"unstated cannot be reconciled when the expected value changes; it just gets "+
			"relaxed.\noutput: %s", w.String())
	}
}

// TestAssertUnresolvedResultTypeCensus_NilFloorsNeverFails pins the vacuity
// guard the narrowed-run wrapper depends on.
//
// The sqldriver gate passes nil under `-test.run` narrowing because a
// whole-corpus population claim has nothing it can honestly decide over a
// filtered run. Nothing else asserts that nil is a no-op, and if it ever stopped
// being one, every narrowed run in the repository would red on the filter rather
// than on a defect — which is precisely the failure that taught the census family
// to withhold floors in the first place.
func TestAssertUnresolvedResultTypeCensus_NilFloorsNeverFails(t *testing.T) {
	t.Parallel()
	var w strings.Builder
	if assertUnresolvedResultTypeCensusState(&w, unresolvedResultTypeCounters{}, nil) {
		t.Fatalf("nil floors failed over an EMPTY census: %s", w.String())
	}
	if w.Len() != 0 {
		t.Fatalf("nil floors wrote %q; the no-op must be silent", w.String())
	}
	if assertUnresolvedResultTypeCensusState(&w, countersWith(t, 1, "only"), nil) {
		t.Fatalf("nil floors failed over a populated census: %s", w.String())
	}
}

func TestFormatUnresolvedResultTypeCensus_RendersEverySiteAndTheTotal(t *testing.T) {
	t.Parallel()
	var c unresolvedResultTypeCounters
	c.record("zeta", nonUnknownType(t))
	c.record("alpha", values.UnknownType)
	c.record("alpha", values.UnknownType)

	got := formatUnresolvedResultTypeCensus(c)
	for _, want := range []string{"alpha", "zeta", "TOTAL"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendering omits %q — a site missing from the report is a site nobody can "+
				"see collapse:\n%s", want, got)
		}
	}
	if strings.Index(got, "alpha") > strings.Index(got, "zeta") {
		t.Fatalf("sites are not sorted; an unordered census makes two runs impossible to diff:\n%s", got)
	}
	// The TOTAL line is the number every finding quotes, so it is asserted, not
	// merely present: 1 resolved and 2 unresolved.
	total := got[strings.LastIndex(got, "TOTAL"):]
	if !strings.Contains(total, "resolved 1") || !strings.Contains(total, "UNRESOLVED 2") {
		t.Fatalf("TOTAL line is %q, want 1 resolved and 2 unresolved", total)
	}
}
