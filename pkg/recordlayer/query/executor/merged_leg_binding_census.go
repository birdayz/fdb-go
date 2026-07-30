package executor

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The MERGED-LEG BINDING census: what bindMergedOuterLegs PRODUCED, and whether
// anything READ it.
//
// It exists because those two are different questions and the answer differs.
// The binder was documented as having "no non-test consumer", which read as
// dormant. It is not dormant — it executes on the real-FDB corpus on every
// clustered-box gather, over ten thousand times in one sqldriver run. What it
// does NOT have is a reader: over that same run its bindings are looked up three
// times, none on a box-gather shape, and neither removing the bindings entirely
// nor pointing every window at the wrong slot changes a single assertion.
//
// A claim like that decays the moment someone adds a consumer, and it decays
// SILENTLY — the binder keeps working, so nothing goes red, and the next person
// reads the stale "nothing reads this" and reasons from it. So the claim is
// measured here rather than asserted in a comment, and the shape that reads zero
// is pinned by a test that says what gets re-armed when it stops being zero.
//
// It is GATED on the same flag as the leg-identity census
// (values.LegIdentityCensusEnabled): the read side sits in
// EvaluationContext.GetCorrelationBinding, which is per-reference-per-row, and
// production must not pay a type assertion there.
type MergedLegBinding struct {
	// OuterAlias is the FlatMap's own alias — the join whose merged row is being
	// decomposed. It is recorded because the join's-own-alias skip is decided
	// against it, and on the live shapes it never engages (the box's alias is not
	// any leg's alias), which is exactly the case the unit fixtures did not cover.
	OuterAlias string
	LegAlias   string
	Offset     int
	Width      int
}

var (
	mergedLegMu    sync.Mutex
	mergedLegBinds = map[MergedLegBinding]int{}
	mergedLegReads = map[string]int{}
)

// recordMergedLegBindings counts the leg windows a FINISHED bindMergedOuterLegs
// context actually carries, for the aliases it claimed.
//
// It reads them out of the produced context rather than taking the binder's word
// for what it decided, so a binder that decides correctly and then returns the
// UNMODIFIED context records nothing — which is the truth about what downstream
// will find. The distinction is not hypothetical: recording the decision instead
// let exactly that mutation pass.
//
// The lookup goes to the bindings map directly, NOT through
// GetCorrelationBinding, because that entry point is the READ counter and a
// census that consulted it would manufacture the reads it exists to count.
func recordMergedLegBindings(ec *EvaluationContext, outerAlias values.CorrelationIdentifier, claimed []values.CorrelationIdentifier) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	for _, alias := range claimed {
		w, isWindow := ec.bindings[alias].(*legWindowRow)
		if !isWindow || w == nil || !w.fromMergedBinder {
			continue
		}
		mergedLegBinds[MergedLegBinding{
			OuterAlias: outerAlias.Name(),
			LegAlias:   alias.Name(),
			Offset:     w.offset,
			Width:      w.width,
		}]++
	}
}

// recordMergedLegRead counts one LOOKUP that resolved to a window
// bindMergedOuterLegs produced. This is the number the "nothing reads it" claim
// rests on, so it is counted at the binding lookup itself rather than inferred
// from the shape of the plan.
func recordMergedLegRead(alias string) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegReads[alias]++
}

// MergedLegBindingCensus returns copies of the bind and read tallies.
func MergedLegBindingCensus() (map[MergedLegBinding]int, map[string]int) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	binds := make(map[MergedLegBinding]int, len(mergedLegBinds))
	for k, v := range mergedLegBinds {
		binds[k] = v
	}
	reads := make(map[string]int, len(mergedLegReads))
	for k, v := range mergedLegReads {
		reads[k] = v
	}
	return binds, reads
}

// ResetMergedLegBindingCensus clears both tallies.
func ResetMergedLegBindingCensus() {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegBinds = map[MergedLegBinding]int{}
	mergedLegReads = map[string]int{}
}

// FormatMergedLegBindingCensus renders the census for a harness to log.
//
// The headline is the RATIO: binds are what the path costs, reads are what it
// buys. Reporting only the bind count would read as evidence the binder is
// carrying the corpus, which is the misreading this census was built to correct.
func FormatMergedLegBindingCensus() string {
	binds, reads := MergedLegBindingCensus()
	var totalBinds, totalReads int
	shapes := make([]string, 0, len(binds))
	for b, n := range binds {
		totalBinds += n
		shapes = append(shapes, fmt.Sprintf("%s<-%s[%d,%d) x%d",
			b.OuterAlias, b.LegAlias, b.Offset, b.Offset+b.Width, n))
	}
	readParts := make([]string, 0, len(reads))
	for a, n := range reads {
		totalReads += n
		readParts = append(readParts, fmt.Sprintf("%s x%d", a, n))
	}
	sort.Strings(shapes)
	sort.Strings(readParts)

	var b strings.Builder
	fmt.Fprintf(&b, "merged-leg binding: %d windows bound over %d distinct shapes; %d READ",
		totalBinds, len(binds), totalReads)
	if len(readParts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(readParts, ", "))
	}
	if len(shapes) > 0 {
		fmt.Fprintf(&b, "\n  shapes:\n    %s", strings.Join(shapes, "\n    "))
	}
	return b.String()
}
