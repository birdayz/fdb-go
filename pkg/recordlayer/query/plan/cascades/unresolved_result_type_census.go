package cascades

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The UNRESOLVED-RESULT-TYPE census (RFC-213 §7).
//
// It sizes the PAYOFF of deriving a plan's result type from its result value,
// and it exists because RFC-213's first draft could only INFER that payoff.
//
// Twelve plans returned values.UnknownType from GetResultType when this census
// was built, and every consumer that reads a result type fails CLOSED on one —
// type-asserts *values.RecordType, misses, and declines. Declining is
// INVISIBLE: it costs an optimization or a proof, never a wrong row, so no test
// goes red and no number moves. That is precisely why the size of the loss had
// to be counted rather than argued.
//
// RFC-232 CLOSED THAT POPULATION, so this paragraph is history: stubInventory()
// is empty, and findStubs finds no UNCONDITIONAL stub among the non-test plans
// under planPkgDir. "Unconditional" is the word the instrument rests on --
// explode.go still returns the singleton on a branch, through GetElementType(),
// and the detector deliberately does not count it.
//
// HISTORICAL, and the figures below are frozen at the readings named with them.
// RFC-213 §6 measured 135 unresolved reads over FIVE consumers -- 31 at
// distinctKeyColumns, 104 at planRowRecordType, and zero at the other three,
// one of which (predicatesFilterIsFullPKPointProbe) declined none of 14,486.
// All 135 flipped: at d15a81c07 the gate recorded four consumers at UNRESOLVED
// 0, planRowRecordType among them at 784 resolved, under MinSites 4. RFC-232 is
// not the sole cause -- RFC-226 landed inside the same window -- so what is
// provable is that the reads flipped, not which change flipped each one.
//
// Two of those consumers were deleted AFTERWARDS by RFC-235, when the
// three-quantifier NLJ arm that was their only live caller went away. That is a
// separate event, and not a way of retiring reads instead of fixing them: the
// zero predates it. The gate reads the two survivors today.
//
// The census stays as the instrument that would show the population coming
// back, and the sqldriver gate keeps asserting the consumers are still reached,
// so a future zero cannot be read as the instrument going dark.
//
// GATED by values.LegIdentityCensusEnabled, like every census on this path: the
// recorder's first statement is the gate, so a disabled census costs one atomic
// load. Callers pass the type they ALREADY have — no work is done to build the
// argument.
//
// THE DECISION TAKES EXPLICIT STATE. Everything below the recorder — the
// classification, the site tally, the floors, the failure text — is a function of
// an unresolvedResultTypeCounters value passed in, never of the package globals.
// That is not tidiness: the gate that consumes this census runs only under the
// real-FDB corpus, so a full-suite run exercises exactly the arms the corpus
// happens to reach and every other arm ships untested. Its first real firing then
// reads as a finding rather than as an untested branch. With the decision
// separated, unresolved_result_type_census_test.go drives every class, every
// floor and every vacuity guard as ordinary unit tests.
type unresolvedResultTypeCounters struct {
	// Resolved / Unresolved partition every classified read by whether the type
	// the consumer got was usable.
	Resolved   map[string]int
	Unresolved map[string]int
}

// classifyResultTypeReadUnresolved reports whether the type a consumer got was
// UNUSABLE, which is the census's single discriminator.
//
// UNRESOLVED is the type CODE, not pointer identity against the UnknownType
// singleton: an equivalent unknown built elsewhere is just as unusable, and
// keying on the shared variable would count it as resolved. This is the same
// discriminator quantifiedObjectValueIsTyped uses, for the same reason. A nil
// type is unresolved too — a consumer that got nothing got nothing usable.
func classifyResultTypeReadUnresolved(t values.Type) bool {
	return t == nil || t.Code() == values.TypeCodeUnknown
}

// record adds one classified read to c. The maps are allocated on demand so a
// zero-value counters struct is usable, which is what lets a test state its
// input literally.
func (c *unresolvedResultTypeCounters) record(site string, t values.Type) {
	if classifyResultTypeReadUnresolved(t) {
		if c.Unresolved == nil {
			c.Unresolved = map[string]int{}
		}
		c.Unresolved[site]++
		return
	}
	if c.Resolved == nil {
		c.Resolved = map[string]int{}
	}
	c.Resolved[site]++
}

// totals returns the classified-read count and the number of DISTINCT deciding
// sites that reported at least one read.
//
// The two are separate populations on purpose, and the reason is the failure this
// census could otherwise not see: one hot consumer can carry the read total three
// orders of magnitude past any floor while every other consumer reports nothing.
// The total then says "reached" about an instrument that has gone dark over most
// of the population it claims to denominate.
func (c unresolvedResultTypeCounters) totals() (reads, sites int) {
	seen := map[string]bool{}
	for k, v := range c.Resolved {
		reads += v
		if v > 0 {
			seen[k] = true
		}
	}
	for k, v := range c.Unresolved {
		reads += v
		if v > 0 {
			seen[k] = true
		}
	}
	return reads, len(seen)
}

var (
	unresolvedRTMu     sync.Mutex
	unresolvedRTCounts = unresolvedResultTypeCounters{
		Resolved:   map[string]int{},
		Unresolved: map[string]int{},
	}
)

// recordResultTypeRead counts ONE consumer read of a plan's result type, keyed
// by the deciding site.
//
// The gate is the FIRST statement and it returns: the classification below is a
// type assertion the caller has usually already made, but the map write and the
// string key are not free and nothing consumes them with the census off.
func recordResultTypeRead(site string, t values.Type) {
	if !values.LegIdentityCensusEnabled() {
		return
	}
	unresolvedRTMu.Lock()
	defer unresolvedRTMu.Unlock()
	unresolvedRTCounts.record(site, t)
}

// unresolvedResultTypeSnapshot deep-copies the process counters.
//
// Deep-copied under the mutex: handing back the maps would give a caller a live
// view of state the planner is still writing — which under a concurrent plan is
// not a stale number but a fatal concurrent map iteration and map write.
func unresolvedResultTypeSnapshot() unresolvedResultTypeCounters {
	unresolvedRTMu.Lock()
	defer unresolvedRTMu.Unlock()
	out := unresolvedResultTypeCounters{
		Resolved:   make(map[string]int, len(unresolvedRTCounts.Resolved)),
		Unresolved: make(map[string]int, len(unresolvedRTCounts.Unresolved)),
	}
	for k, v := range unresolvedRTCounts.Resolved {
		out.Resolved[k] = v
	}
	for k, v := range unresolvedRTCounts.Unresolved {
		out.Unresolved[k] = v
	}
	return out
}

// ResetUnresolvedResultTypeCensus clears the process counters. Present so a test
// can drive the REAL recorder — recordResultTypeRead, by name — rather than a
// local copy of its body, which is the mutation a copy would survive.
func ResetUnresolvedResultTypeCensus() {
	unresolvedRTMu.Lock()
	defer unresolvedRTMu.Unlock()
	unresolvedRTCounts = unresolvedResultTypeCounters{
		Resolved:   map[string]int{},
		Unresolved: map[string]int{},
	}
}

// UnresolvedResultTypeCensus reports a SNAPSHOT of the counters as two maps,
// resolved and unresolved, keyed by deciding site.
func UnresolvedResultTypeCensus() (map[string]int, map[string]int) {
	c := unresolvedResultTypeSnapshot()
	return c.Resolved, c.Unresolved
}

// FormatUnresolvedResultTypeCensus renders the process census for a harness to
// log.
func FormatUnresolvedResultTypeCensus() string {
	return formatUnresolvedResultTypeCensus(unresolvedResultTypeSnapshot())
}

// formatUnresolvedResultTypeCensus renders EXPLICIT state, so the rendering a
// failure quotes is the same code a unit test can drive.
func formatUnresolvedResultTypeCensus(c unresolvedResultTypeCounters) string {
	sites := map[string]bool{}
	for k := range c.Resolved {
		sites[k] = true
	}
	for k := range c.Unresolved {
		sites[k] = true
	}
	keys := make([]string, 0, len(sites))
	for k := range sites {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("result-type reads by consumer (RFC-213 payoff):")
	totalR, totalU := 0, 0
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %-40s resolved %-6d UNRESOLVED %d", k, c.Resolved[k], c.Unresolved[k])
		totalR += c.Resolved[k]
		totalU += c.Unresolved[k]
	}
	fmt.Fprintf(&b, "\n  %-40s resolved %-6d UNRESOLVED %d", "TOTAL", totalR, totalU)
	return b.String()
}

// UnresolvedResultTypeFloors are the two populations this census refuses to let
// collapse silently.
//
// There is deliberately NO floor on the unresolved count and no zero to defend:
// RFC-213 has not been implemented, so the unresolved reads are the DEFECT and
// their number is a measurement, not a contract. What is guarded is the reading.
//
//   - MinReads guards VACUITY. With no classified reads at all, a later
//     "unresolved is now 0" is indistinguishable from success, so every finding
//     denominated against this census becomes unreadable.
//
//   - MinSites guards a DEAD INSTRUMENT. MinReads alone cannot see it: one hot
//     consumer clears any read floor by orders of magnitude while every other
//     deciding site reports nothing, and the census then speaks for a population
//     it stopped observing.
//
// BOTH ARE COLLAPSE GUARDS — the alarm is downward, and that is a statement with
// a shelf life. If RFC-213 lands and a consumer stops reading result types
// because it no longer needs to, zero becomes that site's steady state and
// MinSites becomes unsatisfiable. Reconcile it with the new expected value then;
// do not relax it to whatever the run produced, and do not delete it, which would
// leave the retired consumer's return unwatched.
//
// A nil *UnresolvedResultTypeFloors, and equally a floors value of all zeroes, is
// a NO-OP by construction — the shape a narrowed run needs, where a whole-corpus
// population claim has nothing it can honestly decide.
type UnresolvedResultTypeFloors struct {
	MinReads int
	MinSites int
}

// AssertUnresolvedResultTypeCensus checks the process census against floors.
func AssertUnresolvedResultTypeCensus(w io.Writer, floors *UnresolvedResultTypeFloors) bool {
	return assertUnresolvedResultTypeCensusState(w, unresolvedResultTypeSnapshot(), floors)
}

// assertUnresolvedResultTypeCensusState is the whole decision over EXPLICIT
// state. It reports whether the census failed, writing the reason to w.
func assertUnresolvedResultTypeCensusState(w io.Writer, c unresolvedResultTypeCounters, floors *UnresolvedResultTypeFloors) bool {
	if floors == nil {
		return false
	}
	reads, sites := c.totals()
	failed := false
	if floors.MinReads > 0 && reads < floors.MinReads {
		failed = true
		fmt.Fprintf(w, "UNRESOLVED-RESULT-TYPE CENSUS FAIL: %d classified read(s), want >= %d.\n"+
			"  ALARM DIRECTION: collapse. The consumers RFC-213 is denominated against have gone\n"+
			"  dark, so the unresolved count beside them is measuring an absence of TRAFFIC rather\n"+
			"  than an absence of the defect. Any later claim that the payoff shrank is unreadable.\n",
			reads, floors.MinReads)
	}
	if floors.MinSites > 0 && sites < floors.MinSites {
		failed = true
		fmt.Fprintf(w, "UNRESOLVED-RESULT-TYPE CENSUS FAIL: %d deciding site(s) reported, want >= %d.\n"+
			"  ALARM DIRECTION: collapse. A read floor cannot see this — one hot consumer clears it\n"+
			"  alone while the rest of the population reports nothing, and the census then speaks\n"+
			"  for consumers it has stopped observing. If a consumer was deliberately retired,\n"+
			"  RECONCILE this floor with the new expected count and say so here; do not lower it to\n"+
			"  match the run, and do not remove it, which leaves the retirement unwatched.\n",
			sites, floors.MinSites)
	}
	if failed {
		fmt.Fprintf(w, "  census: %s\n", formatUnresolvedResultTypeCensus(c))
	}
	return failed
}
