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
// TestComputePrimaryKey_IndexScanStructuralPK pins RFC-189 B3: an index scan's
// PrimaryKeyProperty is the STRUCTURAL common PK stamped from the match candidate
// (never the by-name columns, which conflate). An unstamped plan abstains (nil);
// a stamped plan surfaces its structural PK; and the union anti-conflation
// prevents dropped rows.
func TestComputePrimaryKey_IndexScanStructuralPK(t *testing.T) {
	t.Parallel()

	bare := plans.NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false)

	// No structural PK stamped → abstain (nil). By-name PK metadata is NOT
	// surfaced as a common PK.
	if pk := computePrimaryKey(bare); pk != nil {
		t.Fatalf("unstamped index scan PK must be nil (abstain), got %v", pk)
	}
	if pk := computePrimaryKey(bare.WithIndexMetadata([]string{"V"}, []string{"ID"}, false)); pk != nil {
		t.Fatalf("by-name PK metadata must NOT surface a common PK, got %v", pk)
	}

	// Stamped structural PK → surfaced.
	structPK := []values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}}
	stamped := bare.WithCommonPrimaryKey(structPK)
	if pk := computePrimaryKey(stamped); pk == nil {
		t.Fatal("a stamped index scan must surface its structural common PK")
	}

	// Anti-conflation at the union level (the dropped-rows prevention). Two legs
	// with the SAME structural PK → a common PK (safe dedup). Two legs whose PKs
	// share the leaf name "ID" but differ structurally (bare Field vs
	// record-type-prefixed) → NO common PK, so ImplementDistinctUnionRule cannot
	// dedup them (which would drop rows — the M5 hazard).
	flat := bare.WithCommonPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})
	flatSame := bare.WithCommonPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})
	prefixed := bare.WithCommonPrimaryKey([]values.Value{
		values.NewRecordTypeValue(nil),
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
	})
	if commonPKFromChildren([]plans.RecordQueryPlan{flat, flatSame}) == nil {
		t.Fatal("two legs with identical structural PKs must share a common PK (safe dedup)")
	}
	if commonPKFromChildren([]plans.RecordQueryPlan{flat, prefixed}) != nil {
		t.Fatal("Field(ID) and Concat(RecordTypeKey(), Field(ID)) legs must NOT share a common PK (dropped-rows prevention)")
	}

	// Primary scan still carries its PK values (unchanged).
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})
	if pk := computePrimaryKey(scan); pk == nil {
		t.Fatal("primary scan must still carry its PK values")
	}
}
