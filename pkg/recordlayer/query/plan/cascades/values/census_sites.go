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
	AccessorPathOKAllBaked AccessorPathClass = iota
	AccessorPathOKHasLazy
	AccessorPathDeclineDotted
	AccessorPathDeclinePureOrdinal
	AccessorPathDeclineEmptyName
	AccessorPathDeclineNotAColumn
	AccessorPathClassCount
)

// SelectResultMintSite names one non-test select-result-value construction.
//
// The identity is the SITE, not the file position: line numbers on this path
// have already invalidated several written-down attributions of this population.
type SelectResultMintSite int

const (
	SelectResultMintExistsSelect SelectResultMintSite = iota
	SelectResultMintSiteCount
)

// quantifiedObjectValueCarriesAType reports whether a QOV carries a REAL flowed
// type rather than the UnknownType placeholder.
//
// The discriminator is the type CODE and not pointer identity against the
// UnknownType singleton: an equivalent unknown built elsewhere is just as
// untyped, and keying on the shared variable would call it typed. `Typ != nil`
// would be a tautology — every constructible QOV has a non-nil Typ.
// Both this and the spelling below read the HANDLE rather than FlowedType().
// They run, through CensusQOVShape, on every QOV minted while the census is on,
// and FlowedType() would build — and drop — a whole Type graph per call to read
// one field off it.
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
// querycensus/name_split_census.go's header. The parseColRef family contributes THREE sites
// rather than one: its 27 production call sites are overwhelmingly display or
// lookup, and only three of them manufacture a qualifier that then DECIDES
// something. Those three are what is counted, individually, because they are
// three different decisions with three different counterparties and a single
// merged "parseColRef" number could not answer the conversion question for any
// of them.
type QualifierRecoverySite int

const (
	QualRecSiteRecursiveRemap QualifierRecoverySite = iota
	QualRecSiteExistsSortSplit
	QualRecSiteDerivedUnnestSource
	QualRecSiteProjScopeClassify
	QualRecSiteProjQualVsScan
	QualRecSiteDisplayLabelStrip
	QualRecSiteCount
)

// QualifierRecoveryClass is one decision's bucket. The six partition every call.
type QualifierRecoveryClass int

const (
	QualRecCarried QualifierRecoveryClass = iota
	QualRecAgreed
	QualRecDiverged
	QualRecManufactured
	QualRecLeafOnly
	QualRecBare
	QualRecHeuristicDecline
	QualRecClassCount
)

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
	InnerLegProducerClusteredOuter InnerLegProducer = iota
	InnerLegProducerSingleSource
)

// SeedWindowSite is one keyed reader of a seed-window map. These are ALL of them
// in production. The map's remaining call sites consume it as a nil/non-nil
// PREDICATE ("is this an ordinal seed?") and never key it, which is not a read
// and is not counted here.
type SeedWindowSite int

const (
	SeedWindowSiteExistentialRebase SeedWindowSite = iota
	SeedWindowSiteBoxLegRef
	SeedWindowSiteBoxSurvivorQOV
	SeedWindowSiteBoxSurvivorCorrelation
	SeedWindowSiteGatheredGroupSlot
	SeedWindowSiteCount
)

// SeedWindowReadClass is one read's bucket. The four partition every read.
type SeedWindowReadClass int

const (
	SeedWindowHit SeedWindowReadClass = iota
	SeedWindowMiss
	SeedWindowQualifiedNoIdentity
	SeedWindowChildlessBaked
	SeedWindowNestedHit
	SeedWindowReadClassCount
)

// LegIdentitySite identifies one leg-identity comparison site.
type LegIdentitySite int

const (
	LegSiteRowLegsBinder LegIdentitySite = iota
	LegSiteBuriedLegWindow
	LegSiteTextVsIdentity
	LegSiteLeftOuterExistential
	LegSiteFinalizeSeedWindows
	LegSiteSelectOutputLegs
	LegSiteNLJPlanAlias
	LegSiteOrdinalSlotInLegWindow
	LegIdentitySiteCount
)

// OrderingComparisonSite identifies one of the two ordering-value comparators.
type OrderingComparisonSite int

const (
	OrderingSiteRequestedVsCandidate OrderingComparisonSite = iota
	OrderingSiteIntersectionKeys
	OrderingComparisonSiteCount
)

// UnnestLegMintSite is one call site of the name-keyed
// `rebaseUnnestOuterLegPredicate`. The five partition its callers, and they are
// named for WHY they reach it rather than for their line numbers, which move.
type UnnestLegMintSite int

const (
	UnnestLegMintSiteNonChainedMerge UnnestLegMintSite = iota
	UnnestLegMintSiteAnchoredNonExists
	UnnestLegMintSiteBuriedNotWindowed
	UnnestLegMintSiteJoinPredNotWindowed
	UnnestLegMintSiteChainedNameModel
	UnnestLegMintSiteCount
)

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
	UnnestLegMintArmName UnnestLegMintArm = iota
	UnnestLegMintArmOrdinalTwin
	UnnestLegMintArmPlanTimeBake
	UnnestLegMintArmLegRelative
	UnnestLegMintArmCount
)
