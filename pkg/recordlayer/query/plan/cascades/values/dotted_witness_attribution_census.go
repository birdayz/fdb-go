package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Dotted-witness attribution joins reader observations to producer registrations
// by correlation and title; matching names alone cannot establish provenance.

// mintedLeg is one inner-leg registration: who minted it, and the title given.
type mintedLeg struct {
	producer InnerLegProducer
	title    string
}

// attributionState separates decision inputs from process globals so tests can
// drive attribution cases that the corpus does not reach.
type attributionState struct {
	// minted maps a correlation to the producer that minted its inner leg and
	// the title that leg's flowed column was given.
	minted map[string]mintedLeg
	// observed maps a name the dotted arm answered on to the OWNER correlation
	// the reader held.
	observed map[string]string
	// collisions records a name answered under a SECOND owner. See
	// RecordDottedArmAnswer.
	collisions []string
}

var (
	dottedAttrMu    sync.Mutex
	dottedAttrState attributionState
)

// InnerLegProducer names WHICH correlated-scalar seed minted an inner leg.
//
// There are TWO, and RFC-212 §10.3 v1 named only one of them — which is exactly
// the error this census exists to catch, so the producer is recorded rather than
// assumed.
type InnerLegProducer int

const (
	// InnerLegProducerClusteredOuter is clusteredOuterOrdinalSeed, serving the
	// GATED MULTI-TABLE outer.
	InnerLegProducerClusteredOuter InnerLegProducer = iota
	// InnerLegProducerSingleSource is scalarSubqueryOrdinalSeed, serving the
	// SINGLE-SOURCE outer (clusterArity == 1).
	InnerLegProducerSingleSource
)

func (p InnerLegProducer) String() string {
	switch p {
	case InnerLegProducerClusteredOuter:
		return "clusteredOuterOrdinalSeed"
	case InnerLegProducerSingleSource:
		return "scalarSubqueryOrdinalSeed"
	}
	return "unknown"
}

// RecordInnerScalarLegTitleAt registers one (correlation, title) pair minted for
// a correlated-scalar seed inner leg, naming the producer. Callers must guard on
// LegIdentityCensusEnabled().
func RecordInnerScalarLegTitleAt(producer InnerLegProducer, corr CorrelationIdentifier, title string) {
	if corr.IsZero() || title == "" {
		return
	}
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	if dottedAttrState.minted == nil {
		dottedAttrState.minted = map[string]mintedLeg{}
	}
	dottedAttrState.minted[corr.Name()] = mintedLeg{producer: producer, title: title}
}

// RecordDottedArmAnswer records one name the executor's dotted arm answered on,
// with the OWNER correlation the reader held. Callers must guard on
// LegIdentityCensusEnabled().
//
// First writer wins for the map, and a LATER, DIFFERENT owner for the same name
// is RECORDED as a collision rather than dropped. That is not tidiness: a name
// answered under two owners, one attributed and one not, would otherwise report
// as cleanly attributed and the second owner would never appear anywhere. The
// census's whole claim is per-name, so a name with two owners invalidates the
// claim for that name.
func RecordDottedArmAnswer(name string, owner CorrelationIdentifier) {
	if name == "" {
		return
	}
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	if dottedAttrState.observed == nil {
		dottedAttrState.observed = map[string]string{}
	}
	prev, seen := dottedAttrState.observed[name]
	switch {
	case !seen:
		dottedAttrState.observed[name] = owner.Name()
	case prev != owner.Name():
		c := fmt.Sprintf("%s answered under owner %s AND owner %s", name, prev, owner.Name())
		for _, existing := range dottedAttrState.collisions {
			if existing == c {
				return
			}
		}
		dottedAttrState.collisions = append(dottedAttrState.collisions, c)
	}
}

// ResetDottedWitnessAttribution clears the census.
func ResetDottedWitnessAttribution() {
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	dottedAttrState = attributionState{}
}

func snapshotAttribution() attributionState {
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	out := attributionState{
		minted:     make(map[string]mintedLeg, len(dottedAttrState.minted)),
		observed:   make(map[string]string, len(dottedAttrState.observed)),
		collisions: append([]string(nil), dottedAttrState.collisions...),
	}
	for k, v := range dottedAttrState.minted {
		out.minted[k] = v
	}
	for k, v := range dottedAttrState.observed {
		out.observed[k] = v
	}
	return out
}

// DottedWitnessAttribution reports, per observed name, whether it is attributed
// to a correlated-scalar seed inner leg BY IDENTITY.
func DottedWitnessAttribution() (attributed, unattributed []string, mintedCount int) {
	a, u, m, _ := classifyAttribution(snapshotAttribution())
	return a, u, m
}

// classifyAttribution is the three-way partition, over EXPLICIT state.
//
// The three arms are genuinely different findings and the middle one is the
// reason attribution is keyed by owner rather than by name:
//
//   - ATTRIBUTED: the owner IS a minted inner leg and the title it was minted
//     with IS the name the reader answered on.
//   - RETITLED: the owner is a minted inner leg and the title is NOT that name —
//     the leg was retitled or renamed between mint and read. A name-only matcher
//     has no such state and cannot express this arm at all.
//   - THIRD PRODUCER: the owner was minted by neither seed.
func classifyAttribution(s attributionState) (attributed, unattributed []string, mintedCount int, collisions []string) {
	mintedCount = len(s.minted)
	for name, owner := range s.observed {
		m, minted := s.minted[owner]
		switch {
		case minted && m.title == name:
			attributed = append(attributed,
				fmt.Sprintf("%s (owner %s) -> %s, minted title %q", name, owner, m.producer, m.title))
		case minted:
			unattributed = append(unattributed,
				fmt.Sprintf("%s (owner %s IS an inner leg minted by %s, but its title is %q — "+
					"the owner matches and the NAME does not, so this leg was retitled or "+
					"renamed between mint and read)", name, owner, m.producer, m.title))
		default:
			unattributed = append(unattributed,
				fmt.Sprintf("%s (owner %s was minted by NEITHER correlated-scalar seed — a "+
					"third producer names this leg type's column)", name, owner))
		}
	}
	sort.Strings(attributed)
	sort.Strings(unattributed)
	return attributed, unattributed, mintedCount, append([]string(nil), s.collisions...)
}

// FormatDottedWitnessAttribution renders the census for a harness to log.
func FormatDottedWitnessAttribution() string {
	return formatAttribution(snapshotAttribution())
}

func formatAttribution(s attributionState) string {
	attributed, unattributed, minted, collisions := classifyAttribution(s)
	var b strings.Builder
	fmt.Fprintf(&b, "dotted-witness attribution (RFC-212 §10.3 deliverable 1): "+
		"inner-leg titles minted %d; dotted-arm names observed %d",
		minted, len(attributed)+len(unattributed))
	if len(attributed) > 0 {
		// The producer is named PER ROW, never in this header: there are two seeds
		// and naming one here would state a conclusion the rows may contradict.
		// Substituting the currently-right one would go stale the first time a
		// third producer attributed; omitting it removes the class.
		fmt.Fprintf(&b, "\n  ATTRIBUTED to a correlated-scalar seed inner leg (%d):\n    %s",
			len(attributed), strings.Join(attributed, "\n    "))
	}
	if len(unattributed) > 0 {
		fmt.Fprintf(&b, "\n  NOT attributed (%d):\n    %s",
			len(unattributed), strings.Join(unattributed, "\n    "))
	}
	if len(attributed)+len(unattributed) == 0 {
		b.WriteString("\n  NO dotted-arm answers observed — the attribution is VACUOUS and " +
			"decides nothing; see the floors below.")
	}
	if len(collisions) > 0 {
		sorted := append([]string(nil), collisions...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  OWNER COLLISIONS (%d) — the per-name claim does not hold for these:\n    %s",
			len(sorted), strings.Join(sorted, "\n    "))
	}
	return b.String()
}

// DottedWitnessFloors are the two populations that must be non-trivial for this
// census's finding to mean anything.
type DottedWitnessFloors struct {
	// Observed floors the dotted-arm names. A partition over an empty observed
	// population reads exactly like a decided one.
	Observed int
	// Minted floors the registrations. A NOT-attributed finding is load-bearing
	// only when the producers minted titles on that run, so the instrument was
	// demonstrably live. A run minting ZERO reports NOT attributed for every
	// name, vacuously, and looks identical to a real refutation.
	Minted int
}

// AssertDottedWitnessAttribution checks both floors and the owner-collision zero.
func AssertDottedWitnessAttribution(w io.Writer, floors *DottedWitnessFloors) bool {
	return assertAttribution(w, snapshotAttribution(), floors)
}

func assertAttribution(w io.Writer, s attributionState, floors *DottedWitnessFloors) bool {
	attributed, unattributed, minted, collisions := classifyAttribution(s)
	observed := len(attributed) + len(unattributed)
	failed := false

	if len(collisions) > 0 {
		failed = true
		fmt.Fprintf(w, "DOTTED-WITNESS ATTRIBUTION FAIL: %d name(s) answered under MORE THAN ONE\n"+
			"  owner: %s\n"+
			"  This census's claim is PER NAME — 'the leg type carrying this name was built\n"+
			"  by producer X' — and a name with two owners has two answers, of which the\n"+
			"  report shows one. First-writer-wins would have printed such a name as\n"+
			"  cleanly attributed while its second owner appeared nowhere.\n"+
			"  WHAT THIS RE-ARMS: the scoping decision. 'Both witnesses share one producer'\n"+
			"  is only true if each witness HAS one producer.\n",
			len(collisions), strings.Join(collisions, "; "))
	}
	if floors == nil {
		return failed
	}
	if floors.Observed > 0 && observed < floors.Observed {
		failed = true
		fmt.Fprintf(w, "DOTTED-WITNESS ATTRIBUTION FAIL: %d dotted-arm name(s) observed, want >= %d\n"+
			"  (inner-leg titles minted this run: %d).\n"+
			"  The finding is a PARTITION of the dotted-arm names, and a partition over an\n"+
			"  empty observed population is vacuous while printing exactly like a decided one.\n"+
			"  WHAT A COLLAPSE RE-ARMS: the scoping decision. With no reading, 'both\n"+
			"  witnesses retire together' becomes an assumption again — the corollary error\n"+
			"  that already cost this workstream a full implementation cycle.\n",
			observed, floors.Observed, minted)
	}
	if floors.Minted > 0 && minted < floors.Minted {
		failed = true
		fmt.Fprintf(w, "DOTTED-WITNESS ATTRIBUTION FAIL: %d inner-leg title(s) minted, want >= %d\n"+
			"  (dotted-arm names observed this run: %d).\n"+
			"  This is the direction a NOT-attributed finding rests on and it was read from a\n"+
			"  log rather than asserted: the round that refuted RFC-212 §10.3 v1 was load\n"+
			"  bearing ONLY because the producers minted titles on that run, so the\n"+
			"  instrument was demonstrably live.\n"+
			"  WHAT A COLLAPSE RE-ARMS: every NOT-attributed row. With zero registrations\n"+
			"  every name reports as unattributed, vacuously, and reads identically to a real\n"+
			"  refutation of whichever producer the RFC currently names.\n",
			minted, floors.Minted, observed)
	}
	return failed
}
