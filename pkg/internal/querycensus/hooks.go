package querycensus

import (
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// hooks routes every production record site to this package's recorders.
var hooks = &values.CensusHooks{
	LegIdentityLeg:             RecordLegIdentityLeg,
	LegIdentityComparison:      RecordLegIdentityComparison,
	LegIdentityConversion:      RecordLegIdentityConversion,
	SeedWindowRead:             RecordSeedWindowRead,
	SeedWindowLookup:           RecordSeedWindowLookup,
	SeedWindowLookupOfKind:     RecordSeedWindowLookupOfKind,
	InnerScalarLegTitleAt:      RecordInnerScalarLegTitleAt,
	QualifierRecovery:          RecordQualifierRecovery,
	SelectResultMint:           RecordSelectResultMint,
	DottedRowTypeDerivation:    RecordDottedRowTypeDerivation,
	AccessorPathCall:           RecordAccessorPathCall,
	AccessorPathDottedWitness:  RecordAccessorPathDottedWitness,
	FieldValueMint:             NoteFieldValueMint,
	OrderingBridgeDotted:       NoteOrderingBridgeDotted,
	MergeSlotTyping:            recordMergeSlotTyping,
	ResultTypeRead:             recordResultTypeRead,
	OrderingComparison:         recordOrderingComparison,
	LegColumnProvenance:        recordLegColumnProvenance,
	UnnestLegMintCall:          RecordUnnestLegMintCall,
	UnnestLegMintName:          RecordUnnestLegMintName,
	UnnestLegMintBranchReached: RecordUnnestLegMintBranchReached,
	UnnestLegMintArm:           RecordUnnestLegMintArm,
	UnnestLegOrdinalTwinCall:   RecordUnnestLegOrdinalTwinCall,
}

var installMu sync.Mutex

// setEnabled flips one census switch and republishes the production slots, so a
// slot is non-nil exactly while its switch is on (the old gate's semantics).
func setEnabled(flag *atomic.Bool, on bool) {
	installMu.Lock()
	defer installMu.Unlock()
	flag.Store(on)
	var leg, ordering *values.CensusHooks
	if legCensusEnabled.Load() {
		leg = hooks
	}
	if orderingCensusEnabled.Load() {
		ordering = hooks
	}
	values.SetCensusHooks(leg, ordering)
}
