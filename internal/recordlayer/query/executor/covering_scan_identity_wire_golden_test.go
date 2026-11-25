package executor

import (
	"encoding/hex"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The two digests below were MEASURED on the pre-RFC-220 tree (master
// 789b29ea8), where coveringness was a bool on RecordQueryIndexPlan, by running
// indexScanRangeFingerprintSalt over the plan saltGoldenPlan builds — once
// plain, once through WithCovering([]string{"A","B"}).
//
// They are WIRE values. The salt feeds boundScanRangeSet.fingerprint, whose
// digest is embedded in gen.ScanRangeSetContinuation.Fingerprint — a
// user-visible continuation token — and scan_range_set_cursor.go rejects a
// mismatch on resume. So a change to either constant silently invalidates every
// in-flight continuation for the affected plan shape.
//
// THIS TEST MUST NEVER BE RE-BLESSED. If it fails, the salt's field order,
// field names, builder prefix, or value encoding changed, and the fix is to
// restore them — not to paste in the new digest. RFC-220 moved coveringness to
// a distinct plan type; that move is required to be invisible here, which is
// exactly what these constants check.
const (
	saltGoldenPlainDigestHex    = "c53c93457175f0b2c7ad699c61e7d411197ab7dd962b9211a5a535213f3c77fb"
	saltGoldenCoveringDigestHex = "3088d011acafd99ae453710f8e4a53e91a9e52de1a30727dad5c21f5d9364ef9"
)

func saltGoldenPlan() *plans.RecordQueryIndexPlan {
	flowedType := exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "A", FieldType: values.NotNullString},
		values.Field{Name: "B", FieldType: values.NotNullDouble},
	)
	return mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
		"IDX_SALT",
		[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
		[]string{"T"},
		flowedType,
		false,
	)).WithKeyComponentTypes([]values.Type{values.NotNullDouble}).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false)
}

// TestScanIdentitySaltDigestsAreWireStable pins both arms of the continuation
// salt against their pre-RFC-220 values.
//
// Two arms, not one, and each catches a different way to break compatibility:
//
//   - The PLAIN arm catches dropping the now-constant `covering` /
//     `covering-columns` fields from the non-covering salt. That is the obvious
//     cleanup to make once the bool is gone, and it would change the digest of
//     every ordinary index scan in existence.
//   - The COVERING arm catches the new type's salt diverging from the encoding
//     the flag produced — a different builder prefix, a reordered field, a
//     renamed field, or folding the covered columns from somewhere other than
//     the inner's covered entry surface.
func TestScanIdentitySaltDigestsAreWireStable(t *testing.T) {
	t.Parallel()

	plan := saltGoldenPlan()
	plain, err := legacyIndexScanRangeFingerprintSaltFields(
		plan, recordlayer.IndexScanByValue, false, nil)
	if err != nil {
		t.Fatalf("legacy plain salt: %v", err)
	}
	coveringPlan := mustExecutorConstruct(plans.NewRecordQueryCoveringIndexPlan(plan))
	covering, err := legacyIndexScanRangeFingerprintSaltFields(
		plan, recordlayer.IndexScanByValue, true, coveringPlan.GetCoveringColumns())
	if err != nil {
		t.Fatalf("legacy covering salt: %v", err)
	}
	if got := hex.EncodeToString([]byte(plain)); got != saltGoldenPlainDigestHex {
		t.Errorf("NON-COVERING index-scan salt digest changed.\n got  %s\n want %s\n"+
			"This digest reaches the user-visible continuation token. A change here "+
			"rejects every in-flight continuation for an ordinary index scan. Restore "+
			"the salt's fields — do NOT re-bless this constant.", got, saltGoldenPlainDigestHex)
	}

	if got := hex.EncodeToString([]byte(covering)); got != saltGoldenCoveringDigestHex {
		t.Errorf("COVERING index-scan salt digest changed.\n got  %s\n want %s\n"+
			"RFC-220 moved coveringness from a bool on RecordQueryIndexPlan to its own "+
			"plan type; that move MUST be invisible to this digest, or every in-flight "+
			"covering-scan continuation is rejected on upgrade. Restore the field order, "+
			"names and builder prefix — do NOT re-bless this constant.",
			got, saltGoldenCoveringDigestHex)
	}

	// The two arms must differ, or the pin above would be satisfied by a salt
	// that ignores coveringness entirely — and a covering plan could then consume
	// a fetching scan's continuation and rebuild rows from the wrong shape.
	if plain == covering {
		t.Fatal("the covering and non-covering salts are EQUAL; a covering plan " +
			"could resume from a fetching scan's continuation")
	}

	currentPlain, err := indexScanRangeFingerprintSalt(plan, recordlayer.IndexScanByValue)
	if err != nil {
		t.Fatalf("current plain salt: %v", err)
	}
	currentCovering, err := coveringIndexScanRangeFingerprintSalt(
		coveringPlan, recordlayer.IndexScanByValue)
	if err != nil {
		t.Fatalf("current covering salt: %v", err)
	}
	if currentPlain == plain || currentCovering == covering {
		t.Fatal("exact flowed schema did not strengthen the current scan identity")
	}

	legacySpec, err := bindScanComparisonsToRangeSet(
		plan.GetScanComparisons(), plan.GetKeyComponentTypes(), nil,
		plan.IsReverse(), plain)
	if err != nil {
		t.Fatalf("bind legacy range set: %v", err)
	}
	currentSpec, err := bindScanComparisonsToRangeSet(
		plan.GetScanComparisons(), plan.GetKeyComponentTypes(), nil,
		plan.IsReverse(), currentPlain, plain)
	if err != nil {
		t.Fatalf("bind current range set: %v", err)
	}
	started := false
	legacyToken := marshalRangeSetContinuationForTest(
		t, legacySpec.fingerprint,
		make([]uint32, len(legacySpec.alternativeCounts)), nil, &started)
	if _, _, err := parseScanRangeSetContinuation(
		legacyToken, currentSpec.fingerprint, currentSpec.alternativeCounts,
		currentSpec.compatibleFingerprints...,
	); err != nil {
		t.Fatalf("exact plan rejected its pre-exact-schema continuation: %v", err)
	}
}
