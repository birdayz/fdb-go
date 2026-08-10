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

// ---- the GATE ------------------------------------------------------------
//
// The numbers this census produced were, until this gate existed, written down
// in exactly one place — a prose `why` string on the field-decision ratchet,
// which nothing parses. The census itself was PRINTED to stderr and not
// asserted, unlike the ~15 sibling censuses in the same TestMain block that flip
// the exit code. A regression straight back to the pre-fix population would have
// failed nothing. The measurement is the thing that has to survive, not the
// prose.

// AccessorPathGates are the populations this census refuses to let move
// silently. It is named Gates rather than Floors, as its siblings are, because
// the two arms guard OPPOSITE directions on different populations.
//
//   - MinTotal guards VACUITY, on the denominator. A ceiling over zero
//     observations passes perfectly while measuring nothing, and a green from an
//     empty set is this repo's dominant false positive. Nothing else here may be
//     read without it.
//
//   - MaxDeclineDotted is the GROWTH ceiling on the ratchet arm
//     (AccessorPathDeclineDotted), and growth is the ONLY alarm direction it has.
//
// THE DIRECTION HERE INVERTED, and the history is kept because a guard whose
// expected value moved is exactly the one that gets silently relaxed instead.
// The arm carried 4 declines: two RENDERED EXPLAIN LABELS (`q$N.AID#0`) minted by
// RecordQueryInMemorySortPlan.HintOrdering re-entering a display string as an
// identity, and `N.SK` — a genuinely NESTED path (struct column `n`, field `sk`,
// from `GROUP BY n.sk`) the resolver FUSED into one flat Field, which is the real
// `addr.city` versus `T.city` ambiguity this guard was written for. The rendered
// ones died with the advertiser fix. `N.SK` then died too, at its PRODUCER: a
// nested-path GROUP BY key is now rejected before it reaches the planner.
//
// So ZERO is the steady state, a collapse floor on this arm would be
// unsatisfiable, and the event to watch for is the arm coming BACK — by the lazy
// render returning, or by the nested-path GROUP BY rejection being relaxed
// without the resolver keeping the path segmented. The floor is not deleted so
// much as SUPERSEDED: MinTotal still catches the instrument dying, which is the
// thing a collapse floor on the arm would otherwise have been covering.
//
// A nil *AccessorPathGates is a no-op, which is the shape a narrowed run needs
// for MinTotal — a whole-corpus population claim. The ceiling is exact under any
// filter (a subset cannot exceed the whole), so it survives narrowing and is
// checked whenever gates is non-nil; MaxDeclineDotted nil leaves it unchecked.
type AccessorPathGates struct {
	MinTotal         int
	MaxDeclineDotted *int
}

// AssertAccessorPathCensus checks the process census against its gates.
func AssertAccessorPathCensus(w io.Writer, gates *AccessorPathGates) bool {
	return assertAccessorPathCensusState(w, AccessorPathCensus(), AccessorPathDottedWitnesses(), gates)
}

// assertAccessorPathCensusState is the whole decision over EXPLICIT state, so
// every arm can be driven by a unit test rather than only by whatever the corpus
// happens to reach. It reports whether the census failed, writing the reason to w.
func assertAccessorPathCensusState(w io.Writer, counts [accessorPathClassCount]int,
	witnesses map[string]int, gates *AccessorPathGates,
) bool {
	if gates == nil {
		return false
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	dotted := counts[AccessorPathDeclineDotted]
	failed := false

	if gates.MinTotal > 0 && total < gates.MinTotal {
		failed = true
		fmt.Fprintf(w, "ACCESSOR-PATH CENSUS FAIL: %d call(s) to AccessorNamePath, want >= %d.\n"+
			"  ALARM DIRECTION: vacuity. The instrument is dead — the census never ran, the corpus\n"+
			"  is empty, or the hook detached. Every count below is the absence of a POPULATION,\n"+
			"  not the absence of a shape, and the ceiling on the dotted arm passes over zero\n"+
			"  observations while measuring nothing.\n", total, gates.MinTotal)
	}
	if gates.MaxDeclineDotted != nil && dotted > *gates.MaxDeclineDotted {
		failed = true
		fmt.Fprintf(w, "ACCESSOR-PATH CENSUS FAIL: %d DECLINE-lazy-dotted, want <= %d.\n"+
			"  ALARM DIRECTION: growth, and it is the only direction this arm has — zero is the\n"+
			"  steady state, so there is no collapse floor beside this and none is missing.\n"+
			"  A lazy flat-dotted value is reaching the one match-domain column identity again,\n"+
			"  and every one of them is a comparison silently downgraded to a residual. Read the\n"+
			"  witnesses below; they say WHICH of the two retired producers came back:\n"+
			"  - a `#ordinal` in the name is a RENDERED EXPLAIN LABEL, which only a renderer emits.\n"+
			"    RecordQueryInMemorySortPlan.HintOrdering minted these from SortKey.Field while\n"+
			"    SortKey.ValueExpr held the baked identity; that arm advertises UNKNOWN now.\n"+
			"  - a plain dotted name (the old `N.SK`) is a genuinely NESTED path the resolver\n"+
			"    FUSED into one flat Field. Nested-path GROUP BY keys are rejected before the\n"+
			"    planner now, so this returning means that rejection was relaxed without the\n"+
			"    resolver being taught to keep the path segmented.\n"+
			"  Fix the PRODUCER either way — editing the guard re-arms the conflation it stops.\n",
			dotted, *gates.MaxDeclineDotted)
	}
	if failed {
		fmt.Fprintf(w, "  classes:")
		for i := AccessorPathClass(0); i < accessorPathClassCount; i++ {
			fmt.Fprintf(w, " %s=%d", i, counts[i])
		}
		fmt.Fprintf(w, " total=%d\n", total)
		names := make([]string, 0, len(witnesses))
		for n := range witnesses {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(w, "  dotted witness: %-40s x%d\n", n, witnesses[n])
		}
	}
	return failed
}
