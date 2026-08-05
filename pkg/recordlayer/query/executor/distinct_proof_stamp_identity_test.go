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

	// The projection is the carrier §7.1 actually names: the elided shape is
	// `Project([EMAIL#1], Scan(USERS)) distinct-by:BY_EMAIL`, so on the query
	// the acceptance criteria are written over, this is the plan node holding
	// the dependency. Omitting it from this test left the identity split pinned
	// on the two carriers the criteria do NOT produce.
	t.Run("projection", func(t *testing.T) {
		t.Parallel()
		inner := plans.NewRecordQueryScanPlan([]string{"T"}, flowed, false)
		projections := []values.Value{
			values.NewFieldValueWithResolvedOrdinal("EMAIL", 0, values.TypeString),
		}
		plain := plans.NewRecordQueryProjectionPlan(projections, inner)
		stamped, ok := plain.WithDistinctProofIndexName("BY_EMAIL").(*plans.RecordQueryProjectionPlan)
		if !ok {
			t.Fatal("WithDistinctProofIndexName did not return a projection plan")
		}
		if stamped.GetDistinctProofIndexName() != "BY_EMAIL" {
			t.Fatalf("stamp did not stick: %q", stamped.GetDistinctProofIndexName())
		}
		if plain.GetDistinctProofIndexName() != "" {
			t.Fatal("WithDistinctProofIndexName mutated the receiver instead of copying")
		}
		if plain.EqualsPlanWithoutChildren(stamped) {
			t.Fatal("a stamped projection and its unstamped original are the same memo " +
				"member. This is the carrier the SELECT DISTINCT elision produces, so " +
				"collapsing them here is the concrete route by which the memo keeps " +
				"the unstamped survivor and the index dependency vanishes")
		}
		if plain.HashCodeWithoutChildren() == stamped.HashCodeWithoutChildren() {
			t.Fatal("stamped and unstamped projections hash alike, so the split above " +
				"rests on the equality check alone")
		}

		// A projection mints no continuation of its own — the resume identity on
		// this shape is the inner scan's, and WithDistinctProofIndexName is a
		// shallow struct copy that SHARES that inner. So the continuation half of
		// this test reads through the stamped projection to the scan it did not
		// copy: the stamp must leave it byte-identical, for the same reason it
		// must not enter the index scan's own salt above.
		stampedInner, ok := stamped.GetInner().(*plans.RecordQueryScanPlan)
		if !ok {
			t.Fatalf("stamped projection's inner is %T, want the shared scan", stamped.GetInner())
		}
		if got, want := mustPrimaryScanIdentity(t, stampedInner),
			mustPrimaryScanIdentity(t, inner); got != want {
			t.Fatalf("stamping the projection changed the continuation fingerprint of "+
				"the scan beneath it (%s vs %s). A continuation minted before the "+
				"elision shipped must still resume after it", got, want)
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
