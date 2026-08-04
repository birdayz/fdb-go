package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

func TestIndexStatePlanningSignatureInjectiveAndReadabilityEquivalent(t *testing.T) {
	t.Parallel()

	if got := indexStatePlanningSignature(nil); got != "" {
		t.Fatalf("nil/offline signature = %q, want empty", got)
	}
	allReadable := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"A": recordlayer.IndexStateReadable,
	})
	if allReadable == "" {
		t.Fatal("live all-readable snapshot must not equal the offline signature")
	}
	// A READABLE index contributes nothing: adding one cannot change the
	// signature, or every metadata evolution would invalidate every plan.
	if other := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"A": recordlayer.IndexStateReadable,
		"B": recordlayer.IndexStateReadable,
	}); other != allReadable {
		t.Fatalf("all-readable signatures differ: %q != %q", allReadable, other)
	}

	left := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"A":  recordlayer.IndexStateWriteOnly,
		"BC": recordlayer.IndexStateDisabled,
	})
	right := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"AB": recordlayer.IndexStateWriteOnly,
		"C":  recordlayer.IndexStateDisabled,
	})
	if left == right {
		t.Fatalf("length-boundary collision: %q", left)
	}
	// Every non-readable state is equivalently "not READABLE" for plan
	// validity, matching Java's isReadable (exact equality with READABLE).
	if pending := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"A":  recordlayer.IndexStateReadableUniquePending,
		"BC": recordlayer.IndexStateWriteOnly,
	}); pending != left {
		t.Fatalf("readability-equivalent state signatures differ: %q != %q", pending, left)
	}
	// Map iteration order cannot perturb the signature.
	leftReordered := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"BC": recordlayer.IndexStateDisabled,
		"A":  recordlayer.IndexStateWriteOnly,
	})
	if leftReordered != left {
		t.Fatalf("signature is map-order dependent: %q != %q", leftReordered, left)
	}
}

func TestValidatePlanningIndexStateSignatureRejectsStalePlan(t *testing.T) {
	t.Parallel()
	planned := map[string]recordlayer.IndexState{
		"IDX": recordlayer.IndexStateReadable,
	}
	expected := indexStatePlanningSignature(planned)
	if err := validatePlanningIndexStateSignature(expected, planned); err != nil {
		t.Fatalf("unchanged snapshot rejected: %v", err)
	}
	if err := validatePlanningIndexStateSignature("", nil); err != nil {
		t.Fatalf("offline plan unexpectedly validated state: %v", err)
	}
	// READABLE_UNIQUE_PENDING is SCANNABLE, so a check written against
	// scannability would let this through. Java's constraint asks isReadable.
	err := validatePlanningIndexStateSignature(expected, map[string]recordlayer.IndexState{
		"IDX": recordlayer.IndexStateReadableUniquePending,
	})
	if err == nil {
		t.Fatal("READABLE -> READABLE_UNIQUE_PENDING transition accepted stale plan")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("stale-plan error = %T %v, want SQLSTATE %s", err, err, api.ErrCodeSerializationFailure)
	}
	// The reverse transition — an index becoming readable again — is equally a
	// change: a plan made while it was excluded may now be the wrong plan.
	pendingSig := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"IDX": recordlayer.IndexStateReadableUniquePending,
	})
	if err := validatePlanningIndexStateSignature(pendingSig, planned); err == nil {
		t.Fatal("READABLE_UNIQUE_PENDING -> READABLE transition accepted stale plan")
	}
}
