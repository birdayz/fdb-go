package expressions

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func nameDirProps(n int) PhysicalProperties {
	names := make([]string, n)
	desc := make([]bool, n)
	for i := range names {
		names[i] = fmt.Sprintf("C%d", i)
	}
	return OrderingFromNameDir(names, desc)
}

// TestPhysicalProperties_OverflowRequirementUnsatisfiable pins RFC-180 D1:
// an ordering wider than the fixed 8-column capacity previously TRUNCATED
// silently, so a plan ordered on the first 8 keys "satisfied" a 9-key
// requirement and the enforcer sort was elided — the 9th key's ordering
// silently dropped. An overflowed REQUIREMENT is now unsatisfiable: the
// sort is kept, no winner is stamped.
func TestPhysicalProperties_OverflowRequirementUnsatisfiable(t *testing.T) {
	t.Parallel()
	req9 := nameDirProps(9)
	prov8 := nameDirProps(8)
	prov9 := nameDirProps(9)

	if prov8.Satisfies(req9) {
		t.Fatal("an 8-column provider must NOT satisfy a 9-column requirement (pre-fix silent truncation)")
	}
	if prov9.Satisfies(req9) {
		t.Fatal("even an identical 9-column provider must not claim satisfaction — columns beyond capacity are unrepresentable")
	}

	// Provider-side overflow keeps the TRUE first-8 prefix: a 9-column
	// stream genuinely satisfies any representable prefix requirement.
	if !prov9.Satisfies(prov8) {
		t.Fatal("a 9-column provider satisfies an 8-column prefix requirement")
	}
	if !prov9.Satisfies(nameDirProps(3)) {
		t.Fatal("a 9-column provider satisfies a 3-column prefix requirement")
	}

	// Empty requirement is satisfied by anything, overflowed or not.
	if !prov9.Satisfies(NoProperties) {
		t.Fatal("empty requirement is always satisfied")
	}
}

// TestOrderingFromSortKeys_OverflowMirrorsNameDir pins that both
// constructors mark overflow identically.
func TestOrderingFromSortKeys_OverflowMirrorsNameDir(t *testing.T) {
	t.Parallel()
	keys := make([]SortKey, 9)
	for i := range keys {
		keys[i] = SortKey{Value: values.NewFlatFieldValue(fmt.Sprintf("C%d", i), values.UnknownType)}
	}
	req := OrderingFromSortKeys(keys)
	if nameDirProps(8).Satisfies(req) {
		t.Fatal("9-sort-key requirement must be unsatisfiable")
	}
	if req.OrderingCount() != 8 {
		t.Fatalf("stored prefix must cap at capacity, got %d", req.OrderingCount())
	}
}
