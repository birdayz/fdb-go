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
// clustered-box gather, over ten thousand times in one sqldriver run. It also has
// a READER, on a multi-leg merged row: over that same run its bindings are looked
// up six times, every one of them on a merged row carrying SIBLING legs. What
// those reads are not is LOAD-BEARING — neither removing the bindings entirely
// nor pointing every window at the wrong slot changes a single assertion.
//
// A claim like that decays the moment someone adds a load-bearing consumer, and
// it decays SILENTLY — the binder keeps working, so nothing goes red, and the
// next person reads the stale claim and reasons from it. So the claim is measured
// here rather than asserted in a comment: the reads are counted, attributed to
// the merged row they came out of, and excused one shape at a time by a proof
// that re-runs. A read this census cannot match to such a proof turns the gate
// red and says what gets re-armed.
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

// MergedRowRead identifies a read that resolved to a binder-produced window on a
// MULTI-LEG merged row: the alias that was read, AND the LAYOUT of the merged row
// it was read out of.
//
// The layout is carried because the alias alone is not an identity. Alias names
// COLLIDE across queries — the corpus binds twenty unrelated legs under the outer
// name `X` — which is why this census refuses to classify reads by alias. The same
// argument applies with full force to EXCUSING them: a proven-redundant entry keyed
// on the bare name `ST` excuses every future multi-leg read of anything called
// `ST`, including a load-bearing one in a query nobody has written yet. Keyed on
// (alias, shape) it excuses only what the proof actually covered.
type MergedRowRead struct {
	Alias string
	// Shape is mergedRowShape of the merged row the read's window looked into.
	Shape string
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
	mergedLegMergedRowReads = map[MergedRowRead]int{}
	// mergedLegUnshadowedMergedRowReads is the subset of mergedLegMergedRowReads
	// whose window shadowed NOTHING: the binder was the alias's only binding, so
	// the read had no other binding to fall back to.
	//
	// It is a DIAGNOSTIC TALLY, reported by FormatMergedLegBindingCensus. It is
	// deliberately NOT the activation gate's input, and the reason is a
	// measurement that refuted the guess this split was made on. The split was
	// built expecting the corpus's reads to be SHADOWING reads — an alias already
	// resolvable without the binder — so that "shadowed nothing" could stand in
	// for "the binder is the only route, therefore load-bearing". Measured over a
	// full sqldriver run: ZERO of the reads shadow, all six are UNSHADOWED. Wiring
	// this tally into the gate would therefore fire on every run while Baked=0 and
	// say nothing about consumer arrival.
	//
	// What the measurement actually establishes is that "shadowed nothing" does not
	// imply "load-bearing": the corpus's reads are the binder's alone AND declining
	// them changes no row (TestFDB_MergedLegBinding_ReaderShapeIsRedundant). So the
	// gate keys its exclusion on a PROOF per reader shape, not on this structural
	// property, and this tally stays as what it is — the number that shows the
	// structural property is not the one to key on.
	mergedLegUnshadowedMergedRowReads = map[MergedRowRead]int{}
	// mergedLegRedundantReaders names the (alias, merged-row shape) reads a live
	// test has DEMONSTRATED, in this run, to leave the rows unchanged when the
	// binder's window is declined — the reads that are therefore not evidence of a
	// load-bearing consumer.
	//
	// It is populated by proof, not by declaration. The activation criterion
	// excludes exactly this set, so the exclusion's license is the pin that filled
	// it: if the pin's proof fails it does not register, the set is empty, and the
	// same reads it used to excuse turn the gate red. There is no way to keep the
	// exclusion while losing the proof.
	mergedLegRedundantReaders = map[MergedRowRead]string{}
)

// RegisterRedundantMergedLegReader records that reads of ONE (alias, merged-row
// shape) were proven redundant — the same rows with the binder's window serving
// the read and with it declined — by the named proof. Call it ONLY from the
// passing path of that proof.
//
// It takes the read's full identity, not its alias, so the excusal covers exactly
// the shape the proof ran. A proof of `ST` out of an `ST[0,3)|OT[3,5)` merged row
// says nothing about `ST` out of some other merged row, and a registry keyed on
// the name would have said it anyway.
//
// why is the test's own identity, carried so the activation gate can name what
// is excusing a read rather than asserting it on the census's authority.
func RegisterRedundantMergedLegReader(read MergedRowRead, why string) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegRedundantReaders[read] = why
}

// RedundantMergedLegReaders returns a copy of the proven-redundant registry.
func RedundantMergedLegReaders() map[MergedRowRead]string {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	out := make(map[MergedRowRead]string, len(mergedLegRedundantReaders))
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
//
// mergedType is the merged row's own type, carried so the multi-leg read can be
// keyed by the LAYOUT it came out of and not by its alias alone.
func recordMergedLegRead(alias string, siblingLegs, shadowsExisting bool, mergedType *values.RecordType) {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	mergedLegReads[alias]++
	if !siblingLegs {
		return
	}
	read := MergedRowRead{Alias: alias, Shape: mergedRowShape(mergedType)}
	mergedLegMergedRowReads[read]++
	if !shadowsExisting {
		mergedLegUnshadowedMergedRowReads[read]++
	}
}

// MergedLegReadSink collects ONE execution's binder-window reads and declined
// lookups, in parallel with the process-global census.
//
// It exists because the global census cannot answer "what did THIS execution
// do?". Its tallies are process-wide and the suite is parallel, so a before/after
// delta around one execution also picks up whatever a concurrently running test
// read in the same window — and the redundancy pin's whole method is to compare
// two executions of one query by what each of them read. A delta that a sibling
// test can perturb makes that comparison intermittently wrong in the direction of
// a spurious red.
//
// The zero value is not usable; a nil *MergedLegReadSink is, and records nothing,
// so the read path can call through it unconditionally.
type MergedLegReadSink struct {
	mu       sync.Mutex
	reads    map[MergedRowRead]int
	misses   map[MergedRowRead]int
	handoffs map[MergedRowRead]int
}

// NewMergedLegReadSink returns an empty sink.
func NewMergedLegReadSink() *MergedLegReadSink {
	return &MergedLegReadSink{
		reads:    map[MergedRowRead]int{},
		misses:   map[MergedRowRead]int{},
		handoffs: map[MergedRowRead]int{},
	}
}

// recordRead mirrors recordMergedLegRead's MULTI-LEG population, which is the one
// the activation gate consults and therefore the one a proof has to speak about.
func (s *MergedLegReadSink) recordRead(alias string, siblingLegs bool, mergedType *values.RecordType) {
	if s == nil || !siblingLegs {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[MergedRowRead{Alias: alias, Shape: mergedRowShape(mergedType)}]++
}

// recordBypass mirrors recordMergedLegBypass.
func (s *MergedLegReadSink) recordBypass(alias string, shadowsExisting bool, mergedType *values.RecordType) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	read := MergedRowRead{Alias: alias, Shape: mergedRowShape(mergedType)}
	if shadowsExisting {
		s.handoffs[read]++
		return
	}
	s.misses[read]++
}

// Reads returns a copy of this execution's multi-leg binder-window reads.
func (s *MergedLegReadSink) Reads() map[MergedRowRead]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyReadTally(s.reads)
}

// BypassOutcomes returns copies of this execution's declined lookups: those that
// resolved to NOTHING, and those handed a binding the window had displaced.
func (s *MergedLegReadSink) BypassOutcomes() (misses, handoffs map[MergedRowRead]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyReadTally(s.misses), copyReadTally(s.handoffs)
}

// mergedRowShape renders a merged row's LEG LAYOUT — each leg's alias, its span,
// and the columns that span covers, in leg-table order — as the shape identity a
// read out of that row is attributed to.
//
// It is the finest identity available AT THE READ SITE. A correlation lookup
// carries no plan and no SQL, so two queries that build the identical merged
// layout are indistinguishable here and share one shape. What it does separate is
// a read of the same alias out of a DIFFERENT merged row: a different leg set,
// different spans, or different columns under the same span. That is the
// separation a bare-alias key folded away, and folding it away is what let one
// query's proof excuse another query's read.
//
// Built at READ time rather than stamped at bind time: the corpus binds ~15k
// windows per run and reads six of them, so the string is constructed where it is
// actually needed instead of once per outer row.
func mergedRowShape(t *values.RecordType) string {
	if t == nil {
		return "?"
	}
	var b strings.Builder
	for i, leg := range t.Legs {
		if i > 0 {
			b.WriteByte('|')
		}
		// An UNSTATED identity names nothing, but it still occupies a span, and a
		// layout that omitted it would equate two rows that differ.
		name := leg.Alias.Name()
		if leg.Alias.IsZero() {
			name = "?"
		}
		end := leg.Start + leg.Width
		fmt.Fprintf(&b, "%s[%d,%d)", name, leg.Start, end)
		if leg.Start < 0 || leg.Width < 0 || end > len(t.Fields) {
			// An out-of-bounds leg is a layout defect, not a shape; it renders as
			// its own distinct thing rather than silently as the in-bounds prefix.
			b.WriteString(":!")
			continue
		}
		for j := leg.Start; j < end; j++ {
			if j == leg.Start {
				b.WriteByte(':')
			} else {
				b.WriteByte(',')
			}
			b.WriteString(t.Fields[j].Name)
		}
	}
	return b.String()
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
// the reads that resolved to a window on a merged row carrying sibling legs,
// keyed by (alias, merged-row shape).
func MergedRowLegReads() map[MergedRowRead]int {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	return copyReadTally(mergedLegMergedRowReads)
}

// UnshadowedMergedRowLegReads returns a copy of the UNSHADOWED subset — multi-leg
// reads of a window that displaced nothing, so the binder was the alias's only
// binding.
//
// This is a DIAGNOSTIC, not the activation gate's input: over the sqldriver
// corpus every multi-leg read is unshadowed and none is load-bearing, so the
// structural property does not separate the alarm case from the quiet one. See
// mergedLegUnshadowedMergedRowReads for the measurement that settled it. The
// accessor exists so FormatMergedLegBindingCensus can report the number that
// makes the point, and so a future change of that number is visible rather than
// invisible.
func UnshadowedMergedRowLegReads() map[MergedRowRead]int {
	mergedLegMu.Lock()
	defer mergedLegMu.Unlock()
	return copyReadTally(mergedLegUnshadowedMergedRowReads)
}

// copyReadTally copies a read tally. Caller holds the mutex.
func copyReadTally(in map[MergedRowRead]int) map[MergedRowRead]int {
	out := make(map[MergedRowRead]int, len(in))
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
	mergedLegMergedRowReads = map[MergedRowRead]int{}
	mergedLegUnshadowedMergedRowReads = map[MergedRowRead]int{}
	mergedLegRedundantReaders = map[MergedRowRead]string{}
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
	// The multi-leg tallies are keyed by (alias, shape) and this line is per
	// ALIAS, so they are summed back over the shapes an alias was read out of —
	// and the shapes themselves are listed, because "read out of two different
	// merged layouts" is the thing the finer key exists to make visible.
	onMerged, unshadowed := map[string]int{}, map[string]int{}
	mergedShapes := map[string][]string{}
	for r, n := range MergedRowLegReads() {
		onMerged[r.Alias] += n
		mergedShapes[r.Alias] = append(mergedShapes[r.Alias], fmt.Sprintf("%s x%d", r.Shape, n))
	}
	for r, n := range UnshadowedMergedRowLegReads() {
		unshadowed[r.Alias] += n
	}
	readParts := make([]string, 0, len(reads))
	for a, n := range reads {
		totalReads += n
		if m := onMerged[a]; m > 0 {
			sort.Strings(mergedShapes[a])
			readParts = append(readParts, fmt.Sprintf("%s x%d (%d multi-leg, %d of those UNSHADOWED; out of %s)",
				a, n, m, unshadowed[a], strings.Join(mergedShapes[a], " + ")))
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
