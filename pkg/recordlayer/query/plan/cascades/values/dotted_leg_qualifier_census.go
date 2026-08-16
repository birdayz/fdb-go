package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// The DOTTED LEG QUALIFIER census — the TRANSLATOR-side twin of the executor's
// leg-column provenance census.
//
// Both measure the same reader shape: a qualifier is matched,
// case-insensitively, against a leg table's Name text. The executor's copy
// lives beside its one reader (`rowSlotForLegColumn`); this one covers the
// translator's TWO, which no census reached.
//
// WHERE THE QUALIFIER COMES FROM has changed, and the census deliberately did
// NOT narrow with it. It was originally "a qualifier SPLIT OUT of a column
// name", because that was the only way one arrived. The projection channel now
// carries the parser's segment triple end-to-end (CQ-52), so most qualifiers
// reaching these readers were SEGMENTED rather than sliced — and the census
// counts both, because its question is not how the qualifier was obtained but
// whether the leg its TEXT selects states an identity naming the same thing.
// That question survives the conversion unchanged; narrowing the census to the
// splitting callers would have made its two hard zeros hold over a shrinking
// population while the channel they guard stayed the same size.
//
// TWO, and it was briefly three. `bakeDottedRefsToLegQOV`'s SINGLE-ForEach arm
// was a separate reader of the same shape, counted here until the arm itself was
// deleted as unreached (see below). What remains is the multi-ForEach map read
// and the flat baker's leg window.
//
// The question is the same and so is the reason it is asked per CALL rather than
// per site. A site's key provenance is static — these readers hold no
// CorrelationIdentifier at all, because each is guarded on `fv.Child != nil ||
// fv.Resolved != nil → bail` and therefore only ever sees a lazy carrier minted
// from parsed text. What is NOT static is whether the leg the qualifier matched
// carries a stated identity naming the same thing:
//
//   - a leg with a stated Alias equal to the qualifier means the leg TABLE could
//     serve an identity-keyed reader; the lookup still could not, because there
//     is nothing on this side to key WITH;
//   - a leg whose Alias DIFFERS from the qualifier it matched means minting a
//     CorrelationIdentifier out of the qualifier would forge an identifier the
//     leg itself disagrees with — the case-disjoint namespaces (user aliases
//     upper-folded, machine mints lowercase) make that a live hazard rather than
//     a hypothetical one;
//   - a leg with no Alias at all is a producer that never stated an identity.
//
// So this census does NOT decide whether these readers can be re-keyed — they
// cannot, and the reason is structural rather than measured. It decides
// something narrower and more useful: whether the LEG TABLE these readers walk
// is ready for the day their counterparty carries segments instead of a joined
// string (CQ-52), and whether the channel is live enough for that to matter.
//
// WHAT IT MEASURED — a POINT measurement over the whole real-FDB sqldriver
// corpus, dated to the addition of the third site. Re-measure rather than
// trusting the digits; the durable claim is the SHAPE (the blocking classes
// empty), not the totals, which move whenever a query is added anywhere in that
// suite. The standing instrument is `AssertDottedLegQualifierCensus`, wired into
// the sqldriver `TestMain`, and it is what keeps these numbers honest.
//
// Re-measured 2026-08-06 at cb9bc5225, and STABLE across two consecutive
// full-suite runs (the previous reading, 106 | 98 | 8, is kept below it as the
// history it is):
//
//	flatColumnBake     calls 102 | matchAliasIsQualifier 98 | noMatch 4
//	legQOVBake         calls   4 | matchAliasIsQualifier  3 | matchViaTableName 1
//
// Previously (dated to the addition of the third site):
//
//	flatColumnBake     calls 106 | matchAliasIsQualifier 98 | noMatch 8
//	legQOVBake         calls   4 | matchAliasIsQualifier  3 | matchViaTableName 1
//
// The channel is LIVE and the leg table's two spellings agree on every match:
// no MATCH-ALIAS-DIFFERS, no MATCH-NO-ALIAS. So the table is ready; the READERS
// are not, and cannot be made so from this side — they hold no
// CorrelationIdentifier to key with, whatever the qualifier's provenance.
//
// The counterparty conversion has HAPPENED for all four parsed channels: the
// qualifier arrives segmented, so the two spellings no longer have to survive a
// join and a re-split to reach each other. The measured effect is on the SPLIT
// population, not on these totals.
//
// THAT POPULATION NOW HAS ITS OWN INSTRUMENT — name_split_census.go — and this
// paragraph used to be the admission that it did not. The "fell from 110 calls
// to ZERO" figure quoted here was scratch measurement, re-quoted rather than
// re-read, which is the exact shape of claim this file exists to keep honest and
// was making about its own blind spot. The zero is now CONFIRMED by
// AssertNameSplitCensus, and the population it is a zero over is 11 calls
// between the two arms, not the ~110 this sentence implied. Read the numbers
// there; do not re-quote them here, because one census quoting another is how
// the 110 survived.
//
// The re-split arms survive that zero, and not as leftovers: they serve the
// carrier that states NO segments, which the segment triple's own contract
// requires be read as "unknown" rather than "unqualified". One class of such
// carrier can never state them — a projection MACHINERY mints, whose names are
// body output columns with no parse tree behind them. What to do about that
// class is a BEHAVIOUR question, stated and consciously left open at
// query.bakeFlatRefsAgainstColumns' arm, not an un-migrated producer.
//
// THE THIRD SITE IS GONE, and its zero is why. singleForEachBake measured the
// single-ForEach arm's qualifier comparison, and that arm reported 0 over this
// corpus while a panic at its match point was reached by nothing across
// ./pkg/relational/... — the real-FDB sqldriver corpus, explaindiff, plandiff
// and conformance — nor across ./pkg/recordlayer/query/... That is the full
// reach the two arms already deleted from the box wrap were held to (a childless
// dotted splitter and a bare name matcher, both retired after the same probe came
// back empty), run at that reach rather than inheriting a narrower one.
//
// The ARM was deleted and this census site went with it, deliberately. An
// instrument pointed at a channel that no longer exists reports a hard zero
// forever, and a permanent zero reads exactly like a channel measured clean —
// which is the failure every floor on this path exists to prevent. A census
// retires WITH what it measures. Note the scope: it was the DOTTED arm of that
// baker that was dark; the same baker's BARE leaf path is live, is untouched,
// and is a separate debt entry.
//
// The single matchViaTableName is the finding that keeps this census from being
// a formality: `FROM PA AS "s"` registers the layout under BOTH `S` and the scan
// table name `PA`, so one of this map's two key kinds names a TABLE. A table is
// not any quantifier's identity, which means this map cannot be re-keyed by
// identity even in principle — a stronger statement than "the reader holds no
// correlation", and one that only a measurement could have produced.
//
// GATED by LegIdentityCensusEnabled. Counts are per CALL.

// DottedLegSite is one translator reader that matches a name-split qualifier
// against a leg table.
type DottedLegSite int

const (
	// DottedLegSiteFlatColumnBake is query.bakeFlatRefsAgainstColumns' dotted
	// arm: qualifier → leg window over the flat output column list, leaf →
	// first match within the window. Its leg table comes from
	// expressionOutputLegs or from the wholeRowLegFor TEXT-BOUNDARY mint.
	DottedLegSiteFlatColumnBake DottedLegSite = iota

	// DottedLegSiteLegQOVBake is query.bakeDottedRefsToLegQOV's MULTI-ForEach
	// per-leg layout lookup: qualifier → the leg's own layout, leaf → an ordinal
	// in that leg's OWN column domain. Its layouts are keyed by upper-folded text
	// while each layout carries the quantifier's identity, so it is the same
	// held-apart namespace pair the seed windows had.
	DottedLegSiteLegQOVBake

	// There were THREE. DottedLegSiteSingleForEachBake measured the same
	// function's SINGLE-ForEach arm, which compared the qualifier against its one
	// layout's key instead of reading a map. The ARM IS DELETED, and this
	// instrument went with it: a census site measuring a channel that no longer
	// exists reports a hard zero forever and reads exactly like a channel
	// measured clean.
	//
	// The deletion's warrant was a panic at that arm's match point, hit by
	// nothing across ./pkg/relational/... (the real-FDB sqldriver corpus,
	// explaindiff, plandiff, conformance) NOR ./pkg/recordlayer/query/... — the
	// full reach the earlier arm deletions on this path were held to. A
	// qualified read in a single-ForEach select now declines and is loud at
	// evaluation rather than baking flat.

	dottedLegSiteCount
)

func (s DottedLegSite) String() string {
	switch s {
	case DottedLegSiteFlatColumnBake:
		return "flatColumnBake"
	case DottedLegSiteLegQOVBake:
		return "legQOVBake"
	default:
		return "unknown"
	}
}

// DottedLegClass is one call's bucket. The six partition every call.
type DottedLegClass int

const (
	// DottedLegMatchAliasIsQualifier: a leg matched and its stated Alias is
	// EXACTLY the qualifier text. The leg table can serve an identity; the
	// lookup still cannot, having none to offer.
	DottedLegMatchAliasIsQualifier DottedLegClass = iota
	// DottedLegMatchAliasDiffers: a leg matched by FOLDED text and its stated
	// Alias is not that text. Minting from the qualifier would forge an
	// identifier the leg disagrees with — this is the population that would make
	// a text→identifier mint wrong rather than merely redundant.
	DottedLegMatchAliasDiffers
	// DottedLegMatchNoAlias: a leg matched and states no identity at all.
	DottedLegMatchNoAlias
	// DottedLegMatchViaTableName: a leg matched on a qualifier that is NOT its
	// binding — the scan TABLE name, registered as a second addressing route so
	// `FROM PA AS "s"` still answers `PA."ID"` (the ordinal leg type's RecordName
	// contract). The identifier such a qualifier would mint is by DESIGN not the
	// quantifier's, so this population is the reason its map cannot be re-keyed
	// by identity even in principle: one of its two key kinds names a table, and
	// a table is not a quantifier.
	//
	// It is counted apart from MATCH-ALIAS-DIFFERS rather than folded into it
	// because the two look identical at the reader — a qualifier and a leg alias
	// that disagree — and mean opposite things. Folding them would have reported
	// a live contradiction where there is a documented feature, and the census
	// did exactly that before this class existed.
	DottedLegMatchViaTableName
	// DottedLegAmbiguousQualifier: the qualifier named MORE THAN ONE leg and the
	// layout map POISONED that key (`layouts[key] = nil`) so nothing bakes
	// through it. Two legs sharing an alias, or two legs scanning one table under
	// the table-name addressing route.
	//
	// It is a fifth thing, not a flavour of noMatch, and folding it into noMatch
	// is what this class fixes. The census recorded its call BEFORE the `lay ==
	// nil` bail, and a poisoned key and an absent one are the same nil at that
	// point — so "no leg carried the qualifier" was reported for a qualifier
	// carried by two. The two mean opposite things about the leg table: absent is
	// a reference the table does not describe, ambiguous is a table that
	// describes it twice and refuses to choose. Only the second is a fact about
	// how much this channel is being asked to do.
	DottedLegAmbiguousQualifier

	// DottedLegNoMatch: no leg carried the qualifier. Not a name decision — the
	// reference falls through unbaked.
	DottedLegNoMatch

	dottedLegClassCount
)

func (c DottedLegClass) String() string {
	switch c {
	case DottedLegMatchAliasIsQualifier:
		return "matchAliasIsQualifier"
	case DottedLegMatchAliasDiffers:
		return "MATCH-ALIAS-DIFFERS"
	case DottedLegMatchNoAlias:
		return "MATCH-NO-ALIAS"
	case DottedLegMatchViaTableName:
		return "matchViaTableName"
	case DottedLegAmbiguousQualifier:
		return "ambiguousQualifier"
	case DottedLegNoMatch:
		return "noMatch"
	default:
		return "unknown"
	}
}

// DottedLegLookup is what the READER's own map read produced, before any
// question about the leg it found. It is threaded in rather than inferred from
// the matched leg, because two of its three values arrive at the reader as the
// same nil: a qualifier with no entry and a qualifier whose entry was POISONED
// for ambiguity are indistinguishable once the map read is over, and they mean
// opposite things.
type DottedLegLookup int

const (
	// DottedLegLookupMiss: the layout map/leg table has no entry for this
	// qualifier.
	DottedLegLookupMiss DottedLegLookup = iota
	// DottedLegLookupAmbiguous: an entry exists and is POISONED — two legs claim
	// the qualifier, so nothing may bake through it.
	DottedLegLookupAmbiguous
	// DottedLegLookupHit: exactly one leg carried the qualifier.
	DottedLegLookupHit
)

const dottedLegWitnessCap = 128

var (
	dottedLegMu        sync.Mutex
	dottedLegCounts    [dottedLegSiteCount][dottedLegClassCount]int
	dottedLegWitnesses []string
)

// classifyDottedLegQualifier decides one call's bucket. Split from the counter
// mutation so the decision can be exercised without process-global state.
//
// The alias test is EXACT rather than case-folding, for the reason the seed-window
// census states at its own exact test: the two alias namespaces are deliberately
// case-disjoint, so a qualifier `Q$5` over a leg whose alias is `q$5` is the
// forgery this census exists to find, not an approximation of a match.
// matchedBinding is the matched leg's OWN binding text — what the reader would
// have had to be given for the qualifier to be that leg's name rather than one
// of its alternate addressing routes. It is threaded in rather than inferred,
// because from the qualifier and the alias alone a table-name route and a
// two-spellings contradiction are the same observation.
func classifyDottedLegQualifier(qual string, matchedAlias CorrelationIdentifier, matchedBinding string, lookup DottedLegLookup) DottedLegClass {
	switch {
	case lookup == DottedLegLookupMiss:
		return DottedLegNoMatch
	case lookup == DottedLegLookupAmbiguous:
		return DottedLegAmbiguousQualifier
	case !strings.EqualFold(qual, matchedBinding):
		return DottedLegMatchViaTableName
	case matchedAlias.IsZero():
		return DottedLegMatchNoAlias
	case matchedAlias.Name() == qual:
		return DottedLegMatchAliasIsQualifier
	default:
		return DottedLegMatchAliasDiffers
	}
}

// RecordDottedLegQualifier counts one qualifier-against-leg-table match.
// Callers must guard on LegIdentityCensusEnabled().
func RecordDottedLegQualifier(site DottedLegSite, qual string, matchedAlias CorrelationIdentifier, matchedBinding string, lookup DottedLegLookup) {
	class := classifyDottedLegQualifier(qual, matchedAlias, matchedBinding, lookup)
	dottedLegMu.Lock()
	defer dottedLegMu.Unlock()
	if site < 0 || site >= dottedLegSiteCount {
		return
	}
	dottedLegCounts[site][class]++
	if class == DottedLegNoMatch {
		return
	}
	w := fmt.Sprintf("%s [%s] qualifier %q, leg binding %q, leg alias %q",
		site, class, qual, matchedBinding, matchedAlias.Name())
	if len(dottedLegWitnesses) >= dottedLegWitnessCap {
		return
	}
	for _, seen := range dottedLegWitnesses {
		if seen == w {
			return
		}
	}
	dottedLegWitnesses = append(dottedLegWitnesses, w)
}

// DottedLegQualifierCensus reports the per-site class matrix and the witnesses.
func DottedLegQualifierCensus() ([dottedLegSiteCount][dottedLegClassCount]int, []string) {
	dottedLegMu.Lock()
	defer dottedLegMu.Unlock()
	out := make([]string, len(dottedLegWitnesses))
	copy(out, dottedLegWitnesses)
	return dottedLegCounts, out
}

// ResetDottedLegQualifierCensus clears the counters.
func ResetDottedLegQualifierCensus() {
	dottedLegMu.Lock()
	defer dottedLegMu.Unlock()
	dottedLegCounts = [dottedLegSiteCount][dottedLegClassCount]int{}
	dottedLegWitnesses = nil
}

// FormatDottedLegQualifierCensus renders the census for a harness to log.
func FormatDottedLegQualifierCensus() string {
	counts, witnesses := DottedLegQualifierCensus()
	var b strings.Builder
	b.WriteString("translator dotted leg qualifier (per match attempt):")
	for s := DottedLegSite(0); s < dottedLegSiteCount; s++ {
		total := 0
		for c := 0; c < int(dottedLegClassCount); c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(&b, "\n  %-18s calls %d", s, total)
		for c := DottedLegClass(0); c < dottedLegClassCount; c++ {
			if counts[s][c] != 0 {
				fmt.Fprintf(&b, " | %s %d", c, counts[s][c])
			}
		}
	}
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  matching witnesses (%d, cap %d):\n    %s",
			len(sorted), dottedLegWitnessCap, strings.Join(sorted, "\n    "))
	}
	return b.String()
}

// DottedLegQualifierFloors is the minimum population each site must report over
// a whole suite run.
//
// Same reason as every floor on this path, and it bites harder here than usual:
// the findings this census produces are "the MATCH-ALIAS-DIFFERS population is
// empty" and "the MATCH-NO-ALIAS population is empty", and an unreached site
// prints both identically to a site measured clean. A site left at 0 is
// UNFLOORED and that is a statement about the corpus, not an omission.
type DottedLegQualifierFloors struct {
	Calls [dottedLegSiteCount]int
}

// AssertDottedLegQualifierCensus checks the census's hard zeros and its floors.
//
// There are TWO hard zeros and they are different failures of the same table.
//
// MATCH-ALIAS-DIFFERS: a leg matched on its OWN BINDING text while stating a
// different identity. That is the leg table having two spellings that disagree,
// resolved by the weaker one — the same contradiction the seed-window census
// refuses, seen through the one channel that cannot be re-keyed. A leg matched
// through its scan TABLE name is a different fact and has its own class; the
// zero would be unsatisfiable if the two were folded together, which is what the
// first measurement did. It is worth asserting HERE precisely because this
// reader will keep matching on text until its counterparty carries parsed
// segments: the guarantee that the text it matches names the leg the identity
// names is all that holds the channel together in the meantime.
//
// MATCH-NO-ALIAS: a leg matched and states NO identity at all. This class was
// documented as blocking from the day it was written and was never gated, so its
// zero was a sentence rather than a check. It is the same defect the seed-window
// authority now declines on (an Alias-less leg files under the zero identifier
// and a second one displaces it), seen from the reader side: the leg table
// carries an entry whose identity nothing can compare, so the day the
// counterparty carries segments this leg has nothing to be matched against and
// the conversion silently loses it. Fix the PRODUCER; the two documented text
// boundaries that mint a leg from a string both set Name and Alias from that one
// string, so a leg reaching here without an identity came from neither.
func AssertDottedLegQualifierCensus(w io.Writer, floors *DottedLegQualifierFloors) bool {
	counts, _ := DottedLegQualifierCensus()
	return assertDottedLegQualifierCounts(w, counts, floors)
}

func assertDottedLegQualifierCounts(w io.Writer, counts [dottedLegSiteCount][dottedLegClassCount]int, floors *DottedLegQualifierFloors) bool {
	failed := false
	for s := DottedLegSite(0); s < dottedLegSiteCount; s++ {
		if counts[s][DottedLegMatchAliasDiffers] != 0 {
			failed = true
			fmt.Fprintf(w, "DOTTED LEG QUALIFIER CENSUS FAIL: %s reported %d MATCH-ALIAS-DIFFERS, want 0.\n"+
				"  A qualifier parsed out of a column name matched a leg on that leg's OWN\n"+
				"  BINDING, by CASE-FOLDED text, while the leg states a different identity.\n"+
				"  The two spellings of one leg disagree and the weaker one decided, so a\n"+
				"  reference is being routed by a name its own leg does not answer to. This is\n"+
				"  NOT the table-name addressing route, which has its own class. Fix the\n"+
				"  PRODUCER that gave the leg two spellings; this reader has no identifier to\n"+
				"  key with and cannot defend itself.\n", s, counts[s][DottedLegMatchAliasDiffers])
		}
		if counts[s][DottedLegMatchNoAlias] != 0 {
			failed = true
			fmt.Fprintf(w, "DOTTED LEG QUALIFIER CENSUS FAIL: %s reported %d MATCH-NO-ALIAS, want 0.\n"+
				"  A qualifier matched a leg that states NO identity at all. The leg table\n"+
				"  carries an entry nothing can compare by identity, so the day this reader's\n"+
				"  counterparty carries parsed segments (CQ-52) that leg has nothing to be\n"+
				"  matched against and the conversion loses it silently. It is also the reader\n"+
				"  side of the collision the seed-window authority now declines on: two\n"+
				"  Alias-less legs file under the ZERO identifier and the second displaces the\n"+
				"  first. Fix the PRODUCER — both documented text boundaries set Name and\n"+
				"  Alias from the one string they have, so a leg arriving here without an\n"+
				"  identity came from neither.\n", s, counts[s][DottedLegMatchNoAlias])
		}
	}
	// THE CHANNEL IS RETIRED AND THIS IS ITS REVIVAL ALARM.
	//
	// Both sites were arms of the NAME-model bake — query.bakeFlatRefsAgainstColumns
	// and query.bakeDottedRefsToLegQOV — which resolved a reference by splitting a
	// column name at a dot and matching the qualifier against a leg's text. The
	// ordinal model resolves by baked slot and those bakes are gone, so
	// RecordDottedLegQualifier has no caller and every count is zero.
	//
	// The counts used to be FLOORED (10 and 1), because both hard zeros above hold
	// vacuously over an empty population and a reader nothing drives reports
	// exactly like a reader with nothing wrong. Those floors are unsatisfiable
	// now. The direction inverts: any call means a name-splitting resolution path
	// has come back, which is the model this whole channel was retired from.
	for s := DottedLegSite(0); s < dottedLegSiteCount; s++ {
		total := 0
		for c := 0; c < int(dottedLegClassCount); c++ {
			total += counts[s][c]
		}
		if total == 0 {
			continue
		}
		failed = true
		fmt.Fprintf(w, "DOTTED LEG QUALIFIER CENSUS FAIL: %s reached %d match attempt(s), want 0 —\n"+
			"  the dotted-qualifier match channel was RETIRED and this is its revival alarm.\n"+
			"  This reader resolved a reference by splitting a column NAME at a dot and\n"+
			"  matching the qualifier against a leg's text — the one channel that cannot be\n"+
			"  re-keyed by identity. Its producers were the name-model bakes, which the\n"+
			"  ordinal model replaced with resolution by baked slot.\n"+
			"  A non-zero means a name-splitting resolution path is back. Find it; do not\n"+
			"  re-floor this reader.\n", s, total)
	}
	_ = floors
	return failed
}
