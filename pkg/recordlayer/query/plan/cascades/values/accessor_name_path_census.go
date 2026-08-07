package values

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// The ACCESSOR-NAME-PATH census: what actually reaches the one match-domain
// column-identity function, and which of its arms carry traffic.
//
// AccessorNamePath is the sole notion of "are these two plan-time column
// references the same column" on the match side, and it is NAME-domain by
// construction — the candidate side is a []string with no ordinals (RFC-187
// §3.0). That makes every one of its arms a place where a DISPLAY name decides
// something, which is why one of them sits on the field-decision ratchet.
//
// THE ENTRY ON THE RATCHET DESCRIBES THIS FUNCTION AS "accessor path derived by
// splitting the name on dots". It does not split. The arm in question is the
// exact opposite — a guard that REFUSES to split a flat-dotted lazy name and
// declines the whole comparison, because `addr.city` (a real nested path) and
// `T.city` (an alias-qualified leaf) are indistinguishable as strings and
// splitting would mis-root the alias as an accessor. A decline is the
// conservative direction: the caller leaves the predicate a residual filter, so
// the rows and their order stay correct and only the plan is slower.
//
// So the question the entry cannot answer, and this census exists to answer, is
// whether that arm is REACHED AT ALL. The two possibilities have opposite
// consequences and are indistinguishable from the code:
//
//   - non-zero: flat-dotted lazy references really do arrive here, every one of
//     them is a comparison silently downgraded to a residual, and the fix is at
//     the PRODUCERS that mint them rather than at this guard.
//   - zero: the guard is unreachable, the ratchet entry is describing a
//     hypothetical, and what has to be pinned is whatever upstream property
//     keeps it empty — otherwise a future producer arms it with nothing
//     noticing.
//
// Java cannot reach either state, and that is the measure of the gap rather
// than a reason to relax: Java's FieldValue is resolved AT CONSTRUCTION
// (FieldValue.java:303-342 all route through resolveFieldPath at :273), so a
// lazy accessor does not exist, and accessor identity is the ORDINAL ALONE —
// ResolvedAccessor.equals compares getOrdinal() and nothing else (:684), and
// hashCode is Objects.hash(getOrdinal()) (:689). The name survives only for
// rendering (:431, :695). Every arm counted here is therefore an artifact of
// Go's lazy plan-time node, and the counts say how much of that artifact is
// live traffic rather than defensive prose.
//
// GATED by LegIdentityCensusEnabled, like every census on this path: disabled,
// the function pays one atomic load per call. Counts are per CALL, and the
// classes partition calls.

// AccessorPathClass is one call's outcome. These partition every call to
// AccessorNamePath.
type AccessorPathClass int

const (
	// AccessorPathOKAllBaked: a path was returned and EVERY accessor came from a
	// Resolved FieldPath. The names were read off resolved accessors, so an
	// ordinal identity existed for the whole path and the name domain was a
	// choice rather than a necessity. This is the population that RFC-187 §8
	// could convert without needing anything new.
	AccessorPathOKAllBaked AccessorPathClass = iota

	// AccessorPathOKHasLazy: a path was returned but at least one accessor came
	// from a LAZY node, where the name is the only identity that exists. This is
	// the population that cannot be converted without resolve-at-mint.
	AccessorPathOKHasLazy

	// AccessorPathDeclineDotted is THE RATCHET ARM: a lazy Field containing '.'.
	// Declined rather than split. See the header for why zero and non-zero mean
	// opposite things.
	AccessorPathDeclineDotted

	// AccessorPathDeclinePureOrdinal: a resolved accessor with no name at all
	// (Field == ""), so there is nothing to compare in the name domain. This is
	// the one decline that is a pure consequence of the match side being
	// name-based — the value HAS an identity, and it is the comparison that
	// cannot use it.
	AccessorPathDeclinePureOrdinal

	// AccessorPathDeclineEmptyName: a lazy node with no name and no resolution —
	// no identity of any kind.
	AccessorPathDeclineEmptyName

	// AccessorPathDeclineNotAColumn: the walk reached a root with no accessors,
	// so the value is not a column reference. The ordinary negative.
	AccessorPathDeclineNotAColumn

	accessorPathClassCount
)

func (c AccessorPathClass) String() string {
	switch c {
	case AccessorPathOKAllBaked:
		return "OK-all-baked"
	case AccessorPathOKHasLazy:
		return "OK-has-lazy"
	case AccessorPathDeclineDotted:
		return "DECLINE-lazy-dotted"
	case AccessorPathDeclinePureOrdinal:
		return "DECLINE-pure-ordinal"
	case AccessorPathDeclineEmptyName:
		return "DECLINE-empty-name"
	case AccessorPathDeclineNotAColumn:
		return "DECLINE-not-a-column"
	default:
		return "unknown"
	}
}

var (
	accessorPathMu     sync.Mutex
	accessorPathCounts [accessorPathClassCount]int
	// accessorPathDottedWitnesses records the distinct flat-dotted names that
	// reached the ratchet arm. The class count alone cannot name a producer, and
	// with a population this small the names ARE the investigation: three
	// declines out of 270k is a findable set of call sites, not a statistical
	// trend. Capped so a regression that arms this broadly cannot turn the census
	// into a memory leak.
	// accessorPathDottedOrigins keeps ONE stack per distinct witness. The name
	// alone says a display rendering became an identity; only the stack says WHO
	// rendered it, and with three occurrences guessing from grep is slower and
	// less certain than asking the program.
	accessorPathDottedOrigins   = map[string]struct{}{}
	accessorPathDottedWitnesses = map[string]int{}
)

// accessorPathWitnessCap bounds the distinct-witness set. Chosen to match the
// sibling censuses on this path rather than derived: the useful state of this
// set is "a handful", and anything approaching the cap is itself the finding.
const accessorPathWitnessCap = 64

// RecordAccessorPathCall counts one call's outcome. Callers must guard on
// LegIdentityCensusEnabled().
func RecordAccessorPathCall(class AccessorPathClass) {
	if class < 0 || class >= accessorPathClassCount {
		return
	}
	accessorPathMu.Lock()
	defer accessorPathMu.Unlock()
	accessorPathCounts[class]++
}

// AccessorPathCensus reports the class vector.
func AccessorPathCensus() [accessorPathClassCount]int {
	accessorPathMu.Lock()
	defer accessorPathMu.Unlock()
	return accessorPathCounts
}

// ResetAccessorPathCensus clears the counters.
func ResetAccessorPathCensus() {
	accessorPathMu.Lock()
	defer accessorPathMu.Unlock()
	accessorPathCounts = [accessorPathClassCount]int{}
	accessorPathDottedWitnesses = map[string]int{}
	accessorPathDottedOrigins = map[string]struct{}{}
}

// DumpAccessorPathCensus renders the class vector.
func DumpAccessorPathCensus(w io.Writer, label string) {
	c := AccessorPathCensus()
	total := 0
	for _, n := range c {
		total += n
	}
	fmt.Fprintf(w, "\n[%s] accessor-name-path census (per call to AccessorNamePath): total %d\n",
		label, total)
	for i := AccessorPathClass(0); i < accessorPathClassCount; i++ {
		fmt.Fprintf(w, "  %-22s %d\n", i, c[i])
	}
	if ws := AccessorPathDottedWitnesses(); len(ws) > 0 {
		names := make([]string, 0, len(ws))
		for n := range ws {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "  DECLINE-lazy-dotted witnesses (%d distinct):\n", len(names))
		for _, n := range names {
			fmt.Fprintf(w, "    %-40s x%d\n", n, ws[n])
		}
	}
	for _, o := range AccessorPathDottedOrigins() {
		fmt.Fprintf(w, "  origin:\n%s\n", o)
	}
}

// RecordAccessorPathDottedWitness records the flat-dotted NAME that tripped the
// ratchet arm, so the producer can be found rather than guessed at. Callers must
// guard on LegIdentityCensusEnabled().
//
// The name is exactly the string the guard refused to split, which is the whole
// point: it is the only artifact that distinguishes "a real nested path arrived
// lazy" from "a qualifier was concatenated onto a leaf".
func RecordAccessorPathDottedWitness(name string) {
	accessorPathMu.Lock()
	defer accessorPathMu.Unlock()
	if len(accessorPathDottedOrigins) < accessorPathWitnessCap {
		var buf [4096]byte
		accessorPathDottedOrigins[name+"\n"+accessorPathTrim(string(buf[:runtime.Stack(buf[:], false)]))] = struct{}{}
	}
	if len(accessorPathDottedWitnesses) >= accessorPathWitnessCap {
		if _, seen := accessorPathDottedWitnesses[name]; !seen {
			return
		}
	}
	accessorPathDottedWitnesses[name]++
}

// AccessorPathDottedWitnesses returns a copy of the witness set.
func AccessorPathDottedWitnesses() map[string]int {
	accessorPathMu.Lock()
	defer accessorPathMu.Unlock()
	out := make(map[string]int, len(accessorPathDottedWitnesses))
	for k, v := range accessorPathDottedWitnesses {
		out[k] = v
	}
	return out
}

// accessorPathTrim keeps the frames that name a producer and drops the census's
// own, so the printed origin starts at the code that built the value.
func accessorPathTrim(stack string) string {
	lines := strings.Split(stack, "\n")
	keep := make([]string, 0, 24)
	for _, ln := range lines {
		if strings.Contains(ln, "accessor_name_path") || strings.Contains(ln, "runtime.Stack") ||
			strings.HasPrefix(ln, "goroutine ") {
			continue
		}
		if strings.HasPrefix(ln, "\t") {
			keep = append(keep, "      "+strings.TrimSpace(ln))
		}
		if len(keep) >= 18 {
			break
		}
	}
	return strings.Join(keep, "\n")
}

// AccessorPathDottedOrigins returns the captured producer stacks.
func AccessorPathDottedOrigins() []string {
	accessorPathMu.Lock()
	defer accessorPathMu.Unlock()
	out := make([]string, 0, len(accessorPathDottedOrigins))
	for k := range accessorPathDottedOrigins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
