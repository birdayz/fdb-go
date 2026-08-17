package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSortKeyMatchesColumn_NestedShadowsTopLevelColumn pins RFC-187 S2/S3: an
// ORDER BY on a nested field `addr.city` must NOT bind an index/PK sort column
// that only shares the leaf name `city` — otherwise the sort is elided against
// a scan whose order is on the wrong column → wrong output order. The
// no-provider (primary-scan) branch is exercised directly; the provider branch
// routes through valuesMatchColumn (pinned by the S1 test).
func TestSortKeyMatchesColumn_NestedShadowsTopLevelColumn(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("T")
	root := mustOrderedScanQOV(t, src)
	nested := mustOrderedScanField(t,
		mustOrderedScanField(t, root, "ADDR"),
		"CITY")
	flat := mustOrderedScanField(t, root, "CITY")
	bakedValue, err := values.ResolveOrdinalSeedField(root, 6)
	baked := mustConstruct(t, bakedValue, err)

	if sortKeyMatchesColumn(nested, nil, 0, "CITY") {
		t.Fatal("nested addr.city sort key matched top-level CITY column (would elide sort against wrong column)")
	}
	if !sortKeyMatchesColumn(flat, nil, 0, "CITY") {
		t.Fatal("flat city sort key failed to match CITY column (regressed the positive match)")
	}
	if !sortKeyMatchesColumn(baked, nil, 0, "CITY") {
		t.Fatal("baked city sort key failed to match CITY column (baked/lazy bridge regressed)")
	}
}
