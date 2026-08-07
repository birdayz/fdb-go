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
//	for each name the dotted arm answers, was the leg type that carried it
//	built by clusteredOuterOrdinalSeed's INNER SCALAR LEG?
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
// WHAT IT MEASURED, and it is the third branch. Whole real-FDB sqldriver corpus,
// uncached, EXIT=0:
//
//	dotted-witness attribution (RFC-212 §10.3 deliverable 1): inner-leg titles minted 19; dotted-arm names observed 2
//	  NOT attributed (2):
//	    C.CV (owner q$3122 was NOT minted by clusteredOuterOrdinalSeed's inner leg — a different producer names this leg type's column)
//	    I.QTY (owner q$395174 was NOT minted by clusteredOuterOrdinalSeed's inner leg — a different producer names this leg type's column)
//
// NEITHER witness originates at `clusteredOuterOrdinalSeed`. That producer minted
// 19 inner-leg titles over the same run, so the instrument is live and the zero
// is a reading rather than an absence — the two populations simply do not meet.
//
// The reason is that there are TWO correlated-scalar seeds, and RFC-212 §10.3
// named the wrong one. `clusteredOuterOrdinalSeed` serves the GATED MULTI-TABLE
// outer; the SINGLE-SOURCE outer (`clusterArity == 1`) is served by
// `scalarSubqueryOrdinalSeed` in cascades_translator.go, which
// clustered_outer_scalar.go's own comment calls "the single-source seed" and
// whose inner leg is built the same way. The dotted arm's two witnesses come
// from that one.
//
// So the retitling target is right in KIND — a producer naming a quantifier's
// flowed column with a title that can contain a dot — and wrong in WHICH
// PRODUCER. §10.3 must be corrected again before implementation, and the scope
// question it asked ("do both witnesses retire together?") is unanswered until
// the attribution is re-run against the right producer. THIS IS EXACTLY WHY THE
// DELIVERABLE GATES THE CONVERSION: retitling `clusteredOuterOrdinalSeed` would
// have moved nothing, for the third time.
//
// GATED by LegIdentityCensusEnabled, like every census on this path.

var (
	dottedAttrMu sync.Mutex
	// dottedAttrMinted maps a correlation minted by the correlated-scalar seed's
	// inner leg to the TITLE that leg's flowed column was given.
	dottedAttrMinted map[string]string
	// dottedAttrObserved records one dotted-arm answer: the name, and the owner
	// correlation the reader held.
	dottedAttrObserved map[string]string
)

// RecordInnerScalarLegTitle registers one (correlation, title) pair minted for a
// correlated-scalar seed's inner leg flowed column. Callers must guard on
// LegIdentityCensusEnabled().
func RecordInnerScalarLegTitle(corr CorrelationIdentifier, title string) {
	if corr.IsZero() || title == "" {
		return
	}
	dottedAttrMu.Lock()
	defer dottedAttrMu.Unlock()
	if dottedAttrMinted == nil {
		dottedAttrMinted = map[string]string{}
	}
	dottedAttrMinted[corr.Name()] = title
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
		title, minted := dottedAttrMinted[owner]
		switch {
		case minted && title == name:
			attributed = append(attributed,
				fmt.Sprintf("%s (owner %s, minted title %q)", name, owner, title))
		case minted:
			unattributed = append(unattributed,
				fmt.Sprintf("%s (owner %s IS a seed inner leg but its title is %q — the "+
					"owner matches and the NAME does not, so this leg was retitled or "+
					"renamed between mint and read)", name, owner, title))
		default:
			unattributed = append(unattributed,
				fmt.Sprintf("%s (owner %s was NOT minted by clusteredOuterOrdinalSeed's "+
					"inner leg — a different producer names this leg type's column)",
					name, owner))
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
		fmt.Fprintf(&b, "\n  ATTRIBUTED to clusteredOuterOrdinalSeed's inner scalar leg (%d):\n    %s",
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
