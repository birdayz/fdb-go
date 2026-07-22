package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestComputePrimaryKey_IndexScanIsNilPendingStructuralPK pins the SAFE state
// of RFC-188 finding 10 M5: computePrimaryKey returns nil for an index scan.
//
// M5 originally returned FieldValues over the index's PK COLUMN NAMES so a
// union/intersection could dedup by PK — but a by-name PK wrongly equates record
// types whose PK expressions differ yet share field names (Field("ID") vs
// Concat(RecordTypeKey(), Field("ID")) both flatten to ["ID"]), which would let
// ImplementDistinctUnionRule dedup two legs that must both survive (dropped
// rows). Java translates getCommonPrimaryKey() STRUCTURALLY. Until that
// structural port lands, the property returns nil — the safe under-report
// (disables the optimization, never wrong dedup). This test guards that nil so a
// by-name PK is never reintroduced without the structural fix.
func TestComputePrimaryKey_IndexScanIsNilPendingStructuralPK(t *testing.T) {
	t.Parallel()

	// Bare index scan → nil.
	bare := plans.NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false)
	if pk := computePrimaryKey(bare); pk != nil {
		t.Fatalf("index scan PK must be nil (structural-PK port pending), got %v", pk)
	}

	// Index scan WITH PK column-name metadata → STILL nil. The by-name columns
	// must not be surfaced as a common PK (they are not structural identity).
	idx := bare.WithIndexMetadata([]string{"V"}, []string{"ID"}, false)
	if pk := computePrimaryKey(idx); pk != nil {
		t.Fatalf("index scan with by-name PK metadata must STILL yield nil (no wrong dedup), got %v", pk)
	}

	// The primary scan continues to carry its (structural, value-level) PK.
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})
	if pk := computePrimaryKey(scan); pk == nil {
		t.Fatal("primary scan must still carry its PK values")
	}
}
