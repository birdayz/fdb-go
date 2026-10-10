package executor

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Leg-column provenance distinguishes textual matches from identity- and owner-based
// alternatives, so a name-channel migration cannot silently change row selection.
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

// LegColumnProvenanceFloors is the population this census must report over a
// whole suite run — and it is now EMPTY, which is the reconciliation rather than
// an omission.
//
// It used to floor two numbers, because the census's finding was a pair of small
// ones: the dotted arm answered four times in the whole corpus, all four with an
// identity available. A zero population satisfied that second half vacuously,
// satisfied the DottedHitIdentityDiverged zero vacuously, and satisfied the
// partition as 0 == 0 — so a census that stopped being driven reported exactly
// the shape of a census reporting good news, and at that scale "4" and "0" do
// not look different at a glance.
//
// The reader has since been RETIRED, and the retirement is what inverts the
// guard. adaptLegPositional's permutation gather is its only driver, and that
// gather's own note said what would end it: "retiring this gather requires Go's
// seed to bake against the chosen physical leg layout the same way [Java does]".
// The exact-ordinal seed does that, so every leg row now passes
// positionalMatchesLegType and the gather — and this reader with it — is never
// entered. A floor on that population is unsatisfiable; the danger is REVIVAL,
// and that is asserted unconditionally in assertLegColumnProvenanceCounters.
//
// The type stays so the gate keeps its narrowed-run shape (a nil floors pointer
// still means "the corpus was filtered"), and so a future population has
// somewhere to be floored.
type LegColumnProvenanceFloors struct{}

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
	// THE READER IS RETIRED AND THIS IS ITS REVIVAL ALARM. Calls used to be
	// FLOORED, at 100, because a reader nothing reaches makes every zero beside
	// it vacuous. Its only driver is adaptLegPositional's permutation gather,
	// which the exact-ordinal seed removed from the live path: the seed now bakes
	// against the chosen physical leg layout, so every leg row passes
	// positionalMatchesLegType and the gather is never entered.
	//
	// The floor is therefore unsatisfiable and the direction inverts. Zero is the
	// steady state; a non-zero says the two-layout residue is back — a leg seeded
	// with one layout meeting a physical leg that emits another — and that is a
	// finding about the SEED, not about this reader. It is unconditional (no
	// floors pointer) because it holds over any population, narrowed run included.
	if c.Calls != 0 {
		failed = true
		fmt.Fprintf(w, "LEG-COLUMN PROVENANCE CENSUS FAIL: Calls = %d, want 0 — the leg-column\n"+
			"  name reader was RETIRED and this is its revival alarm.\n"+
			"  Its only driver is adaptLegPositional's layout-PERMUTATION gather, which runs\n"+
			"  when a leg's seeded type and the row its physical leg emits are permutations\n"+
			"  of each other. The exact-ordinal seed bakes against the chosen physical leg\n"+
			"  layout, exactly as Java's translateCorrelations does, so the two layouts\n"+
			"  agree and the gather is skipped. A non-zero here means some seed stopped\n"+
			"  doing that — find the PRODUCER, do not re-floor this reader.\n"+
			"  Calls is the ONLY number here; every other counter is a share of it.\n", c.Calls)
	}
	_ = floors
	return failed
}
