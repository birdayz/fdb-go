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

	// DeclinedStep1RVIsMerge counts firings that DECLINED and whose step1RV — the
	// RAW sel.GetResultValue(), handed back unchanged — is itself a positional
	// merge. HARD ZERO.
	//
	// It exists because implementJoinWithExistential's nested-entry opt-in reads
	// step1RV on EVERY class, including the correlated wall. RFC-200 §8's guard
	// chain — correlatedStep1 short-circuits foldStep1Seed before
	// reconstructFoldStep1Seed runs — covers the RECONSTRUCTION, not this read. So
	// if a declined firing's raw result value were a merge row, the nested entry
	// would flip ordinalWindows from nil to NON-nil on an arm nothing analysed,
	// and the correlated arm is exactly where a baked ordinal against a name-keyed
	// row context raises values.BakedNameContextError. That arm carries a
	// two-revert history.
	//
	// Gating the call on gatedSeedStep1 instead would be wrong: step1RV is
	// legitimately a PRISTINE ordinal seed on some firings where gatedSeedStep1 is
	// false (an earlier box dissolution baked it), and those must keep their
	// windows. The hazard is specifically a MERGE row, so that is what is zeroed.
	DeclinedStep1RVIsMerge int

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
// recordFoldStep1DeclinedMergeRV counts a DECLINED firing whose step1RV is
// itself a positional merge. Callers must guard on
// values.LegIdentityCensusEnabled().
func recordFoldStep1DeclinedMergeRV() {
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	foldStep1Counts.DeclinedStep1RVIsMerge++
}

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
	if c.DeclinedStep1RVIsMerge != 0 {
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: %d DECLINED firing(s) carried a step1RV\n"+
			"  that is itself a POSITIONAL MERGE, want 0.\n"+
			"  implementJoinWithExistential reads step1RV through the NESTED entry on\n"+
			"  every class, including the correlated wall. RFC-200 §8's guard chain covers\n"+
			"  the seed RECONSTRUCTION, not that read — so a declined firing whose raw\n"+
			"  result value is a merge row flips ordinalWindows from nil to NON-nil on an\n"+
			"  arm nothing analysed.\n"+
			"  WHAT A NON-ZERO RE-ARMS: on the correlated arm a baked ordinal against a\n"+
			"  name-keyed row context raises values.BakedNameContextError, and that arm has\n"+
			"  a two-revert history. Do NOT simply gate the call on gatedSeedStep1: a\n"+
			"  PRISTINE ordinal seed legitimately reaches that site with gatedSeedStep1\n"+
			"  false and must keep its windows. Find the producer of the merge-shaped\n"+
			"  result value instead.\n", c.DeclinedStep1RVIsMerge)
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

// quantifiedObjectValueIsTyped reports whether a QOV carries a REAL flowed type,
// as opposed to the UnknownType placeholder that stands for "nobody typed this".
//
// It exists because the obvious spelling is a tautology. `Typ != nil` is TRUE for
// every QOV that can be constructed: NewQuantifiedObjectValue stamps
// `Typ: UnknownType` and NewQuantifiedObjectValueOfType degrades a nil argument to
// the same, and UnknownType is a non-nil Type (a `*PrimitiveType` with
// TypeCodeUnknown). So a nil check answers a question nothing can make false, and
// the witness that was supposed to separate the untyped population from the typed
// one printed `typed=true` for all of it.
//
// That matters beyond a cosmetic log line. This witness is the instrument the
// bare-untyped-QOV residue is measured with — the population Java cannot even
// express. Both of QuantifiedObjectValue's factory overloads resolve a Type:
// of(alias, Type) takes one outright (QuantifiedObjectValue.java:187) and
// of(Quantifier) derives it (`:182`) through Quantifier.getFlowedObjectType,
// which is a Verify.verify plus requireNonNull (Quantifier.java:805-810). An
// instrument that reports the target
// state before any work is done would have reported that sweep complete on the day
// it started, with the whole population untouched underneath.
//
// The discriminator is the type CODE, not pointer identity against the UnknownType
// singleton: an equivalent unknown built elsewhere is just as untyped, and keying on
// the shared variable would call it typed.
func quantifiedObjectValueIsTyped(qov *values.QuantifiedObjectValue) bool {
	return qov.Typ != nil && qov.Typ.Code() != values.TypeCodeUnknown
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
		return foldStep1LegShapeBareQOV, fmt.Sprintf("%T rv=bare QOV (typed=%t)", node, quantifiedObjectValueIsTyped(t))
	case *values.RecordConstructorValue:
		return foldStep1LegShapeRCNotMerge, fmt.Sprintf("%T rv=RC(%d) NOT a positional merge", node, len(t.Fields))
	default:
		return foldStep1LegShapeOther, fmt.Sprintf("%T rv=%T", node, rv)
	}
}

// The ORIENTATION-GATE census (RFC-200 step 3d').
//
// It exists because the fix's expected side effect did not appear. Replacing
// materializedNLJOrdinalLayoutMatches' map-count gate with the top-level RUN
// LIST closes a pre-existing fail-open, and RFC-200 says that "CHANGES BEHAVIOUR
// TODAY for box-leg seeds: they stop failing open and start being checked, which
// can DECLINE plans currently yielded". Over the whole suite, nothing moved — no
// golden, no plandiff, no row.
//
// "Nothing moved" has two very different causes and a printed zero cannot tell
// them apart: either no seed reaches the gate in a shape the old count skipped,
// or they all reach it correctly oriented and the check passes. The first means
// the fix is latent; the second means it is live and agreeing. This census
// separates them, so the null result is a measurement rather than an absence.
type orientationGateCounters struct {
	// Calls is every invocation of the gate.
	Calls int
	// NotASeed: the result value yields no run list at all — the overwhelming
	// majority, and not this check's concern under either gate.
	NotASeed int
	// TiledByTwo: exactly two legs tile the row, so the check is ANSWERABLE.
	TiledByTwo int
	// TiledByOther: some other number of tiles — genuinely outside a
	// 2-quantifier join's scope, and skipped under both gates.
	TiledByOther int
	// MapCountDiffers counts the firings where the MAP count and the TILE count
	// disagree. This is the whole population the fix moves: under the old gate
	// these skipped the check, under the new one they are checked.
	//
	// A ZERO here means the fix is LATENT — correct, but no corpus shape reaches
	// it — and that is a different claim from "the fix is live and everything
	// passes". Nothing else can distinguish them.
	MapCountDiffers int
	// Unverifiable: two tiles, but a leg plan cannot state its row type, so the
	// structural comparison has nothing to compare. Skipped under both gates —
	// a SECOND, separate fail-open that this change does not address.
	Unverifiable int
	// Matched / Declined partition the firings the check actually decided.
	Matched  int
	Declined int
	// DeclinedNewlyChecked is the CROSS of Declined with MapCountDiffers: a
	// decline on a firing the OLD map-count gate would have skipped.
	//
	// This is the number 3d' is actually accountable for. Declined alone mixes
	// firings the old gate already checked and already declined (no change) with
	// the ones it waved through (the change), and only the second kind can move a
	// plan.
	DeclinedNewlyChecked int
}

var (
	orientationGateMu     sync.Mutex
	orientationGateCounts orientationGateCounters
)

// recordOrientationGate counts one firing of the orientation check. Callers must
// guard on values.LegIdentityCensusEnabled().
func recordOrientationGate(mut func(*orientationGateCounters)) {
	orientationGateMu.Lock()
	defer orientationGateMu.Unlock()
	mut(&orientationGateCounts)
}

// OrientationGateCensus reports the counters.
func OrientationGateCensus() orientationGateCounters {
	orientationGateMu.Lock()
	defer orientationGateMu.Unlock()
	return orientationGateCounts
}

// ResetOrientationGateCensus clears the counters.
func ResetOrientationGateCensus() {
	orientationGateMu.Lock()
	defer orientationGateMu.Unlock()
	orientationGateCounts = orientationGateCounters{}
}

// FormatOrientationGateCensus renders the census for a harness to log.
func FormatOrientationGateCensus() string {
	c := OrientationGateCensus()
	return fmt.Sprintf("orientation gate (materializedNLJOrdinalLayoutMatches): calls %d "+
		"(not-a-seed %d, tiled-by-2 %d, tiled-by-other %d); of the tiled-by-2: "+
		"unverifiable %d, matched %d, DECLINED %d; firings where the MAP count "+
		"differs from the TILE count (the population 3d' moves) %d, of which "+
		"DECLINED (the plans 3d' is accountable for) %d",
		c.Calls, c.NotASeed, c.TiledByTwo, c.TiledByOther,
		c.Unverifiable, c.Matched, c.Declined, c.MapCountDiffers, c.DeclinedNewlyChecked)
}

// OrientationGateFloors pins that RFC-200 step 3d' stays LIVE.
//
// The step closed a pre-existing fail-open and moved NO plan, and the reason is
// measured rather than assumed: 72 firings that the old map-count gate skipped
// are now checked, and every one of them MATCHES. The 61 declines the gate
// reports were all firings the old gate already checked and already declined.
//
// So the floor that matters is MapCountDiffers. If it reaches zero the fix has
// gone LATENT — no corpus shape reaches the newly-answerable path — and every
// claim that the fail-open is closed becomes vacuous, printing exactly the same
// "no plans moved" as today.
type OrientationGateFloors struct {
	// Calls floors the gate's overall population.
	Calls int
	// MapCountDiffers floors the population the step MOVES. This is the
	// live/latent discriminator; see the type doc.
	MapCountDiffers int
	// UnverifiableCeiling CAPS the second fail-open — the firings where a leg
	// plan cannot state its row shape, so the structural comparison has nothing
	// to compare and the gate returns true.
	//
	// A CEILING, not a floor, because this counter's DANGEROUS DIRECTION IS
	// GROWTH. Every other population on this path is watched for collapse; this
	// one is a permissive answer, so more of it means more orientation checks
	// silently skipped. Left uncapped it could absorb the entire tiled-by-2
	// population — the gate would report a clean partition, no declines, and be
	// checking nothing.
	//
	// Zero means uncapped.
	UnverifiableCeiling int
}

// AssertOrientationGateCensus checks the partitions and the floors.
func AssertOrientationGateCensus(w io.Writer, floors *OrientationGateFloors) bool {
	return assertOrientationGateCounters(w, OrientationGateCensus(), floors)
}

func assertOrientationGateCounters(w io.Writer, c orientationGateCounters, floors *OrientationGateFloors) bool {
	failed := false
	// The partition, which holds over any population: every call lands in exactly
	// one of the three shape buckets.
	if got := c.NotASeed + c.TiledByTwo + c.TiledByOther; got != c.Calls {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: notASeed(%d) + tiledByTwo(%d) + "+
			"tiledByOther(%d) = %d, but calls = %d.\n"+
			"  Every firing has exactly one tile count, so these must partition the\n"+
			"  calls. A gap means a shape arm returns before recording.\n",
			c.NotASeed, c.TiledByTwo, c.TiledByOther, got, c.Calls)
	}
	if got := c.Unverifiable + c.Matched + c.Declined; got != c.TiledByTwo {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: unverifiable(%d) + matched(%d) + "+
			"declined(%d) = %d, but tiledByTwo = %d.\n"+
			"  Only a two-tile row reaches the structural comparison, and it then takes\n"+
			"  exactly one of the three dispositions.\n",
			c.Unverifiable, c.Matched, c.Declined, got, c.TiledByTwo)
	}
	if c.DeclinedNewlyChecked > c.Declined || c.DeclinedNewlyChecked > c.MapCountDiffers {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: declinedNewlyChecked(%d) exceeds "+
			"declined(%d) or mapCountDiffers(%d) — it is the CROSS of those two and "+
			"cannot be larger than either.\n",
			c.DeclinedNewlyChecked, c.Declined, c.MapCountDiffers)
	}
	if floors == nil {
		return failed
	}
	if c.Calls < floors.Calls {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: %d calls, want >= %d — the gate is "+
			"not being reached at all.\n", c.Calls, floors.Calls)
	}
	if floors.UnverifiableCeiling > 0 && c.Unverifiable > floors.UnverifiableCeiling {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: %d UNVERIFIABLE firing(s), want <= %d.\n"+
			"  This is a CEILING because this counter's dangerous direction is GROWTH. An\n"+
			"  unverifiable firing is one where a leg plan cannot state its row shape, so\n"+
			"  the structural orientation comparison has nothing to compare and the gate\n"+
			"  returns TRUE — the permissive answer. It is a SECOND fail-open, separate\n"+
			"  from the map-count one RFC-200 step 3d' closed, and left uncapped it can\n"+
			"  absorb the whole checkable population while the census still reports a\n"+
			"  clean partition and zero declines.\n"+
			"  WHAT GROWTH MEANS: more join orientations are going unchecked. Find which\n"+
			"  leg plans stopped stating a row type — that is the fixable half — rather\n"+
			"  than raising this bound.\n", c.Unverifiable, floors.UnverifiableCeiling)
	}
	if c.MapCountDiffers < floors.MapCountDiffers {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: only %d firing(s) have a MAP count "+
			"differing from the TILE count, want >= %d.\n"+
			"  This is RFC-200 step 3d''s live/latent discriminator. That step replaced a\n"+
			"  map-count gate with the top-level RUN LIST, closing a pre-existing\n"+
			"  fail-open, and it moved NO plan — measured: 72 firings became newly\n"+
			"  checkable and ALL of them matched.\n"+
			"  WHAT A ZERO RE-ARMS: with no firing reaching the newly-answerable path,\n"+
			"  'the fail-open is closed' becomes an untested claim that prints exactly\n"+
			"  the same clean result as a live, agreeing check. Find out why the box-leg\n"+
			"  seed shapes stopped being planned before concluding anything from the\n"+
			"  zero declines beside it.\n", c.MapCountDiffers, floors.MapCountDiffers)
	}
	return failed
}
