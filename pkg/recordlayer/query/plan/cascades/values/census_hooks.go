package values

import "sync/atomic"

// CensusHooks is the planner's test-only census sink; its storage and reports
// live in pkg/internal/querycensus, which installs it. Production leaves it nil.
type CensusHooks struct {
	LegIdentityLeg             func(leg RecordTypeLeg)
	LegIdentityComparison      func(site LegIdentitySite, legName, corrName string)
	LegIdentityConversion      func(site LegIdentitySite, leg, corr CorrelationIdentifier, retiredVerdict bool)
	SeedWindowRead             func(site SeedWindowSite, class SeedWindowReadClass)
	SeedWindowLookup           func(site SeedWindowSite, found bool)
	SeedWindowLookupOfKind     func(site SeedWindowSite, found bool, kind LegKind)
	InnerScalarLegTitleAt      func(producer InnerLegProducer, corr CorrelationIdentifier, title string)
	QualifierRecovery          func(site QualifierRecoverySite, class QualifierRecoveryClass, name, identity string)
	SelectResultMint           func(site SelectResultMintSite, rv Value)
	DottedRowTypeDerivation    func(fields []Field)
	AccessorPathCall           func(class AccessorPathClass)
	AccessorPathDottedWitness  func(name string)
	FieldValueMint             func(field string, baked bool)
	OrderingBridgeDotted       func(leftName, rightName string, answered bool)
	MergeSlotTyping            func(corr CorrelationIdentifier, slotType Type, scavenged bool)
	ResultTypeRead             func(site string, t Type)
	LegColumnProvenance        func(rt *RecordType, name string, flatHit, flatAmbiguous bool, matched *RecordTypeLeg, qualifier string, owner CorrelationIdentifier)
	UnnestLegMintCall          func(site UnnestLegMintSite)
	UnnestLegMintName          func(site UnnestLegMintSite, name string)
	UnnestLegMintBranchReached func(site UnnestLegMintSite)
	UnnestLegMintArm           func(site UnnestLegMintSite, arm UnnestLegMintArm)
	UnnestLegOrdinalTwinCall   func()
}

// OrderingHooks is the ordering-comparison census's sink. That census switches
// independently of the leg-identity family, so it gets its own slot.
type OrderingHooks struct {
	OrderingComparison func(site OrderingComparisonSite, left, right Value, bridge func(a, b Value) bool)
}

var (
	legCensusHooks      atomic.Pointer[CensusHooks]
	orderingCensusHooks atomic.Pointer[OrderingHooks]
)

// Census returns the leg-identity census hooks, or nil when the census is off:
// a record site costs one atomic load and one untaken branch.
func Census() *CensusHooks { return legCensusHooks.Load() }

// OrderingCensus returns the ordering-comparison census hooks, or nil.
func OrderingCensus() *OrderingHooks { return orderingCensusHooks.Load() }

// SetCensusHooks installs (or, with nil, removes) the two census slots.
func SetCensusHooks(leg *CensusHooks, ordering *OrderingHooks) {
	legCensusHooks.Store(leg)
	orderingCensusHooks.Store(ordering)
}

// CensusQOVShape classifies a minted select result for the select-result-mint
// census, which cannot see the unexported QOV it inspects.
func CensusQOVShape(rv Value) (isQOV, typed bool, label string) {
	qov, ok := rv.(*quantifiedObjectValue)
	if !ok {
		return false, false, ""
	}
	return true, quantifiedObjectValueCarriesAType(qov), describeMintedQOVType(qov)
}
