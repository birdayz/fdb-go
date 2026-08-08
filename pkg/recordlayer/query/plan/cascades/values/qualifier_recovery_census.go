package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// The QUALIFIER RECOVERY census — the DARK SPLITTERS.
//
// Its sibling, the NAME SPLIT census (name_split_census.go), states its own
// scope in its header and then names four sites it does NOT watch. This file is
// those four sites. Nothing counted here is counted there, and the two censuses'
// zeros are zeros over disjoint populations.
//
// WHAT A "DARK SPLITTER" IS. A site that recovers a QUALIFIER from a RENDERED
// name — slices a string at a dot and treats the bytes on one side as a source
// alias — and that has, until this file, had nothing counting it. The rendered
// name is lossy by construction: a qualified reference `A.B` and a delimited
// identifier `"A.B"` are the SAME BYTES, so a site that decides qualification by
// looking for a dot cannot tell them apart. Java never faces the question,
// because identity there is `name` + `List<String> qualifier` built segment by
// segment off the ANTLR FullId (Identifier.java:34-58, IdentifierVisitor.java:56-64)
// and joined only for DISPLAY (Identifier.java:61-63). Go's debt is every place
// that joins and then re-splits.
//
// WHY INSTRUMENT BEFORE CONVERTING. Every conversion argument on this path has
// so far been made against scratch figures and then refuted by the first real
// measurement: the "110 → 3 → 0" progression turned out to be a population of
// ELEVEN, and the leg-column provenance block's "calls 52, dotted hits 4" was low
// by 50x on one number and HIGH by two on the one the decision rested on. So this
// census does not argue for a conversion. It measures what each site does, and
// the classes below are chosen so the measurement ANSWERS the conversion question
// rather than motivating it.
//
// THE CLASSES ANSWER "WAS THE IDENTITY ALREADY IN HAND?". That is the whole
// conversion-readiness question, and it is what separates a site that can be
// converted mechanically from one that cannot be converted at all:
//
//   - CARRIED — the site decided from a STRUCTURED identity (a segment triple, a
//     QuantifiedObjectValue's correlation, an ordinal). No rendered name was
//     sliced. Counted so a converted channel's LIVENESS is visible: a conversion
//     that silently stopped being reached reads exactly like one that succeeded.
//   - AGREED — a qualifier WAS manufactured from text, AND a structured identity
//     was in hand at the same site, AND the two agree. THIS IS THE
//     CONVERSION-READY POPULATION: the split is redundant here, and replacing it
//     with the identity is provably behaviour-preserving over this traffic.
//   - DIVERGED — manufactured from text with an identity in hand that DISAGREES.
//     A live misread. ASSERTED ZERO.
//   - MANUFACTURED — manufactured from text with NO counterparty identity
//     available at the site at all. THE HARD DEBT: this population cannot be
//     converted by a local edit, because there is nothing local to convert TO.
//     It is the population that says a producer has to be taught to carry
//     segments first.
//   - LEAF-ONLY — a dot was found and the LEAF taken, but the qualifier was
//     DISCARDED because an ordinal or an identity decides the actual read. Text
//     was sliced; no qualifier was invented. Not debt, and counted separately so
//     it is not banked as one.
//   - BARE — the split ran and found no dot. Nothing was manufactured. Counted
//     because it shares the arm with the debt classes, and a census reporting
//     only debt cannot tell a CLEAN arm from a DARK one.
//
// A site reporting only CARRIED + AGREED is a mechanical conversion. A site
// reporting MANUFACTURED is a producer change. That distinction is the
// deliverable, and it is measured rather than asserted.
//
// GATED by LegIdentityCensusEnabled — the same gate its siblings ride. What
// production pays census-off is ONE ATOMIC LOAD per decision, and that is the
// whole of it: every recorder helper at every site reads the gate as its first
// statement and returns, so no name is parsed, no qualifier is upper-cased and
// no counterparty is looked up to build an argument the disabled recorder would
// discard. The two callers that must compute an identity BEFORE calling a
// recorder (the EXISTS fold's sortKeyName and sortKeySourceValue) hoist the gate
// above that computation for the same reason.
//
// An earlier revision of this line claimed production "never pays an atomic",
// which was false in both directions: it does pay exactly one, and at the time
// it was written it also paid the classification behind it. Counts are per
// CALL, one call per resolution DECISION.

// QualifierRecoverySite is one of the four dark splitters named in
// name_split_census.go's header. The parseColRef family contributes THREE sites
// rather than one: its 27 production call sites are overwhelmingly display or
// lookup, and only three of them manufacture a qualifier that then DECIDES
// something. Those three are what is counted, individually, because they are
// three different decisions with three different counterparties and a single
// merged "parseColRef" number could not answer the conversion question for any
// of them.
type QualifierRecoverySite int

const (
	// QualRecSiteRecursiveRemap is query.recursiveRemapValues' dotted arm
	// (cascades_translator.go). STRICTLY THE WORST OF THE FOUR: it does not
	// manufacture a qualifier STRING to look up in a leg table, it manufactures
	// a CorrelationIdentifier directly out of the bytes before the FIRST dot and
	// hands it to a QuantifiedObjectValue. A misread here does not fail to
	// resolve — it resolves against a correlation that does not exist.
	//
	// Its own header already admits the break: the lazy dotted Field "spells
	// both the qualified B.ID and a QUOTED identifier containing a dot in the
	// same string".
	QualRecSiteRecursiveRemap QualifierRecoverySite = iota

	// QualRecSiteExistsSortSplit is query.splitQualifier (cascades_translator.go),
	// the EXISTS fold's LAST-dot split, recorded at its two callers
	// (sortKeySourceValue, resolveKeyName). Its own doc concedes the deeper
	// case: `A.B.C` is treated as qualifier `A.B`, column `C`.
	QualRecSiteExistsSortSplit

	// QualRecSiteDerivedUnnestSource is query.classifyDerivedUnnestArray's split
	// of the unnest body's source column (derived_unnest.go), whose manufactured
	// qualifier is compared against the base scan's alias/table name.
	QualRecSiteDerivedUnnestSource

	// QualRecSiteProjScopeClassify is embedded.classifyProjFieldValue
	// (logical_predicate.go): inner- vs outer-scoping of a projection field. The
	// parseColRef call is in the ELSE of a QuantifiedObjectValue check, so its
	// CARRIED class is that QOV branch and its split arm runs only where no
	// correlation was carried.
	QualRecSiteProjScopeClassify

	// QualRecSiteProjQualVsScan is embedded's projected-column qualifier check
	// (cascades_generator.go): a manufactured qualifier matched against the
	// scan's name/alias, raising ErrCodeUndefinedColumn on a mismatch. This one
	// decides an ERROR, which is the sharpest consequence in the family.
	QualRecSiteProjQualVsScan

	// QualRecSiteDisplayLabelStrip is embedded's display-label strip
	// (cascades_generator.go), guarded by the PARENTHESIS HEURISTIC in
	// isPlainQualifiedColumnReference — which rejects on `()` because
	// "parentheses identify the rendered aggregate/function label at issue".
	// That is a heuristic over a RENDERING, not a parse, and it is the reason
	// this site is counted despite its own comment stating the provenance is
	// carried: the provenance BOOLEAN (AliasMinted) is carried, the QUALIFIER
	// still is not.
	QualRecSiteDisplayLabelStrip

	qualRecSiteCount
)

func (s QualifierRecoverySite) String() string {
	switch s {
	case QualRecSiteRecursiveRemap:
		return "recursiveRemap"
	case QualRecSiteExistsSortSplit:
		return "existsSortSplit"
	case QualRecSiteDerivedUnnestSource:
		return "derivedUnnestSource"
	case QualRecSiteProjScopeClassify:
		return "projScopeClassify"
	case QualRecSiteProjQualVsScan:
		return "projQualVsScan"
	case QualRecSiteDisplayLabelStrip:
		return "displayLabelStrip"
	default:
		return "unknown"
	}
}

// QualifierRecoverySites returns every site, for callers that walk them.
func QualifierRecoverySites() []QualifierRecoverySite {
	out := make([]QualifierRecoverySite, 0, qualRecSiteCount)
	for s := QualifierRecoverySite(0); s < qualRecSiteCount; s++ {
		out = append(out, s)
	}
	return out
}

// QualifierRecoveryClass is one decision's bucket. The six partition every call.
type QualifierRecoveryClass int

const (
	// QualRecCarried: a structured identity decided. No rendered name was sliced.
	QualRecCarried QualifierRecoveryClass = iota

	// QualRecAgreed: a qualifier was manufactured from text AND a structured
	// identity in hand agrees with it. THE CONVERSION-READY POPULATION.
	QualRecAgreed

	// QualRecDiverged: manufactured from text, identity in hand DISAGREES. A live
	// misread — asserted ZERO.
	QualRecDiverged

	// QualRecManufactured: manufactured from text with NO counterparty. THE HARD
	// DEBT — not convertible by a local edit.
	QualRecManufactured

	// QualRecLeafOnly: a dot was found, the leaf taken, the qualifier DISCARDED
	// because an ordinal or identity decides the read. Not debt.
	QualRecLeafOnly

	// QualRecBare: the split ran, no dot, nothing manufactured.
	QualRecBare

	// QualRecHeuristicDecline: a dot WAS found and the split was declined — but
	// by a HEURISTIC OVER THE RENDERING rather than by any structured fact. The
	// only instance is isPlainQualifiedColumnReference's rejection on `()`,
	// which reads a rendered aggregate/function label out of a string by looking
	// for parentheses in it.
	//
	// It is its own class and not folded into BARE, because the two are opposite
	// findings: bare means the site was handed a name with no qualifier in it,
	// while this means the site WAS handed a dotted name, had to decide whether
	// the dot was a qualifier boundary, and decided by inspecting punctuation. A
	// census that reported those together would show the site's riskiest
	// population as its cleanest.
	//
	// Structurally zero at the other five sites — none of them carries a
	// rendering heuristic — and that zero is construction, not a finding.
	QualRecHeuristicDecline

	qualRecClassCount
)

func (c QualifierRecoveryClass) String() string {
	switch c {
	case QualRecCarried:
		return "carried"
	case QualRecAgreed:
		return "AGREED"
	case QualRecDiverged:
		return "DIVERGED"
	case QualRecManufactured:
		return "MANUFACTURED"
	case QualRecLeafOnly:
		return "leafOnly"
	case QualRecBare:
		return "bare"
	case QualRecHeuristicDecline:
		return "heuristicDecline"
	default:
		return "unknown"
	}
}

// qualRecWitnessCap bounds each (site, class) witness list. Saturation is a
// FAILURE at the assertion, not a silent truncation — see AssertQualifierRecoveryCensus.
const qualRecWitnessCap = 32

// QualifierRecoveryWitnessCap exposes the cap so a harness can check saturation.
func QualifierRecoveryWitnessCap() int { return qualRecWitnessCap }

var (
	qualRecMu        sync.Mutex
	qualRecCounts    [qualRecSiteCount][qualRecClassCount]int
	qualRecWitnesses [qualRecSiteCount][qualRecClassCount][]string
)

// RecordQualifierRecovery records ONE resolution decision at a dark splitter.
//
// `name` is the rendered name the site was handed and `identity` is the
// structured counterparty it had in hand (empty when it had none). Both go into
// the witness so a regrowth arrives with the pair that caused it — for DIVERGED
// in particular, the count alone cannot say which side is wrong.
func RecordQualifierRecovery(site QualifierRecoverySite, class QualifierRecoveryClass, name, identity string) {
	if !LegIdentityCensusEnabled() ||
		site < 0 || site >= qualRecSiteCount ||
		class < 0 || class >= qualRecClassCount {
		return
	}
	qualRecMu.Lock()
	defer qualRecMu.Unlock()
	qualRecCounts[site][class]++
	// BARE and CARRIED are high-population and their spellings say nothing a
	// count does not. The four classes that involve a slice each earn a witness.
	switch class {
	case QualRecAgreed, QualRecDiverged, QualRecManufactured, QualRecLeafOnly, QualRecHeuristicDecline:
	default:
		return
	}
	if len(qualRecWitnesses[site][class]) >= qualRecWitnessCap {
		return
	}
	w := fmt.Sprintf("%q", name)
	if identity != "" {
		w = fmt.Sprintf("%q vs identity %q", name, identity)
	}
	for _, e := range qualRecWitnesses[site][class] {
		if e == w {
			return
		}
	}
	qualRecWitnesses[site][class] = append(qualRecWitnesses[site][class], w)
}

// QualifierRecoveryCensus returns a snapshot of the counters and witnesses.
func QualifierRecoveryCensus() ([qualRecSiteCount][qualRecClassCount]int, [qualRecSiteCount][qualRecClassCount][]string) {
	qualRecMu.Lock()
	defer qualRecMu.Unlock()
	var w [qualRecSiteCount][qualRecClassCount][]string
	for s := range qualRecWitnesses {
		for c := range qualRecWitnesses[s] {
			w[s][c] = append([]string(nil), qualRecWitnesses[s][c]...)
			sort.Strings(w[s][c])
		}
	}
	return qualRecCounts, w
}

// ResetQualifierRecoveryCensus clears the counters. For tests measuring one shape.
func ResetQualifierRecoveryCensus() {
	qualRecMu.Lock()
	defer qualRecMu.Unlock()
	qualRecCounts = [qualRecSiteCount][qualRecClassCount]int{}
	qualRecWitnesses = [qualRecSiteCount][qualRecClassCount][]string{}
}

// QualifierRecoveryFloors is the minimum population each site must report, so a
// site going DARK fails rather than reading as a clean zero.
type QualifierRecoveryFloors struct {
	// Calls is the minimum total calls (all six classes) per site.
	Calls [qualRecSiteCount]int

	// Split is the minimum SPLIT population per site — the calls that actually
	// entered a splitting arm (AGREED + DIVERGED + MANUFACTURED + LEAF-ONLY +
	// BARE), as opposed to CARRIED calls that decided without one.
	//
	// Separate from Calls because at a site whose carried channel does the work
	// the two diverge completely: a healthy Calls total says the CARRIED arm is
	// reached and says nothing whatever about the arms this census's zeros are
	// zeros over.
	//
	// A ZERO ENTRY IS A DECLARATION, NOT AN ABSENT FLOOR, and it is CHECKED IN
	// THE STALE DIRECTION: a site declared 0 whose split population is now
	// NON-zero FAILS. The "watched, not proven" label cannot silently outlive the
	// condition that made it honest. Declaring 0 means "this site's split arms
	// are measured empty over this corpus, they are covered by a unit wiring pin
	// instead, and the day the corpus starts driving them somebody re-reads this
	// line."
	Split [qualRecSiteCount]int
}

// AssertQualifierRecoveryCensus checks the census's hard zero, its saturation
// guards and its floors, writing a report to w. Returns TRUE if anything FAILED
// — the convention its siblings on this path use.
//
// THE HARD ZERO IS **DIVERGED**, and it is the only zero this census asserts.
// That is a deliberate departure from its sibling, which asserts its debt bucket
// at zero: here MANUFACTURED is expected to be non-zero at several sites, because
// the whole finding is that these splitters exist and are reached. Asserting it
// at zero would either be false on the day it was written or would have to be
// relaxed into meaninglessness. What CANNOT be tolerated is a split that
// contradicts an identity the site was holding at the time — that is not debt,
// it is a wrong answer.
func AssertQualifierRecoveryCensus(w io.Writer, exp *QualifierRecoveryExpectations, corpus string) bool {
	counts, witnesses := QualifierRecoveryCensus()
	return assertQualifierRecoveryCounts(w, counts, witnesses, exp, corpus)
}

// QualifierRecoveryExpectations is what one CORPUS expects of the census. Two
// harnesses drive this instrument and they expect different things, so the
// expectations are a parameter rather than a constant.
type QualifierRecoveryExpectations struct {
	// Floors is the per-site population this corpus must report, or nil under a
	// -test.run filter that makes the population meaningless.
	Floors *QualifierRecoveryFloors

	// AllowedDiverged is every DIVERGED witness this corpus's own test FIXTURES
	// deliberately drive, per site.
	//
	// A corpus of SQL cannot need this: every one of its splits comes from a
	// production producer, so any disagreement there is a defect and the zero is
	// a bare zero. A corpus that calls package-private recorders with hand-built
	// fixtures DOES need it — the fixtures that prove the DIVERGED bucket is
	// REACHABLE necessarily fill it, and without them the census's one asserted
	// zero would be a zero nothing had ever shown could be non-zero.
	//
	// The check is per-witness rather than a count tolerance, so a real
	// divergence at a new spelling cannot hide inside a budget. The residual is
	// stated rather than smoothed: a real divergence spelled EXACTLY like one of
	// these is absorbed, because the witness set dedups by spelling — which is
	// why the entries are listed individually with the fixture that drives each.
	AllowedDiverged map[QualifierRecoverySite]map[string]struct{}
}

// assertQualifierRecoveryCounts is the decision, split from the process-global
// counters so every failure direction can be exercised without a corpus run —
// the same split its siblings make, for the same reason.
func assertQualifierRecoveryCounts(
	w io.Writer,
	counts [qualRecSiteCount][qualRecClassCount]int,
	witnesses [qualRecSiteCount][qualRecClassCount][]string,
	exp *QualifierRecoveryExpectations,
	corpus string,
) bool {
	failed := false
	var floors *QualifierRecoveryFloors
	var allowedDiverged map[QualifierRecoverySite]map[string]struct{}
	if exp != nil {
		floors, allowedDiverged = exp.Floors, exp.AllowedDiverged
	}

	fmt.Fprintf(w, "[%s] qualifier recovery census — the DARK SPLITTERS (per resolution decision):\n", corpus)
	for s := QualifierRecoverySite(0); s < qualRecSiteCount; s++ {
		total := 0
		for c := QualifierRecoveryClass(0); c < qualRecClassCount; c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(w, "  %-20s calls %d | carried %d | AGREED %d | DIVERGED %d | MANUFACTURED %d | leafOnly %d | bare %d | heuristicDecline %d\n",
			s, total,
			counts[s][QualRecCarried], counts[s][QualRecAgreed], counts[s][QualRecDiverged],
			counts[s][QualRecManufactured], counts[s][QualRecLeafOnly], counts[s][QualRecBare],
			counts[s][QualRecHeuristicDecline])
		for c := QualifierRecoveryClass(0); c < qualRecClassCount; c++ {
			for _, wit := range witnesses[s][c] {
				fmt.Fprintf(w, "      %s witness: %s\n", c, wit)
			}
		}
	}

	for s := QualifierRecoverySite(0); s < qualRecSiteCount; s++ {
		if counts[s][QualRecDiverged] == 0 {
			continue
		}
		// A corpus whose own fixtures drive the DIVERGED bucket clears it
		// WITNESS BY WITNESS. Anything the fixtures do not account for came from
		// a producer and fails, exactly as if no allowlist existed.
		if allowed, ok := allowedDiverged[s]; ok {
			unaccounted := make([]string, 0, len(witnesses[s][QualRecDiverged]))
			for _, wit := range witnesses[s][QualRecDiverged] {
				if _, declared := allowed[wit]; !declared {
					unaccounted = append(unaccounted, wit)
				}
			}
			if len(unaccounted) == 0 {
				continue
			}
			failed = true
			fmt.Fprintf(w, "FAIL: %s reported DIVERGED witness(es) that no test fixture accounts\n"+
				"  for: %v.\n"+
				"  The fixtures' deliberate pairs are declared per corpus; a witness outside\n"+
				"  that set came from a PRODUCER, and the fix belongs there — use the identity.\n",
				s, unaccounted)
			continue
		}
		if n := counts[s][QualRecDiverged]; n != 0 {
			failed = true
			fmt.Fprintf(w, "FAIL: %s manufactured a qualifier that CONTRADICTS the structured identity\n"+
				"  it was holding, %d time(s). This is not debt — it is a WRONG ANSWER, and it is\n"+
				"  the one population this census asserts at zero. The site sliced a rendered name\n"+
				"  and got a different source than the identity in hand names, which means either\n"+
				"  the rendering is not the identity joined (a delimited identifier containing a\n"+
				"  dot is the canonical case: `A.B` the reference and \"A.B\" the column are the\n"+
				"  same bytes), or a producer rebased one channel and not the other.\n"+
				"  THE FIX IS TO USE THE IDENTITY, never to widen this zero: the identity is what\n"+
				"  the parser saw, the string is a rendering of it. Witnesses above carry both\n"+
				"  sides of each disagreeing pair.\n", s, n)
		}
	}

	// SATURATION. The witness lists are capped, and past the cap a further
	// DISTINCT spelling is counted but retains nothing — so a saturated list is a
	// SUBSET that reads like a whole.
	//
	// Whether that is a FAILURE depends on what the list is used for, and the two
	// answers are opposite:
	//
	//   - DIVERGED and MANUFACTURED are ANOMALY populations. Their witnesses are
	//     the evidence somebody acts on — which pair disagreed, which name had no
	//     counterparty — and a subset presented as a whole would send that person
	//     to fix the spellings that happened to arrive first. Saturation FAILS.
	//   - AGREED, LEAF-ONLY and HEURISTIC-DECLINE are health populations. Their
	//     witnesses illustrate a shape that is working; the COUNT carries the
	//     claim and the list is an example set. Saturation is expected on any
	//     corpus of size (displayLabelStrip alone agrees on 560 calls (measured, unfiltered) across
	//     dozens of aliases) and is announced, not failed. Failing it would mean
	//     the gate goes red because the suite grew.
	for s := QualifierRecoverySite(0); s < qualRecSiteCount; s++ {
		for _, c := range []QualifierRecoveryClass{QualRecDiverged, QualRecManufactured} {
			if len(witnesses[s][c]) >= qualRecWitnessCap {
				failed = true
				fmt.Fprintf(w, "FAIL: %s retained %d %s witnesses, SATURATING the cap of %d.\n"+
					"  This is an ANOMALY population, so its witness list is the evidence acted\n"+
					"  on and a subset presented as a whole sends the reader to whichever\n"+
					"  spellings arrived first. Narrow the corpus or raise the cap — do not read\n"+
					"  the listed witnesses as complete.\n",
					s, len(witnesses[s][c]), c, qualRecWitnessCap)
			}
		}
		for _, c := range []QualifierRecoveryClass{QualRecAgreed, QualRecLeafOnly, QualRecHeuristicDecline} {
			if len(witnesses[s][c]) >= qualRecWitnessCap {
				fmt.Fprintf(w, "NOTE: %s %s witnesses TRUNCATED at the cap of %d — the listed\n"+
					"  spellings are examples, not the whole set. The COUNT above is complete and\n"+
					"  is what this class's claim rests on.\n", s, c, qualRecWitnessCap)
			}
		}
	}

	if floors == nil {
		return failed
	}
	for s := QualifierRecoverySite(0); s < qualRecSiteCount; s++ {
		total := 0
		for c := QualifierRecoveryClass(0); c < qualRecClassCount; c++ {
			total += counts[s][c]
		}
		if total < floors.Calls[s] {
			failed = true
			fmt.Fprintf(w, "FAIL: %s reported %d call(s), below its floor of %d — the site has gone\n"+
				"  DARK. A splitter at zero reads exactly like a splitter measured clean, and\n"+
				"  every zero this census asserts about it is worthless over an empty\n"+
				"  population. Either the shapes that drive it stopped being planned, or the\n"+
				"  recorder was dropped from the site.\n", s, total, floors.Calls[s])
		}
		split := 0
		for _, c := range []QualifierRecoveryClass{
			QualRecAgreed, QualRecDiverged, QualRecManufactured,
			QualRecLeafOnly, QualRecBare, QualRecHeuristicDecline,
		} {
			split += counts[s][c]
		}
		switch f := floors.Split[s]; {
		case f > 0 && split < f:
			failed = true
			fmt.Fprintf(w, "FAIL: %s entered a SPLITTING arm %d time(s), below its split floor of %d.\n"+
				"  Its total call count can stay healthy on CARRIED traffic alone, so this is\n"+
				"  the floor — and the only floor — that says the arms this census measures are\n"+
				"  still being reached.\n", s, split, f)
		case f == 0 && split > 0:
			failed = true
			fmt.Fprintf(w, "FAIL: %s declares a split floor of 0 — WATCHED, NOT PROVEN, meaning its\n"+
				"  splitting arms were measured empty over this corpus and are covered by a unit\n"+
				"  wiring pin instead. They are no longer empty: %d split call(s) now arrive\n"+
				"  here. This is not a defect, it is the DECLARATION GOING STALE. Re-read the\n"+
				"  site, then raise this floor to a real number so the corpus starts carrying\n"+
				"  the guarantee the unit pin was standing in for.\n", s, split)
		}
	}
	return failed
}

// ClassifyQualifierRecovery is the shared classifier for a site that splits a
// rendered name and MAY hold a structured qualifier for the same reference.
//
// It exists so the six sites cannot drift in how they bucket the same situation.
// A site that classified by hand would be free to call its own disagreement
// "manufactured" (no counterparty) rather than "DIVERGED", which is precisely
// the reading that would make the hard zero pass while the misread continued.
//
// `identity` is the structured qualifier the site holds, or "" when it holds
// none — and holding none is a different fact from holding an EMPTY one, which
// is why an unqualified structured identity must be passed as its own signal via
// identityPresent.
func ClassifyQualifierRecovery(name, identity string, identityPresent bool) (QualifierRecoveryClass, string) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot+1 >= len(name) {
		return QualRecBare, ""
	}
	manufactured := name[:dot]
	if !identityPresent {
		return QualRecManufactured, manufactured
	}
	if strings.EqualFold(manufactured, identity) {
		return QualRecAgreed, manufactured
	}
	return QualRecDiverged, manufactured
}
