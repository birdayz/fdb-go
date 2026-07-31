package cascades

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The foldStep1Seed OUTCOME census.
//
// It answers one question per firing of the 3-quantifier join+EXISTS arm: did
// step 1 get an ORDINAL SEED, and when it did not, WHY not. The four decline
// classes and the accept partition every invocation, and the reconstruct-nil
// class is sub-classified by the SHAPE of the leg the layout authority refused —
// which is the cut that separates "a leg Go cannot position" from "a leg Go
// BUILDS as a positional merge and then declines to position".
//
// IT REPLACES A PROBE THAT WAS DELETED. The 108/77/77/200/78 breakdown this
// file's floors and RFC-200's gates are stated against came from a call-site
// probe added for one question and removed after, so the numbers were a DATED
// POINT MEASUREMENT with nothing keeping them true. A number asserted in prose
// and checked by nothing is the failure this census family exists to prevent.
//
// THE DENOMINATOR IS COUNTED INDEPENDENTLY, at implementJoinWithExistential's
// seed-decision call site, and this is the census's one structural defence.
// Summing the classes and calling that the denominator is true BY CONSTRUCTION
// and gates nothing: it cannot see a class arm added without a counter, and it
// cannot see an early return that skips recording. An independent counter turns
// both into a red partition assertion.
//
// GATED by values.LegIdentityCensusEnabled, like every census on this path.
// Totals count RULE FIRINGS, not queries — the memo may explore one rule once
// or many times per query.

const foldStep1WitnessCap = 128

// foldStep1Class is one firing's outcome. The classes partition the
// independently counted denominator.
type foldStep1Class int

const (
	// foldStep1ClassNone is the ZERO value and means "no foldStep1Seed decision
	// governs this firing". It is never recorded by the census itself; it exists
	// so the BURIED rebase site — which has no step-1 seed decision at all — can
	// thread an honest value instead of a misfiled one, and so that a new EXISTS
	// caller that forgets to thread the class shows up as a partition failure
	// rather than being silently counted as an accept.
	foldStep1ClassNone foldStep1Class = iota

	// foldStep1Accept: the seed was built and both layout twins accepted it.
	foldStep1Accept

	// foldStep1DeclineCorrelatedStep1: condition (1) — a correlated or
	// null-extended step 1, where a baked seed hits the loud
	// values.BakedNameContextError. PERMANENT and out of scope by a prior
	// architecture ruling: a WHERE-EXISTS correlating into a leg buried in an
	// inner join is the canonical semijoin, whose good plan is a SARG'd
	// correlated index scan under the FlatMap, and that requires NAME binding to
	// flow the sibling comparand into the index.
	foldStep1DeclineCorrelatedStep1

	// foldStep1DeclineNoExistRef: condition (2) — the result value does not
	// reference the existential quantifier, so the projection sits ABOVE the
	// existential level. A correct pass-through, not a residue: there is nothing
	// to fold, and folding is what the seed is for.
	foldStep1DeclineNoExistRef

	// foldStep1DeclineReconstructNil: condition (3) — reconstructFoldStep1Seed
	// returned nil. Sub-classified by the declined leg's shape below; that
	// sub-classification is the whole reason this census exists.
	foldStep1DeclineReconstructNil

	// foldStep1DeclineWindowsNil: a seed WAS reconstructed and the layout
	// authority then refused it. Measured at zero, and it is its own class rather
	// than being folded into reconstruct-nil precisely so the zero is visible: a
	// builder and its validator disagreeing is a different defect from a leg the
	// builder could not walk, and the two must not print as one number.
	foldStep1DeclineWindowsNil

	foldStep1ClassCount
)

func (c foldStep1Class) String() string {
	switch c {
	case foldStep1ClassNone:
		return "NONE(no step-1 seed decision)"
	case foldStep1Accept:
		return "ACCEPT"
	case foldStep1DeclineCorrelatedStep1:
		return "DECLINE correlatedStep1"
	case foldStep1DeclineNoExistRef:
		return "DECLINE rv-no-exist-ref"
	case foldStep1DeclineReconstructNil:
		return "DECLINE reconstruct-nil"
	case foldStep1DeclineWindowsNil:
		return "DECLINE windows-nil"
	}
	return "unknown"
}

// foldStep1LegShape sub-classifies a reconstruct-nil firing by what the leg the
// walk refused actually IS.
//
// The distinction the whole of RFC-200 turns on lives here: a leg whose result
// value is a bare QuantifiedObjectValue states no row layout at all, while a leg
// whose result value satisfies values.IsPositionalMergeRC is Java's own merged
// row (PartitionSelectRule.java:283-291's
// RecordConstructorValue.ofColumns(... Column::unnamedOf)) — built by Go's own
// positional merge and then declined by Go's own layout authority.
type foldStep1LegShape int

const (
	// foldStep1LegShapeNone: no leg was ordinal-UNSAFE, so the nil came from
	// further down — planBuriedLegConcat refusing a leg, an empty concat, or an
	// ofOrdinal error. A separate residue with a separate fix.
	foldStep1LegShapeNone foldStep1LegShape = iota

	// foldStep1LegShapeBareQOV: the declined node's result value is a bare
	// *values.QuantifiedObjectValue — the identity pass-through. The LARGER
	// residue, and explicitly out of RFC-200's scope (§Residues).
	foldStep1LegShapeBareQOV

	// foldStep1LegShapePositionalMerge: the declined node's result value is a
	// RecordConstructorValue satisfying values.IsPositionalMergeRC. RFC-200's
	// target population.
	foldStep1LegShapePositionalMerge

	// foldStep1LegShapeRCNotMerge: a RecordConstructorValue that is NOT a
	// positional merge. Its own bucket because the two are shape-adjacent and an
	// RFC-200 gate is stated as an EXACT equality against the merge count — a
	// bucket that lumped them would let one drift into the other with the gate
	// still green.
	foldStep1LegShapeRCNotMerge

	// foldStep1LegShapeOther: a declined node the three above do not describe (a
	// non-inner nested-loop join, an aggregate, a union).
	foldStep1LegShapeOther

	foldStep1LegShapeCount
)

func (s foldStep1LegShape) String() string {
	switch s {
	case foldStep1LegShapeNone:
		return "no-unsafe-leg(nil from below legIsOrdinalSafe)"
	case foldStep1LegShapeBareQOV:
		return "rv=bare QuantifiedObjectValue"
	case foldStep1LegShapePositionalMerge:
		return "rv=POSITIONAL-MERGE RC"
	case foldStep1LegShapeRCNotMerge:
		return "rv=RecordConstructorValue (NOT a positional merge)"
	case foldStep1LegShapeOther:
		return "rv=other"
	}
	return "unknown"
}

// foldStep1SeedCounters is the census's state.
type foldStep1SeedCounters struct {
	// Denominator counts invocations at implementJoinWithExistential's
	// seed-decision call site, INDEPENDENTLY of the classes. See the header.
	Denominator int

	// Class partitions the firings foldStep1Seed itself recorded. Its sum must
	// equal Denominator; a gap is a class arm with no counter or an early return
	// that skipped recording.
	Class [foldStep1ClassCount]int

	// ReconstructNilLegShape sub-partitions Class[foldStep1DeclineReconstructNil]
	// by the refused leg's shape. One entry per FIRING (the FIRST refused leg),
	// so it sums to the reconstruct-nil class exactly.
	ReconstructNilLegShape [foldStep1LegShapeCount]int

	// ReconstructNilBothLegsUnsafe counts firings where BOTH legs were
	// ordinal-unsafe.
	//
	// It exists because the sub-partition above records the FIRST refused leg and
	// that is only an honest summary while at most one leg per firing is refused.
	// The removed probe's own breakdown asserted exactly that ("left=FlatMap
	// right=Scan" and "left=Scan right=FlatMap", never both) — an assertion no
	// instrument was checking. This counter checks it.
	ReconstructNilBothLegsUnsafe int
}

var (
	foldStep1Mu        sync.Mutex
	foldStep1Counts    foldStep1SeedCounters
	foldStep1Witnesses map[string]int
)

// recordFoldStep1Denominator counts ONE seed decision, at the call site, before
// the decision runs. Callers must guard on values.LegIdentityCensusEnabled().
func recordFoldStep1Denominator() {
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	foldStep1Counts.Denominator++
}

// recordFoldStep1Outcome counts ONE classified firing. Callers must guard on
// values.LegIdentityCensusEnabled().
func recordFoldStep1Outcome(class foldStep1Class, decline foldStep1LegDecline) {
	if class <= foldStep1ClassNone || class >= foldStep1ClassCount {
		return
	}
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	foldStep1Counts.Class[class]++
	if class != foldStep1DeclineReconstructNil {
		return
	}
	shape := decline.Shape
	if shape < 0 || shape >= foldStep1LegShapeCount {
		shape = foldStep1LegShapeOther
	}
	foldStep1Counts.ReconstructNilLegShape[shape]++
	if decline.BothLegsUnsafe {
		foldStep1Counts.ReconstructNilBothLegsUnsafe++
	}
	if decline.Witness != "" {
		if foldStep1Witnesses == nil {
			foldStep1Witnesses = map[string]int{}
		}
		if _, known := foldStep1Witnesses[decline.Witness]; known || len(foldStep1Witnesses) < foldStep1WitnessCap {
			foldStep1Witnesses[decline.Witness]++
		}
	}
}

// FoldStep1SeedCensus reports the counters and the distinct decline witnesses.
func FoldStep1SeedCensus() (foldStep1SeedCounters, []string) {
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	out := make([]string, 0, len(foldStep1Witnesses))
	for w, n := range foldStep1Witnesses {
		out = append(out, fmt.Sprintf("x%d %s", n, w))
	}
	sort.Strings(out)
	return foldStep1Counts, out
}

// ResetFoldStep1SeedCensus clears the counters.
func ResetFoldStep1SeedCensus() {
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	foldStep1Counts = foldStep1SeedCounters{}
	foldStep1Witnesses = nil
}

// FormatFoldStep1SeedCensus renders the census for a harness to log.
func FormatFoldStep1SeedCensus() string {
	c, witnesses := FoldStep1SeedCensus()
	var b strings.Builder
	b.WriteString("foldStep1Seed outcomes (per rule firing):")
	for cl := foldStep1Class(1); cl < foldStep1ClassCount; cl++ {
		fmt.Fprintf(&b, "\n  %-32s %d", cl, c.Class[cl])
	}
	fmt.Fprintf(&b, "\n  %-32s %d (counted independently at the call site)", "TOTAL", c.Denominator)
	b.WriteString("\n  reconstruct-nil by refused leg shape:")
	for s := foldStep1LegShape(0); s < foldStep1LegShapeCount; s++ {
		fmt.Fprintf(&b, "\n    %-46s %d", s, c.ReconstructNilLegShape[s])
	}
	fmt.Fprintf(&b, "\n    %-46s %d", "firings with BOTH legs unsafe", c.ReconstructNilBothLegsUnsafe)
	for _, w := range witnesses {
		fmt.Fprintf(&b, "\n    witness %s", w)
	}
	return b.String()
}

// FoldStep1SeedGates is the EXACT-equality gate set RFC-200 §Acceptance states.
//
// Equalities, not floors, and the difference is the point: these are
// PREDICTIONS. A measured deviation is itself a reportable finding and must be
// reported rather than absorbed by relaxing the assertion.
//
// A nil field is not gated — that is how a NARROWED run (-test.run) keeps the
// structural checks (the partition, the both-legs-unsafe zero) while dropping
// the population equalities that only hold over the whole corpus.
type FoldStep1SeedGates struct {
	Denominator           *int
	Accept                *int
	CorrelatedStep1       *int
	NoExistRef            *int
	ReconstructNil        *int
	ReconstructNilBareQOV *int
	ReconstructNilMerge   *int
}

// AssertFoldStep1SeedCensus checks the partition, the structural zeros, and any
// stated equalities.
func AssertFoldStep1SeedCensus(w io.Writer, gates *FoldStep1SeedGates) bool {
	c, witnesses := FoldStep1SeedCensus()
	return assertFoldStep1SeedCounters(w, c, witnesses, gates)
}

func assertFoldStep1SeedCounters(w io.Writer, c foldStep1SeedCounters, witnesses []string, gates *FoldStep1SeedGates) bool {
	failed := false
	classSum := 0
	for cl := foldStep1Class(1); cl < foldStep1ClassCount; cl++ {
		classSum += c.Class[cl]
	}
	if classSum != c.Denominator {
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: the classes sum to %d but the INDEPENDENT\n"+
			"  denominator counted %d.\n"+
			"  The denominator is counted at implementJoinWithExistential's seed-decision\n"+
			"  call site and the classes are counted inside foldStep1Seed, deliberately, so\n"+
			"  that exactly this gap is visible. A gap means either a decision arm was added\n"+
			"  without a counter, or an early return skips recording — and in both cases every\n"+
			"  population number below is measuring a subset of the traffic while printing as\n"+
			"  if it measured all of it.\n  census: %s\n", classSum, c.Denominator, FormatFoldStep1SeedCensus())
	}
	shapeSum := 0
	for s := foldStep1LegShape(0); s < foldStep1LegShapeCount; s++ {
		shapeSum += c.ReconstructNilLegShape[s]
	}
	if shapeSum != c.Class[foldStep1DeclineReconstructNil] {
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: the refused-leg shapes sum to %d but the\n"+
			"  reconstruct-nil class counted %d. The sub-partition is recorded from the same\n"+
			"  call as the class, so a gap is a shape arm that returns before recording.\n",
			shapeSum, c.Class[foldStep1DeclineReconstructNil])
	}
	if c.ReconstructNilBothLegsUnsafe != 0 {
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: %d reconstruct-nil firing(s) had BOTH legs\n"+
			"  ordinal-unsafe, want 0.\n"+
			"  The refused-leg sub-partition records the FIRST refused leg, which is an honest\n"+
			"  summary only while at most one leg per firing is refused. That premise came from\n"+
			"  a removed probe's own breakdown (left=FlatMap/right=Scan and its mirror, never\n"+
			"  both) and nothing was checking it.\n"+
			"  WHAT A NON-ZERO RE-ARMS: every population equality below is now counting one leg\n"+
			"  of a firing that had two, so the shapes no longer describe the traffic. Split the\n"+
			"  sub-partition per LEG before reading any of them again.\n"+
			"  witnesses: %v\n", c.ReconstructNilBothLegsUnsafe, witnesses)
	}
	if gates == nil {
		return failed
	}
	type eq struct {
		name string
		want *int
		got  int
	}
	for _, e := range []eq{
		{"denominator (counted at the call site)", gates.Denominator, c.Denominator},
		{"ACCEPT", gates.Accept, c.Class[foldStep1Accept]},
		{"DECLINE correlatedStep1", gates.CorrelatedStep1, c.Class[foldStep1DeclineCorrelatedStep1]},
		{"DECLINE rv-no-exist-ref", gates.NoExistRef, c.Class[foldStep1DeclineNoExistRef]},
		{"DECLINE reconstruct-nil", gates.ReconstructNil, c.Class[foldStep1DeclineReconstructNil]},
		{"reconstruct-nil / bare-QOV leg", gates.ReconstructNilBareQOV, c.ReconstructNilLegShape[foldStep1LegShapeBareQOV]},
		{"reconstruct-nil / positional-merge leg", gates.ReconstructNilMerge, c.ReconstructNilLegShape[foldStep1LegShapePositionalMerge]},
	} {
		if e.want == nil || *e.want == e.got {
			continue
		}
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: %s == %d, want EXACTLY %d.\n"+
			"  This is an EQUALITY, not a floor, and the deviation is itself the finding —\n"+
			"  do NOT relax it into a range. Either the corpus moved (say by how much and\n"+
			"  why), or a decision this population rests on changed shape.\n"+
			"  census: %s\n", e.name, e.got, *e.want, FormatFoldStep1SeedCensus())
	}
	return failed
}

// foldStep1LegDecline is WHY reconstructFoldStep1Seed returned nil, stated by
// the walk that decided it rather than re-derived afterwards.
//
// It is returned from the reconstruction rather than recomputed by the census
// for the reason every census on this path states about its own inputs: a second
// walk of the same plan is a second copy of the rule, and two copies of a rule
// agree until one of them is edited. legOrdinalSafety hands back the node it
// stopped at, so the classification below reads the SAME node the decision was
// made at.
type foldStep1LegDecline struct {
	Shape          foldStep1LegShape
	BothLegsUnsafe bool
	Witness        string
}

// classifyDeclinedLeg describes the node legOrdinalSafety refused.
//
// The result-value shapes it separates are the ones RFC-200's gates are stated
// against; anything else is Other with a witness naming the plan type, so a new
// population arrives as a named witness rather than as movement in a bucket.
func classifyDeclinedLeg(node plans.RecordQueryPlan) (foldStep1LegShape, string) {
	if node == nil {
		return foldStep1LegShapeNone, ""
	}
	rvp, hasRV := node.(interface{ GetResultValue() values.Value })
	if !hasRV {
		return foldStep1LegShapeOther, fmt.Sprintf("%T (no result value)", node)
	}
	rv := rvp.GetResultValue()
	switch {
	case rv == nil:
		return foldStep1LegShapeOther, fmt.Sprintf("%T rv=nil", node)
	case values.IsPositionalMergeRC(rv):
		rc := rv.(*values.RecordConstructorValue)
		return foldStep1LegShapePositionalMerge, fmt.Sprintf("%T rv=positional-merge RC(%d)", node, len(rc.Fields))
	}
	switch t := rv.(type) {
	case *values.QuantifiedObjectValue:
		return foldStep1LegShapeBareQOV, fmt.Sprintf("%T rv=bare QOV (typed=%t)", node, t.Typ != nil)
	case *values.RecordConstructorValue:
		return foldStep1LegShapeRCNotMerge, fmt.Sprintf("%T rv=RC(%d) NOT a positional merge", node, len(t.Fields))
	default:
		return foldStep1LegShapeOther, fmt.Sprintf("%T rv=%T", node, rv)
	}
}
