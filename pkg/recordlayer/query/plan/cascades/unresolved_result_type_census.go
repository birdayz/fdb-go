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
// Twelve plans return values.UnknownType from GetResultType, and every consumer
// that reads a result type fails CLOSED on it — type-asserts *values.RecordType,
// misses, and declines. Declining is INVISIBLE: it costs an optimization or a
// proof, never a wrong row, so no test goes red and no number moves. That is
// precisely why the size of the loss has to be counted rather than argued.
//
// WHAT A ZERO WOULD MEAN, and it is not "no problem": it would mean no corpus
// query reaches these deciders with a stubbed plan at all, so RFC-213's
// implementation would be correct and LATENT — worth doing for coherence, but
// with no measurable plan-quality payoff to claim. That is a materially
// different RFC, and a printed zero is the only thing that can say so.
//
// GATED by values.LegIdentityCensusEnabled, like every census on this path: the
// recorder's first statement is the gate, so a disabled census costs one atomic
// load. Callers pass the type they ALREADY have — no work is done to build the
// argument.
type unresolvedResultTypeCounters struct {
	// Resolved / Unresolved partition every classified read by whether the type
	// the consumer got was usable.
	Resolved   map[string]int
	Unresolved map[string]int
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
	// UNRESOLVED is the type CODE, not pointer identity against the UnknownType
	// singleton: an equivalent unknown built elsewhere is just as unusable, and
	// keying on the shared variable would count it as resolved. This is the same
	// discriminator quantifiedObjectValueIsTyped uses, for the same reason.
	if t == nil || t.Code() == values.TypeCodeUnknown {
		unresolvedRTCounts.Unresolved[site]++
		return
	}
	unresolvedRTCounts.Resolved[site]++
}

// UnresolvedResultTypeCensus reports a SNAPSHOT of the counters.
//
// Deep-copied under the mutex: returning the struct by value would share the
// maps, handing a caller a live view of state the planner is still writing —
// which under a concurrent plan is not a stale number but a fatal concurrent map
// iteration and map write.
func UnresolvedResultTypeCensus() (map[string]int, map[string]int) {
	unresolvedRTMu.Lock()
	defer unresolvedRTMu.Unlock()
	res := make(map[string]int, len(unresolvedRTCounts.Resolved))
	unres := make(map[string]int, len(unresolvedRTCounts.Unresolved))
	for k, v := range unresolvedRTCounts.Resolved {
		res[k] = v
	}
	for k, v := range unresolvedRTCounts.Unresolved {
		unres[k] = v
	}
	return res, unres
}

// FormatUnresolvedResultTypeCensus renders the census for a harness to log.
func FormatUnresolvedResultTypeCensus() string {
	res, unres := UnresolvedResultTypeCensus()
	sites := map[string]bool{}
	for k := range res {
		sites[k] = true
	}
	for k := range unres {
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
		fmt.Fprintf(&b, "\n  %-40s resolved %-6d UNRESOLVED %d", k, res[k], unres[k])
		totalR += res[k]
		totalU += unres[k]
	}
	fmt.Fprintf(&b, "\n  %-40s resolved %-6d UNRESOLVED %d", "TOTAL", totalR, totalU)
	return b.String()
}

// AssertUnresolvedResultTypeCensus checks that the population is REACHED.
//
// There is no zero to defend here and no floor on the unresolved count: RFC-213
// has not been implemented, so the unresolved reads are the DEFECT and their
// number is a measurement, not a contract. What must not happen silently is the
// consumers going dark — with no reads at all, a later "unresolved is now 0"
// would be indistinguishable from success.
func AssertUnresolvedResultTypeCensus(w io.Writer, minReads int) bool {
	res, unres := UnresolvedResultTypeCensus()
	total := 0
	for _, v := range res {
		total += v
	}
	for _, v := range unres {
		total += v
	}
	if total >= minReads {
		return false
	}
	fmt.Fprintf(w, "UNRESOLVED-RESULT-TYPE CENSUS FAIL: %d classified read(s), want >= %d.\n"+
		"  The consumers RFC-213 is denominated against have gone dark, so the unresolved\n"+
		"  count beside them is measuring an absence of TRAFFIC rather than an absence of\n"+
		"  the defect. Any later claim that the payoff shrank would be unreadable.\n"+
		"  census: %s\n", total, minReads, FormatUnresolvedResultTypeCensus())
	return true
}
