package values

import (
	"fmt"
	"strings"
)

// The vocabulary record sites pass to CensusHooks. It stays in production because
// the sites name it; the counting and reporting live in pkg/internal/querycensus.

// AccessorPathClass is one call's outcome. These partition every call to
// AccessorNamePath.
type AccessorPathClass int

const (
	// AccessorPathOKAllBaked: a path was returned and EVERY accessor came from a
	// Resolved FieldPath. The names were read off resolved accessors, so an
	// ordinal identity existed for the whole path and the name domain was a
	// choice rather than a necessity. This is the population that RFC-187 §8
	// could convert without needing anything new.
	AccessorPathOKAllBaked AccessorPathClass = iota

	// AccessorPathOKHasLazy: a path was returned but at least one accessor came
	// from a LAZY node, where the name is the only identity that exists. This is
	// the population that cannot be converted without resolve-at-mint.
	AccessorPathOKHasLazy

	// AccessorPathDeclineDotted is THE RATCHET ARM: a lazy Field containing '.'.
	// Declined rather than split. See the header for why zero and non-zero mean
	// opposite things.
	AccessorPathDeclineDotted

	// AccessorPathDeclinePureOrdinal: a resolved accessor with no name at all
	// (Field == ""), so there is nothing to compare in the name domain. This is
	// the one decline that is a pure consequence of the match side being
	// name-based — the value HAS an identity, and it is the comparison that
	// cannot use it.
	AccessorPathDeclinePureOrdinal

	// AccessorPathDeclineEmptyName: a lazy node with no name and no resolution —
	// no identity of any kind.
	AccessorPathDeclineEmptyName

	// AccessorPathDeclineNotAColumn: the walk reached a root with no accessors,
	// so the value is not a column reference. The ordinary negative.
	AccessorPathDeclineNotAColumn

	AccessorPathClassCount
)

func (c AccessorPathClass) String() string {
	switch c {
	case AccessorPathOKAllBaked:
		return "OK-all-baked"
	case AccessorPathOKHasLazy:
		return "OK-has-lazy"
	case AccessorPathDeclineDotted:
		return "DECLINE-lazy-dotted"
	case AccessorPathDeclinePureOrdinal:
		return "DECLINE-pure-ordinal"
	case AccessorPathDeclineEmptyName:
		return "DECLINE-empty-name"
	case AccessorPathDeclineNotAColumn:
		return "DECLINE-not-a-column"
	default:
		return "unknown"
	}
}

// SelectResultMintSite names one non-test select-result-value construction.
//
// The identity is the SITE, not the file position: line numbers on this path
// have already invalidated several written-down attributions of this population.
type SelectResultMintSite int

const (
	// SelectResultMintExistsSelect is the SQL translator's EXISTS-bearing select
	// builder, which mints NewQuantifiedObjectValue(outerQ.GetAlias()) — a bare
	// UNTYPED QuantifiedObjectValue — whenever no result override is supplied.
	// This is the site Java's GraphExpansion.java:401 types unconditionally.
	SelectResultMintExistsSelect SelectResultMintSite = iota

	SelectResultMintSiteCount
)

func (s SelectResultMintSite) String() string {
	if s == SelectResultMintExistsSelect {
		return "translator buildExistsSelect(MINT)"
	}
	return "unknown"
}

// quantifiedObjectValueCarriesAType reports whether a QOV carries a REAL flowed
// type rather than the UnknownType placeholder.
//
// The discriminator is the type CODE and not pointer identity against the
// UnknownType singleton: an equivalent unknown built elsewhere is just as
// untyped, and keying on the shared variable would call it typed. `Typ != nil`
// would be a tautology — every constructible QOV has a non-nil Typ.
// Both this and the spelling below read the HANDLE rather than FlowedType().
// They are census helpers, so they run on every minted QOV whether or not the
// census is reporting, and FlowedType() would build — and drop — a whole Type
// graph per call to read one field off it.
//
// The nil receiver guard is not defensive noise: FlowedType() answers nil for a
// nil receiver, so it was already total, and reading the field directly is what
// makes the check necessary.
func quantifiedObjectValueCarriesAType(qov *quantifiedObjectValue) bool {
	return qov != nil && qov.flowed != nil && qov.flowed.code != TypeCodeUnknown
}

// describeMintedQOVType spells the flowed type, with a record's ARITY, because
// the boolean above and the spelling answer different questions: a mint counted
// as typed-but-not-a-row is a different fact from one carrying a real row.
//
// anyRecord is excluded from the arity arm deliberately, preserving what the
// thawed spelling did: the erased record thaws to anyRecordType, which is not a
// *RecordType, so it fell through to the bare code and printed RECORD. It has no
// field list to count, and printing RecordType(0) for it would make the erased
// shape indistinguishable from the concrete zero-field unit record.
func describeMintedQOVType(qov *quantifiedObjectValue) string {
	if qov == nil || qov.flowed == nil {
		return "<nil>"
	}
	if qov.flowed.code == TypeCodeRecord && !qov.flowed.anyRecord {
		return fmt.Sprintf("RecordType(%d)", len(qov.flowed.fields))
	}
	return qov.flowed.code.String()
}

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

	// QualRecSiteDerivedUnnestSource is the stable identity of the retired
	// derived-UNNEST descriptor/backtracking classifier. Bound collection
	// consumption does not record traffic here, including CARRIED.
	QualRecSiteDerivedUnnestSource

	// QualRecSiteProjScopeClassify is the stable identity of the retired
	// projection-scope classifier. Exact semantic projection resolution carries
	// its source identity directly; every class at this site is a revival.
	QualRecSiteProjScopeClassify

	// QualRecSiteProjQualVsScan is embedded's projected-column qualifier check
	// (cascades_generator.go): a manufactured qualifier matched against the
	// scan's name/alias, raising ErrCodeUndefinedColumn on a mismatch. This one
	// decides an ERROR, which is the sharpest consequence in the family.
	QualRecSiteProjQualVsScan

	// QualRecSiteDisplayLabelStrip is the stable identity of the retired
	// display-label strip, which split a rendered projection key to label a
	// column. Result columns are now the plan's result row type and SQL labels
	// the logical output names, so every class at this site is a revival.
	QualRecSiteDisplayLabelStrip

	QualRecSiteCount
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
	// only instance, the retired display-label strip's rejection on `()`, read a
	// rendered aggregate/function label out of a string by looking for
	// parentheses in it.
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

	QualRecClassCount
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

// SeedWindowSite is one keyed reader of a seed-window map. These are ALL of them
// in production. The map's remaining call sites consume it as a nil/non-nil
// PREDICATE ("is this an ordinal seed?") and never key it, which is not a read
// and is not counted here.
type SeedWindowSite int

const (
	// SeedWindowSiteExistentialRebase is cascades.rebaseOuterLegValueOrdinal's
	// per-reference window lookup — the EXISTS-over-join ordinal rebase, keyed by
	// the reference's own QuantifiedObjectValue correlation.
	//
	// RETIRED: it reads ZERO. Java-aligned select partitioning keeps an
	// existential's correlated predicates in the partition holding the
	// existential, so none reaches a two-quantifier select beside an ordinal-seed
	// outer. See SeedWindowReaderFloors.Retired.
	SeedWindowSiteExistentialRebase SeedWindowSite = iota

	// SeedWindowSiteBoxLegRef is query.rebaseLegRefsToBox's QOV-shaped arm, keyed
	// by the reference's own correlation. It is also where CHILDLESS-BAKED is
	// recorded: the same walk, one arm further down.
	SeedWindowSiteBoxLegRef

	// SeedWindowSiteBoxSurvivorQOV is that walk's post-verification: does any
	// leg-correlated QuantifiedObjectValue survive the rebase? Keyed by the
	// surviving QOV's own correlation. Its reads are overwhelmingly MISSES by
	// construction — a hit is a decline — so its population, not its outcome, is
	// what this census is watching.
	SeedWindowSiteBoxSurvivorQOV

	// SeedWindowSiteBoxSurvivorCorrelation is the wrap's correct-or-decline net
	// over a translated subquery's correlation set, keyed by each correlation in
	// that set. The smallest population of the five and the one most likely to go
	// dark unnoticed.
	SeedWindowSiteBoxSurvivorCorrelation

	// SeedWindowSiteGatheredGroupSlot is query.slotInGatheredSeed's qualified
	// arm — a group-by key or aggregate operand resolving to a flat seed slot. It
	// is where QUALIFIED-NO-IDENTITY is recorded.
	SeedWindowSiteGatheredGroupSlot

	SeedWindowSiteCount
)

func (s SeedWindowSite) String() string {
	switch s {
	case SeedWindowSiteExistentialRebase:
		return "existentialRebase"
	case SeedWindowSiteBoxLegRef:
		return "boxLegRef"
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

// SeedWindowReadClass is one read's bucket. The four partition every read.
type SeedWindowReadClass int

const (
	// SeedWindowHit: the reference's identity selected a window.
	SeedWindowHit SeedWindowReadClass = iota
	// SeedWindowMiss: no window is filed under that identity. Not an error at any
	// of these sites — a non-leg reference, or (at the survivor sites) the
	// ordinary case.
	SeedWindowMiss
	// SeedWindowQualifiedNoIdentity: a QUALIFIED reference arrived with no
	// correlation, so no window could be selected for it and it declined. HARD
	// ZERO. See the header for what a non-zero re-arms.
	SeedWindowQualifiedNoIdentity
	// SeedWindowChildlessBaked: a CHILDLESS source-relative baked read reached
	// the rebase walk's tail, so no window could be selected for it and the whole
	// wrap declined. HARD ZERO. See the header.
	SeedWindowChildlessBaked

	// SeedWindowNestedHit: the reference's identity selected a window whose Kind
	// is LegKindNested — i.e. the read took the FUSED two-step address rather
	// than flat offset arithmetic.
	//
	// IT IS A HIT, COUNTED SEPARATELY, and it exists to answer one question no
	// other number on this path can: is RFC-200's nested reader arm LIVE on this
	// corpus, or is it correct-but-unentered? Those two states print identically
	// everywhere else — 174 leg-local reads unchanged, EXPLAIN identical,
	// MergedReAnchor 0 — and RFC-200 §6 predicts explicitly that
	// existentialRebase GROWS because the newly-accepted firings' existPreds
	// rebase through it. A prediction with no instrument is a prediction nothing
	// can refute.
	//
	// A read counted here is NOT also counted as SeedWindowHit; the classes
	// partition.
	SeedWindowNestedHit

	SeedWindowReadClassCount
)

func (c SeedWindowReadClass) String() string {
	switch c {
	case SeedWindowHit:
		return "hit"
	case SeedWindowMiss:
		return "miss"
	case SeedWindowQualifiedNoIdentity:
		return "QUALIFIED-NO-IDENTITY"
	case SeedWindowChildlessBaked:
		return "CHILDLESS-BAKED"
	case SeedWindowNestedHit:
		return "NESTED-HIT"
	default:
		return "unknown"
	}
}

// LegIdentitySite identifies one leg-identity comparison site.
type LegIdentitySite int

const (
	// LegSiteRowLegsBinder is the RUNTIME leg binder
	// (executor.rowLegsBinder.GetCorrelationBinding): it resolves a correlation
	// to its window within a merged row's own Type.Legs. Compares EXACTLY today.
	LegSiteRowLegsBinder LegIdentitySite = iota

	// LegSiteBuriedLegWindow is executor.buriedLegWindow: the sub-window of a
	// buried leg inside a clustered box span. Compares EXACTLY today.
	LegSiteBuriedLegWindow

	// LegSiteTextVsIdentity is not a comparison site but the DIVERGENCE census
	// between a leg's two spellings: its recorded pair is (RecordTypeLeg.Name, the
	// same leg's Alias.Name()), sampled at every reader that walks a leg.
	//
	// This is the count that decides whether retyping the readers from Name to
	// Alias is a behaviour change at all. Name and Alias are recorded
	// independently by the producers — several of which normalize one and not the
	// other (a window map keyed by an UPPER fold whose window carries the raw
	// correlation; a select whose sourceAliases entry need not match its
	// quantifier's alias) — so their agreement is an empirical fact, not a
	// structural one. Its zero is what makes the conversion representation-only
	// and the plan-shape golden untouched.
	//
	// It also subsumes the re-mint at executor.spansFromMergedLegs, which used to
	// manufacture a fresh identifier from the leg's text: that re-mint is exactly
	// the operation whose result this compares against the identity the producer
	// actually chose.
	LegSiteTextVsIdentity

	// LegSiteLeftOuterExistential is cascades.hoistLegRefsOntoMergedRow's drift
	// check: a leg absent from the derived windows but present in the merged
	// type's Legs is a LOUD failure. It compares through SameLeg (EXACT), and the
	// recorded pair is that comparison's own: the leg's identity against the
	// reference correlation's. Its retired predicate folded the reference before
	// comparing it to the leg's text, which is why the verdict rather than the pair
	// is what says whether the conversion moved this tripwire.
	//
	// Its absolute totals are a planning-time artifact, not a corpus fact: the
	// hoist runs inside a Cascades rule, so the memo may fire it once or many
	// times for the same query depending on exploration order. Only the RATIOS
	// (foldOnly == 0) carry meaning across runs.
	LegSiteLeftOuterExistential

	// LegSiteFinalizeSeedWindows is values.finalizeSeedWindows' buried-sub-window
	// derivation: "is this buried leg the box run's rightmost leaf?", decided
	// through SameLeg on the leaf's identity against the box window's.
	//
	// It was the last site held back from the conversion, on a justification that
	// was measured to be false: the two identifiers were said to be "legitimately
	// different — box quantifier vs leaf", when the sourceBinding convention is
	// exactly that for the rightmost leaf they are the SAME identifier. The red
	// that was cited as proof came from a test fixture hand-minting the box
	// correlation lowercase where the unnest/mixed producer mints it upper-folded.
	// Measured over the real-FDB corpus, the identity and text comparisons decide
	// identically on all 1311 pairs. The premise now has deterministic pins rather
	// than resting on a count, ONE PER PRODUCER of a box correlation:
	// TestBoxCorrelationIsItsRightmostLeafIdentity for the unnest/mixed mint (the
	// two identifiers agree, so the comparison MATCHES the rightmost leaf) and
	// TestBoxBoxBindingDeclinesAndStillFilesTheLeaf for the pristine gated-join
	// mint (legBinding's "$BOX" suffix makes them differ by design, so BOTH the
	// identity comparison and the retired text one decline, and the leaf
	// sub-window is filed beside the box run rather than replacing it).
	//
	// The map KEYS remain upper-FOLDED text — readers still arrive holding a string
	// — so this one site holds both namespaces at once, deliberately.
	LegSiteFinalizeSeedWindows

	// LegSiteSelectOutputLegs is the translator's expressionOutputLegs, recorded at
	// the PRODUCER rather than at a reader.
	//
	// It is the one site whose two spellings come from genuinely independent
	// sources — the leg's identity from its quantifier, its text from the select's
	// parallel sourceAliases slice — so nothing downstream guarantees they agree.
	// Recording it here rather than trusting a reader to walk these legs is what
	// keeps the text-vs-identity zero from resting on an unmeasured producer.
	LegSiteSelectOutputLegs

	// LegSiteNLJPlanAlias is the materialized nested-loop join's own leg
	// identifiers, recorded at the PLANNER where the plan is constructed.
	//
	// The plan used to hold these as strings, and the executor minted an
	// identifier from each at the plan boundary — so the leg identities on the
	// whole merge path were manufactured downstream of the quantifier that owned
	// them. They are threaded from the select's quantifiers now, and this site
	// records the substitution: the pair is the SOURCE-ALIAS text the plan used to
	// carry against the quantifier identifier it carries instead.
	//
	// The substitution is NOT purely representational, and the measurement is why
	// we know: over the FDB corpus the two disagree on 12 of ~80k firings (the 12 is
	// stable across runs, the denominator is not -- 79040 / 79960 / 80856 measured), and the
	// disagreement is total rather than a case variant — witnesses "q$N vs E". In
	// those the select's source-alias slice carries a RE-MINTED identifier while its
	// quantifier still carries the user alias, so the slice is the stale one and the
	// quantifier is the authority. The old pairing was internally inconsistent
	// there: the seed's QOVs were minted as ToUpper(alias) while the plan's leg
	// identity kept the raw alias, so a lowercase q$N became Q$N in the seed and
	// q$N in the plan, and values.SameLeg — exact — then bound neither.
	// TestReconstructFoldStep1Seed_CarriesTheThreadedIdentityVerbatim pins that.
	//
	// So the zero this site asserts is FoldOnlyEqual, not Neither. Fold-only is the
	// forgery population: a case-ONLY difference means one spelling is an upper fold
	// of the other, which is precisely how ToUpper manufactured a Q$N that could
	// forge a minted q$N leg. A wholly different name is a stale source alias, which
	// threading the quantifier is the fix for.
	LegSiteNLJPlanAlias

	// LegSiteOrdinalSlotInLegWindow is the translator's ordinalSlotInLegWindow: it
	// resolves a qualified column to its slot WITHIN the named leg's window, and its
	// decline is what keeps a qualified `B.ID` from flat-first-matching A's
	// same-named slot. Its counterparty is a correlation, so it compares through
	// SameLeg like every other Group-A reader; it used to compare the leg's text
	// against the UPPER fold of that correlation.
	//
	// This is the site where the instrument's blindness had consequences. Its census
	// recorded (leg text, correlation text) while its comparison evaluated the two
	// identities, so a minted leg read by its own UPPER Name scored an exact MATCH
	// in the census and a DECLINE in the code. Measured with the pair the comparison
	// actually evaluates, the real-FDB corpus reports 0 divergences over 105
	// comparisons; the only such pairs anywhere are the deliberate negative controls
	// in the translator package's own fixture, where the decline is the assertion.
	LegSiteOrdinalSlotInLegWindow

	LegIdentitySiteCount
)

// String names the site for test failure messages.
func (s LegIdentitySite) String() string {
	switch s {
	case LegSiteRowLegsBinder:
		return "rowLegsBinder.GetCorrelationBinding (runtime leg binder)"
	case LegSiteBuriedLegWindow:
		return "buriedLegWindow (buried sub-window in a box span)"
	case LegSiteTextVsIdentity:
		return "leg text vs leg identity (Name vs Alias)"
	case LegSiteLeftOuterExistential:
		return "hoistLegRefsOntoMergedRow (drift check)"
	case LegSiteFinalizeSeedWindows:
		return "finalizeSeedWindows (buried sub-window derivation)"
	case LegSiteSelectOutputLegs:
		return "expressionOutputLegs (producer: quantifier vs sourceAliases)"
	case LegSiteNLJPlanAlias:
		return "NLJ plan leg identity (planner: sourceAlias text vs quantifier)"
	case LegSiteOrdinalSlotInLegWindow:
		return "ordinalSlotInLegWindow (qualified slot within a leg window)"
	default:
		return "unknown leg identity site"
	}
}

// OrderingComparisonSite identifies one of the two ordering-value comparators.
type OrderingComparisonSite int

const (
	// OrderingSiteRequestedVsCandidate is orderingValuesEqualIn: a requested
	// ordering key against a match candidate's ordering key.
	OrderingSiteRequestedVsCandidate OrderingComparisonSite = iota

	// OrderingSiteIntersectionKeys is intersectionValuesEqualIn: the primary-key
	// intersection's own comparison-key, equality-bound and implicit
	// discriminator lists.
	OrderingSiteIntersectionKeys

	OrderingComparisonSiteCount
)

// String names the site for test failure messages.
func (s OrderingComparisonSite) String() string {
	switch s {
	case OrderingSiteRequestedVsCandidate:
		return "orderingValuesEqualIn (requested vs candidate)"
	case OrderingSiteIntersectionKeys:
		return "intersectionValuesEqualIn (intersection key lists)"
	default:
		return "unknown ordering comparison site"
	}
}

// UnnestLegMintSite is one call site of the name-keyed
// `rebaseUnnestOuterLegPredicate`. The five partition its callers, and they are
// named for WHY they reach it rather than for their line numbers, which move.
type UnnestLegMintSite int

const (
	// UnnestLegMintSiteNonChainedMerge is the ELSE of the chained-unnest check
	// in the filter-over-unnest merge. It applies no seed test at all — the
	// `ordinalSeed` test lives INSIDE the chained arm, on the other side of this
	// branch — so no seed-gate flip converts it.
	UnnestLegMintSiteNonChainedMerge UnnestLegMintSite = iota

	// UnnestLegMintSiteAnchoredNonExists is the plain non-chained unnest merge's
	// anchored (not admitted to the gather / no record) arm, which the code
	// names "the correct and now ONLY domain of the name-keyed rebase". Also no
	// seed test.
	UnnestLegMintSiteAnchoredNonExists

	// UnnestLegMintSiteBuriedNotWindowed is the BURIED subquery-internal
	// outer-only filter's `!seedWindowed` else-branch. Its ordinal twin
	// (rebaseUnnestOuterLegPredicateOrdinal) is selected on the other side.
	UnnestLegMintSiteBuriedNotWindowed

	// UnnestLegMintSiteJoinPredNotWindowed is the EXISTS JoinPredicate channel's
	// `!seedWindowed` arm. Same twin on the other side.
	UnnestLegMintSiteJoinPredNotWindowed

	// UnnestLegMintSiteChainedNameModel is the chained rebase's name-model
	// fallback, taken when the chained seed is not ordinal.
	UnnestLegMintSiteChainedNameModel

	UnnestLegMintSiteCount
)

func (s UnnestLegMintSite) String() string {
	switch s {
	case UnnestLegMintSiteNonChainedMerge:
		return "nonChainedMerge(no seed test)"
	case UnnestLegMintSiteAnchoredNonExists:
		return "anchoredNonExists(no seed test)"
	case UnnestLegMintSiteBuriedNotWindowed:
		return "buried(!seedWindowed)"
	case UnnestLegMintSiteJoinPredNotWindowed:
		return "joinPredicate(!seedWindowed)"
	case UnnestLegMintSiteChainedNameModel:
		return "chained(name-model seed)"
	}
	return "unknown"
}

// UnnestLegMintArm is WHICH arm a seed-tested branch point took.
//
// It exists because a per-site call count of ZERO at a `!seedWindowed`
// else-branch is AMBIGUOUS on its own, and the ambiguity has opposite
// consequences. "Never reached" says the shape is not planned; "reached, took
// the ordinal arm" says the shape IS planned and its conversion has already
// happened on the other side of the branch. Both readings support demoting a
// conversion booked against the name arm, and they imply different follow-ups —
// so the census must not leave the reader to pick one.
//
// The arm is recorded AT the arm, and the branch's REACH is recorded
// INDEPENDENTLY at the branch point, before any arm is chosen. Both halves are
// necessary and the second one was missing when this census first shipped: the
// renderer computed the branch total AS the sum of the arms, so an arm added
// without a `CensusHooks.UnnestLegMintArm` call would silently SHRINK the printed
// `reached N` instead of showing up as a gap. That is the summed denominator this
// RFC's own text calls "true by construction", one field over from the twin
// counter that was given a real one.
//
// It bites hardest at the joinPredicate branch, where the census guard doubles as
// the third arm (`else if jpCensus`): inserting an `else if` ahead of it steals
// silently from `leg-relative`. With an independent reach counter that theft is
// an ARM PARTITION FAIL, not a quieter number.
type UnnestLegMintArm int

const (
	// UnnestLegMintArmName: the branch took the name-keyed rebase — the arm the
	// mint counters above measure.
	UnnestLegMintArmName UnnestLegMintArm = iota

	// UnnestLegMintArmOrdinalTwin: the branch took
	// rebaseUnnestOuterLegPredicateOrdinal. This is the reading that turns a zero
	// on the name arm from "dead shape" into "already converted here".
	UnnestLegMintArmOrdinalTwin

	// UnnestLegMintArmPlanTimeBake: the branch took the E-1a plan-time bake
	// (bakeInnerExistsPredicateOrdinal), which reaches the ordinal twin one level
	// down. Counted apart from the twin arm because the two are different
	// DECISIONS that happen to share a callee, and folding them would report an
	// inner-cluster bake as a seed-test outcome.
	UnnestLegMintArmPlanTimeBake

	// UnnestLegMintArmLegRelative: the JoinPredicate channel's windowed,
	// non-plan-time-bake fall-through, which rebases NOTHING and leaves the refs
	// leg-relative for the executor's below-FOD hoist. It is an arm of the branch
	// and therefore counted; without it the branch's arms do not partition and a
	// zero elsewhere cannot be read.
	UnnestLegMintArmLegRelative

	UnnestLegMintArmCount
)

func (a UnnestLegMintArm) String() string {
	switch a {
	case UnnestLegMintArmName:
		return "name"
	case UnnestLegMintArmOrdinalTwin:
		return "ordinal-twin"
	case UnnestLegMintArmPlanTimeBake:
		return "planTimeBake"
	case UnnestLegMintArmLegRelative:
		return "leg-relative"
	}
	return "unknown"
}
