package embedded

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

func TestIndexStatePlanningSignatureInjectiveAndCandidateEquivalent(t *testing.T) {
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
	// READABLE names are already identified by metadata version and do not
	// affect the excluded candidate set.
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
	// All non-readable states are equivalently excluded from planning.
	if pending := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"A":  recordlayer.IndexStateReadableUniquePending,
		"BC": recordlayer.IndexStateWriteOnly,
	}); pending != left {
		t.Fatalf("candidate-equivalent state signatures differ: %q != %q", pending, left)
	}
	// Map iteration order cannot perturb the cache key.
	leftReordered := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"BC": recordlayer.IndexStateDisabled,
		"A":  recordlayer.IndexStateWriteOnly,
	})
	if leftReordered != left {
		t.Fatalf("signature is map-order dependent: %q != %q", leftReordered, left)
	}
}

func TestPlanCacheScopeIncludesIndexStateWithoutComponentCollisions(t *testing.T) {
	t.Parallel()
	readable := indexStatePlanningSignature(map[string]recordlayer.IndexState{})
	pending := indexStatePlanningSignature(map[string]recordlayer.IndexState{
		"IDX": recordlayer.IndexStateReadableUniquePending,
	})
	if planCacheScope("S", 7, "", readable) == planCacheScope("S", 7, "", pending) {
		t.Fatal("index-state transition reused the same plan-cache scope")
	}
	// The tagged, encoded planner component cannot impersonate the following
	// state component, even when its bytes spell the tag exactly.
	if planCacheScope("S", 7, "index-states:"+pending, readable) ==
		planCacheScope("S", 7, "", pending) {
		t.Fatal("planner-option/state components collided")
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
}

func TestMetadataPlanContextAdmitsOnlyStrictlyReadableSecondaryIndexes(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(ordersSchema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	states := map[string]recordlayer.IndexState{
		"IDX_CUSTOMER": recordlayer.IndexStateReadable,
		"IDX_STATUS":   recordlayer.IndexStateWriteOnly,
		"IDX_AMOUNT":   recordlayer.IndexStateDisabled,
		"IDX_TIER":     recordlayer.IndexStateReadableUniquePending,
	}
	ctx := buildCascadesPlanContext(md, cascades.DefaultPlannerConfiguration(), states)
	var secondary []string
	for _, candidate := range ctx.GetMatchCandidates() {
		if !strings.HasPrefix(candidate.CandidateName(), "primary(") {
			secondary = append(secondary, candidate.CandidateName())
		}
	}
	sort.Strings(secondary)
	if want := []string{"IDX_CUSTOMER"}; !reflect.DeepEqual(secondary, want) {
		t.Fatalf("secondary candidates = %v, want %v", secondary, want)
	}

	// A live snapshot is complete by contract; a missing name must fail closed
	// rather than use IndexState's zero value (READABLE).
	partial := buildCascadesPlanContext(
		md, cascades.DefaultPlannerConfiguration(),
		map[string]recordlayer.IndexState{"IDX_CUSTOMER": recordlayer.IndexStateReadable},
	)
	secondary = secondary[:0]
	for _, candidate := range partial.GetMatchCandidates() {
		if !strings.HasPrefix(candidate.CandidateName(), "primary(") {
			secondary = append(secondary, candidate.CandidateName())
		}
	}
	if want := []string{"IDX_CUSTOMER"}; !reflect.DeepEqual(secondary, want) {
		t.Fatalf("partial snapshot admitted a missing-state index: %v", secondary)
	}
}
