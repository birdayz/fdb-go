package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The DISTINCT-elision proof stamp pulls in two opposite directions, and both
// halves have to be pinned because satisfying either one alone is a bug.
//
// It MUST split memo identity. The eliding rule yields a stamped copy of a plan
// whose unstamped original is already in the memo. If the two compared equal
// they would collapse into one group member, the survivor could be the unstamped
// one, and the index dependency would vanish silently — the unguarded elision
// the stamp exists to prevent. Every other test of the mechanism still passes in
// that state, because they all read a freshly built stamped plan.
//
// It MUST NOT enter the continuation fingerprint. The stamp is planner
// provenance; it describes nothing about the physical scan a continuation
// resumes. Folding it in would invalidate every continuation minted before the
// elision shipped, for a plan that reads exactly the same bytes.
//
// This pairing is not a new exception being carved out. strictlySorted is
// already exactly this shape: it IS in RecordQueryIndexPlan.structuralKey and it
// is NOT in indexScanRangeFingerprintSalt.
func TestDistinctProofStampSplitsIdentityButNotContinuation(t *testing.T) {
	t.Parallel()

	flowed := values.NewRecordType("T", false, []values.Field{
		{Name: "EMAIL", FieldType: values.NullableString, Ordinal: 0},
	})

	t.Run("index scan", func(t *testing.T) {
		t.Parallel()
		plain := plans.NewRecordQueryIndexPlan("BY_EMAIL", nil, []string{"T"}, flowed, false)
		stamped, ok := plain.WithDistinctProofIndexName("BY_EMAIL").(*plans.RecordQueryIndexPlan)
		if !ok {
			t.Fatal("WithDistinctProofIndexName did not return an index plan")
		}
		if stamped.GetDistinctProofIndexName() != "BY_EMAIL" {
			t.Fatalf("stamp did not stick: %q", stamped.GetDistinctProofIndexName())
		}
		if plain.GetDistinctProofIndexName() != "" {
			t.Fatal("WithDistinctProofIndexName mutated the receiver instead of copying")
		}
		if plain.EqualsPlanWithoutChildren(stamped) {
			t.Fatal("a stamped index scan and its unstamped original are the same memo " +
				"member. They are not interchangeable: one is correct only while " +
				"BY_EMAIL stays READABLE, the other is correct unconditionally. " +
				"Collapsing them lets the memo keep the unstamped survivor and drop " +
				"the dependency")
		}
		if plain.HashCodeWithoutChildren() == stamped.HashCodeWithoutChildren() {
			t.Fatal("stamped and unstamped index scans hash alike, so the split above " +
				"rests on the equality check alone")
		}
		if got, want := mustIndexScanIdentity(t, stamped, recordlayer.IndexScanByValue),
			mustIndexScanIdentity(t, plain, recordlayer.IndexScanByValue); got != want {
			t.Fatalf("the proof stamp changed the continuation fingerprint (%s vs %s). "+
				"It is planner provenance and describes nothing about the physical "+
				"scan a continuation resumes; a continuation minted before this "+
				"shipped must still resume after it", got, want)
		}
	})

	t.Run("primary scan", func(t *testing.T) {
		t.Parallel()
		plain := plans.NewRecordQueryScanPlan([]string{"T"}, flowed, false)
		stamped, ok := plain.WithDistinctProofIndexName("BY_EMAIL").(*plans.RecordQueryScanPlan)
		if !ok {
			t.Fatal("WithDistinctProofIndexName did not return a scan plan")
		}
		if plain.EqualsPlanWithoutChildren(stamped) {
			t.Fatal("a stamped base-record scan and its unstamped original are the " +
				"same memo member — and this is the carrier the elision actually " +
				"produces on the unordered SELECT DISTINCT regime")
		}
		if got, want := mustPrimaryScanIdentity(t, stamped),
			mustPrimaryScanIdentity(t, plain); got != want {
			t.Fatalf("the proof stamp changed the primary-scan continuation "+
				"fingerprint (%s vs %s)", got, want)
		}
	})
}
