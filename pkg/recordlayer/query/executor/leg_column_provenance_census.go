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
// THE READER'S CHANNEL IS `qov.Type()`, NOT `RecordConstructorValue.Type()`.
// `ordinalJoinBuild.legType` consults `b.Spans` FIRST whenever WindowsOK, and a
// span's leg type comes from `resolveSpanLeaf`, which reads the baked reference's
// `qov.Type()` — the quantifier's OWN flowed type, never the RC field label. So
// RecordConstructorValue.Type() is NOT on this reader's path and a leg table
// attached there would not be read. Pinned in leg_type_channel_test.go.
//
// THAT SENTENCE COST A FULL IMPLEMENTATION CYCLE, because this comment used to
// contradict itself two paragraphs later and the contradiction was not labelled.
// RFC-212 §1.1 was written against the wrong half, built, and measured INERT: the
// leg table was attached to the constructor and propagated through
// RecordConstructorValue.Type(), and the dotted-hit count did not move
// (`available 2` before, `available 2` after, same two witnesses, over two
// uncached corpus runs). The wrong paragraphs are now deleted rather than kept
// as a curiosity — a reader who follows the wrong one builds the wrong thing,
// which is exactly what happened.
//
// The error had a specific shape worth naming, because it recurs on this path: a
// TRUE measurement carrying a FALSE COROLLARY. RFC-212 §3.5 measured correctly
// that RecordConstructorValue.Type() is the SOLE DERIVATION path for this row —
// and then inferred that the reader therefore takes it. Derivation is not
// readership. The row is derived in one place and read through another.
//
// The dotted names arrive as `qov.Type()` COLUMN NAMES, which means a producer
// named a quantifier's flowed column with a label: the correlated-scalar seed
// builds its inner leg as RECORD<csq.ScalarCol>, the subquery's OUTPUT TITLE.
// Measured span tables from TestFDB_CorrelatedScalarJoinInner:
//
//	spans=["C"@0+2:[ID NAME]  "q$204"@2+1:[I.QTY]]
//	spans=["C"@0+2:[ID NAME]  "q$223"@2+1:[SUM(QTY)]]
//	spans=["C"@0+2:[ID NAME]  "q$80"@2+1:[COUNT(*)]]
//	spans=["C"@0+2:[ID NAME]  "q$726"@2+1:[NAME]]
//
// One channel, four titles. `SUM(QTY)` and `COUNT(*)` are plainly titles;
// `I.QTY` is a title that happens to contain a dot, and it is the only one the
// dotted arm answers. So the four dotted hits are NOT leg-qualified references
// with an identity waiting to be keyed on — there is no leg being referenced, so
// there is no identity to name, and the retirement is a producer that must stop
// naming a TYPE by a LABEL.
//
// WHERE THE FIX HAS TO GO: the TITLE, not the derived type. The retirement of
// this arm is a producer that must stop naming a quantifier's flowed column with
// a label that can contain a dot — `clusteredOuterOrdinalSeed` builds
// `innerType := &RecordType{Fields: [{Name: scalarCol}]}` and hands it to
// NewQuantifiedObjectValueOfType, so when the subquery's output title is already
// qualified that title becomes the leg type's only column name and arrives here
// indistinguishable from a leg-qualified reference. The doubled-qualifier
// witness `[TID K C.C.CV]` in the dotted row-type producer census is the same
// fact seen from the other side: a title that was already qualified, qualified
// again.
//
// TWO PARAGRAPHS THAT USED TO SIT HERE ARE DELETED, and what they claimed is
// recorded so nobody re-derives them:
//
//   - "RecordConstructorValue.Type() throws the leg table away, and that stripped
//     type is what widenLegTypesFromPlan stores and adaptLegPositional adapts
//     against." The first clause is true and the second is false; the reader's
//     type comes from `qov.Type()`. This is the sentence RFC-212 §1.1 was written
//     against.
//   - "Restoring it is additive — RecordType.Legs is layout metadata that Equals
//     and Hash IGNORE, so populating it moves no name, no label and no type
//     identity." Refuted twice over. Equals/Hash ignoring Legs establishes TYPE
//     IDENTITY only, never behaviour: `expressions.refineRowTypes` checks
//     `legTablesAgree` BEFORE the Equals fast path and treats a populated table
//     against an empty one as a CONFLICT, declining the refinement (RFC-212 §3.4,
//     pinned in expressions.TestLegTablePopulation_*). And four of the readers
//     that branch on `len(Legs) > 0` DECLINE their layout outright when it
//     becomes non-empty (ordinal_join.go:234 and :187, ordinal_seed_layout.go:391
//     and :528).
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
	// FlatAmbiguous: the row's own type declares the name MORE THAN ONCE, and
	// no leg window resolved it either. This is the outcome that has no benign
	// reading — the value exists at two slots, so neither "here it is" nor "it
	// is not here" is true, and the reader refuses.
	//
	// It is counted SEPARATELY from NotDotted because it used to be
	// indistinguishable from it. The flat lookup declines on absent and on
	// duplicated alike, and a bare duplicated name lands in the `di <= 0` arm,
	// so an ambiguous bind was reported as a plain miss — the census could not
	// have told anyone the reader had started declining rows it used to bind.
	// Expected value over the corpus is ZERO, and the assertion says which
	// direction is the alarm.
	FlatAmbiguous int
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
	legColumnProvenanceFlatAmbiguous
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
// flatAmbiguous says the flat lookup found the name at MORE THAN ONE slot. It
// is asked AFTER the dotted arm's answer, because a qualifier that resolves a
// leg window is strictly more information than the flat namespace carries — the
// ambiguity only stands when nothing resolved it. It is asked BEFORE NotDotted
// so a bare duplicated name is reported as the ambiguity it is rather than as
// an ordinary miss, which is how it went uncounted.
func classifyLegColumnProvenance(rt *values.RecordType, name string, flatHit, flatAmbiguous bool, matched *values.RecordTypeLeg, qualifier string) legColumnProvenanceClass {
	if flatHit {
		return legColumnProvenanceFlatHit
	}
	if matched != nil {
		if matched.Alias.IsZero() {
			return legColumnProvenanceIdentityUnstated
		}
		if !strings.EqualFold(matched.Alias.Name(), qualifier) {
			return legColumnProvenanceIdentityDiverged
		}
		return legColumnProvenanceIdentityAvailable
	}
	if flatAmbiguous {
		return legColumnProvenanceFlatAmbiguous
	}
	if strings.IndexByte(name, '.') <= 0 {
		return legColumnProvenanceNotDotted
	}
	if rt == nil || len(rt.Legs) == 0 {
		return legColumnProvenanceNoLegs
	}
	return legColumnProvenanceMiss
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
func recordLegColumnProvenance(rt *values.RecordType, name string, flatHit, flatAmbiguous bool, matched *values.RecordTypeLeg, qualifier string, owner values.CorrelationIdentifier) {
	class := classifyLegColumnProvenance(rt, name, flatHit, flatAmbiguous, matched, qualifier)
	legColumnProvenanceMu.Lock()
	defer legColumnProvenanceMu.Unlock()
	legColumnProvenanceCounts.Calls++
	switch class {
	case legColumnProvenanceIdentityAvailable, legColumnProvenanceIdentityUnstated, legColumnProvenanceIdentityDiverged:
		// RFC-212 §10.3 deliverable 1: report the answered NAME with the OWNER
		// correlation, so the attribution census can decide BY IDENTITY whether
		// this leg type was built by the correlated-scalar seed's inner leg.
		values.RecordDottedArmAnswer(name, owner)
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
	case legColumnProvenanceFlatAmbiguous:
		legColumnProvenanceCounts.FlatAmbiguous++
		addLegColumnProvenanceWitness(fmt.Sprintf("FLAT-AMBIGUOUS %q: the row declares it %d times over legs %v",
			name, rt.FieldNameHits(name), legNamesOf(rt)))
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

// LegColumnProvenanceDottedNames returns the distinct COLUMN NAMES the dotted
// arm ANSWERED on — the qualified labels, not the witness prose.
//
// It exists so a cross-population claim can be checked rather than eyeballed.
// The retirement condition booked for this reader is "the dotted-hit count goes
// to 0", booked against converting a mint in the TRANSLATOR; whether that is
// reachable depends on whether the names that mint produces are these names.
// Comparing the two sets needs both as data, and this census's witnesses are
// sentences.
func LegColumnProvenanceDottedNames() []string {
	legColumnProvenanceMu.Lock()
	defer legColumnProvenanceMu.Unlock()
	var out []string
	seen := map[string]struct{}{}
	for _, w := range legColumnProvenanceWitnesses {
		if !strings.HasPrefix(w, "DOTTED-HIT") {
			continue
		}
		i := strings.IndexByte(w, '"')
		if i < 0 {
			continue
		}
		j := strings.IndexByte(w[i+1:], '"')
		if j < 0 {
			continue
		}
		name := w[i+1 : i+1+j]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FormatLegColumnProvenanceCensus renders the census for a harness to log.
func FormatLegColumnProvenanceCensus() string {
	c, witnesses := LegColumnProvenanceCensus()
	var b strings.Builder
	fmt.Fprintf(&b, "leg-column provenance: calls %d (flatHit %d, notDotted %d, noLegs %d, "+
		"dottedMiss %d, flatAmbiguous %d); dotted HITS by identity availability: available %d, unstated %d, diverged %d",
		c.Calls, c.FlatHit, c.NotDotted, c.NoLegs, c.DottedMiss, c.FlatAmbiguous,
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
	got := c.FlatHit + c.NotDotted + c.NoLegs + c.DottedMiss + c.FlatAmbiguous +
		c.DottedHitIdentityAvailable + c.DottedHitIdentityUnstated + c.DottedHitIdentityDiverged
	if got != c.Calls {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: the eight outcomes sum to %d, "+
			"but Calls = %d.\n"+
			"  They are the only things one lookup can do, so they must partition it. A\n"+
			"  gap means a call left the reader by a path with no counter on it, and every\n"+
			"  share below — including the one that decides whether this reader can be\n"+
			"  re-keyed by identity — is then a share of an unknown whole.\n", got, c.Calls)
	}
	dottedHits := c.DottedHitIdentityAvailable + c.DottedHitIdentityUnstated + c.DottedHitIdentityDiverged
	if dottedHits != 0 {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: the dotted arm ANSWERED %d time(s), want 0.\n"+
			"  RFC-212 §11.3 retitled the producer that was naming a leg type's only column\n"+
			"  with a dot-containing title, and this arm went from 2 answers to 0 over the\n"+
			"  whole real-FDB corpus. Zero is now the STEADY STATE, so the dangerous\n"+
			"  direction is GROWTH: this population was watched for collapse while the arm\n"+
			"  was live, and is watched for revival now.\n"+
			"  WHAT A NON-ZERO MEANS: some producer is again naming a quantifier's flowed\n"+
			"  column with a title that splits at a dot, so a reference resolves through a\n"+
			"  LEG and COLUMN this leg does not have. Find the producer and give it an\n"+
			"  unqualified title (query.unqualifiedScalarTitle); do NOT relax this zero.\n",
			dottedHits)
	}
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
	if c.FlatAmbiguous != 0 {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: FlatAmbiguous = %d, want 0.\n"+
			"  THE ALARM DIRECTION HERE IS GROWTH, not collapse. Zero is the steady state\n"+
			"  measured over the whole real-FDB corpus: no leg type has ever handed this\n"+
			"  reader a column name that its source row declares twice. This counter is not\n"+
			"  floored, because a floor on it would demand the defect it watches for.\n"+
			"  WHAT A NON-ZERO MEANS: a producer built a leg type whose column name is\n"+
			"  ambiguous against the merged row it will be adapted against, so the reader\n"+
			"  can neither bind it (either slot is a wrong-leg read) nor skip it (the value\n"+
			"  exists, so a nil is a wrong value rather than a missing one). adaptLegPositional\n"+
			"  now FAILS the query on this rather than degrading, so a non-zero here comes\n"+
			"  with a real error — fix the PRODUCER to qualify the leg type's column names or\n"+
			"  to carry a baked ordinal. Do NOT relax this zero and do NOT make the reader guess.\n",
			c.FlatAmbiguous)
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
