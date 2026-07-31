package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// The SEED-WINDOW KEY PROVENANCE census.
//
// OrdinalSeedLegWindows returns a map whose KEYS are UPPER-FOLDED TEXT and whose
// VALUES each carry a stated leg IDENTITY (OrdinalSeedLegWindow.Alias). Every
// keyed read of that map is therefore a place where a leg is selected by text
// while an identity sits inside the thing selected. This census measures, per
// reader, the one question that decides whether the text namespace can be
// retired:
//
//	where did the lookup KEY come from, and did a CorrelationIdentifier naming
//	the same window exist AT THE CALL SITE?
//
// Those are two different questions and the second is the load-bearing one. A
// key spelled by upper-folding an identifier the site already holds is a
// round-trip through text that changes nothing — the site can be re-keyed by
// identity and the lookup is a refactor. A key SPLIT OUT of a column name has no
// identifier behind it at all, and re-keying it would mean minting a
// CorrelationIdentifier from a name, which is the forgery RFC-197 exists to
// remove. The two look identical at the call site (`windows[someString]`) and
// they have opposite dispositions.
//
// Asking it per CALL rather than per SITE is not pedantry. A site's key
// provenance is static, but whether the identity lookup ANSWERS THE SAME is a
// fact about the traffic: a leg can be filed under a key whose text does not
// fold from its own alias (finalizeSeedWindows files a buried sub-window under
// the BURIED leg's Name while the window carries that leg's own identity, and
// files a box run under a minted `X$BOX` key), so text and identity can select
// different windows for one reference. That is the outcome this census exists to
// find, and it cannot be argued — only counted.
//
// WHAT IT MEASURED, whole real-FDB sqldriver corpus:
//
//	existentialRebase        calls 962 | identityAgreesHit 461 | identityAgreesMiss 501
//	existentialDeclineProbe  calls 0
//	boxLegRef                calls  92 | identityAgreesHit  62 | identityAgreesMiss  30
//	boxDottedSplit           calls 0
//	boxSurvivorQOV           calls 184 | identityAgreesMiss 184
//	boxSurvivorCorrelation   calls   2 | identityAgreesMiss   2
//	gatheredGroupSlot        calls 160 | identityAgreesHit 160
//
// 1400 lookups, and every blocking class is EMPTY: no TEXT-ONLY-HIT, no
// IDENTITY-ONLY-HIT, no DIVERGED, anywhere. Every reader that holds a
// correlation gets the same window from it that the fold of it produced.
//
// The two ZEROS are the sharper half, because they are the sites this map's text
// namespace was said to exist FOR. Both are the text-only kind — the box-wrap's
// dotted splitter and the decline probe's mint-from-key — and both are
// UNREACHABLE, not merely quiet: a panic wired into each is reached by nothing in
// ./pkg/relational/... (this corpus plus the explaindiff, plandiff, rowdiff,
// memoinvariant and yamsql harnesses) nor ./pkg/recordlayer/query/... . The
// counts are per LOOKUP and move with the corpus; the ZEROS and the empty
// blocking classes are the durable claim.
//
// The corpus totals are POINT measurements. Re-measure rather than trusting the
// digits: `go test ./pkg/relational/sqldriver/ -count=1 -v | grep -A8
// "seed-window key provenance"`.
//
// GATED by LegIdentityCensusEnabled, like every census on this path: a disabled
// census costs each reader one predicate. Counts are per LOOKUP.

// SeedWindowSite is one keyed reader of a seed-window map. These are ALL of
// them in production; the remaining OrdinalSeedLegWindows call sites consume the
// map as a nil/non-nil PREDICATE ("is this an ordinal seed?") and never key it,
// which is not a name decision and is not counted here.
type SeedWindowSite int

const (
	// SeedWindowSiteExistentialRebase is cascades.rebaseOuterLegValueOrdinal's
	// per-reference window lookup — the EXISTS-over-join ordinal rebase. Its key
	// is the upper fold of the reference's own QuantifiedObjectValue correlation.
	SeedWindowSiteExistentialRebase SeedWindowSite = iota

	// SeedWindowSiteExistentialDeclineProbe is the same file's default-predicate
	// arm, which walks the window KEYS and mints a CorrelationIdentifier out of
	// each to test a predicate's correlation set. The mint is the thing being
	// measured: the window it came from carries the identifier the probe is
	// manufacturing.
	SeedWindowSiteExistentialDeclineProbe

	// SeedWindowSiteBoxLegRef is query.rebaseLegRefsToBox's QOV-shaped arm. Its
	// key comes from legRef, which upper-folds the reference's own QOV
	// correlation.
	SeedWindowSiteBoxLegRef

	// SeedWindowSiteBoxDottedSplit is the same walk's DOTTED-frontier arm: the
	// key is sliced out of the column name at the first dot. No identifier
	// exists at the site — the arm is guarded on `fv.Child == nil`, so there is
	// structurally no correlation to hold.
	SeedWindowSiteBoxDottedSplit

	// SeedWindowSiteBoxSurvivorQOV is the post-walk verification: does any
	// leg-correlated QOV survive the rebase? Keyed by the surviving QOV's own
	// correlation, upper-folded.
	SeedWindowSiteBoxSurvivorQOV

	// SeedWindowSiteBoxSurvivorCorrelation is the wrap's correct-or-decline net
	// over a translated subquery's correlation set — keyed by each correlation in
	// that set, upper-folded.
	SeedWindowSiteBoxSurvivorCorrelation

	// SeedWindowSiteGatheredGroupSlot is query.slotInGatheredSeed's qualified
	// arm. Its key is EITHER the upper fold of a held QOV correlation OR a
	// qualifier split out of a dotted field name — the one site whose provenance
	// is not static, which is why the correlation is threaded in rather than
	// inferred.
	SeedWindowSiteGatheredGroupSlot

	seedWindowSiteCount
)

func (s SeedWindowSite) String() string {
	switch s {
	case SeedWindowSiteExistentialRebase:
		return "existentialRebase"
	case SeedWindowSiteExistentialDeclineProbe:
		return "existentialDeclineProbe"
	case SeedWindowSiteBoxLegRef:
		return "boxLegRef"
	case SeedWindowSiteBoxDottedSplit:
		return "boxDottedSplit"
	case SeedWindowSiteBoxSurvivorQOV:
		return "boxSurvivorQOV"
	case SeedWindowSiteBoxSurvivorCorrelation:
		return "boxSurvivorCorrelation"
	case SeedWindowSiteGatheredGroupSlot:
		return "gatheredGroupSlot"
	default:
		return "unknown"
	}
}

// SeedWindowKeyClass is one lookup's bucket. The ten partition every call.
//
// The first five are the IDENTITY-IN-HAND cut: the site held a
// CorrelationIdentifier and the key was its upper fold, so both keys can be
// tried and compared. The next four are the TEXT-ONLY cut: no identifier
// existed, so the only thing measurable is whether the window the text found
// states an alias that folds back to the key — i.e. whether a mint from the key
// would be a round-trip or a forgery. The last is the no-key call.
type SeedWindowKeyClass int

const (
	// SeedWindowIdentityAgreesHit: identity in hand, text hit, and an
	// identity-keyed lookup selects a window with the SAME offset and the same
	// column list. Re-keying this call is a refactor.
	SeedWindowIdentityAgreesHit SeedWindowKeyClass = iota
	// SeedWindowIdentityAgreesMiss: identity in hand, and BOTH keys miss. The
	// reference is not a leg either way — re-keying is a refactor here too, but
	// vacuously, so it is counted apart from the hits it must not be allowed to
	// pad.
	SeedWindowIdentityAgreesMiss
	// SeedWindowTextOnlyHit: identity in hand, TEXT hit, identity missed.
	// Re-keying this call silently stops resolving a leg it resolves today. This
	// is the population that BLOCKS conversion.
	SeedWindowTextOnlyHit
	// SeedWindowIdentityOnlyHit: identity in hand, text MISSED, identity hit.
	// Re-keying starts resolving something the text reader passes through — also
	// a behaviour change, in the other direction.
	SeedWindowIdentityOnlyHit
	// SeedWindowKeyDiverged: identity in hand, BOTH hit, and they select
	// DIFFERENT windows. Two keys for one reference, disagreeing about which
	// slots it reads. Not a residue — a contradiction.
	SeedWindowKeyDiverged

	// SeedWindowTextSiteHitAliasIsKey: no identifier at the site, text hit, and
	// the window's own Alias is EXACTLY the key text. A mint from the key would
	// reproduce the identifier already sitting in the window — so the window can
	// serve the identity, but the LOOKUP still has to find it by text.
	//
	// The test is exact rather than case-folding, and that is the whole point of
	// having it. The two alias namespaces are deliberately case-DISJOINT (user
	// correlations upper-folded at the semantic scope, machine mints lowercase),
	// so a key `Q$5` over a window whose alias is `q$5` would pass a folding test
	// and still mint an identifier SameLeg refuses to match — which is the
	// forgery, not an approximation of it.
	SeedWindowTextSiteHitAliasIsKey
	// SeedWindowTextSiteHitAliasDiffers: no identifier, text hit, and the
	// window's Alias is not the key text. A mint from the key would forge an
	// identifier the window disagrees with — including the case-variant form,
	// which is the disjointness above being violated rather than tolerated.
	SeedWindowTextSiteHitAliasDiffers
	// SeedWindowTextSiteHitNoAlias: no identifier, text hit, window states no
	// alias at all.
	SeedWindowTextSiteHitNoAlias
	// SeedWindowTextSiteMiss: no identifier, text missed.
	SeedWindowTextSiteMiss

	// SeedWindowNoKey: the call carried neither a key nor an identifier (the
	// bare-column arm, which falls through to a scan).
	SeedWindowNoKey

	seedWindowKeyClassCount
)

func (c SeedWindowKeyClass) String() string {
	switch c {
	case SeedWindowIdentityAgreesHit:
		return "identityAgreesHit"
	case SeedWindowIdentityAgreesMiss:
		return "identityAgreesMiss"
	case SeedWindowTextOnlyHit:
		return "TEXT-ONLY-HIT"
	case SeedWindowIdentityOnlyHit:
		return "IDENTITY-ONLY-HIT"
	case SeedWindowKeyDiverged:
		return "DIVERGED"
	case SeedWindowTextSiteHitAliasIsKey:
		return "textSiteHitAliasIsKey"
	case SeedWindowTextSiteHitAliasDiffers:
		return "TEXT-SITE-HIT-ALIAS-DIFFERS"
	case SeedWindowTextSiteHitNoAlias:
		return "TEXT-SITE-HIT-NO-ALIAS"
	case SeedWindowTextSiteMiss:
		return "textSiteMiss"
	case SeedWindowNoKey:
		return "noKey"
	default:
		return "unknown"
	}
}

const seedWindowKeyWitnessCap = 192

var (
	seedWindowKeyMu        sync.Mutex
	seedWindowKeyCounts    [seedWindowSiteCount][seedWindowKeyClassCount]int
	seedWindowKeyWitnesses []string
)

// sameSeedWindow reports whether two windows would produce the same read: the
// same starting slot over the same column list.
//
// It compares the WINDOW rather than the map key deliberately. finalizeSeedWindows
// can file one leg under two keys (a box run under its minted `X$BOX` binding and
// its rightmost leaf under the leaf's own name), and where those two entries carry
// the same offset and the same columns, a reader keyed either way reads the same
// slots — that is agreement, not divergence, and calling it divergence would
// manufacture a contradiction out of a naming convention.
func sameSeedWindow(a, b OrdinalSeedLegWindow) bool {
	if a.Offset != b.Offset {
		return false
	}
	if a.Typ == nil || b.Typ == nil {
		return a.Typ == b.Typ
	}
	if len(a.Typ.Fields) != len(b.Typ.Fields) {
		return false
	}
	for i := range a.Typ.Fields {
		if a.Typ.Fields[i].Name != b.Typ.Fields[i].Name {
			return false
		}
	}
	return true
}

// seedWindowByIdentity resolves a correlation against the window map the way an
// identity-keyed reader would: by SameLeg against each window's stated Alias.
//
// The scan is over SORTED keys rather than map order. Two windows can carry the
// same identity (the box-run/rightmost-leaf pair above), and an unordered pick
// between them would make this census's verdict depend on the runtime's map
// seed — a census that reports a different answer on each run is not a
// measurement.
func seedWindowByIdentity(windows map[string]OrdinalSeedLegWindow, corr CorrelationIdentifier) (string, OrdinalSeedLegWindow, bool) {
	if corr.IsZero() {
		return "", OrdinalSeedLegWindow{}, false
	}
	keys := make([]string, 0, len(windows))
	for k := range windows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if SameLeg(windows[k].Alias, corr) {
			return k, windows[k], true
		}
	}
	return "", OrdinalSeedLegWindow{}, false
}

// classifySeedWindowKey decides one lookup's bucket from the facts the reader
// holds. Split from the counter mutation so the decision can be exercised
// without process-global state, exactly as its siblings are.
//
// corr is the CorrelationIdentifier the CALL SITE holds, or the zero identifier
// where the site structurally has none. It is threaded in rather than recovered
// from the window, because the entire question is whether the SITE had one — a
// window's own Alias answers a different question and answering with it would
// report every text-only site as convertible.
func classifySeedWindowKey(windows map[string]OrdinalSeedLegWindow, key string, corr CorrelationIdentifier) (SeedWindowKeyClass, string) {
	if key == "" && corr.IsZero() {
		return SeedWindowNoKey, "no key, no identity"
	}
	textW, textHit := windows[key]
	if corr.IsZero() {
		switch {
		case !textHit:
			return SeedWindowTextSiteMiss, fmt.Sprintf("key %q missed", key)
		case textW.Alias.IsZero():
			return SeedWindowTextSiteHitNoAlias, fmt.Sprintf("key %q hit a window stating no alias", key)
		case textW.Alias.Name() == key:
			return SeedWindowTextSiteHitAliasIsKey, fmt.Sprintf("key %q hit, window alias is exactly it", key)
		default:
			return SeedWindowTextSiteHitAliasDiffers,
				fmt.Sprintf("key %q hit, but the window's alias is %q", key, textW.Alias.Name())
		}
	}
	identKey, identW, identHit := seedWindowByIdentity(windows, corr)
	switch {
	case textHit && identHit && sameSeedWindow(textW, identW):
		return SeedWindowIdentityAgreesHit,
			fmt.Sprintf("corr %q, key %q, identity key %q, same window", corr.Name(), key, identKey)
	case textHit && identHit:
		return SeedWindowKeyDiverged,
			fmt.Sprintf("corr %q: key %q -> offset %d, identity key %q -> offset %d",
				corr.Name(), key, textW.Offset, identKey, identW.Offset)
	case textHit:
		return SeedWindowTextOnlyHit,
			fmt.Sprintf("corr %q: key %q hit at offset %d, no window states that identity",
				corr.Name(), key, textW.Offset)
	case identHit:
		return SeedWindowIdentityOnlyHit,
			fmt.Sprintf("corr %q: key %q missed, identity key %q hits at offset %d",
				corr.Name(), key, identKey, identW.Offset)
	default:
		return SeedWindowIdentityAgreesMiss, fmt.Sprintf("corr %q, key %q: both miss", corr.Name(), key)
	}
}

// RecordSeedWindowKeyLookup counts one keyed read of a seed-window map. Callers
// must guard on LegIdentityCensusEnabled().
func RecordSeedWindowKeyLookup(site SeedWindowSite, windows map[string]OrdinalSeedLegWindow, key string, corr CorrelationIdentifier) {
	class, detail := classifySeedWindowKey(windows, key, corr)
	seedWindowKeyMu.Lock()
	defer seedWindowKeyMu.Unlock()
	if site < 0 || site >= seedWindowSiteCount {
		return
	}
	seedWindowKeyCounts[site][class]++
	// Only the classes that DECIDE something get a witness. The two agreeing
	// classes are the bulk of the traffic and their witnesses would evict the
	// ones a reader of this report needs.
	switch class {
	case SeedWindowIdentityAgreesHit, SeedWindowIdentityAgreesMiss, SeedWindowTextSiteMiss, SeedWindowNoKey:
		return
	}
	addSeedWindowKeyWitness(fmt.Sprintf("%s [%s] %s", site, class, detail))
}

func addSeedWindowKeyWitness(w string) {
	if len(seedWindowKeyWitnesses) >= seedWindowKeyWitnessCap {
		return
	}
	for _, seen := range seedWindowKeyWitnesses {
		if seen == w {
			return
		}
	}
	seedWindowKeyWitnesses = append(seedWindowKeyWitnesses, w)
}

// SeedWindowKeyCensus reports the per-site class matrix and the retained
// witnesses.
func SeedWindowKeyCensus() ([seedWindowSiteCount][seedWindowKeyClassCount]int, []string) {
	seedWindowKeyMu.Lock()
	defer seedWindowKeyMu.Unlock()
	out := make([]string, len(seedWindowKeyWitnesses))
	copy(out, seedWindowKeyWitnesses)
	return seedWindowKeyCounts, out
}

// ResetSeedWindowKeyCensus clears the counters.
func ResetSeedWindowKeyCensus() {
	seedWindowKeyMu.Lock()
	defer seedWindowKeyMu.Unlock()
	seedWindowKeyCounts = [seedWindowSiteCount][seedWindowKeyClassCount]int{}
	seedWindowKeyWitnesses = nil
}

// FormatSeedWindowKeyCensus renders the census as a per-site table for a harness
// to log.
func FormatSeedWindowKeyCensus() string {
	counts, witnesses := SeedWindowKeyCensus()
	var b strings.Builder
	b.WriteString("seed-window key provenance (per keyed lookup):")
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		total := 0
		for c := 0; c < int(seedWindowKeyClassCount); c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(&b, "\n  %-24s calls %d", s, total)
		if total == 0 {
			continue
		}
		for c := SeedWindowKeyClass(0); c < seedWindowKeyClassCount; c++ {
			if counts[s][c] != 0 {
				fmt.Fprintf(&b, " | %s %d", c, counts[s][c])
			}
		}
	}
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  deciding witnesses (%d, cap %d):\n    %s",
			len(sorted), seedWindowKeyWitnessCap, strings.Join(sorted, "\n    "))
	}
	return b.String()
}

// SeedWindowKeyFloors is the minimum population this census must report over a
// whole suite run.
//
// The census's finding is a set of per-site dispositions, and every disposition
// that licenses a conversion is of the form "this site's blocking class is
// EMPTY". An unreached site reports exactly that shape — every blocking class
// zero, every zero vacuous — and it reads on the report identically to a site
// measured clean. The floors are what tell those two apart.
//
// Floored per SITE rather than in total, because the sites do not substitute for
// one another: the box-wrap sites can carry thousands of calls while the
// existential rebase goes dark, and a total floor would not notice.
type SeedWindowKeyFloors struct {
	// Calls is the per-site minimum lookup count, indexed by site. A zero entry
	// means the site is not floored.
	Calls [seedWindowSiteCount]int
}

// AssertSeedWindowKeyCensus checks the census's hard zero and its floors, and
// reports whether it failed.
//
// The hard zero is SeedWindowKeyDiverged. It is not a residue to shrink: it says
// one reference resolved to different slots depending on which of the two
// co-resident keys was consulted, which means one of the two is already reading
// the wrong columns. Wherever it appears the answer is at the PRODUCER that filed
// the window, not at the reader that found it.
//
// floors is nil when the run is NARROWED, exactly as its siblings do it.
func AssertSeedWindowKeyCensus(w io.Writer, floors *SeedWindowKeyFloors) bool {
	counts, _ := SeedWindowKeyCensus()
	return assertSeedWindowKeyCounts(w, counts, floors)
}

func assertSeedWindowKeyCounts(w io.Writer, counts [seedWindowSiteCount][seedWindowKeyClassCount]int, floors *SeedWindowKeyFloors) bool {
	failed := false
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		if counts[s][SeedWindowKeyDiverged] != 0 {
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW KEY CENSUS FAIL: %s reported %d DIVERGED lookups, want 0.\n"+
				"  A reference's own correlation and the upper fold of that same correlation\n"+
				"  selected DIFFERENT windows of one seed — different starting slots or\n"+
				"  different columns. Those are two keys for one leg and only one of them can\n"+
				"  be right, so this is not a residue to shrink but a mis-filed window to\n"+
				"  find. Look at the producer (finalizeSeedWindows and whoever built the leg\n"+
				"  table it walks), not at this reader.\n", s, counts[s][SeedWindowKeyDiverged])
		}
	}
	if floors == nil {
		return failed
	}
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		if floors.Calls[s] == 0 {
			continue
		}
		total := 0
		for c := 0; c < int(seedWindowKeyClassCount); c++ {
			total += counts[s][c]
		}
		if total < floors.Calls[s] {
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW KEY CENSUS FAIL: %s reached %d lookups, want >= %d.\n"+
				"  Nothing keyed this site's window map, so every per-class zero it reports\n"+
				"  holds vacuously — including the blocking classes whose emptiness is what\n"+
				"  licenses re-keying the site by identity. An unreached site and a site\n"+
				"  measured clean print the same line.\n", s, total, floors.Calls[s])
		}
	}
	return failed
}
