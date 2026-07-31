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
// THE OBVIOUS ESCAPE WAS TRIED AND MEASURED SHUT. The proposal: hand the reader
// the IDENTITY of the leg it is adapting a row FOR — every call site holds one —
// and select the source window by `Alias == identity` instead of by the
// qualifier. That would take the name out of SELECTION without touching a
// producer, so no label could move. The owner sub-partition below is what it
// measured, over the same corpus:
//
//	dotted HITS by OWNER selection: sameLeg 0, ownerUnstated 0,
//	                                ownerNamesNoLeg 4, ownerSelectsOtherLeg 0
//
// Four of four MISS. The identity a call site holds is the DESTINATION leg's own
// correlation — the correlated-scalar seed's machine-minted `q$NNN` — while the
// qualifier names a leg of the SOURCE merged row, which is a user alias. Those
// namespaces are deliberately case-DISJOINT (values.SameLeg says why), so nothing
// bridges them. Selecting by owner resolves no window, the gather matches zero
// columns, and adaptLegPositional's merge-shaped zero-match tripwire fires.
//
// And no third channel is available: the destination leg types that drive this
// arm are single-column and carry NO leg table of their own, so the qualifier
// inside the column name is the only thing here that names the source leg. The
// carrier an identity-keyed selection needs has to be PUT there by the producer —
// which is the producer-first conclusion above, arrived at a second way.
// leg_column_owner_selection_test.go pins both halves.
//
// WHERE THE ANSWER IS DROPPED, precisely, because it is not missing upstream —
// only here. The seed builds each leg column as
// `RecordConstructorField{Name: leg.binding+"."+COL, Value: FieldValueOfOrdinal(
// outerQOV, leg.start+i)}` (clustered_outer_scalar.go). The VALUE already states
// the source correlation and the ABSOLUTE slot — `leg.start+i` is the very
// arithmetic this reader re-derives from the label. It is
// RecordConstructorValue.Type() that throws it away: it synthesises
// `&RecordType{Nullable: true, Fields: fields}` carrying Name/FieldType/Ordinal
// and no leg table, and that stripped type is what widenLegTypesFromPlan stores
// and adaptLegPositional adapts against.
//
// So the reader is recomputing by text something the producer had as an ordinal
// two hops earlier. Restoring it is additive — RecordType.Legs is layout metadata
// that Equals and Hash IGNORE, so populating it moves no name, no label and no
// type identity (pinned in leg_column_owner_selection_test.go). That is the
// DERIVED-TYPE arm of the RecordConstructorField.Name triple, and it is separable
// from the row-key and result-label arms: this reader needs only the derived type
// to carry what the Value already knows.
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

	// The OWNER sub-partition, over dotted HITS only. The counters above ask
	// whether the leg the TEXT chose also states an identity; these ask the
	// question the conversion actually turns on, which is a different one:
	// would selecting the source window by the identity the READER already
	// holds have chosen that same leg?
	//
	// The two come apart because the identity in hand at the reader is the
	// OWNER — the correlation adaptLegPositional is adapting a row FOR — while
	// the qualifier names whatever leg the producer wrote into the column text.
	// Nothing structurally forces those to be the same leg, and if they are not,
	// an identity-keyed selection resolves a different window and the conversion
	// is a behaviour change rather than a refactor. Measured, not assumed.
	//
	// They sum to the three dotted-hit counters above; the assertion checks it.

	// DottedHitOwnerSelectsSameLeg: the owner identity names a leg of this row
	// and it is the leg the text chose. The population whose conversion is a
	// refactor.
	DottedHitOwnerSelectsSameLeg int
	// DottedHitOwnerUnstated: the reader was handed no identity at all (the zero
	// CorrelationIdentifier), so there is nothing to select by and the site
	// cannot convert until its caller states one.
	DottedHitOwnerUnstated int
	// DottedHitOwnerNamesNoLeg: the owner is stated but names no leg of this
	// row. An identity-keyed selection would find no window and MISS where the
	// text hits.
	DottedHitOwnerNamesNoLeg int
	// DottedHitOwnerSelectsOtherLeg: the owner names a leg of this row and it is
	// a DIFFERENT leg than the text chose. The loudest outcome — the two keys
	// disagree about which window the column lives in, so exactly one of them is
	// reading the right row.
	DottedHitOwnerSelectsOtherLeg int
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

// legColumnOwnerClass is one dotted HIT's owner-selection bucket: what a reader
// that picked the source window by the identity it already holds would have done.
type legColumnOwnerClass int

const (
	legColumnOwnerSameLeg legColumnOwnerClass = iota
	legColumnOwnerUnstated
	legColumnOwnerNamesNoLeg
	legColumnOwnerOtherLeg
)

// classifyLegColumnOwner decides what an IDENTITY-keyed selection would have
// resolved to, against what the text-keyed one did.
//
// Selection goes through values.SameLeg, the one leg-identity authority, rather
// than through a name comparison on Alias.Name() — comparing the alias's text
// would be the same text lookup wearing a different field, which is the move
// this conversion exists to remove.
func classifyLegColumnOwner(rt *values.RecordType, matched *values.RecordTypeLeg, owner values.CorrelationIdentifier) legColumnOwnerClass {
	if owner.IsZero() {
		return legColumnOwnerUnstated
	}
	for i := range rt.Legs {
		if !values.SameLeg(rt.Legs[i].Alias, owner) {
			continue
		}
		if rt.Legs[i].Start == matched.Start && rt.Legs[i].Width == matched.Width {
			return legColumnOwnerSameLeg
		}
		return legColumnOwnerOtherLeg
	}
	return legColumnOwnerNamesNoLeg
}

// recordLegColumnProvenance counts one rowSlotForLegColumn call. matched is the
// leg the dotted arm resolved the qualifier to, or nil. owner is the identity the
// reader was handed, recorded so the conversion's precondition is measured rather
// than asserted.
func recordLegColumnProvenance(rt *values.RecordType, name string, flatHit bool, matched *values.RecordTypeLeg, qualifier string, owner values.CorrelationIdentifier) {
	class := classifyLegColumnProvenance(rt, name, flatHit, matched, qualifier)
	legColumnProvenanceMu.Lock()
	defer legColumnProvenanceMu.Unlock()
	legColumnProvenanceCounts.Calls++
	switch class {
	case legColumnProvenanceIdentityAvailable, legColumnProvenanceIdentityUnstated, legColumnProvenanceIdentityDiverged:
		switch classifyLegColumnOwner(rt, matched, owner) {
		case legColumnOwnerSameLeg:
			legColumnProvenanceCounts.DottedHitOwnerSelectsSameLeg++
			addLegColumnProvenanceWitness(fmt.Sprintf("OWNER-SAME %q: owner %q selects the leg the text chose (%q)",
				name, owner.Name(), matched.Name))
		case legColumnOwnerUnstated:
			legColumnProvenanceCounts.DottedHitOwnerUnstated++
			addLegColumnProvenanceWitness(fmt.Sprintf("OWNER-UNSTATED %q: the reader holds no identity", name))
		case legColumnOwnerNamesNoLeg:
			legColumnProvenanceCounts.DottedHitOwnerNamesNoLeg++
			addLegColumnProvenanceWitness(fmt.Sprintf("OWNER-NO-LEG %q: owner %q names no leg of %v",
				name, owner.Name(), legNamesOf(rt)))
		case legColumnOwnerOtherLeg:
			legColumnProvenanceCounts.DottedHitOwnerSelectsOtherLeg++
			addLegColumnProvenanceWitness(fmt.Sprintf("OWNER-OTHER %q: owner %q selects a DIFFERENT leg than the text's %q",
				name, owner.Name(), matched.Name))
		}
	}
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
	fmt.Fprintf(&b, "\n  dotted HITS by OWNER selection: sameLeg %d, ownerUnstated %d, "+
		"ownerNamesNoLeg %d, ownerSelectsOtherLeg %d",
		c.DottedHitOwnerSelectsSameLeg, c.DottedHitOwnerUnstated,
		c.DottedHitOwnerNamesNoLeg, c.DottedHitOwnerSelectsOtherLeg)
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  distinct witnesses (%d, cap %d):\n    %s",
			len(sorted), legColumnProvenanceWitnessCap, strings.Join(sorted, "\n    "))
	}
	return b.String()
}

// LegColumnProvenanceFloors is the minimum population this census must report
// over a whole suite run.
//
// It exists for the reason its two siblings' floors exist, and the reason is
// sharper here than for either of them. This census's entire finding is a pair
// of small numbers — the dotted arm answers FOUR times in the whole corpus, and
// all four have an identity available — and the retirement decision rests on
// BOTH halves: on the four being all there is, and on all four being
// identity-available. A zero population satisfies the second half vacuously. It
// also satisfies the DottedHitIdentityDiverged zero vacuously, and the partition
// as 0 == 0.
//
// So a census that stopped being driven at all reports exactly the shape of a
// census reporting good news, and the numbers are small enough that "4" and "0"
// do not look different at a glance. The floors are what make them different.
//
// Set at 1 rather than an order of magnitude below the measurement, because
// there is no order of magnitude here: the measured DottedHitIdentityAvailable
// is 4. What a floor detects at this scale is DISAPPEARANCE, which is the whole
// failure mode — the shapes that drive the dotted arm ceasing to be planned, or
// the reader ceasing to be reached.
type LegColumnProvenanceFloors struct {
	// Calls floors the denominator every share is taken against.
	Calls int
	// DottedHitIdentityAvailable floors the population the retirement decision
	// is ABOUT. Calls alone does not cover it: the flat-hit arm carries 40 of
	// the 52 calls, so Calls can stay healthy while the dotted arm — the only
	// arm this census exists for — goes to zero.
	DottedHitIdentityAvailable int
}

// AssertLegColumnProvenanceCensus checks the census's partition, its one zero
// and its population floors, and reports whether it failed.
//
// The partition is the point: every share this census prints is a share of
// Calls, and a share only means something if the shares add up. The zero is
// DottedHitIdentityDiverged — a leg whose text and whose stated identity name
// different things, resolved by the text. That is not a residue to shrink, it is
// a contradiction: two keys for one leg, disagreeing, with only the weaker one
// consulted.
//
// floors is nil when the run is NARROWED, exactly as its siblings do it: the
// floors describe a whole-suite population, and a -test.run selecting tests that
// never adapt a leg row reaches this reader zero times. The partition and the
// zero still run — they hold over any population, which is precisely why they
// are not a proof on their own.
func AssertLegColumnProvenanceCensus(w io.Writer, floors *LegColumnProvenanceFloors) bool {
	c, _ := LegColumnProvenanceCensus()
	return assertLegColumnProvenanceCounters(w, c, floors)
}

func assertLegColumnProvenanceCounters(w io.Writer, c legColumnProvenanceCounters, floors *LegColumnProvenanceFloors) bool {
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
	dottedHits := c.DottedHitIdentityAvailable + c.DottedHitIdentityUnstated + c.DottedHitIdentityDiverged
	owners := c.DottedHitOwnerSelectsSameLeg + c.DottedHitOwnerUnstated +
		c.DottedHitOwnerNamesNoLeg + c.DottedHitOwnerSelectsOtherLeg
	if owners != dottedHits {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: the owner sub-partition sums to %d, "+
			"but there were %d dotted hits.\n"+
			"  Every dotted hit is classified by what an IDENTITY-keyed selection would\n"+
			"  have done, so the two must agree. A gap means hits are reaching the reader\n"+
			"  down a path that records no owner verdict, and the conversion's\n"+
			"  precondition is then a share of an unknown whole.\n", owners, dottedHits)
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
	if floors != nil {
		for _, f := range []struct {
			name  string
			got   int
			floor int
			why   string
		}{
			{
				"Calls", c.Calls, floors.Calls,
				"Nothing reached the reader at all, so the partition held as 0 == 0 and\n" +
					"  the DottedHitIdentityDiverged zero held vacuously. Either leg rows stopped\n" +
					"  being adapted or the census stopped being enabled.",
			},
			{
				"DottedHitIdentityAvailable", c.DottedHitIdentityAvailable,
				floors.DottedHitIdentityAvailable,
				"The DOTTED arm — the only arm this census exists to measure — answered\n" +
					"  zero times. The retirement decision rests on the claim that every dotted\n" +
					"  hit has an identity available; over an empty population that claim costs\n" +
					"  nothing and proves nothing, and it reads on the report exactly like the\n" +
					"  measured four-of-four that licensed it.",
			},
		} {
			if f.got < f.floor {
				failed = true
				fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: %s = %d, want >= %d.\n  %s\n",
					f.name, f.got, f.floor, f.why)
			}
		}
	}
	return failed
}
