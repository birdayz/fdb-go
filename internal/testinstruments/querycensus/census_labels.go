package querycensus

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// The census enums' report names, and what each site or class means, live here
// rather than in production: wrap a value in its Label type where a report prints it.

// AccessorPathClassLabel prints a values.AccessorPathClass.
type AccessorPathClassLabel values.AccessorPathClass

func (c AccessorPathClassLabel) String() string {
	switch values.AccessorPathClass(c) {
	// AccessorPathOKAllBaked: a path was returned and EVERY accessor came from a
	// Resolved FieldPath. The names were read off resolved accessors, so an
	// ordinal identity existed for the whole path and the name domain was a
	// choice rather than a necessity. This is the population that RFC-187 §8
	// could convert without needing anything new.
	case values.AccessorPathOKAllBaked:
		return "OK-all-baked"
	// AccessorPathOKHasLazy: a path was returned but at least one accessor came
	// from a LAZY node, where the name is the only identity that exists. This is
	// the population that cannot be converted without resolve-at-mint.
	case values.AccessorPathOKHasLazy:
		return "OK-has-lazy"
	// AccessorPathDeclineDotted is THE RATCHET ARM: a lazy Field containing '.'.
	// Declined rather than split. See the header for why zero and non-zero mean
	// opposite things.
	case values.AccessorPathDeclineDotted:
		return "DECLINE-lazy-dotted"
	// AccessorPathDeclinePureOrdinal: a resolved accessor with no name at all
	// (Field == ""), so there is nothing to compare in the name domain. This is
	// the one decline that is a pure consequence of the match side being
	// name-based — the value HAS an identity, and it is the comparison that
	// cannot use it.
	case values.AccessorPathDeclinePureOrdinal:
		return "DECLINE-pure-ordinal"
	// AccessorPathDeclineEmptyName: a lazy node with no name and no resolution —
	// no identity of any kind.
	case values.AccessorPathDeclineEmptyName:
		return "DECLINE-empty-name"
	// AccessorPathDeclineNotAColumn: the walk reached a root with no accessors,
	// so the value is not a column reference. The ordinary negative.
	case values.AccessorPathDeclineNotAColumn:
		return "DECLINE-not-a-column"
	default:
		return "unknown"
	}
}

// SelectResultMintSiteLabel prints a values.SelectResultMintSite.
type SelectResultMintSiteLabel values.SelectResultMintSite

func (s SelectResultMintSiteLabel) String() string {
	// SelectResultMintExistsSelect is the SQL translator's EXISTS-bearing select
	// builder, which mints NewQuantifiedObjectValue(outerQ.GetAlias()) — a bare
	// UNTYPED QuantifiedObjectValue — whenever no result override is supplied.
	// This is the site Java's GraphExpansion.java:401 types unconditionally.
	if values.SelectResultMintSite(s) == values.SelectResultMintExistsSelect {
		return "translator buildExistsSelect(MINT)"
	}
	return "unknown"
}

// QualifierRecoverySiteLabel prints a values.QualifierRecoverySite.
type QualifierRecoverySiteLabel values.QualifierRecoverySite

func (s QualifierRecoverySiteLabel) String() string {
	switch values.QualifierRecoverySite(s) {
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
	case values.QualRecSiteRecursiveRemap:
		return "recursiveRemap"
	// QualRecSiteExistsSortSplit is query.splitQualifier (cascades_translator.go),
	// the EXISTS fold's LAST-dot split, recorded at its two callers
	// (sortKeySourceValue, resolveKeyName). Its own doc concedes the deeper
	// case: `A.B.C` is treated as qualifier `A.B`, column `C`.
	case values.QualRecSiteExistsSortSplit:
		return "existsSortSplit"
	// QualRecSiteDerivedUnnestSource is the stable identity of the retired
	// derived-UNNEST descriptor/backtracking classifier. Bound collection
	// consumption does not record traffic here, including CARRIED.
	case values.QualRecSiteDerivedUnnestSource:
		return "derivedUnnestSource"
	// QualRecSiteProjScopeClassify is the stable identity of the retired
	// projection-scope classifier. Exact semantic projection resolution carries
	// its source identity directly; every class at this site is a revival.
	case values.QualRecSiteProjScopeClassify:
		return "projScopeClassify"
	// QualRecSiteProjQualVsScan is embedded's projected-column qualifier check
	// (cascades_generator.go): a manufactured qualifier matched against the
	// scan's name/alias, raising ErrCodeUndefinedColumn on a mismatch. This one
	// decides an ERROR, which is the sharpest consequence in the family.
	case values.QualRecSiteProjQualVsScan:
		return "projQualVsScan"
	// QualRecSiteDisplayLabelStrip is the stable identity of the retired
	// display-label strip, which split a rendered projection key to label a
	// column. Result columns are now the plan's result row type and SQL labels
	// the logical output names, so every class at this site is a revival.
	case values.QualRecSiteDisplayLabelStrip:
		return "displayLabelStrip"
	default:
		return "unknown"
	}
}

// QualifierRecoveryClassLabel prints a values.QualifierRecoveryClass.
type QualifierRecoveryClassLabel values.QualifierRecoveryClass

func (c QualifierRecoveryClassLabel) String() string {
	switch values.QualifierRecoveryClass(c) {
	// QualRecCarried: a structured identity decided. No rendered name was sliced.
	case values.QualRecCarried:
		return "carried"
	// QualRecAgreed: a qualifier was manufactured from text AND a structured
	// identity in hand agrees with it. THE CONVERSION-READY POPULATION.
	case values.QualRecAgreed:
		return "AGREED"
	// QualRecDiverged: manufactured from text, identity in hand DISAGREES. A live
	// misread — asserted ZERO.
	case values.QualRecDiverged:
		return "DIVERGED"
	// QualRecManufactured: manufactured from text with NO counterparty. THE HARD
	// DEBT — not convertible by a local edit.
	case values.QualRecManufactured:
		return "MANUFACTURED"
	// QualRecLeafOnly: a dot was found, the leaf taken, the qualifier DISCARDED
	// because an ordinal or identity decides the read. Not debt.
	case values.QualRecLeafOnly:
		return "leafOnly"
	// QualRecBare: the split ran, no dot, nothing manufactured.
	case values.QualRecBare:
		return "bare"
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
	case values.QualRecHeuristicDecline:
		return "heuristicDecline"
	default:
		return "unknown"
	}
}

// InnerLegProducerLabel prints a values.InnerLegProducer.
type InnerLegProducerLabel values.InnerLegProducer

func (p InnerLegProducerLabel) String() string {
	switch values.InnerLegProducer(p) {
	// InnerLegProducerClusteredOuter is clusteredOuterOrdinalSeed, serving the
	// GATED MULTI-TABLE outer.
	case values.InnerLegProducerClusteredOuter:
		return "clusteredOuterOrdinalSeed"
	// InnerLegProducerSingleSource is scalarSubqueryOrdinalSeed, serving the
	// SINGLE-SOURCE outer (clusterArity == 1).
	case values.InnerLegProducerSingleSource:
		return "scalarSubqueryOrdinalSeed"
	}
	return "unknown"
}

// SeedWindowSiteLabel prints a values.SeedWindowSite.
type SeedWindowSiteLabel values.SeedWindowSite

func (s SeedWindowSiteLabel) String() string {
	switch values.SeedWindowSite(s) {
	// SeedWindowSiteExistentialRebase is cascades.rebaseOuterLegValueOrdinal's
	// per-reference window lookup — the EXISTS-over-join ordinal rebase, keyed by
	// the reference's own QuantifiedObjectValue correlation.
	//
	// RETIRED: it reads ZERO. Java-aligned select partitioning keeps an
	// existential's correlated predicates in the partition holding the
	// existential, so none reaches a two-quantifier select beside an ordinal-seed
	// outer. See SeedWindowReaderFloors.Retired.
	case values.SeedWindowSiteExistentialRebase:
		return "existentialRebase"
	// SeedWindowSiteBoxLegRef is query.rebaseLegRefsToBox's QOV-shaped arm, keyed
	// by the reference's own correlation. It is also where CHILDLESS-BAKED is
	// recorded: the same walk, one arm further down.
	case values.SeedWindowSiteBoxLegRef:
		return "boxLegRef"
	// SeedWindowSiteBoxSurvivorQOV is that walk's post-verification: does any
	// leg-correlated QuantifiedObjectValue survive the rebase? Keyed by the
	// surviving QOV's own correlation. Its reads are overwhelmingly MISSES by
	// construction — a hit is a decline — so its population, not its outcome, is
	// what this census is watching.
	case values.SeedWindowSiteBoxSurvivorQOV:
		return "boxSurvivorQOV"
	// SeedWindowSiteBoxSurvivorCorrelation is the wrap's correct-or-decline net
	// over a translated subquery's correlation set, keyed by each correlation in
	// that set. The smallest population of the five and the one most likely to go
	// dark unnoticed.
	case values.SeedWindowSiteBoxSurvivorCorrelation:
		return "boxSurvivorCorrelation"
	// SeedWindowSiteGatheredGroupSlot is query.slotInGatheredSeed's qualified
	// arm — a group-by key or aggregate operand resolving to a flat seed slot. It
	// is where QUALIFIED-NO-IDENTITY is recorded.
	case values.SeedWindowSiteGatheredGroupSlot:
		return "gatheredGroupSlot"
	default:
		return "unknown"
	}
}

// SeedWindowReadClassLabel prints a values.SeedWindowReadClass.
type SeedWindowReadClassLabel values.SeedWindowReadClass

func (c SeedWindowReadClassLabel) String() string {
	switch values.SeedWindowReadClass(c) {
	// SeedWindowHit: the reference's identity selected a window.
	case values.SeedWindowHit:
		return "hit"
	// SeedWindowMiss: no window is filed under that identity. Not an error at any
	// of these sites — a non-leg reference, or (at the survivor sites) the
	// ordinary case.
	case values.SeedWindowMiss:
		return "miss"
	// SeedWindowQualifiedNoIdentity: a QUALIFIED reference arrived with no
	// correlation, so no window could be selected for it and it declined. HARD
	// ZERO. See the header for what a non-zero re-arms.
	case values.SeedWindowQualifiedNoIdentity:
		return "QUALIFIED-NO-IDENTITY"
	// SeedWindowChildlessBaked: a CHILDLESS source-relative baked read reached
	// the rebase walk's tail, so no window could be selected for it and the whole
	// wrap declined. HARD ZERO. See the header.
	case values.SeedWindowChildlessBaked:
		return "CHILDLESS-BAKED"
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
	case values.SeedWindowNestedHit:
		return "NESTED-HIT"
	default:
		return "unknown"
	}
}

// LegIdentitySiteLabel prints a values.LegIdentitySite.
type LegIdentitySiteLabel values.LegIdentitySite

func (s LegIdentitySiteLabel) String() string {
	switch values.LegIdentitySite(s) {
	// LegSiteRowLegsBinder is the RUNTIME leg binder
	// (executor.rowLegsBinder.GetCorrelationBinding): it resolves a correlation
	// to its window within a merged row's own Type.Legs. Compares EXACTLY today.
	case values.LegSiteRowLegsBinder:
		return "rowLegsBinder.GetCorrelationBinding (runtime leg binder)"
	// LegSiteBuriedLegWindow is executor.buriedLegWindow: the sub-window of a
	// buried leg inside a clustered box span. Compares EXACTLY today.
	case values.LegSiteBuriedLegWindow:
		return "buriedLegWindow (buried sub-window in a box span)"
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
	case values.LegSiteTextVsIdentity:
		return "leg text vs leg identity (Name vs Alias)"
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
	case values.LegSiteLeftOuterExistential:
		return "hoistLegRefsOntoMergedRow (drift check)"
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
	case values.LegSiteFinalizeSeedWindows:
		return "finalizeSeedWindows (buried sub-window derivation)"
	// LegSiteSelectOutputLegs is the translator's expressionOutputLegs, recorded at
	// the PRODUCER rather than at a reader.
	//
	// It is the one site whose two spellings come from genuinely independent
	// sources — the leg's identity from its quantifier, its text from the select's
	// parallel sourceAliases slice — so nothing downstream guarantees they agree.
	// Recording it here rather than trusting a reader to walk these legs is what
	// keeps the text-vs-identity zero from resting on an unmeasured producer.
	case values.LegSiteSelectOutputLegs:
		return "expressionOutputLegs (producer: quantifier vs sourceAliases)"
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
	case values.LegSiteNLJPlanAlias:
		return "NLJ plan leg identity (planner: sourceAlias text vs quantifier)"
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
	case values.LegSiteOrdinalSlotInLegWindow:
		return "ordinalSlotInLegWindow (qualified slot within a leg window)"
	default:
		return "unknown leg identity site"
	}
}

// OrderingComparisonSiteLabel prints a values.OrderingComparisonSite.
type OrderingComparisonSiteLabel values.OrderingComparisonSite

func (s OrderingComparisonSiteLabel) String() string {
	switch values.OrderingComparisonSite(s) {
	// OrderingSiteRequestedVsCandidate is orderingValuesEqualIn: a requested
	// ordering key against a match candidate's ordering key.
	case values.OrderingSiteRequestedVsCandidate:
		return "orderingValuesEqualIn (requested vs candidate)"
	// OrderingSiteIntersectionKeys is intersectionValuesEqualIn: the primary-key
	// intersection's own comparison-key, equality-bound and implicit
	// discriminator lists.
	case values.OrderingSiteIntersectionKeys:
		return "intersectionValuesEqualIn (intersection key lists)"
	default:
		return "unknown ordering comparison site"
	}
}

// UnnestLegMintSiteLabel prints a values.UnnestLegMintSite.
type UnnestLegMintSiteLabel values.UnnestLegMintSite

func (s UnnestLegMintSiteLabel) String() string {
	switch values.UnnestLegMintSite(s) {
	// UnnestLegMintSiteNonChainedMerge is the ELSE of the chained-unnest check
	// in the filter-over-unnest merge. It applies no seed test at all — the
	// `ordinalSeed` test lives INSIDE the chained arm, on the other side of this
	// branch — so no seed-gate flip converts it.
	case values.UnnestLegMintSiteNonChainedMerge:
		return "nonChainedMerge(no seed test)"
	// UnnestLegMintSiteAnchoredNonExists is the plain non-chained unnest merge's
	// anchored (not admitted to the gather / no record) arm, which the code
	// names "the correct and now ONLY domain of the name-keyed rebase". Also no
	// seed test.
	case values.UnnestLegMintSiteAnchoredNonExists:
		return "anchoredNonExists(no seed test)"
	// UnnestLegMintSiteBuriedNotWindowed is the BURIED subquery-internal
	// outer-only filter's `!seedWindowed` else-branch. Its ordinal twin
	// (rebaseUnnestOuterLegPredicateOrdinal) is selected on the other side.
	case values.UnnestLegMintSiteBuriedNotWindowed:
		return "buried(!seedWindowed)"
	// UnnestLegMintSiteJoinPredNotWindowed is the EXISTS JoinPredicate channel's
	// `!seedWindowed` arm. Same twin on the other side.
	case values.UnnestLegMintSiteJoinPredNotWindowed:
		return "joinPredicate(!seedWindowed)"
	// UnnestLegMintSiteChainedNameModel is the chained rebase's name-model
	// fallback, taken when the chained seed is not ordinal.
	case values.UnnestLegMintSiteChainedNameModel:
		return "chained(name-model seed)"
	}
	return "unknown"
}

// UnnestLegMintArmLabel prints a values.UnnestLegMintArm.
type UnnestLegMintArmLabel values.UnnestLegMintArm

func (a UnnestLegMintArmLabel) String() string {
	switch values.UnnestLegMintArm(a) {
	// UnnestLegMintArmName: the branch took the name-keyed rebase — the arm the
	// mint counters above measure.
	case values.UnnestLegMintArmName:
		return "name"
	// UnnestLegMintArmOrdinalTwin: the branch took
	// rebaseUnnestOuterLegPredicateOrdinal. This is the reading that turns a zero
	// on the name arm from "dead shape" into "already converted here".
	case values.UnnestLegMintArmOrdinalTwin:
		return "ordinal-twin"
	// UnnestLegMintArmPlanTimeBake: the branch took the E-1a plan-time bake
	// (bakeInnerExistsPredicateOrdinal), which reaches the ordinal twin one level
	// down. Counted apart from the twin arm because the two are different
	// DECISIONS that happen to share a callee, and folding them would report an
	// inner-cluster bake as a seed-test outcome.
	case values.UnnestLegMintArmPlanTimeBake:
		return "planTimeBake"
	// UnnestLegMintArmLegRelative: the JoinPredicate channel's windowed,
	// non-plan-time-bake fall-through, which rebases NOTHING and leaves the refs
	// leg-relative for the executor's below-FOD hoist. It is an arm of the branch
	// and therefore counted; without it the branch's arms do not partition and a
	// zero elsewhere cannot be read.
	case values.UnnestLegMintArmLegRelative:
		return "leg-relative"
	}
	return "unknown"
}
