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
	// mergedLegMergedRowReads is the subset of mergedLegReads whose merged row
	// bound TWO OR MORE legs — sibling legs — as opposed to a merged row carrying
	// a single leg.
	//
	// The classification is STAMPED BY THE PRODUCER (legWindowRow.siblingLegs), not
	// reconstructed from the bind tallies by alias, because alias names COLLIDE
	// across queries — the corpus binds twenty unrelated legs under the outer name
	// `X` — so an alias-keyed reconstruction attributes one query's shape to
	// another query's read. Measured: it did.
	mergedLegMergedRowReads = map[string]int{}
	// mergedLegUnshadowedMergedRowReads is the ALARM population: a multi-leg read
	// of a window that shadowed NOTHING, so the binder was the alias's only
	// binding and the read had no other route.
	//
	// Splitting it out of mergedLegMergedRowReads is what makes the activation
	// criterion honest. The corpus's standing reads are all SHADOWING reads — the
	// alias was already resolvable, and neutering the binder leaves the answers
	// unchanged — so counting them as consumer-arrival would leave the gate
	// permanently red and say nothing. An UNSHADOWED multi-leg read is the real
	// event: the binder is the only route, so its correctness has become
	// load-bearing.
	//
	// The exclusion of shadowing reads is licensed by a PIN, not by this comment:
	// TestFDB_MergedLegBinding_ShadowedReadIsRedundant runs the corpus's reader
	// shape both ways and fails if the two routes ever disagree.
	mergedLegUnshadowedMergedRowReads = map[string]int{}
	// mergedLegRedundantReaders names the aliases a live test has DEMONSTRATED,
	// in this run, to resolve identically with and without the binder's window —
	// the reader shapes whose reads are therefore not evidence of a load-bearing
	// consumer.
	//
	// It is populated by proof, not by declaration. The activation criterion
	// excludes exactly this set, so the exclusion's license is the pin that filled
	// it: if the pin's two routes ever disagree it fails BEFORE registering, the
	// set is empty, and the same reads it used to excuse turn the gate red. There
	// is no way to keep the exclusion while losing the proof.
	mergedLegRedundantReaders = map[string]string{}
)

// RegisterRedundantMergedLegReader records that alias's binder-produced reads
// were proven redundant — identical rows with the binder's window active and
// with it bypassed — by the named proof. Call it ONLY from the passing path of
// that proof.
//
// why is the test's own identity, carried so the activation gate can name what
// is excusing a read rather than asserting it on the census's authority.
func RegisterRedundantMergedLegReader(alias, why string) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegRedundantReaders[alias] = why
}

// RedundantMergedLegReaders returns a copy of the proven-redundant registry.
func RedundantMergedLegReaders() map[string]string {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	out := make(map[string]string, len(mergedLegRedundantReaders))
	for k, v := range mergedLegRedundantReaders {
		out[k] = v
	}
	return out
}

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
//
// siblingLegs says whether the window's merged row carried two or more legs, and
// shadowsExisting whether it displaced a binding the incoming context already
// had. Both are passed through from the window rather than derived here, because
// the binder stamped them and the binder is the only site that held the row.
func recordMergedLegRead(alias string, siblingLegs, shadowsExisting bool) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegReads[alias]++
	if siblingLegs {
		mergedLegMergedRowReads[alias]++
		if !shadowsExisting {
			mergedLegUnshadowedMergedRowReads[alias]++
		}
	}
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

// MergedRowLegReads returns a copy of the MULTI-LEG subset of the read tally —
// the reads that resolved to a window on a merged row carrying sibling legs.
func MergedRowLegReads() map[string]int {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	return copyReadTally(mergedLegMergedRowReads)
}

// UnshadowedMergedRowLegReads returns a copy of the ALARM subset — multi-leg
// reads of a window that shadowed nothing, so the binder was the alias's only
// binding.
//
// Separate accessor rather than a return value of MergedLegBindingCensus because
// it answers a different question and has a different consumer: the activation
// gate, which alarms on a read the binder alone could serve while no leg-local
// bake produced anything.
func UnshadowedMergedRowLegReads() map[string]int {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	return copyReadTally(mergedLegUnshadowedMergedRowReads)
}

// copyReadTally copies a read tally. Caller holds the mutex.
func copyReadTally(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ResetMergedLegBindingCensus clears all tallies.
func ResetMergedLegBindingCensus() {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegBinds = map[MergedLegBinding]int{}
	mergedLegReads = map[string]int{}
	mergedLegMergedRowReads = map[string]int{}
	mergedLegUnshadowedMergedRowReads = map[string]int{}
	mergedLegRedundantReaders = map[string]string{}
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
	onMerged := MergedRowLegReads()
	unshadowed := UnshadowedMergedRowLegReads()
	readParts := make([]string, 0, len(reads))
	for a, n := range reads {
		totalReads += n
		if m := onMerged[a]; m > 0 {
			readParts = append(readParts, fmt.Sprintf("%s x%d (%d multi-leg, %d of those UNSHADOWED)",
				a, n, m, unshadowed[a]))
			continue
		}
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
