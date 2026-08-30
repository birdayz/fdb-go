package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestComputePrimaryKey_IndexScanStructuralPK pins RFC-189 B3: an index scan's
// PrimaryKeyProperty is the STRUCTURAL common PK stamped from the match candidate
// (never the by-name columns, which conflate). An unstamped plan abstains (nil);
// a stamped plan surfaces its structural PK; and the union anti-conflation
// prevents dropped rows.
//
// A stale block used to sit above this one describing RFC-188 M5, where the
// property returned nil for every index scan as the safe under-report. RFC-189
// B3 replaced that with the structural PK below, and the block was left glued
// here with no blank line — so godoc rendered "the property returns nil" as
// this test's own documentation, asserting current behaviour the code had
// already stopped having. It named a test that no longer exists, which is how
// it was found.
func TestComputePrimaryKey_IndexScanStructuralPK(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	barePlan, err := plans.NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, rowType, false)
	bare := mustConstruct(t, barePlan, err)
	resolvedIndexID, err := values.ResolveFieldOrdinals(bare.GetResultValue(), []int{0})
	indexID := mustConstruct(t, resolvedIndexID, err)

	// No structural PK stamped → abstain (nil). By-name PK metadata is NOT
	// surfaced as a common PK.
	if pk := computePrimaryKey(bare); pk != nil {
		t.Fatalf("unstamped index scan PK must be nil (abstain), got %v", pk)
	}
	if pk := computePrimaryKey(bare.WithIndexMetadata([]string{"V"}, []string{"ID"}, false)); pk != nil {
		t.Fatalf("by-name PK metadata must NOT surface a common PK, got %v", pk)
	}

	// Stamped structural PK → surfaced.
	structPK := []values.Value{indexID}
	stamped := bare.WithCommonPrimaryKey(structPK)
	if pk := computePrimaryKey(stamped); pk == nil {
		t.Fatal("a stamped index scan must surface its structural common PK")
	}

	// Anti-conflation at the union level (the dropped-rows prevention). Two legs
	// with the SAME structural PK → a common PK (safe dedup). Two legs whose PKs
	// share the leaf name "ID" but differ structurally (bare Field vs
	// record-type-prefixed) → NO common PK, so ImplementDistinctUnionRule cannot
	// dedup them (which would drop rows — the M5 hazard).
	flat := bare.WithCommonPrimaryKey([]values.Value{indexID})
	flatSame := bare.WithCommonPrimaryKey([]values.Value{indexID})
	prefixed := bare.WithCommonPrimaryKey([]values.Value{
		values.NewRecordTypeValue(nil),
		indexID,
	})
	if commonPKFromChildren([]plans.RecordQueryPlan{flat, flatSame}) == nil {
		t.Fatal("two legs with identical structural PKs must share a common PK (safe dedup)")
	}
	if commonPKFromChildren([]plans.RecordQueryPlan{flat, prefixed}) != nil {
		t.Fatal("Field(ID) and Concat(RecordTypeKey(), Field(ID)) legs must NOT share a common PK (dropped-rows prevention)")
	}

	// Primary scan still carries its PK values (unchanged).
	scanPlan, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	scan := mustConstruct(t, scanPlan, err)
	resolvedScanID, err := values.ResolveFieldOrdinals(scan.GetResultValue(), []int{0})
	scanID := mustConstruct(t, resolvedScanID, err)
	scan = scan.WithPrimaryKey([]values.Value{scanID})
	if pk := computePrimaryKey(scan); pk == nil {
		t.Fatal("primary scan must still carry its PK values")
	}
}
