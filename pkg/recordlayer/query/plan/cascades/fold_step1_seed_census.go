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
		return "no-unsafe-leg(nil from below legOrdinalSafety)"
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

	// CorrelatedStep1Firings and CorrelatedStep1WithWindows measure the
	// REACHABILITY of the `correlatedStep1 && ordinalWindows != nil` conjunction
	// at implementJoinWithExistential's layout read.
	//
	// It is the one conjunction on this arm that could not be established BY
	// READING, and it is the wall any conversion of the reconstruct-nil residue
	// contacts: `:4124`'s mint and its FlatMap construction run on BOTH the
	// correlated and the materialized arm — the correlatedStep1 block only
	// selects step1Expr — so giving a leg a positionable result value means
	// producing a baked ordinal on an arm where a name-keyed row context raises
	// values.BakedNameContextError. That arm carries a two-revert history.
	//
	// Structurally the conjunction is REACHABLE, not dead: on the correlated arm
	// ordinalWindows is non-nil exactly when sel.GetResultValue() is ALREADY a
	// pristine ordinal seed, which nothing forbids. "Reachable in principle" and
	// "reached by the corpus" are different claims, and only the second one tells
	// a conversion whether it will meet the wall on day one or on the day some
	// unrelated query changes shape. WithWindows is the count that answers it.
	//
	// CorrelatedStep1Firings is the DENOMINATOR for that ratio, counted at the
	// same site. It is deliberately NOT Class[foldStep1DeclineCorrelatedStep1]:
	// that one is counted inside foldStep1Seed and this one at the layout read,
	// and the two being counted apart is what would make a firing that reaches
	// one and not the other visible instead of arithmetic.
	CorrelatedStep1Firings     int
	CorrelatedStep1WithWindows int

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

// recordFoldStep1DeclinedMergeRV counts a DECLINED firing whose step1RV is
// itself a positional merge. Callers must guard on
// values.LegIdentityCensusEnabled().
func recordFoldStep1DeclinedMergeRV() {
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	foldStep1Counts.DeclinedStep1RVIsMerge++
}

// recordCorrelatedStep1Windows counts ONE correlated-arm firing at the layout
// read, and whether the layout answered. Callers must guard on
// values.LegIdentityCensusEnabled(). See CorrelatedStep1WithWindows.
func recordCorrelatedStep1Windows(hasWindows bool) {
	foldStep1Mu.Lock()
	defer foldStep1Mu.Unlock()
	foldStep1Counts.CorrelatedStep1Firings++
	if hasWindows {
		foldStep1Counts.CorrelatedStep1WithWindows++
	}
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
	fmt.Fprintf(&b, "\n  %-48s %d of %d", "correlatedStep1 firings WITH a merged layout",
		c.CorrelatedStep1WithWindows, c.CorrelatedStep1Firings)
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

	// CorrelatedStep1FiringsFloor is a FLOOR, not an equality, and it is the only
	// one in this struct — which is why it is named for what it is.
	//
	// It floors the DENOMINATOR of the correlatedStep1-with-windows measurement.
	//
	// THE NUMERATOR IS 108 OF 108 — UNIVERSAL, not occasional, measured over the
	// whole real-FDB corpus on three consecutive runs. Every correlated firing
	// arrives at the layout read with a merged layout already derived, because on
	// that arm step1RV is sel.GetResultValue() handed back unchanged and it is
	// already a pristine ordinal seed. So any conversion of the reconstruct-nil
	// residue meets the BakedNameContextError wall on day one, on 100% of the
	// correlated population — not on some future corpus shape.
	//
	// THIS COMMENT SAID "currently a measured zero" AND THAT WAS NEVER MEASURED.
	// It was drafted from the reasoning that the correlated wall means nothing is
	// positioned, written BEFORE the counter's first run, and not revisited when
	// the run came back 108 of 108. It is recorded rather than quietly corrected
	// because it is the exact failure this census family exists to prevent,
	// committed inside the census: a prediction shipped in the voice of a
	// measurement, in a comment nothing could contradict.
	//
	// The numerator stays UNGATED, and the reason has flipped with the number.
	// While it read as zero, the argument was "a rise is a finding". At 100% the
	// only movement available is a DROP, and a drop is still a finding rather
	// than a regression — it means either the corpus moved or the layout stopped
	// being derived on that arm, and those need reading apart, not blocking. What
	// must not happen silently is the DENOMINATOR going to zero, because then the
	// ratio measures an absence of traffic and reads exactly like an absence of
	// the shape.
	CorrelatedStep1FiringsFloor *int
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
	if c.CorrelatedStep1WithWindows > c.CorrelatedStep1Firings {
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: correlatedStep1 firings WITH a merged\n"+
			"  layout (%d) exceeds the correlatedStep1 firings counted at the same site\n"+
			"  (%d). They are recorded by one call; a gap means the two stopped being the\n"+
			"  numerator and denominator of one ratio.\n",
			c.CorrelatedStep1WithWindows, c.CorrelatedStep1Firings)
	}
	if gates == nil {
		return failed
	}
	if gates.CorrelatedStep1FiringsFloor != nil && c.CorrelatedStep1Firings < *gates.CorrelatedStep1FiringsFloor {
		failed = true
		fmt.Fprintf(w, "FOLD-STEP1 SEED CENSUS FAIL: %d correlatedStep1 firing(s) reached the\n"+
			"  layout read, want >= %d.\n"+
			"  This is the DENOMINATOR of the `correlatedStep1 && ordinalWindows != nil`\n"+
			"  reachability measurement — the wall any conversion of the reconstruct-nil\n"+
			"  residue contacts. With it at zero the companion count is measuring an\n"+
			"  absence of TRAFFIC while reading as an absence of the SHAPE, and the\n"+
			"  conversion would be planned against a number that says nothing.\n"+
			"  census: %s\n", c.CorrelatedStep1Firings, *gates.CorrelatedStep1FiringsFloor,
			FormatFoldStep1SeedCensus())
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

// quantifiedObjectValueIsTyped remains as the census discriminator while the
// historical untyped bucket ages out. Exact QOV admission now rejects every
// unresolved type, so every QOV recognized by values is typed by construction.
func quantifiedObjectValueIsTyped(qov values.QuantifiedObjectValue) bool {
	return qov != nil
}

// describeQOVType spells the flowed type the boolean above collapses.
//
// The boolean answers "is there a type"; this answers "WHICH", and the two are
// different questions in exactly the way that matters here. A residue counted as
// untyped and a residue counted as typed-but-not-a-row shape are different
// defects with different fixes, and a %t cannot tell them apart — which is how a
// population can be re-measured, found typed, and still be the same residue.
// For a record type the ARITY is what the layout authority needs, so it is what
// is printed.
func describeQOVType(qov values.QuantifiedObjectValue) string {
	if qov == nil {
		return "<nil>"
	}
	typ := qov.FlowedType()
	if rt, ok := typ.(*values.RecordType); ok {
		return fmt.Sprintf("RecordType(%d)", len(rt.Fields))
	}
	return typ.Code().String()
}

// declinedLegOriginSuffix is the producer attribution appended to a bare-QOV
// witness, and it is GATED because its reader is not free.
//
// describeFlatMapResultOrigin takes flatMapProducerMu — a process-global mutex —
// on every call. classifyDeclinedLeg is NOT a census-only function: it runs
// inside reconstructFoldStep1Seed, on the production seed path, for every
// declined leg of every EXISTS-over-join firing, whether or not any census is
// collecting. Reading the attribution there put a global lock acquisition on
// that path for a string nothing consumes with the census off.
//
// The gate is here rather than inside describeFlatMapResultOrigin so that the
// ARGUMENT is not built either: a helper that returns "" after locking has
// already paid the cost this gate exists to remove. That is the same shape the
// qualifier-recovery recorders were hoisted into, for the same reason.
func declinedLegOriginSuffix(rv values.Value) string {
	if values.LegIdentityCensusEnabled() {
		return " " + describeFlatMapResultOrigin(rv)
	}
	return ""
}

// classifyDeclinedLeg describes the node legOrdinalSafety refused.
//
// The result-value shapes it separates are the ones RFC-200's gates are stated
// against; anything else is Other with a witness naming the plan type, so a new
// population arrives as a named witness rather than as movement in a bucket.
//
// It is called from the PRODUCTION path (reconstructFoldStep1Seed), not from a
// census recorder, so anything it does costs something with the census off. The
// shape classification itself is a type switch over a value already in hand; the
// producer attribution is not, and is gated — see declinedLegOriginSuffix.
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
	if qov, ok := values.AsQuantifiedObjectValue(rv); ok {
		return foldStep1LegShapeBareQOV, fmt.Sprintf("%T rv=bare QOV (typed=%t rvtype=%s%s)",
			node, quantifiedObjectValueIsTyped(qov), describeQOVType(qov), declinedLegOriginSuffix(rv))
	}
	switch t := rv.(type) {
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

	// MatchedFloor floors the population the gate PROVES. Added because the two
	// deciding arms — Matched and Declined — had no bound in either direction,
	// so the gate could have gone from proving 232 layouts to proving none and
	// every other number here would still have been satisfied. Matched
	// collapsing means the structural comparison stopped succeeding: either the
	// legs stopped stating rows, or the comparison got stricter than the shapes
	// it is fed.
	//
	// Zero means unfloored.
	MatchedFloor int

	// DeclinedCeiling CAPS the refusals, and the direction is deliberate.
	//
	// A decline here is not a neutral outcome: this gate is what admits the
	// materialized NLJ at all, so declining BOTH orientations does not fall back
	// to a slower plan, it loses the plan entirely ("best expression is not a
	// physical plan"). That is a measured failure mode, not a hypothetical — a
	// stated leg type compared against an unstated seed window produced exactly
	// it, which is why recordFieldsMatch treats an unstated side as unable to
	// contradict. So growth is the alarm: more declines means more queries
	// silently losing their plan.
	//
	// Zero means uncapped.
	DeclinedCeiling int
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
	if floors.MatchedFloor > 0 && c.Matched < floors.MatchedFloor {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: only %d MATCHED firing(s), want >= %d.\n"+
			"  Matched is the population the gate PROVES, and its dangerous direction is\n"+
			"  COLLAPSE. Every other bound here can be satisfied by a gate that proves\n"+
			"  nothing: the partition still adds up, the unverifiable ceiling is a\n"+
			"  ceiling, and declines only have to stay low. This floor is what separates\n"+
			"  \"the comparison succeeds\" from \"the comparison never runs\".\n"+
			"  WHAT COLLAPSE MEANS: either the leg plans stopped stating rows (look for a\n"+
			"  GetResultType that regressed to UnknownType), or the comparison got\n"+
			"  stricter than the shapes it is fed.\n", c.Matched, floors.MatchedFloor)
	}
	if floors.DeclinedCeiling > 0 && c.Declined > floors.DeclinedCeiling {
		failed = true
		fmt.Fprintf(w, "ORIENTATION GATE CENSUS FAIL: %d DECLINED firing(s), want <= %d.\n"+
			"  A CEILING, because a decline here is not a fallback to a slower plan — this\n"+
			"  gate is what admits the materialized NLJ at all, so declining BOTH\n"+
			"  orientations loses the plan outright (\"best expression is not a physical\n"+
			"  plan\"). That is measured, not hypothetical: comparing a STATED leg type\n"+
			"  against an UNSTATED seed-window field produced exactly that failure, which\n"+
			"  is why recordFieldsMatch treats an unstated side as unable to contradict.\n"+
			"  WHAT GROWTH MEANS: queries are losing their plans. Find the comparison that\n"+
			"  started refusing — do not raise this bound to make the red go away.\n",
			c.Declined, floors.DeclinedCeiling)
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

// The FLATMAP RESULT-VALUE PRODUCER census.
//
// It answers the question the outcome census above structurally cannot: WHICH
// construction site emits the result value a declined leg carries, and what that
// value IS at the moment it is handed to the plan.
//
// The outcome census classifies the leg it REFUSED. That is the right cut for
// "why was this seed declined" and the wrong cut for "who built the thing" — a
// refused node names its shape, never its author. So an attribution stated from
// the outcome census is an inference, and this file's whole discipline is that an
// inference about a population is not a measurement of it.
//
// IT ALSO CARRIES A HARD ZERO THAT IS A REFUTATION, NOT A PREDICTION.
// `UntypedQOV` counts result values handed to a FlatMap that are a
// QuantifiedObjectValue carrying UnknownType — the shape Java cannot express at
// all (QuantifiedObjectValue.of requires a Type, QuantifiedObjectValue.java:187;
// Quantifier.getFlowedObjectType is a Verify.verify plus requireNonNull,
// Quantifier.java:801-810). It measures ZERO over the real-FDB corpus, and that
// zero is why the reconstruct-nil residue is NOT a typing gap: those legs carry
// real RecordTypes of arity 1-4, and legOrdinalSafety refuses them on SHAPE
// (values.IsPositionalMergeRC needs a *RecordConstructorValue, which no QOV can
// be, typed or not), never on typing.
//
// A non-zero here re-arms the typing argument and is a finding on its own — hence
// an assertion rather than a printed number.
//
// GATED by values.LegIdentityCensusEnabled, like every census on this path.

// flatMapProducerSite names one non-test RecordQueryFlatMapPlan construction.
// The identity is the SITE, not the file position, so the census survives the
// line-number rotation that has already invalidated three written-down
// attributions of this population.
type flatMapProducerSite int

const (
	// flatMapSiteCorrelated: buildCorrelatedFlatMapPlan, which passes its
	// resultValue parameter straight through.
	flatMapSiteCorrelated flatMapProducerSite = iota
	// flatMapSiteExistentialSelect: implementExistentialSelect, flowing
	// sel.GetResultValue().
	flatMapSiteExistentialSelect
	// flatMapSiteJoinWithExistential: the 3-quantifier join+EXISTS arm — the one
	// site that MINTS a result value rather than flowing one
	// (values.NewQuantifiedObjectValue over the merged outer correlation).
	flatMapSiteJoinWithExistential
	// flatMapSiteYieldExistsFlatMap: yieldExistsFlatMap, flowing
	// sel.GetResultValue().
	flatMapSiteYieldExistsFlatMap

	flatMapProducerSiteCount
)

func (s flatMapProducerSite) String() string {
	switch s {
	case flatMapSiteCorrelated:
		return "buildCorrelatedFlatMapPlan"
	case flatMapSiteExistentialSelect:
		return "implementExistentialSelect"
	case flatMapSiteJoinWithExistential:
		return "implementJoinWithExistential(MINT)"
	case flatMapSiteYieldExistsFlatMap:
		return "yieldExistsFlatMap"
	}
	return "unknown"
}

// flatMapProducerCounters is the producer census's state.
type flatMapProducerCounters struct {
	// Calls counts every construction, per site.
	Calls [flatMapProducerSiteCount]int
	// TypedQOV / UntypedQOV split the QOV-shaped result values by whether they
	// carry a real flowed type. UntypedQOV is a HARD ZERO; see the header.
	TypedQOV   [flatMapProducerSiteCount]int
	UntypedQOV [flatMapProducerSiteCount]int
	// MergeRC counts positional-merge result values — the shape legOrdinalSafety
	// ACCEPTS, and therefore the only shape a conversion of this residue could
	// aim at.
	MergeRC [flatMapProducerSiteCount]int
	// OtherRV counts everything else (projections, named record constructors).
	OtherRV [flatMapProducerSiteCount]int
	// Shapes records the distinct spellings per site, for the same reason the
	// outcome census keeps witnesses: a bucket that moves without a spelling
	// change is a different event from one that gains a spelling.
	Shapes [flatMapProducerSiteCount]map[string]int
}

var (
	flatMapProducerMu     sync.Mutex
	flatMapProducerCounts flatMapProducerCounters
)

// recordFlatMapResultValue counts ONE FlatMap construction. Callers must guard
// on values.LegIdentityCensusEnabled().
func recordFlatMapResultValue(site flatMapProducerSite, rv values.Value) {
	if site < 0 || site >= flatMapProducerSiteCount {
		return
	}
	shape := "rv=nil"
	switch {
	case rv == nil:
	case values.IsPositionalMergeRC(rv):
		shape = fmt.Sprintf("POSITIONAL-MERGE RC(%d)", len(rv.(*values.RecordConstructorValue).Fields))
	default:
		if qov, isQOV := values.AsQuantifiedObjectValue(rv); isQOV {
			shape = fmt.Sprintf("QOV(typed=%t %s)", quantifiedObjectValueIsTyped(qov), describeQOVType(qov))
		} else {
			shape = fmt.Sprintf("%T", rv)
		}
	}

	flatMapProducerMu.Lock()
	defer flatMapProducerMu.Unlock()
	c := &flatMapProducerCounts
	c.Calls[site]++
	if qov, isQOV := values.AsQuantifiedObjectValue(rv); isQOV {
		if quantifiedObjectValueIsTyped(qov) {
			c.TypedQOV[site]++
		} else {
			c.UntypedQOV[site]++
		}
	} else {
		switch rv.(type) {
		case nil:
			c.OtherRV[site]++
		default:
			if values.IsPositionalMergeRC(rv) {
				c.MergeRC[site]++
			} else {
				c.OtherRV[site]++
			}
		}
	}
	if c.Shapes[site] == nil {
		c.Shapes[site] = map[string]int{}
	}
	if _, known := c.Shapes[site][shape]; known || len(c.Shapes[site]) < foldStep1WitnessCap {
		c.Shapes[site][shape]++
	}
	if rv != nil {
		if flatMapResultOrigin == nil {
			flatMapResultOrigin = map[values.Value]flatMapProducerSite{}
		}
		if prior, seen := flatMapResultOrigin[rv]; seen && prior != site {
			flatMapResultOrigin[rv] = flatMapProducerSiteAmbiguous
		} else if !seen {
			flatMapResultOrigin[rv] = site
		}
	}
}

// flatMapResultOrigin maps a result value BACK to the site that handed it to a
// FlatMap, keyed by the value's own identity.
//
// It is what turns deliverable-style producer attribution from an inference into
// a measurement. The alternative — reading the arity signature of the declined
// legs and matching it against the per-site shape histogram — closes only the
// arities that appear at exactly one site, and leaves the rest to the reader's
// judgement. That is the same class of argument this census family exists to
// replace.
//
// Keying on the VALUE rather than on the plan is deliberate: a result value is
// stored in the plan unchanged, so its identity survives the memo's copies of
// the plan, while a plan-level tag would have to be threaded through
// WithQuantifiers and every rewrite. Every Value implementation reaching this
// map is a pointer type, so interface identity is pointer identity.
//
// A value handed to TWO different sites records as ambiguous rather than
// last-writer-wins — an attribution that silently picks one of two answers is
// worse than one that says it cannot tell.
var flatMapResultOrigin map[values.Value]flatMapProducerSite

// flatMapProducerSiteAmbiguous marks a result value seen at more than one site.
const flatMapProducerSiteAmbiguous flatMapProducerSite = -2

// flatMapResultOriginOf reports the site that produced rv, and whether it is
// known at all. Callers must guard on values.LegIdentityCensusEnabled().
func flatMapResultOriginOf(rv values.Value) (flatMapProducerSite, bool) {
	if rv == nil {
		return 0, false
	}
	flatMapProducerMu.Lock()
	defer flatMapProducerMu.Unlock()
	site, ok := flatMapResultOrigin[rv]
	return site, ok
}

// describeFlatMapResultOrigin spells the producing site for a census witness.
//
// The MINT is reported ahead of the FlatMap site when one is known, because they
// answer different questions and the second was being read as an answer to the
// first. A FlatMap construction that flows sel.GetResultValue() verbatim is a
// COURIER; the author is whatever filled that field, and the SQL translator's
// mint registers itself so the witness can say so. Both are printed when both
// are known — the courier is still the thing that put the value in a plan.
func describeFlatMapResultOrigin(rv values.Value) string {
	if mint, minted := values.SelectResultMintOriginOf(rv); minted {
		if site, ok := flatMapResultOriginOf(rv); ok {
			return "mint=" + mint.String() + " via=" + site.String()
		}
		return "mint=" + mint.String()
	}
	site, ok := flatMapResultOriginOf(rv)
	switch {
	case !ok:
		return "origin=UNRECORDED"
	case site == flatMapProducerSiteAmbiguous:
		return "origin=AMBIGUOUS(>1 site)"
	default:
		return "origin=" + site.String()
	}
}

// FlatMapProducerCensus reports the producer counters.
func FlatMapProducerCensus() flatMapProducerCounters {
	flatMapProducerMu.Lock()
	defer flatMapProducerMu.Unlock()
	return copyFlatMapProducerCounters(flatMapProducerCounts)
}

// copyFlatMapProducerCounters DEEP-copies the per-site shape maps.
//
// Returning the struct by value copies the ARRAYS and shares the MAPS inside
// them, which defeats the whole reason assertFlatMapProducerCounters takes an
// explicit state: the caller was handed a live view of a map the planner is
// still writing. The failure message then renders state that has moved since the
// assertion read it — and iterating it while a concurrent planner writes is not
// a stale number, it is a fatal concurrent map iteration and map write.
func copyFlatMapProducerCounters(c flatMapProducerCounters) flatMapProducerCounters {
	out := c
	for s := range out.Shapes {
		if c.Shapes[s] == nil {
			continue
		}
		m := make(map[string]int, len(c.Shapes[s]))
		for k, v := range c.Shapes[s] {
			m[k] = v
		}
		out.Shapes[s] = m
	}
	return out
}

// ResetFlatMapProducerCensus clears the producer counters.
func ResetFlatMapProducerCensus() {
	flatMapProducerMu.Lock()
	defer flatMapProducerMu.Unlock()
	flatMapProducerCounts = flatMapProducerCounters{}
	flatMapResultOrigin = nil
}

// FormatFlatMapProducerCensus renders the producer census for a harness to log.
func FormatFlatMapProducerCensus() string {
	return formatFlatMapProducerCounters(FlatMapProducerCensus())
}

// formatFlatMapProducerCounters renders an EXPLICIT counter state, so a failure
// message quotes the state that failed rather than re-reading the globals — which
// a concurrent run can have moved by the time the message is built.
func formatFlatMapProducerCounters(c flatMapProducerCounters) string {
	var b strings.Builder
	b.WriteString("FlatMap result-value producers (per construction):")
	for s := flatMapProducerSite(0); s < flatMapProducerSiteCount; s++ {
		fmt.Fprintf(&b, "\n  %-34s calls %d | typedQOV %d | UNTYPED-QOV %d | mergeRC %d | other %d",
			s, c.Calls[s], c.TypedQOV[s], c.UntypedQOV[s], c.MergeRC[s], c.OtherRV[s])
		shapes := make([]string, 0, len(c.Shapes[s]))
		for sh, n := range c.Shapes[s] {
			shapes = append(shapes, fmt.Sprintf("x%d %s", n, sh))
		}
		sort.Strings(shapes)
		for _, sh := range shapes {
			fmt.Fprintf(&b, "\n      shape %s", sh)
		}
	}
	return b.String()
}

// FlatMapProducerFloors is the producer census's gate.
//
// Calls is a FLOOR: a site going dark must be visible, because every zero
// recorded beside a dark site measures an absence of traffic rather than an
// absence of the shape. ZERO MEANS NOT FLOORED, which for a floor is the same
// assertion as a floor of zero.
//
// THE UNTYPED-QOV FLOOR IS GONE, and its absence is the reconciliation rather
// than a relaxation. It floored a DIVERGENCE — an untyped QuantifiedObjectValue,
// which Java cannot build at all — to keep the gap counted while it existed.
// The gap is closed: NewQuantifiedObjectValue now requires an exact type, so an
// untyped QOV is unrepresentable and every site measures zero. A floor pointing
// at a population that can no longer exist is unsatisfiable, and lowering it to
// zero would have left the retirement unwatched.
//
// The direction therefore INVERTS. Zero is the steady state, growth is the
// alarm, and that is asserted unconditionally for every site in
// assertFlatMapProducerCounters — where a configurable refutation could not
// live anyway.
type FlatMapProducerFloors struct {
	Calls [flatMapProducerSiteCount]int
}

// AssertFlatMapProducerCensus checks the untyped-QOV zero at every site and the
// per-site call floors.
//
// THE POLARITY HERE HAS INVERTED ONCE ALREADY, AND THE HISTORY IS THE POINT.
// The census was first written asserting UntypedQOV == 0 at every site, on the
// reading that Go should never build a value Java cannot express. Measured over
// the real-FDB corpus, three of the four sites emitted untyped QOVs in bulk —
// 1609, 249 and 269 — so a blanket zero was not a defended invariant but a wish,
// and it became a FLOOR: the divergence kept COUNTED while it stood, because a
// divergence nobody measures is how a population becomes invisible.
//
// The exact-QOV work then closed the gap at its source.
// NewQuantifiedObjectValue requires an exact type, so an untyped QOV cannot be
// constructed at all and every site measures zero. The floor is unsatisfiable
// and is gone; the zero is back, at every site, with the alarm now pointing at
// REVIVAL rather than at silence. What would trip it is a construction path that
// reaches around the constructor.
//
// buildCorrelatedFlatMapPlan's zero always carried a second, independent claim
// and still does: it is the site producing the reconstruct-nil residue, and its
// zero is what refutes the residue-is-a-typing-gap reading. The declined legs
// carry real RecordTypes — arity 1-3 on the FlatMap legs, 1-4 counting the two
// NestedLoopJoin-legged declines — so the residue is not a typing gap, and typing
// cannot convert it: legOrdinalSafety refuses a FlatMap leg on
// values.IsPositionalMergeRC, which needs a *RecordConstructorValue that no QOV
// satisfies at any typing.
//
// TWO OF THE THREE FORMERLY-UNTYPED SITES NEVER BUILT WHAT THEY WERE CREDITED
// WITH. implementExistentialSelect and yieldExistsFlatMap flow
// sel.GetResultValue() verbatim — the same thing Java's three constructions do
// (ImplementNestedLoopJoinRule.java:187,201,214) — so their counts were a count
// of TRAFFIC through a courier. The author is whatever fills that field, which
// is why describeFlatMapResultOrigin reports the mint ahead of the site, and why
// the retirement had to happen at the mint rather than here.
func AssertFlatMapProducerCensus(w io.Writer, floors *FlatMapProducerFloors) bool {
	return assertFlatMapProducerCounters(w, FlatMapProducerCensus(), floors)
}

// assertFlatMapProducerCounters is the assertion logic over an EXPLICIT counter
// state, so each arm can be driven from a test without driving the whole planner
// into the defective state that would produce it — the same split every census on
// this path makes between its gate and its collection.
func assertFlatMapProducerCounters(w io.Writer, c flatMapProducerCounters, floors *FlatMapProducerFloors) bool {
	failed := false
	// EVERY site, unconditionally, and the alarm is REVIVAL. An untyped
	// QuantifiedObjectValue is what Java cannot build — QuantifiedObjectValue.of
	// has no untyped overload — and Go could, in bulk, at three of these four
	// sites. That gap is closed at the constructor: NewQuantifiedObjectValue
	// requires an exact type, so the value this counts is unrepresentable.
	//
	// The census stays, and its DIRECTION is inverted rather than its floor
	// lowered. While the population existed the danger was it going uncounted, so
	// each site carried a floor; now the danger is it coming BACK — through a new
	// construction path that bypasses the constructor's requirement — and a floor
	// pointing at an impossible population would be unsatisfiable, while deleting
	// the check outright would leave the retirement unwatched.
	//
	// buildCorrelatedFlatMapPlan's zero carries a second claim on top: it is the
	// site that produces the reconstruct-nil residue, and its zero is what refutes
	// the residue-is-a-typing-gap reading — every declined leg carries a real
	// RecordType, so there is nothing to type.
	for s := flatMapProducerSite(0); s < flatMapProducerSiteCount; s++ {
		n := c.UntypedQOV[s]
		if n == 0 {
			continue
		}
		failed = true
		fmt.Fprintf(w, "FLATMAP PRODUCER CENSUS FAIL: %s emitted %d result value(s) that are an\n"+
			"  UNTYPED QuantifiedObjectValue, want 0 — the untyped QOV was RETIRED and this\n"+
			"  is its revival alarm.\n"+
			"  Java cannot build one at all, and NewQuantifiedObjectValue now requires an\n"+
			"  exact type, so a non-zero here means a construction path has appeared that\n"+
			"  bypasses that requirement. Find it before adjusting this check; the whole\n"+
			"  ordinal model rests on every QOV carrying the row it addresses.\n"+
			"  census: %s\n", s, n, formatFlatMapProducerCounters(c))
	}
	if floors == nil {
		return failed
	}
	for s := flatMapProducerSite(0); s < flatMapProducerSiteCount; s++ {
		if c.Calls[s] >= floors.Calls[s] {
			continue
		}
		failed = true
		fmt.Fprintf(w, "FLATMAP PRODUCER CENSUS FAIL: %s made %d construction(s), want >= %d —\n"+
			"  the site has gone dark, so every zero recorded beside it is measuring an\n"+
			"  absence of traffic rather than an absence of the shape.\n"+
			"  census: %s\n", s, c.Calls[s], floors.Calls[s], formatFlatMapProducerCounters(c))
	}
	return failed
}
