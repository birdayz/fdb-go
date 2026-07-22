package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestComputePrimaryKey_IndexScanCarriesCommonPK pins RFC-188 finding 10 M5:
// Java PrimaryKeyProperty.visitIndexPlan returns the index's common primary key
// (index entries carry the PK); Go returned nil, losing PK-based dedup/ordering
// reasoning for any plan over index scans. The property must return FieldValues
// over the PK column names — the SAME representation the primary scan uses — so
// a union/intersection of a scan child and an index child over the same table
// finds a common PK.
func TestComputePrimaryKey_IndexScanCarriesCommonPK(t *testing.T) {
	t.Parallel()

	// Index plan with no PK metadata → nil (unchanged, safe).
	bare := plans.NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false)
	if pk := computePrimaryKey(bare); pk != nil {
		t.Fatalf("index plan without PK metadata should yield nil, got %v", pk)
	}

	// Index plan carrying the record type's PK columns → FieldValues over them.
	idx := bare.WithIndexMetadata([]string{"V"}, []string{"ID"}, false)
	pk := computePrimaryKey(idx)
	if pk == nil {
		t.Fatal("index plan with PK metadata yielded nil (M5 regressed)")
	}
	pkVals := pk.([]values.Value)
	if len(pkVals) != 1 {
		t.Fatalf("expected 1 PK value, got %d", len(pkVals))
	}

	// It must be the SAME representation the primary scan produces, so a
	// scan child and an index child over the same table share a common PK.
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})
	scanPK := computePrimaryKey(scan).([]values.Value)
	if !values.ValuesStructurallyEqual(pkVals[0], scanPK[0]) {
		t.Fatalf("index PK %v not structurally equal to scan PK %v (commonPKFromChildren would reject the pair)",
			pkVals[0], scanPK[0])
	}
}
