package executor

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The LEG-COLUMN PROVENANCE census.
//
// It measures ONE question at `rowSlotForLegColumn`, the executor's last live
// reader of a DOTTED leg-qualified column name: when the dotted arm answers,
// was an IDENTITY-keyed alternative available at the reader?
//
// The arm matches `RecordTypeLeg.Name` — a TEXT field — against the qualifier it
// splits out of the column name (`strings.EqualFold(leg.Name, qual)`), then the
// column within that leg's window by name again. RFC-197's position is that a
// name may not decide a column's identity, so the question that decides whether
// this reader can be re-keyed is not "how often does it fire" but "when it fires,
// does the leg it matched also carry a stated ALIAS" — a
// values.CorrelationIdentifier, which is what an identity key would use.
//
// The two are different facts and they come apart in both directions:
//
//   - a leg can carry a Name and NO Alias (an unstated identity), in which case
//     an identity-keyed reader would MISS where this one hits, and re-keying the
//     site would be a silent behaviour change rather than a refactor;
//   - a leg can carry an Alias whose Name() DIFFERS from its Name text, in which
//     case the two keys already disagree and only one of them is right.
//
// Both are counted, because "the reader can be re-keyed" is exactly the claim
// that neither of them holds — and the retirement of the leg-name channel rests
// on that claim rather than on the shape of the lookup.
//
// WHAT IT MEASURED, and what that does and does not license. Whole real-FDB
// sqldriver corpus:
//
//	calls 52 (flatHit 40, notDotted 8, noLegs 0, dottedMiss 0)
//	dotted HITS by identity availability: available 4, unstated 0, diverged 0
//	witnesses: "C.CV", "I.QTY", "O.ID"
//
// So the dotted arm answers FOUR times in the whole corpus, and every one of the
// four matched a leg that also states an identity naming the same thing. The leg
// TABLE is ready to be keyed by identity.
//
// IT DOES NOT FOLLOW THAT THIS READER CAN BE, and the distinction is the finding
// rather than a caveat. The qualifier this arm matches on is TEXT that it SPLIT
// OUT of a column name — `name[:di]` — so keying the comparison by identity
// would mean minting a CorrelationIdentifier from that text, which is the
// forgery RFC-197 exists to remove, or comparing `leg.Alias.Name()` to it, which
// is the same text comparison against a different field. The name channel here
// is the INPUT, not the lookup: `adaptLegPositional` passes
// `legType.Fields[i].Name`, and it is dotted because a producer built a leg type
// whose column names carry a qualifier. This reader retires when that producer
// does, producer-first — the four hits above are what a re-keyed reader would
// have to keep answering, and the zeros are what says nothing else is in the way.
//
// GATED by values.LegIdentityCensusEnabled, like every other census on this
// path: a disabled census costs the reader one predicate. The counts are per
// CALL, and the caller (adaptLegPositional) calls once per leg-type column per
// adapted row, so read the absolute numbers as traffic and the SHARES as the
// facts about the corpus.
const legColumnProvenanceWitnessCap = 128

type legColumnProvenanceCounters struct {
	// Calls is every rowSlotForLegColumn invocation.
	Calls int
	// FlatHit: the row's own type declares the name directly, so the dotted arm
	// was never consulted. The overwhelmingly common case, and the one that
	// retires for free.
	FlatHit int
	// NotDotted: the flat lookup missed and the name carries no qualifier, so
	// there was nothing for the dotted arm to try. A miss, not a name decision.
	NotDotted int
	// NoLegs: dotted, but the row's type carries no leg table at all.
	NoLegs int
	// DottedHitIdentityAvailable: the dotted arm ANSWERED and the leg it matched
	// carries a STATED alias whose Name() equals the qualifier it matched on.
	// These are the calls an identity-keyed reader would answer identically —
	// the population whose re-keying is a refactor rather than a change.
	DottedHitIdentityAvailable int
	// DottedHitIdentityUnstated: the dotted arm ANSWERED but the leg it matched
	// carries NO alias. An identity-keyed reader would miss here, so this
	// population is what blocks re-keying, and it closes at the PRODUCER that
	// built the leg without stating its identity.
	DottedHitIdentityUnstated int
	// DottedHitIdentityDiverged: the dotted arm ANSWERED and the leg carries an
	// alias whose Name() DIFFERS from the qualifier. The two keys disagree, so
	// re-keying would resolve a different leg — the loudest of the three and the
	// reason the question is asked per call rather than per site.
	DottedHitIdentityDiverged int
	// DottedMiss: dotted, legs present, and no leg window declared the column.
	DottedMiss int
}

var (
	legColumnProvenanceMu        sync.Mutex
	legColumnProvenanceCounts    legColumnProvenanceCounters
	legColumnProvenanceWitnesses []string
)

// legColumnProvenanceClass is one call's bucket. The eight partition Calls.
type legColumnProvenanceClass int

const (
	legColumnProvenanceFlatHit legColumnProvenanceClass = iota
	legColumnProvenanceNotDotted
	legColumnProvenanceNoLegs
	legColumnProvenanceIdentityAvailable
	legColumnProvenanceIdentityUnstated
	legColumnProvenanceIdentityDiverged
	legColumnProvenanceMiss
)

// classifyLegColumnProvenance decides one call's bucket from the facts the
// reader itself has in hand. Split from the counter mutation so the decision can
// be exercised without touching process-global state, exactly as its siblings
// are, and for the same reason: a gate is a claim about which states fail.
//
// The ordering is the content. A FLAT hit dominates — the dotted arm was never
// reached, so nothing about the leg table can be held against it. Among dotted
// hits the IDENTITY question is asked last, because it is a question about the
// leg the arm ALREADY chose by name: asking it earlier would report the identity
// state of a leg no lookup selected.
func classifyLegColumnProvenance(rt *values.RecordType, name string, flatHit bool, matched *values.RecordTypeLeg, qualifier string) legColumnProvenanceClass {
	if flatHit {
		return legColumnProvenanceFlatHit
	}
	if strings.IndexByte(name, '.') <= 0 {
		return legColumnProvenanceNotDotted
	}
	if rt == nil || len(rt.Legs) == 0 {
		return legColumnProvenanceNoLegs
	}
	if matched == nil {
		return legColumnProvenanceMiss
	}
	if matched.Alias.IsZero() {
		return legColumnProvenanceIdentityUnstated
	}
	if !strings.EqualFold(matched.Alias.Name(), qualifier) {
		return legColumnProvenanceIdentityDiverged
	}
	return legColumnProvenanceIdentityAvailable
}

// recordLegColumnProvenance counts one rowSlotForLegColumn call. matched is the
// leg the dotted arm resolved the qualifier to, or nil.
func recordLegColumnProvenance(rt *values.RecordType, name string, flatHit bool, matched *values.RecordTypeLeg, qualifier string) {
	class := classifyLegColumnProvenance(rt, name, flatHit, matched, qualifier)
	legColumnProvenanceMu.Lock()
	defer legColumnProvenanceMu.Unlock()
	legColumnProvenanceCounts.Calls++
	switch class {
	case legColumnProvenanceFlatHit:
		legColumnProvenanceCounts.FlatHit++
	case legColumnProvenanceNotDotted:
		legColumnProvenanceCounts.NotDotted++
	case legColumnProvenanceNoLegs:
		legColumnProvenanceCounts.NoLegs++
	case legColumnProvenanceMiss:
		legColumnProvenanceCounts.DottedMiss++
		addLegColumnProvenanceWitness(fmt.Sprintf("DOTTED-MISS %q over legs %v", name, legNamesOf(rt)))
	case legColumnProvenanceIdentityUnstated:
		legColumnProvenanceCounts.DottedHitIdentityUnstated++
		addLegColumnProvenanceWitness(fmt.Sprintf("DOTTED-HIT-NO-IDENTITY %q: leg %q states no alias",
			name, matched.Name))
	case legColumnProvenanceIdentityDiverged:
		legColumnProvenanceCounts.DottedHitIdentityDiverged++
		addLegColumnProvenanceWitness(fmt.Sprintf("DOTTED-HIT-DIVERGED %q: leg text %q vs alias %q",
			name, matched.Name, matched.Alias.Name()))
	case legColumnProvenanceIdentityAvailable:
		legColumnProvenanceCounts.DottedHitIdentityAvailable++
		addLegColumnProvenanceWitness(fmt.Sprintf("DOTTED-HIT %q: leg %q, alias stated and equal",
			name, matched.Name))
	}
}

func legNamesOf(rt *values.RecordType) []string {
	if rt == nil {
		return nil
	}
	out := make([]string, 0, len(rt.Legs))
	for _, l := range rt.Legs {
		out = append(out, l.Name)
	}
	return out
}

func addLegColumnProvenanceWitness(w string) {
	if len(legColumnProvenanceWitnesses) >= legColumnProvenanceWitnessCap {
		return
	}
	for _, seen := range legColumnProvenanceWitnesses {
		if seen == w {
			return
		}
	}
	legColumnProvenanceWitnesses = append(legColumnProvenanceWitnesses, w)
}

// LegColumnProvenanceCensus reports the counters and the retained witnesses.
func LegColumnProvenanceCensus() (legColumnProvenanceCounters, []string) {
	legColumnProvenanceMu.Lock()
	defer legColumnProvenanceMu.Unlock()
	out := make([]string, len(legColumnProvenanceWitnesses))
	copy(out, legColumnProvenanceWitnesses)
	return legColumnProvenanceCounts, out
}

// FormatLegColumnProvenanceCensus renders the census for a harness to log.
func FormatLegColumnProvenanceCensus() string {
	c, witnesses := LegColumnProvenanceCensus()
	var b strings.Builder
	fmt.Fprintf(&b, "leg-column provenance: calls %d (flatHit %d, notDotted %d, noLegs %d, "+
		"dottedMiss %d); dotted HITS by identity availability: available %d, unstated %d, diverged %d",
		c.Calls, c.FlatHit, c.NotDotted, c.NoLegs, c.DottedMiss,
		c.DottedHitIdentityAvailable, c.DottedHitIdentityUnstated, c.DottedHitIdentityDiverged)
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  distinct witnesses (%d, cap %d):\n    %s",
			len(sorted), legColumnProvenanceWitnessCap, strings.Join(sorted, "\n    "))
	}
	return b.String()
}

// AssertLegColumnProvenanceCensus checks the census's partition and its one
// zero, and reports whether it failed.
//
// The partition is the point: every share this census prints is a share of
// Calls, and a share only means something if the shares add up. The zero is
// DottedHitIdentityDiverged — a leg whose text and whose stated identity name
// different things, resolved by the text. That is not a residue to shrink, it is
// a contradiction: two keys for one leg, disagreeing, with only the weaker one
// consulted.
func AssertLegColumnProvenanceCensus(w io.Writer) bool {
	c, _ := LegColumnProvenanceCensus()
	return assertLegColumnProvenanceCounters(w, c)
}

func assertLegColumnProvenanceCounters(w io.Writer, c legColumnProvenanceCounters) bool {
	failed := false
	got := c.FlatHit + c.NotDotted + c.NoLegs + c.DottedMiss +
		c.DottedHitIdentityAvailable + c.DottedHitIdentityUnstated + c.DottedHitIdentityDiverged
	if got != c.Calls {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: the seven outcomes sum to %d, "+
			"but Calls = %d.\n"+
			"  They are the only things one lookup can do, so they must partition it. A\n"+
			"  gap means a call left the reader by a path with no counter on it, and every\n"+
			"  share below — including the one that decides whether this reader can be\n"+
			"  re-keyed by identity — is then a share of an unknown whole.\n", got, c.Calls)
	}
	if c.DottedHitIdentityDiverged != 0 {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: DottedHitIdentityDiverged = %d, want 0.\n"+
			"  A leg's NAME text and its stated ALIAS named different things, and the\n"+
			"  lookup resolved on the text. Those are two keys for one leg and they\n"+
			"  disagree, so one of them is already resolving to the wrong window — this is\n"+
			"  not a residue to shrink but a contradiction to find. Look at the PRODUCER\n"+
			"  that built the leg, not at this reader.\n", c.DottedHitIdentityDiverged)
	}
	return failed
}
