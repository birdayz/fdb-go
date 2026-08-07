package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// THE DOTTED-WITNESS ATTRIBUTION census — RFC-212 §10.3's DELIVERABLE 1, and it
// gates the retitling it precedes.
//
// The executor's dotted leg-column arm answers on exactly two names over the
// real-FDB corpus, `C.CV` and `I.QTY`, and the erratum deliberately refused to
// claim they share a producer: the leg-column provenance census reports them
// under DIFFERENT owners (`q$3122` over `[C E]`, `q$236051` over `[O I]`), and
// asserting they retire together would be the same true-measurement /
// false-corollary error that killed §1.1's first target.
//
// So this measures it instead of assuming it. The question is narrow:
//
//	for each name the dotted arm answers, WHICH producer built the leg type
//	that carried it?
//
// ATTRIBUTION IS BY IDENTITY, NOT BY NAME MATCH, which is the whole point of
// doing it this way. The producer registers the (correlation, title) pair it
// mints; the reader reports the OWNER correlation it was handed. A hit requires
// the owner to BE a correlation this producer minted AND the name to be the
// title it minted for that correlation. Matching on the name alone would prove
// only that two strings agree, which is how a corollary gets mistaken for a
// measurement.
//
// The answer scopes the retitling:
//
//   - BOTH attributed → the retitling retires the whole arm, day-one scope is
//     the whole arm.
//   - ONE attributed → scope to that one; the other gets its own producer hunt
//     and its own booking. The convenient answer must not become the assumed one.
//   - NEITHER → the corrected target is wrong too, and that is a third refutation
//     rather than a surprise.
//
// WHAT IT MEASURED — TWO ROUNDS, and the first one is kept because it is the
// finding, not a false start.
//
// ROUND 1 instrumented only `clusteredOuterOrdinalSeed`, the producer RFC-212
// §10.3 v1 named. Whole real-FDB sqldriver corpus, uncached, EXIT=0:
//
//	inner-leg titles minted 19; dotted-arm names observed 2
//	  NOT attributed (2):
//	    C.CV (owner q$3122 ...), I.QTY (owner q$395174 ...)
//
// NEITHER witness came from it, over a run where it minted 19 titles — so the
// instrument was live and the zero was a reading, not an absence. That refuted
// §10.3 v1 BEFORE any retitling was written, which is the entire purpose of
// gating the conversion on this deliverable.
//
// ROUND 2 instrumented BOTH seeds. There are two: `clusteredOuterOrdinalSeed`
// serves the GATED MULTI-TABLE outer, and `scalarSubqueryOrdinalSeed`
// (scalar_subquery_seed.go) serves the SINGLE-SOURCE outer (clusterArity == 1) —
// the one clustered_outer_scalar.go's own comment calls "the single-source
// seed". Same corpus, uncached, EXIT=0:
//
//	inner-leg titles minted 274; dotted-arm names observed 2
//	  ATTRIBUTED to a correlated-scalar seed inner leg (2):
//	    C.CV (owner q$3122) -> scalarSubqueryOrdinalSeed, minted title "C.CV"
//	    I.QTY (owner q$336732) -> scalarSubqueryOrdinalSeed, minted title "I.QTY"
//
// BOTH witnesses attribute to `scalarSubqueryOrdinalSeed`, by IDENTITY. So the
// retitling target is a producer SWAP from §10.3 v1, and the scope question is
// answered in the same breath: both witnesses share one producer, so the
// retitling retires the whole arm rather than half of it.
//
// The machine-minted `q$N` counter differs between runs (q$395174 in round 1,
// q$336732 in round 2 for the same logical leg), which is why attribution is
// computed WITHIN a run and never by quoting an id across runs.
//
// GATED by LegIdentityCensusEnabled, like every census on this path.

// mintedLeg is one inner-leg registration: who minted it, and the title given.
type mintedLeg struct {
	producer InnerLegProducer
	title    string
}

var (
	dottedAttrMu sync.Mutex
	// dottedAttrMinted maps a correlation minted by EITHER correlated-scalar seed
	// to the producer that minted it and the TITLE its flowed column was given.
	dottedAttrMinted map[string]mintedLeg
	// dottedAttrObserved records one dotted-arm answer: the name, and the owner
	// correlation the reader held.
	dottedAttrObserved map[string]string
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
	if dottedAttrMinted == nil {
		dottedAttrMinted = map[string]mintedLeg{}
	}
	dottedAttrMinted[corr.Name()] = mintedLeg{producer: producer, title: title}
}

// RecordDottedArmAnswer records one name the executor's dotted arm answered on,
// with the OWNER correlation the reader held. Callers must guard on
// LegIdentityCensusEnabled().
func RecordDottedArmAnswer(name string, owner CorrelationIdentifier) {
	if name == "" {
		return
	}
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	if dottedAttrObserved == nil {
		dottedAttrObserved = map[string]string{}
	}
	// First writer wins: the population is two names, and a later owner for the
	// same name would itself be a finding rather than an overwrite.
	if _, seen := dottedAttrObserved[name]; !seen {
		dottedAttrObserved[name] = owner.Name()
	}
}

// ResetDottedWitnessAttribution clears the census.
func ResetDottedWitnessAttribution() {
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	dottedAttrMinted, dottedAttrObserved = nil, nil
}

// DottedWitnessAttribution reports, per observed name, whether it is attributed
// to the correlated-scalar seed's inner leg BY IDENTITY.
func DottedWitnessAttribution() (attributed, unattributed []string, mintedCount int) {
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	mintedCount = len(dottedAttrMinted)
	for name, owner := range dottedAttrObserved {
		m, minted := dottedAttrMinted[owner]
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
	return attributed, unattributed, mintedCount
}

// FormatDottedWitnessAttribution renders the census for a harness to log.
func FormatDottedWitnessAttribution() string {
	attributed, unattributed, minted := DottedWitnessAttribution()
	var b strings.Builder
	fmt.Fprintf(&b, "dotted-witness attribution (RFC-212 §10.3 deliverable 1): "+
		"inner-leg titles minted %d; dotted-arm names observed %d",
		minted, len(attributed)+len(unattributed))
	if len(attributed) > 0 {
		// The producer is named PER ROW, never in this header: there are two seeds
		// and naming one here would state a conclusion the rows may contradict.
		fmt.Fprintf(&b, "\n  ATTRIBUTED to a correlated-scalar seed inner leg (%d):\n    %s",
			len(attributed), strings.Join(attributed, "\n    "))
	}
	if len(unattributed) > 0 {
		fmt.Fprintf(&b, "\n  NOT attributed (%d):\n    %s",
			len(unattributed), strings.Join(unattributed, "\n    "))
	}
	if len(attributed)+len(unattributed) == 0 {
		b.WriteString("\n  NO dotted-arm answers observed — the attribution is VACUOUS and " +
			"decides nothing; see the floor below.")
	}
	return b.String()
}

// AssertDottedWitnessAttribution floors the OBSERVED population.
//
// The finding this census produces is a partition of the dotted-arm names, and a
// partition over an empty population reads exactly like a clean one. The floor is
// the observed-name count, not the minted count: a run where the producer minted
// titles but the reader answered on none of them decides nothing about scope,
// which is the only question this census exists to settle.
func AssertDottedWitnessAttribution(w io.Writer, floor int) bool {
	attributed, unattributed, minted := DottedWitnessAttribution()
	if floor <= 0 || len(attributed)+len(unattributed) >= floor {
		return false
	}
	fmt.Fprintf(w, "DOTTED-WITNESS ATTRIBUTION FAIL: %d dotted-arm name(s) observed, want >= %d\n"+
		"  (inner-leg titles minted this run: %d).\n"+
		"  This census answers ONE question — whether the names the executor's dotted arm\n"+
		"  answers on originate at clusteredOuterOrdinalSeed's inner scalar leg — and that\n"+
		"  answer is what scopes RFC-212 §10.3's retitling. Over an empty observed\n"+
		"  population the partition is vacuous and prints exactly like a decided one.\n"+
		"  WHAT A COLLAPSE RE-ARMS: the scoping decision. With no reading, 'both witnesses\n"+
		"  retire together' becomes an assumption again — which is the corollary error\n"+
		"  that already cost this workstream a full implementation cycle.\n",
		len(attributed)+len(unattributed), floor, minted)
	return true
}
